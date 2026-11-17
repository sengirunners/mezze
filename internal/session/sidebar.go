package session

// What a cockpit's sidebar and stage show about a session (SPEC 8
// "Agents in a shell", "Subagents", "Why it waits", "Activity log"): the
// agent bound to a managed shell, its subagents, why it waits on a person,
// and its recent activity lines.

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

const (
	maxStoredSubagents = 32                 // per session, the subagents column
	subagentKeep       = time.Hour          // an ended subagent stays listed this long
	spawnKeep          = 2 * time.Minute    // a spawn no SubagentStart took
	subagentStale      = 30 * time.Minute   // a running subagent with no hook this long has ended (its stop was missed)
	activityKeep       = 200                // activity lines kept per session
	subagentLines      = 20                 // lines per subagent in GET .../subagents
	activityLogKeep    = 7 * 24 * time.Hour // an ended session's lines
	activityPruneGap   = time.Hour          // between two retention passes
)

// hookExtra is what a hook brings beyond a register call.
type hookExtra struct {
	activity string
	sub      *orchapi.HookSubagent
	needs    string
	start    bool // a session-start hook (notice asked, state idle)
}

// subagentEvent: the hook is a subagent's own (its start, stop or a step
// inside it), never its parent's: it moves no cwd and binds nothing.
func (hk hookExtra) subagentEvent() bool {
	return hk.sub != nil && hk.sub.Event != orchapi.SubagentSpawn
}

// step: a tool step inside a subagent.
func (hk hookExtra) step() bool {
	return hk.sub != nil && hk.sub.Event == "" && hk.sub.ID != ""
}

// cleanHook checks and cleans a hook request's subagent and needs.
func cleanHook(req *orchapi.HookRequest) error {
	switch req.Needs {
	case "", orchapi.NeedsPermission, orchapi.NeedsInput:
	default:
		return api.Errorf(api.CodeBadRequest, "needs must be permission or input, not %q", req.Needs)
	}
	sa := req.Subagent
	if sa == nil {
		return nil
	}
	sa.ID, sa.Type = strings.TrimSpace(sa.ID), clipLine(textclean.Clean(sa.Type))
	sa.Description = clipLine(textclean.Clean(sa.Description))
	if len(sa.ID) > 200 || strings.ContainsAny(sa.ID, " \t\r\n") {
		return api.Errorf(api.CodeBadRequest, "subagent id must be the runtime's agent_id (no spaces)")
	}
	switch sa.Event {
	case "", orchapi.SubagentStart, orchapi.SubagentStop:
		if sa.ID == "" {
			return api.Errorf(api.CodeBadRequest, "subagent id is required")
		}
	case orchapi.SubagentSpawn:
		sa.ID = ""
	default:
		return api.Errorf(api.CodeBadRequest, "subagent event must be start, stop or spawn, not %q", sa.Event)
	}
	return nil
}

// sync derives what a row's Session carries from its stored columns: needs
// only while needs_human, the live subagents (none once ended).
func (r *row) sync() {
	if r.State != orchapi.StateNeedsHuman {
		r.Needs = nil
	}
	r.Subagents = nil
	if r.State == orchapi.StateEnded {
		return
	}
	now := time.Now()
	for _, a := range r.subs {
		if a.ID != "" && a.State == orchapi.StateRunning && !subStale(a, now) {
			r.Subagents = append(r.Subagents, a)
		}
	}
	if n := len(r.Subagents); n > orchapi.MaxSessionSubagents {
		r.Subagents = r.Subagents[n-orchapi.MaxSessionSubagents:]
	}
}

// ---- agents in a managed shell ----

// bindable: a hook of agent naming managed session r ($ZM_SESSION) may bind
// there. r runs a shell (or another program), not an agent runtime.
func bindable(r row, agent string) bool {
	return r.Managed && r.State != orchapi.StateEnded && r.Agent != agent && !orchapi.AgentRuntimes[r.Agent]
}

func boundHere(r row, agent, rid string) bool {
	return r.RuntimeAgent != nil && r.RuntimeAgent.Agent == agent && r.RuntimeAgent.RuntimeID == rid
}

