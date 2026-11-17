package issues

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
)

// Routes registers /v0/issues. Issue ids are one path segment.
func (s *Service) Routes(r *core.Router) {
	r = r.For("issues")
	r.HandleFunc("GET /v0/issues", core.Authed, "read: projects in ReadScope(?project=); issues whose area the caller reads", s.jsonH(func(r *http.Request, sub *authz.Subject, sc authz.Scope) (any, error) {
		q, err := parseQuery(r.URL.Query())
		if err != nil {
			return nil, err
		}
		return s.list(r.Context(), sub, sc, q)
	}))
	r.HandleFunc("POST /v0/issues", core.Authed, "write (not guest) on the resolved project/area (WriteTarget) and the issue's area", s.jsonH(func(r *http.Request, sub *authz.Subject, _ authz.Scope) (any, error) {
		var req orchapi.NewIssueRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.create(r.Context(), sub, req)
	}))
	r.HandleFunc("GET /v0/issues/stats", core.Authed, "read: counts over issues in ReadScope the caller reads", s.jsonH(func(r *http.Request, _ *authz.Subject, sc authz.Scope) (any, error) {
		return s.stats(r.Context(), sc)
	}))
	r.HandleFunc("GET /v0/issues/labels", core.Authed, "read: labels on issues in ReadScope(?project=) the caller reads", s.jsonH(func(r *http.Request, _ *authz.Subject, sc authz.Scope) (any, error) {
		var areas []string
		if a, ok := r.URL.Query()["area"]; ok {
			areas = append([]string{}, a...)
		}
		ls, err := s.labels(r.Context(), sc, areas)
		return map[string]any{"labels": ls}, err
	}))
	r.HandleFunc("GET /v0/issues/comments", core.Authed, "read: comments on issues in ReadScope(?project=) the caller reads; author, prefix, contains, after, limit", s.jsonH(func(r *http.Request, sub *authz.Subject, sc authz.Scope) (any, error) {
		v := r.URL.Query()
		q := orchapi.CommentQuery{Author: v.Get("author"), Prefix: v.Get("prefix"), Contains: v.Get("contains"), After: v.Get("after")}
		if n := v.Get("limit"); n != "" {
			var err error
			if q.Limit, err = strconv.Atoi(n); err != nil || q.Limit < 0 {
				return nil, api.Errorf(api.CodeBadRequest, "bad limit %q", n)
			}
		}
		return s.searchComments(r.Context(), sub, sc, q)
	}))
	r.HandleFunc("GET /v0/issues/projects", core.Authed, "read: projects in ReadScope with native issues", s.jsonH(func(r *http.Request, _ *authz.Subject, sc authz.Scope) (any, error) {
		return s.projects(r.Context(), sc)
	}))
	r.HandleFunc("GET /v0/issues/memories", core.Authed, "read on every area of the one project in ReadScope(?project=) with native issues", s.jsonH(func(r *http.Request, _ *authz.Subject, sc authz.Scope) (any, error) {
		m, err := s.memories(r.Context(), sc)
		return orchapi.MemoryList{Memories: m}, err
	}))
	r.HandleFunc("PUT /v0/issues/memories/{key}", core.Authed, "write (not guest) on the whole resolved project (WriteTarget)", s.jsonH(func(r *http.Request, sub *authz.Subject, _ authz.Scope) (any, error) {
		var req orchapi.Memory
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.remember(r.Context(), sub, r.PathValue("key"), req.Value)
	}))
	r.HandleFunc("DELETE /v0/issues/memories/{key}", core.Authed, "write (not guest) on the whole resolved project (WriteTarget); not_found when there is none", s.jsonH(func(r *http.Request, sub *authz.Subject, _ authz.Scope) (any, error) {
		return s.forget(r.Context(), sub, r.PathValue("key"))
	}))
	r.HandleFunc("POST /v0/issues/sql", core.Authed, "read on every area of the one project in ReadScope(?project=) with native issues; one read-only SELECT", s.jsonH(func(r *http.Request, sub *authz.Subject, sc authz.Scope) (any, error) {
		var req orchapi.SQLRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.readSQL(r.Context(), sub, sc, req.Query)
	}))
	r.HandleFunc("POST /v0/issues/setup", core.Authed, "admin on the resolved project (WriteTarget); prefix only while it has no issues", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		var req struct {
			Prefix string `json:"prefix"`
		}
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.setup(r.Context(), req.Prefix)
	}))
	r.HandleFunc("POST /v0/issues/move", core.Authed, "admin on the resolved project (WriteTarget) and on project to; dry_run plans; claimed issues only with transfer_claims", s.jsonH(func(r *http.Request, sub *authz.Subject, _ authz.Scope) (any, error) {
		var req orchapi.IssueMoveRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.move(r.Context(), sub, req)
	}))
	r.HandleFunc("GET /v0/issues/{id}", core.Authed, "read on the issue's area, else not_found; edges to unreadable issues left out", s.itemH(func(r *http.Request, sub *authz.Subject, l located) (any, error) {
		return s.get(r.Context(), sub, l)
	}))
	r.HandleFunc("PATCH /v0/issues/{id}", core.Authed, "write (not guest) on the issue's area, and on the area a label change moves it to", s.writeH(func(r *http.Request) (istore.Mutation, error) {
		var req orchapi.UpdateIssueRequest
		err := api.DecodeJSON(r, &req)
		return istore.Mutation{Op: istore.OpUpdate, Update: &req}, err
	}))
	r.HandleFunc("POST /v0/issues/{id}/claim", core.Authed, "write (not guest) on the issue's area; conflict when someone else holds it; a lease when the project binds native issues", s.itemH(func(r *http.Request, sub *authz.Subject, l located) (any, error) {
		var req orchapi.ClaimIssueRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		label, err := claimLabel(req.Assignee)
		if err != nil {
			return nil, err
		}
		if err := s.labelHeld(r.Context(), sub, l, label); err != nil {
			return nil, err
		}
		if res, ok, err := s.leaseClaim(r.Context(), sub, l, req, label); ok {
			return res, err
		}
		if req.Fence != 0 { // no native work source: no lease to renew
			return nil, orchapi.StaleLease(l.id, req.Fence)
		}
		res, view, err := s.write(r.Context(), sub, l, istore.Mutation{Op: istore.OpClaim, Assignee: label})
		if err != nil {
			return nil, err
		}
		return writeResult{Issue: view, Noop: res.Noop}, nil
	}))
	r.HandleFunc("POST /v0/issues/{id}/close", core.Authed, "write (not guest) on the issue's area", s.writeH(func(r *http.Request) (istore.Mutation, error) {
		var req orchapi.CloseIssueRequest
		err := api.DecodeJSON(r, &req)
		return istore.Mutation{Op: istore.OpClose, Close: &req}, err
	}))
	r.HandleFunc("POST /v0/issues/{id}/reopen", core.Authed, "write (not guest) on the issue's area", s.writeH(func(r *http.Request) (istore.Mutation, error) {
		var req orchapi.ReopenIssueRequest
		err := api.DecodeJSON(r, &req)
		return istore.Mutation{Op: istore.OpReopen, Reopen: &req}, err
	}))
	r.HandleFunc("GET /v0/issues/{id}/comments", core.Authed, "read on the issue's area", s.itemH(func(r *http.Request, sub *authz.Subject, l located) (any, error) {
		cs, err := l.ps.Comments(r.Context(), l.id, l.vis)
		if err != nil {
			return nil, s.storeErr(err, l.id)
		}
		it, err := l.ps.Get(r.Context(), l.id, l.vis)
		if err != nil {
			return nil, s.storeErr(err, l.id)
		}
		for i := range cs {
			s.showComment(r.Context(), sub, l.project, it.Area, &cs[i])
		}
		return map[string]any{"comments": cs}, nil
	}))
	r.HandleFunc("PATCH /v0/issues/{id}/comments/{cid}", core.Authed, "the comment's author (any session of its identity), with write on the issue's area; base refuses an edit over an unseen change", s.itemH(func(r *http.Request, sub *authz.Subject, l located) (any, error) {
		var req orchapi.EditCommentRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		res, _, err := s.write(r.Context(), sub, l, istore.Mutation{Op: istore.OpCommentEdit,
			CommentEdit: &istore.CommentEdit{ID: r.PathValue("cid"), Text: req.Text, Base: req.Base}})
		if err != nil {
			return nil, err
		}
		return s.commentAnswer(r.Context(), sub, l, res.Comment)
	}))
	r.HandleFunc("POST /v0/issues/{id}/comments/{cid}/redact", core.Authed, "an admin of the issue's area, or the machine owner on the socket: the comment's text, its edits' texts and every other copy are overwritten; cannot be undone", s.itemH(func(r *http.Request, sub *authz.Subject, l located) (any, error) {
		return s.redact(r.Context(), sub, l, r.PathValue("cid"))
	}))
	r.HandleFunc("POST /v0/issues/{id}/comments/{cid}/ack-gap", core.Authed, "whoever may redact the comment (an admin of the issue's area, or the machine owner on the socket): {stop} as the caller's own redaction conflict names it; a stop in a project the caller cannot read is not_found; clears only that gap record and logs who and when", s.itemH(func(r *http.Request, sub *authz.Subject, l located) (any, error) {
		var req orchapi.AckRedactionGapRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.ackGap(r.Context(), sub, l, r.PathValue("cid"), req.Stop)
	}))
	r.HandleFunc("DELETE /v0/issues/{id}/comments/{cid}", core.Authed, "the comment's author, or an admin of the issue's area, or the machine owner on the socket; leaves a tombstone", s.itemH(func(r *http.Request, sub *authz.Subject, l located) (any, error) {
		res, _, err := s.write(r.Context(), sub, l, istore.Mutation{Op: istore.OpCommentDelete, CommentID: r.PathValue("cid")})
		if err != nil {
			return nil, err
		}
		return s.commentAnswer(r.Context(), sub, l, res.Comment)
	}))
	r.HandleFunc("POST /v0/issues/{id}/comments", core.Authed, "write (not guest) on the issue's area; author is the caller", s.itemH(func(r *http.Request, sub *authz.Subject, l located) (any, error) {
		var req orchapi.IssueCommentRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		res, _, err := s.write(r.Context(), sub, l, istore.Mutation{Op: istore.OpComment, Comment: &req})
		if err != nil {
			return nil, err
		}
		return res.Comment, nil
	}))
	r.HandleFunc("GET /v0/issues/{id}/history", core.Authed, "read on the issue's area; actor addresses redacted", s.itemH(func(r *http.Request, sub *authz.Subject, l located) (any, error) {
		hs, err := l.ps.History(r.Context(), l.id, l.vis)
		if err != nil {
			return nil, s.storeErr(err, l.id)
		}
		if it, err := l.ps.Get(r.Context(), l.id, l.vis); err == nil {
			s.showHistory(r.Context(), sub, l, it.Area, hs)
		}
		for i := range hs {
			s.redactRefs(r.Context(), l, &hs[i])
			if hs[i].ActorAddress != "" {
				hs[i].ActorAddress = s.d.RedactFor(r.Context(), sub, hs[i].ActorAddress)
			}
			// An area the caller cannot read is not named.
			for _, a := range []*string{&hs[i].AreaBefore, &hs[i].AreaAfter} {
				if !l.vis.Has(*a) {
					*a = events.Someone
				}
			}
		}
		return map[string]any{"history": hs}, nil
	}))
	r.HandleFunc("POST /v0/issues/{id}/deps", core.Authed, "write (not guest) on the issue's area; a target the caller cannot read is not_found", s.writeH(func(r *http.Request) (istore.Mutation, error) {
		var req orchapi.IssueDepRequest
		err := api.DecodeJSON(r, &req)
		return istore.Mutation{Op: istore.OpDepAdd, Dep: &req}, err
	}))
	r.HandleFunc("DELETE /v0/issues/{id}/deps/{target}", core.Authed, "write (not guest) on the issue's area; a target the caller cannot read is not_found", s.writeH(func(r *http.Request) (istore.Mutation, error) {
		return istore.Mutation{Op: istore.OpDepRemove, Dep: &orchapi.IssueDepRequest{Target: r.PathValue("target")}}, nil
	}))
	r.HandleFunc("POST /v0/issues/{id}/labels", core.Authed, "write (not guest) on the issue's area and on the area the label moves it to", s.writeH(func(r *http.Request) (istore.Mutation, error) {
		var req orchapi.IssueLabelRequest
		err := api.DecodeJSON(r, &req)
		return istore.Mutation{Op: istore.OpLabelAdd, Label: req.Label}, err
	}))
	r.HandleFunc("DELETE /v0/issues/{id}/labels/{label}", core.Authed, "write (not guest) on the issue's area and on the area the removal moves it to", s.writeH(func(r *http.Request) (istore.Mutation, error) {
		return istore.Mutation{Op: istore.OpLabelRemove, Label: r.PathValue("label")}, nil
	}))
	s.attachmentRoutes(r)
}

