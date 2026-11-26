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

// The protocol version this client speaks (ProtocolVersion in
// cribbage.proto). The server refuses others with UPGRADE_REQUIRED.
const PROTOCOL_VERSION = 2;

// The game server: the one this page came from, unless the page is hosted
// apart from it (on Cloudflare Pages: see deploy/README.md), when a meta
// tag names it.
const SERVER = new URL(document.querySelector('meta[name=cribbage-server]')?.content || '.', location.href);
const serverURL = path => new URL(path, SERVER).href;

// Requests that do the same thing however often they are sent (asking
// for a code sends the same code again), so they can be sent again after
// a connection died without answering them.
const RETRYABLE = ['requestSignIn', 'listGames', 'getHistory', 'syncGame', 'ping'];

class Connection {
  constructor(onMessage, onOpen, onClose) {
    this.onMessage = onMessage;
    this.onOpen = onOpen;
    this.onClose = onClose;
    this.nextId = 0;
    this.dc = null;
    this.retry = 500;
    // Requests made while the connection is down (or not yet signed back
    // in) wait here and go once it is ready, so a click during a quick
    // reconnect just works. One that waits too long fails like a reply.
    this.ready = false;
    this.queue = [];
    // Requests waiting for their reply (sent with track), by id: if the
    // connection dies first, safe ones go again and the rest fail with a
    // reply.
    this.inflight = new Map();
  }

  async connect() {
    let cfg = { iceServers: [] };
    // A stopped server does not refuse connections, it ignores them, so
    // do not wait for the browser to give up.
    try { cfg = await (await fetch(serverURL('api/config'), { signal: AbortSignal.timeout(5000) })).json(); } catch (e) { /* use defaults */ }
    setReportLinks(cfg.reportUrl);
    state.passkeysOffered = cfg.passkeys === true;
    $('passkey-signin').hidden = !passkeysUsable();
    const pc = new RTCPeerConnection({ iceServers: cfg.iceServers });
    const dc = pc.createDataChannel('cribbage', { ordered: true });
    const ws = new WebSocket(serverURL('rtc').replace(/^http/, 'ws'));
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
      this.ready = false;
      this.orphans();
      if (this.stopped) return; // out of date: stay down, and say so
      this.onClose();
      setTimeout(() => this.connect(), this.retry);
      this.retry = Math.min(this.retry * 2, 2000);
    };

    pc.onicecandidate = e => {
      if (!e.candidate) return;
      const m = JSON.stringify({ type: 'candidate', candidate: e.candidate.toJSON() });
      if (described && ws.readyState === WebSocket.OPEN) ws.send(m); else localPending.push(m);
    };
    // "failed" takes ~15s to follow "disconnected", which itself comes ~5s
    // after the server goes away (e.g. a restart), so give up on a brief
    // disconnect rather than waiting it out.
    pc.onconnectionstatechange = () => {
      if (['failed', 'closed'].includes(pc.connectionState)) fail();
      else if (pc.connectionState === 'disconnected') {
        setTimeout(() => { if (pc.connectionState === 'disconnected') fail(); }, 1500);
      }
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
    dc.onmessage = e => {
      const m = JSON.parse(e.data);
      if (m.requestId) this.inflight.delete(m.requestId);
      this.onMessage(m);
    };
    dc.onclose = fail;
  }

  // send sends a request now if the connection is ready, or once it is,
  // and returns its id (0 only if this page is out of date). track: the
  // caller waits for the reply (request does).
  send(body, track = false) {
    if (this.stopped) return 0;
    const m = this.message(body);
    if (this.ready && this.open()) {
      this.transmit(m, track);
      return m.requestId;
    }
    this.enqueue(m, track);
    return m.requestId;
  }

  enqueue(m, track) {
    const item = { m, track };
    item.timer = setTimeout(() => {
      this.queue = this.queue.filter(i => i !== item);
      this.onMessage({ requestId: m.requestId, error: { code: 'ERROR_CODE_UNAVAILABLE', message: 'Could not reach the server. Try again in a moment.' } });
    }, 15000);
    this.queue.push(item);
  }

  transmit(m, track) {
    this.dc.send(JSON.stringify(m));
    if (track) this.inflight.set(m.requestId, m);
  }

  // orphans deals with requests the dead connection never answered: ones
  // that are safe to repeat wait for the next connection, and the others
  // fail, since they may or may not have happened.
  orphans() {
    for (const m of this.inflight.values()) {
      if (RETRYABLE.some(k => k in m)) this.enqueue(m, true);
      else this.onMessage({ requestId: m.requestId, error: { code: 'ERROR_CODE_UNAVAILABLE', message: 'The connection dropped before the server answered. Check, and try again if it did not happen.' } });
    }
    this.inflight.clear();
  }

  // sendNow sends on an open connection that is not ready yet (resuming
  // the session that readies it), or returns 0.
  sendNow(body) {
    if (!this.open()) return 0;
    const m = this.message(body);
    this.transmit(m, false);
    return m.requestId;
  }

  // markReady sends the requests that waited for the connection.
  markReady() {
    if (!this.open()) return;
    this.ready = true;
    for (const item of this.queue.splice(0)) {
      clearTimeout(item.timer);
      this.transmit(item.m, item.track);
    }
  }

  open() { return this.dc && this.dc.readyState === 'open'; }

  message(body) {
    return Object.assign({ requestId: ++this.nextId, protocolVersion: PROTOCOL_VERSION }, body);
  }
}

// ---------------------------------------------------------------------------
// Preferences, kept in localStorage between sessions.
// ---------------------------------------------------------------------------

const THEMES = { felt: '#123826', parchment: '#2f6d41', midnight: '#0b0e14' };
const prefs = Object.assign(
  { theme: 'felt', board: 'serpentine', rapid: matchMedia('(prefers-reduced-motion: reduce)').matches, muted: false },
  (() => { try { return JSON.parse(localStorage.getItem('prefs')) || {}; } catch (e) { return {}; } })(),
);
if (!(prefs.theme in THEMES)) prefs.theme = 'felt';
if (!['serpentine', 'long'].includes(prefs.board)) prefs.board = 'serpentine';

function savePrefs() {
  try { localStorage.setItem('prefs', JSON.stringify(prefs)); } catch (e) { /* private mode */ }
}

