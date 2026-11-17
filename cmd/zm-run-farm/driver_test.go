package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/protocol"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

// fakeTasks is the daemon's task API: runs start pending, then run.
type fakeTasks struct {
	mu       sync.Mutex
	down     error
	byKey    map[string]*orchapi.Task
	byID     map[string]*orchapi.Task
	starts   int
	cancels  int
	actors   []string
	reqs     []orchapi.TaskStartRequest
	pendFor  int    // Status calls a new task stays pending
	refuse   string // the machine refuses every start with this
	startErr error
	// cancelEnds, when set, is how a run ended just before a cancel reached
	// it; cancelErr fails every cancel.
	cancelEnds *orchapi.Outcome
	cancelErr  error
}

func newFake() *fakeTasks {
	return &fakeTasks{byKey: map[string]*orchapi.Task{}, byID: map[string]*orchapi.Task{}, pendFor: 1}
}

func (f *fakeTasks) Health(context.Context) error { return f.down }

func (f *fakeTasks) Start(_ context.Context, actor string, req orchapi.TaskStartRequest) (orchapi.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return orchapi.Task{}, f.startErr
	}
	if t, ok := f.byKey[actor+"|"+req.ClientRequestID]; ok {
		return *t, nil
	}
	f.starts++
	f.actors = append(f.actors, actor)
	f.reqs = append(f.reqs, req)
	t := &orchapi.Task{ID: orchapi.RemoteTaskPrefix + strings.Repeat("a", f.starts), Parent: actor, Agent: "run", State: orchapi.TaskPending, Machine: "dev-02"}
	f.byKey[actor+"|"+req.ClientRequestID] = t
	f.byID[t.ID] = t
	return *t, nil
}

func (f *fakeTasks) Status(_ context.Context, id string, _ int) (orchapi.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.byID[id]
	if !ok {
		return orchapi.Task{}, &api.Error{Code: api.CodeNotFound, Message: "no task " + id}
	}
	if t.State == orchapi.TaskPending {
		switch {
		case f.refuse != "":
			t.State, t.Error = orchapi.TaskRefused, f.refuse
		case f.pendFor > 0:
			f.pendFor--
		default:
			t.State = "running"
		}
	}
	return *t, nil
}

func (f *fakeTasks) Cancel(_ context.Context, id string) (orchapi.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErr != nil {
		return orchapi.Task{}, f.cancelErr
	}
	f.cancels++
	t, ok := f.byID[id]
	if !ok {
		return orchapi.Task{}, &api.Error{Code: api.CodeNotFound, Message: "no task " + id}
	}
	if f.cancelEnds != nil && t.State == "running" {
		t.State, t.Result = "ended", &orchapi.TaskResult{Outcome: *f.cancelEnds}
	}
	if t.State == "running" || t.State == orchapi.TaskPending {
		t.State = "ended"
		t.Result = &orchapi.TaskResult{Outcome: orchapi.Outcome{Kind: orchapi.OutcomeKilled, Evidence: "cancelled"}}
	}
	return *t, nil
}

// end makes id end the way a run task does.
func (f *fakeTasks) end(id string, o orchapi.Outcome) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byID[id].State = "ended"
	f.byID[id].Result = &orchapi.TaskResult{Outcome: o}
}

func intp(n int) *int { return &n }

type harness struct {
	t    *testing.T
	f    *fakeTasks
	conf map[string]string
}

func newHarness(t *testing.T) *harness {
	return &harness{t: t, f: newFake(), conf: map[string]string{"target": "farm", "kind": "run", "project": "harbor",
		"machine": "auto", "state_dir": t.TempDir()}}
}

// call sends one envelope and answers (result, error code).
func (h *harness) call(verb, id string, args any) (map[string]any, string) {
	h.t.Helper()
	// The protocol under both keys, as the daemon sends it.
	b, _ := json.Marshal(map[string]any{"potluck": "potluck/1", "zm": "zm/v0", "verb": verb, "id": id, "args": args, "config": h.conf})
	var stdout bytes.Buffer
	if err := answer(&stdout, b, func(project string) (tasks, error) {
		if project != h.conf["project"] {
			h.t.Fatalf("opened project %q", project)
		}
		return h.f, nil
	}); err != nil {
		h.t.Fatal(err)
	}
	// Read by Potluck's one rule for a driver's answer, as the daemon reads it.
	a, err := protocol.ReadAnswer(stdout.Bytes(), nil)
	if err != nil {
		h.t.Fatalf("%s: the daemon refuses this answer: %v (%q)", verb, err, stdout.String())
	}
	var env struct {
		OK     bool
		Result map[string]any
		Error  struct{ Code, Message string }
	}
	env.OK = a.OK
	if a.Error != nil {
		env.Error.Code, env.Error.Message = a.Error.Code, a.Error.Message
	}
	if a.OK && json.Unmarshal(a.Result, &env.Result) != nil {
		h.t.Fatalf("%s: result is not an object: %s", verb, a.Result)
	}
	if !env.OK {
		return nil, env.Error.Code
	}
	return env.Result, ""
}

