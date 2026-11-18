package db

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// ActivityStats are last-seen bucket counts for current members.
type ActivityStats struct {
	CurrentlyOnline, CurrentlyOffline, NeverSeenOffline          int64
	Offline1h, Offline24h, Offline7d, Offline30d, Offline30dPlus int64
}

// GetActivityStats counts current members by how long ago they were seen.
func (d *DB) GetActivityStats(guildID int64, f *UserFilter) ActivityStats {
	var s ActivityStats
	now := nowUTC()
	fc, fp := f.clause("user_id")
	count := func(cond string, args ...any) (int64, error) {
		a := append([]any{guildID}, args...)
		return scalarInt(d.sql, "SELECT COUNT(*) FROM members WHERE guild_id = ? AND is_active = 1"+cond+fc, append(a, fp...)...)
	}
	var err error
	steps := []struct {
		dst  *int64
		cond string
		args []any
	}{
		{&s.CurrentlyOnline, " AND last_seen = 0", nil},
		{&s.NeverSeenOffline, " AND last_seen IS NULL", nil},
		{&s.Offline1h, " AND last_seen > 0 AND last_seen >= ?", []any{now - 3600}},
		{&s.Offline24h, " AND last_seen > 0 AND last_seen >= ?", []any{now - SecondsPerDay}},
		{&s.Offline7d, " AND last_seen > 0 AND last_seen >= ?", []any{now - 7*SecondsPerDay}},
		{&s.Offline30d, " AND last_seen > 0 AND last_seen >= ?", []any{now - 30*SecondsPerDay}},
		{&s.Offline30dPlus, " AND last_seen > 0 AND last_seen < ?", []any{now - 30*SecondsPerDay}},
	}
	for _, st := range steps {
		if *st.dst, err = count(st.cond, st.args...); err != nil {
			log.Error("Failed to get activity stats for %d: %v", guildID, err)
			return s
		}
	}
	total, err := count("")
	if err != nil {
		log.Error("Failed to get activity stats for %d: %v", guildID, err)
		return s
	}
	s.CurrentlyOffline = total - s.CurrentlyOnline
	return s
}

// ParticipationSegments split current members into lurkers and ghosts.
type ParticipationSegments struct {
	TotalActive, Lurkers, Ghosts int64
	LurkerPct                    float64
}

const lurkerCond = `m.guild_id = ? AND m.is_active = 1
	AND (m.join_date IS NULL OR m.join_date < ?)
	AND (m.last_seen = 0 OR (m.last_seen IS NOT NULL AND m.last_seen >= ?))
	AND NOT EXISTS (
		SELECT 1 FROM message_activity ma
		WHERE ma.guild_id = m.guild_id AND ma.user_id = m.user_id AND ma.date >= ?
	)`

const ghostCond = `m.guild_id = ? AND m.is_active = 1
	AND (m.join_date IS NULL OR m.join_date < ?)
	AND (m.last_seen IS NULL OR (m.last_seen > 0 AND m.last_seen < ?))
	AND NOT EXISTS (
		SELECT 1 FROM message_activity ma
		WHERE ma.guild_id = m.guild_id AND ma.user_id = m.user_id
	)`

// GetParticipationSegments counts lurkers (seen, silent) and ghosts (never
// posted, not seen) among established current members.
func (d *DB) GetParticipationSegments(guildID int64, windowDays int, f *UserFilter) ParticipationSegments {
	var r ParticipationSegments
	ws := DayWindowStart(windowDays)
	fc, fp := f.clause("m.user_id")
	var err error
	fail := func() ParticipationSegments {
		log.Error("Failed to get participation segments for guild %d: %v", guildID, err)
		return r
	}
	if r.TotalActive, err = scalarInt(d.sql, "SELECT COUNT(*) FROM members m WHERE m.guild_id = ? AND m.is_active = 1"+fc,
		append([]any{guildID}, fp...)...); err != nil {
		return fail()
	}
	if r.Lurkers, err = scalarInt(d.sql, "SELECT COUNT(*) FROM members m WHERE "+lurkerCond+fc,
		append([]any{guildID, ws, ws, ws}, fp...)...); err != nil {
		return fail()
	}
	if r.Ghosts, err = scalarInt(d.sql, "SELECT COUNT(*) FROM members m WHERE "+ghostCond+fc,
		append([]any{guildID, ws, ws}, fp...)...); err != nil {
		return fail()
	}
	if r.TotalActive > 0 {
		r.LurkerPct = float64(r.Lurkers) / float64(r.TotalActive) * 100
	}
	return r
}

