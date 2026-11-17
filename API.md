# Mezze HTTP API (zm/v0)

Served on `$ZM_HOME/zm.sock` (and, with `zm tailnet up`, on port 7433 of the tailnet). JSON in and
out; the base types are in `potluck/kit/api`, the orchestration ones (work, mail, sessions, tasks, issues,
approvals, trackers, webhooks) in `internal/orchapi`. Every request sends `Potluck-Protocol: potluck/1`;
every response carries it back; a different major is refused (section "Headers"). Each `Zm-*` header below is
sent as `Potluck-*` (`Zm-As` as `Potluck-As`, ...) and read under either name, the Potluck one first; `zm/v0` is
read as the same major as `potluck/1` (Potluck rename).

**Guards.** Each route has a coarse guard enforced by `core.Router` and a fine check the handler does:

- `public`: anyone, even unauthenticated network callers. Returns nothing scoped.
- `authed`: an authenticated caller: the OS user on the socket, a bearer token, or a mapped tailnet login. A
  login without a token is ambient, so core also requires its `Host` to name this node exactly, nothing
  cross-site (`Sec-Fetch-Site`, `Origin`), and `Zm-Protocol` on every write; else `401` (SPEC section 1).
- `local`: the machine owner on the socket **without a token**. Remote and token callers get `403`.
- `device`: a tailnet device identified by WhoIs (stable node ID + node key), with **no token**; the socket
  and any token get `403`, a network caller without a WhoIs node `401`. The handler then requires the node to
  be a paired approver (approvals, SPEC section 16).
- Fine checks (`authz` actions `read | write | operate | admin` on a project or area): an object the caller
  cannot `read` is `404 not_found`, byte-identical to a missing one; `403 forbidden` only when the caller can
  see the object but its role does not allow the action. Lists return only what the caller may read.
- "ReadScope" = every project the caller sees, or the one named by `?project=` (or `--project`).
  "WriteTarget" = the resolved project/area for a write (SPEC section 5), checked for the action.

**Stability.** Every route row carries one label (how clients use them: `docs/clients.md`):

- `stable`: the client contract. Within `potluck/1` it changes only by adding (a new optional field, value or
  route); a removal, rename or change of meaning is a break and gets a "Clients" line in `CHANGELOG.md`.
- `beta`: callable by clients, but its shape may change in any release. A change to a documented field still gets
  a "Clients" line.
- `app`: exists for the Mezze app and changes with it. Third-party clients should not call it.
- `internal`: called only by zm itself, another zm daemon, the daemon's own phone pages, or an outside service's
  webhook or sign-in callback. No promise; never call it from a client.

`internal/wiring/fulltest/routes_test.go` fails if a registered route has no row here with one of these labels,
or if the `stable` routes here and in `docs/clients.md` differ, and
`internal/wiring/fulltest/leak_test.go` calls every route as a guest (SPEC section 6). The fixture seeds with the
JSON request bodies shown here, so these bodies are the frozen contract for seeding: `POST
/v0/sessions/register {agent, runtime_id, title}`, `POST /v0/work {title, area}`, `POST /v0/mail {to, body,
area}`, `POST /v0/leases {key}`, `POST /v0/work/{id}/claim {}` must keep working as written.

## Headers

| header | direction | meaning |
| --- | --- | --- |
| `Zm-Protocol` | both | `potluck/1` (sent as `Potluck-Protocol`; `zm/v0` is the same major); a different major is refused (`400` from the daemon, `unavailable` in the client); required on writes by a tailnet login without a token |
| `Authorization` | request | `Bearer potluck_...` (a token from `zm token mint`; older `zm_...` tokens still work) |
| `Zm-As` | request | the caller's reply address: a session on this machine or this machine's human (socket); must be empty or the token identity's address otherwise. Grants nothing |
| `Zm-Project` | request | explicit project: `slug`, `ULID` or `slug/area` (from `--project` or `$ZM_PROJECT`) |
| `Zm-Project-From` | request | `flag` (default) or `env`; only `flag` narrows lists |
| `Zm-Cwd` | request | the client's working directory, for resolution; socket only, ignored from the network |
| `Zm-Scope-Version` | both | a team replica's last seen `scope_version` (request) and the identity's current one (response, to node-bound tokens); a stale one is `409 rescope` |
| `Zm-Team` | response | to a node-bound token only: the project header the replica caches (base64url JSON `TeamHeader`) |
| `Zm-Write-Id` | request | a replica's write id (ULID) on a queued or live forwarded write, from a node- or key-bound token: the authority runs it once |
| `Zm-Write-Replayed` | response | `1`: the answer recorded for a write id that already ran |
| `Zm-Key`, `Zm-Date`, `Zm-Nonce`, `Zm-Sig` | request | the tunnel listener only: the caller daemon's signature (Tunnel, SPEC 17.11) |
| `Zm-Home-Key`, `Zm-Home-Sig` | response | the tunnel listener only: the home's signature over the answer |
| `Zm-Recovery` | both | the tunnel listener's signed pre-dispatch `503` challenge after replay history is lost; a capable member retries once with the same write id and body, a fresh nonce, and this signed challenge (SPEC 17.11); grants no authority |
| `Zm-Stop-Signals` | response | `POST /v0/sessions/{id}/stop`: the signals sent, in order (`TERM, HUP`) |

