// Package config loads the bot configuration from .env.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"lastseen/internal/logx"
)

var log = logx.Get("config")

// ErrMissingEnv means no .env existed; a default one was written.
var ErrMissingEnv = errors.New("Please edit .env file and add your Discord bot token, then restart the bot.")

// ErrNoToken means DISCORD_BOT_TOKEN is unset or still the placeholder.
var ErrNoToken = errors.New("Please set DISCORD_BOT_TOKEN in .env file with your actual bot token.")

// Config is the bot configuration.
type Config struct {
	BotToken             string
	DBFile               string
	LogLevel             logx.Level
	BotAdminRoleName     string
	DefaultInactiveDays  int
	LogsDaysToKeep       int
	BackupIntervalHours  int
	BackupRetentionCount int
	ShardCount           int // 0 = Discord's recommended count
	TopGGToken           string
	DiscordBotListToken  string
	DiscordBotListBotID  string
	VoidBotsToken        string
	StatsPushEnabled     bool
	StatsSCPDestination  string
	StatsSSHKey          string

	LogFolder    string
	BackupFolder string
}

// Load reads .env (creating a default one, and failing with ErrMissingEnv,
// if it does not exist). Values already set in the process environment win.
func Load() (*Config, error) {
	const envPath = ".env"
	if _, err := os.Stat(envPath); errors.Is(err, os.ErrNotExist) {
		if err := createDefaultEnv(envPath, ".env.template"); err != nil {
			return nil, err
		}
		log.Error("No .env file found. A default .env file has been created.")
		log.Error("Please edit .env and add your Discord bot token, then restart the bot.")
		return nil, ErrMissingEnv
	}
	if err := LoadDotenv(envPath); err != nil {
		return nil, err
	}

	c := &Config{
		BotAdminRoleName:    "LastSeen Admin",
		DefaultInactiveDays: 10,
		LogFolder:           "logs",
		BackupFolder:        "backups",
	}
	c.BotToken = strings.TrimSpace(os.Getenv("DISCORD_BOT_TOKEN"))
	if c.BotToken == "" || c.BotToken == "your-bot-token-here" {
		log.Error("DISCORD_BOT_TOKEN not set in .env file.")
		return nil, ErrNoToken
	}
	c.DBFile = strings.TrimSpace(getenv("DB_FILE", "lastseen_bot.db"))
	c.LogLevel = logx.ParseLevel(getenv("DEBUG_LEVEL", "info"))

	c.LogsDaysToKeep = intEnv("DEBUG_LOGS_DAYS_TO_KEEP", 5, 0, "Invalid DEBUG_LOGS_DAYS_TO_KEEP, using 5", "")
	c.BackupIntervalHours = intEnv("DB_BACKUP_INTERVAL_HOURS", 24, 1,
		"Invalid DB_BACKUP_INTERVAL_HOURS, using 24", "DB_BACKUP_INTERVAL_HOURS must be at least 1, using 24")
	c.BackupRetentionCount = intEnv("DB_BACKUP_RETENTION_COUNT", 5, 1,
		"Invalid DB_BACKUP_RETENTION_COUNT, using 5", "DB_BACKUP_RETENTION_COUNT must be at least 1, using 5")

	if s := strings.TrimSpace(os.Getenv("SHARD_COUNT")); s != "" {
		n, err := strconv.Atoi(s)
		switch {
		case err != nil:
			log.Warn("Invalid SHARD_COUNT, using Discord's recommended count")
		case n < 1:
			log.Warn("SHARD_COUNT must be at least 1, using Discord's recommended count")
		default:
			c.ShardCount = n
		}
	}

	c.TopGGToken = strings.TrimSpace(os.Getenv("TOPGG_TOKEN"))
	c.DiscordBotListToken = strings.TrimSpace(os.Getenv("DISCORDBOTLIST_TOKEN"))
	c.DiscordBotListBotID = strings.TrimSpace(os.Getenv("DISCORDBOTLIST_BOT_ID"))
	c.VoidBotsToken = strings.TrimSpace(os.Getenv("VOIDBOTS_TOKEN"))
	switch strings.ToLower(strings.TrimSpace(os.Getenv("STATS_PUSH_ENABLED"))) {
	case "1", "true", "yes", "on":
		c.StatsPushEnabled = true
	}
	c.StatsSCPDestination = strings.TrimSpace(os.Getenv("STATS_SCP_DESTINATION"))
	c.StatsSSHKey = strings.TrimSpace(os.Getenv("STATS_SSH_KEY"))

	shards := "auto (Discord recommended)"
	if c.ShardCount > 0 {
		shards = strconv.Itoa(c.ShardCount)
	}
	log.Info("Configuration loaded successfully")
	log.Info("Database file: %s", c.DBFile)
	log.Info("Log level: %s", c.LogLevel)
	log.Info("Log retention: %d days", c.LogsDaysToKeep)
	log.Info("Database backup: every %d hours, keep %d backups", c.BackupIntervalHours, c.BackupRetentionCount)
	log.Info("Shard count: %s", shards)
	log.Info("TOP.GG metrics: %s", onOff(c.TopGGToken != "", "disabled (no TOPGG_TOKEN)"))
	log.Info("DiscordBotList metrics: %s", onOff(c.DiscordBotListToken != "", "disabled (no DISCORDBOTLIST_TOKEN)"))
	log.Info("VoidBots metrics: %s", onOff(c.VoidBotsToken != "", "disabled (no VOIDBOTS_TOKEN)"))
	log.Info("Stats push: %s", onOff(c.StatsPushEnabled, "disabled (STATS_PUSH_ENABLED not set)"))
	return c, nil
}

