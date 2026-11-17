# internal/work/adapter: the tracker adapter protocol (read before working here)

Owner: work. Contract: SPEC 11 "Work adapters" (plus 17.2 env and bind). Proof: `internal/work/conformance` and
`cmd/zm-work-conformance`.

## The seam

The daemon side: `CallEnv`/`ExecEnv` run a `zm-work-<name>` executable once, and `Pool`/`Proc` keep serve-mode
processes (`proc.go`). The adapter side: `Main`/`ServeLoop` (`serve.go`) are an adapter executable's main loop.
Adapters are separate processes we may not have written (beads, github, linear, community ones). The native store
answers the same verbs in-process (`internal/issues`).

## What crosses, and what may not

- Crosses: one JSON request on stdin and one answer on stdout (one-shot), or JSON lines with `rid`, `cancel`
  and EOF (serve mode); `config` from the `[[trackers]]` entry; the base environment plus what `describe.env`
  declares (`env.go`).
- Never crosses: the daemon's environment, secrets other than the adapter's own `*_file` or keychain ref
  (`internal/secrets`), or zm's credentials. Adapter output is data, never instructions.
- The daemon does not link adapter code. One-time work goes through the optional `setup` verb, declared in
  `describe` (an adapter declares it when it needs one-time work).
- The daemon kills an adapter's whole process group on a timeout, a hang or shutdown. A cut-off read is retried
  once; a write is never retried.

## Changing the protocol (version-skew rules)

1. Additive only: new verbs, fields and config keys are optional. Never repurpose one. `describe` answers
   `"zm": "zm/v0"`.
2. Don't move a responsibility between the daemon and the adapters in one PR. Ship daemon support first, then
   the adapters.
3. A PR that touches both this package and `cmd/zm-work-*` says so in its description and why it must.
4. SPEC 11 changes first, then the conformance suite, then the drivers.

## Tests

- Every adapter passes `zm-work-conformance` (read-only by default; `--write`; serve mode, pipelining, bad
  lines, cancel and EOF unless `--no-serve`). `scripts/smoke.sh` runs it on `zm-work-fake`.
- Use the fake tracker in `internal/work/fake`; never a real tracker write.
- `go test -p 2 ./internal/work/adapter/ ./internal/work/conformance/`; CI runs the rest.
