package orchapi

// PR lifecycle events (SPEC 18.2 "PR lifecycle events"): the
// GitHub linker publishes these in the linked project's log, so `zm events`
// and `zm webhook` carry them under the same read rules as work events.
const (
	EventGitHubPRReady         = "dev.zm.github.pr.ready"
	EventGitHubPRHeadMoved     = "dev.zm.github.pr.head_moved"
	EventGitHubPRClosed        = "dev.zm.github.pr.closed"
	EventGitHubPRChecksSettled = "dev.zm.github.pr.checks_settled"
)

// GitHubPREvent is the data of a dev.zm.github.pr.* event.
type GitHubPREvent struct {
	Repo    string `json:"repo"` // owner/name, lower case
	Number  int    `json:"number"`
	HeadSHA string `json:"head_sha"`
	Base    string `json:"base"`
	Draft   bool   `json:"draft"`
	Title   string `json:"title"`
	Author  string `json:"author"` // GitHub login, not a zm address
	// Items are the work ids the linker has tied this PR to (may be empty).
	Items []string `json:"items"`
	URL   string   `json:"html_url"`
	State string   `json:"state"` // open | merged | closed
	// PrevHeadSHA is the head before a head_moved.
	PrevHeadSHA string `json:"prev_head_sha,omitempty"`
	// Conclusion is checks_settled's verdict: success | failure | cancelled.
	Conclusion string `json:"conclusion,omitempty"`
}
