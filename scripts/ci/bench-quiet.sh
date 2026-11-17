#!/usr/bin/env bash
# Wait until no other run of the workflows that share bench's runner pool is unfinished, so make bench times zm
# and not a neighbour's go test (main went red whenever ci or another bench ran beside it). Queued,
# pending and waiting runs count: a run held by its concurrency group or waiting for a runner starts the moment
# the one ahead finishes. Another bench counts only while it runs and is older than this one: a newer bench waits
# for this one (its concurrency group, or this same wait), so counting it waited out QUIET_MAX on every main run
# that had a merge behind it (2026-11-07). Bounded: after QUIET_MAX seconds it measures anyway and says
# so in the job summary; the ceilings and the repeat rule are unchanged.
# One runs read per check (the job token has 1000 calls/hour); a failed read counts as busy.
# On a dedicated runner (BENCH_LABEL a zm pool box or zm-bench; bench-class.sh): no wait and no read. It
# takes one job at a time and only this bench, so nothing times beside it; waiting for ci on other pools would only
# delay it.
set -uo pipefail
# shellcheck source=scripts/ci/bench-class.sh
. "$(dirname "$0")/bench-class.sh"
if dedicated "${BENCH_LABEL:-}"; then echo "dedicated bench runner ($BENCH_LABEL): one job at a time, nothing to wait for"; exit 0; fi
: "${GH_REPO:?}" "${RUN_ID:?}"
QUIET_MAX="${QUIET_MAX:-900}" EVERY="${EVERY:-30}"
start=$(date +%s)
while :; do
  busy=$(gh api "repos/$GH_REPO/actions/runs?per_page=100" | jq -r --argjson self "$RUN_ID" '
    [.workflow_runs[] | select(.status != "completed" and .id != $self)
      | select(.path | test("^\\.github/workflows/(ci|real-tests|send-diagnostic|bench)\\.yml$"))
      | select(.path != ".github/workflows/bench.yml" or (.id < $self
          and (now - (.created_at | fromdateiso8601)) < 7200)) # an older bench stuck for hours is not waited on
      | " \(.path | ltrimstr(".github/workflows/")):\(.id)"] | join("")' 2>/dev/null) || busy=" unknown"
  waited=$(($(date +%s) - start))
  if [ -z "$busy" ]; then echo "runner pool quiet after ${waited}s"; exit 0; fi
  if [ "$waited" -ge "$QUIET_MAX" ]; then
    echo "still busy after ${waited}s:$busy; measuring anyway"
    echo "Timed beside other runs (waited ${waited}s):$busy" >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
    exit 0
  fi
  echo "waiting for:$busy"
  sleep "$EVERY"
done