func (h *harness) acquire(reqID string) map[string]any {
	h.t.Helper()
	res, code := h.call("acquire", "", acquireReq(reqID))
	if code != "" {
		h.t.Fatalf("acquire: %s", code)
	}
	return res
}

func acquireReq(reqID string) map[string]any {
	return map[string]any{"request_id": reqID, "actor": "mini/aabbccdd", "ttl_s": 600, "ref": "main",
		"commit": testCommit, "script": "scripts/probe.sh", "args": []string{"--x", "a b"}}
}

func TestDescribe(t *testing.T) {
	h := newHarness(t)
	res, code := h.call("describe", "", nil)
	if code != "" {
		t.Fatal(code)
	}
	if res["isolation"] != "none" || res["credentials"] != true || res["kind"] != "run" || res["available"] != true || res["reports_spend"] != false {
		t.Fatalf("describe = %v", res)
	}
	// The protocol under its new key and, for a daemon from before the rename, its old one.
	if res["potluck"] != "potluck/1" || res["zm"] != "zm/v0" {
		t.Fatalf("describe protocol = %v / %v", res["potluck"], res["zm"])
	}
	h.f.down = errors.New("no socket")
	res, _ = h.call("describe", "", nil)
	if res["available"] != false || !strings.Contains(res["why"].(string), "no socket") {
		t.Fatalf("describe with no daemon = %v", res)
	}
}

func TestConfigRefusals(t *testing.T) {
	for _, tc := range []struct{ key, val string }{{"project", ""}, {"machine", ""}, {"target", ""}, {"target", ".."}, {"target", "."}, {"target", ".hidden"}, {"kind", "db"}, {"ttl_max_s", "-1"}} {
		h := newHarness(t)
		h.conf[tc.key] = tc.val
		if _, code := h.call("describe", "", nil); code != "bad_request" {
			t.Errorf("%s=%q: code %q, want bad_request", tc.key, tc.val, code)
		}
	}
	h := newHarness(t)
	if _, code := h.call("renew", "run:farm/rt-a", map[string]int{"ttl_s": 60}); code != "not_implemented" {
		t.Fatalf("renew: %q", code)
	}
}

func TestAcquireStatusRelease(t *testing.T) {
	h := newHarness(t)
	res := h.acquire("k1")
	handle := res["handle"].(string)
	if handle != "run:farm/rt-a" || res["commit"] != testCommit || res["ttl_s"].(float64) != 600 {
		t.Fatalf("acquire = %v", res)
	}
	exp, err := time.Parse(time.RFC3339, res["expires_at"].(string))
	if err != nil || time.Until(exp) < 590*time.Second || time.Until(exp) > 610*time.Second {
		t.Fatalf("expires_at %v", res["expires_at"])
	}
	r := h.f.reqs[0]
	if r.Agent != orchapi.TaskAgentRun || r.Machine != "auto" || r.TimeoutSeconds != 600 || r.Run.Commit != testCommit ||
		r.Run.Ref != "main" || r.Run.Script != "scripts/probe.sh" || len(r.Run.Args) != 2 || r.Run.Args[1] != "a b" || r.Cwd != "" {
		t.Fatalf("task request = %+v run %+v", r, r.Run)
	}
	if h.f.actors[0] != "mini/aabbccdd" {
		t.Fatalf("parent %q", h.f.actors[0])
	}
	st, _ := h.call("status", handle, nil)
	if st["state"] != "running" || st["commit"] != testCommit || st["exit"] != nil {
		t.Fatalf("status = %v", st)
	}
	rel, _ := h.call("release", handle, nil)
	if rel["released"] != true || rel["exit"] != nil {
		t.Fatalf("release = %v", rel)
	}
	rel, _ = h.call("release", handle, nil)
	if rel["released"] != false || rel["exit"] != nil {
		t.Fatalf("second release = %v", rel)
	}
	st, _ = h.call("status", handle, nil)
	if st["state"] != "released" {
		t.Fatalf("status after release = %v", st)
	}
	if h.f.cancels != 1 {
		t.Fatalf("cancels = %d", h.f.cancels)
	}
}

