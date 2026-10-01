// Command cribserver runs the cribbage game server.
package main

import (
	"flag"
	"log/slog"
	"net"
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
	statsEvery := flag.Duration("stats", 10*time.Second, "log statistics at this interval (0 disables)")
	bcryptCost := flag.Int("bcrypt-cost", bcrypt.DefaultCost, "bcrypt cost for password hashes")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	srv := server.New(server.Config{
		Auth:   &auth.Service{Store: store.NewMemory(), BcryptCost: *bcryptCost},
		Logger: log,
	})

	ln, err := net.Listen("tcp", *tcpAddr)
	if err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
	log.Info("listening", "tcp", ln.Addr().String())
	go func() {
		if err := srv.ServeTCP(ln); err != nil {
			log.Error("serve", "err", err)
			os.Exit(1)
		}
	}()

	if *statsEvery > 0 {
		go logStats(log, srv, *statsEvery)
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	<-sig
	log.Info("shutting down")
	ln.Close()
}

func logStats(log *slog.Logger, srv *server.Server, every time.Duration) {
	prev := srv.Stats()
	for range time.Tick(every) {
		s := srv.Stats()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		secs := every.Seconds()
		log.Info("stats",
			"conns", s.Connections,
			"active_games", s.ActiveGames,
			"finished", s.GamesFinished,
			"games_per_sec", float64(s.GamesFinished-prev.GamesFinished)/secs,
			"actions_per_sec", float64(s.Actions-prev.Actions)/secs,
			"goroutines", runtime.NumGoroutine(),
			"heap_mb", ms.HeapAlloc>>20,
		)
		prev = s
	}
}
