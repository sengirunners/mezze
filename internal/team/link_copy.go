package team

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// copyLink is a link written on the copy (POST /v0/work/{id}/link). One to
// a URL, a session or an item the copy holds answers at once, overlaid on
// reads; one to a work item the copy does not hold (another project's) waits
// for the authority like a claim, since only the authority can say whether
// it reaches it: a refusal goes to the caller, never sits in the outbox
// failed.
func (s *Service) copyLink(w http.ResponseWriter, r *http.Request, rp *replica, it orchapi.WorkItem, wpath string, body []byte, as string) bool {
	ctx := r.Context()
	var req orchapi.Link
	if err := json.Unmarshal(body, &req); err != nil || strings.TrimSpace(req.Rel) == "" || strings.TrimSpace(req.Target) == "" {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "a link needs a rel and a target"))
		return true
	}
	view := func() (int, any) {
		v, err := rp.whole(ctx, it, false)
		if err != nil {
			e := &api.Error{Code: api.CodeInternal, Message: "internal error"}
			_ = errors.As(err, &e)
			return e.Status(), api.ErrorResponse{Error: e}
		}
		v.Comments = nil
		return http.StatusOK, v
	}
	if linkLeavesCopy(ctx, rp, req.Target) {
		return s.syncWrite(w, r, rp, "link", http.MethodPost, wpath, body, as, it.ID, func(outEntry, string) (int, any) { return view() })
	}
	if _, err := rp.enqueue(ctx, "link", http.MethodPost, wpath, body, as, it.ID, ""); err != nil {
		api.WriteError(w, err)
		return true
	}
	code, v := view()
	api.WriteJSON(w, code, v)
	return true
}

// linkLeavesCopy reports whether a link target is a work item this copy
// does not hold (another project's, or one not here yet).
func linkLeavesCopy(ctx context.Context, rp *replica, target string) bool {
	a, err := api.ParseAddress(strings.TrimSpace(target))
	if err != nil || a.Kind != api.KindWork {
		return false
	}
	_, _, ok := rp.findItem(ctx, a.String())
	return !ok
}

// linkGoneFor is why a refused link no longer belongs in the outbox ("" while
// it does): its target is a work item this copy does not hold (another
// project's). Such a link never applies by waiting, and a copy written now
// refuses it to its caller (copyLink); one queued while the authority was
// out, or left by an older zm, goes once settled, its session told first
// (settle). A link to an item that was never carried over stayed failed for days.
func linkGoneFor(ctx context.Context, rp *replica, e outEntry) string {
	if e.kind != "link" || e.state != api.OutboxFailed {
		return ""
	}
	var req orchapi.Link
	if json.Unmarshal([]byte(e.body), &req) != nil || !linkLeavesCopy(ctx, rp, req.Target) {
		return ""
	}
	return "its target " + strings.TrimSpace(req.Target) + " is not an item this copy holds (another project's)"
}
