# Design notes (v0 architecture pass)

Decisions taken before the six builders start, and how the independent review was applied. SPEC.md is the
contract; this file is the why.

## Decisions

1. **Trust model first** (SPEC section 1). The socket is one principal, the OS user, with scope *all*; checks
   there are guard rails. The tailnet listener is the only hard boundary: bearer token or mapped WhoIs login,
   else 401. The boundary is fail-closed (second pass): "local" is a positive mark the Unix listener sets
   per connection; everything unmarked is network. Tokens on the socket always narrow and never pass `local` routes. This removes the fake
   boundary the review called out (the old "agent can unset ZM_AS and become the human" text and the
   "tailnet callers are machine:<peer>, header ignored" sentence are gone).
2. **Scope is structural.** `authz.Scope` is the only way to get data: `store.Project(scope, action, id)`,
   `store.Projects(scope, action)`, `scope.SQL(...)`, `scope.AreaSQL(...)`; every core service interface
   takes `*authz.Subject`; `authz.Subject.Check` returns `api.NotFound` (byte-identical to a missing id) for
   invisible objects. `core.Router` records every route with its guard; the golden leak test walks all of them
   as a guest. Builders cannot add a route the test does not see.
3. **Storage split** (review finding 3). registry.db = projects, identities, grants, tokens, **sessions,
   leases**, machine-level events: one transaction domain for "who holds what and is it alive". project.db =
   mail, local work items, comments, that project's events. Mail-to-work routing reads the lease (registry)
   then the item (project.db) without a cross-file transaction; the lease is authoritative.
4. **Event logs are per project, the cursor is one ULID.** The task asked for per-project logs with
   `(origin, seq)` cursors; the review forbade exposing a global seq. Both are met by minting every event id
   from one in-process monotonic ULID generator under the bus lock: ids increase in publish order across all
   logs, so a single id resumes a merged stream (`Since` queries each visible log for `id > cursor` and
   merges), and the random increments mean gaps reveal nothing. `origin` is the CloudEvents `source`
   (`zm://<machine>`). No seq is stored or sent. If multi-machine replication ever lands, the cursor becomes
   `(source, id)` per origin.
5. **Redaction is generic.** Events carry `Mentions` (addresses that might be hidden); the bus replaces them
   with `someone` per subscriber using a visibility hook the server wires to the session module. Handlers
   use `d.Redact` for the same rule.
6. **Local work ids are random** (`local:<6 base32>`), not `local:<n>`: a guest seeing `local:3` and
   `local:9` would learn that five hidden items exist.
7. **project.toml without a TOML dependency.** The allowed dependency list has no TOML library, so
   `potluck/kit/project/toml.go` implements the subset the files use (tables, arrays of tables, strings, string
   arrays, inline string tables, ints, bools, comments) and rejects anything else, including unknown keys.
8. **Repo files name projects by slug**, registry rows by ULID. ULIDs are per machine; a committed
   `.zm/project.toml` must work on a teammate's machine.
9. **Resolution lives in the daemon.** The client sends `Zm-Project`, `Zm-Project-From` and `Zm-Cwd`;
   `core.Deps.WriteTarget` resolves on demand (it needs the session registry). `$ZM_PROJECT` resolves
   writes but does not narrow lists; `--project` does both.
10. **Protocol negotiation is a header on every request and response**, not a handshake: no extra round
    trip, both sides refuse a different major, and `whoami`/`health` report protocol, version and commit.
11. **`local` means "owner on the socket without a token"** so a narrowed token on the socket (tests, future
    remote-launched sessions) can never mint tokens or spawn.
12. **The leak test lives with the server** and seeds through raw JSON on the HTTP contract (second pass:
    not module-owned client methods). Seeding steps that
    return 501 are skipped; once implemented they must succeed. When a module needs different fixture
    inputs, the integrator changes the fixture, not the assertions.

## Review findings

| # | severity | finding | disposition |
| --- | --- | --- | --- |
| 1 | blocking | Authz vs "who is calling" contradiction | **Applied.** SPEC section 1 trust model; tokens hashed, capped, revocable, `ls`/`revoke`; WhoIs mapping; sessions inherit the launcher's scope via `Subject.ForSession`. *Partly deferred:* sessions are launched only by the local owner in v0 (spawn/register are `local`), so there is no token to hand them yet; the spawn-token path is specified for when remote launch exists. |
| 2 | blocking | Enforcement by prose | **Applied.** Structural scope (decision 2), one golden leak test over every route, both transports; identical not_found; `someone` redaction (holders, acked_by, conflict data); sessions have a required project (`none` = owner only); no global seq (decision 4); random-increment ULIDs; hidden links collapsed per rel. CLI autocomplete is covered because the CLI only reads through these routes. |
| 3 | blocking | Undecided cross-db semantics | **Applied** (decisions 3, 4). One `store.Store` owns all handles with fixed connection limits; one `store.Migrate(home)` walks registry + every project. |
| 4 | major | v0 scope too large | **Applied.** Cut: srt sandbox, Tailscale grant-to-role mapping, OTel, SCIM/cedar/OpenFGA/biscuit language, archive/handover, focus sets, peer mail forwarding, automatic worktree cleanup, the GitHub adapter (stub only; beads is the v0 adapter because it is local and needs no account). *Kept, small:* `project export` (VACUUM INTO + the toml, about 40 lines), because "a project can be exported" is a plan requirement and costs nothing. |
| 5 | major | Resolution order and hooks | **Applied.** cwd rules beat the session's project; hooks register into `none` and print nothing; the "inbox project" is replaced by `default_project`, never set by installation, reported by `zm project why`; explicit rules that miss stop instead of falling through. |
| 6 | major | Lazy lease sweep, renewal storm, escalation target | **Applied, then changed (2026-11-03).** Active ticker (30 s): `lease_expired` event + needs=human mail to the project's `owners` group mailbox (a new address keyword, not a person). 78 such mails in a day were all noise, 0 acked, every holder active later: the mail now goes to the holder session only, needs=agent, and no person is mailed. Bound leases for managed sessions renewed by the daemon, never the agent (second pass: with a real deadline, see below); TTL renewals only by explicit claim, never from hooks. Adapter-arbitrated claims keep a local lease row. |
| 7 | major | "Kill rather than freeze" | **Applied.** Refuse only, typed reason `memory|disk|sessions`, event, no automatic kill; macOS `kern.memorystatus_level` + swap used + disk free + session count; pure function with a table test. |
| 8 | major | Body scanning; `zm inbox` outside a repo | **Applied.** Routing reads only `to` and `ref`; reads default to the union of visible projects. |
| 9 | major | Daemon restart kills sessions | **Applied.** Outcome `infra` "daemon restarted"; `zm daemon stop` refuses without `--force`; future per-session supervisor noted in SPEC section 8. No `daemon upgrade`. |
| 10 | minor | Quota scraping | **Applied.** Exit code + per-runtime detector table, `evidence` + `confidence`; unmatched is `failed`, never `ok`. |
| 11 | minor | Token revoke/list, hashing, identities | **Applied.** `zm token ls|revoke`, sha256 only, identity = (ulid, name, kind, tailscale login), remote access = tailnet only. |
| 12 | minor | install writes ~/.claude | **Applied.** `zm install --home DIR`, dry run by default (`--apply` writes), diff printed; `make test` runs with a temp `HOME` so a test that touches the real home cannot pass silently. |
| 13 | minor | Budget only covers AGENTS.md | **Applied.** `internal/budget`: snippet + MCP tool descriptions + max notice length, one number (8 KiB), one test. |
| 14 | minor | Worktree cleanup still guesses | **Applied.** v0 never deletes worktrees; `zm session gc` lists with reasons (outcome, dirty, ahead count). |

Nothing was rejected outright.

## Things builders will hit

- `orchapi.Claim` is an alias of `orchapi.Lease`, `api.Spec` of `api.Protocol`, kept so ported wip code compiles.
- Mail and other module ids are strings (ULIDs) now, not int64.
- The wip branch (`git show wip/v0-run1:<path>`) has useful module code (mail routing, PTY sessions, tui,
  integrations, beads, conformance). Port it, but it predates projects: every query needs a scope, every row a
  project/area, every returned address a redaction check.

## Second review (skeleton)

| # | severity | finding | disposition |
| --- | --- | --- | --- |
| 1 | major | Network boundary was fail-open: no `WithRemote` meant owner | **Applied.** `core.WithLocal` is set only by the Unix listener's `ConnContext` (on `*net.UnixConn`); `Authenticate` treats every unmarked request as network (token, else WhoIs login, else 401); `WithRemote` now only carries WhoIs data and overrides a local mark. `Daemon.SocketHandler()` is the in-process owner for tests. Tests: `TestUnmarkedListenerFailsClosed` walks every route over a plain TCP `httptest.Server` (401, health 200) and checks `markUnix` ignores non-Unix conns; core table cases for unmarked transports. |
| 2 | major | Bound leases had no deadline: an idle holder kept work forever | **Applied.** Bound leases carry `expires`; the ticker renews (`now + bound_lease_ttl_seconds`, 3600) only while the holder is `running`, releases ended holders at once, and expires every lease past its deadline with the usual event + `owners` escalation. Managed sessions go `idle` after `idle_after_seconds` (300) without output, so a hung agent stops renewing. `Lease.Expires` is always set; inflight shows it. Config: `Config.BoundLeaseTTL()`, `Config.IdleAfter()`. |
| 3 | major | Slow SSE subscribers lost events silently | **Applied.** Publish never blocks; a subscriber more than its buffer behind is cut off (channel closes, no drops past that point). SSE ends with `: overflow <last id>` (live-only streams get a pre-subscription cursor so the id is never empty); `ReadSSE` returns `*events.LagError` (`errors.Is(err, events.ErrLagged)`); `client.Events` reconnects from the last event it delivered and gives up after `MaxResumes` stalls. Tests: bus cut-off + `Since` fills the gap exactly; SSE over a blocking pipe ends with overflow and replay is complete; client resume. |
| 4 | major | Leak test gaps (live SSE, body-bearing writes, builder-owned seeding) | **Applied.** Guests walk every route over three transports (tailnet, unmarked network, socket+token). `leak_writes_test.go`: live SSE as the guest (hidden project, project-level, hidden area and machine events never arrive; hidden session redacted to `someone`); a table of 15 body-bearing writes naming a hidden session, item, other-project item, ref, area, project or link target, each equal to its missing twin (rows with unseeded prerequisites run with stand-ins and start biting when the module lands). Seeding is raw JSON per API.md; the five seeding bodies are frozen there. |

Found by the new tests and fixed: `POST/DELETE /v0/grants` naming an existing area the guest cannot see
answered `no project "harbor"` while a missing area answered `no area "nope-area"` (an area-existence
oracle). `project.Registry.grantTarget` now answers `NotFound("area", area)` for both.

Decided along the way: lease keys are unique on `(project, area, key)`, not `(project, key)`, so a lock held in
a hidden area can neither conflict with nor be detected by a caller in another area. Work item keys take the
item's area, so they stay unique per project. An item's area can change under its lease (`zm area set`, a
relabel in the tracker, project.toml edited before a restart), so a work item's lease follows it: every mirror
write or re-area moves the lease rows of tracker items to the item's mirrored area, and claim, renew, release,
who and show find a work item's lease by `(project, key)` whatever area holds it (a row left behind by a crash
in between is moved on the next claim or release). Otherwise the holder could not renew or release, and the
ticker would expire it and release the item in its tracker under a holder still working.

## Integration (after the six builders)

Decisions the integrator took on the builders' open questions:

| question | decision |
| --- | --- |
| GitHub adapter: stub or ship? | **Ship.** It is built and tested against a fake `gh`; no account is needed to test it. SPEC sections 11 and 15 list it. |
| Sandbox, cockpit focus sets | **In v0**, both optional and small: `--sandbox` refuses without `srt` (never runs unsandboxed silently); focus sets live in `$ZM_HOME/top.json`, not config.json, because `config.File` drops unknown keys on save. |
| bubbletea queries the terminal colour at init in every TTY `zm` call (up to 5 s if nobody answers) | **Superseded** (see Latency below: bubbletea is gone from the binary). Was: **Keep one binary.** The daemon answers a cursor-position query (`ESC[6n`) in a managed session while no client is attached, so the probe returns at once (termenv sends OSC 11 then DSR 6 and gives up on the first non-OSC reply). A normal terminal answers by itself. Splitting out a `zm-top` binary would break the plan's one-binary rule. |
| Two detach keys (`ctrl-]` vs `ctrl-] d`) | **`ctrl-]` everywhere**, as `zm run`/`zm attach` already did; `tui.Attach` matches. |
| Can an unmanaged session report its own end? | **Yes.** Claude Code's `SessionEnd` hook runs `zm hook claude session-end`, which registers with `end` (outcome `ok`). Codex has no such hook, so those sessions still end only by the reaper. |
| A PostToolUse hook to clear a hook-set needs_human in your own terminal | **Not in v0.** "running" from every tool call would also clear a mail-set needs_human, which only mail may clear; the next prompt, Stop or idle notification clears it instead. |
| `IdleManaged` on `core.SessionService` | **Not added.** Mail falls back to the session module's `List` (and logs once if a module has neither). `OwnerOf` was added to `core.WorkService`. |
| Peer mail forwarding, grant-capability mapping | **Still out** (SPEC sections 13 and 15). `mail.Service.SetForwarder` and `tailnet.Who.Caps` are the seams. |
| Health and the machine name | `/v0/health` names the machine to every caller: the tailnet hostname (`zm-<machine>`) already does, and peer discovery needs it. pid and start time stay local-only. |
| `ZM_HOST` | A bare name means `http://zm-<name>:7433`; a host or host:port gets `http://`; a URL is used as is. |

Found during the end-to-end smoke (`scripts/smoke.sh`) and fixed: a live SSE reader held `zm daemon stop`
for the full 5 s shutdown grace (streams now end when shutdown starts); `zm send <project>/<id>`, the form
`zm work ls` prints, was refused; `zm mcp` dropped every reply when its stdin closed right after the
requests (`printf ... | zm mcp`).

## Approvals (SPEC section 16)

| question | decision |
| --- | --- |
| What does the decide right bind to? | **The WhoIs stable node ID and node key**, both, checked on every call. Not the login (the phone and every agent on the Mac share it), not a token (any socket caller can mint one), not a name. A re-keyed node pairs again. |
| How do routes reach an unmapped device? | A new guard, `core.Device`: it ignores token/login authentication, refuses the socket and any token (`403`) and a network caller without a WhoIs node (`401`), before the handler looks anything up. Unmarked listeners still get `401`, so the fail-closed test holds. |
| "This daemon's own node" | `core.Remote.Self`: the tsnet node's own stable ID, **or** a caller whose Tailscale IP is one of the host's interface addresses (the Mac's own Tailscale app, where agents run). If the host's addresses cannot be read the caller counts as this machine. Pairing and deciding refuse `Self`. |
| "A node already bound to a non-approver session" | Read as: nodes that run agents or are not a person's device. Refused: tagged nodes, zm nodes (other daemons: `core.Remote.Zm`, from the node's own Hostinfo hostname or its tailnet name starting `zm-`; `tailnet up` refuses any other hostname, and the Hostinfo name survives an admin-console rename), and a node already bound to another approver. zm keeps no other node-to-session binding to consult. |
| Bootstrap vs. confirmation | Decided at **pairing** time, inside the transaction: the first device to pair while no approver is active is bootstrapped; any later one is `unconfirmed` until an existing approver confirms it from their device. Deciding at `add` time would let two pending adds both bootstrap. Bootstrap is **once per daemon** (`$ZM_HOME/approvals/bootstrapped`, created in the pairing transaction with `O_EXCL`, never removed by the daemon): rm and add are local, so every agent holds them, and re-opening bootstrap when no approver is active let an agent rm everyone and pair a tailnet node it reaches (a dev or farm box). Recovery is the human deleting that file by hand. A file, not a row, so the recovery is a step outside the API. |
| Where does an ask's subject live? | In the approvals module's own `asks` table (registry.db), keyed by the mail id. The mail schema and `SendMailRequest` are unchanged; routing still never reads bodies. |
| How does a decision clear the asker's `needs_human`? | A new `core.MailService.Reply` (same checks as `POST /v0/mail/{id}/reply`); approvals answers as the machine's human (`Projects.Local`), naming the approver and device, which takes the mail. So an ask goes only to `owners` or the asker's machine's human (at a team authority, a member machine's own human, never the authority's OS user; the decision's mail is from the human it was put to); a failed answer is returned as the decision's `mail_error`, and a failed ask insert answers the already-sent mail. |
| Receipt format | ed25519 over `"zm-receipt/1\n"` + compact JSON of the seven fields in a fixed order; `key_id` names the key. The key is `$ZM_HOME/receipt.key` (0600, created on first use; a corrupt file is an error, never silently replaced). The CLI refuses to verify without repo, PR and head unless `--signature-only`, and requires `approve` unless `--decision` or `--any-decision`. |
| Name hygiene | Names are **refused**, never folded: input must already be lowercase, trimmed, NFKC and ASCII. On `add`, a confusables skeleton (a TR39-style subset: Cyrillic/Greek/Latin lookalikes, `0/o`, `1/l/I/i`, `2/z`, `5/s`, `6/b`, `8/b`, `9/g`, `rn/m`, `nn/m`, `vv/w`, `cl/d`, separators dropped) must not match another approver or an identity; identity creation checks the same rule against approvers (`project.Registry.SetIdentityGuard`, wired by the server). `golang.org/x/text` (already in go.sum via tailscale) is now a direct dependency for NFKC; the full confusables.txt is not embedded. |
| QR | No QR library is pinned, so `internal/approvals/qr.go` is a small encoder (byte mode, level L, versions 1-5). Checked by structure, RS syndromes and, by hand, decoding with macOS CoreImage (`TestQRWritePNG`). |
| CSRF and DNS rebinding on the device page | POSTs must be JSON with `Zm-Protocol` (a cross-origin page cannot send either without a CORS preflight, never granted) and not `Sec-Fetch-Site: cross-site`; device routes answer only a Host that is this daemon's tailnet name or a Tailscale IP. |
| GitHub side effects | Behind `approvals.Publisher`; the default `GitHubBot` uses a per-project bot token file (0600) and posts a commit status and a PR comment with the receipt. Tests use a fake and an `httptest` GitHub. |
| Approver scope | Approvers are machine-wide (they see every open ask on this daemon); per-project approvers are not in this pass. An ask's `to` routes its mail only; it does not pick the approver. |

## Latency (2026-10-30, stream FAST)

Goal: native speed for everything that is not a tracker. Budgets, p95 on a reference M3 Mac, process start
included: hooks and `notice` 15 ms, reads 30 ms, `zm top` to its first frame with data 100 ms, MCP
initialize + tools/list 50 ms; tracker reads (`work ls/show/who/sources`) 50 ms against an 11k-item tracker
after its first sync. `make bench` (`scripts/bench.sh`, harness `scripts/bench`) measures them
against a real daemon in a temp `ZM_HOME` (3 projects, 61 sessions, 42 leases, 5,040 mails with 4% open,
a fake tracker of 11,512 issues served by the bench binary as `zm-work-fake`, `internal/work/fake`),
plus the daemon's share of single requests on a kept-alive socket (`api-*`), and fails over budget. The
`*-tty` rows run on a pty nobody answers (an agent's or CI's). The Mac was shared with other builds, so a
few tenths of a ms is noise.

| measurement | main (d7de4a6) p50 / p95 | perf/fast p50 / p95 |
| --- | --- | --- |
| `zm --version` | 7.1 / 7.6 ms | 7.7 / 8.6 ms |
| `zm hook claude prompt` (UserPromptSubmit) | 9.3 / 10.1 ms, 2 requests | 8.7 / 10.3 ms, 1 request |
| `zm hook claude stop` | 8.9 / 9.5 ms | 8.1 / 8.7 ms |
| hook with the daemon down | 8.5 / 9.0 ms | 7.6 / 8.3 ms |
| `zm notice` | 8.6 / 9.0 ms | 8.4 / 9.2 ms |
| `zm inbox` | 11.7 / 13.4 ms | 9.3 / 10.1 ms |
| `zm send` | 9.1 / 9.9 ms | 8.7 / 9.1 ms |
| `zm ls` | 9.2 / 9.8 ms | 8.8 / 9.4 ms |
| **`zm ls` on a terminal** | **5,024 / 5,038 ms** | **9.0 / 9.6 ms** |
| `zm leases` | 9.6 / 11.0 ms | 8.7 / 9.6 ms |
| `zm project why` | 8.8 / 9.4 ms | 8.2 / 8.8 ms |
| `zm events --since 0`, first event | 13.6 / 14.3 ms | 8.9 / 10.9 ms |
| `zm mcp` initialize + tools/list | 10.2 / 11.7 ms | 9.2 / 10.2 ms |
| **`zm top`, first frame with data** | **5,032 / 5,035 ms** | **21.5 / 23.4 ms** |
| daemon exec to first health answer | 11.9 / 12.7 ms | 9.5 / 10.1 ms |
| request: human inbox (5k mails) | 1.9 / 2.2 ms | 0.6 / 0.9 ms |
| request: human inbox (40k mails) | 4.3 / 6.6 ms | 2.3 / 4.6 ms |
| request: notice, sessions, leases, state, guest inbox | 0.1-0.3 ms | 0.1-0.2 ms |

Integrated (perf/integrate = mirror + fast + fixes), `N=100 make bench`, p95: `--version` 7.2 ms; hooks and
`notice` 7.7-8.7 ms; reads 8.6-9.2 ms; `mcp` 10.2 ms; `top` first frame 20.4 ms; daemon ready 9.0 ms;
requests 0.1-1.0 ms. Tracker reads from the mirror, 11,512 issues, process start included:

| measurement | main, live harbor beads | perf/integrate p50 / p95 (fake, same size) |
| --- | --- | --- |
| `zm work ls` (owner) | 2.0-3.3 s | 9.3 / 9.8 ms |
| `zm work ls` (guest of a label area) | 2.7 s | 9.3 / 9.7 ms |
| `zm work ls -q "module 17"` (search) | a `bd search` | 21.5 / 22.1 ms |
| `zm work show bd:...` | 2.5-2.9 s | 8.4 / 8.6 ms |
| `zm who` on a label-area item | 3-7 s | 8.1 / 9.1 ms |
| `zm work sources` | 3.2 s | 8.8 / 9.1 ms |

What changed and why:

| change | decision |
| --- | --- |
| bubbletea out of the binary | Its package init asks the terminal for its background colour (OSC 11 + DSR) in **every** process that links it, and Go runs every linked package's init, so no import arrangement in one binary avoids it. `internal/tui/tea` is the part of the Elm loop the cockpit uses (Model/Cmd/Msg, keys, ticks, batch, exec for attach, window size, line-diff rendering) on x/term and cancelreader; nothing runs at init and nothing queries the terminal. lipgloss stays (no init work; plain colours never ask for the background). `TestNoTerminalProbeAtStartup` runs the real CLI on an unanswered pty and fails on any terminal query or a slow exit. bubbletea is out of `go.mod` and `internal/deps` too, so a stray import fails to build instead of bringing the probe back (`internal/deps` test checks go.mod). |
| go-runewidth held at 0.0.24 (2026-11-01) | 0.0.25+ fills its width tables in `init` (18 ms on every process that links it; lipgloss and x/ansi do), which took `zm version` from 8 to 29 ms p95 on the CI VMs and failed every 15 ms CLI budget in Dependabot's #53. Found with `GODEBUG=inittrace=1 zm version`. x/ansi is held at 0.11.8 because 0.11.9 requires it; `.github/dependabot.yml` ignores both until runewidth builds its tables lazily again. |
| Hooks: one request | `POST /v0/sessions/hook` = register-or-find by runtime id + state + (prompt, start) the session's own notice, via `core.Noticer` (mail implements it; an optional interface so other modules' `MailService` fakes need no change). Covers the old "not found, register, report again" path too. A daemon without the route gets the old request-per-step path. One deadline per call, stdin included: 300 ms on the prompt path (prompt, notification, stop, notify, `notice`: state and notice are best effort and the next hook repeats them), 2 s for session-start and session-end (the register or end must land). A stopped daemon fails at connect (no socket or refused); a wedged one (accepts, never answers) costs at most the bound (`TestPromptPathWedgedDaemon`: was 2.0 s, now ~0.3 s). The hook is silent either way. |
| CLI never reads the user database | `config.LoadClient` (CLI) takes the human from `ZM_HUMAN`, config.json or `$USER`; `os/user` cost ~1 ms per call on macOS. The daemon still uses `Load`, which owns the identity. |
| SQLite | Already WAL, `synchronous=NORMAL`, `busy_timeout` 5 s, IMMEDIATE. Added: a per-connection prepared-statement cache under database/sql (`store/stmtcache.go`, modernc `NewConnector`; prepare is ~2/3 of a small query: 9.5 vs 3.3 us), so modules keep writing plain SQL; busy statements (rows open) are never reused, LRU 128 per connection; the rows wrapper forwards the driver's column-type interfaces so `rows.ColumnTypes()` is unchanged. Every pool connection stays open (the default of 2 idle closed registry connections 3-4 after each burst). Migrations read `schema_migrations` once per file. `PRAGMA optimize` at open, hourly and at close. |
| Indexes | The human inbox's "needs=human to this machine's sessions" term was `substr(recipient, ...)`, which forced a scan of the whole mail table on every human inbox. Now an index range (`lo <= recipient < hi`) over a partial index of open mail (`mail/0002_open`); the orphan sweep uses it too. Plans checked with and without `ANALYZE`. Session, lease and notice queries were already indexed (`sessions_scope`, `sessions_runtime`, `leases_holder`, `leases_expires`, `mail_recipient`). |
| Start-up work | Outcome detector regexps compile on first use; address grammars are byte loops (tested equal to the regexps). Our packages' init went from ~0.45 to ~0.1 ms (cobra's command tree). |
| Event replay | First page 50 events, then 500: the first event is on the wire at once. |
| Client | 8 idle connections kept per process (cockpit, MCP), no gzip on the socket. |

