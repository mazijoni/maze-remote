// Maze Remote — web client
// Talks to the signaling server over WebSocket, then receives the host's screen
// over WebRTC and sends mouse/keyboard input back on a data channel.

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];

// ---------------- storage ----------------
const store = {
  get(k, d = null) { try { const v = localStorage.getItem("mr:" + k); return v === null ? d : JSON.parse(v); } catch { return d; } },
  set(k, v) { try { localStorage.setItem("mr:" + k, JSON.stringify(v)); } catch {} },
  del(k) { try { localStorage.removeItem("mr:" + k); } catch {} },
};

const prefs = Object.assign({ invertScroll: false, autoHide: true, reduceMotion: false }, store.get("prefs", {}));
const savePrefs = () => store.set("prefs", prefs);

// ---------------- motion ----------------
const reducedMotion = () => prefs.reduceMotion || matchMedia("(prefers-reduced-motion: reduce)").matches;
const EASE_OUT = "cubic-bezier(0.22, 1, 0.36, 1)";
const EASE_IN = "cubic-bezier(0.55, 0, 1, 0.45)";
const SPRING = "cubic-bezier(0.34, 1.56, 0.64, 1)";

function animate(el, frames, opts) {
  if (!el) return Promise.resolve();
  if (reducedMotion()) opts = { ...opts, duration: 1 };
  const a = el.animate(frames, { fill: "both", ...opts });
  return a.finished.then(() => a).catch(() => a);
}

// open/close an element with a matching pair of animations
const motions = {
  modal: {
    in: [{ opacity: 0, transform: "translateY(10px) scale(.98)" }, { opacity: 1, transform: "none" }],
    inOpts: { duration: 280, easing: EASE_OUT },
    out: [{ opacity: 1, transform: "none" }, { opacity: 0, transform: "translateY(6px) scale(.99)" }],
    outOpts: { duration: 160, easing: EASE_IN },
  },
  pop: {
    in: [{ opacity: 0, transform: "translate(-50%, -6px)" }, { opacity: 1, transform: "translate(-50%, 0)" }],
    inOpts: { duration: 200, easing: EASE_OUT },
    out: [{ opacity: 1, transform: "translate(-50%, 0)" }, { opacity: 0, transform: "translate(-50%, -4px)" }],
    outOpts: { duration: 120, easing: EASE_IN },
  },
  drawer: {
    in: [{ transform: "translateX(calc(100% + 24px))" }, { transform: "none" }],
    inOpts: { duration: 340, easing: EASE_OUT },
    out: [{ transform: "none" }, { transform: "translateX(calc(100% + 24px))" }],
    outOpts: { duration: 220, easing: EASE_IN },
  },
  view: {
    in: [{ opacity: 0, transform: "translateY(8px)" }, { opacity: 1, transform: "none" }],
    inOpts: { duration: 380, easing: EASE_OUT },
    out: [{ opacity: 1 }, { opacity: 0 }],
    outOpts: { duration: 160, easing: EASE_IN },
  },
};

async function openEl(el, kind) {
  el.hidden = false;
  const m = motions[kind];
  await animate(el, m.in, m.inOpts);
}
async function closeEl(el, kind) {
  if (el.hidden) return;
  const m = motions[kind];
  await animate(el, m.out, m.outOpts);
  el.hidden = true;
  el.getAnimations().forEach((a) => a.cancel());
}

// ---------------- toasts ----------------
function toast(msg, kind = "") {
  const t = document.createElement("div");
  t.className = "toast " + kind;
  t.textContent = msg;
  $("#toasts").append(t);
  animate(t, [{ opacity: 0, transform: "translateX(16px)" }, { opacity: 1, transform: "none" }], { duration: 260, easing: EASE_OUT });
  setTimeout(async () => {
    await animate(t, [{ opacity: 1, transform: "none" }, { opacity: 0, transform: "translateX(16px)" }], { duration: 180, easing: EASE_IN });
    t.remove();
  }, 3400);
}

// ---------------- views ----------------
let currentView = null;
async function showView(id) {
  const next = $("#" + id);
  if (currentView === next) return;
  if (currentView) await closeEl(currentView, "view");
  currentView = next;
  await openEl(next, "view");
}

// ---------------- modals ----------------
let openModalEl = null;
async function openModal(id) {
  const root = $("#modal-root");
  if (openModalEl) await closeModal(true);
  root.hidden = false;
  openModalEl = $("#" + id);
  animate($(".scrim", root), [{ opacity: 0 }, { opacity: 1 }], { duration: 200, easing: "ease-out" });
  await openEl(openModalEl, "modal");
  const f = $("textarea, input:not([type=checkbox])", openModalEl);
  if (f) f.focus();
}
async function closeModal(keepScrim = false) {
  if (!openModalEl) return;
  const root = $("#modal-root");
  const el = openModalEl;
  openModalEl = null;
  if (!keepScrim) animate($(".scrim", root), [{ opacity: 1 }, { opacity: 0 }], { duration: 160, easing: "ease-in" });
  await closeEl(el, "modal");
  if (!keepScrim && !openModalEl) root.hidden = true;
  if (session) $("#stage").focus();
}
$("#modal-root").addEventListener("click", (e) => { if (e.target.closest("[data-close-modal]")) closeModal(); });

// ---------------- app state ----------------
const state = {
  demo: false,
  server: store.get("server", ""),
  token: store.get("token", ""),
  ice: store.get("ice", [{ urls: "stun:stun.l.google.com:19302" }]),
  ws: null,
  hosts: [],
  wsRetry: 0,
};
let session = null;

const isServedByServer = () => location.protocol.startsWith("http") && !location.hostname.endsWith("github.io");
const wsURL = (server) => server.replace(/^http/, "ws").replace(/\/$/, "") + "/ws";

function setConn(kind, text) {
  const p = $("#conn-pill");
  p.className = "status " + kind;
  $("span", p).textContent = text;
}

// ---------------- login ----------------
function initLogin() {
  const srv = $("#login-server");
  srv.value = state.server || (isServedByServer() ? location.origin : "");
  $("#login-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const btn = $("button[type=submit]", e.target);
    const err = $("#login-error");
    err.textContent = "";
    let server = srv.value.trim().replace(/\/$/, "");
    if (!/^https?:\/\//.test(server)) server = "http://" + server;

    if (location.protocol === "https:" && server.startsWith("http:") && !/\/\/(localhost|127\.0\.0\.1)/.test(server)) {
      err.textContent = "This page is HTTPS, so browsers block plain-http servers. Open the site from your Pi instead (" + server + "), or put the Pi behind HTTPS (see README).";
      shake($(".login-card"));
      return;
    }

    btn.classList.add("loading");
    btn.disabled = true;
    try {
      const r = await fetch(server + "/api/login", {
        method: "POST", headers: { "content-type": "application/json" },
        body: JSON.stringify({ password: $("#login-password").value }),
      });
      const j = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(j.error || "Login failed (" + r.status + ")");
      Object.assign(state, { server, token: j.token, ice: j.ice || state.ice, demo: false });
      store.set("server", server); store.set("token", j.token); store.set("ice", state.ice);
      $("#login-password").value = "";
      enterDashboard();
    } catch (ex) {
      err.textContent = ex.message === "Failed to fetch" ? "Couldn't reach the server. Is it running, and is the address right?" : ex.message;
      shake($(".login-card"));
    } finally {
      btn.classList.remove("loading");
      btn.disabled = false;
    }
  });
  $("#btn-demo").addEventListener("click", () => { state.demo = true; enterDashboard(); });
}

