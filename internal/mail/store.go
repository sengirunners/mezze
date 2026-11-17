package mail

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

var storeMigrations = []store.Migration{
	{Kind: store.KindProject, Name: "mail/0001_init", Script: `
CREATE TABLE mail (
	id        TEXT PRIMARY KEY,           -- ULID
	area      TEXT NOT NULL DEFAULT '',
	sender    TEXT NOT NULL,
	to_addr   TEXT NOT NULL,              -- what the sender wrote, canonical
	recipient TEXT NOT NULL,              -- where it was delivered
	via       TEXT NOT NULL,
	ref       TEXT NOT NULL DEFAULT '',
	ref_area  TEXT NOT NULL DEFAULT '',   -- the ref's area, to hide refs a reader cannot see
	kind      TEXT NOT NULL,
	needs     TEXT NOT NULL,
	body      TEXT NOT NULL,
	reply_to  TEXT NOT NULL DEFAULT '',
	thread    TEXT NOT NULL,              -- id of the thread's first mail
	created   INTEGER NOT NULL,
	acked_by  TEXT NOT NULL DEFAULT '',
	acked_at  INTEGER,
	orphan_of TEXT NOT NULL DEFAULT '',   -- this row is the group copy of that mail
	orphaned  INTEGER NOT NULL DEFAULT 0  -- 1: a group copy of this mail exists
);
CREATE INDEX mail_recipient ON mail (recipient, acked_at);
CREATE INDEX mail_sender ON mail (sender);
CREATE INDEX mail_thread ON mail (thread);
CREATE INDEX mail_ref ON mail (ref);
CREATE TABLE mail_seen (                  -- announcements a session has seen
	mail_id TEXT NOT NULL,
	address TEXT NOT NULL,
	PRIMARY KEY (mail_id, address)
);`},
	// Open mail by recipient: the inbox's needs=human-to-this-machine term
	// and the orphan sweep are ranges over session addresses, which the
	// (recipient, acked_at) index cannot narrow to unacked rows.
	{Kind: store.KindProject, Name: "mail/0002_open", Script: `CREATE INDEX mail_open ON mail (recipient, needs) WHERE acked_at IS NULL;`},
	// Taking mail settles its group copy (orphan_of = it) in the same
	// statement; without this every ack scanned the mail table, holding a
	// team file's write lock (the round C load test's profile).
	{Kind: store.KindProject, Name: "mail/0003_orphan_of", Script: `CREATE INDEX mail_orphan_of ON mail (orphan_of);`},
}

