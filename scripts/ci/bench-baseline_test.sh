#!/usr/bin/env bash
# bench-baseline.sh against a fake gh: which main run a bench is judged against, and the warning when none ran on
# Sengi. Run by ci.yml's check job.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
mkdir "$T/bin"
# $F/runs: lines <id> <run_number> <conclusion> [event (push)] [hours ago (none: 2001-01-01, far in the past)],
# newest first; a query with event= gets only that event's runs, one with created=%3C<t> only those created before t.
# $F/on/<id>: the bench job's "<label> <runner>". $F/art/<id>: the run has a bench artifact (download and listing),
# whose bench.json is $F/bench/<id> when that exists (a recorded artifact), else ["<id>"]; dlfail/<id>: listed, but
# its download fails. A FAIL file fails every
# runs read; JOBFAIL every jobs read; jobfail/<id> that run's.
cat > "$T/bin/gh" <<'FAKE'
#!/usr/bin/env bash
case "$1 $2" in
"api "*/jobs)
  [ ! -f "$F/JOBFAIL" ] || exit 1
  id=${2%/jobs}; id=${id##*/}
  [ ! -f "$F/jobfail/$id" ] || exit 1
  if [ -f "$F/on/$id" ]; then read -r l r < "$F/on/$id"
    jq -n --arg l "$l" --arg r "$r" '{jobs: [{name: "route"}, {name: "make bench (latency budgets)", labels: ($l | split(",")), runner_name: $r}]}'
  else echo '{"jobs":[]}'; fi ;;
"api "*/artifacts)
  id=${2%/artifacts}; id=${id##*/}
  if [ -f "$F/art/$id" ]; then echo '{"artifacts":[{"name":"bench","expired":false}]}'; else echo '{"artifacts":[]}'; fi ;;
"api "*)
  [ ! -f "$F/FAIL" ] || exit 1
  want=; case "$2" in *event=*) want=${2##*event=}; want=${want%%&*} ;; esac
  before=; case "$2" in *created=%3C*) before=${2##*created=%3C}; before=${before%%&*}; echo "$before" > "$F/cutoff" ;; esac
  while read -r id n c e h; do
    [ -n "$id" ] || continue; e=${e:-push}
    [ -z "$want" ] || [ "$e" = "$want" ] || continue
    jq -n --argjson id "$id" --argjson n "$n" --arg c "$c" --arg e "$e" --arg h "${h:-}" '{id: $id, run_number: $n,
      conclusion: $c, event: $e, created_at: (if $h == "" then "2001-01-01T00:00:00Z" else now - ($h | tonumber) * 3600 | floor | todate end)}'
  done < "$F/runs" | jq -s --arg b "$before" '{workflow_runs: map(select($b == "" or .created_at < $b))}' ;;
"run download")
  [ -f "$F/art/$3" ] && [ ! -f "$F/dlfail/$3" ] || exit 1
  mkdir -p "$9"; if [ -f "$F/bench/$3" ]; then cp "$F/bench/$3" "$9/bench.json"; else echo "[\"$3\"]" > "$9/bench.json"; fi ;;
esac
FAKE
chmod +x "$T/bin/gh"
fails=0

# scenario <name> <expected output substring> <dir that must hold that run's bench.json, or ->
#   (EVENT and BENCH_LABEL in the environment: the event and runs-on label of the run under test)
#   <runs, ; separated> <id=label runner, ; separated> <ids with artifacts>
scenario() {
  local name=$1 want=$2 dir=$3 out status
  export F="$T/$name"; mkdir -p "$F/on" "$F/art" "$F/w"
  tr ';' '\n' <<<"$4" > "$F/runs"
  tr ';' '\n' <<<"$5" | while IFS='=' read -r id on; do [ -z "$id" ] || echo "$on" > "$F/on/$id"; done
  for id in $6; do touch "$F/art/$id"; done
  out=$(cd "$F/w" && PATH="$T/bin:$PATH" GH_REPO=o/r RUN_NUMBER=50 GITHUB_STEP_SUMMARY="$F/summary" EVENT="${EVENT:-push}" \
    BENCH_LABEL="${BENCH_LABEL:-acme-runner-4}" \
    timeout 20 "$here/bench-baseline.sh" 2>&1); status=$?
  printf '%s\n' "$out" > "$F/out"
  if [ "$status" != 0 ] || [[ "$out" != *"$want"* ]]; then
    echo "FAIL $name: status $status, want \"$want\" in:"; echo "$out"; fails=$((fails + 1)); return
  fi
  if [ "$dir" != - ] && ! grep -q "\"${dir#*:}\"" "$F/w/${dir%%:*}/bench.json" 2>/dev/null; then
    echo "FAIL $name: no ${dir%%:*}/bench.json of run ${dir#*:}"; fails=$((fails + 1)); return
  fi
  echo "ok $name"
}

