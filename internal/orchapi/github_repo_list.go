package orchapi

// A project's GitHub repo list (SPEC "Webhooks and GitHub linking"):
// the repositories automations scoped to the project act on. Not generated:
// zm's own wire, beside potluck's GitHub types.

import "time"

// GitHubRepoList sources.
const (
	// RepoListSet: an admin set the list (PUT /v0/github/repo-list).
	RepoListSet = "list"
	// RepoListRemotes: never set, so the project's (and its areas') declared
	// GitHub remotes.
	RepoListRemotes = "remotes"
)

// GitHubRepoList is GET and PUT /v0/github/repo-list's answer.
type GitHubRepoList struct {
	Project string     `json:"project"` // the project's slug
	ID      string     `json:"id"`      // the project's ULID
	Repos   []string   `json:"repos"`   // owner/name, lower case, never null
	Source  string     `json:"source"`  // RepoListSet or RepoListRemotes
	Updated *time.Time `json:"updated,omitempty"`
}

// GitHubRepoListUpdate is PUT /v0/github/repo-list's body: Repos replaces
// the list (an empty list is a list); null or absent clears it, back to the
// project's remotes.
type GitHubRepoListUpdate struct {
	Repos *[]string `json:"repos"`
}
