package bot

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/snowflake/v2"

	"lastseen/internal/db"
	"lastseen/internal/logx"
)

// Port of cogs/watch.py: admin-configured presence alerts.
//
//   - online_return fires on the offline->online transition (optionally only
//     after a minimum time away), with a 5m away floor and a per-member
//     cooldown against presence flicker. Role watches are bundled into
//     digests.
//   - offline_for is swept every minute and fires once when a target crosses
//     its threshold, re-arming when the target next comes online.

var wlog = logx.Get("cogs.watch")

const (
	maxWatchesPerGuild   = 50
	onlineCooldownSecs   = 3600
	onlineMinAwaySecs    = 5 * 60
	minDurationSecs      = 5 * 60
	maxDurationSecs      = 365 * 86400
	offlineSweepInterval = 60 * time.Second
	watchesPerPage       = 10
	alertListMax         = 50
	roleDigestSecs       = 600
	startupGraceSecs     = 120
	maxDeliveryFailures  = 10
	defaultAdminRoleName = "LastSeen Admin"
)

var durationRe = regexp.MustCompile(`(?i)^\s*(\d+)\s*([mhd])\s*$`)

// parseDuration parses '30m' / '48h' / '7d' into seconds (ok=false when the
// format is unrecognised; the range is not enforced here).
func parseDuration(text string) (int64, bool) {
	m := durationRe.FindStringSubmatch(strings.TrimSpace(text)) // Python's s also strips Unicode spaces
	if m == nil {
		return 0, false
	}
	unit := map[string]int64{"m": 60, "h": 3600, "d": 86400}[strings.ToLower(m[2])]
	v, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || v > math.MaxInt64/unit {
		return math.MaxInt64, true // out of range, like Python's big ints
	}
	if v <= 0 {
		return 0, false
	}
	return v * unit, true
}

// formatDuration renders seconds in the largest exact unit ('7d', '48h', '30m').
func formatDuration(s int64) string {
	switch {
	case s%86400 == 0:
		return fmt.Sprintf("%dd", s/86400)
	case s%3600 == 0:
		return fmt.Sprintf("%dh", s/3600)
	}
	return fmt.Sprintf("%dm", max(s/60, 0))
}

// formatAway renders a measured absence rounded down to its largest unit.
func formatAway(s int64) string {
	switch {
	case s >= 86400:
		return fmt.Sprintf("%dd", s/86400)
	case s >= 3600:
		return fmt.Sprintf("%dh", s/3600)
	}
	return fmt.Sprintf("%dm", max(s/60, 0))
}

type cooldownID struct{ watchID, userID int64 }

// roleDigest collects a role watch's returns until it is posted.
type roleDigest struct {
	since   int64
	order   []int64 // first-return order (a Python dict keeps insertion order)
	members map[int64]*int64
}

// Watch is the WatchCog.
type Watch struct {
	b  *Bot
	mu sync.Mutex
	// Per-(watch, member) last-fired time for the online_return cooldown.
	onlineCooldown map[cooldownID]int64
	// Pending role online_return digests by watch id.
	roleReturns map[int64]*roleDigest
	// online_return alerts are suppressed until this unix time (after a
	// (re)connect Discord resyncs presences and can surface fake returns).
	suppressOnlineUntil atomic.Int64
}

func newWatch(b *Bot) *Watch {
	w := &Watch{b: b, onlineCooldown: map[cooldownID]int64{}, roleReturns: map[int64]*roleDigest{}}
	w.suppressOnlineUntil.Store(nowUnix() + startupGraceSecs)
	w.refreshWatchGuilds()
	go w.sweepLoop()
	return w
}

func (w *Watch) onReady() {
	w.suppressOnlineUntil.Store(nowUnix() + startupGraceSecs)
	wlog.Info("WatchCog: suppressing online-return alerts for %ds after (re)connect", startupGraceSecs)
}

// onMemberRemove deletes DM watches that deliver to a member who left.
func (w *Watch) onMemberRemove(guildID snowflake.ID, user discord.User) {
	if !w.b.WatchGuilds.Has(i64(guildID)) {
		return
	}
	if removed := w.b.DB.RemoveDMWatchesForRecipient(i64(guildID), i64(user.ID)); removed > 0 {
		w.refreshWatchGuilds()
		wlog.Info("Removed %d DM watch(es) delivering to %s (%d), who left guild %s",
			removed, userTag(user), user.ID, w.b.guildName(guildID))
	}
}

// refreshWatchGuilds rebuilds the hot-path set of guilds that have watches.
func (w *Watch) refreshWatchGuilds() { w.b.WatchGuilds.Replace(w.b.DB.GetWatchGuildIDs()) }

func adminRoleName(cfg *db.GuildConfig) string {
	if cfg == nil {
		return defaultAdminRoleName
	}
	return cfg.BotAdminRoleName
}

