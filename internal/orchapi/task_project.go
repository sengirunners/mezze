package orchapi

// TaskStartBody is POST /v0/tasks's body: a TaskStartRequest and, with
// machine, the team project to place the task through instead of the parent
// session's (SPEC 8 "Machine choice"); Where "cloud" runs it on the
// provider's cloud computer (SPEC 8 "Cloud tasks"). Not generated: potluck's
// client.TaskStart sends neither, so a caller naming one posts this.
type TaskStartBody struct {
	TaskStartRequest
	Project string `json:"project,omitempty"`
	Where   string `json:"where,omitempty"`
}