// LifecycleSegments counts current members per lifecycle stage.
type LifecycleSegments map[string]int64

// LifecycleOrder is the fixed display order of the stages.
var LifecycleOrder = []string{"new", "exploring", "active", "quiet", "dormant", "returned"}

// GetLifecycleSegments classifies current members into new / exploring /
// returned / active / quiet / dormant (first matching rule wins).
func (d *DB) GetLifecycleSegments(guildID int64) LifecycleSegments {
	const newDays, activeWindow, activeMin, quietWindow, returnDays = 7, 30, 5, 90, 14
	r := LifecycleSegments{"new": 0, "exploring": 0, "returned": 0, "active": 0, "quiet": 0, "dormant": 0}
	now := nowUTC()
	newWin := now - newDays*SecondsPerDay
	retWin := now - returnDays*SecondsPerDay
	activeWin := DayWindowStart(activeWindow)
	quietWin := DayWindowStart(quietWindow)
	rows, err := d.sql.QueryContext(ctx(), `
		SELECT
			CASE
				WHEN m.join_date >= ? AND agg.last_post IS NULL         THEN 'new'
				WHEN m.join_date >= ?                                   THEN 'exploring'
				WHEN ret.user_id IS NOT NULL                            THEN 'returned'
				WHEN COALESCE(agg.msgs_recent, 0) >= ?                  THEN 'active'
				WHEN (m.last_seen = 0 OR m.last_seen >= ?)
				  OR (agg.last_post IS NOT NULL AND agg.last_post >= ?) THEN 'quiet'
				ELSE 'dormant'
			END AS bucket,
			COUNT(*)
		FROM members m
		LEFT JOIN (
			SELECT user_id, MAX(date) AS last_post,
				   SUM(CASE WHEN date >= ? THEN message_count ELSE 0 END) AS msgs_recent
			FROM message_activity WHERE guild_id = ? GROUP BY user_id
		) agg ON agg.user_id = m.user_id
		LEFT JOIN (
			SELECT DISTINCT user_id FROM member_returns WHERE guild_id = ? AND returned_at >= ?
		) ret ON ret.user_id = m.user_id
		WHERE m.guild_id = ? AND m.is_active = 1
		GROUP BY bucket`,
		newWin, newWin, activeMin, activeWin, quietWin, activeWin, guildID, guildID, retWin, guildID)
	if err != nil {
		log.Error("Failed to get lifecycle segments for guild %d: %v", guildID, err)
		return r
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		var n int64
		if err := rows.Scan(&b, &n); err != nil {
			log.Error("Failed to get lifecycle segments for guild %d: %v", guildID, err)
			return r
		}
		r[b] = n
	}
	if err := rows.Err(); err != nil {
		log.Error("Failed to get lifecycle segments for guild %d: %v", guildID, err)
		return r
	}
	return r
}

// SegmentMember is a lurker/ghost list entry.
type SegmentMember struct {
	UserID   int64
	Username string
	Nickname *string
	LastSeen *int64
}

