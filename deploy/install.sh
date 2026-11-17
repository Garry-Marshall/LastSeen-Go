#!/bin/sh
# Installs or updates LastSeen on a Linux server (systemd).
#
# build.ps1 puts this script in dist/linux-amd64 together with the binary,
# locales/ and lastseen.service. Copy that whole folder to the server and run:
#
#   sudo sh linux-amd64/install.sh [name]
#
# name (default "lastseen") names the instance: its system user, its folder
# /opt/<name> and its service <name>.service. Use another name to run a second
# bot next to the first, e.g. a test bot: sudo sh linux-amd64/install.sh lastseen-dev
#
# Every run: installs the binary, locales/ and the systemd service into
# /opt/<name>, then (re)starts the bot.
# First run: also creates the system user and /opt/<name>.
#
# Put a .env and/or a database (e.g. lastseen_bot.db from the Python bot) in
# the folder next to this script to have them installed too. They are only
# installed when /opt/<name> doesn't have one yet: an existing .env or
# database is never overwritten.
#
# The script refuses to replace a service of the same name that it didn't
# install itself (such as the Python bot's). For local changes to the service,
# use "sudo systemctl edit <name>": this script rewrites
# /etc/systemd/system/<name>.service on every run.

set -eu

NAME=${1:-lastseen}
case "$NAME" in
    "" | -* | *[!a-z0-9-]*)
        echo "Invalid name \"$NAME\": use lowercase letters, digits and dashes, e.g. lastseen-dev." >&2
        exit 1
        ;;
esac
APP_DIR=/opt/$NAME
APP_USER=$NAME
SERVICE=$NAME
UNIT=/etc/systemd/system/$SERVICE.service
MARKER="Managed by LastSeen's install.sh"
SRC=$(cd "$(dirname "$0")" && pwd)

if [ "$(id -u)" -ne 0 ]; then
    echo "Run this as root: sudo sh $0 $*" >&2
    exit 1
fi
for f in lastseen locales lastseen.service; do
    if [ ! -e "$SRC/$f" ]; then
        echo "$SRC/$f is missing: copy the whole build folder (dist/linux-amd64)." >&2
        exit 1
    fi
done

# --- Never take over someone else's service --------------------------------
# Check both the unit systemd has loaded (wherever it lives) and our own path.
loaded=$(systemctl show -p FragmentPath --value "$SERVICE" 2>/dev/null || true)
for f in "$loaded" "$UNIT"; do
    if [ -n "$f" ] && [ -f "$f" ] && ! grep -q "$MARKER" "$f"; then
        echo "A $SERVICE service already exists ($f) that this script didn't install," >&2
        echo "for example the Python bot's. Nothing was changed." >&2
        echo >&2
        echo "To run this bot next to it, use another name:" >&2
        echo "  sudo sh $0 lastseen-dev" >&2
        echo "To replace it, stop the old service and set its file aside first:" >&2
        echo "  sudo systemctl disable --now $SERVICE" >&2
        echo "  sudo mv $f /root/$SERVICE.service.old" >&2
        echo "  sudo systemctl daemon-reload" >&2
        exit 1
    fi
done
if [ -f "$APP_DIR/main.py" ]; then
    echo "$APP_DIR looks like the Python bot's folder (it has main.py). Nothing was changed." >&2
    echo "Install this bot under another name, e.g.: sudo sh $0 lastseen-go" >&2
    exit 1
fi

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
# lastseen.service is written for the default name; adapt it to this one.
unit_tmp=$(mktemp)
trap 'rm -f "$unit_tmp"' EXIT
sed -e "s|/opt/lastseen|$APP_DIR|g" \
    -e "s|^User=lastseen\$|User=$APP_USER|" \
    -e "s|^Group=lastseen\$|Group=$APP_USER|" \
    "$SRC/lastseen.service" >"$unit_tmp"
if [ "$NAME" != lastseen ]; then
    sed -i "s|^Description=.*|& ($NAME)|" "$unit_tmp"
fi
if ! grep -q "$MARKER" "$unit_tmp"; then
    echo "$SRC/lastseen.service lacks the line \"$MARKER\"; not installing it." >&2
    exit 1
fi
if ! cmp -s "$unit_tmp" "$UNIT"; then
    install -o root -g root -m 644 "$unit_tmp" "$UNIT"
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
