package db

import (
	"database/sql"
	"errors"
)

// GuildConfig is a guilds row.
type GuildConfig struct {
	GuildID               int64
	GuildName             string
	NotificationChannelID *int64
	InactiveDays          int
	BotAdminRoleName      string
	UserRoleRequired      bool
	UserRoleName          string
	AddedAt               int64
	TrackOnlyRoles        *string // JSON list of role names
	AllowedChannels       *string // JSON list of channel ids
	PositionsInitialized  bool
	MessageRetentionDays  int
	Timezone              string
	ReportChannelID       *int64
	ReportFrequency       *string
	ReportTypes           *string // JSON list
	ReportDayWeekly       int
	ReportDayMonthly      int
	LastWeeklyReport      int64
	LastMonthlyReport     int64
	ReportTimeHour        int
	Language              string
}

// TrackOnlyRoleList is the parsed track_only_roles (nil when unset/invalid).
func (g *GuildConfig) TrackOnlyRoleList() []string {
	if g == nil || g.TrackOnlyRoles == nil || *g.TrackOnlyRoles == "" {
		return nil
	}
	l, _ := LoadStrings(*g.TrackOnlyRoles)
	return l
}

const guildCols = `guild_id, guild_name, notification_channel_id,
	COALESCE(inactive_days, 10), COALESCE(bot_admin_role_name, 'LastSeen Admin'),
	COALESCE(user_role_required, 0), COALESCE(user_role_name, 'LastSeen User'), added_at,
	track_only_roles, allowed_channels, COALESCE(positions_initialized, 0),
	COALESCE(message_retention_days, 365), COALESCE(timezone, 'UTC'),
	report_channel_id, report_frequency, report_types,
	COALESCE(report_day_weekly, 0), COALESCE(report_day_monthly, 1),
	COALESCE(last_weekly_report, 0), COALESCE(last_monthly_report, 0),
	COALESCE(report_time_hour, 9), COALESCE(language, 'en')`

func scanGuild(sc interface{ Scan(...any) error }) (*GuildConfig, error) {
	var g GuildConfig
	var notif, rch sql.NullInt64
	var tor, ach, rfreq, rtypes sql.NullString
	var urr, posInit int64
	if err := sc.Scan(&g.GuildID, &g.GuildName, &notif, &g.InactiveDays, &g.BotAdminRoleName,
		&urr, &g.UserRoleName, &g.AddedAt, &tor, &ach, &posInit,
		&g.MessageRetentionDays, &g.Timezone, &rch, &rfreq, &rtypes,
		&g.ReportDayWeekly, &g.ReportDayMonthly, &g.LastWeeklyReport, &g.LastMonthlyReport,
		&g.ReportTimeHour, &g.Language); err != nil {
		return nil, err
	}
	g.NotificationChannelID = ptrInt(notif)
	g.ReportChannelID = ptrInt(rch)
	g.TrackOnlyRoles = ptrStr(tor)
	g.AllowedChannels = ptrStr(ach)
	g.ReportFrequency = ptrStr(rfreq)
	g.ReportTypes = ptrStr(rtypes)
	g.UserRoleRequired = urr != 0
	g.PositionsInitialized = posInit != 0
	return &g, nil
}

// AddGuild inserts a guild row if missing; true if a row was inserted.
func (d *DB) AddGuild(guildID int64, name string, inactiveDays int) bool {
	var n int64
	err := d.tx(func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx(), `INSERT OR IGNORE INTO guilds
			(guild_id, guild_name, inactive_days, added_at, bot_admin_role_name, user_role_name)
			VALUES (?, ?, ?, ?, 'LastSeen Admin', 'LastSeen User')`,
			guildID, name, inactiveDays, nowUTC())
		n = affected(res)
		return err
	})
	if err != nil {
		log.Error("Failed to add guild %d: %v", guildID, err)
		return false
	}
	return n > 0
}

