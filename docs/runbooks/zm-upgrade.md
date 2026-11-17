# Runbook: upgrade or restart zm on a machine

Status: in force from 2026-11-02. Contract: SPEC 2, "Swaps". Why: DESIGN-NOTES "Safe swaps".

## Upgrade

```sh
git pull && make install          # build to a temp dir, preflight, place, restart, verify, roll back on failure
```

- `make install` installs into `INSTALL_DIR` (default `$(go env GOBIN)`, else `$(go env GOPATH)/bin`). Each replaced
  file is kept as `<name>.prev`.
- A refusal prints each reason and installs nothing. Fix the build (usually: merge main, so it knows every
  migration and route the running daemon uses). `FORCE=1` swaps anyway. It is loud and recorded, and the verify
  after the swap still rolls back a daemon that does not serve what the old one did.
- Managed sessions that would end block it. Stop them, or pass `END_SESSIONS=1`.
- Never `go install` over the binary a daemon runs from: that skips the preflight and leaves no `.prev`.
- The run-target drivers come from `github.com/sengirunners/potluck`, a public module: the build
  fetches it through the Go module proxy and needs no access to that repository.
- Drivers (`zm-run-*`, `potluck-driver-*`) run only from `zm`'s own directory or `$ZM_HOME/drivers`, never PATH,
  and only while their sha256 matches `$ZM_HOME/driver_pins.json` (SPEC 20, "Drivers are pinned"). An upgrade re-pins
  the drivers it places, and puts the old pins back with the old files on a rollback. A driver changed any other way
  is refused until `zm driver pin <driver>`; `zm status` warns (exit code unchanged) and health's `drivers` lists each.
- `INSTALL_DIR` and every directory above it must not be group- or other-writable. The upgrade takes the write off
  what you own up to `$HOME` (`chmod go-w`, said on stderr); a folder above `$HOME` or not yours (a Homebrew prefix)
  refuses the upgrade before anything moves, the old build keeps running and automatic updates are held with the
  reason (`zm daemon update status`): fix it with its owner (`sudo chmod go-w <dir>`), or install zm where only you
  can write, then `zm daemon update resume` (a current updater also retries every 6 hours and on a newer release).

## Restart or stop

```sh
zm daemon restart        # preflights the binary on disk, restarts, verifies
zm daemon stop           # preflights the binary the next autostart runs
```

Only one stop, restart or upgrade runs at a time. A second one is refused and names who holds `swap.lock`.

## Under systemd or launchd

When a systemd unit or a launchd job runs the daemon as its main process, `make install`, `zm daemon upgrade` and
`zm daemon restart` restart it through that manager (`systemctl [--user] restart <unit>`, `launchctl kickstart -k`).
zm never starts a daemon of its own beside it. The unit's `ExecStart` (or the job's program) must be the binary in
`INSTALL_DIR`. If it names another path, the upgrade refuses before placing anything and names that binary: run
`make install INSTALL_DIR=<its dir>`, or point the unit at `INSTALL_DIR/zm`. Under a manager that
restarts on exit, `zm daemon stop` stops nothing for long: stop the unit with the manager.

**A systemd unit needs `KillMode=process` or sessions die with the daemon.** Holders stay in the daemon's cgroup, and
systemd's default (`control-group`, also `mixed`) kills the whole cgroup when the daemon stops or crashes. zm writes no
unit for its daemon, so add it to yours, under `[Service]`:

```sh
systemctl [--user] edit zm.service        # [Service] then KillMode=process
systemctl [--user] show -p KillMode zm.service
```

It takes effect at the next stop, with no restart. Until then `zm daemon stop`, `restart` and `upgrade` refuse while
sessions run and say why; so they do if the daemon cannot get its unit's KillMode from `systemctl` (missing, no bus,
3 s timeout). `--end-sessions` goes through and ends them. A `systemctl restart` by hand or a crash has no
such guard. systemd logs "Found left-over process" at each start under `KillMode=process`: those are the holders, and
it is expected. Agents stay in the unit's cgroup and inherit its `MemoryMax`, `MemoryMin` and `OOMScoreAdjust`. launchd
needs nothing: it kills only the job's process group, which holders have left.