Checked and left as is (measured):

- **Process start is the floor**: `--version` is ~7 ms; a hello-world Go binary is 2.8 ms here. The rest is
  the runtime plus dependencies' init (tailscale/gvisor via tsnet, the MCP SDK, whose `encoding/gob` alone is
  ~0.4 ms, modernc sqlite) and paging in a 54 MB binary. Stripping symbols changes nothing. Only a separate
  client binary would go lower, which the one-binary rule forbids. Every hot command is within ~1-2 ms of it.
- **Auth and resolution per request**: the socket owner's subject is built in memory (no registry read). A
  token caller costs three indexed registry reads (token, identity, grants), 0.1 ms for a whole guest inbox;
  not cached, because revocation must take effect on the next request and the invalidation would live in
  `potluck/kit/project`. Project resolution reads the cwd's repo files on writes only (~37 us in a worktree); a
  stat-validated cache would do the same syscalls.
- **JSON**: encoding/json caches its reflection per type; whole requests are 0.1-0.6 ms.
- `internal/approvals` still compiles regexps at init (~0.17 ms); not this stream's package.

## Zero-install start (SPEC 17.1, round A stream A1)

Goal: a stranger goes from nothing to tracking work in under a minute: one binary, `zm init`, `zm work
new`. No daemon to start by hand, no tracker, no account.

| question | decision |
| --- | --- |
| Where does autostart live? | In the client transport (`client.Client.Autostart`), not in each command: `send` calls it when a socket request **never connected** (no socket, or refused), then sends the request once more. Nothing reached a daemon, so the retry is safe for writes too. The CLI (`cmd/zm/autostart.go`) sets it in `newClient` unless the running command is excluded; nil (every other client: tests, MCP, the bench) never starts anything. |
| Which commands never start it | `help`, `completion`, `--version` (no client at all), `status` (it answers "is it running", exit 3), `daemon` and `daemon stop`, `mcp`, and everything hooks run: `hook`, `session hook`, `notice`, `session register`, `session state`. A hook must stay silent and within 15 ms with no daemon (bench row `hook-down`), so it may not spend ~10-60 ms starting one. Matched by command path, walking parents, so subcommands inherit. `ZM_NO_AUTOSTART=1` (also `true`, `yes`) turns it off; `0` does not. |
| Once per process | A package flag (`everConnected`) records that this process reached a daemon; after that a missing daemon is reported, never restarted. `zm daemon stop` while `zm top` or `zm events` runs must stay stopped. A start is also attempted only once per process, success or failure (`startAttempt`, under a mutex so concurrent requests share it): a long-lived caller that keeps finding no daemon after a failed start gets the same `unavailable` at once, not another 10 s start per request. An attempt cut short by the caller's context does not count. |
| Two first commands at once | Starters take `flock` on `$ZM_HOME/autostart.lock` and re-check health under it. One daemon starts; only its starter prints the line (`TestZeroInstallStart` runs four at once). Without the lock the loser's daemon exits on `daemon.lock` and its starter would report a failure for a daemon that is in fact up. |
| The daemon's environment | The detached daemon (autostarted or `daemon --detach`) drops the starter's identity: `ZM_AS`, `ZM_PROJECT`, `ZM_TOKEN`, `ZM_HOST`, `ZM_SESSION`, `ZM_NO_AUTOSTART`, `CLAUDE_CODE_SESSION_ID`, `CODEX_THREAD_ID`, `CLAUDECODE`. An agent's first command may well be the one that starts it. An **autostarted** daemon also drops `BEADS_*`, `BD_*` and `DOLT_*`: the first command can run in any shell, and a live `BEADS_DOLT_SERVER_HOST` there must not reach bd through a daemon nobody started on purpose (`TestDaemonEnv`). `zm daemon` and `daemon --detach` still pass them, because existing beads bindings (the dogfood setup) reach their server through the shell's settings; 17.2's per-adapter allowlist replaces both and is where "never reaches bd" is proved end to end. Everything else is inherited as before (`ZM_MACHINE`, `ZM_HUMAN`, `PATH`). Readiness is polled every 5 ms (was 50), so a start no longer waits up to 50 ms past the daemon's first answer (~10-20 ms after exec). |
| `init`: "already resolves" | Resolved with `--project`, `$ZM_PROJECT` and the session cleared, and counted only for the directory's own rules (`cwd-file`, `cwd-path`, `cwd-remote`): `ZM_PROJECT=x zm init` in a new directory still creates its project. |
| `init`: the slug | `--slug`; else a `.zm/project.toml` above the directory naming a project this machine lacks (a teammate's pinned repo: the file then resolves); else the origin's repository name (a worktree's directory is usually its branch); else the directory name. Lowercased, other characters folded to one `-`. A taken slug is `conflict` with the `--slug` hint, never a suffix. |
| `init`: nested and dangerous directories | A different `--slug` where a broader project's path matched creates a nested project only when its own path rule is longer (longest match then picks it; checked with `project.PathMatch` on the existing project's and areas' rules); the same repo root again is `conflict`, as is a pinned directory. A remote another project (or area) already lists stays with it: a second clone given its own `--slug` gets its path rule only and the created line says `remote R stays with project P`, so later clones still resolve to the original (before, the newer project silently won `cwd-remote` for every future clone). `$HOME` and `/` are refused, and a git repo whose root is `$HOME` or `/` (a dotfiles repo) is ignored as a root: a directory under it is treated as outside git (path rule only, no dotfiles remote), and `--pin` writes into that directory, never `~/.zm/project.toml`, which every directory under `$HOME` would find. After creating, init resolves again and says so if the directory still resolves elsewhere. A write from a socket caller in a directory nothing claims is told to run `zm init` first (the MCP `project_current` hint too). |
| MCP with no daemon | `initialize` and `tools/list` never call the daemon; tool calls get the client's `unavailable: zm daemon not running at <socket>` as an MCP tool error (`isError`). `mcp.NewServer` clears `Autostart` on its client copy, so the server never starts a daemon whatever it is handed (`TestNoDaemon`). |
| Release tarballs | `make release` -> `scripts/release`: `go build -trimpath` with `CGO_ENABLED=0` (modernc sqlite, creack/pty and tsnet need no cgo; linux binaries are static ELF), packed by `archive/tar` with sorted entries, root ownership and one time (`SOURCE_DATE_EPOCH`, else the commit's time), so the sums are reproducible (checked: two runs, same sha256). darwin/linux x arm64/amd64; `TARGETS=` narrows. ~20 MB per tarball. `-out` is written into, never deleted: only old `zm_*.tar.gz`, `SHA256SUMS` and `.build` in it are replaced, so `-out .` is harmless. No signing or notarisation yet: a downloaded macOS binary carries the quarantine attribute (`xattr -d com.apple.quarantine zm`), which a Homebrew tap will avoid later. |

Measured on a reference M3, `make bench` (n=40, n=10 for zero-start), every existing budget still met
(`--version` 6.7 / 7.6 ms, hooks 7.2-7.6 / 7.5-8.3 ms, reads 7.1-9.5 / 7.4-13.6 ms, top 23.8 / 28.7 ms):

| measurement | p50 / p95 | budget |
| --- | --- | --- |
| `zero-start`: fresh HOME, `zm init` (starts the daemon) + `zm work new` printed | 33.0 / 37.6 ms | 500 ms |
| the same from an untouched darwin/arm64 tarball, unpack included (3 runs, Mac under load) | 0.51-0.61 s total (unpack 0.09 s; most of the rest is macOS checking a never-run binary on its first exec) | under a minute |
| `mcp-down`: `zm mcp` initialize + tools/list, no daemon | 10.1 / 12.3 ms | 200 ms |
| `hook-down`: prompt hook, no daemon (never autostarts) | 6.8 / 7.5 ms | 15 ms |

## Trackers (SPEC 17.2, round A stream A2)

**Removed 2026-11-09:** the beads, github and linear adapters and the pinned bd, since no
project used them (every tracker went native). The record below is how they were built; the generic parts
(live add, rollback, the environment allowlist, concurrent adds, setup in the protocol) stand. Tests use
`internal/work/fake` (`zm-work-fake`), a stand-in adapter over a JSON state file.

Goal: `zm tracker add beads` takes a stranger from nothing to a working tracker in one command, without the
onboarding a first user hit (Homebrew could not install dolt: EPERM on a non-default prefix; `bd init` wanted safety
tokens and printed warnings; the shell's live farm `BEADS_DOLT_SERVER_*` variables decide which server any bd
talks to).

| question | decision |
| --- | --- |
| Which bd and dolt? | **bd v1.3.2 + dolt v2.2.0.** v1.3.2 is the newest beads release (2026-10-24). Its CI installs exactly dolt 2.2.0 (`scripts/ci/install-dolt.sh`) and its test containers run `dolthub/dolt-sql-server:2.2.0`; dolt v2.6.1 (2026-10-26) is newer but not what this bd is tested with. The farm's bd 1.0.5 is a dev build with no release. bd embeds its Dolt engine, so neither zm mode runs the dolt CLI; it is provisioned with bd (first on the adapter's PATH) for the bd features that spawn one. |
| Where do they come from? | Release archives on github.com, one pin per OS/arch with the archive's sha256 in `internal/tools/pins.go`. The archive is hashed while downloading and refused on a mismatch before anything is extracted; only the named member is extracted, atomically, 0755, into `$ZM_HOME/tools/`. `tools/manifest.json` records the version, the archive sha256 and the binary's sha256; `Verify` re-hashes the binary, so a file swapped behind zm's back is refused (a process with the owner's file access could also rewrite the manifest; that is outside the model, like `receipt.key`). No sudo, no package manager, no shell installer. Both archives download in parallel (about 2-5 s each here). |
| Who downloads? | **The CLI**, before it calls the daemon: it can ask on a terminal (`--yes` skips the question, `--no-download` refuses) and show progress. Off a terminal (scripts, CI, agents) there is no one to ask, so it fetches and says so on stderr; such runs pass `--no-download` to refuse. One `Ensure` is all or nothing (staging files, installed and written to the manifest only when every tool passed); a second CLI on the same home is told at once that another zm is fetching and gives up after `tools.LockWait` (30 s) instead of blocking silently behind a stalled download. The daemon never downloads; a binding whose pinned bd is missing is `unavailable` with a hint. `--bin` runs the user's own bd. |
| How is the environment built? | From the adapter's own declaration (`describe.env`, 2026-11-01): the daemon (`adapter.Env`) gives the base, the declared variables and the declared secrets, per tracker, to the adapter process (one-shot calls and serve-mode processes, which the pool keys by executable + environment); the adapter builds its CLI's environment (beads: bd's, `internal/adapters/beads/env.go`). Before, the daemon hard-coded beads' and github's variables and secret names, so a new adapter needing one meant a daemon change. describe runs with the base (the daemon's for `env = "inherit"`: the reason to inherit is an adapter that needs a variable, and it must be able to describe itself); the describe cache keeps inherited and base answers apart. A tracker with `env = "inherit"` gets the daemon's environment (a `[[trackers]]` key, part of the mirror's binding fingerprint only when set, so existing mirrors keep theirs). |
| What pins embedded mode? | bd v1.3.2 decides the mode from `BEADS_DOLT_SERVER_MODE`/`..._SHARED_SERVER`/`..._HOST` (all excluded) and `metadata.json` `dolt_mode`; only its "owned" mode auto-starts a sql-server, and never with `BEADS_DOLT_AUTO_START=0`. So: that variable in the environment, and `add`/`test` check `dolt_mode: embedded`. `--dir` gets the variable too (an existing `.beads` in owned mode would otherwise have bd start a sql-server with the pinned dolt that nothing zm tracks stops; bd now fails loudly instead), and `add`/`test` note the directory's `dolt_mode`. `TestTrackerRealBDNew` runs the real pinned bd (init, list, claim, release, conformance `--write`) with the live farm's variables in the daemon's environment and finds no server files and no new sql-server process. |
| bd's side effects | `bd init` writes CLAUDE.md, AGENTS.md, `.claude/settings.json` hooks, Codex and Cursor setup into its directory unless `--skip-agents`, git hooks unless `--skip-hooks`, and warns about `beads.role` on every call unless git config has it: zm passes the flags and gives each tracker a HOME with a `.gitconfig` (user name and email from the user's git config, `beads.role = maintainer`). bd spawns a detached metrics uploader every few minutes, which would escape the adapter's process group: `BD_DISABLE_EVENT_FLUSH=1` and `BD_DISABLE_METRICS=1`. |
| The --server schema check | Read-only first contact: `bd --readonly sql --json "SELECT COALESCE(MAX(version),0) ... FROM schema_migrations"` (verified against a bd v1.0.4 schema-32 database on a scratch server: it neither migrates nor writes). bd v1.3.2 itself refuses to auto-migrate a shared server ("refusing to auto-apply 34 pending schema migrations"), but cannot read an older schema either (`table not found: leases`), so a mismatched read-only binding fails its checks and is rolled back with the hint; `--allow-migrate` runs `bd migrate schema` and checks again. A server newer than the pinned bd stays read-only. A `--bin` bd that is not the pinned version: zm does not know its schema, so it compares nothing and binds writable with a note (`schema.pinned` 0) rather than read-only with only `--allow-migrate`, which would migrate nothing, as a way out; the user picked that bd, usually to match the server. |
| Password | `--password-file` is a path; the daemon copies the file to `secrets/<project>/<prefix>.password` (0600 in 0700 dirs), so the password never travels on the socket, in project.toml, a route, `tracker ls` or `work sources`. It is read when the environment is built and masked in errors: by the adapter (`beads.failure`) and by the daemon (`redact`, any adapter error and conformance detail), whatever its length (a short password mangles the message it is masked in; leaking it would be worse). `redact` keeps an `*api.Error`'s type and any other error's identity for `errors.Is` (a cancelled call stays `context.Canceled`), with no `Unwrap`, so `errors.As` cannot reach the unmasked original. `tracker rm` deletes the copy. |
| bd 1.3 claims | bd v1.3.2 refuses `update --assignee ""` on an issue another actor holds ("cannot reassign ... held by"), which broke release (zm's bd actor is never the holder it assigned). release and set_status open fall back to `bd unclaim <id> --if-assignee <holder>` (a compare-and-swap on the holder zm recorded), then drop the review label; older bd (no unclaim) keeps the update path. |
| Concurrent adds | `add` reserves (project, prefix) in memory before any side effect and re-reads the project after reserving; a second `add` of the same prefix is the usual conflict at once. Before, both ran `bd init` into the same directory and the loser's rollback deleted the database the winner had bound. |
| Rollback | The binding is written, then conformance and the first full sync run at once (both list the tracker). A failing check removes the binding (if still this one), stops and wipes the mirror, closes the adapter process, and undoes what setup did (the database it made, the password it copied). A first sync longer than `--wait` (5 min) is not a failure: it goes on in the background. |
| Test fakes | The allowlist drops `ZM_FAKE_STATE`, so the fake bd/gh fall back to `state.json` next to themselves on PATH (where `fake.Install` and the bench put it). Test adapters that read test variables bind with `env = "inherit"`. |

**Bumping the pins** (rclone, cloudflared; the bd and dolt pins went on 2026-11-09 and 2026-11-10). 1) Read the
release's asset digests: `gh api repos/rclone/rclone/releases/tags/vX --jq '.assets[] | .name + " " + .digest'`
(cross-check rclone's `SHA256SUMS`), the same for `cloudflare/cloudflared`. 2) Edit `internal/tools/pins.go`
(versions, URLs, sha256, member paths: `unzip -l` or `tar tzf`). 3) Run `ZM_TEST_DOWNLOAD=1 ZM_TEST_TOOLS=$(mktemp
-d) go test ./internal/tools -run TestRealPins`: it downloads each for this platform, checks sha256 and runs
`--version`. 4) Installed homes see a manifest at the old version as missing: the next command that needs the
tool fetches it.

Measured (a reference M3, 2026-10-30): `zm tracker add beads` from nothing, downloads included, 14.4 s (bd 46 MB
and dolt 39 MB in parallel, 4.7 s; `bd init` about 4 s; checks and first sync 4.6 s; embedded bd serialises
concurrent processes); 9-10 s with the tools present; `--server` on a scratch dolt sql-server 1.2 s, with
`--allow-migrate` from schema v32 4.4 s.

## Tolerant bulk import (SPEC 17.5, round A stream A3; retired 2026-11-01)

**Retired 2026-11-01.** `zm tracker import beads` (a beads JSONL export, or a live tracker read with
`bd --readonly export`, into a zm-owned beads database through batched `bd import`) and `zm work import` (a beads
JSONL export into `local:` items) were removed, with `internal/jobs`, `zm jobs`, `/v0/jobs` and
`scripts/import-bench.sh`. Why: zm had three beads importers for one job, and after harbor went zm-native through
the third nobody used these two. The one that stays is `zm issues import <project> --dir <dolt database>`
(`internal/issues/beads.go`, also used by the native home move), and bd users keep their commands through `bd-zm`.
Kept as contract: project.db's `work_imports` table (migrations are append-only; nothing writes it) and the
`dev.zm.work.imported` event (deprecated: nothing publishes it; the team replica still applies it from older
logs). The mirror pause the beads import held (`mirrors.pause`) is still in the code with no holder.

What was learned, for anyone who imports through bd again: bd 1.3.2 stops at the first row it refuses (7,500 of
11,524 harbor rows in, then nothing; a re-run stops at the same row), so an import has to batch, name the
refused row from bd's "validation failed for issue X" and retry the rest. On the harbor export a batched import
took 11 min 49 s, a single `bd import` of the fixed export 9 min 33 s: bd's per-row cost grows with the database
(`CreateIssuesInTx`, `RecomputeIsBlocked`), not with the batching. The same export into SQLite `local:` items
took 1.9 s. The full design table and the measurements are in this file's history before 2026-11-01.

## Round A integration (17.1 + 17.2 + 17.5)

Retired 2026-11-01: the rows about the 17.5 importers (how the import reached bd, what "zm-owned" meant for an
import target, a live source, `tracker rm` during an import, events from `work import`) went with them; see
"Tolerant bulk import" above and this file's history.

| question | decision |
| --- | --- |
| Autostart vs `zm daemon` | Kept as A1 built it: an autostarted daemon drops `BEADS_*`/`BD_*`/`DOLT_*`, `zm daemon` keeps them. With the allowlist they now matter only to `env = "inherit"` trackers, which is exactly the dogfood case (a beads binding that reaches the farm server through the shell). Such a daemon must be started with `zm daemon` from that shell. |
| Proof that the shell never reaches bd | Unit: `TestTrackerAddNewIsLiveAndAllowlisted`. End to end: smoke starts its daemon with fake farm `BEADS_DOLT_SERVER_*` and `BEADS_DOLT_PASSWORD`, then `tracker add beads` and checks bd's recorded environment; the opt-in real-bd test `TestTrackerRealBDNew` runs the pinned bd with the farm variables in the daemon's environment and finds no sql-server started. `make test` (checked by `TestMakeTestUnsetsTrackerEnv` under the flag it sets), `scripts/smoke.sh` and `scripts/bench.sh` unset the shell's real `BEADS_*`/`BD_*`/`DOLT_*` first (smoke then sets only its fake farm values); a plain `go test` from a shell relies on the allowlist, and the opt-in real-bd tests clear or allowlist the environment themselves. |
| Shared code | A2 and A3 both defined `expandHome` (identical; one kept) and a test helper `fakeBD`. A3's init-order fallback that made a bare `tracker` command is gone. |
| A hand edit vs a live rewrite | Every live change to project.toml (`tracker add/rm`, `project set`, `area add/set`) first compares the file with the text the daemon last read or wrote, and refuses with a conflict naming the restart when they differ: a restart loads the edit, a live command never silently discards it. Refusing beats merging: a merge would have to guess which of two edits to the same tracker wins. |

## Teams by proxy (SPEC 17.3, 17.4, round B1)

