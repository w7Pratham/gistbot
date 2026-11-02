#!/usr/bin/env bash
# Host install for Gistbot.
#
# Run from the repo root with sudo:
#   sudo bash scripts/host-install.sh
#
# Expects two filled-in env files next to the repo root (not committed):
#   ./gistbot.env        TELEGRAM_BOT_TOKEN, OWNER_USER_ID, WORK_DIR, OPENCODE_BIN,
#                        OPENCODE_API_URL, OPENCODE_API_PASSWORD, ...
#   ./opencode-api.env   OPENCODE_PASSWORD (same value as OPENCODE_API_PASSWORD)
#
# Idempotent: safe to re-run. Does not touch git state.
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BOT_USER=gistbot
BOT_HOME=/home/$BOT_USER
BOT_ENV_SRC="$REPO/gistbot.env"
API_ENV_SRC="$REPO/opencode-api.env"
OPENCODE_SRC="${OPENCODE_SRC:-$(command -v opencode || true)}"

die() { echo "ERROR: $*" >&2; exit 1; }
say() { echo "==> $*"; }

[ "$(id -u)" -eq 0 ] || die "must run as root: sudo bash scripts/host-install.sh"
id "$BOT_USER" >/dev/null 2>&1 || die "user $BOT_USER does not exist"

for f in "$BOT_ENV_SRC" "$API_ENV_SRC"; do
  [ -f "$f" ] || die "missing $f (copy .env.example and fill it in)"
done

# Values must be non-empty, not the example placeholders.
grep -qE '^TELEGRAM_BOT_TOKEN=.+' "$BOT_ENV_SRC" || die "TELEGRAM_BOT_TOKEN empty in $BOT_ENV_SRC"
grep -qE '^OWNER_USER_ID=[0-9]+' "$BOT_ENV_SRC" || die "OWNER_USER_ID missing/not numeric in $BOT_ENV_SRC"
grep -qE '^OPENCODE_API_PASSWORD=.+' "$BOT_ENV_SRC" || die "OPENCODE_API_PASSWORD empty in $BOT_ENV_SRC"
grep -qE '^OPENCODE_PASSWORD=.+' "$API_ENV_SRC" || die "OPENCODE_PASSWORD empty in $API_ENV_SRC"

say "building bin/gistbot"
( cd "$REPO" && make build )

say "installing directories"
install -d -o "$BOT_USER" -g "$BOT_USER" -m 0755 "$BOT_HOME/bin" \
  "$BOT_HOME/.config/systemd/user" "$BOT_HOME/.config/gistbot" "$BOT_HOME/gists" \
  "$BOT_HOME/.local/state/gistbot/logs" "$BOT_HOME/.opencode/bin"

say "installing binary"
install -o "$BOT_USER" -g "$BOT_USER" -m 0755 "$REPO/bin/gistbot" "$BOT_HOME/bin/gistbot"

say "installing opencode binary for $BOT_USER"
if [ -n "$OPENCODE_SRC" ] && [ -x "$OPENCODE_SRC" ] && [ -x "$BOT_HOME/.opencode/bin/opencode" ] && cmp -s "$OPENCODE_SRC" "$BOT_HOME/.opencode/bin/opencode"; then
  echo "    already up to date"
else
  [ -n "$OPENCODE_SRC" ] && [ -x "$OPENCODE_SRC" ] || die "opencode binary not found at ${OPENCODE_SRC:-(empty)} (set OPENCODE_SRC=...)"
  install -o "$BOT_USER" -g "$BOT_USER" -m 0755 "$OPENCODE_SRC" "$BOT_HOME/.opencode/bin/opencode"
fi

say "installing env files (0600, $BOT_USER only)"
install -o "$BOT_USER" -g "$BOT_USER" -m 0600 "$BOT_ENV_SRC" "$BOT_HOME/.config/gistbot/gistbot.env"
install -o "$BOT_USER" -g "$BOT_USER" -m 0600 "$API_ENV_SRC" "$BOT_HOME/.config/gistbot/opencode-api.env"

say "installing systemd user units"
install -o "$BOT_USER" -g "$BOT_USER" -m 0644 "$REPO"/deploy/*.service "$BOT_HOME/.config/systemd/user/"

say "enabling linger for $BOT_USER"
loginctl enable-linger "$BOT_USER"

say "starting units"
UID_BOT="$(id -u "$BOT_USER")"
sudo -u "$BOT_USER" -H env XDG_RUNTIME_DIR="/run/user/$UID_BOT" bash -lc '
  systemctl --user daemon-reload
  systemctl --user enable --now opencode-gist.service gistbot.service
  sleep 2
  systemctl --user --no-pager --lines=15 status opencode-gist.service gistbot.service || true
'

say "done. tail logs with:"
echo "    sudo -u $BOT_USER -H env XDG_RUNTIME_DIR=/run/user/$UID_BOT journalctl --user -u gistbot -f"
