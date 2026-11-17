# Contributing to Mezze

Mezze is the fastest agent mail and issue tracker, in components, for a few people running thousands of
agents. The command is `zm`. The project is early, so interfaces will change.

## What is welcome now

- Bug reports.
- Fixes to the docs.
- Small, focused fixes with tests.

For anything bigger than a small fix, open an issue first so we can agree on the shape before you write it.

## Read first

- `AGENTS.md`: the rules for working in this repo (they apply to people too), the commands, and what never to do.
- `docs/ARCHITECTURE.md`: the bricks (no package imports another brick), the trust boundaries, and the brick map.
- `SPEC.md` is the contract and `API.md` lists every route. A new route is added to `API.md`.

## Build and test

```sh
go build ./...
gofmt -l .                              # prints nothing when files are formatted
go vet ./internal/<pkg>/...
go test -p 2 ./internal/<pkg>/...       # run only the packages you changed
make check                              # vet, tests and gofmt for the whole tree; heavy, run it once
```

`make check` runs the full suite and takes a while. Do not loop it; run the packages you touched while you work,
and let CI run the rest.

Tests use a temporary `ZM_HOME` and must clean up every daemon and child process they start. They must not touch
your real home directory or call a real `gh` or `bd`. `AGENTS.md` names the fakes to use.

## A good pull request

- One purpose.
- Tests for what changed.
- Says what it was tested on (OS, Go version, which tests ran).

Fill in the pull request template.

## Sign-off

Contributors outside the maintainers: sign every commit with `git commit -s`. That adds a `Signed-off-by` line and
certifies the statements in the [Developer Certificate of Origin](https://developercertificate.org/). There is no
CLA.

## Review

A person approves every pull request from an outside contributor, and a maintainer lands it (applied as a patch,
shipped in the next release). No agent merges or approves outsiders' pull requests. We aim to respond within 7 days.

## Security

Never report a vulnerability in a public issue. See `SECURITY.md`.

## CI on pull requests

A pull request from a fork runs its checks on GitHub's own runners, with a read-only token and no secrets, so it may
run slower than one from a branch in this repository. A maintainer approves the first run from a new contributor.
There is nothing else for you to do.
