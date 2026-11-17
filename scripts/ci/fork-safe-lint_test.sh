#!/usr/bin/env bash
# fork-safe-lint.sh against this repository's workflows (must pass) and small fixtures (each must fail, or pass,
# for its one reason). Run by ci.yml's fast check.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
lint="$here/fork-safe-lint.sh"
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
# The fork guard, as the lint defines it (its one home).
G=$(sed -n 's/^GUARD = "\(.*\)"$/\1/p' "$lint"); [ -n "$G" ] || { echo "FAIL: no GUARD in $lint"; exit 1; }

"$lint" "$here/../../.github/workflows"

# case NAME pass|fail EXPECTED-OUTPUT, with the fixture's workflow files in $T/NAME (written before the call).
case_() {
  local name=$1 want=$2 expect=$3 out status=0
  out=$("${LINT:-$lint}" "$T/$name" 2>&1) || status=$?
  if [ "$want" = pass ] && [ "$status" != 0 ]; then printf 'FAIL %s: want pass, got\n%s\n' "$name" "$out"; exit 1; fi
  if [ "$want" = fail ] && [ "$status" = 0 ]; then printf 'FAIL %s: want fail, got\n%s\n' "$name" "$out"; exit 1; fi
  grep -qF -- "$expect" <<<"$out" || { printf 'FAIL %s: want %q in\n%s\n' "$name" "$expect" "$out"; exit 1; }
  echo "ok   $name${LINT:+ (allow-listed)}"
}
# wf NAME FILE: the workflow on stdin.
wf() { mkdir -p "$T/$1"; cat > "$T/$1/$2"; }
pr_job() { # a PR workflow whose one job runs on $1
  printf 'name: x\non:\n  pull_request:\n  push:\n    branches: [main]\npermissions:\n  contents: read\njobs:\n  build:\n    runs-on: %s\n    steps:\n      - run: true\n' "$1"
}

