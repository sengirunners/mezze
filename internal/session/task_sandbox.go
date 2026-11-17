package session

// Sandbox tasks (SPEC 8 "Where it runs"): a remote task with sandbox set
// runs on a machine whose owner configured a task driver
// (sandbox_task_cmd). The task's session is opened and its work item bound
// as for any task, but its child is the driver, not an agent: no worktree,
// the task as JSON on stdin, the allowlisted environment plus ZM_SESSION,
// ZM_TASK_ID and ZM_TASK_RESULT. The driver leases a pool box and runs the
// agent there; what it writes to ZM_TASK_RESULT is the task's result. The
// driver is the holder's child in its own process group, so cancel, the
// deadline and a daemon restart (readopt) work as for any task. Not
// SpawnRequest.Sandbox: that is the srt filesystem sandbox of a session.

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// taskAgentSandbox is a sandbox task's session agent: nothing that treats a
// claude or codex session as that CLI (task context, hooks, usage, the
// result's JSON) touches the driver. The task answers with the request's
// agent.
const taskAgentSandbox = "sandbox"

// spawnModel is the model spawn hands the child: none for a task driver,
// which gets the model in the request on its stdin (sandboxTaskInput).
func spawnModel(req orchapi.TaskStartRequest, sandbox *sandboxStart) string {
	if sandbox != nil {
		return ""
	}
	return req.Model
}

// sandboxAgentOK keeps the task driver's agent name for sandbox tasks: the
// session limit and the machine's session count leave those sessions out
// (admit, managedCounted), so no other session may carry the name.
func sandboxAgentOK(agent string, task *taskStart) error {
	if agent == taskAgentSandbox && (task == nil || task.sandbox == nil) {
		return api.Errorf(api.CodeBadRequest, "agent %q is reserved for sandbox tasks", agent).
			WithHint("pass another agent name for this program")
	}
	return nil
}

// admit is the admission verdict for a new session. A task driver runs on
// the machine's sandbox slots, which the waiting line counts (SPEC 8
// "Waiting line"), and its agent runs in a pool box: the session limit and
// memory are not its to check, disk is.
func (s *Service) admit(cwd string, driver bool) orchapi.Admission {
	p := s.Probe(s.d.Config.Home, cwd)
	p.ManagedSessions = s.managedCounted(context.Background())
	if driver {
		p.ManagedSessions, p.MemoryFreePercent, p.SwapUsedMB = 0, -1, -1
	}
	return Admit(p, s.d.Config.File.Admission)
}

// managedCounted is ManagedLive without task drivers: the sessions the
// session limit and the machine's Sessions number count. Task drivers have
// slots of their own and never hold a plain session back.
func (s *Service) managedCounted(ctx context.Context) int {
	var gone []string
	s.unreachable.Range(func(k, _ any) bool { gone = append(gone, k.(string)); return true })
	s.mu.Lock()
	n := 0
	for _, p := range s.procs {
		if p.agent != taskAgentSandbox {
			n++
		}
	}
	s.mu.Unlock()
	for _, id8 := range gone { // they run too
		if r, ok := s.load(ctx, id8); !ok || r.Agent != taskAgentSandbox {
			n++
		}
	}
	return n
}

// sandboxResultMax is how much of the result file becomes the result text.
const sandboxResultMax = 64 << 10

// SandboxTermWait is how long a stopped driver has after TERM to release
// its box and revoke its tokens before KILL (tests shorten it).
var SandboxTermWait = 60 * time.Second

// sandboxOpID is an op id safe as a directory name (rt-<ulid>[.N]).
var sandboxOpID = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,99}$`)

// outputAgent: a task child whose output is plain text and its progress (a
// script, a cloud watcher, a task driver), not an agent's event stream.
func outputAgent(agent string) bool {
	return agent == orchapi.TaskAgentRun || isCloudAgent(agent) || agent == taskAgentSandbox
}

// sandboxStart is what a sandbox task's spawn adds: its environment.
type sandboxStart struct {
	taskID, result string
}

// sandboxTaskInput is the driver's stdin.
type sandboxTaskInput struct {
	TaskID    string                   `json:"task_id"` // rt-…, as the parent knows it
	OpID      string                   `json:"op_id"`   // this placement's op id
	Project   string                   `json:"project"`
	Parent    string                   `json:"parent"`
	TimeLeftS int                      `json:"time_left_s"`
	Request   orchapi.TaskStartRequest `json:"request"`
}

func (s *Service) sandboxDir() string { return filepath.Join(s.d.Config.Home, "sandbox-tasks") }

// startRemoteSandbox starts op's sandbox task here. A repeat of the op
// answers the task it started (startTaskLocked, by client_request_id).
func (s *Service) startRemoteSandbox(ctx context.Context, proj, opID string, op orchapi.TaskStartOp, parent api.Address, req orchapi.TaskStartRequest) (orchapi.Task, error) {
	cmd, _ := s.d.Config.File.SandboxTasks()
	if cmd == "" {
		return orchapi.Task{}, api.Errorf(api.CodeForbidden, "%s takes no sandbox tasks", s.d.Config.Machine).
			WithHint("its owner sets \"sandbox_task_cmd\" (a task driver) in its zm config.json and restarts its daemon")
	}
	if strings.TrimSpace(req.Prompt) == "" || len(req.Prompt) > 256<<10 || len(req.Model) > 200 {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "prompt or model is out of bounds")
	}
	if req.Agent != "claude" && req.Agent != "codex" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "agent must be claude or codex")
	}
	if req.Cwd != "" {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "a sandbox task makes no checkout on its machine: leave cwd out")
	}
	if !sandboxOpID.MatchString(opID) {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "task_start id %q is not a task id", opID)
	}
	in := sandboxTaskInput{TaskID: strings.SplitN(opID, ".", 2)[0], OpID: opID, Project: proj, Parent: parent.String(), TimeLeftS: s.taskTimeout(req), Request: req}
	in.Request.Machine, in.Request.Worktree = "", nil
	stdin, _ := json.Marshal(in)
	dir := filepath.Join(s.sandboxDir(), opID)
	no := false
	req.Worktree, req.Cwd, req.Machine, req.ClientRequestID = &no, dir, "", opID
	b, _ := json.Marshal(req)
	sub := s.d.Projects.Local(parent.String())
	ctx = core.WithSubject(ctx, sub)
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	var started string
	if s.d.Store.Registry().QueryRowContext(ctx, `SELECT id FROM session_tasks WHERE parent=? AND request_id=?`, parent.String(), opID).Scan(&started) == nil {
		return s.getTask(ctx, sub, started) // a repeat of the op: its directory may be gone with it
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return orchapi.Task{}, err
	}
	rs := &remoteStart{project: proj, depth: op.Depth, command: []string{cmd}, sandbox: &sandboxStart{taskID: in.TaskID, result: filepath.Join(dir, "result")}}
	t, err := s.startTaskLocked(ctx, sub, orchapi.Session{Address: parent.String()}, req, string(b), string(stdin)+"\n", "", rs)
	if err != nil {
		var exists bool
		_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_tasks WHERE parent=? AND request_id=?)`, parent.String(), opID).Scan(&exists)
		if !exists {
			_ = os.RemoveAll(dir)
		}
	}
	return t, err
}