S="sengi-standard-4-ubuntu-2404 sengi-4-07" X="acme-runner-4 tfp-abc"
scenario newest-sengi "on Sengi: run 3 (sengi-standard-4-ubuntu-2404" main-bench:3 \
  "1 49 success;2 48 cancelled;3 47 failure;4 46 success" "1=$X;3=$S;4=$S" "1 3 4"
scenario newer-and-unfinished-skipped "on Sengi: run 4" main-bench:4 \
  "1 51 success;2 50 success;3 49 cancelled;4 48 success" "1=$S;2=$S;3=$S;4=$S" "1 2 3 4"
scenario sengi-without-artifact-skipped "on Sengi: run 2" main-bench:2 "1 49 success;2 48 success" "1=$S;2=$S" "2"
scenario none-on-sengi-warns "::warning title=bench: no Sengi verdict::no real verdict in the window: none of main's last 2 finished" \
  main-other:1 "1 49 success;2 48 failure" "1=$X;2=$X" "1 2"
grep -q "older than [0-9]* days" "$T/none-on-sengi-warns/summary" || { echo "FAIL none-on-sengi-warns: no summary line"; fails=$((fails + 1)); }
grep -qx "run 1 on acme-runner-4 tfp-abc" "$T/none-on-sengi-warns/w/main-other/note" || { echo "FAIL none-on-sengi-warns: no note"; fails=$((fails + 1)); }
[ ! -e "$T/none-on-sengi-warns/w/main-bench" ] || { echo "FAIL none-on-sengi-warns: a main-bench"; fails=$((fails + 1)); }
WINDOW=1 scenario window-bounds "none of main's last 1 finished" main-other:1 "1 49 success;2 48 success" "1=$X;2=$S" "1 2"
mkdir -p "$T/failed-read-is-none"; touch "$T/failed-read-is-none/FAIL"
scenario failed-read-is-none "none of main's last 0 finished" - "1 49 success" "1=$S" "1"
scenario nothing-yet "none of main's last 0 finished" - "" "" ""
grep -qx "2001-01-01T00:00:00Z" "$T/newest-sengi/w/main-bench/at" || { echo "FAIL newest-sengi: no main-bench/at"; fails=$((fails + 1)); }

# Nightlies count, and only a nightly goes blind.
blind() { [ -f "$T/$1/w/bench-blind" ]; }
scenario nightly-counts "on Sengi: run 2" main-bench:2 "1 49 success push 1;2 48 success schedule 20" "1=$X;2=$S" "1 2"
scenario dispatch-not-counted "on Sengi: run 2" main-bench:2 "1 49 success workflow_dispatch 1;2 48 success push 30" "1=$S;2=$S" "1 2"
scenario push-never-blind "none of main's last 1 finished" - "1 49 success push 1" "1=$X" ""
blind push-never-blind && { echo "FAIL push-never-blind: blind on a push"; fails=$((fails + 1)); }
EVENT=schedule scenario nightly-recent-not-blind "on Sengi: run 2" main-bench:2 "1 49 success;2 48 success schedule 30" "1=$X;2=$S" "1 2"
blind nightly-recent-not-blind && { echo "FAIL nightly-recent-not-blind: blind"; fails=$((fails + 1)); }
EVENT=schedule scenario nightly-blind "::error title=bench: blind for 3 days::no Sengi verdict on main for 3 days" main-bench:3 \
  "1 49 success push 1;2 48 success schedule 30;3 47 success push 100" "1=$X;2=$X;3=$S" "1 2 3"