function shake(el) { el.classList.remove("shake"); void el.offsetWidth; el.classList.add("shake"); }

// ---------------- dashboard ----------------
async function enterDashboard() {
  await showView("view-dash");
  $("#set-server").textContent = state.demo ? "Demo mode" : state.server;
  if (state.demo) {
    setConn("demo", "Demo mode");
    renderHosts(DEMO_HOSTS, true);
  } else {
    renderHosts(null, true);
    connectSignaling();
  }
}

function logout() {
  if (session) endSession("signed out");
  if (state.ws) { state.ws.onclose = null; state.ws.close(); state.ws = null; }
  store.del("token");
  Object.assign(state, { token: "", demo: false, hosts: [] });
  showView("view-login");
}
$("#btn-logout").addEventListener("click", logout);
$("#btn-settings").addEventListener("click", () => openModal("modal-settings"));
$("#btn-add").addEventListener("click", () => openAddModal());
$("#btn-add-2").addEventListener("click", () => openAddModal());


const OS_ICONS = { windows: "desktop_windows", linux: "terminal", darwin: "laptop_mac", default: "computer" };
const OS_NAMES = { windows: "Windows", linux: "Linux", darwin: "macOS" };

function ago(ts) {
  const s = Math.max(1, Math.round((Date.now() - ts) / 1000));
  if (s < 60) return "just now";
  const m = Math.round(s / 60); if (m < 60) return m + " min";
  const h = Math.round(m / 60); if (h < 48) return h + " h";
  return Math.round(h / 24) + " days";
}

function renderHosts(hosts, animateIn = false) {
  const grid = $("#host-grid");
  if (hosts) state.hosts = hosts;
  const list = state.hosts;
  const known = new Set($$(".host-card", grid).map((c) => c.dataset.id));
  grid.innerHTML = "";

  if (!hosts && !state.demo && list.length === 0) {
    grid.innerHTML = '<div class="empty-state"><div class="spinner"></div><p>Looking for machines…</p></div>';
    return;
  }
  if (list.length === 0) {
    grid.innerHTML = '<div class="empty-state"><span class="material-symbols-outlined">desktop_access_disabled</span><p>No machines online yet</p><p class="sub">Run the agent on a PC and it shows up here automatically.</p><button class="btn btn-primary"><span class="material-symbols-outlined">add</span>Set up a machine</button></div>';
    $(".empty-state .btn", grid).addEventListener("click", () => openAddModal());
    return;
  }
  list.forEach((h, i) => {
    const card = $("#tpl-host").content.firstElementChild.cloneNode(true);
    card.dataset.id = h.id;
    const online = h.online !== false;
    card.classList.add(online ? (h.busy ? "busy" : "online") : "offline");
    $(".host-os", card).textContent = OS_ICONS[h.os] || OS_ICONS.default;
    $(".host-tag", card).textContent = OS_NAMES[h.os] || h.os || "PC";
    $(".host-name", card).textContent = h.name;
    const sub = !online ? "Offline" : h.busy ? "In use" : "Online · " + ago(h.since);
    $(".host-sub", card).innerHTML = '<i class="dot"></i>' + sub;
    const btn = $(".host-connect", card);
    if (!online) { btn.disabled = true; btn.querySelector("span").textContent = "Offline"; }
    else card.addEventListener("click", () => startSession(h, card));
    const more = $(".host-more", card);
    if (!online || state.demo) more.remove();
    else more.addEventListener("click", (e) => { e.stopPropagation(); openCardMenu(h, more); });
    if (animateIn || !known.has(h.id)) {
      card.classList.add("enter");
      card.style.animationDelay = (animateIn ? i * 70 : 0) + "ms";
    }
    grid.append(card);
  });
}

function openAddModal(tab = "win") {
  // config with this server's address filled in (wss://… when opened over HTTPS)
  const server = state.demo ? "wss://remote.maze-development.com/ws" : wsURL(state.server);
  $("#add-config").textContent = JSON.stringify({ server, secret: "PASTE_YOUR_HOST_SECRET", name: "My PC", encoder: "x264", fps: 60, bitrate: "8M" }, null, 2);
  showTab(tab, false);
  openModal("modal-add");
}
$("#btn-help").addEventListener("click", () => openAddModal("tips"));

// tabs: the outgoing panel fades out, the new one slides in
async function showTab(name, animated = true) {
  const cur = $("#modal-add .tab-panel:not([hidden])");
  const next = $(`#modal-add .tab-panel[data-panel="${name}"]`);
  $$("#modal-add .tab").forEach((t) => t.classList.toggle("active", t.dataset.tab === name));
  if (cur === next) return;
  if (cur && animated) await animate(cur, [{ opacity: 1, transform: "none" }, { opacity: 0, transform: "translateX(-8px)" }], { duration: 120, easing: EASE_IN });
  if (cur) { cur.hidden = true; cur.getAnimations().forEach((x) => x.cancel()); }
  next.hidden = false;
  $("#modal-add").scrollTop = 0;
  if (animated) await animate(next, [{ opacity: 0, transform: "translateX(10px)" }, { opacity: 1, transform: "none" }], { duration: 240, easing: EASE_OUT });
  next.getAnimations().forEach((x) => x.cancel());
}
$("#modal-add .tabs").addEventListener("click", (e) => { const t = e.target.closest(".tab"); if (t) showTab(t.dataset.tab); });

// copy buttons on every code box
$("#modal-add").addEventListener("click", async (e) => {
  const b = e.target.closest(".copy");
  if (!b) return;
  try {
    await navigator.clipboard.writeText(b.previousElementSibling.textContent);
    const icon = $(".material-symbols-outlined", b);
    b.classList.add("done"); icon.textContent = "check";
    setTimeout(() => { b.classList.remove("done"); icon.textContent = "content_copy"; }, 1400);
  } catch { toast("Couldn't copy — select it manually", "bad"); }
});

