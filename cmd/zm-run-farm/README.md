# zm-run-farm

A SPEC 20 run target (`kind = "run"`, `isolation = "none"`, `credentials = true`) that runs each job on a machine
of the team project through zm's own script runs (SPEC 8, the path behind `zm exec`): no ssh, no hub.

Per run: the authority places a task of agent `run` on the machine (`machine = "auto"` picks the roomiest). The
machine resolves `ref` on its checkout's remote with a live lookup, refuses a `commit` that is not what the ref
resolves to (`forbidden`), makes a clean checkout of exactly that commit, runs the script with SPEC 20's
environment allowlist, ends it at the deadline (exit 124), and removes the checkout. `acquire` answers once a
machine has taken the run; a run nobody takes within 100 s is withdrawn and `acquire` answers `unavailable`.

The machine's user runs the script beside that user's credentials, so zm treats the target as trusted code
only: the remote head of the binding's `branch`, acquired from the owner's socket, scripts from the binding's
`scripts` list.

## Config (the binding's driver keys)

| key | default | meaning |
| --- | --- | --- |
| `project` | required | the zm project whose team machines run it (its checkout on the machine is used) |
| `machine` | required | a machine name, or `auto` |
| `default_ttl_s` | 1800 | the TTL when `acquire` sends none |
| `ttl_max_s` | 14400 | lifetime cap; there is no `renew` |
| `state_dir` | `~/.zm-run-farm` | one record per handle, for `list` and `not_found` (kept 7 days after the end) |

`actor` in `acquire` must be a live session: the run is that session's task. Output: `zm task read <task> output`
(the `log` in `acquire`'s answer).

## States

| task | state |
| --- | --- |
| pending, running | `running` |
| the script exited N | `ended`, `exit: N` |
| deadline | `expired`, `exit: 124` |
| cancelled (release, or `zm task cancel`) | `released` |
| killed by a signal, the machine lost it, or the authority forgot it | `lost` |

## Check

```sh
zm-run-conformance zm-run-farm --remote --ref main --commit <the tip of main, with probe.sh> \
  --repo <remote> --script probe.sh --config project=<project> --config machine=auto
```

The commit must be the ref's tip: the machine refuses any other, so the "ref moved" check is a SKIP.
