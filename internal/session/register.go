package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

// Register records a session zm did not start (an agent's hook calls it),
// keyed by the agent's own session id, in the project its cwd resolves to
// (else "none": registration never fails for lack of a project). It is
// idempotent per (agent, runtime_id); created reports a new row. An ended
// unmanaged session registering again (a resumed conversation) runs again.
// A conversation that ran under `zm run` and is resumed elsewhere (its
// managed session has ended) becomes a new unmanaged session: the runtime id
// moves to it, since a managed session's end is final. With req.Session
// naming a live managed session (the hook runs inside a zm-spawned agent),
// the runtime id and state go to that session instead, taken over from an
// ended session that had it.
func (s *Service) Register(ctx context.Context, req orchapi.RegisterRequest) (orchapi.Session, bool, error) {
	return s.register(ctx, req, hookExtra{})
}

// register is Register plus what a hook brings (hookExtra: its activity,
// applied with its state, a subagent, why it waits). It notes when the
// session was last heard from (quietHooks).
func (s *Service) register(ctx context.Context, req orchapi.RegisterRequest, hk hookExtra) (orchapi.Session, bool, error) {
	ss, created, err := s.registerHook(ctx, req, hk)
	if err == nil && ss.ID != "" && !ss.Managed {
		s.hooked.Store(ss.ID, time.Now())
	}
	return ss, created, err
}