// ---------------- machine menu (update / restart agent) ----------------
let cardMenuHost = null;
async function openCardMenu(h, btn) {
  const pop = $("#card-pop");
  if (!pop.hidden) { await closeCardMenu(); if (cardMenuHost && cardMenuHost.id === h.id) return; }
  cardMenuHost = h;
  btn.classList.add("on");
  const ok = (h.caps || []).includes("update");
  $("#card-pop-label").textContent = h.name + (h.version ? " · agent v" + h.version : "");
  $$("#card-pop [data-cmd]").forEach((b) => (b.disabled = !ok));
  $("#card-pop-note").hidden = ok;
  $("#card-pop-note").textContent = "This agent is too old to update remotely. Update it once by hand (How-to → Windows PC → step 2), or connect and use Keys → Run… with Type text.";
  pop.hidden = false;
  const r = btn.getBoundingClientRect(), w = pop.offsetWidth;
  Object.assign(pop.style, { top: r.bottom + 6 + "px", left: Math.max(12, Math.min(innerWidth - w - 12, r.right - w)) + "px" });
  await animate(pop, [{ opacity: 0, transform: "translateY(-6px) scale(.97)" }, { opacity: 1, transform: "none" }], { duration: 200, easing: EASE_OUT });
  pop.getAnimations().forEach((a) => a.cancel());
}
async function closeCardMenu() {
  const pop = $("#card-pop");
  if (pop.hidden) return;
  $$(".host-more.on").forEach((b) => b.classList.remove("on"));
  await animate(pop, [{ opacity: 1 }, { opacity: 0, transform: "translateY(-4px)" }], { duration: 120, easing: EASE_IN });
  pop.hidden = true;
  pop.getAnimations().forEach((a) => a.cancel());
}
$("#card-pop").addEventListener("click", async (e) => {
  const b = e.target.closest("[data-cmd]");
  if (!b || b.disabled || !cardMenuHost) return;
  const h = cardMenuHost, cmd = b.dataset.cmd;
  await closeCardMenu();
  sendSignal({ type: "command", to: h.id, cmd });
  toast(cmd === "update" ? `Updating ${h.name}…` : `Restarting ${h.name}…`);
});
document.addEventListener("pointerdown", (e) => { if (!e.target.closest("#card-pop, .host-more")) closeCardMenu(); });
window.addEventListener("scroll", () => closeCardMenu(), { passive: true, capture: true });

// ---------------- signaling ----------------
function connectSignaling() {
  if (state.ws) { state.ws.onclose = null; state.ws.close(); }
  setConn("warn", "Connecting…");
  const ws = new WebSocket(wsURL(state.server));
  state.ws = ws;
  ws.onopen = () => ws.send(JSON.stringify({ type: "client-hello", token: state.token }));
  ws.onmessage = (e) => {
    let m; try { m = JSON.parse(e.data); } catch { return; }
    switch (m.type) {
      case "welcome":
        state.wsRetry = 0;
        if (m.ice) state.ice = m.ice;
        setConn("ok", "Server connected");
        renderHosts(m.hosts || [], true);
        break;
      case "hosts":
        renderHosts(m.hosts || []);
        break;
      case "auth-failed":
        ws.onclose = null;
        toast("Session expired — sign in again", "bad");
        logout();
        break;
      case "signal":
        if (session && session.sid === m.sid) session.onSignal(m.data);
        break;
      case "bye":
        if (session && session.sid === m.sid) endSession("The host ended the session");
        break;
      case "command-result":
        toast(m.msg || (m.ok ? "Done" : "Failed"), m.ok ? "ok" : "bad");
        break;
      case "error":
        if (session && session.sid === m.sid) endSession(m.error || "Connection failed");
        else toast(m.error || "Error", "bad");
        break;
    }
  };
  ws.onclose = () => {
    if (state.ws !== ws) return;
    setConn("warn", "Reconnecting…");
    const delay = Math.min(1000 * 2 ** state.wsRetry++, 15000);
    setTimeout(() => { if (state.ws === ws && state.token) connectSignaling(); }, delay);
  };
}

function sendSignal(msg) {
  if (state.ws && state.ws.readyState === 1) state.ws.send(JSON.stringify(msg));
}

// ---------------- session ----------------
const sessEl = $("#session");
const video = $("#remote-video");
const stage = $("#stage");

function cardClip(card) {
  if (!card || !card.isConnected) return null;
  const r = card.getBoundingClientRect();
  const W = innerWidth, H = innerHeight;
  return `inset(${r.top}px ${W - r.right}px ${H - r.bottom}px ${r.left}px)`;
}

async function startSession(host, card) {
  if (session) return;
  session = state.demo ? new DemoSession(host) : new RtcSession(host);
  session.card = card;
  $("#sess-name").textContent = host.name;
  $("#sess-stats").textContent = "";
  $("#stage-status").textContent = "Connecting to " + host.name + "…";
  $("#stage-overlay").classList.remove("gone", "problem");
  $("#info-drawer").hidden = true;
  $("#keys-pop").hidden = true;
  $("#screens-pop").hidden = true;
  $("#quality-pop").hidden = true;
  setScreens([], 0);
  setQuality(store.get("quality", "auto"), "", false);
  sessEl.hidden = false;
  document.body.style.overflow = "hidden";
  document.body.classList.add("in-session");

  const from = cardClip(card);
  const anim = from
    ? animate(sessEl, [{ clipPath: from, opacity: 0.6 }, { clipPath: "inset(0px 0px 0px 0px)", opacity: 1 }], { duration: 480, easing: EASE_OUT })
    : animate(sessEl, [{ opacity: 0, transform: "scale(1.04)" }, { opacity: 1, transform: "none" }], { duration: 450, easing: EASE_OUT });
  animate($("#toolbar"), [{ opacity: 0, transform: "translate(-50%, -16px)" }, { opacity: 1, transform: "translate(-50%, 0)" }], { duration: 300, delay: 280, easing: EASE_OUT })
    .then((a) => a.cancel());
  showToolbar();
  session.start();
  await anim;
  sessEl.getAnimations().forEach((a) => a.cancel());
  stage.focus();
}

async function endSession(reason) {
  const s = session;
  if (!s) return;
  session = null;
  s.stop();
  releaseAllKeys();
  if (document.fullscreenElement) await document.exitFullscreen().catch(() => {});
  if (!$("#info-drawer").hidden) closeEl($("#info-drawer"), "drawer");
  if (!$("#keys-pop").hidden) closeEl($("#keys-pop"), "pop");
  if (!$("#screens-pop").hidden) closeEl($("#screens-pop"), "pop");
  if (!$("#quality-pop").hidden) closeEl($("#quality-pop"), "pop");

  // the card may have been re-rendered: find it again by id
  const card = $(`.host-card[data-id="${CSS.escape(s.host.id)}"]`);
  const to = cardClip(card);
  if (to) await animate(sessEl, [{ clipPath: "inset(0px 0px 0px 0px)", opacity: 1 }, { clipPath: to, opacity: 0.4 }], { duration: 400, easing: "cubic-bezier(0.65, 0, 0.35, 1)" });
  else await animate(sessEl, [{ opacity: 1, transform: "none" }, { opacity: 0, transform: "scale(.97)" }], { duration: 300, easing: EASE_IN });

  sessEl.hidden = true;
  sessEl.getAnimations().forEach((a) => a.cancel());
  video.srcObject = null;
  document.body.style.overflow = "";
  document.body.classList.remove("in-session");
  stage.style.cursor = "";
  if (reason) toast(reason, reason.startsWith("Disconnected") ? "" : "bad");
  if (card) { card.classList.remove("enter"); void card.offsetWidth; card.classList.add("enter"); card.style.animationDelay = "0ms"; }
}

