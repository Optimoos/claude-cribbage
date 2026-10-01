# Cribbage: Go Server + Neural Network AI + Godot C# Client

## Context
This is a new project; `claude-cribbage` is currently empty. The goal is a two-player cribbage game with standard rules and auto-counted scoring. It has three parts:
- A fast Go server that runs many concurrent games. It's also the training ground for an AI opponent.
- A neural network AI in Go that sees only the visible state, learns through self-play, and saves models to disk.
- A Godot 4 C# client that connects over WebRTC. It ships as a **desktop build** because Godot 4 can't export C# projects to the web. A plain HTML/JS test client covers browser play and debugging.

Decisions confirmed with you:
- C# with desktop export.
- Username and password login, plus a separate nickname.
- The server auto-counts all scores.
- The network is a hand-rolled MLP running on CPU in pure Go.

**Environment notes:** Go 1.23.4 is installed. The .NET SDK is **not** installed. You'll need the .NET 8 SDK and Godot 4.x .NET before milestone 8.

## Architecture

```
/proto            messages.proto: the shared message schema (protobuf)
/server           Go module
  cmd/cribserver  the server binary
  cmd/sim         runs bot-vs-bot games in bulk and reports stats
  cmd/train       neural network self-play trainer
  cmd/botclient   headless bot that connects over TCP
  internal/engine   pure, deterministic rules engine (no I/O)
  internal/bots     Bot interface; random, heuristic, and NN bots
  internal/ai/nn    MLP: forward, backprop, Adam, save/load
  internal/ai/encode  turns an observation into a feature vector
  internal/ai/train   TD(λ) self-play loop, checkpoints, arena
  internal/lobby    game manager: one goroutine (actor) per game
  internal/session  auth, tokens, player connections
  internal/store    SQLite (modernc.org/sqlite, pure Go with no cgo)
  internal/transport/{tcp,webrtc,codec}
  web/              HTML/JS test client, served by the server
/client-godot     Godot 4 .NET project
```

**Core design points**
- **The engine is a pure state machine:**
  - `NewGame(seed)` starts a game.
  - `Legal(p)` returns a player's legal actions.
  - `Apply(p, action)` advances the game.
  - `Observation(p)` returns what that player can see. The opponent's hand and the crib stay hidden until they're shown.

  The engine also emits events (dealt, played, scored with breakdown, go, show, game over). The server, bots, and trainer all use the same engine.
- **Determinism makes persistence and resume simple.** Each game stores its RNG seed and an action log. On restart, the server replays the log to rebuild the exact state. It also writes snapshots every N actions to bound replay time.
- **One protocol, several transports.** Messages are defined once in protobuf. Each client picks a codec at handshake: binary protobuf (bots, Godot) or protojson (HTML client).
  - **WebRTC:** pion/webrtc v4 with one reliable, ordered DataChannel. Signaling (SDP and ICE exchange) runs over an authenticated WebSocket.
  - **TCP:** length-prefixed protobuf frames with no handshake overhead beyond the token. This is for headless bots.
- **Multiple games per player.** A connection can join many games. Each message carries a `game_id`, and each game's actor goroutine owns its own state, so there's no global lock.
- **Training runs in-process.** The trainer drives the engine directly, which is far faster than going through a network. The TCP path still lets external bots play many concurrent games against the server, both for load testing and for distributed play.

## Milestones (build and verify one at a time)

### M1: Rules engine
- Cards, deck with a seeded RNG, cutting for first deal.
- Discard to the crib.
- The starter card, including two points for his heels when it's a jack.
- Pegging scores: 15s, 31, pairs and pair royals, runs, go, last card.
- The show, in order: non-dealer's hand, dealer's hand, then crib. Scores fifteens, pairs, runs, flushes (four-card flush counts in the hand but not the crib), and nobs.
- Win at 121, checked right away, including during pegging and mid-show.
- Optionally track skunks and the game-over reason.

**Verify:** run `go test ./internal/engine/...`.
- Table tests for known hands: the 29 hand, 28, 24 with double runs, a flush in hand versus in the crib, nobs, and zero hands.
- Pegging edge cases: go handling, 31 reset, last card.
- A fuzz/property test that plays 1M random games and checks invariants: 52 unique cards, scores only increase, the game ends exactly once.
- Benchmark target: at least 50k random games/sec on one core.

### M2: Baseline bots + simulator
- A `Bot` interface with two baselines:
  - **RandomBot.**
  - **HeuristicBot:** discards to maximize expected hand value plus crib value over all possible starters, and pegs greedily while avoiding easy 15s and 31s for the opponent.
- `cmd/sim -a heuristic -b random -n 100000 -workers N` reports win rate, average points, and games/sec.

