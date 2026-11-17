package webhooks

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

// Routes registers /v0/webhooks.
func (s *Service) Routes(r *core.Router) {
	r = r.For("webhooks")
	r.HandleFunc("GET /v0/webhooks", core.Authed, "admin on the hook's project/area (hooks the machine owner made on its socket: that owner only); ?project= like ReadScope", s.h(s.handleList))
	r.HandleFunc("POST /v0/webhooks", core.Authed, "admin on the named project/area; no project and allow_private: machine owner on the socket only", s.h(s.handleCreate))
	r.HandleFunc("GET /v0/webhooks/{id}", core.Authed, "admin on the hook, else not_found", s.h(s.handleGet))
	r.HandleFunc("PATCH /v0/webhooks/{id}", core.Authed, "admin on the hook, else not_found", s.h(s.handleUpdate))
	r.HandleFunc("DELETE /v0/webhooks/{id}", core.Authed, "admin on the hook, else not_found", s.h(s.handleDelete))
	r.HandleFunc("POST /v0/webhooks/{id}/test", core.Authed, "admin on the hook, else not_found; sends one signed ping now", s.h(s.handleTest))
}

func (s *Service) h(fn func(r *http.Request, sub *authz.Subject) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := fn(r, core.SubjectOf(r.Context()))
		if err != nil {
			api.WriteError(w, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, v)
	}
}

// machineOwner is the OS user on the socket without a token.
func machineOwner(sub *authz.Subject) bool {
	return sub != nil && sub.Via == authz.ViaLocal && !sub.Remote && sub.TokenID == ""
}

// administers reports whether sub may see and change h.
func administers(sub *authz.Subject, h hook) bool {
	if machineOwner(sub) {
		return true
	}
	if h.Project == "" || h.CreatorAll {
		return false // the machine owner's hooks (every allow-private one among them) are machine settings
	}
	return sub.Can(authz.Admin, authz.Object{Project: h.Project, Area: h.Area})
}

// visible loads hook id for sub; hidden is exactly missing.
func (s *Service) visible(ctx context.Context, sub *authz.Subject, id string) (hook, error) {
	key := strings.ToUpper(strings.TrimSpace(id))
	if !ids.Valid(key) {
		return hook{}, api.NotFound("webhook", id)
	}
	h, err := s.load(ctx, key)
	if err != nil || !administers(sub, h) {
		if err != nil && !api.IsCode(err, api.CodeNotFound) {
			return hook{}, err
		}
		return hook{}, api.NotFound("webhook", id)
	}
	return h, nil
}

func (s *Service) handleList(r *http.Request, sub *authz.Subject) (any, error) {
	ctx := r.Context()
	explicit := r.URL.Query().Get("project")
	if in := core.ResolveInputOf(ctx); explicit == "" && in.ExplicitFrom == project.RuleFlag {
		explicit = in.Explicit
	}
	only, onlyArea := "", ""
	if explicit != "" {
		if _, err := s.d.ReadScope(ctx, explicit); err != nil {
			return nil, err
		}
		ref, area, _ := strings.Cut(explicit, "/")
		onlyArea = area
		if ref != api.ProjectNone {
			p, err := s.d.Projects.Get(sub.Scope, ref)
			if err != nil {
				return nil, err
			}
			only = p.ID
		}
	}
	hooks, err := s.list(ctx)
	if err != nil {
		return nil, err
	}
	out := orchapi.WebhookList{Webhooks: []orchapi.Webhook{}}
	for _, h := range hooks {
		if !administers(sub, h) || (explicit != "" && h.Project != only) || (onlyArea != "" && h.Area != onlyArea) {
			continue
		}
		out.Webhooks = append(out.Webhooks, s.view(ctx, sub, h))
	}
	return out, nil
}

