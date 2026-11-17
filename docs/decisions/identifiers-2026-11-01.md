# Identifiers and comments for a community of thousands (design, 2026-11-01)

Status: **decided** (2026-11-01). Stages 1 and 2 are built; stage 3 part 1 (names of this machine's sessions) is built,
and part 2 (names of other machines' sessions, through presence) is not. Stage 4 is decided in its own doc,
[owners-2026-11-01.md](owners-2026-11-01.md) (GitHub-backed owners, 2026-11-02); nothing of it is built yet.

## Why

Mezze is meant to grow into a community of thousands of engineers, then a social layer where anyone can start
a coop or an open project that anyone on earth can work on. People and agents must be able to point at a specific
issue, project, agent or session by a short name they can say and type, which never changes underneath them. They also
need names they can edit. Authorities run anywhere, and offline replicas (round C) create items with no central server
to ask. Projects open to strangers also need moderation.

Two decisions come out of the Linear-compatibility work: native issues keep bd-style ids rather than Linear's `KEY-number`,
and comments become editable and deletable without losing the record.

## Today

| thing | id now | how it is made | shown to people |
| --- | --- | --- | --- |
| native issue | `harbor-r74`; children `harbor-r74.2` | base36 of a sha256 over title, description, actor, time, nonce; length 3-8 grows with the project (collision odds at most 25% before a retry) | yes (bd, zm, web, GraphQL identifier) |
| issue number | 1, 2, 3 ... per project | counter at create, never reused | bd-zm JSON `number`, GraphQL `number`, web UI |
| project | ULID + slug `harbor` | ULID random; slug chosen, `[a-z0-9][a-z0-9._-]{0,62}` | slug |
| identity (human, agent) | ULID + name `ivo` | name chosen, same grammar | name |
| session | `mini/4bb7545a` (machine / 8 hex) | random | address; `title` is the terminal title |
| everything stored | ULID | `internal/ids`, monotonic with random steps (no count leaks) | no |

Comments are append-only. A history row records each one, and events carry them to subscribers, webhooks and replicas.

## Principles

1. **Three layers.** Every entity has an internal id (ULID, never shown, used for links, sync and merge), a **handle**
   (short, random, readable aloud, unique in its scope, never changed, never reused) and a **name** (free text,
   editable at any time).
2. **No counters as visible ids.** A counter needs a single place to hand out numbers (two offline replicas both mint
   ENG-501), grows without bound and gives away size and order. Random handles grow with the log of the size.
3. **Handles survive renames and moves.** An old handle stays valid as an alias that answers with the current one.
   It is never given to anything else.
4. **Everything addressable is addressable by handle or by a unique name.** "the linear-adapter session", `4bb7`
   and `mini/4bb7545a` all reach the same session. A name that matches more than one thing is refused, and the
   refusal names the candidates.
5. **Moderation is first-class.** In open projects, comments can be edited, deleted and redacted. Each act leaves an
   audit trail, except that redaction truly removes the content.

## Who mints what (today and with offline replicas)

Today every native issue, child and number is minted by the project's authority, in one SQLite transaction, one
writer: a collision is found and retried at once. Round C replicas queue writes in an outbox and the authority
applies them; creating native issues offline is not built (17.6 "Not done"). The rules below are written for both:
what the authority mints now, and what a replica must do once it can create items offline.

- **Authority-minted handles** may be short, because a collision is retried in the same transaction.
- **Replica-minted handles** (future) cannot be retried against other replicas, so they get a floor of 7 symbols
  (31^7 = 2.75e10). An offline creation carries a client-minted ULID (as offline comments do), so the authority
  can tell a replay of the same creation from a second issue that drew the same handle. The first creation to reach
  the authority keeps the handle. A later one is minted again, and the authority answers `{old, new}`. The replica
  rewrites its own structured references (outbox deps, children, its copy) and shows a note. There is no alias:
  the old handle belongs to the winner. Free-text mentions of the old handle written offline are an accepted
  loss, which the floor makes rare.
- **Children** (`<parent>.<N>`, bd's scheme) and **numbers** are counters, so only the authority assigns them. A
  replica's offline child is created with a random child handle (`<parent>.<random>`); the bd door and every parser
  already accept any `a.b` id. A random child handle has the same 7-symbol floor and at least one letter, so it
  cannot collide with the numeric counter range (`childID`'s scan of existing children skips non-numeric ones). A
  replica's offline issue has no number until the authority commits it, and is not served through GraphQL
  (Linear's `number` is non-null) until then.

## Handles: the alphabet and length

For new native issue handles (and later any new handle):

- **Symbols**: lowercase `a-z` and `2-9` without the look-alikes `0 1 i l o`: 31 symbols. Vowels stay. Dropping
  them shrinks the space by about a quarter per symbol and does not stop profanity (`fkng`, `kkk` can be minted
  without vowels). A small blocklist is checked at mint instead: a minted handle containing a listed word is drawn
  again. The list is English-only for now (a known gap for a global community); session and project names get the
  same check when they become editable.
- **At least one letter**: a handle never looks like a number. `harbor-742` (a hash or a count?) cannot be minted,
  and a client that upper-cases a numeric-looking identifier (HARBOR-742) cannot confuse it. This removes the 8^L
  all-digit codes of length L (L=3: 512 of 29,791).
- **Lowercase** (native handles only; work refs such as GitHub owners keep their case): handles are minted
  lowercase and lookups fold case (GraphQL; the CLI and bd door
  take ids as typed, and bd mints lowercase). The importer keeps ids as they are: bd's are lowercase by
  construction.
- **Random**: each symbol is drawn from `crypto/rand`, not cut from a content hash. The old `hashSuffix` fed only
  40 bits into an 8-symbol id, less than the id's space.
- **Length**: the shortest L in 3..8 for which the chance that one new mint hits any existing handle is at most
  1 in 1,000 (n / space(L) <= 0.001, space(L) = 31^L - 8^L), then retried on a hit, up to 8 symbols. This replaces
  the 25% birthday bound, which describes the whole set rather than one mint. Up to 29 issues: 3 symbols; up to
  919: 4; up to 28,596: 5; up to 887k: 6; up to 27.5M: 7.
- **Readable**: short and unambiguous to read, type and say letter by letter. Not syllables: a pronounceable scheme
  (CVCV) would cost length, and handles are read off screens far more than said.
- **Old ids keep working**: ids minted before (base36, any letters, all digits) stay valid, parse the same way, and
  are never renumbered.

## Prefixes and global references

New handles keep the project's prefix (`harbor-k4x`). That is bd's shape, and existing ids and every agent's
habits depend on it. A prefix is unique per authority (as now). Across authorities it is not, so a bare `web-k4x`
is a local reference. The global reference waits for stage 4 (`@owner/project/k4x`, with the prefix then implied
by the project). Until stage 4 decides whether a prefix is the project's handle or only a display, nothing global
is built on prefixes. Stage 1 changes only the random part.

## The issue number

`Issue.number` stays in the data and in the Linear-compatible GraphQL. Linear's schema requires it, and Linear
tools sort and filter by it. It is hidden from human surfaces (the web UI, `zm` CLI output, the bd door's human
output); bd-zm `--json` keeps the key so scripts do not break. It is authority-local: assigned on commit by the
authority, null on a replica's uncommitted item, never a federated fact and never an identifier. Agents that talk
to zm through Linear tools may still see "742"; zm's own surfaces always show the handle.

## Comments: edit, delete, redact

Comment rows gain `identity` (the author's identity ULID; today only a name and an address are stored, and names can
be renamed), `edited_at`, `deleted_at` and `deleted_by`, and the history gets kinds `comment_edited`,
`comment_deleted` and `comment_redacted`, plus a `comment_id` column on history rows (so an edit's `base` can name the comment's last
change). One migration; existing comments get the identity resolved from their address where it is known.

| act | who | what stays | event (appended) |
| --- | --- | --- | --- |
| edit | the author: any session of the author's identity, while it can still read the issue; not a guest | earlier texts, in history; the comment shows `edited_at` | `dev.zm.issue.comment_edited` |
| delete | the author (as for edit), or an `admin` on the issue's area, or the machine owner on the socket | a tombstone (`deleted_at`; text not shown). The text stays in history, readable by admins only | `dev.zm.issue.comment_deleted` |
| redact | an `admin` on the issue's area, or the machine owner on the socket | no text: the comment and every history row about it hold `[redacted]`; a history row records who redacted and when | `dev.zm.issue.comment_redacted` |

- **Moderators are not named to readers.** When a moderator (not the author) deletes or redacts, `deleted_by`,
  the history row's actor and the event's actor are shown only to admins: events go to an admin-only audience, and
  history shows `a moderator` to everyone else.
- **Edits race like status writes.** An edit carries `base` (the comment's last history id). If the comment changed
  since then, the edit is refused (`conflict`) instead of the last write winning. A replica's outbox replay of an
  edit or delete to a comment redacted meanwhile answers `gone` and is dropped. Redaction always wins.
- **The event log stays append-only.** Native issue events carry a comment's id, not its text
  (`dev.zm.issue.commented`), so redacting a native comment needs no event rewritten. A redaction appends
  `comment_redacted`, and every reader that keeps a copy (replica apply, webhook follower, snapshot, web UI)
  overwrites the comment by its id when it sees one. The text does reach one other log: native issues reach
  `zm work` through the native adapter, and the work module's `dev.zm.work.commented` events carry the body. Those
  stored events have their body field overwritten with `[redacted]`, keeping their id, type and order. That is
  the one in-place change, made only by a redaction, and the note does not pretend otherwise. A consumer that
  already read them is told to drop its copy by the redaction event.

### Where comment text lives, and what redaction guarantees

| copy | owner | redaction |
| --- | --- | --- |
| hot store: comment row, history rows | issues | overwritten (guaranteed) |
| the work module's mirror of native comments (`work_comments` in project.db) | work | overwritten (guaranteed) |
| `dev.zm.work.commented` events carrying the body | events | body field overwritten, id kept (guaranteed) |
| webhook queue (`webhook_queue`, up to 5,000 per hook) | webhooks | queued payloads overwritten before delivery (guaranteed) |
| team write records (`team_writes`, 30 days) | team | recorded answers overwritten (guaranteed) |
| replica copy of work comments and outbox (`outbox.body`, `result`, refused rows) | team, on each replica | purged when the replica applies the redaction event (best effort: a replica that never syncs again keeps its copy). Native issues are not proxied to replicas yet, so this row is about the work mirror's copies |
| agents' transcripts, MCP `work_get` answers already returned | the agent | cannot be recalled; documented |
| delivered webhook payloads | the receiver | cannot be recalled; the redaction event is delivered so the receiver can act; documented |
| `export` backups (17.9: file, iCloud, S3) | tracker home | cannot be rewritten; the next backup is clean; documented; restoring an old backup re-applies redactions from the store's redaction list |
| the frozen bd Dolt home kept after a zm-native move | tracker home | not touched (it is the rollback copy); documented; dropping the rollback copy is the cure |
| a re-import from bd or a backup | issues importer | refused for any comment on the redaction list (by comment id and source id) |
| GraphQL request memo and loaded web views | graphql, web | per request, and the web UI overwrites on the event |
| comment search (`LIKE` over the hot store; no FTS on comments) | issues | follows the store |

A **redaction list** (comment id, source id, who, when; no text) is kept outside the store it protects: in
registry.db per project, and appended to every `export` backup. A restore of an older store re-applies it, and an
import refuses any comment on it, so redacted text cannot come back.

## Session and project names (later stages)

- **Sessions**: an editable `name` beside the handle (`mini/4bb7545a`; its 8 characters are the first 8 of the
  runtime UUID). A name is an alias that resolves only while the session is live, and it must be unique on its
  machine among live sessions. Mail and commands resolve it at once and store the resolved address, so a reused
  name never redirects older mail. Names stay ASCII (`ValidName`), so no homoglyphs.
- **Projects and owners**: `@owner/project`, where an owner is a GitHub user or org bound by its numeric id.
  Decided in [owners-2026-11-01.md](owners-2026-11-01.md).

## Stages

1. **New issue handles and the hidden number.** Alphabet, a letter in every handle, lowercase, random, the new
   length rule, a blocklist. Old ids still parse. The number is hidden from the web UI and CLI. No migration.
   Tests: alphabet, no all-digit or blocked handles, length growth, old ids, case folding.
2. **Comment edit, delete, redact.** One migration (comment columns, history kinds, the redaction list). Store,
   history, events, REST, GraphQL (`commentUpdate`, `commentDelete`, `editedAt`), the redaction table above, edit
   `base`, authz (author identity, area admins, machine owner, guests, tombstone fields). Tests per row of the
   redaction table. No rollback once text is destroyed, which the CLI states before it redacts.
3. **Session names** (with the cockpit session).
4. **Owner namespaces and global references** (its own design first).

Stage 1 and stage 2 do not depend on each other. Stage 2's replica row concerns the work mirror's copies (round C,
merged); native issues on replicas come later. Neither needs stage 3 or 4. Replica-minted handles and children (above) land with offline creation, whenever
that is built.

## Open questions

- Should project owners be able to export a comment's pre-redaction text before redacting it (for an incident
  record)? Proposal: no. Redaction is for content that must not exist.
