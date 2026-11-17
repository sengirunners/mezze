package work

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/potluck/kit/api"
)

// envNoCursorLog is the file the nocursor adapter appends one line per
// changes call to.
const envNoCursorLog = "ZM_TEST_NOCURSOR_LOG"

// noCursorHandlers are a tracker with changes whose listings never return
// a cursor (a third-party adapter conformance would flag).
func noCursorHandlers() map[string]adapter.Handler {
	item := orchapi.WorkItem{ID: "nc:1", Title: "one", Status: orchapi.WorkOpen}
	one := func(context.Context, orchapi.AdapterRequest) (any, error) { return item, nil }
	return map[string]adapter.Handler{
		orchapi.VerbDescribe: func(context.Context, orchapi.AdapterRequest) (any, error) {
			return orchapi.AdapterDescribe{Spec: api.Protocol, Name: "nocursor", Prefix: "nc",
				Verbs: append(append([]string{}, orchapi.RequiredVerbs...), orchapi.VerbChanges)}, nil
		},
		orchapi.VerbChanges: func(ctx context.Context, req orchapi.AdapterRequest) (any, error) {
			if f, err := os.OpenFile(os.Getenv(envNoCursorLog), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
				f.WriteString("changes\n")
				f.Close()
			}
			if req.Config["hang"] != "" { // a listing that never ends (until the daemon gives up on it)
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return orchapi.AdapterChanges{Items: []orchapi.WorkItem{item}}, nil
		},
		orchapi.VerbList: func(context.Context, orchapi.AdapterRequest) (any, error) {
			return orchapi.WorkList{Items: []orchapi.WorkItem{item}}, nil
		},
		orchapi.VerbGet:       one,
		orchapi.VerbClaim:     one,
		orchapi.VerbRelease:   one,
		orchapi.VerbComment:   func(context.Context, orchapi.AdapterRequest) (any, error) { return orchapi.Comment{}, nil },
		orchapi.VerbSetStatus: one,
	}
}

// installNoCursor puts the nocursor adapter on PATH; it returns the call log.
func installNoCursor(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"nocursor", "listonly"} {
		if err := os.Symlink(self, filepath.Join(dir, adapter.ExecPrefix+name)); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(dir, "calls")
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(envNoCursorLog, log)
	return log
}

// A tracker whose full listing never gives a cursor is listed in full,
// paced like a tracker without changes (listEvery), never back to back.
func TestMirrorNoCursorIsPaced(t *testing.T) {
	old, oldL := syncEvery, listEvery
	syncEvery, listEvery = 10*time.Millisecond, time.Hour
	t.Cleanup(func() { syncEvery, listEvery = old, oldL })
	h := newHarness(t)
	log := installNoCursor(t)
	trs := []api.Tracker{{Adapter: "nocursor", Prefix: "nc", Env: api.TrackerEnvInherit}} // it logs to envNoCursorLog
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	if _, err := orchclient.GetWork(h.ctx, h.as(h.owner, ""), "nc:1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond) // 40 incremental periods
	b, _ := os.ReadFile(log)
	if n := strings.Count(string(b), "changes"); n > 3 {
		t.Fatalf("%d full listings in 400 ms of a tracker without cursors (want it paced by listEvery)", n)
	}
}

// A binding change stops the old syncer and waits for it before the new
// one wipes the mirror: no page of the old listing lands after the wipe.
func TestMirrorRebindAwaitsOldSyncer(t *testing.T) {
	quiet(t)
	h := newHarness(t)
	log := installNoCursor(t)
	p := h.harbor
	old := h.s.m.syncer(p.ID, api.Tracker{Adapter: "nocursor", Prefix: "nc", Config: map[string]string{"hang": "1"}, Env: api.TrackerEnvInherit})
	deadline := time.Now().Add(30 * time.Second)
	for b, _ := os.ReadFile(log); !strings.Contains(string(b), "changes"); b, _ = os.ReadFile(log) {
		if time.Now().After(deadline) {
			t.Fatal("the first listing never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	sy := h.s.m.syncer(p.ID, api.Tracker{Adapter: "nocursor", Prefix: "nc", Env: api.TrackerEnvInherit})
	if sy == old {
		t.Fatal("a new binding kept the old syncer")
	}
	select {
	case <-old.done:
	default:
		t.Fatal("the new syncer started while the old one was still listing")
	}
}

// A rate-limited tracker's retry_after holds sync back that long, even on
// the first failure (whose own backoff is 2 s).
func TestErrPauseHonorsRetryAfter(t *testing.T) {
	plain := api.Errorf(api.CodeUnavailable, "linear: HTTP 502")
	limited := api.Errorf(api.CodeUnavailable, "Linear rate limit reached").WithRetryAfter(90)
	if got := errPause(plain, 1); got != 2*time.Second {
		t.Errorf("first failure: %v", got)
	}
	if got := errPause(limited, 1); got != 90*time.Second {
		t.Errorf("rate limited: %v, want 90s", got)
	}
	if got := errPause(fmt.Errorf("adapter: %w", limited), 1); got != 90*time.Second {
		t.Errorf("wrapped: %v", got)
	}
	huge := api.Errorf(api.CodeUnavailable, "rate limited").WithRetryAfter(1 << 30)
	if got := errPause(huge, 1); got != time.Hour {
		t.Errorf("an absurd retry_after: %v, want capped at 1h", got)
	}
}
