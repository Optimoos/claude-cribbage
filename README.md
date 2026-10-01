# Cribbage

Two-player cribbage: a Go game server with a neural network AI, and a Godot
client. See [docs/PLAN.md](docs/PLAN.md) for the milestone plan.

## Server (`server/`, Go 1.27)

```sh
cd server
go test ./...                       # all tests
go run ./cmd/cribserver             # server; TCP for bots on :7777, data in cribbage.db
go run ./cmd/cribserver -db ""      # in-memory only (nothing survives a restart)
go run ./cmd/botclient -user alice -opponent heuristic -games 1000 -concurrent 100
go run ./cmd/botclient -user bob -games 1000      # matchmaking: run two of these
go run ./cmd/sim -a heuristic -b random -n 100000 # in-process bot matches
```

| Path | Purpose |
|---|---|
| `internal/engine` | Rules engine: pure, deterministic state machine |
| `internal/bots` | Bot interface, random and heuristic bots, game runner |
| `internal/protocol` | Engine to protobuf conversions; `pb/` is generated |
| `internal/server` | Connections, auth, lobby, matchmaking, game actors |
| `internal/store` | Users, sessions and games: in-memory or SQLite |
| `internal/transport/tcp` | Length-prefixed protobuf framing |
| `internal/client`, `internal/botclient` | Go client and headless bot runner |

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
