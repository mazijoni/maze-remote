// Maze Remote — server stats
// Samples the box the server runs on (CPU, memory, temperature, disk, network,
// uptime) every few seconds from /proc and /sys, so the website can show how
// the Raspberry Pi is doing. Everything is optional: on a machine without a
// given file, that value is just left out.

import fs from "node:fs";
import os from "node:os";
import { execFile } from "node:child_process";

const INTERVAL_MS = 2000;
const HISTORY = 90; // samples kept for the graphs (3 minutes)

const read = (f) => { try { return fs.readFileSync(f, "utf8"); } catch { return null; } };
const readNum = (f) => { const v = read(f); const n = v === null ? NaN : Number(v.trim()); return Number.isFinite(n) ? n : null; };
const round = (n, d = 1) => (n === null || n === undefined ? null : Math.round(n * 10 ** d) / 10 ** d);

function run(cmd, args) {
  return new Promise((resolve) => {
    execFile(cmd, args, { timeout: 1500 }, (err, out) => resolve(err ? null : String(out)));
  });
}

// ---------- things that don't change ----------
function staticInfo() {
  const osRelease = read("/etc/os-release") || "";
  const pretty = (osRelease.match(/^PRETTY_NAME="?([^"\n]*)"?/m) || [])[1];
  const model = (read("/proc/device-tree/model") || "").replace(/\0/g, "").trim();
  return {
    hostname: os.hostname(),
    model: model || null,
    os: pretty || `${os.type()} ${os.release()}`,
    kernel: os.release(),
    arch: os.arch(),
    cores: os.cpus().length,
    node: process.version,
  };
}

// ---------- CPU ----------
function cpuTimes() {
  return os.cpus().map((c) => {
    const t = c.times;
    return { idle: t.idle, total: t.user + t.nice + t.sys + t.idle + t.irq };
  });
}

function cpuFreqMHz() {
  const khz = readNum("/sys/devices/system/cpu/cpu0/cpufreq/scaling_cur_freq");
  return khz === null ? null : Math.round(khz / 1000);
}

// ---------- temperature ----------
// thermal_zone0 is the SoC on a Pi; fall back to vcgencmd if sysfs is missing.
async function cpuTempC() {
  const milli = readNum("/sys/class/thermal/thermal_zone0/temp");
  if (milli !== null) return round(milli / 1000);
  const out = await run("vcgencmd", ["measure_temp"]);
  const m = out && out.match(/temp=([\d.]+)/);
  return m ? Number(m[1]) : null;
}

// Pi-only: is the board being throttled or short on power right now / since boot?
const THROTTLE_BITS = {
  0: "Under-voltage", 1: "CPU frequency capped", 2: "Throttled", 3: "Soft temperature limit",
};
async function throttled() {
  const out = await run("vcgencmd", ["get_throttled"]);
  const m = out && out.match(/throttled=(0x[0-9a-f]+)/i);
  if (!m) return null;
  const v = parseInt(m[1], 16);
  const now = [], past = [];
  for (const [bit, label] of Object.entries(THROTTLE_BITS)) {
    if (v & (1 << Number(bit))) now.push(label);
    if (v & (1 << (Number(bit) + 16))) past.push(label);
  }
  return { now, sinceBoot: past };
}

// Pi 5 official fan / any hwmon fan
function fanRpm() {
  for (const base of ["/sys/devices/platform/cooling_fan/hwmon", "/sys/class/hwmon"]) {
    let dirs = [];
    try { dirs = fs.readdirSync(base); } catch { continue; }
    for (const d of dirs) {
      const rpm = readNum(`${base}/${d}/fan1_input`);
      if (rpm !== null) return rpm;
    }
  }
  return null;
}

// ---------- memory ----------
function memory() {
  const info = read("/proc/meminfo");
  if (!info) {
    const total = os.totalmem(), free = os.freemem();
    return { total, used: total - free, swapTotal: null, swapUsed: null };
  }
  const kb = (k) => { const m = info.match(new RegExp(`^${k}:\\s+(\\d+)`, "m")); return m ? Number(m[1]) * 1024 : 0; };
  const total = kb("MemTotal");
  const avail = kb("MemAvailable") || kb("MemFree") + kb("Buffers") + kb("Cached");
  const swapTotal = kb("SwapTotal");
  return { total, used: total - avail, swapTotal, swapUsed: swapTotal - kb("SwapFree") };
}

