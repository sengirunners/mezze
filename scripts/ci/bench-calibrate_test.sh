#!/usr/bin/env bash
# bench-calibrate.sh against a fake gh and a fake go: it takes only the class's runs with an artifact, stops
# at COUNT, refuses with too few, and never overwrites a committed class file. Run by ci.yml's check job.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
mkdir "$T/bin"
# $F/runs: "<id> <event> <conclusion> <label>" lines, newest first; $F/art/<id>: the run has a bench artifact.
cat > "$T/bin/gh" <<'FAKE'
#!/usr/bin/env bash
case "$1 $2" in
"api "*/jobs) id=${2%/jobs}; id=${id##*/}
  l=$(awk -v id="$id" '$1 == id {print $4}' "$F/runs")
  jq -n --arg l "$l" '{jobs: [{name: "route", labels: ["x"]}, {name: "make bench (latency budgets)", labels: ($l | split(","))}]}' ;;
"api "*) while read -r id e c l; do jq -n --argjson id "$id" --arg e "$e" --arg c "$c" '{id: $id, event: $e, conclusion: $c}'
  done < "$F/runs" | jq -s '{workflow_runs: .}' ;;
"run download") [ -f "$F/art/$3" ] || exit 1; mkdir -p "$9"; echo "[\"$3\"]" > "$9/bench.json" ;;
esac
FAKE
cat > "$T/bin/go" <<'FAKE'
#!/usr/bin/env bash
echo "$*" > "$F/go-args"; echo '{"bench": {}}'
FAKE
chmod +x "$T/bin/"*
export GH_REPO=example-org/zm
fails=0
# scenario <name> <want output substring> <want exit> <runs, ; separated> <ids with artifacts>
scenario() {
  local name=$1 sub=$2 want=$3 out status
  export F="$T/$name"; mkdir -p "$F/art" "$F/w/scripts/ceilings"
  tr ';' '\n' <<<"$4" > "$F/runs"
  for id in $5; do touch "$F/art/$id"; done
  [ -z "${EXISTING:-}" ] || echo '{}' > "$F/w/scripts/ceilings/ceilings.zm-bench.json"
  out=$(cd "$F/w" && PATH="$T/bin:$PATH" CLASS="${CLASS:-zm-bench}" COUNT=2 timeout 20 "$here/bench-calibrate.sh" 2>&1); status=$?
  if [ "$status" != "$want" ] || [[ "$out" != *"$sub"* ]]; then
    echo "FAIL $name: status $status (want $want), want \"$sub\" in:"; echo "$out"; fails=$((fails + 1))
  else echo "ok $name"; fi
}
scenario takes-the-class "wrote scripts/ceilings/ceilings.zm-bench.json from 2 runs" 0 \
  "1 push success sengi-standard-4-ubuntu-2404;2 push cancelled zm-bench;3 schedule failure zm-bench;4 workflow_dispatch success zm-bench;5 push success zm-bench;6 push success zm-bench;7 push success zm-bench" "1 2 3 4 6 7"
args=$(cat "$T/takes-the-class/go-args")
[[ "$args" == "run ./scripts/bench calibrate -class zm-bench -min 2 "*"/3/bench.json "*"/6/bench.json" ]] || { echo "FAIL takes-the-class: go $args"; fails=$((fails + 1)); }
[ -s "$T/takes-the-class/w/scripts/ceilings/ceilings.zm-bench.json" ] || { echo "FAIL takes-the-class: no file"; fails=$((fails + 1)); }
# A zm pool box: GitHub lists the job's labels (self-hosted,<pool>:<size>); its class is box-<size>, and another
# size is another class.
CLASS=box-c2m8 scenario takes-a-box "wrote scripts/ceilings/ceilings.box-c2m8.json from 2 runs" 0 \
  "1 push success self-hosted,sengi:c2m4;2 push success self-hosted,sengi:c2m8;3 push success zm-bench;4 schedule success self-hosted,sengi:c2m8" "1 2 3 4"
[[ "$(cat "$T/takes-a-box/go-args")" == "run ./scripts/bench calibrate -class box-c2m8 -min 2 "*"/2/bench.json "*"/4/bench.json" ]] ||
  { echo "FAIL takes-a-box: go $(cat "$T/takes-a-box/go-args")"; fails=$((fails + 1)); }
scenario too-few "has 1 finished main runs with a bench artifact, need 2: keep calibrating" 1 "1 push success zm-bench;2 push success sengi-x" "1 2"
[ ! -e "$T/too-few/w/scripts/ceilings/ceilings.zm-bench.json" ] || { echo "FAIL too-few: wrote a file"; fails=$((fails + 1)); }
EXISTING=1 scenario never-overwrites "exists: zm-bench is calibrated, and its file only tightens" 1 "1 push success zm-bench;2 push success zm-bench" "1 2"
[ "$(cat "$T/never-overwrites/w/scripts/ceilings/ceilings.zm-bench.json")" = '{}' ] || { echo "FAIL never-overwrites: overwritten"; fails=$((fails + 1)); }
[ "$fails" = 0 ] || exit 1
