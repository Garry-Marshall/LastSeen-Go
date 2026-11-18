package bot

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"lastseen/internal/db"
	"lastseen/internal/locale"
	"lastseen/internal/logx"
	"lastseen/internal/textfmt"
)

// The user commands: /whois, /lastseen, /mystats, /about and the others.

var clog = logx.Get("commands")

// numFloat reads a db.Num (int64 or float64) as a float.
func numFloat(n db.Num) float64 {
	switch v := n.(type) {
	case int64:
		return float64(v)
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}

// fmtTS is format_timestamp for a plain timestamp (0 renders "Never").
func fmtTS(ts int64, style, lang string) string { return formatTimestamp(&ts, style, lang) }

func truthy(p *int64) bool { return p != nil && *p != 0 }

// checkPermissions is _check_permissions: may the caller use commands here?
// Returns the error embed when not, whether the guild restricts commands to
// allowed channels (results are then public), and the guild language.
func (b *Bot) checkPermissions(ix *Ix) (ok bool, errEmbed discord.Embed, restricted bool, lang string) {
	cfg := ix.guildConfig()
	lang = guildLang(cfg)
	if cfg != nil && !canUseBotCommands(ix.memberCtx(), cfg) {
		return false, errorEmbed(t("errors.no_permission", lang, "role", cfg.UserRoleName), lang), false, lang
	}
	if cfg != nil {
		restricted = cfg.AllowedChannels != nil && *cfg.AllowedChannels != ""
		if !isChannelAllowed(ix.ChannelID, cfg, ix.channelParentID()) {
			return false, errorEmbed(t("errors.channel_not_allowed", lang), lang), restricted, lang
		}
	}
	return true, discord.Embed{}, restricted, lang
}

// isBotAdmin checks the caller against the guild's bot-admin role.
func (b *Bot) isBotAdmin(ix *Ix, cfg *db.GuildConfig) (bool, string) {
	role := adminRoleName(cfg)
	return hasBotAdminRole(ix.memberCtx(), role), role
}

// parseUserMention turns "<@123>" / "<@!123>" into "123"; anything else is
// returned trimmed.
func parseUserMention(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "<@") && strings.HasSuffix(s, ">") && len(s) >= 3 {
		return strings.TrimPrefix(s[2:len(s)-1], "!")
	}
	return s
}

// avatarURL is display_avatar.url for a cached member, else for the user
// from any shared guild ("" when unknown).
func (b *Bot) avatarURL(guildID, userID snowflake.ID) string {
	opts := func(hash *string) []discord.CDNOpt {
		o := []discord.CDNOpt{discord.WithSize(1024)}
		if hash != nil && strings.HasPrefix(*hash, "a_") {
			o = append(o, discord.WithFormat(discord.FileFormatGIF))
		}
		return o
	}
	if m, ok := b.Client.Caches.Member(guildID, userID); ok {
		if m.Avatar != nil {
			return m.EffectiveAvatarURL(opts(m.Avatar)...)
		}
		return m.User.EffectiveAvatarURL(opts(m.User.Avatar)...)
	}
	if u, ok := b.findUser(guildID, userID); ok {
		return u.EffectiveAvatarURL(opts(u.Avatar)...)
	}
	return ""
}

func setThumbnail(e *discord.Embed, url string) {
	if url != "" {
		e.Thumbnail = &discord.EmbedResource{URL: url}
	}
}

// memberOnline is `member and member.status != offline`.
func (b *Bot) memberOnline(guildID, userID snowflake.ID) bool {
	if _, ok := b.Client.Caches.Member(guildID, userID); !ok {
		return false
	}
	return b.isOnline(guildID, userID)
}