pr_job "\${{ $G && 'ubuntu-latest' || needs.route.outputs.label || 'example-standard-4' }}" | wf guarded ci.yml
case_ guarded pass 'ok   fork-safe: 1 workflows, 1 fork-reachable jobs'
# A second expression after the guard would extend the hosted label (ubuntu-latest-factory) for a fork.
pr_job "\${{ $G && 'ubuntu-latest' || 'x' }}\${{ '-factory' }}" | wf guard-suffix ci.yml
case_ guard-suffix fail 'can run on a self-hosted label'
pr_job "\${{ $G && 'ubuntu-latest' || 'x' }} \${{ '-factory' }}" | wf guard-spaced-suffix ci.yml
case_ guard-spaced-suffix fail 'can run on a self-hosted label'
pr_job "ubuntu-24.04" | wf hosted-literal ci.yml
case_ hosted-literal pass 'ok   fork-safe'
pr_job "'ubuntu-latest'" | wf hosted-quoted ci.yml
case_ hosted-quoted pass 'ok   fork-safe'
pr_job "example-standard-4" | wf self-hosted-literal ci.yml
case_ self-hosted-literal fail 'ci.yml: job build is reachable from a fork'
pr_job "self-hosted" | wf self-hosted-label ci.yml
case_ self-hosted-label fail 'job build is reachable from a fork'
pr_job "\${{ needs.route.outputs.label || 'example-standard-4' }}" | wf unguarded-expression ci.yml
case_ unguarded-expression fail 'job build is reachable from a fork'
pr_job "\${{ vars.CI_RUNNER || 'ubuntu-latest' }}" | wf variable-first ci.yml
case_ variable-first fail 'job build is reachable from a fork'
pr_job "\${{ $G && 'example-standard-4' || 'ubuntu-latest' }}" | wf guard-picks-self-hosted ci.yml
case_ guard-picks-self-hosted fail 'job build is reachable from a fork'
pr_job "\${{ github.event.pull_request.head.repo.fork && 'ubuntu-latest' || 'example-standard-4' }}" | wf weaker-guard ci.yml
case_ weaker-guard fail 'job build is reachable from a fork'
pr_job "\${{ github.event.pull_request.head.repo.full_name != github.repository && 'ubuntu-latest' || 'acme-runner-4' }}" | wf guard-without-event ci.yml
case_ guard-without-event fail 'job build is reachable from a fork'
pr_job "[self-hosted, linux]" | wf list-runs-on ci.yml
case_ list-runs-on fail 'runs-on must be one line'
printf 'on: pull_request\npermissions: {}\njobs:\n  build:\n    runs-on:\n      group: factory\n    steps:\n      - run: true\n' | wf group-runs-on ci.yml
case_ group-runs-on fail 'runs-on must be one line'
printf 'on: [push, pull_request]\npermissions: read-all\njobs:\n  build:\n    runs-on: example-standard-4\n    steps:\n      - run: true\n' | wf inline-triggers ci.yml
case_ inline-triggers fail 'job build is reachable from a fork'
printf 'on:\n  - push\n  - pull_request\npermissions: {}\njobs:\n  build:\n    runs-on: example-standard-4\n    steps:\n      - run: true\n' | wf list-triggers ci.yml
case_ list-triggers fail 'job build is reachable from a fork'
printf 'on:\n  pull_request:\npermissions: {}\njobs:\n  build:\n    steps:\n      - run: true\n' | wf no-runs-on ci.yml
case_ no-runs-on fail 'job build has no runs-on'
# A job that skips PRs today still needs the guard: its if can change.
printf 'on:\n  pull_request:\n  push:\npermissions: {}\njobs:\n  full:\n    if: github.event_name != '"'pull_request'"'\n    runs-on: example-standard-4\n    steps:\n      - run: true\n' | wf if-is-not-enough ci.yml
case_ if-is-not-enough fail 'job full is reachable from a fork'
# Self-hosted labels outside PR-triggered workflows are today's routing, untouched.
printf 'on:\n  push:\n    branches: [main]\n  schedule:\n    - cron: "1 1 * * *"\n  workflow_dispatch:\npermissions:\n  contents: read\njobs:\n  bench:\n    permissions:\n      contents: write\n    runs-on: example-standard-4\n    steps:\n      - run: true\n' | wf trusted-only bench.yml
case_ trusted-only pass 'ok   fork-safe: 1 workflows, 0 fork-reachable jobs'
# Reusable workflows: a local callee is checked as fork-reachable; a remote one cannot be.
printf 'on:\n  pull_request:\npermissions: {}\njobs:\n  route:\n    uses: ./.github/workflows/route.yml\n' | wf local-callee ci.yml
printf 'on:\n  workflow_call:\npermissions: {}\njobs:\n  pick:\n    runs-on: ${{ vars.INTERNAL_CONTROL_RUNNER || '"'acme-runner-4'"' }}\n    steps:\n      - run: true\n' | wf local-callee route.yml
case_ local-callee fail 'route.yml: job pick is reachable from a fork'
printf 'on:\n  workflow_call:\npermissions: {}\njobs:\n  pick:\n    runs-on: acme-runner-4\n    steps:\n      - run: true\n' | wf callee-of-trusted route.yml
printf 'on:\n  push:\npermissions: {}\njobs:\n  route:\n    uses: ./.github/workflows/route.yml\n' | wf callee-of-trusted ci.yml
case_ callee-of-trusted pass 'ok   fork-safe: 2 workflows, 0 fork-reachable jobs'
printf 'on:\n  pull_request:\npermissions: {}\njobs:\n  route:\n    uses: other/repo/.github/workflows/route.yml@main\n' | wf remote-callee ci.yml
case_ remote-callee fail 'a reusable workflow this lint cannot check'
printf 'on:\n  pull_request:\npermissions: {}\njobs:\n  route:\n    uses: ./.github/workflows/gone.yml\n' | wf missing-callee ci.yml
case_ missing-callee fail 'which is missing'
# pull_request_target: only with an allow-list entry.
printf 'on:\n  pull_request_target:\npermissions: {}\njobs:\n  label:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n' | wf prt label.yml
case_ prt fail 'label.yml: pull_request_target without an ALLOW_PULL_REQUEST_TARGET entry'
printf 'on: [push, pull_request_target]\npermissions: {}\njobs:\n  label:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n' | wf prt-inline label.yml
case_ prt-inline fail 'pull_request_target without an ALLOW_PULL_REQUEST_TARGET entry'
sed 's/^ALLOW_PULL_REQUEST_TARGET = {}$/ALLOW_PULL_REQUEST_TARGET = {"label.yml": "test"}/' "$lint" > "$T/allowing-lint.sh"
grep -q '"label.yml": "test"' "$T/allowing-lint.sh"; chmod +x "$T/allowing-lint.sh"
LINT="$T/allowing-lint.sh" case_ prt pass 'ok   fork-safe'
# workflow_run: secrets and a write token whatever triggered it.
printf 'on:\n  workflow_run:\n    workflows: [ci]\n    types: [completed]\npermissions: {}\njobs:\n  report:\n    runs-on: acme-runner-4\n    steps:\n      - run: true\n' | wf wr-no-if report.yml
case_ wr-no-if fail 'report.yml: job report runs on workflow_run without ruling out PR runs'
printf 'on:\n  workflow_run:\n    workflows: [ci]\n    types: [completed]\npermissions: {}\njobs:\n  report:\n    if: github.event.workflow_run.conclusion == '"'success'"'\n    runs-on: acme-runner-4\n    steps:\n      - run: true\n' | wf wr-weak-if report.yml
case_ wr-weak-if fail 'without ruling out PR runs'
printf 'on:\n  workflow_run:\n    workflows: [ci]\n    types: [completed]\npermissions: {}\njobs:\n  report:\n    if: github.event.workflow_run.event != '"'pull_request'"'\n    runs-on: acme-runner-4\n    steps:\n      - run: true\n' | wf wr-guarded report.yml
case_ wr-guarded fail 'without ruling out PR runs'  # merge_group, issue_comment ... still get through
wr_if() { # NAME IF: a workflow_run job on the control pool with that if
  printf 'on:\n  workflow_run:\n    workflows: [ci]\n    types: [completed]\npermissions: {}\njobs:\n  report:\n    if: %s\n    runs-on: acme-runner-4\n    steps:\n      - run: true\n' "$2" | wf "$1" report.yml
}
wr_if wr-push "github.event.workflow_run.event == 'push'"; case_ wr-push pass 'ok   fork-safe'
wr_if wr-and "!cancelled() && github.event.workflow_run.event == 'push' && github.event.workflow_run.conclusion == 'success'"
case_ wr-and pass 'ok   fork-safe'
wr_if wr-allow-list "contains(fromJSON('[\"push\",\"schedule\"]'), github.event.workflow_run.event)"; case_ wr-allow-list pass 'ok   fork-safe'
wr_if wr-allow-list-pr "contains(fromJSON('[\"push\",\"pull_request\"]'), github.event.workflow_run.event)"
case_ wr-allow-list-pr fail 'without ruling out PR runs'
# The event test must decide the if: text that merely mentions it does not.
wr_if wr-or-bypass "always() || github.event.workflow_run.event == 'push'"; case_ wr-or-bypass fail 'without ruling out PR runs'
wr_if wr-negated "!(github.event.workflow_run.event == 'push')"; case_ wr-negated fail 'without ruling out PR runs'
wr_if wr-unbalanced "(github.event.workflow_run.event == 'push'"; case_ wr-unbalanced fail 'without ruling out PR runs'
# Only trusted names, read exactly: GitHub compares case-insensitively, and any untrusted name may be the trigger.
wr_if wr-other-name "github.event.workflow_run.event == 'pull_request_review_comment'"; case_ wr-other-name fail 'without ruling out PR runs'
wr_if wr-upper "github.event.workflow_run.event == 'PULL_REQUEST'"; case_ wr-upper fail 'without ruling out PR runs'
wr_if wr-upper-push "github.event.workflow_run.event == 'PUSH'"; case_ wr-upper-push fail 'without ruling out PR runs'
wr_if wr-list-other "contains(fromJSON('[\"push\",\"check_suite\"]'), github.event.workflow_run.event)"; case_ wr-list-other fail 'without ruling out PR runs'
wr_if wr-event-name-upper "github.event_name == 'WORKFLOW_RUN' && github.event.workflow_run.event == 'push'"; case_ wr-event-name-upper pass 'ok   fork-safe'
wr_if wr-event-name-or "github.event_name == 'WORKFLOW_RUN' || github.event.workflow_run.event == 'push'"; case_ wr-event-name-or fail 'without ruling out PR runs'
# ! binds tighter than != in GitHub: (!event) != 'push' is always true.
wr_if wr-not-binds "\${{ !github.event.workflow_run.event != 'push' }}"; case_ wr-not-binds fail 'without ruling out PR runs'
wr_if wr-partial-template "github.event.workflow_run.event == 'push' && \${{ false }}"; case_ wr-partial-template fail 'without ruling out PR runs'
wr_if wr-whole-template "\${{ github.event.workflow_run.event == 'push' && always() }}"; case_ wr-whole-template pass 'ok   fork-safe'
wr_if wr-deep "$(printf '(%.0s' $(seq 1000))"; case_ wr-deep fail 'without ruling out PR runs'
for ref in "ref: \${{ github.event.workflow_run['head_sha'] }}" "repository: \${{ github.event.workflow_run.head_repository.full_name }}"; do
  printf 'on:\n  workflow_run:\n    workflows: [ci]\n    types: [completed]\npermissions: {}\njobs:\n  report:\n    if: github.event.workflow_run.event == '"'push'"'\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n        with:\n          %s\n' "$ref" | wf wr-head-ref report.yml
  case_ wr-head-ref fail "uses the triggering run's head"; rm -rf "$T/wr-head-ref"