func (d *DB) segmentMembers(what string, guildID int64, query string, args []any, limit int) []SegmentMember {
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := d.sql.QueryContext(ctx(), query, args...)
	if err != nil {
		log.Error("Failed to get %s for guild %d: %v", what, guildID, err)
		return nil
	}
	defer rows.Close()
	var out []SegmentMember
	for rows.Next() {
		var m SegmentMember
		var nick sql.NullString
		var last sql.NullInt64
		if err := rows.Scan(&m.UserID, &m.Username, &nick, &last); err != nil {
			log.Error("Failed to get %s for guild %d: %v", what, guildID, err)
			return nil
		}
		m.Nickname, m.LastSeen = ptrStr(nick), ptrInt(last)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		log.Error("Failed to get %s for guild %d: %v", what, guildID, err)
		return nil
	}
	return out
}

// GetLurkers lists lurkers, most recently seen first (limit 0 = all).
func (d *DB) GetLurkers(guildID int64, windowDays, limit int) []SegmentMember {
	ws := DayWindowStart(windowDays)
	return d.segmentMembers("lurkers", guildID, `SELECT m.user_id, m.username, m.nickname, m.last_seen
		FROM members m WHERE `+lurkerCond+`
		ORDER BY CASE WHEN m.last_seen = 0 THEN 1 ELSE 0 END DESC, m.last_seen DESC`,
		[]any{guildID, ws, ws, ws}, limit)
}

// GetGhosts lists ghosts, longest gone first (limit 0 = all).
func (d *DB) GetGhosts(guildID int64, windowDays, limit int) []SegmentMember {
	ws := DayWindowStart(windowDays)
	return d.segmentMembers("ghosts", guildID, `SELECT m.user_id, m.username, m.nickname, m.last_seen
		FROM members m WHERE `+ghostCond+` ORDER BY m.last_seen ASC`,
		[]any{guildID, ws, ws}, limit)
}

// GuildRemoval is what RemoveGuildData deleted.
type GuildRemoval struct {
	Found                  bool // the guild was in the database
	Members, Daily, Hourly int64
}

func (r GuildRemoval) String() string {
	return fmt.Sprintf("%d members, %d daily and %d hourly activity records", r.Members, r.Daily, r.Hourly)
}

// RemoveGuildData deletes a guild and (by cascade) all of its data. ok is
// false on a database error; the caller logs the outcome.
func (d *DB) RemoveGuildData(guildID int64) (r GuildRemoval, ok bool) {
	err := d.tx(func(tx *sql.Tx) error {
		var err error
		if r.Members, err = scalarInt(tx, "SELECT COUNT(*) FROM members WHERE guild_id = ?", guildID); err != nil {
			return err
		}
		if r.Daily, err = scalarInt(tx, "SELECT COUNT(*) FROM message_activity WHERE guild_id = ?", guildID); err != nil {
			return err
		}
		if r.Hourly, err = scalarInt(tx, "SELECT COUNT(*) FROM message_activity_hourly WHERE guild_id = ?", guildID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx(), "DELETE FROM guilds WHERE guild_id = ?", guildID)
		r.Found = affected(res) > 0
		return err
	})
	if err != nil {
		log.Error("Failed to remove guild data for %d: %v", guildID, err)
		return GuildRemoval{}, false
	}
	return r, true
}

