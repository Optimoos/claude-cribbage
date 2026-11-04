package server

import (
	"context"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"cribbage/internal/botclient"
	"cribbage/internal/client"
	"cribbage/internal/protocol/pb"
	"cribbage/internal/transport/rtc"
)

// startRTC serves WebRTC signaling for srv and returns the ws:// URL.
func startRTC(t *testing.T, srv *Server) string {
	t.Helper()
	api, err := rtc.NewAPI(rtc.Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/rtc", api.Handler(func(c *rtc.Conn) { srv.ServeConn(c) }))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/rtc"
}

// TestWebRTCGameJSON plays a whole game over WebRTC with the JSON encoding
// the browser client uses.
func TestWebRTCGameJSON(t *testing.T) {
	srv, _ := startServer(t)
	url := startRTC(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := client.DialWebRTC(ctx, url, rtc.JSON)
	if err != nil {
		t.Fatal(err)
	}
	tc := &tclient{Client: c, msgs: make(chan *pb.ServerMessage, 1024)}
	t.Cleanup(func() { c.Close() })
	if tc.auth, err = c.BotLogin("webby", "Web Player", testBotKey); err != nil {
		t.Fatal(err)
	}
	tc.startReading()

	id := tc.send(t, &pb.ClientMessage{Body: &pb.ClientMessage_CreateGame{CreateGame: &pb.CreateGame{Bot: "heuristic"}}})
	u := tc.next(t, replyTo(id)).GetGameUpdate()
	r := rand.New(rand.NewPCG(3, 4))
	for u.Game.Status == pb.GameStatus_GAME_STATUS_ACTIVE {
		if len(u.Legal) == 0 {
			t.Fatalf("no legal moves at seq %d", u.Seq)
		}
		id = tc.send(t, act(u.Game.GameId, u.Legal[r.IntN(len(u.Legal))]))
		u = tc.next(t, replyTo(id)).GetGameUpdate()
		if u == nil {
			t.Fatal("expected an update")
		}
	}
	if u.Game.Status != pb.GameStatus_GAME_STATUS_FINISHED || u.Game.Winner < 0 {
		t.Fatalf("game did not finish: %v", u.Game)
	}
}

// TestMixedTransports matches a TCP bot client against a WebRTC bot client.
func TestMixedTransports(t *testing.T) {
	srv, tcpAddr := startServer(t)
	url := startRTC(t, srv)
	var wg sync.WaitGroup
	results := make([]botclient.Stats, 3)
	errs := make([]error, 3)
	cfgs := []botclient.Config{
		{Addr: tcpAddr, Username: "tcpbot"},
		{Addr: url, Username: "rtcbot", Seed: 1},
		{Addr: url, Username: "rtcsolo", Opponent: "heuristic"},
	}
	for i, cfg := range cfgs {
		cfg.Key, cfg.Bot, cfg.Concurrent, cfg.Games = testBotKey, "heuristic", 50, 300
		wg.Add(1)
		go func(i int, cfg botclient.Config) {
			defer wg.Done()
			results[i], errs[i] = botclient.Run(context.Background(), cfg)
		}(i, cfg)
	}
	wg.Wait()
	for i, r := range results {
		if errs[i] != nil {
			t.Fatalf("%s: %v", cfgs[i].Username, errs[i])
		}
		t.Logf("%s: %+v", cfgs[i].Username, r)
		if r.Finished != 300 || r.Errors != 0 {
			t.Errorf("%s: %+v", cfgs[i].Username, r)
		}
	}
	if results[0].Won+results[1].Won != 300 {
		t.Errorf("tcp and rtc wins don't add up: %d + %d", results[0].Won, results[1].Won)
	}
}