// ensureAdmin gates a command on the bot-admin role; ok=false when refused.
func (w *Watch) ensureAdmin(ix *Ix) (string, bool) {
	cfg := ix.guildConfig()
	lang := guildLang(cfg)
	role := adminRoleName(cfg)
	if !hasBotAdminRole(ix.memberCtx(), role) {
		ix.Respond(embedMsg(errorEmbed(t("errors.no_permission", lang, "role", role), lang), true))
		return "", false
	}
	return lang, true
}

func (w *Watch) replyError(ix *Ix, msg, lang string) {
	ix.Respond(embedMsg(errorEmbed(msg, lang), true))
}

// ==================== Commands ====================

// watchSpec is a validated watch about to be saved.
type watchSpec struct {
	targetType string
	targetID   int64
	alertType  string
	threshold  *int64
	destID     snowflake.ID // 0 for DM delivery
	dm         bool
}

func (w *Watch) checkDuration(ix *Ix, lang string, text string) (int64, bool) {
	secs, ok := parseDuration(text)
	if !ok {
		w.replyError(ix, t("watch.err_duration_format", lang), lang)
		return 0, false
	}
	if secs < minDurationSecs || secs > maxDurationSecs {
		w.replyError(ix, t("watch.err_duration_range", lang,
			"min", formatDuration(minDurationSecs), "max", formatDuration(maxDurationSecs)), lang)
		return 0, false
	}
	return secs, true
}

func (w *Watch) cmdOnline(ix *Ix) {
	lang, ok := w.ensureAdmin(ix)
	if !ok {
		return
	}
	var threshold *int64
	if after, ok := ix.cmd.OptString("after"); ok {
		secs, ok := w.checkDuration(ix, lang, after)
		if !ok {
			return
		}
		threshold = &secs
	}
	w.addWatch(ix, lang, "online_return", threshold)
}

func (w *Watch) cmdOffline(ix *Ix) {
	lang, ok := w.ensureAdmin(ix)
	if !ok {
		return
	}
	secs, ok := w.checkDuration(ix, lang, ix.cmd.String("duration"))
	if !ok {
		return
	}
	w.addWatch(ix, lang, "offline_for", &secs)
}

// addWatch is the shared validation + persistence of the add commands.
func (w *Watch) addWatch(ix *Ix, lang, alertType string, threshold *int64) {
	b := w.b
	// The mentionable option can resolve to a member, a user, a role, or
	// nothing at all (Discord often omits @everyone), so validate off the id
	// and the guild's own cache.
	targetID := ix.cmd.Snowflake("target")
	resolved, _ := ix.cmd.OptMentionable("target")

	if targetID == ix.GuildID {
		w.replyError(ix, t("watch.err_everyone", lang), lang)
		return
	}

	var spec watchSpec
	if role, isRole := resolved.(discord.Role); isRole {
		if cached, ok := b.Client.Caches.Role(ix.GuildID, role.ID); ok {
			role = cached
		}
		if role.Tags != nil && role.Tags.BotID != nil {
			w.replyError(ix, t("watch.err_bot", lang), lang)
			return
		}
		spec.targetType, spec.targetID = "role", i64(role.ID)
	} else {
		isBot := false
		switch v := resolved.(type) {
		case discord.ResolvedMember:
			isBot = v.User.Bot
		case discord.User:
			isBot = v.Bot
		}
		if cached, ok := b.Client.Caches.Member(ix.GuildID, targetID); ok && cached.User.Bot {
			isBot = true
		}
		switch {
		case isBot:
			w.replyError(ix, t("watch.err_bot", lang), lang)
			return
		case b.OptedOut.Has(i64(targetID)):
			w.replyError(ix, t("watch.err_opted_out", lang), lang)
			return
		case b.NoWatch.Has(i64(targetID)):
			w.replyError(ix, t("watch.err_no_watch", lang), lang)
			return
		}
		spec.targetType, spec.targetID = "user", i64(targetID)
	}

	dm := ix.cmd.Bool("dm")
	channel, hasChannel := ix.cmd.OptSnowflake("channel")
	if dm && hasChannel {
		w.replyError(ix, t("watch.err_channel_and_dm", lang), lang)
		return
	}
	spec.alertType, spec.threshold, spec.dm = alertType, threshold, dm
	if !dm {
		spec.destID = ix.ChannelID
		if hasChannel {
			spec.destID = channel
		}
	}

	// Enforce the cap, but never block reconfiguring an existing watch.
	existing := b.DB.GetGuildWatches(i64(ix.GuildID))
	var prior *db.Watch
	for i := range existing {
		e := &existing[i]
		if e.TargetType == spec.targetType && e.TargetID == spec.targetID && e.AlertType == alertType {
			prior = e
			break
		}
	}
	if prior == nil && len(existing) >= maxWatchesPerGuild {
		w.replyError(ix, t("watch.err_cap", lang, "max", maxWatchesPerGuild), lang)
		return
	}

	// A new watch on a user DMs that member a heads-up, so the admin
	// confirms first.
	if spec.targetType == "user" && prior == nil {
		e := newEmbed(t("watch.confirm_title", lang), ColorOrange)
		e.Description = t("watch.confirm_desc", lang, "target", mentionUser(spec.targetID))
		ix.Respond(Msg{Embeds: []discord.Embed{e}, View: w.confirmView(ix, lang, spec), Ephemeral: true})
		return
	}

	e, _ := w.saveWatch(ix, lang, spec, prior)
	ix.Respond(embedMsg(e, true))
}

