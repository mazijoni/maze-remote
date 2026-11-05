<p align="center">
  <img src="web/logo.svg" width="90" alt="">
</p>
<h1 align="center">MAZE_REMOTE</h1>
<p align="center">Remote desktop with nothing to install on the viewing side — just a browser.<br>
A <a href="https://github.com/mazijoni">MAZE_DEVELOPMENT</a> project.</p>

<p align="center"><a href="https://mazijoni.github.io/maze-remote/?demo"><b>▶ Try the demo</b></a></p>

---

## How it works

```
 Browser ──(HTTPS/WS signaling)──►  Raspberry Pi  ◄──(WebSocket)── Agent on your PC
    ▲                                                                   │
    └───────────── WebRTC: H.264 video ⬅  / mouse + keyboard ➡ ──────────┘
                        (direct, peer-to-peer)
```

| Part | Where it runs | What it does |
| --- | --- | --- |
| `agent/` | The PC you control (Windows) | Captures the screen with ffmpeg (Desktop Duplication), encodes H.264, streams it over WebRTC ([Pion](https://github.com/pion/webrtc)), applies input with `SendInput`. |
| `server/` | Raspberry Pi 5 | Login, list of online PCs, WebRTC signaling. Also serves the website. Video does **not** pass through it. |
| `web/` | Any browser | The control panel and viewer. Also deployed to GitHub Pages with a demo mode. |

## Setup

### 1. Server on the Pi

```bash
# Node 18+ (Raspberry Pi OS: sudo apt install nodejs npm)
git clone https://github.com/mazijoni/maze-remote ~/maze-remote
cd ~/maze-remote/server && npm install

# Pick a website password and generate a secret for your agents
cat > .env <<EOF
MR_PASSWORD=choose-a-strong-password
MR_HOST_SECRET=$(openssl rand -hex 24)
PORT=8080
EOF
cat .env   # copy MR_HOST_SECRET for the agent

# Run it as a service so it starts on boot
sudo cp maze-remote.service /etc/systemd/system/
sudo systemctl enable --now maze-remote
```

Open `http://<pi-ip>:8080` and sign in with `MR_PASSWORD`.

### 2. Agent on the Windows PC

1. Install ffmpeg (6.0+): `winget install Gyan.FFmpeg`
2. Grab `maze-remote-agent.exe` from the [Actions artifacts / Releases](../../releases), or build it: `cd agent && go build`
3. Put a `maze-remote.json` next to it (see [`agent/maze-remote.example.json`](agent/maze-remote.example.json)):

```json
{
  "server": "ws://raspberrypi.local:8080/ws",
  "secret": "the MR_HOST_SECRET from the Pi",
  "name": "MAZE-PC",
  "encoder": "nvenc",
  "fps": 60,
  "bitrate": "8M"
}
```

4. Run `maze-remote-agent.exe`. The PC shows up on the website within seconds.

| Encoder | GPU | Notes |
| --- | --- | --- |
| `x264` | none (CPU) | Works everywhere, default |
| `nvenc` | NVIDIA | Lowest latency, frames stay on the GPU |
| `amf` | AMD | |
| `qsv` | Intel | |

**Multiple monitors:** in a session, the screen button in the toolbar lists every monitor plus **All screens** (all of them stitched together the way they're arranged on your desk). The website remembers your choice per PC. To pick the starting screen on the agent side, set `"monitor": 1` (second screen) or `"monitor": -1` (all screens).

Other flags: `-test` streams a test pattern (two fake screens) and only logs input.

### Add the Raspberry Pi itself

The Pi can show up as a machine too. On the Pi (64-bit Raspberry Pi OS with desktop):

```bash
cd ~/maze-remote && git pull
bash scripts/pi-agent.sh
```

It installs ffmpeg and xdotool, downloads the ARM agent, reads the host secret from the server's `.env`, and starts the agent with the desktop. Screen capture needs an **X11** desktop; if the Pi uses Wayland (the default), the script tells you how to switch (`sudo raspi-config nonint do_wayland W1`, then reboot). The Pi 5 has no hardware H.264 encoder, so it streams with x264 at 30 fps.

### 3. Using it from outside your home

The easiest secure option is **[Tailscale](https://tailscale.com)** on the Pi, the PC and your phone/laptop — then use the Pi's Tailscale address. For HTTPS (needed for clipboard and keyboard-lock features), run `sudo tailscale serve --bg 8080` on the Pi and open the `https://…ts.net` URL it prints.

If a direct connection can't be made (strict networks), add a TURN server on the Pi (`sudo apt install coturn`) and set `MR_ICE=stun:stun.l.google.com:19302,turn:user:pass@your-pi:3478` in `.env`.

## About the GitHub Pages site

[mazijoni.github.io/maze-remote](https://mazijoni.github.io/maze-remote/) is a preview with **demo mode** (a fake desktop that reacts to your mouse and keyboard). For real sessions, use the site served by the Pi: browsers block an HTTPS page (Pages) from talking to a plain-HTTP server on your LAN. If your Pi has HTTPS through `tailscale serve`, the Pages site can connect to it too.

## Security notes

- The website password and host secret are the only things standing between the internet and your PC. Use long random values and don't expose port 8080 directly to the internet — use Tailscale or a VPN.
- Login is rate-limited; tokens expire after 7 days.
- WebRTC media and input are encrypted end to end (DTLS-SRTP).

## Roadmap

- [ ] Tray icon on the host with a "someone is connected" indicator
- [ ] Clipboard sync
- [ ] Multi-monitor switching from the toolbar
- [ ] Run as a Windows service (control the login screen / UAC)
- [ ] File transfer
