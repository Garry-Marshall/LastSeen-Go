package db

import (
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

// Num is an int64 or a float64. Some averages are a float normally but the
// int 0 when there is no data; the type decides the rendered text ("0" or
// "0.0"), which stays as users know it.
type Num = any

// ---- Role changes ----

// RecordRoleChange stores a role added/removed event (last 20 kept per member).
func (d *DB) RecordRoleChange(guildID, userID int64, roleName, action string) bool {
	if strings.TrimSpace(roleName) == "" {
		log.Error("Invalid role_name: %s", roleName)
		return false
	}
	if action != "added" && action != "removed" {
		log.Error("Invalid action '%s', must be 'added' or 'removed'", action)
		return false
	}
	var b strings.Builder
	for _, r := range strings.TrimSpace(roleName) {
		if r >= 32 || r == '\n' {
			b.WriteRune(r)
		}
	}
	sanitized := b.String()
	if utf8.RuneCountInString(sanitized) > 100 {
		sanitized = string([]rune(sanitized)[:100])
		log.Warn("Role name truncated to 100 characters: %s", sanitized)
	}
	err := d.tx(func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx(), `INSERT INTO role_changes (guild_id, user_id, role_name, action, timestamp)
			VALUES (?, ?, ?, ?, ?)`, guildID, userID, sanitized, action, nowUTC()); err != nil {
			return err
		}
		cleanupRoleChanges(tx, guildID, userID, 20)
		return nil
	})
	if err != nil {
		log.Error("Failed to record role change for user %d in guild %d: %v", userID, guildID, err)
		return false
	}
	return true
}

func cleanupRoleChanges(tx *sql.Tx, guildID, userID int64, keep int) {
	var id int64
	err := tx.QueryRowContext(ctx(), `SELECT id FROM role_changes WHERE guild_id = ? AND user_id = ?
		ORDER BY timestamp DESC LIMIT 1 OFFSET ?`, guildID, userID, keep-1).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return
	}
	if err != nil {
		log.Error("Failed to cleanup role changes for user %d: %v", userID, err)
		return
	}
	res, err := tx.ExecContext(ctx(), "DELETE FROM role_changes WHERE guild_id = ? AND user_id = ? AND id < ?", guildID, userID, id)
	if err != nil {
		log.Error("Failed to cleanup role changes for user %d: %v", userID, err)
		return
	}
	if n := affected(res); n > 0 {
		log.Debug("Cleaned up %d old role changes for user %d", n, userID)
	}
}

// RoleChange is one role_changes row.
type RoleChange struct {
	RoleName  string
	Action    string
	Timestamp int64
}

