package session

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// Routes registers /v0/sessions/*. {id} is a session8, a URL-escaped full
// address, or a live session's name (<name> or <machine>/<name>, this machine).
func (s *Service) Routes(r *core.Router) {
	r = r.For("session")
	r.HandleFunc("GET /v0/tasks/agents", core.Local, "local task agent availability", s.hTaskAgents)
	r.HandleFunc("POST /v0/tasks", core.Local, "local parent session; admission and task limits; where cloud: the project's [cloud] opt-in", s.hTaskStart)
	r.HandleFunc("GET /v0/tasks/cloud", core.Local, "read on the resolved project", s.hTaskCloudGet)
	r.HandleFunc("PUT /v0/tasks/cloud", core.Local, "admin on the resolved project, which is this machine's", s.hTaskCloudPut)
	r.HandleFunc("GET /v0/tasks/{id}/read", core.Local, "local task chain history", s.hTaskRead)
	r.HandleFunc("GET /v0/tasks/{id}", core.Local, "operate on task session", s.hTaskStatus)
	r.HandleFunc("POST /v0/tasks/{id}/cancel", core.Local, "operate on task session", s.hTaskCancel)
	r.HandleFunc("POST /v0/sessions", core.Local, "operate on the resolved project (none allowed); admission control", s.hSpawn)
	r.HandleFunc("GET /v0/sessions", core.Authed, "read: ReadScope(?project=), Scope.SQL on (project, area); ?before= pages", s.hList)
	r.HandleFunc("POST /v0/sessions/register", core.Local, "write on the cwd-resolved project, else project none; never fails for lack of a project", s.hRegister)
	r.HandleFunc("POST /v0/sessions/hook", core.Local, "register (as /register) and report state in one call; the session's own notice", s.hHook)
	r.HandleFunc("GET /v0/sessions/admission", core.Local, "local: the current probe and limits", s.hAdmission)
	r.HandleFunc("GET /v0/sessions/gc", core.Local, "local: ended sessions' worktrees with reasons; deletes nothing", s.hGC)
	r.HandleFunc("DELETE /v0/tasks/{id}/transcript", core.Local, "local: remove an ended task's transcript", s.hTaskTranscriptDelete)
	r.HandleFunc("GET /v0/sessions/{id}", core.Authed, "read on the session's project/area, else not_found", s.hGet)
	r.HandleFunc("POST /v0/sessions/{id}/state", core.Local, "the session itself, or operate", s.hState)
	r.HandleFunc("POST /v0/sessions/{id}/name", core.Local, "the session itself, or operate; 409 when another live session here has the name", s.hName)
	r.HandleFunc("POST /v0/sessions/{id}/project", core.Local, "operate on the session, write on the target project/area; 409 while it holds a lease", s.hMove)
	r.HandleFunc("POST /v0/sessions/{id}/input", core.Local, "operate on the session", s.hInput)
	r.HandleFunc("POST /v0/sessions/{id}/prompt", core.Authed, "operate on the session's project/area; shared, managed, live (SPEC 17.10)", s.hPrompt)
	r.HandleFunc("GET /v0/sessions/{id}/prompts", core.Authed, "read on the session's project/area (SPEC 17.10)", s.hPrompts)
	r.HandleFunc("GET /v0/sessions/{id}/activity", core.Authed, "read on the session's project/area, else not_found", s.hActivity)
	r.HandleFunc("GET /v0/sessions/{id}/subagents", core.Authed, "read on the session's project/area, else not_found", s.hSubagents)
	r.HandleFunc("POST /v0/sessions/{id}/resize", core.Local, "operate on the session", s.hResize)
	r.HandleFunc("POST /v0/sessions/{id}/stop", core.Authed, "operate on the session; escalate: TERM, HUP (+KILL with force); from anyone but this machine's socket (or for another machine's session): only a structured session started from another machine (SPEC 8), operate on its project/area, no agent session", s.hStop)
	r.HandleFunc("GET /v0/sessions/{id}/output", core.Local, "operate on the session", s.hOutput)
	r.HandleFunc("GET /v0/sessions/{id}/attach", core.Local, "operate on the session (HTTP Upgrade zm-pty; ?input=0 read-only)", s.hAttach)
	s.pipeRoutes(r)
}

