// Package store is the automation record brick's interface (SPEC 18.6): one
// project's records, their revisions and health, and its homes' claims. The
// driver built is sqlite (the project's project.db); the second named is
// Postgres. storetest is the conformance suite every driver passes.
//
// A driver stores; it does not decide who may write (the automations
// module's home rule) nor whether a definition is valid (its validators).
// It owns what must be atomic: a revision's rev compare-and-set, a write
// id's single effect, history bounds, a claim's holder and wanted install.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/authz"
)

// Bounds a driver keeps.
const (
	// HistoryRevisions and HistoryBytes bound one record's history: the
	// oldest revisions go first; the newest is always kept.
	HistoryRevisions = 50
	HistoryBytes     = 1 << 20
	// TombstoneFor is how long a tombstone is kept: from the delete, or
	// from the home's health post that applied it, whichever is later.
	TombstoneFor = 30 * 24 * time.Hour
)

// Revision ops.
const (
	OpCreate = "create"
	OpUpdate = "update"
	OpDelete = "delete"
)

var (
	// ErrNotFound: no such record (or home claim).
	ErrNotFound = errors.New("not found")
	// ErrChanged: the record's rev, or a claim's holder, is not the one the
	// write expected.
	ErrChanged = errors.New("changed since")
	// ErrDeleted: the record is a tombstone; only health may still be posted.
	ErrDeleted = errors.New("deleted")
	// ErrWriteIDUsed: the write id already made another record's revision.
	ErrWriteIDUsed = errors.New("write id already used")
	// ErrNotWanted: a holder change names an install that is not the
	// home's wanted install.
	ErrNotWanted = errors.New("not the wanted install")
	// ErrWantedZ3: the wanted install keeps its definitions in the Mezze app
	// (store z3); it must switch to zm first.
	ErrWantedZ3 = errors.New("the wanted install keeps its definitions in its own database (store z3)")
)

// ExistsError is a create whose home and task id another record holds.
type ExistsError struct{ ID string }

func (e *ExistsError) Error() string { return fmt.Sprintf("record %s has this home and task id", e.ID) }

// Definition is what a definition write changes, and what a revision keeps.
type Definition struct {
	Name      string                               `json:"name"`
	Enabled   bool                                 `json:"enabled"`
	ResumeRev *int64                               `json:"resume_rev,omitempty"`
	Owner     string                               `json:"owner,omitempty"`
	Fields    json.RawMessage                      `json:"fields"`
	Body      string                               `json:"body"`
	Settings  map[string]orchapi.AutomationSetting `json:"settings,omitempty"`
}

// Record is one automation record.
type Record struct {
	ID     string
	Area   string
	Kind   string
	Home   string
	TaskID string
	Definition
	Rev       int64
	Created   time.Time
	Updated   time.Time
	UpdatedBy string
	Deleted   *time.Time
	Health    *orchapi.AutomationHealth
}

// Revision is one entry of a record's history.
type Revision struct {
	Rev     int64
	Op      string
	At      time.Time
	By      string
	WriteID string
	Def     Definition
	Bytes   int
}

// Write is who makes a definition write, when, and its write id ("" none):
// a write repeated with one write id makes one revision and returns the
// first one's result. Resume sets the record's resume_rev to the rev this
// write makes (an approved resume; nothing else sets it).
type Write struct {
	ID     string
	By     string
	At     time.Time
	Resume bool
}

// Query narrows a list.
type Query struct {
	Home, Kind string
	Deleted    bool // include tombstones
	Limit      int  // 0: no limit
}

// Claim is an install's claim of a home.
type Claim struct {
	Install, Store, Path string
}

// Home is one home's claim row.
type Home struct {
	Home string
	Claim
	Claimed time.Time // the holder's first claim
	Updated time.Time
	WriteID string // the holder change that made this holder ("" a first claim)
	Wanted  *Wanted
	// LastHealth is the newest health post of the home's records (zero none).
	LastHealth time.Time
}

// Wanted is the install refused a home's claim most lately.
type Wanted struct {
	Claim
	First time.Time
}

// Store is one project's automation records.
type Store interface {
	// List answers the records sc reads (AreaSQL on the record's area),
	// ordered by id; truncated when Limit cut it.
	List(ctx context.Context, sc authz.Scope, project string, q Query) (recs []Record, truncated bool, err error)
	// Get answers one record, tombstones included (ErrNotFound).
	Get(ctx context.Context, id string) (Record, error)
	// Create stores r at rev 1 (r.ID set by the caller). Another record
	// with r's home and task id is *ExistsError; a repeat of w.ID returns
	// the record as its first create left it.
	Create(ctx context.Context, r Record, w Write) (Record, error)
	// Update runs change on the record and stores what it returns as the
	// next revision, if the definition changed. ErrDeleted on a tombstone;
	// ErrChanged when another write landed first. A repeat of w.ID on this
	// record returns the record as that revision left it, without running
	// change; on another record it is ErrWriteIDUsed.
	Update(ctx context.Context, id string, w Write, change func(Record) (Definition, error)) (Record, error)
	// Delete makes the record a tombstone in a new revision; an existing
	// tombstone is returned as it is. rev, when not 0, is the revision the
	// delete was made from: ErrChanged when the record is at another.
	Delete(ctx context.Context, id string, rev int64, w Write) (Record, error)
	// History answers the record's revisions, newest first.
	History(ctx context.Context, id string) ([]Revision, error)
	// PutHealth stores h as the record's health (a tombstone's too) and
	// answers the one it replaced (nil none). A post on a tombstone whose
	// applied_rev reaches the tombstone's rev keeps it TombstoneFor from h's
	// receipt.
	PutHealth(ctx context.Context, id string, h orchapi.AutomationHealth, received time.Time) (*orchapi.AutomationHealth, error)
	// Homes answers every home's claim, by home.
	Homes(ctx context.Context) ([]Home, error)
	// Home answers one home's claim (ErrNotFound: never claimed).
	Home(ctx context.Context, home string) (Home, error)
	// Claim claims home for c.Install. held is true when it holds the home
	// after the call: a first claim, or the holder's repeat (store and path
	// updated, first claim time and write id kept). Otherwise the claim is
	// refused and c recorded as the home's wanted install: a different
	// install replaces the one recorded, the same install's repeat updates
	// its store and path and keeps its first time.
	Claim(ctx context.Context, home string, c Claim, at time.Time) (h Home, held bool, err error)
	// SetHolder gives home to its wanted install to, if from holds it now
	// (ErrChanged otherwise; ErrNotWanted when to is not the wanted install;
	// ErrWantedZ3 when it claimed with store z3). The new holder's claim
	// time is at and it keeps writeID; the wanted install is cleared. A
	// repeat with writeID, while to holds the home under it, returns the
	// home unchanged.
	SetHolder(ctx context.Context, home, from, to, writeID string, at time.Time) (Home, error)
	// Purge drops tombstones kept past TombstoneFor, with their history and
	// health; it answers how many.
	Purge(ctx context.Context, now time.Time) (int, error)
	// Scrub overwrites texts in bodies, fields, history and apply errors
	// with core.Redacted: in every record for texts at least minLen long,
	// and at any length in the records named in ids.
	Scrub(ctx context.Context, ids, texts []string, minLen int) error
}
