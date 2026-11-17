package door

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// Routes: SPEC 18.4.
func (s *Service) Routes(r *core.Router) {
	r = r.For("door")
	const who = "a machine's own daemon: its owner on the socket (a member's is sent on to the project's authority) or, at the authority, a replica's node-bound token; "
	r.HandleFunc("POST "+hookPrefix+"{token}", core.Public, "the task's URL token (32 random bytes) is the credential; a missing, rotated or switched-off one is 404; answers nothing scoped", s.hook)
	r.HandleFunc("GET /v0/door", core.Authed, who+"read on the whole team project; the caller's machine only", s.handle(s.status))
	r.HandleFunc("PUT /v0/door", core.Authed, who+"operate on the whole team project; the caller's machine only", s.handle(s.toggle))
	r.HandleFunc("GET /v0/door/tasks", core.Authed, who+"read on the whole team project; the caller's machine's tasks only", s.handle(s.tasks))
	r.HandleFunc("PUT /v0/door/tasks/{task}", core.Authed, who+"operate on the whole team project; registers a task of the caller's machine only, its target loopback", s.handle(s.putTask))
	r.HandleFunc("DELETE /v0/door/tasks/{task}", core.Authed, who+"operate on the whole team project; the caller's machine's task only", s.handle(s.removeTask))
	r.HandleFunc("POST /v0/door/tasks/{task}/rotate", core.Authed, who+"operate on the whole team project; the caller's machine's task only", s.handle(s.rotate))
	r.HandleFunc("GET /v0/door/tasks/{task}/deliveries", core.Authed, who+"read on the whole team project; the caller's machine's task only; never a body", s.handle(s.deliveries))
}

func (s *Service) handle(f func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := f(r)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, v)
	}
}

// caller is the team project (by ?project=) and the caller's machine: a
// replica's node-bound token names its machine, the socket this one.
func (s *Service) caller(ctx context.Context, raw string, action authz.Action) (api.Project, string, error) {
	sub := core.SubjectOf(ctx)
	var machine string
	switch {
	case sub != nil && sub.Node != "" && sub.Machine != "":
		machine = sub.Machine
	case sub != nil && sub.Via == authz.ViaLocal:
		machine = s.d.Config.Machine
	default:
		return api.Project{}, "", api.Errorf(api.CodeForbidden, "the Mezze app door is set up by a machine's own daemon, on its socket").
			WithHint("run it on the machine whose Mezze app owns the task: zm door -P <team project>")
	}
	if raw == "" {
		return api.Project{}, "", api.Errorf(api.CodeBadRequest, "name the team project (project=)").WithHint("zm door -P <team project>")
	}
	sc, err := s.d.ReadScope(ctx, raw)
	if err != nil {
		return api.Project{}, "", err
	}
	p, err := s.d.Projects.Get(sc, raw)
	if err != nil {
		return api.Project{}, "", err
	}
	if !sc.WholeProject(p.ID) || sub.Check(action, authz.Object{Project: p.ID}, "project", raw) != nil {
		return api.Project{}, "", api.NotFound("project", raw)
	}
	if action != authz.Read {
		wctx := core.WithResolveInput(ctx, project.ResolveInput{Explicit: raw, ExplicitFrom: project.RuleFlag})
		t, err := s.d.WriteTarget(wctx, "", action)
		if err != nil {
			return api.Project{}, "", err
		}
		if t.Project.ID != p.ID || t.Area != "" {
			return api.Project{}, "", api.NotFound("project", raw)
		}
	}
	if !s.d.Store.IsTeam(p.ID) {
		return api.Project{}, "", api.Errorf(api.CodeBadRequest, "%s is not a team project this machine hosts: the Mezze app door lives at a team project's authority", p.Slug).
			WithHint("name a team project this machine joined or hosts (zm team ls)")
	}
	return p, machine, nil
}

