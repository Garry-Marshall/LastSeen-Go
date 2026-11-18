package bot

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/snowflake/v2"

	"lastseen/internal/db"
	"lastseen/internal/logx"
	"lastseen/internal/tzdb"
)

var tlog = logx.Get("tracking")

const (
	flushInterval            = 30 * time.Second
	cleanupInterval          = 24 * time.Hour
	reportCheckInterval      = time.Minute
	reportJitterMinutes      = 10
	walCheckpointInterval    = 15 * time.Minute
	walTruncateEveryN        = 4
	statusRotateInterval     = 3 * time.Minute
	statsPushInterval        = 10 * time.Minute
	returnThresholdSeconds   = 30 * 86400
	enumChunkAttempts        = 3
	enumChunkRetryDelay      = 60 * time.Second
	presenceQueueMax         = 10000
	presenceBatchMax         = 500
	massDepartureMin         = 50
	massDepartureRatio       = 0.2
	heartbeatKey             = "heartbeat"
	provisionalAccountFlag   = 1 << 23
	maxActivityBufferEntries = 10000
	presenceStripes          = 64
)

// statusMessages are cycled as the bot's custom status (emoji, text).
var statusMessages = [][2]string{
	{"", "Listening to /lastseen"},
	{"", "Watching you"},
	{"", "Listening to /help"},
	{"", "Listening to /whois"},
	{"", "Listening to /mystats"},
	{"🌐", "lastseen.bot"},
}

// MemberDiff lists the user ids whose stored state disagrees with the cache.
type MemberDiff struct {
	Online   []int64 // online now, stored as offline or never seen
	Offline  []int64 // offline now, stored as online (last_seen = 0)
	Rejoined []int64 // in the guild, stored as departed
	New      []int64 // in the guild, not stored at all
	Departed []int64 // stored as active, no longer in the guild
}

// diffMemberState compares a guild's member cache {user_id: online} with its
// stored rows. Pure, so it is tested without Discord.
func diffMemberState(cacheOnline map[int64]bool, rows map[int64]db.PresenceRow) MemberDiff {
	var d MemberDiff
	for uid, online := range cacheOnline {
		row, ok := rows[uid]
		switch {
		case !ok:
			d.New = append(d.New, uid)
		case !row.IsActive:
			d.Rejoined = append(d.Rejoined, uid)
		case online && (row.LastSeen == nil || *row.LastSeen != 0):
			d.Online = append(d.Online, uid)
		case !online && row.LastSeen != nil && *row.LastSeen == 0:
			d.Offline = append(d.Offline, uid)
		}
	}
	for uid, row := range rows {
		if _, ok := cacheOnline[uid]; row.IsActive && !ok {
			d.Departed = append(d.Departed, uid)
		}
	}
	for _, l := range [][]int64{d.Online, d.Offline, d.Rejoined, d.New, d.Departed} {
		slices.Sort(l)
	}
	return d
}

type dailyKey struct{ guild, user, date int64 }
type hourlyKey struct {
	guild, user, ts int64
	hour            int
}
type returnRec struct{ guild, user, away, ts int64 }

type presenceItem struct {
	guildID, userID snowflake.ID
	wentOffline     bool
	ts              int64
}

// guildReconcile is a presence-queue item: reconcile one guild against a
// cache snapshot, ordered with the presence events around it.
type guildReconcile struct {
	guildID   snowflake.ID
	guildName string
	online    map[int64]bool
	members   map[int64]discord.Member
	since     int64
}

type queueItem struct {
	presence  *presenceItem
	reconcile *guildReconcile
}

type onlineResult struct {
	guildID, userID snowflake.ID
	prev            *int64
	ts              int64
}

