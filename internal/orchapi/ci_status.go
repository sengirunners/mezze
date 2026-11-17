package orchapi

import "github.com/sengirunners/potluck/runners"

// CIStatus is the shared read model for zm ci status and the Compute repo rows.
// Issues and Partial must be displayed: an incomplete read is never an all-clear.
type CIStatus struct {
	State   string         `json:"state"` // configured, not_configured, no_repositories
	Repos   []CIRepoStatus `json:"repos"`
	Issues  []string       `json:"issues"`
	Partial bool           `json:"partial"`
}

type CIRepoStatus struct {
	Repo      string   `json:"repo"`
	RunsOn    string   `json:"runs_on"` // complete YAML line; empty when no serving machine is known
	JobLimitS int      `json:"job_limit_s"`
	Summary   string   `json:"summary"`
	Issues    []string `json:"issues"`
	Jobs      []CIJob  `json:"jobs"` // newest first, including waiting/refused/unseen
	Partial   bool     `json:"partial"`
}

type CIJob struct {
	runners.JobView
	Name         string `json:"name,omitempty"`
	URL          string `json:"url,omitempty"`
	GitHubStatus string `json:"github_status,omitempty"`
}
