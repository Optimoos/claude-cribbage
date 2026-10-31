package rtc

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"

	"cribbage/internal/protocol/pb"
)

func newTestAPI(t *testing.T) *API {
	t.Helper()
	a, err := NewAPI(Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// TestCodecMirroring checks that a server-side Auto connection answers in
// whichever encoding the client used.
func TestCodecMirroring(t *testing.T) {
	server := newTestAPI(t)
	ts := httptest.NewServer(server.Handler(func(c *Conn) {
		defer c.Close()
		for {
			var m pb.ClientMessage
			if err := c.Read(&m); err != nil {
				return
			}
			c.Write(&pb.ServerMessage{RequestId: m.RequestId, Body: &pb.ServerMessage_Pong{Pong: &pb.Pong{ClientTime: m.GetPing().GetClientTime()}}})
		}
	}))
	defer ts.Close()
	url := "ws" + strings.TrimPrefix(ts.URL, "http")

	client := newTestAPI(t)
	for _, codec := range []Codec{JSON, Binary} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		c, err := client.Dial(ctx, url, codec)
		cancel()
		if err != nil {
			t.Fatalf("codec %v: dial: %v", codec, err)
		}
		if err := c.Write(&pb.ClientMessage{RequestId: 7, Body: &pb.ClientMessage_Ping{Ping: &pb.Ping{ClientTime: 42}}}); err != nil {
			t.Fatal(err)
		}
		select {
		case in := <-c.in:
			if in.text != (codec == JSON) {
				t.Fatalf("codec %v: reply text=%v", codec, in.text)
			}
			if codec == JSON && !strings.Contains(string(in.data), `"pong"`) {
				t.Fatalf("unexpected JSON reply %s", in.data)
			}
			c.in <- in // let Read decode it
			var m pb.ServerMessage
			if err := c.Read(&m); err != nil || m.RequestId != 7 || m.GetPong().GetClientTime() != 42 {
				t.Fatalf("codec %v: decoded %v, %v", codec, &m, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("codec %v: no reply", codec)
		}
		c.Close()
	}
}

// The server sees the client's address on the candidate pair in use.
func TestRemoteIP(t *testing.T) {
	server := newTestAPI(t)
	ips := make(chan string, 1)
	ts := httptest.NewServer(server.Handler(func(c *Conn) {
		ips <- c.RemoteIP()
		c.Close()
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := newTestAPI(t).Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), Binary)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case ip := <-ips:
		if net.ParseIP(ip) == nil {
			t.Fatalf("RemoteIP() = %q, want an IP address", ip)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no connection")
	}
}

// A message over the advertised limit is refused.
func TestMessageSizeLimit(t *testing.T) {
	ts := httptest.NewServer(newTestAPI(t).Handler(func(c *Conn) {
		var m pb.ClientMessage
		for c.Read(&m) == nil {
		}
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := newTestAPI(t).Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), Binary)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.dc.Send(make([]byte, maxMessage+1)); err == nil {
		t.Fatal("sent a message over the limit")
	}
	if err := c.dc.Send(make([]byte, 1000)); err != nil {
		t.Fatalf("small message: %v", err)
	}
}

func TestDialRejectsAuto(t *testing.T) {
	if _, err := newTestAPI(t).Dial(context.Background(), "ws://unused", Auto); err == nil {
		t.Fatal("Auto accepted for a client")
	}
}

func TestCandidateFilter(t *testing.T) {
	for cand, want := range map[string]bool{
		"candidate:1 1 udp 2122260223 8.8.8.8 54321 typ host":                                    true,
		"a=candidate:1 1 udp 2122260223 2a00:1450:4001::1 54321 typ host":                        true,
		"candidate:2 1 udp 1686052607 1.1.1.1 61000 typ srflx raddr 192.168.1.5 rport 54321":     true,
		"candidate:1 1 udp 2122260223 192.168.1.5 54321 typ host":                                false,
		"candidate:1 1 udp 2122260223 10.0.0.7 54321 typ host":                                   false,
		"candidate:1 1 udp 2122260223 127.0.0.1 54321 typ host":                                  false,
		"candidate:1 1 udp 2122260223 169.254.169.254 80 typ host":                               false,
		"candidate:1 1 udp 2122260223 100.64.1.1 54321 typ host":                                 false,
		"candidate:1 1 udp 2122260223 ::1 54321 typ host":                                        false,
		"candidate:1 1 udp 2122260223 fe80::1 54321 typ host":                                    false,
		"candidate:1 1 udp 2122260223 fd00::1 54321 typ host":                                    false,
		"candidate:1 1 udp 2122260223 0.0.0.0 54321 typ host":                                    false,
		"candidate:1 1 udp 2122260223 0b3f5ed1-7b7d-4b8e-a3d4-0123456789ab.local 54321 typ host": false,
		"candidate:garbage": false,
	} {
		if got := publicAddress(candidateAddress(cand)); got != want {
			t.Errorf("%q: public %v, want %v", cand, got, want)
		}
	}

	sdp := "v=0\r\n" +
		"a=candidate:1 1 udp 2122260223 192.168.1.5 54321 typ host\r\n" +
		"a=candidate:2 1 udp 1686052607 8.8.8.8 61000 typ srflx raddr 192.168.1.5 rport 54321\r\n" +
		"a=end-of-candidates\r\n"
	got, n, err := filterSDP(sdp, publicAddress)
	if err != nil || n != 2 || strings.Contains(got, "192.168.1.5 54321 typ host") || !strings.Contains(got, "8.8.8.8") ||
		!strings.HasPrefix(got, "v=0\r\n") || !strings.HasSuffix(got, "a=end-of-candidates\r\n") {
		t.Fatalf("filtered SDP %q, %d, %v", got, n, err)
	}
	if got, _, _ := filterSDP(sdp, nil); got != sdp {
		t.Fatalf("SDP changed without a filter: %q", got)
	}
	if _, _, err := filterSDP(strings.Repeat("a=candidate:1 1 udp 1 8.8.8.8 1 typ host\r\n", maxCandidates+1), nil); err == nil {
		t.Fatal("accepted too many SDP candidates")
	}

	r := remoteCandidates{keep: publicAddress}
	for _, c := range []string{"candidate:1 1 udp 1 10.1.2.3 1 typ host", "candidate:1 1 udp 1 8.8.4.4 1 typ host"} {
		if err := r.add(nil, webrtc.ICECandidateInit{Candidate: c}); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.pending) != 1 || !strings.Contains(r.pending[0].Candidate, "8.8.4.4") {
		t.Fatalf("kept %v", r.pending)
	}
	var err2 error
	for i := 0; i < maxCandidates && err2 == nil; i++ {
		err2 = r.add(nil, webrtc.ICECandidateInit{Candidate: "candidate:1 1 udp 1 10.1.2.3 1 typ host"})
	}
	if err2 == nil {
		t.Fatal("accepted too many trickled candidates")
	}
}

func TestClientIP(t *testing.T) {
	for _, c := range []struct {
		remote string
		xff    []string
		want   string
	}{
		{"198.51.100.7:4000", nil, "198.51.100.7"},
		{"198.51.100.7:4000", []string{"1.2.3.4"}, "198.51.100.7"}, // not from a local proxy: ignored
		{"127.0.0.1:4000", nil, "127.0.0.1"},
		{"127.0.0.1:4000", []string{"9.9.9.9, 1.2.3.4"}, "1.2.3.4"},
		{"127.0.0.1:4000", []string{"9.9.9.9", "1.2.3.4"}, "1.2.3.4"},
		{"[::1]:4000", []string{"2001:db8::5"}, "2001:db8::5"},
		{"127.0.0.1:4000", []string{"not-an-ip"}, "127.0.0.1"},
	} {
		r := httptest.NewRequest("GET", "/rtc", nil)
		r.RemoteAddr = c.remote
		for _, v := range c.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := clientIP(r); got != c.want {
			t.Errorf("%s %v: %q, want %q", c.remote, c.xff, got, c.want)
		}
	}
}

// Connection setups in progress are limited per address.
func TestSessionLimit(t *testing.T) {
	a, err := NewAPI(Options{MaxSessionsPerAddr: 1, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(a.Handler(func(c *Conn) { c.Close() }))
	defer ts.Close()
	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, _, err := websocket.Dial(ctx, url, nil) // never sends an offer
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // let the handler count it
	if _, resp, err := websocket.Dial(ctx, url, nil); err == nil || resp == nil || resp.StatusCode != 503 {
		t.Fatalf("second setup from one address: %v, %v", resp, err)
	}
	first.CloseNow()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, _, err := websocket.Dial(ctx, url, nil)
		if err == nil {
			c.CloseNow()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("setup refused after the first ended: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
