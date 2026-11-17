package team

// Sessions on other machines (SPEC 8): this machine's owner
// starts a shared structured session on another machine of a team project,
// so a scheduled Mezze app run (provider-zm's machine option) can run there. It
// rides the machine-ops queue as a session_start op, with machine choice's
// who-may: the caller's identity owns or administers the project, and the
// target is trusted and opted in ("remote_tasks"). The answer waits for the
// target's report and then for the session's presence row, so the caller's
// first turn (relayed through the authority) finds it.

import (
	"context"
	"encoding/json"
	"github.com/sengirunners/potluck/kit/teamlink"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
)

// sessionStartWait bounds how long a start waits for the target (under a
// replica's CallTimeout, which forwards the call). Tests shorten it.
var sessionStartWait = 25 * time.Second

// sessionStartReport: a carried session start with no report by then is
// ended (its machine did not report it). Tests shorten it.
var sessionStartReport = 3 * time.Minute

var _ orchcore.RemoteSessions = (*Service)(nil)

// StartSessionOn implements orchcore.RemoteSessions.
func (s *Service) StartSessionOn(ctx context.Context, sub *authz.Subject, project, area string, req orchapi.SpawnRequest) (orchapi.Session, error) {
	rr := orchapi.RemoteSessionRequest{Project: project, Machine: req.Machine, Area: area, Request: remoteSpawn(req)}
	var ss orchapi.Session
	var err error
	if x := s.ByName(project); x != nil {
		rr.Project = x.Snapshot().Project.ID
		err = s.CallRemote(ctx, x, http.MethodPost, "/v0/team/sessions", rr, &ss)
		err = teamlink.OlderFor(err, x, "sessions on other machines")
	} else if s.d.Store.IsTeam(project) {
		ss, err = s.createSessionOp(ctx, sub, s.d.Config.Machine, rr)
	} else {
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "a session on another machine needs a team project: this one is not").
			WithHint("start it from a team project's checkout, or leave machine out")
	}
	return ss, err
}

// remoteSpawn is what of a spawn request crosses to another machine: no
// command, environment, cwd, work, name or prompt from the caller's side.
func remoteSpawn(req orchapi.SpawnRequest) orchapi.SpawnRequest {
	return orchapi.SpawnRequest{Agent: req.Agent, Model: req.Model, Title: req.Title, Mode: req.Mode, Approvals: req.Approvals,
		Resume: req.Resume, Worktree: req.Worktree, Shared: true, ClientRequestID: req.ClientRequestID, For: req.For, JobKind: req.JobKind,
		AllowedTools: req.AllowedTools, PermissionMode: req.PermissionMode, SafeOutputs: req.SafeOutputs}
}

// checkRemoteSpawn is the request's shape, checked at the authority and
// again on the target.
func checkRemoteSpawn(req orchapi.SpawnRequest) error {
	if err := orchapi.CheckToolGrant(req.AllowedTools, req.PermissionMode); err != nil {
		return err
	}
	if err := orchapi.CheckSafeOutputs(req.SafeOutputs); err != nil {
		return err
	}
	switch {
	case req.Mode != orchapi.SessionModePipe:
		return api.Errorf(api.CodeBadRequest, "only a structured session (mode pipe) starts on another machine")
	case req.Agent != "claude" && req.Agent != "codex":
		return api.Errorf(api.CodeBadRequest, "agent must be claude or codex")
	case len(req.Command) > 0 || len(req.Env) > 0 || len(req.MCP) > 0 || req.Cwd != "" || req.Work != "" || req.Name != "" || req.Prompt != "" || req.Sandbox:
		return api.Errorf(api.CodeBadRequest, "command, env, mcp, cwd, work, name, prompt and sandbox stay on the caller's machine")
	case req.Approvals != "" && req.Approvals != "ask" && req.Approvals != "never":
		return api.Errorf(api.CodeBadRequest, "approvals must be ask or never")
	case len(req.Model) > 200 || len(req.Title) > 500 || len(req.Resume) > 200 || len(req.ClientRequestID) > 200 || len(req.For) > 200 || len(req.JobKind) > 200:
		return api.Errorf(api.CodeBadRequest, "model, title, resume, for, job_kind or client_request_id is too long")
	}
	return nil
}

