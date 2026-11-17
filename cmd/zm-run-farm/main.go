// Command zm-run-farm is a SPEC 20 run target (kind "run") over zm's own
// remote script runs (SPEC 8 "Script runs", `zm exec`): each acquire is a
// task of agent "run" that the team project's authority places on a farm
// machine, which makes a clean checkout of exactly the commit, runs the
// script with SPEC 20's environment allowlist, enforces the deadline itself
// (exit 124) and removes the checkout. No ssh, no hub.
//
// A farm machine runs scripts as its own user, beside that user's
// credentials, so the class is isolation "none", credentials true: zm sends
// it only the remote head of the binding's branch, acquires it only from the
// owner's socket, and the machine itself refuses a commit that is not what
// the ref resolves to on the remote.
//
// It talks to the local daemon through potluck/kit/client, as zm exec does, and
// keeps one small record per handle (for list and the 24 h not_found rule)
// under state_dir.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/client"
	zmconfig "github.com/sengirunners/potluck/kit/config"
)

// main answers one request and exits 0, a refusal included: Potluck reads a
// driver's answer by one rule (protocol.ReadAnswer, Potluck #167), exactly
// one JSON value on stdout and exit 0; anything else is refused as internal.
// Nothing else is ever written to stdout.
func main() {
	in, _ := io.ReadAll(os.Stdin)
	_ = answer(os.Stdout, in, daemonTasks)
	os.Exit(0)
}

// answer writes serve's answer to in as the driver's whole stdout.
func answer(w io.Writer, in []byte, open func(project string) (tasks, error)) error {
	return json.NewEncoder(w).Encode(serve(in, open))
}

// serve answers one SPEC 11 envelope.
func serve(in []byte, open func(project string) (tasks, error)) map[string]any {
	var req struct {
		Verb   string            `json:"verb"`
		ID     string            `json:"id"`
		Args   json.RawMessage   `json:"args"`
		Config map[string]string `json:"config"`
	}
	var res any
	err := json.Unmarshal(in, &req)
	if err != nil {
		err = refuse("bad_request", "invalid request: %v", err)
	} else {
		res, err = dispatch(context.Background(), req.Verb, req.ID, req.Args, req.Config, open)
	}
	if err != nil {
		var e refusal
		if !errors.As(err, &e) {
			e = refusal{"internal", err.Error()}
		}
		if e.msg == "" { // an empty message is a broken answer (protocol.ReadAnswer)
			e.msg = "refused (" + e.code + ")"
		}
		return map[string]any{"ok": false, "error": map[string]string{"code": e.code, "message": e.msg}}
	}
	return map[string]any{"ok": true, "result": res}
}

// tasks is the part of zm's task API the driver uses.
type tasks interface {
	Health(ctx context.Context) error
	Start(ctx context.Context, actor string, req orchapi.TaskStartRequest) (orchapi.Task, error)
	Status(ctx context.Context, id string, wait int) (orchapi.Task, error)
	Cancel(ctx context.Context, id string) (orchapi.Task, error)
}

// daemonTasks is the local daemon, through its socket.
func daemonTasks(project string) (tasks, error) {
	cfg, err := zmconfig.LoadClient()
	if err != nil {
		return nil, err
	}
	c := client.New(cfg)
	c.As, c.Project, c.ProjectFrom = "", project, "flag" // the owner on the socket
	return clientTasks{c}, nil
}

type clientTasks struct{ c *client.Client }

func (t clientTasks) Health(ctx context.Context) error { _, err := t.c.Health(ctx); return err }

func (t clientTasks) Start(ctx context.Context, actor string, req orchapi.TaskStartRequest) (orchapi.Task, error) {
	c := *t.c
	c.As = actor // the parent of the run: the session the acquire is made for
	c.Cwd = ""   // a script run makes its own checkout; the server refuses a cwd on one
	return orchclient.TaskStart(ctx, &c, req)
}

func (t clientTasks) Status(ctx context.Context, id string, wait int) (orchapi.Task, error) {
	return orchclient.TaskStatus(ctx, t.c, id, wait)
}

func (t clientTasks) Cancel(ctx context.Context, id string) (orchapi.Task, error) {
	return orchclient.TaskCancel(ctx, t.c, id)
}