func (d *DB) addedAt(q querier, guildID int64) (int64, error) {
	v, err := scalarInt(q, "SELECT added_at FROM guilds WHERE guild_id = ?", guildID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

// SnapshotStats is the /user-stats overview data.
type SnapshotStats struct {
	ActiveMembers, Active30d, Inactive30d      int64
	JoinsThisMonth, LeavesThisMonth, NetGrowth int64
	TotalMessages30d                           int64
	AvgMessagesPerMember                       Num
	MostActiveUser                             string
	MostActiveCount                            int64
}

// GetServerSnapshotStats returns the overview numbers (nil on error).
func (d *DB) GetServerSnapshotStats(guildID int64) *SnapshotStats {
	fail := func(err error) *SnapshotStats {
		log.Error("Failed to get server snapshot stats for guild %d: %v", guildID, err)
		return nil
	}
	now := nowUTC()
	thirty := now - 30*SecondsPerDay
	added, err := d.addedAt(d.sql, guildID)
	if err != nil {
		return fail(err)
	}
	var s SnapshotStats
	var am, a30 sql.NullInt64
	if err := d.sql.QueryRowContext(ctx(), `SELECT
			SUM(CASE WHEN is_active = 1 THEN 1 ELSE 0 END),
			SUM(CASE WHEN is_active = 1 AND `+SeenSinceSQL+` THEN 1 ELSE 0 END)
		FROM members WHERE guild_id = ?`, thirty, added, thirty, thirty, guildID).Scan(&am, &a30); err != nil {
		return fail(err)
	}
	s.ActiveMembers, s.Active30d = am.Int64, a30.Int64
	n := time.Unix(now, 0).UTC()
	monthStart := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC).Unix()
	if s.JoinsThisMonth, err = scalarInt(d.sql, "SELECT COUNT(*) FROM members WHERE guild_id = ? AND join_date >= ? AND join_date >= ?",
		guildID, monthStart, added); err != nil {
		return fail(err)
	}
	if s.LeavesThisMonth, err = scalarInt(d.sql, "SELECT COUNT(*) FROM members WHERE guild_id = ? AND "+LeftTSSQL+" >= ? AND "+LeftTSSQL+" >= ?",
		guildID, monthStart, added); err != nil {
		return fail(err)
	}
	monthWindow := DayWindowStart(30)
	if s.TotalMessages30d, err = scalarInt(d.sql, "SELECT SUM(message_count) FROM message_activity WHERE guild_id = ? AND date >= ?",
		guildID, monthWindow); err != nil {
		return fail(err)
	}
	var name string
	var cnt sql.NullInt64
	err = d.sql.QueryRowContext(ctx(), `SELECT m.username, SUM(ma.message_count) as msg_count
		FROM message_activity ma
		JOIN members m ON ma.guild_id = m.guild_id AND ma.user_id = m.user_id
		WHERE ma.guild_id = ? AND ma.date >= ?
		GROUP BY ma.user_id ORDER BY msg_count DESC LIMIT 1`, guildID, monthWindow).Scan(&name, &cnt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		s.MostActiveUser = "N/A"
	case err != nil:
		return fail(err)
	default:
		s.MostActiveUser, s.MostActiveCount = name, cnt.Int64
	}
	s.Inactive30d = s.ActiveMembers - s.Active30d
	s.NetGrowth = s.JoinsThisMonth - s.LeavesThisMonth
	s.AvgMessagesPerMember = int64(0)
	if s.ActiveMembers > 0 {
		s.AvgMessagesPerMember = float64(s.TotalMessages30d) / float64(s.ActiveMembers)
	}
	return &s
}

// GrowthStats are joins/leaves over a period.
type GrowthStats struct {
	Joins, Leaves, NetGrowth, CurrentTotal, PreviousTotal int64
	GrowthRate                                            float64
	PeriodDays                                            int
}

// GetMemberGrowthStats returns growth over the last days days (nil on error).
func (d *DB) GetMemberGrowthStats(guildID int64, days int, f *UserFilter) *GrowthStats {
	fail := func(err error) *GrowthStats {
		log.Error("Failed to get member growth stats for guild %d: %v", guildID, err)
		return nil
	}
	fc, fp := f.clause("user_id")
	periodStart := nowUTC() - int64(days)*SecondsPerDay
	added, err := d.addedAt(d.sql, guildID)
	if err != nil {
		return fail(err)
	}
	g := GrowthStats{PeriodDays: days}
	if g.Joins, err = scalarInt(d.sql, "SELECT COUNT(*) FROM members WHERE guild_id = ? AND join_date >= ? AND join_date >= ?"+fc,
		append([]any{guildID, periodStart, added}, fp...)...); err != nil {
		return fail(err)
	}
	if g.Leaves, err = scalarInt(d.sql, "SELECT COUNT(*) FROM members WHERE guild_id = ? AND "+LeftTSSQL+" >= ? AND "+LeftTSSQL+" >= ?"+fc,
		append([]any{guildID, periodStart, added}, fp...)...); err != nil {
		return fail(err)
	}
	if g.CurrentTotal, err = scalarInt(d.sql, "SELECT COUNT(*) FROM members WHERE guild_id = ? AND is_active = 1"+fc,
		append([]any{guildID}, fp...)...); err != nil {
		return fail(err)
	}
	g.PreviousTotal = g.CurrentTotal - g.Joins + g.Leaves
	if g.PreviousTotal > 0 {
		g.GrowthRate = float64(g.CurrentTotal-g.PreviousTotal) / float64(g.PreviousTotal) * 100
	}
	g.NetGrowth = g.Joins - g.Leaves
	return &g
}

// Cohort is one join cohort's retention.
type Cohort struct {
	TotalJoined, StillActive, ActiveRecently int64
	RetentionRate                            Num
}

const activationMinCohort = 5

// CohortPeriods is the cohort display order.
var CohortPeriods = []string{"30d", "60d", "90d"}

// GetRetentionCohorts returns the 30/60/90-day join cohorts with at least 5
// joiners (only members who joined after the bot arrived count).
func (d *DB) GetRetentionCohorts(guildID int64, f *UserFilter) map[string]Cohort {
	fc, fp := f.clause("user_id")
	now := nowUTC()
	t30, t60, t90 := now-30*SecondsPerDay, now-60*SecondsPerDay, now-90*SecondsPerDay
	added, err := d.addedAt(d.sql, guildID)
	if err != nil {
		log.Error("Failed to get retention cohorts for guild %d: %v", guildID, err)
		return map[string]Cohort{}
	}
	sel := `SELECT COUNT(*),
			SUM(CASE WHEN is_active = 1 THEN 1 ELSE 0 END),
			SUM(CASE WHEN is_active = 1 AND ` + SeenSinceSQL + ` THEN 1 ELSE 0 END)
		FROM members WHERE guild_id = ? AND join_date >= ?`
	type q struct {
		key  string
		cond string
		args []any
	}
	qs := []q{
		{"30d", " AND join_date >= ?", []any{t30, added}},
		{"60d", " AND join_date < ? AND join_date >= ?", []any{t60, t30, added}},
		{"90d", " AND join_date < ? AND join_date >= ?", []any{t90, t60, added}},
	}
	out := map[string]Cohort{}
	for _, c := range qs {
		args := append([]any{t30, added, t30, t30, guildID}, c.args...)
		args = append(args, fp...)
		var total int64
		var still, recent sql.NullInt64
		if err := d.sql.QueryRowContext(ctx(), sel+c.cond+fc, args...).Scan(&total, &still, &recent); err != nil {
			log.Error("Failed to get retention cohorts for guild %d: %v", guildID, err)
			return map[string]Cohort{}
		}
		co := Cohort{TotalJoined: total, StillActive: still.Int64, ActiveRecently: recent.Int64, RetentionRate: int64(0)}
		if total > 0 {
			co.RetentionRate = float64(still.Int64) / float64(total) * 100
		}
		if co.TotalJoined >= activationMinCohort {
			out[c.key] = co
		}
	}
	return out
}

var activationEras = []struct {
	label  string
	ws, we int64
}{{"D1", 0, 1}, {"D7", 2, 7}, {"D30", 8, 30}, {"D90", 31, 90}}

// Checkpoint is one activation-funnel era.
type Checkpoint struct {
	Label           string
	Active, Matured int64
	Rate            float64
}

// ActivationFunnel is the windowed new-member activation funnel.
type ActivationFunnel struct {
	CohortSize     int
	Checkpoints    []Checkpoint
	LargestDropoff *[2]string
}

// GetActivationFunnel measures the share of recent joiners who posted during
// each era after joining (only members old enough to have lived through an
// era count towards it).
func (d *DB) GetActivationFunnel(guildID int64, f *UserFilter) ActivationFunnel {
	fail := func(err error) ActivationFunnel {
		log.Error("Failed to get activation funnel for guild %d: %v", guildID, err)
		return ActivationFunnel{}
	}
	now := nowUTC()
	var ret, added sql.NullInt64
	err := d.sql.QueryRowContext(ctx(), "SELECT message_retention_days, added_at FROM guilds WHERE guild_id = ?", guildID).Scan(&ret, &added)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fail(err)
	}
	retentionDays := int64(365)
	if ret.Valid && ret.Int64 != 0 {
		retentionDays = ret.Int64
	}
	cutoff := max(now-retentionDays*SecondsPerDay, added.Int64)

	fc, fp := f.clause("user_id")
	rows, err := d.sql.QueryContext(ctx(), "SELECT user_id, join_date FROM members WHERE guild_id = ? AND join_date IS NOT NULL AND join_date >= ?"+fc,
		append([]any{guildID, cutoff}, fp...)...)
	if err != nil {
		return fail(err)
	}
	joinDay := map[int64]int64{}
	var order []int64
	for rows.Next() {
		var uid, jd int64
		if err := rows.Scan(&uid, &jd); err != nil {
			rows.Close()
			return fail(err)
		}
		if _, seen := joinDay[uid]; !seen {
			order = append(order, uid)
		}
		joinDay[uid] = jd - floorMod(jd, SecondsPerDay)
	}
	rows.Close()
	res := ActivationFunnel{CohortSize: len(joinDay)}
	if res.CohortSize < activationMinCohort {
		return res
	}
	rows, err = d.sql.QueryContext(ctx(), `SELECT ma.user_id, ma.date FROM message_activity ma
		JOIN members m ON m.guild_id = ma.guild_id AND m.user_id = ma.user_id
		WHERE ma.guild_id = ? AND m.join_date >= ? AND ma.date >= ?`, guildID, cutoff, cutoff)
	if err != nil {
		return fail(err)
	}
	activity := map[int64][]int64{}
	for rows.Next() {
		var uid, date int64
		if err := rows.Scan(&uid, &date); err != nil {
			rows.Close()
			return fail(err)
		}
		activity[uid] = append(activity[uid], date)
	}
	rows.Close()
	active := map[string]int64{}
	matured := map[string]int64{}
	for _, uid := range order {
		jday := joinDay[uid]
		dates := activity[uid]
		for _, e := range activationEras {
			ws, we := jday+e.ws*SecondsPerDay, jday+e.we*SecondsPerDay
			if now < we {
				continue
			}
			matured[e.label]++
			for _, dt := range dates {
				if ws <= dt && dt <= we {
					active[e.label]++
					break
				}
			}
		}
	}
	for _, e := range activationEras {
		if matured[e.label] > 0 {
			res.Checkpoints = append(res.Checkpoints, Checkpoint{
				Label: e.label, Active: active[e.label], Matured: matured[e.label],
				Rate: float64(active[e.label]) / float64(matured[e.label]) * 100,
			})
		}
	}
	worst := 0.0
	for i := 1; i < len(res.Checkpoints); i++ {
		drop := res.Checkpoints[i-1].Rate - res.Checkpoints[i].Rate
		if drop > worst {
			worst = drop
			res.LargestDropoff = &[2]string{res.Checkpoints[i-1].Label, res.Checkpoints[i].Label}
		}
	}
	return res
}

