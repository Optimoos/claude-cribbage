# Cribbage

Two-player cribbage: a Go game server with a neural network AI, and a Godot
client. See [docs/PLAN.md](docs/PLAN.md) for the milestone plan.

## Server (`server/`, Go 1.27)

```sh
cd server
go test ./...                       # all tests
go run ./cmd/cribserver             # web client on http://localhost:8080, TCP for bots on :7777
go run ./cmd/cribserver -db ""      # in-memory only (nothing survives a restart)
go run ./cmd/cribserver -model models-ft/best.bin   # also offer the "nn" bot
go run ./cmd/cribserver -expert models-expert/expert.bin   # also offer the "expert" bots (three levels)
go run ./cmd/botclient -user alice -opponent heuristic -games 1000 -concurrent 100
go run ./cmd/botclient -user bob -games 1000      # matchmaking: run two of these
go run ./cmd/botclient -addr ws://localhost:8080/rtc -user carol -opponent random   # over WebRTC
go run ./cmd/botclient -user dave -bot nn -model models-ft/best.bin -opponent heuristic   # NN plays as the client
go run ./cmd/sim -a heuristic -b random -n 100000 # in-process bot matches
go run ./cmd/gauntlet -bot heuristic2 -vs heuristic -n 200000   # score-aware discards (M17) against the frozen heuristic
go generate ./internal/bots                       # rebuild heuristic2's self-play statistics (h2stats.go)
go run ./cmd/train run -out models                # train the neural network (Ctrl-C to stop)
go run ./cmd/train eval -model models/best.bin -vs heuristic -n 10000
go run ./cmd/gauntlet -bot models-ft/best.bin -vs random,heuristic -n 100000   # duplicate-deal table
go run ./cmd/expert build -bot models-big/best.bin -games 500000   # expert bundle (win table) from self-play
go run ./cmd/expert inspect models-expert/expert.bin
go run ./cmd/expert eval -bundle models-expert/expert.bin -seed 2       # calibration against fresh self-play
go run ./cmd/gauntlet -bot models-expert/expert.bin@100,nokeep -vs heuristic -n 20000   # expert; @effort plus options nokeep, nopeg, heurdiscard, pegtemp=X
go run ./cmd/expert iterate -start models-expert/expert.bin -rounds 3   # policy iteration: rebuild tables from self-play, keep if not worse
```

| Path | Purpose |
|---|---|
| `internal/engine` | Rules engine: pure, deterministic state machine |
| `internal/bots` | Bot interface, random, heuristic and heuristic2 bots, game runner |
| `internal/protocol` | Engine to protobuf conversions; `pb/` is generated |
| `internal/server` | Connections, auth, lobby, matchmaking, game actors |
| `internal/store` | Users, sessions and games: in-memory or SQLite |
| `internal/ai` | Neural network player (`Agent`), duplicate-deal `Arena` matches, decision timing |
| `internal/ai/nn` | MLP with sparse inputs, Adam, and the model file format |
| `internal/ai/encode` | Observation to features; afterstates |
| `internal/ai/train` | Self-play TD(λ) training |
| `internal/expert` | Expert AI: learned tables (win probability), the bundle file format, pegging search and the `expert` bot |
| `internal/players` | Player specs for commands: bot names, model files, bundles |
| `internal/transport/tcp` | Length-prefixed protobuf framing |
| `internal/transport/rtc` | WebRTC data channel transport and WebSocket signaling |
| `web/static` | HTML test client, embedded in the server binary |
| `internal/client`, `internal/botclient` | Go client and headless bot runner |

## Neural network AI

The network estimates the probability of winning from what one player can
see:
- the cards they hold and kept, their crib discards, and the starter
- cards played this hand by each side, and the current count and sequence
- both scores, plus dealer and phase flags
- a few hand-crafted features (more under Training):
  - their show value: exact once the starter is cut, otherwise averaged over possible starters
  - the expected crib value of their discards
  - the points their last play pegged
  - the opponent's expected points in reply

**Choosing a move.** The agent looks at the state each legal action leads to
(TD-Gammon style) and picks the one the network values most. That means 15
discard choices, or up to 4 pegging plays. Those states are computed only from
the player's own observation.

**Training** (`cmd/train run`):
- **Self-play:** actors play games against each other, with 10% against the
  heuristic bot by default. They use ε-greedy exploration, with ε decaying from
  0.1 to 0.02.
- **Targets:** every decision becomes a sample whose target is its TD(λ)
  return, with λ = 0.7.
- **Learning:** a learner draws minibatches from a replay buffer and trains
  with Adam, computing gradients in parallel.
