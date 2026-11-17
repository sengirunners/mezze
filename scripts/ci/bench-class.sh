# Sourced by the bench scripts. class_of <labels> (comma-separated, then an optional " <runner>"): the verdict class
# of a bench job's runs-on labels, or empty (judges nothing: Factory, a Mac). sengi-* is sengi (ceilings.json);
# zm-bench[-*] is zm-bench (a dedicated runner); a zm pool box, "<pool label>:<size>" (served by zm's runner broker),
# is box-<size>; a class's own name is its class. The same rule as scripts/ceilings ClassOf.
class_of() {
  local l s=${1#"${1%%[![:space:]]*}"}; local IFS=,
  for l in ${s%% *}; do
    case "$l" in
    *:*) [[ "$l" == ?*:* && "${l#*:}" =~ ^[a-z0-9][a-z0-9-]*$ ]] && { echo "box-${l#*:}"; return; } ;; # a pool box
    sengi-*) echo sengi; return ;;
    zm-bench | zm-bench-*) echo zm-bench; return ;;
    box-*) [[ "${l#box-}" =~ ^[a-z0-9][a-z0-9-]*$ ]] && { echo "$l"; return; } ;;
    esac
  done
}
# dedicated <labels>: a runner that takes one job at a time and gets only benches (zm-bench, a zm pool box): nothing
# times beside the bench there, so the quiet-pool wait is skipped.
dedicated() { case "$(class_of "$1")" in zm-bench | box-*) return 0 ;; esac; return 1; }
