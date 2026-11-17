#!/usr/bin/env bash
# The tools the bench job runs that a runner may lack (jq and gh, for bench-baseline.sh and the reports), fetched
# when missing. Sengi has them: nothing happens. A zm pool box runs zm's Linux box image, which lacks gh,
# and its container has no root (sudo refuses: "no new privileges"), so nothing is installed system-wide: each
# missing tool is downloaded as pinned in bench-tools.pins, its sha256 checked, into DIR, which goes on the job's PATH
# ($GITHUB_PATH). TOOLS, PINS and DIR override (tests).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
pins=${PINS:-$here/bench-tools.pins} dir=${DIR:-${RUNNER_TEMP:-/tmp}/bench-tools} arch=$(uname -m)
missing=()
for t in ${TOOLS:-jq gh}; do command -v "$t" >/dev/null || missing+=("$t"); done
if [ ${#missing[@]} = 0 ]; then echo "bench tools present"; exit 0; fi
echo "fetching: ${missing[*]} ($arch)"
mkdir -p "$dir"
for t in "${missing[@]}"; do
  read -r _ _ url sum path < <(awk -v t="$t" -v a="$arch" '$1 == t && $2 == a' "$pins") ||
    { echo "bench tools: no pin for $t on $arch in $pins" >&2; exit 1; }
  dl="$dir/$t.download"
  curl -fsSL --retry 3 -o "$dl" "$url"
  echo "$sum  $dl" | sha256sum -c --quiet - || { echo "bench tools: $t download does not match its pinned sha256" >&2; exit 1; }
  if [ "$path" = . ]; then mv "$dl" "$dir/$t"
  else
    tar -xzf "$dl" -C "$dir" "$path" || { echo "bench tools: $t: no $path in its download" >&2; exit 1; }
    mv "$dir/$path" "$dir/$t"; rm -f "$dl"
  fi
  chmod +x "$dir/$t"
done
echo "$dir" >> "${GITHUB_PATH:-/dev/null}"
for t in "${missing[@]}"; do "$dir/$t" --version >/dev/null || { echo "bench tools: $t does not run" >&2; exit 1; }; done
echo "fetched into $dir: ${missing[*]}"
