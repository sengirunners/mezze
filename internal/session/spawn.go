package session

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/mezze/internal/session/tasklaunch"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/project"
)

// knownAgents are runtimes zm knows how to ask for a model and a version.
var knownAgents = orchapi.AgentRuntimes

// agentCommand is the argv for an agent kind: config.json "agents", else the
// built-in default, else the agent name itself.
func (s *Service) agentCommand(agent string, env map[string]string) []string {
	if c := s.d.Config.File.Agents[agent]; len(c) > 0 {
		return append([]string(nil), c...)
	}
	if agent == "shell" {
		for _, sh := range []string{env["SHELL"], "/bin/sh"} {
			if sh != "" {
				return []string{sh}
			}
		}
	}
	return []string{agent}
}

// modelOf finds a model flag in argv (--model X, --model=X, -m X).
func modelOf(argv []string) string {
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		if v, ok := strings.CutPrefix(a, "--model="); ok {
			return v
		}
		if (a == "--model" || a == "-m") && i+1 < len(argv) {
			return argv[i+1]
		}
	}
	return ""
}

// codexInitialPrompt recognizes a new interactive Codex launch with no
// positional prompt. Be conservative: wrappers, subcommands, explicit prompts
// and options whose arity we do not know retain the caller's command behavior.
func codexInitialPrompt(agent string, argv []string) bool {
	if agent != "codex" || len(argv) == 0 || filepath.Base(argv[0]) != "codex" {
		return false
	}
	for i := 1; i < len(argv); i++ {
		a, _, equal := strings.Cut(argv[i], "=")
		switch a {
		case "--":
			return i == len(argv)-1
		case "--model", "-m", "--config", "-c", "--profile", "-p", "--sandbox", "-s",
			"--ask-for-approval", "-a", "--cd", "-C", "--image", "-i", "--add-dir",
			"--enable", "--disable", "--local-provider", "--remote", "--remote-auth-token-env":
			if !equal {
				i++
				if i == len(argv) {
					return false
				}
			}
		case "--full-auto", "--dangerously-bypass-approvals-and-sandbox", "--yolo",
			"--dangerously-bypass-hook-trust", "--no-alt-screen", "--oss", "--search",
			"--approve-for-me", "--not-so-yolo", "--no-daemon", "--strict-config", "--worktree":
			if equal {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// strippedEnv are variables a child must not inherit: other zm identities,
// a project override (it would beat the cwd rules), and parent agent markers.
func stripped(k string) bool {
	return strings.HasPrefix(k, "ZM_") || config.IsAgentEnv(k)
}

// envMap is the daemon's environment overlaid with the client's.
func envMap(extra map[string]string) (map[string]string, error) {
	m := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	for k, v := range extra {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.Contains(v, "\x00") {
			return nil, api.Errorf(api.CodeBadRequest, "bad env name %q", k)
		}
		m[k] = v
	}
	for k := range m {
		if stripped(k) {
			delete(m, k)
		}
	}
	return m, nil
}

func (s *Service) childEnv(m map[string]string, addr, scratch string, task *taskStart) []string {
	cfg := s.d.Config
	s.identityEnv(m, addr, scratch, task)
	m[orchapi.EnvHome], m[config.EnvMachine], m[config.EnvHuman] = cfg.Home, cfg.Machine, cfg.Human
	if m["TERM"] == "" || m["TERM"] == "dumb" {
		m["TERM"] = "xterm-256color"
	}
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// version runs `<bin> --version` (known agents only; cached per binary).
func (s *Service) version(agent, bin string, env []string, cachedOnly bool) string {
	if !knownAgents[agent] || filepath.Base(bin) != agent {
		return ""
	}
	fi, err := os.Stat(bin)
	if err != nil {
		return ""
	}
	key := fmt.Sprintf("%s|%d|%d", bin, fi.ModTime().UnixNano(), fi.Size())
	s.vmu.Lock()
	v, ok := s.versions[key]
	s.vmu.Unlock()
	if ok {
		return v
	}
	if cachedOnly {
		// A task's metadata must not execute its agent before the holder can
		// establish never-started proof. Ordinary sessions populate this cache.
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "--version")
	cmd.Env = env
	out, _ := cmd.Output()
	for _, l := range strings.Split(string(out), "\n") {
		if l = oneLine(l); l != "" {
			v = l
			break
		}
	}
	if len(v) > 80 {
		v = v[:80]
	}
	s.vmu.Lock()
	s.versions[key] = v
	s.vmu.Unlock()
	return v
}

// target resolves the project/area a session goes to (SPEC section 5 with
// the spawn's cwd); nothing resolving means project none. An explicit
// project that does not resolve is not_found, never none.
func (s *Service) target(ctx context.Context, cwd, area string, a authz.Action, lenient bool) (api.ProjectRef, string, error) {
	in := core.ResolveInputOf(ctx)
	if cwd != "" && core.IsLocal(ctx) {
		in.Cwd = cwd
	}
	if tr, ok := orchcore.Team(s.d).(orchcore.TeamReplica); ok && core.IsLocal(ctx) && core.SubjectOf(ctx).Via == authz.ViaLocal {
		// Sessions on a replica are local (SPEC 17.3): one started in a team
		// project this machine joined belongs to it, owned by this OS user.
		p, ar, ok, err := tr.TeamTarget(core.WithResolveInput(ctx, in), in.Cwd, area)
		if err != nil {
			if lenient {
				return api.ProjectRef{ID: api.ProjectNone, Slug: api.ProjectNone}, "", nil
			}
			return api.ProjectRef{}, "", err
		}
		if ok {
			return p, ar, nil
		}
	}
	t, err := s.d.WriteTarget(core.WithResolveInput(ctx, in), area, a)
	if err == nil {
		return t.Project, t.Area, nil
	}
	if lenient || (api.IsCode(err, api.CodeBadRequest) && in.Explicit == "" && area == "") {
		sub := core.SubjectOf(ctx)
		if e := sub.Check(a, authz.Object{Project: api.ProjectNone}, "project", api.ProjectNone); e != nil {
			return api.ProjectRef{}, "", err
		}
		return api.ProjectRef{ID: api.ProjectNone, Slug: api.ProjectNone}, "", nil
	}
	return api.ProjectRef{}, "", err
}

// Admission is the current probe and verdict (admit, for a session).
func (s *Service) Admission(cwd string) orchapi.Admission { return s.admit(cwd, false) }

// Spawn starts an agent in a new PTY, held by its own holder process (holders.go), as the caller in ctx.
func (s *Service) Spawn(ctx context.Context, req orchapi.SpawnRequest) (orchapi.Session, error) {
	if req.Machine != "" && req.Machine != s.d.Config.Machine {
		return s.startSessionOn(ctx, req)
	}
	if req.Machine == "" && req.ClientRequestID != "" {
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "client_request_id is for a session on another machine (machine)")
	}
	req.Machine, req.ClientRequestID = "", "" // this machine by name starts here, as task_start's; its retry key goes too
	return s.spawnEchoingCaps(ctx, req)
}

func (s *Service) spawn(ctx context.Context, req orchapi.SpawnRequest, task *taskStart) (orchapi.Session, error) {
	return s.spawnFor(ctx, req, task, nil)
}

// spawnFor is spawn; pin is set for a session another machine asked for
// (pipe_remote.go).
func (s *Service) spawnFor(ctx context.Context, req orchapi.SpawnRequest, task *taskStart, pin *remotePin) (orchapi.Session, error) {
	ctx, sub, inherited, lineageParent, lineageDepth, err := s.spawnCaller(ctx, task, pin)
	if err != nil {
		return orchapi.Session{}, err
	}
	agent := strings.ToLower(strings.TrimSpace(req.Agent))
	if agent == "" && len(req.Command) > 0 {
		agent = config.Sanitize(filepath.Base(req.Command[0]))
	}
	if !api.ValidName(agent) {
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "bad agent name %q", req.Agent).
			WithHint("agent is a name like claude, codex, opencode, gemini, shell; pass the program in command")
	}
	if err := sandboxAgentOK(agent, task); err != nil {
		return orchapi.Session{}, err
	}
	env, err := envMap(req.Env)
	if err != nil {
		return orchapi.Session{}, err
	}
	forID, jobKind, err := attribution(req.For, req.JobKind)
	if err != nil {
		return orchapi.Session{}, err
	}
	env = taskChildEnv(task, env)
	argv := append([]string(nil), req.Command...)
	if len(argv) == 0 {
		argv = s.agentCommand(agent, env)
	}
	if len(argv) == 0 || argv[0] == "" {
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "empty command")
	}
	pipe := false
	switch req.Mode {
	case "", "pty":
		if req.Approvals != "" || req.Resume != "" || len(req.MCP) > 0 || len(req.AllowedTools) > 0 || req.PermissionMode != "" || len(req.SafeOutputs) > 0 {
			return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "approvals, resume, mcp, allowed_tools, permission_mode and safe_outputs are for a structured session (mode pipe)")
		}
	case orchapi.SessionModePipe:
		pipe = true
	default:
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "mode must be pty or pipe, not %q", req.Mode)
	}
	var adapter []string
	var caps orchapi.AgentCapabilities
	if pipe {
		switch {
		case task != nil:
			return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "a task is one-shot: it has no pipe mode")
		case req.Approvals != "" && req.Approvals != "ask" && req.Approvals != "never":
			return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "approvals must be ask or never, not %q", req.Approvals)
		}
		if adapter, caps, err = s.pipeAdapter(agent, env["PATH"]); err != nil {
			return orchapi.Session{}, err
		}
		if err := pipeCapsFit(agent, req, caps); err != nil {
			return orchapi.Session{}, err
		}
	}
	model := strings.TrimSpace(req.Model)
	if pipe {
		// The adapter hands the model to the runtime its own way.
	} else if found := modelOf(argv); model != "" && found == "" {
		if !knownAgents[agent] {
			return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "zm does not know how to ask %s for a model", agent).
				WithHint("put the model flag in the command itself")
		}
		argv = append(argv, "--model", model)
	} else if found != "" {
		if model != "" && model != found {
			return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "model %q conflicts with %q in the command", model, found)
		}
		model = found
	}
	bin, err := lookPath(argv[0], env["PATH"])
	if err != nil {
		return orchapi.Session{}, err
	}
	cwd := req.Cwd
	if fi, err := os.Stat(cwd); cwd == "" || err != nil || !fi.IsDir() || !filepath.IsAbs(cwd) {
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "cwd %q is not an absolute directory", cwd)
	}
	src, firstTurn, err := s.pipeContinuation(ctx, sub, req, pipe && task == nil)
	if err != nil {
		return orchapi.Session{}, err
	}
	var proj api.ProjectRef
	var area string
	if task != nil && task.source != "" && task.project == api.ProjectNone {
		if err := sub.Check(authz.Operate, authz.Object{Project: api.ProjectNone}, "project", api.ProjectNone); err != nil {
			return orchapi.Session{}, err
		}
		proj = api.ProjectRef{ID: api.ProjectNone, Slug: api.ProjectNone}
	} else {
		// A continuation stays in its source's project; a task an authority
		// asked for (SPEC 8 "Machine choice") starts in the project it came in.
		pinned := task != nil && (task.source != "" || task.remote) || pin != nil
		if pinned {
			in := core.ResolveInputOf(ctx)
			if pin != nil {
				in.Cwd, in.Explicit = cwd, pin.project
			} else {
				if task.remote {
					in.Cwd = cwd
				}
				in.Explicit = task.project
			}
			in.ExplicitFrom = project.RuleFlag
			ctx = core.WithResolveInput(ctx, in)
		}
		handled := false
		if pinned && sub.Via == authz.ViaLocal {
			if tr, ok := orchcore.Team(s.d).(orchcore.TeamReplica); ok {
				proj, area, handled, err = tr.TeamTarget(ctx, cwd, req.Area)
				if err != nil {
					return orchapi.Session{}, err
				}
				if handled {
					if err = sub.Check(authz.Operate, authz.Object{Project: proj.ID, Area: area}, "project", proj.Slug); err != nil {
						return orchapi.Session{}, err
					}
				}
			}
		}
		if !handled {
			proj, area, err = s.target(ctx, cwd, req.Area, authz.Operate, false)
			if err != nil {
				return orchapi.Session{}, err
			}
		}
		if task != nil && task.source != "" && (proj.ID != task.project || area != task.area) {
			return orchapi.Session{}, api.Errorf(api.CodeConflict, "continuation project or area changed")
		}
		if req.ContinueFrom != "" && (proj.ID != src.Project || area != src.Area) {
			return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "continue_from stays in its source's project and area")
		}
		if pin != nil && proj.ID != pin.project {
			return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "%s's checkout is not in the project here", s.d.Config.Machine)
		}
		if task != nil && task.remote && proj.ID != task.project {
			return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "cwd %q is not in the task's project here", cwd).
				WithHint("pass cwd: a checkout of the project on that machine")
		}
	}
	if argv, err = s.cloudArgv(task, proj, cwd, argv); err != nil {
		return orchapi.Session{}, err
	}
	if req.Shared && !s.teamProject(proj.ID) {
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "only a session in a team project can be shared").
			WithHint("make the project a team project (zm project team <slug>), or start it unshared")
	}
	// A joined project's item is claimed at its authority.
	var teamLeases orchcore.TeamLeases
	if req.Work != "" && proj.ID != api.ProjectNone && s.d.Projects.Slug(proj.ID) == "" {
		var ok bool
		if teamLeases, ok = orchcore.Team(s.d).(orchcore.TeamLeases); !ok {
			return orchapi.Session{}, api.Errorf(api.CodeUnavailable, "this daemon cannot take a lease at the project's authority").
				WithHint("start the session without --work and claim the item from it: zm work claim " + req.Work)
		}
	}
	srt := ""
	if req.Sandbox {
		if srt, err = lookPath("srt", env["PATH"]); err != nil {
			return orchapi.Session{}, errNoSandbox()
		}
		if proj.ID == api.ProjectNone {
			return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "--sandbox needs a project: its repos are what the agent may write").
				WithHint("run it in a project's repo, or pass --project")
		}
	}
	rows, cols := req.Rows, req.Cols
	if rows <= 0 || rows > 1000 {
		rows = 24
	}
	if cols <= 0 || cols > 1000 {
		cols = 80
	}

	name, err := cleanName(req.Name)
	if err != nil {
		return orchapi.Session{}, err
	}
	s.spawnMu.Lock()
	defer s.spawnMu.Unlock()
	if _, taken, _ := byName(ctx, s.d.Store.Registry(), name); name != "" && taken {
		return orchapi.Session{}, s.nameConflict(ctx, sub, name)
	}
	if adm := s.admit(cwd, task != nil && task.sandbox != nil); !adm.OK {
		_, _ = s.d.Events.Publish(context.Background(), events.Input{Type: orchapi.EventSessionAdmissionRefused,
			Subject: s.d.Config.Machine, Data: adm})
		return orchapi.Session{}, api.Errorf(api.CodeUnavailable, "not starting a session: %s", adm.Detail).WithData(adm).
			WithHint("free memory or disk, or stop a session (zm session stop <id>); thresholds: config.json \"admission\"")
	}
	reg := s.d.Store.Registry()
	var id8 string
	for {
		id8 = api.NewSession8()
		taken, err := idExists(ctx, reg, id8)
		if err != nil {
			return orchapi.Session{}, err
		}
		if !taken && s.proc(id8) == nil {
			break
		}
	}
	addr := api.SessionAddress(s.d.Config.Machine, id8)
	scratch := filepath.Join(s.d.Config.Home, config.DirScratch, id8)
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return orchapi.Session{}, err
	}
	if task != nil && agent == "codex" {
		path := filepath.Join(scratch, "task-result-schema.json")
		if err := os.WriteFile(path, []byte(taskResultSchema), 0600); err != nil {
			_ = os.RemoveAll(scratch)
			return orchapi.Session{}, err
		}
		// The positional '-' must stay after options.
		argv = append(argv[:len(argv)-1], "--output-schema", path, "-")
	}
	var wt, branch string
	undo := func() {
		removeWorktree(context.Background(), wt, branch)
		_ = os.RemoveAll(scratch)
	}
	if req.Worktree {
		base := "HEAD"
		if task != nil && task.baseBranch != "" {
			base = "refs/heads/" + task.baseBranch
		}
		if wt, branch, cwd, err = addWorktreeFrom(ctx, s.d.Config.Home, cwd, id8, base); err != nil {
			undo()
			return orchapi.Session{}, task.refusedCheckout(err)
		}
	}
	guardClaudeSkills, agentHome := false, env[agentHomeEnv(agent)]
	if task != nil && agent == "claude" {
		argv, guardClaudeSkills, err = s.prepareClaudeTaskContext(ctx, bin, argv, env, cwd, scratch, addr)
	} else if task != nil && agent == "codex" {
		err = prepareCodexTaskHome(s.d.Config.UserHome, scratch, env)
	}
	if err != nil {
		undo()
		return orchapi.Session{}, err
	}
	exe := append([]string{bin}, argv[1:]...)
	if pipe {
		exe = append(append(append([]string(nil), adapter...), "--"), exe...)
	}
	policy := ""
	if req.Sandbox {
		var repos []string
		if p, err := s.d.Projects.Get(authz.All(), proj.ID); err == nil {
			repos = append(repos, p.Paths...)
			for _, a := range p.Areas {
				if area == "" || a.Name == area {
					repos = append(repos, a.Paths...)
				}
			}
		}
		repos = project.ExpandRoots(repos) // a glob rule allows the directories it matches now
		if policy, err = writeSandboxPolicy(scratch, sandboxPaths(repos, agent, wt, scratch, env)); err != nil {
			undo()
			return orchapi.Session{}, err
		}
		exe = sandboxArgv(srt, policy, exe)
	}
	var guardedCommand []string
	if guardClaudeSkills {
		launcher, e := s.holderExe()
		if e != nil {
			undo()
			return orchapi.Session{}, e
		}
		probe := []string{bin, "--version"}
		if srt != "" {
			probe = sandboxArgv(srt, policy, probe)
		}
		guardedCommand = append([]string{launcher}, tasklaunch.Arguments(probe, exe)...)
	}
	if task != nil {
		env["ZM_TASK_DEPTH"] = fmt.Sprint(task.depth)
	}
	account := ""
	if agent == "claude" {
		account = s.claudeAccount(agentHome)
	}
	if forID != "" {
		env[envFor] = forID
	}
	if jobKind != "" {
		env[envJobKind] = jobKind
	}
	childEnv := s.childEnv(env, addr, scratch, task)
	now := storedTime(time.Now())
	r := row{Session: orchapi.Session{Address: addr, ID: id8, Machine: s.d.Config.Machine, Project: proj.ID, Area: area,
		Launcher: sub.Identity.ID, Agent: agent, Command: exe, Version: s.version(agent, bin, childEnv, task != nil), Model: model,
		Cwd: cwd, Worktree: wt, Branch: branch, Scratch: scratch, Sandbox: policy, Title: oneLine(textclean.Clean(req.Title)),
		State: orchapi.StateRunning, Work: req.Work, Managed: true, Shared: req.Shared, Started: now, Updated: now, Name: name,
		Account: account, For: forID, JobKind: jobKind}, agentHome: agentHome}
	if pipe {
		r.Mode, r.Capabilities = orchapi.SessionModePipe, &caps
	}
	inserted := false
	err = s.withSessionDesignation(func() error {
		if err := insertRow(ctx, reg, r); err != nil {
			return err
		}
		inserted = true
		if err := insertSafeOutputs(ctx, reg, id8, req.SafeOutputs); err != nil {
			return err
		}
		if lineageDepth > 0 {
			if _, err := reg.ExecContext(ctx, `INSERT INTO session_lineage(id,parent,depth) VALUES(?,?,?)`, id8, lineageParent, lineageDepth); err != nil {
				return err
			}
		}
		if task != nil {
			if err := s.insertTask(ctx, r.Session, task); err != nil {
				return err
			}
			s.taskIDs.Store(id8, true)
		}
		if req.ContinueFrom != "" {
			if _, err := reg.ExecContext(ctx, `INSERT OR REPLACE INTO session_pipe_handoffs(session,continue_from) VALUES(?,?)`, id8, src.ID); err != nil {
				return err
			}
		}
		if pin != nil {
			return s.recordRemoteStart(ctx, id8, pin)
		}
		if inherited != nil {
			return s.recordRemoteStart(ctx, id8, inherited)
		}
		return nil
	})
	if err != nil {
		// The designation read barrier is already released: rollback takes
		// its exclusive side to remove any hold created after a failed insert.
		if inserted {
			_ = s.rollbackUnstartedSessionRow(context.Background(), reg, id8)
		} else if nameTaken(err) {
			err = s.nameConflict(ctx, sub, name)
		}
		undo()
		return orchapi.Session{}, err
	}
	s.scopes.Store(addr, object(r.Session))
	if req.Work != "" {
		var err error = api.Errorf(api.CodeUnavailable, "the work module is not available")
		switch {
		case teamLeases != nil:
			_, err = teamLeases.BindAt(ctx, sub, proj.ID, req.Work, addr)
		case orchcore.Work(s.d) != nil:
			_, err = orchcore.Work(s.d).BindLease(ctx, sub, proj.ID, req.Work, addr)
		}
		if err != nil {
			_ = s.rollbackUnstartedSessionRow(context.Background(), reg, id8)
			undo()
			return orchapi.Session{}, err
		}
	}

	cliPrompt := task == nil && !pipe && req.Prompt != "" && codexInitialPrompt(agent, argv)
	launchExe := exe
	if cliPrompt {
		// Only the child argv gets the prompt: never the persisted/public
		// session Command or session-started event. A quiet startup menu is
		// not readiness to receive keystrokes; let Codex submit this itself.
		launchExe = codexPromptCommand(bin, argv, req.Prompt)
		if srt != "" {
			launchExe = sandboxArgv(srt, policy, launchExe)
		}
	}
	args := append([]string(nil), launchExe...)
	if !pipe {
		args[0] = argv[0]
	}
	if srt != "" {
		args[0] = "srt"
	}
	spec := holder.Spec{Session: id8, Home: s.d.Config.Home, Path: exe[0], Args: args, Dir: cwd, Env: childEnv, Rows: rows, Cols: cols, Pipe: pipe, Nice: s.agentNice(agent)}
	if guardClaudeSkills {
		spec.Path, spec.Args, spec.RecordedLaunch = guardedCommand[0], guardedCommand, true
	}
	if task != nil {
		var tr orchapi.TaskStartRequest
		_ = json.Unmarshal([]byte(task.requestJSON), &tr)
		spec.Task = true
		if task.prompt != "" {
			tr.Prompt = task.prompt
		}
		spec.TaskPrompt = taskStdin(task, tr.Prompt)
	}
	p, pid, err := s.startHolder(spec, addr, agent, now)
	if err != nil {
		var neverStarted *holder.NeverStartedError
		neverStartedRecorded := false
		if task != nil && errors.As(err, &neverStarted) {
			if _, e := reg.ExecContext(context.Background(), `UPDATE session_tasks SET child_never_started=1 WHERE id=?`, id8); e != nil {
				s.d.Log.Warn("record task never started", "session", id8, "err", e)
			} else {
				neverStartedRecorded = true
			}
		}
		var refused *holder.StartRefused
		if task != nil && task.source != "" && errors.As(err, &refused) {
			// The holder confirmed exec failed; release the reservation and its
			// generated idempotency key, while retaining the failed task record.
			tx, e := reg.BeginTx(context.Background(), nil)
			if e == nil {
				_, e = tx.Exec(`UPDATE session_handoffs SET continuation='', status='failed', error=? WHERE source=? AND continuation=?`, oneLine(err.Error()), task.source, id8)
				if e == nil {
					_, e = tx.Exec(`UPDATE session_tasks SET request_id='' WHERE id=?`, id8)
				}
				if e == nil {
					e = tx.Commit()
				}
				_ = tx.Rollback()
			}
		}
		s.end(context.Background(), id8, orchapi.Outcome{Kind: orchapi.OutcomeInfra, Confidence: "high",
			Evidence: oneLine(err.Error()), Note: "spawn failed"})
		if teamLeases != nil {
			// The authority keeps an ended session's default-TTL lease: give it back.
			if e := teamLeases.ReleaseAt(context.Background(), proj.ID, req.Work, addr); e != nil {
				s.d.Log.Warn("release the lease of a session that never started", "session", addr, "work", req.Work, "err", e)
			}
		}
		if neverStartedRecorded {
			// No child can write: finish the task before another admission
			// counts its slot. Ambiguous starts still wait for reconciliation.
			s.completeTask(id8)
		}
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "start %s: %v", argv[0], err)
	}
	ss, err := s.update(context.Background(), id8, "", func(r *row) (bool, error) {
		r.PID = pid
		if st, ok := procStat(pid); ok {
			r.pidStart = st.start
		}
		return true, nil
	})
	if err != nil {
		s.d.Log.Warn("session pid", "session", id8, "err", err)
		ss = r.Session
		ss.PID = pid
	}
	if p == nil { // it exited before its holder was adopted
		s.publish(orchapi.EventSessionStarted, ss)
		if r, ok := s.load(context.Background(), id8); ok {
			s.endFromFile(context.Background(), r)
		}
		return ss, nil
	}
	// Started goes out before the holder runs: its first update must not
	// reach a subscriber ahead of the session it updates.
	s.publish(orchapi.EventSessionStarted, ss)
	s.goRun(p)
	if pipe {
		go s.pipeStart(p, orchapi.AgentStart{Cwd: cwd, Model: model, Approvals: req.Approvals, Resume: req.Resume, MCP: req.MCP,
			AllowedTools: req.AllowedTools, PermissionMode: req.PermissionMode}, firstTurn)
	} else if req.Prompt != "" && !cliPrompt {
		go s.typePrompt(p, req.Prompt)
	}
	return ss, nil
}

