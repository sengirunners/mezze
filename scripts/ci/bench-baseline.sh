#!/usr/bin/env bash
# Finds what this bench run is judged against. The ceilings came from Sengi, so only a Sengi run gives a
# verdict (scripts/bench repeat -runner): the baseline is the newest finished main run (a push or the nightly, which
# is pinned to Sengi) older than this one, red or green, whose bench job ran on a Sengi label, among the
# last WINDOW; its bench.json goes to main-bench/, its created_at to main-bench/at (repeat's pair bound). Runs on
# main can overlap, hence the run number. When the window has none, it warns (count and age: nothing has judged main
# since), and main's last finished run on any runner goes to main-other/ for `bench compare` only, labelled as
# other-runner data in main-other/note: it is never the first of two. A failed read or download is "not found".
# Blindness: on the nightly (EVENT=schedule), no Sengi-timed main bench in the STALE_DAYS days before this
# run writes bench-blind (the message), and the nightly's last step goes red on it. Pushes only warn. A nightly timed
# on Sengi (BENCH_LABEL sengi-*) is itself today's verdict, so it is never blind.
# A contended Sengi run with rows over is no verdict: its bench.json's worse runner probe p95 (runner/probe or
# runner/probe-late) over CONTENDED_P95 and a row over its ceiling, which bench repeat can never judge (this run, or the
# next one paired with it), so it does not count against blindness, this nightly included. A contended run with no row
# over did judge (all under, even on a slow VM). A Sengi run whose artifact cannot be read counts, as before, or stops
# the judging (never a false blind red). A quiet run counts even when its own pair was withheld for the run before it.
# Runner classes (bench-class.sh): a run on bench.yml's ZM_BENCH_RUNNER (a zm pool box, class box-<size>, or
# a dedicated runner, zm-bench) is judged against its own class's runs only, never Sengi's (its speed is not Sengi's):
# the baseline is the newest same-class run, and "Sengi" above reads "this run's class". Any other label (Factory)
# looks for Sengi, as before. While the class has no ceilings file it calibrates: the count of its runs in the window
# goes to bench-class-runs (bench repeat -class-runs). From CALIBRATE_MIN runs every run warns that calibration is
# ready; once that has lasted STALE_DAYS, a nightly goes blind. Blindness counts a run on any class (a nightly there
# is its own day's run, like a Sengi one), so switching classes is never a blind day.
set -uo pipefail
: "${GH_REPO:?}" "${RUN_NUMBER:?}"
WINDOW="${WINDOW:-30}" JOB="${JOB:-make bench (latency budgets)}" SUMMARY="${GITHUB_STEP_SUMMARY:-/dev/null}"
STALE_DAYS="${STALE_DAYS:-3}" CALIBRATE_MIN="${CALIBRATE_MIN:-10}"
# contendedP95 in scripts/bench/repeat.go (a test there holds the two equal).
CONTENDED_P95="${CONTENDED_P95:-1.3}"
rm -rf main-bench main-other bench-blind bench-class-runs
# shellcheck source=scripts/ci/bench-class.sh
. "$(dirname "$0")/bench-class.sh" # class_of
CLASS=$(class_of "${BENCH_LABEL:-}"); CLASS=${CLASS:-sengi}
NAME=Sengi; [ "$CLASS" = sengi ] || NAME=$CLASS
calibrating=; [ "$CLASS" = sengi ] || [ -f "scripts/ceilings/ceilings.$CLASS.json" ] || calibrating=1
# finished <limit> <query>: "<id> <created_at> <event>" of finished main runs older than this one, newest first.
finished() {
  gh api "repos/$GH_REPO/actions/workflows/bench.yml/runs?branch=main&status=completed&per_page=100$2" |
    jq -r --argjson n "$RUN_NUMBER" --argjson w "$1" 'limit($w; .workflow_runs[] | select(.run_number < $n
      and (.event == "push" or .event == "schedule") and (.conclusion == "success" or .conclusion == "failure")))
      | "\(.id) \(.created_at) \(.event)"' 2>/dev/null
}
# on <id>: the bench job's "<labels> <runner>", or empty.
on() {
  gh api "repos/$GH_REPO/actions/runs/$1/jobs" </dev/null | jq -r --arg job "$JOB" \
    'first(.jobs[] | select(.name == $job) | "\(.labels | join(",")) \(.runner_name // "?")") // empty' 2>/dev/null
}
# has_bench <id>: the run left a bench artifact (bench.json). A calibrating class counts only those: a run that broke
# before timing anything (the box's first runs, 2026-11-10) is no calibration data, as bench-calibrate.sh agrees. An
# unreadable answer counts as none, which only delays the calibration notices.
has_bench() {
  gh api "repos/$GH_REPO/actions/runs/$1/artifacts" </dev/null |
    jq -e 'any(.artifacts[]; .name == "bench" and (.expired | not))' >/dev/null 2>&1
}
readfail=
runs=$(finished "$WINDOW" "") || { readfail=1 runs=; }
first= firstOn= checked=0 oldest= found= foundAt= classRuns=0
while read -r id at _; do
  [ -n "$id" ] || continue
  checked=$((checked + 1)) oldest=$at
  where=$(on "$id") || { where= readfail=1; } # unreadable: the blindness check won't judge
  [ -n "$first" ] || { first=$id firstOn=${where:-unknown runner}; }
  [ "$(class_of "$where")" = "$CLASS" ] || continue
  [ -z "$calibrating" ] || has_bench "$id" || continue
  classRuns=$((classRuns + 1))
  [ -z "$found" ] || continue # still counting a calibrating class's runs
  if gh run download "$id" -R "$GH_REPO" -n bench -D main-bench </dev/null >/dev/null 2>&1 && [ -f main-bench/bench.json ]; then
    echo "$at" > main-bench/at
    echo "main's last bench on $NAME: run $id ($where, $at)"; found=$id foundAt=$at
    [ -n "$calibrating" ] || break
  else
    rm -rf main-bench
  fi