// GetRoleHistory lists a member's role changes, newest first.
func (d *DB) GetRoleHistory(guildID, userID int64, limit int) []RoleChange {
	rows, err := d.sql.QueryContext(ctx(), `SELECT role_name, action, timestamp FROM role_changes
		WHERE guild_id = ? AND user_id = ? ORDER BY timestamp DESC LIMIT ?`, guildID, userID, limit)
	if err != nil {
		log.Error("Failed to get role history for user %d in guild %d: %v", userID, guildID, err)
		return nil
	}
	defer rows.Close()
	var out []RoleChange
	for rows.Next() {
		var c RoleChange
		if err := rows.Scan(&c.RoleName, &c.Action, &c.Timestamp); err != nil {
			log.Error("Failed to get role history for user %d in guild %d: %v", userID, guildID, err)
			return nil
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		log.Error("Failed to get role history for user %d in guild %d: %v", userID, guildID, err)
		return nil
	}
	return out
}

// ---- Message activity ----

// IncrementMessageActivity adds count messages to a member's day row and
// folds the write into member_activity_summary in the same transaction.
// Returns false when the member has no row (nothing written).
func (d *DB) IncrementMessageActivity(guildID, userID, date int64, count int64) (bool, error) {
	if count <= 0 {
		return false, nil
	}
	written := false
	err := d.tx(func(tx *sql.Tx) error {
		var present, hasToday bool
		if err := tx.QueryRowContext(ctx(), `SELECT
				EXISTS(SELECT 1 FROM members WHERE guild_id = ? AND user_id = ?),
				EXISTS(SELECT 1 FROM message_activity WHERE guild_id = ? AND user_id = ? AND date = ?)`,
			guildID, userID, guildID, userID, date).Scan(&present, &hasToday); err != nil {
			return err
		}
		if !present {
			return nil
		}
		if _, err := tx.ExecContext(ctx(), `INSERT INTO message_activity (guild_id, user_id, date, message_count)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(guild_id, user_id, date) DO UPDATE SET message_count = message_count + ?`,
			guildID, userID, date, count, count); err != nil {
			return err
		}
		if err := applyActivitySummary(tx, guildID, userID, date, count, !hasToday); err != nil {
			return err
		}
		written = true
		return nil
	})
	if err != nil {
		log.Error("Failed to increment message activity for user %d: %v", userID, err)
		return false, err
	}
	return written, nil
}

func applyActivitySummary(tx *sql.Tx, guildID, userID, date, count int64, isNewDay bool) error {
	var first, last sql.NullInt64
	var total, days, cur, best, gap int64
	err := tx.QueryRowContext(ctx(), `SELECT first_active, last_active, total_messages, active_days,
			current_streak, longest_streak, longest_gap
		FROM member_activity_summary WHERE guild_id = ? AND user_id = ?`, guildID, userID).
		Scan(&first, &last, &total, &days, &cur, &best, &gap)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx(), `INSERT INTO member_activity_summary
			(guild_id, user_id, first_active, last_active, total_messages,
			 active_days, current_streak, longest_streak, longest_gap)
			VALUES (?, ?, ?, ?, ?, 1, 1, 1, 0)`, guildID, userID, date, date, count)
		return err
	}
	if err != nil {
		return err
	}
	total += count
	if isNewDay {
		if !last.Valid || date > last.Int64 {
			var closed int64
			if last.Valid && last.Int64 != 0 {
				closed = floorDiv(date-last.Int64, SecondsPerDay) - 1
			}
			if closed <= 0 {
				cur++
			} else {
				if closed > gap {
					gap = closed
				}
				cur = 1
			}
			if cur > best {
				best = cur
			}
			days++
			last = sql.NullInt64{Int64: date, Valid: true}
			if !first.Valid {
				first = sql.NullInt64{Int64: date, Valid: true}
			}
		} else {
			days++
			if !first.Valid || date < first.Int64 {
				first = sql.NullInt64{Int64: date, Valid: true}
			}
		}
	}
	_, err = tx.ExecContext(ctx(), `UPDATE member_activity_summary
		SET first_active = ?, last_active = ?, total_messages = ?, active_days = ?,
			current_streak = ?, longest_streak = ?, longest_gap = ?
		WHERE guild_id = ? AND user_id = ?`,
		nullInt(ptrInt(first)), nullInt(ptrInt(last)), total, days, cur, best, gap, guildID, userID)
	return err
}

// MemberJourney is the Member Journey view data.
type MemberJourney struct {
	JoinDate        *int64
	LastSeen        *int64
	FirstActive     *int64
	LastActive      *int64
	TotalMessages   int64
	ActiveDays      int64
	LongestStreak   int64
	LongestGap      int64
	ReturnCount     int64
	LongestAwayDays *int64
}

