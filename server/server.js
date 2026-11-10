// Maze Remote — signaling server
// Serves the web client, handles login, keeps a registry of online hosts,
// and relays WebRTC signaling between browsers and host agents.
// Video never flows through here: browser and host connect peer-to-peer.

import http from "node:http";
import fs from "node:fs";
import path from "node:path";
import crypto from "node:crypto";
import { fileURLToPath } from "node:url";
import { WebSocketServer } from "ws";
import { createStats } from "./stats.js";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

// ---------- config ----------
const PORT = Number(process.env.PORT || 8080);
const PASSWORD = process.env.MR_PASSWORD || "";
const HOST_SECRET = process.env.MR_HOST_SECRET || "";
const WEB_DIR = process.env.MR_WEB_DIR || path.resolve(__dirname, "../web");
const TOKEN_TTL_MS = 1000 * 60 * 60 * 24 * 7; // 7 days
// Comma-separated ICE servers handed to browsers and hosts, e.g. "stun:stun.l.google.com:19302,turn:user:pass@pi.local:3478"
const ICE = (process.env.MR_ICE || "stun:stun.l.google.com:19302").split(",").filter(Boolean);

if (!PASSWORD || !HOST_SECRET) {
  console.error("Set MR_PASSWORD (website login) and MR_HOST_SECRET (agent key) before starting.");
  console.error("Example: MR_PASSWORD=hunter2 MR_HOST_SECRET=$(openssl rand -hex 24) npm start");
  process.exit(1);
}

// Optional Cloudflare TURN relay, for when browser and PC are on networks that
// can't reach each other directly (mobile data, strict Wi-Fi). Credentials are
// short-lived, so we mint them from the Cloudflare API and refresh them.
const CF_TURN_KEY_ID = process.env.MR_CF_TURN_KEY_ID || "";
const CF_TURN_TOKEN = process.env.MR_CF_TURN_TOKEN || "";
const TURN_TTL = 24 * 3600;
let turnCache = { servers: null, until: 0 };

async function refreshTurn() {
  if (!CF_TURN_KEY_ID || !CF_TURN_TOKEN) return;
  try {
    const r = await fetch(`https://rtc.live.cloudflare.com/v1/turn/keys/${CF_TURN_KEY_ID}/credentials/generate-ice-servers`, {
      method: "POST",
      headers: { Authorization: `Bearer ${CF_TURN_TOKEN}`, "Content-Type": "application/json" },
      body: JSON.stringify({ ttl: TURN_TTL }),
    });
    if (!r.ok) throw new Error(`HTTP ${r.status}`);
    const j = await r.json();
    // port 53 URLs are blocked by browsers and only cause timeouts
    const servers = (j.iceServers || []).map((s) => ({ ...s, urls: [].concat(s.urls).filter((u) => !/:53(\?|$)/.test(u)) }))
      .filter((s) => s.urls.length);
    turnCache = { servers, until: Date.now() + (TURN_TTL / 2) * 1000 };
    console.log(`[turn] got ${servers.length} Cloudflare ICE server entries`);
  } catch (e) {
    console.error("[turn] could not get Cloudflare TURN credentials:", e.message);
  }
}
if (CF_TURN_KEY_ID) {
  refreshTurn();
  setInterval(() => { if (Date.now() > turnCache.until) refreshTurn(); }, 10 * 60_000);
}

function iceServers() {
  if (turnCache.servers && turnCache.servers.length) {
    return [{ urls: "stun:stun.l.google.com:19302" }, ...turnCache.servers];
  }
  return ICE.map((u) => {
    // turn:user:pass@host:port -> {urls, username, credential}
    const m = u.match(/^(turns?):([^:@]+):([^@]+)@(.+)$/);
    if (m) return { urls: `${m[1]}:${m[4]}`, username: m[2], credential: m[3] };
    return { urls: u };
  });
}

// ---------- auth ----------
const tokens = new Map(); // token -> expiry
const failed = new Map(); // ip -> {count, until}

function safeEqual(a, b) {
  const ha = crypto.createHash("sha256").update(String(a)).digest();
  const hb = crypto.createHash("sha256").update(String(b)).digest();
  return crypto.timingSafeEqual(ha, hb);
}

function issueToken() {
  const t = crypto.randomBytes(32).toString("hex");
  tokens.set(t, Date.now() + TOKEN_TTL_MS);
  return t;
}

function validToken(t) {
  const exp = tokens.get(t);
  if (!exp) return false;
  if (exp < Date.now()) { tokens.delete(t); return false; }
  return true;
}

function rateLimited(ip) {
  const f = failed.get(ip);
  return f && f.until > Date.now();
}

function noteFailure(ip) {
  const f = failed.get(ip) || { count: 0, until: 0 };
  f.count++;
  if (f.count >= 5) { f.until = Date.now() + 60_000 * Math.min(f.count - 4, 15); }
  failed.set(ip, f);
}