blind nightly-blind || { echo "FAIL nightly-blind: no bench-blind"; fails=$((fails + 1)); }
grep -q "Bench is blind" "$T/nightly-blind/summary" || { echo "FAIL nightly-blind: no summary line"; fails=$((fails + 1)); }
EVENT=schedule scenario nightly-blind-nothing "blind for 3 days" - "" "" ""
mkdir -p "$T/nightly-failed-read-not-blind"; touch "$T/nightly-failed-read-not-blind/FAIL"
EVENT=schedule scenario nightly-failed-read-not-blind "history unreadable" - "1 49 success" "1=$S" "1"
blind nightly-failed-read-not-blind && { echo "FAIL nightly-failed-read-not-blind: blind on a failed read"; fails=$((fails + 1)); }
mkdir -p "$T/nightly-jobs-unreadable-not-blind"; touch "$T/nightly-jobs-unreadable-not-blind/JOBFAIL"
EVENT=schedule scenario nightly-jobs-unreadable-not-blind "history unreadable" - "1 49 success schedule 30" "1=$S" ""
blind nightly-jobs-unreadable-not-blind && { echo "FAIL nightly-jobs-unreadable-not-blind: blind on unreadable jobs"; fails=$((fails + 1)); }
# A window crowded by Factory pushes still finds yesterday's Sengi nightly, artifact or not.
EVENT=schedule WINDOW=2 scenario crowded-window-finds-nightly "main's last Sengi nightly: run 3" main-other:1 \
  "1 49 success push 1;2 48 success push 2;3 47 success schedule 20" "1=$X;2=$X;3=$S" "1 2"
blind crowded-window-finds-nightly && { echo "FAIL crowded-window-finds-nightly: blind"; fails=$((fails + 1)); }
# A nightly timed on Sengi is itself the verdict, even with no Sengi run before it (the gate's first night).
EVENT=schedule BENCH_LABEL=sengi-standard-4-ubuntu-2404 scenario sengi-nightly-is-its-own-verdict \
  "this nightly ran on Sengi" main-other:1 "1 49 success push 1;2 48 success push 30" "1=$X;2=$X" "1 2"
blind sengi-nightly-is-its-own-verdict && { echo "FAIL sengi-nightly-is-its-own-verdict: blind"; fails=$((fails + 1)); }

# A contended Sengi run with rows over is no verdict: recorded artifacts, worse runner probe p95 over 1.3 ms. Contended:
# run 43677887923 (probe 1.805, 12 rows over, main's red at b39b841a). Quiet: 43536681544 (1.132, zero-start over).
# Late: that quiet run with a runner/probe-late of 1.805 (no recorded run has a late probe yet), the VM that slowed
# after the first probe. Contended with no row over: the nightly 43731286666 (1.563): all under, so it judged.
TD="$here/../bench/testdata" HOT="$TD/run43677887923.json" COOL="$TD/run43536681544.json" ALLUNDER="$TD/run43731286666.json"
LATE="$T/late.json"; jq '. + [{name: "runner/probe-late", n: 120, p50_ms: 0.9, p95_ms: 1.805, max_ms: 3, budget_ms: 0, ok: true,
  tries: 1, first_p95_ms: 1.805}]' "$COOL" > "$LATE"
# with <scenario> <id=file ...>: those runs' recorded bench.json (w=file: the run under test's own).
with() { local s=$1; shift; mkdir -p "$T/$s/bench" "$T/$s/w"
  for a; do if [ "${a%%=*}" = w ]; then cp "${a#*=}" "$T/$s/w/bench.json"; else cp "${a#*=}" "$T/$s/bench/${a%%=*}"; fi; done; }
with sengi-nightly-contended-is-no-verdict w="$HOT"
EVENT=schedule BENCH_LABEL=sengi-standard-4-ubuntu-2404 scenario sengi-nightly-contended-is-no-verdict \
  "but was contended (runner/probe p95 1.805 ms, over 1.3, with rows over): no verdict" - "1 49 success push 1;2 48 success push 100" "1=$X;2=$S" "1 2"
blind sengi-nightly-contended-is-no-verdict || { echo "FAIL sengi-nightly-contended-is-no-verdict: no bench-blind"; fails=$((fails + 1)); }
with sengi-nightly-late-probe-is-no-verdict w="$LATE"
EVENT=schedule BENCH_LABEL=sengi-standard-4-ubuntu-2404 scenario sengi-nightly-late-probe-is-no-verdict \
  "but was contended (runner/probe-late p95 1.805 ms" - "1 49 success push 100" "1=$S" "1"
