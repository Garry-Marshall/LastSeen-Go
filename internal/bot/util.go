package bot

import (
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"lastseen/internal/db"
	"lastseen/internal/locale"
	"lastseen/internal/pyfmt"
)

// discord.py colour values used by the Python bot's embeds.
const (
	ColorBlue     = 0x3498DB
	ColorGreen    = 0x2ECC71
	ColorOrange   = 0xE67E22
	ColorBlurple  = 0x5865F2
	ColorRed      = 0xE74C3C
	ColorGold     = 0xF1C40F
	ColorTeal     = 0x1ABC9C
	ColorPurple   = 0x9B59B6
	ColorDarkGrey = 0x607D8B
)

type A = locale.A

// t is locale.T (key, language, then name/value pairs).
func t(key, lang string, kv ...any) string { return locale.T(key, lang, kv...) }

func nowUnix() int64 { return time.Now().Unix() }

// formatDT is discord.utils.format_dt: <t:unix:style>.
func formatDT(ts int64, style string) string { return fmt.Sprintf("<t:%d:%s>", ts, style) }

// formatTimestamp renders a Discord timestamp, or common.never for 0/nil.
func formatTimestamp(ts *int64, style, lang string) string {
	if ts == nil || *ts == 0 {
		return t("common.never", lang)
	}
	return formatDT(*ts, style)
}

func formatRelative(ts *int64, lang string) string { return formatTimestamp(ts, "R", lang) }

// formatUTCDate is a plain YYYY-MM-DD for a UTC day bucket.
func formatUTCDate(ts int64) string { return time.Unix(ts, 0).UTC().Format("2006-01-02") }

// userTag is Python's str(user): the username, or name#discriminator for
// the few accounts that still have one.
func userTag(u discord.User) string {
	if u.Discriminator == "" || u.Discriminator == "0" {
		return u.Username
	}
	return u.Username + "#" + u.Discriminator
}

// userDisplayName is discord.User.display_name: global name, else username.
func userDisplayName(u discord.User) string {
	if u.GlobalName != nil && *u.GlobalName != "" {
		return *u.GlobalName
	}
	return u.Username
}

// memberDisplayName is discord.Member.display_name: nick, global name, username.
func memberDisplayName(m discord.Member) string {
	if m.Nick != nil && *m.Nick != "" {
		return *m.Nick
	}
	return userDisplayName(m.User)
}

// storedNickname is what the bot stores as "nickname": the display name when
// it differs from the username, else nil.
func storedNickname(m discord.Member) *string {
	d := memberDisplayName(m)
	if d == userTag(m.User) {
		return nil
	}
	return &d
}

// joinTimestamp is the member's join time, or fallback when unknown.
func joinTimestamp(m discord.Member, fallback int64) int64 {
	if m.JoinedAt == nil || m.JoinedAt.IsZero() {
		return fallback
	}
	return m.JoinedAt.Unix()
}

// sortRoles orders roles like discord.py (position, then id).
func sortRoles(roles []discord.Role) {
	sort.Slice(roles, func(i, j int) bool {
		if roles[i].Position != roles[j].Position {
			return roles[i].Position < roles[j].Position
		}
		return roles[i].ID < roles[j].ID
	})
}

// memberRoles is discord.Member.roles: @everyone plus the member's cached
// roles, sorted by position.
func (b *Bot) memberRoles(guildID snowflake.ID, roleIDs []snowflake.ID) []discord.Role {
	var out []discord.Role
	if r, ok := b.Client.Caches.Role(guildID, guildID); ok {
		out = append(out, r)
	}
	for _, id := range roleIDs {
		if id == guildID {
			continue
		}
		if r, ok := b.Client.Caches.Role(guildID, id); ok {
			out = append(out, r)
		}
	}
	sortRoles(out)
	return out
}

// memberRoleNames is get_member_roles: role names without @everyone.
func (b *Bot) memberRoleNames(guildID snowflake.ID, roleIDs []snowflake.ID) []string {
	names := []string{}
	everyoneDropped := false
	for _, r := range b.memberRoles(guildID, roleIDs) {
		if r.Name == "@everyone" && !everyoneDropped {
			everyoneDropped = true // list.remove drops the first match only
			continue
		}
		names = append(names, r.Name)
	}
	return names
}

func boolPtr(v bool) *bool { return &v }

// field is an embed field (discord.py defaults inline to True, so every
// call site states it).
func field(name, value string, inline bool) discord.EmbedField {
	return discord.EmbedField{Name: name, Value: value, Inline: boolPtr(inline)}
}

// newEmbed is create_embed: a titled, coloured embed stamped with now.
func newEmbed(title string, color int) discord.Embed {
	now := time.Now().UTC()
	return discord.Embed{Title: title, Color: color, Timestamp: &now}
}

func errorEmbed(msg, lang string) discord.Embed {
	e := newEmbed(t("common.error_title", lang), ColorRed)
	e.Description = msg
	return e
}

func successEmbed(msg, lang string) discord.Embed {
	e := newEmbed(t("common.success_title", lang), ColorGreen)
	e.Description = msg
	return e
}

// memberCtx is what the permission helpers need to know about a member.
type memberCtx struct {
	GuildID     snowflake.ID
	Permissions discord.Permissions
	Roles       []discord.Role // including @everyone
}

