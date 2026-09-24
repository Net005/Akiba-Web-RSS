"use strict";
/* Akiba-Web control panel — vanilla JS, no build step. */
const $ = (s, r = document) => r.querySelector(s);
const $$ = (s, r = document) => [...r.querySelectorAll(s)];
const esc = s => String(s ?? "").replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
const view = $("#view");

const store = {
  get(k, d) { try { const v = localStorage.getItem("aw." + k); return v === null ? d : JSON.parse(v); } catch { return d; } },
  set(k, v) { try { localStorage.setItem("aw." + k, JSON.stringify(v)); } catch {} },
};

async function api(path, opts = {}) {
  const o = { method: opts.body ? "POST" : "GET", headers: {}, ...opts };
  if (opts.body && typeof opts.body !== "string") { o.body = JSON.stringify(opts.body); o.headers["Content-Type"] = "application/json"; }
  const r = await fetch(path, o);
  let j = null; try { j = await r.json(); } catch {}
  if (r.status === 401 && !path.startsWith("/api/auth/")) { location.href = "/login?next=" + encodeURIComponent(location.pathname + location.hash); throw new Error("signed out"); }
  if (!r.ok) throw new Error((j && j.error) || r.statusText);
  return j;
}
function toast(msg, kind = "") {
  const t = document.createElement("div"); t.className = "toast " + kind; t.textContent = msg;
  $("#toasts").append(t); setTimeout(() => t.remove(), 4200);
}
async function act(fn, okMsg) {
  try { const r = await fn(); if (okMsg) toast(okMsg, "ok"); return r; }
  catch (e) { toast(e.message, "err"); }
}
const fmtDur = s => { s = Math.max(0, Math.round(s)); const h = Math.floor(s / 3600), m = Math.floor(s % 3600 / 60), x = s % 60; return h ? `${h}h ${m}m` : m ? `${m}m ${x}s` : `${x}s`; };
const ago = iso => iso ? fmtDur((Date.now() - new Date(iso)) / 1000) + " ago" : "—";
const when = iso => iso ? new Date(iso).toLocaleString() : "—";

/* ───────────── status polling ───────────── */
let status = null;
async function pollStatus() {
  try { status = await api("/api/status"); } catch { status = null; }
  paintPill(); if (page.status) page.status();
  $("#who").textContent = status && status.user ? status.user : "";
}
$("#btnOut").onclick = async () => { try { await api("/api/auth/logout", { method: "POST", body: {} }); } catch {} location.href = "/login"; };
function paintPill() {
  const p = $("#pill"), b = $("#btnRun");
  if (!status) { p.className = "pill"; $("span", p).textContent = "offline"; b.disabled = true; return; }
  const run = status.pipeline.running;
  p.className = "pill " + (run ? "run" : "idle");
  $("span", p).textContent = run ? (status.pipeline.phase || "running") : "idle";
  b.disabled = run; b.textContent = run ? "Running…" : "Run now";
}
$("#btnRun").onclick = () => act(() => api("/api/run", { method: "POST", body: {} }), "Pipeline started").then(() => setTimeout(pollStatus, 400));
setInterval(pollStatus, 2000);

/* ───────────── live log (SSE) ───────────── */
const LOGMAX = 5000;
const logs = [];
let lastLogId = 0, sse = null, paused = false, pausedQueue = [];
function connectSSE() {
  sse = new EventSource("/api/logs/stream?since=" + lastLogId);
  sse.onopen = () => $("#logdot").className = "dot on";
  sse.onerror = () => { $("#logdot").className = "dot off"; fetch("/api/auth/state").then(r => r.json()).then(s => { if (!s.authenticated) location.href = "/login"; }).catch(() => {}); };
  sse.onmessage = ev => {
    const e = JSON.parse(ev.data); lastLogId = e.id;
    if (paused) { pausedQueue.push(e); page.logPaused && page.logPaused(pausedQueue.length); return; }
    pushLog(e);
  };
}
function pushLog(e) {
  logs.push(e); if (logs.length > LOGMAX) logs.shift();
  page.log && page.log(e);
}
connectSSE();

