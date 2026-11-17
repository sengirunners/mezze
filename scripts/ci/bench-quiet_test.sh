#!/usr/bin/env bash
# bench-quiet.sh against a fake gh: when it starts timing, when it waits, and when it gives up waiting.
# Run by ci.yml's check job.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
mkdir "$T/bin"
# Each runs read pops the next line of $F/runs: space-separated <workflow>:<id>:<status>[:<age in minutes>]
# (FAIL fails the read); the last line repeats. A run's age defaults to 1 minute.
cat > "$T/bin/gh" <<'FAKE'
#!/usr/bin/env bash
[ -e "$F/runs" ] || { echo '{"workflow_runs":[]}'; exit 0; }
line=$(head -1 "$F/runs"); [ "$(wc -l < "$F/runs")" -gt 1 ] && sed -i.bak 1d "$F/runs"
[ "$line" != FAIL ] || exit 1
for r in $line; do IFS=: read -r wf id st age <<<"$r"
  at=$(jq -nr --argjson m "${age:-1}" 'now - $m * 60 | floor | todateiso8601')
  printf '{"id":%s,"path":".github/workflows/%s","status":"%s","created_at":"%s"}\n' "$id" "$wf" "$st" "$at"
done | jq -s '{workflow_runs: .}'
FAKE
chmod +x "$T/bin/gh"
fails=0

# scenario <name> <expected output substring> <QUIET_MAX> [<runs lines, ; separated>]
scenario() {
  local name=$1 want=$2 max=$3 out status
  export F="$T/$name"; mkdir "$F"
  [ $# -lt 4 ] || tr ';' '\n' <<<"$4" > "$F/runs"
  out=$(PATH="$T/bin:$PATH" BENCH_LABEL="${BENCH_LABEL:-}" GH_REPO=o/r RUN_ID=7 QUIET_MAX=$max EVERY=0 GITHUB_STEP_SUMMARY="$F/summary" \
    timeout 20 "$here/bench-quiet.sh" 2>&1); status=$?
  if [ "$status" != 0 ] || [[ "$out" != *"$want"* ]]; then
    echo "FAIL $name: status $status, want \"$want\" in:"; echo "$out"; fails=$((fails + 1))
  else echo "ok $name"; fi
}

scenario nothing-running "quiet after 0s" 60
scenario only-itself "quiet after" 60 "bench.yml:7:in_progress"
scenario finished-and-other-workflows "quiet after" 60 "ci.yml:5:completed z3-report.yml:6:in_progress factory-watch.yml:8:pending"
scenario waits-for-ci "waiting for: ci.yml:5" 60 "ci.yml:5:in_progress;ci.yml:5:in_progress;"
scenario pending-counts "waiting for: ci.yml:9" 60 "ci.yml:9:pending;"
scenario queued-counts "waiting for: real-tests.yml:4" 60 "real-tests.yml:4:queued;"
scenario then-quiet "runner pool quiet" 60 "ci.yml:5:waiting;"
scenario failed-read-is-busy "waiting for: unknown" 60 "FAIL;"
scenario older-bench-running-waits "waiting for: bench.yml:5" 60 "bench.yml:5:in_progress;"
# A newer bench waits for this one: counting it waited out QUIET_MAX on every main run with a merge behind it.
scenario newer-bench-is-not-busy "quiet after" 60 "bench.yml:7:in_progress bench.yml:9:pending bench.yml:10:in_progress"
scenario older-waiting-bench-counts "waiting for: bench.yml:5 bench.yml:6" 60 "bench.yml:5:pending bench.yml:6:queued;"
scenario older-stuck-bench-is-not-busy "quiet after" 60 "bench.yml:5:queued:180 bench.yml:6:waiting:300"
scenario gives-up "measuring anyway" 0 "bench.yml:7:in_progress ci.yml:8:queued"
grep -q 'ci.yml:8' "$T/gives-up/summary" || { echo "FAIL gives-up: no summary line"; fails=$((fails + 1)); }
# A dedicated runner (a zm pool box, or zm-bench) runs one job at a time and only this bench: no wait, and no
# runs read at all (FAIL would be busy). The rule is bench-class.sh's, the same as the class's.
BENCH_LABEL=zm-bench scenario dedicated-never-waits "dedicated bench runner (zm-bench): one job at a time" 60 "FAIL;ci.yml:5:in_progress bench.yml:5:in_progress"
[ ! -s "$T/dedicated-never-waits/summary" ] || { echo "FAIL dedicated-never-waits: a summary line"; fails=$((fails + 1)); }
BENCH_LABEL=sengi:c2m8 scenario box-never-waits "dedicated bench runner (sengi:c2m8)" 60 "FAIL;ci.yml:5:in_progress"
BENCH_LABEL=zm-benchmark scenario not-a-class-still-waits "waiting for: ci.yml:5" 60 "ci.yml:5:in_progress;"
BENCH_LABEL=sengi-standard-4-ubuntu-2404 scenario sengi-still-waits "waiting for: ci.yml:5" 60 "ci.yml:5:in_progress;"
scenario factory-still-waits "waiting for: ci.yml:5" 60 "ci.yml:5:in_progress;"

[ "$fails" = 0 ] || exit 1
