// Package homes is the tracker backups module (SPEC 17.9): where a tracker's
// data lives (zm tracker where), scheduled backups (sqlite copies of native
// issues, export snapshots of any tracker), zm issues restore, the off-site
// home backup sets and the watchdog heartbeat. It owns
// $ZM_HOME/trackers/<project>/<prefix>/home.json and the copies under a
// backup target. Targets are parsed in internal/homes/home.
//
// Modules never import each other (package core), so this one meets the work
// module through two core interfaces the server wires:
//
//   - orchcore.TrackerHost, from orchcore.Trackers(d), is what homes asks of work: a
//     tracker's directory, its view, its adapter's describe, a mirror relist,
//     and the export backup, a JSON Lines snapshot of the mirror.
//   - orchcore.HomeService, in d.Homes, is what work asks of homes: tracker rm's
//     guard and cleanup, the export directories a redaction scrubs, the
//     newest copy tracker ls shows; and the issues module asks it to purge
//     old sqlite copies.
//
// The rest it needs is core's d.Projects and d.Store, and orchcore.Issues(d) (copies and
// restores of the store). Every home.json write goes through updateHome;
// every restore and backup of one tracker holds its ops lock.
package homes

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// Service implements orchcore.HomeService and the home, backup and restore
// routes.
type Service struct {
	d   *core.Deps
	now func() time.Time

	hs  homeStates // home.json writers (state.go)
	ops homeOps    // the restore or backup running per tracker
	bk  sync.Map   // tracker key -> its due backups are running (backupDue)
	hb  heartbeats // the watchdog heartbeat's last send
	// snapping: the off-site backup is copying the stores (WaitIdle waits
	// for that part, not for its upload).
	snapping atomic.Bool

	stop context.CancelFunc
	wg   sync.WaitGroup
}

var (
	_ orchcore.HomeService    = (*Service)(nil)
	_ core.Module             = (*Service)(nil)
	_ core.Starter            = (*Service)(nil)
	_ core.Closer             = (*Service)(nil)
	_ orchcore.BackupReporter = (*Service)(nil)
)

// New returns the homes service.
func New(d *core.Deps) *Service { return &Service{d: d, now: time.Now} }

// Start launches the backup schedulers (trackers' and the off-site home
// backup) and the watchdog heartbeat. It returns at once; they stop when ctx
// ends or Close is called.
func (s *Service) Start(ctx context.Context) error {
	ctx, s.stop = context.WithCancel(ctx)
	s.wg.Go(func() {
		t := time.NewTicker(backupTick)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.backupDue(ctx)
			}
		}
	})
	s.wg.Go(func() { s.offsiteLoop(ctx) })
	s.wg.Go(func() { s.heartbeatLoop(ctx) })
	return nil
}

// Close stops the scheduler and waits for the backups it started. The
// server closes homes before work: an export reads work's mirror.
func (s *Service) Close() error {
	if s.stop != nil {
		s.stop()
	}
	s.wg.Wait() // the scheduler and the backups it started (they end with ctx)
	return nil
}

// ScrubExports runs scrub on each export backup directory of tracker
// (project, prefix) under its home lock, so no snapshot is written while a
// redaction runs (orchcore.HomeService). A tracker with no export backup takes
// no lock.
func (s *Service) ScrubExports(project, slug, prefix string, scrub func(dir string) error) error {
	st, err := s.loadHome(project, prefix)
	if err != nil {
		return err
	}
	var dirs []string
	for _, b := range st.Backups {
		if b.Kind == orchapi.BackupExport {
			dirs = append(dirs, backupDir(b.Base, slug, prefix, orchapi.BackupExport))
		}
	}
	if len(dirs) == 0 {
		return nil
	}
	release, err := s.ops.acquire(project, prefix, "redaction scrub")
	if err != nil {
		return err
	}
	defer release()
	var errs []error
	for _, dir := range dirs {
		errs = append(errs, scrub(dir))
	}
	return errors.Join(errs...)
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
		api.WriteJSON(w, http.StatusOK, v)
	}
}

func (s *Service) slugOf(pid string) string {
	if f, ok := s.d.Projects.File(pid); ok {
		return f.Slug
	}
	return pid
}

func (s *Service) home() string {
	if s.d.Config != nil && s.d.Config.Home != "" {
		return s.d.Config.Home
	}
	return os.TempDir() // only bare test services have no config
}

// warn logs under the "work:" prefix homes has always logged with.
func (s *Service) warn(msg string, args ...any) {
	if s.d.Log != nil {
		s.d.Log.Warn("work: "+msg, args...)
	}
}