## After any swap

```sh
zm status                # last swap (who, what, result); PROBLEM lines if the self-check found any (exit 1)
zm events --types dev.zm.daemon. --since 0 --no-follow   # every stop, swap and self-check change
```

A rollback leaves the previous binary running and exits 1, saying why. "The daemon is down" or "the rejected daemon
still runs" means the rollback itself failed. Read `$ZM_HOME/daemon.log`, then start the right binary by hand
(`zm daemon --detach`, or through the unit).

"NOT rolled back" (result `rolled_forward`) means the new build had already migrated the databases, so the previous
build could not read them: the new daemon keeps running and the swap exits 1 with what failed. Fix forward with a
newer build; the preflight's "the swap is one-way" note says in advance when an upgrade applies migrations.

## One-way upgrades (release notes)

Builds that change what an older build can read, newest first. A downgrade past one fails closed: say so before
rolling a lending host back.

- **Potluck c7ac0ef: host shares and the handle ledger keyed by project id.** At its first start
  every machine writes `$ZM_HOME/host_keys.json` (empty when there is nothing to move) and adds table
  `run_host_keys` to `registry.db` (that migration applies by itself); a host that lends boxes also rewrites
  `run_targets.toml` (a `format = 2` line, shares as project ids) and `host_handles.json` (a `version`). An older zm refuses both rewritten files, so a downgraded host lends nothing until
  it is upgraded again; restore the pre-upgrade files only together with the `registry.db` of that time (potluck
  [docs/daemon.md](https://github.com/sengirunners/potluck/blob/main/docs/daemon.md) "Moving from slug-keyed files"). A hand-written `run_targets.toml` needs `format = 2` and project
  ids in `share` (SPEC 20 "Across machines").

## A host pulls green main (opt in)

An owner sets this up once **on that host**, with a canonical checkout, an authenticated `gh` with read access to
the zm repository (through `gh auth setup-git`), `git`, and Go on the scheduler's PATH. No remote shell is needed. First bootstrap a finished-green main build containing
`zm daemon update`. Run it as the same user and `ZM_HOME` as the daemon; it requires a running local daemon.

```sh
zm daemon update --repo /absolute/path/checkout --dir /absolute/path/bin \
  --hold-file /absolute/path/update.hold --quiet-window 22:00-06:00
```

This is one check: selection/build is bounded to 20 minutes, then install/verify gets its own 10-minute budget. It selects the newest successful **completed `ci.yml` push on main**
from the source repository, tolerating newer failed or unfinished main runs. It verifies the canonical
origin and main ancestry, refuses unresolved/divergent running commits, and holds if the running build is
newer than the selected green candidate (main CI must catch up). It builds that exact SHA in a disposable detached worktree, and invokes the ordinary
staged `zm daemon upgrade`. Your checkout and branch stay in place. It never passes force or end-sessions;
preflight, managed-session refusal, systemd KillMode checks, verify and rollback/roll-forward all apply.
Do not run another updater for the same home; an `update.lock` serializes timer ticks.

The hold file and daily window (host local time, start inclusive/end exclusive, supports midnight crossing)
are checked before network/build work and again before install. Equal endpoints hold all day. A held or
unchanged check exits 0; failures exit nonzero. `touch /absolute/path/update.hold` holds; remove it to resume.
Every native `projects/*/issues.db` is checked read-only before building and again before installing:
an issue still present with its ID recorded as moved out refuses the update and names the issue to repair.

The command prints a JSON result (`what=update`, `result=held|unchanged|failed|ok`) and publishes
`dev.zm.daemon.update_checked` through the local daemon. Actual installs also publish the ordinary swapped
result, including `rolled_back` or `rolled_forward`; an unsuccessful verify remains a failed update.
Check results do not replace health's last actual swap. If the daemon cannot record the event, a warning and
JSON remain in scheduler output; keep that output. Subscribe to `zm events --types dev.zm.daemon. --since 0`
for the owner/operator's post-update report.

For a systemd **user** timer, create `~/.config/systemd/user/zm-update.service` (replace every absolute path;
use your actual Go directory). This is a separate updater unit, not the daemon's unit. Its `KillMode=process` also preserves a detached
replacement daemon if the old daemon was not in its own service; updater-unit teardown must not kill that
replacement or its holders. The daemon's own unit still needs its separate `KillMode=process` guard:

```ini
[Unit]
Description=Pull green zm main
[Service]
Type=oneshot
KillMode=process
Environment=PATH=/absolute/go/bin:/absolute/bin:/usr/local/bin:/usr/bin:/bin
Environment=ZM_HOME=/absolute/zm-home
ExecStart=/absolute/bin/zm daemon update --repo /absolute/checkout --dir /absolute/bin --hold-file /absolute/update.hold --quiet-window 22:00-06:00
TimeoutStartSec=35min
```

Create `zm-update.timer` beside it, then the owner runs `systemctl --user daemon-reload` and
`systemctl --user enable --now zm-update.timer`:

```ini
[Unit]
Description=Check green zm main every fifteen minutes
[Timer]
OnBootSec=5min
OnUnitInactiveSec=15min
Persistent=true
[Install]
WantedBy=timers.target
```

On macOS, the owner creates `~/Library/LaunchAgents/local.zm.update.plist`, substitutes absolute paths, and
loads it once with `launchctl bootstrap gui/$(id -u) <absolute plist path>`. Do not replace the daemon's job.
Include the directories your agents live in (`claude` is in `~/.local/bin`, written out: launchd does not expand `~`).
A daemon keeps the directories of the previous daemon's PATH (`$ZM_HOME/daemon-path`), and a swap that leaves an
agent's binary missing rolls back, but the updater's own PATH should still find them. The first swap onto a
build with `daemon-path` depends on this PATH alone: older daemons never wrote the file.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>local.zm.update</string>
  <key>ProgramArguments</key><array>
    <string>/absolute/bin/zm</string><string>daemon</string><string>update</string>
    <string>--repo</string><string>/absolute/checkout</string>
    <string>--dir</string><string>/absolute/bin</string>
    <string>--hold-file</string><string>/absolute/update.hold</string>
    <string>--quiet-window</string><string>22:00-06:00</string>
  </array>
  <key>EnvironmentVariables</key><dict>
    <key>PATH</key><string>/Users/you/.local/bin:/absolute/go/bin:/absolute/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    <key>ZM_HOME</key><string>/absolute/zm-home</string>
  </dict>
  <key>StartInterval</key><integer>900</integer>
  <key>StandardOutPath</key><string>/absolute/update.log</string>
  <key>StandardErrorPath</key><string>/absolute/update-error.log</string>
</dict></plist>
```

Owners can disable the timer (`systemctl --user disable --now zm-update.timer`) or unload the updater job
(`launchctl bootout gui/$(id -u)/local.zm.update`). Enrollment for a source build is manual; `zm daemon update enable`
(below) schedules only the release channel.

## A machine pulls signed releases (the release channel)

The default for boxes and Macs that do not build zm (SPEC 2 "Release channel"). No Go, checkout or shell access is needed: the machine pulls, verifies
and installs on its own timer. CI publishes a release after each green push to main, daily as a backstop
(`release` workflow, `.github/workflows/zm-release.yml`), and on demand (`gh workflow run zm-release.yml`).

Where releases are read is one setting, `publicRepo` in `cmd/zm/daemon_update.go` (`sengirunners/mezze`),
then the repository the binary was built from when that is another (`buildRepo`, set at link time: the release
workflow passes its own repository, `make` the checkout's origin). The channel takes the first repo that answers
with releases (a repo that is private or has no releases yet passes to the next; a release's manifest and tarball
never come from two repos) and always release tags `zm-main-<sha12>`, never a branch. The never-go-backwards check
compares the running commit with the release's in that repo, then in the others; a release commit copied
from another repository's commit (its `Source: <sha>` line, written by `scripts/publish`) is placed by that source in
the other repo, so a copy of an older snapshot never replaces a newer build. When nothing places the two, the machine holds,
saying so (a build time says nothing about whose code is newer); install a release by hand there. A 403 (an SSO
the login is not authorized for) stops the check, like the network. The compute plugin's
`zm-release.pin` names `publicRepo` first (a build from another repository may add that repository after it), and both plugin manifests name it (`TestUpdateSourceOneSetting`): change them together. Canary one machine first: run `zm daemon update --release github --dir <dir>` there once and read its
`update-last.json`, before the rest of the fleet's timers take the build.

### A machine's own order of release repositories

A machine can read releases from another order of repositories than the built-in one: the key `update_repos` in its
own `$ZM_HOME/config.json`, a list such as `["your-org/your-repo", "your-org/your-other-repo"]` (owner/name, 1 to 8,
no duplicates). The order is used only by `--release github`; the first listed repo that answers with releases is
taken, with the same fall-through, signature, never-go-backwards and placing rules as the built-in order. On a
machine's own order the answers about history must name the commits asked, or the check holds, and a release whose
tag is yanked on any listed or built-in repo the machine can read is not taken. A yank is per tag: a public copy and
the release it was copied from have different tags, so yank each repository's tag for that build; a machine that
cannot read a repo does not see its yank. Reading the other repos for yanks fails the check like the listing does
(anything but a 404), and a chosen repo whose releases are all yanked holds rather than passing to the next.
Failed-here records and the canary's vouches and vetoes are per version too, so the same build under another
repository's tag counts as a new release. A key that is there but empty or malformed fails every release check that
gets past the holds and an unfinished swap's report, naming the problem, until it is fixed or removed; it never
falls back to the default. Nothing sets the key but an edit of that file on the machine, by hand or with the script
below: no zm command, route, token or mail does, though restoring a home from its backup brings the file, and the
key, with it.

Put a machine on another order:

1. On that machine, add `"update_repos": [...]` to `$ZM_HOME/config.json`, keeping every other key.
2. Run one check by hand: `zm daemon update --release github --dir <install dir>`. Its JSON line's `reason` ends
   with `release sources: <the order> (config update_repos)`, and its `result` is `ok`, `unchanged` or `held`, never
   `failed`. A failed result names what to fix.
3. `zm daemon update status` there ends with `release sources on <machine>: <the order> (config update_repos)`. Other
   machines read the same note in that machine's last check reason.

Return a machine to the default order:

1. Remove the `update_repos` key from its `$ZM_HOME/config.json`.
2. Run the same check by hand. Its `reason` has no `release sources:` note, and `zm daemon update status` there ends
   with `release sources on <machine>: <the built-in order> (default)`.

#### A machine built from a private repository: its own builds first

While a team works privately and publishes copies later ([zm-publish.md](zm-publish.md)), its machines run builds
of the private repository. On the default order they read the public repository first, so once it has releases
they take only those: a copy is days older than what they run, and they hold until a newer copy. Putting the build
repository first keeps them on the private releases, with the public repository after it:

```sh
zm exec --machine <name> -- scripts/releaseorder/release-order.py show    # the order it reads; changes nothing
zm exec --machine <name> -- scripts/releaseorder/release-order.py set     # [build repository, public repository]
zm exec --machine <name> -- scripts/releaseorder/release-order.py unset   # back to the default order
```

The script reads both names from that machine's own zm (the default order `zm daemon update status` prints there),
so no repository name is typed, and it changes only `update_repos`. It refuses, writing nothing, on a build without
the setting, a build that reads one repository, a home zm does not pass, or a `config.json` it cannot read back the
same (or not its user's, or a link); done, it ends with the order zm reads. On a machine that takes no remote tasks, its user runs the script itself from
a checkout of the repository (`scripts/releaseorder/release-order.py set`), as the user that runs its daemon. A key
zm cannot read (the status line says why) is not the script's: fix or remove it by hand. A home doctor warns about
(readable by others) is refused too: `chmod 700` it first.

- **When:** after the machine runs a build whose public repository name is final. The list names it explicitly: a
  machine set before a build that renames the public repository keeps the old name until `unset` then `set`.
- **Check:** the machine's next update check ends its reason with `release sources: <the order>`
  (`zm daemon update status` from any machine of the team) and is `ok`, `unchanged` or `held`, never `failed`.
- **Leave one machine on the default order** as the canary of the public path: once the public repository has
  releases it takes them, so its results show what outside machines see. Watch its checks after each publish.
- **Before the first public copy:** every machine on a private build is set, or is that canary, or is listed to the
  team with what it needs (a hand install once it holds). A machine that is not set holds on the copy.

One line per machine: `zm daemon update enable` pins the channel's key and installs a daily job (a systemd user
timer, or the LaunchAgent `dev.zm.update`) running `--release github --min-age 6h --no-canary`; `zm pool host` and
`zm host share` run it once a machine is lent (`--no-auto-update` skips it). `zm daemon update disable` removes it.
It leaves an updater it did not write (a linked `zm-update.service`, or one written from this runbook)
alone. For a ring or the canary gate, write the job by hand instead:

Once per machine: put the channel's public key in `config.json` (`"update_keys": ["ed25519:…"]`, the key printed
when the signing key was made; see "Rotating the key"), give its `gh` read access to the repository's releases,
and install the timer. Rings by `--min-age`: one canary box `0s`, the authority `3h`, other boxes and Macs `6h`.

`~/.config/systemd/user/zm-update.service` (and the 30-minute `zm-update.timer` as above):

```ini
[Unit]
Description=Install the newest signed zm release
[Service]
Type=oneshot
KillMode=process
Environment=PATH=%h/.local/bin:/usr/local/bin:/usr/bin:/bin
ExecStart=%h/.local/bin/zm daemon update --release github --dir %h/.local/bin --hold-file %h/.zm/update.hold --min-age 6h
TimeoutStartSec=35min
```

On a Mac, the launchd job above with `--release github --min-age 6h` in place of `--repo …`. Each check prints its
JSON result and publishes `dev.zm.daemon.update_checked`.

How a machine chooses: the newest release that is `--min-age` old, did not fail on this machine, and, behind the
canary (`--min-age` above 0), that another machine vouches for: it ran that release 20 minutes with no daemon
restart, and no machine lists it as failed. A busy day does not starve the rings; the canary vouches for each
release it ran for a tick. With no machine numbers to read (no team), or for time rings only, pass `--no-canary`. A
release whose install rolled back here is never retried here and keeps every other machine off it; the machine
waits for a newer one.

```sh
zm daemon update status          # each machine: version, HELD, the last check's result and why, stable, failed here
zm daemon update hold "why"      # this machine keeps the build it runs (writes $ZM_HOME/update.hold)
zm daemon update resume          # undo it; touch/rm of ~/.zm/update.hold does the same
gh release edit zm-main-<sha12> --prerelease   # yank: no machine reading that repo takes that release (never delete it: that is not a yank)
```

A hold never blocks a hand `zm daemon upgrade` or `make install`; to pin an older build, install it by hand, then
hold. A yanked release stays on machines that already run it until a newer one arrives.

Rotating the key: a planned rotation is [secret-leak.md](secret-leak.md) section 4A (it changes the build's
`ChannelKey` too, or a tick re-adds the old key); a leaked seed is section 4B. Do not run `-keygen` where an agent
transcript would keep its output.