done <<<"$runs"
# contended <bench.json>: "<probe row> p95 <ms> ms", its worse runner probe, when that is over CONTENDED_P95 and a row is
# over its ceiling; else nothing (a quiet run, no row over, a run from before the probe, or not a bench.json).
contended() {
  jq -r --argjson l "$CONTENDED_P95" '[.[] | objects] as $rows | def probe: .name == "runner/probe" or .name == "runner/probe-late";
    [$rows[] | select(probe and (.p95_ms | type == "number"))] | max_by(.p95_ms) // empty | select(.p95_ms > $l)
    | select(any($rows[]; (probe | not) and .ok == false)) | "\(.name) p95 \(.p95_ms) ms"' "$1" 2>/dev/null
}
# no_bench <id>: the run's artifact list reads, and has no bench (a failed read is not "none").
no_bench() {
  local l; l=$(gh api "repos/$GH_REPO/actions/runs/$1/artifacts" </dev/null 2>/dev/null) &&
    jq -e '.artifacts | type == "array" and (any(.[]; .name == "bench" and (.expired | not)) | not)' <<<"$l" >/dev/null 2>&1
}
# recent <created_at>: within STALE_DAYS of now.
recent() { [ -n "$1" ] && jq -en --arg t "$1" --argjson d "$STALE_DAYS" '(now - ($t | fromdateiso8601)) <= $d * 86400' >/dev/null 2>&1; }
if [ -n "$calibrating" ]; then
  echo "$classRuns" > bench-class-runs
  echo "$CLASS is calibrating (no scripts/ceilings/ceilings.$CLASS.json): $classRuns earlier runs in the window"
  # Calibration ready (CALIBRATE_MIN runs, this one included) but no file committed: say what to do on every run.
  # A nightly goes blind once it was already ready STALE_DAYS ago (CALIBRATE_MIN of the class's runs finished before
  # then), since the class's runs still judge nothing. That looks past the window, which pushes fill in hours: at
  # most 100 runs from before the cutoff, newest first; a failed read is not stale.
  if [ $((classRuns + 1)) -ge "$CALIBRATE_MIN" ]; then
    msg="$CLASS has $((classRuns + 1)) runs (this one included) and no ceilings file, so its runs judge nothing: run CLASS=$CLASS scripts/ci/bench-calibrate.sh, review scripts/ceilings/ceilings.$CLASS.json and commit it (runbook: docs/runbooks/zm-bench-vm.md in the private ops repo)"
    echo "::warning title=bench: calibration ready::$msg"
    printf '\n**Calibration ready:** %s.\n' "$msg" >> "$SUMMARY"
    stale=
    if [ "${EVENT:-}" = schedule ] &&
      cutoff=$(jq -nr --argjson d "$STALE_DAYS" 'now - $d * 86400 | floor | todate' 2>/dev/null) &&
      before=$(finished 100 "&created=%3C$cutoff"); then
      n=0
      while read -r id _; do
        [ -n "$id" ] || continue
        where=$(on "$id") || break
        [ "$(class_of "$where")" = "$CLASS" ] && has_bench "$id" || continue
        n=$((n + 1)); [ "$n" -lt "$CALIBRATE_MIN" ] || { stale=1; break; }
      done <<<"$before"
    fi
    if [ -n "$stale" ]; then
      msg="calibration of $CLASS was ready $STALE_DAYS days ago ($CALIBRATE_MIN of its runs finished before $cutoff) and is still not committed: main's timing has gone unjudged. $msg"
      echo "::error title=bench: blind (calibration not committed)::$msg"
      printf '\n**Bench is blind:** %s\n' "$msg" >> "$SUMMARY"
      echo "$msg" > bench-blind
      exit 0
    fi
  fi
