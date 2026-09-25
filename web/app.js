// Owner: Claude. The page reads one snapshot per tick from /v1/stream and paints everything from it; no state
// lives here beyond the chart rings, the timeline dedupe cursor and the pinned tenant.
'use strict';

const COLORS = ['#2ecc71', '#f1c40f', '#e67e22', '#9b59b6']; // ACTIVE, RIDING_THROUGH, KEY_UNAVAILABLE, REVOKED
const STATES = ['ACTIVE', 'RIDING_THROUGH', 'KEY_UNAVAILABLE', 'REVOKED'];
const PROVIDERS = ['aws', 'gcp', 'azure'];
const COLS = 40, CELL = 20, N = 1000;
const RING = 240; // ChartWindow 2 min at 2 Hz
const $ = id => document.getElementById(id);

let es = null, connected = false, world = null, lastSeq = 0, snap = null;
let hoverTimer = null, hoverIdx = -1, pinIdx = -1, pinTimer = null;
const charts = {};

// ---- connection -------------------------------------------------------------------------------------------------

function banner(text) { $('banner').textContent = text; }

function connect() {
  if (es) es.close();
  es = new EventSource('/v1/stream');
  es.onopen = () => { connected = true; banner(world ? 'live · ' + world : 'waiting for the first snapshot…'); };
  es.onmessage = e => onSnapshot(JSON.parse(e.data));
  es.addEventListener('reconnect', () => { es.close(); connect(); }); // the server ends a stream at StreamMaxAge, before Cloud Run's cut
  es.onerror = () => { connected = false; banner('reconnecting…'); renderCards(snap); };
}

function onSnapshot(s) {
  if (s.world !== world) {
    world = s.world;
    resetRings();
    $('timeline').innerHTML = '';
    lastSeq = 0;
    banner('fresh world ' + s.world);
    setTimeout(() => { if (connected) banner('live · ' + world); }, 4000);
  }
  connected = true;
  snap = s;
  paintGrid(s.grid);
  renderTiles(s);
  renderPanel(s.invariants);
  renderCards(s);
  renderMode(s.key_fetch);
  pushCharts(s);
  renderTimeline(s.events);
}

// ---- grid ---------------------------------------------------------------------------------------------------------

const ctx = $('grid').getContext('2d');

function paintGrid(str) {
  ctx.fillStyle = '#0b0d11';
  ctx.fillRect(0, 0, COLS * CELL, (N / COLS) * CELL);
  for (let i = 0; i < N; i++) {
    const code = str ? str.charCodeAt(i) - 48 : -1;
    const x = (i % COLS) * CELL, y = Math.floor(i / COLS) * CELL;
    ctx.fillStyle = code < 0 ? '#2a2f3a' : COLORS[code & 3];
    ctx.fillRect(x + 1, y + 1, CELL - 2, CELL - 2);
    if (code >= 0 && (code & 4)) { // scenario target
      ctx.strokeStyle = 'rgba(255,255,255,.85)';
      ctx.lineWidth = 2;
      ctx.strokeRect(x + 4, y + 4, CELL - 8, CELL - 8);
    }
  }
}

function drawBands() {
  const el = $('bands');
  el.innerHTML = '';
  PROVIDERS.forEach((p, k) => {
    const start = Math.ceil(k * N / PROVIDERS.length);
    const d = document.createElement('div');
    d.textContent = p;
    d.style.top = (Math.ceil(start / COLS) * CELL + 2) + 'px'; // the band's first full row
    el.appendChild(d);
  });
}

function tenantId(i) { return 't-' + String(i).padStart(4, '0'); }
function providerOf(i) { return PROVIDERS[Math.floor(i * PROVIDERS.length / N)]; }
function cellAt(ev) {
  const r = $('grid').getBoundingClientRect();
  const x = Math.floor((ev.clientX - r.left) / CELL), y = Math.floor((ev.clientY - r.top) / CELL);
  if (x < 0 || x >= COLS || y < 0) return -1;
  const i = y * COLS + x;
  return i < N ? i : -1;
}

// ---- tiles and panel ------------------------------------------------------------------------------------------------

const fmt = (v, d = 0) => v == null ? '—' : Number(v).toFixed(d);

