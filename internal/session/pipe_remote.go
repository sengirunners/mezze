package session

// Sessions on other machines (SPEC 8): POST /v0/sessions with
// machine hands a structured session's start to the team module, which
// carries it through the project's authority (orchcore.RemoteSessions); on the
// target this file starts it (orchcore.RemoteSessionRunner): shared, in the
// project's checkout here, and capped (orchcore.SessionCaps): a request acting
// as it sees and does nothing outside that project, never more than the
// launcher's role (SPEC 1.3). Only a session started this way is stopped
// from another machine.

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var pipeRemoteMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "session/0021_remote_starts", Script: `
CREATE TABLE session_remote_starts (
	id      TEXT PRIMARY KEY,     -- session8
	op      TEXT NOT NULL UNIQUE, -- the authority's session_start op
	project TEXT NOT NULL,        -- the cap: this project only
	role    TEXT NOT NULL,        -- at most this role (the launcher's)
	asker   TEXT NOT NULL,        -- the identity that asked, at the authority
	created INTEGER NOT NULL
);`},
}

var (
	_ orchcore.RemoteSessionRunner = (*Service)(nil)
	_ orchcore.SessionCaps         = (*Service)(nil)
)

// sessionCap is a remote-started session's cap.
type sessionCap struct {
	project string
	role    authz.Relation
}

// remoteCaps: address -> sessionCap, loaded from the table until a load
// succeeds. Until then every check fails closed: caps
// count as present (anyCaps) and an unknown session's cap allows nothing
// (SessionCap).
type remoteCaps struct {
	mu       sync.Mutex // one load at a time
	loaded   atomic.Bool
	m        sync.Map
	loadSeam func() error // tests: fails a load
}

// unloadedProject is the cap of a session whose cap cannot be known (the
// caps did not load): no project, so nothing.
const unloadedProject = "\x00caps-unloaded"