func orText(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func strOr(p *string, fallback string) string {
	if p == nil || *p == "" {
		return fallback
	}
	return *p
}

// previousNicknames is the stored nickname history minus the shown name.
func previousNicknames(m *db.Member, current string) []string {
	if m.NicknameHistory == nil || *m.NicknameHistory == "" {
		return nil
	}
	hist, ok := db.LoadStrings(*m.NicknameHistory)
	if !ok {
		return nil
	}
	var out []string
	for _, n := range hist {
		if n != current {
			out = append(out, n)
		}
	}
	return out
}

func (b *Bot) userAutocomplete(ix *Ix, current string) []discord.AutocompleteChoice {
	if ix.GuildID == 0 || runeLen(current) < 2 {
		return nil
	}
	var out []discord.AutocompleteChoice
	for _, m := range b.DB.SearchMembersByName(i64(ix.GuildID), current, 25) {
		display := strOr(m.Nickname, m.Username)
		out = append(out, discord.AutocompleteChoiceString{Name: runeCut(display, 100), Value: runeCut(m.Username, 100)})
	}
	return out
}

// ==================== /whois ====================

var statusEmoji = map[discord.OnlineStatus]string{
	discord.OnlineStatusOnline:  "🟢",
	discord.OnlineStatusIdle:    "🟡",
	discord.OnlineStatusDND:     "🔴",
	discord.OnlineStatusOffline: "⚫",
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + strings.ToLower(s[1:])
}

func (b *Bot) cmdWhois(ix *Ix) {
	ok, errE, restricted, lang := b.checkPermissions(ix)
	if !ok {
		ix.Respond(embedMsg(errE, true))
		return
	}
	eph := !restricted
	ix.Defer(eph)

	userArg := ix.cmd.String("user")
	gid := i64(ix.GuildID)
	md := b.DB.FindMemberByName(gid, parseUserMention(userArg))
	if md == nil {
		ix.Followup(embedMsg(errorEmbed(t("commands.whois.not_found", lang), lang), eph))
		return
	}
	uid := sf(md.UserID)
	member, cached := b.Client.Caches.Member(ix.GuildID, uid)

	cfg := b.DB.GetGuildConfig(gid)
	isAdmin, _ := b.isBotAdmin(ix, cfg)

	username := orText(md.Username, t("common.unknown", lang))
	e := newEmbed(t("commands.whois.title", lang, "username", username), ColorBlue)
	setThumbnail(&e, b.avatarURL(ix.GuildID, uid))

	var d strings.Builder
	d.WriteString(t("commands.whois.user_id", lang, "user_id", md.UserID))
	if isAdmin && cached {
		d.WriteString(t("commands.whois.account_created", lang, "date", fmtTS(member.User.CreatedAt().Unix(), "F", lang)))
	}
	if truthy(md.JoinDate) {
		joinStr := fmtTS(*md.JoinDate, "F", lang)
		if truthy(md.JoinPosition) {
			d.WriteString(t("commands.whois.joined_with_position", lang, "date", joinStr, "position", *md.JoinPosition))
		} else {
			d.WriteString(t("commands.whois.joined", lang, "date", joinStr))
		}
	}
	d.WriteString("\n")

	nickname := strOr(md.Nickname, t("common.not_set", lang))
	d.WriteString(t("commands.whois.nickname", lang, "nickname", nickname))
	if isAdmin {
		if prev := previousNicknames(md, nickname); len(prev) > 0 {
			d.WriteString(t("commands.whois.previously_known", lang, "names", strings.Join(prev, ", ")))
		}
	}
	if cached {
		roles := b.memberRoles(ix.GuildID, member.RoleIDs)
		if len(roles) > 1 {
			if top := roles[len(roles)-1]; top.Name != "@everyone" {
				d.WriteString(t("commands.whois.highest_role", lang, "role", mentionRole(i64(top.ID))))
			}
		}
	}
	d.WriteString("\n")

	if cached {
		status := b.statuses.Status(ix.GuildID, uid)
		name := statusName(status)
		emoji, ok := statusEmoji[discord.OnlineStatus(name)]
		if !ok {
			emoji = "⚫"
		}
		d.WriteString(t("commands.whois.status", lang, "emoji", emoji, "status", capitalize(name)))
		switch {
		case name != "offline":
			d.WriteString(t("commands.whois.last_seen_online", lang))
		case truthy(md.LastSeen):
			d.WriteString(t("commands.whois.last_seen", lang, "date", fmtTS(*md.LastSeen, "R", lang)))
		default:
			d.WriteString(t("commands.whois.last_seen_unavailable", lang))
		}
	} else {
		d.WriteString(t("commands.whois.status_left", lang))
	}
	if cached && member.PremiumSince != nil {
		d.WriteString(t("commands.whois.boosting_yes", lang, "date", fmtTS(member.PremiumSince.Unix(), "F", lang)))
	} else {
		d.WriteString(t("commands.whois.boosting_no", lang))
	}
	d.WriteString("\n")

	act := b.DB.GetMessageActivityPeriod(gid, md.UserID, 30)
	if act.Total > 0 || act.Today >= 0 {
		d.WriteString(t("commands.whois.activity_header", lang))
		d.WriteString(t("commands.whois.activity_today", lang, "count", act.Today))
		d.WriteString(t("commands.whois.activity_week", lang, "count", act.ThisWeek))
		d.WriteString(t("commands.whois.activity_month", lang, "count", act.ThisMonth))
		if numFloat(act.AvgPerDay) > 0 {
			d.WriteString(t("commands.whois.activity_avg", lang, "avg", act.AvgPerDay))
		}
	}

	// When-active patterns: admins, or members viewing themselves.
	showProfile := isAdmin || i64(ix.User.ID) == md.UserID
	if showProfile {
		tz := "UTC"
		if cfg != nil {
			tz = cfg.Timezone
		}
		panel := b.DB.GetWhoisActivityPanel(gid, md.UserID, 30, tz)
		p := panel.Profile
		if p.Total > 0 {
			d.WriteString("\n")
			d.WriteString(t("commands.whois.profile_header", lang))
			d.WriteString(t("commands.whois.profile_active_days", lang, "active", p.ActiveDays, "total", p.WindowDays))
			d.WriteString(t("commands.whois.profile_timeline", lang, "sparkline", buildActivitySparkline(panel.Trend, 30)))
			if p.Total >= 15 && p.ActiveDays >= 5 {
				if p.PeakWeekday != nil {
					d.WriteString(t("commands.whois.profile_peak_day", lang, "day", locale.Weekday(*p.PeakWeekday, lang)))
				}
				if p.PeakHours != nil {
					d.WriteString(t("commands.whois.profile_peak_hours", lang,
						"start", hourLabel(p.PeakHours[0]), "end", hourLabel(p.PeakHours[1])))
				}
				if key, ok := map[string]string{
					"increasing": "commands.whois.profile_trend_up",
					"decreasing": "commands.whois.profile_trend_down",
					"steady":     "commands.whois.profile_trend_steady",
				}[p.Trend]; ok {
					d.WriteString(t(key, lang))
				}
				if pc := panel.Percentile; pc != nil {
					filled := int(textfmt.Round(float64(pc.Percentile) / 10))
					filled = max(0, min(filled, 10))
					bar := strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)
					d.WriteString(t("commands.whois.profile_activity_bar", lang, "bar", bar, "percentile", pc.Percentile))
				}
			}
		}
	}
	e.Description = d.String()

	msg := embedMsg(e, eph)
	if showProfile {
		msg.View = b.journeyView(ix.GuildID, md.UserID, username, ix.User.ID, lang)
	}
	ix.Followup(msg)
	clog.Info("User %s used /whois for '%s' in guild %s", userTag(ix.User), userArg, ix.GuildName())
}

