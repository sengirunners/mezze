package orchapi

import (
	"time"
)

// OffsiteRequest is PUT /v0/backup. Keys are secret references (keychain:,
// file:, env:), never the secrets.
type OffsiteRequest struct {
	Target       string `json:"target"` // s3://bucket[/folder] or file:///path
	Endpoint     string `json:"endpoint,omitempty"`
	Region       string `json:"region,omitempty"`
	AccessKey    string `json:"access_key,omitempty"`
	SecretKey    string `json:"secret_key,omitempty"`
	Encrypt      string `json:"encrypt,omitempty"`
	EncryptSalt  string `json:"encrypt_salt,omitempty"`
	Home         string `json:"home"` // this home's id: its folder in the target
	EverySeconds int    `json:"every_seconds,omitempty"`
	Keep         int    `json:"keep,omitempty"`
	KeepHourly   int    `json:"keep_hourly,omitempty"`
	KeepDaily    int    `json:"keep_daily,omitempty"`
	RunNow       bool   `json:"run_now,omitempty"`
}

// HomeBackup is a home's off-site backup at a glance: in GET /v0/health
// (local callers), a replica's authority row of GET /v0/work/sources, and the
// watchdog heartbeat. LastOK is when the newest verified set was taken (null:
// never).
type HomeBackup struct {
	Kind         string     `json:"kind"` // s3 or file
	EverySeconds int        `json:"every_s"`
	LastOK       *time.Time `json:"last_ok"`
	Bytes        int64      `json:"bytes,omitempty"` // the newest set, as stored
	Failing      bool       `json:"failing,omitempty"`
	Failures     int        `json:"failures,omitempty"`
	Error        string     `json:"error,omitempty"`
}

// OffsiteView is GET /v0/backup.
type OffsiteView struct {
	Configured   bool          `json:"configured"`
	Target       string        `json:"target,omitempty"` // its URL and endpoint host; no secret
	Home         string        `json:"home,omitempty"`
	AccessKey    string        `json:"access_key,omitempty"` // references, not values
	SecretKey    string        `json:"secret_key,omitempty"`
	Encrypt      string        `json:"encrypt,omitempty"`
	EncryptSalt  string        `json:"encrypt_salt,omitempty"`
	EverySeconds int           `json:"every_seconds,omitempty"`
	Keep         int           `json:"keep,omitempty"`
	KeepHourly   int           `json:"keep_hourly,omitempty"`
	KeepDaily    int           `json:"keep_daily,omitempty"`
	Status       *HomeBackup   `json:"status,omitempty"`
	Set          string        `json:"set,omitempty"` // the newest good set's stamp
	LastRun      *time.Time    `json:"last_run,omitempty"`
	Running      bool          `json:"running,omitempty"`
	ScrubDue     *time.Time    `json:"scrub_due,omitempty"` // a redaction waits for a fresh set
	Watchdog     *WatchdogView `json:"watchdog,omitempty"`
}

// OffsiteRunResult is POST /v0/backup/run (and PUT's run_now).
type OffsiteRunResult struct {
	Ran      bool     `json:"ran"`
	Set      string   `json:"set,omitempty"`
	Files    int      `json:"files,omitempty"`
	Uploaded int      `json:"uploaded,omitempty"` // files sent
	Copied   int      `json:"copied,omitempty"`   // unchanged files copied on the target
	Bytes    int64    `json:"bytes,omitempty"`    // the set, as stored
	Seconds  float64  `json:"seconds"`
	Deleted  []string `json:"deleted,omitempty"` // sets retention removed
	Error    string   `json:"error,omitempty"`
}

// OffsiteSet is one set on the target (GET /v0/backup/sets).
type OffsiteSet struct {
	Stamp    string `json:"stamp"`
	Complete bool   `json:"complete"`
	Bytes    int64  `json:"bytes"`
	Objects  int    `json:"objects"`
}

// OffsiteSets is GET /v0/backup/sets.
type OffsiteSets struct {
	Target string       `json:"target"`
	Home   string       `json:"home"`
	Sets   []OffsiteSet `json:"sets"`
}

// WatchdogRequest is PUT /v0/backup/watchdog. Secret is a secret reference.
type WatchdogRequest struct {
	URL    string `json:"url"`
	Secret string `json:"secret"`
	Home   string `json:"home"`
}

// HeartbeatBrief is the watchdog heartbeat's last send, in GET /v0/health
// (local callers) next to the backup.
type HeartbeatBrief struct {
	LastSent   *time.Time `json:"last_sent"`
	LastStatus int        `json:"last_status,omitempty"`
	Failing    bool       `json:"failing,omitempty"`
	Failures   int        `json:"failures,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// WatchdogView is the heartbeat's setting and its last send.
type WatchdogView struct {
	URL        string     `json:"url"`
	Home       string     `json:"home"`
	Secret     string     `json:"secret"` // a reference
	LastSent   *time.Time `json:"last_sent,omitempty"`
	LastStatus int        `json:"last_status,omitempty"`
	LastError  string     `json:"last_error,omitempty"`
	Failures   int        `json:"failures,omitempty"`
}