// capsLoaded is the caps, loaded first when not yet (retried on every call
// until a load succeeds); ok false: they could not load.
func (s *Service) capsLoaded() (*sync.Map, bool) {
	c := &s.rcaps
	if c.loaded.Load() {
		return &c.m, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.loaded.Load() {
		return &c.m, true
	}
	if err := s.loadCaps(); err != nil {
		s.d.Log.Warn("session: load remote-started session caps (every cap check fails closed until they load)", "err", err)
		return &c.m, false
	}
	c.loaded.Store(true)
	return &c.m, true
}

func (s *Service) loadCaps() error {
	if f := s.rcaps.loadSeam; f != nil {
		if err := f(); err != nil {
			return err
		}
	}
	// Not a request's context: a cancelled request must not fail the load.
	rows, err := s.d.Store.Registry().QueryContext(context.Background(), `SELECT id, project, role FROM session_remote_starts`)
	if err != nil {
		return err
	}
	defer rows.Close()
	got := map[string]sessionCap{}
	for rows.Next() {
		var id, project, role string
		if err := rows.Scan(&id, &project, &role); err != nil {
			return err
		}
		got[api.SessionAddress(s.d.Config.Machine, id)] = sessionCap{project, authz.Relation(role)}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for a, c := range got {
		s.rcaps.m.Store(a, c)
	}
	return nil
}

// SessionCap implements orchcore.SessionCaps. While the caps cannot load,
// every session of this machine is capped to nothing (fails closed).
func (s *Service) SessionCap(address string) (string, authz.Relation, bool) {
	_, loaded := s.capsLoaded()
	return s.capKnown(address, loaded)
}

// capKnown is SessionCap without a load (inside a registry transaction,
// which a load would wait on); loaded: whether the caps loaded.
func (s *Service) capKnown(address string, loaded bool) (string, authz.Relation, bool) {
	if v, ok := s.rcaps.m.Load(address); ok {
		c := v.(sessionCap)
		return c.project, c.role, true
	}
	if !loaded {
		if a, err := api.ParseAddress(address); err == nil && a.Kind == api.KindSession && a.Machine == s.d.Config.Machine {
			return unloadedProject, authz.None, true
		}
	}
	return "", authz.None, false
}

// startSessionOn hands a spawn with machine set to the team module, in the
// project and area req's cwd resolves to here.
func (s *Service) startSessionOn(ctx context.Context, req orchapi.SpawnRequest) (orchapi.Session, error) {
	sub := core.SubjectOf(ctx)
	rs, ok := orchcore.Team(s.d).(orchcore.RemoteSessions)
	if !ok {
		return orchapi.Session{}, api.Errorf(api.CodeUnavailable, "a session on another machine needs the team module")
	}
	if sub.Session != "" {
		return orchapi.Session{}, api.Errorf(api.CodeForbidden, "a person starts sessions on other machines, not an agent session (%s)", sub.Session)
	}
	if len(req.Command) > 0 || len(req.Env) > 0 || len(req.MCP) > 0 || req.Work != "" || req.Name != "" || req.Prompt != "" || req.Sandbox ||
		req.ContinueFrom != "" {
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "command, env, mcp, work, name, prompt, sandbox and continue_from stay on this machine: leave them out with machine").
			WithHint("send the first turn after the session starts")
	}
	proj, area, err := s.target(ctx, req.Cwd, req.Area, authz.Operate, false)
	if err != nil {
		return orchapi.Session{}, err
	}
	if !s.teamProject(proj.ID) {
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "a session on another machine needs a team project: %s is not one", proj.Slug).
			WithHint("make the project a team project (zm project team <slug>), or leave machine out")
	}
	ss, err := rs.StartSessionOn(ctx, sub, proj.ID, area, req)
	if err == nil && len(req.SafeOutputs) > 0 && len(ss.SafeOutputs) == 0 {
		// The target's zm (or the authority's) predates safe_outputs and
		// dropped them: the session must not run uncapped.
		if _, err := s.relayOp(ctx, sub, ss, orchapi.SessionOp{Stop: &orchapi.SessionStopOp{}}); err != nil {
			s.d.Log.Warn("stop an uncapped remote session", "session", ss.Address, "err", err)
		}
		return orchapi.Session{}, api.Errorf(api.CodeConflict, "%s did not apply safe_outputs, so its session %s was stopped", ss.Machine, ss.Address).
			WithHint("update zm on " + ss.Machine + " and on the project's authority")
	}
	return ss, err
}

// StartRemoteSession implements orchcore.RemoteSessionRunner.
func (s *Service) StartRemoteSession(ctx context.Context, proj, opID, from string, op orchapi.SessionStartOp) (orchapi.Session, error) {
	if !s.d.Config.File.RemoteTasks {
		return orchapi.Session{}, api.Errorf(api.CodeForbidden, "%s takes no sessions from other machines", s.d.Config.Machine).
			WithHint("its owner sets \"remote_tasks\": true in its zm config.json and restarts its daemon")
	}
	if opID == "" {
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "session_start without a request")
	}
	role, err := authz.ParseRelation(op.Role)
	if err != nil {
		return orchapi.Session{}, err
	}
	req := op.Request
	switch {
	case req.Mode != orchapi.SessionModePipe || req.Agent != "claude" && req.Agent != "codex":
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "only a claude or codex structured session starts for another machine")
	case len(req.Command) > 0 || len(req.Env) > 0 || len(req.MCP) > 0 || req.Cwd != "" || req.Work != "" || req.Name != "" || req.Prompt != "" || req.Sandbox:
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "command, env, mcp, cwd, work, name, prompt and sandbox are not taken from another machine")
	}
	reg := s.d.Store.Registry()
	s.remoteStartMu.Lock()
	defer s.remoteStartMu.Unlock()
	var id string
	switch err := reg.QueryRowContext(ctx, `SELECT id FROM session_remote_starts WHERE op = ?`, opID).Scan(&id); err {
	case nil: // this op's session, started before (a repeated op)
		if r, ok := s.load(ctx, id); ok {
			return s.withSafeOutputs(ctx, r.Session, nil)
		}
		return orchapi.Session{}, api.NotFound("session", id)
	case sql.ErrNoRows:
	default:
		return orchapi.Session{}, err
	}
	req.Cwd = s.projectCheckout(ctx, proj)
	if req.Cwd == "" {
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "%s has no known checkout of the project", s.d.Config.Machine).
			WithHint("start a session in the project's checkout there once, so zm knows it")
	}
	req.Shared, req.Machine, req.ClientRequestID, req.Area = true, "", "", op.Area
	sub := s.d.Projects.Local("")
	ctx = core.WithSubject(ctx, sub)
	ss, err := s.spawnFor(ctx, req, nil, &remotePin{project: proj, op: opID, role: role, asker: from})
	return s.withSafeOutputs(ctx, ss, err)
}

