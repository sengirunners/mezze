# Changelog

Every main commit is built and tagged (`zm-main-<sha>`), so this is a flat list, newest first, not per release.

## Clients

**The rule** (docs/clients.md "Changes"): a PR that breaks a `stable` route, an event stream or a listed `--json`
command (removes or renames a field, changes a type or a meaning), changes a documented `beta` field, or takes a
route out of `stable` or a command out of the listed table adds one line at the top of this list, in the same PR:
the date, what changed and what a client does about it. Adding a field, value or route needs no line.

- 2026-11-17: a lease's new `fence` (fencing token), and optional `fence` on claim, release, comment and status
  requests and the native issue door's writes (`lease_fence` on a lease-backed native claim). A write or release
  sent with the fence of a lease that has gone is refused `409` with `error.data` `{"reason": "stale-lease"}`
  (SPEC 10 "Fencing"); a client that sends none sees no change. `zm mcp` now sends the fence of its own
  `work_claim` on that agent's `work_renew` and `work_release` of the key, so those answer `stale-lease` once the
  lease lapsed or was taken (once: the server then forgets the fence); the agent claims again with `work_claim`.
- 2026-11-17: new optional field and reasons, nothing to do: `POST /v0/asks/{id}/choose` takes `card_sha256` (the card
  text the person read; a different current card is `409` reason `card-changed`, nothing recorded), and its other
  `409`s now carry `data.reason` (`held`, `joined-action-ask`, `withdrawn`, `default-stands`; the standing Decision
  stays the data of an answered ask). Send `card_sha256` only to a daemon that knows it: an older one refuses an
  unknown request field (`400`).
- 2026-11-17: answer-only tokens (`zm token mint --answer-only --project <p> --node <device> --ttl <d>`, `limit`
  `answer` on `POST /v0/tokens`): for a client that shows and answers cards, from one tailnet device. It reaches
  `GET /v0/whoami`, `GET /v0/asks`, `GET /v0/asks/{id}`, `GET /v0/decisions` (asks only), `POST /v0/asks/{id}/choose`
  and `GET /v0/receipts/verify`; anything else is `403` reason `token-limit`. Its answers carry the new `surface`
  value `answer-token` and `machine` `answer-token:<id>` (receipt node `owner:answer-token:<id>`); treat an
  unknown `surface` as unknown. `GET /v0/receipts/verify` takes an optional `signed=1`. Owner tokens, socket
  answers and receipts are unchanged; a client holding an owner token only to answer asks may move to one
  (docs/clients.md section 5). `zm token mint` no longer marks `--as` and `--role` required in its flag
  definitions: it still refuses a mint without them unless `--answer-only`.
- 2026-11-17: the text `zm inbox`, `zm thread`, `zm sent` and `zm mail` print (not `--json`) now sits between
  the fixed lines "Untrusted data: the mail below was written by ..." and "End of untrusted mail data.", and each
  body is one quoted JSON string; the MCP `mail_inbox` answer is the same opening line, the JSON, the closing line.
  `--json` and the REST bodies are unchanged. A script that read the text output reads `--json` instead.
- 2026-11-17: new optional parts of the schema, nothing to do: the `429` `error.data` of `POST /v0/mail` and
  `POST /v0/mail/{id}/reply` (`limit`, `max`, `window_s`), `dev.zm.daemon.stopping`'s data (`by`, `what`, `force`),
  health's `drivers`, `skipped` on the mail and ask lists a team replica merges. A test now fails a
  new `required` entry in an object schema the floor already had.
- 2026-11-17: new optional fields, nothing to do: `limit` and `node` on `POST /v0/tokens`, `limit` and `device` on
  `Token`, `device` on the mint response and `limit` on `GET /v0/whoami`, for tokens limited to some routes and pinned
  to one tailnet device. No route admits a limit yet, so a mint with `limit` is always refused and every
  existing token works as before.
- 2026-11-16: `POST /v0/asks` with a script or command run action now refuses (`400`) a `ref` the run machine
  would refuse (`refs/...`, a path), a `commit` that is not exactly 40 hex, a ref the project's remote does not
  have, and a commit that is not what the ref resolves to on the remote now; before, such cards failed only after
  the yes. Send the short branch or tag name and the full id `git rev-parse <ref>` prints. A checked card's run
  carries `run.commit_on_ref` and still runs its commit after the branch moves on; new: `POST
  /v0/asks/{id}/action/retry` starts an approved action that failed before it started again (SPEC 16 "Retry").
- 2026-11-16: `docs/schema/openapi.json` (OpenAPI 3.1; its schemas are JSON Schema 2020-12) describes the request
  and response bodies of every `stable` route and the data of each `dev.zm.*` event, generated from the Go types;
  ci fails when the committed file and the types differ, so a shape change shows as a diff of it. Nothing changed on
  the wire. A client may validate against it or generate types from it; ignore fields and values it does not list.
- 2026-11-16: `POST /v0/mail` and `POST /v0/mail/{id}/reply` may answer `429 rate_limited` (SPEC 9 "Send limits":
  the same mail again, a reply loop between two addresses, or one sender's flood). Nothing was sent; wait
  `retry_after` seconds (the message says it too) or send something else; through a team replica, retry with a
  new `client_request_id`.
- 2026-11-16: `POST /v0/asks/{id}/choose` (the owner answers an ask), `GET /v0/receipts/key` and
  `GET /v0/receipts/verify` are now `stable`, and `zm ask answer`, `zm receipt key` and `zm receipt verify` are
  listed commands; docs/clients.md section 5 walks list, show, answer and check. Their shapes did not change, and
  nobody may answer who could not before. Nothing to do; a client may now build on them.
- 2026-11-14: API.md labels every route `stable`, `beta`, `app` or `internal`, and docs/clients.md describes the
  contract: access, headers, event streams, the stable routes and the listed `--json` commands, whose shapes a
  test now checks. Nothing changed on the wire.