func codexPromptCommand(bin string, argv []string, prompt string) []string {
	cmd := append([]string{bin}, argv[1:]...)
	if argv[len(argv)-1] != "--" {
		cmd = append(cmd, "--")
	}
	return append(cmd, prompt)
}

// cursorQuery is a terminal's cursor position request (DSR 6); cursorReply
// is the answer the daemon gives when no client is attached.
var cursorQuery, cursorReply = []byte("\x1b[6n"), []byte("\x1b[1;1R")

// feed takes output the holder streamed, starting at offset off, into the
// mirror: what the holder held before this daemon adopted it is history
// (titles and modes are read from it, nothing else).
func (s *Service) feed(p *proc, off int64, data []byte) {
	if p.pipe != nil {
		s.feedPipe(p, off, data)
		return
	}
	if _, ok := s.taskIDs.Load(p.id8); ok {
		s.captureTask(p.id8, off, data)
	}
	p.out.WriteAt(off, data)
	if cut := p.replayEnd - off; cut > 0 {
		old := data[:min(cut, int64(len(data)))]
		p.modes.feed(old)
		if title, ok := p.tp.feed(old); ok {
			s.titleSeen(p, title)
		}
		data = data[len(old):]
	}
	if _, end := p.out.Bounds(); end >= p.replayEnd {
		select {
		case <-p.caught:
		default:
			close(p.caught)
		}
	}
	if len(data) == 0 {
		return
	}
	p.modes.feed(data)
	if title, ok := p.tp.feed(data); ok {
		s.titleSeen(p, title)
	}
	p.mu.Lock()
	p.lastOut = time.Now()
	wake := p.timerIdle && p.state == orchapi.StateIdle
	if wake {
		p.timerIdle = false
	}
	answered := p.hookHuman && p.answered && p.state == orchapi.StateNeedsHuman
	if answered {
		p.hookHuman, p.answered = false, false
	}
	gen := p.stateGen
	unattached := len(p.conns) == 0
	p.mu.Unlock()
	if unattached && bytes.Contains(data, cursorQuery) {
		// Nobody's terminal is attached to answer a cursor position
		// query; answer it like a terminal would, so programs in the
		// session (termenv's colour probe in every TTY `zm` call)
		// do not wait seconds for a reply that never comes.
		go func() { _ = p.write(cursorReply) }()
	}
	if wake {
		go s.wake(p.id8)
	}
	if answered {
		go s.wakeAnswered(p, gen)
	}
}

