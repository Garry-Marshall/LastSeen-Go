package db

import (
	"database/sql"
	"errors"
	"os"
	"strings"
)

// Member is a members row.
type Member struct {
	GuildID         int64
	UserID          int64
	Username        string
	Nickname        *string
	JoinDate        *int64
	LastSeen        *int64 // nil = never seen, 0 = online now, else went offline at
	IsActive        bool
	Roles           []string
	JoinPosition    *int64
	NicknameHistory *string // JSON list
	LeftDate        *int64
}

// DisplayName is the nickname if set, else the username.
func (m *Member) DisplayName() string {
	if m.Nickname != nil && *m.Nickname != "" {
		return *m.Nickname
	}
	return m.Username
}

const memberCols = "guild_id, user_id, username, nickname, join_date, last_seen, is_active, roles, join_position, nickname_history, left_date"

func prefixed(alias, cols string) string {
	parts := strings.Split(cols, ", ")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ", ")
}

func scanMember(sc interface{ Scan(...any) error }) (*Member, error) {
	var m Member
	var nick, roles, hist sql.NullString
	var join, last, pos, left, active sql.NullInt64
	if err := sc.Scan(&m.GuildID, &m.UserID, &m.Username, &nick, &join, &last, &active, &roles, &pos, &hist, &left); err != nil {
		return nil, err
	}
	m.Nickname = ptrStr(nick)
	m.JoinDate = ptrInt(join)
	m.LastSeen = ptrInt(last)
	m.IsActive = !active.Valid || active.Int64 != 0
	m.JoinPosition = ptrInt(pos)
	m.NicknameHistory = ptrStr(hist)
	m.LeftDate = ptrInt(left)
	if roles.Valid && roles.String != "" {
		m.Roles, _ = LoadStrings(roles.String)
	}
	if m.Roles == nil {
		m.Roles = []string{}
	}
	return &m, nil
}