// GetMemberJourney assembles the journey for one member (nil if not stored).
func (d *DB) GetMemberJourney(guildID, userID int64) *MemberJourney {
	var j MemberJourney
	var join, last sql.NullInt64
	err := d.sql.QueryRowContext(ctx(), "SELECT join_date, last_seen FROM members WHERE guild_id = ? AND user_id = ?", guildID, userID).Scan(&join, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		log.Error("Failed to get member journey for user %d: %v", userID, err)
		return nil
	}
	j.JoinDate, j.LastSeen = ptrInt(join), ptrInt(last)
	var fa, la sql.NullInt64
	err = d.sql.QueryRowContext(ctx(), `SELECT first_active, last_active, total_messages, active_days,
			longest_streak, longest_gap
		FROM member_activity_summary WHERE guild_id = ? AND user_id = ?`, guildID, userID).
		Scan(&fa, &la, &j.TotalMessages, &j.ActiveDays, &j.LongestStreak, &j.LongestGap)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Error("Failed to get member journey for user %d: %v", userID, err)
		return nil
	}
	j.FirstActive, j.LastActive = ptrInt(fa), ptrInt(la)
	var maxAway sql.NullInt64
	if err := d.sql.QueryRowContext(ctx(), "SELECT COUNT(*), MAX(away_seconds) FROM member_returns WHERE guild_id = ? AND user_id = ?",
		guildID, userID).Scan(&j.ReturnCount, &maxAway); err != nil {
		log.Error("Failed to get member journey for user %d: %v", userID, err)
		return nil
	}
	if maxAway.Valid && maxAway.Int64 != 0 {
		v := floorDiv(maxAway.Int64, SecondsPerDay)
		j.LongestAwayDays = &v
	}
	return &j
}

// IncrementMessageActivityHourly adds count messages to a member's hour row.
// Returns false when the member has no row.
func (d *DB) IncrementMessageActivityHourly(guildID, userID, ts int64, hour int, count int64) (bool, error) {
	if count <= 0 {
		return false, nil
	}
	if !d.MemberExists(guildID, userID) {
		return false, nil
	}
	err := d.tx(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx(), `INSERT INTO message_activity_hourly (guild_id, user_id, timestamp, hour, message_count)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(guild_id, user_id, timestamp) DO UPDATE SET message_count = message_count + ?`,
			guildID, userID, ts, hour, count, count)
		return err
	})
	if err != nil {
		log.Error("Failed to increment hourly message activity for user %d: %v", userID, err)
		return false, err
	}
	return true, nil
}

// ActivityPeriod is a member's message counts.
type ActivityPeriod struct {
	Total, Today, ThisWeek, ThisMonth int64
	AvgPerDay                         Num
}

// GetMessageActivityPeriod returns a member's totals for the window, today,
// the last 7 days, and the 7-day per-day average.
func (d *DB) GetMessageActivityPeriod(guildID, userID int64, days int) ActivityPeriod {
	zero := ActivityPeriod{AvgPerDay: int64(0)}
	today := todayStart()
	cutoff := DayWindowStart(days)
	week := DayWindowStart(7)
	sum := func(q string, args ...any) (int64, error) { return scalarInt(d.sql, q, args...) }
	total, err := sum(`SELECT COALESCE(SUM(message_count), 0) FROM message_activity
		WHERE guild_id = ? AND user_id = ? AND date >= ?`, guildID, userID, cutoff)
	if err != nil {
		log.Error("Failed to get message activity for user %d: %v", userID, err)
		return zero
	}
	todayCount, err := sum(`SELECT COALESCE(SUM(message_count), 0) FROM message_activity
		WHERE guild_id = ? AND user_id = ? AND date = ?`, guildID, userID, today)
	if err != nil {
		log.Error("Failed to get message activity for user %d: %v", userID, err)
		return zero
	}
	var activeDays, weekTotal int64
	if err := d.sql.QueryRowContext(ctx(), `SELECT COUNT(DISTINCT date), COALESCE(SUM(message_count), 0)
		FROM message_activity WHERE guild_id = ? AND user_id = ? AND date >= ?`, guildID, userID, week).
		Scan(&activeDays, &weekTotal); err != nil {
		log.Error("Failed to get message activity for user %d: %v", userID, err)
		return zero
	}
	var avg Num = int64(0)
	if activeDays > 0 {
		dwd, err := d.daysWithData(guildID, 7, &userID)
		if err != nil {
			log.Error("Failed to get message activity for user %d: %v", userID, err)
			return zero
		}
		avg = roundTo(float64(weekTotal)/float64(dwd), 1)
	}
	return ActivityPeriod{Total: total, Today: todayCount, ThisWeek: weekTotal, ThisMonth: total, AvgPerDay: avg}
}

