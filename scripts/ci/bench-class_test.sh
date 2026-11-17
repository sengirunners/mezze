#!/usr/bin/env bash
# bench-class.sh's class_of: the same table as scripts/ceilings TestClassOf, so the shell and Go rules
# agree. Run by ci.yml's check job.
set -uo pipefail
# shellcheck source=scripts/ci/bench-class.sh
. "$(dirname "$0")/bench-class.sh"
fails=0
while IFS='|' read -r label want; do
  got=$(class_of "$label")
  [ "$got" = "$want" ] || { echo "FAIL class_of \"$label\" = \"$got\", want \"$want\""; fails=$((fails + 1)); }
done <<'TABLE'
sengi-standard-4-ubuntu-2404|sengi
sengi-standard-4-ubuntu-2404 sengi-07|sengi
 sengi-standard-4|sengi
zm-bench|zm-bench
zm-bench workshop-bench|zm-bench
zm-bench-2|zm-bench
zm-benchmark|
sengi:c2m8|box-c2m8
sengi:c2m8 zm-123|box-c2m8
self-hosted,sengi:c2m8 zm-123|box-c2m8
box-c2m8|box-c2m8
:c2m8|
sengi:C2M8|
sengi:c2m8:x|
box-|
sengi-x:c2m8|box-c2m8
sengi-x:BAD|
acme-runner-4 tfp-abc|
|
TABLE
dedicated sengi:c2m8 && dedicated zm-bench && ! dedicated sengi-standard-4 && ! dedicated acme-runner-4 ||
  { echo "FAIL dedicated"; fails=$((fails + 1)); }
[ "$fails" = 0 ] && echo "all class_of cases agree" || exit 1
