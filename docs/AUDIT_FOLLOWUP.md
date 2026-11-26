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
were not run in a browser or in Godot.** The second pass, below, ran most of them.

## Second pass (2026-11-25)

On branch `audit-followup` (web tests, Register) and `crib-cap` (#10):

| # | Was | Now |
|---|---|---|
| 7 | The web client had no tests. | `server/web/test`: Playwright in headless Chromium, against a real `cribserver` per test (own port, empty database, emails read from its log). Six tests (a seventh, History opened twice, came in the third pass): a sign-in link that creates an account, one that signs in an existing account, a delete link, Sign out with the server gone, the next user on the same tab after a session ended from another device, and a game that ended while the page was offline. Each fails on the client from before the fixes (`b86e088`) at the step its finding names, and passes now. Runs in CI as the `web` job of `server-tests.yml`. Run: `cd server/web/test && npm ci && npx playwright install chromium && npx playwright test`. |
| 10 | The dealer's hand plus crib was capped at 29 in the discard valuation. | The array is per-decision scratch, not stored in the bundle, so it doubled without a rebuild. New test `TestDealerShowPlusCribPasses29`, checked to fail with the cap. Measured as PLAN M46 (on `crib-cap`), paired by deal against the old code: +0.00 ± 0.01 win % from scores 70-95 (14 of 20,000 deals changed), +0.01 ± 0.01 over full games (26 of 10,000); the Hard level, which takes the other edited path, -0.01 ± 0.01 (45 of 10,000). Neutral; kept as a correctness fix. Needs no new bundle. |
| 11 | The server still served `Register`, which no client sends. | Removed: the request, `auth.Register`, the `registers` limiter and `AuthLimits.RegistersPerIP`, the ticket's `reg` branch and `ErrTooManyAccounts` (only Register returned it). Field 10 is reserved in the proto. The tests that made accounts through it now use the new-account code from `RequestSignIn`; the checks that only Register could reach moved to the mailbox (`TestStopEmail` checks no mail goes to a stopped address's +tag variant; `TestMailboxLimit` uses accounts made directly, so the new-account cap plays no part). `TestRegistrationLimit` was dropped: `TestNewAccountCodeLimit` tests the same cap through sign-in. The Godot client builds against the new proto. |

Answers to two "Not checked" rows:
- **Pri 7, the deployed page:** `origin/pages` contains `ed19136`, and the three link tests fail on its copy of `static/`, each just after the button on the link's panel is clicked. **Emailed sign-in and delete links do not work in production today.** The fix is on `main`; shipping it is a `pages` push (after the server, per deploy/README.md). This is inferred from the branch's client under test, not seen on the live site. The `pages` client never sends `Register`: run against a server without it, it still created an account by code in the tests above, so the server can ship first.
- **Pri 10, running the clients:** the web findings 1, 2, 3 and 6 are now covered by the tests above. Finding 5 (`UPGRADE_REQUIRED`) was not run. Godot: autoplay played a full game against a local server over WebRTC, headless, and resumed a game after being killed mid-hand. Not run in Godot: findings 2, 4 (the device link opens the system browser) and 6, which need the UI driven by hand.

## Third pass (2026-11-25)

Also on `audit-followup`. Fixes said to have a test have one; only the
SMTP address test, the History web test and the expert pool check were
checked to fail (or differ) without the fix. The rest are marked "not
run", "no test", or were checked by hand as described.

| Finding | Fix |
|---|---|
| #15: a 284 KB discard table per awake expert game | Borrowed from a `sync.Pool` per decision. Exact: 0 of 1,000 deal pairs changed against the same code without it. |
| Godot: the table shows the result before the replay gets there | `RenderTable` reads the view's game; the "ended while away" path updates the view too. Not run: needs the UI. |
| Godot: a bad saved server hid the Server field | The sign-in page shows when the address is refused, or after 5 seconds without a connection. Checked by screenshot against an unreachable server. |
| Godot: "Sending…" stayed after the connection dropped | Waiting requests are answered with "The connection dropped; try again", except during an account deletion (whose drop is expected). Not run. |
| Godot: the session token was not tied to its server | One token per server address in the settings file. Checked with autoplay: server B never gets A's token, and A still resumes after a run against B. |
| Web: History twice listed games twice; "more" died after a reconnect | Replies to all but the latest first-page fetch are dropped; a reconnect with History open fetches afresh. Web test for opening it twice (fails on the old client); the reconnect case is not tested. |
| Gauntlet `-base` with another `-start` | The pair file records `-start`; a mismatch is refused. No test. |
| `nn.Read` trusted layer sizes | Sizes and total weights checked before allocating. Test added (it cannot fail by allocating, so it checks the refusal only). |
| Corpus and rollout readers panicked on a corrupt file | Records and lines are range-checked. The real corpora (1.37M and 42k hands) still load. Corpus test added; the rollout readers' checks have none. |
| Shutdown: a request in flight could write after the store closed | `Shutdown` waits for connections' read loops; HTTP requests in flight get 5 seconds (`http.Server.Shutdown`). Not tested by a race test; `-race` passes. |
| `AddPasskey` succeeded for a deleted account | Returns `ErrNotFound` when no row was added. |
| Invites accepted up to an hour after expiry | Accept and redeem check the expiry; the sweep still cleans up. |
| Dead code | `searcher.sample`, `CorpusFilter.NoSynthetic`, `Text.Value`, three CSS rules, two `RETRYABLE` names, and two methods `deadcode -test` found: `Adam.Steps`, `Limiter.Return` (its only callers were the password checks M31 removed; the emailed-code limit charges under the same lock it checks, so needs no refund). |

