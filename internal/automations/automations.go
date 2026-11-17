// Package automations is the automation record brick (SPEC 18.6;
// docs/decisions/2026-11-15-automations-as-zm-records.md): the definition of
// a job a Mezze app home fires, held at the project's authority with its history,
// the home's health summary, and each home's claim (one Mezze app install fires a
// home's jobs). Interface store.Store, driver store/sqlite (project.db),
// second driver named Postgres, conformance store/storetest.
//
// Reads are scoped by the record's project and area. A definition is
// written by its home's own Mezze app only (the home rule, home.go); P-1 adds the
// other way, a proposal a person approves, through Apply. No MCP tool.
// Owned by the automations lane (OWNERSHIP.md).
package automations

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/automations/store"
	"github.com/sengirunners/mezze/internal/automations/store/sqlite"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/project"
)

// Service is the automations module.
type Service struct {
	d *core.Deps
	// Now is the clock (tests set it).
	Now func() time.Time

	stop context.CancelFunc
	wg   sync.WaitGroup
}

var (
	_ core.Module  = (*Service)(nil)
	_ core.Starter = (*Service)(nil)
	_ core.Closer  = (*Service)(nil)
)

// purgeEvery is how often tombstones past their keep are dropped.
var purgeEvery = time.Hour

// listMax is the most records one list answers (truncated past it).
const listMax = 5000

// New returns the automations module and registers its tables and scrubber.
func New(d *core.Deps) *Service {
	d.AddMigrations(sqlite.Migrations...)
	s := &Service{d: d, Now: time.Now}
	d.AddScrubber(s.scrub)
	return s
}