// sessionRoutes registers the authority's route.
func (s *Service) sessionRoutes(r *core.Router) {
	r.HandleFunc("POST /v0/team/sessions", core.Authed, "a replica's node-bound token only; its identity owns or administers the team project; the machine is trusted and takes remote tasks", jsonH(func(r *http.Request) (any, error) {
		sub := core.SubjectOf(r.Context())
		if sub.Node == "" || sub.Machine == "" {
			return nil, api.Errorf(api.CodeForbidden, "a team replica's daemon asks for sessions (its node-bound token)").
				WithHint("start one with POST /v0/sessions and machine, on a machine of the project")
		}
		var req orchapi.RemoteSessionRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.createSessionOp(r.Context(), sub, sub.Machine, req)
	}))
}

// createSessionOp queues a session_start for from (or runs it here) and
// waits for the session.
func (s *Service) createSessionOp(ctx context.Context, sub *authz.Subject, from string, req orchapi.RemoteSessionRequest) (orchapi.Session, error) {
	rt, err := s.runner()
	if err != nil {
		return orchapi.Session{}, err
	}
	project, err := s.OwnerProject(ctx, sub, req.Project, "starts sessions on other machines")
	if err != nil {
		return orchapi.Session{}, err
	}
	req.Request.Shared = true
	if err := checkRemoteSpawn(req.Request); err != nil {
		return orchapi.Session{}, err
	}
	if req.Area != "" && !s.d.Projects.HasArea(project, req.Area) {
		return orchapi.Session{}, api.NotFound("area", req.Area)
	}
	if !sub.Can(authz.Operate, authz.Object{Project: project, Area: req.Area}) {
		return orchapi.Session{}, api.Errorf(api.CodeForbidden, "your role cannot start sessions in this project")
	}
	req.Request.Machine = ""
	reqJSON, _ := json.Marshal(req)
	if rid := req.Request.ClientRequestID; rid != "" {
		o, err := s.FindOp(ctx, project, from, "", rid, opSessionStart)
		if err == nil {
			if o.KeyBody != string(reqJSON) {
				return orchapi.Session{}, api.Errorf(api.CodeConflict, "client_request_id was used for a different request")
			}
			return s.waitSessionStart(ctx, o.ID)
		}
		if !teamlink.IsNoOp(err) {
			return orchapi.Session{}, err
		}
	}
	s.ExpireOps(ctx)
	// Operator class (SPEC 8): the session acts in the project with the
	// launcher's authority, so it goes only to a trusted machine.
	trusted, public, err := s.TrustedMachines(ctx, project, rt.ProjectMachines(ctx, project))
	if err != nil {
		return orchapi.Session{}, err
	}
	if public[req.Machine] {
		return orchapi.Session{}, teamlink.PublicRefused(req.Machine, "takes sessions from other machines")
	}
	if req.Machine == from && req.Machine != "auto" {
		return orchapi.Session{}, api.Errorf(api.CodeBadRequest, "%s is the caller's own machine: leave machine out to start it there", from)
	}
	machine, err := pickMachine(trusted, req.Machine, from, s.d.Config.Machine, false)
	if err != nil {
		return orchapi.Session{}, err
	}
	payload, _ := json.Marshal(orchapi.SessionStartOp{Request: req.Request, Area: req.Area, Role: string(authz.Owner)})
	op := api.MachineOp{ID: ids.New(), Kind: opSessionStart, From: sub.Identity.Name, Payload: payload}
	if err := s.QueueOp(ctx, teamlink.Op{MachineOp: op, Project: project, Machine: machine, FromMachine: from,
		Key: req.Request.ClientRequestID, KeyBody: string(reqJSON)}); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") && req.Request.ClientRequestID != "" {
			return s.createSessionOp(ctx, sub, from, req) // a racing retry: answer the first
		}
		return orchapi.Session{}, err
	}
	if machine == s.d.Config.Machine {
		s.RunHere(ctx, project, op)
	}
	return s.waitSessionStart(ctx, op.ID)
}