// wake undoes a timer-set idle when output arrives.
func (s *Service) wake(id8 string) {
	_, _ = s.update(s.ctx, id8, orchapi.EventSessionStateChanged, func(r *row) (bool, error) {
		if r.State != orchapi.StateIdle || r.Note != "no output" {
			return false, nil
		}
		r.State, r.Note = orchapi.StateRunning, ""
		return true, nil
	})
}

// wakeAnswered sets running when a hook-set needs_human (a permission
// prompt) was answered and the agent printed again, unless any state was
// reported since (gen): a mail-set needs_human is never undone here.
func (s *Service) wakeAnswered(p *proc, gen uint64) {
	_, _ = s.update(s.ctx, p.id8, orchapi.EventSessionStateChanged, func(r *row) (bool, error) {
		p.mu.Lock()
		stale := p.stateGen != gen
		p.mu.Unlock()
		if stale || r.State != orchapi.StateNeedsHuman {
			return false, nil
		}
		r.State, r.Note = orchapi.StateRunning, ""
		return true, nil
	})
}

// typePrompt types prompt once the agent has printed something and gone
// quiet for PromptQuiet (or after 10 s regardless).
func (s *Service) typePrompt(p *proc, prompt string) {
	start := time.Now()
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-p.exited:
			return
		case <-s.ctx.Done():
			return
		case <-t.C:
		}
		p.mu.Lock()
		last := p.lastOut
		p.mu.Unlock()
		if last.IsZero() {
			// A fast program can print before adoption. Those bytes are replay
			// history (feed intentionally leaves lastOut alone), but they still
			// establish startup output for this newly spawned prompt.
			if _, end := p.out.Bounds(); end > 0 {
				last = start
			}
		}
		if (!last.IsZero() && time.Since(last) >= s.PromptQuiet) || time.Since(start) >= 10*time.Second {
			_ = p.typeLine(prompt, true)
			return
		}
	}
}