// Tracking records joins, leaves, profile changes, presence and message
// activity, and runs the periodic maintenance loops.
type Tracking struct {
	b  *Bot
	db *db.DB

	bufMu        sync.Mutex
	daily        map[dailyKey]int64
	hourly       map[hourlyKey]int64
	failedDaily  map[dailyKey]int64
	failedHourly map[hourlyKey]int64
	returns      []returnRec

	// flushMu serialises flushes (the 30s loop and a forced flush).
	flushMu sync.Mutex

	order *memberOrder

	staleCleanupDone atomic.Bool
	enumMu           sync.Mutex
	enumerating      map[int64]bool

	stateMu           sync.Mutex
	startupStaleSince *int64
	staleSince        map[int64]int64
	downShards        map[int]bool
	reconciled        map[int64]bool
	reconcileQueued   map[int64]bool

	// stripes keep a presence event and its status-cache update atomic with
	// respect to a guild snapshot (see onGuildChunked).
	stripes [presenceStripes]sync.RWMutex

	queue   chan queueItem
	dropped atomic.Int64

	statusIdx int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

func newTracking(b *Bot) *Tracking {
	tr := &Tracking{
		b:               b,
		db:              b.DB,
		daily:           map[dailyKey]int64{},
		hourly:          map[hourlyKey]int64{},
		failedDaily:     map[dailyKey]int64{},
		failedHourly:    map[hourlyKey]int64{},
		order:           newMemberOrder(),
		enumerating:     map[int64]bool{},
		staleSince:      map[int64]int64{},
		downShards:      map[int]bool{},
		reconciled:      map[int64]bool{},
		reconcileQueued: map[int64]bool{},
		queue:           make(chan queueItem, presenceQueueMax),
	}
	tr.ctx, tr.cancel = context.WithCancel(context.Background())
	// The previous process's heartbeat bounds how stale the stored presence
	// data is; read it before the flush loop overwrites it.
	tr.startupStaleSince = b.DB.GetBotState(heartbeatKey)

	tr.spawn(tr.drainPresenceQueue)
	tr.loop("flush_activity_buffer", flushInterval, nil, func(int) { tr.flush() })
	tr.loop("cleanup_old_data", cleanupInterval, nil, func(int) { tr.cleanupOldData() })
	tr.loop("record_health_snapshots", 24*time.Hour, nil, func(int) { tr.recordHealthSnapshots() })
	tr.loop("check_scheduled_reports", reportCheckInterval, tr.alignReports, func(int) { tr.checkScheduledReports() })
	tr.loop("backup_database", time.Hour, nil, func(int) { tr.backupDatabase() })
	tr.loop("checkpoint_wal", walCheckpointInterval, nil, tr.checkpointWAL)
	tr.loop("rotate_status", statusRotateInterval, nil, func(int) { tr.rotateStatus() })
	if b.Cfg.StatsPushEnabled {
		tr.loop("push_stats_loop", statsPushInterval, nil, func(int) { b.pushStats() })
	}
	return tr
}

func (tr *Tracking) spawn(fn func()) {
	tr.wg.Add(1)
	go func() {
		defer tr.wg.Done()
		fn()
	}()
}

// waitReady blocks until the bot's first on_ready (false on shutdown).
func (tr *Tracking) waitReady() bool {
	select {
	case <-tr.b.readyCh:
		return true
	case <-tr.ctx.Done():
		return false
	}
}

// loop runs fn now and then every interval once the bot is ready; before
// runs once first. A panic in one iteration is
// logged and the loop continues.
func (tr *Tracking) loop(name string, interval time.Duration, before func() bool, fn func(iteration int)) {
	tr.spawn(func() {
		if !tr.waitReady() {
			return
		}
		if before != nil && !before() {
			return
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for i := 0; ; i++ {
			func() {
				defer recoverLog(name)
				fn(i)
			}()
			select {
			case <-ticker.C:
			case <-tr.ctx.Done():
				return
			}
		}
	})
}

func (tr *Tracking) sleep(d time.Duration) bool {
	select {
	case <-time.After(d):
		return true
	case <-tr.ctx.Done():
		return false
	}
}

func (tr *Tracking) isOptedOut(userID snowflake.ID) bool { return tr.b.OptedOut.Has(i64(userID)) }

// ==================== Positions and enumeration ====================

func (tr *Tracking) initializeMemberPositions(guildID snowflake.ID, name string, force bool) bool {
	defer recoverLog("initialize member positions")
	gid := i64(guildID)
	if !force && tr.db.GuildPositionsInitialized(gid) {
		tlog.Info("Member positions already initialized for guild %s", name)
		return true
	}
	tlog.Info("Starting member position initialization for guild %s...", name)
	members := tr.db.GetAllGuildMembers(gid)
	updated := 0
	if len(members) > 0 {
		// Ascending join_date; unknown (0/NULL) last. Stable, like sorted().
		sort.SliceStable(members, func(i, j int) bool {
			ki, kj := members[i].JoinDate, members[j].JoinDate
			if ki == nil || *ki == 0 {
				return false
			}
			if kj == nil || *kj == 0 {
				return true
			}
			return *ki < *kj
		})
		positions := make(map[int64]int64, len(members))
		for idx, m := range members {
			positions[m.UserID] = int64(idx + 1)
		}
		if tr.db.SetMemberJoinPositions(gid, positions) {
			updated = len(positions)
		}
	}
	if tr.db.MarkPositionsInitialized(gid) {
		tlog.Info("Initialized join positions for %d members in %s", updated, name)
		return true
	}
	tlog.Error("Failed to mark guild %d as positions-initialized", gid)
	return false
}

// startMemberEnumeration schedules a one-shot import of a guild's members,
// deduplicated per guild for the process lifetime.
func (tr *Tracking) startMemberEnumeration(guildID snowflake.ID) {
	tr.enumMu.Lock()
	if tr.enumerating[i64(guildID)] {
		tr.enumMu.Unlock()
		return
	}
	tr.enumerating[i64(guildID)] = true
	tr.enumMu.Unlock()
	goSafe("member enumeration", func() { tr.processGuildMembers(guildID) })
}

func (tr *Tracking) forgetEnumeration(guildID snowflake.ID) {
	tr.enumMu.Lock()
	delete(tr.enumerating, i64(guildID))
	tr.enumMu.Unlock()
}

func (tr *Tracking) processGuildMembers(guildID snowflake.ID) {
	b := tr.b
	name := b.guildName(guildID)
	if !b.isChunked(guildID) {
		tlog.Info("Chunking %s before member enumeration...", name)
		delay := enumChunkRetryDelay
		ok := false
		for attempt := 1; attempt <= enumChunkAttempts; attempt++ {
			_, err := b.chunkGuild(guildID)
			if err == nil {
				ok = true
				break
			}
			tlog.Warn("Chunking %s failed (attempt %d/%d): %v", name, attempt, enumChunkAttempts, err)
			if attempt < enumChunkAttempts {
				if !tr.sleep(delay) {
					return
				}
				delay *= 2
			}
		}
		if !ok {
			tlog.Error("Gave up chunking %s; member enumeration will retry at the next (re)connect", name)
			tr.forgetEnumeration(guildID)
			return
		}
	}

	var members []discord.Member
	for m := range b.Client.Caches.Members(guildID) {
		members = append(members, m)
	}
	total := len(members)
	tlog.Info("Starting member enumeration for %s (%d cached members)...", name, total)

	const chunkSize = 100
	gid := i64(guildID)
	added, online := 0, 0
	for i := 0; i < total; i += chunkSize {
		for _, m := range members[i:min(i+chunkSize, total)] {
			if m.User.Bot || tr.isOptedOut(m.User.ID) {
				continue
			}
			tr.db.AddMember(gid, i64(m.User.ID), userTag(m.User), storedNickname(m), joinTimestamp(m, 0),
				b.memberRoleNames(guildID, m.RoleIDs))
			if b.isOnline(guildID, m.User.ID) {
				tr.db.UpdateLastSeen(gid, i64(m.User.ID), 0)
				online++
			}
			added++
		}
		if (i+chunkSize)%1000 == 0 || i+chunkSize >= total {
			tlog.Info("Processed %d/%d members from %s", min(i+chunkSize, total), total, name)
		}
	}
	tlog.Info("Completed member enumeration for %s: added %d members (%d online, %d offline)", name, added, online, added-online)
	tr.initializeMemberPositions(guildID, name, true)
}

// PurgeUserFromBuffers discards a user's buffered activity (/forgetme).
func (tr *Tracking) PurgeUserFromBuffers(userID int64) {
	tr.bufMu.Lock()
	defer tr.bufMu.Unlock()
	for _, m := range []map[dailyKey]int64{tr.daily, tr.failedDaily} {
		for k := range m {
			if k.user == userID {
				delete(m, k)
			}
		}
	}
	for _, m := range []map[hourlyKey]int64{tr.hourly, tr.failedHourly} {
		for k := range m {
			if k.user == userID {
				delete(m, k)
			}
		}
	}
	tr.returns = slices.DeleteFunc(tr.returns, func(r returnRec) bool { return r.user == userID })
}

// calculateAndSetJoinPosition ranks a member who joined while the bot was
// offline by their join date.
func (tr *Tracking) calculateAndSetJoinPosition(guildID snowflake.ID, m discord.Member) bool {
	if m.JoinedAt == nil || m.JoinedAt.IsZero() {
		tlog.Warn("Cannot calculate join position for %s - no join_date available", userTag(m.User))
		return false
	}
	gid, uid := i64(guildID), i64(m.User.ID)
	pos, ok := tr.db.CalculateJoinPosition(gid, m.JoinedAt.Unix())
	if !ok {
		tlog.Error("Failed to calculate join position for %s", userTag(m.User))
		return false
	}
	if tr.db.SetMemberJoinPosition(gid, uid, pos) {
		tlog.Info("Set join position %d for %s (joined late)", pos, userTag(m.User))
		return true
	}
	tlog.Error("Failed to set join position for %s", userTag(m.User))
	return false
}

// ensureMemberExists adds an untracked member; true if a row was added.
func (tr *Tracking) ensureMemberExists(guildID snowflake.ID, m discord.Member) bool {
	if tr.isOptedOut(m.User.ID) {
		return false
	}
	gid, uid := i64(guildID), i64(m.User.ID)
	// A guild not registered yet is populated by its enumeration later.
	if tr.db.GetGuildConfig(gid) == nil {
		return false
	}
	if tr.db.MemberExists(gid, uid) {
		return false
	}
	tr.db.AddMember(gid, uid, userTag(m.User), storedNickname(m), joinTimestamp(m, nowUnix()),
		tr.b.memberRoleNames(guildID, m.RoleIDs))
	if tr.db.GuildPositionsInitialized(gid) {
		tr.calculateAndSetJoinPosition(guildID, m)
	}
	return true
}

// ==================== Ready and guild lifecycle ====================

func (tr *Tracking) onReady() {
	b := tr.b
	self, _ := b.Client.Caches.SelfUser()
	all := b.guildIDs()
	tlog.Info("Bot logged in as %s", userTag(self.User))
	tlog.Info("Connected to %d guilds", len(all))

	if tr.staleCleanupDone.CompareAndSwap(false, true) {
		current := map[int64]bool{}
		for _, id := range all {
			current[i64(id)] = true
		}
		var stale []int64
		for _, id := range tr.db.GetAllGuildIDs() {
			if !current[id] {
				stale = append(stale, id)
			}
		}
		if len(stale) > 0 {
			tlog.Info("Found %d stale guild(s) in database, removing...", len(stale))
		}
		for _, id := range stale {
			if tr.db.RemoveGuildData(id) {
				tlog.Info("Removed stale guild %d from database", id)
			} else {
				tlog.Warn("Failed to remove stale guild %d", id)
			}
		}
	}

	tr.enumMu.Lock()
	enumerating := make(map[int64]bool, len(tr.enumerating))
	for k := range tr.enumerating {
		enumerating[k] = true
	}
	tr.enumMu.Unlock()

	missing := 0
	var needsEnum, needsPos []discord.CacheGuild
	for g := range b.Client.Caches.Guilds() {
		gid := i64(g.ID)
		if tr.db.GetGuildConfig(gid) == nil {
			tr.db.AddGuild(gid, g.Name, 10)
			missing++
			tlog.Info("Guild '%s' (ID: %d) was added to database (sync after connecting)", g.Name, gid)
		}
		if enumerating[gid] {
			continue
		}
		if !tr.db.GuildPositionsInitialized(gid) {
			if len(tr.db.GetAllGuildMembers(gid)) == 0 {
				needsEnum = append(needsEnum, g)
			} else {
				needsPos = append(needsPos, g)
			}
		}
	}
	if missing > 0 {
		tlog.Warn("Added %d missing guild(s) to database while syncing after connecting", missing)
	}
	for _, g := range needsEnum {
		tlog.Info("Guild %s has no members in database, scheduling member enumeration...", g.Name)
		tr.startMemberEnumeration(g.ID)
	}
	tlog.Info("Checking for guilds needing member position initialization...")
	for _, g := range needsPos {
		tlog.Info("Guild %s needs position initialization, scheduling background task...", g.Name)
		go tr.initializeMemberPositions(g.ID, g.Name, false)
	}
}

func (tr *Tracking) onGuildJoin(guildID snowflake.ID, name string) {
	goSafe("on_guild_join", func() {
		tlog.Info("Joined guild: %s (ID: %s)", name, guildID)
		tr.db.AddGuild(i64(guildID), name, tr.b.Cfg.DefaultInactiveDays)
		tr.startMemberEnumeration(guildID)
		tr.b.updateListings()
	})
}

func (tr *Tracking) onGuildRemove(guildID snowflake.ID, name string) {
	goSafe("on_guild_remove", func() {
		gid := i64(guildID)
		tlog.Info("Bot left guild: %s (%d). Cleaning up database...", name, gid)
		ok := tr.db.RemoveGuildData(gid)
		tr.forgetEnumeration(guildID)
		if ok {
			tlog.Info("Successfully wiped data for guild %d", gid)
			tr.bufMu.Lock()
			dailyPurged, hourlyPurged := 0, 0
			for k := range tr.daily {
				if k.guild == gid {
					delete(tr.daily, k)
					dailyPurged++
				}
			}
			for k := range tr.hourly {
				if k.guild == gid {
					delete(tr.hourly, k)
					hourlyPurged++
				}
			}
			for k := range tr.failedDaily {
				if k.guild == gid {
					delete(tr.failedDaily, k)
				}
			}
			for k := range tr.failedHourly {
				if k.guild == gid {
					delete(tr.failedHourly, k)
				}
			}
			tr.returns = slices.DeleteFunc(tr.returns, func(r returnRec) bool { return r.guild == gid })
			tr.bufMu.Unlock()
			if dailyPurged > 0 || hourlyPurged > 0 {
				tlog.Info("Purged %d daily and %d hourly buffered activity entries for guild %d", dailyPurged, hourlyPurged, gid)
			}
			purgeGuildState(guildID)
		} else {
			tlog.Error("Failed to wipe data for guild %d during on_guild_remove", gid)
		}
		tr.b.updateListings()
	})
}

func (tr *Tracking) onGuildUpdate(guildID snowflake.ID, oldName, newName string) {
	if oldName == newName {
		return
	}
	tlog.Info("Guild name changed from '%s' to '%s' (ID: %s)", oldName, newName, guildID)
	goSafe("on_guild_update", func() { tr.db.UpdateGuildName(i64(guildID), newName) })
}

// ==================== Member events ====================

func (tr *Tracking) onMemberJoin(guildID snowflake.ID, m discord.Member) {
	if m.User.Bot || tr.isOptedOut(m.User.ID) {
		return
	}
	b := tr.b
	guildName := b.guildName(guildID)
	tlog.Info("Member joined: %s in guild %s", userTag(m.User), guildName)

	gid, uid := i64(guildID), i64(m.User.ID)
	roles := b.memberRoleNames(guildID, m.RoleIDs)
	joinDate := joinTimestamp(m, nowUnix())
	nickname := storedNickname(m)
	username := userTag(m.User)
	online := b.isOnline(guildID, m.User.ID)

	tr.order.Go(memberKey{gid, uid}, func() {
		defer recoverLog("on_member_join")
		if existing := tr.db.GetMember(gid, uid); existing != nil {
			if existing.IsActive && existing.JoinDate != nil && *existing.JoinDate == joinDate {
				return // already stored by the presence update that followed the join
			}
			tlog.Info("Member %s is rejoining guild %s", username, guildName)
			tr.db.RejoinMember(gid, uid, username, nickname, joinDate, roles, online)
			return
		}
		tr.db.AddMember(gid, uid, username, nickname, joinDate, roles)
		if online {
			tr.db.UpdateLastSeen(gid, uid, 0)
		}
		if tr.db.GuildPositionsInitialized(gid) {
			if pos, ok := tr.db.CalculateJoinPosition(gid, joinDate); ok {
				tr.db.SetMemberJoinPosition(gid, uid, pos)
			}
		}
	})
}

func (tr *Tracking) onMemberRemove(guildID snowflake.ID, user discord.User, m discord.Member) {
	// Only members that were cached count as departures.
	if m.User.ID == 0 || user.Bot {
		return
	}
	b := tr.b
	guildName := b.guildName(guildID)
	tlog.Info("Member left: %s from guild %s", userTag(user), guildName)
	gid, uid := i64(guildID), i64(user.ID)
	now := nowUnix()
	tr.order.Go(memberKey{gid, uid}, func() {
		defer recoverLog("on_member_remove")
		data := tr.db.GetMember(gid, uid)
		if data == nil {
			tlog.Warn("Member %d not found in database for guild %d", uid, gid)
			return
		}
		tr.db.SetMemberInactive(gid, uid)
		tr.db.UpdateLastSeen(gid, uid, now)
		tr.db.SetMemberLeftDate(gid, uid, &now)
		cfg := tr.db.GetGuildConfig(gid)
		goSafe("leave notification", func() { tr.sendLeaveNotification(guildID, guildName, m, data, cfg, now) })
	})
}

func (tr *Tracking) sendLeaveNotification(guildID snowflake.ID, guildName string, m discord.Member, data *db.Member, cfg *db.GuildConfig, leftAt int64) {
	if cfg == nil || cfg.NotificationChannelID == nil || *cfg.NotificationChannelID == 0 {
		tlog.Info("No notification channel set for guild %s", guildName)
		return
	}
	// Only members matching track_only_roles trigger a notification.
	if cfg.TrackOnlyRoles != nil && *cfg.TrackOnlyRoles != "" {
		tracked, _ := db.LoadStrings(*cfg.TrackOnlyRoles)
		if len(tracked) > 0 {
			match := false
			for _, r := range data.Roles {
				if slices.Contains(tracked, r) {
					match = true
					break
				}
			}
			if !match {
				tlog.Debug("Member %s left %s but has no tracked role; skipping leave notification", userTag(m.User), guildName)
				return
			}
		}
	}
	chID := *cfg.NotificationChannelID
	ch, ok := tr.b.Client.Caches.Channel(sf(chID))
	if !ok {
		tlog.Warn("Notification channel %d not found", chID)
		return
	}
	if ch.GuildID() != guildID {
		tlog.Warn("Notification channel %d is not in guild %d", chID, i64(guildID))
		return
	}

	lang := guildLang(cfg)
	e := newEmbed(t("tracking.member_left.title", lang), ColorRed)
	e.Thumbnail = &discord.EmbedResource{URL: m.EffectiveAvatarURL()}
	var sb strings.Builder
	sb.WriteString(t("tracking.member_left.user_id", lang, "user_id", data.UserID))
	sb.WriteString(t("tracking.member_left.username", lang, "username", data.Username))
	nick := t("common.not_set", lang)
	if data.Nickname != nil && *data.Nickname != "" {
		nick = *data.Nickname
	}
	sb.WriteString(t("tracking.member_left.nickname", lang, "nickname", nick))
	sb.WriteString("\n")
	if roles := tr.b.memberRoles(guildID, m.RoleIDs); len(roles) > 1 {
		if top := roles[len(roles)-1]; top.Name != "@everyone" {
			sb.WriteString(t("tracking.member_left.highest_role", lang, "role", "<@&"+top.ID.String()+">"))
		}
	}
	sb.WriteString("\n")
	if data.JoinDate != nil && *data.JoinDate != 0 {
		sb.WriteString(t("tracking.member_left.joined", lang, "date", formatDT(*data.JoinDate, "F")))
		if days := int64(time.Since(time.Unix(*data.JoinDate, 0)) / (24 * time.Hour)); days > 0 {
			sb.WriteString(t("tracking.member_left.member_for", lang, "days", days))
		}
	}
	sb.WriteString(t("tracking.member_left.left_guild", lang, "date", formatDT(leftAt, "R")))
	e.Description = sb.String()

	if _, err := tr.b.Client.Rest.CreateMessage(ch.ID(), discord.MessageCreate{Embeds: []discord.Embed{e}}); err != nil {
		tlog.Error("Failed to send leave notification: %v", err)
	}
}

// onMemberUpdate records display name, username and role changes. disgo's
// OldMember is the true previous state, so username and global display name
// changes are handled here too, per guild.
func (tr *Tracking) onMemberUpdate(guildID snowflake.ID, before, after discord.Member) {
	if after.User.Bot || before.User.ID == 0 {
		return // bots, and members that were not cached
	}
	b := tr.b
	gid, uid := i64(guildID), i64(after.User.ID)
	beforeDisplay, afterDisplay := storedNickname(before), storedNickname(after)
	usernameChanged := userTag(before.User) != userTag(after.User)
	newUsername := userTag(after.User)

	beforeRoles := b.memberRoles(guildID, before.RoleIDs)
	afterRoles := b.memberRoles(guildID, after.RoleIDs)
	rolesChanged := !slices.EqualFunc(beforeRoles, afterRoles, func(x, y discord.Role) bool { return x.ID == y.ID })
	var roleNames, added, removed []string
	if rolesChanged {
		roleNames = b.memberRoleNames(guildID, after.RoleIDs)
		names := func(rs []discord.Role) map[string]bool {
			s := map[string]bool{}
			for _, r := range rs {
				if r.Name != "" {
					s[r.Name] = true
				}
			}
			return s
		}
		valid := func(n string) bool { return n != "" && n != "@everyone" && strings.TrimSpace(n) != "" }
		bn, an := names(beforeRoles), names(afterRoles)
		for _, r := range afterRoles {
			if !bn[r.Name] && valid(r.Name) && !slices.Contains(added, r.Name) {
				added = append(added, r.Name)
			}
		}
		for _, r := range beforeRoles {
			if !an[r.Name] && valid(r.Name) && !slices.Contains(removed, r.Name) {
				removed = append(removed, r.Name)
			}
		}
	}
	if !usernameChanged && !rolesChanged && ptrStrEq(beforeDisplay, afterDisplay) {
		// Nothing stored changed; still make sure the member is tracked.
		tr.order.Go(memberKey{gid, uid}, func() {
			defer recoverLog("on_member_update")
			tr.ensureMemberExists(guildID, after)
		})
		return
	}

	tr.order.Go(memberKey{gid, uid}, func() {
		defer recoverLog("on_member_update")
		if tr.ensureMemberExists(guildID, after) {
			return // just added with the current state
		}
		if !tr.db.MemberExists(gid, uid) {
			return // untracked, or the guild is not registered
		}
		if !ptrStrEq(beforeDisplay, afterDisplay) {
			tr.db.UpdateNicknameHistory(gid, uid, beforeDisplay, afterDisplay)
			tr.db.UpdateMemberNickname(gid, uid, afterDisplay)
		}
		if usernameChanged {
			tr.db.UpdateMemberUsername(gid, uid, newUsername)
		}
		if rolesChanged {
			tr.db.UpdateMemberRoles(gid, uid, roleNames)
			for _, r := range added {
				tr.db.RecordRoleChange(gid, uid, r, "added")
			}
			for _, r := range removed {
				tr.db.RecordRoleChange(gid, uid, r, "removed")
			}
		}
	})
}

func ptrStrEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// ==================== Messages ====================

func (tr *Tracking) onMessage(guildID snowflake.ID, msg discord.Message) {
	author := msg.Author
	if author.Bot || msg.WebhookID != nil {
		return
	}
	// Provisional accounts (game players relayed into a channel) never join.
	if msg.Member == nil && int(author.PublicFlags)&provisionalAccountFlag != 0 {
		return
	}
	if tr.isOptedOut(author.ID) {
		return
	}
	gid, uid := i64(guildID), i64(author.ID)
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()
	hourStart := time.Date(now.Year(), now.Month(), now.Day(), now.Hour(), 0, 0, 0, time.UTC).Unix()

	tr.bufMu.Lock()
	tr.daily[dailyKey{gid, uid, today}]++
	tr.hourly[hourlyKey{gid, uid, hourStart, now.Hour()}]++
	size := len(tr.daily) + len(tr.hourly)
	tr.bufMu.Unlock()

	// A flush already running swapped the buffers out; what arrives
	// meanwhile goes in the next one.
	if size >= maxActivityBufferEntries && tr.flushMu.TryLock() {
		tlog.Warn("Buffer size limit reached (%d entries), forcing flush", size)
		goSafe("forced flush", func() {
			defer tr.flushMu.Unlock()
			tr.flushOnce()
		})
	}
	guildName := tr.b.guildName(guildID)
	tlog.Debug("Buffered message from %s in %s", userTag(author), guildName)

	if msg.Member == nil {
		appID := "None"
		if msg.ApplicationID != nil {
			appID = msg.ApplicationID.String()
		}
		chType := "unknown"
		if ch, ok := tr.b.Client.Caches.Channel(msg.ChannelID); ok {
			chType = fmt.Sprint(int(ch.Type()))
		}
		tlog.Info("Message from non-member %s (%d) in %s: type=%d, application_id=%s, public_flags=%d, channel_type=%s",
			userTag(author), uid, guildName, int(msg.Type), appID, int(author.PublicFlags), chType)
	}
}

// ==================== Presence ====================

func (tr *Tracking) stripe(guildID snowflake.ID) *sync.RWMutex {
	return &tr.stripes[uint64(guildID)%presenceStripes]
}

// onPresenceUpdate runs before disgo stores the new status, so the cached
// status is the previous one. It stores the new status itself, under the
// guild's stripe lock, so a concurrent snapshot sees each event either
// already queued and applied to the cache, or neither.
func (tr *Tracking) onPresenceUpdate(e *events.PresenceUpdate) {
	guildID, userID := e.GuildID, e.PresenceUser.ID
	m, ok := tr.b.Client.Caches.Member(guildID, userID)
	if !ok || m.User.Bot {
		return // presences of members that aren't cached are ignored
	}
	st := tr.stripe(guildID)
	st.RLock()
	defer st.RUnlock()
	// Only real status changes (online/idle/dnd/offline) are written, not
	// activity updates.
	before := tr.b.statuses.Status(guildID, userID)
	if statusName(before) == statusName(e.Status) {
		return
	}
	tr.b.statuses.Put(guildID, userID, e.Presence)
	if tr.isOptedOut(userID) {
		return
	}
	item := presenceItem{guildID: guildID, userID: userID, wentOffline: isOffline(e.Status), ts: nowUnix()}
	key := memberKey{i64(guildID), i64(userID)}
	if tr.order.Pending(key) {
		// A join/leave/update of this member is still being written: queue
		// after it, so the member is found stored.
		tr.order.Go(key, func() { tr.enqueuePresence(item) })
	} else {
		tr.enqueuePresence(item)
	}
	tlog.Debug("User %s is now %s in %s", userTag(m.User), statusName(e.Status), tr.b.guildName(guildID))
}

func statusName(s discord.OnlineStatus) string {
	if isOffline(s) {
		return "offline"
	}
	return string(s)
}

func (tr *Tracking) enqueuePresence(item presenceItem) {
	select {
	case tr.queue <- queueItem{presence: &item}:
	default:
		if n := tr.dropped.Add(1); n%1000 == 1 {
			tlog.Warn("Presence queue full; dropped %d presence update(s) so far", n)
		}
	}
}

// ==================== Reconciliation ====================

func (tr *Tracking) onShardDisconnect(shardID int) {
	now := nowUnix()
	tr.stateMu.Lock()
	defer tr.stateMu.Unlock()
	tr.downShards[shardID] = true
	for _, id := range tr.b.guildIDs() {
		if tr.b.shardOf(id) == shardID {
			if _, ok := tr.staleSince[i64(id)]; !ok {
				tr.staleSince[i64(id)] = now
			}
		}
	}
}

func (tr *Tracking) onShardResumed(shardID int) {
	tr.stateMu.Lock()
	defer tr.stateMu.Unlock()
	delete(tr.downShards, shardID)
	for _, id := range tr.b.guildIDs() {
		if tr.b.shardOf(id) == shardID && !tr.b.Client.Caches.IsGuildUnavailable(id) {
			delete(tr.staleSince, i64(id))
		}
	}
}

func (tr *Tracking) onShardReady(shardID int) {
	tr.stateMu.Lock()
	delete(tr.downShards, shardID)
	tr.stateMu.Unlock()
}

func (tr *Tracking) onGuildUnavailable(guildID snowflake.ID) {
	tr.stateMu.Lock()
	if _, ok := tr.staleSince[i64(guildID)]; !ok {
		tr.staleSince[i64(guildID)] = nowUnix()
	}
	tr.stateMu.Unlock()
}

// onGuildChunked queues a reconcile of the guild's stored members against
// its now complete member cache.
func (tr *Tracking) onGuildChunked(guildID snowflake.ID) {
	b := tr.b
	gid := i64(guildID)
	tr.stateMu.Lock()
	queued := tr.reconcileQueued[gid]
	tr.stateMu.Unlock()
	if queued || !b.isChunked(guildID) {
		return
	}

	st := tr.stripe(guildID)
	st.Lock()
	online := map[int64]bool{}
	members := map[int64]discord.Member{}
	for m := range b.Client.Caches.Members(guildID) {
		if !m.User.Bot && !tr.isOptedOut(m.User.ID) {
			online[i64(m.User.ID)] = b.isOnline(guildID, m.User.ID)
			members[i64(m.User.ID)] = m
		}
	}

	tr.stateMu.Lock()
	var since int64
	consider := func(v int64) {
		if v != 0 && (since == 0 || v < since) {
			since = v
		}
	}
	if v, ok := tr.staleSince[gid]; ok {
		consider(v)
		delete(tr.staleSince, gid)
	}
	if !tr.reconciled[gid] && tr.startupStaleSince != nil {
		consider(*tr.startupStaleSince)
	}
	if since == 0 {
		since = nowUnix()
	}
	tr.reconciled[gid] = true
	tr.reconcileQueued[gid] = true
	tr.stateMu.Unlock()

	item := &guildReconcile{guildID: guildID, guildName: b.guildName(guildID), online: online, members: members, since: since}
	// Never dropped: blocks until the consumer makes room.
	select {
	case tr.queue <- queueItem{reconcile: item}:
	case <-tr.ctx.Done():
	}
	st.Unlock()
}

func (tr *Tracking) memberRow(guildID snowflake.ID, m discord.Member, online bool) db.MemberSnapshot {
	return db.MemberSnapshot{
		UserID:   i64(m.User.ID),
		Username: userTag(m.User),
		Nickname: storedNickname(m),
		JoinDate: joinTimestamp(m, nowUnix()),
		Roles:    tr.b.memberRoleNames(guildID, m.RoleIDs),
		Online:   online,
	}
}

func (tr *Tracking) reconcileGuild(item *guildReconcile) {
	gid := i64(item.guildID)
	if tr.db.GetGuildConfig(gid) == nil {
		return // still being set up by on_ready / on_guild_join
	}
	online := make(map[int64]bool, len(item.online))
	for uid, s := range item.online {
		if !tr.b.OptedOut.Has(uid) { // /forgetme may have run since the snapshot
			online[uid] = s
		}
	}
	rows, err := tr.db.GetMemberPresenceRows(gid)
	if err != nil {
		panic(err)
	}
	diff := diffMemberState(online, rows)
	departed := diff.Departed
	active := 0
	for _, r := range rows {
		if r.IsActive {
			active++
		}
	}
	if len(departed) > massDepartureMin && float64(len(departed)) > float64(active)*massDepartureRatio {
		tlog.Warn("Reconcile %s: %d of %d stored members look departed; not trusting the cache, departures skipped",
			item.guildName, len(departed), active)
		departed = nil
	}
	var newRows, rejoinedRows []db.MemberSnapshot
	for _, uid := range diff.New {
		newRows = append(newRows, tr.memberRow(item.guildID, item.members[uid], online[uid]))
	}
	for _, uid := range diff.Rejoined {
		rejoinedRows = append(rejoinedRows, tr.memberRow(item.guildID, item.members[uid], online[uid]))
	}
	if len(diff.Online) == 0 && len(diff.Offline) == 0 && len(departed) == 0 && len(newRows) == 0 && len(rejoinedRows) == 0 {
		return
	}
	if tr.db.ReconcileMembers(gid, diff.Online, diff.Offline, departed, rejoinedRows, newRows, item.since) {
		tlog.Info("Reconciled %s: %d online, %d offline, %d new, %d rejoined, %d departed",
			item.guildName, len(diff.Online), len(diff.Offline), len(newRows), len(rejoinedRows), len(departed))
	}
}

// applyBatch applies queued items in FIFO order; reconcile items in their
// queue position, the presence events between them as one transaction.
func (tr *Tracking) applyBatch(batch []queueItem) []onlineResult {
	var results []onlineResult
	var run []presenceItem
	for _, it := range batch {
		if it.reconcile != nil {
			results = append(results, tr.applyPresenceRun(run)...)
			run = nil
			func() {
				defer func() {
					if r := recover(); r != nil {
						tlog.Error("Failed to reconcile guild %s: %v", it.reconcile.guildID, r)
					}
				}()
				tr.reconcileGuild(it.reconcile)
			}()
			continue
		}
		if !tr.isOptedOut(it.presence.userID) {
			run = append(run, *it.presence)
		}
	}
	return append(results, tr.applyPresenceRun(run)...)
}

func (tr *Tracking) applyPresenceRun(run []presenceItem) []onlineResult {
	if len(run) == 0 {
		return nil
	}
	apply := func(items []presenceItem) []db.PresenceResult {
		evs := make([]db.PresenceEvent, len(items))
		for i, it := range items {
			evs[i] = db.PresenceEvent{GuildID: i64(it.guildID), UserID: i64(it.userID), WentOffline: it.wentOffline, TS: it.ts}
		}
		res, err := tr.db.ApplyPresenceEvents(evs)
		if err != nil {
			tlog.Error("Failed to apply %d presence update(s): %v", len(items), err)
			return nil
		}
		return res
	}
	results := apply(run)
	if results == nil {
		return nil // rolled back as a whole
	}
	var online []onlineResult
	var missing []presenceItem
	for i, it := range run {
		switch {
		case !results[i].Found:
			missing = append(missing, it)
		case !it.wentOffline:
			online = append(online, onlineResult{it.guildID, it.userID, results[i].PrevLastSeen, it.ts})
		}
	}
	if len(missing) > 0 {
		created := map[memberKey]bool{}
		for _, it := range missing {
			k := memberKey{i64(it.guildID), i64(it.userID)}
			if created[k] {
				continue
			}
			created[k] = true
			if m, ok := tr.b.Client.Caches.Member(it.guildID, it.userID); ok {
				func() {
					defer func() {
						if r := recover(); r != nil {
							tlog.Error("Failed to add member %s in guild %s: %v", it.userID, it.guildID, r)
						}
					}()
					tr.ensureMemberExists(it.guildID, m)
				}()
			}
		}
		// Still-missing members are untracked: dropped.
		res := apply(missing)
		for i, r := range res {
			if r.Found && !missing[i].wentOffline {
				it := missing[i]
				online = append(online, onlineResult{it.guildID, it.userID, r.PrevLastSeen, it.ts})
			}
		}
	}
	return online
}

// drainPresenceQueue is the single consumer of the presence queue: FIFO
// order guarantees an offline write never lands after the online write
// that followed it.
func (tr *Tracking) drainPresenceQueue() {
	if !tr.waitReady() {
		return
	}
	for {
		var batch []queueItem
		select {
		case it := <-tr.queue:
			batch = append(batch, it)
		case <-tr.ctx.Done():
			return
		}
	drain:
		for len(batch) < presenceBatchMax {
			select {
			case it := <-tr.queue:
				batch = append(batch, it)
			default:
				break drain
			}
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					tlog.Error("Error applying presence update batch: %v", r)
				}
				tr.stateMu.Lock()
				for _, it := range batch {
					if it.reconcile != nil {
						delete(tr.reconcileQueued, i64(it.reconcile.guildID))
					}
				}
				tr.stateMu.Unlock()
			}()
			for _, r := range tr.applyBatch(batch) {
				if r.prev != nil && *r.prev > 0 {
					if away := r.ts - *r.prev; away >= returnThresholdSeconds {
						tr.bufMu.Lock()
						tr.returns = append(tr.returns, returnRec{i64(r.guildID), i64(r.userID), away, r.ts})
						tr.bufMu.Unlock()
					}
				}
				if tr.b.WatchGuilds.Has(i64(r.guildID)) && tr.b.watch != nil {
					tr.b.watch.onMemberOnline(r.guildID, r.userID, r.prev)
				}
			}
		}()
	}
}

