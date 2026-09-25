const $ = s => document.querySelector(s);
const $$ = s => Array.from(document.querySelectorAll(s));
const esc = s => (s == null ? '' : String(s)).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const fmtDT = ts => { if (!ts) return '-'; const d = new Date(ts); return isNaN(d) ? ts : d.toLocaleString(); };
const fmtAgo = sec => { if (sec == null) return '-'; if (sec > 60*60*24*30) return 'never'; sec = Math.max(0, Math.round(sec));
  if (sec < 60) return sec + 's ago'; if (sec < 3600) return Math.round(sec/60) + 'm ago';
  if (sec < 86400) return Math.round(sec/3600) + 'h ago'; return Math.round(sec/86400) + 'd ago'; };

async function api(path, opts) {
  const r = await fetch(path, Object.assign({ headers: { 'Content-Type': 'application/json' } }, opts));
  if (r.status === 401) { location.href = '/login?next=' + encodeURIComponent(location.pathname + location.hash); throw new Error('signed out'); }
  const ct = r.headers.get('content-type') || '';
  const body = ct.includes('json') ? await r.json().catch(() => null) : await r.text();
  if (!r.ok) throw new Error((body && body.error) || r.statusText);
  return body;
}

function toast(msg, kind) {
  const t = document.createElement('div'); t.className = 'toast ' + (kind || '');
  t.textContent = msg; $('#toasts').appendChild(t);
  requestAnimationFrame(() => t.classList.add('show'));
  setTimeout(() => { t.classList.remove('show'); setTimeout(() => t.remove(), 250); }, 4000);
}

