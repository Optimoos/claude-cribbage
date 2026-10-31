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

## Later (out of scope now)
- Web export for Godot once .NET web support lands, or a GDScript port.
- Matchmaking and ratings.
- TURN deployment.
- Manual counting with muggins.

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