// buildActivitySparkline renders the last `days` UTC days of a member's
// message counts, oldest left: '·' for silent days, ▁..█ scaled to the
// member's own busiest day.
func buildActivitySparkline(trend []db.DayCount, days int) string {
	counts := map[int64]int64{}
	for _, r := range trend {
		counts[r.Date] = r.Count
	}
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()
	series := make([]int64, 0, days)
	var peak int64
	for off := days - 1; off >= 0; off-- {
		c := counts[today-int64(off)*86400]
		series = append(series, c)
		peak = max(peak, c)
	}
	if peak == 0 {
		return strings.Repeat("·", days)
	}
	ramp := []rune("▁▂▃▄▅▆▇█")
	var sb strings.Builder
	for _, c := range series {
		if c == 0 {
			sb.WriteRune('·')
			continue
		}
		level := (c*int64(len(ramp)-1) + peak - 1) / peak
		sb.WriteRune(ramp[level])
	}
	return sb.String()
}

// journeyView is JourneyView: one 🧬 Journey button posting the member's
// participation journey as an ephemeral followup.
func (b *Bot) journeyView(guildID snowflake.ID, userID int64, username string, ownerID snowflake.ID, lang string) *View {
	v := b.newView(300*time.Second).ownerOnly(ownerID, lang)
	v.Button(t("commands.journey.button", lang), discord.ButtonStylePrimary, "", -1, func(ix *Ix) {
		ix.DeferUpdate()
		j := b.DB.GetMemberJourney(i64(guildID), userID)
		if j == nil {
			ix.Followup(Msg{Content: t("commands.journey.no_data", lang), Ephemeral: true})
			return
		}
		e := b.journeyEmbed(j, b.memberOnline(guildID, sf(userID)), username, lang)
		setThumbnail(&e, b.avatarURL(guildID, sf(userID)))
		ix.Followup(embedMsg(e, true))
	})
	return v
}

// journeyEmbed renders get_member_journey (message rollups + last_seen).
func (b *Bot) journeyEmbed(j *db.MemberJourney, online bool, username, lang string) discord.Embed {
	e := newEmbed(t("commands.journey.title", lang, "username", username), ColorTeal)
	var d strings.Builder
	if truthy(j.JoinDate) {
		d.WriteString(t("commands.journey.joined", lang, "date", fmtTS(*j.JoinDate, "R", lang)))
	}
	if truthy(j.FirstActive) {
		when := fmtTS(*j.FirstActive, "R", lang)
		if truthy(j.JoinDate) && *j.FirstActive/86400 == *j.JoinDate/86400 {
			when = t("commands.journey.same_day", lang)
		}
		d.WriteString(t("commands.journey.first_activity", lang, "when", when))
		d.WriteString(t("commands.journey.active_days", lang, "days", j.ActiveDays))
		d.WriteString(t("commands.journey.messages", lang, "count", j.TotalMessages))
		d.WriteString(t("commands.journey.longest_streak", lang, "days", j.LongestStreak))
		d.WriteString(t("commands.journey.longest_silence", lang, "days", j.LongestGap))
	} else {
		d.WriteString(t("commands.journey.no_activity", lang))
	}
	if j.ReturnCount > 0 && truthy(j.LongestAwayDays) {
		d.WriteString(t("commands.journey.returned", lang, "days", *j.LongestAwayDays))
	}
	if truthy(j.LastActive) {
		d.WriteString(t("commands.journey.last_message", lang, "when", fmtTS(*j.LastActive, "R", lang)))
	}
	if online {
		d.WriteString(t("commands.journey.last_seen_online", lang))
	} else if truthy(j.LastSeen) {
		d.WriteString(t("commands.journey.last_seen", lang, "when", fmtTS(*j.LastSeen, "R", lang)))
	}
	e.Description = d.String()
	e.Footer = &discord.EmbedFooter{Text: t("commands.journey.footer", lang)}
	return e
}

// ==================== /lastseen, /seen ====================