// waitSessionStart answers op id's session once its machine reported it and
// this authority has its presence row, or its refusal; a start still
// running past sessionStartWait is 503 (a retry with the same
// client_request_id waits for it again).
func (s *Service) waitSessionStart(ctx context.Context, id string) (orchapi.Session, error) {
	deadline := time.Now().Add(sessionStartWait)
	for {
		o, err := s.LoadOp(ctx, id)
		if err != nil {
			return orchapi.Session{}, err
		}
		if o.State == teamlink.OpDone {
			if o.Error != "" {
				var rr relayResult
				_ = json.Unmarshal([]byte(o.Result), &rr)
				code := rr.Code
				if code == "" {
					code = api.CodeUnavailable
				}
				hint := rr.Hint
				if o.Error == "not taken in time" || o.Error == "its machine did not report it" {
					hint = "start again with a new client_request_id; a session its machine started late shows in its zm ls"
				}
				return orchapi.Session{}, &api.Error{Code: code, Message: o.Error, Hint: hint}
			}
			var ss orchapi.Session
			if err := json.Unmarshal([]byte(o.Result), &ss); err != nil || ss.Address == "" {
				return orchapi.Session{}, api.Errorf(api.CodeUnavailable, "%s answered no session", o.Machine)
			}
			// The machine's next heartbeat carries its presence row; the
			// caller's first turn is refused without it.
			if o.Machine == s.d.Config.Machine {
				return ss, nil
			}
			if _, ok := orchcore.Sessions(s.d).Lookup(ctx, ss.Address); ok {
				return ss, nil
			}
		}
		if time.Now().After(deadline) {
			return orchapi.Session{}, api.Errorf(api.CodeUnavailable, "the session on %s has not started yet", o.Machine).
				WithHint("retry with the same client_request_id: it answers the same start (without one, a retry starts another)")
		}
		select {
		case <-ctx.Done():
			return orchapi.Session{}, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// opSessionStart starts a shared structured session on the machine (SPEC
// 8 "Sessions on other machines"): payload orchapi.SessionStartOp, report
// orchapi.Session.
const opSessionStart = "session_start"

// registerSessionStartOps registers session_start with teamlink's queue.
func (s *Service) registerSessionStartOps() {
	s.RegisterOp(opSessionStart, teamlink.OpHandler{Run: s.runSessionStart, Apply: s.applySessionStart,
		Expiry: relayOpTake, ReportWithin: sessionStartReport})
}

// applySessionStart takes a machine's report on a session_start.
func (s *Service) applySessionStart(ctx context.Context, o teamlink.Op, rep api.MachineOpReport) {
	res := ""
	if rep.Error != "" {
		b, _ := json.Marshal(relayResult{Code: rep.Code, Hint: rep.Hint})
		res = string(b)
	} else if len(rep.Payload) > 0 {
		res = string(rep.Payload)
	}
	s.FinishOp(ctx, o.ID, res, rep.Error)
}

// runSessionStart runs a session_start here.
func (s *Service) runSessionStart(ctx context.Context, project string, op api.MachineOp) (json.RawMessage, error) {
	sr, ok := orchcore.Sessions(s.d).(orchcore.RemoteSessionRunner)
	var p orchapi.SessionStartOp
	if !ok || json.Unmarshal(op.Payload, &p) != nil {
		return nil, api.Errorf(api.CodeUnavailable, "this machine starts no sessions for other machines")
	}
	ss, err := sr.StartRemoteSession(ctx, project, op.ID, op.From, p)
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(ss)
	return b, nil
}