func queryMembers(q querier, query string, args ...any) ([]*Member, error) {
	rows, err := q.QueryContext(ctx(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Member
	for rows.Next() {
		m, err := scanMember(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetMemberJoinPosition sets a member's join position.
func (d *DB) SetMemberJoinPosition(guildID, userID, position int64) bool {
	var n int64
	err := d.tx(func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx(), "UPDATE members SET join_position = ? WHERE guild_id = ? AND user_id = ?", position, guildID, userID)
		n = affected(res)
		return err
	})
	if err != nil {
		log.Error("Failed to set join position for user %d in guild %d: %v", userID, guildID, err)
		return false
	}
	return n > 0
}

// SetMemberJoinPositions writes many join positions in one transaction.
func (d *DB) SetMemberJoinPositions(guildID int64, positions map[int64]int64) bool {
	err := d.tx(func(tx *sql.Tx) error {
		st, err := tx.PrepareContext(ctx(), "UPDATE members SET join_position = ? WHERE guild_id = ? AND user_id = ?")
		if err != nil {
			return err
		}
		defer st.Close()
		for uid, pos := range positions {
			if _, err := st.ExecContext(ctx(), pos, guildID, uid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Error("Failed to set join positions in guild %d: %v", guildID, err)
		return false
	}
	return true
}

// CalculateJoinPosition is 1 + the number of members who joined earlier.
func (d *DB) CalculateJoinPosition(guildID, joinTS int64) (int64, bool) {
	n, err := scalarInt(d.sql, "SELECT COUNT(*) FROM members WHERE guild_id = ? AND join_date < ?", guildID, joinTS)
	if err != nil {
		log.Error("Failed to calculate join position for guild %d at timestamp %d: %v", guildID, joinTS, err)
		return 0, false
	}
	return n + 1, true
}

const addMemberSQL = `INSERT INTO members
	(guild_id, user_id, username, nickname, join_date, last_seen, is_active, roles, nickname_history)
	VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)
	ON CONFLICT(guild_id, user_id) DO UPDATE SET
		username = excluded.username,
		nickname = excluded.nickname,
		roles = excluded.roles`

func nickHistoryInit(nick *string) any {
	if nick == nil || *nick == "" {
		return nil
	}
	return DumpStrings([]string{*nick})
}

// AddMember upserts a member. On conflict only the profile fields (username,
// nickname, roles) change, so history columns survive (no REPLACE cascade).
func (d *DB) AddMember(guildID, userID int64, username string, nickname *string, joinDate int64, roles []string) bool {
	err := d.tx(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx(), addMemberSQL, guildID, userID, username, nullStr(nickname),
			joinDate, nil, DumpStrings(roles), nickHistoryInit(nickname))
		return err
	})
	if err != nil {
		log.Error("Failed to add member %d to guild %d: %v", userID, guildID, err)
		return false
	}
	return true
}

func (d *DB) execRows(what string, query string, args ...any) bool {
	var n int64
	err := d.tx(func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx(), query, args...)
		n = affected(res)
		return err
	})
	if err != nil {
		log.Error("Failed to %s: %v", what, err)
		return false
	}
	return n > 0
}

// UpdateMemberUsername sets a member's username.
func (d *DB) UpdateMemberUsername(guildID, userID int64, username string) bool {
	return d.execRows(sprintf("update username for %d in guild %d", userID, guildID),
		"UPDATE members SET username = ? WHERE guild_id = ? AND user_id = ?", username, guildID, userID)
}

// UpdateMemberNickname sets a member's nickname.
func (d *DB) UpdateMemberNickname(guildID, userID int64, nickname *string) bool {
	return d.execRows(sprintf("update nickname for %d in guild %d", userID, guildID),
		"UPDATE members SET nickname = ? WHERE guild_id = ? AND user_id = ?", nullStr(nickname), guildID, userID)
}

// UpdateMemberRoles sets a member's role names.
func (d *DB) UpdateMemberRoles(guildID, userID int64, roles []string) bool {
	return d.execRows(sprintf("update roles for %d in guild %d", userID, guildID),
		"UPDATE members SET roles = ? WHERE guild_id = ? AND user_id = ?", DumpStrings(roles), guildID, userID)
}

const maxNicknameHistory = 10

// UpdateNicknameHistory appends old/new nicknames (unique, last 10 kept).
func (d *DB) UpdateNicknameHistory(guildID, userID int64, oldNick, newNick *string) bool {
	if newNick == nil || *newNick == "" || (oldNick != nil && *oldNick == *newNick) {
		return true // nothing to record
	}
	found := true
	err := d.tx(func(tx *sql.Tx) error {
		var raw sql.NullString
		err := tx.QueryRowContext(ctx(), "SELECT nickname_history FROM members WHERE guild_id = ? AND user_id = ?", guildID, userID).Scan(&raw)
		if errors.Is(err, sql.ErrNoRows) {
			found = false
			return nil
		}
		if err != nil {
			return err
		}
		var history []string
		if raw.Valid && raw.String != "" {
			h, ok := LoadStrings(raw.String)
			if !ok {
				log.Warn("Invalid nickname_history JSON for user %d, resetting to empty list", userID)
			}
			history = h
		}
		contains := func(s string) bool {
			for _, h := range history {
				if h == s {
					return true
				}
			}
			return false
		}
		if oldNick != nil && *oldNick != "" && !contains(*oldNick) {
			history = append(history, *oldNick)
		}
		if !contains(*newNick) {
			history = append(history, *newNick)
		}
		if len(history) > maxNicknameHistory {
			history = history[len(history)-maxNicknameHistory:]
		}
		if history == nil {
			history = []string{}
		}
		_, err = tx.ExecContext(ctx(), "UPDATE members SET nickname_history = ? WHERE guild_id = ? AND user_id = ?",
			DumpStrings(history), guildID, userID)
		return err
	})
	if err != nil {
		log.Error("Failed to update nickname history for %d in guild %d: %v", userID, guildID, err)
		return false
	}
	return found
}

// UpdateLastSeen sets members.last_seen.
func (d *DB) UpdateLastSeen(guildID, userID, ts int64) bool {
	return d.execRows(sprintf("update last_seen for %d in guild %d", userID, guildID),
		"UPDATE members SET last_seen = ? WHERE guild_id = ? AND user_id = ?", ts, guildID, userID)
}

// PresenceEvent is one queued presence change.
type PresenceEvent struct {
	GuildID, UserID int64
	WentOffline     bool
	TS              int64
}

// PresenceResult is the outcome of one PresenceEvent: Found is false when
// the member has no row; for an online event PrevLastSeen is last_seen before
// it was set to 0 (a timestamp, 0 = already online, nil = never seen).
type PresenceResult struct {
	Found        bool
	PrevLastSeen *int64
}

// ApplyPresenceEvents applies a run of presence events in order, in one
// write transaction. Returns an error when the transaction fails.
func (d *DB) ApplyPresenceEvents(events []PresenceEvent) ([]PresenceResult, error) {
	results := make([]PresenceResult, 0, len(events))
	err := d.tx(func(tx *sql.Tx) error {
		results = results[:0]
		for _, e := range events {
			if e.WentOffline {
				res, err := tx.ExecContext(ctx(), "UPDATE members SET last_seen = ? WHERE guild_id = ? AND user_id = ?", e.TS, e.GuildID, e.UserID)
				if err != nil {
					return err
				}
				results = append(results, PresenceResult{Found: affected(res) > 0})
				continue
			}
			var prev sql.NullInt64
			err := tx.QueryRowContext(ctx(), "SELECT last_seen FROM members WHERE guild_id = ? AND user_id = ?", e.GuildID, e.UserID).Scan(&prev)
			if errors.Is(err, sql.ErrNoRows) {
				results = append(results, PresenceResult{})
				continue
			}
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx(), "UPDATE members SET last_seen = 0 WHERE guild_id = ? AND user_id = ?", e.GuildID, e.UserID); err != nil {
				return err
			}
			results = append(results, PresenceResult{Found: true, PrevLastSeen: ptrInt(prev)})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// SetMemberInactive marks a member as departed.
func (d *DB) SetMemberInactive(guildID, userID int64) bool {
	return d.execRows(sprintf("set member %d inactive in guild %d", userID, guildID),
		"UPDATE members SET is_active = 0 WHERE guild_id = ? AND user_id = ?", guildID, userID)
}

// SetMemberLeftDate sets (or clears with nil) the departure time.
func (d *DB) SetMemberLeftDate(guildID, userID int64, ts *int64) bool {
	return d.execRows(sprintf("set left_date for %d in guild %d", userID, guildID),
		"UPDATE members SET left_date = ? WHERE guild_id = ? AND user_id = ?", nullInt(ts), guildID, userID)
}

// RecordDeparture stores a member leaving: inactive, left_date and last_seen
// set to ts, in one transaction.
func (d *DB) RecordDeparture(guildID, userID, ts int64) bool {
	return d.execRows(sprintf("record departure of %d in guild %d", userID, guildID),
		"UPDATE members SET is_active = 0, left_date = ?, last_seen = ? WHERE guild_id = ? AND user_id = ?",
		ts, ts, guildID, userID)
}

// GetMember returns one member, or nil.
func (d *DB) GetMember(guildID, userID int64) *Member {
	m, err := scanMember(d.sql.QueryRowContext(ctx(), "SELECT "+memberCols+" FROM members WHERE guild_id = ? AND user_id = ?", guildID, userID))
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Error("Failed to get member %d from guild %d: %v", userID, guildID, err)
		}
		return nil
	}
	return m
}

// AddMemberReturn records a member coming back after a long absence.
func (d *DB) AddMemberReturn(guildID, userID, awaySeconds, returnedAt int64) bool {
	err := d.tx(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx(), `INSERT INTO member_returns (guild_id, user_id, away_seconds, returned_at)
			VALUES (?, ?, ?, ?)`, guildID, userID, awaySeconds, returnedAt)
		return err
	})
	if err != nil {
		log.Error("Failed to record member return for %d in guild %d: %v", userID, guildID, err)
		return false
	}
	return true
}

// CountReturns counts returns recorded since ts.
func (d *DB) CountReturns(guildID, since int64) int64 {
	n, err := scalarInt(d.sql, "SELECT COUNT(*) FROM member_returns WHERE guild_id = ? AND returned_at >= ?", guildID, since)
	if err != nil {
		log.Error("Failed to count returns for guild %d: %v", guildID, err)
		return 0
	}
	return n
}

// MemberReturn is a recent returning member.
type MemberReturn struct {
	UserID      int64
	AwaySeconds int64
	ReturnedAt  int64
	Username    string
	Nickname    *string
}

// GetRecentReturns lists recent returns (newest first) for members still stored.
func (d *DB) GetRecentReturns(guildID, since int64, limit int) []MemberReturn {
	rows, err := d.sql.QueryContext(ctx(), `
		SELECT r.user_id, r.away_seconds, r.returned_at, m.username, m.nickname
		FROM member_returns r
		JOIN members m ON m.guild_id = r.guild_id AND m.user_id = r.user_id
		WHERE r.guild_id = ? AND r.returned_at >= ?
		ORDER BY r.returned_at DESC
		LIMIT ?`, guildID, since, limit)
	if err != nil {
		log.Error("Failed to get recent returns for guild %d: %v", guildID, err)
		return nil
	}
	defer rows.Close()
	var out []MemberReturn
	for rows.Next() {
		var r MemberReturn
		var nick sql.NullString
		if err := rows.Scan(&r.UserID, &r.AwaySeconds, &r.ReturnedAt, &r.Username, &nick); err != nil {
			log.Error("Failed to get recent returns for guild %d: %v", guildID, err)
			return nil
		}
		r.Nickname = ptrStr(nick)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		log.Error("Failed to get recent returns for guild %d: %v", guildID, err)
		return nil
	}
	return out
}

// FindMemberByName finds a member by user id, username or nickname
// (case-insensitive, Unicode-aware). Preference: id, then username, then
// nickname; ties go to current members, then online, then most recently seen.
func (d *DB) FindMemberByName(guildID int64, search string) *Member {
	term := Casefold(search)
	ue, ne := nameEqualsSQL("username"), nameEqualsSQL("nickname")
	m, err := scanMember(d.sql.QueryRowContext(ctx(), `
		SELECT `+memberCols+` FROM members
		WHERE guild_id = ? AND (CAST(user_id AS TEXT) = ? OR `+ue+` OR `+ne+`)
		ORDER BY
			CASE WHEN CAST(user_id AS TEXT) = ? THEN 0 WHEN `+ue+` THEN 1 ELSE 2 END,
			is_active DESC,
			CASE WHEN last_seen = 0 THEN 1 ELSE 0 END DESC,
			last_seen DESC
		LIMIT 1`,
		guildID, search, term, term, term, term, search, term, term))
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Error("Failed to find member '%s' in guild %d: %v", search, guildID, err)
		}
		return nil
	}
	return m
}