var stateRank = map[orchapi.SessionState]int{orchapi.StateNeedsHuman: 0, orchapi.StateRunning: 1, orchapi.StateIdle: 2, orchapi.StateEnded: 3}

// List returns the sessions sc may read: needs_human first, then running,
// idle, ended; most recently updated first within a state.
func (s *Service) List(ctx context.Context, sc authz.Scope, q orchapi.SessionQuery) ([]orchapi.Session, error) {
	out, _, err := s.listPage(ctx, sc, q)
	return out, err
}

// ListOwnLocal returns only the person's sessions on this machine, including
// joined projects whose grants are held by an authority. It never reads presence.
func (s *Service) ListOwnLocal(ctx context.Context, identity string) ([]orchapi.Session, error) {
	rows, err := listRows(ctx, s.d.Store.Registry(), "WHERE machine = ? AND launcher = ? ORDER BY updated DESC LIMIT 500", s.d.Config.Machine, identity)
	if err != nil {
		return nil, err
	}
	out := make([]orchapi.Session, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.Session)
	}
	return out, nil
}

// maxList is the most sessions one page of GET /v0/sessions returns.
const maxList = 500

// listPage is List with paging: the page is the q.Limit most recently
// updated rows after cursor q.Before (ordered by updated, then id, both
// descending, so ties never drop a row), sorted by state; next is the
// cursor after its last row when more rows follow.
func (s *Service) listPage(ctx context.Context, sc authz.Scope, q orchapi.SessionQuery) ([]orchapi.Session, string, error) {
	limit := q.Limit
	switch {
	case limit == 0:
		limit = maxList
	case limit < 0 || limit > maxList:
		return nil, "", api.Errorf(api.CodeBadRequest, "limit must be 1..%d", maxList)
	}
	where, a := sc.SQL("project", "area", authz.Read)
	conds := []string{where}
	if q.State != "" {
		if _, ok := stateRank[q.State]; !ok {
			return nil, "", api.Errorf(api.CodeBadRequest, "unknown state %q (running, needs_human, idle, ended)", q.State)
		}
		conds, a = append(conds, "state = ?"), append(a, string(q.State))
	} else if !q.All {
		conds, a = append(conds, "state != ?"), append(a, string(orchapi.StateEnded))
	}
	if q.Agent != "" {
		conds, a = append(conds, "agent = ?"), append(a, strings.ToLower(q.Agent))
	}
	if q.Before != "" {
		ms, id, ok := parseCursor(q.Before)
		if !ok {
			return nil, "", api.Errorf(api.CodeBadRequest, "bad page cursor %q (pass the previous page's next)", q.Before)
		}
		conds, a = append(conds, "(updated < ? OR (updated = ? AND id < ?))"), append(a, ms, ms, id)
	}
	rows, err := listRows(ctx, s.d.Store.Registry(), "WHERE "+strings.Join(conds, " AND ")+" ORDER BY updated DESC, id DESC LIMIT ?",
		append(a, limit+1)...)
	if err != nil {
		return nil, "", err
	}
	// One row past the page says whether there is a next page, so a result
	// of exactly N pages costs no extra empty request.
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	out := make([]orchapi.Session, len(rows))
	for i, r := range rows {
		out[i] = r.Session
	}
	next := ""
	if more {
		last := rows[len(rows)-1]
		next = strconv.FormatInt(store.Millis(last.Updated), 10) + "-" + last.ID
	}
	// On a team authority, the replicas' sessions (presence) too: on the
	// first page only (presence is small and not paged by the cursor).
	if q.Before == "" {
		pres, err := s.listPresence(ctx, sc, q)
		if err != nil {
			return nil, "", err
		}
		out = append(out, pres...)
		sort.SliceStable(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	}
	sort.SliceStable(out, func(i, j int) bool { return stateRank[out[i].State] < stateRank[out[j].State] })
	s.dressAll(ctx, out)
	return out, next, nil
}

// parseCursor splits a page cursor "<updated ms>-<session8>".
func parseCursor(c string) (int64, string, bool) {
	msPart, id, ok := strings.Cut(c, "-")
	ms, err := strconv.ParseInt(msPart, 10, 64)
	if !ok || err != nil || ms < 0 || id == "" || len(id) > 64 {
		return 0, "", false
	}
	return ms, id, true
}

func (s *Service) hSpawn(w http.ResponseWriter, r *http.Request) {
	var req orchapi.SpawnRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	ss, err := s.Spawn(r.Context(), req)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	s.writeSessionResponse(w, r, http.StatusCreated, ss)
}

func (s *Service) hList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sc, err := s.d.ReadScope(r.Context(), q.Get("project"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	all := q.Get("all")
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			api.WriteError(w, api.Errorf(api.CodeBadRequest, "limit must be 1..%d", maxList))
			return
		}
		limit = n
	}
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	defer controller.SetWriteDeadline(time.Time{})
	s.outputIO.RLock()
	defer s.outputIO.RUnlock()
	ss, next, err := s.listPage(r.Context(), sc, orchapi.SessionQuery{State: orchapi.SessionState(q.Get("state")), Agent: q.Get("agent"),
		All: all == "1" || all == "true", Before: q.Get("before"), Limit: limit})
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if ss == nil {
		ss = []orchapi.Session{}
	}
	api.WriteJSON(w, http.StatusOK, orchapi.SessionList{Sessions: ss, Next: next})
}

