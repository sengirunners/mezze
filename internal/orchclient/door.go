package orchclient

// The Mezze app door (SPEC 18.4). project names the team project; on a member's
// machine the daemon sends each call on to its authority.

import (
	"context"
	"net/http"
	"net/url"
	"strconv"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

func doorPath(p, project string, q url.Values) string {
	if q == nil {
		q = url.Values{}
	}
	q.Set("project", project)
	return p + "?" + q.Encode()
}

// Door is GET /v0/door.
func Door(ctx context.Context, c *client.Client, project string) (orchapi.DoorStatus, error) {
	var st orchapi.DoorStatus
	return st, c.Do(ctx, http.MethodGet, doorPath("/v0/door", project, nil), nil, &st)
}

// SetDoor is PUT /v0/door.
func SetDoor(ctx context.Context, c *client.Client, project string, on bool) (orchapi.DoorStatus, error) {
	var st orchapi.DoorStatus
	return st, c.Do(ctx, http.MethodPut, doorPath("/v0/door", project, nil), orchapi.DoorToggle{On: on}, &st)
}

// DoorTasks is GET /v0/door/tasks.
func DoorTasks(ctx context.Context, c *client.Client, project string) ([]orchapi.DoorTask, error) {
	var l orchapi.DoorTaskList
	return l.Tasks, c.Do(ctx, http.MethodGet, doorPath("/v0/door/tasks", project, nil), nil, &l)
}

// PutDoorTask is PUT /v0/door/tasks/{task}.
func PutDoorTask(ctx context.Context, c *client.Client, project, task string, req orchapi.DoorTaskRequest) (orchapi.DoorTask, error) {
	var t orchapi.DoorTask
	return t, c.Do(ctx, http.MethodPut, doorPath("/v0/door/tasks/"+client.Seg(task), project, nil), req, &t)
}

// RemoveDoorTask is DELETE /v0/door/tasks/{task}.
func RemoveDoorTask(ctx context.Context, c *client.Client, project, task string) (orchapi.DoorTask, error) {
	var t orchapi.DoorTask
	return t, c.Do(ctx, http.MethodDelete, doorPath("/v0/door/tasks/"+client.Seg(task), project, nil), nil, &t)
}

// RotateDoorTask is POST /v0/door/tasks/{task}/rotate.
func RotateDoorTask(ctx context.Context, c *client.Client, project, task string) (orchapi.DoorTask, error) {
	var t orchapi.DoorTask
	return t, c.Do(ctx, http.MethodPost, doorPath("/v0/door/tasks/"+client.Seg(task)+"/rotate", project, nil), nil, &t)
}

// DoorDeliveries is GET /v0/door/tasks/{task}/deliveries (limit 0: the
// default).
func DoorDeliveries(ctx context.Context, c *client.Client, project, task string, limit int) ([]orchapi.DoorDelivery, error) {
	var l orchapi.DoorDeliveryList
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	return l.Deliveries, c.Do(ctx, http.MethodGet, doorPath("/v0/door/tasks/"+client.Seg(task)+"/deliveries", project, q), nil, &l)
}
