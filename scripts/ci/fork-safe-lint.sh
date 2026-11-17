#!/usr/bin/env bash
# Fork-safe CI lint: in a public repository a fork's pull request must never reach a self-hosted
# runner (the project's own machines) and never run with secrets or a write token. Fails when:
#   - a fork-reachable job has a runs-on that is not a GitHub-hosted label or the fork guard:
#     ${{ <GUARD> && '<hosted label>' || <today's label> }}, one expression. Fork-reachable: every trigger but push, schedule,
#     workflow_dispatch, workflow_call and workflow_run (its own rule below), and every local workflow such a
#     workflow calls. The guard only reads a pull_request payload, so a workflow with any other such trigger
#     (merge_group, issue_comment, ...) needs a hosted label outright;
#   - a fork-reachable job calls a remote reusable workflow (its runners cannot be checked here);
#   - pull_request_target appears in a workflow not in ALLOW_PULL_REQUEST_TARGET (none today);
#   - a workflow_run job's if can be true when an untrusted event (anything but push, schedule, workflow_dispatch,
#     workflow_run) triggered the run, whatever its other terms say (event names are read exactly, lowercase); or a
#     line uses the triggering run's head (head_sha, head_commit, pull_requests, a checkout ref or repository);
#   - a workflow has no top-level permissions, or grants a write scope there (writes go on the job that needs them).
# Every job in a PR-triggered workflow must carry the guard, even one whose if skips PRs today: an if can change.
# This reads the workflows' deliberately simple layout (block style, two-space steps of indent, one-line runs-on)
# and fails closed on any other; actionlint/GitHub remain the YAML validators. It checks the workflows in this tree:
# a fork's PR can rewrite them, so fork approval and runner-group access are the real fence (runner-route.yml). Usage: fork-safe-lint.sh [workflows dir]
set -euo pipefail
dir=${1:-$(cd "$(dirname "$0")/../.." && pwd)/.github/workflows}
exec python3 - "$dir" <<'PYTHON'
import re
import sys
from pathlib import Path

# Head repo is not this repository: a fork, or no head repo at all (a deleted fork), which counts as a fork.
GUARD = "github.event.pull_request && github.event.pull_request.head.repo.full_name != github.repository"
# The standard GitHub-hosted labels, exactly (docs.github.com/en/actions/reference/runners/github-hosted-runners,
# 2026-11-07); a self-hosted runner may carry any other name, ubuntu-* included. Larger runners are org-defined: not here.
HOSTED_LABELS = [
    "ubuntu-slim", "ubuntu-latest", "ubuntu-26.04", "ubuntu-24.04", "ubuntu-22.04",
    "ubuntu-26.04-arm", "ubuntu-24.04-arm", "ubuntu-22.04-arm",
    "windows-latest", "windows-2025", "windows-2025-vs2026", "windows-2022", "windows-11-arm", "windows-11-vs2026-arm",
    "macos-latest", "macos-26", "macos-15", "macos-26-intel", "macos-15-intel", "xcode-27",
]
HOSTED = "(?:" + "|".join(re.escape(l) for l in HOSTED_LABELS) + ")"
# file name -> the reviewed reason it may use pull_request_target. Never check out PR code in such a workflow.
ALLOW_PULL_REQUEST_TARGET = {}
# Only people with write access (or this repository's own runs) can start these.
TRUSTED_EVENTS = {"push", "schedule", "workflow_dispatch", "workflow_call", "workflow_run"}
# Events whose payload has github.event.pull_request, so the guard can tell a fork's from this repository's.
PR_PAYLOAD_EVENTS = {"pull_request", "pull_request_target", "pull_request_review", "pull_request_review_comment"}
# Events that only people with write access (or this repository's own runs) start. A workflow_run job's if must be
# false whenever any other event triggered the run.
WR_TRUSTED = {"push", "schedule", "workflow_dispatch", "workflow_run"}
WR_EV = r"github\.event\.workflow_run\.event"
UNSAFE = object()