func (b *Bot) lastseenImpl(ix *Ix, commandName string) {
	ok, errE, restricted, lang := b.checkPermissions(ix)
	if !ok {
		ix.Respond(embedMsg(errE, true))
		return
	}
	eph := !restricted
	ix.Defer(eph)

	userArg := ix.cmd.String("user")
	md := b.DB.FindMemberByName(i64(ix.GuildID), parseUserMention(userArg))
	if md == nil {
		ix.Followup(embedMsg(errorEmbed(t("commands.whois.not_found", lang), lang), eph))
		return
	}
	uid := sf(md.UserID)
	e := newEmbed(t("commands.lastseen.title", lang), ColorGreen)
	setThumbnail(&e, b.avatarURL(ix.GuildID, uid))
	add := func(name, value string) { e.Fields = append(e.Fields, field(name, value, false)) }

	add(t("commands.lastseen.field_username", lang), orText(md.Username, t("common.not_set", lang)))
	add(t("commands.lastseen.field_nickname", lang), strOr(md.Nickname, t("common.not_set", lang)))
	if b.memberOnline(ix.GuildID, uid) {
		add(t("commands.lastseen.field_status", lang), t("commands.lastseen.currently_online", lang))
		add(t("commands.lastseen.field_last_seen", lang), t("commands.lastseen.right_meow", lang))
	} else {
		add(t("commands.lastseen.field_status", lang), t("commands.lastseen.offline", lang))
		if truthy(md.LastSeen) {
			add(t("commands.lastseen.field_last_seen", lang), fmtTS(*md.LastSeen, "R", lang))
			add(t("commands.lastseen.field_exact_time", lang), fmtTS(*md.LastSeen, "F", lang))
		} else {
			add(t("commands.lastseen.field_last_seen", lang), t("commands.lastseen.not_available", lang))
		}
	}
	if !md.IsActive {
		add(t("commands.lastseen.field_note", lang), t("commands.lastseen.left_server", lang))
	}
	ix.Followup(embedMsg(e, eph))
	clog.Info("User %s used /%s for '%s' in guild %s", userTag(ix.User), commandName, userArg, ix.GuildName())
}

// ==================== /role-history ====================

func (b *Bot) cmdRoleHistory(ix *Ix) {
	ok, errE, restricted, lang := b.checkPermissions(ix)
	if !ok {
		ix.Respond(embedMsg(errE, true))
		return
	}
	gid := i64(ix.GuildID)
	if admin, role := b.isBotAdmin(ix, b.DB.GetGuildConfig(gid)); !admin {
		ix.Respond(embedMsg(errorEmbed(t("errors.no_permission", lang, "role", role), lang), true))
		return
	}
	eph := !restricted
	ix.Defer(eph)

	userArg := ix.cmd.String("user")
	md := b.DB.FindMemberByName(gid, parseUserMention(userArg))
	if md == nil {
		ix.Followup(embedMsg(errorEmbed(t("commands.role_history.not_found", lang), lang), eph))
		return
	}
	changes := b.DB.GetRoleHistory(gid, md.UserID, 20)
	if len(changes) == 0 {
		e := newEmbed(t("commands.role_history.title", lang, "username", md.Username), ColorBlue)
		e.Description = t("commands.role_history.no_changes", lang)
		ix.Followup(embedMsg(e, eph))
		return
	}
	e := newEmbed(t("commands.role_history.title", lang, "username", orText(md.Username, t("common.unknown", lang))), ColorBlue)
	var d strings.Builder
	d.WriteString(t("commands.role_history.header", lang))
	for _, c := range changes {
		if c.Action != "added" && c.Action != "removed" {
			clog.Warn("Invalid action '%s' in role_changes for %d", c.Action, md.UserID)
			continue
		}
		roleName := "Unknown"
		if c.RoleName != "" {
			roleName = escapeMarkdown(c.RoleName)
		}
		emoji, action := "➕", t("commands.role_history.added", lang)
		if c.Action == "removed" {
			emoji, action = "➖", t("commands.role_history.removed", lang)
		}
		d.WriteString(t("commands.role_history.line", lang, "emoji", emoji, "action", action,
			"role", roleName, "time", fmtTS(c.Timestamp, "R", lang)))
	}
	e.Description = d.String()
	ix.Followup(embedMsg(e, eph))
	clog.Info("User %s used /role-history for '%s' in guild %s", userTag(ix.User), userArg, ix.GuildName())
}

// ==================== /inactive ====================

