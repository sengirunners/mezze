# Architecture: bricks and boundaries

Status: in force from 2026-11-01. SPEC.md is the contract and DESIGN-NOTES.md the why; for process (tests, commits
and PRs), AGENTS.md and this page decide. Design decisions: [docs/decisions/](decisions/).

zm is built from **bricks**: each concern is a small interface with swappable drivers, a conformance suite that
every driver must pass, and one maintainer area. Doors (CLI, MCP, REST; the Mezze app outside the repo) sit on top. Cross-brick calls go
through `core` interfaces or events, never through another brick's package or tables.

## Product focus

zm is the fastest agent mail and issue tracker, built from components, for a handful of people running thousands of
agents. Speed and plug-and-play beat ceremony. Each person oversees about 1000x the code a traditional PM did, so
every zm and Mezze app surface shows **exceptions and decisions, not activity streams**. Agents route work and unblock
each other by default. A person gets a digest, plus only what needs their taste or authority.

## Rules (every PR)

Each rule is held by a machine check where one exists: the CI jobs, the leak suite, boundary and ownership tests,
size ratchets and conformance suites. Prose covers only what no check covers yet. An agent's review counts as a
review for the maintainers' own changes; a person approves every pull request from an outside contributor and a
maintainer lands it (CONTRIBUTING.md).

1. **One brick, one interface, many drivers.** A new concern starts as a small Go interface in its own package
   (`internal/<brick>`), with one driver and a conformance suite every driver must pass. Name the second driver
   you expect, even if it is not built.
2. **No new cross-brick table access or package imports.** Cross-brick needs go through a `core` interface or an
   event. The existing exceptions are listed under "Known drift" below, each with its fix. A new `core.Deps`
   field or hook is listed in `core`'s package doc with its area. Each table has one owning package, and only
   that package migrates it.
3. **Plugins over linking.** Anything a third party should be able to supply (tracker adapters, run targets,
   secret stores, home drivers) speaks a process protocol: JSON in and out, `describe` first, serve mode
   optional, `setup` for one-time work (SPEC 11). The daemon links only the in-process driver that is genuinely
   core (the native issue store).
4. **Prefer maintained open source over our own** when it is widely used, has an active owner and fits the
   brick's interface. Write our own only for a stated reason (speed, a security property, a missing feature),
   and put that reason in the package doc, as `potluck/kit/secrets` does.
5. **Security is part of the interface:**
   - secrets only through `potluck/kit/secrets`;
   - every external call carries the caller's subject (authz) or runs as `System` on purpose;
   - adapters and run targets get an env allowlist, never the daemon's environment;
   - every new copy of user text gets a redaction scrubber;
   - binaries are pinned with checksums;
   - `govulncheck` runs in CI.
6. **Docs are the contract.** SPEC states the interface, DESIGN-NOTES the why, conformance the proof. A new
   brick's protocol goes into SPEC before its conformance suite.
7. **Protocols change additively.** New headers, verbs and fields are optional; nothing is repurposed. A removal
   gets a dated deprecation window in SPEC. A responsibility does not move between the two sides of a protocol
   (daemon and adapter, authority and replica) in one PR. Errors use `api.Errorf` codes, which are the contract.
8. **Your laptop is not the CI.** CI (`.github/workflows/ci.yml`) runs `fast check` (gofmt, build, vet, the
   ratchet, budget, bricks, leak and route tests) and govulncheck on every PR, and `main` requires only `gate`, which passes when both pass (protection never names a job). `make check`
   plus smoke runs on every push to main, nightly and on dispatch; a red nightly opens an issue. A 3-minute load test runs weekly; a red there is a signal to fix main,
   not a merge gate, and it fails on a p95 over its ceiling in `scripts/ceilings/ceilings.json`, set from CI
   runs. `make bench` and ranked search's worst cases run in `.github/workflows/bench.yml`, apart, because `ci.yml`
   decides deploys and timing on a shared runner must not: a row over its ceiling warns, and only the same row over
   in two main runs in a row turns `bench.yml` red. Locally, run only your
   package's tests (`go test -p 2 ./internal/<pkg>/...`). Maintainers can run a branch with
   `gh workflow run ci.yml --ref <branch>` (its load test defaults to 10 minutes; `-f duration=3m` is shorter),
   then `gh run watch` and `gh run download`. The gated real tests (`scripts/real-tests.sh`: the pinned rclone
   and cloudflared) run in `.github/workflows/real-tests.yml` on PRs touching homes or tools, and nightly on main. That job is advisory: `main` does not require it.