func sameBinding(a, b *orchapi.RuntimeAgent) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Agent == b.Agent && a.RuntimeID == b.RuntimeID
}

// boundTo finds the live managed session agent's runtime rid is bound to.
func boundTo(ctx context.Context, db *sql.DB, agent, rid string) (row, bool, error) {
	rows, err := listRows(ctx, db, `WHERE managed = 1 AND state != ? AND runtime_agent != ''
		AND json_extract(runtime_agent, '$.agent') = ? AND json_extract(runtime_agent, '$.runtime_id') = ? LIMIT 1`,
		string(orchapi.StateEnded), agent, rid)
	if err != nil || len(rows) == 0 {
		return row{}, false, err
	}
	return rows[0], true, nil
}

// bindShell binds agent's runtime rid to managed shell r and applies the
// hook there. ok=false: not bound here, and the caller registers the hook
// as before. Only the first agent binds: while it runs, another (a `claude
// -p` it starts inherits ZM_SESSION) gets its own row. A binding whose
// agent has exited (its pid gone; with no pid known, a new agent's
// session-start) is replaced. A conversation that already has a live row of
// its own (registered before this daemon bound agents) keeps it.
func (s *Service) bindShell(ctx context.Context, r row, req orchapi.RegisterRequest, agent, rid string, hk hookExtra) (orchapi.Session, bool, error) {
	if boundHere(r, agent, rid) {
		ss, err := s.applyBound(ctx, r, req, hk)
		return ss, true, err
	}
	if hk.subagentEvent() {
		return orchapi.Session{}, false, nil
	}
	if req.End {
		// The end of a conversation the shell no longer holds (a late end
		// after /clear rebound it): nothing to do, unless it has a row of
		// its own.
		if _, found, err := byRuntime(ctx, s.d.Store.Registry(), agent, rid); err != nil || found {
			return orchapi.Session{}, false, err
		}
		return r.Session, true, nil
	}
	if cur := r.RuntimeAgent; cur != nil {
		stale := hk.start
		if cur.PID > 1 && !(hk.start && req.PID == cur.PID) { // the same agent's new conversation (/clear, /resume) rebinds
			stale, _ = processGone(cur.PID, r.rtPIDStart, cur.Started)
		}
		if !stale {
			return orchapi.Session{}, false, nil
		}
	}
	reg := s.d.Store.Registry()
	if old, found, err := byRuntime(ctx, reg, agent, rid); err != nil {
		return orchapi.Session{}, false, err
	} else if found && old.State != orchapi.StateEnded {
		return orchapi.Session{}, false, nil
	}
	now := time.Now().UTC()
	if _, err := s.update(ctx, r.ID, "", func(x *row) (bool, error) {
		if x.State == orchapi.StateEnded || !sameBinding(x.RuntimeAgent, r.RuntimeAgent) {
			return false, nil // another hook bound it meanwhile
		}
		clearAgent(x)
		x.RuntimeAgent = &orchapi.RuntimeAgent{Agent: agent, RuntimeID: rid, Model: oneLine(req.Model), Started: storedTime(now)}
		takeRuntimePID(x, req.PID)
		return true, nil
	}); err != nil {
		return orchapi.Session{}, false, err
	}
	b, err := loadRow(ctx, reg, r.ID)
	if err != nil || !boundHere(b, agent, rid) {
		return orchapi.Session{}, false, err
	}
	s.publish(orchapi.EventSessionUpdated, b.Session)
	ss, err := s.applyBound(ctx, b, req, hk)
	return ss, true, err
}