func (b *Bot) cmdInactive(ix *Ix) {
	ok, errE, restricted, lang := b.checkPermissions(ix)
	if !ok {
		ix.Respond(embedMsg(errE, true))
		return
	}
	gid := i64(ix.GuildID)
	if admin, role := b.isBotAdmin(ix, b.DB.GetGuildConfig(gid)); !admin {
		ix.Respond(embedMsg(errorEmbed(t("errors.no_permission", lang, "role", role), lang), true))
		return
	}
	days, hasDays := ix.cmd.OptInt("days")
	if hasDays && (days < 1 || days > 365) {
		ix.Respond(embedMsg(errorEmbed(t("commands.inactive.validate_range", lang), lang), true))
		return
	}
	eph := !restricted
	ix.Defer(eph)

	cfg := b.DB.GetGuildConfig(gid)
	if cfg == nil {
		ix.Followup(embedMsg(errorEmbed(t("commands.inactive.no_guild_config", lang), lang), true))
		return
	}
	inactiveDays := cfg.InactiveDays
	if hasDays {
		inactiveDays = days
	}
	members := b.DB.GetInactiveMembers(gid, inactiveDays)
	if tracked := b.DB.GetTrackedUserIDs(gid); tracked != nil {
		keep := map[int64]bool{}
		for _, id := range tracked.IDs {
			keep[id] = true
		}
		filtered := members[:0]
		for _, m := range members {
			if keep[m.UserID] {
				filtered = append(filtered, m)
			}
		}
		members = filtered
	}
	clog.Info("User %s used /inactive in guild %s with threshold %d", userTag(ix.User), ix.GuildName(), inactiveDays)
	clog.Info("Found %d inactive members (>%d days)", len(members), inactiveDays)

	if len(members) == 0 {
		e := newEmbed(t("commands.inactive.empty_title", lang), ColorBlue)
		addedAt := cfg.AddedAt
		trackedDays := int(math.Floor((float64(time.Now().UnixMicro())/1e6 - float64(addedAt)) / 86400))
		if inactiveDays > trackedDays {
			e.Description = t("commands.inactive.too_early_desc", lang, "days", inactiveDays, "tracked_days", trackedDays,
				"joined", fmtTS(addedAt, "R", lang), "ready", fmtTS(addedAt+int64(inactiveDays)*86400, "R", lang))
			if trackedDays >= 1 {
				e.Description += t("commands.inactive.too_early_hint", lang, "tracked_days", trackedDays)
			}
		} else {
			e.Description = t("commands.inactive.empty_desc", lang, "days", inactiveDays)
		}
		ix.Followup(embedMsg(e, eph))
		return
	}

	pages := chunk(members, 8)
	embeds := make([]discord.Embed, 0, len(pages))
	export := make([]exportMember, 0, len(members))
	for i, page := range pages {
		e := newEmbed(t("commands.inactive.page_title", lang, "days", inactiveDays, "count", len(members),
			"page", i+1, "total", len(pages)), ColorBlue)
		for _, m := range page {
			lastSeen := t("common.never", lang)
			if truthy(m.LastSeen) {
				lastSeen = fmtTS(*m.LastSeen, "R", lang)
			}
			info := t("commands.inactive.member_info", lang, "nickname", strOr(m.Nickname, t("common.not_set", lang)), "last_seen", lastSeen)
			e.Fields = append(e.Fields, field(orText(m.Username, t("common.unknown", lang)), info, false))
		}
		embeds = append(embeds, e)
	}
	for _, m := range members {
		export = append(export, exportMember{Username: m.Username, Nickname: strOr(m.Nickname, ""), UserID: m.UserID,
			LastSeen: m.LastSeen, JoinDate: m.JoinDate, Roles: m.Roles})
	}
	ix.Followup(Msg{Embeds: embeds[:1], View: b.newPaginationView(embeds, ix.User.ID, lang, export), Ephemeral: eph})
}

// ==================== /chat-history ====================