// NamePair is an autocomplete suggestion.
type NamePair struct {
	Username string
	Nickname *string
}

// SearchMembersByName is the lightweight substring search for autocomplete.
func (d *DB) SearchMembersByName(guildID int64, query string, limit int) []NamePair {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(Casefold(query))
	like := "%" + escaped + "%"
	rows, err := d.sql.QueryContext(ctx(), `
		SELECT username, nickname FROM members
		WHERE guild_id = ? AND is_active = 1 AND (`+nameLikeSQL("username")+` OR `+nameLikeSQL("nickname")+`)
		LIMIT ?`, guildID, like, like, like, like, limit)
	if err != nil {
		log.Error("Failed to search members '%s' in guild %d: %v", query, guildID, err)
		return nil
	}
	defer rows.Close()
	var out []NamePair
	for rows.Next() {
		var p NamePair
		var nick sql.NullString
		if err := rows.Scan(&p.Username, &nick); err != nil {
			log.Error("Failed to search members '%s' in guild %d: %v", query, guildID, err)
			return nil
		}
		p.Nickname = ptrStr(nick)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		log.Error("Failed to search members '%s' in guild %d: %v", query, guildID, err)
		return nil
	}
	return out
}

// GetInactiveMembers lists members inactive for more than inactiveDays
// (never-seen members count once the threshold has passed since the bot
// could first observe them; anyone who posted within it is excluded).
func (d *DB) GetInactiveMembers(guildID int64, inactiveDays int) []*Member {
	threshold := nowUTC() - int64(inactiveDays)*SecondsPerDay
	ms, err := queryMembers(d.sql, `
		SELECT `+prefixed("m", memberCols)+` FROM members m
		JOIN guilds g ON g.guild_id = m.guild_id
		WHERE m.guild_id = ?
		AND m.is_active = 1
		AND (
			(m.last_seen IS NOT NULL AND m.last_seen != 0 AND m.last_seen <= ?)
			OR (m.last_seen IS NULL AND MAX(COALESCE(m.join_date, 0), g.added_at) <= ?)
		)
		AND NOT EXISTS (
			SELECT 1 FROM member_activity_summary s
			WHERE s.guild_id = m.guild_id AND s.user_id = m.user_id
			AND s.last_active > ? - 86400
		)
		ORDER BY m.last_seen ASC`, guildID, threshold, threshold, threshold)
	if err != nil {
		log.Error("Failed to get inactive members for guild %d: %v", guildID, err)
		return nil
	}
	return ms
}

