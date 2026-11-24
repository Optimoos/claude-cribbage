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

**Environment notes:** Go 1.27 is installed (upgraded from 1.23.4 during M3). The .NET SDK is **not** installed. You'll need the .NET 8 SDK and Godot 4.x .NET before milestone 8.

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

**Built (2026-10-30):** `client-godot/`, a Godot 4.7.2 .NET project
(net8.0, rolling forward to the installed .NET 10). WebRTC comes from the
webrtc-native 1.2.2 GDExtension, fetched by `fetch-webrtc.sh` (pinned
SHA-256) rather than checked in. The C# protocol classes are generated at
build time by Grpc.Tools from `/proto`. The server needed no changes: it
already answers binary data channels in binary.
- **Connection** mirrors the web client: an offer over the `/rtc`
  WebSocket, held-back candidates on both sides, a 15 s open deadline, a
  1.5 s grace on "disconnected", and reconnects backing off from 0.5 s
  to 2 s. ICE servers come from `/api/config` and the bot list from
  `/api/bots`.
- **Screens** are built in code from one `Main.tscn`. The login form is
  an overlay with the server URL. The lobby (bot picks, find a match, open
  game, your/open/recent games) sits down the left. The table has the
  opponent plate and hand, a 3 × 40 board with front and back pegs, the
  crib, the starter, the count with the current and earlier sequences, the
  last hand's counts, your hand, a game log, and a game-over banner with
  "Play again". Cards and the board are drawn with `_Draw`.
- **Session** token and server URL persist in `user://<profile>.cfg`.
- **Autoplay** (`--autoplay`) plays random legal moves through the UI
  code paths (discards go through pick-and-press), for headless tests.
  Other test options are `--quit-after`, `--pace` and `--screenshot`.

**Verified:**
- Headless Godot played full games against `heuristic` and `heuristic2`.
- Two Godot clients played each other through an open game and agreed on
  the result (121–92).
- A client killed with SIGKILL after 12 moves was reopened with the same
  profile and finished the same game from 9–27.
- Windowed screenshots checked the layout in pegging, the show and game
  over.
- Not checked here: Godot against a browser player, since no scriptable
  browser with WebRTC is installed. The server keeps a codec per
  connection, so a mixed game should behave like the two above. No export
  templates are installed, so there is no exported build yet.
- Simpler than the web client: no event-by-event animation or sound, and
  only the default theme.

## Later (out of scope now)
- Web export for Godot once .NET web support lands, or a GDScript port.
- Matchmaking and ratings.
- TURN deployment.
- Manual counting with muggins.
- Three-player games (two people and a bot, or one person and two bots):
  see "Roadmap: three-player games" at the end.

---

# Phase 2: Expert AI (M9 to M16)

Added 2026-10-04. M8 (Godot client) is paused while this phase runs. The
heuristic and `nn` bots stay as they are; this phase adds a third bot.

## Where the network got to

The longest run (`models-big`, 463 → 256 → 128 → 1) played 17M games in
about 22 hours and peaked at 52.9% ± 1.5% against the heuristic. It was
flat from roughly 5M games on. Three things cap it:
- The target is win or loss, nine hands and about 90 decisions later, in a
  game dominated by deal luck. Most of the network's capacity learns noise.
- Evaluations of 2000 games (±1.5%) cannot see a two-point gain, so
  `best.bin` partly tracks lucky evaluations.
- The heuristic already does the most valuable thing right: expected-value
  discards with a crib table. Its weaknesses are one-ply pegging with a
  uniform opponent model, treating the opponent's crib discards as random
  cards, ignoring the pegging value of the kept hand, and ignoring the
  score when choosing between points and winning chances.

Cribbage is luck-heavy, so the ceiling is modest: the best possible player
probably beats this heuristic by 56 to 60%.

## Decisions

- **Design:** search plus learned tables (an expert system), not a bigger
  network. Hands are independent except through the score, pegging is a
  tiny imperfect-information subgame, and the show is exact once cards are
  known. Learned components fill only the gaps enumeration cannot.
- **Tooling:** everything stays in Go. The bottleneck is game simulation,
  not gradient computation. A GPU/PyTorch path is deferred unless M16
  wants a large distilled network.
- **Search effort is one knob.** Self-play generation fixes it low and
  equal on both seats so games finish quickly. Against humans and other
  bots it is configurable. Latency only matters when a human is waiting;
  a few hundred milliseconds is acceptable there.
- **Lobby:** a third bot, `expert`, with difficulty levels. Effort alone
  gives a narrow spread because the tables carry most of the skill, so
  weaker levels also drop components (opponent inference first, then the
  crib and pegging tables). Every level is deterministic and plays its
  single best move; ties break in a fixed order.
- **Hardware:** overnight runs on the i7-9750H laptop (6 cores) now; a
  Ryzen 5900X desktop is available in a few days for policy iteration.
- **Rules:** standard two-player cribbage as the engine implements today.
  Rule changes later are accepted as rework.
- **Success metric:** paired (duplicate-deal) win rate of at least 57%
  against the heuristic with a confidence interval of ±0.5% or better, a
  clear points-per-game edge, and a win over the `nn` bot of similar size.

## The model

A bundle file (header plus tables, in the same magic-and-JSON style as the
network files) containing:
1. **Win-probability table** W(my score, their score, who deals next):
   the chance of winning at the start of a hand. Estimated from self-play
   outcomes and smoothed by dynamic programming over hand-outcome
   distributions. Every decision maximizes expected W, so the endgame
   (points versus winning chances, pone counts first) is handled uniformly.
2. **Crib tables:** the distribution of crib points from a pair of discard
   ranks, conditioned on whose crib it is, learned from self-play rather
   than assuming random opponent discards.
3. **Pegging tables:** the distribution of pegging point differential by
   kept-hand rank shape and dealer, learned from self-play of the search
   pegger. Used only when choosing discards.
4. **Opponent discard model:** for a six-card rank shape, which four a
   sensible player keeps. Used to weight sampled opponent hands.

Decisions:
- **Discard:** for each of the 15 options, the hand-score distribution over
  starters is exact; crib and pegging distributions come from the tables.
  Convolve, apply the end-of-hand ordering (pegging, pone's hand, dealer's
  hand, crib), and map through W.
- **Pegging:** enumerate or sample opponent hands consistent with
  everything seen, weighted by the opponent model. Suits do not matter in
  pegging, so an unknown hand is a rank multiset and the expectation is
  nearly exact. Solve each world's remaining pegging exactly and value the
  end of the hand through W.
- **Policy iteration:** regenerate every table from self-play of the
  expert against itself and repeat until a paired evaluation shows no gain.

## Milestones

### M9: Evaluation harness
- Duplicate-deal arena: each deck is played twice with seats swapped, so
  deal luck cancels. Report paired win rate and points-per-game
  differential, each with a 95% confidence interval.
- A gauntlet command that runs one bot against random, heuristic, nn and
  a named earlier bundle in one table, with decision latency per phase.
- Re-measure the `nn` bot at 100k paired games for a precise baseline.

**Verify:** confidence intervals shrink measurably at equal game counts
against the current arena; the baseline nn figure is reproducible across
seeds within its interval.

**Baseline (2026-10-04, `cmd/gauntlet`, 100k paired games, 95% intervals):**

| Bot | vs heuristic, seed 1 | seed 2 | margin/game |
|---|---|---|---|
| `models-ft/best.bin` (served as `nn`) | 50.04% ± 0.20 | 50.18% ± 0.20 | -0.29 ± 0.06 |
| `models-big/best.bin` | 50.89% ± 0.20 | 51.14% ± 0.20 | 0.00 ± 0.06 |

`models-big` beats `models-ft` head to head 51.08% ± 0.20. Pairing cuts the
win-rate interval from ±0.31 to ±0.20 (about 2.4x fewer games for equal
precision) and the margin interval by half. The training-time figures
(50.3%, 52.9%) were inflated by picking the best of many noisy evaluations.
M9+ comparisons use `models-big/best.bin` as the `nn` reference.

### M10: Win-probability table and bundle format
- Collect (scores, dealer, outcome) at every hand start from self-play of
  the best current bot. Smooth with DP over empirical hand-outcome
  distributions binned by score region.
- Bundle file format with a versioned header, a loader, and a `cmd/expert`
  tool with `build`, `inspect` and `eval` subcommands.

**Verify:** W is monotone in both scores, swapping seats and dealer gives
complementary probabilities, and spot checks match raw empirical
frequencies within their intervals.

**Built (2026-10-04):** `internal/expert`, `cmd/expert`, bundle `CRIBEX01` v1.
- One table V(dealer score, pone score) = P(dealer wins); either seat's
  chance follows from it, so the two seats are complementary by construction.
- Every self-play hand is stored as its start and its ordered scoring steps.
  The DP replays hands step by step, so races to 121 are exact.
- "Binned by score region" became a sliding window: each start (x, y) is
  valued with the 5000 hands that started nearest below and left of it.
  Score-independent hands (all hands pooled) were off by up to 2 points in
  the endgame, because the nn plays differently near 121; the heuristic,
  which ignores the score, was calibrated either way. Windows below-left
  keep hands cut short by a win usable without bias.
- Sampling noise is clamped away: each V(x, y) is held between its two
  already-computed neighbours, so the table is exactly monotone.
- Result from 500k games of `models-big`, checked on 500k fresh games: no
  monotonicity violations; 76 of 86 10x10 bins within their 95% interval
  (chi-squared/bin 1.53); largest error 0.8 points (both scores 110-119);
  per-state noise about 0.2 points; first dealer wins 56.1%.

### M11: Pegging search
- Opponent-hand enumeration by rank multiset over unseen cards (uniform
  weighting for now), an exact pegging solver per world, W-valued
  terminals, and an effort parameter controlling the number of worlds.
- New `expert` bot: pegging by search, discards still via the heuristic.

**Verify:** pegging points per hand beat the heuristic in a paired match,
overall win rate rises, and latency per decision is reported at each
effort level.

**Built (2026-10-04):** `internal/expert` (`peg.go`, `search.go`, `bot.go`);
players are specs like `models-expert/expert.bin@400` (effort = most
opponent hands searched per pegging decision, 0 = all).
- A rank-only pegging model, checked move by move against the engine.
- Worlds are the opponent's possible rank multisets, weighted by card
  combinations; above the effort limit a deterministic systematic sample.
  Gos are hard evidence (two plays running by me, or a reset below 31,
  means the opponent held nothing that fit), tested never to exclude the
  real hand. Minimax per world with alpha-beta; terminals run the shows in
  order (crib as a distribution over the unseen cards) and then W.
