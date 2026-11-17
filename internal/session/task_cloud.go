package session

// Cloud tasks (SPEC 8 "Cloud tasks"): task_start with where "cloud" runs the
// task on the provider's cloud computer (claude --cloud, codex cloud exec)
// through this machine's own CLI login. The task's child is the zm binary as
// a watcher (taskcloud) under the usual holder, so a cloud task keeps a local
// task's lifecycle: parent and depth limits, deadline, cancel, the result
// mail and the work comment. Only a project whose project.toml names the
// runtime's environment in [cloud] sends a provider code.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/mezze/internal/session/taskcloud"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// cloudSectionName is project.toml's [cloud] table.
const cloudSectionName = "cloud"

// cloudPromptLimit keeps the prompt one argv string (Linux caps one at 128 KiB).
const cloudPromptLimit = 100 << 10

// The watcher's first status check and the longest wait between two
// (Service.CloudPoll overrides them in tests).
const (
	cloudPollFirst = time.Minute
	cloudPollMax   = 10 * time.Minute
)

func init() { project.RegisterSection(cloudSectionName, cloudSection{}) }

// cloudAgents maps a runtime to its cloud task session's agent name, so
// nothing that treats a claude or codex session as that runtime's local CLI
// (task context, output schema, hooks, usage) touches the watcher.
var cloudAgents = map[string]string{"claude": "claude-cloud", "codex": "codex-cloud"}

func isCloudAgent(agent string) bool { return agent == "claude-cloud" || agent == "codex-cloud" }

// cloudEnvRE is a provider environment id: never a flag, never a path.
var cloudEnvRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)

type cloudSection struct{}

func (cloudSection) Parse(where string, raw any) (any, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: cloud must be one [cloud] table", where)
	}
	var c orchapi.TaskCloudSettings
	for k, v := range m {
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s [cloud]: %s must be a string", where, k)
		}
		switch k {
		case "claude":
			c.Claude = s
		case "codex":
			c.Codex = s
		default:
			return nil, fmt.Errorf("%s [cloud]: unknown key %s (claude, codex)", where, k)
		}
	}
	return c, nil
}

func (cloudSection) Validate(v any, _ project.File) error {
	c, _ := v.(orchapi.TaskCloudSettings)
	for k, env := range map[string]string{"claude": c.Claude, "codex": c.Codex} {
		if env != "" && !cloudEnvRE.MatchString(env) {
			return api.Errorf(api.CodeBadRequest, "[cloud] %s: %q is not an environment id", k, env)
		}
	}
	if c.Codex == taskcloud.DefaultEnvironment {
		return api.Errorf(api.CodeBadRequest, "[cloud] codex: codex cloud exec needs an environment id (default is Claude's)")
	}
	return nil
}

func (cloudSection) Encode(v any) string {
	c, _ := v.(orchapi.TaskCloudSettings)
	if c.Claude == "" && c.Codex == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n[cloud]\n")
	if c.Claude != "" {
		fmt.Fprintf(&b, "claude = %s\n", project.TOMLString(c.Claude))
	}
	if c.Codex != "" {
		fmt.Fprintf(&b, "codex = %s\n", project.TOMLString(c.Codex))
	}
	return b.String()
}

func (s *Service) cloudSettings(id string) orchapi.TaskCloudSettings {
	c, _ := s.d.Projects.Section(id, cloudSectionName).(orchapi.TaskCloudSettings)
	return c
}

// cloudEnvironment is the environment project p set for runtime's cloud
// tasks; a project that set none has not opted in.
func (s *Service) cloudEnvironment(p api.ProjectRef, runtime string) (string, error) {
	c := s.cloudSettings(p.ID)
	env := c.Claude
	if runtime == "codex" {
		env = c.Codex
	}
	if env == "" {
		return "", api.Errorf(api.CodeBadRequest, "project %s has not opted in to %s cloud tasks: zm sends no code to a provider for it", or(p.Slug, p.ID), runtime).
			WithHint("only this machine's owner opts a project in (zm project cloud, from a terminal); ask them, or start the task here")
	}
	return env, nil
}

