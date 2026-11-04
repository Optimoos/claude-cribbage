// Command botclient connects a headless bot to a server and plays many
// concurrent games.
//
//	go run ./cmd/botclient -user alice -opponent heuristic -games 1000 -concurrent 100
//	go run ./cmd/botclient -user bob -games 1000 -concurrent 100    # matchmaking
//	go run ./cmd/botclient -user carol -bot nn -model models/best.bin -opponent heuristic
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"cribbage/internal/ai"
	"cribbage/internal/botclient"
)

func main() {
	var cfg botclient.Config
	flag.StringVar(&cfg.Addr, "addr", "localhost:7777", "server address: TCP host:port, or ws://host:port/rtc for WebRTC")
	flag.StringVar(&cfg.Username, "user", "bot1", "bot account username (created on first use)")
	keyFile := flag.String("key-file", "", "file holding the server's bot key (cribserver -bot-key-file), or set CRIBBAGE_BOT_KEY")
	flag.StringVar(&cfg.Nickname, "nick", "", "nickname (defaults to username)")
	flag.StringVar(&cfg.Bot, "bot", "heuristic", "local bot that makes decisions")
	flag.StringVar(&cfg.Opponent, "opponent", "", "server-side bot opponent; empty uses matchmaking")
	flag.IntVar(&cfg.Concurrent, "concurrent", 10, "games in flight at once")
	flag.IntVar(&cfg.Games, "games", 100, "games to finish (0 = until interrupted)")
	flag.Uint64Var(&cfg.Seed, "seed", 1, "seed for the local bot")
	flag.DurationVar(&cfg.Reconnect, "reconnect", 30*time.Second, "how long to keep retrying a lost connection (negative disables)")
	model := flag.String("model", "", "neural network model, playable locally with -bot nn")
	flag.Parse()
	cfg.Key = os.Getenv("CRIBBAGE_BOT_KEY")
	if *keyFile != "" {
		b, err := os.ReadFile(*keyFile)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		cfg.Key = strings.TrimSpace(string(b))
	}
	if cfg.Key == "" {
		fmt.Fprintln(os.Stderr, "error: bots sign in with the server's bot key: pass -key-file or set CRIBBAGE_BOT_KEY")
		os.Exit(2)
	}

	if *model != "" {
		if _, err := ai.Register(*model); err != nil {
			fmt.Fprintln(os.Stderr, "error: load model:", err)
			os.Exit(1)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	st, err := botclient.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	rate := 0.0
	if st.Finished > 0 {
		rate = 100 * float64(st.Won) / float64(st.Finished)
	}
	fmt.Printf("%s: finished %d games in %v (%.0f games/s), won %d (%.1f%%), resigned %d, lost %d, reconnects %d, errors %d\n",
		cfg.Username, st.Finished, st.Elapsed.Round(time.Millisecond), float64(st.Finished)/st.Elapsed.Seconds(),
		st.Won, rate, st.Resigned, st.Lost, st.Reconnects, st.Errors)
	if err != nil || st.Errors > 0 {
		os.Exit(1)
	}
}