func (b *Bot) cmdChatHistory(ix *Ix) {
	ok, errE, restricted, lang := b.checkPermissions(ix)
	if !ok {
		ix.Respond(embedMsg(errE, true))
		return
	}
	gid := i64(ix.GuildID)
	userArg, hasUser := ix.cmd.OptString("user")

	// Resolve the member first so a refusal stays private.
	var md *db.Member
	if hasUser {
		md = b.DB.FindMemberByName(gid, parseUserMention(userArg))
		if md != nil && md.UserID != i64(ix.User.ID) {
			if admin, role := b.isBotAdmin(ix, b.DB.GetGuildConfig(gid)); !admin {
				ix.Respond(embedMsg(errorEmbed(t("commands.chat_history.others_admin_only", lang, "role", role), lang), true))
				return
			}
		}
	}
	eph := !restricted
	ix.Defer(eph)

	if !hasUser {
		stats := b.DB.GetGuildMessageActivityStats(gid, 365, nil)
		title := t("commands.chat_history.server_title", lang, "guild", ix.GuildName())
		if stats.Total365d == 0 {
			e := newEmbed(title, ColorBlue)
			e.Description = t("commands.chat_history.no_activity", lang)
			ix.Followup(embedMsg(e, eph))
			return
		}
		e := newEmbed(title, ColorBlue)
		var d strings.Builder
		d.WriteString(t("commands.chat_history.longterm_header", lang))
		d.WriteString(t("commands.chat_history.total_messages", lang, "count", stats.Total365d))
		d.WriteString(t("commands.chat_history.avg_day", lang, "avg", stats.AvgPerDay))
		if s := stats.BusiestDay; s != nil {
			d.WriteString(t("commands.chat_history.busiest_day", lang, "count", s.Count, "date", formatUTCDate(s.Date)))
		}
		if s := stats.QuietestDay; s != nil {
			d.WriteString(t("commands.chat_history.quietest_day", lang, "count", s.Count, "date", formatUTCDate(s.Date)))
		}
		d.WriteString("\n" + t("commands.chat_history.period_header", lang))
		d.WriteString(t("commands.chat_history.period_30", lang, "count", stats.Total30d))
		d.WriteString(t("commands.chat_history.period_90", lang, "count", stats.Total90d))
		d.WriteString(t("commands.chat_history.period_365", lang, "count", stats.Total365d))
		d.WriteString(t("commands.chat_history.recent_header", lang))
		d.WriteString(t("commands.chat_history.recent_today", lang, "count", stats.Today))
		d.WriteString(t("commands.chat_history.recent_week", lang, "count", stats.Total7d))
		d.WriteString(t("commands.chat_history.recent_month", lang, "count", stats.Total30d))

		humans := 0
		for m := range b.Client.Caches.Members(ix.GuildID) {
			if !m.User.Bot {
				humans++
			}
		}
		d.WriteString(t("commands.chat_history.member_stats_header", lang))
		d.WriteString(t("commands.chat_history.active_members", lang, "active", stats.ActiveMembers30d, "total", humans))
		if numFloat(stats.AvgPerMember) > 0 {
			d.WriteString(t("commands.chat_history.per_member", lang, "count", stats.AvgPerMember))
		}
		e.Description = d.String()
		e.Footer = &discord.EmbedFooter{Text: t("commands.chat_history.footer_guild", lang)}
		ix.Followup(embedMsg(e, eph))
		clog.Info("User %s used /chat-history (guild-wide) in guild %s", userTag(ix.User), ix.GuildName())
		return
	}

	if md == nil {
		ix.Followup(embedMsg(errorEmbed(t("commands.role_history.not_found", lang), lang), eph))
		return
	}
	username := orText(md.Username, t("common.unknown", lang))
	title := t("commands.chat_history.user_title", lang, "username", username)
	trend := b.DB.GetMessageActivityTrend(gid, md.UserID, 365)
	if len(trend) == 0 {
		e := newEmbed(title, ColorBlue)
		e.Description = t("commands.chat_history.no_activity", lang)
		ix.Followup(embedMsg(e, eph))
		return
	}
	var total int64
	maxDay, minDay := trend[0], trend[0]
	for _, r := range trend {
		total += r.Count
		if r.Count > maxDay.Count {
			maxDay = r
		}
		if r.Count < minDay.Count {
			minDay = r
		}
	}
	trackedDays := max(b.DB.GetMemberDaysWithData(gid, md.UserID, 365), 1)
	avg := textfmt.RoundTo(float64(total)/float64(trackedDays), 1)
	s30 := b.DB.GetMessageActivityPeriod(gid, md.UserID, 30)
	s90 := b.DB.GetMessageActivityPeriod(gid, md.UserID, 90)

	e := newEmbed(title, ColorBlue)
	var d strings.Builder
	d.WriteString(t("commands.chat_history.longterm_header", lang))
	d.WriteString(t("commands.chat_history.total_messages", lang, "count", total))
	d.WriteString(t("commands.chat_history.avg_day_user", lang, "avg", avg))
	d.WriteString(t("commands.chat_history.busiest_day", lang, "count", maxDay.Count, "date", formatUTCDate(maxDay.Date)))
	d.WriteString(t("commands.chat_history.quietest_day_user", lang, "count", minDay.Count, "date", formatUTCDate(minDay.Date)))
	d.WriteString(t("commands.chat_history.period_header", lang))
	d.WriteString(t("commands.chat_history.period_30", lang, "count", s30.ThisMonth))
	d.WriteString(t("commands.chat_history.period_90", lang, "count", s90.ThisMonth))
	d.WriteString(t("commands.chat_history.period_365", lang, "count", total))

	now := time.Now().UTC()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC).Unix()
	var month int64
	for _, r := range trend {
		if r.Date >= monthStart {
			month += r.Count
		}
	}
	d.WriteString(t("commands.chat_history.recent_header", lang))
	d.WriteString(t("commands.chat_history.recent_month_user", lang, "count", month))
	d.WriteString(t("commands.chat_history.recent_week", lang, "count", s30.ThisWeek))
	d.WriteString(t("commands.chat_history.recent_today", lang, "count", s30.Today))
	e.Description = d.String()
	e.Footer = &discord.EmbedFooter{Text: t("commands.chat_history.footer_user", lang)}
	ix.Followup(embedMsg(e, eph))
	clog.Info("User %s used /chat-history for '%s' in guild %s", userTag(ix.User), userArg, ix.GuildName())
}

// ==================== /mystats ====================

func (b *Bot) cmdMyStats(ix *Ix) {
	// Always private, so acknowledge before any DB work.
	ix.Defer(true)
	ok, errE, _, lang := b.checkPermissions(ix)
	if !ok {
		ix.Followup(embedMsg(errE, true))
		return
	}
	gid, uid := i64(ix.GuildID), i64(ix.User.ID)
	md := b.DB.GetMember(gid, uid)
	if md == nil {
		ix.Followup(embedMsg(errorEmbed(t("commands.mystats.not_tracked", lang), lang), true))
		return
	}
	username := orText(md.Username, t("common.unknown", lang))
	e := newEmbed(t("commands.mystats.title", lang, "username", username), ColorBlue)
	var d strings.Builder
	if truthy(md.JoinDate) {
		joinStr := fmtTS(*md.JoinDate, "F", lang)
		if truthy(md.JoinPosition) {
			d.WriteString(t("commands.whois.joined_with_position", lang, "date", joinStr, "position", *md.JoinPosition))
		} else {
			d.WriteString(t("commands.whois.joined", lang, "date", joinStr))
		}
	}
	nickname := strOr(md.Nickname, t("common.not_set", lang))
	d.WriteString(t("commands.whois.nickname", lang, "nickname", nickname))
	if prev := previousNicknames(md, nickname); len(prev) > 0 {
		d.WriteString(t("commands.whois.previously_known", lang, "names", strings.Join(prev, ", ")))
	}
	d.WriteString("\n")

	switch {
	case b.memberOnline(ix.GuildID, ix.User.ID):
		d.WriteString(t("commands.whois.last_seen_online", lang))
	case truthy(md.LastSeen):
		d.WriteString(t("commands.whois.last_seen", lang, "date", fmtTS(*md.LastSeen, "R", lang)))
	default:
		d.WriteString(t("commands.whois.last_seen_unavailable", lang))
	}
	d.WriteString("\n")

	act := b.DB.GetMessageActivityPeriod(gid, uid, 30)
	trend := b.DB.GetMessageActivityTrend(gid, uid, 365)
	d.WriteString(t("commands.mystats.activity_header", lang))
	d.WriteString(t("commands.whois.activity_today", lang, "count", act.Today))
	d.WriteString(t("commands.whois.activity_week", lang, "count", act.ThisWeek))
	d.WriteString(t("commands.whois.activity_month", lang, "count", act.ThisMonth))
	if len(trend) > 0 {
		var total int64
		busiest := trend[0]
		for _, r := range trend {
			total += r.Count
			if r.Count > busiest.Count {
				busiest = r
			}
		}
		d.WriteString(t("commands.mystats.activity_365", lang, "count", total))
		d.WriteString(t("commands.mystats.activity_busiest", lang, "count", busiest.Count, "date", formatUTCDate(busiest.Date)))
	}
	if pc := b.DB.GetActivityPercentile(gid, uid, 30); pc != nil {
		if pc.CallerTotal > 0 {
			d.WriteString(t("commands.mystats.percentile", lang, "pct", pc.Percentile, "rank", pc.Rank, "total", pc.TotalRanked))
		} else {
			d.WriteString(t("commands.mystats.percentile_unranked", lang))
		}
	}
	e.Description = d.String()
	e.Footer = &discord.EmbedFooter{Text: t("commands.mystats.footer", lang)}
	ix.Followup(Msg{Embeds: []discord.Embed{e}, View: b.journeyView(ix.GuildID, uid, username, ix.User.ID, lang), Ephemeral: true})
	clog.Info("User %s used /mystats in guild %s", userTag(ix.User), ix.GuildName())
}

