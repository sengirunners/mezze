# zm/v0

Mezze (the command is `zm`) is the bus for coding agents: one small daemon per machine that carries
mail, work items, leases, sessions and events between agents (Claude Code, Codex, OpenCode, Gemini CLI, a shell)
and the humans directing them, grouped by project. Agents use the same CLI humans do.

Principles:

- **The bus, not the apps.** zm moves messages and state. Agents, trackers and UIs plug in.
- **Boring core, swappable doors.** The core is HTTP + JSON + SSE, SQLite, git, PTYs and the shell.
  MCP, tracker adapters and Tailscale are doors at the edge.
- **Mail is data, never authority.** A mail body never runs anything and never grants anything. The
  daemon never types a mail body into a terminal; it types at most one fixed line (the nudge).
- **Zero accounts on one machine.** One machine works with no sign-up and no network.
- **Rules are code, not prose.** Scope is enforced by the store and the router and proved by one golden
  leak test (`internal/wiring/fulltest/leak_test.go`), not by each module remembering.

## 1. Trust model (read this first)

1. **The Unix socket is one principal: the OS user** (socket mode 0600 in a 0700 `$ZM_HOME`). Every
   unauthenticated socket caller, human or agent, is the machine's owner, with scope *all*. Project and area
   checks on the socket are **guard rails against mis-filing, not security**. `Zm-As` only picks the
   caller's reply address (a session on this machine, or this machine's human); it grants nothing.
2. **The only hard boundary is the network listener** (the tailnet door, section 13, and the tunnel door, 17.11). It is **fail-closed**:
   the Unix listener positively tags each connection local (`http.Server.ConnContext` on a `*net.UnixConn`
   sets `core.WithLocal`); any request without that tag is network, whichever listener it came through and
   whether or not the listener remembered `core.WithRemote` (which only carries the WhoIs login). Every
   network request must carry a bearer token (`Authorization: Bearer potluck_...`, older `zm_...` tokens still work; the registry stores only its sha256; each
   token is capped at a role and optionally one project and area; revocable; optionally expiring) or come
   from a Tailscale WhoIs login mapped to a registry identity. Anything else gets `401 unauthenticated`
   (only `GET /v0/health` and the public receipt key answer, with no machine details, plus `POST
   /v0/invites/redeem`, which answers only a tailnet node WhoIs identified, 17.4; the tunnel door also passes the GitHub join's calls, signed, 17.11). A node-bound token (17.4)
   works only from its tailnet node, never on a Unix socket. **A mapped login is ambient** (the person's
   browser presents it with any request any page makes to the node, like a cookie), so a login caller without
   a token must also prove a zm client sent the request: its `Host` names this node exactly (a Tailscale
   address, or the name the tailnet gave the node, bare or full MagicDNS; before the node has one,
   `potluck-<machine>` (`zm-<machine>` is accepted too) or the configured name; never a prefix: DNS rebinding), it is not cross-site
   (`Sec-Fetch-Site` same-origin or none, `Origin` this host when sent), and every write carries
   `Potluck-Protocol` (read under its older name `Zm-Protocol` too; no page can add it cross-origin without a CORS preflight, which the daemon never grants).
   Otherwise `401`, checked before the login is looked up, so the answer is the same for a mapped and an
   unmapped login (`core/ambient.go`). One right is outside
   this model: an approver's `decide` (section 16) belongs to a paired tailnet device's node, never to the socket or
   a token; the project's owner also answers card asks from their own machine (section 16 "Owners decide").
3. **Sessions act with at most their launcher's scope.** A session's subject is its launcher's scope with the
   session as reply address (`authz.Subject.ForSession`), never more. In v0 only the machine owner can start or
   register sessions (those routes are `local`), so every session's scope is the owner's. A token presented
   on the socket always narrows the caller (tests use this); `local` routes refuse any token caller. A session
   started for another machine's launcher (section 8 "Sessions on other machines") is capped instead: a request
   acting as it (`Zm-As`), or as a session or task it starts, has the owner's scope in that one project only,
   never above the launcher's role there. The cap follows the session however it reaches the daemon: a request
   from its process tree (found by process ancestry) is capped as one sent with `Zm-As`; a session it registers
   or starts takes its cap and its project; it binds, revives, moves the pid of, ends or updates no session but its own
   and those below it (a hook naming another session's runtime id is `not_found`, with or without a pid); the task routes apply every cap
   along the session's parent chain (`session_lineage`); every route that operates a session by id (state, name,
   stop, input, resize, attach, output, project, prompt, turn, interrupt, answer, events) or reads one by id (the
   session, activity, subagents, prompts) answers `not_found` for any other. Its process stays capped after its
   session ends, until that process exits (recorded with its start time, never a later process at that pid); the
   session never moves its pid off that live process (`conflict`). While the daemon cannot load the caps, every
   session here counts as capped to nothing.
   Through zm's routes the one way out is a process that
   leaves its session's process tree and names another, uncapped session in `Zm-As`: every socket caller is the
   machine's owner (rule 1), so a cap is a guard rail for agents acting through zm, not a boundary against a
   same-user process.
   The socket itself stays the OS user's, as for every session on the machine.

## 2. Runtime and storage

- One binary, `zm`. `zm daemon` serves; every other subcommand is a client. Adapters are separate
  executables `zm-work-<name>`.
- `$ZM_HOME` (default `~/.zm`, mode 0700):

```
zm.sock                     the API (Unix socket, 0600)
config.json                   optional (human, machine, agents, default_project, admission, tailnet,
                              lease_tick_seconds, bound_lease_ttl_seconds, idle_after_seconds, agent_nice); the daemon
                              writes `machine` at its first start (below)
registry.db                   projects, identities, grants, tokens, sessions, leases, machine-level events
projects/<ulid>/project.db    that project's mail, local work items, comments, events (a team project's leases too, 17.3)
projects/<ulid>/project.toml  that project's config (slug, name, repo rules, areas, trackers)
projects/<ulid>/issues.db     that project's native issue store (section "Native issues")
backup.json                   the off-site home backup and the watchdog heartbeat (17.9; secret references only)
offsite/                      that backup's staging and rclone's scratch (0700)
scratch/<session8>/           per-session scratch dirs
worktrees/<session8>/         zm run --worktree (never deleted by zm)
sessions/<session8>.out       last output of an ended managed session (pruned after 7 days)
holders/<session8>.sock       a live managed session's holder (0600, dir 0700; section 8); .exit and .log beside it
tools/                        the pinned tools zm fetched (tools/manifest.json: versions, sha256)
trackers/<ulid>/<prefix>/     a tracker's own files (its adapter's data_dir),
                              home.json (its backups; 17.9), moves/<id>/ (a restore's staging)
secrets/<ulid>/<prefix>.password  a --server tracker's password (0600, dir 0700)
receipt.key                   the ed25519 key that signs approval receipts (0600)
approvals/github/<ulid>.token optional per-project GitHub bot token for posting receipts (0600)
jobs/<ulid>/                  left by an import job before 2026-11-01 (17.5, retired); nothing reads it, safe to delete
remotes/<ulid>.token          a joined team project's node-bound token (17.4; 0600; <ulid> = the authority's project)
daemon.log                    when detached
daemon.lock                   flock held by the one running daemon (its pid inside); a second daemon on
                              the same home is refused even if the first one's socket is gone
swap.lock                     flock held by a stop, restart or upgrade (who, what, since, pid, nonce inside; "Swaps")
served.json                   what this home's daemon served at its last clean self-check, and its build ("Swaps")
swap-last.json                the last swap record (health's last_swap)
daemon-path                   the PATH the last daemon ran with: a daemon at start appends its existing directories
                              its own PATH lacks, then rewrites it, so a starter with a minimal PATH (a launchd
                              updater, a service manager) does not lose agents (delete it to forget them).
                              Ignored unless the user's and not group- or world-writable; skips directories that are
                              group- or world-writable or owned by neither the user nor root; at most 64 entries, 8 KiB
swap-pending.json             an upgrade past its commit, removed when it ends verified, rolled back or forward, or
                              untouched; found with swap.lock free, its swapper died ("Release channel")
update-release.json           what this home installed from the release channel, and which releases ran stably or
                              failed here (section 2, "Release channel"); update-last.json: the last update check
update.hold                   the hold switch: while it exists no update installs anything (zm daemon update hold)
update-rollout.json           the canary rollout's settings: canary, members, authority, idle_processes (a dry run only);
                              update-rollout-dryrun.jsonl: its dry-run log
autostart.lock                flock held by a CLI command while it starts the daemon (section 17.1), so
                              commands started at once start one daemon
```

- **One authoritative store per project, no client-side sync.** Clients never cache or fall back; if the
  daemon is unreachable or speaks another protocol, the client refuses (exit 3) and says why: "not running"
  only when it never connected (most commands first start it, section 17.1); a request the daemon dropped after connecting (it stopped or restarted) says
  so, and for a write that it may or may not have taken effect. A live event stream (`zm events`) that
  ends because the daemon stopped says the same (exit 3); only a replay (`--no-follow`) ends quietly.
- **The machine name is stable.** Sessions are named `<machine>/<session8>` (hooks build that address on the
  client), so the name must not follow the host name, which a network's DHCP can change (a Wi-Fi renamed a Mac
  to `unknown42a411d299bc.attlocal.net`). The name is `$ZM_MACHINE`, else config.json `machine`, else the host's
  name: on macOS `scutil --get LocalHostName` (the Bonjour name, never changed by a network), elsewhere the short
  host name. At its first start the daemon writes the name it uses into config.json (keeping every other key;
  `Save` keeps unknown keys too), so it never changes after, and logs what it wrote and from where. A home whose
  daemon ran before (an upgrade from a zm that did not persist) keeps the name of its last `dev.zm.daemon.started`
  unless that was a DHCP name (`unknown<12 hex>`). A Mac with no LocalHostName right now, or no name at all, is not
  persisted (warned; tried again at the next start). `$ZM_MACHINE` overrides a persisted name for that run, with a
  warning in the daemon's log (sessions named under the persisted name will not match); on a fresh home it is what
  gets persisted. Upgrading the CLI while an older daemon runs: restart the daemon, so both read the persisted
  name. A `$ZM_HOST` client never looks up the host's name.
- **Swaps** (`potluck/kit/swap`; DESIGN-NOTES "Safe swaps"). No session can break what this machine serves by
  installing or restarting zm:
  - `zm daemon preflight [--json]`, run by the build that would run next, checks it against the running daemon's
    manifest (`GET /v0/daemon/manifest`), or with none against the home's databases. It refuses (exit 1, each reason
    listed): another protocol major; a migration applied in `registry.db`, a `project.db`, an `issues.db` or a replica
    copy that the build does not register; while a team project is joined, a route the running build forwards to an
    authority that this one does not; a route answered since the daemon started that this build lacks. A route it
    lacks that nobody called is a warning. A daemon older than manifests is checked on protocol, databases and what a
    joined project needs (`GET /v0/issues`, `/v0/issues/projects`, `/v0/issues/{id}`, `/v0/work` forwarded). A build
    that registers migrations the home's databases lack notes that the swap is one-way (it names them): the previous
    build cannot read the data once they are applied.
  - A launchd-owned upgrade snapshots its existing user-owned LaunchAgent plist and verifies its label,
    executable and home before placing files. It re-registers only that unchanged owning job from the same
    plist, preserving its environment, so a replaced executable gets a fresh launch requirement. Missing,
    changed or foreign jobs refuse; registration failure uses the same guarded file rollback and exact job.
  - `zm daemon upgrade <zm> [--dir D]` takes `swap.lock`, runs `<zm>`'s preflight, places `<zm>` and the `bd-zm` and
    `zm-*` files beside it in D (default: the daemon's directory), keeping each previous file as `<name>.prev`, then
    stops the daemon, starts the binary it ran from with the same arguments, and verifies: it answers within 15 s, it
    is the build the preflight described, and it serves what the old one did (the served set: joined projects, issue
    projects and each one's first issue, each project's first work item, each tracker's adapter, each task agent
    whose binary resolves in `GET /v0/tasks/agents` (an agent the old daemon found and the new one cannot is a
    regression; a failing auth probe is login state, not one; the self-check does not list agents; each run-target
    binding this machine serves, described by the old daemon before a file moved: SPEC 20, "Drivers are pinned"); a project that left
    is not expected, but a joined project, from `GET /v0/team/remotes` with state ok, always is, even when the new
    daemon's lists drop it). A regression that can be the new daemon's tailnet still coming up (an item of a joined
    project missing, or failing `unavailable`, such as "authority ... unreachable") is probed again every 2 s for up
    to 90 s before it counts; local regressions that all fail `unavailable` (a store still busy at the new daemon's
    first start) are probed again the same way for up to 30 s; any other regression refuses at once. On a failure it stops the new daemon (the stop
    route as the lock's holder, then the pid it saved), puts the `.prev` files back, starts the previous binary and
    exits 1, saying what runs now. When the new daemon answered but the databases now carry migrations the previous
    build does not register (from the old daemon's manifest; for one older than manifests, those applied since the
    swap began), it rolls forward instead: the new daemon keeps running on the new files (the `.prev` files stay),
    the record's result is `rolled_forward` with what failed and why it was kept, and it exits 1, loudly. A new
    daemon that never answered is rolled back even then, with a warning that the previous build runs on newer data. A daemon that a
    systemd unit or a launchd job runs as its main process is restarted through it (`systemctl [--user] restart`,
    `launchctl kickstart -k`), never beside it; an upgrade into a directory other than the one its `ExecStart` (or
    program) names is refused before anything is placed, naming that binary. A daemon running beside such a unit or
    job (a loaded launchd agent in `~/Library/LaunchAgents` whose program is the daemon's binary on this home) is
    stopped first (the stop route, as the lock's holder) and the manager then starts its own, rollbacks too, so the
    swap ends with the manager's daemon. The 15 s start wait counts from the old process's exit (at most 90 s). A
    binary's preflight has 60 s. Runbook: `docs/runbooks/zm-upgrade.md`.
    A daemon running from outside D is not restarted. With no daemon it only places the files.
    `make install` builds into a temp directory and runs this (`FORCE=1`, `END_SESSIONS=1`, `INSTALL_DIR`).
  - `zm daemon restart` is the same with the binary on disk and no files placed (so no rollback: it says so).
    `zm daemon stop` runs the preflight of the binary on disk too (the next command autostarts it).
  - All three refuse, before anything moves, while `swap.lock` is held (naming who, what and since) and while managed
    sessions would end (listing them). `--end-sessions` ends them; `--force` overrides the preflight, loudly, and the
    record keeps what it overrode. `zm daemon stop --force` does both, as before. The daemon's stop route refuses
    while `swap.lock` is held unless the request carries its nonce, and `dry_run` checks without stopping. An
    autostart never starts a daemon while `swap.lock` is held (checked again under `autostart.lock`): it uses the
    swap's daemon as soon as one answers, or once the swap ends, and after 2 minutes fails naming the swap.
  - A host owner can opt into periodic `zm daemon update --repo <canonical checkout> --dir <install dir>`.
    It selects the latest finished successful ci.yml push/main run in the canonical repo, verifies main
    ancestry, holds rather than downgrade a newer running build, refuses unresolved/divergent running commits,
    and builds the exact SHA in a detached temporary worktree. It uses the same staged upgrade,
    never force/end-sessions; native moved-out ID collisions refuse read-only before install. A hold file or
    daily local-time quiet window blocks both before build and before install. One updater per home runs
    under update.lock. Every check reports held/unchanged/failed/ok via dev.zm.daemon.update_checked and JSON
    scheduler output (also the fallback when no daemon can publish); last_swap remains the actual swap.
  - **Release channel**: machines pull signed releases;
    nothing is pushed to them and nothing needs a shell on them. After each green `ci` push to main (and daily, and on
    demand) CI builds the last green main with `scripts/release`, writes `manifest.json` (`zm-release/1`: version,
    full commit, created, SHA-256 of each tarball) and signs its exact bytes with an Ed25519 key held only in the
    `zm-release` environment (`scripts/release-sign`), and publishes a GitHub release tagged `zm-main-<sha12>`; it keeps
    the newest 50 such releases and every one younger than 14 days. `zm daemon update --release github --dir <install dir>` (or a directory holding one release) walks the newest 100
    releases, newest published first, skipping drafts, prereleases (a **yanked** release: `gh release edit <tag>
    --prerelease`; deleting a release is not a yank, since the next run may publish the same green main again) and other tags. It stops at the running build's release (`unchanged`) or at the one
    this home last installed (`$ZM_HOME/update-release.json`: never a rollback; an unreadable one holds the updater until fixed or removed, so failed installs are never forgotten), and otherwise takes the first that is
    `--min-age` old (its ring: the canary 0, the rest later), did not fail here, and, when `--min-age` is above 0 and
    unless `--no-canary`, passes the **canary gate**: another machine in its machine numbers (`GET /v0/machines`)
    lists the release as stable and none lists it as failed; with no list or no vouch it holds, naming why. When it
    skipped newer releases before stopping, the result is `held` with the first one's reason. A machine lists a release
    as stable once it ran it 20 minutes with no daemon restart since its install (the last 20), and as failed when its
    install attempted a swap that did not end `ok` or left no daemon answering (the last 20; never retried there; a
    refusal before anything moved is retried next tick). A tick that finds `swap-pending.json` with `swap.lock` free
    (a swapper killed past its commit: its build may run unverified) reports `failed`, records that build failed here
    and removes the marker, unless `swap-last.json` holds an upgrade newer than the marker that ended `ok`,
    `rolled_back` or `rolled_forward` (killed after its record: the marker just goes). A placement that failed and put
    the files back leaves no marker. A release that failed here is never recorded as installed (so never stable)
    while it runs, until a later `zm daemon upgrade` to that exact build ends `ok`: the next tick then drops it from
    the failed list (the way back). The chosen release is installed only when: the manifest
    verifies with a key in config `update_keys` (several during a rotation; none configured refuses) and names the
    tag's version, the platform's tarball matches its signed sum, the signed `created` is newer than what this home
    installed and `--min-age` old, and the running build is behind the release's commit on GitHub, asked of each update repo, or behind the
    commit its `Source:` line names when the release is a public copy (a newer or divergent hand install, or one no
    repo can place, is never replaced). **Update repos**: the repositories `--release github` reads, in order, are the
    public repo then the repository the binary was built from (when another), unless this machine's own
    `$ZM_HOME/config.json` holds `update_repos` (that exact case), a list of 1 to 8 distinct GitHub `owner/name`s, read by zm itself and
    set only by editing that file on the machine, by hand or with `scripts/releaseorder/release-order.py` run there
    (directly or through `zm exec`); no zm route, flag or subcommand of its own writes it. The first repo that answers with channel
    releases is taken; a 404 or a repo with none passes to the next, any other failure stops the check there. A key
    that is there but empty or malformed fails each release check that gets past the holds and an unfinished swap's
    report, naming it, and never falls back
    to the default. The order never changes the keys a release must verify with; on a machine's own order a GitHub
    compare or commit answer must name the commits asked (the compared base, the full running commit as merge base),
    else the check holds, and a release whose tag is yanked (or a draft) on any other listed or built-in repo it can
    read is not taken (that read failing other than 404 stops the check; a chosen repo left with no release holds,
    it does not pass to the next). Yanks, failed-here records and vouches are per tag (version): a public copy and the
    release it was copied from have different tags, so each repository's tag is yanked on its own.
    A check on a machine's own order ends its reason with `release sources: <order> (config update_repos)`, and
    `zm daemon update status` on the machine ends with the order it reads. Only regular program files under the tarball's own prefix are
    installed, through the same staged upgrade, hold file, quiet window and `update.lock` as above. The **hold
    switch**: `zm daemon update hold [reason]` writes `$ZM_HOME/update.hold` (always honoured, beside any
    `--hold-file`); `resume` removes it; a hold pins the build it runs and never blocks a hand upgrade.
    `zm daemon update status` lists each machine's `zm` block (section 8, "Machine numbers"). A machine needs read
    access to the repository's releases (its own `gh` login); when gh is missing, not logged in, or refused (while
    the repository is private GitHub answers an account without read access with a 404), the check fails with one
    plain sentence saying which, never the raw HTTP error (the listing, the first call; past it access is proven and gh's words stay). A `--release github` tick on a machine with `update_keys` also pins the running build's `release.ChannelKey` there, so a key rotation reaches machines nobody touches. `--dir` defaults to the directory of the running `zm`,
    symlinks resolved; a directory this user cannot write is refused before anything runs, in one sentence naming
    `--dir`. **Schedule**: `zm daemon update enable [--min-age 6h]` pins the channel's public key
    (`release.ChannelKey`, the one the farm's update setup script pins) in config `update_keys` when it is not
    there, and installs this user's daily job, never root: on Linux `~/.config/systemd/user/zm-update.service` and
    `zm-update.timer` (03:MM local time, the minute from the machine name, `Persistent=true`; enabled with
    `systemctl --user`), on macOS `~/Library/LaunchAgents/dev.zm.update.plist` (`StartCalendarInterval` 03:MM,
    `AbandonProcessGroup`, output in `$ZM_HOME/update.log`; `launchctl bootout` then `bootstrap`). The job runs
    `zm daemon update --release github --dir <install dir> --min-age 6h --no-canary` with the enabling shell's PATH
    and `ZM_HOME`: the farm's ring age for every box but the canary and the authority, and no canary gate, since a
    lent machine sees no other machine's numbers (an area guest sends none); the signature, never-older, failed-here
    and yank rules all hold. Each file starts with zm's mark: enable rewrites its own (never a second job), and a
    unit at those names without the mark (the farm links its own there) or any other user unit or agent running
    `zm daemon update` makes enable and disable change nothing, naming that file. `zm daemon update disable` removes
    zm's job (and names, never touches, another updater beside it). On Linux without lingering the line adds that the
    timer runs only while the user is logged in (`sudo loginctl enable-linger`). An upgrade whose daemon runs through a
    symlink into the install directory restarts it (`runsFrom`), as one run from the directory itself. `zm pool host` and `zm host share` run enable once the machine is lent (after the pool lists its boxes; when some
    sizes failed, once any is bound),
    saying so in one line (stderr under `--json`); `--no-auto-update` skips it; a failure there is one line and never
    undoes the lend. See the upgrade runbook.
  - Each one is recorded by the daemon that runs after it (`POST /v0/daemon/swaps`, the caller as `by`): a warn line
    in its log, a `dev.zm.daemon.swapped` event, `swap-last.json`, and health's `last_swap`; with no daemon that
    has the route, the CLI appends the line to `daemon.log`. `dev.zm.daemon.stopping` carries `{by, what, force}`.
  - **Self-check.** After start the daemon probes its own served set through its socket handler, checks every
    database against its build, and, when `served.json` names another build, lists what that build served and this
    one does not. It never stops for it: problems go to health's `selfcheck` (local callers), to `zm status`
    (`PROBLEM:` lines, exit 1), to the log at error level and to a `dev.zm.daemon.selfcheck` event. It checks again
    every minute while problems remain, else every 10 minutes; only a clean check rewrites `served.json` (delete it
    to forget a set on purpose).
- `potluck/kit/store.Store` owns every handle: registry.db (4 connections) and each project.db (2 connections),
  all kept open, all WAL, `synchronous=NORMAL`, `busy_timeout` 5 s, `BEGIN IMMEDIATE`, each connection with its
  own prepared-statement cache (modules write plain SQL). Each module lists its migrations per kind
  (`[]store.Migration`: registry, project, replica) and its `New` adds them (`core.Deps.AddMigrations`), so a
  daemon has the tables of the modules its wiring builds; `store.Migrate` applies them to the registry and to
  every project.db (a replica's copy as it opens, `Store.OpenReplica`), each once, in name order.
- **No cross-file transactions.** A change that touches both files commits its authoritative row first and
  treats the other as derived: a lease (registry) is authoritative for who holds an item; the item's
  `status` in project.db follows it and is repaired by the lease ticker. Events are appended after commit.
- Times on the wire are RFC 3339 UTC; in SQLite, Unix milliseconds.
- Ids: projects, identities, tokens, mail, comments and events are ULIDs from `potluck/kit/ids` (monotonic per
  daemon with **random** increments). Local work items are `local:<6 lowercase base32 chars>`, random, unique
  per project (never sequential: a count would leak).
- `zm project export <slug> --dir D` writes a consistent copy of project.db and project.toml. Leases,
  sessions and grants live in registry.db and are not exported.
- Errors are `{"error": {"code", "message", "hint"?, "data"?, "retry_after"?}}`: `bad_request` 400, `unauthenticated` 401,
  `forbidden` 403, `not_found` 404, `conflict` 409, `not_implemented` 501, `unavailable` 503, `internal` 500.
  `retry_after` (seconds) is a tracker's rate limit, passed on by its adapter; the mirror's sync waits at least
  that long before it tries again.
  An unexpected Go error is reported as `internal error` (details only in the daemon log).
- **Protocol negotiation.** Every request carries `Potluck-Protocol: potluck/1`; every response too. Either
  side refuses a different major; `Zm-Protocol` and `zm/v0` (the names before the Potluck rename) are read as
  the same. `/v0/health` and `/v0/whoami` report `protocol`, `version` and `commit`.
- **Secrets** (`potluck/kit/secrets`, the one way the daemon, the CLI and in-tree adapters read a key, token or
  password). A reference names where a secret lives: `file:PATH` (absolute; a regular file of at most 64 KiB that
  only its owner may read or write, so a file with any group or other permission bit is refused with the hint
  `chmod 600 PATH`; read on every use, so a rotated secret is picked up at once), `keychain:SERVICE[:ACCOUNT]`
  (a macOS generic-password item via `/usr/bin/security`; for interactive hosts, since the daemon must run
  headless and with the keychain locked) and `env:NAME`. Surrounding whitespace is trimmed. Errors never carry
  the secret; a missing one is `unavailable` (matching `secrets.ErrNotFound`), or `bad_request` when the file was
  named in the request (`--password-file`, `--token-file`, `--secret-file`, `token_file`, `key_file`). Values
  print and encode as `[secret]`. A new backend (a third-party store such as 1Password `op`) is a driver added to
  the reader, not a new read path. The config keys stay as they are (`key_file`, `keychain`, `token_file`, ...):
  each names a reference of one scheme.
  **Classes** (docs/runbooks/secret-leak.md, section 3, lists each key and login zm keeps and how to rotate it). **P**,
  person-present: needs a touch on hardware, never a file. **S**, service-isolated: a key with wide power (the
  runner GitHub App key, the release signing key) lives where no agent session runs and is reached only by typed
  requests inside limits the owner fixed, never by signing caller bytes. **O**, owner-user at rest: a 0600 file or
  a `keychain:` item, which every process of that OS user can read, agents included (`/usr/bin/security` answers
  any of them); O protects against disk theft, backups and slips, not local agents, so an O key is scoped,
  rotatable and cheap to lose. **E**, in a box: `$POTLUCK_SECRETS` on a memory filesystem, through the door only.
  The approver key is class O today (a software key in a file, section 16 "Key approvers"); a touch-bound key in
  the signed Mezze app build makes it P. No key file is ever in a home backup set (17.9).

## 3. Projects and areas

- A **project** is the top-level boundary (e.g. `harbor`, `zm`, `personal`): a stable ULID plus a
  renameable slug. Storage keys by ULID; humans see slugs. Everything (mail, work, leases, sessions, events)
  belongs to exactly one project. **`none`** is the project of things outside every project (a shell started
  outside any repo, daemon events); only the machine owner sees it.
- An **area** is a named scope inside a project (`harbor/solver`). Objects get an area at creation (`""` =
  project-level). Area match rules: `paths`, `remotes` (repos), `work` (id globs, `*` matches anything) and
  `labels` (a tracker item carrying any of these labels; checked after `work`, so an area can follow a
  tracker's labels when its ids carry no area prefix). Whoever can label items in the tracker can move them
  into or out of an area. A `paths` rule may be a glob (`~/code/harbor*`, `*` within one path element).
- `projects/<ulid>/project.toml` (written by the daemon; `zm project set`, `zm area add`, `zm tracker
  add|rm` and `zm area set <project>/<area>` apply at once, with no restart; hand edits need a restart, and
  until then a live change is refused with a conflict naming the restart rather than discarding the edit). `zm project show
  harbor/solver` shows one area as the caller sees it: its rules and the trackers serving it that the caller
  sees, the same set `zm work sources` lists (a project-level tracker counts when the area takes labels or a
  work glob names its prefix):

```toml
id = "01J9Z3K0QX7Y8AZ5V2N4B6C8D0"
slug = "harbor"
name = "Harbor"
paths = ["~/code/harbor"]
remotes = ["github.com/example-org/harbor"]

[[areas]]
name = "solver"
paths = ["~/code/harbor/services/solver"]
work = ["bd:solver-*"]
labels = ["solver"]        # tracker items labelled solver, whatever their id

[[trackers]]
adapter = "native"         # the project's native issues; <name> runs zm-work-<name>
prefix = "bd"
```

- `<repo>/.zm/project.toml` (written by `zm project use <slug> [--area a]`) names a project **by slug**
  (slugs are portable between machines; ULIDs are per machine): `project = "harbor"`, optional `area`.
- Area of a new object: an explicit `area` field, else the area of its `ref`/work item, else (mail to a session)
  the recipient session's area, else the resolved area (section 5). A caller who sees exactly one area of a project
  (a guest) writes there by default.

## 4. Who may do what

- **Identity**: `(ulid, name, kind human|agent|machine, optional tailscale login)`. The machine owner's
  identity (config human, flagged local) is created at first start. Others are added by `zm identity add`,
  `zm grant` or `zm token mint` (which create a missing identity as `human`). An identity's mailbox
  address is `human:<name>` (`machine:<name>` for machines).
- **Grant tuple**: `(identity ulid, relation, object)` in `registry.grants`, relation `owner | member | guest`,
  object `project:<ulid>` or `area:<ulid>/<name>`. Default deny.
- **Actions** (`authz.Action`) and what each relation allows:

| relation | read | write | operate | admin |
| --- | --- | --- | --- | --- |
| owner | yes | yes | yes | yes |
| member | yes | yes | yes | no |
| guest | yes | yes | no | no |

  `read`: see it (lists, gets, events, autocomplete). `write`: send mail, create/comment/claim work.
  `operate`: spawn, stop, attach, type into sessions, force-release leases. `admin`: grants, tokens, areas,
  project settings. A project relation covers the project-level objects and every area; an area relation
  covers only that area's objects. **Admin from the network is not machine admin**: tracker adapters and
  their config (binaries, `-C` dirs, databases), run-target bindings (`[[run_targets]]`, section 20) and repo `paths`/`remotes` (project or area; they feed the
  `--sandbox` policy) are what the daemon runs and reads on this machine, so only the machine's owner on its
  socket without a token may set them (`PATCH /v0/projects/{p}` `repos`/`trackers`, area `paths`/`remotes`);
  an owner token or tailnet owner gets `forbidden` and may still change slug, name and areas' `work` globs. `authz.Check` is plain Go over these tuples; the data stays a plain table
  so the checker can be swapped without a migration.
- **Tokens**: `zm token mint --as NAME --role R [--project P [--area A]] [--ttl D]` adds the grant (unless
  `--no-grant`) and returns the secret once. A token's effective scope is the identity's **current** tuples
  narrowed to its project/area and capped at its role, so revoking the grant disarms the token too.
  `zm token ls`, `zm token revoke <id>`. Stored: sha256 only. Unknown, expired and revoked tokens get
  the same `unauthenticated` answer.
- **Answer-only tokens**: `zm token mint --answer-only -P P --node DEVICE --ttl D` mints a token with the
  route limit `answer` (the kit's limited token): it reaches only the routes that admit that limit
  (`GET /v0/whoami`, `GET /v0/asks`, `GET /v0/asks/{id}`, `GET /v0/decisions`, `POST /v0/asks/{id}/choose`,
  `GET /v0/receipts/verify`), and every other route, an unmatched path included, answers it `403` with
  data `{"reason":"token-limit"}` before anything is looked up. Public routes run for it with no
  subject. Inside an admitted route it acts with its scope, its identity's current tuples on the
  project, capped at owner. Minting is local, for the caller's own identity only (`as` is refused), which
  must already own the whole project; it adds no grant and no identity. Project (no area), device and TTL
  are required, the TTL is at most `limited_token_max_ttl_seconds` (default 30 days), and a call to
  `POST /v0/tokens` with a limit from inside a registered agent session is refused by the socket's
  origin check, failing closed (section 16). The device is a tailnet peer named at mint (its MagicDNS
  name): the token authenticates only over the tailnet listener, from exactly that node's stable ID and
  node key, never from this host, the socket or the tunnel; any other caller gets `unauthenticated`. Mint
  refuses this daemon's own node, a tagged node, a node named as a daemon, a node that relays other
  hosts' traffic and a node shared in from another tailnet, and answers the device's name, login and OS
  for the person to check. Revoking it, its expiry, or its identity's whole-project grant falling below
  owner answers `unauthenticated` at the next call. `zm token ls` and `GET /v0/tokens` show its limit and
  device. A token without a limit is unchanged.
- Example: ana and bob own `acme`; `zm token mint --as sam --role guest -P acme/solver`
  lets sam see only solver work items, solver repos and mail in solver: no other project, area, session
  or name.

## 5. Which project (resolution)

A write needs one project. Rules, first match wins (`zm project why` prints every step and which fired):

1. `--project` (`Zm-Project` header, from flag), accepts `slug`, `ULID` or `slug/area`
2. `$ZM_PROJECT` (same header, `Zm-Project-From: env`)
3. `cwd-file`: `.zm/project.toml` in the cwd or any parent up to the git root
4. `cwd-path`: the longest configured `paths` rule containing the cwd (area rules win ties)
5. `cwd-remote`: the cwd's repo `origin` (read from `.git/config`, worktrees followed) matches `remotes`
6. `session`: the calling session's registered project (so a session that `cd`s into another project's repo
   files there, not into its own)
7. `default_project` in config.json (never set by installation)
8. otherwise **refused** (`bad_request` with a hint). Never filed into a default silently. `zm project why`
   then exits 1.

- A rule that names a project explicitly (1, 2, 3, 7) and does not resolve stops resolution (`not_found`);
  it never falls through to a later rule.
- Only projects the caller can see are candidates. Remote callers' cwd is ignored.
- **Reads never need a resolved project**: lists (`inbox`, `work ls`, `sessions`, `leases`, `top`) return the
  union of every project the caller sees, grouped by project; `--project` (or `?project=`) narrows them.
  `$ZM_PROJECT` does not narrow lists, nor a mail id (`ack`, `reply`, `thread`: the id names its project).
- **Hooks degrade silently**: `session register` with no project registers in `none`; `notice` prints
  nothing; neither ever fails the agent's prompt.
- Sessions zm spawns never get `ZM_PROJECT` in their environment (it would beat the cwd rules).

## 6. Leaks (enforced, tested)

Every route, list, autocomplete source, error and event obeys:

- **Not found = not visible.** An object outside the caller's scope is answered with exactly
  `api.NotFound(kind, id)`, byte-identical to a missing id (`authz.Subject.Check` does this). `forbidden` is
  only for objects the caller can already see.
- **Scoped queries are structural.** Project handles come from `store.Project(scope, action, id)` /
  `store.Projects(scope, action)`; every row query adds `scope.SQL(projectCol, areaCol, action)` (registry
  tables) or `scope.AreaSQL(project, areaCol, action)` (project.db tables). Service methods take the caller's
  `*authz.Subject`; daemon-internal calls pass `d.System()`.
- **Hidden addresses become `someone`**: lease holders, `acked_by`, `owner`, `via`, conflict `error.data`
  (`d.Redact`). Events list such addresses in `Mentions`; the bus redacts per subscriber.
- **Hidden links**: a link to an item the caller cannot read is `{"rel": ..., "hidden": true}`, one per rel,
  so the count does not leak.
- **No counters on the wire**: ids are ULIDs with random increments; event cursors are event ids, never a
  global sequence; local work ids are random.
- **Golden leak test**: `internal/wiring/fulltest/leak_test.go` seeds two projects and two areas through raw JSON on
  the HTTP contract (never module-owned client methods), then calls **every registered route** as a guest of
  `harbor/solver` over three transports (tailnet with WhoIs, an unmarked network listener, the socket with the
  token) with every hidden id, slug and key, and asserts that no response contains anything from outside
  solver and that hidden ids answer exactly like missing ones. New routes join it automatically.
  `leak_writes_test.go` adds: a table of **body-bearing** writes that name a hidden recipient, ref, area,
  project or link target (mail, work, link, leases, grants, areas), each answering exactly like its missing
  twin; the **live** SSE stream as the guest (hidden-project, project-level, hidden-area and machine events
  never arrive; hidden session addresses arrive as `someone`); and every route over an unmarked TCP listener
  without credentials answering 401 (health 200). All must stay green.

## 7. Addresses

```
address   = session | machine | human | work | "all" | "owners"
session   = name "/" session8          ; mini/4bb7545a   one agent session on one machine
machine   = "machine:" name | name     ; machine:mini    that machine's group mailbox (per project)
human     = "human:" name              ; human:ana       an identity's mailbox
work      = source ":" ref             ; local:k3x9qa, bd:solver-3x
"owners"                               ; the project's owner group mailbox (escalations)
"all"                                  ; every session in the project (announcements)
name      = [a-z0-9][a-z0-9._-]{0,62}
session8  = [a-z0-9]{8}
named     = name "/" sname | sname      ; mini/auth-fix  a live session by its name (resolved at once)
sname     = name, not a session8, not none, all or owners, gc, admission, register or hook,
                                       ; no blocked word (a session address is never read as a name)
source    = [a-z][a-z0-9-]{0,31}       ; not "human" or "machine"
ref       = non-empty, no whitespace (case kept)
```

- Session address = `<machine>/<session8>`; `session8` = the first 8 alphanumerics of the runtime's own
  session id (Claude Code / Codex UUID), else random. Leases and locks are owned by the exact session
  address, never the machine.
- Inside an agent session zm did not start, the CLI answers as that session automatically
  (`client.DefaultAs`: `$ZM_AS`, else `$ZM_SESSION`, else the runtime's session id variable).
  A token caller uses its token identity instead of either session fallback; `--as` overrides the defaults.

## 8. Sessions

One agent in a PTY zm owns through a per-session holder process (`managed: true`, "Daemon restarts" below), or one agent in someone's own terminal registered by
its hooks (`managed: false`; no attach or input). Stored in registry.db. **Every session has a project**
(`none` included) and an optional area.

For new interactive Codex launches with recognized options and no explicit positional prompt,
`run --prompt` supplies Codex's initial CLI prompt rather than typing into a startup screen.
The generated prompt is omitted from stored and returned session commands and session events;
it is visible in the child's OS argv. Custom commands, unknown options, subcommands and explicit
command prompts retain the existing terminal-input behavior. One-shot task input remains on stdin.

**One-shot tasks.** `task_start` starts Claude or Codex in a holder without a PTY or controlling terminal. The prompt goes to stdin; stdout and stderr are captured separately in private, durable files capped at 32 MiB each; structured events come only from stdout. Read-only output viewing remains available. An overflow or incomplete drain fails the task. The structured response reports `finished` as `done` or `not_done`; an absent report is `unreported` and counts as not done. This is the child's word, not proof that the work was completed; the process outcome is separate. Worktree defaults on and its branch remains for the parent; `work` links the result by comment without claiming a lease. Only the parent or socket owner can inspect or cancel a task; other callers see not found. Unix peer ancestry tracks registered parents, including self-registration; ordinary spawns count toward task depth. These limits guard against mistakes and runaway agents, not a same-user process deliberately escaping its ancestry: every socket caller is an owner (section 1). At most three tasks run per parent (tasks the team authority placed here for an approved ask action count apart from the parent's own, up to three each) and depth is at most two. A parent with a nonzero exit, signal or explicit cancellation cancels its task children, never its ordinary sessions; text-based outcome detectors do not drive lifecycle actions; a normally ended parent leaves tasks running. Admission refusals remain `unavailable`. `client_request_id` is idempotent per parent; changed requests conflict; one starting `rt-` is refused (the authority's ops use that space). `task_status` waits at most 45 seconds. Five minutes without a completed agent or tool step stops a child, measured afresh after daemon restart; runtime heartbeats, tool output and diagnostics do not count as progress; an overall deadline defaults to four hours, configurable with `task_deadline_seconds` or per task with `timeout_s`. Task input, resize and writable attaches are refused. Each task has a recorded process group, start time and home identity; ending a task stops its group only when these match, and startup reconciles ended groups without signalling a reused PID or a group from a copied home. A failed holder dial leaves a session unreachable unless an exit file or a dead or reused recorded process proves it ended. A verified home and process group allow stopping an unreachable session; if a task cannot be safely signalled and its child is not proven gone, it remains running and records the child PID and that it may still be running and needs manual attention; after exit is proven it ends not done and its completion evidence and mail retain that warning. Linux also uses parent-death signalling. An explicit cancellation or process signal takes precedence over runtime classification; the first terminal runtime event otherwise decides process success or failure; later events can only lower confidence. A zero exit without a terminal event fails. Results contain `finished`, `text`, typed `outcome`, token `usage` and `branch`; task text is untrusted. zm sends the parent a completion mail and comments on the linked work item, retrying known failures up to three times and not replaying ambiguous writes. A completion mail the mail send limits refuse (`rate_limited` with `retry_after`, SPEC 9 "Send limits") waits for `retry_after` without spending an attempt, up to an hour from the first refusal; a refusal whose wait would end past that fails the mail (`mail_delivery` `failed`, the reason in `mail_error`), and the result stays readable in `task_status` and the work comment. A `repeat` refusal means the same mail is already there and counts as delivered. Issue redaction scrubs all output streams, transcripts, scrollback, results and requests only after the task ends. A redaction holds every task of the project running when it happens; its own retries (as each held task ends) keep every hold it placed but hold a task started after its first hold only when that task's own copies carry the text. A scrub carries its redaction's time (`core.Scrub.At`): a first attempt now, a pending-scrub retry the first attempt's, a redaction re-applied after an import now again; a task started after that time is held only when its own copies carry the text. A separate whole-buffer escape-normalizing check verifies the scrub; on uncertainty, output files are dropped and result text is withheld while trusted outcome and finished fields remain. A durable hold covers every running task in the project while scrubbing waits for it to end: an agent may repeat text it already received even when it has not emitted it yet. The hold survives a daemon restart; the completed task is then scrubbed and released. If a task ends without a reachable holder, its named output files are unlinked and the hold remains pending until process and holder checks prove the writer has gone; no signal is sent without identity proof. After 30 seconds, `zm session gc --discard-redaction <id>` explicitly seals the output paths and releases an ended task's hold after scrubbing its retained data; the output paths remain blocked. If a holder may still write in a former home, discard refuses until that holder is proven gone. Tasks cannot move between projects. Status exposes `redaction_pending` and withholds text until scrubbing is proved; after a bounded wait a completion notice may also omit the text. Ended, unaffected tasks remain available. `zm session gc` lists ended task transcripts and worktrees; explicit transcript cleanup removes the spool and scrollback, while worktree cleanup retains the branch.

**Cloud tasks** (Claude and ChatGPT, opt-in per project). `task_start` with `where: "cloud"` (CLI: `zm tasks start --where cloud`) runs a claude or codex task on the provider's cloud computer instead of here, through this machine's own CLI login (a plan login: zm holds no API key): `claude [--settings '{"remote":{"defaultEnvironmentId":"<id>"}}'] --cloud=<prompt> --permission-mode auto` (the id goes through `--settings`, for an Anthropic-hosted `env_` id and a self-hosted `ccpool_` id alike: `--environment` takes only the latter; `default` sends no setting, so the account's default environment; `auto` so an unattended session does not stop on a prompt; the CLI runs on a terminal, which it wants) or `codex cloud exec --env <id> [--branch=<the checkout's branch>] -- <prompt>` (the prompt never reads as a flag), for the task's `cwd`, whose repo the provider clones (codex runs from a private directory: its `error.log` stays out of the checkout). Cloud tokens count against the same plan limits as local ones; the machine is what is free. **Opt-in:** a project sends a provider code only when its project.toml names that runtime's environment in `[cloud]` (`claude = "<id>"` or `"default"`, the account's default environment; `codex = "<id>"`), set by this machine's owner with `zm project cloud [--claude <id>] [--codex <id>]` (`PUT /v0/tasks/cloud`, live; `""` takes it back; a session cannot: an agent never opts its own project in). Without it, or when the task's repo (its git root, else `cwd`) is not that project's checkout by its `paths` or `remotes` (a `--project`, the session's project or a repo's own `.zm/project.toml` names a project, not the repo that goes out), the start is refused before any provider call; a project joined from another machine cannot opt in here. The task's child is the zm binary as a watcher under the usual holder, so parent, depth and concurrency limits, the overall deadline, cancel, the completion mail and the work comment are a local task's; the task's agent is `claude-cloud` or `codex-cloud`, it has no worktree, `model`, `machine`, `front`, `continue_from` or `on_limit`, and no five-minute no-output stop. The watcher records the provider's task id and URL (status `cloud`). Codex has no webhook: zm asks `codex cloud status` (and `codex cloud list --json`) after 1, 2, 4 and 8 minutes, then every 10, and ends the task with its diff (`codex cloud apply <id>` brings it here) once ready, or failed. Claude Code has no command that reports a cloud session without attaching, so a Claude task ends once the session exists, with its link and `claude --teleport <id>`; the id is read from what the launch prints (the `View:` link and `Resume with: claude --teleport` line; a `Session ID:` line too), and a launch that prints none fails the task with the output's tail. `finished` is `unreported`: zm never reads the cloud agent's own word, and the work waits on the provider's branch or diff and the normal review gate. Cancelling stops zm's watch, not the provider's task. Cloud agents cannot reach zm (no mail, claims or MCP): the prompt must be self-contained, and goes as given (at most 100 KiB). Grok is out (its cloud agent has no CLI).

**Task descendant stop.** A task stop follows the requested signal plan for both its recorded group and its observed descendants: soft signals allow cleanup; KILL occurs only when requested or at the final forced step. It checks the recorded home and process start identities, and never walks into or signals this daemon or a holder subtree. It repeats the descendant scan within the stop bound, including for an unreachable holder; before the final KILL it suspends verified task processes to capture a stable tree. A scan or signal failure never prevents an otherwise verified group signal. Known survivors or incomplete inspection are reported as `may still be running`, with no successful high-confidence outcome. An unreachable child is not recorded ended without evidence it exited. The scan cannot identify a process that detached and was reparented before observation; that process may still run and needs manual inspection. Task redaction covers zm's captured output and retained data, not a surviving tool's worktree files or new calls to zm; once the holder has closed its output pipes, the tool cannot write to those captured streams.

**Claude task context.** Claude one-shot tasks load native CLAUDE.md/imports from the resolved repository-root-to-worktree-cwd ancestry through additional directories, while repository settings stay disabled. At launch, a bounded regular-file snapshot of ancestor `.claude/skills` becomes a generated skills-only `zm-task-repo` plugin; source skill/resources remain byte-identical and frozen for that task. Names are explicit `/zm-task-repo:<name>`; duplicate/ambiguous names, unsafe files, unsupported frontmatter or exceeded budgets refuse launch. Nonempty skills require verifiable Claude Code 2.1.318+ and disabled skill shell preprocessing in addition to disabled hooks and strict MCP; stored authentication remains unchanged. Capability probing runs only inside a managed task child after durable holder/group recording; it has bounded output/time, preserves prompt stdin and needs no warmed version cache. A failed guard exits with plain diagnostic stderr, not a synthesized provider result. Limits and accepted scalar/text-block frontmatter are in [the task-context contract](internal/session/taskcontext/README.md). Snapshots share session scratch lifetime, survive daemon adoption and are rebuilt only for new tasks/continuations. Legacy commands, existing repository plugins, rich YAML/custom agents and full native discovery/override parity are excluded; this is not a claim of full native project customization parity. **User-level skills.** Neither agent's task loads the OS user's `~/.claude/skills` or `~/.codex/skills` (checked by `TestTaskAgentsIgnoreUserSkills`): Claude via `--setting-sources ""`; a Codex task runs with its own `CODEX_HOME` under scratch that links only auth, config, sessions and the model cache.

**Task handoff.** `on_limit: next_agent` selects a ready agent not already tried when a task ends with a quota or auth outcome; it is off by default. A source has at most one started continuation; refused starts and exhausted agents are reported to the parent and can be retried. Structured runtime error events may classify limits from runtime-specific messages; plain output never does. A Claude task's bare 429 (`api_error_status` 429 with no `rate_limit_event` whose window is `rejected` and not yet reset, and no usage-limit text in its result) is a plain failure, not a limit, so it never hands off. Unrecorded quota formats carry low confidence. A continuation is a sibling with the same parent and depth and uses normal admission, cancellation and concurrency limits. Its project and area stay those of the source. Before handoff, dirty source worktrees are committed locally as an automatic unreviewed work-in-progress snapshot, including non-ignored untracked files; a failed snapshot refuses the handoff. Its worktree starts at the source branch. The original request and newest whole transcript messages are selected with a 16,000-byte UTF-8 text budget (the original is never shortened); omitted item IDs can be read with socket-only `task_read` in bounded pages. Historical text is untrusted. Only the source parent, socket owner, or a continuation in its chain can read it. A redaction hold also applies to history reads. Handoff and history reads require the holder to record a successful group stop, a matching recorded home and process-group identity, and proof that the group no longer exists; otherwise they refuse with manual-inspection guidance. Withheld history is never passed on; a separately verified original request is delivered with a withholding notice. If verification was lost on restart, the continuation fails closed and reports why. `continue_from` explicitly requests a continuation of an ended quota or auth task; cancelling the source cancels its live continuation or pending handoff.

**Structured sessions** (`mode: "pipe"`). `zm run --pipe <agent>` (`POST /v0/sessions {"mode": "pipe"}`) starts the agent with no terminal: its holder (protocol `zm-holder/1.2`, `Spec.Pipe`) runs a **zm-agent/1 adapter** on pipes. The adapter's stdin takes operations, one JSON object per line: `start` (first, once: `cwd`, `model`, `resume`, `approvals`, `mcp`, `allowed_tools`, `permission_mode`), `turn`, `steer`, `interrupt`, `answer`, `stop`; its stdout is the session's **event stream**, one event per line, `seq` from 1. The holder hands the daemon the stdin's write end in place of a PTY master and spools stdout whole (`sessions/<id>.events`, 64 MiB, then the stream goes on live and reads say `truncated`), so the stream outlives daemon restarts like a PTY's; stderr goes to `sessions/<id>.pipe-err` (4 MiB). Built-in adapters, run as `zm __zm-agent <driver> -- <the agent's command>`: `claude` (`--input-format stream-json --output-format stream-json`, permission prompts over stdio) and `codex` (`codex app-server`). Another agent needs a `zm-agent-<agent>` on PATH that answers `describe` with `{"proto": "zm-agent/1", "adapter", "capabilities"}`. **Capabilities, never names**: what a session can do is the capabilities its adapter declared (`steer`, `interrupt`, `approvals`, `questions`, `resume`, `images`, `partial`, `usage`, `plan`, `tool_output`, `model`, `mcp`, `tools`), recorded on the session; an operation the adapter cannot do is `409` before anything is sent. `resume` (capability `resume`) goes on from the runtime's own id, the `runtime_id` a structured session reports. `approvals`: `ask` (the runtime asks before acting; approval events), `never` (what would ask is declined at once, so a headless session never waits on a person), unset (the runtime's own settings). `mcp` (capability `mcp`): up to 16 remote MCP servers `{name, url, headers}` the runtime starts with for this session only (name `[A-Za-z0-9_-]`, url http or https, header names distinct HTTP tokens, values printable ASCII, 64 KiB in all); the built-in adapters hand them over the runtime's stdin (Claude `mcp_set_servers`, Codex thread config), never argv, a file or an event, since header values and URLs may be credentials: a recording and a runtime error quoting them get `[redacted]`; a server that fails to connect leaves the session running without it. **Tool grant** (capability `tools`): `allowed_tools` (up to 64 Claude tool specs such as `Bash`, `Read`, `Bash(gh pr view:*)`) and `permission_mode` (`default` or `acceptEdits`) are what the runtime runs without asking, so a session with `approvals: never` can still act (an unattended automation; without a grant only what the runtime's own settings allow runs). Claude takes them as `--allowedTools` and `--permission-mode`; Codex has no per-tool list, so a grant that writes (`Bash`, `Edit`, `Write`, `MultiEdit`, `NotebookEdit` or `acceptEdits`) is its `workspace-write` sandbox, with network when `Bash` is granted, and `ask` becomes `on-request`; a read-only grant changes nothing there. The adapter refuses a grant it did not declare, and the authority and the target each check a remote one. A grant widens what the runtime may do on its machine, never the session's zm scope. **Safe outputs**: `safe_outputs` (`{"bead-write": n, "zm-ask": n, "mail-send": n}`, each 0 to 1000) caps the writes the session makes through zm: bead writes (every issue write but a claim, `sql` and `setup`, including comments, labels, deps and memories; work item create, comment, status and link), asks, and mail sends and replies. With any cap set, a kind it leaves out is 0, and the session starts no sessions or tasks (`403`). The start's answer echoes the caps (`safe_outputs`, every kind); a start on another machine whose answer has none (an older zm on the way) is stopped and refused `409`. The daemon the session runs on counts each write whose `Zm-As` is the session before routing or forwarding it, refuses the one past its cap `403` and gives back the count of a write zm refused `4xx` (not a `5xx`: a forwarded write may have landed; a retried write may count twice). Since `Zm-As` on the socket is asserted (SPEC 1), this keeps an unattended run within what its automation allows, not a process that hides who it is; writes outside zm (gh, hub) are bounded by the tool grant. PTY stays the default; a task has no pipe mode; a structured session may be shared (SPEC 17.10 "Structured"). Terminal input, resize, a writable attach, nudges and shared prompts are refused for it; a read-only attach shows the raw stream.

**Pipe handoffs.** A structured session started with `continue_from: <pipe session>` (a client switching agents, say after a usage limit) begins with zm's handoff as its first turn, so the client never writes its own: the chain's whole user and assistant messages (each continued session's handoff turn counts as just the prompt sent with it), newest first within the 16,000-byte task budget less the prompt, as untrusted data with omitted ranges, then the prompt as the user's new message. Item ids are `<session>:<n>`; `task_read` with the session id reads one, for the socket owner or a session that continued from it along the chain. It needs operate on the source, keeps the source's project and area, does not share an unshared source, and takes history only from chain sessions the caller may operate; redaction holds apply. This machine only: the source's stream is here. zm never ends the source; the client does.

Every event is `{seq, at, type, turn?, item?, status?, ...}`, and a whole state of what it names (an upsert by its id), except `message.delta` and `output`, which add to one:

| type | key | fields |
| --- | --- | --- |
| `session` | the session | `status` `starting\|ready\|running\|waiting\|stopped\|error`, `runtime_id` (Claude session, Codex thread), `model`, `cwd`, `version`, `capabilities` (on starting), `error` |
| `turn` | `turn` | `status` `running`, then exactly one of `completed\|interrupted\|cancelled\|failed` (with `error {kind: quota\|auth\|failed\|infra, message}`), `usage`; the last event of its turn |
| `message` | `item` | `role` `user\|assistant`, `text` (whole so far), `status` `streaming\|completed` |
| `message.delta` | `item` | `text` added to a streaming message (capability `partial`) |
| `reasoning` | `item` | `text` (the model's visible summary), `status` |
| `tool` | `item` | `kind` `command\|file_change\|mcp\|web\|task\|other`, `name`, `title` (one line), `input` (8 KiB), `output` (last 32 KiB), `exit_code`, `parent` (a subagent's tool), `status` `running\|completed\|failed\|declined` |
| `output` | `item` | `text` added to a running tool's output (16 KiB a chunk; capability `tool_output`) |
| `approval` | `request` | `kind` `command\|file_change\|tool\|permissions\|question`, `title`, `detail`, `options`, `status` `pending\|answered\|cancelled`, `decision`; never pending past its turn's end |
| `usage` | `turn` | `usage {input_tokens, output_tokens, cached_input_tokens, reasoning_tokens, context_tokens, context_window, cost_usd}`: the session's totals so far |
| `plan` | `turn` | `steps [{text, status}]` |
| `op` | `op` | `ok`, `error`: exactly one per operation id |
| `notice` | - | `level`, `text` or `error`: a runtime warning (a retry, a usage limit) |

The session's state follows the stream: `ready` is `idle`; `running` is `running` (activity `Working`, or the running tool's kind: `Running a command`, `Editing files`, ...); a pending approval is `needs_human` with the note `asks to run a command` (and the like). Notes and activity never carry a command, a path or the agent's words. The runtime's own hooks do not set its state; the idle timer leaves it alone; its outcome comes from the adapter's exit, never from text in the stream. Reads (`GET /v0/sessions/{id}/events`, JSON pages or SSE) and operations (`/turn` with `steer`, `/interrupt`, `/answer`) need operate on the session and, from anyone but its machine's own socket, a shared session (17.10 "Structured"); an operation answers once the adapter took it, or `pending` after 10 s. MCP serves them as the opt-in tools `session_events` and `session_send` (`zm mcp --sessions`). Event texts are untrusted. The spool is the session's scrollback: kept after the end, pruned with ended scrollback by age, never while the session lives, covered by issue redaction along with interactive scrollback and structured stderr. Conformance: `zm-agent-conformance check <adapter> -- <recordings>` replays scrubbed recordings of real sessions (`internal/agentpipe/testdata`) against an adapter with a fake runtime that refuses any line the recording does not have.

**What a session still owes** (`GET /v0/owed`, `zm owed`). The answer to "anything left?", read from records only, for one session or for every session of a person: the open pull requests (an item that is claimed or in review and carries a `pr` link; GitHub linking moves it to done on a merge and back to open on an unmerged close, so the item says the PR is open), the work items its lease holds, the cards it filed that no one has answered (`ask_open`) or that were answered yes with work not done (`ask_not_done`), and the sessions it started (by parent) that still run. A session lists its own by default, a person their own sessions' (`?person=me`, or their own name; another person's answers like a missing one, and so does a session the reader cannot see); a person's ended sessions count back 14 days unless `?since=` says otherwise. Each module reads only its own records under its own scope rules and registers its part as an `orchcore.OwedSource` (work: items and PRs; approvals: cards; session: lanes and the person's sessions); the `internal/owed` brick joins them and owns no table. Nothing is guessed: a source that fails or times out is listed as unknown (never its error text), and so is one that read only part of its records (a cap on cards or sessions, a tracker mirror that is not current), and `unknown` always names what zm keeps no record of (cloud tasks, lanes a host app or another tool started outside zm, background processes, uncommitted or unpushed worktree changes, pull requests linked to no item or whose item no lease holds now) and, when this machine holds a project only as a replica, that those leases and cards are at the authority. An empty `items` with that `unknown` is not "nothing left". The list is read-only: it settles, releases and closes nothing; closing out stays the person's call. It is local to this machine (a replica never forwards it).

**Session output redaction.** A redaction durably withholds a live managed non-task session in its project when the redaction is scoped to the session's work item (its agent may repeat text it received before the redaction, even text it has not emitted yet) or when any of its copies (metadata, spools, scrollback, the live output ring) already carries the text. A live session working something else, with no copy of the text, is not held, so it never blocks a redaction; an agent that read the issue without holding it and has not yet printed the text is the accepted gap. Output, event pages, SSE and attach replay cannot deliver held or cached pre-redaction bytes; existing viewers disconnect. The hold survives daemon restart and holder adoption. After verified output drain or writer disappearance, the daemon scrubs the event and stderr spools, scrollback and derived session text, then releases the hold. Structured events retain their sequence and envelope fields; nested tool text is scrubbed without corrupting JSON. Matching checks both escape-decoded bytes and a terminal view that removes ANSI control sequences (C1 controls count as their ESC forms, cursor-forward as a space) and drops line breaks, whitespace and box-drawing characters, which joins hard, indented and boxed wraps; matched raw spans include the intervening controls. Screen edits the view cannot follow (backspace, a carriage return that does not end a line, cursor positioning, cut-off or unknown controls) make output unverifiable only when it also carries the text rearranged into pieces (one of half the text, at most 32 bytes, and 8-byte pieces covering all but 7 of its bytes; a text of at most 16 bytes, 8 for a session working the redacted item, counts only whole) or, for a session working the redacted item, the text in order within four times its length (a cell-diff renderer interleaving repaints); otherwise the output is checked as above, so everyday terminal output, or a sibling secret differing in 8 bytes or more (a JWT header, reissued or refresh claims, a workspace's webhook URL, an account key prefix), never holds an unrelated session. Rearrangements into pieces all shorter than half the text or with more than 7 bytes in pieces under 8 bytes, and interleaved copies in unscoped sessions, are not detected. Malformed or unverifiable closed output is discarded. Unknown writer identity retains the hold and redaction reports incomplete. Unrelated projects are unchanged, and storage compaction and backup cleanup wait for both task and ordinary-session holds to clear. Work holder/inflight responses and web push session summaries reproject cached descriptions at the actual response write under the same barrier, retaining each endpoint's restricted field projection.


| state | meaning |
| --- | --- |
| `running` | working |
| `needs_human` | blocked on a person: a question, a permission prompt, `needs=human` mail it sent |
| `idle` | waiting at its prompt |
| `ended` | final; see `outcome` |

- **What ran is what was asked**: `agent`, `command` (argv executed), `version`, `model`, `cwd`, `worktree`,
  `launcher` are recorded at spawn/register and never guessed later.
- **Typed outcomes** (`outcome.kind`): `ok | quota | oom | auth | infra | killed | failed`, with `exit_code`,
  `signal`, `evidence` (the matched output line or reason) and `confidence` (`high` for exit status or an
  explicit runtime signal, `low` for a text match). Detectors are one table per runtime, tested on fixture
  transcripts. Anything not positively `ok` is never `ok`; an unmatched non-zero exit is `failed`; a quota
  message with exit 0 is `quota`, not `ok`.
- **Admission control** (spawn only): refuse with `unavailable` and `error.data` = `Admission{reason:
  memory|disk|sessions, detail, probe}` when macOS `kern.memorystatus_level` (Linux MemAvailable %) is below
  `min_memory_free_percent` (20), swap used is above `max_swap_used_mb` (4096), free disk on `$ZM_HOME`'s
  or the cwd's volume is below `min_disk_free_mb` (5120), or an owner-set `max_managed_sessions` are alive (default
  none: pressure decides, not a count, and idle shells cost almost nothing). Emits
  `dev.zm.session.admission_refused`. **Interactive sessions are never killed automatically**; the human stops sessions
  (`zm session stop`, or from a cockpit). The check is a pure function with a table test.
- **Stopping** (`zm kill`, `zm session stop`): TERM and up to 3 s for the agent to clean up and exit,
  then HUP (what a closing terminal sends; interactive shells ignore TERM) and up to 1 s; `--force` then sends
  KILL and waits up to 3 s. Each wait ends as soon as the session does. `--signal S` sends only S. The command
  exits 1 whenever the session is still running at the end. The stop route does the same escalation in one
  call with `escalate: true` (`force: true` adds KILL) and names the signals it sent (`Zm-Stop-Signals`).
- `zm run <agent> [--worktree]`: `--worktree` creates `git worktree add $ZM_HOME/worktrees/<session8>
  -b zm/<session8>` from the cwd's repo; every session gets `scratch/<session8>/`. The agent gets the
  caller's environment (minus `ZM_*` and parent-agent markers such as `CLAUDE_CODE_SESSION_ID`) plus
  `ZM_AS` and `ZM_SESSION` (its address), `ZM_SCRATCH`, `ZM_HOME`, `ZM_MACHINE`, `ZM_HUMAN`;
  never `ZM_PROJECT`.
- **Sandbox door** (optional): `zm run --sandbox` runs the agent inside Anthropic's `srt` sandbox runtime
  with a policy written from the project (writable: the project's repos, the worktree, the scratch dir).
  Refused (never silently unsandboxed) when `srt` is not installed or the session has no project.
- While no client is attached, the daemon answers a terminal cursor-position query (`ESC[6n`) itself, so
  programs that probe the terminal (every TTY `zm` call does, via termenv) never wait on a reply.
- **Embedded attach** (`zm attach --embed`, for an app that shows each session in its own terminal surface, such
  as a cockpit's libghostty surface): it replays all the scrollback the daemon holds (`attach?replay=all`, 1 MiB; a plain attach
  replays the last 64 KiB), since the surface keeps its own history. The replay makes the host terminal answer the queries in it (cursor
  position, device attributes, device status, OSC 4/10/11/12 colours, XTVERSION and other DCS replies, DECRQM
  mode reports, kitty keyboard flags, window size reports; not `ESC[1;2..8R`, which is a modified F3 key though a
  cursor report at row 1 looks the same), and those answers would reach the agent as typing.
  From the attach until output, once it has begun, has been quiet for 250 ms (before the first byte only the cap
  counts: a loaded machine replays late; at most 3 s) the client drops input that is exactly
  such an answer, across read boundaries, and passes everything else (a person's keys; a bracketed paste,
  `ESC[200~` to `ESC[201~`, verbatim; an unfinished escape is
  held at most 50 ms; one the window closes on is still read to its end through the filter, never sent half as
  keys); after that it passes everything, since live queries are answered correctly. ctrl-] is an
  ordinary key, nothing but errors goes to stderr, the resize and redraw wiggle stay. Exit codes: 0 the session
  ended (also before the attach), 3 the connection to the daemon was lost (attach again), 2 usage or no such
  session (or one zm did not start), 1 anything else, and 4 to 6 for another machine's session (below).
- **Experimental fresh-surface replay** (`zm attach --embed --replay-modes`, local only): ordinary `--embed` stays unchanged until pinned Ghostty rendering acceptance. The opted-in client offers `zm-pty-replay-v1, zm-pty` and identifies a fresh surface; the same authorized attach route selects one explicitly. No error triggers an alternate request. The framed outbound stream carries an offset-bound parser checkpoint, exact raw chunks, an explicit normal end, or a gap requiring a new surface through the host's existing bounded reconnect. Inbound keys use the unchanged shared input path. The frontend consumes only an incomplete leading control/UTF-8 fragment before choosing a safe Ground boundary; its raw cursor includes omitted bytes and excludes all generated prefix bytes. It restores only observed screen selection with47 and paste2004 to frontend stdout. It never injects into the managed PTY or changes input/paste authority, task data or stored output. Mouse restoration is withheld because the daemon's UTF-8 mouse classifier still derives from raw output. Unknown/omitted state and skipped fragments report incomplete replay and point to recorded conversation; terminal redraws can erase this notice, so it is not a persistent app status. Old holders and ended raw archives remain unknown when their prefix is gone. This is neither full VT/saved-cursor/screen restoration nor complete history recovery. Remote opt-in is refused; existing remote attachment continues unchanged. API.md defines frame sizes, offsets and compatibility. No actual-session rendering acceptance has enabled this experimental mode by default.
- **Attach over ssh** (`zm attach <machine>/<id>`, `zm attach --on <machine> <id>`, `zm run --on <machine>`, `zm ssh
  <machine>`): another machine's terminal comes through the system `ssh`, with the user's own config, keys and agent.
  zm stores no key, opens no port and has no relay; no route changes. **The machine's name is the ssh destination**,
  so a Host block in `~/.ssh/config` is all the setup a differently named box needs; a name that is not
  `[A-Za-z0-9][A-Za-z0-9._-]*` is refused (it comes from a teammate's machine, so it must not read as an option or
  carry ssh's `%` tokens). Who may: this machine's daemon must show the caller the session (`GET
  /v0/sessions/<address>`; a session known from presence has `seen`, and that is what sends the attach to ssh; hidden
  answers as missing, exit 2, and ssh never runs; unmanaged and ended are as for a local session), and ssh must let
  them log in. On the far side they are that machine's user on its socket, which `ssh` already made them. `--on`
  skips this machine's daemon and asks that one instead: for a session just started there, which its next heartbeat
  has not listed here (about 5 s), and for a machine in no team project. `--on mini other/abcd1234` is refused (exit 2
  with `--embed`), not read as mini.
  **Trust rule: zm connects only to a name this Mac already knows as an ssh host.** A machine's name is what a
  replica chose when it joined, so a teammate could otherwise send this Mac's ssh anywhere. Known means either a `Host`
  pattern with no `*` or `?` that equals the name, in the ssh config or a file it Includes, or a key for the name's
  resolved host in known_hosts. A wildcard (`Host *`, `*.local`, `*.ts.net`, `dev-*`) does not make a name known, as it
  fits any name a teammate picks, and neither does a `Match` block; a host reached only through those is known once its
  key is in known_hosts, which `zm ssh <host>` by hand does on a first connection. ssh's own tools decide (`ssh -v -G`,
  `ssh-keygen -F`), neither connects, and anything they can't answer counts as not known. It is checked before any ssh
  runs, in `zm attach`, `zm run --on` and `zm ssh`; a name that fails runs no ssh, exit 7, and the message says to add
  an exact Host entry or connect once by hand, with no button in a cockpit. One exception: a person at a terminal running `zm ssh <host>` chose the host themselves, so a first
  connection goes ahead. `zm ssh --known-only` (a cockpit, with a name from presence) and `zm ssh` with no terminal have no
  exception. Once connected, the host is in known_hosts. A known name can still come from a teammate: they can point a
  click at a host this Mac trusts, which runs `zm attach` there and finds no session, but they can't introduce a new one.
  What runs is `ssh -tt -e none … -- <machine> "sh -c '…exec zm attach [--embed] <id>'"`: a pty, no escape character
  (`~` is a key like any other), `ForwardAgent=no` and `ForwardX11=no` on every ssh zm runs, `zm ssh` included, whatever
  the user's config says (the machine may be a teammate's), `ServerAliveInterval=15` with 3 misses, `ControlMaster=auto` with `ControlPersist=600` and
  the socket in `$ZM_HOME/ssh/%C` (left out when the path would not fit a socket: it is only a speed-up), and
  `ObscureKeystrokeTiming=no` (DESIGN-NOTES, "Remote terminals"). The far `sh` adds `~/go/bin`, `~/.local/bin`,
  `/opt/homebrew/bin` and `/usr/local/bin` to PATH, as ssh's non-login shell often lacks them. ssh owns the terminal
  (raw mode, window size, typing); `zm` stays as its parent only to read how it ended. With `--embed`: `BatchMode=yes`
  and `ConnectTimeout=5` (a surface cannot answer a prompt), stderr carries `Connecting to <machine>…` until the far
  side clears the screen, the far zm has `ZM_NO_AUTOSTART=1` and `ZM_REPLY_QUIET_MS=1000` (the reply window's quiet,
  as a terminal's answers arrive a round trip late; ignored by an older zm), and the exit codes gain: 4 no answer
  from the machine (ssh's words: unknown host, timed out, no route, connection refused), 5 ssh refused (permission
  denied, host key), 6 ssh works and zm is not on its PATH (the far shell's 126/127), 7 the trust rule. ssh's own failure
  with none of those words is 3. `--why-file <path>` (hidden, for a host app) gets one line on any failure: zm's reason,
  else ssh's last stderr line, so a host need not reattach blind: a cockpit reattaches only a session that was really up,
  at most 40 times in a row (about 5 minutes), and shows the line otherwise. Without `--embed` a person gets the same reasons as a line
  and exit 1, and ssh may prompt. `zm run --on <machine> [flags] <agent>` runs `zm run --detach --json` there with
  every flag given but `--on`, `--detach`, `--json` and `--as`, prints the session (`--json`) or its address
  (`--detach`, no terminal), else attaches with ssh as above; its exit codes are 0, 1 and 4 to 7. `zm ssh <machine>
  [command]` is plain ssh over the same shared connection: what it asks (a passphrase, a new host key) is answered once
  for the attaches that follow while it stays up. None of this was tried against a real remote machine yet.
- **Read-only attach** (`attach?input=0`, `zm attach --read-only`): a viewer gets the same scrollback and live
  output, but what it sends is dropped, never counts as a person typing (a hook-set `needs_human` stays), and
  it is not a terminal: the daemon keeps answering cursor queries while only viewers are attached.
- `GET /v0/sessions` pages: `?limit=` (at most 500) rows, most recently updated first, and `next`, a cursor
  for `?before=` when more rows follow (an exactly full last page has none).
- **zm never deletes a worktree.** `zm session gc` lists ended sessions' worktrees with reasons
  (outcome, `git status` dirty, branch ahead-of-upstream count) and `safe` = ended AND clean AND ahead == 0.
  The human deletes.
- `spawn --work <id>` takes a **bound lease** (section 10) on the item for the session. In a team project this machine
  joined, the team module takes it at the project's authority as the new session's own claim (`core.TeamLeases`), answered there or refused; refused, no session starts. It is never queued: with the authority away, or
  silent for 5 s, the spawn is refused (`503`) and nothing claims the item later (a claim that may have landed is
  released). The authority holds it as the session's default-TTL lease and binds it once a heartbeat shows the
  session running; a session whose start fails after the claim gives it back (queued if the authority is away).
- **Parents**: `parent` on a session is the address of the session that started it, from Unix peer ancestry (the nearest
  registered session above the calling process): `zm run` and `task_start` from inside a session, and an agent that
  self-registers below one (a `codex exec` from an agent's shell, a Codex subagent thread). It is optional: nothing
  above the process (a person's terminal, a daemon or a job runner), a session from before lineage was recorded, or a
  Claude Code subagent, whose hooks carry its parent's session id and so have no row of their own. A hook naming a live
  managed session through `ZM_SESSION` updates that session and adds no row, so what such a session's agent runs
  without `zm run` is not a child either. A parent can be one that has ended, and is left out when the reader cannot
  see it (section 6). A replica sends it in presence; the authority keeps it only when it is on the beat's own machine.
  Events carry it for this machine's owner only; token readers get it from reads.
- **Names** (identifiers-2026-11-01 stage 3): `zm run --name auth-fix`, `zm session name <session> <name>`
  (`""` clears it). A name is an alias beside the address, unique among the machine's live sessions (`409`
  otherwise), and resolves only while its session is live; an ended session keeps it, unresolved. Every
  session route takes `<name>` or `<machine>/<name>` for this machine's sessions; mail takes
  `<machine>/<name>` (a bare name is a machine; a bare name a visible session here holds answers with that
  hint) and stores the address it resolved to, so a name reused later never redirects older mail. `Zm-As` may
  name a live session on the socket and becomes its address (a name of nothing is refused), so no name is
  ever stored as a sender. A name is never a session8, so a stored or typed address (any `<machine>/<8
  letters and digits>`, minted or derived from a runtime id) is never read as a name. On a team replica the proxy resolves this
  machine's names in `POST /v0/mail` before forwarding (the authority does not know them). Internal lookups by
  a stored address never read names. Names may not be `gc`, `admission`, `register` or `hook` (routes). A
  revived registered session whose name went to another live session loses it. `title` stays free text.
  Names of other machines' sessions (presence) are not resolved yet.
- **Daemon restarts keep managed sessions** (session holders). Each managed session runs under its own holder:
  `zm __zm-holder <home> <session8>`, the zm binary in its own Unix session (on macOS a daemon that keeps sessions
  runs new holders from `$ZM_HOME/holder-bin/<build>/zm`, an APFS clone of its own binary made at start: a `cp`
  over the installed file kills every new exec of it (code signing), never a running process), which owns the PTY, the agent (its
  child) and the 1 MiB scrollback, listens only on `$ZM_HOME/holders/<session8>.sock` (0600, directory 0700) and
  exits only after its child does (or, tied, when the daemon that started it goes: below). The daemon talks to it
  over that socket, protocol `zm-holder/<major>`: frames `type u8, length u32 BE, payload`; the holder's first
  frame is always `H`, a JSON hello (`proto`, `minor`, `version`, `commit`, `session`, `home`, `holder_pid`,
  `child_pid`, ring `start`/`end`, `exited`) whose shape never changes; the daemon adopts with `A {since}`, gets
  `F` carrying the PTY master (`SCM_RIGHTS`; the daemon writes input, resizes and reads the foreground group on
  it), then `O` frames (8-byte offset plus bytes) and `X` (the exit: `code`, `signal`, `out_bytes`, `stop_sig`,
  `stop_by`) after the last byte; it sends `S {sig, by}` to stop and `K` once the exit is recorded. Unknown frame
  types are ignored both ways (a minor bump only adds). Before the holder reports an exit it writes
  `holders/<session8>.exit` and `sessions/<session8>.out`.
  - **Replay checkpoints (minor1)**: a holder may advertise `replay_modes:1`; a daemon requests that exact schema in `A`. `F` remains first. Only then can an `M` frame (8-byte offset +44-byte bounded parser state) immediately precede the first or discontinuous `O` from the same ring snapshot. Older clients omit the request and receive legacy frames; unknown schemas/old holders remain unknown rather than guessed. The holder checkpoints bytes as they leave its1MiB ring, including skipped prefixes of oversized writes. The daemon mirror consumes validated metadata only with the matching next raw write; duplicates do not replay parser bytes, and gaps without metadata invalidate state. No title, activity, provider/input authorization or task event is generated by metadata. Absolute offsets and `.out` format remain unchanged; no archive checkpoint sidecar is claimed.
  - **On start** the daemon connects to the holder of every managed session not `ended` and adopts it only when the
    socket and its directory are this user's with no group or other bits, the peer's kernel credentials are this
    uid and the hello's holder pid, the hello names this home and session, and the child is the row's `pid` with
    the holder as its parent. It replays the holder's ring into its own before serving, so the first attach
    replays everything; replayed bytes are history (no idle wake, no cursor-query answer). The session's row,
    state and bound lease are untouched, so a restart shorter than `bound_lease_ttl_seconds` loses no lease.
    A holder that is gone leaves its exit file (the session ends with its real outcome) or nothing (outcome
    `infra`, note `daemon restarted`, as for a session from a zm before holders, whose PTY died with that daemon).
    A signal from outside zm is `killed` with the signal and the note that the sender is unknown (zm never guesses
    memory pressure); one whose exit came before this daemon started adds that it ended while no zm daemon ran.
  - **Why a daemon ended** is in `daemon.log`: a `daemon exiting` line with its cause (a caught INT, TERM or HUP, an
    error, or a stop request). A daemon that never got there (SIGKILL, power loss) leaves `daemon.run`, and the next
    one logs `previous daemon ended without a clean exit` with its pid, build and start, pointing at
    `daemon-crash.log` (a Go crash report) when that is newer.
  - **Another major** is not adopted: the session keeps running, `unreachable` on the session (owner only) and in
    `zm status` says why, attach, input and stop answer `409`, it still counts for admission, and the daemon tries
    again every 15 s. A minor difference adopts. A daemon whose binary was replaced under it by an incompatible one
    refuses new spawns with "restart the daemon".
  - **Stopping** goes through the holder, which signals the child's process group (the daemon signals it itself
    when the holder does not answer). `zm daemon stop`, `restart` and `upgrade` stop the daemon only: sessions keep
    running and the next daemon adopts them. Their guard (`orchcore.SessionService.RestartBlockers`, section 2
    "Swaps") lists only sessions a restart would end: none once every session has a holder; every live one on a
    daemon from before holders, or one that ties its holders. `--end-sessions` (stop: also `--force`) attempts to end them, reports only the number that actually ended, and lists each remaining session with its reason and manual action. An authenticated holder answering on this home socket can stop a legacy session without recorded home identity; direct signals still require the recorded identity. An upgrade or restart whose new daemon
    reports `holders.unreachable` (sessions alive that this build cannot adopt) fails its verify and rolls back.
  - **Under a service manager.** A holder leaves the daemon's process group but not its cgroup. launchd kills only
    the job's process group, so holders survive `launchctl kickstart -k` and `bootout`. A systemd unit whose main
    process is the daemon kills its whole cgroup on stop or exit unless it sets `KillMode=process` (or `none`), so
    without that `RestartBlockers` lists every live session, the refusal says why, and a hand-run `systemctl restart`
    or a crash ends them (docs/runbooks/zm-upgrade.md). The same holds when the daemon's cgroup names a `.service` and
    `systemctl` gives no answer. A daemon with no such cgroup (macOS, a login session) has no unit, and `systemctl` is
    not asked. zm writes no unit for its daemon.
  - **Outside the starting app (macOS).** A process stays in the resource coalition of the app that started it, and
    force-quitting that app ends the whole coalition, setsid or not. So on macOS every daemon start (autostart,
    `--detach`, a swap's start and rollback) goes through a one-shot launchd job (`dev.zm.launch.<nonce>`, gui
    domain, Interactive): it runs `zm __zm-launch`, which starts the daemon in its own session, writes its pid and
    exits; the job is then unloaded. The daemon and its holders sit in that job's coalition, not the app's, and the
    daemon is no job's main process. The environment goes in a 0600 file the launcher deletes, never in the job.
    Where launchd takes no job (no GUI login, as over ssh) the daemon starts directly and `daemon.log` says it
    shares the starting app's fate.
  - **Only `zm daemon` keeps sessions.** Every other daemon (tests, in-process) starts its holders tied: the
    holder keeps the daemon's pipe on its stdin and ends the child (TERM, KILL after 2 s) at EOF.
  - **Orphans**: a holder whose home is gone (moved or deleted) is reached by no daemon. `zm status` (`health.holders`,
    local callers) lists those of this user it can see, with their pid; zm never stops one.
  - An attach whose daemon stops ends like a dropped connection (`zm attach --embed` exits 3); attaching again
    after the restart replays the scrollback, from before the restart too.
- Hooks report `running` on prompt submit, `idle` on stop, `needs_human` on a permission prompt, in **one
  request per hook** (`POST /v0/sessions/hook`: register-or-find by the runtime's id, the state, and on
  prompt/start the session's notice line); never a tracker call. Hooks never renew leases (no coordination on the prompt path). Inside a
  `zm run` session the start hook registers with `session=$ZM_SESSION`, recording the runtime's id on
  the managed session instead of creating a second one (in a managed shell: binding it, below). A runtime's session-end hook ends an unmanaged
  session (`register` with `end`, outcome `ok`); a managed session ends only when its process exits.
- **Agents in a shell**: a hook naming (`ZM_SESSION`) a live managed session that runs no agent runtime (a cockpit's
  shell, a custom program) binds its agent there instead of registering a second session: `runtime_agent` `{agent,
  runtime_id, model, task, pid, started}` on the shell's row, and the agent's state, activity, subagents and needs go
  to that row (`task` is its latest activity, kept while idle). Only the first agent binds: while it runs (its pid
  live, or with no pid until another agent's session-start; a session-start from the bound pid itself is its new
  conversation, `/clear` or `/resume`, and rebinds), another agent naming the shell (a `claude -p` it starts
  inherits `ZM_SESSION`) registers its own row, as does a conversation that already has a live row of its own. A
  later hook of the bound agent that does not name the shell (a completion) reaches it by the runtime id. Its
  session-end hook (sent with `session` from inside a managed session) unbinds it, as does its pid exiting (checked
  every 15 s; Codex sends no session-end): the binding, activity, subagents and needs go and a running or
  needs_human shell goes idle. Each binding change publishes `dev.zm.session.updated`. Presence does not carry it
  yet (a newer replica's field would make an older authority refuse the whole heartbeat).
- **Subagents** (Claude Code): `SubagentStart` and `SubagentStop` (`zm hook claude subagent-start|subagent-stop`)
  add and end a subagent under the session (`subagents`: id, type, description, state running or ended, its latest
  activity, started, updated, ended), changing nothing else; the parent's `Task`/`Agent` tool hook (a spawn) gives the
  next start of its type its description. A tool hook carrying `agent_id` is a step inside that subagent: its activity
  is the subagent's, and it never moves the parent's state, cwd, project or title. A running subagent no hook reached
  for 30 minutes (its stop was missed) counts as ended. Reads carry the live ones (at most
  20, oldest first); `GET /v0/sessions/{id}/subagents` lists them with those ended in the last hour, each with its
  last 20 activity lines. A start or stop publishes `updated`; a subagent's activity alone is live-only. Codex
  subagent threads stay sessions of their own with `parent` (below).
- **Why it waits**: `needs {kind, at}`, only while `needs_human`: `permission` (a permission prompt) or `input` (an
  MCP elicitation) from the agent's own notification hook, `decision` from needs=human mail the session sent, `manual`
  from `POST /v0/sessions/{id}/state`. Any state other than `needs_human` clears it (the next prompt or tool). The
  notification names no subagent, so a hook's wait records the subagent whose step came last before it (`subagent`),
  unless the parent's own work came after; that subagent's next step or its stop ends the wait.
- **Activity log**: every change of a session's or subagent's activity is a line (`GET /v0/sessions/{id}/activity`,
  newest first), the last 200 per session (trimmed on every write), kept 7 days after it ends (checked hourly); a task session's lines are dropped on a
  redaction in its project. Same privacy rule as `activity`. This machine's sessions only.
- A hook-set `needs_human` (a permission prompt) on a managed session clears when a person types into it
  (attach or `zm session input`) and the agent prints again. A `needs_human` set by mail clears only by mail.
  What a terminal sends by itself is not a person typing: on an attach, input made only of focus reports
  (`ESC[I`, `ESC[O`, DECSET 1004), mouse reports (SGR `ESC[<b;x;yM`/`m`, X10 `ESC[M` plus three coordinate
  bytes, or UTF-8 coordinates once the program's output turned on DECSET 1005, urxvt `ESC[b;x;yM`) and answers
  to queries (the attaching terminal answers those in the replayed scrollback: `ESC[?…c`/`>…c`/`=…c`, cursor
  reports `ESC[r;cR` and `ESC[?r;cR` but not `ESC[1;2..8R`, which is a modified F3, `ESC[0n`/`3n`, `…$y`,
  `ESC[?…u`, `ESC[digits…t`, OSC 4/10/11/12 colour answers, any DCS) reaches the program but does not count;
  any other byte in the read does. Unlike `zm attach --embed`'s filter this is not limited to the replay: none of
  these shapes is a key. Sequences split across reads are decided when they complete; a read ending in a bare
  `ESC`, `ESC[`, `ESC]` or `ESC P` counts (it may be the Escape key or an alt-key).
- A managed session with no PTY output for `idle_after_seconds` (default 300) becomes `idle` (note `no
  output`); output makes it `running` again. Bound leases are renewed only while `running` (section 10).
- A managed session of any kind but `shell` (an agent, a task) runs at nice `agent_nice` (default 10, at most 19;
  `-1` keeps the daemon's own): its holder lowers the child's process group as soon as it runs, so the agent and
  every build and test it starts yield the CPU to the owner's interactive UIs under load. A person's shell is not
  lowered. Best effort: a daemon already niced further keeps its own.
- **A session in `none` follows its agent.** A managed shell opened outside every project starts in `none`; when a
  hook of the agent in it (session-start, prompt, tool) reports a cwd that a cwd rule resolves (section 5 rules 3-5,
  or on a replica a joined team project's `.zm/project.toml` or recorded checkout; never `--project`, `$ZM_PROJECT`,
  the session rule or `default_project`) to a project the launcher may write in, the session moves to that project
  and area (`dev.zm.session.updated`). The same holds for an unmanaged session registered in `none`. A session that
  has a project never moves this way, nor one holding a lease; each session's last cwd tried is remembered, so a
  repeat costs no resolution.
- **Moving a session by hand**: `zm session move <id> <project>[/<area>]` (`POST /v0/sessions/{id}/project`, local)
  puts a session in another project, area or `none`; the caller needs operate on the session and write on the
  target (a hidden target answers like a missing one). Refused (`409`) while the session holds a lease in its
  project, and for an ended session.
- **Any move** (by hand or by following the cwd; at an authority, a replica's session that shows up in another
  project's or area's heartbeat) publishes `dev.zm.session.left` to the old project/area's log, data `{address,
  project, area}` of where it was and nothing about where it went, then the session's event (`updated` for a local
  move, `registered` for presence) to the new one. The old project's next heartbeat does not end a session that
  moved.
- **Machine numbers** (a cockpit's Machines panel): memory used and total, swap used and total, free and total disk
  of `$ZM_HOME`'s volume, and live managed sessions against the admission limit (`max_managed_sessions`, 0 when none is set), from
  the admission probe (macOS: `kern.memorystatus_level`, `hw.memsize`, `vm.swapusage`; Linux `/proc/meminfo`;
  `statfs`), sampled at most every 30 s; memory used is the total less what the OS counts available, so it reads as
  pressure, not as cache. Unknown values are `-1`. `GET /v0/machines` lists this machine (live) and, on a team
  authority, every joined machine from its last heartbeat (`seen`; a machine that left the project, by `zm leave`
  or a revoked token, is not listed). Only members' machines count: an authority records a replica's numbers only
  when its token reads the project whole, so an area guest's machine is neither recorded nor shown (a report as a guest drops what it sent as a member). Privacy follows
  presence: a replica's numbers are shown only to whole-project readers of a team project it heartbeats for (an
  area guest sees none, as with a machine's group mailbox), and
  this machine's own only to its owner or to a whole-project reader of a team project it is the authority of. On a
  replica the list is merged with each authority's, the authority's copy of this machine dropped (answered here),
  and a machine several authorities list (it joined projects at more than one) is one row: the latest `seen`, all
  its projects. Each machine's numbers carry `zm` (section 2, "Release channel"): its version, commit and daemon
  start, the channel release it runs and when it was installed, the hold switch's reason and since, the last update
  check (result, reason, at; `$ZM_HOME/update-last.json`), and the releases it vouches for (`stable`) and vetoes
  (`failed`). An authority ignores fields it does not know in a report (numbers only grow). One older than that
  refuses the whole report naming the field; the replica sends it again without that field (`script_runs` or `zm`, so a
  canary's vouch still reaches an authority that knows `zm`), and leaves it out for an hour; `task_slots`, and
  `sandbox_tasks` with `sandbox_slots` (both go together), are left out of that one report.

- **Machine choice**: `task_start` with `machine: "auto"|"<name>"` starts the task on another machine
  of the parent's team project (or of `project`, below), and nothing connects to that machine: no ssh, no new port. The parent's machine asks
  the project's authority (`POST /v0/team/tasks`, its node-bound token; in-process on the authority), which queues a
  machine op; the target's next heartbeat answer carries it (within 15 s; 2 s while it reports ops), the target records
  it before running it (never twice, also across a restart; the op ID is the task's `client_request_id`) and starts an
  ordinary task under its own limits (admission, depth, per-parent cap, no prompts, deadlines, redaction) whose parent
  is the remote session; its reports (the result, then the task's status) go with each beat until an answer took the
  final word. An op for the authority's own machine runs at once. `task_status`, `task_read` and `task_cancel` on the
  `rt-` ID work from the parent's machine; only the parent and that machine's owner see it. Ending mails the parent
  once (from the task's session, through the authority) and comments on the linked item, as for a local task.
  **Another project**: `project` (a slug or ID; only with `machine`, never this machine, and
  never with `cwd`: the machine uses its own checkout of that project) places the task through that team project
  instead of the parent's, so a session in a solo or other project can use a pool. The parent's machine resolves
  it as the caller writes there (`WriteTarget`, operate), the caller narrowed by every cap along its session's
  chain (section 1, rule 3), and checks the parent's own project the same way when `project` is left out: a
  project the caller cannot see answers `not_found` exactly like a missing one (status, body and hint); a visible
  project that is no team project is `bad_request`. The authority then applies the who-may below to the named
  project. The parent stays the caller's session: status, read, cancel and the ending mail go to it; the task, its
  session and that mail are filed under the named project (the parent's whole inbox shows the mail; one filtered
  to its own project does not). `client_request_id` is per parent across projects: the parent's machine records
  the project a request id went to, a start naming another project with it conflicts, and a `lookup` finds its
  run whatever project it names (so `zm exec --cancel` needs no `-P`). The MCP tool and `zm -P <project> exec`
  send `project`; omitted, nothing changes.
  **Who may**: only an identity that owns or administers the project (`core.Deps.ProjectAdmin`, by its own grants:
  a member Mac's token is capped at member), and only onto a machine whose owner opted in (`"remote_tasks": true` in
  its config, shown as `remote_tasks` in its machine numbers; default off: a member's machine is not the owner's to
  drive). **Choosing**: a name must be a machine with numbers for the project (it pins the start there); `auto` picks
  among those that take remote tasks, never the parent's own machine, never one at its session limit or under 2 GB of
  disk, the most free memory less 1.5 GB per live session, ties by name. A sandbox task (`sandbox`, "Where it runs")
  goes only to a machine that also reports `sandbox_tasks`; its choice skips the session limit and free memory (the
  agent runs in a pool box, the driver is small), keeps the 2 GB of disk and the heartbeat, and prefers the machine
  with the most `sandbox_slots`, ties by name. A start waits in the authority's line until
  such a machine is free and its heartbeat was heard in 2 minutes (the authority's own always is), and at most 3 of a
  parent's tasks are placed at once across machines and the authority's projects ("Waiting line" below). The
  parent's machine refuses a start while 3 of the parent's tasks are unsettled at other authorities (it counts them
  there, one start per parent at a time; a task its authority hides counts until known ended, one whose authority
  cannot answer for 10 minutes after its start); the authority asked keeps the parent's 4th and later waiting instead.
  **Where it runs**: `cwd` (an absolute path there), else the main checkout of the project's most recent session on
  that machine; it must resolve to the project there. With neither, a start is refused, except an approved ask's
  agent action, which runs in a scratch directory of its own there, removed when it ends (kept while its process
  may still run, or when on_limit hands it to a next agent). An ask's action with no `cwd` also skips
  a checkout no worktree can be made from (a submodule, a filter, a partial clone): it tries the next of the
  project's 3 most recent checkouts there, then its scratch directory. **Sandbox tasks**: `sandbox: {"size"?, "egress"?}`
  (`zm tasks start --sandbox [--size c2m8] [--egress host,...]`; `size` a pool box size `c<cpus>m<GB>`, empty the driver's
  default; `egress` at most 32 hosts, each `name.tld` or `*.name.tld` (no IP address, port, bare `*` or line break), that
  the driver adds to the box's egress allowlist for this task, under its own policy; empty, its default list) runs
  the task in a pool box through the machine's task driver: its owner's config `sandbox_task_cmd` (an absolute
  path; unset or not absolute, the machine takes none, and the daemon logs a warning: a bad value never fails the
  config load) with `sandbox_task_slots` (1..16, default 1, clamped). The driver is not a session for
  `max_managed_sessions` or the machine's `sessions` number: its slots are its own (memory is not checked for it;
  disk is), and no other session may use its agent name `sandbox`. The machine opens the task's
  session and binds its `work` item as for any task, makes no worktree and no checkout, and runs the driver as the
  task holder's child in its own process group, from `$ZM_HOME/sandbox-tasks/<op id>` (removed when it ends), with
  the task as one JSON line on stdin (never argv) and only SPEC 20's environment allowlist plus zm's session
  variables. The driver contract (the farm's `zm-task.mjs` driver): stdin JSON `{"task_id","op_id","project",
  "time_left_s","request":{"work":"bd:<bead>","prompt","sandbox":{"size","egress"}}}` (driver also accepts `body` for prompt
  and `timeout_s` for time left; default 4 h); env ZM_SESSION (session address, required), ZM_TASK_ID,
  ZM_TASK_RESULT (file path the driver writes the FINAL text to); exit 0 ended, non-zero failed, exit 2 = bad stdin;
  SIGTERM then SIGKILL after 60 s; HOME and PATH of the daemon user. zm also sends `parent` and the whole request,
  `ZM_TASK_ID` is the `rt-` id, and between the 60 s and KILL comes HUP, as in every zm stop (below). On exit the result is that file (at most 64 KiB: a JSON
  object of `finished` and `text`, or plain text; none: the output's tail): exit 0 ends it `ok` (`done` unless the
  file says `not_done`), any other exit `failed`. Its output is its progress, so no no-output timeout. A cancel or
  the deadline sends TERM to the group and gives the driver 60 s to release its box and revoke its tokens, then HUP
  and KILL; a cancel answers at once and the task ends when the driver exits. A daemon restart re-adopts the live
  driver through its holder like any task (else it ends from the holder's exit file, or as `infra`). Status
  shows `sandbox` and the request's agent. Refused: `sandbox` with `run` or `continue_from`, an unknown size, an
  `egress` entry that is not a host name, a `cwd`, or without `machine` (`400`). Rollback is unsetting `sandbox_task_cmd`: sandbox starts are then refused
  `503` (or `403` when pinned). Not the session `sandbox` (the srt filesystem sandbox of `zm run --sandbox`). A
  placement its machine did not take in 10 minutes goes back
  to the line; a cancel before its machine took it means it never runs, after that the machine stops it with its
  next beat. `continue_from` is not taken from another machine, and a remote parent's end
  does not cancel its tasks there (cancel them by ID). `on_limit: next_agent` hands off there as for a local task
  (the continuation keeps the source's project and depth); the `rt-` task's status then follows its chain: the
  newest agent's session, state and result, with `tried` listing each earlier agent and its outcome, and it stays
  `running` while the next agent is chosen. The same queue carries any later op a machine runs for its
  authority (structured-session turns are one more kind).
- **Waiting line** (the farm's task queue, inside machine choice rather than a queue of its own). Every machine-choice start (agent task or script run) is a row of the authority's
  `team_task_line` (the registry: it survives an authority restart). A start no suitable machine can take now waits
  there instead of being refused; the authority places waiting starts, front ones first, then oldest first, each on
  the roomiest free machine that suits it. Each placement is one ordinary `task_start` op (the first carries the
  task's id, a later one `<id>.<n>`), answered at once on the machine's held `GET /v0/team/ops` poll (a machine that
  takes remote tasks holds it, as a box host does), so a task starts within about a second of a slot freeing. A
  report counts only for the task's current placement. The authority places again when a task settles, a start is
  cancelled, and every 5 s. **Suitable**: the machine has numbers for the task's project (its daemon joined that
  project: one that joined only another never gets the task), is trusted there, takes
  remote tasks (and runs scripts, for a run; reports `sandbox_tasks`, for a sandbox task), its heartbeat was heard in
  2 minutes, and it is under its session limit (its live sessions plus the plain tasks placed there not started yet;
  not for a sandbox task) with 2 GB of disk; `auto` also skips the parent's own machine. **Free**: fewer of the
  authority's placed, unsettled tasks are there than its `task_slots` (machine numbers; its config
  `remote_task_slots`, default 1; a machine that reports none counts as 1); a sandbox task counts apart, against its
  `sandbox_slots` (config `sandbox_task_slots`), and never takes a plain slot, nor a plain task a sandbox one. **Order**: `front:
  true` (`zm tasks start --front`, `zm exec --front`) puts a start before every non-front one, front starts in their
  own order; a named machine pins it, so it waits for that machine only and never holds up the starts behind it. A
  start no machine could ever take is refused at once (an unknown machine `404`, one that takes no remote tasks or is
  public `403`, a run onto a machine whose zm runs no scripts `503`, `auto` with no machine that takes remote tasks
  `503`; a sandbox task pinned to a machine without a task driver `403`, `auto` with none in the project `503`); one whose machine is only busy or quiet waits, at most 24 hours (then `refused`); one pinned to a machine
  that becomes public is refused. Status shows a waiting start as `pending` with `position` (1 is next) and
  `machine` its pin (empty for `auto`). **Per parent**: at most 3 of a parent's tasks are placed at once across the
  authority's projects; more wait, skipped in order until one of the parent's settles, and at most 100 of a parent's
  wait across them (`503` past that). **Lost
  machines**: a placed task whose machine's heartbeat to this authority stopped for 3 minutes (looked for only once
  the authority has itself been up that long; the heartbeat time is the authority's own record, so no failed lookup
  reads as lost), or that its machine took and never reported in 10 minutes, goes back to the line once, in its old
  place, never onto that machine again, with what is left of its time budget (`timeout_s`, else 4 hours, from its
  first placement) as its `timeout_s`; `requeued` counts it. It ends instead, `ended` with `lost` saying why and an
  `infra` outcome, when it went back once already, was cancelled, zm saw it take an outward action (`acted`), is a
  script run its machine took (it may have run: zm cannot see what a script did), under a minute of its budget is
  left, or it is a sandbox task and no machine of the project but those it was lost on (and the parent's) takes
  sandbox tasks (it would only wait out the 24 hours). A pinned task is never run again: it waits for its machine, whose copy is the task, until its budget is
  spent; one carried to a machine that beats but never took it goes back to wait for it as a new placement, without
  counting, while its budget lasts (then it ends; cancelled, it ends at once). A start queued by an authority from before the line counts as pinned to its machine. A placement not
  carried yet goes back without counting; one on the authority's own machine that never began
  there (a restart in between) is refused after 10 minutes. A requeue or end applies only to the task as the sweep
  read it: a report since (a push) decides again. **Outward actions**: every write a task's session sends through the
  authority (each forwarded write route is classed: mail, comment, status, claim, issue, ask, session, box, ...; one
  not classed counts as `write`), except a mail ack or got-it or an issues SQL read; and a push its machine reports (a
  commit of its worktree branch made after zm created the branch that a remote-tracking ref reaches). The session is
  known from the task's first report, which the machine sends with the next beat, kicked as the task starts. **The
  old copy**: once a task went back or ended lost, its placement on the lost machine is a recorded copy (a keyed
  table, so the check costs the same whatever the line holds). The authority refuses its session's writes (`409`),
  live or replayed from an outbox, so a machine waking from sleep cannot act for it; while that copy may still run
  there (until a heartbeat from the machine no longer lists it), the machine's forwarded writes as its owner, without
  `Zm-As`, are refused too (an agent can call the socket without it). When that machine reports the copy running, the
  authority cancels it there; cancelling the task cancels its copies too, also after it ended lost. A report never
  revives a refused, cancelled or lost task. An action zm cannot see (a `gh` comment, a push to a bare URL) is not
  known, and a task that ends lost mails nobody: its parent reads the status. Known gaps: a script run whose checkout
  takes over 10 minutes before its first report ends lost and its copy is cancelled; an authority restart between
  its own machine starting a task and recording it leaves that task refused while it runs on, untracked. **Lists**: `GET /v0/team/tasks?project=`
  (`zm tasks -P <project>`) for the project's owner or admin, on the authority's socket or a member machine's (sent
  on to the authority): the waiting line in order, then the placed tasks, then the 20 newest settled, parent and
  session addresses redacted. Status and cancel stay per task (`GET /v0/tasks/{rt-id}`, `POST
  /v0/tasks/{rt-id}/cancel`; `zm tasks status|cancel`): cancelling a waiting start takes it out of the line
  (`refused`, "cancelled while waiting in the line").
- **Sessions on other machines**: `POST /v0/sessions` with `machine: "auto"|"<name>"` and `mode: pipe`
  starts a shared structured session on another machine of the cwd's team project, on machine choice's path and with
  its who-may (the caller's identity owns or administers the project; the target is trusted and has
  `"remote_tasks": true`; named machines heard from in 2 minutes) and its op queue (a `session_start` op, run at most
  once there; one no heartbeat carried within a minute never runs, and one carried but not reported in 3 minutes is
  answered as such). This is how a scheduled Mezze app run on a provider-zm
  machine starts (the Mezze app's scheduler, not zm, keeps the schedule). Only the agent (claude or codex), model, title,
  approvals, the tool grant, safe outputs, resume, worktree, `for` and `job_kind` cross (a grant is no wider than the remote tasks
  the target already takes); command, env, MCP, cwd, work, name, prompt and sandbox
  stay on the caller's side (`400`), and no agent session asks (`403`). The session runs in the project's checkout
  there (the main checkout of its most recent session in the project on that machine), shared, as that machine's
  owner starts it, capped to the project (section 1, rule 3; only owners start them, so the role cap is owner); a
  session or task it starts takes the same cap. The answer is the session once
  the authority has its presence row (at most 25 s; then `503`, and a retry with the same `client_request_id`
  answers the same start). Its events, turns, interrupts and answers go through the authority as for any shared
  structured session; it, and no other session (not even one it starts), may also be stopped from another machine (`POST
  /v0/sessions/{id}/stop`, relayed, a plain TERM: another `signal`, `escalate` or `force` is `400`; `zm stop` sends
  just that). A session nobody stops runs until its machine's owner stops it. Naming the caller's own machine is a
  local start (the cwd given, no cap), as for machine choice, and its `client_request_id` is ignored (a retry starts
  again; a local start answers `503` only before it starts anything).
- **Operator class**: each machine of a team project is `trusted` there (whoever runs it is trusted
  with the project's secrets, private code and a parent's authority) or `public` (a lender's or a stranger's:
  secret-free work only). The authority decides it, never the machine's own numbers: by default a machine whose
  live node-bound token for the project is itself a whole-project member's or more (the token's role and area,
  not the identity's other grants) is trusted, one that joined as a guest or for one area is public, and the
  authority's own machine is always trusted (setting it public is `400`). **An invite may carry the class**
  (`zm invite --class trusted|public`, `zm pool create --host`/`--public-host`): only the
  whole project's owner or admin makes one (an area's admin: `403`), and redemption writes it onto the node-bound
  token it mints, in the same transaction, so the machine has it the moment it joins and the owner need not
  wait. It comes only from the invite row: the redeem request has no field for it (an unknown field is `400`),
  and a retry from the redeeming node gets the same token and class. That class is then the machine's default
  (with several such tokens, `public` if any says so); `public` holds while the token is live; `trusted`, like the
  member default, only while its identity also keeps the grant the token is for (the same area), so revoking a
  grant never makes a machine more trusted; a later join from the same node (a GitHub join, SPEC 22) keeps the
  stricter class still counting, and removing the redeemed invite keeps it. The project's owner or admin sets it
  either way, or drops it back to the default (the invite's, else the role's); a set class holds for the node it
  was set for, so another node that later joins under the same machine name starts from the default, and starts queued for a machine that no
  heartbeat carried yet are refused when it becomes public (`POST /v0/team/machines/{machine}/class`;
  `GET /v0/team/machines/class` lists every machine's class and why; `zm machine-class`), from the authority's
  socket or a member machine's (sent on to the authority). **Where it is enforced**: work is placed only on
  trusted machines. A remote task (machine choice, agent or script run) to a named public machine is `403` with
  the command that trusts it in the hint, and `auto` never picks one; starting the Mezze app on a machine (below) is refused the same way.
  A run target on a public host is credential-free (section 20, "Host class"). The team module answers the class
  in process (`core.MachineClasses`) for anything else that places work, such as a sandbox pool.
  **A person's own machine**: a machine is a team machine (the default) or one person's own, a
  setting zm keeps at the authority beside the class (`POST /v0/team/machines/{machine}/person`, `zm machine-class
  <machine> person <name>|team`): only the person marks a machine as their own, a person who owns or administers
  the project, on a machine whose joins are theirs alone (run targets' `people`), never from a session that says it
  is one (`403`; a process of the person's OS user is not told apart, as for the class itself); only
  they clear it, unless they no longer own or administer the project (then any owner may); each write is one
  conditional statement. It is kept by the machine's name, so a new node under the name stays theirs. An action an
  ask carries on such a machine runs only on that person's signed yes (SPEC 16 "Actions that run something"); work
  started there directly (remote tasks, the Mezze app, session starts) is not gated by it yet.
- **Mezze app on a machine**: the project's owner starts
  (or reuses) a Mezze app server on another machine of a team project and gets a one-time pairing credential for it, with no
  ssh and no pairing URL: `POST /v0/team/machines/{machine}/t3` on the asking machine's socket (`zm t3 <machine> -P
  <project>`; the Mezze app brick calls it) goes to the project's authority, which queues a `t3_ensure` machine op on the same
  queue and waits up to 25 s for the answer. **Who may**: as for machine choice, an identity that owns or administers
  the project, asking from its own machine's socket (no parent session: a T3 client is not a zm session), and only onto
  a machine whose owner set a `"t3"` key in its config (shown as `t3_enabled` in its machine numbers; without it the
  machine refuses the op too). **The switch**: `"t3": {}` takes every default (`{"checkout"?, "commit"?, "home"?,
  "port"?, "node"?, "vp"?}`: checkout `<zm home>/t3code`, no commit, home `~/.z3`, port 3773, `node` and `vp` on the
  daemon's PATH); `zm t3 enable` on the machine writes it and `zm t3 disable` removes it, each restarting a running
  daemon through `zm daemon restart`'s swap, and refused from a zm session or with `ZM_HOST` (the machine owner's
  consent). The daemon reads the key once as it starts and fills an empty block's checkout into config.json then
  (`zm t3 enable` writes it filled), so zm's own config writes, which drop an empty block, keep it. **What the
  machine runs**: the server from a t3code checkout at the pinned commit, which must be on its `origin/main` (the
  boxes need the headless server, not a desktop build, and `t3 up` installs only published release binaries); with
  no commit, `origin/main`'s tip when it (re)starts the server (no pin is published yet), and a server zm started at
  the port and home is reused at whatever commit it runs. A missing checkout (no directory, or an empty one) is cloned
  from the t3code repository the build names (`internal/team/t3_repo.txt`; a build that names none refuses and asks
  for a clone) with the machine's own git credentials (never a prompt) into a sibling and moved in place; a directory
  that is not a checkout is refused. Before it prepares, git, node and vp must be there (a refusal names
  what to install; zm installs no system packages), and a clone or an install needs 8 GB free on the checkout's
  volume (a refusal states the free MB and the floor; machine numbers carry `disk_free_mb` of zm's home, the default
  checkout's volume). When the pin moved it fetches, checks out (a tree with local changes is refused) and runs `vp i`
  (again until one finishes at that commit); it starts `node apps/server/src/bin.ts serve --lan --port <port, 3773> --base-dir
  <home, ~/.z3>` in its own process group, which outlives the daemon, and records it (`t3.json` in zm's home). A server
  zm started at the pin, port and home and answering `/.well-known/t3/environment` on loopback is reused; one at another
  commit, port or home is replaced, stopped only once the checkout is ready (a pin that cannot be prepared leaves it
  running); a port another server answers is refused. Then it mints the credential with that server's own `auth
  pairing create --ttl 5m --label "zm: <asking machine>" [--scope S]... --json` (scopes T3 scope names; none: T3's
  standard client scopes; its stdout never goes into an error). The answer is `{machine, environment_id, version,
  commit, port, urls, started, credential, scopes, expires_at}`, the URLs `http://<ip>:<port>` for the machine's
  tailnet then private IPv4 addresses; the client spends the credential with T3's `connectPairing`, checking
  `environment_id`, and the session it gets is T3's (revoked in that server's Authorized clients, where it shows as
  `zm: <asking machine>`). **The secret**: the authority keeps a credential only while its requester waits, hands it
  out once and closes the op as the wait ends (answered, timed out or the requester gone), blanking its record; a
  report that comes later, or is resent after a lost ack, does not fill it again, and one for an op nobody waits for
  (the authority restarted) keeps no credential. The machine blanks its copy once a heartbeat answer took it, and
  drops one unsent after 10 minutes. An op no machine took in a minute is dropped. **Presence**: while that server
  answers on loopback the machine's numbers carry `t3: {environment_id, version, port}`, which joins it to a T3
  client's saved environment.
- **Usage** (all optional in the JSON; the Mezze app's Usage page reads them from `zm ls --all --json`): recorded when a session
  is spawned or first registered, never inferred later.
  - `for` (the identity it works for: a farm lane's dispatching owner, a standing task's owner; one line, at most 128
    bytes) and `job_kind` (`interactive`, `standing`, `farm` or `timer`): `zm run --for <identity> --job-kind
    <kind>`, `zm session register` the same, `POST /v0/sessions` and `POST /v0/tasks` `for` and `job_kind`, MCP
    `task_start` `for` and `job_kind`. The CLI and `zm mcp` fall back to `$ZM_FOR` and `$ZM_JOB_KIND`; a hook sends
    its own. A task without them takes its parent's. A managed session's child gets both in its environment, so what
    it starts inherits them. Another job kind is `400` from a spawner and dropped from a hook (hooks never fail).
  - `account`: a Claude session's login (`oauthAccount.emailAddress`) in `$CLAUDE_CONFIG_DIR/.claude.json`, else
    `~/.claude.json` (the file cswap rewrites on a switch), read once when the session is spawned or registered.
    The daemon reads the user's home only as `zm daemon` loads it; a daemon whose config names no home reads none.
  - `tokens` (`{input, cache_read, cache_write, output}`, `input` without cache reads or writes): the whole session's,
    from its own transcript: Claude's `<home>/projects/*/<runtime_id>.jsonl` plus `<runtime_id>/subagents/*.jsonl`,
    an assistant message counted once per message id and request id; Codex's
    `<home>/sessions/YYYY/MM/DD/rollout-*-<runtime_id>.jsonl` around its start day, adding each `token_count`
    event's `last_token_usage` (a re-emitted total or a zero context-size report is no request). `<home>` is the
    session's `$CLAUDE_CONFIG_DIR` or `$CODEX_HOME`, else `~/.claude` or `~/.codex`. The daemon reads every live
    claude or codex session with a runtime id every 3 minutes and once when it ends, each file on from where it
    stopped (whole lines only; a restarted daemon reads each once whole). A changed total is written to the row
    with no event and no `updated` change; readers and the next heartbeat carry it. A shell's bound agent
    (`runtime_agent`) is not counted.
  - Presence (17.3) carries all four for live and ended sessions; the authority keeps a field a beat leaves out
    (an older replica), and stores an ended session's later tokens (its final read) without an event, so `zm ls
    --all --json` on any machine shows other machines' sessions with them. Older daemons simply lack them.
- **Work** (`work`): the work item the session holds a lease on, else the last one it held: it stays after the
  release. Set by `--work` at spawn, by a claim on this machine's work module (`dev.zm.work.claimed` naming the
  session) and, on a replica, by the keys its heartbeat reports the session holds (a `lock:` key is not work); an
  authority's presence row keeps the last key a replica reported.
- **Script runs** (`zm exec`): `task_start` with `agent: "run"` and `run: {ref, script, args?,
  commit?}` runs a repository script instead of an agent (or `run: {ref, commit?, command}`, one line of at most 4 KiB
  and no script or args, which a fixed `/bin/sh` wrapper runs in the same checkout: an approved `command` action,
  SPEC 16; a machine whose zm predates it sees no script and refuses the run), and only through machine choice (no `machine` is
  `bad_request`; so are a prompt, model, `on_limit`, `continue_from`, `cwd` or `worktree: true`). Who may, choosing,
  idempotency, status, cancel and the per-parent cap are machine choice's, except that a run goes only to a machine
  whose numbers carry `script_runs` (its zm runs scripts: `auto` skips the others, and naming one is `unavailable`).
  SPEC 20's code rule applies: the target resolves `ref` (a branch, then a tag) by a live `git ls-remote` on its
  project checkout's `origin`, never a local branch. A machine with no checkout of the project whose `origin` is one of
  the project's listed remotes (any URL form) (none, a farm worktree without `origin`, or a session filed from another repository)
  uses a bare partial clone of the project's first listed remote (this machine's own rules, else a joined project's
  authority rules, which only its machine owner sets: no more than `remote_tasks` already trusts;
  `https://<host/owner/repo>.git`, with the machine's own git credentials), made once under `$ZM_HOME/run-mirrors/` and cloned again if damaged; a project that lists no remote is `bad_request` there; a sent `commit` that differs is refused (`forbidden`), unless
  `commit_on_ref` is set (only an approved ask's start may carry it, `bad_request` otherwise at the authority and on
  the machine: its commit zm saw was the ref's head when asked, SPEC 16): then the commit
  must be the ref's head or an ancestor of the head just fetched from the remote (`git merge-base --is-ancestor`; a
  history that cannot tell refuses too), else `forbidden` "is not on"; an unpushed ref is `bad_request`. `ref` is a
  branch or tag name, never `refs/...`, `-...`, `..` or a path, and `commit` is exactly 40 lower-case hex (a
  SHA-256 repository cannot carry a pinned run); git runs with no hooks, no `GIT_*` from the daemon and
  `GIT_NO_REPLACE_OBJECTS=1` and no graft file. It fetches the
  ref, makes a clean detached checkout of exactly that commit (the sent one, never a newer head) under `$ZM_HOME/runs/` (never the working tree), and
  runs `script` (a clean repository-relative path to an executable regular file inside the checkout; no shell
  string) with `args` as literal arguments, stdin empty, stderr into the output, and only SPEC 20's environment
  allowlist (`PATH, HOME, LANG, TERM, TZ`) plus zm's session variables. It is an ordinary task there (process group,
  deadline, cancel, redaction holds over its output) except that quiet output never ends it (only `timeout_s` or the
  machine's task deadline does). Only the script's own exit reports success: the outcome carries its exit code;
  a deadline is `killed` with exit 124; cancel is `killed`. The result's text is the output's last 4,000 bytes and
  `task_read` item `output` pages the whole output once the task ended. The checkout is removed when the task ends.
  Bindings, budgets and spend (SPEC 20's `[[run_targets]]`) are not involved: the target is the machine itself, and
  only the project's owner or admin can start one, as with an agent task (which can already run anything there).
  `zm exec` always sends a `client_request_id` (`--request-id`, else a fresh `exec-…`, printed first). A signal
  while its start is in flight (the authority may have queued it) sends the same start again (not a lookup: the
  first may still be on its way, and the resend is the same start whichever lands first) to learn the task, and
  cancels it. A resend with no answer in 10 s (it may have become the start, running the checkout on the
  authority's own machine) is followed by lookups for up to 30 s, and the run one finds is cancelled. A caller that may kill `zm exec` outright (SIGKILL, OOM) records its `--request-id`, and
  `zm exec --cancel <request-id>`, from the same session with the run's `--ref`, `--commit`, `--timeout`, script
  and arguments (`--machine` is not compared), sends the start with `lookup: true`: the authority answers the task
  that request made (a changed request is `conflict`) or `not_found` (none, or its record expired after 30 days),
  and never queues or runs one. It cancels that task and polls its status for at most 30 s until it is ended or
  refused; otherwise it says the cancel was not confirmed and exits 1. A cancel that reaches the machine while its
  start is still making the checkout ends that checkout, and the start is refused ("cancelled before it
  started") without running the script; one that arrives before its start is remembered for it (in memory, for
  20 minutes, at most 1024). A start for the authority's own machine takes its cancel the same way, and is
  recorded even when its request was cut. No route is added.
- **What it is doing, and whose it is** (all optional in the JSON, for UIs such as the Mezze app):
  - `activity` (one line, at most 120 characters, control characters stripped) and `activity_at`: the agent's
    latest step, from its hooks. Claude Code's `PreToolUse` hook (`zm hook claude tool`) reports it with state
    `running` in the one hook request; `UserPromptSubmit` reports `Thinking`; a hook reporting `idle` without an activity (Claude's `Stop` and `SessionStart`, an idle `Notification`, Codex
    `notify`, `zm session state <s> idle`) clears it, in the same write and event as the state (`updated` when the session was idle already); an
    idle no hook reported (the idle timer, mail) leaves it.
    A repeat of the same text is no write and no event, except that it refreshes `activity_at` once 30 s old (a
    row write, still no event).
    **Privacy rule**: activity goes to every reader of the session, teammates included, so it never carries a
    command, a prompt, a search pattern, a URL path or file contents. It is built from the tool's name and only
    these inputs: Bash its `description` (else `Running a command`), Edit/MultiEdit/Write/Read/NotebookEdit the
    file's base name (`Editing store.go`), Grep/Glob `Searching`, Task/Agent its `description`, WebFetch the host
    (`Web: go.dev`), WebSearch `Searching the web`, TodoWrite the in-progress todo's `activeForm`; any other tool
    its name. `zm hook` builds it (`internal/integrations/activity.go`); the daemon only clips it.
  - `term_title`: for a managed session, the latest terminal title its program set (OSC 0 or 2, ended by BEL or
    ST, split across reads or not), parsed from the PTY stream as it passes (no copy kept beyond the title being
    read), same limits as activity. Written at most once a second; the last one before exit is kept. **It stays on
    this machine** unless the session's agent is a known agent runtime (claude, codex, gemini, opencode) running
    that runtime's own binary (or under `srt`): any other program's titles (a shell's preexec or PROMPT_COMMAND,
    vim) carry command lines, paths and hosts, so presence never sends them and an authority drops one an older
    replica sends. The same title is shown to **no reader but this machine's owner** (scope all: the socket
    without a token): a token or tailnet caller (a teammate through a replica's proxied `GET /v0/sessions`, `GET
    /v0/sessions/{id}`) gets the session without it, and its events carry it as an owner-only key (section 12).
  - `fg` (`{pid, name}`), `busy`, `jobs` and `ports`, for a managed session on this machine: `fg` is the PTY's
    foreground process group leader (`tcgetpgrp` on the master) and its executable name (`zsh`, `claude`, `vim`,
    `npm`); `busy` is true when it is not the session's own process (a child runs in front: a build, vim under a
    shell); `jobs` counts the processes in the session's process tree other than its own (a shell's background `&`
    and suspended `^Z` jobs, which leave `busy` false because the shell holds the terminal, and an agent's helpers
    such as MCP servers), read with `fg` from the same process table and present, `0` included, whenever `fg` was
    sampled (absent: no sample, so a UI cannot tell and should ask). Like `ports`, `jobs` sees only descendants: a
    process that reparents itself away (a double fork, a subshell's `(cmd &)`, `nohup … &` under a shell that
    exits) is not counted. A zombie (exited, not yet reaped by its parent) is counted until it is reaped, which
    errs towards asking; so a UI may close a shell without asking only
    when it is neither `busy` nor has `jobs`; `ports` are the TCP ports in LISTEN state of any process in the
    session's process tree (descendants of its pid; a daemon that reparents itself away is not counted). A sampler
    reads `fg` at most every 2 s per session, and only while a client is attached or this machine's owner read the
    session by itself (`GET /v0/sessions/{id}`, not the list a UI reloads on every event) in the last 30 s; `ports`
    at most every 10 s, sooner when the session's process tree changed but never within 5 s of the last read (an
    agent's tree changes with every tool run), and after a failed read (an `lsof` timeout) only after 10 s whatever
    the tree does; never on a request's path (macOS: one
    `kern.proc.all` sysctl per cycle and one `lsof` run for all sessions whose ports are due, bounded at 1.5 s plus
    1 s for its pipes, a timeout keeping the previous ports; Linux: `/proc`). Measured on a loaded M3 (load 65-98),
    10 sessions: a cycle reading `fg` only 0.8 ms daemon CPU (about 9 ms wall); one also reading ports 2.7 ms plus
    about 22 ms of `lsof` CPU (about 86 ms wall). A change publishes the coalesced `updated`, except that a
    change of `jobs` alone publishes only when it crosses 0 and more (an agent's helpers come and go with every
    tool run; reads carry the exact count). When a session stops
    being sampled (nobody attached or asking), its `fg`, `busy` and `jobs` are dropped (they would be stale),
    `ports` kept. `GET /v0/sessions/{id}?sample=1` (the owner only; a UI about to close a session) samples that
    session's `fg`, `busy` and `jobs` before answering, its ports as they were; it is a one-off look: it does not
    make the session sampled afterwards and publishes no event (nor does dropping that sample later), and it never
    waits for a cycle's ports read. Like a
    shell's `term_title` they are **this machine's owner's only**: never in presence, stripped from reads by a
    token or tailnet caller and from events for any other reader (owner-only keys, section 12). Unmanaged sessions
    have none.
  - `who` (`{name, kind}`, `kind` human, machine or agent) and `reach` (`all`, `project` or `area`), resolved by
    the daemon that answers: a local session is its launcher's (the machine owner) with reach `all`; a replica's
    session at a team authority (17.3) is the identity its machine joined as, with that identity's kind, and
    reaches what the node-bound token is capped to (`project`, or `area`: the session's `area` says which).
  - A change of activity or term_title alone publishes `dev.zm.session.updated` (data: the Session); a hook that
    changes the state and the activity together publishes one `state_changed` carrying both. Nothing changed, no
    event. Every event is stored in its project's log (section 12 keeps the last 20 000), so `updated` is
    **coalesced per session**: at most one every 5 s; a change inside that gap publishes once, at its end, the
    session as it is then (the last change always goes out), and any other event of the session (which carries the
    whole Session) cancels that pending one. A move (below) is never held back. An `updated` whose only changes
    are the activity (`activity`, `activity_at`) is **live-only** (section 12): delivered to live subscribers,
    webhooks following live and a replica's presence loop, but not stored, so an agent's step-by-step activity
    never fills the replayable log; a client that reconnects reloads sessions anyway. An `updated` that also
    carries another change (a title, a sample, also coalesced with an activity) is stored, as are state changes.
    On a replica, a coalesced change reaches the authority with the heartbeat it triggers, so within about 5 s.

## 9. Mail

```json
{"id": "01JA0M2C9Q3T4V5W6X7Y8Z9A0B", "project": "01J9Z3K0QX7Y8AZ5V2N4B6C8D0", "area": "solver",
 "from": "mini/4bb7545a", "to": "bd:solver-3x", "recipient": "mini/9c0e11aa", "via": "holder of bd:solver-3x",
 "ref": "bd:solver-3x", "kind": "note", "needs": "any", "body": "PR #7 is up; can you review the migration?",
 "reply_to": "", "thread": "01JA0M2C9Q3T4V5W6X7Y8Z9A0B", "created": "2026-10-30T18:00:00Z", "acked_by": "",
 "acked_at": null}
```

Mail bodies, session display text, local work item titles and bodies, and work and native issue text and comments have hidden controls stripped on write, while valid emoji and script-shaping joiners are kept. CLI output and MCP untrusted results are cleaned on output too, including older stored text; REST reads may still return legacy stored bytes. Script shaping and valid emoji sequences are preserved.

Mail lives in its project's project.db. `kind`: `note` (default), `announce` (only to `all`), `nudge` (only to
`human:<name>` or `owners`: a rare, urgent note people's team chat shows; other agent mail stays out of it) or
`notice` (only the daemon sends one, section 10 "Active expiry") or `report` (below, "Reports"). `needs`:
`any` (default); `agent` (refused to a human); `human` (a decision only a person can make: only a human may
ack it, it is never surfaced inside an agent's terminal, and a session that sends it is `needs_human` until
it is acked or answered). `reply_to` must be mail the caller is a party to; `thread` is the id of the thread's
first mail (`GET /v0/mail/{id}/thread`). **Restricted mail** (to `owners`, or
`needs=human`) stays with those who can act on it: in an area the caller may only read (a guest), it is
listed, fetched, threaded and streamed only to its sender and recipient; other area mail is readable by
everyone in the area. The CLI accepts any unique id
suffix of 6+ characters (within the caller's scope).

**Personal audience (additive).** A direct human or session send may set `audience:"participants"`.
Personal composers must set it explicitly; omitted audience retains the existing area-mail contract.
The server records the authenticated sender and canonical recipient as exact participants. Project ownership,
operator status, machine ownership and a shared machine/session prefix do not substitute for participation.
Every list, by-ID/suffix read, thread, notice, snapshot, replica read and event requires both the normal scope
and exact participant address; hidden mail includes its IDs, counts and provenance. Replies inherit the
private audience and may address only existing participants. Private session mail never orphans into a group. Public session notes/events omit personal mail identifiers.
Outbox listings check the actual caller scope and persisted participant audience before limits, including
settled errors and metadata; replies retain that audience independently of their parent copy.
Node-bound replicas must declare `Zm-Personal-Mail: participants/1`; unaware replicas receive no personal
rows, IDs or events through reads, snapshots or change streams. Upgraded replicas send this declaration on
both ordinary and queued requests; the replica migration forces one fresh snapshot so previously withheld
personal records are recovered without losing queued writes. For a joined project, the local human reads
as the identity that joined that project, even when the socket's local human name differs; sessions keep
their own participant addresses. This mapping is per joined project, not a local identity rename or alias.
Downgrading a daemon against a database/copy that already contains private
mail is unsupported; verify the running daemon build before recovery or adoption.
Portable project exports filter personal rows/events and related mail metadata for the authenticated exporting
address, discard team delivery-response caches, and vacuum the disposable copy to remove deleted bytes.

**For you.** Personal mail is not a separate inbox: everything addressed to
a person is one item in that person's Decisions, of one of three kinds. *decide* is an ask (section 16).
*know* is mail to `human:<name>` that is not an ask (`for_you: "know"`): news with one action, Got it.
*digest* is the same sent with `digest: true` (`zm send --digest`, only to a person): `held_until` is the next
08:00 Pacific, and until then it is in no inbox, list of for-you items or push; after, it is a know item.
- **Private.** Mail to a person that is not an ask takes `audience: "participants"` without the sender asking
  (sender and person), so the personal-audience rules above hide it from everyone else, project owners
  included. An ask's mail keeps the restricted-mail rule. Sharing an item to Team chat is the person's own
  post (a client's one tap), never an agent's.
- **Read only by its person.** `POST /v0/mail/{id}/ack` on a for-you item is `403` for every caller (the
  sending session, the person on the socket, `zm ack`, MCP); a reply leaves it unread. Only Got it marks it
  read: `POST /v0/mail/{id}/got-it` from a Decisions client, accepted only from the person it is addressed
  to (never a session; anything the caller cannot see is `not_found`), or a paired device's `POST
  /v0/approvals/got-it` (for the device's person: its `--for` identity, else the machine owner; the device
  page lists that person's items as `for_you`). On a member Mac the owner's items in joined projects are
  listed and marked read live at each authority with that project's node token; it speaks
  only for the owner, so another person paired there gets none of them. No zm command calls
  either. Plainly: like owner answers (section 16), a process running as the owner that goes around the CLI
  to the socket could call the first; a paired device is the strong path.
- **Push.** A know item pushes like needs=human mail; a held digest item does not.
- hub forwards: `hub send <person>` sends zm mail to that person; `hub person` points at Decisions, and its
  read verb and the hook line telling sessions to announce personal mail are gone.

**Read marks.** A read mark is one person's "read up to here" in one conversation, kept in the
project's database so every device of that person shows the same unread (Team chat). The conversation key is
the client's (a project id, a `human:` address, a group id; 1 to 200 bytes) and zm never interprets it; the
mark is a time in ms on mail's `created` clock, clamped to now, and only moves forward (an older write leaves
it). The person is always the caller (`human:<name>`), never the request: a person reads and moves only their
own, a session has none. A read mark is not an ack: it takes no mail for anyone, changes no inbox, notice or
for-you item, and has no event, so a member's copy never holds one and every read asks the authority
(`GET`/`PUT /v0/mail/read-marks`, API.md).

**Retry-safe native operations.** Send and reply accept optional `client_request_id` (1..128 bytes when set).
A key is unique per authenticated identity/address and project across send/reply operations. The original
request, resolved area and reply parent are hash-bound before body cleaning. A changed payload/operation
conflicts; an identical retry returns the same native mail. The mail and key commit atomically; a team
authority also commits its delivery event in that transaction. Native delivery is the authority's mailbox,
not a separate external send. Replicas reserve a stable write ID and persist one pending outbox entry per
key; an interrupted reservation can be re-enqueued. A queued response is provisional delivery, not a human
read receipt. Parent acknowledgement is retried after an idempotent reply, without resending the reply.
Authority delivery caches bind both queued and live retries to the authenticated identity/address and node,
personal-mail capability, effective project scope, method, URI, base and payload. Replays return the original
complete result only while that evidence still matches, including ack/error bodies and deleted parents.
Changed scope conservatively holds the replay even when its particular target might remain readable.
Legacy cache rows without this evidence are retained for review, never redispatched or exposed; a held
queued result stops the batch without settling its pending operation. Live-to-queued retries adopt queued
retention. Transient reply/parent-ack failures remain uncached so the durable reply ID repairs the ack.
Private or keyed sends cannot use a solo-project external forwarder. No dual send or timeout transport switch.
Existing internal mail IDs remain a separate contract.

**Send limits.** Mail a client sends is counted where it is stored: the project's authority (a replica's writes,
live or queued, count there once; a replayed write or `client_request_id` is not counted again), so every door
(CLI, HTTP, MCP, a replica) has the same rules. Nothing reads meaning: counts and exact equality only. Three rules,
each a setting in the project's `project.toml` `[mail_limits]` at its authority (absent or 0: the default; a hand
edit takes a restart, as for every project.toml edit):
- **repeat** (`repeat_seconds`, default 600): the same mail again (the same sender, `to`, area, kind, ref,
  `reply_to` and body, byte for byte) to the same recipient is refused, naming the first.
- **pair** (`per_pair`, default 60 per `window_seconds`, default 600): mail between two addresses, both ways
  together, so a reply loop stops with both sides refused.
- **sender** (`per_sender`, default 1000 per window): one sender's mail. Fan-out reaches many recipients, so only
  this generous cap bounds it; the loop stopper is the pair rule.
Only agents are counted: sessions, machine identities and identities of kind `agent` (whose mailbox reads
`human:<name>`). Not counted: mail the daemon sends as itself (notices, triage, expiry reports, lease notes), an
ask's mail, and a person's (a human identity, or a tool sending as one; mail to several people included): a person
types, and a loop with an agent in it stops on the agent's side. Counts are per project. Mail zm sends on an
agent's behalf counts as the agent's. A refusal is `429 rate_limited` with `retry_after` (seconds until
one more fits) and `data` `{"limit": "repeat|pair|sender", "max"?, "window_s"}`; its message gives the wait in words
and says nothing was sent; nothing is stored, no event, and a refused reply leaves its parent open; a fresh send
names the recipient as its sender wrote it, a reply names none. A refusal is never recorded under its write id
(SPEC 17.6), so on a team replica a retry with the same `client_request_id` after `retry_after` is sent: the
authority does not replay it, a queued one settles `failed` in `zm team outbox` and tells its sending session
(17.6 "Writes") without holding the writes behind it, and the replica frees the key. The count lives in
`mail_sends` (sender, recipient, time and a digest;
no text), pruned within a minute of leaving the longest window and never exported.

**Body size.** A mail body has no limit of its own: the request carrying it is at most 1 MiB (`400` past that).
Imports have their own limits (below).

**Explicit legacy recovery.** Mail owns `core.LegacyMailImporter` and authenticated `POST /v0/mail/imports`.
The authenticated local owner human may import locally. An aware node-bound human may import to their own
canonical authority address through an existing joined project, requiring personal-mail capability and an
explicit project (flag/header or query, never cwd/environment discovery). Sessions, machine identities,
unbound bearer tokens, unaware replicas and mappings to any other person are refused. WriteTarget and current
project/area write checks still apply. A joined import forwards once with existing node authentication,
without replica sink/outbox, fallback, implicit grants or new projects. The finite artifact supplies a stable source installation
ID, mapping version, 1..100 exact source IDs, explicit source-project/recipient mappings to that current person,
and 1..100 full-body records. There are no inferred sender aliases, project mappings, grants or private projects.
Before serving a LIVE cached import response, the authority resolves and authorizes the actual selected
project/area again. Import replay evidence binds the canonical project ID and area, permitting equivalent
slug/ID and query/header selections; the explicit query takes precedence. Changed destinations must reject
or hold the completed write, never redispatch it. Older import cache proofs lacking destination evidence
remain held; a new write ID still uses the authoritative immutable import ledger for safe artifact recovery.
Unknown mappings, missing IDs, invalid records and immutable-content conflicts are reported individually.
A batch is at most 1 MiB, each body at most 256 KiB, and each source-read or sink phase is limited to 30 seconds (the CLI caps the combined network phases at 60 seconds). No silent truncation.

`zm mail-import artifact.json` defaults to dry-run; only `--apply` writes native mail. Optional `--read-hub`
with an explicit HTTPS `/mail` endpoint and `--hub-token-env` performs finite read-only `GET /mail?id=N`
requests for exactly the manifest's IDs. It rejects missing/mismatched body lengths, previews, redirects,
non-HTTPS endpoints, oversized responses and missing full messages. Source project comes from the explicit
manifest, since the legacy feed has no canonical zm project. The reader never uses unread/session inbox
filters, never writes to the source, and never reads or persists credentials implicitly.

Uniqueness is `(server-derived importer Identity.ID, source_instance, record_kind, source_id)` in a mail-owned registry reservation with immutable
payload/destination hash and stable native ID. The registry reservation uses synchronous FULL before the
project sink may commit, so power loss cannot retain a sink while losing its mapping reservation. The same
person's devices share the stable identity namespace; another human cannot inspect or poison its tuples.
A second destination for that person cannot create a duplicate. In the project,
mail plus its import sink/hash commit atomically (and the team event where applicable). After interruption,
retry the identical retained artifact to finish a reserved sink; never advance an inferred source checkpoint.
No automatic remapping, source revisions/metadata updates, continuous sync or automatic thread joins are
implemented. Source parent/thread IDs remain provenance only. The same record with changed metadata conflicts
rather than resetting native read state. Dry-run reserves nothing and checks the same immutable ledger.

An additive migration retains the previous ownerless reservation journal and native sinks. Completed old rows
are adopted only with exact server-owned sink/provenance evidence and unchanged authority-local owner
identity predating the reserved native ID. Changed owners and reservation-only unfinished operations have
no trusted continuity anchor: they remain held for explicit local reconciliation, without guessed ownership
or automatic replay. Any ambiguous old row uniformly holds valid imports with a generic unavailable error;
authentication is checked first, and responses do not reveal whose source tuple exists. This conservative
availability limit prevents duplicate recovery after an owner change. Dry-run never writes migration state.

Imported `from` is the actual authenticated importer, never the source sender. `origin` labels the source
system/instance/ID, source sender/recipient/kind/ref/parent/thread, original sent time, import time and actor,
with `trust:"source_reported"`; `legacy_delivery` preserves source scheduling/ack/delivery times separately.
Imports emit one `dev.zm.mail.imported` event after durable commit and create native unread mail with only the mapped current person as participant, no human receipt,
session state transition, nudge, autoreply or source ack. `acked_by=digest`, `acked_at=sent_at`, null digest time
and zero deliveries normalize to source `digest_held`, never human-read. `reply_unavailable` is
`legacy_return_route_unverified`; both ordinary reply and send-with-reply_to refuse imported records.
No immutable verified return route or legacy egress transport exists in this version. Local query/inbox and
replica ordering use effective created time (original source sent time for imports), then stable ID; threads
are oldest first. Existing native creation timestamps retain their meaning. Session-like provenance addresses
undergo native redaction for scoped readers; unverified legacy names never authenticate a person.

**Routing** (`POST /v0/mail`), inside the resolved project P, in order. Routing reads only `to` and the
explicit `ref`; **mail bodies are never scanned**.

1. `all` (kind `announce` only): stored once; every session of P sees it once (notice + inbox); never acked. `GET /v0/mail/inbox/peek` (`zm inbox --peek`, a hook's poll) lists it without marking it seen.
2. `owners`: P's owner group mailbox; `needs` defaults to `human`; in the inbox of every identity that owns P.
3. `human:<name>`: that identity's mailbox. The identity must have a grant the sender can see in P (else
   `not_found`). `needs` defaults to `human`. Never typed into or shown inside an agent session. It is a
   **for-you item** (below): private, and read only by that person's Got it.
   **Several people**, `human:<a>,human:<b>` (2 to 8 besides the sender, deduped; only a person sends it): one
   copy per person, each exactly the mail that person alone would get (private to sender and person, its own
   Got it), plus `members` (sender and recipients, sorted) and `batch` (the first copy's id, on every copy);
   fresh copies take that id as their `thread`. Every copy is checked before any is stored: one refusal sends
   nothing. `client_request_id` is kept per copy as `<id>/<recipient>`. A reply to a copy goes to every other
   member: the same `members`, a fresh `batch`, the parent's thread.
4. Session `<m>/<s8>`: must be on this machine (cross-machine mail is out of v0 for solo projects: `not_found`;
   a team project's authority also accepts its replicas' sessions, 17.3) and visible to the sender. A session
   registered in another project, or in `none`, is reached too (**across projects**, below). Filed in the
   session's area unless the sender named one or a `ref` did (a member mailing a guest's session from the repo root
   would otherwise file it at project level, where the guest can never read it). An ended session still receives;
   unacked after 30 minutes it is copied (`via: "orphan of <addr>"`) to the live session holding its `ref`, when
   the mail is filed in the item's area and that session is neither the ended one nor the sender
   (mail about work reaches whoever does it now). Mail about a work item that nobody live holds at all (filed in
   the item's area; not restricted mail) is not copied: zm adds it to the item as a comment (as the system: the
   mail's short id, its time and its body quoted as data, no session address; the mail's id as the write id), and
   in one transaction takes the mail as the machine and opens one triage item about the item unless one is open
   (`via: "nobody holds it"`, from the system, to the machine's triage agent, below, else its group mailbox; one
   per item, not per mail; an orphaned one does not count). An item that takes no comment (gone, refused, now in
   another project or area; asked again after an hour) and an item held by someone the mail cannot go to (its
   sender, a person) keep the old route. Other mail goes to its
   machine's group mailbox in P,
   except an ask notice (zm's word on an ask's outcome to its asker or the item's holder: the answer, a withdrawal, a
   default applied or undone; stored `ask_notice`): the outcome is already in the ask's record (and on the work item),
   and a machine has nobody to act on it or the context to revert a default, so it stays on the session, and
   after 24 hours (`NoticeKeep`) the machine takes it (`acked_by` `machine:<m>`; the daily summary counts it).
5. Work item `<source>:<ref>` in P: the session or person holding its live lease (`holder of <id>`), else its
   creator (`creator of <id>`), else refused (`conflict`, hint `zm who <id>`). Mail never starts work. `ref`
   defaults to the id; the mail's area is the item's. A tracker owner or creator session that has ended, or is
   gone from the registry (at an authority: from its machine's presence), is not mailed: a session of this
   machine whose name is now held by a live session of the item's project that can read its area hands it to
   that successor (via `... (successor of an ended session)`); otherwise it goes to the group mailbox of the
   session's machine (via `... (that session has ended)`). A registry that does not answer keeps the address.
6. `machine:<m>` (m = this machine): if `ref` is held by a session on m, to that session; else P's group
   mailbox on m: every session of P on m sees it in notice until one acks it (the first ack wins and records
   the taker; later ackers get `{"acked": false, "acked_by": <taker or "someone">}`); it wakes one idle managed
   session of P on m (nudge). Only sessions registered in P count: an address the daemon has no row for
   (hooks not installed) or a session in project `none` gets direct mail only, never group mail or
   announcements, and cannot ack them (`forbidden`, hint: install the hooks / `zm run` in a project).
   Group mail nobody takes in 24 hours (`GroupKeep`; a note, not `needs=human`, which the machine's human reads)
   expires (in practice: 54 unread in one machine's mailbox, the oldest 9 days): the sweep first puts mail about a work item on the item (a comment and one triage item, as under Triage); item
   mail the item refuses, and so reaches nobody, is named in one report to `owners` (short id, ref, first line; at most 20).
   It then takes each as `machine:<m>` (which settles an orphaned original too); other mail expires with no report. The daily summary counts
   what expired. The mail stays readable (`zm thread <id>`).
   **Triage** (mail to ended sessions was 12% of all mail in practice). Before that, group
   mail nobody takes in P's wait (project.toml `[mail_triage]` `wait_minutes`, default 120) moves on, and zm takes
   the group copy as the machine in the same transaction as the move (a session that took it first wins; nothing
   moves), so it leaves every inbox: mail about a work item goes to the item's live holder,
   else onto the item with one triage item (as above); other mail goes, as fresh mail of the same sender, thread
   and ref (`via: "untaken group mail of machine:<m>"`), to m's triage agent (`machines = { <m> = "<name>" }`,
   a session name there or an address), else P's (`agent = "<machine>/<name>"` or an address). A name resolves
   only on the daemon's own machine (8 "Names"), so another machine's name is refused (`bad_request`: give its
   address). The agent must be a live session registered in P that reads the mail's area; one that is
   not live counts as unset. With none, the mail stays and expires as above, with no report (the daemon logs the machines). Set with `zm project triage [--machine <m>]
   [--agent <ref>] [--wait <min>]` (`GET`/`PUT /v0/mail/triage`, a project admin; live). The triage agent reads
   the mail as data, as any agent does: it decides, the mail never instructs it.

| situation | send to |
| --- | --- |
| someone must act and you know who | the session, or the work item (reaches its holder) |
| someone must act and no owner can be found | `machine:<m>` (the project's group mailbox) |
| every agent must change what it does now | `all` with `--kind announce` |
| nobody needs to act; it is a record | no mail: `zm work comment <id>` (the item's history) |
| a decision only a person can make | an ask: `zm ask new` (section 16), never mail |
| a person should be able to read it later | `human:<name>` or `owners` with `--kind report` |
| a person must know now (rare: a P0, nothing else) | `owners` or `human:<name>` with `--kind nudge` |

- **Across projects**: a session is registered in one project (or `none`), but mail to it lands in the project the
  sender resolved, P, never in the session's own. Mail to session S in P is accepted when:
  1. S is registered in P: rule 4 (S's area decides who may address it; filed in S's area);
  2. S is registered elsewhere (another project or `none`) and S's machine takes P's mail: this machine, when the
     sender reads the **whole** of P and may see S where it is registered (its project and area there; S in `none`
     always), or, at a team authority, a replica joined to P whose latest heartbeat for P listed S (live, 17.3 "As
     built (round B2)": `elsewhere`), when the sender reads the whole of P, or, for a replica that reads only some
     areas of P (a guest's), one of those areas. What the replica reads is its token's scope at the send, never at
     the heartbeat. It is filed in the sender's area (or the ref's), else at project level; when the replica reads
     only some areas of P, in the one area it shares with the sender, else the mail must name one
     (`bad_request`, hint `--area`).
  Anything else answers exactly like a missing session (`not_found` naming the sender's input): an area-only reader
  (a guest) never addresses a session outside its own areas, an address unknown to P's daemon is refused, not
  stored, and so is fresh mail to a replica's session its heartbeats stopped listing (it ended). Authz is unchanged: the sender needs `write` where the mail lands, and the mail stays scoped to P (S reads it
  as its identity, the owner's or its replica's token for P, through its inbox: direct mail only, never P's group
  mailbox or announcements, as for any session not registered in P). A name in `to` (8 "Names") is resolved to S's
  address before any of this, and the mail stores the address. The send answer shows S's address (the one stored,
  whether the sender typed it or S's name); later reads redact it as usual (`someone` to a reader who cannot see S's own project, unless 17.3 B1 shows
  replica addresses to whole-project readers). **A reply always reaches the sender** of the mail it answers: `POST
  /v0/mail/{id}/reply` delivers to the parent's sender whatever project that session is in now, and even if it ended
  or P's daemon never knew it; it is filed in the parent's area.
- **Notice** (`GET /v0/mail/notice`): one line of at most `orchapi.MaxNoticeLen` (160) bytes, or `""`, describing
  unread mail: `zm: 2 unread (1 group) from mini/9c0e11aa, human:ana. Run: zm inbox`. Never a
  body; never an error (no project, no daemon: the CLI prints nothing and exits 0). A tracker whose mirror syncs
  have kept failing for 5 minutes is named too, to readers of its whole project only (from memory, no tracker
  call): alone `zm: mirror harbor/bd failing for 1h12m (37 syncs). Run: zm tracker ls`, or after the mail part
  (`... Run: zm inbox | mirror harbor/bd failing ...`, shortened or left out when it would pass the limit).
  Agent surfaces (`zm notice`, prompt/session-start hooks and completed MCP tools) use `?once=1` or the hook
  route's equivalent: each mail event is announced at most once per session address, across surfaces and daemon
  restarts. The line counts fresh unread events, names only the newest direct sender and visible work ref
  (or mail suffix), and includes `Run: zm inbox`. Group mail and announcements contribute counts only; no body,
  title or subject is injected. `unread` remains the full unread snapshot even when `line` is empty. Delivery
  records are local, never acknowledgements or announcement inbox receipts; mail stays available through
  `zm inbox`. Concurrent polls share delivery records. A lost response can lose a notice, never the mail.
  Ready team copies supply scoped/redacted mail metadata for the same per-mail deduplication. Before a copy
  is ready, each distinct cached heartbeat summary is announced once as a team count without sender/ref details
  or a network call (direct mail uses the placeholder `someone`); exact per-mail deduplication in that fallback
  requires peer mail ids.
  Persistent tracker failures are announced once per failure streak, after fresh mail, rather than on every
  prompt as the duration grows. Non-session callers keep the ordinary unread snapshot.
- **Reports: people take decisions only**. Mail from an agent (a session or
  a `machine:` address, the daemon's own included) that would reach a person (routed to `human:<name>` or
  `owners`, or `needs=human`, which the machine's human reads for its sessions and group mailbox) is filed as
  `kind: "report"` with `needs: "human"`, unless it is an ask's own mail (section 16) or a `nudge`. A reply is
  mail like any other, and `digest` is dropped (a report waits for nobody's 08:00). A client's own fresh note
  (`POST /v0/mail`, kind `note`, not an ask's mail, not a `reply_to` answering its parent's other party) from an agent that would reach a person is
  refused instead, `400` naming `zm ask new`, and nothing is stored (a question sent as mail sat unseen in
  reports); a replica refuses it before queueing, judging a work item's recipient as the authority does. zm's own in-process mail and replies are still filed. Anyone may send `--kind
  report` to a person on purpose (a digest job or an FYI does); a report to anyone else is `bad_request`. A
  report is stored, threaded and readable (`GET /v0/mail`, `/v0/mail/{id}`, events) like the mail it replaced,
  and one to `human:<name>` is private to its participants like a for-you item; but it is never in an inbox,
  Decisions ("For you" lists none), a push, the notice count, a hook's line or a nudge, never puts its sender in
  `needs_human`, and no session's inbox or notice ever lists it. Its person reads it with `GET /v0/mail/inbox?box=reports` (`zm
  inbox --reports`; the same mailboxes as the inbox, unread unless `all=1`) and marks it read with `ack` (only a
  human may). The send answer carries `note` (`orchapi.ReportNote`: people take decisions only, use `zm ask new`,
  this was filed as a report), which the CLI prints. Mail between people, and a person's mail to agents (an
  `OWNER DECISION` receipt included), is unchanged. A team replica's queued answer applies the same rule; the
  authority's answer is what is stored.
- **No notice about what you just did.** The approvals module's in-process answer to an ask's mail (the
  `OWNER DECISION` receipt, "withdrawn by ...", an applied, dropped or undone default) goes only to an agent that asked, and not when that agent
  withdrew the ask itself. Otherwise it takes the ask's mail without sending anything (`Silent`): a person who
  asked sees the decision in Decisions and gets no notice from zm (at a team authority the answer is sent as the
  authority's own human, so it would reach the asker as mail between people).
- **Asks are answered by decisions** (section 16): `ack` and `reply` over HTTP on the mail of an ask nobody
  has decided are `forbidden` for every caller (socket, tokens, the asker), after the usual visibility check (a
  caller who cannot see the mail gets `not_found`). Only the approvals module's in-process reply, after the
  decision, takes it; otherwise the asking session would leave `needs_human` with nothing decided.
- **Nudge**: when mail reaches an idle managed session nobody can be typing in, the daemon types the fixed line
  `zm: you have mail. Run: zm inbox` + Enter. Nothing else is ever typed for mail. It never types into or submits a
  person's text, so it holds back while either is true: a client that can type is attached (read-only viewers don't
  count), or keys have gone in since the last plain Enter (`\`-Enter, alt-Enter, ctrl-J and shift-Enter
  are not a submit; an answer to a hook's permission prompt is not a draft; the versioned input knowledge and parser context survive a daemon restart).
  New sessions begin with durable empty proof; adopted sessions without readable versioned proof stay conservative
  unknown, including pre-tracking upgrades. Ordered human input must persist unknown knowledge before PTY writes;
  failure refuses those bytes. Only a proven ordered plain submission establishes empty input again.
  Prompt hooks carry no input sequence and may arrive after a newer draft, so they never clear the mark. Encoded Enter
  and other ambiguous keys conservatively hold PTY nudges until a later plain submit; mail remains available through notice. The
  keystrokes are read as a person's the same way as for `needs_human` (section 8). Held-back mail stays in the inbox;
  the session sees it via notice on its next turn, like a busy one. When the last client that can type detaches, no
  keys are waiting and unread mail names that session, the daemon nudges then. Group mail goes to the next idle session
  nobody has open. Typing a line and its Enter holds the session's input, so a key arriving meanwhile goes after them.
- **Push**: clients learn about mail from `GET /v0/events` (`dev.zm.mail.sent`), never by polling.

## 10. Work items and leases

```json
{"id": "local:k3x9qa", "project": "01J9Z3K0QX7Y8AZ5V2N4B6C8D0", "area": "solver", "source": "local",
 "title": "Add the down migration", "status": "claimed", "owner": "mini/4bb7545a", "created_by": "human:ana",
 "labels": ["v0"], "links": [{"rel": "pr", "target": "https://github.com/example-org/harbor/pull/9"}],
 "created": "2026-10-30T17:00:00Z", "updated": "2026-10-30T18:00:00Z"}
```

- Ids are `<source>:<ref>`, unique within a project. `local:` items and their comments live in project.db;
  other prefixes belong to the project's trackers (project.toml `[[trackers]]`). Adapter items get their area
  from the area `work` globs, else the tracker's `area`. Bare native issue ids also resolve through the project's native
  work source on work-item routes and `run --work`; replies and leases keep the canonical source-prefixed id.
  Bare ids never guess an external tracker or a local item, and project/area visibility still applies.
- `status`: `open | claimed | in_review | done`. `links[].rel`: `pr, blocks, blocked_by, parent, child,
  session, related, url`.
- Records go to the item's history (`zm work comment`), not mail.
- **Tracker mirror.** Reads never wait on a tracker: each project's project.db holds a mirror of its trackers'
  items (`work_mirror`: id, area, status, owner, title, body, labels, updated, the rest as JSON), and every
  read of a tracker item (`work ls/show/who/sources`, locate for mail, leases and links, the area-by-label
  lookup) is served from it plus zm's own leases. The row's area is computed from the area rules when it
  is written and again (once, before the next read) whenever those rules change. A background syncer per
  tracker keeps it current: on daemon start (or the first use of a new binding) a full listing, written page
  by page as it comes; then an incremental sync by cursor every 5 s (at least the adapter's `poll_seconds`)
  and at once after any zm write; a full listing again when the tracker's `count` differs from the
  mirror's (at most every 10 min when the mirror holds more, i.e. deletions; every 6 h when it holds fewer),
  when the adapter refuses the cursor, and every 24 h. So an item deleted in the tracker can be served for
  up to about 10 min plus the listing, and one the incremental syncs missed for up to 6 h. A full listing
  drops the rows it did not return, unless a page of it said `partial`. Adapters without `changes` are
  listed in full (`list` not done + `list` done) every 30 s, dropping nothing when a list was cut at its
  limit; so is (by `changes`) one whose full listing gave no cursor. Writes stay write-through (the tracker
  arbitrates) and the tracker's answer is written to the mirror at once; the next sync corrects it. A change
  seen by an incremental sync publishes `dev.zm.work.changed`. Another adapter or config for a prefix is
  another tracker: its mirror starts over; an unbound tracker's mirror is deleted.
- `GET /v0/work` returns `{"items", "skipped", "behind"}`: `skipped` names each tracker with nothing to serve
  (its first sync failed; `{project, source, error}`, sanitised); `behind` names each tracker served from a
  mirror that is not current (`state` `loading`: the first sync is still running, items may be missing;
  `stale`: the last sync failed or is overdue, items are as of `synced`; with the sanitised `error`).
  `zm work ls` warns on stderr for both; `--source` naming a failing tracker is an error; `zm work
  export` refuses while a tracker is skipped or loading. A partial list never looks complete. Tracker items
  without `--status` exclude `done`, like the trackers' own lists. An item from a stale mirror carries
  `stale` (why, and as of when). Before the first full sync an id the caller might see answers `unavailable`
  (the same for hidden and missing ids); after it, an id not in the mirror is `not_found`.
- `GET /v0/work/sources` reports each tracker's mirror from its syncer, asking no tracker: `ok` only when
  current, else `error`; `sync` = `{state, synced, lag_seconds, full_sync, error, items, mode, serve}` (`items`
  for whole-project readers; `mode` and `serve` for local callers). On a replica a joined project is one
  `authority` row for its copy (17.6), with `items` the copy's count: what the replica's token reads (the whole
  project, or its area for an area invite), so nothing hidden is counted.
- `GET /v0/work/{id}` includes the item's `comments`. A cross-project link carries the target's `project`.
  Work events carry the item without `links` or `comments` (a link target may be in an area a subscriber
  cannot read); readers fetch the item.
- `set_status claimed` on a local item is refused: claiming is what makes it claimed.
- A claim, release, expiry or ticker repair that moves an item's status publishes `dev.zm.work.status_changed`
  with the item, after the lease event, so subscribers never infer status from leases. A local item publishes
  exactly when it moves (`open <-> claimed`). A tracker item's mirror may be stale or missing, so its claim,
  release and expiry publish the adapter's answer every time (a renewal only when it differs from the mirror):
  a subscriber may see a status it already has, never miss a move.

**Leases** (registry.db): an atomic claim with a server-enforced deadline on `(project, area, key)`, where key
is a work item id (area = the item's area, so effectively unique per project; the lease follows the item
when its area changes, by an area rule or its labels, and is found by id whatever area it was claimed in) or any string
(`lock:migrations`, area = the resolved write area), held by an exact address. Area is part of the key so a
lock in a hidden area neither conflicts with nor reveals itself to a caller in another area. A
**project-level** lease (area `""`) covers every area: it conflicts with another holder's live lease on the
same key in any area, and an area lease conflicts with a project-level one (holder redacted as usual).

- `claim` succeeds if the key is free, expired, or held by the same holder (renewal moves the deadline to
  now + ttl). `renew: true` (`zm work renew`, `zm lease --renew`) only extends a lease you hold. Otherwise `conflict` with the current lease in `error.data`, holder redacted. Default TTL 3600 s,
  max 7 days. Keys starting `db:`, `run:` or `box:` are reserved for run targets (section 20): only their `acquire`
  creates them, and the lease verbs here refuse them.
- **Fencing**: every lease carries a `fence`, its fencing token. A claim that takes a key (free, expired or another
  holder's) gets a new one, larger than every fence the project handed out before, by a random step (1 to 65536: a
  reader sees roughly how many claims the project had, never the exact count or which; each lease file keeps the
  project's last one in `lease_fences`, and `zm project team|solo` carries it with the rows); a renewal keeps it,
  and a lease from before fences (`fence` 0) gets one when it is next claimed (the ticker's renewals of a bound lease
  leave it 0 until its holder claims again). A write or release made under a lease
  sends the fence back (`fence` on claim (a claim naming one only renews that lease, never takes the key), release,
  comment and status requests, and on the native door's update, close, reopen, comment and claim): it stands only
  while the caller's own lease with that fence is the item's live lease (a release: the caller's lease with that
  fence, even past its deadline before the ticker drops it; a forced release: that lease, whoever holds it), else
  `409 conflict` with `error.data` `{"reason": "stale-lease"}`, naming nothing about who holds the key now, and
  nothing is written. So a holder that paused past its deadline, or whose key was taken (by another holder or by
  itself again), cannot write under or release the newer lease; a fenced release of a key nobody holds answers as
  an unfenced one. The check is made when the write is served: in the write's transaction for a team project's
  local items, where the lease and the item share a file; elsewhere just before the write, so a lease that lapses
  in between still lets that one write through. Through a team replica a fenced comment or status is checked at
  the authority when its queued write arrives; a refusal then is the outbox's (17.6). A hidden or missing item answers `not_found` before any fence is
  looked at. No fence sent: as before (fencing is opt-in per write). `zm claim`/`zm lease` print the fence and
  `zm renew`, `zm release`, `zm lease --renew|--release`, `zm work comment` and `zm work status` take `--fence`;
  `zm mcp` remembers the fence its `work_claim` took and sends it on that agent's `work_renew` and `work_release`
  of the key (a release forgets it, and so does a `stale-lease` answer: told once), so an MCP agent's renewal never
  quietly takes a lost key again and its late release never drops a newer lease; its comments and statuses go
  unfenced (a comment needs no lease, and on a team replica they are queued, so a refusal would reach nobody); bd's
  door answers a lease-backed `update --claim --json` with `lease_fence` and takes `--fence` on `update`, `close`,
  `reopen` and `comment(s) add`.
- **Bound leases**: a session's lease: spawn `--work`, or a claim made by a live managed or hook session
  (an agent zm did not start: cmux, a plain terminal) asking no `ttl_seconds` of its own (bd's claims ask none).
  A claim that names `ttl_seconds` (any session's, even renewing a bound lease: `zm lease lock:x --ttl 60`)
  is a TTL lease, and the lease records that its TTL was named. A bound lease has a real deadline like every
  lease, but the **daemon** renews it, from session liveness, not the agent:
  on each tick, holder `running` -> `expires = now + bound_lease_ttl_seconds` (default 3600); `idle` or
  `needs_human` -> not renewed (run-target leases excepted, section 20), so a holder stuck there longer than the TTL expires; `ended` or
  gone -> released at once (`released`, note `session ended`; a run-target lease instead stops renewing and runs
  to its `expires_at`, section 20). A managed session with no PTY output for
  `idle_after_seconds` (300) is set `idle`, so a hung agent stops renewing. A running hook session that no hook
  has reached for `bound_lease_ttl_seconds` is set `idle` (note `no hooks`; the next hook runs it again), its
  session-end hook ends it (Claude's `SessionEnd`; Codex's too since `zm install codex` adds it, with `PreToolUse`
  and `PostToolUse` so a long Codex turn stays `running`), and one registered with a pid that is gone is ended
  within 15 s (outcome `failed`, evidence `process gone`). Every hook sends its agent's pid: the nearest ancestor that looks
  like the agent by command name, executable or argv[0] (`codex*`, `claude*`, a versioned install such as
  `.../claude/versions/2.1.316`, or node or bun whose script is the agent's package or bin), past shells and
  wrappers, else 0 (an orphaned hook: Codex runs notify without waiting). The daemon records the process's start
  with it and takes a pid only from a live process (never 1) that started no later than the session (or, reviving
  it, this hook), never over a recorded pid whose process still runs. A pid now held by another process, or a
  zombie, counts as gone. An ended session revives on a hook from a live agent; a hook naming none revives it while
  the recorded agent runs, when none was recorded, or when it reports work (`running`, `needs_human`; the dead
  pid is then dropped), so a late idle notify from an exited agent revives nothing.
  A live session an older daemon recorded with a pid but no start gets its start filled in (ended when its pid
  names a process that started after the session). A hook session with no pid that no hook has reached for twice
  `bound_lease_ttl_seconds`, counted from the store (a `no hooks` idle counts from its last hook), is ended
  (evidence `no hook for ... and no pid to watch`): its leases lapsed long before, and its next hook revives it
  under the same address. So a
  dead agent never renews for more than about one bound TTL, and never stays in lists or presence. Hooks
  themselves renew nothing and write nothing: the ticker reads the session's state. A session store that does not
  answer is not an ended session: the tick keeps the lease and decides at the next one. On a team authority a member's session is its presence (17.3):
  `running` while the member's heartbeats say so, `idle` once they are 60 s stale (so a silent member's leases lapse
  one bound TTL later), and `ended` the heartbeat after it ends there, which releases at once. An authority keeps one
  presence row per session, so a member sends each live session in exactly one of the heartbeats it sends to that
  authority: its own project's when that project is one the authority serves, else (a session registered in a
  project of another authority or of none, that claimed naming a project here) the first project here, by id, it
  holds keys in, while that project's answer says it holds one; the other heartbeats to that authority carry it as a
  session entry marked `elsewhere` (address only; apart from 17.3's `elsewhere` list of addresses for mail, which it
  is also on when registered outside that project), so none of them takes it for gone and its row moves between projects without ever
  passing through `ended` (which would release its bound leases everywhere). One row is enough: the ticker renews a
  session's bound leases in every project of that authority from it, and the row's `work` names only a key of the
  row's own project. Each project's answer confirms only that project's keys
  (the lost-lease check); keys in the others are not checked until the session is present in their project. A lease whose TTL was not named and that a
  session the ticker finds running holds is made bound then (one claimed before the member's first heartbeat
  reached the authority, or before this rule). TTL leases are for an explicit
  `ttl_seconds`, humans and remote callers; they renew by claiming again, never from hooks.
- **Active expiry**: a ticker in the daemon (every `lease_tick_seconds`, default 30) renews bound leases as
  above, then expires every lease (TTL or bound) past its deadline: event `dev.zm.work.lease_expired`, whose
  mentions carry the holder, redacted per subscriber, and a mail of kind
  `notice`, `needs=agent`, from `machine:<m>` to the holder when it is a session that has not ended, filed in the
  lease's area (ref = the key when it is a work item of the project; a plain lock such as `lock:migrations` is named in
  the body). Only the daemon sends a `notice`: it shows at the holder's next prompt, wakes nobody, is never copied to a
  group mailbox, refuses a reply (ack it), and settles itself after a day. No person is mailed for an expiry: the item is released as any
  lease's end releases it, so it shows as open once its tracker takes the release. Daemons before this rule
  mailed the owners `needs=human` ("zm: the lease on <key> ... expired at ..."); the mail sweep acks each such
  open note (from `machine:*`, to `owners`, that body opening) at once, taker the sweeping daemon's `machine:<m>`,
  as it does the CI results the GitHub link once mailed a person (18.2); the daily summary counts both,
  and the expiry notices of the day ("Native project summaries"). An expired lease
  frees the key; the old holder must claim again. There is no eternal "in progress": every lease, list and
  inflight row shows its effective `expires`.
- Only the holder releases. `--force` needs `operate` on the project and the local socket.
- Claiming a tracker item also calls the adapter's `claim` verb (the adapter arbitrates; `conflict` refuses);
  the local lease row is still recorded so it can expire. Releasing calls `release`. A `local:` item
  moves `open -> claimed` on claim and back to `open` on release unless it moved on.
- **Rollup** (`GET /v0/work/rollup`): counts per theme and epic, one rollup per project in scope, across every tracker
  and local items, from the mirror (on a replica, from the copy, with no tailnet call). It reads a tracker's optional
  `extra` fields (section 11), so a tracker that reports none still gets its counts, all under `no_epic`. An **epic** is
  an item with `extra.issue_type = epic` that is not done. A **theme** is an epic labelled `theme` with no open epic
  above it; below one it is an epic. An item's owner is the nearest open epic up its `extra.parent` chain, through tasks
  and closed epics; with none it counts under `no_epic`. If open-epic parents cycle, the smallest source/ref key
  in each cycle becomes a root before aggregation; the remaining edges and direct item ownership stay intact.
  A node's counts are everything beneath it; an epic is never
  counted as an item, and an epic with no items, no epics under it and no theme label is left out. Each item counts
  once: `done` (zm status done), else `working` (claimed or in review), else `blocked` or `deferred` (`extra.status`,
  ignoring case), else `open`. `blocked` is that status alone, not a blocking dependency. `urgent` is the open and
  blocked ones at priority 0 or 1; `changed` is the latest change among them and the epics. `claimed` is every live
  lease on an item the caller can read, oldest first; `mine` is the caller's session or address or, for a person, a
  session they started (on a replica: on that machine). A claim's `epic` is the node that counts its item, and its
  `state` is `blocked` when the item is blocked or its holding session waits on a person (`needs_human`) or has
  ended, `live` when that session is running or idle, else absent (a person's claim, a holder the caller does not
  see, or on a replica a session it keeps no row for). A node's `claims` counts the live and blocked claims beneath it,
  as its item counts roll up. `GET /v0/work/page?epic=<node id>` lists what that open epic owns by the same rule (its
  open child epics included, not what is under them), walking only rows in sight (and in `in=`'s areas, as
  `?area=` narrows the counts): an item out of sight ends a chain here as it does in the counts. The page echoes
  `epic`; an authority older than `epic=` ignores it, so a member's socket answers a page without the echo as empty,
  naming the authority in `skipped`. `deadlines` is each item not done with `extra.due_at`, then
  those with `extra.defer_until` coming back within 7 days; `day` is the day typed (the nearest UTC midnight, since bd
  stores midnight in the typist's zone), and `overdue` compares it with today in UTC, so a client in another zone may
  judge `day` itself. Scope is the rows': an item out of sight adds no count, and an epic out of sight has no id here,
  its visible items counting under `no_epic`. A tracker's visibility is `GET /v0/work`'s. On 11.5k items the read comes
  from indexes (no item's JSON is opened); `make bench` rows `work-rollup`, `replica-rollup`.
- `GET /v0/work/inflight` is a read-only join of leases, items and holding sessions, with each lease's
  effective deadline (`expires`, bound leases included) and the holder's state.
  After the leases come rows marked `unheld`: items whose status is claimed or in_review, that carry a `pr` link
  and that no live lease holds. These are leases that lapsed while their PR is open (an agent waiting on CI), so
  in-flight work does not look abandoned. Such a row's lease names only the project, area and key, with no holder
  or deadline. The item's status stands for the PR's (18.3 moves it to done on merge, to open on an unmerged close).
  A team replica answers the same rows from its copy.

## 11. Work adapters

An adapter is an executable `zm-work-<name>` on `PATH` (or in config.json `work_adapters`), bound to a
project by a `[[trackers]]` entry. **One-shot mode** (every adapter): the daemon runs it once per call: one
JSON request on stdin, one JSON response on stdout, exit 0. A non-zero exit, crash, invalid JSON or no answer
in 30 s is `unavailable` (the adapter path is shown to local callers only). **Serve mode**
(optional; describe says `"serve": true`): the daemon starts `zm-work-<name> --serve` once and keeps it:
requests are JSON lines on stdin carrying a `"rid"`, answers are JSON lines on stdout carrying the same
`"rid"`, possibly out of order (requests run concurrently); `{"cancel": "<rid>"}` asks it to stop that
request (no answer for the cancel line); a line that is not a request answers an error without a rid and
serving goes on; it exits 0 when stdin closes. Each request has the one-shot timeout; one not answered
2 s after its cancel means the adapter is hung and its process group is killed. A crashed adapter is
restarted on the next call (at most once a second); a read cut off by the crash is sent once more, a write
never is (it is `unavailable`, though it may have reached the tracker). An adapter executable replaced on disk
gets a new process for new calls; the old one's stdin is closed and it finishes what it has in flight (killed
after the timeout plus 2 s, or when the daemon stops). Adapters are stateless and use their tracker's own CLI or API and credentials, never
zm's. zm ships no tracker adapter (native issues are served in process, "Native issues"); it ships `zm-work-conformance`
(`--read-only` by default; `--write` also exercises claim, release, comment, set_status and link; `--config
k=v` is sent with every request; an adapter offering serve mode is checked again over one `--serve` process,
plus pipelining, bad lines, cancel and exit on EOF, unless `--no-serve`; `--setup` also runs an adapter's setup in a
throwaway `data_dir`: a dry run writes nothing, a `frozen` call only probes, undo paths stay inside `data_dir`, and no
result carries the secret `--secret-env NAME` puts in the adapter's environment, `--has-password` saying the slot is
filled; every run of an adapter listing `check` also checks that it writes nothing). A
`[[trackers]]` entry may say `env = "inherit"` (17.2). Every adapter runs with an environment allowlist (17.2:
`PATH, HOME, LANG, TERM, TZ` plus what its `describe.env` declares); `zm tracker add` binds one live.
The daemon kills an adapter's whole process group when it times out, when it hangs, and when the daemon stops.

Required verbs: `describe, list, get, claim, release, comment, set_status`. Optional: `link, watch, changes, setup, check`.
An unsupported verb answers `{"ok": false, "error": {"code": "not_implemented", ...}}`.

```json
{"zm": "zm/v0", "verb": "claim", "id": "bd:solver-3x",
 "args": {"holder": "mini/4bb7545a", "note": "taking it"}, "config": {"url": "https://tracker.example"}}
{"ok": true, "result": {"id": "bd:solver-3x", "source": "bd", "title": "Fix solver timeout", "status": "claimed",
 "owner": "mini/4bb7545a", "extra": {"status": "in_progress"}}}
{"ok": false, "error": {"code": "conflict", "message": "bd:solver-3x is assigned to bea-mac/1a2b3c4d"}}
```

| verb | id | args | result |
| --- | --- | --- | --- |
| `describe` | - | - | `{"zm": "zm/v0", "name": "acme", "prefix": "bd", "version": "0.1.0", "verbs": [...], "backup"?: ["sqlite"], "env"?: {"vars"?: [name], "secrets"?: [{"name", "from"}]}}` (17.9, 17.2) |
| `list` | - | `{"status"?, "owner"?, "q"?, "limit"?}` | `{"items": [WorkItem]}` |
| `get` | yes | - | WorkItem |
| `claim` | yes | `{"holder", "note"?}` | WorkItem; `conflict` if held elsewhere |
| `release` | yes | `{"holder"}` | WorkItem |
| `comment` | yes | `{"body", "author", "delivery_id"?}` | `{}` |
| `set_status` | yes | `{"status"}` | WorkItem |
| `link` | yes | `{"rel", "target"}` | WorkItem |
| `watch` | - | `{"since"?}` | NDJSON: one WorkItem per line until stdin closes or it is killed |
| `changes` | - | `{"since"?, "page"?}` | `{"items": [WorkItem], "next"?, "cursor"?, "deleted"?: [id], "count"?, "partial"?}` |
| `setup` | - | `{"dry_run"?, "options"?: {key: value}, "zm"?: {"data_dir"?, "issue_prefix"?, "siblings"?: [prefix], "has_password"?}, "frozen"?}` | `{"notes"?: [string], "config"?: {key: value}, "created"?, "schema"?, "undo"?: [{"op": "remove_dir", "path"}]}` |
| `check` | - | - | `{"checks"?: [{"name", "ok", "skipped"?, "detail"?}], "notes"?: [string], "cli"?: {"bin", "version"?, "error"?}}` |

- `describe.idempotent_comments` (optional, default false) promises that `comment.delivery_id` is
  atomically bound to item, author and body at the final sink: exact replay succeeds without adding a
  comment; changed payload conflicts. Native issues implement it in the comment transaction. Durable
  decision delivery refuses unsupported adapters as `unknown` requiring reconciliation; it does not make
  an unsupported external write or claim exactly-once delivery. Ordinary comments remain compatible.
- `setup` (optional) is how an adapter does what it needs once, without zm linking its code. `describe.setup`
  (`{"touches": [string], "options"?: [string], "password"?: bool, "before_bind"?: bool}`) says what it touches outside zm. `zm tracker add` calls it twice: with `dry_run`
  before binding (the adapter checks its config and may return defaults, merged only where the config sets
  nothing; the request's `actor` is the machine's human), then without it once the tracker is bound (its notes
  are shown, and a failure is a note, never a refusal). For every adapter zm itself refuses an inline secret in
  the config (`key`, `api_key`, `apikey`, `token`, `secret`, `password`, `client_secret`) and makes `*_file` and
  `*_dir` paths absolute.
- **Setup options, frozen, undo** (additive, 2026-11-01). `options` carries `tracker add`'s flags as given, by
  key: `mode`, `dir`, `server`, `database`, `user` and `allow_migrate` (`"true"`). The map is exactly the keys
  `describe.setup.options` declares that have a value: a flag whose key the adapter did not declare is refused
  (`bad_request`) before any call, an undeclared key is never sent, and a `--config` key that is a declared option
  is refused too (the setup plan sets it; use the flag), so a config never points setup elsewhere; an adapter
  also refuses a config that disagrees with its plan. What zm fills itself is `zm`, which no option or config
  reaches: `data_dir` (`$ZM_HOME/trackers/<project>/<prefix>`, the tracker's own directory), `issue_prefix` (the
  project's slug), `has_password` (zm stored `--password-file`; only for an adapter whose `describe.setup` says
  `password`, else the flag is refused) and `siblings`: the prefixes, nothing else, of the same project's other
  trackers of this adapter (an adapter may refuse a second new database next to one, likely a mistake). `frozen` (a Dolt home's
  rollback copy) is no longer set by zm since the Dolt homes went (17.9); an adapter that gets it only probes: it
  opens its store read-only and creates, migrates and writes nothing. A setup result
  never carries a secret: zm copies `--password-file` into the tracker's secret slot (0600) before setup, and the
  adapter gets it only in its environment (17.2).
  `before_bind`: zm calls setup without `dry_run` before binding instead of after; its `config` is merged like
  the dry run's, its failure refuses the add, and its `undo` runs when a later step of the add fails (binding,
  checks, first sync). `created` says this call made something new. zm runs `undo` only for a call that answered
  `created`, only for paths that did not exist before that call, and only for a path that, symlinks resolved, is
  strictly inside the tracker's `data_dir`; any other path is refused and logged. No undo touches a server
  database (the containment guards against a buggy adapter, not a hostile one, which owns its directory anyway).
  `schema` is a server schema check an adapter may report (`TrackerResult.schema`). setup answers in 120 s.
  An existing binding is never set up again: `tracker ls` and `tracker test` never call setup, and
  stored config keys keep their names and meaning.
- `describe.env` (optional, additive 2026-11-01) is all an adapter gets beyond `PATH, HOME, LANG, TERM, TZ`
  (17.2). `vars` pass from the daemon's environment when set, except zm's own names, which zm fills and never
  takes from there: `ZM_DATA_DIR` (the tracker's `data_dir`) and `ZM_TOOLS_DIR` (`$ZM_HOME/tools`); any other
  `ZM_` name is refused, and so is a plain variable zm never passes from its shell: a secret-shaped name
  (`*_TOKEN`, `*_PASSWORD`, `*_SECRET`, `*_KEY`, `*_KEY_*`: declare it in `secrets`), an injection or
  credential-agent one (`LD_*`, `DYLD_*`, `SSH_AUTH_SOCK`, `GIT_SSH*`, `GIT_ASKPASS`, `GIT_CONFIG*`) or a
  tracker CLI's own (`BEADS_*`, `BD_*`, `DOLT_*`: the adapter builds them from its config); `tracker add` and
  `tracker test` show the declaration. `secrets` zm reads through its Secrets brick, never from its environment: `from` is
  `slot` (the tracker's secret slot, `--password-file`) or `config:<key>` (the file that config key names); one
  that reads empty is unset. A secret named in `vars` too, or an unknown `from`, makes describe fail. describe
  itself runs with the base (the daemon's environment for `env = "inherit"`, which gets nothing added).
  `zm-work-conformance` checks that the declaration is valid and zm can honour it; the daemon's test
  `TestAdapterGetsOnlyDeclaredEnv` (`internal/work`) proves an adapter gets nothing undeclared.
- `check` (optional, read-only; an adapter whose describe does not list it is never sent it, and it writes
  nothing): checks only the adapter knows how to make. `tracker add` and `tracker test` call it after the
  conformance checks; its `checks` count like theirs, its `notes` are shown, and `cli` is the tracker CLI as the
  adapter ran it (reported as a check).

- A write verb's request carries `actor`: the zm identity name it is made for (the caller, else the launcher
  of the claim's holder session; absent for the daemon's own writes). Adapters may attribute the write to it.
- `extra` is the adapter's, with these keys read by the rollup (section 10), all optional: `status`, `issue_type`
  (`epic` makes a node), `priority` (a number, 0 highest), `parent` (the parent's ref, with no prefix), `due_at` and
  `defer_until` (RFC 3339). native reports all of them; an adapter that reports none has its
  items count flat under `no_epic`. A full listing announces an item whose `due_at` or `defer_until` the mirror lacks,
  so a tracker that starts reporting them reaches replicas without a change to the item.
- Ids in and out carry the prefix. Statuses map onto the four zm statuses; the tracker's own goes in
  `extra.status`. `claim` records the holder in the tracker where it can. `comment` prefixes the author when
  the tracker shows a different user. `config` is the project's tracker config, passed on every call.
- `changes` feeds the mirror (section 10). `since` empty: a full listing of every item, done ones included,
  in pages (call again with `page` = `next` until `next` is empty); the last page has the `cursor`.
  `since` = a cursor: what changed after it (re-sending some unchanged items is fine). `deleted` ids, if the
  tracker knows them; `count` = the tracker's total, which lets the daemon notice deletions; `partial` on a
  page of a full listing = it may leave items out (cut at a limit), so the daemon drops nothing as gone; a
  cursor it cannot use is `bad_request` (the daemon then lists in full). Items need an id with the prefix;
  a title is optional. describe may add `"poll_seconds"`: the least
  pause between incremental syncs (for a tracker's rate limit).
- `zm-work-conformance <adapter> [--config k=v] [--id <id>] [--write] [--no-serve]` checks an adapter
  against this contract (read-only unless `--write`).

## 12. Events

Every change is a CloudEvents 1.0 JSON envelope, appended to its project's log (project.db; machine-level
events to registry.db; the last 20 000 per log are kept) and streamed over SSE (`GET /v0/events`). A
**live-only** event (`events.Input.Live`) is streamed to live subscribers and never stored, so no replay, catch-up,
webhook catch-up or team sync ever returns it: only a change that is superseded at once and that a reconnecting
client recovers by reading the object again (a session's activity, section 8). Its id is minted under the bus lock
like any other (stored publishes, team transactions included, hold it from minting to delivery), so it is a valid
resume cursor that skips no stored event.

```json
{"specversion": "1.0", "id": "01JA0M2C9Q3T4V5W6X7Y8Z9A0B", "source": "zm://mini", "type": "dev.zm.mail.sent",
 "subject": "01JA0M2C9Q3T4V5W6X7Y8Z9A0B", "time": "2026-10-30T18:00:00Z", "datacontenttype": "application/json",
 "data": {"id": "01JA0M2C9Q3T4V5W6X7Y8Z9A0B", "...": "..."}, "zmproject": "01J9Z3K0QX7Y8AZ5V2N4B6C8D0", "zmarea": "solver"}
```

- Extensions: `zmproject` (ULID or `none`), `zmarea` (`""` = project-level).
- `id` is a ULID minted under the bus lock, so ids increase in publish order across all logs. It is the SSE
  `id:` and the **one resume cursor**: `Last-Event-ID: <id>` replays every visible event after it from every
  log, merged by id. `?since=0` replays everything stored; no `since` = live only. There is no sequence number.
  A live-only stream starts with `: cursor <id>` right after the banner (an id minted just after it subscribed:
  every event after the cursor is on the stream and replayable from it, and one published while the stream
  started is delivered live), so a stream that drops before its first event still resumes without a gap.
- **No silent gaps.** The bus never blocks on, and never silently drops for, a slow subscriber: a live reader
  more than its buffer (256) behind is cut off. In-process, its channel closes before it cancelled
  (resubscribe, then `Since(last)`); over SSE the stream ends with `: overflow <last id>` and the client
  reconnects with `Last-Event-ID` (`client.Events` does this itself, so `zm events` and hooks see every event
  once, in order, and fail rather than loop when no progress is possible).
- Each subscriber gets only events it may `read` (by `zmproject`/`zmarea`), with hidden `Mentions`
  replaced by `someone`. An event may name **owner-only keys** of its data (`events.Input.OwnerOnly`, stored with
  it): only the machine's owner in person (scope all on the socket without a token) gets them, live or replayed;
  every other reader gets the event without them, the daemon's own followers included (webhooks, which POST
  events to any URL even when the owner created them; presence; sync), and `zm project export` scrubs them from
  its copy of the log (a shell session's `term_title`, and every session's `fg`, `busy` and `ports`, section 8). Any new stream or export of session data must keep that rule. Likewise **members-only keys**
  (`events.Input.MembersOnly`): only readers of the whole project (and the owner) get them, an area guest gets the
  event without them (a shared session's `last_prompt` and `in_use`, 17.10), and `zm project export` scrubs them
  from its copy too. An event may carry an **audience** (stored, so replay applies it too): readers
  need its action (e.g. `operate`) on the area unless their address is one of its parties. Restricted mail
  (section 9) uses it. Authz and token events are machine-level (owner only).

| type | subject | data |
| --- | --- | --- |
| `dev.zm.daemon.started` / `.stopping` | machine | Health / `{by, what, force}` (null when not asked by a request) |
| `dev.zm.daemon.swapped` | machine | SwapRecord (section 2, "Swaps") |
| `dev.zm.daemon.update_checked` | machine | SwapRecord, what=update; host pull held/unchanged/failed/ok |
| `dev.zm.daemon.selfcheck` | machine | SelfCheck: problems found, or cleared |
| `dev.zm.project.created` / `.updated` | project ULID | Project |
| `dev.zm.project.area_added` | area | Area (event area = that area) |
| `dev.zm.authz.granted` / `.revoked` | identity | Grant |
| `dev.zm.authz.token_minted` / `.token_revoked` | token id | Token (never the secret) |
| `dev.zm.mail.sent` / `.acked` | mail id | Mail |
| `dev.zm.work.created` / `.status_changed` / `.linked` / `.changed` | work id | WorkItem (`created` also announces an issue moved into the project, including a move back) |
| `dev.zm.work.commented` | work id | Comment |
| `dev.zm.github.pr.ready` / `.head_moved` / `.closed` / `.checks_settled` | `owner/name#N` | GitHubPREvent (18.2 "PR lifecycle events") |
| `dev.zm.work.refreshed` | empty | Content-free project freshness hint after a tracker state transition; refetch scoped work metadata, including when no item changed |
| `dev.zm.work.imported` | job id | WorkImported (phase, work ids). **Deprecated 2026-11-01**: nothing publishes it since `work import` was retired (17.5); replicas still read it from older logs |
| `dev.zm.work.claimed` / `.released` / `.lease_expired` | key | Lease |
| `dev.zm.run_target.expired` | handle | RunTargetLease (a Lease's fields, no `bound`; section 20) |
| `dev.zm.work.moved` | old work id | WorkMoved `{id}`, in the project and area it left (never where it went: those readers may not see it); a replica drops the item from its copy |
| `dev.zm.session.started` / `.registered` / `.state_changed` / `.updated` / `.ended` | session address | Session (`updated`: activity or term_title alone, coalesced) |
| `dev.zm.session.left` | session address | SessionLeft `{address, project, area}`: where it was (old project/area's log) |
| `dev.zm.session.admission_refused` | machine | Admission |
| `dev.zm.tailnet.up` / `.down` | machine | TailnetStatus |
| `dev.zm.approver.added` / `.removed` | approver name | Approver (machine-level) |
| `dev.zm.ask.created` | ask id | Ask (ask's project/area; audience operate + parties) |
| `dev.zm.ask.default_applied` | ask id | Ask (as ask.created) |
| `dev.zm.ask.default_undone` | ask id | Ask (as ask.created) |
| `dev.zm.ask.repeated` | ask id | Ask (as ask.created): an open card counted a repeat |
| `dev.zm.decision.recorded` | ask id | Decision (ask's project/area; audience operate + parties) |
| `dev.zm.job.started` / `.finished` | job id | **Retired 2026-11-01** with the import jobs (17.5); nothing publishes them |
| `dev.zm.team.joined` | replica machine | TeamJoined (machine-level: a replica redeemed an invite, 17.4) |

## 13. Remote access (tailnet, optional)

- Without a tailnet, zm is one machine and one principal. `zm tailnet up` starts an embedded Tailscale
  node (tsnet) in the daemon and serves the same API over HTTP on port 7433 of the tailnet. Remote access is the
  tailnet, plus, for team members only, the tunnel door (17.11): a loopback listener behind a Cloudflare Tunnel
  that takes nothing but signed requests with a key-bound token.
- Every tailnet request is authenticated by core (section 1): a bearer token, else the WhoIs login mapped to
  an identity (`zm identity add bea --tailscale bea@example.com`, then `zm grant bea member side`). A
  login alone works for zm clients by the node's own name (they send the protocol header), not for a web page.
  Remote callers never pass `local` routes (spawn, attach, input, stop, register, tokens, identities,
  project create/export, tailnet control).
- A new zm node is named `potluck-<machine>` on the tailnet; a node made before the rename keeps its
  `zm-<machine>` name, and both are read (`up` refuses a name with neither prefix). `zm daemon
  --tailnet` (or `zm tailnet up`) joins; the auth key comes from `--auth-key` or the daemon's `TS_AUTHKEY`
  and is never saved. `up --auth-key` restarts a node still waiting for login and conflicts once logged in.
- A remote client sets `ZM_HOST` to the node's name (`potluck-<machine>` or `zm-<machine>`; a bare
  `<machine>` still means `http://zm-<machine>:7433`, so it misses a new node; a URL or host[:port] also works)
  and `ZM_TOKEN=potluck_...`; the same CLI works, scoped.
- In a Potluck pool box, `POTLUCK_BOX_CREDENTIAL` names the box credential file a potluck-token job grant
  delivers (`$POTLUCK_SECRETS/POTLUCK_TOKEN`, JSON `potluck-box/1`: a token bound to a per-box key, and the
  authority's tunnel URL and key fingerprint). `zm` and `bd-zm` then call that authority's tunnel door with it
  alone: signed with the box key, answers read only when that authority signed them. No socket, no autostart, no
  runtime registration, no prefix cache; `ZM_HOST` and `ZM_TOKEN` are ignored; `ZM_AS`, `ZM_PROJECT`, `--as`
  and `--project` apply. An unreadable or malformed file, another kind, a refused (expired, revoked) token or
  another authority's key exits non-zero naming the variable, never falls back, and never prints the token or
  key. Unset or empty: the client as before. The authority needs a potluck with `potluck-box/1` (the
  `tokens.box_key` column).
- `zm peers` finds nodes by either prefix and checks `/v0/health`, which names the machine to every
  caller (the hostname already does). v0 forwards nothing between daemons for solo projects: mail to another
  machine is `not_found` until a forwarder is wired (`mail.Service.SetForwarder`). Team projects (17.3) route
  mail through their authority instead.

### 13.1 Phone push

zm has no web UI of its own (the UI is the Mezze app); the one page it serves is the Decisions page (SPEC 16),
whose paired devices get Web Push ("Phone alerts" there). Delivery:

- The daemon stores its VAPID private key via Secrets and sends Web Push only for that person's session entering
  `needs_human` or ending not ok (except `killed`), and `needs=human` or news mail for them. At most five
  subscriptions per person are kept.
- Mail and its waiting session are one decision; bursts coalesce into one alert. Deliveries queue durably, retry
  transient failures including 429 with bounded Retry-After backoff for at most an hour, and recheck scope and
  whether the item is still open. The push TTL is one hour. Every alert opens `/v0/approvals`. Replica presence
  carries the outcome kind of ended sessions to its authority; an older replica's missing outcome does not
  generate an end push.
- `zm web test-push` sends the local human's subscriptions a test notification. Apple, Mozilla and Google push
  service endpoints are supported; delivery uses their browser push services, with no zm relay. The default
  VAPID contact is the project's public URL; `web.vapid_subject` overrides it with an HTTPS URL.

## 14. Integrations

- **Hooks** (`zm install claude|codex [--home DIR] [--yes]`): installed hooks run the hidden `zm hook
  <agent> <session-start|prompt|tool|notification|stop|notify|session-end|subagent-start|subagent-stop>`. Start registers the session and
  tells the agent its address; prompt sets `running` and prints the `zm notice` line; tool sets `running` with the
  step as activity (Claude `PreToolUse`; Codex `PreToolUse` and `PostToolUse`); notification sets `needs_human` on
  a permission prompt; stop/notify set `idle`; session-end ends an unmanaged session (inside a managed shell, unbinds its agent: section 8); subagent-start and subagent-stop (Claude SubagentStart, SubagentStop) list subagents (section 8). Codex gets SessionStart,
  UserPromptSubmit, PreToolUse, PostToolUse, SessionEnd and SubagentStop in `hooks.json`, plus notify. SubagentStop
  is a subagent thread's turn ending, not the thread (Codex runs it at the end of every subagent turn): it sets that
  thread's session idle, as stop does, and its claims stay for its next turn. Codex's SessionEnd is the root thread's
  only, so a subagent thread's session is never ended by a hook: it goes idle by the no-hooks rule and its claims
  lapse one bound TTL after its last running tick. A Codex thread is one
  session: its hooks' `session_id` is the root thread's id (on a subagent thread too) and `agent_id` the subagent
  thread's own, while notify's `thread-id` and the tools' `CODEX_THREAD_ID` are the thread's own, so a Codex hook is
  registered by `agent_id`, else `session_id` (codex-rs 0.166: `Session::session_id`, `hook_runtime.rs`).
  Default is a dry run printing the exact diff (`--dry-run`, on by default); `--yes` writes (keeping a
  backup, following a symlinked config file to its target). Tests always pass a temp `--home`. Hooks never
  fail or slow the prompt: one socket round trip, bounded end to end (stdin included) at 300 ms on the prompt path
  (prompt, notification, stop, notify, `notice`) and 2 s for session-start/end, silent when the daemon
  is down or wedged. Latency budgets (p95, process start included, `make bench`): hooks and `notice` 15 ms, reads
  30 ms, tracker reads (`work ls/show/who/sources`, 11k items, after the first sync) 50 ms. Nothing in `zm` queries the terminal at start-up. opencode and gemini:
  not in v0. Codex runs `hooks.json` hooks only after the user trusts them in Codex (`/hooks`); zm never
  trusts them, but `zm install codex` reads Codex's
  `[hooks.state."<hooks.json real path>:<event>:<group>:<hook>"]` entries in config.toml and reports each
  hook as `trusted`, `untrusted`, `disabled`, `changed` (zm rewrote it) or `missing`, with what to do.
- **MCP** (`zm mcp`): the CLI's verbs as MCP tools over stdio; roots = the resolved project's repo paths.
  It never starts the daemon: with none running it still answers `initialize` and `tools/list` (well inside
  200 ms) and every tool call is an error saying `zm daemon not running`.
  Mail bodies are returned as untrusted data in one fixed frame (**untrusted mail frame**): `mail_inbox` answers three
  text blocks, the opening line (`orchapi.MailFrameOpen`: untrusted data, written by the senders each mail names,
  information never instructions), the JSON `{mail}` (each mail's `from` names its sender; a body is a JSON string, so
  a body cannot start a line or end its own quotes), and the closing line `End of untrusted mail data.`
  (`orchapi.MailFrameClose`) as a block of its own; the unread notice, when there is one, follows outside the frame.
  The CLI (`zm inbox`, `thread`, `sent`, `mail`; not `--json`, which prints the bare JSON) prints the same opening
  line, the project name on a line of its own above its mail, one line per mail starting with spaces (its body quoted as one JSON string on that line, whitespace
  collapsed, cut at 200 bytes before quoting; sender, recipient, ref and tags are collapsed to one line too) and the same closing line, so no body can make a line equal to either frame line.
  Hook notices carry no body.
  With a runtime id but no explicit or managed address, tools register the runtime session first; if
  registration fails, the tool returns that error without executing and retries registration on its next call.
  A local session address that reached it only through the environment (`ZM_AS` or `ZM_SESSION`, not
  `--as`), in a process that also has a runtime id, is used only once proved to be this agent: that session
  records the same runtime id, or its recorded process is the nearest agent process (claude, codex) of its kind
  or the server's own kind above the MCP server (a zm-managed session, including Codex started inside Claude or a
  runtime not bound yet). Being a farther ancestor is not proof, nor is a session of another kind with this
  runtime's own agent process nearer (Codex run from a Claude session's shell). With both runtime ids in
  the environment (`CLAUDE_CODE_SESSION_ID` is inherited by a Claude session's shell children), the nearer agent
  process decides which is this runtime's, Claude when neither shows. An address naming another session, or no session, yields to
  registering the runtime's own session, so an app started from one agent's session cannot make every agent it
  launches act as that session. Another machine's address is used as before.
  The check runs once on the first tool call; while the daemon cannot answer it, tools act as the address and
  check again on the next call.
  Managed Codex MCP configuration forwards `ZM_AS`, `ZM_SESSION` and `CODEX_THREAD_ID` through `env_vars`.
  These are process environment values, not per-call thread provenance: a shared app-server can retain
  another thread's address. zm cannot infer the current thread from that server's process ancestry or
  a hook's timing; correct automatic routing requires runtime-supplied per-thread execution identity.
- **Claude Code plugin** (`integrations/claude-code/`) and an **AGENTS.md snippet** for repos.
- **Agent-facing budget**: AGENTS.md snippet + MCP tool names and descriptions + the longest notice line must
  fit in 8 KiB (`internal/budget`, one number, one test).
- **Cockpit**: the Mezze app, outside this repo. Bare `zm` prints help.

## 15. Versioning and scope

Each route carries a stability label (API.md "Stability"; clients: `docs/clients.md`). Within the
`potluck/1` protocol (`zm/v0` is the same major), a `stable` route, the listed `--json` commands and the event
streams change only by adding; a break gets a "Clients" line in `CHANGELOG.md`. `beta`, `app` and `internal`
routes may change in any release (a change to a documented `beta` field still gets that line). A field added to a `stable` shape within `potluck/1` is optional: `docs/schema/openapi.json` is generated from the Go types (`scripts/clientschema`), and a test fails a new `required` entry in any object schema that `docs/schema/openapi.released.json` (the floor as of the last release cut) already had; query parameters stay in API.md, not in the schema. Out of v0 (see DESIGN-NOTES.md): Tailscale ACL grant mapping
(WhoIs app capabilities), OpenTelemetry, SCIM, external policy engines, project
archive/handover, peer mail forwarding, automatic worktree deletion, remote-launched sessions, adapter
`watch` (the mirror syncs with `changes` instead). In v0 after integration: the `--sandbox` door (optional, needs `srt`) and approvals from a paired device (section 16).

## 16. Approvals: the owner decides; a paired device is opt-in

The `approvals` module (`internal/approvals`; routes in API.md "Approvals"). First use: a project's owner trade
asks, replacing hand-made `brief,decide` tokens.

- **Owners decide.** The identity that owns or administers a project answers its card asks
  directly, in one call with no confirmation step, typing check or delay: `POST /v0/asks/{id}/choose` (`zm ask
  answer <id> <choice>`, a cockpit's choice buttons). Who counts (`core.Deps.ProjectAdmin`, one helper for every
  owner right a member's link would lose, inbox aliases too): a caller whose scope reaches admin on the project
  (this machine's owner on its Unix socket; a token or login granted that much); or a member machine's
  node-bound token forwarded to the authority, by its identity's own grants there (the token is capped at member,
  so its scope cannot say so; the cap still bounds everything else). A minted token keeps its cap: a member-capped
  agent's token does not count. A member who is not an owner or admin is `403`; an ask the caller cannot read is
  `404`. `GET /v0/owner?project=` answers the same check for the caller (and its GitHub link) before it answers
  anything, so a client shows choice buttons only when they will be taken. The decision records `method: owner` and the `machine` it came from (`token:<id>` for a token, whose
  machine is unknown); its receipt is signed by the authority's receipt key as
  always, with approver the identity's name and node `owner:<machine>`. A pull request's ask is not answered
  this way: a merge gate's receipt stays a paired device's. **Plainly:** every process running as the owner's OS
  user on that machine reaches the socket, so an agent there can answer too. When that matters, pair a device
  (below) or pair a cockpit's key approver (below); neither is required.
- **Answer-only tokens.** A client that only shows and answers cards holds an answer-only token (section 4:
  route limit `answer`, one project, one tailnet device, an expiry) instead of an owner token. It reads the
  project's asks (`GET /v0/asks`, `GET /v0/asks/{id}`), the Decisions list with asks only (`GET /v0/decisions`
  forces `work=0` and ignores `label=` and `for=` for it), answers with `choose`, and checks receipts
  (`GET /v0/receipts/verify`); every other route is `403` `token-limit`. `choose` judges it exactly as an owner
  token (owner or admin of the project by its identity's current grants, every refusal below), plus one rule:
  a choice that carries a confirm word is `403` `signed-only` for it, whatever its kind (the word is printed on
  the card, so it guards a slip, not a token). Its decision is the owner's unsigned one: `method: owner`,
  `surface: answer-token`, `machine: answer-token:<token id>`, receipt node `owner:answer-token:<token id>` (a
  signed field: the receipt says which token answered, and names the token to revoke), approver the token's
  identity, person attribution as for an answer on the authority's socket by its local account; the written-back
  line names the machine as `answer-token:<id>`. So every consumer that needs a signed answer keeps refusing it:
  a pull request's ask, a choice that runs something or hands a brief, rulings and permits. A stolen one, until
  it is revoked, expires or its identity stops owning the project, can from its device only read that
  project's asks and decisions and answer any card the owner could answer unsigned, written answers included
  (an answer's own effects follow: the asker's reply, the work item's comment, an ack-mail card's ack of the
  mails frozen on it); nothing else. **Plainly:** it gives an agent on the owner's machine nothing it lacks, since the socket
  already answers; minting it from inside an agent session is refused (the origin check below) and it never
  works from this host, but those are guard rails: the device it is pinned to must run no agents.
- **Paired approvers, the opt-in.** An approver's decision records `method: approver` and its `device`. The
  approver right is bound to its Tailscale node (WhoIs stable node ID **and** node key; a node that re-keys must pair
  again), never to a token, a name or the shared Tailscale login. Agents on the Mac share that login, so
  login-bound rights would be impersonable. Its routes have their own guard, `device`: the socket and any
  token get `403`, a network caller without a WhoIs node `401`, before any object is looked up. The one other
  way to hold the approver right is a key approver's signature (below), which no caller holds by being a caller.
- **Pairing.** `zm approver add <name>` (local) reserves the name and prints a one-time code (8 characters,
  10 minutes) + a QR of the pairing page's tailnet URL. An unexpired pending code cannot be
  replaced, regardless of caller address; after expiry, add may issue a new code. The device opens
  `GET /v0/approvals` on the daemon's
  tailnet listener and enters the code; the daemon binds the approver to that node. Pairing is refused from
  this machine (the daemon's own tsnet node, and any caller whose Tailscale address is one of the host's: the
  Mac's own Tailscale, where its agents run), from tagged nodes (servers, CI), from other zm daemons' nodes
  (which run agents: recognised by the node's own hostname or its tailnet name starting `potluck-` or `zm-`;
  `zm tailnet up` refuses any other hostname, so a configured hostname is covered and a rename in the admin
  console is not enough to hide one) and from a node already bound to an approver. A node whose key changed
  since it paired is told to pair again (`zm approver rm <name>`, then `add`; an existing approver
  confirms it). Five wrong codes per node (20
  overall) within the TTL lock pairing out. Joining a device to the tailnet needs a browser sign-in agents
  cannot do. The first approver is bootstrapped this way (`confirmed_by: bootstrap`), **once per daemon**:
  bootstrapping creates `$ZM_HOME/approvals/bootstrapped`, which the daemon never removes. Every later
  pairing, including one after every approver was removed, is `unconfirmed` and gets the right only when an
  existing approver confirms it on their own device (never itself, never from the socket). Otherwise any socket
  caller could `rm` every approver, `add` a name and pair a tailnet node it reaches (a dev or farm box with its
  own login), which no node check catches. Recovery when no approver is left is manual and outside the API:
  the human deletes that file by hand on the Mac; the next pairing bootstraps again and is logged and
  published as `dev.zm.approver.added` with `confirmed_by: bootstrap`. While the marker exists and no
  approver is active, a pending or unconfirmed approver carries a `note` saying nobody can confirm it and
  giving that recovery (in `approver add`'s answer, the pairing answer, the device page and `zm approver
  ls`); a device is told the marker's place under `$ZM_HOME`, never the Mac's paths. (A process with the owner's file
  access is outside this model anyway: it can also read `receipt.key`.) `zm approver ls`, `zm approver rm <name>` (takes effect immediately;
  rm needs only local). Approver names are reserved from `add` on: nobody can squat them, or a lookalike of
  them, before pairing.
- **One-tap phone pairing (2026-11-06).** The owner's phone needs no code: on `GET /v0/approvals` a node whose
  self-reported OS is iOS or android and whose tailnet login is the one this daemon's own node signed in with
  sees **Use this phone**, which is `POST /v0/approvals/pair-phone`. Every rule above still applies: the
  refusals (this machine, tagged, zm, bound), the attempt limit, bootstrap once per daemon, and every later
  phone `unconfirmed` until an existing approver taps confirm (the stuck case stays manual). The approver is
  named from the node's name; a refused attempt leaves no pending name. Limits, plainly: the login does not
  tell the owner from agents on the owner's other nodes, and the OS is self-reported, so a forked tailnet client
  on such a node could claim to be a phone. That is why the OS only narrows: it never skips confirmation, and
  the only thing it can win is a daemon's not-yet-used bootstrap. Any other device sees a QR of the page's own
  address to scan with the phone; a `?code=` link (from `zm approver add`'s QR) needs one tap, nothing typed.
- **Asks.** `POST /v0/asks` (`zm ask new --repo R --pr N --head SHA <question>`) sends a `needs: human` mail
  (to `owners` or this machine's `human:<name>`, the only addressees the decision can answer; anything else is
  `400`) and records its `subject` `{repo, pr, head_sha}` (canonical repo
  `host/owner/name`; the full head SHA, never a prefix) and options (default approve, decline); the ask's id
  is the mail's id. Asks follow the restricted-mail rule: a reader sees one only with `operate` on its area or
  as a party. The approver device lists open asks with one-tap buttons; `to` only routes the mail: every active approver
  sees and may decide every open ask on this daemon (approvers are machine-wide). One decision per ask: the same answer
  again returns the same receipt; a conflicting second answer is refused (`409`). After the decision the
  ask's mail is answered (as the machine's human, naming the approver and device) and taken, so the asking
  session leaves `needs_human`; if that answer fails, the decision stands and its response carries
  `mail_error`. The ask is recorded before its mail is sent (under the id the mail then takes), so the mail is
  never open without its ask; if the ask cannot be recorded nothing is sent, and if the mail cannot be sent
  the ask is removed. On a member machine every ask route but `decide` goes to the project's authority (section
  17.3), where the ask, its approvers and its receipts live; `decide` is answered here, with the socket's refusal.
  `choose` goes out once (never hedged): by `?project=` or `-P`, else to each joined authority while the ask is
  not found.
- **Subjects.** An ask is about one of: a pull request's head commit (as above), a work item (`subject.work`, any
  id `zm work` takes or a bare tracker id; stored canonical; `zm ask new --work`), or a plain question (no
  subject; `subject.question` is SHA256 of `zm-question/1\n<ask id>\n<card_sha256>`). The last two need a note card. A work
  item the caller cannot read answers `not_found`. Resolution carries canonical ID, area and destination
  project after a move; write authorization is checked again there before creating the ask and mail.
- **Note cards** (`card`). Agents go with their gut unless it could truly go either way; a person answers about 40
  of these a day, each in one sitting. A card has `context` (explain it like they are 15), `background`,
  `question`, 2 to 4 `choices` (`label`, `effect` (optional: one line on what picking it changes), `pros`, `cons`;
  `key` defaults to a, b, c, d and becomes the option), `recommend` (a key), `why` (what it does, good and bad),
  `after_press` (optional: who acts after the press and how the person will know) and `searched` (what the asker
  checked first, at least 3 words, not on the card). A card stored without the optional fields keeps its bytes,
  so its `card_sha256` is unchanged. What a person reads is at most 600 words (only against abuse: whether it says enough is
  the card judge's call, below; long detail goes in `background`, which Decisions folds under More) and carries no paths, file names, commit
  hashes, `#123`, env var names, or ids of this project or the subject's source (a word like `zm-native` is
  fine). Text fields are single lines without `OWNER DECISION`, `PROGRAM DECISION` or recommendation
  presentation markers; untrusted question and answer prose is quoted in write-back. A card is at most
  12,000 JSON bytes, each text field at most 2,000 bytes, title at most 80 runes, searched at most 600
  Unicode words. Party and contact names use the same presentation and machine-token checks.
  Beside it: `priority` (0 first to 4, default 2), `by` (a deadline within 30 days: first in the inbox; it
  ends nothing), `topic`, and `for` (`group`, `contact`, `ref`, `contact_ref`: held for a person outside the
  team, `group` naming their organisation; needs a topic, no `by`). A held ask's mail is needs=human (only its
  decision answers it) but waits on that party, not on anyone here: while it is open it holds no asker at
  `needs_human`, never pushes, is not counted as unread or as a session's open ask, and mail listings carry the
  party as `held_for`, which the mail row and its `mail.sent` event carry, so a team replica's copy counts it
  the same way. One ask per work item and topic, or outside
  party and topic, open or answered, and per plain
  question's topic within area and decision authority: operators share a namespace, while a guest's namespace
  is its own address. An unreadable ask neither reserves a visible slot nor suppresses visible work.
  Legacy slots are explicitly namespaced before serving asks or starting delivery retries. The upgrade
  uses retained identity, session-launcher, grant and token-cap evidence; unknown or guest-cap-ambiguous
  authors keep only their party slot. It cannot reconstruct an absent historical subject. Where an old
  and a new slot already coexist, the oldest ask retains the primary slot and the others retain duplicate
  reservations until withdrawn; all work and receipts remain.
  A repeat is `409` carrying the earlier ask when the caller may read it. Past 40 card asks in a
  project in 24 hours the answer's `note` says so to callers who may operate; it is never a refusal. `POST /v0/asks/{id}/withdraw` (the asker, or operate on the area) takes back an open ask:
  its mail is answered and its slot freed; nothing is decided or signed.
- **Repeating cards.** An ask may carry `repeat_key` (`zm ask new --repeat-key`): the id of the
  thing it is about that can break again, such as an automation's id, matched exactly as syntax (3 to 120 letters,
  digits, `.`, `:`, `-`, `_`), never read for meaning, and `latest`, its newest state in one plain line (card text
  rules; at most 300 bytes). While an open ask (not answered, withdrawn or defaulted; one the card judge holds, for its own asker only) of the
  project in the same area and namespace (operators share one; a guest's is its own address, as topics) carries the
  key, a new ask with it files nothing: no judging, mail or slot check; that ask's `repeat` counts it (`seen`, from 1,
  `last_seen`, and `latest` when given), the answer is `200` with that ask, `updated` and a `note`, and
  `dev.zm.ask.repeated` is published (not while the ask is held). Once the ask is withdrawn, the next one files a
  fresh card counted from 1; once it is answered, the usual slot holds (the same work item and topic is `409`
  carrying the answer, which is the asker's), and another item or topic files a fresh card. New asks with one key
  are taken one at a time, so two first failures at once file one card. The key is looked up before the topic slot,
  so a counted repeat never meets the slot's `409`. While the judge holds cards, each asker's held card counts only its
  own repeats, so an outage can leave one card per asker of a key. Asks of a joined group (`same_as`) take no key.
- **Defaults with deadlines.** An ask made with `apply_default` (`zm ask new --apply-default`)
  needs a card with `recommend` and `by`. If nobody answered, withdrew or decided it when `by` passes, the daemon's
  ticker (30 s) records **DEFAULT APPLIED** on the work item, answers the ask's mail (so the asker wakes and leaves
  `needs_human`) and publishes `dev.zm.ask.default_applied`. It is not a decision: no receipt, no `OWNER DECISION`,
  and the ask stays open, so the card stays in Decisions reading "I will do X at <by in the reader's zone> unless you
  say no" before, and offering Undo after. Undo (`POST /v0/asks/{id}/undo`, `zm ask undo`, the owner as `choose`
  judges it; or a paired approver's `POST /v0/asks/{id}/device-undo`, as `decide`
  judges it) records **DEFAULT UNDONE** on the item and mails the asker to put it back; the ask then waits for the
  person's answer. Answering ends the card (keeping the default is answering with it); a decision after an applied
  default says whether it keeps or replaces it. Irreversible asks never auto-apply: the flag is `400` on an ask held for an
  outside party, one carrying an action, or when the card, the work item's title or body, or its label `irreversible`
  names a production write, a publish, an outbound message, money, access or a secret (the label as it is; the text's
  meaning by the irreversible judge, `internal/approvals/irreversible.md` with eval cases beside it, told that a wrong
  pass costs more than a wrong fail; the ask still goes out without the flag). A judge that cannot answer
  makes the flag `503`, never set unchecked. The check runs again at the deadline: a fail turns the default off and
  mails the asker that it waits for the owner; no answer leaves it for the next tick, never applied. While a default stands the ask is undone or answered, never withdrawn
  (`409`). Records retry like answers' effects, each with its own write id; a DEFAULT APPLIED record still unsent
  when the ask is answered or undone is dropped, so the asker's last word is never a stale "proceed".
- **Asks that carry an action** (`card.action`). A card may name one typed, bounded action the daemon runs as
  the approver when they pick its choice (`on`, default the first). The first kind is `ack-mail`: `ids`, 1 to 500
  mails. At creation each must exist, be visible to the asker, be in the ask's project, and be neither an
  announcement nor an open ask, or the ask fails (hidden answers like missing). The daemon freezes the full ids
  and fills `count` and up to 3 `samples` (first lines; asker values are dropped); the card's text shows them.
  `card_sha256` covers the action and the v2 receipt signs an `action` line (kind, count, ids sha256, choice),
  decided either way. Picking the choice adds an `ack` decision effect: it re-checks the card hash and the
  frozen ids, then takes exactly those mails as the approver (`human:<name>`), skipping the needs=human rule
  server side only; taken or gone mail is skipped, so a retry acks nothing twice. Any other answer runs
  nothing. A joined ask never carries the first's action (Routing). An agent still cannot ack needs=human mail itself.
- **Actions that run something**. A person judges or clicks a permission;
  once an owner approves a well-made decision, it happens. Three more kinds, each on a named `machine` of the team
  project (never `auto`: the approver approves a place) that is trusted and takes remote tasks: `run` is a script
  run (`script`, `ref`, the full `commit` ref points at, literal `args`; the machine also runs scripts, and
  re-checks the commit, SPEC 8 "Script runs") or an agent task (`agent` claude or codex, a `brief` of at most 8 KiB;
  the card says an agent will, not that zm runs); `command` is one shell command line (`command`, at most 2 KiB,
  with `ref` and the full `commit`; the machine also runs scripts) shown on the card word for word and run as a script
  run whose program is that line (SPEC 8 "Script runs": the same clean checkout, environment allowlist, output,
  result mail and item comment), as `/bin/sh -c` with a fixed `exec 2>&1` line before it (no positional arguments); the machine's numbers carry `command_runs` (an older zm's do
  not, and naming it is `unavailable` when asked). The line is refused, never cleaned: more than one line, anything
  but printable ASCII (no tab, control, invisible, direction or look-alike character), leading or trailing space,
  text that reads as zm's own (an owner decision, a recommendation, a fingerprint), `sudo`/`su`/`doas`/`pkexec`/
  `run0` (zm never changes user), or what looks like a secret (a known token shape, a secret-shaped name set to a
  literal such as `NPM_TOKEN=...` or `--password x`, an `Authorization` value, `-u user:password`, credentials in a
  URL). A command that needs a secret
  reads it on the machine (its keychain or a file there, section 2's secrets), never from the card. The card's text
  warns when the line, or a script run's, names an irreversible step (the defaults rule below) or a shell removal
  (`rm`, `dd`, `git clean`, a forced push, `terraform apply`, a pipe into a shell, and the like); `authorize-key` adds one OpenSSH public key line (ed25519,
  ecdsa, rsa of 3072 bits or more; no options) to `user`'s `~/.ssh/authorized_keys`, where `user` must be the
  account zm runs as there (its `os_user`; zm never uses sudo); `browser` (a `plan` of at most 4 KiB and a `scope`
  of `origins`, `sign_in`, `submit`) is refused `unavailable` until a runtime enforces its scope. At creation, at
  the authority: the asker may operate on the area, a run's asker is a session, and every field is checked and
  frozen (`command`, `brief_sha256`, `key_comment`, `key_fingerprint` are the daemon's); anything zm cannot run is
  `400` with a hint. A script or command run's `ref` and `commit` are checked by the run machine's own rule (SPEC 8
  "Script runs": a branch or tag name, never `refs/...`; exactly 40 hex), then by a live `git ls-remote` of the
  project's remote at the authority (its checkout's `origin` when that is a listed remote, else the first listed
  remote; nothing fetched or written, at most 15 seconds): a ref the remote does not have, or a `commit` that is
  not what it resolves to now (a tag's peeled commit), is `400`, so no person sees a card that cannot run. When it
  matched, the daemon sets `pin_checked` on the frozen action (covered by `card_sha256`; the card says "the head of
  <ref> when this was asked") and the run carries `commit_on_ref`: it still runs exactly that commit after the
  ref moved on, while the commit is on it. When the authority has no way to reach the remote (no checkout of the
  project there and no listed remote, or no answer in time) the ask is made unchecked and the run takes the
  commit only while the ref still points at it, as before. The check spares a dead card; the run machine's check
  stays the boundary. A tag is the safest ref for a card that may wait. `card_sha256` covers it; the v2 receipt's `action` line is `<kind> sha256=<the frozen
  action's JSON> on=<key>`. `action_text` on the ask says exactly what runs, as the approver reads it (the card's
  no-ids-or-paths rule does not apply to it).
  **A person's own machine** (SPEC 8 "Operator class"): the machine's person is frozen on the action
  (`machine_person`, the daemon's; covered by `card_sha256`) and the card is addressed to them when made (a card
  for the owners becomes theirs; one naming someone else is `400`), so no other owner is shown a choice that
  cannot count. Any answer to it from anyone else (by their linked accounts, the person's count as theirs) is `403`,
  checked against the frozen person and the machine's person now; the start checks again and fails the action
  otherwise. A team machine takes either owner's yes.
  **Whose answer it is**. A card is one person's to decide: the person it is addressed to, else (a card
  for every owner) the human whose session asked (`asker_human`). Any other owner's answer stands, as the owners'
  standing rule lets co-owners answer each other's cards, but never silently: the answer delivered to the asker
  (and written on the work item) carries a second line `ANSWERED BY ANOTHER OWNER: ... <approver> answered it, not
  <person> ...: hold anything that cannot be undone until <person> agrees`, or `ANSWERED ON A DEVICE PAIRED FOR NO
  ONE` when zm cannot tie the device to anyone. The decision (and its event) carries `not_theirs`, that person,
  exactly when such a line is sent; on a card for every owner it is the asker's human, so it shows only where
  `asker_human` does (never in the event). The person's linked accounts count as theirs. A device paired for
  no one (`zm approver add` without `--for`) answers as this machine's owner only while that owner is the one
  person who answers the project; otherwise it answers as no one: its `approver_person` is empty and a person's
  own machine refuses its answer. Nothing is labelled for a sent-back card or a program contact's answer.
  Only a signed answer (`decide` on a paired device, or a key approver's `answer`) may pick such a choice:
  `choose` is `403` "answer on a paired device or in the Mezze app: this choice runs something" (data `{"reason":"signed-only"}`), since every process of the
  owner's OS user reaches their socket. The decision commits with an `action` effect. It re-checks the card hash,
  then acts as the approver's person by their own grants now: one who no longer owns or administers the project
  fails it. It starts through the team module (`orchcore.AskActions`; approvals reads no team tables): a run or command is
  the remote task start of SPEC 8 "Machine choice" with the asking session as parent (its result mails the asker
  and comments on the item; the named machine may be the asker's own: the start is marked an ask's, and the
  target takes it as a task whose parent's end, there too, never stops it) and `client_request_id` `ask-<ask id>`
  (a retry's later attempt `ask-<ask id>-<n>`), so a retried effect never starts twice. Request ids starting `ask-`
  (and `relane-`, a fresh lane's) are kept for these starts: any other start naming one is `bad_request`, and an id already used by a start that is
  not an approved action's is a conflict, never followed. A start whose answer failed is looked up by its id and
  followed when it is this very request, never taken for one that did not start;
  a command or script run on the approver's own machine (its one joined person, or the authority's human, is the
  approver) is marked `as_approver` and runs with `ZM_AS` set to that machine's human and no `ZM_SESSION`, so its zm
  calls act as the approver (an owner-only `zm ask check --evidence`, say); anywhere else, and an agent
  action anywhere, acts as its task session;
  authorize-key is the `authorize_key` machine op on the same queue, whose `from` is the approver: the target,
  only with `remote_tasks`, appends exactly the frozen line for its own user (creating `.ssh` 0700 and the file
  0600) unless the key is already there. A start the module cannot take yet is tried again for 10 minutes; any
  refusal fails the action. The effect settles once the start is queued or refused.
  The ask's `action_state` (`queued`, `running`, `done`, `failed` with a plain `reason`, `started_at`,
  `ended_at`, `task`, `confirmed_by`, `confirmed_at`, `retries`, `retryable`, `retry_until`) and `action_state_text` follow the task or op: done only
  for an ok exit (the state is the task's in the waiting line, so a refusal there shows); failed for a refusal, a machine that never took it within 30 minutes (the line places it again every 10 minutes; a run still waiting in the line, or placed on another machine and not carried to it, 30 minutes after its start (after a requeue, after its latest placement or requeue) is ended there, so it never runs later; one its machine may have begun is left to the line's lost-machine rules), a non-zero exit with its
  last lines, a kill, an approver who may not, a changed card. A failure tells who can act, once each under write
  ids fixed when it failed: the asking session while it is live, else the item's holder (else the asker's
  mailbox); the approver's person (never another person when a device's person cannot be resolved), in plain
  words: what did not run, zm's record of why quoted, whether it can run again and until when; and `ACTION
  FAILED: <reason>` on the item. A failure that cannot run again: the asker fixes what it can and asks again
  naming the old ask.
  **Retry** (`POST /v0/asks/{id}/action/retry`, `zm ask retry <id>`, body `card_sha256`). A run or command
  action that failed before it started may start again, identical, without a new card or yes. Only when it
  provably never started: approvals never handed it to the team module (a refusal there other than a request-id
  conflict, or a start it could not take within 10 minutes; the retry asks again under the same request id,
  which the line answers with any start it has, never a second), or the team module shows the attempt's line
  row ended refused with no session, no cancel, no requeue or lost machine, no recorded copy, and either never
  placed or placed once and unsent on a machine other than the authority (`retryable` on `action_state`); then
  the next attempt has a new request id. Never once anything may have run (carried, lost, ended, killed,
  cancelled), never an authorize-key, and never a refusal that would fail the same way (an unsigned yes, an
  approver who may not, a changed card). Its asker or a person who owns or administers the project may ask, with
  the card's `card_sha256`; nothing in the request can change the action. One conditional update claims it
  (state `failed`, `retryable`, unconfirmed, the retries read): two at once, or one racing a confirm, start at
  most one. At most 3 retries, and only until 1 hour after the yes (`retry_until`): a yes is for now, never
  hours later; never once an owner objected to the outcome. A recorded attempt moves to a new request id only while
  the team module still shows, read again then, that it was never handed to a machine. It runs the stored action
  effect through the same start, one start of an ask at a time: the card and effect hashes, the signed yes, the
  approver's grants now, the very device or key that answered still paired and active (a new device paired under
  the same name is not it), the same machine and as-approver rule. A run machine whose zm predates
  `commit_on_ref` refuses a moved ref as before (`forbidden`, not retryable). A confirm ends retries; `action_state.retries` counts them. A late read of an earlier
  attempt never fails or finishes a later one (its updates are bound to the attempt's task). Decisions keeps an answered ask with an unconfirmed action among the open ones (a failed one on
  top as an exception, a done one for 24 hours) until a project owner confirms it (`POST
  /v0/asks/{id}/action/confirm`, no signature: it changes nothing but the list).
  **Risky actions**. An action that runs something or hands a brief may carry `risk`:
  `irreversible` (picking it cannot simply be undone, or it reaches people outside the team), `spending` (it pays,
  buys, charges or changes a subscription or price) or `unchecked` (released without the card judge's check, so
  nobody knows). The asker declares it (`zm ask new --risk irreversible|spending`; another value, or a risk on an
  ack-mail action, is `400`); the card judge answers `risk` for every action card (cardjudge.md question 7, eval
  cases in testdata/cardjudge with `want_risk`) and the higher of the two holds (none < irreversible < spending < unchecked; the judge's is its votes'
  majority, as for pass and fail; a split is unknown and takes the highest any vote names): the judge raises it, never lowers it, and zm never reads it from the card's words. A card released
  unchecked (`release-unchecked`) becomes `unchecked`. With a risk the daemon freezes `confirm_word`: the action's
  `machine`, else the project's slug. Both sit on `card.action` before anyone is asked (at creation, or while held)
  and `card_sha256` covers them; the card's text and `action_text` add "This cannot be undone: to choose A, type
  <word>." The answer that picks the action's choice must carry `confirm` (`decide`'s body, or the signed
  `zm-answer/1` JSON) equal to `confirm_word` (spaces around it ignored), else `409` with data
  `{"reason":"confirm-word"}`; other choices take none. A `409` of `choose` (and of the signed answer, but for the 409s of its release of a held card, which carry no data except "another card") says its reason in data
  (`card-changed`, `held`, `joined-action-ask`, `withdrawn`, `default-stands`, `confirm-word`), except an ask
  already answered differently (its data is the standing Decision) and a write a team replica forwarded and lost or
  replayed (no data). `choose` also takes an optional `card_sha256`
  (the card the person read): when it is not the ask's current card the answer is `409` `card-changed` and nothing
  is recorded. The phone page keeps that choice off until the word is
  typed. Clients (the Mezze app) read `card.action.risk` (any non-empty value: ask for the word) and
  `card.action.confirm_word`. It guards against a slip, not an agent: an agent can read the word. A repeat joins
  only a first card whose action has the same risk (else it is linked). Cards made before this carry no risk.
  **Approved, not done**. A yes is not the work: "I think I already did this" closed an item
  whose app side was still not live. A card may carry `verify`: `command`, one printable shell line of at most
  1 KiB that exits 0 only once all the approved work is done, and `on`, its choice (the action's, else the
  first; an ack-mail card takes none). It is frozen at creation (`card_sha256` covers it) and the card shows it
  word for word; an ask with one takes no `apply_default`, and an ask that joins it does not carry it (Routing). Picking a choice that needs work (an action that runs something,
  or `verify.on`) adds `APPROVED, NOT DONE: zm keeps ask <id> open in Decisions until ...` to the decision's
  comment and mail. The state is the same `ask_actions` row: a verify-only ask gets one (kind `verify`) in state
  `approved` at the decision; an action with a check goes to `approved` instead of `done` when it ends ok. The
  asker runs `zm ask check <id>`: zm runs the frozen command with `sh -c`, as the caller, for at most 10 minutes
  (its asker's by default; `--yes` for anyone else), only once any action finished, and reports `POST
  /v0/asks/{id}/check` with the card hash read, the exit (required) and its last 2 KiB; only the asker or who may
  operate on the area reports. The check is reported, not proven: zm records who reported it (`checked_by`) and
  names them in the DONE record. A report while the action is still queued or running is `409`. An exit of 0 while
  `approved`, or `failed` by its machine (a refusal, an exit, a lost machine: the asker did the work another way;
  not one that never started or was given up), makes it `done` with `verified_at`, and `DONE (verified ...)` is commented on the item once, under
  a write id fixed at that moment and retried by the ticker; an action with no check records `DONE` the same way
  when it ends ok. Any other exit is kept (`checked_at`, `check_exit`, `check_output`) and changes nothing.
  `--give-up "<why>"` (a reason, no exit) fails it (`ACTION FAILED`, on top until an owner confirms), also an
  action that is stuck queued or running (zm does not stop a task that already started). The ask has `not_done` while
  its state is `queued`, `running` or `approved`; confirming it is `409` until then. Undone work comes back,
  each step once, timed from the decision by the project's `[asks]` table in project.toml
  (`remind_after`, `relane_after`, `escalate_after`: durations from 1 minute to 30 days, in that order; defaults
  24h, 48h, 72h): at `remind_after` the asker (a session, else the item's holder) is mailed `APPROVED, NOT DONE
  ...`; at `relane_after` zm hands it to a **fresh lane**: an agent task (`lane_agent`) on `lane_machine`, started
  as the approver through the action's own task start (request id `relane-<ask>`, parent the asking session), with
  a brief that carries the card and ends in `zm ask check`. `lane_machine = "vm:<size>@<machine>"` (a sandbox task
  size, required) makes each lane a sandbox task of that size on that machine ("Sandbox tasks",
  SPEC 8), never a task on the machine itself: the box, what it holds and its release when the task ends (60 s
  after a stop, then HUP and KILL) are the task driver's; the machine passes the checks below, and `relane_machine`
  is the machine. `lane_egress = ["host", ...]` (only with a `vm:` lane; the sandbox task's `egress` rule, checked at
  load) is the hosts each such lane's box egress allowlist adds.
  Only work whose check is what is left (`approved`) gets
  one, never an action zm still runs; only on a signed yes; only on a ready machine that is nobody's or the
  approver's own (joined by no one else's token, and not marked another person's by `zm machine-class`: a person's
  machine takes work only on that person's yes). When none can start (so with no
  `lane_machine`, at `relane_after`), `relane_reason` says why and the approver hears at once; else at
  `escalate_after` the approver's person is mailed, and it moves to the top of Decisions. A lane runs as the
  approver's person recorded at the decision, never looked up again by device name; its brief carries the check's
  last exit, never its output. Work still queued or running at `relane_after` gets no lane (zm runs it). Each step counts in `zm measures` (row `not_done_steps`, one line per step, for the weekly
  report). A wrong check no longer keeps finished work open: the asker, or a caller that names no session and owns or administers the
  project, reports `evidence` (one plain line, at most 500 bytes; no exit, no give_up; `zm ask check <id> --evidence
  "..."`) in a state a check may close (`approved`, or failed by its machine after it started; the latter also on a card with no verify check, for work done by hand, but then only by a person who owns or administers the project, never the asking agent), which makes it `done` without `verified_at`, and the DONE record says `recorded with evidence,
  not its check`, quotes it and names who gave it. A reminder or DONE record that did not land is tried again every
  10 minutes (a DONE for an item that is gone is dropped). Decisions lists not-done asks on
  their own, after the open asks (rank 2), and a done one for 24 hours after it was verified.
  **Whose turn.** Every ask read carries `next`, one of `you` (the person it is for), `agent` (its asker, a fresh
  lane or zm) or `nobody`, derived from the records above and never from text, one state at a time: open (to
  answer, or for an owner to release a card the judge could not check) is `you`, held for an outside party
  `nobody`; queued or running is `agent`; approved, not done is `agent` until it
  escalates to the approver, then `you`; failed is `you` until an owner confirms it; withdrawn, confirmed, verified,
  done with evidence or answered with nothing tracked is `nobody`. `ran_unchecked` is an action that ran to done
  with no verify check and no evidence, before an owner confirms or objects (`you`: Looks right is
  `action/confirm`, Something's wrong is `object`). `your_step` is approved work waiting on the person's own step
  (`you`). zm mails the person only when it becomes their turn (an escalation, a failure), never while it is an
  agent's. An ask from an authority older than this has no `next`: a client reads that as unknown.
  **Your step.** Some work only the person can do with their own hands: a sign-in, a press in a service's own
  console or account settings, accepting on their own account. A card carries it as `owner_step` (`zm ask new
  --step-label --step ... --step-link https://... [--step-minutes] --verify ...`): a label, 1 to 8 plain numbered
  lines (no ids, hashes or paths), an `https` link on the service's own site (an ASCII host name, a non-ASCII
  one in its `xn--` form, never an IP address, another port, a user, a password or a `#` part; zm stores its
  host as `domain`, which a client's button shows: "Open github.com") and minutes. It needs a verify
  check and a person in `to` (any member of the project, not only an owner; their machine need not take tasks),
  and takes no action: the step is the card's work, one per card. The card judge reads it (the full link and its
  domain) and counts the step carried only when no agent with access or with the person's key could do it instead, its steps reach the end, the
  domain is the named service's own and the card has a verify line; an owner_step an agent could do, a link to
  another site, or a password typed into the card fails. What someone outside the card does later because of it
  (an invitee accepting) is not a step of the card. After the yes the ask is approved, not done and `your_step`
  (`next` = `you`). The person's "I did it" (`POST /v0/asks/{id}/step`, `zm ask step-done`; only the person it is for, by
  name or directory person, never an agent session or another person) records `step_done_at` and mails the asker
  to run the check (a second press within a minute records and mails nothing new);
  it closes nothing, and `your_step` comes back if a check after it fails. Only the verify check (`zm ask check`)
  records it done: evidence (`--evidence`) is refused for it, and no fresh lane starts for it (the approver hears
  at once instead, since it is their turn). A card with a step joins an open card only with the same step.
  **Person steps** (`zm ask bundle`). A manual step for one person with no real choice in it (install an update,
  accept an invite sent to them, turn on a setting, install an app on their account) is not a message and not a card
  of its own: it waits at the project's authority as a person step until one daily steps card per person carries
  every pending one (a hand ask: one press hands them all to the person's own agent). `zm ask bundle add --to
  human:<name> --work <id> --title <line> --brief <file> --check <line>` (`POST /v0/steps`) needs write on the work
  item and names a member of the whole project (a caller who reads all of it); title is one plain line of at most 80
  bytes (what the person reads), brief the numbered steps to done for their agent (at most 900 bytes), check one
  printable shell line of at most 200 bytes that its adder runs later, so it reads the result from where the adder
  can see it. The plain limits are checked first; then the **step judge** (`internal/approvals/stepjudge.md`, eval
  cases in `testdata/stepjudge`, three votes at once, majority as for cards) answers a fixed schema:
  `needs_the_person` (not something the adder could do without them), `agent_doable` (their agent can finish it on
  their machine and accounts; nothing needs their own hands), `numbered_to_done`, `self_contained` (says where every
  link, version or value comes from), `no_real_choice`, `check_proves_done`, `title_plain`, `risk` (none,
  irreversible, spending; the highest any vote names) and `pass`. The verdict is zm's rule over the answers: a pass
  only when the model passed it and every answer is fit, each unfit answer adding zm's own fix; a real choice leads
  ("file its own ask"). A fail is `400` with the fixes; no answer (or one outside the schema) is `503` and nothing is
  stored. `--draft <id>` completes a draft step (its person and work item stay); `--from-ask <ask>` moves an open,
  unanswered hand ask with a verify check, for one person, on a work item (its asker or an operator) into a step,
  its brief and verify line taken whole (title: its card's unless given), and withdraws it; unless this call is the
  one that withdrew it (it was answered or withdrawn meanwhile), the step is removed and the add is `409`. States: `draft` (a refused report saved whole), `pending` (judged, waiting for a card),
  `carded` (on a filed card: `bundle`, `number`, `ask`), `reported` (the person's agent said done), `done` (its
  check passed), `failed` (its agent said it failed, or a check failed after a done report), `dropped`; the table's
  checks keep a carded, reported or failed step on exactly one card number and every step but a draft with a title
  and check. A step is read by who may operate on its area, its adder and the person it is for (anyone else gets
  `not_found`); `zm ask bundle ls [--to] [--state]` (`GET /v0/steps`, oldest first; default every state but done and
  dropped), `drop <step>` (its adder or an operator). The person's agent reports each step of a card: `zm ask bundle
  report <card> <n> done|failed "<note>"` (`POST /v0/bundles/{id}/steps/{n}/report`, `<card>` its bundle id or its
  ask; the person, any of their sessions, or an operator; one plain line of at most 500 bytes) records it, comments
  it on the step's work item and mails its adder (done: "run `zm ask bundle step-check <step>`"); a report never
  makes it done. `zm ask bundle step-check <step>` runs the check line as `zm ask check` runs a verify line (its
  adder's command; `--yes` for anyone else) and reports its exit (`POST /v0/steps/{id}/check`): 0 makes any pending,
  carded, reported or failed step `done` (STEP DONE on its work item); another exit after a done report makes it
  `failed`. `zm ask bundle check <card>` (`GET /v0/bundles/{id}`: the steps the caller may read, and `done` for the
  whole card) exits 0 when every step of the card is done or dropped. The store API the daily card and the mail redirect use, at the authority (`internal/approvals`):
  `saveDraftStep` (a refused report to one person, saved whole: sender, work item, body), `pendingSteps` (one
  directory person's pending steps across the authority's projects, oldest first), `cardSteps` (pending steps onto a
  filed card, numbered 1..n in the order given, all or nothing; a card cut again returns its steps first),
  `returnSteps` (a card's carded steps back to pending: "Not today", or withdrawn to be cut again) and `bundleSteps`
  (a card's steps in order, by bundle or ask id). Bundle ids are ULIDs minted before the card is filed, since its
  verify line (`zm ask bundle check <bundle>`) is frozen into the card.
  **Card judge** (owners are not coders). At the authority, before a new card is stored (a join shares
  one that passed), a model reads the whole card as the person reads it, its action in full (brief, command,
  plan), its verify line, and the approver with their machines (`run-targets`), and answers a fixed schema
  (`internal/approvals/cardjudge.md`, with eval cases beside it): could a smart person who does not code decide
  from this card alone (what it is, why now, what each choice changes, cost or risk in plain words, on a face of
  about 120 words: a face past 140 words, counted by zm over question, context, labels, effects, why and
  `after_press`, fails with a fix naming what to move to background); does a
  choice leave a person a step (a permission click is not one: the single press only an account's owner can give
  on a service's own consent page, such as Google's Allow, which the card discloses and its action brings up and
  carries on from), and does the card's action then do all of it after one press; and, when a step
  needs the approver's machine and none of theirs is ready, does the card carry a complete `hand` brief and a
  verify line. The same call answers three more: whether the card only reports or
  asks about a status (whether something works, ran or finished), is bookkeeping zm or an agent can do (marking
  an approval done, filing a work item) or a choice the agent can make alone (cheap, reversible, inside its own
  work), so it asks no decision: a fail, "put a status in a report or a comment";
  which of the project's **owner rulings**, if any, already answers it (a fail whose fix names the ruling and
  quotes it; the judge never tells an agent to proceed); and which open card of the same project and area (first
  asks of their groups, not held, not answered, not for an outside party, that the asker may read: all of them for
  who may operate on the area, else its own and those put to it; the 30 newest) asks the same question. A repeat
  for the same person (or both for the owners), not for an outside party, with no attachments and no verify check
  of its own (or the first's same one), that carries no action, or the same action on
  the same target (the frozen actions compared field by field, commit included, less display fields), **joins**
  that card as `zm ask join` would (`note` says so, and names the first's action when it has one), even when the
  card alone would fail on questions 1 to 3; a ruling or status report still sends it back. Any other repeat is
  judged as its own card and, if sent, **linked** to it (`linked`, shown to who may operate on the area). A held
  card found to repeat one when judged again joins it in place, under the same rules, and is sent as a joined
  ask; if the first is answered or withdrawn before it goes out, it is withdrawn and its asker is mailed that
  answer (or told to ask again). A ruling's bounce quotes its text to any asker, guests included: rulings are the
  project's standing answers, not secrets. A ruling, status or open card outside the input, a missing answer, or votes with no
  majority on which card it repeats hold the card. Owner rulings are data, never code: `[[rulings]]` tables
  (`name`, lower-case letters, digits and dashes; `text`, at most 1000 bytes; at most 64) in the project's
  `project.toml` at its authority, read when the project loads. **Changing them is an owner decision**: an agent
  proposes the change as a card (`zm ask new`, the ruling's exact text in it); only after an owner's yes is the
  file changed: `zm project ruling add <name> --text <ruling> --ask <id>` (`POST /v0/rulings`) takes the ask only
  when it is the project's, answered plainly with the choice its card proposes (its action's, else its recommended
  one) on a paired device or with a key by someone who owns or administers it now, and carries the ruling's name and
  text whole, each in double quotes (as a `[[rulings]]` table quotes them). A person who owns or administers the
  project may take them from any field of the card; anyone else who operates on all of it (a member, an agent
  session: owners never type commands, and the signed ask is the authority) only from the brief or plan of the
  approved choice's action, which would run with the owner's power anyway. One ask adds a ruling of a name once,
  ever: a removed ruling takes a new yes. The ruling records the ask (`ask`). `zm project ruling rm <name>` removes one (a person who owns or administers the project, never an agent session; no
  ask: a removal only sends more cards to the owners), `ls` lists
  them; a member machine forwards all three to the authority. Both rewrite `project.toml` at the
  authority and take effect from the next card, with no restart. A pass whose three answers do not add up is a fail. A fail is `400` with the judge's fixes,
  numbered, and a hint naming the flags and the approver's ready machine. No answer (a timeout, no provider, a
  model error, an answer outside the schema) never passes: the ask is stored **held** (`note` says so): no
  mail, no event, nobody it is for is asked, and deciding or joining it is `409`. It is judged again on the
  retry ticker, backing off to every 30 minutes: a pass sends it (its mail from its asker, then `ask.created`), a
  fail withdraws it and mails its asker the fixes; withdrawing a held ask sends nothing. **A held card is never
  silent**: from the moment it is held it is listed in Decisions for who may operate on it (the
  project's owners), marked `not_checked` with the judge's reason, after what is open and
  deadlines first, and not counted in the day's asks. Each hold is logged with its reason; once a card has waited on the judge over 15
  minutes (`ZM_JUDGE_NOTICE_AFTER`, a Go duration of at least a minute; an unreadable one is ignored) health carries `card_judge` (`zm status` warns) and
  the project's owners get one notice ("N cards held over 15 minutes", with the oldest's reason: a down judge and a card it keeps splitting on look alike), told again only after
  no card is held that long and one is held again. Each card is judged **three times at
  once** and the majority's verdict is the card's (a split around a vote with no answer is held): a small model
  answers the same card differently now and then. The verdict is the producer's rule over the model's three
  answers, never its own `pass`. The prompt's eval cases (at least 8 pass and 8 fail) must each be stable over 3
  live runs (`ZM_JUDGE_EVAL=1 go test ./internal/approvals -run TestCardJudgeEval`). **Release.** An owner who cannot wait
  releases a held card that is `not_checked`: a paired approver device's `POST /v0/asks/{id}/decide`, or a key approver's
  signed `POST /v0/asks/{id}/answer`, with decision `release-unchecked` (as an answer that runs something, never the
  socket owner's `choose`, which any agent of the owner's OS user reaches). The card is then sent as a passed one, the
  release is recorded with the approver and logged, the judge failing it afterwards cannot withdraw it, and its default
  never applies by itself. zm never releases or passes an unchecked card on its own, and no default applies to a held one. The model is the provider
  CLI on the authority's own plan login (`ZM_JUDGE=claude[:<model>]`, default `claude:haiku`; `none` holds every
  card; `codex` is refused, as `codex exec` always offers its model a shell and tools), never an API key, with no
  tools and an allowlisted environment (`PATH`, `HOME`, user, locale, temp dir, the provider's config and login
  variables, proxy and CA settings; never the daemon's keys or zm session) (`internal/judge`, the one judge
  helper).
  **Card router**. After the judge passes it, an operator's card to the owners with a topic that names no
  outside party, person, deadline, default or action, in an area whose operators' asks were held for any outside party, is
  read by a second fixed schema (`internal/approvals/cardroute.md`, eval cases in `testdata/cardroute`), in what is
  left of the judge's wait and on one of its slots: is its question one of those parties' to answer (their own rules,
  schedule or facts, which the owners could only pass on)? The parties are that area's own records (each group and
  contact its operators' open or answered asks, not held by the judge, were held for, the 20 most recent; never a guest's), never a list in
  code. Three votes;
  when every vote that answered (at least two) names one party, the ask is stored held for that party (`for`, as if asked so;
  its slot is the one it asked for), and the asker's `note` says so. Anything else (one vote to stay, too few answers, a split; an
  answer off the list or out of shape counts as a vote to stay) leaves the card with the owners: a wrong stay costs a pass-on, a wrong route hides the owners' question.
  A held card is routed when a later judging passes it, before it is sent (only an operator's, as its slot shows).
  **Hand briefs** (`hand`): for an approver with no machine that takes tasks, an action of `agent` (claude or
  codex) and `brief` (at most 8 KiB, numbered steps to done, any one-time setup included), no machine, and a
  required `verify`. zm runs nothing for it: picking its choice takes a signed answer (as a running action) and
  records approved, not done; the Mezze app's Decisions offers "Hand to my agent", which starts a thread on the approver's
  own Mezze app with the brief, preapproved. `zm ask new --hand --agent claude --brief <file> --verify '<check>'`.
  **Run targets**. `GET /v0/run-targets` (`zm ask machines [human:<name>]`), at the authority, for who
  may operate on the whole project (an area member is `403`: it learns nothing of who has a machine here): each
  machine of it with `people` (the identities whose live node-bound tokens joined it, as `MachineJoin` counts
  them, each still granted; the authority's own machine is its human's), `trusted`, `remote_tasks`,
  `script_runs`, `command_runs`, `os_user`, `seen`, and `ready` (trusted and takes remote tasks, what a run action needs, and
  joined by one person: a name with several people's live tokens may be a reused name) or a `why` naming who
  fixes it; `?person=` (a name or alias of a human who reads the whole project; anyone else is `404`) keeps that
  person's. A `run` or `command` action with no `machine` runs on the card's person's (`--to human:<name>`, `owner`, else the
  item's `owner:<name>`; a whole-project caller only) only ready machine (one that also runs scripts, for a
  script; commands, for a command), frozen on the card; no person, none or several is `400` with the list, never a guess. A card
  judge refusal's hint names the person's ready machine and the flags. An agent's `report` to a person (SPEC 9 "Reports") whose body asks them to do a step
  (the mail-chore judge's question, `internal/orchapi/mailchore.md` with eval cases beside it) is `400`
  with the judge's fixes, pointing at `zm ask machines` and the action form, at the authority and on a replica before
  it queues: a report is read later, if at all, so its step is lost. No answer is `503`: the report is not sent,
  never filed unchecked. A reply passes. Every judge asks the provider CLI on the daemon's own plan login
  (`ZM_JUDGE`, `internal/judge`), at most 4 at once per daemon.
- **Standing permissions** (`zm permit`). A permit lets zm do one kind of act it performs itself
  (a task start, a machine choice) without a card each time, while every condition of its kind holds from zm's
  own records; it never covers an act an agent does with its own tools (a merge, a push). Kinds are a closed list
  in the daemon (`internal/approvals/permits.go`), each with frozen text and conditions, and a `sha256` over
  name, text and conditions with their versions; a permit whose kind's hash changed is `void`. Stage 1 kinds:
  `lane-start` (the card's action is an agent task zm starts on the machine it names, about a work item zm reads
  now from a current copy, not done, created before the card by someone other than the asker) and `machine-pick`
  (the card's action is an agent brief or a script at one commit, never a free shell line, and runs on a machine
  that carries the box class `no-secrets`). A box class is a fact about the machine an owner set, never a
  claim on a job. Granting a kind for N days (1 to 365), renewing it, widening it, and putting a class on a box
  all take a signed answer: `GET /v0/permits/card` gives the exact card (`zm permit ask` files it unchanged but
  for recommend, why and searched), and `POST /v0/permits` with the same target and the ask's id records the
  permit or mark only if that card was answered `yes` with method `approver` (a paired device or key, never the
  owner's socket) by an approver whose person (its `--for` identity; a device paired for no one is no one's and
  never grants, renews or widens) owns or administers the project then. Each ask grants once, ever;
  an answer given before the latest revoke or unmark of its kind or box no longer grants; a permit's days count
  from the answer; a box must be one of the project's machines (`run-targets`). A grant for a kind with a live
  permit replaces it (`replaced`). Revoking (`POST /v0/permits/{id}/revoke`) and taking a mark
  off (`POST /v0/permits/boxes/unmark`) take one unsigned press by an owner or admin. `GET /v0/permits` lists
  kinds, permits (`live`, `expired`, `revoked`, `replaced`, `void`) and marks to who may operate on the whole
  project. Revoking any permit of a kind ends every live one of that kind. A grant counts only a plain yes (no
  free text, not answered for a contact).
  **Dry run.** While a kind has `dry_run`, zm skips no card for it, granted or not: for every new card whose
  action is `run` or `command` (not a join), zm records per dry-run kind whether a permit would have covered it,
  the first failed condition, and the target the use would be bound to (work item, machine, commit, brief hash).
  Any fact zm cannot read in 3 seconds is a failed condition. `GET /v0/permits/dryrun` (`zm permit dryrun`; the
  weekly report's row) counts per kind over cards asked in a window (default 7 days): checked, would cover, and of
  those saved (a person picked the choice the permit would have taken: the action's), wrongly covered (any other
  answer: no, another choice,
  free text, sent back), open and withdrawn, plus not covered by failed condition.
- **Answers.** A decision on a card ask is a choice's key, optionally with free `text` (at most 2000 bytes); text
  alone records `other`. `as` is `owner` (default, "My answer") or, with `for`, `contact` ("John said"). A pull
  request's ask takes neither. An ask's `status` is `open`, `answered`, `recorded` or `withdrawn`.
- **Answers land on the work.** Signed decision and immutable per-effect intents commit together. Comment,
  original ask reply, captured holder notice and decision event have separate random server-owned IDs,
  payload hashes and `pending|succeeded|failed|unknown` states. The holder destination is captured at decision
  commit and never retargeted by retries. Native comment identity reaches the final native transaction;
  mail consumes its existing internal IDs; the event sink records a durable delivery hash and event cursor.
  Death after a sink commit replays the same identity, without duplicating the effect. Failures retry
  independently with backoff (30 s to 1 h); unknown outcomes require reconciliation. Pending deliveries
  from before the effects migration stop as unknown, preserving receipts and avoiding ambiguous replay.
  `decision.deliveries` reports kind and state without IDs or private recipients; `write_error` and
  `mail_error` report outstanding failures. `recorded_at` means all required effects succeeded; comment
  success alone means work is recorded even if notifications remain pending. Redaction scrubs stored
  effect bodies; changed unfinished intents stop for reconciliation rather than reviving old text.
- **Content-card count.** `zm ask content-count [--days 7 | --since D --until D] [--tz <zone>] [--from asks.json]` (the window is the last N whole days, today left out; `--until` is exclusive) counts, over a past window, how many content cards (a card that asks a person to approve wording before it goes out to people outside the team) one daily card with a yes per item would have merged: content cards less the days that had any. A count only: no card changes and nothing is sent. An agent judge (`internal/contentcards`, Haiku by default, a fixed schema, eval cases in its `testdata`) decides which cards are content; a card the judge cannot answer is held, counted as held and never as content, and the report says it is incomplete. `--json` prints the report and the one weekly-report row. The daemon lists at most 200 asks, so a window past that is read from `--from` (a list, or `zm ask ls --json`). Day boundaries come from `--tz` (default UTC), never a built-in zone.
- **Routing.** Fewer, better-aimed asks; whether two questions are the same or an earlier answer
  settles one is the asking agent's call, never the daemon's. Before asking, an agent reads `GET /v0/asks?q=`
  (`zm ask similar <words>`): the first asks of groups it may read, answered or open, ranked by shared words. An
  answered one that settles it is its answer (recorded as an `AGENT DECISION` naming the ask; nobody is asked). An
  open one with the same question it joins (`same_as`, `zm ask join <id> --work <its item>`; the card judge also
  joins a repeat it finds, above): its own ask, mail and slot, with the first's card, priority and owner, less the
  first's action and verify check, which stay the first's (its action runs once, on it). A joined ask of an open card
  that carries an action is not answered itself (`409` naming the first; once the first is withdrawn or answered
  it is answered on its own); a signed answer to that first ask reaches its joined asks; the inbox lists it with `same_as`, and a client shows the group as
  one card. A join and an owner's group answer are ordered, so a join never stays open behind an answered group. The owner's answer (`choose`) to any ask of
  a group answers every open one, each with its own receipt, comment and mail; a paired device's or key's answer
  stays on the one ask it signed, except a signed answer to a first ask that carries an action, which reaches its
  joined asks (they carry none). Joining an answered ask is `409` carrying it. An ask is for a person
  (`owner`): one named (`<name>` or `human:<name>`), or by default the person its work item's `owner:<name>` label
  names; either must be a person who owns or administers the project, else nobody, meaning the project's owners.
  A person is the human identities linked to one GitHub account (else one identity), plus the accounts live-linked
  to it as data (**person links**, below), and any of their identity
  names, aliases or GitHub login names them, in any case; a name that is no such person, or several, is `400`
  listing who can answer. The ask carries `owner_person` (that person's id, as `GET /v0/people`'s) and
  `asker_human` (the name of the person whose token or machine asked; both hidden from an area member). A note card's ask may also send `thread_title` (the asking thread's title, one line, ≤200 characters) and `pr_url` (`https://<host>/<owner>/<repo>/pull/<n>`, ASCII, no port, query or fragment, host github.com, git.cafe or one of `ZM_FORGE_HOSTS` in the daemon's environment) beside the card (`zm ask new --thread-title --pr-url`, MCP `ask_new`'s same-named fields; a client retries once without them when an older daemon answers `unknown field`); the ask returns them with `work_title`, its work item's title as one plain line as it read when asked (empty when the ask is filed in another area than the item's, or the title reads as a decision marker). They are display fields a client renders as an About line and links: no card text, no part of the card hash, never judged. `zm people`
  and MCP `project_current`'s `people` list who can answer: the human identities that own or administer it, names,
  aliases and GitHub login. A note card's `to: "human:<name>"` naming anyone but the asker's own human
  is the same as `owner` (both given must agree); so is a bare `to: "<name>"` (any name, alias or `@login`). A note
  card's `to: "me"` is the person who started the asking session: from it along `Parent`, the first session
  whose `launcher` is a person who answers the project; a remote task's own launcher is the machine that ran it, so its
  parent (the authority's task line) is next. A caller that is no session is its own identity. None found falls to the
  label, then the owners, and the reply's `owner_via` says which; a box's, a runner account's or a guest's session never
  names the host's human. An ask with a person goes to that person alone (`to: "human:<name>"`;
  an ask directed at one person must not reach everyone); the answer still reaches the asker, sent as
  that person (an agent reaches a person this way). Only a caller who reads the whole project names a person: an area
  guest's `owner` is `400` whatever the name, so it never says who has an account (its `to` its own human still goes there, naming nobody); only one who may operate on the
  area has the label read. The inbox carries it as `owner_name`, `zm ask show` and the send line as `for
  human:<name>`. It only aims the card: who may answer is unchanged.
- **One person, several accounts.** `zm identity person-link <account> --person <main>`
  (`POST /v0/person-links`, at the project's authority) says an account belongs to the person whose main account
  is `<main>`, with no GitHub link needed (a box's OS account, a second login). It is live when the main account
  agreed (nobody is given an extra account without their say) and so did the linked account itself or someone who
  administers every project either account is in (checked when recorded; it stays until removed; so a main
  account that administers all of them makes an account its own alone, and that account can remove it); an
  account never labels itself as someone else. One level among live links: a main account is never itself linked; a pending
  claim blocks nothing, and only the account itself or an admin moves a live link or the account's own claim. A verified GitHub link wins and a link that contradicts one is `409`. A live link folds the
  accounts wherever people fold: `GET /v0/people`, an ask's `owner_person`, and a decision's `approver_person`
  (the answering person's id, worked out when read from the links then, shown to the readers `owner_person` is
  shown to; empty when the answer is the authority's own account only by default, a phone paired for no one
  or a tap on its own socket, and that account is linked: anyone at the box answers as it), so a card for
  the person answered from any of their accounts is their answer, not someone else's.
  Inside approvals the folded person also aims things: naming any of its accounts (`--owner`, `to: "me"`) finds
  the person, and its run targets are every account's machines. It **grants nothing**: answering, owning and
  every right stay with each identity's own grants, and a signed answer stays required where it is. Private
  mail follows consent only: `--share-mail`, run as the account whose mail it is, adds the other account of that
  live link as a participant of this account's for-you mail from then on (pairwise along the link: a linked
  account's mail to the main account, the main account's to that linked account), so the other account reads it
  where it can read the project and area (inbox, Decisions, Got it marks it read once; never the sender's own
  copy). Like a cc, the mail's participants then name that account to everyone on the thread, the sender
  included: sharing tells people who mail the account that the other account reads it. A verified GitHub link
  made later that contradicts the link stops the fold and the sharing. Mail is never shared into the
  authority's own account (anyone at the box, and a phone paired for no one, reads as it). The switch lives on the link: moving or
  removing it, or `--no-share-mail`, stops new mail; mail already shared keeps its participants. Nobody else
  turns it on. A caller who is not an admin of every project both accounts are in names only an account it
  shares a project with and never moves a link it is no party to; a missing account, a hidden one and a link
  it may not make all answer the same `404` (recorded nothing). Links live at the authority; a replica shares
  and lists nothing by itself.
- **Measures (`zm measures`).** How often cards and mail needed a person, from zm's own records, read
  through the REST door and computed by `internal/measures` (a pure package: rows in, tables out). `zm measures
  [--days 7 | --from D --to D] [--tz Z] [--md] [--json]` prints, per day and per owner (a card is filed for its
  `owner`, answered by its approver, so the two are kept apart; a card joined to another is one press, a send-back no
  answer; owner names are merged through `zm people`; one project, `-P` or the one you are in): cards filed; cards answered; answered with the
  recommendation (a rate, per kind); withdrawn within `--withdrawn-within` (default 10 minutes); repeats of an open card
  (zm's own join record `same_as`, or a judge's `repeat_of` label); the median time to answer; yeses whose work was
  still not done `--not-done-after` (24 hours) after the answer; mail sent to ended sessions (route "orphan of
  <session>"); group mail nobody took within `--group-untaken-after` (24 hours; the mail the expiry sweep picks);
  and harm, an owner's press after the fact. It reads every card, past the list's 200: `GET /v0/asks` takes `limit`
  (up to 1000) and `before_id`, and answers `next` while pages remain; `GET /v0/mail` takes `since` and `until`
  (RFC 3339, inclusive). A card's kind and repeat come only from labels a judge stored (`SetLabels`; keys `kind`,
  `repeat_of`, at most 8 labels of short words): no label reads `unlabelled`; nothing is guessed from words. Days are
  cut in `--tz` (default the machine's zone); no owner, project, zone or limit is named in code. Harm is
  `POST /v0/asks/{id}/object` (`zm ask object <id> [--note]`): an owner or admin (never a session) records, on an answered card, that its
  outcome should not have happened (one record per card, the first stands; it undoes nothing), plus an applied
  default an owner undid; and `not_done_steps`, what zm did with approved work still not done (back to the asker, a
  fresh lane, no lane could take it, back to the approver, done with evidence). A row is one `measures.Row` in `internal/measures/rows.go`: adding one is appending it.
  A daemon that cannot page (older than this) is refused; `--allow-short` reads its newest 200 cards and 500 mails instead and the
  report opens with a NOTE saying how far back it saw.
  `zm measures report [--to owners]` files the same table as a `report` mail to the project's owners, once per
  window (a second run says so); schedule it weekly with any scheduler.
- **Inbox.** `GET /v0/decisions` (`zm ask inbox`) is the one list: open asks (a deadline first, then priority,
  oldest first), then asks held for an outside party, by group, then work items waiting on a person with no live ask, then what was
  answered. Waiting means not done, not an epic, and waiting on the reader: `owner:<name>`, `ask:<name>` or
  `needs-<name>` for one of the reader's names (its identity's name, or an alias set with `zm identity alias add`,
  matched exactly under the name rules above), `ask:owner` for an owner of the item's project (by the identity's own
  grants: a replica token is capped below owner), or, on an item that names no one and is not labelled `ruled`,
  `owner-decision`, `human`, `questions`, `decision`, `needs-night-ruling`, `owner-run` or a tracker type `decision`.
  A person's lists (`for=me`, `for=everyone`) hold only steps: a waiting item that is a question (`owner-decision`,
  `questions`, `decision`, `needs-night-ruling`, any `ask:<who>`, or type `decision`) and carries neither `human`
  nor `owner-run` waits on an agent until someone writes its ask. `?for=agents` (`zm ask inbox --agents`) lists those
  not labelled `ruled`, and no asks: the agent writes the card (`zm ask new --work`), or, where the item states a default and nothing
  irreversible hangs on it, records an `AGENT DECISION` with it and closes the item.
  `needs-<x>` names someone only when `x` is an identity's name or alias at the authority; `needs-spec`,
  `needs-design-pass` and the like are plain tags. `?for=everyone` (`zm ask inbox --everyone`) lists every person's waits; `?label=` replaces the list. An alias
  lives with the identity at the project's authority, applies at every project there, and only picks among rows the
  reader already reads. So only the authority's owner, an owner of every project the identity is in, or the identity
  itself when it owns every project on the authority sets one (ownership by the identity's own grants,
  `core.Deps.ProjectAdmin`, so from a member Mac's capped link too). Anyone else never claims its own (first come
  would hand out short names), and the setter is recorded. An alias
  and an identity name never look alike, whichever came first.
  Reading it gives no right: an ask shows under the restricted-mail rule, a work
  item by read on its area. Visibility and rank apply before the 500-ask SQL bound. Waiting work consumes
  at most 20 pages; the merged result is capped at 500 and `skipped` explicitly says when more remain.
  A member's socket merges every authority's answer (`skipped` names one that did not).
- **A person pairs their own phone.** `zm approver pair-me [name]` (`POST /v0/approvers/self`)
  reserves an approver for the caller's own identity, from any machine: a member forwards it to the project's
  authority, where the node-bound token says who the caller is. Never for anyone else, and never from an agent
  session (a session's address is refused). The answer is a pairing code and the authority's page; the phone
  pairs there and stays `unconfirmed` until an existing approver on another device taps confirm (the card names
  whose phone it is). A self-added phone never takes the daemon's one bootstrap. Nobody needs a terminal on the
  authority.
- **Where approvers decide.** Approvers live per daemon and decide only the asks their daemon decides;
  a joined project's asks are decided at its authority. `zm approver ls` names the daemon its approvers are paired
  on and each joined project's authority; `zm approver ls -P <project>` names the machine that decides that
  project's asks and the approvers there: all of this daemon's, or at an authority only the caller's own
  (`GET /v0/approvers/self`, a person never a session; the full list stays local to the authority). Approvers do
  not follow a person to other daemons: a device binds to the daemon that saw its tailnet node pair.
- **Phone alerts.** A paired device turns on Web Push from its Decisions page (`GET
  /v0/approvals/push-key`, `POST /v0/approvals/push`, `/push/off`; the page's service worker shows them). The
  subscription belongs to the device's person (`--for`, else the machine owner) and is stored with no token: each
  alert re-checks that the device is still an active approver for that person, so removing the approver ends it.
  The daemon that holds the item sends it, so a phone paired at a project's authority hears that person's new
  asks and news items in every project there, whichever Mac is asleep. The person hears only what is theirs: an
  ask's mail or a news item addressed to them (or to the owners, when they own the project), and their own
  sessions; a digest item waits for 08:00; the alert says only that something is waiting and opens the page.
- **Key approvers.** A cockpit (the Mezze app) runs on the machine its agents run on and speaks over the socket, where every caller is
  the owner, so its right cannot come from the caller. It holds a P-256 key (today a software key in a file the
  person's own OS user can read: nothing stops a local agent of that user from using it, see below). The key gets
  its right one of two ways:
  - **A GitHub sign-in.** The cockpit starts a login of purpose `key` (section 22) carrying the key
    (uncompressed X9.63, base64), a single-use nonce, the time and a signature over `zm-bind-key/1\n<the
    authority's receipt key id>\n<project slug as sent, or its id>\n<key>\n<nonce>\n<time>` (within 5 minutes). The
    start is refused unless the proof holds, the identity owns or administers the project, no other person holds
    the key and the nonce is new (taken last, so only a caller who passed the rest spends one; at most 64 per person
    per 20 minutes, in memory). When GitHub says who approved, the authority links the starting identity (never
    joining two people) and binds the key **active at once**, in one transaction, with the account
    (`github:<id>`, the value of record), its login, the machine and the node the login came from (a member's
    tailnet node, or `local` for the authority's own socket). The person's other bound key on that node, and their
    keys paired the old way, end (`removed`, `ended_why` `replaced`, `replaced_by` the new fingerprint), so a key
    bound behind the person's back ends theirs, and the cockpit shows it. The owners brick mails the person and the
    project's owners: a record, not an alarm.
  - **Paired the old way.** `POST /v0/approvals/pair-key` takes a pairing code from `zm approver add` on the
    project's authority, the key and a signature over `zm-pair-key/1\n<code, upper case, no dashes>\n<key>`; the key
    stays `unconfirmed` until an active approver on another device (`POST /v0/approvers/{name}/confirm`, which refuses
    the socket and tokens) compares its fingerprint (`p256:<16 hex>`) and confirms. An unconfirmed pairing is dropped
    after 24 hours and cannot be confirmed after that; a key paired this way ends (`ended_why` `legacy`) 7 days
    after this rule first ran at the authority, unless a sign-in binds it (in place, same name). The 7 days are
    checked when the key is used: from then its answers, new pairings and confirms of keys are refused, whether or
    not the sweep has marked the rows yet (the sweep publishes `dev.zm.approver.removed` for each). A bound key's name is
    `key-<machine>-<16 hex>`, or with `-2`... if someone took that name first (a bind never takes a row by name).

  `POST /v0/asks/{id}/answer` takes the base64 of a `zm-answer/1` JSON and a signature over `zm-answer/1\n` + those
  bytes. Before anything is looked up, the approver must be an active key approver whose key signed them, for this
  ask and this daemon's receipt key, within 5 minutes. On every answer, for every key: (a) its person (a key paired
  for nobody is this machine's owner) owns or administers the ask's project now by its own grants (a person link or a
  shared GitHub account never lends a right), checked before anything else about the ask is answered; for a key bound by a sign-in also (b) its person is still linked to the account it was bound as, (c)
  the call comes from its node (the authority's own socket for `local`, else a node-bound token of that node; never a
  minted token or a tailnet login) and (d) the daemon whose socket took the call checked the caller. Every failure is
  the same `403`, except (d) from a member, which is `403` with data `{"reason":"member-too-old","machine":...}`. The
  bytes must also name the ask's project and `card_sha256` (`409`: read it again). A key approver's receipt has node
  `key:<fingerprint>`. It cannot confirm anyone, and **cannot decide a pull request's ask**: a merge gate trusts that
  receipt, which stays with a device on the tailnet. `zm approver rm` ends it at once; the key ends itself with
  `DELETE /v0/approvals/key` (a signature over `zm-unbind-key/1\n<receipt key id>\n<fingerprint>\n<nonce>\n<time>`,
  single-use nonce, spent only after the node check passes; a bound key only from its node). The decision's
  transaction re-reads the key, so a key ended after its checks ran records nothing. An ended key's row stays, so old receipts still name it.
  `GET /v0/approvals/key` and `GET /v0/approvers/self` show `account`, `login`, `machine`, `via`, `bound`, `ended`,
  `ended_why` and `replaced_by`. A decision records `surface` (`key`, `device` or `owner`), `account` and `login`
  (a bound key's own; anyone else's verified link at answer time) and, for a key, `machine`, all from the approver
  row and the caller, never from the signed bytes.

  **The caller check (d)** is a guard against careless calls, not a boundary. The daemon whose socket takes a signed
  answer, a key sign-in or a key's removal walks the caller's parent processes to a registered session and refuses a
  caller inside one (fail closed when it cannot tell); only that daemon sees the process, so on a member it runs
  there, and the call goes on marked `origin_checked=1`. A process started outside the session's tree (a detached
  child, a scheduled job, a terminal it opens) escapes it, and whoever holds a member's token (readable by the
  member's own OS user) can call the authority with the mark. What
  holds against a local agent of the person's own OS user: it cannot answer a project its key's person does not
  answer, use a bound key from another node or after the person's grant or link is gone, join two people, or make an
  identity's account up. What does not: it can use the cockpit's key, and it can get its own key bound if the person
  enters a device code it shows them (a code shown anywhere but the cockpit's own sign-in sheet is phishing); its
  bind then ends the person's key, which the cockpit shows. `choose` from a session is not refused yet: each socket
  `choose` logs its origin. Minting an answer-only token (`POST /v0/tokens` with a limit or a device, read with
  the mint handler's own decoder; a body it cannot read is refused) is checked the same way, failing closed; an
  owner-token mint is not.
- **Receipts.** Each decision produces a receipt signed by the daemon's ed25519 key (`$ZM_HOME/receipt.key`)
  over `{project, ask, subject, decision, approver, node, time}`. `zm receipt verify <file|->` and
  `GET /v0/receipts/verify` check it; the public key is published (`GET /v0/receipts/key`, `zm receipt
  key`) so a CI gate can verify offline (`zm receipt verify --key ed25519:... --repo R --pr N --head SHA`,
  no daemon) without trusting mail or comments. The CLI requires repo, PR and head unless
  `--signature-only`, and the decision defaults to `approve` (another needs `--decision`, any needs
  `--any-decision`), so a gate that names only the PR fails on a decline receipt. Replay to another project, ask, repo, PR or head SHA fails verification, as does any
  tampered field or another key. A decision on anything but a pull request has a `zm-receipt/2` receipt: the same
  fields with `subject {work}` or `{question}`, plus `card_sha256`, `text_sha256` (empty without text),
  `answered_as` and, for a card with an action, `action`, under that version's signing line; `zm receipt verify` takes `--work` or `--question` for
  `--repo`, `--pr`, `--head`; a work gate also requires `--ask` or `--card`. Card expectations are
  enforced by Go, HTTP and CLI. A receipt names the device or account that answered (a paired device signs as its own name);
  asked of the daemon, `--approver <person>` also matches a receipt that is an ask's recorded decision, answered
  on a device paired for that person (`zm approver add --for`; a device paired for no one never matches a person)
  or from any of their accounts as linked at verification time, when the caller may read the ask. A local account
  that has linked accounts does not match its own unbound answers (approver_person is empty there).
  Offline verification cannot know whose a device is, so there the name stays exact. `signed=1` (Go:
  `ReceiptExpect.Signed`) also requires a signed answer: a receipt whose node starts `owner:` (the socket, an
  owner token, an answer-only token) is invalid; without it nothing changes. Every ask surface names the authority's `receipt_key`; project-scoped
  key/verification requests go to that authority, including receipt-project discovery through a member
  socket. A key remains a signer identity, never an approval right. Version 1's bytes are unchanged. A version 1 verifier refuses version 2, and a gate
  naming a pull request fails on it. It signs hashes, so redacting a card or answer leaves it verifying.
- **Identity hygiene.** Identity is never inferred from names: a name is used only typed exactly in canonical
  form; case, whitespace, non-NFKC or non-ASCII input is refused, never folded; prefixes and suffixes never
  match; no approver may share a confusables skeleton (NFKC, lookalike letters and digits `0/o 1/l 2/z 5/s 6/b
  8/b 9/g`, `rn`/`nn`/`m`, `vv`/`w`, `cl`/`d`, separators) with another approver or an identity, checked both
  when an approver is added and when an identity is created (`identity add`, or a grant or token naming a new
  one). Everything fails closed: daemon unreachable
  (exit 3), unknown node, re-keyed node, expired or replaced code, removed approver, unverifiable receipt,
  unreadable host addresses (treated as this machine).
- **Device page hygiene.** The page is served only under this daemon's own tailnet name or a Tailscale IP (DNS
  rebinding); its POSTs must be JSON with `Potluck-Protocol` (or its older name `Zm-Protocol`) and not cross-site (no CSRF without a CORS
  preflight, which is never granted); strict CSP; mail bodies are escaped data.
- **Decisions page.** For an active approver the device page is its person's Decisions: every decision
  `GET /v0/decisions` gives that person (note cards, pull requests, work waiting on them), named with that
  identity and its linked GitHub login, if any. The person is whom the machine's owner named when adding the
  device (`zm approver add <name> --for <identity>`, read by that identity's own grants), else the machine's
  owner. Approvers are added only on this socket and are this daemon's alone: a phone paired to a member Mac's
  daemon is not an approver at the authority, and pairs there too. The device's tailnet node (stable ID and
  key) is the sign-in, with no code or token. The person's own open asks come first; an outside party's ask
  (`for` a program's contact) and one whose `owner_name` is someone else fold away below, one section per
  program or person. Each list, and the sections in a fold, run most urgent first: a
  deadline (soonest), then priority, then oldest; a card shows its priority above P2 and its deadline.
  Every answer is the device's own `POST /v0/asks/{id}/decide`. The page installs to a phone's home screen
  (manifest, icon) and is reached at the node's MagicDNS HTTPS name; plain HTTP redirects there once the node
  serves HTTPS. It renders with the Mezze app's Decisions logic, generated from t3code's zm-client
  (`scripts/sync-zm-client.sh`), as text only.
- **Side effects.** Posting a receipt to GitHub (commit status `zm/approval` and a PR comment carrying the
  receipt) goes through a least-privilege bot or GitHub App token held by the daemon for that project
  (`$ZM_HOME/approvals/github/<project ULID>.token`, mode 0600), never the owner's own login; no token, no
  post. It runs after the decision commits and never undoes it, and only for a pull request's ask.
- **Events.** `dev.zm.approver.added` (an approver became active) and `dev.zm.approver.removed`
  (machine-level), `dev.zm.ask.created` (after the ask's `mail.sent`, data the Ask) and
  `dev.zm.decision.recorded` (both in the ask's project and area; audience: operate, plus the asker and
  recipient). An undecided ask's mail cannot be acked or replied to over HTTP (section 9).
- **Redaction.** A scrub overwrites an ask's body and card and a decision's text; the receipt stays valid.
- **Acceptance** (`internal/approvals`, `internal/team/asks_test.go`, `owner_answer_test.go`): the owner answering
  from the socket and from a member Mac (a non-owner member and minted tokens refused), non-PR subjects, the gate, write-back with the
  asker gone and across retries, the inbox, version 1's bytes against a signature from before version 2, an agent on
  the socket refused at every door including its own key; pairing, deciding, refusal of Mac, agent, token and socket callers,
  later approvers needing confirmation, receipt replay across project, repo, PR, head and ask, tampering,
  lookalike names, and fail-closed paths, with a fake WhoIs through the real tailnet handler; the golden leak
  test seeds asks in every place.

## 17. Onboarding and teams (round A built; rounds B1 and B2 built, see 17.3 "As built"; C built, see 17.6 and 17.7 "As built"; 17.9 backups built, Dolt homes retired)

Goal: a stranger goes from nothing to tracking work in under a minute (excluding tool downloads), a team of
five humans and ~40 agents shares one tracker at Linear-grade speed, and the whole suite keeps **one auth door**
(section 1). Build order, each round shippable on its own with its own acceptance tests:

- **Round A, solo onboarding** (no replication): 17.1 zero-install, 17.2 trackers, 17.5 import (retired 2026-11-01).
- **Round B, teams by proxy**: 17.3 authority and 17.4 join, with a *proxy replica* that forwards every
  call to the authority over the tailnet (no local copy; reads cost one tailnet round trip). Section 2's "no
  client-side sync" still holds in round B.
  Ships as two PRs: B1 = `project team`, invites, join, node tokens, proxied work and leases, the claim-race
  test; B2 = cross-machine mail and presence.
- **Round C, local-first replicas**: 17.6 replica mirror, sync and outbox, and the 17.7 load test.

"The tracker mirror" below is section 11's mirror (`work_mirror` in project.db, synced in the background). It
lands with the speed round and is a prerequisite for every round here.

### 17.1 Zero-install start (round A; built)

- One static binary (`go install`, or a release tarball with sha256 sums; Homebrew tap later). Native work
  items (`local:`) are the default tracker; nothing else is required: no bd, no Dolt, no account, no network.
  `make release` builds `dist/zm_<version>_<os>_<arch>.tar.gz` for darwin and linux on arm64 and amd64
  (`CGO_ENABLED=0`, `-trimpath`; `zm`, `bd-zm`, `zm-work-conformance`, the run-target drivers that lend a machine
  (`zm-run-farm` and `zm-run-local` in every tarball; `zm-run-tart`, which `zm host share` binds on every layer, in darwin/arm64's for Tart and in the linux ones for `vm=oci`), README, SPEC, API) and
  `dist/SHA256SUMS` (`shasum -a 256 -c`). Entries are sorted and carry the commit's time, so one commit and Go
  version give byte-identical tarballs. It publishes nothing.
- **Autostart**: a command that needs the daemon starts it when no daemon answers (`zm daemon` in its own
  session, which is what `--detach` does) and prints one line to stderr (`zm: started the daemon (~/.zm)`; the home is shown under
  `~` when it is there). Never for `help`, `--version`, `completion`, `status`, `daemon`, `mcp`, any `hook`, or
  when `ZM_NO_AUTOSTART=1`; a hook with no daemon answers within its 15 ms budget; an MCP server with no
  daemon still answers `initialize` and `tools/list` within 200 ms and every tool call with a "daemon not
  running" error (section 14).
  As built: "any hook" is `zm hook`, `zm session hook` and the commands hooks are documented for
  (`notice`, `session register`, `session state`). Only a socket client autostarts (never with
  `ZM_HOST`), only when the request never connected (so nothing was sent and the retry is safe, writes
  included), and only once per process before it has reached a daemon: a daemon stopped while `zm events`
  runs is not restarted behind the user's back. A start is attempted at most once per process, success or
  failure, so a long-lived caller does not retry a failing 10 s start on every request. Starters serialise on `autostart.lock` and re-check health
  under it, so concurrent first commands start one daemon and only its starter prints the line. The daemon
  does not inherit the starting command's identity (`ZM_AS`, `ZM_PROJECT`, `ZM_TOKEN`, `ZM_HOST`,
  `ZM_SESSION`) or the agent session it ran in (`config.IsAgentEnv`: `CLAUDECODE`, `CLAUDE_CODE_CHILD_SESSION` and
  other per-session `CLAUDE_*`, `CODEX_THREAD_ID`, `CMUX_*`), and a session never gets them either, whichever daemon
  starts it: a Claude Code that inherits the child marker saves no transcript. An autostarted daemon also
  drops the shell's `BEADS_*`, `BD_*` and `DOLT_*` (defence in depth behind 17.2's allowlist); `zm daemon`
  and `--detach`, started on purpose, keep them in the daemon's own environment, and they reach bd only through
  a tracker bound with `env = "inherit"`: every other binding gets the 17.2 allowlist whichever way the daemon
  was started.
  A start that fails is `unavailable` (exit 3) with the daemon log's last line. A write from a socket caller in
  a directory no project claims is refused with a hint that starts with `zm init`.
- `zm init [--slug S] [--pin]` in a directory: in a git repo, slug from the repo name and rules = the repo
  root path plus its remote; outside git, slug from the directory name and a path rule only. A slug already
  taken is refused with a hint to pass `--slug` (never `-2` suffixes). Writes nothing into the repo unless
  `--pin` (`.zm/project.toml`). In a directory that already resolves it names the project and exits 0.
  Ends by printing the three next commands (`work new`, `run`, `top`).
  As built: "the repo name" is the origin's repository name (a worktree's directory is often named after its
  branch), else the repo root's directory name; names are lowercased and other characters become `-`.
  "Already resolves" counts only the directory's own rules (`cwd-file`, `cwd-path`, `cwd-remote`), never
  `--project`, `$ZM_PROJECT`, the calling session or `default_project`; `--pin` there pins the project it
  resolves to. A `.zm/project.toml` naming a project this machine lacks (a teammate's repo) gives the slug,
  so the file resolves. A different `--slug` in a directory a broader project claims makes a nested project
  only when its path rule is longer than the one that matched (else `conflict`); a pinned directory refuses
  it. A remote another project already lists is never copied: a second clone of a repo given its own
  `--slug` gets a path rule only (`remote R stays with project P`), so later clones still resolve to the
  original. `$HOME` and `/` are refused (every directory under them would belong to the project), and a git
  repo rooted at either (a dotfiles repo at `$HOME`) is not used as a root: a directory under it is treated as
  outside git, and `--pin` writes into that directory.
- Measured (`make bench` row `zero-start`, budget 500 ms; and a fresh HOME unpacking the darwin/arm64 tarball):
  fresh HOME to the first `zm work new` printed, autostart included, p50 33 ms / p95 38 ms with a warm binary;
  0.51-0.61 s from an untouched tarball (unpack 0.09 s, then the first exec of a new binary pays macOS's code
  check). Not done: Homebrew tap, signed or notarised macOS binaries, publishing releases.

### 17.2 One-command trackers (round A; built)

- `zm tracker add native|<name> [-P project] [flags]`, `zm tracker ls|test|rm` (all `local` routes; `<name>`
  runs `zm-work-<name>`, section 11). Applied live by the daemon, which rewrites `project.toml`; no restart. A
  failing check rolls the binding back. Every `add` ends with the read-only conformance checks and the mirror's
  first sync, and prints item count, sync time and `zm work ls`. An adapter with the optional `check` verb
  (section 11) also reports the tracker CLI it runs, as it runs it; one that does not answer `version` is a
  skipped check, not a refusal. A mirror whose syncs keep failing shows in `tracker ls`, `tracker where` and
  `work sources` as failing since when and how many syncs in a row (`failing_since`, `failures`), and in the
  notice line (section 9), not only in the daemon log.
- **Setup options** (`--new`, `--dir`, `--server`, `--database`, `--user`, `--allow-migrate`,
  `--password-file`) go to the adapter's `setup` verb (section 11); an adapter takes the ones its
  `describe.setup` declares and `add` refuses the rest. A password is copied to `$ZM_HOME/secrets/` (0600) and
  reaches only the adapter's environment (its secret slot): never the adapter JSON, a route, a log, `tracker ls`
  or `work sources`.
- **Tool provisioning**: zm fetches the pinned tools it runs (rclone for off-site backups, 17.9; cloudflared, 17.11)
  into `$ZM_HOME/tools/` (pins per OS and arch, with sha256, in the source; a mismatch refuses the download;
  never sudo, never a package manager; `--no-download` refuses instead). The pin bump procedure is in
  DESIGN-NOTES.md.
- **Environment allowlist** per adapter (the user's shell never leaks in; `env = "inherit"` opts out per
  tracker and shows in `tracker ls`). The adapter declares what it needs (`describe.env`, section 11): the
  daemon gives `PATH`, `HOME`, `LANG`, `TERM`, `TZ`, the declared variables and the declared secrets, nothing
  else, and an adapter that declares nothing gets the base.
- `add` reserves the project's prefix before any side effect, so a second `add` of it while the first runs is a
  conflict at once and never touches the first one's directory. describe runs with the base environment (the
  daemon's, for `env = "inherit"`).

### 17.3 Teams: one authority per project (round B)

- A project is **solo** (this machine is its authority, as today) or **team**: one daemon is the project's
  **authority** and every other member machine runs a **replica** of it. One authority per team project; no
  multi-authority, no federation.
- **The authority alone** holds tracker credentials, runs the project's tracker adapters, keeps the tracker
  mirror, and decides leases. Replicas never run tracker adapters for team projects and never see tracker
  credentials: a teammate holds a zm token, never a database password.
- **Claims (overrides section 10 for team projects)**: the zm lease at the authority *is* the claim, taken
  by compare-and-set in one transaction; the tracker's assignee is derived from it by background write-back.
  If the tracker later disagrees (a bead assigned in bd directly), the authority releases the lease with note
  `tracker conflict`, publishes it and mails the holder; the tracker's holder shows as a non-zm holder
  (as in v0) until someone claims through zm. Claims stay a compare-and-set at the authority; from round C a
  claim made while the authority is unreachable is queued on the replica and decided there when it drains
  (17.6): it is never a lease until the authority says so, and one that loses comes back as a conflict.
- **Becoming a team project**: `zm project team <slug>` (local, on the machine that becomes the authority)
  moves the project's leases from registry.db into its project.db (two files cannot share a transaction: under the
  project's lease lock it copies the rows into the target, flips the mode in registry.db as the commit point, then
  drops the source rows; rows a crash leaves on the wrong side are dropped at the next start) and refuses while a
  managed session's bound lease is live (a hook session's moves with the rest); `zm project solo <slug>` moves them back and refuses while any replica is joined. There is
  no other transition.
- **One file per team project**: a team project's items mirror, comments, mail, leases, event log and write
  records live in its project.db, so a change, its event and its write record commit in one transaction. A
  crash can lose a whole write, never half of one. The lease schema is identical in both stores and
  `work/leases.go` is one code path given a database handle; the lease ticker walks registry.db and every
  team project.db each tick. A team file gets one writer connection and four readers (snapshots and sync run on
  readers), WAL, and a `busy_timeout` below the claim budget.
- **Event ids in commit order** (overrides section 12's "appended after commit" for team projects): a team
  project's writes hold that file's write lock from the moment the event id is minted until commit, so id
  order, commit order and publish order are the same and a replica resuming at any id never misses an earlier
  id that committed later. Writes per team file are serialised; at ~40 agents that is well inside budget.
- **Sessions on a replica are local**: spawn, attach, input and stop stay `local` routes on the replica, owned
  by its OS user (so a guest such as dot can `zm run` in `harbor/solver` on their own machine). Only
  presence (address, state, work held) goes to the authority, with `write`. `release --force` from a replica
  is `forbidden` (section 10 requires the authority's socket).
- **Round B proxy**: the replica caches only the project header (id, slug, areas, the member's role, cap and
  `scope_version`, and the member's path rules), refreshed from a response header on every proxied call;
  everything else is forwarded. A replica token may set `Zm-As` to any session address whose machine is the
  replica's machine name recorded at redemption.
- **People** (`GET /v0/people`, a cockpit's Team tab): for a project the caller reads whole, every identity holding a
  grant in it; for an area guest, only those holding a grant on an area it reads (it learns nothing outside its
  area, whole-project members' names included); the caller's own person included, flagged `you`; never a machine
  identity; each with the projects shared (one project row per identity there), its identity ULID there, and the machines and
  latest change of its live sessions the caller sees. Roles and areas are not shown, save `answers` on a project
  row: the person owns that project, shown only to a caller who reads it whole. Identities linked to one account
  fold into one person, every name in `names`. Each person has a stable person id: the GitHub account (`github:<numeric id>`)
  its identity is linked to at that authority (the device flow's verified link, section 22 "Sign in"), else the
  identity's ULID there; with it come the linked login, the identity's exact aliases and, per project, the
  identity's name there (its `human:<name>`). A member's socket merges every authority's answer by person id, never
  by name: identities at two authorities are one person only when both link the same account (an authority
  without ids is merged by name, as before).
- **Machine names are unique per authority**: redemption records node ID -> machine name and refuses a second
  node claiming a name already held; every presence write and mail sent from a replica is checked against that
  mapping (Tailscale's own renaming to `zm-mini-1` does not make two `mini/...` addresses legal).
- **Scope on a replica**: for a team project every local caller on the replica, the OS user included, acts with
  at most the replica's token scope (as built in B1, the socket callers *without* a token: a `zm token mint` token
  presented on a replica's socket acts in that machine's projects only, so agents on a replica use the socket
  without a token); the authority enforces it and the replica also checks it for fast local
  errors. A guest replica is sent nothing outside its area, ever: not in reads, sync, snapshots, events, errors,
  counts, presence or write answers.
- **Mail and presence across machines** (overrides section 13's "forwards nothing" and section 15's out-of-v0
  list, for team projects only): mail to a team project's session, item holder, group or human is routed by the
  authority; in round B a replica fetches it through the proxy, in round C through sync. Presence heartbeats
  every 15 s; presence older than 60 s counts as `idle`, so its bound leases stop renewing and expire after
  `bound_lease_ttl`. When a replica learns its session's lease is gone it logs the loss and marks it gone locally,
  leaves the session's state and note unchanged, and never attributes later writes to it as holder. Solo
  projects never leave the machine.

- **As built (round B1)** (routes in API.md "Teams"; why in DESIGN-NOTES.md "Teams by proxy"):
  - `zm project team|solo <slug>` (`POST /v0/projects/{p}/team|solo`, local). The move holds the project's
    lease lock (every lease operation takes it shared), copies the rows into the target file, then flips
    `projects.team` in registry.db (the commit point), then deletes the source rows; a crash before the flip
    leaves a copy that the next start drops, and one after it leaves source rows that are dropped the same way
    (a solo project's rows live only in registry.db, a team project's only in its file). `solo` refuses while a
    live node-bound token exists for the project. `work/leases.go` takes a handle (`leaseDB`/`leaseReadDB`); the
    ticker and every cross-project lease list walk registry.db and every team file.
  - The team pool: one writer connection (`Store.Writer`, `busy_timeout` 200 ms) and four readers
    (`Store.Project`, `busy_timeout` 5 s), WAL. Claims, renewals, releases, expiry and moves run on the writer; a claim, a
    local item's `open -> claimed` and the claimed (and replaced lease's) events commit in one transaction
    (`events.Bus.TeamTx`), as do a release or expiry with its event. Other modules' writes (mail, comments,
    status, local items) still commit in their own transaction on a reader connection, then publish; not done:
    moving them onto the writer and write records (round C). Readers open transactions with BEGIN IMMEDIATE too, so
    such a write takes the file's write lock: "one writer" is a convention until round C. A claim waits at most its
    200 ms behind one (they are short), else fails `database is locked`; a reader-side write waits up to 5 s behind
    the writer.
  - Event ids: every event of a team file is minted under that file's write lock (BEGIN IMMEDIATE on its
    writer, then the bus lock) and committed before the next is minted there, so id order is commit order
    (`TestTeamEventIDsInCommitOrder`). Lock order is always the file, then the bus, so a solo publish cannot
    deadlock with a team one. A team log keeps 200 000 rows and 30 days, whichever is longer.
  - Tracker items: the claim is the lease at once; the tracker's `claim` (or `release`) runs from a background
    write-back queue. An adapter `conflict` releases the lease with note `tracker conflict`, publishes
    `dev.zm.work.released` and mails the holder; so does a later sync whose mirrored owner is another non-empty
    owner (checked each lease tick, once the claim is older than 30 s, so a write-back still in flight never
    trips it).
  - The proxy replica: work, lease and (section "Native issues") native issue calls (and `GET /v0/projects`, so the CLI shows team projects by slug)
    from this machine's socket without a token go to a joined project's authority by the rules in API.md
    "Teams"; lists naming no project merge this machine's answer with every authority's (an unreachable one is
    named in `skipped`, never silently missing; `GET /v0/projects` too). A minted token on the replica's socket is
    not proxied (not done: intersecting a minted cap with the replica's scope). A body over 1 MiB is refused
    (`400`), never forwarded cut short. Proxied calls are logged (Debug) like any other. The authority answers every node-bound token's call with the
    project header (`Zm-Team`) and `Zm-Scope-Version`; the replica caches the header in `team_remotes`
    (registry.db). The header carries the project's path and remote rules (`repos`, whole-project members) and those
    of each area the member sees (`area_repos`), so the replica resolves a cwd against them locally, with no call,
    fresh with every answer. Its own role/area checks before forwarding are only `release --force` (403); the authority
    decides everything else.
  - A session address on a joined replica machine with no presence row (yet) is visible to whole-project readers of
    a project that machine joined (`core.Deps.Visible`), never to area-only readers; with presence (B2) its own
    project and area decide, like a local session's. `spawn --work` of a team item on a replica: built since (section 8).
  - Not done in B1: cross-machine mail and presence (built in B2, below), the event stream for team projects on a
    replica (built since: `zm events -P <joined>` streams from the authority for this machine's token, and with no
    project a replay also covers each joined project), 17.2's team-authority rules (`--server`
    only on an authority, the zm identity as git author there), local copies, sync, snapshots, the outbox and 7-day
    stale reads (round C).

- **As built (round B2)** (routes in API.md "Teams"; why in DESIGN-NOTES.md "Mail and presence across machines"):
  - **Presence**: every 15 s, and within about a second of a local session event (batched, at most one a second),
    a replica posts `POST /v0/team/presence` per joined project: every live session of the project on it (address,
    area, agent, title, state, note, managed, activity, activity_at, term_title (only an agent runtime's, section 8),
    the keys it saw the session claim, and its usage fields: account, for, job_kind, tokens). Its internal source supplies the complete local live inventory independently
    of public session pagination (still 500 per page), up to 10000 live sessions, plus the newest 500 ended local
    outcomes from the last 24 hours. A legacy source whose result could be a full 500-row page is refused rather
    than sent as an incomplete replacement. Presence uses a dedicated 8 MiB body limit: at most 10000 live/address-only
    session entries plus 500 ended-session prompt-ack entries, 10000 Elsewhere addresses and 500 ended outcomes
    (21000 rows total), 10000 delivered prompt ids and 100000 reported work keys. The full request is checked before
    presence or prompt acknowledgments change. Pending prompt acknowledgments beyond these limits stay on the
    replica for later complete heartbeats; live sessions and Elsewhere addresses are never silently truncated.
    Heartbeats remain one replacement transaction, never independent chunks. The authority tolerates unknown
    fields; older authorities with the 500-session limit must be upgraded before larger complete heartbeats.
    After a heartbeat goes through, at most every 30 s, the
    replica also posts its **machine numbers** (section 8) on their own route, `POST /v0/team/machine`; an older
    authority without it answers 404 or 405, and the replica stays quiet for an hour.
    An activity or title change is a session event, so it rides the same within-a-second heartbeat. A term_title
    that may not leave its machine (section 8) is neither sent in presence nor shown by the authority to a replica:
    the authority's own sessions reach members through proxied reads, which leave it out for token callers. (A replica has
    no live stream of the team project yet: there, other machines' activity shows in the merged `GET /v0/sessions`.) The authority accepts it only from
    a node-bound token, refuses any address not on the machine name recorded at redemption (`403`: Tailscale's
    `zm-mini-1` changes nothing), puts a guest's session with no area in its area, answers a session in an area the
    token may not write in like a missing area (not stored), stores the rest in the session module's `presence` table
    (registry.db) and marks the machine's other sessions in the project ended (`gone from presence`; their bound
    leases released at once), with the token's identity (`who`) and cap (`reach`); a change of activity or
    term_title alone publishes `dev.zm.session.updated` there (coalesced, section 8); a session listed in another
    project or area than its row's publishes `dev.zm.session.left` to the old one first (section 8). It answers, per session, the keys it holds, its unread mail as notice parts and how
    many needs=human mails it sent are open, plus the replica identity's own notice parts. Presence rows answer like
    local sessions to `Lookup`, `Visible` (by their project and area), `Live` and `List` (`GET /v0/sessions`,
    `GET /v0/sessions/{address}`, with `seen`), so mail routing, redaction, `zm who` and `zm ls` at the
    authority see them. A heartbeat older than 60 s shows the session `idle` (note `presence stale`): a bound lease
    it holds stops renewing and expires after `bound_lease_ttl`. A claim by a replica
    session whose presence says `managed` is a bound lease unless the claim names `ttl_seconds`; before its first
    heartbeat, a TTL lease. Presence rows end (`session.ended`, bound leases released) when their machine no longer holds a live (unrevoked, unexpired)
    node-bound token for the
    project (`zm leave`, `zm token revoke`, `zm project solo`, and `zm revoke` of the identity's last grant in the
    project, after which the token is unusable; note `replica left`, at once and on the 15 s session tick; a session
    that holds leases in another project of the authority its machine still has a token for is not ended: its row
    moves there (`session.left`, `session.registered`; its work, identity and reach blank until its next heartbeat
    there, so nothing of the left project goes with it) and only the left project's bound leases are released) and
    when their heartbeat is older than `bound_lease_ttl` + 2 min, never under 10 min (note `presence
    lost`; a bound lease has expired by then), so the authority's `zm ls` does not keep ghosts of
    every box that ever joined; a replica that comes back heartbeats them live again.
  - **Sessions on a replica** in a joined project (an explicit `--project`, a `.zm/project.toml` naming the slug, a
    recorded checkout) are local rows carrying the authority's project ULID; spawn, attach, input and stop stay local.
    `zm project why` on a replica shows that rule as the step `team` when no local rule named a project.
    A guest's session gets its area, another area is refused locally (`404`; hook registration falls back to `none`).
    Their events go to this machine's log (project `none`, owner only), since the project has no log here.
    `spawn --work` of a team item takes its lease at the authority (section 8).
  - **Mail**: in a team project the authority stores mail to a replica's session (known by presence, any state; an
    unknown one is `404` like a missing one), to an item held by one, and to `machine:<replica>` (a machine joined to
    the project; only whole-project readers and that replica may name another machine's group mailbox, others get
    the `404` of a machine that never joined) like local mail; the group mailbox, first-ack-wins, needs=human
    visibility to a machine's human (a replica's own identity is its machine's human) and the 30-minute orphan copy
    work per replica machine. On the replica, mail lists (`inbox`, `sent`, `GET /v0/mail`) and `GET /v0/sessions`
    merge every authority's answer (an unreachable one named in `skipped`); a mail id, ack, reply and thread go here
    first, then to each authority while `404`; `POST /v0/mail` follows the write rules (and the calling session's own
    team project when no local rule matched by path or remote).
  - **Hooks stay local**: the notice (`GET /v0/mail/notice`, and the hook route's) is never proxied; it adds the
    caller's team mail as of the last heartbeat answer (an in-memory cache, refreshed every heartbeat and right after
    any proxied mail call), so a hook never waits on the tailnet, whether the authority is up or gone. Merged lists
    (`zm ls`, `inbox`, `work ls`, `project ls`) ask every authority at once, with connecting
    bounded at 2 s, so an unreachable authority costs a list about 2 s (unmeasured on a real tailnet). A forwarded
    read silent for 1 s goes again on a new connection, at most twice; the first whole answer wins (API.md "Teams"),
    so one stuck connection never holds a read for its TCP backoff. Only routes that are pure reads at the authority
    are hedged, by each route's row (potluck/kit/teamlink routes.go; a daemon with a forwarded row that has no decision does not start):
    never a write, and never `GET /v0/mail/inbox`, which marks what it answers seen (its `/peek` marks nothing: hedged). Hedges are at most 10% of the
    hedged routes' reads over the last minute (at least 3 a minute), so an authority slow for everyone gets about 10%
    more reads, not three times as many; `GET /v0/health` on the socket counts them (`hedges`). Team mail can
    therefore show in a notice up to one heartbeat (15 s) late. The replica types the nudge into an idle managed
    session when its direct team mail grew (one idle managed session of the project for group mail).
  - **needs=human and lost leases on the replica**: a session that sends needs=human mail through the proxy becomes
    `needs_human` (`waiting on mail <id>`) locally; when a heartbeat says none of its needs=human mail is open it goes
    back to `idle` (`answered`). A key the replica saw its session claim through the proxy that a heartbeat no longer
    lists as held, and that it did not release through the proxy, is a lost lease: it is logged and the key is no
    longer reported as held. Lease loss, including expiry and force release, leaves the session's state and note
    unchanged; genuine permission requests and open needs=human mail remain requests. Expiry still sends the holder
    its quiet `notice` at the authority; missing or delayed notice delivery does not create a human request on the
    replica. Reclaiming alone does not clear an existing state or note. A lease that ended because
    the GitHub link closed the issue (a merged PR names it) comes back in the answer's `finished`, which is not a loss;
    the holder gets a `notice` saying so. The authority keeps that record in memory for a day and forgets it on a new
    claim; after its restart such a lease reads as lost. The held set is in memory:
    a lease lost while the replica daemon was down is not reported as lost after it restarts.
  - Measured (loopback, `TestReplicaMailLatency`, 200 runs, in-process clients on the replica's Unix socket): the
    prompt hook with its notice p50 0.08 ms / p95 0.18-0.27 ms, the same with the authority stopped p50 0.07-0.09 /
    p95 0.19-0.26 ms; a proxied inbox 0.34-0.36 / 0.58-0.72 ms; a proxied send 0.32-0.35 / 0.45-0.94 ms; one heartbeat
    with 9 sessions 1.1-1.2 / 1.7-2.7 ms. Not measured: a real tailnet.
  - Not done in B2: the live event stream for team projects on a replica, a minted token on a
    replica's socket reaching a team project, and everything of round C.
  - **Sessions outside the project** (section 9 "Across projects"): a heartbeat also lists, as `elsewhere`, the
    addresses of the replica's live sessions registered outside that project (another project or `none`): addresses
    only, never their project, area, title or state; always sent (`[]` for none; absent = an older replica, whose set
    is left alone), at most 500 (the authority drops the rest rather than refuse the heartbeat). A needs=human mail
    one sent through the proxy is answered on the replica when no project it is outside of still counts it open. The authority keeps them in memory (a restart forgets them until
    the next heartbeat, at most 15 s) for 7 days after a heartbeat last listed one, refuses any not on the replica's
    machine name (`403`), and answers each with its unread mail in the project as notice parts, so the replica's notice,
    nudge and orphan sweep treat them like the project's own sessions: mail to one still listed is never orphaned. Such
    an address is shown (not `someone`) to the readers who may address it (section 9): readers of the whole project,
    and, for a replica that reads only some areas (a guest's), readers of those areas, where its mail is filed. The
    session ticker drops what is past the 7 days. An older authority ignores the field (mail to
    those sessions stays `not_found` there); an older replica sends none. Measured (loopback, `TestElsewhereHeartbeatCost`,
    200 runs): a heartbeat with 9 sessions p50 1.12 / p95 1.66 ms, with 9 more listed as elsewhere 1.49 / 1.91 ms.

### 17.4 Join (round B)

- **Prerequisite, stated plainly**: both machines are on one tailnet (or the authority's node is shared into the
  member's), and the replica daemon runs its own tsnet node. Or the home is behind a Cloudflare Tunnel and the code
  is an https one (17.11): then no tailnet at all. `zm join` runs `zm tailnet up` first
  (`--auth-key`, or it prints the login URL). "Under a minute" starts once the member is logged into Tailscale.
  `zm-<machine>` must be unique per tailnet: `join` refuses a taken name and suggests `--machine`. The
  replica always dials the authority through its own tsnet node (`Node.HTTPClient()`), never the host's
  Tailscale.
- On the authority: `zm invite <project>[/<area>] --as NAME --role member|guest [--ttl 24h] [--class trusted|public]`
  (the redeeming machine's operator class, section 8) prints a
  one-time **join code** `zm-<machine>:<authority node fingerprint>:XXXX-XXXX` (40+ bits of secret, expires
  with `--ttl`, at most 10 live per project). The invite stores the intended identity, role and scope; **no
  identity, grant or token exists until redemption**, so expired or removed invites leave nothing behind.
  `zm invite ls|rm` (local). The code names the node by its **full MagicDNS name**
  (`zm-dev-05.tail1234.ts.net:<fingerprint>:XXXX-XXXX`): a node shared in from another tailnet resolves only
  by it. Older codes with the short `zm-<machine>` still parse, and a replica whose short name does
  not resolve dials the peer's full name instead (as do remotes recorded with a short name, and the GitHub-join
  discovery of SPEC 22).
- On the member's machine: `zm join <code>`. The replica pins the authority's node fingerprint from the code
  and checks it on every call. It calls `POST /v0/invites/redeem`, the only unauthenticated route besides health
  and the receipt key: rate-limited per node and per invite, refused from the authority's own node and from
  tagged nodes unless the invite was made with `--allow-tagged` (CI replicas), and the same answer for wrong,
  used and expired codes. Redemption creates the identity (if new), the grant and the
  token in one transaction, and answers `{token, cap: {project, area, role}, scope_version, machine}`; the replica
  writes the token file atomically before it reports success and does its local scope checks from `cap`, never
  by presenting the secret locally. Redemption is **idempotent per (code, node)**: until that node's first
  authenticated call with the token, presenting the same code again returns the *same* token (so a lost response
  never burns the code and a retry never revokes a token in use); after it, and from any other node, the
  uniform refusal.
- **Node-bound tokens** are a separate token kind (a `node` column; `token mint` tokens are unchanged, so the
  section 6 socket-token tests keep working). The token is bound to the redeeming node's stable node ID and
  has no expiry (revocation is its lifecycle). A node-bound token is `401`
  from any other node *and* on any Unix socket, so the only working presenter is the replica daemon. That is
  what makes "agents never get it" true: the file (`$ZM_HOME/remotes/<project>.token`, 0600) is readable by
  the OS user's agents, but it fails from the host's own Tailscale (a different node) and from the socket. Join
  tokens bind the node ID only, so routine node-key rotation does not break a team member; `decide` (section 16)
  still binds ID *and* key because it is the highest right. A reinstall that changes the node ID needs a new
  invite; the old token is revoked with `zm revoke`.
- `join` then takes the snapshot (round C) or checks access (round B), records the member's checkout paths
  (asked for, or found by the remote), and prints what the member can see. `zm leave <project>` deletes the
  token and any local copy. Before a copy is deleted (leave, purge, revocation) its still-pending outbox writes are appended
  to `replicas/<ULID>.db.unsent.jsonl`, shown by `zm remotes`; they are never re-sent automatically.
- **A project id keeps its authority key** (Potluck #143, #163): the first join of an id that works binds it on this
  machine to that authority's key (the pinned fingerprint and transport, never an address or name); leave, revoke and
  purge keep the binding, and a join presenting the id at another key is refused (`403`) before any token is written.
  Only this machine's owner removes it, naming the key shown (`GET /v0/team/project-keys/{project}`, then `POST
  /v0/team/project-keys/{project}/forget` with `host_state` keep or drop), and only once no join of the id is recorded.
  `zm forget <project> (--keep-host-state | --drop-host-state) [--yes]` is Potluck's `forget` mounted as `isolation`
  is: it shows the bound key, asks unless `--yes`, and sends the shown fingerprint.
- **Revocation and narrowing**: every grant change on an identity (revoke, role change, area change) bumps that
  identity's `scope_version`; the authority checks it on every replica call, and a mismatch answers `409
  rescope`, after which the replica purges that project's copy and re-snapshots (or stops, if revoked). A
  replica that gets `401` three times over at least 10 minutes purges its copy (one 401 can be a restarting
  authority); a replica that cannot reach its authority for 7 days keeps serving reads marked stale and refuses
  writes. Data already copied to a machine cannot be recalled; the docs say so.

- **As built (round B1)**:
  - The code is `zm-<authority machine>:<fingerprint>:XXXX-XXXX`: 16 base32 characters of sha256 over the
    authority node's stable node ID (not its key, which rotates), and 8 Crockford base32 characters (40 bits;
    typed `I`/`L` read as 1, `O` as 0). `zm invite` needs this machine's tailnet node running (the code names it)
    and a team project; roles member or guest; default TTL 24 h, at most 7 days. Only the code's sha256 is stored.
  - Redemption's own refusals come before the code is read: the socket `403`, a caller WhoIs did not identify
    `401`, this machine (`core.Remote.Self`) `403`. After that one `403` answers a wrong, used, expired or
    removed code, a tagged node on an invite without `--allow-tagged`, a node over its limit (5 wrong codes per
    node and 50 overall per 10 minutes) and an invite past 10 attempts. A machine name held by another node, or
    a node that already joined under another name, is `409` and does not spend the code. An existing stronger
    grant is kept (an invite never demotes). Until its first authenticated call the token's secret stays in the
    invite row so the same (code, node) gets the same token (in plaintext: a deliberate trade-off, DESIGN-NOTES);
    that call clears it. Such retries are not counted against the invite's 10 attempts. Each redemption publishes
    `dev.zm.team.joined` (machine-level: project, area, role, identity, machine, node).
  - The machine name is the replica daemon's own (`zm join --machine M` with M not this daemon's name is refused
    with how to change it): session addresses are `<machine>/<session8>`, and the authority accepts `Zm-As` from
    a node-bound token only for its identity, `machine:<m>` and sessions on the name it recorded. The authority
    refuses its own machine name at redemption (`409`, the code not spent), and a token that records it anyway
    answers as none of its sessions, its group mailbox or its human. A node holds its name (`team_nodes`) only while
    it holds a live node-bound token in any project: after `zm leave` or `zm token revoke` the name is free, so a
    reimaged box (new node ID) rejoins under the same name with a new invite (17.4's "a reinstall needs a new
    invite", not a new name).
  - Fingerprint pinning: the replica's HTTP client dials through its own node (`core.NetService`, the tailnet
    module) and asks its node who answered (WhoIs on the peer) for every new connection; a different node is
    refused before any request is sent.
  - `zm join` refuses a code for this machine, a name the tailnet renamed (`zm-mini-1`: taken), a project this
    machine already joined (the new token revokes itself), and a slug a local project has (rename it and join
    again with the same code; checked before the token file is written, and a join that fails after writing it
    removes the file). `zm invite` likewise refuses when the tailnet renamed the authority's node (the code names
    the node by the name the tailnet gave it, which must be `zm-<machine>`). It records `--path` checkouts, else the cwd's git root when its origin is one of
    the remotes the member may see; writes from inside a recorded checkout (or a repo whose `.zm/project.toml`
    names the joined slug) go to the team project. So do writes whose cwd the joined project's own rules from its
    header match, exactly like a local project's (section 5 rules 4 and 5: the longest path wins, local or joined (a tie
    goes local), an area's over its project's at equal length; remotes only when no path matched). `zm project why` names the
    rule in the `team` step.
  - Rescope: on `409 rescope` the replica takes the new header from the answer and sends the call once more
    (the authority refused it before any route ran). A header with no role (every grant of the identity in the
    project revoked, the token left alone) gates the replica: state `revoked`, `last_error` naming the authority,
    calls answer "access ended", and lists name it in `skipped`. Its copy and old outbox are deleted. The retained
    node-bound token permits only bounded authenticated project-header reads until a later accepted header
    genuinely grants a role again. Then, after old-copy teardown, it recovers automatically with a fresh snapshot;
    old queued writes never replay. Stale headers and cached roles cannot recover access. The token file stays
    so `zm leave` can revoke it; a new `zm join` retires the old copy and revokes its token first. Three `401`s, the first and third at least 10 minutes apart,
    purge the remote: round B holds no copy, so its token file and cached header go and its state is `purged`.
    `zm leave` asks the authority to revoke the token (`POST /v0/team/leave`), then deletes the file and record.
  - A guest's checkout is recorded only when its area lists the repo's remote (a guest never learns the project's
    own remotes), so a guest joining from a checkout passes `--path`, or pins it later with `zm project use <slug>`
    (both documented; the join says so when nothing was recorded).
  - Tests run two to five daemons on this machine over a loopback tailnet (`potluck/kit/tailnet/tailnettest`: the
    same `tailnet.Node` interface, WhoIs answering faked node IDs, keys, logins and tags). `go run ./scripts/loopnet`
    runs the same thing for a person: several daemons, each in its own `ZM_HOME` with its own socket, on one fake
    tailnet, so the real CLI can rehearse an authority, members and guests on one machine (README "Rehearsing a
    team on one machine"); it is a development tool, never part of `zm`. Measured over loopback
    (`TestProxyLatency`, 200 runs, in-process clients on the replica's Unix socket; a CLI call adds its ~8 ms
    process start, as in `make bench`): a direct read at the authority (the round trip) p50 0.31-0.33 ms / p95
    0.57-0.64 ms; a proxied `work ls` 0.37-0.43 / 0.67-0.89 ms (budget: round trip + 30 ms); a proxied claim
    0.35-0.37 / 0.65-0.68 ms (budget 50 ms). `TestClaimRace`: 4 replicas x 4 workers racing on 12 items, exactly
    one winner each, 180 conflicts, the authority holding exactly the winners' leases. Not measured: a real
    tailnet (no real nodes or auth keys were used).

### 17.5 Tolerant bulk import (round A; retired 2026-11-01)

- **Retired 2026-11-01.** `zm tracker import beads` (a beads JSONL export or a live tracker into a zm-owned beads
  database through `bd import`) and `zm work import` (a beads JSONL export into `local:` items) are gone, with
  their routes (`POST /v0/trackers/import`, `POST /v0/work/import`), the background jobs that ran them (`zm jobs`,
  `/v0/jobs`) and `scripts/import-bench.sh`. Nobody used them after the harbor project moved to native issues. bd users keep
  their commands on native issues through `bd-zm`, installed as `bd` (`zm issues import`, a beads Dolt database into
  native issues, was retired on 2026-11-10 with the Dolt homes, 17.9).
- What stays, because it is contract: project.db's `work_imports` table (migrations are append-only; nothing
  writes it now) and the `dev.zm.work.imported` event (deprecated: nothing publishes it; replicas still apply it
  when they replay an older log). What the importers taught is summed up in DESIGN-NOTES "Tolerant bulk
  import"; the full design, measurements and code are in git history before the retirement.

### 17.6 Local-first replicas (round C)

- **Cursor** (keeps section 6's "no counters on the wire" and section 12's "no sequence number"): the sync
  cursor is the event id, a ULID from the authority's monotonic generator; across machines the cursor is
  `(source, id)` (DESIGN-NOTES #4). Gaps reveal nothing. Lag is shown as an age, never a count.
- `GET /v0/sync?project=P&since=<id>` returns the caller's visible changes after `<id>`; SSE carries the same ids,
  so a replica resumes with `Last-Event-ID`. The authority keeps a team project's changes for at least 30 days
  and 200 000 rows, whichever is longer; a `since` older than that answers `410 resync`.
  A cursor lost when the authority restores an older backup also answers `410 resync`, even after newer events
  have passed it; the replica takes a fresh snapshot and drops rows absent from the restored authority.
  `GET /v0/sync/snapshot?project=P` streams NDJSON from one read transaction on the project's file and ends with
  the last change id read in that same transaction.
- **Area transitions** (section 3: labels and rules move items between areas): every change row records
  `area_before` and `area_after`, computed when it is written. A row with an area change goes as a full upsert of
  the item (with its comments and lease) to readers of `area_after`, and as a tombstone `{id, gone: true}` to
  readers of `area_before` only; never both to one reader. An item's area is stored on its row (`work_mirror.area` for tracker
  items, the item row for local ones) and recomputed on every mirror sync and every area-rule edit; change rows
  come from that diff, so items the authority has never mirrored need none. Snapshots apply the same rule.
  Mail keeps the area it was stored with at send time (section 9) and never transitions.
- **Replica reads** are local: the replica answers every read for the project from its copy (budgets in 17.7);
  `status` and `work sources` show the authority, the cursor's age and the last error, sanitised (no adapter
  paths, no hidden ids). An unreachable authority leaves reads served, marked stale with the age.
  Sessions have no copy: the machine owner's `GET /v0/sessions` with no project and no query (`zm ls`) asks each
  authority, but if one does not answer within 50 ms it is answered from that copy's roster cache, the list the
  authority gave the replica's own identity (refreshed every `PresenceEvery`, dropped with the copy and on any
  revoke, leave, purge or role change, marked `behind` once older than a heartbeat, not served after three).
  A session, an area-scoped caller or a query forwards as before.
- **Writes**:
  - Claims and leases: synchronous compare-and-set at the authority (one round trip). Offline (round C as
    built, amending "refused"): queued like any write and answered `202 queued`, never as a lease; the authority
    decides when the outbox drains, and a queued claim that loses is a conflict the replica shows.
  - **No-queue lease writes**: a claim, lease or release with `Zm-No-Queue: 1` (`--no-queue`) never
    enters the outbox. Authority unreachable: `503 unavailable`, data outcome `not_sent`. Else it goes live with a
    fresh write id and waits `ClaimWait`; an answer is relayed as for any write. No answer: the replica sends one
    `POST /v0/team/writes/{id}/cancel`. The authority takes the same per-write lock as a run (so it waits behind an
    in-flight one). The id already in `team_writes`: `{state: "ran", status}` (never the body) and the replica
    resends the same id and relays the recorded answer. Otherwise it records a tombstone (status 409, req
    `x1:cancelled`) and answers `{state: "cancelled"}`; the caller gets `503` outcome `cancelled` (nothing was
    claimed or released). Any later delivery of that id, live or queued, is a definitive `409 conflict` "this write
    was cancelled; it never ran" (never "retained for review", never a run). No cancel answer, or an old authority
    (404): `503` outcome `unknown` with the write id; `zm project team cancel <write>` settles it later. Tombstones are
    pruned by time only (`CancelKeep`, 7 days) and are exempt from the `MaxLiveRecords` cap. Limit: export strips
    `team_writes`, so a tombstone does not survive an authority restore; no-queue writes live only on live
    connections, so nothing is left to replay after a restore. A direct (non-replica) daemon ignores the header.
    A no-queue write the copy cannot serve (the copy not ready, the item not in it) is never forwarded: `503`
    outcome `not_sent` for a named or cwd project, a `404` on the item path; either way nothing was sent, so a
    no-queue release answered `404` does not prove the lease is gone (check `zm leases`). A
    no-queue release of a lease taken through this replica is served even before the copy lists it. The
    authority refuses to tombstone an id older than its live records (`LiveWriteKeep` less `CancelSkew`, 1 hour,
    or not newer than the oldest kept record plus `CancelSkew` when the count cap is full): `409` "too old to settle", since a
    missing record no longer proves it never ran. A caller with an `unknown` outcome settles it (`zm project team cancel`)
    before it writes that key again, or sends its release with the lease's `fence` (section 10, "Fencing"): an
    unfenced release carries no lease generation, so an unsettled one could still drop a newer lease. A caller's own timeout must outlast `ClaimWait` plus two `CancelWait`s; a timeout or a lost
    socket on the caller's side is `unknown` with no write id. A daemon too old for the header queues the write
    and answers `202 queued`, which the client reports as an error (code `queued`): treat it as `unknown`.
  - Comments and mail carry a ULID minted by the replica; the authority stores them by id, so a replay never
    duplicates, however late.
  - Field writes (status, title) carry `base`: any change id for that item the replica had seen (for an item
    created offline, its creation id). The authority keeps each field's last change id and refuses only when
    that is newer than `base` (`conflict`, recorded and returned); a comment or another field changing after
    `base` does not refuse it. The replica reverts its pending mark and shows the conflict. No silent
    last-writer-wins. Links are a set (add and remove by id, like comments) and need no `base`.
  - Optimistic: applied to the copy at once, marked pending, sent; offline they wait in an outbox (bounded,
    shown in `status`) and replay in order. Write records are scoped to (identity, project) and kept until the
    replica's cursor passes the write or 30 days, whichever is later.
- **Resync** (`410`, `409 rescope`): apply the snapshot and drop rows not in it, re-overlay pending writes, then
  replay the outbox with their `base`. A replay whose target is gone or hidden gets `not_found` and is dropped
  with a local note, never retried forever.
- **A restored home**: `zm backup restore` writes a new random restore generation into the home
  (`$ZM_HOME/restored`, before any file lands; never in a set, kept across restarts). The snapshot's end line
  names it; the replica keeps it with the copy, set only by a snapshot, and sends it on every sync (`gen=`, even
  empty) and write batch (`POST /v0/team/writes?gen=`: a query parameter, since an older home refuses an unknown
  body field). A different one is `410 resync` whatever the cursor, so a copy whose cursor survived the restore
  still drops what the home lost, and its queued writes wait for that snapshot. A queued write on an item the
  restore lost fails saying so, and its session goes `needs_human`. An acked write that snapshot does not show
  (the home took it, then lost it) stops overlaying and is kept as failed, saying so, its session `needs_human`;
  one already collected (the copy showed it) goes unreported. A copy taken by a zm before generations re-snapshots
  but counts no restore (what it lacks may be ordinary deletions). The replica logs how many items and writes it
  dropped. Older replicas send no `gen` and are unchecked; an unreadable generation file refuses syncs,
  snapshots and write batches that name one (`503`, read again each time). A restore by hand mints none. Before
  anything lands the restore also writes `$ZM_HOME/restoring` (the set stamp; never in a set) and removes it only after
  every file and directory is synced; a daemon refuses to start on a home that has it, or whose marker is unreadable
  (`zm backup restore --to <new dir>` again).
- Clocks: ordering never uses clocks; lease deadlines, invite expiry and stale ages use the authority's clock.

- **As built (round C)** (routes in API.md "Local-first replicas (round C)"; why in DESIGN-NOTES.md "Local-first
  replicas"):
  - **The changelog** is the team project's event log (17.8). `GET /v0/sync?project=&since=&limit=&wait_ms=` is
    `events.Bus.Changes`: the caller's visible rows after `since`, redacted for it, oldest first; `cursor` moves only to
    an id the caller was sent (with nothing visible it stays put, so gaps reveal nothing); `wait_ms` (at most 25 s)
    long-polls on the bus and ends at once with `409 rescope` when the caller's scope_version moves, `401` when its
    token is revoked, and an empty answer when the daemon stops. Area transitions are computed from the log, not
    stored: a reader of only some areas gets a tombstone `dev.zm.sync.gone` for a work event in an area it cannot
    read whose previous work event about the same subject was in one it can (an item that left its view; one
    whose earlier events were pruned, or that arrived by snapshot with no event in the log, gets none, so such a
    row stays in a guest's copy until its next snapshot). The prune records how far it cut (`events_meta
    pruned_upto`); a cursor below it is `410 resync`.
  - **A change and its event commit together**: in a team project a local item's creation, status change and
    link, a comment, a mail (sent, its orphan copy, taken) commit their row and their event in one `TeamTx` on the
    file's writer (closing 17.3's "not done: moving them onto the writer"); claims, releases and expiry already
    did. A restart between the two would leave a row no replica ever hears of (the load test caught a copy one
    comment short for good). Item events read the item (and its live lease's holder) inside that transaction, so
    an event carries the item as it is at its place in the log; rendering after the commit let a later event
    carry an older state. Tracker items' adapter calls stay outside (the tracker is not in the transaction).
  - **The snapshot** reads its cursor first (`Bus.LastVisible`: the newest id the caller may read; everything committed
    later has a larger id), then streams every item (all statuses, links checked per target), comment, live lease
    and mail the caller may read (`orchcore.Snapshotter`, implemented by the work and mail modules), not in one read
    transaction: a change committed meanwhile is in the snapshot and comes again through sync, and applying a
    change twice is harmless (rows are upserted whole, comments and mail by id).
  - **The copy** is `$ZM_HOME/replicas/<ULID>.db` (`store.OpenReplica`: WAL, its own migrations). A write queued in
    the outbox commits with synchronous FULL (an acknowledged write survives a power cut); what can be had again
    commits on a second connection with synchronous NORMAL (`store.OpenReplicaApply`): snapshots, sync pages and
    items fetched whole, settles of accepted writes (the authority's write records answer a replayed write; a
    refusal settles on FULL, since its caller is told at once and a replay would report it again) and deleting settled
    rows. A power cut can take those back to an earlier consistent state, cursor included, never corrupt them:
    items (with `base`, the newest change id applied to the row), comments, leases, mail, announcement-seen marks,
    the outbox and the cursor. A snapshot replaces the rows in one transaction; each sync page applies in one
    transaction with its cursor. An item event about an item the copy lacks (it came into view), a `linked` event
    (events carry no links) and an import fetch the item whole in the background. `zm join` waits up to 10 s for
    the first snapshot. Reads are served from the copy once it landed (the routes in API.md; lists from a stale
    copy carry `behind` and items `stale`; `zm who` asks the authority while it answers, since the copy keeps no
    presence); the hook notice counts the copy's unread mail (falling back to the heartbeat cache before the first
    snapshot). Stale means the last call to the authority failed or nothing answered for 30 s.
  - **Writes** commit to the outbox (one local commit) before anything else. Comments, status and links answer at
    once and are overlaid on reads until the copy shows them (a link to a work item the copy does not hold, another
    project's, waits like a claim, so a target the authority cannot reach is refused to the caller) (a status answer carries `write`, its outbox id: `zm work
    status` and MCP `work_set_status` wait up to 3 s on it, report a refusal as an error and a pending write as
    queued, never as done); claims, releases, leases, mail, replies and acks wait
    up to 5 s for the authority's answer while it is reachable and relay it (a claim's 409 is the caller's, not a
    queued conflict), else answer at once (`202 queued` for lease writes; the mail with `via` "queued (...)"; an
    optimistic ack). `POST /v0/work` still goes to the authority (`503` while it is unreachable); the new item
    enters the copy at once when its answer still belongs to the current copy generation. One drain per authority sends pending entries in order, up to 100 per
    `POST /v0/team/writes`; an answer that stands settles the entry (acked, conflict or failed), applies the
    authority's answer to the copy (read your writes; never one older than what the copy shows, and a claim's
    answer only as a lease: its item is a view) and then hands it to a waiting caller; "not now" leaves it and every
    later entry pending (backoff 250 ms to 5 s). Later pending status writes of the same item take what an earlier
    one left (`seen`) as their base. Refused entries stay as `conflict`/`failed` for 7 days (`zm outbox`), except one that no longer applies: a refused status, claim, lease, release or link of an item the copy shows done (or a status write asking for the status the item has), or a failed link to a work item the copy does not hold
    (another project's: it never applies by waiting), dropped once the drain settles it or the copy moves, with a log record; a session
    whose queued claim, lease or status lost, or whose queued link to another project's item failed, becomes `needs_human` (note `queued claim conflict: <key>`). So does a session whose queued mail or reply the authority refused with any 4xx (note `queued <mail|reply> <failed|conflict>: <error code>`, with `(<rule> limit)` and `retry after <wait>` for a send limit, and a pointer to `zm team outbox` (read as the sending session), which has the authority's message: the note is listed beyond the mail's audience, so it never carries the message, which can name the recipient): told once, when the entry settles. It is told through the session's own state, a local write, never by mail, so no send limit reaches the notice and it cannot start a reply loop; the refused entry stays in `zm outbox`. Acked
    entries go once the copy shows them. Bounded at 10 000 pending; writes are refused after 7 days without an
    answer.
    Copy mutations are fenced by snapshot generation and known target ordering. A fenced result still settles
    the durable outbox entry and its waiting caller; its payload cannot reapply through overlays or appear in
    current outbox views (including `--all`). Pending writes keep their normal visibility and replay behavior.
  - **The authority's write path**: each write of a batch runs as the route it names through the daemon's own
    handler, as the replica (and the session in `as`), with `Zm-Write-Id`; one batch per (identity, project) at a
    time; its answer is recorded in `team_writes` (project.db, 30 days) and a replay gets the record, except a
    `429 rate_limited` (a send or prompt limit: it depends on time, so it is answered, the batch goes on, and it is
    never recorded, live or queued). Comments and
    mail are stored under the write id (a replay after a lost record still stores nothing twice; another sender's
    use of an id is a conflict). A status write with `base` (sent as `Zm-Base`) is refused (`409`, recorded) when
    the item's last `status_changed` is newer than base; for a local item the work module checks it in the
    `TeamTx` that changes the row and mints the event, so no status change (a batch's or the authority's own
    socket's) can slip between; a tracker item's is checked before the adapter call. Only a status event a
    replica's own write minted (`status_seen`: its status write, or its claim that moved the item to claimed)
    raises a later status write's base, in the batch and on the replica; another write's events never do (they
    would hide a status change the replica never saw). For a tracker item, the claim answers at once and its
    tracker write-back (one FIFO worker) runs later: the status events it mints (the move to claimed, and the
    release of an expired holder the claim replaced) carry the claim's lease (`writeback_for`, holder and claim
    time; owner-only, so no replica or guest receives it), and the check skips an event carrying the writer's own
    live lease, however late it was minted. Any other status change after base, another writer's or a
    write-back for another lease, still refuses it. A batch runs to its end even if the replica hangs up.
    Not done: offline item creation, field writes other than status (there is no title route), tracker items' own
    status changes (`work.changed`) counting against a base, and a quiet guest's cursor advancing (it stays on the
    last id it was sent, so once pruning passes it the guest resyncs although it missed nothing).
  - **Lifecycle**: the copy opens at start and at join, its loops (sync, drain, refresh) stop at shutdown; a
    rescope (scope_version, role, areas or cap moved) takes a snapshot again; a revocation, a purge (three 401s over
    10 minutes, which the replica's own sync now meets) and `zm leave` delete the copy. Data already copied cannot be
    recalled (17.4). Role regrant can reopen a fresh copy after teardown; token purge and explicit leave are terminal.
  - **CLI**: `zm outbox [--all]`; `zm remotes` shows each copy (state, age, reachable, pending, refused); a queued
    claim, release or lease exits 4 with the write's id (`queued:` on stderr).
  - Tests: `internal/team/replica_test.go` (offline reads, outbox order, a queued claim that loses, replay and stale
    base, resync with pending writes, a guest copy holding only its area), `potluck/kit/events/changes_test.go` (cursor
    never on a hidden id, tombstones, paging, 410 after a prune, concurrent writers past the prune), and round B's
    tests where cross-machine reads now wait for propagation (`eventually`, at most 2-3 s).

### 17.7 Proof: budgets and load test (round C; a claim-race test ships in round B)

- Budgets (p95; the authority on a small always-on box): replica read <= 50 ms; claim over the tailnet <= 250 ms
  (<= 50 ms loopback in tests); a change visible on every other replica <= 1 s; tracker write-back <= 10 s;
  hooks unchanged (<= 15 ms, local only). Round B's proxy reads: <= one tailnet round trip + 30 ms.
- `scripts/loadtest` (`make loadtest`): a scratch authority with a scratch Dolt server (never a live one), 5
  replica daemons with 8 agent workers each, for at least 10 minutes: replicas resuming sync while writes commit
  concurrently, claims racing on the same items, renewals,
  releases, comments, status changes, mail, sessions starting and ending, one replica offline past
  `bound_lease_ttl` while holding a lease, one authority restart, and a label flip moving items in and out of a
  guest replica's area plus one area-rule edit. It asserts: one winner per contested claim; no lost or
  duplicated write; every replica converges to the authority (diff empty) within the propagation budget after
  load stops; the tracker matches the authority after write-back; no guest replica ever held a row outside its
  area, and tombstones removed every row that left it; every steady-state p95 under its ceiling in
  `scripts/ceilings/ceilings.json` (a ceiling above a number here is listed in DESIGN-NOTES "The speed gate" with its
  reason). CI uses `-budgets=false`: latency misses warn in the report; missing operation samples and
  correctness failures still fail the run. The CLI defaults to strict budgets. It prints the latency table for the dogfood log.
- Other acceptance tests: invite and join (wrong, used, expired code; a lost redeem response; redemption and
  token use from another node and from the socket; a taken `zm-<machine>` name; a mismatched authority
  fingerprint), rescope and revoke purges, offline reads and outbox replay after the retention window, a stale
  `base` refused, `410` resync with pending writes, the environment allowlist, the `--server` schema check,
  provisioning checksum refusal, import repairs, skips and resume, and the golden leak test extended to sync,
  snapshots, invites and presence. Built in B1: invite and join (all of the first list), rescope and the 401
  purge, and the golden leak test extended to invites and proxied calls (`TestGoldenLeakTeams`: invites in every
  hidden place, then a guest replica walking every route on its own socket). Built in B2: the same test extended to
  presence and cross-machine mail (a member replica's sessions in a hidden area and at project level known by
  presence and mailed; group mail to the guest's machine in a hidden area; mail to the guest's session from a hidden
  replica session; the guest walks every route at the authority over three transports and on its replica, where
  mail and session routes must answer hidden like missing; its notice counts only what it may read; a guest cannot
  probe which machines joined). Sync and snapshots wait for round C.

- **As built (round C)**: `scripts/loadtest` (`make loadtest`, `DURATION=` and `OUT=`) runs an authority, 5 member
  replicas x 8 agent sessions and 1 guest replica (`harbor/solver`) x 4 in one process over the loopback tailnet
  (`tailnettest`, each daemon in a throwaway `ZM_HOME`, `BEADS_*`/`BD_*`/`DOLT_*` unset) for 10 minutes: agents race
  claims on 40 local items (TTL 30 s) and in the same cycle comment, change status and release; comment on any item;
  mail agents on other machines; read and ack their inbox; list and show work; read the hook notice; flip their session
  state (presence). At 30% one replica is cut off from the authority (`tailnettest.Net.Cut`: open connections drop, new
  dials hang) for 30 s; at 60% the authority restarts. A probe comments on one replica every second and times the
  comment's arrival on the others. The guest's agents work, mail and read in its area only, and its copy file is checked
  every 2 s for any row outside it (items of another area, their comments and leases, mail filed elsewhere). Afterwards
  it asserts: every outbox drained; every copy equals the authority as its scope sees it (items with status and owner,
  leases, comment counts per item, every mail and whether it is taken; a holder the guest may not see is `someone`); the
  guest's copy never held a row outside its area; every comment and mail an agent was given an id for (200 or queued) is
  at the authority exactly once under that id; no claimed event in the authority's log names a new holder while another
  holds the key. Measured: DESIGN-NOTES.md "Local-first replicas". Not covered: tracker write-back against a scratch
  Dolt server (native items only), items moving in and out of the guest's area (local items do not change area; the
  tombstone path is covered by `potluck/kit/events` tests), a replica offline past `bound_lease_ttl` holding a bound lease
  (the agents' leases are TTL leases), and a real tailnet.

### 17.8 Amendments to sections 1-16 for team projects

Section 2 "no client-side sync" holds through round B; from round C team-project replicas keep a synced copy.
Section 2: team-project leases live in project.db, whose pool is one writer and four readers. Section 12: a team
project's event log is its changelog, kept for 30 days and 200 000 rows (whichever is longer), and its ids are
minted in commit order (17.3), with item events carrying the item as it is at their place in the log (17.6).
Section 4: `release --force` stays authority-local. Section 10: claims as in 17.3 (from round C a replica queues a
claim made offline: 17.6). Section 13 and 15: cross-machine mail and presence exist for team projects. Solo
projects are unchanged everywhere.

### 17.9 Tracker backups (built; Dolt homes retired 2026-11-10)

A tracker keeps zero or more **backups**, each named by a target, run by the daemon on a schedule. Routes in API.md
"Tracker backups"; why in DESIGN-NOTES.md "Tracker homes".

**Retired 2026-11-10.** Dolt homes are gone: `zm tracker home` (moves, freezes, rollback, abort, admin
logins), `zm tracker restore` (a Dolt home from a dolt backup), `zm tracker serve`, `zm tracker env`, the `dolt`
backup kind, zm's local and ssh:// dolt servers, the pinned dolt, and `zm issues import` (a beads Dolt database into
native issues; import from a release before this one). Their routes answer 404. A `home.json` written before keeps
loading: its `admin`, `move` and `previous` records are ignored and dropped on the next write; its backups stay.

**Lego contract (section 11).** describe may list native backup kinds (`"backup": ["sqlite"]`, the native
adapter's). **Every** adapter gets the neutral `export` backup with no verb of its own: a JSON Lines snapshot of its
items from the mirror. A tracker's own service or store is its home; zm never moves it.

**Targets.** `file:///abs/path` (also an absolute path or `~/...`) and `icloud[:subdir]` (macOS:
`~/Library/Mobile Documents/com~apple~CloudDocs/zm-backups`). Any other target is an unknown target (400). A target
never carries a password (refused). The whole home goes off-site through rclone (below, "Off-site home backups").

- **`zm tracker where [prefix]`**: the tracker, the mirror, every backup (kind, schedule, last success and its age,
  failures with the error, size, path) and notes (no backup, an export only for a native store, failing backups).
- **`zm tracker backup add <p> <target> [--kind sqlite|export] [--every 1h] [--keep 48] [--now]`**, `ls`, `rm` (the
  schedule goes; the copies stay), `run [id]`. Default kind: the tracker's native one, else export. One restore or
  backup runs per tracker at a time (409). The daemon checks every minute and runs due backups one at a time per
  tracker, each tracker in its own goroutine (a long copy of one never delays another); a failure retries after 10
  min and shows in `where` (`FAILING (n x): ...`). A `file://` directory must exist (an unmounted volume is an
  error, not a new folder on the boot disk); `icloud` needs iCloud Drive. Copies live under
  `<target>/<slug>-<prefix>/<kind>`:
  - `export`: the mirror's items of that tracker, every status, with zm's comments, one per line after a header
    line (`zm_export`, project, tracker, adapter, items, synced, state), written atomically as
    `<slug>-<prefix>-<UTC>.jsonl`; the newest `--keep` stay. Refused while the mirror is on its first sync (a snapshot
    must be whole); a stale mirror is written and its header says so.
  - `sqlite` (native trackers): a set of copies, `<slug>-<prefix>-<UTC>.issues.db.enc` and `.project.db.enc`,
    encrypted ("Backup encryption" below: `project.db` holds secret names). Each store is first copied into the
    tracker's staging in the home (`moves/backup-*`) with `VACUUM INTO` (it does not hold up the writer) and
    checked there: `PRAGMA integrity_check`, its size matched its pages, and the issue store's copy is not under a
    quarter of the last good one (a store emptied by mistake must not rotate good copies out; to accept a real
    shrink, `backup rm` and `add` it again). It is then encrypted onto the target as `*.tmp`, read back through the
    cipher and compared, and only then named. So `last_ok` means a verified file exists, and a failed run prunes
    nothing. A run fails at once, writing nothing, when the home's disk has under 1.1 times the source free; a target
    that fills mid-write keeps no partial file. A copy is 0600 from before its first byte. The plain copies (`.issues.db`,
    `.project.db`) an older zm wrote are deleted once an encrypted set is there (a run that cannot delete them
    fails); restore still reads them. `file://` targets only, hourly by default, keeping 24 sets: `icloud` is refused (400), because each copy is a
    whole store. A leftover from a crash (`*.tmp`, or staging in `moves/`) is removed by the next run.
  - `tracker ls` and `where` show the newest copy found on disk, from its name, not from the record: a backup whose
    copies are gone shows that, whatever `last_ok` says; `OVERDUE` past twice its period, `FAILING`, and `REDACTION
    WAITING`. A native tracker counts only sqlite copies when it has any; an export cannot restore a store.
- **A redaction and sqlite copies.** After every module's scrubber ran and the stores were compacted, each sqlite
  backup of the project's native trackers is marked (`scrub_due`, in home.json, no text), runs, and deletes its older
  copies only once the fresh set is verified. If that fails, the old copies stay, the mark stays, and the redaction
  answers `unavailable` ("not fully scrubbed"). The scheduler retries a marked backup (the failure retries above, and
  a daemon restart loses nothing); redacting the comment again retries at once. A marked backup never ends with no
  copy. A run that began before the mark does not clear it. A redaction first waits up to 30 s for a backup in
  progress, which holds a lock its scrub needs and a read that keeps its compaction out. Copies under an old
  tracker prefix in known sqlite targets (including targets in retained old home.json records) are reported as
  not fully scrubbed and kept: no current schedule proves they were replaced. This also checks old-prefix file
  names left in a current prefix's directory, including plaintext and temporary copies. A folder that could be
  either of two projects' (`harbor-ios-issue`: `harbor`'s `ios-issue` or `harbor-ios`'s `issue`) is the other
  project's only when that project is known to use its prefix (tracker or tracker folder) and this one is not;
  otherwise it is reported. This holds in both directions. A reported copy stays until deleted by hand; the
  redaction's hint names it. Known gaps: names cannot tell the two apart, so when both projects are known to use
  the folder's prefix, each reports the other's live copies as its own; when only the other project is known to
  use it (this one forgot an old prefix), this project's copies there are not reached; after a slug rename,
  folders under the old slug are not scanned. Not reached: copies of a backup unscheduled with `rm` whose target
  is no longer known, copies moved elsewhere, and (out of scope here) an export snapshot iCloud Drive evicted:
  `exportscrub` rewrites files, and a stub has no content.
- **`zm issues restore [<prefix>] [--from <backup id|file:///path|path>] [--at <stamp>] [--dry-run] [--force]`**
  (`POST /v0/issues/restore`): a sqlite copy becomes the project's native store. `--from` is a backup id, a target (or
  directory) holding copies, or one copy; by default every sqlite backup's newest copy competes.
  `--at` matches part of a copy's stamp. In order: the copy is staged and checked; the issues module opens that staged
  file, refuses a schema with migrations this binary does not know (409), a copy of another project's store (409), a
  file that is not an issue store (400) and one taken during the retired beads import (409), migrates an older one up, and applies
  the redaction list to it, compacting it, so a redacted text is never served; `--dry-run` stops here, and proves the
  copy restores. Then writes are held, the current store is copied into the backup's directory as a normal set (so a
  restore is undone by another, and that copy is purged like any other; if it cannot be copied the restore is refused
  unless `--force`), and the copy replaces the store in one rename: a crash leaves the old store or the new one. The
  old store's log is set aside first and put back if the rename fails. A swap that fails names the kept copy in its
  error. The tracker's mirror is wiped and listed again, because a mirror keeps the newest of each row and these rows went back
  in time. Issue changes since the copy are gone. `project.db` is not restored: mail, leases and local work stay. A
  lease may then name an issue that no longer exists, and an open web page shows the old issues until it reloads. A
  `project.db` copy is for a machine that lost the file: with the daemon stopped, put it at
  `projects/<ulid>/project.db`; use the newest set, since one older than a redaction holds what the scrub removed.
**Off-site home backups.** Tracker backups copy one tracker; this copies the whole home off the machine, so a
dead box (an authority) is rebuilt elsewhere. `zm backup set <target> --home <id>` (`PUT /v0/backup`) sets it,
after checking the target answers with the keys given; `status`, `run`, `ls`, `off`, `restore`; routes in API.md.
- **Targets** (`internal/homes/offsite`, a brick with a conformance suite): `s3://bucket[/folder]` with `--endpoint`
  (Cloudflare R2, any S3), and `file:///abs/path`. S3, and any target with `--encrypt`, go through the pinned rclone
  (`$ZM_HOME/tools`, sha256-checked; the CLI fetches it): its remotes exist only in the child's environment, which
  is all it gets, so keys reach it never through argv, a config file or a log, and its errors are masked for every
  secret. `--access-key`, `--secret-key`, `--encrypt` and `--encrypt-salt` are secret references (section 2's secrets
  brick: keychain:, file:, env:), read at each run; a target URL carrying a login is refused. Every set is encrypted
  with the home's backup key whatever the target ("Backup encryption" above). `--encrypt` names a
  passphrase and `--encrypt-salt` a second secret (rclone crypt's `password2`; one without the other is refused, so
  no set uses rclone's built-in salt): rclone's crypt encrypts names and contents again on this machine, a second
  layer. Lose either and the sets are unreadable.
- **A set**, every `--every` (15 min by default, at least 1 min; 10 min after a failure): `registry.db`, each
  project's `project.db` with `VACUUM INTO` on their own connections, its `issues.db` through the issues module's
  checked copy (the shrink guard of sqlite copies applies), `project.toml`, `config.json` and each
  tracker's `home.json`, staged in `$ZM_HOME/offsite/staging` (0700, removed after; a crash's leftover by the next
  run), gzipped, encrypted with the backup key and written as `<home>/<stamp>/files/<path>.gz`, then
  `<home>/<stamp>/manifest.json` last (each file's size and sha256, the cipher and key id): a set without its manifest is a leftover the next run removes. A file whose sha256 equals
  its namesake in the last set is copied on the target, not sent. Never sent: `secrets/`, `receipt.key`, tokens
  (`remotes/`, `approvals/`), `dolt/` (left by a retired Dolt home), `tools/`, sessions and logs, replica copies.
  The staged `config.json` removes `tunnel.edge_secret`; when it was nonempty it also removes `tunnel.listen`, so
  a restored listener cannot start with weaker edge trust. Other settings, including unknown fields and secret
  references, stay; the live config is unchanged. Malformed config or repeated tunnel/edge-secret fields fail
  the backup without disclosing its text. The staged `registry.db` has every module's secrets blanked
  (`core.BackupScrubber`) and is rewritten so no free page keeps them: webhook secrets and the GitHub link's webhook
  secret (a restored home re-keys hooks and links again), connections' plaintext tokens (`token_ref` stays; the
  provider refuses a restored connection until its admin registers it again: `zm connection rm`, then `add`), and
  every web push subscription (endpoint and keys; each device turns alerts on again). Not yet blanked:
  `invites.pending_secret` (a Potluck change).
  This sanitation applies to new sets; it does not scrub previously written sets.
- **Retention** after each good set: the newest `--keep` (8), the newest of each of the last `--keep-hourly` hours
  (24) and of each of the last `--keep-daily` days (30, UTC). A failed run deletes nothing.
- **Redaction.** A redaction's purge (above) marks the backup (`scrub_due`) and runs it: a set taken after the mark
  is sent, then every older set is deleted; until both succeed the mark stays, the redaction answers "not fully
  scrubbed", and the scheduler retries. A redaction first waits (up to 30 s) for a run's copying, not its upload.
- **Status.** `last_ok` (when the newest set that was fully sent was taken), its stored bytes, failures and the last
  error are in `$ZM_HOME/backup.json`, `GET /v0/backup`, `GET /v0/health` (local callers, `backup`) and the sync
  answer a whole-project replica gets, which shows it on its authority row of `GET /v0/work/sources`.
- **`zm backup restore --from <target> --home <id> --to <dir> [--at <stamp>] [--key <ref>] [--dry-run]`**: no daemon.
  `--to` must be empty but for `tools/`, `secrets/` (where the backup key may wait) and the lock files below. It holds `--to`'s daemon lock (`daemon.lock`, the flock a daemon
  takes) for the whole restore: a daemon running there refuses the restore (`409`), and none can start there until
  it ends, so no autostart writes a registry the restore would rename over. Every file is fetched into `<dir>/.restore`, decrypted, gunzipped and checked against the manifest
  (size, sha256, and `integrity_check` for a database) before any is moved into place; a set written by a newer
  layout is refused, and so is a manifest naming any path but those a set holds (`registry.db`, `config.json`,
  `top.json` from older sets, `projects/<ulid>/{project.db,issues.db,project.toml}`, `trackers/<ulid>/<prefix>/home.json`; an
  upload refuses any other too): restore never accepts an executable, a secret-file path or a setting such as
  `backup.json`. `--dry-run` checks a set and keeps nothing. A crash or error while moving files in leaves a `restoring` marker, and the daemon will not start on that home. The restored home then needs its secrets again
  (tracker passwords, `receipt.key`, team tokens); reconfigure the tunnel listener and edge trust before enabling
  a listener disabled by sanitation. A restored webhook sends nothing until re-added (`zm webhook rm`, then
  `add`: its last error says so), and the GitHub link answers no delivery until its secret is set again. Older
  sets may still contain `tunnel.edge_secret` and signing keys; restore does not scrub those historical files. Each restore writes a new restore generation first (17.6 "A restored home"), so
  members drop what the set lost. `ZM_HOME=<dir> zm daemon` starts the home and migrates it up.
- **Watchdog heartbeat.** `zm backup watchdog <url> --home <id> --secret <ref>` (`PUT /v0/backup/watchdog`, which
  sends one at once): every minute the daemon POSTs `{"v": 1, "home", "sent", "version", "commit", "sync": {"ok",
  "sources", "failing": <count>}, "backup": HomeBackup without "error" | null}` (a tracker counts as failing after
  10 minutes of failed syncs). Counts only: no project, tracker or path name, no error text, no issue data, no
  secret. Signed per Standard Webhooks (`webhook-id`, `webhook-timestamp`, `webhook-signature`, section 18.1) with
  the home's own `whsec_` secret. Outbound only: a home on a private tailnet is watched without a port. https only
  (http to loopback for tests). The last send's outcome is `heartbeat` in `GET /v0/health` (local callers) next to
  `backup`. A receiver: `deploy/cloudflare/zm-watchdog`, which derives each home's key from its master, drops a
  `webhook-id` seen in 15 minutes, refuses a beat signed before the stored one (409), and alerts once per incident
  when a home is silent 10 min or its last good set is over 60 min old.

**Backup encryption** (both backups are encrypted, on by default, the key with zm's secrets). Sqlite copies and every file of an off-site set are encrypted on this machine, always (no
switch); export snapshots are not (they hold mirror items only, and the export filter keeps secret names out).
- **Key**: `$ZM_HOME/secrets/backup.key` (base64 of 32 random bytes, 0600, `secrets/` 0700), made the first time a
  backup is set or runs, never replaced, and never in a backup (no set or copy holds `secrets/`). Lose it and no
  backup of the home can be read: keep a copy off the machine (`zm backup` says so).
- **Format** (`internal/homes/bkcrypt`, `zm-aes256gcm-stream-v1`): a 40-byte header (magic `zmbk\0v1\n`, the key
  id: 8 bytes of a SHA-256 of the key, a 24-byte random salt), then 64 KiB chunks sealed with AES-256-GCM under
  HKDF-SHA256(key, salt); a chunk's nonce is its index and a last-chunk flag, and the header is every chunk's
  additional data. A changed, reordered, cut or extended file does not open; one sealed under another key says
  whose (the key ids). Standard library only.
- **Off-site sets**: each file is gzipped, then encrypted; the manifest (layout 2) names the cipher and key id
  (`cipher`, `key_id`) and stays readable (paths, sizes, hashes; no content). A file is carried over from the last
  set only if that set was encrypted with the same key. `--encrypt` (rclone crypt) is a second layer that also hides
  names. A home whose last good set is plain (layout 1, before encryption) deletes every older set once its first
  encrypted set is sent; until that succeeds the run fails and is retried, like a redaction's.
- **Restore**: `zm issues restore` decrypts a copy with the home's key (a plain older copy is read as it is).
  `zm backup restore --key <secret ref>` names the key of the home that wrote the sets; by default it uses
  `--to`'s `secrets/backup.key` (a `secrets/` directory may be there before the restore), else this machine's
  home's; with none, an encrypted set is refused with its key id (`--key` hint), a wrong key likewise, and a plain
  set restores without one. A restored home gets that key, so its own sets go on using it.

**State.** `$ZM_HOME/trackers/<project>/<prefix>/home.json`: the backups and their last runs (and `scrub_due`).

### 17.10 Shared sessions

A managed session on a team project can be **shared**: project members may send it prompts from their own machines,
and every prompt is logged where the project's members can read it.

- **Marking**: at spawn only, `POST /v0/sessions {"shared": true}` or `zm run --shared`, and only in a team project
  (a solo project, or a session in `none`, is `400`). A session is shared for its life; presence carries the flag.
- **What sharing gives**: `operate` on a shared session means driving the owner's agent with the owner's
  permissions, on the owner's machine: a prompt can answer whatever is on screen, a y/n permission prompt included.
  Share only with people you would let type into that terminal.
- **Sending**: `POST /v0/sessions/{address}/prompt {"text"}` (`authed`). For now only a person sends a prompt:
  agents are asked not to, and zm refuses (`403`) a caller that says it is an agent session (`Zm-As`, on the replica
  and at the authority alike). This is a courtesy, not a boundary: an agent on the owner's machine shares the
  owner's socket and can simply not say so, so it cannot be enforced. The caller needs `operate` on the session's
  project and area (a guest cannot; a reader who cannot see the session gets `404`, one who can but may not
  operate it `403`), and the session must be shared, managed and live (else `409`: a non-shared session refuses
  remote input; its owner types into it with attach or `zm session input`, which stay `local`). The text is at most
  8 KiB, and only printable text, newlines and tabs: any other control character (an escape sequence, `^C`) is
  `400`, so a prompt can never act as a terminal key; so is a prompt with any line whose first character, past
  whitespace (Unicode's), U+FEFF and U+200B, is `!` (Claude Code runs it as a shell command with no permission
  prompt) or `#` (its memory mode, which writes the owner's `CLAUDE.md`), on any line because any line can become
  a prompt of its own if bracketed paste is lost; so a Markdown heading is refused too. So is one with a bidi control (U+202A-202E,
  U+2066-2069) or a Unicode line or paragraph separator (U+2028, U+2029), which would make the log show other text
  than the agent reads; zero-width characters are allowed. Slash commands (`/...`) are allowed. Several lines are typed as one bracketed paste when the
  program turned bracketed paste on (DECSET 2004, watched in its output), else `400`. No Enter is appended: the recipient reviews the staged input and submits it themselves.
  Staging does not mark a permission prompt answered. Delivered means staged, not submitted or processed.
- **Routing**, the way mail goes (17.3): the project's authority decides and logs every prompt. On a replica the
  route is forwarded to the authority (a session of this machine by its project; another machine's, to each joined
  authority while `404`). The authority types a prompt to its own session at once; a prompt to a replica's session
  waits (`state: pending`) until that replica's next heartbeat, whose answer carries the session's pending prompts;
  the replica types them in order, each at most once, never twice: it records a prompt (`team_prompts_typed`, kept
  across a daemon restart) before typing it and skips any prompt it has a record of. A prompt it cannot record is
  not typed (the next beat tries again); one the session refuses before typing (not running, no bracketed paste)
  loses its record and is tried again; a crash between the record and the typing, or a terminal write cut off
  midway, loses that prompt rather than typing it twice (never reported delivered, it expires). It reports typed
  prompts delivered in the following beats until
  the authority has taken the report, also for a session that ended right after typing (sent as an ended entry,
  which the authority marks before it ends the session's pending prompts) and after its own restart. A replica
  with a live shared session heartbeats every 2 s, so a prompt arrives within a few seconds; a prompt still pending
  when its session ends, or 10 minutes after it was sent, is `expired` (a late report that it was typed still
  marks it `delivered`), and a heartbeat answer carries at most 20
  pending prompts, oldest first (a replica coming back types only those). A sender may send one session at most
  10 prompts a minute (`429` with `Retry-After`).
- **Members only**: the prompt log, `last_prompt` and `in_use` are for whole-project readers (members, and the
  owner); an area guest who can see the session gets it without them, in reads and in events (members-only keys,
  section 12), and `403` for the log.
- **The log**: `GET /v0/sessions/{address}/prompts` (read on the session, whole project) lists the session's prompts, newest
  first (at most 200): `id`, `from` (the sender's address, redacted like any address the reader may not see),
  `who` (the sender's identity), `text`, `at`, `state` (`pending`, `delivered`, `expired`) and `delivered_at`.
  It is kept at the authority, in registry.db (`session_prompts`), for 30 days.
- **Who is using it**: a session's `last_prompt` (`{who, at, text}`, the text cut to 120 characters) and `in_use`
  (`{by, attached, typed_at}`: the machine owner's writable attaches now, and when a person last typed into it,
  shown for 5 minutes) go to presence and every answer about the session, so a UI warns before sending a prompt only
  when another person is attached or typed lately.
- **Structured**: a structured session (`mode: "pipe"`, SPEC 8) shared at spawn takes no prompts
  (`409`); members drive it with its own routes instead: `GET /v0/sessions/{address}/events` and `POST .../turn`,
  `/interrupt`, `/answer`, with the rules above (operate on its project and area: hidden `404`, visible `403`; no
  agent session for an operation, `403`; live, `409`) and no `images` (a path names a file on the session's
  machine: `400`). The adapter's capabilities ride in presence, so one it lacks is `409` before anything is sent.
  The session's machine answers its own; another machine's goes through the project's authority (a replica
  forwards the call there, a stream copied as it comes): **reads** leave a want at the authority, the next heartbeat
  answer for the session carries `stream` (the seq to read after), and the machine uploads a page of its spool
  (`POST /v0/team/session-events`, its node-bound token, read under its own redaction barrier) into a ring of the
  last 4 MiB per session that the authority serves; a page no reader waited for in 30 s is dropped. A read waits
  at most 25 s (a replica's forward gives up on 30 s of silence). **Operations** are machine ops (SPEC 8 "Machine
  choice", kind `session_op`): taken with a heartbeat answer, recorded before they run, run at most once as the
  machine's owner, the result reported in the next beat; the caller gets it, or `202` pending after 15 s, and an op
  no beat took within a minute is dropped. A shared live session beats every 2 s, so both take a few seconds. The
  ring is memory only; an issue redaction in the project drops the project's rings, and for an hour any uploaded
  page that still holds a redacted text is dropped and the session's relayed reads are `409` (withheld pending
  redaction, until its machine scrubbed them: SPEC "Comments: redact"). An upload
  stays under 4 MiB; an event bigger than that is not relayed (its seq is skipped). The stream re-checks the
  caller each page. A session that ended before a member read it is read on its machine.

### 17.11 Joining over a Cloudflare Tunnel (built 2026-11-01)

A member can join a team project and sync with its home without Tailscale. The home runs `cloudflared` in front of a
loopback HTTP listener; members reach it at an `https://` URL. Cloudflare is transport only: auth stays zm's (17.4's
invites, per-member tokens, scope_version, every scope check). Routes and wire format in API.md "Tunnel"; why and
the threat model in DESIGN-NOTES.md "Joining over a Cloudflare Tunnel".

- **Configuration only, off by default**: `"tunnel": {"listen": "127.0.0.1:7435", "url": "https://zm.example.com"}`
  in config.json. `listen` is loopback only; `url` is https (http only to a loopback host, for rehearsals), with no
  path. A home serves the tailnet and the tunnel at once. `GET /v0/tunnel` (local) shows the listener and this
  daemon's key fingerprint.
- **Keys**: every daemon has one ed25519 key (`$ZM_HOME/tunnel/key`, 0600, made on first use). Its id
  `zmkey:<fingerprint>` takes the place of the tailnet node ID for everything 17.4 binds to a node: the token
  (`tokens.node`), the machine name (`team_nodes`), idempotent redemption. A key-bound token is `401` on the tailnet,
  and a node-bound token is `401` on the tunnel.
- **The home key across a restore**: a backup set never holds the key, so a restored home would
  make a new one and every member would refuse it. `tunnel.key` in config.json names a secret reference
  (`keychain:`, `file:`, `env:`; a reference, so a set may carry it) that also holds the key. With it, a home
  with no key file reads the key from there and writes the file; it is not "fresh" (the replay hold applies). A
  named place that cannot be read leaves the daemon with no key (`GET /v0/tunnel` says so), never a new one. A key
  file there wins (a restart reads no secret). `zm tunnel key escrow [--to REF]` writes the key there (default on a
  Mac: the login keychain, `keychain:zm-home-key:<fingerprint>`, through `security -i` on stdin, never argv;
  `file:` is written 0600 and never over another value; `env:` is the host's to set; a `file:` inside the home or relative is
  refused; a keychain item is never written over) and `POST /v0/tunnel/key` records it only after checking it
  holds this key. A daemon with no key adopts the key a new reference holds (the recovery step after a restore
  whose escrow path moved; not fresh); clearing is refused while it has no key (that would make a new one).
  `zm tunnel key clear` stops naming it. The keychain item is local (Time Machine, not iCloud) and unreadable to a
  daemon started before login; an older zm drops `tunnel.key` when it rewrites config. Planned, stronger:
  the owner's paired device signs a new home key, so no copy is kept anywhere.
- **Invites**: `zm invite <project>[/<area>] --as NAME --via tunnel` prints `<url>#<home fingerprint>:<secret>`, a
  26-character fingerprint of the home's public key and a 25-character secret (125 bits). `--via` defaults to the
  tailnet while this machine's node runs, else to the tunnel when `tunnel.url` is set. `zm join` takes either kind;
  an https code needs no tailnet, and `zm remotes` shows each remote's transport (`via`).
- **The door** answers nothing to an unsigned request (`401`). Every request is signed by the caller daemon's key
  over the home's fingerprint, the method, the path and query, a date (within 5 minutes of the home's clock; else `400`),
  a nonce (16 to 64 base64url characters, never reused by a token caller's key: kept 2×MaxSkew plus a minute past its
  date, across restarts in `$ZM_HOME/tunnel/nonces`; else `400`; a journal that cannot be written is `503`, one that
  could not be read at start holds writes off, `503`, for the same 11 minutes), `Authorization`,
  `Content-Type`, every `Zm-*` header (length-prefixed values) and the body. Each member key has its own replay
  budget (`503` past it). Live forwarded writes carry `Zm-Write-Id`, as queued ones do, over both transports; the
  authority runs a write id once and answers a repeat from its record (`Zm-Write-Replayed: 1`), for every forwarded
  write route; the record is tied to the request, so a reused id with another request is `409`. Reads carry none. Without a token only
  `POST /v0/invites/redeem` passes, and the GitHub join's three calls (SPEC 22 over https): `GET
  /v0/join/github`, `POST /v0/join/github` and `POST /v0/login/github/{id}`; they are signed and fresh like all
  the rest, the key id is the node a login binds to, and the start limits stay (per key, per client address, and the
  tunnel's own pool of 8). The collect path passes only with the shape of the id the authority mints (a ULID),
  so no other route under `/v0/login/github/` is tokenless. These calls have a replay cache of their own,
  in memory, for POSTs only (a replayed GET is harmless), each entry kept to its date plus the skew (the date
  clamped to now plus the skew): at most 64 nonces per client address (IPv4 address or IPv6 /64) and 4096 in all
  as a last resort, `503` past either, so one address cannot fill the cache and block the rest (filling the global
  cap takes about 64 distinct addresses or /64s: accepted). A replay is found in one global map whatever
  address it comes from; the address buckets only count capacity. A replayed
  signed start or collect is `400`. With one, it must be key-bound to the signing key; anything else is `401` before
  any route, public ones included. No tailnet login applies. Live event streams are not served there.
- **Recovery freshness.** A missing or unreadable replay journal leaves a durable `tunnel/nonces.hold`; each
  restart waits a full local monotonic 11 minutes before clearing it. During that hold a member write without
  the current process's challenge gets a signed `503` with one `Zm-Recovery` header (32 random bytes,
  canonical base64url). Only this pre-dispatch refusal carries that header. A capable member verifies the
  complete response against the pinned home and retries once with the same method, URI, body, authorization
  and write id, a fresh nonce/date, and the challenge included in its signature. The door still requires the
  normal token binding, clock check, nonce budget and successful nonce append. A matching challenge proves the
  write was signed for this process; it neither clears the durable hold nor grants access. A new process
  chooses a new challenge. Legacy members and no-token redemption retain the hold; reads need none. No retry
  follows a network error, unverified response or ordinary application `503`; a second refusal is returned.
- **Redemption over the tunnel** sends a proof, never the code: HMAC-SHA256 keyed by the invite's code hash over the
  home's fingerprint, the member's key id and its machine name. Wrong proofs are limited to 5 per client address
  (`CF-Connecting-IP` from cloudflared's loopback peer, and with `tunnel.edge_secret` set only alongside it in
  `X-Zm-Edge-Secret`; else the peer's address) and 50 overall per 10 minutes, apart from the tailnet's limits; the same `403` answers every
  code problem. Redemption from the home's own key is `403`.
- **Pinning the home**: TLS by the public CA, plus the home's key. Every answer to a signed request carries
  `Zm-Home-Key` and `Zm-Home-Sig` (over the request's nonce, the status, `Content-Type`, every `Zm-*` header and the
  body). The replica reads each answer whole and drops it, before using any of it, when the key is not the pinned
  one or the signature fails (its caller gets `403`: not the home), or when it is unsigned (the tunnel's own error
  page: the home is unreachable, as a dropped tailnet connection is). Unsigned headers are dropped.
- **The same rules over both transports**: forwarded reads, hedges and their budget, the sync loop, snapshots,
  presence, the outbox, rescope, purge and revocation run unchanged. The replica's reader reports progress while it
  reads an answer whole, so a moving answer is not hedged; a hedge gets a client that does not reuse connections.
- **Cloudflare Access (optional)**: `zm join <code> --cf-access-id <id> --cf-access-secret-file <file>` stores a
  service token in `$ZM_HOME/remotes/<ULID>.headers` (0600) and sends it on every call. Access is a second gate in
  front of the home, never instead of zm's tokens.
- **Transports**: a transport is one driver of `core.TeamTransport` (join codes, readying this machine, pinning,
  signing and verifying, the redemption body, invites, the home's redemption check, rate-limit buckets), registered
  once in `base.NewKit` (potluck/wiring). The team module looks drivers up by `via` only. Built: `tailnet`, `tunnel`; planned: the
  host's own Tailscale. `zm invite` without `--via` tries them in registration order, skipping one not serving here.
- **Your own domain** (`zm tunnel setup <hostname>`, built 2026-11-02): one command puts the home at
  `https://<hostname>` on a zone in the team's Cloudflare account. It provisions the pinned cloudflared
  (`internal/tools`, checked against its sha256), runs cloudflared's login with its HOME under
  `$ZM_HOME/tunnel/cloudflared/home` (the URL is printed on its own line: open it on any device and pick the zone;
  nothing goes to `~/.cloudflared`), creates or reuses the tunnel `zm-<hostname with dashes>`, routes the hostname
  to it (refused, never overwritten, when another record has that name), writes cloudflared's config there (ingress
  to `tunnel.listen`, the edge secret as the Host; when `runners.listen` is set, the runner broker's five public
  paths go to it first, SPEC 20), keeps `cloudflared tunnel run` alive as this user's launchd
  agent or systemd `--user` service (it refuses to run as root unless `--as-root`, and says when a Linux user needs
  `loginctl enable-linger`), starts zm's listener live (`POST /v0/tunnel/up`: no restart), and checks
  `https://<hostname>/v0/health` through a public resolver until it answers zm's 401 signed by this home's key over a
  nonce the check sent (as a member's transport checks) and the edge secret has arrived since this run's start. No API token: answers carry `Cache-Control: no-store` (no cache rule), and the secret
  rides in the Host (no Transform Rule). Bot Fight Mode is the one zone setting it cannot change; the check names its
  challenge and prints the dashboard step. Each step is recorded in `state.json` there, so a re-run picks up where
  the last stopped and changes nothing that is in place; a failure before the check undoes what that run made (a
  tunnel it created is deleted unless DNS already points at it), and a failed check leaves the setup for
  `zm tunnel status` to look at again. `zm tunnel remove [--keep-tunnel]` stops zm's listener, removes the service
  and deletes the tunnel only when this machine created it (a reused one is kept, with its DNS record); deleting the
  DNS record needs the dashboard (cloudflared cannot), and it prints the step. A cloudflared in `$ZM_HOME/tools` that
  no longer hashes to what was installed is refused, never run.
  One hostname per home.
- **Sealed secrets**: a renter's `secrets` and a GitHub runner's `jit_config` cross the tunnel
  only sealed, so Tailscale is optional for every pool role, GitHub Actions hosts included. Each daemon has a second
  key, X25519 (`$ZM_HOME/tunnel/seal`, 0600, made on first use; not escrowed: a restored home makes a new one),
  certified by its ed25519 key: `Zm-Seal-Key: 1.<base64url public key>.<base64url ed25519 signature over
  "zm-seal-key/1\n<key id>\nx25519-hkdfsha256-chacha20poly1305\n<base64url public key>">`. A member sends its
  certificate on every request and the home on every answer, inside the signed headers; each side takes it only as
  the identity that signed that request or answer certified it (the key-bound token's key, the pinned home), never
  from an unsigned field. Sealing is HPKE (RFC 9180, Go's `crypto/hpke`, base mode) with DHKEM(X25519, HKDF-SHA256),
  HKDF-SHA256 and ChaCha20-Poly1305, one suite with no negotiation; a sealed blob is `zs1.<expiry, Unix
  seconds>.<base64url(enc || ciphertext)>`, and its HPKE info binds the sender's and the recipient's key ids, the
  expiry and the purpose, so it opens for nothing else (a blob past its expiry by more than 5 minutes does not open;
  every failure is the same `400`). Two legs, hop by hop: a member seals the whole body of a run-target call that
  holds either to the home's key, bound to that request (method, URI, `Zm-Date`, `Zm-Nonce`; it expires with the
  date's window), as `{"zm_sealed": "<blob>"}` with `Zm-Sealed: 1` (the whole body, so no spelling of a field rides
  beside it); the door opens it for a token caller only, before routing, so the route sees the request byte for byte
  as the tailnet would carry it. Either side refuses a run-target body over the tunnel that holds either unsealed
  (under any spelling a JSON decoder folds to them): the member's transport before it leaves (`403`), the door before
  routing (`403`). The authority seals a relayed op's whole arguments, when
  they hold either, to the key the host's poll or heartbeat certified, bound to the project, op id, binding, verb
  and box (`RunTargetOp.sealed` in place of `args`, 10 minutes); only the host opens them, then hands them to the
  driver as a local call's. The authority holds the plaintext in memory for the one call, as over the tailnet, and
  writes neither it nor a sealed blob anywhere. Not end to end: the authority is trusted with both today (it mints
  jit configs, routes and checks calls), and Cloudflare is the party this keeps out. A peer that certifies no key
  (a zm from before this) gets the earlier refusal: a member never sends them to such a home (`403`, naming the
  tailnet), and the authority never hands such an op to such a host (the call answers the same `403`). Calls
  without secrets, every output, and the rest of the API stay readable by Cloudflare. A secret grant's
  body does not: it names a secret, so it crosses the tunnel only sealed, and the door refuses one in the clear.
  The secret ledger (`dev.zm.secret.*`, `dev.zm.secret_grant.*`) is not copied to a replica, an export, or a
  webhook, and a tunnel read of it or of the grant list omits the names. The pool's machine shows them on its
  own socket and over the tailnet.
  **Over the tailnet** the same sealing applies (Potluck PR 39), and a pool's sealing mode decides what a peer that
  names no key gets: `preferred` (a pool with no sealing setting reads as `preferred`; a project turned team now begins `required`) sends to it, and takes from it, in the clear as before,
  with a warning; `required` refuses (`403`, naming the machine; the op never runs). Over the tunnel such a peer is
  refused in either mode, as above. A peer that ever named a key to this machine is never sent secrets in the clear
  again once its pool machine reported `required` (`$ZM_HOME/tunnel/seal_peers.json`, 0600; its owner clears one with
  the `POST /v0/sealing/forget` route). `GET /v0/sealing` shows the mode and which machines cannot seal; `PUT
  /v0/sealing` (admin on the whole project) sets it, and `required` is refused while one cannot, unless `force`.
  Update every machine, check none is listed, then set `required`. There is no `zm sealing` command yet: Potluck's
  `potluck sealing` commands are not importable (its `cli` package keeps its app private), so use the routes.
- What Cloudflare can do: read every request and answer in plain text but sealed secrets and jit configs, and drop
  them. What it cannot: act as a member, replay or alter a request or an answer, redeem an invite it carries, or open
  or redirect a sealed secret (DESIGN-NOTES "Joining over a Cloudflare Tunnel", threat model). A team that must not
  be readable by Cloudflare uses the tailnet.

### 17.12 Secret handoff: the key directory (built 2026-11-15; sealed drops next)

A person or agent hands a secret (a password, an API key, a signing key) to another machine sealed to that machine's
own key before it leaves the sender, so the project's authority stores and relays only ciphertext it cannot open, for
24 hours at most (the "no team store" ruling, 2026-11-10). This step is the key directory; sealed drops (`internal/secretdrop`) and the Decisions
secret card build on it.

- **Keys**: a machine's recipient key is its daemon's existing seal key (17.11 "Sealed secrets": X25519, certified by
  its ed25519 identity key), named by the key id `zmkey:<fingerprint>` and the X25519 public key, the two inputs
  `sealbox.Seal` takes. A published key's fingerprint is 26 base32 characters of sha256 over
  `"zm-secret-key/1\n<key id>\n<base64url seal key>"`.
- **Publish** (`PUT /v0/secret-keys`, `zm secret keys publish`): the key is never taken from a body. On a member
  machine the call goes to the project's authority with the machine's node-bound token, and the key is the one the
  member's transport certified on that call (over the tunnel the key that signed it; over the tailnet the identity
  the member's daemon names in its seal headers: the node vouches for the machine, not for the key, so a member could
  name another key, which only misdirects secrets sent to its own machine); the machine is the one the token names. On the authority, its owner on the
  socket publishes the authority's own key. Anything else (a minted token, a tailnet login without a node token) is
  refused, so nobody publishes a key under another machine's name. Rows are per machine: a key someone names under
  their own machine is that machine's row, never in the way of its holder's, and only its holder opens what is sealed
  to it. One key per machine is `active`; a new key makes the old one `replaced`; a key once replaced or revoked is
  never taken back by that machine (`409`: make a new seal key). A machine's history keeps its 20 newest replaced
  keys and every revoked one (at most 100, then `409` until an admin looks). The publisher needs read on the resolved
  project/area; the key is listed to readers of the area it was first published from.
- **List** (`GET /v0/secret-keys`, `zm secret keys [--to machine:<m>|human:<h>] [--all]`): active keys the caller may
  read, with the owner redacted when hidden (`human:` matches only an owner the caller sees).
- **Revoke** (`POST /v0/secret-keys/{fingerprint}/revoke`, `zm secret keys revoke`): the key's own machine (its own
  row), or an admin of the whole project (every row listing that key). Senders refuse a revoked key.
- **Pins** (the sender's own, `$ZM_HOME/secretdrop/pins.json`, 0600): trust on first use, per project and machine
  (a machine name is unique only within its project's authority). The sender recomputes each listed key's
  fingerprint from its key id and seal key and refuses a listing where one does not match. A listed key
  is `new`, `pinned` or `changed`; a sender seals to a changed key only after a person compares the fingerprint (on
  that machine, `zm secret keys`) and runs `zm secret trust <fingerprint>`. An unreadable pins file is an error,
  never empty. The residual risk: an authority that swaps a key before the first send to a machine (an open owner
  ask, "first-contact").
- The directory holds public keys only: it is in the registry (`secret_keys`), backed up like any table, never a
  secret.

## 18. Webhooks and GitHub linking

Two bricks at the edge, each its own module (`internal/webhooks`, `internal/ghlink`); routes in API.md "Webhooks
and GitHub linking". Neither adds a listener or a dependency, and neither imports another module.

### 18.1 Outgoing webhooks

- `zm webhook add <url> -P <project>[/<area>] [--events work.*,mail.*] [--secret-file F] [--note N]`; `--all`
  (every project and machine events) and `--allow-private` are the machine owner's on its socket only. `zm webhook
  ls|show <id>|rm <id>|test <id>|enable <id>|disable <id>`. At most 200 hooks per daemon.
- **Who may.** Creating, seeing and changing a hook needs `admin` on its project (or area). A hook the machine
  owner made on its socket (no token) is a machine setting: only that owner sees it. A network caller must name a
  project.
- **What it receives.** A hook follows the event bus as its creator: a bus subscription with the creator's
  current scope (the identity's grants now, narrowed by the token it was created with; the machine owner's hooks:
  everything), so the bus's read check, audience and redaction (section 12) decide what reaches it, exactly as
  for `zm events`. Any `dev.zm.authz.*`, `dev.zm.project.*` or `dev.zm.team.*` event rebuilds every hook's
  subscription (and every 5 minutes, for token expiry); a revoked or expired token, or no grant left, stops the
  hook until that changes. Each delivery is checked again at send time (`skipped` when the creator can no longer
  read its project/area). `--events` takes type prefixes (`work.*`, `mail.sent`, `dev.zm.session.`); `-P p/area`
  narrows to that area. A new hook gets events published after it was created, none before.
- **Durable, never blocking.** The follower copies matching events into `webhook_queue` (registry.db) in the
  same transaction that moves the hook's cursor (the last event id handled), so a restart neither loses nor
  repeats an event; a follower that falls behind is cut off by the bus (section 12) and catches up from the event
  log at its cursor, page by page, each page bounded by the same page read unfiltered (the bus filters audience
  after its SQL limit, so a short filtered page is not the end of the log). On start, every active hook resumes from its cursor (events published while the module was
  not running are caught up, within the log's retention). A queue holds at most 5000 deliveries per hook; the
  oldest beyond that are dropped and logged (`dropped`).
- **Delivery.** Up to 8 at once, one per hook at a time, in event order. `POST <url>` with the event's
  CloudEvents JSON as the body, signed per [Standard Webhooks](https://www.standardwebhooks.com) with its
  reference library: `webhook-id` (the queue row's id, the same across retries: receivers dedupe on it),
  `webhook-timestamp` (Unix seconds of this attempt) and `webhook-signature: v1,<base64 HMAC-SHA256(key,
  "<id>.<timestamp>.<body>")>`. The key follows the secret's form: `whsec_` + valid standard base64 (every
  secret the daemon makes from 2026-11-01) keys on the decoded bytes, so any Standard Webhooks library takes the
  secret as given; any other secret (a caller's own, or a daemon-made one from before 2026-11-01, which was
  URL-safe base64 and no library decodes) keys on its bytes as given (`NewWebhookRaw`). To move an older hook to a
  library-ready secret, `zm webhook rm` and `add` it again. `X-Hub-Signature-256` and the deprecated header always
  key on the whole secret string. Also
  GitHub's `X-Hub-Signature-256: sha256=<hex HMAC-SHA256(secret, body)>` for existing GitHub receivers, and
  `Zm-Webhook-Id` (the hook), `Zm-Webhook-Delivery` (= `webhook-id`), `Zm-Webhook-Event`, `Zm-Webhook-Timestamp`.
  Receivers verify in constant time and refuse a timestamp more than 5 minutes off (`webhooks.VerifyStd` does
  both); the timestamped signature is the replay defence. **Deprecated:** `Zm-Webhook-Signature:
  sha256=<hex HMAC-SHA256(secret, "<timestamp>.<body>")>` is still sent until 2027-05-01 and then dropped; move
  receivers to `webhook-signature`. The secret is the caller's (`--secret-file`, 16 to 512 bytes) or made by the
  daemon (`whsec_` + standard base64 of 32 random bytes, shown once); it is kept in registry.db (it must sign) and
  never returned again. 2xx is delivered; anything else, a timeout (10 s) or a refused dial is retried after
  10 s, 30 s, 1 min, 5 min, 15 min, 30 min, 1 h, 2 h, 4 h, 8 h (+ up to 20% jitter; the last repeats), at most
  12 attempts, then `failed`. Redirects are never followed. A 410 disables the hook at once.
- **Auto-disable.** A hook whose failing streak (no success in between) is 24 h old and holds at least 10 failed
  attempts is disabled: `dev.zm.webhook.disabled` on the machine log (owner only), the reason in `zm webhook ls`.
  Its queue is kept; `zm webhook enable` resumes it from now (events published while it was off are skipped,
  what was queued is delivered). `zm webhook test` sends one signed `dev.zm.webhook.ping` at once (logged, never
  retried), also to a disabled hook. The log keeps 500 attempts per hook: `delivered | retry | failed | dropped |
  skipped | ping`, with the HTTP status and the receiver's first 200 printable bytes.
- **SSRF.** Without `--allow-private` a URL must be `https`, carry no credentials, and name a public host: an IP
  literal outside loopback, RFC 1918, link-local (cloud metadata 169.254.169.254), CGNAT (tailnet), unique-local
  (fd00:ec2::254), multicast, reserved and documentation ranges and the IPv6 forms embedding IPv4 (mapped,
  NAT64, 6to4, Teredo); names such as `localhost`, `*.local`, `*.internal`, `*.ts.net`, bare names and numeric
  TLDs are refused at creation. Every attempt re-checks the URL, uses no proxy, and dials through a check of the
  resolved address (so DNS rebinding cannot reach a private address).

### 18.2 GitHub branch and PR linking

- `zm github link -P <project> --repo owner/name [--repo ...] [--prefix harbor] [--source bd] [--match
  title,branch,body] [--gate PATH] [--on-open claimed|in_review|none] [--on-merge done|in_review|none] [--on-close
  open|none] [--poll 1m] [--bin gh] [--host H] [--token-file F] [--secret-file F]` creates or replaces a project's
  link; `zm github ls|unlink <project>|sync
  <project>`. All `local` (the linker runs `gh` with the owner's credentials and mints a token), except the
  receiver.
- **Which items.** In the PR texts `--match` names (any of `title`, `branch`, `body`; default all three; a gate
  answering `match` replaces this, 18.3): `<source>:<ref>` written out (`[bd:harbor-abc12]`,
  `local:k3x9qa`) for the link's sources and `local`, and bare ids `<prefix>-<ref>` (`harbor-abc12`,
  `harbor-abc12.3`; case folded) looked up as `<source>:<prefix>-<ref>` in each source. Prefixes default to the
  project slug (the issue prefix a tracker's setup gets), sources to every tracker of the project. At most 10
  candidates per PR; a candidate that is not an item (or not one the linker may see) is ignored.
- **What moves.** On the first sight of a PR naming an item: a `pr` link to the PR. PR opened (or reopened):
  `--on-open` (default `claimed`, "in progress") when the item is `open` and nobody holds it; a local item, which
  only a lease can claim, goes to `in_review` instead; a held item keeps its status and the comment says so.
  Merged: `--on-merge` (default `done`). Closed unmerged: `--on-close` (default `open`) when the linker saw the PR
  open, the item is `claimed` or `in_review` and nobody holds it. Each state change of a PR comments on the item
  with the PR's link (merged: "Closed by merged PR owner/name#N: <url>", the close reason; with `--on-merge none`
  "PR owner/name#N merged: <url> (status left as is ...)", never "Closed by"). A status the tracker
  refuses (conflict) is named in the comment instead of retried.
- **As whom.** The linker writes only through the work API (`/v0/work/{id}`, `/status`, `/comment`, `/link`),
  in-process, as identity `zm-github` with a `member` token capped to the linked project (its secret in
  `$ZM_HOME/secrets/ghlink/<project ULID>.token`, 0600). So the work module's authz, events and tracker
  write-through apply unchanged, an item in another project is never touched, and native issues plug in through
  the same routes. `unlink` revokes the token and the grant. Run the link where the project's items are written
  (a solo project, or a team project's authority).
- **Inputs.** Both feed one durable inbox (`ghlink_inbox`, registry.db) applied in arrival order by one worker;
  an entry that fails (the tracker did not answer) is retried after 30 s, doubling, 8 times, then dropped and
  named in `last_error`. Snapshots of a PR apply in GitHub's `updated_at` order: one not newer than the last
  applied changes nothing, so replays, redeliveries and receiver/poller overlap are harmless; per item, the PR state
  last applied is kept so no comment repeats.
  - **Receiver** (`POST /v0/github/hook`, public; on any listener that reaches the daemon, e.g. the tailnet with
    Funnel or a reverse proxy): GitHub's `pull_request` deliveries, content type `application/json`, at most 5 MiB.
    `X-Hub-Signature-256` is checked against every link's secret (`ghsec_...` made at link time, shown once, or
    `--secret-file`) in constant time before the body is parsed; a missing or malformed signature, or one that
    matches no link's secret, is the same `401` whatever repo the body names (a verified delivery for a repo its
    link does not cover is accepted and ignored). A verified delivery is queued for each link that names its repo, in the same transaction as its
    `X-GitHub-Delivery` id; an id seen before (kept 7 days) is dropped. `ping` answers 200, other events 202 ignored.
  - **Polling** (`--poll D`, at least 15 s; `zm github sync` polls once now): `gh api --method GET
    repos/<owner>/<name>/pulls?state=all&sort=updated&direction=desc&per_page=50&page=N` per repo, newest first
    until a PR is older than the repo's cursor (with a 2-minute overlap), at most 10 pages; the first poll of a
    repo looks back 24 h. No inbound port. `gh` runs read-only with the environment allowlist `PATH, HOME, LANG,
    TERM, TZ`, `GH_HOST` from `--host` and `GH_TOKEN` only from `--token-file`; never a write to GitHub.
    A project moves off this machine when its authority is elsewhere: this machine joins it, as a replica under
    the same project ULID, and its old copy (renamed) stays in the registry. From then on the link is not
    this machine's: it supplies no `gh`, host or token, is not listed, polled, applied or checked against webhook
    signatures, and `zm github link` for the project is refused (conflict). The daemon deletes it (revoking the
    linker's token and grant, as `zm github unlink` does, held gate decisions included) at the next start, list,
    GitHub read or poller turn, so it cannot come back when the replica is left.
  - **CI results** (polling links): each poll also reads `repos/<owner>/<name>/actions/runs?status=completed&per_page=50`
    since the repo's CI cursor (2-minute overlap; a repo's first pass starts now, never mailing old results). For each
    open PR head among them that the link has tied to items (18.2), once the newest run of every workflow for that head
    has finished and it is still the PR's head, its open items are handled once per result. A machine notice never
    reaches a person: **green** mails no one; the project's daily summary counts it.
    **Red or cancelled** goes to the agents its items reach: one `needs=agent` note per recipient, where items whose
    mail reaches the same agent now (a session holding it, else the session or ended session's group mailbox that made
    it) share one note, addressed to the first and ending `Also names <ids>.`, so mail routing takes it to the holder:
    `CI red|cancelled for <repo>#<n> "<title>" at <sha8>: <workflow>: <conclusion> (<failing jobs, at most 5>) <run
    url>; ...`, marked as GitHub's data, not instructions. Items no agent has (nobody holds them, or a person holds or
    made them, so mail would reach `human:<name>`), and an item whose note is refused because that changed since, share
    one `needs=agent` note to the linker's machine's group mailbox `machine:<m>` (ref the first, so an idle managed
    session of the project there wakes), ending `No agent holds <ids>: whoever here can fix it, take it.` It carries no
    `client_request_id` (retry-safe mail cannot reach a holder on another machine of a non-team project); the linker
    records each handled item per result (project, repo, PR, item, outcome digest, verdict, where it went), so a
    repeated pass sends nothing and a re-run with a new outcome is handled again. If the item a shared note is
    addressed to is gone or hidden at send time, the others in its group are mailed one by one. Push runs, PRs that
    name no item, superseded heads and closed PRs send nothing; an item that is gone or hidden is skipped, said on
    the link.

  - **PR lifecycle events**: applying a snapshot, from the receiver or the poller alike, publishes
    in the linked project's log (so `zm events` and `zm webhook` carry them under the read rules of work events;
    `--events github.pr.` subscribes): `dev.zm.github.pr.ready` when an open PR that is not a draft is first seen,
    a draft is marked ready, or a PR is reopened; `.head_moved` for a new head SHA on a ready open PR; `.closed`
    when it is merged or closed unmerged. The CI pass (polling links) publishes `.checks_settled` when every
    workflow's newest run for an open PR's head has finished, for any PR, whether or not it names an item. Data
    (GitHubPREvent): `repo, number, head_sha, base, draft, title, author` (GitHub login), `items` (the work ids the
    linker has tied it to, maybe none), `html_url, state` (open, merged or closed), `prev_head_sha` (head_moved),
    `conclusion` (checks_settled: success, failure or cancelled). Each is published once per (project, repo, PR,
    head SHA, type), plus the CI outcome for checks_settled (a re-run with a new outcome is published again), so a
    poll after a delivery, a retry or a replay fires nothing twice. Events wait for a gate's match like the item
    writes do, never for the tracker.

### 18.3 Gates: a project's own rule for what a PR may do

A link may name a **gate** (`--gate PATH`; re-running `link` without `--gate` keeps it, `--gate none` removes it and
drops the decisions it holds; `--match` likewise keeps, `--match all` resets): an executable the machine owner trusts
(the link routes are `local`)
that decides, for each PR and each item it names, whether the linker may act. zm stays generic; a project's policy
(evidence a deploy shipped, owner-only items, reverts) lives in its gate. Without a gate nothing changes (18.2).

- **Protocol** (one-shot, like section 11): one JSON request on stdin, one JSON response on stdout, exit 0, within
  the timeout (describe's `timeout_seconds`, default 60, at most 300; the process group is killed after it). Output
  over 1 MiB, a non-zero exit, invalid JSON or `{"ok": false}` is an error. Its output is data: only the fields below
  are read, and a note is plain text of at most 2,000 bytes (longer is cut).
  - `describe` (args none) -> `{"zm": "zm/v0", "name", "version", "verbs": ["describe", "decide", "match"?],
    "timeout_seconds"?, "recheck_seconds"? (default 600, at least 60), "retention_seconds"? (default 14 days, at most
    90)}`. `zm github link` runs it and refuses a gate that fails it; the daemon runs it again at start and when the
    file changes.
  - `match` (optional) `{"pr": PR}` -> `{"ids": ["harbor-abc12", "bd:harbor-x.2", ...]}`: the items the PR names, by
    the project's own convention (bare ids are looked up in each source, as in 18.2; at most 10). Without `match`,
    18.2's matcher over `--match` decides.
  - `decide` `{"pr": PR, "item": Item, "proposed": {"to": "<zm status>" | "", "comment": "..."}, "first_asked":
    RFC 3339, "attempt": n, "final": bool}` -> `{"decision": "apply" | "note" | "wait" | "skip", "note"?: "..."}`.
    `proposed.to` is the status 18.2 would set, `""` when it would only comment (and link).
  - PR = `{repo, number, title, body, branch, url, state: open|merged|closed, draft, labels: [..], merged_at?,
    merge_sha?, head_sha?, base?, updated}` (base: the branch it merges into). Item = `{id, title, status, owner, source, labels, extra, last_write?}`, as the
    linker reads it through `/v0/work`; `last_write` is when the linker last wrote to this item for this PR (so a
    gate can tell its own write from a person's later touch).
- **Asked before every write.** With a gate, the linker asks `decide` before it writes anything to an item for a
  PR state (link, comment, status): `apply` does what 18.2 proposes (the gate's note, if any, is the comment);
  `note` comments (the note, else the proposed comment) and links, and moves nothing; `skip` writes nothing. All
  three record the PR state for the item, so it is asked once per state. `wait` keeps it pending in the linker's
  own table (`ghlink_waits`): asked again every `recheck_seconds` with the PR as last seen (a newer snapshot of the
  PR replaces it) until `retention_seconds` after the first ask; then once more with `final: true`, and a `wait`
  then gives up with one comment ("not moved: <gate> kept it waiting <d>; decide by hand"). A wait's note is
  commented once per distinct text. A held decision is asked again with the PR as last seen; a newer snapshot of
  the PR replaces it (a title edited after the merge is not re-read until GitHub reports the PR again). **A gate
  error or timeout is a wait** (named in `last_error`): the linker never moves an item on doubt. A gate whose
  describe fails (missing, changed and broken) makes every decide a wait. An error is not a verdict: past the
  gate's retention the decision is still held, up to 90 days, then one comment says the gate did not answer.
- **Environment** (allowlist): `PATH, HOME, LANG, TERM, TZ`, `GH_HOST` and `GH_TOKEN` as for polling, `ZM_HOME` (this
  daemon's, so a `zm` or `bd` it runs reaches this daemon as the machine owner) and `ZM_NO_AUTOSTART=1`. The gate
  fetches what else it needs itself (changed files, Actions runs, the item's detail); the linker passes no secret
  beyond `GH_TOKEN`. The gate runs as the machine's owner: point `--gate` at a fixed checkout, not one that moves.
- **Conformance**: `internal/ghlink/gate` holds the interface (`Gate`: Describe, Match, Decide), the process
  driver and `gate.Conform`, the protocol checks every driver must pass (describe's shape and bounds, a valid
  decision for each PR state and on a final ask, valid match ids or no match verb, an unknown verb refused). It
  proves the protocol, not a project's policy (that is the gate's own tests). Never moving on a gate's error is
  the linker's property, tested there. A second driver (an HTTP gate) would plug in behind the same interface.

### 18.4 The Mezze app door: a public URL per webhook task

A Mezze app webhook task (GitHub, Sentry, a `report` job) gets a stable public URL at its team project's authority,
through the authority's tunnel host, whichever linked Mezze app environment (a Mac, the headless automations home) owns
it. Module `internal/door`; routes in API.md "Mezze app door". The Mezze app keeps its own verification: zm never holds a task's
HMAC secret.

- **Where.** At a team project's authority. Every control route takes `project`; on a member's socket it goes to
  the authority (the team proxy), so the caller's machine is the one its node-bound token names, or the authority
  itself on its own socket. Nothing else registers a task (a session or tailnet token is refused). Reads need
  `read`, writes `operate`, on the whole project.
- **On and off.** `PUT /v0/door` `{on}` turns the door on or off for the caller's machine in the project. Off:
  every URL of that machine's tasks answers 404 and nothing is queued (its queue is dropped). `GET /v0/door` says
  whether it is on, the public base URL (`door.public_url`, else `tunnel.url`), and `available`: the authority
  serves the door (`door.listen` set). Without it the routes still answer, URLs are empty.
- **Tasks.** `PUT /v0/door/tasks/{task}` `{target, label}` registers or updates one of the caller machine's tasks:
  `task` is the Mezze app's id (`[A-Za-z0-9._-]{1,100}`), `target` the task's own hook URL on that machine (loopback HTTP, path
  under `/api/hooks/`, at most 1000 bytes). Its URL is `<base>/v0/door/hook/<token>`, `token` 32 random bytes
  (`dh_` + base64url), kept across updates. `POST .../rotate` mints a new one; the old answers 404 at once.
  `DELETE` drops the task and its queue. `GET /v0/door/tasks` lists the caller machine's tasks with URLs. At most
  500 tasks per machine and project.
- **Deliveries in.** `POST /v0/door/hook/{token}` is public, served on `door.listen` (loopback; `zm tunnel setup`
  routes `^/v0/door/hook/` to it unsigned, the rest of the host stays signed) and on the API listeners. A missing,
  rotated or switched-off token is 404, all alike. The body is at most 1 MiB (413). Headers go on as
  they came except hop-by-hop, `Host`, `Content-Length`, `Cookie`, `Cf-*`, `Cdn-Loop`, `X-Forwarded-*`,
  `X-Real-Ip` and `Zm-*` (at most 64 headers, 32 KiB). The delivery id is the first of `X-GitHub-Delivery`,
  `Webhook-Id`, `Svix-Id`, `Request-Id` (Sentry), `X-Request-Id` (`[A-Za-z0-9._:-]{1,100}`): a repeated id for the task
  is answered 202 and not queued again. Every accepted delivery is queued on disk (it survives a restart) and
  answered `202 {"queued": true}`.
- **Deliveries out.** A worker sends each machine's queue in order through `orchcore.DoorRelay` (the team module): to
  the authority's own machine at once, to another as a `door_delivery` machine op (SPEC 8 "Machine choice"), taken
  by its held-open poll or heartbeat answer; the body stays in memory, out of `team_ops`. The machine checks the
  target again (loopback, `/api/hooks/`), POSTs the body and headers there (30 s, no redirects) and reports the
  status. 2xx is `delivered`; any other 4xx but 408 and 429 is `refused` (the Mezze app's signature check or filter said no;
  never retried); anything else (a 5xx, a refused connection, a machine not heard from in 60 s, no answer) waits
  and retries, 10 s doubling to 10 min, the machine's later deliveries behind it. One still queued 72 h after it
  came is `dropped`. A delivery may reach the Mezze app twice (an answer lost after the POST); the Mezze app's own replay dedupe drops
  it.
- **Last deliveries.** `GET /v0/door/tasks/{task}/deliveries?limit=` (1-100, default 20): newest first, with the
  delivery id, `X-GitHub-Event`/`Sentry-Hook-Resource` as `event`, state (`queued`, `delivered`, `refused`,
  `dropped`), attempts, the last status or error, and times; never the body. A body is blanked once its delivery
  ends; each task keeps its last 100 rows, none older than 7 days.
- **Copies.** Queued bodies are third-party text: a redaction scrubs them as it does outgoing webhooks'. Tokens are
  blanked in a backup copy: a restored home re-mints (`rotate`) each task's URL.
- **Driver.** `orchcore.DoorRelay` (`RelayDoor`) is the interface; the team module's machine op its one driver; a
  second (a push to a Mezze app that serves its own public URL) would plug in behind it. The door's tests drive it with a
  fake relay; `internal/team` tests the op.

### 18.5 A project's repo list

The GitHub repositories automations scoped to a project act on (the Mezze app's `scope`, gates). `zm github ls -P <project>
[--json]` reads it (`GET /v0/github/repo-list`); `zm github repos -P <project> [--set a/b,c/d] [--add r] [--rm r]
[--reset]` changes it (`PUT`; `--add`/`--rm` read, then write the whole list: the later of two writes wins).

- **Project config, not a machine setting.** It lives at the project's authority (`ghlink_repo_lists`,
  registry.db). On a member the team proxy forwards both routes there, so every member machine reads the same list
  and an admin may change it from any of them. Unlike 18.2's link it runs nothing: setting it moves no item, polls
  nothing and maps no repository for `GET /v0/github/data`. Unlike the project's `remotes` (machine settings that
  resolve a checkout to a project) a network admin may set it.
- **Who may.** Read: read on the whole project. Write: admin on the whole project. An area name, a hidden project
  and a missing one answer alike (`not_found`; an area is 400).
- **What it holds.** At most 200 `owner/name`, lower case, deduplicated, in the order given (a github.com URL or
  `git@github.com:` remote is accepted). Never set (or `--reset`), it answers the project's and then its areas'
  declared github.com remotes with `source: "remotes"`; once set, `source: "list"` (an empty list is a list).

### 18.6 Automation records

A Mezze app automation's definition (what fires, when, with which prompt) is a record at its project's authority; the Mezze app
install that holds the record's home fires it, from a copy it keeps. Module `internal/automations`; routes in
API.md "Automations"; the decision is docs/decisions/2026-11-15-automations-as-zm-records.md. Runs, digests,
webhook tokens and signing secrets stay in the Mezze app. Brick: interface `store.Store`
(`internal/automations/store`), driver `store/sqlite` (tables in the project's `project.db`), second driver
Postgres (named, not built), conformance `store/storetest`.

- **Where.** Only at the project's authority, like native issues: no replica copy. On a member's socket the team
  proxy sends every route there; an id names no project, so an item is asked of each joined project in turn while
  it answers 404. An unreachable authority fails a member's read as an issue read fails.
- **The record.** `id` (ULID), `project` (slug) and `area` (may be empty), `kind`, `name` (one line, at most 200
  bytes, not unique), `enabled` (the owner's intent), `resume_rev` (the rev of the last approved resume; nothing
  else sets it), `owner` (a bead or address, at most 200 bytes), `home` (the machine whose Mezze app fires it; never
  changed: a move is a copy, then a delete), `task_id` (the task's id on the home: default `automation:<id>`,
  never changed, at most 200 printable characters, unique per project and home, a tombstone's included), `fields`
  (a JSON object, the automation file's frontmatter as written, at most 32 KiB), `body` (the prompt, at most 64
  KiB), `settings`, `rev`, `created`, `updated`, `updated_by`, `deleted` (a tombstone's time) and `health`.
  `name`, `enabled`, `owner` and `settings` live only at the top level: inside `fields` they are refused. zm does
  not compile schedules or parse YAML; a record the home cannot compile is kept, and its health says `unapplied`.
- **Kinds.** `kind` is `automation` (the default); each kind has one validator of `fields`, and an unknown kind
  is refused. `automation`'s refuses `fields.on` that is not an object and a signing secret in
  `fields.on.webhook` or its `signature` map (`secret`, `secret-ref`, `secretRef`, `secret_ref`): a webhook's
  secret and URL token stay in the Mezze app and the door (18.4). Stage 4 adds `kind: loop`.
- **Typed settings.** At most 50 keys (`[A-Za-z0-9_.-]{1,64}`), each `{type, value, options?, help?}`: `type`
  is `string`, `number`, `boolean` or `enum` (whose `options`, 1 to 50, list its values); `value` is checked
  against its type; no secret type. The `{key: value}` map as Go's `json.Marshal` writes it is at most 3,500
  bytes: it reaches a script as one argument.
- **Revisions.** Each definition write (create, patch, delete) raises `rev` by one and keeps a revision: the
  definition it left, who and when, and the write id that made it (a member's `Zm-Write-Id`, or an approval's
  ask id), never shown. A write repeated with one write id makes one revision and answers the first one's
  result; a write id another record's revision holds is `409`. A write that changes nothing makes no revision.
  `GET .../history` answers the last 50 revisions within 1 MiB per record, newest first; the oldest go first,
  the newest is always kept.
- **Who reads.** Read on the record's project and area. A hidden record answers exactly like a missing one,
  for one record and for `?project=`, directly and through a member. A list needs `?project=` and takes
  `?home=` (then the answer names the home's claim `holder`), `?kind=` and `?deleted=1` (tombstones too); it
  answers envelopes (no `fields`, `body` or `settings`), by id, at most 5,000 (`truncated`). `updated_by`, and
  `owner` when it is an address, are redacted for a reader who may not see them.
- **Who writes: the home rule.** A definition is code that runs with its home's credentials. Every write route
  takes a call only from the daemon of the record's home: that machine's socket owner (no token), or at the
  authority that machine's node-bound token (the door's rule, 18.4); with operate on the whole project; marked
  checked by the daemon that took the socket call, which refuses a caller inside a registered agent session (the
  origin gate of SPEC 16 "Key approvers"; unmarked is refused: a member's zm too old is named, this socket's gate
  missing fails closed) and acting as no session (`Zm-As` a session is `403`); and naming (`install`) the Mezze app
  install that holds the home's claim (else `409`). A definition write also needs that claim's `store` to be `zm`
  (else `409`). For a record: not readable is `404`, readable but not the home `403`. Anything else (the
  authority's socket owner when it is not the home, a minted token, an agent session, another machine) cannot
  write a definition; the other way, a proposal a person approves with a signed answer, is planned
  work, not built. It is a guard against mistakes, not a boundary: any process of the home's OS user writes
  as the home.
- **Create.** `POST /v0/automations` `{project, area?, kind?, home, task_id?, install, name, enabled?, owner?,
  fields, body, settings?}` (`project` falls back to `?project=`). The same project, home and `task_id` again is
  `409` naming the record.
- **Patch.** `PATCH /v0/automations/{id}` `{rev, install, name?, enabled?, owner?, body?, fields?, settings?,
  replace_fields?}`: `rev` is the revision the change was made from (another is `409`, "changed since"); a key
  left out is unchanged; `name`, `enabled`, `owner` (`null` clears it) and `body` are replaced; `fields` and
  `settings` take a JSON merge patch (RFC 7396), and with `replace_fields: true` `fields` is the whole new value
  (a merge patch cannot set a `null`). Any other key is `400`. A tombstone is `409`.
- **Delete.** `DELETE /v0/automations/{id}?install=[&rev=]` makes a tombstone (a new revision); `rev`, when named,
  is the revision the delete was made from (another is `409`); again, it answers the tombstone. A tombstone is
  kept 30 days from the delete, or from the home's health post that applied it (`applied_rev` at its rev),
  whichever is later; then the record, its history and health are dropped and its `task_id` is free. The home
  deletes its task on a tombstone; a record that is merely absent never deletes one.
- **Health.** `PUT /v0/automations/{id}/health` (the home rule; any store): `{install, state, applied_rev,
  apply_error?, failure_streak?, secret?, hook_changed?, last_run_at?, last_run_status?, next_run_at?, as_of}`,
  `state` one of `healthy`, `degraded`, `paused`, `unapplied`, `conflict`; `secret` one of `none`, `set`,
  `missing`; `apply_error` at most 300 bytes; `last_run_status` a status word. Never a run's error text,
  output or prompt. A tombstone takes it too (the last post). It makes no revision, does not change `rev`, and
  is answered on the record as `health` (with `received`).
- **Home claims.** One install per project and home fires its jobs. `PUT /v0/automation-homes/{home}?project=`
  `{install, store, path}` (`store` `zm` or `z3`; `path` the install's userdata path) takes the home rule without
  the claim. The first claim holds the home; the holder's repeat updates its store and path and keeps its first
  claim time and the write id of the change that made it holder. Another install is `409` (the answer's data is
  the claim) and is recorded as the home's one `wanted` install with its store, path and first refused claim: a
  different install's refusal replaces it, the same install's repeat updates its store and path and keeps its
  first time, and a change of holder clears it. `GET /v0/automation-homes?project=` (read on the whole project)
  lists each home's holder, store, path, claim time, last health post and wanted install. A change of holder is a
  compare-and-set (the expected holder, the wanted install, a write id; a `store: z3` wanted install is
  refused) that only an approved takeover (P-1) calls; repeated with its write id it makes one change. The claim
  routes sit outside `/v0/automations/` because `/v0/automations/{id}/health` and a claim under it would clash.
- **Events.** `dev.zm.automation.created`, `.updated` and `.deleted` on each definition write, and
  `dev.zm.automation.health` when the posted `state` changes; data `{project, area, id, rev, state?}`, never the
  definition. The home polls the list (by `rev`) in the first build.
- **Copies.** A redaction scrubs names, owners, bodies, fields, settings, history and apply errors (rule 5);
  it does not move `rev`, so the home keeps its own copy. A project export keeps the records (they hold no
  secret and nothing personal), and the off-site home sets carry them in `project.db`.
- **Speed.** Every route under the 16 ms p95 budget at the authority with 1,000 records in one project
  (targets: list 6 ms, show 3 ms, write 15 ms, health 4 ms; the bench rows are P-3's). No MCP tool.

## Native issues (phase 1 core; built early, sync waits for round C)

zm's own issue tracker, at parity with how agents use beads, so beads becomes an import source instead of the hot
path (option B). Section 10's work items and section 11's
adapters are unchanged; native issues reach `zm work` through the built-in native adapter (below).

- **Number**: every issue has a per-project sequence `number` (1, 2, ...; Linear's `Issue.number`), assigned at
  creation in commit order and never reused; migration 0002 and the importer number existing issues by creation time.
  The id stays the identifier: the number is authority-local, kept for bd `--json`
  and never shown as an identifier on zm's own surfaces. `GET /v0/issues?number=N` finds one.
- **Model** (`orchapi.Issue`; beads' field names): id `<prefix>-<handle>` with children `<parent>.<N>` (bd's shape;
  child numbers come from a per-parent counter and are never reused). A new handle is random (crypto/rand) over
  `a-z 2-9` without the look-alikes `0 1 i l o` (31 symbols), has at least one letter (it never looks like a
  number), contains no blocked word, and is the shortest length in 3..8 at which one new mint hits an existing
  handle with probability at most 1 in 1,000 (a hit is drawn again). Ids minted before (base36 of a sha256) stay
  valid and are never renumbered; handles are lower case and lookups fold case
  (`docs/decisions/identifiers-2026-11-01.md`, stage 1); title (one line, at most 500 bytes), description, design, acceptance criteria and notes (markdown, at most
  1 MiB each), status `open | in_progress | in_review | blocked | deferred | closed | pinned | hooked` with `zm_status`
  (in_progress and hooked = claimed, in_review = in_review, closed = done, the rest = open), priority 0-4, type
  (beads' aliases mapped), labels, assignee (a display name) and `claimed_by` (the zm address that claimed it),
  owner, estimate, due and defer times, external ref, spec id, metadata (a JSON object), created/updated/started/
  closed times, close reason and closing session, and `extra` (source columns zm does not model, kept verbatim).
- **Dependencies**: `{id} depends on {target}` with a type (`blocks` default, `conditional-blocks`, `parent-child`,
  `related`, `relates-to`, `discovered-from`, any other short word; `external:...` targets kept as text). One edge
  per pair; blocks edges may not form a cycle; a parent chain may not loop. **Blocked** (beads' `is_blocked`): not
  closed or pinned and either an open blocks/conditional-blocks target or a blocked parent; recomputed in the write's
  transaction for the issue and, transitively, its dependents and children. **Ready work**: open, not blocked, not
  pinned, not ephemeral, not deferred (nor its parent), and not an infrastructure type.
- **Claim** (`POST .../claim`, bd's `update --claim`): in one transaction, open and unassigned (or already the
  caller's) becomes in_progress with the caller as assignee and `claimed_by`; anything else is `conflict` naming the
  assignee. Two sessions of one identity do not share a claim (`claimed_by` differs). **A claim is a lease** when
  the project binds its native issues as a work source (below): the claim door (and so bd's `update --claim` and
  `ready --claim`) claims the issue's work id `<prefix>:<id>` through section 10, as `zm work claim`
  does: a TTL lease (`ttl_seconds`, default 3600) or, from a live session, a bound lease (section 10); claiming
  again renews; the answer carries the lease's `lease_fence`, and the door's writes take `fence` (section 10,
  "Fencing"; without the binding there is no lease, so any fence is stale);
  expiry releases the issue (in_progress goes back to open, unassigned). An issue made a moment before its claim is
  read through into the mirror first. Any write that ends the claim, native or through `zm work status`, drops the
  lease: a close, a status whose zm status is no longer claimed or in_review (in_progress, hooked and in_review keep
  it; open, blocked, deferred and pinned end it, and clear `claimed_by`), or another assignee. The issue keeps what bd keeps (its assignee),
  and a release arriving after the claim ended changes nothing. A renewal the issue refuses (closed meanwhile) drops
  the lease. In a team project the native claim is made at once, not on the write-back, and in the claimer's name.
  Binding native issues turns claims made without a lease into TTL leases (note `adopted`). **Unleased claims**:
  every 30 s the issue store's home (the authority, for a team project) puts back to open an
  in_progress or hooked issue that no lease row names and that has not changed for an hour (the default TTL): a
  hand-set status, a claim another assignee ended, a release that found nothing. It keeps its assignee (an
  assignee is not a claim) and gets a comment saying why; the write is refused if the issue changed meanwhile.
  Without the binding a claim is atomic with no deadline.
- **Orphaned**: when a session ends (locally, or a replica's through presence), every issue it filed
  (`created_by_addr`) that is still open with no assignee, not ephemeral or pinned, gets the label `orphaned`, as
  the system: a follow-up or handoff bead nobody took. `bd ready --label orphaned` is a supervisor's queue. A
  claim, an assignee, a close or another status takes the label off, and the add re-checks (a claim racing the end
  leaves no mark on a held issue). A best-effort hint: an event overflow skips
  ends, a claim the end itself releases may be marked or not, and a session that revives keeps its marks.
- **Comments: edit and delete** (`docs/decisions/identifiers-2026-11-01.md`, stage 2a): a comment's
  author is its identity, so any session of it may `PATCH /v0/issues/{id}/comments/{cid}` (`text`; `base` = the
  comment's last history id refuses an edit over a change it did not see, `conflict`). Its author, an admin of the
  issue's area, or the machine owner on the socket may `DELETE` it: a tombstone (`deleted`, `deleted_at`). Its
  text and `deleted_by` are shown only to the area's moderators, and its edits' texts in history too. A
  moderator's delete shows readers `a moderator`, in history and in `dev.zm.issue.comment_deleted`. Guests do
  neither. Edits keep every earlier text in history (`comment_edited`) and set `edited_at`; the bd door leaves
  tombstones out.
- **Comments: redact** (stage 2b): `POST /v0/issues/{id}/comments/{cid}/redact`, an admin of the issue's area or
  the machine owner on the socket, for a leaked secret, personal data or abuse; it cannot be undone. The comment
  and every history copy of its texts (its edits') become `[redacted]` and it is tombstoned; history and
  `dev.zm.issue.comment_redacted` say "a moderator". Every module then scrubs its own copies of those texts
  (`core.Scrub`): the work module's comment copies (a `zm work comment` on a native item is kept there too) and
  events filed under the issue, where only values equal to a text change (so a short text such as "fix" touches
  nothing else); and any event, queued webhook payload or team write record holding a text of 8 bytes or more,
  wherever it occurs. The touched databases are vacuumed and their write-ahead logs truncated (a busy checkpoint
  is a failure), so no free page or old frame keeps the text. If a scrub cannot finish, the answer is
  `unavailable` and zm retries every 30 s from memory (the texts are never written down, so a daemon restart
  ends the retries: redact again only scrubs the issue store then, and the operator is told so). The redaction list,
  written before the comment, lives in registry.db: an import into a reset store redacts the listed comments
  again, and so does `zm issues restore`. Backups: export snapshots are rewritten in place, sqlite copies replaced
  by a fresh one (17.9). Not reached: payloads already delivered to webhooks, replica copies (they purge on the
  event: part 2), backups outside zm's records, agents' transcripts. A replica whose copy held the comment takes
  its old text as the text (the event carries none): it scrubs its outbox rows, and its own modules scrub their
  copies (`core.Scrub` for the project there: session holds and scrubs, its event log), queued in the copy until
  they did. A replica whose copy never held the comment has nothing to match.
  A moved issue's comment lived at every earlier stop: the redaction lists and scrubs each stop a surviving move
  record names (either direction, any local store, so a round trip's erased redirect does not hide its middle),
  and a restore there finds the comment by its id when the row names another id it had there. The
  scrub runs on every source project, including ones the caller cannot read. Each stop's state is in registry.db
  (`issue_redaction_stops`, ids only, no text or hash of it), written with its row after the comment and the
  caller are checked: pending until a scrub holding the text finishes there, so a cut-off attempt cannot pass
  as clean. A stop zm cannot prove clean is a gap, answered `conflict`: one whose text is gone from every store
  (an older zm's scrub, or a cut-off attempt), a "moved" hop no record places, or a stop held while the
  comment existed that is remote or past 32 stops. A stop held the text if the issue was there between the
  comment's creation and the store's overwrite (its `comment_redacted` row's time, kept by a move; now when
  none), not the first attempt: a stop reached after a cut-off attempt counts. The conflict
  names only stops in projects the caller can read. A stop is clean only once its backups' purge succeeded too; until then it stays unsettled and the next
  attempt purges again. A gap is reported, never cleared automatically: once a log is pruned, zm cannot prove
  by id that a stop's other copies are gone, and it never searches for the text. Startup recovery logs gaps
  (ids only). An operator clears one gap by acknowledging it (`POST /v0/issues/{id}/comments/{cid}/ack-gap`,
  `zm issues ack-gap <id> <comment> <stop>`): whoever may redact the comment names the stop as their
  own conflict names it, after dealing with its copies by hand. That stop is marked clean, so a later redaction
  no longer reports it, and registry.db's `issue_redaction_acks` keeps who, when, the stop and the gap it was.
  It searches no text and deletes no backup. A stop in a project the caller cannot read answers like no gap; one
  in another project they read but do not moderate is refused.
  It also clears the false gap a crash between a good purge and the stop's clean mark leaves.
- **Attachments** (a decision's brief: the memo is the item's text, the images go here): `zm issues
  attach <item-id> <file> [--expires 14d]`, `zm issues attachments <item-id>` (metadata), `... get <item-id> <aid>
  [--out <file>|-]`, `... rm <item-id> <aid>`; `/v0/issues/{id}/attachments[/{aid}]` (API.md). PNG, JPEG or WebP,
  told by the bytes (never a name or a declared type; no SVG), or an HTML page (UTF-8, kept verbatim, at most
  1 MiB; a static mockup shown sandboxed: one self-contained page with inline CSS and data: images, where scripts and network do not run; never SVG, video or other types). Images are decoded and encoded again so no metadata
  survives (EXIF, GPS, XMP, ICC, text chunks; Go has no WebP encoder, so a WebP comes back a JPEG, or a PNG when it
  has transparency), at most 20 megapixels and about 1 MiB after re-encoding, at most 8 live per item. Each expires
  (set at attach: default 14 days, at most 90). Read is the item's read check; attaching is whoever may comment;
  removing is its attacher (any session of their identity) or an admin of the item's area. The bytes are served
  `nosniff`, `inline` and sandboxed. Metadata (the contract the Mezze app reads): `{id, type, size, sha256, created,
  created_by, expires}`; an item read (`GET /v0/issues/{id}`, `bd show --json`, the native work item's
  `extra.attachments`, whose `created_by` is empty because the mirror is shared) carries `attachments` when there
  are any. **Kept at the authority only**, in `projects/<ulid>/attachments.db` beside `issues.db`: no backup,
  offsite copy, restore, export, move or replica copy holds one (a moved item's attachments are removed, not
  moved). A team replica forwards every attachment call to the authority, like every issue call (the CLI
  re-encodes an image over 1 MiB first, so it fits a forwarded body). History rows `attached`,
  `attachment_removed`, `attachment_expired` carry the metadata, never bytes, so the change feed, the work mirror,
  `dev.zm.issue.updated` and webhooks see one come and go. Removal and expiry (a sweep every 5 minutes) are hard
  deletes: the row's pages are overwritten (`secure_delete`), freed to the file system (`auto_vacuum`) and the
  write-ahead log truncated. `zm ask new --work <id> --attach <file>` (repeatable; MCP `ask_new` `attach`) attaches the files to the
  item, then lists their metadata on the ask (`attachments`: `{id, type, size, sha256, created, expires}`, only
  those still live on the item); without `--work` it is refused, and if any attach fails no ask is created. The card stays decidable alone.
- **History**: every write records rows in the same transaction (`orchapi.IssueChange`: kind, actor name and zm
  address, changed fields old and new, area before and after, time). History ids are ULIDs minted under the one
  writer connection, so they are in commit order: the history is also the project's issue changelog. Imported beads
  events are history rows too (their source id keeps a re-import idempotent).
- **Moves** (`POST /v0/issues/move`, `zm issues move`): open issues move to another project on the same authority,
  admin on both. New id = the target's prefix + the old id without its prefix (children keep their numbers; the
  child counter comes along); a taken id, a closed issue, and a claimed one (in_progress, hooked, or a live lease)
  without `transfer_claims` are refused. With it the claim moves, and once the move ends the old lease ends and
  the holder's lease is made again on the new id. Comments keep their ids; history is copied with new ids in
  commit order (times, actors and source ids kept) plus a `moved` row on both sides; the home area is kept when
  the target has an area of that name. The old project keeps a redirect: every `/v0/issues/{id}` route on the old
  id answers for the new issue (`moved_from`; a chain of moves is followed up to 4 hops), or not_found when the
  caller cannot see it there (through a replica, a call on one issue that the named project answers 404 has its id
  resolved by a GET to the replica's other joined projects on the same authority, whose tokens see the new
  project, then goes out once to the one project that found it; a write is never fanned out; when no project finds it and
  one of them failed, the answer is that failure, not not_found). `GET /v0/work/{id}` (with `/holder`, `/comments`)
  and mail (`to`, `ref`) on the old work id follow it too, for a native tracker's id only, the mail stored in the
  issue's project under its new id (mail to `all` or `owners` stays put); other work writes (claim, comment, status)
  name the id the issue has now. The move takes the old row out of the mirror and publishes `dev.zm.work.moved`, which
  takes it out of a replica's copy on its next sync. A create
  naming a moved id is refused, and moving an issue back under an id ends that id's redirect. Edges between moved issues
  stay internal; an edge across projects is an external edge naming the other id, which a read resolves to that
  issue for a caller who sees it (else leaves out). It does not block, and the dependents list shows only edges
  from the issue's own project. A run holds both projects' writes (a hold waits for writes already under way, and is released however the run
  ends) and plans again under them (an issue closed or
  claimed since the dry run is refused); each issue then moves in three transactions: the copy (pending: it
  refuses writes), the redirect and removal, then done. A rerun finishes a half-moved issue (copying the original
  again) and reports moved ones as `already moved`. `dry_run` answers the plan, cross-project edges included.
  Restoring a backup of the source made before a move brings the moved issues back there with no redirect.
- **Areas and authz**: an issue's area is the project's area rules (section 3: work globs on its id, else labels),
  else the area it was filed in, recomputed when its labels or the rules change. Reads need `read` on that area.
  Writes need `write` there and a role above guest: **guests are read-only on issues** (a deliberate narrowing of
  section 4's guest row for this module), unless the project sets `guest_issues` (`zm project set --guest-issues`,
  admin): then its guests write issues (create, comment, claim, close, labels, deps) wherever they may write, still
  never project memories. Built for an outside collaborator's own project. A write that moves an issue (a label) needs `write` on both areas. Hidden
  answers like missing; edges, parents and counts leave out issues the caller cannot read, history shows them as
  `someone`, and a refusal names only what the caller sees (close: "blocked by [a] and 1 you cannot see"). `blocked`
  counts every blocker (a reader learns that an issue waits, not on what). Addresses are redacted as everywhere. Who wrote is always the caller's identity (bd's `--author` is
  ignored; bd's actor only labels a claim, below).
- **Store brick** (`internal/issues/store`): one interface, drivers per backend. **SQLite** (built): one file per
  project, `projects/<ulid>/issues.db`, WAL, one writer connection, four readers, a background PASSIVE checkpointer
  (commits never checkpoint), prepared statements cached per connection, an FTS5 index (title, description, notes;
  prefix indexes) and partial covering indexes for the default list and ready work. Postgres and Dolt drivers
  implement the same interface (`ProjectStore`): reads take the caller's visible areas, every write is one
  `Apply(Mutation)` whose `Authorize(before, after)` callback runs inside the transaction.
- **A zm work source** (binds on an empty project too: a store with no history lists to the lowest cursor):
  a `[[trackers]]` entry with adapter `native` (`zm tracker add native [--prefix P]`) is served in process by the issues module through section 11's verbs, so `zm work`, leases, the
  mirror, inflight and MCP see native issues like any tracker's items. Reads (list, get, changes) see
  every issue and the work module scopes them; writes (claim, release, comment, set_status) act as the caller
  of the request with the native rules (guests read-only), so a guest cannot claim a native issue through `zm work`
  either. `changes` is the history feed: a full listing by id in pages of 1,000, then the issues whose history moved
  past the cursor (a history row id). A native item's area is the store's; its extra carries the native status,
  priority, type, number and assignee. Writes through `/v0/issues` reach `zm work` at the next mirror sync (5 s).
  It does not list `link`: zm work keeps an item's links itself (it sends trackers URL links only, and a native
  issue's structure is its dependencies). Section 11's conformance suite passes against it in process.
- **bd shim output**: every issue object carries `number`, then `claimed_by` (the zm address holding the claim,
  redacted as everywhere; omitted when empty), as extra keys after bd's own keys (bd's keys keep their order).
- **Events**: `dev.zm.issue.created | updated | commented | comment_edited | comment_deleted` (issue without edges or parent; the change without issue
  references), `dev.zm.issue.moved` (`{id, gone}` in the area an issue left), `dev.zm.issue.imported` (deprecated: the import that
  published it was retired on 2026-11-10; older event logs still carry it). Webhooks
  consume these; they fetch the issue for its edges.
- **Replica hook (for round C, not built)**: `store.Feed`. `Changes(since)` returns history rows in commit order
  with `area_before`/`area_after` (an upsert to readers of the new area, a tombstone to readers of the old one only,
  as 17.6); `Snapshot` streams every issue from one read transaction with the last change id it saw. Replayed writes
  go through `Apply`: `UpdateIssueRequest.base` refuses a field changed after the replica's copy (`conflict`, no
  last-writer-wins), and a comment's client-minted ULID makes a replay a no-op. Round C adds the sync routes, the
  replica copy and the outbox on top; on a team replica native issues are proxied, with no copy (below).
- **On a team replica**: `/v0/issues*` from the replica's socket goes to the project's authority by the rules of 17.3's
  proxy, with the replica's node-bound token and the calling session in `Zm-As`, so `bd` and every other client on a
  member's machine keep talking to the local daemon, which follows `zm remotes`: no client names a host. A project
  named by `?project=` or `Zm-Project` goes to its authority. A call on an issue id goes here first, then to each
  authority while `404`. `POST /v0/issues` and the routes that take a project (`stats`, `labels`, `comments`,
  `memories`, `sql`, `setup`) go by where the caller is (17.3's write rule), else stay here. An id this machine's own
  project also holds is answered here, as with work items, so a local prefix should not match a joined project's.
  `GET /v0/issues/projects`
  merges, naming in `skipped` an authority that did not answer. `GET /v0/issues` naming no project merges each store's
  first page (`total` and `more` add up, no `next`: a caller that pages names the project). `POST /v0/issues/import`
  is never forwarded: it reads a directory on the machine it runs on. The authority decides everything, as for any
  node-bound token: a guest's replica sees its area, read-only, and a redaction needs the authority's rule (a replica's
  socket is its owner's, not the authority's). A claim is held by the session (bound for a managed one unless an
  explicit TTL is requested), and a write that ends the session's own claim (`bd close`, a status change, `zm work status`) is a release, not a lost lease
  (17.3). **There is no copy**: every call waits
  for the authority, the replica adds about 2 ms to a loopback round trip, and an authority that does not answer is an
  error (`unavailable`), never an empty list or `not_found`. `bd` then exits 3, and `bd prime` still exits 0. For 15 s
  after the replica's daemon starts, a call waits for its tailnet node to come up rather than failing. `CallTimeout`
  (30 s) bounds waiting for an answer and each silence in it, not the whole transfer, and a transfer that fails partway
  drops the caller's connection: a short answer never ends cleanly.
- **bd-compatible door** (`cmd/bd-zm`, installed as `bd`): `create, update (--claim), show, list, ready (--claim),
  search, children, close, reopen, comments [add], comment, note, dep add|remove|list, label add|remove, stats, count,
  blocked` with bd v1.0.5's flags and output; `--json` output is bd's field for field (objects with sorted keys and
  `schema_version`, arrays as they are). Checked against the live bd on 11.5k imported issues (DESIGN-NOTES).
  One deliberate difference: `create` files a P0 or P1 at P2, with a one-line hint on stderr, unless it has `--due`,
  `--assignee` or a `--deps blocks:<id>` link (urgency needs a reason; epics keep their priority).
  **Project**: bd works in one project, like its one database: `$ZM_PROJECT`, else the project the directory
  resolves to (section 5) if it has native issues, else the one `$BEADS_DIR`'s directory resolves to, else the only
  project with native issues the caller reads (bd on a machine reached one database from any directory), else the
  resolved project. `--project <slug>` (any verb) names it outright and beats `$ZM_PROJECT`. Lists, ready, search,
  count, stats, memories, export and sql cover it and a new issue lands in it (a child in its parent's project);
  `list` and `ready` cover every project the caller reads with `--all-projects` (refused with `--project` or
  `--parent`).
  **An id names its own project**: the verbs that take issue ids (`show, update, close, reopen, comments, comment,
  note, dep, label, children`, and `--parent`) find the project from the id's prefix, the longest prefix of a
  project the caller sees (as the daemon routes ids), and send that call naming it (`Zm-Project`, as `--project`
  does). So on a team replica it goes straight to that project's authority, one round trip, and `bd show
  sengi-abc12` works from a harbor checkout or under `ZM_PROJECT=harbor`; lists under `--parent` cover the parent's
  project. `--project` turns this off (the id is looked up there). A prefix two visible projects share is refused,
  naming them and the fix (`bd --project <slug> ...`). A prefix no visible project has is `not found` naming the
  prefixes the caller sees, after one fresh look (a project joined a moment ago is found); while an authority is
  unreachable such an id goes out unnamed and the daemon answers. **No dependency spans projects**: a dependency
  lives in one project's store, whose blocked and ready computation cannot follow another project's issue (held by
  another authority, maybe away), so `dep add` and `dep --blocks` across projects are refused before any call, with
  the alternative (`bd dep add <id> external:<other-id> --type related`: kept as text, never blocks; or a comment);
  `dep remove` across projects says no such dependency exists. An `external:` target is text: never completed or
  routed. **The prefix map** (`GET /v0/issues/projects`) is
  cached for 30 s in `$ZM_HOME/cache/bd-prefixes.json` (0600; keyed by one sha256 over the socket, `$ZM_HOST` and
  the token, never the token itself; never when an authority was skipped; a bare hash completes from it too): bd is a process per call, so the cache is a file,
  and a warm call adds no round trip; a cold one adds one, answered by the daemon from memory (a replica asks its
  authorities at once). `TestRouteRoundTrips` holds both counts. Measured (CI VM, smoke, `bd show`, process start
  included, p50 of 20): 4.72 ms naming `--project` (no routing), 5.09 ms routed with a warm map, 5.42 ms cold. `--db`/`BEADS_DB`, `--global`, writes under `--readonly` and verbs
  it lacks are refused, never ignored. Not carried: markdown batch create, `--watch`, `--as-of`, molecules, gates,
  wisps. Around the issue verbs:
  - **Claim label** (until lease-backed native claims name the holder): `update --claim` and `ready --claim` send
    bd's actor as the claim's assignee: `--actor`, then `$BEADS_ACTOR`, `$BD_ACTOR`, `$FARM_BD_ACTOR` (bd-guard
    passes it as `--actor`; an explicit actor takes no lane); then, inside an agent session (the call answers as a
    session: `$ZM_AS`, else the runtime's session id, never under `$ZM_TOKEN`), that session's address, or `<lane>@<machine>/<session8>` with `$ZM_LANE` (a sub-agent
    under the session; Claude Code gives a sub-agent's shell no id of its own, so its dispatcher sets it; a name
    outside the name grammar is dropped with a note); then git's `user.name`, then `$USER`; with none, the
    caller's name as before. `hub who`/`hub send <work item>` and the claim hooks route by it; older agents set a hub
    address (`ivo-mac/58565660`). A session's claim made under a label naming no session (a person's name:
    the `user.name` fallback before 2026-11-01) is taken over when that session (`claimed_by`) claims again
    under its own label or a lane's under it; a label naming another session, or another lane, still
    conflicts. It is a **routing label, not an identity**: `claimed_by` is the caller's zm address, authz is the
    caller's, and authors, comments and history actors stay the zm identity. `ClaimIssueRequest.assignee` carries
    it (at most 255 bytes, no control characters); a claim under another label on an issue claimed under one is a
    conflict naming it, even from the same address (scripts on the socket share one), checked before the work
    module renews a lease so a refused claim never releases the holder's (two such claims racing can still both
    pass that check; the store refuses the second, and the work module then drops the lease). An address's own
    unlabelled claim (`zm work claim`, MCP) takes the label. Under the native binding the label
    reaches the native adapter's claim through the request context. The label is advisory: it is never used for authz
    or as the lease's identity. The lease holder (`registry.leases.holder`, the caller's zm address) stays the
    identity. Identifier stage 3 (`docs/decisions/identifiers-2026-11-01.md`) will make the holder name
    a session, derive the label from it, and deprecate `ClaimIssueRequest.assignee` with a dated window.
  - **Search** (`search <words>`): ranked, so an agent can find an existing issue before filing one. Every word
    must match (the last also as a prefix); `GET /v0/issues?q=<words>&sort=rank` orders the matches by bm25
    over title, description and notes (weights 10, 1, 0.5), best first, each issue's `score` set (higher is
    better; a key after bd's in `--json`, a column in the text). Filters: `--status` (default every status but
    closed; `all` adds closed), `--label`, `--label-any`, `--type`, `--parent`, `--assignee`; default limit 10
    (`--limit 0`: every match), and the text says how many matched. The plan follows two counts, FTS matches
    and issues passing the filters, and the ranked pass stops at a deadline inside the 16 ms budget (11.7k
    issues); a query too common to rank in it (a common word over every status) answers `unranked` with the
    newest matches, no score, and `total` an upper bound (0: unknown), and bd says to add words or filters. A
    one-letter last word is not a prefix. `--sort`, or a query that is an
    issue id, searches bd's way (substring of title, id or external ref); a daemon without `sort=rank` gets
    the same with a note.
  - **Memories** (`remember [--key]`, `memories [word]`, `recall`, `forget`; beads' `kv.memory.*` config rows, which
    the import keeps): `GET /v0/issues/memories`, `PUT|DELETE /v0/issues/memories/{key}`. Project-level, so only a
    caller who reads (writes) every area of the project sees (changes) them; no history.
  - **`prime`**: bd's override files first (`./.beads/PRIME.md`, `$BEADS_DIR/PRIME.md`, the user config dir's
    `beads/PRIME.md`), else zm-native context with every memory (`--memories-only`, `--mcp`, `--hook-json`,
    `--export` as bd). It exits 0 with nothing on stderr even when zm is down (SessionStart hooks run it).
  - **`export`** (`-o`, `--all`, `--include-infra`, `--include-memories`, `--scrub`): bd's JSONL
    (`{"_type":"issue",...}` with labels, dependencies, comments and counts; memories as `{"_type":"memory"}`), in
    bd's order. Templates are never exported (zm lists leave them out).
  - **`sql`** (`--json`, `--csv`, table): `POST /v0/issues/sql`, one read-only SELECT (or WITH) statement over
    beads' tables and columns (`issues`, `dependencies` with `depends_on_issue_id`/`depends_on_external`, `labels`,
    `comments`, `events`, `config`, `child_counters`; plus zm's `number`, `area`, `parent`). The tables a query
    names are copied, in one read transaction, from a read-only attachment of the store into an in-memory database,
    which is then detached and made `query_only`: the query never sees the file, its path or the native columns zm
    redacts (session addresses). At most 50,000 rows, a 32 MiB answer, 4 MiB per value, 30 s, two queries at a
    time. DATETIME columns are UTC text `YYYY-MM-DD HH:MM:SS` inside the query (MySQL-style comparisons work) and
    `...T...Z` in the answer, as bd printed them. The dialect is SQLite's: MySQL-only syntax (`interval`, `now()`)
    fails, and `=` on text is case-sensitive. Raw SQL cannot be scoped to areas, so only a reader of the whole
    project may run it.
  - **No-ops with a note**: `dolt push|pull|commit|status`, `sync` ("zm-native: nothing to push; zm syncs").
    `dolt show` asks zm and prints `✓ Server connection OK` only when it answers (bd-guard greps for it).
    `version` and `--version`/`-V` print `bd version 1.0.5 (bd-zm: zm-native)` (no zm call, BEADS_DB ignored); `where` names the
    project and the daemon, and the authority of a joined project. `context [--json]` is bd's ContextInfo with the
    fields zm has (`bd_version`, `backend` zm-native, `database` = the project, `project_id`, `issue_prefix`,
    `server`, `sync_remote` = a joined project's authority; no Dolt or `.beads` fields), asked of zm each time: it
    fails when no project resolves or zm has no such project, and exits 3 when zm does not answer (embeadify's
    doctor needs both).
  - **Refused with a reason**: `config, init, import, backup, restore, migrate, doctor, prune, purge, gc, compact,
    cleanup, delete, edit, rename(-prefix), kv, mol, gate, mail, federation, branch, vc, diff, history, hooks, setup,
    upgrade`, other `dolt` verbs, and any verb bd-zm does not know.
- **Budget**: list, show, search, create and update through the daemon socket at most 16 ms p95 on 11.5k issues, and
  the same for reads and queued writes from a replica's copy. `make bench` measures it in CI on a store it generates
  (the `issue-*` and `replica-*` rows; `scripts/ceilings/ceilings.json`): `bench.yml` goes red when the same row is over
  in two main runs in a row.

## 20. Run targets (protocol first; reference drivers live in the harbor project today)

A run target lends one of three things for a while: a throwaway **database** (`kind: "db"`), a **run** of a named
script somewhere other than the caller's machine (`kind: "run"`), or a **box**: a VM kept up for its lease, which
its holder sends commands with `exec` (`kind: "box"`, the CI sandbox). Its driver is an executable `potluck-driver-<driver>`
(else `zm-run-<driver>`, its old name) speaking section 11's one-shot envelope (`{"potluck": "potluck/1", "zm":
"zm/v0", "verb", "id"?, "args", "config"}` on stdin: the protocol under both keys, a driver reads either; one
`{"ok": true, "result"}` or `{"ok": false, "error": {"code", "message"}}` on stdout, exit 0; human text on stderr
only; section 11's codes). `describe`, `status`, `renew`, `release` and `list` answer in 30 s; `acquire` answers
once the database exists or the run has started (never when the run ends), in 120 s. Serve mode is optional, as
in section 11.

A sandbox task (SPEC 8 "Where it runs") is not a run target: zm only places it on a machine with a task driver
(`sandbox_task_cmd`), which leases and drives the box itself; bindings, budgets and driver pins are not involved.

**Drivers are pinned** (potluck `DRIVERS.md`, `runtarget/pin.go`). The daemon runs a driver only
from the directory of its own program (symlinks resolved: where `make install`, `zm daemon update` and a release put
the drivers, beside `zm`) or `$ZM_HOME/drivers`, never from PATH; only when the file, any link to it and every
directory above them are owned by the daemon's user or root and not group- or other-writable (a sticky directory
above is allowed); and only while its sha256 is the one pinned in `$ZM_HOME/driver_pins.json`. A driver with no pin is
pinned on first use (the bind's `describe`, or the first call after upgrading from a build without pins; one log
line; at start, with run targets and no pins, one WARN). A changed, moved or missing file is refused, naming the
binding, the path, both hashes and `zm driver pin <driver>`, which pins the file there now (local; with a daemon
running, in the daemon's program directory, not the CLI's, saying so when they differ). A box fails safe on upgrade
(`zm daemon upgrade`, so `make install` and `zm daemon update`, whose staged build runs the swap):
- **Folder check, before anything moves.** When the build places drivers where the daemon will run them (its install
  directory, the daemon restarting from there or none running), that folder must pass Potluck's rule (checked with
  `runtarget.PinDriver` on a stand-in driver). If not, it takes group and other write off each directory from there up
  to `$HOME` (never above, only directories the user owns; said on stderr) and checks again; still unsafe, it is a
  preflight problem: nothing is placed, the old build keeps running, and the swapper turns the hold switch on
  (`update.hold`, which every updater reads before it fetches; machine numbers show it as `zm.held`) with the reason
  and the fix, noting in `update-autohold.json` that the hold is its own. The release is not marked failed (no veto for
  other machines). An updater of this build lifts that hold after 6 hours, or when the channel has a release newer than
  the one refused, and tries again; an older one keeps it until `zm daemon update resume`. A hold set by hand is never
  touched; an upgrade that lands lifts the swapper's own. Only Potluck's refusal of the folder (its mode or owner)
  holds; a check that fails otherwise (the folder cannot be made, a full disk) refuses that attempt alone.
- **Pins follow the files.** Before placing, it writes `$ZM_HOME/swap-driver-pins.json`: the sha256 of each driver it
  places (from the staged files) and of each file it replaces when that file is the one pinned (a driver changed by
  hand, refused, or never pinned is never pinned by the swap), and the old folder's pins when the daemon's folder moves
  (a `--dir` that replaces a symlinked `zm`). Right after placing it re-pins the drivers pinned there to the placed
  files (`runtarget.RepinPlaced`; a moved folder through `PinDriver`, checked against the same sums), so the new
  daemon runs them during its verify; after putting the previous files back it re-pins them to those; at its end it
  settles each driver on whichever of the two files is there and removes the file (written synced; one that does not
  parse is moved to `.corrupt` and said). A swapper that died midway leaves it, and the next swap or update tick
  settles it. A driver whose file is neither stays refused, and the swap names it. A home with no pins file gets none
  written. A moved pin is checked after `PinDriver`: a file that changed in between gets its previous pin back.
- **Verify.** Before placing, the old daemon describes every binding it serves itself (machine-level ones and those
  of projects not joined from another machine); a binding that answered then and does not through the new daemon is
  asked again every 2 s for the same 90 s grace as a joined project's items, then it is a regression: rolled back. A
  binding whose listing fails now (the project list, or its scope's) is unknown: said, never counted.
- **Day to day** the daemon asks the same 2 minutes after start, then every 30 minutes, and keeps the answer as
  health's `drivers` (`{checked, refused, problems}`; each `driver <project or machine>/<binding>: <why, ending with
  the fix>`; a warn log line when the set changes). It is a warning, never a failure: `zm status` prints it and keeps
  its exit code, and the self-check does not list drivers.

`zm host share` and `zm pool host` run conformance on the file the daemon will pin (the daemon's folder, as above).

**Bindings.** A project uses a target through a binding: a `[[run_targets]]` entry in project.toml, set only by
the machine's owner on the socket without a token, like tracker binaries and config (section 4); an owner token
or tailnet owner gets `forbidden`. A binding belongs to exactly one project. Its keys: `name` (unique in the
project; the `<target>` in handles), `driver` (one driver may serve several bindings and both kinds), `kind`,
the class `isolation` (`"none"|"ephemeral"`) and `credentials` (bool), `repo` (one of section 4's `repos`; its
remote is the one zm asks), `branch` (that repo's protected default branch, e.g. `main`), `allow_any_ref`
(default false), `scripts` (run; required when the effective class has `credentials` true or `isolation` `"none"`, checked at bind), `budget`, `ttl_max_s`, `handles_max` and,
for db, `create_in`; anything else is driver config. `config` on every call carries the driver config plus
`target` (the binding name) and `kind`; `describe` answers for that binding. zm uses the stricter class of the
binding and `describe` (`none` over `ephemeral`, `true` over `false`): a target never declares itself safer than
its owner did. One-time work uses section 11's optional `setup`, called only by the owner's bind (a `dry_run` first; what it
changes outside zm is in `describe.setup.touches`), never on `acquire`.

Required verbs: `describe, acquire, status, release, list`, and for a box `exec`. Optional: `renew`, `setup`, and
for a box `spawn`, `output` and `signal` (background processes, listed together), `input`
with capability `pty` (terminals, listed together), and `stop` and `start` with capability
`persist` (kept disks, listed together).
Unsupported: `not_implemented`.
The args below are what zm sends a driver; `commit` comes from zm alone.

| verb | id | args | result |
| --- | --- | --- | --- |
| `describe` | - | - | `{"zm": "zm/v0", "name", "kind": "db"\|"run"\|"box", "version", "verbs": [...], "available": bool, "why"?, "isolation", "credentials", "cost": {"unit", "amount", "currency"?}, "reports_spend": bool, "ttl_max_s", "capabilities": [...], "size" (box only): {"name", "os", "cpu", "memory_mb", "disk_gb"?}}` |
| `acquire` | - | `{"request_id", "actor", "ttl_s"?}` plus db: `{"name", "ref"?, "commit"?}`; run: `{"ref", "commit", "script", "args"?: [string]}`; box: `{"image"?, "ref"?, "commit"?, "jit_config"?, "jit_guard"?: {"repo", "events"?: [string]}, "net"?: "on"\|"off", "persist"?: bool}` | `{"handle", "expires_at", "ttl_s", "commit"?, "url"? (db), "log"? (run), "size" and "image_digest" (box), "net"? (box: "on"\|"off"), "spend"?}` |
| `status` | handle | - | `{"handle", "state": "ready"\|"running"\|"stopped"\|"ended"\|"expired"\|"released"\|"reclaimed"\|"lost", "commit"?, "exit"?, "expires_at", "spend"?}` |
| `release` | handle | - | `{"released": bool, "exit"?, "spend"?}` |
| `list` | - | - | `{"handles": [{"handle", "request_id", "state", "expires_at"}]}` |
| `renew` | handle | `{"ttl_s"}` | `{"expires_at"}`; past the lifetime cap is `bad_request` |
| `exec` (box) | handle | `{"argv": [string], "stdin"?, "timeout_s"?, "cwd"?, "secrets"?: {"NAME": "value"}, "encoding"?: "base64"}` | `{"exit", "stdout", "stderr", "truncated": bool, "encoding"?}` |
| `spawn` (box) | handle | exec's `{"argv", "stdin"?, "cwd"?, "secrets"?, "encoding"?}` plus `{"request_id", "timeout_s"?, "pty"?: {"rows", "cols", "term"?}}` | `{"pid"}` |
| `output` (box) | handle | `{"pid", "stdout_from"?, "stderr_from"?, "max_bytes"?, "wait_s"?, "encoding"?}` | `{"stdout", "stderr", "stdout_next", "stderr_next", "dropped_stdout"?, "dropped_stderr"?, "running": bool, "exit"?, "encoding"?}` |
| `signal` (box) | handle | `{"pid", "signal": "TERM"\|"INT"\|"KILL"}` | `{"sent": bool}` |
| `input` (box) | handle | `{"pid", "data"?, "encoding"?, "rows"?, "cols"?}` | `{"written", "running": bool}` |
| `stop` (box) | handle | - | `{"stopped": true, "expires_at", "spend"?}` |
| `start` (box) | handle | `{"ttl_s"?}` | acquire's box answer |

- **Code, not callers, decides where a run goes.** Every socket caller is the owner (section 1), so who asks
  proves nothing; the rule is about the code a target runs. A run's caller names a `ref` and never the commit:
  at acquire time zm resolves `ref` by a live lookup on the binding's remote (`git ls-remote`, never a local
  tracking ref) and sends the result as `commit`; a caller that sends a commit differing from it gets
  `forbidden`. Every run needs a pushed commit: no `ref`, or one the remote cannot resolve, is `bad_request`.
  The code is trusted only when `commit` is the remote head of the binding's `branch`. Any other commit goes only
  to an untrusted-capable target (class `isolation: "ephemeral"`, `credentials: false`), which fetches it from
  the remote; elsewhere it is `forbidden` unless the binding says `allow_any_ref`, the only override
  (owner-only on the socket, like every binding key). Every run target runs a clean checkout of exactly
  `commit`, never a caller's working tree, and echoes it in `acquire` and `status`.
- **Credentialed or unisolated targets** (effective class `credentials: true` or `isolation: "none"`):
  `acquire` is local-only (the socket owner, as spawning is, section 1; a `"none"` target runs as some user on
  some machine, and a UID is no boundary), and a script not in the binding's `scripts` is `forbidden`.
- **Databases.** A db target serves no code, so for a db `ref` is advisory: zm routes by it as above (no ref, or
  an unresolvable one, counts as untrusted), but nothing ties the database's later users to it. For a db, `credentials: true` means
  the server holds something beyond the throwaway copies (other databases, a role that reaches them). Every db
  `acquire` is local-only, so a `url` never crosses the network. `create_in` lists where the target may create:
  each entry is a server `host:port` plus the maintenance database it connects to (`host:port/postgres`);
  anything else is `forbidden` before connecting, never best effort. It drops only databases it made, never a
  template or the server's own. A db's `url` is a secret: zm returns it only in `acquire` answers (a repeat with
  the same `request_id` from the socket gets it again), never in `status`, `list`, events, logs, mail or stderr.
- **Roles.** `acquire` needs `operate` on the project (it spawns work and spends); `describe`, `status` and
  `list` need `read`; `renew` and `release` are the lease holder's (the owner may force `release` on any handle, with section 10's force rules; section 10's own lease verbs refuse these keys). `actor` is
  the zm identity the acquire is made for, as on section 11's write verbs.
- **Names.** Handles are `db:<target>/<name>`, `run:<target>/<id>` and `box:<target>/<id>`. A db `<name>` is the
  caller's (the target may prefix it internally). `status`, `renew`, `release` and `exec` take the handle in the
  envelope's `id`. A malformed
  handle or another binding's is `bad_request`; one this binding never issued is `not_found` (a target keeps an
  ended handle's record for at least 24 h).
- **Idempotent acquire.** `request_id` is required: a repeat with the same key answers the same handle and
  never makes a second resource. A db name that exists under another key is `conflict`. After an `acquire`
  times out, zm retries with the same key or reconciles from `list`; it never acquires afresh.
- **States.** `ready` (the db exists, or the box answers `exec`), `running`, `ended` (the run exited on its own; `exit` is its code),
  `expired` (the deadline ended it; a run has `exit: 124`), `released`, `reclaimed` (the machine's owner took it
  back: the target's own reclaim policy, such as owner activity or a schedule window, ended it; a known cause, no
  `exit`, never billed), `lost` (the target cannot say how it ended), and for a box acquired with `persist`
  `stopped` (not an end: "Kept disks" below). Only `ended` reports a script's own exit, so
  a run cut off never reports success. `release` answers
  `released: true` when it ended the resource and `false` when it was already gone, never an error; it carries
  `exit` only when the run had already ended.
- **Reclaim.** A target on a lent machine may let its owner take it back. While its policy says no, `describe`
  answers `available: false` with `why`, `acquire` is `unavailable`, and the target ends every live resource
  (as on release) and answers `reclaimed` for it from then on. It does this itself, like expiry, whether or not
  zm is up. `release` of a reclaimed handle answers `released: false`, with no `exit`.
- **Lifetime.** A resource lives exactly as long as its lease. On release or expiry the target ends it (drops the
  db, kills the run's process group), and it enforces `expires_at` itself: it never relies on zm to clean up
  (zm's leases survive a restart, but zm may be down at the deadline). harbor's per-run supervisor is the
  reference. `ttl_max_s` (the binding's or describe's, the lower) caps the whole lifetime since `acquire` (a
  persistent box's: since its last `start`), renewals included.
- **Leases.** zm holds each handle as a section-10 lease keyed by the handle (those keys are reserved: only
  `acquire` creates them), whose deadline is the target's `expires_at`. A session-bound lease renews while its
  session exists, even when idle (a session waiting on a long run goes idle; an exception to section 10's idle
  rule), by calling `renew` only once a third of the TTL is left and at most once a minute per handle. When the
  session ends, renewal stops and the target ends the resource at `expires_at`. Without `renew` a lease cannot
  be extended. Expiry publishes `dev.zm.run_target.expired` and nothing else: no section-10 notice to the holder,
  since a run reaching its TTL is normal. A reclaim ends the lease the same way: zm learns of it when `status`
  answers `reclaimed` (it asks when a `renew` is refused, and at the deadline), drops the lease and publishes
  `dev.zm.run_target.expired`.
- **Caps and spend.** A `ttl_s` past the lifetime cap is `bad_request`; an actor already holding `handles_max`
  live handles of the binding gets `forbidden`. `acquire`, `status` and `release` may report `spend`
  (`{"unit", "amount"}`): the handle's total so far, in describe's `cost.unit`. zm keeps the highest figure seen
  per handle and calls `status` once after release, expiry or reclaim for the final one. A reclaimed handle's
  spend is still reported (it never falls), but it is never billed: zm leaves it out of `budget`. `budget` is
  `{"unit", "limit", "period": "day"|"week"|"month"}` (calendar periods, UTC); zm sums its other handles' figures
  per binding and refuses
  an `acquire` once the period's spend reaches `limit` (`forbidden`). A budget on a driver that does not report
  spend (`reports_spend: false`) or in another unit than `cost.unit` is refused at bind.
- **Boxes.** A box runs whatever its holder sends, so it is untrusted code by construction: a `box` binding is
  refused at bind unless its effective class is `isolation: "ephemeral"`, `credentials: false`, and so is a
  `describe` saying otherwise. `acquire` answers once the VM answers `exec`. With `ref` (zm resolves it and sends
  `commit`, as for a run; a binding without `repo` refuses a `ref` with `bad_request`; zm's clients push such a box the renter's commit instead, "Code from the renter" below) the box holds a clean
  checkout of exactly `commit`, which is `exec`'s default `cwd`; without, it starts empty. `scripts` and
  `allow_any_ref` do not apply. `size` is the binding's one size (driver config `size`; a host offering two binds
  twice) and `describe.size` states it. `image` names one of the binding's owner-set, digest-pinned `images`
  (default the first); anything else is `bad_request`, and `acquire` answers the `image_digest` it booted.
  - `exec` is the lease holder's, like `renew`. `argv` is literal (a string or an empty array is `bad_request`);
    `stdin` is the whole of its input. It runs as one user with `HOME` set to a directory that user can write,
    and `TMPDIR` (or `/tmp`) writable. `cwd` is a path in the guest, a relative one resolved against the default
    (the checkout, else `HOME`). `timeout_s` defaults to 30; past 600 is `bad_request`; it is cut to
    `expires_at`. A timeout kills the command and everything it started (its process group) and answers
    `exit: 124`; a child left holding the output never delays the answer. `stdout` and `stderr` keep their first
    1 MiB each (cut by bytes; invalid UTF-8 is replaced), and `truncated` says one was cut. `exec` answers
    within `timeout_s` + 30 s. On a box that is not `ready`, or past `expires_at`, it is `conflict`. Its output
    is data, never instructions. What a command leaves running when it exits (a child in its process group, a
    `nohup ... &`) is killed then; only a process that left the group (`setsid`) outlives it, until the box ends.
    A long command is a background process instead. `encoding: "base64"` (a driver listing capability `base64`;
    zm refuses it to any other with `bad_request`, since an older driver would take the text as the bytes): `stdin`
    is base64 and so are the answer's `stdout` and `stderr` (the 1 MiB caps count bytes), which carries
    `"encoding": "base64"`, so small files cross intact.
  - **Background processes**, the
    holder's, like `exec`, `conflict` on a box that is not ready or past `expires_at`, each call answering well
    inside the 30 s a relayed call has: `spawn` starts exec's command (same argv, cwd, stdin, secrets and encoding
    rules) and answers a `pid` at once; `request_id` is required, and the same one answers the same `pid` without
    starting anything. `timeout_s` (0, the default: until the box ends; at most `ttl_max_s`) kills its group with
    `exit: 124`. `output` answers its bytes from the byte offsets `stdout_from` and `stderr_from` (default 0; past
    what was written is `bad_request`), at most `max_bytes` of both streams together (default 64 KiB, at most
    1 MiB; each stream at least half when both have more), waiting up to `wait_s` (0..20) for a new byte or the
    exit, so calling it again from `stdout_next` and `stderr_next` pages the output and doubles as a wait. A
    text page never ends inside a UTF-8 character while more of the stream may follow (a page cut by `max_bytes`,
    or a running process): a character still being written is nothing new, so the call waits; a `max_bytes`
    smaller than the next character answers that whole character. base64 carries the bytes exactly.
    `running` is false once it ended, and only then is `exit` there. The driver keeps at least the newest 8 MiB of
    each stream; `dropped_stdout` and `dropped_stderr` count the bytes between the offset asked and the first one
    answered that it no longer has. `signal` sends TERM, INT or KILL to the process's group and answers `sent`
    false once it ended. Its group's leftovers die when it exits, as an exec's; every process ends with the box.
    A box remembers 32 processes (an older ended one is forgotten past that; 32 running refuse `spawn` with
    `conflict`); an unknown `pid` is `not_found`. A driver without the verbs answers `not_implemented`. In
    `zm-run-tart` one file serves Tart, Incus and OCI: a detached holder per process runs it through the layer's
    exec channel and writes its output and exit to host files under the state directory (`potluck/runtarget/
    boxproc`, shared with the reference driver), `output` reads those files without asking the guest, and
    `signal` is a short guest exec on the group id the guest wrapper wrote.
  - **Terminals** (Celesto's `computer ssh`, with no ssh key and no open port: the exec channel,
    over every path the background-process verbs take), the holder's, like them: `spawn` with `pty` (a driver
    listing capability `pty`; zm refuses it to any other with `bad_request`, since an older driver would run the
    command with no terminal) runs the command on a pseudo-terminal of `rows` by `cols` (1..1000) with `TERM` set
    to `term` (a terminal type name; default `xterm-256color`) in its own session, the terminal its controlling
    one; both streams are its output and come as `stdout` (`stderr` stays empty); `stdin` is typed first. A pty
    takes no `secrets` (`bad_request`: a terminal echoes what it reads). `input` writes `data` (at most 64 KiB;
    base64 with `encoding`, which every driver serving `input` takes, so zm asks no capability for it) to the terminal, applying `rows` and `cols` (together, 1..1000) first, and answers
    `written` (bytes taken; fewer when the terminal read nothing for 10 s) and `running`; an ended process answers
    `written: 0, running: false`, one without a terminal is `bad_request`, an unknown `pid` `not_found`. A Ctrl-C
    or Ctrl-D is a byte like any other: the box's own terminal turns it into SIGINT for its foreground process
    group or an end of input. `output` and `signal` work as for any background process (`signal` reaches the
    command's own process group, not a job its shell moved to the foreground; a waiting `output` on a terminal looks
    for new bytes every 10 ms, any other every 50 ms). In `zm-run-tart` the holder runs the command through the layer's terminal exec
    (`tart exec -i -t`, `docker exec -i -t`, `incus exec --force-interactive`) on a host pseudo-terminal it owns,
    whose size the layer carries into the guest; `input` hands the holder frames of at most 512 bytes (PIPE_BUF,
    so each is whole) through a FIFO in the process's directory (`potluck/runtarget/boxproc`, shared with the
    reference driver). `zm sandbox shell <handle> [-- argv]` (default a login shell: bash, else sh) puts the local
    terminal in raw mode, sends its size and each resize, keeps one `input` call in flight (keys typed meanwhile go
    in the next, at most 32 KiB a call, base64 so any byte crosses; the part a terminal did not take goes again; a
    lost call loses its keys, never repeats them; a refusal that would recur on every key, such as an older host
    that relays no `input`, ends the session with its message) and one `output` call
    waiting, exits with the command's code, kills it when zm itself is signalled, and ends it on Enter `~` `.`.
    Piped input works too (an end of input sends Ctrl-D). A key's echo measured on one machine: about 30 ms (p50)
    through the local daemon and relayed to a host over the tailnet or the tunnel stand-in alike; a real network adds about two
    round trips. Keys cross a relay as `exec`'s stdin does: in the clear to the authority's operator, and over a
    Cloudflare Tunnel where Cloudflare can read them (only `secrets` are sealed), so a password typed into a box's
    terminal crosses as typed. MCP has no terminal tool.
  - **Code from the renter** (`potluck/boxpush`). A box whose binding has no `repo`
    (every box served from another machine) cannot fetch a ref, and needs no credential to get one: the renter's
    client sends it. It bundles one commit of its own checkout (default `HEAD`; a git v2 bundle, v3 for SHA-256
    repositories, holding that commit and its tree only, the box marking it shallow as `clone --depth 1` would;
    `--full` the whole history; `--dirty` adds the working tree's changes to tracked files as a second commit on
    top, and `--include-untracked` with it the files git neither tracks nor ignores, each named on stderr (an
    untracked `.env` never goes unasked); written through a temporary index so the renter's refs, index and
    files are untouched), refuses one over the limit (default 128 MiB) before anything is sent, and ships it through `exec`
    alone, no new verb: one short `sh` script per call with literal arguments (no value spliced into a script),
    stdin base64 (capability `base64`). A probe checks the box has `git` and a SHA-256 tool (`sha256sum`,
    `shasum` or `openssl`) and lists the chunks already there; each 512 KiB chunk (its request about 700 KB of
    JSON, inside every door's and the team proxy's 1 MiB body, each call answering in well under a second, so
    inside a relayed call's limits and the tunnel's 100 s) goes to `$HOME/.zm/push/<key>/c.<i>`, kept only when
    its SHA-256 matches the one sent beside it (written to a temporary name, then renamed), up to four in
    flight; a call that fails without a refusal, or a chunk that arrived damaged, is sent again (at most four
    tries), and a push that still fails ends with the chunks kept, so the same push again sends only what is
    missing. The last call joins the chunks, checks the whole bundle's SHA-256 (a mismatch drops them and checks
    nothing out), fetches the bundle into the push's own git directory (`$HOME/.zm/push-git/<sha256 of the
    checkout's physical path>`; the ref `refs/zm/push`, no remote), checks the commit out detached in `~/checkout`
    (or `--dir`: under HOME, never `/`, HOME, a directory holding it, or in `HOME/.zm`), verifies `HEAD` and removes
    the chunks, in 90 s; sent again after its answer was lost, it finds the chunks gone and `HEAD` at the commit and
    answers the same. The checkout's `.git` is a file naming that git directory: a `.git` already there (a layer's,
    the image's) is moved aside unread, so its hooks, config and replace refs never apply, and the first push into
    a directory removes every untracked file but ignored ones and `node_modules`, so it holds the commit's tree
    (Potluck #157). Nothing but the bundle's bytes
    enters the box. Measured on the reference driver (one machine): zm's own `HEAD` is a 7.0 MiB bundle in 15
    chunks, made in 0.3 s and pushed in 0.5 s; 3 MiB in 7 chunks through a relay across two daemons, 0.4 s.
    `zm sandbox create --ref <ref>` on such a box acquires it, then pushes (printing the handle first; a failed
    push keeps the box and names `zm sandbox push <handle>`, which pushes into a box held already, to resume);
    on a binding with a `repo` `--ref` stays the pushed ref zm resolves, as above. MCP's `sandbox_create` does
    the same (`ref`; `dirty`, tracked files only) from the checkout `dir` names (absolute; a relative one is
    `bad_request`; default the server's working directory), and its answer's `push` names the `repo` and
    `commit` sent.
  - **Files in and out** (`potluck/boxcp`; Celesto parity).
    `zm sandbox cp <local> <handle>:<path>` and `zm sandbox cp <handle>:<path> <local>` copy a file or a
    directory with everything beneath it, through `exec` alone as a push does (no new verb or route; so across
    machines and the tunnel within the same caps, and the holder's only, by exec's checks). A box path is
    under the box's `HOME` unless absolute (`<handle>:` alone is `HOME`); `box:` starts a box argument, so
    `./box:x` is a local file. As `cp`: an existing directory takes the copy by its name, a path ending in `/`
    is made a directory first, else the path is the copy's own name. The limit is one copy's bytes (a file, or
    a directory as a tar): default 128 MiB, `--max-mib` up to 1024; past it the copy is refused with
    `too_large` before a byte is sent up, or by the box before a byte comes down. Up: a file goes as its bytes,
    a directory as a tar the client writes (directories, regular files, symlinks as links never followed;
    devices, sockets and FIFOs left out and listed; no owner names or ids; a directory holding this machine's
    temporary directory, where the tar is written, is refused). A first exec resolves the destination once, as
    `cp` has it, to one absolute path (refusing a directory onto a file or a file onto a directory before a
    byte is sent); every later exec writes exactly that path, so one sent again after a lost answer does the
    same thing, never a second copy inside the first. The bytes go in 512 KiB chunks by the push's own chunk
    sender (`boxpush.Send`: probe, checked chunks under `$HOME/.zm/push/<key>`, four in flight, retries,
    resume), the key naming the bytes and that destination, so two copies of the same bytes to two places
    never share or clean up each other's chunks; one more exec (90 s) joins them, checks the whole SHA-256 (a
    mismatch writes nothing), writes the file (its mode kept) or untars it with the box's `tar -x -o`
    (merging into a directory there), and answers the path as the box's shell has it. The chunks go after it
    answers, written or refused; with no answer (or a chunk missing) they stay, and the same copy again
    resumes. Down: one exec (90 s) snapshots the path in the box's `TMPDIR` (a file's bytes, or the box's
    `tar -cf - .` of the directory, cut at the limit + 1 to see it passed) and answers kind, size, SHA-256 and a
    file's executable bit; a directory holding that `TMPDIR` (`/`, say) is refused, never a tar of its own
    snapshot. 512 KiB pages come back by `dd` through exec's base64 output (each checked for its length,
    `truncated` refused), four in flight, into a temporary file beside the destination; the snapshot is removed
    whatever happened (else it ends with the box). The client checks the SHA-256 against the box's, then writes
    the file (mode 0644, or 0755 when executable there; replacing a file at the destination) or unpacks the
    tar itself with Go's `archive/tar` into a fresh directory beside the destination under `os.Root`, renamed
    into place at the end: the box's bytes and names are data, never run, never given to a local `tar`. An
    entry whose name is absolute or holds a `..` component refuses the whole copy (`forbidden`, nothing
    written), and so does a sparse entry (an old GNU `S` entry or PAX `GNU.sparse.*` records, which a tar
    reader expands to any size) and writing past the copy's limit in all (a running total); only directories
    and regular files are written (modes at most 0755, no set-id bits, no owners);
    symlinks, hard links, devices and FIFOs are left out and listed (names quoted, at most 50); a directory
    already at the destination is `conflict`, never merged. MCP's opt-in `sandbox_write_file` (`path`,
    `content`, `encoding` `text` or `base64`; at most 1 MiB; exactly that path, a directory there refused) and
    `sandbox_read_file` (`path`, `offset`, `max_bytes` default 32 KiB, at most 512 KiB; one exec; answers
    `size`, `next`, `eof`, and `content` as text when UTF-8, cut back to a whole character, else base64;
    untrusted) are the same over one file. Measured on the reference driver through a relay across two
    daemons: a 2 MiB file and a 2 MiB directory up and the whole tree down in 0.7 s.
  - **Jobs**: `zm job run [--on|--size|--os] [--ref] [--dirty] [--timeout 30m] [--keep] [--net off]
    [--image|--template] [--deps npm[:path]] [-- argv]` places and acquires a box as `zm sandbox create` does (lease: the timeout plus 10 minutes, cut to
    the binding's lifetime cap, refused when the timeout does not fit), pushes the commit, `spawn`s the command
    in the checkout with `timeout_s` the timeout, follows `output` (20 s waits, 256 KiB pages) to the terminal as
    it comes and exits with its exit code; the box is released at the end, also on Ctrl-C or a closed terminal
    (SIGHUP, unless zm was started with it ignored; the process is sent TERM first; exit 130; a Ctrl-C during
    the acquire asks again with the same `request_id`, which answers the box the daemon went on making, and
    releases it), an error, or the timeout (exit 124; zm sends KILL itself 90 s past it), unless
    `--keep`; the release is asked again for up to 3 minutes when the authority does not answer. The box is
    asked for a 5-minute lease, not the whole one, and zm renews it every minute while the job runs (up to the
    planned end, when the driver renews; a `--keep` box has the whole lease): a client that dies leaves a box
    that ends within 5 minutes, not at the end of the timeout plus 10 minutes, once no live
    session holds it (the sweep also renews a box whose holder is a live session, up to the lifetime cap). A process that
    ends with no exit code, or whose output stays unreachable for 2 minutes, is
    `lost` and fails (exit 1), never a pass. No argv: the commit's `.zm/job` (run directly when committed
    executable, else with `sh`): the job lives with the code, so a commit names one job. No argv and no
    `.zm/job` is refused before a box is made. `--json` prints `{"handle", "target", "commit", "pid", "state": "exited"|"timeout"|
    "lost"|"interrupted"|"failed", "exit"?, "error"?, "deps"?, "kept", "released", "seconds"}` on stdout alone (the
    command's output then goes to stderr). Agents use the sandbox tools in the same order (the `sandboxes`
    skill); there is no job tool, since a job outlives a tool call.
  - **Dependency layers** (the rules are Potluck's, [docs/daemon.md](https://github.com/sengirunners/potluck/blob/main/docs/daemon.md) "Dependency layers"):
    `zm sandbox create` and `zm job run` take `--deps npm[:path]` (repeatable, at most 4), `--deps-wait S` (0 to
    3600, default 600) and `--deps-scripts off`, sent as acquire's `deps` and `deps_wait_s`. The files
    (`package-lock.json`, `package.json`, each workspace's `package.json`) are read from the working directory
    through a root they cannot leave (`runtarget.CollectDepsFiles`: no symlink out, regular files only); the
    path defaults to the push's checkout (`--dir`) plus the directory's place in its repository, so the push
    checks the commit out around the layer's `node_modules` (`git checkout -f` writes tracked files only; the
    box sees the sealed layer copy-on-write); `zm job run` then runs the command (the commit's `.zm/job` too) in that
    directory, beside the layer. A file that differs from the pushed commit is warned about. One
    stderr line says each layer's answer: `hit`, `built` or `not mounted`; `--json` carries `deps` as the daemon
    answered. On a binding another machine serves, `--image` with `--deps` is refused before a box is made
    (the layer is keyed by the pin the host advertises for a template): by the daemon's acquire, and by
    `zm sandbox create` before it makes the bundle. The flags, the line and the warning are Potluck's `depscli`. With `--deps` and `--ref`, a binding that
    checks out refs itself does not fit (its checkout is not where a layer mounts): placement passes to the next
    binding, and with none left the error says to create without `--ref`, then `zm sandbox push`.
  - A box is never `running` or `ended` and has no `exit`; it is `ready` until it is `released`, `expired`,
    `reclaimed` or `lost` (a persistent one is also `stopped` between). Files persist across `exec` in one box; no
    box sees another's, and every box is wiped when it ends, commands still running in it included.
  - **A GitHub runner is a box**: `jit_config` is a
    just-in-time runner config the authority minted (GitHub's `encoded_jit_config`), and the box's first command is
    that runner, started before `acquire` answers. A driver serving it lists capability `github-jit`; one that does
    not answers `bad_request`, as does a `jit_config` on any other kind. It is a single-use credential: the driver
    hands it to the guest on the exec channel, never the host's argv or environment, VM config, a record or a log,
    and keeps no copy (inside the guest it is the runner's, for that box's life). A runner that exits within the
    first seconds fails the `acquire` (`unavailable`); reusing a `request_id` with a different `jit_config` is
    `bad_request`. Registering and deregistering the runner, and sweeping orphans, are the authority's, never the host's.
    The box stays `ready` after the runner exits, until it is released or expires. Whoever may acquire the box
    may bring their own `jit_config` (the box then serves the repo that config names; the class rule above still
    holds); the authority's runner broker brings one it minted for a linked installation. `jit_guard` (with a
    `jit_config` only; `repo` owner/name and `events`, at most 32 event names, not secrets) says which jobs the
    runner serves: the runner starts with a job-started hook (`ACTIONS_RUNNER_HOOK_JOB_STARTED`, a JavaScript
    file outside the runner's directory, which the runner runs with its own node) that stops a job unless
    `GITHUB_REPOSITORY` is `repo` (case-folded) and, when `events` is given, `GITHUB_EVENT_NAME` is one of them
    and, for a `pull_request*` event, the event payload (`GITHUB_EVENT_PATH`, parsed as JSON) names a
    `pull_request.head.repo` whose `full_name` is `repo` and that is not a fork. No `events`: any event, any
    origin. These are the values the hook's environment has for this (actions/runner: the `github` context's
    allowlisted keys, the payload written before the hook runs; no head repository, visibility or labels).
    The runner runs the hook after "Set up job" and before any other step, and waits for it; a hook's non-zero
    exit only marks the job failed (its `if: always()` steps and actions' `pre:` scripts would still run), so
    on a refusal the hook SIGKILLs its parent first (`Runner.Worker`, which starts the hook's node itself and
    waits for it), which leaves no step to start, then its other ancestors up to `run.sh`, else `run.sh`'s
    process group (found through `/proc`, else `ps`), then, on a VM layer (Tart, Incus), powers the guest off
    where it may (as root or through `sudo -n`; the box then ends `lost`). An image where the launch cannot
    find its process group (no `/proc` and no `ps -p`), or whose runner `.env` names a hook of its own, fails
    the `acquire`. zm sends `jit_guard` only to a driver listing capability `github-jit-guard`
    (else `bad_request`), and reusing a `request_id` with another `jit_guard` is a different request. What the
    hook cannot do: when it runs, the runner already holds the job's message (its secrets and token among it)
    and "Set up job" has downloaded the job's actions; no step of a refused job runs, and the hook's own
    environment holds no secret.
  - **Secrets**. A renter may hand a command its own secrets: `exec`'s `secrets`, names
    `[A-Z][A-Z0-9_]{0,63}`, at most 64 KiB of names and values in all (an empty map is none). Only a driver
    listing `"secrets"` in `describe.capabilities` takes them; any other answers `bad_request` and never runs the
    command without them (a box that is not ready answers `conflict` first). The driver puts each
    value in a fresh `0700` directory as a `0600` file named for it, in memory where the guest offers it (a
    tmpfs, Linux `/dev/shm`), else under `TMPDIR`; the guest receives the values on the exec channel, framed ahead
    of the command's stdin, never in argv, the environment or the VM's config, in the guest or in any host-side
    process (`tart exec`, `incus exec` and the like). The command runs with `ZM_SECRETS` set to that directory (a
    path, never a value) and the directory is removed before the answer when the command ends or times out, and
    with the box; where the guest has no memory filesystem, a crash leaves the file until the box is wiped. One
    box is one trust domain: anything else running in it as the same user can read the directory while it
    exists. An `exec` without `secrets` has no `ZM_SECRETS`. zm sends `secrets` only to
    a target on a `trusted` host (section 8 "Operator class"). Every binding's driver runs on its own machine
    today, the owner's, so trusted; once a binding is served from another machine, secrets go to it only when
    that machine is `trusted`, else `403`. Neither `secrets` nor a `jit_config` crosses a Cloudflare Tunnel in the
    clear (Cloudflare reads the traffic), on any leg: they go sealed to the receiving
    daemon's certified key (section 17.11 "Sealed secrets"): a member joined over the tunnel seals them to its
    home, and an authority seals an op holding them to the host whose poll or heartbeat came over the tunnel. A peer
    too old to seal keeps the earlier refusal: the member's machine refuses before the call leaves (`403`,
    naming the tailnet), and the authority never hands the op to such a host (the call answers the same `403`; the
    op never runs). Over the tailnet both are sealed too between machines that can seal; to or from one that cannot, the pool's
    sealing mode decides (17.11: `preferred` sends as before and warns, `required` refuses); over the socket they go as they are.
    A call is sealed only when it carries `secrets` or a `jit_config`, and then its whole body is; without them argv,
    stdin and cwd travel readable, and the output always does: the pool's machine reads them and, over the tunnel, so can
    Cloudflare. A command that prints a secret sends it back readable. Pass sensitive inputs as `secrets` (`--secret`).
    Each `exec`/`spawn` `secrets` and each acquire's `jit_config` is checked at the authority against the pool's secret grants
    (`GET /v0/secret_grants`; the built-in defaults let a member send `value` secrets and a `jit_config` to a trusted host's box,
    as before; no default covers the box-scope `secrets` verb). Job grants (Potluck #129): the daemon runs Potluck's
    secrets issuer (`issuers.NewModule`, after run targets and before the runner broker) only when config.json's
    `secrets.issuers` names exactly one `github-app` issuer; with none, or one it cannot read as safe, nothing is
    minted. A grant `to: {"job": {repo, workflow, ref, events}}` (kind `github-token`, scope `box`) gets the GitHub job
    it names a one-repository token in its runner box, minted only once the job holds that box (`in_progress` on its
    own runner, every fact read again from GitHub, a fork or foreign reusable workflow refused, the host's class
    from run targets' record), renewed while it runs and revoked at `completed` or when the box ends (potluck
    [docs/daemon.md](https://github.com/sengirunners/potluck/blob/main/docs/daemon.md) "Secrets issuers"). A replica, an export and a webhook do not carry
    the names in that table or on the secret ledger, and a tunnel read omits them (17.11). zm never stores, logs, events, meters or echoes them: a refusal names
    neither a value nor the names given, a malformed `secrets` field is refused without quoting it, and a
    driver's own refusal reaches the caller with every name and value of 4 bytes or more replaced by
    `[secret]` (and a `jit_config` by `[jit_config]`), scrubbed on the machine that runs the driver, before a
    relayed op's report leaves it. A driver never puts them on its stderr or in an answer. A driver killed at zm's deadline
    (`timeout_s` + 30 s) cannot clean up: its directory then lasts until the box ends. Values come from the renter's own client or the
    automations home, which read their own secret references (`zm target exec` and `zm sandbox exec` take
    `--secret NAME=@file` or `--secret NAME` for `$NAME`, never a value on the command line); zm keeps no secret store. "Trusted" means
    trusting that host's root.
  - **Network** (sandboxes reach parity with Celesto's, internet
    included; the design: trusted hosts open egress, public hosts an
    allowlist). `acquire`'s `net` (a box's; `on`, the default, or `off`; anything else `bad_request`) says whether
    the box has a network. A driver that honours it lists capability `net-off` (`zm-run-tart` on `vm=oci`), and zm
    sends it `net` on every box acquire: `on` when the binding's machine is `trusted` (section 8 "Operator class";
    this machine's own bindings are) and the renter did not ask for `off`, else `off`. With `on` the box is made on
    the binding's filtered network and nothing wider: on oci its `oci_network` (`potluck-box`; `zm-box` where zm set it up; whose filter drops
    this machine's own and broadcast addresses, private, CGNAT, link-local and cloud metadata ranges, outbound mail
    and all IPv6; or an owner-named one that passed `share`'s net checks), with `NET_RAW` dropped and its own
    resolvers, on a runner binding as on any other; a binding with network `none` stays `none`. So a trusted host's
    sandbox or job reaches the internet (Claude's API, npm, GitHub) from the lender's IP, and never the lender's
    machine or LAN. With `off` it is made with network `none` whatever the binding's network (Celesto's
    `networkPolicy` off), its checkout fetched by the host. `acquire`'s answer says which the box got, `net`: `on`
    or `off` (from the driver, which knows the binding's network; `zm-run-tart` answers it for every box: Tart and
    Incus boxes are `on`), passed on in zm's acquire answer only, never stored; `zm sandbox create` prints it
    (`net=on`). A public host's box gets `off`: no egress allowlist
    exists yet, so until one does a public host's oci box has no network (open question 6 of the sandboxes plan, whose default this is; an allowlist would be a binding key the
    lender sets). `net` is part of the request: `on` and unset are the same request, and a `request_id` reused
    with the other is `conflict` (the driver answers `bad_request`). The class is read at `acquire` only: a host
    demoted from `trusted` keeps its existing boxes' network until they end (`renew` does not check again), and
    a `request_id` retried after the class changed is refused as a different net. Fail closed: when zm sent
    `off` and the answer's `net` is not exactly `off` (missing, or `on`: say a host put back an older
    `zm-run-tart` while its last advert still lists `net-off`), zm releases the box and answers `internal`, as for
    an image off the pin. A driver without `net-off` (Tart, whose Softnet
    gives every box egress; Incus; any older driver) is sent no `net` and keeps its own rule; `net` `off` to it is
    `bad_request` before it is called, and placement (`zm sandbox create --net off`, `zm job run --net off`, MCP
    `sandbox_create`'s `net`) skips it with the reason. On a machine whose `potluck-box` filter unit is not `active`
    the driver makes no box on that network (fail closed: the acquire is `unavailable` and names `--net off`); a
    machine `share`d without the network binds network `none`, so its sandboxes have no internet, and `doctor`
    says so. How code gets in is unchanged: the binding's checkout comes through the host on a runner binding (a
    box fetches for itself only on a binding without `runner_dir` set to `fetch=guest`), the renter's own commit as
    a bundle through `exec` ("Code from the renter"), so no host ever fetches a URL a renter chose. Conformance
    (`--kind box`, group `net`) acquires its networked box with `net` `on` from a driver listing `net-off`, so the
    net checks (this machine's addresses, `--net-forbid`, outbound mail, peers, `--net-allow`) cover a sandbox on
    that network, and then checks a box acquired with `net` `off` reaches none of `--net-allow` while its probe
    still sees a port open inside it (no `--net-allow`: a SKIP).
  - **Kept disks** (Celesto parity, overriding an earlier
    default of no persistent disks in v1; trusted hosts only). `acquire`'s `persist` (a box's; on a driver listing
    capability `persist`, else `bad_request`; never with a `jit_config`; a public host's box is refused `403`, since
    its host can read a kept disk; part of the request, so a `request_id` reused with the other value is
    `conflict`) makes a box that stops instead of ending: `stop` (the holder's, or the machine owner's; a box
    acquired without `persist` is `bad_request`) ends everything running in it and keeps its disk, answering
    `stopped` and, as `expires_at`, when the kept disk is deleted (the driver's keep, `persist_keep_s` in
    `zm-run-tart`: 30 days by default, the owner's to lower or raise); the box's lease reaching `expires_at`, a
    reclaim, or the guest shutting itself down stop it the same way, never wiping it. `start` (`ttl_s`, as
    acquire's; the holder's alone: not even the machine owner starts another's disk) boots that same disk on the
    same host with a new lease and answers as `acquire` does; the lifetime cap counts from it. `release` deletes a
    stopped box's disk at once; past `expires_at` the target deletes it itself and the box is `expired`. A stopped
    box is `stopped` in `status` and `list` (zm keeps it live: it counts against `handles_max`, it is not renewed,
    and `exec`, `spawn`, `renew` and the rest are `conflict`); it holds no slot, only its disk, and its spend does
    not grow (the driver reports time up only), so the usage row written when it finally ends bills only its time
    up. zm asks `trusted` again at `start`: a host made `public` since keeps the disk but starts nothing on it
    (`403`; release deletes it). Secrets are never written to a kept disk by zm: on a persistent box `exec`'s
    `secrets` go only to a memory filesystem (`/dev/shm`), and a guest without one refuses them (a macOS Tart
    guest today); zm sends nothing else long-lived into a box, and a runner box is never persistent. What is kept
    is the renter's: anything the renter wrote stays on the lender's disk until the box is released or expires.
    In `zm-run-tart` (`potluck/vmhost`, layers Tart and OCI; Incus not yet): a stop is a `sync` in the
    guest, then a power-off that keeps the VM (Tart: the VM's own disk; OCI: the same container, stopped, with its
    `HOME` also on a named volume of its own, `<vm>-home`, mounted at the image's `HOME` read from its config).
    gVisor's runsc keeps a container's root changes in an overlay of its own by default (`overlay2` `root:self`;
    `root:memory` alike), which a stop drops: measured on runsc release-20261102.0 under docker 29.8, files
    written in `/etc`, `/usr/local`, `/var/tmp` and `HOME` were all gone after `docker stop` and `docker start`.
    So a kept runsc box is created with the OCI annotation `dev.gvisor.flag.overlay2=none` (docker's and
    podman's `--annotation`; no host config changes), which sends the root's writes through the gofer to the
    container's own writable layer: the same measurement with it kept every file, and so did the driver's own
    create, stop and start. Kata keeps the root as it is. runsc takes the flag only where its override allowlist
    has it: a runsc that refuses the annotation does not fall back to `HOME` alone, it fails the kept box's boot,
    and the driver answers that failure naming the annotation and that runsc's version. Under nerdctl (no
    annotation is sent) only `HOME` is kept; describe's `disks.root` says which, and conformance's `persist` group holds a driver
    to it (a file in `/var/tmp` must survive when it says `root`). Its supervisor's guard powers it off, never deletes it, when the
    supervisor goes. `start` waits for the last supervisor to be done with the VM, then a new supervisor boots it
    without a clone or a checkout. At most `persist_max` persistent boxes (stopped or not; default 4, 0 turns
    persistence off) live in a state directory, and describe's `disks` says `{"kept", "max", "keep_s", "root"}`; a
    persist acquire or a `start` is `unavailable` while the layer's disk directory (docker's root dir, podman's
    graph root, `$TART_HOME`) has less free space than `persist_min_free_gb` (default 10; 0: no floor), since kept
    disks live for weeks; a
    stopped box frees its slot, and `start` without a free slot, or while the owner's reclaim policy says no, is
    `unavailable` (the disk stays kept). A reclaim stops a running persistent box at once (no `sync`) and leaves
    a stopped disk where it is: it holds no CPU or memory. The driver deletes a disk past its time on its next
    call (every verb sweeps; zm's own sweep asks `status` at `expires_at`, so a bound binding's disks go on
    time); a binding the owner removed leaves its state directory's kept disks until `zm-run-tart` next runs on
    that directory (the authority records such boxes `lost`). A box held by a
    session can be started only by that session. A supervisor marks itself alive every 5 s, so a box whose
    supervisor died (the host went down) is stopped by the next sweep with its time up counted to that last
    mark, never through the downtime. `start` asks the driver's `status` first, so a row that still says `ready`
    after the lease's end stopped the box starts it rather than answering the stale lease. For owners: a stopped
    box is a live handle, so rebinding a project binding (`replace`) or removing it is refused (`409`) while
    stopped boxes keep disks there, up to their 30 days; `zm sandbox ls` lists them (`stopped`) and the machine
    owner ends each with `zm sandbox rm`, which deletes its disk. Clients: `zm sandbox create --persist`, `zm sandbox stop`,
    `zm sandbox start [--ttl]`, `zm sandbox rm` (deletes the disk), `zm sandbox ls` (a stopped box reads
    `persist (disk kept until expires; zm sandbox start)`); MCP `sandbox_create`'s `persist`, `sandbox_stop` (a
    persistent box stops; any other ends, as before), `sandbox_start` and `sandbox_delete`. Conformance group
    `persist` (a driver listing it: a file in `HOME` survives stop and start, stop of a box without persist is
    `bad_request`, release of a stopped box, a lease's end stops rather than expires). Not yet: a home that
    outlives a box and attaches to new ones (Celesto's `persistentHome`), named volumes attached to boxes, kept
    disks counted by bytes against the host's free disk (they are counted by number), Incus.
  - **Root and Docker** (Celesto parity, root inside and
    containers inside, no access to the host). A box target lists capability `root` when its user has working
    `sudo` (no password), and `docker` (always with `root`) when that user can `docker run`. In `zm-run-tart`
    they are an oci binding's `root=on` and `docker=on` (docker implies root; a Tart or Incus box is a full VM
    whose image decides both, and the keys are refused there); `zm host share --docker` binds them, and a re-run
    without the flag keeps what an earlier share of those bindings bound, as admission keys are kept
    (`--docker=false` turns it off). Sudo is opt-in: a default box keeps every flag it had and has no root
    (a decision). Measured on runsc release-20261102.0 under docker 29 with zm's Linux image: `sudo`
    is setuid, and a default box has neither: `no-new-privileges` stops setuid, and gVisor ignores setuid bits
    unless runsc runs with `--allow-suid`. So `root=on` drops `no-new-privileges`, also drops `MKNOD` (and `NET_RAW` with no network), and needs
    a runtime that honours setuid: Kata (a guest kernel), or `runsc-root`, a second docker runtime entry, the
    same runsc with `--allow-suid` and nothing else. `zm host share --docker` registers it once beside `runsc`:
    it copies `/etc/docker/daemon.json` to `daemon.json.zm-bak-<date and time>` (zm's own backup; gVisor's
    `daemon.json~` is rewritten by every `runsc install`), runs `runsc install --runtime=runsc-root --clobber
    --download-sidecars=NEVER -- --allow-suid` with the runsc docker's `runsc` entry runs, then reloads docker
    (`systemctl reload docker`, else SIGHUP to `/var/run/docker.pid`; running containers keep running): one new
    key; `runsc`, which every default box and any container of the owner's own uses, is not changed, and
    doctor's root line leaves the step out. `runsc-root` counts only as zm makes it: `doctor` and `zm-run-tart`
    (before every box with root, and every start of a kept one: docker resolves the name each time) decode
    docker's runtimes and require the path of docker's `runsc` entry, both resolved (PATH, symlinks; else a
    binary named `runsc`) and `runtimeArgs` exactly `["--allow-suid"]`; a hand-made or stale entry (runc, or runsc
    with `--net-raw` or `--network=host`) is a problem whose root step replaces it, and no box with root is made
    on it. Nothing else changes: Docker's default capability set with `NET_RAW` dropped on a network, no
    `--cap-add`, no device, no host mount. Root holds the container's bounding set and no more, and under gVisor
    every capability is the Sentry's, never one of the host kernel's (runsc's processes hold none on the host), so
    `--allow-suid` changes only which in-sandbox process holds the box's own capabilities; which is why the
    bounding set's `NET_RAW` drop is load-bearing, and conformance checks it. A kept box keeps the
    root and docker it was made with (its container is reused at `start`), so a `start` under a binding whose
    root or docker now differ is `conflict` (its disk stays kept until `release` or its time; never recreated,
    which would lose the renter's disk): a box never runs with more root than describe says, nor advertises a
    docker it lacks. Docker is rootless: `zm-dockerd` (zm's Linux image), started by PID 1 when docker is on,
    runs dockerd in a user namespace of the box user's (rootlesskit; `newuidmap` is the setuid part) on a
    slirp4netns user-mode network, so containers' traffic leaves as that user's ordinary sockets through the
    box's own filtered network; every exec gets `DOCKER_HOST`, and sudo keeps it. Measured and refused: a rootful
    dockerd needs `NET_ADMIN`, and with `NET_ADMIN` root in a gVisor box re-addressed its `eth0` (a neighbour's
    address, a new MAC) and answered for it on the shared bridge; Docker's bridge networking inside gVisor
    (gVisor's docker-in-gvisor guide) needs runsc `--net-raw` and `--allow-packet-socket-write`, which let a box
    forge frames onto that bridge. Bridge networking inside stays off (a decision; per-port
    anti-spoofing first, a follow-up), so: inside a box `docker run` and `docker build` have no network unless
    given `--network host` (the slirp stack: the internet, through the box's filter), there is no
    container-to-container bridge, `-p` publishes nothing outside the box, and storage is `vfs` under `HOME`
    (overlay inside gVisor needs a memory-backed upper), so large images are slow to unpack; a kept box keeps its
    images. Conformance group `root`: a driver listing neither `root` nor `docker` must not give sudo (`sudo -n
    true` refused where sudo exists); one listing `root`, for every image the binding lends (config `images`):
    its boxes acquired with `net` `on` where the driver lists `net-off`: `sudo -n id -u` answers `0`, and
    root's `CapBnd` lacks `NET_ADMIN` (bit 12), `NET_RAW` (bit 13) and `MKNOD` (bit 27); with `docker`: a container runs from an image imported from the box's
    `docker-init`, so no network or registry is needed. `share` runs it in its quick groups. Not yet: `podman` and
    `nerdctl` (no `runsc-root`), Kata (expected, unmeasured: slirp4netns needs `/dev/net/tun` in the guest), a
    bridge network inside a gVisor box, Docker on Tart (a macOS guest has none).
  - **Usage.** When a box ends (zm's final status recorded), the usage module writes one immutable row per handle
    (`GET /v0/usage`, rows in the areas the caller reads): area, actor, target, size, image digest, end state,
    lease seconds from `acquire`'s answer to the recorded end (never past `expires_at`), the driver's spend, `billable_minutes` =
    ceil(min(lease minutes, the driver's minutes where its unit is time)) and `billed` (false for `reclaimed`,
    `lost`, or a box that never answered `acquire`). A repeated end report writes nothing; a failed write is
    retried, and a row still missing comes back when the module reconciles from run targets' ended handles (the
    whole record at start, then every 10 minutes from a day before its newest row). Area guests read their area's rows. Nothing a box was sent is recorded. Rates come later; the team
    pool is free.
  - **Area minute limits.** An area can have a sandbox minute limit per UTC day, ISO week or month: `[[box_minutes]]` in
    `project.toml` (`area`, `minutes`, `period`), listed by `GET /v0/usage/limits` (the areas the caller reads) and set by an
    owner or admin of the whole project with `PUT /v0/usage/limits/{area}` (0 removes it; an area's own admin cannot raise its
    own). Before a new box in the area, or the start of a stopped one, run targets ask the usage module, which adds the
    area's billed minutes for boxes that ended this period, boxes that ended in the last hour whose row is not written yet,
    and each live box's minutes so far; at the limit the box is refused, with the command that raises it, and minutes that
    cannot be read refuse too. A running box is not stopped, a box counts in the period it ends in, and two boxes asked for
    at one moment can both pass.
  - **Before a handle exists.** `GET /v0/run_targets/{target}/acquires/{request_id}` reads the caller's own acquire's
    progress; `POST /v0/run_targets/{target}/acquires/{request_id}/cancel` cancels it. A confirmed cancellation
    refuses reuse of that key; a ready acquire returns its handle to release. Drivers need `acquire-progress` to
    cancel; without it progress says starting. Driver messages for acquires carrying `jit_config` are omitted,
    and lending hosts relay only state and phase (API.md).
  - Clients: `zm sandbox create|ls|exec|renew|rm|stop|start|usage|spawn|output|kill|push|cp|shell|preview` and `zm job run` (`output --follow` pages until the
    process exits and exits with its code, or 1 for a process that ended with no exit; create places the box: `--on` takes a binding as
    named; otherwise every box binding describes itself and the available ones matching `--size` and `--os` are
    ranked by Potluck's one placement rule (`potluck/placement`): this machine's own, then trusted hosts', then
    ones whose driver charges per use, never a public host's; within each the smallest (fewest CPUs, then least
    memory), then name; a binding whose budget is spent is passed over and named for it; with `--net off` only a binding listing `net-off`; a refusal names each binding and why, and, when `--size` names
    a size no binding placement may pick states, the sizes they do (zm's own: `c1m2`, `c2m4`, `c2m8`, `c4m16`, `c8m32`)), and MCP's opt-in tools `sandbox_create`,
    `sandbox_exec` (output untrusted), `sandbox_list`, `sandbox_renew`, `sandbox_stop`, `sandbox_start`, `sandbox_delete`
    ("Kept disks"), `sandbox_spawn`,
    `sandbox_output` (32 KiB pages by default; untrusted), `sandbox_signal`, `sandbox_write_file`,
    `sandbox_read_file` (untrusted; "Files in and out"), `sandbox_preview_publish` (requires explicit
    `public:true`; a public HTTP application URL until revoke or lease end), `sandbox_preview_list`
    (untrusted), `sandbox_preview_revoke` (`handle`, `preview`) (`zm mcp --sandboxes`; `--sandboxes=auto`
    adds them only if the project has a box binding at start; `--sandboxes=only` serves only
    sandbox tools, allowing both plugins without duplicate default tools). The zm-compute plugin carries a `sandboxes` skill.
    Both use the run-target routes; there are no sandbox routes.
  - **Templates** (Celesto's coding-agent template): a template is a
    named box image, one of zm's own (`api.BoxTemplateImage`; today `agent`: `ghcr.io/sengirunners/potluck-box-agent`,
    potluck `cmd/potluck-driver-tart/images/agent`, built FROM zm's default Linux image so it keeps the runner and the box tools,
    adding git, gh, Node 22 with npm and corepack, Python 3 with pip and venv, ripgrep, jq, build-essential and the
    Claude Code and Codex CLIs; every version is pinned, the Node and gh tarballs also by SHA-256, the npm CLIs by
    version only, with no integrity pin). `potluck/hostprobe/images.pin`
    pins it on its own line, `oci linux <pin or -> runner_dir=/home/runner template=agent`: a line with
    `template=NAME` (lowercase letters, digits and `-`; one per layer, OS and name, beside the one plain line) is
    never a layer's default, and its name must be one the API knows (a test fails otherwise). potluck's `box-images.yml`
    builds, checks (each platform runs the box tools and `claude`, `codex`, `node`, `git`, `gh`, `python3 --version`)
    and pins it with the default, rewriting only the image of each line. `zm host share` and `zm pool host` without
    `--image` lend the default first and each published template image after it (one binding, its `images` list;
    an unpublished one, or one whose pull fails (a private package, a full disk, the network), is left out of the
    binding and its conformance and the result says so; only the default failing to pull stops share). Acquire's `template` (CLI `zm sandbox create --template
    agent`, MCP `sandbox_create`'s `template`) picks the binding's image of the template's name, any digest (a binding
    lent by another zm pins another build), its booted digest checked against that pin at the authority like any
    pin; a template and an image together, an unknown template, or a binding that lends none of it are refused by
    acquire (400; the last names `zm host share`). On a binding served from another machine the authority holds no
    images: the image name goes to the host's driver, which picks and checks the pin itself, by name, and refuses
    one its binding lacks. Placement with a template takes only a binding that lends it, those known to (on this
    machine) ranked before ones on another machine (whose images it cannot see); none lending it is `unavailable`,
    naming each and `zm host share`. No template: the binding's first image, as before.
  - **E2B gateway** (`potluck/cmd/zm-e2b`): a socket client like `zm sandbox` (no routes of its own) that
    lets the unmodified E2B SDKs run on boxes. Control plane: create (placement as `zm sandbox create`), get, list,
    kill, timeout and connect (a renew within `ttl_max_s`); a sandbox id is a hash of the handle. Data plane: it
    answers envd's `process.Process/Start` (Connect, JSON) and `GET`/`POST /files` itself with `exec` (no envd in
    the box), routed by `E2b-Sandbox-Id` and authenticated by `X-Access-Token` (an HMAC per key and sandbox).
    Output past exec's 1 MiB is `out_of_range` and a command past its deadline or 600 s `deadline_exceeded`,
    never a cut copy; network rules, pause, volumes, PTY and stdin streaming are refused, not dropped. API keys
    come from a `0600` keys file, each acting as the gateway's own identity or a zm token. Clients set
    `E2B_DOMAIN=<domain>:<port>`: `*.localhost` is loopback (RFC 6761) and the gateway signs a certificate per
    name from its own CA, name-constrained to the domain; one port serves TLS and plain HTTP (plain only on loopback:
    a non-loopback `--addr` needs `--tls-only`). A scanned box belongs to the key whose tag its request id carries.
- **GitHub runner broker** (`potluck/runners`; factory-as-zm-host plan D3, D4). On an
  authority whose `config.json` has `runners`, zm serves GitHub Actions jobs from a pool project's box bindings
  through one GitHub App. Nothing about a host or a team is built in: the App, the pool and the sizes are config.
  - **Config** (`runners`): `github`: `app_id`, `client_id`, `slug`, `private_key`, `client_secret` and
    `webhook_secret` (secret references, section 11; never values), `api_url`/`web_url` (default github.com);
    `pool`: `project` (a slug), `sizes` (optional: a size name to the one box binding that serves it, the
    owner's override of placement below) and `default_size` (the size a plain `label` job asks; unset: any, the
    smallest box that fits); `label` (default: the pool project's slug, so a pool `friends` serves `runs-on: friends`
    or `friends:<size>`, with or without `self-hosted`; an explicit label, `sengi` included, works the same);
    `public_url` (an https URL that forwards only the broker's five public paths, `/v0/runners/hook`,
    `/v0/runners/oauth` and `/v0/runners/app/{new,created,installed}`, to the daemon unsigned, e.g. Tailscale Funnel on just
    those paths or a reverse proxy, never the rest of the API; the zm tunnel's door (17.11) takes only
    member-signed requests, which GitHub and a browser cannot make); `listen` (a loopback `host:port` the broker
    opens serving those paths and nothing else; HEAD is refused where a state is spent). With `listen` set,
    `zm tunnel setup` adds cloudflared ingress for exactly those paths on the tunnel's hostname to `listen`, ahead
    of the door, so `public_url` can be `tunnel.url` (`zm runners setup` rewrites that ingress itself); `allow_fork_prs`
    (default false; a private repository's runs only, a public one's never); `job_ttl_s` (default 3600; never under three sweeps, 900 s): a job's time limit.
    Its box is leased this long and the lease is renewed by that much while the job runs (the sweep), up to the
    lending machine's own `ttl_max_s` and never past it. A job keeps the limit it was served with (migration
    `runners/0006_ttl`, two columns, one-way): a later `job_ttl_s` is the next job's. A job whose box expired under
    it says in its row that it was stopped at its time limit. `zm runners set <setting> <value>`
    (`POST /v0/runners/settings {"key", "value"}`, local, the pool project's owner; potluck PR 61) changes
    `label` (a name, or `default` for the pool's slug) or `job_ttl_s` (seconds, 900 to 432000): written into `config.json` as it is on disk and served from the next job on, with no restart.
    After a `label` change a recorded job that asks for the old label is not served (its row says so) and is never
    placed again without its size. Without `runners.github.app_id` every route below but `app` answers
    `unavailable`.
  - **Connected repositories** (potluck PR 77). A linked installation's repository is served only once the pool's
    owner connects it: `zm runners allow <owner/repo> [--public]` (`POST /v0/runners/repos`, local, the pool
    project's owner). The broker keeps the record (its `runners_repos`, by name and GitHub's repository id, so a
    repository that takes a connected one's name is not connected); no config is edited. A public repository needs
    `--public`, the owner's yes for that repository alone: its own branches are served (pushes, manual runs,
    schedules, pull requests from its own branches), a fork's run never. The broker reads a public repository's
    approval rule for outside contributors' runs at the yes and hourly, and warns when it is weaker than all
    outside contributors. `zm runners disallow <owner/repo>` disconnects one; `zm runners repos` (and `zm runners
    ls`) list them with any warning. GitHub is asked again before each runner, so a repository made public while
    its job waits is not served without the yes. zm only calls these routes; the rule is the broker's.
    **Upgrade step.** A pool that served before this existed (every repository of a linked installation, public ones
    under the old global switch) keeps serving only what the broker's migration (`runners/0008_repos`) connected: a
    repository with a job the broker took (not ignored) through a still-linked installation, and the broker deletes
    job rows after 30 days. A repository whose last job is older (a rarely run bench, say), or whose installation is
    no longer linked, is not connected and its jobs are not served. After upgrading, run `zm runners repos`, then
    `zm runners allow <owner/repo>` for each repository missing from it, with `--public` for a public one. A public
    repository the migration connected (`by` is `upgrade`) has no yes yet and is not served until `zm runners allow
    <owner/repo> --public`; `repos` and `ls` say so in its WARNING column and note the step under the table.
    `zm runners ls` also prints the checker's state (`check`: how jobs arrive, and per installation the last complete
    round, its errors and any cut), from `GET /v0/runners/setup` (potluck PR 78), and degrades to one `repos` line
    when the repositories cannot be read (a daemon not yet restarted into this version).
  - **Links.** An installation of the App serves a pool area only once linked: `POST /v0/runners/links`
    `{"installation_id", "area"?}` (admin of that area in the pool project) answers a GitHub authorize URL; the
    person signs in there and GitHub returns to `GET /v0/runners/oauth` (public, a single-use `state` bound to
    that request, 10 minutes), where the authority exchanges the code for a user token and goes on only if the
    person administers the installation's account (an active admin of its org, `GET /user/memberships/orgs/{org}`,
    which the user token may call only if the App has `members` read; GitHub's 404 there, not a member, is a no,
    and its 403 "Resource not accessible by integration" is an error page saying the App lacks Members: read and
    how to add it, never "not an admin"; for a user account, that account itself), then drops the token. The page then names the area and who asked,
    and the link is made only on that person's yes (`POST /v0/runners/oauth`, a second single-use state), so a
    link URL handed to an org's admin cannot bind their org to an area they did not choose. An installation
    linked to another area moves only for someone who also administers that area. The
    link mints identity `zm-runners` a `member` token capped to the pool project and that area (only its id is
    kept, never its secret); boxes are acquired as that token's subject through the run-target routes' own checks
    (`core.Deps.RunTargets`), so they are that area's and its budget and ledger apply. `GET /v0/runners/links` lists links in the caller's areas;
    `DELETE /v0/runners/links/{installation}` (admin) unlinks and revokes the token, and disconnects every
    repository connected through that installation (`zm runners unlink` says so; linking it again does not bring
    them back, each needs `zm runners allow`). An `installation` deleted on GitHub unlinks too.
  - **CI doctor** (`zm ci doctor [owner/repo] [--json]`, `GET /v0/runners/doctor`, local; potluck PR 72,
    [docs/ci-doctor.md](https://github.com/sengirunners/potluck/blob/main/docs/ci-doctor.md) there): the broker's numbered read-only checks of why a repository's jobs are not
    running, each failure with one next step; any failure exits nonzero. Without a repository it checks the
    connected ones and those whose jobs were refused. Workflow and waiting-job reads use the machine owner's `gh`
    sign-in (never a token over the API), within the broker's read budget a run; a missing sign-in is a warning, never a pass.
    The broker also runs it every 5 minutes on the connected repositories and tells the pool's owners once when a
    check starts failing (its `runners_doctor` table). zm only prints it; a daemon without the route says so.
    `--probe` (one repository; `--labels`, `--size`, `--os`, `--public` only with it) adds check
    18, Potluck's own `ci doctor --probe` run in process as for `zm ci pr`: it writes, never runs, a script that
    proves the path with a generated test job (pushed with the person's own `gh`); text mode prints the script as
    written, as Potluck's does. zm keeps no copy. A public or
    internal repository's yes is the owner's: without their `--public`, check 18 fails with no command in its
    fix and `--json` carries `ask` (kind `ask`, choices yes/no) for an agent to put to them. A prepared script
    is the person's step too: `ask` with its `command`, as `zm ci add`'s pull-request script.
  - **CI PR** (`zm ci pr <owner/repo> [--job <id>]`): Potluck's own `ci pr` (package `cli`,
    [docs/ci-pr.md](https://github.com/sengirunners/potluck/blob/main/docs/ci-pr.md) there), run in process with zm's home, so against this machine's zm daemon; zm keeps no copy.
    Flags, help, refusals and exit codes are Potluck's; human text says Potluck's command names as zm's, except
    the workflow diff and the pull-request script, shown as they are (`ci pr` prints no JSON). zm's autostart starts
    zm's daemon first and Potluck's never runs; with `ZM_HOST` set it refuses (the broker's routes are the socket
    owner's); zm's `--as` is dropped and `--project` is Potluck's `--pool`.
  - **Isolation** (`zm isolation [test <size>]`): Potluck's own `isolation` command (potluck
    [docs/isolation.md](https://github.com/sengirunners/potluck/blob/main/docs/isolation.md)), mounted the same way: per size and target this machine lends, whether it may run anyone's
    code and, for a shared size, its latest block test (`test <size>` runs it and records it under
    `$ZM_HOME/isolation/`; it boots sandboxes on the size's driver and waits for the machine to be idle). This machine's
    daemon only (`ZM_HOST` refused), started by zm's autostart as for `zm ci pr`.
  - **Compute MCP tools** (`zm mcp --compute`, opt in, untrusted; no shipped plugin passes it yet):
    `compute_status` (read-only) and `compute_doctor` (additive: its probe writes a temporary script) answer the same JSON and errors as `zm ci status --json` and
    `zm ci doctor --json` (one `orchclient` call each; failed checks are a result, not a tool error, where the CLI
    also exits 1). `compute_doctor` with `probe` (and `labels`, `size`, `os`; one `repo`) is
    `zm ci doctor --probe --json`, local only, with no `public`: the owner's yes comes back as its `ask`.
    `compute_setup` is `zm ci add --json` (same code, on the tool's client, which never starts a
    daemon; Potluck's autostart off): `{repos, mine, job, workflow, org, own_account}`, never waits, and a person's
    own step (a GitHub press, a public repository's yes, a pull request to open or merge) comes back as a step of
    kind `ask` for the agent to put to them, never taken. It has no `public`: that yes is the owner's own
    `zm ci add <repo> --public`. `compute_pause` (no input) is `zm lend pause --json` (same code, on the tool's client):
    `{state: paused|refused, sizes, refused: {reason: job_running|lend_running|admission, size, message}, next}`;
    a refusal changes nothing and is a result (exit 0), not an error. No `compute_resume` yet: resume is a lend
    (Mac password, image download), the owner's `zm lend resume`.
  - **CI status** (`zm ci status [--json]`, Stage 1 setup UX): a read-only front over the local
    broker's setup/jobs, its project's described machines, and scoped GitHub repository/run/job reads.
    JSON is the Compute repo-row contract: `{state,repos:[{repo,runs_on,job_limit_s,summary,issues,jobs,partial}],
    issues,partial}`. `state` is `not_configured` when the broker/App is off, `no_repositories` when
    the configured broker has no mapped or recorded repos, otherwise `configured`. `runs_on` is a complete
    YAML line using the broker's `runners.RunsOn` labels, with explicit OS and a size or alias when
    supported (a pool-only fallback follows the broker's default size), preferring macOS when offered
    (`darwin` becomes `macos`); it is empty with a reason when no serving machine is known.
    The line describes the workflow request, not a reservation: placement and admission stay with the broker.
    `jobs` retains the broker job fields and reasons; GitHub-only waiting jobs whose labels ask for the pool, read by the
    broker's own rule (`runners.ParseLabels`, whatever machines serve now), have `state: unseen`, `github_status`, a reason, `name` and `url`. Reasons distinguish environment
    protection/approval, pending concurrency, and jobs not yet in the runner queue; newly queued jobs
    get two minutes for webhook delivery before suggesting an App coverage check. Broker `failed`,
    `lost`, and `taken` exceptions remain in the summary after a newer `done` job. Text displays the
    same facts and the job limit; `runners ls` uses the same workflow line and `runners jobs` the same table.
    Repositories come from readable project/area GitHub mappings plus the broker's recorded jobs; a
    GitHub install alone does not prove a repo is connected. Each row is checked against the broker's connected
    list (`GET /v0/runners/repos`): a repo not in it gets the issue `Not connected in the pool areas you can read, so
    its jobs are not served: zm runners allow <repo> (--public for a public one)`, a connected one any `zm runners repos` warning (its
    settings, a public one with no yes, an upgrade row whose visibility is not read yet), and a list that cannot be
    read sets `partial` with an issue. A recorded repo without a mapping explicitly
    says unseen jobs could not be checked. GitHub reads use the daemon's login, never a second client login.
    Active runs are queried by status, so old waiting runs are not hidden by recent completed runs. Scans
    page up to the existing read door's bounds (300 runs per active status, 1000 jobs per run); broker
    history is the latest 500 jobs. A refresh makes at most 64 GitHub reads across all repos, including
    pagination, to bound work and cache pressure; the first repo rotates between refreshes so the
    tail is not starved. Empty status filters need no job reads or further pages; runs seen through
    multiple status filters are read once. Job status takes precedence over a run's overall gate.
    Reaching a bound, a failed/stale read, or an uncheckable repo sets
    `partial` and a visible issue. Neither text nor JSON may describe these as an all-clear. This first
    slice requires the broker machine's local socket for its configuration; remote setup/status remains
    future broker work. The broker's hints, errors and job reasons name zm's commands at the source
    (`runners.Service.Command`, set to `zm` in zm's wiring; potluck PR 81), so JSON, MCP and the panel say them too.
  - **CI add** (`zm ci add <owner/repo>... [--job J] [--workflow F] [--public] [--mine] [--org O] [--wait D] [--json]`,
    alias `zm ci setup`, Stage 2 setup UX): connects repositories in one re-runnable command on the lending
    machine's socket. It keeps no rule: it reads `GET /v0/runners/setup` (not set up: the step is the owner's ask `zm lend`, else `zm runners setup <pool> --repo R...`),
    `POST /v0/runners/setup` with `repos` (the install page pre-selects them, at most 20; when GitHub will not name one, again without) for the App's create or install page when no link covers the owner (people:
    printed, then `GET /v0/runners/links` polled until `--wait`; `--json`: no wait), `POST /v0/runners/repos`
    (a public repository without `--public` is an owner-only yes ask), `GET /v0/runners/doctor?repo=` (its
    first failed check, or an unavailable check 1 to 5, is the step; a fix that changes this Mac or its broker is the owner's ask), and Potluck's own `ci pr --json`
    (its JSON fields, never its stdout text; a refusal is read from its error), for `--job` or else the one job on a GitHub-hosted macOS
    label (no workflows, none or several: only noted; any other failure is the step), whose script opens the pull request with the person's gh sign-in. Says what it will change first; the last line is `done` or one
    `next:` step (exit 1). `--json` answers `{state: done|needs_human|blocked, repos:[{repo,state,changed,
    notes,next}], next}` with exit 0; a step is `{kind: ask|run|info, say, url, command, choices}`: an ask is
    a person's own press or yes, never taken by an agent; `info` is a block with nothing to do yet (the App is
    private, so once linked to one account other owners' repositories are blocked).
  - **Jobs.** `POST /v0/runners/hook` (public) takes the App's deliveries, `X-Hub-Signature-256` checked against
    `webhook_secret` before anything else (401), at most 5 MiB, de-duplicated by `X-GitHub-Delivery`, answered
    202 once durable. Workers apply them in two lanes, oldest first: `queued` (a serve waits for a box to boot)
    on 3 workers (`Serves`), never two on one job, and `in_progress`, `completed` and `installation`, which end
    boxes and so never wait behind a serve, on one, in arrival order. A `workflow_job` `queued` whose labels
    name the pool's label (plain or `<label>:<size>`) is the pool's; with anything but that, `self-hosted` and
    GitHub's OS (`linux`, `macos`, `windows`) and arch labels it is ignored with why (a job without the pool's
    label is another runner's, not recorded). It is served when its installation is
    linked, its repository is connected (public only with the owner's yes), GitHub has not already finished it, its run
    is of that repository, and the run is not from a fork (the run's head repository is the repository) and was
    started by an event only someone who can push causes (`push`, `pull_request`, `workflow_dispatch`, `schedule`,
    `merge_group`, `release`, `repository_dispatch`; never `pull_request_target`, `workflow_run`, `issue_comment` and
    the like, which act on outsiders' input) unless `allow_fork_prs`; a check GitHub does not answer fails closed
    (retried). Else it is ignored and recorded with why. Served:
    the broker mints a JIT config on the job's repository (repo-level for an Organization installation too: an
    org runner sits in a runner group every repo of the org may use, so a refused job of another repo with the
    same labels could take it), named `zm-<job id>`, with the job's labels, and acquires a box placed as below with request id
    `gh-<job id>`, `ttl_s` `job_ttl_s` (default 3600), that `jit_config` and a `jit_guard` of the job's
    repository with the trusted events above (none with `allow_fork_prs`, which checks neither event nor fork)
    (so a redelivery makes nothing new); the sweep renews it while the job runs. A job's k-th runner (a refused
    box, a takeover, a lost box) is `zm-<job id>-<k>` with request id `gh-<job id>-<k>`.
    **Placement**: a size in `sizes` goes to its binding alone; else the job may go to any box
    binding of the pool project, its own or a host's (`<binding>@<machine>`, "Across machines"), whose host is
    `trusted` (a `jit_config` never goes to another host: placement skips one and the acquire refuses it,
    "Host class"), whose `describe` lists `github-jit` and `github-jit-guard`, says it is available, and matches
    the job's size and OS (`linux` when it names none, as GitHub's own runners; `macos` is a `darwin` box;
    arch labels are not checked: no describe states one), whose lifetime cap (`ttl_max_s`) holds `job_ttl_s`,
    and that did not refuse a box in the last 2 minutes (so a binding that says available but refuses costs at
    most one runner minted and deleted per 2 minutes, however many jobs wait).
    They are ranked by the same placement rule as sandboxes (own, trusted, then paid; within each the smallest,
    fewest CPUs then least memory), then the one serving fewest broker jobs, then by name, and up to 3 are tried in turn, the runner deleted after each
    refusal. When none fits or takes it, the job is `waiting` with why (each box and why not, drivers quoted)
    and tried again at most 5 minutes apart (sooner once a served job completes, except a serve backing off
    after an error) for as long as GitHub has it
    queued (asked at most every 15 minutes; a `completed` delivery ends it at once; a day at most), never
    dropped after a number of tries; a delivery fails out after 6 failures in a row of anything else (a check
    GitHub does not answer), its job `failed` with why. While an installation's API budget, from GitHub's
    `X-RateLimit-*` headers, is at or below 10% of its hourly limit (at most 100 calls), a waiting serve makes
    no call until the reset. **A box lost before its job started**: every 30 s the broker asks each served
    job's box (run-target status, no GitHub call while the box is there); one the host took or lost (`reclaimed`
    by its Mac's owner, `expired`, `lost`, or the host's marked answer that it has no such handle; a normal end, `ended`, is never a loss: the box's own process finishing, closed by the job's `completed` delivery) or whose host has not listed it for 5 minutes is, once GitHub says
    the job is still queued (one call) and its runner is deleted (GitHub refuses a busy one: a job that did
    start there lost its box while it ran, next), released in the background and the job served again, as after a
    takeover. **A box lost while its job ran** (potluck `runners/lost.go`; the full rule is potluck's
    [docs/daemon.md](https://github.com/sengirunners/potluck/blob/main/docs/daemon.md)): GitHub cannot move a job off a runner that vanished (it fails the job after its
    lost-communication timeout, about 10 minutes, has no call that cancels one job, and re-runs a job only once its
    whole run has completed). So when the host itself says the box ended (`reclaimed`, `expired`, `lost`) or that it
    has no such box (a status `not_found` marked as about the handle itself, which is the only `not_found` that counts: any other, such as a host that is not in that project, leaves the box's record as it was and answers unavailable; a normal end, `ended`, is never a loss, and the status of an ended handle answers its record without asking the host), the broker asks GitHub about the job that runs on it (the one that took the runner, recorded at
    `in_progress`) and records the loss only while GitHub still has it `in_progress` on that runner; a job that
    reported a conclusion first is never re-run, and a host that is only unlisted or silent has said nothing (the job
    keeps its box). Once GitHub ends the job with `failure` and its run has completed with `failure`, the broker asks
    GitHub to re-run it: the job alone when it is the run's only lost job, else the run's failed jobs. Only while the
    run's `run_attempt` is below 3 (two re-runs at most; a person's re-runs count). The job's row and the daemon's log
    say what happened. Re-running needs the App's repository permission Actions: write; without it the loss is
    recorded and logged with what to add, and the job stays failed. This came with migration `runners/0005_lost`,
    which is one-way (the swap preflight names it: fix forward, never back to an older build on the same home). Every job's state, why and box are listed by
    `GET /v0/runners/jobs` (newest first, the caller's readable areas; `zm runners jobs`). GitHub still gives a repo's
    runner to any job of that repo with its labels, so a job the broker refuses there (a fork's run, a
    `pull_request_target`) can take it: the box's hook stops it before its first step. The repository's
    visibility and link are the served job's, so the hook checks the rest; it cannot see labels, so a job of
    a trusted run without the pool's labels passes it and runs until its box is released. A job the broker
    would serve, of any run, passes. `in_progress` is checked against the same rules: a runner of a serving
    job taken by a job they refuse (or cannot check) has its box released and the runner deleted at once, and
    its job is served again in the serve lane while GitHub still has it waiting (runner `zm-<job id>-<n>`,
    request id `gh-<job id>-<n>`, at most 3 serves per job; then `failed`); a job they would serve taking it
    is a swap, left alone. A box whose job ended or was taken while it started is released as `acquire` answers, and its runner
    deleted. `completed` releases the box the job ran on (the one whose runner it took) and deletes that runner; the
    job that runner was minted for is served again if GitHub still has it queued, and the completed job's own box
    goes too unless another job runs on it; for a pool job not yet served it records the
    job done, so a serve still checking it mints nothing. Every installation token the broker mints names one repository, the job's, and only
    what its call needs: `actions` and `metadata` read for the checks, `administration` write for its runner
    (repo-level registration: GitHub has nothing narrower), `actions` write for a re-run; `members` read is used only by a linking person's user
    token, never in an installation token.
    Every 5 minutes a sweep releases boxes whose job ended without a delivery and deletes
    idle runners named `zm-<digits>[-<n>]` that no live job holds; registration, deregistration and the sweep never run
    on a host.
  - **The App.** `POST /v0/runners/app` (owner of the pool project; `{"org"?}`) answers a one-time page URL
    (24 h). The page posts GitHub's App manifest (`administration` write, for per-repository runners;
    `actions` write (read for the checks, write to re-run a job whose box was taken back) and `metadata` read; `members` read, for the link's org-admin check (an organization permission: an
    installation on a user account gets nothing from it); never `organization_self_hosted_runners`; event `workflow_job`, webhook `public_url` + `/v0/runners/hook`, callback
    `public_url` + `/v0/runners/oauth`, private) to github.com; GitHub returns to `GET /v0/runners/app/created`
    (public, single-use `state`), which converts the code and writes the key, client secret and webhook secret
    under `$ZM_HOME/secrets/runners/` (0600) and the ids into `config.json` (re-read first: edits made there since
    the daemon started are kept). No person copies a secret. The manifest's `setup_url` is `public_url` +
    `/v0/runners/app/installed`, and the created page links to the App's install URL with a new single-use
    install `state` (1 h) bound to the owner who asked and the request's `area`; `POST /v0/runners/links`
    without `installation_id` mints one too (`install_url`). After an install GitHub returns there with
    `installation_id`, a claim anyone could type: the route spends the state only if the id is a number and the broker is set up,
    checks it is this App's installation and not linked to another area (the page does not name it), then answers a 303 to GitHub's authorize URL
    with a link state for that installation, area and requester. From there it is the link above: the
    sign-in, the admin check of the installation's account and the person's yes decide. No id is copied. An App
    whose "Request user authorization (OAuth) during installation" is on (GitHub's button reads "Install &
    Authorize") has no setup URL: GitHub returns to the first callback URL, `GET /v0/runners/oauth`, with
    `installation_id`, `setup_action`, its own `code` and the install URL's `state`. Without a link state such a
    return is handled exactly as `setup_url`'s (the same checks, the same 303 to a sign-in zm starts; the person
    has authorized already, so GitHub comes straight back with a code for that link state). GitHub's install-time
    code is never exchanged, and a return with no live install state links nothing and mints nothing: it names
    `zm runners link <installation-id>`, whose sign-in and yes decide.
  - **Setup from zm**. `zm runners setup <pool> [--url https://host] [--org] [--name] [--area] [--repo R]...`,
    the pool project's owner on the machine's socket (`POST /v0/runners/setup`, local): writes `runners`
    `pool.project`, `public_url` (the flag, else the one set, else `tunnel.url`; neither is an error naming
    `zm tunnel setup`; once the App exists a different `--url` is refused, since GitHub calls the App's URLs) and `listen` (the one set, else a free `127.0.0.1` port) into `config.json` as it is on
    disk, opens that listener live (no restart; a changed address replaces the old one), and, when
    `public_url` is the hostname of this home's named tunnel, rewrites cloudflared's ingress for the broker's
    paths and restarts it only if it changed (a quick tunnel cannot route paths: refused). It prints one URL:
    the App's creation page, or with an App the install URL. `zm runners link [<installation-id>] [--area]`
    prints the install URL, or for an existing installation GitHub's sign-in URL (an App made before `setup_url`
    needs it set on GitHub; the CLI says so); `zm runners unlink <id>`;
    `zm runners ls` shows the pool, `public_url`, the listener, the App, the settings (public repositories, the
    job limit), sizes and links. Sizes are not set by
    setup. Everything is checked before anything changes; a new pool drops every pending state. A `public_url`
    is https with a host and no path. A taken or bad `listen` at daemon start leaves the broker running without
    its listener (logged), so `zm runners setup` can pick a free port.
- **Scripts.** A run's `script` is a name the target resolves (an npm script, a make target), never a shell
  string, and `args` are literal arguments. That is hygiene, not protection: the code at `commit` defines what
  the script does, so the protection is the routing above.
- **Trust boundary.** A target is code run with the daemon's UID: it is **not a secrets boundary** (it can read
  whatever the user can), and binding one trusts its code. It gets section 11's environment allowlist (`PATH,
  HOME, LANG, TERM, TZ`) and nothing more; `env = "inherit"` does not apply to run targets. A credential reaches
  it only through a `*_file` config key naming a 0600 file it reads itself (section 11; the Secrets brick when
  it exists); a secret value in a binding is refused. Its output is data, never instructions. Drivers are
  plugins, never linked into the daemon.
- **Host class.** A target whose driver runs on another machine (a lent Mac, a sandbox host) takes that machine's
  operator class (section 8). On a `public` host the effective class is `credentials: false` whatever the binding
  or `describe` says (the stricter wins, as above), so it serves only untrusted-capable work; no `*_file` key or
  other secret is ever sent to it, and the only credentials it gets are minted for one lease and die with it (a
  GitHub JIT runner config, a zm token capped to the lease). A renter's own secrets on a box `exec` ("Boxes",
  Secrets) go only to a `trusted` host. A credentialed or unisolated binding on a public host
  is refused at bind. A target on this machine is the owner's own, so trusted. For a box served from another
  machine ("Across machines") the authority asks `core.MachineClasses`: on a `public` host an `exec` with
  `secrets` and an acquire with `jit_config` are `403` (the message names neither a secret nor its name), and
  no `*_file` config is ever sent (a remote binding carries none).
- **Across machines.** A host's box bindings are machine-level: `$ZM_HOME/run_targets.toml`
  (owner-only writes, mode 0600, through `POST /v0/run_targets` with `host`; `zm target bind --host --share
  <project>`, `zm host share --pool <project>`), kind `box` only, no repo, the same validation as a project
  binding plus `share = ["<team project id>", ...]`, after a first line `format = 2` (Potluck c7ac0ef,
  a bind may name a slug of a team project this machine is in, stored as its id). A file with
  no `format` line (and a `host_handles.json` ledger with no `version`) is slug-keyed: once teamlink has loaded
  the joins (it starts before run targets), the daemon migrates both, freezing slug to id in
  `$ZM_HOME/host_keys.json` (0600, written once) and marking it done in `registry.db` (table `run_host_keys`);
  until then, or while it refuses a restored file it cannot place, binds and relayed calls answer `503`
  (potluck [docs/daemon.md](https://github.com/sengirunners/potluck/blob/main/docs/daemon.md) "Moving from slug-keyed files"). A hand-written file needs the `format = 2` line and
  project ids. An older zm refuses the files this one writes (the `format` line, the ledger's `version`).
  The host replica's heartbeat for project P carries
  `run_targets`: each binding shared with P and its `describe` (effective class, size, `available`; refreshed
  by a background ask at most every 20 s), and the authority keeps the latest set per machine in memory,
  dropping a machine's after 60 s without a beat. It keeps them only from an enrolled host: a `trusted` machine (a
  member's by default) or one whose class the owner set (`public`: a lender the owner named); a guest's or area
  member's machine by default lends nothing, and only status, release and list are relayed to one (a host
  that is no longer enrolled gets no box, command or renewal; its boxes can still end). Placement (`zm sandbox create`)
  never picks a box on a `public` host unless it is named (`--on`). It ranks every binding that fits, and `zm sandbox
  create`, `zm job run` and MCP `sandbox_create` ask them in turn with one request id (Potluck #161, #177): they pass
  to the next only on a refusal the daemon marked as made before it recorded the request (unavailable, handles_max,
  budget, lifetime) or a binding that does not fit the call (a `--timeout` past its lifetime cap; `--dirty`, or
  `--ref` with dependency layers, on one that checks out refs itself); any other failure stops at that binding, so at most one box
  is ever made. When every binding passed, the error names each one's reason. At the authority, `GET /v0/run_targets` lists them as
  `<binding>@<machine>` (path-safe; handles `box:<binding>@<machine>/<id>`) and every view carries `host` (the
  serving machine; this one for a local binding) and `host_class` (`trusted` or `public`, from the authority;
  local is `trusted`). describe answers from the advert. acquire, status, release, renew, exec, spawn, output, signal and input on such a name
  behave as for a local box (rows, areas, holder checks, the sweep, the end hook, all at the authority) except
  the driver call is a machine op `run_target` (`{binding, verb, id, args}`; section 8's ops: recorded before it
  runs, run at most once; one not taken within a minute never runs). A host that lends boxes to the project holds
  `GET /v0/team/ops` open at the authority (a long poll like `/v0/sync`, at most 25 s per request, outbound only,
  so over the tailnet and the tunnel alike), answered as soon as an op is queued for it, and posts each result at
  once (`POST /v0/team/ops/report`); heartbeat answers and beats carry both too, the fallback while the poll is
  down. The host's run-target module checks the binding exists and is shared with that project (else
  `not_found`) and calls the driver exactly as a local call (env allowlist, limits, error mapping); `list` is
  answered only for the `request_id` the authority names. The authority waits the verb's limit (exec:
  `timeout_s` + 30 s; output: `wait_s` + 10 s, at most 30 s; input: 15 s) plus 20 s; no answer is a driver timeout (acquire retries with the same key, then
  reconciles; never acquires afresh). A host not heard from in 60 s (no beat) is `unavailable` at once, not waited on. The effective class is the stricter of the advert and `describe`. Arguments
  (an exec's argv, stdin, cwd, secrets) and answers are held in memory for the one call, never written to the
  authority's database; a relayed call is not end to end: the authority's operator can see an exec's argv, stdin,
  cwd and output (and its secrets as they pass through); the host blanks its report once an answer took it. The host keeps each answer within
  3 MiB as encoded (an exec's output is cut to fit and `truncated`, a base64 one in whole quanta; a relayed
`output` page is cut to 256 KiB at the host, and its `*_next` says where to go on; any other answer over it is an error), and a
  heartbeat carries at most 4 MiB of reports (the rest go with the next beats). The host's node-bound token is the
  only credential; exec is proxied through the authority, never direct. A relayed call adds tens of milliseconds
  on a warm or a cold host (measured on one machine: about 15 ms over the driver's own exec); through the
  heartbeat fallback it waits for a beat. The host keeps a handle ledger (`$ZM_HOME/host_handles.json`, 0600): each handle its drivers
  issued through a relay is recorded, on the acquire answer, against the project whose authority acquired it
  (kept until untouched for 30 days, far past the 24 h an ended handle is kept); status, release, renew, exec, spawn, output, signal, input and
  list from another project's authority answer `not_found`, as for a missing handle. The host sends its driver
  `<project tag>-<key>` (the authority's key behind a tag of the project), so two projects sharing a binding never
  meet on one key; an acquire answered with a handle the ledger has for another project is `409` and that box is
  left alone. A ledger that cannot be read fails closed (`503`, never rewritten). A member's own machine
  reaches all of this through its socket: the team proxy forwards `/v0/run_targets` and below for a joined
  project (by `-P` or the cwd) to its authority, waiting the verb's limit plus the relay's slack (exec: `timeout_s` +
  60 s; acquire: two tries and a reconcile, 340 s), not its 30 s call timeout; bind and unbind are refused there (`400`, "bound on the machine
  that serves them"; `zm target unbind` of this machine's own binding, bare or as `<binding>@<this machine>`, then names `zm target unbind --host <binding>`), and `?host=1` and a bind with `host` stay local. Not yet: image choice on a remote box (a `ref` there is the renter's push: "Code from the renter"),
  placement across hosts, runs across machines, the authority's own pin of a remote box's image
  (the host reports its digest and is believed), a relayed call past Cloudflare's 100 s limit over
  the tunnel (the answer is one signed body: a long command there is a background process, whose calls each
  answer within 50 s).
- Prefer existing tools inside a target (Postgres template copies, official images, a runner provider's own API)
  over provisioning code of our own.
- **Built in zm:** `potluck/runtarget` and `zm target`: owner-only bindings (API.md
  "Run targets"), describe, acquire with the routing, class, local-only, allowlist, cap and budget rules above,
  status, release (with the final status), renew within the lifetime cap, idempotent `request_id`, per actor (zm sends the driver `<actor tag>-<request_id>`; a repeat
  sends the commit the first resolved), handle records in `run_handles` (spend never falls; reclaimed never
  billed; a budget first refreshes the binding's live handles, so it may overshoot by what runs spend after acquire). A member reads a binding without its driver config or `create_in` (the owner's). A `scripts` list is enforced on every run binding that has one, not only on guarded ones. Bindings
  live in a project this machine holds (a solo project or the authority's). A deadline sweep (at start and every 30 s)
  asks status once a live handle is 5 s past `expires_at` (its final state and spend), records it `lost` when its
  binding is gone or the driver cannot answer 15 minutes past the deadline. The sweep also renews a handle held by a live
  session (a third of its TTL left, at most once a minute, idle or not; never past the lifetime cap; not once the
  session ended), an expired or reclaimed handle publishes `dev.zm.run_target.expired` (a `RunTargetLease` keyed by the
  handle, holder the actor) and nothing else, the lease verbs refuse `db:`, `run:` and `box:` keys, and the
  owner's bind with `setup` runs the driver's `setup` (a dry run, then the real one) before binding. Not yet:
  handles listed among `zm leases` (they are in `zm target status`). Boxes: bind and acquire rules above, the
  `exec` route for the holder, `images` pins sent to the driver as config `images` (comma-joined), a booted digest
  off the pin released at once; and `rt.Service.OnEnd` (potluck/runtarget/rt, reached as `rt.From(d)`), called once per handle (the one write that
  ends its row, after its final status; each subscriber on its own goroutine) for the sandbox ledger, which reconciles a failed write from
  `EndedSince(project, since, limit)` (ended handles, oldest end first); a handle's
  `acquired` is when acquire answered. A handle records the area its acquire resolved (`area`; a pool team is an
  area): lists, status, release, renew and exec answer it only to callers who may act in that area, others as missing. The authority forwarding to a box host's replica, with section 8's
  host-class refusal: "Across machines" above.
- **The harbor project's drivers do not speak this yet.** `scripts/scratch-db.sh` (db: `local` template copy, `farm` via
  farm-db, `url` = any Postgres) and `scripts/remote.mjs` (run: `local`, `farm` via hub run, `ci` not enabled; `zm-run-farm` below supersedes its farm backend)
  are close in shape but need real changes before conformance: an async `acquire` (remote's blocks until the
  run ends); running a clean checkout of `commit` (remote's backends run the caller's working tree today);
  `status`, `renew` and `list`, and `request_id`; section 11's error envelope (a failure exits non-zero with
  stderr today); remote's outcomes `owner-gone`, `killed`, `not-started` and unknown mapped to the states above;
  scratch-db enforcing its own TTL, with a default one; `conflict` for an existing name; and a
  `zm-run-<driver>` wrapper.
- **`zm-run-farm`** (zm's own, `cmd/zm-run-farm`): a run on a machine of the team project through section 8's
  script runs (`zm exec`), so no ssh and no hub. Class `isolation: "none"`, `credentials: true` (the machine's
  user runs it), so only the remote head of the binding's branch goes to it, and the machine itself refuses a
  `commit` its `ref` no longer resolves to (`forbidden`). Config: `project`, `machine` (a name or `auto`),
  `ttl_max_s`, `default_ttl_s`, `state_dir`. `actor` must be a live session: the run is its task. `acquire`
  answers once a machine has taken the run (one not taken in 100 s is withdrawn: `unavailable`); the machine
  enforces the deadline (`exit: 124`). No `renew`; spend is not reported.
- **`zm-run-local`** (zm's own, `cmd/zm-run-local`): a run on the machine whose daemon calls it, as that daemon's
  user, for credentialed jobs that need that user's files (the automations home's): no zm task and no session, so
  any socket caller's acquire works, and `zm-run-farm`'s refusal of a run on its own machine does not apply. Class
  `isolation: "none"`, `credentials: true`. Config: `remote` (the binding's repo as a git URL; required), `state_dir`
  (default `~/.zm-run-local`; one directory per binding), `ttl_max_s`, `default_ttl_s`. A bare mirror of `remote`
  fetches `ref` and refuses a `commit` that ref's history there lacks (`forbidden`); the run is a detached
  checkout of exactly `commit`, the script an executable file in it, run by a detached supervisor with the
  allowlist, stdin closed and its output in `log`; the supervisor enforces the deadline (`exit: 124`), removes
  the checkout, and a supervisor gone without a verdict makes the run `lost`. Liveness is a lock per run, never a bare PID. No `renew`; spend is not reported.
- **Host onboarding (`zm host doctor`, `zm host share`, `potluck/hostprobe`).** Any machine's owner lends it as box targets in two commands, with nothing host-specific in zm: facts are detected at run time or given in flags. `doctor [--share 0.5] [--state-dir] [--json]` reports OS, arch, cores, RAM, free disk, KVM, and each layer (`incus`: incus plus KVM; `oci`: docker, podman or nerdctl with a Kata or runsc runtime, where runc, crun and youki are never counted as isolation; `tart`: macOS with tart and Virtualization.framework) with its problems in plain words; the sizes (`c1m2`, `c2m4`, `c2m8`, `c4m16`, `c8m32`: vCPU/GiB) where two VMs fit within `--share` of the RAM and cores (default 0.5; `--max-cores` and `--max-ram-mb` cap it further), none below the floor of the best layer's default image, exactly what `share` lends without `--sizes` or `--image` (`hostprobe.Lend`); and every pending root step as one `sudo sh -c '...'` line. On Linux with a container CLI but no isolating runtime (Debian and Ubuntu carry no `kata-containers` package, and Ubuntu's `runsc` is a 2023 or 2024 snapshot), that line installs gVisor from its own release channel, pinned in `potluck/hostprobe/gvisor.go`: one point release, `<yyyymmdd>.0` (never `latest`, and never the bare date, which is that date's latest point release), per arch (`amd64`, `arm64`; another arch gets a problem and no step) the URL `https://storage.googleapis.com/gvisor/releases/release/<yyyymmdd>.0/<x86_64|aarch64>/gvisor.tar.bz2` and the SHA-512 its `.sha512` publishes, written into zm. In one step it downloads into a fresh `mktemp -d` (curl `--proto =https`, else wget), checks that SHA-512 (`sha512sum -c`), and only then unpacks into `/usr/local/bin` `gvisor-bin/` (the sidecars runsc needs) and `containerd-shim-runsc-v1`, then `runsc` last; the temporary directory is removed on every path, and a failed download or check unpacks nothing and stops the script. Never a script piped into a shell. curl or wget and bzip2 come first from the distribution when missing. With docker it then registers that runsc (`/usr/local/bin/runsc install --download-sidecars=NEVER`: gVisor merges runtime `runsc` into `/etc/docker/daemon.json`, keeping every other key, backing the old file up as `daemon.json~`, replacing a `runsc` entry docker never loaded, and downloading nothing) and reloads docker (`systemctl reload docker`: dockerd re-reads its runtimes on SIGHUP, so running containers keep running; never a restart), waiting up to 10 s for `docker info` to list it; podman and nerdctl find `runsc` on the PATH. A runsc already on the PATH that docker does not list is never registered as it is (it may be the user's own, writable without root, or lack the sidecars its `runsc install` would fetch unpinned): the same pinned install replaces it in `/usr/local/bin` and docker gets that one. Kata, when installed, is used as before; zm never installs it. Two, because `zm-run-tart` runs two boxes at once per guest OS on a machine by default (`linux_slots`, `macos_slots`; Apple's licence caps macOS guests at two per Mac), counting every live box of its state directory whichever size's binding made it, so two of the largest size lent may run together. When the owner gave no `--share` (caps alone keep the default) and no size fits twice inside the default half (a 4-core, 7 GB Linux machine fits one `c1m2` there, not two), `share` lends the sizes that fit once inside it, none below the image's floor, and binds each with `linux_slots=1` and `macos_slots=1`, so the driver runs one box at a time across the bindings it makes and they never take more than the default half (a binding of another size left from an earlier run keeps its own slots); the result says so in one line, and doctor's line reads `sizes (one at a time: none fits twice inside 50%)`. Conformance runs without those keys (its peers check runs two boxes at once; two that fit once inside half fit inside the whole machine). Not when sizes fit twice but none clears the image's floor: there two boxes of a size that fits once could take the whole machine during conformance, so that stays refused. An explicit `--share`, even 0.5, keeps the rule that two fit: nothing fitting twice is refused, as before. `share [--image name@sha256:<64 hex>]... [--layer] [--sizes] [--name-prefix] [--oci-runtime] [--oci-cli] [--net-allow] [--net-forbid] [--net-mail] [--net-mbit] [--runner-dir] [--docker] [--admission-url] [--admission-token-file] [--admission-ttl-s] [--full] [--reclaim-idle-s] [--pool] [--no-sudo] [--dry-run]` runs the root step it needs itself (`doctor` stays a read-only report that prints it), once its own refusals have passed (`hostprobe.Precheck`: share's dry run, and when that stops at a layer that is not ready, its dry run again with that layer as the root step would leave it), so zm never asks for root and then refuses: doctor's root steps for the layer it uses (`--layer`, else the best available, else each supported one), less zm's network's when no box would serve a runner on it (`--runner-dir none`, an image with no known runner, or the owner's own `--oci-network`). When there are any and stdin and stdout are a terminal, it prints one plain sentence of what and why (`zm needs admin rights once to set up the firewall its VMs run behind; sudo may ask for your password`), runs that very script (`hostprobe.RootScript`, which `RootCommand` quotes for doctor) as `sudo sh -c <script>` on the terminal, so sudo asks for the password itself and zm never reads, sees, stores or passes it, then reads the machine again and goes on as if the owner had run it. Not at a terminal, without sudo, with `--no-sudo`, in a dry run, or when sudo does not finish (a wrong password, a failed step), it says so in plain words and goes on as before: it prints the root line and lends jobs only, or refuses. Ctrl-C at sudo's prompt reaches sudo from the terminal (zm does not kill it, so sudo restores the terminal's echo) and stops zm, as anywhere. `share` lends, without `--image`, zm's default image for the chosen layer (`potluck/hostprobe/images.pin`, embedded in zm so its pins update with zm; one line per layer and guest OS, each a full digest or `-` while zm's own image is unpublished, and a test fails on any other; `tart`: Cirrus Labs' macOS base image, `oci`: zm's Linux image from potluck `cmd/potluck-driver-tart/images/linux`, built and pinned by potluck's `box-images.yml`; a layer with none is refused in plain words naming `--image`; a line may carry the image's floor, `min_cpu=N min_memory_mb=N` (a Tart image's `cpuCountMin` and `memorySizeMin`, below which `tart set` refuses), and no size below the floor of a known image, default or `--image`, is lent or accepted in `--sizes`; the pin is checked at acquire like any other); refuses an image not pinned by digest, a layer that is not ready (printing the one root command), or a named OCI network (`--oci-network` other than `none`, whose filter zm cannot read) without `--net-mail`, before running anything (and leaves such a size unbound unless a mail check passed: all SKIPs prove nothing); then pulls each image through the layer's CLI and, per size, runs `potluck-conformance <driver> --kind box` on the box config (`vm`, `size`, `cpu`, `memory`, `images`, and for oci `oci_runtime`, `oci_cli`, `oci_network`), with the box network targets (`--net-peers`, the owner's `--net-allow`, `--net-forbid` and `--net-mail`, and a `--net-forbid` for each of this machine's own addresses but loopback and IPv6 link-local, on a port share listens on during the runs, so a pass means blocked rather than closed), and binds `<prefix>-<size>` (kind box, ephemeral, credential-free, the images pinned; the prefix defaults to this machine's zm name) only if it passed. A failed size stays unbound and is reported. By default the check is quick: the first size runs `--only handles,lifecycle,refuse,net,root` with the network targets (every security check: a new box sees nothing of the last, renter secrets and their refusals, refused images, shell strings and foreign handles, peers, this machine's addresses, `--net-forbid`, outbound mail; and, for a box listing `root`, that root has no `NET_ADMIN`), and each other size `--only boot` (one box of that size boots, answers its size and exec, releases; `boot` runs only when named); when the first fails, no size binds. It drops `renew` and `expiry`, which wait out short leases and are the driver's own code on every size (`code`, `ended` and `reclaim` are SKIPs for a share anyway). `--full` runs the whole suite, network included, on every size. Measured on the reference driver: 3 sizes in 3.8 s quick, 44 s full. On macOS the binding also carries `reclaim_idle_s` (`--reclaim-idle-s`, default 300; 0 lends all the time; refused elsewhere): a box ends as `reclaimed` (never billed) within about a second of the owner's keyboard or pointer input, and none starts until the Mac has been idle that long. Conformance runs without it, since the owner is at the keyboard. `--dry-run` prints the plan. A layer zm-run-tart cannot serve yet (`incus`, for now) is reported by `doctor` but never bound: `hostprobe.Supported` is the one place to flip. `share`'s binding carries the pins as its `images` only, never also as a config key `images` (the daemon sends them to the driver as config `images`; a machine-level binding with both could not be read back from `run_targets.toml`). A box serves GitHub Actions jobs (capability `github-jit`, so the runner broker can place a job on it) only with `runner_dir`: `share` binds it from the image's pin (`runner_dir=PATH` on its `images.pin` line: `actions-runner` for Cirrus Labs' macOS base, `/home/runner` for zm's Linux image; potluck's `box-images.yml` rewrites only the digest), or from `--runner-dir PATH` for an image zm does not pin (`none`: no runner), and never guesses: an unknown image without the flag binds no runner and the result says to pass it. A re-run computes the same `runner_dir` from the pin or the flag, so a re-run with the same flags keeps it (a hand-edited one is replaced). On oci a runner must reach GitHub, which a box on network `none` cannot (`zm-run-tart` refuses `runner_dir` with `oci_network` `none`): on an oci binding with `runner_dir`, a box whose acquire brought a `jit_config` is made on the binding's `oci_network` (any `jit_config`, with or without `jit_guard`: the guard is the caller's own, its shape all the daemon checks, so requiring it would stop no renter and would cut off a renter's own runner, which the `jit_config` rule above allows; what bounds a renter's networked box is the network's filter and that a `jit_config` is taken only on a `trusted` host), and so is every other box (a sandbox, a job) acquired with `net` `on`, which zm sends only for a `trusted` host's box ("Boxes", Network); a box acquired with `net` `off`, or with no `net` (an older daemon), is made with network `none`. Either way a runner binding's checkouts are fetched by the host (`fetch=guest` refused), and describe's network says so (`potluck-box (filtered by its owner) for GitHub runners and boxes acquired with net on (a trusted host's); none for the rest`). A box on any network other than `none` drops `NET_RAW` (`--cap-drop`: no forged ARP or addresses on the shared bridge, which a Kata guest's own kernel would allow). `doctor` (docker only) looks for Potluck's filtered network, `potluck-box` (on a machine set up under zm, `zm-box`, whose names it keeps: potluck [docs/network.md](https://github.com/sengirunners/potluck/blob/main/docs/network.md)), readable without root: `docker network inspect` must show exactly what Potluck makes (driver bridge, IPv4 only, not internal, ingress or config-only, IPAM subnet `10.199.0.0/24` alone, gateway unset or `10.199.0.1`, and driver options exactly `com.docker.network.bridge.name=br-zmbox` and `com.docker.network.bridge.enable_icc=false`, plus `enable_ipv4=true`/`enable_ipv6=false` if Docker records them; any other option, a gateway mode, masquerading off or trusted host interfaces, is not Potluck's network and is named), and its filter unit `potluck-box-net.service` (`zm-box-net.service`) must be `active` (`systemctl is-active`). When the unit is missing or inactive, doctor says so (the layer stays ready: boxes then serve jobs only, and sandboxes have no internet) and adds to its one root line the steps that write that systemd oneshot unit, enable it and restart it; when it is `failed`, doctor names the last line of its journal (`journalctl -u`, when readable; e.g. a subnet already in use) and `sudo systemctl restart potluck-box-net.service` (under its name there), not the root line again. The unit runs only `sh`, `iptables`, `ip6tables`, `sysctl` and `docker`, never zm. Every `iptables`/`ip6tables` call waits for the xtables lock (`-w`), and each rule, one address apiece, is checked (`-C`) and inserted only when missing (a re-run, as on each Docker restart, never opens a gap or doubles one), in the raw table's `PREROUTING`, the first hook a packet from the bridge meets, where no other daemon's ACCEPT precedes it (tailscaled inserts its `ts-forward`, ending `-o tailscale0 -j ACCEPT`, and `ts-input` at position 1 of the filter table's `FORWARD` and `INPUT` whenever it starts, which would outrun rules there): from `br-zmbox`, tcp 25, 465, 587 and 2525 anywhere; any address of this machine (`-m addrtype --dst-type LOCAL`) or broadcast address (`--dst-type BROADCAST`); 0/8, 10/8, 100.64/10, 127/8, 168.63.129.16/32, 169.254/16, 172.16/12, 192.0.0/24, 192.168/16, 198.18/15, 224/4 and 240/4; and, where the host has IPv6 (`/proc/sys/net/ipv6`), every IPv6 packet, required. It then makes the network if it is missing (last, so a unit that failed part way leaves no new network) and, with IPv6, sets the bridge's `disable_ipv6`. iptables rules end at reboot, so the unit is `WantedBy=` and `PartOf=` `docker.service`, `After=` it: it runs whenever Docker starts. `zm-run-tart` makes no box on that network unless the unit is `active` (fail closed). zm cannot read the rules without root, so what removes them while the unit stays active goes unseen: a firewalld reload on firewalld's iptables backend, an `nft flush ruleset` (iptables-nft keeps its tables in nftables), an `iptables -t raw -F`. Restart the unit after one. A machine whose owner guards its uplink caps what all boxes on `potluck-box` download together (potluck `hostprobe/boxnet.go`): `share` and `pool host` take `--net-mbit N` (Mbit/s, 1..100000), which adds two `tc` lines to that unit (a `tbf` at N Mbit/s on `br-zmbox`'s transmit side, every packet routed to a box, and `fq_codel` under it); it is oci only and refused on any other layer (a tart box is not on that network), with a cap or without (`--net-mbit 0`), never accepted and ignored; it is a root step like the network's own (run with sudo at a terminal; with `--no-sudo` printed, and the result notes that downloads are not capped yet). The installed unit is the setting: a re-run without the flag keeps it, `--net-mbit 0` rewrites the unit without the cap and deletes the live one, and `doctor` reads it without root (`network_mbit` in `--json`; `downloads capped at N Mbit/s for all of them together` on the network's line). It is refused where zm does not keep the network (`--oci-network`, `--runner-dir none`). Uploads, image pulls and host-fetched checkouts do not cross the bridge and are not capped. When a container volume named `potluck-cache` exists on the host, every oci box gets it read-only at `/opt/potluck-cache`, and a runner box uses its `tools/` (GitHub's tool cache layout) and `goproxy/` (potluck [cmd/potluck-driver-tart/README.md](https://github.com/sengirunners/potluck/blob/main/cmd/potluck-driver-tart/README.md), filled by potluck's `scripts/box-cache-fill.sh`); without the volume nothing changes. `share` and `pool host` then bind a runner box with `oci_network` `potluck-box` (`zm-box` on an old-name machine) when no `--oci-network` is given, with `--net-mail` defaulting to `smtp.gmail.com` (a submission server most networks reach, so the mail check proves something). For a runner box on any network they also pass `--net-allow github.com:443` (a runner must reach GitHub; with a name, the box must also resolve it) and a `--net-forbid` for the forward path past this machine, each only when it opens from here: the default gateway on 53, 80 or 443 (`/proc/net/route`) and Tailscale's `100.100.100.100:53` (none opens: the result says the forward path is unchecked). Conformance runs the binding's config without `runner_dir`, so its boxes (which bring no `jit_config`) are made on exactly the network a runner box gets; the binding carries `runner_dir`. A size binds only when its net checks passed, as for any named network (on a mail SKIP the error also names `--runner-dir none`); without the network, or with `--oci-network none`, they bind the box for jobs only (no `runner_dir`) and print why, with the root line. An owner-named `--oci-network` is used as before (the owner's word, `--net-mail` required). On a host that runs other work beside zm, `share` and `pool host` bind `zm-run-tart`'s admission keys on every size from `--admission-url URL` (http(s), no credentials or query), `--admission-token-file PATH` (a 0600 file, bound as an absolute path) and `--admission-ttl-s N` (120..3600: share binds no `boot_timeout_s`, so the driver's 90 s plus 30), refused as `zm-run-tart` refuses them, before anything runs (`Precheck` included); conformance runs with them too, since its boxes are real VMs on the same room and so take leases like a renter's (a controller with no room fails the run, and nothing binds). Every run reads the bindings it replaces, in the one scope it binds in (machine-level for `share --pool` and for a pool joined, the project's for `share` without `--pool`, the pool's on a pool's own machine). A re-run without `--admission-url` keeps the keys of those earlier box bindings `<prefix>-<size>` ( bindings that disagree are refused until the owner names one), each flag given replacing its own key; `--admission-url URL` takes the flags alone, and `--admission-url none` removes them. Fail closed: unless `--admission-url none`, a run whose read of those bindings failed or showed one without its config (a non-owner's view) is refused (`could not read this machine's bindings (...)`): without the flag it would drop their keys, with a URL a keyless one could stay live beside the new ones; a dry run reads them only from a daemon already running, and notes when it could not. A conformance run that found no room (zm-run-tart's `no room on this host now`) leaves its size unbound as `the admission controller had no room now; try again later`, and the quick check's other sizes say the same instead of blaming the security checks. When a run with admission keys (the first switch-on included) leaves a size unbound whose earlier binding in that same scope has none, share unbinds exactly that binding (also when no size passed and `pool host` binds nothing, but for an invite code, which has not joined yet), since its boxes take room the controller does not count, and says so; when that fails, the size's error says it is still bound WITHOUT admission and names `zm target unbind`. `doctor` prints `admission: <bindings> take a lease from <url> (lease N s, token from PATH) before each box`, one line per controller (`admission` in `--json`).
- **Pools (`zm pool`).** A pool is a team project whose members lend boxes and run sandboxes on each other's, with these roles: a host is a guest of the `hosts` area, never a project member (a project relation covers every area); a renter is a member of a team's area. Three commands wrap the steps above and keep no state of their own. `zm pool create <pool> [--team team] [--renter NAME]... [--host NAME]... [--public-host NAME]... [--via tailnet|tunnel] [--image pin]...` creates the project if missing with the areas `hosts` and the team's, makes this machine its authority (`zm project team`), brings the tailnet node up unless `--via tunnel` (printing the login URL once, waiting up to 10 minutes), and prints one one-time invite per name with the line to run on that friend's machine: a renter `<pool>/<team>` as `member` (`zm pool join '<code>'`), a host `<pool>/hosts` as `guest` (`zm pool host '<code>'`, with `--image` only when create was given one). Each host invite carries its machine's class (section 8): `trusted` for `--host`, `public` for `--public-host`, so the machine has it the moment it joins and create returns at once (`--wait` is deprecated and does nothing). Next to each host invite create prints what the class means in plain words (trusted: placed unasked, may run GitHub Actions jobs and get renters' secrets; public: only sandboxes a renter names, never GitHub Actions jobs or any secret), and next to every invite with a tailnet code the step the friend needs (17.4): share this machine's node (`zm-<machine>`) with them at the Tailscale admin console's Machines page, or invite them to the tailnet at its Users page; a tunnel code (`--via tunnel`) needs neither, so none is printed. A friend who comes later gets an invite from the same command run again (create keeps what exists), or `zm invite <pool>/hosts --as NAME --role guest --class trusted|public`. Section 8's class rules then apply as for any host: a class the owner chose enrolls it (a `trusted` host's boxes are placed and take secrets; a `public` host's only when named, never with secrets), and an area guest's machine with a chosen class is listed (it sends no machine numbers). `zm pool join '<code>'` is `zm join` with the code, then lists the pool's box targets as its authority does and names `zm sandbox create -P <pool>`. `zm pool host '<code>'|<pool> [--image pin]... [share's flags]` runs `share`'s root step as `share` does, once the pool is known (with sudo at a terminal, `--no-sudo` to print it instead), and lends the same sizes (one box at a time on a machine too small for two at the default share); it runs `share`'s checks, pulls and conformance first (`hostprobe.Check`) and joins with the code only when a size passed, so a machine that cannot lend or fails the security checks never spends the invite (a pool named must already be joined, or run here, where it binds in the project); then binds each passed size with `--pool` (`hostprobe.Bind`) and waits up to 45 s for the authority to list each as `<binding>@<machine>` (a host's heartbeat carries it every 15 s); a slower one is reported, not an error. Boxes that serve runners on a host the authority lists as public get no GitHub Actions job (the broker gives a runner only to a trusted host), so host then says so with the owner's command (`zm machine-class <machine> trusted -P <pool>`), and `--dry-run` says up front that runners need a trusted host. Once lent, it installs zm's daily signed-release update (section 2, "Schedule"; `--no-auto-update` skips it, `zm daemon update disable` removes it). A lent Mac's macOS guests are placed and admitted as Linux ones are, whoever's work they do: another member's sandbox, or (on a trusted host) a GitHub Actions job of any linked org. zm offers no per-minute Mac rental and no pooled iOS Simulator time.
- **Lend this Mac (`zm lend`, stage 1).** A front over `zm pool host` for one owner's own repos. It makes the personal pool `home` silently if this machine has none (`zm pool create home --via tunnel`, no invites), then lends to it with defaults: the image of an earlier lend in `home` (its `image_name`, else the pin table's name for its image) and that lend's sizes, (an `--image` lend's own images) and its `reclaim_idle_s`, with the smallest share (in twentieths, above the default) at which `hostprobe.Lend` lends each; else, after `zm lend pause`, the settings it saved in `$ZM_HOME/lend.json`; else the pin table's default image and share's default sizes. Root, the runner folder, the floor and the disk budget come from the pin table through `hostprobe`; jobs run any time (`--when away` sets `reclaim_idle_s`). Before anything runs it says this machine's cores, memory and free disk, what it will lend, the image in words and its `min_free_gb`; share's disk refusal comes before any download. Its output never uses pool, project, binding, box, digest, label, runner or installation (Potluck's text is rewritten, commands and flags kept), prints a line at least every 20 s, and ends `done`. `--detach` does the admin step and share's dry-run checks in the foreground, then goes on in the background (`$ZM_HOME/lend.log`, `lend.pid`). `zm lend status` lists the lent sizes, running jobs and the host switch (open, draining with when running jobs are cut, closed, auto, or unreadable; `--json` answers `LendStatus`); `zm lend switch open|drain|close|auto [--for] [--until] [--deadline]` sets the switch through Potluck's `vmhost.SetHost`; status reads it from a lent size's describe host block, else, with nothing lent, Potluck's `vmhost.HostNow`; `pause` unbinds them keeping the settings (`resume` lends them again); `stop` unbinds them, forgets the settings and prints the `tart delete` line for the image. Both refuse while a job runs or a detached lend is going on, and pause refuses a lend with admission keys (it cannot keep them); `pause --json` answers `compute_pause`'s JSON; a lend refuses while a detached one runs, refuses a `home` this machine joined from another authority or one with any grant or invite besides the caller's own identity (it fails closed when it cannot read them), and has no `--json` yet. Only `--with`, `--when`, `--detach`, `--share`, `--sizes`, `--dry-run`, `--no-sudo` and `--no-auto-update` show in its help; share's other flags still work.
- **`zm-run-tart`** (zm's own, `cmd/zm-run-tart`; with `admission_url`, every VM first takes a lease from the host's
  admission controller, `POST /leases {vcpu, memory_mib, holder, ttl_s}` -> `201 {lease, expires_at}` or `409 {reason,
  retry_s}` (acquire `unavailable`), renewed by `POST /leases/{lease}/renew` (`404`/`410`: ended as `reclaimed`) and given
  back with `DELETE`, the lease short and kept by the run's supervisor; with `vm=incus`, each run a KVM VM in an Incus project zm reaches
  only through a client certificate restricted to that project (whose limits must be set: the owner's share), never the admin socket, its expanded config
  checked before boot for host paths, extra devices, `raw.*` keys and NICs without port isolation and an ACL, or whose ACLs drop or reject no outbound mail (tcp 25, 465, 587 to anywhere); with `vm=oci` on a Linux box, each run a container under a
  runtime that is a VM or sandboxed kernel, Kata or gVisor, never plain runc, with no network unless the owner
  names a filtered one, whose filter, mail included, is the owner's word; on a named network its resolvers are `oci_dns`
  (comma-separated public unicast IPs, at most 3, IPv6 only in 2000::/3; a private, CGNAT, loopback, link-local,
  multicast, documentation, Teredo, 6to4 or NAT64 one is refused, since the filter drops it, it can tunnel to a private
  address, or it is the host; default `1.1.1.1,8.8.8.8`; refused with network `none` or one joining another namespace,
  `container:`/`ns:`, which keeps that namespace's DNS), passed as `--dns` and, but for nerdctl, as a read-only bind of
  the box's own resolv.conf (under `state_dir`, removed with the box) onto `/etc/resolv.conf`, because on a
  user-defined network Docker keeps its embedded resolver `127.0.0.11` and Podman aardvark-dns on the gateway, which
  a boundary runtime or the filter cannot reach; nerdctl writes `--dns` into a resolv.conf of its own, gets
  `--dns-search .` so no host search domain is copied, and still copies the host's resolver options, puts slirp4netns's
  resolver first when rootless, and binds the file writable): runs in throwaway Tart VMs on a Mac (Softnet, no host
  mounts, Softnet's gateway blocked but DNS, outbound mail blocked (tcp 25, 465, 587 to anywhere), APFS clones deleted after every run; `network=nat`, on any layer, turns the
  isolated network off and so describes `isolation: "none"`: refused as a box, an unisolated run), with a reclaim policy
  for a lent machine (owner HID idle time, a schedule window). It passes
  `potluck-conformance --remote`. Its `expires_at` counts from the script's start, since boot is part of acquire.
- `potluck-conformance <driver> [--kind db|run] [--config k=v]` (a run also `--ref`, `--commit`, `--script`) checks a
  driver against this section. It sends the envelope with only the allowlisted environment, and sees what a
  resource does without taking the driver's word: a run through a probe script committed at `commit`
  (`--print-probe` prints it; exit codes are its channel, so a driver passes them through), a database through
  `--db-probe` (default `psql`). A run's `--commit` should be older than the tip of `--ref` on `--repo` (the ref
  moving after zm resolved it): only then does a driver running the ref instead fail, else that check is a SKIP. `--worktree` names the checkout to dirty, `--remote` says a run shares no
  filesystem with it, `--outside-config` is the config that points a db target outside `create_in`,
  `--reclaim-config` the config whose reclaim policy says no once `{window}` closes (the suite fills it with
  `HH:MM:SS-HH:MM:SS`, local time, closing `--reclaim-in` after the acquire; default 20 s); `--net-allow` and
`--net-forbid` (`host:port`, `[v6]:port`, repeatable) are addresses a run or a box must and must not open a TCP connection to from
inside (the internet; the LAN, the host, a tailnet, metadata, private IPv6; a box runs the same probe through `exec`, as
`sh -c`'s script, so it needs no commit), `--net-mail` (a host, checked on 25, 465 and 587, or `host:port`, repeatable)
an outside mail server none may open (resolved on the suite's machine and each address probed as a literal, so a
guest that cannot resolve the name does not pass; a SKIP unless it opens from the suite's machine: a closed port proves nothing),
`--net-resolve` (a DNS name, or `name:port`, repeatable) a name a run or box must resolve (`getent hosts`; a guest
without getent must open `name:port`, default 443), by default with `--net-allow` its names, or `github.com` when all
are IP literals, so a guest that reaches addresses but resolves no name fails here, not in its first job (a SKIP
when the name does not resolve on the suite's machine),
and `--net-peers` runs two at once and
checks neither reaches the other over IPv4 or IPv6 link-local neighbours (the probe's `net` subcommand; a guest without
`nc`, `ip` or `ping` is a SKIP, a probe committed before it fails with re-commit `--print-probe`); what cannot be
  seen is a SKIP, not a pass. It checks: describe shape
  (class, `reports_spend`, spend in `cost.unit`); acquire -> status -> release, `commit` echoed; a run's HEAD
  equal to the `commit` sent, with a dirty working tree where the driver runs not leaking in; release twice
  (`released: false`), and `exit` only for a run already ended; an unknown handle `not_found` and another
  binding's `bad_request`; the same `request_id` twice giving one handle and one resource; an existing name
  under a new key `conflict`; `list` showing the handle and its key; renew bounds (or `not_implemented`) and
  the lifetime cap across renewals; a short ttl expiring with `exit: 124` and the resource gone without a
  release; a reclaim ending the resource as `reclaimed` with no `exit`, `describe` then unavailable and
  `acquire` `unavailable`; spend never falling, and readable by `status` after the end; a db host not in `create_in`
  `forbidden`; no `url` on stderr; only the allowlisted environment reaching the driver; a shell-string script
  refused; and nothing left behind. With `--kind box` (`--ref` and `--commit` optional; the guest needs a POSIX
  `sh`, `awk`, `sleep`, `cat`, `kill`, `ps`, for the checkout `git`, and for the net checks `nc`: a box without it fails them, never a SKIP) the handle, renew, expiry, reclaim and refusal
  groups run as for the other kinds, and in place of a run's checks it checks: `exec` and `size` in describe and
  the untrusted-capable class; acquire answering once `exec` works, with `size` and `image_digest`; `exec`'s
  stdout, stderr and exit, stdin, files kept across `exec`, a `timeout_s` answering 124 in time with the
  command's child ended, exactly the first 1 MiB of stdout and of stderr with `truncated`; `exec` after release
  and after the deadline `conflict`; a new box not seeing the last one's files (in `$HOME` and
  `${TMPDIR:-/tmp}`); a clean checkout at `commit`; an image not in `images`, a shell-string or empty `argv` and
  `timeout_s` past 600 `bad_request`; with capability `base64`, every byte value through exec's stdin and stdout.
  Group `procs` (a box listing `spawn`; else `spawn` must answer `not_implemented`; not in `zm host share`'s quick
  run): a pid per `request_id`, the same one again starting nothing; `output` with `wait_s` waiting on a quiet
  process; TERM to a process that traps it ending its child (its group); `sent` false after the end; the 64 KiB
  default page, `max_bytes`, offsets and bad args; past 8 MiB the newest 8 MiB kept and the rest counted as
  dropped; base64 through spawn and output; secrets as for exec, gone once it ends; a 22 s process read in pages
  while it runs, then its exit; all three `conflict` after release. With the `secrets` capability: the value a `0600` file in a `0700`
  `$ZM_SECRETS` (read back in hex), in no process's environment or argv the box can see (`env`, `ps`, and
  `/proc/*/environ` where there is one; best effort: a host-side leak is beyond a black box), the directory gone
  after the command and after a timeout, no `ZM_SECRETS` without secrets, bad names (with a space, empty, 65
  characters, a trailing newline, lower case) and 64 KiB plus one byte `bad_request` while exactly 64 KiB runs,
  and neither a value nor a name sent on the driver's stderr or in an answer; without it, an `exec` with secrets
  `bad_request`. zm's side is daemon tests: routing (a non-head commit refused on a trusted
  target, allowed with `allow_any_ref`; a caller-sent commit that differs `forbidden`; no ref or an unpushed
  one `bad_request`; a local `main` ahead of the remote and a stale tracking ref both resolved from the
  remote), the stricter class, roles, owner-only bindings, local-only acquire for db, credentialed and
  unisolated targets, the `scripts` allowlist, caps, budget refusal and refused binds, reserved lease keys, the lease
  deadline equal to `expires_at` with throttled renewal that goes on while the session is idle and stops when
  it ends, expiry with no mail, a reclaim ending the lease and left out of budget, the final `status` after the end, the environment allowlist with no inherit,
  secret values refused in a binding, a timed-out acquire never acquired afresh, and `url` kept out of every
  place but the acquire answer.

## 21. Folder search ("find in folder")

`GET /v0/search?project=<slug|ULID>&q=<text>[&path=<one of the project's paths>][&regex=1]` searches the text of
a project's files for this machine's owner (a `local` route: the socket without a token; a token or tailnet
caller gets `403`). It is a small brick (`internal/search`): one `Searcher` interface, two drivers, one
conformance suite both pass.

- **Where**: the project's `paths` and its areas' `paths` as spawn and the MCP server see them (`~` expanded, a
  glob rule replaced by the directories it matches now), or only `path` when given, which must be exactly one of
  those directories (else `400`). Directories that do not exist are skipped, and one under another searched
  directory (an area's path inside the project's) is searched once, as part of the outer one.
- **Drivers**: `ripgrep` when an `rg` binary is on the daemon's `PATH`, else a Go walker. Both: argv only (no
  shell); the query is always the argument after `--`, a fixed string unless `regex=1` (then Go's RE2 syntax is
  checked first, and a pattern ripgrep refuses, for its syntax or its compiled size, is `400 pattern not supported`, never an
  empty answer).
- **What is searched, in a repository** (the nearest directory at or above the searched one with a `.git` directory
  or file): the walker searches what git itself lists, `git -C <dir> ls-files --cached --others --exclude-standard
  -z` (a clean environment, `core.fsmonitor` off, at most half the time limit, at most 64 MiB of output; a listing
  cut there sets `truncated.files`). So every ignore rule is git's: parents up to the repository's top,
  `info/exclude`, worktrees, `core.excludesFile`, `core.ignorecase`. A nested repository or submodule, one entry to
  git, is listed the same way in turn. ripgrep applies its own reading of the same files. The drivers differ in
  three ways, by design: a tracked file an ignore rule matches is shown by the walker (git lists it) and hidden by
  ripgrep; `.ignore` files count for ripgrep only (git does not know them); `core.excludesFile` counts for the
  walker only (ripgrep runs with `--no-ignore-global`).
- **Outside a repository, or where git cannot answer** (not on `PATH`, it refuses the repository, or its share of
  the time ran out, as on a FIFO for a `.gitignore`), the walker uses a small matcher, and outside a repository
  ripgrep reads the same files: `.gitignore` and `.ignore` from the searched directory down, nothing above it (so
  a repository without git loses its parents' rules and `info/exclude`). An ignore file is read when it is a
  regular file, through a symlink or not (never a FIFO or device), at most 1 MiB of whole lines, within the time
  limit. Its subset: patterns, `!` negation, `/` anchoring, trailing `/` for directories, `**` (`dir/**` is what is
  inside `dir`, so `!dir/keep.txt` re-includes), `\` escapes (`a\ ` keeps its trailing space, `\\x` is a backslash
  and `x`; unescaped trailing spaces are dropped). On macOS it matches case-insensitively, and ripgrep runs with
  `--ignore-file-case-insensitive` to agree; elsewhere case counts.
- Hidden files and directories, `.git` and binary files (a NUL in the first 8 KiB) are skipped; symlinks are never
  followed, a tracked path under a directory that became a symlink included, so nothing outside the searched paths
  is read (a root that is itself a symlink is resolved once, and a file whose real path leaves every searched root
  is dropped).
- **Limits** (hard): 2,000 matches, 200 files with matches, 5 s, 200 characters of text per match line (cut on
  a character boundary, the cut marked with `…`); the query is at most 1,000 bytes. The answer says which limit
  cut it short (`truncated.matches`, `.files`, `.time`); lines over 200 characters set `text_clipped` on the match.
- Matches: `{file, line, col, text}`; `file` is absolute, `line` and `col` are 1-based, `col` counted in
  characters (the match's first). Order: by file (the absolute path, segment by segment, each by its bytes: the
  order of a directory walk, so `dir/x` comes before `dir-b` and `dir.go`), then line and column, for both drivers.
  When a limit cuts a search short, the walker's matches are the first of the whole answer; ripgrep searches in
  parallel, so its kept matches may differ from the walker's and from run to run, in the same order.

## 22. Owners and Sign in with GitHub (identifiers stage 4; steps 2 and 3, the authority's half, built)

Design and rulings: docs/decisions/owners-2026-11-01.md. The owners brick
(`potluck/kit/owners`: interfaces `Accounts` and `Device`, GitHub driver, `potluck/kit/owners/conformance`) answers who
an owner is: a GitHub user or org, bound by GitHub's numeric account id (`github:<id>`), never its login.

- **Config:** `config.json` `github: {client_id, api_url, web_url}`; empty URLs mean github.com. The client id
  is an OAuth app's with "Enable Device Flow" set. The device flow needs no client secret, so none is configured.
- **Sign in (built): the authority runs GitHub's device flow.** `zm login github` asks the authority to start a
  login (`POST /v0/login/github/start`); the authority asks GitHub for a device code with its client id (scopes
  `read:user read:org`) and answers the user code, the URL, who is asking (its machine, the identity or node)
  and a poll secret (192 bits; it keeps the hash). The person approves in a browser. The authority polls GitHub,
  receives the token itself, calls `GET /user`, keeps only the account, and drops the token. The CLI collects
  the outcome once with the secret (`POST /v0/login/github/{id}`, long poll), and **the authority acts only
  then**; an outcome nobody collects within 2 minutes expires with nothing done. No client ever handles a
  GitHub token. Logins live as long as GitHub's code (15 minutes). A slot is reserved before GitHub is asked: at
  most 4 in flight and 20 an hour per identity, 64 in all for remote callers and a separate 4 for the machine
  owner's socket. The act runs detached from the collecting request and the outcome is spent only once it
  committed. On a replica, both calls are forwarded to the project's authority (by the cwd or `-P`), once each.
- **Link (built):** purpose `link` maps the approving account to the identity that started the login, in
  `owner_links`. Only the socket owner or a replica's node-bound token starts one (a minted token is capped and
  would otherwise become a full login), only a human identity, only a person's account (not an org or bot). A
  link is made once: another account for the identity, or another identity for the account, is a conflict; an
  email never links anything. A first link mails the identity and the owners of each project it holds a grant
  in, naming the account and where the login started, with the way to undo it.
- **Key (built):** purpose `key` binds a person's signing key (16 "Key approvers"): the project on
  `?project=` (the body may repeat it, never contradict it), the starter as for `link`, and the approvals brick's check
  (proof, single-use nonce, the identity owns or administers the project, a member's call marked `origin_checked`, else
  `403` `member-too-old`). The latest key login of one identity from one node wins once GitHub gives it a code; the
  older one's poll answers `404`. On collect the link and the bind are one transaction: an identity not yet linked is
  refused (`409`) when the account already belongs to another identity, so a key login never joins two people. The
  outcome carries `key: {approver, fingerprint, state, receipt_key, account, login, machine}`. Key logins have their own
  hourly limit (20 starts per identity and node). An authority without it answers `400 unknown login purpose`.
- **Unlink (built):** `zm identity unlink github <identity> [--revoke]` (the machine owner on the socket, the
  identity's own replica, or an admin of a project it holds a grant in; never a minted token or a tailnet login) removes the link, and with `--revoke` the identity's node-bound
  tokens; it mails the same people. The GitHub user then links again with a fresh device flow. This is the way
  back from a phished device code (owners-2026-11-01 Q2).
- Machines and agents keep invites and tokens (17.4); they never need GitHub.
- **GitHub grants (built):** `zm grant @beagh owner harbor` (`POST /v0/grants/github`, an admin of the
  project or area) resolves the login to its numeric id now and stores the grant by id (`github_grants`); `zm
  grant @example-org member harbor` grants an org, by the org's id: at each join the org is looked up by id for its
  current login, and its active members qualify, asked live with the person's login token (a renamed org's
  members still match; a new org squatting the old login matches nobody). An org grant needs
  `github.lookup_token`: a secret reference (potluck/kit/secrets; never the token itself in config.json) to an org
  member's token with scope `read:org` only, needed only once an org grant exists, so the authority can re-check
  members after the person's token is gone. Every hour, a member GitHub reports as no longer in the org (or a
  gone org or account) loses what the org's grant gave. A check that cannot run (the token expired, revoked or
  rate limited) removes nobody: the project's owners are mailed once, and after 24 hours of failures new joins
  through org grants stop until a check runs again. Later, not v1: a zm GitHub App installed in the org with
  `members:read` (installation tokens, nothing personal stored). `zm revoke @x harbor` removes a grant and **everything it gave**: each admission is
  recorded (`github_admissions`), its grants row carries the source (`created_by` `github:<id> grant:...`), and
  revoking deletes those rows (never a grant made another way) and revokes the node-bound tokens. `zm grants
  harbor` lists them. No identity exists until the person joins.
- **Join by GitHub, the authority's half (built):** `GET /v0/join/github` answers only "a zm authority", its
  protocol and machine: no project input, no list, so it is no oracle. `POST /v0/join/github` from a tailnet
  node (WhoIs; never the socket, this machine or a tagged node) starts a login with purpose join, bound to that
  node, for any project name. Starts are limited per node (4 in flight, 20 an hour, 64 remote in all; the
  machine owner's socket has its own pool). When GitHub answers, the authority keeps the account and which of
  the project's granted orgs it is an active member of (any failed check counts as "not a member"), drops the
  token, and waits until 1.5 s after GitHub's answer whatever it found. When that node collects the outcome,
  and only then, the authority takes the strongest of the account's GitHub grants (a whole-project grant wins a
  tie), finds the identity linked to the account or creates one named after the login (a taken name gets `-2`;
  created and linked in the admission's own transaction, so never an orphan and never a name match), and admits
  the node exactly as a redeemed invite does (`project.Registry.Admit`, shared with `Redeem`). A join again
  from the same node replaces its token and keeps the stronger of the old and new roles (a GitHub join never
  downgrades a node: an explicit lower request later keeps the stronger token and its area, the same code
  path). An owner's act caps it (Potluck #159, #174, #175, #179): after an owner removes or lowers a person, a
  GitHub join of theirs is refused (forbidden: "an owner removed or lowered <name>'s grants in <project>") or
  capped at the lowered role until an owner raises them or they redeem a new invite, even once the GitHub grant
  that made the row is gone. The ceiling holds the GitHub account, not only the identity: the act also removes
  or lowers, to the same role, the grants above it of every other identity that is or was linked to the same
  account (never the machine owner's own): on the same object and, for a whole-project act, their area grants in
  the project too. They are named in the grant answer's `also_lowered` (and printed by `zm grant` and `zm
  revoke`); raising one back raises no other. Lowering or removing someone also deletes their
  unredeemed invites in the project that would undo it. **Scoped joins:** the request may carry `area` and `role` (the most the joiner asks for). The admission
  is then exactly that area at that role when the account's GitHub grants give at least that much there (a
  whole-project grant covers any area, an area grant only its own; guest < member < owner); asking for more, an
  area that does not exist or one no grant covers is the one refusal below. A role alone asks for the whole
  project (only a whole-project grant covers it); an area alone takes the role the covering grants give. With
  neither, the tailnet's behaviour is the strongest grant, unchanged. **Over https** (17.11's door passes the
  GitHub join's three unauthenticated calls, signed and fresh, with the same limits: discover, start, collect):
  the caller is the signing key (`zmkey:...`, never the home's own), the login is bound to that key id and only
  that key collects, an `area` is required (`400`) and the join is **guest only**: a `role` other than `guest`
  is `400` ("a member or owner joins over the tailnet, or with an invite"), because a stranger could otherwise
  ask for owner and phish a grant holder into approving the code. A host or a stranger joins one area as a
  guest and never takes the account's strongest role. The approver is shown the key, the area and the role.
  Tunnel callers are unauthenticated (anyone can make a key), so they have their own limits: a pool of 8
  logins in flight in all (the tailnet's 64 are never used up by them), and per client address (`core.Remote.Addr`)
  4 in flight and 20 starts an hour, besides the per-key limits; refused starts leave nothing in the limit
  tables, which also drop owners with no start in the last hour. Addresses are bucketed: the exact address for
  IPv4, the /64 for IPv6. Known limit (accepted): two addresses at 4 each fill the tunnel's 8-slot pool and
  block tunnel joins (only those) for the length of a login. An unknown project, one that is not a team project, one with no
  GitHub grants and no grant for this account are one refusal, the same bytes but for the name typed. A first join from a node
  mails the identity and the project's owners, naming the node, the GitHub login and how to undo it.
- **Owner names (built):** `zm names bind <project> @owner/name` runs a GitHub login with purpose bind (an admin
  of the project; the name checked first). While GitHub's token is in hand the authority looks the owner up and,
  for an org, asks live whether the approver is an active member; when the outcome is collected it binds,
  provided the approver is the binder's linked account and the owner is that account or that org.
  `owner_names (owner id, name) -> project` is append-only (triggers refuse an update or delete): a rename or a
  transfer adds a binding and makes it current, and the old one answers `moved`. `login_paths (login, name) ->
  owner id` pins every path this authority bound or answered; resolving (`zm names resolve`, `GET
  /v0/names/{owner}/{name}`) follows the pin, whoever holds the login on GitHub now, so a renamed owner's old
  links keep reaching its project after GitHub reassigns the login, and the newcomer cannot bind those paths. A
  new path goes to the login's current holder (cached 1 hour; a login GitHub no longer answers redirects one hop
  to its last account) and is pinned. A project the caller cannot read answers exactly like a missing name (and
  pins nothing). Only an org's **admins** bind under `@org`: a name is never moved, so it is the org's to spend.
  A pin records what this authority bound or answered: a name Ivo binds after renaming `ivo` to `iv`
  is pinned under `iv` only, and `@ivo/<it>` follows whoever holds `ivo` then. GitHub lookups for
  names: a login GitHub does not know is cached as missing for 10 minutes, each caller makes at most 30 uncached
  lookups a minute (429 after), and with GitHub away an unseen login answers like a missing one.
- **Frozen owners (built):** when GitHub stops answering for an owner's id (`GET /user/{account_id}` 404), its
  names resolve read-only (`frozen`), and nothing is bound, renamed or transferred under it, nor is a project
  whose current name is under one moved away; checked at every bind and on a resolve whose owner record is
  older than 1 hour; it lifts when GitHub answers again.
- **Join by GitHub over https, the member's half (built):** `zm join <project>/<area> --from
  https://<host>#<home key fingerprint> [--role guest]` needs no tailnet. The fingerprint (given by
  the home's owner, as an invite's code carries it) is the trust anchor, in the place of the peer's tailnet
  owner: the home is pinned by it and every call is signed by this daemon's key, as for a redeemed invite; a
  URL with no fingerprint is refused. The daemon asks `GET /v0/join/github` (an authority that runs GitHub
  logins), starts the join with the area, as a guest (`--role` other than guest is refused), the CLI prints the code and who
  asks (the home key, this key, the area and role), and the next call collects the token; from there the join
  goes on as a redeemed invite's. `--cf-access-*` apply as for a tunnel invite. The tailnet path below is
  unchanged (`project/area` there also scopes it, with `--from <machine>`).
- **Join by GitHub, the member's half (built):** `zm join <project>` (no `:` in the argument, so no code) brings
  this daemon's node up as any join does, then asks each zm peer on the tailnet (`zm-*`, `GET /v0/join/github`,
  3 seconds) whether it is an authority that runs GitHub logins. Any zm node can claim to be one, so nothing
  starts until the person chooses: the first answer lists the authorities found, each with its node's tailnet
  owner (WhoIs), and every skipped peer with why (no answer, not an authority, `github:false`). On a terminal a
  sole authority is confirmed ("join harbor at mini (tailnet owner ivo@...)? [y/N]"); elsewhere, or with
  several, `--from <machine>` (any case) names it; `--json` prints that first answer. Bringing the node up and asking the
  peers takes 25 seconds at most: no authority (or the node not up or not logged in) is a prompt refusal, "no zm
  authority answered on the tailnet for GitHub joins (checked N zm-* peers; ...); is Tailscale up? or pass a join
  code (it contains ':')". A code-shaped argument (a ':', the `zm-` prefix, a secret's `XXXX-XXXX`, anything that
  is no project slug) is a join code, and a malformed one is refused as one; only a valid slug is a project name. Then the chosen node is
  pinned by its fingerprint, the join starts there, the CLI prints who is asking (authority, tailnet owner,
  this node) with the code, and the next call (`POST /v0/team/join` with `login`, long poll) collects the member
  token; from there the join goes on as a redeemed invite's (the token file, 0600, the first call, the
  checkouts, the copy). A GitHub join refused after admission gives its new token back. The poll secret stays
  in the daemon. The remote records the authority's tailnet owner; a later connection to the same node under
  another owner is a warning in `zm remotes`. Proven against a fake GitHub on the fake tailnet (`internal/team`
  a join test: two authorities and a `github:false` peer, `--from`, a refusal and an expiry through the
  daemon, then grant, join, bd, mail and work from the member's machine; `TestRogueSoleAuthorityNeedsAChoice`).
- Not built yet: the host-network transport (a third driver on #114's `core.TeamTransport`),
  project handles and `zm:` permalinks.

## Native project summaries (Commune)

The summaries brick owns durable daily project digests and opt-in local scheduling. It reads work only through orchcore.WorkReader, with a caller subject and explicit project; it never queries another brick's tables. Digests include bounded titles/counts and coverage metadata, not mail, comments, transcripts, approval or deployment claims. They also count, by plain label, the day's machine notices that went to no person's inbox: each brick that holds some registers a core.NoticeCounter over its own tables (the GitHub link: CI results by where they went; mail: old machine notices its sweep acked, lease-expiry notices). Counts only, never a body, address or item id; a counter that fails marks the digest partial. Whole-project read is required to read a projectwide digest; generating requires whole-project operate; configuration requires an authenticated human project owner with whole-project admin. Explicit joined-project calls forward to the authority; no local archive or schedule fork is created. Area-only and hidden projects answer exactly like missing projects. Revoking whole-project access immediately removes access to old digests.

The built-in archive is SQLite; Repository is its narrow get/put seam, with a remote archive driver as the prospective second driver. A unique project/day/configuration-version key makes daily retries and restart catch-up idempotent. Retain at most 400 rows/project. A redaction scrubber replaces matching source text in archived bodies; new summaries never copy source bodies or comments. No messages or external requests are emitted. Sharing is an explicit copy/share action in the client. Bounds, timezone behavior, source partialness and route contracts are in API.md Project summaries.

## 23. zm-connect: outreach connection (slice 1 built)

A Cloudflare Worker (`deploy/cloudflare/zm-connect`) plus a zm-side bridge (`cmd/zm-connect`,
`internal/connect`), never part of the daemon. It runs on Cloudflare.

- **Deploys** come only from `.github/workflows/zm-connect-deploy.yml`. Each one is run by hand on main and
  waits for the owner in the `zm-connect-deploy` environment. It verifies their zm receipt for the PR that last
  changed the Worker (`zm receipt verify --repo --pr --head`), and checks that main's Worker tree equals that PR's
  head. The Worker serves only its custom domain: no `workers.dev`, no Version or Preview URLs, no invocation
  logs.
- **Human routes** (`/signin`, `/setup`, `/forget`, `/oauth/google/callback`, `/health`) are behind Cloudflare
  Access and re-checked in the Worker:
  - the `Cf-Access-Jwt-Assertion` header, RS256 against the team's certs;
  - `iss`, `aud`, `exp`, `nbf`, and the email in `OWNERS`;
  - every POST needs `Origin` equal to `ORIGIN` and `Sec-Fetch-Site: same-origin`.
- **Sign-in:** the state is single-use, lasts 10 minutes, and is bound to the Access subject and a `__Host-zmc`
  cookie nonce. PKCE is S256 and the scopes are exactly `gmail.readonly` and `gmail.compose`. After the exchange,
  the scopes and the configured mailbox are checked. The Google client is a Web client file uploaded once on
  `/setup` (at most 8 KiB, our redirect URI, the pinned project number), and only `/forget` replaces it, revoking
  first.
- **Secrets:** the refresh token and client secret live in the `Vault` Durable Object, sealed with AES-256-GCM
  under `VAULT_KEY` (a Worker secret, made only by an explicit deploy input), with the field name as AAD. Neither
  appears in an answer, page, notice or log.
- **Gmail** is read and draft only. A guarded `fetch` allows only `gmail.googleapis.com` under
  `/gmail/v1/users/me/`: GET profile, history, labels, messages, threads and drafts; POST and PUT drafts. It
  refuses send, batch, upload, delete, method overrides (headers and query keys, in any case), odd paths and every
  redirect. The token client allows only `oauth2.googleapis.com` `/token` and `/revoke`.
- **Sync,** cron `*/15`, one pass at a time (a vault lease), at most about 20 s of CPU per pass, oldest first:
  - Headers are read first. Only a message to or from an address in `crm.outreach` is fetched in full and written
    to `crm.messages`, through Hyperdrive as `zm_connect` (statements in `sql/`, also run by
    `internal/connect/dbrole`'s real-Postgres test).
  - Its gmail rows move: sent (and `gmail-manual` for a hand-sent mail), replied, or bounced. Only Gmail's own
    mailer-daemon counts as a bounce.
  - Auto-replies are logged and move nothing.
- **Bridge:** `POST /bridge/pull` and `POST /bridge/ack`, with no Access.
  - Each request is signed with Ed25519 over `zmc-bridge/1`, method, host, path and query, a timestamp within
    60 s, a nonce (single use, 5 minutes) and the body's SHA-256, by a key listed in `BRIDGE_KEYS`. The answers
    are signed by a key derived from `VAULT_KEY`, which the bridge pins.
  - A pull leases up to 50 notices for 2 minutes. An ack marks them delivered. Notices expire after 30 days, at
    most 1,000 are kept, and replies are never dropped first.
  - Notice kinds: `external_reply` (outside text: the bridge mails it quoted, as one line, links inert),
    `signin_needed` (at most daily; the link must be the Worker's), `pass` (posted to the bridge's Mezze app report URL)
    and `alert`. The Worker alerts through the team's alert hub when the oldest undelivered notice is over an hour old.
- **Version gate:** the Worker uses the Gmail token (sync, sign-in) only while its running version
  (`CF_VERSION_METADATA.id`) equals `approved_version`. The owner sets that on `/approve` (Access, same-origin
  POST, the running version only). `POST /bridge/seal` clears it; a bridge can seal, never approve. Each pull
  answer carries `{live, tag, uploaded, approved}`. The bridge mails the owner on every change of the live version
  and seals unapproved code. Code that lies about its version defeats the gate but not the change mail.
- **Copies** are `env` blocks in `wrangler.jsonc` (account, domain, mailbox, database); the code is shared.
- **Database role** `zm_connect` (SQL from `zm-connect dbrole-sql`, holding a SCRAM verifier): SELECT, INSERT and
  UPDATE on `crm.*`, and SELECT on `enrich.*` and `public.*`. `crm` is owned by NOLOGIN `crm_owner`.
  `jacks_agent` keeps SELECT only. The setup refuses a `trust` or cleartext pg_hba rule over TCP.
## 24. Connections: show

A connection is an outside system (a CI, a build farm, a vendor) that zm shows as one typed summary. zm owns the
contract; a provider is anything that answers it. A third party plugs in with a zm project, a token and an
endpoint, and no zm code changes. Show only for now; act (guarded tools) and ask (zm cards) come later behind the
same registration.

### 24.1 Registration

- `zm connection add <name> <endpoint> -P <project>[/<area>]` (`POST /v0/connections`) needs admin on the project
  or area. Names are unique per project. 200 connections per daemon.
- The endpoint is `https://...` (checked like a webhook URL: public hosts only, every dial rechecked after DNS) or
  `unix:/abs/path.sock[?path=/show]`. A socket endpoint, `allow_private` (http, loopback, private and tailnet
  addresses) and `token_ref` are the machine owner's on its socket only.
- The token is the provider's: zm sends it as `Authorization: Bearer`. It is stored in registry.db (as webhook
  secrets are) and never returned. The machine owner may pass `token_ref` (a secrets reference: `file:`,
  `keychain:`, `env:`) instead; it is read at each fetch.
- Reads need read on the connection's project or area; a hidden connection answers like a missing one. Only
  admins see the endpoint and who made it.
- A connection lives on the daemon that registered it. A joined team project's connections are not forwarded to
  its authority yet.

### 24.2 The summary (`zm.connection.show/1`)

The provider answers `GET` with JSON (API.md "Connections" has the shape):

- `as_of` (required): when the numbers were true. `stale_after_s` (default 300, 30 to 86400): how long they stay
  fresh.
- `headline`: rows with a `state` of `ok`, `warn` or `red`, an optional `count`, `since` and `detail`.
- `meters`: `used`, optional `reserved`, `total` and a `unit`. Used plus reserved may pass total (overcommit);
  clients clamp the bar.
- `running`: items with a `title`, optional `owner`, `started` (clients show the age) and up to 4 http(s) links.
- **Every row, meter and item carries `account`** (required): whose slice of the provider it is (a customer, an
  org, a pool). `show?account=` keeps one account's.
- Bounds: 256 KiB, 32 headline rows, 32 meters, 200 items; keys unique per list; labels one line with no control
  characters. A summary over a bound is refused, never cut short.
- Additive: unknown fields are ignored, a missing list is empty. A breaking change is a new schema name.
- `zm connection check <file|->` checks a summary offline; `internal/connections/fakeci` is the example provider
  (a CI with a queue and a runner pool) and the conformance fixture.

### 24.3 Fetching, cache and freshness

- zm fetches when someone shows the connection, at most once per 15 seconds per connection (concurrent shows share
  one fetch), with a 10-second deadline. No redirects; no proxy.
- A fetch that fails, is refused (401/403), breaks the contract or carries a future `as_of` (2 minutes of skew)
  does not replace the last good summary. The last good body and both stamps are kept in registry.db, so they
  survive a restart.
- `show` answers a `state`:
  - `fresh`: the last fetch worked and `as_of` is within `stale_after_s`;
  - `stale`: the last good summary, with a plain-words `reason` (the provider has not updated its numbers for 6
    minutes; zm could not refresh it: the provider refused zm's token);
  - `unavailable`: nothing good, or the newest `as_of` is over 24 hours old. No summary is returned.
- Reasons never carry the endpoint or the token.