// ---------- disk ----------
async function disk(pathname = "/") {
  if (fs.promises.statfs) {
    try {
      const s = await fs.promises.statfs(pathname);
      const total = s.blocks * s.bsize;
      return { path: pathname, total, used: total - s.bfree * s.bsize, free: s.bavail * s.bsize };
    } catch {}
  }
  const out = await run("df", ["-kP", pathname]);
  const row = out && out.trim().split("\n")[1];
  if (!row) return null;
  const [, total, used, free] = row.split(/\s+/).map(Number);
  return { path: pathname, total: total * 1024, used: used * 1024, free: free * 1024 };
}

// ---------- network ----------
// total bytes over all real interfaces (not loopback / docker bridges)
function netBytes() {
  const dev = read("/proc/net/dev");
  if (!dev) return null;
  let rx = 0, tx = 0;
  for (const line of dev.split("\n").slice(2)) {
    const [name, rest] = line.split(":");
    if (!rest) continue;
    const n = name.trim();
    if (n === "lo" || n.startsWith("docker") || n.startsWith("veth") || n.startsWith("br-")) continue;
    const f = rest.trim().split(/\s+/).map(Number);
    rx += f[0]; tx += f[8];
  }
  return { rx, tx };
}

function addresses() {
  const out = [];
  for (const [name, list] of Object.entries(os.networkInterfaces())) {
    for (const a of list || []) {
      if (!a.internal && a.family === "IPv4") out.push({ iface: name, address: a.address });
    }
  }
  return out;
}

// ---------- sampler ----------
export function createStats({ extra = () => ({}) } = {}) {
  const info = staticInfo();
  const history = { cpu: [], temp: [], rx: [], tx: [] };
  let prevCpu = cpuTimes(), prevNet = netBytes(), prevAt = Date.now();
  let latest = null;
  let slowTick = 0, slow = { disk: null, throttled: null };

  async function sample() {
    const now = Date.now();
    const dt = Math.max(0.001, (now - prevAt) / 1000);

    const cur = cpuTimes();
    const perCore = cur.map((c, i) => {
      const p = prevCpu[i] || c;
      const total = c.total - p.total, idle = c.idle - p.idle;
      return total > 0 ? round(100 * (1 - idle / total)) : 0;
    });
    const cpu = round(perCore.reduce((a, b) => a + b, 0) / (perCore.length || 1));
    prevCpu = cur;

    const net = netBytes();
    let rxRate = null, txRate = null;
    if (net && prevNet) {
      rxRate = Math.max(0, (net.rx - prevNet.rx) / dt);
      txRate = Math.max(0, (net.tx - prevNet.tx) / dt);
    }
    prevNet = net; prevAt = now;

    // disk and vcgencmd change slowly and spawn processes: every 15 s is plenty
    if (slowTick++ % 8 === 0) slow = { disk: await disk("/"), throttled: await throttled() };

    const temp = await cpuTempC();
    const push = (k, v) => { history[k].push(v); if (history[k].length > HISTORY) history[k].shift(); };
    push("cpu", cpu); push("temp", temp); push("rx", rxRate === null ? null : Math.round(rxRate)); push("tx", txRate === null ? null : Math.round(txRate));

    const load = os.loadavg().map((n) => round(n, 2));
    latest = {
      at: now,
      interval: INTERVAL_MS,
      info,
      uptime: Math.round(os.uptime()),
      cpu: { usage: cpu, perCore, load, freqMHz: cpuFreqMHz() },
      temp: { cpu: temp, fanRpm: fanRpm(), throttled: slow.throttled },
      memory: memory(),
      disk: slow.disk,
      network: { rxRate: rxRate === null ? null : Math.round(rxRate), txRate: txRate === null ? null : Math.round(txRate), rxTotal: net && net.rx, txTotal: net && net.tx, addresses: addresses() },
      server: { uptime: Math.round(process.uptime()), rss: process.memoryUsage().rss, ...extra() },
      history,
    };
    return latest;
  }

  const listeners = new Set();
  let busy = false;
  setInterval(async () => {
    if (busy) return;
    busy = true;
    try {
      const s = await sample();
      for (const fn of listeners) fn(s);
    } catch (e) {
      console.error("[stats] sample failed:", e.message);
    } finally { busy = false; }
  }, INTERVAL_MS).unref();

  return {
    latest: () => latest,
    onSample: (fn) => { listeners.add(fn); return () => listeners.delete(fn); },
  };
}