function renderTiles(s) {
  $('t-delivered').textContent = fmt(s.tiles.delivered_ps) + ' / ' + fmt(s.tiles.capacity_ps);
  $('l4-split').textContent = 'rejected: within share ' + fmt(s.l4.rejected_within_share_ps) + '/s · over share ' + fmt(s.l4.rejected_over_share_ps) + '/s';
  $('t-offered').textContent = 'offered ' + fmt(s.tiles.offered_ps) + '/s · accepted ' + fmt(s.ingest_ps.accepted) + '/s';
  $('t-p99').textContent = s.p99_ms.healthy ? fmt(s.p99_ms.healthy, 1) : '—';
  $('t-baseline').textContent = 'baseline ' + (s.p99_ms.baseline ? fmt(s.p99_ms.baseline, 1) + ' ms' : '—') + (s.p99_ms.affected ? ' · affected ' + fmt(s.p99_ms.affected, 1) + ' ms' : '');
  $('t-kms').textContent = fmt(s.tiles.kms_calls_ps, 1);
  const inf = s.inflight || {};
  $('inflight').textContent = 'in flight: ' + PROVIDERS.map(p => p + ' ' + (inf[p] == null ? '—' : inf[p])).join(' · ');
  const b = s.tiles.by_state;
  $('t-states').innerHTML = b.map((n, k) => '<span style="color:' + COLORS[k] + '">' + n + '</span>').join(' <span style="color:#4a5160">·</span> ');
  $('t-backlog').textContent = 'backlog ' + s.backlog.total + (s.backlog.affected ? ' · affected ' + s.backlog.affected : '');
}

function renderPanel(inv) {
  document.querySelectorAll('#panel .light').forEach(el => {
    const k = el.dataset.k, v = inv && inv[k];
    el.classList.remove('ok', 'bad');
    if (!v || v.ok == null) { el.querySelector('span').textContent = 'pending'; return; } // M5's keys light up with no web change
    el.classList.add(v.ok ? 'ok' : 'bad');
    el.querySelector('span').textContent = (v.n == null ? '' : v.n + ' · ') + (v.at ? clock(v.at) : '') + (v.detail ? ' · ' + v.detail : '');
  });
}

// ---- cards ----------------------------------------------------------------------------------------------------------

function renderCards(s) {
  const running = s ? s.scenario.name : '';
  document.querySelectorAll('.card').forEach(card => {
    const mine = running && card.dataset.scenario.split(' ').includes(running);
    card.classList.toggle('running', !!mine);
    card.querySelectorAll('[data-start]').forEach(b => { b.disabled = !connected || !!running; });
    card.querySelectorAll('[data-stop]').forEach(b => { b.disabled = !connected || !mine; });
    const st = card.querySelector('.status');
    if (!mine) { st.textContent = ''; return; }
    const sc = s.scenario;
    let text = sc.phase;
    if (sc.ends_at == null) text += sc.phase === 'recovery' ? ' · waiting for drain' : ' · waiting for Restore';
    else text += ' · ' + Math.max(0, (sc.ends_at - s.t) / 1000).toFixed(0) + ' s';
    if (sc.recovery_s != null) text += ' · recovered in ' + sc.recovery_s.toFixed(1) + ' s';
    if (sc.drain_s != null) text += ' · drained in ' + sc.drain_s.toFixed(1) + ' s';
    st.textContent = text;
  });
  $('restore').disabled = !(connected && s && s.scenario.name === 'key_revocation' && s.scenario.phase === 'revoked');
}

async function post(url, body) {
  const r = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: body == null ? undefined : JSON.stringify(body) });
  if (r.status === 409) toast('busy: a scenario is already running');
  else if (r.status === 404) toast('unknown scenario or tenant');
  else if (r.status >= 400) toast('request failed: ' + r.status);
  return r;
}

function targetTenant() { // the ringed cell of the running scenario (code & 4)
  if (!snap) return null;
  for (let i = 0; i < N; i++) if ((snap.grid.charCodeAt(i) - 48) & 4) return tenantId(i);
  return null;
}