// remotePin pins a session an authority asked for: its project here, and
// the cap recorded with it.
type remotePin struct {
	project, op, asker string
	role               authz.Relation
}

// recordRemoteStart keeps a remote-started session's cap (inside the
// session row's designation, so a failed start leaves none). An inherited
// cap has no op of its own.
func (s *Service) recordRemoteStart(ctx context.Context, id8 string, pin *remotePin) error {
	if err := insertCap(ctx, s.d.Store.Registry(), id8, pin); err != nil {
		return err
	}
	caps, _ := s.capsLoaded()
	caps.Store(api.SessionAddress(s.d.Config.Machine, id8), sessionCap{pin.project, pin.role})
	return nil
}

// insertCap writes recordRemoteStart's row (in db, which may be a tx).
func insertCap(ctx context.Context, db execer, id8 string, pin *remotePin) error {
	op := pin.op
	if op == "" {
		op = "inherit:" + id8
	}
	_, err := db.ExecContext(ctx, `INSERT INTO session_remote_starts (id, op, project, role, asker, created) VALUES (?, ?, ?, ?, ?, ?)`,
		id8, op, pin.project, string(pin.role), pin.asker, time.Now().UnixMilli())
	return err
}

// remoteStarted reports a session of this machine started for another one
// (not a child that only inherited its cap).
func (s *Service) remoteStarted(ctx context.Context, address string) bool {
	a, err := api.ParseAddress(address)
	if err != nil || a.Kind != api.KindSession {
		return false
	}
	var op string
	err = s.d.Store.Registry().QueryRowContext(ctx, `SELECT op FROM session_remote_starts WHERE id = ?`, a.Session).Scan(&op)
	return err == nil && !strings.HasPrefix(op, "inherit:")
}

// stopFar is POST /v0/sessions/{id}/stop from anyone but this machine's
// socket, or for another machine's session: a structured session started
// from another machine, stopped through its authority (relayed) or here.
func (s *Service) stopFar(w http.ResponseWriter, r *http.Request) {
	ctx, sub := r.Context(), core.SubjectOf(r.Context())
	ss, _, relay, err := s.pipeTarget(ctx, sub, r.PathValue("id"), true)
	switch {
	case err != nil:
		api.WriteError(w, err)
	case relay:
		res, err := s.relayOp(ctx, sub, ss, orchapi.SessionOp{Stop: &orchapi.SessionStopOp{}})
		writeOp(w, res, err)
	default:
		res, err := s.stopRemoteStarted(ctx, ss.Address)
		writeOp(w, res, err)
	}
}

// stopRemoteStarted stops this machine's session started for another
// machine, as this machine's owner; any other session answers 409.
func (s *Service) stopRemoteStarted(ctx context.Context, address string) (orchapi.SessionOpResult, error) {
	if !s.remoteStarted(ctx, address) {
		return orchapi.SessionOpResult{}, api.Errorf(api.CodeConflict, "session %s was not started from another machine: only its machine stops it", address)
	}
	if _, _, err := s.stop(ctx, s.d.Projects.Local(""), address, orchapi.StopRequest{}); err != nil {
		return orchapi.SessionOpResult{}, err
	}
	return orchapi.SessionOpResult{Op: "stop", OK: true}, nil
}
