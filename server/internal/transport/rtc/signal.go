package rtc

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"cribbage/internal/netkey"
)

// signal is one message on the signaling WebSocket.
type signal struct {
	Type      string                   `json:"type"` // "offer", "answer" or "candidate"
	SDP       string                   `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit `json:"candidate,omitempty"`
}

// signaler sends signals over a WebSocket. Local ICE candidates found before
// our description has been sent are held back, because the peer cannot add
// candidates before it has a remote description.
type signaler struct {
	ws      *websocket.Conn
	mu      sync.Mutex
	ready   bool
	pending []signal
}

func (s *signaler) send(ctx context.Context, m signal) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.ws.Write(ctx, websocket.MessageText, b)
}

// description sends our offer or answer, then any held candidates.
func (s *signaler) description(ctx context.Context, m signal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.send(ctx, m); err != nil {
		return err
	}
	s.ready = true
	for _, p := range s.pending {
		if err := s.send(ctx, p); err != nil {
			return err
		}
	}
	s.pending = nil
	return nil
}

func (s *signaler) candidate(ctx context.Context, c *webrtc.ICECandidate) {
	if c == nil {
		return // gathering finished
	}
	init := c.ToJSON()
	m := signal{Type: "candidate", Candidate: &init}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.ready {
		s.pending = append(s.pending, m)
		return
	}
	_ = s.send(ctx, m)
}

// remoteCandidates queues candidates that arrive before the remote
// description is set. With keep set, it ignores candidates keep refuses,
// and refuses more than maxCandidates in all.
type remoteCandidates struct {
	keep func(addr string) bool

	mu      sync.Mutex
	ready   bool
	pending []webrtc.ICECandidateInit
	count   int
}

// maxCandidates bounds the remote candidates one session may offer: a
// browser sends a handful, and each one is an address we send probes to.
const maxCandidates = 32

var errTooManyCandidates = errors.New("too many ICE candidates")

func (r *remoteCandidates) add(pc *webrtc.PeerConnection, c webrtc.ICECandidateInit) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.count++; r.count > maxCandidates {
		return errTooManyCandidates
	}
	if r.keep != nil && !r.keep(candidateAddress(c.Candidate)) {
		return nil
	}
	if !r.ready {
		r.pending = append(r.pending, c)
		return nil
	}
	return pc.AddICECandidate(c)
}

func (r *remoteCandidates) described(pc *webrtc.PeerConnection) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready = true
	for _, c := range r.pending {
		if err := pc.AddICECandidate(c); err != nil {
			return err
		}
	}
	r.pending = nil
	return nil
}

// candidateAddress is the connection address of an ICE candidate line
// ("candidate:foundation component protocol priority address port typ
// ..."), or "" if it has none.
func candidateAddress(line string) string {
	line = strings.TrimPrefix(strings.TrimPrefix(line, "a="), "candidate:")
	if f := strings.Fields(line); len(f) >= 6 {
		return f[4]
	}
	return ""
}

// publicAddress reports whether a candidate's address is a public IP. It
// refuses private, loopback and link-local addresses, which would let a
// client make the server send probes into its own network, and mDNS names
// (".local"), which only resolve on the client's network anyway.
func publicAddress(addr string) bool {
	return netkey.Public(net.ParseIP(addr))
}

// filterSDP drops the candidate lines of sdp whose address keep refuses,
// and fails if there are more than maxCandidates.
func filterSDP(sdp string, keep func(string) bool) (string, int, error) {
	lines := strings.SplitAfter(sdp, "\n")
	out := lines[:0]
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "a=candidate:") {
			if n++; n > maxCandidates {
				return "", n, errTooManyCandidates
			}
			if keep != nil && !keep(candidateAddress(strings.TrimSpace(l))) {
				continue
			}
		}
		out = append(out, l)
	}
	return strings.Join(out, ""), n, nil
}

func readSignal(ctx context.Context, ws *websocket.Conn) (signal, error) {
	var m signal
	_, b, err := ws.Read(ctx)
	if err != nil {
		return m, err
	}
	return m, json.Unmarshal(b, &m)
}

// maxMessage is the largest data channel message accepted.
const maxMessage = 64 << 10

// Options configure WebRTC networking.
type Options struct {
	// UDPPort, if non-zero, carries all WebRTC traffic on one UDP port
	// (simpler firewalling); otherwise each connection picks its own.
	UDPPort int
	// PublicIPs are advertised in addition to local addresses, for servers
	// behind a 1:1 NAT (e.g. a cloud VM).
	PublicIPs []string
	// ICEServers (e.g. "stun:stun.l.google.com:19302") are given to clients
	// and used by the server.
	ICEServers []string
	// PublicOnly ignores clients' ICE candidates on private, loopback and
	// link-local addresses, so a client cannot make the server probe its
	// internal network. Set it on a server on the internet; leave it off
	// to play on one machine or a LAN.
	PublicOnly bool
	// MaxSessions bounds connection setups in progress at once (default
	// 200), and MaxSessionsPerAddr those from one address (default 10).
	// Behind a reverse proxy on localhost, the address is the last
	// X-Forwarded-For entry.
	MaxSessions        int
	MaxSessionsPerAddr int
	Logger             *slog.Logger
}

// API holds WebRTC settings shared by all connections.
type API struct {
	api    *webrtc.API
	config webrtc.Configuration
	log    *slog.Logger
	mux    *ice.MultiUDPMuxDefault
	keep   func(addr string) bool // remote candidates to use; nil for all

	smu         sync.Mutex
	sessions    int
	byAddr      map[string]int
	maxSessions int
	maxPerAddr  int

	// live counts accepted connections whose peer connection has not yet
	// finished closing, so Close can let peers hear that we are going.
	mu      sync.Mutex
	closing bool
	live    sync.WaitGroup
}

// NewAPI prepares WebRTC networking.
func NewAPI(o Options) (*API, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	var se webrtc.SettingEngine
	if !o.PublicOnly {
		se.SetIncludeLoopbackCandidate(true) // lets a browser on the same machine connect
	}
	// Every protocol message is small. Advertise that, and bound what a
	// peer can make us buffer (pion's defaults: 1 GiB messages, 1 MiB
	// receive buffer); conn.go closes a peer that sends more anyway.
	se.SetSCTPMaxMessageSize(maxMessage)
	se.SetSCTPMaxReceiveBufferSize(4 * maxMessage)
	a := &API{
		log:         o.Logger,
		byAddr:      map[string]int{},
		maxSessions: cmp.Or(o.MaxSessions, 200),
		maxPerAddr:  cmp.Or(o.MaxSessionsPerAddr, 10),
	}
	if o.PublicOnly {
		a.keep = publicAddress
	}
	if o.UDPPort != 0 {
		mux, err := ice.NewMultiUDPMuxFromPort(o.UDPPort, ice.UDPMuxFromPortWithLoopback())
		if err != nil {
			return nil, fmt.Errorf("listen udp %d: %w", o.UDPPort, err)
		}
		se.SetICEUDPMux(mux)
		a.mux = mux
	}
	if len(o.PublicIPs) > 0 {
		if err := se.SetICEAddressRewriteRules(webrtc.ICEAddressRewriteRule{
			External: o.PublicIPs, AsCandidateType: webrtc.ICECandidateTypeSrflx,
		}); err != nil {
			return nil, err
		}
	}
	if len(o.ICEServers) > 0 {
		a.config.ICEServers = []webrtc.ICEServer{{URLs: o.ICEServers}}
	}
	a.api = webrtc.NewAPI(webrtc.WithSettingEngine(se))
	return a, nil
}

// closeWait bounds how long Close waits for connections to finish closing.
const closeWait = 2 * time.Second

// Close waits briefly for accepted connections that are already closing
// (e.g. by Server.Shutdown) to tell their peers, so browsers reconnect at
// once instead of waiting ~15s to notice, then releases the shared UDP port.
func (a *API) Close() error {
	a.mu.Lock()
	a.closing = true
	a.mu.Unlock()
	done := make(chan struct{})
	go func() { a.live.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(closeWait):
	}
	if a.mux != nil {
		return a.mux.Close()
	}
	return nil
}

// ClientConfig is served to browsers so they use the same ICE servers.
func (a *API) ClientConfig() map[string]any {
	servers := []map[string]any{}
	for _, s := range a.config.ICEServers {
		servers = append(servers, map[string]any{"urls": s.URLs})
	}
	return map[string]any{"iceServers": servers}
}

// Handler accepts signaling WebSockets and calls onConn with each data
// channel once it opens.
func (a *API) Handler(onConn func(*Conn)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := netkey.Of(clientIP(r))
		if !a.startSession(key) {
			http.Error(w, "too many connection attempts", http.StatusServiceUnavailable)
			return
		}
		defer a.endSession(key)
		// Any origin may connect: signaling grants nothing by itself, and
		// clients still authenticate over the data channel.
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
		if err != nil {
			return
		}
		defer ws.CloseNow()
		if err := a.answer(r.Context(), ws, onConn); err != nil {
			a.log.Debug("webrtc signaling failed", "remote", r.RemoteAddr, "err", err)
			ws.Close(websocket.StatusPolicyViolation, "signaling failed")
			return
		}
		ws.Close(websocket.StatusNormalClosure, "")
	})
}

// clientIP is the address of the client making r: the TCP peer, or, for a
// request from a reverse proxy on this machine, the last address the proxy
// added to X-Forwarded-For (earlier entries come from the client and could
// be anything).
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			last := xff[len(xff)-1]
			last = strings.TrimSpace(last[strings.LastIndex(last, ",")+1:])
			if net.ParseIP(last) != nil {
				return last
			}
		}
	}
	return host
}

// startSession counts a connection setup from key, unless that would pass
// the limits.
func (a *API) startSession(key string) bool {
	a.smu.Lock()
	defer a.smu.Unlock()
	if a.sessions >= a.maxSessions || a.byAddr[key] >= a.maxPerAddr {
		return false
	}
	a.sessions++
	a.byAddr[key]++
	return true
}

func (a *API) endSession(key string) {
	a.smu.Lock()
	defer a.smu.Unlock()
	a.sessions--
	if a.byAddr[key]--; a.byAddr[key] <= 0 {
		delete(a.byAddr, key)
	}
}

const signalTimeout = 30 * time.Second

func (a *API) answer(ctx context.Context, ws *websocket.Conn, onConn func(*Conn)) error {
	ctx, cancel := context.WithTimeout(ctx, signalTimeout)
	defer cancel()
	pc, err := a.api.NewPeerConnection(a.config)
	if err != nil {
		return err
	}
	opened := make(chan *Conn, 1)
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != Label {
			dc.Close()
			return
		}
		dc.OnOpen(func() {
			a.mu.Lock()
			closing := a.closing
			if !closing {
				a.live.Add(1)
			}
			a.mu.Unlock()
			if closing {
				go pc.Close()
				return
			}
			select {
			case opened <- newConn(pc, dc, Auto, a.live.Done):
			default:
			}
		})
	})
	sig := &signaler{ws: ws}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) { sig.candidate(ctx, c) })
	remote := remoteCandidates{keep: a.keep}

	errc := make(chan error, 1)
	go func() {
		for {
			m, err := readSignal(ctx, ws)
			if err != nil {
				errc <- err
				return
			}
			switch m.Type {
			case "offer":
				sdp, n, err := filterSDP(m.SDP, a.keep)
				if err != nil {
					errc <- err
					return
				}
				remote.mu.Lock()
				remote.count += n
				remote.mu.Unlock()
				if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}); err != nil {
					errc <- err
					return
				}
				if err := remote.described(pc); err != nil {
					errc <- err
					return
				}
				ans, err := pc.CreateAnswer(nil)
				if err == nil {
					err = pc.SetLocalDescription(ans)
				}
				if err == nil {
					err = sig.description(ctx, signal{Type: "answer", SDP: ans.SDP})
				}
				if err != nil {
					errc <- err
					return
				}
			case "candidate":
				if m.Candidate != nil {
					if err := remote.add(pc, *m.Candidate); err != nil {
						errc <- err
						return
					}
				}
			}
		}
	}()

	select {
	case c := <-opened:
		go onConn(c)
		return nil
	case err := <-errc:
		pc.Close()
		return err
	case <-ctx.Done():
		pc.Close()
		return ctx.Err()
	}
}

// Dial connects to a signaling URL (ws://host/rtc) and returns once the data
// channel is open.
func (a *API) Dial(ctx context.Context, url string, codec Codec) (*Conn, error) {
	if codec == Auto {
		return nil, errors.New("rtc: clients must choose Binary or JSON")
	}
	ctx, cancel := context.WithTimeout(ctx, signalTimeout)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return nil, err
	}
	defer ws.CloseNow()

	pc, err := a.api.NewPeerConnection(a.config)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Conn, error) {
		pc.Close()
		return nil, err
	}
	dc, err := pc.CreateDataChannel(Label, nil)
	if err != nil {
		return fail(err)
	}
	opened := make(chan struct{})
	var openOnce sync.Once
	dc.OnOpen(func() { openOnce.Do(func() { close(opened) }) })
	conn := newConn(pc, dc, codec, nil)

	sig := &signaler{ws: ws}
	pc.OnICECandidate(func(c *webrtc.ICECandidate) { sig.candidate(ctx, c) })
	var remote remoteCandidates

	offer, err := pc.CreateOffer(nil)
	if err == nil {
		err = pc.SetLocalDescription(offer)
	}
	if err == nil {
		err = sig.description(ctx, signal{Type: "offer", SDP: offer.SDP})
	}
	if err != nil {
		return fail(err)
	}

	errc := make(chan error, 1)
	answered := make(chan struct{})
	go func() {
		for {
			m, err := readSignal(ctx, ws)
			if err != nil {
				select {
				case <-answered:
					// The server closes signaling once its side of the data
					// channel opens, which can happen before ours does.
					return
				default:
				}
				errc <- err
				return
			}
			switch m.Type {
			case "answer":
				err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: m.SDP})
				if err == nil {
					err = remote.described(pc)
				}
				if err == nil {
					close(answered)
				}
			case "candidate":
				if m.Candidate != nil {
					err = remote.add(pc, *m.Candidate)
				}
			}
			if err != nil {
				errc <- err
				return
			}
		}
	}()

	select {
	case <-opened:
		ws.Close(websocket.StatusNormalClosure, "")
		return conn, nil
	case err := <-errc:
		conn.Close()
		return nil, err
	case <-ctx.Done():
		conn.Close()
		return nil, ctx.Err()
	}
}