// confirmView is WatchConfirmView: Proceed / Cancel, 60s.
func (w *Watch) confirmView(cmdIx *Ix, lang string, spec watchSpec) *View {
	v := w.b.newView(60 * time.Second)
	cancelled := func() discord.Embed {
		e := newEmbed(t("common.cancelled", lang), ColorBlue)
		e.Description = t("watch.confirm_cancelled", lang)
		return e
	}
	v.Button(t("watch.btn_proceed", lang), discord.ButtonStyleSuccess, "", -1, func(ix *Ix) {
		v.Stop()
		ix.DeferUpdate()
		e, notify := w.saveWatch(ix, lang, spec, nil)
		ix.EditOriginal(Msg{Embeds: []discord.Embed{e}, ClearView: true})
		// After the confirmation so the DM's latency can't delay it.
		if notify {
			w.notifyWatchedUser(ix.GuildID, spec.targetID, spec.alertType)
		}
	})
	v.Button(t("common.cancel", lang), discord.ButtonStyleSecondary, "", -1, func(ix *Ix) {
		v.Stop()
		ix.Update(Msg{Embeds: []discord.Embed{cancelled()}, ClearView: true})
	})
	v.onTimeout = func() { cmdIx.EditOriginal(Msg{Embeds: []discord.Embed{cancelled()}, ClearView: true}) }
	return v
}

// saveWatch persists a validated watch; returns the confirmation (or error)
// embed and whether the watched member should now be DM'd.
func (w *Watch) saveWatch(ix *Ix, lang string, spec watchSpec, prior *db.Watch) (discord.Embed, bool) {
	b := w.b
	var channelID *int64
	if !spec.dm {
		id := i64(spec.destID)
		channelID = &id
	}
	seq, ok := b.DB.AddWatch(i64(ix.GuildID), spec.targetType, spec.targetID, spec.alertType,
		spec.threshold, channelID, i64(ix.User.ID), spec.dm)
	if !ok {
		return errorEmbed(t("watch.err_generic", lang), lang), false
	}
	w.refreshWatchGuilds()

	// Reconfiguring keeps the row id, so clear its online_return cooldown.
	if prior != nil {
		w.mu.Lock()
		for k := range w.onlineCooldown {
			if k.watchID == prior.ID {
				delete(w.onlineCooldown, k)
			}
		}
		w.mu.Unlock()
	}

	target := mentionUser(spec.targetID)
	if spec.targetType == "role" {
		target = mentionRole(spec.targetID)
	}
	dest := t("watch.dest_dm", lang)
	if !spec.dm {
		dest = mentionChannel(i64(spec.destID))
	}
	var desc string
	if spec.alertType == "online_return" {
		extra := ""
		if spec.threshold != nil && *spec.threshold != 0 {
			extra = t("watch.added_online_extra", lang, "duration", formatDuration(*spec.threshold))
		}
		desc = t("watch.added_online", lang, "target", target, "channel", dest, "extra", extra)
	} else {
		desc = t("watch.added_offline", lang, "target", target, "channel", dest, "duration", formatDuration(*spec.threshold))
	}

	if spec.dm {
		desc += t("watch.dm_note", lang)
	} else if ch, ok := b.guildChannel(ix.GuildID, spec.destID); ok && !b.selfCanSend(ch) {
		desc += t("watch.warn_cannot_send", lang, "channel", dest)
	}

	if prior != nil {
		if prior.CreatedBy != 0 && prior.CreatedBy != i64(ix.User.ID) {
			desc += t("watch.replaced_existing_other", lang, "user", mentionUser(prior.CreatedBy))
		} else {
			desc += t("watch.replaced_existing", lang)
		}
	}

	notify := spec.targetType == "user" && prior == nil
	if notify {
		desc += t("watch.notified_target", lang, "target", target)
	}

	e := newEmbed(t("watch.added_title", lang), ColorGreen)
	e.Description = desc
	e.Footer = &discord.EmbedFooter{Text: fmt.Sprintf("#%d", seq)}
	mode := "channel"
	if spec.dm {
		mode = "dm"
	}
	wlog.Info("Watch #%d (%s, %s:%d, %s) created in guild %s by %s",
		seq, spec.alertType, spec.targetType, spec.targetID, mode, ix.GuildName(), userTag(ix.User))
	return e, notify
}

// notifyWatchedUser DMs a member that an admin set a watch on them.
func (w *Watch) notifyWatchedUser(guildID snowflake.ID, userID int64, alertType string) {
	b := w.b
	user, ok := b.findUser(guildID, sf(userID))
	if !ok {
		return
	}
	guildName := b.guildName(guildID)
	lang := guildLang(b.DB.GetGuildConfig(i64(guildID)))
	e := newEmbed(t("watch.dm_notify_title", lang), ColorBlurple)
	e.Description = t("watch.dm_notify_body", lang, "guild", guildName)
	err := b.sendDM(user.ID, discord.MessageCreate{Embeds: []discord.Embed{e}})
	switch {
	case err == nil:
		wlog.Info("Notified watched user %d of new %s watch in guild %s", userID, alertType, guildName)
	case isForbidden(err):
		wlog.Info("Could not notify watched user %d (DMs closed) in guild %s", userID, guildName)
	default:
		wlog.Warn("Failed to notify watched user %d in guild %s: %v", userID, guildName, err)
	}
}

func (w *Watch) cmdList(ix *Ix) {
	lang, ok := w.ensureAdmin(ix)
	if !ok {
		return
	}
	watches := w.b.DB.GetGuildWatches(i64(ix.GuildID))
	if len(watches) == 0 {
		e := newEmbed(t("watch.list_title", lang), ColorBlue)
		e.Description = t("watch.list_empty", lang)
		ix.Respond(embedMsg(e, true))
		return
	}
	roleName := adminRoleName(ix.guildConfig())
	chunked := w.b.isChunked(ix.GuildID)
	var lines []string
	for i := range watches {
		wt := &watches[i]
		line := w.formatWatchLine(wt, lang, i64(ix.User.ID))
		// Only judge "paused" once the member cache is complete.
		if wt.DeliverDM && chunked && !w.dmRecipientOK(ix.GuildID, wt, roleName) {
			line += t("watch.dm_paused", lang, "user", mentionUser(wt.CreatedBy))
		}
		lines = append(lines, line)
	}
	var embeds []discord.Embed
	for _, page := range chunk(lines, watchesPerPage) {
		e := newEmbed(t("watch.list_title", lang), ColorBlue)
		e.Description = strings.Join(page, "\n")
		embeds = append(embeds, e)
	}
	if len(embeds) == 1 {
		ix.Respond(embedMsg(embeds[0], true))
		return
	}
	ix.Respond(Msg{Embeds: embeds[:1], View: w.b.newPaginationView(embeds, ix.User.ID, lang, nil), Ephemeral: true})
}

func (w *Watch) formatWatchLine(wt *db.Watch, lang string, viewerID int64) string {
	target := mentionUser(wt.TargetID)
	if wt.TargetType == "role" {
		target = mentionRole(wt.TargetID)
	}
	var channel string
	switch {
	case wt.DeliverDM && wt.CreatedBy == viewerID:
		channel = t("watch.dest_dm", lang)
	case wt.DeliverDM:
		channel = t("watch.dest_dm_other", lang, "user", mentionUser(wt.CreatedBy))
	case wt.ChannelID != nil && *wt.ChannelID != 0:
		channel = mentionChannel(*wt.ChannelID)
	default:
		channel = "?"
	}
	if wt.AlertType == "online_return" {
		extra := ""
		if wt.ThresholdSeconds != nil && *wt.ThresholdSeconds != 0 {
			extra = t("watch.list_extra_after", lang, "duration", formatDuration(*wt.ThresholdSeconds))
		}
		return t("watch.list_line_online", lang, "id", wt.Seq, "target", target, "extra", extra, "channel", channel)
	}
	state := t("watch.state_armed", lang)
	if wt.State == "triggered" {
		state = t("watch.state_triggered", lang)
	}
	return t("watch.list_line_offline", lang, "id", wt.Seq, "target", target,
		"duration", formatDuration(thresholdOf(wt)), "channel", channel, "state", state)
}

func thresholdOf(wt *db.Watch) int64 {
	if wt.ThresholdSeconds == nil {
		return 0
	}
	return *wt.ThresholdSeconds
}

// targetLabel is a readable name for a watch's target (cache only).
func (w *Watch) targetLabel(guildID snowflake.ID, wt *db.Watch) string {
	if wt.TargetType == "role" {
		if r, ok := w.b.Client.Caches.Role(guildID, sf(wt.TargetID)); ok {
			return "@" + r.Name
		}
		return fmt.Sprintf("role %d", wt.TargetID)
	}
	if m, ok := w.b.Client.Caches.Member(guildID, sf(wt.TargetID)); ok {
		return "@" + memberDisplayName(m)
	}
	return fmt.Sprintf("user %d", wt.TargetID)
}

var (
	userMentionRe = regexp.MustCompile(`^<@!?(\d+)>$`)
	roleMentionRe = regexp.MustCompile(`^<@&(\d+)>$`)
)

// resolveTarget maps free text to a watch target. targetType "" with ok
// means a bare snowflake (match either type); ok=false when nothing matched.
func (w *Watch) resolveTarget(guildID snowflake.ID, raw string) (targetType string, id int64, ok bool) {
	parse := func(s string) (int64, bool) {
		v, err := strconv.ParseInt(s, 10, 64)
		return v, err == nil
	}
	if m := userMentionRe.FindStringSubmatch(raw); m != nil {
		id, ok := parse(m[1])
		return "user", id, ok
	}
	if m := roleMentionRe.FindStringSubmatch(raw); m != nil {
		id, ok := parse(m[1])
		return "role", id, ok
	}
	if isASCIIDigits(raw) {
		id, ok := parse(raw)
		return "", id, ok
	}
	if m := w.b.DB.FindMemberByName(i64(guildID), raw); m != nil {
		return "user", m.UserID, true
	}
	lower := strings.ToLower(raw)
	for _, r := range w.b.guildRoles(guildID) {
		if strings.ToLower(r.Name) == lower {
			return "role", i64(r.ID), true
		}
	}
	return "", 0, false
}