var personalMigrations = []store.Migration{
	{Kind: store.KindProject, Name: "mail/0004_personal", Script: `
ALTER TABLE mail ADD COLUMN audience TEXT NOT NULL DEFAULT '';
ALTER TABLE mail ADD COLUMN participants TEXT NOT NULL DEFAULT '[]';
ALTER TABLE mail ADD COLUMN provenance TEXT NOT NULL DEFAULT '{}';
CREATE INDEX mail_created ON mail(created DESC,id DESC);
CREATE INDEX mail_thread_created ON mail(thread,created,id);
CREATE TABLE mail_requests (actor TEXT NOT NULL, request_id TEXT NOT NULL, payload_hash TEXT NOT NULL, mail_id TEXT NOT NULL, PRIMARY KEY(actor,request_id));
CREATE TABLE mail_import_sink (mail_id TEXT PRIMARY KEY, payload_hash TEXT NOT NULL);
`},
	// A reply in a thread under restricted mail is restricted too,
	// at any depth; the flag is set at the send from the parent's.
	{Kind: store.KindProject, Name: "mail/0005_parent_restricted", Script: `
ALTER TABLE mail ADD COLUMN parent_restricted INTEGER NOT NULL DEFAULT 0;
WITH RECURSIVE r(id) AS (
  SELECT id FROM mail WHERE recipient = 'owners' OR needs = 'human'
  UNION SELECT m.id FROM mail m JOIN r ON m.reply_to = r.id)
UPDATE mail SET parent_restricted = 1 WHERE reply_to != '' AND reply_to IN (SELECT id FROM r);
`},
	// Mail to several people (SPEC 9): each copy carries the member list and
	// the id of the send's first copy.
	{Kind: store.KindProject, Name: "mail/0006_members", Script: `
ALTER TABLE mail ADD COLUMN members TEXT NOT NULL DEFAULT '[]';
ALTER TABLE mail ADD COLUMN batch TEXT NOT NULL DEFAULT '';
`},
	// Read marks (SPEC 9): one per person per conversation, max-merged.
	{Kind: store.KindProject, Name: "mail/0007_read_marks", Script: `
CREATE TABLE mail_read_marks (
	identity TEXT NOT NULL,     -- the person, human:<name>
	conv     TEXT NOT NULL,     -- the client's conversation key, opaque, at most 200 bytes
	upto_ms  INTEGER NOT NULL,  -- read up to here (ms, mail's created clock)
	updated  INTEGER NOT NULL,
	PRIMARY KEY (identity, conv)
);`},
	{Kind: store.KindRegistry, Name: "mail/0001_imports", Script: `CREATE TABLE mail_imports (
 source_instance TEXT NOT NULL, record_kind TEXT NOT NULL, source_id TEXT NOT NULL,
 payload_hash TEXT NOT NULL, project TEXT NOT NULL, mail_id TEXT NOT NULL,
 PRIMARY KEY(source_instance,record_kind,source_id));`},
	{Kind: store.KindRegistry, Name: "mail/0002_import_identity", Script: `
ALTER TABLE mail_imports RENAME TO mail_import_legacy;
CREATE TABLE mail_imports (
 importer_identity TEXT NOT NULL, source_instance TEXT NOT NULL, record_kind TEXT NOT NULL, source_id TEXT NOT NULL,
 payload_hash TEXT NOT NULL, project TEXT NOT NULL, mail_id TEXT NOT NULL,
 PRIMARY KEY(importer_identity,source_instance,record_kind,source_id));
CREATE TABLE mail_import_legacy_state (
 source_instance TEXT NOT NULL, record_kind TEXT NOT NULL, source_id TEXT NOT NULL,
 state TEXT NOT NULL, importer_identity TEXT NOT NULL DEFAULT '',
 PRIMARY KEY(source_instance,record_kind,source_id));
CREATE INDEX mail_import_legacy_held ON mail_import_legacy_state(state);
`},
}

// row is a stored mail plus the bookkeeping columns that are not on the wire.
type row struct {
	orchapi.Mail
	RefArea  string
	OrphanOf string
	Replayed bool
	Orphaned bool
	// AskNotice: zm's word on an ask's outcome; never orphaned to a machine.
	AskNotice bool
	// ParentRestricted: this mail replies, at any depth, to restricted mail
	// (an ask's decision or withdraw reply, and replies to those), so it
	// follows the restricted rule too (column parent_restricted).
	ParentRestricted bool
}

const cols = `id, area, sender, to_addr, recipient, via, ref, ref_area, kind, needs, body, reply_to, thread,
	created, acked_by, acked_at, orphan_of, orphaned, audience, participants, provenance,
	parent_restricted, members, batch`

type storedProvenance struct {
	Origin           *orchapi.MailOrigin     `json:"origin,omitempty"`
	LegacyDelivery   *orchapi.LegacyDelivery `json:"legacy_delivery,omitempty"`
	ReplyUnavailable string                  `json:"reply_unavailable,omitempty"`
	// HeldUntil (ms) holds a for-you digest item until its 08:00 Pacific.
	HeldUntil int64 `json:"held_until,omitempty"`
	// HeldFor: an ask held for an outside party (SPEC 16, ask.for), stored
	// at its send so the mail.sent event carries it to replicas, whose copy
	// has no asks table to ask.
	HeldFor   string `json:"held_for,omitempty"`
	AskNotice bool   `json:"ask_notice,omitempty"`
}

type scanner interface{ Scan(...any) error }