def wr_tokens(cond):
    """An if's boolean structure: operators, parentheses and atoms (comparisons and calls, read whole)."""
    cond = re.sub(r"^[>|][+-]?\s", "", cond.strip())  # a folded or literal block scalar's indicator
    if cond.startswith("${{") and cond.endswith("}}"):
        cond = cond[3:-2].strip()
    if "${{" in cond or "}}" in cond:
        # Text around ${{ }} makes GitHub read the if as a template, a non-empty string: always true.
        raise ValueError("${{ }} inside the expression")
    out, i, n = [], 0, len(cond)
    while i < n:
        c = cond[i]
        if c.isspace():
            i += 1
        elif cond.startswith(("&&", "||"), i):
            out.append(cond[i:i + 2]); i += 2
        elif c == "!" and not cond.startswith("!=", i):
            out.append("!"); i += 1
        elif c in "()":
            out.append(c); i += 1
        else:
            j, depth, quote = i, 0, None
            while j < n:
                ch = cond[j]
                if quote:
                    if ch == quote:
                        quote = None
                elif ch in "'\"":
                    quote = ch
                elif ch == "(":
                    depth += 1
                elif ch == ")":
                    if depth == 0:
                        break
                    depth -= 1
                elif depth == 0 and cond.startswith(("&&", "||"), j):
                    break
                j += 1
            if quote or depth:
                raise ValueError("unbalanced quotes or parentheses")
            out.append(" ".join(cond[i:j].split())); i = j
    return out


def wr_atom(atom):
    """The atom's value when an untrusted event triggered the run: True, False, None (unknown: either), or UNSAFE
    (a test of the triggering event this lint does not read as excluding every untrusted one). Names compare
    exactly: GitHub ignores case, so 'PUSH' is not read as push."""
    m = re.fullmatch(WR_EV + r" (==|!=) '([\w-]+)'", atom)
    if m:
        if m.group(2) not in WR_TRUSTED:
            return UNSAFE
        return m.group(1) == "!="
    m = re.fullmatch(r"contains\(fromJSON\('\[([^\]']*)\]'\), " + WR_EV + r"\)", atom)
    if m:
        names = [v.strip() for v in m.group(1).split(",")]
        return False if all(n[:1] == n[-1:] == '"' and n[1:-1] in WR_TRUSTED for n in names) else UNSAFE
    if re.search(r"workflow_run\s*(?:\.\s*event\b|\[)", atom):
        return UNSAFE
    m = re.fullmatch(r"github\.event_name (==|!=) 'workflow_run'", atom)
    if m:
        return m.group(1) == "=="
    return None


def wr_can_run(cond):
    """Whether the if can be true for a run an untrusted event triggered, whatever its other terms (status
    functions, conclusions, branches) say. Anything unreadable counts as can run."""
    try:
        toks = wr_tokens(cond)
    except ValueError:
        return True
    atoms = [t for t in toks if t not in ("&&", "||", "!", "(", ")")]
    if not toks or any(wr_atom(t) is UNSAFE for t in atoms):
        return True
    free = sorted({t for t in atoms if wr_atom(t) is None})
    if len(free) > 12:
        return True
    for bits in range(1 << len(free)):
        vals = {a: bool(bits >> k & 1) for k, a in enumerate(free)}
        pos = 0

        def expr():
            nonlocal pos
            v = term()
            while pos < len(toks) and toks[pos] == "||":
                pos += 1
                v = term() or v
            return v

        def term():
            nonlocal pos
            v = factor()
            while pos < len(toks) and toks[pos] == "&&":
                pos += 1
                v = factor() and v
            return v

        def factor():
            nonlocal pos
            if pos >= len(toks):
                raise ValueError("ends early")
            t = toks[pos]; pos += 1
            if t == "!":
                # GitHub's ! binds tighter than == and !=: !a == b is (!a) == b, not read here.
                if pos < len(toks) and re.search(r"[=!]=", toks[pos]):
                    raise ValueError("! before a comparison")
                return not factor()
            if t == "(":
                v = expr()
                if pos >= len(toks) or toks[pos] != ")":
                    raise ValueError("no )")
                pos += 1
                return v
            if t in ("&&", "||", ")"):
                raise ValueError("operator out of place")
            known = wr_atom(t)
            return vals[t] if known is None else known

        try:
            v = expr()
            if pos != len(toks):
                return True
        except (ValueError, RecursionError):
            return True
        if v:
            return True
    return False


root = Path(sys.argv[1])
errors = []


