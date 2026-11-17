#!/bin/sh
# Installs or updates LastSeen on a Linux server (systemd).
#
# build.ps1 puts this script in dist/linux-amd64 together with the binary,
# locales/ and lastseen.service. Copy that whole folder to the server and run:
#
#   sudo sh linux-amd64/install.sh
#
# Every run: installs the binary, locales/ and the systemd service into
# /opt/lastseen, then (re)starts the bot.
# First run: also creates the "lastseen" system user and /opt/lastseen.
#
# Put a .env and/or a database (e.g. lastseen_bot.db from the Python bot) in
# the folder next to this script to have them installed too. They are only
# installed when /opt/lastseen doesn't have one yet: an existing .env or
# database is never overwritten.
#
# For local changes to the service, use "sudo systemctl edit lastseen": this
# script replaces /etc/systemd/system/lastseen.service on every run.

set -eu

APP_DIR=/opt/lastseen
APP_USER=lastseen
SERVICE=lastseen
UNIT=/etc/systemd/system/$SERVICE.service
SRC=$(cd "$(dirname "$0")" && pwd)

if [ "$(id -u)" -ne 0 ]; then
    echo "Run this as root: sudo sh $0" >&2
    exit 1
fi
for f in lastseen locales lastseen.service; do
    if [ ! -e "$SRC/$f" ]; then
        echo "$SRC/$f is missing: copy the whole build folder (dist/linux-amd64)." >&2
        exit 1
    fi
done

# --- User and folder (first run) --------------------------------------------
if ! id "$APP_USER" >/dev/null 2>&1; then
    useradd --system --home-dir "$APP_DIR" --no-create-home \
        --shell /usr/sbin/nologin "$APP_USER"
    echo "Created system user $APP_USER"
fi
if [ ! -d "$APP_DIR" ]; then
    install -d -o "$APP_USER" -g "$APP_USER" -m 750 "$APP_DIR"
    echo "Created $APP_DIR"
fi

# --- Binary and translations (every run) ------------------------------------
# Install next to the old binary and check it runs here (right architecture)
# before swapping it in. Renaming over a running binary is fine; the restart
# below switches to the new version.
install -o "$APP_USER" -g "$APP_USER" -m 755 "$SRC/lastseen" "$APP_DIR/lastseen.new"
if ! version=$("$APP_DIR/lastseen.new" -version); then
    rm -f "$APP_DIR/lastseen.new"
    echo "The new binary doesn't run on this server (wrong build target?). Nothing was changed." >&2
    exit 1
fi
mv "$APP_DIR/lastseen.new" "$APP_DIR/lastseen"

# Swap in a fresh copy so translations that were removed don't linger.
rm -rf "$APP_DIR/locales.new"
cp -r "$SRC/locales" "$APP_DIR/locales.new"
chown -R "$APP_USER:$APP_USER" "$APP_DIR/locales.new"
chmod -R u=rwX,g=rX,o= "$APP_DIR/locales.new"
rm -rf "$APP_DIR/locales"
mv "$APP_DIR/locales.new" "$APP_DIR/locales"
echo "Installed $version"

# --- .env and database (only if /opt/lastseen has none yet) -----------------
if [ -f "$SRC/.env" ]; then
    if [ -f "$APP_DIR/.env" ]; then
        echo "Kept the existing $APP_DIR/.env (the .env next to this script was not used)."
    else
        install -o "$APP_USER" -g "$APP_USER" -m 600 "$SRC/.env" "$APP_DIR/.env"
        echo "Installed .env"
    fi
fi

for db in "$SRC"/*.db; do
    [ -f "$db" ] || continue # no .db files: the pattern stays unexpanded
    name=$(basename "$db")
    if [ -e "$APP_DIR/$name" ]; then
        echo "Kept the existing $APP_DIR/$name. To replace it, stop the bot," \
            "move that file (and its -wal/-shm files) away, and run this again."
        continue
    fi
    # Moving to a new server: copy the database with its write-ahead log.
    if systemctl is-active --quiet "$SERVICE"; then
        echo "Stopping $SERVICE to install the database..."
        systemctl stop "$SERVICE"
    fi
    # A leftover -wal/-shm from another database must not be applied to this one.
    rm -f "$APP_DIR/$name-wal" "$APP_DIR/$name-shm"
    install -o "$APP_USER" -g "$APP_USER" -m 640 "$db" "$APP_DIR/$name"
    if [ -f "$db-wal" ]; then
        install -o "$APP_USER" -g "$APP_USER" -m 640 "$db-wal" "$APP_DIR/$name-wal"
    fi
    echo "Installed database $name"
done

# --- systemd service (every run) --------------------------------------------
if ! cmp -s "$SRC/lastseen.service" "$UNIT"; then
    install -o root -g root -m 644 "$SRC/lastseen.service" "$UNIT"
    systemctl daemon-reload
    echo "Installed $UNIT"
fi
systemctl enable --quiet "$SERVICE"

# --- Start ------------------------------------------------------------------
if [ ! -f "$APP_DIR/.env" ]; then
    echo
    echo "No $APP_DIR/.env yet, so the bot was not started. Either:"
    echo "  - put your .env next to this script and run it again, or"
    echo "  - write it in place:  sudo -u $APP_USER nano $APP_DIR/.env"
    echo "    then start the bot: sudo systemctl start $SERVICE"
    exit 0
fi

systemctl restart "$SERVICE"
sleep 3
if systemctl is-active --quiet "$SERVICE"; then
    echo
    echo "LastSeen is running. Follow its log with: sudo journalctl -u $SERVICE -f"
else
    echo
    echo "LastSeen did not stay up. Last log lines:" >&2
    journalctl -u "$SERVICE" -n 20 --no-pager >&2 || true
    exit 1
fi
echo "This folder ($SRC) is no longer needed and may contain your .env: rm -r $SRC"