- Opponent model: minimax beat a greedy opponent model (53.9% vs 52.7%).

| Effort | vs heuristic | pegging/hand | latency mean / p99 (1 thread) |
|---|---|---|---|
| 25 | 52.88% ± 0.39 | +0.033 ± 0.010 | 0.3 / 1.8 ms |
| 100 | 53.94% ± 0.39 | +0.074 ± 0.010 | 0.7 / 5.9 ms |
| 400 (default) | 54.05% ± 0.39 | +0.085 ± 0.010 | 2.0 / 20 ms |
| all | 54.04% ± 0.54 | +0.089 ± 0.014 | 5.7 / 67 ms |

Against `models-big` (effort 400, 20k games): 52.28% ± 0.45, margin +0.77,
but pegging -0.013 ± 0.010 per hand: the nn pegs at least as well, so the
gain is from playing for the win near 121. The minimax opponent (who sees
my hand in each world) is the suspect; revisit in M13.

### M12: Discard engine
- Exact hand distributions over starters; crib and pegging-shape tables
  built from self-play of the M11 bot; convolution and end-of-hand ordering
  through W.

**Verify:** paired win rate and points edge over heuristic and nn improve
on M11. Spot-check a sample of discards against published expert tables.

**Built (2026-10-04):** `internal/expert` (`tables.go`, `discard.go`), bundle v2
with optional `crib`, `peg` and `show` tables (v1 bundles still load and
discard like the heuristic). `cmd/expert build -tables discard -base B`
keeps B's win table and builds the rest.
- Crib: distribution by (whose crib, discarded rank pair, starter rank),
  each cell shrunk (20 hands) towards the exact random-crib distribution.
  Already conditioned on whose crib it is (planned for M13).
- Pegging: my and their points by (dealer or pone, kept rank shape);
  opponent show by (dealer or pone, starter rank). Only complete hands
  starting below 70-70 are used, so games ending mid-hand bias nothing.
- Each option: my show exact per starter; crib and their show mixed over
  the same starters; pegging, pone show, dealer show, crib in order, then W.
  1.7 ms per discard on one thread.
- Tables from 200k games of the M11 bot (effort 25), 1.05M hands. Dealer
  crib averages: 5-5 9.0, 5-J 7.0, 5-10 6.7, A-4 5.4 (published: about
  8.5, 7.0, 6.6, 5.4).
- Results, 20k paired games each: vs heuristic 55.31% ± 0.45 (M11 54.05%),
  vs the M11 bot 51.70% ± 0.39, vs `models-big` 53.53% ± 0.45 (M11
  52.28%), pegging now +0.09 per hand against the nn as well.

### M13: Opponent modelling
- Weight each sampled opponent hand by the probability the discard model
  keeps it, and by consistency with their pegging plays so far.
- Crib tables conditioned on whose crib it is.

**Verify:** paired gain over the M12 bot.

**Built (2026-10-05):** `internal/expert/keep.go` and the search's world
weighting; bundle table `keep` (two temperatures).
- Discard model: for every six-rank shape and side, a softmax over the
  distinct keeps of (expected hand over starters + pegging edge from the
  peg table +/- the crib table's average). The temperature per side is
  fitted by maximum likelihood to self-play discards (0.47 dealer, 0.43
  pone on M12 self-play; the bot's real keep gets probability 0.54 on
  average). A world's weight sums over the two cards the opponent may
  have thrown; it is cached per hand by rank shape.
- Pegging consistency: each opponent play this hand is scored by a
  softmax on its immediate points (temperature 1; 2 played the same).
- In heuristic games the model makes the opponent's real hand 2.4x more
  likely on average than card counting alone.
- Sampling: a pool of 4x effort worlds drawn by card counts, weighted by
  the model, then the searched worlds drawn by weight: same strength as
  weighting every world (53.00% vs 52.96%), 40% faster.
- Results at effort 100 both sides, 20k paired games vs M12: 53.00% ±
  0.37, margin +1.23, pegging +0.12 per hand. The pegging model alone is
  +0.34% ± 0.28; the keep model is most of the gain.
- Crib tables were already conditioned on whose crib it is (M12).

### M14: Policy iteration and long runs
- Regenerate all tables from expert self-play (low, equal effort), repeat
  for several rounds, tune effort levels. Long runs on the 5900X.

**Verify:** each round is evaluated paired against the previous round and
accepted only if it does not regress. Final run against the M9 baseline.

**Built (2026-10-05):** `cmd/expert iterate` (each round rebuilds every table
from self-play of the current best at effort 25, then plays it paired
against the best at effort 100 and keeps it if it wins at least half).
Run on the i7 laptop: 300k games per round, about 80 minutes each.

| Round | vs | Win % | Margin | Accepted |
|---|---|---|---|---|
| 1 | M13 | 50.65 ± 0.39 | +0.26 ± 0.11 | yes |
| 2 | round 1 | 50.02 ± 0.38 | -0.02 ± 0.10 | yes |

Round 2 showed no gain, so the iteration stopped there; it is the default
bundle `models-expert/expert.bin` (the M10 bundle is kept as
`expert-m10.bin`). Final run at effort 400:

| Opponent | Paired games | Win % | Margin/game | Pegging/hand |
|---|---|---|---|---|
| heuristic | 40k | **57.16 ± 0.32** | +1.96 ± 0.09 | +0.240 ± 0.007 |
| `models-big` (nn) | 20k | 55.85 ± 0.45 | +1.88 ± 0.13 | +0.156 ± 0.010 |
| M13 bot | 20k | 50.31 ± 0.39 | +0.17 ± 0.11 | -0.006 ± 0.007 |

The phase 2 success metric is met: at least 57% against the heuristic with
an interval within ±0.5, a clear points edge, and a clear win over the nn.
Decision time under 8-way load: discard 3.1 ms mean (p99 8 ms), pegging
5.3 ms mean (p99 47 ms). Longer runs on the 5900X (more games per round,
a smaller win-table window) remain an option but would gain little.

### M15: Server integration and difficulty levels
- `cribserver -expert models/expert.bin` registers the `expert` bot and its
  difficulty levels; the lobby offers them; `GET /api/bots` lists them.
- Shared read-only tables, per-game scratch; resume after restart.
- Define the difficulty levels from measured strength (effort plus
  dropped components), all deterministic.

**Verify:** 10k games through the server match the arena result, a
browser game against each level plays well, and a saved game against
`expert` resumes after a restart.

**Built (2026-10-05):** `cribserver -expert models-expert/expert.bin`
registers three levels (`internal/expert/levels.go`); the lobby shows a
button for each level the server offers. Bots share the bundle's tables;
each game gets its own scratch (about 0.4 MB). Saved games are recreated
by bot name on restart.

| Level | Settings | vs heuristic (paired) | pegging decision |
|---|---|---|---|
| `expert-easy` | effort 10, no opponent model, heuristic discards | 51.02% ± 0.55 | 0.3 ms |
| `expert-medium` | effort 50, discard tables, no opponent model | 54.80% ± 0.63 | 0.8 ms |
| `expert` | effort 400, everything | 57.16% ± 0.32 | 5.3 ms |

- 10k games through the server (botclient heuristic vs `expert`, TCP,
  16 in flight): the expert won 57.0%, matching the arena; no errors.
- Tests: all three levels play 30 games each through the server, and a
  saved game against `expert` resumes after a restart.
- Left to verify by hand: a browser game against each level.

### M16 (optional): Distillation (skipped 2026-10-05: the expert already meets the target and decides in milliseconds)
- Train a policy and value network in Go from millions of expert
  decisions, as a fast approximation and as the search's opponent model.

**Verify:** the distilled network beats `nn` and approaches the expert at
a fraction of the latency.

# Possible future milestones: a stronger heuristic (M17 to M21)

Added 2026-10-05, not scheduled. The `heuristic` bot stays frozen: every
result above, the network's training evaluations, the difficulty tiers
and the win-table calibration are anchored to it. Any improvement goes
in under a new name, `heuristic2`, measured paired against the frozen
bot with `cmd/gauntlet` (40k games resolve a one-point gain). The payoff
is a cheap lobby tier between `heuristic` and `expert-easy`.

## Where the heuristic loses

From the phase 2 results, with the heuristic as the opponent:

| Component added to the heuristic | Paired win % |
|---|---|
| Pegging search only, 10 worlds, no opponent model (`expert-easy`) | 51.0 |
| Pegging search only, 400 worlds (M11) | 54.1 |
| Plus learned discard tables (M12) | 55.3 |
| Plus opponent modelling and policy iteration (M14) | 57.2 |

About four of the seven points come from pegging and three from discards
and opponent inference. The search's pegging gain comes from enumerating
the rest of the hand with the actual cards, which rules cannot replicate,
so a rule-based bot should expect 3 to 4 points over the current
heuristic, not the full seven.

## Milestones, in order of expected gain per effort

### M17: Score-aware discards
The heuristic maximizes mean points at 3-3 and at 115-118 alike. It
already enumerates every starter, so when either player is within a
hand of 121 it should maximize the probability of reaching 121 (pone
counts first), and as the trailing player prefer variance. M11 found
most of the expert's edge over the network came from playing for the
win near 121.

**Verify:** paired gain over `heuristic`, largest in games where a
player starts a hand at 95 or more.

**Built (2026-10-29):** `heuristic2` (`internal/bots/heuristic2.go`).
Once either player has 90 or more, it keeps the four cards with the
highest win probability, in counting order: joint pegging, the pone's
show, then the dealer's show, crib and heels, then a win table for the
hands after. The table is computed at first use by dynamic programming
over per-hand distributions from 200k heuristic self-play games
(`h2stats.go`, regenerated with `go generate ./internal/bots`). It
agrees with the expert bundle's learned table to 0.15% on average.
Near-ties in win probability (1e-4 per point) go to the heuristic's
points value. Below 90, and in pegging, it plays exactly as `heuristic`.

| Variant (200k games, paired vs `heuristic`) | Win % | Margin per game |
|---|---|---|
| Endgame from 90, no points tie-break | 50.01 ± 0.17 (40k) | −0.11 |
| Endgame from 90 (shipped) | 50.30 ± 0.07 | −0.06 |
| Endgame from 0 (every discard) | 50.29 ± 0.09 | −0.13 |

- The gain is real but small: +0.3 points, not the one or more hoped
  for. The threshold doesn't matter (60, 80, 90 and 100 all gave the
  same result within noise), so mid-game discards gain nothing from it.
  Every gain comes from hands where someone has 90 or more, since no
  other decision changes.