done
printf 'on:\n  workflow_run:\n    workflows: [ci]\n    types: [completed]\npermissions: {}\njobs:\n  report:\n    if: github.event.workflow_run.event == '"'push'"'\n    runs-on: ubuntu-latest\n    steps:\n      - run: git checkout ${{ github.event.workflow_run.head_sha }}\n' | wf wr-run-head report.yml
case_ wr-run-head fail "uses the triggering run's head"
printf 'on:\n  workflow_run:\n    workflows: [ci]\n    types: [completed]\npermissions: {}\njobs:\n  release:\n    if: >-\n      github.event_name != '"'workflow_run'"' ||\n      github.event.workflow_run.event == '"'push'"'\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n' | wf wr-folded-if release.yml
case_ wr-folded-if pass 'ok   fork-safe'
printf 'on:\n  workflow_run:\n    workflows: [ci]\n    types: [completed]\npermissions: {}\njobs:\n  report:\n    if: github.event.workflow_run.event == '"'push'"'\n    runs-on: ubuntu-latest\n    steps:\n      - uses: actions/checkout@v4\n        with:\n          ref: ${{ github.event.workflow_run.head_sha }}\n' | wf wr-checkout report.yml
case_ wr-checkout fail "uses the triggering run's head"
# Least privilege: read-only at the top, writes per job.
printf 'on:\n  push:\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n' | wf no-permissions a.yml
case_ no-permissions fail 'a.yml: no top-level permissions'
printf 'on:\n  push:\npermissions:\n  contents: read\n  actions: write\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n' | wf top-write a.yml
case_ top-write fail 'top-level permissions grant actions: write'
printf 'on:\n  push:\npermissions: write-all\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n' | wf write-all a.yml
case_ write-all fail 'top-level permissions write-all'
printf 'on:\n  push:\npermissions: { contents: write }\njobs:\n  a:\n    runs-on: ubuntu-latest\n    steps:\n      - run: true\n' | wf flow-write a.yml
case_ flow-write fail 'top-level permissions grant contents: write'
# Shapes this lint cannot follow fail closed.
printf 'on:\n  push:\npermissions: {}\njobs:\n  a: &base\n    runs-on: ubuntu-latest\n  b:\n    <<: *base\n' | wf anchors a.yml
case_ anchors fail 'YAML anchors or merge keys'
printf 'permissions: {}\njobs:\n  a:\n    runs-on: ubuntu-latest\n' | wf no-on a.yml
case_ no-on fail 'a.yml: no on: block'
# Review fixes: layouts the lint does not read fail closed rather than reading as "no jobs".
printf 'on:\n  pull_request:\npermissions: {}\njobs:\n    build:\n        runs-on: acme-runner-4\n' | wf jobs-four-spaces ci.yml
case_ jobs-four-spaces fail 'ci.yml: jobs: not the layout this lint reads'
printf 'on:\n    pull_request:\npermissions: {}\njobs:\n  build:\n    runs-on: acme-runner-4\n' | wf triggers-four-spaces ci.yml
case_ triggers-four-spaces fail 'ci.yml: on: not the layout this lint reads'
printf 'on: { pull_request: {} }\npermissions: {}\njobs:\n  build:\n    runs-on: acme-runner-4\n' | wf flow-on ci.yml
case_ flow-on fail 'flow mappings are not read here'
printf 'on:\n  pull_request:\npermissions: {}\njobs: { build: { runs-on: acme-runner-4 } }\n' | wf flow-jobs ci.yml
case_ flow-jobs fail 'ci.yml: jobs: { build: { runs-on: acme-runner-4 } }: flow style'
printf 'on:\n  pull_request:\npermissions: {}\njobs:\n  build: { runs-on: acme-runner-4 }\n' | wf flow-job ci.yml
case_ flow-job fail 'ci.yml: job build: { runs-on: acme-runner-4 }: flow style'
printf 'on:\npermissions: {}\njobs:\n  build:\n    runs-on: acme-runner-4\n' | wf empty-on ci.yml
case_ empty-on fail 'ci.yml: on: no triggers read'
printf 'on:\n  pull_request:\npermissions: {}\njobs:\n' | wf empty-jobs ci.yml
case_ empty-jobs fail 'ci.yml: jobs: no jobs read'
# Every trigger but push, schedule, workflow_dispatch, workflow_call and workflow_run is fork-reachable.
for ev in pull_request_review pull_request_review_comment merge_group issue_comment issues release discussion; do
  printf 'on:\n  %s:\npermissions: {}\njobs:\n  build:\n    runs-on: acme-runner-4\n' "$ev" | wf "event-$ev" ci.yml
  case_ "event-$ev" fail 'job build is'