// GetGuildMembers lists members (current only unless includeLeft), most
// recently seen first.
func (d *DB) GetGuildMembers(guildID int64, includeLeft bool) []*Member {
	q := "SELECT " + memberCols + " FROM members WHERE guild_id = ? AND is_active = 1 ORDER BY last_seen DESC"
	if includeLeft {
		q = "SELECT " + memberCols + " FROM members WHERE guild_id = ? ORDER BY last_seen DESC"
	}
	ms, err := queryMembers(d.sql, q, guildID)
	if err != nil {
		log.Error("Failed to get guild members for guild %d: %v", guildID, err)
		return nil
	}
	return ms
}

// GetAllGuildMembers lists every stored member of a guild.
func (d *DB) GetAllGuildMembers(guildID int64) []*Member {
	ms, err := queryMembers(d.sql, "SELECT "+memberCols+" FROM members WHERE guild_id = ?", guildID)
	if err != nil {
		log.Error("Failed to get all members for guild %d: %v", guildID, err)
		return nil
	}
	return ms
}

// CountGuildMembers counts every stored member (current and departed).
func (d *DB) CountGuildMembers(guildID int64) int64 {
	n, err := scalarInt(d.sql, "SELECT COUNT(*) FROM members WHERE guild_id = ?", guildID)
	if err != nil {
		log.Error("Failed to get all members for guild %d: %v", guildID, err)
	}
	return n
}