// ==================== /forgetme, /optin, /no-watch ====================

func (b *Bot) cmdForgetMe(ix *Ix) {
	lang := guildLang(ix.guildConfig())
	e := newEmbed(t("commands.forgetme.confirm_title", lang), ColorOrange)
	e.Description = t("commands.forgetme.confirm_desc", lang)
	ix.Respond(Msg{Embeds: []discord.Embed{e}, View: b.forgetMeView(i64(ix.User.ID), lang), Ephemeral: true})
}

// forgetMeView is ForgetMeConfirmView (60s).
func (b *Bot) forgetMeView(userID int64, lang string) *View {
	v := b.newView(60 * time.Second)
	v.Button(t("commands.forgetme.btn_confirm", lang), discord.ButtonStyleDanger, "🗑️", -1, func(ix *Ix) {
		ix.DeferUpdate()
		// Opt out first so no event re-creates rows while the purge runs.
		b.OptedOut.Add(userID)
		if !b.DB.AddOptedOutUser(userID) {
			b.OptedOut.Remove(userID)
			ix.EditOriginal(Msg{Embeds: []discord.Embed{errorEmbed(t("commands.forgetme.save_failed", lang), lang)}, ClearView: true})
			v.Stop()
			return
		}
		if b.tracking != nil {
			b.tracking.PurgeUserFromBuffers(userID)
		}
		counts := b.DB.PurgeUserData(userID)
		if counts == nil {
			ix.EditOriginal(Msg{Embeds: []discord.Embed{errorEmbed(t("commands.forgetme.delete_failed", lang), lang)}, ClearView: true})
			v.Stop()
			return
		}
		ix.EditOriginal(Msg{Embeds: []discord.Embed{successEmbed(t("commands.forgetme.success", lang,
			"members", counts.Members, "roles", counts.RoleChanges,
			"activity", counts.MessageActivity+counts.MessageActivityHourly), lang)}, ClearView: true})
		clog.Info("User %d opted out of tracking and purged their data", userID)
		v.Stop()
	})
	v.Button(t("common.cancel", lang), discord.ButtonStyleSecondary, "❌", -1, func(ix *Ix) {
		e := newEmbed(t("common.cancelled", lang), ColorBlue)
		e.Description = t("commands.forgetme.cancelled_desc", lang)
		ix.Update(Msg{Embeds: []discord.Embed{e}, ClearView: true})
		v.Stop()
	})
	return v
}

func (b *Bot) cmdOptIn(ix *Ix) {
	uid := i64(ix.User.ID)
	lang := guildLang(ix.guildConfig())
	if !b.OptedOut.Has(uid) {
		ix.Respond(embedMsg(errorEmbed(t("commands.optin.not_opted_out", lang), lang), true))
		return
	}
	ix.Defer(true)
	if !b.DB.RemoveOptedOutUser(uid) {
		ix.Followup(embedMsg(errorEmbed(t("commands.optin.update_failed", lang), lang), true))
		return
	}
	b.OptedOut.Remove(uid)
	// Re-create the member rows right away: message tracking only counts
	// members that already exist in the database.
	if b.tracking != nil {
		for g := range b.Client.Caches.Guilds() {
			if m, ok := b.Client.Caches.Member(g.ID, ix.User.ID); ok {
				b.tracking.ensureMemberExists(g.ID, m)
			}
		}
	}
	ix.Followup(embedMsg(successEmbed(t("commands.optin.success", lang), lang), true))
	clog.Info("User %s (%d) opted back in to tracking", userTag(ix.User), uid)
}

