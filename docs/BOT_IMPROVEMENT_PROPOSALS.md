# Expert bot: ten proposals to raise the win rate

Written 2026-11-23 as a handoff for an implementing agent. This is research
only: nothing here has been built or measured unless a line says so. Read
`docs/PLAN.md` lines 220-760 first; it is the history this document builds on.

## How to read this

- **Baseline.** The strongest bot is `expert` (`server/internal/expert`). It
  wins 57.16% ± 0.32 of paired games against the frozen `heuristic` and
  55.85% ± 0.45 against the `models-big` network (PLAN M14).
- **Source.** Twenty research agents each studied one area (code audits,
  literature, game theory, statistics, two small measurements). Their findings
  were merged here. The code claims marked *verified* were re-checked against
  the source by the author of this document.
- **Gain estimates are guesses.** Every "expected gain" is a researcher's
  estimate in points of paired win rate, not a measurement. Several estimates
  for the same idea disagreed by a factor of two or more; where they did, the
  range is given and the disagreement is noted.
- **Gains do not add.** Proposals 1-3 overlap, as do 4-6. A realistic total for
  general strength is +1 to +3 points head to head against the current expert,
  which is roughly 58 to 60% against the heuristic.
- **Scope.** The owner's goal is general strength. Proposals 1-6 and 10 are
  the active set. Proposals 7 and 8 (opponent-specific play) are deferred and
  proposal 9 (randomized play) is last; see "Decisions from the project
  owner" at the end.
- **Conversion rule.** From the PLAN's results, one point of margin per game is
  worth about 3 win-rate points (range 2.2 to 3.65), and 0.01 pegging points
  per hand is worth about 0.2 to 0.3. Pegging points per hand is the most
  sensitive metric the arena already reports.

## What the research found

These are the facts the proposals rest on.

**Verified in the code**

- **Pegging search is clairvoyant on both sides.** `search.go:170-197` runs
  alpha-beta per sampled world with both hands known. The opponent minimizes
  knowing my cards, and my own later plays are chosen knowing theirs. Only the
  root move is a real decision under uncertainty. This is the "strategy fusion"
  the PLAN already suspected (M11 note).
- **The pegging likelihood ignores leads.** `search.go:416-470` is a softmax on
  the points a play scores at once. Every zero-point play, including every
  lead, is equally likely, so a lead carries no information.
- **The crib at pegging terminals is world-independent.** `cribDistribution`
  (`search.go:268-287`) draws the opponent's two crib cards uniformly from all
  unseen cards. `keepWeight` already computes the posterior over the thrown
  pair and discards it.
- **Opponent shows drop flush and nobs** whenever a card is hidden
  (`search.go:514-533`).
- **The keep model is rank-only and score-blind.** `keep.go:24-43` ignores
  suits, so it mis-scores four-flush hands (about 10% of deals). It is a
  softmax approximation of a policy that is actually a deterministic argmax;
  the real keep gets probability 0.54 on average (PLAN M13).
- **Discard pegging is two independent marginals keyed by my shape only.**
  `discard.go:144-169` multiplies my pegging distribution by theirs. Nothing
  depends on what the opponent is likely to hold. Both players pegging out is
  scored as a flat 0.5 (`discard.go:157`).
- **Discard tables come from hands starting below 70-70** (`hands.go:96`),
  from self-play at effort 25, yet they drive endgame decisions at effort 400.
- **Policy iteration accepts on a coin flip.** `cmd/expert/iterate.go:81`
  accepts a round at ≥ 50.0%; round 2 was 50.02 ± 0.38.
- **No self-play corpus is kept.** Each round replays about 70 minutes of
  games and throws them away; `HandStat` omits the play sequence and the
  opponent's six cards.

**Measured during this research (small samples)**

- **Profile at effort 400.** The keep-model lookup is 37% of CPU (map access
  and a linear scan), alpha-beta 31%, `ScoreHand` 12%, discards 11%.
- **Double-dummy pegging is tiny.** A 4-card v 4-card pegging game has about
  296 unique states and solves in about 1.4 ms with naive maps.
- **Crib error is small.** Comparing the crib table with the exact crib over
  sampled opponent hands: rms error 0.16 to 0.30 points, and the best discard
  changes in 4 to 15% of hands, for a regret of only 0.005 to 0.02 points per
  hand. The largest error is against the heuristic as dealer.
- **Table noise.** Two bundles built from independent small samples disagree
  on 18% of discards; extrapolated to production size, about 2 to 3%.
- **Ablations were inconclusive.** The diagnostic runs reached only 300 to 500
  paired games each because the machine was loaded by the other agents.
  Nothing can be concluded from them; they must be rerun (see E1).

**Evidence of saturation**

- Searching all worlds equals searching 400 (54.04% v 54.05%).
- Policy iteration round 2 gained nothing.
- So more compute or more rounds of the same model will not help. Gains must
  come from a different model class: information-set search, sharper
  inference, joint rather than marginal tables, or opponent-specific play.

**Prior art**

- No documented open cribbage program is ahead of this one. Reinforcement
  learning on game outcome has failed repeatedly elsewhere, as it did here.
- One paper's abstract (Kelly and Churchill 2017) reports information-set MCTS
  beating determinized search in cribbage. The paper itself could not be
  fetched.

## Deployment constraints

The bot is served from an AWS t4g.micro: 2 burstable ARM vCPUs and 1 GB of
memory, shared with the game server. Every proposal must respect this.

- **Latency is single-core ARM latency.** Cost figures in this document were
  estimated on a desktop x86 core, and several researchers assumed 24 cores
  per decision. Treat them as optimistic by a small multiple and measure on
  the instance (or an equivalent Graviton core) before accepting a change.
  The working budget is about 200 ms per decision on one core.
- **Burst credits.** Sustained CPU use drains the instance's credits and then
  throttles it. Heavy per-decision work must be rare (for example, the endgame
  solver) or cheap on average.
- **Memory.** Tables loaded at run time must stay small: tens of megabytes in
  total. The 13 MB pair table in proposal 4 fits. The 2 to 6 GB blueprint in
  proposal 9 does not fit this instance. The owner is open to a larger
  instance for it, but only if the measured gain justifies the added cost.
- **All building and training happens elsewhere**, on the development machine
  or a temporary large AWS instance; the server only loads the results.
- **Report latency with every result**: mean and p99 per decision for discard
  and pegging, as the PLAN's milestone tables do.

## Enablers (do these first; they are not counted in the ten)

Most proposals are worth 0.1 to 1.0 points. The current harness needs 40k
paired games to see ±0.32, so without these enablers most results will be
noise.

**E1. A more sensitive test harness**

- **Baseline-coupled A/B.** Store per-deal results of the baseline expert
  against each opponent for a fixed seed set. Run the candidate on the same
  deals and report the mean and interval of the per-deal difference. Every bot
  is deterministic and the deck (including the starter) is fixed per seed, so
  games where the candidate never deviates contribute exactly zero. Estimated
  3 to 20 times fewer games for small changes. Validate with an A/A run, which
  must give exactly zero.
- **Sequential test.** Replace fixed-size runs with a sequential probability
  ratio test (as chess engines do), with a correction for testing ten
  candidates.
- **Two gates, not one.** Primary, and the owner's stated goal: head to head
  against the previous accepted expert (general strength). Guard: win rate
  against `heuristic`, `heuristic2` and `models-big`, which must not fall. A
  change that gains against the heuristic but loses head to head is
  exploitation and is rejected.
- **Seed hygiene.** `gauntlet` and `expert build` both default to seed 1, so
  tables are evaluated on deals they were built from. Keep a held-out seed set.
- **Rerun the baseline and ablations** on an idle machine at 20k+ pairs:
  expert v `heuristic`, `heuristic2`, `models-big`, and expert v its own
  `nokeep`, `nopeg`, `heurdiscard`, `@25` and `@0` variants.

**E2. Oracle experiments that bound the headroom**

Build these as test-only `Options` variants. They take about a day and decide
which proposals deserve the effort.

| Oracle | What it knows | What it bounds |
|---|---|---|
| Six-card | Opponent's six dealt cards, not the keep | Proposal 3 (keep model) |
| Clairvoyant | Opponent's actual four cards | Proposals 1, 2, 9 (expect 10 to 25% of the gap to be reachable) |
| Heuristic replay (optional, deferred) | Filters worlds by replaying the heuristic's deterministic policy | Proposal 7 |
| Rollout discard | 10,000 common-random-number playouts per option on about 2,000 stored decisions | Proposals 4, 5 |

- **Kill rules.** If the clairvoyant pegger gains under 0.5 pegging points per
  hand, shrink proposals 1, 2 and 9. If rollout discard regret is under 0.02
  points per hand, drop proposal 5 and keep only the cheap parts of 4.

**E3. Speed**

- **Dense keep model.** Replace the maps in `keep.go` with a perfect hash over
  the 18,395 six-rank multisets and a `[1820]` cache. About 1.4x overall.
- **Per-hand caching** of the crib distribution and table-driven show scoring.
  About 1.2x more.
- **Double-dummy pegging database** keyed at count-reset boundaries (about
  10 MB, built in seconds). Estimated 3 to 10x on the search share. It also
  supplies the pair table for proposal 4 and leaf values for proposal 9.
- **Check:** identical bot decisions over 10k seeded games before and after.

**E4. A persistent self-play corpus**

- Log both six-card deals, starter, scores, dealer, discards, the full play
  sequence, shows, crib and result (about 100 bytes a hand).
- Add an exploring seat (occasional non-best discards) so rare table cells get
  data, and hand-level simulation from stratified score states so the endgame
  is covered.
- Proposals 2, 3, 5, 6 and 10 all train from this corpus.

## The ten proposals

### 1. Information-set pegging search

- **Problem.** The search assumes it will know the opponent's hand on later
  plays, and that the opponent knows its hand now. It therefore overvalues
  flexible holdings and never exploits the opponent's ignorance.
- **Mechanism.**
  - A node is a public play history plus a weight vector over the worlds still
    consistent with it.
  - At my nodes, pick the one card that maximizes the weighted sum over all
    worlds in the node, so my choice cannot depend on the world.
  - At opponent nodes, each world's opponent plays by a policy that sees only
    its own cards and the public history. The policy probabilities split the
    world weights among the children.
  - Start with a cheap no-peek policy: immediate points minus my expected best
    reply under a rank-marginal belief. Then swap in proposal 2's policy.
  - Blend for safety: value = λ·minimax + (1−λ)·policy expectation, λ tuned.
  - Terminals stay as now (shows, crib, W).
- **Expected gain.** +0.2 to +1.0. Researchers disagreed: the search and
  literature reviews said 0.4 to 1.0; the game-theory review argued pegging
  reveals cards so fast that the loss from the current method is probably
  under 0.5.
- **Cost.** No alpha-beta across summed vectors, so about 20 to 100 ms per
  decision unoptimized. E3 brings it back down.
- **Risks.** PLAN M11 found a naive greedy opponent model lost to minimax
  (52.7% v 53.9%). A poor policy repeats that; the λ blend is the guard.
- **Test.** Pegging points per hand, paired against the current expert, at
  λ ∈ {0, 0.25, 0.5, 0.75, 1}. Ablate the two halves separately: my-side fix
  only, and opponent-side fix only.

### 2. A learned opponent pegging policy, used for inference and prediction

- **Problem.** The likelihood of an observed play uses immediate points only.
  Leads, safe plays and held-back cards tell the bot nothing.
- **Mechanism.**
  - From the corpus, fit P(card | own hand ranks, public sequence, count,
    scores) by maximum likelihood.
  - Begin with a conditional logit on a few features: immediate points, reply
    risk, resulting count (5, 10, 21, 26), lead rank, pair held, cards left.
    Move to a small network only if held-out likelihood justifies it.
  - Use it in `pegLikelihood` to weight worlds, and as the opponent policy in
    proposal 1. One model for both keeps them consistent.
  - Fit it on expert self-play plus games against the other bots, as one
    general model. Per-opponent models belong to the deferred proposal 7.
