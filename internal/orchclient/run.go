package orchclient

import (
	"context"
	"errors"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// RunTasks is the part of a Client a script run uses (zm exec).
type RunTasks interface {
	TaskStart(ctx context.Context, r orchapi.TaskStartRequest) (orchapi.Task, error)
	TaskStatus(ctx context.Context, id string, wait int) (orchapi.Task, error)
	TaskCancel(ctx context.Context, id string) (orchapi.Task, error)
}

// runCancelWait bounds finding the run after the caller gave up, and then
// its cancel's confirmation; runConfirmEvery is the status poll meanwhile.
// After a signal the resend gets runResendWait; past that, lookups of
// runLookupWait each, every runLookupEvery. Variables for tests only.
var (
	runCancelWait   = 30 * time.Second
	runResendWait   = 10 * time.Second
	runLookupWait   = 5 * time.Second
	runLookupEvery  = time.Second
	runConfirmEvery = 500 * time.Millisecond
)

// StartRun starts a script run (SPEC 8 "Script runs"). When ctx ends while
// the start is in flight, the authority may already have queued it, so it
// cancels the run its client_request_id names before returning ctx's error:
// no script is left running for a caller that gave up. It finds that run by
// sending the same start again, not a lookup: the first start may still be
// on its way, and a lookup that answers not_found before it lands would
// leave it running, while a resend is the same start (made at most once)
// whichever arrives first.
//
// The resend can itself be the start, when it overtook the first: on the
// authority's own machine that start runs its checkout inside the request,
// longer than any bound here. So a resend that gets no answer is followed by
// lookups (which never make a run) until one finds the run or runCancelWait
// passes, and the run found is cancelled.
func StartRun(ctx context.Context, c RunTasks, r orchapi.TaskStartRequest) (orchapi.Task, error) {
	if r.ClientRequestID == "" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "a script run needs a client_request_id")
	}
	r.Lookup = false
	t, err := c.TaskStart(ctx, r)
	if err == nil || ctx.Err() == nil {
		return t, err
	}
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runCancelWait)
	t, err = findRun(fctx, c, r)
	cancel()
	if err != nil {
		return orchapi.Task{}, errors.Join(ctx.Err(), runFindError(err, r.ClientRequestID))
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), runCancelWait)
	defer cancel()
	if _, cerr := cancelFound(cctx, c, t, r.ClientRequestID); cerr != nil {
		return orchapi.Task{}, errors.Join(ctx.Err(), cerr)
	}
	return orchapi.Task{}, ctx.Err()
}

// findRun finds the run r names after a signal: the same start again, then,
// when that gets no answer within runResendWait, lookups until ctx ends. A
// lookup that answers not_found is tried again: a start may still land.
func findRun(ctx context.Context, c RunTasks, r orchapi.TaskStartRequest) (orchapi.Task, error) {
	rctx, cancel := context.WithTimeout(ctx, runResendWait)
	t, err := c.TaskStart(rctx, r)
	cancel()
	if err == nil || runAnswered(err) {
		return t, err
	}
	r.Lookup = true
	for {
		lctx, cancel := context.WithTimeout(ctx, runLookupWait)
		var lerr error
		t, lerr = c.TaskStart(lctx, r)
		cancel()
		if lerr == nil || runAnswered(lerr) && !api.IsCode(lerr, api.CodeNotFound) {
			return t, lerr
		}
		if api.IsCode(lerr, api.CodeNotFound) || ctx.Err() == nil {
			err = lerr
		}
		select {
		case <-ctx.Done():
			return orchapi.Task{}, err
		case <-time.After(runLookupEvery):
		}
	}
}

// runAnswered: err is the authority's answer about the run, not a lost or
// late request.
func runAnswered(err error) bool {
	for _, code := range []string{api.CodeNotFound, api.CodeConflict, api.CodeBadRequest, api.CodeForbidden} {
		if api.IsCode(err, code) {
			return true
		}
	}
	return false
}

// CancelRun cancels the run r.ClientRequestID names, from the parent session
// that started it, and waits (at most 30 s, or ctx's deadline) until its
// status confirms it: ended or refused. It finds the run with a lookup (the
// same start with lookup set), which never makes one: an unknown request,
// or one whose record expired, is not_found; a changed request is conflict.
// An unconfirmed cancel is an error that says so.
func CancelRun(ctx context.Context, c RunTasks, r orchapi.TaskStartRequest) (orchapi.Task, error) {
	if r.ClientRequestID == "" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "a script run needs a client_request_id")
	}
	r.Lookup = true
	return cancelRun(ctx, c, r)
}

func cancelRun(ctx context.Context, c RunTasks, r orchapi.TaskStartRequest) (orchapi.Task, error) {
	t, err := c.TaskStart(ctx, r)
	if err != nil {
		return orchapi.Task{}, runFindError(err, r.ClientRequestID)
	}
	return cancelFound(ctx, c, t, r.ClientRequestID)
}

// runFindError says what finding request rid's run answered.
func runFindError(err error, rid string) error {
	switch {
	case api.IsCode(err, api.CodeConflict):
		return api.Errorf(api.CodeConflict, "request %s was a different run", rid).
			WithHint("cancel with the run's --ref, --commit, --timeout, script and arguments (--machine is not compared)")
	case api.IsCode(err, api.CodeNotFound):
		return api.Errorf(api.CodeNotFound, "no run from this session has request %s: nothing cancelled", rid).
			WithHint("cancel from the session that started it; a run's record is kept 30 days")
	}
	return err
}

// cancelFound cancels run t (unless settled) and confirms it.
func cancelFound(ctx context.Context, c RunTasks, t orchapi.Task, rid string) (orchapi.Task, error) {
	if !runSettled(t) {
		var err error
		if t, err = c.TaskCancel(ctx, t.ID); err != nil {
			return orchapi.Task{}, err
		}
	}
	return confirmRun(ctx, c, t, rid)
}

// confirmRun polls t's status until it is ended or refused, at most
// runCancelWait (or until ctx ends).
func confirmRun(ctx context.Context, c RunTasks, t orchapi.Task, rid string) (orchapi.Task, error) {
	deadline := time.Now().Add(runCancelWait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for !runSettled(t) {
		if time.Now().Add(runConfirmEvery).After(deadline) {
			return t, api.Errorf(api.CodeUnavailable, "the cancel of %s (request %s) was not confirmed: it is still %s", t.ID, rid, t.State).
				WithHint("it may still run: check with zm task status " + t.ID + ", or run zm exec --cancel again")
		}
		select {
		case <-ctx.Done():
			return t, api.Errorf(api.CodeUnavailable, "the cancel of %s (request %s) was not confirmed: it is still %s", t.ID, rid, t.State)
		case <-time.After(runConfirmEvery):
		}
		if cur, err := c.TaskStatus(ctx, t.ID, 0); err == nil {
			t = cur
		}
	}
	return t, nil
}

// runSettled: a run status no later report changes.
func runSettled(t orchapi.Task) bool { return t.State == "ended" || t.State == orchapi.TaskRefused }