// LifespanBuckets are non-overlapping tenure ranges of departed members.
type LifespanBuckets struct{ Under1d, Under7d, Under30d, Under90d, Over90d int64 }

// DepartureLifespan is how long departed members stayed.
type DepartureLifespan struct {
	Sample        int
	MedianDays    int64
	AvgDays       int64
	Buckets       LifespanBuckets
	EarlyChurnPct float64
}

// GetDepartureLifespan analyses tenure (left_date - join_date) of departed
// members; stats stay zero below a sample of 3.
func (d *DB) GetDepartureLifespan(guildID int64, f *UserFilter) DepartureLifespan {
	const minSample, earlyChurnDays = 3, 7
	var r DepartureLifespan
	fc, fp := f.clause("user_id")
	tenures, err := queryInts(d.sql, `SELECT (left_date - join_date) AS tenure FROM members
		WHERE guild_id = ? AND is_active = 0
		  AND left_date IS NOT NULL AND join_date IS NOT NULL
		  AND left_date > join_date`+fc, append([]any{guildID}, fp...)...)
	if err != nil {
		log.Error("Failed to get departure lifespan for guild %d: %v", guildID, err)
		return r
	}
	sort.Slice(tenures, func(i, j int) bool { return tenures[i] < tenures[j] })
	n := len(tenures)
	r.Sample = n
	if n < minSample {
		return r
	}
	mid := n / 2
	var median float64
	if n%2 == 1 {
		median = float64(tenures[mid])
	} else {
		median = float64(tenures[mid-1]+tenures[mid]) / 2
	}
	r.MedianDays = roundHalfEven(median / SecondsPerDay)
	var sum int64
	for _, t := range tenures {
		sum += t
	}
	r.AvgDays = roundHalfEven(float64(sum) / float64(n) / SecondsPerDay)
	const dd = SecondsPerDay
	early := 0
	for _, t := range tenures {
		switch {
		case t < 1*dd:
			r.Buckets.Under1d++
		case t < 7*dd:
			r.Buckets.Under7d++
		case t < 30*dd:
			r.Buckets.Under30d++
		case t < 90*dd:
			r.Buckets.Under90d++
		default:
			r.Buckets.Over90d++
		}
		if t < earlyChurnDays*dd {
			early++
		}
	}
	r.EarlyChurnPct = float64(early) / float64(n) * 100
	return r
}