// daysWithData is how many of the last days days could hold message data
// (bounded by the bot's arrival, retention and, for a member, their join or
// first message in the window). Always at least 1.
func (d *DB) daysWithData(guildID int64, days int, userID *int64) (int64, error) {
	window := DayWindowStart(days)
	start := window
	var added, retention sql.NullInt64
	err := d.sql.QueryRowContext(ctx(), "SELECT added_at, message_retention_days FROM guilds WHERE guild_id = ?", guildID).Scan(&added, &retention)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if err == nil {
		start = max(start, added.Int64)
		if retention.Valid && retention.Int64 != 0 {
			cutoff := nowUTC() - retention.Int64*SecondsPerDay
			start = max(start, -floorDiv(-cutoff, SecondsPerDay)*SecondsPerDay)
		}
	}
	if userID != nil {
		var join, firstMsg sql.NullInt64
		err := d.sql.QueryRowContext(ctx(), `SELECT m.join_date,
				(SELECT MIN(date) FROM message_activity
				 WHERE guild_id = m.guild_id AND user_id = m.user_id AND date >= ?) AS first_msg
			FROM members m WHERE m.guild_id = ? AND m.user_id = ?`, window, guildID, *userID).Scan(&join, &firstMsg)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if err == nil && join.Valid && join.Int64 != 0 {
			ms := join.Int64
			if firstMsg.Valid {
				ms = min(join.Int64, firstMsg.Int64)
			}
			start = max(start, ms)
		}
	}
	start -= floorMod(start, SecondsPerDay)
	return max(1, floorDiv(DayWindowStart(1)-start, SecondsPerDay)+1), nil
}

func floorMod(a, b int64) int64 { return a - floorDiv(a, b)*b }

// GetMemberDaysWithData is daysWithData for one member (days on error).
func (d *DB) GetMemberDaysWithData(guildID, userID int64, days int) int64 {
	n, err := d.daysWithData(guildID, days, &userID)
	if err != nil {
		log.Error("Failed to get tracked days for user %d in guild %d: %v", userID, guildID, err)
		return int64(days)
	}
	return n
}

// ActivityTotals are a member's message totals.
type ActivityTotals struct{ Total, ThisWeek, Today int64 }

// GetGuildActivityTotals returns per-member totals for the whole guild.
func (d *DB) GetGuildActivityTotals(guildID int64, days int) map[int64]ActivityTotals {
	rows, err := d.sql.QueryContext(ctx(), `SELECT user_id,
			SUM(message_count),
			SUM(CASE WHEN date >= ? THEN message_count ELSE 0 END),
			SUM(CASE WHEN date = ? THEN message_count ELSE 0 END)
		FROM message_activity WHERE guild_id = ? AND date >= ? GROUP BY user_id`,
		DayWindowStart(7), todayStart(), guildID, DayWindowStart(days))
	if err != nil {
		log.Error("Failed to get activity totals for guild %d: %v", guildID, err)
		return map[int64]ActivityTotals{}
	}
	defer rows.Close()
	out := map[int64]ActivityTotals{}
	for rows.Next() {
		var uid int64
		var t ActivityTotals
		var total, week, today sql.NullInt64
		if err := rows.Scan(&uid, &total, &week, &today); err != nil {
			log.Error("Failed to get activity totals for guild %d: %v", guildID, err)
			return map[int64]ActivityTotals{}
		}
		t.Total, t.ThisWeek, t.Today = total.Int64, week.Int64, today.Int64
		out[uid] = t
	}
	if err := rows.Err(); err != nil {
		log.Error("Failed to get activity totals for guild %d: %v", guildID, err)
		return map[int64]ActivityTotals{}
	}
	return out
}