func (s *Service) registerHook(ctx context.Context, req orchapi.RegisterRequest, hk hookExtra) (orchapi.Session, bool, error) {
	req.Title, req.Note = textclean.Clean(req.Title), textclean.Clean(req.Note)
	req.Version, req.Model, hk.activity = textclean.Clean(req.Version), textclean.Clean(req.Model), textclean.Clean(hk.activity)
	sub := core.SubjectOf(ctx)
	agent := strings.ToLower(strings.TrimSpace(req.Agent))
	rid := strings.TrimSpace(req.RuntimeID)
	if !api.ValidName(agent) {
		return orchapi.Session{}, false, api.Errorf(api.CodeBadRequest, "bad agent name %q", req.Agent)
	}
	if rid == "" || len(rid) > 200 || strings.ContainsAny(rid, " \t\r\n") {
		return orchapi.Session{}, false, api.Errorf(api.CodeBadRequest, "runtime_id must be the agent's own session id (no spaces)")
	}
	switch req.State {
	case "", orchapi.StateRunning, orchapi.StateIdle, orchapi.StateNeedsHuman:
	default:
		return orchapi.Session{}, false, api.Errorf(api.CodeBadRequest, "state must be running, idle or needs_human, not %q", req.State)
	}
	// Every write to an existing session (a state, title or end, a bind,
	// a revive, a pid move) is also checked against the caller found by its
	// process (capGate), so a capped caller can neither take over an
	// uncapped session and be found as it, nor end or retitle one by naming
	// its runtime id.
	gate := s.capGate(sub)
	may := func(r row) bool {
		return sub.Can(authz.Write, object(r.Session)) && gate.allows(ctx, r, authz.Write, false)
	}
	reg := s.d.Store.Registry()
	if req.Session != "" {
		if r, ok := s.load(ctx, req.Session); ok && r.Managed && r.State != orchapi.StateEnded && may(r) {
			// ZM_SESSION can be inherited from another thread. Its managed
			// override is valid only before first binding or for this runtime.
			useManaged := r.Agent == agent && (r.RuntimeID == "" || r.RuntimeID == rid)
			if r.RuntimeID == "" && r.Agent == agent {
				old, found, err := byRuntime(ctx, reg, agent, rid)
				if err != nil {
					return orchapi.Session{}, false, err
				}
				if found && old.State == orchapi.StateEnded && may(old) {
					if err := s.releaseRuntimeID(ctx, old.ID); err != nil {
						return orchapi.Session{}, false, err
					}
					found = false
				}
				useManaged = !found // a live runtime already belongs to its own row
				if !found {
					bound, err := s.update(ctx, r.ID, "", func(x *row) (bool, error) {
						if x.State == orchapi.StateEnded || x.RuntimeID != "" {
							return false, nil
						}
						x.RuntimeID = rid
						return true, nil
					})
					if err != nil {
						return orchapi.Session{}, false, err
					}
					useManaged = bound.State != orchapi.StateEnded && bound.RuntimeID == rid
				}
			}
			if useManaged {
				if !hk.subagentEvent() {
					s.follow(ctx, r, req.Cwd)
				}
				ss, err := s.applyHook(ctx, r.ID, req, hk, nil)
				return ss, false, err
			}
			if bindable(r, agent) {
				// An agent started at a managed shell's prompt: it shows on
				// the shell's row (runtime_agent), not as a second session.
				if ss, ok, err := s.bindShell(ctx, r, req, agent, rid, hk); err != nil || ok {
					return ss, false, err
				}
			}
		}
	}
	// A hook of an agent bound to a shell that does not name it (a
	// completion, whose ZM_SESSION may be inherited, goes by the runtime id).
	if b, ok, err := boundTo(ctx, reg, agent, rid); err != nil {
		return orchapi.Session{}, false, err
	} else if ok && may(b) {
		ss, err := s.applyBound(ctx, b, req, hk)
		return ss, false, err
	}
	if old, ok, err := byRuntime(ctx, reg, agent, rid); err != nil || ok {
		if err != nil {
			return orchapi.Session{}, false, err
		}
		if !may(old) {
			return orchapi.Session{}, false, api.NotFound("session", rid)
		}
		if old.Managed && old.State == orchapi.StateEnded {
			if req.End {
				return old.Session, false, nil
			}
			if err := s.releaseRuntimeID(ctx, old.ID); err != nil {
				return orchapi.Session{}, false, err
			}
			return s.registerNew(ctx, req, agent, rid, hk)
		}
		dropName := false // revived, and another live session here has its name since
		// An ended session revives on a hook from a live agent. A hook that
		// names none (orphaned, an older client, an agent AgentPID cannot
		// tell) revives it while the agent zm recorded still runs, when zm
		// recorded none, or when it reports work (running, needs_human):
		// then the dead pid is dropped and the quiet expiry watches it. A late
		// Codex notify (idle) after its agent exited revives nothing.
		liveAgent := req.PID > 1 && agentLive(req.PID)
		working := req.State == orchapi.StateRunning || req.State == orchapi.StateNeedsHuman
		revive := func(r *row) (bool, error) {
			changed := false
			recorded := recordedLive(*r)
			reviving := r.State == orchapi.StateEnded && !r.Managed && !req.End && (liveAgent || req.PID <= 1 && (recorded || working))
			if reviving && !liveAgent && !recorded {
				r.PID, r.pidStart = 0, 0
			}
			if reviving {
				r.State, r.Note, r.Ended, r.Outcome, changed = orchapi.StateRunning, "", nil, nil, true
				if dropName {
					r.Name = ""
				}
			}
			set := func(dst *string, v string) {
				if v = oneLine(v); v != "" && v != *dst {
					*dst, changed = v, true
				}
			}
			if !hk.subagentEvent() { // a subagent's hook never moves its parent's folder or title
				set(&r.Cwd, req.Cwd)
				set(&r.Title, req.Title)
			}
			set(&r.Version, req.Version)
			set(&r.Model, req.Model)
			if r.State != orchapi.StateEnded && takePID(r, req.PID, reviving, time.Now()) {
				changed = true
			}
			return changed, nil
		}
		_, err = s.update(ctx, old.ID, orchapi.EventSessionRegistered, revive)
		if nameTaken(err) {
			dropName = true
			_, err = s.update(ctx, old.ID, orchapi.EventSessionRegistered, revive)
		}
		if err != nil {
			return orchapi.Session{}, false, err
		}
		if !req.End && !hk.subagentEvent() {
			s.follow(ctx, old, req.Cwd)
		}
		ss, err := s.applyHook(ctx, old.ID, req, hk, nil)
		return ss, false, err
	}
	return s.registerNew(ctx, req, agent, rid, hk)
}

