# Cribbage

Two-player cribbage: a Go game server with a neural network AI, and a Godot
client. See [docs/PLAN.md](docs/PLAN.md) for the milestone plan.

## Server (`server/`, Go 1.27)

```sh
cd server
go test ./...                       # all tests
go run ./cmd/cribserver             # server; TCP for bots on :7777
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
| `internal/transport/tcp` | Length-prefixed protobuf framing |
| `internal/client`, `internal/botclient` | Go client and headless bot runner |

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