// UserFilter restricts a query to some user ids. A nil *UserFilter (or one
// with All set) means no restriction; an empty ID list matches nobody.
type UserFilter struct {
	IDs []int64
}

// clause returns " AND <col> IN (...)" for the filter. The ids travel as one
// JSON parameter, so huge role filters never hit SQLite's variable limit.
func (f *UserFilter) clause(col string) (string, []any) {
	if f == nil {
		return "", nil
	}
	if len(f.IDs) == 0 {
		return " AND 1=0", nil
	}
	return " AND " + col + " IN (SELECT value FROM json_each(?))", []any{DumpInts(f.IDs)}
}

// GetTrackedUserIDs returns the members matching the guild's track_only_roles,
// or nil when no filter is configured (everyone is in scope).
func (d *DB) GetTrackedUserIDs(guildID int64) *UserFilter {
	cfg := d.GetGuildConfig(guildID)
	if cfg == nil || cfg.TrackOnlyRoles == nil || *cfg.TrackOnlyRoles == "" {
		return nil
	}
	allowedList, ok := LoadStrings(*cfg.TrackOnlyRoles)
	if !ok || len(allowedList) == 0 {
		return nil
	}
	allowed := map[string]bool{}
	for _, r := range allowedList {
		allowed[r] = true
	}
	rows, err := d.sql.QueryContext(ctx(), "SELECT user_id, roles FROM members WHERE guild_id = ?", guildID)
	if err != nil {
		log.Error("Failed to get tracked user ids for guild %d: %v", guildID, err)
		return nil
	}
	defer rows.Close()
	f := &UserFilter{IDs: []int64{}}
	for rows.Next() {
		var uid int64
		var roles sql.NullString
		if err := rows.Scan(&uid, &roles); err != nil {
			log.Error("Failed to get tracked user ids for guild %d: %v", guildID, err)
			return nil
		}
		if !roles.Valid || roles.String == "" {
			continue
		}
		rl, ok := LoadStrings(roles.String)
		if !ok {
			continue
		}
		for _, r := range rl {
			if allowed[r] {
				f.IDs = append(f.IDs, uid)
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		log.Error("Failed to get tracked user ids for guild %d: %v", guildID, err)
		return nil
	}
	return f
}

// MemberExists reports whether a member row exists (false on error).
func (d *DB) MemberExists(guildID, userID int64) bool {
	var one int
	err := d.sql.QueryRowContext(ctx(), "SELECT 1 FROM members WHERE guild_id = ? AND user_id = ?", guildID, userID).Scan(&one)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		log.Error("Failed to check if member %d exists in guild %d: %v", userID, guildID, err)
	}
	return err == nil
}

// MemberIsMissing reports that a member is confirmed absent (false on error).
func (d *DB) MemberIsMissing(guildID, userID int64) bool {
	var one int
	err := d.sql.QueryRowContext(ctx(), "SELECT 1 FROM members WHERE guild_id = ? AND user_id = ?", guildID, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return true
	}
	if err != nil {
		log.Error("Failed to check if member %d is missing in guild %d: %v", userID, guildID, err)
	}
	return false
}

// ---- Reconciliation ----

// ReconcileBatchSize is the number of rows written per transaction.
const ReconcileBatchSize = 500

const rejoinSQL = `UPDATE members SET is_active = 1, username = ?, nickname = ?,
	join_date = ?, roles = ?,
	last_seen = CASE WHEN ? THEN 0 ELSE last_seen END
	WHERE guild_id = ? AND user_id = ?`

const joinPositionSQL = `UPDATE members SET join_position = (
	SELECT COUNT(*) FROM members m2
	WHERE m2.guild_id = members.guild_id AND m2.join_date < members.join_date
) + 1
WHERE guild_id = ? AND user_id = ?`

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RejoinMember reactivates a stored member who joined again.
func (d *DB) RejoinMember(guildID, userID int64, username string, nickname *string, joinDate int64, roles []string, online bool) bool {
	positions := d.GuildPositionsInitialized(guildID)
	err := d.tx(func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx(), rejoinSQL, username, nullStr(nickname), joinDate, DumpStrings(roles),
			boolInt(online), guildID, userID); err != nil {
			return err
		}
		if positions {
			_, err := tx.ExecContext(ctx(), joinPositionSQL, guildID, userID)
			return err
		}
		return nil
	})
	if err != nil {
		log.Error("Failed to record rejoin of %d in guild %d: %v", userID, guildID, err)
		return false
	}
	return true
}