Errors: `{"error": {"code": "not_found", "message": "no project \"x\"", "hint"?: "...", "data"?: {...}, "retry_after"?: 60}}`
(`retry_after`: seconds to wait, a tracker's rate limit);
codes `bad_request` 400, `unauthenticated` 401, `forbidden` 403, `not_found` 404, `conflict` 409, `too_large` 413
(a body over a route's limit; nothing ran),
`rescope` 409 (a team replica's identity changed grants since the `Zm-Scope-Version` it sent), `not_implemented`
501, `unavailable` 503, `internal` 500.

## Core (integrator)

| route | stability | guard | check |
| --- | --- | --- | --- |
| `GET /v0/health` | stable | public | none; machine for everyone (the tailnet hostname says it anyway), pid, started, hedges, backup, heartbeat, holders, drivers and card_judge only for local callers |
| `GET /v0/whoami` | stable | authed | none: describes the caller |
| `POST /v0/daemon/stop` | internal | local | refuses while managed sessions that would end run (none once every session has a holder) unless `force`, and while `swap.lock` is held unless `swap` is its nonce |
| `GET /v0/daemon/manifest` | internal | local | - |
| `POST /v0/daemon/swaps` | internal | local | the caller is recorded as `by` |
| `GET /v0/events` | stable | authed | per event: read on (zmproject, zmarea); hidden mentions redacted |
| `GET /v0/projects` | stable | authed | projects the caller sees; areas, repo rules and trackers filtered |
| `POST /v0/projects` | beta | local | the caller's identity becomes owner |
| `GET /v0/projects/resolve` | stable | authed | only visible projects are candidates |
| `GET /v0/projects/{project}` | stable | authed | read on the project, else not_found |
| `PATCH /v0/projects/{project}` | beta | authed | admin on the project; `repos`/`trackers` only from the machine owner on the socket (no token) |
| `GET /v0/projects/{project}/areas` | stable | authed | read on the project; only visible areas |
| `POST /v0/projects/{project}/areas` | beta | authed | admin on the project; `paths`/`remotes` only from the machine owner on the socket (no token) |
| `GET /v0/projects/{project}/areas/{area}` | beta | authed | read on the area; a hidden area or project is not_found, like a missing one |
| `PATCH /v0/projects/{project}/areas/{area}` | beta | authed | read on the area, else not_found; admin on the project, else forbidden; `paths`/`remotes` only from the machine owner on the socket (no token) |
| `POST /v0/projects/{project}/export` | beta | local | admin on the project |
| `GET /v0/grants` | beta | authed | admin on each listed tuple's object; `?project=` must be visible |
| `POST /v0/grants` | beta | authed | admin on the project/area |
| `DELETE /v0/grants` | beta | authed | admin on the project/area |
| `GET /v0/identities` | beta | local | - |
| `POST /v0/identities` | beta | local | - |
| `GET /v0/tokens` | stable | local | - |
| `POST /v0/tokens` | stable | local | admin on the project/area (adds the grant) |
| `DELETE /v0/tokens/{id}` | stable | local | - |

```
GET /v0/health
<- {"ok": true, "protocol": "potluck/1", "version": "v0.1.0", "commit": "abc1234",
    "machine": "mini", "pid": 4242, "started": "2026-10-30T17:00:00Z"}      (machine/pid/started: local only)
   a team replica that has forwarded hedged reads adds (local only; SPEC 17.3):
   "hedges": {"reads_1m": 412, "hedges_1m": 2, "refused_1m": 0, "reads": 9120, "hedges": 31, "won": 29, "refused": 0}
   session holders (local only; SPEC 8 "Daemon restarts"), when any runs or needs a person:
   "holders": {"adopted": 3, "unreachable"?: [{"session": "9c0e11aa", "why": "its terminal is held by ..."}],
               "orphans"?: [{"pid": 5120, "session": "4bb7545a", "home": "/Users/ana/old-zm"}]}
   run targets whose driver the daemon cannot run, as of its last check (local only; SPEC 20, "Drivers are pinned"),
   a warning that fails nothing (zm status exits 0):
   "drivers": {"checked": "...", "refused": 1, "problems": ["driver acme/box: run target \"box\": driver ref ... re-pin with: zm driver pin ref"]}
   ask cards the card judge has held over 15 minutes (`ZM_JUDGE_NOTICE_AFTER`; local only; SPEC 16 "Card judge"), a warning (zm status exits 0):
   "card_judge": {"held": 2, "since": "...", "reason": "card judge: claude: exit status 1: not logged in"}
   the self-check and the last swap (local only; SPEC 2, "Swaps"); "ok": true stays: the daemon runs:
   "selfcheck": {"ok": false, "checked": "...", "problems": ["issues shop: served before, missing now (the previous build ...)"]},
   "last_swap": {"at": "...", "by": "mini/4bb7545a", "what": "upgrade", "from": "v0.1.0 (abc1234)", "to": "v0.1.1 (def5678)",
                 "exe": "/Users/ana/go/bin/zm", "result": "ok" | "rolled_back" | "rolled_forward" | "failed", "forced": false, "overridden": [], "reason": ""}

GET /v0/daemon/manifest
<- {"build": {"protocol": "potluck/1", "version": "...", "commit": "...", "routes": ["GET /v0/health", ...],
              "proxied": ["GET /v0/issues", ...], "schema": {"registry": [...], "project": [...], "issues": [...], "replica": [...]}},
    "exe": "/Users/ana/go/bin/zm", "args": ["daemon"], "used": ["GET /v0/issues", ...], "joined": ["shop"],
    "applied": {"registry.db": [...], "projects/01J.../project.db": [...], "replicas/01J....db": [...]}}

POST /v0/daemon/swaps    -> {"what": "stop|restart|upgrade|update", "from", "to", "exe", "result", "forced", "overridden", "reason"}
<- the record with "by" (the caller) and "at"; stop/restart/upgrade logged at warn, published as dev.zm.daemon.swapped, kept in swap-last.json
what=update publishes dev.zm.daemon.update_checked (result held|unchanged|failed|ok); does not replace swap-last.json or health last_swap

GET /v0/whoami
<- {"as": "mini/4bb7545a", "kind": "session", "identity": {"id": "01J...", "name": "ana", "kind": "human"},
    "via": "local", "session": "mini/4bb7545a", "machine": "mini", "protocol": "potluck/1", "version": "v0.1.0",
    "commit": "abc1234", "home": "/Users/ana/.zm", "socket": "/Users/ana/.zm/zm.sock"}   (home/socket: local only)
   "limit"?: the route limit of the caller's token (see POST /v0/tokens); absent for every other caller

POST /v0/daemon/stop        -> {"force": false, "swap": "<swap.lock nonce>", "what": "upgrade", "dry_run": false}
<- {"stopping": true, "managed_sessions": 2, "ended": false}           (dry_run: the guards only, "stopping": false)
   managed sessions keep running in their holders and the next daemon adopts them (SPEC 8). With force,
   "ended": true and managed_sessions is the number actually ended; not_ended lists {session, reason}
   for each one that could not safely be stopped, including what the operator can do.
   409 conflict "2 managed sessions are running; ...: mini/4bb7545a (claude), ..." unless force, only for sessions a
       restart would end (a daemon from before holders); data {"sessions": [...]}
   409 conflict "a restart of this daemon is in progress: <by> since <time> (pid N)" while swap.lock is held and swap is not its nonce

GET /v0/events?types=dev.zm.mail.,dev.zm.work.&subject=&project=shop&since=<event id>|0&live=0
    Last-Event-ID: <event id>       (wins over since; absent and no since = live only)
    A reader that falls behind gets ": overflow <last id>" and the stream ends: reconnect with
    Last-Event-ID (client.Events does). Events are never dropped silently. When the daemon stops, live
    streams end with ": daemon stopping". A live-only stream (no since, or since=now) starts, right after
    the banner, with ": cursor <event id>" (minted just after the subscription, so every later event is
    both on the stream and replayable): a reader cut off before its first event resumes with
    Last-Event-ID: <that id> and misses nothing. Some events are live-only (an activity-only
    dev.zm.session.updated, SPEC 12): streamed, never replayed; their ids are valid cursors.
    On a replica, a joined project named here is streamed from its authority, for this machine's token (over a
    tunnel: replay only, as the tunnel door serves no live stream). With none named, live=0 replays this machine's
    events then each joined project's: several logs, so their ids are not one cursor. A stream that cannot be whole
    ends with ": error <why>" and a dropped connection, never a short stream that looks complete; a project this
    machine is out of is left out, as in the lists. A live stream with none named stays this machine's. A caller's
    access is rechecked at each ping: access that changed or ended ends the stream the same way.
<- text/event-stream:
   id: 01JA0M2C9Q3T4V5W6X7Y8Z9A0B
   event: dev.zm.mail.sent
   data: {CloudEvents envelope, see SPEC section 12}
   (": ping" every 15 s)

GET /v0/projects
   local=1 returns only the caller-scoped local registry, without joined-authority discovery.
<- {"projects": [{"id": "01J9Z...", "slug": "shop", "name": "Shop", "paths": ["/Users/ana/code/Shop"],
    "remotes": ["github.com/example-org/Shop"], "areas": [{"name": "solver", "paths": [...], "work": ["bd:solver-*"]}],
    "trackers": [{"adapter": "native", "prefix": "bd"}], "role": "owner",
    "created": "2026-10-30T17:00:00Z"}]}
   (an area guest sees only its areas and their trackers; no project-level paths/remotes)

POST /v0/projects           -> {"slug": "shop", "name": "Shop", "paths": ["/abs/path"], "remotes": ["github.com/o/r"]}
<- Project (role "owner")    409 if the slug exists

GET /v0/projects/resolve    (uses Zm-Project, Zm-Project-From, Zm-Cwd, the calling session, default_project)
<- {"project": {"id": "01J9Z...", "slug": "shop"}, "area": "solver", "rule": "cwd-path",
    "steps": [{"rule": "cwd-file", "input": "/Users/ana/code/Shop/solver", "result": "no .zm/project.toml"},
              {"rule": "cwd-path", "input": "/Users/ana/code/Shop/solver", "result": "shop/solver"}]}
   (no match: "project" absent, every step listed)

GET /v0/projects/{project}                  <- Project
PATCH /v0/projects/{project}                -> {"slug"?: "...", "name"?: "...", "repos"?: {"paths": [], "remotes": []},
                                                "trackers"?: [Tracker], "guest_issues"?: true}   <- Project
GET /v0/projects/{project}/areas            <- {"areas": [Area]}
POST /v0/projects/{project}/areas           -> {"name": "solver", "paths"?: [], "remotes"?: [], "work"?: ["bd:solver-*"],
                                                "labels"?: ["solver"]}          <- Project
GET /v0/projects/{project}/areas/{area}     <- {"project": {"id": "01J9Z...", "slug": "shop"}, "name": "solver", "paths": [...],
                                                "remotes": [...], "work": [...], "labels": [...], "role": "guest", "trackers": [Tracker]}
                                               (every list is an array, [] when empty; trackers: the area's own, plus
                                               project-level ones whose items reach it (the area has labels or a work glob
                                               names the prefix, as in work sources; adapter and prefix only), or all
                                               project-level ones for whole-project readers)
PATCH /v0/projects/{project}/areas/{area}   -> {"paths"?: [], "remotes"?: [], "work"?: [], "labels"?: []}   <- the same AreaView
                                               (a present list replaces that rule, [] clears it; applied at once, no restart;
                                               event dev.zm.project.area_updated)
   (PATCH project, POST/PATCH areas and POST/DELETE trackers rewrite project.toml: 409 "edited by hand since the
   daemon loaded it; restart the daemon" when the file is not what the daemon last wrote, and nothing changes)
POST /v0/projects/{project}/export          -> {"dir": "/abs/new/dir"}
                                            <- {"files": ["/abs/new/dir/project.db", "/abs/new/dir/project.toml"]}

GET /v0/grants?project=shop
<- {"grants": [{"identity": "sam", "role": "guest", "project": "shop", "area": "solver",
    "object": "area:01J9Z.../solver", "created": "..."}]}
POST /v0/grants             -> {"identity": "sam", "role": "guest", "project": "shop", "area": "solver"}   <- Grant
DELETE /v0/grants           -> {"identity": "sam", "project": "shop", "area": "solver"}                   <- Grant
                               (409 for the machine owner's own project grant)
                               A removal or lowering also lowers, to the same role, the grants above it of every other
                               identity that is or was linked to the same GitHub account (never the machine owner's;
                               a whole-project act takes their area grants too; Potluck #179): "also_lowered": [names]

GET /v0/identities          <- {"identities": [{"id": "01J...", "name": "bob", "kind": "human",
                                "tailscale_login": "bob@example.com", "created": "..."}]}
POST /v0/identities         -> {"name": "bob", "kind": "human", "tailscale_login": "bob@example.com"}   <- Identity

GET /v0/tokens              <- {"tokens": [{"id": "01J...", "identity": "sam", "project": "shop", "area": "solver",
                                "role": "guest", "note": "", "created": "...", "expires"?: "...", "revoked"?: "...",
                                "last_used"?: "...", "limit"?: "answer", "device"?: "ana-laptop.example.ts.net"}]}
POST /v0/tokens             -> {"as": "sam", "role": "guest", "project": "shop", "area": "solver",
                                "ttl_seconds": 86400, "note": "solver review", "no_grant": false,
                                "limit"?: "answer", "node"?: "ana-laptop"}
                            <- {"token": Token, "secret": "potluck_...", "device"?: {"name", "node_id", "login"?, "os"?}}
                               (the secret is shown only this once)
   limit/node (optional, a limited token): "limit" names a route limit; the token then reaches only the routes
   that admit it, for the caller's own identity ("as" empty, no grant added; needs "project", "node" and
   "ttl_seconds"). "node" names the tailnet device it works from, and only from; the token carries it as
   "device" and the response's "device" says which device that was. Only the machine's owner on its own socket mints
   one, never a session: from inside a registered agent session it is 403 (the socket's origin check, failing
   closed; a body the mint handler cannot read is 400). zm's one limit is "answer" (zm token mint --answer-only):
   GET /v0/whoami, GET /v0/asks, GET /v0/asks/{id}, GET /v0/decisions (asks only), POST /v0/asks/{id}/choose and
   GET /v0/receipts/verify admit it, and every other route answers it 403 with data {"reason": "token-limit"}
   before anything is looked up (SPEC 4, 16 "Answer-only tokens"). Any other limit is 400. Refused too: "as",
   another "role" than owner, an "area", no "ttl_seconds" or one over limited_token_max_ttl_seconds (default 30
   days), an identity without a whole-project owner grant (403), a device that is not a person's own tailnet
   peer (400; 404 unknown, 409 ambiguous, 503 no tailnet). It authenticates only over the tailnet from that
   device; elsewhere, revoked, expired or once its identity stops owning the project it is 401.
   A mint without "limit" and "node" is unchanged.
DELETE /v0/tokens/{id}      <- Token (revoked set; idempotent)
```

## Mail (mail builder)

| route | stability | guard | check |
| --- | --- | --- | --- |
| `POST /v0/mail/imports` | beta | authed | local owner or aware node-bound human self-import; explicit joined project; WriteTarget; bounded recovery artifact |
| `POST /v0/mail` | stable | authed | write on WriteTarget (area: explicit, else the ref's, else resolved); recipient and ref visible to the sender |
| `GET /v0/mail` | stable | authed | ReadScope; rows by AreaSQL; in read-only (guest) areas, owners-mailbox and needs=human mail only to its parties |
| `GET /v0/mail/inbox` | stable | authed | the caller's mail across ReadScope, grouped by project; `box=reports`: the reports filed for a human caller (none for agents) |
| `GET /v0/mail/inbox/peek` | beta | authed | the same as the inbox, and no announcement is marked seen |
| `GET /v0/mail/sent` | stable | authed | mail the caller sent, across ReadScope |
| `GET /v0/mail/read-marks` | beta | authed | ReadScope(`?project=`); only the caller's own marks (identity from the subject, never the request); `[]` for a session |
| `PUT /v0/mail/read-marks` | beta | authed | read on `?project=` (required), else not_found; only the caller's own marks; a session is 403 |
| `GET /v0/mail/triage` | beta | authed | read on the whole resolved project (an area guest is forbidden) |
| `PUT /v0/mail/triage` | beta | authed | admin on the resolved project, whose file this daemon holds |
| `GET /v0/mail/notice` | beta | authed | read; never an error |
| `GET /v0/mail/{id}` | stable | authed | read on the mail's project/area (guest areas: restricted mail only to its parties), else not_found |
| `POST /v0/mail/{id}/ack` | stable | authed | write on the mail's area and party to it; needs=human: human callers only; never an undecided ask's mail, never a for-you item |
| `POST /v0/mail/{id}/got-it` | beta | authed | read on the mail, else not_found; only the person it is addressed to (`human:<name>`, never a session) and only a for-you item |
| `POST /v0/mail/{id}/reply` | stable | authed | write on the mail's area and party to it; never an undecided ask's mail; never takes a for-you item |
| `GET /v0/mail/{id}/thread` | stable | authed | read; mails the caller cannot read are omitted |

```
POST /v0/mail      -> {"to": "bd:solver-3x", "body": "PR #7 is up", "area"?: "solver", "ref"?: "bd:solver-3x",
                       "kind"?: "note|announce|nudge|report", "needs"?: "any|agent|human", "reply_to"?: "<mail id>",
                       "audience"?: "participants", "client_request_id"?: "<stable retry key>"}
<- Mail (SPEC section 9), plus "note" when the daemon filed it as a report (an agent's reply to a person, SPEC 9
   "Reports": "people take decisions only: use zm ask new. This mail was filed as a report ...")
   400 no project / bad address / a report not to a person / an agent's fresh note to a person ("not sent: ...
   zm ask new"; nothing stored); 404 hidden or missing recipient or ref; 409 work item
   with no holder or creator
   "to": "human:a,human:b" (from a person; 2 to 8 people besides the sender, deduped): one private two-party copy per
   person, each with "members" (sender and recipients, sorted) and "batch" (the first copy's id); <- the first copy.
   Every copy is checked first: any refusal (400 not a person / wrong count / not sent by a person, 404 unknown
   person) stores nothing. client_request_id is kept per copy as <id>/<recipient>.

GET /v0/mail?project=&to=&from=&ref=&needs=&open=1&since=&until=&limit=50     <- {"mail": [Mail]}   since/until: RFC 3339, inclusive, on when it was sent
GET /v0/mail/inbox?project=&all=1&limit=50                     <- {"mail": [Mail]}   unacked unless all=1; never a report
                                                                  (the REST body is bare JSON; the CLI text and MCP mail_inbox
                                                                  frame bodies as untrusted data, SPEC 14 "untrusted mail frame")
GET /v0/mail/inbox/peek?project=&all=1&box=&limit=50           <- {"mail": [Mail]}   the inbox's rows, marking no announcement
                                                                  seen (a hook's poll; zm inbox --peek); an older daemon: not_found
GET /v0/mail/inbox?box=reports&project=&all=1&limit=50         <- {"mail": [Mail]}   the caller's reports (kind "report"), unread
                                                                  unless all=1; ack marks one read; [] for agents
GET /v0/mail/sent?project=&limit=50                            <- {"mail": [Mail]}
GET /v0/mail/triage       <- {"project": "harbor", "agent": "mini/triage", "machines": {"air": "night"}, "wait_minutes": 120}
PUT /v0/mail/triage       -> {"machine"?: "air", "agent"?: "night", "wait_minutes"?: 120}   ("" takes an agent back)
GET /v0/mail/notice       <- {"line": "zm: 2 unread (1 group) from mini/9c0e11aa, human:ana. Run: zm inbox", "unread": 2}
                             {"line": "", "unread": 0} when nothing is unread or no project resolves
   ?once=1 (agent hooks, zm notice, MCP): sessions share durable local delivery records. The line counts only
   fresh unread events, with one direct sender/newest visible ref and group counts; unread is the total still
   unread, even for an empty line. No ack or announcement receipt. Non-session callers keep snapshot behavior.
GET /v0/mail/read-marks?project=   <- {"marks": [{"project": "<slug>", "conv": "<client key>", "upto_ms": 1763000000000}]}
PUT /v0/mail/read-marks?project=<slug> -> {"marks": [{"conv": "...", "upto_ms": 1763000000000}]}   (1 to 100)
                          <- {"marks": [...]}   all of the caller's marks in that project
   Read marks (SPEC 9): the caller's (a person's) "read up to here" per conversation; conv is opaque, 1 to 200
   bytes; upto_ms is clamped to now and only moves forward (max-merge). Not an ack: no mail is taken. 400 no
   ?project= on a PUT, a bad conv or upto_ms, 0 or over 100 marks; 403 a session's PUT. Team: always the authority's (no
   copy); an authority without the route is named in "skipped" on a merged GET, and answers a named GET or a PUT
   404 not_found (an older zm: a client keeps its own marks for that project).
GET /v0/mail/{id}         <- Mail          ({id} = a ULID or a unique suffix of 6+ chars)
POST /v0/mail/{id}/ack    <- {"id": "01J...", "acked": true, "acked_by": "mini/4bb7545a", "acked_at": "..."}
                             {"id": "01J...", "acked": false, "acked_by": "someone", "acked_at": "..."}   (group mail taken first by another session; a hidden one shows as someone)
POST /v0/mail/{id}/got-it -> {}  <- {"id", "acked": true, "acked_by": "human:<name>", "acked_at"}
   For you (SPEC 9): mail to a person (human:<name>, not an ask) carries "for_you": "know" or "digest"
   ("held_until": the next 08:00 Pacific; POST /v0/mail takes "digest": true, only to a person). It is private
   to its participants, and only the person's Got it (this route from a Decisions client, or a paired device's
   POST /v0/approvals/got-it) marks it read: ack is 403 for everyone, a reply leaves it unread, a session's
   got-it is 403. No zm command calls this route.
POST /v0/mail/{id}/reply  -> {"body": "on it", "needs"?: "any", "client_request_id"?: "<stable retry key>"}       <- Mail (to the original sender; acks the original, except a for-you item)
   On mail with "members": one copy to every other member who still reads the project (one left: a two-party reply), a fresh batch; <- the first copy.
   ack and reply on an ask's mail (SPEC 16) that no approver has decided: 403 "mail ... is an open ask; only an
   approver's decision answers it", for every caller (socket, tokens, the asker); the decision answers it. A
   caller who cannot see the mail gets the usual not_found first.
GET /v0/mail/{id}/thread  <- {"mail": [Mail]}  oldest first
```

Every Mail carries `thread` (the id of its thread's first mail; the copies of mail to several people and their
replies share the first copy's id) and, on mail to several people, `members` and `batch`.

Personal mail requires `audience:"participants"`, which mail to a person (`human:<name>`, not an ask) gets
without asking; every read/event/snapshot/replica also checks exact
participants, including operators. Imported records expose `origin`, `legacy_delivery` and
`reply_unavailable:"legacy_return_route_unverified"`; imported reply and send-with-reply_to return 409.
Outbox entries apply caller scope and persisted participant visibility before limits; hidden entries expose no
operation IDs, request keys, bodies or errors. Public session notes omit personal mail IDs.
Node-bound replicas declare `Zm-Personal-Mail: participants/1` to receive personal rows/events; older replicas
are excluded even when their token names a participant. This header grants no identity or project scope.
Imports emit `dev.zm.mail.imported` with the same personal audience, and replicas consume it as mail.
Native keys are payload/parent/operation bound, atomic with mail, and scoped by identity/address/project;
changed reuse returns 409. Replicas retain one pending write ID per key. Omitted audience/key retain prior behavior.

`POST /v0/mail/imports` accepts `orchapi.LegacyMailImportRequest` (see SPEC 9):

```json
{"source_instance":"fixture-installation","mapping_version":"explicit-1","source_ids":["42"],
 "dry_run":true,
 "mappings":[{"source_project":"ownerprivate","source_recipient":"personfixture","recipient":"human:currentperson"}],
 "records":[{"record_kind":"mail","source_id":"42","source_project":"ownerprivate",
   "source_sender":"fixture-mac/1234abcd","source_recipient":"personfixture",
   "source_sent_at":"2026-11-01T12:00:00Z","body":"Invented recovery fixture.","body_complete":true,
   "legacy_delivery":{"state":"source_reported","delivered_count":0}}]}
```

The destination is the existing resolved project/area, not a source project name or inferred ref. The mapping's
recipient must exactly match the authenticated canonical human. A local owner may import into an existing
local project; a joined project forwards once to its existing authority and requires an explicit flag/header
or query project (never cwd or environment discovery). Authority callers must be a node-bound human using
the personal-mail capability, with current project/area write permission. Sessions, machines, unaware clients,
unbound bearer tokens and arbitrary destination people are refused. `source_thread_id`, `source_parent_id`,
`source_kind`, `source_ref` and original delivery `ack_by/ack_at/digest_at/last_delivered_at/dead_at` are optional
source claims. No `from` override or source write is accepted. Limits: 100 explicit IDs/records, 1 MiB total,
256 KiB/body, 30 seconds/phase (CLI combined network deadline: 60 seconds). CLI `zm mail-import artifact.json` forces dry-run unless `--apply` is specified;
`--read-hub https://<explicit-host>/mail --hub-token-env <explicit-variable>` fetches exact full bodies first.

Response: `{"dry_run":true,"partial":false,"records":[{"source_id":"42","status":"would_import"}]}`.
Per-record status is `would_import|imported|already_present|conflict|unmapped|invalid|missing`; committed/replayed
records include `native_id`, gaps include a bounded reason, and `partial` marks gaps. Errors are 400 for invalid
batch shape/bounds or missing explicit authority project, 403 for ineligible importers, and normal hidden-as-missing scope errors. A failed
request may have committed earlier records; replay the identical bounded artifact to recover. There is no
background cursor or automatic remap. A conflict never silently overwrites content or native read state.
The authority namespaces each source tuple by the server-derived stable human identity ID, preserving retries
across that person's devices without sharing another person's mapping. Ambiguous old ownerless reservations
return a generic 503 requiring explicit local reconciliation; no automatic adoption or replay occurs. Retained
old mappings migrate only when native sink evidence proves unchanged local-owner identity continuity. Dry-run
does not reserve or migrate rows. Imports never queue into a replica outbox or fall back to a local sink.
Live retry-cache reads reauthorize the selected destination and bind its canonical project ID and area;
equivalent slug/ID selections replay, while changing the destination cannot return the old successful result.
An explicit query project takes precedence over a project header, consistently for fresh requests and retries.

## Work and leases (work builder)

| route | stability | guard | check |
| --- | --- | --- | --- |
| `GET /v0/work/sources` | beta | authed | trackers of projects in ReadScope, from their mirrors' syncers; adapter paths, sync mode only for local callers; item counts only for whole-project readers |
| `GET /v0/work` | stable | authed | ReadScope; local rows and mirrored tracker rows by AreaSQL (the mirror's area follows the area rules) |
| `POST /v0/work` | stable | authed | write on WriteTarget |
| `GET /v0/work/page` | beta | authed | ReadScope; local and mirrored rows by AreaSQL, like `GET /v0/work`; `in=` entries naming a hidden or missing project or area match nothing; `epic=` walks only rows in sight, and an epic out of sight matches nothing |
| `GET /v0/work/labels` | beta | authed | labels of the rows `GET /v0/work/page` would read (ReadScope, AreaSQL, `in=`) |
| `GET /v0/work/inflight` | stable | authed | leases x items x sessions in ReadScope; hidden holders redacted |
| `GET /v0/owed` | beta | authed | what a session or the caller's own sessions still owe, read from records only: each module's rows by its own scope on ReadScope; `?session=` a session the caller may read (hidden answers like missing), default the caller's own session; `?person=` only the caller itself (another person answers like a missing one); read-only, settles nothing |
| `GET /v0/work/rollup` | beta | authed | ReadScope; rows by AreaSQL, like `GET /v0/work`; an epic out of sight is not named and its visible items count under `no_epic`; hidden holders redacted |
| `GET /v0/work/{id}` | stable | authed | read on the item's project/area, else not_found |
| `GET /v0/work/{id}/holder` | stable | authed | read on the item; holder redacted |
| `GET /v0/work/{id}/comments` | stable | authed | read on the item |
| `POST /v0/work/{id}/claim` | stable | authed | write on the item; conflict data = the lease, holder redacted |
| `POST /v0/work/{id}/release` | stable | authed | write on the item and holder == caller; force: operate, local socket only |
| `POST /v0/work/{id}/comment` | stable | authed | write on the item |
| `POST /v0/work/{id}/status` | stable | authed | write on the item |
| `POST /v0/work/{id}/link` | beta | authed | write on the item; a target the caller cannot read is not_found |
| `GET /v0/leases` | stable | authed | ReadScope; holders redacted |
| `POST /v0/leases` | stable | authed | write on WriteTarget |
| `DELETE /v0/leases/{key}` | stable | authed | write; holder == caller, or force (operate, local socket) |

`{id}` is one URL-escaped path segment (`client.Seg`). Without `--project` the id is looked up in every
project the caller sees; if it exists in several of them the call is `400 bad_request` (pass `--project`).
Projects the caller cannot see never count.

```
GET /v0/work/sources?project=     <- {"sources": [{"project": "01J...", "name": "local", "prefix": "local", "verbs": [...], "ok": true},
                                      {"project": "01J...", "name": "acme", "prefix": "ac", "path": "/usr/local/bin/zm-work-acme",
                                       "verbs": [...], "ok": true, "sync": {"state": "ok", "synced": "...", "lag_seconds": 2.1,
                                       "full_sync": "...", "items": 11512, "mode": "changes", "serve": true}}]}
                                     (state loading | ok | stale; "ok" is true only when ok; "error" says why not)
GET /v0/work?project=&area=&source=&status=&owner=&q=&limit=   <- {"items": [WorkItem],
                                     "skipped"?: [{"project": "01J...", "source": "bd", "error": "bd: unavailable"}],
                                     "behind"?: [{"project": "01J...", "source": "bd", "state": "stale", "synced": "...", "error": "..."}]}
                                     (skipped = trackers with nothing to serve, ?source= naming one is an error instead;
                                      behind = served from a mirror still loading or stale. Tracker items without
                                      ?status= exclude done.)
POST /v0/work       -> {"title": "Add the down migration", "body"?: "...", "area"?: "solver", "labels"?: ["v0"]}
                    <- WorkItem (id local:<6 random base32>)
GET /v0/work/page?in=&id=&status=&label=&owner=&q=&source=&created_after=&created_before=&updated_after=&updated_before=
                  &epic=&order=created|updated&dir=desc|asc&after=&limit=&with=comments,links&count=1
                   <- {"items": [WorkItem], "cursors": ["..."], "next"?: "<cursor>", "skipped"?: [...], "behind"?: [...]}
                      (every readable item, done included, totally ordered by (created|updated, project, id), newest first
                      unless dir=asc; keyset cursors: an item that did not move is never skipped or repeated. in, id,
                      status, label repeat (ORed); in = slug, ULID, slug/area or ULID/ (project-level items only); id = a
                      work id or "<project ULID>/<work id>"; owner = live lease holder (tracker owner for unleased tracker
                      items), checked after the lease overlay and redaction, so a page may hold fewer than limit items
                      (follow next); at most 500 values per repeated parameter; limit default 50, max 500; count=1
                      answers {"items": [], "cursors": [], "total": N} and refuses owner=. epic = a rollup node id
                      (a tracker work id, or "<project ULID>/<work id>"): only the items that open epic owns by the
                      rollup's rule (SPEC 10 "Rollup"), the open epics just under it included; an epic out of sight
                      or not an open epic matches nothing; a local: id is 400; in= areas narrow the walk as
                      rollup?area= narrows the counts. The answer echoes "epic": an authority that predates epic=
                      ignores it, so a caller refuses a page without the echo; a member's socket answers such a
                      page as empty with the authority in skipped.)
GET /v0/work/labels?in=            <- {"labels": ["bug", ...], "skipped"?: [...]}
GET /v0/work/inflight?project=     <- {"inflight": [{"lease": Lease, "item"?: WorkItem, "session"?: Session}]}
GET /v0/owed?session=&person=&since= <- {"for": "session"|"person", "subject": "<address>|<name>", "sessions": ["mini/4bb7545a"],
                                      "items": [{"kind": "pr"|"work"|"ask_open"|"ask_not_done"|"lane", "ref": "<url|work id|ask id|session>",
                                                 "title": "...", "state": "...", "project"?: "01J...", "area"?: "...",
                                                 "holder"?: "<session, redacted>", "since"?: "<time>"}],
                                      "unknown": [{"kind": "source_failed"|"partial"|"not_recorded", "name": "cloud_tasks", "why": "..."}]}
                                     what a session still owes, from zm's records: pr = an open pull request linked to
                                     work it holds or last held (the item says so: claimed or in review with a pr
                                     link); work = an item its lease holds; ask_open = a card it filed with no answer;
                                     ask_not_done = answered yes, work not done; lane = a session it started, still
                                     running. unknown is never empty: it names what zm keeps no record of (cloud tasks,
                                     lanes outside zm, background processes, worktree changes, PRs no lease holds now),
                                     joined projects held at another authority, any source that failed (source_failed), and any
                                     that read only part of its records (partial: a cap, a stale tracker mirror). With no
                                     query: a session lists its own, a person their own sessions' (since: how far back
                                     their ended sessions count, default 336h, at most 2160h). 400 naming both
                                     session and person; 404 a session out of sight or missing, or another person.
GET /v0/work/rollup?project=&area= <- {"rollups": [{"project": "01J...", "slug": "shop",
                                      "totals": Counts, "no_epic": Counts,
                                      "nodes": [{"id": "bd:shop-gkdw0", "title": "...", "kind": "theme", "parent"?: "bd:...",
                                                 "children"?: ["bd:..."], ...Counts, "claims": {"live": 2, "blocked": 1}}],
                                      "claimed": [{"id": "bd:shop-642nu", "title": "...", "status": "claimed", "priority"?: 1,
                                                   "holder": "mini/4bb7545a", "mine": true, "bound": true, "claimed": "...", "expires"?: "...",
                                                   "epic"?: "bd:shop-gkdw0", "state"?: "live|blocked"}],
                                      "deadlines": [{"id": "bd:...", "title": "...", "status": "open", "priority"?: 1, "due_at"?: "...",
                                                     "returns_at"?: "...", "day": "2026-11-05", "overdue"?: true}]}],
                                      "skipped"?: [...], "behind"?: [...]}
   Counts = {"total", "open", "urgent", "working", "blocked", "deferred", "done", "changed"?}
   total = open + working + blocked + deferred + done. See SPEC 10 "Rollup". kind: theme | epic. nodes are flat, most urgent
   first; parent and children are the tree, and a node's counts include everything beneath it, claims too. A claim's
   epic is the node that counts its item; state is live (holder session running or idle), blocked (the item blocked,
   or the holder waiting on a person or ended) or absent (a person's claim, or a holder the caller does not see).
GET /v0/work/{id}                  <- WorkItem with "comments": [Comment] (hidden link targets: {"rel": "blocks",
                                      "hidden": true}; a cross-project link carries the target's "project"; a tracker
                                      item from a stale mirror carries "stale": "tracker data as of ...: <why>")
                                      (a tracker item before its tracker's first full sync: 503 unavailable, the same
                                      for hidden and missing ids)
GET /v0/work/{id}/holder           <- {"id": "bd:solver-3x", "holder": "mini/4bb7545a", "lease": Lease,
                                       "mail_to": "mini/4bb7545a", "via": "holder of bd:solver-3x",
                                       "session"?: {address, machine, project, area, agent, title, state, note, managed,
                                       started, updated, seen?}}   (the holding session when the caller sees it; seen:
                                       a team replica's session, its last heartbeat)
GET /v0/work/{id}/comments         <- {"comments": [{"id": "01J...", "item": "local:k3x9qa", "author": "...", "body": "...", "created": "..."}]}
POST /v0/work/{id}/claim   -> {"ttl_seconds"?: 3600, "note"?: "...", "renew"?: false, "fence"?: 0}   (renew: only extend your own lease;
                              fence: only renew your live lease with this fence, never take the key; else 409 stale-lease)
                           <- {"lease": {"key": "local:k3x9qa", "project": "01J...", "area": "solver", "holder": "mini/4bb7545a",
                                "bound": false, "claimed": "...", "renewed": "...", "expires": "...", "fence": 7340291}, "item": WorkItem}
                              (fence: the lease's fencing token, new per claim, kept by renewals; SPEC 10 "Fencing")
                              (expires is always set; for bound leases it is the daemon-renewed deadline. bound:
                              a claim by a live managed session, or by a live hook session with no ttl_seconds,
                              renewed while the session runs and released when it ends; SPEC 10)
                              409 conflict, error.data = Lease (holder "someone" if hidden)
POST /v0/work/{id}/release -> {"force"?: false, "fence"?: 0}   <- {"key": "local:k3x9qa", "released": true}
POST /v0/work/{id}/comment -> {"body": "...", "fence"?: 0}    <- Comment
POST /v0/work/{id}/status  -> {"status": "in_review", "fence"?: 0}   <- WorkItem   ("claimed" on a local item: 400, claim instead)
                              fence (claim, release, comment, status, POST /v0/leases, DELETE /v0/leases/{key}): a write under your
                              lease; once that lease has gone (lapsed, released, taken again) 409 conflict,
                              error.data {"reason": "stale-lease"}, nothing written (checked when the write is
                              served; SPEC 10 "Fencing"). Omitted: no check.
POST /v0/work/{id}/link    -> {"rel": "pr", "target": "https://..."}   <- WorkItem
GET /v0/leases?project=&holder=    <- {"leases": [Lease]}   (from a replica's copy, a bound lease has no "expires": it is renewed at the authority)
POST /v0/leases            -> {"key": "lock:migrations", "ttl_seconds"?: 600, "note"?: "...", "renew"?: false, "fence"?: 0}   <- {"lease": Lease}
DELETE /v0/leases/{key}    -> {"force"?: false, "fence"?: 0}   <- {"key": "lock:migrations", "released": true}
```

## Trackers (SPEC 17.2)

| route | stability | guard | check |
| --- | --- | --- | --- |
| `GET /v0/trackers` | beta | local | every binding in ReadScope (`?project=`); never a password |
| `POST /v0/trackers` | beta | local | admin on WriteTarget; applied live (project.toml rewritten, no restart); checked; rolled back on a failing check |
| `POST /v0/trackers/{prefix}/test` | beta | local | admin on WriteTarget; the read-only conformance checks and a sync |
| `DELETE /v0/trackers/{prefix}` | beta | local | admin on WriteTarget; unbinds live; the mirror and the stored password go |

```
POST /v0/trackers  -> {"adapter": "native|<name>", "prefix"?: "bd", "area"?: "solver",
                       "mode"?: "new|dir|server", "dir"?: "/abs/dir", "server"?: "host:port", "database"?: "db",
                       "user"?: "ana", "password_file"?: "/abs/pw", "allow_migrate"?: false,
                       "readonly"?: false, "env"?: "inherit", "config"?: {"token_file": "/abs/tok"},
                       "wait_seconds"?: 300}
<- {"ok": true, "tracker": TrackerView, "created": true, "schema"?: {"server": 66, "pinned": 66, "migrated": false,
    "readonly": false}, "checks": [{"name": "describe", "ok": true, "skipped"?: false, "detail"?: ""}],
    "items": 11524, "sync_seconds": 31.4, "synced": true, "notes"?: ["..."]}
   <name> runs zm-work-<name>. mode, dir, server, database, user and allow_migrate are setup options (SPEC 11):
   an adapter takes the ones its describe.setup declares, any other is 400. password_file is a path the daemon
   copies to $ZM_HOME/secrets/<project>/<prefix>.password (0600): the password never travels in a request or
   answer. A second add of a prefix whose first add is still running is 409 at once (it never touches the first
   one's directory). A failing check is 503 "did not pass its checks, so it was not bound"
   with error.data = this result (the binding, its mirror, what its setup made and a password it copied are gone).
TrackerView = {"project": "01J...", "slug": "shop", "adapter": "native", "prefix": "bd", "area"?: "", "mode"?: "new",
               "env": "allowlist|inherit", "readonly"?: false, "config"?: {...}, "password"?: true,
               "data"?: "/Users/ana/.zm/trackers/01J.../bd", "sync"?: WorkSync,
               "backup"?: {"id", "kind", "newest"?, "age_seconds"?, "overdue"?, "failing"?, "scrub_pending"?}}
               (backup: the newest copy on disk; absent when the tracker has no backup)
GET /v0/trackers?project=          <- {"trackers": [TrackerView]}
POST /v0/trackers/{prefix}/test    <- the same result as add ("ok": false when a check failed; nothing changes)
DELETE /v0/trackers/{prefix}       <- {"tracker": TrackerView}
```

## Tracker backups (SPEC 17.9)

| route | stability | guard | check |
| --- | --- | --- | --- |
| `GET /v0/trackers/{prefix}/home` | beta | local | admin on WriteTarget; where the tracker's data lives, its backups and their age |
| `GET /v0/trackers/{prefix}/backups` | beta | local | admin on WriteTarget; the tracker's backups |
| `POST /v0/trackers/{prefix}/backups` | beta | local | admin on WriteTarget; schedules one (`run_now` runs it at once) |
| `DELETE /v0/trackers/{prefix}/backups` | beta | local | admin on WriteTarget; `?id=&kind=`; unschedules it, the copies stay |
| `POST /v0/trackers/{prefix}/backups/run` | beta | local | admin on WriteTarget; runs one (`id`) or all now; 409 while a restore or another backup of it runs |

```
GET /v0/trackers/{prefix}/home <- {"tracker": TrackerView, "home": {"target": "native|<adapter>", "kind": "service"},
    "backups": [BackupView], "backup_kinds": ["sqlite", "export"], "notes"?: ["..."]}
POST /v0/trackers/{prefix}/backups -> {"target": "icloud|icloud:sub|file:///abs", "kind"?: "sqlite|export",
    "every_seconds"?: 3600, "keep"?: 48, "run_now"?: false}  <- BackupResult
   a sqlite backup to icloud is refused (400); default keep: export 48, sqlite 24
BackupResult = {"backup": BackupView, "ran", "seconds"?, "notes"?}
BackupView = {"id": "icloud", "target", "kind", "path", "every_seconds", "keep"?, "created", "last_run"?, "last_ok"?,
    "last_error"?, "failures"?, "age_seconds"?, "items"?, "bytes"?, "running"?, "due"?,
    "newest"?: "<time of the newest copy on disk>", "copies"?: 3, "scrub_pending"?: true}
GET /v0/trackers/{prefix}/backups <- [BackupView]
DELETE /v0/trackers/{prefix}/backups?id=icloud&kind= <- BackupView
POST /v0/trackers/{prefix}/backups/run -> {"id"?: "icloud", "kind"?} <- [BackupResult]
```

### Off-site home backups and the watchdog (SPEC 17.9 "Off-site home backups")

| route | stability | guard | check |
| --- | --- | --- | --- |
| `GET /v0/backup` | beta | local | the machine's owner; the off-site backup (target, secret references, the last set, failures) and the watchdog |
| `PUT /v0/backup` | beta | local | the machine's owner; refuses a target that does not answer with the keys named; `run_now` runs it |
| `DELETE /v0/backup` | beta | local | the machine's owner; stops it, the sets stay |
| `POST /v0/backup/run` | beta | local | the machine's owner; takes and sends a set now; 409 while one runs |
| `GET /v0/backup/sets` | beta | local | the machine's owner; the sets on the target |
| `PUT /v0/backup/watchdog` | beta | local | the machine's owner; sets the heartbeat and sends one at once (the answer shows how it went) |
| `DELETE /v0/backup/watchdog` | beta | local | the machine's owner; stops the heartbeat |

```
PUT /v0/backup -> {"target": "s3://bucket[/folder]|file:///abs", "home": "team-home", "endpoint"?: "https://...",
    "region"?: "auto", "access_key"?, "secret_key"?, "encrypt"?, "encrypt_salt"? (together), "every_seconds"?: 900,
    "keep"?: 8, "keep_hourly"?: 24, "keep_daily"?: 30, "run_now"?}  <- OffsiteView
   keys are secret references (keychain:S[:A], file:/abs, env:NAME), never secrets; an s3 target needs endpoint and both keys
OffsiteView = {"configured", "target"?: "<url> at <endpoint host> (encrypted)", "home"?, "access_key"?, "secret_key"?,
    "encrypt"?, "encrypt_salt"? (references), "every_seconds"?, "keep"?, "keep_hourly"?, "keep_daily"?, "status"?: HomeBackup,
    "set"?: "<newest stamp>", "last_run"?, "running"?, "scrub_due"?, "watchdog"?: WatchdogView}
HomeBackup = {"kind": "s3|file", "every_s", "last_ok": "<time>|null", "bytes"?, "failing"?, "failures"?, "error"?}
POST /v0/backup/run <- {"ran", "set"?, "files"?, "uploaded"?, "copied"?, "bytes"?, "seconds", "deleted"?: ["<stamp>"]}
GET /v0/backup/sets <- {"target", "home", "sets": [{"stamp": "20261102T031500Z", "complete", "bytes", "objects"}]}
PUT /v0/backup/watchdog -> {"url": "https://...", "secret": "<secret reference of a whsec_ secret>", "home"}  <- OffsiteView
WatchdogView = {"url", "home", "secret" (a reference), "last_sent"?, "last_status"?, "last_error"?, "failures"?}
GET /v0/health (local callers) also carries "backup"?: HomeBackup and "heartbeat"?: {"last_sent", "last_status"?,
   "failing"?, "failures"?, "error"?} (the last send); GET /v0/sync carries the backup and the daemon's start time for
   a whole-project reader, and a replica shows them as "backup" and "started" on its authority row of GET
   /v0/work/sources
```


## Jobs and imports (SPEC 17.5; retired 2026-11-01)

**Retired 2026-11-01.** These routes are gone and answer 404: `POST /v0/work/import` (`zm work import`, a beads
JSONL export into `local:` items), `POST /v0/trackers/import` (`zm tracker import beads`, into a zm-owned beads
database), and the jobs that ran them (`GET /v0/jobs`, `GET /v0/jobs/{id}`, `POST /v0/jobs/{id}/cancel`,
`POST /v0/jobs/{id}/resume`; `zm jobs`). `POST /v0/issues/import` (`zm issues import`, a beads Dolt database
into native issues) is gone too (2026-11-10): import from a release before it. `bd-zm` (installed as `bd`) keeps
bd's commands on native issues.

`dev.zm.work.imported` is **deprecated**: nothing publishes it now, but replicas still apply it from older event
logs, so its shape stays: subject the job id, data WorkImported = {"job": "01J...", "phase": "importing" |
"links", "items": ["local:..."]}, scoped to the area of the items it names. `dev.zm.job.started` and
`dev.zm.job.finished` are no longer published.

## One-shot tasks (session builder)

All task routes are socket only and refuse token and network callers. Task IDs are session IDs, except `rt-` IDs of a task on another machine (below); results contain untrusted agent text.

| route | stability | access | guard |
| --- | --- | --- | --- |
| `GET /v0/tasks/agents` | beta | local | installed Claude and Codex commands; standard commands have authentication status probed |
| `POST /v0/tasks` | beta | local | live parent session, admission, depth and concurrency limits; a `client_request_id` starting `rt-` is refused (400); with `machine`, the authority's checks below; with `where: "cloud"`, the task's project opted that runtime in |
| `GET /v0/tasks/cloud` | beta | local | read on the resolved project: its cloud opt-in |
| `PUT /v0/tasks/cloud` | beta | local | admin on the resolved project, which must be this machine's; project.toml `[cloud]` rewritten live |
| `GET /v0/tasks/{id}` | beta | local | operate on the task session; `wait_s` capped at 45 |
| `GET /v0/tasks/{id}/read` | beta | local | parent, socket owner or chain continuation; bounded task history item |
| `POST /v0/tasks/{id}/cancel` | beta | local | operate on the task session |
| `DELETE /v0/tasks/{id}/transcript` | beta | local | remove an ended task's transcript, stdout and stderr spools, and scrollback after delivery and redaction; `?discard_redaction=1` explicitly seals output paths and releases an ended task's hold after 30 seconds |

`task_start` accepts `on_limit: "next_agent"` and `continue_from`; `task_read` reads omitted task history by item ID and byte offset. A continuation inherits its source project, area, request, worktree and limits; cancelling the source cancels its live continuation. Dirty source worktrees are committed locally as an unreviewed work-in-progress snapshot, including non-ignored untracked files. A refused continuation may be retried. Handoff text is untrusted; redaction holds and process-identity failures prevent reads or continuation.

**Machine choice** (SPEC 8): `POST /v0/tasks` with `machine: "auto"|"<name>"` (empty or this machine's name: here) and an optional `cwd` (an absolute path on that machine; default: the main checkout of the project's most recent session there) starts the task on another machine of the parent's team project through the authority. `project` (a slug or ID; with `machine` only, and without `cwd`: the machine uses its own checkout of that project) places it through that team project instead, resolved as the caller writes within its session's caps (a project it cannot see is `404` exactly like a missing one; not a team project, or `project` with `cwd`, is `400`); the parent stays the caller's session, and a `client_request_id` is the parent's across projects (another project with it is `409`; `lookup` finds its run whatever project it names). The answer is `{id: "rt-…", machine, state: "pending", …}`; status adds `pending` (not taken yet) and `refused` (`error` says why) to `running` and `ended`, `session` is the task's session on that machine once taken, and the text is cut at 64 KiB. `GET /v0/tasks/{rt-id}`, `/read` and `/cancel` answer from the parent's machine; only the parent and this machine's owner see it (else `404`). `continue_from` with `machine` is `400`. A start waits in the authority's line for a free suitable machine (`front: true` puts it before every non-front start; a named machine pins it; status adds `position`, `requeued`, `acted` and `lost`; SPEC 8 "Waiting line"); one cancelled while waiting, or before its machine took it, never runs. `sandbox: {"size"?, "egress"?}` (with `machine` only; `size` like `c2m8`; `egress` up to 32 hosts `name.tld` or `*.name.tld` the box's egress allowlist adds) runs it in a pool box through a machine's task driver, on that machine's sandbox slots (SPEC 8 "Where it runs"); status carries `sandbox`. With `run`, `continue_from` or `cwd`, without `machine`, an unknown size, or an `egress` entry that is not a host name: `400`; pinned to a machine without a task driver `403`; no machine of the project has one `503`.

**Cloud tasks** (SPEC 8): `POST /v0/tasks` with `where: "cloud"` (empty: this machine; anything else `400`) runs a claude or codex task on the provider's cloud computer through this machine's own CLI login: `claude [--settings '{"remote":{"defaultEnvironmentId":"<id>"}}'] --cloud=<prompt> --permission-mode auto` (`default`: no `--settings`), `codex cloud exec --env <id> [--branch=<the checkout's branch>] -- <prompt>`. The task's project (resolved as for a local task) must name that runtime's environment, and the repo `cwd` is in (its git root) must be that project's checkout by its `paths` or `remotes` (a `--project`, the session's project or a repo's own `.zm/project.toml` does not count: the provider clones the cwd's repo), else `400` before any provider call. `machine`, `project`, `front`, `model`, `continue_from`, `on_limit`, `run`, `lookup` and `worktree: true` are `400`; the prompt (at most 100 KiB) goes as given. The answer and status are the task plus `cloud: {agent, environment?, id?, url?, status}` (`status`: `submitting`, `submitted`, `pending`, `ready`, `failed`, `unknown`); `agent` is `claude-cloud` or `codex-cloud`. A Codex task is checked with backoff (after 1, 2, 4 and 8 minutes, then every 10) and ends `ok` with its diff once ready; a Claude task ends `ok` once the session exists (Claude Code has no status command). `finished` is `unreported`; there is no no-output stop; cancel stops zm's watch, not the provider's task. `GET /v0/tasks/cloud` <- `{project, claude?, codex?}`; `PUT /v0/tasks/cloud` -> `{claude?: "<id>"|"default"|"", codex?: "<id>"|""}` (a present field replaces, `""` takes it back; an id is `[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}`; a session caller is `403`: only the machine's owner opts in) <- the same.

`POST /v0/tasks` accepts `{prompt, agent, model?, work?, worktree?, client_request_id?, timeout_s?, machine?, project?, cwd?, for?, job_kind?, run?, lookup?, front?, where?}` (`for` and `job_kind` default to the parent's). Worktree defaults to true; `work` receives a completion comment and does not claim or bind a lease. A repeated request ID for the same parent returns the same task; a changed request conflicts. `lookup: true` (with `machine` and `client_request_id`; also on `POST /v0/team/tasks`) answers the task an earlier start with that request ID made, else `not_found`, and never starts one (SPEC 8 "Script runs"). Status returns `{id, session, parent, agent, model?, work?, state, result?, redaction_pending?, mail_delivery, comment_delivery, mail_error?, comment_error?}`. State is `running` or `ended`. An ended result is `{finished: "done"|"not_done"|"unreported", text, text_withheld?, outcome, usage: {input_tokens, output_tokens, cached_input_tokens}, branch?}`. `finished` is the child's self-report; unreported counts as not done. Outcome describes the process independently; cancellation or a process signal takes precedence over runtime classification, including when a limit stops a task. A withheld text is empty until pending redaction is verified; `redaction_pending` shows a durable hold even before a result exists. Delivery states (`pending`, `sending`, `sent`, `queued`, `failed`, `unknown`, or `skipped`) and errors remain visible; known failures retry up to three times, and ambiguous writes are not replayed. Task output and results are untrusted. `GET /v0/tasks/agents` returns `{agents: [{agent, available, authenticated?, reason?, models: [], capabilities}]}`: an empty model list means the runtime default, not a verified model inventory; authentication is not checked for custom commands.

## Sessions (session builder)

| route | stability | guard | check |
| --- | --- | --- | --- |
| `POST /v0/sessions` | stable | local | operate on WriteTarget (project `none` allowed when nothing resolves); admission control. With `machine` (another machine; SPEC 8 "Sessions on other machines"): no agent session (`403`), no command, env, mcp, work, name, prompt or sandbox (`400`), the cwd's project a team project (`400`), sent on to its authority as `POST /v0/team/sessions` |
| `GET /v0/sessions` | stable | authed | ReadScope; Scope.SQL on (project, area); pages with `?before=`; on a replica the machine owner's list with no project or query falls back to a cached roster per authority (SPEC 17.6) |
| `POST /v0/sessions/register` | beta | local | cwd-resolved project, else `none`; never fails for lack of a project |
| `POST /v0/sessions/hook` | internal | local | as register, plus the state and (asked) the session's own notice: an agent hook in one round trip |
| `GET /v0/sessions/admission` | beta | local | - |
| `GET /v0/machines` | stable | authed | this machine: the owner, or a whole-project reader of a team project it hosts; a joined machine: a whole-project reader of a project it heartbeats for |
| `GET /v0/sessions/gc` | beta | local | lists; deletes nothing |
| `GET /v0/sessions/{id}` | stable | authed | read on the session's project/area, else not_found |
| `POST /v0/sessions/{id}/state` | internal | local | the session itself, or operate |
| `POST /v0/sessions/{id}/name` | beta | local | the session itself, or operate; `409` when another live session here has the name, or the session ended |
| `POST /v0/sessions/{id}/project` | beta | local | operate on the session; write on the target project/area (hidden or missing: not_found); `409` while it holds a lease or has ended |
| `POST /v0/sessions/{id}/input` | internal | local | operate on the session |
| `POST /v0/sessions/{id}/prompt` | beta | authed | operate on the session's project/area; the session shared, managed and live (`409` otherwise); on a replica forwarded to the authority (SPEC 17.10) |
| `GET /v0/sessions/{id}/prompts` | beta | authed | read on the session's project/area, and the whole project (an area guest: `403`); on a replica forwarded to the authority |
| `GET /v0/sessions/{id}/activity` | beta | authed | read on the session's project/area, else not_found; this machine's sessions only (never forwarded) |
| `GET /v0/sessions/{id}/subagents` | beta | authed | read on the session's project/area, else not_found; this machine's sessions only (never forwarded) |
| `POST /v0/sessions/{id}/resize` | internal | local | operate on the session |
| `POST /v0/sessions/{id}/stop` | stable | authed | operate on the session; `escalate` runs TERM, HUP (+ KILL with `force`). From anyone but this machine's socket, or for another machine's session: checked and relayed like `POST /v0/sessions/{id}/turn`; only a session started from another machine (`409`; SPEC 8 "Sessions on other machines"); a plain TERM (another `signal`, `escalate` or `force`: `400`; `wait_ms` ignored); answers `{op: "stop", ok}`, or `202` pending after 15 s |
| `GET /v0/sessions/{id}/output` | internal | local | operate on the session |
| `GET /v0/sessions/{id}/attach` | internal | local | operate on the session; `?input=0` read-only; `?replay=all` all scrollback |
| `GET /v0/sessions/{id}/events` | stable | authed | operate on the session; a structured session only (`409` otherwise); SSE with `Accept: text/event-stream`. Shared: from anyone but this machine's socket, or for another machine's session, the session must be shared and in a team project (`409`); hidden or missing `404`, visible without operate `403`; another machine's session is relayed by the project's authority (`wait_s` at most 25) |
| `POST /v0/sessions/{id}/turn` | stable | authed | operate on the session; a live structured session; `steer` needs the adapter's steer capability (`409`). Shared: as `GET /v0/sessions/{id}/events`, live, no agent session (`403`), no `images` (`400`); relayed: the result, or `202` pending after 15 s |
| `POST /v0/sessions/{id}/interrupt` | stable | authed | operate on the session; a live structured session with the interrupt capability. Shared: as `POST /v0/sessions/{id}/turn` |
| `POST /v0/sessions/{id}/answer` | stable | authed | operate on the session; a live structured session with the approvals capability. Shared: as `POST /v0/sessions/{id}/turn` |

`{id}` is a session8 or a URL-escaped full address.

```
POST /v0/sessions  -> {"agent": "claude", "command"?: ["claude", "--model", "opus"], "model"?: "opus",
                       "cwd": "/Users/ana/code/Shop", "area"?: "solver", "title"?: "...", "work"?: "local:k3x9qa",
                       "prompt"?: "...", "worktree"?: true, "sandbox"?: false, "env"?: {"K": "V"}, "rows"?: 40, "cols"?: 120,
                       "shared"?: true, "mode"?: "pipe", "approvals"?: "ask|never", "resume"?: "<runtime id>",
                       "for"?: "bob-mac", "job_kind"?: "interactive|standing|farm|timer",
                       "mcp"?: [{"name": "t3", "url": "http://127.0.0.1:3773/mcp", "headers"?: {"Authorization": "..."}}],
                       "machine"?: "<another machine>", "client_request_id"?: "...", "continue_from"?: "<pipe session id>",
                       "allowed_tools"?: ["Bash", "Read", "Bash(gh pr view:*)"], "permission_mode"?: "default|acceptEdits",
                       "safe_outputs"?: {"bead-write": 3, "zm-ask": 1, "mail-send": 2}}
   (shared: a team project only, else 400; SPEC 17.10. mode pipe: a structured session, SPEC 8 "Structured
   sessions": the agent's zm-agent/1 adapter on pipes instead of a PTY; 400 when no adapter serves the agent, with
   shared, with approvals, resume, mcp or a tool grant but no mode pipe, resume, mcp or a tool grant when the adapter
   cannot, a malformed mcp list (SPEC 8; header values are never echoed), more than 64 allowed_tools, one over 200
   bytes or not a tool name with an optional parenthesised rule (no comma), or another permission_mode, or safe_outputs without mode pipe, with another kind or a cap outside 0-1000 (SPEC 8 "Safe
   outputs": the session's writes past a cap are 403 forbidden; the answer echoes the caps); prompt becomes its first
   turn; rows and cols are ignored. continue_from: SPEC 8 "Pipe handoffs"; 400 without mode pipe, with machine,
   or from a terminal session, an unshared source when shared, or another project or area)
<- Session {"address": "mini/9c0e11aa", "id": "9c0e11aa", "machine": "mini", "project": "01J...", "area": "solver",
            "launcher": "01J...", "agent": "claude", "command": [...], "version": "2.3.1", "model": "opus",
            "cwd": "...", "worktree": "/Users/ana/.zm/worktrees/9c0e11aa", "branch": "zm/9c0e11aa", "sandbox"?: true,
            "scratch": "/Users/ana/.zm/scratch/9c0e11aa",
            "state": "running", "work": "local:k3x9qa", "managed": true, "pid": 4243, "started": "...", "updated": "...",
            "activity"?: "Editing store.go", "activity_at"?: "...", "term_title"?: "Fix the presence bug",
            "who"?: {"name": "ana", "kind": "human|machine|agent"}, "reach"?: "all|project|area",
            "fg"?: {"pid": 4250, "name": "vim"}, "busy"?: true, "jobs"?: 2, "ports"?: [3000, 5173],
            "unreachable"?: "its terminal is held by an older or newer zm: ...",
            "account"?: "ana@example.com", "for"?: "bob-mac", "job_kind"?: "farm",
            "tokens"?: {"input": 12, "cache_read": 3400, "cache_write": 120, "output": 560},
            "mode"?: "pipe", "capabilities"?: {"steer": true, "interrupt": true, "approvals": true, "questions": true,
            "resume": true, "images": true, "partial": true, "usage": true, "plan": true, "tool_output": true, "model": true,
            "mcp": true, "tools": true}}
   Every Session answer (lists, gets, hook, register, state, stop, events) carries who and reach as the answering daemon
   resolves them: a local session is its launcher's (this machine's owner) with reach all; a replica's session at a
   team authority (presence) is the identity its machine joined as (kind from the registry), reach project or area
   by that node-bound token's cap. activity (<= 120 characters, one line) is what the agent's hooks report: never a
   command, a prompt or file contents (SPEC section 8); term_title is a managed session's last OSC 0/2 terminal title,
   written at most once a second, and sent in presence only for a known agent runtime (never a shell's: its titles
   carry command lines). A change of either alone publishes dev.zm.session.updated.
   account, for, job_kind and tokens: usage attribution recorded at spawn or registration (SPEC 8 "Usage"); tokens
   are the whole session's from its transcript, refreshed every 3 minutes and at its end without an event. work is
   the item it holds a lease on, else the last one it held. Presence carries all of them.
   unreachable (this machine's owner only): a live managed session whose holder this daemon cannot adopt (another
   holder protocol major, SPEC 8); it keeps running, and attach, input and stop answer 409 until one adopts it.
   fg, busy, jobs and ports (managed sessions on this machine only): the terminal's foreground process (tcgetpgrp on the
   PTY, its executable name), busy = it is not the session's own process, jobs = the other processes in the session's
   process tree (background and suspended jobs, an agent's helpers), and the TCP ports in LISTEN state of the
   session's process tree. fg sampled at most every 2 s, ports at most every 10 s (sooner when the process tree
   changed), only while a client is attached or the owner read the session in the last 30 s with GET
   /v0/sessions/{id} (a list does not count); a change publishes dev.zm.session.updated (coalesced).
   This machine's owner only: a token or tailnet reader, events to one and presence never carry them.
   503 unavailable, error.data = {"ok": false, "reason": "memory", "detail": "memory free 12% < 20%",
        "probe": {"memory_free_percent": 12, "swap_used_mb": 6100, "disk_free_mb": 80000, "managed_sessions": 3}}

GET /v0/sessions?project=&state=&agent=&all=1&limit=500&before=<next>   <- {"sessions": [Session], "next"?: "<cursor>", "skipped"?: [{"project", "source", "error"}], "behind"?: [{"project", "source", "state", "synced"?, "error"?}]}
   skipped names unavailable shares; behind names shares not current. Preserve these when displaying partial/stale lists; neither changes Session state or heartbeat seen.
   most recently updated first (ties by id), sorted by state within the page; limit 1..500 (default 500);
   next is set when more rows follow: pass it as before for the rest (opaque; 400 when malformed)
   on a team authority, the first page also carries replicas' sessions from presence, each with "seen"; a
   heartbeat older than 60 s shows "idle", note "presence stale"
   a session may carry "parent": the address of the session that started it (SPEC 8 "Parents"); absent when
   none is known or the caller cannot see it; GET /v0/sessions/{id} too
POST /v0/sessions/register  -> {"agent": "claude", "runtime_id": "4bb7545a-...", "cwd"?: "...", "pid"?: 0,
                                "title"?: "", "command"?: [], "version"?: "", "model"?: "",
                                "session"?: "mini/9c0e11aa", "state"?: "idle", "note"?: "", "end"?: false,
                                "for"?: "", "job_kind"?: "", "agent_home"?: "<$CLAUDE_CONFIG_DIR or $CODEX_HOME>"}
                            <- 201 Session when created, 200 when updated (managed false; idempotent per (agent, runtime_id)).
                               session = the zm run session this hook runs in ($ZM_SESSION): the runtime id is
                               recorded on it, no second session. state/note apply in the same call. end = the
                               runtime's session ended (unmanaged only; outcome ok).
POST /v0/sessions/hook      -> {register's fields..., "notice"?: true, "activity"?: "Editing store.go"}
                               "subagent"?: {"event"?: "start|stop|spawn", "id"?: "a1b2", "type"?: "Explore",
                                             "description"?: "Find browse package"}, "needs"?: "permission|input"}
                            <- {"session": Session, "notice"?: Notice}   200; what `zm hook` sends: one request per
                               hook (register-or-find, state, and on prompt/session-start the session's notice line,
                               as GET /v0/mail/notice?once=1: never an error). activity is recorded with the state in the same
                               write (clipped to one line of 120 characters): one state_changed event when the state
                               changed, else one updated event when the activity did (at most one per session per 5 s:
                               changes inside the gap go out once, trailing), else none; a repeat refreshes activity_at
                               (no event) only once it is 30 s old. Ignored for an ended session. `zm hook claude tool`
                               (PreToolUse) sends state running and the tool's activity; prompt sends "Thinking".
                               subagent (SPEC 8 "Subagents"): start/stop (Claude SubagentStart/SubagentStop) add or end
                               one under the session and change nothing else; no event = a tool step inside subagent id
                               (its activity, logged; the session's state, cwd and title stay); spawn = the parent's
                               Task/Agent tool (its activity as usual; type and description wait for the start). needs
                               = why a needs_human state waits (default permission). Inside a managed shell
                               ("session" = $ZM_SESSION naming a live managed session that runs no agent runtime) the
                               agent binds to the shell instead of registering (runtime_agent; SPEC 8 "Agents in a
                               shell"); a later hook of it reaches the shell by its runtime id; end unbinds it.
   Session gains "runtime_agent"?: {"agent": "claude", "runtime_id": "4bb7...", "model"?, "task"?: "Editing repos.go",
   "pid"?, "started"} (an agent bound to this managed shell; task = its latest activity, kept while idle),
   "subagents"?: [Subagent] (live ones, oldest first, at most 20; Subagent = {"id", "type"?, "description"?,
   "state": "running|ended", "activity"?, "started", "updated", "ended"?}; one no hook reached for 30 min counts as
   ended) and "needs"?: {"kind": "permission|input|decision|manual", "at", "subagent"?} (only while needs_human:
   permission and input from the agent's own hooks, decision from needs=human mail it sent, manual from POST
   /v0/sessions/{id}/state; subagent = the one that stepped last before a hook's wait: its next step or stop ends it). A start, stop, binding or unbinding
   publishes dev.zm.session.updated; a subagent's activity alone a live-only one.
GET /v0/sessions/{id}/activity?limit=50&subagent=<id>   <- {"lines": [{"at", "text", "subagent"?}]}   newest first;
                               limit 1..200 (default 50; 400 otherwise); subagent filters ("" = the session's own lines).
                               The hooks' activity lines as they changed (repeats are one line), the last 200 per
                               session, kept 7 days after it ends.
GET /v0/sessions/{id}/subagents <- {"subagents": [Subagent + "lines": [{"at", "text", "subagent"}]]}   oldest first:
                               running, and ended within the last hour (an ended session's show ended), each with its
                               last 20 lines.
POST /v0/sessions/{id}/prompt  -> {"text": "please rerun the solver tests"}
                            <- 202 SessionPrompt {"id", "session", "from", "who", "text", "at", "state": "pending|delivered",
                               "delivered_at"?}   Delivered means staged without Enter, not submitted or processed.
                               400 text over 8 KiB, empty, a control character other than newline or
                               tab, or several lines for a program without bracketed paste, or a "!" first; 403 operate
                               refused or an agent session's call; 429 over 10 a minute per sender and session
                               (Retry-After);
                               404 hidden or missing; 409 not shared, unmanaged or ended
GET /v0/sessions/{id}/prompts  <- {"prompts": [SessionPrompt]}   newest first, at most 200 (30 days kept)
   Session gains "shared"?: true, "last_prompt"?: {"who", "at", "text"}, "in_use"?: {"by", "attached", "typed_at"?}.
   POST /v0/team/presence ("sessions" part): each session may carry "shared", "in_use" and "delivered": [prompt ids typed since the
   last beat]; each answer may carry "prompts": [SessionPrompt] pending for that session.
GET /v0/sessions/admission  <- {"ok": true, "probe": {...}}
GET /v0/machines            <- {"machines": [{"machine": "mini", "projects"?: ["01J..."], "seen"?: "...",
                                "stats": MachineStats}], "skipped"?: [...]}
   MachineStats = {"memory_used_mb", "memory_total_mb", "swap_used_mb", "swap_total_mb", "disk_free_mb",
                   "disk_total_mb", "sessions", "max_sessions", "at", "remote_tasks"?}   (-1 = unknown; max_sessions 0 = no limit; at = when sampled;
                   remote_tasks: the machine takes tasks from other machines, SPEC 8 "Machine choice";
                   "script_runs"?: it takes them and its zm runs scripts, SPEC 8 "Script runs";
                   "task_slots"?: how many it runs at once; "sandbox_tasks"?, "sandbox_slots"?: it runs a task driver
                   for sandbox tasks (config sandbox_task_cmd) and how many at once, SPEC 8 "Where it runs";
                   "os_user"?: the account zm runs as there, set when it takes them (an authorize-key action's user, SPEC 16);
                   "t3_enabled"?, "t3"?: {"environment_id", "version", "port"}: a t3 block is set, and the Mezze app server zm
                   started there while it answers on loopback, SPEC 8 "Mezze app on a machine";
                   "zm"?: {"version", "commit", "started", "release"?, "installed"?, "held"?, "held_since"?,
                   "last"?: {"at", "result", "to"?, "reason"?}, "stable"?: [...], "failed"?: [...]}: the zm build and
                   its release channel state, SPEC 2 "Release channel")
   This machine first (no seen: sampled at most every 30 s); then, on a team authority, each joined machine from
   its last heartbeat (seen), in the projects the caller reads whole. A replica merges each authority's list.
GET /v0/sessions/gc         <- {"candidates": [{"session": "mini/9c0e11aa", "outcome": {"kind": "ok"}, "worktree": "...",
                                "branch": "zm/9c0e11aa", "dirty": false, "ahead": 2,
                                "reasons": ["ended ok", "clean", "2 commits not on upstream"], "safe": false}]}
GET /v0/sessions/{id}       <- Session (ended: "outcome": {"kind": "quota", "exit_code": 1,
                               "evidence": "Claude usage limit reached", "confidence": "low"})
GET /v0/sessions/{id}?sample=1 <- Session: the owner (socket, no token) gets fg, busy and jobs sampled as of now (ports
                               as last read), not as of the sampler's last cycle; a one-off: no event, and the session
                               is not sampled afterwards (a failed sample answers without fg, busy and jobs); anyone
                               else gets the plain answer
POST /v0/sessions/{id}/state   -> {"state": "needs_human", "note"?: "asked: deploy to prod?"}   <- Session
POST /v0/sessions/{id}/name    -> {"name": "auth-fix" | ""}   <- Session   (`zm session name <id> <name>`; SPEC 8 "Names")
POST /v0/sessions/{id}/project -> {"project": "acme" | "<ULID>" | "acme/solver" | "none", "area"?: "solver"}   <- Session
                                  `zm session move <id> <project>[/<area>]`: puts the session in that project and area
                                  (dev.zm.session.left {address, project, area} of where it was, to that log, then
                                  dev.zm.session.updated to the new one; nothing when it is already there). On a replica a joined team
                                  project is a target too. 409 while the session holds a lease in its project (release
                                  it first) or has ended. A session in none also moves by itself when its agent's hook
                                  reports a cwd a cwd rule resolves (SPEC section 8).
POST /v0/sessions/{id}/input   -> {"data": "yes", "enter": true}       <- 204
POST /v0/sessions/{id}/resize  -> {"rows": 40, "cols": 120}            <- 204
POST /v0/sessions/{id}/stop    -> {"signal"?: "TERM|INT|HUP|KILL", "wait_ms"?: 3000}   <- Session (as it is after the wait;
                                  wait_ms 1..10000, default 3000; `zm kill` escalates TERM -> HUP (-> KILL with --force))
                               -> {"escalate": true, "force"?: false}   <- Session, header Zm-Stop-Signals: "TERM, HUP"
                                  what `zm kill` does, in one call: TERM and up to 3 s, HUP and up to 1 s, with force
                                  KILL and up to 3 s; each wait ends with the session; the header lists the signals
                                  sent (empty when the session had already exited). escalate with signal or wait_ms, or force without escalate: 400
GET /v0/sessions/{id}/output?since=<offset>   <- {"data": "...", "offset": 0, "next": 5120}
GET /v0/sessions/{id}/attach   Connection: Upgrade, Upgrade: zm-pty  -> 101, then raw PTY bytes both ways
GET /v0/sessions/{id}/attach?input=0                                -> 101, PTY bytes to the client only: what it
                                  sends is dropped, never counts as a person typing (a hook-set needs_human stays),
                                  and the daemon keeps answering the session's ESC[6n itself (a viewer is not a
                                  terminal); closed when the session ends. input other than 0/1: 400
GET /v0/sessions/{id}/attach?replay=all                             -> as above, replaying all the scrollback held (1 MiB)
                                  instead of its last 64 KiB. replay other than all: 400
```

Structured sessions (`mode: "pipe"`, SPEC 8 "Structured sessions"). Input, resize and a writable attach answer
`409` for one; a read-only attach shows its raw event stream. Event texts are the agent's and its tools': untrusted.

```
GET /v0/sessions/{id}/events?after=<seq>&limit=500&wait_s=0
   <- {"events": [AgentEvent...], "next": 42, "state": "idle", "ended"?: true, "truncated"?: true}
   events after seq `after`, oldest first, at most limit (max 5000); with none yet and the session live, waits up to
   wait_s (max 45) for one. next: the last seq returned (else after). ended: the session ended and nothing follows.
   truncated: the stored stream reached its 64 MiB cap and later events were not kept. 409 for a terminal session.
   Accept: text/event-stream: "id: <seq>", "event: <type>", "data: <AgentEvent>" per event, resuming after
   Last-Event-ID (else after), ": ping" while quiet, ": ended" after the last event.
   AgentEvent {"seq": 7, "at": "...", "type": "session|turn|message|message.delta|reasoning|tool|output|approval|
               usage|plan|op|notice", "turn"?, "item"?, "status"?, ...}: the fields per type are SPEC 8's table.
POST /v0/sessions/{id}/turn       -> {"text": "...", "images"?: ["/abs/x.png"], "model"?: "...", "steer"?: true, "turn"?: "t1"}
POST /v0/sessions/{id}/interrupt  -> {"turn"?: "t1"}
POST /v0/sessions/{id}/answer     -> {"request": "r0", "decision": "accept|accept_session|decline|cancel", "answer"?: "..."}
   <- 200 {"op": "zm-...", "ok": true} once the adapter took it; 202 {"op", "pending": true} when it had not answered
      in 10 s (its op event follows on the stream). Refused by the adapter: 409 (wrong state: a turn already runs,
      no such running turn or pending request) or 400 (bad or unsupported), with the op result in error.data.
      A capability the adapter lacks: 409 before anything is sent. A session that is not structured, or ended: 409.
```

Fresh embedded replay is an **opt-in representation of the same attach route**, with every existing local/operate, task-parent, read-only and redaction check retained. Ordinary `Upgrade: zm-pty` remains raw and byte-for-byte unchanged. A fresh client requests `?replay=all`, optional `input=0`, `Connection: Upgrade`, `Upgrade: zm-pty-replay-v1, zm-pty`, and `Zm-Replay-Surface: fresh-v1`. A current daemon selects `zm-pty-replay-v1` in its101 response. An old daemon can explicitly select the offered `zm-pty`; that is the only fallback, with no retry on auth, missing-session, conflict or transport errors. A framed request without fresh-surface metadata or `replay=all` is400. The CLI enables this only with experimental local `zm attach --embed --replay-modes`; ordinary embedded and remote attaches retain their existing behavior.

Framed **outbound** bytes use `type:u8, length:u32 BE, payload`, maximum payload32,776bytes. Inbound bytes remain the same raw key stream through the shared attach input loop:

| type | payload | meaning |
| --- | --- | --- |
| `M` | exactly52bytes: nonnegative offset:u64 BE +44-byte version1 parser checkpoint | first frame; state immediately before the first raw byte; not an executable escape string |
| `O` | offset:u64 BE +1..32,768 raw bytes | exact retained output at that offset; contiguous after `M`/previous `O` |
| `G` | next offset:u64 BE | later ring eviction skipped bytes; close and reconnect a **new** surface, never reset an existing screen |
| `X` | empty | output ended after all retained raw bytes; EOF without this marker is a dropped stream |

Offsets must fit signed64-bit nonnegative cursors; output length must not overflow the cursor. Unknown types, sizes, versions or metadata enum/flag values are rejected. The fixed checkpoint schema contains only bounded numeric state (at most16 CSI parameters), mode-known flags and partial escape/UTF-8 state; never OSC/DCS payload strings. Its grammar is pinned to Ghostty1.4.0; the version1 layout is defined by `replaymode.CheckpointSize` and its checked binary codec.

The frontend discards only an incomplete leading escape/control-string/UTF-8 fragment until its checkpoint reaches a safe Ground boundary, advancing the **raw** cursor for those bytes. It then writes a generated whitelist prefix only to its new surface: observed alternate/primary screen selection via47 and bracketed paste2004. Prefix length never changes raw offsets, `.out`, output endpoints or task transcripts and never reaches managed PTY input. Mouse selector/encoding restoration is omitted because daemon input-report classification still follows raw output, and rendering metadata grants no input or shared-prompt paste authority. Unknown state, omitted mouse state and skipped leading fragments produce an explicit incomplete-replay notice with recorded-conversation guidance. That notice can be overwritten by terminal redraws; it is not a persistent app badge. Existing holders with already-lost prefix and ended `.out` archives have no retroactive full-state restoration. No saved cursor, screen contents, full VT fidelity or complete conversation reconstruction is promised. The recorded conversation remains available independently.

## Tailnet (tailnet builder)

| route | stability | guard | check |
| --- | --- | --- | --- |
| `GET /v0/tailnet` | beta | local | - |
| `POST /v0/tailnet/up` | beta | local | saves config |
| `POST /v0/tailnet/down` | beta | local | - |
| `GET /v0/peers` | beta | local | - |

```
GET /v0/tailnet          <- {"enabled": true, "running": true, "hostname": "zm-mini", "tailnet": "tail1234.ts.net",
                             "url": "http://zm-mini.tail1234.ts.net:7433", "https_url"?: "https://zm-mini.tail1234.ts.net", "auth_url"?: "https://login.tailscale.com/...", "error"?: ""}
POST /v0/tailnet/up      -> {"hostname"?: "zm-mini", "auth_key"?: "tskey-...", "control_url"?: "https://headscale.example"}
                         <- TailnetStatus   (hostname must be zm-<name>; auth_key falls back to the daemon's
                            TS_AUTHKEY and is never saved; a new auth_key restarts a node waiting for login and is
                            409 once it is logged in; a node whose listener failed is 503 with the error)
POST /v0/tailnet/down    <- TailnetStatus
GET /v0/peers            <- {"peers": [{"machine": "studio", "url": "http://zm-studio.tail1234.ts.net:7433", "online": true,
                             "last_seen": "...", "version": "v0.1.0", "owner": "bob@example.com"}]}
```

On the tailnet listener every request reaches the same handler with `core.WithRemote(ctx, Remote{Login,
Node, NodeID, NodeKey, Tagged, Self})` (`Self`: the daemon's own node, or a caller whose Tailscale address is
one of this machine's); core authenticates it (token, else mapped login, else `401`). Only the Unix listener marks requests
local (`core.WithLocal` via `ConnContext`), so a listener that forgets `WithRemote` still gets token-or-401,
never the owner.

## Tunnel (SPEC 17.11)

| route | stability | guard | check |
| --- | --- | --- | --- |
| `GET /v0/tunnel` | beta | local | - |
| `POST /v0/tunnel/up` | beta | local | listen loopback only; url https (http only to loopback), no path; edge_secret 16-64 lowercase hex |
| `POST /v0/tunnel/preview-domain` | beta | local | configure an existing wildcard HTTPS domain for lease-bound sandbox previews; empty disables publication |
| `POST /v0/tunnel/down` | beta | local | - |
| `POST /v0/tunnel/key` | beta | local | ref a secret reference (keychain:, file:, env:) that holds this daemon's key (else `409`; unreadable `503`), or "" |

```
GET /v0/tunnel   <- {"listen"?: "127.0.0.1:7435", "url"?: "https://zm.example.com", "running": true,
                     "fingerprint": "<26-char key fingerprint>", "error"?: "", "edge_secret"?: true,
                     "edge_seen"?: "<when a request last arrived carrying it>", "key_escrow"?: "<secret reference>"}
POST /v0/tunnel/up    -> {"listen": "127.0.0.1:7435", "url": "https://zm.example.com", "edge_secret"?: "<hex>"}
                      <- TunnelStatus   (starts or moves the listener now, then saves tunnel.* in config.json;
                         a listener config does not record is never left running)
POST /v0/tunnel/down  <- TunnelStatus   (stops it and clears tunnel.* but tunnel.key; the key and the replay journal stay)
POST /v0/tunnel/key   -> {"ref": "keychain:zm-home-key:<fp>" | "file:/path" | "env:NAME" | ""}
                      <- TunnelStatus   (saves tunnel.key only when the reference holds this daemon's key, 409
                         otherwise; a daemon with no key adopts it. The key never crosses this route. "" clears it,
                         409 while the daemon has no key. A file: inside the home or relative is 400)
```

`zm tunnel setup|status|remove` (SPEC 17.11 "Your own domain") drive these. The edge secret arrives either in
`X-Zm-Edge-Secret` or as the request's Host, `zm-edge-<secret>`, which cloudflared's ingress sets
(`originRequest.httpHostHeader`). Every answer on the listener carries `Cache-Control: no-store`.

The listener (config `tunnel.listen`, loopback only, off by default) serves the same handler behind a door. Every
request must carry `Zm-Key` (the caller daemon's ed25519 public key, base64url), `Zm-Date` (Unix seconds),
`Zm-Nonce` (16 random bytes, base64url) and `Zm-Sig`, an ed25519 signature over

```
zm-req/1\n<home fp>\n<METHOD>\n<request URI, RFC 3986 percent-normalised>\n<Zm-Date>\n<Zm-Nonce>\n
<sha256 hex of the signed headers>\n<sha256 hex of the body>
```

where the signed headers are `Authorization`, `Content-Type` and every `Zm-*` but `Zm-Sig`: names in lower case and
sorted, one `<len(name)>:<name><len(value)>:<value>` entry per value, in order (no separators to confuse). `Zm-Nonce`
is 16 to 64 base64url characters; one each of `Zm-Key`, `Zm-Date`, `Zm-Nonce`. Unsigned or badly signed: `401`. A date over 5
minutes from the home's clock, or (with a token) a nonce that key already sent (kept 11 minutes past its date, across
restarts in `$ZM_HOME/tunnel/nonces`): `400`; past a key's replay budget, or the journal cannot be written: `503`;
for 11 minutes after a start whose journal could not be read, every write (not `GET`/`HEAD`): `503`; a body over 16
MiB: `413` (never `401`, which counts toward a replica's purge). Without `Authorization` only `POST
/v0/invites/redeem`; `GET /v0/events` without `live=0` is `400`. The request then
reaches core with `Remote{Via: "tunnel", NodeID: "zmkey:<caller fp>", Addr: <CF-Connecting-IP>}`; a token must be
key-bound to that key id (a minted token is `401`). Every answer carries `Zm-Home-Key` and `Zm-Home-Sig` over

```
zm-resp/1\n<home fp>\n<the request's Zm-Nonce>\n<status>\n<sha256 hex of Content-Type and every Zm-* but Zm-Home-Sig>\n
<sha256 hex of the body>
```

(the headers hashed the same way). Redemption's rate limits key on `CF-Connecting-IP` only from a loopback peer, and
with `tunnel.edge_secret` set only when `X-Zm-Edge-Secret` carries it; else on the peer's address. A replica sends
`Zm-Write-Id` (a new ULID) on every live forwarded write (a route whose row is a `Write`, potluck/kit/teamlink routes.go; never a read, bd sql's POST
included), over either transport. The authority runs each (identity, write id) once: a repeat waits for the first to
finish, then gets its recorded status and body with `Zm-Write-Replayed: 1`; the same id with another method, path,
query or body is `409`, and a body over 8 MiB is `413` (never cut short). Records: `team_writes`, a day, at most 100k per project; over 64 KiB only the status is kept
(the replay answers `{}`); 5xx, 401, rescope and busy answers are not recorded.

A replica drops an answer whose key is not the pinned one or whose signature fails (`403` to its caller), and
treats an unsigned one (the tunnel's own error page) as an unreachable home. It drops every unsigned header.

Sealed secrets (SPEC 17.11 "Sealed secrets"). Every signed request and answer carries `Zm-Seal-Key` (signed like
every `Zm-*` header): the sender's X25519 key certified by its ed25519 key,

```
Zm-Seal-Key: 1.<base64url X25519 public key>.<base64url ed25519 sig over
             "zm-seal-key/1\n<zmkey:fp>\nx25519-hkdfsha256-chacha20poly1305\n<base64url public key>">
sealed blob: zs1.<expiry Unix seconds>.<base64url(HPKE enc || ciphertext)>
HPKE info:   zm-seal/1\n then <len>:<sender key id> <len>:<recipient key id> <len>:<expiry> <len>:<purpose>
```

A member's run-target call holding `secrets` or `jit_config` goes with `Zm-Sealed: 1` and the body
`{"zm_sealed": "<the whole body sealed>"}` (purpose `request\n<METHOD>\n<normalised URI>\n<Zm-Date>\n<Zm-Nonce>`);
the door opens it before routing, for a token caller only, and a blob that does not open (another key or request,
expired, changed, anything beside it) is `400`. A run-target body over the tunnel holding either unsealed, under any
spelling a JSON decoder folds to them, is `403` at the door (the member's transport never sends one). An op the authority relays over the tunnel whose arguments hold either
carries `"sealed": "<blob>"` instead of `"args"` (purpose the JSON array `["run_target op", project, op id, binding,
verb, box]`), sealed to the key on the host's own poll or heartbeat. A peer whose messages carry no certificate
gets neither: `403` naming the tailnet.

## Teams (SPEC 17.3, 17.4; rounds B1 and B2)

A team project has one **authority** (the machine that ran `zm project team`); members' machines run a
**replica** that forwards every team-project work, lease and native issue call to it (`internal/team`; issues: SPEC
"Native issues", "On a team replica"). Tokens from redemption
are **node-bound** (`tokens.node`): they answer `401` from any other tailnet node, on any listener without
WhoIs, and on any Unix socket, so only the replica daemon that redeemed one can use it.

| route | stability | guard | check |
| --- | --- | --- | --- |
| `POST /v0/projects/{project}/team` | beta | local | admin on the project; moves its leases into project.db; `409` while a managed session's bound lease is live |
| `POST /v0/projects/{project}/solo` | beta | local | admin on the project; moves the leases back; `409` while any replica is joined (a live node-bound token) |
| `POST /v0/invites` | beta | local | admin on the project/area; the project is team; at most 10 live per project; this machine's tailnet node runs under its own name (`409` when the tailnet renamed it); a `class` only from an owner or admin of the whole project (else `403`), and only `trusted` or `public` (else `400`) |
| `GET /v0/invites` | beta | local | - |
| `DELETE /v0/invites/{id}` | beta | local | - (a redeemed invite's token stays: `zm token revoke`) |
| `POST /v0/invites/redeem` | beta | public | its own checks, before the code is read: socket `403`, no WhoIs node `401`, this machine `403`; then one answer (`403`) for a wrong, used, expired or removed code, a tagged node without `--allow-tagged`, and a rate-limited node |
| `POST /v0/team/leave` | beta | authed | a node-bound token only: revokes itself (`zm leave` asks this) |
| `POST /v0/team/presence` | internal | authed | a node-bound token only (else `403`); at most 8 MiB and 200 op reports (else `413`); the project visible to it and team (else `404`, like a missing one); the sessions part: every address a session on the machine name recorded at redemption (else `403`); each session's area one the token may write in (else that session's answer carries a not-found error, like a missing area) |
| `GET /v0/team/ops` | internal | authed | a node-bound token only (else `403`); the project visible to it and team (else `404`, like a missing one); only the ops queued for the machine name recorded at redemption; `wait_ms` at most 25 s; `409` rescope or `401` revoked while waiting |
| `POST /v0/team/ops/report` | internal | authed | a node-bound token only (else `403`); the project visible to it and team (else `404`); at most 8 MiB (`413`) and 200 reports; a report on an op that is not its machine's in the project is ignored |
| `POST /v0/team/machine` | internal | authed | a node-bound token only (else `403`); the project visible to it and team (else `404`); recorded only for a token that reads the project whole (an area guest's machine: answered `{}`, not recorded) |
| `POST /v0/team/join` | beta | local | the replica's side of `zm join` (a code, or a project name: a GitHub join, SPEC 22, continued with `login`) |
| `GET /v0/team/remotes` | beta | local | - |
| `DELETE /v0/team/remotes/{project}` | beta | local | the replica's side of `zm leave` |
| `GET /v0/team/project-keys/{project}` | beta | local | the authority key this machine bound the project id to at its first working join (Potluck #143): `{project, via, fingerprint, authority, bound}`; `404` when none |
| `POST /v0/team/project-keys/{project}/forget` | beta | local | this machine's owner forgets that binding (Potluck #163), so the next join binds the id again: `{host_state: "keep"\|"drop", fingerprint}`; the fingerprint must be the bound key's (else `409`, nothing forgotten); refused while a join of the id is recorded here (leave first) or one of its ops still runs here (`409`); a bad id, no fingerprint or another `host_state`: `400`; joins not loaded yet: `503`; answers `{key, host_state, dropped}` |
| `GET /v0/sync` | internal | authed | read on a team project (else `404`, like a missing one); only the caller's visible changes, redacted for it; a reader of some areas also gets a tombstone for an item that left them |
| `GET /v0/sync/snapshot` | internal | authed | read on a team project (else `404`); what the caller may read, redacted for it |
| `POST /v0/team/tasks` | internal | authed | a node-bound token only (else `403`); the project visible to it and team (else `404`); its identity owns or administers the project by its own grants (else `403`); the parent a session on its machine (else `400`); the machine named (else `404`) takes remote tasks (else `403`), trusted (else `403`), runs scripts for a run (else `503`), has a task driver for a sandbox task (else `403`); `auto` with no machine that takes remote tasks (sandbox tasks, for one): `503`; sandbox with run or continue_from, or an unknown size: `400`; at most 100 of a parent's waiting (else `503`); the start waits in the line for a free machine, at most 3 of a parent's placed at once (SPEC 8 "Waiting line") |
| `GET /v0/team/tasks` | beta | authed | this machine's owner on its socket (sent on to a joined project's authority), or at the authority a node-bound token (else `403`); `project` named (else `400`), visible and team (else `404`); its identity owns or administers the project by its own grants (else `403`); parent and session addresses redacted |
| `GET /v0/team/tasks/{id}` | internal | authed | a node-bound token only (else `403`); the machine that started it while its identity still owns or administers the project, else `404` |
| `POST /v0/team/tasks/{id}/cancel` | internal | authed | as `GET /v0/team/tasks/{id}` |
| `POST /v0/team/sessions` | internal | authed | a node-bound token only (else `403`); the project visible to it and team (else `404`); its identity owns or administers the project by its own grants (else `403`); operate on the area (else `403`); a claude or codex structured session with no command, env, mcp, cwd, work, name, prompt or sandbox (else `400`); the machine named (else `404`) not the caller's (else `400`), trusted (else `403`), takes remote tasks (else `403`), heard from in 2 minutes (else `503`); a repeated `client_request_id` answers the same start, a changed request `409`; waits at most 25 s for the session and its presence row (`503` after; retry with the same id) |
| `POST /v0/team/session-events` | internal | authed | a node-bound token only (else `403`); the project visible to it and team (else `404`); the session on its machine (else `403`), a shared structured session of the project in an area it may write (else `404`); every event JSON (else `400`); at most 8 MiB (`413`); a page no reader waits for, or holding a text redacted in the project in the last hour, is dropped |
| `GET /v0/team/tasks/{id}/read` | internal | authed | as `GET /v0/team/tasks/{id}`; `?item=&offset=`; waits at most 30 s for the machine (`503` after) |
| `POST /v0/team/machines/{machine}/t3` | app | authed | a caller on this machine's socket, its OS user (a token or tailnet login: `403`), sent on to the named joined project's authority; at the authority a node-bound token (else `403`), the project visible to it and team (else `404`), its identity owns or administers the project by its own grants (else `403`); `project` named (else `400`); the machine one with numbers for the project (else `404`) with a t3 block (else `403`), heard from in 2 minutes (else `503`); scopes T3 scope names (else `400`); waits at most 25 s for the machine (`503` after); each credential answered once (SPEC 8 "Mezze app on a machine") |
| `POST /v0/team/machines/{machine}/class` | beta | authed | a caller on this machine's socket, its OS user (a token or tailnet login: `403`), sent on to the named joined project's authority; at the authority its own socket or a node-bound token (else `403`), the project visible and team (else `404`), its identity owns or administers the project by its own grants (else `403`); `project` named and `class` `trusted`, `public` or empty (else `400`); the machine one of the project's (else `404`); empty drops an explicit class (SPEC 8 "Operator class") |
| `GET /v0/team/machines/class` | beta | authed | as `POST /v0/team/machines/{machine}/class`, the project in `?project=` (else `400`); every machine of the project with its class and why |
| `POST /v0/team/machines/{machine}/person` | beta | authed | a caller on this machine's socket, its OS user, not a session (a token or tailnet login, or a session: `403`), sent on to the named joined project's authority; at the authority its own socket or a node-bound token (else `403`), the project visible to it and team (else `404`), its identity owns or administers the project (else `403`); `project` named (else `400`); marking: the person is the caller (else `403`), owns or administers the project (else `400`), the machine one of the project's (else `404`) whose live joins are the caller's alone (else `403`); clearing or changing a person's machine is that person's alone, or any owner's once that person no longer owns or administers the project (else `403`) |
| `GET /v0/team/machines/person` | beta | authed | as `POST /v0/team/machines/{machine}/person`, the project in `?project=` (else `400`); the machines that are one person's own |
| `POST /v0/team/writes` | internal | authed | a node-bound token only (else `403`); the project visible to it and team (else `404`); each write runs as its own route, as the replica (and the session in `as`), with that route's checks |
| `POST /v0/team/writes/{id}/cancel` | internal | authed | a node-bound token only, at the authority (`?project=`, else `400`; the project visible to it and team, else `404`); a caller on this machine's socket, sent on to the joined project named by `?project=` or else `Zm-Project` (`zm project team cancel`); waits behind a run of that id; an id older than the live records kept: `409` |
| `GET /v0/team/outbox` | internal | local | the replica's queued writes |
| `GET /v0/people` | stable | authed | ReadScope; a project read whole: every grant holder; else only holders of a grant on an area the caller reads (an area guest learns no whole-project member); the caller's own person flagged `you`; machines and seen from the sessions the caller sees; on a replica merged with every authority's answer |

```
POST /v0/projects/{project}/team   -> {}   <- {"project": {"id", "slug"}, "team": true, "moved": 2}
POST /v0/projects/{project}/solo   -> {}   <- {"project": {...}, "team": false, "moved": 1}
POST /v0/invites   -> {"project": "acme" | "acme/solver", "area"?: "solver", "as": "sam", "role": "member|guest",
                       "ttl_seconds"?: 86400, "allow_tagged"?: false, "via"?: "tailnet|tunnel", "class"?: "trusted|public"}
                   <- {"invite": Invite, "code": "zm-mini:<16-char node fingerprint>:K7QM-3XRT"}
                      (via tunnel: "https://zm.example.com#<26-char key fingerprint>:XXXXX-XXXXX-XXXXX-XXXXX-XXXXX")
   via defaults to the tailnet while this machine's node runs, else the tunnel when tunnel.url is set; a tunnel
   invite without tunnel.url is 409.
   Invite = {"id", "project", "project_id", "area"?, "as", "role", "allow_tagged"?, "class"?, "created_by", "created",
             "expires", "state": "live|redeemed|expired", "machine"?, "node"?, "redeemed"?, "used"?}
   class is the operator class (SPEC 8) the redeeming machine gets: redemption writes it onto the token it mints;
   the redeem request cannot name one.
   No identity, grant or token exists until redemption. 503 when this machine's tailnet node is not running (the
   code names it); 409 when the project is solo or has 10 live invites, or when the tailnet renamed this node (a
   duplicate zm-mini became zm-mini-1: a code naming zm-mini would reach the other node); owner is not an invite role.
GET /v0/invites    <- {"invites": [Invite]}
DELETE /v0/invites/{id}            <- Invite
POST /v0/invites/redeem  -> {"code": "<the whole code or XXXX-XXXX>", "machine": "bob-mac"}
                         -> over the tunnel: {"proof": "<base64url HMAC-SHA256>", "machine": "bob-mac"}, signed by the
                            member's key; proof = HMAC(key: the code hash, "zm-redeem/1\n<home fp>\n<zmkey:...>\n<machine>");
                            a code in the clear there is answered like a wrong one
                   <- {"token": "potluck_...", "cap": {"project": "<ULID>", "slug": "shop", "area"?: "", "role": "member"},
                       "scope_version": 3, "machine": "bob-mac", "identity": "bob", "authority": "mini"}
   One transaction: the identity (if new), the grant (an existing stronger grant is kept) and the node-bound token.
   The same code from the same node returns the same token until that token's first authenticated call; after it,
   and from any other node, 403 like a wrong code. 409 (the code is not spent) when the machine name is the
   authority's own, when the node already joined under another machine name, or when the machine name is held by
   another node. A node holds its name only while it holds a live node-bound token (in any project): after `zm leave`
   or `zm token revoke` the name is free, so a reimaged box (new node ID) rejoins under it with a new invite, and the
   old node may join under another name. Rate limits per 10 minutes: 5 wrong codes per
   node, 50 overall, 10 attempts per invite (a retry from the node that redeemed it is not an attempt). Over the
   tunnel the "node" is the member's key id, and the limits count wrong proofs per client address (CF-Connecting-IP),
   5 each and 50 overall, apart from the tailnet's.
POST /v0/team/leave      -> {}     <- {"revoked": true}
   The token is revoked and the machine's presence in the project ends at once (note `replica left`; bound leases
   released), as it does for `DELETE /v0/tokens/{id}` of a node-bound token and for `zm project solo`. A session that
   holds leases in another project of this authority its machine still has a token for moves there instead: only the
   left project's bound leases are released, and its work, identity and reach are blank until its next heartbeat
   there. An expired node-bound token counts as revoked here.
POST /v0/team/join       -> {"code": "...", "machine"?: "bob-mac", "auth_key"?: "tskey-...", "paths"?: ["/abs"], "cwd"?: "/abs",
                             "wait_ms"?: 10000, "access_id"?: "<CF Access client id>", "access_secret"?: "..."}
                         <- {"remote"?: Remote, "auth_url"?: "https://login.tailscale.com/...", "items": 12, "notes"?: [...]}
   A tunnel code (https://...) skips the tailnet: it redeems with a proof over the tunnel, pins the home's key, and
   stores an Access service token, when given, in $ZM_HOME/remotes/<ULID>.headers (0600, "Name: value" lines). A tailnet code runs zm
   tailnet up first (auth_url: log in, then call again); refuses a zm-<machine> name the tailnet renamed
   (taken), a machine other than this daemon's, a code for this machine, and a slug a local project already has
   (the code still works: rename, then join again; checked before any token file is written). The token goes to
   $ZM_HOME/remotes/<ULID>.token (0600, written atomically) before the first call checks access; a join that fails
   after that removes the file (and, if the token was already used, asks the authority to revoke it). Joining again
   over a `revoked` or `purged` remote of the same project revokes its old token first.
   Remote = {"project": {"id", "slug"}, "authority": "mini", "url": "http://zm-mini:7433", "fingerprint", "via": "tailnet|tunnel", "cap",
             "scope_version", "machine", "identity", "areas"?, "role"?, "paths"?, "joined", "state": "ok|purged|revoked",
             "last_error"?, "token_file", "unsent"?, "unsent_count"?}
   unsent: when a dropped copy (leave, purge, revocation) still had pending writes, the JSON Lines file they were
   saved in next to the copy ($ZM_HOME/replicas/<ULID>.db.unsent.jsonl) and its line count; zm never re-sends them.
GET /v0/team/remotes     <- {"remotes": [Remote]}
GET /v0/people?project=  <- {"people": [{"id": "github:4242|01J...", "name": "bob", "kind": "human|agent",
                              "github"?: "bob-gh", "names"?: ["bob"], "you"?: true, "aliases"?: ["cc"],
                              "projects": [{"project": "01J...", "slug": "shop", "identity": "01J...", "name": "bob",
                                            "answers"?: true}],
                              "machines"?: ["bob-mac"], "seen"?: "..."}], "skipped"?: [...]}
   Teammates for a cockpit's Team tab (SPEC 17.3 "People"). id is the stable person id: the verified GitHub account
   the identity is linked to at its authority, else its identity ULID there. identity and name are the person's
   identity ULID and name (its human:<name>) at that project's authority; aliases are its exact label names. A
   member's socket merges every authority's rows by id, never by name (an authority without ids: by name). you
   marks the caller's own person; names are every name of the person's identities the caller sees. Roles and areas are not shown,
   save answers (the person owns that project; only to a caller reading it whole); machine identities are left out.
DELETE /v0/team/remotes/{project}  <- {"project", "removed": ["<token file>"], "revoked": true}
POST /v0/team/presence   -> {"project": "<authority ULID>", "ops"?: [MachineOpReport], "parts"?: {"<name>": <the part's JSON>}}
                         <- {"ops"?: [MachineOp], "parts"?: {"<name>": <the part's answer>}}
   The heartbeat (potluck/kit/teamlink): op reports and pending ops are common to every beat; the rest is the
   registered parts', each under its name (unknown parts are ignored; a part an authority does not have gets no
   answer). Parts today: "sessions" (internal/team, below) and "run_targets" (a member's box adverts for the project:
   [RunTargetAdvert], kept per machine at the authority, none when a beat omits it; no answer). Its body limit is
   8 MiB (413). A daemon without internal/team (kit only) beats with no parts and takes machine ops all the same.
   "sessions" part -> {"sessions": [{"address": "bob-mac/4bb7545a", "area"?: "solver",
                             "agent"?: "claude", "title"?: "...", "state": "running|idle|needs_human", "note"?: "...",
                             "managed"?: true, "started": "...", "work"?: ["local:k3x9qa"], "activity"?: "Searching",
                             "activity_at"?: "...", "term_title"?: "...", "parent"?: "bob-mac/9c0e11aa"}],
                             "elsewhere"?: ["bob-mac/9c0e11aa"]}
                   answer <- {"sessions": [{"address", "error"?: "not_found: no area \"x\"", "held": ["local:k3x9qa"],
                             "notice"?: [NoticePart], "waiting"?: 1}], "human"?: [NoticePart],
                             "elsewhere"?: [{"address", "held": [], "notice"?: [NoticePart], "waiting"?: 1}]}
   A replica's heartbeat (every 15 s, and within ~1 s of a local session event): every live session of the project on
   the replica, plus (with no area) any registered in a project this authority does not serve that holds a key in this
   one and in no project here of a smaller id (one row per session at an authority: never in two of its heartbeats),
   so its bound leases follow it (SPEC 10). A session in another project's heartbeat to the same authority is sent as
   a `sessions` entry `{"address", "elsewhere": true}` only (not the request's `elsewhere` list below, though such a
   session is in that list too when registered outside this project): not ended here, its row left as it is, no
   answer data in `sessions`; the authority marks the machine's other sessions in it ended (bound leases they held are released). A
   guest's session with no area is in its area. held = the keys with a live lease held by the address (the replica's
   lost-lease check); notice = its unread mail in the project; waiting = needs=human mails it sent still open; human =
   the replica identity's own unread mail (and its machine's needs=human mail). elsewhere (optional, SPEC 9 "Across
   projects") = the addresses of the replica's live sessions registered outside the project (each on the recorded
   machine name, else `403`), kept in memory so mail in the project reaches them; each is answered with its notice and
   waiting. activity, activity_at and term_title
   (a replica sends it only for claude, codex, gemini or opencode running their own binary; the authority drops it
   for any other agent) are stored with the row (clipped as on the hook route; an activity_at ahead of the authority's clock is taken as
   now) and served by GET /v0/sessions and GET /v0/sessions/{address} with who (the token's identity) and reach (the
   token's cap: project, or area); a change of them alone publishes dev.zm.session.updated at the authority.
   NoticePart = {"project", "slug", "unread", "group"?, "announce"?, "owners"?, "quiet"?, "direct_wakeable"?: integer, "from"?: [address, redacted]}
   The sessions part is one complete replacement, never independent pages. The beat's dedicated body limit is 8 MiB; the global
   request limit and public session pages are unchanged. Bounds: 10000 live/address-only session entries plus 500
   ended-session prompt-ack entries, 10000 Elsewhere addresses, 500 ended outcomes (21000 rows total), 10000 delivered
   prompt ids and 100000 reported work keys. Overbudget bodies/rows are 413 before presence or prompt acknowledgments
   change. The sender uses a complete local inventory; an uncertain full 500-row legacy list is not sent. Recent
   ended outcomes are limited to the newest 500 local outcomes in 24 hours; unsent prompt acknowledgments remain
   pending for later complete heartbeats. Live/Elsewhere listings are not truncated. Unknown fields are ignored;
   older authorities enforcing 500 sessions must be upgraded before sending larger complete heartbeats.
   Machine ops (SPEC 8 "Machine choice"): the request may carry "ops": [MachineOpReport] (at most 200, else 413) and the
   answer "ops": [MachineOp] (at most 20, oldest first) for the beat's machine. MachineOp = {"id", "kind", "from"?,
   "payload"?}; MachineOpReport = {"id", "kind"?, "error"?, "code"?, "hint"?, "payload"?}. The payload is the kind's
   (registered by the module that owns it): task_start {"parent", "depth", "request": TaskStartRequest} -> Task;
   task_cancel {"task"} -> Task; task_read {"task", "item"?, "offset"?} -> TaskHistoryPage; session_op {"session",
   "op": SessionOp} -> SessionOpResult; t3_ensure {"label", "scopes"?} -> T3Environment; session_start {"request":
   SpawnRequest, "area"?, "role"?} -> Session; door_delivery DoorDeliveryOp -> DoorDeliveryResult; run_target
   RunTargetOp -> the driver's answer. A machine runs
   an op at most once (recorded before it runs, kept across a restart) and reports it, and a started task's status,
   in each beat until an answer took its final word; it beats every 2 s while it has such a report.
GET /v0/team/ops?project=<authority ULID>&wait_ms=20000   <- {"ops": [MachineOp]}
POST /v0/team/ops/report -> {"project": "<authority ULID>", "ops": [MachineOpReport]}   <- {}
   Ops without the heartbeat's wait: a machine that wants them (today: one that lends boxes to
   the project, its last heartbeat there carried `run_targets`) holds the poll open; the authority answers as soon as an op is queued for
   it (else empty after wait_ms), the same ops a heartbeat answer would carry. The machine posts each run_target
   and door_delivery op's report as soon as it ran; one the post did not deliver goes with the next heartbeat. An authority older than
   the routes answers 404: the heartbeat alone carries ops.
POST /v0/team/sessions   -> {"project": "<authority ULID>", "machine": "auto|<name>", "area"?: "...",
                             "request": SpawnRequest}   <- Session (on that machine, shared, mode pipe)
POST /v0/team/tasks      -> {"project": "<authority ULID>", "machine": "auto|<name>", "parent": "<address>", "depth": 1,
                             "request": TaskStartRequest}   <- Task (id rt-…, machine, state pending|running|ended|refused)
GET /v0/team/tasks/{id}  <- Task;  POST /v0/team/tasks/{id}/cancel <- Task;  GET /v0/team/tasks/{id}/read <- TaskHistoryPage
   The waiting line (SPEC 8): request.front puts the start before every non-front one; a named machine pins it. A
   waiting start is state pending with "position" (1 is next) and machine its pin ("" for auto); once placed,
   machine is where it runs. "requeued" counts its returns after a lost machine, "acted" the outward actions zm
   saw, "lost" (on an ended one) why it did not go back. A write from the session of a task's copy on a lost machine
   is refused 409 conflict at the authority, live or from the outbox; while that copy may still run, so are the
   machine's forwarded writes without Zm-As. A machine that takes remote tasks holds GET /v0/team/ops; its numbers
   carry "task_slots", and with a task driver "sandbox_tasks" and "sandbox_slots". request.sandbox places it only
   there, on its sandbox slots; Task carries "sandbox" (SPEC 8 "Where it runs").
GET /v0/team/tasks?project=<slug|authority ULID>   <- {"project", "tasks": [Task, ...]}
   The waiting line in order, then the placed tasks, then the 20 newest settled (`zm tasks -P <project>`).
POST /v0/team/machine    -> {"project": "<authority ULID>", "stats": MachineStats}   <- {}
POST /v0/team/machines/{machine}/class -> {"project": "<slug|authority ULID>", "class": "trusted|public|"}
   <- MachineClass {"machine", "class", "why", "set_by"?, "set_at"?, "from_invite"?}
   from_invite: the class is the one the machine's invite carried (no class set since); empty drops back to it.
GET /v0/team/machines/class?project=<slug|authority ULID>   <- {"project", "machines": [MachineClass, ...]}
   Operator class (SPEC 8): the authority decides it; on this machine's socket (`zm machine-class [<machine>
   trusted|public|default] -P <project>`) the call goes to the project's authority.
POST /v0/team/machines/{machine}/person -> {"project": "<slug|authority ULID>", "person": "<name>|"}
   <- MachinePerson {"machine", "person"?, "set_by"?, "set_at"?}
GET /v0/team/machines/person?project=<slug|authority ULID>   <- {"project", "machines": [MachinePerson, ...]}
   A person's own machine (SPEC 8 "Operator class"): an action an ask carries runs there only on
   that person's signed yes; "" makes it a team machine again (either owner's). Only the person marks it
   (`zm machine-class <machine> person <name>|team -P <project>`), owning or administering the project, on a
   machine they alone joined, never from a session (one that says it is: a process of their OS user is not told
   apart); only they clear it unless they no longer own the project.
   Listed: only the machines that are a person's.
POST /v0/team/machines/{machine}/t3 -> {"project": "<slug|authority ULID>", "scopes"?: ["orchestration:read", ...]}
   <- T3Environment {"machine", "environment_id", "version", "commit", "port", "urls": ["http://<ip>:<port>", ...],
      "started"?, "credential", "scopes"?, "expires_at"}
   Mezze app on a machine (SPEC 8): on this machine's socket (`zm t3 <machine> -P <project> --json`) the call goes to the
   project's authority (a hosted project's is this one), which queues a t3_ensure op for machine and waits up to 25 s.
   The credential is single use and good for 5 minutes; the authority blanks it as it answers or the wait ends (a
   later report keeps none), the machine once a heartbeat answer took its report. A 503 after the wait: ask again (a
   running server only mints a new credential). An authority older than the route: 503. A machine without a "t3" key
   in its config: 403 (its owner runs `zm t3 enable` there); one missing git, node or vp, or under 8 GB free for a
   clone or install: 503 naming it.
   A replica's numbers (SPEC 8 "Machine numbers"), sent after a heartbeat to that authority went through, at most every
   30 s per project; the authority keeps the latest per (machine, project) for GET /v0/machines (through the session module, else in memory until it restarts; a kit-only machine sends -1 for every number). An authority without
   the route (older) answers 404 or 405: the replica stays quiet and tries again an hour later.
```

**Local-first replicas (round C, SPEC 17.6).**

```
GET /v0/sync?project=<ULID>&since=<event id>&limit=500&wait_ms=20000&gen=<restore generation>
    <- {"changes": [Event], "cursor": "<id>", "more"?: true, "now": "<authority clock>"}
   The caller's visible changes after since, oldest first (a team project's event log is its changelog: kept 30
   days and 200 000 rows, whichever is longer). cursor only moves to an id the caller was sent (with nothing visible
   it stays at since). wait_ms (at most 25 s) long-polls: the answer comes when a visible change commits. A waiting
   call ends with 409 rescope when the caller's scope_version moves, and 401 when its token is revoked. A reader of
   only some areas gets {"type": "dev.zm.sync.gone", "subject": "<item id>", "data": {"id", "gone": true}} for an item
   whose previous work event was in an area it reads and whose next is not. 410 resync: since is older than the
   rows kept, or gen (the snapshot's generation; sent even when empty) is not this home's restore generation
   (take a snapshot). No gen: unchecked (older replicas). 503 when the home's generation file cannot be read.
GET /v0/sync/snapshot?project=<ULID>   <- NDJSON, one {"kind", "data"} per line:
   item (WorkItem, every status, links checked per target), comment, lease, mail, all redacted for the caller, then
   {"kind": "end", "cursor": "<id>", "now", "generation"?}: generation is the home's restore generation, absent
   when it was never restored (zm backup restore writes a new one; 503 when it cannot be read). The cursor (the newest id the caller may read) is read before the rows,
   so a change committed meanwhile comes again through sync. A stream without its end line is incomplete.
   {"kind": "error", "data": ErrorResponse} ends a stream that failed.
POST /v0/team/writes?gen=<restore generation> -> {"project": "<ULID>", "writes": [{"id": "<ULID>", "method": "POST", "path": "/v0/work/local:k3x9qa/comment",
                          "body"?: {...}, "as"?: "bob-mac/4bb7545a", "item"?: "local:k3x9qa", "base"?: "<event id>"}]}
                     <- {"results": [{"id", "status": 200, "body": <the route's answer>, "seen"?: "<event id>", "replayed"?: true}],
                         "stopped"?: "<why the rest did not run>"}
   At most 200 per batch, run in order, one batch per (identity, project) at a time. Each write runs as the route it
   names (POST /v0/work, /v0/leases, /v0/mail, /v0/work/{id}/claim|release|comment|status|link, /v0/mail/{id}/ack|reply,
   DELETE /v0/leases/{key}; anything else is a recorded 400) with Zm-Write-Id = id: a comment or mail is stored under
   that id, so a replay never duplicates. Its answer is recorded per (identity, id) for 30 days and a replay gets the
   record (replayed). A status write with base is refused (409 conflict, recorded) when the item's status changed at
   the authority after base; earlier writes of the same batch count as seen. seen = the newest change id about item
   after the write. An answer that means "not now" (5xx, unavailable, locked, 401, rescope) is not recorded and stops
   the batch: the replica retries from that write, in order. gen (a query parameter, so an older home ignores
   it), when sent and not this home's restore generation, refuses the whole batch with 410 resync before any
   write runs (the replica snapshots first); 503 when the home's generation file cannot be read.
POST /v0/team/writes/{id}/cancel?project=<ULID>   <- {"state": "cancelled"} | {"state": "ran", "status": 200}
   Settles one write id (a live or queued write's). ran: the id is already recorded (its status only, never its body).
   cancelled: it never ran and never will (a tombstone, kept 7 days): any delivery of that id, live or queued, is
   409 conflict "this write was cancelled; it never ran". An id too old for its record to be kept: 409 "too old to settle". Zm-No-Queue: 1 on a replica's claim, lease or release
   (POST /v0/work/{id}/claim|release, POST /v0/leases, DELETE /v0/leases/{key}) never queues it: 503 unavailable with
   data {"write"?, "outcome": "not_sent|cancelled|unknown"}; a direct daemon ignores the header.
GET /v0/team/outbox?project=&all=1   <- {"entries": [{"id", "project", "kind", "item"?, "as"?, "state": "pending|acked|conflict|failed",
                                         "error"?, "created", "done"?, "body"?}]}   (acked only with all=1)
```

On the replica, a joined project whose first snapshot landed is served from its copy (`$ZM_HOME/replicas/<ULID>.db`):
`GET /v0/work` (filters as at the authority), `/v0/work/{id}` (with comments), `/v0/work/{id}/comments`,
`/v0/work/inflight`, `/v0/work/rollup`, `/v0/leases`, `/v0/work/sources` (one `authority` row: `sync.state` loading, ok or stale,
`lag_seconds` the copy's age, `error` the last error, sanitised, `items` the copy's item count, which holds what its
token reads), `/v0/mail`, `/v0/mail/inbox` (and `/peek`), `/v0/mail/sent`,
`/v0/mail/{id}` and `/thread`, and the hook notice: no tailnet call. Lists from a stale copy (no answer from the
authority for 30 s, or the last call failed) carry `"behind": [{"source": "authority", "state": "stale", "synced",
"error"}]` and an item `"stale"`. `GET /v0/work/{id}/holder` asks the authority while it answers (presence is not in
the copy). `GET /v0/projects` lists a joined project whose authority is away from the replica's header and names it in
`skipped` too. Writes commit to the outbox first: a comment, status or link answers at once (overlaid on the copy until
the copy shows it; a status write carries `base`, the item's newest change id the copy had, and its answer `write`, the outbox entry's
id; `zm work status` and MCP `work_set_status` wait up to 3 s on it: a refusal is an error naming `zm outbox`, still
pending says queued); a claim, release, lease,
mail, reply or ack waits up to 5 s for the authority's answer while it is reachable and relays it, else answers at
once: `202 {"queued": true, "write": "<id>", "project", "item", "note"}` for a claim, release or lease, the mail (`via`
"queued (...)") or an optimistic ack otherwise. A queued write the authority refuses is kept in the outbox as
`conflict` (a claim that lost, a stale base, group mail taken first) or `failed`, never retried; a session whose queued
claim, lease or status lost becomes `needs_human` (note `queued claim conflict: <key>`). `POST /v0/work` still goes to
the authority (an item is made there; `503` while it is unreachable) and the new item enters the copy at once. The
outbox holds at most 10 000 writes; after 7 days without an answer writes are refused. `GET /v0/team/remotes` carries
each copy's `replica`: `{state, cursor, synced, age_seconds, reachable, pending, conflicts?, last_error?}`. `zm leave`,
a purge and a revocation delete the copy.

**Mail and presence across machines (round B2).** On a team project's authority, mail to a session on a joined
replica (known by its presence, any state; an unknown one is `404` like a missing one; filed in the session's area
unless `area` or `ref` says otherwise), to an item whose holder is
such a session, and to `machine:<replica>` (a machine joined to the project; only whole-project readers and that
replica itself may name another machine's group mailbox, others get `404` as for a machine that never joined) is
stored at the authority like local mail. Presence rows answer like local sessions in `GET /v0/sessions`,
`GET /v0/sessions/{address}` (with `seen`, the last heartbeat), mail routing, `zm who` (`GET /v0/work/{id}/holder`
carries the holding `session` when the caller sees it) and lease binding: a claim by a replica session whose presence
says `managed` is a bound lease. A heartbeat older than 60 s shows the session `idle` (note `presence stale`), so its
bound leases stop renewing and expire after `bound_lease_ttl`. A presence row ends (`dev.zm.session.ended`, bound
leases released) when its machine no longer holds a usable node-bound token for the project (`zm leave`, `zm token
revoke`, `zm project solo`, or `zm revoke` of the identity's last grant there: note `replica left`, at once and on
the 15 s session tick; one holding leases in another project of the authority its machine still has a token for
moves there instead, releasing only the left project's bound leases) or when its heartbeat is older
than `bound_lease_ttl` + 2 min (at least 10 min: note `presence lost`); a replica that comes back heartbeats its
sessions live again. A node-bound token never answers as the authority's own machine (`Zm-As` of its sessions or
`machine:<authority>` is `403`, and its identity is not the authority machine's human), whatever name it records. On
the replica: `GET /v0/mail`, `/v0/mail/inbox` (and `/peek`),
`/v0/mail/sent` and `GET /v0/sessions` are merged with every joined authority's answer (the authority's copies of this
machine's own sessions are dropped: they are answered here); `GET /v0/mail/{id}` and its `ack`, `reply`, `thread`,
and `GET /v0/sessions/{address}` for another machine's session go here first, then to each authority while `404`;
`POST /v0/mail` goes by the write rules above (also: the calling session's own team project when no local rule
matched by path or remote). `GET /v0/mail/notice` and the hook route are never proxied: they add the caller's team
mail as of the last heartbeat. A session that sends needs=human mail through the proxy is `needs_human` (`waiting on
mail <id>`) on the replica until a heartbeat says none of its needs=human mail is open (then `idle`, `answered`). A
key a session claimed through the proxy that a heartbeat no longer lists as held (and that it did not release through
the proxy) is a lost lease: the session becomes `needs_human` with note `lease lost: <key>`. Sessions registered or
spawned on a replica in a joined project (an explicit `--project`, a `.zm/project.toml` naming the slug, or a recorded
checkout) are local rows with the authority's project ULID, their events go to this machine's own (owner-only) log,
and a guest's session gets its area (another area is `404`; register falls back to `none`). `spawn --work` of a team
item is refused (`400`, not built).

**The proxy** (replica side, socket callers without a token; a caller presenting a `zm token mint` token acts in this
machine's projects only and gets the same not-found as for any project it cannot see): a work or lease call goes to a joined project's
authority when `?project=` or `Zm-Project` names it (its slug, or the authority's ULID, optionally `/area`); a list
naming no project (`GET /v0/work`, `/v0/work/sources`, `/v0/work/inflight`, `/v0/work/rollup`, `/v0/leases`) is this machine's answer
merged with every joined project's, an authority that does not answer named in `skipped` (`source: "authority"`;
`sources` gets an `ok: false` row); an item route naming none tries this machine, then each joined project while the
answer is 404; `POST /v0/work` and `POST /v0/leases` naming none go to the joined project whose `.zm/project.toml`,
recorded checkout, or path or remote rules (the header's `repos` and `area_repos`, matched like a local project's;
a local path as long or longer wins) take the cwd (unless a local rule named a project explicitly). Forwarded calls carry the
node-bound token, `Zm-Scope-Version`, `Zm-As` when the caller is a session on this machine (the authority accepts
`Zm-As` only for the token's identity, `machine:<m>` and sessions `<m>/<s8>` of the machine name recorded at
redemption), and never `Zm-Cwd`. `release --force` is refused on the replica (`403`). After `409 rescope` the replica
takes the new header and sends the call once more; a header with no role (the identity's grants in the project were
revoked outright) stops the remote: state `revoked`, `last_error` naming the authority, every call to it `401`
"access ended" with a hint (a new invite, or `zm leave`, which still revokes the token), lists naming it in
`skipped`. Three `401`s, the first and third at least 10 minutes apart, purge the remote (token file deleted, state
`purged`). `GET /v0/projects` on a replica adds the joined projects and names any it could not get in
`"skipped": [{"project", "source": "authority", "error"}]` (`zm project ls` warns); `GET /v0/projects/resolve`
there answers a joined project with rule `team` when no local rule matched but the proxy's write rule does. Merged lists ask every
authority at once, and connecting to one (the dial through this node and the fingerprint check) is bounded at 2 s, so
an unreachable authority costs a list about 2 s, not the 30 s a forwarded call may take once connected (item routes
naming no project still try authorities one after another, each bounded the same way). A forwarded read on the
hedged list (`Hedged` route rows, potluck/kit/teamlink routes.go, registered by each route's module: the work, lease, mail-by-id and thread, session, machine and issue reads, and
`POST /v0/issues/sql`; never a write, `GET /v0/mail/inbox` (it marks what it answers seen), `wait`/`wait_ms` or an
event stream) that has heard nothing from the authority for 1 s (bd sql: 4 s until its answer starts, as the
authority may still be copying tables) is sent again on a new connection, at most twice, and the first whole answer
wins: one stuck connection costs a read about 1 s, not its TCP retransmission backoff. Hedges are at most 10% of the
last minute's hedged-route reads (at least 3 a minute); past that a read waits on its one connection. An answer is
read whole (up to 1 MiB) before it is relayed; a longer one still arriving is relayed as it comes. A forwarded body over 1 MiB is
`400 body over 1 MiB` (never forwarded cut short). Each redemption publishes `dev.zm.team.joined`
(machine-level, data `{project, area, role, identity, machine, node}`).

## Phone push (SPEC 16 "Phone alerts")

Web Push to the devices paired on the Decisions page: they subscribe with `/v0/approvals/push-key`,
`/v0/approvals/push` and `/v0/approvals/push/off` (below). Every alert opens `/v0/approvals`.

| route | stability | guard | check |
| --- | --- | --- | --- |
| `POST /v0/web/push/test` | beta | local | local: send a test to the machine owner's subscriptions |

```
POST /v0/web/push/test <- {"sent": 1}  (local only; `zm web test-push`); 404 when the owner has no active phone
```

## Approvals (approvals builder)

Decisions on asks (SPEC section 16). The project's owner or admin answers a card ask from their own machine
(`POST /v0/asks/{id}/choose`); every process of that OS user reaches the socket, agents too. A paired approver
(a device bound to its tailnet node, stable node ID **and** node key, or a confirmed key) is the opt-in for when
that matters, and the only way to decide a pull request's ask.

| route | stability | guard | check |
| --- | --- | --- | --- |
| `POST /v0/approvers` | beta | local | exact canonical name; refused if its confusables skeleton matches another approver or identity |
| `POST /v0/approvers/self` | beta | authed | a human identity for its own phone only, never a session; the phone waits for an existing approver's confirm (never bootstraps) |
| `GET /v0/approvers` | beta | local | - |
| `GET /v0/approvers/self` | beta | authed | a human identity, never a session: only its own person's approvers (added for it, `--for` or pair-me; this machine's owner also those for nobody); a member forwards it to the project's authority |
| `DELETE /v0/approvers/{name}` | beta | local | exact name only (no case, prefix or lookalike match); takes effect at once |
| `POST /v0/approvers/{name}/confirm` | internal | device | caller's node is an active approver; the named one is paired and unconfirmed; never itself |
| `GET /v0/approvals` | internal | device | the device page: pairing form, or (active approvers) open asks and confirmations |
| `GET /v0/approvals/device` | internal | device | the page's data as JSON |
| `GET /v0/approvals/app.js` | internal | device | none: the page's script (renders the device view as text) |
| `GET /v0/approvals/sw.js` | internal | device | none: the page's service worker (shows phone alerts, opens the page on a tap) |
| `GET /v0/approvals/zm-decisions.js` | internal | device | none: the Mezze app's Decisions logic, generated from t3code's zm-client (scripts/sync-zm-client.sh) |
| `GET /v0/approvals/manifest.webmanifest` | internal | device | none: the web app manifest (Add to Home Screen) |
| `GET /v0/approvals/icon.png` | internal | device | none: the home-screen icon |
| `GET /v0/approvals/push-key` | internal | device | an active approver's node; the Web Push public key its browser subscribes with |
| `POST /v0/approvals/push` | internal | device | an active approver's node; phone alerts for that device's person, while it stays active |
| `POST /v0/approvals/push/off` | internal | device | an active approver's node; removes that node's own subscription |
| `POST /v0/approvals/got-it` | internal | device | an active approver's node; one for-you mail addressed to that device's person (its `--for` identity, else this daemon's machine owner), else not_found; the owner's in a joined project is marked read at its authority with the node token |
| `POST /v0/approvals/pair` | internal | device | one-time code; refused from this machine, tagged nodes, zm nodes (hostname or name `zm-*`) and bound nodes |
| `POST /v0/approvals/pair-phone` | internal | device | no code: a phone (node OS iOS or android) signed in to the tailnet as this daemon's own login; refused as `pair` is; answers the approver (`active`, or `unconfirmed` until an approver taps) |
| `GET /v0/approvals/key` | beta | authed | none: the approver paired to `?key=p256:<16 hex>`, else not_found; a member forwards it to the authority |
| `DELETE /v0/approvals/key` | beta | authed | none from the caller: the body's signature by that key (`zm-unbind-key/1`, single-use nonce); a key bound by a sign-in only from its own node; a member's call must carry `origin_checked` (else 403 `member-too-old`); a member forwards it to the authority |
| `POST /v0/approvals/pair-key` | beta | authed | one-time code and proof of the key; stays unconfirmed until an approver on another device confirms |
| `POST /v0/asks` | stable | authed | write on the resolved project/area (WriteTarget), and on the work item; `to` owners, the asker's machine's human or (note card) a person who owns or administers the project; sends needs=human mail |
| `GET /v0/asks` | stable | authed | ReadScope; each ask only to readers who may `operate` on its area or are a party (asker, recipient); `?q=` ranks the same set by shared words; admits an answer-only token |
| `GET /v0/asks/{id}` | stable | authed | same rule, else not_found (identical to a missing id); admits an answer-only token |
| `POST /v0/asks/{id}/decide` | internal | device | active approver node (ID + key match, not this machine); one decision per ask |
| `POST /v0/asks/{id}/answer` | stable | authed | none from the caller: the body's signature by an active key approver whose person owns or administers the ask's project; a key bound by a sign-in only from its own node, while its person stays linked to its account, and a member's call must carry `origin_checked` (else 403 `member-too-old`); never a pull request's ask |
| `POST /v0/asks/{id}/choose` | stable | authed | read on the ask (else not_found), then owner or admin of its project (core.Deps.ProjectAdmin): by scope (this machine's owner on the socket, an owner-role token, or a tailnet login mapped to an owner), or a member machine's node-bound token by its identity's own grant; never a pull request's ask; admits an answer-only token (as an owner token, and never a choice that carries a confirm word: `403` `signed-only`) |
| `POST /v0/asks/{id}/undo` | beta | authed | read on the ask (else not_found), then owner or admin of its project, as `choose` judges it; only an applied default not yet answered |
| `POST /v0/asks/{id}/device-undo` | internal | device | an active approver's node (stable ID + key), checked before the ask is looked up; only an applied default not yet answered |
| `POST /v0/asks/{id}/withdraw` | stable | authed | read on the ask, and its asker or operate on its area |
| `POST /v0/asks/{id}/action/retry` | beta | authed | read on the ask (else not_found), then its asker, or a person (never another session) who owns or administers its project, as `choose` judges it (else 403); `card_sha256` must be the ask's; only a run or command action that failed before it started (`retryable`), unconfirmed, within an hour of the yes, at most 3 times (else 409); starts the identical approved action as its approver, checked again |
| `POST /v0/asks/{id}/action/confirm` | beta | authed | read on the ask (else not_found), then owner or admin of its project, as `choose` judges it; only an action that is done or failed; changes nothing but where the ask is listed |
| `POST /v0/asks/{id}/object` | beta | authed | read on the ask (else not_found), then owner or admin of its project, as `choose` judges it, never a session (403); only an answered ask (else 409); records the owner's objection (`objection`) and does nothing else; the first press stands |
| `POST /v0/asks/{id}/step` | beta | authed | read on the ask (else not_found), then only the person it is for (by name or directory person), never a session or another person (403); card_sha256 must be the ask's; only a card with a person's step, approved and not done (else 409); records it and mails the asker to run the check; closes nothing |
| `POST /v0/asks/{id}/check` | beta | authed | read on the ask (else not_found), then its asker or operate on its area (evidence: its asker or a person who owns or administers the project); only a card with a verify check that was approved |
| `POST /v0/steps` | beta | authed | write on the work item's area (WriteTarget, then the item); `to` names a member of the whole project, for a caller who reads all of it (else 403); `draft`: read on the step (else not_found), then its adder or operate on its area (else 403), only a draft (else 409); `from_ask`: read on the ask (else not_found), then its asker or operate on its area (else 403), only an open, unanswered hand ask with a verify check, for one person, on a work item (else 409); the step judge passes it (fail 400, no answer 503) |
| `GET /v0/steps` | beta | authed | ReadScope: read on the step's area, and operate there, its adder or the person it is for |
| `GET /v0/steps/{id}` | beta | authed | read on the step's area, and operate there, its adder or the person it is for, else not_found |
| `POST /v0/steps/{id}/drop` | beta | authed | read on the step (else not_found), then its adder or operate on its area (else 403); not done or dropped (else 409) |
| `POST /v0/steps/{id}/check` | beta | authed | read on the step (else not_found), then its adder or operate on its area (else 403); not a draft, done or dropped (else 409) |
| `GET /v0/bundles/{id}` | beta | authed | the steps card's steps the caller may read, as `GET /v0/steps/{id}`; none is not_found; `done` is the whole card's |
| `POST /v0/bundles/{id}/steps/{n}/report` | beta | authed | read on the step (else not_found), then the person it is for (any of their sessions) or operate on its area (else 403); only a carded, reported or failed step (else 409) |
| `GET /v0/run-targets` | beta | authed | operate on the resolved project/area (WriteTarget); the project's machines an approved action may run on and whose they are (`?person=`) |
| `GET /v0/permits` | beta | authed | operate on the whole resolved project (WriteTarget); the permit kinds, the project's standing permissions and box marks |
| `GET /v0/permits/card` | beta | authed | none: the frozen card for a kind (`?kind=&days=`) or a box class (`?machine=&class=`), from the daemon's closed list |
| `GET /v0/permits/dryrun` | beta | authed | operate on the whole resolved project (WriteTarget); per dry-run kind, cards a permit would have covered and how they were answered (`?from=&to=`, RFC 3339; default the last 7 days) |
| `POST /v0/permits` | beta | authed | operate on the whole project of the named ask (else not_found); the grant is the ask's signed yes (method approver) by an approver who owns or administers the project now, on exactly the card `GET /v0/permits/card` makes |
| `POST /v0/permits/{id}/revoke` | beta | authed | operate on the permit's whole project (else not_found), then owner or admin of it (core.Deps.ProjectAdmin); unsigned; ends every live permit of its kind |
| `POST /v0/permits/boxes/unmark` | beta | authed | operate on the whole resolved project, then owner or admin of it (core.Deps.ProjectAdmin); unsigned; voids older answers for that box even when no mark is on |
| `GET /v0/rulings` | beta | authed | read on the whole resolved project (an area guest is forbidden); its owner rulings (`[[rulings]]`) |
| `POST /v0/rulings` | beta | authed | a person who owns or administers the whole resolved project (core.Deps.ProjectAdmin), or anyone who operates on all of it (a member, an agent session), else 403; this daemon holds its file; the named ask, the authority, is the project's (else not_found), answered with the choice its card proposes, plainly, with a signature (method approver) by an approver who owns or administers the project now, carrying the ruling's name and text whole, each in double quotes: anywhere on the card for the person, only in the approved action's brief or plan for anyone else; one ask adds a ruling of a name once, ever (else 409) |
| `DELETE /v0/rulings/{name}` | beta | authed | a person, never a session (403), who owns or administers the whole resolved project (core.Deps.ProjectAdmin), whose file this daemon holds; a missing name is not_found |
| `GET /v0/decisions` | stable | authed | ReadScope: asks as `GET /v0/asks`, work items by read on their area; admits an answer-only token, which gets asks only (`work=0` forced, `label=` and `for=` ignored) |
| `GET /v0/aliases` | beta | authed | the identity itself (default the caller), the machine owner on the socket, or another identity that is admin of every project it holds a grant in; else the 404 of a missing identity |
| `POST /v0/aliases` | beta | authed | the machine owner on the socket, another identity that owns every project the target holds a grant in, or the target itself when it owns every project on the authority; ownership by the identity's own grants (a member Mac's link too); else 403 for the identity itself, the 404 of a missing identity for others |
| `DELETE /v0/aliases/{alias}` | beta | authed | as POST, for the alias's identity; else not_found |
| `GET /v0/person-links` | beta | authed | the links the caller is either account of, or administers every project of (as POST); nothing else |
| `POST /v0/person-links` | beta | authed | either account itself (each records its own consent), or a caller that is not the linked account and administers every project either account holds a grant in; share_mail only for the caller's own account (403 otherwise); a non-admin party names only an account it shares a project with and never moves a link it is no party to; every not-found case is one not_found |
| `DELETE /v0/person-links/{identity}` | beta | authed | either account, or an admin as POST; else not_found |
| `GET /v0/receipts/key` | stable | public | none: the public receipt key |
| `GET /v0/receipts/verify` | stable | authed | none: signature and expectations; `?project=` slug must be visible; a person as `?approver=` also needs the ask readable; admits an answer-only token |

```
POST /v0/approvers        -> {"name": "bob", "for"?: "<identity name>"}   (for: whose device; default this machine's
                              owner. The Decisions page lists that person's decisions, by their own grants; 404 an
                              unknown or non-human identity)
                          <- {"approver": Approver, "code": "K7QM-3XRT", "expires": "...", "pair_path":
                              "/v0/approvals?code=K7QM3XRT", "confirm": true, "note"?: "..."}   (confirm: an existing
                              approver must confirm; note: nobody can, because no approver is active and bootstrap is
                              used up: delete $ZM_HOME/approvals/bootstrapped by hand, rm, add, pair again)
                             a pending name gets a new code only after expiry; every unexpired code is protected
                             regardless of caller attribution (409). 409 also when the name, or a
                             lookalike of it (be.a, iv0, rnallory), is another approver or an identity
GET /v0/approvers         <- {"daemon": "jo-mac", "approvers": [{"name": "jo", "state": "pending|unconfirmed|active", "node": "nXXXX",
                              "node_name": "jo-phone", "login": "...", "added_by": "human:jo", "identity"?: "jo",
                              "confirmed_by": "bootstrap", "created": "...", "paired": "...", "code_expires"?: "...",
                              "note"?: "..."}]}   (note on pending/unconfirmed approvers nobody can confirm; also in
                              the pairing answer and the device view, naming the marker only as $ZM_HOME/...)
                             daemon: the machine they are paired on. An approver decides only the asks its daemon decides:
                             a joined project's asks are decided at its authority
GET /v0/approvers/self?project=<slug> <- as GET /v0/approvers, only the caller's person's approvers, daemon the machine that
                             answered (a joined project's authority); added_by redacted as other holders are. 403 for a
                             session or a non-human caller
zm approver ls --json     <- [Approver + "daemon"]   (this daemon's list, each row with the daemon it is paired on)
zm approver ls -P <project> --json
                          <- {"project": "zm", "decided_at": "dev-05", "joined": true, "only"?: "yours: ...",
                              "approvers": [Approver + "daemon"]}   (decided_at: the machine that decides the project's asks;
                              joined: a team project decided at its authority, whose approvers listed are only the caller's
                              own there (GET /v0/approvers/self); else this daemon and all its approvers. No approver of
                              yours where joined is true: zm approver pair-me -P <project>)
DELETE /v0/approvers/{name} <- Approver
POST /v0/approvals/pair   -> {"code": "K7QM-3XRT"}     (from the device; JSON with Zm-Protocol, see below)
                          <- Approver: active for this daemon's first approver ever ("bootstrap", once:
                             $ZM_HOME/approvals/bootstrapped), else unconfirmed (also after every approver was removed)
                             403 invalid or expired code (same answer), this machine, tagged or zm node; 403
                             after 5 wrong codes per node (20 overall) within the code TTL; 409 node already bound
                             (a re-keyed node: 409 naming its approver; rm and add it, then pair again)
POST /v0/approvers/{name}/confirm -> {}  <- Approver (active, confirmed_by = the caller's approver name)
POST /v0/approvers/self   -> {"name"?: "bob-phone"}  <- as POST /v0/approvers, for the caller's own identity
                             (added_by "self:human:<name>"; default name <name>-phone); a member forwards it to the
                             project's authority (-P or the cwd); 403 for a session or a non-human caller
GET /v0/approvals/device  <- {"node": "nXXXX", "node_name": "jo-phone", "approver"?: Approver,
                              "identity"?: "josmith", "github"?: "<linked login>", "decisions"?: [InboxItem],
                              "skipped"?: [...],
                              "asks"?: [Ask], "confirm"?: [Approver], "for_you"?: [Mail]}   (asks, confirm and
                              for_you: active approvers only; for_you is the device's person's unread for-you mail,
                              on a member Mac the owner's in joined projects too, live from each authority;
                              skipped names one that did not answer)
GET /v0/approvals/push-key <- {"public_key": "<base64url VAPID key>"}
POST /v0/approvals/push   -> PushSubscription JSON {"endpoint", "keys": {"p256dh", "auth"}}  <- {"on": true}
POST /v0/approvals/push/off -> the same  <- {"on": false}
                             SPEC 16 "Phone alerts": the subscription is the device's person's (its --for identity,
                             else the machine owner); every alert re-checks the device is still an active approver
POST /v0/approvals/got-it -> {"id": "<mail id>"}  <- {"id", "acked": true, "acked_by": "human:<the device's person>", "acked_at"}
GET /v0/approvals?code=   <- text/html, CSP default-src 'none' (scripts by nonce or same-origin; POSTs via fetch). The
                             Decisions page: an active approver's device sees every decision GET /v0/decisions gives
                             its person (the approver's identity, by that identity's own grants; else this daemon's
                             machine owner), rendered by app.js, and answers each with POST /v0/asks/{id}/decide.
                             Over plain HTTP, when the node serves HTTPS at its MagicDNS name, 302 to that https URL
                             (an iPhone installs a page only from a secure origin).

POST /v0/asks             -> {"to"?: "owners" | "human:<the asker's machine's human>", "body": "merge?", "area"?: "solver",
                              "subject": {"repo": "example-org/Shop", "pr": 7, "head_sha": "<40 or 64 hex>"},
                              "options"?: ["approve", "decline"]}
                          <- Ask {"id": "<the mail's id>", "project", "area", "subject": {"repo":
                              "github.com/example-org/shop", "pr": 7, "head_sha"}, "options", "body", "asked_by",
                              "to", "created", "receipt_key": "<authority key_id>", "decision"?: Decision}
                             the repo is canonical (host/owner/name, lower case); a short SHA is 400; another
                             human is 400 (the decision answers the mail as that human; from a member machine's
                             node-bound token, its own human, not the authority's); `to` routes the mail only (any
                             active approver decides). With a card, another person (`"to": "human:bob"`) who owns or
                             administers the project is `"owner": "bob"` and the mail goes to human:bob alone
                             after the mail: event dev.zm.ask.created (data Ask; audience as decision.recorded:
                             operate on the area, or the asker or recipient)
                          -> a work item or plain question (SPEC 16 "Note cards"; no subject = plain question):
                             {"subject"?: {"work": "bd:zm-1"}, "area"?, "to"?, "priority"?: 2, "topic"?: "holiday-shifts",
                              "by"?: "2026-11-07T17:00:00Z",
                              "for"?: {"group": "Santa Monica", "contact": "Dr. R", "ref"?, "contact_ref"?},
                              "card": {"title"?, "context", "background"?, "question", "recommend": "a", "why", "searched",
                                       "after_press"?, "choices": [{"key"?: "a", "label", "effect"?, "pros"?, "cons"?}],
                                       "action"?: {"kind": "ack-mail", "ids": ["<mail id>"], "on"?: "a"},
                                       "verify"?: {"command", "on"?},
                                       "owner_step"?: {"label", "steps": ["<1 to 8 plain lines>"], "link": "https://...",
                                                       "minutes"?}}}
                             (owner_step: SPEC 16 "Your step"; needs verify and a person in "to", no action; 400 a link
                             that is not https, carries a user, port or #part, or whose host is not plain ASCII or is an
                             IP address, or a step line with an id, hash or path; stored with "domain", the link's host)
                             (action: SPEC 16 "Asks that carry an action"; stored with full "ids", "count", "samples")
                             other kinds, checked and frozen at creation (400 with a hint for anything zm cannot run):
                             {"kind": "run", "machine", "on"?, "script", "ref", "commit": "<full sha>", "args"?: [...]}
                             (stored with "command") or {"kind": "run", "machine", "agent": "claude|codex", "brief": "<at
                             most 8 KiB>"} (stored with "brief_sha256"); {"kind": "command", "machine", "on"?, "command":
                             "<one shell line, at most 2 KiB, no secret, no sudo>", "ref", "commit": "<full sha>"} (stored
                             verbatim; runs as a script run, SPEC 16); {"kind": "authorize-key", "machine", "user", "key":
                             "<one OpenSSH public key line: ed25519, ecdsa, rsa of 3072 bits or more>"} (stored normalised,
                             with "key_comment", "key_fingerprint": "SHA256:..."); {"kind": "browser", "machine", "plan",
                             "scope": {"origins": ["https://..."], "sign_in"?, "submit"?}} is 503 unavailable until a runtime
                             enforces its scope. The machine: a named trusted machine of the team project with remote_tasks
                             (a script run or command: script_runs too), never auto; the asker operates on the area (403); a run's
                             asker is a session; authorize-key's user is the machine's "os_user" (GET /v0/machines). A
                             card whose choice leaves the reader a step its action does not carry, and that is no owner_step, is 400
                             with the card judge's fixes (SPEC 16 "Card judge")
                          <- Ask, also {"status": "open|answered|recorded|withdrawn", "card", "card_sha256", "priority",
                              "topic"?, "for"?, "by"?, "withdrawn"?, "note"?}; subject {"repo": "", "pr": 0, "head_sha": "",
                              "work"?, "question"?}; body is the card as text, options the choice keys; with an action
                              also "action_text" (exactly what runs, as the approver reads it) and, once answered with its
                              choice, "action_state": {"state": "queued|running|done|failed", "reason"?, "task"?, "started_at"?,
                              "ended_at"?, "confirmed_by"?, "confirmed_at"?} and "action_state_text"
                             400 a card past its word/byte/field limits or with reserved presentation, id, hash or path; 409 the same scoped authority/subject/topic
                             (data: the earlier ask, when the caller may read it); not_found a work item it cannot read
                          -> repeating cards (SPEC 16 "Repeating cards"): "repeat_key"?: "<3-120 of A-Za-z0-9._:->", "latest"?: "<one
                             plain line, at most 300 bytes>" (a note card's own ask; not with same_as: 400). While an open
                             ask of the project in the same area and namespace carries the key: 200 with that ask, "updated":
                             true and a "note"; nothing is filed, judged or mailed; event dev.zm.ask.repeated (data Ask; audience
                             as decision.recorded; none while that ask is held by the card judge). Every Ask read carries
                             "repeat"?: {"key", "seen", "last_seen", "latest"?}
                          -> card router (SPEC 16 "Card judge"): an operator's passed card to owners with a topic and no for, owner, by,
                             apply_default or action, in an area with operators' asks held for outside parties, may come
                             back with "for" set to one of those parties and a "note" saying so
                          -> routing (SPEC 16 "Routing"): "owner"?: "<a person who owns or administers the project>" (default: whom
                             the work item's owner:<name> label names, when such a person; else none; a caller who
                             may not operate on the area: 400 for any owner, and no default); "same_as": "<ask id>" joins that
                             open card ask (its group's first): no card, options, for or pull request (400); its card (less its
                             action and verify, which stay the first's), priority, owner and to; the same project (400); one answered is 409 "already answered" (data: that ask);
                             withdrawn 409; unreadable or missing not_found
                          <- Ask, also "owner"?, "same_as"?, "linked"? (an open ask the card judge found it repeats but
                             could not join; shown to who may operate on the area); a repeat the card judge joins answers
                             as a join, with "note" saying so
                          -> attachments: "attachments": [ids of live attachments on the work item] shows them on the card;
                             400 with no work item or no card, or an id not live on the item (nothing is created)
                          <- Ask, also "attachments"?: [{"id","type","size","sha256","created","expires"}] (only those still live on the item)
                          -> defaults (SPEC 16 "Defaults with deadlines"): "apply_default": true needs a card with "recommend" and
                             "by"; 400 with no card, no recommend, no by, "for", an action, or when the card, the work item's
                             title or body, or its label `irreversible` names an irreversible step (a production write, a publish,
                             an outbound message, money, access, a secret)
                          <- Ask, also "apply_default"?, "default_applied"?, "default_undone"?, "default_undone_by"?
                             at by with no answer: DEFAULT APPLIED comment on the work item, a reply to the ask's mail (the
                             asker wakes), event dev.zm.ask.default_applied (data Ask; audience as decision.recorded); the
                             ask stays open (no decision, no receipt) until answered
GET /v0/asks?project=&open=1 <- {"asks": [Ask], "next"?: "<id>"}: newest first, 200 a page (`limit` up to 1000); `next`
                             is set when the page was full: pass it as `before_id` for the older page (zm measures reads
                             every card this way). Here, `GET /v0/asks/{id}` and the object answer, an Ask carries `labels` (a judge's, e.g. kind, repeat_of) and `objection`
GET /v0/asks?project=&q=<words> <- {"asks": [Ask]}: at most 10 first asks of groups (answered or open, never withdrawn) the
                             caller reads, ranked by shared words of three letters or more; 400 with no words
GET /v0/asks/{id}         <- Ask. A card the card judge could not check is held: it reaches its asker and anyone who may operate on
                             its area (not a party only), with "not_checked": true, "not_checked_reason", "not_checked_since";
                             "released_unchecked": "<approver>" once an approver released it without the check (SPEC 16)
POST /v0/asks/{id}/decide -> {"decision": "release-unchecked"} on a held ask  <- Ask (not a Decision): a paired device
                             releases it to the people it is for, as it is; the same on /answer with the key approver's signed
                             payload (decision "release-unchecked"); 403 anyone else (the socket owner and a session included; the owner's
                             /choose is 409 held); 409 passed and being sent; an ask that was never held is 400 as any unknown choice;
                             again = the same answer (409 once the card was decided). Any other answer to a held ask is 409. A card released this way is never joined
                             (409) and loses its default (apply_default false).
POST /v0/asks/{id}/decide -> {"decision": "approve", "text"?: "only these three", "as"?: "owner|contact", "confirm"?: "<word>", "card_sha256"?: "<as on choose>"}
                             (text alone records decision "other"; never text or as on a pull request's ask; confirm: a risky
                             action's word, below)
                          <- Decision {"ask", "decision", "approver", "time", "receipt": Receipt,
                              "mail_error"?: "...", "text"?, "answered_as"?, "answer"?: "B) ... [only these three]",
                              "not_theirs"?: "<the card's person, when the answer says they did not give it>",
                              "recorded_at"?, "write_error"?, "deliveries"?: [{"kind": "comment|reply|holder|event", "state": "pending|succeeded|failed|unknown"}]}   (mail_error: the decision stands, answering its mail
                              failed; write_error: a record/event effect is pending or unknown; recorded_at means all effects succeeded)
                             the same answer again returns the same receipt; another is 409 (data: the decision)
                             the approver check runs before the ask is looked up (no oracle)
POST /v0/asks/{id}/answer?project=<slug> -> {"approver": "jo-mac", "payload": "<base64 of the JSON below>", "sig": "<base64>"}
                             payload {"v": "zm-answer/1", "daemon": "<Ask.receipt_key, authority key_id>", "project": "<ULID>",
                              "ask", "card_sha256", "decision", "text"?, "as"?, "confirm"?, "time": "<RFC 3339, within 5 minutes>"}
                             risky action (card.action.risk "irreversible" | "spending" | "unchecked", any non-empty value):
                             the answer picking card.action.on must carry "confirm" equal to card.action.confirm_word (the
                             action's machine, else the project's slug; spaces around it ignored), on decide and on this
                             signed answer alike, else 409 {"data": {"reason": "confirm-word"}} (SPEC 16 "Risky actions")
                             sig: ASN.1 ECDSA P-256 SHA-256 over "zm-answer/1\n" + the payload's bytes
                          <- Decision, with "surface": "key", "account", "login" and (a key bound by a sign-in) "machine";
                             403 (one answer) for anything not signed by an active key approver for this ask, this
                             daemon and this moment, a key whose person does not own or administer the ask's project
                             now, a bound key from another node or no longer linked to its account, and a pull
                             request's ask; 403 {"data": {"reason": "member-too-old", "machine"}} for a member's call
                             without origin_checked (SPEC 16 "Key approvers"); 409 another card
DELETE /v0/approvals/key?project=<slug> -> {"key": "p256:<16 hex>", "nonce": "<8-128 of A-Za-z0-9_.:=+/->", "time": "<RFC 3339>",
                              "proof": "<base64 ASN.1 ECDSA SHA-256 by that key over zm-unbind-key/1\n + receipt key id + \n +
                              key + \n + nonce + \n + time>"}
                          <- Approver, now "state": "removed", "ended_why": "signed-out"; 403 for any proof that does not
                             hold, a nonce used before, an unknown or ended key, a bound key from another node
POST /v0/asks/{id}/choose?project=<slug> -> {"decision": "b", "text"?: "only these three", "as"?: "owner|contact", "card_sha256"?: "<Ask.card_sha256 the person read>"}
                             card_sha256: when it is not the ask's current card, 409 {"data": {"reason": "card-changed"}} and
                             nothing is recorded. Its other 409s carry a data.reason too: held (the card judge has not passed
                             it), joined-action-ask, withdrawn, default-stands (sent back while its default stands),
                             confirm-word; an ask answered differently has the standing Decision as data, a write a team replica lost or replayed no data
                             (project: optional; on a member machine's socket it names the joined project whose authority
                             answers. A client's walk-through: docs/clients.md section 5. A call from inside an agent
                             session may be refused, 403, in a later release, with a "Clients" line: SPEC 16 "Key approvers")
                          <- Decision, with "method": "owner", "surface": "owner", "machine": "<the owner's machine>"; receipt approver is the
                             owner's identity, node "owner:<machine>" (machine "token:<id>" for a token, "tailnet" for a
                             tailnet login). With an answer-only token: "surface": "answer-token", "machine": "answer-token:<token id>",
                             node "owner:answer-token:<token id>", method still "owner"; any choice that carries a confirm word is
                             403 signed-only for it. 404 an ask the caller cannot read, as a missing one; 409 one the card judge holds; 403 a caller who reads the
                             ask but does not own the project (a member, a member-capped token) and a pull request's ask; the same answer
                             again returns the same receipt; another is 409; 403 "answer on a paired device or in the Mezze app: this
                             choice runs something" (data {"reason": "signed-only"}) for a run, command, browser, authorize-key or hand action's choice. No confirmation step. On a member machine it
                             goes to the ask's authority once (never hedged). A paired approver's Decision has
                             "method": "approver", "device": "<its device>", "surface": "device"; every Decision carries
                             "account" and "login": the answerer's verified link at answer time (a bound key's own).
POST /v0/asks/{id}/undo?project=<slug> -> {}  <- Ask, with "default_undone", "default_undone_by"; DEFAULT UNDONE comment on
                             the work item, mail to the asker, event dev.zm.ask.default_undone. 409 no applied default or
                             already answered; again = the same answer; 403 as choose
POST /v0/asks/{id}/device-undo -> {}  <- Ask, as undo, from a paired approver's device (undone by the approver's name)
POST /v0/asks/{id}/object -> {"note"?: "why, one line"}  <- Ask, with "objection": {"at", "by", "note"?}; 409 before the answer
POST /v0/asks/{id}/withdraw -> {}  <- Ask (status withdrawn; 409 once decided, or while an applied default stands; again = the same answer)
POST /v0/asks/{id}/action/confirm -> {}  <- Ask, with action_state "confirmed_by", "confirmed_at": the ask leaves the open
                             ones in GET /v0/decisions. 409 no action that ran, or one still queued, running or approved (not done); again = the
                             same answer; 403 as choose
POST /v0/asks/{id}/action/retry -> {"card_sha256"}  <- Ask (SPEC 16 "Retry"), action_state back to "queued" with
                             "retries" counting; the identical action starts again. 409 a changed card_sha256, no run or
                             command action, not failed, confirmed, not retryable (it may have started, or would fail the
                             same way), past "retry_until", or 3 retries; 403 neither its asker nor an owner
POST /v0/asks/{id}/step -> {"card_sha256"}  <- Ask (SPEC 16 "Your step"), action_state "step_done_at" and "step_done_by":
                             the person says they did the card's owner_step; zm mails the asker to run the check now.
                             your_step stays false until a check after it fails. 409 no owner_step, not approved and not
                             done, or a changed card_sha256; 403 not the person it is for, or an agent session. A
                             second press within a minute changes nothing and mails nobody
POST /v0/asks/{id}/check -> {"card_sha256", "exit"|"give_up"|"evidence", "output"?}  <- Ask (SPEC 16 "Approved, not done"): the
                             asker's run of the card's verify command. exit 0 while action_state is "approved", or
                             "failed" by its machine (work done another way), makes it
                             "done" with "verified_at" and comments DONE on the item; any other exit records
                             "checked_at", "check_exit", "check_output" (last 2 KiB), "checked_by" and changes nothing.
                             give_up (one plain line, at most 500 bytes; no exit) fails it, also an action still queued
                             or running. 400 no exit, or a blank give_up; 409 no verify on the card (give_up: nor an
                             action), not approved yet, an action still queued or running (exit), already done, failed
                             before it started or given up, or card_sha256 not the card's; 403 neither asker nor operate.
                             An Ask's "next" is whose turn it is: "you" (the person it is for), "agent" or "nobody",
                             with "ran_unchecked" (its action ran, nothing checked it, no owner confirmed or objected)
                             and "your_step" (approved work waits on the person's own step) (SPEC 16 "Whose turn").
                             An Ask's "not_done" is true while its action_state is queued, running or approved;
                             action_state also carries "reminded_at" (the asker was told) and "escalated_at" (the approver),
                             "relaned_at" with "relane_task" and "relane_machine" (a fresh lane) or "relane_reason" (none
                             could start), and "done_evidence". evidence (one plain line, at most 500 bytes; no exit, no
                             give_up), where an exit 0 would be taken, makes it "done" without "verified_at": its asker,
                             or a caller naming no session who owns or administers the project, only (403 else).
POST /v0/steps -> {"to": "human:<name>", "work", "title", "brief", "check"} | {"draft", "title", "brief", "check"} |
                             {"from_ask", "title"?}  <- PersonStep {"id", "project", "area"?, "person", "added_by", "work",
                             "title", "brief", "check", "risk", "state", "bundle"?, "number"?, "ask"?, "created", "updated",
                             "report_note"?, "reported_by"?, "reported_at"?, "checked_at"?, "check_exit"?, "check_output"?,
                             "checked_by"?} (SPEC 16 "Person steps"): state "pending". title one plain line of at most 80
                             bytes, brief at most 900, check one printable line of at most 200 (else 400, before any
                             judge); the step judge's three votes decide (a fail is 400 with its fixes numbered; a real
                             choice's first fix says to file its own ask); no answer is 503 and nothing is stored.
                             from_ask takes the hand ask's brief, verify line, person and work item (title: its card's
                             unless given; brief or check given is 400) and withdraws the ask; 409 if this call did not
                             withdraw it (answered or withdrawn meanwhile), and nothing is added
GET /v0/steps?project=&to=human:<name>&state=all|<state>  <- {"steps": [PersonStep]}: oldest first, at most 500;
                             default every state but done and dropped
GET /v0/steps/{id}          <- PersonStep
POST /v0/steps/{id}/drop    <- PersonStep, state "dropped"
POST /v0/steps/{id}/check -> {"exit", "output"?}  <- PersonStep: its adder's run of the check line. exit 0 makes it
                             "done" (from pending, carded, reported or failed) and comments STEP DONE on its work item;
                             another exit after a done report makes it "failed"; else it records the check only. 400 no exit
GET /v0/bundles/{id}        <- {"id", "ask"?, "steps": [PersonStep], "done"}: one steps card's steps the caller may read,
                             in card order, by its bundle id or its ask; done when every step of the card, read or not,
                             is done or dropped
POST /v0/bundles/{id}/steps/{n}/report -> {"result": "done"|"failed", "note"}  <- PersonStep: step n of the card, by
                             the person's agent. done makes it "reported", failed "failed" (note: one plain line, at most
                             500 bytes, else 400); either is commented on its work item and its adder is mailed (done:
                             run zm ask bundle step-check <step>). Only the check makes it done
GET /v0/run-targets?project=<slug>&person=<name>  <- {"project", "person"?, "targets": [{"machine", "people": [identity],
                             "trusted", "remote_tasks", "script_runs", "command_runs", "os_user"?, "seen"?, "ready", "why"?}], "hint"?}: the
                             machines a run action's --run-on may name (ready), whose they are; at the authority
GET /v0/rulings             <- {"project", "rulings": [{"name", "text", "ask"?}]}: the owner rulings the card judge
                             quotes, in file order (zm project ruling ls)
POST /v0/rulings            -> {"name", "text", "ask"}  <- the list after: adds a [[rulings]] table at the authority, recording
                             the ask; the judge reads it from the next card, no restart (zm project ruling add)
DELETE /v0/rulings/{name}   <- the list after (zm project ruling rm)
POST /v0/approvals/pair-key?project=<slug> -> {"code", "public_key": "<base64 of uncompressed P-256>", "device"?: "Mezze app on the Mac",
                              "proof": "<base64 ASN.1 ECDSA SHA-256 over zm-pair-key/1\n + code (upper case, no dashes) + \n + public_key>"}
                          <- Approver (always unconfirmed; "key": "p256:<16 hex>" to compare before confirming)
GET /v0/approvals/key?project=<slug>&key=p256:<16 hex>
                          <- Approver paired to that key (its state turns active once a phone confirms it, or at once
                             when a sign-in bound it); 404 for any other key; 400 for a malformed one. A key bound by a
                             sign-in carries "via": "login", "account": "github:<id>", "login", "machine", "bound"; an
                             ended one "state": "removed", "ended", "ended_why" (replaced, legacy, signed-out) and, when
                             replaced, "replaced_by": "p256:<16 hex>". GET /v0/approvers/self rows carry the same. GET /v0/approvers is local to the authority, so a
                             key paired from a member machine reads its own state here (the member forwards it).
GET /v0/decisions?project=&status=open,answered&work=0&label=&for=me|everyone|agents <- {"decisions": [{"kind": "ask|work", "project_slug", "priority",
                              "ask"?: Ask, "work"?: WorkItem, "owner_name"?: "bob"}], "skipped"?, "behind"?}
                             a held card (the judge could not check it) is listed at once for who may operate on it, with
                             "not_checked": true, after the open ones, deadlines first (SPEC 16 "Card judge")
                             one daemon's rows are ordered (SPEC 16 "Inbox"); a member's merged list is one run per
                             authority, so its reader sorts by the same fields. status open also lists an answered ask
                             whose action nobody confirmed (POST /v0/asks/{id}/action/confirm): a failed one first of
                             all (and approved work still not done 24 hours after the yes), other not-done work (queued,
                             running, approved) on its own after the open asks, a done one there for 24 hours.
                             A member's merged list is one run per
                             authority, so its reader sorts by the same fields. Bounded results report "more" in skipped;
                             visibility and ranking precede the cap. No continuation token: narrow project/status.
                             An Ask always carries authority receipt_key (create/get/list/inbox/device).
                             Work verification requires ask or card; question is an ask-bound hash, not card_sha256.
                             Verification through a member follows the receipt project; signatures are checked there.
                             Work items count when they wait on the reader: owner:<name>, ask:<name> or needs-<name>
                             for the reader's identity name or an alias (GET /v0/aliases), ask:owner for an owner of
                             the item's project (its own grants, not a replica token's cap), or, when the item names
                             no one and is not labelled ruled, the generic labels (human, owner-decision, decision,
                             questions, owner-run, needs-night-ruling) or type decision. needs-<x> names x only when
                             x is an identity's name or alias at this authority; needs-spec and the like are tags. ?for=everyone lists every person's waits; ?label= lists whatever
                             carries those labels. me and everyone list only steps: an item that is a question
                             (owner-decision, questions, decision, needs-night-ruling, ask:<who>, type decision) and
                             not labelled human or owner-run lists under ?for=agents (no asks) until its ask exists.
POST /v0/aliases          -> {"alias": "jo", "identity"?: "josmith"}
                          <- {"identity", "aliases": ["jo"], "added_by": {"jo": "human:bob"}}
                             canonical names only (approver name rules), never a role word (owner, chief, chiefs, me,
                             everyone), never a lookalike of another identity's name or alias (409); a new identity
                             may not take an alias's name either (409). Set by the authority's owner or an admin of
                             every project the identity is in, never by the identity itself (403); added_by names the
                             setter. Forwarded to the project's authority like other project writes (-P or the cwd).
GET /v0/aliases?identity= <- {"identity", "aliases", "added_by"}     DELETE /v0/aliases/{alias} <- the same
POST /v0/person-links     -> {"identity": "jo-box", "person": "jo", "share_mail"?: true}
                          <- {"links": [{"identity": "jo-box", "person": "jo", "live": true, "by_admin"?: "human:bob",
                              "by_self"?, "by_person"?, "shares_mail"?, "person_shares_mail"?}]}
                             One person, several accounts: identity belongs to the person whose main account
                             is person. Live when person posted it, and so did identity or a caller who administers every
                             project either account is in (never identity itself); one level among live links only. Live links fold the accounts wherever
                             people fold (GET /v0/people, owner_person, a decision's approver_person); a verified GitHub link
                             wins (409 when identity has one that differs from person's). Grants nothing. One level: person may
                             not itself be linked, nor identity be another link's person (409). share_mail is the caller's
                             own account only, on this link only: private (for-you) mail to it also goes to the link's other account
                             (dropped when the link moves or goes). A non-admin party names only an account it shares
                             a project with (else 404).
GET /v0/person-links      <- the same                     DELETE /v0/person-links/{identity} <- the same

Receipt = {"v": "zm-receipt/1", "project": "<ULID>", "ask": "<id>", "subject": {...}, "decision": "approve",
           "approver": "jo", "node": "nXXXX", "time": "2026-10-30T18:00:00.000Z", "key_id": "zm:<16 hex>",
           "sig": "<base64 ed25519>"}
  sig signs "zm-receipt/1\n" + the compact JSON of {v, project, ask, subject {repo, pr, head_sha}, decision,
  approver, node, time} in that order (internal/approvals.SigningBytes).
Receipt v2 = the same with "v": "zm-receipt/2", "subject": {"repo": "", "pr": 0, "head_sha": "", "work" | "question"},
  "card_sha256", "text_sha256"?, "answered_as", "action"? (signed after time, in that order, under "zm-receipt/2\n";
  an empty optional field is left out); node may be "key:p256:<16 hex>", or "owner:<machine>" when the project's
  owner decided ("owner:token:<id>" for a token, "owner:tailnet" for a tailnet login). A receipt's fields never grow
  within its "v": ParseReceipt refuses unknown fields. How a client checks one: docs/clients.md section 5.
GET /v0/receipts/key?project= <- {"alg": "ed25519", "key_id": "zm:...", "public_key": "<base64>", "line": "ed25519:<base64>"}
GET /v0/receipts/verify?receipt=<base64url JSON>&project=&ask=&repo=&pr=&head_sha=&work=&question=&card=&decision=&approver=&signed=
                          <- {"valid": false, "reason": "receipt is for another head commit", "key_id": "...",
                              "checked"?: ["project", "repo", ...]}     (400 without ?receipt=)
                          approver= is the receipt's exact approver, or a person: then it also matches when the receipt is
                          the ask's recorded decision, answered on a device paired for that person (--for) or a linked account, and the
                          caller may read the ask. signed=1 also requires a signed answer: a node starting "owner:" (the socket,
                          an owner token, an answer-only token) is invalid, and "signed" is in checked; absent (or 0) changes
                          nothing; any other value is 400
```

Device routes check, after the router's guard: the `Host` names this daemon (`potluck-<machine>[.<tailnet>]` or
`zm-<machine>[.<tailnet>]`, the
configured node name or a Tailscale IP; DNS rebinding), and every POST is `application/json` with
`Zm-Protocol` and no cross-site `Sec-Fetch-Site` (a cross-site form cannot send them without a CORS
preflight, which the daemon never grants). After a decision commits: `dev.zm.decision.recorded` (project
event, audience operate + parties), the ask's mail is answered as the machine's human and taken (the asking
session leaves needs_human), and, when the project has a bot token (`$ZM_HOME/approvals/github/<project
ULID>.token`, mode 0600), a commit status `zm/approval` and a PR comment with the receipt are posted.

## Webhooks and GitHub linking

Outgoing webhooks and GitHub branch/PR linking (SPEC "Webhooks and GitHub linking").

| route | stability | guard | check |
| --- | --- | --- | --- |
| `GET /v0/webhooks` | beta | authed | admin on the hook's project/area (hooks the machine owner made on its socket: that owner only); ?project= like ReadScope |
| `POST /v0/webhooks` | beta | authed | admin on the named project/area; no project and `allow_private`: machine owner on the socket only |
| `GET /v0/webhooks/{id}` | beta | authed | admin on the hook, else not_found (identical to a missing id) |
| `PATCH /v0/webhooks/{id}` | beta | authed | admin on the hook, else not_found |
| `DELETE /v0/webhooks/{id}` | beta | authed | admin on the hook, else not_found |
| `POST /v0/webhooks/{id}/test` | beta | authed | admin on the hook, else not_found; sends one signed ping now |
| `GET /v0/github/repo-list` | beta | authed | read on the whole project named by `?project=`; hidden, area-only and missing answer `not_found` alike; on a member, forwarded to the authority |
| `PUT /v0/github/repo-list` | beta | authed | admin on the whole project named by `?project=`; hidden and missing answer `not_found`; on a member, forwarded to the authority |
| `GET /v0/github/links` | beta | local | every project's GitHub link |
| `GET /v0/github/repos` | beta | authed | explicit project/area repository mappings in ReadScope; hidden mappings omitted; on the socket also joined team projects' (from the authority's last header; GitHub is read with this machine's login) |
| `GET /v0/github/data` | beta | authed | named project and mapped repository; fixed bounded resources, cached once by zm; hidden and missing match before any upstream read |
| `POST /v0/github/links` | beta | local | create or replace a project's link (runs gh, mints the linker's token) |
| `DELETE /v0/github/links/{project}` | beta | local | revokes the linker's token and grant |
| `POST /v0/github/links/{project}/sync` | beta | local | poll now and apply the inbox |
| `POST /v0/github/hook` | internal | public | none: `X-Hub-Signature-256` must verify with a link's secret, else 401 whatever repo the body names; answers nothing scoped |

`GET /v0/github/repos?project=&area=` returns `{repositories:[{project,slug,area?,repo}]}`
from the project's declared GitHub remotes and authorized area remotes. The data door requires
`project`, `repo` and `resource`: `pulls` (open PRs, fixed GraphQL query, optional `after` cursor),
`pull` (`pull` required: one PR in any state, fixed GraphQL query: state, draft, head and base,
mergeability, labels, the first 100 files, the last 20 comments, the head's first 100 checks and
statuses, the merge commit's Actions runs, the author; `partial` when files, checks or merge-commit runs were cut;
a PR GitHub does not have answers `not_found`), `files` (`pull` required,
optional `after`: the next 100 files), `runs` (optional `workflow` file name such as `ci.yml`,
`branch`, `event` and `status` filters, each matched whole; status is queued, in_progress, waiting, pending, requested, action_required or completed), `jobs` (`run` required), `runners`, `checks`
or `status` (`pull` and full `head_sha` required). REST `page` is 1–10. Jobs must name a run
already listed for that mapped repository (a runs page or a cached merged `pull`'s merge-commit
runs); checks must name the current head in a fresh cached `pulls` page or `pull` read. A pull's checks are
dropped when its last commit is not its head. Caller URLs, queries and hosts are refused. There
is no org-wide runner read and no run log through this door.

The answer is `{project,repo,resource,head_sha?,payload?,fetched_at?,stale,partial,error?}`.
Payload is the fixed GitHub response consumed by existing pure client parsers, not an arbitrary
proxy response. A failed refresh retains its prior payload with `stale:true` and a structured
`{code,message,retry_after}` error; it never labels an old response fresh or sends raw CLI errors.
PR events invalidate cached and in-flight reads. Cache limits: 128 entries, 32 MiB payload total,
2 MiB per answer, four simultaneous reads, 15-second fetch deadline. Default freshness is one
minute (jobs 20 seconds, repository runners five minutes); failures back off one minute.
Credentials stay with the daemon's existing GitHub CLI login or configured link token file.
Reads do not create a link, mint a token, or alter a work item.

```
POST /v0/webhooks        -> {"url": "https://hooks.example.com/zm", "project"?: "shop/solver",
                             "events"?: ["work.*", "mail.sent"], "secret"?: "...", "allow_private"?: false, "note"?: "..."}
                         <- {"webhook": Webhook, "secret"?: "whsec_..."}   (secret only when the daemon made it)
                            400 non-https or non-public URL without allow_private (loopback, RFC 1918, link-local,
                            CGNAT/tailnet, metadata, reserved; names like localhost, *.local, *.internal, *.ts.net)
Webhook = {"id", "url", "project"?, "area"?, "events"?, "allow_private"?, "created_by", "note"?, "state": "active|disabled",
           "disabled_reason"?, "created", "last_ok"?, "failing_since"?, "last_error"?, "pending"}
GET /v0/webhooks?project= <- {"webhooks": [Webhook]}   (?project=slug/area narrows to that area's hooks)
GET /v0/webhooks/{id}?limit=50 <- {"webhook": Webhook, "deliveries": [{"delivery", "event", "type", "attempt", "time",
                             "status", "outcome": "delivered|retry|failed|dropped|skipped|ping", "error"?, "duration_ms"}]}
PATCH /v0/webhooks/{id}  -> {"enabled": true}   <- Webhook   (enabling skips events published while it was off)
DELETE /v0/webhooks/{id} <- Webhook
POST /v0/webhooks/{id}/test <- {"ok": true, "delivery": {...}}

A delivery: POST <url>, body = the event's CloudEvents JSON (as the hook's creator may see it, mentions redacted),
headers Content-Type: application/json, User-Agent: zm-webhooks/<version>, the Standard Webhooks trio webhook-id (the
delivery id, stable across retries), webhook-timestamp (Unix s), webhook-signature = "v1," + base64
HMAC-SHA256(key, id + "." + timestamp + "." + body) (key, by the secret's form: the decoded bytes when it is "whsec_" + valid
standard base64, as every daemon-made secret is from 2026-11-01; otherwise the secret's bytes, including older
daemon-made URL-safe secrets), X-Hub-Signature-256 = "sha256=" + hex HMAC-SHA256(secret, body) (key: the whole secret string), and Zm-Webhook-Id (the hook),
Zm-Webhook-Delivery, Zm-Webhook-Event (the type), Zm-Webhook-Timestamp. Deprecated, removed 2027-05-01:
Zm-Webhook-Signature = "sha256=" + hex HMAC-SHA256(secret, timestamp + "." + body).
2xx = delivered; anything else (redirects are not followed) is retried; 410 disables the hook.

GET /v0/github/repo-list?project=shop   (zm github ls -P shop --json prints this answer as is)
                         <- GitHubRepoList
PUT /v0/github/repo-list?project=shop -> {"repos": ["example-org/Shop", "https://github.com/sengirunners/potluck"]}
                         <- GitHubRepoList   ("repos": null or absent clears the list: back to the remotes;
                            400 a bad owner/name, over 200 repos, or an area in ?project=)
GitHubRepoList = {"project": "shop" (slug), "id": "<project ULID>", "repos": ["example-org/shop", ...] (owner/name,
                  lower case, deduplicated, never null), "source": "list|remotes", "updated"?: "<RFC 3339>"}
   source "remotes": never set, so the project's then its areas' declared github.com remotes (no "updated");
   "list": set by an admin (SPEC 18.5). Compare repos case-insensitively (GitHub's full_name keeps case).

POST /v0/github/links    -> {"project": "shop", "repos": ["example-org/Shop"], "prefixes"?: ["shop"], "sources"?: ["bd"],
                             "on_open"?: "claimed|in_review|none", "on_merge"?: "done|in_review|none", "on_close"?: "open|none",
                             "poll_seconds"?: 60, "bin"?: "gh", "host"?: "...", "token_file"?: "...", "secret"?: "...",
                             "match"?: ["title", "branch", "body"], "gate"?: "/abs/path/to/gate"}   (gate: SPEC 18.3; it must
                             answer describe or the link is 400)
                         <- {"link": GitHubLink, "secret"?: "ghsec_..."}   (secret only when the daemon made it)
GitHubLink = {"project", "repos", "prefixes", "sources", "on_open", "on_merge", "on_close", "poll_seconds", "bin"?, "host"?,
              "token_file"?, "identity": "human:zm-github", "created", "last_poll"?, "last_event"?, "last_error"?,
              "pending", "receiver": "/v0/github/hook", "match": [...], "gate"?, "gate_name"?, "gate_error"?, "waiting"}
GET /v0/github/links     <- {"links": [GitHubLink]}
DELETE /v0/github/links/{project} <- GitHubLink
POST /v0/github/links/{project}/sync <- {"prs": 3, "applied": 5, "errors"?: ["..."]}
POST /v0/github/hook     <- 202 {"ok": true} (pull_request), 200 (ping), 202 {"ignored": true} (other events),
                            401 when the signature matches no link's secret (the same answer whatever repo the body names;
                            a verified delivery for a repo its link does not cover is accepted and ignored), 400 over 5 MiB
```

## Connections (SPEC 24)

Outside systems shown as one typed summary each. zm fetches the provider's summary when someone looks, checks it
against `zm.connection.show/1`, keeps the last good one, and says when it is stale or unusable.

| route | stability | guard | check |
| --- | --- | --- | --- |
| `GET /v0/connections` | beta | authed | read on the connection's project/area; ?project= like ReadScope |
| `POST /v0/connections` | beta | authed | admin on the named project/area; `token_ref`, `allow_private` and `unix:` endpoints: machine owner on the socket only |
| `DELETE /v0/connections/{id}` | beta | authed | admin on the connection, else not_found (read without admin: forbidden) |
| `GET /v0/connections/{id}/show` | beta | authed | read on the connection, else not_found; fetches from the provider at most once per refresh |

```
POST /v0/connections -> {"project": "shop[/area]", "name": "Example CI", "endpoint": "https://ci.example.com/zm/show"
                          | "unix:/abs/path.sock[?path=/show]", "token"?: "...", "token_ref"?: "keychain:ci/zm",
                          "allow_private"?: false, "note"?: "..."}
                     <- Connection   (400 a bad or non-public endpoint, as for webhooks; 409 a name the project has)
Connection = {"id", "name", "project", "area"?, "endpoint"? (admins only), "allow_private"?, "note"?, "created_by"? (admins
              only), "created"}
GET /v0/connections?project= <- {"connections": [Connection]}
DELETE /v0/connections/{id} <- Connection
GET /v0/connections/{id}/show?account= <- {"connection": Connection, "state": "fresh|stale|unavailable", "reason"?,
                          "checked_at"?, "fetched_at"?, "summary"?: Summary}   (no summary when unavailable; ?account= keeps
                          that account's rows, meters and items)

The provider's side: GET <endpoint>, headers Accept: application/json, User-Agent: zm-connections/1, Authorization:
Bearer <token> (when one is set). 2xx with a Summary body (256 KiB max), anything else is a failed fetch. No redirects.
Summary = {"schema": "zm.connection.show/1", "title"?, "as_of": RFC 3339, "stale_after_s"?: 300 (30..86400),
  "headline": [{"key", "label", "state": "ok|warn|red", "count"?, "since"?, "detail"?, "account"}],      (32 max)
  "meters":   [{"key", "label", "used", "reserved"?, "total" (> 0), "unit", "account"}],               (32 max)
  "running":  [{"id", "title", "owner"?, "started"?, "links"?: [{"label", "url" (http/https)}], "account"}]}  (200 max)
```

## Native issues (issues builder; SPEC "Native issues")

Issue ids are one path segment (`shop-ofoqu`, `shop-ofoqu.2`); the project is the one whose prefix the id
carries. Reads need `read` on the issue's area; writes need `write` there **and a role above guest** (guests are
read-only on issues unless the project sets `guest_issues`: then a guest writes issues where it may write; the
`(not guest)` below reads that way, memories excepted), plus `write` on the area a write moves the issue to (a label). A hidden issue answers like a
missing one; edges to issues the caller cannot read are left out of every answer and every count. Writes answer the
issue as the caller sees it afterwards, with `"noop": true` when nothing changed (an idempotent repeat).

| route | stability | guard | check |
| --- | --- | --- | --- |
| `GET /v0/issues` | beta | authed | read: projects in ReadScope(?project=); issues whose area the caller reads |
| `POST /v0/issues` | beta | authed | write (not guest) on the resolved project/area (WriteTarget) and the issue's area |
| `GET /v0/issues/stats` | beta | authed | read: counts over issues in ReadScope the caller reads |
| `GET /v0/issues/labels` | beta | authed | read: labels on issues in ReadScope(?project=) the caller reads |
| `GET /v0/issues/comments` | beta | authed | read: comments on issues in ReadScope(?project=) the caller reads, oldest id first; `author` (address), `prefix`, `contains`, `after` (a comment id), `limit` (default 100, at most 1,000); answers `{comments, next}` |
| `GET /v0/issues/projects` | beta | authed | read: projects in ReadScope with native issues |
| `GET /v0/issues/memories` | beta | authed | read on every area of the one project in ReadScope(?project=) with native issues: `{memories: {key: text}}` (bd memories, beads' `kv.memory.*` rows) |
| `PUT /v0/issues/memories/{key}` | beta | authed | write (not guest) on the whole resolved project (WriteTarget): `{value}`; answers `{key, value, action: remembered\|updated}` |
| `DELETE /v0/issues/memories/{key}` | beta | authed | write (not guest) on the whole resolved project (WriteTarget); not_found when there is none; answers `{key, value, action: deleted}` |
| `POST /v0/issues/sql` | beta | authed | read on every area of the one project in ReadScope(?project=) with native issues: `{query}`, one read-only SELECT over bd-shaped views; answers `{columns, rows}` (at most 50,000 rows, 30 s) |
| `POST /v0/issues/setup` | beta | authed | admin on the resolved project (WriteTarget); prefix only while it has no issues |
| `POST /v0/issues/move` | beta | authed | admin on the resolved project (WriteTarget) and on project `to`; `{to, ids, children?, transfer_claims?, dry_run?}` moves open issues there (new id: `to`'s prefix + the old id without its prefix; the old id redirects); claimed issues only with `transfer_claims`; answers `{from, to, items: [{id, new_id, result, note, claim, cross}], moved, refused}` |
| `POST /v0/issues/restore` | beta | local | admin on WriteTarget; replaces the project's native store with a sqlite backup's copy, redaction list applied first; 409 for a newer schema, another project's store, while a restore or backup of the tracker runs, or while its writes are held |
| `GET /v0/issues/{id}` | beta | authed | read on the issue's area, else not_found; edges to unreadable issues left out |
| `PATCH /v0/issues/{id}` | beta | authed | write (not guest) on the issue's area, and on the area a label change moves it to |
| `POST /v0/issues/{id}/claim` | beta | authed | write (not guest) on the issue's area; conflict when someone else holds it; `{ttl_seconds?, assignee?, fence?}` (assignee: a routing label, not an identity); a lease-backed claim answers `lease_fence`, and `fence` on this, PATCH, close, reopen and comments is SPEC 10 "Fencing" |
| `POST /v0/issues/{id}/close` | beta | authed | write (not guest) on the issue's area |
| `POST /v0/issues/{id}/reopen` | beta | authed | write (not guest) on the issue's area |
| `GET /v0/issues/{id}/comments` | beta | authed | read on the issue's area |
| `POST /v0/issues/{id}/comments` | beta | authed | write (not guest) on the issue's area; author is the caller |
| `PATCH /v0/issues/{id}/comments/{cid}` | beta | authed | the comment's author (any session of its identity), write on the issue's area; `{text, base?}`; `base` refuses an edit over an unseen change (409) |
| `POST /v0/issues/{id}/comments/{cid}/redact` | beta | authed | an admin of the issue's area, or the machine owner on the socket: the text, its edits' texts and every other copy are overwritten with `[redacted]` (cannot be undone) |
| `POST /v0/issues/{id}/comments/{cid}/ack-gap` | beta | authed | whoever may redact the comment (an admin of the issue's area, or the machine owner on the socket): `{stop}`, one gap as the caller's own redaction conflict names it (`project:issue`, or `old>new`; a trailing ` (why)` is ignored); a stop in a project the caller cannot read, or no such gap, is not_found; a stop in another project the caller reads but does not moderate is forbidden; marks only that stop clean and logs who, when and the gap in `issue_redaction_acks`; searches no text, deletes no backup; answers `{comment_id, stop, why, by, at, remaining}` (`by` redacted; `remaining` named as the conflict names them) |
| `DELETE /v0/issues/{id}/comments/{cid}` | beta | authed | the comment's author, an admin of the issue's area, or the machine owner on the socket: a tombstone; text and deleter shown to moderators only |
| `GET /v0/issues/{id}/history` | beta | authed | read on the issue's area; actor addresses redacted |
| `POST /v0/issues/{id}/deps` | beta | authed | write (not guest) on the issue's area; a target the caller cannot read is not_found |
| `DELETE /v0/issues/{id}/deps/{target}` | beta | authed | write (not guest) on the issue's area; a target the caller cannot read is not_found |
| `POST /v0/issues/{id}/labels` | beta | authed | write (not guest) on the issue's area and on the area the label moves it to |
| `DELETE /v0/issues/{id}/labels/{label}` | beta | authed | write (not guest) on the issue's area and on the area the removal moves it to |
| `GET /v0/issues/{id}/attachments` | beta | authed | read on the issue's area: `{attachments: [Attachment]}`, the live ones, oldest first |
| `GET /v0/issues/{id}/attachments/{aid}` | beta | authed | read on the issue's area: the image's bytes, `Content-Type` its type, `X-Content-Type-Options: nosniff`, `Content-Disposition: inline`, `Content-Security-Policy: default-src 'none'; sandbox` (an HTML page: `default-src 'none'; style-src 'unsafe-inline'; img-src data:; font-src data:; sandbox`); an expired one is not_found |
| `POST /v0/issues/{id}/attachments` | beta | authed | whoever may comment (write, not guest, on the issue's area): the body is the image's bytes (any `Content-Type`; at most 8 MiB, 1 MiB through a team replica); `?expires=` `14d` (default), `36h`, at most `90d`; answers the Attachment |
| `DELETE /v0/issues/{id}/attachments/{aid}` | beta | authed | its attacher (any session of their identity) or an admin of the issue's area, with write there: a hard delete; answers the Attachment |

- `GET /v0/issues` query: `status` (comma list, or `all`; default: not closed or pinned), `priority` (0-4 or P0-P4),
  `type`, `assignee`, `no_assignee`, `label` (repeat: all), `label_any` (repeat: any), `parent`, `id` (repeat or
  comma list), `title_contains`, `desc_contains`, `notes_contains`, `q` (full text: words over title, description,
  notes; the last word is a prefix; title matches first), `substr` (bd search: substring of title, id or external
  ref), `ready` (open, unblocked, not deferred), `blocked`, `sort` (`priority` default, `created`, `updated`,
  `closed`, `id`, `title`, `status`, `type`, `assignee`, `oldest`; `rank` with `q`: bm25 relevance, best first,
  each issue's `score` set; a query too common to rank in budget answers `unranked: true`, newest first, no score,
  `total` an upper bound, 0 when unknown), `reverse`, `limit` (default 50), `all` (every
  status, no limit), `updated_after` (RFC 3339), `deps` (each issue's own edges), `count` (exact `total` when the page
  is cut), `number` (the issue with that per-project number), `area` (repeat; `area=` = project-level),
  `contains` (substring of title or description), `claimed_by`, `created_after`/`created_before`/`updated_before`,
  `id_after` (with `sort=id`: paging by id),
  and keyset paging: with `sort=created|updated` a cut page has `next`; pass it back as `after`. Answer
  `{"issues": [Issue], "more"?, "total", "next"?, "unranked"?}`. Every issue has a per-project sequence `number` (creation order,
  never reused; imported issues numbered by creation time); its id stays the identifier.
- Bodies: `POST /v0/issues` `NewIssueRequest` (`title` required; `parent` makes the id `<parent>.N`; `deps` are `id`
  or `type:id`, `blocks:X` = the new issue blocks X); `PATCH` `UpdateIssueRequest` (nil fields unchanged; `base` = a
  `version` you had: a changed field refuses with `409`); `close {reason?, session?, force?}`; `reopen {reason?}`
  (the reason is also a comment); `comments {text, id?}` (a client ULID makes a replay a no-op); `deps {target,
  type?}` (default `blocks`; `external:...` targets kept as text); `labels {label}`; `setup {prefix}`.
- Attachments: PNG, JPEG or WebP, told by the bytes (SVG and anything else `400`), decoded and encoded again
  without metadata (a WebP comes back a JPEG, or a PNG when it has transparency), at most 20 megapixels and 1 MiB
  after re-encoding (`413`), or an HTML page (UTF-8, opening like a page; kept verbatim, at most 1 MiB, served as
  `text/html; charset=utf-8` under `sandbox`: inline CSS and data: images render, scripts and network do not), at most 8 live per issue (`409`). Attachment = `{"id": ULID, "type":
  "image/png|image/jpeg|image/webp|text/html", "size": bytes, "sha256": hex, "created": RFC 3339, "created_by": address
  (redacted), "expires": RFC 3339}`. `GET /v0/issues/{id}` (and the native work item's `extra.attachments`, with
  `created_by` empty, and `bd show --json`) carries `attachments` when there are any; lists do not. Bytes never
  appear in an issue, an event, a webhook or mail; history rows `attached`, `attachment_removed` and
  `attachment_expired` carry the metadata (`fields.attachment`).
- Events: `dev.zm.issue.created` / `.updated` / `.commented` (data `{issue, change}`; no edges, no parent, and no
  issue ids in the change: readers fetch the issue), `.moved` (`{id, gone: true}` to readers of the area an issue
  left), `.imported` (`{imported: [ids]}` per batch and area).

## Search (SPEC 21)

| route | stability | guard | check |
| --- | --- | --- | --- |
| `GET /v0/search` | beta | local | the project the owner names; its paths (or `path`, one of them) |

```
GET /v0/search?project=shop&q=TODO&path=/Users/ana/code/Shop&regex=0
    <- {"project": "01J...", "engine": "ripgrep|walk", "roots": ["/Users/ana/code/Shop"],
        "matches": [{"file": "/Users/ana/code/Shop/main.go", "line": 12, "col": 5, "text": "// TODO: …",
                     "text_clipped"?: true}],
        "truncated"?: {"matches"?: true, "files"?: true, "time"?: true}}
   400: no q, q over 1,000 bytes, an invalid regex (regex=1), a path that is not one of the project's;
   404: no such project; 403: a token or tailnet caller (local route).
   Limits: 2,000 matches, 200 files, 5 s, 200 characters per line. The query always follows "--" in the
   driver's argv, a fixed string unless regex=1; .gitignore respected; binaries and hidden files skipped;
   symlinks never followed.
```

## Run targets (SPEC 20)

| route | stability | guard | check |
| --- | --- | --- | --- |
| `GET /v0/run_targets` | beta | authed | read on WriteTarget; `?describe=1` asks each driver (a failure is the row's `error`); also lists boxes other machines advertise, as `<binding>@<machine>` (SPEC 20 "Across machines"); `?host=1`: the machine's own machine-level bindings, machine owner only; `?live=1` (machine owner only, with or without `host=1`) adds `live` and `stopped` to this machine's rows (`slots` comes with describe) |
| `POST /v0/run_targets` | beta | local | admin on WriteTarget; the binding checked, describe must answer; project.toml rewritten live; with `host`, a box written to `$ZM_HOME/run_targets.toml` (`format = 2`) shared with `share` (project ids, or slugs of team projects this machine is in, stored as ids) instead |
| `DELETE /v0/run_targets/{target}` | beta | local | admin on WriteTarget; refused while the binding has live handles; `?host=1` removes a machine-level binding; a `<binding>@<machine>` name is its host's to remove |
| `GET /v0/run_targets/{target}` | beta | authed | read on WriteTarget; the binding, its effective class and describe |
| `POST /v0/run_targets/{target}/acquire` | beta | authed | operate on WriteTarget; a db, credentialed or unisolated target: the machine owner only; routing, caps and budget (SPEC 20) |
| `GET /v0/run_targets/{target}/acquires/{request_id}` | beta | authed | read on WriteTarget; the caller's own request_id |
| `POST /v0/run_targets/{target}/acquires/{request_id}/cancel` | beta | authed | operate on WriteTarget; the caller's own request_id |
| `GET /v0/run_targets/{target}/handles` | beta | authed | read on WriteTarget; only handles in areas the caller reads; `?live=1` only live handles; never a db url |
| `GET /v0/run_targets/{target}/handles/{id}` | beta | authed | read on WriteTarget; asks the driver and records the answer; never a db url |
| `POST /v0/run_targets/{target}/handles/{id}/release` | beta | authed | operate on WriteTarget; the holder (`actor`), or the machine owner |
| `POST /v0/run_targets/{target}/handles/{id}/renew` | beta | authed | operate on WriteTarget; the holder; within the lifetime cap since acquire |
| `POST /v0/run_targets/{target}/handles/{id}/exec` | beta | authed | operate on WriteTarget; a box's holder; `conflict` unless ready and before `expires_at` |
| `POST /v0/run_targets/{target}/handles/{id}/spawn` | beta | authed | operate on WriteTarget; a box's holder; `conflict` unless ready and before `expires_at`; the same `request_id` answers the same `pid` |
| `POST /v0/run_targets/{target}/handles/{id}/output` | beta | authed | operate on WriteTarget; a box's holder; waits up to `wait_s` (at most 20) |
| `POST /v0/run_targets/{target}/handles/{id}/signal` | beta | authed | operate on WriteTarget; a box's holder; TERM, INT or KILL to the process's group |
| `POST /v0/run_targets/{target}/handles/{id}/input` | beta | authed | operate on WriteTarget; a box's holder; bytes to a process's terminal, or a resize |
| `POST /v0/run_targets/{target}/handles/{id}/stop` | beta | authed | operate on WriteTarget; a box acquired with persist; its holder, or the machine owner; the disk is kept |
| `POST /v0/run_targets/{target}/handles/{id}/start` | beta | authed | operate on WriteTarget; a stopped box's holder only; a trusted host; a new lease within the lifetime cap |
| `POST /v0/run_targets/{target}/handles/{id}/port` | beta | authed | operate on WriteTarget; a live box's holder; bounded authenticated loopback TCP channel |
| `GET /v0/run_targets/{target}/handles/{id}/previews` | beta | authed | operate on WriteTarget; a live box's holder; current lease previews |
| `POST /v0/run_targets/{target}/handles/{id}/previews` | beta | authed | operate on WriteTarget; a live box's holder; publish requires explicit `public:true`, or revoke by preview id |
| `POST /v0/run_targets/{target}/handles/{id}/secrets` | beta | authed | operate on WriteTarget; a box's holder, on a trusted host, under a secrets grant at scope `box` (no default covers it); a driver listing `box-secrets`; never a name the pool mints itself (`403`, as for an uncovered name) |

```
POST /v0/run_targets -> {"target": {"name": "mac-vm", "driver": "tart", "kind": "run|db", "isolation": "ephemeral|none",
                         "credentials": false, "repo": "https://github.com/acme/app", "branch": "main",
                         "allow_any_ref"?: false, "scripts"?: ["test:e2e"], "budget"?: {"unit", "limit", "period": "day|week|month"},
                         "ttl_max_s"?: 3600, "handles_max"?: 2, "create_in"?: ["127.0.0.1:5432/postgres"],
                         "config"?: {"image": "macos-xcode", "key_file": "/abs/0600"}}, "replace"?: false, "setup"?: false}
                     <- RunTargetView {target fields..., "effective": {"isolation", "credentials"}, "describe"?, "error"?,
                        "host": "<machine serving it; this machine for a local binding>", "host_class": "trusted|public"}
   host binding: "host": true, "share": ["<team project id or slug>"] (box only, no repo; written to run_targets.toml as ids)
   <binding>@<machine>: a box another machine lends, from its advert (an enrolled host's only: trusted, or a class the owner set); acquire, status, release, renew and exec work as
   for a local box through a relayed driver call; on a public host exec "secrets" and acquire "jit_config" are 403;
   an unreachable host is unavailable or the driver's timeout (acquire: retry the same request_id).
   On a member's machine these routes are forwarded to the joined project's authority; POST/DELETE bind and unbind
   there answer 400 (bind on the machine that serves the binding). A host answers not_found for a handle another
   project's authority acquired.
   400: a bad binding (name, kind, class, repo not one of the project's remotes, no scripts on a credentialed or
   unisolated run target, a secret value in config, a budget the driver cannot count); 409: bound already
   (without replace) or live handles; 403: a token or tailnet caller (local route).
POST /v0/run_targets/{target}/acquire -> {"request_id": "auto-42", "ref": "main", "commit"?: "<sha>", "script": "test:e2e",
                                          "args"?: ["--unit"], "ttl_s"?: 1800, "actor"?: "<owner only>", "name"?: "<db>"}
   <- RunTargetHandle {"handle": "run:mac-vm/r7", "target", "kind", "request_id", "actor", "state": "running|ready",
                       "commit", "ref", "script", "expires_at", "ttl_s", "log"?, "url"? (db, this answer only), "spend"?, "created"}
   400: no ref, an unpushed ref, a shell-string script, ttl_s past the lifetime cap; 403: a commit that differs from
   the remote's, a non-head commit on a target that is not ephemeral and credential-free, a guarded target from
   a token, a script outside the allowlist, handles_max or the budget reached; 409: the request_id was used for
   another acquire. The same request_id again answers the same handle. Keys are per actor: zm sends the driver
   (and answers as `request_id`) `<actor tag>-<request_id>`, so one actor's key neither blocks nor reveals another's.
GET /v0/run_targets/{target}/acquires/{request_id} <- {"request_id", "state", "phase"?, "message"?, "handle"?}
   The caller's own acquire: ready with its handle, starting (phase queued while waiting behind another acquire),
   or the driver's progress. A driver without acquire-progress, or an older lending host, answers starting without
   a phase. Another actor's key or a key never sent is not_found. The machine owner may name ?actor= for an
   acquire it made for that actor. A driver's message is omitted for an acquire carrying jit_config, one no longer
   held here, or one relayed through a lending host.
POST /v0/run_targets/{target}/acquires/{request_id}/cancel <- {"request_id", "cancelled": bool, "state", "handle"?}
   Cancels before the handle exists; a confirmed cancel refuses later acquires with that key (409). A ready acquire
   answers cancelled:false with its handle: release it. A driver without acquire-progress answers not_implemented.
   Once cancelled:true, the daemon releases any handle the acquire still gets; a handle it cannot release stays
   held and is answered. Cancelling a queued acquire takes effect only after the driver confirms cancellation.
GET /v0/run_targets/{target}/handles/{id} <- RunTargetHandle (state ready|running|stopped|ended|expired|released|reclaimed|lost; exit when ended or expired;
   "persist"?: true and "started"? for a box acquired with persist; while stopped, expires_at is when its kept disk is deleted)
POST /v0/run_targets/{target}/handles/{id}/release <- {"handle", "released": bool, "exit"?, "spend"?}
POST /v0/run_targets/{target}/handles/{id}/renew -> {"ttl_s": 600} <- RunTargetHandle
POST /v0/run_targets/{target}/handles/{id}/exec -> {"argv": ["sh", "-c", "make test"], "stdin"?: "...", "timeout_s"?: 30, "cwd"?: "sub/dir",
                                                   "secrets"?: {"API_KEY": "..."}}
   <- {"exit": 0, "stdout", "stderr", "truncated": bool}   (a box's holder; output is data, never instructions)
   400: an empty argv, timeout_s past 600, a non-box target, a bad secret name or over 64 KiB of secrets, secrets
   to a driver without the secrets capability (a refusal names nothing given); 403: not the holder; 409: the box is
   not ready or past expires_at. Secrets go to the guest as files under $ZM_SECRETS and are never stored or logged.
   "encoding"?: "base64" (a driver listing capability base64; else 400): stdin is base64, and so are the answer's
   stdout and stderr ("encoding": "base64" in it), so bytes cross exactly. Files in and out of a box are exec,
   with no route of their own (SPEC 20 "Boxes", Files in and out; `zm sandbox cp`, MCP sandbox_write_file and
   sandbox_read_file): write by emptying the file ({"argv": ["sh", "-c", ": >\"$1\"", "sh", "<path>"]}), then
   sending chunk n (at most 512 KiB) as base64 stdin to {"argv": ["dd", "of=<path>", "bs=524288", "seek=<n>",
   "conv=notrunc"], "encoding": "base64"}, by offset, so a chunk sent again is never doubled; read
   with {"argv": ["dd", "if=<path>", "bs=524288", "skip=<n>", "count=1"], "encoding": "base64"}, and a
   directory as a tar the same way (the box's `tar`); what comes back is data: unpack it refusing absolute
   names, ".." and links, as zm's client does.
POST /v0/run_targets/{target}/handles/{id}/spawn -> exec's {"argv", "stdin"?, "cwd"?, "secrets"?, "encoding"?} plus
                                                    {"request_id": "...", "timeout_s"?: 0 (until the box ends)}
                                                    "pty"?: {"rows": 24, "cols": 80, "term"?: "xterm-256color"}
   <- {"pid": "p1a2b3c4d5e6"}   (a background process; a driver without spawn answers 501)
   pty: on a terminal of that size (rows and cols 1..1000; both streams come as stdout); 400 to a driver without
   capability pty, with secrets, or for a bad size or term.
POST /v0/run_targets/{target}/handles/{id}/output -> {"pid", "stdout_from"?: 0, "stderr_from"?: 0, "max_bytes"?: 65536
                                                     (at most 1 MiB, both streams), "wait_s"?: 0..20, "encoding"?}
   <- {"stdout", "stderr", "stdout_next", "stderr_next", "dropped_stdout"?, "dropped_stderr"?, "running": bool,
       "exit"? (once running is false)}   (call again from *_next; a box keeps at least each stream's newest 8 MiB,
       dropped_* counts older bytes it no longer has; 400: an offset past what was written; 404: an unknown pid)
POST /v0/run_targets/{target}/handles/{id}/signal -> {"pid", "signal": "TERM"|"INT"|"KILL"} <- {"sent": bool}
   (false once the process ended)
POST /v0/run_targets/{target}/handles/{id}/input -> {"pid", "data"?: "ls\r" (at most 64 KiB), "encoding"?: "base64" (any driver with input),
                                                    "rows"?: 30, "cols"?: 100 (together; applied first)}
   <- {"written": 3, "running": true}   (a process spawned with pty; Ctrl-C and Ctrl-D are bytes its terminal acts
   on; an ended process answers written 0, running false; 400: no terminal, rows without cols, over 64 KiB;
   404: an unknown pid; a driver without input answers 501)
Box (kind "box"): bind needs isolation ephemeral and credentials false (binding and describe), no scripts or
allow_any_ref, "images": ["name@sha256:<hex>", ...] (the first the default); repo and branch only to serve a ref.
Acquire takes "image"? (by name), "template"? (one of zm's box templates, e.g. "agent": the binding's image of that
template's name, any digest; not with "image"; 400 when unknown or the binding lends none), "ref"?, "commit"?, "jit_config"? (a GitHub JIT runner config the box starts first;
sent to the driver only, never stored or answered), "jit_guard"? ({"repo", "events"?}: the box stops a job of another
repo, event or fork before its first step; needs driver capability github-jit-guard), "net"? ("on", the default:
the binding's filtered network on a trusted host, none on a public one; "off": no network at all; off needs driver
capability net-off, else 400; on and unset are the same request, the other is 409 for a reused request_id; SPEC 20
"Boxes", Network); answers state "ready", "size", "image_digest" (a digest off the pin is released and answered 500)
and "net"? ("on" or "off": whether the driver made the box with a network; acquire's answer only, never stored;
sent net off and not answered exactly "off", the box is released and answered 500). "persist"?: true (SPEC 20
"Boxes", Kept disks): the box's disk is kept when it stops; 400 to a driver without capability persist, with a
jit_config, or for a non-box target; 403 on a public host; persist is part of the request (a reused request_id
without it is 409). "volumes"?: [{"name", "path", "mode"?: "rw"|"ro"|"cow"}] (at most 8; SPEC 20 "Boxes", Named
volumes): named volumes that outlive the box, mounted at path (relative to the box user's
HOME; absolute is 400); zm sends the driver "volume_owner": the caller's identity ("identity:<id>"), or "actor:<address>" for an
acquire the machine owner made for another actor, so a volume is its holder's alone; a host serving a binding to
several projects keys volume_owner and actor by the relaying project (as request_id), sets volume_project (whose
volumes share a cap) itself, and refuses any relayed arg key that is not the protocol's lowercase spelling (a driver
decoding JSON case-insensitively would take VOLUME_OWNER), so no project's authority names another project's holder; rw is one box's at a time, ro
and cow are shared (the driver answers 503 while the lock is taken); 400 to a driver without capability volumes,
with persist or a jit_config, for a bad name or mode, or "new" in the request; 403 on a public host; part of the
request (a reused request_id with other volumes is 409). The answer carries "volumes" (acquire only, never stored),
each with "new": true when this acquire made it, empty.
POST /v0/run_targets/{target}/handles/{id}/stop <- RunTargetHandle (state "stopped"; expires_at: when the kept disk
   is deleted, the driver's keep, 30 days by default)   (a box acquired with persist; nothing runs in it; exec,
   spawn and renew answer 409; a stopped box answers as it is; 400: acquired without persist; 403: not the holder or
   the machine owner)
POST /v0/run_targets/{target}/handles/{id}/start -> {"ttl_s"?: 1800} <- RunTargetHandle (state "ready", a new
   expires_at, "started")   (the same disk on the same host; the lifetime cap counts from this start; a ready box
   answers as it is; 403: not the holder, or the host is public now; 409: released or its disk expired; 503: no free
   slot, or the machine's owner has it, the disk staying kept). Release of a stopped box deletes its disk.
```

Box usage (SPEC 20 "Boxes"): one row per ended box, written by the usage module when run targets report the end.

| route | stability | guard | check |
| --- | --- | --- | --- |
| `GET /v0/usage` | beta | authed | read on WriteTarget; only rows in areas the caller reads; `?since=` RFC 3339 |
| `GET /v0/usage/limits` | beta | authed | read on the resolved project; only limits on areas the caller reads |
| `PUT /v0/usage/limits/{area}` | beta | authed | admin on the whole resolved project (an area's own admin cannot set its limit) |

```
GET /v0/usage?since=2026-11-01T00:00:00Z
   <- {"project": "acme", "billed_minutes": 42, "boxes": [{"handle": "box:linux/b3", "target", "area"?, "actor",
       "size"?, "image_digest"?, "end_state": "released|expired|reclaimed|lost", "acquired"?, "ended", "lease_s",
       "driver_unit"?, "driver_amount"?, "billable_minutes", "billed": bool}], "truncated"?: true}
   The newest 1000 rows by ended (fixed-width UTC times); billed_minutes counts every matching billed row, and
   truncated says more matched than were listed. Area guests read their area's rows too.
   billable_minutes = ceil(min(lease from acquire's answer to the recorded end or expires_at, whichever is first,
   the driver's minutes)); billed is false for reclaimed and lost boxes and for one that never answered acquire.
   400: a since that is not RFC 3339.
```

```
GET /v0/usage/limits
   <- {"project": "acme", "limits": [{"area": "ci", "minutes": 600, "period": "day|week|month"}]}
   The project's per-area sandbox minute limits ([[box_minutes]] in project.toml), only those on areas the caller
   reads; "limits" is [] when there are none.
PUT /v0/usage/limits/{area} -> {"minutes": 600, "period"?: "month"}   <- as GET /v0/usage/limits
   Sets the area's limit; minutes 0 removes it (also one left on an area that is gone); period day, week or month
   (UTC calendar periods, ISO week; default month). 400: negative minutes or another period; 403: not an owner or
   admin of the whole project; 404: no such area (for minutes above 0).
   Enforcement: before a new box in the area, or the start of a stopped one, run targets ask the usage module,
   which adds the area's billed minutes for boxes that ended this period, boxes that ended in the last hour whose
   row is not written yet, and each live box's minutes so far. At the limit the acquire is 403 (the hint names
   `potluck usage limit <area> <minutes> --per <period>`, which zm's CLI and MCP say as
   `zm sandbox usage limit`); minutes that cannot be read refuse too (503). A running box is
   not stopped; a box counts in the period it ends in; two boxes asked for at one moment can both pass.
```

```
POST /v0/run_targets/{target}/handles/{id}/secrets -> {"secrets"?: {"NAME": "value"}, "unset"?: ["NAME"]}
   <- {"dir": "<path in the guest>", "count": 2}   (the directory and how many names the box holds; no value, no name)
   400: a non-box target, a bad name, over 64 KiB; 403: not the holder, a public host, no grant covers a name (unsetting
   one's own names needs none), a name the pool mints; 409: the box is not ready. Values land in memory in the guest
   (`/dev/shm`), are never stored or logged, and ride sealed on any leg between machines (below).
   Since the bump to Potluck 1ad65786, exec/spawn `secrets` and acquire's `jit_config` are also checked at the
   authority against the pool's secret grants before anything is sent; the built-in defaults (a member may send
   `value` secrets to a trusted host's box, and hand it a `jit_config`) cover what worked before, so a pool with no
   grants behaves as it did.
```

## Secret grants (SPEC 20; potluck/grants)

Who may put which secret into which of a pool's boxes. A member's machine forwards these to the pool's machine.

| route | stability | guard | check |
| --- | --- | --- | --- |
| `GET /v0/secret_grants` | beta | authed | operate on the resolved project; the defaults first, then the rows in areas the caller operates in (a guest `403`) |
| `POST /v0/secret_grants` | beta | authed | admin on the grant's area (the whole project for none); an issuer kind needs its issuer on the pool's machine |
| `DELETE /v0/secret_grants/{id}` | beta | authed | admin on the grant's area; one in an area the caller cannot operate in answers `not_found` |
| `POST /v0/secret_grants/default` | beta | authed | admin on the whole resolved project; `{"on": bool}` turns both built-in defaults off or on |

```
GET /v0/secret_grants <- {"project", "grants": [{"id", "area"?, "to": {"identity"?|"role"?|"job"?}, "kind": "value|github-jit|...",
                          "name": "NAME|*", "ceiling"?, "host_class": "trusted", "scopes": ["exec"|"box"], "targets"?,
                          "created_by"?, "created"?, "builtin"?, "off"?, "corrupt"?, "stale"?}]}
POST /v0/secret_grants -> {"area"?, "to", "kind", "name", "ceiling"?, "host_class"?, "scopes"?, "targets"?} <- SecretGrant
POST /v0/secret_grants/default -> {"on": true}
   A grant add's body names a secret, so over the tunnel any non-empty body crosses only sealed; the member's
   transport never sends one in the clear, and the door answers `403` if one arrives.
   Events (never a value): dev.zm.secret.granted|delivered|refused (operate on the box's area), dev.zm.secret_grant.added|
   removed|default (admin). A replica does not copy them, an export leaves them out, and a webhook does not send them.
   A read over the tunnel omits the names; a count stays on `secret.*` events. The pool's machine shows the names
   on its own socket and over the tailnet. A member joined over the tunnel does not: its list is read through the tunnel.
   Rolling the pool's machine back to a build without grants stops enforcement (that build
   sends secrets as it always did); the table stays, unread.
```

## Sealing (SPEC 17.11 "Sealed secrets"; potluck/kit/tunnel, runtarget)

Secrets and a runner's `jit_config` are sealed on every leg between machines. A pool's mode says what a machine too old
to seal gets: `preferred` (a pool with no sealing setting; a project turned team now begins `required`) sends to it as before over the tailnet and warns; `required` refuses.

| route | stability | guard | check |
| --- | --- | --- | --- |
| `GET /v0/sealing` | beta | authed | read on the resolved project; its mode and the machines heard from since the daemon started (the machine list only for project-wide operators) |
| `PUT /v0/sealing` | beta | authed | admin on the whole resolved project; `{"mode": "preferred|required", "force"?}`; `required` is `409` while a machine heard from cannot seal, unless `force` |
| `POST /v0/sealing/forget` | beta | local | this machine's owner only; `{"pool"}`: forget that the pool's machine named a seal key (after its owner rolled it back on purpose) |

```
GET /v0/sealing <- {"project", "mode", "peers": [{"machine", "seals", "lends", "seen"}], "cannot_seal": ["machine"], "since"}
PUT /v0/sealing -> {"mode": "required", "force"?: true} <- Sealing        (event dev.zm.sealing.changed: who, from, to)
POST /v0/sealing/forget -> {"pool": "<slug>"} <- {"pool", "forgot": bool}
   Between daemons the headers Zm-Seal-Id, Zm-Seal-Key, Zm-Sealing, Zm-Seal-Sig and Zm-Sealed carry the keys and the
   sealed body; this machine's record of what its pool machines named is $ZM_HOME/tunnel/seal_peers.json (0600).
```

## Secret keys (SPEC 17.12; internal/secretkeys)

The key directory: each member machine's seal key, so a sender seals a secret to that machine before it leaves the
sender. The key is never taken from a body: a member's is the one its transport certified on the call (Zm-Seal-Id
and Zm-Seal-Key over the tailnet, the signing key's over the tunnel), the authority's own is its own. Answers carry
public keys only.

| route | stability | guard | check |
| --- | --- | --- | --- |
| `GET /v0/secret-keys` | beta | authed | read on the key's project/area; ?project= like ReadScope |
| `PUT /v0/secret-keys` | beta | authed | read on the resolved project/area; only the calling machine's own key: the authority's owner on its socket, or a member's node-bound token whose transport certified the key |
| `POST /v0/secret-keys/{fingerprint}/revoke` | beta | authed | the key's own machine, or admin on the whole project; a key the caller cannot read answers not_found |

```
GET /v0/secret-keys?to=machine:<name>|human:<name>&all=1 <- {"keys": [SecretKey]}   (active keys; all=1 adds ended ones)
PUT /v0/secret-keys (no body) <- SecretKey   (the same key again changes nothing; a new key replaces the machine's
                     active one; 409 for a key this machine once had replaced or revoked, 100 revoked keys, or a
                     call that certified no key; 403 for a caller that is not a machine)
POST /v0/secret-keys/{fingerprint}/revoke <- SecretKey   (the caller's own machine's row; an admin revokes every row
                     listing that key; an ended key answers as it is)
SecretKey = {"project", "machine", "owner" (redacted when hidden), "key_id": "zmkey:...", "seal_key": base64url X25519,
             "fingerprint": 26 base32, "state": "active|replaced|revoked", "published", "ended"?, "ended_by"?}
   A sender's pins (trust on first use, per project and machine; fingerprints recomputed from key_id and seal_key)
   are $ZM_HOME/secretdrop/pins.json (0600), the sender's own.
```

## GitHub runner broker (SPEC 20; potluck/runners)

Off until `config.json` has `runners` (then `unavailable` without the App). Owned by zm-compute-mac.

| route | stability | access | check |
| --- | --- | --- | --- |
| `POST /v0/runners/hook` | internal | public | none: `X-Hub-Signature-256` must verify with the App's webhook secret, else 401; answers nothing scoped |
| `POST /v0/runners/links` | beta | authed | admin of the area in the runner pool project (hidden like missing) |
| `GET /v0/runners/links` | beta | authed | links in the caller's readable areas of the pool project |
| `DELETE /v0/runners/links/{installation}` | beta | authed | admin of the link's area (hidden like missing); also disconnects every repository connected through that installation |
| `GET /v0/runners/jobs` | beta | authed | jobs of links in the caller's readable areas of the pool project |
| `GET /v0/runners/pool` | beta | authed | readable runner pool project; hidden like missing; placement inputs only, readable by lending members |
| `GET /v0/runners/oauth` | internal | public | a single-use `state` minted by `POST /v0/runners/links` or `GET /v0/runners/app/installed` (10 min); proves the person admins the installation's account; answers a confirmation page, nothing scoped. Without such a state, a return carrying `installation_id` or `setup_action` (an App that authorizes during installation: GitHub's "Install & Authorize" skips `setup_url`) is `GET /v0/runners/app/installed` exactly: only a live install `state` goes on, by a 303 to a sign-in with a fresh link state; GitHub's install-time `code` is never exchanged; no state answers a page naming `zm runners link <id>` |
| `POST /v0/runners/oauth` | internal | public | a single-use `state` from that confirmation page (the person's yes); makes the link |
| `POST /v0/runners/app` | beta | authed | owner of the runner pool project |
| `GET /v0/runners/app/new` | internal | public | a `state` minted by `POST /v0/runners/app` (24 h); answers the manifest form |
| `GET /v0/runners/app/created` | internal | public | that `state`, consumed; converts GitHub's code and keeps the App's secrets |
| `GET /v0/runners/app/installed` | internal | public | the App's `setup_url`: a single-use install `state` (1 h; from `POST /v0/runners/links` without `installation_id`, `POST /v0/runners/setup` or the App's creation page); GitHub's `installation_id` is only a claim: it must be this App's, then the link goes on to `/v0/runners/oauth`'s sign-in, admin check and yes; answers a 303 to GitHub's authorize URL or a page, nothing scoped; no `installation_id` keeps the state |
| `POST /v0/runners/setup` | beta | local | the machine's owner on its socket, also owner of the pool project (and admin of `area`); writes `config.json` `runners` (`pool.project`, `public_url`, `listen`) over the file as it is on disk, opens `listen` live |
| `POST /v0/runners/settings` | beta | local | the machine's owner on its socket, also owner of the pool project; writes one of `config.json` `runners` `label`, `job_ttl_s` over the file as it is on disk, served from the next job on (no restart) |
| `POST /v0/runners/repos` | beta | local | the machine's owner on its socket, also owner of the pool project; connects one repository of a linked installation (a public one only with `public`, the owner's yes); answers its record |
| `DELETE /v0/runners/repos/{owner}/{name}` | beta | local | the machine's owner on its socket, also owner of the pool project; disconnects a repository |
| `GET /v0/runners/repos` | beta | authed | connected repositories of links in the caller's readable areas of the pool project |
| `GET /v0/runners/doctor` | beta | local | the machine's owner on its socket; read-only repository checks (`?repo=owner/repo`, else the connected repositories and refused ones), GitHub reads through the owner's `gh` sign-in, within the broker's budget a run (potluck PR 72) |
| `GET /v0/runners/setup` | beta | local | the machine's owner on its socket; the broker's config and listener, secret references only as `secrets: bool` |

```
POST /v0/runners/hook        <- 202 {"accepted": true} (workflow_job, installation, others ignored), 401 (bad signature)
POST /v0/runners/links       -> {"installation_id": 123, "area"?: "team-a"}   <- {"authorize_url", "expires_at"}
POST /v0/runners/links       -> {"area"?: "team-a"}                           <- {"install_url", "expires_at"} (no installation yet)
GET  /v0/runners/app/installed?installation_id=123&setup_action=install&state=S  <- 303 Location: <web_url>/login/oauth/authorize?...
GET  /v0/runners/oauth?code=C&installation_id=123&setup_action=install&state=S  <- the same 303 (S an install state; C unused)
POST /v0/runners/setup       -> {"pool": "pool", "public_url"?: "https://h", "org"?, "name"?, "area"?}
                             <- {"pool", "public_url", "setup_url", "listen", "listening", "label", "job_ttl_s", "sizes"?, "default_size"?,
                                 "app"?: {"id", "slug", "client_id", "secrets": bool, "install_url"},
                                 "next": "create_app"|"install", "url", "expires_at"}
   public_url defaults to runners.public_url, else tunnel.url (400 with neither). listen keeps runners.listen, or
   a free 127.0.0.1 port when unset or taken; 409 when links serve another pool project or the App was registered
   at another public_url. Everything is checked before config.json or the listener changes; a new pool drops
   every pending state.
GET  /v0/runners/setup       <- the same without next, url and expires_at
   Once the App exists it also has "check": {"mode": "check"|"webhook", "every_s", "error"?, "error_at"?, "installations":
   [{"installation_id", "repos", "last_ok"?, "error"?, "error_at"?, "cut"?, "cut_at"?}]}: how waiting jobs arrive
   ("check": GitHub is asked every every_s, no public_url; "webhook": deliveries serve, a catch-up every every_s) and the
   checker's last round per linked installation (zm runners ls). error is why a round could not begin; an
   installation's error and cut are the lists that failed and why the round read less than everything.
GET  /v0/runners/pool        <- {"pool", "label", "job_ttl_s", "sizes"?, "default_size"?}; no App or listener details
POST /v0/runners/settings    -> {"key": "label"|"job_ttl_s", "value": "default"}   <- as GET /v0/runners/setup
   label: a name, or default for the pool's slug. job_ttl_s: whole seconds, 900 to 432000. 400 for another key or
   value (a public repository is switched on by itself, POST /v0/runners/repos below); a running job keeps the
   limit it was served with (zm runners set).
POST /v0/runners/repos       -> {"repo": "acme/app", "public"?: true}
                             <- {"repo", "installation_id", "public", "by", "since", "visibility"?, "approval"?, "warning"?, "checked"?}
   The App must be installed on the repository and its installation linked to the pool (404, 409 otherwise). A public
   repository needs public (400 without); a private one refuses it (the yes is given while it is public). Again
   changes the yes (zm runners allow <owner/repo> [--public]).
DELETE /v0/runners/repos/acme/app  <- {"repo"}; 404 if not connected (zm runners disallow)
GET  /v0/runners/repos       <- {"repos": [<as POST answers>]}  (zm runners repos; zm runners ls)
GET  /v0/runners/links       <- {"links": [{"installation_id", "account", "account_type", "area", "linked_by", "created"}]}
GET  /v0/runners/jobs?limit= <- {"jobs": [{"job_id", "repo", "run_id", "labels", "state", "why"?, "target"?, "runner"?, "created", "updated"}]}  (newest first; limit 1-500, default 50; zm runners jobs)
POST /v0/runners/app         -> {"org"?: "acme", "name"?: "Acme CI", "area"?: "team-a"}   <- {"url", "expires_at"}
```

## Mezze app door (SPEC 18.4; internal/door)

At a team project's authority. Control routes take `project` (on a member's socket the team proxy sends them on
to the authority) and act for the caller's machine only: its node-bound token's, or the authority's own on its
socket. A session or tailnet token is refused (403).

| route | stability | access | check |
| --- | --- | --- | --- |
| `POST /v0/door/hook/{token}` | internal | public | none: the task's URL token (32 random bytes) is the credential; a missing, rotated or switched-off one is 404; answers nothing scoped |
| `GET /v0/door` | app | authed | a machine's own daemon; read on the whole team project |
| `PUT /v0/door` | app | authed | a machine's own daemon; operate on the whole team project |
| `GET /v0/door/tasks` | app | authed | a machine's own daemon; read on the whole team project; its machine's tasks |
| `PUT /v0/door/tasks/{task}` | app | authed | a machine's own daemon; operate on the whole team project; target loopback under `/api/hooks/` |
| `DELETE /v0/door/tasks/{task}` | app | authed | a machine's own daemon; operate on the whole team project; its machine's task (hidden like missing) |
| `POST /v0/door/tasks/{task}/rotate` | app | authed | a machine's own daemon; operate on the whole team project; its machine's task |
| `GET /v0/door/tasks/{task}/deliveries` | app | authed | a machine's own daemon; read on the whole team project; its machine's task; never a body |

```
POST /v0/door/hook/{token}   <- 202 {"queued": true} (a repeated delivery id too), 404, 413 (over 1 MiB), 429 (200 queued)
GET  /v0/door?project=zm     <- {"project", "machine", "on", "available", "base_url"?, "tasks"}
PUT  /v0/door?project=zm     -> {"on": true}                                   <- the same
GET  /v0/door/tasks          <- {"tasks": [{"task", "machine", "label"?, "target", "url"?, "created", "rotated", "queued", "last_at"?, "last_state"?}]}
PUT  /v0/door/tasks/{task}   -> {"target": "http://127.0.0.1:3773/api/hooks/<task>/<token>", "label"?}   <- the task
GET  /v0/door/tasks/{task}/deliveries?limit=20
     <- {"deliveries": [{"id", "task", "delivery"?, "event"?, "bytes", "state", "attempts", "status"?, "error"?, "received", "ended"?, "next_try_at"?}]}
```

## Automations (SPEC 18.6; internal/automations)

At the project's authority (on a member's socket the team proxy sends every route there). Reads are scoped by the
record's project and area; a hidden record is `404`, byte-identical to a missing one. Write routes take only the
home rule: the record's home machine's own daemon (its socket owner, or at the authority its node-bound token),
with operate on the whole project, marked by the socket's origin gate (outside any agent session), naming the
install holding the home's claim; a definition write also needs that claim's `store` to be `zm`. Not the home is
`403`; the claim (another install, `store: z3`), a stale `rev` or a tombstone is `409`. No MCP tool.

| route | stability | access | check |
| --- | --- | --- | --- |
| `GET /v0/automations` | app | authed | read; `?project=` required; records in areas the caller reads |
| `POST /v0/automations` | app | authed | the home rule; `store: zm`; same project, home and `task_id` again is `409` naming the record |
| `GET /v0/automations/{id}` | app | authed | read on the record's area (hidden like missing) |
| `PATCH /v0/automations/{id}` | app | authed | read (hidden like missing), then the home rule; `store: zm`; `rev` read (stale is `409`) |
| `DELETE /v0/automations/{id}` | app | authed | read (hidden like missing), then the home rule; `store: zm`; sets the tombstone |
| `GET /v0/automations/{id}/history` | app | authed | read on the record's area (hidden like missing) |
| `PUT /v0/automations/{id}/health` | app | authed | read (hidden like missing), then the home rule (any store); a tombstone takes it too |
| `GET /v0/automation-homes` | app | authed | read on the whole project; `?project=` required |
| `PUT /v0/automation-homes/{home}` | app | authed | the home rule without the claim; another install is `409` and is recorded as the home's wanted install |

```
GET    /v0/automations?project=zm[&home=mac][&kind=automation][&deleted=1]
       <- {"automations": [Envelope], "holder"?: "<install holding ?home=>", "truncated"?}
          Envelope = {"id", "project", "area"?, "kind", "name", "enabled", "resume_rev"?, "owner"?, "home", "task_id", "rev",
                      "created", "updated", "updated_by"?, "deleted"?, "health"?: Health}
POST   /v0/automations   -> {"project", "area"?, "kind"?, "home", "task_id"?, "install", "name", "enabled"?, "owner"?, "fields": {...},
                             "body", "settings"?: {"<key>": {"type": "string|number|boolean|enum", "value", "options"?, "help"?}}}
       <- Record = Envelope + {"fields", "body", "settings"?}            409 {"error": "home ... already has a record for task ...: <id>"}
GET    /v0/automations/{id}            <- Record
PATCH  /v0/automations/{id}   -> {"rev", "install", "name"?, "enabled"?, "owner"? (null clears), "body"?, "fields"? (merge patch),
                                  "settings"? (merge patch), "replace_fields"?: true (fields whole)}   <- Record
DELETE /v0/automations/{id}?install=<install>[&rev=]                  <- Record (with "deleted")
GET    /v0/automations/{id}/history    <- {"revisions": [{"rev", "op": "create|update|delete", "updated", "updated_by"?, "name", "enabled",
                                           "resume_rev"?, "owner"?, "fields", "body", "settings"?}]}   (newest first)
PUT    /v0/automations/{id}/health   -> Health = {"install", "state": "healthy|degraded|paused|unapplied|conflict", "applied_rev",
                                         "apply_error"? (<= 300 bytes), "failure_streak"?, "secret"?: "none|set|missing", "hook_changed"?,
                                         "last_run_at"?, "last_run_status"?, "next_run_at"?, "as_of"}   <- Health + {"received"}
GET    /v0/automation-homes?project=zm <- {"homes": [Home]}
       Home = {"home", "install", "store": "zm|z3", "path"?, "claimed"?, "updated"?, "last_health"?,
               "wanted"?: {"install", "store", "path"?, "first"}}
PUT    /v0/automation-homes/{home}?project=zm   -> {"install", "store": "zm|z3", "path"}
       <- Home (this install holds it)   409 {"error", "data": Home} (another install holds it; this one is now "wanted")
```

## Owners (SPEC 22)

| route | stability | guard | check |
| --- | --- | --- | --- |
| `POST /v0/login/github/start` | beta | authed | link: the caller's own human identity, by the socket owner or a replica's node-bound token only; never a minted token or a tailnet login; at most 4 logins in flight per identity; key: the same starter, the project on `?project=`, the approvals brick's check (proof, single-use nonce, owns or administers the project, a member's call marked `origin_checked` else 403 `member-too-old`) |
| `POST /v0/login/github/{id}` | beta | public | its own: the login's single-use poll secret and the identity that started it; one 404 for an unknown id, a wrong secret, another caller and a collected or expired outcome |
| `GET /v0/owner` | beta | authed | read on the project named by `?project=` (else not_found); the caller's own owner right, judged as `POST /v0/asks/{id}/choose` judges it, and its GitHub link |
| `POST /v0/grants/github` | beta | authed | admin on the project/area; the login resolved to its GitHub id now |
| `DELETE /v0/grants/github` | beta | authed | admin on the project/area |
| `GET /v0/grants/github` | beta | authed | admin on the project named by `?project=` |
| `GET /v0/join/github` | internal | public | none: "a zm authority", its protocol and machine; no project input, no project list |
| `POST /v0/join/github` | internal | public | a tailnet node WhoIs identified (not the socket, not this machine, not a tagged node), or a tunnel caller's key (not the home's own); any project name starts a login; optional area and role scope it (the tunnel requires an area and is guest only); only that node or key collects; at most 4 logins in flight per node |
| `GET /v0/projects/{project}/names` | beta | authed | read on the project |
| `GET /v0/names/{owner}/{name}` | beta | authed | read on the project the name reaches, else answered like a missing name |
| `DELETE /v0/identities/{identity}/github` | beta | authed | the machine owner on the socket, the identity's own replica (node-bound token), or an admin of a project it holds a grant in; never a minted token or a tailnet login; anyone else gets the 404 of a missing identity |

```
POST /v0/login/github/start -> {"purpose": "link"}
                            <- {"id": "01J...", "secret": "<poll secret, 192 bits>", "user_code": "WDJB-MJHT",
                                "verification_uri": "https://github.com/login/device", "expires": "...",
                                "authority": "mini", "for": "identity jo"}
   The authority asks GitHub for the device code with its own client id; the person approves in a browser.
   403: a minted token, a tailnet login, a non-human identity; 429: too many logins in flight; 503: no
   github.client_id, or GitHub did not answer. On a replica this is forwarded to the project's authority.
POST /v0/login/github/{id}  -> {"secret": "<poll secret>", "wait_ms": 25000}
                            <- {"pending": true}                       (not approved yet; poll again)
                            <- {"link": {"identity": "jo", "account": "github:583231", "login": "jo",
                                         "linked": "..."}}             (once)
   The authority polls GitHub, receives the token itself, asks GET /user who approved and drops the token
   (never stored, logged or answered). It links only now, when the outcome is collected; an outcome nobody
   collects within 2 minutes expires with nothing linked. A first link mails the identity and the owners of
   each project it holds a grant in. 403: declined, expired, an org or bot account; 409: the identity is
   linked to another account, or the account to another identity.
POST /v0/login/github/start?project=<slug> -> {"purpose": "key", "key": "<base64 X9.63 P-256>", "device"?: "Mezze app", "install"?: "<id, shown only>",
                                "nonce": "<8-128 of A-Za-z0-9_.:=+/->", "time": "<RFC 3339, within 5 minutes>",
                                "proof": "<base64 ASN.1 ECDSA SHA-256 over zm-bind-key/1\n + receipt key id + \n + project slug
                                (as sent) or id + \n + key + \n + nonce + \n + time>"}
                            <- as for link; the poll's outcome is {"key": {"approver": "key-<machine>-<16 hex>",
                                "fingerprint": "p256:<16 hex>", "state": "active", "receipt_key", "account": "github:<id>",
                                "login", "machine"}}
   The key answers at once, as the identity that started the login, from the node it came from; the person's
   other bound key on that node and their keys paired the old way end. 403: bad proof, nonce used before, the
   identity does not own or administer the project, member-too-old; 409: the key is another person's, or the
   account belongs to another identity (a key login never joins two people). The latest key login of one
   identity from one node wins; the older one's poll answers 404. An old authority answers 400 unknown purpose.
DELETE /v0/identities/jo/github?revoke=1
                            <- {"identity": "jo", "account": "github:583231", "login": "jo", "revoked": []}
   Removes the link (revoke=1: and the identity's node-bound tokens), and mails the identity and the owners;
   the GitHub user then links again with a fresh device flow. 404: no such identity, not linked, or not yours
   to unlink.
GET /v0/owner?project=shop <- {"project": "shop", "identity": "jo", "owner": true, "github"?: "jo"}
   owner: this identity answers the project's card asks (core.Deps.ProjectAdmin, as choose); github: the linked login,
   absent when none. On a member machine it is forwarded to the project's authority, which judges the identity's own
   grants. A client shows choice buttons on owner, and offers zm login github when not linked.
POST /v0/grants/github      -> {"owner": "@bobjones", "role": "owner", "project": "shop", "area"?: "solver"}
                            <- {"account": "github:9919", "login": "bobjones", "kind": "user", "role": "owner",
                                "project": "shop", "created": "..."}
   The login is resolved to its numeric id now; an org ("@example-org") grants its active members, by the org's
   id, checked live at each join and re-checked hourly with github.lookup_token. 404: no such GitHub account;
   403: a bot, not admin, or an org this authority's lookup token cannot see the members of. No identity
   exists until the person joins.
DELETE /v0/grants/github    -> {"owner": "@bobjones", "project": "shop"}   <- {"revoked": true, "admissions": 1}
   Also revokes what the grant gave: the grants rows it made and the node-bound tokens of each join.
GET /v0/grants/github?project=shop   <- {"grants": [GitHubGrant, ...]}
GET /v0/join/github         <- {"authority": true, "protocol": "potluck/1", "machine": "mini", "github": true}
POST /v0/join/github        -> {"project": "shop", "machine": "bobs-mac", "area": "solver", "role": "guest"}
                            <- GitHubLoginStart ("for": "node bobs-mac (n...) as machine bobs-mac")
   Any project name starts the login. "area" and "role" are optional on the tailnet and scope the admission:
   exactly that area, at the role asked (at most; guest < member < owner) when the account's GitHub grants give
   at least that much there (a whole-project grant covers any area, an area grant only its own); asking for
   more, an unknown area or an area no grant covers is the same 403 as no grant. Without them the tailnet gets
   the strongest grant. Over the tunnel (a signed caller, no token: this route, GET /v0/join/github and POST
   /v0/login/github/{id} pass the door tokenless) "area" is required (400) and "role" is guest or omitted; the
   node is the caller's key id and only that key collects. A tunnel join is guest only: another "role" is a 400. Tunnel starts have their own pool (8) and per-address limits (4 in flight, 20 an hour); the POSTs carry a replay cache of their own, capped per client address (a replay is a 400). A join again from the same node keeps the stronger
   of its old and new role (and that token's area): a lower request never downgrades it. Then POST /v0/login/github/{id} from the same node collects it once, and
   only then does the authority act: the strongest GitHub grant of the approving account (its own, or an org's
   it was an active member of when GitHub answered), the identity linked to the account (created, named after
   the login, when new; a taken name gets -2), and a node-bound token for that node, as invite redemption gives:
   <- {"join": RedeemResponse}. An unknown project, one that is not a team project, one with no GitHub grants
   and no grant for this account are one 403, the same bytes but for the name typed. A first join from a node
   mails the identity and the project's owners, naming the node. 409: the machine name is another node's.
POST /v0/login/github/start -> {"purpose": "bind", "project": "shop", "owner": "@acme", "name": "shop"}
   A bind: an admin of the project, a person; the name a project name (2+ characters, no reserved or blocked word, no zm-
   prefix). When GitHub answers, the authority looks the owner up and (an org) asks live whether the approver is an
   admin of it; when the outcome is collected it binds: <- {"bind": NameBinding}. 403: the approver is not the
   binder's linked account, the owner is another person, an org the approver is not an admin of, a bot, or
   frozen; 404: no such GitHub account; 409: the name is bound to another project under that owner, or the path
   already reaches another account's project here. The project's own name again makes it current (a rename back).
GET /v0/projects/shop/names  <- {"names": [{"path": "@acme/shop", "account": "github:9919", "name": "shop",
                                              "project": "01J...", "current": true, "bound": "..."}, ...]}
GET /v0/names/acme/shop      <- {"project": "01J...", "slug": "shop", "path": "@acme/on-call", "moved"?: true,
                                   "frozen"?: true}
   A path this authority bound or answered before is pinned to the owner it reached and follows the pin, whoever
   holds the login on GitHub now (a renamed owner's old links keep working; a newcomer under the login never
   gets them). A new path goes to the login's current holder (cached 1 hour; a login GitHub no longer answers
   redirects one hop to its last account). "frozen": GitHub no longer answers for the owner's id (suspended or
   deleted): read-only, and nothing is bound, renamed or transferred under it. 404 for a missing name and for
   one reaching a project the caller cannot read, alike.
```

## Project summaries

| route | stability | access | check |
| --- | --- | --- | --- |
| `GET /v0/summaries` | beta | authed | explicit hosted project, whole-project read |
| `POST /v0/summaries` | beta | authed | explicit hosted project, whole-project operate |
| `GET /v0/summaries/config` | beta | authed | explicit hosted project, whole-project read |
| `PUT /v0/summaries/config` | beta | authed | human project owner, explicit whole-project admin |

`GET /v0/summaries?project=tools&q=&limit=30` returns `{summaries:[ProjectSummary]}`; limit 1–100, one-line q at most 512 bytes. Area-only readers receive the same missing-project answer as unknown projects. Histories and schedules remain on their project authority; joined-project calls with an explicit project forward there and fail visibly if it is unavailable.

`POST /v0/summaries?project=tools` accepts `{day?:"2026-11-04"}` (default today in the configured timezone; no future dates or dates over one year old). It returns a durable `{id,project,day,timezone,config_version,generated,body,counts,notices?,partial,notes}`; `notices` counts, by plain label, the local day's machine notices that went to no person's inbox (green CI results, lease expiries, old machine notices the mail sweep acked). Repeated project/day/config-version returns the saved result. Reads sample at most 1,000 work items per status, display at most 40 titles per status, and keep the body within 64 KiB; reached bounds and missing/stale sources set partial and explain coverage. Counts are current work snapshot counts, with done entries restricted to those updated on the requested local day, not proof of closure/deployment. No mail, comments or transcript content is read (machine notices are counted, never quoted). At most 400 summaries are retained per project.

`GET /v0/summaries/config?project=tools` returns `{project,enabled,hour,minute,timezone,version}`. Defaults are disabled, 07:00 UTC. PUT replaces enabled/hour/minute/timezone; timezone is an installed IANA timezone, hour 0–23, minute 0–59. Version binds the complete effective configuration. Only an authenticated human project owner can enable a schedule (whole-project admin); attributed agent sessions are refused. A one-minute daemon loop generates yesterday's digest after the configured local wall time, including on restart; it does not backfill older days, send mail or invoke an external model. DST uses calendar days. Manual generation remains available with scheduling disabled. Unavailable sources are explicit; a failed read saves nothing.

After a new archive row commits, `dev.zm.summary.created` carries a content-free `{id,day}` refresh hint in that project. The event has project-level scope, never summary text, and is not a delivery/read receipt. Native views reload through the existing event stream rather than poll history.

Schedule responses additionally report optional last_attempt, last_generated and a generic last_error. These are server-owned status, excluded from the configuration version; PUT ignores supplied status. A concurrent schedule edit fences an old run's status update. Scheduled failure/recovery changes emit content-free `dev.zm.summary.schedule_changed` hints, so a visible native summary view can refresh its status. Archive replay guarantees apply while a row is retained; retention keeps the 400 most recently generated summaries per project.

Archive redaction applies the exact displayed title rendering, including sanitation and truncation. New archive records retain only private fixed-size subject/line hashes (not exposed by summary API responses) so shortened IDs cannot direct a short-text redaction at another work item. A redaction of a captured subject removes that captured title as a unit; legacy records also scrub rendered variants with short-text matching restricted to the subject line. Individual title truncation sets partial and a note, independently of source availability. Successful schedule edits, including disabling a schedule, emit the same content-free schedule_changed refresh hint as scheduler status changes.

### Sandbox HTTP previews

`zm sandbox preview publish box:<target>/<id> <port> --public` calls the shared
Potluck preview API with explicit public consent. `preview ls` lists that box's
previews; `preview revoke <box handle> <preview-id>` ends one and reports
`{"preview":"<preview-id>","revoked":true}` after success. The opt-in MCP tools
`sandbox_preview_publish`, `sandbox_preview_list` and `sandbox_preview_revoke`
use the same caller and routes. Publishing without `public:true` is refused by
the authority. Preview URLs are public HTTP application access, bounded by the
box lease and revocation; credentials and raw browser CDP remain private.

The machine owner uses `zm sandbox preview configure <existing-domain>` (empty
domain disables publishing), calling
`POST /v0/tunnel/preview-domain {"domain":"preview.example.com"}`. Generic public
host routing uses the tunnel ingress; the runner broker is a separate service.