**Verify:** the heuristic bot beats the random bot by a wide margin (at least 90%), and throughput scales with the number of workers.

### M3: Server core + TCP bot transport
- Protobuf schema with:
  - Auth messages: register, login, and resume token.
  - Lobby messages: create a game against a human or a bot type, list games, join.
  - Game messages: action, state/observation, events, error.
- Lobby and game actors.
- Auth with bcrypt hashes, opaque session tokens, and a nickname. It sits behind a `Store` interface, in-memory for now.
- TCP transport.
- `cmd/botclient` logs in and plays K concurrent games.

**Verify:**
- Integration test: two botclients play 1,000 concurrent games through the server, and every game finishes with legal results.
- A load test reports games/sec and memory.
- A cheating test: the server rejects illegal or out-of-turn actions, and an observation never leaks the opponent's hand.

### M4: Persistence + resume
- SQLite store with these tables:
  - `users`
  - `sessions`
  - `games`: seed, players, status
  - `game_actions`: the action log
  - `snapshots`
- Write-ahead: an action is persisted before it's broadcast.
- On startup, the server loads every active game and rebuilds its actor by replaying.
- If a client disconnects, its games pause (opponents are notified). Reconnecting with the token re-subscribes it and sends a full observation for each game.

**Verify:**
- Kill the server mid-game with botclients running, then restart it. The botclients reconnect and every game completes.
- A test asserts that replaying a game reproduces exactly the same state.

### M5: WebRTC transport + HTML test client
- WebSocket signaling endpoint.
- pion PeerConnection and DataChannel, adapted onto the same connection interface as TCP.
- STUN config, with an optional TURN setting for later.
- The server serves `web/` with:
  - Login and register.
  - A lobby: create against a human or a bot, plus a list of my games.
  - A minimal card-table view that renders the observation and the event log.

**Verify:**
- Two browser tabs play a full game against each other.
- One tab plays the heuristic bot.
- Refreshing the page resumes the game.
- Restarting the server also resumes it.
- One user can run several games at once.

### M6: Neural network AI + self-play training
- **Encoding** uses only visible state:
  - Own hand: 52 one-hot.
  - Starter: 52 one-hot.
  - Cards in the current pegging sequence, plus all cards played this hand (own versus opponent's).
  - Both scores, normalized and as distance to 121.
  - Dealer flag, phase, current count.
  - At the show, the opponent's hand and the crib once they're revealed.
- **Model:** an MLP (for example, inputs → 256 → 128 → 1) that estimates win probability. Action selection evaluates each legal action's afterstate, the TD-Gammon approach:
  - 15 discard choices.
  - Up to 4 pegging plays.
- **Training:** TD(λ) or Monte Carlo targets from game outcomes.
  - ε-greedy or softmax exploration, decaying over time.
  - N worker goroutines play self-play games.
  - Gradient batches go to a central learner (Adam), and weights are periodically re-broadcast to the workers.
  - Optionally, games against frozen past checkpoints and the heuristic bot help avoid collapse.
- **Model file:** a versioned binary with a header (architecture, encoding version, training step, metrics). Checkpoints go to `models/`.
- **Arena:** `cmd/train eval` scores a model against the random bot, the heuristic bot, and earlier checkpoints.

**Verify:**
- A finite-difference gradient check test.
- A save/load round-trip test gives identical outputs.
- Training curves are logged to CSV and stdout.
- Win rate against the heuristic bot rises past 50% and keeps improving. That's the key success metric.

### M7: NN bot in the server
- The server loads a model by name (`-model models/best.bin`) and offers an "NN" opponent in the lobby, alongside random and heuristic.
- An optional server-side training mode runs NN-vs-NN games through the lobby and actors. External TCP bots can join training pools.

**Verify:** play against the NN bot from the HTML client. Check that the server's NN bot matches in-process arena results over 10k games.

### M8: Godot 4 C# desktop client
- **Prerequisites:** the .NET 8 SDK, Godot 4.x .NET, and the **webrtc-native GDExtension** (desktop Godot needs it for WebRTC).
- Google.Protobuf NuGet, with C# classes generated from `/proto`.
- Scenes: login, lobby, and a 2D table showing hands, crib, starter, a pegging area, a cribbage board with pegs, and a score log.
- The client reconnects automatically with a stored token.

**Verify:**
- Desktop Godot plays a full game against the bot.
- Godot plays against an HTML-client human.
- Killing the client and reopening it resumes the game.

## Later (out of scope now)
- Web export for Godot once .NET web support lands, or a GDScript port.
- Matchmaking and ratings.
- TURN deployment.
- Manual counting with muggins.
