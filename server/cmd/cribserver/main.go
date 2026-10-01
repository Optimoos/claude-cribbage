// Command cribserver runs the cribbage game server.
package main

import (
	"context"
	"flag"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"time"

	"golang.org/x/crypto/bcrypt"

	"cribbage/internal/auth"
	"cribbage/internal/server"
	"cribbage/internal/store"
)

func main() {
	tcpAddr := flag.String("tcp", ":7777", "TCP listen address for headless clients")
	dbPath := flag.String("db", "cribbage.db", "SQLite database file; empty keeps everything in memory")
	statsEvery := flag.Duration("stats", 10*time.Second, "log statistics at this interval (0 disables)")
	bcryptCost := flag.Int("bcrypt-cost", bcrypt.DefaultCost, "bcrypt cost for password hashes")
	pprofAddr := flag.String("pprof", "", "serve net/http/pprof on this address (e.g. localhost:6060)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	fail := func(msg string, err error) {
		log.Error(msg, "err", err)
		os.Exit(1)
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
	srv.Shutdown()
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