func TestAcquireIsIdempotent(t *testing.T) {
	h := newHarness(t)
	a := h.acquire("same")
	b := h.acquire("same")
	if a["handle"] != b["handle"] || h.f.starts != 1 {
		t.Fatalf("handles %v %v, starts %d", a["handle"], b["handle"], h.f.starts)
	}
	// The record lost (a crash before it was written): the daemon's key
	// still answers the same task.
	os.RemoveAll(h.conf["state_dir"])
	c := h.acquire("same")
	if c["handle"] != a["handle"] || h.f.starts != 1 {
		t.Fatalf("after losing the record: %v, starts %d", c["handle"], h.f.starts)
	}
	ls, _ := h.call("list", "", nil)
	hs := ls["handles"].([]any)
	if len(hs) != 1 || hs[0].(map[string]any)["request_id"] != "same" || hs[0].(map[string]any)["handle"] != a["handle"] {
		t.Fatalf("list = %v", ls)
	}
	// The task key is per binding: another binding's same request key is
	// another run.
	h.conf["target"] = "farm2"
	d := h.acquire("same")
	if d["handle"] == a["handle"] || h.f.starts != 2 {
		t.Fatalf("other binding: %v, starts %d", d["handle"], h.f.starts)
	}
}

func TestEndStates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		o     orchapi.Outcome
		state string
		exit  any
	}{
		{"exit 0", orchapi.Outcome{Kind: orchapi.OutcomeOK, ExitCode: intp(0), Evidence: "script exited 0"}, "ended", 0.0},
		{"exit 3", orchapi.Outcome{Kind: orchapi.OutcomeFailed, ExitCode: intp(3), Evidence: "script exited 3"}, "ended", 3.0},
		{"deadline", orchapi.Outcome{Kind: orchapi.OutcomeKilled, ExitCode: intp(124), Evidence: "overall deadline exceeded"}, "expired", 124.0},
		{"signal", orchapi.Outcome{Kind: orchapi.OutcomeKilled, Signal: "KILL", Evidence: "killed by SIGKILL"}, "lost", nil},
		{"infra", orchapi.Outcome{Kind: orchapi.OutcomeInfra, Evidence: "daemon restarted"}, "lost", nil},
		{"may still run", orchapi.Outcome{Kind: orchapi.OutcomeFailed, Evidence: "process may still be running"}, "lost", nil},
		{"cancelled elsewhere", orchapi.Outcome{Kind: orchapi.OutcomeKilled, Evidence: "cancelled"}, "released", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			handle := h.acquire("k")["handle"].(string)
			h.f.end("rt-a", tc.o)
			st, _ := h.call("status", handle, nil)
			if st["state"] != tc.state || st["exit"] != tc.exit {
				t.Fatalf("status = %v, want %s exit %v", st, tc.state, tc.exit)
			}
			rel, _ := h.call("release", handle, nil)
			wantExit := any(nil)
			if tc.state == "ended" {
				wantExit = tc.exit
			}
			if rel["released"] != false || rel["exit"] != wantExit {
				t.Fatalf("release after end = %v", rel)
			}
			if h.f.cancels != 0 {
				t.Fatal("release of an ended run cancelled it")
			}
		})
	}
}

func TestLostTask(t *testing.T) {
	h := newHarness(t)
	handle := h.acquire("k")["handle"].(string)
	delete(h.f.byID, "rt-a") // the authority forgot it
	st, code := h.call("status", handle, nil)
	if code != "" || st["state"] != "lost" {
		t.Fatalf("status = %v %s", st, code)
	}
}

func TestHandleRefusals(t *testing.T) {
	h := newHarness(t)
	h.acquire("k")
	for handle, want := range map[string]string{
		"nonsense": "bad_request", "db:farm/x": "bad_request", "run:other/rt-a": "bad_request",
		"run:farm/rt-zzz": "not_found", "run:farm/../../etc": "not_found",
	} {
		for _, verb := range []string{"status", "release"} {
			if _, code := h.call(verb, handle, nil); code != want {
				t.Errorf("%s %s: %q, want %q", verb, handle, code, want)
			}
		}
	}
}

