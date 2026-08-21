#!/bin/sh
# Registers relayd server and/or agent as launchd user services on macOS,
# replacing the leave-a-terminal-open workflow. Run from the repo root:
#
#   scripts/install-mac-services.sh server        # this Mac hosts the control plane
#   scripts/install-mac-services.sh agent         # this Mac is a fleet machine
#   scripts/install-mac-services.sh server agent  # both (typical single-Mac setup)
#
# Services start at login and restart on crash. Logs: ~/.relay/log/*.log
# Manage:  launchctl kickstart -k gui/$UID/dev.relay.server   (restart)
#          launchctl bootout gui/$UID/dev.relay.server        (stop+remove)
set -eu

REPO="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$REPO/relayd/bin/relayd"
[ -x "$BIN" ] || { echo "build first: (cd relayd && go build -o bin/relayd ./cmd/relayd)" >&2; exit 1; }

LOG_DIR="$HOME/.relay/log"
AGENTS_DIR="$HOME/Library/LaunchAgents"
mkdir -p "$LOG_DIR" "$AGENTS_DIR"

install_one() {
  name="$1"; shift
  plist="$AGENTS_DIR/dev.relay.$name.plist"
  {
    printf '<?xml version="1.0" encoding="UTF-8"?>\n'
    printf '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n'
    printf '<plist version="1.0"><dict>\n'
    printf '  <key>Label</key><string>dev.relay.%s</string>\n' "$name"
    printf '  <key>ProgramArguments</key><array>\n'
    printf '    <string>%s</string>\n' "$BIN"
    for arg in "$@"; do printf '    <string>%s</string>\n' "$arg"; done
    printf '  </array>\n'
    printf '  <key>RunAtLoad</key><true/>\n'
    printf '  <key>KeepAlive</key><true/>\n'
    printf '  <key>StandardOutPath</key><string>%s/%s.log</string>\n' "$LOG_DIR" "$name"
    printf '  <key>StandardErrorPath</key><string>%s/%s.log</string>\n' "$LOG_DIR" "$name"
    printf '  <key>EnvironmentVariables</key><dict>\n'
    printf '    <key>PATH</key><string>/usr/local/bin:/opt/homebrew/bin:/usr/bin:/bin</string>\n'
    printf '  </dict>\n'
    printf '</dict></plist>\n'
  } > "$plist"
  launchctl bootout "gui/$(id -u)/dev.relay.$name" 2>/dev/null || true
  launchctl bootstrap "gui/$(id -u)" "$plist"
  echo "✓ dev.relay.$name running (logs: $LOG_DIR/$name.log)"
}

for what in "$@"; do
  case "$what" in
    server) install_one server server ;;
    agent)
      [ -f "$HOME/.relay/agent/credentials.json" ] || {
        echo "agent has not joined yet. Run the \`relay connect\` join line once first." >&2
        exit 1
      }
      # Server address comes from the stored credentials.
      addr="$(python3 -c 'import json,os;print(json.load(open(os.path.expanduser("~/.relay/agent/credentials.json")))["server_addr"])')"
      install_one agent agent --server "$addr" ;;
    *) echo "usage: $0 [server] [agent]" >&2; exit 2 ;;
  esac
done