func (w *Watch) removeAutocomplete(ix *Ix, current string) []discord.AutocompleteChoice {
	if ix.GuildID == 0 {
		return nil
	}
	watches := w.b.DB.GetGuildWatches(i64(ix.GuildID))
	cur := strings.TrimLeft(strings.ToLower(current), "#")
	var out []discord.AutocompleteChoice
	for i := range watches {
		wt := &watches[i]
		kind := "offline"
		if wt.AlertType == "online_return" {
			kind = "online"
		}
		seq := strconv.FormatInt(wt.Seq, 10)
		name := runeCut(fmt.Sprintf("#%d · %s · %s", wt.Seq, kind, w.targetLabel(ix.GuildID, wt)), 100)
		if cur == "" || strings.Contains(seq, cur) || strings.Contains(strings.ToLower(name), cur) {
			out = append(out, discord.AutocompleteChoiceString{Name: name, Value: seq})
		}
		if len(out) >= 25 {
			break
		}
	}
	return out
}

func (w *Watch) cmdRemove(ix *Ix) {
	lang, ok := w.ensureAdmin(ix)
	if !ok {
		return
	}
	b := w.b
	raw := strings.TrimLeft(strings.TrimSpace(ix.cmd.String("target")), "#")
	watches := b.DB.GetGuildWatches(i64(ix.GuildID))
	if len(watches) == 0 {
		w.replyError(ix, t("watch.list_empty", lang), lang)
		return
	}

	// An existing watch number wins; otherwise resolve a target and remove
	// every watch on it.
	var matched []db.Watch
	byNum := false
	num, numErr := strconv.ParseInt(raw, 10, 64)
	if isASCIIDigits(raw) && numErr == nil {
		for _, wt := range watches {
			if wt.Seq == num {
				matched = append(matched, wt)
			}
		}
		byNum = len(matched) > 0
	}
	if !byNum {
		tgtType, tgtID, ok := w.resolveTarget(ix.GuildID, raw)
		if ok {
			for _, wt := range watches {
				if wt.TargetID == tgtID && (tgtType == "" || wt.TargetType == tgtType) {
					matched = append(matched, wt)
				}
			}
		}
	}
	if len(matched) == 0 {
		w.replyError(ix, t("watch.remove_not_found", lang, "query", raw), lang)
		return
	}

	removed := 0
	ids := make([]int64, len(matched))
	for i, wt := range matched {
		ids[i] = wt.ID
		if b.DB.RemoveWatch(i64(ix.GuildID), wt.ID) {
			removed++
		}
	}
	w.refreshWatchGuilds()

	var msg string
	if byNum {
		msg = t("watch.removed", lang, "id", num)
	} else {
		msg = t("watch.removed_target", lang, "count", removed, "target", w.targetLabel(ix.GuildID, &matched[0]))
	}
	ix.Respond(embedMsg(successEmbed(msg, lang), true))
	wlog.Info("Removed %d watch(es) (%s) in guild %s by %s", removed, pyIntList(ids), ix.GuildName(), userTag(ix.User))
}

// ==================== online_return firing + re-arm ====================

// onMemberOnline fires online_return watches for a returning member and
// re-arms their offline_for watches. prev is the last_seen before this
// presence overwrote it (0 = was already online, nil = no prior row).
func (w *Watch) onMemberOnline(guildID, userID snowflake.ID, prev *int64) {
	goSafe("watch online return", func() { w.handleOnline(guildID, userID, prev) })
}