def uncomment(line):
    if line.lstrip().startswith("#"):
        return ""
    return re.sub(r"(^|\s)#.*$", "", line).rstrip()


def unquote(v):
    v = v.strip()
    if len(v) >= 2 and v[0] == v[-1] and v[0] in "'\"":
        return v[1:-1]
    return v


def blocks(lines, indent, where=None):
    """Keys at exactly this indent -> (inline value, the lines under it). With where, any line that is neither such
    a key nor under one (another indent, a list item, flow style) is an error: the layout is not the one read here."""
    out, key = {}, None
    pad = " " * indent
    for line in lines:
        if not line.strip():
            if key:
                out[key][1].append(line)
            continue
        m = re.match(r"^" + pad + r"""(["']?[A-Za-z_][\w-]*["']?):(?:\s+(.*))?$""", line)
        if m and not line[indent].isspace():
            key = unquote(m.group(1))
            out[key] = ((m.group(2) or "").strip(), [])
        elif key and line.startswith(pad + " "):
            out[key][1].append(line)
        else:
            key = None
            if where:
                errors.append(f"{where}: not the layout this lint reads (keys at {indent} spaces): {line.strip()}")
    return out


def value(entry):
    """A key's whole value on one line: inline text plus any folded continuation lines."""
    inline, rest = entry
    return " ".join([inline] + [l.strip() for l in rest if l.strip()]).strip()


files = sorted(p for p in root.iterdir() if p.suffix in (".yml", ".yaml"))
parsed = {}
for path in files:
    raw = path.read_text().splitlines()
    lines = [uncomment(l) for l in raw]
    name = path.name
    if any(re.match(r"^\s*<<:", l) or re.match(r"^\s*[\w-]+:\s+[&*][A-Za-z]", l) for l in lines):
        errors.append(f"{name}: YAML anchors or merge keys: this lint cannot follow them")
    top = blocks(lines, 0)
    if "true" in top and "on" not in top:  # YAML 1.1 reads a bare on as true
        top["on"] = top.pop("true")
    if "on" not in top:
        errors.append(f"{name}: no on: block")
        continue
    inline, rest = top["on"]
    body = [l for l in rest if l.strip()]
    if inline:
        listed = re.fullmatch(r"\[\s*([\w-]+(?:\s*,\s*[\w-]+)*)\s*\]|([\w-]+)", inline)
        triggers = set(re.split(r"\s*,\s*", (listed.group(1) or listed.group(2)).strip())) if listed else set()
        if not listed or body:
            errors.append(f"{name}: on: {inline}: not a trigger name or [list] (flow mappings are not read here)")
    elif body and body[0].startswith("  - "):
        triggers = set()
        for l in body:
            m = re.fullmatch(r"  - ([\w-]+)", l)
            if m:
                triggers.add(m.group(1))
            else:
                errors.append(f"{name}: on: list item not read here: {l.strip()}")
    else:
        triggers = set(blocks(rest, 2, f"{name}: on"))
    if not triggers:
        errors.append(f"{name}: on: no triggers read")
    if any("pull_request_target" in l for l in lines) and name not in ALLOW_PULL_REQUEST_TARGET:
        errors.append(f"{name}: pull_request_target without an ALLOW_PULL_REQUEST_TARGET entry (fork code would run with secrets and a write token)")
    # Least privilege: the workflow default is read-only.
    if "permissions" not in top:
        errors.append(f"{name}: no top-level permissions (set read-only, or {{}}, and grant writes per job)")
    else:
        inline, rest = top["permissions"]
        if inline in ("{}", "read-all"):
            pass
        elif inline == "write-all":
            errors.append(f"{name}: top-level permissions write-all")
        else:
            pairs = re.findall(r"([\w-]+)\s*:\s*([\w-]+)", inline) if inline else [(k, v[0]) for k, v in blocks(rest, 2).items()]
            if not pairs:
                errors.append(f"{name}: top-level permissions not understood")
            for scope, level in pairs:
                if level not in ("read", "none"):
                    errors.append(f"{name}: top-level permissions grant {scope}: {level} (grant it on the job that needs it)")
    jobs = {}
    if "jobs" not in top:
        errors.append(f"{name}: no jobs: block")
    elif top["jobs"][0]:
        errors.append(f"{name}: jobs: {top['jobs'][0]}: flow style is not read here")
    else:
        for job, entry in blocks(top["jobs"][1], 2, f"{name}: jobs").items():
            if entry[0]:
                errors.append(f"{name}: job {job}: {entry[0]}: flow style is not read here")
            jobs[job] = blocks(entry[1], 4, f"{name}: job {job}")
            if not jobs[job]:
                errors.append(f"{name}: job {job}: no keys read")
        if not jobs:
            errors.append(f"{name}: jobs: no jobs read")
    parsed[name] = (triggers, jobs, lines)

