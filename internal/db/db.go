// Package db is the SQLite storage layer.
//
// The schema is the Python bot's, unchanged: the Go bot opens an existing
// lastseen_bot.db as-is, and the Python bot can still open a database the
// Go bot has written. JSON columns are written byte for byte as the Python
// bot wrote them (see pyjson.go), which the compatibility test relies on.
package db

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/cases"
	"modernc.org/sqlite"

	"lastseen/internal/logx"
)

var log = logx.Get("db")

const (
	SecondsPerDay  = 86400
	SecondsPerHour = 3600

	// HealthSnapshotRetentionDays: the Community Pulse trend only reads the
	// newest ~84 days, so 120 keeps it intact while capping table growth.
	HealthSnapshotRetentionDays = 120

	// BotStatsCacheTTL is how long the global /about counts stay cached.
	BotStatsCacheTTL = 300 * time.Second

	vacuumMinFreeRatio = 0.20
	vacuumMinFreeMB    = 16
)

// LeftTSSQL is when a member left, for counting departures in a window.
// left_date survives a rejoin; departures recorded before the column existed
// fall back to last_seen, which the leave handler set to the same moment.
const LeftTSSQL = "COALESCE(left_date, CASE WHEN is_active = 0 THEN last_seen END)"

// SeenSinceSQL: online now, went offline after the cutoff, or posted on/after
// the cutoff's UTC day; a never-seen member counts only while the bot could
// not yet observe them for the whole window. The exact complement of
// GetInactiveMembers. Params: (cutoff, guild added_at, cutoff, cutoff).
const SeenSinceSQL = "(last_seen = 0 OR last_seen > ? " +
	"OR (last_seen IS NULL AND MAX(COALESCE(join_date, 0), ?) > ?) " +
	"OR EXISTS (SELECT 1 FROM member_activity_summary s " +
	"WHERE s.guild_id = members.guild_id AND s.user_id = members.user_id " +
	"AND s.last_active > ? - 86400))"

// Unicode case-insensitive name matching. NOCASE folds ASCII only, so names
// with other characters go through CASEFOLD() (registered below). Each
// fragment takes the casefolded search term twice.
const nonASCIISQL = "GLOB '*[^ -~]*'"

func nameEqualsSQL(col string) string {
	return fmt.Sprintf("(%s = ? COLLATE NOCASE OR (%s %s AND CASEFOLD(%s) = ?))", col, col, nonASCIISQL, col)
}

func nameLikeSQL(col string) string {
	return fmt.Sprintf(`(%s LIKE ? ESCAPE '\' OR (%s %s AND CASEFOLD(%s) LIKE ? ESCAPE '\'))`, col, col, nonASCIISQL, col)
}

var folder = cases.Fold()

// Casefold is full Unicode case folding, for case-insensitive matching.
func Casefold(s string) string { return folder.String(s) }

var registerOnce sync.Once

func registerFunctions() {
	registerOnce.Do(func() {
		err := sqlite.RegisterDeterministicScalarFunction("CASEFOLD", 1,
			func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
				if s, ok := args[0].(string); ok {
					return Casefold(s), nil
				}
				return args[0], nil
			})
		if err != nil {
			panic(err)
		}
	})
}

// nowUTC is the current Unix time; a variable so tests can pin it.
var nowUTC = func() int64 { return time.Now().Unix() }

// todayStart is the current UTC day's midnight.
func todayStart() int64 {
	n := time.Unix(nowUTC(), 0).UTC()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC).Unix()
}

// DayWindowStart is the earliest message_activity.date in a "last days days"
// window: today so far plus the days-1 full days before it.
func DayWindowStart(days int) int64 {
	return todayStart() - int64(days-1)*SecondsPerDay
}

// DB is the database handle. Safe for concurrent use.
type DB struct {
	sql  *sql.DB
	path string

	statsMu      sync.Mutex
	statsCache   *BotStatistics
	statsExpires time.Time
}

