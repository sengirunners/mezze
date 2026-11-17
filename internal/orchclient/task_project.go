package orchclient

import (
	"context"
	"net/http"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

// TaskStartIn is TaskStart placed through team project instead of the
// parent's (with r.Machine; SPEC 8 "Machine choice"). It sends no cwd from
// this checkout: the machine uses its own checkout of that project. An
// empty project is TaskStart.
func TaskStartIn(ctx context.Context, c *client.Client, project string, r orchapi.TaskStartRequest) (orchapi.Task, error) {
	if project == "" {
		return TaskStart(ctx, c, r)
	}
	var t orchapi.Task
	err := c.Do(ctx, http.MethodPost, "/v0/tasks", orchapi.TaskStartBody{TaskStartRequest: r, Project: project}, &t)
	return t, err
}

// TasksIn is Tasks whose starts go through project ("": Tasks).
func TasksIn(c *client.Client, project string) RunTasks {
	if project == "" {
		return Tasks(c)
	}
	return projectTasks{tasks{c}, project}
}

type projectTasks struct {
	tasks
	project string
}

func (p projectTasks) TaskStart(ctx context.Context, r orchapi.TaskStartRequest) (orchapi.Task, error) {
	return TaskStartIn(ctx, p.c, p.project, r)
}

// TaskStartWhere is TaskStartIn with where ("cloud": the provider's cloud
// computer, SPEC 8 "Cloud tasks"); it answers the task with its cloud view.
func TaskStartWhere(ctx context.Context, c *client.Client, project, where string, r orchapi.TaskStartRequest) (orchapi.TaskView, error) {
	if project == "" && r.Cwd == "" && r.Agent != orchapi.TaskAgentRun {
		r.Cwd = c.Cwd // as TaskStart
	}
	var t orchapi.TaskView
	err := c.Do(ctx, http.MethodPost, "/v0/tasks", orchapi.TaskStartBody{TaskStartRequest: r, Project: project, Where: where}, &t)
	return t, err
}

// TaskStatusView is TaskStatus with a cloud task's cloud view.
func TaskStatusView(ctx context.Context, c *client.Client, id string, wait int) (orchapi.TaskView, error) {
	var t orchapi.TaskView
	err := c.Do(ctx, http.MethodGet, client.Query("/v0/tasks/"+client.Seg(id), "wait_s", client.Itoa(int64(wait))), nil, &t)
	return t, err
}

// TaskCloud is the resolved project's cloud opt-in (GET /v0/tasks/cloud).
func TaskCloud(ctx context.Context, c *client.Client) (orchapi.TaskCloudSettings, error) {
	var s orchapi.TaskCloudSettings
	err := c.Do(ctx, http.MethodGet, "/v0/tasks/cloud", nil, &s)
	return s, err
}

// SetTaskCloud changes it (PUT /v0/tasks/cloud; the machine's owner).
func SetTaskCloud(ctx context.Context, c *client.Client, u orchapi.TaskCloudUpdate) (orchapi.TaskCloudSettings, error) {
	var s orchapi.TaskCloudSettings
	err := c.Do(ctx, http.MethodPut, "/v0/tasks/cloud", u, &s)
	return s, err
}