// PresenceRow is a member's stored (last_seen, is_active).
type PresenceRow struct {
	LastSeen *int64
	IsActive bool
}

// GetMemberPresenceRows maps user_id -> (last_seen, is_active) for a guild.
func (d *DB) GetMemberPresenceRows(guildID int64) (map[int64]PresenceRow, error) {
	rows, err := d.sql.QueryContext(ctx(), "SELECT user_id, last_seen, is_active FROM members WHERE guild_id = ?", guildID)
	if err != nil {
		log.Error("Database error: %v", err)
		return nil, err
	}
	defer rows.Close()
	out := map[int64]PresenceRow{}
	for rows.Next() {
		var uid int64
		var last, active sql.NullInt64
		if err := rows.Scan(&uid, &last, &active); err != nil {
			return nil, err
		}
		out[uid] = PresenceRow{LastSeen: ptrInt(last), IsActive: !active.Valid || active.Int64 != 0}
	}
	return out, rows.Err()
}

func (d *DB) execBatched(query string, params [][]any) error {
	for i := 0; i < len(params); i += ReconcileBatchSize {
		end := min(i+ReconcileBatchSize, len(params))
		err := d.tx(func(tx *sql.Tx) error {
			st, err := tx.PrepareContext(ctx(), query)
			if err != nil {
				return err
			}
			defer st.Close()
			for _, p := range params[i:end] {
				if _, err := st.ExecContext(ctx(), p...); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// MemberSnapshot is a live member's state for reconciliation.
type MemberSnapshot struct {
	UserID   int64
	Username string
	Nickname *string
	JoinDate int64
	Roles    []string
	Online   bool
}

// ReconcileMembers applies the differences between the live member cache and
// the database. Every statement is idempotent, so a partial failure is
// repaired by the next reconciliation.
func (d *DB) ReconcileMembers(guildID int64, onlineIDs, offlineIDs, departedIDs []int64, rejoined, newMembers []MemberSnapshot, since int64) bool {
	run := func() error {
		var p [][]any
		for _, uid := range onlineIDs {
			p = append(p, []any{guildID, uid})
		}
		if err := d.execBatched("UPDATE members SET last_seen = 0 WHERE guild_id = ? AND user_id = ?", p); err != nil {
			return err
		}
		p = nil
		for _, uid := range offlineIDs {
			p = append(p, []any{since, guildID, uid})
		}
		if err := d.execBatched("UPDATE members SET last_seen = ? WHERE guild_id = ? AND user_id = ? AND last_seen = 0", p); err != nil {
			return err
		}
		p = nil
		for _, uid := range departedIDs {
			p = append(p, []any{since, since, guildID, uid})
		}
		if err := d.execBatched(`UPDATE members SET is_active = 0, left_date = ?, last_seen = ?
			WHERE guild_id = ? AND user_id = ? AND is_active = 1`, p); err != nil {
			return err
		}
		p = nil
		for _, m := range rejoined {
			p = append(p, []any{m.Username, nullStr(m.Nickname), m.JoinDate, DumpStrings(m.Roles), boolInt(m.Online), guildID, m.UserID})
		}
		if err := d.execBatched(rejoinSQL, p); err != nil {
			return err
		}
		p = nil
		for _, m := range newMembers {
			var last any
			if m.Online {
				last = 0
			}
			p = append(p, []any{guildID, m.UserID, m.Username, nullStr(m.Nickname), m.JoinDate, last, DumpStrings(m.Roles), nickHistoryInit(m.Nickname)})
		}
		if err := d.execBatched(addMemberSQL, p); err != nil {
			return err
		}
		if (len(newMembers) > 0 || len(rejoined) > 0) && d.GuildPositionsInitialized(guildID) {
			p = nil
			for _, m := range newMembers {
				p = append(p, []any{guildID, m.UserID})
			}
			for _, m := range rejoined {
				p = append(p, []any{guildID, m.UserID})
			}
			if err := d.execBatched(joinPositionSQL, p); err != nil {
				return err
			}
		}
		return nil
	}
	if err := run(); err != nil {
		log.Error("Failed to reconcile members for guild %d: %v", guildID, err)
		return false
	}
	return true
}

// DatabaseHealth is the result of GetDatabaseHealth.
type DatabaseHealth struct {
	Status     string
	CanConnect bool
	CanRead    bool
	CanWrite   bool
	FileSizeMB float64
	Error      string
}

// GetDatabaseHealth checks connect/read/write (with a real committed write).
func (d *DB) GetDatabaseHealth() DatabaseHealth {
	h := DatabaseHealth{Status: "unknown"}
	if st, err := os.Stat(d.path); err == nil {
		h.FileSizeMB = roundTo(float64(st.Size())/(1024*1024), 2)
	}
	fail := func(err error) DatabaseHealth {
		log.Error("Database health check failed: %v", err)
		h.Status = "unhealthy"
		h.Error = err.Error()
		return h
	}
	if err := d.sql.PingContext(ctx()); err != nil {
		return fail(err)
	}
	h.CanConnect = true
	if _, err := scalarInt(d.sql, "SELECT COUNT(*) FROM guilds"); err != nil {
		return fail(err)
	}
	h.CanRead = true
	err := d.tx(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx(), `INSERT INTO bot_state (key, value) VALUES ('health_check', ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, nowUTC())
		return err
	})
	if err != nil {
		return fail(err)
	}
	h.CanWrite = true
	h.Status = "healthy"
	return h
}

// GuildStats are member counts for a guild.
type GuildStats struct {
	TotalMembers, ActiveMembers, InactiveMembers int64
}

// GetGuildStats counts stored, current and departed members.
func (d *DB) GetGuildStats(guildID int64, f *UserFilter) GuildStats {
	var s GuildStats
	fc, fp := f.clause("user_id")
	args := append([]any{guildID}, fp...)
	var err error
	if s.TotalMembers, err = scalarInt(d.sql, "SELECT COUNT(*) FROM members WHERE guild_id = ?"+fc, args...); err != nil {
		log.Error("Failed to get guild stats for %d: %v", guildID, err)
		return s
	}
	if s.ActiveMembers, err = scalarInt(d.sql, "SELECT COUNT(*) FROM members WHERE guild_id = ? AND is_active = 1"+fc, args...); err != nil {
		log.Error("Failed to get guild stats for %d: %v", guildID, err)
		return s
	}
	if s.InactiveMembers, err = scalarInt(d.sql, "SELECT COUNT(*) FROM members WHERE guild_id = ? AND is_active = 0"+fc, args...); err != nil {
		log.Error("Failed to get guild stats for %d: %v", guildID, err)
	}
	return s
}