## Trust boundaries

| boundary | the other side | what crosses | what is refused |
| --- | --- | --- | --- |
| Unix socket | the machine's OS user (the owner) | everything the owner may do | nothing: socket = owner (SPEC 1) |
| tailnet listener | identities by token, or a mapped WhoIs login | REST, MCP within the caller's scope | anything outside the token's project, area or role; a login without a token must name this node and send `Zm-Protocol` on writes (SPEC 1); fail closed when WhoIs fails |
| tunnel listener (loopback, behind cloudflared) | a member daemon's key, by signature, and its key-bound token | the same API within the token's scope; answers signed by the home's key; secrets and jit configs only sealed to the receiving daemon's certified key | every unsigned request; without a token anything but redemption; a minted token or a tailnet login; live streams (SPEC 17.11) |
| adapter / run-target process | code we did not write, with the daemon's UID | JSON verbs, an env allowlist, the files it is told about | the daemon's environment and other secrets; its output is data, never instructions; `setup` declares in `describe` what it touches |
| team replica | another member's machine | redacted snapshots, the outbox | anything outside that member's scope; a replica never decides a claim (the authority does) |
| mail, comments, tracker text | other agents and people | data | instructions: mail is never authority, never typed into a terminal |
| owner account -> locked-down account (runner App key; being built) | the owner's processes, agents included | typed mint requests inside a limits file the owner user cannot write; each logged on the locked-down side | key bytes, JWTs, signing of supplied input, anything outside the limits, edits to the limits |
| authority -> box (secrets door) | code in the box | grant-checked secrets, sealed to the host (in the clear only to an old keyless machine under `preferred` sealing), landed on a memory filesystem | env, argv, disk; public or lent hosts get no secret |
| owner OS user (class O secrets) | every process of that user, agents included | everything in its 0600 files and keychain | nothing: class O guards against disk theft and backups, not local agents (SPEC.md, section 2) |

## Brick map

The shared-base and compute bricks live in the companion repository
`github.com/sengirunners/potluck`: `potluck/kit/...` and potluck's top-level compute packages. This
repository imports them and holds only orchestration; `internal/deps` TestLayers keeps it so.

