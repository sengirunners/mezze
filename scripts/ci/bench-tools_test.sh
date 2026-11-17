#!/usr/bin/env bash
# bench-tools.sh against a fake curl: nothing when every tool is there; a missing tool is fetched as pinned
# (a bare binary or one inside a tarball), its sha256 checked, into DIR, which goes on GITHUB_PATH; a wrong sum or no
# pin fails the step and installs nothing. No root anywhere. Run by ci.yml's check job.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
mkdir -p "$T/bin" "$T/srv/pkg/bin"
# The fake curl logs its URL and serves $T/srv/<basename of the URL>.
cat > "$T/bin/curl" <<'FAKE'
#!/usr/bin/env bash
out= url=
while [ $# -gt 0 ]; do case "$1" in -o) out=$2; shift 2 ;; --retry) shift 2 ;; -*) shift ;; *) url=$1; shift ;; esac; done
echo "$url" >> "$F/curl"
cp "$SRV/${url##*/}" "$out"
FAKE
chmod +x "$T/bin/curl"
printf '#!/bin/sh\necho zz 1\n' > "$T/srv/zz-bare"
printf '#!/bin/sh\necho zz 1\n' > "$T/srv/pkg/bin/zz-tar"
tar -czf "$T/srv/zz-tar.tar.gz" -C "$T/srv" pkg/bin/zz-tar
sum() { if command -v sha256sum >/dev/null; then sha256sum "$1"; else shasum -a 256 "$1"; fi | cut -d' ' -f1; }
arch=$(uname -m)
{
  echo "# comment"
  echo "zz-bare $arch https://dl.example/zz-bare $(sum "$T/srv/zz-bare") ."
  echo "zz-bare other-arch https://dl.example/zz-bare 0000 ."
  echo "zz-tar $arch https://dl.example/zz-tar.tar.gz $(sum "$T/srv/zz-tar.tar.gz") pkg/bin/zz-tar"
  echo "zz-badtar $arch https://dl.example/zz-tar.tar.gz $(sum "$T/srv/zz-tar.tar.gz") pkg/bin/elsewhere"
  echo "zz-tarsum $arch https://dl.example/zz-tar.tar.gz $(printf '1%.0s' {1..64}) pkg/bin/zz-tar"
  echo "zz-bad $arch https://dl.example/zz-bare $(printf '0%.0s' {1..64}) ."
} > "$T/pins"
# sha256sum on a Mac: shasum speaks the same check format.
command -v sha256sum >/dev/null || { printf '#!/bin/sh\nexec shasum -a 256 "$@"\n' > "$T/bin/sha256sum"; chmod +x "$T/bin/sha256sum"; }
fails=0
scenario() { # <name> <TOOLS> <want exit> <want output substring>
  local name=$1 out status
  export F="$T/$name" SRV="$T/srv"; mkdir -p "$F"
  out=$(PATH="$T/bin:$PATH" TOOLS=$2 PINS="$T/pins" DIR="$F/dir" GITHUB_PATH="$F/path" timeout 20 "$here/bench-tools.sh" 2>&1)
  status=$?
  if [ "$status" != "$3" ] || [[ "$out" != *"$4"* ]]; then
    echo "FAIL $name: status $status (want $3), want \"$4\" in:"; echo "$out"; fails=$((fails + 1))
  else echo "ok $name"; fi
}
check() { "$@" || { echo "FAIL: $*"; fails=$((fails + 1)); }; }
scenario all-present "sh bash" 0 "bench tools present"
check test ! -e "$T/all-present/curl"
scenario fetches-missing "sh zz-bare zz-tar" 0 "fetched into $T/fetches-missing/dir: zz-bare zz-tar"
check test "$("$T/fetches-missing/dir/zz-bare")" = "zz 1"
check test "$("$T/fetches-missing/dir/zz-tar")" = "zz 1"
check test "$(cat "$T/fetches-missing/path")" = "$T/fetches-missing/dir"
check test "$(cat "$T/fetches-missing/curl")" = "$(printf 'https://dl.example/zz-bare\nhttps://dl.example/zz-tar.tar.gz')"
scenario wrong-sum-fails "zz-bad" 1 "zz-bad download does not match its pinned sha256"
check test ! -e "$T/wrong-sum-fails/dir/zz-bad"
check test ! -s "$T/wrong-sum-fails/path"
scenario wrong-tar-sum-fails "zz-tarsum" 1 "zz-tarsum download does not match its pinned sha256"
check test ! -e "$T/wrong-tar-sum-fails/dir/zz-tarsum"
scenario tar-member-missing-fails "zz-badtar" 1 "bench tools: zz-badtar: no pkg/bin/elsewhere in its download"
check test ! -s "$T/tar-member-missing-fails/path"
scenario no-pin-fails "zz-none" 1 "no pin for zz-none on $arch"
# The real pins: every tool the job fetches has a pin for both runner arches, each a 64-hex sum.
for t in jq gh; do for a in x86_64 aarch64; do
  check grep -Eq "^$t $a https://[^ ]+ [0-9a-f]{64} [^ ]+$" "$here/bench-tools.pins"
done; done
[ "$fails" = 0 ] || exit 1
