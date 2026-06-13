#!/usr/bin/env bash
# Install / manage launchd agent for the Multica daemon on macOS.
#
# - Reads daemon_id from ~/.multica/daemon.id so the daemon re-registers
#   against the same runtime UUIDs after every reboot (no orphaned
#   runtimes piling up on the FE).
# - Logs to ~/.multica/daemon.log (rotated by daemon itself) and
#   ~/.multica/daemon.out (launchd stdout).
# - KeepAlive=true with throttle=10s so a crash auto-restarts but a
#   broken binary doesn't busy-loop.
#
# Usage:
#   ./scripts/multica-daemon-launchd.sh install   # install + load + start
#   ./scripts/multica-daemon-launchd.sh uninstall # stop + remove
#   ./scripts/multica-daemon-launchd.sh restart   # restart agent
#   ./scripts/multica-daemon-launchd.sh status    # show state + last log lines

set -euo pipefail

LABEL="com.multica.daemon"
PLIST_PATH="$HOME/Library/LaunchAgents/${LABEL}.plist"
BIN_PATH="${MULTICA_BIN:-$HOME/bin/multica}"
LOG_PATH="$HOME/.multica/daemon.log"
OUT_PATH="$HOME/.multica/daemon.out"
DAEMON_ID_PATH="$HOME/.multica/daemon.id"

die() {
  echo "error: $*" >&2
  exit 1
}

require_binary() {
  [[ -x "$BIN_PATH" ]] || die "multica binary not found at $BIN_PATH (set MULTICA_BIN env to override)"
}

write_plist() {
  mkdir -p "$(dirname "$PLIST_PATH")" "$HOME/.multica"
  cat > "$PLIST_PATH" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>${LABEL}</string>
    <key>ProgramArguments</key>
    <array>
        <string>${BIN_PATH}</string>
        <string>daemon</string>
        <string>start</string>
        <string>--foreground</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>ThrottleInterval</key>
    <integer>10</integer>
    <key>WorkingDirectory</key>
    <string>${HOME}</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>${HOME}</string>
        <key>PATH</key>
        <string>${HOME}/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:${HOME}/bin</string>
    </dict>
    <key>StandardOutPath</key>
    <string>${OUT_PATH}</string>
    <key>StandardErrorPath</key>
    <string>${LOG_PATH}</string>
</dict>
</plist>
EOF
  echo "wrote $PLIST_PATH"
}

bootstrap_uid() {
  # gui/$UID is the per-user GUI session; load via bootstrap so the
  # agent survives Terminal close. legacy `launchctl load -w` is
  # deprecated on macOS 11+ and emits warnings.
  echo "gui/$(id -u)"
}

kill_legacy_daemon() {
  # If a daemon was started by hand (nohup ... &) it holds :19514 and
  # blocks launchd's instance. Kill any matching PID before bootstrap.
  local pid
  pid=$(lsof -ti :19514 2>/dev/null || true)
  if [[ -n "${pid}" ]]; then
    echo "killing non-launchd daemon PID ${pid}"
    kill "${pid}" 2>/dev/null || true
    sleep 2
  fi
}

cmd_install() {
  require_binary
  if [[ ! -f "$DAEMON_ID_PATH" ]]; then
    echo "warn: ${DAEMON_ID_PATH} missing — daemon will register a NEW daemon_id on first start"
    echo "      back up daemon.id once it's generated to survive ~/.multica wipes"
  fi
  write_plist
  kill_legacy_daemon
  # bootout silently if already loaded, then bootstrap fresh
  launchctl bootout "$(bootstrap_uid)/${LABEL}" 2>/dev/null || true
  launchctl bootstrap "$(bootstrap_uid)" "$PLIST_PATH"
  launchctl enable "$(bootstrap_uid)/${LABEL}"
  echo "installed + loaded. tail -f ${LOG_PATH}"
}

cmd_uninstall() {
  launchctl bootout "$(bootstrap_uid)/${LABEL}" 2>/dev/null || true
  rm -f "$PLIST_PATH"
  echo "removed $PLIST_PATH"
}

cmd_restart() {
  launchctl kickstart -k "$(bootstrap_uid)/${LABEL}"
  echo "restarted ${LABEL}"
}

cmd_status() {
  local target="$(bootstrap_uid)/${LABEL}"
  if launchctl print "${target}" >/dev/null 2>&1; then
    echo "loaded: ${target}"
    launchctl print "${target}" | grep -E "^\s+(state|pid|last exit code|program)" | head
  else
    echo "not loaded"
  fi
  echo "---daemon_id---"
  cat "$DAEMON_ID_PATH" 2>/dev/null || echo "(missing)"
  echo
  echo "---last 10 log lines---"
  tail -10 "$LOG_PATH" 2>/dev/null || echo "(no log yet)"
}

case "${1:-}" in
  install)   cmd_install ;;
  uninstall) cmd_uninstall ;;
  restart)   cmd_restart ;;
  status)    cmd_status ;;
  *)
    cat <<USAGE
usage: $0 <install|uninstall|restart|status>

  install    write plist, kill legacy daemon, bootstrap launchd agent
  uninstall  bootout + remove plist
  restart    kickstart agent (apply binary update)
  status     loaded state + daemon_id + tail log

env:
  MULTICA_BIN  path to multica binary (default: ~/bin/multica)
USAGE
    exit 1
    ;;
esac