// GetGuildLastActive maps each member to their latest active day.
func (d *DB) GetGuildLastActive(guildID int64) map[int64]int64 {
	rows, err := d.sql.QueryContext(ctx(), `SELECT user_id, last_active FROM member_activity_summary
		WHERE guild_id = ? AND last_active IS NOT NULL`, guildID)
	if err != nil {
		log.Error("Failed to get last active days for guild %d: %v", guildID, err)
		return map[int64]int64{}
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var uid, la int64
		if err := rows.Scan(&uid, &la); err != nil {
			log.Error("Failed to get last active days for guild %d: %v", guildID, err)
			return map[int64]int64{}
		}
		out[uid] = la
	}
	if err := rows.Err(); err != nil {
		log.Error("Failed to get last active days for guild %d: %v", guildID, err)
		return map[int64]int64{}
	}
	return out
}

// Percentile ranks a member's activity against all current members.
type Percentile struct {
	Percentile  int64
	Rank        int64
	TotalRanked int64
	CallerTotal int64
}

// GetActivityPercentile returns nil when fewer than 5 members rank, or on error.
func (d *DB) GetActivityPercentile(guildID, userID int64, days int) *Percentile {
	const minRanked = 5
	cutoff := DayWindowStart(days)
	caller, err := scalarInt(d.sql, `SELECT COALESCE(SUM(message_count), 0) FROM message_activity
		WHERE guild_id = ? AND user_id = ? AND date >= ?`, guildID, userID, cutoff)
	if err != nil {
		log.Error("Failed to get activity percentile for user %d in guild %d: %v", userID, guildID, err)
		return nil
	}
	var above, below, totalRanked sql.NullInt64
	err = d.sql.QueryRowContext(ctx(), `SELECT
			SUM(CASE WHEN total > ? THEN 1 ELSE 0 END),
			SUM(CASE WHEN total < ? THEN 1 ELSE 0 END),
			COUNT(*)
		FROM (
			SELECT COALESCE(SUM(ma.message_count), 0) AS total
			FROM members m
			LEFT JOIN message_activity ma
				ON ma.guild_id = m.guild_id AND ma.user_id = m.user_id AND ma.date >= ?
			WHERE m.guild_id = ? AND m.is_active = 1
			GROUP BY m.user_id
		)`, caller, caller, cutoff, guildID).Scan(&above, &below, &totalRanked)
	if err != nil {
		log.Error("Failed to get activity percentile for user %d in guild %d: %v", userID, guildID, err)
		return nil
	}
	if totalRanked.Int64 < minRanked {
		return nil
	}
	var pct int64
	if totalRanked.Int64 > 1 {
		pct = roundHalfEven(float64(below.Int64) / float64(totalRanked.Int64-1) * 100)
	}
	return &Percentile{Percentile: pct, Rank: above.Int64 + 1, TotalRanked: totalRanked.Int64, CallerTotal: caller}
}

// DayCount is one day's message count.
type DayCount struct{ Date, Count int64 }

// GetMessageActivityTrend lists a member's daily counts, newest first.
func (d *DB) GetMessageActivityTrend(guildID, userID int64, days int) []DayCount {
	rows, err := d.sql.QueryContext(ctx(), `SELECT date, message_count FROM message_activity
		WHERE guild_id = ? AND user_id = ? AND date >= ? ORDER BY date DESC`, guildID, userID, DayWindowStart(days))
	if err != nil {
		log.Error("Failed to get message activity trend for user %d: %v", userID, err)
		return nil
	}
	defer rows.Close()
	var out []DayCount
	for rows.Next() {
		var c DayCount
		var n sql.NullInt64
		if err := rows.Scan(&c.Date, &n); err != nil {
			log.Error("Failed to get message activity trend for user %d: %v", userID, err)
			return nil
		}
		c.Count = n.Int64
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		log.Error("Failed to get message activity trend for user %d: %v", userID, err)
		return nil
	}
	return out
}

