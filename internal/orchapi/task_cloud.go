package orchapi

// Cloud tasks (SPEC 8 "Cloud tasks"): TaskStartBody's where "cloud" runs the
// task on the provider's cloud computer through this machine's own CLI login.
// Not generated: zm's own wire, beside potluck's task types.

// TaskWhereCloud is TaskStartBody.Where for a cloud task.
const TaskWhereCloud = "cloud"

// TaskCloud is a cloud task's provider task as its watcher last saw it.
type TaskCloud struct {
	Agent       string `json:"agent"`                 // claude or codex
	Environment string `json:"environment,omitempty"` // the project's environment for it
	ID          string `json:"id,omitempty"`          // the provider's task or session id
	URL         string `json:"url,omitempty"`
	// Status: submitting, submitted, pending, ready, failed or unknown.
	Status string `json:"status"`
}

// TaskView is a task as POST /v0/tasks and GET /v0/tasks/{id} answer it:
// Cloud is set on a cloud task.
type TaskView struct {
	Task
	Cloud *TaskCloud `json:"cloud,omitempty"`
}

// TaskCloudSettings is a project's [cloud] table: the provider environment
// a cloud task of each runtime runs in (GET and PUT /v0/tasks/cloud). A
// runtime without one is not opted in: no cloud task sends it code.
type TaskCloudSettings struct {
	Project string `json:"project,omitempty"` // answers only: the project's slug
	Claude  string `json:"claude,omitempty"`  // claude cloud environment id, or "default"
	Codex   string `json:"codex,omitempty"`   // codex cloud exec --env
}

// TaskCloudUpdate is PUT /v0/tasks/cloud's body: a present field replaces
// that runtime's environment, "" takes the opt-in back.
type TaskCloudUpdate struct {
	Claude *string `json:"claude,omitempty"`
	Codex  *string `json:"codex,omitempty"`
}
