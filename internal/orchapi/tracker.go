package orchapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// Beads binding modes.
const (
	TrackerModeNew    = "new"    // bd's embedded Dolt under $ZM_HOME/trackers/<project>/<prefix>
	TrackerModeDir    = "dir"    // an existing .beads directory
	TrackerModeServer = "server" // a shared Dolt server
)

var trackerPrefixRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidTrackerPrefix reports whether s has a tracker prefix's shape.
func ValidTrackerPrefix(s string) bool { return trackerPrefixRe.MatchString(s) }

// TrackerPrefixTaken is the conflict when a project already binds prefix.
func TrackerPrefixTaken(slug, prefix string) error {
	return api.Errorf(api.CodeConflict, "project %s already has a tracker with prefix %q", slug, prefix).
		WithHint("zm tracker rm " + prefix + " first, or pass --prefix")
}

// FindTracker is the binding with prefix in trs.
func FindTracker(trs []api.Tracker, prefix string) (api.Tracker, bool) {
	for _, t := range trs {
		if t.Prefix == prefix {
			return t, true
		}
	}
	return api.Tracker{}, false
}

// TrackerKey is an in-memory map key for a project's tracker.
func TrackerKey(project, prefix string) string { return project + "\x00" + prefix }

// TrackerBinding fingerprints what a tracker's mirror depends on: another
// adapter or config is another tracker, whose mirror starts over. A home
// operation flips a binding only if it is still the one it read.
func TrackerBinding(tr api.Tracker) string {
	b, _ := json.Marshal(struct {
		A, P string
		C    map[string]string
		E    string `json:",omitempty"` // env inherit reaches other servers; "" keeps old fingerprints
	}{tr.Adapter, tr.Prefix, tr.Config, tr.Env})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:12])
}

// AddTrackerRequest is POST /v0/trackers. The project is the resolved
// write target (Zm-Project, else the cwd rules).
type AddTrackerRequest struct {
	Adapter string `json:"adapter"`          // "native", or <name> (zm-work-<name>)
	Prefix  string `json:"prefix,omitempty"` // default: the adapter name
	Area    string `json:"area,omitempty"`
	// beads
	Mode     string `json:"mode,omitempty"`     // new (default when the project has no beads), dir, server
	Dir      string `json:"dir,omitempty"`      // mode dir: the .beads directory (or the repo holding it)
	Server   string `json:"server,omitempty"`   // mode server: host:port
	Database string `json:"database,omitempty"` // mode server
	User     string `json:"user,omitempty"`     // mode server
	// PasswordFile is a file the daemon copies into $ZM_HOME/secrets
	// (0600). The password itself never travels in a request.
	PasswordFile string `json:"password_file,omitempty"`
	// AllowMigrate lets the pinned bd migrate a --server database whose
	// beads schema is older than the one it expects (otherwise: read-only).
	AllowMigrate bool              `json:"allow_migrate,omitempty"`
	Readonly     bool              `json:"readonly,omitempty"`
	Env          string            `json:"env,omitempty"`    // "inherit": no environment allowlist (refused for server)
	Config       map[string]string `json:"config,omitempty"` // extra adapter config (github: repos, token_file, host)
	// WaitSeconds bounds the wait for the mirror's first full sync (default
	// 300); a slower one keeps syncing in the background.
	WaitSeconds int `json:"wait_seconds,omitempty"`
}

// TrackerView is one binding as tracker ls shows it: never a password.
type TrackerView struct {
	Project  string            `json:"project"`
	Slug     string            `json:"slug"`
	Adapter  string            `json:"adapter"`
	Prefix   string            `json:"prefix"`
	Area     string            `json:"area,omitempty"`
	Mode     string            `json:"mode,omitempty"`
	Env      string            `json:"env"` // "allowlist" or "inherit"
	Readonly bool              `json:"readonly,omitempty"`
	Config   map[string]string `json:"config,omitempty"`
	Password bool              `json:"password,omitempty"` // a password is stored (in $ZM_HOME/secrets)
	Data     string            `json:"data,omitempty"`     // where zm keeps this tracker's own files
	Sync     *WorkSync         `json:"sync,omitempty"`
	Backup   *BackupBrief      `json:"backup,omitempty"` // its newest copy; nil: it has no backup
}

// TrackerEnvAllowlist is TrackerView.Env for a tracker without env = "inherit".
const TrackerEnvAllowlist = "allowlist"

// TrackerCheck is one conformance check (zm-work-conformance --read-only).
type TrackerCheck struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Skipped bool   `json:"skipped,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// SchemaCheck is the read-only first contact with a --server database.
type SchemaCheck struct {
	Server   int  `json:"server"`   // its beads schema version
	Pinned   int  `json:"pinned"`   // the version the pinned bd expects; 0: --bin runs another bd (not compared)
	Migrated bool `json:"migrated"` // --allow-migrate moved it to Pinned
	Readonly bool `json:"readonly"` // a mismatch left the binding read-only
}

// TrackerResult is POST /v0/trackers and POST /v0/trackers/{prefix}/test.
type TrackerResult struct {
	// OK: every check passed and the first sync did not fail (tracker test
	// exits 1 otherwise; a failing add is an error and rolls back).
	OK      bool           `json:"ok"`
	Tracker TrackerView    `json:"tracker"`
	Created bool           `json:"created,omitempty"` // setup made something new (its undo runs if the add fails): a new beads database for --new, the local .beads/ metadata for --server
	Schema  *SchemaCheck   `json:"schema,omitempty"`
	Checks  []TrackerCheck `json:"checks"`
	// Items mirrored after the first sync, and how long it took.
	Items       int     `json:"items"`
	SyncSeconds float64 `json:"sync_seconds"`
	// Synced is false when the first full sync outlasted the wait; it goes
	// on in the background (zm work sources shows it).
	Synced bool     `json:"synced"`
	Notes  []string `json:"notes,omitempty"`
}

// TrackerList is GET /v0/trackers.
type TrackerList struct {
	Trackers []TrackerView `json:"trackers"`
}

// RemoveTrackerResult is DELETE /v0/trackers/{prefix}.
type RemoveTrackerResult struct {
	Tracker TrackerView `json:"tracker"`
}

// TrackerFailure is a tracker whose mirror syncs keep failing (the notice
// line names it, SPEC 10).
type TrackerFailure struct {
	Project  string    `json:"project"`
	Slug     string    `json:"slug"`
	Prefix   string    `json:"prefix"`
	Since    time.Time `json:"since"`
	Failures int       `json:"failures"`
}
