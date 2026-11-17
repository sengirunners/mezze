package orchapi

import (
	"time"
)

// EventSummaryCreated's data is SummaryCreated.
const EventSummaryCreated = "dev.zm.summary.created"

// EventSummaryScheduleChanged's data is ProjectHint: read the project's
// summary config again.
const EventSummaryScheduleChanged = "dev.zm.summary.schedule_changed"

// SummaryCreated is dev.zm.summary.created: the new summary's id and day.
// Fields in key order, so the bytes match the map it replaced.
type SummaryCreated struct {
	Day string `json:"day"`
	ID  string `json:"id"`
}

// SummaryConfig is an opt-in daily digest on the project's hosting daemon.
type SummaryConfig struct {
	Project       string     `json:"project"`
	Enabled       bool       `json:"enabled"`
	Hour          int        `json:"hour"`
	Minute        int        `json:"minute"`
	Timezone      string     `json:"timezone"`
	Version       string     `json:"version,omitempty"`
	LastAttempt   *time.Time `json:"last_attempt,omitempty"`
	LastGenerated *time.Time `json:"last_generated,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

type SummaryRequest struct {
	Day string `json:"day,omitempty"`
}

// ProjectSummary contains work titles and counts, never mail, comments or
// transcripts. Notices counts, by a plain label, the machine notices of the
// day that went to no person's inbox (a green CI result, a lease expiry, an
// old notice the mail sweep acked).
type ProjectSummary struct {
	ID            string             `json:"id"`
	Project       string             `json:"project"`
	Day           string             `json:"day"`
	Timezone      string             `json:"timezone"`
	ConfigVersion string             `json:"config_version"`
	Generated     time.Time          `json:"generated"`
	Body          string             `json:"body"`
	Counts        map[WorkStatus]int `json:"counts"`
	Notices       map[string]int     `json:"notices,omitempty"`
	Partial       bool               `json:"partial"`
	Notes         []string           `json:"notes"`
}

type SummaryList struct {
	Summaries []ProjectSummary `json:"summaries"`
}
