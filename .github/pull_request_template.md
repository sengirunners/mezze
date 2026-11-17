<!-- What changed and why, in a few lines. Link the issue if there is one. -->

**Tested on** (OS, Go version, which tests ran)

## Bricks checklist (docs/ARCHITECTURE.md)

Only the boxes that apply: skip this section for a typo, a docs fix or a small bug fix.

- [ ] Which brick does this change? If it adds one: the SPEC protocol, the interface, a driver and a conformance suite.
- [ ] Did `internal/work`'s top level grow? (It should not.)
- [ ] Does any package now import another brick's package, or read another brick's tables?
- [ ] New `core.Deps` field or hook? Is it listed with its area in `core`'s package doc (start the list if it is missing)?
- [ ] Did we write something a maintained open-source component already does (Litestream, rclone, Dolt's tools, x/oauth2, Standard Webhooks...)? If so, is the reason in the package doc?
- [ ] Protocol changes are additive, and removals have a dated window in SPEC. No responsibility moved between the two sides of a protocol.
- [ ] `go vet` and `go test -p 2` pass for the packages I changed, and `gofmt -l .` prints nothing. Heavy suites ran in CI, not in a loop on my machine.
- [ ] Secrets go through `secrets` (potluck kit/secrets); external calls carry the caller; envs are allowlisted; new copies of user text have a scrubber.
- [ ] SPEC (contract), DESIGN-NOTES (why) and conformance (proof) are updated.
- [ ] Commits are signed off (`git commit -s`, see CONTRIBUTING.md), if you are not a maintainer.

## Suggested rules

<!-- Optional: a trap worth a rule (non-obvious, recurring, actionable). Don't edit AGENTS.md for it; see "Proposing rules" in docs/ARCHITECTURE.md. -->