// ==================== Flush ====================

func (tr *Tracking) describeMembers(keys [][2]int64) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		name := fmt.Sprint(k[0])
		if g, ok := tr.b.Client.Caches.Guild(sf(k[0])); ok {
			name = g.Name
		}
		parts[i] = fmt.Sprintf("%s (%d)", name, k[1])
	}
	return strings.Join(parts, ", ")
}

// flush is one flush pass under the flush lock.
func (tr *Tracking) flush() {
	tr.flushMu.Lock()
	defer tr.flushMu.Unlock()
	tr.flushOnce()
}

func (tr *Tracking) flushOnce() {
	defer recoverLog("flush activity buffers")
	// Heartbeat: the last moment every shard was connected.
	tr.stateMu.Lock()
	allUp := len(tr.downShards) == 0
	tr.stateMu.Unlock()
	if allUp {
		tr.db.SetBotState(heartbeatKey, nowUnix())
	}

	tr.bufMu.Lock()
	if len(tr.failedDaily) > 0 {
		for k, n := range tr.failedDaily {
			tr.daily[k] += n
		}
		tlog.Info("Re-added %d failed daily entries for retry", len(tr.failedDaily))
		tr.failedDaily = map[dailyKey]int64{}
	}
	if len(tr.failedHourly) > 0 {
		for k, n := range tr.failedHourly {
			tr.hourly[k] += n
		}
		tlog.Info("Re-added %d failed hourly entries for retry", len(tr.failedHourly))
		tr.failedHourly = map[hourlyKey]int64{}
	}
	dailyCount, hourlyCount := len(tr.daily), len(tr.hourly)
	if dailyCount == 0 && hourlyCount == 0 && len(tr.returns) == 0 {
		tr.bufMu.Unlock()
		return
	}
	tlog.Debug("Flushing activity buffers: %d daily entries, %d hourly entries", dailyCount, hourlyCount)
	daily, hourly, returns := tr.daily, tr.hourly, tr.returns
	tr.daily, tr.hourly, tr.returns = map[dailyKey]int64{}, map[hourlyKey]int64{}, nil
	tr.bufMu.Unlock()

	// Daily
	failedD := map[dailyKey]int64{}
	var droppedD [][2]int64
	okD := 0
	for k, n := range daily {
		if ok, _ := tr.db.IncrementMessageActivity(k.guild, k.user, k.date, n); ok {
			okD++
		} else if tr.db.MemberIsMissing(k.guild, k.user) {
			droppedD = append(droppedD, [2]int64{k.guild, k.user})
		} else {
			failedD[k] = n
		}
	}
	tr.bufMu.Lock()
	for k, n := range failedD {
		tr.failedDaily[k] += n
	}
	tr.bufMu.Unlock()
	if len(droppedD) > 0 {
		tlog.Info("Dropped %d daily activity entries for members no longer in the database: %s", len(droppedD), tr.describeMembers(droppedD))
	}
	if len(failedD) > 0 {
		tlog.Warn("Failed to flush %d/%d daily entries, will retry", len(failedD), dailyCount)
	}
	tlog.Debug("Flushed %d/%d daily activity entries", okD, dailyCount)

	// Hourly
	failedH := map[hourlyKey]int64{}
	var droppedH [][2]int64
	okH := 0
	for k, n := range hourly {
		if ok, _ := tr.db.IncrementMessageActivityHourly(k.guild, k.user, k.ts, k.hour, n); ok {
			okH++
		} else if tr.db.MemberIsMissing(k.guild, k.user) {
			droppedH = append(droppedH, [2]int64{k.guild, k.user})
		} else {
			failedH[k] = n
		}
	}
	tr.bufMu.Lock()
	for k, n := range failedH {
		tr.failedHourly[k] += n
	}
	tr.bufMu.Unlock()
	if len(droppedH) > 0 {
		tlog.Info("Dropped %d hourly activity entries for members no longer in the database: %s", len(droppedH), tr.describeMembers(droppedH))
	}
	if len(failedH) > 0 {
		tlog.Warn("Failed to flush %d/%d hourly entries, will retry", len(failedH), hourlyCount)
	}
	tlog.Debug("Flushed %d/%d hourly activity entries", okH, hourlyCount)

	// Returning-member captures: rare and non-critical, never retried.
	if len(returns) > 0 {
		ok := 0
		for _, r := range returns {
			if tr.db.AddMemberReturn(r.guild, r.user, r.away, r.ts) {
				ok++
			}
		}
		tlog.Debug("Flushed %d/%d returning-member records", ok, len(returns))
	}
}