func (s *Service) handleCreate(r *http.Request, sub *authz.Subject) (any, error) {
	ctx := r.Context()
	var req orchapi.NewWebhookRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	owner := machineOwner(sub)
	h := hook{ID: ids.New(), Note: strings.TrimSpace(req.Note), CreatorAddr: sub.Address, State: orchapi.WebhookActive}
	if len(h.Note) > 200 {
		return nil, api.Errorf(api.CodeBadRequest, "note too long (200 bytes max)")
	}
	if ref := strings.TrimSpace(req.Project); ref != "" {
		slug, area, _ := strings.Cut(ref, "/")
		p, err := s.d.Projects.Get(sub.Scope, slug)
		if err != nil {
			return nil, err
		}
		if area != "" && (!s.d.Projects.HasArea(p.ID, area) || !sub.Can(authz.Read, authz.Object{Project: p.ID, Area: area})) {
			return nil, api.NotFound("area", area)
		}
		if err := sub.Check(authz.Admin, authz.Object{Project: p.ID, Area: area}, "project", ref); err != nil {
			return nil, err
		}
		h.Project, h.Area = p.ID, area
	} else if !owner {
		return nil, api.Errorf(api.CodeBadRequest, "a webhook needs a project").WithHint("pass --project <slug>[/<area>]")
	}
	if req.AllowPrivate && !owner {
		return nil, api.Errorf(api.CodeForbidden, "only the machine owner on its socket may let a webhook reach private addresses")
	}
	u, err := CheckURL(req.URL, req.AllowPrivate)
	if err != nil {
		return nil, err
	}
	h.URL, h.AllowPrivate = u.String(), req.AllowPrivate
	if h.Events, err = NormalizeEvents(req.Events); err != nil {
		return nil, err
	}
	made := ""
	switch {
	case req.Secret == "":
		made = NewSecret()
		h.Secret = made
	case len(req.Secret) < MinSecret || len(req.Secret) > 512:
		return nil, api.Errorf(api.CodeBadRequest, "secret must be %d to 512 bytes", MinSecret)
	default:
		h.Secret = req.Secret
	}
	if owner {
		h.CreatorAll = true
	} else {
		h.CreatorID, h.Token = sub.Identity.ID, sub.TokenID
		if sub.Cap != nil {
			h.CapProject, h.CapArea, h.CapRole = sub.Cap.Project, sub.Cap.Area, string(sub.Cap.Role)
		}
		if h.CreatorID == "" {
			return nil, api.Errorf(api.CodeForbidden, "this caller has no identity to create a webhook as")
		}
	}
	now := s.now()
	h.Created = store.Millis(now)
	h.Cursor = ids.New() // every event published from now on, none before
	evs, _ := json.Marshal(nonNil(h.Events))
	err = store.Tx(ctx, s.db(), func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM webhooks`).Scan(&n); err != nil {
			return err
		}
		if n >= maxHooks {
			return api.Errorf(api.CodeConflict, "this daemon already has %d webhooks", n)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO webhooks (id, url, project, area, events, secret, allow_private, creator_id,
			creator_address, creator_all, token, cap_project, cap_area, cap_role, note, created, cursor)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, h.ID, h.URL, h.Project, h.Area, string(evs), h.Secret,
			b2i(h.AllowPrivate), h.CreatorID, h.CreatorAddr, b2i(h.CreatorAll), h.Token, h.CapProject, h.CapArea, h.CapRole, h.Note,
			h.Created, h.Cursor)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.startFollower(h.ID)
	return orchapi.NewWebhookResponse{Webhook: s.view(ctx, sub, h), Secret: made}, nil
}

func (s *Service) handleGet(r *http.Request, sub *authz.Subject) (any, error) {
	ctx := r.Context()
	h, err := s.visible(ctx, sub, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = min(n, 500)
		}
	}
	rows, err := s.db().QueryContext(ctx, `SELECT delivery, event_id, event_type, attempt, time, status, outcome, error, duration_ms
		FROM webhook_log WHERE hook = ? ORDER BY time DESC, rowid DESC LIMIT ?`, h.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := orchapi.WebhookDetail{Webhook: s.view(ctx, sub, h), Deliveries: []orchapi.WebhookDelivery{}}
	for rows.Next() {
		var d orchapi.WebhookDelivery
		var t int64
		if err := rows.Scan(&d.Delivery, &d.Event, &d.Type, &d.Attempt, &t, &d.Status, &d.Outcome, &d.Error, &d.DurationMS); err != nil {
			return nil, err
		}
		d.Time = store.FromMillis(t)
		out.Deliveries = append(out.Deliveries, d)
	}
	return out, rows.Err()
}

func (s *Service) handleUpdate(r *http.Request, sub *authz.Subject) (any, error) {
	ctx := r.Context()
	h, err := s.visible(ctx, sub, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	var req orchapi.UpdateWebhookRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	if req.Enabled != nil {
		if *req.Enabled && h.State != orchapi.WebhookActive {
			// Back on: from now (the events published while it was off are
			// skipped; what was queued before is still delivered).
			if _, err := s.db().ExecContext(ctx, `UPDATE webhooks SET state = 'active', disabled_reason = '', fail_since = NULL,
				fail_count = 0, last_error = '', cursor = ? WHERE id = ?`, ids.New(), h.ID); err != nil {
				return nil, err
			}
			s.startFollower(h.ID)
			s.poke()
		} else if !*req.Enabled && h.State == orchapi.WebhookActive {
			if _, err := s.db().ExecContext(ctx, `UPDATE webhooks SET state = 'disabled', disabled_reason = ? WHERE id = ?`,
				"disabled by "+disabler(sub), h.ID); err != nil {
				return nil, err
			}
			s.stopFollower(h.ID)
		}
	}
	if h, err = s.load(ctx, h.ID); err != nil {
		return nil, err
	}
	return s.view(ctx, sub, h), nil
}

func (s *Service) handleDelete(r *http.Request, sub *authz.Subject) (any, error) {
	ctx := r.Context()
	h, err := s.visible(ctx, sub, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	s.stopFollower(h.ID)
	v := s.view(ctx, sub, h)
	err = store.Tx(ctx, s.db(), func(tx *sql.Tx) error {
		for _, q := range []string{`DELETE FROM webhooks WHERE id = ?`, `DELETE FROM webhook_queue WHERE hook = ?`, `DELETE FROM webhook_log WHERE hook = ?`} {
			if _, err := tx.ExecContext(ctx, q, h.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return v, err
}

// handleTest sends one signed ping now (not queued, never retried), logged
// like any attempt. It works on a disabled hook too, to check a fix.
func (s *Service) handleTest(r *http.Request, sub *authz.Subject) (any, error) {
	ctx := r.Context()
	h, err := s.visible(ctx, sub, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	project := h.Project
	if project == "" {
		project = api.ProjectNone
	}
	data, _ := json.Marshal(orchapi.WebhookPing{Webhook: h.ID, By: sub.Address})
	ev := api.Event{SpecVersion: "1.0", ID: ids.New(), Source: s.d.Events.Source(), Type: orchapi.EventWebhookPing, Subject: h.ID,
		Time: s.now().Truncate(time.Millisecond), DataContentType: "application/json", Data: data, Project: project, Area: h.Area}
	body, _ := json.Marshal(ev)
	rec := orchapi.WebhookDelivery{Delivery: ids.New(), Event: ev.ID, Type: ev.Type, Attempt: 1, Time: s.now(), Outcome: "ping"}
	rec.Status, rec.Error = s.post(ctx, h, rec.Delivery, ev.Type, body)
	rec.DurationMS = time.Since(rec.Time).Milliseconds()
	_ = insertLog(ctx, s.db(), h.ID, rec)
	s.prune(ctx, h.ID)
	return orchapi.WebhookTestResponse{OK: rec.Error == "", Delivery: rec}, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

// disabler names who disabled a hook by its identity, never a session
// address (other admins may not see the session).
func disabler(sub *authz.Subject) string {
	if sub.Identity.Name == "" {
		return "an admin"
	}
	if sub.Identity.Kind == api.IdentityMachine {
		return "machine:" + sub.Identity.Name
	}
	return "human:" + sub.Identity.Name
}
