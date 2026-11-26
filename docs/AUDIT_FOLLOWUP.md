# Audit follow-up (2026-11-25)

A whole-repo audit was run on `main` at `b86e088`. This file records what it
found, what was fixed on the `audit-fixes` branch, and what is left: the
findings not fixed, and the parts of the repo the audit did not check,
each with a priority out of 10 (10 = do first).

Load assumed: one small public server (the 1 GB "family and friends"
machine in deploy/README.md), tens of players at once, open to strangers.

## What the audit found to be sound

Read in full, with no finding: sign-in by code, link, passkey, device
approval and bot key (`internal/auth`, `internal/server/account.go`,
`device.go`); what a player is sent about the other's cards
(`engine/observation.go`, `protocol/convert.go`, events); the shuffle seed;
the SQLite store, its group commit, action-log sweep and backups; email
encryption and stop links; rate limits; the WebRTC signaling and TCP
framing limits; the admin socket; the deploy units and scripts. The web
client's `innerHTML` sinks all escape server strings, and its CSP has no
inline script.

## Fixed on `audit-fixes`

Commit "Clients: fix emailed links…":

| # | Was | Fix |
|---|---|---|
| 1 | Web: `#signin=` and `#delete=` links from email never reached the server; the click sat in the request queue (since `ed19136`). | `onOpen` readies the connection before showing either panel. |
| 2 | Both clients: a game that ended while the client was offline stayed "in progress". | The game list handler takes the result from `recent`. |
| 3 | Web: Sign out while disconnected threw, so no reload and no server-side sign-out. | Null check on `conn.dc`. |
| 4 | Godot: the device approval URL from the server went to `OS.ShellOpen` unchecked. | Opened only if http or https (`IsWebUrl`). |
| 5 | Web: `UPGRADE_REQUIRED` was shown as a form error; the page never reloaded. | `onMessage` routes it to `onError` first. |
| 6 | Web: the last player's games showed for the next sign-in on the same tab. Godot: same after a stale session. | Games are cleared when a different user signs in (web) or the stale token is dropped (Godot). |
| 17 | Web: `shortDate` defined twice; the history list always printed the year. | The account dialog's one is `fullDate`. |

Commit "Server: end-of-game rule tests…":

| # | Was | Fix |
|---|---|---|
| 8 | Nothing ran the Go tests automatically. | `.github/workflows/server-tests.yml`: `go vet` and `go test` on push. |
| 9 | `expert build -base` dropped the exact keep, pegging policy and shape-pair tables. | They carry over; a note is printed when discard tables are rebuilt under them. |
| 12 | No test for a game won on heels, a go point, the last card, the dealer's hand or the crib. | Five tests in `engine/game_test.go`. The heels test was checked to fail with the rule broken. |
| 14 | `ReadBundle` allocated from the file's shape before checking it, and accepted a table listed twice. | Checked first; duplicates refused. |
| 16 | Account deletion removed the account, then renamed it in old games; a failure in between could not be retried. | Order swapped. |
| 17 | `cmd/expert`: `discardPolicy` repeated `chooseDiscard`. | It calls it. |

Checked after the fixes: `go vet ./...` clean, `go test ./...` passes,
`go test -race -short` passes on the engine, bots, ai, expert, players and
protocol packages, `dotnet build` of the Godot client has no warnings,
`node --check app.js` passes. **Not checked: the web and Godot changes
were not run in a browser or in Godot.** See the first row below.

## Found, not fixed

| Pri | # | Finding | Why it was left |
|---|---|---|---|
| 9 | 7 | The web client has no tests. Finding 1 shipped unnoticed. | A real test needs a headless browser, which is a new dependency and a choice of tool. Smallest useful one: open `#signin=<token>` against a local server and check it reaches the lobby. Until then, the client fixes above are verified by reading only. |
| 6 | 10 | The dealer's hand plus crib is capped at 29 in the discard valuation (`expert/discard.go:29,185`, `conddiscard.go:257`): the array is `[maxShow]`, the pone's is `[2*maxShow]`. | It changes how the bot plays, so it needs a gauntlet run and a PLAN milestone, on its own branch, under the bot-work rules. |
| 5 | 13 | One address asking for codes to made-up addresses every 5 minutes keeps the global new-account email limit (20, then one per 15 minutes) empty: newcomers are told "sent" and get nothing, and the bounces hurt the mail account (`auth/signin.go:212`). | A commented trade-off. A per-address limit low enough to stop it (under 4 an hour) also makes the fourth member of a family signing up from one house wait, silently. Owner's call. The Warn line "too many new accounts from everyone lately" is the signal to watch. |
| 4 | 11 | No client sends the `Register` request, but the server still serves it (`server/account.go:106`, `auth/signin.go:326`, the `registers` limiter, the `reg` ticket branch): about 110 lines. | About ten tests in `auth_test.go`, `account_test.go` and `limit_test.go` create accounts and test the email limits through it, and its errors (`ErrEmailStopped`, `ErrTooManyAccounts`) are ones the normal sign-in path hides on purpose. Removing it means rewriting those tests, and reserving field 10 in the proto. |
| 2 | 15 | Each awake game against the "expert" level holds a 284 KB scratch table (`expert/conddiscard.go:28`). | Only matters near 1,000 awake games. Fix when needed: a `sync.Pool`, as `search.go` does for `endCache`. |

Smaller findings, not fixed (from the helper audits; **not re-read by the
main audit unless marked**):

| Pri | Finding |
|---|---|
| 5 | Godot: the table shows the final result before the replay reaches it (`Main.cs` `RenderTable` uses `gs.Game`, not the view's). |
| 5 | Godot: with a bad saved server address, the Server field cannot be reached (`ShowLogin(false)` in `_Ready`). |
| 4 | Godot: requests waiting for a reply never fail when the connection drops ("Sending…" stays). |
| 4 | Web: History clicked twice lists every game twice; after a reconnect with History open, "more" does nothing. |
| 3 | Godot: the session token is not tied to the server it came from; `--server` to another server erases it. |
| 3 | Gauntlet `-base` accepts a baseline saved with a different `-start` (`cmd/gauntlet/main.go:156`, `ai/pairs.go:32`). |
| 3 | `nn.Read` trusts layer sizes from the model file (`ai/nn/file.go:77`); same fix as `ReadBundle`. |
| 2 | Corpus and rollout readers index with values from the file and panic on a corrupt one (`expert/corpus.go:110`, `cmd/expert/rollcheck.go:108`). Developer tools only. |
| 2 | Server (re-read): a request in flight at shutdown can write to the store after it closes and panic. Nothing is lost; the exit is just ugly. |
| 2 | Server (re-read): `AddPasskey` in SQLite reports success for an account deleted meanwhile; an invite can be accepted up to an hour after it expires (the sweep is hourly). |
| 2 | Lean: `expert.Collect` repeats the corpus path (about 135 lines); shape indexing exists in both `bots/heuristic2.go` and `expert/tables.go`; `searcher.sample`, `CorpusFilter.NoSynthetic`, `Text.Value`, three unused CSS rules and two unused `RETRYABLE` names are dead. |
| 1 | Lean, owner's call: the in-memory store (570 lines, for `-db ""` and tests), `cmd/sim` (136), `expert iterate` (92), `ai/clock_windows.go` (now that development is on Linux). All are documented features. |

## Not checked

| Pri | What | Why it matters |
|---|---|---|
| 10 | **Run the fixed clients for real.** Nothing was run in a browser or in Godot, before or after the fixes. Open a sign-in link and a delete link from an email; sleep a laptop through the end of a game; sign out while offline; sign in as a second user in the same tab; do "Sign in with browser" in the desktop app. | Findings 1 to 6 and their fixes rest on reading the code. The web page is the front door. |
| 7 | Is the deployed Cloudflare Pages build from `ed19136` or later? If so, emailed links are broken in production now. | Decides how soon the web fix must ship. |
| 6 | The four Godot and two web findings above that the main audit did not re-read. | They may be wrong, or worse than described. |
| 5 | `server/web/static/index.html` lines 126-310, `style.css`, `privacy.html`. | Ids and classes were only cross-checked by script. |
| 5 | `deploy/README.md` (639 lines) was searched, not read. The units and scripts beside it were read. | Wrong instructions there cause outages and lost backups, not code bugs. In particular, nothing alerts when a backup or the S3 copy fails. |
| 4 | Godot project files: `Cribbage.csproj`, `export_presets.cfg`, `project.godot`, `Main.tscn`, the fetched `addons/`. | They decide what ships in the desktop zips. |
| 4 | A restore from a backup was not tried, and nor was starting with a wrong or lost email key. | The backup is only as good as the last restore that worked. |
| 3 | `cmd/expert`: `keep.go`, the second half of `exploit.go`, most of `rollout.go` and `rollcheck.go`. `internal/expert`: `exactkeep.go`, the build halves of `pegpair.go`, `winprob.go` and `tables.go`, `checks.go`, `FitKeepTemps`. | A bug there makes a weaker bundle, as finding 9 did, and nothing reports it. |
| 3 | The dead-code search was a word grep, so a dead method sharing a name with a live one can hide. | Lean only. |
| 2 | `internal/ai/train/train.go` after line 140 and `cmd/train`. | Training is finished; the bots in use are built. |
| 2 | Most test bodies outside `internal/engine` were not read for what they assert. | They pass; whether they test the right thing was sampled only. |
| 1 | Generated code (`protocol/pb`, `bots/h2stats.go`), model files, `runs/`, `docs/PLAN.md` beyond searches. | Generated or historical. |