// ==================== Maintenance loops ====================

func (tr *Tracking) cleanupOldData() {
	tlog.Info("Starting daily data retention cleanup...")
	daily, hourly, guilds := tr.db.CleanupAllGuildsMessageActivity()
	if daily > 0 || hourly > 0 {
		tlog.Info("Data retention cleanup completed: %d daily records, %d hourly records deleted across %d guilds", daily, hourly, guilds)
	} else {
		tlog.Debug("Data retention cleanup: no old records to delete (%d guilds checked)", guilds)
	}
	if n := tr.db.PruneOldHealthSnapshots(db.HealthSnapshotRetentionDays); n > 0 {
		tlog.Info("Pruned %d old health snapshot(s)", n)
	}
}

func (tr *Tracking) recordHealthSnapshots() {
	n := tr.db.RecordHealthSnapshots()
	tlog.Info("Recorded health snapshots for %d guilds", n)
}

func (tr *Tracking) checkpointWAL(iteration int) {
	mode := "PASSIVE"
	if iteration%walTruncateEveryN == walTruncateEveryN-1 {
		mode = "TRUNCATE"
	}
	tr.db.CheckpointWAL(mode)
}

func (tr *Tracking) rotateStatus() {
	msg := statusMessages[tr.statusIdx%len(statusMessages)]
	tr.statusIdx++
	name := msg[1]
	if msg[0] != "" {
		name = msg[0] + " " + msg[1]
	}
	ctx, cancel := context.WithTimeout(tr.ctx, 10*time.Second)
	defer cancel()
	var errs []error
	for _, s := range tr.b.shards() { // a snapshot: SetPresenceForShard takes the shard manager's lock
		if err := tr.b.Client.SetPresenceForShard(ctx, s.ShardID(), gateway.WithCustomActivity(name)); err != nil {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		tlog.Error("Error rotating bot status: %v", err)
	}
}

func (tr *Tracking) backupDatabase() {
	cfg := tr.b.Cfg
	interval := float64(cfg.BackupIntervalHours)
	var lastUnix float64
	if last, ok := db.LatestBackupTime(cfg.BackupFolder); ok {
		lastUnix = float64(last.UnixNano()) / 1e9
	}
	sinceHours := (float64(time.Now().UnixNano())/1e9 - lastUnix) / 3600
	if sinceHours < interval {
		return
	}
	tlog.Info("Starting database backup (interval: %dh)...", cfg.BackupIntervalHours)
	file := tr.db.CreateBackup(cfg.BackupFolder)
	if file == "" {
		tlog.Error("Database backup failed")
		return
	}
	tlog.Info("Database backup completed: %s", file)
	if n := db.CleanupOldBackups(cfg.BackupFolder, cfg.BackupRetentionCount); n > 0 {
		tlog.Info("Deleted %d old backup(s), keeping %d most recent", n, cfg.BackupRetentionCount)
	}
}

// alignReports waits for the next whole minute so per-guild send-minute
// buckets land on minute boundaries.
func (tr *Tracking) alignReports() bool {
	interval := int(reportCheckInterval / time.Minute)
	now := time.Now().UTC()
	into := (now.Minute()%interval)*60 + now.Second()
	wait := interval*60 - into
	if wait > 0 && wait < interval*60 {
		tlog.Info("Syncing scheduled reports to %d-minute mark. Waiting %ds...", interval, wait)
		return tr.sleep(time.Duration(wait) * time.Second)
	}
	return true
}

func (tr *Tracking) checkScheduledReports() {
	guilds := tr.db.GetGuildsWithReportsEnabled()
	if len(guilds) == 0 {
		return
	}
	nowUTC := time.Now().UTC()
	for _, rg := range guilds {
		func() {
			defer func() {
				if r := recover(); r != nil {
					tlog.Error("Error processing scheduled report for guild: %v", r)
				}
			}()
			tr.checkGuildReport(rg, nowUTC)
		}()
	}
}

func intOr(p *int64, def int) int {
	if p == nil {
		return def
	}
	return int(*p)
}

func (tr *Tracking) checkGuildReport(rg db.ReportGuild, nowUTC time.Time) {
	guildID := sf(rg.GuildID)
	g, ok := tr.b.Client.Caches.Guild(guildID)
	if !ok {
		tlog.Warn("Guild %d not found, skipping report", rg.GuildID)
		return
	}
	hour := intOr(rg.Hour, 9)
	tzName := "UTC"
	if rg.Timezone != nil {
		tzName = *rg.Timezone
	}
	if !tzdb.Valid(tzName) {
		tlog.Warn("Invalid timezone '%s' for guild %d, using UTC", tzName, rg.GuildID)
		tzName = "UTC"
	}
	loc := tzdb.Load(tzName)
	local := nowUTC.In(loc)
	if local.Hour() < hour {
		return
	}
	// Per-guild minute offset past the configured hour spreads the sends.
	if sendMinute := int(rg.GuildID % reportJitterMinutes); local.Hour() == hour && local.Minute() < sendMinute {
		return
	}
	var types []string
	if rg.Types != nil && *rg.Types != "" {
		var ok bool
		if types, ok = db.LoadStrings(*rg.Types); !ok {
			tlog.Error("Failed to parse report_types JSON for guild %d", rg.GuildID)
			types = nil
		}
	}
	if len(types) == 0 {
		return
	}
	sameLocalDay := func(ts int64) bool {
		a := time.Unix(ts, 0).In(loc)
		return a.Year() == local.Year() && a.YearDay() == local.YearDay()
	}
	due := func(today bool, last *int64) bool {
		if !today {
			return false
		}
		if last == nil || *last == 0 {
			return true
		}
		return !sameLocalDay(*last)
	}
	freq := rg.Frequency
	if (freq == "weekly" || freq == "both") && due(weekdayFromMonday(local) == intOr(rg.DayWeekly, 0), rg.LastWeekly) {
		tlog.Info("Sending weekly report for guild %s at %02d:00 %s", g.Name, local.Hour(), tzName)
		sent := tr.b.sendScheduledReport(guildID, rg.ChannelID, types, 7)
		// Marked even on failure so a persistent error does not retry hourly.
		tr.db.UpdateLastReportTime(rg.GuildID, "weekly")
		if !sent {
			tlog.Warn("Weekly report for guild %s failed to send; will retry at next scheduled occurrence", g.Name)
		}
	}
	if (freq == "monthly" || freq == "both") && due(local.Day() == intOr(rg.DayMonthly, 1), rg.LastMonthly) {
		tlog.Info("Sending monthly report for guild %s at %02d:00 %s", g.Name, local.Hour(), tzName)
		sent := tr.b.sendScheduledReport(guildID, rg.ChannelID, types, 30)
		tr.db.UpdateLastReportTime(rg.GuildID, "monthly")
		if !sent {
			tlog.Warn("Monthly report for guild %s failed to send; will retry at next scheduled occurrence", g.Name)
		}
	}
}

// weekdayFromMonday is the day of the week counted from Monday (0) to
// Sunday (6).
func weekdayFromMonday(t time.Time) int { return (int(t.Weekday()) + 6) % 7 }

// unload stops the loops and writes everything still buffered (shutdown).
func (tr *Tracking) unload() {
	tlog.Info("Shutting down, flushing remaining buffers...")
	tr.cancel()
	done := make(chan struct{})
	go func() {
		tr.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		tlog.Warn("Background tasks did not stop within 15s; flushing anyway")
	}
	if pending := len(tr.queue); pending > 0 {
		tlog.Info("Discarding %d queued presence update(s) on unload", pending)
	}
	tr.flush()
	tr.bufMu.Lock()
	left := len(tr.failedDaily) + len(tr.failedHourly)
	tr.bufMu.Unlock()
	if left > 0 {
		tlog.Warn("%d activity entr(ies) could not be written on unload and are lost", left)
	} else {
		tlog.Info("Flushed all buffers on unload")
	}
}
