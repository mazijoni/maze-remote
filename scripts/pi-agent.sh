#!/usr/bin/env bash
# Sets up the Maze Remote agent on a Raspberry Pi (64-bit Raspberry Pi OS with desktop),
# so the Pi's own screen shows up on the website next to your PC.
# Run it on the Pi:   bash ~/maze-remote/scripts/pi-agent.sh
set -euo pipefail

DIR="$HOME/maze-remote-agent"
ENV_FILE="$HOME/maze-remote/server/.env"
URL="https://github.com/mazijoni/maze-remote/releases/latest/download/maze-remote-agent-linux-arm64"

echo "== Maze Remote: Pi agent setup =="

if [ "$(uname -m)" != "aarch64" ]; then
  echo "This needs 64-bit Raspberry Pi OS (uname -m says $(uname -m))." >&2
  exit 1
fi

echo "-> Installing ffmpeg and xdotool (screen capture + mouse/keyboard)"
sudo apt-get update -qq
sudo apt-get install -y -qq ffmpeg xdotool >/dev/null

echo "-> Downloading the agent"
mkdir -p "$DIR"
curl -fsSL "$URL" -o "$DIR/maze-remote-agent"
chmod +x "$DIR/maze-remote-agent"

if [ ! -f "$DIR/maze-remote.json" ]; then
  SECRET=""
  if [ -f "$ENV_FILE" ]; then
    SECRET="$(grep -E '^MR_HOST_SECRET=' "$ENV_FILE" | cut -d= -f2-)"
  fi
  if [ -z "$SECRET" ]; then
    read -rp "Paste the MR_HOST_SECRET from your server: " SECRET
  fi
  # The server runs on this same Pi, so talk to it locally.
  cat > "$DIR/maze-remote.json" <<EOF
{
  "server": "ws://127.0.0.1:8080/ws",
  "secret": "$SECRET",
  "name": "Raspberry Pi",
  "encoder": "x264",
  "fps": 30,
  "bitrate": "6M"
}
EOF
  echo "-> Wrote $DIR/maze-remote.json"
fi

echo "-> Starting the agent automatically when the desktop starts"
mkdir -p "$HOME/.config/autostart"
cat > "$HOME/.config/autostart/maze-remote-agent.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=Maze Remote agent
Exec=sh -c 'cd "$DIR" && ./maze-remote-agent >> "$DIR/agent.log" 2>&1'
X-GNOME-Autostart-enabled=true
NoDisplay=true
EOF

SESSION="${XDG_SESSION_TYPE:-}"
if [ -z "$SESSION" ] && command -v loginctl >/dev/null; then
  SESSION="$(loginctl show-session "$(loginctl | awk -v u="$USER" '$3==u{print $1; exit}')" -p Type --value 2>/dev/null || true)"
fi

echo
if [ "$SESSION" = "x11" ]; then
  echo "Done. Starting the agent now..."
  (cd "$DIR" && DISPLAY="${DISPLAY:-:0}" nohup ./maze-remote-agent >> "$DIR/agent.log" 2>&1 &)
  sleep 2; tail -n 3 "$DIR/agent.log" || true
  echo "The Pi should now appear on the website."
else
  echo "Almost done. Your desktop uses Wayland, but screen capture and mouse control need X11:"
  echo "  1. Switch to X11:  sudo raspi-config nonint do_wayland W1"
  echo "     (or: sudo raspi-config -> Advanced Options -> Wayland -> X11)"
  echo "  2. Reboot:  sudo reboot"
  echo "After the reboot the agent starts by itself and the Pi appears on the website."
fi
echo "Agent log: $DIR/agent.log"