func TestAcquireRefusals(t *testing.T) {
	cases := map[string]func(map[string]any){
		"no request_id": func(a map[string]any) { delete(a, "request_id") },
		"no actor":      func(a map[string]any) { delete(a, "actor") },
		"no ref":        func(a map[string]any) { delete(a, "ref") },
		"short commit":  func(a map[string]any) { a["commit"] = "abc123" },
		"shell string":  func(a map[string]any) { a["script"] = "npm test && curl x" },
		"ttl over cap":  func(a map[string]any) { a["ttl_s"] = defaultTTLMax + 1 },
		"negative ttl":  func(a map[string]any) { a["ttl_s"] = -5 },
	}
	for name, mod := range cases {
		h := newHarness(t)
		a := acquireReq("k")
		mod(a)
		if _, code := h.call("acquire", "", a); code != "bad_request" {
			t.Errorf("%s: %q, want bad_request", name, code)
		}
		if h.f.starts != 0 {
			t.Errorf("%s: started a task", name)
		}
	}
}

func TestMachineRefusals(t *testing.T) {
	for msg, want := range map[string]string{
		"run.commit 0123 is not what main resolves to on the remote (89ab)": "forbidden",
		"the remote has no branch or tag nope":                              "bad_request",
		"scripts/x.sh must be an executable file inside the repository":     "bad_request",
		"fetch main: network down":                                          "unavailable",
	} {
		h := newHarness(t)
		h.f.refuse = msg
		if _, code := h.call("acquire", "", acquireReq("k")); code != want {
			t.Errorf("%q: %q, want %q", msg, code, want)
		}
	}
	h := newHarness(t)
	h.f.startErr = &api.Error{Code: api.CodeForbidden, Message: "only the owner"}
	if _, code := h.call("acquire", "", acquireReq("k")); code != "forbidden" {
		t.Fatalf("daemon refusal: %q", code)
	}
}

func TestAcquireWithdrawsAnUntakenRun(t *testing.T) {
	h := newHarness(t)
	h.f.pendFor = 1 << 30
	clock := time.Now()
	b, _ := json.Marshal(map[string]any{"verb": "acquire", "args": acquireReq("k"), "config": h.conf})
	var req struct {
		Args   json.RawMessage
		Config map[string]string
	}
	_ = json.Unmarshal(b, &req)
	cfg, err := parseConfig(req.Config)
	if err != nil {
		t.Fatal(err)
	}
	d := &drv{cfg: cfg, t: h.f, now: func() time.Time { clock = clock.Add(30 * time.Second); return clock }}
	_, err = d.acquire(context.Background(), req.Args)
	var r refusal
	if !errors.As(err, &r) || r.code != "unavailable" {
		t.Fatalf("acquire = %v", err)
	}
	if h.f.cancels != 1 || h.f.byID["rt-a"].State != "ended" {
		t.Fatalf("the untaken run was not withdrawn: cancels %d state %s", h.f.cancels, h.f.byID["rt-a"].State)
	}
	// zm retries with the same key: the withdrawn run is never a handle.
	if res, code := h.call("acquire", "", acquireReq("k")); code != "unavailable" {
		t.Fatalf("retry after withdrawal = %v %q", res, code)
	}
	if h.f.starts != 1 {
		t.Fatalf("starts = %d", h.f.starts)
	}
}

func TestAcquireOfAFastRun(t *testing.T) {
	h := newHarness(t)
	// The script ran and exited before acquire looked: it started, so it
	// is a handle, ended with its exit.
	h.f.pendFor = 1 << 30
	b, _ := json.Marshal(acquireReq("fast"))
	cfg, _ := parseConfig(h.conf)
	d := &drv{cfg: cfg, t: fastTasks{h.f}, now: time.Now}
	res, err := d.acquire(context.Background(), b)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := h.call("status", res.(map[string]any)["handle"].(string), nil)
	if st["state"] != "ended" || st["exit"] != 0.0 {
		t.Fatalf("status = %v", st)
	}
}

// fastTasks ends every run with exit 0 as soon as it starts.
type fastTasks struct{ *fakeTasks }