// ---------- router ----------
const views = ['dashboard', 'log', 'releases', 'runs', 'system', 'settings'];
function setView(v) {
  if (!views.includes(v)) v = 'dashboard';
  views.forEach(x => $('#v-' + x).classList.toggle('on', x === v));
  $$('#tabs a').forEach(a => a.classList.toggle('on', a.dataset.v === v));
  if (v === 'releases') loadReleases();
  if (v === 'runs') loadRuns();
  if (v === 'system') loadSystem();
  if (v === 'settings') loadSettings();
  if (v === 'log' && !logLoaded) { logLoaded = true; loadLogHistory(); }
}
function routeFromHash() { setView((location.hash.replace(/^#\/?/, '') || 'dashboard').split('/')[0]); }
window.addEventListener('hashchange', routeFromHash);

// ---------- status polling ----------
let lastStatus = null;
function pillState(s) {
  if (s.pipeline && s.pipeline.running) return 'running';
  if (s.pipeline && s.pipeline.last_run && s.pipeline.last_run.ok === false) return 'error';
  if (s.pipeline && s.pipeline.last_run) return 'ok';
  return 'idle';
}
function pill(el, state) {
  el.classList.remove('idle', 'run', 'ok', 'err');
  el.classList.add(state === 'running' ? 'run' : state === 'error' ? 'err' : state === 'ok' ? 'ok' : 'idle');
  $('b', el).textContent = state === 'running' ? 'RUNNING' : state === 'error' ? 'ERROR' : state === 'ok' ? 'OK' : 'IDLE';
}
function renderTiles(s) {
  const c = s.counts || {};
  const byState = c.by_state || {};
  const t = [
    ['Releases tracked', c.releases ?? 0, ''],
    ['Fully queued', byState.queued ?? 0, 'ok'],
    ['Awaiting links', (byState.waiting ?? 0) + (byState.nolinks ?? 0), (byState.waiting || byState.nolinks) ? 'warn' : ''],
    ['Last run', s.pipeline && s.pipeline.last_run ? (s.pipeline.last_run.ok === false ? 'failed' : 'ok') + ' &middot; ' + esc(s.pipeline.last_run.started || '') : 'never', s.pipeline && s.pipeline.last_run && s.pipeline.last_run.ok === false ? 'err' : ''],
  ];
  $('#tiles').innerHTML = t.map(([k, v, cls]) => `<div class="stat ${cls}"><div class="k">${esc(k)}</div><div class="v">${v}</div></div>`).join('');
}
async function refresh() {
  let s;
  try { s = await api('/api/status'); } catch { return; }
  lastStatus = s;
  const st = pillState(s);
  pill($('#hdrPill'), st);
  renderTiles(s);
  const p = s.pipeline || {};
  $('#pipeMeta').textContent = p.running ? ('run: ' + (p.reason || '')) : (p.next_run ? ('next run ' + fmtDT(p.next_run)) : '');
  const busy = !!p.running;
  $('#bar').classList.toggle('busy', busy);
  $('#bar').style.width = busy && p.total ? Math.round(100 * (p.done || 0) / p.total) + '%' : (busy ? '100%' : '0%');
  $('#barTxt').textContent = busy ? (p.phase || 'Running...') : 'Idle';
  $('#curItem').textContent = p.current || ' ';
  $('#btnStop').disabled = !busy;
  $('#btnRun').disabled = busy;
  $('#feedUrl').value = location.origin + '/giga/feed';
  $('#hdrUser').textContent = s.user ? ('@' + s.user) : '';
  banner(s);
  $('#connKv').innerHTML = [
    ['MyJDownloader', s.needs_myjd ? 'not configured' : ((s.jd && s.jd.connected) ? 'connected' : 'disconnected')],
    ['Pushover', (s.pushover || 0) + ' destination(s)'],
    ['Forum pages scanned', s.forum_max_page ?? '-'],
    ['Feed cached', s.feed_age > 60*60*24*30 ? 'no feed yet' : fmtAgo(s.feed_age)],
  ].map(([k, v]) => `<div class="krow"><span class="k">${esc(k)}</span><span class="v">${esc(v)}</span></div>`).join('');
}
function banner(s) {
  const msgs = [];
  if (s.imported) msgs.push('Imported settings from a legacy config.json &mdash; it was renamed with an <code>.imported</code> suffix.');
  if (s.needs_myjd) msgs.push('MyJDownloader is not configured yet &mdash; add credentials in <a href="#/settings">Settings</a> to enable queueing.');
  $('#banner').innerHTML = msgs.length ? msgs.map(m => `<div class="block banner">${m}</div>`).join('') : '';
}
setInterval(refresh, 2000);

// ---------- clock ----------
setInterval(() => { $('#hdrClock').textContent = new Date().toLocaleTimeString(); }, 1000);

// ---------- live log (SSE) ----------
let logLoaded = false, logPaused = false, logBuf = [], flushTimer = null;
const LVL_CLASS = { INFO: 'INFO', WARNING: 'WARNING', WARN: 'WARNING', ERROR: 'ERROR', DEBUG: 'DEBUG' };

function logLine(e, short) {
  const cls = LVL_CLASS[e.level] || 'INFO';
  const t = e.time ? new Date(e.time) : new Date();
  const ts = short ? t.toLocaleTimeString() : t.toLocaleString();
  return `<div class="ll ${cls}"><span class="t">${esc(ts)}</span><span class="lv">${esc(e.level || 'INFO')}</span><span class="m">${esc(e.msg || '')}</span></div>`;
}
function currentFilter() {
  const active = $('#lvlSeg .on');
  return { level: active ? active.dataset.l : 'ALL', q: ($('#logSearch').value || '').toLowerCase() };
}
function matches(e, f) {
  if (f.level !== 'ALL' && (e.level || 'INFO') !== f.level) return false;
  if (f.q && !(e.msg || '').toLowerCase().includes(f.q)) return false;
  return true;
}
async function loadLogHistory() {
  try {
    const rows = (await api('/api/logs')) || [];
    logBuf = rows;
    renderLog();
  } catch {}
}
function renderLog() {
  const f = currentFilter();
  const rows = logBuf.filter(e => matches(e, f));
  $('#logBox').innerHTML = rows.map(e => logLine(e, false)).join('');
  $('#logCount').textContent = rows.length + ' / ' + logBuf.length + ' lines';
  if ($('#logFollow').checked) $('#logBox').scrollTop = $('#logBox').scrollHeight;
  const mini = logBuf.slice(-40);
  $('#miniLog').innerHTML = mini.map(e => logLine(e, true)).join('');
  $('#miniLog').scrollTop = $('#miniLog').scrollHeight;
}
function scheduleFlush() { if (flushTimer) return; flushTimer = setTimeout(() => { flushTimer = null; renderLog(); }, 80); }

function connectSSE() {
  let es;
  try { es = new EventSource('/api/logs/stream'); } catch { return; }
  es.onmessage = ev => {
    $('#liveDot').classList.add('on');
    if (logPaused) return;
    try {
      const e = JSON.parse(ev.data);
      logBuf.push(e);
      if (logBuf.length > 5000) logBuf.splice(0, logBuf.length - 5000);
      scheduleFlush();
    } catch {}
  };
  es.onerror = async () => {
    $('#liveDot').classList.remove('on');
    try { const st = await api('/api/auth/state'); if (!st.authenticated) { location.href = '/login'; } } catch {}
  };
}
$('#lvlSeg').addEventListener('click', e => {
  const b = e.target.closest('button'); if (!b) return;
  $$('#lvlSeg button').forEach(x => x.classList.remove('on')); b.classList.add('on'); renderLog();
});
$('#logSearch').addEventListener('input', renderLog);
$('#logPause').addEventListener('click', () => { logPaused = !logPaused; $('#logPause').textContent = logPaused ? 'Resume' : 'Pause'; });
$('#logWrap').addEventListener('change', () => $('#logBox').classList.toggle('nowrap', !$('#logWrap').checked));
$('#logClear').addEventListener('click', () => { logBuf = []; renderLog(); });

// ---------- releases ----------
let releasesCache = [];
async function loadReleases() {
  try { releasesCache = (await api('/api/releases')) || []; } catch { releasesCache = []; }
  renderReleases();
}
function relTitle(r) { return (r.detail && r.detail.title) || r.rss_title || r.id; }
function relCover(r) { return (r.detail && r.detail.cover) || r.thumbnail || ''; }
function relActress(r) { return (r.detail && r.detail.actress) || ''; }
function renderReleases() {
  const q = ($('#relSearch').value || '').toLowerCase();
  const st = $('#relState').value;
  const rows = releasesCache.filter(r => {
    if (st && r.state !== st) return false;
    if (!q) return true;
    return [(r.id||''), relTitle(r), relActress(r)].join(' ').toLowerCase().includes(q);
  });
  $('#relCount').textContent = rows.length + ' / ' + releasesCache.length;
  $('#relGrid').innerHTML = rows.map(r => {
    const pct = r.links_total ? Math.round(100 * (r.links_queued||0) / r.links_total) : 0;
    return `<div class="tile" data-id="${esc(r.id)}">
      <div class="cv" style="background-image:url('${esc(relCover(r))}')"></div>
      <div class="tb">
        <div class="tt"><span class="relid">${esc(r.id)}</span>${esc(relTitle(r))}</div>
        <div class="tm"><span class="tag st-${esc(r.state)}">${esc(r.state)}</span><span>${r.links_queued||0}/${r.links_total||0} links</span></div>
        <div class="bar"><div style="width:${pct}%"></div></div>
      </div>
    </div>`;
  }).join('') || '<p class="muted">No releases match.</p>';
}
$('#relSearch').addEventListener('input', renderReleases);
$('#relState').addEventListener('change', renderReleases);
$('#relGrid').addEventListener('click', e => {
  const t = e.target.closest('.tile'); if (!t) return;
  const r = releasesCache.find(x => String(x.id) === t.dataset.id);
  if (r) openReleaseModal(r);
});
// The site's own description text ends in a trailing "show more/close"
// toggle label (e.g. "▲Close" or "▲閉じる") that only makes sense inside
// their own expand/collapse widget - strip it so it doesn't leak into ours.
function cleanStory(s) {
  return (s || '').replace(/[\s　]*[▲▼]\s*(close|閉じる|とじる)\s*$/i, '').trim();
}
function openReleaseModal(r) {
  const d = r.detail || {};
  const cover = relCover(r);
  const shots = d.screenshots || [];
  const story = cleanStory(d.story);
  const info = [
    ['State', `<span class="tag st-${esc(r.state)}">${esc(r.state)}</span>`],
    ['Links queued', `${r.links_queued||0} / ${r.links_total||0}`],
    ['Actress', esc(d.actress || '-')],
    ['Director', esc(d.director || '-')],
    ['Duration', esc(d.duration || '-')],
    ['Release date', esc(d.release_date || '-')],
    ['Found', esc(fmtDT(r.pub_date))],
    ['Notified', r.notified_at ? esc(fmtDT(r.notified_at)) : 'not yet'],
  ];
  $('#mTitle').innerHTML = `<span class="relid">${esc(r.id)}</span>${esc(relTitle(r))}`;
  $('#modalBox').classList.add('wide');
  $('#mBody').innerHTML = `
    <div class="rd-top">
      ${cover ? `<img class="rd-cover" src="${esc(cover)}">` : '<div class="rd-cover"></div>'}
      <div class="rd-info">${info.map(([k, v]) => `<span class="k">${esc(k)}</span><span class="v">${v}</span>`).join('')}</div>
    </div>
    ${story ? `<h4>Story</h4><p class="rd-story">${esc(story)}</p>` : ''}
    ${shots.length ? `<h4>Screenshots</h4><div class="shots rd-shots">${shots.map(u => `<a href="${esc(u)}" target="_blank" rel="noopener noreferrer"><img loading="lazy" src="${esc(u)}"></a>`).join('')}</div>` : ''}
    <div class="btnrow" style="margin-top:14px">
      <a class="btn sm" href="${esc(r.url)}" target="_blank" rel="noopener noreferrer">Open thread</a>
      <button class="btn primary sm" data-act="queue">Send to JD</button>
      <button class="btn sm" data-act="renotify">Re-notify</button>
      <button class="btn danger sm" data-act="forget">Forget</button>
    </div>`;
  $('#mBody').querySelectorAll('[data-act]').forEach(b => b.onclick = async () => {
    try {
      await api('/api/release/action', { method: 'POST', body: JSON.stringify({ id: r.id, action: b.dataset.act }) });
      toast('Done', 'ok'); closeModal(); loadReleases();
    } catch (e) { toast(e.message, 'err'); }
  });
  $('#modal').hidden = false;
}
function closeModal() { $('#modal').hidden = true; $('#modalBox').classList.remove('wide'); }
$('#mClose').addEventListener('click', closeModal);
$('#modal').addEventListener('click', e => { if (e.target.id === 'modal') closeModal(); });
document.addEventListener('keydown', e => { if (e.key === 'Escape' && !$('#modal').hidden) closeModal(); });

// ---------- run history ----------
async function loadRuns() {
  let rows = [];
  try { rows = (await api('/api/history')) || []; } catch {}
  $('#runsTbl').innerHTML = '<tr><th>Started</th><th>Reason</th><th>Result</th><th>Releases</th><th>Matched</th><th>Queued links</th><th>Notified</th><th>Duration</th></tr>' +
    rows.slice().reverse().map(r => `<tr>
      <td>${fmtDT(r.started)}</td><td>${esc(r.reason||'-')}</td>
      <td><span class="tag st-${r.ok === false ? 'err' : 'ok'}" title="${esc(r.error||'')}">${r.ok === false ? 'error' : 'ok'}</span></td>
      <td>${r.releases ?? '-'}</td><td>${r.matched ?? '-'}</td><td>${r.queued_links ?? '-'}</td><td>${r.notifications ?? '-'}</td>
      <td>${r.seconds != null ? r.seconds.toFixed(1)+'s' : '-'}</td>
    </tr>`).join('');
}

// ---------- system ----------
async function loadSystem() {
  try {
    const s = await api('/api/status');
    const jd = await api('/api/jd').catch(() => null);
    $('#jdBody').innerHTML = s.needs_myjd
      ? '<p class="muted">Not configured. Add credentials in Settings.</p>'
      : `<div class="kv"><div class="krow"><span class="k">Status</span><span class="v">${jd && jd.connected ? 'connected' : 'disconnected'}</span></div>
         <div class="krow"><span class="k">Devices</span><span class="v">${(jd && jd.devices || []).length}</span></div>
         ${jd && jd.error ? `<div class="krow"><span class="k">Error</span><span class="v err">${esc(jd.error)}</span></div>` : ''}</div>
         ${(jd && jd.devices || []).map(d => `<div class="krow"><span class="k">${esc(d.name||d.id)}</span><span class="v">${esc(d.type||'')}</span></div>`).join('')}`;
    $('#pushBody').innerHTML = `<div class="kv"><div class="krow"><span class="k">Destinations</span><span class="v">${s.pushover||0}</span></div></div>`;
  } catch {}
  try {
    const sent = (await api('/api/state')) || {};
    renderSentLinks(sent.found_links || []);
  } catch { renderSentLinks([]); }
}
let sentLinksCache = [];
function renderSentLinks(list) {
  sentLinksCache = list;
  const q = ($('#sq') && $('#sq').value || '').toLowerCase();
  const rows = list.filter(x => !q || String(x).toLowerCase().includes(q));
  $('#sc').textContent = rows.length + ' / ' + list.length;
  $('#sl').innerHTML = rows.map(x => `<div class="ll INFO"><span class="m">${esc(x)}</span></div>`).join('');
}
document.addEventListener('input', e => { if (e.target.id === 'sq') renderSentLinks(sentLinksCache); });

// ---------- settings ----------
function pushRow(v) {
  const w = document.createElement('div'); w.className = 'pushdest';
  w.innerHTML = `<input class="in" placeholder="user key or user:device" value="${esc(v||'')}"><button class="btn sm danger" type="button">&times;</button>`;
  w.querySelector('button').onclick = () => w.remove();
  return w;
}
async function loadSettings() {
  let c = {};
  try { c = await api('/api/config'); } catch {}
  const f = $('#cfgForm');
  Object.keys(c).forEach(k => { const el = f.elements[k]; if (el && el.type !== 'password') el.value = c[k] ?? ''; });
  $('#pd').innerHTML = '';
  (c.pushover_destinations || []).forEach(d => $('#pd').appendChild(pushRow(d)));
  if (!(c.pushover_destinations || []).length) $('#pd').appendChild(pushRow(''));
  try {
    const st = await api('/api/auth/state');
    $('#acctUser').textContent = st.user ? ('signed in as @' + st.user) : '';
  } catch {}
}
$('#pAdd').addEventListener('click', () => $('#pd').appendChild(pushRow('')));
$('#cfgReset').addEventListener('click', loadSettings);
$('#cfgForm').addEventListener('submit', async e => {
  e.preventDefault();
  const f = e.target, patch = {};
  new FormData(f).forEach((v, k) => {
    if (k === 'myjd_password' && !v) return;
    if (f.elements[k].type === 'number') { patch[k] = v === '' ? null : Number(v); }
    else patch[k] = v;
  });
  patch.pushover_destinations = $$('#pd .pushdest input').map(i => i.value.trim()).filter(Boolean);
  try {
    await api('/api/config', { method: 'POST', body: JSON.stringify(patch) });
    $('#cfgMsg').textContent = 'Saved.'; $('#cfgMsg').className = 'small ok';
    toast('Settings saved', 'ok'); refresh();
  } catch (err) { $('#cfgMsg').textContent = err.message; $('#cfgMsg').className = 'small err'; }
});
$('#pwForm').addEventListener('submit', async e => {
  e.preventDefault();
  const f = e.target;
  const cur = f.elements.current.value, nw = f.elements.new.value, cf = f.elements.confirm.value;
  if (nw !== cf) { $('#pwMsg').textContent = 'New passwords do not match'; $('#pwMsg').className = 'small err'; return; }
  try {
    await api('/api/auth/password', { method: 'POST', body: JSON.stringify({ current: cur, new: nw }) });
    $('#pwMsg').textContent = 'Password changed.'; $('#pwMsg').className = 'small ok'; f.reset();
    toast('Password changed', 'ok');
  } catch (err) { $('#pwMsg').textContent = err.message; $('#pwMsg').className = 'small err'; }
});

// ---------- buttons ----------
$('#btnRun').addEventListener('click', async () => { try { await api('/api/run', { method: 'POST' }); refresh(); } catch (e) { toast(e.message, 'err'); } });
$('#btnStop').addEventListener('click', async () => { try { await api('/api/stop', { method: 'POST' }); refresh(); } catch (e) { toast(e.message, 'err'); } });
$('#jdReconnect').addEventListener('click', async () => { try { await api('/api/jd/reconnect', { method: 'POST' }); toast('Reconnecting...', 'ok'); loadSystem(); } catch (e) { toast(e.message, 'err'); } });
$('#copyFeed').addEventListener('click', () => { $('#feedUrl').select(); navigator.clipboard && navigator.clipboard.writeText($('#feedUrl').value); toast('Feed URL copied', 'ok'); });
$('#btnLogout').addEventListener('click', async () => { try { await api('/api/auth/logout', { method: 'POST' }); } catch {} location.href = '/login'; });

// ---------- boot ----------
routeFromHash();
refresh();
connectSSE();