// Open opens (creating if needed) the database and runs the schema setup
// and migrations. poolSize caps concurrent connections.
func Open(path string, poolSize int) (*DB, error) {
	registerFunctions()
	q := url.Values{}
	for _, p := range []string{
		"foreign_keys(1)",
		"journal_mode(WAL)",
		"synchronous(NORMAL)",
		"busy_timeout(5000)",
		"journal_size_limit(67108864)",
	} {
		q.Add("_pragma", p)
	}
	// Writes take the write lock up front (BEGIN IMMEDIATE), so a read-then-write
	// transaction never fails to upgrade its lock mid-way.
	q.Set("_txlock", "immediate")
	dsn := "file:" + path + "?" + q.Encode()
	sdb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	sdb.SetMaxOpenConns(poolSize)
	sdb.SetMaxIdleConns(poolSize)
	sdb.SetConnMaxIdleTime(0)
	d := &DB{sql: sdb, path: path}
	if err := sdb.Ping(); err != nil {
		sdb.Close()
		return nil, err
	}
	log.Info("Initialized database connection pool with %d connections", poolSize)
	if err := d.initialize(); err != nil {
		sdb.Close()
		return nil, err
	}
	return d, nil
}

// Close closes all connections.
func (d *DB) Close() error {
	err := d.sql.Close()
	log.Info("Closed all database connections in pool")
	return err
}

// Path is the database file path.
func (d *DB) Path() string { return d.path }

// SQL exposes the handle (tests and tooling).
func (d *DB) SQL() *sql.DB { return d.sql }

func ctx() context.Context { return context.Background() }

// tx runs fn in a write transaction (BEGIN IMMEDIATE), committing on success.
func (d *DB) tx(fn func(tx *sql.Tx) error) error {
	t, err := d.sql.BeginTx(ctx(), nil)
	if err != nil {
		log.Error("Database error: %v", err)
		return err
	}
	if err := fn(t); err != nil {
		_ = t.Rollback()
		log.Error("Database error: %v", err)
		return err
	}
	if err := t.Commit(); err != nil {
		log.Error("Database error: %v", err)
		return err
	}
	return nil
}

// querier is satisfied by *sql.DB, *sql.Tx and *sql.Conn.
type querier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func scalarInt(q querier, query string, args ...any) (int64, error) {
	var v sql.NullInt64
	if err := q.QueryRowContext(ctx(), query, args...).Scan(&v); err != nil {
		return 0, err
	}
	return v.Int64, nil
}