- Without the tie-break the bot gives away points in games already won
  or lost, where options tie on win probability.
- Most likely limit: pegging is drawn from one distribution whatever is
  kept, so an endgame keep can't be chosen for its pegging. M21 adds
  that.
- Endgame discards take 0.5 ms mean (p99 5 ms) under 24-way load.
- Tests: identical to `heuristic` below the endgame, the win table is
  monotonic, and the pone on 115 always keeps a sure 6 when one exists
  (the heuristic risks it in 12 of 200 such hands).

### M18: Go and 31 awareness in pegging
The risk term counts only the opponent's immediate scoring reply. Add
a second ply: the go point when the opponent cannot play, keeping a
card that makes 31 later, and what I score back on the following play.

**Verify:** pegging points per hand improve in a paired match.

**Built (2026-10-29):** `heuristic2` pegs with a three-play lookahead
(`internal/bots/heuristic2_peg.go`). A card is worth its points, plus
the go point times the chance the opponent cannot play, plus, over their
replies, P(their best scoring card is u) × (my best answer to u − their
points for u). Unseen cards are equally likely to be held (exact
hypergeometric odds). Ties in their choice go to the reply that leaves
me least. When they have just said go, the go is certain.

| Pegging variant (paired vs `heuristic`, M17 discards) | Win % | Pegging per hand |
|---|---|---|
| M17 (heuristic's pegging) | 50.30 ± 0.07 | −0.002 |
| Lookahead without my answer | 50.47 ± 0.12 | +0.007 |
| Lookahead, go valued as my whole solo run | 51.61 ± 0.12 | +0.077 |
| Lookahead, go point only (shipped) | 51.78 ± 0.12 | +0.085 |
| Shipped, but every reply weighted as in the heuristic's risk | 49.66 ± 0.12 | −0.028 |

- My answer to their reply carries most of the gain. It only helps once
  their reply is modelled as their best card. Weighting every card they
  might hold counts my answers to replies that can't all happen, and
  that variant loses to the heuristic.
- Against `expert-easy` (bundle@10,nokeep,nopeg,heurdiscard): 50.74% ±
  0.12, so `heuristic2` already sits above the easiest expert level
  (51.0% vs heuristic in M15) rather than below it.
- Pegging decisions: 9 µs mean (p99 28 µs) under 24-way load.
- Tests: the lookahead's value on hand-worked positions (their reply, my
  answer, their best card, a forced go, an empty hand).

### M19: A non-uniform opponent model
Every unseen card is treated as equally likely in the opponent's hand,
but kept hands are rich in fives and ten-value cards, and a go is hard
evidence. Use a fixed per-rank prior for kept cards (from self-play or
from the expert bundle's keep model) and exclude ranks a go rules out.

**Verify:** the real opponent hand gets higher probability than under
card counting alone; paired gain over M18.

**Built (2026-10-29):** `heuristic2`'s pegging lookahead draws the
opponent's hand from weighted holdings (`holdings` in
`internal/bots/heuristic2_peg.go`):
- **Prior:** each unseen card weighs how much likelier heuristic
  self-play keeps its rank than a random card's, by side (`h2Keep` in
  `h2stats.go`). The pone keeps fives 1.46× as often and kings 0.60×;
  the dealer's weights are flatter.
- **Go floor:** cards the gos rule out weigh nothing. `goFloor` moved
  from `internal/expert` to `bots.GoFloor`, and the expert's search
  calls it there.
- **Odds:** the hypergeometric "they hold none of these" becomes a
  weighted draw. It is exact when weights are equal, and the uniform
  variant reproduces M18's result exactly.

Mean log-likelihood of the opponent's real hand per pegging decision
(3000 heuristic games): card counting −6.577, prior −6.550, floor
−6.505, both −6.477. No variant ever rules out a card they hold.

| Variant (200k games, paired vs `heuristic`) | Win % | Pegging per hand |
|---|---|---|
| M18 (card counting) | 51.78 ± 0.12 | +0.085 |
| Go floor only | 51.78 ± 0.12 | +0.085 |
| Rank prior only | 52.22 ± 0.12 | +0.106 |
| Both (shipped) | 52.23 ± 0.12 | +0.107 |

- The prior is worth +0.45; the floor improves the likelihood but not
  play. M18 already treats a go in the current count as certain, and
  most other go evidence concerns cards that would not fit anyway.
- Against `expert-easy`: 51.09% ± 0.12 (M18: 50.74%).
- Pegging decisions: 9.8 µs mean.
- Tests: weighted odds (equal weights give the hypergeometric; heavier
  cards are likelier held) and the real hand's likelihood beating card
  counting over 500 games.

### M20: Crib tables by side
`CribEV` assumes the other two crib cards are random. Opponents throw
poor cards to your crib and good ones to their own. Replace it with two
91-entry tables (dealer's crib, pone's crib), taken from the expert
bundle's crib tables or published values.

**Verify:** dealer and pone discards change in the expected direction
(for example pone throws fewer fives); paired gain.

**Built (2026-10-29), no gain:** `heuristic2` values the crib by side
in all its discards: the points rule before the endgame
(`chooseDiscard2`), the endgame's win probability and its tie-break.
The other two crib cards are enumerated with each pair weighted by how
much likelier the other side is to throw those ranks than a random
pair's. Those habits come from heuristic self-play (`h2Throw` in
`h2stats.go`), not the expert bundle, so `heuristic2` stays
self-contained. The tables are per starter rank, as before, so 91 × 13
per side rather than 91.

- Directions are as expected. My discards are worth less to my own crib
  than to a random one (5-5: 8.69 vs 8.99; 10-K: 2.69 vs 3.41) and more
  to the dealer's when I am the pone (5-5: 9.28; 10-K: 3.72).
- Discards change in 7.4% of pone hands and 10.1% of dealer hands. The
  dealer throws slightly fewer fives (125 vs 130 per 1000 hands). The
  pone barely changes (14 vs 13), because the heuristic almost never
  throws the dealer a five already.

| Variant (200k games, paired vs `heuristic`) | Win % | Margin per game |
|---|---|---|
| M19 (random crib) | 52.23 ± 0.12 | +0.88 |
| By side before the endgame only | 52.20 ± 0.13 | +0.90 |
| By side in the endgame only | 52.25 ± 0.12 | +0.90 |
| Both (shipped) | 52.22 ± 0.13 | +0.91 |

- No win gain. Points rise by about 0.03 per game, and against
  `expert-easy` it's 51.22% ± 0.13 (M19: 51.09%), both within noise.
  More accurate crib values only change close decisions, where little
  is at stake. Kept because it is no slower, and M21's pegging value is
  added to these discard values.
- Tests: summed over every pair, the dealer's crib table lies below the
  random one and the pone's above.

### M21: Pegging value of the kept hand
Discards ignore how the kept cards peg. Add a per-rank or per-shape
pegging bonus, derived from the expert bundle's peg table (mean of my
points minus theirs by side and shape), to the discard value.

**Verify:** paired gain; combined `heuristic2` lands between
`heuristic` and `expert-easy` in the level table, and is offered in the
lobby if so.

**Built (2026-10-29):** `heuristic2`'s points rule for discards adds
0.75 × the kept shape's pegging value: the mean of my pegging less the
opponent's, by side and by shape (the 1820 multisets of four ranks).
It comes from the same heuristic self-play hands (`h2PegShape` in
`h2stats.go`) and is shrunk toward a per-rank estimate with the weight
of 200 hands. Heuristic self-play keeps the generator independent of
`heuristic2`, so regenerating is stable. Low cards and pairs peg best
(dealer 2-2-3-3: +2.13), ten-cards worst (pone 10-J-Q-K: −2.41). The
endgame rule is unchanged.

| Variant (200k games, paired vs `heuristic`) | Win % | Margin | Pegging per hand |
|---|---|---|---|
| M20 (weight 0) | 52.22 ± 0.13 | +0.91 | +0.100 |
| Weight 0.5 | 52.65 ± 0.13 | +1.12 | +0.133 |
| Weight 0.75 (shipped) | 52.71 ± 0.13 | +1.16 | +0.152 |
| Weight 1 | 52.61 ± 0.14 | +1.13 | +0.170 |
| Weight 1.5 | 52.23 ± 0.14 | +0.96 | +0.196 |

With weight 0.75, starting the endgame at 100 or 110 instead of 90 gave
the same win rate (52.70, 52.69), so the endgame rule's lack of a
pegging value costs little.

## Where heuristic2 ended up

| Milestone | vs `heuristic` |
|---|---|
| M17 score-aware endgame discards | 50.30 ± 0.07 |
| M18 three-play pegging lookahead | 51.78 ± 0.12 |
| M19 weighted opponent holdings | 52.23 ± 0.12 |
| M20 crib by side | 52.22 ± 0.13 |
| M21 pegging value of the kept hand | 52.71 ± 0.13 |

Paired over 200k games, the final `heuristic2` wins 51.57% ± 0.14
against `expert-easy` and 47.86% ± 0.14 against `expert-medium`. It
lands between those two levels, not below `expert-easy` as planned,
with 0.5 ms discards (mean; p99 5 ms) and 10 µs pegging decisions.
Pegging gave most of the gain (M18, M19, M21: +2.4 points), and the
discard ideas alone (M17, M20) gave +0.3. The plan expected 3 to 4
points from rules; this is 2.7.

The lobby offers it as its own level, "Heuristic bot (strong)", listed
between Expert (medium) and Expert (easy). Unlike the expert levels it
needs no bundle, so it is always shown, like `heuristic`.

---

# Godot polish and builds (M22)

Added 2026-10-30, after every planned milestone was built. M8's client
played correctly but showed each update at once, had no sound and one
theme, and there was no exported build.