func scanRow(sc scanner, project string) (row, error) {
	var r row
	var created int64
	var acked sql.NullInt64
	var needs, participants, members, provenance, heldFor string
	err := sc.Scan(&r.ID, &r.Area, &r.From, &r.To, &r.Recipient, &r.Via, &r.Ref, &r.RefArea, &r.Kind, &needs, &r.Body,
		&r.ReplyTo, &r.Thread, &created, &r.AckedBy, &acked, &r.OrphanOf, &r.Orphaned, &r.Audience, &participants, &provenance, &r.ParentRestricted,
		&members, &r.Batch)
	if err == nil {
		err = json.Unmarshal([]byte(participants), &r.Participants)
		if err == nil {
			err = json.Unmarshal([]byte(members), &r.Members)
		}
		var extra storedProvenance
		if err == nil {
			err = json.Unmarshal([]byte(provenance), &extra)
		}
		r.Origin, r.LegacyDelivery, r.ReplyUnavailable, heldFor, r.AskNotice = extra.Origin, extra.LegacyDelivery, extra.ReplyUnavailable, extra.HeldFor, extra.AskNotice
		if extra.HeldUntil != 0 {
			t := store.FromMillis(extra.HeldUntil)
			r.HeldUntil = &t
		}
	}
	r.Project = project
	r.Needs = orchapi.Needs(needs)
	if personal(r) && r.From != r.Recipient {
		r.ForYou = orchapi.ForYouKnow
		if r.HeldUntil != nil {
			r.ForYou = orchapi.ForYouDigest
		}
	}
	r.Created = store.FromMillis(created)
	r.AckedAt = store.NullMillis(acked)
	if r.AckedAt == nil {
		r.HeldFor = heldFor // taken mail waits on nobody
	}
	return r, err
}

type querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

// getRow loads one mail by its full id, ignoring scope (callers check).
func getRow(ctx context.Context, q querier, project, id string) (row, error) {
	r, err := scanRow(q.QueryRowContext(ctx, `SELECT `+cols+` FROM mail WHERE id = ?`, id), project)
	if errors.Is(err, sql.ErrNoRows) {
		return r, api.NotFound("mail", id)
	}
	return r, err
}

// listRows runs SELECT cols FROM mail WHERE <where>.
func listRows(ctx context.Context, q querier, project, where string, args ...any) ([]row, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+cols+` FROM mail WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []row
	for rows.Next() {
		r, err := scanRow(rows, project)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func insertRow(ctx context.Context, tx *sql.Tx, r row) error {
	participants, _ := json.Marshal(r.Participants)
	if r.Participants == nil {
		participants = []byte("[]")
	}
	members, _ := json.Marshal(r.Members)
	if r.Members == nil {
		members = []byte("[]")
	}
	held := int64(0)
	if r.HeldUntil != nil {
		held = store.Millis(*r.HeldUntil)
	}
	provenance, _ := json.Marshal(storedProvenance{Origin: r.Origin, LegacyDelivery: r.LegacyDelivery, ReplyUnavailable: r.ReplyUnavailable, HeldUntil: held, HeldFor: r.HeldFor, AskNotice: r.AskNotice})
	_, err := tx.ExecContext(ctx, `INSERT INTO mail (id, area, sender, to_addr, recipient, via, ref, ref_area, kind, needs,
		body, reply_to, thread, created, orphan_of, audience, participants, provenance, parent_restricted, members, batch)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Area, r.From, r.To, r.Recipient, r.Via, r.Ref, r.RefArea, r.Kind, string(r.Needs), r.Body, r.ReplyTo,
		r.Thread, store.Millis(r.Created), r.OrphanOf, r.Audience, string(participants), string(provenance), r.ParentRestricted,
		string(members), r.Batch)
	return err
}

// scoped is one project database the caller may read, with its area filter.
type scoped struct {
	id    string
	db    *sql.DB
	where string // AreaSQL for read
	args  []any
}

// readable lists the projects in sc the caller (address me) may read, with
// their mail filters (readWhere).
func (s *Service) readable(sc authz.Scope, me string, personal ...bool) []scoped {
	var out []scoped
	for _, p := range s.d.Store.Projects(sc, authz.Read) {
		w, a := readWhere(sc, p.ID, me)
		if len(personal) > 0 && !personal[0] {
			w = "(" + w + ") AND audience = ''"
		}
		out = append(out, scoped{id: p.ID, db: p.DB, where: w, args: a})
	}
	return out
}

// restrictedSQL selects mail that stays with those who can act on it: the
// owners mailbox and needs=human mail. In an area the caller may only read
// (a guest), such mail is shown only when the caller sent or received it.
// A reply under such mail (any depth) is restricted too (an ask's decision reply
// quotes the question and the owner's answer).
const restrictedSQL = "(recipient = 'owners' OR needs = 'human' OR " + parentRestrictedSQL + ")"