function onVideoReady() {
  $("#stage-overlay").classList.add("gone");
}

class RtcSession {
  constructor(host) {
    this.host = host;
    this.sid = crypto.randomUUID ? crypto.randomUUID() : String(Math.random()).slice(2);
    this.dc = null;
    this.pc = null;
    this.statsTimer = null;
    this.lastBytes = 0;
    this.lastTs = 0;
    this.info = {};
  }
  start() {
    const pc = (this.pc = new RTCPeerConnection({ iceServers: state.ice }));
    const tr = pc.addTransceiver("video", { direction: "recvonly" });
    // Prefer H.264 (what the agent sends)
    try {
      const caps = RTCRtpReceiver.getCapabilities("video");
      const h264 = caps.codecs.filter((c) => /h264/i.test(c.mimeType));
      if (h264.length) tr.setCodecPreferences(h264);
    } catch {}
    this.dc = pc.createDataChannel("input", { ordered: true });
    // Fast lane for mouse positions: never wait for a lost packet to be resent —
    // a newer position is already on its way.
    this.moveDc = pc.createDataChannel("move", { ordered: false, maxRetransmits: 0 });
    // With a real mouse, draw the pointer here (instant) instead of in the video.
    this.localCursor = matchMedia("(pointer: fine)").matches;
    if (this.localCursor) stage.style.cursor = "default";
    this.dc.onopen = () => { if (this.pendingScreen !== undefined) { this.send({ t: "screen", n: this.pendingScreen }); this.pendingScreen = undefined; } };
    this.dc.onmessage = (e) => {
      let m; try { m = JSON.parse(e.data); } catch { return; }
      if (m.t === "hello") {
        this.info = m;
        setScreens(m.screens || [], m.screen ?? 0);
        // restore the screen you used last time on this PC
        const want = store.get("screen:" + this.host.id, null);
        if (want !== null && want !== m.screen && screenExists(want)) this.send({ t: "screen", n: want });
        setQuality(m.quality || "auto", m.level || "high", !!m.relayed);
        const pref = store.get("quality", "auto");
        if (pref !== "auto" && pref !== m.level) this.send({ t: "quality", s: pref });
      } else if (m.t === "quality") {
        const prev = quality.level;
        setQuality(m.quality, m.level, !!m.relayed);
        if (prev !== m.level) toast("Quality: " + LEVEL_NAMES[m.level] + (m.quality === "auto" ? " (auto)" : ""), "ok");
      } else if (m.t === "cursor") {
        if (this.localCursor) stage.style.cursor = m.c || "default";
      } else if (m.t === "status") {
        // e.g. "Waking the screen…" while the agent recovers screen capture
        const ov = $("#stage-overlay");
        ov.classList.toggle("problem", !!m.err);
        if (m.s) {
          $("#stage-status").textContent = m.s;
          ov.classList.remove("gone");
          // the agent knows what's wrong: keep the session open (it recovers by itself
          // if the problem goes away) instead of replacing this with a generic timeout
          if (m.err) { clearTimeout(this.connectTimer); this.knownProblem = true; }
        } else {
          this.knownProblem = false;
          if (video.readyState >= 2) ov.classList.add("gone");
        }
      } else if (m.t === "screen") {
        setScreens(m.screens || screens.list, m.screen);
        $("#stage-overlay").classList.add("gone");
        toast("Showing " + screenName(m.screen), "ok");
      }
    };
    pc.ontrack = (e) => {
      try { e.receiver.playoutDelayHint = 0; e.receiver.jitterBufferTarget = 0; } catch {}
      video.srcObject = e.streams[0] || new MediaStream([e.track]);
      video.play().catch(() => {});
    };
    pc.onicecandidate = (e) => { if (e.candidate) sendSignal({ type: "signal", to: this.host.id, sid: this.sid, data: { candidate: e.candidate.toJSON() } }); };
    pc.onconnectionstatechange = () => {
      const st = pc.connectionState;
      if (st === "connecting") $("#stage-status").textContent = "Establishing a connection…";
      if (st === "connected") { this.connectedAt = performance.now(); $("#stage-status").textContent = "Connected — waiting for the picture…"; }
      if (st === "failed") endSession("Couldn't reach the host — check the TURN relay is set up on the server");
      if (st === "disconnected") $("#stage-status").textContent = "Connection interrupted…";
    };
    (async () => {
      await pc.setLocalDescription(await pc.createOffer());
      sendSignal({ type: "signal", to: this.host.id, sid: this.sid, data: { sdp: pc.localDescription, localCursor: this.localCursor } });
    })().catch((e) => endSession("Couldn't start: " + e.message));

    // Not connected at all after 25s: give up. Connected but no picture yet: auto quality
    // steps down (see stats) and we keep waiting up to 50s before giving up.
    this.connectTimer = setTimeout(() => {
      if (session !== this || video.readyState >= 2) return;
      if (this.pc.connectionState !== "connected") return endSession("Timed out connecting to the host");
      this.connectTimer = setTimeout(() => {
        if (session === this && video.readyState < 2) endSession("Connected, but no picture arrived — the connection may be too slow. Try quality: Low.");
      }, 25000);
    }, 25000);
    this.statsTimer = setInterval(() => this.stats(), 1000);
  }
  async onSignal(d) {
    if (!d) return;
    try {
      if (d.sdp) await this.pc.setRemoteDescription(d.sdp);
      else if (d.candidate) await this.pc.addIceCandidate(d.candidate);
    } catch (e) { console.warn(e); }
  }
  send(ev) {
    const ch = ev.t === "mm" && this.moveDc && this.moveDc.readyState === "open" ? this.moveDc : this.dc;
    if (ch && ch.readyState === "open") ch.send(JSON.stringify(ev));
  }
  async stats() {
    if (!this.pc) return;
    const rep = await this.pc.getStats();
    let fps = 0, bytes = 0, rtt = null, w = 0, h = 0, codec = "", local = "", remote = "";
    const byId = new Map(); rep.forEach((r) => byId.set(r.id, r));
    rep.forEach((r) => {
      if (r.type === "inbound-rtp" && r.kind === "video") {
        fps = r.framesPerSecond || 0; bytes = r.bytesReceived; w = r.frameWidth; h = r.frameHeight;
        this.health(r);
        const c = byId.get(r.codecId); if (c) codec = c.mimeType.split("/")[1];
      }
      if (r.type === "candidate-pair" && r.nominated && r.state === "succeeded") {
        rtt = r.currentRoundTripTime;
        const l = byId.get(r.localCandidateId), rm = byId.get(r.remoteCandidateId);
        if (l) local = l.candidateType; if (rm) remote = rm.candidateType;
      }
    });
    const now = performance.now();
    const mbps = this.lastTs ? ((bytes - this.lastBytes) * 8) / ((now - this.lastTs) * 1000) : 0;
    this.lastBytes = bytes; this.lastTs = now;
    const ms = rtt != null ? Math.round(rtt * 1000) + " ms" : "–";
    $("#sess-stats").textContent = `${Math.round(fps)} fps · ${mbps.toFixed(1)} Mbps · ${ms}`;
    updateInfo({
      Host: this.host.name, OS: this.info.os || this.host.os || "–", Agent: this.info.version ? "v" + this.info.version : "–",
      Encoder: this.info.encoder || "–", Codec: codec || "–", Screen: screens.list.length ? screenName(screens.current).replace(/^./, (c) => c.toUpperCase()) : "–", Resolution: w ? `${w}×${h}` : "–",
      Framerate: Math.round(fps) + " fps", Bitrate: mbps.toFixed(2) + " Mbps", Latency: ms,
      Route: remote === "relay" || local === "relay" ? "Relayed (TURN)" : local ? "Direct (peer-to-peer)" : "–",
      Quality: quality.level ? LEVEL_NAMES[quality.level] + (quality.choice === "auto" ? " (auto)" : "") : "–",
    });
  }
  // Auto quality: if frames stop getting through or loss is heavy, step down a level.
  health(r) {
    const now = performance.now();
    const dec = r.framesDecoded || 0, lost = r.packetsLost || 0, got = r.packetsReceived || 0;
    if (this.h) {
      const dLost = lost - this.h.lost, dGot = got - this.h.got;
      if (dec > this.h.dec) this.lastFrameAt = now;
      this.badLoss = dGot + dLost > 20 && dLost / (dGot + dLost) > 0.08 ? (this.badLoss || 0) + 1 : 0;
    }
    this.h = { dec, lost, got };
    if (quality.choice !== "auto" || !this.connectedAt || !quality.level || this.knownProblem) return;
    const since = now - (this.lastFrameAt || this.connectedAt);
    const stalled = since > (this.lastFrameAt ? 6000 : 8000);
    if ((stalled || this.badLoss >= 4) && now - (this.lastStep || 0) > 9000) {
      const next = { high: "balanced", balanced: "low" }[quality.level];
      if (!next) return;
      this.lastStep = now; this.badLoss = 0; this.lastFrameAt = now;
      $("#stage-status").textContent = "Weak connection — lowering quality…";
      this.send({ t: "quality", s: "auto", c: next });
    }
  }
  stop() {
    clearInterval(this.statsTimer); clearTimeout(this.connectTimer);
    sendSignal({ type: "bye", to: this.host.id, sid: this.sid });
    try { this.pc && this.pc.close(); } catch {}
  }
}