// stopStep is one signal of a stop and how long to wait for the exit.
type stopStep struct {
	sig  syscall.Signal
	name string
	wait time.Duration
}

// Stop escalation (SPEC section 8, what `zm kill` does): TERM gets the time
// an agent needs to flush and exit; HUP is what a closing terminal sends
// (interactive shells ignore TERM but not HUP); KILL only when forced.
var (
	StopTermWait = 3 * time.Second
	StopHupWait  = time.Second
	StopKillWait = 3 * time.Second
)

// stopPlan is the signals req asks for.
func stopPlan(req orchapi.StopRequest) ([]stopStep, error) {
	if req.WaitMS < 0 || req.WaitMS > 10_000 {
		return nil, api.Errorf(api.CodeBadRequest, "wait_ms must be 0..10000")
	}
	if req.Escalate {
		if req.Signal != "" || req.WaitMS != 0 {
			return nil, api.Errorf(api.CodeBadRequest, "escalate sends TERM, then HUP (and KILL with force); it takes no signal or wait_ms")
		}
		plan := []stopStep{{syscall.SIGTERM, "TERM", StopTermWait}, {syscall.SIGHUP, "HUP", StopHupWait}}
		if req.Force {
			plan = append(plan, stopStep{syscall.SIGKILL, "KILL", StopKillWait})
		}
		return plan, nil
	}
	if req.Force {
		return nil, api.Errorf(api.CodeBadRequest, "force goes with escalate (KILL after TERM and HUP); to send only KILL, pass signal KILL")
	}
	sig, name, err := parseSignal(req.Signal)
	if err != nil {
		return nil, err
	}
	wait := 3 * time.Second
	if req.WaitMS > 0 {
		wait = time.Duration(req.WaitMS) * time.Millisecond
	}
	return []stopStep{{sig, name, wait}}, nil
}

