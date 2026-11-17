#!/usr/bin/env bash
# Compare against the immutable base of the tested PR event, never a later branch tip.
set -euo pipefail
if [[ ! "${PR_BASE_SHA:-}" =~ ^[0-9a-f]{40}$ ]]; then
  echo "ceilings: PR_BASE_SHA must be a lowercase 40-hex commit SHA" >&2
  exit 1
fi
: "${RUNNER_TEMP:?ceilings: RUNNER_TEMP is required}"
git fetch --no-tags --depth=1 origin "$PR_BASE_SHA"
git show "$PR_BASE_SHA:scripts/ceilings/ceilings.json" > "$RUNNER_TEMP/base-ceilings.json"
go run ./scripts/bench tighten "$RUNNER_TEMP/base-ceilings.json"
# A runner class's file (ceilings.<class>.json) on the base only tightens too; a new one has nothing to give away.
git ls-tree --name-only "$PR_BASE_SHA" scripts/ceilings/ | while read -r f; do
  [[ "$f" =~ ^scripts/ceilings/ceilings\.([a-z0-9][a-z0-9-]*)\.json$ ]] || continue
  class=${BASH_REMATCH[1]}
  git show "$PR_BASE_SHA:$f" > "$RUNNER_TEMP/base-ceilings.$class.json"
  go run ./scripts/bench tighten -class "$class" "$RUNNER_TEMP/base-ceilings.$class.json"
done