// releaseRuntimeID takes the runtime id off an ended session so a new
// session can carry it (it is unique per agent).
func (s *Service) releaseRuntimeID(ctx context.Context, id8 string) error {
	_, err := s.update(ctx, id8, "", func(x *row) (bool, error) {
		if x.State != orchapi.StateEnded || x.RuntimeID == "" {
			return false, nil
		}
		x.RuntimeID = ""
		return true, nil
	})
	return err
}

// registerNew inserts a new unmanaged session for (agent, rid).
func (s *Service) registerNew(ctx context.Context, req orchapi.RegisterRequest, agent, rid string, hk hookExtra) (orchapi.Session, bool, error) {
	sub := core.SubjectOf(ctx)
	// The caller found by its process (or Zm-As), within its chain's caps:
	// a session registered from inside a capped one takes its cap and its
	// project, so registering is no way out of a cap (SPEC 1.3).
	caller, origin, err := s.processCaller(ctx, sub)
	if err != nil {
		return orchapi.Session{}, false, err
	}
	inherited := s.inheritedCap(ctx, caller)
	if inherited != nil {
		in := core.ResolveInputOf(ctx)
		in.Explicit, in.ExplicitFrom = inherited.project, project.RuleFlag
		c := *sub
		c.Scope = caller.Scope
		ctx = core.WithSubject(core.WithResolveInput(ctx, in), &c)
	}
	reg := s.d.Store.Registry()
	proj, area, err := s.target(ctx, req.Cwd, "", authz.Write, true)
	if err != nil {
		return orchapi.Session{}, false, err
	}
	now := storedTime(time.Now())
	state := req.State
	if state == "" {
		state = orchapi.StateRunning
	}
	// A hook must never fail on attribution: a kind it does not know is dropped.
	forID, jobKind, err := attribution(req.For, req.JobKind)
	if err != nil {
		forID, jobKind = cleanFor(req.For), ""
	}
	home, account := "", ""
	if agentHomeEnv(agent) != "" && filepath.IsAbs(req.AgentHome) {
		home = filepath.Clean(req.AgentHome)
	}
	if agent == "claude" {
		account = s.claudeAccount(home)
	}
	r := row{Session: orchapi.Session{Machine: s.d.Config.Machine, Project: proj.ID, Area: area, Launcher: sub.Identity.ID,
		Agent: agent, Command: req.Command, Version: oneLine(req.Version), Model: oneLine(req.Model), Cwd: req.Cwd,
		Title: oneLine(req.Title), State: state, Note: oneLine(req.Note), RuntimeID: rid, PID: req.PID,
		Started: now, Updated: now, Account: account, For: forID, JobKind: jobKind}, agentHome: home}
	r.PID = 0
	takePID(&r, req.PID, true, now)
	if hk.activity != "" && !req.End && !hk.subagentEvent() {
		r.Activity, r.ActivityAt = hk.activity, &now
	}
	if state == orchapi.StateNeedsHuman {
		r.Needs = &orchapi.SessionNeeds{Kind: hookNeeds(hk.needs), At: now}
	}
	parentID := ""
	if origin != "" {
		if p, ok := s.Lookup(ctx, origin); ok {
			parentID = p.ID
		}
	}
	for try := 0; ; try++ {
		r.ID = api.Session8(rid)
		if try > 0 {
			r.ID = api.NewSession8()
		}
		r.Address = api.SessionAddress(r.Machine, r.ID)
		if err = s.insertRegistered(ctx, r, inherited, origin, parentID); err == nil {
			if registeredSeam != nil {
				registeredSeam(r.Address)
			}
			break
		}
		if !store.IsUnique(err) || try > 5 {
			return orchapi.Session{}, false, err
		}
		if old, ok, _ := byRuntime(ctx, reg, agent, rid); ok { // lost a race with the same hook
			return old.Session, false, nil
		}
	}
	s.scopes.Store(r.Address, object(r.Session))
	s.publish(orchapi.EventSessionRegistered, r.Session)
	if r.Activity != "" {
		s.logActivity(r.ID, "", r.Activity, now)
	}
	if req.End {
		ss, err := s.applyHook(ctx, r.ID, orchapi.RegisterRequest{End: true, Note: req.Note}, hookExtra{}, nil)
		return ss, true, err
	}
	if hk.sub != nil { // the subagent part (its parent's state went in with the row)
		ss, err := s.applyHook(ctx, r.ID, orchapi.RegisterRequest{}, hookExtra{sub: hk.sub, activity: hk.activity}, nil)
		return ss, true, err
	}
	return r.Session, true, nil
}