// parentRestrictedSQL: the mail replies, at any depth, to restricted mail.
const parentRestrictedSQL = "parent_restricted = 1"

// readWhere is the mail filter for one project: every area sc may read,
// minus, in the areas it may read but not operate in, restricted mail the
// caller (me) is not a party to.
func readWhere(sc authz.Scope, project, me string) (string, []any) {
	w, a := areaReadWhere(sc, project, me)
	return "(" + w + ") AND (audience = '' OR (audience = 'participants' AND EXISTS (SELECT 1 FROM json_each(mail.participants) WHERE value = ?)))", append(a, me)
}

func areaReadWhere(sc authz.Scope, project, me string) (string, []any) {
	w, a := sc.AreaSQL(project, "area", authz.Read)
	if sc.Relation(authz.Object{Project: project}).Allows(authz.Operate) {
		return w, a
	}
	ow, oa := sc.AreaSQL(project, "area", authz.Operate)
	args := append(append(append([]any{}, oa...), a...), me, me, me)
	return "(" + ow + " OR (" + w + " AND NOT (" + restrictedSQL + " AND sender != ? AND recipient != ? AND to_addr != ?)))", args
}

// restricted reports whether m is restricted mail (restrictedSQL).
func restricted(m row) bool {
	return m.Recipient == "owners" || m.Needs == orchapi.NeedsHuman || m.ParentRestricted
}

// query runs where (AND the area filter) in every readable project, newest
// first, at most limit rows in total.
func (s *Service) query(ctx context.Context, sc authz.Scope, me string, limit int, where func(p scoped) (string, []any, bool)) ([]row, error) {
	var all []row
	for _, p := range s.readable(sc, me, authz.PersonalMailReader(core.SubjectOf(ctx))) {
		w, args, ok := where(p)
		if !ok {
			continue
		}
		rs, err := listRows(ctx, p.db, p.id, "("+w+") AND "+p.where+" ORDER BY created DESC, id DESC LIMIT ?",
			append(append(args, p.args...), limit)...)
		if err != nil {
			return nil, err
		}
		all = append(all, rs...)
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].Created.Equal(all[j].Created) {
			return all[i].Created.After(all[j].Created)
		}
		return all[i].ID > all[j].ID
	})
	if len(all) > limit {
		all = all[:limit]
	}
	s.markHeld(ctx, all)
	return all, nil
}

// find resolves a mail id or a unique suffix of 6+ characters inside sc.
// Anything that does not resolve (malformed, too short, hidden, missing) is
// exactly api.NotFound("mail", raw).
func (s *Service) find(ctx context.Context, sc authz.Scope, me, raw string) (row, *sql.DB, error) {
	nf := api.NotFound("mail", raw)
	id := strings.ToUpper(strings.TrimSpace(raw))
	if len(id) < 6 || len(id) > 26 || !alnum(id) {
		return row{}, nil, nf
	}
	var hits []row
	var db *sql.DB
	for _, p := range s.readable(sc, me, authz.PersonalMailReader(core.SubjectOf(ctx))) {
		var rs []row
		var err error
		if len(id) == 26 {
			rs, err = listRows(ctx, p.db, p.id, "id = ? AND "+p.where+" LIMIT 2", append([]any{id}, p.args...)...)
		} else {
			rs, err = listRows(ctx, p.db, p.id, "id LIKE ? AND "+p.where+" LIMIT 2", append([]any{"%" + id}, p.args...)...)
		}
		if err != nil {
			return row{}, nil, err
		}
		if len(rs) > 0 {
			db = p.db
		}
		hits = append(hits, rs...)
	}
	switch len(hits) {
	case 0:
		return row{}, nil, nf
	case 1:
		s.markHeld(ctx, hits)
		return hits[0], db, nil
	}
	return row{}, nil, api.Errorf(api.CodeBadRequest, "mail id %q matches more than one mail", raw).
		WithHint("use more characters of the id")
}

func alnum(s string) bool {
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

func limitOf(n int) int {
	switch {
	case n <= 0:
		return 50
	case n > 500:
		return 500
	}
	return n
}

// newID mints a mail id (a ULID with random increments: gaps reveal nothing).
func newID() string { return ids.New() }