function bindCards() {
  document.querySelectorAll('[data-start]').forEach(b => b.onclick = () => post('/v1/scenarios/' + b.dataset.start + '/start'));
  document.querySelectorAll('[data-stop]').forEach(b => b.onclick = () => post('/v1/scenarios/x/stop'));
  document.querySelectorAll('[data-fetch]').forEach(b => b.onclick = () => post('/v1/keyfetch', { mode: b.dataset.fetch }));
  $('restore').onclick = () => { const id = targetTenant(); if (id) post('/v1/tenants/' + id + '/key', { action: 'restore' }); };
  $('reset').onclick = onReset;
  $('cardtoggle').onclick = () => setCards(document.body.classList.contains('nocards'));
  document.querySelectorAll('.card .hide').forEach(b => b.onclick = () => hideCard(b.closest('.card').dataset.scenario, true));
  document.querySelector('#hiddencards a').onclick = () => document.querySelectorAll('.card.hidden').forEach(c => hideCard(c.dataset.scenario, false));
  let show = true, hidden = [];
  try {
    show = localStorage.getItem('cards') !== 'hidden';
    hidden = JSON.parse(localStorage.getItem('cards.hidden') || '[]');
  } catch (e) { /* storage blocked: every card stays shown */ }
  setCards(show);
  document.querySelectorAll('.card').forEach(c => hideCard(c.dataset.scenario, hidden.includes(c.dataset.scenario)));
}

// renderMode highlights the active key-fetch mode on every set of mode buttons (the header's and the Slow KMS
// card's) and tints the header control while the service is not on the design that ships.
function renderMode(mode) {
  document.querySelectorAll('[data-fetch]').forEach(b => { b.classList.toggle('on', b.dataset.fetch === mode); b.disabled = !connected; });
  $('fetchctl').classList.toggle('on', !!mode && mode !== 'async');
}

// setCards shows or hides the whole scenario column (a per-viewer preference, remembered in this browser only).
function setCards(show) {
  document.body.classList.toggle('nocards', !show);
  $('cardtoggle').textContent = show ? 'Hide cards' : 'Show cards';
  try { localStorage.setItem('cards', show ? 'shown' : 'hidden'); } catch (e) { /* storage blocked */ }
}

// hideCard hides or shows one card (keyed by its data-scenario), remembers the set and keeps the "n hidden · show
// all cards" line current.
function hideCard(key, hide) {
  const card = document.querySelector('.card[data-scenario="' + key + '"]');
  if (card) card.classList.toggle('hidden', hide);
  const hidden = [...document.querySelectorAll('.card.hidden')];
  const line = $('hiddencards');
  line.classList.toggle('on', hidden.length > 0);
  line.querySelector('span').textContent = hidden.length + (hidden.length === 1 ? ' card hidden' : ' cards hidden');
  try { localStorage.setItem('cards.hidden', JSON.stringify(hidden.map(c => c.dataset.scenario))); } catch (e) { /* storage blocked */ }
}

function onReset() { post('/v1/reset'); } // the stream ends with the old World; the reconnect's first snapshot carries the new id and resets the page

let toastTimer = null;
function toast(text) {
  const t = $('toast');
  t.textContent = text;
  t.style.display = 'block';
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.style.display = 'none'; }, 3000);
}

// ---- charts ---------------------------------------------------------------------------------------------------------

function ring() { return []; } // grows to RING, then slides (uPlot needs a numeric, ascending x array: no null padding)
function push(arr, v) { arr.push(v); if (arr.length > RING) arr.shift(); }

const SERIES = {
  ingest: [['accepted', '#2ecc71'], ['rate_limited', '#f1c40f'], ['backlog_full', '#e67e22'], ['overloaded', '#e74c3c'], ['key_unavailable', '#3498db'], ['key_revoked', '#9b59b6']],
  p99: [['healthy', '#2ecc71'], ['affected', '#e74c3c'], ['baseline', '#9aa3b2', [4, 4]]],
  kms: [['ok', '#2ecc71'], ['transient', '#f1c40f'], ['deny', '#9b59b6'], ['events/s', '#3498db', [4, 4]]],
  backlog: [['total', '#3498db'], ['affected', '#e74c3c']],
};

