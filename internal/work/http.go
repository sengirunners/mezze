package work

import (
	"net/http"
	"strconv"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/work/sessiondelivery"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// Routes registers work and lease routes; client.Seg escapes item ids.
func (s *Service) Routes(r *core.Router) {
	r = r.For("work")
	s.trackerRoutes(r)
	r.HandleFunc("GET /v0/work/sources", core.Authed, "read: trackers of projects in ReadScope (adapter paths only to local callers)", s.jsonH(func(r *http.Request, sub *authz.Subject, sc authz.Scope) (any, error) {
		return orchapi.WorkSourceList{Sources: s.sources(r.Context(), sub, sc)}, nil
	}))
	r.HandleFunc("GET /v0/work", core.Authed, "read: ReadScope(?project=), rows by AreaSQL; adapter items by AreaForWork", s.jsonH(s.handleList))
	r.HandleFunc("GET /v0/work/page", core.Authed, "read: ReadScope(?project=), rows by AreaSQL like GET /v0/work; in= entries hidden or missing match nothing; epic= walks rows in sight only", s.jsonH(s.handlePage))
	r.HandleFunc("GET /v0/work/labels", core.Authed, "read: labels of items in ReadScope(?project=), rows by AreaSQL; in= entries hidden or missing match nothing", s.jsonH(func(r *http.Request, sub *authz.Subject, sc authz.Scope) (any, error) {
		return s.labels(r.Context(), sub, sc, r.URL.Query()["in"])
	}))
	r.HandleFunc("GET /v0/work/rollup", core.Authed, "read: ReadScope(?project=), rows by AreaSQL like GET /v0/work; an epic out of sight is not named and its visible items count under no_epic; hidden holders redacted", s.jsonH(func(r *http.Request, sub *authz.Subject, sc authz.Scope) (any, error) {
		return s.rollup(r.Context(), sub, sc, r.URL.Query().Get("area"))
	}))
	r.HandleFunc("POST /v0/work", core.Authed, "write on the resolved project/area (WriteTarget)", s.jsonH(func(r *http.Request, sub *authz.Subject, _ authz.Scope) (any, error) {
		var req orchapi.CreateWorkRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.create(r.Context(), sub, req)
	}))
	r.HandleFunc("GET /v0/work/inflight", core.Authed, "read: leases x items x sessions in ReadScope; hidden holders redacted", s.jsonH(func(r *http.Request, sub *authz.Subject, sc authz.Scope) (any, error) {
		rows, err := s.inflight(r.Context(), sub, sc)
		return orchapi.InflightList{Inflight: rows}, err
	}))
	r.HandleFunc("GET /v0/work/{id}", core.Authed, "read on the item's project/area, else not_found", s.readH(func(r *http.Request, sub *authz.Subject, _ authz.Scope, l loc) (any, error) {
		return s.get(r.Context(), sub, l)
	}))
	r.HandleFunc("GET /v0/work/{id}/holder", core.Authed, "read on the item; holder redacted", s.readH(s.handleHolder))
	r.HandleFunc("GET /v0/work/{id}/comments", core.Authed, "read on the item", s.readH(func(r *http.Request, sub *authz.Subject, _ authz.Scope, l loc) (any, error) {
		cs, err := s.comments(r.Context(), sub, l)
		return orchapi.CommentList{Comments: cs}, err
	}))
	r.HandleFunc("POST /v0/work/{id}/claim", core.Authed, "write on the item; conflict data = the lease, holder redacted", s.itemH(func(r *http.Request, sub *authz.Subject, _ authz.Scope, l loc) (any, error) {
		var req orchapi.ClaimRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.claimItem(r.Context(), sub, l, req, "")
	}))
	r.HandleFunc("POST /v0/work/{id}/release", core.Authed, "write on the item and holder == caller; force: operate, local callers only", s.itemH(func(r *http.Request, sub *authz.Subject, _ authz.Scope, l loc) (any, error) {
		var req orchapi.ReleaseRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.releaseItem(r.Context(), sub, l, req.Force, req.Fence)
	}))
	r.HandleFunc("POST /v0/work/{id}/comment", core.Authed, "write on the item", s.itemH(func(r *http.Request, sub *authz.Subject, _ authz.Scope, l loc) (any, error) {
		var req orchapi.CommentRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.comment(r.Context(), sub, l, req.Body, core.WriteID(r), req.Fence)
	}))
	r.HandleFunc("POST /v0/work/{id}/status", core.Authed, "write on the item", s.itemH(func(r *http.Request, sub *authz.Subject, _ authz.Scope, l loc) (any, error) {
		var req orchapi.StatusRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.setStatus(r.Context(), sub, l, req.Status, core.BaseOf(r), req.Fence)
	}))
	r.HandleFunc("POST /v0/work/{id}/link", core.Authed, "write on the item; a target the caller cannot read is not_found", s.itemH(func(r *http.Request, sub *authz.Subject, _ authz.Scope, l loc) (any, error) {
		var req orchapi.LinkRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		// Link targets may be in any project the caller reads (--project only
		// narrows where {id} is looked up).
		return s.link(r.Context(), sub, sub.Scope, l, req)
	}))
	r.HandleFunc("GET /v0/leases", core.Authed, "read: leases in ReadScope; holders redacted", s.jsonH(func(r *http.Request, sub *authz.Subject, sc authz.Scope) (any, error) {
		ls, err := s.leases(r.Context(), sub, sc, r.URL.Query().Get("holder"))
		return orchapi.LeaseList{Leases: ls}, err
	}))
	r.HandleFunc("POST /v0/leases", core.Authed, "write on the resolved project/area", s.jsonH(func(r *http.Request, sub *authz.Subject, _ authz.Scope) (any, error) {
		var req orchapi.ClaimRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.claimKey(r.Context(), sub, req)
	}))
	r.HandleFunc("DELETE /v0/leases/{key}", core.Authed, "write; holder == caller, or force (operate, local)", s.jsonH(func(r *http.Request, sub *authz.Subject, sc authz.Scope) (any, error) {
		var req orchapi.ReleaseRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.releaseKey(r.Context(), sub, sc, r.PathValue("key"), req.Force, req.Fence)
	}))
}

type handlerFunc func(r *http.Request, sub *authz.Subject, sc authz.Scope) (any, error)

// jsonH runs fn with the caller and its read scope (?project= or --project
// narrows it; a hidden project answers like a missing one) and writes JSON.
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
		_ = sessiondelivery.WriteJSON(w, r, orchcore.Sessions(s.d), v) // writes its own error
	}
}