// writeSessionResponse keeps the final decoration and serialization within
// the output barrier. Mutations run before this gate because they may publish.
func (s *Service) writeSessionResponse(w http.ResponseWriter, r *http.Request, status int, ss orchapi.Session) {
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	defer controller.SetWriteDeadline(time.Time{})
	s.outputIO.RLock()
	defer s.outputIO.RUnlock()
	api.WriteJSON(w, status, s.dress(r.Context(), ss))
}

func (s *Service) hRegister(w http.ResponseWriter, r *http.Request) {
	var req orchapi.RegisterRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	ss, created, err := s.Register(r.Context(), req)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	s.writeSessionResponse(w, r, status, ss)
}

// hHook is an agent hook in one round trip: register (or find) the session,
// apply its state and, when asked, return its notice line. The notice never
// fails the call (as GET /v0/mail/notice never fails).
func (s *Service) hHook(w http.ResponseWriter, r *http.Request) {
	var req orchapi.HookRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	// A completion cannot prove that inherited ZM_SESSION names its runtime.
	// Resolve the full runtime instead; SessionStart has Notice, tools Activity.
	if req.State == orchapi.StateIdle && !req.Notice && req.Activity == "" && req.Subagent == nil {
		req.Session = ""
	}
	if err := cleanHook(&req); err != nil {
		api.WriteError(w, err)
		return
	}
	ctx := r.Context()
	hk := hookExtra{activity: clipLine(req.Activity), sub: req.Subagent, needs: req.Needs,
		start: req.Notice && req.State == orchapi.StateIdle}
	ss, _, err := s.register(ctx, req.RegisterRequest, hk)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	out := orchapi.HookResponse{Session: ss}
	if req.Notice {
		n := orchapi.Notice{}
		if m, ok := orchcore.Mail(s.d).(orchcore.Noticer); ok && ss.State != orchapi.StateEnded {
			read := m.Notice
			if hm, ok := m.(orchcore.HookNoticer); ok {
				read = hm.HookNotice
			}
			if got, err := read(ctx, core.SubjectOf(ctx).ForSession(ss.Address)); err == nil {
				n = got
			} else {
				s.d.Log.Warn("hook notice", "session", ss.Address, "err", err)
			}
		}
		out.Notice = &n
	}
	// A prompt hook has no input sequence to identify which submission it
	// acknowledges. It may arrive after a newer draft, so only ordered input
	// accounting may clear that draft; the hook still updates state/notices.
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	defer controller.SetWriteDeadline(time.Time{})
	s.outputIO.RLock()
	defer s.outputIO.RUnlock()
	out.Session = s.dress(ctx, out.Session)
	api.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) hAdmission(w http.ResponseWriter, r *http.Request) {
	api.WriteJSON(w, http.StatusOK, s.Admission(api.GetHeader(r.Header, api.HeaderCwd)))
}