// Stop signals a managed session and waits up to req.WaitMS (default 3 s,
// at most 10 s) for it to exit; with req.Escalate it runs the whole
// escalation. sent lists the signals sent, in order.
func (s *Service) Stop(ctx context.Context, sub *authz.Subject, id string, req orchapi.StopRequest) (orchapi.Session, error) {
	ss, _, err := s.stop(ctx, sub, id, req)
	return ss, err
}

func (s *Service) stop(ctx context.Context, sub *authz.Subject, id string, req orchapi.StopRequest) (orchapi.Session, []string, error) {
	plan, err := stopPlan(req)
	if err != nil {
		return orchapi.Session{}, nil, err
	}
	r, err := s.operable(ctx, sub, id)
	if err != nil {
		return orchapi.Session{}, nil, err
	}
	plan = sandboxStopPlan(r.Agent, req, plan) // a task driver releases its box on TERM
	if err := s.taskSessionAccess(ctx, sub, r); err != nil {
		return orchapi.Session{}, nil, err
	}
	p, err := s.liveProc(r)
	if err != nil {
		if r.Managed && r.State != orchapi.StateEnded && s.unreachableWhy(r.ID) != "" {
			defer s.trackParentStop(r, nil)()
			var isTask bool
			_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_tasks WHERE id=?)`, r.ID).Scan(&isTask)
			if isTask {
				return s.stopUnreachableTask(ctx, r, plan)
			}
			signalErr := s.signalUnreachable(ctx, r, plan[len(plan)-1].sig)
			if signalErr != nil {
				return orchapi.Session{}, nil, api.Errorf(api.CodeConflict, "cannot safely signal unreachable session: %s; inspect and stop the process manually", signalErr)
			}
			s.cancelTaskChildren(r.Address)
			deadline := time.Now().Add(plan[len(plan)-1].wait)
			for time.Now().Before(deadline) {
				if _, ok := holder.ReadExit(s.d.Config.Home, r.ID); ok {
					s.endFromFile(ctx, r)
					break
				}
				select {
				case <-ctx.Done():
					break
				case <-time.After(50 * time.Millisecond):
				}
			}
			out, _ := s.load(ctx, r.ID)
			return out.Session, []string{plan[len(plan)-1].name}, nil
		}
		return orchapi.Session{}, nil, err
	}
	if err := s.holderSignalAllowed(ctx, r.ID); err != nil {
		// The socket may have disappeared after adoption. Drop the stale
		// connection and use the separately verified unreachable path.
		if s.lose(p) {
			return s.stop(ctx, sub, id, req)
		}
		return orchapi.Session{}, nil, api.Errorf(api.CodeConflict, "cannot safely signal session: %s; inspect and stop the process manually", err)
	}

	defer s.trackParentStop(r, p)()
	s.cancelTaskChildren(r.Address)
	_, isTask := s.taskIDs.Load(r.ID)
	var sweep *taskSweep
	var unlock func()
	if isTask {
		unlock = s.lockTaskStop(r.ID)
		p.mu.Lock()
		swept := p.taskSweepFinal
		p.mu.Unlock()
		gone, _ := processGone(r.PID, r.pidStart, r.Started)
		if swept && gone {
			unlock()
			select {
			case <-p.exited:
			case <-time.After(time.Second):
			case <-ctx.Done():
			}
			out, _ := s.load(ctx, r.ID)
			return out.Session, nil, nil
		}
		sweep = s.newTaskSweep(ctx, r)
	}
	var sent []string
	for _, st := range plan {
		if !isTask && p.isExited() {
			break
		}
		deadline := time.Now().Add(st.wait)
		if isTask {
			sweep.startStep(deadline)
		}
		if isTask {
			if st.sig == syscall.SIGKILL {
				sweep.freezeForKill(ctx, taskFreezeDeadline(deadline))
			} else {
				sweep.refresh(ctx)
			}
		}
		p.mu.Lock()
		p.stopped, p.stopSig, p.stopBy = true, st.name, sub.Address
		p.mu.Unlock()
		if isTask {
			sweep.signal(st.sig)
		}
		if !p.isExited() {
			_ = p.signal(st.sig)
			sent = append(sent, st.name)
		}
		if isTask && st.sig == syscall.SIGKILL {
			sweep.resumeFrozen()
		}
		for time.Now().Before(deadline) {
			if isTask {
				sweep.refresh(ctx)
				sweep.signal(st.sig)
				if taskExitReported(p) && len(sweep.survivors()) == 0 {
					break
				}
				if p.isExited() && len(sweep.survivors()) == 0 {
					break
				}
			} else if p.isExited() {
				break
			}
			if ctx.Err() != nil || contextStopped(s.ctx) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if isTask {
			if taskExitReported(p) && len(sweep.survivors()) == 0 && sweep.err == nil {
				break
			}
		} else if p.isExited() {
			break
		}
	}
	if isTask {
		if sweep.bound.IsZero() || time.Now().Before(sweep.bound) {
			sweep.refresh(ctx)
		}
		// A soft signal may intentionally leave the whole task running. Do
		// not classify that as a failed stop until the child actually exits.
		select {
		case <-p.rawExited:
			s.recordTaskSweepWarning(ctx, r.ID, sweep.warning())
		default:
			gone, _ := processGone(r.PID, r.pidStart, r.Started)
			if gone || len(plan) > 1 || plan[0].sig == syscall.SIGKILL {
				s.recordTaskSweepWarning(ctx, r.ID, sweep.warning())
			}
		}
		gone, _ := processGone(r.PID, r.pidStart, r.Started)
		if gone || len(plan) > 1 || plan[0].sig == syscall.SIGKILL {
			p.mu.Lock()
			p.taskSweepFinal = true
			p.mu.Unlock()
		}
		unlock()
		select {
		case <-p.rawExited:
			select {
			case <-p.exited:
			case <-time.After(time.Second):
			case <-ctx.Done():
			}
		default:
		}
	}

	out, _ := s.load(ctx, r.ID)
	return out.Session, sent, nil
}

// liveProc returns a live managed session's process or a conflict saying why not.
func (s *Service) liveProc(r row) (*proc, error) {
	if p := s.proc(r.ID); p != nil {
		return p, nil
	}
	if why := s.unreachableWhy(r.ID); why != "" && r.State != orchapi.StateEnded {
		return nil, api.Errorf(api.CodeConflict, "session %s is running, but %s", r.Address, why).
			WithHint("zm status lists the sessions this daemon cannot reach")
	}
	if !r.Managed {
		return nil, api.Errorf(api.CodeConflict, "session %s runs in its own terminal (not started by zm)", r.Address)
	}
	return nil, api.Errorf(api.CodeConflict, "session %s has ended", r.Address).
		WithHint("see its last output with: zm session output " + r.ID)
}

// Input types into a managed session.
func (s *Service) Input(ctx context.Context, sub *authz.Subject, id string, req orchapi.InputRequest) error {
	r, err := s.operable(ctx, sub, id)
	if err != nil {
		return err
	}
	if err := s.taskSessionAccess(ctx, sub, r); err != nil {
		return err
	}
	if _, ok := s.taskIDs.Load(r.ID); ok {
		return api.Errorf(api.CodeConflict, "task input is disabled")
	}
	if r.Mode == orchapi.SessionModePipe {
		return refusePipe(r.Address, "terminal input")
	}
	p, err := s.liveProc(r)
	if err != nil {
		return err
	}
	return p.typeInput(req.Data, req.Enter)
}

// Resize sets a managed session's terminal size.
func (s *Service) Resize(ctx context.Context, sub *authz.Subject, id string, req orchapi.ResizeRequest) error {
	if req.Rows < 1 || req.Rows > 1000 || req.Cols < 1 || req.Cols > 1000 {
		return api.Errorf(api.CodeBadRequest, "rows and cols must be 1..1000")
	}
	r, err := s.operable(ctx, sub, id)
	if err != nil {
		return err
	}
	if _, ok := s.taskIDs.Load(r.ID); ok {
		return api.Errorf(api.CodeConflict, "task sessions do not have a terminal")
	}
	if r.Mode == orchapi.SessionModePipe {
		return api.Errorf(api.CodeConflict, "session %s is a structured session: it has no terminal", r.Address)
	}
	p, err := s.liveProc(r)
	if err != nil {
		return err
	}
	return pty.Setsize(p.pty, &pty.Winsize{Rows: uint16(req.Rows), Cols: uint16(req.Cols)})
}

// Output returns scrollback from offset since: live from memory, ended from disk.
func (s *Service) Output(ctx context.Context, sub *authz.Subject, id string, since int64) (orchapi.Output, error) {
	r, err := s.operable(ctx, sub, id)
	if err != nil {
		return orchapi.Output{}, err
	}
	if err := s.taskSessionAccess(ctx, sub, r); err != nil {
		return orchapi.Output{}, err
	}
	if p := s.proc(r.ID); p != nil {
		p.waitCaught()
	}
	var out orchapi.Output
	err = s.withOutputRow(ctx, r, since, func(v orchapi.Output) error { out = v; return nil })
	return out, err
}

// withOutputRow serializes both the read and its consumer against redaction.
// A consumer must not call another output gate while this callback runs.
func (s *Service) withOutputRow(ctx context.Context, r row, since int64, consume func(orchapi.Output) error) error {
	s.taskIO.Lock()
	defer s.taskIO.Unlock()
	var withheld bool
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT text_withheld FROM session_tasks WHERE id=?`, r.ID).Scan(&withheld)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if withheld {
		return api.Errorf(api.CodeConflict, "task output withheld pending redaction")
	}
	return s.withSessionOutput(ctx, r.ID, func() error {
		out, err := s.outputRow(r, since)
		if err != nil {
			return err
		}
		return consume(out)
	})
}