func (s *Service) enabled(ctx context.Context, project, machine string) bool {
	var on bool
	_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT enabled FROM door_machines WHERE project = ? AND machine = ?`, project, machine).Scan(&on)
	return on
}

func (s *Service) statusOf(ctx context.Context, p api.Project, machine string) (orchapi.DoorStatus, error) {
	st := orchapi.DoorStatus{Project: p.ID, Machine: machine, On: s.enabled(ctx, p.ID, machine), Available: s.available()}
	if st.Available {
		st.BaseURL = s.baseURL()
	}
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT COUNT(*) FROM door_tasks WHERE project = ? AND machine = ?`, p.ID, machine).Scan(&st.Tasks)
	return st, err
}

// status is GET /v0/door.
func (s *Service) status(r *http.Request) (any, error) {
	p, machine, err := s.caller(r.Context(), r.URL.Query().Get("project"), authz.Read)
	if err != nil {
		return nil, err
	}
	return s.statusOf(r.Context(), p, machine)
}

// toggle is PUT /v0/door: off drops the machine's queue.
func (s *Service) toggle(r *http.Request) (any, error) {
	ctx := r.Context()
	p, machine, err := s.caller(ctx, r.URL.Query().Get("project"), authz.Operate)
	if err != nil {
		return nil, err
	}
	var req orchapi.DoorToggle
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	db := s.d.Store.Registry()
	now := s.now()
	if _, err := db.ExecContext(ctx, `INSERT INTO door_machines (project, machine, enabled, updated) VALUES (?, ?, ?, ?)
		ON CONFLICT (project, machine) DO UPDATE SET enabled = excluded.enabled, updated = excluded.updated`, p.ID, machine, req.On, now); err != nil {
		return nil, err
	}
	if !req.On {
		if _, err := db.ExecContext(ctx, `UPDATE door_deliveries SET state = ?, error = 'the door was turned off', headers = '', body = x'', ended = ? WHERE project = ? AND machine = ? AND state = ?`,
			orchapi.DoorDropped, now, p.ID, machine, orchapi.DoorQueued); err != nil {
			return nil, err
		}
	}
	return s.statusOf(ctx, p, machine)
}

const taskCols = `t.task, t.machine, t.label, t.target, t.token, t.created, t.rotated,
	(SELECT COUNT(*) FROM door_deliveries d WHERE d.project = t.project AND d.machine = t.machine AND d.task = t.task AND d.state = 'queued'),
	COALESCE((SELECT d.received FROM door_deliveries d WHERE d.project = t.project AND d.machine = t.machine AND d.task = t.task ORDER BY d.id DESC LIMIT 1), 0),
	COALESCE((SELECT d.state FROM door_deliveries d WHERE d.project = t.project AND d.machine = t.machine AND d.task = t.task ORDER BY d.id DESC LIMIT 1), '')`

type scanner interface{ Scan(...any) error }

func (s *Service) scanTask(sc scanner, on bool) (orchapi.DoorTask, error) {
	var t orchapi.DoorTask
	var token string
	if err := sc.Scan(&t.Task, &t.Machine, &t.Label, &t.Target, &token, &t.Created, &t.Rotated, &t.Queued, &t.LastAt, &t.LastState); err != nil {
		return t, err
	}
	if base := s.baseURL(); on && s.available() && base != "" && token != "" {
		t.URL = base + hookPrefix + token
	}
	return t, nil
}

func (s *Service) loadTask(ctx context.Context, p api.Project, machine, task string) (orchapi.DoorTask, error) {
	row := s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+taskCols+` FROM door_tasks t WHERE t.project = ? AND t.machine = ? AND t.task = ?`, p.ID, machine, task)
	t, err := s.scanTask(row, s.enabled(ctx, p.ID, machine))
	if errors.Is(err, sql.ErrNoRows) {
		return t, api.NotFound("door task", task)
	}
	return t, err
}

