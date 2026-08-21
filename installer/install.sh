#!/bin/sh
# Relay agent installer. Served by relayd at /install.sh; run as:
#   curl -fsSL http://<server>:7460/install.sh | sh -s -- --join TOKEN@HOST:PORT [--name NAME]
#
# Installs the relayd binary for this platform from the same server, then
# joins the fleet and registers a system service (systemd or launchd).
set -eu

JOIN=""
NAME=""
SERVER_URL=""

while [ $# -gt 0 ]; do
  case "$1" in
    --join) JOIN="$2"; shift 2 ;;
    --name) NAME="$2"; shift 2 ;;
    --server-url) SERVER_URL="$2"; shift 2 ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

[ -n "$JOIN" ] || { echo "usage: install.sh --join TOKEN@HOST:PORT [--name NAME]" >&2; exit 2; }

# Derive the HTTP url (binaries) from the gRPC host in the join string
# (strip the #fingerprint suffix first).
if [ -z "$SERVER_URL" ]; then
  HOSTPORT="${JOIN#*@}"
  HOSTPORT="${HOSTPORT%%#*}"
  HOST="${HOSTPORT%:*}"
  SERVER_URL="http://${HOST}:7460"
fi
SERVER_ADDR="${JOIN#*@}"
SERVER_ADDR="${SERVER_ADDR%%#*}"

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"
case "$ARCH" in
  x86_64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) echo "unsupported arch: $ARCH" >&2; exit 1 ;;
esac

ASSET="relayd-${OS}-${ARCH}"
BIN_DIR="/usr/local/bin"
[ -w "$BIN_DIR" ] || BIN_DIR="$HOME/.local/bin"
mkdir -p "$BIN_DIR"

echo "→ fetching $ASSET from $SERVER_URL"
curl -fsSL "$SERVER_URL/v1/install/$ASSET" -o "$BIN_DIR/relayd"
chmod +x "$BIN_DIR/relayd"
echo "✓ installed $BIN_DIR/relayd ($("$BIN_DIR/relayd" version))"

STATE_DIR="$HOME/.relay/agent"
NAME_FLAG=""
[ -n "$NAME" ] && NAME_FLAG="--name $NAME"

# Join with a bounded, verifiable step: --join-only registers, stores
# credentials (incl. the pinned server cert), and exits 0 on success.
echo "→ joining fleet"
if ! "$BIN_DIR/relayd" agent --join "$JOIN" $NAME_FLAG \
      --state-dir "$STATE_DIR" --join-only; then
  echo "✗ join failed. Check the output above." >&2
  exit 1
fi
echo "✓ joined; installing service"

if [ "$OS" = "linux" ] && command -v systemctl >/dev/null 2>&1; then
  SERVICE_DIR="$HOME/.config/systemd/user"
  mkdir -p "$SERVICE_DIR"
  cat > "$SERVICE_DIR/relay-agent.service" <<EOF
[Unit]
Description=Relay machine agent
After=network-online.target docker.service

[Service]
ExecStart=$BIN_DIR/relayd agent --server $SERVER_ADDR --state-dir $STATE_DIR
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
EOF
  systemctl --user daemon-reload
  systemctl --user enable --now relay-agent.service
  echo "✓ systemd user service relay-agent running"
  echo "  (for headless machines: sudo loginctl enable-linger $USER)"
elif [ "$OS" = "darwin" ]; then
  PLIST="$HOME/Library/LaunchAgents/dev.relay.agent.plist"
  mkdir -p "$HOME/Library/LaunchAgents"
  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>dev.relay.agent</string>
  <key>ProgramArguments</key><array>
    <string>$BIN_DIR/relayd</string><string>agent</string>
    <string>--server</string><string>$SERVER_ADDR</string>
    <string>--state-dir</string><string>$STATE_DIR</string>
  </array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
</dict></plist>
EOF
  launchctl unload "$PLIST" 2>/dev/null || true
  launchctl load "$PLIST"
  echo "✓ launchd agent dev.relay.agent running"
else
  echo "! no service manager found. Run manually:"
  echo "  $BIN_DIR/relayd agent --server $SERVER_ADDR --state-dir $STATE_DIR"
fi
