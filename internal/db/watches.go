package db

import (
	"database/sql"
	"errors"
)

// --- Privacy / opt-out ---

func (d *DB) addUserTo(table, what string, userID int64) bool {
	_, err := d.sql.ExecContext(ctx(), "INSERT OR IGNORE INTO "+table+" (user_id, opted_out_at) VALUES (?, ?)", userID, nowUTC())
	if err != nil {
		log.Error("Failed to add %s user %d: %v", what, userID, err)
		return false
	}
	return true
}

func (d *DB) removeUserFrom(table, what string, userID int64) bool {
	res, err := d.sql.ExecContext(ctx(), "DELETE FROM "+table+" WHERE user_id = ?", userID)
	if err != nil {
		log.Error("Failed to remove %s user %d: %v", what, userID, err)
		return false
	}
	return affected(res) > 0
}

func (d *DB) userSet(table, what string) map[int64]bool {
	ids, err := queryInts(d.sql, "SELECT user_id FROM "+table)
	if err != nil {
		log.Error("Failed to get %s users: %v", what, err)
		return map[int64]bool{}
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// AddOptedOutUser adds a user to the global tracking opt-out list.
func (d *DB) AddOptedOutUser(userID int64) bool {
	return d.addUserTo("opted_out_users", "opted-out", userID)
}

// RemoveOptedOutUser removes a user from the opt-out list (false if absent).
func (d *DB) RemoveOptedOutUser(userID int64) bool {
	return d.removeUserFrom("opted_out_users", "opted-out", userID)
}

// GetOptedOutUserIDs is the set of opted-out users.
func (d *DB) GetOptedOutUserIDs() map[int64]bool { return d.userSet("opted_out_users", "opted-out") }

// AddNoWatchUser adds a user to the global "don't watch me" list.
func (d *DB) AddNoWatchUser(userID int64) bool {
	return d.addUserTo("no_watch_users", "no-watch", userID)
}

// RemoveNoWatchUser removes a user from the no-watch list (false if absent).
func (d *DB) RemoveNoWatchUser(userID int64) bool {
	return d.removeUserFrom("no_watch_users", "no-watch", userID)
}

// GetNoWatchUserIDs is the set of users who opted out of being watched.
func (d *DB) GetNoWatchUserIDs() map[int64]bool { return d.userSet("no_watch_users", "no-watch") }

// PurgeCounts are the rows a purge removed.
type PurgeCounts struct {
	Members, RoleChanges, MessageActivity, MessageActivityHourly int64
}

// PurgeUserData deletes everything stored about a user in every guild,
// including watches that target them or DM them. nil on failure.
func (d *DB) PurgeUserData(userID int64) *PurgeCounts {
	var c PurgeCounts
	err := d.tx(func(tx *sql.Tx) error {
		for _, t := range []struct {
			dst   *int64
			table string
		}{
			{&c.Members, "members"},
			{&c.RoleChanges, "role_changes"},
			{&c.MessageActivity, "message_activity"},
			{&c.MessageActivityHourly, "message_activity_hourly"},
		} {
			var err error
			if *t.dst, err = scalarInt(tx, "SELECT COUNT(*) FROM "+t.table+" WHERE user_id = ?", userID); err != nil {
				return err
			}
		}
		// Deleting the member rows cascades to the dependent tables.
		for _, q := range []string{
			"DELETE FROM members WHERE user_id = ?",
			"DELETE FROM watchlists WHERE target_type = 'user' AND target_id = ?",
			"DELETE FROM watchlists WHERE deliver_dm = 1 AND created_by = ?",
		} {
			if _, err := tx.ExecContext(ctx(), q, userID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Error("Failed to purge data for user %d: %v", userID, err)
		return nil
	}
	log.Info("Purged user %d: %d member records, %d role changes, %d daily and %d hourly activity records",
		userID, c.Members, c.RoleChanges, c.MessageActivity, c.MessageActivityHourly)
	return &c
}

// --- Watchlists ---

// Watch is one watchlists row.
type Watch struct {
	ID               int64
	Seq              int64
	GuildID          int64
	TargetType       string // "user" or "role"
	TargetID         int64
	AlertType        string // "offline_for" or "online_return"
	ThresholdSeconds *int64
	ChannelID        *int64
	DeliverDM        bool
	State            string
	FiredTargets     *string
	FailedDeliveries int64
	CreatedBy        int64
	CreatedAt        int64
}

const watchCols = `id, COALESCE(seq, 0), guild_id, target_type, target_id, alert_type, threshold_seconds,
	channel_id, COALESCE(deliver_dm, 0), COALESCE(state, 'armed'), fired_targets,
	COALESCE(failed_deliveries, 0), created_by, created_at`

func scanWatch(sc interface{ Scan(...any) error }) (Watch, error) {
	var w Watch
	var thr, ch sql.NullInt64
	var fired sql.NullString
	var dm int64
	err := sc.Scan(&w.ID, &w.Seq, &w.GuildID, &w.TargetType, &w.TargetID, &w.AlertType, &thr,
		&ch, &dm, &w.State, &fired, &w.FailedDeliveries, &w.CreatedBy, &w.CreatedAt)
	w.ThresholdSeconds, w.ChannelID, w.FiredTargets, w.DeliverDM = ptrInt(thr), ptrInt(ch), ptrStr(fired), dm != 0
	return w, err
}

func (d *DB) queryWatches(errMsg string, query string, args ...any) []Watch {
	rows, err := d.sql.QueryContext(ctx(), "SELECT "+watchCols+" FROM watchlists "+query, args...)
	if err != nil {
		log.Error("%s: %v", errMsg, err)
		return nil
	}
	defer rows.Close()
	var out []Watch
	for rows.Next() {
		w, err := scanWatch(rows)
		if err != nil {
			log.Error("%s: %v", errMsg, err)
			return nil
		}
		out = append(out, w)
	}
	return out
}

// AddWatch creates or updates a watch and returns its per-guild display
// number. Re-adding the same (guild, target, alert type) updates its settings
// and re-arms it, keeping its number; new watches take the smallest free
// number. ok is false on failure.
func (d *DB) AddWatch(guildID int64, targetType string, targetID int64, alertType string,
	thresholdSeconds, channelID *int64, createdBy int64, deliverDM bool) (seq int64, ok bool) {
	err := d.tx(func(tx *sql.Tx) error {
		used, err := queryInts(tx, "SELECT seq FROM watchlists WHERE guild_id = ? AND seq IS NOT NULL", guildID)
		if err != nil {
			return err
		}
		taken := map[int64]bool{}
		for _, s := range used {
			taken[s] = true
		}
		next := int64(1)
		for taken[next] {
			next++
		}
		if _, err := tx.ExecContext(ctx(), `INSERT INTO watchlists
			(guild_id, seq, target_type, target_id, alert_type, threshold_seconds,
			 channel_id, deliver_dm, state, fired_targets, created_by, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'armed', NULL, ?, ?)
			ON CONFLICT(guild_id, target_type, target_id, alert_type) DO UPDATE SET
				threshold_seconds = excluded.threshold_seconds,
				channel_id = excluded.channel_id,
				deliver_dm = excluded.deliver_dm,
				created_by = excluded.created_by,
				state = 'armed',
				fired_targets = NULL,
				failed_deliveries = 0`,
			guildID, next, targetType, targetID, alertType, nullInt(thresholdSeconds), nullInt(channelID),
			boolInt(deliverDM), createdBy, nowUTC()); err != nil {
			return err
		}
		var s sql.NullInt64
		if err := tx.QueryRowContext(ctx(), `SELECT seq FROM watchlists
			WHERE guild_id = ? AND target_type = ? AND target_id = ? AND alert_type = ?`,
			guildID, targetType, targetID, alertType).Scan(&s); err != nil {
			return err
		}
		seq = s.Int64
		return nil
	})
	if err != nil {
		log.Error("Failed to add watch in guild %d: %v", guildID, err)
		return 0, false
	}
	return seq, true
}

// RemoveWatch deletes a watch by row id within its guild.
func (d *DB) RemoveWatch(guildID, watchID int64) bool {
	res, err := d.sql.ExecContext(ctx(), "DELETE FROM watchlists WHERE guild_id = ? AND id = ?", guildID, watchID)
	if err != nil {
		log.Error("Failed to remove watch %d in guild %d: %v", watchID, guildID, err)
		return false
	}
	return affected(res) > 0
}

// RemoveDMWatchesForRecipient deletes a guild's DM watches delivering to userID.
func (d *DB) RemoveDMWatchesForRecipient(guildID, userID int64) int64 {
	res, err := d.sql.ExecContext(ctx(), "DELETE FROM watchlists WHERE guild_id = ? AND deliver_dm = 1 AND created_by = ?", guildID, userID)
	if err != nil {
		log.Error("Failed to remove DM watches for %d in guild %d: %v", userID, guildID, err)
		return 0
	}
	return affected(res)
}

// GetWatch returns one watch by row id (nil if gone or on error).
func (d *DB) GetWatch(watchID int64) *Watch {
	w, err := scanWatch(d.sql.QueryRowContext(ctx(), "SELECT "+watchCols+" FROM watchlists WHERE id = ?", watchID))
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Error("Failed to get watch %d: %v", watchID, err)
		}
		return nil
	}
	return &w
}

// GetGuildWatches lists a guild's watches by display number.
func (d *DB) GetGuildWatches(guildID int64) []Watch {
	return d.queryWatches(sprintf("Failed to get watches for guild %d", guildID), "WHERE guild_id = ? ORDER BY seq", guildID)
}

// CountGuildWatches is the number of watches in a guild.
func (d *DB) CountGuildWatches(guildID int64) int64 {
	n, err := scalarInt(d.sql, "SELECT COUNT(*) FROM watchlists WHERE guild_id = ?", guildID)
	if err != nil {
		log.Error("Failed to count watches for guild %d: %v", guildID, err)
		return 0
	}
	return n
}

// GetWatchGuildIDs is the set of guilds with at least one watch.
func (d *DB) GetWatchGuildIDs() map[int64]bool {
	ids, err := queryInts(d.sql, "SELECT DISTINCT guild_id FROM watchlists")
	if err != nil {
		log.Error("Failed to get watch guild ids: %v", err)
		return map[int64]bool{}
	}
	out := make(map[int64]bool, len(ids))
	for _, id := range ids {
		out[id] = true
	}
	return out
}

// GetOfflineWatches lists every offline_for watch.
func (d *DB) GetOfflineWatches() []Watch {
	return d.queryWatches("Failed to get offline watches", "WHERE alert_type = 'offline_for'")
}

// GetWatchesForMember lists a guild's watches of alertType that target the
// member directly or any of their roles.
func (d *DB) GetWatchesForMember(guildID, userID int64, roleIDs []int64, alertType string) []Watch {
	q := "WHERE guild_id = ? AND alert_type = ? AND ((target_type = 'user' AND target_id = ?)"
	args := []any{guildID, alertType, userID}
	if len(roleIDs) > 0 {
		q += " OR (target_type = 'role' AND target_id IN (SELECT value FROM json_each(?)))"
		args = append(args, DumpInts(roleIDs))
	}
	return d.queryWatches(sprintf("Failed to get watches for member %d in guild %d", userID, guildID), q+")", args...)
}

// GetMembersLastSeen batch-reads last_seen for the given members.
func (d *DB) GetMembersLastSeen(guildID int64, userIDs []int64) map[int64]*int64 {
	out := map[int64]*int64{}
	if len(userIDs) == 0 {
		return out
	}
	rows, err := d.sql.QueryContext(ctx(), `SELECT user_id, last_seen FROM members
		WHERE guild_id = ? AND user_id IN (SELECT value FROM json_each(?))`, guildID, DumpInts(userIDs))
	if err != nil {
		log.Error("Failed to batch-read last_seen for guild %d: %v", guildID, err)
		return map[int64]*int64{}
	}
	defer rows.Close()
	for rows.Next() {
		var uid int64
		var ls sql.NullInt64
		if err := rows.Scan(&uid, &ls); err != nil {
			log.Error("Failed to batch-read last_seen for guild %d: %v", guildID, err)
			return map[int64]*int64{}
		}
		out[uid] = ptrInt(ls)
	}
	if err := rows.Err(); err != nil {
		log.Error("Failed to batch-read last_seen for guild %d: %v", guildID, err)
		return map[int64]*int64{}
	}
	return out
}

// UpdateWatchFireState sets whichever of state / firedTargets is non-nil.
func (d *DB) UpdateWatchFireState(watchID int64, state, firedTargets *string) bool {
	var sets string
	var args []any
	if state != nil {
		sets, args = "state = ?", append(args, *state)
	}
	if firedTargets != nil {
		if sets != "" {
			sets += ", "
		}
		sets += "fired_targets = ?"
		args = append(args, *firedTargets)
	}
	if sets == "" {
		return false
	}
	res, err := d.sql.ExecContext(ctx(), "UPDATE watchlists SET "+sets+" WHERE id = ?", append(args, watchID)...)
	if err != nil {
		log.Error("Failed to update fire-state for watch %d: %v", watchID, err)
		return false
	}
	return affected(res) > 0
}

// RecordWatchDeliveryFailure increments the consecutive failed-delivery
// count and returns it (0 if the watch is gone or on error).
func (d *DB) RecordWatchDeliveryFailure(watchID int64) int64 {
	var n int64
	err := d.tx(func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx(), "UPDATE watchlists SET failed_deliveries = COALESCE(failed_deliveries, 0) + 1 WHERE id = ?", watchID); err != nil {
			return err
		}
		v, err := scalarInt(tx, "SELECT failed_deliveries FROM watchlists WHERE id = ?", watchID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		n = v
		return err
	})
	if err != nil {
		log.Error("Failed to record delivery failure for watch %d: %v", watchID, err)
		return 0
	}
	return n
}

// ResetWatchDeliveryFailures clears the failed-delivery count.
func (d *DB) ResetWatchDeliveryFailures(watchID int64) bool {
	res, err := d.sql.ExecContext(ctx(), "UPDATE watchlists SET failed_deliveries = 0 WHERE id = ?", watchID)
	if err != nil {
		log.Error("Failed to reset delivery failures for watch %d: %v", watchID, err)
		return false
	}
	return affected(res) > 0
}