// taskTimeout is req's overall deadline in seconds (insertTask's rule).
func (s *Service) taskTimeout(req orchapi.TaskStartRequest) int {
	timeout := req.TimeoutSeconds
	if timeout == 0 {
		timeout = s.d.Config.File.TaskDeadlineSeconds
	}
	if timeout <= 0 {
		timeout = 4 * 60 * 60
	}
	return timeout
}

// sandboxOutcome is a script run's outcome, said of the driver.
func sandboxOutcome(failure string, process *orchapi.Outcome) orchapi.Outcome {
	o := runOutcome(failure, process)
	o.Evidence = strings.Replace(o.Evidence, "script exit", "task driver exit", 1)
	return o
}

// sandboxResult is a sandbox task's result: the driver's result file (a
// JSON object of finished and text, or plain text), else its output's
// tail. Exit 0 is done unless the driver says not_done.
func sandboxResult(dir string, transcript []byte, outcome orchapi.Outcome) taskEventResult {
	r := runResult(transcript, outcome)
	b, ok := readSandboxResult(dir)
	if !ok {
		return r
	}
	var report struct {
		Finished string `json:"finished"`
		Text     string `json:"text"`
	}
	if json.Unmarshal(b, &report) == nil && (report.Finished == "done" || report.Finished == "not_done") {
		r.Text = report.Text
		if r.Success {
			r.Finished = report.Finished
		}
		return r
	}
	r.Text = strings.ToValidUTF8(strings.TrimRight(string(b), "\n"), "�")
	return r
}

// readSandboxResult reads at most sandboxResultMax of dir's result file,
// never following a link the driver left there.
func readSandboxResult(dir string) ([]byte, bool) {
	if dir == "" {
		return nil, false
	}
	f, err := os.OpenFile(filepath.Join(dir, "result"), os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, sandboxResultMax))
	return b, err == nil
}

// removeSandboxDir removes an ended sandbox task's directory, only one
// directly under the sandbox tasks directory.
func removeSandboxDir(root, dir string) {
	if dir != "" && filepath.Dir(dir) == root {
		_ = os.RemoveAll(dir)
	}
}

// taskChildEnv is a task child's environment: a script run's and a task
// driver's only the allowlisted variables, a driver's with its own two.
func taskChildEnv(task *taskStart, env map[string]string) map[string]string {
	if task == nil || !task.run && task.sandbox == nil {
		return env
	}
	env = runFilterEnv(env)
	if task.sandbox != nil {
		env["ZM_TASK_ID"], env["ZM_TASK_RESULT"] = task.sandbox.taskID, task.sandbox.result
	}
	return env
}

// sandboxStopPlan is a stop's escalation for a task driver: TERM and the
// time to release its box, then as every escalation HUP, and KILL when
// forced.
// Any other stop keeps plan.
func sandboxStopPlan(agent string, req orchapi.StopRequest, plan []stopStep) []stopStep {
	if agent != taskAgentSandbox || !req.Escalate {
		return plan
	}
	plan = []stopStep{{syscall.SIGTERM, "TERM", SandboxTermWait}, {syscall.SIGHUP, "HUP", StopHupWait}}
	if req.Force {
		plan = append(plan, stopStep{syscall.SIGKILL, "KILL", StopKillWait})
	}
	return plan
}

// stopSandbox stops sandbox task id off the caller (its stop takes up to
// SandboxTermWait), once at a time.
func (s *Service) stopSandbox(id string) {
	if _, busy := s.sandboxStops.LoadOrStore(id, true); busy {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.sandboxStops.Delete(id)
		_, _ = s.Stop(s.ctx, s.d.System(), id, orchapi.StopRequest{Escalate: true, Force: true})
		s.completeTask(id)
	}()
}