- **Evaluation and checkpoints:** every 25k games, the current weights play
  the heuristic and random bots. The run then writes `ckpt-<games>.bin` and
  `latest.bin`, plus `best.bin` when the score against the heuristic
  improves, and appends a row to `train.csv`.
- **Resuming:** `-init models/latest.bin` continues an earlier run.

**Results so far** (win rate against the heuristic bot):
- Raw card features only: leveled off at about 40%.
- With the hand-crafted features (feature encoding v2): passed 40% within
  25k games and reached 48.3% ± 0.7% after about 1M games.
- Fine-tuned from that model for 1M more games at `-lr 3e-4`: 50.3% ± 0.7%
  over 20k games, level with the heuristic. It wins 99% against the random bot.

**In the server.** `cribserver -model <file>` registers the network as the
`nn` bot, which the web lobby then offers as "vs neural network bot".
`GET /api/bots` lists the bots a server offers. Every game creates its own
`Agent`, and all of them share one read-only copy of the weights. A move
takes well under a millisecond. Saved games against `nn` resume after a
restart only if the server is started with a model again. Otherwise they
are skipped and left in the database. Over 10k games through the server,
the `nn` bot won 50.3% against a heuristic botclient, matching the
arena result.

**Model files** start with a JSON header recording layer sizes, encoding
version, games, steps and evaluation metrics, followed by float32 weights.
A model trained with a different encoding version is rejected when loaded.

## Web client and WebRTC

Browse to `http://localhost:8080`, register, and play against a bot or another
browser tab. The page uses WebRTC. A WebSocket at `/rtc` exchanges the SDP
offer, answer and ICE candidates. After that, all traffic uses one reliable,
ordered data channel labelled `cribbage`.

The client is plain HTML, CSS and JavaScript in `web/static`, with no build
step:
- **Board:** a continuous 121-hole board drawn in SVG. Four runs of 30
  snake across it with both players' lanes side by side, and each player
  has their own game hole. Skunk lines are marked at 61 (SS) and 91 (S).
  Each player has a trailing peg. The trailing peg is tracked in the browser, so after a
  reload it starts out level with the front peg.
- **Animation:** each update's events are replayed one at a time. Pegs hop,
  cards slide in, points float up and the log scrolls, and then the table
  settles on the server's state. Tap the table to skip ahead.
- **Settings menu:**
  - Theme: card room, parchment or midnight.
  - **Enable rapid play:** apply updates instantly, with no animation or
    sound.
- **Sound:** effects are synthesized with Web Audio. The speaker button in
  the header mutes them.
- **Saved settings:** theme, rapid play and mute are kept in `localStorage`.
- **Phones:** the layout is responsive, and the lobby becomes a drawer
  behind the ☰ button.

- **Encoding:** text messages are protobuf JSON, as used by the browser; binary
  messages are protobuf, for Godot and Go clients. The server replies in the
  encoding the client used first.
- **UDP port:** all WebRTC traffic uses UDP port 7778 (`-rtc-port`). Open it in
  the firewall for remote players.
- **NAT:** behind a 1:1 NAT, such as a cloud VM, pass `-public-ip`.
  `-ice-servers` sets STUN/TURN servers, which are also given to browsers via
  `/api/config`.
- **Development:** `-web-dir web/static` serves the client from disk, so edits
  show up on reload without rebuilding.

## Persistence

The server keeps users, sessions and games in SQLite (`-db`, default
`cribbage.db`), using the pure-Go driver, so no cgo is needed.

- **Write-ahead.** Every move is committed before any client sees it. After a
  crash or restart, the server rebuilds each unfinished game by replaying its
  seed and action log, and clients resume with their session token.
- **Group commit.** One writer goroutine batches all queued writes into a
  single transaction, so thousands of concurrent games share each commit.
- **Action log.** Moves of unfinished games append to `action_log`. A
  finished game's moves are packed into `games.actions` (4 bytes per move).
  A periodic sweep deletes the log's dead prefix.
- **Exclusive access.** The database is opened in exclusive locking mode.
  Only one server process may use a database file at a time.

`-pprof localhost:6060` exposes Go's profiler for performance work.

## Protocol

The wire schema is `proto/cribbage/v1/cribbage.proto`. Regenerate Go code from
the repo root with [buf](https://buf.build) and `protoc-gen-go` (keep it in step
with the `google.golang.org/protobuf` version in `server/go.mod`):

```sh
buf lint && buf generate
```

The TCP transport sends each message as a 4-byte big-endian length followed by
a binary `ClientMessage` / `ServerMessage`.

`server/go.mod` requests toolchain go1.27.1. With the default `GOTOOLCHAIN=auto`,
an older local Go downloads that toolchain automatically.