func (w *Watch) handleOnline(guildID, userID snowflake.ID, prev *int64) {
	b := w.b
	uid := i64(userID)
	if b.OptedOut.Has(uid) || b.NoWatch.Has(uid) {
		return
	}
	member, ok := b.Client.Caches.Member(guildID, userID)
	if !ok {
		return
	}
	var roleIDs []int64
	for _, r := range b.memberRoles(guildID, member.RoleIDs) {
		if r.ID != guildID {
			roleIDs = append(roleIDs, i64(r.ID))
		}
	}
	now := nowUnix()
	var away *int64
	if prev != nil && *prev > 0 {
		a := now - *prev
		away = &a
	}
	defer func() {
		if r := recover(); r != nil {
			wlog.Error("Error handling online return for %s in guild %s: %v", userTag(member.User), b.guildName(guildID), r)
		}
	}()

	// Fire only on a genuine return (prev > 0, or no prior row), and not
	// during the post-connect grace window.
	if now >= w.suppressOnlineUntil.Load() && (prev == nil || *prev != 0) {
		for _, wt := range b.DB.GetWatchesForMember(i64(guildID), uid, roleIDs, "online_return") {
			threshold := thresholdOf(&wt)
			if threshold != 0 && (away == nil || *away < threshold) {
				continue
			}
			if away != nil && *away < onlineMinAwaySecs {
				continue
			}
			key := cooldownID{wt.ID, uid}
			var reported *int64
			if threshold != 0 {
				reported = away
			}
			w.mu.Lock()
			if last, ok := w.onlineCooldown[key]; ok && last != 0 && now-last < onlineCooldownSecs {
				w.mu.Unlock()
				continue
			}
			w.onlineCooldown[key] = now
			if wt.TargetType == "role" {
				d := w.roleReturns[wt.ID]
				if d == nil {
					d = &roleDigest{since: now, members: map[int64]*int64{}}
					w.roleReturns[wt.ID] = d
				}
				if _, seen := d.members[uid]; !seen {
					d.order = append(d.order, uid)
				}
				d.members[uid] = reported
				w.mu.Unlock()
				continue
			}
			w.mu.Unlock()
			w.fireAlert(guildID, &wt, mentionUser(uid), alert{kind: alertOnline, duration: reported})
		}
	}

	// Re-arm this member's offline_for watches.
	for _, wt := range b.DB.GetWatchesForMember(i64(guildID), uid, roleIDs, "offline_for") {
		if wt.TargetType == "user" {
			if wt.State != "armed" {
				armed := "armed"
				b.DB.UpdateWatchFireState(wt.ID, &armed, nil)
			}
			continue
		}
		fired := firedSet(&wt)
		if fired[uid] {
			delete(fired, uid)
			s := dumpSortedSet(fired)
			b.DB.UpdateWatchFireState(wt.ID, nil, &s)
		}
	}
}

func firedSet(wt *db.Watch) map[int64]bool {
	out := map[int64]bool{}
	if wt.FiredTargets != nil && *wt.FiredTargets != "" {
		ids, _ := db.LoadInts(*wt.FiredTargets)
		for _, id := range ids {
			out[id] = true
		}
	}
	return out
}