// cloudCheckout reports whether cwd, whose repo the provider would clone,
// is project id's by the rules this machine's owner set on it (its paths,
// its remotes). A --project, $ZM_PROJECT, the session's project or a repo's
// own .zm/project.toml name a project, never which repo goes out.
func (s *Service) cloudCheckout(id, cwd string) bool {
	cwd = project.ExpandPath(cwd)
	// The repo's root, not cwd, is what goes out: a nested repo or a wider
	// monorepo under a path rule is not that project's checkout.
	root := project.GitRoot(cwd)
	if pid, _, _ := s.d.Projects.MatchPath(authz.All(), or(root, cwd)); pid == id {
		return true
	}
	f, ok := s.d.Projects.File(id)
	if root == "" || !ok {
		return false
	}
	remote := project.NormalizeRemote(project.OriginURL(root))
	remotes := append([]string(nil), f.Repos.Remotes...)
	for _, a := range f.Areas {
		remotes = append(remotes, a.Remotes...)
	}
	for _, r := range remotes {
		if remote != "" && project.NormalizeRemote(r) == remote {
			return true
		}
	}
	return false
}

func (s *Service) hTaskCloudGet(w http.ResponseWriter, r *http.Request) {
	t, err := s.d.WriteTarget(r.Context(), "", authz.Read)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	c := s.cloudSettings(t.Project.ID)
	c.Project = t.Project.Slug
	api.WriteJSON(w, http.StatusOK, c)
}

func (s *Service) hTaskCloudPut(w http.ResponseWriter, r *http.Request) {
	var u orchapi.TaskCloudUpdate
	if err := api.DecodeJSON(r, &u); err != nil {
		api.WriteError(w, err)
		return
	}
	ctx := r.Context()
	// An agent must not opt its own project in: sending code to a provider
	// is the machine owner's call.
	if sub, _, err := s.processCaller(ctx, core.SubjectOf(ctx)); err != nil || sub.Session != "" {
		api.WriteError(w, api.Errorf(api.CodeForbidden, "a session (a zm shell session too) cannot opt a project in to cloud tasks").
			WithHint("this machine's owner runs zm project cloud from a terminal no zm session started"))
		return
	}
	t, err := s.d.WriteTarget(ctx, "", authz.Admin)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if _, ok := s.d.Projects.File(t.Project.ID); !ok {
		api.WriteError(w, api.NotFound("project", t.Project.Slug).WithHint("a project joined from another machine opts in there"))
		return
	}
	v, err := s.d.Projects.SetSection(ctx, t.Project.ID, cloudSectionName, func(old any) (any, error) {
		c, _ := old.(orchapi.TaskCloudSettings)
		if u.Claude != nil {
			c.Claude = strings.TrimSpace(*u.Claude)
		}
		if u.Codex != nil {
			c.Codex = strings.TrimSpace(*u.Codex)
		}
		if c.Claude == "" && c.Codex == "" {
			return nil, nil
		}
		return c, nil
	})
	if err != nil {
		api.WriteError(w, err)
		return
	}
	c, _ := v.(orchapi.TaskCloudSettings)
	c.Project = t.Project.Slug
	api.WriteJSON(w, http.StatusOK, c)
}

