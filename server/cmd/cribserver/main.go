// Command cribserver runs the cribbage game server.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"cribbage/internal/ai"
	"cribbage/internal/auth"
	"cribbage/internal/bots"
	"cribbage/internal/expert"
	"cribbage/internal/server"
	"cribbage/internal/store"
	"cribbage/internal/transport/rtc"
	"cribbage/web"
)

func main() {
	tcpAddr := flag.String("tcp", ":7777", "TCP listen address for headless clients")
	httpAddr := flag.String("http", ":8080", "HTTP address for the web client and WebRTC signaling (empty disables)")
	webDir := flag.String("web-dir", "", "serve the web client from this directory instead of the embedded copy")
	rtcPort := flag.Int("rtc-port", 7778, "UDP port for all WebRTC traffic (0 picks a port per connection)")
	publicIPs := flag.String("public-ip", "", "comma-separated public IPs to advertise for WebRTC (servers behind 1:1 NAT)")
	iceServers := flag.String("ice-servers", "", "comma-separated STUN/TURN URLs for WebRTC, e.g. stun:stun.l.google.com:19302")
	dbPath := flag.String("db", "cribbage.db", "SQLite database file; empty keeps everything in memory")
	statsEvery := flag.Duration("stats", 10*time.Second, "log statistics at this interval (0 disables)")
	bcryptCost := flag.Int("bcrypt-cost", bcrypt.DefaultCost, "bcrypt cost for password hashes")
	pprofAddr := flag.String("pprof", "", "serve net/http/pprof on this address (e.g. localhost:6060)")
	model := flag.String("model", "", "neural network model to offer as the \"nn\" bot (e.g. models/best.bin)")
	expertPath := flag.String("expert", "", "expert bundle to offer as the \"expert\" bots (e.g. models-expert/expert.bin)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fail := func(msg string, err error) {
		log.Error(msg, "err", err)
		os.Exit(1)
	}

	if *model != "" {
		meta, err := ai.Register(*model)
		if err != nil {
			fail("load model", err)
		}
		log.Info("loaded model", "path", *model, "bot", ai.BotName, "sizes", meta.Sizes, "games", meta.Games,
			"vs_heuristic", meta.Metrics["vs_heuristic"])
	}

	if *expertPath != "" {
		b, err := expert.LoadBundle(*expertPath)
		if err != nil {
			fail("load expert bundle", err)
		}
		expert.Register(b)
		names := make([]string, len(expert.Levels))
		for i, l := range expert.Levels {
			names[i] = l.Name
		}
		log.Info("loaded expert bundle", "path", *expertPath, "saved", b.Meta.Saved, "bots", strings.Join(names, ","),
			"discard_tables", b.Discard != nil, "opponent_model", b.KeepTemp[0] > 0)
	}

	var users store.Store
	var games store.GameStore
	var sq *store.SQLite
	if *dbPath == "" {
		m := store.NewMemory()
		users, games = m, m
		log.Warn("no database: users and games are lost on restart")
	} else {
		var err error
		if sq, err = store.OpenSQLite(*dbPath); err != nil {
			fail("open database", err)
		}
		users, games = sq, sq
	}

	srv := server.New(server.Config{
		Auth:   &auth.Service{Store: users, BcryptCost: *bcryptCost},
		Games:  games,
		Logger: log,
	})
	start := time.Now()
	n, err := srv.Restore(context.Background())
	if err != nil {
		fail("restore games", err)
	}
	log.Info("restored games", "count", n, "took", time.Since(start).Round(time.Millisecond))

	ln, err := net.Listen("tcp", *tcpAddr)
	if err != nil {
		fail("listen", err)
	}
	log.Info("listening", "tcp", ln.Addr().String(), "db", *dbPath)
	go func() {
		if err := srv.ServeTCP(ln); err != nil {
			fail("serve", err)
		}
	}()

	var httpSrv *http.Server
	var rtcAPI *rtc.API
	if *httpAddr != "" {
		rtcAPI, err = rtc.NewAPI(rtc.Options{
			UDPPort:    *rtcPort,
			PublicIPs:  splitList(*publicIPs),
			ICEServers: splitList(*iceServers),
			Logger:     log,
		})
		if err != nil {
			fail("webrtc", err)
		}
		mux := http.NewServeMux()
		mux.Handle("/rtc", rtcAPI.Handler(func(c *rtc.Conn) { srv.ServeConn(c) }))
		mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(rtcAPI.ClientConfig())
		})
		mux.HandleFunc("/api/bots", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(bots.Names())
		})
		mux.Handle("/", web.Handler(*webDir))
		httpSrv = &http.Server{Addr: *httpAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		hl, err := net.Listen("tcp", *httpAddr)
		if err != nil {
			fail("listen http", err)
		}
		log.Info("web client", "url", "http://"+displayAddr(hl.Addr()), "webrtc_udp", *rtcPort)
		go func() {
			if err := httpSrv.Serve(hl); err != nil && err != http.ErrServerClosed {
				fail("serve http", err)
			}
		}()
	}

	if *pprofAddr != "" {
		go func() { log.Error("pprof", "err", http.ListenAndServe(*pprofAddr, nil)) }()
	}
	if *statsEvery > 0 {
		go logStats(log, srv, sq, *statsEvery)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	log.Info("shutting down")
	ln.Close()
	if httpSrv != nil {
		httpSrv.Close()
	}
	srv.Shutdown()
	if rtcAPI != nil {
		rtcAPI.Close()
	}
	if sq != nil {
		if err := sq.Close(); err != nil {
			fail("close database", err)
		}
	}
}

func logStats(log *slog.Logger, srv *server.Server, sq *store.SQLite, every time.Duration) {
	prev := srv.Stats()
	var prevBatches, prevOps int64
	for range time.Tick(every) {
		s := srv.Stats()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		secs := every.Seconds()
		attrs := []any{
			"conns", s.Connections,
			"active_games", s.ActiveGames,
			"finished", s.GamesFinished,
			"games_per_sec", float64(s.GamesFinished-prev.GamesFinished) / secs,
			"actions_per_sec", float64(s.Actions-prev.Actions) / secs,
			"goroutines", runtime.NumGoroutine(),
			"heap_mb", ms.HeapAlloc >> 20,
		}
		if sq != nil {
			b, o := sq.WriteStats()
			if b > prevBatches {
				attrs = append(attrs, "db_tx_per_sec", float64(b-prevBatches)/secs,
					"db_writes_per_tx", float64(o-prevOps)/float64(b-prevBatches))
			}
			prevBatches, prevOps = b, o
		}
		log.Info("stats", attrs...)
		prev = s
	}
}

func splitList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

// displayAddr turns ":8080" style listen addresses into a usable URL host.
func displayAddr(a net.Addr) string {
	host, port, err := net.SplitHostPort(a.String())
	if err != nil || host == "" || host == "::" || host == "0.0.0.0" {
		return "localhost:" + port
	}
	return a.String()
}