video.addEventListener("loadeddata", () => { if (session) onVideoReady(); });

function updateInfo(obj) {
  const dl = $("#info-list");
  dl.innerHTML = Object.entries(obj).map(([k, v]) => `<dt>${k}</dt><dd>${String(v).replace(/</g, "&lt;")}</dd>`).join("");
}

// ---------------- input ----------------
function contentRect() {
  // where the picture actually is inside the <video> (object-fit: contain letterboxing)
  const el = session && session.canvas ? session.canvas : video;
  const r = el.getBoundingClientRect();
  const vw = el.videoWidth || el.width || 16, vh = el.videoHeight || el.height || 9;
  const scale = Math.min(r.width / vw, r.height / vh);
  const w = vw * scale, h = vh * scale;
  return { x: r.left + (r.width - w) / 2, y: r.top + (r.height - h) / 2, w, h };
}
function norm(e) {
  const c = contentRect();
  return { x: Math.min(1, Math.max(0, (e.clientX - c.x) / c.w)), y: Math.min(1, Math.max(0, (e.clientY - c.y) / c.h)) };
}

let pendingMove = null, lastMoveAt = 0, moveTimer = 0;
function flushMove() {
  moveTimer = 0;
  if (pendingMove && session) {
    session.moveSeq = (session.moveSeq || 0) + 1; // lets the agent drop positions that arrive late
    session.send({ t: "mm", n: session.moveSeq, ...pendingMove });
    lastMoveAt = performance.now();
  }
  pendingMove = null;
}
stage.addEventListener("pointermove", (e) => {
  if (!session) return;
  pendingMove = norm(e);
  const wait = 4 - (performance.now() - lastMoveAt);
  if (wait <= 0) flushMove();
  else if (!moveTimer) moveTimer = setTimeout(flushMove, wait);
});
stage.addEventListener("pointerdown", (e) => {
  if (!session) return;
  stage.focus();
  stage.setPointerCapture(e.pointerId);
  session.send({ t: "md", b: e.button, ...norm(e) });
  e.preventDefault();
});
stage.addEventListener("pointerup", (e) => { if (session) session.send({ t: "mu", b: e.button, ...norm(e) }); });
stage.addEventListener("contextmenu", (e) => e.preventDefault());
stage.addEventListener("auxclick", (e) => e.preventDefault());
stage.addEventListener("wheel", (e) => {
  if (!session) return;
  e.preventDefault();
  const k = e.deltaMode === 1 ? 33 : e.deltaMode === 2 ? 400 : 1;
  const inv = prefs.invertScroll ? -1 : 1;
  session.send({ t: "wh", dx: e.deltaX * k * inv, dy: e.deltaY * k * inv });
}, { passive: false });

const held = new Set();
function keyHandler(e) {
  if (!session || openModalEl || document.activeElement !== stage) return;
  if (!e.code) return;
  e.preventDefault();
  if (e.type === "keydown") { held.add(e.code); session.send({ t: "kd", c: e.code }); }
  else { held.delete(e.code); session.send({ t: "ku", c: e.code }); }
}
document.addEventListener("keydown", keyHandler);
document.addEventListener("keyup", keyHandler);
function releaseAllKeys() {
  for (const c of held) session && session.send({ t: "ku", c });
  held.clear();
}
window.addEventListener("blur", releaseAllKeys);
stage.addEventListener("blur", releaseAllKeys);

async function sendCombo(combo) {
  if (!session) return;
  const keys = combo.split("+");
  for (const c of keys) { session.send({ t: "kd", c }); await new Promise((r) => setTimeout(r, 30)); }
  for (const c of keys.reverse()) { session.send({ t: "ku", c }); await new Promise((r) => setTimeout(r, 30)); }
  toast("Sent " + combo.replace(/Left|Key/g, "").replace(/\+/g, " + ").replace("Meta", "Win"));
}

// ---------------- quality ----------------
const LEVEL_NAMES = { high: "High", balanced: "Balanced", low: "Low" };
const quality = { choice: "auto", level: "", relayed: false };

function setQuality(choice, level, relayed) {
  Object.assign(quality, { choice, level, relayed });
  $$("#quality-pop [data-q]").forEach((b) => b.classList.toggle("current", b.dataset.q === choice));
  $("#q-auto-desc").textContent = choice === "auto" && level
    ? `Now ${LEVEL_NAMES[level]}${relayed ? " · via relay" : ""}` : "Adjusts to your connection";
}