blind sengi-nightly-late-probe-is-no-verdict || { echo "FAIL sengi-nightly-late-probe-is-no-verdict: no bench-blind"; fails=$((fails + 1)); }
with sengi-nightly-quiet-is-its-verdict w="$COOL"
EVENT=schedule BENCH_LABEL=sengi-standard-4-ubuntu-2404 scenario sengi-nightly-quiet-is-its-verdict \
  "this nightly ran on Sengi (sengi-standard-4-ubuntu-2404): main has today's verdict" - "1 49 success push 100" "1=$X" "1"
blind sengi-nightly-quiet-is-its-verdict && { echo "FAIL sengi-nightly-quiet-is-its-verdict: blind"; fails=$((fails + 1)); }
# A Factory nightly whose only Sengi runs in 3 days were contended (the newest the baseline, one in the history,
# one by its late probe): still blind, and it says so.
with contended-history-stays-blind 2="$HOT" 3="$HOT" 4="$LATE"
EVENT=schedule scenario contended-history-stays-blind "other than 3 on a contended Sengi VM (a runner probe p95 over 1.3 ms, with rows over: no verdict)" - \
  "1 49 success push 1;2 48 success push 10;3 47 success schedule 20;4 46 success schedule 40;5 45 success schedule 100" \
  "1=$X;2=$S;3=$S;4=$S;5=$S" "1 2 3 4 5"
blind contended-history-stays-blind || { echo "FAIL contended-history-stays-blind: no bench-blind"; fails=$((fails + 1)); }
grep -q "run 2 on Sengi .* was contended (runner/probe p95 1.805 ms" "$T/contended-history-stays-blind/out" &&
  grep -q "run 4 on Sengi .* was contended (runner/probe-late p95 1.805 ms" "$T/contended-history-stays-blind/out" ||
  { echo "FAIL contended-history-stays-blind: runs 2 and 4 not named contended"; fails=$((fails + 1)); }
# A quiet Sengi run behind contended ones still counts: a push, or a nightly with no artifact (it judged when it ran).
with quiet-push-behind-contended 2="$HOT" 3="$COOL"
EVENT=schedule scenario quiet-push-behind-contended "main's last Sengi run: run 3" - \
  "1 49 success push 1;2 48 success push 10;3 47 success push 40" "1=$X;2=$S;3=$S" "1 2 3"
blind quiet-push-behind-contended && { echo "FAIL quiet-push-behind-contended: blind"; fails=$((fails + 1)); }
with quiet-nightly-behind-contended 2="$HOT"
EVENT=schedule scenario quiet-nightly-behind-contended "main's last Sengi nightly: run 3" - \
  "1 49 success push 1;2 48 success schedule 10;3 47 success schedule 40" "1=$X;2=$S;3=$S" "1 2"
blind quiet-nightly-behind-contended && { echo "FAIL quiet-nightly-behind-contended: blind"; fails=$((fails + 1)); }
# Contended with no row over judged (all under even so): this nightly, or one in the history.
with sengi-nightly-all-under-is-its-verdict w="$ALLUNDER"
EVENT=schedule BENCH_LABEL=sengi-standard-4-ubuntu-2404 scenario sengi-nightly-all-under-is-its-verdict \
  "this nightly ran on Sengi (sengi-standard-4-ubuntu-2404): main has today's verdict" - "1 49 success push 100" "1=$X" "1"
blind sengi-nightly-all-under-is-its-verdict && { echo "FAIL sengi-nightly-all-under-is-its-verdict: blind"; fails=$((fails + 1)); }
with all-under-history-not-blind 2="$ALLUNDER"
EVENT=schedule scenario all-under-history-not-blind "on Sengi: run 2" - "1 49 success push 1;2 48 success schedule 20" "1=$X;2=$S" "1 2"
blind all-under-history-not-blind && { echo "FAIL all-under-history-not-blind: blind"; fails=$((fails + 1)); }
# A Sengi push behind a contended one whose bench cannot be downloaded, though listed: not judged, never blind. One
# with no bench listed judged nothing: skipped (blind).
with push-download-fails-not-judged 2="$HOT"; mkdir -p "$T/push-download-fails-not-judged/dlfail"; touch "$T/push-download-fails-not-judged/dlfail/3"
EVENT=schedule scenario push-download-fails-not-judged "could not read run 3's bench, so blindness is not judged" - \
  "1 49 success push 1;2 48 success push 10;3 47 success push 40" "1=$X;2=$S;3=$S" "1 2 3"
