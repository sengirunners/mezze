package session

// Structured sessions for members: a structured session
// shared at spawn in a team project takes reads and operations from the
// project's members, like a shared session's prompts (SPEC 17.10): the
// caller needs operate on its project and area, agents are refused
// operations as a courtesy, and images never come from someone else's
// machine (a path names a file here). Another machine's session goes through
// its authority: the team module relays it (orchcore.SessionRelay) and, on the
// session's machine, runs what comes (orchcore.SessionPipe, below).

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

var _ orchcore.SessionPipe = (*Service)(nil)

// pipeTarget is the session a structured-session call names when the local
// rules do not apply. local=true: this machine's own session called on its
// own socket, which the existing handlers serve as before. Else ss is a
// shared structured session of a team project sub may operate (relay: it
// is another machine's, through the team module).
func (s *Service) pipeTarget(ctx context.Context, sub *authz.Subject, raw string, op bool) (ss orchapi.Session, local, relay bool, err error) {
	id := strings.ToLower(strings.TrimSpace(raw))
	if core.IsLocal(ctx) && !s.remoteAddr(id) {
		return orchapi.Session{}, true, false, nil
	}
	ss, err = s.promptTarget(ctx, sub, raw, authz.Operate)
	if err != nil {
		return orchapi.Session{}, false, false, err
	}
	switch {
	case ss.Mode != orchapi.SessionModePipe:
		return orchapi.Session{}, false, false, api.Errorf(api.CodeConflict, "session %s is a terminal session: it has output, not events", ss.Address)
	case !ss.Shared:
		return orchapi.Session{}, false, false, api.Errorf(api.CodeConflict, "session %s is not shared: its events and turns stay on its machine", ss.Address).
			WithHint("a structured session is shared at spawn: zm run --pipe --shared, in a team project")
	case !s.teamProject(ss.Project):
		return orchapi.Session{}, false, false, api.Errorf(api.CodeConflict, "session %s is not in a team project", ss.Address)
	}
	if op {
		if sub != nil && sub.Session != "" {
			return orchapi.Session{}, false, false, api.Errorf(api.CodeForbidden, "only a person operates another's shared session, not an agent session (%s)", sub.Session)
		}
		if ss.State == orchapi.StateEnded {
			return orchapi.Session{}, false, false, api.Errorf(api.CodeConflict, "session %s has ended", ss.Address)
		}
	}
	return ss, false, s.remoteAddr(ss.Address), nil
}

func (s *Service) sessionRelay() (orchcore.SessionRelay, error) {
	rl, ok := orchcore.Team(s.d).(orchcore.SessionRelay)
	if !ok {
		return nil, api.Errorf(api.CodeUnavailable, "this daemon relays no sessions of other machines")
	}
	return rl, nil
}

// relayCaps refuses, before anything is queued, an operation the session's
// adapter cannot do (its machine checks again).
func relayCaps(ss orchapi.Session, op orchapi.SessionOp) error {
	caps := orchapi.AgentCapabilities{}
	if ss.Capabilities != nil {
		caps = *ss.Capabilities
	}
	switch {
	case op.Turn != nil && op.Turn.Steer && !caps.Steer:
		return refuseCap(ss.Address, "steer a running turn")
	case op.Turn != nil && !op.Turn.Steer && op.Turn.Model != "" && !caps.Model:
		return refuseCap(ss.Address, "change the model")
	case op.Interrupt != nil && !caps.Interrupt:
		return refuseCap(ss.Address, "interrupt a turn")
	case op.Answer != nil && !caps.Approvals:
		return refuseCap(ss.Address, "ask for approvals")
	}
	return nil
}

// relayOp sends op to another machine's session through its authority.
func (s *Service) relayOp(ctx context.Context, sub *authz.Subject, ss orchapi.Session, op orchapi.SessionOp) (orchapi.SessionOpResult, error) {
	if op.Turn != nil && len(op.Turn.Images) > 0 {
		return orchapi.SessionOpResult{}, api.Errorf(api.CodeBadRequest, "a turn from another machine carries no images")
	}
	if err := relayCaps(ss, op); err != nil {
		return orchapi.SessionOpResult{}, err
	}
	rl, err := s.sessionRelay()
	if err != nil {
		return orchapi.SessionOpResult{}, err
	}
	return rl.RelayOp(ctx, sub, ss, op)
}