function applyPrefs() {
  document.documentElement.dataset.theme = prefs.theme;
  document.querySelector('meta[name=theme-color]').content = THEMES[prefs.theme];
  $('theme').value = prefs.theme;
  $('board-style').value = prefs.board;
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
// The cribbage board, drawn in SVG in one of two styles (prefs.board). Each
// style supplies the static drawing and xy(), which places a peg; the pegs
// themselves are shared, and persist between renders so they can hop from
// hole to hole.
// ---------------------------------------------------------------------------

const Board = (() => {
  const NS = 'http://www.w3.org/2000/svg';
  const el = (tag, attrs, parent) => {
    const e = document.createElementNS(NS, tag);
    for (const k in attrs) e.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(e);
    return e;
  };

  // Serpentine: the classic board cut in the shape of its track, lying on its
  // side. Holes 1-35 run out along the top arm, 37-44 fan round the far end,
  // 46-80 come back along the bottom arm, 82-84 turn at the near end, and
  // 86-120 run out along the middle to the single 121 game hole. Holes are
  // grouped in fives with a number between groups. The geometry is worked in
  // portrait, with the start at the bottom left; an upright board is drawn
  // that way, and otherwise it is turned a quarter clockwise to lie along
  // the top of the table.
  const serpentine = upright => {
    const P = 18;        // distance between rows of holes
    const S = 70;        // distance between arms; the far turn's radius
    const HW = 10;       // half the distance between the two lanes
    const BW = 23;       // half the board's width
    const CYT = -6, YB = 44 * P, CYB = YB + 6; // centers of the far and near turns
    // map turns a portrait position into drawing coordinates.
    const map = upright ? (px, py) => [100 + px, 105 + py] : (px, py) => [910 - py, 100 + px];
    const pt = (px, py) => map(px, py).join(' ');
    const lane = { me: 0, opp: 1 }; // lane 1 runs on the outside of both turns

    // hole gives the portrait position of hole n (1-121, or 0 for the start
    // holes) in lane j; pegIdx picks between the two start holes.
    function hole(j, n, pegIdx) {
      const sg = j ? -1 : 1, A = -S + sg * HW, B = sg * HW, C = S - sg * HW;
      if (n <= 0) return [A, (pegIdx ? 45 : 44) * P];
      if (n <= 35) return [A, (2 + 6 * (6 - Math.floor((n - 1) / 5)) + 4 - (n - 1) % 5) * P];
      if (n === 36) return [A, 0];
      if (n <= 44) {
        const r = S - sg * HW, t = -Math.PI + (n - 36.5) * Math.PI / 8;
        return [r * Math.cos(t), CYT + r * Math.sin(t)];
      }
      if (n === 45) return [C, 0];
      if (n <= 80) { const k = n - 46; return [C, (2 + 6 * Math.floor(k / 5) + k % 5) * P]; }
      if (n === 81) return [C, YB];
      if (n <= 84) {
        const r = S / 2 - sg * HW, t = (n - 81.5) * Math.PI / 3;
        return [S / 2 + r * Math.cos(t), CYB + r * Math.sin(t)];
      }
      if (n === 85) return [B, YB];
      if (n <= 120) { const k = n - 86; return [B, (2 + 6 * (6 - Math.floor(k / 5)) + 4 - k % 5) * P]; }
      return [0, 0];
    }

    // track traces the board's whole length at offset off from its center
    // line: off 0 is the center, ±HW the lanes.
    function track(off, end) {
      const rT = S - off, rB = S / 2 - off;
      return `M ${pt(-S + off, 45 * P)} L ${pt(-S + off, CYT)}` +
        ` A ${rT} ${rT} 0 0 1 ${pt(S - off, CYT)} L ${pt(S - off, CYB)}` +
        ` A ${rB} ${rB} 0 0 1 ${pt(off, CYB)} L ${pt(off, end)}`;
    }

    function draw(svg, names) {
      const defs = el('defs', {}, svg);
      const wood = el('linearGradient', { id: 'board-wood', x1: 0, y1: 0, x2: 0, y2: 1 }, defs);
      el('stop', { class: 'wood-1', offset: 0 }, wood);
      el('stop', { class: 'wood-2', offset: 1 }, wood);
      el('path', { class: 'track-edge', d: track(0, 0) }, svg);
      el('path', { class: 'track', d: track(0, 0), stroke: 'url(#board-wood)' }, svg);
      for (const k of ['opp', 'me']) el('path', { class: `lane ${k}`, d: track(lane[k] ? -HW : HW, 2 * P) }, svg);

      // Numbers between the groups of five; the skunk lines fall at 60/61 and
      // 90/91, where the 60 and 90 sit.
      const num = (px, slot, text) => {
        const [x, y] = map(px, slot * P);
        const skunk = text === 60 || text === 90;
        if (skunk) {
          const g = el('g', {}, svg);
          el('title', {}, g).textContent = text === 60 ? 'Double skunk line' : 'Skunk line';
          const [x1, y1] = map(px - BW + 3, slot * P), [x2, y2] = map(px + BW - 3, slot * P);
          el('line', { class: 'skunk', x1, y1, x2, y2 }, g);
        }
        el('text', { class: 'num' + (skunk ? ' skunk-num' : '') + (text === 'Start' ? ' start' : ''), x, y: y + 3.5, 'text-anchor': 'middle' }, svg).textContent = text;
      };
      num(-S, 1, 35); num(0, 1, 120); num(S, 1, 45);
      for (let g = 0; g < 6; g++) { num(-S, 7 + 6 * g, 30 - 5 * g); num(0, 7 + 6 * g, 115 - 5 * g); num(S, 7 + 6 * g, 50 + 5 * g); }
      num(-S, 43, 'Start'); num(0, 43, 85); num(S, 43, 80);

      const at = ([px, py]) => { const [cx, cy] = map(px, py); return { cx, cy }; };
      for (const j of [0, 1]) {
        for (let n = 1; n <= 120; n++) el('circle', { class: 'hole', ...at(hole(j, n)), r: 4.2 }, svg);
        for (const i of [0, 1]) el('circle', { class: 'hole', ...at(hole(j, 0, i)), r: 4.2 }, svg);
      }
      el('circle', { class: 'game-hole', ...at(hole(0, 121)), r: 6.5 }, svg);
      // Lying down, the names fit beyond the start holes; upright they don't.
      if (!upright) {
        for (const k of ['opp', 'me']) {
          const [x, y] = map(hole(lane[k], 0)[0], 45 * P + BW + 6);
          names[k] = el('text', { class: `name lbl-${k}`, x, y: y + 3.5, 'text-anchor': 'end' }, svg);
        }
      }
    }

    return {
      viewBox: upright ? '0 0 200 975' : '0 0 1020 200', pegR: 7, draw,
      xy: (k, s, pegIdx) => map(...hole(lane[k], Math.min(s, 121), pegIdx)),
    };
  };

  // Long track: a continuous 121-hole board. Four runs of 30 holes snake
  // across it (1-30 out, 31-60 back, 61-90 out, 91-120 back), with the two
  // players' lanes side by side all the way; each player then finishes in
  // their own 121 game hole. The lanes swap sides at every turn so the turns
  // nest, as on a real board.
  const long = (() => {
    const X0 = 132, STEP = 25, GAP = 11;
    const LANE = 22, RUN = 50, TOP = 30; // lane spacing, run spacing, first lane
    const holeX = i => X0 + i * STEP + Math.floor(i / 5) * GAP;
    const LAST = holeX(29);
    const XL = X0 - 14, XR = LAST + 14; // where straight runs meet the turns
    // laneY is the y of seat k's lane in run r (0-3). Even runs put the
    // opponent above; odd runs swap.
    const laneY = (r, k) => TOP + r * RUN + ((k === 'opp') === (r % 2 === 0) ? 0 : LANE);

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
    function skunkLine(svg, r, label) {
      const right = r % 2 === 1;
      const cy = (TOP + (r - 1) * RUN + TOP + r * RUN + LANE) / 2;
      const x = right ? XR : XL, dir = right ? 1 : -1;
      el('line', { class: 'skunk', x1: x + dir * 6, y1: cy, x2: x + dir * 44, y2: cy }, svg);
      el('text', { class: 'skunk-text', x: x + dir * 50, y: cy + 4, 'text-anchor': right ? 'start' : 'end' }, svg).textContent = label;
    }

    function draw(svg, names) {
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
      skunkLine(svg, 2, 'SS');
      skunkLine(svg, 3, 'S');
    }

    return { viewBox: '0 0 1000 232', pegR: 9, draw, xy };
  })();

  let svg = null, style = null, gameId = null, last = null;
  let pegs = { me: [], opp: [] }, pos = { me: null, opp: null }, names = {};

  // build draws the board in container in the named style, upright if asked
  // and the style allows, replacing any board already there, and puts the
  // pegs back where they were.
  function build(container, styleName, upright) {
    style = styleName === 'long' ? long : serpentine(!!upright);
    container.replaceChildren();
    container.dataset.style = styleName === 'long' ? 'long' : 'serpentine';
    svg = el('svg', { viewBox: style.viewBox, role: 'img', 'aria-label': 'Cribbage board' }, container);
    names = {};
    style.draw(svg, names);
    pegs = { me: [], opp: [] };
    pos = { me: null, opp: null };
    for (const k of ['opp', 'me']) {
      for (let i = 0; i < 2; i++) {
        const g = el('g', { class: `peg ${k}` }, svg);
        el('circle', { class: 'body', r: style.pegR }, g);
        el('circle', { class: 'shine', r: style.pegR * .29, cx: -style.pegR / 3, cy: -style.pegR / 3 }, g);
        pegs[k].push(g);
      }
    }
    if (last) show(...last, false);
  }

  function place(k, i, s, animate) {
    const [x, y] = style.xy(k, s, i);
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

  function show(id, meName, oppName, v, animate) {
    last = [id, meName, oppName, v];
    if (id !== gameId) { gameId = id; pos = { me: null, opp: null }; animate = false; }
    if (names.me) {
      names.me.textContent = meName.toUpperCase().slice(0, 7);
      names.opp.textContent = oppName.toUpperCase().slice(0, 7);
    }
    const me = v.g.seat < 0 ? 0 : v.g.seat;
    update('me', v.scores[me], v.trail[me], animate);
    update('opp', v.scores[1 - me], v.trail[1 - me], animate);
  }

  return { build, show };
})();

// ---------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------

const state = {
  me: null,           // AuthOk
  userId: null,       // of the last sign-in on this page, which the games belong to
  games: new Map(),   // gameId -> latest GameUpdate (or {game} from a list)
  views: new Map(),   // gameId -> view: what the table currently shows
  logs: new Map(),    // gameId -> [{text, cls}]
  open: [],
  recent: [],
  invites: [],        // pending invites to and from me
  selected: null,
  pane: null,         // 'history' while the history replaces the table
  account: null,      // Account: email address, passkeys
  passkeysOffered: false, // the server offers passkeys (api/config)
  signin: null,       // a sign-in waiting for its emailed code: { challenge, name }
  newAccount: null,   // a code or link that creates an account, waiting for a username: { retry }
  history: null,      // {games, next, records, total} once fetched
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

// Handlers for replies to particular requests, by request id. A handler
// returns true if it dealt with the reply completely.
const replies = new Map();
function request(body, onReply) {
  const id = conn.send(body, true);
  if (id) replies.set(id, onReply);
  return id;
}

// Point the "Report a bug" links where the server says.
function setReportLinks(url) {
  if (typeof url !== 'string' || !url.startsWith('https://')) return;
  for (const a of document.querySelectorAll('a[data-report]')) a.href = url;
}

// A link from an email (#signin=, #verify=, #change= or #delete=<token>),
// or one to approve a device (#device=<code>). Read it once and take it
// out of the address bar and history.
let emailLink = null;
let pendingDevice = null; // a device code to approve once signed in
let stopToken = null; // from the link in an email that stops email to its address
(() => {
  const m = /^#(signin|verify|change|delete)=([0-9a-f]{64})$/.exec(location.hash);
  const d = /^#device=([A-Za-z]{4}-?[A-Za-z]{4})$/.exec(location.hash);
  const s = /^#stop=([0-9a-f]{96})$/.exec(location.hash);
  if (location.hash) history.replaceState(null, '', location.pathname + location.search);
  if (m) emailLink = { kind: m[1], token: m[2] };
  if (d) pendingDevice = d[1].toUpperCase();
  if (s) stopToken = s[1];
})();

// A link opened in a tab already showing the client changes only the
// fragment; load the page afresh so it is read like any other.
addEventListener('hashchange', () => {
  if (/^#(signin|verify|change|delete|device|stop)=/.test(location.hash)) location.reload();
});

function onOpen() {
  setStatus('online');
  clearTimeout(offlineTimer);
  offlineTimer = null;
  clearTimeout(reconnectTimer);
  reconnectTimer = null;
  $('offline').hidden = true;
  checkVersion();
  loadBots();
  state.me = null; // each connection authenticates afresh
  if (stopToken) { show('stop-panel'); return; }
  if (emailLink && (emailLink.kind === 'verify' || emailLink.kind === 'change')) {
    const { kind, token } = emailLink;
    emailLink = null;
    request({ verifyEmail: { token } }, m => {
      if (m.error) toast(m.error.message);
      else toast(kind === 'change' ? 'Confirmed. Now open the link we sent to your new address.' : 'Email address changed. Sign-in codes will go there now.');
      return true;
    });
  }
  // These panels' buttons send requests, so the connection must be ready.
  if (emailLink && emailLink.kind === 'signin') { conn.markReady(); setMsg('link-msg', ''); show('link-panel'); return; }
  if (emailLink && emailLink.kind === 'delete') { conn.markReady(); setMsg('delete-panel-msg', ''); show('delete-panel'); return; }
  const token = localStorage.getItem('token');
  if (token) {
    resumeSession(token);
    return;
  }
  conn.markReady();
  if ($('code-panel').hidden && $('username-panel').hidden) showLogin();
}

// resumeSession signs this connection back in; requests made meanwhile go
// once it has (or has failed: they then get the server's own answer).
function resumeSession(token) {
  const id = conn.sendNow({ resume: { token } });
  if (id) replies.set(id, () => { queueMicrotask(() => conn.markReady()); return false; });
}

// showLogin shows the sign-in page, saying why if a device is waiting.
function showLogin() {
  if (pendingDevice) setMsg('login-error', `Sign in to approve the device showing ${pendingDevice.slice(0, 4)}-${pendingDevice.slice(-4)}.`, true);
  show('login');
}

// A connection that drops comes back within a second or two (the server
// also closes one left on the sign-in page after 30 seconds), so say
// "reconnecting" only if it takes longer; requests made meanwhile wait.
let reconnectTimer = null;
function onClose() {
  if (!$('offline').hidden) setStatus('offline');
  else if (!reconnectTimer) reconnectTimer = setTimeout(() => { if (!conn.dc) setStatus('reconnecting…'); }, 3000);
  watchOffline();
}

// A new version of this page. The Pages build names its commit in a meta
// tag and in version.json; when they differ, the page offers to reload,
// and reloads by itself only while nobody is looking at it and no sign-in
// is under way. (The server's copy has no version and does not check.)
const PAGE_VERSION = document.querySelector('meta[name=cribbage-version]')?.content || '';
let newVersion = '', versionChecked = 0;
async function checkVersion() {
  if (!PAGE_VERSION || Date.now() - versionChecked < 60000) return;
  versionChecked = Date.now();
  try {
    const r = await fetch('version.json', { cache: 'no-store' });
    const v = r.ok ? String((await r.json()).version || '') : '';
    if (!v || v === PAGE_VERSION) return;
    newVersion = v;
  } catch (e) {
    return;
  }
  if (conn.stopped || (document.hidden && reloadIsSafe())) location.reload();
  else $('update').hidden = false;
}
// reloadIsSafe: nothing typed or opened would be lost. Games are on the
// server, so a reload during one only replays it.
function reloadIsSafe() {
  return !state.signin && !state.newAccount && !emailLink && !stopToken && !pendingDevice;
}
document.addEventListener('visibilitychange', () => {
  if (document.hidden && newVersion && reloadIsSafe()) location.reload();
  else if (!document.hidden) checkVersion();
});
setInterval(checkVersion, 10 * 60 * 1000);
$('update-reload').onclick = () => location.reload();
$('update-later').onclick = () => { $('update').hidden = true; };

// Say so if the server stays out of reach. After a restart it is back in
// seconds, but a stopped machine can take minutes or hours. The page
// keeps trying meanwhile, and the overlay leaves whatever was showing
// underneath, so play picks up where it was.
// Every failed retry calls this; the time counts from the first.
let offlineTimer = null;
function watchOffline() {
  if (offlineTimer) return;
  offlineTimer = setTimeout(() => {
    if (conn.dc || conn.stopped) return;
    setStatus('offline');
    $('offline').hidden = false;
  }, 6000);
}

function setStatus(text) {
  $('status').textContent = text;
  $('status').classList.toggle('online', text === 'online');
}

function show(...ids) {
  for (const id of ['login', 'lobby', 'username-panel', 'code-panel', 'link-panel', 'delete-panel', 'stop-panel']) $(id).hidden = !ids.includes(id);
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
  // An out-of-date page must reload whichever request found out.
  if (m.error && m.error.code === 'ERROR_CODE_UPGRADE_REQUIRED') {
    replies.delete(m.requestId);
    return onError(m.error);
  }
  const h = m.requestId && replies.get(m.requestId);
  if (h) {
    replies.delete(m.requestId);
    if (h(m)) return;
  }
  if (m.authOk) {
    try { sessionStorage.removeItem('upgrade-reload'); } catch (e) { /* ignore */ }
    if (state.userId !== m.authOk.userId) {
      // Someone else signed in on this tab: forget the last player's games.
      state.games.clear();
      state.views.clear();
      state.logs.clear();
      state.selected = null;
      state.userId = m.authOk.userId;
    }
    state.me = m.authOk;
    state.account = m.authOk.account || {};
    state.history = null;
    state.signin = null;
    emailLink = null;
    localStorage.setItem('token', m.authOk.token);
    $('who').textContent = `Signed in as ${m.authOk.nickname} (${m.authOk.username})`;
    $('account').hidden = false;
    setMsg('login-error', '');
    show('lobby');
    renderTable();
    conn.send({ listGames: {} });
    if (pendingDevice) openDeviceDialog(pendingDevice);
  } else if (m.account) {
    state.account = m.account;
    renderAccount();
  } else if (m.history) {
    onHistory(m.history);
  } else if (m.error) {
    onError(m.error);
  } else if (m.gameUpdate) {
    onUpdate(m.gameUpdate);
  } else if (m.invites) {
    setInvites(m.invites.invites || [], true);
  } else if (m.gameList) {
    const l = m.gameList;
    setInvites(l.invites || [], false);
    for (const g of l.mine || []) {
      if (!state.games.has(g.gameId)) state.games.set(g.gameId, { game: g });
    }
    state.open = l.open || [];
    state.recent = l.recent || [];
    // A game that ended while this page was away comes only in recent.
    let ended = false;
    for (const g of state.recent) {
      const u = state.games.get(g.gameId);
      if (!u || u.game.status === 'GAME_STATUS_FINISHED') continue;
      state.games.set(g.gameId, Object.assign({}, u, { game: g }));
      const v = state.views.get(g.gameId);
      if (v) v.g = g;
      ended = true;
    }
    renderLobby();
    if (ended) { updateTitle(); renderTable(); }
  } else if (m.matchQueued) {
    notice('Waiting for an opponent…');
  }
}

function onError(e) {
  if (e.code === 'ERROR_CODE_UPGRADE_REQUIRED') {
    // The server was updated since this page loaded: load the new client,
    // once. If that does not help, say so instead of reloading forever.
    conn.stopped = true;
    let reloaded = false;
    try { reloaded = sessionStorage.getItem('upgrade-reload') === '1'; sessionStorage.setItem('upgrade-reload', '1'); } catch (err) { /* ignore */ }
    if (!reloaded) {
      toast('Cribbage was updated. Reloading… (your games are saved)');
      setTimeout(() => location.reload(), 1500);
      return;
    }
    setStatus('out of date');
    setMsg('login-error', e.message);
    show('login');
    return;
  }
  if (!state.me && ['ERROR_CODE_INVALID_CREDENTIALS', 'ERROR_CODE_UNAUTHENTICATED'].includes(e.code)) {
    if (localStorage.getItem('token')) {
      localStorage.removeItem('token'); // stale session: sign in again
      showLogin();
      return;
    }
  }
  if (!state.me) {
    setMsg('login-error', e.message);
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

const anim = { queue: [], running: false, skip: false, waiters: new Set(), tails: new Set() };

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

// wait pauses the replay; a tail wait is one the player may end by acting.
function wait(ms, tail = false) {
  if (anim.skip || prefs.rapid) return Promise.resolve();
  return new Promise(resolve => {
    const done = () => { clearTimeout(t); anim.waiters.delete(done); anim.tails.delete(done); resolve(); };
    const t = setTimeout(done, ms);
    anim.waiters.add(done);
    if (tail) anim.tails.add(done);
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
    for (const [i, e] of events.entries()) {
      const fx = applyEvent(v, e, u);
      if (e.seat !== u.game.seat) oppActed = true;
      if (animated()) {
        // When the last event leaves us to act, unlock the hand now rather
        // than after the score bubbles fade; acting cuts the pause short.
        const unlock = i === events.length - 1 && u.observation && u.observation.mustAct;
        if (unlock) { syncView(v, u); v.busy = false; }
        renderTable(true);
        effects(v, fx);
        if (unlock) {
          if (oppActed) sfx.turn();
          await wait(fx.ms, true);
          return;
        }
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
      fx.ms = 330;
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
  f.dataset.key = `${at}:${seat}`;
  // Stack above labels still rising from the same spot (e.g. hand then crib).
  const stacked = [...layer.children].filter(o => o.dataset.key === f.dataset.key).length;
  layer.appendChild(f);
  f.style.left = `${x}px`;
  f.style.top = `${y - stacked * (f.offsetHeight + 6)}px`;
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
    case 'EVENT_KIND_EXPIRED': return 'The game expired: no moves for too long';
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

// ---------------------------------------------------------------------------
// History
// ---------------------------------------------------------------------------

function openHistory() {
  state.pane = 'history';
  setDrawer(false);
  renderLobby();
  renderTable();
  // Fetch afresh each time: games may have finished since.
  state.history = null;
  renderHistory();
  conn.send({ getHistory: {} });
}

function closeHistory() {
  state.pane = null;
  renderTable();
}

function onHistory(h) {
  const first = !state.history;
  if (first) state.history = { games: [], records: h.records || [], total: h.total || {} };
  state.history.games.push(...(h.games || []));
  state.history.next = h.next || '';
  renderHistory();
}

// botLabel names a bot as the lobby does ("Hard").
function botLabel(bot) {
  const b = botInfo.get(bot);
  return b ? b.label : 'Computer';
}

function playerLabel(p) {
  return p.bot ? botLabel(p.bot) : p.nickname || 'Unknown';
}

function shortDate(ms) {
  const d = new Date(Number(ms));
  const opts = { month: 'short', day: 'numeric' };
  if (d.getFullYear() !== new Date().getFullYear()) opts.year = 'numeric';
  return d.toLocaleDateString(undefined, opts);
}

function winRate(r) {
  const n = (r.wins || 0) + (r.losses || 0);
  return n ? `${Math.round(100 * (r.wins || 0) / n)}%` : '–';
}

function renderHistory() {
  const h = state.history;
  const total = $('history-total');
  const tbody = $('records').tBodies[0];
  if (!h) {
    total.textContent = 'Loading…';
    tbody.replaceChildren();
    $('history-games').replaceChildren();
    $('history-more').hidden = true;
    return;
  }
  const t = h.total;
  if (!t.games) {
    total.textContent = 'No finished games yet.';
  } else {
    const parts = [`<b>${t.games}</b> ${t.games === 1 ? 'game' : 'games'}`, `<b>${t.wins || 0}</b> won`, `<b>${t.losses || 0}</b> lost`];
    if (t.unfinished) parts.push(`<b>${t.unfinished}</b> unfinished`);
    total.innerHTML = parts.join(' · ') + ` · win rate <b>${winRate(t)}</b>`;
  }
  $('records').hidden = !h.records.length;
  tbody.replaceChildren(...h.records.map(r => {
    const tr = document.createElement('tr');
    tr.innerHTML = `<td>${esc(playerLabel(r.opponent))}</td><td>${r.games || 0}</td><td>${r.wins || 0}</td>` +
      `<td>${r.losses || 0}</td><td>${winRate(r)}</td>`;
    if (r.unfinished) tr.title = `${r.unfinished} unfinished (expired or cancelled)`;
    return tr;
  }));
  fillList($('history-games'), h.games.map(g => {
    const li = document.createElement('li');
    const res = g.winner < 0 ? 'unfinished' : g.winner === g.seat ? 'won' : 'lost';
    const tag = res === 'lost' && g.resigned ? 'resigned' : res;
    const opp = (g.players || [])[1 - g.seat] || {};
    li.innerHTML = `<span class="when">${esc(shortDate(g.updated))}</span>` +
      `<span class="name">vs ${esc(playerLabel(opp))}</span>` +
      `<span class="score">${g.scores[g.seat] || 0}–${g.scores[1 - g.seat] || 0}</span>` +
      `<span class="tag ${res}">${tag}</span>`;
    return li;
  }), 'None yet');
  $('history-more').hidden = !h.next;
  $('history-more').disabled = false;
}

// ---------------------------------------------------------------------------
// Signing in: emailed codes and links, passkeys, approving other devices
// ---------------------------------------------------------------------------

// formatCode shows a typed code as "1234 5678".
function formatCode(s) {
  const d = String(s).replace(/\D/g, '');
  return d.length > 4 ? d.slice(0, 4) + ' ' + d.slice(4) : d;
}

// startCode shows the code page for a sign-in the server has started.
function startCode(started, name) {
  state.signin = { challenge: started.challenge, name };
  const mins = Math.round((started.expiresIn || 900) / 60);
  const byEmail = name.includes('@');
  $('code-hint').textContent = byEmail
    ? `We sent a code to ${name}. Enter it here within ${mins} minutes, or open the link in the email on this device. New to Cribbage? The same code creates your account.`
    : `If “${name}” has an account, we sent a code to its email address. Enter it here within ${mins} minutes, or open the link in the email on this device.`;
  $('code-new').textContent = byEmail
    ? 'No email after a few minutes? Check your spam folder.'
    : 'No email after a few minutes? Check your spam folder. New here? Go back and enter your email address instead: a username only works once you have an account.';
  $('signin-code-form').reset();
  setMsg('signin-code-msg', '');
  show('code-panel');
  $('signin-code-form').elements.code.focus();
}

$('signin-form').onsubmit = ev => {
  ev.preventDefault();
  const name = String(new FormData(ev.target).get('name')).trim();
  if (!name) return;
  setMsg('login-error', '');
  const sent = request({ requestSignIn: { name } }, m => {
    if (m.error) { setMsg('login-error', m.error.message); return true; }
    startCode(m.signInStarted, name);
    return true;
  });
  if (!sent) setMsg('login-error', 'Not connected yet…');
};

// chooseUsername asks for the new account's name, once the server has
// taken a code or link that creates one (NeedsUsername). retry sends the
// code or link again with { username, nickname }.
function chooseUsername(email, retry) {
  state.newAccount = { retry };
  $('username-hint').textContent = `Your account will use ${email}. Choose a username: other players see your nickname.`;
  $('username-form').reset();
  setMsg('username-msg', '');
  show('username-panel');
  $('username-form').elements.username.focus();
}

$('username-form').onsubmit = ev => {
  ev.preventDefault();
  if (!state.newAccount) return showLogin();
  const f = new FormData(ev.target);
  const names = { username: String(f.get('username')).trim(), nickname: String(f.get('nickname')).trim() };
  setMsg('username-msg', '');
  const sent = state.newAccount.retry(names, m => {
    if (!m.error) return false; // AuthOk signs in as usual
    if (m.error.code === 'ERROR_CODE_USERNAME_TAKEN') setMsg('username-msg', 'That username is taken. Try another.');
    else if (m.error.code === 'ERROR_CODE_NOT_FOUND') {
      state.newAccount = null;
      setMsg('login-error', 'That code has expired. Enter your email again for a new one.');
      showLogin();
    } else setMsg('username-msg', m.error.message);
    return true;
  });
  if (!sent) setMsg('username-msg', 'Not connected yet…');
};

$('signin-code-form').elements.code.addEventListener('input', e => {
  const el = e.target;
  if (/^[\d\s-]*$/.test(el.value)) el.value = formatCode(el.value);
});

$('signin-code-form').onsubmit = ev => {
  ev.preventDefault();
  if (!state.signin) return showLogin();
  const code = String(new FormData(ev.target).get('code'));
  const { challenge } = state.signin;
  setMsg('signin-code-msg', '');
  const sent = request({ enterCode: { challenge, code } }, m => {
    if (m.needsUsername) {
      chooseUsername(m.needsUsername.email, (names, onReply) => request({ enterCode: { challenge, code, ...names } }, onReply));
      return true;
    }
    if (!m.error) return false; // AuthOk signs in as usual
    if (m.error.code === 'ERROR_CODE_NOT_FOUND') {
      setMsg('signin-code-msg', 'That code has expired or had too many wrong tries. Choose “Send again” for a new one.');
    } else {
      setMsg('signin-code-msg', m.error.message);
    }
    return true;
  });
  if (!sent) setMsg('signin-code-msg', 'Not connected yet…');
};

$('code-resend').onclick = () => {
  const s = state.signin;
  if (!s) return showLogin();
  const sent = request({ requestSignIn: { name: s.name } }, m => {
    if (m.error) { setMsg('signin-code-msg', m.error.message); return true; }
    startCode(m.signInStarted, s.name);
    setMsg('signin-code-msg', 'Sent. Check your email (and its spam folder).', true);
    return true;
  });
  if (!sent) setMsg('signin-code-msg', 'Not connected yet…');
};

$('link-confirm').onclick = () => {
  if (!emailLink || emailLink.kind !== 'signin') return showLogin();
  const { token } = emailLink;
  const sent = request({ useSignInLink: { token } }, m => {
    if (m.needsUsername) {
      emailLink = null;
      chooseUsername(m.needsUsername.email, (names, onReply) => request({ useSignInLink: { token, ...names } }, onReply));
      return true;
    }
    if (!m.error) return false;
    emailLink = null;
    setMsg('login-error', m.error.code === 'ERROR_CODE_NOT_FOUND'
      ? 'That sign-in link has expired or was already used. Ask for a new code below.' : m.error.message);
    showLogin();
    return true;
  });
  if (!sent) setMsg('link-msg', 'Not connected yet…');
};

for (const b of document.querySelectorAll('[data-to-login]')) {
  b.onclick = () => {
    emailLink = null;
    state.signin = null;
    state.newAccount = null;
    setMsg('login-error', '');
    showLogin();
    const token = localStorage.getItem('token');
    if (token && !state.me) resumeSession(token);
  };
}

// Passkeys. The server speaks WebAuthn's JSON form (binary values in
// base64url); browsers want ArrayBuffers.
const b64u = {
  decode(s) {
    const t = s.replace(/-/g, '+').replace(/_/g, '/') + '='.repeat((4 - s.length % 4) % 4);
    return Uint8Array.from(atob(t), c => c.charCodeAt(0)).buffer;
  },
  encode(buf) {
    let bin = '';
    for (const b of new Uint8Array(buf)) bin += String.fromCharCode(b);
    return btoa(bin).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  },
};

function passkeysUsable() {
  return state.passkeysOffered && !!window.PublicKeyCredential && !!navigator.credentials;
}

function creationOptions(pk) {
  if (PublicKeyCredential.parseCreationOptionsFromJSON) return PublicKeyCredential.parseCreationOptionsFromJSON(pk);
  return Object.assign({}, pk, {
    challenge: b64u.decode(pk.challenge),
    user: Object.assign({}, pk.user, { id: b64u.decode(pk.user.id) }),
    excludeCredentials: (pk.excludeCredentials || []).map(c => Object.assign({}, c, { id: b64u.decode(c.id) })),
  });
}

function requestOptions(pk) {
  if (PublicKeyCredential.parseRequestOptionsFromJSON) return PublicKeyCredential.parseRequestOptionsFromJSON(pk);
  return Object.assign({}, pk, {
    challenge: b64u.decode(pk.challenge),
    allowCredentials: (pk.allowCredentials || []).map(c => Object.assign({}, c, { id: b64u.decode(c.id) })),
  });
}

function credentialJSON(cred) {
  if (typeof cred.toJSON === 'function') return JSON.stringify(cred.toJSON());
  const r = cred.response;
  const response = { clientDataJSON: b64u.encode(r.clientDataJSON) };
  if (r.attestationObject) {
    response.attestationObject = b64u.encode(r.attestationObject);
    if (r.getTransports) response.transports = r.getTransports();
  } else {
    response.authenticatorData = b64u.encode(r.authenticatorData);
    response.signature = b64u.encode(r.signature);
    if (r.userHandle) response.userHandle = b64u.encode(r.userHandle);
  }
  return JSON.stringify({
    id: cred.id, rawId: b64u.encode(cred.rawId), type: cred.type,
    authenticatorAttachment: cred.authenticatorAttachment || undefined,
    clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
    response,
  });
}

// passkeyError explains a failed browser ceremony, or nothing if the user
// just cancelled it.
function passkeyError(e) {
  if (e && (e.name === 'NotAllowedError' || e.name === 'AbortError')) return '';
  if (e && e.name === 'InvalidStateError') return 'This device already has a passkey for your account.';
  return 'Your browser could not use a passkey' + (e && e.message ? `: ${e.message}` : '.');
}

$('passkey-signin').onclick = () => {
  setMsg('login-error', '');
  const sent = request({ passkeySignInBegin: {} }, m => {
    if (m.error) { setMsg('login-error', m.error.message); return true; }
    const opts = JSON.parse(m.passkeyOptions.optionsJson);
    navigator.credentials.get({ publicKey: requestOptions(opts.publicKey) }).then(cred => {
      request({ passkeySignInFinish: { credentialJson: credentialJSON(cred) } }, r => {
        if (!r.error) return false;
        setMsg('login-error', r.error.code === 'ERROR_CODE_INVALID_CREDENTIALS'
          ? "That passkey didn't work here. It may have been removed: sign in with a code instead." : r.error.message);
        return true;
      });
    }).catch(e => setMsg('login-error', passkeyError(e)));
    return true;
  });
  if (!sent) setMsg('login-error', 'Not connected yet…');
};

// ---------------------------------------------------------------------------
// Account: email address, passkeys, devices, deleting
// ---------------------------------------------------------------------------

function openAccount() {
  for (const f of ['email-form', 'passkey-form', 'device-form', 'delete-form']) $(f).reset();
  for (const m of ['email-msg', 'passkey-msg', 'device-msg', 'sign-out-msg', 'delete-msg']) setMsg(m, '');
  renderAccount();
  $('settings').hidden = true;
  if (!$('account-dialog').open) $('account-dialog').showModal();
}

function fullDate(ms) {
  return new Date(Number(ms)).toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
}

function renderAccount() {
  const a = state.account || {};
  let status = a.email ? `Sign-in codes go to ${a.email}.` : 'This account has no email address.';
  if (a.pendingEmail) {
    status += a.pendingNeedsCurrent
      ? ` To change it to ${a.pendingEmail}, open the link we sent to ${a.email || 'your current address'}, then the one we send to the new address.`
      : ` We sent a link to ${a.pendingEmail}: open it to start using that address.`;
  }
  $('email-status').textContent = status;

  $('passkey-section').hidden = !a.passkeysEnabled || !window.PublicKeyCredential;
  const list = $('passkey-list');
  list.replaceChildren();
  for (const k of a.passkeys || []) {
    const li = document.createElement('li');
    const used = Number(k.lastUsed) ? `last used ${fullDate(k.lastUsed)}` : 'not used yet';
    li.innerHTML = `<span><b>${esc(k.name)}</b><small>added ${esc(fullDate(k.created))}, ${esc(used)}</small></span>`;
    const rm = document.createElement('button');
    rm.type = 'button';
    rm.className = 'btn btn-ghost';
    rm.textContent = 'Remove';
    rm.onclick = () => {
      if (!confirm(`Remove the passkey “${k.name}”? It will no longer sign you in.`)) return;
      request({ passkeyRemove: { id: k.id } }, m => {
        if (m.error) setMsg('passkey-msg', m.error.message);
        else { state.account = m.account; renderAccount(); setMsg('passkey-msg', 'Passkey removed. You can also delete it from your device or password manager.', true); }
        return true;
      });
    };
    li.appendChild(rm);
    list.appendChild(li);
  }
  if (!(a.passkeys || []).length) list.innerHTML = '<li class="muted">No passkeys yet.</li>';

  $('delete-username').hidden = !!a.email;
  $('delete-form').elements.username.required = !a.email;
  $('delete-status').textContent = a.email
    ? `We'll email a link to ${a.email}; your account is deleted when you open it and confirm.`
    : 'Your account is deleted at once. This cannot be undone.';
}

function setMsg(id, text, ok = false) {
  $(id).textContent = text;
  $(id).classList.toggle('ok', ok);
}

$('account-btn').onclick = () => openAccount();
$('account-close').onclick = () => $('account-dialog').close();

$('email-form').onsubmit = ev => {
  ev.preventDefault();
  const email = String(new FormData(ev.target).get('email')).trim();
  if (!email) return setMsg('email-msg', 'Enter an email address.');
  setMsg('email-msg', 'Sending…', true);
  const sent = request({ setEmail: { email } }, m => {
    if (m.error) { setMsg('email-msg', m.error.message); return true; }
    state.account = m.account;
    ev.target.reset();
    renderAccount();
    const a = m.account;
    setMsg('email-msg', !a.pendingEmail ? 'Saved.' : a.pendingNeedsCurrent
      ? `Check ${a.email} for a link to confirm the change.` : `Check ${a.pendingEmail} for a link to confirm it.`, true);
    return true;
  });
  if (!sent) setMsg('email-msg', 'Not connected.');
};

$('passkey-form').onsubmit = ev => {
  ev.preventDefault();
  const name = String(new FormData(ev.target).get('name')).trim() || defaultPasskeyName();
  setMsg('passkey-msg', '');
  const sent = request({ passkeyAddBegin: {} }, m => {
    if (m.error) { setMsg('passkey-msg', m.error.message); return true; }
    const opts = JSON.parse(m.passkeyOptions.optionsJson);
    navigator.credentials.create({ publicKey: creationOptions(opts.publicKey) }).then(cred => {
      request({ passkeyAddFinish: { credentialJson: credentialJSON(cred), name } }, r => {
        if (r.error) { setMsg('passkey-msg', r.error.message); return true; }
        state.account = r.account;
        ev.target.reset();
        renderAccount();
        setMsg('passkey-msg', 'Passkey added. Next time, choose “Use a passkey” to sign in.', true);
        return true;
      });
    }).catch(e => setMsg('passkey-msg', passkeyError(e)));
    return true;
  });
  if (!sent) setMsg('passkey-msg', 'Not connected.');
};

// defaultPasskeyName guesses what device this is, for the passkey list.
function defaultPasskeyName() {
  const ua = navigator.userAgent;
  for (const [re, name] of [[/iPhone/, 'iPhone'], [/iPad/, 'iPad'], [/Android/, 'Android'], [/Mac OS X/, 'Mac'], [/Windows/, 'Windows'], [/CrOS/, 'Chromebook'], [/Linux/, 'Linux']]) {
    if (re.test(ua)) return name;
  }
  return 'Passkey';
}

function approveDevice(code, msgId, done) {
  setMsg(msgId, '');
  const sent = request({ approveDevice: { code } }, m => {
    if (m.error) {
      setMsg(msgId, m.error.code === 'ERROR_CODE_NOT_FOUND'
        ? 'No device is showing that code. Check it, or start again on the device: codes last 10 minutes.' : m.error.message);
      return true;
    }
    done();
    return true;
  });
  if (!sent) setMsg(msgId, 'Not connected.');
}

$('device-form').onsubmit = ev => {
  ev.preventDefault();
  const code = String(new FormData(ev.target).get('code')).trim();
  approveDevice(code, 'device-msg', () => {
    ev.target.reset();
    setMsg('device-msg', 'Approved. The device is signing in.', true);
  });
};

function openDeviceDialog(code) {
  $('device-code').textContent = `${code.slice(0, 4)}-${code.slice(-4)}`;
  setMsg('device-dialog-msg', '');
  if (!$('device-dialog').open) $('device-dialog').showModal();
}
$('device-approve').onclick = () => {
  const code = pendingDevice;
  if (!code) return $('device-dialog').close();
  approveDevice(code, 'device-dialog-msg', () => {
    pendingDevice = null;
    $('device-dialog').close();
    toast('Approved. The device is signing in.');
  });
};
$('device-cancel').onclick = () => { pendingDevice = null; $('device-dialog').close(); };
$('device-dialog').addEventListener('cancel', () => { pendingDevice = null; });

$('sign-out-others').onclick = () => {
  setMsg('sign-out-msg', '');
  const sent = request({ signOutOthers: {} }, m => {
    if (m.error) setMsg('sign-out-msg', m.error.message);
    else setMsg('sign-out-msg', 'Your other devices are signed out.', true);
    return true;
  });
  if (!sent) setMsg('sign-out-msg', 'Not connected.');
};

$('delete-form').onsubmit = ev => {
  ev.preventDefault();
  const f = new FormData(ev.target);
  setMsg('delete-msg', '');
  const sent = request({ deleteAccount: { username: f.get('username') || '' } }, m => {
    if (m.error) { setMsg('delete-msg', m.error.message); return true; }
    ev.target.reset();
    if (m.ack) {
      setMsg('delete-msg', `Check ${state.account.email} for a link to confirm. It works for an hour.`, true);
      return true;
    }
    accountDeleted();
    return true;
  });
  if (!sent) setMsg('delete-msg', 'Not connected.');
};

// accountDeleted forgets the session; the server closes the connection,
// and the client reconnects to the sign-in page.
function accountDeleted() {
  localStorage.removeItem('token');
  state.me = null;
  if ($('account-dialog').open) $('account-dialog').close();
  setMsg('login-error', 'Your account was deleted.', true);
  show('login');
}

$('delete-confirm').onclick = () => {
  if (!emailLink || emailLink.kind !== 'delete') return show('login');
  const token = emailLink.token;
  const sent = request({ confirmDeleteAccount: { token } }, m => {
    if (m.error) {
      setMsg('delete-panel-msg', m.error.code === 'ERROR_CODE_NOT_FOUND'
        ? 'This link has expired or was already used. Ask for a new one from your account settings.' : m.error.message);
      return true;
    }
    emailLink = null;
    // This browser may be signed in as that account (resuming will fail
    // and forget it) or as another one (which carries on).
    const token = localStorage.getItem('token');
    setMsg('login-error', `The account ${m.accountDeleted.username} was deleted.`, true);
    toast(`The account ${m.accountDeleted.username} was deleted.`);
    show('login');
    if (!state.me && token) resumeSession(token);
    return true;
  });
  if (!sent) setMsg('delete-panel-msg', 'Not connected yet…');
};

// ---------------------------------------------------------------------------
// Invites
// ---------------------------------------------------------------------------

function setInvites(list, pushed) {
  const known = new Set(state.invites.map(i => i.inviteId));
  if (pushed) {
    for (const i of list) if (i.incoming && !known.has(i.inviteId)) toast(`${i.from.nickname} invited you to a game`);
  }
  state.invites = list;
  renderInvites();
}

function timeLeft(expires) {
  const ms = Number(expires) - Date.now();
  const h = Math.floor(ms / 3600000);
  return h >= 1 ? `${h} h left` : `${Math.max(1, Math.ceil(ms / 60000))} min left`;
}

function renderInvites() {
  const ul = $('invites');
  ul.replaceChildren();
  $('invites-title').hidden = state.invites.length === 0;
  for (const inv of state.invites) {
    const li = document.createElement('li');
    const respond = accept => conn.send({ respondInvite: { inviteId: inv.inviteId, accept } });
    const button = (label, primary, fn) => {
      const b = document.createElement('button');
      b.className = `btn btn-small${primary ? ' btn-primary' : ''}`;
      b.textContent = label;
      b.onclick = fn;
      return b;
    };
    const acts = document.createElement('span');
    acts.className = 'acts';
    if (inv.incoming) {
      li.className = 'incoming';
      li.innerHTML = `<span class="name"><b>${esc(inv.from.nickname)}</b> invited you</span>`;
      acts.append(button('Play', true, () => respond(true)), button('No thanks', false, () => respond(false)));
    } else if (inv.code) {
      li.innerHTML = `<span class="name">Code <span class="code" title="Give this to the person you want to play">${esc(inv.code)}</span></span>`;
      acts.append(button('Cancel', false, () => respond(false)));
    } else {
      li.innerHTML = `<span class="name">Waiting for <b>${esc(inv.to.nickname)}</b></span>`;
      acts.append(button('Cancel', false, () => respond(false)));
    }
    const when = document.createElement('span');
    when.className = 'when';
    when.textContent = timeLeft(inv.expires);
    li.append(when, acts);
    ul.appendChild(li);
  }
}

function select(id) {
  if (state.selected !== id) skipAnimation();
  state.selected = id;
  state.pane = null;
  state.picked.clear();
  setDrawer(false);
  renderLobby();
  renderTable();
}

// On a landscape screen the serpentine board stands upright beside the table.
const sideBoard = matchMedia('(min-width: 861px) and (orientation: landscape)');
function buildBoard() {
  const side = prefs.board === 'serpentine' && sideBoard.matches;
  $('table').classList.toggle('side-board', side);
  Board.build($('board'), prefs.board, side);
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

function plate(el, v, seat, isMe) {
  const g = v.g;
  const p = (g.players || [])[seat] || {};
  const name = isMe ? 'You' : (p.nickname || 'Waiting…');
  const initial = (isMe ? (state.me && state.me.nickname) || 'Y' : p.nickname || '?').trim().charAt(0).toUpperCase();
  const online = isMe || p.bot || p.connected;
  const sub = p.bot ? botLabel(p.bot) : isMe ? (state.me ? state.me.nickname : '') : online ? 'online' : 'away';
  el.className = `plate ${isMe ? 'me' : 'opp'}`;
  el.innerHTML = `<div class="avatar">${esc(initial)}<span class="conn ${online ? 'on' : ''}"></span></div>` +
    `<div><div class="nm">${esc(name)}${v.dealer === seat ? '<span class="dealer" title="Dealer: the crib is theirs">DEALER</span>' : ''}</div>` +
    `<div class="sub">${esc(sub)}</div></div><div class="pts${v.bump.has(seat) ? ' bump' : ''}">${v.scores[seat]}</div>`;
}

function renderTable(animate = false) {
  const hist = !!(state.me && state.pane === 'history');
  const has = !hist && !!(state.me && state.selected && state.games.has(state.selected));
  document.body.classList.toggle('has-table', has || hist);
  $('history').hidden = !hist;
  $('table').hidden = !has;
  $('empty').hidden = has || hist || !state.me;
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
  for (const w of [...anim.tails]) w();
  state.picked.clear();
  const v = view(state.selected);
  if (v) { v.legal = []; v.mustAct = false; renderTable(); }
}

// ---------------------------------------------------------------------------
// UI wiring
// ---------------------------------------------------------------------------

$('logout').onclick = () => {
  localStorage.removeItem('token');
  // End the session on the server too. It closes the connection once it
  // has; reload then, or after a moment if that never comes.
  const reload = () => location.reload();
  if (!conn.send({ logout: {} })) return reload();
  if (conn.dc) conn.dc.addEventListener('close', reload);
  setTimeout(reload, 1500);
};

// The server says which bots to offer and how to label every bot it has,
// so the lobby follows the server rather than this file.
const botInfo = new Map();
function loadBots() {
  fetch(serverURL('api/lobby')).then(r => r.json()).then(l => {
    botInfo.clear();
    for (const b of l.bots || []) botInfo.set(b.name, b);
    const picks = $('bot-picks');
    picks.replaceChildren();
    for (const name of l.offer || []) {
      const b = botInfo.get(name);
      if (!b) continue;
      const btn = document.createElement('button');
      btn.className = 'pick';
      btn.onclick = () => conn.send({ createGame: { bot: name } });
      const icon = document.createElement('span');
      icon.className = 'pick-icon';
      icon.textContent = b.icon || '●';
      const text = document.createElement('span');
      const label = document.createElement('b');
      label.textContent = b.label;
      const blurb = document.createElement('small');
      blurb.textContent = b.blurb || '';
      text.append(label, blurb);
      btn.append(icon, text);
      picks.append(btn);
    }
    // Labels may have changed under views drawn before the list came.
    renderLobby();
    if (state.pane === 'history') renderHistory(); else renderTable();
  }).catch(() => { /* try again on the next connection */ });
}
$('create-open').onclick = () => conn.send({ createGame: { bot: '' } });
$('invite-form').onsubmit = ev => {
  ev.preventDefault();
  const username = $('invite-user').value.trim();
  if (!username) return;
  conn.send({ sendInvite: { username } });
  $('invite-user').value = '';
};
$('invite-code').onclick = () => conn.send({ sendInvite: {} });
$('code-form').onsubmit = ev => {
  ev.preventDefault();
  const code = $('code-input').value.trim();
  if (!code) return;
  conn.send({ redeemCode: { code } });
  $('code-input').value = '';
};
setInterval(() => { if (state.invites.length) renderInvites(); }, 60000);
$('open-history').onclick = () => openHistory();
$('history-close').onclick = () => closeHistory();
$('history-more').onclick = () => {
  if (!state.history || !state.history.next) return;
  $('history-more').disabled = true;
  conn.send({ getHistory: { after: state.history.next } });
};
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
$('board-style').onchange = () => { prefs.board = $('board-style').value; savePrefs(); buildBoard(); };
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
buildBoard();
sideBoard.addEventListener('change', buildBoard);
// The stop link in an email. It needs no connection: it posts its token
// to the server over HTTPS, as mail apps' one-click unsubscribe does.
$('stop-confirm').onclick = async () => {
  $('stop-confirm').disabled = true;
  setMsg('stop-msg', 'Stopping…', true);
  try {
    const r = await fetch(serverURL('api/stop?t=' + stopToken), { method: 'POST', signal: AbortSignal.timeout(10000) });
    if (r.ok) {
      setMsg('stop-msg', 'Done. Cribbage will not email this address again.', true);
      $('stop-cancel').textContent = 'Close';
      return;
    }
    setMsg('stop-msg', r.status === 404 ? 'That link is not one this server sent.' : 'That did not work. Try again in a few minutes.');
  } catch (e) {
    setMsg('stop-msg', 'Could not reach the server. Try again in a few minutes.');
  }
  $('stop-confirm').disabled = false;
};
$('stop-cancel').onclick = () => {
  stopToken = null;
  show();
  if (conn.dc) onOpen();
};

if (stopToken) show('stop-panel');
conn.connect();
watchOffline();
