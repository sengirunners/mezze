#!/usr/bin/env bash
# Hermetic command-boundary tests for ensure-ripgrep.sh: fake curl, sha256sum and
# uname; no network, no apt, no real rg.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
bash_path=$(command -v bash)
t=$(mktemp -d)
trap 'rm -rf -- "$t"' EXIT
mkdir "$t/bin" "$t/fixture"
for tool in mktemp rm tar gzip mkdir cp chmod mv cat; do ln -s "$(command -v "$tool")" "$t/bin/$tool"; done

name=ripgrep-14.1.1-x86_64-unknown-linux-musl
mkdir "$t/fixture/$name"
cat >"$t/fixture/$name/rg" <<'SH'
#!/bin/bash
printf '%s\n' 'ripgrep fixture'
exit "${RG_EXIT:-0}"
SH
chmod +x "$t/fixture/$name/rg"
tar -czf "$t/release.tar.gz" -C "$t/fixture" "$name"

cat >"$t/bin/uname" <<'SH'
#!/bin/bash
printf '%s\n' "${FAKE_ARCH:-x86_64}"
SH
cat >"$t/bin/curl" <<'SH'
#!/bin/bash
printf 'curl %s\n' "$*" >>"$FAKE_LOG"
if [ "${CURL_EXIT:-0}" != 0 ]; then exit "$CURL_EXIT"; fi
out=
while [ $# -gt 0 ]; do if [ "$1" = -o ]; then out=$2; shift; fi; shift; done
cp "$FAKE_TARBALL" "$out"
SH
cat >"$t/bin/sha256sum" <<'SH'
#!/bin/bash
line=$(cat)
printf 'sha256sum %s <%s>\n' "$*" "${line%%  *}" >>"$FAKE_LOG"
[ -f "${line#*  }" ] || exit 2
exit "${SHA_EXIT:-0}"
SH
chmod +x "$t/bin/uname" "$t/bin/curl" "$t/bin/sha256sum"
export FAKE_LOG="$t/log" FAKE_TARBALL="$t/release.tar.gz"

url=https://github.com/BurntSushi/ripgrep/releases/download/14.1.1/$name.tar.gz
pin=4cf9f2741e6c465ffdb7c26f38056a59e2a2544b51f7cc128ef28337eeae4d8e
fetched="curl -fsSL --retry 5 --retry-all-errors --retry-delay 5 --connect-timeout 20 --max-time 120 -o"
sha_call="sha256sum -c --strict - <$pin>"

# run_case name expected-exit [preinstalled-on-PATH yes|no] [cached yes|no]
run_case() {
	local case=$1 expected=$2 onpath=${3:-no} cached=${4:-no}
	: >"$t/log"
	: >"$t/github_path"
	rm -rf -- "$t/cache" "$t/bin/rg"
	if [ "$onpath" = yes ]; then cp "$t/fixture/$name/rg" "$t/bin/rg"; fi
	if [ "$cached" = yes ]; then mkdir -p "$t/cache" && cp "$t/fixture/$name/rg" "$t/cache/rg"; fi
	local code=0
	PATH="$t/bin" RG_DIR="$t/cache" GITHUB_PATH="$t/github_path" \
		"$bash_path" "$here/ensure-ripgrep.sh" >"$t/out" 2>&1 || code=$?
	if [ "$code" != "$expected" ]; then
		printf 'FAIL %s: exit %s, expected %s\n' "$case" "$code" "$expected"
		cat "$t/out"
		exit 1
	fi
	printf 'ok %s\n' "$case"
}
fail() { printf 'FAIL %s\n' "$1"; cat "$t/out"; exit 1; }
expect_calls() {
	local got
	got=$(cut_urls <"$t/log")
	[ "$got" = "$1" ] || fail "calls: got <$got>, expected <$1>"
}
# Log lines carry temp paths; keep the command and flags, then the URL.
cut_urls() {
	local l
	while IFS= read -r l; do
		case $l in
		curl*) printf '%s %s\n' "${l%% -o *} -o" "${l##* }" ;;
		*) printf '%s\n' "$l" ;;
		esac
	done
}
expect_path() { [ "$(cat "$t/github_path")" = "$1" ] || fail "GITHUB_PATH: got <$(cat "$t/github_path")>, expected <$1>"; }
expect_installed() { [ -x "$t/cache/rg" ] || fail 'rg not installed in RG_DIR'; [ ! -e "$t/cache/rg.partial" ] || fail 'partial left behind'; }
expect_not_installed() { [ ! -e "$t/cache/rg" ] || fail 'rg installed despite failure'; }

run_case rg-on-path-skips-download 0 yes
expect_calls ''
expect_path ''

run_case missing-rg-downloads-pinned-release 0
expect_calls "$fetched $url"$'\n'"$sha_call"
expect_installed
expect_path "$t/cache"
grep -q 'ripgrep fixture' "$t/out" || fail 'version not printed'

run_case cached-rg-skips-download 0 no yes
expect_calls ''
expect_path "$t/cache"

CURL_EXIT=6 run_case unreachable-github-fails 6
expect_not_installed
expect_path ''

SHA_EXIT=1 run_case checksum-mismatch-fails 1
expect_not_installed
expect_path ''

FAKE_ARCH=riscv64 run_case unknown-arch-fails 1
expect_calls ''
expect_path ''

FAKE_ARCH=aarch64 run_case arm-uses-its-own-pin 2
grep -q 'aarch64-unknown-linux-gnu.tar.gz' "$t/log" || fail 'arm URL not used'
grep -q '<c827481c4ff4ea10c9dc7a4022c8de5db34a5737cb74484d62eb94a95841ab2f>' "$t/log" || fail 'arm pin not used'

RG_EXIT=13 run_case broken-rg-is-not-added-to-path 13
expect_path ''

printf '%s\n' 'all 8 ripgrep setup scenarios passed'