// ---------- http ----------
// Behind Cloudflare Tunnel every request comes from cloudflared on localhost,
// so trust Cloudflare's header for the real visitor IP (used by the rate limit).
function clientIp(req) {
  const peer = req.socket.remoteAddress || "";
  const local = peer === "127.0.0.1" || peer === "::1" || peer === "::ffff:127.0.0.1";
  return (local && req.headers["cf-connecting-ip"]) || peer;
}

const MIME = {
  ".html": "text/html; charset=utf-8", ".js": "text/javascript; charset=utf-8",
  ".css": "text/css; charset=utf-8", ".svg": "image/svg+xml", ".png": "image/png",
  ".json": "application/json", ".ico": "image/x-icon", ".webmanifest": "application/manifest+json",
};

function cors(res) {
  res.setHeader("Access-Control-Allow-Origin", "*");
  res.setHeader("Access-Control-Allow-Headers", "content-type");
  res.setHeader("Access-Control-Allow-Methods", "GET,POST,OPTIONS");
}

function json(res, code, body) {
  cors(res);
  res.writeHead(code, { "content-type": "application/json" });
  res.end(JSON.stringify(body));
}

const server = http.createServer((req, res) => {
  const url = new URL(req.url, "http://x");
  const ip = clientIp(req);

  if (req.method === "OPTIONS") { cors(res); res.writeHead(204); return res.end(); }

  if (url.pathname === "/api/login" && req.method === "POST") {
    if (rateLimited(ip)) return json(res, 429, { error: "Too many attempts. Wait a minute." });
    let body = "";
    req.on("data", (c) => { body += c; if (body.length > 4096) req.destroy(); });
    req.on("end", () => {
      let pw = "";
      try { pw = JSON.parse(body).password || ""; } catch {}
      if (!safeEqual(pw, PASSWORD)) { noteFailure(ip); return json(res, 401, { error: "Wrong password." }); }
      failed.delete(ip);
      json(res, 200, { token: issueToken(), ice: iceServers() });
    });
    return;
  }

  if (url.pathname === "/api/health") return json(res, 200, { ok: true, hosts: hosts.size });

  // static files
  let p = decodeURIComponent(url.pathname);
  if (p.endsWith("/")) p += "index.html";
  const file = path.resolve(WEB_DIR, "." + p);
  if (!file.startsWith(WEB_DIR)) { res.writeHead(403); return res.end(); }
  // Always let browsers check for a newer version (cheap: unchanged files get
  // a 304), so after an update the page and its script never get mixed up.
  fs.stat(file, (err, st) => {
    if (err || !st.isFile()) { res.writeHead(404); return res.end("Not found"); }
    const etag = `"${st.size.toString(36)}-${Math.floor(st.mtimeMs).toString(36)}"`;
    const headers = { "cache-control": "no-cache", etag };
    if (req.headers["if-none-match"] === etag) { res.writeHead(304, headers); return res.end(); }
    fs.readFile(file, (err, data) => {
      if (err) { res.writeHead(404); return res.end("Not found"); }
      res.writeHead(200, { ...headers, "content-type": MIME[path.extname(file)] || "application/octet-stream" });
      res.end(data);
    });
  });
});

// ---------- signaling ----------
// hosts: id -> {ws, name, os, since}
const hosts = new Map();
// clients: connId -> ws
const clients = new Map();

// Pi / server stats, pushed every 2 s to signed-in browsers that asked for them
const stats = createStats({ extra: () => ({ hosts: hosts.size, viewers: clients.size }) });
stats.onSample((s) => {
  const msg = JSON.stringify({ type: "stats", stats: s });
  for (const ws of clients.values()) if (ws.wantStats && ws.readyState === 1) ws.send(msg);
});

const wss = new WebSocketServer({ server, path: "/ws", maxPayload: 256 * 1024 });

function send(ws, msg) {
  if (ws && ws.readyState === 1) ws.send(JSON.stringify(msg));
}

function hostList() {
  return [...hosts.entries()].map(([id, h]) => ({ id, name: h.name, os: h.os, since: h.since, busy: !!h.session, version: h.version, caps: h.caps }));
}

function broadcastHosts() {
  const msg = { type: "hosts", hosts: hostList() };
  for (const ws of clients.values()) send(ws, msg);
}