function mkChart(name, el, log) {
  const data = [ring(), ...SERIES[name].map(ring)];
  const axis = { stroke: '#9aa3b2', grid: { stroke: '#2a2f3a', width: 1 }, ticks: { stroke: '#2a2f3a' } };
  const opts = {
    width: el.clientWidth || 390, height: 170,
    cursor: { show: true, y: false }, legend: { show: true },
    scales: { x: { time: true }, y: log ? { distr: 3, range: (u, min, max) => [min > 0 ? min / 1.5 : 1, max > 0 ? max * 1.5 : 100] } : { range: (u, min, max) => [0, max > 0 ? max * 1.1 : 1] } },
    axes: [{ ...axis, space: 60 }, { ...axis, size: 52 }],
    series: [{ label: 'time' }, ...SERIES[name].map(([label, stroke, dash]) => ({ label, stroke, width: 1.5, dash, spanGaps: false }))],
  };
  charts[name] = { u: new uPlot(opts, data, el), data };
}

function resetRings() {
  for (const c of Object.values(charts)) { c.data.forEach((arr, i) => { c.data[i] = ring(); }); c.u.setData(c.data); }
}

function pushCharts(s) {
  const t = s.t / 1000, nz = v => v > 0 ? v : null; // 0 samples plot as a gap on the log axis
  const rows = {
    ingest: [s.ingest_ps.accepted, s.ingest_ps.rate_limited, s.ingest_ps.backlog_full, s.ingest_ps.overloaded, s.ingest_ps.key_unavailable, s.ingest_ps.key_revoked],
    p99: [nz(s.p99_ms.healthy), nz(s.p99_ms.affected), nz(s.p99_ms.baseline)],
    kms: [s.kms_ps.ok, s.kms_ps.transient, s.kms_ps.deny, s.kms_ps.events_ps],
    backlog: [s.backlog.total, s.backlog.affected],
  };
  for (const [name, vals] of Object.entries(rows)) {
    const c = charts[name];
    if (!c) continue;
    push(c.data[0], t);
    vals.forEach((v, i) => push(c.data[i + 1], v));
    c.u.setData(c.data);
  }
}

function initCharts() {
  mkChart('ingest', $('c-ingest'), false);
  mkChart('p99', $('c-p99'), true);
  mkChart('kms', $('c-kms'), false);
  mkChart('backlog', $('c-backlog'), false);
  window.addEventListener('resize', () => { for (const [name, c] of Object.entries(charts)) c.u.setSize({ width: $('c-' + name).clientWidth, height: 170 }); });
}

// ---- timeline -------------------------------------------------------------------------------------------------------

function clock(ms) { return new Date(ms).toLocaleTimeString([], { hour12: false }); }

function renderTimeline(evs) {
  if (!evs) return;
  const ol = $('timeline');
  for (let i = evs.length - 1; i >= 0; i--) { // newest-first in the snapshot; prepend oldest-first so order holds
    const e = evs[i];
    if (e.seq <= lastSeq) continue;
    lastSeq = e.seq;
    const li = document.createElement('li');
    li.textContent = clock(e.at) + ' ' + e.text;
    if (/^scenario |restored|not restored/.test(e.text)) li.className = 'mark';
    ol.prepend(li);
  }
  while (ol.children.length > 200) ol.removeChild(ol.lastChild);
}

// ---- hover and pin --------------------------------------------------------------------------------------------------

function tipText(i, d) {
  const code = snap ? snap.grid.charCodeAt(i) - 48 : -1;
  let t = tenantId(i) + ' · ' + providerOf(i) + (code >= 0 ? ' · ' + STATES[code & 3] : '') + (code >= 0 && (code & 4) ? ' · target' : '');
  if (d) t += '\nrank ' + d.rank + ' · lease ' + (d.lease_remaining_ms / 1000).toFixed(1) + ' s left · backlog ' + d.backlog + ' · ' + d.offered_ps.toFixed(1) + ' ev/s · ' + d.kms_calls_per_min + ' KMS calls/min';
  return t;
}

async function fetchTenant(i) {
  const r = await fetch('/v1/tenants/' + tenantId(i));
  return r.ok ? r.json() : null;
}