type handlerFunc func(r *http.Request, sub *authz.Subject, sc authz.Scope) (any, error)

func (s *Service) jsonH(fn handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sub := core.SubjectOf(r.Context())
		sc, err := s.d.ReadScope(r.Context(), r.URL.Query().Get("project"))
		if err != nil {
			api.WriteError(w, err)
			return
		}
		v, err := fn(r, sub, sc)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, v)
	}
}

// itemH locates {id} in the caller's read scope before anything else (an id
// that moved: the issue it became): a hidden or missing id is the same
// not_found, whatever the route.
func (s *Service) itemH(fn func(r *http.Request, sub *authz.Subject, l located) (any, error)) http.HandlerFunc {
	return s.jsonH(func(r *http.Request, sub *authz.Subject, sc authz.Scope) (any, error) {
		l, err := s.locateFor(r.Context(), sub, sc, r.PathValue("id"))
		if err != nil {
			return nil, err
		}
		return fn(r, sub, l)
	})
}

// writeH is a write on {id}: the issue as the caller sees it afterwards,
// with noop set when nothing changed.
func (s *Service) writeH(build func(r *http.Request) (istore.Mutation, error)) http.HandlerFunc {
	return s.itemH(func(r *http.Request, sub *authz.Subject, l located) (any, error) {
		m, err := build(r)
		if err != nil {
			return nil, err
		}
		res, view, err := s.write(r.Context(), sub, l, m)
		if err != nil {
			return nil, err
		}
		return writeResult{Issue: view, Noop: res.Noop}, nil
	})
}

