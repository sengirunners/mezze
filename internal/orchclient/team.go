package orchclient

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// StatusWait bounds how long SetStatusSettled waits for the authority.
const StatusWait = 3 * time.Second

// SetStatusSettled is SetStatus, and on a replica (the answer names its
// outbox write) waits up to wait for the authority's answer. A refusal is an
// error with the authority's reason; queued is true when the write is still
// pending at the end (it applies when the authority is reachable).
func SetStatusSettled(ctx context.Context, c *client.Client, id string, req orchapi.StatusRequest, wait time.Duration) (it orchapi.WorkItem, queued bool, err error) {
	it, err = SetStatus(ctx, c, id, req)
	if err != nil || it.Write == "" {
		return it, false, err
	}
	end := time.Now().Add(wait)
	for {
		es, err := c.Outbox(ctx, it.Project, true)
		if err != nil {
			return it, true, nil // the write is queued either way
		}
		state, msg := "", ""
		for _, e := range es {
			if e.ID == it.Write {
				state, msg = e.State, e.Error
			}
		}
		switch state {
		case "", api.OutboxAcked: // gone: acked and the copy shows it
			return it, false, nil
		case api.OutboxConflict, api.OutboxFailed:
			return it, false, api.Errorf(api.CodeConflict, "the authority refused %s %s: %s", it.ID, req.Status, msg).WithHint("zm outbox shows the refused write")
		}
		if time.Now().After(end) {
			return it, true, nil
		}
		select {
		case <-ctx.Done():
			return it, true, nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// EnsureT3 is POST /v0/team/machines/{machine}/t3: start or reuse the Mezze app on
// machine and mint a one-time pairing credential (answered once).
func EnsureT3(ctx context.Context, c *client.Client, machine string, req orchapi.T3EnsureRequest) (orchapi.T3Environment, error) {
	var r orchapi.T3Environment
	return r, c.Do(ctx, http.MethodPost, "/v0/team/machines/"+client.Seg(machine)+"/t3", req, &r)
}

// Health is GET /v0/health with the parts zm's modules add (orchapi.Health).
func Health(ctx context.Context, c *client.Client) (orchapi.Health, error) {
	var h orchapi.Health
	return h, c.Do(ctx, http.MethodGet, "/v0/health", nil, &h)
}

// SetMachinePerson is POST /v0/team/machines/{machine}/person: mark machine
// as one person's own (or, with Person "", a team machine again).
func SetMachinePerson(ctx context.Context, c *client.Client, machine string, req orchapi.MachinePersonRequest) (orchapi.MachinePerson, error) {
	var r orchapi.MachinePerson
	return r, c.Do(ctx, http.MethodPost, "/v0/team/machines/"+client.Seg(machine)+"/person", req, &r)
}

// MachinePeople is GET /v0/team/machines/person: the machines of project
// that are one person's own.
func MachinePeople(ctx context.Context, c *client.Client, project string) (orchapi.MachinePersonList, error) {
	var r orchapi.MachinePersonList
	return r, c.Do(ctx, http.MethodGet, "/v0/team/machines/person?project="+url.QueryEscape(project), nil, &r)
}