$("#tb-quality").addEventListener("click", async (e) => {
  e.stopPropagation();
  const pop = $("#quality-pop");
  for (const [p, b] of [["#keys-pop", "#tb-keys"], ["#screens-pop", "#tb-screens"]]) {
    if (!$(p).hidden) { $(b).classList.remove("on"); closeEl($(p), "pop"); }
  }
  if (pop.hidden) { $("#tb-quality").classList.add("on"); await openEl(pop, "pop"); }
  else { $("#tb-quality").classList.remove("on"); await closeEl(pop, "pop"); }
});
$("#quality-pop").addEventListener("click", async (e) => {
  const b = e.target.closest("[data-q]");
  if (!b || !session) return;
  $("#tb-quality").classList.remove("on");
  await closeEl($("#quality-pop"), "pop");
  stage.focus();
  store.set("quality", b.dataset.q);
  session.send({ t: "quality", s: b.dataset.q });
});
document.addEventListener("pointerdown", (e) => {
  const pop = $("#quality-pop");
  if (!pop.hidden && !e.target.closest("#quality-pop, #tb-quality")) { $("#tb-quality").classList.remove("on"); closeEl(pop, "pop"); }
});

// ---------------- screens ----------------
const screens = { list: [], current: 0 };
const screenExists = (n) => (n === -1 && screens.list.length > 1) || screens.list.some((s) => s.i === n);
const screenName = (n) => (n === -1 ? "all screens" : (screens.list.find((s) => s.i === n) || { name: "screen" }).name.toLowerCase().replace(/^screen/, "screen"));

function setScreens(list, current) {
  screens.list = list || [];
  screens.current = current;
  const multi = screens.list.length > 1;
  $("#tb-screens").hidden = !multi;
  $("#tb-screens-badge").textContent = !multi ? "" : current === -1 ? "∗" : String(screens.list.findIndex((s) => s.i === current) + 1);
  renderScreens();
}

function renderScreens() {
  const list = $("#screens-list");
  const items = screens.list.map((s, k) => ({ n: s.i, icon: s.primary ? "desktop_windows" : "monitor", label: s.name + (s.primary ? " · main" : ""), dims: `${s.w}×${s.h}` }));
  if (screens.list.length > 1) {
    const xs = screens.list.map((s) => [s.x, s.y, s.x + s.w, s.y + s.h]);
    const W = Math.max(...xs.map((r) => r[2])) - Math.min(...xs.map((r) => r[0]));
    const H = Math.max(...xs.map((r) => r[3])) - Math.min(...xs.map((r) => r[1]));
    items.push({ n: -1, icon: "view_column", label: "All screens", dims: `${W}×${H}` });
  }
  list.innerHTML = "";
  for (const it of items) {
    const b = document.createElement("button");
    b.className = it.n === screens.current ? "current" : "";
    b.innerHTML = `<span class="material-symbols-outlined">${it.icon}</span><span></span><span class="screen-dims">${it.dims}</span>`;
    b.children[1].textContent = it.label;
    b.addEventListener("click", () => pickScreen(it.n));
    list.append(b);
  }
  // mini map of how the monitors are arranged
  const map = $("#screens-map");
  map.innerHTML = "";
  map.classList.toggle("all", screens.current === -1);
  if (screens.list.length < 2) { map.hidden = true; return; }
  map.hidden = false;
  const minX = Math.min(...screens.list.map((s) => s.x)), minY = Math.min(...screens.list.map((s) => s.y));
  const maxX = Math.max(...screens.list.map((s) => s.x + s.w)), maxY = Math.max(...screens.list.map((s) => s.y + s.h));
  const box = { w: 230, h: 70 };
  const k = Math.min(box.w / (maxX - minX), box.h / (maxY - minY));
  const offX = (box.w - (maxX - minX) * k) / 2, offY = (box.h - (maxY - minY) * k) / 2;
  screens.list.forEach((s, idx) => {
    const d = document.createElement("div");
    Object.assign(d.style, { left: offX + (s.x - minX) * k + "px", top: offY + (s.y - minY) * k + "px", width: s.w * k - 3 + "px", height: s.h * k - 3 + "px" });
    d.textContent = idx + 1;
    if (s.i === screens.current) d.className = "current";
    d.addEventListener("click", () => pickScreen(s.i));
    map.append(d);
  });
}

async function pickScreen(n) {
  if (!session) return;
  $("#tb-screens").classList.remove("on");
  await closeEl($("#screens-pop"), "pop");
  stage.focus();
  if (n === screens.current) return;
  store.set("screen:" + session.host.id, n);
  session.send({ t: "screen", n });
}

$("#tb-screens").addEventListener("click", async (e) => {
  e.stopPropagation();
  const pop = $("#screens-pop");
  if (!$("#keys-pop").hidden) { $("#tb-keys").classList.remove("on"); closeEl($("#keys-pop"), "pop"); }
  if (pop.hidden) { renderScreens(); $("#tb-screens").classList.add("on"); await openEl(pop, "pop"); }
  else { $("#tb-screens").classList.remove("on"); await closeEl(pop, "pop"); }
});
document.addEventListener("pointerdown", (e) => {
  const pop = $("#screens-pop");
  if (!pop.hidden && !e.target.closest("#screens-pop, #tb-screens")) { $("#tb-screens").classList.remove("on"); closeEl(pop, "pop"); }
});

// ---------------- toolbar ----------------
let hideTimer = null;
function showToolbar() {
  $("#toolbar").classList.remove("tucked");
  clearTimeout(hideTimer);
  if (prefs.autoHide) hideTimer = setTimeout(() => {
    if (!$("#keys-pop").hidden || !$("#screens-pop").hidden || !$("#quality-pop").hidden || $("#toolbar").matches(":hover")) return showToolbar();
    $("#toolbar").classList.add("tucked");
  }, 2600);
}
sessEl.addEventListener("pointermove", (e) => { if (e.clientY < 70) showToolbar(); });
$("#toolbar").addEventListener("pointerenter", showToolbar);
$("#toolbar-handle").addEventListener("pointerenter", showToolbar);

$("#tb-close").addEventListener("click", () => endSession("Disconnected"));
$("#tb-full").addEventListener("click", async () => {
  if (document.fullscreenElement) { await document.exitFullscreen(); return; }
  try {
    await sessEl.requestFullscreen({ navigationUI: "hide" });
    if (navigator.keyboard && navigator.keyboard.lock) await navigator.keyboard.lock();
  } catch {}
  stage.focus();
});
document.addEventListener("fullscreenchange", () => {
  const on = !!document.fullscreenElement;
  $("#tb-full").classList.toggle("on", on);
  $("#tb-full .material-symbols-outlined").textContent = on ? "fullscreen_exit" : "fullscreen";
});

$("#tb-keys").addEventListener("click", async (e) => {
  e.stopPropagation();
  const pop = $("#keys-pop");
  if (!$("#screens-pop").hidden) { $("#tb-screens").classList.remove("on"); closeEl($("#screens-pop"), "pop"); }
  if (pop.hidden) { $("#tb-keys").classList.add("on"); await openEl(pop, "pop"); }
  else { $("#tb-keys").classList.remove("on"); await closeEl(pop, "pop"); }
});
$("#keys-pop").addEventListener("click", async (e) => {
  const b = e.target.closest("[data-combo]");
  if (!b) return;
  $("#tb-keys").classList.remove("on");
  await closeEl($("#keys-pop"), "pop");
  sendCombo(b.dataset.combo);
  stage.focus();
});
document.addEventListener("pointerdown", (e) => {
  const pop = $("#keys-pop");
  if (!pop.hidden && !e.target.closest("#keys-pop, #tb-keys")) { $("#tb-keys").classList.remove("on"); closeEl(pop, "pop"); }
});