// registeredSeam, when set (tests), runs as soon as a registered session's
// row is visible to other requests.
var registeredSeam func(address string)

// insertRegistered writes a registered session's row, its inherited cap and
// its lineage in one transaction, the cap known in memory (SessionCap)
// before it commits: its pid is never found as an uncapped session, nor
// outside its parent's chain. Depth stays the parent's
// (0 when it has no row, a top-level session): self-registration never
// deepens a task chain.
func (s *Service) insertRegistered(ctx context.Context, r row, pin *remotePin, origin, parentID string) error {
	caps, _ := s.capsLoaded() // loads from the registry: before the tx holds it
	tx, err := s.d.Store.Registry().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertRow(ctx, tx, r); err != nil {
		return err
	}
	if pin != nil {
		if err := insertCap(ctx, tx, r.ID, pin); err != nil {
			return err
		}
	}
	if parentID != "" && parentID != r.ID {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO session_lineage(id,parent,depth) VALUES(?,?,COALESCE((SELECT depth FROM session_lineage WHERE id=?),0))`, r.ID, origin, parentID); err != nil {
			return err
		}
	}
	if pin == nil {
		return tx.Commit()
	}
	caps.Store(r.Address, sessionCap{pin.project, pin.role}) // kept if the commit fails: a cap on nothing fails closed
	return tx.Commit()
}

// applyHook applies a register call's state, note and end, and a hook's
// activity, subagent and wait, to a session; mut (may be nil) is a further
// change in the same write (the bound agent's runtime_agent).
func (s *Service) applyHook(ctx context.Context, id8 string, req orchapi.RegisterRequest, hk hookExtra, mut func(*row, time.Time) int) (orchapi.Session, error) {
	r, err := loadRow(ctx, s.d.Store.Registry(), id8)
	if err != nil {
		return orchapi.Session{}, err
	}
	if req.End && !r.Managed {
		o := orchapi.Outcome{Kind: orchapi.OutcomeOK, Confidence: "high", Evidence: "session-end hook", Note: oneLine(req.Note)}
		ss, _ := s.end(ctx, id8, o)
		return ss, nil
	}
	if r.State == orchapi.StateEnded {
		return r.Session, nil
	}
	activity, state, line := hk.activity, req.State, ""
	if sa := hk.sub; sa != nil {
		sm := subagentMut(*sa, hk.activity, &line)
		if hk.subagentEvent() {
			// A subagent's start, stop or step: its own activity, and no
			// state for the parent (a parallel subagent's step must not
			// clear the parent's permission prompt).
			activity, state = "", ""
		}
		if mut == nil {
			mut = sm
		} else {
			bm := mut
			mut = func(x *row, now time.Time) int { return max(bm(x, now), sm(x, now)) }
		}
	}
	// The subagent that stepped last: a permission prompt that follows is
	// most likely its own (the Notification hook names no subagent), so its
	// next step or its stop ends the wait. The parent's own work forgets it.
	switch {
	case hk.step():
		s.lastStep.Store(id8, hk.sub.ID)
	case !hk.subagentEvent() && (state == orchapi.StateRunning || state == orchapi.StateIdle):
		s.lastStep.Delete(id8)
	}
	if state == "" && activity == "" && mut == nil {
		return r.Session, nil
	}
	var ss orchapi.Session
	if state == "" {
		ss, err = s.setActivity(ctx, id8, activity, mut)
	} else {
		var needs orchapi.SessionNeeds
		if state == orchapi.StateNeedsHuman {
			needs.Kind = hookNeeds(hk.needs)
			if v, ok := s.lastStep.Load(id8); ok {
				needs.Subagent = v.(string)
			}
		}
		ss, err = s.setState(ctx, id8, state, req.Note, true, activity, needs, mut)
	}
	if err == nil && line != "" {
		s.logActivity(id8, hk.sub.ID, line, time.Now())
	}
	return ss, err
}

// hookNeeds is why a hook's needs_human waits: what it said, else a
// permission prompt (the only needs_human an older zm hook reports).
func hookNeeds(n string) string {
	if n == "" {
		return orchapi.NeedsPermission
	}
	return n
}

// ---- hooks ----

// HookEvents are what `zm hook <event>` understands (agent hook names in parentheses).
var HookEvents = []string{
	"session-start (Claude SessionStart)", "prompt (UserPromptSubmit)", "tool (PreToolUse/PostToolUse)",
	"notification (Notification)", "stop (Stop)", "session-end (SessionEnd)", "codex-notify (Codex notify)",
}

// hookPayload is the union of what Claude Code (stdin JSON), Gemini CLI and
// Codex (`notify` argv JSON) send.
type hookPayload struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"` // Codex: a subagent thread's own id (session_id is its root's)
	Cwd       string `json:"cwd"`
	Event     string `json:"hook_event_name"`
	Message   string `json:"message"`
	Reason    string `json:"reason"`
	Model     string `json:"model"`
	// Codex notify.
	Type     string `json:"type"`
	ThreadID string `json:"thread-id"`
}

// ParseHook turns an agent hook invocation into one register call (with the
// state it implies). event is the `zm hook` argument; input is the hook's
// stdin (Claude Code, Gemini CLI) or its last argv element (Codex notify);
// agent may be "" (guessed from the event). ok=false means there is nothing
// to report (no runtime session id anywhere); the hook then does nothing.
func ParseHook(event, agent string, input []byte, getenv func(string) string) (orchapi.RegisterRequest, bool) {
	var p hookPayload
	_ = json.Unmarshal(input, &p)
	ev := strings.ToLower(strings.TrimSpace(event))
	if ev == "" {
		ev = strings.ToLower(p.Event)
	}
	req := orchapi.RegisterRequest{Agent: strings.ToLower(strings.TrimSpace(agent)), Cwd: p.Cwd, Model: p.Model,
		Session: getenv("ZM_SESSION")}
	switch ev {
	case "codex-notify", "notify", "agent-turn-complete":
		if req.Agent == "" {
			req.Agent = "codex"
		}
		req.RuntimeID = firstOf(p.ThreadID, p.SessionID, getenv("CODEX_THREAD_ID"))
		req.State = orchapi.StateIdle
		if p.Type != "" && p.Type != "agent-turn-complete" {
			req.State = ""
		}
	default:
		if req.Agent == "" {
			req.Agent = "claude"
		}
		req.RuntimeID = firstOf(p.SessionID, getenv("CLAUDE_CODE_SESSION_ID"))
		if req.Agent == "codex" { // Codex's hooks.json hooks: the thread (a subagent's agent_id), as its notify names it
			req.RuntimeID = firstOf(p.AgentID, p.SessionID, p.ThreadID, getenv("CODEX_THREAD_ID"))
		}
		switch strings.ReplaceAll(ev, "_", "-") {
		case "session-start", "sessionstart":
			req.State = orchapi.StateIdle
		case "prompt", "userpromptsubmit", "user-prompt-submit", "tool", "pretooluse", "posttooluse":
			req.State = orchapi.StateRunning
		case "stop":
			req.State = orchapi.StateIdle
		case "notification":
			msg := oneLine(p.Message)
			low := strings.ToLower(msg)
			switch {
			case strings.Contains(low, "waiting for your input"):
				req.State = orchapi.StateIdle
			default:
				req.State, req.Note = orchapi.StateNeedsHuman, msg
			}
		case "session-end", "sessionend":
			req.End, req.Note = true, oneLine(p.Reason)
		}
	}
	if req.Cwd == "" {
		req.Cwd = getenv("PWD")
	}
	return req, req.RuntimeID != "" && api.ValidName(req.Agent)
}

func firstOf(xs ...string) string {
	for _, x := range xs {
		if x = strings.TrimSpace(x); x != "" {
			return x
		}
	}
	return ""
}