// tasks is GET /v0/door/tasks.
func (s *Service) tasks(r *http.Request) (any, error) {
	ctx := r.Context()
	p, machine, err := s.caller(ctx, r.URL.Query().Get("project"), authz.Read)
	if err != nil {
		return nil, err
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT `+taskCols+` FROM door_tasks t WHERE t.project = ? AND t.machine = ? ORDER BY t.task`, p.ID, machine)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	on := s.enabled(ctx, p.ID, machine)
	out := orchapi.DoorTaskList{Tasks: []orchapi.DoorTask{}}
	for rows.Next() {
		t, err := s.scanTask(rows, on)
		if err != nil {
			return nil, err
		}
		out.Tasks = append(out.Tasks, t)
	}
	return out, rows.Err()
}

func taskID(r *http.Request) (string, error) {
	t := r.PathValue("task")
	if len(t) < 1 || len(t) > 100 {
		return "", api.Errorf(api.CodeBadRequest, "task must be 1-100 characters of A-Z a-z 0-9 . _ -")
	}
	for _, c := range t {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return "", api.Errorf(api.CodeBadRequest, "task must be 1-100 characters of A-Z a-z 0-9 . _ -")
		}
	}
	return t, nil
}

// putTask is PUT /v0/door/tasks/{task}: register or update; the token is
// kept across updates.
func (s *Service) putTask(r *http.Request) (any, error) {
	ctx := r.Context()
	p, machine, err := s.caller(ctx, r.URL.Query().Get("project"), authz.Operate)
	if err != nil {
		return nil, err
	}
	task, err := taskID(r)
	if err != nil {
		return nil, err
	}
	var req orchapi.DoorTaskRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	if err := orchapi.CheckDoorTarget(req.Target); err != nil {
		return nil, err
	}
	if len(req.Label) > 200 {
		return nil, api.Errorf(api.CodeBadRequest, "label is at most 200 bytes")
	}
	db := s.d.Store.Registry()
	var n int
	var exists bool
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(task = ?), 0) > 0 FROM door_tasks WHERE project = ? AND machine = ?`, task, p.ID, machine).Scan(&n, &exists)
	if !exists && n >= maxTasks {
		return nil, api.Errorf(api.CodeTooLarge, "at most %d door tasks per machine and project", maxTasks).WithHint("remove one: zm door rm <task>")
	}
	now := s.now()
	if _, err := db.ExecContext(ctx, `INSERT INTO door_tasks (project, machine, task, token, target, label, created, rotated) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project, machine, task) DO UPDATE SET target = excluded.target, label = excluded.label`,
		p.ID, machine, task, newToken(), req.Target, cut(req.Label, 200), now, now); err != nil {
		return nil, err
	}
	return s.loadTask(ctx, p, machine, task)
}

// removeTask is DELETE /v0/door/tasks/{task}: the task and its queue.
func (s *Service) removeTask(r *http.Request) (any, error) {
	ctx := r.Context()
	p, machine, err := s.caller(ctx, r.URL.Query().Get("project"), authz.Operate)
	if err != nil {
		return nil, err
	}
	task, err := taskID(r)
	if err != nil {
		return nil, err
	}
	t, err := s.loadTask(ctx, p, machine, task)
	if err != nil {
		return nil, err
	}
	tx, err := s.d.Store.Registry().BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM door_tasks WHERE project = ? AND machine = ? AND task = ?`, p.ID, machine, task); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM door_deliveries WHERE project = ? AND machine = ? AND task = ?`, p.ID, machine, task); err != nil {
		return nil, err
	}
	t.URL, t.Queued = "", 0
	return t, tx.Commit()
}

// rotate is POST /v0/door/tasks/{task}/rotate: a new URL; the old one
// answers 404 at once.
func (s *Service) rotate(r *http.Request) (any, error) {
	ctx := r.Context()
	p, machine, err := s.caller(ctx, r.URL.Query().Get("project"), authz.Operate)
	if err != nil {
		return nil, err
	}
	task, err := taskID(r)
	if err != nil {
		return nil, err
	}
	res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE door_tasks SET token = ?, rotated = ? WHERE project = ? AND machine = ? AND task = ?`,
		newToken(), s.now(), p.ID, machine, task)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, api.NotFound("door task", task)
	}
	return s.loadTask(ctx, p, machine, task)
}

// deliveries is GET /v0/door/tasks/{task}/deliveries: newest first.
func (s *Service) deliveries(r *http.Request) (any, error) {
	ctx := r.Context()
	p, machine, err := s.caller(ctx, r.URL.Query().Get("project"), authz.Read)
	if err != nil {
		return nil, err
	}
	task, err := taskID(r)
	if err != nil {
		return nil, err
	}
	if _, err := s.loadTask(ctx, p, machine, task); err != nil {
		return nil, err
	}
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if limit, err = strconv.Atoi(v); err != nil || limit < 1 || limit > 100 {
			return nil, api.Errorf(api.CodeBadRequest, "limit must be 1-100")
		}
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT id, task, delivery, event, bytes, state, attempts, status, error, received, ended, next_at
		FROM door_deliveries WHERE project = ? AND machine = ? AND task = ? ORDER BY id DESC LIMIT ?`, p.ID, machine, task, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := orchapi.DoorDeliveryList{Deliveries: []orchapi.DoorDelivery{}}
	for rows.Next() {
		var d orchapi.DoorDelivery
		var next int64
		if err := rows.Scan(&d.ID, &d.Task, &d.Delivery, &d.Event, &d.Bytes, &d.State, &d.Attempts, &d.Status, &d.Error, &d.Received, &d.Ended, &next); err != nil {
			return nil, err
		}
		if d.State == orchapi.DoorQueued {
			d.NextTryAt = next
		}
		out.Deliveries = append(out.Deliveries, d)
	}
	return out, rows.Err()
}