// relayEvents serves another machine's session's events, as hEvents does
// this machine's: one JSON page, or an SSE stream.
func (s *Service) relayEvents(w http.ResponseWriter, r *http.Request, sub *authz.Subject, ss orchapi.Session, after, limit, wait int64) {
	rl, err := s.sessionRelay()
	if err != nil {
		api.WriteError(w, err)
		return
	}
	ctx := r.Context()
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		page, err := rl.RelayEvents(ctx, ss, after, int(limit), time.Duration(wait)*time.Second)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, page)
		return
	}
	page, err := rl.RelayEvents(ctx, ss, after, relayPageMax, 0)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "streaming is not supported here"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, ": "+api.WireProtocol+" session events\n\n")
	fl.Flush()
	controller := http.NewResponseController(w)
	defer controller.SetWriteDeadline(time.Time{})
	for {
		_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
		for _, ev := range page.Events {
			var head struct {
				Seq  int64  `json:"seq"`
				Type string `json:"type"`
			}
			_ = json.Unmarshal(ev, &head)
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", head.Seq, head.Type, ev); err != nil {
				return
			}
		}
		if len(page.Events) == 0 {
			if page.Ended {
				fmt.Fprint(w, ": ended\n\n")
				fl.Flush()
				return
			}
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
		}
		fl.Flush()
		_ = controller.SetWriteDeadline(time.Time{})
		after = page.Next
		// The caller and the session again, each page: a grant revoked, or the
		// session moved or unshared, ends the stream (its presence row may
		// also have ended: the relay says so once the ring has nothing more).
		cur, _, _, err := s.pipeTarget(ctx, sub, ss.Address, false)
		if err != nil {
			fmt.Fprintf(w, ": error %s\n\n", strings.ReplaceAll(err.Error(), "\n", " "))
			fl.Flush()
			return
		}
		ss = cur
		if page, err = rl.RelayEvents(ctx, ss, after, relayPageMax, 25*time.Second); err != nil {
			fmt.Fprintf(w, ": error %s\n\n", strings.ReplaceAll(err.Error(), "\n", " "))
			fl.Flush()
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

const relayPageMax = 500

// pipeShared is this machine's shared structured session address in
// project, for the authority's relay.
func (s *Service) pipeShared(ctx context.Context, project, address string) (row, error) {
	if s.remoteAddr(address) {
		return row{}, api.NotFound("session", address)
	}
	r, ok := s.load(ctx, address)
	if !ok || r.Address != address || r.Project != project || !r.Shared || r.Mode != orchapi.SessionModePipe {
		return row{}, api.NotFound("session", address)
	}
	return r, nil
}

// PipeEvents implements orchcore.SessionPipe: a page under the session's output
// barrier (a redaction hold withholds it here too).
func (s *Service) PipeEvents(ctx context.Context, project, address string, after int64, limit int) (orchapi.SessionEvents, error) {
	r, err := s.pipeShared(ctx, project, address)
	if err != nil {
		return orchapi.SessionEvents{}, err
	}
	if limit <= 0 || limit > maxEventsPage {
		limit = maxEventsPage
	}
	var page orchapi.SessionEvents
	err = s.withEventPage(ctx, r.ID, after, limit, func(v orchapi.SessionEvents) error { page = v; return nil })
	return page, err
}

// PipeOp implements orchcore.SessionPipe: op as this machine's owner runs it,
// never with images.
func (s *Service) PipeOp(ctx context.Context, project, address string, op orchapi.SessionOp) (orchapi.SessionOpResult, error) {
	r, err := s.pipeShared(ctx, project, address)
	if err != nil {
		return orchapi.SessionOpResult{}, err
	}
	owner := s.d.Projects.Local("")
	switch {
	case op.Turn != nil && op.Interrupt == nil && op.Answer == nil && op.Stop == nil:
		if len(op.Turn.Images) > 0 {
			return orchapi.SessionOpResult{}, api.Errorf(api.CodeBadRequest, "a turn from another machine carries no images")
		}
		return s.Turn(ctx, owner, r.ID, *op.Turn)
	case op.Interrupt != nil && op.Turn == nil && op.Answer == nil && op.Stop == nil:
		return s.Interrupt(ctx, owner, r.ID, *op.Interrupt)
	case op.Answer != nil && op.Turn == nil && op.Interrupt == nil && op.Stop == nil:
		return s.Answer(ctx, owner, r.ID, *op.Answer)
	case op.Stop != nil && op.Turn == nil && op.Interrupt == nil && op.Answer == nil:
		return s.stopRemoteStarted(ctx, r.Address)
	}
	return orchapi.SessionOpResult{}, api.Errorf(api.CodeBadRequest, "one operation: a turn, an interrupt, an answer or a stop")
}