// startCloudTask starts req as a cloud task of the caller (sub's session).
func (s *Service) startCloudTask(ctx context.Context, sub *authz.Subject, req orchapi.TaskStartRequest, project, where string) (orchapi.Task, error) {
	switch {
	case where != orchapi.TaskWhereCloud:
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "where must be cloud, or empty for this machine")
	case req.Machine != "" || project != "" || req.Front:
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "a cloud task starts from this machine, on its own provider login: drop machine, project and front")
	case isRun(req) || req.Lookup:
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "where cloud is for a claude or codex task")
	case req.ContinueFrom != "" || req.OnLimit != "":
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "a cloud task takes no continue_from or on_limit")
	case req.Model != "":
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "a cloud task takes no model: the provider's environment picks it")
	case req.Worktree != nil && *req.Worktree:
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "a cloud task has no local worktree: the provider clones the repo")
	case cloudAgents[req.Agent] == "":
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "agent must be claude or codex")
	case strings.TrimSpace(req.Prompt) == "":
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "prompt is required")
	case len(req.Prompt) > cloudPromptLimit:
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "a cloud task's prompt is at most %d bytes", cloudPromptLimit)
	case req.TimeoutSeconds < 0 || req.TimeoutSeconds > 7*24*60*60:
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "timeout_s must be between 0 and 604800")
	case len(req.ClientRequestID) > 200:
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "client_request_id is too long")
	}
	parent, ok := s.Lookup(ctx, sub.Session)
	if !ok || parent.State == orchapi.StateEnded {
		return orchapi.Task{}, api.Errorf(api.CodeBadRequest, "parent session is not live")
	}
	if req.Cwd == "" {
		req.Cwd = core.ResolveInputOf(ctx).Cwd
	}
	if req.Cwd == "" {
		req.Cwd = parent.Cwd
	}
	no := false
	req.Worktree = &no
	b, _ := json.Marshal(struct {
		orchapi.TaskStartRequest
		Where string `json:"where"`
	}{req, orchapi.TaskWhereCloud})
	s.taskMu.Lock()
	defer s.taskMu.Unlock()
	return s.startTaskLocked(ctx, sub, parent, req, string(b), "", "", nil)
}

// cloudCommand is a cloud task's child: this binary as the watcher. Its
// environment slot stays empty until spawn has resolved the task's project
// (withCloudEnvironment).
func (s *Service) cloudCommand(runtime string, env map[string]string) ([]string, error) {
	cli := s.agentCommand(runtime, env)
	bin, err := lookPath(cli[0], env["PATH"])
	if err != nil {
		return nil, api.Errorf(api.CodeUnavailable, "%s cloud tasks run the %s CLI on this machine: %v", runtime, runtime, err)
	}
	exe, err := s.holderExe()
	if err != nil {
		return nil, err
	}
	first, max := cloudPollFirst, cloudPollMax
	if s.CloudPoll > 0 {
		first, max = s.CloudPoll, s.CloudPoll
	}
	return append([]string{exe}, taskcloud.Arguments(runtime, "", bin, first, max)...), nil
}

// cloudArgv is spawn's check of a cloud task (else argv as is): the project
// it landed in opted its runtime in and cwd is that project's checkout. It
// fills cloudCommand's environment slot.
func (s *Service) cloudArgv(task *taskStart, p api.ProjectRef, cwd string, argv []string) ([]string, error) {
	if task == nil || task.cloud == "" {
		return argv, nil
	}
	env, err := s.cloudEnvironment(p, task.cloud)
	if err != nil {
		return nil, err
	}
	if !s.cloudCheckout(p.ID, cwd) {
		return nil, api.Errorf(api.CodeBadRequest, "cwd %q is not project %s's checkout by its paths or remotes: a cloud task sends the provider that repo", cwd, or(p.Slug, p.ID)).
			WithHint("start it from the project's own checkout")
	}
	out := append([]string(nil), argv...)
	out[3] = env // exe, Arg, agent, env, ...
	return out, nil
}

// taskStdin is what a task's child reads: the prompt and the report request,
// nothing for a script, and a cloud task's prompt as given (zm never reads
// the cloud agent's report).
func taskStdin(task *taskStart, prompt string) string {
	switch {
	case task.sandbox != nil:
		return prompt // the task as JSON (task_sandbox.go)
	case task.run:
		return ""
	case task.cloud != "":
		return prompt
	}
	return prompt + "\n\nEnd your response with a JSON object containing exactly finished (done or not_done) and text (a concise explanation). Use not_done unless all requested work is complete.\n"
}

