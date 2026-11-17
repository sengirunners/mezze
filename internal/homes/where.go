package homes

// zm tracker where (SPEC 17.9), and the routes of the homes module.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// Routes registers the where, backup and restore routes (SPEC 17.9). All
// local: backups are what this machine's daemon runs and reads.
func (s *Service) Routes(r *core.Router) {
	r = r.For("homes")
	r.HandleFunc("GET /v0/trackers/{prefix}/home", core.Local, "local: admin on the resolved project; where the tracker's data lives, its backups and their age", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		return s.where(r.Context(), r.PathValue("prefix"))
	}))
	r.HandleFunc("GET /v0/trackers/{prefix}/backups", core.Local, "local: admin on the resolved project; the tracker's backups", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		return s.listBackups(r.Context(), r.PathValue("prefix"))
	}))
	r.HandleFunc("POST /v0/trackers/{prefix}/backups", core.Local, "local: admin on the resolved project; schedules a backup (run_now runs it)", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		var req orchapi.AddBackupRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.addBackup(r.Context(), r.PathValue("prefix"), req)
	}))
	r.HandleFunc("DELETE /v0/trackers/{prefix}/backups", core.Local, "local: admin on the resolved project; unschedules ?id= (&kind=); the copies stay", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		return s.removeBackup(r.Context(), r.PathValue("prefix"), r.URL.Query().Get("id"), r.URL.Query().Get("kind"))
	}))
	r.HandleFunc("POST /v0/trackers/{prefix}/backups/run", core.Local, "local: admin on the resolved project; runs one backup (id) or all now", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		var req struct{ ID, Kind string }
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.runBackups(r.Context(), r.PathValue("prefix"), req.ID, req.Kind)
	}))
	r.HandleFunc("POST /v0/issues/restore", core.Local, "local: admin on the resolved project; replaces its native store with a sqlite backup's copy, redaction list applied; refuses a newer schema or another project's store", s.jsonH(func(r *http.Request, _ *authz.Subject, _ authz.Scope) (any, error) {
		var req orchapi.IssueRestoreRequest
		if err := api.DecodeJSON(r, &req); err != nil {
			return nil, err
		}
		return s.restoreNative(r.Context(), req)
	}))
	s.offsiteRoutes(r)
}

// where is GET /v0/trackers/{prefix}/home.
func (s *Service) where(ctx context.Context, prefix string) (orchapi.TrackerWhere, error) {
	pid, slug, f, err := s.d.TrackerTarget(ctx)
	if err != nil {
		return orchapi.TrackerWhere{}, err
	}
	tr, ok := orchapi.FindTracker(orchcore.ProjectTrackers(f), prefix)
	if !ok {
		return orchapi.TrackerWhere{}, api.NotFound("tracker", prefix)
	}
	w := orchapi.TrackerWhere{Tracker: orchcore.Trackers(s.d).TrackerView(ctx, pid, slug, tr), Backups: []orchapi.BackupView{},
		Home: orchapi.HomeView{Target: tr.Adapter, Kind: "service"}}
	w.Kinds = s.backupKinds(ctx, pid, tr)
	st, err := s.loadHome(pid, prefix)
	if err != nil {
		return w, err
	}
	now := s.now()
	failing, restorable := 0, false
	for _, b := range st.Backups {
		v := s.backupView(pid, slug, prefix, b, now)
		if v.Failures > 0 {
			failing++
		}
		if v.ScrubPending {
			w.Notes = append(w.Notes, fmt.Sprintf("backup %s holds copies from before a redaction: they go once a fresh copy is verified", v.ID))
		}
		restorable = restorable || b.Kind == orchapi.BackupSqlite
		w.Backups = append(w.Backups, v)
	}
	if len(st.Backups) == 0 {
		w.Notes = append(w.Notes, fmt.Sprintf("no backups: zm tracker backup add %s icloud (or file:///path)", prefix))
	} else if tr.Adapter == orchapi.AdapterNative && !restorable {
		w.Notes = append(w.Notes, fmt.Sprintf("an export cannot restore a native store: zm tracker backup add %s <target> --kind sqlite", prefix))
	}
	if failing > 0 {
		w.Notes = append(w.Notes, fmt.Sprintf("%d backup(s) failing: zm tracker backup run %s shows why", failing, prefix))
	}
	return w, nil
}