From the three read-throughs of what was not checked:

- **deploy/README.md** (read in full against the units, scripts and flags;
  almost all of it was right):
  - Fixed: a lost email key is fatal, not just an inconvenience. The server
    will not start on that database or its backups. A restore was tried
    locally: a backup taken after games keeps them, it starts with the
    same key, and it refuses a wrong or missing key.
  - Fixed: the restore steps deleted the broken database's WAL; now kept.
  - Fixed: restoring onto a new instance with the S3 job on would upload an
    empty backup as the newest; the steps now say to wait.
  - Fixed: `-yes` goes before the command name.
  - Fixed: the install block started the service before its secrets
    existed.
  - Fixed: `-ice-servers` belongs in the config file.
  - **Backups:** `cribbage-s3-backup` now fails when the newest backup is
    over 26 hours old (the server's own failure only logs). It can ping an
    optional dead-man's switch (`CRIBBAGE_PING_URL`), the only thing that
    would alert a person. Checked with a stub `aws`.
  - Left: see "Found, not fixed".
- **Web markup and Godot project:**
  - Fixed: game rows can be opened from the keyboard (web test).
  - Fixed: messages, prompt and banner announce themselves (`role=status`).
  - Fixed: picked cards say so (`aria-pressed`), and face-up cards read as
    images.
  - Fixed: Esc updates the settings button's state.
  - Fixed: privacy page: games after deletion, and backups up to about 10 days (S3 rounds expiry to midnight UTC and deletes later).
  - Fixed: SMTP errors no longer carry the recipient's address into the log
    (a test fails without it).
  - Desktop: .NET 8 lost support in November 2026, so the client targets
    .NET 10. The Linux and Windows exports bundle .NET 10.0.14 and each
    played a full autoplay game (Windows under wine).
  - Desktop: a release tag fails without `CRIBBAGE_SERVER`.
  - Desktop: the cached Godot editor is unpacked afresh from the checked
    zip, and `export.sh` always fetches the checked webrtc addon.
- **Expert build code** (no silent table bug like #9; the bundle builders
  agree with their readers):
  - Fixed: `rollpair` halved its "per decision overall" figure when
    disagreements passed `-max`. M40's published run was just under the
    cap. No test, nor for the three help-text fixes below.
  - Fixed: `rollcheck` now says its regret is biased high.
  - Fixed: `rollout`'s help says its "expert" is the default discarder,
    not the very hard level's.
  - Fixed: `expert build` says when keep temperatures come from `-base`,
    and that keep fitting uses at most 50,000 hands a side.

## Found, not fixed

| Pri | # | Finding | Why it was left |
|---|---|---|---|
| 5 | 13 | One address asking for codes to made-up addresses every 5 minutes keeps the global new-account email limit (20, then one per 15 minutes) empty: newcomers are told "sent" and get nothing, and the bounces hurt the mail account (`auth/signin.go:212`). | A commented trade-off. A per-address limit low enough to stop it (under 4 an hour) also makes the fourth member of a family signing up from one house wait, silently. Owner's call. The Warn line "too many new accounts from everyone lately" is the signal to watch. |
| 5 | | S3 versioning keeps an overwritten backup for only a day (`NoncurrentDays: 1`), and the server's role may overwrite. An intruder can replace every S3 backup, and the originals expire a day later. | A longer `NoncurrentDays`, or Object Lock, keeps deleted accounts in S3 longer, which changes the privacy page's promise. Owner's call. The README now says so. |
| 2 | | Desktop: the workflow's `contents: write` applies to test runs too, and actions are pinned by tag, not SHA. The Windows exe's properties say Godot, not Cribbage. Release builds keep the `--autoplay` test mode (harmless: it needs the server's bot key). | Hardening and polish. |
| 2 | | Expert tools: `pegpair` plays hands nobody chose (random padding), so its table may not match real games (it is not shipped). `rollout -seed` does not reproduce with several workers. The random-crib prior never counts nobs for a thrown jack (about 0.25 points, in a prior worth 20 hands). | Changing them changes future measurements, not the shipped bot; worth doing with the next table build. |
| 2 | | Lean: `expert.Collect` repeats the corpus path (about 135 lines); shape indexing exists in both `bots/heuristic2.go` and `expert/tables.go`. | Merges, not deletions; each needs its own check that the bots play the same. |
| 1 | | Lean, owner's call: the in-memory store (570 lines, for `-db ""` and tests), `cmd/sim` (136), `expert iterate` (92), `ai/clock_windows.go`. | Documented features. |

## Not checked

| Pri | What | Why it matters |
|---|---|---|
| 8 | A real emailed link from a phone or desktop mail app (the tests take links from the log); `UPGRADE_REQUIRED` in the web client; the Windows export on real Windows (only wine was tried); in Godot, by hand: a game ended while away, "Sign in with browser", "Sending…" after a drop, the replay fix. | The web tests use Chromium and a local server only. |
| 3 | `deploy/cribbage-dns` beyond a search, the ACME/TLS code, `exploit.go:1-279`, `wintable.go`, `pegfit.go`, `iterate.go`. | Read by no one. |
| 2 | `internal/ai/train/train.go` after line 140 and `cmd/train`. | Training is finished; the bots in use are built. |
| 2 | Most test bodies outside `internal/engine` were not read for what they assert. | They pass; whether they test the right thing was sampled only. |
| 1 | Generated code (`protocol/pb`, `bots/h2stats.go`), model files, `runs/`, `docs/PLAN.md` beyond searches. | Generated or historical. |