- **Expected gain.** +0.15 to +0.4 alone; more as the input to proposal 1.
- **Cost.** Offline fit in minutes; run-time cost unchanged.
- **Risks.** Overfitting to the bots that generated the data. Always validate
  against a bot not used for fitting.
- **Test.** Held-out log-likelihood of the opponent's true hand against the
  current temperature-1 model, then paired games.

### 3. An exact-policy keep table

- **Problem.** Opponent inference was the largest single gain in the PLAN
  (M13), yet its model gives the true keep only 0.54 probability and ignores
  suits and score.
- **Mechanism.**
  - Run the real discarder on all 962,988 six-card hands (distinct up to suit
    relabelling), for both sides. That is about 2.5 minutes on 24 cores.
  - Tabulate P(kept rank shape | six ranks, side), weighting each hand by its
    number of suit variants. Flush and nobs effects come in for free, and the
    table is the same size as today's.
  - Mix with the current softmax: (1−ε)·exact + ε·softmax, ε about 0.05 to
    0.1, so the true world is never given zero weight.
  - Add two or three score bands (neutral, one player past about 85, both past
    about 100) because the real discarder changes near 121.
  - Rebuild whenever the bundle changes.
- **Expected gain.** +0.3 to +0.8 in expert-v-expert play; +0.1 to +0.4
  against the heuristic or the network, whose policies differ.
- **Cost.** Offline minutes; no run-time cost.
- **Risks.** Posteriors that are too sharp against opponents who do not
  discard like the expert. ε and proposal 7 address that.
- **Test.** Mean probability of the true keep on held-out hands of each bot
  type (target about 0.9 on expert self-play), then paired games at
  ε ∈ {0, 0.05, 0.2}.

### 4. An exact-conditional discard evaluator

- **Problem.** The discard value treats crib, opponent show and both pegging
  totals as independent tables that ignore what the opponent probably holds.