// itemH locates {id} in the caller's read scope first: every item route
// answers a hidden or missing id with the same not_found before anything else.
func (s *Service) itemH(fn func(r *http.Request, sub *authz.Subject, sc authz.Scope, l loc) (any, error)) http.HandlerFunc {
	return s.jsonH(func(r *http.Request, sub *authz.Subject, sc authz.Scope) (any, error) {
		l, err := s.locate(r.Context(), sub, sc, r.PathValue("id"))
		if err != nil {
			return nil, err
		}
		return fn(r, sub, sc, l)
	})
}

func (s *Service) handleList(r *http.Request, sub *authz.Subject, sc authz.Scope) (any, error) {
	v := r.URL.Query()
	limit := 0
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 0 {
			return nil, api.Errorf(api.CodeBadRequest, "bad limit %q", l)
		}
		limit = n
	}
	return s.list(r.Context(), sub, sc, orchapi.WorkQuery{Area: v.Get("area"), Source: v.Get("source"),
		Status: orchapi.WorkStatus(v.Get("status")), Owner: v.Get("owner"), Q: textclean.Clean(v.Get("q")), Limit: limit})
}

func (s *Service) handleHolder(r *http.Request, sub *authz.Subject, _ authz.Scope, l loc) (any, error) {
	ctx := r.Context()
	h := orchapi.HolderResponse{ID: l.id}
	cur, err := s.liveLease(ctx, l.project, l.area, l.id)
	if err != nil {
		return nil, err
	}
	if cur != nil {
		v := s.leaseView(ctx, sub, *cur)
		h.Holder, h.Lease = v.Holder, &v
		// The holding session, when the caller sees it: its state and, for a
		// session on a team replica, its presence (SPEC 17.3).
		if orchcore.Sessions(s.d) != nil && v.Holder == cur.Holder && s.d.Visible(ctx, sub.Scope, cur.Holder) {
			if ss, ok := orchcore.Sessions(s.d).Lookup(ctx, cur.Holder); ok {
				h.Session = sessiondelivery.HolderView(ss)
			}
		}
	}
	to, via, err := s.mailTarget(ctx, sub, l)
	switch {
	case err == nil:
		h.MailTo, h.Via = s.d.Redact(ctx, sub, to), via
	case !api.IsCode(err, api.CodeConflict):
		return nil, err
	}
	if cur == nil && !l.local() {
		if it, err := s.load(ctx, sub, l); err == nil && it.Owner != "" {
			h.TrackerOwner = s.d.Redact(ctx, sub, it.Owner)
		}
	}
	return h, nil
}