Goal: a small team (an owner, a member, a guest of harbor/solver, the farm's agent boxes) on one
project with one auth door: the authority's tailnet listener and its tokens. Built in `internal/team` (routes,
proxy), `potluck/kit/project/team.go` (invites, redemption, node-bound tokens, scope_version), `internal/work/team.go`
(leases in project.db, team claims, write-back), `store` (the team pool) and `events` (commit-ordered ids).

| question | decision |
| --- | --- |
| Where is "team" recorded? | `projects.team` in registry.db (`project/0002_team`). `store.Open` reads it to open team files with the team pool; `Registry.SetTeam` flips it and reopens the file (old handles stay open until close, so requests holding them finish). A replica has no project row for a joined project, only `team_remotes`: round B keeps no copy. |
| One lease code path | The same `leases` table in every project.db (`work/0002_leases`); `takeTx` is the compare-and-set for both files; `leaseDB`/`leaseReadDB` pick the handle; cross-project queries (`queryLeasesAll`) walk registry.db and every team file, each asked only for its own kind of project, so rows a crashed move left behind are never served (and are dropped at start). |
| "In one transaction" across two files | Not possible (no cross-file commit). The move copies into the target, flips the mode flag (the commit point), deletes the source, all under the project's lease lock (every lease operation holds it shared, the ticker's bound-lease renewal included). Either side of the flag is consistent after a crash. `Registry.SetTeam` writes the registry row before it switches the file's pool, and puts the row back if the pool will not switch, so memory and registry.db never disagree. |
| The team pool | Writer: one connection, the lease/claim/event path. Readers: four connections, what `Store.Project` returns, so every module's reads (and, for now, mail's and comments' writes) run there. Making the single writer carry every module's writes risks a deadlock wherever code queries the handle inside its own transaction or open rows; moving those writes is round C work with write records. Readers still begin with IMMEDIATE, so their writes take the file's write lock and "one writer" is a convention until then. `busy_timeout` 200 ms on the writer (under the 250 ms claim budget: a claim behind a mail commit waits at most that, else fails `database is locked`); 5 s on the readers (B2 review: at 200 ms a mail commit failed behind any lease transaction longer than that; their writes are not on the claim budget). |
| Commit-ordered ids | `Bus.TeamTx(project, fn)`: BEGIN IMMEDIATE on the writer, fn's change, then the bus lock, mint, insert, commit, fan out. A plain `Publish` into a team project goes through it too, so every id in a team log is minted under its write lock. The lock order is always file, then bus; solo publishes take the bus lock and touch only solo files or registry.db. |
| Tracker items on a team project | The lease is the claim (no synchronous adapter call: the race is decided in SQLite, not in bd). Write-back runs from a bounded queue; an adapter conflict, or a later mirrored owner that differs (after a 30 s grace for in-flight write-back), releases with note `tracker conflict` and mails the holder. |
| Join code | `zm-<machine>:<fp>:XXXX-XXXX`; fp = sha256 of the stable node ID, so node-key rotation does not break a code; 40 bits of Crockford base32 with lookalike folding. Only its sha256 is stored. |
| Uniform refusal | One `403` for every code problem (wrong, used, expired, removed, tagged without `--allow-tagged`, over a rate limit, attempts exhausted). Refusals that do not depend on the code come first and say what they are (socket, unidentified node, this machine). A taken machine name is a distinct `409` only for a holder of a valid code. Limits: 5 wrong codes per node and 50 overall per 10 minutes (in memory), 10 attempts per invite (stored). |
| Idempotent per (code, node) | The token secret stays in the invite row (`pending_secret`, registry.db is 0600 in a 0700 home) until the token's first authenticated call, which clears it; retries before that get the same token, never a second one, and are not counted against the 10 attempts. **Deliberate trade-off**: for that window (normally the seconds between redemption and `zm join`'s access check) the secret of a working node-bound token sits in plaintext in the authority's registry.db, readable by anything running as the authority's OS user. It is only usable from the redeeming node (node-bound), so the exposure is a copy that already only works from that one machine; hashing it would make a lost answer burn the code. |
| Revoked outright | A grant revoked with no other grant in the project (token left alone) bumps scope_version like any rescope; the refreshed header then carries no role, and the replica stops: state `revoked`, `last_error` naming the authority, every call answered "access ended" with a hint, lists naming it in `skipped`, `GET /v0/projects` too. It keeps the token file so `zm leave` can still revoke the token at the authority (and a new `zm join` over it revokes it first). Without this the replica forwarded forever and every call came back as a missing project. |
| Node-bound tokens | `tokens.node` and `tokens.machine`; `Registry.FromTokenAt(secret, remote, node)` answers the uniform 401 unless the request is remote and WhoIs named that node, so the socket, an unmarked listener and the host's own Tailscale all fail. `FromToken` keeps its signature for the other modules. The subject carries `Node`, `Machine`, `ScopeVersion` and its `Cap`. |
| Rescope | `identities.scope_version`, bumped in the same transaction as every grant insert, change or delete. Checked in `core.Authenticate` when a node-bound token sends `Zm-Scope-Version`; the server middleware sets `Zm-Team` and `Zm-Scope-Version` on every answer to such a token (409s included), so the replica learns the new header from the refusal and retries once. |
| The machine name | A replica's machine name is its daemon's (`cfg.Machine`): its sessions are already `<machine>/<s8>`, and the authority accepts those only for the recorded name. Renaming addresses in flight (both ways, in bodies) was rejected as fragile. A taken `zm-<machine>` (Tailscale renamed this node) is refused with how to change the daemon's name. B2 review: the authority itself refuses its own name at redemption (a direct POST got past `zm join`'s client-side check and its token could act as every authority session); `withAddress` and mail's machine-human rule also ignore a token recording it. A name is held only while its node holds a live node-bound token (checked at redemption, so every revocation path frees it), because farm boxes are reimaged routinely and a new name would change every session address. |
| Join code names the node as the tailnet named it | `zm invite` reads the first label of the node's DNS name, and refuses when the tailnet renamed it (`zm-mini-1`): a code naming `zm-mini` would reach the other node and fail its fingerprint check, safely but with no hint to the owner. |
| Pinning | The replica's transport dials through its own node (`core.NetService.Dial`, never the host's network) and checks `Peer(conn.RemoteAddr)` against the pinned fingerprint on every new connection, before a byte of the request goes out. |
| Proxy routing | Explicit project: forward. Lists naming none: local + every authority, unreachable ones in `skipped` (sources get an `ok:false` row). Items naming none: local first, then authorities while 404, so hidden and missing stay identical end to end. Writes needing resolution: a recorded checkout or a repo file naming the joined slug, unless a local rule named a project explicitly. `Zm-Cwd` never leaves the machine. `GET /v0/projects` merges the joined projects so the CLI prints slugs (an authority that does not answer goes in its `skipped`). Bodies over 1 MiB are refused, never forwarded cut short. Only socket callers *without* a token are proxied: a `token mint` token on the socket acts in this machine's projects only (it is scoped to a local project, so it could not name the team project anyway); agents on a replica use the socket without a token, as they do today. Every proxied call logs a Debug line (method, path, project, authority) and the request line with `proxied=true`. |
| Replica sessions at the authority | No presence in B1, so the authority had no session rows for them. `core.Deps.Visible` shows a session on a joined replica machine to whole-project readers of a project that machine joined (area-only readers keep `someone`); since B2 that is only the fallback for an address with no presence row yet. |
| Test transport | `potluck/kit/tailnet/tailnettest`: fake nodes behind `tailnet.Node`, loopback listeners, WhoIs from the identities a test gives, a renamed duplicate hostname like Tailscale's. Two optional node interfaces keep the existing fakes compiling: `Dialer` (tsnet's `Dial`) and `HostChecker` (which callers are "this machine"; the loopback fake answers from its identities, since every loopback caller would otherwise count as the host). Never linked into `zm`. |