// applyBound applies a hook of the agent bound to shell r: its end unbinds
// it; anything else goes to the shell's row as to the agent's own.
func (s *Service) applyBound(ctx context.Context, r row, req orchapi.RegisterRequest, hk hookExtra) (orchapi.Session, error) {
	if req.End {
		return s.unbind(ctx, r.ID, r.RuntimeAgent)
	}
	if !hk.subagentEvent() {
		s.follow(ctx, r, req.Cwd)
	}
	model, pid := oneLine(req.Model), req.PID
	task := ""
	if !hk.step() {
		task = hk.activity
	}
	return s.applyHook(ctx, r.ID, req, hk, func(x *row, _ time.Time) int {
		a := x.RuntimeAgent
		if a == nil || !sameBinding(a, r.RuntimeAgent) {
			return chNone
		}
		ch := chNone
		if model != "" && model != a.Model {
			a.Model, ch = model, chDurable
		}
		if a.PID <= 1 && takeRuntimePID(x, pid) {
			ch = max(ch, chQuiet)
		}
		if task != "" && task != a.Task {
			a.Task, ch = task, max(ch, chActivity)
		}
		return ch
	})
}

// takeRuntimePID records the bound agent's pid when it is a live process.
func takeRuntimePID(x *row, pid int) bool {
	if pid <= 1 || x.RuntimeAgent == nil {
		return false
	}
	p, ok := procStat(pid)
	if ok && p.zombie || !ok && !agentLive(pid) {
		return false
	}
	x.RuntimeAgent.PID, x.rtPIDStart = pid, p.start
	return true
}

// clearAgent drops what a bound agent left on its shell's row: the binding,
// its activity, subagents and a wait it reported.
func clearAgent(x *row) {
	x.RuntimeAgent, x.rtPIDStart, x.subs = nil, 0, nil
	x.Activity, x.ActivityAt = "", nil
	if x.State == orchapi.StateNeedsHuman || x.State == orchapi.StateRunning {
		x.State, x.Note = orchapi.StateIdle, ""
	}
}

// unbind ends the binding of agent a on shell id8 (its session-end hook,
// or its process gone).
func (s *Service) unbind(ctx context.Context, id8 string, a *orchapi.RuntimeAgent) (orchapi.Session, error) {
	typ := ""
	ss, err := s.update(ctx, id8, "", func(x *row) (bool, error) {
		if a == nil || !sameBinding(x.RuntimeAgent, a) || x.State == orchapi.StateEnded {
			return false, nil
		}
		typ = orchapi.EventSessionUpdated
		if x.State == orchapi.StateNeedsHuman || x.State == orchapi.StateRunning {
			typ = orchapi.EventSessionStateChanged
		}
		clearAgent(x)
		return true, nil
	})
	if err == nil && typ != "" {
		s.publish(typ, ss)
	}
	return ss, err
}

// reapBound unbinds agents whose process has exited without a session-end
// hook (Codex sends none).
func (s *Service) reapBound() {
	rows, err := listRows(s.ctx, s.d.Store.Registry(), `WHERE managed = 1 AND state != ? AND runtime_agent != ''`, string(orchapi.StateEnded))
	if err != nil {
		return
	}
	for _, r := range rows {
		if a := r.RuntimeAgent; a != nil && a.PID > 1 {
			if gone, _ := processGone(a.PID, r.rtPIDStart, a.Started); gone {
				_, _ = s.unbind(s.ctx, r.ID, a)
			}
		}
	}
}

// ---- subagents ----

// Change levels a hook's row mutation reports (the event it is worth).
const (
	chNone     = iota
	chQuiet    // a row write, no event
	chActivity // a live-only updated (activity alone)
	chDurable  // a stored updated
	chState    // a state_changed (a subagent's step ended the wait it caused)
)

// subStale: a running subagent no hook reached for subagentStale (its stop
// was missed: an interrupt, a crash) counts as ended.
func subStale(a orchapi.Subagent, now time.Time) bool {
	return a.State == orchapi.StateRunning && now.Sub(a.Updated) > subagentStale
}

// pruneSubs drops ended subagents past subagentKeep and spawns past
// spawnKeep, then the oldest ended (else oldest) past maxStoredSubagents.
func pruneSubs(subs []orchapi.Subagent, now time.Time) []orchapi.Subagent {
	out := subs[:0]
	for _, a := range subs {
		if subStale(a, now) {
			ended := a.Updated
			a.State, a.Ended = orchapi.StateEnded, &ended
		}
		switch {
		case a.ID == "" && now.Sub(a.Started) > spawnKeep:
		case a.State == orchapi.StateEnded && a.Ended != nil && now.Sub(*a.Ended) > subagentKeep:
		default:
			out = append(out, a)
		}
	}
	for len(out) > maxStoredSubagents {
		drop := 0
		for i, a := range out {
			if a.State != orchapi.StateRunning {
				drop = i
				break
			}
		}
		out = append(out[:drop], out[drop+1:]...)
	}
	return out
}

