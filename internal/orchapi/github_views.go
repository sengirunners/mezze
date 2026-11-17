package orchapi

import (
	"encoding/json"
	"time"
)

// GitHubRepository is an explicitly mapped repository the caller may read.
type GitHubRepository struct {
	Project string `json:"project"`
	Slug    string `json:"slug"`
	Area    string `json:"area,omitempty"`
	Repo    string `json:"repo"`
}

type GitHubRepositories struct {
	Repositories []GitHubRepository `json:"repositories"`
}

// GitHubData is a bounded cached read, never a caller-selected upstream request.
// Payload follows the GitHub response for the fixed resource named by Resource.
type GitHubData struct {
	Project   string           `json:"project"`
	Repo      string           `json:"repo"`
	Resource  string           `json:"resource"`
	HeadSHA   string           `json:"head_sha,omitempty"`
	Payload   json.RawMessage  `json:"payload,omitempty"`
	FetchedAt *time.Time       `json:"fetched_at,omitempty"`
	Stale     bool             `json:"stale"`
	Partial   bool             `json:"partial"`
	Error     *GitHubReadError `json:"error,omitempty"`
}

type GitHubReadError struct {
	Code       string    `json:"code"`
	Message    string    `json:"message"`
	RetryAfter time.Time `json:"retry_after"`
}
