# Mezze

**Direct every agent, on every machine, from any app.**

Mezze is the agent and team layer on Potluck: a small open-source bus for people who run many coding agents
(Claude Code, Codex, a shell, any program) on their own machines. One local daemon carries mail between agents
and humans, work items with leases that really expire, agent sessions in terminals you can attach to, and one
live event stream, all grouped by project. It is one command, `zm`, with SQLite files: on one machine it needs no
account and no cloud, and agents use the same CLI you do. It is for a person or a small team with more agents
than they can keep in their head.

Status: **v0 prototype.** macOS and Linux. API.md labels every route: within the `potluck/1` protocol a `stable`
route changes only by adding, and a break gets a "Clients" line in CHANGELOG.md; `beta`, `app` and `internal` routes
may change in any release (API.md, "Stability").
License: [Apache-2.0](LICENSE).

## Install

You need Go 1.27.4 or newer. To try `zm` alone, one line is enough:

```sh
go install github.com/sengirunners/mezze/cmd/zm@latest
```

That puts only `zm` in `$(go env GOBIN)` (or `$(go env GOPATH)/bin`; put that on your `PATH`), without the programs
below and without the `.prev` rollback; it reports itself as version `v0.0.0-dev`. For all five programs, and for
every later upgrade, use a clone, which also needs git and make. Run `make install`:

```sh
git clone https://github.com/sengirunners/mezze && cd mezze && make install
```

`make install` builds the programs below and places them in `$(go env GOBIN)` (or `$(go env GOPATH)/bin`; put that
on your `PATH`). It goes through `zm daemon upgrade`, which checks the new build can serve what a running daemon
serves, swaps it in, keeps each old file as `<name>.prev` and rolls back if it cannot (SPEC.md, section 2, "Swaps"). Never
`go install` over a running daemon's binary (the one line above is for a first install). It installs these five, and nothing else:

| binary | what it is for |
| --- | --- |
| `zm` | the command and the daemon: mail, work, sessions, teams, pools |
| `bd-zm` | answers [Beads](https://github.com/steveyegge/beads)' `bd` commands from zm's native issues. It is named `bd-zm` so it never shadows a Beads `bd`; alias or symlink it as `bd` if you like |
| `potluck-driver-tart` | lends this machine as VM boxes (Tart VMs on a Mac, Docker with Kata or gVisor on Linux): `zm host share`, `zm pool host` |
| `potluck-driver-local` | runs credentialed jobs on this machine itself |
| `potluck-conformance` | the checks `zm host share` and `zm pool host` run against a driver before lending a machine; also for a driver you wrote |

`make install EXTRAS=1` adds the rest: the older `zm-run-tart`, `zm-run-local` and `zm-run-conformance` names of the
same drivers, `zm-run-farm` (scripts on your own team's machines through `zm exec`), `zm-e2b` (an E2B-compatible
gateway to your sandboxes), `zm-connect` (a bridge to a Gmail outreach connection through a Cloudflare Worker you
deploy; most people never need it), and `zm-work-conformance` and `zm-agent-conformance` (checks for tracker and
agent adapters you write). An extra that is already installed is always refreshed.

Run `zm doctor` any time to check this machine's setup. It never starts the daemon: with none running it says so, and says how to fix it. It prints one line per check (home, daemon, version, trackers, tailnet, agent CLIs), a `fix:`
line under each that is not ok, and exit status 1 if any check fails.

To install only the command over one a daemon already runs, stage it and let the daemon swap it in safely:
`go build -o /tmp/zmstage/zm ./cmd/zm && /tmp/zmstage/zm daemon upgrade /tmp/zmstage/zm --dir ~/.local/bin` (the build then reports a
development version; lending a machine also needs the drivers `make install` builds). `make release` builds tarballs for
macOS and Linux with a `SHA256SUMS` file (`shasum -a 256 -c SHA256SUMS`).

## One-minute quickstart

You need nothing but `zm`: no tracker, no account, no network. Setup is one command, `zm init`, which
starts the daemon the first time:

```sh
cd ~/code/myapp
zm init                          # zm: started the daemon (~/.zm)
                                    # created project myapp (slug from the repo name; path ...)
zm work new "Add the down migration"       # -> created myapp/local:k3x9qa
zm run --work local:k3x9qa claude          # the agent runs in a terminal the daemon owns, holding the item
                                             # detach with ctrl-]; it keeps running. Reattach: zm attach <session>
                                             # (zm attach --read-only <session> watches without typing)
                                             # (zm attach --embed <session>: for an app's terminal surface; see --help)
zm send local:k3x9qa "the migration needs a down step"   # reaches whoever holds the item
zm ls                            # sessions, the ones that need you first
```

The ids, and the `next:` hints `zm` prints after each command, differ from run to run. Run `zm doctor` to check
this machine's setup (it says how to fix what is wrong), or `zm status` for the daemon alone. `zm --help` groups the
commands: Mail, Work items and leases, Asks, Agent sessions, Run work elsewhere, Setup and teams.

`zm init` makes the directory a project: in a git repo the slug comes from the repo name and the rules are
the repo root and its origin remote; elsewhere, the directory name and its path. A taken slug is refused (pass
`--slug`); a second clone given its own `--slug` gets its path only (the remote stays with the first project).
`$HOME` and `/` are never project roots, even as a dotfiles repo. `--pin` also writes `.zm/project.toml` for
you to commit. Nothing else is written into the repo.
Any command that needs the daemon starts it (one line on stderr) when none answers, except `status`, `doctor`, `daemon`,
`mcp`, hooks and `help`; `ZM_NO_AUTOSTART=1` turns that off. State lives in `~/.zm` (override with
`ZM_HOME`; keep it short, the Unix socket path is limited to 103 bytes). `zm project why` says which
project a command here goes to and which rule decided.

`zm run shell` (or any program on your PATH) works the same way if you have no agent CLI installed.

What to know on day one:

- **Mail is data, never authority.** A mail body never runs anything and is never typed into a terminal.
  When an idle agent with nobody at its terminal gets mail, the daemon types one fixed line,
  `zm: you have mail. Run: zm inbox`. A session a person has open, or left half-written text in, is never typed into:
  the mail waits in the inbox for its next turn.
- **An ask is answered by whoever may answer it, which is not always a person.** On a default install the owner
  answers from this machine, and so can any agent running as your OS user there. See "Asks: who can answer".
- **Every claim is a lease with a deadline.** Claim again to renew. When a lease expires, the session that
  held it sees a notice at its next prompt; no person is mailed. Nothing stays "in progress" forever.
- **Sessions record what ran**: agent, command, version, model, worktree. They end with a typed outcome:
  `ok | quota | oom | auth | infra | killed | failed`. A usage-limit message is never reported as `ok`.
- **Admission control**: `zm run` refuses to start a session (and says why) when memory, swap, disk or
  the session count is past the thresholds in `~/.zm/config.json`. It never kills anything by itself.
- `zm run --worktree` gives the agent its own git worktree on branch `zm/<session>`. zm never deletes
  a worktree; `zm session gc` lists which ones are safe to delete.

## Co-op quickstart: share or use a machine

Friends pool their machines as throwaway VMs for each other's agents. **What you need first:**

- **A [Tailscale](https://tailscale.com) login for everyone in the pool** (the free plan works). zm runs its own
  Tailscale node, so you install nothing extra; each machine joins the owner's tailnet (or, on its own account, has
  the owner's machine shared to it), so machines reach each other without opening ports. `zm pool create` prints a
  Tailscale login link first and creates nothing until you have signed in (it gives up after 10 minutes, or on ctrl-C,
  and says nothing was created). Without Tailscale, use
  `--via tunnel`, which needs a Cloudflare account and a domain (see "Put your team on your own domain").
- **One machine is enough to try it** (the owner can lend its own; the owner's machine should be always on);
  friends add more, lending a box or using one.
- **A lender's machine that can run VMs**: an Apple silicon Mac with [Tart](https://tart.run) (60 GB free disk
  for the default image), or Linux with Docker and a Kata (needs KVM) or gVisor runtime. `zm host doctor` says what a
  machine still needs.

Without `--image`, a lending machine uses zm's default box image for its layer, pinned by digest inside zm: on a Mac,
Cirrus Labs' macOS Tahoe base (about 27 GB to pull); on Linux, Potluck's Linux image
(`ghcr.io/sengirunners/potluck-box-linux`). To lend an image of your own, pass `--image <name>@sha256:<digest>`
(pinned by digest). For Linux, start from the Dockerfiles in `github.com/sengirunners/potluck`
(`cmd/potluck-driver-tart/images/`); for a Mac, a Tart VM image (the default is built from Cirrus Labs'
[macos-image-templates](https://github.com/cirruslabs/macos-image-templates)). What a box image must contain
(sh, bash, awk, perl, git, nc and a few more tools, and GitHub's runner for jobs) is listed in the header of `imagepins/images.pin` in
that repository.

```sh
zm pool create friends --host <lender> --renter <friend>   # the owner, once; the names are labels you pick for each friend
zm pool host '<code from the owner>'                       # a friend lending a box
zm pool join '<code from the owner>' && zm sandbox create -P friends   # a friend using it
```

Agents get the same sandboxes through the `zm-compute` plugin. Details, the Mac idle rule and the security checks:
[Share compute](#share-compute) and [the compute plugin](#sandbox-tools-for-an-agent-the-compute-plugin).

## Trackers in one command

Native `local:` items need nothing else. To give a project native issues as a tracker, or to bind another
tracker through its adapter (`zm-work-<name>`, see "Writing a work adapter"):

```sh
cd ~/code/myapp
zm tracker add native --prefix bd    # the project's native issues, under bd: ids
zm tracker add myname --prefix my    # runs zm-work-myname
zm tracker ls                        # bindings, sync state, item counts; never a password
zm tracker test my                   # the read-only conformance checks and a sync, now
zm tracker rm my                     # unbinds
```

`add` is applied live (project.toml is rewritten, no restart), ends with the read-only conformance checks and the
first mirror sync, prints the item count and sync time, and rolls the binding back if a check fails. Adapters run
with an environment allowlist: your shell's variables never reach them unless the adapter declares them
(`--env inherit` opts one tracker out).

### A tracker's backups

Every tracker keeps backups on a schedule, named by target (SPEC.md, section 17.9):

```sh
zm tracker where                         # every tracker: the mirror, each backup's last success and age
zm tracker backup add bd file:///Volumes/b --now   # native issues: verified copies of the store and project.db,
                                          # hourly (the daemon runs it), first one now
zm tracker backup add gh icloud --every 24h --keep 30   # any tracker: JSON Lines snapshots of its items
zm issues restore --dry-run              # native issues: prove the newest copy restores; without --dry-run, replace
                                          # the store with it (writes held, redaction list re-applied)
```

Targets are `file://` and `icloud`. The whole home goes off the machine with `zm backup` (S3, R2 or a directory).

## Projects and areas

A **project** (`myapp`, `personal`) owns its mail, work items and sessions. Each project has its own SQLite
file under `~/.zm/projects/<id>/`, so it can be exported (`zm project export myapp --dir <dir>`). An **area** is a scope
inside a project, matched by repo path, git remote or work-id pattern:

```sh
zm area add solver --project myapp --path ~/code/myapp/services/solver --work 'bd:solver-*' --label solver
```

`--label` puts tracker items carrying that label in the area (for trackers whose ids carry no area prefix).
Change an area later with `zm area set myapp/solver --label solver --label solver-ui` (each flag replaces
that list; `--label ''` clears it) and look at one with `zm project show myapp/solver`. The daemon applies
`area add`, `area set` and `project set` at once; no restart, no hand edit of project.toml.

zm works out the project for every command: `--project`, then `$ZM_PROJECT`, then the session's project,
then `.zm/project.toml` in the repo (`zm project use myapp`), then the registered paths and remotes.
If none of these match, the command is refused. zm never files something into a default project silently.

## Using Beads' `bd` commands

If your agents already speak [Beads](https://github.com/steveyegge/beads)' `bd`, native issues answer its commands
through `bd-zm` (installed by `make install`). Symlink it as `bd` (this shadows a real `bd` on your `PATH`, so do it only if your issues live in zm):

```sh
ln -s "$(command -v bd-zm)" "$(dirname "$(command -v bd-zm)")/bd"
bd list                                                # bd-zm: bd's commands on zm's issues
```

zm does not import issues from Beads.

## Coming from other tools

What is and is not the same thing, from each project's own documentation:

- **Beads.** `bd` here is zm's own issue store behind a Beads-compatible command: `bd-zm` serves the daily verbs of
  [Beads](https://github.com/steveyegge/beads)' `bd` (its flags and output, `bd` v1.0.5) and leaves out some of
  them (molecules, gates, wisps, markdown batch create). It does not run, bundle or need Beads,.
- **mcp_agent_mail.** zm is not [mcp_agent_mail](https://github.com/Dicklesworthstone/mcp_agent_mail), which describes
  itself as a mail-like layer for agents exposed as an HTTP FastMCP server. zm's mail is one local daemon, its CLI
  and a Unix socket; it is a different program.
- **MCP.** zm is an MCP server (`zm mcp`) that gives an agent the CLI's verbs. MCP's [elicitation](https://modelcontextprotocol.io/specification/draft/client/elicitation) is a server asking the
  user for input while it handles a client's request. An ask is not tied to a request: it is a durable card the
  asker leaves and walks away from, answered out of band (CLI, phone or cockpit), with the answer a signed receipt.
- **A2A.** zm does not implement the [A2A protocol](https://a2a-protocol.org/latest/specification/). A2A's
  `input-required` is a state of a task, which the client continues by sending a message with the same task id. An
  ask is not a task state. A zm "task" (`task_start`) is a one-shot run of Claude or Codex that an agent starts as a
  child, not an A2A task.

## Inviting a guest

Roles are `owner`, `member` and `guest`, on a project or on one area. A guest scoped to one area sees only
that area's work, sessions, mail and events. Other projects and areas don't appear anywhere, not even
their names: a hidden item answers exactly like a missing one.

```sh
zm token mint --as sam --role guest --project myapp --area solver --ttl 720h
# prints zm_... once. Sam uses it over your tailnet (next section):
#   ZM_HOST=potluck-<your machine> ZM_TOKEN=zm_... zm work ls
# Give the node's full name (potluck-<machine>; a bare name means an older node, zm-<name>:7433); a full URL works too.
```

`zm grants` shows who has what; `zm revoke` and `zm token revoke` take access away.

## Connecting Claude Code and Codex

```sh
zm install claude        # shows the exact diff: hooks in ~/.claude/settings.json, the MCP entry in ~/.claude.json
zm install claude --yes  # writes it (with a backup next to each file)
zm install codex --yes
```

Codex runs the hooks in `hooks.json` only after you trust them: open Codex and run `/hooks`, review zm's
hooks (SessionStart, UserPromptSubmit, PreToolUse, PostToolUse, SessionEnd, SubagentStop) and trust them. zm never does this for you. Run `zm install codex`
again (a dry run) to see whether Codex trusts them yet (`codex hooks: SessionStart trusted, ...`).

The hooks register each session with zm, mark it `running`, `idle` or `needs_human`, and print one
short line on each prompt when mail is waiting. The `zm mcp` server gives MCP clients the same verbs as
the CLI. There is also a Claude Code plugin folder (`integrations/claude-code/`) and a short AGENTS.md
snippet for your repos (`integrations/AGENTS.md.snippet`). Everything an agent reads at startup must fit
in 8 KiB, and a test enforces that.

### Sandbox tools for an agent (the compute plugin)

`zm-compute` gives an agent throwaway VMs (`sandbox_create`, `_exec`, `_list`, `_renew`, `_stop`) through
`zm mcp --sandboxes`, on a machine that may not have zm yet. It has no hooks; the default `zm` plugin keeps
its tools as they are. Claude Code:

```sh
claude plugin marketplace add sengirunners/mezze     # or a clone: /plugin marketplace add .
claude plugin install zm-compute@zm --config install_zm=true
```

In `/plugin` the install asks once, "Install zm if it is missing". With it on, the first session with no zm downloads
the zm release the plugin pins (`integrations/claude-code-compute/zm-release.pin`), refuses it unless its SHA256SUMS has
the pinned SHA-256 and the tarball matches that file, unpacks it into `~/.local/lib/zm/<tag>`, links
`~/.local/bin/zm` and starts the daemon. That first start downloads about 40 MB before the server answers, so Claude
Code may give up waiting ("connection timed out") and skip the server for a while: once the install is done, reconnect
it in `/mcp` (`plugin:zm-compute:zm`, Reconnect). A zm you already have is used as it is. With it off, the server says how to
install and exits; `scripts/zm-install` in the plugin does the same install by hand (it asks first). If the release
cannot be downloaded, the message says why: install zm from a clone (`make install`). So does a pin with a blank `tag=`
(a copy that names no release yet).
To create a box the project needs a box target: join a pool or add one (`zm target ls`).

Codex reads the same marketplace (`codex plugin marketplace add sengirunners/mezze`, then
`codex plugin add zm-compute@zm`); its entry runs `zm` from your `PATH`, so run the plugin's `scripts/zm-install` once
(and `zm daemon --detach`). macOS does not put `~/.local/bin` on `PATH`: add it, or skip the plugin and put this in
`~/.codex/config.toml`, with the full path (TOML does not expand `~`):

```toml
[mcp_servers.zm-compute]
command = "/Users/you/.local/bin/zm"   # the full path zm-install printed
args = ["mcp", "--sandboxes"]
enabled_tools = ["sandbox_create", "sandbox_exec", "sandbox_list", "sandbox_renew", "sandbox_stop"]
```

## The tailnet upgrade

On one machine zm needs no network at all: the Unix socket only lets your OS user in. To reach it from
other machines, or to share it with a guest, join your Tailscale tailnet:

```sh
zm tailnet up            # or: zm daemon --tailnet; prints a login URL the first time
zm identity add alice --tailscale alice@example.com && zm grant alice member myapp
```

The node is named `potluck-<machine>` (older nodes keep `zm-<machine>`), and the tailnet is the only network listener. Every caller on it needs a
token or a tailnet login mapped to an identity; anything else gets 401. Scope checks run inside the daemon
on every request and on the event stream. `zm peers` lists other zm nodes. In v0, daemons do not
forward mail to each other.

## Put your team on your own domain

Teammates can join without Tailscale when the home is reachable at an https name on your own domain (a zone in
your Cloudflare account). On the home, as the user who runs zm (not root):

```sh
zm tunnel setup zm.example.com
```

It downloads the pinned cloudflared, prints a Cloudflare login URL (open it on any device, even from a headless
box, and pick the zone), creates the tunnel and the DNS record, runs cloudflared as your launchd or systemd user
service, starts zm's listener and checks `https://zm.example.com/v0/health` from outside. Then:

```sh
zm invite myapp --as sam --role member --via tunnel       # an https code; zm join <code> on Sam's machine
zm tunnel status                                          # what it made, and one look from outside
zm tunnel remove                                          # undo (it prints the one dashboard step: the DNS record)
```

Re-running setup is safe. If the check reports a challenge, turn Bot Fight Mode off for the zone (Security > Bots).
On a Linux server, `loginctl enable-linger $USER` keeps the service up while nobody is logged in. Cloudflare sees
the traffic in plain text (it cannot act as anyone); a team that must not be readable by Cloudflare uses the tailnet
(SPEC.md, section 17.11).

## A team on one project

One machine is the project's **authority**; each teammate's machine **joins** it and runs a replica that
forwards the project's work and lease calls to the authority over the tailnet. Teammates never get a tracker
password or a token they can copy: the join token works only from their own zm daemon.

```sh
# on the authority (an always-on machine), once:
zm tailnet up                          # the node potluck-<machine>; a login URL the first time
zm project team acme                 # this machine becomes acme's authority
zm invite acme --as ana --role member           # prints a one-time code, valid 24 h
zm invite acme/api --as sam --role guest     # a guest of one area
zm invite acme --as build-01 --role member --allow-tagged   # a tagged build box
zm invite ls                           # live, redeemed, expired; zm invite rm <id>

# on the teammate's machine (logged in to the same tailnet):
cd ~/code/acme && zm join potluck-host:abcdefghijklmnop:K7QM-3XRT
zm work ls                             # this machine's projects plus acme, from the authority
zm claim local:k3x9qa                  # a compare-and-set at the authority: one winner
zm remotes                             # what this machine joined; zm leave acme
zm run claude                          # in the checkout: a local session in acme, reported to the authority
zm send ana-mac/4bb7545a "PR is up"     # mail to a session on another machine of the team project
zm inbox                               # this machine's mail plus acme's, from the authority
zm ls                                  # your sessions and the team's (presence), with each one's last heartbeat
```

`zm join` brings this machine's tailnet node up first, pins the authority's node from the code, writes the
token to `~/.zm/remotes/<project>.token` (0600), records this checkout (or `--path`), and says what you can see.
Writes from a recorded checkout go to the team project; `-P acme` works anywhere, and `zm project why` names
the rule (`team`). A guest sees only its area, exactly as through a token; its checkout is recorded only when
its area lists the repo's remote (it never learns the project's), so a guest passes `--path .` to `zm join`, or
pins the checkout later with `zm project use acme`. Agents on a replica reach the team project through the socket without a token (a
`zm token mint` token there acts in that machine's own projects only). If the owner revokes your access, `zm remotes`
shows `revoked` and calls say so; ask for a new invite or `zm leave acme`.

Mail and presence cross machines inside a team project: sessions you start in the checkout are local
(attach, input and stop stay on your machine) and your daemon reports them to the authority every 15 s, so mail to
`ana-mac/4bb7545a`, to an item you hold or to `machine:ana-mac` reaches you, `zm who` names the holder with its
last heartbeat, and the authority's `zm ls` shows your sessions. Hooks never wait on the tailnet: the
prompt notice counts team mail as of the last heartbeat (up to 15 s late). A session whose heartbeats stop for 60 s
counts as idle, so its bound lease stops renewing; if a lease of yours goes (expired, force-released), your session
turns `needs_human` with note `lease lost: <id>`. `zm leave` (or the owner's `zm token revoke`, or `zm revoke` of your last role in the project) ends your sessions at
the authority at once and frees your machine name there, so a reimaged box rejoins under the same name with a new
invite (an invite code is spent by the join: a second machine, or a rejoin after `zm leave`, needs a new one); sessions of a replica that just disappears end once their heartbeat is older than `bound_lease_ttl` + 2 min.
An unreachable authority costs a merged list (`zm ls`, `zm inbox`, `zm work ls`) about 2 s and is named in its
warnings. Not yet: the authority's live event stream on a replica, `zm run --work` of a team item, and local copies for offline reads; a replica's reads
cost one tailnet round trip.

### Rehearsing a team on one machine

Before anyone has a tailnet node, run the whole team on one machine. `scripts/loopnet` (a development tool, not part
of `zm`) runs several daemons, each in its own `ZM_HOME` with its own socket, on the loopback tailnet the tests
use (`potluck/kit/tailnet/tailnettest`: faked node identities, no auth key, nothing leaves the machine):

```sh
go run ./scripts/loopnet host:you:/tmp/zm-host ana-mac:ana:/tmp/zm-ana sam-mac:sam:/tmp/zm-sam
ZM_HOME=/tmp/zm-host zm init && ZM_HOME=/tmp/zm-host zm project team acme     # the authority, in a checkout
ZM_HOME=/tmp/zm-host zm invite acme --as ana --role member                    # then, in ana's checkout:
ZM_HOME=/tmp/zm-ana zm join potluck-host:...                                    # and so on, per machine
```

Each argument is `machine:human:home[:tagged]`; ctrl-c stops every daemon. Keep the homes short (socket paths
are limited to 103 bytes).

## Share compute

A pool is a team project whose members lend machines as throwaway VMs (boxes) and run sandboxes on each
other's. Free, but everyone needs a [Tailscale](https://tailscale.com) login: each machine joins the owner's tailnet or has
the owner's machine shared to it (or the owner uses their own domain behind Cloudflare, `--via tunnel`, and nobody
needs Tailscale). See the co-op
quickstart above for what you need first. Three commands:

```sh
# the pool's owner, once (an always-on machine; starts its tailnet node, printing a login URL the first time;
# prints each friend's code, their machine's class and how to share this machine with them, then returns; run
# it again for a friend who comes later):
zm pool create friends --host bob --renter ana   # --public-host for a stranger; --image <name>@sha256:<digest> for your own box image

# a friend lending a Mac or Linux box (the code from the owner; a missing root step prints as one sudo line):
zm pool host 'potluck-host:abcdefghijklmnop:K7QM-3XRT'

# a friend using the pool:
zm pool join 'potluck-host:abcdefghijklmnop:Q2WE-8RTY'
zm sandbox create -P friends && zm sandbox exec <handle> -- uname -a

# a job: this checkout's commit (with --dirty, edits to tracked files too) runs on a free pool machine; its output
# streams here, the command exits with the job's code, and the box is gone afterwards (--keep keeps it):
zm job run -P friends --timeout 30m -- make test     # no command: the commit's .zm/job
```

The code reaches a box from your machine, not from a remote: `zm sandbox create --ref HEAD` (and `zm job run`)
sends the commit as a git bundle in checksummed chunks through the box's exec and checks it out in `~/checkout`;
no token or key enters the box, and a push cut off resumes (`zm sandbox push <handle>`). Other files and
directories go either way the same way: `zm sandbox cp ./data <handle>:work/`, `zm sandbox cp <handle>:work/out .`.

A host is a guest of the pool's `hosts` area and sees nothing else; a renter is a member of the team area. Each host
invite carries its machine's class, so `create` returns at once: `--host` trusted (boxes placed, renters' secrets allowed),
`--public-host` public (secret-free boxes, used only when named). `zm pool host` pulls and runs a quick conformance
check before it joins, so a machine that fails never spends the invite (every security check once, a boot per size;
`--full` for the whole suite), and on a Mac lends only while you are away: a box ends
the moment you touch the keyboard or mouse, is never billed, and none starts until you have been idle 5 minutes
(`--reclaim-idle-s`). On a Linux host whose uplink you guard, `--net-mbit N` caps what all its boxes download
together (`--net-mbit 0` removes the cap; `zm host doctor` shows it). Each step is an ordinary command (`zm host share --pool`, `zm invite`, `zm join`); SPEC.md, section 20.
A lent Mac's macOS guests serve any pool member and, on a trusted host, GitHub Actions jobs from any linked org;
selling Mac minutes and pooled iOS Simulator time are not offered.

## Asks: who can answer

An ask is a decision card for a person. An agent sends it with `zm ask new` and carries on; the answer comes back
later, and every decision is a signed receipt.

**On a default install, an agent can answer an ask too.** The project's owner answers from this machine with
`zm ask answer <id> <choice>` (or a cockpit's choice buttons), with no confirmation step. But every process running
as your OS user on this machine reaches the same Unix socket, so an agent running there can answer as well. The
receipt then records `method: owner` and the machine: it says whose socket took the answer, not that a person
pressed the button (SPEC.md, section 16). Two kinds are refused from the socket and take a signed answer: a pull
request's ask (a paired device), and a card choice that runs something when picked.

Two things are stronger than the socket; only the first makes an answer provably a person's:

- **A paired device**, on your tailnet (below). Its right is bound to that device's Tailscale node; the socket and
  tokens cannot decide, and pairing cannot be done from this machine. `zm approver add <name>` prints the code,
  and `zm approver pair-me` pairs your own phone with the project's Decisions page.
- **A key approver**: the cockpit app holds a key that signs its answers, bound to a person by a GitHub sign-in
  (SPEC.md, section 16, also lists an older way). No `zm` command creates one. Today that key is a software key in a
  file your OS user can read, so it is stronger than the socket but a local agent of that user could still use it.

### Approvals from your phone

Pair a phone that is on your tailnet; only that device can decide, and every decision is a signed
receipt a CI gate can check offline:

```sh
zm approver add alice   # prints a one-time code and a QR; open it on the phone and tap Pair
zm ask new --repo acme/app --pr 7 --head "$(git rev-parse HEAD)" "Merge the migration?"
zm ask receipt <id> > receipt.json     # after you tap Approve on the phone (the ask's mail cannot be
                                        # acked or replied to from the machine; the decision answers it)
zm receipt key                          # commit this line; then in CI, with no daemon:
zm receipt verify receipt.json --key ed25519:... --repo acme/app --pr 7 --head <sha> --decision approve
```

Pairing is refused from the machine itself; only the very first approver pairs without a confirmation. Every
later one (even after you remove them all) must be confirmed on an existing approver's phone; if none is left,
delete `$ZM_HOME/approvals/bootstrapped` by hand to start over. See SPEC.md section 16.

## Writing a work adapter

Trackers plug in as executables named `zm-work-<name>` on your PATH. The daemon runs one per call, writes
one JSON request to its stdin, and reads one JSON response from its stdout (or, if `describe` says
`"serve": true`, keeps one `--serve` process and exchanges JSON lines with a `"rid"`; SPEC.md section 11):

```json
{"zm": "zm/v0", "verb": "claim", "id": "bd:solver-3x", "args": {"holder": "laptop/4bb7545a"}, "config": {"url": "https://tracker.example"}}
{"ok": true, "result": {"id": "bd:solver-3x", "source": "bd", "title": "Fix solver timeout", "status": "claimed"}}
```

The required verbs are `describe, list, get, claim, release, comment, set_status`; `link` and `changes` are
optional. zm reads tracker items from a local mirror it keeps in sync, so reads never wait on your tracker:
with `changes` (a paged full listing, then what changed since a cursor) the mirror syncs every few seconds;
without it zm lists everything every 30 s, which is fine for small trackers.
Any language works. In this repo, `internal/work/adapter` does the plumbing for Go, and `internal/work/fake`
(the test tracker) is a small working example. Check yours with `zm-work-conformance` (installed by `make install EXTRAS=1`):

```sh
zm-work-conformance zm-work-myname              # read-only checks
zm-work-conformance zm-work-myname --write --id my:1   # also claim/release/comment/... on that item
```

Then bind it to a project, live: `zm tracker add myname --prefix my --config url=https://tracker.example`
(add `--env inherit` if it needs variables from your environment: by default an adapter gets only PATH, HOME,
LANG, TERM and TZ). That writes into `~/.zm/projects/<id>/project.toml`:

```toml
[[trackers]]
adapter = "myname"   # runs zm-work-myname
prefix = "my"
config = { url = "https://tracker.example" }
```

Run targets (SPEC.md section 20: a throwaway database, or a run of a named script somewhere else) plug in as
`potluck-driver-<name>` (or `zm-run-<name>`) executables, which `zm target bind --driver <name>` looks for in the daemon program's
directory or `$ZM_HOME/drivers`, never on `PATH` (`potluck-conformance` itself does take a driver from `PATH`, so a pass there does not mean it is bound). Check one with `potluck-conformance`:

```sh
potluck-conformance zm-run-mydb --kind db --db-probe 'psql "$ZM_CONFORMANCE_URL" -c "select 1"'
potluck-conformance --print-probe > probe.sh      # commit and push this, then one more commit on top:
potluck-conformance zm-run-myrun --ref main --commit "$(git rev-parse main~1)" --repo "$(git remote get-url origin)" \
  --script probe.sh --worktree ~/code/throwaway
```

`--commit` is older than the tip of `--ref` on purpose (the ref moving after zm resolved it), and `--repo` lets the
suite check that: a driver that runs the ref instead of the commit fails only then. Without it, that check is a SKIP.

## More

- [SPEC.md](SPEC.md): the contract, covering the trust model, projects, resolution, leaks, sessions, mail,
  leases, adapters and events.
- [API.md](API.md): every HTTP endpoint and the check that guards it.
- [DESIGN-NOTES.md](DESIGN-NOTES.md): why things are the way they are.
- [CONTRIBUTING.md](CONTRIBUTING.md) and [AGENTS.md](AGENTS.md): how to contribute, and the rules for anyone
  (people or agents) working on this repo.
- The shared base and the compute bricks live in the companion repository
  [sengirunners/potluck](https://github.com/sengirunners/potluck).
- `scripts/smoke.sh`: a real daemon, end to end, in a throwaway home.
- `make bench` (`scripts/bench.sh`): p50/p95 of every hot path, process start included, against a real
  daemon in a throwaway home (with a fake 11,512-issue tracker, native issues and a replica on a generated
  11,512-issue store); fails over the ceilings in `scripts/ceilings/ceilings.json`. In CI that's `bench.yml`, red only when the same row is
  over twice in a row on main, timed on a fixed runner class (DESIGN-NOTES.md, "The speed gate").

License: [Apache-2.0](LICENSE). Third-party notices: [NOTICE](NOTICE).

## History

The Mezze app, a fork of T3 Code outside this repository, is zm's desktop cockpit.