// Activation is a join cohort's first-week activation.
type Activation struct {
	Matured, Activated int64
	Rate               float64
}

// ServerHealth is the Community Pulse data (current vs previous windows).
type ServerHealth struct {
	DaysOfData                    int64
	WeeklyComparable              bool
	MonthlyComparable             bool
	Posters7d, PostersPrev7d      int64
	Messages7d, MessagesPrev7d    int64
	Posters30d, PostersPrev30d    int64
	Messages30d, MessagesPrev30d  int64
	ActiveMembers                 int64
	Joins30d, JoinsPrev30d        int64
	Leaves30d, LeavesPrev30d      int64
	Returns30d, ReturnsPrev30d    int64
	ActivationCur, ActivationPrev *Activation
}

const (
	healthWeekHistoryDays  = 14
	healthMonthHistoryDays = 60
)

// GetServerHealth returns the health vitals (nil on error).
func (d *DB) GetServerHealth(guildID int64, f *UserFilter) *ServerHealth {
	fail := func(err error) *ServerHealth {
		log.Error("Failed to get server health for guild %d: %v", guildID, err)
		return nil
	}
	now := nowUTC()
	today := todayStart()
	const dd = SecondsPerDay
	var addedN, retN sql.NullInt64
	err := d.sql.QueryRowContext(ctx(), "SELECT added_at, message_retention_days FROM guilds WHERE guild_id = ?", guildID).Scan(&addedN, &retN)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fail(err)
	}
	added := now
	if err == nil {
		added = addedN.Int64
	}
	retentionDays := int64(365)
	if retN.Valid && retN.Int64 != 0 {
		retentionDays = retN.Int64
	}
	h := ServerHealth{
		DaysOfData:        max(0, floorDiv(now-added, dd)),
		WeeklyComparable:  added <= today-healthWeekHistoryDays*dd,
		MonthlyComparable: added <= now-healthMonthHistoryDays*dd,
	}
	fc, fp := f.clause("user_id")
	msgWindow := func(start, end int64) (int64, int64, error) {
		var p, m int64
		err := d.sql.QueryRowContext(ctx(), `SELECT COUNT(DISTINCT user_id), COALESCE(SUM(message_count), 0)
			FROM message_activity WHERE guild_id = ? AND date >= ? AND date < ?`+fc,
			append([]any{guildID, start, end}, fp...)...).Scan(&p, &m)
		return p, m, err
	}
	if h.Posters7d, h.Messages7d, err = msgWindow(today-7*dd, today); err != nil {
		return fail(err)
	}
	if h.PostersPrev7d, h.MessagesPrev7d, err = msgWindow(today-14*dd, today-7*dd); err != nil {
		return fail(err)
	}
	if h.Posters30d, h.Messages30d, err = msgWindow(today-30*dd, today); err != nil {
		return fail(err)
	}
	if h.PostersPrev30d, h.MessagesPrev30d, err = msgWindow(today-60*dd, today-30*dd); err != nil {
		return fail(err)
	}
	if h.ActiveMembers, err = scalarInt(d.sql, "SELECT COUNT(*) FROM members WHERE guild_id = ? AND is_active = 1"+fc,
		append([]any{guildID}, fp...)...); err != nil {
		return fail(err)
	}
	countWindow := func(q string, start, end int64) (int64, error) {
		return scalarInt(d.sql, q, append([]any{guildID, start, end}, fp...)...)
	}
	joinsSQL := "SELECT COUNT(*) FROM members WHERE guild_id = ? AND join_date >= ? AND join_date < ?" + fc
	leavesSQL := "SELECT COUNT(*) FROM members WHERE guild_id = ? AND " + LeftTSSQL + " >= ? AND " + LeftTSSQL + " < ?" + fc
	returnsSQL := "SELECT COUNT(*) FROM member_returns WHERE guild_id = ? AND returned_at >= ? AND returned_at < ?" + fc
	for _, w := range []struct {
		dst        *int64
		q          string
		start, end int64
	}{
		{&h.Joins30d, joinsSQL, now - 30*dd, now + 1},
		{&h.JoinsPrev30d, joinsSQL, now - 60*dd, now - 30*dd},
		{&h.Leaves30d, leavesSQL, now - 30*dd, now + 1},
		{&h.LeavesPrev30d, leavesSQL, now - 60*dd, now - 30*dd},
		{&h.Returns30d, returnsSQL, now - 30*dd, now + 1},
		{&h.ReturnsPrev30d, returnsSQL, now - 60*dd, now - 30*dd},
	} {
		if *w.dst, err = countWindow(w.q, w.start, w.end); err != nil {
			return fail(err)
		}
	}
	mc, mp := f.clause("m.user_id")
	cohort := func(start, end int64) (*Activation, error) {
		if start < now-retentionDays*dd {
			return nil, nil
		}
		var a Activation
		err := d.sql.QueryRowContext(ctx(), `SELECT COUNT(*),
				COALESCE(SUM(CASE WHEN EXISTS (
					SELECT 1 FROM message_activity ma
					WHERE ma.guild_id = m.guild_id AND ma.user_id = m.user_id
					  AND ma.date >= (m.join_date - m.join_date % ?)
					  AND ma.date <= (m.join_date - m.join_date % ?) + 7 * ?
				) THEN 1 ELSE 0 END), 0)
			FROM members m
			WHERE m.guild_id = ? AND m.join_date >= ? AND m.join_date < ? AND m.join_date >= ?`+mc,
			append([]any{dd, dd, dd, guildID, start, end, added}, mp...)...).Scan(&a.Matured, &a.Activated)
		if err != nil {
			return nil, err
		}
		if a.Matured < activationMinCohort {
			return nil, nil
		}
		a.Rate = float64(a.Activated) / float64(a.Matured) * 100
		return &a, nil
	}
	if h.ActivationCur, err = cohort(now-37*dd, now-7*dd); err != nil {
		return fail(err)
	}
	if h.ActivationPrev, err = cohort(now-67*dd, now-37*dd); err != nil {
		return fail(err)
	}
	return &h
}