Measured (a reference M3, loopback, in-process clients on the replica's Unix socket, 200 runs): direct read at the
authority p50 0.31-0.33 / p95 0.57-0.64 ms; proxied `work ls` 0.37-0.43 / 0.67-0.89 ms (budget: round trip + 30
ms); proxied claim 0.35-0.37 / 0.65-0.68 ms (budget 50 ms); a CLI call adds ~8 ms of process start. Claim race: 4
replicas x 4 workers x 12 items, one winner each. `make bench`: every budget met, unchanged (hooks 8-10 ms p95,
reads 8-12 ms, tracker reads 10-25 ms, top 19.6 ms).

Verified after B1 (2026-10-31, real bd 1.3.2 + dolt from `tracker add beads --new`, scratch team authority): a
claim of a real bd item survived 106 s (past the 30 s grace and two lease ticks); bd showed `in_progress` with
assignee equal to the lease holder (`human:<user>`), and no `tracker conflict` release fired.

Not done in B1: cross-machine mail and presence (B2, below), the live event stream for team projects on a replica, bound
leases for replica sessions (`spawn --work` of a team item), 17.2's team-authority tracker rules, and everything of round C (copies, sync, snapshots, outbox, write records, stale reads).

Hedged forwarded reads (2026-11-01). A member Mac reading native issues through its replica from an authority
behind a home uplink saw rare multi-second reads (26 s once; bd sql 8 s about one in five under load 6-170).
Measured with read-only GETs on that Mac: about 1 in 150 reads of the 50-epic list (166 KB) waited 1.2-6.5 s, mostly
mid-body (first byte at 0.18 s, done at 6.5 s), while reads started during the wait answered in 0.13 s; small answers
(stats, 3 epics) never did, the local copy never did, and the slow ones were 12 s to 157 s apart (so not the
authority's once-a-minute token touch). The same code over loopback with 12k issues, writes and syncs and load 52 kept
every read under 0.25 s, and bd sql on the real 11.7k-issue store took 15-100 ms. So the time is one TCP connection in
retransmission backoff, not a lock, the store or GC. A read silent for 1 s goes again on a new connection (at most
twice); the first whole answer wins. Writes are never sent twice, and only routes known to be pure reads at the
authority are hedged (an explicit list: `GET /v0/mail/inbox` marks what it answers seen, so a second attempt would
lose announcements; `GET /v0/mail/inbox/peek` marks nothing and is hedged). A budget keeps hedges to about 10% of a minute's reads, so an authority slow for everyone is not
asked three times. bd sql waits 4 s before its answer starts (a copy
of `events` takes 1.7 s at the authority; hedging that only doubles the work). Not explained: FarmView's
parent-link `bd sql` (8,293 rows) took 7.7-8.1 s in 4 of 13 calls and 0.2-0.5 s otherwise; a fixed ~7.5 s does not look
like random backoff, and it did not reproduce (a 432 KB GET probed 178 times: max 2.4 s). A copy of native issues on the replica (the
`store.Feed` work) is what takes the network off the read path entirely.

## Mail and presence across machines (SPEC 17.3, round B2)

Goal: a team on one project with mail and "who is on what" crossing machines, without hooks ever waiting
on the tailnet. Built in `potluck/kit/teamlink/beat.go` (the heartbeat loop and route, with registered parts),
`internal/team/presence_beat.go` and `presence.go` (the sessions part both ways, the replica's cache, lost leases),
`internal/session/presence.go` (presence rows behind the session service), `internal/mail` (routing for team
projects, notice parts), the team proxy (mail and session reads) and `internal/work` (`HeldBy`, the holder's session
in `who`).

| question | decision |
| --- | --- |
| Where does presence live at the authority? | In the session module (`presence` in registry.db), not the team module: then `Lookup`, `Visible`, `Live` and `List` answer for replica sessions like local ones, and mail routing, redaction, `zm who`, the bound-lease ticker and the group mailbox need no special case. A separate table, not the `sessions` rows: those are keyed by session8 and carry PTY state only this machine has. The team module only checks and orchestrates (`core.PresenceStore`, `core.LeaseHolders`, `core.TeamMail`: optional interfaces next to the services, so fakes need no change). |
| What does a heartbeat carry? | Every live session of the project on the replica (address, area, agent, title, state, note, managed, the keys it saw the session claim). Sessions it leaves out are ended at the authority (`gone from presence`) and their bound leases released at once, as for a local session that ends. The replica is the authority on its own sessions. |
| Machine names | Every address in a heartbeat must be on the machine name the token was redeemed for (`403` otherwise), and mail from a replica is already limited to it by `Zm-As` (B1). A renamed tailnet node changes nothing. |
| Guests | A guest's session with no area is in its area; one in another area is answered like a missing area and not stored, so a heartbeat is no area oracle. Presence rows are scoped by their own (project, area), so a guest reading `GET /v0/sessions` (or a hidden address through mail and `who`) learns nothing outside its area. `machine:<m>` for another machine needs a whole-project reader (or that machine itself): otherwise a guest could probe which machines joined. |
| Stale presence | Computed when read: a live row whose heartbeat is older than 60 s is `idle` (note `presence stale`). The ticker's existing rule (renew bound leases only while `running`) then does the rest: no new timer, and an authority restart cannot forget a stale replica. |
| Ending presence | B2 review: nothing ended a departed replica's rows, so the authority's lists kept ghosts forever. `ReapPresence` (session module, `core.PresenceReaper`) ends live rows whose machine holds no live node-bound token for the project (`replica left`; called right after `team/leave`, a node-bound `token revoke` and `project solo`, and on the 15 s session tick, so any revocation path is covered) and rows whose heartbeat is older than `bound_lease_ttl` + 2 stale periods, at least 10 (`presence lost`): late enough that a bound lease the replica held has expired and escalated rather than been released quietly. `MachineProjects` is cached per machine and cleared on every node-bound token write, since `Visible` and the reaper ask it per remote address. |
| Bound leases for replica sessions | Fall out of `holderOf`: a claim by a session whose presence says `managed` is bound. Before its first heartbeat, a TTL lease (B1's behaviour). |
| Mail storage | The authority stores it, in the team project's file, like local mail; a replica fetches through the proxy (round B: no copy). `SetForwarder` stays unused: forwarding to replicas would make the replica a second store and break "one authoritative store per project". |
| The notice on a replica | Never proxied. Each heartbeat answer carries, per session, its unread team mail as notice parts (counts and redacted senders, computed by the authority for that session's scope), and the replica identity's own. The mail module adds the cached parts for the caller (`core.TeamReplica.TeamNotice`): a map lookup. The cost is staleness, up to one heartbeat (15 s); a proxied mail call (send, ack, reply) triggers a heartbeat at once, so your own actions show within about a second. Measured: the prompt hook with notice p95 0.2-0.3 ms in process, the same with the authority stopped. |
| Heartbeat cadence | Every 15 s, plus one soon after a local session event (subscribed on the bus) or a proxied mail call, batched for 100 ms and at most one a second (prompt hooks change state on every prompt). One heartbeat at a time per authority, answers applied in order, so an older answer never resurrects a key a newer one dropped. |
| Sessions outside the project (SPEC 9 "Across projects") | A session lives in one project (or `none`), but people run one agent across several projects, so mail lands in the sender's project and reaches the session there, under that project's authz. The authority could not tell a no-project session on a replica from a typo, so the heartbeat lists those addresses (`elsewhere`, addresses only) and the authority keeps them in memory: existence stays checked (a typo is still `not_found`), no presence row moves, and an older authority simply ignores the field. Who may address such a session is who may see it: whole-project readers, plus, for a guest's replica, readers of its areas (where its mail is filed); anyone else gets a missing session's answer. The replica's areas are read from its token at the send, so a downgrade counts at once; only live listings are reached. A reply skips the check: the parent proves the address. |
| Lost leases | The replica remembers which keys each session claimed through the proxy (from the claim's answer) and forgets the ones it released through it. A heartbeat answer listing the keys the session holds at the authority shows a loss (expired, force-released, a tracker conflict): `needs_human`, note `lease lost: <key>`, and the key leaves the held set, so later heartbeats do not attribute it to the session. In memory only: a restart forgets the set (the first answer is adopted). |
| needs=human from a replica session | The authority cannot set a state on a session it only knows by presence (the replica owns it and would overwrite it). The replica reads the proxied answer: needs=human mail sent by the session sets it `needs_human` locally (`waiting on mail <id>`), and a heartbeat answer saying none of its needs=human mail is open sets it `idle` (`answered`), mirroring the local rule. |
| Nudges | The authority cannot type into a replica's PTY. The replica nudges (the same fixed line) an idle managed session whose direct team mail count grew, and one idle managed session of the project when group mail grew. |
| Session events for team sessions on a replica | The project has no log on the replica, so they go to the machine log (project `none`, owner only): the replica's owner sees them, nobody else can subscribe to that log. |
| Sessions list on a replica | Merged with the authority's (which includes other replicas' presence and the authority's own sessions); the authority's copies of this machine's sessions are dropped, the local rows being the truth. |

Merged lists on a replica (B2 review): every authority is asked at once (`fanOut`) and the pinned transport bounds the
dial plus the fingerprint check at 2 s (`team.DialTimeout`); `CallTimeout` (30 s) still bounds the whole call once
connected. Tested over loopback with a fake node whose dials hang (`tailnettest.Net.Offline`): two offline
authorities cost `work ls` one dial timeout. Unmeasured on a real tailnet: how long a dial to an offline peer takes
to fail there, and whether 2 s is too short for a slow DERP-relayed first connection (it then shows in `skipped` and
the next list retries). Item routes naming no project still try authorities in turn.

`go test -race ./internal/work`: `TestMirrorPropagation` and `TestTrackerRmRefusedDuringImport` are timing-sensitive
under the race detector and pre-existing: run alone with `-race` twice each on main (e714cb7) and on this branch,
`TestTrackerRmRefusedDuringImport` fails identically on both (`tracker test` while paused takes ~14 s under the race
detector against its "at once" bound), and `TestMirrorPropagation` passes on both (it failed only inside a full
`-race` package run, i.e. under load). No DATA RACE report in either. `make check` (no `-race`) passes.

## Rehearsal: a three-machine morning on one Mac (round B2 review)

A stranger's walk-through with the real CLI against three daemons on the loopback tailnet (`scripts/loopnet`, added
for it: the docs offered `tailnettest` only to Go tests, so nobody could rehearse a team without real nodes).
Every CLI step, process start included, took 20-37 ms (join 27-36 ms, a proxied claim 21-28 ms, a merged `zm ls`
at the authority 22-37 ms); two replicas racing on one item gave one winner and one `conflict`. Found and fixed:

| finding | fix |
| --- | --- |
| A member mailing a guest's session from the repo root got "sent", and the guest's inbox stayed empty: the mail was filed at project level (the sender's resolved area), which the guest cannot read. | Mail to a session is filed in the session's area unless the sender named one or a `ref` did (SPEC 3, 9). `TestMailToGuestSessionFiledInItsArea`. |
| `zm revoke dot harbor/solver` cut the guest's access, but its session stayed in the authority's `zm ls` (with a stale held key) until `presence lost`, an hour later; only `zm token revoke` ended it. | A node-bound token whose identity has no grant left in the project no longer counts as joined (`MachineProjects`), and the grant revoke reaps presence at once (note `replica left`). `TestRescopeAndPurge`. |
| `zm project why` on a replica said "no project: writes from here are refused" in a checkout where `zm work new` worked (the proxy's write rule). | The proxy answers `resolve` with rule `team` when no local rule matched. |

Still friction, not fixed: a guest joining from a checkout gets "no checkout recorded" (its area has no remote;
documented: `--path`, or `zm project use`); the claim conflict's hint names `zm release --force` "on this machine",
which a replica refuses; `zm leave` after the owner revoked the token says "the authority did not answer" (it
answered 401); after `zm leave` the replica's own session rows show under the project's ULID; `-P harbor/<area>`
on a list silently ignores an area the caller cannot see or that does not exist (both answer alike, so nothing
leaks); `zm project team` always suggests `zm tailnet up`, even when the node is up; `zm status` without the
daemon's full health shows `pid 0`; inside a Claude Code terminal the CLI's default `Zm-As` names the host's
real session, which another `ZM_HOME`'s daemon refuses (`ZM_AS=` or unsetting `CLAUDE_CODE_SESSION_ID` fixes it).

Not done in B2: the authority's live event stream on a replica (`zm top` there reloads every 15 s and on its own
events), `spawn --work` of a team item, presence for a replica session registered in no project, a minted token on a
replica's socket reaching a team project, and round C. Not verified: a real tailnet (loopback only, as in B1).

## Daemon gaps a native cockpit found (G1-G7; zmux, retired 2026-11-05)

| gap | decision |
| --- | --- |
| G1 read-only attach | `attach?input=0` (and `zm attach --read-only`). The viewer's bytes are still read, so a hang-up is noticed, then dropped; it never calls `humanInput`; and it sits in its own `proc.viewers` set, not `conns`, so the cursor-reply rule ("answer `ESC[6n` while nobody is attached") still fires. `closeConns` closes both sets when the session ends. A viewer does not resize (the CLI skips its SIGWINCH wiggle). Still `operate` and `local`: watching a PTY shows everything the agent prints. |
| G2 ask mail | The mail module asks `mail.AskChecker` (approvals' `OpenAsk`: an `asks` row with no decision), wired by the server with `SetAsks`, in the **HTTP** ack and reply handlers only, after the visibility, write and party checks (a caller who cannot see the mail still gets `not_found`). The approvals module answers in process through `core.MailService.Reply`, which does not check, so the decision is the only thing that takes the mail. A setter, not a `core.Deps` field, because only the mail module needs it (like `SetForwarder`). An unwired checker means no check (mail unit tests); the server always wires it, and `TestAsksScope` plus the leak rows prove it. Not a mail column: an ask whose decision was recorded but whose answer failed (`mail_error`) must become answerable again, and asks made before this change are covered. The ask row is inserted before its mail is sent, under a caller-minted id (`SendMailRequest.ID`, `json:"-"`, in-process only), so there is no window where a human caller can take the mail before the row exists (`TestAskRecordedBeforeItsMail`); a failed send deletes the row. |
| G3 status events | `setLocalStatusIf` reports whether it moved the row; claim, release, expiry and the ticker's repair publish `work.status_changed` (the item as `set_status` publishes it) only then. A tracker item publishes the adapter's answer on every claim, release and expiry (a renewal only when it differs from the mirror): comparing with the mirror missed moves when the row was stale or missing (`TestTrackerStatusEventsIgnoreMirror`), and a repeated status is harmless to a reducer. Published after the lease event, so a reducer can apply both in order. Touches `internal/work` (not this stream's tree): one helper and four call sites. |
| G4 ask event | `dev.zm.ask.created`, data the Ask, published after the ask row is recorded (so `mail.sent` comes first) with decision.recorded's audience (operate, or asker/recipient), stored, so replays apply it. |
| G5 live cursor | The handler subscribes, then mints the live-only cursor (minting first left an event published in between off the stream, above the cursor: a silent gap; `TestSSELiveCursorAfterSubscribe`), and writes it as `: cursor <id>` after the banner. A live-only stream does not drop channel events below the cursor (only a replay's own events are deduplicated), so an event published while it started is delivered live. SSE comments are ignored by readers that do not know it (`ReadSSE` included); `client.Events` needs no change (its resume already uses the overflow cursor). |
| G6 session paging | `?limit=&before=<next>`; order `updated DESC, id DESC` so ties never drop a row; `next` (`<ms>-<id>`) only when more rows follow (the query reads `limit+1` rows), so an exactly full last page costs no extra empty request. The state sort stays within a page. |
| G7 escalating stop | `escalate: true` runs the CLI's plan in the daemon (TERM 3 s, HUP 1 s, KILL 3 s with `force`; each wait ends with the session); `Zm-Stop-Signals` names what was sent, so a client can say "ended after HUP (TERM ignored)". The CLI keeps its own client-side loop (it predates the flag and works against any v0 daemon). |

## Tracker homes (SPEC 17.9; Dolt homes retired 2026-11-10)

- **What went, and why.** Rounds 1 and 2 (2026-10-31, 2026-11-01) moved beads trackers between Dolt servers with a
  verified live cutover: a freeze in `dolt_branch_control` plus a privilege belt (dolt 2.2.0 ignored `read_only`),
  `dolt_backup` copies, fingerprints, an intent log with `--abort` and `--rollback`, zm's own dolt server locally and
  on ssh boxes. Every tracker moved to native issues, so on 2026-11-10 (no outside users: delete,
  git is the undo) the moves, restores, `serve`, `env`, the `dolt` backup kind, the pinned dolt and the beads import
  went. Git history before that date has the code and the reasons.
- **What stayed.** Backups (sqlite and export), `zm issues restore`, the off-site sets and the heartbeat. home.json
  keeps its name and its `backups`; an old one's `admin`, `move` and `previous` are ignored and dropped on the next
  write (`TestLiveHomeJSON`). `home.Manager` (launchd, systemd, a process) stayed because `zm tunnel` runs
  cloudflared through it.
- **Why backups have their own ticker (not a jobs runner).** One job at a time would queue an hourly backup behind
  any long job; the ticker runs each tracker's due backups in its own goroutine, one at a time per tracker.

## Native issues (SPEC "Native issues", phase 1 core, built early)

- **Why a brick per project file.** issues.db sits next to project.db but is its own file with its own pool (one
  writer, four readers, one checkpointer): the Store interface has to fit Postgres and Dolt too, so the driver owns
  its handles (`potluck/kit/store.OpenFile` gives it the same pragmas and statement cache). Writes are one funnel,
  `Apply(Mutation)`, with authz as a callback inside the transaction, so a driver never learns zm's roles and a write
  cannot check one area and land in another.
- **History doubles as the changelog.** ULIDs minted under the single writer are in commit order, and each row
  carries `area_before`/`area_after`: round C can serve `Changes(since)` without a second log. Per-field versions
  (`versions` on the row) make `base` cheap: only a field changed after the caller's copy refuses.
- **Events carry no issue references.** The golden leak test caught the first cut: a `dep_added` change published in
  solver named the billing issue it now depended on. Events now leave out edges, the parent and reference fields;
  the history route shows a reference the reader cannot see as `someone`. One known, accepted signal: `blocked` is
  computed from every blocker, so a reader sees that an issue waits on something even when the blocker is hidden.
- **Guests are read-only on issues** (the lead's ruling for this module; SPEC section 4 lets guests write work).
  One function (`canWrite`) holds the rule.
- **Import read Dolt with the CLI (retired 2026-11-10 with the Dolt homes).** No MySQL driver (no new
  dependencies), and the importer never touched a live server: `dolt sql -r json` on a restored backup copy, SELECT
  only. A fresh import refused unless every table's row count matched.
- **Speed work, in the order it mattered** (11,557 issues, 297 MB file, history is 160 MB of beads' event payloads):
  1. *Counts*: the first list ran a `count(*)` per page over the wide rows (30 ms). Lists now say `more` and count
     only on request (`count=1`, used by `bd ready`'s footer).
  2. *Partial covering indexes* for the default list (`status NOT IN ('closed','pinned')`) and ready work: a page
     or a count never reads the wide rows (30 ms -> 1 ms in C sqlite).
  3. *Search without ranking everything*: bm25 over 2,500 title matches and 6,700 body matches of "call" cost 25 ms
     in modernc. Now: title matches first (ranked only when fewer than 300), then body matches in list order; with
     many matches the query scans the ordered index and tests FTS membership (`+i.rowid IN (...)`) instead of
     fetching and sorting every match. FTS5 `prefix='2 3'` for search-as-you-type.
  4. *Column order*: short columns first, long text last, so a scan testing `external_ref` or `status` never walks a
     description's overflow pages; bd search's substring scan is index-only (`issues_status` covers title and
     external ref): 10 ms -> 3.5 ms p50.
  5. *Writes*: p50 was 1-3 ms but p95 150-300 ms right after an import. Two causes: the planner had no statistics
     (the file was opened empty; now `ANALYZE` ends every import) and commits paid for WAL checkpoints (now
     `wal_autocheckpoint=0` on the writer and a PASSIVE checkpoint every 500 ms on a third connection).
- **Numbers** (2026-10-31, M3 16 GB shared with other agents, load average 9-11; `TestRealBeadsImport` on a fresh
  import, 300 calls each over the daemon's Unix socket, client to decoded JSON; the member rows use a token for one
  label-following area, the rest the owner):

| call | p50 | p95 | p99 |
| --- | --- | --- | --- |
| list (default 50, labels, counts) | 1.4 ms | 2.5 ms | 4.3 ms |
| list with each issue's edges | 1.5 ms | 2.4 ms | 3.2 ms |
| ready (100, edges, exact total) | 3.6 ms | 5.6 ms | 7.5 ms |
| list, area member (every status) | 4.8 ms | 5.8 ms | 6.4 ms |
| full-text search, area member | 3.9 ms | 12.1 ms | 16.7 ms |
| show (edges both ways, comments) | 0.5 ms | 1.4 ms | 3.2 ms |
| full-text search | 3.0 ms | 5.0 ms | 6.6 ms |
| bd search (substring) | 2.9 ms | 4.7 ms | 6.7 ms |
| create | 1.5 ms | 2.6 ms | 3.6 ms |
| update | 0.4 ms | 1.1 ms | 2.1 ms |
| comment | 0.4 ms | 0.9 ms | 2.5 ms |
| claim + close | 1.1 ms | 2.0 ms | 3.4 ms |

  At load 13-18 (earlier runs, same schema) every p95 stayed under 16 ms too (worst: ready 11 ms, bd search 9.7 ms).
  The 16 ms assertion runs only with `ZM_BEADS_COPY` set, so it guards releases, not CI.
  Import of the backup copy: 11,557 issues, 14,923 comments, 12,120 dependencies, 12,606 labels, 68,978 events,
  593 child counters, 47 config rows, every count verified, 1.4 min (most of it Dolt's JSON output and FTS
  indexing); reported, not dropped: 2 issues with several parents (every edge kept).
- **Shim fidelity, checked against the live bd** (read-only): `show` on 5 ids, `list --all` (11,557 common ids),
  `ready -n 0` (1,867), `children`, `search -n 0`, `list --label`: identical keys in identical order for every
  common id; the only value differences (18 in `list --all`) were issues edited on the live server after the backup.
  Dependency order follows beads' row ids, which the importer keeps.
- **Review (fresh-context fable reviewer) and dispositions.** Blocking: `close` named hidden blockers in its
  refusal (fixed: it names visible ones and counts the rest; test). Major, all fixed: the shim's lists spanned
  every readable project (now the resolved project; ids still resolve anywhere); `--db`/`BEADS_DB`/`--global`/
  `--readonly` were ignored (now refused); re-import clobbered native edits and a failed import half-wrote the store
  and disabled verification (import now runs once into an empty store, deletes what it wrote on failure, clears a
  crashed one, and always verifies); no CI test for the importer (fake `RowSource` tests: every column kind, deps,
  history, counters, failure, bad times); unknown `dep`/`label` subcommands exited 0 (now errors). Minor, fixed:
  unparseable times, dangling labels in the counts, empty dep targets, the writer's no-autocheckpoint pragma now on
  every connection (DSN), stats' ready = what `ready` lists, merged-list sorts, `?parent=` of a hidden issue (no
  oracle), child-id lookup by key range, N+1 parent visibility, small shim deltas (reopen --json, --reason-file,
  comments add extras). Accepted: `create --id` reports a collision with a hidden id (refusing a duplicate cannot
  avoid it); `blocked` counts hidden blockers; event old/new NULL and "" both import as null; the hash-id length
  counts top-level issues on each create (~1 ms at 11.5k); exit 3 when the daemon is down (zm's convention).
- **Numbers (follow-up, feat/issues-graphql).** The Linear SDK's `Issue` fragment selected `number`, so native
  issues got a per-project sequence (`max(number)+1` inside the create transaction, unique index; migration 0002
  numbers existing rows by `created_at, id`). The GraphQL door that asked for it was removed on 2026-11-09
  (nothing called it); the number stays for bd `--json` and `GET /v0/issues?number=`.
- **Native issues as a work source: an in-process adapter, not a new kind of source.** The work module already
  serves trackers from a mirror fed by section 11 verbs, with leases, inflight, MCP and the UI on top. A binding with
  adapter `native` is answered by the issues module through `core.Deps.Issues` (no executable; conformance runs
  against it in process), so none of that changes. Reads see every issue (the work module scopes its mirror); writes
  run as the request's caller, so native rules hold through `zm work` too (a guest's `zm work claim` on a native issue
  is refused, tested). The mirror's `changes` feed is the issue history, so an incremental sync is "rows after this
  id". The move from a beads Dolt home into native issues (`--set zm-native`, its freeze and privilege belt, the
  import, rollback) did its job for every tracker and was retired on 2026-11-10 (see "Tracker homes"); a binding's
  prefix still names its work ids (`bd:harbor-abc12`).

## Local-first replicas (SPEC 17.6, 17.7, round C)

Goal: a teammate's zm keeps working when the authority is unreachable, at the speed bar (no tracker or tailnet
call on a read, a hook or a comment). Built in `internal/team` (`replica.go` the copy, `syncloop.go` snapshot, sync,
drain and refresh loops, `outbox.go`, `serve.go` reads and writes from the copy, `authsync.go` the authority's
routes), `potluck/kit/events/changes.go` (the changelog), `work/snapshot.go` and `mail/snapshot.go`.

| question | decision |
| --- | --- |
| What is the changelog? | The team project's event log, unchanged: ids already commit in order (17.3), events already carry whole rows (items without links, comments, leases, mail). No second change table. `Bus.Changes` filters per reader with the bus's own read, audience and redaction rules. |
| Cursors and gaps | The cursor only moves to an id the reader was sent; with nothing visible it stays put. A guest therefore never learns a hidden event's id (a ULID carries its time). The snapshot's cursor is `LastVisible`, for the same reason. Cost: a guest with nothing visible rescans from its old cursor (indexed, bounded by the log). |
| 410 | The prune records `pruned_upto` (events_meta); only a cursor below it is refused. Before, nothing could tell "older than the log" from "the log was never cut". |
| Area transitions | Computed at read time from the log: a work event a reader cannot see whose previous work event about the subject it could see is a tombstone. No `area_before` column; needs the new `events (subject, id)` index. Misses an item whose earlier events were pruned or that came only by snapshot: it stays in a guest's copy until its next snapshot (documented in 17.6). |
| Snapshot consistency | Cursor first, then the rows (not one read transaction: the rows come from the work and mail modules' own queries through `core.Snapshotter`). Anything committed after the cursor has a larger id, so sync sends it again; every apply is an idempotent upsert, so twice is harmless. |
| Why item events are now rendered under the write lock | The load test left a replica on `claimed` while the authority said `in_review`: work published item events after the commit, rendering the item, then minting the id, so a concurrent status change could mint between and the older render landed last in the log. Events are the replica's only source of row state, so each item event now reads the row (and its lease holder) inside the `TeamTx` that mints it. Reads go through that transaction (the writer's one connection): using the reader pool there could stall behind readers waiting on the same write lock. |
| Row and event in one transaction | A later load-test run (with the authority restart) ended with a replica one comment short, for good: comments and mail committed their row on a reader connection and published the event in a second transaction, the gap 17.3 left for round C. A restart between the two keeps the row and loses the event, and a replica learns only from events (a snapshot would repair it, but nothing asks for one). Comment, create, link (work) and send, orphan copy, ack (mail) now commit row and event in one `TeamTx` in team projects; solo projects keep the old path (nothing replicates them). |
| The bus deadlock | Also found by the load test (authority hung, hundreds of sync calls waiting on the bus lock): every 500th team event pruned the log on the writer from `fanout`, under the bus lock, while another `TeamTx` held the writer and waited for the bus lock. Latent since B1 (the old prune did the same). The prune now runs inside the committing `TeamTx`. |
| Idempotency | Two layers. Write records `(identity, write id) -> answer` make a replayed batch answer what it answered (a claim, release, status or ack never runs twice). Under them, comments and mail are stored under the write id, so even a write that ran but whose record was lost (a crash between the two) stores nothing twice. One batch per (identity, project) at a time, so a replica that gave up waiting and resent cannot race its own first run. |
| Order | One drain per authority, strictly by outbox sequence, batches of 100. The authority runs a batch in order and stops at the first "not now" without recording it, so nothing after it runs first. Refusals (conflict, not found, forbidden) stand and are recorded; they are never retried. |
| Stale base | `base` = the newest change id the copy applied for the item; refused when the item's last `status_changed` is newer, checked by the work module inside the `TeamTx` that changes the row and mints the event (a first cut checked in the team module before dispatch: a direct status change whose row had committed but whose event was not minted yet slipped past; review finding). A replica's own earlier writes must not count against it (claim, then set status at once, is the common flow), but nothing else may: only the status event a replica's own write minted (`status_seen`, reported through a context sink the work module fills from `TeamTx`'s done callback) raises a later status write's base, in the batch and in the outbox. The first cut used the item's newest event of any kind, so a queued comment ahead of a queued status write hid another agent's status change (review finding, now `TestWritesReplayAndStaleBase`). A tracker claim's write-back runs after the claim answers, so its move to claimed post-dates the claimer's base: a 2 s wait for it in the claim lost to a slow tracker or an earlier FIFO job, so the write-back's status event now names the lease it was for (owner-only `writeback_for`) and the check skips the writer's own live lease's, whatever the timing . Marking the event, not raising the base, keeps another writer's change between the base and the write-back refused. |
| Read your writes | The authority's answer to an acked write goes into the copy before the caller gets it (then the change's own event re-applies it). Two guards: never when the copy's base already passed `seen` (the answer would be older than the copy), and a claim's answer only as a lease (its item says `claimed` as a view even when the row stays `in_review`; the load test caught a copy that took it as the row). Mail never reopens once taken. |
| Which writes wait | Comments, status and links are optimistic (an agent's progress notes must never wait on the tailnet). Claims, releases and leases wait (17.6: a compare-and-set at the authority), and so do mail, replies and acks while the authority answers, because their answer carries routing (recipient, area, "taken first") the sender needs and the replica cannot compute. Unreachable: everything queues and answers at once. |
| Queued claims | The brief asked for them, overriding 17.3's "never queued offline": a queued claim answers `202` (the client turns it into an error, exit 4) so no caller mistakes it for a lease. A lost one is kept as `conflict` and makes the session `needs_human`; an online claim's 409 goes to its caller and is not kept (the waiter hand-off is exact: the answer is sent under the waiter lock, and a caller that stops waiting checks once more). |
| Rescope and revocation without calls | Reads no longer meet the authority, so the long-poll does the checking: a waiting `GET /v0/sync` looks at the caller's scope_version and token every 500 ms and answers `409 rescope` or `401`. Narrowing reaches a replica in about a second, then a snapshot drops what left. |
| Shutdown | A waiting sync call ends when the daemon stops (it held shutdown for up to 20 s; found by the load test's authority restart). |
| Reads that still ask the authority | `zm who` (presence is not in the copy), `zm ls` (sessions, merged as in B2) and `POST /v0/work`. Each skips an authority the replica knows is unreachable instead of waiting on the dial. |

Review (fresh context, model fable) of the data-loss and ordering paths: ten findings, all checked against the code.
Fixed: the base masking and the check outside the transaction (above); a background fetch overwriting a newer row
(`putWhole` keeps the row when its base moved past the fetch, taking only links and comments); a scope change seen
while a snapshot was in flight being cleared by it (resnap is a generation: a snapshot clears only the changes seen
before it was asked for, so a guest's copy cannot keep old-scope rows); a long poll whose bus subscription was cut
off spinning for the rest of its wait (the waiter drains and re-subscribes); a queued status write reading its base
outside the transaction that queues it; a create's answer overwriting a row sync had already brought; a batch cut
off by the replica hanging up (it now runs to its end, so a write is never left run but unrecorded); a batch the
authority refuses as a whole retrying silently (it shows in the remote's `last_error`). Not fixed: a quiet guest's
cursor stays on the last id it was sent, so after a prune passes it the guest resyncs although it missed nothing:
advancing it to hidden ids would hand a guest the times of events outside its area (17.6 "gaps reveal nothing"),
and a spurious snapshot every 30 days is the cheaper side.

Found by the load test and its profile, besides the two bugs in the table: the outbox GC ran after every sync page
over every acked row with correlated lookups, and acked acks matched no rule but the 5-minute timer, so it grew into
a quarter of the run's CPU (now at most once a second, from the acked rows with point lookups, acks collectable
once the copy shows the mail taken); and a claim waited for every write queued behind it in its batch (a batch now
ends at the first write a caller is waiting on, order unchanged). Claim p95 over a 3-minute run went from 71 ms to
11.5 ms, the notice from 9.3 to 2.3 ms.

Load test (`make loadtest`, 2026-11-01, a 16 GB M3; every daemon, agent and client in one process, so the
authority shares the CPU with five replicas and 40 agents; in-process clients on each replica's real Unix socket, a
CLI call adds its ~8 ms process start): authority + 5 replicas x 8 agents, 40 contested items, 10 minutes, one replica
cut off for 30 s at 3 min, the authority restarted at 6 min. 200,500 timed operations (about 330 a second).

| operation (on a replica's socket) | n | p50 ms | p95 ms | p99 ms | max ms |
| --- | --- | --- | --- | --- | --- |
| ack | 12343 | 7.74 | 55.14 | 143.94 | 5000.19 |
| claim | 28072 | 3.32 | 37.92 | 103.86 | 5001.29 |
| comment | 52188 | 0.29 | 4.28 | 13.31 | 254.66 |
| inbox | 12393 | 1.88 | 8.29 | 17.30 | 234.59 |
| notice | 7395 | 1.78 | 8.27 | 16.25 | 144.86 |
| presence | 9823 | 0.41 | 2.00 | 6.08 | 65.55 |
| propagation (replica to replica) | 2136 | 11.72 | 67.82 | 190.63 | 426.07 |
| release | 27464 | 5.20 | 43.52 | 113.56 | 5000.15 |
| send | 22287 | 2.40 | 38.10 | 108.07 | 5000.44 |
| status | 9276 | 0.49 | 3.73 | 9.79 | 118.51 |
| work ls | 9638 | 0.64 | 2.62 | 8.03 | 213.16 |
| work show | 9638 | 3.80 | 12.84 | 25.24 | 95.61 |

- No lost or duplicated write: 52,188 comments and 22,287 mails agents were given ids for (200 or queued), each at
  the authority exactly once under that id. No double claim: 37,007 claims tried, 27,583 in the authority's log, 8,935
  conflicts answered at once, 608 queued (during the cut and the restart), 489 of those lost and kept as conflicts.
- Every outbox drained by the time load stopped; every copy equalled the authority 1.4 s later (41 items, 52,722
  comments, 22,287 mails, owners and leases).
- Against 17.7's budgets (p95): replica reads 2.6-12.8 ms (budget 50); a claim 37.9 ms (budget 50 on loopback); a
  change on every other replica 68 ms (budget 1 s; probes during the cut and the restart are not counted); the
  notice 8.3 ms in process (hooks 15 ms with process start; `make bench`'s hook rows are unchanged, 9.5-9.9 ms p95).
  The p95s of the other rows include the cut and the restart; the 5.0 s maxima are writes that waited the full
  ClaimWait while the authority went away and were then answered `queued`.
- A 3-minute run without the cut and restart: claim p50 2.7 / p95 11.5 ms, notice 0.8 / 2.3 ms, propagation 6.4 /
  15.5 ms. The 10-minute numbers are higher with 3x the rows and the shared CPU.

## Lease-backed native claims (SPEC "Native issues": claim)

- **One lease system, not two.** A native claim does not grow its own deadline: when the project binds its native
  issues, the claim door calls the work module (`core.WorkService.ClaimItem`), which takes the section 10 lease and
  then the native claim through the adapter, so TTL, renewal, bound leases, team projects (round C's TeamTx path),
  expiry and escalation are the ones every tracker item has. Writes the work module makes through the adapter are
  marked (`fromWork`) so the hook below never re-enters it (no recursive lock).
- **Ending a claim from the native side.** A native write that touches status, assignee or `claimed_by` drops the
  item's lease unless the address that still claims it holds it (`EndItemLease`); a write touching none of them
  costs nothing. The native release verb now changes nothing once the claim has ended, so a lease leaving after a
  close or a reopen keeps bd's semantics (a closed issue keeps its assignee; `status open` keeps it too).
- **Found on the way.** `zm tracker add native` failed on a project with no issues: an empty store's full listing
  had no cursor (refused by conformance). It now ends at the lowest ULID, and a missing store answers a bad cursor
  as `bad_request`. A claim right after `bd create` found no mirror row (sync every 5 s); `ClaimItem` reads it
  through first (only for a caller who sees the project).
- **From the review.** `fromWork` first marked every adapter verb, so `zm work status done` on a native claim left
  the lease live; only claim and release are marked now (the work module handles the lease around those two). In
  a team project the source's claim is a write-back, so the native door made no claim and the write-back ran as
  the system (assignee: the machine); the door now claims at once as the claimer, and a system call names the
  request's `actor`. A renewal the source refuses (closed meanwhile) used to keep its lease; it drops it now
  (transient failures keep it). Claims made before a binding are adopted as TTL leases at bind (and, until
  2026-11-01, at every start).
  A repeat claim by the claiming address is idempotent
  whatever name it came under (the team write-back of a claim the door made). Adoption leaves expired lease rows to
  the ticker. Not tested: the team-mode native door (no team fixture in the issues tests).

- **Hook sessions' claims are bound (2026-11-01).** A hook session's claim was a 1 h TTL lease that nothing renews
  (claims renew by claiming again, never from hooks), so every agent outside the cockpit lost its claims an hour in: on the
  first day with the authority off the Mac, four claims on one session reset to open at claim + 1 h + the sweep.
  Binding them to the session (like a managed session's) reuses the ticker that already renews from liveness, and on
  a team authority that liveness is the presence it already receives: no new route, no renewal call, nothing for
  the hedged-read path to send twice. Hooks still write nothing. The risk of binding is renewing a dead agent for
  ever, so a running hook session that no hook reached for a bound TTL goes idle (an in-memory last-hook time, not a
  write per hook); `zm hook` registers no pid, so the session-end hook (Codex's too, now installed) and that idle
  rule are what end a dead agent's hold. An explicit `ttl_seconds` stays a TTL lease, renewals included (a short
  lock means short), recorded on the lease (`ttl_set`) so the ticker binds only leases whose TTL was not named. A
  lookup error keeps the lease for the next tick (an error is not an ended session). A member's session registered
  in another project rides the team project's heartbeat while it holds keys there, or the authority would never
  know it. Codex reported only prompts and turn ends, so a long Codex turn looked silent: `zm install codex` adds
  its tool and session-end hooks (they need trusting again in Codex's /hooks).
  Follow-ups: the replica's held-keys cache is per project (one project's heartbeat answer had read another's keys
  as lost, which set the session needs_human), and a session is in one heartbeat per authority (the authority's one
  presence row had moved between projects on every beat); a session whose own project is joined at another authority
  is still present where it claimed. Codex's hook `session_id` is the root thread's id, not the subagent's: hooks
  register by `agent_id` first so a thread's hooks, notify and tools name one session. A row moving between projects
  must not pass through ended: when a session released its last key in its row's project, that project's next
  heartbeat left it out before the other's had moved the row, the authority ended it ("gone from presence") and
  released its bound leases in the other project. The other heartbeats now name it `elsewhere`. Codex's SubagentStop
  (installed) ends a subagent turn, not its thread, so it means idle, never ended: ending it dropped the subagent's
  claims mid-task. A machine leaving one project (zm leave, a revoked token, the project gone solo) ended its sessions
  there, and ending releases a session's bound leases in every project; a session that holds leases in another
  project of the same authority now has its row moved there and only the left project's bound leases released.

## Native issues through a replica (SPEC "Native issues": "On a team replica", 2026-11-01)

Moving zm's authority off a laptop exposed that `bd` could not follow it: native issues had no replica path, so the
first plan gave every client `ZM_HOST` and a token. That puts a host in env files and scripts, and it makes every claim
a shared human's (`Zm-As` must be the token's own address), with TTL leases only. Clients should not know where the
data lives, so the replica forwards instead.

| decision | why |
| --- | --- |
| Forward, no copy | A copy of native issues is a real replica (the `store.Feed` hook, still to build). Forwarding is about ten lines on the rules the proxy already has, and every check stays at the authority. It adds the network, so it does not meet the 16 ms bar from a replica |
| An away authority is an error | With no copy, an empty answer reads as "no work" (a spike printed `0`, exit 0). `bd` names the skipped project so the call fails saying why, and an id that no reachable store knows is `unavailable`, not `not_found` |
| Always ask, ignore the reachable flag | The flag is false until the first sync answers, up to a long poll after a replica starts. The work routes have a copy to serve meanwhile; issues would just fail. A dead authority costs a 2 s dial instead |
| A dial waits for this daemon's own node | A real tsnet node takes seconds to come up, and `bd` right after a daemon restart would exit 3 for nothing. For 15 s after start a dial waits until the node runs. Nothing has been sent, so writes are safe, and a running node never waits |
| A merged list has no `next` | Keyset cursors are per store. bd names its project first, so only a caller that names none gets the merge |
| Native claims need no held-key plumbing | The heartbeat's answer already teaches the replica what the session holds (`issue:<id>`), so a lost native lease is reported. A session's own close or status change must drop the key, or the next heartbeat calls it lost. A heartbeat already in flight can bring the key back, so the replica also remembers what the session ended until an answer stops listing it |

## Comment edit and delete (identifiers design, stage 2a)

- **Author = identity.** Comments now record the author's identity ULID, so any session of an agent edits what
  another of its sessions wrote. Comments made before keep matching by address.
- **Tombstones, not deletes.** A deleted comment keeps its row and history; readers see `deleted` with no text and
  no deleter, the area's moderators see both. A moderator's act shows readers "a moderator" in history and events,
  so a community can moderate without singling out who did it.
- **Left for stage 2b (redact).** Purging text from every copy: the work module's copies of comments sent through
  `zm work comment` and its `dev.zm.work.commented` events, the webhook queue, team write records, replicas, the
  redaction list outside the store, backups, the importer.

## Comment redaction (identifiers design, stage 2b, part 1)

- **Scrub by content, not by id.** A comment's text has copies with unrelated ids (`zm work comment` writes the
  native comment and a work comment with another id, plus a work event). Every module scrubs its own tables for
  the redacted texts through one hook (`core.Scrub`): scoped rows (filed under the issue) for any text, and
  unscoped rows (any event, the webhook queue, team write records) only for texts of 8 bytes or more, so a short
  text cannot wipe unrelated content. No module knows another's tables.
- **SQLite keeps what you overwrite.** Overwritten rows survive in free pages and WAL frames; without compaction
  the end-to-end test found both secrets still in issues.db. Redaction vacuums the issue store, the project
  database and the registry and truncates their WALs. Redactions are rare, so the vacuum's cost is acceptable.
- **The list outlives the store.** registry.db keeps (project, comment id, issue, who, when); an import into a
  reset store redacts the listed comments again (tested: without it the original text came back).
- **Found on the way.** `zm work comment` (or any work verb) on a native issue made a moment earlier failed with
  "no work item" until the next sync; native items are now read through on a miss, in process.
- **Part 2 (below).** Replica copies and export backups.

## Comment redaction, part 2: replicas and backups (2026-11-01)

| Decision | Why |
| --- | --- |
| A replica refetches, it is not sent the text | The authority's `dev.zm.issue.comment_redacted` event carries no text (sending it would make one more copy). The replica refetches the work items filed under the issue (`issue:<id>`); their comment copies come back as `[redacted]`. The copy's old body *is* the text, so the replica scrubs it from that item's outbox rows (the request body and the authority's answer), then compacts `replicas/<project>.db` (VACUUM and a truncating checkpoint), as the authority does with its own stores. |
| Durable, retried, and exact (from the review) | The refetch is written to the replica's `refetch` table in the transaction that applies the event, so a restart before the fetch cannot lose it; a failed fetch is queued again. The compaction mark is cleared only after VACUUM and a full truncating checkpoint, and the sync loop retries it after every round (a reader holding a transaction makes a checkpoint busy). Outbox rows are scrubbed by text field (`core.ScrubJSON`: a comment that says "done" leaves a status write of "done" alone), and anywhere only for texts of 8 bytes or more. An item dropped from the copy (a 404 on refetch, or gone from the replica's areas, where the redaction event will never arrive) takes its settled outbox rows with it and marks the database for compaction. |
| A resync is covered too | A replica away during the redaction and back below what the log keeps takes a snapshot, which deletes and rewrites every row. The bodies before and after are compared, so a comment that comes back redacted is scrubbed the same way. Tested both ways: without the snapshot comparison, the text survived in the replica's WAL. |
| Backups are rewritten, not marked | Native issues are backed up as export snapshots (JSON Lines from the mirror, with the work comment copies, up to `keep` of them). No zm restore reads them, so a list in the file would protect nothing; the redaction rewrites every snapshot of the project's native trackers instead. Only lines of items filed under the issue change, and only text fields equal to a redacted text (`core.ScrubJSON`, which keeps numbers and unknown fields as written); other lines are copied byte for byte (tested). The tracker's home lock is taken, so a backup cannot write the text after the scrub; a held lock is a failed scrub, retried every 30 s. |
| Out of zm's reach | Copies outside zm's files: iCloud Drive's own version history of a snapshot, a backup copied elsewhere, a terminal's scrollback. A redaction's answer says what zm scrubbed; SPEC "comment redact" keeps saying that a text already read cannot be unread. |


## Adapter setup in the protocol (architecture steer, step 3)

- **Why.** `zm tracker add` linked zm-work-linear's package to check its config and add Linear's review state, so
  the daemon shipped Linear code and a community adapter could not get the same powers
  (`docs/ARCHITECTURE.md`, rule 3). Setup is now an optional section 11 verb: a dry run
  before binding (checks, defaults such as `app_default` from the adding identity) and a real run after (Linear's
  review state). `describe.setup.touches` declares what it changes outside zm, which the run-target protocol
  (section 20) also uses.
- **What zm keeps.** Only rules every adapter gets: no inline secret in project.toml, absolute `*_file`/`*_dir`
  paths. `internal/work` no longer imports `work/linear`. beads' setup (database init, schema checks) is next,
  with the zm lead.

## Secrets brick (2026-11-01)

| Decision | Why |
| --- | --- |
| One package, `potluck/kit/secrets`, with a driver per scheme (file, keychain, env) | Before it, ten call sites read secret files their own way: some trimmed only newlines, one checked permissions, none masked. One rule now: owner-only files, trimmed, masked `Value`s, errors without the secret. |
| No go-keyring | On macOS it shells out to `/usr/bin/security` as we do; on Linux it needs a D-Bus secret service that a headless CI VM lacks. Reading a file or an env var needs no library. |
| Owner-only files, refused otherwise | ssh's rule for keys. Every file the daemon writes is already 0600; a user's `--password-file` or `key_file` at 0644 now fails with `chmod 600` as the hint instead of being read. |
| Keychain for interactive hosts only | The daemon and serve-mode adapters run headless and must work with the keychain locked, so files stay the default; `keychain:` is for a laptop's adapter key. |
| Whitespace is trimmed everywhere | Before, beads and homes passwords were trimmed of line ends only. A stored password with a leading or trailing space or tab would now log in differently; every stored password zm made is random alphanumerics, and a hand-made one should be re-added. |
| Relative paths are refused | A `token_file` or `key_file` stored before the CLI made paths absolute resolved against the daemon's working directory; it now fails with "not an absolute path" until the tracker or link is added again with an absolute path. |
| Next driver: 1Password `op` with a service-account token | Teams share secrets without copying files; it is a driver, not a new read path. |

## Ranked search (SPEC "Native issues": bd door, Search)

Agents check for an existing issue before filing one, so `bd search` ranks (bm25, title weighted 10, body 1,
notes 0.5) instead of bd's title substring in priority order. bm25 on harbor's store is not cheap: common
words ("call" is in 9.5k of 11.7k issues) in ~200-word bodies cost about 2.6 µs a scored row, and finding a
match's row by rowid about 1.1 µs (the rows are wide). Measured choices:

- **The filters first when they are selective**: run once as a rowid set the FTS scan tests (`+f.rowid IN`,
  so FTS5 never does a lookup per rowid; labels from their index). Two words over open issues: 12 -> 5 ms.
- **One pass** reads every scored row (the total) and keeps the page; no second scan to count.
- **A cost model picks the plan** from two counts, but no count shows how often the words occur in long
  bodies, so **a deadline** (11 ms after the search began, the counts included; modernc interrupts the query)
  stops the ranked pass, and the newest matches come back `unranked`. The 16 ms bar holds by construction;
  what varies by machine is only whether a borderline query ranks.
- FTS5 `detail=column` or `none` would shrink the index but bm25 got 2-8x slower on it (measured); kept `full`.

On the CI VMs (standard-4), the generated 11.7k store of `TestRankedSearchSpeed`, p95: common word over open issues
10.1 ms ranked; two words 7.7 ms; two words over every status 11.5 ms; type filter 10.7 ms; rare word 0.6 ms; a
label filter with a common word 14.9 ms (deadline: unranked); a common word over every status 3.0 ms
(unranked). `make bench`'s `issue-rank` rows on its 11.5k store: 4.2 ms.

## bd door parity for the live switch (SPEC "Native issues": bd-compatible door)

- **Why these verbs.** Surveyed what the team's projects and `~/.claude` run: the SessionStart hook runs `bd prime`,
  `.beads/prime.mjs` reads `bd memories --json`, bd-guard and the farmer grep `bd dolt show`, renumber-migration
  parses `bd export`, and inflight, test-prune and daily-summary read `bd sql --json`. Anything else bd has either
  maps to an existing verb (`note` is `update --append-notes`), succeeds with a note where nothing needs doing
  (`dolt push|pull|commit`, `sync`), or refuses with a reason. A wrong answer is worse than a refusal.
- **bd sql is our own, on purpose.** No off-the-shelf component translates MySQL-dialect queries over beads'
  schema to another store. We serve SQLite's dialect over bd-shaped copies of the tables a query names. Copies, not
  views: a view over the live file still lets `main.<table>` and `pragma_database_list` reach redacted columns and
  the path (found in review). The copy costs about 1 s for the issues table on 11.6k issues (3 s for the 69k-row
  events), which is fine for scripts that run weekly. Any driver other than SQLite answers "not supported"
  (`istore.SQLReader` is optional).
- **Project choice.** bd on a machine reached one database from any directory (BEADS_DIR plus server mode), so
  agents in worktrees, in `~` and in other repos all filed into harbor. bd-zm keeps that: when the directory
  resolves to a project without native issues and exactly one project has them, bd works there and says so on
  stderr. A cwd-only rule would have filed issues into a new store for whatever repo the agent stood in.
- **Checked on a restored copy** (11,619 issues, real bd 1.0.5 on a scratch dolt server vs bd-zm on a throwaway
  import): memories and `prime --memories-only` are identical. Export has the same records, order and keys. It
  differs in two places, neither of which bd specifies or anything parses: the order of each issue's dependencies
  (bd's is the Dolt query plan's), and `dependency_count` for three edges to malformed targets. The script queries
  return the same rows.

## GitHub linking gates (SPEC 18.3, 2026-11-01)

harbor's old GitHub -> beads sync (`scripts/beads-pr-sync.mjs`, dead since the farm went off) closes a bead on a
merge only on positive evidence: a migration PR waits for a Supabase Deploy run that covers the merge; owner
packets, epics, parents, beads another open PR names and beads touched after the merge get a note; reverts are
ignored; closed beads are never touched; only the PR title names beads. 18.2's linker did none of that (it matched
branch and body too, and closed every named item on merge), so it could not replace the sync. Two changes, both
generic:

- `--match` picks the PR texts the matcher reads. harbor PR bodies mention unrelated beads; its title is the
  contract (`check-pr-title-bead.mjs`).
- A **gate** (a process plugin, like section 11, not linked code: architecture steer rule 3) answers apply, note,
  wait or skip before every item write. The policy stays in harbor's repo, in the same pure functions the old sync
  tested (`planAction`, `titleVerdict`, `deployCovers`, `ownerGuard`), so there is no fork of its logic in Go.
- **wait** is the part a stateless plugin cannot do alone (a migration waits for a deploy that may come hours
  later), so the linker keeps it: `ghlink_waits`, re-asked on the gate's pace up to its stated retention, then a
  give-up comment. The gate stays stateless. `last_write` lets it tell the linker's own comment from a person's
  later touch (the old sync's `syncUpdatedAt`).
- **Doubt never closes.** A gate error, timeout, garbage or broken describe is a wait. A gate's `match` failing
  holds the snapshot at the recheck pace (not the inbox's 8 doubling attempts), up to the maximum retention, so a
  gate down for an afternoon does not lose a merge.
- The gate gets an allowlisted environment with this daemon's `ZM_HOME` (its `bd` reaches this daemon, never one
  it would autostart) and fetches what else it needs. The linker passes data, not credentials, beyond `GH_TOKEN`.
- Not done: a gate cannot set a status other than the proposed one (the old sync set in_progress on a merge that
  shipped plain SQL; the gate notes instead), and the close reason is the comment (the work API's status call has
  no reason field). A Standard Webhooks HTTP driver for the gate brick is the expected second driver.
## Run targets (SPEC 20, protocol only)

- **Route by code, not caller.** Every socket caller is the owner, so the caller says nothing about risk; the
  code does. Callers name a ref, never a commit: zm resolves it live on the remote (`git ls-remote`), since a
  local or tracking ref can say anything. Only the protected default branch's remote head is trusted; anything
  else goes to ephemeral, credential-free targets unless the owner's binding says `allow_any_ref`. Class comes
  from the owner's binding as well as `describe`, the stricter winning, so a driver cannot vouch for itself.
- **Async, idempotent acquire plus a required `list`**, so a timed-out acquire is retried or reconciled, never
  doubled; targets end their own resources at the deadline so nothing depends on zm being up.
- **`zm-run-conformance` does not take a driver's word.** A driver can say `expired` while its run goes on, so the
  suite watches the resource: a probe script's heartbeat files and a database probe. During a short ttl it does not
  call the driver at all, since a one-shot driver that only sweeps when called looks fine to anything that polls
  `status`. A run reports back by exit code, the one channel the protocol guarantees (`log` is optional). Each check
  has a broken `refdriver` variant, and a test fails if a check passes on a good driver with no variant failing it.
  It sits in `potluck/runtarget`, not beside the tracker suite: the boundary test keeps the two modules apart, and
  this one needs its own runner anyway, for the allowlisted environment and the stderr it reads.

## internal/work tidy (2026-11-01)

- **No start-up adoption sweep.** Every daemon start used to list every native tracker's claimed and in_review
  issues to turn pre-lease claims into leases. The owner checked the live store: nothing is left to adopt. Binding
  native issues still adopts (`adoptNative` from `zm tracker add native`), since `zm issues import` brings claims
  in without leases; `TestNativeClaimWithoutBinding` covers it. A claim that loses its lease row later (a restored
  or wiped database while the issue store keeps `claimed_by`) is no longer fixed by a restart: binding the native
  tracker again (`zm tracker add native`) re-runs the adoption.
- **Only built targets parse.** `s3://`, `r2://`, `dolthub://`, `server://` as a backup and `embedded` as a move
  target used to parse only to answer 501. They are unknown targets now (400). Cloud targets, when built, go through
  `dolt backup` remotes (aws://, gs://) or rclone, never an S3 SDK. No home.json could hold one (they were refused).
- **Boxes need rsync and a systemd user manager (2026-11-01).** The `--service process` path (a nohup'd server that
  nothing restarts) and the tar copy for a box without rsync doubled the box code paths, and neither was exercised
  end to end (round 2's "not exercised" list). A box is now refused at plan time, before anything changes, when it
  has no rsync, no `systemctl --user`, or this machine has no rsync; the error says what to install or enable.
  Boxes stay Linux only (there is no launchd box). A box recorded earlier as `process` keeps working while its
  server answers; if it stops, zm says to start it by hand or set the box up again with systemd. `home.Process`
  stays: it is not the box fallback but the local-server `Manager` the tests in home, work and issues use so they
  never install a real launchd agent or systemd unit on the developer's machine; production picks launchd or
  systemd (`DefaultManager`).

## The speed gate (2026-11-01, `make bench`, load test, `scripts/ceilings`)

Since 2026-11-03 it reports and doesn't gate: `make bench` no longer blocks a merge, and the load test runs weekly.

- **One file, only tightening.** `scripts/ceilings/ceilings.json` holds every p95 ceiling, the same idea as
  `internal/deps/caps.json`. `make bench` and the load test embed it and fail over it (in CI, see "Timing doesn't decide green"). A raise or a removed row needs a
  one-line reason in the same PR (`bench tighten`, run against main's file on every PR); the agent makes that call
  alone. `issue-*` and `replica-*` cannot pass 16 ms whatever the reason: that is a constant in `ceilings.go`, so moving
  the bar is a code change, not a number.
- **Numbers come from the CI VMs, with headroom.** The highest p95 of several concurrent runs x 1.6 + 1 ms for bench rows and
  x 2 + 2 ms for the load test, capped by the old budget and by SPEC. The extra millisecond is for sub-millisecond rows,
  where a ratio means nothing. A Mac run says nothing about them: load on the shared Mac was 49 when the rows were first
  run, and every row looked ten times slower.
- **Noise.** A row over its ceiling is measured twice more and judged on its best try: a stalled VM passes on the next
  try, a real regression does not. The `issue-*` and `replica-*` rows take 200 samples for a steadier p95. The load
  test has no retry (3 minutes a try), hence its wider headroom. The comparison with main is a report, never a failure,
  and compares first tries, so that a retried row does not look faster than it was.
- **Timing doesn't decide green (2026-11-03).** Run 41641907512 on main at 0e51d6e failed `make bench` with
  16 rows over on attempt 1, then passed it and failed ranked search at 16-19 ms on attempt 2, with no change under those
  rows. Deploys key on `ci.yml`, so timing moved to `bench.yml`. One run over warns in the summary; on main, the same row
  over in two finished runs in a row turns `bench.yml` red and sends ci-result mail (`bench repeat`). The previous run
  counts red or green, since a red run's rows are what the next one repeats, and a missing artifact can't repeat. Ranked
  search's cases are `rank/*` rows of `bench.json`. `bench tighten` stayed in `ci.yml`, since it reads a file and times
  nothing; it is in `fast check`, the PR gate, since the full suite left PRs for main and nightly (2026-11-06: 12
  minutes a push, and about half its reds were flakes). The
  load test uses `-budgets=false` in `ci.yml`: latency misses warn in its log, artifact and job summary,
  while missing operation samples and correctness failures (including lost writes) still fail. Its CLI keeps strict
  budgets by default; no ceiling changes.
- **Only a CI-VM run gives a verdict (2026-11-07).** The ceilings came from the CI VMs. On 43 main runs on the Factory
  pool with no change under the CLI rows, `zm --version` p95 went from 7 to 51 ms and the rows over went with it (none
  at 12 ms or under, 15 to 40 at once from 15 ms), while the in-process `api-*` rows stayed under: two slow VMs in a
  row made a red. So `bench repeat -runner` gives no verdict off a `sengi-*` label (a notice; the rows over listed for
  the record), and `scripts/ci/bench-baseline.sh` passes only a CI-VM run as the first of two. With Factory routing on,
  CI-VM runs are rare (the last one was 25 finished main runs back), so when the last 30 have none it warns with the
  count and age, and `bench compare` falls back to main's last run labelled as other-runner data. `runner/probe` times
  an empty Go binary's start; on 14 Factory dispatches it followed `version` (correlation about 0.9). It has no
  ceiling; on the CI VMs it now marks a contended run (below).
- **A contended CI VM gives no verdict (2026-11-15).** CI VMs share hosts with other repos' jobs, and
  `bench-quiet.sh` sees only this repo's runs (it said quiet after 2 s before the b39b841a red). Main went red twice in
  two days on pairs of slow CI VMs, every row 1.5-2.5x over, `version` included. On the 18 CI-VM runs of
  2026-11-13..15, `runner/probe` p95 was 0.91-1.23 ms on the 8 with 0-1 rows over and 1.31-5.91 on 8 of the 10 with
  7-26 rows over; every noise red had one of those in its pair, and the one red on two quiet VMs (zero-start) was real.
  So `bench repeat` withholds a red when either run's probe p95 is over 1.3 ms (`-contended`) and lists the rows.
- **The probe is timed again after the rows, and a contended run is no verdict (2026-11-16).** Two noisy runs had
  quiet probes (0.99 and 1.05 ms, then 12 and 18 rows over from `hook-stop` on, five of them in both: that pair reds on
  the first probe alone): the VM slowed after it. bench.yml now times the probe again after its last timing step
  (`bench probe`, row `runner/probe-late`) and the worse of the two counts, against the same 1.3 ms until CI-VM runs
  measure the late probe. Its first two CI-VM readings: 4.93 (first probe 1.53, 14 rows over) and 1.24 (first 2.90, 6
  over), so it is not stuck high after the heavy steps; two readings are no calibration. A contended run with a row over can never be judged (it, or the next run paired with it), so
  `bench-baseline.sh` no longer counts it against blindness: neither a contended nightly nor one in the history is a
  day's verdict, and the blind message counts them. A contended run with no row over did judge (all under even on a
  slow VM: two such runs on 2026-11-15/16, probe 1.36 and 1.56) and counts. A CI-VM run whose bench cannot be read
  counts or stops the judging, as before (never a false blind red).
- **A verdict every day, and blindness goes red (2026-11-07).** Pushes route to Factory, so a baseline of
  push runs alone almost never held a CI-VM run. The nightly is pinned to the CI VMs, has its own concurrency group (a push
  never replaces it while it waits), and counts in the baseline with pushes. "Twice in a row" is bounded by age: the
  previous CI-VM run must have started within 48 h (`bench repeat -prev-at`, `-pair-max`), since the CI-VM run before
  a push can be days and many merges old. If main has had no CI-VM verdict for 3 days before a nightly, that nightly
  goes red (and `scheduled-red.sh` reports it), unless it was itself timed on the CI VMs: then it is the day's verdict
  (otherwise the gate's first nightly was red by construction); a push only warns. A Factory run compared with a CI-VM baseline labels
  its "slower" rows as runner against the CI VMs.
- **zm's own bench machine, calibrated before it judges (2026-11-07; a pool box 2026-11-09).** Owner
  decision: a quiet 8 GB machine on the Factory host, and (2026-11-07) through zm's own sharing of the Factory host, not a VM made
  by hand. So it is a zm pool box: the repo variable `ZM_BENCH_RUNNER` names a box size's runs-on label,
  `<pool label>:<size>` (e.g. `sengi:c2m8`), and pushes and the nightly ask `[self-hosted, <it>]`, which zm's runner
  broker serves with a fresh box of that size, one job per box (runbook: zm-bench-vm.md in the private ops repo). A label
  `zm-bench` (a dedicated runner) still works the same way. Empty, bench.yml routes exactly as before; unset it to fall
  back. The machine's speed is not the CI VMs', so runner classes (`scripts/ci/bench-class.sh`, `ceilings.ClassOf`: one
  rule): the CI VMs (`ceilings.json`), a box size `box-<size>` and `zm-bench`, each with its own file
  (`ceilings.<class>.json`); each size is its own class, since another size's binding may be another machine. A run
  pairs only with the same class within 48 h (`bench-baseline.sh`, `bench repeat`), and repeat re-judges both runs on
  the class's file. A class with no file is calibrating: its runs give no verdict and say "calibrating: N runs so
  far". From 10 runs every run warns "calibration ready"; `scripts/ci/bench-calibrate.sh` proposes the file with
  ceilings.json's own rule (highest p95 x 1.6 + 1 ms). The cap: `issue-*` and `replica-*` rows at the 16 ms bar, and a
  row with its own test budget at that budget, so calibration never moves a SPEC number; a capped row is already over
  on that machine and is printed for a look before the commit. If the warning lasts 3 days uncommitted, the nightly
  goes blind (red, scheduled-red reports it): the class's runs judge nothing meanwhile. Committed, the file only
  tightens (`bench tighten -class`), and deleting it counts as loosening every row. A CI-VM row with no class line is
  not judged there (the CI VMs' number says nothing about the machine). `make bench` retries against the class's ceilings
  once calibrated. Blindness counts a run on any class, and a nightly on any is its own day's run (as an earlier change made a
  CI-VM nightly), so the switch is not a blind day. The quiet-pool wait is skipped on a box or zm-bench (one job,
  benches only): a box shares the Factory host with another project's CI, which no wait here can see, and its admission ranks zm
  below it; calibration's spread shows whether that is quiet enough. The box image lacks `make` and `gh`, and its
  container has no root (sudo refuses: no new privileges; the first box run, 42731592417, failed there): the job runs
  `scripts/bench.sh` itself and `bench-tools.sh` fetches gh (jq if missing) at pinned versions, sha256-checked.
- **A CI-VM red from a busy pool, not code (2026-11-13).** Run 43472856102 (909218fc) went red with the
  nightly 43463288606 (79b9f306): `version` 17.2 and 22.1 ms against 13, `zero-start` 186 and 201 against 155. Both
  quiet waits ran out with 10 and 11 ci runs still on the pool (the 11 CI-VM runs before them since 2026-11-09: 0 to
  3), and `runner/probe` read 5.9 and 2.9 ms (those 11: 1.0 to 2.4, one 3.7). An interleaved A/B on one CI-VM job (43485048746,
  three rounds) found no difference between 8d45f136 (last green CI-VM run, `version` 11.6 ms) and 909218fc:
  `zm version` exec p50 11.97 and 11.23 ms (400 alternating runs each), bench `version` p95 19.8 and 20.4. The green
  build was over its own ceilings there. Main at 2791f7bd matched in the round where the probe read 1.3 ms (`version`
  15.5 ms; 909218fc 14.4, 8d45f136 26.1) and was slow only in a round whose probe read 9.6 ms. No ceiling moved.
- **`zero-start` 155 -> 250 ms: CI VMs differ in speed, and a fresh home's start has grown (2026-11-14).**
  Main went red on 0b59cf83 and 26a117ec (p95 192.7 and 194.6, 3 tries each). An interleaved A/B on one CI-VM job
  (43573208036, 80 alternating samples per build; the run was cancelled after its table printed) found no step in the
  range: p50 124.7, 124.6, 125.6, 126.9 and p95 145.7, 137.5, 136.0, 141.7 ms for cf6c97c3, 0b59cf83, 26a117ec and
  ee978b54; six bench rounds per build, p95 120 to 142. #747 is replica-only; #731 adds one migration. The red runs
  had a slower median, not more slow samples: each CI-VM run gets its own VM, and p50 across runs ranged 123 to 182 ms
  (155.0 at 3e3356c3, whose p95 176.0 was over the line), while builds on one VM differed by under 3 ms. 4 of 6 CI-VM
  main runs on today's code were over 155, so 155 was too tight for the CI VMs' mix of speeds. The 155 dates from
  2026-11-01; CI-VM runs of Nov 5-11 had p50 72 to 133, median 98. Since then `version` and `daemon-ready` held flat,
  but a fresh home's daemon start grew: on one CI-VM job (43574745166) exec to health took p50 65 to 78 ms at the
  Nov 7-9 builds and 99.7 ms at ee978b54. A fresh home runs every migration, one transaction each (Potluck
  `kit/store`), and there are now about 109 registry and 45 project migrations, against 69 and 32 on Nov 9. Builds
  from 2c86ff7b to f852ee15 pin Potluck commits that no longer resolve publicly, so the drift cannot be bisected
  further. That count is the likely cost, not yet profiled. Ceiling: 250, 25% over the worst CI-VM p95 seen (200.8),
  under SPEC's 500; 313 (194.6 x 1.6 + 1) was refused as double-counting the noise margin, and the owners chose 250
  It tightens again with a later change (a fresh database's migrations in one transaction).
- **The store is generated, not copied.** `scripts/bench/native.go` writes 11,512 issues with the harbor import's shape
  (per issue 1.3 comments, 1.05 deps, 1.1 labels, 6 history rows; 80% closed; 1 KiB bodies) through the store's
  `Import`, then copies the file into each daemon before it opens it. No beads copy, no secret, 28 s on the CI VMs.
  `internal/issues/real_test.go` stays: it proves the same bar on the real import.
- **Replica rows run in the bench's own process.** A fake tailnet node (`tailnettest`) cannot cross into a separate
  process, so the authority and the replica are `server.New` daemons from this tree, on real Unix sockets. `ZM=<other
  build>` therefore turns them off. `issue-*` rows have a daemon of their own: on the main bench daemon, a second 11.5k
  project doubled `work-search`, because `zm work ls -q` searches every project.
- **`team-*` rows are not on the bar.** A claim, a release and `zm work new` wait for the authority by design (SPEC
  17.6). They keep SPEC 17.7's 50 ms loopback as an outer limit. A native issue has no route of its own on a replica yet
  (SPEC "Replica hook"), so the replica's rows are the work routes over its native-tracker items.
- **The first run found two real misses, both fixed in `internal/team`.** Search on a replica: 51 ms p95, from decoding
  done items one by one and running LIKE on two JSON fields of every row. Status now goes down to SQL (an item with an
  unsettled status write always passes, for the Go checks to judge), and the search moved to Go: `bytes.Contains` over
  the raw row, so only likely rows are decoded, and text JSON escapes is found after decoding. The copy is memory-mapped
  (`mmap_size`): most of the walk was `pread`, one page at a time past SQLite's 2 MB cache. A search with `--status done`
  walks four times the rows and is not on the bar's row. Queued writes on a replica: 22 ms p95 and stalls up to 742 ms,
  from writers on the copy polling in SQLite's busy handler (it sleeps 1, 2, 5... 100 ms) behind the sync, drain and
  refresh loops, each paying a FULL fsync. Writers now queue on one Go mutex (`replica.write`). Writes stay under 16 ms at about 12 ms p95: a queued write
  waits for at most the transactions ahead of it. The same queue took the load test's comment p95 from about 110 to
  65 ms and claim from about 270 to 175 ms.
- **The load test could not fail before.** CI ran `loadtest-bin | tee` under GitHub's default `bash -e`, with no
  pipefail, so tee's exit code was the step's: a lost write or a double claim left the job green. The step now keeps the
  binary's status and writes the table to the summary either way. It had been hiding a real failure of 10-minute runs:
  90 s after the authority restarted, one replica stopped draining for good, "invalid or expired token". The fake
  tailnet was at fault (`tailnettest`): an idle connection to the old listener closed and deleted the WhoIs entry of a
  newer connection on the same local port, and a node-bound token with no WhoIs is a 401 for as long as the connection
  lives. A stuck replica now leaves its status and `loadtest-goroutines.txt`.
- **Claim is over SPEC's loopback number.** The load test's claim p95 is about 175 ms, 200 at 10 minutes: under SPEC 17.7's
  250 ms for a tailnet, which is its ceiling, but over the 50 ms it names for loopback. That run puts 44 agents and six
  daemons on one 8-core VM. Idle, `team-claim` is 15 ms. A queued write under that load is about 65 ms, so the 16 ms bar
  holds in `make bench`, one agent at a time, not at full load.
- **No GC during package init (2026-11-12).** Every CLI call runs every linked package's init first. Linking
  potluck (runtarget, textclean, ghapp, runners and more) took that from 5.5 to 7.7 MB of allocations, so from two to
  three collections before main, and from CI-VM run 43110112286 on the hook and CLI rows sat 1 to 4 ms over in run
  after run. `internal/initgc` turns the GC off while init runs on a plain command (argv a command word or
  `--version`); main (and cmd/zm's TestMain) turns it back on first thing, honouring GOGC. Hidden modes (`__zm-holder`,
  `__zm-task-*`, `__zm-agent`, `__zm-launch`) start from an init and never reach main, so they keep the GC on. Locally
  `zm --version` now collects nothing and a hook collects once; the ceilings stay as they are. Check an init
  regression with `GODEBUG=inittrace=1 zm --version` (also where initgc falls in the order).

## Homes out of the work module (2026-11-01)

- **internal/homes is a module; the server wires the seam.** `home_*.go` left `internal/work` with no change to
  routes, CLI output, API shapes or disk formats. Work could have imported homes, but modules never import each other
  (package core) and the boundary test would need a standing exception. So core holds two small interfaces:
  `core.TrackerHost` (`d.Trackers`, homes asks work) and `core.HomeService` (`d.Homes`, work asks homes). The
  targets moved with it (`internal/homes/home`) for the same reason.
- **The export backup stays in work.** It is a snapshot of the mirror, so `TrackerHost.Export` writes it (with
  `home_redact.go`, which scrubs it). The schedule, the home lock, retention and the record stay in homes. A redaction
  reaches the directories through `HomeService.ScrubExports`, which holds the lock.
- **Helpers both modules need have one implementation, in shared libraries.** `internal/work/setup` was not an option:
  it is in the work brick, so homes may not import it. `secrets.CopyFile` and `secrets.ReadFile` hold the password rules
  (0600, 0700, 4 KiB, non-empty), so every caller keeps the same rules; the tracker vocabulary is in
  `api/tracker.go`, `ExpandHome`, `PathExists` and `ClipText` in `core`. Log lines keep their `work:` prefix.
- **homes starts after work and closes before it**, since an export reads the mirror; that is the order work's own
  Start and Close had. homes' tests wire a real work module, which a test may do. They quiet its mirrors with
  `Config.SyncPause`, which is never read from disk: another package cannot reach work's unexported pauses, so a
  `worktest` package would have needed them exported.

## Remote terminals (zm attach over ssh, 2026-11-01)

On the cmux walkthrough, the question was: "I don't know why we can't attach to remote terminals." The cockpit showed another machine's session as a
card of status and mail. Now a click gives its terminal, typing included. SPEC 8 "Attach over ssh" is the contract.

| decision | why |
| --- | --- |
| The system `ssh`, running `zm attach --embed` on the far side | No relay, no new auth, no new route. The far side is an ordinary local attach, so replay, resize, the reply filter and exit codes come for free, and whoever may ssh in is who may attach. Same choice the retired Dolt homes made. |
| The machine's name is the ssh destination; no zm config | `~/.ssh/config` already says how to reach a name. A `machines` map in config.json would be a second place to say it. |
| `ssh` owns the terminal, `zm` only waits for it | A byte proxy in zm would add a hop to every key and redo raw mode and SIGWINCH. zm needs one thing: how ssh ended. Stderr goes through zm (tee), stdin and stdout never do. |
| The reason comes from ssh's words, in exit codes 4, 5, 6 | A cockpit must say why in plain words. A preflight (`ssh host true`) would cost a round trip on every attach and can still race. ssh's messages have been stable for years and are not translated; an unknown one is "dropped, attach again", the safe answer. |
| `WaitDelay` on the ssh process | A `ControlPersist` master stays behind and may hold stderr open; waiting for EOF would hang the attach for ten minutes. |
| `BatchMode` for `--embed`, prompts otherwise | A reattach runs in a hidden surface, where a prompt would wait forever. A person runs `zm ssh <machine>` (a cockpit: Open a Terminal) once and answers there. |
| `ObscureKeystrokeTiming=no` | OpenSSH 9.5+ holds keys to a 20 ms beat so a network watcher can't time them. Measured on loopback: **14 ms added to every key** (p50 14.6 ms against 0.67 ms without), against the "must feel instant" bar. The price: someone on the path could time keystrokes, as with every OpenSSH before 9.5; the content stays encrypted. `-o` beats ssh_config, so a user who sets it on is overridden here. `IgnoreUnknown` keeps an older ssh working. |
| `ForwardAgent=no`, `ForwardX11=no` | The machine is named by presence and may be a teammate's: a `Host *` forward in someone's config must not hand it their agent. |
| Machine names checked before ssh, and `--` before the name | A teammate's machine name reaches an argv here: `-oProxyCommand=...` would be code execution on a click. |
| `-e none` | `~.` after a newline would otherwise kill the connection, and `~` would vanish from what is typed. |
| Only a name this Mac already knows as an ssh host (lead's ruling, review of #86) | `sub.Machine` is what a replica chose at join, so a teammate could name a machine `evil.example.com` and have a cockpit connect there and tell the person to answer its prompts. `ssh -v -G` reports which Host pattern applied, Includes followed, and `ssh-keygen -F` reads known_hosts, hashed or not; no hand parser, and a changed message fails closed. Only an exact pattern counts: `*.local` or `dev-*` fits whatever name a teammate picks. `Match` blocks print no such line and don't count either; those hosts connect once by hand. 5 ms a check, on every attach. A person typing `zm ssh <host>` chose it, so that one goes ahead. A teammate can still name a host you trust (`github.com`): ssh runs `zm attach` there and finds nothing, which is the price of `known_hosts` counting. |
| Reattach only after a session was up, at most 40 in a row | A first attach that failed with exit 3, or a failure of ssh's that no pattern knew, reattached for ever with a spinner. Now it shows ssh's last line (`--why-file`, not scraped off the screen, where a wrapped line is cut). Up means the session's screen arrived and held 250 ms. |
| The daemon's check is the only one on this side, and it is not a boundary | Anyone who can ssh to the box is its owner on its socket. The check keeps the promise "only what the daemon shows you"; the login is the real gate. |
| `zm attach --on` | A new session is not listed here until its machine's next heartbeat; waiting 5 s for a shell is not instant. It asks the far daemon, which is the one that knows. |
| Reply window widened on the far side only | A terminal's answers to the replay arrive a round trip after the output. 250 ms of quiet would let them through on a slow link and type them into the agent. An environment variable, so an older far zm just ignores it. |

**Tested.** `cmd/zm/remote_test.go` runs two real daemons with a shim `ssh` on PATH (both directions, exits, refusals, `run --on`). Beyond
the brief, a throwaway `sshd` on 127.0.0.1 with its own keys and a forced command pinned to a throwaway ZM_HOME ran the real client: ControlMaster, a
killed master (exit 3), refused key, closed port, unknown host, no zm, `~.` passing, and the 14 ms above. **Never tried against a real remote machine.**
Typing, keystroke to echo through a pty, with a real program echoing:

| path | p50 | p95 |
| --- | --- | --- |
| local attach | 0.06-0.23 ms | 0.14-0.39 ms |
| fake ssh shim (load 55) | 0.20 ms | 9.71 ms |
| real ssh on loopback, ObscureKeystrokeTiming on (default) | 14.0-15.3 ms | 15.2-20.1 ms |
| real ssh on loopback, off (what ships) | 0.61-0.67 ms | 4.5-6.6 ms |

Ready to type: 54 ms over a shared connection, 102 ms over a new one, against 13-22 ms local. A real link adds its round trip to each of these.

## Backup encryption (SPEC 17.9, 2026-11-11)

`project.db` holds secret names (the grants table, the secret ledger), and both its backups were plain copies:
sqlite copies on a disk, and off-site sets in S3/R2 unless `--encrypt` was set. The owners ruled (2026-11-11): encrypt
both, on by default, the key with zm's existing secrets, restore must round-trip.

- **One key per home, in `secrets/`.** `$ZM_HOME/secrets/backup.key` is where zm keeps every secret file (tracker
  passwords, the GitHub link token), 0600 under a 0700 directory, and no backup holds `secrets/`. It is made on first
  use, so encryption needs no setup and no switch; the cost is that losing the machine loses the key unless it was
  copied off, which `zm backup` says on every status. A restore puts the key into the new home, so old and new sets
  share it.
- **Our own cipher, not rclone crypt, for the default.** rclone crypt already encrypts off-site sets, but only
  through the pinned rclone, which a plain `file://` target does not need today: making it the default would make every
  file target fetch and run a binary, and break a home whose daemon has none. The sqlite copies have no rclone path
  at all. So `internal/homes/bkcrypt`: AES-256-GCM in 64 KiB chunks with a per-file HKDF key (the STREAM
  construction: a chunk's nonce is its index and a last-chunk flag), a few hundred lines of standard library, no new
  dependency. rclone crypt stays as an optional second layer that also hides names. Rejected: age (a dependency
  for the same construction) and sealbox (HPKE to a daemon's key, for blobs up to 12 MiB between machines, not
  local files of hundreds of MB).
- **Blanking instead was the other choice** (the bead's alternative: blank the grant names and ledger rows in the
  copies, as the export filter does). Rejected by the ruling: a restore would bring back a project.db with its
  grants broken, and every new secret-bearing column would need its own blanking rule.
- **Plain copies go once an encrypted one exists.** An older zm's sqlite copies and off-site sets still hold the
  names, and retention would keep them for a day (copies) or a month (daily sets). So the first encrypted set deletes
  them, with the redaction's rule: only after the new set is whole, and a failed delete fails the run and retries.
  Restore still reads plain copies and plain sets.

## Native store backup (2026-11-01)

Export snapshots hold only mirror items and work comments, so they could not rebuild a native store. `issues.db` and
`project.db` had no real backup. This adds the `sqlite` kind, the `potluck/kit/sqlbackup` brick and `zm issues restore`.

- **VACUUM INTO, not Litestream.** A redaction must make text stop existing. Litestream ships WAL frames and keeps
  them until retention expires, so a redacted text would outlive its redaction by the retention window. A VACUUM INTO
  copy holds only live rows, carries no free pages or log, and is deleted as a file. The cost is a whole file per copy,
  not an increment.
- **Local copies only (2026-11-01).** A sqlite backup goes on `file://` targets; `icloud` is refused. An
  off-site tier with gzip and a debounced purge was built and reviewed, then cut: each copy is a whole store, 320 MB an
  hour to iCloud Drive stalled a Mac, and copies on our own machines are enough. An export can still go to iCloud.
- **The purge comes last, and its state is a mark.** It runs after every scrubber and compaction: `project.db` must
  be clean before it is copied, which `core.Deps.ScrubAll`'s registration order could not promise from inside a
  scrubber. So it is a `HomeService` call from issues, not a `Scrubber`. Unlike other scrubs it needs no text, only "copy
  again, then delete", so its retry lives in `home.json` (`scrub_due`), survives a restart, and uses the scheduler's
  pacing. The 30 s in-memory retry would mean a whole-store copy every 30 s against a full disk. A redaction waits up to
  30 s for a backup in progress first (`WaitIdle`, in `scrubWithRetry` only: an import's re-apply runs under a move's
  own lock). Waiting beats failing: a scrub needs that lock, and a copy's read makes a compaction's checkpoint busy. A run clears only the
  mark it read, and only a run that began after the mark purges: a copy started before a redaction finished may hold
  the text. A crash between the store write and the purge leaves no mark, as it leaves other scrubs undone (SPEC
  "comment redact").
- **Redaction order.** Fresh set written, checked, named; only then `KeepOnly` removes the others, by name rather than
  by age, so a copy dated in the future by a clock change cannot survive; it refuses to run if the copy to keep is not
  there. Hence never zero copies.
- **`last_ok` means a file.** PR #80's export reported hourly success with nothing on disk. Now a copy gets its name
  only after integrity_check, a size that equals its pages, and (the issue store) at least a quarter of the last good
  one. The last check also protects retention: without it a store emptied by mistake rotates every good copy out in
  a day. `tracker ls` reads the directory, not the record, so a deleted copy shows.
- **Nothing partial, and nothing world-readable.** VACUUM INTO's output is 0600 from before its first byte: the empty
  file is made first, and VACUUM INTO accepts an empty file.
- **The swap sets the log aside instead of deleting it.** A log beside the new file would be replayed into it, so
  it must go before the rename, but a failed rename must not lose it: for a store that could not close cleanly its
  newest rows are only there. A crash leaves `-wal.aside`, which the next open deletes without restoring: it cannot be
  told whether the rename happened, and it may hold text.
- **The restore finishes on the copy.** The agreed order was hold, swap, re-apply. It re-applies before the swap
  instead: the redaction list runs on the staged file, which is compacted, then that file replaces the store. So a
  failure leaves the live store untouched, and a redacted text is never served even for a moment. Writes are still
  held for the swap and the copy of the current store before it. A dry pass runs first, so a refused copy costs nothing;
  it is also the whole of `--dry-run`, so "does this backup restore" is a command.
- **The mirror is wiped, not re-listed.** `mirrorUpsert` keeps the row with the newest `updated`, which is right
  everywhere except here: restored rows are older on purpose, so a full listing changes nothing and the mirror would
  keep showing the rewound text until the end of time. An earlier version of this change only forced a re-list, and
  the end-to-end test caught it. `TrackerHost.Relist` wipes and lists. A crash between the swap and the wipe leaves the
  mirror stale; restoring again fixes it. `Relist` grew `internal/work`'s top level, so `exportscrub` moved to a
  sub-package and the cap went down.
- **`project.db` is copied, not restored.** It holds mail, events and leases of everything else in the project: a
  store restore that rewound them would lose mail to fix issues, and `potluck/kit/store` has no way to swap a live file.
  Its copies are for a machine that lost the file, by hand with the daemon stopped (SPEC 17.9).
- **The current store is kept as a normal copy.** A `.before-restore` file next to the store would be a copy with no
  scrubber (ARCHITECTURE rule 5). In the backup's directory it is purged like any other and counts against `keep`.
- **Schema check by names, not a number.** The store has `schema_migrations`, no version. A copy naming a migration
  this binary lacks is newer; `zm` cannot read it safely, and there are no down-migrations. Older copies migrate up
  through the same path as any open, on the staged file.
- **The default kind for a native tracker is now sqlite.** A move to zm-native still turns dolt backups into exports:
  the record cannot pick a target for a whole store, and a dolt backup on icloud cannot become a sqlite one. `where` says
  an export cannot restore the store. A rollback unschedules sqlite backups, since it
  empties the store they copy.
- **`potluck/kit/sqlbackup` is a shared library.** Homes and issues both call it, and modules may not import each other,
  so it is in `sharedLibs`. It imports no module. The issue store driver stays free of it: `Restorer.Copy` hands the
  service a handle, and `Snapshot` runs there. That handle is a fresh connection: the reader pool is `query_only`,
  which refuses VACUUM INTO, and the writer pool is one connection a copy must not take.
- **Not reached by a purge:** copies of a backup unscheduled with `rm` (the schedule goes, the copies stay, and zm no
  longer knows of them), and a `--from` copy outside the backup's directory. The restore re-applies the list, but the
  file it read is untouched.
- **Out of scope: evicted export snapshots.** An export on iCloud Drive that was evicted is a stub, `.<name>.icloud`,
  which `exportscrub` does not match, so a redaction misses that snapshot. A stub has no content to rewrite, and
  deleting it trades retention for the scrub. Exports and their iCloud target are left as they were.
- **`project.db` is copied through its own connection.** The solo pool has two, and a copy of a large file must not
  hold one, so a copy opens the file itself, as the issue store's does.
- **Free space is checked first.** A copy asks for 1.1 times the source, so a full disk fails at once with a clear
  error rather than part way through a whole-store write.

## Safe swaps: preflight, upgrade, restart, self-check (2026-11-01)

On 2026-11-01 a session ran `make install` from main and restarted the daemon. That build did not forward native
issue reads for a joined project to its authority, so `bd` said `no project "harbor"` for 6 minutes, managed sessions
ended, and a second restart came a minute later. The only guard was a mail rule naming one session. Now zm enforces it.

- **Two checks: one before the swap, one after.** The preflight is static and costs no downtime. It catches what a
  build can say about itself: its protocol, the migrations it knows, its routes, and which routes its team proxy
  forwards. The verify after the swap is dynamic. It catches everything else by asking the new daemon the same reads
  the old one answered, and it is why a missed case costs a rollback, not an outage.
- **The new binary judges itself, against what the running daemon reports.** `zm daemon preflight` is run by the new
  binary (`make install` and `zm daemon upgrade` run the staged one). Only the new build knows what it can do; only the
  running daemon knows what it serves and what its build forwards. So the daemon reports a manifest (`GET
  /v0/daemon/manifest`), and the new binary wires a throwaway daemon in a temp home to list its own. A binary older
  than this feature has no `preflight` command, and that alone refuses it.
- **What refuses, and why each is mechanical.** No list of feature names to keep up to date:
  - another protocol major;
  - a migration applied in any database of the home (`registry.db`, each `project.db` and `issues.db`, each replica
    copy) that the new build does not register: the data is newer than the build;
  - with a project joined: a route the running build forwards to an authority that the new build does not
    (`team.Proxies` run over every registered pattern). This is the 2026-11-01 case;
  - a route the running daemon answered since it started that the new build lacks.
  A route the new build lacks but nobody called is a warning: refusing every downgrade past an unused route would teach
  people to pass `--force`.
- **`--force` is loud.** It prints each overridden problem, and the swap record carries `forced` and the problems, so
  the next session sees what was overridden and by whom.
- **The served set.** `swap.Probe` asks a daemon what `bd` and `zm` ask: the joined projects (`GET /v0/team/remotes`),
  the issue projects, each issue project's first issue (bd's read), each project's first work item, and each tracker's
  adapter (`GET /v0/work/sources`). Items that answer are the served set. The upgrade probes before and after; an item
  that answered before and fails after is a regression and rolls back. A project that left between the two is not
  expected.
- **A joined project is always expected after a swap.** The probe takes the joined projects from `GET
  /v0/team/remotes` (state ok) before the swap. A build that cannot serve one also drops it from `/v0/projects` and
  the issue projects, so "the project is gone" cannot mean "it left": each joined project, and each of its items,
  missing afterwards is a regression (review of #108; `TestProbeCatchesNoProjectOncall` replays the outage). Only a
  local project the new daemon no longer lists is taken as removed.
- **Rollback keeps the previous binary beside the new one.** Each replaced file is kept as `<name>.prev` in the same
  directory (a rename, so the swap is atomic per file and needs no extra space). On a failed start, health check,
  build check (the daemon must report the build it was given) or probe, the new daemon is stopped by its saved pid, the
  `.prev` files are renamed back, the old binary is started with the same arguments and probed again. A rejected
  daemon that answered is stopped first, as the lock's holder, then by the pid this swap saved when it started it
  (TERM, then KILL), and only then do the files go back: otherwise the old binary's start meets `daemon.lock` and the
  rejected build keeps serving (review of #108). Each failure message says what runs afterwards: the old daemon,
  the rejected one, or none.
- **A supervised daemon is restarted through its supervisor.** A systemd unit (or a launchd job) whose main process
  is the daemon would bring its own daemon back beside one zm started. So before any start, zm looks for one: on
  Linux the `.service` in `/proc/<pid>/cgroup`, accepted only when that unit's `MainPID` is the daemon (a daemon
  started from a shell some service runs is not that service's); on macOS the job in `launchctl list` whose pid is
  the daemon. Under one, an upgrade places the files, checks the stop guards with a dry run (an older daemon: the
  managed sessions it lists), runs `systemctl [--user] restart <unit>` or `launchctl kickstart -k gui/<uid>/<label>`,
  waits for a daemon with another pid, and verifies. A rollback restores the files and restarts through it again.
  zm never starts a daemon of its own there. A detection error refuses the swap. The unit must run the binary in the
  install directory: its `ExecStart` path (or the launchd job's program) is read first, and an upgrade into another
  directory is refused before anything is placed, naming the binary the unit runs. Placing it elsewhere and exiting 0
  would report a swap that never happened.
- **A preflight has 60 s.** A binary whose preflight hangs is refused when that time is up, so it cannot hold
  `swap.lock` forever.
- **The handover is short because only the stop and the start are in it.** The files are placed while the old daemon
  still runs (a running process keeps its inode). Clients that find no daemon during the gap do not autostart one: an
  autostart never starts one while `swap.lock` is held, so no session starts a third binary between the two. (It
  used to give up waiting after 15 s and start one: under launchd the old daemon took 15 s to exit, the autostarted
  daemon took the home, launchd's exited "already runs", and the upgrade rolled back onto a daemon beside the job.)
- **One swap at a time, enforced by the daemon too.** `swap.lock` is an flock held for the whole stop, restart or
  upgrade; its body says who (the caller's zm address), what, since when, the pid, and a random nonce. A second one is
  refused with that holder. The daemon's stop route refuses while the lock is held unless the request carries the
  holder's nonce, so an old CLI that calls the route directly cannot stop a daemon mid-swap.
- **Stop is a restart in disguise.** The next command autostarts whatever `zm` is on disk, so `zm daemon stop` runs
  that binary's preflight when it is not the build the daemon runs, and refuses on failure. This is the path the
  incident took (install, stop, autostart), and the one a plain `go install` still opens.
- **Sessions.** Stop, restart and upgrade refuse while `RestartBlockers` lists managed sessions that would end, and
  print each (address, agent, title, state). The zm-session-survive lane lets a session outlive the daemon; such a
  session drops out of that list, so the guards let the swap through with no change here. `--end-sessions` overrides
  (stop's old `--force` stays as its alias).
- **Startup self-check.** After its modules start, the daemon probes itself through its own socket handler, checks
  every database's migrations against its build, and compares with `served.json`, the set the last daemon on this home
  served. Whatever it cannot serve goes into `selfcheck.problems` in health (local callers) and `zm status` (exit 1),
  into the log at error level and a `dev.zm.daemon.selfcheck` event. It does not stop: it serves what it can. While
  problems remain it checks again every minute, so an authority that comes back clears its line. This also catches a
  downgrade that bypassed every guard (a plain `go install` and a crash), because the previous daemon's set is on disk.
- **Who swapped what, and when.** Every stop, restart and upgrade is recorded by the daemon that ends up running
  (`POST /v0/daemon/swaps`): a log line at warn level, a `dev.zm.daemon.swapped` event, and `swap-last.json`, which
  health and `zm status` show. When no daemon of this feature answers (a rollback onto an older build), the CLI appends
  the same line to `daemon.log` itself.
- **A signal past the commit is answered, not obeyed.** From its first change a swap ignores SIGINT and
  SIGTERM and ends within its budget (13 min): verified, rolled back or rolled forward. The first signal prints when
  it ends; each later one prints the step it waits on and the time left. There is no hard escape (a third Ctrl-C
  that exits): most points a swap could stop at are ones the next run does not recover from by itself (a new daemon
  running unverified, the previous files restored under it, a rejected daemon left up, or no daemon and the next
  autostart running the unverified build), which is what a later change removed. SIGKILL remains, and is the user's call.
- **Not covered.** A raw `go install` into the daemon's directory replaces the file with no check and no `.prev`; the
  next start's self-check reports what broke, but nothing rolls it back. A `project.db` that fails to open still stops
  the daemon at start, as before: skipping it would serve a project with half its data. A restart drops the shell's
  `BEADS_*`, `BD_*` and `DOLT_*`, as autostart does; a binding with `env = "inherit"` that needs them is restarted by
  hand with `zm daemon --detach`.

## Joining over a Cloudflare Tunnel (SPEC 17.11, 2026-11-01)

Goal: a teammate joins a team project and syncs with its home without installing Tailscale. The home runs
`cloudflared` in front of a localhost HTTP listener; the member reaches it at an `https://` URL. Why tsnet first:
it gave the home an identity (WhoIs on the node, so tokens bind to a node) and a private network in one embedded
library, with no account resources. A tunnel gives neither, so zm adds the identity itself and treats Cloudflare as
transport only.

| question | decision |
| --- | --- |
| The listener | `tunnel.listen` in config.json, loopback only (like `web.listen`), off by default. `tunnel.url` is the public `https://` URL invites name. Requests there are network (never local), like the tailnet's. Both listeners can run at once. |
| What replaces the node | Each daemon has one ed25519 key, `$ZM_HOME/tunnel/key` (0600, made on first use). Its id, `zmkey:<fp>`, is the "node" of a key-bound token: `tokens.node`, `team_nodes`, idempotent redemption and every node rule work unchanged. A key id has a colon, a Tailscale stable ID never does, so the two kinds cannot collide, and a token bound to one transport fails on the other. |
| Proof of the key | Cloudflare ends TLS at its edge, so there is no connection to bind to. Every request is signed: `Zm-Key`, `Zm-Date`, `Zm-Nonce`, `Zm-Sig` over the home's fingerprint, method, path and query, the date, the nonce, `Authorization`, `Content-Type`, every `Zm-*` header and the body's sha256 (each header value length-prefixed, so repeated headers cannot be read two ways). The home refuses a date more than 5 minutes from its own clock and, for token callers, a nonce (16 to 64 base64url characters) that key sent while the request's date could still pass, across restarts. |
| What replaces fingerprint pinning | TLS from the public CA, plus the home's key. A tunnel invite is `<url>#<home fp>:<secret>`, fp = 26 base32 characters of sha256 over the home's public key. Every answer to a signed request carries `Zm-Home-Key` and `Zm-Home-Sig` over the request's nonce, the status, `Content-Type`, every `Zm-*` header and the body's sha256. The replica drops any answer that is not signed by the pinned key, before reading a byte of it as data. |
| Redemption | The code's secret never crosses. The replica sends a proof: HMAC-SHA256 keyed by the invite's stored code hash over the home's fp, its own key id and its machine name. The home tries each live invite. A tunnel code's secret is 25 Crockford characters (125 bits), so a captured proof cannot be guessed offline; the tailnet's 40 bits are safe only online. |
| Only zm's auth | Unsigned requests are `401` before routing. Signed without a token: only `POST /v0/invites/redeem` and the GitHub join's three calls (SPEC 22). With a token: only a key-bound token of the signing key (`core.Authenticate`); a `zm token mint` token is `401` there, and no tailnet login applies. The web UI, its sign-in and live event streams are not served on the tunnel. Redemption limits: 5 wrong proofs per client address (`CF-Connecting-IP`) and 50 overall per 10 minutes, apart from the tailnet's, so internet noise cannot lock out tailnet joins. |
| Clock and replay refusals | A stale date or a reused nonce is `400` with a hint, not `401`: three `401`s purge a replica's token, and a skewed clock must not do that. Only the distance between the two clocks counts (at most 5 minutes; the hint says one of them is wrong, not which). Replays are stopped by the nonces, not by time. Each is kept until its own `Zm-Date` plus 2×MaxSkew plus a minute: a request passes only within MaxSkew of the home's clock, so a home clock that steps back by up to MaxSkew after an entry was dropped still refuses the request for its date (a 6-minute keep let a step back of over a minute reopen it; review round 4). Every nonce is appended to a journal (`$ZM_HOME/tunnel/nonces`, 0600) before its request runs; if that append fails the request is `503`, never run memory-only, and the next one reopens the file. A restart reloads the live entries. If the journal cannot be read at start (missing while the key is not new, unreadable, a read error), writes over the tunnel are `503` for as long as an entry is kept (2×MaxSkew plus a minute, 11 minutes) by the monotonic clock, logged as an error; reads go on. A durable `tunnel/nonces.hold` preserves uncertainty across restarts, each of which waits a full local monotonic window before clearing it. To let current members work immediately, only the door's pre-dispatch refusal supplies a signed `Zm-Recovery` challenge (32 random bytes for this Service, never persisted). After verifying that whole response, the client retries once with the same body and write id, a fresh nonce/date, and the challenge signed into the request. The normal nonce append and authorization remain mandatory. A captured earlier request cannot add the new challenge without invalidating its signature, and the next process chooses another challenge. This does not clear the hold: legacy clients and no-token redemption still wait, and generic application503 or ambiguous transport failures never cause this retry. The journal is rewritten with only live entries once dead lines outnumber live ones by 4096, off the request path: the live set is copied under the lock; the big write and its fsync run without it; under it, the lines appended meanwhile are written and fsynced, the file is renamed over the journal and appends move to the new file (always, once the rename happened; a failed directory fsync is only logged). A failed rewrite waits a minute and another 4096 lines before it tries again. Appends are not fsynced (`F_FULLFSYNC` per request costs milliseconds): a power loss can lose the last ones, which the write-id records still cover for writes. An earlier version refused dates before the daemon's start instead; review: a home clock stepping back, or a slow member, then locked members out. Each member key has its own share (2^17 nonces), and only callers whose token authenticates enter it. |
| Live writes carry a write id | A write forwarded live (a route whose row is a `Write`, never a read: bd sql's POST carries none) gets a `Zm-Write-Id` like a queued one, and the authority runs each (identity, write id) once, whatever the write route: a native-issue create or comment, a claim, mail. A repeat gets the recorded answer (`Zm-Write-Replayed: 1`); one that arrives while the first runs waits for it. The record is tied to the request (method, path and query, body sha256): a write id reused for another request is `409`, never the old answer. Records sit in `team_writes` (scrubbed on redaction); live ones are kept a day and at most 100k per project, newest first, and an answer over 64 KiB keeps only its status (a replay answers `{}`). A body over 8 MiB is refused whole (`413 too_large`), never cut short. Transient answers (5xx, 401, rescope, busy) are not recorded. Done once in the team module's authority path rather than in each module: one rule for every forwarded write route, including later ones. |
| Whose address limits redemption | `CF-Connecting-IP` is believed only from a loopback peer (cloudflared on the home), and, when `tunnel.edge_secret` is set, only with that secret in `X-Zm-Edge-Secret` (a Cloudflare Transform Rule adds it). Otherwise the peer's own address is the bucket. A process on the home is its owner anyway. |
| Bodies are buffered | Signing an answer needs its whole body, so the home buffers each answer, and the replica reads each answer whole before checking it. The replica's reader reports progress to the hedge's silence timer, so a large answer that is moving is not hedged. A hedge gets a transport with no connection reuse, as on the tailnet. |
| Secrets cross only sealed (2026-11-08) | Before this, a renter's `secrets` and a runner's `jit_config` never crossed the tunnel, so a GitHub Actions host needed Tailscale. Now they cross sealed, hop by hop on each tunnel leg. Key: a second, X25519 key per daemon (`tunnel/seal`), not one derived from the ed25519 key: ed25519 stays a signing key only, and the seal key can be replaced (lost with a restore) without a new identity; the identity certifies it (`Zm-Seal-Key`, inside the signed headers of every request and answer), so a peer takes it only as the identity the token is bound to, or the pinned home, vouches for it. Not escrowed: a restored home certifies a new one, and members take it from the next answer (the old one opened only what was in flight). Crypto: Go's `crypto/hpke` (RFC 9180, base mode, DHKEM(X25519, HKDF-SHA256), HKDF-SHA256, ChaCha20-Poly1305): standard library, no new dependency, a 32-byte key cheap enough to ride every request; one suite named in the certificate, never negotiated. The HPKE info binds sender and recipient key ids, an expiry and the purpose: a member's sealed body is bound to that signed request (its date and nonce, which the door never accepts twice), an op's sealed arguments to the project, op id, binding, verb and box. Where: the member's transport seals the body when the team module asks (`Zm-Seal`, never sent) once it knows the request's nonce, and the door opens it before routing, so nothing behind the door changes; the authority's team module seals a relayed op to the key on the host's own poll or heartbeat (`Remote.SealKey`, set by the door), through `core.Sealer`, and the host opens it before its driver call. Hop by hop, not end to end: the authority already holds both (it mints jit configs, checks names and sizes, routes), and Cloudflare, not the authority, is the party this keeps out; end to end would move those checks to the host (SPEC rule 7: one responsibility per PR). Only a secret-carrying call or op is sealed, and then whole (argv and stdin too): sealing only the two fields let `"Secrets"` (encoding/json folds case) ride beside the blob in the clear (review); either side also refuses such a body unsealed, under any spelling. A host scrubs its driver's refusal of the secrets and jit_config it was sent before the report crosses back (review: a driver quoting them put them on the wire). Old peers: no certificate, no secrets, the earlier `403`; a home whose answers stop carrying one (downgraded) is sealed to no more. Rejected: post-quantum hybrid (ML-KEM-768 + X25519, also in `crypto/hpke`): a 1.2 KB key on every request; the certificate's suite name leaves room for it. |
| Same rules over both | The team module sees an `*http.Client` either way (`core.TeamTransport.Client`): forwarded reads, hedges and their budget, the sync loop, presence and the outbox are one code path. Leak walks and the hedge and sync tests run over both. |

**Threat model.**
- *Cloudflare, cloudflared and the edge* see everything in plain text: tokens, requests, answers, the team's data,
  except a renter's secrets and a runner's jit configs, which cross only sealed. They cannot act
  as a member (no member key), replay or change a request or an answer (signed, nonce, date), or redeem an invite
  they carry (only a proof bound to the member's key crosses). They can read and drop. A team that must not be
  readable by Cloudflare uses the tailnet.
- *Sealed secrets, against a Cloudflare that reads (passive).* It cannot learn a secret's value or name or a jit
  config. It still learns that a call or op carried them, their size within a few bytes (no padding), when, for
  which target, box and verb, and every output, so a command that prints a secret shows it. What it records stays shut unless a recipient's seal key
  leaks later: HPKE base mode's key is ephemeral only on the sender's side, so a stolen `tunnel/seal` (the host's
  or the home's disk) opens what was recorded for it; jit configs are single-use and short-lived, secrets may not
  be. Not post-quantum.
- *Sealed secrets, against a Cloudflare that changes traffic (active).* It cannot put its own seal key in: a
  certificate counts only as signed by the identity the token is bound to (a host's request) or the pinned home (an
  answer), and it rides inside headers those signatures cover. It cannot strip the certificate to get plaintext: a
  missing one is the refusal, never the clear. It cannot move a sealed blob to another request, op, box or host
  (the info binds them, and nonces stop a request's replay), nor stretch its expiry (bound too). It can drop or
  delay. At join nothing changes: the identities sealing builds on are bound as before (the home's fingerprint in
  the invite, the member's key in its redemption proof), so an active Cloudflare that cannot redeem an invite or
  answer as the home cannot get between the keys either; an invite code sent over a channel it reads is as weak as
  before, for sealing too.
- *A hijacked hostname* (DNS taken over, a lapsed quick-tunnel name, a misissued certificate) passes TLS. It cannot
  answer as the home: no answer it signs verifies, so the replica treats the home as unreachable and keeps serving its
  copy, marked stale after 7 days. A token it sees is useless without the member's key, and it cannot redeem the
  invite. It does see what the replica sends while the hijack lasts, and it can relay to the real home and read both
  ways, like Cloudflare. That residual risk is the price of no end-to-end encryption.
- *A network attacker without a certificate* is stopped by TLS.
- *A process on the member's machine as the same OS user* can read the token and the key, as it can read the tsnet
  state today. Binding stops a copied token from working elsewhere; a same-user process is the owner (SPEC 1).
- *A process on the home's machine* can reach the loopback listener; it is the owner anyway, and the listener still
  needs signed requests and tokens. It could name any `CF-Connecting-IP` unless `tunnel.edge_secret` is set; that
  only moves it between rate-limit buckets under the same overall cap.
- *Whoever holds a signed request* (Cloudflare, or a hijacker) cannot send it again: the nonce is remembered for
  longer than the date can be accepted even across a clock step back of MaxSkew, in a journal a restart reloads; a
  journal that could not be read holds writes off until any request signed before the start has expired.
  A write that did get through twice is answered from its write id's record, not run again.
- *Scanners on the internet* get `401` for every path except redemption, which is rate limited and answers every
  code problem the same. Anyone can make a key, so only a request whose token authenticates enters the replay
  cache. Someone sending wrong proofs from many addresses can use up the 50-per-10-minutes budget and hold off
  joins over the tunnel (never over the tailnet); a Cloudflare rate-limit rule or Access removes that.

**Cloudflare Access (optional).** Access can sit in front as a second gate: a service token per member, so the edge
refuses anything without it and the home sees only members' traffic. zm needs none of it. `zm join --cf-access-id`
with `--cf-access-secret-file` stores the pair next to the token (`remotes/<ULID>.headers`, 0600) and sends it as
`CF-Access-Client-Id` and `CF-Access-Client-Secret` on every call; nothing else changes. Access never replaces zm's
tokens: it decides who reaches the home, not what they may see.

Rejected: mutual TLS (Cloudflare ends TLS, client certificates would need Access mTLS and account setup); a hello
challenge before each connection (a relay passes it through, so it proves nothing about later requests); per-chunk
signing of streamed answers (more code for a case the replica never uses); end-to-end encryption of all bodies (a
Noise-style layer is the next step if Cloudflare must not read the data; secrets and jit configs alone are sealed
since the seal change).

Verified (2026-11-01, this Mac, two sandbox daemons in temp homes, neither on a tailnet): a real quick tunnel
(`cloudflared tunnel --url`, a trycloudflare name, no account resources). Unsigned requests, with and without the
member's token, got `401` through the edge; `zm join` with the https code, `zm remotes` showing `via tunnel`, the copy
syncing an item made after the join, a claim the home saw held by the member's session, and the release. So answers
and their signatures survive Cloudflare's edge unchanged. A tunnel name queried before its DNS record is out can be
negatively cached by the local resolver for a minute or so (seen once; `zm join` then says the home is unreachable,
and works on retry). Not measured: latency through the edge, and a named tunnel.

## Team transports: one seam (SPEC 17.11 "Transports", 2026-11-02)

#114 added the tunnel beside the tailnet, and the choice leaked into the team module: a join branched on the code's
shape, every remote's client on `via`, the home's redemption on `Remote.Via`, and the Cloudflare Access header lived in
team code. With a third transport coming (the host's own Tailscale, lane "names"), that would have become three-way
branches in four places. Now a transport is one driver of `core.TeamTransport`, registered once in `base.NewKit` (potluck/wiring)
(`Deps.AddTransport`), and the team module only looks drivers up by `via`.

| question | decision |
| --- | --- |
| What is behind the interface | Everything transport-specific: reading its join code (`ParseCode`), readying this machine (`Prepare`: the tailnet's node up and named), headers a remote carries (`Extra`: the Access token), the redemption body (`RedeemRequest`: the tailnet's code, the tunnel's proof), the pinned client (`Client`: the tailnet's WhoIs pin, the tunnel's signing and verification), what an error means (`Explain`), and on the home what an invite names (`Invite`), the rate-limit bucket (`RateKey`) and the registry input of a redemption (`RedeemInput`). |
| Where drivers live | Next to what they wrap: the tailnet's in `potluck/kit/tailnet` (with `waitNode` and its tunables, moved from team), the tunnel's in `potluck/kit/tunnel` (its errors moved from core). `core.TunnelService` and `Deps.Tunnel` are gone: nothing outside the tunnel package needs them. |
| Invites without `--via` | The registered transports in order (the tailnet first); one answering unavailable (not serving here now) is skipped, any other error is the answer. When none serves, the project's own refusals come first. |
| Limits | One limiter per transport, so noise on one cannot hold off joins on another. |
| Stored headers | `remotes/<ULID>.headers` ("Name: value" lines, 0600), whatever the transport wants sent; was `.access`, nothing shipped with it yet. |
| What stays in team | The join flow (machine name, redemption, token file, access check, checkouts, copy), hedging (a client whose transport has `NoReuse` hedges on that), and the GitHub join's discovery of authorities among the tailnet's `zm-*` peers, which is tailnet by nature until lane "names" moves it behind the seam. |
| Proof it is pluggable | `TestTeamTakesAnyRegisteredTransport` registers a third driver (the tailnet's under another name, with its own code prefix) on a member's daemon: join, the remote's `via`, a claim at the authority, all through it, with no team change. |

## zm tunnel setup: your own domain in one command (SPEC 17.11, 2026-11-02)

Goal: any team puts its join address on its own domain with one command on the home, re-runnable, with nothing to
click but Cloudflare's own login. Built as `potluck/kit/tunnel/cfsetup` (the steps, driving cloudflared's CLI and zm's
routes through small interfaces) and `cmd/zm/tunnel.go` (wiring: the pinned download, the tracker homes' service
manager, the socket).

| question | decision |
| --- | --- |
| Which credentials | Only cloudflared's login certificate (scoped to the zone picked at login: tunnels and their DNS records). It lives under `$ZM_HOME/tunnel/cloudflared/home/.cloudflared` (cloudflared's HOME is set there for every call, so `~/.cloudflared`, and any config in it, is never read or written), 0600, and stays for re-runs and remove. No API token. |
| Cache rule | Not needed: every answer on the listener carries `Cache-Control: no-store`, which the edge honours whatever the zone's cache rules; it is unsigned, so members drop it. |
| Edge secret | cloudflared's ingress sets the Host it sends (`originRequest.httpHostHeader`), so the secret rides as `Host: zm-edge-<secret>` and no Transform Rule is needed; `X-Zm-Edge-Secret` still works for a rule. Status shows when it was last seen; the check waits for it. |
| Bot Fight Mode | The one zone setting the certificate cannot change, and on the Free plan it cannot be skipped per hostname. Off by default on a zone. The check tells a challenge (`cf-mitigated: challenge`, or an HTML 403/503 without zm's key) from zm's 401 and prints the dashboard step. |
| No restart | `POST /v0/tunnel/up` starts the listener live and saves `tunnel.*`, re-reading config.json first (other modules write it); `down` undoes it. The tunnel module now reads its block through an atomic snapshot, as requests read it without a lock. |
| Supervisor | The tracker homes' `home.Manager` (launchd agent, systemd `--user` unit), one label per ZM_HOME (`dev.zm.cloudflared.<fnv32>`). Never root unless `--as-root`; Linux without lingering gets the `loginctl enable-linger` hint. |
| Rollback | Every step is recorded in `state.json`. A failure before the check undoes what that run made, in reverse: the service, zm's listener (if it was off before), the config, and a tunnel it created, unless DNS already points at it (deleting it would break the name; a re-run reuses it). The login stays. A failed check undoes nothing: its cause is outside the machine, and `zm tunnel status` looks again. |
| DNS deletion | cloudflared has no verb for it; remove prints the dashboard step. Remove deletes only a tunnel this machine created; one it reused (made elsewhere) is kept with its record, and only the local half goes (security review). |
| The check is a member's check | The probe sends a nonce and verifies the answer's signature over it with the fingerprint of this home's key (`tunnel.VerifyAnswer`, the transport's own check), so a relayed or replayed answer, or another home, never passes. The edge sighting resets on every `up` and `down`, so a re-run's check needs a fresh one. |
| Secrets off argv | Quick mode writes its own 0600 config (`url`, `http-host-header`) too, so the edge secret is never on argv or in a launchd plist or systemd unit (0644). cloudflared logs its settings, secret included, into `cloudflared.log` in the 0700 setup directory. |
| A changed binary | `tools.Verify` failing for any reason but "missing" stops the command; only a missing cloudflared is fetched. |
| The pin | cloudflared 2026.10.7, pinned by GitHub's own asset digests. The release notes list, for macOS, the sum of the executable inside each archive (checked: darwin-arm64's matches, Developer ID signed by Cloudflare); for Linux, of the bare executables the release ships, so `internal/tools` gained pins with no archive member. |
| Rehearsal | `--quick` (hidden) runs the same service, listener and check on a quick tunnel's temporary trycloudflare name: no login, nothing in any account. `--supervisor process` (hidden) runs it as a plain process with a pid file instead of launchd or systemd. The end-to-end run used both. |

Verified (2026-11-02, this Mac, a sandbox home and member in temp homes): `zm tunnel setup --quick --supervisor
process` downloaded the pinned cloudflared (sha256 ok), started it, and its check saw zm's signed 401 through the
edge with the edge secret arriving as the Host; an unsigned curl got `401` with `cache-control: no-store` and the home
key; `zm invite --via tunnel` printed a code for the trycloudflare name; a member joined with it and its copy synced;
`zm tunnel remove` stopped cloudflared by its pid file and the listener. Not exercised here: the named-tunnel path
against a real account (login, create, route; unit-tested with a fake cloudflared), launchd and systemd (the tracker
homes' manager, tested there).

## Sessions survive daemon restarts (session holders, 2026-11-01)

Problem: the daemon owned every managed session's PTY, so `zm daemon stop`, a crash or an upgrade ended them all
and released their bound leases (three times on one Mac on 2026-11-01). tmux and cmux keep terminals alive across
their UI restarting because a separate process owns the PTY. zm does the same.

- **One holder process per session.** The daemon starts `zm __zm-holder <home> <session8>` (the same binary, from
  `os.Executable()`) in its own session (`setsid`). The holder owns the PTY, the child (it is the parent that
  waits on it) and the 1 MiB scrollback ring, and serves one Unix socket, `$ZM_HOME/holders/<session8>.sock` (0600,
  directory 0700). It has no network listener and reads no config. It exits only when its child exits (after
  handing the exit over) or when it is told to stop the child. Per session, not one host for all: a crash or a
  bug takes down one session, and an upgrade needs no handover (old holders keep running their old binary until
  their sessions end; new sessions get the new one). Measured on an M3: about 6 MB of physical footprint per holder
  (RSS shows 22 MB, most of it the zm binary's shared clean pages), and a spawn takes 60-75 ms end to end with the
  holder's exec. A dedicated small holder binary would save little, and would be a second file to install and swap.
- **Written here, on creack/pty (already pinned), for a stated reason** (bricks rule 4). tmux, dtach and abduco are
  not on a stock Mac; dtach and abduco keep no scrollback; tmux re-emulates the terminal, keeps lines rather than
  the raw bytes, and has no exit status hand-off. zm needs the raw byte stream with absolute offsets (attach replay,
  `output?since=`), the wait status (typed outcomes) and the PTY's foreground group. The holder is about 400 lines.
  The second driver we expect is one long-lived PTY host for every session (less memory, but one crash or upgrade
  takes all sessions with it); the protocol does not change for it.
- **The daemon keeps its code paths.** The holder passes the PTY master to the daemon over the socket
  (`SCM_RIGHTS`). The daemon writes input, resizes and reads the foreground group on that descriptor exactly as
  before, so keystroke latency does not change. Only the holder reads the master; it streams output to the daemon,
  which keeps a mirror of the ring at the same offsets, so attach, replay, `output`, outcome detection, titles and
  the idle timer are unchanged. Measured under load (M3, load average 20-30), 5 interleaved runs each: keystroke
  echo through an attach p50 64-197 µs with holders against 57-99 µs before, p95 0.1-3.2 ms against 0.1-2.2 ms;
  attach to the first replayed byte p50 0.13-0.25 ms against 0.17-0.26 ms (budget 100 ms). The extra hop costs
  tens of microseconds, far under a frame.
- **Protocol `zm-holder/1`**: frames of `type u8, length u32, payload`. On accept the holder sends `H` (JSON hello:
  protocol major and minor, zm version, session8, home, holder pid, child pid, ring bounds). That first frame never
  changes shape; fields only grow. The daemon adopts with `A {since}`; the holder answers `F` carrying the master
  descriptor, then `O` frames (8-byte offset plus bytes) from `since`, and `X` (exit: code or signal, total bytes,
  who asked it to stop) after the last byte. The daemon sends `S {signal, by}` to stop and `K` once the exit is
  recorded. Unknown frame types are ignored both ways, so a minor bump is additive. A different major is not
  adopted: the session keeps running, `zm status`, `zm ls` and a cockpit say why (`unreachable`), and the daemon tries
  again every 15 s (a downgrade, or its own upgrade).
- **Adoption checks who it talks to.** On start the daemon connects to the socket of every managed session that is
  not ended. It checks: the socket is a socket owned by this user with no group or other bits, in a 0700
  directory; the peer's kernel credentials (uid is ours, pid is the hello's holder pid); the hello names this
  home and this session; the child pid is the row's `pid` and its parent is the holder. Then it replays the ring
  into its mirror and waits for it to catch up before serving, so the first attach after a restart replays
  everything. Replayed bytes do not count as new output (no idle wake) and never get a cursor-query answer.
- **Exits while no daemon listens are kept.** The holder writes `holders/<session8>.exit` (JSON: wait status, total
  bytes, stop) and `sessions/<session8>.out` (the ring) before it reports the exit. A daemon that starts and finds
  the exit file but no holder records the real outcome, not `infra`. No socket and no exit file (a holder that was
  killed, or a session from a zm before holders) is `infra`, note `daemon restarted`, as before.
- **Leases.** Nothing about the session changes on a restart, so its bound lease stays with it; the work ticker
  renews it again once the new daemon runs. A restart shorter than `bound_lease_ttl_seconds` loses nothing.
- **Stopping goes through the holder** (`S`), which signals the child's process group; if the holder is gone the
  daemon signals the group itself. With safe swaps (#108), `zm daemon stop`, `restart` and `upgrade` refuse only for
  `RestartBlockers`, which is now empty when the daemon keeps sessions: they let a swap through, and the first stop
  of a daemon from before holders (all its sessions end) stays guarded. `--end-sessions` (stop: `--force`) still ends
  them (TERM, then KILL after 2 s, outcome `infra` "daemon stopped"). An upgrade whose new daemon reports
  `holders.unreachable` fails its verify and rolls back, so the previous build can adopt those sessions. Verify reads
  health again after its probe: `GET /v0/health` skips the start gate (#137), so the first answer can predate adoption.
- **Tests and other in-process daemons tie their holders.** Only `zm daemon` keeps sessions on exit. Every other
  user of the session module (unit tests, `potluck/kit/server` tests) starts holders tied to it: the holder keeps
  its stdin pipe from the daemon and ends the child when it reads EOF, so a test that dies leaks no process.
- **Orphans are reported, never killed.** A holder whose home is gone (a moved or deleted `ZM_HOME`) cannot be
  reached by any daemon. `zm status` lists those it can see (same user, `__zm-holder` in argv, home missing) with
  the pid to stop by hand.
- **An embedding client needs no change to reattach.** `zm attach --embed` already exits 3 when the stream drops with the session
  live, and a cockpit keeps the old screen, re-attaches off screen with a full replay and sends the keys typed
  meanwhile. Before this change the reattach found the session ended. A cockpit can also show `unreachable`.
- **A dropped stream is not an end.** If the holder's stream ends with no exit file while the holder process lives
  (it hung up on a daemon that stalled past its 30 s write deadline, or the socket broke), the session stays
  running, its socket stays, and the daemon adopts it again at once (then every 15 s while it fails). A mirror that
  starts after the holder's ring moved on keeps a floor, so a replay never serves bytes it never got.
- **Under a service manager (2026-11-02).** `setsid` leaves the process group, not the cgroup. launchd's kill follows
  the group, so a holder survives `kickstart -k`, `zm daemon upgrade` and `bootout` (run for real on a throwaway job).
  systemd's default kills the cgroup, so the guard said "safe" and systemd then killed the session (shown on real units
  on the CI VMs, `TestSessionsUnderSystemd`). Fixed with `KillMode=process` in the unit, plus a guard that reads the unit's
  KillMode, because zm writes no unit for its daemon and homes already run hand-written ones. Rejected for now: a
  holder moving to its own scope (`systemd-run --scope`). It needs no unit edit and takes agents out of the daemon's
  `MemoryMin` and `OOMScoreAdjust`, but it needs a reachable user bus, has no answer for a system unit run by a
  non-root user, and adds a path to fall back from.
- **The first restart onto this version still ends sessions**: a daemon from before holders owns their PTYs in its
  own address space, so there is nothing to adopt. Every later restart or upgrade keeps them.

## Session usage attribution (SPEC 8 "Usage", 2026-11-05)

The Mezze app's Usage page splits tokens by person, job and Claude account. It could only guess those from titles and
cswap's switch log on one Mac, and it had no tokens for another machine's session. So zm records them where they
are known, on the machine that ran the session, and presence carries them like any other session field.

- **Recorded, never inferred.** `for` and `job_kind` come from whoever starts the session, and the account from
  the config the session starts with. cswap switches by rewriting `~/.claude.json`, so a later read would give the
  wrong account to a long session.
- **Tokens are a tail, not a scan.** The loop keeps each live transcript's offset and seen message ids in memory,
  so a pass reads only what was appended. A restarted daemon reads each live transcript whole once. The only state
  on disk is the total.
- **No event for tokens.** A token total changes every few minutes for every running agent. Publishing it would
  fill each project's 20 000-event log and wake every UI. The row keeps it, and reads and heartbeats carry it.
- **Work stays.** Before this, `work` was the live bound lease, so almost every ended session had none. The last
  item a session held is what a reader wants to know afterwards. While it holds one, it is still the held one.
- **The user's home comes from `config.Load` only.** Tests build configs with `LoadFrom` or by hand, so they never
  read the real `~/.claude.json` or transcripts.

## The Mezze app door (SPEC 18.4, 2026-11-08)

A Mezze app webhook task needed a public URL that works wherever the task's Mezze app runs. Until now it got one only through T3
Connect, or a hand-set base on the automations home.

- **At the authority, not in the Mezze app.** The authority already has a public tunnel host and is the one place every
  member machine dials. One URL per task there survives the task's Mezze app sleeping, and the queue sits on the always-on
  side.
- **The Mezze app keeps the signature check.** It already verifies HMAC, filters and dedupes replays. zm forwards the body and
  the sender's headers unchanged to the task's own hook URL, so that logic is not copied and zm never holds a task's
  secret. The door's own credential is the URL token, and its dedupe on the delivery id only keeps its queue small.
- **The machine-op queue, not a new channel.** Members only dial out. A `door_delivery` op rides the same poll and
  heartbeat answers as `run_target`. Its body stays in memory and out of `team_ops`. The door keeps the durable queue
  and retries, because an op expires in a minute and a Mac can sleep for a weekend.
- **Loopback and `/api/hooks/` only.** The authority tells a member where to post. Checking the target on both sides
  stops a door task from becoming a way to reach anything else on that machine.
- **Its own listener.** The tunnel's door refuses unsigned requests by design. As with the runner broker, cloudflared
  routes just the hook path to a loopback listener that serves nothing else.

## Sandbox internet (SPEC 20 "Boxes", Network, 2026-11-08)

It was ruled on 2026-11-08 that zm sandboxes must match Celesto's, internet included. Before this, an oci box got
the binding's filtered network only when it brought a `jit_config`; a sandbox or job got network `none`, so it
could not reach Claude's API, npm or GitHub.

- **The daemon decides, the driver obeys.** Trust is the authority's record (section 8 class); a host does not
  know its own class. So zm sends acquire's `net` (`on` for a trusted host's box, `off` for a public host's or
  when asked) and `zm-run-tart` makes the box on the binding's network or on `none`. A renter cannot ask for
  `on` on a public host: the daemon computes it, and a host replica relays the authority's args unchanged. A
  malicious authority could send `on` to a public host, but it can already send that host any work it likes.
- **Capability, not version.** A driver lists `net-off` when it honours `net`. Without it zm sends nothing, as
  before (Tart keeps Softnet egress), and refuses `off` before the call, since an older driver would ignore it
  and give the box a network the renter turned off. Placement skips such a binding for `--net off`.
- **Unsaid keeps the old rule.** A box acquired with no `net` (an older daemon, a conformance run) is made as
  before (only runner boxes on a runner binding get the network), so a mixed fleet never widens by accident.
- **Public hosts: none, not an allowlist.** An allowlist (GitHub, registries) needs a name-based egress filter
  zm does not have (the `zm-box` filter is iptables by address). Until one exists, a public host's oci box has
  no network; that is open question 6's default.
- **Checkout stays host-fetched.** A runner binding still fetches its checkouts on the host (`fetch=guest`
  refused), and a box with `net` `off` always does. A networked guest fetching for itself would buy nothing: the
  renter's code arrives as a bundle through `exec`, and the binding's own remote is fetched by the host.
- **Fail closed.** With the `zm-box` unit inactive the driver refuses a networked box (`unavailable`, naming
  `--net off`) instead of quietly making it with no network.

## Automation records (SPEC 18.6, 2026-11-16)

The decision and its reviews are docs/decisions/2026-11-15-automations-as-zm-records.md; these are the build's own
choices (S3-09).

- **Its own brick, not an issue type.** Health posts would write issue history and events, automations would show in
  `bd ready` and the issue benches, and the home rule cannot sit on `PATCH /v0/issues/{id}`.
- **One table set per project.db, at the authority only.** No replica copy: a member's every call goes to the
  authority (`NoCopy` rows), and an item route, whose id names no project, is asked of each joined project while it
  answers 404, as a native issue is. Writes go through the project's writer (a team project's one connection).
- **The store owns what must be atomic.** The rev compare-and-set, a write id's single effect (a unique index on the
  write id over revisions), history bounds, and the claim's holder and wanted install all live in one driver
  transaction, so P-1's retried effects and a member's retried forward cannot double-apply. The module owns who may
  write and what is valid; a driver never decides either.
- **The home rule reuses two existing checks.** "The caller's own machine" is the door's rule (socket owner, or a
  node-bound token's machine), and "not from an agent session" is the origin gate key answers already use
  (`orchcore.OriginChecked`, moved out of approvals with the refused thing's text as a parameter). No new token kind.
- **No export filter (evidence gap 2).** A project export is an admin's, of the whole project; records hold no
  secret (signing secrets are refused) and nothing one person owns, so they travel whole. The off-site home sets copy
  every `project.db` (`internal/homes/offsite_home.go`), so backups cover them with no change. **Area filter (gap 3):**
  as usage and grants do, `d.Store.Project` for the file and `Scope.AreaSQL` on the record's `area` column.
- **A redaction does not move `rev`.** It is not a definition write, and moving `rev` would make the home re-apply a
  change no home made. The home's copy is the Mezze app's.