func findSub(subs []orchapi.Subagent, id string) int {
	for i, a := range subs {
		if a.ID == id {
			return i
		}
	}
	return -1
}

// subagentMut is the row change a subagent hook makes: a spawn waits for
// its SubagentStart; start takes the oldest spawn of its type (its
// description); stop ends it; a step records its activity (logged after
// the write: *line).
func subagentMut(sa orchapi.HookSubagent, activity string, line *string) func(*row, time.Time) int {
	return func(x *row, now time.Time) int {
		at := storedTime(now)
		subs := pruneSubs(x.subs, now)
		defer func() { x.subs = subs }()
		switch sa.Event {
		case orchapi.SubagentSpawn:
			subs = append(subs, orchapi.Subagent{Type: sa.Type, Description: sa.Description, Started: at, Updated: at})
			return chQuiet
		case orchapi.SubagentStop:
			ch := endWait(x, sa.ID)
			i := findSub(subs, sa.ID)
			if i < 0 || subs[i].State == orchapi.StateEnded {
				return max(ch, chQuiet)
			}
			subs[i].State, subs[i].Ended, subs[i].Updated = orchapi.StateEnded, &at, at
			return max(ch, chDurable)
		}
		ch := chQuiet
		i := findSub(subs, sa.ID)
		if i < 0 {
			ch = chDurable // a step of one whose start was missed (hooks installed mid-run) shows it too
			a := orchapi.Subagent{ID: sa.ID, Type: sa.Type, State: orchapi.StateRunning, Started: at, Updated: at}
			if sa.Event == orchapi.SubagentStart {
				for j, p := range subs {
					if p.ID == "" && (sa.Type == "" || p.Type == sa.Type) {
						a.Description = p.Description
						subs = append(subs[:j], subs[j+1:]...)
						break
					}
				}
			}
			subs, i = append(subs, a), len(subs)
			if sa.Event == orchapi.SubagentStart {
				return chDurable
			}
		}
		if subs[i].State != orchapi.StateRunning {
			subs[i].State, subs[i].Ended, ch = orchapi.StateRunning, nil, chDurable
		}
		if sa.Type != "" && subs[i].Type == "" {
			subs[i].Type, ch = sa.Type, chDurable
		}
		if sa.Event == "" && activity != "" && activity != subs[i].Activity {
			subs[i].Activity, *line = activity, activity
			ch = max(ch, chActivity)
		}
		subs[i].Updated = at
		if sa.Event == "" {
			ch = max(ch, endWait(x, sa.ID))
		}
		return ch
	}
}

// endWait ends a hook's wait that subagent id caused (SessionNeeds.Subagent):
// it went on (its next step) or stopped, so the prompt was answered.
func endWait(x *row, id string) int {
	if id == "" || x.State != orchapi.StateNeedsHuman || x.Needs == nil || x.Needs.Subagent != id {
		return chNone
	}
	x.State, x.Note, x.Needs = orchapi.StateRunning, "", nil
	return chState
}

// ---- activity log ----

// logActivity records one activity line of session id8 (subagent "" = its
// own) and trims the session's lines to activityKeep (one indexed delete).
func (s *Service) logActivity(id8, subagent, text string, at time.Time) {
	if text == "" {
		return
	}
	db := s.d.Store.Registry()
	if _, err := db.ExecContext(s.ctx, `INSERT INTO session_activity (session, subagent, at, text) VALUES (?,?,?,?)`,
		id8, subagent, store.Millis(at), text); err != nil {
		s.d.Log.Warn("activity log", "session", id8, "err", err)
		return
	}
	_, _ = db.ExecContext(s.ctx, `DELETE FROM session_activity WHERE session = ? AND seq <=
		(SELECT seq FROM session_activity WHERE session = ? ORDER BY seq DESC LIMIT 1 OFFSET ?)`, id8, id8, activityKeep)
}

