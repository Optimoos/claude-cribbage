package rtc

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestDialRejectsAuto(t *testing.T) {
	if _, err := newTestAPI(t).Dial(context.Background(), "ws://unused", Auto); err == nil {
		t.Fatal("Auto accepted for a client")
	}
}