/* ───────────── router ───────────── */
let page = {};
const routes = { dashboard, log: logPage, releases, runs, system, settings };
function route() {
  const name = (location.hash.replace(/^#\//, "") || "dashboard").split("/")[0];
  const fn = routes[name] || dashboard;
  $$("#nav a").forEach(a => a.classList.toggle("on", a.dataset.p === name));
  page = {}; view.innerHTML = ""; fn(); paintPill();
}
window.addEventListener("hashchange", route);

/* ───────────── log line rendering ───────────── */
function logLine(e, q, short) {
  let m = esc(e.msg);
  if (q) m = m.replace(new RegExp(q.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"), "gi"), x => `<mark>${x}</mark>`);
  return `<div class="ll ${esc(e.level)}" data-id="${e.id}"><span class="t">${esc(short ? (e.time || "").slice(11) : e.time)}</span><span class="l">${esc(e.level)}</span><span class="m">${m}</span></div>`;
}

/* ───────────── dashboard ───────────── */
function dashboard() {
  view.innerHTML = `
  <div class="page-h"><div><h1>Dashboard</h1><hr class="rule"><p>Scrape, match and dispatch — at a glance.</p></div>
    <div class="row"><button class="btn danger" id="dStop">Stop run</button><button class="btn primary" id="dRun">Run pipeline now</button></div></div>
  <div class="card hero" style="margin-bottom:16px">
    <div class="row sp"><div><div class="stat"><div class="k">Pipeline</div><div class="v" id="dPhase" style="font-size:34px">—</div></div>
      <div class="mute" id="dCur" style="margin-top:4px"></div></div>
      <div style="text-align:right"><div class="mute">Next scheduled run</div><div style="font-family:var(--serif);font-size:28px" id="dNext">—</div><div class="dim" id="dLast"></div></div></div>
    <div class="bar" style="margin-top:18px"><i id="dBar" style="width:0"></i></div>
    <div class="row sp mute" style="margin-top:8px;font-size:12px"><span id="dProg"></span><span id="dReason"></span></div>
  </div>
  <div class="grid g4" id="dStats"></div>
  <div class="grid g2" style="margin-top:16px">
    <div class="card"><h3>Recent activity <a href="#/log" style="font:12px var(--sans)">open live log →</a></h3><div class="logwrap"><div class="log mini wrap" id="dLog"></div></div></div>
    <div class="card"><h3>Connections</h3><dl class="kv" id="dConn"></dl>
      <h3 style="margin-top:18px">RSS feed</h3>
      <div class="row"><input readonly id="dFeed" class="mono" value=""><button class="btn sm" id="dCopy">Copy</button><a class="btn sm" href="/giga/feed" target="_blank">Open</a></div></div>
  </div>`;
  $("#dRun").onclick = () => $("#btnRun").click();
  $("#dStop").onclick = () => act(() => api("/api/stop", { method: "POST", body: {} }), "Stop requested");
  $("#dFeed").value = location.origin + "/giga/feed";
  $("#dCopy").onclick = () => { navigator.clipboard?.writeText($("#dFeed").value); toast("Feed URL copied", "ok"); };
  const mini = $("#dLog");
  logs.slice(-40).forEach(e => mini.insertAdjacentHTML("beforeend", logLine(e, "", true)));
  mini.scrollTop = mini.scrollHeight;
  page.log = e => { mini.insertAdjacentHTML("beforeend", logLine(e, "", true)); while (mini.children.length > 60) mini.firstChild.remove(); mini.scrollTop = mini.scrollHeight; };
  page.status = () => {
    if (!status) return;
    const p = status.pipeline, c = status.counts, lr = p.last_run;
    $("#dPhase").textContent = p.running ? p.phase : "Idle";
    $("#dCur").textContent = p.running && p.current ? "Processing " + p.current : (lr ? (lr.ok ? "Last run succeeded" : "Last run failed: " + (lr.error || "")) : "No run yet");
    $("#dBar").style.width = p.running && p.total ? (p.done / p.total * 100) + "%" : (p.running ? "8%" : "0");
    $("#dProg").textContent = p.running && p.total ? `${p.done} / ${p.total} releases` : "";
    $("#dReason").textContent = p.running ? "trigger: " + p.reason + (p.stop_pending ? " · stopping…" : "") : "";
    $("#dNext").textContent = p.next_run && !p.running ? "in " + fmtDur((new Date(p.next_run) - Date.now()) / 1000) : (p.running ? "running" : "—");
    $("#dLast").textContent = lr ? `last: ${ago(lr.started)} · ${lr.seconds.toFixed(1)}s` : "";
    $("#dStop").style.display = p.running ? "" : "none";
    $("#dRun").disabled = p.running;
    const t = (k, v, s, cls = "") => `<div class="card stat ${cls}"><div class="k">${k}</div><div class="v">${v}</div><div class="s">${s || "&nbsp;"}</div></div>`;
    $("#dStats").innerHTML =
      t("Releases", c.releases, "in feed") +
      t("Queued", c.by_state.queued || 0, `${c.queued_releases} fully sent to JD`, "rose") +
      t("Waiting", (c.by_state.waiting || 0) + (c.by_state.partial || 0), "links found, not yet queued") +
      t("No links yet", c.by_state.nolinks || 0, "not on forum yet") +
      t("Notified", c.notified, `${c.pending_notifications} pending`) +
      t("JD devices", status.jd.devices, status.jd.connected ? "connected" : "offline") +
      t("Forum pages", status.forum_max_page || "—", "last known thread length") +
      t("Uptime", fmtDur(status.uptime), (status.feed_age > 2592000 ? "no feed yet" : "feed age " + fmtDur(status.feed_age)));
    $("#dConn").innerHTML = `
      <dt>MyJDownloader</dt><dd>${status.jd.connected ? '<span class="tag ok">connected</span>' : `<span class="tag err">${status.jd.has_credentials ? "offline" : "no credentials"}</span>`}</dd>
      <dt>Pushover targets</dt><dd>${status.pushover}</dd>
      <dt>Schedule</dt><dd>every ${fmtDur(status.interval)}</dd>
      <dt>Signed in as</dt><dd>${esc(status.user || "")}</dd>
      <dt>RSS feed</dt><dd><span class="tag ok">public, no login</span></dd>`;
  };
  page.status();
}

/* ───────────── live log page ───────────── */
function logPage() {
  const f = store.get("logf", { DEBUG: false, INFO: true, WARNING: true, ERROR: true, RAW: true });
  view.innerHTML = `
  <div class="page-h"><div><h1>Live Log</h1><hr class="rule"><p>Streaming straight from the server.</p></div></div>
  <div class="logwrap">
    <div class="logbar">
      <div class="chips">${["ERROR", "WARNING", "INFO", "DEBUG"].map(l => `<span class="chip ${f[l] ? "on" : ""}" data-l="${l}">${l}</span>`).join("")}</div>
      <input type="search" id="lq" placeholder="Filter…  (text or release ID)">
      <button class="btn sm" id="lPause">Pause</button>
      <label class="row mute" style="gap:5px"><input type="checkbox" id="lAuto" checked> follow</label>
      <label class="row mute" style="gap:5px"><input type="checkbox" id="lWrap"> wrap</label>
      <button class="btn sm" id="lClear">Clear view</button>
      <a class="btn sm" href="/api/logs/download">Download</a>
      <span class="dim mono" id="lCount"></span>
    </div>
    <div class="log" id="lBody"></div>
  </div>`;
  const body = $("#lBody"); let q = "";
  $("#lWrap").checked = store.get("wrap", false); body.classList.toggle("wrap", $("#lWrap").checked);
  const pass = e => { const lv = e.level === "RAW" ? "RAW" : e.level; if (f[lv] === false) return false; return !q || (e.msg + e.level).toLowerCase().includes(q); };
  const count = () => $("#lCount").textContent = body.children.length + " lines";
  const follow = () => { if ($("#lAuto").checked) body.scrollTop = body.scrollHeight; };
  const full = () => { body.innerHTML = logs.filter(pass).map(e => logLine(e, q)).join(""); count(); follow(); };
  full();
  page.log = e => { if (pass(e)) { body.insertAdjacentHTML("beforeend", logLine(e, q)); while (body.children.length > LOGMAX) body.firstChild.remove(); count(); follow(); } };
  page.logPaused = n => $("#lPause").textContent = `Resume (${n})`;
  $$(".chip").forEach(c => c.onclick = () => { f[c.dataset.l] = !f[c.dataset.l]; c.classList.toggle("on", f[c.dataset.l]); store.set("logf", f); full(); });
  $("#lq").oninput = e => { q = e.target.value.trim().toLowerCase(); full(); };
  $("#lWrap").onchange = e => { body.classList.toggle("wrap", e.target.checked); store.set("wrap", e.target.checked); };
  $("#lPause").onclick = () => {
    paused = !paused;
    if (!paused) { const qd = pausedQueue; pausedQueue = []; qd.forEach(pushLog); $("#lPause").textContent = "Pause"; } else $("#lPause").textContent = "Resume";
  };
  if (paused) $("#lPause").textContent = `Resume (${pausedQueue.length})`;
  $("#lClear").onclick = async () => { logs.length = 0; await act(() => api("/api/logs/clear", { method: "POST", body: {} })); full(); };
  body.addEventListener("wheel", () => { if (body.scrollTop + body.clientHeight < body.scrollHeight - 40) $("#lAuto").checked = false; });
}

/* ───────────── releases ───────────── */
const STATE = { queued: ["ok", "queued"], partial: ["warn", "partial"], waiting: ["gold", "waiting"], nolinks: ["", "no links"] };
function releases() {
  view.innerHTML = `
  <div class="page-h"><div><h1>Releases</h1><hr class="rule"><p>Everything in the feed, with its download and notification state.</p></div></div>
  <div class="tools"><input type="search" id="rq" placeholder="Search ID, title, actress…">
    <select id="rf" style="width:auto"><option value="">All states</option><option value="queued">Queued</option><option value="partial">Partial</option><option value="waiting">Waiting</option><option value="nolinks">No links</option></select>
    <span class="dim" id="rc"></span></div>
  <div class="rgrid" id="rg"></div>`;
  let data = [];
  const draw = () => {
    const q = $("#rq").value.toLowerCase(), st = $("#rf").value;
    const list = data.filter(r => (!st || r.state === st) && (!q || (r.id + r.detail.title + r.detail.actress).toLowerCase().includes(q)));
    $("#rc").textContent = list.length + " of " + data.length;
    $("#rg").innerHTML = list.length ? list.map(card).join("") : `<div class="empty" style="grid-column:1/-1">Nothing here yet.</div>`;
  };
  const card = r => {
    const [cls, lbl] = STATE[r.state] || ["", r.state];
    const img = r.detail.cover || r.thumbnail;
    return `<article class="rc" data-id="${esc(r.id)}">
      <div class="cover">${img ? `<img loading="lazy" referrerpolicy="no-referrer" src="${esc(img)}" alt="">` : ""}
        <span class="id">${esc(r.id)}</span><span class="st tag ${cls}">${lbl}</span></div>
      <div class="rb"><div class="tt">${esc(r.detail.title)}</div>
        <div class="meta"><span>${esc(r.detail.actress)}</span><span>${esc((r.pub_date || "").slice(0, 10))}</span></div>
        <div class="bar"><i style="width:${r.links_total ? r.links_queued / r.links_total * 100 : 0}%"></i></div>
        <div class="meta"><span>${r.links_queued}/${r.links_total} links in JD</span><span>${r.notified_at ? "🔔 notified" : r.pending_notify ? "🔔 pending" : ""}</span></div>
        <div class="act"><button class="btn sm" data-a="open">Details</button>
          <button class="btn sm gold" data-a="queue" ${r.links_queued >= r.links_total ? "disabled" : ""}>Send to JD</button></div></div></article>`;
  };
  const load = async () => { data = await api("/api/releases"); draw(); };
  $("#rq").oninput = draw; $("#rf").onchange = draw;
  $("#rg").onclick = async e => {
    const b = e.target.closest("[data-a]"); if (!b) return;
    const id = b.closest(".rc").dataset.id, r = data.find(x => x.id === id);
    if (b.dataset.a === "open") return detail(r, load);
    if (b.dataset.a === "queue") { const x = await act(() => api("/api/release/action", { body: { id, action: "queue" } })); if (x) toast(`Queued ${x.queued} link(s)`, "ok"); load(); }
  };
  load(); page.status = () => { if (status && !status.pipeline.running && page._lastRun !== (status.pipeline.last_run || {}).id) { page._lastRun = (status.pipeline.last_run || {}).id; load(); } };
}
function detail(r, reload) {
  const d = r.detail, m = document.createElement("div"); m.className = "modal";
  m.innerHTML = `<div class="box">${(d.cover || r.thumbnail) ? `<div class="mhero" style="background-image:url('${esc(d.cover || r.thumbnail)}')"></div>` : ""}<div class="row sp"><h2>${esc(r.id)} <span class="tag ${(STATE[r.state] || [""])[0]}">${(STATE[r.state] || ["", r.state])[1]}</span></h2><button class="btn sm" data-x>Close</button></div>
    <p style="margin:8px 0 14px">${esc(d.title)}</p>
    <dl class="kv" style="margin-bottom:14px"><dt>Actress</dt><dd>${esc(d.actress)}</dd><dt>Director</dt><dd>${esc(d.director)}</dd><dt>Duration</dt><dd>${esc(d.duration)}</dd><dt>Release date</dt><dd>${esc(d.release_date)}</dd>
      <dt>Queued in JD</dt><dd>${r.queued_at ? when(r.queued_at) : "—"}</dd><dt>Notified</dt><dd>${r.notified_at ? when(r.notified_at) : "—"}</dd></dl>
    ${d.story ? `<div class="story">${esc(d.story)}</div>` : ""}
    ${d.screenshots?.length ? `<div class="shots">${d.screenshots.slice(0, 12).map(s => `<a href="${esc(s)}" target="_blank" rel="noreferrer"><img loading="lazy" referrerpolicy="no-referrer" src="${esc(s)}"></a>`).join("")}</div>` : ""}
    <h3 style="margin:12px 0 6px">Download links (${r.links_total})</h3>
    <div class="linkl">${r.links.map(l => `<div class="n" title="${esc(l)}">${esc(l.split("/").pop().split("?")[0])}</div>`).join("") || '<span class="dim">none yet</span>'}</div>
    <div class="row" style="margin-top:16px"><a class="btn sm" href="${esc(r.url)}" target="_blank" rel="noreferrer">Akiba-Web ↗</a>
      <button class="btn sm gold" data-a="queue">Send to JD</button><button class="btn sm" data-a="renotify">Re-send notification</button>
      <button class="btn sm danger" data-a="forget">Forget state</button></div></div>`;
  document.body.append(m);
  m.onclick = async e => {
    if (e.target === m || e.target.dataset.x !== undefined) return m.remove();
    const a = e.target.dataset.a; if (!a) return;
    if (a === "forget" && !confirm("Clear queue + notification state for " + r.id + "? It will be re-queued on the next run.")) return;
    const x = await act(() => api("/api/release/action", { body: { id: r.id, action: a } }), "Done");
    if (x) { m.remove(); reload && reload(); }
  };
}

/* ───────────── activity ───────────── */
function runs() {
  view.innerHTML = `<div class="page-h"><div><h1>Activity</h1><hr class="rule"><p>History of pipeline runs.</p></div></div><div class="card" id="rr">Loading…</div>`;
  const load = async () => {
    const h = (await api("/api/history")) || [];
    $("#rr").innerHTML = h.length ? `<table><thead><tr><th>#</th><th>Started</th><th>Trigger</th><th>Result</th><th>Releases</th><th>Matched</th><th>Queued</th><th>Notified</th><th>Forum</th><th>Time</th></tr></thead><tbody>${h.map(r => `
      <tr><td>${r.id}</td><td>${when(r.started)}</td><td>${esc(r.reason)}</td><td>${r.ok ? '<span class="tag ok">ok</span>' : `<span class="tag err" title="${esc(r.error)}">failed</span>`}</td>
      <td>${r.releases}</td><td>${r.matched}</td><td>${r.queued_links}</td><td>${r.notifications}</td>
      <td>${r.forum_ok ? `<span class="tag ok">p${r.forum_max_page || "?"}</span>` : '<span class="tag warn">unavailable</span>'}</td><td>${r.seconds.toFixed(1)}s</td></tr>`).join("")}</tbody></table>` : `<div class="empty">No runs recorded yet.</div>`;
  };
  load(); page.status = () => { const id = status?.pipeline?.last_run?.id; if (id !== page._id) { page._id = id; load(); } };
}

/* ───────────── system (JD, state) ───────────── */
function system() {
  view.innerHTML = `<div class="page-h"><div><h1>System</h1><hr class="rule"><p>MyJDownloader devices and persistent state.</p></div></div>
  <div class="grid g2"><div class="card"><h3>MyJDownloader <button class="btn sm" id="jRe">Reconnect</button></h3><div id="jBody">…</div></div>
  <div class="card"><h3>Pushover</h3><div id="pBody"></div></div></div>
  <div class="sec"><h2>Sent links</h2></div>
  <div class="card"><div class="tools"><input type="search" id="sq" placeholder="Filter links…"><span class="dim" id="sc"></span></div><div class="linkl" style="max-height:420px" id="sl"></div></div>`;
  const jd = async () => {
    const j = await api("/api/jd");
    $("#jBody").innerHTML = (j.connected ? '<span class="tag ok">connected</span>' : `<span class="tag err">offline</span> <span class="mute">${esc(j.error)}</span>`) +
      ((j.devices || []).length ? `<table style="margin-top:12px"><thead><tr><th>Order</th><th>Device</th><th>Type</th></tr></thead><tbody>${j.devices.map((d, i) => `<tr><td>${i + 1}</td><td>${esc(d.name)}</td><td class="mute">${esc(d.type)}</td></tr>`).join("")}</tbody></table>` : "");
  };
  $("#jRe").onclick = async () => { $("#jRe").disabled = true; const r = await act(() => api("/api/jd/reconnect", { method: "POST", body: {} })); $("#jRe").disabled = false; if (r) toast(r.ok ? "Connected" : "Failed: " + r.error, r.ok ? "ok" : "err"); jd(); };
  jd();
  api("/api/config").then(c => {
    const l = c.pushover_destinations || [];
    $("#pBody").innerHTML = l.length ? l.map((d, i) => `<div class="row sp" style="padding:8px 0;border-bottom:1px solid var(--line)"><span>Destination ${i + 1} ${d.include_sensitive_data ? '<span class="tag rose">rich</span>' : '<span class="tag">plain</span>'}</span><button class="btn sm" data-i="${i}">Send test</button></div>`).join("") : '<span class="mute">No destinations configured.</span>';
    $("#pBody").onclick = e => { const b = e.target.closest("[data-i]"); if (b) act(() => api("/api/pushover/test", { body: { index: +b.dataset.i } }), "Test sent"); };
  });
  api("/api/state").then(s => {
    const ll = s.found_links || [];
    const draw = () => { const q = $("#sq").value.toLowerCase(); const f = ll.filter(l => l.toLowerCase().includes(q)); $("#sc").textContent = `${f.length} of ${ll.length}`; $("#sl").innerHTML = f.slice(0, 1500).map(l => `<div class="q" title="${esc(l)}">${esc(l)}</div>`).join(""); };
    $("#sq").oninput = draw; draw();
  });
}

/* ───────────── settings ───────────── */
const MASK = "••••••••";
function settings() {
  view.innerHTML = `<div class="page-h"><div><h1>Settings</h1><hr class="rule"><p>Stored in <span class="mono">settings.json</span> in the data folder. Secrets stay masked; leave them untouched to keep the current value.</p></div>
    <button class="btn primary" id="cSave">Save changes</button></div><div id="banner"></div><div id="cf">Loading…</div>`;
  const banner = () => {
    if (!status) return;
    const msgs = [];
    if (status.imported) msgs.push("Your existing <span class='mono'>config.json</span> was imported and renamed to <span class='mono'>config.json.imported</span>. Settings now live here.");
    if (status.needs_myjd) msgs.push("MyJDownloader isn't set up yet — add your login below and save to start queueing links.");
    const html = msgs.map(m => `<div class="banner">${m}</div>`).join("");
    if ($("#banner") && $("#banner").innerHTML !== html) $("#banner").innerHTML = html;
  };
  banner(); page.status = banner;
  api("/api/config").then(c => {
    const F = (k, label, o = {}) => `<label class="f"><span>${label}</span><input data-k="${k}" type="${o.type || "text"}" value="${esc(c[k] ?? "")}" ${o.ph ? `placeholder="${o.ph}"` : ""}>${o.h ? `<small>${o.h}</small>` : ""}</label>`;
    $("#cf").innerHTML = `<div class="grid g2">
      <div class="card"><h3>Sources</h3>${F("akiba_base", "Akiba-Web base URL")}${F("akiba_releases_path", "Releases listing path")}${F("forum_base", "Forum base URL")}${F("forum_thread", "Forum thread")}
        <div class="two">${F("forum_pages", "Pages to scan", { type: "number", h: "newest pages of the thread" })}${F("forum_retries", "Retries per request", { type: "number", h: "backoff on connection resets" })}</div></div>
      <div class="card"><h3>Schedule &amp; server</h3><div class="two">${F("schedule_interval", "Interval (seconds)", { type: "number" })}${F("port", "Port", { type: "number", h: "restart required" })}</div>
        ${F("listen_host", "Listen address", { h: "0.0.0.0 = all interfaces · restart required" })}${F("public_base_url", "Public URL (RSS channel link)", { ph: "http://host:5000/" })}</div>
      <div class="card"><h3>MyJDownloader</h3>${F("myjd_email", "Email")}${F("myjd_password", "Password", { type: "password" })}</div>
      <div class="card"><h3>Account</h3><p class="mute" style="margin-top:0">Signed in as <b>${esc(status?.user || "")}</b>. The RSS feed and <span class="mono">/health</span> never require a login.</p>
        <label class="f"><span>Current password</span><input id="pwCur" type="password" autocomplete="current-password"></label>
        <label class="f"><span>New password</span><input id="pwNew" type="password" autocomplete="new-password" minlength="8"></label>
        <button class="btn" id="pwGo">Change password</button><small class="dim" style="display:block;margin-top:8px">Other devices are signed out when you change it.</small></div>
    </div>
    <div class="sec"><h2>Pushover</h2></div><div class="card"><div id="pd"></div><button class="btn sm" id="pAdd">+ Add destination</button></div>`;
    const dests = JSON.parse(JSON.stringify(c.pushover_destinations || []));
    const pd = () => { $("#pd").innerHTML = dests.map((d, i) => `<div class="two" style="grid-template-columns:1fr 1fr auto auto;align-items:end;margin-bottom:10px" data-i="${i}">
      <label class="f" style="margin:0"><span>App API token</span><input data-f="api_token" type="password" value="${esc(d.api_token)}"></label>
      <label class="f" style="margin:0"><span>User / group key</span><input data-f="user_key" type="password" value="${esc(d.user_key)}"></label>
      <label class="row mute" style="padding-bottom:9px"><input type="checkbox" data-f="include_sensitive_data" ${d.include_sensitive_data ? "checked" : ""}> rich content</label>
      <button class="btn sm danger" data-rm="${i}">Remove</button></div>`).join("") || '<p class="mute">None — notifications disabled.</p>'; };
    pd();
    $("#pd").oninput = e => { const r = e.target.closest("[data-i]"), f = e.target.dataset.f; if (r && f) dests[+r.dataset.i][f] = e.target.type === "checkbox" ? e.target.checked : e.target.value; };
    $("#pd").onclick = e => { const b = e.target.closest("[data-rm]"); if (b) { dests.splice(+b.dataset.rm, 1); pd(); } };
    $("#pAdd").onclick = () => { dests.push({ api_token: "", user_key: "", include_sensitive_data: false }); pd(); };
    $("#pwGo").onclick = async () => {
      const r = await act(() => api("/api/auth/password", { body: { current: $("#pwCur").value, new: $("#pwNew").value } }), "Password changed");
      if (r) { $("#pwCur").value = ""; $("#pwNew").value = ""; }
    };
    $("#cSave").onclick = async () => {
      const patch = {};
      $$("#cf [data-k]").forEach(i => { patch[i.dataset.k] = i.type === "number" ? (i.value === "" ? 0 : +i.value) : i.value; });
      patch.pushover_destinations = dests;
      const r = await act(() => api("/api/config", { body: patch }), "Settings saved");
      if (r?.restart_required) toast("Port / listen address changes need a restart of the service", "");
    };
  });
}

route(); pollStatus();