blind push-download-fails-not-judged && { echo "FAIL push-download-fails-not-judged: blind"; fails=$((fails + 1)); }
with push-no-bench-skipped 2="$HOT"
EVENT=schedule scenario push-no-bench-skipped "blind for 3 days" - \
  "1 49 success push 1;2 48 success push 10;3 47 success push 40" "1=$X;2=$S;3=$S" "1 2"
# A quiet baseline is a verdict, as before.
with quiet-baseline-not-blind 2="$COOL"
EVENT=schedule scenario quiet-baseline-not-blind "on Sengi: run 2" - "1 49 success push 1;2 48 success push 10" "1=$X;2=$S" "1 2"
blind quiet-baseline-not-blind && { echo "FAIL quiet-baseline-not-blind: blind"; fails=$((fails + 1)); }

# The dedicated VM (BENCH_LABEL zm-bench) pairs only with zm-bench runs; while it has no ceilings file it
# counts its runs (bench-class-runs); blindness counts a run on either class.
Z="zm-bench workshop-bench"
runs_file() { cat "$T/$1/w/bench-class-runs" 2>/dev/null; }
BENCH_LABEL=zm-bench scenario zm-bench-pairs-with-its-class "main's last bench on zm-bench: run 2" main-bench:2 \
  "1 49 success push 1;2 48 success push 2;3 47 success push 3;4 46 success push 4" "1=$S;2=$Z;3=$X;4=$Z" "1 2 3 4"
[ "$(runs_file zm-bench-pairs-with-its-class)" = 2 ] || { echo "FAIL zm-bench-pairs-with-its-class: bench-class-runs '$(runs_file zm-bench-pairs-with-its-class)', want 2"; fails=$((fails + 1)); }
BENCH_LABEL=zm-bench scenario zm-bench-never-pairs-with-sengi "::warning title=bench: no zm-bench verdict::no real verdict in the window: none of main's last 2 finished bench runs ran on zm-bench" \
  main-other:1 "1 49 success push 1;2 48 success push 2" "1=$S;2=$S" "1 2"
[ ! -e "$T/zm-bench-never-pairs-with-sengi/w/main-bench" ] || { echo "FAIL zm-bench-never-pairs-with-sengi: a main-bench"; fails=$((fails + 1)); }
[ "$(runs_file zm-bench-never-pairs-with-sengi)" = 0 ] || { echo "FAIL zm-bench-never-pairs-with-sengi: bench-class-runs not 0"; fails=$((fails + 1)); }
# Calibrated (its file exists): no count, and the walk stops at the pair.
mkdir -p "$T/zm-bench-calibrated/w/scripts/ceilings"; echo '{}' > "$T/zm-bench-calibrated/w/scripts/ceilings/ceilings.zm-bench.json"
BENCH_LABEL=zm-bench scenario zm-bench-calibrated "main's last bench on zm-bench: run 1" main-bench:1 "1 49 success;2 48 success" "1=$Z;2=$Z" "1 2"
[ ! -e "$T/zm-bench-calibrated/w/bench-class-runs" ] || { echo "FAIL zm-bench-calibrated: counted runs"; fails=$((fails + 1)); }
# A Sengi run never pairs with a zm-bench one, and writes no count.
scenario sengi-skips-zm-bench "on Sengi: run 2" main-bench:2 "1 49 success;2 48 success" "1=$Z;2=$S" "1 2"
[ ! -e "$T/sengi-skips-zm-bench/w/bench-class-runs" ] || { echo "FAIL sengi-skips-zm-bench: counted runs"; fails=$((fails + 1)); }
# The first zm-bench nightly after the switch is that day's run on a verdict class,
# though zm-bench has no history yet: not blind, and said to be a calibration run.
EVENT=schedule BENCH_LABEL=zm-bench scenario switch-day-not-blind "this nightly ran on zm-bench (zm-bench): main has today's calibration run" - \
  "1 49 success push 1;2 48 success push 100" "1=$X;2=$X" "1 2"