// ActivityProfile is a member's message-time profile in the guild's timezone.
type ActivityProfile struct {
	Total       int64
	ActiveDays  int
	WindowDays  int
	ByWeekday   [7]int64
	ByHour      [24]int64
	PeakWeekday *int
	PeakHours   *[2]int
	Trend       string // "increasing", "steady", "decreasing" or ""
}

// GetActivityProfile builds a member's activity profile from the hourly
// message buckets (never presence data).
func (d *DB) GetActivityProfile(guildID, userID int64, days int, tzName string) ActivityProfile {
	empty := ActivityProfile{WindowDays: days}
	loc := LoadTZ(tzName)
	periodStart := nowUTC() - int64(days)*SecondsPerDay
	rows, err := d.sql.QueryContext(ctx(), `SELECT timestamp, message_count FROM message_activity_hourly
		WHERE guild_id = ? AND user_id = ? AND timestamp >= ?`, guildID, userID, periodStart)
	if err != nil {
		log.Error("Failed to build activity profile for user %d in guild %d: %v", userID, guildID, err)
		return empty
	}
	p := ActivityProfile{WindowDays: days}
	type ymd struct{ y, m, d int }
	perDate := map[ymd]int64{}
	for rows.Next() {
		var ts int64
		var cnt sql.NullInt64
		if err := rows.Scan(&ts, &cnt); err != nil {
			rows.Close()
			log.Error("Failed to build activity profile for user %d in guild %d: %v", userID, guildID, err)
			return empty
		}
		local := time.Unix(ts, 0).In(loc)
		p.ByWeekday[weekdayFromMonday(local)] += cnt.Int64
		p.ByHour[local.Hour()] += cnt.Int64
		y, m, dd := local.Date()
		perDate[ymd{y, int(m), dd}] += cnt.Int64
		p.Total += cnt.Int64
	}
	if err := rows.Err(); err != nil {
		log.Error("Failed to build activity profile for user %d in guild %d: %v", userID, guildID, err)
		return empty
	}
	rows.Close()
	if p.Total == 0 {
		return empty
	}
	p.ActiveDays = len(perDate)
	pw := 0
	for i := 1; i < 7; i++ {
		if p.ByWeekday[i] > p.ByWeekday[pw] {
			pw = i
		}
	}
	p.PeakWeekday = &pw
	bestStart, bestSum := 0, int64(-1)
	for start := 0; start < 24; start++ {
		var s int64
		for k := 0; k < 4; k++ {
			s += p.ByHour[(start+k)%24]
		}
		if s > bestSum {
			bestSum, bestStart = s, start
		}
	}
	p.PeakHours = &[2]int{bestStart, (bestStart + 4) % 24}

	half := days / 2
	ny, nm, nd := time.Unix(nowUTC(), 0).In(loc).Date()
	today := time.Date(ny, nm, nd, 0, 0, 0, 0, time.UTC)
	var recent, prior int64
	for k, c := range perDate {
		age := int(today.Sub(time.Date(k.y, time.Month(k.m), k.d, 0, 0, 0, 0, time.UTC)).Hours() / 24)
		if age < half {
			recent += c
		} else if age < days {
			prior += c
		}
	}
	if recent+prior > 0 {
		switch {
		case prior == 0:
			p.Trend = "increasing"
		case float64(recent) >= float64(prior)*1.2:
			p.Trend = "increasing"
		case float64(recent) <= float64(prior)*0.8:
			p.Trend = "decreasing"
		default:
			p.Trend = "steady"
		}
	}
	return p
}

// WhoisPanel bundles the per-member activity reads /whois needs.
type WhoisPanel struct {
	Profile    ActivityProfile
	Trend      []DayCount
	Percentile *Percentile
}

