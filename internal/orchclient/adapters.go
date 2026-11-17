package orchclient

import (
	"context"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

// Adapters: *client.Client had these calls as methods, and some code takes
// an interface that names them.

// Tasks is c as the RunTasks a script run uses (StartRun, CancelRun):
// *client.Client had these as methods before they moved here.
func Tasks(c *client.Client) RunTasks { return tasks{c} }

type tasks struct{ c *client.Client }

func (t tasks) TaskStart(ctx context.Context, r orchapi.TaskStartRequest) (orchapi.Task, error) {
	return TaskStart(ctx, t.c, r)
}

func (t tasks) TaskStatus(ctx context.Context, id string, wait int) (orchapi.Task, error) {
	return TaskStatus(ctx, t.c, id, wait)
}

func (t tasks) TaskCancel(ctx context.Context, id string) (orchapi.Task, error) {
	return TaskCancel(ctx, t.c, id)
}

// HolderLookup is the holder call as a method (ghlink's byRecipient takes one).
type HolderLookup interface {
	Holder(ctx context.Context, id string) (orchapi.HolderResponse, error)
}

// Holders is c as a HolderLookup.
func Holders(c *client.Client) HolderLookup { return holders{c} }

type holders struct{ c *client.Client }

func (h holders) Holder(ctx context.Context, id string) (orchapi.HolderResponse, error) {
	return Holder(ctx, h.c, id)
}

// HookCalls is the calls a hook makes, as methods (integrations.Daemon).
type HookCalls interface {
	Register(ctx context.Context, req orchapi.RegisterRequest) (orchapi.Session, error)
	SetState(ctx context.Context, id string, req orchapi.StateRequest) (orchapi.Session, error)
	Notice(ctx context.Context) (orchapi.Notice, error)
	Hook(ctx context.Context, req orchapi.HookRequest) (orchapi.HookResponse, error)
}

// Hooks is c as HookCalls.
func Hooks(c *client.Client) HookCalls { return hooks{c} }

type hooks struct{ c *client.Client }

func (h hooks) Register(ctx context.Context, req orchapi.RegisterRequest) (orchapi.Session, error) {
	return Register(ctx, h.c, req)
}

func (h hooks) SetState(ctx context.Context, id string, req orchapi.StateRequest) (orchapi.Session, error) {
	return SetState(ctx, h.c, id, req)
}

func (h hooks) Notice(ctx context.Context) (orchapi.Notice, error) { return HookNotice(ctx, h.c) }

func (h hooks) Hook(ctx context.Context, req orchapi.HookRequest) (orchapi.HookResponse, error) {
	return Hook(ctx, h.c, req)
}