done
# The guard reads github.event.pull_request: enough for review events, not for merge_group or issue_comment.
pr_job "\${{ $G && 'ubuntu-latest' || 'acme-runner-4' }}" | sed 's/^  pull_request:$/  pull_request_review:/' | wf guarded-review ci.yml
case_ guarded-review pass 'ok   fork-safe: 1 workflows, 1 fork-reachable jobs'
pr_job "\${{ $G && 'ubuntu-latest' || 'acme-runner-4' }}" | sed 's/^  pull_request:$/  pull_request:\n  merge_group:/' | wf guarded-merge-group ci.yml
case_ guarded-merge-group fail 'fork-reachable through merge_group, which the fork guard cannot read'
pr_job "ubuntu-latest" | sed 's/^  pull_request:$/  issue_comment:/' | wf hosted-issue-comment ci.yml
case_ hosted-issue-comment pass 'ok   fork-safe'
printf 'on:\n  merge_group:\npermissions: {}\njobs:\n  route:\n    uses: ./.github/workflows/route.yml\n' | wf callee-of-merge-group ci.yml
printf 'on:\n  workflow_call:\npermissions: {}\njobs:\n  pick:\n    runs-on: ${{ %s && '"'ubuntu-latest'"' || '"'acme-runner-4'"' }}\n' "$G" | wf callee-of-merge-group route.yml
case_ callee-of-merge-group fail 'route.yml: job pick is fork-reachable through merge_group'
# Exact hosted labels: a self-hosted runner may be named ubuntu-anything.
for l in ubuntu-24.04-factory ubuntu-latest-xl ubuntu-2404 macos-ci windows-gpu; do
  pr_job "$l" | wf "lookalike-$l" ci.yml
  case_ "lookalike-$l" fail 'job build is reachable from a fork'
done
pr_job "\${{ $G && 'ubuntu-24.04-factory' || 'acme-runner-4' }}" | wf guard-lookalike ci.yml
case_ guard-lookalike fail 'job build is reachable from a fork'
for l in ubuntu-24.04-arm windows-2025 macos-15 ubuntu-slim; do
  pr_job "$l" | wf "hosted-$l" ci.yml
  case_ "hosted-$l" pass 'ok   fork-safe'
done
echo 'all passed'