// hasBotAdminRole: guild administrators, or members with the configured bot
// admin role (never @everyone).
func hasBotAdminRole(m memberCtx, roleName string) bool {
	if m.Permissions.Has(discord.PermissionAdministrator) {
		return true
	}
	for _, r := range m.Roles {
		if r.Name == roleName && r.ID != m.GuildID {
			return true
		}
	}
	return false
}

// canUseBotCommands: bot admins always; others when no user role is
// required or they have it.
func canUseBotCommands(m memberCtx, cfg *db.GuildConfig) bool {
	adminRole, userRole, required := "LastSeen Admin", "LastSeen User", false
	if cfg != nil {
		adminRole, userRole, required = cfg.BotAdminRoleName, cfg.UserRoleName, cfg.UserRoleRequired
	}
	if hasBotAdminRole(m, adminRole) {
		return true
	}
	if !required {
		return true
	}
	for _, r := range m.Roles {
		if r.Name == userRole {
			return true
		}
	}
	return false
}

// isChannelAllowed applies the allowed-channels filter (a thread counts as
// its parent channel). Unparsable or empty config allows everything.
func isChannelAllowed(channelID snowflake.ID, cfg *db.GuildConfig, parentID *snowflake.ID) bool {
	if cfg == nil || cfg.AllowedChannels == nil || *cfg.AllowedChannels == "" {
		return true
	}
	ids, ok := db.LoadInts(*cfg.AllowedChannels)
	if !ok || len(ids) == 0 {
		return true
	}
	for _, id := range ids {
		if id == int64(channelID) || (parentID != nil && id == int64(*parentID)) {
			return true
		}
	}
	return false
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

// joinCapped joins items within limit characters, summarising the rest as
// common.and_more (Discord caps an embed field value at 1024).
func joinCapped(items []string, lang string, limit int, sep string) string {
	full := strings.Join(items, sep)
	if runeLen(full) <= limit {
		return full
	}
	reserve := runeLen(sep) + runeLen(t("common.and_more", lang, "count", len(items)))
	var shown []string
	for _, item := range items {
		if runeLen(strings.Join(append(append([]string{}, shown...), item), sep))+reserve > limit {
			break
		}
		shown = append(shown, item)
	}
	return strings.Join(append(shown, t("common.and_more", lang, "count", len(items)-len(shown))), sep)
}

// truncateString cuts text to maxLen characters, ending in suffix.
func truncateString(text string, maxLen int, suffix string) string {
	r := []rune(text)
	if len(r) <= maxLen {
		return text
	}
	if maxLen < runeLen(suffix) {
		return string(r[:max(maxLen, 0)])
	}
	return string(r[:maxLen-runeLen(suffix)]) + suffix
}

// formatHealthDelta is the localized change vs a previous window.
func formatHealthDelta(cur, prev int64, lang string) string {
	if prev > 0 {
		pct := float64(cur-prev) / float64(prev) * 100
		r := pyfmt.Round(pct)
		if r > 0 {
			return t("commands.stats_view.health_delta_up", lang, "pct", r, "prev", prev)
		}
		if r < 0 {
			return t("commands.stats_view.health_delta_down", lang, "pct", -r, "prev", prev)
		}
		return t("commands.stats_view.health_delta_flat", lang, "prev", prev)
	}
	if cur > 0 {
		return t("commands.stats_view.health_delta_new", lang)
	}
	return t("commands.stats_view.health_delta_flat", lang, "prev", prev)
}

const (
	pulseRisingThreshold  = 5.0
	pulseCoolingThreshold = -5.0
)

// Pulse is the directional community-pulse read.
type Pulse struct {
	State                       string // rising, steady, cooling, collecting
	PostersDelta, MessagesDelta *float64
}

// computeCommunityPulse compares the last 30 days with the 30 before.
func computeCommunityPulse(h *db.ServerHealth) Pulse {
	if h == nil || !h.MonthlyComparable {
		return Pulse{State: "collecting"}
	}
	pct := func(cur, prev int64) *float64 {
		if prev > 0 {
			v := float64(cur-prev) / float64(prev) * 100
			return &v
		}
		return nil
	}
	p := Pulse{PostersDelta: pct(h.Posters30d, h.PostersPrev30d), MessagesDelta: pct(h.Messages30d, h.MessagesPrev30d)}
	var deltas []float64
	for _, d := range []*float64{p.PostersDelta, p.MessagesDelta} {
		if d != nil {
			deltas = append(deltas, *d)
		}
	}
	if len(deltas) == 0 {
		p.State = "steady"
		if h.Posters30d != 0 || h.Messages30d != 0 {
			p.State = "rising"
		}
		return p
	}
	var sum float64
	for _, d := range deltas {
		sum += d
	}
	avg := sum / float64(len(deltas))
	switch {
	case avg >= pulseRisingThreshold:
		p.State = "rising"
	case avg <= pulseCoolingThreshold:
		p.State = "cooling"
	default:
		p.State = "steady"
	}
	return p
}

// formatPctArrow is the compact pulse percentage (e.g. "↑8%").
func formatPctArrow(pct float64) string {
	r := pyfmt.Round(pct)
	switch {
	case r > 0:
		return fmt.Sprintf("↑%d%%", r)
	case r < 0:
		return fmt.Sprintf("↓%d%%", -r)
	}
	return "→0%"
}

// sf converts a stored ID to a snowflake.
func sf(id int64) snowflake.ID { return snowflake.ID(id) }

// i64 converts a snowflake to the stored form.
func i64(id snowflake.ID) int64 { return int64(id) }
