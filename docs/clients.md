# Clients: how a program talks to zm

For anyone writing a client: a UI, a bot, a script. The Mezze app is one client and gets nothing others
don't, apart from the routes labelled `app`. The contract is three things: the routes labelled `stable` in
`API.md`, the two event streams, and the `--json` output of the commands listed below. Request and response types
are Go structs: the core ones (projects, tokens, errors) in `potluck/kit/api`, which you can import, and mail,
work, sessions and asks in this module's `internal/orchapi`, which you can read but not import. Their machine-readable
form is [`docs/schema/openapi.json`](schema/openapi.json) (see "Schema" below).

## 1. Get access

**On the same machine, as its OS user:** no token. Talk to the daemon's Unix socket, `$ZM_HOME/zm.sock`
(default `~/.zm/zm.sock`), or run `zm`, which uses the socket. The socket's caller is the machine's owner, and
only the socket can create a token or act as a local session (`Zm-As`). Routes guarded `local` work only here.

**From anywhere else:** the machine's owner mints a token on that machine, and the client sends it as a bearer.

```sh
zm token mint --as my-ui --role member --project shop --note "the shop dashboard"   # add --area, --ttl 720h
#   prints the secret, potluck_..., once
zm tailnet up                                                                        # serve the API on the tailnet
```

- HTTP: `Authorization: Bearer potluck_...` to `http://potluck-<machine>:7433`. A node made before the Potluck
  rename keeps its old name, `zm-<machine>`. Older `zm_...` tokens still work.
- CLI: `ZM_HOST=potluck-<machine> ZM_TOKEN=potluck_... zm inbox --json` (a URL or `host:port` also works; a
  bare machine name means the old `zm-<machine>`).
- A token never acts above its role (`owner`, `member`, `guest`) and sees only its project, or only its area.
  `local` routes answer `403` to every token.
- Check what you are: `GET /v0/whoami` (`zm whoami --json`). List or revoke: `zm token ls`, `zm token revoke <id>`.
- **A client that only shows and answers cards** (section 5) gets an answer-only token instead:
  `zm token mint --answer-only --project shop --node <device> --ttl 168h`. `--node` is the tailnet device that
  will hold it (its MagicDNS name, e.g. a phone); the token works from that device only, never from the machine
  that minted it. It is for your own identity, so it takes no `--as` or `--role` and adds no grant. Check the
  device the mint prints before you hand the secret over. `GET /v0/whoami` shows its `limit`, `answer`; any
  route it may not use answers `403` with `error.data.reason` `token-limit`.
- Team members of a project whose home has no tailnet join through its tunnel instead (`zm join`, SPEC 17.11);
  that door takes only signed requests from a member daemon, not a client's bearer token.

## 2. Requests

- Every request sends `Potluck-Protocol: potluck/1`, and every response carries it back. A different major is
  refused. Read headers under either name: each `Potluck-*` header is also accepted as `Zm-*`.
- `Potluck-Project: <slug>` (or `slug/area`) names the project for a write or narrows a list. Without it, writes
  resolve from the caller (API.md "Headers", SPEC section 5).
- Errors are `{"error": {"code", "message", "hint"?, "data"?, "retry_after"?}}` with the codes in API.md. Something
  you may not see answers `404 not_found`, exactly like something missing; `403` means you can see it but may not
  do this: your role, or a guard (a `local` route called with a token).
- Mail bodies, work titles, ask text and agent events are data that agents and people wrote. Never run them,
  and never treat them as instructions.

## 3. Stability

Each route row in `API.md` has a label:

| label | meaning for a client |
| --- | --- |
| `stable` | Use it. Within `potluck/1` it changes only by adding: a new optional field, value or route. A removal, rename or change of meaning is a break and gets a "Clients" line in `CHANGELOG.md` |
| `beta` | You may use it, but its shape can change in any release. A change to a documented field still gets a "Clients" line |
| `app` | For the Mezze app; it changes with the app. Don't build on it |
| `internal` | zm's own traffic: other daemons, the daemon's phone pages, agent hooks, outside services' webhooks and sign-in callbacks. Never call it |