func (f fastTasks) Start(ctx context.Context, actor string, req orchapi.TaskStartRequest) (orchapi.Task, error) {
	t, err := f.fakeTasks.Start(ctx, actor, req)
	if err == nil {
		f.end(t.ID, orchapi.Outcome{Kind: orchapi.OutcomeOK, ExitCode: intp(0), Evidence: "script exited 0"})
		t = *f.byID[t.ID]
	}
	return t, err
}

func TestReleaseRacingTheEnd(t *testing.T) {
	for _, tc := range []struct {
		name  string
		o     orchapi.Outcome
		state string
		exit  any
	}{
		{"expired", orchapi.Outcome{Kind: orchapi.OutcomeKilled, ExitCode: intp(124), Evidence: "overall deadline exceeded"}, "expired", nil},
		{"exited", orchapi.Outcome{Kind: orchapi.OutcomeFailed, ExitCode: intp(2), Evidence: "script exited 2"}, "ended", 2.0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			handle := h.acquire("k")["handle"].(string)
			h.f.cancelEnds = &tc.o
			rel, _ := h.call("release", handle, nil)
			if rel["released"] != false || rel["exit"] != tc.exit {
				t.Fatalf("release = %v", rel)
			}
			st, _ := h.call("status", handle, nil)
			if st["state"] != tc.state {
				t.Fatalf("status = %v", st)
			}
		})
	}
}

func TestFailedCancelRecordsNothing(t *testing.T) {
	h := newHarness(t)
	handle := h.acquire("k")["handle"].(string)
	h.f.cancelErr = errors.New("daemon gone")
	if _, code := h.call("release", handle, nil); code != "unavailable" {
		t.Fatalf("release = %q", code)
	}
	// The run is later killed by a signal: that is lost, not released.
	h.f.end("rt-a", orchapi.Outcome{Kind: orchapi.OutcomeKilled, Signal: "KILL", Evidence: "killed by SIGKILL"})
	st, _ := h.call("status", handle, nil)
	if st["state"] != "lost" {
		t.Fatalf("status = %v", st)
	}
}

// TestClientTasksSendsNoCwd: the real client fills cwd from the working
// directory, which the server refuses on a script run; the run's parent is
// the actor.
func TestClientTasksSendsNoCwd(t *testing.T) {
	var got orchapi.TaskStartRequest
	var as, project string
	c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		as, project = api.GetHeader(r.Header, api.HeaderAs), api.GetHeader(r.Header, api.HeaderProject)
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		_ = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(orchapi.Task{ID: "rt-x", State: orchapi.TaskPending})
	}))
	c.Cwd, c.Project = "/some/where", "harbor"
	if _, err := (clientTasks{c}).Start(context.Background(), "mini/aabbccdd", orchapi.TaskStartRequest{Agent: "run", Run: &orchapi.TaskRun{Ref: "main", Script: "s.sh"}}); err != nil {
		t.Fatal(err)
	}
	if got.Cwd != "" || as != "mini/aabbccdd" || project != "harbor" || c.As != "" {
		t.Fatalf("cwd %q as %q project %q (client As now %q)", got.Cwd, as, project, c.As)
	}
}

// A request the driver cannot read is still one answer, exit 0: a refusal
// Potluck's rule takes (bad_request), never stray text or an internal error.
func TestUnreadableRequestIsOneAnswer(t *testing.T) {
	for _, in := range []string{"", "not json", `{"verb":`} {
		var stdout bytes.Buffer
		if err := answer(&stdout, []byte(in), func(string) (tasks, error) { t.Fatal("opened a project"); return nil, nil }); err != nil {
			t.Fatal(err)
		}
		a, err := protocol.ReadAnswer(stdout.Bytes(), nil)
		if err != nil || a.OK || a.Error == nil || a.Error.Code != "bad_request" {
			t.Errorf("%q: %+v %v (%q)", in, a, err, stdout.String())
		}
	}
}

// A daemon refusal with no message still answers its code: an empty message
// would make the whole answer broken (internal) by Potluck's rule.
func TestEmptyRefusalMessageKeepsItsCode(t *testing.T) {
	h := newHarness(t)
	h.f.startErr = &api.Error{Code: api.CodeConflict}
	if _, code := h.call("acquire", "", acquireReq("k")); code != "conflict" {
		t.Fatalf("code %q, want conflict", code)
	}
}
