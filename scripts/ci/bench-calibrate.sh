#!/usr/bin/env bash
# Proposes a runner class's ceilings file: downloads the bench.json of the class's last COUNT finished main
# bench runs (push or nightly, red or green) and writes `bench calibrate`'s proposal (highest p95 x 1.6 + 1 ms, bar
# rows capped) to scripts/ceilings/ceilings.<class>.json. A person or agent reviews it and commits it; from that
# commit the class gives verdicts, and the file only tightens (bench tighten -class), so this refuses to overwrite one.
# Run from the repo root with gh logged in: CLASS=box-c2m8 scripts/ci/bench-calibrate.sh   (COUNT=10 by default;
# CLASS is the class the bench job's notice names, bench-class.sh)
set -euo pipefail
# shellcheck source=scripts/ci/bench-class.sh
. "$(dirname "$0")/bench-class.sh"
GH_REPO="${GH_REPO:-$(gh repo view --json nameWithOwner -q .nameWithOwner)}" CLASS="${CLASS:-zm-bench}" COUNT="${COUNT:-10}"
JOB="${JOB:-make bench (latency budgets)}"
[ -n "$GH_REPO" ] || { echo "set GH_REPO=<owner/name>: gh could not name this checkout's repository" >&2; exit 2; }
out="scripts/ceilings/ceilings.$CLASS.json"
if [ -e "$out" ]; then echo "$out exists: $CLASS is calibrated, and its file only tightens (lower a line by hand)" >&2; exit 1; fi
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
ids=$(gh api "repos/$GH_REPO/actions/workflows/bench.yml/runs?branch=main&status=completed&per_page=100" | jq -r '
  .workflow_runs[] | select((.event == "push" or .event == "schedule") and (.conclusion == "success" or .conclusion == "failure")) | .id')
got=0
for id in $ids; do
  labels=$(gh api "repos/$GH_REPO/actions/runs/$id/jobs" | jq -r --arg job "$JOB" \
    'first(.jobs[] | select(.name == $job) | .labels | join(",")) // empty') || continue
  [ "$(class_of "$labels")" = "$CLASS" ] || continue
  if gh run download "$id" -R "$GH_REPO" -n bench -D "$T/$id" >/dev/null 2>&1 && [ -f "$T/$id/bench.json" ]; then
    got=$((got + 1)); echo "run $id ($labels): $got of $COUNT"
    [ "$got" -lt "$COUNT" ] || break
  fi
done
if [ "$got" -lt "$COUNT" ]; then echo "$CLASS has $got finished main runs with a bench artifact, need $COUNT: keep calibrating" >&2; exit 1; fi
go run ./scripts/bench calibrate -class "$CLASS" -min "$COUNT" "$T"/*/bench.json > "$T/proposal.json"
mv "$T/proposal.json" "$out"
echo "wrote $out from $got runs: review it, then commit it (from then on $CLASS's runs give verdicts)"
