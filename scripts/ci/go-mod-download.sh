#!/usr/bin/env bash
# Prefetch Go modules with a bounded, retried `go mod download`, so a flaky
# module proxy fails a try instead of hanging a job until its timeout.
# Extra args are one more go command run under the same retry, e.g.
#   scripts/ci/go-mod-download.sh install golang.org/x/vuln/cmd/govulncheck@v1.9.0
# Env: TRIES (5), TRY_TIMEOUT seconds per try (120).
set -euo pipefail

tries=${TRIES:-5}
per_try=${TRY_TIMEOUT:-120}

retry() {
	local n=1 delay=5 status
	while :; do
		echo "go $* (try $n/$tries, ${per_try}s limit)"
		status=0
		timeout --kill-after=10 "$per_try" go "$@" || status=$?
		[ "$status" -eq 0 ] && return 0
		if [ "$n" -ge "$tries" ]; then
			echo "::error::go $* failed after $tries tries (last exit $status): module proxy unreachable?" >&2
			exit 1
		fi
		echo "go $* failed (exit $status); retrying in ${delay}s" >&2
		sleep "$delay"
		n=$((n + 1))
		delay=$((delay * 2))
	done
}

retry mod download
if [ "$#" -gt 0 ]; then retry "$@"; fi
