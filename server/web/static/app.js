// Cribbage web client. Talks to the server over a WebRTC data channel using
// the protobuf JSON mapping (see proto/cribbage/v1/cribbage.proto).
//
// Every GameUpdate carries the events since the previous one. In the default
// (animated) mode they are replayed one at a time onto a local view of the
// table, moving pegs and cards as they happen; the view then snaps to the
// authoritative observation. Rapid play applies them all at once.
'use strict';

const RANKS = ['A', '2', '3', '4', '5', '6', '7', '8', '9', '10', 'J', 'Q', 'K'];
const SUITS = ['♣', '♦', '♥', '♠'];
const cardName = c => RANKS[c % 13] + SUITS[Math.floor(c / 13)];
const isRed = c => [1, 2].includes(Math.floor(c / 13));
const $ = id => document.getElementById(id);
const esc = s => String(s).replace(/[&<>"']/g, ch => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]));
const narrow = () => matchMedia('(max-width: 860px)').matches;

// ---------------------------------------------------------------------------
// Connection: WebSocket signaling, then a data channel carrying JSON.
// ---------------------------------------------------------------------------

class Connection {
  constructor(onMessage, onOpen, onClose) {
    this.onMessage = onMessage;
    this.onOpen = onOpen;
    this.onClose = onClose;
    this.nextId = 0;
    this.dc = null;
    this.retry = 500;
  }

  async connect() {
    let cfg = { iceServers: [] };
    try { cfg = await (await fetch('api/config')).json(); } catch (e) { /* use defaults */ }
    const pc = new RTCPeerConnection({ iceServers: cfg.iceServers });
    const dc = pc.createDataChannel('cribbage', { ordered: true });
    const ws = new WebSocket(location.href.replace(/^http/, 'ws').replace(/[^/]*$/, 'rtc'));
    let described = false;
    const localPending = [];
    let remoteSet = false;
    const remotePending = [];
    let closed = false;

    const fail = () => {
      if (closed) return;
      closed = true;
      try { ws.close(); } catch (e) { /* ignore */ }
      try { pc.close(); } catch (e) { /* ignore */ }
      this.dc = null;
      this.onClose();
      setTimeout(() => this.connect(), this.retry);
      this.retry = Math.min(this.retry * 2, 5000);
    };

    pc.onicecandidate = e => {
      if (!e.candidate) return;
      const m = JSON.stringify({ type: 'candidate', candidate: e.candidate.toJSON() });
      if (described && ws.readyState === WebSocket.OPEN) ws.send(m); else localPending.push(m);
    };
    pc.onconnectionstatechange = () => {
      if (['failed', 'closed'].includes(pc.connectionState)) fail();
    };
    ws.onopen = async () => {
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      ws.send(JSON.stringify({ type: 'offer', sdp: offer.sdp }));
      described = true;
      localPending.splice(0).forEach(m => ws.send(m));
    };
    // Give up if the data channel hasn't opened in time.
    const timer = setTimeout(() => { if (!this.dc) fail(); }, 15000);
    ws.onmessage = async ev => {
      const m = JSON.parse(ev.data);
      if (m.type === 'answer') {
        await pc.setRemoteDescription({ type: 'answer', sdp: m.sdp });
        remoteSet = true;
        for (const c of remotePending.splice(0)) await pc.addIceCandidate(c);
      } else if (m.type === 'candidate' && m.candidate) {
        if (remoteSet) await pc.addIceCandidate(m.candidate); else remotePending.push(m.candidate);
      }
    };
    // The server closes signaling once its side of the data channel opens,
    // which may be before ours does, so a close after the answer is normal.
    ws.onerror = () => { if (!this.dc && !remoteSet) fail(); };
    ws.onclose = () => { if (!this.dc && !remoteSet) fail(); };
    dc.onopen = () => {
      clearTimeout(timer);
      this.dc = dc;
      this.retry = 500;
      ws.close();
      this.onOpen();
    };
    dc.onmessage = e => this.onMessage(JSON.parse(e.data));
    dc.onclose = fail;
  }

  send(body) {
    if (!this.dc || this.dc.readyState !== 'open') return 0;
    const id = ++this.nextId;
    this.dc.send(JSON.stringify(Object.assign({ requestId: id }, body)));
    return id;
  }
}

// ---------------------------------------------------------------------------
// Preferences, kept in localStorage between sessions.
// ---------------------------------------------------------------------------

const THEMES = { felt: '#123826', parchment: '#2f6d41', midnight: '#0b0e14' };
const prefs = Object.assign(
  { theme: 'felt', rapid: matchMedia('(prefers-reduced-motion: reduce)').matches, muted: false },
  (() => { try { return JSON.parse(localStorage.getItem('prefs')) || {}; } catch (e) { return {}; } })(),
);
if (!(prefs.theme in THEMES)) prefs.theme = 'felt';

function savePrefs() {
  try { localStorage.setItem('prefs', JSON.stringify(prefs)); } catch (e) { /* private mode */ }
}

function applyPrefs() {
  document.documentElement.dataset.theme = prefs.theme;
  document.querySelector('meta[name=theme-color]').content = THEMES[prefs.theme];
  $('theme').value = prefs.theme;
  $('rapid').checked = prefs.rapid;
  $('mute').innerHTML = `<svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M11 5 6 9H2v6h4l5 4z"/>${prefs.muted ? '<path d="m22 9-6 6M16 9l6 6"/>' : '<path d="M15.5 8.5a5 5 0 0 1 0 7M19 5a10 10 0 0 1 0 14"/>'}</svg>`;
  $('mute').title = prefs.muted ? 'Unmute sounds' : 'Mute sounds';
  $('mute').setAttribute('aria-pressed', String(prefs.muted));
}

// ---------------------------------------------------------------------------
// Sound: short effects synthesized with Web Audio, so there are no files.
// They play only in animated mode and when not muted.
// ---------------------------------------------------------------------------

const sfx = (() => {
  let ctx = null;
  let noiseBuf = null;
  const unlock = () => {
    if (!ctx) {
      const AC = window.AudioContext || window.webkitAudioContext;
      if (!AC) return;
      ctx = new AC();
      noiseBuf = ctx.createBuffer(1, ctx.sampleRate / 2, ctx.sampleRate);
      const d = noiseBuf.getChannelData(0);
      for (let i = 0; i < d.length; i++) d[i] = Math.random() * 2 - 1;
    }
    if (ctx.state === 'suspended') ctx.resume();
  };
  const tone = (f, at, dur, type = 'sine', vol = .12, slide = 0) => {
    const o = ctx.createOscillator(), g = ctx.createGain();
    o.type = type;
    o.frequency.setValueAtTime(f, at);
    if (slide) o.frequency.exponentialRampToValueAtTime(slide, at + dur);
    g.gain.setValueAtTime(0, at);
    g.gain.linearRampToValueAtTime(vol, at + .006);
    g.gain.exponentialRampToValueAtTime(.0001, at + dur);
    o.connect(g).connect(ctx.destination);
    o.start(at);
    o.stop(at + dur + .02);
  };
  const noise = (at, dur, vol, freq) => {
    const s = ctx.createBufferSource(), f = ctx.createBiquadFilter(), g = ctx.createGain();
    s.buffer = noiseBuf;
    f.type = 'bandpass';
    f.frequency.value = freq;
    f.Q.value = 1.2;
    g.gain.setValueAtTime(vol, at);
    g.gain.exponentialRampToValueAtTime(.0001, at + dur);
    s.connect(f).connect(g).connect(ctx.destination);
    s.start(at);
    s.stop(at + dur + .02);
  };
  const play = fn => () => {
    if (prefs.muted || prefs.rapid || !ctx || ctx.state !== 'running') return;
    try { fn(ctx.currentTime); } catch (e) { /* ignore audio errors */ }
  };
  return {
    unlock,
    card: play(t => { noise(t, .08, .5, 2200); tone(160, t, .05, 'triangle', .06); }),
    deal: play(t => { for (let i = 0; i < 6; i++) noise(t + i * .07, .06, .4, 2000 + i * 150); }),
    peg: play(t => { tone(820, t, .07, 'triangle', .16, 420); noise(t, .025, .3, 3500); }),
    score: play(t => { [660, 880, 1175].forEach((f, i) => tone(f, t + i * .075, .16, 'triangle', .09)); }),
    go: play(t => tone(300, t, .2, 'sine', .12, 220)),
    turn: play(t => { tone(988, t, .14, 'sine', .07); tone(1319, t + .09, .22, 'sine', .06); }),
    win: play(t => {
      [523, 659, 784, 1047].forEach((f, i) => tone(f, t + i * .12, .3, 'triangle', .11));
      [523, 659, 784].forEach(f => tone(f, t + .5, 1.1, 'sine', .07));
    }),
    lose: play(t => { [440, 392, 349, 294].forEach((f, i) => tone(f, t + i * .18, .35, 'triangle', .09)); }),
  };
})();

// ---------------------------------------------------------------------------
// The cribbage board: a continuous 121-hole board drawn in SVG. Four runs of
// 30 holes snake across it (1-30 out, 31-60 back, 61-90 out, 91-120 back),
// with the two players' lanes side by side all the way; each player then
// finishes in their own 121 game hole. The lanes swap sides at every turn so
// the turns nest, as on a real board. Pegs persist between renders so they can
// hop from hole to hole.
// ---------------------------------------------------------------------------

const Board = (() => {
  const NS = 'http://www.w3.org/2000/svg';
  const X0 = 132, STEP = 25, GAP = 11, W = 1000, H = 232;
  const LANE = 22, RUN = 50, TOP = 30; // lane spacing, run spacing, first lane
  const holeX = i => X0 + i * STEP + Math.floor(i / 5) * GAP;
  const LAST = holeX(29);
  const XL = X0 - 14, XR = LAST + 14; // where straight runs meet the turns
  // laneY is the y of seat k's lane in run r (0-3). Even runs put the
  // opponent above; odd runs swap.
  const laneY = (r, k) => TOP + r * RUN + ((k === 'opp') === (r % 2 === 0) ? 0 : LANE);
  let svg = null, gameId = null;
  const pegs = { me: [], opp: [] };
  let pos = { me: null, opp: null };
  const names = {};

  const el = (tag, attrs, parent) => {
    const e = document.createElementNS(NS, tag);
    for (const k in attrs) e.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(e);
    return e;
  };

  function xy(k, s, pegIdx) {
    if (s <= 0) return [pegIdx ? 98 : 74, laneY(0, k)];
    if (s >= 121) return [98, laneY(3, k)];
    const r = Math.floor((s - 1) / 30), i = (s - 1) % 30;
    return [holeX(r % 2 ? 29 - i : i), laneY(r, k)];
  }

  // lanePath traces seat k's whole track, from the start holes, through the
  // four runs and three turns, to the game hole.
  function lanePath(k) {
    let d = `M 60 ${laneY(0, k)} H ${XR}`;
    for (let r = 1; r < 4; r++) {
      const y0 = laneY(r - 1, k), y1 = laneY(r, k), right = r % 2 === 1;
      d += ` A ${(y1 - y0) / 2} ${(y1 - y0) / 2} 0 0 ${right ? 1 : 0} ${right ? XR : XL} ${y1}`;
      d += ` H ${r === 3 ? 86 : right ? XL : XR}`;
    }
    return d;
  }

  // skunkLine marks the turn between runs r-1 and r: a dashed line across
  // both lanes at the apex of the turn.
  function skunkLine(r, label) {
    const right = r % 2 === 1;
    const cy = (TOP + (r - 1) * RUN + TOP + r * RUN + LANE) / 2;
    const x = right ? XR : XL, dir = right ? 1 : -1;
    el('line', { class: 'skunk', x1: x + dir * 6, y1: cy, x2: x + dir * 44, y2: cy }, svg);
    el('text', { class: 'skunk-text', x: x + dir * 50, y: cy + 4, 'text-anchor': right ? 'start' : 'end' }, svg).textContent = label;
  }

  function build(container) {
    svg = el('svg', { viewBox: `0 0 ${W} ${H}`, role: 'img', 'aria-label': 'Cribbage board' }, container);
    for (const k of ['opp', 'me']) el('path', { class: `lane ${k}`, d: lanePath(k) }, svg);
    for (const k of ['opp', 'me']) el('path', { class: 'groove', d: lanePath(k) }, svg);
    for (let r = 0; r < 4; r++) {
      for (const k of ['opp', 'me']) {
        for (let i = 0; i < 30; i++) el('circle', { class: 'hole' + (i % 5 === 4 ? ' tick' : ''), cx: holeX(i), cy: laneY(r, k), r: 4.6 }, svg);
      }
    }
    for (const k of ['opp', 'me']) {
      for (const x of [74, 98]) el('circle', { class: 'hole', cx: x, cy: laneY(0, k), r: 4.6 }, svg);
      el('circle', { class: 'game-hole', cx: 98, cy: laneY(3, k), r: 7 }, svg);
      names[k] = el('text', { class: `name lbl-${k}`, x: 12, y: laneY(0, k) + 4 }, svg);
    }
    el('text', { class: 'goal', x: 82, y: (laneY(3, 'me') + laneY(3, 'opp')) / 2 + 4, 'text-anchor': 'end' }, svg).textContent = '121';
    // A player who loses without passing 61 is double-skunked; without
    // passing 91, skunked.
    skunkLine(2, 'SS');
    skunkLine(3, 'S');
    for (const k of ['opp', 'me']) {
      for (let i = 0; i < 2; i++) {
        const g = el('g', { class: `peg ${k}` }, svg);
        el('circle', { class: 'body', r: 9 }, g);
        el('circle', { class: 'shine', r: 2.6, cx: -3, cy: -3 }, g);
        pegs[k].push(g);
      }
    }
  }

  function place(k, i, s, animate) {
    const [x, y] = xy(k, s, i);
    const g = pegs[k][i];
    const to = `translate(${x}px, ${y}px)`;
    if (animate && g._xy && g.animate) {
      const [fx, fy] = g._xy;
      const mid = `translate(${(fx + x) / 2}px, ${Math.min(fy, y) - 26}px) scale(1.6)`;
      svg.appendChild(g); // draw the moving peg on top
      g.animate([{ transform: g.style.transform }, { transform: mid, offset: .45 }, { transform: to }],
        { duration: 700, easing: 'cubic-bezier(.3, .7, .4, 1)' });
      setTimeout(sfx.peg, 640);
    }
    g.style.transform = to;
    g._xy = [x, y];
  }

  // update moves seat k's pegs so the front one is at score and the back one
  // at trail, leapfrogging the way a real peg does.
  function update(k, score, trail, animate) {
    const cur = pos[k];
    let next;
    if (!cur) next = [trail, score];
    else if ((cur[0] === trail && cur[1] === score) || (cur[0] === score && cur[1] === trail)) next = cur;
    else if (cur[0] === trail) next = [trail, score];
    else if (cur[1] === trail) next = [score, trail];
    else if (cur[0] === score) next = [score, trail];
    else if (cur[1] === score) next = [trail, score];
    else next = [trail, score];
    next.forEach((s, i) => { if (!cur || cur[i] !== s) place(k, i, s, animate && !!cur); });
    pos[k] = next;
    const front = Math.max(...next);
    next.forEach((s, i) => {
      pegs[k][i].classList.toggle('trail', s < front);
    });
  }

  return {
    build,
    show(id, meName, oppName, v, animate) {
      if (id !== gameId) { gameId = id; pos = { me: null, opp: null }; animate = false; }
      names.me.textContent = meName.toUpperCase().slice(0, 7);
      names.opp.textContent = oppName.toUpperCase().slice(0, 7);
      const me = v.g.seat < 0 ? 0 : v.g.seat;
      update('me', v.scores[me], v.trail[me], animate);
      update('opp', v.scores[1 - me], v.trail[1 - me], animate);
    },
  };
})();

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

const state = {
  me: null,           // AuthOk
  games: new Map(),   // gameId -> latest GameUpdate (or {game} from a list)
  views: new Map(),   // gameId -> view: what the table currently shows
  logs: new Map(),    // gameId -> [{text, cls}]
  open: [],
  recent: [],
  selected: null,
  picked: new Set(),  // cards selected for discard
  logShown: { id: null, n: 0 },
};

function emptyView(g) {
  const s = (g.scores || [0, 0]).slice();
  return {
    g, scores: s, trail: s.slice(), phase: '', handNumber: 0, dealer: -1, hand: [], oppHandSize: 0,
    discarded: false, oppDiscarded: false, myDiscards: [], starter: null, played: [], seqStart: 0, count: 0,
    shows: [], oppReveal: null, cribOpen: null, legal: [], mustAct: false, busy: false,
    gameOver: null, bannerClosed: false,
    // One-shot animation hints, cleared after each render.
    fresh: new Set(), freshOpp: false, freshStarter: false, freshCrib: false, bump: new Set(), hot: false,
  };
}

function newView(u) {
  const v = emptyView(u.game);
  if (u.observation) syncView(v, u);
  return v;
}

function view(id) {
  if (!state.views.has(id) && state.games.has(id)) state.views.set(id, newView(state.games.get(id)));
  return state.views.get(id);
}

// syncView makes v agree with the server's observation.
function syncView(v, u) {
  const g = u.game, o = u.observation;
  v.g = g;
  const scores = (o && o.scores) || g.scores || [0, 0];
  for (const s of [0, 1]) {
    if (scores[s] !== v.scores[s]) { v.trail[s] = v.scores[s]; v.scores[s] = scores[s]; }
  }
  if (g.status === 'GAME_STATUS_FINISHED' && !v.gameOver && g.winner >= 0) {
    v.gameOver = { winner: g.winner, resigned: g.resigned };
  }
  v.legal = u.legal || [];
  if (!o) { v.phase = ''; v.mustAct = false; return; }
  if (o.phase === 'PHASE_PEGGING') v.shows = [];
  if (o.phase !== 'PHASE_GAME_OVER') { v.oppReveal = null; v.cribOpen = null; }
  Object.assign(v, {
    phase: o.phase, handNumber: o.handNumber, dealer: o.dealer, hand: o.hand || [], oppHandSize: o.oppHandSize || 0,
    discarded: !!o.discarded, oppDiscarded: !!o.oppDiscarded, myDiscards: o.discards || [],
    starter: o.starter === undefined ? null : o.starter, played: o.played || [], seqStart: o.seqStart || 0,
    count: o.count || 0, mustAct: !!o.mustAct,
  });
}

// ---------------------------------------------------------------------------
// Connection events
// ---------------------------------------------------------------------------

const conn = new Connection(onMessage, onOpen, onClose);

function onOpen() {
  setStatus('online');
  state.me = null; // each connection authenticates afresh
  const token = localStorage.getItem('token');
  if (token) conn.send({ resume: { token } });
  else show('login');
}

function onClose() {
  setStatus('reconnecting…');
}

function setStatus(text) {
  $('status').textContent = text;
  $('status').classList.toggle('online', text === 'online');
}

function show(...ids) {
  for (const id of ['login', 'lobby']) $(id).hidden = !ids.includes(id);
  if (!ids.includes('lobby')) { $('table').hidden = true; $('empty').hidden = true; }
}

function notice(text) {
  $('notice').textContent = text || '';
  if (text) toast(text);
}

function toast(text) {
  const t = document.createElement('div');
  t.className = 'toast';
  t.textContent = text;
  $('toasts').appendChild(t);
  setTimeout(() => t.remove(), 3300);
}

function onMessage(m) {
  if (m.authOk) {
    state.me = m.authOk;
    localStorage.setItem('token', m.authOk.token);
    $('who').textContent = `Signed in as ${m.authOk.nickname} (${m.authOk.username})`;
    $('account').hidden = false;
    $('login-error').textContent = '';
    show('lobby');
    renderTable();
    conn.send({ listGames: {} });
  } else if (m.error) {
    onError(m.error);
  } else if (m.gameUpdate) {
    onUpdate(m.gameUpdate);
  } else if (m.gameList) {
    const l = m.gameList;
    for (const g of l.mine || []) {
      if (!state.games.has(g.gameId)) state.games.set(g.gameId, { game: g });
    }
    state.open = l.open || [];
    state.recent = l.recent || [];
    renderLobby();
  } else if (m.matchQueued) {
    notice('Waiting for an opponent…');
  }
}

function onError(e) {
  if (!state.me && ['ERROR_CODE_INVALID_CREDENTIALS', 'ERROR_CODE_UNAUTHENTICATED'].includes(e.code)) {
    if (localStorage.getItem('token')) {
      localStorage.removeItem('token'); // stale session; ask for a password
      show('login');
      return;
    }
  }
  if (!state.me) {
    $('login-error').textContent = e.message;
    show('login');
    return;
  }
  notice(e.message);
}

function onUpdate(u) {
  const id = u.game.gameId;
  const prev = state.games.get(id);
  if (!state.views.has(id)) state.views.set(id, newView(prev || { game: u.game }));
  state.games.set(id, u);
  if (u.game.status === 'GAME_STATUS_FINISHED') {
    if (id !== state.selected) notice(gameOverText(u.game));
    conn.send({ listGames: {} });
  }
  if (!state.selected || (!prev && u.game.status !== 'GAME_STATUS_FINISHED' && !u.full)) select(id);
  if (u.game.status === 'GAME_STATUS_ACTIVE' && $('notice').textContent.startsWith('Waiting for an opponent')) $('notice').textContent = '';
  renderLobby();
  updateTitle();
  enqueue(u);
}

// ---------------------------------------------------------------------------
// Replaying events
// ---------------------------------------------------------------------------

const anim = { queue: [], running: false, skip: false, waiters: new Set() };

function enqueue(u) {
  anim.queue.push(u);
  if (!anim.running) drain();
}

async function drain() {
  anim.running = true;
  while (anim.queue.length) {
    try { await replay(anim.queue.shift()); } catch (e) { console.error(e); }
  }
  anim.running = false;
  anim.skip = false;
}

function wait(ms) {
  if (anim.skip || prefs.rapid) return Promise.resolve();
  return new Promise(resolve => {
    const done = () => { clearTimeout(t); anim.waiters.delete(done); resolve(); };
    const t = setTimeout(done, ms);
    anim.waiters.add(done);
  });
}

// skipAnimation finishes any replay in progress immediately.
function skipAnimation() {
  if (!anim.running) return;
  anim.skip = true;
  for (const w of [...anim.waiters]) w();
}

async function replay(u) {
  const id = u.game.gameId;
  const animated = () => !anim.skip && !prefs.rapid && id === state.selected && !document.hidden;
  let v = view(id);
  if (u.full || !v) {
    v = newView(u);
    state.views.set(id, v);
  } else {
    const events = u.events || [];
    let oppActed = false;
    v.busy = events.length > 0;
    for (const e of events) {
      const fx = applyEvent(v, e, u);
      if (e.seat !== u.game.seat) oppActed = true;
      if (animated()) {
        renderTable(true);
        effects(v, fx);
        await wait(fx.ms);
      }
    }
    syncView(v, u);
    v.busy = false;
    if (oppActed && v.mustAct && animated()) sfx.turn();
  }
  if (id === state.selected) renderTable(animated());
  else renderLog();
}

function dealtHand(u) {
  const o = u.observation;
  if (!o) return [];
  return o.discarded ? [...(o.kept || []), ...(o.discards || [])] : [...(o.hand || [])];
}

// applyEvent updates the view for one event, logs it, and returns the effects
// to show and how long to pause afterwards.
function applyEvent(v, e, u) {
  const g = u.game, me = g.seat, mine = e.seat === me;
  const fx = { ms: 600, scored: [], label: '', at: 'plate', sound: null, note: null };
  const k = e.kind;
  const cls = k === 'EVENT_KIND_DEAL' ? 'hand' : ['EVENT_KIND_COUNT_RESET', 'EVENT_KIND_CUT_FOR_DEAL'].includes(k) ? '' : mine ? 'me' : 'opp';
  pushLog(g.gameId, describe(g, e), cls, e.points);
  if (e.scores && e.scores.length === 2) {
    for (const s of [0, 1]) {
      if (e.scores[s] !== v.scores[s]) {
        fx.scored.push({ seat: s, pts: e.scores[s] - v.scores[s] });
        v.trail[s] = v.scores[s];
        v.scores[s] = e.scores[s];
        v.bump.add(s);
      }
    }
  }
  switch (k) {
    case 'EVENT_KIND_CUT_FOR_DEAL':
      fx.ms = 1300;
      fx.note = { text: `Cut: ${cardName(e.cards[me === 1 ? 1 : 0])} vs ${cardName(e.cards[me === 1 ? 0 : 1])}`, at: 'deck' };
      break;
    case 'EVENT_KIND_DEAL':
      Object.assign(v, {
        dealer: e.seat, phase: 'PHASE_DISCARD', played: [], seqStart: 0, count: 0, starter: null, discarded: false,
        oppDiscarded: false, myDiscards: [], oppReveal: null, cribOpen: null, hand: dealtHand(u), oppHandSize: 6, legal: [],
      });
      v.hand.forEach(c => v.fresh.add(c));
      v.freshOpp = true;
      fx.sound = 'deal';
      fx.ms = 1000;
      break;
    case 'EVENT_KIND_DISCARD':
      if (mine) {
        const d = (u.observation && u.observation.discards) || [];
        v.hand = v.hand.filter(c => !d.includes(c));
        v.myDiscards = d;
        v.discarded = true;
      } else {
        v.oppDiscarded = true;
        v.oppHandSize = Math.max(0, v.oppHandSize - 2);
      }
      v.freshCrib = true;
      fx.sound = 'card';
      fx.ms = mine ? 300 : 650;
      break;
    case 'EVENT_KIND_STARTER':
      v.starter = e.card;
      v.phase = 'PHASE_PEGGING';
      v.shows = [];
      v.freshStarter = true;
      fx.sound = 'card';
      fx.ms = 1100;
      if (e.points) { fx.label = 'his heels'; fx.at = 'deck'; fx.ms = 1600; }
      break;
    case 'EVENT_KIND_PLAY':
      v.played = [...v.played, { card: e.card, seat: e.seat }];
      v.count = e.count;
      if (mine) v.hand = v.hand.filter(c => c !== e.card);
      else v.oppHandSize = Math.max(0, v.oppHandSize - 1);
      v.fresh.add(e.card);
      v.hot = e.count === 15 || e.count === 31;
      fx.sound = 'card';
      fx.ms = mine ? 500 : 950;
      if (e.points) { fx.label = pegDetail(e.peg || {}); fx.at = 'play'; fx.ms += 600; }
      break;
    case 'EVENT_KIND_GO':
      fx.note = { text: `${mine ? 'You' : 'They'} say “Go”`, at: 'play' };
      fx.sound = 'go';
      fx.ms = 900;
      break;
    case 'EVENT_KIND_GO_POINT':
      fx.label = 'for the go'; fx.at = 'play'; fx.ms = 900;
      break;
    case 'EVENT_KIND_LAST_CARD':
      fx.label = 'last card'; fx.at = 'play'; fx.ms = 900;
      break;
    case 'EVENT_KIND_COUNT_RESET':
      v.seqStart = v.played.length;
      v.count = 0;
      fx.ms = 550;
      break;
    case 'EVENT_KIND_SHOW_HAND':
    case 'EVENT_KIND_SHOW_CRIB': {
      const crib = k === 'EVENT_KIND_SHOW_CRIB';
      if (v.phase !== 'SHOW') { v.phase = 'SHOW'; v.shows = []; }
      v.shows.push({ seat: e.seat, crib, cards: e.cards || [], starter: e.card, points: e.points, show: e.show || {}, fresh: true });
      if (crib) v.cribOpen = e.cards || [];
      else if (!mine) v.oppReveal = e.cards || [];
      fx.label = crib ? 'crib' : 'hand';
      fx.ms = 2400;
      break;
    }
    case 'EVENT_KIND_GAME_OVER':
      v.gameOver = { winner: e.seat, skunk: e.skunk, fresh: true };
      fx.ms = 0;
      break;
    case 'EVENT_KIND_RESIGN':
      v.gameOver = { winner: 1 - e.seat, resigned: true, fresh: true };
      fx.ms = 0;
      break;
  }
  return fx;
}

function effects(v, fx) {
  if (fx.sound) sfx[fx.sound]();
  for (const s of fx.scored) {
    const who = s.seat === v.g.seat ? 'me' : 'opp';
    floatAt(fx.at, s.seat, `+${s.pts}${fx.label ? `<small>${esc(fx.label)}</small>` : ''}`, who);
  }
  if (fx.scored.length) setTimeout(sfx.score, 120);
  if (fx.note) floatAt(fx.note.at, null, esc(fx.note.text), 'neutral');
}

// floatAt shows a rising label over the table near a player's score, the
// play area or the deck.
function floatAt(at, seat, html, cls) {
  const layer = $('fx');
  let anchor;
  if (at === 'plate') anchor = document.querySelector(seat === state.views.get(state.selected).g.seat ? '#me-plate .pts' : '#opp-plate .pts');
  else if (at === 'deck') anchor = $('deck');
  else anchor = $('sequence');
  if (!anchor) return;
  const lr = layer.getBoundingClientRect(), r = anchor.getBoundingClientRect();
  const x = Math.min(Math.max(r.left + r.width / 2 - lr.left, 70), lr.width - 70);
  const y = Math.max(r.top + r.height / 2 - lr.top, 30);
  const f = document.createElement('div');
  f.className = `float ${cls}`;
  f.innerHTML = html;
  f.style.left = `${x}px`;
  f.style.top = `${y}px`;
  layer.appendChild(f);
  setTimeout(() => f.remove(), 1700);
}

// ---------------------------------------------------------------------------
// Game log
// ---------------------------------------------------------------------------

function seatName(g, seat) {
  if (seat === g.seat) return 'You';
  const p = (g.players || [])[seat];
  return p ? p.nickname : `Seat ${seat}`;
}

function breakdown(b) {
  const parts = [];
  if (b.fifteens) parts.push(`fifteens ${b.fifteens}`);
  if (b.pairs) parts.push(`pairs ${b.pairs}`);
  if (b.runs) parts.push(`runs ${b.runs}`);
  if (b.flush) parts.push(`flush ${b.flush}`);
  if (b.nobs) parts.push(`nobs ${b.nobs}`);
  return parts.join(', ') || 'nineteen (nothing)';
}

function pegDetail(p) {
  const parts = [];
  if (p.fifteen) parts.push('fifteen');
  if (p.thirtyOne) parts.push('thirty-one');
  if (p.pairs === 2) parts.push('pair');
  if (p.pairs === 6) parts.push('pair royal');
  if (p.pairs === 12) parts.push('double pair royal');
  if (p.run) parts.push(`run of ${p.run}`);
  return parts.join(' + ');
}

function describe(g, e) {
  const who = seatName(g, e.seat);
  const cards = (e.cards || []).map(cardName).join(' ');
  const s = who === 'You' ? '' : 's';
  switch (e.kind) {
    case 'EVENT_KIND_CUT_FOR_DEAL': return `Cut for deal: ${seatName(g, 0)} ${cardName(e.cards[0])}, ${seatName(g, 1)} ${cardName(e.cards[1])}`;
    case 'EVENT_KIND_DEAL': return `New hand — ${who} deal${s}`;
    case 'EVENT_KIND_DISCARD': return `${who} discard${s} to the crib`;
    case 'EVENT_KIND_STARTER': return `Starter: ${cardName(e.card)}` + (e.points ? ` — ${who} score${s} 2 for his heels` : '');
    case 'EVENT_KIND_PLAY': return `${who} play${s} ${cardName(e.card)} (${e.count})` + (e.points ? ` — ${pegDetail(e.peg || {})}` : '');
    case 'EVENT_KIND_GO': return `${who} say${s} go`;
    case 'EVENT_KIND_GO_POINT': return `${who} score${s} 1 for the go`;
    case 'EVENT_KIND_LAST_CARD': return `${who} score${s} 1 for last card`;
    case 'EVENT_KIND_COUNT_RESET': return 'Count goes back to 0';
    case 'EVENT_KIND_SHOW_HAND': return `${who === 'You' ? 'Your' : who + "'s"} hand ${cards} + ${cardName(e.card)}: ${breakdown(e.show || {})}`;
    case 'EVENT_KIND_SHOW_CRIB': return `${who === 'You' ? 'Your' : who + "'s"} crib ${cards} + ${cardName(e.card)}: ${breakdown(e.show || {})}`;
    case 'EVENT_KIND_GAME_OVER': return `${who} win${s} ${e.scores[e.seat]}–${e.scores[1 - e.seat]}` + (e.skunk === 2 ? ' — double skunk!' : e.skunk === 1 ? ' — skunk!' : '');
    case 'EVENT_KIND_RESIGN': return `${who} resign${s}`;
  }
  return e.kind;
}

function pushLog(id, text, cls, points) {
  if (!state.logs.has(id)) state.logs.set(id, []);
  state.logs.get(id).push({ text, cls, points });
}

function renderLog() {
  const log = $('log');
  const lines = state.logs.get(state.selected) || [];
  const shown = state.logShown;
  if (shown.id !== state.selected || shown.n > lines.length) {
    log.replaceChildren();
    shown.id = state.selected;
    shown.n = Math.max(0, lines.length - 300);
  }
  const animate = !prefs.rapid && log.childElementCount > 0;
  for (const l of lines.slice(shown.n)) {
    const li = document.createElement('li');
    li.innerHTML = esc(l.text) + (l.points ? ` <span class="pts">+${l.points}</span>` : '');
    li.className = (l.cls || '') + (animate ? ' enter' : '');
    log.appendChild(li);
  }
  shown.n = lines.length;
  while (log.childElementCount > 300) log.firstChild.remove();
  log.scrollTo({ top: log.scrollHeight, behavior: animate ? 'smooth' : 'auto' });
}

function gameOverText(g) {
  if (g.winner < 0) return 'Game cancelled.';
  const won = g.winner === g.seat;
  return `${won ? 'You won' : 'You lost'} ${g.scores[g.seat]}–${g.scores[1 - g.seat]}${g.resigned ? ' (resignation)' : ''}.`;
}

function updateTitle() {
  const turn = [...state.games.values()].some(u => u.game.yourTurn && u.game.status === 'GAME_STATUS_ACTIVE');
  document.title = turn ? '● Your turn — Cribbage' : 'Cribbage';
}

// ---------------------------------------------------------------------------
// Lobby
// ---------------------------------------------------------------------------

function opponent(g) {
  const p = (g.players || [])[1 - g.seat];
  if (!p || (!p.nickname && !p.bot)) return 'waiting for opponent';
  return p.nickname;
}

function gameItem(g, onClick, tag, tagCls) {
  const li = document.createElement('li');
  const score = g.scores && g.status !== 'GAME_STATUS_WAITING' ? `${g.scores[g.seat]}–${g.scores[1 - g.seat]}` : '';
  li.innerHTML = `<span class="name">vs ${esc(opponent(g))}</span><span class="score">${score}</span>` +
    (tag ? `<span class="tag ${tagCls || ''}">${esc(tag)}</span>` : '');
  li.onclick = onClick;
  if (g.gameId === state.selected) li.classList.add('selected');
  return li;
}

function fillList(ul, items, empty) {
  ul.replaceChildren(...items);
  if (!items.length) {
    const li = document.createElement('li');
    li.className = 'empty-note';
    li.textContent = empty;
    ul.appendChild(li);
  }
}

function renderLobby() {
  const mine = [];
  for (const [id, u] of state.games) {
    const g = u.game;
    if (g.status === 'GAME_STATUS_FINISHED') continue;
    const waiting = g.status === 'GAME_STATUS_WAITING';
    mine.push(gameItem(g, () => select(id), waiting ? 'open' : g.yourTurn ? 'your turn' : '', g.yourTurn ? 'turn' : ''));
  }
  fillList($('my-games'), mine, 'No games in progress');
  fillList($('open-games'), state.open.map(g => {
    const li = document.createElement('li');
    li.innerHTML = `<span class="name">${esc(g.players[0].nickname)}</span><span class="tag turn">join</span>`;
    li.onclick = () => conn.send({ joinGame: { gameId: g.gameId } });
    return li;
  }), 'Nobody is waiting');
  fillList($('recent-games'), state.recent.slice(0, 10).map(g => {
    const res = g.winner === g.seat ? 'won' : g.winner < 0 ? 'cancelled' : 'lost';
    return gameItem(g, () => { if (state.games.has(g.gameId)) select(g.gameId); }, res, res);
  }), 'None yet');
}

function select(id) {
  if (state.selected !== id) skipAnimation();
  state.selected = id;
  state.picked.clear();
  setDrawer(false);
  renderLobby();
  renderTable();
}

function setDrawer(open) {
  document.body.classList.toggle('drawer-open', open);
  $('scrim').hidden = !open || !document.body.classList.contains('has-table');
  $('menu').setAttribute('aria-expanded', String(open));
}

// ---------------------------------------------------------------------------
// Table
// ---------------------------------------------------------------------------

function cardEl(c, opts = {}) {
  const el = document.createElement(opts.button ? 'button' : 'div');
  el.className = 'card';
  if (c === undefined || c === null) {
    el.classList.add('back');
    el.setAttribute('aria-label', 'face-down card');
  } else {
    const r = RANKS[c % 13], s = SUITS[Math.floor(c / 13)];
    el.innerHTML = `<span class="ix">${r}<i>${s}</i></span><span class="pip">${s}</span>`;
    el.setAttribute('aria-label', cardName(c));
    if (isRed(c)) el.classList.add('red');
  }
  for (const cls of opts.classes || []) if (cls) el.classList.add(cls);
  if (opts.delay) el.style.animationDelay = `${opts.delay}ms`;
  return el;
}

const BOT_LABELS = { nn: 'Neural network bot', heuristic: 'Heuristic bot', random: 'Random bot' };

function plate(el, v, seat, isMe) {
  const g = v.g;
  const p = (g.players || [])[seat] || {};
  const name = isMe ? 'You' : (p.nickname || 'Waiting…');
  const initial = (isMe ? (state.me && state.me.nickname) || 'Y' : p.nickname || '?').trim().charAt(0).toUpperCase();
  const online = isMe || p.bot || p.connected;
  const sub = p.bot ? BOT_LABELS[p.bot] || 'Computer' : isMe ? (state.me ? state.me.nickname : '') : online ? 'online' : 'away';
  el.className = `plate ${isMe ? 'me' : 'opp'}`;
  el.innerHTML = `<div class="avatar">${esc(initial)}<span class="conn ${online ? 'on' : ''}"></span></div>` +
    `<div><div class="nm">${esc(name)}${v.dealer === seat ? '<span class="dealer" title="Dealer: the crib is theirs">DEALER</span>' : ''}</div>` +
    `<div class="sub">${esc(sub)}</div></div><div class="pts${v.bump.has(seat) ? ' bump' : ''}">${v.scores[seat]}</div>`;
}

function renderTable(animate = false) {
  const has = !!(state.me && state.selected && state.games.has(state.selected));
  document.body.classList.toggle('has-table', has);
  $('table').hidden = !has;
  $('empty').hidden = has || !state.me;
  if (!has) { setDrawer(false); return; }
  const v = view(state.selected);
  const g = v.g;
  const me = g.seat < 0 ? 0 : g.seat;
  const opp = 1 - me;
  const oppName = opponent(g);

  $('table-title').textContent = `vs ${oppName}`;
  const finished = g.status === 'GAME_STATUS_FINISHED';
  $('resign').textContent = g.status === 'GAME_STATUS_WAITING' ? 'Cancel game' : 'Resign';
  $('resign').disabled = finished;
  $('phase').textContent = finished ? 'Game over' : g.status === 'GAME_STATUS_WAITING' ? 'Waiting' :
    v.handNumber ? `Hand ${v.handNumber} · ${v.phase === 'SHOW' ? 'The show' : v.phase === 'PHASE_PEGGING' ? 'Pegging' : 'Discard'}` : '';

  Board.show(state.selected, 'You', oppName, v, animate);
  plate($('me-plate'), v, me, true);
  plate($('opp-plate'), v, opp, false);

  // Opponent's hand: backs, or their cards once shown.
  const oh = $('opp-hand');
  oh.replaceChildren();
  if (v.oppReveal) v.oppReveal.forEach(c => oh.appendChild(cardEl(c)));
  else for (let i = 0; i < v.oppHandSize; i++) oh.appendChild(cardEl(null, { classes: [animate && v.freshOpp ? 'deal-in-opp' : ''], delay: animate && v.freshOpp ? i * 60 : 0 }));

  // Deck with the starter on top.
  const deck = $('deck');
  deck.replaceChildren(cardEl(null), cardEl(null));
  deck.appendChild(v.starter === null ? cardEl(null) : cardEl(v.starter, { classes: ['starter-card', animate && v.freshStarter ? 'flip-in' : ''] }));

  // Crib, on the dealer's side, in the dealer's color.
  const cz = $('crib-zone');
  const cribMine = v.dealer === me;
  cz.classList.toggle('mine', v.dealer >= 0 && cribMine);
  cz.classList.toggle('theirs', v.dealer >= 0 && !cribMine);
  $('crib-owner').textContent = v.dealer < 0 ? '—' : cribMine ? 'Your crib' : `${oppName}'s crib`;
  const crib = $('crib');
  crib.replaceChildren();
  crib.classList.toggle('open', !!v.cribOpen);
  if (v.cribOpen) {
    v.cribOpen.forEach(c => crib.appendChild(cardEl(c)));
  } else {
    const cards = [...v.myDiscards];
    if (v.oppDiscarded) cards.unshift(null, null);
    cards.forEach((c, i) => {
      const el = cardEl(c, { classes: [animate && v.freshCrib && i >= cards.length - 2 ? 'deal-in' : ''] });
      el.style.left = `calc(var(--cw) * ${i * .3})`;
      el.style.transform = `rotate(${(i - 1.5) * 4}deg)`;
      crib.appendChild(el);
    });
  }

  // Middle: pegging, or the show.
  const showing = v.shows.length > 0 && (v.phase === 'SHOW' || v.phase === 'PHASE_DISCARD' || finished);
  $('show').hidden = !showing;
  $('sequence').hidden = showing;
  $('earlier').hidden = showing;
  $('count').textContent = showing || v.phase !== 'PHASE_PEGGING' ? '' : v.count;
  $('count').classList.toggle('hot', animate && v.hot);
  $('play-title').textContent = showing ? (v.phase === 'SHOW' ? 'The show' : 'Last hand') : 'Pegging';
  const seq = $('sequence'), earlier = $('earlier');
  seq.replaceChildren();
  earlier.replaceChildren();
  v.played.forEach((p, i) => {
    const el = cardEl(p.card, { classes: [p.seat === me ? 'by-me' : 'by-opp', animate && v.fresh.has(p.card) ? (p.seat === me ? 'deal-in' : 'deal-in-opp') : ''] });
    (i >= v.seqStart ? seq : earlier).appendChild(el);
  });
  $('show').classList.toggle('compact', showing && v.phase !== 'SHOW');
  if (showing) renderShow(v, me, oppName, animate);

  // Your hand.
  const hand = $('hand');
  hand.replaceChildren();
  const discarding = v.phase === 'PHASE_DISCARD' && v.mustAct && !v.busy;
  const playable = new Set(v.legal.filter(a => a.kind === 'ACTION_KIND_PLAY').map(a => a.cards[0]));
  const myPlay = v.phase === 'PHASE_PEGGING' && v.mustAct && !v.busy;
  [...v.hand].sort((a, b) => a % 13 - b % 13 || a - b).forEach((c, i) => {
    const fresh = animate && v.fresh.has(c) && !v.played.some(p => p.card === c);
    const el = cardEl(c, {
      button: true, delay: fresh ? i * 70 : 0,
      classes: [state.picked.has(c) ? 'selected' : '', fresh ? 'deal-in' : '', myPlay && playable.has(c) ? 'playable' : '', myPlay && !playable.has(c) ? 'dim' : ''],
    });
    if (discarding) {
      el.onclick = () => {
        if (state.picked.has(c)) state.picked.delete(c);
        else if (state.picked.size < 2) state.picked.add(c);
        sfx.card();
        renderTable();
      };
    } else if (myPlay && playable.has(c)) {
      el.onclick = () => act({ kind: 'ACTION_KIND_PLAY', cards: [c] });
    } else {
      el.disabled = true;
    }
    hand.appendChild(el);
  });
  $('discard').hidden = !discarding;
  $('discard').disabled = state.picked.size !== 2;

  let prompt = '', go = false;
  if (finished) prompt = gameOverText(g);
  else if (g.status === 'GAME_STATUS_WAITING') prompt = 'Waiting for someone to join…';
  else if (v.busy) prompt = '';
  else if (discarding) { prompt = narrow() ? `Pick 2 for ${cribMine ? 'your' : 'their'} crib` : `Choose two cards for ${cribMine ? 'your' : 'their'} crib`; go = true; }
  else if (v.phase === 'PHASE_DISCARD') prompt = 'Waiting for opponent to discard…';
  else if (myPlay) { prompt = narrow() ? 'Your turn' : 'Your turn — play a card'; go = true; }
  else if (v.phase === 'PHASE_PEGGING') prompt = 'Opponent to play…';
  $('prompt').textContent = prompt;
  $('prompt').classList.toggle('go', go);

  renderBanner(v, g, animate);
  renderLog();

  v.fresh.clear();
  v.bump.clear();
  v.freshOpp = v.freshStarter = v.freshCrib = v.hot = false;
  for (const s of v.shows) s.fresh = false;
}

function renderShow(v, me, oppName, animate) {
  const box = $('show');
  box.replaceChildren();
  for (const s of v.shows) {
    const mine = s.seat === me;
    const row = document.createElement('div');
    row.className = `show-row ${mine ? 'me' : 'opp'}${animate && s.fresh ? ' enter' : ''}`;
    const who = `${mine ? 'Your' : `${esc(oppName)}'s`} ${s.crib ? 'crib' : 'hand'}`;
    row.innerHTML = `<div class="who">${who}</div><div class="tot">${s.points}</div><div class="mini"></div><div class="why">${esc(breakdown(s.show))}</div>`;
    const mini = row.querySelector('.mini');
    s.cards.forEach(c => mini.appendChild(cardEl(c)));
    const plus = document.createElement('span');
    plus.className = 'plus';
    plus.textContent = '+';
    mini.appendChild(plus);
    mini.appendChild(cardEl(s.starter, { classes: ['starter-card'] }));
    box.appendChild(row);
  }
}

function renderBanner(v, g, animate) {
  const b = $('banner');
  const over = v.gameOver && !v.bannerClosed && !v.busy;
  if (!over) { b.hidden = true; b.dataset.game = ''; return; }
  if (!b.hidden && b.dataset.game === g.gameId) return;
  b.dataset.game = g.gameId;
  const me = g.seat < 0 ? 0 : g.seat;
  const won = v.gameOver.winner === me;
  const loser = v.scores[1 - v.gameOver.winner];
  const skunk = v.gameOver.resigned ? 0 : loser < 61 ? 2 : loser < 91 ? 1 : 0;
  const bot = ((g.players || [])[1 - me] || {}).bot;
  b.innerHTML = `<div class="banner-card"><h3>${won ? 'You win!' : 'You lose'}</h3>` +
    `<div class="final">${v.scores[me]} – ${v.scores[1 - me]}</div>` +
    (skunk ? `<div class="skunk-note">${skunk === 2 ? 'Double skunk!' : 'Skunk!'}</div>` : '') +
    (v.gameOver.resigned ? `<div class="muted">${won ? 'Your opponent resigned.' : 'You resigned.'}</div>` : '') +
    `<div class="row">${bot ? '<button class="btn btn-primary" data-act="rematch">Play again</button>' : ''}<button class="btn" data-act="close">View table</button></div></div>`;
  b.hidden = false;
  b.querySelector('[data-act=close]').onclick = () => { v.bannerClosed = true; b.hidden = true; };
  const re = b.querySelector('[data-act=rematch]');
  if (re) re.onclick = () => { v.bannerClosed = true; b.hidden = true; conn.send({ createGame: { bot } }); };
  if (animate && v.gameOver.fresh) {
    (won ? sfx.win : sfx.lose)();
    if (won) confetti(b);
  }
  v.gameOver.fresh = false;
}

function confetti(parent) {
  const colors = ['var(--me)', 'var(--opp)', 'var(--accent)', '#fff', '#60a5fa', '#facc15'];
  for (let i = 0; i < 70; i++) {
    const p = document.createElement('span');
    p.className = 'confetti';
    p.style.left = `${Math.random() * 100}%`;
    p.style.background = colors[i % colors.length];
    p.style.animationDuration = `${1.8 + Math.random() * 1.6}s`;
    p.style.animationDelay = `${Math.random() * .5}s`;
    p.style.transform = `rotate(${Math.random() * 360}deg)`;
    parent.appendChild(p);
    setTimeout(() => p.remove(), 4200);
  }
}

function act(action) {
  conn.send({ act: { gameId: state.selected, action } });
  state.picked.clear();
  const v = view(state.selected);
  if (v) { v.legal = []; v.mustAct = false; renderTable(); }
}

// ---------------------------------------------------------------------------
// UI wiring
// ---------------------------------------------------------------------------

$('login-form').onsubmit = ev => {
  ev.preventDefault();
  const f = new FormData(ev.target);
  const action = ev.submitter ? ev.submitter.value : 'login';
  const creds = { username: f.get('username'), password: f.get('password') };
  if (!conn.dc) { $('login-error').textContent = 'Not connected yet…'; return; }
  if (action === 'register') conn.send({ register: Object.assign(creds, { nickname: f.get('nickname') }) });
  else conn.send({ login: creds });
};

$('logout').onclick = () => {
  localStorage.removeItem('token');
  location.reload();
};

for (const b of document.querySelectorAll('[data-create]')) {
  b.onclick = () => conn.send({ createGame: { bot: b.dataset.create } });
}
// Show buttons only for bots this server offers (nn needs a model).
fetch('api/bots').then(r => r.json()).then(names => {
  for (const b of document.querySelectorAll('[data-create]')) b.hidden = !names.includes(b.dataset.create);
}).catch(() => { /* keep the defaults */ });
$('create-open').onclick = () => conn.send({ createGame: { bot: '' } });
$('find-match').onclick = () => conn.send({ findMatch: {} });
$('refresh').onclick = () => conn.send({ listGames: {} });
$('discard').onclick = () => {
  if (state.picked.size === 2) act({ kind: 'ACTION_KIND_DISCARD', cards: [...state.picked] });
};
$('resign').onclick = () => {
  const u = state.games.get(state.selected);
  if (!u) return;
  const what = u.game.status === 'GAME_STATUS_WAITING' ? 'Cancel this open game?' : 'Resign this game?';
  if (confirm(what)) conn.send({ resign: { gameId: state.selected } });
};
// A tap on the table while events are replaying skips to the end.
document.querySelector('.felt').addEventListener('click', () => skipAnimation());

$('menu').onclick = () => setDrawer(!document.body.classList.contains('drawer-open'));
$('scrim').onclick = () => setDrawer(false);
$('settings-btn').onclick = e => {
  e.stopPropagation();
  const open = $('settings').hidden;
  $('settings').hidden = !open;
  $('settings-btn').setAttribute('aria-expanded', String(open));
};
document.addEventListener('click', e => {
  if (!$('settings').hidden && !$('settings').contains(e.target)) {
    $('settings').hidden = true;
    $('settings-btn').setAttribute('aria-expanded', 'false');
  }
});
document.addEventListener('keydown', e => {
  if (e.key === 'Escape') { $('settings').hidden = true; setDrawer(false); }
});
$('theme').onchange = () => { prefs.theme = $('theme').value; savePrefs(); applyPrefs(); };
$('rapid').onchange = () => {
  prefs.rapid = $('rapid').checked;
  savePrefs();
  if (prefs.rapid) skipAnimation();
};
$('mute').onclick = () => {
  prefs.muted = !prefs.muted;
  savePrefs();
  applyPrefs();
  if (!prefs.muted) { sfx.unlock(); sfx.card(); }
};
// Browsers only allow audio after a user gesture.
for (const ev of ['pointerdown', 'keydown']) document.addEventListener(ev, sfx.unlock, { passive: true });
document.addEventListener('visibilitychange', () => { if (document.hidden) skipAnimation(); });

if (narrow()) $('log-panel').open = false;
applyPrefs();
Board.build($('board'));
conn.connect();