// UpdateGuildName renames a guild.
func (d *DB) UpdateGuildName(guildID int64, name string) bool {
	var n int64
	err := d.tx(func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx(), "UPDATE guilds SET guild_name = ? WHERE guild_id = ?", name, guildID)
		n = affected(res)
		return err
	})
	if err != nil {
		log.Error("Failed to update guild name %d: %v", guildID, err)
		return false
	}
	return n > 0
}

// setGuildField ensures the guild row exists, then sets one column (and fills
// in a placeholder 'Unknown' guild name).
func (d *DB) setGuildField(guildID int64, guildName, column string, value any, what string) bool {
	err := d.tx(func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx(), `INSERT OR IGNORE INTO guilds (guild_id, guild_name, inactive_days, added_at)
			VALUES (?, ?, 10, ?)`, guildID, guildName, nowUTC()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx(), `UPDATE guilds SET `+column+` = ?,
			guild_name = CASE WHEN guild_name = 'Unknown' THEN ? ELSE guild_name END
			WHERE guild_id = ?`, value, guildName, guildID)
		return err
	})
	if err != nil {
		log.Error("Failed to set %s for guild %d: %v", what, guildID, err)
		return false
	}
	return true
}

func (d *DB) SetNotificationChannel(guildID int64, channelID *int64, guildName string) bool {
	return d.setGuildField(guildID, guildName, "notification_channel_id", nullInt(channelID), "notification channel")
}

func (d *DB) SetInactiveDays(guildID int64, days int, guildName string) bool {
	return d.setGuildField(guildID, guildName, "inactive_days", days, "inactive days")
}

func (d *DB) SetMessageRetentionDays(guildID int64, days int, guildName string) bool {
	return d.setGuildField(guildID, guildName, "message_retention_days", days, "message retention days")
}

func (d *DB) SetTimezone(guildID int64, tz, guildName string) bool {
	return d.setGuildField(guildID, guildName, "timezone", tz, "timezone")
}

func (d *DB) SetGuildLanguage(guildID int64, lang, guildName string) bool {
	return d.setGuildField(guildID, guildName, "language", lang, "language")
}

func (d *DB) SetBotAdminRole(guildID int64, role, guildName string) bool {
	return d.setGuildField(guildID, guildName, "bot_admin_role_name", role, "bot admin role")
}

func (d *DB) SetUserRoleRequired(guildID int64, required bool, guildName string) bool {
	v := 0
	if required {
		v = 1
	}
	return d.setGuildField(guildID, guildName, "user_role_required", v, "user role required")
}

func (d *DB) SetUserRoleName(guildID int64, role, guildName string) bool {
	return d.setGuildField(guildID, guildName, "user_role_name", role, "user role name")
}

const maxRoleNameLength = 100

// SetTrackOnlyRoles sets the tracked roles (empty = track all).
func (d *DB) SetTrackOnlyRoles(guildID int64, roles []string, guildName string) bool {
	var truncated []string
	for _, r := range roles {
		rs := []rune(r)
		if len(rs) > maxRoleNameLength {
			r = string(rs[:maxRoleNameLength])
		}
		truncated = append(truncated, r)
	}
	var v any
	if len(truncated) > 0 {
		v = DumpStrings(truncated)
	}
	return d.setGuildField(guildID, guildName, "track_only_roles", v, "track only roles")
}

// SetAllowedChannels sets the command channels (empty = all channels).
func (d *DB) SetAllowedChannels(guildID int64, channelIDs []int64, guildName string) bool {
	var v any
	if len(channelIDs) > 0 {
		v = DumpInts(channelIDs)
	}
	return d.setGuildField(guildID, guildName, "allowed_channels", v, "allowed channels")
}

