package orchclient

import (
	"context"
	"net/http"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

func TaskAgents(ctx context.Context, c *client.Client) (orchapi.TaskAgents, error) {
	var a orchapi.TaskAgents
	err := c.Do(ctx, http.MethodGet, "/v0/tasks/agents", nil, &a)
	return a, err
}
func TaskStart(ctx context.Context, c *client.Client, r orchapi.TaskStartRequest) (orchapi.Task, error) {
	// A script run makes its own checkout and refuses a cwd:
	// only an agent task starts in the caller's directory.
	if r.Cwd == "" && r.Agent != orchapi.TaskAgentRun {
		r.Cwd = c.Cwd
	}
	var t orchapi.Task
	err := c.Do(ctx, http.MethodPost, "/v0/tasks", r, &t)
	return t, err
}
func TaskStatus(ctx context.Context, c *client.Client, id string, wait int) (orchapi.Task, error) {
	var t orchapi.Task
	err := c.Do(ctx, http.MethodGet, client.Query("/v0/tasks/"+client.Seg(id), "wait_s", client.Itoa(int64(wait))), nil, &t)
	return t, err
}
func TaskCancel(ctx context.Context, c *client.Client, id string) (orchapi.Task, error) {
	var t orchapi.Task
	err := c.Do(ctx, http.MethodPost, "/v0/tasks/"+client.Seg(id)+"/cancel", struct{}{}, &t)
	return t, err
}

func TaskRead(ctx context.Context, c *client.Client, id, item string, offset int) (orchapi.TaskHistoryPage, error) {
	var page orchapi.TaskHistoryPage
	err := c.Do(ctx, http.MethodGet, client.Query("/v0/tasks/"+client.Seg(id)+"/read", "item", item, "offset", client.Itoa(int64(offset))), nil, &page)
	return page, err
}

// RemoteTasks is GET /v0/team/tasks: a team project's remote tasks, its
// waiting line first in order (SPEC 8 "Waiting line"); for its owner.
func RemoteTasks(ctx context.Context, c *client.Client, project string) (orchapi.RemoteTaskList, error) {
	var r orchapi.RemoteTaskList
	err := c.Do(ctx, http.MethodGet, client.Query("/v0/team/tasks", "project", project), nil, &r)
	return r, err
}
