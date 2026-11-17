package orchclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// fakeRuns is an authority whose one run never ends, whatever the cancel.
type fakeRuns struct {
	starts, lookups, cancels int
}

func (f *fakeRuns) TaskStart(_ context.Context, r orchapi.TaskStartRequest) (orchapi.Task, error) {
	if r.Lookup {
		f.lookups++
		if r.ClientRequestID != "known" {
			return orchapi.Task{}, api.NotFound("task", r.ClientRequestID)
		}
	} else {
		f.starts++
	}
	return orchapi.Task{ID: "rt-1", State: "running"}, nil
}

func (f *fakeRuns) TaskStatus(context.Context, string, int) (orchapi.Task, error) {
	return orchapi.Task{ID: "rt-1", State: "running"}, nil
}

func (f *fakeRuns) TaskCancel(context.Context, string) (orchapi.Task, error) {
	f.cancels++
	return orchapi.Task{ID: "rt-1", State: "running"}, nil
}

// CancelRun only looks up (an unknown request is not_found, nothing
// started), and a cancel whose run never ends is an error that says the
// cancel was not confirmed, not a success.
func TestCancelRunLooksUpAndConfirms(t *testing.T) {
	f := &fakeRuns{}
	req := orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun, Machine: "box", ClientRequestID: "unknown", Run: &orchapi.TaskRun{Ref: "main", Script: "s.sh"}}
	if _, err := CancelRun(context.Background(), f, req); !api.IsCode(err, api.CodeNotFound) || f.starts != 0 || f.lookups != 1 || f.cancels != 0 {
		t.Fatalf("unknown request: %v %+v", err, f)
	}
	req.ClientRequestID = "known"
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	_, err := CancelRun(ctx, f, req)
	if !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "not confirmed") || f.cancels != 1 || f.starts != 0 {
		t.Fatalf("unconfirmed cancel: %v %+v", err, f)
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("the confirmation outlived ctx: %s", d)
	}
}

// overtakingResend is an authority on whose own machine the resend after a
// signal overtook the first start and became it: it runs its checkout
// inside the request, past any bound, and the first never lands. A lookup
// finds it once its record exists (not at the first try).
type overtakingResend struct {
	starts, lookups, cancels int
	cancelled                bool
}

func (f *overtakingResend) TaskStart(ctx context.Context, r orchapi.TaskStartRequest) (orchapi.Task, error) {
	if !r.Lookup {
		f.starts++
		<-ctx.Done() // the first start is cut by the signal; the resend runs the checkout
		return orchapi.Task{}, ctx.Err()
	}
	f.lookups++
	if f.lookups == 1 {
		return orchapi.Task{}, api.NotFound("task", r.ClientRequestID)
	}
	return orchapi.Task{ID: "rt-1", State: orchapi.TaskPending}, nil
}

func (f *overtakingResend) TaskStatus(context.Context, string, int) (orchapi.Task, error) {
	if f.cancelled {
		return orchapi.Task{ID: "rt-1", State: orchapi.TaskRefused}, nil
	}
	return orchapi.Task{ID: "rt-1", State: orchapi.TaskPending}, nil
}

func (f *overtakingResend) TaskCancel(context.Context, string) (orchapi.Task, error) {
	f.cancels++
	f.cancelled = true
	return orchapi.Task{ID: "rt-1", State: orchapi.TaskPending}, nil
}

// A signal during a start whose resend gets no answer: the
// run is found by lookups, never by another start, and cancelled, confirmed.
func TestStartRunSignalResendTimesOut(t *testing.T) {
	defer func(a, b, c, d, e time.Duration) {
		runCancelWait, runResendWait, runLookupWait, runLookupEvery, runConfirmEvery = a, b, c, d, e
	}(runCancelWait, runResendWait, runLookupWait, runLookupEvery, runConfirmEvery)
	runCancelWait, runResendWait, runLookupWait, runLookupEvery, runConfirmEvery = 3*time.Second, 200*time.Millisecond, 200*time.Millisecond, 50*time.Millisecond, 20*time.Millisecond
	f := &overtakingResend{}
	ctx, signal := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, signal)
	req := orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun, Machine: "mini", ClientRequestID: "r1", Run: &orchapi.TaskRun{Ref: "main", Script: "s.sh"}}
	_, err := StartRun(ctx, f, req)
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "\n") || f.starts != 2 || f.lookups != 2 || f.cancels != 1 {
		t.Fatalf("signal with a resend that times out: %v %+v", err, f)
	}
}