# Fork-reachable: workflows with any untrusted trigger, and every local workflow they call (with the caller's events).
reachable = {n: t - TRUSTED_EVENTS for n, (t, _, _) in parsed.items() if t - TRUSTED_EVENTS}
todo = list(reachable)
while todo:
    caller = todo.pop()
    for job, keys in parsed[caller][1].items():
        if "uses" not in keys:
            continue
        uses = unquote(keys["uses"][0])
        m = re.fullmatch(r"\./\.github/workflows/([\w.-]+)", uses)
        if not m:
            errors.append(f"{caller}: job {job} calls {uses}, a reusable workflow this lint cannot check, on a fork's PR")
        elif m.group(1) not in parsed:
            errors.append(f"{caller}: job {job} calls {uses}, which is missing")
        elif not reachable[caller] <= reachable.get(m.group(1), set()):
            reachable[m.group(1)] = reachable.get(m.group(1), set()) | reachable[caller]
            todo.append(m.group(1))

hosted = re.compile(HOSTED)
# One expression, the whole value: a second ${{ }} after it would extend the hosted label for a fork.
guarded = re.compile(r"\$\{\{ " + re.escape(GUARD) + r" && '" + HOSTED + r"' \|\| \S(?:(?!\}\}|\$\{\{).)* \}\}")
checked = 0
for name in sorted(reachable):
    unguardable = sorted(reachable[name] - PR_PAYLOAD_EVENTS)
    for job, keys in parsed[name][1].items():
        if "uses" in keys:
            continue
        if "runs-on" not in keys:
            errors.append(f"{name}: job {job} has no runs-on")
            continue
        inline, rest = keys["runs-on"]
        if not inline or any(l.strip() for l in rest) or inline[0] in "[{":
            errors.append(f"{name}: job {job}: runs-on must be one line (a hosted label or the fork guard), not a list or group")
            continue
        label = unquote(inline)
        checked += 1
        if hosted.fullmatch(label):
            continue
        if unguardable:
            errors.append(f"{name}: job {job} is fork-reachable through {', '.join(unguardable)}, which the fork guard "
                          f"cannot read: runs-on: {inline}; use a GitHub-hosted label")
        elif not guarded.fullmatch(label):
            errors.append(f"{name}: job {job} is reachable from a fork's pull_request and can run on a self-hosted label: "
                          f"runs-on: {inline}; use a GitHub-hosted label or ${{{{ {GUARD} && 'ubuntu-latest' || ... }}}}")

# workflow_run runs in this repository's context, with secrets and a write token, whatever triggered it.
for name, (triggers, jobs, lines) in sorted(parsed.items()):
    if "workflow_run" not in triggers:
        continue
    for job, keys in jobs.items():
        cond = value(keys["if"]) if "if" in keys else ""
        if wr_can_run(cond):
            errors.append(f"{name}: job {job} runs on workflow_run without ruling out PR runs in its if "
                          f"(the triggering event must be one of {', '.join(sorted(WR_TRUSTED))}, e.g. "
                          "github.event.workflow_run.event == 'push' as an && term)")
    for l in lines:
        # The triggering run's code: its head commit, its pull requests, or a checkout of its repository or ref.
        # Defense in depth: the if rule above already keeps PR-started runs out.
        if re.search(r"workflow_run\s*\[|workflow_run\.(?:head_sha|head_commit|pull_requests)", l) or re.match(
                r"^\s*(?:ref|repository):.*workflow_run", l):
            errors.append(f"{name}: uses the triggering run's head ({l.strip()})")

for e in errors:
    print("fork-safe: " + e)
if errors:
    sys.exit(1)
print(f"ok   fork-safe: {len(parsed)} workflows, {checked} fork-reachable jobs on hosted or guarded runners")
PYTHON
