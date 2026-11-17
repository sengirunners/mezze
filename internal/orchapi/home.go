package orchapi

import (
	"time"
)

// Backup kinds. Every adapter gets BackupExport (a JSON Lines snapshot of
// the tracker's items from the mirror); an adapter whose describe lists a
// native kind gets that too: BackupSqlite for native issues (copies of the
// issue store and project.db a restore can use).
const (
	BackupExport = "export"
	BackupSqlite = "sqlite"
)

// HomeView is where a tracker's data lives, as `zm tracker where` shows it.
type HomeView struct {
	Target string `json:"target"` // the adapter ("native" for zm's own issues)
	Kind   string `json:"kind"`   // "service": the tracker's own store or service
}

// BackupView is one backup of a tracker.
type BackupView struct {
	ID           string    `json:"id"`     // what backup rm/run take: the target as typed (icloud, file:///x)
	Target       string    `json:"target"` // canonical
	Kind         string    `json:"kind"`   // export or sqlite
	Path         string    `json:"path"`   // where this tracker's copy lives
	EverySeconds int       `json:"every_seconds"`
	Keep         int       `json:"keep,omitempty"` // copies kept (export, sqlite)
	Created      time.Time `json:"created"`
	// The last attempt and the last success.
	LastRun    *time.Time `json:"last_run,omitempty"`
	LastOK     *time.Time `json:"last_ok,omitempty"`
	LastError  string     `json:"last_error,omitempty"`
	Failures   int        `json:"failures,omitempty"`    // consecutive
	AgeSeconds float64    `json:"age_seconds,omitempty"` // since LastOK (0 = never)
	Items      int        `json:"items,omitempty"`       // export: items written
	Bytes      int64      `json:"bytes,omitempty"`
	Running    bool       `json:"running,omitempty"`
	Due        *time.Time `json:"due,omitempty"`
	// Newest is when the newest copy on disk was made; nil: none is there,
	// whatever LastOK says. Copies is how many there are.
	Newest *time.Time `json:"newest,omitempty"`
	Copies int        `json:"copies,omitempty"`
	// ScrubPending: a redaction finished after older copies were written and
	// they are not gone yet (sqlite; the daemon keeps trying).
	ScrubPending bool `json:"scrub_pending,omitempty"`
}

// BackupBrief is a tracker's newest copy, as tracker ls shows it. A native
// tracker's counts only sqlite copies when it has a sqlite backup: an export
// cannot restore a store.
type BackupBrief struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Newest is nil when no copy is on disk; AgeSeconds is then 0.
	Newest       *time.Time `json:"newest,omitempty"`
	AgeSeconds   float64    `json:"age_seconds,omitempty"`
	Overdue      bool       `json:"overdue,omitempty"` // older than twice its schedule
	Failing      bool       `json:"failing,omitempty"`
	ScrubPending bool       `json:"scrub_pending,omitempty"`
}

// TrackerWhere is GET /v0/trackers/{prefix}/home.
type TrackerWhere struct {
	Tracker TrackerView  `json:"tracker"`
	Home    HomeView     `json:"home"`
	Backups []BackupView `json:"backups"`
	Kinds   []string     `json:"backup_kinds"` // backup kinds this tracker offers
	Notes   []string     `json:"notes,omitempty"`
}

// AddBackupRequest is POST /v0/trackers/{prefix}/backups.
type AddBackupRequest struct {
	Target       string `json:"target"`
	Kind         string `json:"kind,omitempty"` // export or sqlite; default: the tracker's native kind, else export
	EverySeconds int    `json:"every_seconds,omitempty"`
	Keep         int    `json:"keep,omitempty"`
	RunNow       bool   `json:"run_now,omitempty"`
}

// BackupResult answers backup add and run.
type BackupResult struct {
	Backup  BackupView `json:"backup"`
	Ran     bool       `json:"ran"`
	Seconds float64    `json:"seconds,omitempty"`
	Notes   []string   `json:"notes,omitempty"`
}
