package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/project"
)

// Wire real storage and holders without calling Service.Start: no task loop
// exists to complete a failed row behind the test's back. taskMu alone cannot
// suppress taskTick, which does not take it when there are no pending handoffs.
func failedStartHarness(t *testing.T, script string) (*harness, orchapi.Session) {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	bin := filepath.Join(home, "fake-agent")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "me", "mini"
	cfg.File.Agents = map[string][]string{"codex": {bin}, "claude": {bin}}
	ctx := context.Background()
	st, err := storetest.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	reg, err := project.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := events.New(ctx, st, cfg.Machine)
	if err != nil {
		t.Fatal(err)
	}
	d := &core.Deps{Config: cfg, Store: st, Projects: reg, Events: bus, Started: time.Now(),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := New(d)
	t.Cleanup(func() { _ = s.Close() })
	s.Probe = func(...string) orchapi.AdmissionProbe {
		return orchapi.AdmissionProbe{MemoryFreePercent: 80, SwapUsedMB: 0, DiskFreeMB: 100000}
	}
	if s.homeIdentity, err = taskHomeIdentityAt(home, true); err != nil {
		t.Fatal(err)
	}
	fw := &fakeWork{}
	orchcore.Wire(d).Sessions, orchcore.Wire(d).Work = s, fw
	bus.SetVisibility(func(ctx context.Context, sc authz.Scope, addr string) bool { return d.Visible(ctx, sc, addr) })
	h := &harness{t: t, home: home, d: d, s: s, work: fw}
	parent, _, err := s.Register(core.WithSubject(ctx, h.owner()), orchapi.RegisterRequest{Agent: "shell", RuntimeID: "failed-start-parent", Cwd: home, State: orchapi.StateRunning})
	if err != nil {
		t.Fatal(err)
	}
	return h, parent
}

// The caller holds taskMu, as production starts do. The fixture above never
// starts background reconciliation; all completion must come from the path tested.
func startFailureTaskLocked(t *testing.T, h *harness, parent orchapi.Session, req orchapi.TaskStartRequest) (orchapi.Task, error) {
	t.Helper()
	req.Cwd = h.home
	sub := h.d.Projects.Local(parent.Address)
	ctx := core.WithSubject(context.Background(), sub)
	ctx = core.WithResolveInput(ctx, project.ResolveInput{Cwd: h.home})
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	return h.s.startTaskLocked(ctx, sub, parent, req, string(b), "", "", nil)
}

func failedStartAgent(t *testing.T) string {
	t.Helper()
	bad := filepath.Join(t.TempDir(), "bad-agent")
	if err := os.WriteFile(bad, []byte("#!/nonexistent/zm-interpreter\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return bad
}

func assertFailedStartEnded(t *testing.T, h *harness, id string) {
	t.Helper()
	var state, raw string
	var neverStarted bool
	if err := h.d.Store.Registry().QueryRow(`SELECT state,result,child_never_started FROM session_tasks WHERE id=?`, id).Scan(&state, &raw, &neverStarted); err != nil {
		t.Fatal(err)
	}
	var result orchapi.TaskResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil || state != "ended" || !neverStarted || result.Finished != "unreported" || result.Outcome.Kind != orchapi.OutcomeInfra {
		t.Fatalf("failed start %s: state=%s never_started=%v result=%s error=%v", id, state, neverStarted, raw, err)
	}
}

func TestFailedTaskStartsReleaseSlotsImmediately(t *testing.T) {
	h, parent := failedStartHarness(t, `if [ "$1" = --version ]; then echo fake; exit; fi; cat >/dev/null; exec sleep 30`)
	original := h.d.Config.File.Agents["claude"]
	h.d.Config.File.Agents["claude"] = []string{failedStartAgent(t)}
	no := false
	h.s.taskMu.Lock()
	defer h.s.taskMu.Unlock()
	for i := 0; i < taskParentLimit+2; i++ {
		req := orchapi.TaskStartRequest{Agent: "claude", Prompt: "go", Worktree: &no, ClientRequestID: fmt.Sprintf("failed-%d", i)}
		if _, err := startFailureTaskLocked(t, h, parent, req); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("start %d: %v (must fail exec, not concurrency admission)", i, err)
		}
		var id string
		if err := h.d.Store.Registry().QueryRow(`SELECT id FROM session_tasks WHERE parent=? AND request_id=?`, parent.Address, req.ClientRequestID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		assertFailedStartEnded(t, h, id)
		// Ordinary failed requests keep their key and replay their exact result.
		again, err := startFailureTaskLocked(t, h, parent, req)
		if err != nil || again.ID != id || again.State != "ended" || again.Result == nil || again.Result.Outcome.Kind != orchapi.OutcomeInfra {
			t.Fatalf("retry %d: %+v %v", i, again, err)
		}
	}
	h.d.Config.File.Agents["claude"] = original
	if child, err := startFailureTaskLocked(t, h, parent, orchapi.TaskStartRequest{Agent: "claude", Prompt: "valid", Worktree: &no}); err != nil || child.ID == "" {
		t.Fatalf("valid start after repeated failures: %+v %v", child, err)
	}
}

func TestFailedContinuationStartsReleaseSlotsImmediately(t *testing.T) {
	script := `if [ "$1" = --version ]; then echo fake; exit; fi
if [ "$1" = exec ]; then printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else cat >/dev/null; exec sleep 30; fi`
	h, parent := failedStartHarness(t, script)
	no := false
	h.s.taskMu.Lock()
	defer h.s.taskMu.Unlock()
	source, err := startFailureTaskLocked(t, h, parent, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	// Holder-driven completion remains active; only the periodic task loop is
	// absent. Wait for this real quota source to finish, without polling/sleeps.
	if p := h.s.proc(source.ID); p != nil {
		select {
		case <-p.exited:
		case <-time.After(10 * time.Second):
			t.Fatal("quota source did not exit")
		}
	}
	source, err = h.s.getTask(context.Background(), h.d.Projects.Local(parent.Address), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if source.Result == nil || source.Result.Outcome.Kind != orchapi.OutcomeQuota {
		t.Fatalf("source is not limited: %+v", source)
	}
	original := h.d.Config.File.Agents["claude"]
	h.d.Config.File.Agents["claude"] = []string{failedStartAgent(t)}
	key := "handoff:" + source.ID
	req := orchapi.TaskStartRequest{Agent: "claude", Prompt: "go", Worktree: &no, ContinueFrom: source.ID, ClientRequestID: key}
	db := h.d.Store.Registry()
	reserve := func() {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO session_handoffs(source) VALUES(?) ON CONFLICT(source) DO UPDATE SET status='pending',error='' WHERE status='failed' AND continuation=''`, source.ID); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < taskParentLimit+2; i++ {
		reserve()
		if _, err := startFailureTaskLocked(t, h, parent, req); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("continuation %d: %v (must fail exec, not concurrency admission)", i, err)
		}
		var status, continuation string
		if err := db.QueryRow(`SELECT status,continuation FROM session_handoffs WHERE source=?`, source.ID).Scan(&status, &continuation); err != nil || status != "failed" || continuation != "" {
			t.Fatalf("reservation %d: %s %q %v", i, status, continuation, err)
		}
		var retainedKeys, ended int
		if err := db.QueryRow(`SELECT count(*) FROM session_tasks WHERE request_id=?`, key).Scan(&retainedKeys); err != nil || retainedKeys != 0 {
			t.Fatalf("generated key retained: %d %v", retainedKeys, err)
		}
		if err := db.QueryRow(`SELECT count(*) FROM session_tasks WHERE parent=? AND id!=? AND state='ended' AND child_never_started=1`, parent.Address, source.ID).Scan(&ended); err != nil || ended != i+1 {
			t.Fatalf("failed rows not finalized: %d want %d error %v", ended, i+1, err)
		}
	}
	h.d.Config.File.Agents["claude"] = original
	reserve()
	child, err := startFailureTaskLocked(t, h, parent, req)
	if err != nil || child.ID == "" {
		t.Fatalf("valid continuation after repeated failures: %+v %v", child, err)
	}
	var continuation string
	if err := db.QueryRow(`SELECT continuation FROM session_handoffs WHERE source=? AND status='started'`, source.ID).Scan(&continuation); err != nil || continuation != child.ID {
		t.Fatalf("valid continuation reservation: %q %v", continuation, err)
	}
}

func TestAmbiguousHolderStartKeepsTaskSlotForReconciliation(t *testing.T) {
	h, parent := failedStartHarness(t, `echo fake`)
	bad := filepath.Join(t.TempDir(), "unconfirmed-holder")
	// The holder process launched, but its response provides no child-start
	// proof. This exercises a transport/protocol error, not NeverStartedError.
	if err := os.WriteFile(bad, []byte("#!/bin/sh\nread spec\nprintf '%s\\n' 'invalid-ready-json'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	h.s.HolderExe = bad
	no := false
	req := orchapi.TaskStartRequest{Agent: "claude", Prompt: "go", Worktree: &no, ClientRequestID: "ambiguous"}
	h.s.taskMu.Lock()
	defer h.s.taskMu.Unlock()
	if _, err := startFailureTaskLocked(t, h, parent, req); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("ambiguous start: %v", err)
	}
	var state, result string
	var neverStarted bool
	if err := h.d.Store.Registry().QueryRow(`SELECT state,result,child_never_started FROM session_tasks WHERE request_id=?`, req.ClientRequestID).Scan(&state, &result, &neverStarted); err != nil || state != "running" || result != "" || neverStarted {
		t.Fatalf("ambiguous task prematurely finalized: state=%s result=%q never_started=%v error=%v", state, result, neverStarted, err)
	}
}

func TestFailedStartWithoutDurableProofKeepsTaskSlot(t *testing.T) {
	h, parent := failedStartHarness(t, `echo fake`)
	h.d.Config.File.Agents["claude"] = []string{failedStartAgent(t)}
	// A holder refusal alone must not release the slot if its durable
	// never-started proof could not be recorded for recovery and redaction.
	if _, err := h.d.Store.Registry().Exec(`CREATE TRIGGER reject_start_proof BEFORE UPDATE OF child_never_started ON session_tasks BEGIN SELECT RAISE(ABORT, 'injected proof write failure'); END`); err != nil {
		t.Fatal(err)
	}
	no := false
	req := orchapi.TaskStartRequest{Agent: "claude", Prompt: "go", Worktree: &no, ClientRequestID: "no-proof"}
	h.s.taskMu.Lock()
	defer h.s.taskMu.Unlock()
	if _, err := startFailureTaskLocked(t, h, parent, req); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("failed start: %v", err)
	}
	var state, result string
	var neverStarted bool
	if err := h.d.Store.Registry().QueryRow(`SELECT state,result,child_never_started FROM session_tasks WHERE request_id=?`, req.ClientRequestID).Scan(&state, &result, &neverStarted); err != nil || state != "running" || result != "" || neverStarted {
		t.Fatalf("undurable proof released task: state=%s result=%q never_started=%v error=%v", state, result, neverStarted, err)
	}
}
