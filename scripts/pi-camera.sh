#!/usr/bin/env bash
# Adds the Raspberry Pi camera to Maze Remote as its own machine ("Pi Camera").
# Runs as a system service, so it starts at boot — no desktop needed.
# Run it on the Pi:   bash ~/maze-remote/scripts/pi-camera.sh
set -euo pipefail

DIR="$HOME/maze-remote-agent"
CONF="$DIR/maze-remote-camera.json"
ENV_FILE="$HOME/maze-remote/server/.env"
URL="https://github.com/mazijoni/maze-remote/releases/latest/download/maze-remote-agent-linux-arm64"

echo "== Maze Remote: Pi camera setup =="

if [ "$(uname -m)" != "aarch64" ]; then
  echo "This needs 64-bit Raspberry Pi OS (uname -m says $(uname -m))." >&2
  exit 1
fi

echo "-> Installing the camera tools and ffmpeg"
sudo apt-get update -qq
sudo apt-get install -y -qq rpicam-apps ffmpeg >/dev/null 2>&1 || sudo apt-get install -y -qq libcamera-apps ffmpeg >/dev/null

CAM_CMD="$(command -v rpicam-vid || command -v libcamera-vid || true)"
if [ -z "$CAM_CMD" ]; then
  echo "Couldn't find rpicam-vid after installing rpicam-apps." >&2
  exit 1
fi

echo "-> Looking for the camera"
LIST="$("${CAM_CMD/-vid/-hello}" --list-cameras 2>&1 || true)"
if ! echo "$LIST" | grep -qE '^\s*0 :'; then
  echo "$LIST"
  echo
  echo "No camera found. Check the ribbon cable (contacts facing the right way, latch closed)," >&2
  echo "then reboot and run this script again." >&2
  exit 1
fi
echo "$LIST" | grep -E '^\s*[0-9]+ :' | head -3

echo "-> Downloading the newest agent"
mkdir -p "$DIR"
# always fetch the newest one (older agents don't know camera mode); download
# next to it and rename, since a running program can't be overwritten in place
curl -fsSL "$URL" -o "$DIR/maze-remote-agent.new"
chmod +x "$DIR/maze-remote-agent.new"
mv -f "$DIR/maze-remote-agent.new" "$DIR/maze-remote-agent"

if [ ! -f "$CONF" ]; then
  SECRET=""
  [ -f "$ENV_FILE" ] && SECRET="$(grep -E '^MR_HOST_SECRET=' "$ENV_FILE" | cut -d= -f2-)"
  [ -z "$SECRET" ] && read -rp "Paste the MR_HOST_SECRET from your server: " SECRET
  cat > "$CONF" <<EOF
{
  "server": "ws://127.0.0.1:8080/ws",
  "secret": "$SECRET",
  "id": "pi-camera",
  "name": "Pi Camera",
  "encoder": "x264",
  "fps": 30,
  "bitrate": "3M",
  "camera": { "width": 1280, "height": 720, "fps": 30, "rotation": 0, "command": "$CAM_CMD" }
}
EOF
  echo "-> Wrote $CONF"
fi

echo "-> Installing the service"
sudo tee /etc/systemd/system/maze-remote-camera.service >/dev/null <<EOF
[Unit]
Description=Maze Remote camera agent
After=network-online.target maze-remote.service
Wants=network-online.target

[Service]
User=$USER
WorkingDirectory=$DIR
ExecStart=$DIR/maze-remote-agent -config $CONF
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
sudo systemctl daemon-reload
sudo systemctl enable --now maze-remote-camera >/dev/null 2>&1
sudo systemctl restart maze-remote-camera
sleep 3
journalctl -u maze-remote-camera -n 4 --no-pager -o cat || true
echo
echo "Done. \"Pi Camera\" should now be on the website."
echo "Picture upside down? Set \"rotation\": 180 in $CONF, then: sudo systemctl restart maze-remote-camera"
