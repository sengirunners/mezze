package orchapi

import "encoding/json"

// Automation records (SPEC 18.6; internal/automations): the definition of
// a job a Mezze app home fires, held at the project's authority. JSON is snake_case
// (the Mezze app's client, packages/zm-client/src/automationRecords.ts, reads it).

// Automation kinds: one entry per kind the brick validates.
const (
	AutomationKindAutomation = "automation"
)

// Home claim stores: where the claiming install keeps its definitions.
const (
	AutomationStoreZm = "zm" // this record store (Z3_AUTOMATION_STORE=zm)
	AutomationStoreZ3 = "z3" // the install's own database; zm refuses definition writes
)

// Automation health states (SPEC 18.6 "Health").
const (
	AutomationHealthy   = "healthy"
	AutomationDegraded  = "degraded"
	AutomationPaused    = "paused"
	AutomationUnapplied = "unapplied"
	AutomationConflict  = "conflict"
)

// Automation events: data AutomationEvent; the health event only when the
// state changes.
const (
	EventAutomationCreated = "dev.zm.automation.created"
	EventAutomationUpdated = "dev.zm.automation.updated"
	EventAutomationDeleted = "dev.zm.automation.deleted"
	EventAutomationHealth  = "dev.zm.automation.health"
)

// AutomationEvent is an automation event's data: never the definition.
type AutomationEvent struct {
	Project string `json:"project"`
	Area    string `json:"area,omitempty"`
	ID      string `json:"id"`
	Rev     int64  `json:"rev"`
	State   string `json:"state,omitempty"` // the health event's new state
}

// AutomationSetting is one typed setting. Value is checked against Type;
// Options are an enum's values.
type AutomationSetting struct {
	Type    string          `json:"type"` // string | number | boolean | enum
	Value   json.RawMessage `json:"value"`
	Options []string        `json:"options,omitempty"`
	Help    string          `json:"help,omitempty"`
}

// AutomationEnvelope is a record without its fields and body (lists).
type AutomationEnvelope struct {
	ID        string            `json:"id"`
	Project   string            `json:"project"` // slug
	Area      string            `json:"area,omitempty"`
	Kind      string            `json:"kind"`
	Name      string            `json:"name"`
	Enabled   bool              `json:"enabled"`
	ResumeRev *int64            `json:"resume_rev,omitempty"`
	Owner     string            `json:"owner,omitempty"`
	Home      string            `json:"home"`
	TaskID    string            `json:"task_id"`
	Rev       int64             `json:"rev"`
	Created   string            `json:"created"`
	Updated   string            `json:"updated"`
	UpdatedBy string            `json:"updated_by,omitempty"`
	Deleted   string            `json:"deleted,omitempty"`
	Health    *AutomationHealth `json:"health,omitempty"`
}

// Automation is a whole record (GET /v0/automations/{id} and every write).
type Automation struct {
	AutomationEnvelope
	Fields   json.RawMessage              `json:"fields"` // a JSON object
	Body     string                       `json:"body"`
	Settings map[string]AutomationSetting `json:"settings,omitempty"`
}

// AutomationList is GET /v0/automations. Holder is the install holding the
// claim of the home named by ?home= ("" none, or no home named).
type AutomationList struct {
	Automations []AutomationEnvelope `json:"automations"`
	Holder      string               `json:"holder,omitempty"`
	Truncated   bool                 `json:"truncated,omitempty"`
}

// AutomationCreate is POST /v0/automations. Project falls back to
// ?project=; TaskID defaults to automation:<id>; Enabled to true; Kind to
// automation.
type AutomationCreate struct {
	Project  string                       `json:"project,omitempty"`
	Area     string                       `json:"area,omitempty"`
	Kind     string                       `json:"kind,omitempty"`
	Home     string                       `json:"home"`
	TaskID   string                       `json:"task_id,omitempty"`
	Install  string                       `json:"install"`
	Name     string                       `json:"name"`
	Enabled  *bool                        `json:"enabled,omitempty"`
	Owner    string                       `json:"owner,omitempty"`
	Fields   json.RawMessage              `json:"fields,omitempty"`
	Body     string                       `json:"body"`
	Settings map[string]AutomationSetting `json:"settings,omitempty"`
}

