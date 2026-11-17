package db

import (
	"database/sql"
	"time"
)

// HealthSnapshot is one daily health_snapshots row.
type HealthSnapshot struct {
	Date, ActiveMembers, Posters7d, Messages7d, Joins7d, Leaves7d, Returns7d int64
}

// RecordHealthSnapshots stores today's 7-day vitals for every guild. All
// reads happen first so the write lock is held for one short batch. Returns
// the number of guilds snapshotted.
func (d *DB) RecordHealthSnapshots() int {
	fail := func(err error) int {
		log.Error("Failed to record health snapshots: %v", err)
		return 0
	}
	day := todayStart()
	start := day - 7*SecondsPerDay
	ids, err := queryInts(d.sql, "SELECT guild_id FROM guilds")
	if err != nil {
		return fail(err)
	}
	type row struct {
		guild int64
		s     HealthSnapshot
	}
	rows := make([]row, 0, len(ids))
	for _, g := range ids {
		r := row{guild: g, s: HealthSnapshot{Date: day}}
		if err := d.sql.QueryRowContext(ctx(), `SELECT COUNT(DISTINCT user_id), COALESCE(SUM(message_count), 0)
			FROM message_activity WHERE guild_id = ? AND date >= ? AND date < ?`, g, start, day).
			Scan(&r.s.Posters7d, &r.s.Messages7d); err != nil {
			return fail(err)
		}
		for _, q := range []struct {
			dst  *int64
			sql  string
			args []any
		}{
			{&r.s.ActiveMembers, "SELECT COUNT(*) FROM members WHERE guild_id = ? AND is_active = 1", []any{g}},
			{&r.s.Joins7d, "SELECT COUNT(*) FROM members WHERE guild_id = ? AND join_date >= ? AND join_date < ?", []any{g, start, day}},
			{&r.s.Leaves7d, "SELECT COUNT(*) FROM members WHERE guild_id = ? AND " + LeftTSSQL + " >= ? AND " + LeftTSSQL + " < ?", []any{g, start, day}},
			{&r.s.Returns7d, "SELECT COUNT(*) FROM member_returns WHERE guild_id = ? AND returned_at >= ? AND returned_at < ?", []any{g, start, day}},
		} {
			if *q.dst, err = scalarInt(d.sql, q.sql, q.args...); err != nil {
				return fail(err)
			}
		}
		rows = append(rows, r)
	}
	err = d.tx(func(tx *sql.Tx) error {
		st, err := tx.PrepareContext(ctx(), `INSERT OR REPLACE INTO health_snapshots
			(guild_id, date, active_members, posters_7d, messages_7d, joins_7d, leaves_7d, returns_7d)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer st.Close()
		for _, r := range rows {
			if _, err := st.ExecContext(ctx(), r.guild, r.s.Date, r.s.ActiveMembers, r.s.Posters7d,
				r.s.Messages7d, r.s.Joins7d, r.s.Leaves7d, r.s.Returns7d); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fail(err)
	}
	return len(ids)
}

// GetHealthHistory samples snapshots every stepDays days, walking back from
// the newest (skipping days the bot was offline). Up to points rows, oldest
// first.
func (d *DB) GetHealthHistory(guildID int64, points, stepDays int) []HealthSnapshot {
	rows, err := d.sql.QueryContext(ctx(), `SELECT date, active_members, posters_7d, messages_7d,
			joins_7d, leaves_7d, returns_7d
		FROM health_snapshots WHERE guild_id = ? ORDER BY date DESC LIMIT ?`, guildID, points*stepDays)
	if err != nil {
		log.Error("Failed to get health history for guild %d: %v", guildID, err)
		return nil
	}
	defer rows.Close()
	var all []HealthSnapshot
	for rows.Next() {
		var s HealthSnapshot
		if err := rows.Scan(&s.Date, &s.ActiveMembers, &s.Posters7d, &s.Messages7d, &s.Joins7d, &s.Leaves7d, &s.Returns7d); err != nil {
			log.Error("Failed to get health history for guild %d: %v", guildID, err)
			return nil
		}
		all = append(all, s)
	}
	if len(all) == 0 {
		return nil
	}
	step := int64(stepDays) * SecondsPerDay
	selected := []HealthSnapshot{all[0]}
	target := all[0].Date - step
	for _, r := range all[1:] {
		if len(selected) >= points {
			break
		}
		if r.Date <= target {
			selected = append(selected, r)
			target = r.Date - step
		}
	}
	for i, j := 0, len(selected)-1; i < j; i, j = i+1, j-1 {
		selected[i], selected[j] = selected[j], selected[i]
	}
	return selected
}

// PruneOldHealthSnapshots deletes snapshots older than retentionDays.
func (d *DB) PruneOldHealthSnapshots(retentionDays int) int64 {
	cutoff := todayStart() - int64(retentionDays)*SecondsPerDay
	res, err := d.sql.ExecContext(ctx(), "DELETE FROM health_snapshots WHERE date < ?", cutoff)
	if err != nil {
		log.Error("Failed to prune old health snapshots: %v", err)
		return 0
	}
	return affected(res)
}

// MessageCount is a member with a message total.
type MessageCount struct {
	UserID        int64
	Username      string
	Nickname      *string
	TotalMessages int64
}

// DisplayName is the nickname if set, else the username.
func (m MessageCount) DisplayName() string {
	if m.Nickname != nil && *m.Nickname != "" {
		return *m.Nickname
	}
	return m.Username
}

func (d *DB) messageCounts(what string, guildID int64, query string, args ...any) []MessageCount {
	rows, err := d.sql.QueryContext(ctx(), query, args...)
	if err != nil {
		log.Error("Failed to get %s for guild %d: %v", what, guildID, err)
		return nil
	}
	defer rows.Close()
	var out []MessageCount
	for rows.Next() {
		var m MessageCount
		var nick sql.NullString
		var total sql.NullInt64
		if err := rows.Scan(&m.UserID, &m.Username, &nick, &total); err != nil {
			log.Error("Failed to get %s for guild %d: %v", what, guildID, err)
			return nil
		}
		m.Nickname, m.TotalMessages = ptrStr(nick), total.Int64
		out = append(out, m)
	}
	return out
}

// GetActivityLeaderboard is the top current posters over days days (0 =
// all time).
func (d *DB) GetActivityLeaderboard(guildID int64, days, limit int) []MessageCount {
	if days > 0 {
		return d.messageCounts("activity leaderboard", guildID, `SELECT m.user_id, m.username, m.nickname,
				SUM(ma.message_count) as total_messages
			FROM message_activity ma
			JOIN members m ON ma.guild_id = m.guild_id AND ma.user_id = m.user_id
			WHERE ma.guild_id = ? AND ma.date >= ? AND m.is_active = 1
			GROUP BY ma.user_id ORDER BY total_messages DESC LIMIT ?`, guildID, DayWindowStart(days), limit)
	}
	return d.messageCounts("activity leaderboard", guildID, `SELECT m.user_id, m.username, m.nickname,
			SUM(ma.message_count) as total_messages
		FROM message_activity ma
		JOIN members m ON ma.guild_id = m.guild_id AND ma.user_id = m.user_id
		WHERE ma.guild_id = ? AND m.is_active = 1
		GROUP BY ma.user_id ORDER BY total_messages DESC LIMIT ?`, guildID, limit)
}

// GetTopActiveUsersPeriod is the top current posters over days days.
func (d *DB) GetTopActiveUsersPeriod(guildID int64, days, limit int, f *UserFilter) []MessageCount {
	fc, fp := f.clause("m.user_id")
	args := append([]any{guildID, DayWindowStart(days)}, fp...)
	args = append(args, limit)
	return d.messageCounts("top active users", guildID, `SELECT m.user_id, m.username, m.nickname,
			SUM(ma.message_count) as total_messages
		FROM message_activity ma
		JOIN members m ON ma.user_id = m.user_id AND ma.guild_id = m.guild_id
		WHERE ma.guild_id = ? AND ma.date >= ? AND m.is_active = 1`+fc+`
		GROUP BY m.user_id, m.username, m.nickname
		ORDER BY total_messages DESC LIMIT ?`, args...)
}

// pyWeekdayOf is Python's date.weekday() (Monday = 0).
func pyWeekdayOf(t time.Time) int { return (int(t.Weekday()) + 6) % 7 }

// GetActivityByDay sums messages per local weekday (Monday = 0) over the
// last days days, from the hourly table so the weekday is right in the
// guild's time zone. ok is false on error.
func (d *DB) GetActivityByDay(guildID int64, days int, f *UserFilter, tz string) (counts [7]int64, ok bool) {
	loc := LoadTZ(tz)
	fc, fp := f.clause("user_id")
	rows, err := d.sql.QueryContext(ctx(), `SELECT timestamp, SUM(message_count) as count
		FROM message_activity_hourly WHERE guild_id = ? AND timestamp >= ?`+fc+`
		GROUP BY timestamp`, append([]any{guildID, nowUTC() - int64(days)*SecondsPerDay}, fp...)...)
	if err != nil {
		log.Error("Failed to get activity by day for guild %d: %v", guildID, err)
		return counts, false
	}
	defer rows.Close()
	for rows.Next() {
		var ts int64
		var n sql.NullInt64
		if err := rows.Scan(&ts, &n); err != nil {
			log.Error("Failed to get activity by day for guild %d: %v", guildID, err)
			return [7]int64{}, false
		}
		counts[pyWeekdayOf(time.Unix(ts, 0).In(loc))] += n.Int64
	}
	return counts, true
}

// ActivityWindows is guild message activity bucketed in local time.
type ActivityWindows struct {
	Total       int64
	ActiveDays  int
	ByWeekday   [7]int64
	ByHour      [24]int64
	BestWeekday *int
	PeakHour    *int
	// Recommend / Quiet are (weekday, start hour, exclusive end hour).
	Recommend, Quiet *[3]int
}

// GetServerActivityWindows aggregates hourly activity into the guild's
// local weekday/hour grid and finds the busiest and quietest 3-hour window
// on a single weekday.
func (d *DB) GetServerActivityWindows(guildID int64, days int, tz string, f *UserFilter) ActivityWindows {
	const band = 3
	var w ActivityWindows
	loc := LoadTZ(tz)
	fc, fp := f.clause("user_id")
	rows, err := d.sql.QueryContext(ctx(), `SELECT timestamp, message_count
		FROM message_activity_hourly WHERE guild_id = ? AND timestamp >= ?`+fc,
		append([]any{guildID, nowUTC() - int64(days)*SecondsPerDay}, fp...)...)
	if err != nil {
		log.Error("Failed to build server activity windows for guild %d: %v", guildID, err)
		return ActivityWindows{}
	}
	defer rows.Close()
	var grid [7][24]int64
	dates := map[[3]int]bool{}
	for rows.Next() {
		var ts int64
		var n sql.NullInt64
		if err := rows.Scan(&ts, &n); err != nil {
			log.Error("Failed to build server activity windows for guild %d: %v", guildID, err)
			return ActivityWindows{}
		}
		if n.Int64 <= 0 {
			continue
		}
		local := time.Unix(ts, 0).In(loc)
		wd, h := pyWeekdayOf(local), local.Hour()
		w.ByWeekday[wd] += n.Int64
		w.ByHour[h] += n.Int64
		grid[wd][h] += n.Int64
		dates[[3]int{local.Year(), int(local.Month()), local.Day()}] = true
		w.Total += n.Int64
	}
	if w.Total == 0 {
		return ActivityWindows{}
	}
	w.ActiveDays = len(dates)
	best, peak := 0, 0
	for i := 1; i < 7; i++ {
		if w.ByWeekday[i] > w.ByWeekday[best] {
			best = i
		}
	}
	for i := 1; i < 24; i++ {
		if w.ByHour[i] > w.ByHour[peak] {
			peak = i
		}
	}
	w.BestWeekday, w.PeakHour = &best, &peak
	bestSum, quietSum := int64(-1), int64(-1)
	for dd := 0; dd < 7; dd++ {
		for start := 0; start <= 24-band; start++ {
			var s int64
			for k := 0; k < band; k++ {
				s += grid[dd][start+k]
			}
			if s > bestSum {
				bestSum, w.Recommend = s, &[3]int{dd, start, start + band}
			}
			if quietSum < 0 || s < quietSum {
				quietSum, w.Quiet = s, &[3]int{dd, start, start + band}
			}
		}
	}
	return w
}

// ReportGuild is a guild with scheduled reports enabled.
type ReportGuild struct {
	GuildID, ChannelID      int64
	Frequency               string
	Types                   *string
	DayWeekly, DayMonthly   *int64
	LastWeekly, LastMonthly *int64
	Timezone                *string
	Hour                    *int64
}

// GetGuildsWithReportsEnabled lists guilds with a report channel and frequency.
func (d *DB) GetGuildsWithReportsEnabled() []ReportGuild {
	rows, err := d.sql.QueryContext(ctx(), `SELECT guild_id, report_channel_id, report_frequency, report_types,
			report_day_weekly, report_day_monthly, last_weekly_report, last_monthly_report,
			timezone, report_time_hour
		FROM guilds WHERE report_frequency IS NOT NULL AND report_channel_id IS NOT NULL`)
	if err != nil {
		log.Error("Failed to get guilds with reports enabled: %v", err)
		return nil
	}
	defer rows.Close()
	var out []ReportGuild
	for rows.Next() {
		var g ReportGuild
		var types, tz sql.NullString
		var dw, dm, lw, lm, hr sql.NullInt64
		if err := rows.Scan(&g.GuildID, &g.ChannelID, &g.Frequency, &types, &dw, &dm, &lw, &lm, &tz, &hr); err != nil {
			log.Error("Failed to get guilds with reports enabled: %v", err)
			return nil
		}
		g.Types, g.Timezone = ptrStr(types), ptrStr(tz)
		g.DayWeekly, g.DayMonthly, g.LastWeekly, g.LastMonthly, g.Hour = ptrInt(dw), ptrInt(dm), ptrInt(lw), ptrInt(lm), ptrInt(hr)
		out = append(out, g)
	}
	return out
}

// PeriodMember is a member row in a report's joins / departures list.
type PeriodMember struct {
	UserID       int64
	Username     string
	Nickname     *string
	JoinDate     *int64
	JoinPosition *int64
	LastSeen     *int64
	LeftDate     *int64
}

func (d *DB) periodMembers(what string, guildID int64, departed bool, query string, args ...any) []PeriodMember {
	rows, err := d.sql.QueryContext(ctx(), query, args...)
	if err != nil {
		log.Error("Failed to get %s for guild %d: %v", what, guildID, err)
		return nil
	}
	defer rows.Close()
	var out []PeriodMember
	for rows.Next() {
		var m PeriodMember
		var nick sql.NullString
		var a, b sql.NullInt64
		if err := rows.Scan(&m.UserID, &m.Username, &nick, &a, &b); err != nil {
			log.Error("Failed to get %s for guild %d: %v", what, guildID, err)
			return nil
		}
		m.Nickname = ptrStr(nick)
		if departed {
			m.LastSeen, m.LeftDate = ptrInt(a), ptrInt(b)
		} else {
			m.JoinDate, m.JoinPosition = ptrInt(a), ptrInt(b)
		}
		out = append(out, m)
	}
	return out
}

// GetNewMembersPeriod lists members who joined in the last days days
// (including those who left again), newest first.
func (d *DB) GetNewMembersPeriod(guildID int64, days int) []PeriodMember {
	cutoff := nowUTC() - int64(days)*SecondsPerDay
	return d.periodMembers("new members", guildID, false, `SELECT user_id, username, nickname, join_date, join_position
		FROM members WHERE guild_id = ? AND join_date >= ? ORDER BY join_date DESC`, guildID, cutoff)
}

// GetDepartedMembersPeriod lists members who left in the last days days
// (including those who rejoined), most recent first.
func (d *DB) GetDepartedMembersPeriod(guildID int64, days int) []PeriodMember {
	cutoff := nowUTC() - int64(days)*SecondsPerDay
	return d.periodMembers("departed members", guildID, true, `SELECT user_id, username, nickname, last_seen, left_date
		FROM members WHERE guild_id = ? AND `+LeftTSSQL+` >= ? ORDER BY `+LeftTSSQL+` DESC`, guildID, cutoff)
}

// CleanupOldMessageActivity deletes a guild's activity older than
// retentionDays; returns (daily, hourly) rows deleted.
func (d *DB) CleanupOldMessageActivity(guildID int64, retentionDays int64) (int64, int64) {
	cutoff := nowUTC() - retentionDays*86400
	var daily, hourly int64
	err := d.tx(func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx(), "DELETE FROM message_activity WHERE guild_id = ? AND date < ?", guildID, cutoff)
		if err != nil {
			return err
		}
		daily = affected(res)
		res, err = tx.ExecContext(ctx(), "DELETE FROM message_activity_hourly WHERE guild_id = ? AND timestamp < ?", guildID, cutoff)
		if err != nil {
			return err
		}
		hourly = affected(res)
		return nil
	})
	if err != nil {
		log.Error("Failed to cleanup old activity for guild %d: %v", guildID, err)
		return 0, 0
	}
	if daily > 0 || hourly > 0 {
		log.Info("Cleaned up old activity for guild %d: %d daily, %d hourly records", guildID, daily, hourly)
	}
	return daily, hourly
}

// CleanupAllGuildsMessageActivity applies every guild's retention setting.
func (d *DB) CleanupAllGuildsMessageActivity() (daily, hourly int64, guilds int) {
	rows, err := d.sql.QueryContext(ctx(), "SELECT guild_id, message_retention_days FROM guilds WHERE message_retention_days IS NOT NULL")
	if err != nil {
		log.Error("Failed to run global cleanup: %v", err)
		return 0, 0, 0
	}
	type gr struct{ id, days int64 }
	var list []gr
	for rows.Next() {
		var g gr
		var days sql.NullInt64
		if err := rows.Scan(&g.id, &days); err != nil {
			rows.Close()
			log.Error("Failed to run global cleanup: %v", err)
			return 0, 0, 0
		}
		g.days = days.Int64
		if g.days == 0 {
			g.days = 365
		}
		list = append(list, g)
	}
	rows.Close()
	for _, g := range list {
		a, b := d.CleanupOldMessageActivity(g.id, g.days)
		daily += a
		hourly += b
	}
	if daily > 0 || hourly > 0 {
		log.Info("Global cleanup completed: %d daily, %d hourly records deleted across %d guilds", daily, hourly, len(list))
	}
	return daily, hourly, len(list)
}

// GetBotState reads a bot-level value (nil if unset or on error).
func (d *DB) GetBotState(key string) *int64 {
	var v sql.NullInt64
	err := d.sql.QueryRowContext(ctx(), "SELECT value FROM bot_state WHERE key = ?", key).Scan(&v)
	if err != nil {
		if err != sql.ErrNoRows {
			log.Error("Failed to read bot state '%s': %v", key, err)
		}
		return nil
	}
	return ptrInt(v)
}

// SetBotState writes a bot-level value.
func (d *DB) SetBotState(key string, value int64) bool {
	_, err := d.sql.ExecContext(ctx(), `INSERT INTO bot_state (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	if err != nil {
		log.Error("Failed to write bot state '%s': %v", key, err)
		return false
	}
	return true
}

// BotStatistics are the global /about counts.
type BotStatistics struct {
	TotalGuilds, TotalUsers, Last24h, Last7d int64
}

// GetBotStatistics returns global counts, cached for BotStatsCacheTTL (every
// query is a cross-guild full scan).
func (d *DB) GetBotStatistics() BotStatistics {
	now := time.Now()
	d.statsMu.Lock()
	if d.statsCache != nil && now.Before(d.statsExpires) {
		s := *d.statsCache
		d.statsMu.Unlock()
		return s
	}
	d.statsMu.Unlock()
	var s BotStatistics
	var err error
	for _, q := range []struct {
		dst  *int64
		sql  string
		args []any
	}{
		{&s.TotalGuilds, "SELECT COUNT(DISTINCT guild_id) FROM guilds", nil},
		{&s.TotalUsers, "SELECT COUNT(*) FROM members WHERE is_active = 1", nil},
		{&s.Last24h, "SELECT COALESCE(SUM(message_count), 0) FROM message_activity_hourly WHERE timestamp >= ?", []any{nowUTC() - SecondsPerDay}},
		{&s.Last7d, "SELECT COALESCE(SUM(message_count), 0) FROM message_activity WHERE date >= ?", []any{DayWindowStart(7)}},
	} {
		if *q.dst, err = scalarInt(d.sql, q.sql, q.args...); err != nil {
			log.Error("Failed to get bot statistics: %v", err)
			return BotStatistics{}
		}
	}
	d.statsMu.Lock()
	c := s
	d.statsCache, d.statsExpires = &c, now.Add(BotStatsCacheTTL)
	d.statsMu.Unlock()
	return s
}