### M22: Bring the Godot client up to the web client, and export it
- Replay each update's events one at a time, as the web client does:
  cards deal in, the starter flips, pegs hop, points float up, and a click
  skips ahead.
- Synthesized sound effects, the three web themes, rapid play and mute,
  saved between runs.
- Release exports for Linux and Windows.

**Verify:** themes and animations look right in windowed screenshots; full
games still pass with autoplay, rapid and animated; the exported builds
play a full game.

**Built (2026-10-30):**
- **Replay** (`Main.Replay.cs`) ports the web client's `replay`,
  `applyEvent` and `syncView`. The table now draws a view that each event
  changes (`GameState.View`) rather than the raw observation. Pauses use
  the web client's timings. The last pause before my move ends as soon as
  I act, and clicking the table or switching games skips to the end.
- **Effects:** cards slide in from the dealer's side, the starter flips,
  the peg that was behind hops over the other, and "+N" labels rise from
  the score, the play area or the deck, stacked when they come together.
  A count of 15 or 31 bounces. The game-over banner pops in with the
  final score, a skunk note, "Play again" against a bot, "View table", and
  confetti on a win.
- **Sound** (`Ui/Sfx.cs`) renders the web client's eight Web Audio
  recipes into 16-bit WAV streams at startup: enveloped sine and triangle
  tones with pitch slides, and band-passed noise.
- **Themes:** card room, parchment and midnight use the web client's
  colours. Changing theme rebuilds the interface in place. Theme, rapid
  play and mute are stored in the profile file.
