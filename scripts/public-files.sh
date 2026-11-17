#!/usr/bin/env bash
# Prints the tracked files a public copy keeps: every tracked file except those internal/deps/public_exclude.txt
# leaves out. With a commit argument, the files of that commit by that commit's own list.
# scripts/publish does the same in Go (internal/deps/publictree); TestPublicFilesScriptAgreesWithPublish keeps the
# two equal. Run from anywhere in the checkout. Usage: public-files.sh [commit]
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
ref=${1:-}
if [ -n "$ref" ]; then
  exclude=$(git show "$ref:internal/deps/public_exclude.txt")
  files=$(git ls-tree -r --name-only "$ref")
else
  exclude=$(cat internal/deps/public_exclude.txt)
  files=$(git ls-files)
fi
globs=()
while IFS= read -r line; do
  line=${line%%#*}
  line=$(printf '%s' "$line" | tr -d '[:space:]')
  [ -n "$line" ] && globs+=("$line")
done <<<"$exclude"
while IFS= read -r f; do
  [ -n "$f" ] || continue
  for g in "${globs[@]}"; do
    if [[ "$g" == *'/**' ]]; then
      [[ "$f" == "${g%'/**'}/"* ]] && continue 2
    else
      # shellcheck disable=SC2053 # $g is a glob on purpose
      [[ "$f" == $g ]] && continue 2
    fi
  done
  printf '%s\n' "$f"
done <<<"$files"