$("#tb-info").addEventListener("click", async () => {
  const d = $("#info-drawer");
  if (d.hidden) { $("#tb-info").classList.add("on"); await openEl(d, "drawer"); }
  else { $("#tb-info").classList.remove("on"); await closeEl(d, "drawer"); }
});
$("[data-close-drawer]").addEventListener("click", async () => { $("#tb-info").classList.remove("on"); await closeEl($("#info-drawer"), "drawer"); stage.focus(); });

$("#tb-paste").addEventListener("click", () => openModal("modal-paste"));
$("#btn-send-text").addEventListener("click", async () => {
  const s = $("#paste-text").value;
  if (!s || !session) return;
  session.send({ t: "text", s });
  if ($("#paste-enter").checked) {
    await new Promise((r) => setTimeout(r, 150));
    session.send({ t: "kd", c: "Enter" }); session.send({ t: "ku", c: "Enter" });
  }
  $("#paste-text").value = "";
  await closeModal();
  toast("Typed " + s.length + " characters", "ok");
});

document.addEventListener("keydown", (e) => {
  if (e.key === "Escape" && openModalEl) { e.preventDefault(); closeModal(); }
});

// ---------------- settings ----------------
function initSettings() {
  const bind = (id, key, after) => {
    const el = $("#" + id);
    el.checked = prefs[key];
    el.addEventListener("change", () => { prefs[key] = el.checked; savePrefs(); after && after(); });
  };
  bind("set-scroll-invert", "invertScroll");
  bind("set-autohide", "autoHide", showToolbar);
  bind("set-motion", "reduceMotion", () => document.documentElement.classList.toggle("reduce-motion", prefs.reduceMotion));
  document.documentElement.classList.toggle("reduce-motion", prefs.reduceMotion);
}

// =====================================================================
// DEMO MODE — a fake desktop drawn on a canvas, so the UI can be tried
// on GitHub Pages without a server. Your mouse and typing reach it.
// =====================================================================
const DEMO_HOSTS = [
  { id: "demo-maze-pc", name: "MAZE-PC", os: "windows", since: Date.now() - 1000 * 60 * 47 },
  { id: "demo-studio", name: "Studio Laptop", os: "windows", since: Date.now() - 1000 * 60 * 60 * 5, busy: false },
  { id: "demo-pi", name: "raspberrypi", os: "linux", since: Date.now() - 1000 * 60 * 60 * 26 },
  { id: "demo-old", name: "Old Tower", os: "windows", online: false },
];

class DemoSession {
  constructor(host) {
    this.host = host;
    this.sid = "demo";
    this.cursor = { x: 0.5, y: 0.5 };
    this.typed = "";
    this.down = false;
    this.ripples = [];
    this.scroll = 0;
    this.t0 = performance.now();
    this.log = ["online as \"" + host.name + "\"", "viewer connecting (session demo)", "peer connection: connected", "▶ streaming to viewer"];
  }
  start() {
    // Draw straight onto a canvas in the stage (no video encode/decode round-trip),
    // with everything static pre-rendered once, and only repaint when something changes.
    const c = (this.canvas = document.createElement("canvas"));
    c.width = 1600; c.height = 900;
    c.className = "demo-canvas";
    this.ctx = c.getContext("2d", { alpha: false });
    this.bg = this.renderStatic();
    if (document.fonts) document.fonts.ready.then(() => { if (session === this) { this.bg = this.renderStatic(); this.dirty = true; } });
    this.dirty = true;
    video.hidden = true;
    stage.prepend(c);
    setTimeout(() => {
      if (session !== this) return;
      onVideoReady();
      setScreens([
        { i: 0, name: "Screen 1", x: 0, y: 0, w: 2560, h: 1440, primary: true },
        { i: 1, name: "Screen 2", x: 2560, y: 180, w: 1920, h: 1080 },
      ], store.get("screen:" + this.host.id, 0));
      setQuality(store.get("quality", "auto"), store.get("quality", "auto") === "auto" ? "high" : store.get("quality", "auto"), false);
      this.screenLabel = screenName(screens.current);
      this.bg = this.renderStatic(); this.dirty = true;
    }, 700);
    this.raf = requestAnimationFrame(this.loop);
    this.blink = setInterval(() => { this.dirty = true; }, 500);
    this.statsTimer = setInterval(() => {
      const fps = 58 + Math.round(Math.random() * 2), mbps = (3.5 + Math.random() * 1.5).toFixed(1), ms = 2 + Math.round(Math.random() * 3);
      $("#sess-stats").textContent = `${fps} fps · ${mbps} Mbps · ${ms} ms`;
      updateInfo({ Host: this.host.name, OS: this.host.os, Agent: "v0.1.0 (demo)", Encoder: "x264", Codec: "H264", Resolution: "1600×900", Framerate: fps + " fps", Bitrate: mbps + " Mbps", Latency: ms + " ms", Route: "Direct (peer-to-peer)" });
    }, 1000);
  }
  loop = () => {
    if (this.dirty || this.ripples.length) { this.dirty = false; this.draw(); }
    this.raf = requestAnimationFrame(this.loop);
  };
  onSignal() {}
  send(ev) {
    this.dirty = true;
    if (ev.t === "quality") {
      const level = ev.s === "auto" ? "high" : ev.s;
      setTimeout(() => { if (session === this) { setQuality(ev.s, level, false); toast("Quality: " + LEVEL_NAMES[level] + (ev.s === "auto" ? " (auto)" : ""), "ok"); } }, 300);
      return;
    }
    if (ev.t === "screen") {
      setTimeout(() => {
        if (session !== this) return;
        setScreens(screens.list, ev.n);
        this.screenLabel = screenName(ev.n);
        this.bg = this.renderStatic(); this.dirty = true;
        toast("Showing " + this.screenLabel, "ok");
      }, 350);
      return;
    }
    if (ev.t === "mm") Object.assign(this.cursor, { x: ev.x, y: ev.y });
    else if (ev.t === "md") { this.down = true; this.cursor.x = ev.x ?? this.cursor.x; this.cursor.y = ev.y ?? this.cursor.y; this.ripples.push({ x: this.cursor.x, y: this.cursor.y, t: performance.now(), b: ev.b }); }
    else if (ev.t === "mu") this.down = false;
    else if (ev.t === "kd") {
      const c = ev.c;
      if (c === "Backspace") this.typed = this.typed.slice(0, -1);
      else if (c === "Enter") this.typed += "\n";
      else if (c === "Space") this.typed += " ";
      else if (c.startsWith("Key")) this.typed += held.has("ShiftLeft") || held.has("ShiftRight") ? c[3] : c[3].toLowerCase();
      else if (c.startsWith("Digit")) this.typed += c[5];
      else if (c === "Period") this.typed += ".";
      else if (c === "Comma") this.typed += ",";
      else if (!/^(Shift|Control|Alt|Meta)/.test(c)) this.log.push("key " + c);
      if (this.typed.length > 400) this.typed = this.typed.slice(-400);
    } else if (ev.t === "text") this.typed += ev.s;
  }
  stop() {
    cancelAnimationFrame(this.raf); clearInterval(this.statsTimer); clearInterval(this.blink);
    setTimeout(() => { this.canvas.remove(); video.hidden = false; }, 600);
  }