fi
if [ -z "$found" ]; then
  age=$(jq -nr --arg t "${oldest:-}" 'if $t == "" then "" else ((now - ($t | fromdateiso8601)) / 3600 | floor) as $h
    | if $h < 48 then "\($h) h" else "\($h / 24 | floor) days" end end' 2>/dev/null) || age=
  msg="no real verdict in the window: none of main's last $checked finished bench runs ran on $NAME${age:+, so the last verdict is older than $age}"
  echo "::warning title=bench: no $NAME verdict::$msg"
  printf '\n**Bench verdicts:** %s. Timing on other runners is listed but judges nothing.\n' "$msg" >> "$SUMMARY"
  if [ -n "$first" ] && gh run download "$first" -R "$GH_REPO" -n bench -D main-other >/dev/null 2>&1 && [ -f main-other/bench.json ]; then
    echo "run $first on $firstOn" > main-other/note
    echo "main's last finished bench (other-runner data, compare only): run $first ($firstOn)"
  else
    rm -rf main-other
    echo "main's last finished bench: none"
  fi
fi
[ "${EVENT:-}" = schedule ] || exit 0
if [ -n "$(class_of "${BENCH_LABEL:-}")" ]; then # on a verdict class: this nightly is today's run, unless contended
  hot=; [ "$CLASS" != sengi ] || hot=$(contended bench.json)
  if [ -z "$hot" ]; then
    what=verdict; [ -z "$calibrating" ] || what="calibration run"
    echo "this nightly ran on $NAME ($BENCH_LABEL): main has today's $what"; exit 0
  fi
  echo "this nightly ran on $NAME ($BENCH_LABEL) but was contended ($hot, over $CONTENDED_P95, with rows over): no verdict, so not today's"
fi
hot=; [ -z "$found" ] || [ "$CLASS" != sengi ] || hot=$(contended main-bench/bench.json)
recent "$foundAt" && [ -z "$hot" ] && exit 0
# Pushes can crowd the window: look at the last few nightlies too (no artifact needed: it judged when it ran), and at
# the window's other Sengi runs when the newest was contended. Newest first, each once.
nightlies=$(finished "$((STALE_DAYS + 2))" "&event=schedule") || readfail=1
if [ -n "$readfail" ]; then # a failed read is not "no runs": never a false blind red
  echo "::warning title=bench: history unreadable::could not read main's bench history, so blindness is not judged this run"
  exit 0
fi
busy=0 seen=$(mktemp -d) || exit 0
trap 'rm -rf "$seen"' EXIT
while read -r id at ev; do
  [ -n "$id" ] && recent "$at" || break
  if ! where=$(on "$id"); then # unreadable: not proof it ran elsewhere
    echo "::warning title=bench: history unreadable::could not read run $id's jobs, so blindness is not judged this run"
    exit 0
  fi
  c=$(class_of "$where")
  case "$c" in
  sengi)
    if [ "$id" = "$found" ]; then p=$hot
    elif gh run download "$id" -R "$GH_REPO" -n bench -D "$seen/$id" </dev/null >/dev/null 2>&1 && [ -f "$seen/$id/bench.json" ]; then
      p=$(contended "$seen/$id/bench.json")
    elif [ "$ev" = schedule ]; then p= # a nightly judged when it ran
    elif no_bench "$id"; then continue # a push with no bench judged nothing
    else
      echo "::warning title=bench: history unreadable::could not read run $id's bench, so blindness is not judged this run"
      exit 0
    fi
    if [ -n "$p" ]; then
      echo "run $id on Sengi ($at) was contended ($p, over $CONTENDED_P95, with rows over): no verdict"
      busy=$((busy + 1)); continue
    fi
    what="Sengi nightly"; [ "$ev" = schedule ] || what="Sengi run"
    echo "main's last $what: run $id ($at)"; exit 0 ;;
  ?*) [ "$ev" = schedule ] || continue; echo "main's last $c nightly: run $id ($at)"; exit 0 ;;
  esac
done < <(printf '%s\n%s\n' "$runs" "$nightlies" | sort -r -k2,2 | awk 'NF && !seen[$1]++')
msg="no $NAME verdict on main for $STALE_DAYS days before this nightly: no finished push or nightly bench on main ran on a verdict class's runner (sengi-*, zm-bench or a zm pool box) since"
[ "$busy" = 0 ] || msg="$msg, other than $busy on a contended Sengi VM (a runner probe p95 over $CONTENDED_P95 ms, with rows over: no verdict)"
msg="$msg. Ceilings come from those runners, so main's timing has gone unjudged. Check that the nightly ran where bench.yml pins it (Sengi, or ZM_BENCH_RUNNER when it is set) and was not cancelled, and that runner is up."
echo "::error title=bench: blind for $STALE_DAYS days::$msg"
printf '\n**Bench is blind:** %s\n' "$msg" >> "$SUMMARY"
echo "$msg" > bench-blind