func onOff(on bool, off string) string {
	if on {
		return "enabled"
	}
	return off
}

func getenv(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// intEnv parses an integer setting. An unparsable value
// falls back to def with invalidMsg, and a value below min falls back to def
// with lowMsg (min 0 = no lower bound check).
func intEnv(key string, def, min int, invalidMsg, lowMsg string) int {
	s := strings.TrimSpace(getenv(key, strconv.Itoa(def)))
	n, err := strconv.Atoi(s)
	if err != nil {
		log.Warn("%s", invalidMsg)
		return def
	}
	if min > 0 && n < min {
		log.Warn("%s", lowMsg)
		return def
	}
	return n
}

// LoadDotenv sets the variables from a .env file that are not already set in
// the environment. It understands comments, "export " prefixes, quoted
// values and inline " #" comments after unquoted values.
func LoadDotenv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		key, val, ok := parseDotenvLine(sc.Text())
		if !ok {
			continue
		}
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, val)
		}
	}
	return sc.Err()
}

func parseDotenvLine(line string) (string, string, bool) {
	line = strings.TrimSpace(strings.TrimPrefix(line, string(rune(0xFEFF))))
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	key, val, ok := strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return "", "", false
	}
	val = strings.TrimSpace(val)
	if len(val) > 0 && (val[0] == '"' || val[0] == '\'') {
		q := val[0]
		if end := strings.IndexByte(val[1:], q); end >= 0 {
			inner := val[1 : 1+end]
			if q == '"' {
				inner = strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\"`, `"`, `\\`, `\`).Replace(inner)
			}
			return key, inner, true
		}
	}
	// Unquoted: an inline comment starts at whitespace followed by '#'.
	for i := 1; i < len(val); i++ {
		if val[i] == '#' && (val[i-1] == ' ' || val[i-1] == '\t') {
			val = strings.TrimSpace(val[:i])
			break
		}
	}
	return key, val, true
}

func createDefaultEnv(envPath, templatePath string) error {
	content := defaultEnv
	if data, err := os.ReadFile(templatePath); err == nil {
		content = string(data)
		log.Info("Created .env file from template")
	} else {
		log.Info("Created default .env file")
	}
	if err := os.WriteFile(envPath, []byte(content), 0o600); err != nil {
		log.Error("Failed to create .env file: %v", err)
		return fmt.Errorf("create .env: %w", err)
	}
	return nil
}

const defaultEnv = `# Discord Bot Configuration
# Fill in your bot token below

# REQUIRED: Your Discord bot token from https://discord.com/developers/applications
DISCORD_BOT_TOKEN=your-bot-token-here

# REQUIRED: database name
DB_FILE=lastseen_bot.db

# Logging and Debug Settings
DEBUG_LEVEL=info  # options: info, debug, warning, error
DEBUG_LOGS_DAYS_TO_KEEP=5  # number of days to keep log files (older logs are deleted)

# Database Backup Settings
DB_BACKUP_INTERVAL_HOURS=24  # how often to backup the database (in hours)
DB_BACKUP_RETENTION_COUNT=5  # number of backup copies to keep (older backups are deleted)

# Sharding Settings
# SHARD_COUNT=2  # uncomment to force a fixed shard count (testing only); unset = Discord's recommended count

# TOP.GG Token (optional): Set your token here to automatically update the server count on the TOP.GG site listing.
# Leave unset to disable this feature.
TOPGG_TOKEN=

# DiscordBotList Token (Optional): Set your token here to automatically update the server count on the discordbotlist.com site listing.
# Leave unset to disable this feature.
DISCORDBOTLIST_TOKEN=
# The bot's discordbotlist.com URL slug (e.g. lastseen-2591), NOT the numeric Discord ID.
# Find it in your bot's discordbotlist.com page URL. Falls back to the numeric ID if unset.
DISCORDBOTLIST_BOT_ID=

# VoidBots.net Token (Optional): Set your token here to automatically update the server count on the VoidBots.net site listing.
# Leave unset to disable this feature.
VOIDBOTS_TOKEN=

# Stats push (optional): when enabled, the bot writes a small stats.json (server
# count, user count, timestamp) every 10 minutes and copies it to the LAN
# webserver via scp. Leave unset/false on dev so it never overwrites the
# production stats file. Set to true only on the production instance.
STATS_PUSH_ENABLED=false
# scp destination (user@host:/remote/path) and the SSH private key to use.
STATS_SCP_DESTINATION=
STATS_SSH_KEY=

`