// cloudTaskAgent is a task's session agent and, for a cloud task, its runtime.
func cloudTaskAgent(req orchapi.TaskStartRequest, requestJSON string) (agent, cloud string) {
	if isCloudRequest(requestJSON) {
		return cloudAgents[req.Agent], req.Agent
	}
	return req.Agent, ""
}

// cloudInfo is what a cloud task's watcher last printed about the provider's
// task; an ended task whose watcher printed nothing is unknown.
func (s *Service) cloudInfo(ctx context.Context, id, agent string, ended bool) *orchapi.TaskCloud {
	out, err := os.ReadFile(holder.TaskOutputPath(s.d.Config.Home, id))
	if err != nil {
		_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT transcript FROM session_tasks WHERE id=?`, id).Scan(&out)
	}
	c := &orchapi.TaskCloud{Agent: strings.TrimSuffix(agent, "-cloud"), Status: "submitting"}
	if ended {
		c.Status = "unknown"
	}
	if i, ok := lastCloudLine(out); ok {
		c.Environment, c.ID, c.URL, c.Status = i.Environment, i.ID, i.URL, i.Status
	}
	return c
}

func lastCloudLine(out []byte) (taskcloud.Info, bool) {
	var last taskcloud.Info
	found := false
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 4096), taskTranscriptLimit)
	for sc.Scan() {
		var l taskcloud.Line
		if json.Unmarshal(bytes.TrimSpace(sc.Bytes()), &l) == nil && l.Cloud != nil {
			last, found = *l.Cloud, true
		}
	}
	return last, found
}

// cloudOutcome is a script run's outcome with the watcher's word as evidence.
func cloudOutcome(failure string, process *orchapi.Outcome, transcript []byte) orchapi.Outcome {
	o := runOutcome(failure, process)
	if i, ok := lastCloudLine(transcript); ok && strings.HasPrefix(o.Evidence, "script exited") {
		o.Evidence = fmt.Sprintf("%s cloud task %s %s", i.Agent, or(i.ID, "(no id)"), i.Status)
	}
	return o
}

// cloudEnd is an ended cloud task's outcome and result.
func cloudEnd(failure string, process *orchapi.Outcome, transcript []byte) (orchapi.Outcome, taskEventResult) {
	o := cloudOutcome(failure, process, transcript)
	return o, cloudResult(transcript, o)
}

// cloudResult is a cloud task's result: the watcher's last text. Finished
// is unreported: zm never reads the cloud agent's own word, and its work
// waits on the provider (a diff to apply, a session to teleport).
func cloudResult(transcript []byte, outcome orchapi.Outcome) taskEventResult {
	r := taskEventResult{Complete: true, Success: outcome.Kind == orchapi.OutcomeOK, Kind: outcome.Kind, Evidence: outcome.Evidence, Finished: "unreported"}
	if i, ok := lastCloudLine(transcript); ok && i.Text != "" {
		r.Text = i.Text
	} else {
		r.Text = runText(transcript, runTextTail)
	}
	return r
}

// taskView is t as the task routes answer it: a cloud task with its cloud view.
func (s *Service) taskView(ctx context.Context, t orchapi.Task) any {
	if !isCloudAgent(t.Agent) {
		return t
	}
	return orchapi.TaskView{Task: t, Cloud: s.cloudInfo(ctx, t.ID, t.Agent, t.State == "ended")}
}

// isCloudRequest reports a stored task request startCloudTask made.
func isCloudRequest(requestJSON string) bool {
	var w struct {
		Where string `json:"where"`
	}
	return json.Unmarshal([]byte(requestJSON), &w) == nil && w.Where == orchapi.TaskWhereCloud
}