- **Exports:** `export_presets.cfg` (Linux and Windows x86_64),
  `export.sh` and `Cribbage.sln` (Godot's .NET export requires one). Each
  build is about 150–190 MB, most of it a self-contained .NET runtime.
  Only the x86_64 Linux and Windows templates were installed (350 MB of
  the 1.2 GB download, whose SHA-512 checked out).
- **Fixed on the way:** the log was rebuilt from scratch on every render,
  which made each move slower as a game went on. A rapid autoplay game
  took 23 s; with the log appended to instead, it takes 3 s. The table
  also no longer slides under the log at the base 1280-pixel width.

**Verified:**
- Rapid and animated autoplay games complete.
- An animated game that switched theme every 10 moves completed with no
  errors.
- Windowed screenshots in all three themes show a floating "pair royal",
  the show rows and the board.
- The exported Linux build played a full game headless and rendered
  correctly in a window. The exported Windows build played a full game
  under Wine.
- Not done: the web client's second board style ("long") and its phone
  layout.

---

# Public hosting (M23)

Added 2026-10-30. A review before hosting publicly (security, concurrency
and storage, game logic) found no critical problems. It found ways one
account could exhaust memory or CPU, gaps in the new login limits, and no
panic recovery. M23 fixes those and adds invites for slow family games.

### M23: Invites, sleeping games and the review's fixes

Decided with you: invite by username and by code, keep open games (one
per player), games between people expire after 60 days without a move, 20
games per player of which 5 against the computer.

**Built (2026-10-30):**
- **Invites** (`internal/server/invites.go`, `invites` table). An invite
  is a database row: no game, goroutine or bot until it is accepted, which
  creates the game with both players seated. It goes to a username, or is
  a code such as `K7M-Q2X` (31 symbols without I, L, O, 0, 1; 6 of them,
  case, spaces and dashes ignored).
  - **Lifetime:** 48 hours.
  - **Limits:** 5 sent and unanswered per player, 20 waiting for one
    player, one between any two players. Sending is limited to 10 then one
    every 6 minutes; code guesses to 10 then one a minute.
  - **Messages:** `SendInvite`, `RespondInvite` and `RedeemCode`, plus
    `Invites` pushes and `GameList.invites`.
  - **Clients:** the web lobby and the Godot lobby both show them.
- **Sleeping games.**
  - After 10 minutes without activity a game's actor exits and drops its
    engine and bots, keeping only its summary for the lobby.
  - The next command restarts it. It rebuilds the game from the store: its
    header and a replay of its actions. SQLite already keeps each
    unfinished game's actions in memory (about 3 KB), so waking reads no
    rows.
  - A command that panics now drops the half-changed state, and the next
    command reloads from the store.
- **Expiry.**
  - An hourly sweep cancels open games after 48 hours, and ends games
    after 60 days without a move between people or 30 against a bot
    (winner −1, `EVENT_KIND_EXPIRED`).
  - The stored `updated` time is refreshed at most daily while moves are
    made, so expiry is accurate to a day across restarts.
  - Games from before M23 date their idle time from their last header
    write.
- **Caps.**
  - 20 unfinished games per player, 5 against bots, 1 open, enforced on
    create, join, matchmaking and accepting an invite.
  - Places are reserved under `gmu` while a game is being created, so
    concurrent requests cannot pass a cap.
  - Waiting matchmaking requests count toward the cap. That bounds the
    queue, and a player can still ask for several matches.
  - New `ERROR_CODE_TOO_MANY_GAMES`, with a message naming who is at which
    limit.
- **Review fixes.**
  - **Panics:** a bot that panics resigns its seat. Bots are
    deterministic, so a panic would otherwise crash-loop on restart.
  - **Expert memory:** the expert's 370 KB end-of-hand table comes from a
    `sync.Pool` during each decision instead of living with every bot.
  - **WebRTC:**
    - 64 KB maximum message, advertised and enforced: a larger message
      closes the connection.
    - 256 KB SCTP receive buffer.
  - **Connections:**
    - 20 per address and 5,000 in total.
    - Remote connections must log in within 30 s.
  - **Logins:**
    - A login name that cannot exist is refused before any lookup.
    - An unknown user costs a dummy bcrypt, so timing does not reveal
      usernames.
    - Failed logins count per username and address, so a stranger cannot
      lock a player out.
    - IPv6 addresses count by /64.
    - An unknown address is limited (it used to be exempt).
    - bcrypt uses at most GOMAXPROCS−1 cores.

**Verified:**
- **Unit tests:**
  - caps
  - sleeping and waking through a whole game, on both stores
  - expiry, including a move delaying it
  - a panicking bot and a panicking command
  - invites: accept, decline, cancel, codes typed loosely, limits, expiry,
    persistence across a restart, an offline recipient
  - addresses grouped by /64
  - failures counted per address
  - connection caps and the login deadline
  - login timing
  - the WebRTC size limit
- **Race detector:** the whole suite passes under `-race`.
- **Godot clients:** two headless Godot clients played a full game from an
  invite. An earlier run of the inviting client had sent the invite, and the
  other client accepted it later.
- **Not tested:** the web client's invite UI in a browser.

**Left for later:** review items 10–18, done in M24.

### M24: The review's remaining items

**Built (2026-10-30):**
- **Bot CPU (10).**
  - Bot decisions run under a semaphore of GOMAXPROCS−1
    (`GameLimits.BotMoves`), so scripted bot games cannot take every core.
  - After login, each player gets 60 requests at once, then 10 a second,
    across all their connections (`AuthLimits.ActionsPerUser`). Pings and
    loopback connections are not counted.
- **WebRTC setup (11).**
  - At most 10 setups in progress per address and 200 in all
    (`rtc.Options.MaxSessions*`). The address is the TCP peer, or the last
    `X-Forwarded-For` entry when the peer is a proxy on localhost.
  - At most 32 remote candidates per session, counting those in the offer.
  - `-rtc-public-only` drops remote candidates on private, loopback,
    link-local, CGNAT, documentation and reserved addresses, and mDNS
    names. That covers trickled candidates and those inline in the SDP, and
    the server stops offering its own loopback candidate. It is off by
    default for local and LAN play; `deploy/cribbage.service` sets it.
  - Address keys moved to `internal/netkey`.
- **Backups (12).** At startup a backup is written only if the newest is
  at least `-backup-every` old (`store.LastBackup` reads the file names).
  The deploy unit keeps 7, not 14.
- **Sessions (13).**
  - SQLite stores SHA-256 hashes of tokens; schema migration 1
    (`PRAGMA user_version`) hashes existing rows.
  - Expired sessions are deleted by the hourly sweep.
  - New `Logout` message: deletes the session, acks, closes the
    connection. The web and Godot clients send it from "Log out".
  - Tokens that are not 64 hex characters are refused before any lookup.
- **Flags (14).** `-http` and `-tcp` default to `127.0.0.1`.
- **HTTP (15).** Read 30 s, write 60 s, idle 2 min, 16 KB headers. The
  `/rtc` WebSocket is hijacked, which clears these deadlines.
- **Database (16).**
  - A failed batch is retried once, in one transaction with a savepoint per
    write, instead of once per write. Savepoints on every batch halved
    throughput in `BenchmarkSQLiteCommitParallel`, so the fast path has
    none.
  - A game whose first save fails is recorded as finished, so it does not
    come back after a restart.
  - `journal_size_limit` 16 MB: the WAL still grows to about 80 MB between
    checkpoints but shrinks back afterwards.
- **Startup (17).** `bots.Prepare` builds the heuristic tables before
  serving (about 50 ms on this machine).
- **CSP (18).** The server sends `Content-Security-Policy` with
  `default-src 'none'` and only `'self'` for script, style and connect, plus
  `data:` images for the icon. The client needed no changes.

**Verified:**
- **Unit tests:**
  - a failing write in a batch of 500 (4–5 transactions instead of about
    500)
  - an unsaved game start is not restored
  - session deletion, expiry and token hashing, including the migration
  - logout
  - the request rate limit
  - the bot move cap
  - candidate filtering, SDP filtering and the candidate cap
  - client addresses behind a proxy
  - the setup limit
  - finding the latest backup
- **Race detector:** server, store, transport, auth and bots pass.
- **Web client in headless Firefox (WebDriver BiDi):**
  - two players register, one invites the other by username, the other
    accepts, and both reach the game
  - an invite code typed in lower case with a space works
  - logging out makes the old token fail to resume
  - an injected inline style and script are blocked; the page itself
    logs no CSP violations or errors
  - this showed invite rows cutting the sender's name to "Grand…", so the
    name now has its own line above the time and buttons
- **Godot:** builds. Its logout was not exercised.
- **Load test fix:** WebRTC test connections can use the LAN address
  rather than loopback, so they are rate limited; the load-test server
  raises the request limit.

---

# History and accounts (M25, M26)

Added 2026-11-01, before deploying. Decided with you:
- History: a list of all finished games and a record against each
  opponent, without replays.
- Forgotten passwords: optional, confirmed email addresses with reset
  links.
- Admin resets: a command that reaches the running server over a local
  socket.

### M25: Game history and records

**Built (2026-11-01):**
- `GetHistory` returns a page of finished games, newest first (default 50,
  at most 100), with a cursor (`<updated ms>:<game id>`) for the next page.
  The first page also carries `Record`s: games, wins, losses and unfinished
  (expired or cancelled) games against each person (by user id, with their
  latest nickname) and each bot, plus a total.
- Open games that nobody joined are left out. `GameSummary.updated` gives
  each game's end time.
- SQLite queries use the existing `(seatN_user, updated)` indexes. The
  memory store pages through the 50 games it keeps per player.
- Web: "History and records" in the lobby opens a pane in place of the
  table, with a records table, the games, and "Load more". Godot: the same
  in a window.

### M26: Passwords, email resets and admin resets

**Built (2026-11-01):**
- **Schema migration 2:** `users.email` and `users.must_change`. A new
  `account_tokens` table holds SHA-256 hashes of single-use tokens: one
  reset and one verification token per user, which a new one replaces.
  The hourly sweep deletes expired ones.
- **ChangePassword:** needs the old password and a different new one. It
  ends every other session and closes their connections. Wrong guesses
  count against the login failure limit.
- **SetEmail:** needs the password. It mails a confirmation link
  (`#verify=`) that works for 48 hours; the address is used once the link
  is opened, and until then it shows as `pending_email`. An empty address
  removes it.
- **RequestPasswordReset:** takes a username or an address. It answers
  `Ack` at once and mails in the background, so neither the reply nor its
  timing shows whether the account exists. The link (`#reset=`) works for
  an hour.
- **ResetPassword:** a new password that is too short does not use up the
  link. A good one ends every session and logs this connection in.
- **Mail limits:** each account gets 3 emails, then 1 every 20 minutes.
  Each address may ask for 5 resets, then 1 every 10 minutes. The token
  buckets moved to `internal/ratelimit` so `auth` can use them.
- **Links** put the token in the URL fragment, which browsers do not send to
  the server. The web client reads it and removes it from the address bar.
  If the link opens in a tab already showing the client, the page reloads
  so the link is read.
- **`internal/mail`:** SMTP with implicit TLS on 465, otherwise STARTTLS
  required except on loopback. Plain text, quoted-printable, header
  injection refused. `mail.Log` (`-mail-log`) logs mail instead, for testing.
- **Flags:** `-smtp`, `-smtp-user`, `-smtp-password-file` (or
  `CRIBBAGE_SMTP_PASSWORD`), `-mail-from`, `-mail-from-name` and
  `-public-url` (https unless local; required with `-smtp`).
- **Admin:** `-admin-socket` serves one JSON command per connection on a
  Unix socket of mode 0600. The deploy unit puts it in
  `/run/cribbage` (`RuntimeDirectory`, mode 0700).
  `cribserver admin reset-password <user>` sets a temporary password like
  `k7mq-x2r9-pd4h`, ends all of the player's sessions and closes their
  connections. With `must_change` set, the server refuses everything but
  `ChangePassword` and `Logout` (`PASSWORD_CHANGE_REQUIRED`), and the
  clients open a password form that cannot be dismissed.
- **Clients:** an Account dialog (change password, email address),
  "Forgot your password?" on the login screen, and a reset form for links
  (web only; the Godot client points to the browser). Godot gained
  `--show history|account|forgot` for screenshots.

**Verified:**
- **Unit tests (memory and SQLite stores):** tokens are single use, expire,
  are replaced, and are of the right kind; addresses match without regard to
  case; SetPassword keeps one session and ends the rest; history pages
  without overlap, including two games with the same end time; records
  count wins, losses and unfinished games and use the latest nickname;
  migration 2 on a version-0 database.
- **Server tests:** change password, email confirmation and reset end to
  end with a fake mailer, the per-account mail limit, a server without
  mail, the admin reset with its forced change, and history paging.
- **Mail:** address checks, a message sent to a fake SMTP server, header
  injection.
- **Admin socket:** mode 0600, stale socket replaced, unknown users and
  commands.
- **Race detector:** server, store, auth, mail, ratelimit and cmd/cribserver
  pass.
- **Web client in headless Firefox (WebDriver BiDi), against a server with
  `-mail-log`:**
  - history and records after three bot games
  - adding an address, a wrong password, and confirming from the logged link
  - changing the password
  - "Forgot your password?", the reset link, mismatched passwords, the
    link refused a second time
  - the admin reset logging the page out, then the forced change, which
    cannot be dismissed
  - no horizontal scroll at 390 px, and no console errors
  - this found that a link opened in an existing tab changed only the
    fragment, hence the reload
- **Godot:** screenshots of the history, account, forgot-password and
  forced-change windows, which led to fixed-size windows (wrapped labels
  made content-sized windows far too tall). Typing into these forms in
  Godot was not exercised; they send the same messages the web client does.
  On this machine, windowed Godot needs X11 with Mesa's software renderer
  (`__GLX_VENDOR_LIBRARY_NAME=mesa LIBGL_ALWAYS_SOFTWARE=1`) when the GPU's
  memory is full.

**Not tested:** sending through a real SMTP provider. That needs your
account, and then one reset email to yourself after deploying.

### M27: Security review fixes

A last review before deploying, in three parts (accounts, network,
clients), found no critical issues. Fixed:

- **Extra data channels.** The server accepted every "cribbage" data
  channel a client opened on one peer connection but read only the first;
  each extra one could hold about 4 MiB until the client left, enough to
  run the server out of memory without an account. Now only the first is
  accepted and the rest are closed. The per-channel send buffer limit went
  from 8 MiB to 1 MiB.
- **Links outliving a password change.** A new password (changed, reset by
  email or by the admin) deletes every reset and verification link, not
  just reset links, so an address someone else asked for can no longer be
  confirmed afterwards. A new address deletes reset links sent to the old
  one. The admin reset also removes the email address and reports it.
- **Logins racing a password change.** A session is created only if the
  password hash is still the one that was checked, and after a connection
  registers, its session must still exist, else it is closed.
- **Password checks.** Changing the password or email costs a bcrypt hash
  even with the right password, so these are limited per player (5, then
  1 a minute). Failures are charged before the check and refunded on
  success, so a burst of guesses gets no more than the limit.
- **Mail.** Confirmation emails are limited per recipient as well as per
  account (3, then 1 an hour) and no longer include the nickname.
- **Before login,** a remote connection may send 50 messages.
- **Godot** refuses plain `ws://` except to this machine or the local
  network, and suggests `wss://`.
- **systemd:** a syscall filter, no capabilities, more `Protect*`
  settings, and memory (with a matching `GOMEMLIMIT`) and task limits.

Left for later (low): seeding the shuffle with 128 bits from
`crypto/rand`, per-/48 limits for IPv6, idle timeouts for connections
with no games, a hard cap on rate-limiter tables, owner-only permissions
on the Godot settings file, an Origin check on `/rtc`, and checking the
seat before waking a sleeping game.

Tests: an extra data channel is closed (fails on the old code); store
tests for links surviving password and address changes and for sessions
of an old password; a login racing a reset gets no session; password
checks limited; a burst of 12 wrong passwords checked only up to the
limit (12 of 12 on the old code); the pre-login cap; the admin reset
removing the address. All tests and the race detector pass. The server
started and answered under the new systemd sandbox settings (run with
`systemd-run --user`, which cannot apply every setting).

### M28: Protocol versions, account deletion, encrypted emails, privacy

- **Protocol version.** Every ClientMessage carries `protocol_version`
  (`PROTOCOL_VERSION_CURRENT`, now 1). The server answers any other with
  UPGRADE_REQUIRED and closes the connection. The web client reloads once
  to pick up the new page, then says it is out of date; Godot says so and
  stops reconnecting.
- **Deleting accounts.** DeleteAccount with the password: with a confirmed
  address, a link (an hour, single use) is mailed and ConfirmDeleteAccount
  deletes; without one, the username is typed again and the account goes at
  once. Deleting marks the user so they can start nothing, closes their
  connections, cancels open games and resigns active ones, drops invites,
  deletes the user, sessions and tokens, and renames them "Deleted player"
  in finished games (IDs are AUTOINCREMENT, never reused). `cribserver
  admin delete-account <user>` does the same. A password change also
  cancels a pending deletion link.
- **Email addresses encrypted** (AES-256-GCM, bound to the user ID) under
  `-email-key-file`, with a keyed hash for lookups; the first key used is
  remembered and a different one refused. The old address is told when an
  address is changed or removed.
- **Mail outside the hashing slot.** Setting an email and deleting check the
  password under the bcrypt semaphore, then send mail without holding it.
- **Report a bug** (`-report-url`, served in `/api/config`) and **Privacy**
  (`/privacy.html`) links in both clients.
- **Last messages before a close arrive.** A WebRTC close now waits for
  queued data to drain (up to 2 s) plus a moment, and the Go client's Read
  returns queued messages before EOF. Before, an error sent just before
  closing (and the Logout ack) was often lost.

Tests: protocol version refusal; deletion by link (games resigned, invites
dropped, opponent's history kept), without an address, and by the admin;
notices to the old address; email encryption (nothing in the clear in the
database file, wrong key refused, rows cannot be swapped); store deletion
with ID reuse; last message delivered on close. The web client was driven
end to end in headless Firefox (links, both deletion paths, used link
refused, version mismatch reload then "out of date", privacy page at 390
px). Godot: built, and screenshots of the login links, settings menu,
account window with the delete section, deletion, and the upgrade message.

### M29: Two WebRTC connection races

`TestRemoteIP`, `TestCodecMirroring` and `TestCloseDeliversLastMessage`
failed now and then (a few percent of runs). pion's debug logs showed two
server-side races, both able to hit real players:

- **Signaling closed early.** A client closes the signaling WebSocket as
  soon as its side of the data channel opens. If that close reached the
  server before the server's side opened, the server took it for a failure
  and closed the peer connection. Over the internet the close (TCP) and the
  channel's opening (UDP) arrive in either order. Now, once the server has
  sent its answer, a closed WebSocket is normal and it waits for the
  channel until the signaling timeout, as the Go client already did.
- **First message dropped.** The server made its Conn, and so set
  OnMessage, in the channel's OnOpen. pion runs OnOpen on its own goroutine
  and starts reading at the same moment, dropping messages that arrive
  before OnMessage is set, such as a client's first (Resume or Login): the
  client then waited for an answer that never came. The Conn is now made
  in OnDataChannel, which pion runs before the channel opens.

Undoing either fix brings the failures back (most batches of 100 fail);
with both, each test passed 100 runs in a row, and the full suite passed
twice and under the race detector.

## M30: config file

`cribserver -config <file>` reads settings as `name = value` lines, the
flag names without the dash, so a deployed server's command line is just
`-config /etc/cribbage/cribserver.conf` (`deploy/cribserver.conf`).
Command-line flags override the file. Unknown or repeated names are
errors, values may be quoted, and `${NAME}` expands an environment
variable, which lets the file name systemd credentials as
`${CREDENTIALS_DIRECTORY}/email-key`. `-print-config` writes a file with
every setting and its help, those at their defaults commented out.

## M31: no passwords

Passwords are gone. People sign in with an 8-digit code emailed to them
(or the link in the same email, which signs in the browser that opens
it), a passkey in the web client (go-webauthn, discoverable credentials,
tied to `-public-url`'s host), or, in the desktop app, by approving the
code it shows from a signed-in device (`StartDeviceSignIn`,
`ClaimDeviceSignIn`, `ApproveDevice`, `DeviceApproved`). Registering
mails a code and creates the account only when it is used; an address may
be shared by several accounts, and then one email carries a code per
account. Bots sign in to bot accounts (no address) with `-bot-key-file`;
the key cannot open a person's account. Protocol version 2.

- **Codes.** A client gets a secret challenge and can try 5 codes against
  it; an account takes 10 wrong codes from anywhere, then 1 an hour, so
  guessing an 8-digit code would take millennia. Asking again resends the
  same code while most of its 15 minutes are left. Codes and challenges
  live in memory only. Emails: 10 per account and 20 per address, then 1
  every 5 minutes; 10 code requests per IP, then 1 a minute.
- **Sessions** last 90 days from last use (renewed at most daily).
  "Sign out other devices" ends the rest. A connection that has started
  signing in may stay 20 minutes instead of 30 seconds.
- **Address changes** need a link to the current address, then one to the
  new address, so a device left signed in cannot take the account; the old
  address is told. `cribserver admin set-email` recovers an account: new
  address, every session ended, passkeys removed.
- **Mail.** Without `-smtp`, emails go to the log (testing). Without
  `-email-key-file`, a key is created beside the database.
- Tests: a software passkey authenticator (`internal/auth/passkeytest`)
  drives the real WebAuthn checks; the web client's passkey code was run
  end to end in headless Firefox against an in-page software authenticator.

---

# Expert bot improvements (M32 onward)

Started 2026-11-23 from `docs/BOT_IMPROVEMENT_PROPOSALS.md` (ten proposals,
four enablers E1 to E4). The goal is general strength: a change is accepted
on its paired result head to head against the previous expert, and must not
lose ground against the guards (`heuristic`, `heuristic2`, `models-big`).
`models-expert/expert.bin` (M14) stays as the reference bundle. Runs and
their pair files live in `server/runs/` (gitignored); the commands below
regenerate them.

### M32: A more sensitive test harness (E1)

**Built (2026-11-23):** `cmd/gauntlet` and `internal/ai`.
- `-save-pairs DIR` writes every deal of a match (wins, margin, pegging,
  hands, a hash of the play) to `DIR/<opponent>.csv`. `-base DIR` plays a
  candidate on the same deals and reports its change deal by deal. Every
  bot is deterministic and the engine's generator is used only for dealing,
  so a deal both played the same way differs by exactly zero; only deals
  where the candidate deviated add variance. The random bot is reseeded per
  deal so it couples too.
- `-sprt LO,HI` stops a match once a GSPRT (normal approximation, variance
  from the data) decides between the two win rates, or with `-base` the two
  changes. Defaults: alpha 0.005 (0.05 over ten candidates), beta 0.05, no
  decision before 2000 pairs.
- Evaluation seeds default to 900001; seeds below 900000 are for building
  tables, so tables are never scored on the deals they came from.
- A/A: the expert against its own saved baseline gives exactly zero, with
  no deviating deal, over 1000 deals; so does `heuristic2` against
  `heuristic` and `random`.
- For the head-to-head gate the plain paired arena already has this
  property (a deal played identically splits one win each), so coupling
  mainly helps the guards. There the head-to-head needs about 100k pairs to
  resolve 0.2 points: per-pair standard deviation about 0.19 (M14 round 2).

Baselines, `gauntlet -bot models-expert/expert.bin@400 -n 40000 -save-pairs
runs/base-m14` (seed 900001, 24 workers at nice 19, other jobs running):

| Expert@400 v | Win % | Margin per game | Pegging per hand |
|---|---|---|---|
| `heuristic` | 57.22 ± 0.32 | +1.95 ± 0.09 | +0.249 ± 0.007 |
| `heuristic2` | 55.98 ± 0.31 | +1.36 ± 0.09 | +0.159 ± 0.007 |
| `models-big` | 56.00 ± 0.32 | +1.97 ± 0.10 | +0.168 ± 0.007 |
| itself `@400,nokeep` | 51.98 ± 0.26 | +0.90 ± 0.07 | +0.087 ± 0.006 |
| itself `@400,heurdiscard` | 51.53 ± 0.28 | +0.13 ± 0.08 | +0.089 ± 0.005 |
| itself `@25` | 51.56 ± 0.26 | +0.66 ± 0.07 | +0.069 ± 0.006 |
| itself `@400,nopeg` | 50.07 ± 0.20 | +0.04 ± 0.05 | +0.003 ± 0.004 |
| itself `@0` (every world) | 49.92 ± 0.15 | −0.06 ± 0.03 | −0.004 ± 0.003 |

- The M14 figures reproduce on held-out seeds (57.16 and 55.85 then).
- The keep model is worth about 2 points and the discard tables about 1.5.
- The pegging-play likelihood is worth nothing measurable (M13 reported
  +0.34 ± 0.28 from a run ten times smaller), so proposal 2 must bring a
  genuinely better model to gain anything.
- Effort 400 plays as well as searching every world: search is saturated.
- Times: 40k games took 5.5 to 6 minutes against a guard, 11 minutes
  expert against expert and 25 at effort 0; the whole run 83.5 minutes.
  Games average 9.05 hands.

### M33: Speed (E3, first part)

**Built (2026-11-23):** `internal/expert` (`keep.go`, `search.go`).
- The keep model is a dense table: six sorted ranks are numbered by the
  combinatorial number system (`rank6`, 18,564 slots) instead of a map
  key. A second layout, by kept shape and thrown pair (1.3 MB), serves the
  search's opponent weights directly. The per-hand cache of those weights
  is an array with generation stamps.
- Opponent shows are scored only for the worlds searched, not for every
  world enumerated.
- Search nodes pass by value: a pointer to a copied node escaped to the
  heap through the recursion, one allocation per node (15% of the time).
- The benchmark game went from 109 to 61 ms (one thread, loaded machine).
- Identical play: coupled against the saved M14 baseline, 5000 deals with
  `heuristic`, none deviated.
- The double-dummy pegging database is left for proposals 1 and 4, whose
  needs decide its shape. Discards (2.5 ms) were not touched.

Latency on the production t4g.micro (Graviton, one worker, nice 19, 200
games against `heuristic`, `GOARCH=arm64`):

| Build | Pegging mean / p99 | Discard mean / p99 |
|---|---|---|
| M14 | 4.4 / 33.6 ms | 2.5 / 5.9 ms |
| M33 | 1.6 / 14.1 ms | 2.5 / 5.9 ms |

Map access was dearer on ARM than on x86, so the gain there is 2.7x. The
working budget is about 200 ms per decision on one core.

### M34: Oracles that bound the headroom (E2)

**Built (2026-11-23):** expert options `oracle=six` and `oracle=hand`
(test only: `bots.Watcher` hands the bot the game), and `cmd/expert
rollout`. `engine.NewHand` starts a game at a given deal.
- `oracle=hand` searches only the opponent's real hand (clairvoyant
  pegging). `oracle=six` searches the hands four of their six dealt cards
  make, weighted by the keep model's chance of keeping each from those
  six. Discards are unchanged.
- `rollout` takes discard decisions from expert self-play (seed 950001),
  and for each deals 1000 opponent sixes and starters shared by all
  options. The opponent discards with the expert's discarder (it cannot
  see mine), both players peg at effort 25, and the hand's end is valued
  through the win table. Each policy's regret is the best option's value
  less its choice's: "naive" picks the best on all samples (biased high),
  "split" picks on one half and scores on the other (biased low).

Pegging oracles, 40k games each (seed 900001):

| `@400` with | v expert@400 | Pegging/hand | v `heuristic` (change, coupled) |
|---|---|---|---|
| `oracle=hand` | 70.38 ± 0.34 | +0.994 ± 0.006 | +18.62 ± 0.40 |
| `oracle=six` | 69.36 ± 0.34 | +0.957 ± 0.006 | +17.95 ± 0.39 |

- Hidden cards are worth about a point of pegging per hand. That is a
  ceiling on what any inference or search change could reach, not an
  estimate: the doc's "10 to 25% reachable" would mean 2 to 7 win points
  from pegging alone, more than its own realistic total of 1 to 3 for all
  proposals together. The kill rule for proposals 1, 2 and 9 (gap under
  0.5) passes, but at this size it would pass for almost any oracle.
- Proposal 1 corrects strategy fusion, a structural error, which no oracle
  here measures; it has to be tested directly.
- The six-card oracle is not a bound on proposal 3. It shows that once the
  six cards are known the current keep model recovers nearly everything
  (0.957 of 0.994), but proposal 3 sharpens the keep model when the six is
  unknown, as in play. Proposal 3 is cheap, so it is judged on its own
  test (probability of the true keep on held-out hands, then games).

Rollout discards, `expert rollout -decisions 2000 -samples 1000 -out
runs/e2-rollout.csv` (49 minutes, 24 workers). Regret per decision:

| Policy | Agrees with best | Win % naive | Win % split | Points naive | Points split |
|---|---|---|---|---|---|
| expert | 82.8% | 0.090 ± 0.022 | 0.042 ± 0.023 | 0.078 ± 0.012 | 0.061 ± 0.012 |
| `heuristic2` | 79.8% | 0.181 ± 0.042 | 0.132 ± 0.041 | 0.081 ± 0.011 | 0.063 ± 0.011 |
| `heuristic` | 77.8% | 0.249 ± 0.050 | 0.200 ± 0.050 | 0.086 ± 0.011 | 0.069 ± 0.011 |

- Regret in points cannot tell the expert from the heuristic, which is 1.5
  win points worse, so the doc's kill rules in points (0.02, 0.04) are not
  applied. Win-probability regret separates them.
- Calibration: at 9.05 discards per player per game, the heuristic's
  extra regret is (0.200 − 0.042) × 9.05 = 1.43 win points (naive 1.44),
  against 1.53 ± 0.28 measured for `@400,heurdiscard`. The oracle is
  calibrated.
- The expert's own regret is therefore 0.4 to 0.8 win points per game:
  the combined ceiling for proposals 4 and 5 against an expert-like
  opponent, with playouts at effort 25.
- `runs/e2-rollout.csv` holds every option's values, the offline test set
  for proposal 4; the command above regenerates it.
- Times: oracle matches 1 to 4 minutes each (an oracle searches about 30x
  less per decision), rollouts 49 minutes; the whole run 59 minutes
  against an estimate of 78.

Kill rules, applied:
- Proposals 1 and 2: proceed; proposal 2's ablation (`nopeg` = 0) means
  the current likelihood gives nothing to build on.
- Proposal 3: proceed, judged on its own test.
- Proposal 4: proceed, with its gain capped by the 0.4 to 0.8 measured.
- Proposal 5: defer; it would only add to what 4 leaves of that ceiling,
  at the cost of a Python step.
- Proposal 9: unchanged, last.

### M35: A persistent corpus (E4)

**Built (2026-11-23):** `expert corpus` plays games between any two player
specs and saves every hand in full: both deals, discards, starter, plays
in order, every scoring step, shows, crib, pegging and the winner (a
64-byte `HandRecord`, file magic `CRIBCORP1`).
- Game i deals from `ai.DealSeed(seed, i)`, so a corpus does not depend on
  the number of workers. `-explore` makes a seat discard at random on a
  share of hands (flagged), and `-start LO,HI` starts games at random
  scores to cover the endgame (flagged synthetic).
- `CorpusSelfPlay` turns a corpus into the `SelfPlay` the table builders
  take, so tables can be rebuilt without replaying games; the test checks
  it matches `Collect` exactly.
- Size and speed: 10k games are about 90k hands and 5.8 MB, 1.5 to 3
  minutes at `@400` on 24 workers under load.

### M36: The exact keep table (proposal 3)

**Built (2026-11-23):** `expert keepx` runs the bundle's own discarder on
every six-card hand, on both sides, at 30-30, and stores the share of
each rank shape's hands that keep each option (the `keepx` table, 3.4
minutes on 24 workers: 18,395 rank shapes, 962,988 suit classes). The
opponent model becomes (1-eps)·exact + eps·softmax; `noexact` and
`keepeps=X` are the options.