function bindGrid() {
  const g = $('grid'), tip = $('tip');
  g.onmousemove = ev => {
    const i = cellAt(ev);
    if (i < 0) { tip.style.display = 'none'; hoverIdx = -1; return; }
    tip.style.display = 'block';
    tip.style.left = (ev.clientX - g.getBoundingClientRect().left + 24) + 'px';
    tip.style.top = (ev.clientY - g.getBoundingClientRect().top + 4) + 'px';
    if (i !== hoverIdx) {
      hoverIdx = i;
      tip.textContent = tipText(i);
      clearTimeout(hoverTimer);
      hoverTimer = setTimeout(async () => { const d = await fetchTenant(i); if (d && hoverIdx === i) tip.textContent = tipText(i, d); }, 150);
    }
  };
  g.onmouseleave = () => { tip.style.display = 'none'; hoverIdx = -1; };
  g.onclick = ev => { const i = cellAt(ev); if (i >= 0) pin(i === pinIdx ? -1 : i); };
}

function pin(i) {
  clearInterval(pinTimer);
  pinIdx = i;
  const el = $('pin');
  if (i < 0) { el.style.display = 'none'; el.innerHTML = ''; return; }
  el.style.display = 'block';
  const refresh = async () => {
    const d = await fetchTenant(i);
    if (!d || pinIdx !== i) return;
    // built with createElement + textContent: server strings (state, audit detail) are never parsed as HTML
    el.replaceChildren();
    const b = document.createElement('b');
    b.textContent = d.id;
    el.append(b, ' · ' + d.provider + ' · ' + d.state + ' · rank ' + d.rank + ' · lease age ' + (d.lease_age_ms / 1000).toFixed(1) + ' s, ' + (d.lease_remaining_ms / 1000).toFixed(1) + ' s left' +
      (d.next_probe_ms > 0 ? ' · next probe in ' + (d.next_probe_ms / 1000).toFixed(1) + ' s' : '') + ' · backlog ' + d.backlog + ' (ready ' + d.ready + ') · offered ' + d.offered_ps.toFixed(1) + '/s · KMS ' + d.kms_calls_per_min + '/min');
    if (d.affected) {
      const aff = document.createElement('span');
      aff.style.color = '#e74c3c';
      aff.textContent = 'affected';
      el.append(' · ', aff);
    }
    const unpin = document.createElement('button');
    unpin.textContent = 'unpin';
    unpin.style.cssText = 'float:right;padding:1px 6px';
    unpin.onclick = () => pin(-1);
    el.append(' ', unpin);
    const ol = document.createElement('ol');
    const audit = d.audit || [];
    for (const a of audit) {
      const li = document.createElement('li');
      li.textContent = clock(Date.parse(a.at)) + ' ' + a.op + ' ' + a.outcome + (a.detail ? ' (' + a.detail + ')' : '') + (a.latency_ms ? ' ' + a.latency_ms.toFixed(0) + ' ms' : '');
      ol.append(li);
    }
    if (!audit.length) {
      const li = document.createElement('li');
      li.textContent = 'no audit entries yet';
      ol.append(li);
    }
    el.append(ol);
  };
  refresh();
  pinTimer = setInterval(refresh, 2000);
}

// ---- hover text -------------------------------------------------------------------------------------------------------

// bindHelp shows an element's data-tip in #help as soon as the pointer enters it (native title tooltips need a
// second of rest and are easy to miss), below the element and kept inside the viewport.
function bindHelp() {
  const help = $('help');
  document.querySelectorAll('[data-tip]').forEach(el => {
    el.addEventListener('mouseenter', () => {
      help.textContent = el.dataset.tip;
      help.style.display = 'block';
      const r = el.getBoundingClientRect();
      const w = help.offsetWidth, h = help.offsetHeight;
      help.style.left = Math.max(8, Math.min(r.left, window.innerWidth - w - 8)) + 'px';
      help.style.top = (r.bottom + 6 + h > window.innerHeight ? r.top - h - 6 : r.bottom + 6) + 'px';
    });
    el.addEventListener('mouseleave', () => { help.style.display = 'none'; });
  });
}

// ---- boot -----------------------------------------------------------------------------------------------------------

paintGrid(null);
drawBands();
initCharts();
bindCards();
bindGrid();
bindHelp();
renderCards(null);
connect();