// Start drops tombstones past their keep, at once and every purgeEvery.
func (s *Service) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	s.stop = cancel
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(purgeEvery)
		defer t.Stop()
		for {
			s.purge(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	return nil
}

// Close stops the purge loop.
func (s *Service) Close() error {
	if s.stop != nil {
		s.stop()
	}
	s.wg.Wait()
	return nil
}

func (s *Service) purge(ctx context.Context) {
	for _, p := range s.d.Store.Projects(s.d.System().Scope, authz.Write) {
		if ctx.Err() != nil {
			return
		}
		st, err := s.storeOf(p.ID)
		if err != nil {
			continue
		}
		if n, err := st.Purge(ctx, s.Now()); err != nil {
			slog.Warn("automations: purge", "project", p.ID, "err", err)
		} else if n > 0 {
			slog.Info("automations: tombstones dropped", "project", p.ID, "n", n)
		}
	}
}

// storeOf is project's store: reads through its pool, writes through its
// writer (a team project's single writer connection).
func (s *Service) storeOf(project string) (store.Store, error) {
	read, err := s.d.Store.Project(s.d.System().Scope, authz.Read, project)
	if err != nil {
		return nil, err
	}
	return sqlite.New(read, s.d.Store.Writer(project)), nil
}

// Routes: SPEC 18.6, API.md "Automations".
func (s *Service) Routes(r *core.Router) {
	r = r.For("automations")
	const rule = "the home rule: the record's home machine's own daemon (its owner on the socket, or at the authority its node-bound token) with operate on the whole project, checked outside any agent session (origin mark), naming the install that holds the home's claim"
	r.HandleFunc("GET /v0/automations", core.Authed, "read; ?project= required; only records in areas the caller reads", s.handle(s.list))
	r.HandleFunc("POST /v0/automations", core.Authed, rule+"; the claim's store is zm", s.handle(s.create))
	r.HandleFunc("GET /v0/automations/{id}", core.Authed, "read on the record's area (hidden like missing)", s.handle(s.show))
	r.HandleFunc("PATCH /v0/automations/{id}", core.Authed, "read on the record's area (hidden like missing), then "+rule+"; the claim's store is zm", s.handle(s.patch))
	r.HandleFunc("DELETE /v0/automations/{id}", core.Authed, "read on the record's area (hidden like missing), then "+rule+"; the claim's store is zm", s.handle(s.remove))
	r.HandleFunc("GET /v0/automations/{id}/history", core.Authed, "read on the record's area (hidden like missing)", s.handle(s.history))
	r.HandleFunc("PUT /v0/automations/{id}/health", core.Authed, "read on the record's area (hidden like missing), then "+rule, s.handle(s.putHealth))
	r.HandleFunc("GET /v0/automation-homes", core.Authed, "read on the whole project; ?project= required", s.handle(s.homes))
	r.HandleFunc("PUT /v0/automation-homes/{home}", core.Authed, "the home rule without the claim: the home machine's own daemon with operate on the whole project, outside any agent session", s.handle(s.claim))
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

func (s *Service) now() time.Time { return s.Now().UTC() }

// wire is how a time is answered.
func wire(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// projectOf is the project a list or claim names: ?project=, else a
// Zm-Project the caller set with --project. It answers only a project the
// caller sees (hidden like missing) and the caller's scope narrowed to it.
func (s *Service) projectOf(r *http.Request, raw string) (api.Project, authz.Scope, error) {
	ctx := r.Context()
	if raw == "" {
		raw = r.URL.Query().Get("project")
	}
	if in := core.ResolveInputOf(ctx); raw == "" && in.ExplicitFrom == project.RuleFlag {
		raw = in.Explicit
	}
	ref, _, _ := strings.Cut(raw, "/")
	if ref == "" {
		return api.Project{}, authz.Scope{}, bad("name the project (project=)")
	}
	sc, err := s.d.ReadScope(ctx, ref)
	if err != nil {
		return api.Project{}, authz.Scope{}, err
	}
	p, err := s.d.Projects.Get(sc, ref)
	if err != nil {
		return api.Project{}, authz.Scope{}, err
	}
	return p, sc, nil
}

// found is a record as the caller found it.
type found struct {
	p   api.Project
	st  store.Store
	rec store.Record
}

// find loads record id from the projects sub reads, then checks sub reads
// its area: hidden answers exactly like missing.
func (s *Service) find(ctx context.Context, sub *authz.Subject, id string) (found, error) {
	nf := api.NotFound("automation", id)
	if sub == nil || !ids.Valid(id) {
		return found{}, nf
	}
	key := strings.ToUpper(id)
	for _, pd := range s.d.Store.Projects(sub.Scope, authz.Read) {
		st := sqlite.New(pd.DB, s.d.Store.Writer(pd.ID))
		rec, err := st.Get(ctx, key)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return found{}, err
		}
		if !sub.Can(authz.Read, authz.Object{Project: pd.ID, Area: rec.Area}) {
			return found{}, nf
		}
		p, err := s.d.Projects.Get(sub.Scope, pd.ID)
		if err != nil {
			return found{}, nf
		}
		return found{p: p, st: st, rec: rec}, nil
	}
	return found{}, nf
}

// view is rec as sub sees it (full: with fields, body and settings).
func (s *Service) view(ctx context.Context, sub *authz.Subject, p api.Project, rec store.Record) orchapi.Automation {
	owner := rec.Owner
	if _, err := api.ParseAddress(owner); err == nil && owner != "" {
		owner = s.d.RedactFor(ctx, sub, owner)
	}
	e := orchapi.AutomationEnvelope{ID: rec.ID, Project: p.Slug, Area: rec.Area, Kind: rec.Kind, Name: rec.Name, Enabled: rec.Enabled,
		ResumeRev: rec.ResumeRev, Owner: owner, Home: rec.Home, TaskID: rec.TaskID, Rev: rec.Rev, Created: wire(rec.Created),
		Updated: wire(rec.Updated), UpdatedBy: s.d.RedactFor(ctx, sub, rec.UpdatedBy), Health: rec.Health}
	if rec.Deleted != nil {
		e.Deleted = wire(*rec.Deleted)
	}
	fields := rec.Fields
	if len(fields) == 0 {
		fields = []byte(`{}`)
	}
	return orchapi.Automation{AutomationEnvelope: e, Fields: fields, Body: rec.Body, Settings: rec.Settings}
}

// publish sends an automation event (never the definition).
func (s *Service) publish(ctx context.Context, typ string, p api.Project, rec store.Record, state string) {
	if s.d.Events == nil {
		return
	}
	if _, err := s.d.Events.Publish(context.WithoutCancel(ctx), events.Input{Type: typ, Subject: rec.ID, Project: p.ID, Area: rec.Area,
		Data: orchapi.AutomationEvent{Project: p.Slug, Area: rec.Area, ID: rec.ID, Rev: rec.Rev, State: state}}); err != nil {
		slog.Warn("automations: event", "type", typ, "id", rec.ID, "err", err)
	}
}

// list is GET /v0/automations.
func (s *Service) list(r *http.Request) (any, error) {
	ctx := r.Context()
	p, sc, err := s.projectOf(r, "")
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	st, err := s.storeOf(p.ID)
	if err != nil {
		return nil, err
	}
	recs, truncated, err := st.List(ctx, sc, p.ID, store.Query{Home: q.Get("home"), Kind: q.Get("kind"), Deleted: q.Get("deleted") == "1", Limit: listMax})
	if err != nil {
		return nil, err
	}
	sub := core.SubjectOf(ctx)
	out := orchapi.AutomationList{Automations: []orchapi.AutomationEnvelope{}, Truncated: truncated}
	for _, rec := range recs {
		out.Automations = append(out.Automations, s.view(ctx, sub, p, rec).AutomationEnvelope)
	}
	if home := q.Get("home"); home != "" {
		if h, err := st.Home(ctx, home); err == nil {
			out.Holder = h.Install
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	return out, nil
}

// show is GET /v0/automations/{id}.
func (s *Service) show(r *http.Request) (any, error) {
	sub := core.SubjectOf(r.Context())
	f, err := s.find(r.Context(), sub, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	return s.view(r.Context(), sub, f.p, f.rec), nil
}

// history is GET /v0/automations/{id}/history.
func (s *Service) history(r *http.Request) (any, error) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	f, err := s.find(ctx, sub, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	revs, err := f.st.History(ctx, f.rec.ID)
	if err != nil {
		return nil, err
	}
	out := orchapi.AutomationHistory{Revisions: []orchapi.AutomationRevision{}}
	for _, v := range revs {
		owner := v.Def.Owner
		if _, err := api.ParseAddress(owner); err == nil && owner != "" {
			owner = s.d.RedactFor(ctx, sub, owner)
		}
		out.Revisions = append(out.Revisions, orchapi.AutomationRevision{Rev: v.Rev, Op: v.Op, Updated: wire(v.At), UpdatedBy: s.d.RedactFor(ctx, sub, v.By),
			Name: v.Def.Name, Enabled: v.Def.Enabled, ResumeRev: v.Def.ResumeRev, Owner: owner, Fields: v.Def.Fields, Body: v.Def.Body, Settings: v.Def.Settings})
	}
	return out, nil
}

// create is POST /v0/automations.
func (s *Service) create(r *http.Request) (any, error) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	var req orchapi.AutomationCreate
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	p, _, err := s.projectOf(r, req.Project)
	if err != nil {
		return nil, err
	}
	if err := validHome(req.Home); err != nil {
		return nil, err
	}
	if err := s.asHome(r, p.ID, req.Home); err != nil {
		return nil, err
	}
	area := req.Area
	if area == "" {
		_, area, _ = strings.Cut(firstNonEmpty(req.Project, r.URL.Query().Get("project")), "/")
	}
	if area != "" && !s.d.Projects.HasArea(p.ID, area) {
		return nil, api.NotFound("area", area)
	}
	kind, err := validKind(req.Kind)
	if err != nil {
		return nil, err
	}
	def, err := checkDef(kind, store.Definition{Name: req.Name, Enabled: req.Enabled == nil || *req.Enabled, Owner: req.Owner,
		Fields: req.Fields, Body: req.Body, Settings: req.Settings})
	if err != nil {
		return nil, err
	}
	id := ids.New()
	task := req.TaskID
	if task == "" {
		task = "automation:" + id
	}
	if !taskIDRe.MatchString(task) {
		return nil, bad("task_id is at most 200 printable characters, no spaces")
	}
	st, err := s.storeOf(p.ID)
	if err != nil {
		return nil, err
	}
	if err := s.holds(ctx, st, req.Home, req.Install, true); err != nil {
		return nil, err
	}
	rec, err := st.Create(ctx, store.Record{ID: id, Area: area, Kind: kind, Home: req.Home, TaskID: task, Definition: def}, s.write(r, req.Home))
	var ex *store.ExistsError
	switch {
	case errors.As(err, &ex):
		return nil, api.Errorf(api.CodeConflict, "home %s already has a record for task %s: %s", req.Home, task, ex.ID).
			WithHint("read it (GET /v0/automations/" + ex.ID + ") and patch it")
	case errors.Is(err, store.ErrWriteIDUsed):
		return nil, api.Errorf(api.CodeConflict, "this write id already made another change")
	case err != nil:
		return nil, err
	}
	if rec.ID == id {
		s.publish(ctx, orchapi.EventAutomationCreated, p, rec, "")
	}
	return s.view(ctx, sub, p, rec), nil
}

func firstNonEmpty(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// checkDef validates a whole definition for kind.
func checkDef(kind string, d store.Definition) (store.Definition, error) {
	if err := line("name", d.Name, maxName, true); err != nil {
		return d, err
	}
	if err := line("owner", d.Owner, maxOwner, false); err != nil {
		return d, err
	}
	if err := validBody(d.Body); err != nil {
		return d, err
	}
	f, err := validFields(kind, d.Fields)
	if err != nil {
		return d, err
	}
	d.Fields = f
	if d.Settings, err = validSettings(d.Settings); err != nil {
		return d, err
	}
	return d, nil
}

// write is the store write a home's call makes: as its machine, with the
// replica's write id when a member forwarded it.
func (s *Service) write(r *http.Request, home string) store.Write {
	return store.Write{ID: core.WriteID(r), By: "machine:" + home, At: s.now()}
}

// patch is PATCH /v0/automations/{id}.
func (s *Service) patch(r *http.Request) (any, error) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	f, err := s.find(ctx, sub, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	var req orchapi.AutomationPatch
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	if err := s.asHome(r, f.p.ID, f.rec.Home); err != nil {
		return nil, err
	}
	if err := s.holds(ctx, f.st, f.rec.Home, req.Install, true); err != nil {
		return nil, err
	}
	if req.Rev <= 0 {
		return nil, bad("rev: name the revision this change was made from")
	}
	rec, err := f.st.Update(ctx, f.rec.ID, s.write(r, f.rec.Home), func(cur store.Record) (store.Definition, error) {
		if cur.Rev != req.Rev {
			return store.Definition{}, changedSince(cur.ID, cur.Rev, req.Rev)
		}
		return applyPatch(cur, req)
	})
	if err := writeErr(err, f.rec.ID); err != nil {
		return nil, err
	}
	if rec.Rev > f.rec.Rev {
		s.publish(ctx, orchapi.EventAutomationUpdated, f.p, rec, "")
	}
	return s.view(ctx, sub, f.p, rec), nil
}

func changedSince(id string, at, read int64) error {
	return api.Errorf(api.CodeConflict, "automation %s changed since: it is at rev %d, this change was made from rev %d", id, at, read).
		WithHint("read it again, then decide")
}

// writeErr maps a store write's refusals to the API's.
func writeErr(err error, id string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrNotFound): // purged since it was read
		return api.NotFound("automation", id)
	case errors.Is(err, store.ErrDeleted):
		return api.Errorf(api.CodeConflict, "automation %s is deleted", id)
	case errors.Is(err, store.ErrChanged):
		return api.Errorf(api.CodeConflict, "automation %s changed since", id).WithHint("read it again, then decide")
	case errors.Is(err, store.ErrWriteIDUsed):
		return api.Errorf(api.CodeConflict, "this write id already made another change")
	}
	return err
}

// applyPatch is cur's definition with req applied: top-level keys replace,
// fields and settings take a merge patch (fields whole with replace_fields).
func applyPatch(cur store.Record, req orchapi.AutomationPatch) (store.Definition, error) {
	d := cur.Definition
	if req.Name != nil {
		d.Name = *req.Name
	}
	if req.Enabled != nil {
		d.Enabled = *req.Enabled
	}
	if len(req.Owner) > 0 {
		var o *string
		if err := jsonUnmarshal(req.Owner, &o); err != nil {
			return d, bad("owner must be text or null")
		}
		d.Owner = ""
		if o != nil {
			d.Owner = *o
		}
	}
	if req.Body != nil {
		d.Body = *req.Body
	}
	if len(req.Fields) > 0 {
		if req.ReplaceFields {
			d.Fields = req.Fields
		} else {
			merged, err := mergePatch(cur.Fields, req.Fields)
			if err != nil {
				return d, bad("fields: not a JSON merge patch: %v", err)
			}
			d.Fields = merged
		}
	} else if req.ReplaceFields {
		return d, bad("replace_fields needs fields")
	}
	if len(req.Settings) > 0 {
		curJSON, err := jsonMarshal(cur.Settings)
		if err != nil {
			return d, err
		}
		merged, err := mergePatch(curJSON, req.Settings)
		if err != nil {
			return d, bad("settings: not a JSON merge patch: %v", err)
		}
		d.Settings = nil
		if string(merged) != "null" {
			if err := jsonStrict(merged, &d.Settings); err != nil {
				return d, bad("settings: %v", err)
			}
		}
	}
	return checkDef(cur.Kind, d)
}

// remove is DELETE /v0/automations/{id}?install=[&rev=].
func (s *Service) remove(r *http.Request) (any, error) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	f, err := s.find(ctx, sub, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	if err := s.asHome(r, f.p.ID, f.rec.Home); err != nil {
		return nil, err
	}
	q := r.URL.Query()
	if err := s.holds(ctx, f.st, f.rec.Home, q.Get("install"), true); err != nil {
		return nil, err
	}
	var want int64
	if raw := q.Get("rev"); raw != "" && f.rec.Deleted == nil {
		if want, err = parseRev(raw); err != nil {
			return nil, err
		} else if want != f.rec.Rev {
			return nil, changedSince(f.rec.ID, f.rec.Rev, want)
		}
	}
	rec, err := f.st.Delete(ctx, f.rec.ID, want, s.write(r, f.rec.Home))
	if err := writeErr(err, f.rec.ID); err != nil {
		return nil, err
	}
	if rec.Rev > f.rec.Rev {
		s.publish(ctx, orchapi.EventAutomationDeleted, f.p, rec, "")
	}
	return s.view(ctx, sub, f.p, rec), nil
}

// putHealth is PUT /v0/automations/{id}/health: no history row, no rev; an
// event only when the state changes. A tombstone takes it too (the last).
func (s *Service) putHealth(r *http.Request) (any, error) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	f, err := s.find(ctx, sub, r.PathValue("id"))
	if err != nil {
		return nil, err
	}
	var h orchapi.AutomationHealth
	if err := api.DecodeJSON(r, &h); err != nil {
		return nil, err
	}
	if err := s.asHome(r, f.p.ID, f.rec.Home); err != nil {
		return nil, err
	}
	if err := s.holds(ctx, f.st, f.rec.Home, h.Install, false); err != nil {
		return nil, err
	}
	if err := validHealth(&h); err != nil {
		return nil, err
	}
	now := s.now()
	h.Received = wire(now)
	prev, err := f.st.PutHealth(ctx, f.rec.ID, h, now)
	if err := writeErr(err, f.rec.ID); err != nil {
		return nil, err
	}
	if prev == nil || prev.State != h.State {
		s.publish(ctx, orchapi.EventAutomationHealth, f.p, f.rec, h.State)
	}
	return h, nil
}

// homes is GET /v0/automation-homes: read on the whole project.
func (s *Service) homes(r *http.Request) (any, error) {
	ctx := r.Context()
	p, sc, err := s.projectOf(r, "")
	if err != nil {
		return nil, err
	}
	if !sc.WholeProject(p.ID) {
		return nil, api.NotFound("project", firstNonEmpty(r.URL.Query().Get("project"), core.ResolveInputOf(ctx).Explicit))
	}
	st, err := s.storeOf(p.ID)
	if err != nil {
		return nil, err
	}
	hs, err := st.Homes(ctx)
	if err != nil {
		return nil, err
	}
	out := orchapi.AutomationHomes{Homes: []orchapi.AutomationHome{}}
	for _, h := range hs {
		out.Homes = append(out.Homes, homeView(h))
	}
	return out, nil
}

func homeView(h store.Home) orchapi.AutomationHome {
	v := orchapi.AutomationHome{Home: h.Home, Install: h.Install, Store: h.Store, Path: h.Path, Claimed: wire(h.Claimed), Updated: wire(h.Updated),
		LastHealth: wire(h.LastHealth)}
	if h.Wanted != nil {
		v.Wanted = &orchapi.AutomationWanted{Install: h.Wanted.Install, Store: h.Wanted.Store, Path: h.Wanted.Path, First: wire(h.Wanted.First)}
	}
	return v
}

// claim is PUT /v0/automation-homes/{home}?project=: one install per home.
func (s *Service) claim(r *http.Request) (any, error) {
	ctx := r.Context()
	p, _, err := s.projectOf(r, "")
	if err != nil {
		return nil, err
	}
	home := r.PathValue("home")
	if err := validHome(home); err != nil {
		return nil, err
	}
	if err := s.asHome(r, p.ID, home); err != nil {
		return nil, err
	}
	var c orchapi.AutomationHomeClaim
	if err := api.DecodeJSON(r, &c); err != nil {
		return nil, err
	}
	if err := validInstall(c.Install); err != nil {
		return nil, err
	}
	if err := validStore(c.Store); err != nil {
		return nil, err
	}
	if err := line("path", c.Path, maxPath, false); err != nil {
		return nil, err
	}
	st, err := s.storeOf(p.ID)
	if err != nil {
		return nil, err
	}
	h, held, err := st.Claim(ctx, home, store.Claim{Install: c.Install, Store: c.Store, Path: c.Path}, s.now())
	if err != nil {
		return nil, err
	}
	v := homeView(h)
	if !held {
		return nil, api.Errorf(api.CodeConflict, "install %s holds home %s (since %s); install %s is refused and recorded as the home's wanted install", h.Install, home, wire(h.Claimed), c.Install).
			WithHint("this copy of the Mezze app must not fire the home's jobs; a person gives it the home with a takeover").WithData(v)
	}
	return v, nil
}
