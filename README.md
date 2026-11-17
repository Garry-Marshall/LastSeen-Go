# LastSeen Discord Bot (Go)

A Discord bot for monitoring and tracking member activity across servers: joins, leaves, nickname and role changes, presence (last seen) and message activity.

This is the Go version of LastSeen. It behaves exactly like the Python bot: same commands, buttons, dialogs, texts and translations. It also uses the same database, so an existing `lastseen_bot.db` works as-is. It builds to a single self-contained program for Windows and Linux; nothing else needs installing to run it.

 - Invite LastSeen into your Discord Server: [Invite Bot](https://discord.com/oauth2/authorize?client_id=1068871708322320445)
 - Get support in our Community Discord Server: [Get Support](https://discord.gg/d3N5sd58fh)

## Core Features

### 🚀 Quick Setup
Guided step-by-step wizard for first-time configuration. Just follow the prompts to set up the notification channel, inactive threshold, admin role and timezone.

### 👁️ LastSeen Tracking
Know exactly when members were last online. Track presence changes, join/leave times, and activity patterns across your entire server.

### 💤 Inactive Members
Instantly identify members who've been offline beyond your threshold. Perfect for engagement campaigns, role cleanup, or understanding server health.

### 👀 Watchlists & Activity Alerts
Get pinged automatically when specific members — or a whole role — come back online or go quiet. Watch a user or role for a return to online (optionally only after a long absence), or for crossing an offline threshold (e.g. "alert me if any staff member hasn't been seen in 48h"). Alerts post to the channel you set them up in, or to your DMs. When you start watching a member, they're automatically sent a DM letting them know, and anyone can opt out of being watched with `/no-watch`. See `/help watch` in-app for full details.

### 👤 WhoIs (Member Profiles)
Comprehensive member information at a glance: join date, roles, nickname history, last seen, and message activity stats.

### 📊 Analytics Dashboard
Interactive statistics with retention reports, growth trends, activity leaderboards, heatmaps, participation and member lifecycle. Export to CSV/TXT for deeper analysis.

### 📅 Scheduled Reports
Automated weekly/monthly reports delivered to your chosen channel. Track activity, new members, and departures without lifting a finger.

### 💾 Automatic Backups
Configurable database backups with automatic retention management.

---

## Features

- **Multi-Guild Support**: Works across multiple Discord servers simultaneously
- **User Tracking**: Monitors joins, leaves, nickname changes, and role updates
- **Presence Monitoring**: Tracks when users go offline/online
- **Watchlists & Alerts**: Admin-configured alerts when a user or role returns online or crosses an offline threshold
- **Activity Statistics**: Detailed server activity metrics with visual charts
- **Scheduled Reports**: Automated weekly/monthly reports with activity, new members, departures and retention
- **Timezone Support**: Per-server timezone for accurate timestamp display
- **Localization**: Per-server language with built-in translations (English, Spanish, French, Dutch, Polish)
- **Bot list integrations**: Optional server-count updates for TOP.GG, discordbotlist.com and VoidBots
- **Role-Based Visibility**: Optionally track only members with specific roles
- **Channel Restrictions**: Limit bot commands to specific channels
- **Privacy controls**: `/forgetme` deletes a member's data and stops tracking; `/no-watch` opts out of watch alerts
- **SQLite database**: Connection pooling, buffered writes and indexed queries for busy servers
- **Admin Panel**: Interactive configuration with buttons and dialogs
- **Automatic Backups**: Scheduled database backups with configurable interval and retention
- **Logging**: Daily log files with configurable log levels and automatic cleanup

## Commands

### User Commands (Requires "LastSeen User" if "User Role Required" = ON)

- `/whois <user>` - Display detailed information about a user
- `/lastseen <user>` - Check when a user was last seen online
- `/seen <user>` - Alias for `/lastseen`
- `/chat-history [user]` - Message activity for the last 365 days: your own, or anyone's for bot admins (leave empty for server-wide stats)
- `/mystats` - View your own activity statistics (always private)
- `/forgetme` - Delete your tracked data and opt out of tracking
- `/optin` - Re-enable activity tracking after a `/forgetme` opt-out
- `/no-watch` - Stop admins from setting presence-watch alerts on you (activity tracking stays on); run again to allow it
- `/help [command]` - Show available commands, or detailed help for one (e.g. `/help watch`)
- `/about` - Show bot information and system resources

### Admin Commands (Requires the "LastSeen Admin" role or Administrator permission)

- `/inactive [days]` - List members inactive for more than the configured days (add days to override the threshold)
- `/user-stats` - Server statistics and analytics dashboard
- `/config` - Open the interactive configuration panel (see [Per-Server Configuration](#per-server-configuration))
- `/search` - Advanced member search and filtering, with CSV/TXT export
- `/watch` - Watchlists & activity alerts
  - `/watch online <user|role> [after] [channel] [dm]` - Alert when the target comes back online (add `after`, e.g. `7d`, to only alert after a long absence)
  - `/watch offline <user|role> <duration> [channel] [dm]` - Alert when the target has been offline for `<duration>` (e.g. `48h`, `7d`)
  - `/watch list` - List this server's watches and their ids
  - `/watch remove <target>` - Remove a watch by its number from `/watch list`, or by user or role
- `/role-history <user>` - Show the last 20 role changes for a member

## Usage Examples

### Check when a user was last seen
```
/lastseen @username
/seen username
/lastseen 123456789012345678
```

### Search and filter members (Admin only)

Combine filters to find specific members:

- `roles` - One or more roles (e.g. `@Moderator,@Admin`)
- `status` - `online`, `offline`, `idle`, `dnd`, or `all`
- `inactive` - Days since last seen (e.g. `>30`, `<7`, `=14`)
- `activity` - Messages in the last 30 days (e.g. `>100`, `<10`)
- `silent` - Days since last message, recent joiners excluded (e.g. `>60`)
- `joined` - Join date (e.g. `>2025-01-01`, `<2024-06-01`)
- `departed` - Leave date; lists members who left (e.g. `>2024-01-01`)
- `username` - Partial, case-insensitive username match
- `export` - Export results as `csv` or `txt`

```
/search roles:@Moderator status:online
/search inactive:>30 activity:<10
/search joined:>2025-01-01 export:csv
/search roles:@Member status:offline inactive:>7 activity:<50
```

Results are private, paginated (15 per page) and capped at 1000.

### Watchlists & activity alerts (Admin only)

```
/watch online @Alice              # alert when Alice comes back online
/watch online @Alice after:7d     # ...only after an absence of 7+ days
/watch offline @Bob 7d            # alert when Bob has been offline for 7 days
/watch offline @Staff 48h         # alert when any Staff member goes quiet for 48h
/watch offline @Alice 7d dm:True  # get the alert as a DM instead of a channel post
/watch list
/watch remove 3
```

- Durations use `m`/`h`/`d`, e.g. `30m`, `48h`, `7d`
- `dm:True` sends the alert to whoever set the watch, and can't be combined with a channel
- Online alerts have a 1-hour per-member cooldown
- Watched members get a DM when a watch is created on them, and can opt out with `/no-watch`
- Members who opted out with `/forgetme` can't be watched
- Up to 50 watches per server

### Server statistics (Admin only)

`/user-stats` opens a dashboard with an overview (members, growth, activity, last-seen distribution) and buttons for:

- 📊 **Retention Report**: cohorts of members who joined in the last 30/60/90 days
- 📈 **Server Growth**: joins vs leaves over 30/90/365 days
- 💓 **Community Pulse**: community health and trends
- 🏆 **Leaderboard**: top members by messages for 7 days, 30 days, 90 days or all time
- 🔥 **Activity Heatmap**: activity by day of the week and hour
- 👥 **Participation**: lurkers (present but silent) and ghosts (never active)
- 🔄 **Lifecycle**: new, exploring, active, quiet, dormant and returned members
- 📋 **Export Report**: a full text report

## Per-Server Configuration

Use `/config` in your server. The panel has these buttons:

| Button | What it does |
|---|---|
| 📢 Set Notification Channel | Where member-leave messages are posted (leave empty to turn them off) |
| 📅 Set Inactive Days | Threshold for `/inactive` (1–365, default 10) |
| 🌍 Set Timezone | Server timezone for timestamps and reports, e.g. `Europe/Brussels` (default UTC) |
| 🌐 Set Language | The bot's language for this server |
| 👑 Set Bot Admin Role | Role that can manage the bot (default "LastSeen Admin"; Administrators always can) |
| 👤 Set User Role | Role required for user commands when the toggle below is ON (default "LastSeen User") |
| 🔐 User Role Required = ON/OFF | Whether user commands require the user role |
| 🎯 Set Track Only Roles | Only track members with at least one of these roles (empty = track everyone) |
| 📝 Set Allowed Channels | Only allow commands in these channels (empty = all channels) |
| 📊 Configure Reports | Scheduled reports (see below) |
| ⏹️ Disable Reports | Turn scheduled reports off |
| 🚀 Quick Setup | Guided 5-step wizard: channel, inactive days, admin role, timezone, summary |
| ⚙️ View Config | Show the current configuration |

### Scheduled Reports

- **Channel**: a channel mention, ID or name
- **Frequency**: `weekly`, `monthly` or `both`
- **Report types**: the activity overview is always included; optional sections are `members` (new members), `departures` and `retention`
- **Days**: `weekly,monthly`, e.g. `1,15`. The weekly day is 1–7 (1 = Monday, 7 = Sunday); the monthly day is 1–28.
- **Hour**: 0–23, in the server's timezone

A report is sent at most once per scheduled day, even across restarts. Empty sections are left out.

### Track Only Roles

When roles are set, only members with **at least one** of them are tracked; everyone else is ignored. Role names are case-sensitive and must match Discord exactly. A member who gains a listed role is tracked from then on; one who loses all of them stops being updated, but their existing data is kept.

---

## Running the bot

### Discord application setup

1. Go to https://discord.com/developers/applications and create an application.
2. In **Bot**, create the bot and copy its token.
3. Enable these **Privileged Gateway Intents**:
   - ✅ PRESENCE INTENT
   - ✅ SERVER MEMBERS INTENT
4. Invite it via **OAuth2 → URL Generator** with scopes `bot` and `applications.commands`, and permissions *View Channels*, *Send Messages* and *Embed Links*.

### Building

You need [Go](https://go.dev/dl/) 1.27 or newer to build. The finished program needs nothing installed.

Run directly from the source folder (development):
```bash
go run ./cmd/lastseen
```

Build release binaries for Windows and Linux (PowerShell):
```powershell
.\build.ps1
```

This produces two ready-to-use folders, `dist\windows-amd64\` and `dist\linux-amd64\`. Each holds the program (`lastseen.exe` or `lastseen`, with the git version stamped in; check it with `lastseen -version`) and `locales\`. The Linux folder also holds `install.sh` and `lastseen.service` for [deploying](#deploying-on-linux). For an ARM server (e.g. a Raspberry Pi), use `.\build.ps1 linux/arm64`. If PowerShell blocks scripts, run `powershell -ExecutionPolicy Bypass -File .\build.ps1`.

### Running on Windows

Run the bot **from its folder**: `.env`, the database, `locales\`, `logs\` and `backups\` are all relative to the folder you start it from.

```powershell
.\dist\windows-amd64\lastseen.exe
```

On the first run it creates a `.env` file and exits. Add your bot token to it and start the bot again. Press Ctrl+C to stop it cleanly.

### Deploying on Linux

The bot runs as a systemd service from `/opt/lastseen`, under its own `lastseen` user. That folder belongs to the `lastseen` user, so you can't copy files into it directly ("Permission denied" or "Access denied"). Instead, copy the build folder to your home folder on the server and let [deploy/install.sh](deploy/install.sh) put everything in place.

**First install**

1. Build, then copy the Linux folder to the server. `you@server:` with nothing after the colon means your home folder there, so this creates `~/linux-amd64`.
   ```powershell
   .\build.ps1
   ```
   ```powershell
   scp -r dist\linux-amd64 you@server:
   ```

2. Put your `.env` (see [Configuration](#configuration)) in that folder. When moving from the Python bot, add its database too (see [Moving from the Python bot](#moving-from-the-python-bot)).
   ```powershell
   scp C:\path\to\.env C:\path\to\lastseen_bot.db you@server:linux-amd64/
   ```
   If the Python bot runs on the same server, copy them there instead:
   ```bash
   cp /path/to/python-bot/.env /path/to/python-bot/lastseen_bot.db ~/linux-amd64/
   ```

3. Run the installer on the server:
   ```bash
   sudo sh linux-amd64/install.sh
   ```

The installer:
- creates the `lastseen` user and `/opt/lastseen` (first time only);
- installs the program, `locales/` and the systemd service, then (re)starts the bot;
- installs the `.env` and database from the folder only when `/opt/lastseen` doesn't have them yet, so it never overwrites them;
- shows the last log lines if the bot doesn't stay up.

**Updating**

Build, then copy and install in one line from Windows (`ssh -t` lets sudo ask for your password):
```powershell
.\build.ps1
```
```powershell
scp -r dist\linux-amd64 you@server: ; if ($LASTEXITCODE -eq 0) { ssh -t you@server sudo sh linux-amd64/install.sh }
```

This replaces the program, `locales/` and the service, and restarts the bot. Your `.env`, database, logs and backups stay as they are.

**Day to day:**
```bash
sudo systemctl status lastseen             # running? recent log lines
sudo journalctl -u lastseen -f             # follow the live log
sudo -u lastseen nano /opt/lastseen/.env   # change settings (then restart)
sudo systemctl restart lastseen            # restart
sudo systemctl stop lastseen               # clean shutdown
```

The service restarts the bot 5 seconds after a crash and starts it at boot. It does **not** restart after a configuration error, such as a missing or rejected token: fix `.env`, then start it again. If Discord refuses the privileged intents, the bot waits 10 minutes before exiting, so restarts stay well under Discord's daily login limit.

**A second bot on the same server (e.g. a test bot)**

Give the installer a name. That name is used for the user, the folder and the service, so the two bots don't touch each other:
```bash
sudo sh linux-amd64/install.sh lastseen-dev
```

This installs into `/opt/lastseen-dev`, as user `lastseen-dev`, with the service `lastseen-dev` (`sudo journalctl -u lastseen-dev -f`, and so on). Pass the same name every time you update it. Give it its own `.env` with the test bot's token, and leave `STATS_PUSH_ENABLED` off and the bot-list tokens empty, so it doesn't overwrite the production bot's numbers.

The installer never takes over a service it didn't install itself. If another `lastseen` service already exists, such as the Python bot's, it stops without changing anything and says what to do.

### Exit codes

| Code | Meaning | Restarted? |
|---|---|---|
| 0 | Clean stop (Ctrl+C, `systemctl stop`) | No |
| 1 | Crash; details appended to `logs/crash.log` | Yes |
| 2 | Configuration error (no `.env`, missing or rejected token) | No |

### Moving from the Python bot

The Go bot uses the same database schema and the same `.env` settings, so there is nothing to convert.

1. Stop the Python bot and wait for it to exit cleanly. It flushes its buffers and closes the database on the way out.
2. Copy `lastseen_bot.db` and `.env` into the Go bot's folder. If `lastseen_bot.db-wal` exists next to the database, copy it too. Optionally copy `backups/` as well. On Linux, put them next to `install.sh` before running it (see [Deploying on Linux](#deploying-on-linux)).
3. Start the Go bot.

Never run both bots at the same time: they share a token and would both answer commands. You can switch back by stopping the Go bot and starting the Python bot on the same database.

**On Linux, when the Python bot runs as a service named `lastseen`:**

1. Stop the Python bot and set its service file aside, so the installer can install the Go bot's service under the same name. The installer's message shows the file's exact path.
   ```bash
   sudo systemctl disable --now lastseen
   ```
   ```bash
   sudo mv /etc/systemd/system/lastseen.service /root/lastseen-python.service && sudo systemctl daemon-reload
   ```
2. Copy the Python bot's `.env` and database (with `lastseen_bot.db-wal` if it exists) next to `install.sh`, and run it:
   ```bash
   sudo cp /path/to/python-bot/.env /path/to/python-bot/lastseen_bot.db* ~/linux-amd64/
   ```
   ```bash
   sudo sh linux-amd64/install.sh
   ```

To switch back, stop the Go bot, copy its database back to the Python bot's folder (it holds everything recorded since the switch), and restore the Python service:
```bash
sudo systemctl disable --now lastseen
```
```bash
sudo cp /opt/lastseen/lastseen_bot.db* /path/to/python-bot/
```
```bash
sudo mv /root/lastseen-python.service /etc/systemd/system/lastseen.service && sudo systemctl daemon-reload && sudo systemctl enable --now lastseen
```
Check that the copied files belong to the user the Python bot runs as.

## Configuration

Settings live in `.env` in the bot's folder. If it's missing, the bot creates a template and exits. Values already set in the environment take precedence.

```env
# REQUIRED: your Discord bot token
DISCORD_BOT_TOKEN=your-bot-token-here

# Database filename (default: lastseen_bot.db)
DB_FILE=lastseen_bot.db

# Logging: info, debug, warning or error (default: info)
DEBUG_LEVEL=info
# Days of log files to keep (default: 5)
DEBUG_LOGS_DAYS_TO_KEEP=5

# Database backups: how often (hours) and how many copies to keep
DB_BACKUP_INTERVAL_HOURS=24
DB_BACKUP_RETENTION_COUNT=5

# Optional: force a fixed shard count (testing only); unset = Discord's recommendation
# SHARD_COUNT=2

# Optional bot-list server-count updates (leave empty to disable)
TOPGG_TOKEN=
DISCORDBOTLIST_TOKEN=
DISCORDBOTLIST_BOT_ID=   # the discordbotlist.com URL slug, e.g. lastseen-2591
VOIDBOTS_TOKEN=

# Optional: every 10 minutes, write stats.json (server/user counts) and copy it
# with scp to a web server. Enable only on the production instance.
STATS_PUSH_ENABLED=false
STATS_SCP_DESTINATION=   # user@host:/remote/path
STATS_SSH_KEY=           # path to the SSH private key
```

On Linux, keep the stats-push SSH key inside `/opt/lastseen` (e.g. `/opt/lastseen/.ssh/`). The service can't read home folders, and scp looks for `known_hosts` in the service user's home, which is `/opt/lastseen`.

## Logging

- `logs/bot.log` is the current log. At midnight (UTC) it is renamed to `logs/YYYY-MM-DD.log`.
- Logs older than `DEBUG_LOGS_DAYS_TO_KEEP` days are deleted at startup and at each rotation.
- `logs/crash.log` records every abnormal exit, with the error.
- On Linux the same output also goes to the systemd journal (`journalctl -u lastseen`).

## Troubleshooting

### Bot not responding to commands
- Check the bot's permissions in your server and the allowed-channels setting (`/config` → View Config).
- New or changed commands can take a few minutes to appear in Discord.
- Check `logs/bot.log` for errors.

### Presence tracking not working
- Enable **PRESENCE INTENT** and **SERVER MEMBERS INTENT** in the Developer Portal.
- From 100 servers, Discord requires verification and intent approval for these intents to keep working.

### The service doesn't start or keeps stopping
- `sudo systemctl status lastseen` shows the exit code; see [Exit codes](#exit-codes).
- Exit code 2 means a configuration problem: check `.env` and the log.
- `status=203/EXEC` means the binary is missing or not executable: check `/opt/lastseen/lastseen` and its permissions.

### Database errors
- The bot needs write access to its folder (the database, its `-wal`/`-shm` files and `backups/`).
- Schema updates run automatically at startup.

### Scheduled reports not sending
- Check that the report channel exists and the bot can send messages there.
- Check the server timezone: the report hour is in that timezone.
- Look for "report" lines in the log.

## Project Structure

```
LastSeen-Go/
├── cmd/lastseen/main.go     # Entry point: startup, exit codes, crash.log
├── internal/
│   ├── bot/                 # Discord side (port of bot/ and cogs/)
│   │   ├── bot.go           #   client, gateway, shards, startup/shutdown
│   │   ├── tracking.go      #   presence, member and message tracking, background tasks
│   │   ├── commands.go      #   user commands (whois, lastseen, mystats, about...)
│   │   ├── search.go        #   /search
│   │   ├── stats_view.go    #   /user-stats dashboard
│   │   ├── watch.go         #   /watch alerts
│   │   ├── admin.go         #   /config panel, dialogs, quick setup, /help
│   │   ├── reports.go       #   scheduled reports
│   │   ├── listings.go      #   bot-list updates, stats push
│   │   ├── interact.go      #   buttons, menus, dialogs, command dispatch
│   │   ├── registry.go      #   command → handler table
│   │   ├── commands.json    #   slash command definitions synced to Discord
│   │   └── testdata/        #   reference output from the Python bot (golden tests)
│   ├── db/                  # SQLite database (same schema as the Python bot)
│   ├── config/              # .env loading
│   ├── locale/              # translations
│   ├── logx/                # logging with daily rotation
│   ├── pyfmt/               # Python-compatible number/text formatting
│   └── tzdb/                # timezone list and data (built into the binary)
├── locales/                 # translation files (one JSON per language; deploy with the binary)
├── deploy/
│   ├── install.sh           # Linux installer/updater (run with sudo)
│   └── lastseen.service     # systemd service
├── tools/gen_pyref.py       # regenerates the golden-test reference data from the Python bot
└── build.ps1                # builds the Windows and Linux folders in dist/
```

## Development

```bash
go vet ./...
```

```bash
go test ./...
```

The tests include **golden tests** that compare the Go bot with the Python bot. They cover the button layout of every view in every language, duration parsing, markdown escaping and member-state reconciliation, using reference data in `internal/bot/testdata/pyref.json`. If the Python bot changes, regenerate that file with the Python bot's virtual environment:

```bash
python tools/gen_pyref.py <path-to-LastSeen-Python>
```

## Privacy & Data

This bot stores:
- User IDs, usernames, nicknames
- Join dates, last seen timestamps, message counts (never message content)
- Role names
- Server settings

All data is stored locally in the SQLite database. The only data sent elsewhere is the server count, to bot lists you configure, and the optional stats push.

Members control their own data: `/forgetme` deletes it and opts out of tracking entirely, and `/no-watch` opts out of being a watch target while leaving activity tracking on.

## License

MIT License - See LICENSE file for details