blind switch-day-not-blind && { echo "FAIL switch-day-not-blind: blind"; fails=$((fails + 1)); }
grep -qx "this nightly ran on zm-bench (zm-bench): main has today's calibration run" "$T/switch-day-not-blind/out" ||
  { echo "FAIL switch-day-not-blind: not that exact line"; fails=$((fails + 1)); }
mkdir -p "$T/zm-bench-nightly-calibrated/w/scripts/ceilings"; echo '{}' > "$T/zm-bench-nightly-calibrated/w/scripts/ceilings/ceilings.zm-bench.json"
EVENT=schedule BENCH_LABEL=zm-bench scenario zm-bench-nightly-calibrated "this nightly ran on zm-bench (zm-bench): main has today's verdict" - "" "" ""
# A nightly elsewhere counts a zm-bench nightly in the history as a verdict class's run.
EVENT=schedule scenario zm-bench-nightly-not-blind "main's last zm-bench nightly: run 1" - "1 49 success schedule 20" "1=$Z" "1"
blind zm-bench-nightly-not-blind && { echo "FAIL zm-bench-nightly-not-blind: blind"; fails=$((fails + 1)); }
EVENT=schedule scenario factory-nightly-still-blind "no Sengi verdict on main for 3 days" - "1 49 success schedule 20" "1=$X" "1"
blind factory-nightly-still-blind || { echo "FAIL factory-nightly-still-blind: no bench-blind"; fails=$((fails + 1)); }

# A zm pool box (BENCH_LABEL <pool>:<size>; GitHub lists the job's labels self-hosted,<pool>:<size>) is class
# box-<size>: it pairs only with that size's runs, and calibrates like zm-bench.
B="self-hosted,sengi:c2m8 zm-77"
BENCH_LABEL=sengi:c2m8 scenario box-pairs-with-its-size "main's last bench on box-c2m8: run 3" main-bench:3 \
  "1 49 success push 1;2 48 success push 2;3 47 success push 3" "1=$S;2=self-hosted,sengi:c2m4 zm-5;3=$B" "1 2 3"
[ "$(runs_file box-pairs-with-its-size)" = 1 ] || { echo "FAIL box-pairs-with-its-size: bench-class-runs '$(runs_file box-pairs-with-its-size)', want 1"; fails=$((fails + 1)); }
# Calibration ready: from CALIBRATE_MIN runs (this one included) every run says what to do; a push never goes blind.
CALIBRATE_MIN=3 BENCH_LABEL=sengi:c2m8 scenario calibration-ready-warns \
  "::warning title=bench: calibration ready::box-c2m8 has 3 runs (this one included) and no ceilings file" - \
  "1 49 success push 100;2 48 success push 200" "1=$B;2=$B" "1 2"
grep -q "CLASS=box-c2m8 scripts/ci/bench-calibrate.sh" "$T/calibration-ready-warns/summary" || { echo "FAIL calibration-ready-warns: no summary line"; fails=$((fails + 1)); }
blind calibration-ready-warns && { echo "FAIL calibration-ready-warns: blind on a push"; fails=$((fails + 1)); }
CALIBRATE_MIN=4 BENCH_LABEL=sengi:c2m8 scenario calibration-not-ready "box-c2m8 is calibrating" - \
  "1 49 success push 1;2 48 success push 2" "1=$B;2=$B" "1 2"
grep -q "calibration ready" "$T/calibration-not-ready/out" && { echo "FAIL calibration-not-ready: said ready"; fails=$((fails + 1)); }
# A nightly goes blind once calibration was already ready STALE_DAYS ago: CALIBRATE_MIN of the class's runs finished
# before the cutoff (runs 3 and 4, 100 h and more ago; run 2 is another class). Looked up past the window: WINDOW=1
# holds only run 1, as pushes fill it in hours. Only one class run before the cutoff: still a calibration run.
EVENT=schedule WINDOW=1 CALIBRATE_MIN=2 BENCH_LABEL=sengi:c2m8 scenario calibration-stale-nightly-blind \
  "::error title=bench: blind (calibration not committed)::calibration of box-c2m8 was ready 3 days ago (2 of its runs finished before" - \
  "1 49 success push 1;2 48 success push 90;3 47 success push 100;4 46 success push 200" "1=$B;2=$X;3=$B;4=$B" "1 2 3 4"