Probability of the real keep in held-out games (`runs/p3-keepcheck.txt`,
mean probability and mean log probability):

| Opponent | Softmax | eps 0.2 | eps 0.3 | eps 0.5 |
|---|---|---|---|---|
| self (expert) | 0.644 (-0.926) | 0.765 (-0.753) | 0.750 (-0.743) | 0.720 (-0.752) |
| `heuristic` | 0.635 (-0.866) | 0.712 (-0.880) | 0.702 (-0.835) | 0.683 (-0.796) |
| `heuristic2` | 0.634 (-0.854) | 0.705 (-0.889) | 0.696 (-0.841) | 0.678 (-0.796) |
| `models-big` | 0.607 (-1.034) | 0.658 (-1.163) | 0.651 (-1.096) | 0.639 (-1.024) |

- The pure table (eps 0) is sharpest but fatal on log-likelihood when an
  opponent keeps something the expert would not. At eps 0.5 it beats the
  softmax for every opponent, including bots it was not built from.

Games (`@400` with the table v `@400` without, both `nofixes`):

| eps | Games | Head to head | Pegging/hand |
|---|---|---|---|
| 0.3 | 40k | 50.11 ± 0.17 | |
| 0.5 | 40k | 50.15 ± 0.16 | +0.004 ± 0.003 |
| 0.5, sequential test 0 v +0.3 | 71k (stopped) | 50.11 ± 0.12 | +0.004 ± 0.002 |