wss.on("connection", (ws, req) => {
  ws.role = null;
  ws.id = crypto.randomBytes(8).toString("hex");
  ws.isAlive = true;
  ws.on("pong", () => { ws.isAlive = true; });

  const helloTimer = setTimeout(() => { if (!ws.role) ws.close(4001, "hello timeout"); }, 10_000);

  ws.on("message", (raw) => {
    let msg;
    try { msg = JSON.parse(raw); } catch { return; }

    // --- handshake ---
    if (!ws.role) {
      if (msg.type === "host-hello") {
        if (!safeEqual(msg.secret || "", HOST_SECRET)) { ws.close(4003, "bad host secret"); return; }
        const id = String(msg.id || "").slice(0, 64) || ws.id;
        const prev = hosts.get(id);
        if (prev) prev.ws.close(4000, "replaced");
        ws.role = "host"; ws.hostId = id;
        hosts.set(id, {
          ws, name: String(msg.name || id).slice(0, 64), os: String(msg.os || "").slice(0, 32), since: Date.now(), session: null,
          version: String(msg.version || "").slice(0, 32),
          caps: Array.isArray(msg.caps) ? msg.caps.filter((c) => typeof c === "string").slice(0, 16) : [],
        });
        clearTimeout(helloTimer);
        send(ws, { type: "welcome", ice: iceServers() });
        console.log(`[host] online: ${id} (${msg.name})`);
        broadcastHosts();
        return;
      }
      if (msg.type === "client-hello") {
        if (!validToken(msg.token)) { send(ws, { type: "auth-failed" }); ws.close(4003, "bad token"); return; }
        ws.role = "client";
        clients.set(ws.id, ws);
        clearTimeout(helloTimer);
        send(ws, { type: "welcome", id: ws.id, ice: iceServers(), hosts: hostList() });
        return;
      }
      return;
    }

    // --- client: server stats on/off ---
    if (ws.role === "client" && msg.type === "stats-sub") {
      ws.wantStats = !!msg.on;
      if (ws.wantStats && stats.latest()) send(ws, { type: "stats", stats: stats.latest() });
      return;
    }

    // --- client -> host ---
    if (ws.role === "client" && msg.type === "signal") {
      const h = hosts.get(msg.to);
      if (!h) return send(ws, { type: "error", sid: msg.sid, error: "Host is offline." });
      if (msg.data && msg.data.sdp && msg.data.sdp.type === "offer") {
        h.session = { client: ws.id, sid: msg.sid };
        broadcastHosts();
      }
      const fwd = { type: "signal", from: ws.id, sid: msg.sid, data: msg.data };
      if (msg.data && msg.data.sdp && msg.data.sdp.type === "offer") fwd.ice = iceServers();
      send(h.ws, fwd);
      return;
    }
    // client -> host: maintenance commands (work without a video session)
    if (ws.role === "client" && msg.type === "command") {
      const h = hosts.get(msg.to);
      if (!h) return send(ws, { type: "command-result", from: msg.to, ok: false, msg: "That machine is offline." });
      if (!["update", "restart"].includes(msg.cmd)) return;
      console.log(`[host] ${msg.cmd} requested for ${msg.to}`);
      send(h.ws, { type: "command", from: ws.id, cmd: msg.cmd });
      return;
    }
    if (ws.role === "host" && msg.type === "command-result") {
      send(clients.get(msg.to), { type: "command-result", from: ws.hostId, ok: !!msg.ok, msg: String(msg.msg || "").slice(0, 300) });
      return;
    }
    if (ws.role === "client" && msg.type === "bye") {
      const h = hosts.get(msg.to);
      if (h) {
        send(h.ws, { type: "bye", from: ws.id, sid: msg.sid });
        if (h.session && h.session.sid === msg.sid) { h.session = null; broadcastHosts(); }
      }
      return;
    }

    // --- host -> client ---
    if (ws.role === "host" && (msg.type === "signal" || msg.type === "bye")) {
      const c = clients.get(msg.to);
      send(c, { type: msg.type, from: ws.hostId, sid: msg.sid, data: msg.data });
      if (msg.type === "bye") {
        const h = hosts.get(ws.hostId);
        if (h && h.session && h.session.sid === msg.sid) { h.session = null; broadcastHosts(); }
      }
      return;
    }
  });

  ws.on("close", () => {
    clearTimeout(helloTimer);
    if (ws.role === "host") {
      const h = hosts.get(ws.hostId);
      if (h && h.ws === ws) {
        if (h.session) send(clients.get(h.session.client), { type: "bye", from: ws.hostId, sid: h.session.sid });
        hosts.delete(ws.hostId);
        console.log(`[host] offline: ${ws.hostId}`);
        broadcastHosts();
      }
    } else if (ws.role === "client") {
      clients.delete(ws.id);
      for (const [id, h] of hosts) {
        if (h.session && h.session.client === ws.id) {
          send(h.ws, { type: "bye", from: ws.id, sid: h.session.sid });
          h.session = null;
        }
      }
      broadcastHosts();
    }
  });
});

// keepalive
setInterval(() => {
  for (const ws of wss.clients) {
    if (!ws.isAlive) { ws.terminate(); continue; }
    ws.isAlive = false;
    ws.ping();
  }
}, 20_000);

server.listen(PORT, () => {
  console.log(`Maze Remote server on http://0.0.0.0:${PORT}`);
  console.log(`Serving web client from ${WEB_DIR}`);
  console.log(CF_TURN_KEY_ID ? "TURN relay: Cloudflare" : "TURN relay: none (set MR_CF_TURN_KEY_ID + MR_CF_TURN_TOKEN for other networks)");
});
