// Command botclient connects a headless bot to a server and plays many
// concurrent games.
//
//	go run ./cmd/botclient -user alice -opponent heuristic -games 1000 -concurrent 100
//	go run ./cmd/botclient -user bob -games 1000 -concurrent 100    # matchmaking
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"cribbage/internal/botclient"
)

func main() {
	var cfg botclient.Config
	flag.StringVar(&cfg.Addr, "addr", "localhost:7777", "server TCP address")
	flag.StringVar(&cfg.Username, "user", "bot1", "username (registered on first use)")
	flag.StringVar(&cfg.Password, "pass", "botpassword", "password")
	flag.StringVar(&cfg.Nickname, "nick", "", "nickname (defaults to username)")
	flag.StringVar(&cfg.Bot, "bot", "heuristic", "local bot that makes decisions")
	flag.StringVar(&cfg.Opponent, "opponent", "", "server-side bot opponent; empty uses matchmaking")
	flag.IntVar(&cfg.Concurrent, "concurrent", 10, "games in flight at once")
	flag.IntVar(&cfg.Games, "games", 100, "games to finish (0 = until interrupted)")
	flag.Uint64Var(&cfg.Seed, "seed", 1, "seed for the local bot")
	flag.Parse()

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
	fmt.Printf("%s: finished %d games in %v (%.0f games/s), won %d (%.1f%%), resigned %d, errors %d\n",
		cfg.Username, st.Finished, st.Elapsed.Round(1e6), float64(st.Finished)/st.Elapsed.Seconds(),
		st.Won, rate, st.Resigned, st.Errors)
	if err != nil || st.Errors > 0 {
		os.Exit(1)
	}
}
