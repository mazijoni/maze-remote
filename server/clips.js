// Camera clips: the camera agent on this Pi saves a clip whenever it sees
// motion (see agent/motion.go) into the clips folder, one folder per day:
//   2026-11-11/142630.mp4  .jpg  .json
// This lists them for the website's timeline, streams them (with Range
// support, so the video can be scrubbed), deletes them, and passes the
// website's recording settings to the agent through settings.json.

import fs from "node:fs";
import fsp from "node:fs/promises";
import os from "node:os";
import path from "node:path";

export const CLIPS_DIR = path.resolve(process.env.MR_CLIPS_DIR || path.join(os.homedir(), "maze-remote-clips"));

const DAY = /^\d{4}-\d{2}-\d{2}$/;
const NAME = /^\d{6}(-\d+)?$/;
const DEFAULTS = { enabled: true, sensitivity: "medium", keepDays: 14, maxGB: 0 };

function send(res, code, body) {
  res.writeHead(code, { "content-type": "application/json", "cache-control": "no-store" });
  res.end(JSON.stringify(body));
}

async function readJSON(p, fallback = null) {
  try { return JSON.parse(await fsp.readFile(p, "utf8")); } catch { return fallback; }
}

function readBody(req, limit = 4096) {
  return new Promise((resolve) => {
    let body = "";
    req.on("data", (c) => { body += c; if (body.length > limit) req.destroy(); });
    req.on("end", () => { try { resolve(JSON.parse(body)); } catch { resolve({}); } });
    req.on("error", () => resolve({}));
  });
}

async function listDays() {
  let entries = [];
  try { entries = await fsp.readdir(CLIPS_DIR, { withFileTypes: true }); } catch { return []; }
  const days = [];
  for (const e of entries) {
    if (!e.isDirectory() || !DAY.test(e.name)) continue;
    const files = await fsp.readdir(path.join(CLIPS_DIR, e.name)).catch(() => []);
    const count = files.filter((f) => f.endsWith(".json")).length;
    if (count) days.push({ day: e.name, count });
  }
  return days.sort((a, b) => (a.day < b.day ? 1 : -1)); // newest first
}

async function listClips(day) {
  const dir = path.join(CLIPS_DIR, day);
  const files = await fsp.readdir(dir).catch(() => []);
  const clips = [];
  for (const f of files) {
    if (!f.endsWith(".json")) continue;
    const name = f.slice(0, -5);
    if (!NAME.test(name)) continue;
    const m = await readJSON(path.join(dir, f));
    if (!m) continue;
    m.id = day + "/" + name;
    m.thumb = files.includes(name + ".jpg");
    clips.push(m);
  }
  return clips.sort((a, b) => a.start - b.start);
}

async function status() {
  const st = await readJSON(path.join(CLIPS_DIR, "status.json"));
  const settings = { ...DEFAULTS, ...(await readJSON(path.join(CLIPS_DIR, "settings.json"), {})) };
  // the agent rewrites status.json every 2 s while it runs
  const live = !!st && Date.now() - st.updated < 15_000;
  const state = !st ? "none" : !live ? "offline" : !settings.enabled ? "off" : st.recording ? "recording" : "watching";
  let disk = null;
  try {
    const s = await fsp.statfs(CLIPS_DIR);
    disk = { free: s.bavail * s.bsize, total: s.blocks * s.bsize };
  } catch {}
  return { state, since: live && st.recording ? st.since : null, settings, disk };
}

function parseId(id) {
  const [day, name] = String(id || "").split("/");
  if (!DAY.test(day || "") || !NAME.test(name || "")) return null;
  return path.join(CLIPS_DIR, day, name);
}

function serveFile(req, res, file, type) {
  fs.stat(file, (err, st) => {
    if (err || !st.isFile()) { res.writeHead(404); return res.end("Not found"); }
    const headers = { "content-type": type, "accept-ranges": "bytes", "cache-control": "private, max-age=86400" };
    const m = /^bytes=(\d*)-(\d*)$/.exec(req.headers.range || "");
    if (m && (m[1] || m[2])) {
      let start = m[1] ? Number(m[1]) : st.size - Number(m[2]);
      let end = m[1] && m[2] ? Number(m[2]) : st.size - 1;
      start = Math.max(0, start); end = Math.min(end, st.size - 1);
      if (start > end) { res.writeHead(416, { "content-range": `bytes */${st.size}` }); return res.end(); }
      res.writeHead(206, { ...headers, "content-range": `bytes ${start}-${end}/${st.size}`, "content-length": end - start + 1 });
      if (req.method === "HEAD") return res.end();
      return fs.createReadStream(file, { start, end }).pipe(res);
    }
    res.writeHead(200, { ...headers, "content-length": st.size });
    if (req.method === "HEAD") return res.end();
    fs.createReadStream(file).pipe(res);
  });
}

export async function handleClips(req, res, url) {
  const p = url.pathname;
  try {
    // GET /api/clips[?day=YYYY-MM-DD] → status, settings, days with clips, and that day's clips
    if (p === "/api/clips" && req.method === "GET") {
      const days = await listDays();
      const day = DAY.test(url.searchParams.get("day") || "") ? url.searchParams.get("day") : null;
      return send(res, 200, { ...(await status()), days, day, clips: day ? await listClips(day) : [] });
    }
    // GET /api/clips/media/YYYY-MM-DD/HHMMSS.mp4|.jpg
    const mm = /^\/api\/clips\/media\/(\d{4}-\d{2}-\d{2})\/(\d{6}(?:-\d+)?)\.(mp4|jpg)$/.exec(p);
    if (mm && (req.method === "GET" || req.method === "HEAD")) {
      return serveFile(req, res, path.join(CLIPS_DIR, mm[1], mm[2] + "." + mm[3]), mm[3] === "mp4" ? "video/mp4" : "image/jpeg");
    }
    if (p === "/api/clips/delete" && req.method === "POST") {
      const { ids } = await readBody(req, 64 * 1024);
      let n = 0;
      for (const id of [].concat(ids || []).slice(0, 500)) {
        const base = parseId(id);
        if (!base) continue;
        for (const ext of [".mp4", ".jpg", ".json"]) await fsp.rm(base + ext, { force: true });
        n++;
      }
      return send(res, 200, { deleted: n });
    }
    if (p === "/api/clips/settings" && req.method === "POST") {
      const b = await readBody(req);
      const cur = { ...DEFAULTS, ...(await readJSON(path.join(CLIPS_DIR, "settings.json"), {})) };
      if (typeof b.enabled === "boolean") cur.enabled = b.enabled;
      if (["low", "medium", "high"].includes(b.sensitivity)) cur.sensitivity = b.sensitivity;
      if (Number.isFinite(b.keepDays)) cur.keepDays = Math.max(1, Math.min(365, Math.round(b.keepDays)));
      if (Number.isFinite(b.maxGB)) cur.maxGB = Math.max(0, Math.min(10000, b.maxGB));
      await fsp.mkdir(CLIPS_DIR, { recursive: true });
      const tmp = path.join(CLIPS_DIR, "settings.json.tmp");
      await fsp.writeFile(tmp, JSON.stringify(cur, null, 2));
      await fsp.rename(tmp, path.join(CLIPS_DIR, "settings.json"));
      return send(res, 200, { ...(await status()) });
    }
    send(res, 404, { error: "Not found" });
  } catch (e) {
    console.error("[clips]", e);
    send(res, 500, { error: "Couldn't read the clips folder." });
  }
}