type writeResult struct {
	orchapi.Issue
	Noop bool `json:"noop,omitempty"`
	// LeaseFence is a lease-backed claim's fence (orchapi.Lease.Fence): the
	// claimer sends it back on writes under the claim.
	LeaseFence int64 `json:"lease_fence,omitempty"`
}

func parseQuery(v url.Values) (orchapi.IssueQuery, error) {
	q := orchapi.IssueQuery{
		Project: v.Get("project"), Status: v.Get("status"), Type: v.Get("type"), Assignee: v.Get("assignee"),
		Parent: v.Get("parent"), TitleContains: v.Get("title_contains"), DescContains: v.Get("desc_contains"),
		NotesContains: v.Get("notes_contains"), Q: v.Get("q"), Substr: v.Get("substr"), Sort: v.Get("sort"),
		Contains: v.Get("contains"), ClaimedBy: v.Get("claimed_by"), After: v.Get("after"), IDAfter: v.Get("id_after"),
	}
	if a, ok := v["area"]; ok {
		q.Areas = append([]string{}, a...)
	}
	multi := func(k string) []string {
		var out []string
		for _, x := range v[k] {
			for _, y := range strings.Split(x, ",") {
				if y = strings.TrimSpace(y); y != "" {
					out = append(out, y)
				}
			}
		}
		return out
	}
	q.Labels, q.LabelAny, q.IDs = v["label"], v["label_any"], multi("id")
	flag := func(k string) bool { b, _ := strconv.ParseBool(v.Get(k)); return b }
	q.Ready, q.Reverse, q.All, q.NoAssignee, q.Deps, q.Count, q.Blocked = flag("ready"), flag("reverse"), flag("all"), flag("no_assignee"), flag("deps"), flag("count"), flag("blocked")
	q.Incoming, q.Comments = flag("incoming"), flag("comments")
	if p := v.Get("priority"); p != "" {
		n, err := strconv.Atoi(strings.TrimPrefix(strings.ToUpper(p), "P"))
		if err != nil || n < 0 || n > 4 {
			return q, api.Errorf(api.CodeBadRequest, "invalid priority %q (expected 0-4 or P0-P4)", p)
		}
		q.Priority = &n
	}
	if n := v.Get("number"); n != "" {
		x, err := strconv.Atoi(n)
		if err != nil || x < 1 {
			return q, api.Errorf(api.CodeBadRequest, "bad number %q", n)
		}
		q.Number = &x
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 0 {
			return q, api.Errorf(api.CodeBadRequest, "bad limit %q", l)
		}
		q.Limit = n
	}
	for k, dst := range map[string]**time.Time{"updated_after": &q.UpdatedAfter, "updated_before": &q.UpdatedBefore,
		"created_after": &q.CreatedAfter, "created_before": &q.CreatedBefore} {
		if u := v.Get(k); u != "" {
			t, err := time.Parse(time.RFC3339Nano, u)
			if err != nil {
				return q, api.Errorf(api.CodeBadRequest, "%s is RFC 3339", k)
			}
			*dst = &t
		}
	}
	switch q.Sort {
	case "", "priority", "created", "updated", "closed", "id", "number", "title", "status", "type", "assignee", "oldest", "rank":
	default:
		return q, api.Errorf(api.CodeBadRequest, "invalid sort %q", q.Sort)
	}
	for _, st := range strings.Split(q.Status, ",") {
		if st = strings.TrimSpace(st); st != "" && st != "all" && !orchapi.ValidIssueStatus(st) {
			return q, api.Errorf(api.CodeBadRequest, "invalid status %q", st)
		}
	}
	return q, nil
}
