#!/bin/sh
# MusicOps container entrypoint: Go core only, no Python.
#
# - Generates /config/web-password (32 characters, 0600) on first boot
#   and reuses it on restart, so the WebUI password survives restarts.
# - Exports MUSICOPS_APP_PASSWORD for the core (Basic auth
#   musicops/<password>), the vault key location, and a persistent HOME.
# - Prepends /opt/musicops/tools to PATH so the core finds bundled tools
#   via exec.LookPath (ffmpeg/ffprobe themselves come from the image's
#   /usr/bin, installed by apt).
# - execs the core; tini (PID 1) forwards SIGTERM and the core shuts
#   down gracefully on its own, so there is no supervisor loop here.
set -eu
umask 077

PASSWORD_FILE=/config/web-password
PROFILE=/config/profile
STATE_DIR="$PROFILE/state/native"
BACKUP_DIR="$PROFILE/state/native-backup"

mkdir -p "$STATE_DIR" "$BACKUP_DIR" /config/keys /config/home

if [ ! -f "$PASSWORD_FILE" ]; then
  tmp="$PASSWORD_FILE.tmp.$$"
  trap 'rm -f "$tmp"' EXIT
  # 24 random bytes -> exactly 32 base64 characters, no padding.
  openssl rand -base64 24 > "$tmp"
  chmod 0600 "$tmp"
  mv "$tmp" "$PASSWORD_FILE"
  trap - EXIT
  echo '首次启动：网页登录用户 musicops，密码保存在 /config/web-password。'
fi

# First line only; the file is a single line by construction.
IFS= read -r PASSWORD < "$PASSWORD_FILE" || true
if [ "${#PASSWORD}" -lt 16 ]; then
  echo "entrypoint: $PASSWORD_FILE holds a password shorter than 16 characters; the Go core requires MUSICOPS_APP_PASSWORD of 16+ characters" >&2
  exit 1
fi
export MUSICOPS_APP_PASSWORD="$PASSWORD"
unset PASSWORD

export MUSICOPS_VAULT_KEY_FILE=/config/keys/vault.key
export HOME=/config/home
export XDG_DATA_HOME=/config/home/.local/share

case ":$PATH:" in
  *":/opt/musicops:"*) ;;
  *) PATH="/opt/musicops:$PATH" ;;
esac
case ":$PATH:" in
  *":/opt/musicops/tools:"*) ;;
  *) PATH="/opt/musicops/tools:$PATH" ;;
esac
export PATH

exec musicops --state-dir "$STATE_DIR" --backup-dir "$BACKUP_DIR" --listen 0.0.0.0:8080