- The sequential test accepted +0 after 35,656 pairs. Coupled guards
  against `runs/base-m14` were flat at both eps (eps 0.5: heuristic -0.03,
  heuristic2 +0.12, models-big -0.04, each ± 0.17).
- **Decision:** `DefaultKeepEps` is 0.5. The table passes its own test,
  costs nothing at run time, and gives a small real pegging gain, but no
  win-rate gain this size of test can see. It also feeds proposals 2 and
  4.

### M37: Proposal 6's small fixes, and endgame starts

**Built (2026-11-23):**
- Heels are scored before pegging in discard values from 90 up (a jack
  starter can win the game for the dealer at the cut).
- When both players can peg out in the hand, the discard values who gets
  there first (`pegOutFirst`) instead of calling it even.
- The pegging search's terminals add the opponent's expected nobs and
  flushes from hidden cards (`hiddenSuits`).
- From 90 up, the crib at pegging terminals depends on the world
  (`worldCribs`): what they kept says what they threw.
- `nofixes` restores M14's play exactly (0 deviations against
  `runs/base-m14`), so a same-binary opponent can play without the fixes.
- `gauntlet -start LO,HI` (`ai.ArenaFrom`) starts every pair at a random
  hand with both scores in [LO, HI]; both games of a pair start from it
  with the seats swapped.

| `@400` v `@400,nofixes` | Games | Head to head | Pegging/hand |
|---|---|---|---|
| Normal starts | 40k | 50.15 ± 0.10 | +0.000 ± 0.002 |
| Starts at 90-120 | 40k | 50.09 ± 0.09 | +0.003 ± 0.009 |

- Coupled guards against `runs/base-m14`: heuristic -0.00, heuristic2
  +0.02, models-big -0.08 (each ± 0.10; 42% of pairs deviated).
- About +0.1, inside the proposal's +0.05 to +0.15. The fixes stay on.
- Times: 8 minutes for the normal head to head, 40 seconds for the
  endgame one (games starting at 90+ last about a hand and a half).

### M38: A fitted opponent pegging policy (proposal 2)