// AutomationPatch is PATCH /v0/automations/{id}: Rev is the revision read.
// A key left out is unchanged; name, enabled, owner (null clears) and body
// are replaced whole; fields and settings take a JSON merge patch (RFC
// 7396), or with ReplaceFields fields is the whole new value (a merge patch
// cannot set a null).
type AutomationPatch struct {
	Rev           int64           `json:"rev"`
	Install       string          `json:"install"`
	Name          *string         `json:"name,omitempty"`
	Enabled       *bool           `json:"enabled,omitempty"`
	Owner         json.RawMessage `json:"owner,omitempty"`
	Body          *string         `json:"body,omitempty"`
	Fields        json.RawMessage `json:"fields,omitempty"`
	Settings      json.RawMessage `json:"settings,omitempty"`
	ReplaceFields bool            `json:"replace_fields,omitempty"`
}

// AutomationRevision is one revision in a record's history: the definition
// as that write left it. The write id that made it is kept, never shown.
type AutomationRevision struct {
	Rev       int64                        `json:"rev"`
	Op        string                       `json:"op"` // create | update | delete
	Updated   string                       `json:"updated"`
	UpdatedBy string                       `json:"updated_by,omitempty"`
	Name      string                       `json:"name"`
	Enabled   bool                         `json:"enabled"`
	ResumeRev *int64                       `json:"resume_rev,omitempty"`
	Owner     string                       `json:"owner,omitempty"`
	Fields    json.RawMessage              `json:"fields"`
	Body      string                       `json:"body"`
	Settings  map[string]AutomationSetting `json:"settings,omitempty"`
}

// AutomationHistory is GET /v0/automations/{id}/history, newest first.
type AutomationHistory struct {
	Revisions []AutomationRevision `json:"revisions"`
}

// AutomationHealth is what the home last posted (PUT
// /v0/automations/{id}/health). Never a run's error text, output or prompt.
type AutomationHealth struct {
	Install       string `json:"install"`
	State         string `json:"state"`
	AppliedRev    int64  `json:"applied_rev"`
	ApplyError    string `json:"apply_error,omitempty"` // 300 bytes at most
	FailureStreak int    `json:"failure_streak,omitempty"`
	Secret        string `json:"secret,omitempty"` // none | set | missing
	HookChanged   bool   `json:"hook_changed,omitempty"`
	LastRunAt     string `json:"last_run_at,omitempty"`
	LastRunStatus string `json:"last_run_status,omitempty"`
	NextRunAt     string `json:"next_run_at,omitempty"`
	AsOf          string `json:"as_of"`
	Received      string `json:"received,omitempty"` // set by zm
}

// AutomationHomeClaim is PUT /v0/automation-homes/{home}?project=.
type AutomationHomeClaim struct {
	Install string `json:"install"`
	Store   string `json:"store"` // zm | z3
	Path    string `json:"path"`  // the install's userdata path
}

// AutomationWanted is the one install refused a home's claim most lately.
type AutomationWanted struct {
	Install string `json:"install"`
	Store   string `json:"store"`
	Path    string `json:"path,omitempty"`
	First   string `json:"first"` // its first refused claim
}

// AutomationHome is one home's claim: the holder, the time it first
// claimed, its last health post, and the wanted install.
type AutomationHome struct {
	Home       string            `json:"home"`
	Install    string            `json:"install"`
	Store      string            `json:"store"`
	Path       string            `json:"path,omitempty"`
	Claimed    string            `json:"claimed,omitempty"`
	Updated    string            `json:"updated,omitempty"`
	LastHealth string            `json:"last_health,omitempty"`
	Wanted     *AutomationWanted `json:"wanted,omitempty"`
}

// AutomationHomes is GET /v0/automation-homes.
type AutomationHomes struct {
	Homes []AutomationHome `json:"homes"`
}
