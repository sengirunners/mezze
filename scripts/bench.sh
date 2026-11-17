#!/usr/bin/env bash
# Latency benchmark of a real zm daemon in a throwaway ZM_HOME: p50/p95
# of every hot path, process start included, and exit 1 if a p95 is over its
# ceiling (scripts/ceilings/ceilings.json, set from Sengi runs: a Mac run says
# nothing about them; results: DESIGN-NOTES.md).
#
#   scripts/bench.sh                    build, measure everything (n=40)
#   N=100 scripts/bench.sh              more runs
#   ONLY=hook-prompt,notice scripts/bench.sh
#   BENCH_JSON=bench.json scripts/bench.sh   also write the results as JSON
#   ZM=/path/to/zm scripts/bench.sh   measure another build (no ceiling check: BUDGETS=false;
#                                           replica-* and team-* rows run in the bench itself,
#                                           from this tree, so they are off)
#   MAIL_LIMITS=false ZM=/path/to/zm scripts/bench.sh   a build from before send limits (it refuses [mail_limits])
#
# Never touches ~/.zm or real dotfiles; kills every process it starts.
set -euo pipefail

repo=$(cd "$(dirname "$0")/.." && pwd)
T=$(mktemp -d /tmp/zm-bench.XXXX)
trap 'rm -rf "$T"' EXIT
GO=$(command -v go)
unset ZM_AS ZM_PROJECT ZM_TOKEN ZM_HOST ZM_SESSION CLAUDE_CODE_SESSION_ID CODEX_THREAD_ID CI

bin=${ZM:-}
if [ -z "$bin" ]; then
	(cd "$repo" && "$GO" build -o "$T/" ./cmd/zm)
	bin=$T/zm
fi
(cd "$repo" && "$GO" build -o "$T/bench" ./scripts/bench && "$GO" build -o "$T/probe" ./scripts/bench/probe)
"$T/bench" -zm "$bin" -probe "$T/probe" -n "${N:-40}" -budgets="${BUDGETS:-true}" -mail-limits="${MAIL_LIMITS:-true}" ${ONLY:+-only "$ONLY"} \
	${BENCH_JSON:+-out "$BENCH_JSON"} ${ZM:+-replica=false}