// dumpSortedSet is json.dumps(sorted(ids)).
func dumpSortedSet(set map[int64]bool) string {
	ids := make([]int64, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return db.DumpInts(ids)
}

// ==================== offline_for sweep ====================

func (w *Watch) sweepLoop() {
	select {
	case <-w.b.readyCh:
	case <-w.b.ctx.Done():
		return
	}
	ticker := time.NewTicker(offlineSweepInterval)
	defer ticker.Stop()
	for {
		w.checkOfflineWatches()
		select {
		case <-ticker.C:
		case <-w.b.ctx.Done():
			return
		}
	}
}

func (w *Watch) checkOfflineWatches() {
	defer func() {
		if r := recover(); r != nil {
			wlog.Error("Error in check_offline_watches: %v", r)
		}
	}()
	now := nowUnix()
	w.mu.Lock()
	cutoff := now - onlineCooldownSecs
	for k, v := range w.onlineCooldown {
		if v < cutoff {
			delete(w.onlineCooldown, k)
		}
	}
	w.mu.Unlock()

	w.flushRoleDigests(now)

	for _, wt := range w.b.DB.GetOfflineWatches() {
		func() {
			defer func() {
				if r := recover(); r != nil {
					wlog.Error("Error sweeping watch #%d: %v", wt.ID, r)
				}
			}()
			guildID := sf(wt.GuildID)
			if _, ok := w.b.Client.Caches.Guild(guildID); !ok || thresholdOf(&wt) == 0 {
				return
			}
			if wt.TargetType == "user" {
				w.sweepUserWatch(guildID, &wt, now)
			} else {
				w.sweepRoleWatch(guildID, &wt, now)
			}
		}()
	}
}

// flushRoleDigests posts each role digest that is roleDigestSecs old.
func (w *Watch) flushRoleDigests(now int64) {
	w.mu.Lock()
	due := map[int64]*roleDigest{}
	for id, d := range w.roleReturns {
		if now-d.since >= roleDigestSecs {
			due[id] = d
			delete(w.roleReturns, id) // later returns start a new digest
		}
	}
	w.mu.Unlock()
	for id, d := range due {
		func() {
			defer func() {
				if r := recover(); r != nil {
					wlog.Error("Error posting role digest for watch %d: %v", id, r)
				}
			}()
			wt := w.b.DB.GetWatch(id)
			if wt == nil {
				return
			}
			guildID := sf(wt.GuildID)
			if _, ok := w.b.Client.Caches.Guild(guildID); !ok {
				return
			}
			var members []alertMember
			for _, uid := range d.order {
				if w.b.OptedOut.Has(uid) || w.b.NoWatch.Has(uid) {
					continue
				}
				members = append(members, alertMember{mentionUser(uid), d.members[uid]})
			}
			if len(members) == 0 {
				return
			}
			label := mentionRole(wt.TargetID)
			if r, ok := w.b.Client.Caches.Role(guildID, sf(wt.TargetID)); ok {
				label = "@" + r.Name
			}
			w.fireAlert(guildID, wt, label, alert{kind: alertRoleOnline, members: members})
		}()
	}
}

func (w *Watch) sweepUserWatch(guildID snowflake.ID, wt *db.Watch, now int64) {
	b := w.b
	if b.NoWatch.Has(wt.TargetID) {
		return
	}
	m := b.DB.GetMember(i64(guildID), wt.TargetID)
	// A target who left keeps the watch, untouched, until they rejoin.
	if m == nil || !m.IsActive {
		return
	}
	if m.LastSeen == nil || *m.LastSeen == 0 { // online or never seen: re-arm
		if wt.State != "armed" {
			armed := "armed"
			b.DB.UpdateWatchFireState(wt.ID, &armed, nil)
		}
		return
	}
	if wt.State == "triggered" {
		return
	}
	threshold := thresholdOf(wt)
	if now-*m.LastSeen >= threshold {
		// A paused DM watch stays armed, so it fires once resumed.
		if w.fireAlert(guildID, wt, mentionUser(wt.TargetID), alert{kind: alertOffline, duration: &threshold}) {
			triggered := "triggered"
			b.DB.UpdateWatchFireState(wt.ID, &triggered, nil)
		}
	}
}

func (w *Watch) sweepRoleWatch(guildID snowflake.ID, wt *db.Watch, now int64) {
	b := w.b
	roleID := sf(wt.TargetID)
	role, ok := b.Client.Caches.Role(guildID, roleID)
	if !ok {
		return
	}
	var members []int64
	for m := range b.Client.Caches.Members(guildID) {
		if m.User.Bot || b.OptedOut.Has(i64(m.User.ID)) || b.NoWatch.Has(i64(m.User.ID)) {
			continue
		}
		for _, r := range m.RoleIDs {
			if r == roleID {
				members = append(members, i64(m.User.ID))
				break
			}
		}
	}
	if len(members) == 0 {
		return
	}
	sort.Slice(members, func(i, j int) bool { return members[i] < members[j] })
	lastSeen := b.DB.GetMembersLastSeen(i64(guildID), members)

	threshold := thresholdOf(wt)
	oldFired := firedSet(wt)
	newFired := map[int64]bool{}
	var newly []alertMember
	for _, uid := range members {
		ls := lastSeen[uid]
		if ls != nil && *ls != 0 && now-*ls >= threshold {
			newFired[uid] = true
			if !oldFired[uid] {
				newly = append(newly, alertMember{mention: mentionUser(uid)})
			}
		}
	}
	if len(newly) > 0 {
		if !w.fireAlert(guildID, wt, "@"+role.Name, alert{kind: alertRoleOffline, duration: &threshold, members: newly}) {
			return // paused DM watch: keep fired_targets so these alert once resumed
		}
	}
	if !sameSet(newFired, oldFired) {
		s := dumpSortedSet(newFired)
		b.DB.UpdateWatchFireState(wt.ID, nil, &s)
	}
}

func sameSet(a, b map[int64]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// ==================== delivery ====================

// dmRecipientOK: a DM watch delivers only to its creator while they are a
// cached member with bot-admin rights (else the watch is paused).
func (w *Watch) dmRecipientOK(guildID snowflake.ID, wt *db.Watch, roleName string) bool {
	_, ok := w.dmRecipient(guildID, wt, roleName)
	return ok
}

func (w *Watch) dmRecipient(guildID snowflake.ID, wt *db.Watch, roleName string) (discord.Member, bool) {
	m, ok := w.b.Client.Caches.Member(guildID, sf(wt.CreatedBy))
	if !ok || !hasBotAdminRole(w.b.cachedMemberCtx(guildID, m), roleName) {
		return discord.Member{}, false
	}
	return m, true
}

type alertKind int

const (
	alertOnline alertKind = iota
	alertOffline
	alertRoleOnline
	alertRoleOffline
)

type alertMember struct {
	mention string
	away    *int64
}

type alert struct {
	kind     alertKind
	duration *int64 // away time (online) or threshold (offline)
	members  []alertMember
}

// fireAlert builds one alert and delivers it to the watch's channel, or as
// a DM to its creator. Returns false only when a DM watch is paused, so the
// caller leaves its fire-state untouched.
func (w *Watch) fireAlert(guildID snowflake.ID, wt *db.Watch, target string, a alert) bool {
	b := w.b
	cfg := b.DB.GetGuildConfig(i64(guildID))
	lang := guildLang(cfg)
	roleName := adminRoleName(cfg)
	guildName := b.guildName(guildID)

	listing := ""
	if len(a.members) > 0 {
		var lines []string
		for i, m := range a.members {
			if i >= alertListMax {
				break
			}
			line := "• " + m.mention
			if m.away != nil && *m.away != 0 {
				line += t("watch.alert_away_suffix", lang, "duration", formatAway(*m.away))
			}
			lines = append(lines, line)
		}
		if len(a.members) > alertListMax {
			lines = append(lines, t("watch.alert_more", lang, "count", len(a.members)-alertListMax))
		}
		listing = strings.Join(lines, "\n")
	}

	var title, desc string
	var color int
	switch a.kind {
	case alertRoleOnline:
		title, color = t("watch.alert_online_title", lang), ColorGreen
		desc = t("watch.alert_role_online_desc", lang, "role", target, "members", listing)
	case alertRoleOffline:
		title, color = t("watch.alert_offline_title", lang), ColorOrange
		desc = t("watch.alert_role_offline_desc", lang, "role", target, "duration", formatDuration(*a.duration), "members", listing)
	case alertOffline:
		title, color = t("watch.alert_offline_title", lang), ColorOrange
		desc = t("watch.alert_offline_desc", lang, "target", target, "duration", formatDuration(*a.duration))
	default:
		title, color = t("watch.alert_online_title", lang), ColorGreen
		if a.duration != nil && *a.duration != 0 {
			desc = t("watch.alert_online_desc_away", lang, "target", target, "duration", formatAway(*a.duration))
		} else {
			desc = t("watch.alert_online_desc", lang, "target", target)
		}
	}
	e := newEmbed(title, color)
	e.Description = desc

	removeHint := t("watch.alert_footer", lang, "seq", wt.Seq)
	origin := fmt.Sprintf("(%s, %s:%d) in guild %s", wt.AlertType, wt.TargetType, wt.TargetID, guildName)

	if wt.DeliverDM {
		user, ok := w.dmRecipient(guildID, wt, roleName)
		if !ok {
			wlog.Debug("Watch #%d %s: DM recipient %d is not a bot admin in this guild (or not cached yet); paused",
				wt.Seq, origin, wt.CreatedBy)
			return false
		}
		e.Footer = &discord.EmbedFooter{Text: guildName + " • " + removeHint}
		err := b.sendDM(user.User.ID, discord.MessageCreate{Embeds: []discord.Embed{e}})
		switch {
		case err == nil:
			wlog.Info("Watch #%d fired %s -> DM %s", wt.Seq, origin, userTag(user.User))
		case isForbidden(err):
			wlog.Warn("Watch #%d: can't DM %s (DMs closed); alert dropped", wt.Seq, userTag(user.User))
		default:
			wlog.Error("Watch #%d: failed to DM %d: %v", wt.ID, wt.CreatedBy, err)
		}
		return true
	}

	e.Footer = &discord.EmbedFooter{Text: removeHint}
	var ch discord.GuildChannel
	ok := false
	if wt.ChannelID != nil && *wt.ChannelID != 0 {
		ch, ok = b.guildChannel(guildID, sf(*wt.ChannelID))
	}
	if !ok {
		wlog.Warn("Watch #%d: channel %s not found in guild %s; can't deliver", wt.ID, pyOptInt(wt.ChannelID), guildName)
		w.recordDeliveryFailure(guildID, wt)
		return true
	}
	if !b.selfCanSend(ch) {
		wlog.Warn("Watch #%d: no send permission in channel %d; can't deliver", wt.ID, ch.ID())
		w.recordDeliveryFailure(guildID, wt)
		return true
	}
	_, err := b.Client.Rest.CreateMessage(ch.ID(), discord.MessageCreate{Embeds: []discord.Embed{e}})
	switch {
	case err == nil:
		wlog.Info("Watch #%d fired %s -> #%s", wt.Seq, origin, ch.Name())
		if wt.FailedDeliveries != 0 {
			b.DB.ResetWatchDeliveryFailures(wt.ID)
		}
	case isForbidden(err) || isNotFound(err):
		wlog.Warn("Watch #%d: can't send alert to channel %d: %v", wt.ID, ch.ID(), err)
		w.recordDeliveryFailure(guildID, wt)
	default:
		// Transient (5xx, rate limit, network): not counted towards removal.
		wlog.Error("Watch #%d: failed to send alert to channel %d: %v", wt.ID, ch.ID(), err)
	}
	return true
}

// pyOptInt formats an optional int like Python's f-string (None when nil).
func pyOptInt(v *int64) string {
	if v == nil {
		return "None"
	}
	return strconv.FormatInt(*v, 10)
}

// recordDeliveryFailure counts a failed channel delivery and removes the
// watch at maxDeliveryFailures.
func (w *Watch) recordDeliveryFailure(guildID snowflake.ID, wt *db.Watch) {
	b := w.b
	if b.Client.Caches.IsGuildUnavailable(guildID) {
		return // channels can be missing from cache during an outage
	}
	failures := b.DB.RecordWatchDeliveryFailure(wt.ID)
	if failures < maxDeliveryFailures {
		return
	}
	if b.DB.RemoveWatch(i64(guildID), wt.ID) {
		w.refreshWatchGuilds()
		wlog.Info("Removed watch #%d (%s, %s:%d) in guild %s: %d consecutive failed deliveries to channel %s",
			wt.Seq, wt.AlertType, wt.TargetType, wt.TargetID, b.guildName(guildID), failures, pyOptInt(wt.ChannelID))
	}
}