func (b *Bot) cmdNoWatch(ix *Ix) {
	uid := i64(ix.User.ID)
	lang := guildLang(ix.guildConfig())
	if b.NoWatch.Has(uid) {
		if !b.DB.RemoveNoWatchUser(uid) {
			ix.Respond(embedMsg(errorEmbed(t("commands.no_watch.update_failed", lang), lang), true))
			return
		}
		b.NoWatch.Remove(uid)
		ix.Respond(embedMsg(successEmbed(t("commands.no_watch.now_watchable", lang), lang), true))
		clog.Info("User %s (%d) opted back in to being watchable", userTag(ix.User), uid)
		return
	}
	// Add first so no watch can fire in the gap; roll back on failure.
	b.NoWatch.Add(uid)
	if !b.DB.AddNoWatchUser(uid) {
		b.NoWatch.Remove(uid)
		ix.Respond(embedMsg(errorEmbed(t("commands.no_watch.update_failed", lang), lang), true))
		return
	}
	ix.Respond(embedMsg(successEmbed(t("commands.no_watch.now_exempt", lang), lang), true))
	clog.Info("User %s (%d) opted out of being watched", userTag(ix.User), uid)
}

// ==================== /about ====================

func commaInt(n int64) string { return textfmt.Value(n, ",") }

func (b *Bot) cmdAbout(ix *Ix) {
	ix.Defer(true)
	var cfg *db.GuildConfig
	if ix.GuildID != 0 {
		cfg = ix.guildConfig()
	}
	lang := guildLang(cfg)
	e := newEmbed(t("commands.about.title", lang), ColorGreen)
	e.Description = t("commands.about.description", lang)

	st := b.DB.GetBotStatistics()
	blank := field("​", "​", true)
	e.Fields = append(e.Fields,
		field(t("commands.about.servers_served", lang), commaInt(st.TotalGuilds), true),
		field(t("commands.about.users_counted", lang), commaInt(st.TotalUsers), true),
		blank,
		field(t("commands.about.messages_24h", lang), commaInt(st.Last24h), true),
		field(t("commands.about.messages_7d", lang), commaInt(st.Last7d), true),
		blank,
	)

	ps := readProcStats()
	uptime := "Unknown"
	if start := b.startTime.Load(); start > 0 {
		delta := time.Since(time.Unix(start, 0))
		days := int64(delta / (24 * time.Hour))
		secs := int64((delta % (24 * time.Hour)) / time.Second)
		h, m, s := secs/3600, secs%3600/60, secs%60
		switch {
		case days > 0:
			uptime = fmt.Sprintf("%dd %dh %dm %ds", days, h, m, s)
		case h > 0:
			uptime = fmt.Sprintf("%dh %dm %ds", h, m, s)
		default:
			uptime = fmt.Sprintf("%dm %ds", m, s)
		}
	}
	latency := textfmt.Round(b.latency().Seconds() * 1000)
	health := b.DB.GetDatabaseHealth()
	dbStatus := "❌ Unhealthy"
	if health.Status == "healthy" {
		dbStatus = "✅ Healthy"
	}
	system := "```\n" +
		fmt.Sprintf("Version:   %s\n", Version) +
		fmt.Sprintf("Memory:    %s MB\n", textfmt.Value(ps.memMB, ".1f")) +
		fmt.Sprintf("CPU:       %s%%\n", textfmt.Value(ps.cpuPct, ".1f")) +
		fmt.Sprintf("Threads:   %d\n", ps.threads) +
		fmt.Sprintf("Uptime:    %s\n", uptime) +
		fmt.Sprintf("Latency:   %d ms\n", latency) +
		fmt.Sprintf("Go:        %s\n", goVersion()) +
		fmt.Sprintf("Database:  %s\n", dbStatus) +
		fmt.Sprintf("DB Size:   %s MB\n", textfmt.Value(health.FileSizeMB, ".2f")) +
		"```"
	e.Fields = append(e.Fields,
		field(t("commands.about.system_resources", lang), system, false),
		field(t("commands.about.privacy_title", lang), t("commands.about.privacy_value", lang), false),
		field(t("commands.about.help_title", lang), t("commands.about.help_value", lang), false),
	)
	e.Footer = &discord.EmbedFooter{Text: t("commands.about.footer", lang)}
	ix.Followup(Msg{Embeds: []discord.Embed{e}})
}

// latency is bot.latency: the average heartbeat latency over the shards.
func (b *Bot) latency() time.Duration {
	var sum time.Duration
	n := 0
	for _, g := range b.shards() {
		sum += g.Latency()
		n++
	}
	if n == 0 {
		return 0
	}
	return sum / time.Duration(n)
}

// ==================== discord.utils.escape_markdown ====================

var markdownRe = regexp.MustCompile(`(?m)(?:(?P<url><[^: >]+:\/[^ >]+>|(?:https?|steam):\/\/[^\s<]+[^<.,:;"'\]\s])|(?P<markdown>[_\\~|\*` + "`" + `]|^>(?:>>)?\s|\[.+\]\(.+\)|^#{1,3}|^\s*-))`)

// escapeMarkdown is discord.utils.escape_markdown with its defaults: links
// are left alone, every markdown character gets a backslash.
func escapeMarkdown(text string) string {
	urlIdx := markdownRe.SubexpIndex("url")
	var sb strings.Builder
	last := 0
	for _, loc := range markdownRe.FindAllStringSubmatchIndex(text, -1) {
		sb.WriteString(text[last:loc[0]])
		if loc[2*urlIdx] < 0 {
			sb.WriteByte('\\')
		}
		sb.WriteString(text[loc[0]:loc[1]])
		last = loc[1]
	}
	sb.WriteString(text[last:])
	return sb.String()
}
