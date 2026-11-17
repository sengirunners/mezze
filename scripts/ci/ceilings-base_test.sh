#!/usr/bin/env bash
# Real local Git snapshots, fake comparator: no compiler, credentials or external network.
# The comparator's own Go tests cover raises/removals; here we test its input and status.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0 GIT_ALLOW_PROTOCOL=file
export GIT_AUTHOR_NAME=fixture GIT_AUTHOR_EMAIL=fixture@example.invalid
export GIT_COMMITTER_NAME=fixture GIT_COMMITTER_EMAIL=fixture@example.invalid
mkdir "$fixture/bin" "$fixture/runner temp"
git init -q -b main "$fixture/origin"
mkdir -p "$fixture/origin/scripts/ceilings"
printf '%s\n' '{"bench":{"existing":{"p95_ms":15}}}' > "$fixture/origin/scripts/ceilings/ceilings.json"
git -C "$fixture/origin" add scripts/ceilings/ceilings.json
git -C "$fixture/origin" commit -qm original
base=$(git -C "$fixture/origin" rev-parse HEAD)
git clone -q --depth=1 "file://$fixture/origin" "$fixture/candidate"
cp "$fixture/candidate/scripts/ceilings/ceilings.json" "$fixture/expected.json"

# Simulate main gaining a ceiling after the PR's merge checkout was tested.
printf '%s\n' '{"bench":{"existing":{"p95_ms":15},"arrived-later":{"p95_ms":16}}}' > "$fixture/origin/scripts/ceilings/ceilings.json"
git -C "$fixture/origin" commit -qam 'main advanced during tests'
test "$(git -C "$fixture/origin" rev-parse HEAD)" != "$base"
cat > "$fixture/bin/go" <<'FAKE'
#!/usr/bin/env bash
set -euo pipefail
echo "called $*" >> "$CEILINGS_FIXTURE/calls"
test "$1" = run && test "$2" = ./scripts/bench && test "$3" = tighten
if [ "$#" = 6 ]; then # a runner class's file
  test "$4" = -class && test "$5" = zm-bench && test "$6" = "$RUNNER_TEMP/base-ceilings.zm-bench.json"
  cmp "$CEILINGS_FIXTURE/expected-class.json" "$6"
  exit "${CLASS_CHECKER_EXIT:-0}"
fi
test "$#" = 4
test "$4" = "$RUNNER_TEMP/base-ceilings.json"
cmp "$CEILINGS_FIXTURE/expected.json" "$4"
exit "${CHECKER_EXIT:-0}"
FAKE
chmod +x "$fixture/bin/go"

scenario() {
  local name=$1 sha=$2 expected=$3 called=$4 checker_exit=${5:-0} status
  rm -f "$fixture/calls"
  if (cd "$fixture/candidate" &&
    PATH="$fixture/bin:$PATH" CEILINGS_FIXTURE="$fixture" CHECKER_EXIT="$checker_exit" CLASS_CHECKER_EXIT="${CLASS_CHECKER_EXIT:-0}" \
    PR_BASE_SHA="$sha" RUNNER_TEMP="$fixture/runner temp" "$here/ceilings-base.sh") > "$fixture/output" 2>&1; then
    status=0
  else
    status=$?
  fi
  if { [ "$expected" = failure ] && [ "$status" = 0 ]; } ||
    { [ "$expected" != failure ] && [ "$status" != "$expected" ]; } ||
    { [ "$called" = yes ] && [ ! -f "$fixture/calls" ]; } ||
    { [ "$called" = no ] && [ -f "$fixture/calls" ]; }; then
    echo "FAIL $name: exit $status, expected $expected; comparator expected $called"
    cat "$fixture/output"
    exit 1
  fi
  echo "ok $name"
}

scenario event-base-survives-later-main "$base" 0 yes
[ "$(wc -l < "$fixture/calls" | tr -d ' ')" = 1 ] || { echo "FAIL no class file: checked a class"; exit 1; }
scenario checker-rejection-propagates "$base" 37 yes 37
scenario empty-base '' failure no
scenario branch-name-refused main failure no
scenario option-refused --all failure no
scenario short-sha-refused "${base:0:39}" failure no
scenario long-sha-refused "${base}0" failure no
scenario uppercase-sha-refused AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA failure no
scenario newline-refused "$base"$'\n' failure no
scenario missing-commit 0000000000000000000000000000000000000000 failure no
git -C "$fixture/candidate" remote set-url origin "$fixture/absent-origin"
scenario fetch-error-fails-closed "$base" failure no
git -C "$fixture/candidate" remote set-url origin "file://$fixture/origin"
# A runner class's file on the base is checked against its own base copy too, and its rejection fails.
printf '%s\n' '{"bench":{"version":{"p95_ms":20}}}' > "$fixture/origin/scripts/ceilings/ceilings.zm-bench.json"
printf '%s\n' '{"bench":{"version":{"p95_ms":20}}}' > "$fixture/expected-class.json"
git -C "$fixture/origin" add scripts/ceilings/ceilings.zm-bench.json
git -C "$fixture/origin" commit -qm 'zm-bench calibrated'
cp "$fixture/origin/scripts/ceilings/ceilings.json" "$fixture/expected.json"
classed=$(git -C "$fixture/origin" rev-parse HEAD)
scenario class-file-checked "$classed" 0 yes
grep -q "^called run ./scripts/bench tighten -class zm-bench " "$fixture/calls" || { echo "FAIL class-file-checked: not checked"; cat "$fixture/calls"; exit 1; }
CLASS_CHECKER_EXIT=1 scenario class-rejection-propagates "$classed" failure yes
git -C "$fixture/origin" rm -q scripts/ceilings/ceilings.json
git -C "$fixture/origin" commit -qm 'base without ceiling file'
missing=$(git -C "$fixture/origin" rev-parse HEAD)
scenario missing-baseline-fails-closed "$missing" failure no
echo "all ceiling base snapshot checks passed"