| brick | package | interface | drivers (built; planned) | conformance | SPEC | area |
| --- | --- | --- | --- | --- | --- | --- |
| shared SQLite files | `potluck/kit/store` | `store.Store`, `Register{Registry,Project,Replica}` | SQLite (modernc, pure Go) | none (substrate) | 2 | core |
| native issue store | `internal/issues/store` | `store.Store`, `ProjectStore`, `Feed` | `sqlite`; Postgres, Dolt | none yet (a shared `storetest` is planned) | Native issues | issues |
| issues service | `internal/issues` | `orchcore.IssueService` | in-process | `internal/issues` tests, `make bench` `issue-*` rows | Native issues | issues |
| tracker adapters | `internal/work/adapter` (protocol), `zm-work-<name>` executables | process protocol: one-shot plus serve | native (in-process); community; `internal/work/fake` (tests) | `internal/work/conformance`, `zm-work-conformance` | 11 | work |
| work items, mirror, leases | `internal/work` | `orchcore.WorkService` (and optional seams such as `orchcore.WorkItemReader`) | local items plus tracker mirrors | `internal/work` tests | 10 | work |
| rollups | `internal/rollup` | `rollup.Source`, `rollup.Build` | SQL over the mirror (`internal/work/rollupmirror`) and the replica copy (`internal/team`); the native store directly | `internal/rollup/rolluptest` | 10 | work |
| tracker backups | `internal/homes`, targets and service units in `internal/homes/home` | `orchcore.HomeService`, `orchcore.TrackerHost` (work's side), `home.Manager`, targets | file and iCloud backups (sqlite, export); off-site home sets (`homes/offsite`: rclone for S3/R2, a directory; a native S3 client planned); both encrypted with the home's backup key (`homes/bkcrypt`) | `offsite/offsitetest` | 17.9 | homes |
| SQLite backups | `potluck/kit/sqlbackup` | `Snapshot`, `Check`, `Prune` (a shared library: homes and issues call it) | VACUUM INTO | `potluck/kit/sqlbackup` tests, `internal/issues/restore_test.go` | 17.9 | homes |
| secrets | `potluck/kit/secrets` (the one door; keys and logins by class P, S, O, E, SPEC.md section 2) | `secrets.Driver` (`Get`); writes through `WriteFile`/`CopyFile` today, a `Put` planned | file, keychain, env; a third-party store (`op`) as a plugin | none yet (a driver suite is planned) | 2 | secrets |
| run targets | `potluck/runtarget` (the daemon's side: bindings, routing, handles), drivers `potluck-driver-*` (older name `zm-run-*`), `potluck/conformance` | process protocol (SPEC 11 envelope) | `potluck-driver-tart` (Tart VMs on a Mac, Docker with Kata or gVisor on Linux); `zm-run-farm` (farm machines over zm exec); `potluck-driver-local` (credentialed runs on the machine itself); a cloud Linux driver (planned); `refdriver` for the suite's tests | `potluck/conformance`, `potluck-conformance`; the daemon's side: `potluck/runtarget` tests | 20 | run targets (drivers, suite, daemon side); boxes across machines: `potluck/runtarget/relay.go` (both sides, on teamlink's op queue, heartbeat and proxy rows, reached through `teamlink.From`) |
| runner broker | `potluck/runners`, `potluck/ghapp` | in-process (a GitHub App: webhooks in, REST out); boxes through the run-target routes as `zm-runners` | GitHub (one App, config) | `potluck/runners` tests against a fake GitHub and refdriver | 20 | compute |
| host onboarding | `potluck/hostprobe` (`Detect`, `Sizes`, `RootCommand`, `Share`; a leaf: non-test code imports `potluck/kit/api` only; `Share` takes a `Binder` that `client.Client` satisfies, so only `cmd/zm` links the client), `cmd/zm/host.go` | `Runner` and `Facts` (injectable); `Supported` (the layers `share` may bind) | the host's CLIs (incus, docker, podman, tart) through `Runner`; `potluck-conformance`; `BindRunTarget` | `potluck/hostprobe` tests with fake runner and facts, share against an in-process daemon with `refdriver` | 20 | run targets |
| box usage | `potluck/usage` | `rt.From(d).OnEnd` (run targets' end hook); `GET /v0/usage` | one row per ended box in project.db | `potluck/usage` tests, leak suite | 20 | sandboxes |
| E2B gateway | `potluck/cmd/zm-e2b` (a client door: links the shared `potluck/kit/client`, `potluck/kit/api` and `potluck/kit/config` only, no module) | E2B's REST control plane and envd's `/files` and `process.Process/Start`, over the run-target routes | the E2B SDKs (Python and TypeScript, pinned to tested versions) | `potluck/cmd/zm-e2b` tests: recorded SDK requests and the real SDKs against `refdriver` | 20 | sandboxes |
| events | `potluck/kit/events` | bus plus SSE, CloudEvents 1.0 | in-process | `potluck/kit/events` tests | 12 | core |
| mail | `internal/mail` | `orchcore.MailService`, `orchcore.Noticer` | in-process | leak suite | 9 | mail |
| sessions | `internal/session` | `orchcore.SessionService`, `SessionWorkRecorder`; holders: `zm-holder/1` over a Unix socket (`internal/session/holder`) | holder-managed PTYs (survive daemon restarts), hook-registered; pipe mode for structured sessions | `internal/session` tests | 8 | session |
| owed (what a session still owes) | `internal/owed` | `orchcore.OwedSource` (each module's part), `orchcore.PersonSessions`; `GET /v0/owed` | in-process: work (items, PRs), approvals (cards), session (lanes) | `internal/owed`, `internal/wiring/fulltest/owed_test.go`, leak suite | 8 | owed |
| structured-session adapters | `internal/agentpipe` (protocol, runner), drivers in `internal/agentpipe/{claude,codex}`, linked by `cmd/zm` only | process protocol `zm-agent/1`: ops in, events out, `describe` first | claude stream-json, codex app-server; `zm-agent-<name>` on PATH (ACP planned) | `internal/agentpipe/conformance` (recorded real sessions), `zm-agent-conformance` | 8 | session |
| auth | `potluck/kit/authz`, `potluck/kit/project`, `core` auth, `potluck/kit/tailnet` | `authz.Subject`, `core.Authenticate`, `core.NetService` | socket, tokens, tailnet WhoIs | leak suite (`internal/wiring/fulltest/leak_*_test.go`) | 1, 4, 13 | core, tailnet |
| teams and replicas | `internal/team`, on `potluck/kit/teamlink` (its hooks, proxy rows, machine ops and heartbeat parts; `core.Deps.Team` stays teamlink's proxy) | `orchcore.Team`: `TeamReplica`, `TeamLeases`, `TaskRelay`, `RemoteTasks` and the other team seams, `LeaseHolders`, `Snapshotter` | authority, replica | loadtest (SPEC 17.7), `make bench` `replica-*` rows, `leak_team_test.go` | 17.3-17.6 | team |
| team transports | `core` (`transport.go`: the interface and registry); drivers in `potluck/kit/tailnet`, `potluck/kit/tunnel` | `core.TeamTransport`, registered in `base.NewKit` (potluck/wiring) | tailnet (tsnet), tunnel (signed HTTPS behind cloudflared); host Tailscale planned. Both drivers are kept on purpose: tsnet gives WhoIs caller identity (device-bound approvals, node-bound join tokens, mapped logins) and MagicDNS HTTPS for the approvals page, and the tunnel is the transport for a member with no Tailscale (Potluck's front door). | `internal/team` `TestTeamTakesAnyRegisteredTransport`, team tests over both, leak suite over the door | 17.11 | tunnel (seam), each driver its package's area |
| key directory (secret handoff) | `internal/secretkeys` | the routes; `Pins` (the sender's trust on first use) | the registry table; a Postgres authority planned | `internal/secretkeys` tests, leak suite | 17.12 | secret handoff |
| approvals | `internal/approvals` | device routes, signed key answers, `core.WorkRecorder` (work's side) | paired tailnet devices, confirmed P-256 keys; passkeys | `internal/approvals` tests | 16 | approvals |
| daemon swaps | `potluck/kit/swap` | `Check` (preflight), `Probe` (served set), `swap.lock`, `Place` | one daemon per home | `potluck/kit/swap`, `potluck/kit/server/swap_test.go`, `internal/wiring/fulltest/swap_test.go`, `cmd/zm/daemon_swap_test.go` | 2 ("Swaps") | core |
| release channel | `potluck/kit/release`, `scripts/release-sign`, `.github/workflows/zm-release.yml`; `scripts/publish` (each release's one-way copy and tag on the published repository) | `zm-release/1` signed manifest; `zm daemon update --release` (and `hold`, `resume`, `status`); machine numbers' `zm` | GitHub releases, a release directory, the public repo | `potluck/kit/release`, `cmd/zm/daemon_release_test.go`, `scripts/publish` | 2 ("Release channel") | core |
| REST door | `potluck/kit/server`, `core.Router` | routes in API.md | net/http | `internal/wiring/fulltest/routes_test.go`, leak suite | API.md | core |
| CLI and bd shim | `cmd/zm`, `cmd/bd-zm` | the REST door | | smoke (`scripts/smoke.sh`) | 17.1 | core (`cmd/zm`), issues (`cmd/bd-zm`) |
| MCP door | `internal/mcp` | MCP tools | official go-sdk | `internal/budget` (surface size) | 14 | integrations |
| owners (who an owner is; GitHub sign-in; identity links) | `potluck/kit/owners` | `owners.Accounts`, `owners.Device` | GitHub; GitLab | `potluck/kit/owners/conformance` | 22 | owners |
| connections (show) | `internal/connections` | `connections.Fetcher`; the `zm.connection.show/1` contract (`Validate`) | https, Unix socket; a SPEC 11 process | `internal/connections` tests against `fakeci` (the example provider), `zm connection check` | 24 | connections |
| webhooks, GitHub links | `internal/webhooks`, `internal/ghlink` | outgoing events, PR/branch links | HTTP | `leak_webhooks_test.go` | 18 | webhooks, ghlink |
| Mezze app door | `internal/door` | `orchcore.DoorRelay` (`internal/team/door_relay.go`: a `door_delivery` machine op); public hook on `door.listen` | team machine ops; a push to a Mezze app that serves its own public URL (planned) | `internal/door` tests with a fake relay, `internal/team` door op tests, leak suite | 18.4 | door |
| automation records | `internal/automations` (the module: routes, the home rule), `internal/automations/store` | `store.Store` (one project's records, revisions, health and home claims) | `store/sqlite` (project.db); Postgres planned | `internal/automations/store/storetest`, `internal/automations` tests, leak suite (`leak_automations_test.go`, `leak_team_test.go`) | 18.6 | automations |
| outreach connection (zm-connect) | `deploy/cloudflare/zm-connect` (the Worker: guard, OAuth, sync, vault DO), `internal/connect` (`bridge`, `dbrole`), `cmd/zm-connect` | `Mailbox`, `Store` (Worker); the bridge protocol (SPEC 23) | Gmail, Postgres via Hyperdrive; Instantly planned | Worker vitest, the cross-language signing vector, `dbrole`'s real-Postgres test of the Worker's SQL | 23 | connect |
| phone push | `internal/webui` | `core.DevicePush` (the Decisions page's subscriptions) | Web Push (Apple, Mozilla, Google) | `internal/webui` tests | 13.1, 16 | phone push |

Seams with their own AGENTS.md: `potluck/kit/server` (in sengirunners/potluck), `internal/work/adapter`, `internal/issues/store`.

## Known drift (each listed until fixed)

As of 2026-11-01:
- **Cross-brick table reads** (become `core` methods):
  - `events`: `potluck/kit/server/server.go` and `internal/work/work.go`. Area: core, with work.
  - `tokens`: `internal/ghlink/http.go` and `internal/webhooks/webhooks.go`. Area: core.
- **Team's copy tables (drift 3b, corrected).** Team's SQL on `mail`, `leases`, `comments` and `meta` reads
  its **own replica copy tables** (`internal/team/replica.go`, migration `team/0001_copy`), not mail's or work's.
  But they reuse those names, so a grep or a lint cannot tell them apart. Rename them `copy_*` before any
  table-ownership test. Area: team.
- `internal/work/team.go` (team-owned) migrates `work/0002_leases` next to work's `work/0002_mirror`: two owners
  and a duplicate number under one prefix.
- `internal/work` is the largest package (7.2k non-test lines at its top level on 2026-11-01, after homes left).
  Do not grow its top level; new code goes into a sub-package with its own interface.
- `core.Deps` is turning into a service locator with hooks. Every field or hook needs an area listed in the
  package doc. The list in `potluck/kit/core/core.go` starts with Homes and Trackers; older fields are still to add.

## Open-source decisions

| area | decision |
| --- | --- |
| tracker homes and backups | zm's SQLite: VACUUM INTO (`potluck/kit/sqlbackup`), not Litestream, whose WAL history keeps redacted text until its retention expires (DESIGN-NOTES "Native store backup"). rclone uploads the off-site home sets (pinned binary, crypt for client-side encryption; `internal/homes/offsite`) |
| OAuth client credentials | adopted `golang.org/x/oauth2/clientcredentials` (#46), keeping our 0600 token cache |
| GitHub sign-in | `golang.org/x/oauth2`'s device flow (`DeviceAuth`, `DeviceAccessToken`), run by the authority; GitHub's `/user` call is ours (one GET) |
| outbound calls to caller-named URLs | `potluck/kit/netguard` (moved out of webhooks 2026-11-05): one SSRF guard shared by webhooks and connections |
| webhook signatures | Standard Webhooks added (#47); `Zm-Webhook-Signature` is deprecated with a window in SPEC 18; the GitHub-style header stays |
| secrets | the Secrets brick (#48: file, keychain, env; a third-party store such as `op` would be a plugin); no go-keyring (it needs D-Bus on Linux) |
| Postgres authority | design the store topology first; sqlc is optional after that |
| events, router, migrations, SSE, TUI loop, MCP | keep: small, spec-shaped, or the official SDK |
| session holders | our own on creack/pty (`internal/session/holder`): tmux, dtach and abduco are not on a stock Mac, and none hands over raw bytes at offsets, the wait status and the PTY master; a second driver could be one long-lived PTY host for all sessions |
| supply chain | govulncheck in CI and Dependabot (#50) |

## Proposing rules

AGENTS.md files hold traps, not maps, and they have byte budgets. An agent that hits a trap worth a rule does not
edit an AGENTS.md itself. It proposes the rule under **"Suggested rules"** in its PR description or report. A
maintainer adds the ones that recur, and only if all three hold:
1. **Non-obvious:** a capable agent reading the code would still get it wrong.
2. **Recurring:** it has happened more than once, or would on every pass through that code.
3. **Actionable:** concrete enough to follow or check, ideally naming the test or lint that enforces it.

If a rule can become a test or a lint, write the test, and keep at most one line of prose for it.