// pruneActivity drops the lines of sessions ended over activityLogKeep ago
// (and of sessions gone from the table).
func (s *Service) pruneActivity() {
	cut := store.Millis(time.Now().Add(-activityLogKeep))
	_, _ = s.d.Store.Registry().ExecContext(s.ctx, `DELETE FROM session_activity WHERE session IN
		(SELECT id FROM sessions WHERE state = ? AND ended < ?) OR session NOT IN (SELECT id FROM sessions)`,
		string(orchapi.StateEnded), cut)
}

func (s *Service) activityLines(ctx context.Context, id8 string, subagent *string, limit int) ([]orchapi.ActivityLine, error) {
	q, a := `SELECT at, text, subagent FROM session_activity WHERE session = ?`, []any{id8}
	if subagent != nil {
		q, a = q+` AND subagent = ?`, append(a, *subagent)
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, q+` ORDER BY seq DESC LIMIT ?`, append(a, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []orchapi.ActivityLine{}
	for rows.Next() {
		var l orchapi.ActivityLine
		var at int64
		if err := rows.Scan(&at, &l.Text, &l.Subagent); err != nil {
			return nil, err
		}
		l.At = store.FromMillis(at)
		out = append(out, l)
	}
	return out, rows.Err()
}

// hActivity is GET /v0/sessions/{id}/activity?limit=&subagent=.
func (s *Service) hActivity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	row, err := s.readable(ctx, core.SubjectOf(ctx), r.PathValue("id"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > activityKeep {
			api.WriteError(w, api.Errorf(api.CodeBadRequest, "limit must be 1..%d", activityKeep))
			return
		}
	}
	var sub *string
	if r.URL.Query().Has("subagent") {
		v := r.URL.Query().Get("subagent")
		sub = &v
	}
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	defer controller.SetWriteDeadline(time.Time{})
	err = s.withSessionOutput(ctx, row.ID, func() error {
		// Reauthorize the current row under the delivery barrier: a row or its
		// activity read before a completed scrub must not become a cached reply.
		current, err := s.visible(ctx, core.SubjectOf(ctx), row.ID)
		if err != nil {
			return err
		}
		lines, err := s.activityLines(ctx, current.ID, sub, limit)
		if err != nil {
			return err
		}
		api.WriteJSON(w, http.StatusOK, orchapi.ActivityList{Lines: lines})
		return nil
	})
	if err != nil {
		api.WriteError(w, err)
	}
}

// hSubagents is GET /v0/sessions/{id}/subagents: every subagent kept
// (running, and ended within subagentKeep), oldest first, with its lines.
func (s *Service) hSubagents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	row, err := s.readable(ctx, core.SubjectOf(ctx), r.PathValue("id"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	defer controller.SetWriteDeadline(time.Time{})
	err = s.withSessionOutput(ctx, row.ID, func() error {
		current, err := s.visible(ctx, core.SubjectOf(ctx), row.ID)
		if err != nil {
			return err
		}
		out := orchapi.SubagentList{Subagents: []orchapi.SubagentDetail{}}
		for _, a := range current.subs {
			if a.ID == "" {
				continue
			}
			switch {
			case current.State == orchapi.StateEnded && a.State == orchapi.StateRunning:
				a.State, a.Ended = orchapi.StateEnded, current.Ended
			case subStale(a, time.Now()):
				ended := a.Updated
				a.State, a.Ended = orchapi.StateEnded, &ended
			}
			id := a.ID
			lines, err := s.activityLines(ctx, current.ID, &id, subagentLines)
			if err != nil {
				return err
			}
			out.Subagents = append(out.Subagents, orchapi.SubagentDetail{Subagent: a, Lines: lines})
		}
		api.WriteJSON(w, http.StatusOK, out)
		return nil
	})
	if err != nil {
		api.WriteError(w, err)
	}
}
