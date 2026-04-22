#!/usr/bin/env bash
# Install Open Streamer as a systemd service on Linux.
#
# Usage:
#   sudo ./build/install.sh          # build, install binary + service, enable, start
#   sudo ./build/install.sh uninstall # stop, disable, remove binary + service + user
#
# Idempotent: safe to re-run after rebuilding the binary to upgrade in place.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN_SRC="${REPO_ROOT}/bin/open-streamer"
BIN_DST="/usr/local/bin/open-streamer"
UNIT_SRC="${REPO_ROOT}/build/open-streamer.service"
UNIT_DST="/etc/systemd/system/open-streamer.service"
DATA_DIR="/var/lib/open-streamer"
SERVICE_USER="open-streamer"

log() { printf '\033[36m[install]\033[0m %s\n' "$*"; }
err() { printf '\033[31m[error]\033[0m %s\n' "$*" >&2; }

require_root() {
  if [[ $EUID -ne 0 ]]; then
    err "must run as root (use sudo)"
    exit 1
  fi
}

require_linux() {
  if [[ "$(uname -s)" != "Linux" ]]; then
    err "systemd installer only supports Linux; run 'make build && ./bin/open-streamer' on other platforms"
    exit 1
  fi
}

cmd_install() {
  require_root
  require_linux

  if [[ ! -x "$BIN_SRC" ]]; then
    log "binary not found at $BIN_SRC — running 'make build'"
    (cd "$REPO_ROOT" && make build)
  fi

  if ! id -u "$SERVICE_USER" >/dev/null 2>&1; then
    log "creating system user $SERVICE_USER"
    useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
  fi

  # Add to GPU groups if they exist, so VAAPI / NVENC device nodes are accessible.
  for grp in video render; do
    if getent group "$grp" >/dev/null 2>&1; then
      usermod -a -G "$grp" "$SERVICE_USER"
    fi
  done

  log "installing binary → $BIN_DST"
  install -m 0755 "$BIN_SRC" "$BIN_DST"

  log "creating data directory → $DATA_DIR"
  install -d -o "$SERVICE_USER" -g "$SERVICE_USER" -m 0755 "$DATA_DIR"

  log "installing systemd unit → $UNIT_DST"
  install -m 0644 "$UNIT_SRC" "$UNIT_DST"

  systemctl daemon-reload
  systemctl enable open-streamer
  systemctl restart open-streamer

  log "done — check status with: systemctl status open-streamer"
  log "follow logs with:           journalctl -u open-streamer -f"
}

cmd_uninstall() {
  require_root
  require_linux

  if systemctl list-unit-files open-streamer.service >/dev/null 2>&1; then
    systemctl disable --now open-streamer || true
  fi
  rm -f "$UNIT_DST" "$BIN_DST"
  systemctl daemon-reload

  if id -u "$SERVICE_USER" >/dev/null 2>&1; then
    userdel "$SERVICE_USER" || true
  fi

  log "uninstalled binary, service, and user — data dir $DATA_DIR left intact"
  log "remove data dir manually if desired: rm -rf $DATA_DIR"
}

case "${1:-install}" in
  install)   cmd_install ;;
  uninstall) cmd_uninstall ;;
  *)         err "unknown command: $1 (use 'install' or 'uninstall')"; exit 1 ;;
esac