func (s *Service) hGC(w http.ResponseWriter, r *http.Request) {
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	defer controller.SetWriteDeadline(time.Time{})
	s.outputIO.RLock()
	defer s.outputIO.RUnlock()
	sub := core.SubjectOf(r.Context())
	where, a := sub.Scope.SQL("project", "area", authz.Operate)
	rows, err := listRows(r.Context(), s.d.Store.Registry(), "WHERE "+where+" AND state = ? ORDER BY ended",
		append(a, string(orchapi.StateEnded))...)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	out := orchapi.GCList{Candidates: []orchapi.GCCandidate{}}
	for _, row := range rows {
		// A pending writer must not be advertised as safe to remove, and its
		// old outcome/path text must not appear in this maintenance surface.
		if held, err := s.sessionOutputHeld(r.Context(), row.ID); err != nil || held {
			out.Candidates = append(out.Candidates, orchapi.GCCandidate{Session: row.Address, Reasons: []string{"output withheld pending redaction"}})
			continue
		}
		row.Session = s.maskSessionOutputMetadata(r.Context(), row.Session)
		var n int
		task := s.d.Store.Registry().QueryRowContext(r.Context(), `SELECT length(transcript) FROM session_tasks WHERE id=?`, row.ID).Scan(&n) == nil
		if task && n == 0 {
			for _, path := range []string{holder.TaskOutputPath(s.d.Config.Home, row.ID), holder.TaskErrorPath(s.d.Config.Home, row.ID)} {
				if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
					n = 1
					break
				}
			}
		}
		c, ok := gcCandidate(r.Context(), row, task)
		if ok && task {
			active, e := s.handoffInUse(r.Context(), row.ID)
			if e != nil || active {
				c.Safe = false
				c.Remove = ""
				c.Reasons = append(c.Reasons, "continuation is pending or running")
			}
		}
		if n > 0 {
			if !ok {
				c = orchapi.GCCandidate{Session: row.Address, Outcome: row.Outcome, Reasons: []string{"ended task"}}
			}
			c.Transcript = true
			c.TranscriptRemove = "zm session gc --transcript " + row.ID
			ok = true
		}
		if ok {
			out.Candidates = append(out.Candidates, c)
		}
	}
	api.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) hGet(w http.ResponseWriter, r *http.Request) {
	ctx, id := r.Context(), r.PathValue("id")
	if s.remoteAddr(strings.ToLower(strings.TrimSpace(id))) {
		// A replica's session at a team authority (presence).
		sub := core.SubjectOf(ctx)
		// A capped process reads no session on another machine.
		if ss, ok := s.loadPresence(ctx, strings.ToLower(strings.TrimSpace(id))); ok && sub.Can(authz.Read, object(ss)) &&
			(!core.IsLocal(ctx) || s.capGate(sub).uncapped(ctx)) {
			api.WriteJSON(w, http.StatusOK, s.dress(ctx, ss))
			return
		}
		api.WriteError(w, api.NotFound("session", id))
		return
	}
	row, err := s.readable(r.Context(), core.SubjectOf(r.Context()), r.PathValue("id"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if sub := core.SubjectOf(ctx); sub != nil && sub.Scope.IsAll() && row.Managed {
		switch r.URL.Query().Get("sample") {
		case "1", "true":
			// The owner wants fg, busy and jobs as of now, once: no cycle of
			// samples and events follows (sample.go).
			s.sampleNow(row.ID)
		default:
			s.asked(row.ID)
		}
	}
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	defer controller.SetWriteDeadline(time.Time{})
	s.outputIO.RLock()
	defer s.outputIO.RUnlock()
	row, err = s.visible(ctx, core.SubjectOf(ctx), row.ID)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, s.dress(ctx, row.Session))
}

// hState is how hooks report state: the session itself, or anyone who may operate it.
func (s *Service) hState(w http.ResponseWriter, r *http.Request) {
	var req orchapi.StateRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	sub, id := core.SubjectOf(r.Context()), r.PathValue("id")
	row, err := s.visible(r.Context(), sub, id)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if sub.Session != row.Address {
		if err := sub.Check(authz.Operate, object(row.Session), "session", id); err != nil {
			api.WriteError(w, err)
			return
		}
	}
	if !s.capGate(sub).allows(r.Context(), row, authz.Operate, true) { // a capped process: its own sessions only
		api.WriteError(w, api.NotFound("session", id))
		return
	}
	ss, err := s.setState(r.Context(), row.ID, req.State, req.Note, true, "", orchapi.SessionNeeds{Kind: orchapi.NeedsManual}, nil)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	s.writeSessionResponse(w, r, http.StatusOK, ss)
}

func (s *Service) hInput(w http.ResponseWriter, r *http.Request) {
	var req orchapi.InputRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	if err := s.Input(r.Context(), core.SubjectOf(r.Context()), r.PathValue("id"), req); err != nil {
		api.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) hResize(w http.ResponseWriter, r *http.Request) {
	var req orchapi.ResizeRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	if err := s.Resize(r.Context(), core.SubjectOf(r.Context()), r.PathValue("id"), req); err != nil {
		api.WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) hStop(w http.ResponseWriter, r *http.Request) {
	var req orchapi.StopRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	// A capped process stops only its own sessions or ones below it, and
	// none on another machine.
	ctx, sub, id := r.Context(), core.SubjectOf(r.Context()), r.PathValue("id")
	gate := s.capGate(sub)
	local := core.IsLocal(ctx)
	if !local || s.remoteAddr(strings.ToLower(strings.TrimSpace(id))) {
		if sig := strings.ToUpper(strings.TrimPrefix(strings.ToUpper(req.Signal), "SIG")); sig != "" && sig != "TERM" || req.Escalate || req.Force {
			api.WriteError(w, api.Errorf(api.CodeBadRequest, "a stop from another machine is a plain TERM: no other signal, escalate or force"))
			return
		}
		if local && !gate.uncapped(ctx) {
			api.WriteError(w, api.NotFound("session", id))
			return
		}
		s.stopFar(w, r)
		return
	}
	row, err := s.visible(ctx, sub, id)
	if err == nil && !gate.allows(ctx, row, authz.Operate, true) {
		err = api.NotFound("session", id)
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	ss, sent, err := s.stop(ctx, sub, row.ID, req)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	w.Header().Set(orchapi.HeaderStopSignals, strings.Join(sent, ", "))
	s.writeSessionResponse(w, r, http.StatusOK, ss)
}

func (s *Service) hOutput(w http.ResponseWriter, r *http.Request) {
	var since int64
	if v := r.URL.Query().Get("since"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			api.WriteError(w, api.Errorf(api.CodeBadRequest, "since must be a byte offset"))
			return
		}
		since = n
	}
	row, err := s.operable(r.Context(), core.SubjectOf(r.Context()), r.PathValue("id"))
	if err == nil {
		err = s.taskSessionAccess(r.Context(), core.SubjectOf(r.Context()), row)
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if p := s.proc(row.ID); p != nil {
		p.waitCaught()
	}
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	defer controller.SetWriteDeadline(time.Time{})
	if err := s.withOutputRow(r.Context(), row, since, func(out orchapi.Output) error {
		api.WriteJSON(w, http.StatusOK, out)
		return nil
	}); err != nil {
		api.WriteError(w, err)
	}
}

// hAttach upgrades to a raw byte stream: PTY output (after a replay of the
// last 64 KiB of scrollback, or all of it with ?replay=all) one way,
// keystrokes the other. Closing it detaches; the session keeps running.
//
// ?input=0 is a read-only attach (a viewer): bytes from the client are read
// only to notice the hang-up and are dropped, never count as a person
// typing (a hook-set needs_human stays), and the connection does not count
// as an attached terminal, so the daemon still answers the session's cursor
// position queries itself.
func (s *Service) hAttach(w http.ResponseWriter, r *http.Request) {
	readOnly := false
	switch v := r.URL.Query().Get("input"); v {
	case "", "1", "true":
	case "0", "false":
		readOnly = true
	default:
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "input must be 0 (read-only) or 1, not %q", v))
		return
	}
	replay := int64(attachReplay)
	switch v := r.URL.Query().Get("replay"); v {
	case "":
	case "all":
		replay = scrollbackSize
	default:
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "replay must be all, not %q", v))
		return
	}
	row, err := s.operable(r.Context(), core.SubjectOf(r.Context()), r.PathValue("id"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if err := s.taskSessionAccess(r.Context(), core.SubjectOf(r.Context()), row); err != nil {
		api.WriteError(w, err)
		return
	}
	_, task := s.taskIDs.Load(row.ID)
	locked := false
	if task {
		s.taskIO.Lock()
		locked = true
		defer func() {
			if locked {
				s.taskIO.Unlock()
			}
		}()
	}
	s.outputIO.RLock()
	outputLocked := true
	defer func() {
		if outputLocked {
			s.outputIO.RUnlock()
		}
	}()
	if held, err := s.sessionOutputHeld(r.Context(), row.ID); err != nil || held {
		if err == nil {
			err = api.Errorf(api.CodeConflict, "session output withheld pending redaction")
		}
		api.WriteError(w, err)
		return
	}
	revision := s.outputRevisions[row.ID]
	if task && !readOnly {
		api.WriteError(w, api.Errorf(api.CodeConflict, "task attach is read-only"))
		return
	}
	if row.Mode == orchapi.SessionModePipe && !readOnly {
		api.WriteError(w, refusePipe(row.Address, "keystrokes (attach with ?input=0 to watch its raw stream)"))
		return
	}
	var withheld bool
	_ = s.d.Store.Registry().QueryRowContext(r.Context(), `SELECT text_withheld FROM session_tasks WHERE id=?`, row.ID).Scan(&withheld)
	if withheld {
		api.WriteError(w, api.Errorf(api.CodeConflict, "task output withheld pending redaction"))
		return
	}
	framed := headerHas(r.Header, "Upgrade", orchapi.AttachReplayProtocol)
	if framed && (r.Header.Get(orchapi.HeaderReplaySurface) != orchapi.FreshReplaySurface || r.URL.Query().Get("replay") != "all") {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "framed replay requires a fresh embedded surface and replay=all"))
		return
	}
	protocol := orchapi.AttachProtocol
	if framed {
		protocol = orchapi.AttachReplayProtocol
	}
	if !headerHas(r.Header, "Connection", "upgrade") || (!framed && !headerHas(r.Header, "Upgrade", orchapi.AttachProtocol)) {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "attach needs Connection: Upgrade and Upgrade: %s", orchapi.AttachProtocol))
		return
	}
	p, err := s.liveProc(row)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	conn, brw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		api.WriteError(w, api.Errorf(api.CodeInternal, "attach needs a connection that can be hijacked"))
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: %s\r\n%s: %s\r\n\r\n",
		protocol, api.HeaderNew(api.HeaderProtocol), api.WireProtocol); err != nil {
		return
	}
	p.addConn(conn, readOnly)
	s.outputIO.RUnlock()
	outputLocked = false
	if locked {
		s.taskIO.Unlock()
		locked = false
	}
	if !readOnly {
		defer s.wakeAfterDetach(context.WithoutCancel(r.Context()), p) // runs once removeConn has
	}
	defer p.removeConn(conn)

	detached := make(chan struct{})
	go func() { // output pump
		defer conn.Close()
		p.waitCaught()
		if framed {
			streamReplayAttach(p.out, &sessionOutputConn{Conn: conn, service: s, ctx: r.Context(), id: row.ID, revision: revision}, detached, replay)
			return
		}
		start, end := p.out.Bounds()
		off := max(start, end-replay)
		for {
			data, _, next, wait, closed := p.out.Next(off, 32<<10)
			if len(data) > 0 {
				_ = conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
				if _, err := (&sessionOutputConn{Conn: conn, service: s, ctx: r.Context(), id: row.ID, revision: revision}).Write(data); err != nil {
					return
				}
				off = next
				continue
			}
			if closed {
				return
			}
			select {
			case <-wait:
			case <-detached:
				return
			}
		}
	}()
	buf := make([]byte, 4096)
	// Focus reports, mouse reports and query answers are forwarded, never a person typing.
	keys := humanKeys{utf8Mouse: p.utf8Mouse.Load}
	for {
		n, err := brw.Reader.Read(buf)
		if n > 0 && !readOnly {
			if p.writeInput(buf[:n], keys.Feed(buf[:n])) != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	close(detached)
}

func headerHas(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

// sessionOutputConn gates every outbound write, including replay frames whose
// bytes were assembled before a redaction began. A revision change permanently
// invalidates that stream, even if the hold has already been cleared.
type sessionOutputConn struct {
	net.Conn
	service  *Service
	ctx      context.Context
	id       string
	revision uint64
}

func (c *sessionOutputConn) Write(data []byte) (n int, err error) {
	err = c.service.withSessionOutput(c.ctx, c.id, func() error {
		if c.revision != c.service.outputRevisions[c.id] {
			return api.Errorf(api.CodeConflict, "session output changed during redaction; reconnect")
		}
		var writeErr error
		n, writeErr = c.Conn.Write(data)
		return writeErr
	})
	return n, err
}