**Built (2026-11-23):** a conditional logit on each play the opponent could
make (`pegpolicy.go`), fitted by Newton's method to corpus plays (`expert
pegfit`) and stored as the bundle's `pegpol` table. It replaces the softmax
on immediate points in weighting worlds; `nopegpol` turns it off.
- Features: points scored, the expected points of the reply (over unseen
  ranks), the count left at 5, 10, 21 or 22-30, the card's value, leads
  (value, fives), and from the rest of the hand: a pair held (and leading
  from one), a card making 15 with it, the share of the rest still
  playable, and holding nothing playable. Reply risk takes its belief from
  the public cards only, in the fit and in play alike.
- Public features are computed once per opponent play for all 13 ranks;
  only the five hand features are worked out per world, so weighting costs
  no measurable time (ARM: 1.9 ms per pegging decision with or without).
- Fitted to 1.42M decisions from 30k games (expert self-play, expert v
  `heuristic`, expert v `models-big`, `runs/p2/`, seeds 200001-220001).

Held-out log-likelihood per decision (softmax at temperature 1 is the old
model; the best temperature for it is 1):

| Decisions | Fitted | Softmax |
|---|---|---|
| expert self-play (new seed) | -0.734 | -0.888 |
| `heuristic2` (not in the fit) | -0.582 | -0.833 |

The proposal's own test, the share of world weight on the opponent's real
hand (mean log, `pegcheck_test.go`, 400 games of each bot):

| Opponent | Gain | Positions where the share falls below 1/e of the old one |
|---|---|---|
| `heuristic2` (held out) | +0.204 | 0.7% |
| `heuristic` | +0.135 | 2.8% |
| `models-big` | +0.067 | 5.0% |
| `random` | -0.317 | |

Games, `@400` with the policy v `@400` (both with proposal 6's fixes):
- Head to head: **50.71 ± 0.35**, margin +0.31 ± 0.09, pegging +0.037 ±
  0.007; the sequential test accepted +0.3 after 8,918 pairs.
- Coupled guards (`nofixes`, against `runs/base-m14`): `heuristic` -0.13 ±
  0.26, `heuristic2` **+0.93 ± 0.27** (pegging +0.046), `models-big` -0.31 ±
  0.29.
- The gain is large where the policy models the opponent well (the expert
  itself; `heuristic2`'s leads) and absent against `heuristic`, whose plays
  the old points-only model already described. Against the network it may
  cost a little: a sharper model is more confidently wrong about an
  opponent it does not fit, which matters for humans.
- `pegmix=X` mixes the old softmax into each play's chance at weight X. At
  0.2 the bad tail against `models-big` falls from 5.0% to 1.8% and the mean
  gain there rises to +0.078 (heuristic2 +0.179, heuristic +0.125).

With `pegmix=0.2`:
- Head to head v `@400`: **51.17 ± 0.47**, margin +0.43 ± 0.12, pegging
  +0.042 ± 0.010 (stopped at H1 +0.3 after 4,594 pairs).
- Guards: `heuristic` -0.14 ± 0.25, `heuristic2` **+1.00 ± 0.25**,
  `models-big` -0.20 ± 0.27.
- Both head-to-head runs stopped at H1 of the sequential test, so their
  estimates read high and their ± is not a fixed-sample interval; the
  guards are fixed-length (40k games each).
- **Decision:** the policy is on whenever the bundle has one, mixed at 0.2
  (`DefaultPegMix`). No guard loses significantly; the mix costs nothing
  measurable and protects against opponents unlike the fitted bots.
- Times: the corpus took 9 minutes, the fit 11 seconds.

### M39: Information-set pegging search (proposal 1)

**Built (2026-11-23):** `is` searches the worlds together (`issearch.go`):
a node is the public play history and the worlds still consistent with it;
at my nodes one card is chosen for all of them by the weighted sum; at the
opponent's nodes each world's opponent plays the card worst for me.
Worlds are regrouped after every play by the public state that follows.
With `islambda=X` and a pegging policy, the opponent's nodes blend best
reply (weight X) with the policy's expectation, and the worlds below an
opponent play are weighted by the policy's chance of that play, so my
later choices infer from it as the root does.
- Checked: with one world it equals minimax exactly (3,000 positions); the
  blend matches a brute-force reference over 2-4 worlds (1,600 root plays),
  and that test fails if the inner reweighting is dropped.
- Scratch comes from a pooled stack buffer sized per decision, so a
  decision allocates nothing.

Latency on the production t4g.micro, one game at a time, pegging decisions:

| Bot | Mean | p99 | Max |
|---|---|---|---|
| `@400` | 1.9 ms | 17 ms | 22 ms |
| `@400,is` | 6.8 ms | 67 ms | 91 ms |
| `@400,islambda=0.5` (policy computed once per node) | 12.4 ms | 134 ms | 161 ms |

Games:

| Bot | Opponent | Games | Head to head | Margin | Pegging/hand |
|---|---|---|---|---|---|
| `@400,is` | `@400` | 32k (stopped, H0 v +0.5) | 50.12 ± 0.28 | +0.15 ± 0.07 | +0.015 ± 0.006 |
| policy `@400,is` | policy `@400` | 12k (stopped, H1 +0.3) | **51.07 ± 0.45** | +0.53 ± 0.12 | +0.053 ± 0.010 |
| policy `@400,islambda=0.5` | policy `@400,is` | 28k (stopped, H1 +0.3) | **50.50 ± 0.27** | +0.16 ± 0.07 | +0.017 ± 0.006 |

- Both runs were stopped by the sequential test, so their estimates read
  high (a test stopped at H1 stops on a run of good luck) and the ± is not
  a fixed-sample interval. The fixed-length check of the whole stack is
  M41's.
- Alone the search gains little; on top of proposal 2 it gains a point.
  At my nodes the search averages over the worlds, so it is only as good
  as their weights, and proposal 2 is what makes them good.
- The blend adds about half a point more: an opponent who is not
  clairvoyant does not always find my worst case, and planning for the
  plays they will actually make pays. (These runs used the policy without
  the mix.) Smaller λ is not yet tried.
- The whole stack is measured at fixed length in M41.

### M40: Discards conditioned on the opponent's hand (proposal 4)

**Built (2026-11-23):**
- `cond`: once per discard decision, every (kept four, thrown pair) the
  opponent may hold is weighted by the ways it can be dealt from the 46
  unseen cards and by the keep model, filling a table by starter rank,
  their show and their pair (about 165k combinations, shared by the 15
  options). Their show then follows exactly from their four and the
  starter, the crib from my pair, their pair and the starter, and the
  pone's total keeps the two joint. Ranks only, with hidden nobs at its
  chance of a suit match; my own discarded jack's nobs is exact. Checked
  against sampling (means within 0.01 to 0.02 points).
- `condpeg`: a shape-pair pegging table (`expert pegpair`, every pair of
  kept shapes played 4 times with the pegger at effort 25 from 0-0, 1 h 4
  min, 27 MB), and each option's pegging distributions shifted by how the
  opponent's likely shapes move its means, relative to the table's average
  opponent.
- `expert rollcheck` scores discarders on stored rollouts; `expert
  rollpair` finds where two discarders disagree in self-play and plays out
  just those two choices on shared samples.

| Test | `cond` | `condpeg` |
|---|---|---|
| Stored rollouts (2,000 decisions): decisions changed | 93 | 120 |
| ... change in rollout win % per decision | +0.011 ± 0.007 | +0.001 ± 0.018 |
| ... regret | 0.069 ± 0.014 (from 0.081) | 0.080 ± 0.021 |
| `rollpair`: disagreements in 40k decisions | 1,966 (4.9%) | |
| ... gain where they differ, win % | **+0.153 ± 0.035** | |

- `cond` is a real but small gain: 4.9% of discards change, each by 0.15
  win points, so about +0.07 win points per game, too small for games to
  see. It costs 25 ms per discard on the t4g.micro (2.6 ms before).
- `condpeg` is unresolved rather than refuted: +0.001 ± 0.018 covers all
  of `cond`'s gain. With 4 samples a pair's mean has a standard deviation
  of about 1.2 points, which can swamp the interaction it is meant to
  carry. A smoothed model of the interaction, or many more samples, might
  do better; neither is pursued now.
- **Decision:** `cond` goes into the top level; `condpeg` and its 27 MB
  table stay out.
- Times: rollcheck 2 to 3 minutes per set of policies; rollpair 38
  minutes (35 of them playouts) against an estimate of 15 to 20, slowed by
  three other jobs.

### M41: The whole stack, and proposal 6's endgame parts

The stack: the exact keep table (eps 0.5), the pegging policy mixed at 0.2,
IS search at λ 0.5, `cond` discards and proposal 6's small fixes, in one
bundle (`runs/final/expert-v1.bin`: M14's tables plus `keepx` and
`pegpol`), spec `@400,islambda=0.5,cond`.

Fixed length, 40k games (seed 950001) against M14's play
(`expert.bin@400,nofixes`): **52.13 ± 0.27**, margin +0.94 ± 0.07 per game,
pegging +0.096 ± 0.006 per hand. About two points, at the top of the
proposals' own realistic range (+1 to +3).

Proposal 6's endgame parts:
- **Win table:** 100k games of the stack started at random scores in
  60-120 (210k hands, 30 minutes) rebuilt W where both scores are 70 or
  more (`expert wintable`). Held-out log loss on 20k more such games:
  0.28217 for M14's table, 0.28182 rebuilt (window 500 hands; worst 10x10
  bin |z| 3.9 to 2.9). In 40k games started at 90-120 the two tables play
  **49.98 ± 0.05**: no difference. M14's table is already calibrated in
  the endgame; it stays.
- **Need-aware discard solver:** not built. On the stored rollouts the
  expert's discard regret is 0.149 win % per decision when either score is
  90 or more (495 decisions) against 0.058 below, so about half the
  remaining discard regret is in the endgame. But at run time a rollout of
  ~50 samples per option has about 7% noise per value against differences
  of about 0.15%; it cannot resolve them within the latency budget.

Guards, 20k games each, coupled against `runs/base-m14`:

| Opponent | Win % | Change from M14 | Margin change | Pegging change |
|---|---|---|---|---|
| `heuristic` | 58.57 ± 0.46 | **+1.21 ± 0.46** | +0.48 ± 0.11 | +0.038 ± 0.009 |
| `heuristic2` | 58.47 ± 0.45 | **+2.31 ± 0.47** | +0.93 ± 0.12 | +0.086 ± 0.009 |
| `models-big` | 57.27 ± 0.45 | **+1.34 ± 0.49** | +0.52 ± 0.13 | +0.046 ± 0.010 |

- Every guard improves, by 1.2 to 2.3 points; the stack generalizes beyond
  the expert-like opponents its policy was fitted to.
- Times: head to head 41 minutes, guards 31 minutes (about 16 to 37 games
  a second under load against estimates of 2 to 2.5 hours in all).

---

# Roadmap: three-player games (not started)

Noted 2026-11-15, to look at soon. The goal is three-handed cribbage,
mainly so that **two people can play against a bot** and see how they
measure up, and so that **one person can play two different bots**.
Three people would come for free.

## Rules to implement (standard three-handed)

- Five cards each, plus one dealt straight to the crib; each player
  discards one, so hands and the crib are four cards as now.
- The deal passes to the left. The player to the dealer's left leads, and
  play goes round to the left.
- Pegging: a player who cannot play says go and the next player carries
  on. When nobody can play, whoever played last pegs 1 (2 at 31), and the
  count restarts with the player after them. Pairs and runs count across
  all three players' cards, as now.
- The show goes left of the dealer first, then the next player, then the
  dealer, then the crib. Counting order matters more with three: the
  first to 121 wins at once, wherever the show is.
- **To decide:** skunks (often not played three-handed, or only for the
  last player); what "winning" records for second and third place.

## What it touches

The two-player assumption is everywhere, so this is a large change, about
the size of a phase. In order of work:

1. **Engine** (`internal/engine`, the biggest and most careful part):
   about 25 `[2]`-sized arrays (scores, hands, discards, go flags) and
   `1 - player` turn arithmetic in `game.go`. Generalize to two or three
   seats: deal sizes, a one-card discard (`Action.Cards` holds two), turn
   order, go handling with three players, show order, and `Scores` in
   events and observations, which need a list of opponents rather than
   one. **Two-player play must stay exactly the same**: stored games
   replay from their seed and action log, and the bots' tables assume it.
   Rule-case tests for three-handed pegging and the show.
2. **Protocol:** seats and scores as lists rather than "me" and "the
   opponent"; how a three-seat game is created and listed. A protocol
   version bump, so Godot builds must be updated.
3. **Store:** `games` has `seat0_*`/`seat1_*` columns: add `seat2_*` (a
   migration) or a seats table. History and the record against each
   opponent need a meaning for three-player games (placings).
4. **Server:** creating a game with chosen seats (a bot in one or two of
   them), invites for a second person into a game with a bot, open games
   and Find a match filling three seats, limits counting three-player
   games, and the game actor's seat handling.
5. **Bots:** every bot is two-player. A first version adapts the heuristic
   bot: discarding one of five is a small enumeration, with the crib
   belonging to the dealer, who may be either opponent; pegging stays
   one-ply against two opponents. The expert bot needs new search
   assumptions and retrained tables, a project of its own; the `nn` bot
   would not play three-handed. Offer random and heuristic first.
6. **Web client:** a three-track board (the current one draws two lanes
   side by side), a table with two opponents' hands, whose crib it is,
   three scores, events and animation for a third player, and a lobby
   control for choosing seats (invite someone, add a bot).
7. **Godot client:** the same board, table and lobby changes; it could
   follow the web client.

## Suggested order

Engine with tests (and a check that two-player replays are unchanged),
then protocol and store, then one person against two bots (no invites
needed), then the web client, then two people and a bot by invite, then
Godot, then stronger three-player bots.