// outputRow reads an already-authorized session; callers serialize it against
// redaction with taskIO.
func (s *Service) outputRow(r row, since int64) (orchapi.Output, error) {
	if p := s.proc(r.ID); p != nil {
		data, from, to, _, _ := p.out.Next(since, scrollbackSize)
		return output(data, from, to), nil
	}
	if !r.Managed {
		return orchapi.Output{}, api.Errorf(api.CodeConflict, "session %s runs in its own terminal; zm has no output for it", r.Address)
	}
	b, err := os.ReadFile(s.scrollbackPath(r.ID))
	if err != nil {
		return orchapi.Output{Offset: r.outBytes, Next: r.outBytes}, nil
	}
	base := r.outBytes - int64(len(b))
	from := min(max(since, base), r.outBytes)
	return output(b[from-base:], from, r.outBytes), nil
}

// output trims a trailing partial UTF-8 character so it arrives whole next time.
func output(data []byte, from, to int64) orchapi.Output {
	for k := 1; k <= 3 && k <= len(data); k++ {
		c := data[len(data)-k]
		if c < 0x80 {
			break
		}
		if c >= 0xC0 { // a lead byte: is its sequence complete?
			need := 2
			if c >= 0xF0 {
				need = 4
			} else if c >= 0xE0 {
				need = 3
			}
			if k < need {
				data, to = data[:len(data)-k], to-int64(k)
			}
			break
		}
	}
	return orchapi.Output{Data: string(data), Offset: from, Next: to}
}