  // Everything that never changes: wallpaper, watermark, window frames, taskbar.
  renderStatic() {
    const W = 1600, H = 900;
    const c = document.createElement("canvas"); c.width = W; c.height = H;
    const g = c.getContext("2d");
    g.fillStyle = "#0d0b09"; g.fillRect(0, 0, W, H);
    // dot grain wallpaper
    g.fillStyle = "rgba(236,232,223,.06)";
    for (let y = 0; y < H; y += 6) for (let x = (y / 6) % 2 ? 3 : 0; x < W; x += 6) g.fillRect(x, y, 1, 1);
    // big serif wordmark on the wallpaper
    g.fillStyle = "rgba(236,232,223,.07)"; g.font = "400 150px 'Instrument Serif', Georgia, serif";
    g.fillText("Maze", 120, 760);
    const mw = g.measureText("Maze ").width;
    g.fillStyle = "rgba(199,114,254,.16)"; g.font = "italic 400 150px 'Instrument Serif', Georgia, serif";
    g.fillText("remote.", 120 + mw, 760);
    if (this.screenLabel) {
      g.fillStyle = "#88826f"; g.font = "500 13px 'Geist Mono', monospace";
      g.fillText(("demo · " + this.screenLabel).toUpperCase(), 120, 800);
    }
    this.winFrame(g, ...this.notepadRect, "notes.txt — Notepad", "#161310");
    this.winFrame(g, ...this.consoleRect, "maze-remote-agent.exe", "#0d0b09");
    // taskbar
    g.fillStyle = "#161310"; g.fillRect(0, H - 56, W, 56);
    g.fillStyle = "rgba(236,232,223,.22)"; g.fillRect(0, H - 56, W, 1);
    for (let i = 0; i < 6; i++) {
      g.fillStyle = i === 0 ? "#c772fe" : "#262019";
      g.fillRect(W / 2 - 165 + i * 56, H - 46, 36, 36);
    }
    return c;
  }
  get notepadRect() { return [120, 90, 640, 380]; }
  get consoleRect() { return [860, 150, 620, 330]; }

  draw() {
    const g = this.ctx, W = 1600, H = 900, t = (performance.now() - this.t0) / 1000;
    g.drawImage(this.bg, 0, 0);

    // notepad text
    let [x, y] = this.notepadRect;
    g.fillStyle = "#ece8df"; g.font = "400 20px 'Geist Mono', monospace";
    const text = (this.typed || "Click here and type — your keyboard\nreaches this demo PC.") + (Math.floor(t * 2) % 2 ? "▍" : "");
    let ly = y + 74;
    const lines = [];
    text.split("\n").forEach((line) => (line.match(/.{1,46}/g) || [""]).forEach((ch) => lines.push(ch)));
    lines.slice(-11).forEach((ch) => { g.fillText(ch, x + 20, ly); ly += 28; });

    // console text
    [x, y] = this.consoleRect;
    g.font = "400 16px 'Geist Mono', monospace";
    this.log.slice(-9).forEach((l, i) => {
      g.fillStyle = "#57534a"; g.fillText("13:0" + (i % 10), x + 16, y + 72 + i * 30);
      g.fillStyle = l.startsWith("▶") ? "#6fcf97" : "#c8c2b3"; g.fillText(l, x + 86, y + 72 + i * 30);
    });

    // clock
    const now = new Date();
    g.fillStyle = "#ece8df"; g.font = "500 16px 'Geist Mono', monospace"; g.textAlign = "right";
    g.fillText(now.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }), W - 24, H - 30);
    g.fillStyle = "#88826f"; g.font = "400 12px 'Geist Mono', monospace";
    g.fillText(now.toLocaleDateString(), W - 24, H - 12); g.textAlign = "left";

    // click ripples
    const nowMs = performance.now();
    this.ripples = this.ripples.filter((r) => nowMs - r.t < 600);
    this.ripples.forEach((r) => {
      const p = (nowMs - r.t) / 600;
      g.strokeStyle = r.b === 2 ? `rgba(226,104,95,${1 - p})` : `rgba(199,114,254,${1 - p})`;
      g.lineWidth = 2; const sz = 10 + p * 34; g.strokeRect(r.x * W - sz, r.y * H - sz, sz * 2, sz * 2);
    });

    // cursor
    const cx = this.cursor.x * W, cy = this.cursor.y * H;
    g.save(); g.translate(cx, cy); if (this.down) g.scale(0.9, 0.9);
    g.beginPath(); g.moveTo(0, 0); g.lineTo(0, 26); g.lineTo(7, 20); g.lineTo(12, 31); g.lineTo(16, 29); g.lineTo(11, 18); g.lineTo(20, 18); g.closePath();
    g.fillStyle = "#fff"; g.fill(); g.strokeStyle = "#000"; g.lineWidth = 1.5; g.stroke(); g.restore();
  }
  round(g, x, y, w, h, r) { g.beginPath(); g.moveTo(x + r, y); g.arcTo(x + w, y, x + w, y + h, r); g.arcTo(x + w, y + h, x, y + h, r); g.arcTo(x, y + h, x, y, r); g.arcTo(x, y, x + w, y, r); g.closePath(); }
  winFrame(g, x, y, w, h, title, bodyColor) {
    g.save();
    g.shadowColor = "rgba(0,0,0,.5)"; g.shadowBlur = 30; g.shadowOffsetY = 10;
    g.fillStyle = bodyColor; g.fillRect(x, y, w, h);
    g.restore();
    g.fillStyle = "#1e1a15"; g.fillRect(x, y, w, 40);
    g.fillStyle = "rgba(236,232,223,.10)"; g.fillRect(x, y + 40, w, 1);
    g.fillStyle = "#88826f"; g.font = "500 12px 'Geist Mono', monospace";
    g.fillText(title.toUpperCase(), x + 16, y + 25);
    ["#e2685f", "#57534a", "#57534a"].forEach((c, i) => { g.fillStyle = c; g.fillRect(x + w - 26 - i * 20, y + 16, 9, 9); });
    g.strokeStyle = "rgba(236,232,223,.22)"; g.lineWidth = 1; g.strokeRect(x + .5, y + .5, w - 1, h - 1);
  }
}

// ---------------- boot ----------------
initLogin();
initSettings();
const params = new URLSearchParams(location.search);
if (params.has("demo")) { state.demo = true; enterDashboard(); }
else if (state.token && state.server) enterDashboard();
else showView("view-login");