// SetReportConfig sets the scheduled report configuration.
func (d *DB) SetReportConfig(guildID, channelID int64, frequency string, types []string,
	dayWeekly, dayMonthly, hour int, guildName string) bool {
	err := d.tx(func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx(), `INSERT OR IGNORE INTO guilds (guild_id, guild_name, inactive_days, added_at)
			VALUES (?, ?, 10, ?)`, guildID, guildName, nowUTC()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx(), `UPDATE guilds
			SET report_channel_id = ?, report_frequency = ?, report_types = ?,
				report_day_weekly = ?, report_day_monthly = ?, report_time_hour = ?,
				guild_name = CASE WHEN guild_name = 'Unknown' THEN ? ELSE guild_name END
			WHERE guild_id = ?`,
			channelID, frequency, DumpStrings(types), dayWeekly, dayMonthly, hour, guildName, guildID)
		return err
	})
	if err != nil {
		log.Error("Failed to set report config for guild %d: %v", guildID, err)
		return false
	}
	return true
}

// DisableReports clears the report configuration.
func (d *DB) DisableReports(guildID int64) bool {
	err := d.tx(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx(), `UPDATE guilds SET report_channel_id = NULL,
			report_frequency = NULL, report_types = NULL WHERE guild_id = ?`, guildID)
		return err
	})
	if err != nil {
		log.Error("Failed to disable reports for guild %d: %v", guildID, err)
		return false
	}
	return true
}

// UpdateLastReportTime stamps the last weekly/monthly report time.
func (d *DB) UpdateLastReportTime(guildID int64, reportType string) bool {
	col := ""
	switch reportType {
	case "weekly":
		col = "last_weekly_report"
	case "monthly":
		col = "last_monthly_report"
	default:
		return true
	}
	err := d.tx(func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx(), "UPDATE guilds SET "+col+" = ? WHERE guild_id = ?", nowUTC(), guildID)
		return err
	})
	if err != nil {
		log.Error("Failed to update last report time for guild %d: %v", guildID, err)
		return false
	}
	return true
}

// GetGuildConfig returns the guild's row, or nil.
func (d *DB) GetGuildConfig(guildID int64) *GuildConfig {
	g, err := scanGuild(d.sql.QueryRowContext(ctx(), "SELECT "+guildCols+" FROM guilds WHERE guild_id = ?", guildID))
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Error("Failed to get guild config %d: %v", guildID, err)
		}
		return nil
	}
	return g
}

// RemoveGuild deletes a guild (cascading to all its data).
func (d *DB) RemoveGuild(guildID int64) bool {
	var n int64
	err := d.tx(func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx(), "DELETE FROM guilds WHERE guild_id = ?", guildID)
		n = affected(res)
		return err
	})
	if err != nil {
		log.Error("Failed to remove guild %d: %v", guildID, err)
		return false
	}
	return n > 0
}

// GuildPositionsInitialized reports whether join positions were enumerated.
func (d *DB) GuildPositionsInitialized(guildID int64) bool {
	v, err := scalarInt(d.sql, "SELECT positions_initialized FROM guilds WHERE guild_id = ?", guildID)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			log.Error("Failed to check positions initialized for guild %d: %v", guildID, err)
		}
		return false
	}
	return v != 0
}

// MarkPositionsInitialized records that join positions were enumerated.
func (d *DB) MarkPositionsInitialized(guildID int64) bool {
	var n int64
	err := d.tx(func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx(), "UPDATE guilds SET positions_initialized = 1 WHERE guild_id = ?", guildID)
		n = affected(res)
		return err
	})
	if err != nil {
		log.Error("Failed to mark positions initialized for guild %d: %v", guildID, err)
		return false
	}
	return n > 0
}

// GetAllGuildIDs lists every stored guild id.
func (d *DB) GetAllGuildIDs() []int64 {
	ids, err := queryInts(d.sql, "SELECT guild_id FROM guilds")
	if err != nil {
		log.Error("Failed to get all guild IDs: %v", err)
		return nil
	}
	return ids
}

func queryInts(q querier, query string, args ...any) ([]int64, error) {
	rows, err := q.QueryContext(ctx(), query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var v sql.NullInt64
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v.Int64)
	}
	return out, rows.Err()
}