// affected is the number of rows a statement changed; 0 when it failed
// (res is nil then).
func affected(res sql.Result) int64 {
	if res == nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

func columns(q querier, table string) (map[string]bool, error) {
	rows, err := q.QueryContext(ctx(), "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		cols[name] = true
	}
	return cols, rows.Err()
}

var schema = []string{
	`CREATE TABLE IF NOT EXISTS guilds (
		guild_id INTEGER PRIMARY KEY,
		guild_name TEXT NOT NULL,
		notification_channel_id INTEGER,
		inactive_days INTEGER DEFAULT 10,
		bot_admin_role_name TEXT DEFAULT 'LastSeen Admin',
		user_role_required INTEGER DEFAULT 0,
		user_role_name TEXT DEFAULT 'LastSeen User',
		added_at INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS members (
		guild_id INTEGER NOT NULL,
		user_id INTEGER NOT NULL,
		username TEXT NOT NULL,
		nickname TEXT,
		join_date INTEGER,
		last_seen INTEGER,
		is_active INTEGER DEFAULT 1,
		roles TEXT,
		PRIMARY KEY (guild_id, user_id),
		FOREIGN KEY (guild_id) REFERENCES guilds(guild_id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS role_changes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		guild_id INTEGER NOT NULL,
		user_id INTEGER NOT NULL,
		role_name TEXT NOT NULL,
		action TEXT NOT NULL,
		timestamp INTEGER NOT NULL,
		FOREIGN KEY (guild_id) REFERENCES guilds(guild_id) ON DELETE CASCADE,
		FOREIGN KEY (guild_id, user_id) REFERENCES members(guild_id, user_id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS message_activity (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		guild_id INTEGER NOT NULL,
		user_id INTEGER NOT NULL,
		date INTEGER NOT NULL,
		message_count INTEGER DEFAULT 1,
		UNIQUE(guild_id, user_id, date),
		FOREIGN KEY (guild_id) REFERENCES guilds(guild_id) ON DELETE CASCADE,
		FOREIGN KEY (guild_id, user_id) REFERENCES members(guild_id, user_id) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS idx_members_username ON members(guild_id, username COLLATE NOCASE)`,
	`CREATE INDEX IF NOT EXISTS idx_members_nickname ON members(guild_id, nickname COLLATE NOCASE)`,
	`CREATE INDEX IF NOT EXISTS idx_members_active ON members(guild_id, is_active, last_seen)`,
	`CREATE INDEX IF NOT EXISTS idx_members_join_date ON members(guild_id, join_date)`,
	`CREATE INDEX IF NOT EXISTS idx_role_changes_guild_user ON role_changes(guild_id, user_id, timestamp DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_message_activity_user_date ON message_activity(guild_id, user_id, date DESC)`,
}

// guildColumnMigrations are columns added to guilds after the first release.
var guildColumnMigrations = []struct{ name, ddl string }{
	{"bot_admin_role_name", "TEXT DEFAULT 'LastSeen Admin'"},
	{"user_role_required", "INTEGER DEFAULT 0"},
	{"user_role_name", "TEXT DEFAULT 'LastSeen User'"},
	{"track_only_roles", "TEXT"},
	{"allowed_channels", "TEXT"},
	{"positions_initialized", "INTEGER DEFAULT 0"},
	{"message_retention_days", "INTEGER DEFAULT 365"},
	{"timezone", "TEXT DEFAULT 'UTC'"},
	{"report_channel_id", "INTEGER"},
	{"report_frequency", "TEXT"},
	{"report_types", "TEXT"},
	{"report_day_weekly", "INTEGER DEFAULT 0"},
	{"report_day_monthly", "INTEGER DEFAULT 1"},
	{"last_weekly_report", "INTEGER DEFAULT 0"},
	{"last_monthly_report", "INTEGER DEFAULT 0"},
	{"report_time_hour", "INTEGER DEFAULT 9"},
	{"language", "TEXT DEFAULT 'en'"},
}

var laterSchema = []string{
	`CREATE TABLE IF NOT EXISTS message_activity_hourly (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		guild_id INTEGER NOT NULL,
		user_id INTEGER NOT NULL,
		timestamp INTEGER NOT NULL,
		hour INTEGER NOT NULL,
		message_count INTEGER DEFAULT 1,
		UNIQUE(guild_id, user_id, timestamp),
		FOREIGN KEY (guild_id) REFERENCES guilds(guild_id) ON DELETE CASCADE,
		FOREIGN KEY (guild_id, user_id) REFERENCES members(guild_id, user_id) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS idx_message_activity_hourly_guild_time ON message_activity_hourly(guild_id, timestamp DESC)`,
	`CREATE INDEX IF NOT EXISTS idx_message_activity_hourly_user ON message_activity_hourly(guild_id, user_id, timestamp DESC)`,
	`CREATE TABLE IF NOT EXISTS opted_out_users (
		user_id INTEGER PRIMARY KEY,
		opted_out_at INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS no_watch_users (
		user_id INTEGER PRIMARY KEY,
		opted_out_at INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS bot_state (
		key TEXT PRIMARY KEY,
		value INTEGER
	)`,
	`CREATE TABLE IF NOT EXISTS watchlists (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		seq INTEGER,
		guild_id INTEGER NOT NULL,
		target_type TEXT NOT NULL,
		target_id INTEGER NOT NULL,
		alert_type TEXT NOT NULL,
		threshold_seconds INTEGER,
		channel_id INTEGER,
		deliver_dm INTEGER DEFAULT 0,
		state TEXT DEFAULT 'armed',
		fired_targets TEXT,
		failed_deliveries INTEGER DEFAULT 0,
		created_by INTEGER NOT NULL,
		created_at INTEGER NOT NULL,
		UNIQUE(guild_id, target_type, target_id, alert_type),
		FOREIGN KEY (guild_id) REFERENCES guilds(guild_id) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS idx_watchlists_guild ON watchlists(guild_id)`,
	`CREATE TABLE IF NOT EXISTS member_returns (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		guild_id INTEGER NOT NULL,
		user_id INTEGER NOT NULL,
		away_seconds INTEGER NOT NULL,
		returned_at INTEGER NOT NULL,
		FOREIGN KEY (guild_id) REFERENCES guilds(guild_id) ON DELETE CASCADE,
		FOREIGN KEY (guild_id, user_id) REFERENCES members(guild_id, user_id) ON DELETE CASCADE
	)`,
	`CREATE INDEX IF NOT EXISTS idx_member_returns_guild_time ON member_returns(guild_id, returned_at DESC)`,
	`CREATE TABLE IF NOT EXISTS health_snapshots (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		guild_id INTEGER NOT NULL,
		date INTEGER NOT NULL,
		active_members INTEGER NOT NULL,
		posters_7d INTEGER NOT NULL,
		messages_7d INTEGER NOT NULL,
		joins_7d INTEGER NOT NULL,
		leaves_7d INTEGER NOT NULL,
		returns_7d INTEGER NOT NULL,
		UNIQUE(guild_id, date),
		FOREIGN KEY (guild_id) REFERENCES guilds(guild_id) ON DELETE CASCADE
	)`,
	`CREATE TABLE IF NOT EXISTS member_activity_summary (
		guild_id INTEGER NOT NULL,
		user_id INTEGER NOT NULL,
		first_active INTEGER,
		last_active INTEGER,
		total_messages INTEGER NOT NULL DEFAULT 0,
		active_days INTEGER NOT NULL DEFAULT 0,
		current_streak INTEGER NOT NULL DEFAULT 0,
		longest_streak INTEGER NOT NULL DEFAULT 0,
		longest_gap INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (guild_id, user_id),
		FOREIGN KEY (guild_id) REFERENCES guilds(guild_id) ON DELETE CASCADE,
		FOREIGN KEY (guild_id, user_id) REFERENCES members(guild_id, user_id) ON DELETE CASCADE
	)`,
}

// initialize creates the tables and runs the migrations, in the same order
// as the Python bot, so either bot can open a database the other created.
func (d *DB) initialize() error {
	// One connection for the whole setup: PRAGMA user_version and the DDL must
	// see each other's effects.
	conn, err := d.sql.Conn(ctx())
	if err != nil {
		return err
	}
	defer conn.Close()
	exec := func(q string, args ...any) error {
		_, err := conn.ExecContext(ctx(), q, args...)
		if err != nil {
			return fmt.Errorf("%w (in %.60s)", err, strings.TrimSpace(q))
		}
		return nil
	}
	for _, s := range schema {
		if err := exec(s); err != nil {
			return err
		}
	}
	cols, err := columns(conn, "guilds")
	if err != nil {
		return err
	}
	for _, m := range guildColumnMigrations {
		if !cols[m.name] {
			if err := exec("ALTER TABLE guilds ADD COLUMN " + m.name + " " + m.ddl); err != nil {
				return err
			}
			log.Info("Added %s column to guilds table", m.name)
		}
	}
	if err := exec(`CREATE INDEX IF NOT EXISTS idx_guilds_reports
		ON guilds(report_frequency, report_channel_id)
		WHERE report_frequency IS NOT NULL AND report_channel_id IS NOT NULL`); err != nil {
		return err
	}
	mcols, err := columns(conn, "members")
	if err != nil {
		return err
	}
	for _, m := range []struct{ name, ddl string }{
		{"join_position", "INTEGER"}, {"nickname_history", "TEXT"}, {"left_date", "INTEGER"},
	} {
		if !mcols[m.name] {
			if err := exec("ALTER TABLE members ADD COLUMN " + m.name + " " + m.ddl); err != nil {
				return err
			}
			log.Info("Added %s column to members table", m.name)
		}
	}
	if err := exec(`CREATE INDEX IF NOT EXISTS idx_members_left_date
		ON members(guild_id, left_date) WHERE left_date IS NOT NULL`); err != nil {
		return err
	}
	for _, s := range laterSchema {
		if err := exec(s); err != nil {
			return err
		}
	}

	// One-time sweep of legacy orphaned activity rows (PRAGMA user_version 1).
	uv, err := scalarInt(conn, "PRAGMA user_version")
	if err != nil {
		return err
	}
	if uv < 1 {
		r1, err := conn.ExecContext(ctx(), `DELETE FROM message_activity WHERE NOT EXISTS (
			SELECT 1 FROM members m WHERE m.guild_id = message_activity.guild_id AND m.user_id = message_activity.user_id)`)
		if err != nil {
			return err
		}
		r2, err := conn.ExecContext(ctx(), `DELETE FROM message_activity_hourly WHERE NOT EXISTS (
			SELECT 1 FROM members m WHERE m.guild_id = message_activity_hourly.guild_id AND m.user_id = message_activity_hourly.user_id)`)
		if err != nil {
			return err
		}
		if a, b := affected(r1), affected(r2); a > 0 || b > 0 {
			log.Info("Cleaned up orphaned activity rows: %d daily, %d hourly", a, b)
		}
		if err := exec("PRAGMA user_version = 1"); err != nil {
			return err
		}
	}

	if err := d.backfillSummary(conn); err != nil {
		return err
	}

	wcols, err := columns(conn, "watchlists")
	if err != nil {
		return err
	}
	if !wcols["seq"] {
		if err := exec("ALTER TABLE watchlists ADD COLUMN seq INTEGER"); err != nil {
			return err
		}
		rows, err := conn.QueryContext(ctx(), "SELECT id, guild_id FROM watchlists ORDER BY guild_id, id")
		if err != nil {
			return err
		}
		type wr struct{ id, gid int64 }
		var all []wr
		for rows.Next() {
			var r wr
			if err := rows.Scan(&r.id, &r.gid); err != nil {
				rows.Close()
				return err
			}
			all = append(all, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil { // valid after Close
			return err
		}
		counters := map[int64]int64{}
		for _, r := range all {
			counters[r.gid]++
			if err := exec("UPDATE watchlists SET seq = ? WHERE id = ?", counters[r.gid], r.id); err != nil {
				return err
			}
		}
		log.Info("Backfilled per-guild seq numbers for existing watches")
	}
	if !wcols["deliver_dm"] {
		if err := exec("ALTER TABLE watchlists ADD COLUMN deliver_dm INTEGER DEFAULT 0"); err != nil {
			return err
		}
		log.Info("Added deliver_dm column to watchlists table")
	}
	if !wcols["failed_deliveries"] {
		if err := exec("ALTER TABLE watchlists ADD COLUMN failed_deliveries INTEGER DEFAULT 0"); err != nil {
			return err
		}
		log.Info("Added failed_deliveries column to watchlists table")
	}
	log.Info("Database initialized: %s", d.path)
	return nil
}

// backfillSummary seeds member_activity_summary once (when it is empty) from
// the retained message_activity rows.
func (d *DB) backfillSummary(conn *sql.Conn) error {
	n, err := scalarInt(conn, "SELECT COUNT(*) FROM member_activity_summary")
	if err != nil || n != 0 {
		return err
	}
	rows, err := conn.QueryContext(ctx(), `
		SELECT ma.guild_id, ma.user_id, ma.date, ma.message_count
		FROM message_activity ma
		JOIN members m ON m.guild_id = ma.guild_id AND m.user_id = ma.user_id
		JOIN guilds g ON g.guild_id = ma.guild_id
		ORDER BY ma.guild_id, ma.user_id, ma.date`)
	if err != nil {
		return err
	}
	type key struct{ g, u int64 }
	type sum struct{ first, last, total, days, cur, best, gap int64 }
	summaries := map[key]*sum{}
	var order []key
	for rows.Next() {
		var gid, uid, date int64
		var mc sql.NullInt64
		if err := rows.Scan(&gid, &uid, &date, &mc); err != nil {
			rows.Close()
			return err
		}
		k := key{gid, uid}
		s := summaries[k]
		if s == nil {
			summaries[k] = &sum{first: date, last: date, total: mc.Int64, days: 1, cur: 1, best: 1}
			order = append(order, k)
			continue
		}
		s.total += mc.Int64
		gap := floorDiv(date-s.last, SecondsPerDay) - 1
		if gap <= 0 {
			s.cur++
		} else {
			if gap > s.gap {
				s.gap = gap
			}
			s.cur = 1
		}
		if s.cur > s.best {
			s.best = s.cur
		}
		s.days++
		s.last = date
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(order) == 0 {
		return nil
	}
	t, err := conn.BeginTx(ctx(), nil)
	if err != nil {
		return err
	}
	for _, k := range order {
		s := summaries[k]
		if _, err := t.ExecContext(ctx(), `INSERT OR IGNORE INTO member_activity_summary
			(guild_id, user_id, first_active, last_active, total_messages, active_days,
			 current_streak, longest_streak, longest_gap)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			k.g, k.u, s.first, s.last, s.total, s.days, s.cur, s.best, s.gap); err != nil {
			_ = t.Rollback()
			return err
		}
	}
	if err := t.Commit(); err != nil {
		return err
	}
	log.Info("Backfilled member_activity_summary for %d member(s)", len(order))
	return nil
}

// floorDiv divides and rounds down (towards minus infinity); Go's / rounds
// towards zero.
func floorDiv(a, b int64) int64 {
	q := a / b
	if (a%b != 0) && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

// WALSizeMB is the current size of the -wal file in MB.
func (d *DB) WALSizeMB() float64 {
	st, err := os.Stat(d.path + "-wal")
	if err != nil {
		return 0
	}
	return float64(st.Size()) / (1024 * 1024)
}

// CheckpointWAL runs PRAGMA wal_checkpoint(mode) and reports whether it
// completed without being blocked.
func (d *DB) CheckpointWAL(mode string) bool {
	switch mode {
	case "PASSIVE", "FULL", "RESTART", "TRUNCATE":
	default:
		log.Error("WAL checkpoint failed: Invalid checkpoint mode: %s", mode)
		return false
	}
	var busy, logFrames, checkpointed int64
	err := d.sql.QueryRowContext(ctx(), "PRAGMA wal_checkpoint("+mode+")").Scan(&busy, &logFrames, &checkpointed)
	if err != nil {
		log.Error("WAL checkpoint failed: %v", err)
		return false
	}
	if busy != 0 {
		msg := fmt.Sprintf("WAL %s checkpoint blocked (checkpointed %d/%d frames, wal file %.1f MB); will retry next cycle",
			mode, checkpointed, logFrames, d.WALSizeMB())
		if mode != "PASSIVE" {
			log.Warn("%s", msg)
		} else {
			log.Debug("%s", msg)
		}
		return false
	}
	log.Debug("WAL %s checkpoint ok (%d/%d frames, wal file %.1f MB)", mode, checkpointed, logFrames, d.WALSizeMB())
	return true
}

// ErrNotFound is returned by lookups that found no row.
var ErrNotFound = errors.New("not found")

func nullInt(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullStr(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func ptrInt(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	x := v.Int64
	return &x
}

func ptrStr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	x := v.String
	return &x
}