blind calibration-stale-nightly-blind || { echo "FAIL calibration-stale-nightly-blind: no bench-blind"; fails=$((fails + 1)); }
[ -s "$T/calibration-stale-nightly-blind/cutoff" ] || { echo "FAIL calibration-stale-nightly-blind: no created filter"; fails=$((fails + 1)); }
EVENT=schedule CALIBRATE_MIN=2 BENCH_LABEL=sengi:c2m8 scenario calibration-fresh-nightly-not-blind \
  "this nightly ran on box-c2m8 (sengi:c2m8): main has today's calibration run" - \
  "1 49 success push 20;2 48 success push 30;3 47 success push 200" "1=$B;2=$B;3=$B" "1 2 3"
# The same history on a push: it only warns.
CALIBRATE_MIN=2 BENCH_LABEL=sengi:c2m8 scenario calibration-stale-push-not-blind "calibration ready" - \
  "1 49 success push 1;2 48 success push 100;3 47 success push 200" "1=$B;2=$B;3=$B" "1 2 3"
blind calibration-stale-push-not-blind && { echo "FAIL calibration-stale-push-not-blind: blind"; fails=$((fails + 1)); }
# A run before the cutoff whose jobs cannot be read stops the count: not proof of anything, never a blind red (runs 3
# and 4 would make it stale if the walk went past run 2).
mkdir -p "$T/calibration-unreadable-not-blind/jobfail"; touch "$T/calibration-unreadable-not-blind/jobfail/2"
EVENT=schedule WINDOW=1 CALIBRATE_MIN=2 BENCH_LABEL=sengi:c2m8 scenario calibration-unreadable-not-blind "calibration ready" - \
  "1 49 success push 1;2 48 success push 100;3 47 success push 150;4 46 success push 200" "1=$B;2=$B;3=$B;4=$B" "1 2 3 4"
blind calibration-unreadable-not-blind && { echo "FAIL calibration-unreadable-not-blind: blind"; fails=$((fails + 1)); }
blind calibration-fresh-nightly-not-blind && { echo "FAIL calibration-fresh-nightly-not-blind: blind"; fails=$((fails + 1)); }
# A class run that left no bench artifact (it broke before timing: the box's first runs) is no calibration data: not
# counted, for the notice or for staleness. Runs 1 and 2 failed with nothing; 3 and 4 alone are below CALIBRATE_MIN=3.
CALIBRATE_MIN=4 BENCH_LABEL=sengi:c2m8 scenario calibration-counts-only-bench-runs "box-c2m8 is calibrating (no scripts/ceilings/ceilings.box-c2m8.json): 2 earlier runs" - \
  "1 49 failure push 1;2 48 failure push 2;3 47 success push 3;4 46 success push 4" "1=$B;2=$B;3=$B;4=$B" "3 4"
grep -q "calibration ready" "$T/calibration-counts-only-bench-runs/out" && { echo "FAIL calibration-counts-only-bench-runs: said ready"; fails=$((fails + 1)); }
[ "$(runs_file calibration-counts-only-bench-runs)" = 2 ] || { echo "FAIL calibration-counts-only-bench-runs: bench-class-runs '$(runs_file calibration-counts-only-bench-runs)', want 2"; fails=$((fails + 1)); }
EVENT=schedule WINDOW=1 CALIBRATE_MIN=2 BENCH_LABEL=sengi:c2m8 scenario calibration-stale-needs-bench-runs "calibration ready" - \
  "1 49 success push 1;2 48 success push 100;3 47 failure push 150;4 46 failure push 200" "1=$B;2=$B;3=$B;4=$B" "1 2"
blind calibration-stale-needs-bench-runs && { echo "FAIL calibration-stale-needs-bench-runs: blind"; fails=$((fails + 1)); }
# Exactly CALIBRATE_MIN with this nightly: ready since now, not blind.
EVENT=schedule CALIBRATE_MIN=2 BENCH_LABEL=sengi:c2m8 scenario calibration-ready-tonight-not-blind "calibration ready" - \
  "1 49 success push 200" "1=$B" "1"
blind calibration-ready-tonight-not-blind && { echo "FAIL calibration-ready-tonight-not-blind: blind"; fails=$((fails + 1)); }

[ "$fails" = 0 ] || exit 1