- **Mechanism.**
  - Once per decision, enumerate the opponent's six-rank multisets from the 46
    unseen cards and apply the keep model (or proposal 3's table) to get a
    weight for each (opponent keep, opponent throw) pair. This one pass is
    shared by all 15 options.
  - **Pegging:** a new table `PegPair[dealer][my shape][their shape]` holding
    the joint (my points, their points). Build it offline by playing each of
    the 3.3M shape pairs with the real pegger (about 11 minutes at effort 25
    on 24 cores), or analytically from the double-dummy database (E3) with a
    calibration fitted from the corpus.
  - **Crib:** exact for (my pair, their pair, starter), with my real suits for
    flush and nobs.
  - **Opponent show:** exact from their kept shape and the starter.
  - Keep the existing ordering and W mapping, and the full distribution rather
    than the mean so the endgame stays correct.
- **Expected gain.** +0.3 to +0.6. The pegging interaction is the main term.
  The measured crib error (above) suggests the crib and card-removal parts
  alone are worth only 0.1 to 0.3.
- **Cost.** About 5 to 10 ms per discard if the opponent pairs are collapsed
  to the top few hundred.
- **Risks.** The pair table is built at neutral score; noise in rarely kept
  shapes. PLAN M20 is a warning: a crib refinement changed 7 to 10% of
  discards and gained nothing.
- **Test.** Offline first, on 100k stored hands: how often the choice changes
  and by how much, against the rollout oracle from E2. Then paired games,
  adding one component at a time.

### 5. A rollout-distilled discard network

- **Problem.** Tables are at a fixed point within their model class. A larger
  function class needs clean training targets, which game outcomes are not.
- **Mechanism.**
  - For millions of deals, value all 15 keeps by playing the rest of the hand
    over shared samples of opponent hands and starters. The target is the
    change in W over the hand. Sharing samples across the 15 keeps cancels
    most of the variance.
  - Train a network on the six cards with suits, dealer and both scores. Two
    designs: output a value for each keep directly, or output distributions
    for crib, pegging and opponent show that plug into the existing
    convolution in `discard.go`. The second is safer because my own show stays
    exact.
  - Best used as a correction on top of proposal 4's value.
  - Train in PyTorch on the GTX 1080 Ti and export weights into the Go MLP
    format (`nn/file.go` is plain float32; a multi-output head is a small
    extension). Recent PyTorch builds may not support this GPU; a small
    network also trains acceptably on 24 CPU cores.
- **Expected gain.** +0.3 to +0.8 alone; perhaps +0.1 to +0.3 on top of
  proposal 4.
- **Cost.** A few hours of rollouts and training; 1 to 3 ms inference.
- **Risks.** Labels are only as good as the rollout pegging policy. Training
  on its own output can reinforce errors. It adds a Python step to an all-Go
  project.
- **Test.** Agreement with the 10,000-sample oracle on held-out deals compared
  with the tables, then paired games. Run only if E2 shows discard regret
  above about 0.04 points per hand.

### 6. Endgame exactness

- **Problem.** About 14% of hands start with a player at 100 or more, where
  win probability is two to four times steeper per point, and every
  approximation is at its worst there.
- **Mechanism.**
  - **Need-aware discard solver.** When either score is past about 95 to 100,
    value the top few discards by rollout: sample opponent hands, let them
    discard with the real discarder, enumerate starters, play pegging with W
    at the terminals, and count in order. This captures "I need three pegs"
    and "they need two, play safe", which the mid-game tables cannot. Cost is
    about 50 ms with a cheap pegging policy.
  - **Endgame-seeded win table.** Rebuild W for states in [85,120]² by
    simulating many hands from each exact state with the deployed policy,
    using the same deals at every state. Sweep from the highest band down.
    This replaces the sliding window where it is widest and least accurate
    (0.8 points error at 110-119).
  - **Small fixes.**
    - Make the crib at pegging terminals depend on the world, using the
      thrown-pair posterior `keepWeight` already computes.
    - Add expected flush and nobs to `oppShow` for hidden cards.
    - Score his heels before pegging in `discard.go`.
    - Resolve the both-peg-out case properly instead of 0.5.
- **Expected gain.** +0.3 to +0.5 for the whole package. The small fixes are
  about +0.05 to +0.15 and cost a few lines each.
- **Risks.** The policy and W depend on each other; iterate two or three times
  and check calibration on fresh endgame hands.
- **Test.** Paired games restricted to deals where a hand starts at 90 or
  more. This cuts the games needed about fivefold.

### 7. Opponent identification and exact best response (deferred)

**Status: future investigation, do not start.** The owner wants general
strength, and opponent-specific tuning only once enough data exists for the
opponents that matter (real players, of whom there are few today). The design
is kept here so it is not lost. It raises the win rate against known bots; it
is exploitation, not general strength.

- **Problem.** `heuristic` and `heuristic2` are deterministic. The expert
  models them with a soft generic model and then assumes they see its cards.
- **Mechanism.**
  - **Identify.** Keep a posterior over opponent types: heuristic, heuristic2,
    expert-like, generic. Every show reveals the opponent's kept hand and, by
    elimination, their two crib cards, so each hand gives one full discard
    observation. A deterministic type is confirmed or ruled out within a hand
    or two.
  - **Exact keep filter.** For a confirmed heuristic, a world has weight only
    if the heuristic would keep exactly those four from some consistent six.
    This needs suit-aware enumeration, because its discards use flushes.
  - **Exact play filter.** Keep only worlds in which replaying the bot's
    pegging rule reproduces every play seen.
  - **Predicted replies.** At opponent nodes, call the bot's real pegging rule
    instead of minimizing. Its pegging is a pure function of ranks, so this is
    exact and cheap (about 10 µs).
  - **Safety.** Mix (1−ε)·model + ε·minimax, cap the model's weight, and fall
    back to the generic model if every world is filtered out.
- **Expected gain.** +1.0 to +3.0 against `heuristic` (to roughly 58 to 60%,
  ceiling perhaps 63%); +1 to +2.5 against `heuristic2`; zero against others.
- **Cost.** Cheaper than today: fewer worlds, no alpha-beta at opponent nodes.
- **Risks.** It tunes the bot to the benchmark. A human who happens to match
  the heuristic for a few hands could be misidentified.
- **Test.** Paired 200k games against each bot with the keep filter, play
  filter and reply model switched on one at a time. Confirm no loss head to
  head against the current expert, and test a mid-game opponent swap.

### 8. Opponent-conditioned tables and win table (deferred)

**Status: future investigation, do not start**, for the same reason as
proposal 7. The per-account part also needs human game records, which sit on
the remote server and are few. Revisit when traffic has grown.

- **Problem.** Every table and W describe expert-v-expert play. Against a
  weaker player the crib throws, shows and correct risk attitude all differ.
- **Mechanism.**
  - Build a second bundle from expert-v-heuristic games and select it with
    proposal 7's posterior. The measured crib error was largest against the
    heuristic as dealer.
  - Make W asymmetric: record hands by player, not role, so W reflects that
    the expert is the favourite. A favourite should reduce variance when
    ahead; today it plays as if the game were even.
  - Adapt online for unknown opponents: count the rank pairs they actually
    throw to the crib and shrink the crib table toward those counts; estimate
    a per-hand points edge and shift W by it.
- **Expected gain.** +0.3 to +0.8 against the heuristic; unknown against
  humans; zero in self-play.
- **Cost.** One self-play run per opponent class; bundle size doubles.
- **Risks.** Same benchmark-fitting concern as proposal 7.
- **Test.** Paired games against the heuristic with the new bundle, with the
  other bots as a cross-check.

### 9. An equilibrium blueprint for the opening plays (last)

**Status: try last.** It has the least certain payoff, and it makes play
randomized, which the owner accepts in principle but which is hard to test
both in code and in measured outcome. The exploitability measurement in the
first step is deterministic and cheap, and can be run earlier as a diagnostic.

- **Problem.** The lead and first reply are the plays with the most hidden
  information, and the bot's play there is fully predictable.
- **Mechanism.**
  - **Measure first.** Compute a best response to the current expert on
    sampled deals (about 4 minutes for 1,000 deals at effort 25). The gap is
    the bot's exploitability and says whether to continue.
  - **Two-ply blueprint.** Tabulate leaf values for every (my shape, their
    shape, lead, reply) from the double-dummy database, then solve the small
    game with CFR+. Use the result for the lead and first reply when both
    scores are below about 90; otherwise use the search.
  - **Full blueprint.** External-sampling Monte Carlo CFR over all shape pairs
    with a points objective: about 70 minutes on 24 cores, 2 to 6 GB of
    memory. Use it for the first three plays, and as the opponent policy in
    proposal 1.
  - Equilibrium play is mixed, so the bot stops being deterministic. Draw
    from a generator seeded per game so any game can be replayed exactly, and
    keep the deterministic bot selectable.
  - The baseline-coupled test in E1 relies on determinism (identical play
    gives a difference of exactly zero). With a mixed strategy that no longer
    holds, so plan for more games, or compare the expected value of the mixed
    strategy instead of sampled play.
  - Only the two-ply blueprint fits the current server's memory. Build and
    measure it first. Build the full blueprint only on the development machine
    as an experiment; if it shows a clear gain over the two-ply form, report
    that gain alongside the memory it needs and the monthly cost of the
    smallest instance that would hold it, so the owner can decide. First try
    shrinking it (quantized probabilities, merged equivalent states, fewer
    plies).
- **Expected gain.** 0.0 to +0.6; the researcher gave 70% odds it is under
  0.5 against the current expert. It does not exploit weak opponents, so it
  may gain nothing against the heuristic. Its main value is robustness against
  strong humans, which no current test measures.
- **Risks.** A points objective ignores W near the endgame; memory; it may
  simply underperform the 400-world search.
- **Test.** Exploitability before and after; 100k paired games.

### 10. Policy iteration and table statistics, rebuilt

- **Problem.** The learning loop stalled after one round, but its acceptance
  test could not tell a fixed point from noise, and the tables come from a
  weaker, mid-game-only version of the bot.
- **Mechanism.**
  - Generate self-play at the deployed effort and across all score ranges
    (E4). Tables stay general: built from expert self-play, with the other
    bots used for evaluation only.
  - Pool tables across rounds instead of replacing them.
  - Replace "shrink toward the global mean" for rare pegging shapes with a
    feature-based prior (counts of low cards, fives, tens, pairs, runs) fitted
    by regression, and shrink toward that. Rare shapes currently win the
    argmax partly through noise.
  - Fix the crib shrinkage target: the comment at `tables.go:99-102` describes
    a "same discards, any starter" backoff that the code does not implement.
  - Tune hand-set constants (smoothing 20, window 5000, pegging temperature,
    pool factor 4) by held-out likelihood where possible, and by SPSA on
    paired games for the rest (about 4M games, 10 hours).
  - Accept a round by the sequential test in E1, sized to detect 0.2 points.
- **Expected gain.** +0.1 to +0.4. Its larger value is that every other
  proposal needs a loop that can detect a 0.2 point gain.
- **Cost.** Hours of self-play per round on 24 cores.
- **Test.** Split-half agreement of two independent builds, held-out
  likelihood, then paired games.

## Suggested order

1. E1 and E3, then the oracles in E2. Apply the kill rules.
2. E4, then proposal 3 and the small fixes in proposal 6. These are cheap and
   low risk.
3. Proposals 1 and 2 together, if the clairvoyant oracle justifies them.
4. Proposal 4, then proposal 6's endgame solver and win table.
5. Proposal 10 as the loop that retrains everything after each change.
6. Proposal 5, if the rollout oracle shows enough discard regret.
7. Proposal 9, last.

Not scheduled: proposals 7 and 8, until there is enough data on real
opponents.

## What was considered and rejected

- **A bigger network trained on game outcome** (TD, DouZero-style, NFSP, Deep
  CFR, MuZero). The target is too noisy; it failed here and in every
  published cribbage attempt found.
- **More search worlds or more rounds of the current iteration.** Measured as
  saturated.
- **A value network at pegging leaves.** The search already reaches the end of
  the hand, so there is no horizon to replace.
- **Luck-adjusted estimators (AIVAT-style) for evaluation.** Seat-swapped
  duplicate deals already cancel deal luck; expected benefit 1.1 to 1.4x.
- **Human positional theory (Colvert's par holes, playing on and off).**
  Maximizing expected W already subsumes it. The gaps are in the accuracy of
  the distributions near the endgame, which proposal 6 covers.
- **Mixed-strategy discards.** Discards are near-deterministic best responses;
  an equilibrium mix would buy almost nothing.

## Decisions from the project owner (2026-11-23)

1. **The goal is general strength.** The acceptance metric is head to head
   against the current expert, with the heuristic, `heuristic2` and
   `models-big` as guards. Tuning against a specific opponent is worthwhile
   only once enough data exists for that opponent, so proposals 7 and 8 are
   recorded as future investigation and are not to be started now.
2. **Deployment target.** The server is an AWS t4g.micro (2 burstable ARM
   vCPUs, 1 GB of memory) running only the Go server. See "Deployment
   constraints" above. Background work on it is allowed if it runs at lower
   priority than the server and does not slow interactions.
3. **Compute for training.** Use the 24-core development machine; it is
   currently busy with other training, so check the load before timing
   anything. A large AWS compute instance can be provided for a job that needs
   a lot of compute for a defined period: state the instance size and hours
   needed when asking.
4. **A Python/PyTorch training step is acceptable.** Inference stays in Go.
5. **Determinism is not required, but randomized play is hard to test,** both
   in code and in measured outcome. Proposal 9 is therefore the last to try,
   and it must come with a seeded generator and a test plan that accounts for
   the extra variance.
6. **A larger server instance is possible** to hold the full blueprint in
   proposal 9, but the feature's measured performance has to justify the
   higher running cost. The default remains the t4g.micro.
7. **Stored human games may be used,** but they live on the remote server and
   there is very little traffic. Per-account adaptation is low priority and
   part of the deferred proposal 8.
