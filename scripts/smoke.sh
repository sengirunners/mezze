#!/usr/bin/env bash
# End-to-end smoke test of a real zm daemon in a throwaway ZM_HOME.
#
#   scripts/smoke.sh            build into the temp dir, run every step, clean up
#   KEEP=1 scripts/smoke.sh     keep the temp dir for a look afterwards
#
# Never touches ~/.zm, ~/.claude, ~/.codex or real dotfiles: HOME and
# ZM_HOME both point into the temp dir. Leaves no process behind (checked).
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
T=$(mktemp -d /tmp/zm-smoke.XXXX) # short: Unix socket paths max 103 bytes
GO=$(command -v go)
pass=0
fail=0
daemon_pids=()

say() { printf '\n== %s\n' "$*"; }
ok() { pass=$((pass + 1)); printf '  ok   %s\n' "$*"; }
bad() { fail=$((fail + 1)); printf '  FAIL %s\n' "$*"; }
check() { # check "what" command...
	local what=$1
	shift
	if "$@"; then ok "$what"; else bad "$what"; fi
}
has() { grep -q -- "$2" <<<"$1"; }    # has "$text" pattern
hasnt() { ! grep -q -- "$2" <<<"$1"; } # hasnt "$text" pattern
until_true() {                         # until_true seconds command...: poll every 0.2 s
	local n=$(($1 * 5))
	shift
	for ((i = 0; i < n; i++)); do
		if "$@" 2>/dev/null; then return 0; fi
		sleep 0.2
	done
	return 1
}

cleanup() {
	set +e
	export ZM_NO_AUTOSTART=1 # never start a daemon while cleaning up
	for h in "$T/home" "$T/adm" "$T/trk" "$T/zh/.zm"; do
		[ -S "$h/zm.sock" ] || continue
		for s in $(ZM_HOME=$h zm ls --json 2>/dev/null | jq -r '.sessions[]? | select(.managed) | .address'); do
			ZM_HOME=$h zm kill --signal KILL "$s" >/dev/null 2>&1
		done
		ZM_HOME=$h zm daemon stop --force >/dev/null 2>&1
	done
	[ -n "${events_pid:-}" ] && kill "$events_pid" 2>/dev/null
	for p in "${daemon_pids[@]}"; do kill "$p" 2>/dev/null; done
	if [ -z "${KEEP:-}" ]; then rm -rf "$T"; else echo "kept $T"; fi
}
trap cleanup EXIT

# A clean environment: nothing from the caller's agent session or real home.
unset ZM_AS ZM_PROJECT ZM_TOKEN ZM_HOST ZM_SESSION CLAUDE_CODE_SESSION_ID CODEX_THREAD_ID
export GOCACHE=$("$GO" env GOCACHE) GOMODCACHE=$("$GO" env GOMODCACHE) GOPATH=$("$GO" env GOPATH)
export HOME=$T/userhome ZM_HOME=$T/home ZM_MACHINE=smoke ZM_HUMAN=ana
export GIT_AUTHOR_NAME=smoke GIT_AUTHOR_EMAIL=smoke@example.invalid GIT_COMMITTER_NAME=smoke GIT_COMMITTER_EMAIL=smoke@example.invalid
export PATH=$T/bin:$T/fakebin:/usr/bin:/bin:$(dirname "$GO")
mkdir -p "$HOME" "$ZM_HOME" "$T/bin" "$T/fakebin"

say "build"
(cd "$repo" && "$GO" build -o "$T/bin/" ./cmd/... && "$GO" test -c -o "$T/fakebin/work.test" ./internal/work &&
	"$GO" test -c -o "$T/fakebin/approvals.test" ./internal/approvals)
ln -s "$T/fakebin/work.test" "$T/fakebin/zm-work-fake" # a fake tracker adapter (internal/work/fake)
ok "zm, zm-work-*, fake tracker adapter built in $T"

cat >"$ZM_HOME/config.json" <<'EOF'
{"admission": {"min_memory_free_percent": 1, "max_swap_used_mb": 100000000, "min_disk_free_mb": 10},
 "lease_tick_seconds": 1, "idle_after_seconds": 2}
EOF

say "zero-install start: a fresh HOME, no daemon, two commands"
Z=$T/zh
mkdir -p "$Z/code/demo/.git"
printf '[remote "origin"]\n\turl = git@github.com:acme/demo.git\n' >"$Z/code/demo/.git/config"
zp() { (cd "$Z/code/demo" && env -u ZM_HOME HOME="$Z" zm "$@"); }
out=$(zp status 2>&1) && rc=0 || rc=$?
check "status with no daemon: exit 3, starts nothing" test "$rc:$(test -e "$Z/.zm/zm.sock" && echo sock)" = "3:"
out=$(zp hook claude prompt '{"session_id":"zero"}' 2>&1) && rc=0 || rc=$?
check "a hook with no daemon: silent, exit 0, starts nothing" test "$rc:$out:$(test -e "$Z/.zm/zm.sock" && echo sock)" = "0::"
out=$(ZM_NO_AUTOSTART=1 zp work ls 2>&1) && rc=0 || rc=$?
check "ZM_NO_AUTOSTART=1: refused with exit 3" test "$rc" = 3
zp init >"$T/zinit.out" 2>"$T/zinit.err"
check "init starts the daemon with exactly one stderr line" test "$(cat "$T/zinit.err")" = "zm: started the daemon (~/.zm)"
daemon_pids+=("$(zp status --json | jq -r .pid)")
check "init: slug from the repo, path and remote rules" has "$(cat "$T/zinit.out")" "created project demo (slug from the repo name; path .*, remote github.com/acme/demo)"
check "init prints the next commands" has "$(cat "$T/zinit.out")" "zm run claude"
check "init wrote nothing into the repo" test ! -e "$Z/code/demo/.zm"
out=$(zp work new "first task" 2>&1)
check "work new: created demo/local:..., no second start line" test "$(grep -c '^created demo/local:' <<<"$out"):$(grep -c started <<<"$out")" = "1:0"
check "init again names the project, exit 0" has "$(zp init)" "is already project demo"
out=$(printf '%s\n' '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"s","version":"0"}}}' \
	'{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"work_list","arguments":{}}}' | env -u ZM_HOME HOME="$T/nobody" zm mcp)
