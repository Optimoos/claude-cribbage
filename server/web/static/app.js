// Cribbage HTML test client. Talks to the server over a WebRTC data channel
// using the protobuf JSON mapping (see proto/cribbage/v1/cribbage.proto).
'use strict';

const RANKS = ['A', '2', '3', '4', '5', '6', '7', '8', '9', '10', 'J', 'Q', 'K'];
const SUITS = ['♣', '♦', '♥', '♠'];
const cardName = c => RANKS[c % 13] + SUITS[Math.floor(c / 13)];
const isRed = c => [1, 2].includes(Math.floor(c / 13));
const $ = id => document.getElementById(id);

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
// State
// ---------------------------------------------------------------------------

const state = {
  me: null,           // AuthOk
  games: new Map(),   // gameId -> latest GameUpdate
  logs: new Map(),    // gameId -> [{text, cls}]
  open: [],
  recent: [],
  selected: null,
  picked: new Set(),  // cards selected for discard
};

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
  for (const id of ['login', 'lobby', 'table']) $(id).hidden = !ids.includes(id);
}

function notice(text) {
  $('notice').textContent = text || '';
}

// ---------------------------------------------------------------------------
// Messages from the server
// ---------------------------------------------------------------------------

function onMessage(m) {
  if (m.authOk) {
    state.me = m.authOk;
    localStorage.setItem('token', m.authOk.token);
    $('who').textContent = `${m.authOk.nickname} (${m.authOk.username})`;
    $('logout').hidden = false;
    $('login-error').textContent = '';
    show('lobby', 'table');
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
  state.games.set(id, u);
  addEvents(u);
  if (u.game.status === 'GAME_STATUS_FINISHED') {
    notice(gameOverText(u.game));
    conn.send({ listGames: {} });
  }
  if (!state.selected || (!prev && u.game.status !== 'GAME_STATUS_FINISHED' && !u.full)) {
    select(id);
  }
  if (u.game.status === 'GAME_STATUS_ACTIVE' && $('notice').textContent.startsWith('Waiting for an opponent')) notice('');
  renderLobby();
  if (id === state.selected) renderTable();
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
  return parts.join(', ') || 'nineteen (zero)';
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
  switch (e.kind) {
    case 'EVENT_KIND_CUT_FOR_DEAL': return `Cut for deal: ${seatName(g, 0)} ${cardName(e.cards[0])}, ${seatName(g, 1)} ${cardName(e.cards[1])}`;
    case 'EVENT_KIND_DEAL': return `— New hand: ${who} deal${who === 'You' ? '' : 's'} —`;
    case 'EVENT_KIND_DISCARD': return `${who} discarded to the crib`;
    case 'EVENT_KIND_STARTER': return `Starter: ${cardName(e.card)}` + (e.points ? ` — ${who} score${who === 'You' ? '' : 's'} 2 for his heels` : '');
    case 'EVENT_KIND_PLAY': return `${who} played ${cardName(e.card)} (${e.count})` + (e.points ? ` +${e.points} ${pegDetail(e.peg || {})}` : '');
    case 'EVENT_KIND_GO': return `${who} said go`;
    case 'EVENT_KIND_GO_POINT': return `${who} +1 for go`;
    case 'EVENT_KIND_LAST_CARD': return `${who} +1 for last card`;
    case 'EVENT_KIND_COUNT_RESET': return 'Count reset';
    case 'EVENT_KIND_SHOW_HAND': return `${who} show ${cards} + ${cardName(e.card)}: ${e.points} (${breakdown(e.show || {})})`;
    case 'EVENT_KIND_SHOW_CRIB': return `${who === 'You' ? 'Your' : who + "'s"} crib ${cards} + ${cardName(e.card)}: ${e.points} (${breakdown(e.show || {})})`;
    case 'EVENT_KIND_GAME_OVER': return `${who} win${who === 'You' ? '' : 's'} ${e.scores[e.seat]}–${e.scores[1 - e.seat]}` + (e.skunk === 2 ? ' (double skunk!)' : e.skunk === 1 ? ' (skunk!)' : '');
    case 'EVENT_KIND_RESIGN': return `${who} resigned`;
  }
  return e.kind;
}

function addEvents(u) {
  const id = u.game.gameId;
  if (!state.logs.has(id)) state.logs.set(id, []);
  const log = state.logs.get(id);
  for (const e of u.events || []) {
    const cls = e.kind === 'EVENT_KIND_DEAL' ? 'hand' : e.seat === u.game.seat ? 'me' : '';
    log.push({ text: describe(u.game, e), cls });
  }
}

function gameOverText(g) {
  if (g.winner < 0) return 'Game cancelled.';
  const won = g.winner === g.seat;
  return `${won ? 'You won' : 'You lost'} ${g.scores[g.seat]}–${g.scores[1 - g.seat]}${g.resigned ? ' (resignation)' : ''}.`;
}

// ---------------------------------------------------------------------------
// Rendering
// ---------------------------------------------------------------------------

function opponent(g) {
  const p = (g.players || [])[1 - g.seat];
  if (!p || (!p.nickname && !p.bot)) return 'waiting for opponent';
  return p.nickname;
}

function gameItem(g, onClick, tag) {
  const li = document.createElement('li');
  const turn = g.yourTurn ? '<span class="turn">●</span>' : '';
  li.innerHTML = `${turn}<span>vs ${escapeHTML(opponent(g))}</span><span>${g.scores ? g.scores[g.seat] + '–' + g.scores[1 - g.seat] : ''}</span><span class="tag">${tag || ''}</span>`;
  li.onclick = onClick;
  if (g.gameId === state.selected) li.classList.add('selected');
  return li;
}

function renderLobby() {
  const mine = $('my-games');
  mine.replaceChildren();
  for (const [id, u] of state.games) {
    const g = u.game;
    if (g.status === 'GAME_STATUS_FINISHED') continue;
    const tag = g.status === 'GAME_STATUS_WAITING' ? 'open' : '';
    mine.appendChild(gameItem(g, () => select(id), tag));
  }
  const open = $('open-games');
  open.replaceChildren();
  for (const g of state.open) {
    const li = document.createElement('li');
    li.innerHTML = `<span>${escapeHTML(g.players[0].nickname)}</span><span class="tag">join</span>`;
    li.onclick = () => conn.send({ joinGame: { gameId: g.gameId } });
    open.appendChild(li);
  }
  const recent = $('recent-games');
  recent.replaceChildren();
  for (const g of state.recent.slice(0, 10)) {
    recent.appendChild(gameItem(g, () => {
      if (state.games.has(g.gameId)) select(g.gameId);
    }, g.winner === g.seat ? 'won' : g.winner < 0 ? 'cancelled' : 'lost'));
  }
}

function select(id) {
  state.selected = id;
  state.picked.clear();
  renderLobby();
  renderTable();
}

function cardEl(c, opts = {}) {
  const el = document.createElement(opts.button ? 'button' : 'div');
  el.className = 'card';
  if (c === undefined || c === null) {
    el.classList.add('back');
  } else {
    el.textContent = cardName(c);
    if (isRed(c)) el.classList.add('red');
  }
  for (const cls of opts.classes || []) el.classList.add(cls);
  return el;
}

function escapeHTML(s) {
  return String(s).replace(/[&<>"']/g, ch => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]));
}

function renderTable() {
  const u = state.games.get(state.selected);
  $('table').hidden = !u || !state.me;
  if (!u || !state.me) return;
  const g = u.game;
  const o = u.observation;
  const me = g.seat;
  $('table-title').textContent = `vs ${opponent(g)}`;
  $('resign').textContent = g.status === 'GAME_STATUS_WAITING' ? 'Cancel' : 'Resign';
  $('resign').disabled = g.status === 'GAME_STATUS_FINISHED';

  // Scoreboard
  const sb = $('scoreboard');
  sb.replaceChildren();
  for (const seat of [me, 1 - me]) {
    const p = (g.players || [])[seat] || {};
    const score = (g.scores || [0, 0])[seat];
    const div = document.createElement('div');
    div.className = 'player' + (seat === me ? '' : ' opp');
    const dealer = o && o.dealer === seat ? ' (dealer)' : '';
    div.innerHTML = `<span><span class="dot ${p.connected ? 'on' : ''}"></span>${seat === me ? 'You' : escapeHTML(p.nickname || '—')}${dealer}</span>` +
      `<b>${score}</b><div class="bar"><div style="width:${Math.min(score, 121) / 121 * 100}%"></div></div>`;
    sb.appendChild(div);
  }

  const hand = $('hand');
  const oppHand = $('opp-hand');
  for (const el of ['hand', 'opp-hand', 'starter', 'crib', 'sequence', 'earlier']) $(el).replaceChildren();
  $('discard').hidden = true;

  if (!o) {
    $('prompt').textContent = g.status === 'GAME_STATUS_WAITING' ? 'Waiting for someone to join…' : '';
    renderLog();
    return;
  }

  for (let i = 0; i < o.oppHandSize; i++) oppHand.appendChild(cardEl(null));
  if (o.starter !== undefined && o.starter !== null) $('starter').appendChild(cardEl(o.starter));
  else $('starter').appendChild(cardEl(null));

  // Crib: our own discards face up, the rest face down.
  $('crib-owner').textContent = o.dealer === me ? 'yours' : 'theirs';
  const crib = $('crib');
  for (const c of o.discards || []) crib.appendChild(cardEl(c, { classes: ['mine'] }));
  const cribCount = (o.discarded ? 2 : 0) + (o.oppDiscarded ? 2 : 0);
  for (let i = (o.discards || []).length; i < cribCount; i++) crib.appendChild(cardEl(null));

  $('count').textContent = o.count;
  const played = o.played || [];
  played.forEach((p, i) => {
    const target = i >= o.seqStart ? $('sequence') : $('earlier');
    target.appendChild(cardEl(p.card, { classes: [p.seat === me ? 'mine' : 'played-by-opp'] }));
  });

  // Hand and actions
  const legal = u.legal || [];
  const discarding = o.phase === 'PHASE_DISCARD' && o.mustAct;
  const playable = new Set(legal.filter(a => a.kind === 'ACTION_KIND_PLAY').map(a => a.cards[0]));
  for (const c of o.hand || []) {
    const el = cardEl(c, { button: true, classes: state.picked.has(c) ? ['selected'] : [] });
    if (discarding) {
      el.onclick = () => {
        if (state.picked.has(c)) state.picked.delete(c);
        else if (state.picked.size < 2) state.picked.add(c);
        renderTable();
      };
    } else if (playable.has(c)) {
      el.onclick = () => act({ kind: 'ACTION_KIND_PLAY', cards: [c] });
    } else {
      el.disabled = true;
    }
    hand.appendChild(el);
  }
  if (discarding) {
    $('discard').hidden = false;
    $('discard').disabled = state.picked.size !== 2;
  }

  let prompt = '';
  if (g.status === 'GAME_STATUS_FINISHED') prompt = gameOverText(g);
  else if (discarding) prompt = `Choose two cards for ${o.dealer === me ? 'your' : 'their'} crib`;
  else if (o.phase === 'PHASE_DISCARD') prompt = 'Waiting for opponent to discard…';
  else if (o.mustAct) prompt = 'Your turn — play a card';
  else prompt = 'Opponent to play…';
  $('prompt').textContent = prompt;
  renderLog();
}

function renderLog() {
  const log = $('log');
  log.replaceChildren();
  const lines = state.logs.get(state.selected) || [];
  for (const l of lines.slice(-200)) {
    const li = document.createElement('li');
    li.textContent = l.text;
    if (l.cls) li.className = l.cls;
    log.appendChild(li);
  }
  log.scrollTop = log.scrollHeight;
}

function act(action) {
  conn.send({ act: { gameId: state.selected, action } });
  state.picked.clear();
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

conn.connect();
