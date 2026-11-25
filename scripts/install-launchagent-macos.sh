#!/usr/bin/env bash
# Install/remove the macOS LaunchAgent that mounts qrypt at login.
#
#   scripts/install-launchagent-macos.sh install     # write plist + start now
#   scripts/install-launchagent-macos.sh uninstall   # stop + remove plist
#   scripts/install-launchagent-macos.sh status      # show launchd state + mounts
#
# Overridable via environment:
#   QRYPT_BIN      qrypt executable        (default: $HOME/bin/qrypt, then PATH)
#   QRYPT_CONFIG   config file             (default: $HOME/.qrypt/qrypt.toml)
#   QRYPT_LOG_DIR  launchd log directory   (default: $HOME/.qrypt/logs)
#   QRYPT_LABEL    launchd job label       (default: com.qrypt.mount)
#
# The job passes --stop-core and uses KeepAlive={SuccessfulExit=false}: it is
# restarted only when `qrypt mount` exits non-zero, so a deliberate Ctrl+C or
# external eject stays stopped. See docs/for-user/macos-autostart.md.
set -euo pipefail

LABEL="${QRYPT_LABEL:-com.qrypt.mount}"
CONFIG="${QRYPT_CONFIG:-$HOME/.qrypt/qrypt.toml}"
LOG_DIR="${QRYPT_LOG_DIR:-$HOME/.qrypt/logs}"
PLIST_DIR="$HOME/Library/LaunchAgents"
PLIST="$PLIST_DIR/$LABEL.plist"
DOMAIN="gui/$(id -u)"

die() {
  echo "error: $*" >&2
  exit 1
}

usage() {
  sed -n '2,17p' "$0" | sed 's/^# \{0,1\}//'
}

resolve_bin() {
  if [ -n "${QRYPT_BIN:-}" ]; then
    printf '%s\n' "$QRYPT_BIN"
  elif [ -x "$HOME/bin/qrypt" ]; then
    printf '%s\n' "$HOME/bin/qrypt"
  else
    command -v qrypt 2>/dev/null || die "qrypt executable not found; set QRYPT_BIN"
  fi
}

cmd_install() {
  [ "$(uname -s)" = "Darwin" ] || die "this installer is macOS-only"

  local bin
  bin="$(resolve_bin)"
  [ -x "$bin" ] || die "$bin is not executable"
  [ -f "$CONFIG" ] || die "config not found: $CONFIG"

  mkdir -p "$LOG_DIR" "$PLIST_DIR"

  cat >"$PLIST" <<PLIST_EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>$bin</string>
		<string>mount</string>
		<string>--config</string>
		<string>$CONFIG</string>
		<string>--stop-core</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>$LOG_DIR/launchd.out.log</string>
	<key>StandardErrorPath</key>
	<string>$LOG_DIR/launchd.err.log</string>
</dict>
</plist>
PLIST_EOF

  plutil -lint "$PLIST" >/dev/null || die "generated plist is invalid: $PLIST"

  # Drop any running copy first: its clean unmount has to finish before the new
  # process mounts the same point, otherwise the mount fails on a busy path.
  if launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; then
    launchctl bootout "$DOMAIN/$LABEL"
    for _ in $(seq 1 60); do
      launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1 || break
      sleep 0.5
    done
  fi

  # --stop-core asks the resident Core to shut down asynchronously; wait for
  # any running or older Core holding this config to exit before bootstrap.
  "$bin" core stop --config "$CONFIG" >/dev/null 2>&1 || true

  launchctl bootstrap "$DOMAIN" "$PLIST"
  sleep 1

  echo "installed: $PLIST"
  echo "started:   $LABEL -> $bin mount --config $CONFIG --stop-core"
  echo "logs:      $LOG_DIR/launchd.err.log"
}

cmd_uninstall() {
  if launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; then
    launchctl bootout "$DOMAIN/$LABEL" || die "launchctl bootout failed for $DOMAIN/$LABEL"
  fi
  rm -f "$PLIST"
  echo "removed: $PLIST (its mount was unmounted too)"
}

cmd_status() {
  if launchctl print "$DOMAIN/$LABEL" >/dev/null 2>&1; then
    launchctl print "$DOMAIN/$LABEL" |
      grep -E "^\s+(state|pid|runs|last exit code) = " || true
  else
    echo "not registered in launchd: $DOMAIN/$LABEL"
  fi
  [ -f "$PLIST" ] && echo "plist: $PLIST"
  echo "mounts:"
  mount | grep -iE "fuse-t:|macfuse:|\((fuse-t|macfuse)," || echo "  (none)"
}

case "${1:-}" in
  install)
    shift
    [ $# -eq 0 ] || die "install takes no arguments (use the environment variables)"
    cmd_install
    ;;
  uninstall)
    shift
    [ $# -eq 0 ] || die "uninstall takes no arguments"
    cmd_uninstall
    ;;
  status)
    shift
    [ $# -eq 0 ] || die "status takes no arguments"
    cmd_status
    ;;
  -h | --help | help)
    usage
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac
