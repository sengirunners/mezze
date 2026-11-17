# Runbook: publish a release copy early (the urgent route)

Status: in force from 2026-11-17. Contract: the header of `scripts/publish/main.go`. Machines and the copy:
[zm-upgrade.md](zm-upgrade.md), "A machine built from a private repository".

A copy of a release is published only once its rolling release is five days old (`-soak`, default `120h`): the team's
machines have run that tree first. The urgent route publishes one sooner. It shortens that wait and nothing else.

## When

A fix must reach the public copy before its five days, and waiting harms people who run the copy: a security fix,
a defect that loses or exposes data, or one that stops machines updating or starting. Never for a feature, a date or
convenience. The team's own machines do not need it: a machine on its own order takes private releases without the
five days, at its own `--min-age`.

## Who presses

An owner, on the publish card. Publishing is an owner step: an agent drafts and files the card, and never runs the
push or presses for anyone. The card runs on the machine that publishes: a trusted machine of the project that takes
tasks, with Go, git, a `gh` login that can push to the public repository, and the config judge's model login.

## What is skipped, and what never is

- **Skipped: the age rule only.** The card's `-soak` is the release's age when the card is drafted, in whole hours
  rounded down (publish takes any release at least that old, so a later press still passes). The release must exist
  (CI built it from green main), and before drafting, `zm daemon update status` must show the canary machine listing
  it as stable and no machine listing it as failed (publish does not check this; the drafter does).
- **Never skipped:** every scan of the public tree (the public-tree gate, the private names list, gitleaks, and the
  config judge: a judge that does not answer holds the copy), the commit being on private main, the module path,
  the release pin check, the neutral identity, a new tag and a tree that differs from the last copy. A hold is
  fixed and put on a new card, never bypassed: no flag turns a scan off, and the card must not add one.
- **The reason is public:** the same command writes it as the notes of the version's release on the public
  repository, starting `Published early, before the usual five days, because`. The card's check passes only when
  the tag and those notes are there. Write the reason in plain ASCII words: no quote marks, `$`, backtick or
  backslash, since it sits inside the command's quotes.

## The card (ready text)

Fill the angle brackets; keep the context's first sentence: the Decisions page shows only its first words.

```sh
zm ask new "Publish this fix to the public copy now, before its five days?" --work bd:<item> --to owners \
  --title "Publish a fix early" \
  --context "A fix has to reach people who use the public copy now, not in five days: <what goes wrong for them until it ships, one sentence>. Publishing early skips only the five-day wait. Every safety scan still runs, and the reason is written on the public release for anyone to read." \
  --choice "Publish now | The fix reaches everyone today | The build has run on our machines for only <hours> hours | Publishes the build now, with the reason on its public release" \
  --choice "Wait the five days | The usual wait | People keep the problem until <date> | Nothing changes; the normal publish card comes on <date>" \
  --recommend <a or b> --why "<what publishing now does, good and bad>" \
  --searched "<the fix, its tests, the hours it ran on our machines, any failed update since>" \
  --risk irreversible --run-on <publishing machine> --ref zm-main-<sha12> --commit <full sha> \
  --command "go run ./scripts/publish -tag v<X.Y.Z> -ref <full sha> -ident '<neutral identity>' -soak <age>h -push && gh release create v<X.Y.Z> -R <public owner/name> --verify-tag --latest=false --title v<X.Y.Z> --notes 'Published early, before the usual five days, because <the reason in one plain sentence>. Every scan ran.'" \
  --verify "test \"\$(git ls-remote https://github.com/<public owner/name>.git refs/tags/v<X.Y.Z> | cut -f1)\" != '' && gh release view v<X.Y.Z> -R <public owner/name> --json body --jq .body | grep -q '^Published early, before the usual five days, because'" \
  --after-press "The publishing machine runs the copy and writes the reason on the release; you get its result. Your machines keep updating from the private releases."
```

A publish that holds (a scan hit, a judge with no answer) pushes nothing and writes no notes. It ran, so it is not
retried: fix the cause and file a new card (a scan hit needs a fix on private main, so a new commit and release; a
`-soak` above the release's age or a tag that exists needs only a corrected card).

A push that succeeded with notes that failed (network, a refused login) has published: the check stays red. File a
card whose `--command` is only the notes step, safe to run again, with the same `--verify`:

```sh
gh release edit v<X.Y.Z> -R <public owner/name> --notes '<the same notes>' || gh release create v<X.Y.Z> -R <public owner/name> --verify-tag --latest=false --title v<X.Y.Z> --notes '<the same notes>'
```
