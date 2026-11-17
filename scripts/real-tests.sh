#!/usr/bin/env bash
# The gated real tests: every test named *Real* in the packages below, against
# the pinned tools (ZM_TEST_DOWNLOAD=1: rclone, cloudflared). A gated test that
# skips is a failure, so a lost gate cannot pass quietly; so name a new one
# *Real*.
#
#   scripts/real-tests.sh
#
# A CI workflow can run it on a runner with the tools. HOME and ZM_HOME are
# throwaway.
set -uo pipefail

cd "$(dirname "$0")/.." || exit 1
tmp=$(mktemp -d /tmp/zm-real.XXXX) # short: Unix socket paths max 103 bytes
trap 'rm -rf "$tmp"' EXIT
mkdir -p "$tmp/home"

log=${REAL_TESTS_LOG:-real-tests.log}
summary=${GITHUB_STEP_SUMMARY:-/dev/stdout}
start=$SECONDS
env GOCACHE="$(go env GOCACHE)" GOMODCACHE="$(go env GOMODCACHE)" GOPATH="$(go env GOPATH)" \
	HOME="$tmp/home" ZM_HOME="$tmp/zm" ZM_TEST_TOOLS="$tmp/tools" ZM_TEST_DOWNLOAD=1 \
	go test -p 2 -count=1 -v -timeout 30m -run Real \
	./internal/homes/... ./internal/tools/... 2>&1 |
	tee "$log" | grep --line-buffered -E '^(--- |ok |FAIL|panic:)'
status=${PIPESTATUS[0]}

# The filter above keeps only result lines; a failure's reason (t.Errorf, a
# timeout's goroutine dump) is in the lines between. Print each failed test's
# whole output, so a failure always says why.
failed=$(awk '
	dump { print; next }
	/^=== RUN   [^\/]+$/ { buf = "" }
	{ buf = buf $0 "\n" }
	/^--- FAIL: [^\/]+ \(/ { printf "%s", buf; buf = "" }
	/^panic: / { printf "%s", buf; dump = 1 } # a timeout: the rest is its dump
' "$log")
if [ -n "$failed" ]; then
	printf '\n### Failed test output\n%s\n' "$failed"
fi

closed='set ZM_TEST_DOWNLOAD=1 (to|\(or)'
if grep -E "$closed" "$log"; then
	echo "a gated test skipped with its gate open" >&2
	status=1
fi

{
	echo "### Real tests: $((SECONDS - start)) s, exit $status"
	echo
	echo '| test | result | seconds |'
	echo '| --- | --- | --- |'
	sed -nE 's/^--- (PASS|FAIL|SKIP): ([^ ]+) \(([0-9.]+)s\)$/| \2 | \1 | \3 |/p' "$log"
	if [ -n "$failed" ]; then
		# A step summary is capped at 1 MiB: the summary gets the first 200 KB (the log has it all).
		printf '\n<details><summary>failed test output</summary>\n\n~~~~\n%s\n~~~~\n</details>\n' "$(printf '%s' "$failed" | head -c 200000)$([ ${#failed} -gt 200000 ] && printf '\n(truncated: the job log has it all)')"
	fi
} >>"$summary"
exit "$status"