check "mcp with no daemon: initialize answers, tool calls say daemon not running" test "$(grep -c '"id":1,"result"' <<<"$out"):$(grep -c 'daemon not running' <<<"$out")" = "1:1"
zp daemon stop >/dev/null

say "daemon up"
# The daemon's shell points at a live farm server (unreachable, made-up values; the password is a placeholder): the
# environment allowlist (SPEC 17.2) must keep every one of them away from adapters.
BEADS_DOLT_SERVER_HOST=203.0.113.1 BEADS_DOLT_SERVER_PORT=1 BEADS_DOLT_SERVER_MODE=1 BEADS_DOLT_PASSWORD=changeme \
	zm daemon --detach >/dev/null
daemon_pids+=("$(zm status --json | jq -r .pid)")
w=$(zm whoami --json)
check "whoami: human:ana over potluck/1" test "$(jq -r '.as + " " + .protocol' <<<"$w")" = "human:ana potluck/1"

say "projects and areas"
mkdir -p "$T/acme/solver" "$T/acme/billing" "$T/side"
zm project new acme --path "$T/acme" >/dev/null
zm project new side --path "$T/side" >/dev/null
zm area add solver --project acme --path "$T/acme/solver" >/dev/null
zm area add billing --project acme --path "$T/acme/billing" >/dev/null
acme=$(zm project show acme --json | jq -r .id)
side=$(zm project show side --json | jq -r .id)
why=$(cd "$T/acme/solver" && zm project why)
check "project why in acme/solver names the area and the rule" has "$why" "acme/solver (rule: cwd-path)"
why=$(cd "$T" && zm project why 2>&1 || true)
check "project why outside every project refuses (no silent default)" hasnt "$why" "acme"
check "project why outside every project exits non-zero" bash -c "cd '$T' && ! zm project why >/dev/null 2>&1"

solver_item=$(cd "$T/acme/solver" && zm work new "solver bug" --json | jq -r .id)
billing_item=$(cd "$T/acme/billing" && zm work new "billing secret" --json | jq -r .id)
side_item=$(cd "$T/side" && zm work new "side thing" --json | jq -r .id)
zm send owners "side owners only" --project side >/dev/null
zm send human:ana "billing payroll note" --project acme --area billing >/dev/null
zm send human:ana "solver note for the area" --project acme --area solver >/dev/null
check "three work items in three scopes" test -n "$solver_item$billing_item$side_item"

say "guest vida: acme/solver only"
G=$(zm token mint --as vida --role guest --project acme --area solver | grep '^potluck_')
guest() { ZM_TOKEN=$G "$@"; }
check "guest is human:vida via token" has "$(guest zm whoami)" "human:vida (identity vida, via token"
out=$(guest zm project ls)
check "guest project ls: acme with only solver" has "$out" "areas: solver"
check "guest project ls: no side, no billing" hasnt "$out" "side\|billing"
out=$(guest zm work ls --json)
check "guest work ls: the solver item" has "$out" "$solver_item"
check "guest work ls: no billing or side item" hasnt "$out" "$billing_item\|$side_item\|billing secret\|side thing"
zm send human:vida "solver note for vida" --project acme --area solver >/dev/null
out=$(guest zm mail --json)
check "guest mail: its solver note only (not ana's needs=human notes, billing or side)" test "$(jq -r '[.mail[].body] | join(",")' <<<"$out")" = "solver note for vida"
hidden=$(guest zm project show side 2>&1 || true)
missing=$(guest zm project show nope 2>&1 || true)
check "guest: hidden project answers exactly like a missing one" test "${hidden//side/X}" = "${missing//nope/X}"
out=$(guest zm project show acme/solver --json 2>&1 || true)
check "guest project show acme/solver: the area, as a guest" test "$(jq -r '.name + " " + .role' <<<"$out" 2>/dev/null)" = "solver guest"
hidden=$(guest zm project show acme/billing 2>&1 || true)
missing=$(guest zm project show acme/nope 2>&1 || true)
check "guest: hidden area answers exactly like a missing one" test "${hidden//billing/X}" = "${missing//nope/X}"
zm area set acme/solver --label solverlabel >/dev/null
check "area set applies live (no restart)" test "$(zm project show acme/solver --json | jq -r '.labels | join(",")')" = "solverlabel"
check "area set wrote project.toml" grep -q solverlabel "$ZM_HOME/projects/$acme/project.toml"
out=$(guest zm area set acme/solver --work '*' 2>&1) && rc=0 || rc=$?
check "guest cannot widen its own area" test "$rc:$(grep -c 'needs admin' <<<"$out")" = "1:1"
zm area set acme/solver --label '' >/dev/null
check "a cleared list answers [], never null" test "$(zm project show acme/solver --json | jq -c '[.labels, .work, .remotes]')" = "[[],[],[]]"
hidden=$(guest zm work show "$billing_item" --project acme 2>&1 || true)
missing=$(guest zm work show local:zzzzzz --project acme 2>&1 || true)
check "guest: hidden-area item answers exactly like a missing one" test "${hidden//$billing_item/X}" = "${missing//local:zzzzzz/X}"
out=$(guest zm events --since 0 --no-follow)
check "guest event replay: only acme/solver events" test -z "$(jq -c "select(.zmproject != \"$acme\" or .zmarea != \"solver\")" <<<"$out")"
check "guest event replay: sees its area's events" has "$out" "solver note\|work.created"
ZM_TOKEN=$G zm events >"$T/guest-live.jsonl" 2>"$T/guest-live.err" &
events_pid=$!