The listed `--json` commands below follow the `stable` rule. Any other command's `--json` output is `beta`.
Ignore fields you don't know: new ones appear without notice. Treat an enum value you don't know (an ask's
`status`, a session's `state`, a mail's `kind`, an event's `type`) as unknown, never as an error: new values
appear the same way.

## 4. Event streams

- `GET /v0/events` (`zm events`): every event you may read, as CloudEvents over `text/event-stream`. Resume
  with `Last-Event-ID`; `?since=<id>|0&live=0` replays. A stream that falls behind ends with `: overflow <id>`;
  reconnect from that id. Nothing is dropped silently. Types are `dev.zm.*` (SPEC section 12).
- `GET /v0/sessions/{id}/events`: one structured session's agent events. Long-poll with
  `?after=<seq>&limit=&wait_s=` (answers `{events, next, state, ended?, truncated?}`), or ask for
  `Accept: text/event-stream`. Stop when `ended` is true.

## 5. Answer an ask

An ask is a decision an agent put to a person (SPEC section 16). A client lists asks, shows one, answers it as the
project's owner and checks the signed receipt, all on `stable` routes.

1. **List and show.** `GET /v0/decisions` lists what waits on you; `GET /v0/asks?open=1` lists open asks, and
   `GET /v0/asks/{id}` shows one. Show the card as it is (`card`: question, choices, recommendation), and keep
   its `id`, `card_sha256` and `subject.work` (or `subject.question`) for the check.
2. **Answer.** `POST /v0/asks/{id}/choose` with `{"decision": "<a choice's key>", "text"?: "...", "as"?:
   "owner|contact"}` (`zm ask answer <id> <key>`). The answer is the Decision with its
   `receipt`. The same answer sent again returns the same receipt, so a retry is safe. Read `decision`, `answer`
   (as read), `approver`, `time` and `surface` (`owner`, `answer-token`, `device` or `key`: where it was answered;
   `method` is the older `owner|approver` split). `machine` is display text: a machine name, `token:<id>`,
   `answer-token:<id>` or `tailnet`. Besides a
   choice's key, `decision` may be `other` (a written answer, in `text`, that picks no choice) or `needs_context`
   (sent back with a note: nothing was decided, and the asker asks again). `recorded_at` is set once every effect
   landed (the work item comment, the asker's reply or the item holder's notice, the event, and a card action's
   ack or run); until then `write_error`, `mail_error` and `deliveries` (`{kind, state}`) say what is not. zm
   retries a `pending` delivery; a `failed` or `unknown` one needs a person. The ask's `status` is `answered`, and
   `recorded` once `recorded_at` is set: read `decision` too, since a sent-back ask is `answered` as well.
3. **Check the receipt.** `GET /v0/receipts/verify?receipt=<base64url of the receipt JSON>&project=<slug>&ask=<id>
   &work=<subject.work>&card=<the card_sha256 you showed>&decision=<the key you sent>` (`question=` for a
   question's ask; `zm receipt verify --decision <key>`, whose default is `approve`).
   `valid: true` with each expectation named in `checked` means the deciding daemon's key signed that choice to
   that card. Written text and `answered_as` are signed (`text_sha256`, `answered_as`) but not compared. The route
   checks with the daemon's current key only. Add `&signed=1` to require a signed answer (a paired device's or a
   key approver's): any owner answer, from the socket or a token of either kind, is then `valid: false`.

**Who may answer:** the project's owner (`core.Deps.ProjectAdmin`), and nothing about that changes with the label.

- From a phone, or any device that runs no agents: an **answer-only token** (section 1,
  `zm token mint --answer-only --project <p> --node <device> --ttl 168h`). It does steps 1 to 3 above for that one
  project and nothing else: it reads no mail, work items, sessions or events, sees no other project, and changes
  nothing but the answers it gives (an answer's own effects stay: the asker's reply, the work item's comment, a
  card's ack of the mails it froze). Every other route
  answers `403`, reason `token-limit`; `GET /v0/decisions` lists asks only. Its answers are the owner's, unsigned:
  `surface` is `answer-token`, `machine` and the receipt's `node` name the token (`answer-token:<id>`,
  `owner:answer-token:<id>`), and it never picks a choice that runs something, hands a brief or carries a confirm
  word (`403`, reason `signed-only`), nor answers a pull request's ask. It works only over the tailnet, from the
  device named at mint (a new device, or one that re-keys, needs a new token), so a client that reaches the daemon
  through the tunnel door cannot use it yet. A stolen one can, from that device, read that project's asks and
  answer any card the owner could answer unsigned, written answers included, until it expires, is revoked
  (`zm token revoke <id>`) or you stop owning the project; every answer it gives names it. It is only as safe as
  the device holding it: never put it where agents run. A gate that must refuse every unsigned answer checks the
  receipt with `signed=1`.
- On the machine, as its OS user, over the socket: no token. Every process of that user can do the same,
  agents included (SPEC 16 "Owners decide").
- From elsewhere, for a client that needs more than asks: a token for your own identity (`identity.name` in `zm whoami --json` on that machine):
  `zm token mint --as <you> --role owner --project <p> --ttl 24h`. Never mint an owner token for a new name
  (`--as my-ui`): that makes a new owner of the project, who then counts as a person asks go to. An owner token
  carries all of the owner's rights over the network, grants included: keep it out of agents' environments,
  give it a TTL, and revoke it when done. A tailnet login mapped to the owner's identity passes too, but every
  node signed in with that login (agents' machines included) can use it: prefer the token.
- Whoever holds an owner token answers exactly as the owner, an agent included: nothing tells them apart.
- Anyone else is refused: a member token reads asks but cannot answer (`403`); an ask you may not read (as a
  guest who is not its asker or recipient, or outside a token's area) is `404`. zm may refuse calls from inside an
  agent session in a later release, on the socket and to mint an owner token; that change gets a "Clients" line.

**Refusals:** branch on the status and `error.data.reason`, never on the message.

| answer | meaning |
| --- | --- |
| `403`, reason `signed-only` | this choice runs something (risky choices included), or, for an answer-only token, carries a confirm word: answer it on a paired device or with a key approver |
| `403`, reason `token-limit` | an answer-only token on a route it may not use |
| `403` | not the project's owner; a pull request's ask (only a paired device decides those); or an ask whose action runs on another person's own machine (only that person answers it) |
| `409`, `data` a Decision | already answered differently; `data` is the answer that stands |
| `409`, reason `card-changed` | the card is not the one you read: you sent `card_sha256` (optional; the `card_sha256` of the ask you showed the person) and it is not the ask's now, or the card changed while the answer was made. Read the ask again and show it again |
| `409`, reason `held` / `joined-action-ask` / `withdrawn` / `default-stands` | the card judge has not passed the ask yet; it joined an open ask that carries an action (answer that one); the asker withdrew it; you sent it back (`needs_context`) while its applied default stands |
| `409`, reason `confirm-word` | the choice runs a risky action: send its confirm word (`card.action.confirm_word`) as `confirm` |
| any other `409` | no reason: a write a team replica forwarded was lost or replayed. Read the ask again |
| `400` | not one of the ask's choices; `other` or `needs_context` without `text`; `as: contact` on an ask with no outside party; text too long |
| `404` | missing, or not yours to read |

**Offline check.** Compare `receipt.key_id` with the ask's `receipt_key` and with the `key_id` of
`GET /v0/receipts/key?project=<slug>`. Verify the ed25519 `sig` (standard base64, as is `public_key`) over the
receipt's `v` (`zm-receipt/2`), a newline, and the compact JSON of `v, project, ask, subject {repo, pr, head_sha,
work?, question?}, decision, approver, node, time, card_sha256?, text_sha256?, answered_as?, action?` in that
order. Leave out a field marked `?` when it is empty (`zm-receipt/2` always has `card_sha256` and `answered_as`);
a pull request's receipt is `zm-receipt/1`, with none of the `?` fields. zm encodes like Go's `encoding/json`:
`<`, `>`, `&`, U+2028 and U+2029 are escaped as `\u003c`, `\u003e`, `\u0026`, `\u2028` and `\u2029`. A
signature is not the whole check: then compare `project`, `ask`, the subject, `card_sha256` and `decision` with
what you showed and sent, and check the form (`zm-receipt/2` has exactly one of `work` or `question`,
`answered_as` is `owner` or `contact`; a card's answer is never `zm-receipt/1`), as `Verify` in
`internal/approvals/receipt.go` does; else a receipt for another ask, card or choice passes. From now on a
receipt gains no field within its `v` (`action` was the last, added to `zm-receipt/2` on 2026-11-05), because
verifiers refuse fields they don't know. Prefer the route unless you need to check without a daemon.

## Schema

[`docs/schema/openapi.json`](schema/openapi.json) is one OpenAPI 3.1 document: each `stable` route's request body,
its success bodies by status and the error body, with the refusals these docs name (an answer's `403` and `409`, a
claim's `409`, a repeated ask's `409`, mail's `429`) and `error.data`'s type where it has one, and under
`components.schemas` one schema per `dev.zm.*` event type (the envelope `Event`, with that type's `data`). Its schemas are JSON Schema 2020-12, made from the Go types by
`scripts/clientschema`, so any 2020-12 validator or OpenAPI 3.1 code generator reads it.

- **Response bodies.** A `required` field is always present; any other field may be absent, and a field that may be
  `null` says so. A field added within `potluck/1` is added optional, never `required` (a test checks it against
  `docs/schema/openapi.released.json`, the floor as of the last release cut, and against main's copy of the
  schema when git has main; the floor is refreshed only to cut a release, never with a shape change), so a body a team replica relays from an older daemon still validates. No schema
  forbids an unknown property or lists a string's values: a newer daemon adds both, so a client validating with an
  older copy does not reject it.
- Through a team replica, a claim, release or lease answers `202` with the queued write while the project's
  authority is away; the document lists that answer too.
- **Event data** (the `...Data` schemas) requires nothing. An event replayed from a log may be older than a field,
  or written by another machine's daemon, and the bus leaves some keys out for some readers.
- **Request bodies** (the `...Request` and `...Input` schemas) require nothing: an absent field is its zero value.
  The daemon refuses a field it does not know (`400`), so send only the fields your copy lists.
- Query parameters, headers and guards are in `API.md`, not in the document (decided 2026-11-17: they are few and
  stable, and a second place for them would drift).
- An event whose data has no Go type yet says so in its `description` and allows any data. A team replica's merged
  mail and ask lists may carry `skipped` (an authority that did not answer), as work lists do; health may
  carry `drivers` (bindings whose driver the daemon cannot run).

ci runs `go test ./scripts/clientschema/`, which fails when the committed document differs from what the Go types
generate, when a `stable` route in `API.md` or a `dev.zm.*` type has no entry in it, when a place (a component or a property object) the
last release cut's schema had gains a `required` entry, or when a golden shape of
`TestClientJSONShapes` (below) has a field the schema lacks, a field of another type, or lacks a required one.
Regenerate it with `go run ./scripts/clientschema` from the repository root.

## Stable routes

Guards and checks are in `API.md`. A test keeps this table equal to the routes `API.md` labels `stable`.

| route | for |
| --- | --- |
| `GET /v0/health` | is the daemon up; which machine |
| `GET /v0/whoami` | who the daemon thinks you are |
| `GET /v0/events` | the event stream |
| `GET /v0/projects` | projects you can see |
| `GET /v0/projects/resolve` | which project a request resolves to (header, working directory, session) |
| `GET /v0/projects/{project}` | one project |
| `GET /v0/projects/{project}/areas` | its areas |
| `GET /v0/tokens` | tokens (socket only) |
| `POST /v0/tokens` | mint a token (socket only) |
| `DELETE /v0/tokens/{id}` | revoke a token (socket only) |
| `POST /v0/mail` | send mail |
| `GET /v0/mail` | query mail you may read |
| `GET /v0/mail/inbox` | unacked mail to you |
| `GET /v0/mail/sent` | mail you sent |
| `GET /v0/mail/{id}` | one mail |
| `POST /v0/mail/{id}/ack` | take a mail |
| `POST /v0/mail/{id}/reply` | reply in its thread |
| `GET /v0/mail/{id}/thread` | the whole thread |
| `GET /v0/work` | work items |
| `POST /v0/work` | new work item |
| `GET /v0/work/inflight` | what is being worked on |
| `GET /v0/work/{id}` | one item |
| `GET /v0/work/{id}/holder` | who holds it |
| `GET /v0/work/{id}/comments` | its comments |
| `POST /v0/work/{id}/claim` | take the lease on it |
| `POST /v0/work/{id}/release` | let it go |
| `POST /v0/work/{id}/comment` | comment on it |
| `POST /v0/work/{id}/status` | change its status |
| `GET /v0/leases` | live leases |
| `POST /v0/leases` | lease any key (`lock:migrations`) |
| `DELETE /v0/leases/{key}` | release it |
| `POST /v0/sessions` | start an agent session (socket only) |
| `GET /v0/sessions` | sessions you can see |
| `GET /v0/machines` | machines and their presence |
| `GET /v0/sessions/{id}` | one session |
| `POST /v0/sessions/{id}/stop` | stop it |
| `GET /v0/sessions/{id}/events` | a structured session's events |
| `POST /v0/sessions/{id}/turn` | send a structured session a turn |
| `POST /v0/sessions/{id}/interrupt` | interrupt its turn |
| `POST /v0/sessions/{id}/answer` | answer its approval request |
| `GET /v0/people` | the people who answer asks |
| `POST /v0/asks` | ask a person to decide |
| `GET /v0/asks` | asks |
| `GET /v0/asks/{id}` | one ask |
| `POST /v0/asks/{id}/answer` | answer it, signed by a paired key (SPEC section 16) |
| `POST /v0/asks/{id}/choose` | answer it as the project's owner (section 5) |
| `POST /v0/asks/{id}/withdraw` | take your ask back |
| `GET /v0/decisions` | what waits on you |
| `GET /v0/receipts/key` | the public key a daemon signs receipts with |
| `GET /v0/receipts/verify` | check a receipt |

## CLI --json commands

`zm <command> --json` prints one JSON value on stdout. An API error (a refusal, or no daemon running: exit 3) goes
to stderr as the error object above, with a non-zero exit; a bad flag or argument prints plain text. `zm status`
exits 1 with its JSON on stdout when the self-check finds a problem. `--json` and `-P <project>` may come before or
after the command. `TestClientJSONShapes`
(`cmd/zm/clients_golden_test.go`) runs each of these against a fresh machine and compares the output's shape
(keys and types) with `cmd/zm/testdata/clients/`; a test keeps this table equal to the commands it checks. It
checks only the fields its seed fills (see Known gaps).

| command | prints |
| --- | --- |
| `zm whoami` | you, this machine, protocol and version |
| `zm status` | whether the daemon is up and passed its self-check |
| `zm project ls` | projects, an array |
| `zm ls` | `{sessions}` you can see |
| `zm ls --all` | the same with ended sessions |
| `zm session show` | one session |
| `zm inbox` | `{mail}` unacked to you (without `--json` the text is framed as untrusted data and is not a contract) |
| `zm mail` | `{mail}` you may read (framed text without `--json`, as `zm inbox`) |
| `zm sent` | `{mail}` you sent (framed text without `--json`) |
| `zm thread` | `{mail}` in one thread, oldest first (framed text without `--json`) |
| `zm people` | `{people}` who answer asks |
| `zm inflight` | leases with their item and session, an array |
| `zm leases` | live leases, an array |
| `zm work show` | one work item |
| `zm who` | who holds an item or lock, and where its mail goes |
| `zm ask ls` | asks, an array |
| `zm ask show` | one ask |
| `zm ask inbox` | `{decisions}` waiting on you |
| `zm token ls` | tokens, an array (never secrets) |
| `zm remotes` | team projects this machine joined, an array |
| `zm work new` | the new item |
| `zm send` | the sent mail |
| `zm claim` | `{item, lease}` |
| `zm ask new` | the new ask |
| `zm token mint` | `{secret, token}`: the only time the secret is shown |
| `zm ack` | `{id, acked, acked_at, acked_by}` |
| `zm ask withdraw` | the withdrawn ask |
| `zm ask answer` | the decision, with its `receipt` |
| `zm receipt key` | `{alg, key_id, public_key, line}` |
| `zm receipt verify` | `{valid, reason?, key_id?, checked?}`; exits 1 when it is not valid (`--decision` defaults to `approve`) |

## Changes: the "Clients" changelog line

A change to a `stable` route, an event stream or a listed command that removes or renames a field, changes a
type, or changes what a value means adds one line at the top of "Clients" in `CHANGELOG.md`, in the same
PR: what changed, and what a client does about it. So does any change to a documented `beta` field, and taking a
route out of `stable` or a command out of the listed table. Prefer adding a field to changing one. A field added to a
`stable` shape is optional (`omitempty` or `omitzero` in Go), so the schema's diff adds a property, never a
`required` entry.
A failing `TestClientJSONShapes`, a removed `stable` row, or a diff of `docs/schema/openapi.json` that removes or
changes something rather than adding it is the signal; refresh the golden files with
`go test -p 2 ./cmd/zm -run TestClientJSONShapes -update-clients` and the schema with `go run ./scripts/clientschema`
only together with that line. No test checks that the line is there: the reviewer does.

## Known gaps

- The Mezze app writes mostly through the CLI (`zm send`, `zm ask new`, `zm work new` and others) rather
  than HTTP, and reads issues through `bd`, not the `GET /v0/issues` routes (`beta`).
- The app calls some `beta` routes over the socket: `GET /v0/owner`, `GET /v0/team/remotes`, `GET /v0/tailnet`,
  `GET|PUT /v0/mail/read-marks`, `POST /v0/mail/{id}/got-it`, `POST /v0/asks/{id}/undo`, the GitHub
  sign-in routes and the approver key routes. They are not part of the contract yet.
- The app's structured-session client sends no protocol header, and one helper still sends `Zm-Protocol: zm/v0`.
  Send `Potluck-Protocol: potluck/1`.
- The app runs about 45 `--json` commands; the table above lists the tested ones. The rest (`host`,
  `target`, `connection`, `owed`, `peers`, `approver`, `invite`, `github ls`, `identity alias ls`,
  `session admission`, `t3`, ...) are `beta`.
- The golden test checks only fields its seed fills. It seeds a session that needs its human, mail with a work
  ref, an ask left open and then withdrawn, a second ask the owner answers (its decision, receipt and a verify
  against the ask, item, card and choice), and a token with a note and expiry beside a revoked one. Not covered: an
  ended session (`zm ls --all` checks only live ones), a device's or key's decision, `zm remotes` entries (one
  machine, no team: checked only as an array), and most other optional fields.
- `scripts/clientschema` names each `stable` route's Go types by hand, from its handler; only the CLI goldens (the
  commands' `--json` output, which the CLI decodes and writes again) are checked against the schema. The schema
  gives each value's JSON type (and `date-time` for times), never which strings it may hold.
- Only the owner's path answers an ask on `stable` routes alone. A key approver's answer route is `stable`, but
  binding the key is not (the sign-in's key purpose, `/v0/approvals/pair-key`, `/v0/approvals/key`), and a key
  bound by a sign-in counts only from its own node, never with a minted token. `GET /v0/owner` (may I answer?) is
  `beta`: on `stable` routes a client learns it from `choose`'s `403` when the person answers. Never call
  `choose` to test: an allowed call records the answer.
- `choose` takes no `card_sha256`: check the card you showed afterwards, with `card=` on verify.
- A request body with a field the daemon does not know is `400`: a client sending a field added later checks the
  daemon's `version` (`GET /v0/health`) first.
- Tokens are bearer secrets with no scopes finer than role and area, and minting one needs the machine's socket.
  There is no sign-in flow for a third-party client.
