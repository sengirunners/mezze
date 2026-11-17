package session

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

func (s *Service) pipeRoutes(r *core.Router) {
	const shared = "; from anyone but this machine's socket (or for another machine's session): a shared structured session in a team project, operate on its project/area"
	r.HandleFunc("GET /v0/sessions/{id}/events", core.Authed, "operate on the session; a structured session's events (SSE with Accept: text/event-stream)"+shared, s.hEvents)
	r.HandleFunc("POST /v0/sessions/{id}/turn", core.Authed, "operate on the session; a structured session's turn or steer"+shared+", no agent session, no images", s.hTurn)
	r.HandleFunc("POST /v0/sessions/{id}/interrupt", core.Authed, "operate on the session; end a structured session's running turn"+shared+", no agent session", s.hInterrupt)
	r.HandleFunc("POST /v0/sessions/{id}/answer", core.Authed, "operate on the session; answer a structured session's approval"+shared+", no agent session", s.hAnswer)
}

func writeOp(w http.ResponseWriter, res orchapi.SessionOpResult, err error) {
	if err != nil {
		api.WriteError(w, err)
		return
	}
	status := http.StatusOK
	if res.Pending {
		status = http.StatusAccepted
	}
	api.WriteJSON(w, status, res)
}

func (s *Service) hTurn(w http.ResponseWriter, r *http.Request) {
	var req orchapi.SessionTurnRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	ctx, sub := r.Context(), core.SubjectOf(r.Context())
	ss, local, relay, err := s.pipeTarget(ctx, sub, r.PathValue("id"), true)
	switch {
	case err != nil:
		api.WriteError(w, err)
	case relay:
		res, err := s.relayOp(ctx, sub, ss, orchapi.SessionOp{Turn: &req})
		writeOp(w, res, err)
	case !local && len(req.Images) > 0:
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "a turn from another machine carries no images"))
	default:
		res, err := s.Turn(ctx, sub, r.PathValue("id"), req)
		writeOp(w, res, err)
	}
}

func (s *Service) hInterrupt(w http.ResponseWriter, r *http.Request) {
	var req orchapi.SessionInterruptRequest
	if r.ContentLength != 0 {
		if err := api.DecodeJSON(r, &req); err != nil {
			api.WriteError(w, err)
			return
		}
	}
	ctx, sub := r.Context(), core.SubjectOf(r.Context())
	ss, _, relay, err := s.pipeTarget(ctx, sub, r.PathValue("id"), true)
	switch {
	case err != nil:
		api.WriteError(w, err)
	case relay:
		res, err := s.relayOp(ctx, sub, ss, orchapi.SessionOp{Interrupt: &req})
		writeOp(w, res, err)
	default:
		res, err := s.Interrupt(ctx, sub, r.PathValue("id"), req)
		writeOp(w, res, err)
	}
}

func (s *Service) hAnswer(w http.ResponseWriter, r *http.Request) {
	var req orchapi.SessionAnswerRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	ctx, sub := r.Context(), core.SubjectOf(r.Context())
	ss, _, relay, err := s.pipeTarget(ctx, sub, r.PathValue("id"), true)
	switch {
	case err != nil:
		api.WriteError(w, err)
	case relay:
		res, err := s.relayOp(ctx, sub, ss, orchapi.SessionOp{Answer: &req})
		writeOp(w, res, err)
	default:
		res, err := s.Answer(ctx, sub, r.PathValue("id"), req)
		writeOp(w, res, err)
	}
}

func intParam(r *http.Request, name string, def int64) (int64, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, api.Errorf(api.CodeBadRequest, "%s must be a whole number", name)
	}
	return n, nil
}

// hEvents serves a structured session's events after ?after= (a seq): one
// JSON page (waiting up to ?wait_s=, at most 45, for the first), or with
// Accept: text/event-stream a stream that resumes from Last-Event-ID and
// ends after the session's last event.
func (s *Service) hEvents(w http.ResponseWriter, r *http.Request) {
	after, err := intParam(r, "after", 0)
	if err == nil && r.Header.Get("Last-Event-ID") != "" {
		after, err = strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
		if err != nil || after < 0 {
			err = api.Errorf(api.CodeBadRequest, "Last-Event-ID must be an event seq")
		}
	}
	var limit, wait int64
	if err == nil {
		limit, err = intParam(r, "limit", 0)
	}
	if err == nil {
		wait, err = intParam(r, "wait_s", 0)
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	sub := core.SubjectOf(r.Context())
	id := r.PathValue("id")
	ss, _, relay, err := s.pipeTarget(r.Context(), sub, id, false)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if relay {
		s.relayEvents(w, r, sub, ss, after, limit, wait)
		return
	}
	row, err := s.operable(r.Context(), sub, id)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	id = row.ID
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		_, err := s.Events(r.Context(), sub, id, after, int(limit), time.Duration(wait)*time.Second)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		pageLimit := int(limit)
		if pageLimit <= 0 || pageLimit > maxEventsPage {
			pageLimit = maxEventsPage
		}
		controller := http.NewResponseController(w)
		_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
		defer controller.SetWriteDeadline(time.Time{})
		if err := s.withEventPage(r.Context(), id, after, pageLimit, func(fresh orchapi.SessionEvents) error {
			api.WriteJSON(w, http.StatusOK, fresh)
			return nil
		}); err != nil {
			api.WriteError(w, err)
		}
		return
	}
	// The first page checks access and existence before any stream starts.
	page, err := s.Events(r.Context(), sub, id, after, 500, 0)
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
		ended := false
		err = s.withEventPage(r.Context(), id, after, 500, func(fresh orchapi.SessionEvents) error {
			page = fresh
			for _, ev := range page.Events {
				var head struct {
					Seq  int64  `json:"seq"`
					Type string `json:"type"`
				}
				_ = json.Unmarshal(ev, &head)
				if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", head.Seq, head.Type, ev); err != nil {
					return err
				}
			}
			if len(page.Events) == 0 {
				if page.Ended {
					ended = true
					_, err := fmt.Fprint(w, ": ended\n\n")
					if err != nil {
						return err
					}
				} else if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
					return err
				}
			}
			fl.Flush()
			return nil
		})
		if err != nil || ended {
			return
		}
		_ = controller.SetWriteDeadline(time.Time{})
		after = page.Next
		if !core.IsLocal(r.Context()) {
			// Someone else's stream: still shared and theirs to read, each page
			// (a grant revoked, or the session unshared, ends it).
			if _, _, _, err = s.pipeTarget(r.Context(), sub, id, false); err != nil {
				fmt.Fprintf(w, ": error %s\n\n", strings.ReplaceAll(err.Error(), "\n", " "))
				fl.Flush()
				return
			}
		}
		if page, err = s.Events(r.Context(), sub, id, after, 500, 25*time.Second); err != nil {
			fmt.Fprintf(w, ": error %s\n\n", strings.ReplaceAll(err.Error(), "\n", " "))
			fl.Flush()
			return
		}
		if r.Context().Err() != nil {
			return
		}
	}
}