// GetWhoisActivityPanel runs the profile, the daily trend and (only past the
// same thin-data gate the embed uses) the percentile.
func (d *DB) GetWhoisActivityPanel(guildID, userID int64, days int, tzName string) WhoisPanel {
	w := WhoisPanel{
		Profile: d.GetActivityProfile(guildID, userID, days, tzName),
		Trend:   d.GetMessageActivityTrend(guildID, userID, days),
	}
	if w.Profile.Total >= 15 && w.Profile.ActiveDays >= 5 {
		w.Percentile = d.GetActivityPercentile(guildID, userID, days)
	}
	return w
}

// GuildMessageStats are guild-wide message statistics.
type GuildMessageStats struct {
	Total365d, Total90d, Total30d, Total7d, Today int64
	AvgPerDay                                     Num
	BusiestDay, QuietestDay                       *DayCount
	ActiveMembers30d                              int64
	AvgPerMember                                  Num
}

// GetGuildMessageActivityStats returns guild-wide message statistics.
func (d *DB) GetGuildMessageActivityStats(guildID int64, days int, f *UserFilter) GuildMessageStats {
	zero := GuildMessageStats{AvgPerDay: int64(0), AvgPerMember: int64(0)}
	fc, fp := f.clause("user_id")
	sum := func(cond string, v int64) (int64, error) {
		return scalarInt(d.sql, "SELECT COALESCE(SUM(message_count), 0) FROM message_activity WHERE guild_id = ? AND "+cond+fc,
			append([]any{guildID, v}, fp...)...)
	}
	var s GuildMessageStats
	var err error
	fail := func() GuildMessageStats {
		log.Error("Failed to get guild message activity stats for guild %d: %v", guildID, err)
		return zero
	}
	cutoff := DayWindowStart(days)
	month := DayWindowStart(30)
	if s.Total365d, err = sum("date >= ?", cutoff); err != nil {
		return fail()
	}
	if s.Total90d, err = sum("date >= ?", DayWindowStart(90)); err != nil {
		return fail()
	}
	if s.Total30d, err = sum("date >= ?", month); err != nil {
		return fail()
	}
	if s.Total7d, err = sum("date >= ?", DayWindowStart(7)); err != nil {
		return fail()
	}
	if s.Today, err = sum("date = ?", todayStart()); err != nil {
		return fail()
	}
	extreme := func(order string) (*DayCount, error) {
		var c DayCount
		var total sql.NullInt64
		err := d.sql.QueryRowContext(ctx(), `SELECT date, SUM(message_count) as total FROM message_activity
			WHERE guild_id = ? AND date >= ?`+fc+` GROUP BY date ORDER BY total `+order+` LIMIT 1`,
			append([]any{guildID, cutoff}, fp...)...).Scan(&c.Date, &total)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		c.Count = total.Int64
		return &c, err
	}
	if s.BusiestDay, err = extreme("DESC"); err != nil {
		return fail()
	}
	if s.QuietestDay, err = extreme("ASC"); err != nil {
		return fail()
	}
	if s.ActiveMembers30d, err = scalarInt(d.sql, "SELECT COUNT(DISTINCT user_id) FROM message_activity WHERE guild_id = ? AND date >= ?"+fc,
		append([]any{guildID, month}, fp...)...); err != nil {
		return fail()
	}
	s.AvgPerDay = int64(0)
	if s.Total365d > 0 {
		dwd, e := d.daysWithData(guildID, days, nil)
		if e != nil {
			err = e
			return fail()
		}
		s.AvgPerDay = roundTo(float64(s.Total365d)/float64(dwd), 1)
	}
	s.AvgPerMember = int64(0)
	if s.ActiveMembers30d > 0 {
		s.AvgPerMember = roundTo(float64(s.Total30d)/float64(s.ActiveMembers30d), 1)
	}
	return s
}