say "a fake agent in a worktree (zm run --worktree)"
cat >"$T/fakeagent" <<'EOF'
#!/bin/sh
# Fake agent: runs each line typed into it; records the daemon's nudge line.
printf 'fake agent ready\n'
while IFS= read -r line; do
	case "$line" in
	"zm: you have mail"*) echo nudged >>"$ZM_SCRATCH/nudged" ;;
	*) eval "$line" ;;
	esac
done
EOF
(cd "$T/acme/solver" && git init -q && git commit -q --allow-empty -m init)
S=$(cd "$T/acme/solver" && zm run -d --worktree --name fake --agent shell /bin/sh "$T/fakeagent")
s8=${S#smoke/}
ss=$(zm session show "$S" --json)
check "session is managed in acme/solver" test "$(jq -r '"\(.managed) \(.project) \(.area)"' <<<"$ss")" = "true $acme solver"
check "session records what ran" test "$(jq -r '.agent + " " + (.command | join(" "))' <<<"$ss")" = "shell /bin/sh $T/fakeagent"
wt=$(jq -r .worktree <<<"$ss")
check "worktree on branch zm/$s8" test "$(git -C "$wt" rev-parse --abbrev-ref HEAD)" = "zm/$s8"
scratch=$(jq -r .scratch <<<"$ss")
run_in() { zm session input "$S" "$1" >/dev/null; }

say "mail: direct, group mailbox, notice inside the session"
out=$(zm send "$S" "please look at the solver bug" --project acme)
check "mail to the session is direct" has "$out" "($S\|direct)"
gid=$(zm send machine:smoke "group: anyone free?" --project acme --area solver --json | jq -r .id)
check "group mail goes to the group mailbox" test "$(zm mail --to machine:smoke --json | jq -r '.mail[0].via')" = "group"
run_in 'zm notice > $ZM_SCRATCH/notice; echo "$ZM_AS" > $ZM_SCRATCH/as'
until_true 10 test -s "$scratch/as" || true
check "ZM_AS inside the session is its address" test "$(cat "$scratch/as" 2>/dev/null)" = "$S"
notice=$(cat "$scratch/notice" 2>/dev/null)
check "zm notice inside the session: unread mail incl. group" has "$notice" "zm: 2 unread (1 group)"
check "zm notice names the command to read mail" has "$notice" "Run: zm inbox"
check "zm notice never includes mail bodies" hasnt "$notice" "please look at the solver bug\|group: anyone free?"
run_in 'zm notice > $ZM_SCRATCH/notice-again; echo done > $ZM_SCRATCH/notice-again-done'
until_true 10 test -s "$scratch/notice-again-done" || true
check "zm notice announces each event once per session" test "$(cat "$scratch/notice-again-done" 2>/dev/null):$(cat "$scratch/notice-again" 2>/dev/null)" = "done:"
run_in "zm ack $gid > \$ZM_SCRATCH/ack 2>&1"
until_true 10 test -s "$scratch/ack" || true
check "the session takes the group mail (ack records the taker)" test "$(zm mail --to machine:smoke --json | jq -r ".mail[] | select(.id == \"$gid\") | .acked_by")" = "$S"

say "work: claim, mail the item, lease expiry"
run_in "zm claim acme/$solver_item --note fixing > \$ZM_SCRATCH/claim 2>&1"
until_true 10 test -s "$scratch/claim" || true
check "the session holds the item" has "$(zm who "acme/$solver_item")" "held by $S"
out=$(zm send "acme/$solver_item" "status of the fix?")
check "mail to the item reaches its lease holder" has "$out" "to $S (holder of $solver_item)"
zm lease lock:deploy --ttl 2 --project acme --note "short on purpose" >/dev/null
expired() { zm events --since 0 --no-follow --types dev.zm.work.lease_expired | jq -e 'select(.subject == "lock:deploy")' >/dev/null; }
check "an expired lease publishes lease_expired" until_true 10 expired
check "an expiry mails no person" hasnt "$(zm mail --needs human --open --json)" "lock:deploy"
check "lease_expired event, project-tagged" test "$(zm events --since 0 --no-follow --types dev.zm.work.lease_expired | jq -r '.subject + " " + .zmproject')" = "lock:deploy $acme"
check "the expired lease is gone" hasnt "$(zm leases)" "lock:deploy"

say "guest scope: locks across the area boundary"
(cd "$T/acme/solver" && zm lease lock:solverjob --ttl 2 --note "solver area" >/dev/null)
solverexp() { zm events --since 0 --no-follow --types dev.zm.work.lease_expired | jq -e 'select(.subject == "lock:solverjob" and .zmarea == "solver")' >/dev/null; }
check "an expired solver-area lease publishes lease_expired in solver" until_true 10 solverexp
check "the guest gets no mail about a person's expired lease" hasnt "$(guest zm mail --json)" "lock:solverjob"
zm lease lock:migrate --project acme >/dev/null
out=$(guest zm lease lock:migrate --project acme 2>&1) && rc=0 || rc=$?
check "a project-level lock covers the guest's area (conflict)" test "$rc:$(grep -c 'is held by' <<<"$out")" = "1:1"
check "the lock is held once" test "$(zm leases --json | jq '[.[] | select(.key == "lock:migrate")] | length')" = 1
zm lease lock:migrate --release --project acme >/dev/null

say "network admins never set what the daemon runs or reads"
O=$(zm token mint --as owen --role owner --project side | grep '^potluck_')
out=$(ZM_TOKEN=$O zm project set side --path /etc 2>&1) && rc=0 || rc=$?
check "an owner token cannot set repo paths (machine settings)" test "$rc:$(grep -c 'machine settings' <<<"$out")" = "1:1"
check "the project kept its paths" test "$(zm project show side --json | jq -r '.paths | join(",")')" = "$T/side"
rename_side() { ZM_TOKEN=$O zm project set side --name "Side Project" >/dev/null; }
check "an owner token can still rename" rename_side

say "a session the daemon does not know gets no group mail"
sgid=$(zm send machine:smoke "side group job" --project side --json | jq -r .id)
check "unknown session: empty notice" test -z "$(ZM_AS=smoke/deadbeef zm notice)"
check "unknown session: no group mail in its inbox" test "$(ZM_AS=smoke/deadbeef zm inbox --json | jq '.mail | length')" = 0
out=$(ZM_AS=smoke/deadbeef zm ack "$sgid" 2>&1) && rc=0 || rc=$?
check "unknown session cannot take group mail (hint: install hooks)" test "$rc:$(grep -c 'zm install' <<<"$out")" = "1:1"

say "hooks: needs_human first in zm ls; session end"
hook() { printf '%s' "$2" | zm hook claude "$1"; }
hook session-start "{\"session_id\":\"11111111-aaaa\",\"cwd\":\"$T/acme\"}" >/dev/null
hook notification "{\"session_id\":\"11111111-aaaa\",\"cwd\":\"$T/acme\",\"notification_type\":\"permission_prompt\",\"message\":\"Claude needs your permission to use Bash\"}"
hook session-start "{\"session_id\":\"22222222-aaaa\",\"cwd\":\"$T/side\"}" >/dev/null
first=$(zm ls | grep -m1 'smoke/')
check "zm ls puts the needs_human session first" has "$first" "^! smoke/11111111 .*needs_human"
hook session-end "{\"session_id\":\"22222222-aaaa\",\"cwd\":\"$T/side\"}"
check "SessionEnd hook ends an unmanaged session (outcome ok)" test "$(zm session show smoke/22222222 --json | jq -r '.state + " " + .outcome.kind')" = "ended ok"
out=$(guest zm ls -a)
check "guest ls: the solver session only" test "$(grep -c 'smoke/' <<<"$out")" = 1
check "guest ls: no project-level or side sessions" hasnt "$out" "11111111\|22222222"

say "activity, who and reach; the terminal title"
hook tool "{\"session_id\":\"11111111-aaaa\",\"cwd\":\"$T/acme\",\"tool_name\":\"Bash\",\"tool_input\":{\"command\":\"echo SECRETCMD\",\"description\":\"Print a marker\"}}"
act=$(zm session show smoke/11111111 --json)
check "PreToolUse hook: running, activity from Bash's description" test "$(jq -r '.state + "|" + .activity' <<<"$act")" = "running|Print a marker"
check "the command itself never reaches the session" hasnt "$act" "SECRETCMD"
check "who and reach: this machine's owner, all" test "$(jq -r '"\(.who.name) \(.who.kind) \(.reach) \(.activity_at != null)"' <<<"$act")" = "ana human all true"
run_in "printf '\\033]0;smoke title\\007'"
titled() { test "$(zm session show "$S" --json | jq -r .term_title)" = "smoke title"; }
check "a managed session's OSC title becomes term_title" until_true 10 titled
fgseen() { test "$(zm session show "$S" --json | jq -r '"\((.fg.name // "") | test("sh$")) \(.busy // false) \(.fg.pid == .pid)"')" = "true false true"; }
check "the owner sees a managed session's foreground process (its shell, not busy)" until_true 10 fgseen
# The title's event is coalesced (SPEC 8: one updated per 5 s per session, so it may trail the row by 5 s),
# and a state change inside that gap (idle after 2 s) cancels it and carries the Session itself. The log is
# stored: wait on it, bounded, never on session show.
titlevent() { zm events --since 0 --no-follow --subject "$S" --types dev.zm.session.updated,dev.zm.session.state_changed |
	jq -e 'select(.data.term_title == "smoke title")' >/dev/null; }
check "a title change publishes the session (dev.zm.session.updated, coalesced)" until_true 15 titlevent
moved() { test "$(zm session move smoke/11111111 side --json | jq -r .project)" = "$(zm project show side --json | jq -r .id)"; }
check "zm session move puts a session in another project" moved
check "and back" test "$(zm session move smoke/11111111 acme --json | jq -r .project)" = "$acme"

say "nudge: group mail wakes an idle managed session"
idle() { test "$(zm session show "$S" --json | jq -r .state)" = idle; }
until_true 15 idle || true
zm send machine:smoke "group: second job" --project acme --area solver >/dev/null
check "the idle session got the fixed nudge line (never a mail body)" until_true 10 test -s "$scratch/nudged"

say "nudge: a half-written line is never typed into or sent"
D=$(cd "$T/acme" && zm run -d --name draft --agent shell /bin/sh -c "while IFS= read -r l; do printf '%s\n' \"\$l\" >>'$T/draft-lines'; done")
idle_d() { test "$(zm session show "$D" --json | jq -r .state)" = idle; }
until_true 15 idle_d || true
zm session input "$D" half-wri --enter=false >/dev/null
until_true 15 idle_d || true
zm send "$D" "wait for the person" --project acme >/dev/null
sleep 1
check "mail did not type into the draft or send it" test ! -e "$T/draft-lines"
zm session input "$D" tten >/dev/null
check "the draft reached the agent whole" until_true 10 grep -qx half-written "$T/draft-lines"
until_true 15 idle_d || true
zm send "$D" "now it can wake" --project acme >/dev/null
check "with the draft gone, mail wakes it" until_true 10 grep -q "you have mail" "$T/draft-lines"
zm kill "$D" >/dev/null

say "events"
out=$(zm events --since 0 --no-follow)
check "every work/mail/session event carries zmproject" test -z "$(jq -c 'select((.type | test("work|mail|session")) and (.zmproject // "") == "")' <<<"$out")"
check "solver events carry zmarea=solver" has "$(jq -c "select(.subject == \"$solver_item\") | .zmarea" <<<"$out")" '"solver"'
sleep 1
kill "$events_pid" 2>/dev/null || true
wait "$events_pid" 2>/dev/null || true
events_pid=
live=$(cat "$T/guest-live.jsonl")
check "guest live SSE got events" test -n "$live"
check "guest live SSE: only acme/solver" test -z "$(jq -c "select(.zmproject != \"$acme\" or .zmarea != \"solver\")" <<<"$live")"
check "guest live SSE: nothing from side or billing" hasnt "$live" "$side\|billing\|11111111\|22222222"
check "guest live SSE: no owners-mailbox mail" test -z "$(jq -c 'select((.data | type) == "object" and .data.to == "owners")' <<<"$live")"

say "work adapter conformance (zm-work-fake)"
echo '{"issues":{"acme-1":{"id":"acme-1","title":"First","status":"open","created_at":"2026-10-30T17:00:00Z"}}}' >"$T/fake.json"
out=$(ZM_FAKE_STATE=$T/fake.json zm-work-conformance zm-work-fake --write --id bd:acme-1 2>&1) && rc=0 || rc=$?
check "conformance passes (read and write verbs)" test "$rc" = 0
check "conformance ran the write checks" has "$out" "PASS  release by the holder reopens the item"
check "the fake tracker was used" has "$(cat "$T/fake.json")" "calls"

say "tracker add (fake adapter): one command, applied live, environment allowlist"
echo '{"issues":{"side-1":{"id":"side-1","title":"Side item","status":"open","created_at":"2026-10-30T17:00:00Z"}}}' >"$T/fakebin/state.json"
out=$(cd "$T/side" && zm tracker add fake --prefix bd 2>&1) && rc=0 || rc=$?
check "tracker add: bound, no restart" has "$out" "bound bd to side"
check "tracker add: checks passed, first sync counted" bash -c 'test "$1" = 0 && grep -q "sync:   1 items" <<<"$2"' _ "$rc" "$out"
check "tracker add printed the next command" has "$out" "next: zm work ls -P side"
check "the new item is served at once" has "$(zm work ls --project side)" "side/bd:side-1"
check "project.toml has the binding" has "$(cat "$ZM_HOME/projects/$side/project.toml")" 'adapter = "fake"'
ls_out=$(zm tracker ls --project side)
check "tracker ls: allowlisted, synced" bash -c "grep -q 'env allowlist' <<<'$ls_out' && grep -q ' ok ' <<<'$ls_out'"
fenv=$(jq -r '.env[]' "$T/fakebin/state.json")
check "the adapter got nothing else of the daemon's environment" hasnt "$fenv" "ZM_\|GOCACHE\|GIT_COMMITTER\|BEADS_DOLT_SERVER\|BEADS_DOLT_PASSWORD"
check "tracker test passes" bash -c "zm tracker test bd --project side >/dev/null"
check "tracker add of a taken prefix is refused" bash -c "! zm tracker add fake --prefix bd --project side >/dev/null 2>&1"
check "tracker routes are local: a guest token is refused" bash -c "! ZM_TOKEN=$G zm tracker ls >/dev/null 2>&1"
rm_out=$(zm tracker rm bd --project side)
check "tracker rm unbinds" has "$rm_out" "unbound bd from side"
check "after rm the item is gone from work ls" hasnt "$(zm work ls --project side)" "bd:side-1"

say "tracker items: a lease follows its item when area set moves it"
TH=$T/trk
mkdir -p "$TH"
cp "$ZM_HOME/config.json" "$TH/"
echo '{"issues":{"acme-q1":{"id":"acme-q1","title":"QA one","status":"open","labels":["qa"],"created_at":"2026-10-30T17:00:00Z"}}}' >"$TH/fake.json"
tp() { ZM_HOME=$TH ZM_FAKE_STATE=$TH/fake.json zm "$@"; }
tp daemon --detach >/dev/null
daemon_pids+=("$(tp status --json | jq -r .pid)")
tp project new qp >/dev/null
tp area add qp/qa --work 'bd:none-*' >/dev/null
curl -s --unix-socket "$TH/zm.sock" -H 'Potluck-Protocol: potluck/1' -H 'Content-Type: application/json' \
	-X PATCH http://zm/v0/projects/qp -d '{"trackers":[{"adapter":"fake","prefix":"bd","env":"inherit"}]}' >/dev/null # the fake reads ZM_FAKE_STATE
mirrored() { tp work ls --project qp --source bd --json 2>/dev/null | jq -e 'length == 1' >/dev/null; }
check "the tracker item is mirrored" until_true 20 mirrored
tp work claim qp/bd:acme-q1 >/dev/null
tp area set qp/qa --label qa >/dev/null
check "who after area set: still held by its holder" has "$(tp who qp/bd:acme-q1)" "held by human:ana"
check "the lease moved to the item's new area" test "$(tp leases --json | jq -r '[.[] | select(.key == "bd:acme-q1") | .area] | join(",")')" = qa
check "renew after area set" bash -c "ZM_HOME=$TH ZM_FAKE_STATE=$TH/fake.json zm work renew qp/bd:acme-q1 >/dev/null"
check "release after area set" has "$(tp work release qp/bd:acme-q1 2>&1)" "released"
check "no lease left behind" test "$(tp leases --json | jq 'length')" = 0
tp daemon stop >/dev/null

say "admission control"
mkdir -p "$T/adm"
echo '{"admission": {"min_disk_free_mb": 999999999999}}' >"$T/adm/config.json"
ZM_HOME=$T/adm zm daemon --detach >/dev/null
daemon_pids+=("$(ZM_HOME=$T/adm zm status --json | jq -r .pid)")
out=$(ZM_HOME=$T/adm zm run -d shell 2>&1) && rc=0 || rc=$?
check "spawn refused with a typed reason" test "$rc" != 0
check "the reason names disk" has "$out" "disk"
check "nothing was started" test "$(ZM_HOME=$T/adm zm ls -a --json | jq '.sessions | length')" = 0
ZM_HOME=$T/adm zm daemon stop >/dev/null

say "approvals: no socket caller decides; receipts verify offline"
out=$(zm approver add ana --no-qr)
check "approver add prints a one-time pairing code" has "$out" "Pairing code: [A-Z2-9]\{4\}-[A-Z2-9]\{4\}"
check "approver add names the tailnet pairing page" has "$out" "/v0/approvals?code="
check "approver ls: ana pending" has "$(zm approver ls)" "^ana  *pending"
check "approver add refuses a non-canonical name" bash -c "! zm approver add Ana >/dev/null 2>&1"
out=$(zm approver add vlda 2>&1) && rc=0 || rc=$?
check "approver add refuses a lookalike of identity vida" test "$rc:$(grep -c 'looks like identity' <<<"$out")" = "1:1"
head=0123456789abcdef0123456789abcdef01234567
ask=$(zm ask new --project acme --area solver --repo acme/app --pr 7 --head $head "merge the solver fix?" --json | jq -r .id)
out=$(zm ask new --project acme --area solver --to human:vida --repo acme/app --pr 7 --head $head "x" 2>&1) && rc=0 || rc=$?
check "ask to another human is refused (the decision could never answer it)" test "$rc:$(grep -c "where you asked" <<<"$out")" = "1:1"
check "ask: a needs=human mail to owners with the same id" test "$(zm mail --needs human --json | jq -r ".mail[] | select(.id == \"$ask\") | .to")" = owners
check "ask show: not decided" has "$(zm ask show "$ask")" "not decided yet"
check "ask receipt refuses an undecided ask" bash -c "! zm ask receipt $ask >/dev/null 2>&1"
sock() { curl -s --unix-socket "$ZM_HOME/zm.sock" -H 'Potluck-Protocol: potluck/1' -H 'Content-Type: application/json' "$@"; }
out=$(sock -X POST "http://zm/v0/asks/$ask/decide" -d '{"decision":"approve"}')
check "decide from the socket (the machine owner) is refused" has "$out" "no socket caller holds the decide right"
out=$(sock -X POST "http://zm/v0/approvals/pair" -d '{"code":"AAAA-AAAA"}')
check "pairing from the socket is refused" has "$out" '"forbidden"'
out=$(ZM_AS=smoke/deadbeef sock -H 'Potluck-As: smoke/deadbeef' -X POST "http://zm/v0/asks/$ask/decide" -d '{"decision":"approve"}')
check "decide from an agent session on the socket is refused" has "$out" '"forbidden"'
out=$(curl -s --unix-socket "$ZM_HOME/zm.sock" -H 'Zm-Protocol: zm/v0' -H 'Zm-As: smoke/deadbeef' -H 'Content-Type: application/json' -X POST "http://zm/v0/asks/$ask/decide" -d '{"decision":"approve"}')
check "decide from an agent session on the socket is refused (the old header names)" has "$out" '"forbidden"'
out=$(curl -s --unix-socket "$ZM_HOME/zm.sock" -H 'Potluck-Protocol: potluck/1' -H "Authorization: Bearer $O" -X POST "http://zm/v0/asks/$ask/decide" -d '{}')
check "decide with an owner token on the socket is refused" has "$out" '"forbidden"'
key=$(zm receipt key)
check "receipt key is published" has "$key" "^ed25519:"
check "the receipt key file is private" test "$(stat -c %a "$ZM_HOME/receipt.key" 2>/dev/null || stat -f %Lp "$ZM_HOME/receipt.key")" = 600
ZM_SMOKE_SIGN=$ZM_HOME/receipt.key ZM_SMOKE_OUT=$T/receipt.json ZM_SMOKE_REPO=acme/app ZM_SMOKE_HEAD=$head \
	ZM_SMOKE_PROJECT=$acme ZM_SMOKE_ASK=$ask "$T/fakebin/approvals.test" -test.run TestSmokeSignReceipt >/dev/null
v() { zm receipt verify "$T/receipt.json" "$@" >/dev/null 2>&1; }
check "receipt verifies offline with the published key (no daemon needed)" v --key "$key" --repo acme/app --pr 7 --head $head --decision approve --approver ana --ask "$ask" --for-project "$acme"
check "receipt verifies on the daemon" v --repo github.com/acme/app --pr 7 --head $head --for-project acme
check "replay to another PR fails" bash -c "! zm receipt verify '$T/receipt.json' --key '$key' --repo acme/app --pr 8 --head $head >/dev/null 2>&1"
check "replay to another head fails" bash -c "! zm receipt verify '$T/receipt.json' --key '$key' --repo acme/app --pr 7 --head ${head/0/f} >/dev/null 2>&1"
check "replay to another project fails" bash -c "! zm receipt verify '$T/receipt.json' --repo acme/app --pr 7 --head $head --for-project side >/dev/null 2>&1"
check "a lookalike approver name fails" bash -c "! zm receipt verify '$T/receipt.json' --key '$key' --repo acme/app --pr 7 --head $head --approver Ana >/dev/null 2>&1"
sed 's/"pr":7/"pr":8/' "$T/receipt.json" >"$T/tampered.json"
check "a tampered receipt fails" bash -c "! zm receipt verify '$T/tampered.json' --key '$key' --repo acme/app --pr 8 --head $head >/dev/null 2>&1"
ZM_SMOKE_SIGN=$ZM_HOME/receipt.key ZM_SMOKE_OUT=$T/decline.json ZM_SMOKE_REPO=acme/app ZM_SMOKE_HEAD=$head \
	ZM_SMOKE_PROJECT=$acme ZM_SMOKE_ASK=$ask ZM_SMOKE_DECISION=decline "$T/fakebin/approvals.test" -test.run TestSmokeSignReceipt >/dev/null
check "a decline receipt fails the default gate (repo, PR, head only)" bash -c "! zm receipt verify '$T/decline.json' --key '$key' --repo acme/app --pr 7 --head $head >/dev/null 2>&1"
check "a decline receipt fails the default gate on the daemon too" bash -c "! zm receipt verify '$T/decline.json' --repo acme/app --pr 7 --head $head >/dev/null 2>&1"
check "a decline receipt verifies only when --decision decline is asked for" bash -c "zm receipt verify '$T/decline.json' --key '$key' --repo acme/app --pr 7 --head $head --decision decline >/dev/null 2>&1"
check "verify without repo, PR and head refuses (fail closed)" bash -c "! zm receipt verify '$T/receipt.json' --key '$key' >/dev/null 2>&1"
out=$(guest zm ask show "$ask" 2>&1 || true)
check "the guest cannot read the owners' ask in its own area" has "$out" "no ask"
zm approver rm ana >/dev/null
check "approver rm takes effect" hasnt "$(zm approver ls)" "^ana "
zm approver add mallory --no-qr >/dev/null
out=$(zm identity add rnallory 2>&1) && rc=0 || rc=$?
check "identity add refuses a lookalike of approver mallory" test "$rc:$(grep -c 'looks like approver' <<<"$out")" = "1:1"
zm approver rm mallory >/dev/null
check "bootstrap marker absent until a device pairs (none can without a tailnet)" test ! -e "$ZM_HOME/approvals/bootstrapped"

say "teams (SPEC 17.3, 17.4; cross-machine parts are tested in internal/team over a loopback tailnet)"
zm project new crew >/dev/null
zm lease lock:crew-deploy --project crew >/dev/null
out=$(zm project team crew)
check "project team: this machine is the authority, the lease moved" has "$out" "crew is a team project.*1 leases moved"
check "the lease is served from crew's project.db" test "$(zm leases --project crew --json | jq -r '.[0].key')" = "lock:crew-deploy"
out=$(ZM_AS=smoke/abcd1234 zm lease lock:crew-deploy --project crew 2>&1) && rc=0 || rc=$?
check "a team claim is still a compare-and-set (conflict)" test "$rc:$(grep -c 'held by' <<<"$out")" = "1:1"
out=$(zm invite crew --as bea 2>&1) && rc=0 || rc=$?
check "invite needs this machine's tailnet node (the code names it)" test "$rc:$(grep -c 'tailnet up' <<<"$out")" = "3:1"
out=$(sock -X POST "http://zm/v0/invites/redeem" -d '{"code":"AAAA-AAAA","machine":"x"}')
check "redeem from the socket is refused" has "$out" '"forbidden"'
start=$SECONDS
out=$(zm join nonsense 2>&1) && rc=0 || rc=$?
check "join by a name no authority answers fails fast ($((SECONDS - start)) s)" test "$rc:$(grep -c 'is Tailscale up' <<<"$out"):$((SECONDS - start < 40))" = "1:1:1"
for bad in potluck-mini:abc zm-nonsense zm-mini:abc ABCD-EFGH NotASlug; do # code-shaped: a malformed code, never a GitHub join
  out=$(zm join "$bad" 2>&1) && rc=0 || rc=$?
  check "join refuses a malformed code ($bad)" test "$rc:$(grep -c 'potluck-<machine>:<fingerprint>:XXXX-XXXX' <<<"$out")" = "1:1"
done
check "remotes: nothing joined" test "$(zm remotes --json | jq length)" = 0
out=$(sock -X POST "http://zm/v0/team/presence" -d '{"project":"crew","sessions":[]}')
check "presence from the socket is refused (a replica's node-bound token only)" has "$out" "node-bound token"
out=$(ZM_AS=smoke/abcd1234 zm send machine:nobody hi --project crew 2>&1) && rc=0 || rc=$?
check "team mail to a machine that never joined is not found" test "$rc:$(grep -c 'no machine' <<<"$out")" = "1:1"
check "project solo moves the lease back" has "$(zm project solo crew)" "crew is solo again (1 leases moved)"
check "the lease is served from registry.db again" test "$(zm leases --project crew --json | jq -r '.[0].key')" = "lock:crew-deploy"

say "kill escalates for sessions that ignore TERM"
printf '#!/bin/sh\ntrap "" TERM\necho ready\nwhile :; do sleep 1; done\n' >"$T/noterm"
printf '#!/bin/sh\ntrap "" TERM HUP\necho ready\nwhile :; do sleep 1; done\n' >"$T/nohup"
chmod +x "$T/noterm" "$T/nohup"
K=$(cd "$T/acme" && zm run -d --name noterm --agent shell "$T/noterm")
until_true 5 bash -c "zm session output '$K' | grep -q ready" || true
out=$(zm kill "$K" 2>&1) && rc=0 || rc=$?
check "kill: a shell that ignores TERM ends on HUP (exit 0)" test "$rc:$(grep -c 'ended after HUP' <<<"$out")" = "0:1"
K=$(cd "$T/acme" && zm run -d --name nohup --agent shell "$T/nohup")
until_true 5 bash -c "zm session output '$K' | grep -q ready" || true
out=$(zm kill "$K" 2>&1) && rc=0 || rc=$?
check "kill: still running after TERM and HUP exits 1 and names --force" test "$rc:$(grep -c 'still .* after TERM, HUP.*--force' <<<"$out")" = "1:1"
out=$(zm kill --force "$K" 2>&1) && rc=0 || rc=$?
check "kill --force ends it with KILL" test "$rc:$(zm session show "$K" --json | jq -r .state)" = "0:ended"

say "zm attach --embed: exit codes a host can act on"
out=$(zm attach --embed "$K" </dev/null 2>&1) && rc=0 || rc=$?
check "embed attach to an ended session: exit 0, says nothing" test "$rc:$out" = "0:"
zm attach --embed deadbeef </dev/null >/dev/null 2>&1 && rc=0 || rc=$?
check "embed attach to no such session: exit 2" test "$rc" = 2
E=$(cd "$T/acme" && zm run -d --name embed --agent shell /bin/sh -c 'echo embed-out; sleep 1')
out=$(zm attach --embed "$E" </dev/null 2>"$T/embed.err") && rc=0 || rc=$?
check "embed attach until the session ends: its output, exit 0, no stderr" test "$rc:$(grep -c embed-out <<<"$out"):$(wc -c <"$T/embed.err" | tr -d ' ')" = "0:1:0"

say "sessions survive a daemon restart (SPEC 8, session holders)"
R=$(cd "$T/acme" && zm run -d --name survives-restart --agent shell /bin/sh -c 'echo survivor-ready; exec cat')
until_true 25 bash -c "zm session output '$R' | grep -q survivor-ready" || true
rpid=$(zm session show "$R" --json | jq -r .pid)
check "daemon stop says sessions keep running" has "$(zm daemon stop)" "keep running"
check "the session outlived its daemon" kill -0 "$rpid"
zm daemon --detach >/dev/null
daemon_pids+=("$(zm status --json | jq -r .pid)")
check "the next daemon adopted it" test "$(zm session show "$R" --json | jq -r '.state + ":" + (.unreachable // "")')" = "running:"
zm session input "$R" after-restart >/dev/null
check "input reaches it after the restart" until_true 25 bash -c "zm session output '$R' | grep -q after-restart"
check "its scrollback from before the restart is kept" has "$(zm session output "$R")" "survivor-ready"
zm kill "$R" >/dev/null
check "it ends through its holder" test "$(zm session show "$R" --json | jq -r .state)" = ended

say "bd routes an id to its project by prefix (SPEC Native issues)"
zm project new rta >/dev/null
zm project new rtb >/dev/null
ra=$(bd-zm --project rta create "in rta" --silent)
rb=$(bd-zm --project rtb create "in rtb" --silent)
out=$(ZM_PROJECT=rta bd-zm show "$rb" 2>&1) && rc=0 || rc=$?
check "under ZM_PROJECT=rta, bd show finds an rtb id" test "$rc:$(grep -c "in rtb" <<<"$out")" = "0:1"
out=$(ZM_PROJECT=rta bd-zm dep add "$ra" "$rb" 2>&1) && rc=0 || rc=$?
check "a dependency across projects is refused with the alternative" test "$rc:$(grep -c 'external:' <<<"$out")" = "1:1"
check "the prefix map is cached 0600" test "$(stat -c %a "$ZM_HOME/cache/bd-prefixes.json" 2>/dev/null || stat -f %Lp "$ZM_HOME/cache/bd-prefixes.json")" = 600
if [ -n "${EPOCHREALTIME:-}" ]; then # bash 5: the cost of routing, process start included (p50 of 20)
	p50() { # p50 label setup-command bd-args...
		local label=$1 setup=$2 i t0 ts=()
		shift 2
		for ((i = 0; i < 20; i++)); do
			eval "$setup"
			t0=$EPOCHREALTIME
			ZM_PROJECT=rta bd-zm "$@" >/dev/null 2>&1
			ts+=("$(awk -v a="$t0" -v b="$EPOCHREALTIME" 'BEGIN{printf "%.2f", (b-a)*1000}')")
		done
		printf '  cost %-34s p50 %s ms\n' "$label" "$(printf '%s\n' "${ts[@]}" | sort -n | sed -n 10p)"
	}
	p50 "show, --project (no routing)" ":" --project rtb show "$rb"
	p50 "show, routed, warm prefix map" ":" show "$rb"
	p50 "show, routed, cold prefix map" "rm -f '$ZM_HOME/cache/bd-prefixes.json'" show "$rb"
fi

say "stop everything"
spid=$(zm session show "$S" --json | jq -r .pid)
zm kill "$S" >/dev/null
check "the fake agent ended as killed" test "$(zm session show "$S" --json | jq -r '.state + " " + .outcome.kind')" = "ended killed"
check "gc lists the worktree, deletes nothing" has "$(zm session gc)" "$s8"
check "worktree still there" test -d "$wt"
zm events >"$T/events.out" 2>"$T/events.err" &
epid=$!
sleep 0.5 # let the stream connect
zm daemon stop >/dev/null
wait "$epid" && erc=0 || erc=$?
check "events cut off by daemon stop says so and exits 3" test "$erc:$(grep -c 'stopped or restarted during this request' "$T/events.err")" = "3:1"
gone() {
	for p in "${daemon_pids[@]}" "$spid"; do ! kill -0 "$p" 2>/dev/null || return 1; done
	! pgrep -f "$T/" >/dev/null
}
until_true 3 gone || true
left=$(pgrep -fl "$T/" || true)
check "no processes left${left:+: $left}" gone

printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" = 0 ]