// ---- deliveries in ----

// deliveryHeaders are where a delivery id comes from, in order.
var deliveryHeaders = []string{"X-Github-Delivery", "Webhook-Id", "Svix-Id", "Request-Id", "X-Request-Id"}

func deliveryID(h http.Header) string {
	for _, k := range deliveryHeaders {
		if v := h.Get(k); v != "" {
			if len(v) > 100 {
				return ""
			}
			for _, c := range v {
				if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == ':' || c == '-') {
					return ""
				}
			}
			return v
		}
	}
	return ""
}

// keepHeaders are the headers that go on to the Mezze app; ok false when over the
// limits.
func keepHeaders(h http.Header) (map[string][]string, bool) {
	out := map[string][]string{}
	n, size := 0, 0
	for k, vs := range h {
		if orchapi.DoorDropHeader(k) {
			continue
		}
		for _, v := range vs {
			n++
			size += len(k) + len(v)
		}
		out[k] = vs
	}
	return out, n <= maxHeaders && size <= maxHeaderBytes
}

func notFound(w http.ResponseWriter) { api.WriteError(w, api.NotFound("door hook", "")) }

// hook is POST /v0/door/hook/{token}.
func (s *Service) hook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	token := r.PathValue("token")
	if !validToken(token) {
		notFound(w)
		return
	}
	var project, machine, task string
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT t.project, t.machine, t.task FROM door_tasks t
		JOIN door_machines m ON m.project = t.project AND m.machine = t.machine AND m.enabled = 1 WHERE t.token = ?`, token).Scan(&project, &machine, &task)
	if err != nil {
		notFound(w)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		api.WriteError(w, api.Errorf(api.CodeTooLarge, "a delivery is at most %d bytes", maxBody))
		return
	}
	headers, ok := keepHeaders(r.Header)
	if !ok {
		api.WriteError(w, api.Errorf(api.CodeTooLarge, "a delivery carries at most %d headers, %d bytes", maxHeaders, maxHeaderBytes))
		return
	}
	hjs, _ := json.Marshal(headers)
	event := r.Header.Get("X-Github-Event")
	if event == "" {
		event = r.Header.Get("Sentry-Hook-Resource")
	}
	db := s.d.Store.Registry()
	var n int
	_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM door_deliveries WHERE project = ? AND machine = ? AND task = ? AND state = ?`, project, machine, task, orchapi.DoorQueued).Scan(&n)
	if n >= maxQueued {
		api.WriteError(w, api.Errorf(api.CodeRateLimited, "%d deliveries already wait for this task's machine", maxQueued).WithRetryAfter(60))
		return
	}
	now := s.now()
	if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO door_deliveries (id, project, machine, task, delivery, event, headers, body, bytes, state, received, next_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, newID(), project, machine, task, deliveryID(r.Header), cut(event, 100), string(hjs), body, len(body),
		orchapi.DoorQueued, now, now); err != nil {
		api.WriteError(w, err)
		return
	}
	s.kick()
	api.WriteJSON(w, http.StatusAccepted, map[string]bool{"queued": true})
}
