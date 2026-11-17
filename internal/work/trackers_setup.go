package work

// The work service's side of SPEC 11's setup verb for zm tracker add: the
// args (internal/work/setup checks the options), the dry run, the call
// before or after binding, and its undo (contained by internal/work/setup). The adapter
// does the work; nothing here knows a tracker.

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/work/setup"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/project"
)

// beforeSetupHook runs just before a before_bind setup call (tests only).
var beforeSetupHook func()

// setupArgs is what setup gets besides dry_run: the options
// (setup.Options: the declared flags, nothing a --config may override) and
// zm, what zm fills itself, which no caller input reaches (SPEC 11).
func (s *Service) setupArgs(t tracked, pid string, f project.File, tr api.Tracker, req orchapi.AddTrackerRequest) (orchapi.AdapterSetupArgs, error) {
	var info *orchapi.AdapterSetupInfo
	if t.has(orchapi.VerbSetup) {
		info = t.desc.Setup
	}
	opts, err := setup.Options(info, tr.Adapter, req)
	if err != nil {
		return orchapi.AdapterSetupArgs{}, err
	}
	args := orchapi.AdapterSetupArgs{Options: opts}
	if t.has(orchapi.VerbSetup) {
		z := &orchapi.AdapterSetupZM{DataDir: s.trackerDir(pid, tr.Prefix), IssuePrefix: f.Slug, HasPassword: req.PasswordFile != ""}
		for _, x := range orchcore.ProjectTrackers(f) { // prefixes only: never another tracker's config
			if x.Adapter == tr.Adapter && x.Prefix != tr.Prefix {
				z.Siblings = append(z.Siblings, x.Prefix)
			}
		}
		args.ZM = z
	}
	return args, nil
}

// setupDryRun asks an adapter that offers setup to check tr's config and
// options and merges the defaults it returns (only keys tr does not set).
// The actor is the machine's human (whose identity a default may name).
func (s *Service) setupDryRun(ctx context.Context, t tracked, tr *api.Tracker, args orchapi.AdapterSetupArgs) error {
	if !t.has(orchapi.VerbSetup) {
		return nil // no setup; preflight already vouched for the adapter
	}
	var out orchapi.AdapterSetupResult
	if err := s.callAs(ctx, t, orchapi.VerbSetup, s.human(), setup.With(args, true), &out); err != nil {
		return err
	}
	setup.MergeConfig(tr, out.Config)
	return nil
}

// setupBefore runs a before_bind adapter's setup before binding: its config
// is merged into tr, its notes and schema go into res, and the returned undo
// takes back what it made (an add that fails later runs it).
func (s *Service) setupBefore(ctx context.Context, pid string, tr *api.Tracker, args orchapi.AdapterSetupArgs, res *orchapi.TrackerResult) (func(), error) {
	none := func() {}
	t, err := s.tracker(ctx, pid, *tr)
	if err != nil {
		return none, err
	}
	if t.desc.Setup != nil && len(t.desc.Setup.Touches) > 0 {
		res.Notes = append(res.Notes, "setup touches: "+strings.Join(t.desc.Setup.Touches, "; "))
	}
	root := s.trackerDir(pid, tr.Prefix)
	before := setup.Snapshot(root)
	if beforeSetupHook != nil {
		beforeSetupHook()
	}
	ctx, cancel := context.WithTimeout(ctx, setupWait)
	defer cancel()
	var out orchapi.AdapterSetupResult
	if err := s.callAs(ctx, t, orchapi.VerbSetup, s.human(), setup.With(args, false), &out); err != nil {
		return none, err
	}
	setup.MergeConfig(tr, out.Config)
	res.Created, res.Schema = out.Created, out.Schema
	res.Notes = append(res.Notes, out.Notes...)
	if !out.Created || len(out.Undo) == 0 {
		return none, nil
	}
	return func() { setup.Undo(root, before, out.Undo, s.warn) }, nil
}

// setupWait bounds a setup call (SPEC 11: 120 s).
var setupWait = 2 * time.Minute

// setupApply runs an after-bind adapter's one-time setup once tr is bound;
// its notes (and a failure) are reported, never fatal.
func (s *Service) setupApply(ctx context.Context, pid string, tr api.Tracker, args orchapi.AdapterSetupArgs, res *orchapi.TrackerResult) {
	t, err := s.tracker(ctx, pid, tr)
	if err != nil || !t.has(orchapi.VerbSetup) || (t.desc.Setup != nil && t.desc.Setup.BeforeBind) {
		return
	}
	if t.desc.Setup != nil && len(t.desc.Setup.Touches) > 0 {
		res.Notes = append(res.Notes, "setup touches: "+strings.Join(t.desc.Setup.Touches, "; "))
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var out orchapi.AdapterSetupResult
	if err := s.callAs(ctx, t, orchapi.VerbSetup, s.human(), setup.With(args, false), &out); err != nil {
		res.Notes = append(res.Notes, "the adapter's setup failed: "+err.Error())
		return
	}
	res.Notes = append(res.Notes, out.Notes...)
}

// closeStale stops the serve process t ran in when tr's environment is
// now another one (the dry run ran before the config chose the mode).
func (s *Service) closeStale(t tracked, pid string, tr api.Tracker) {
	if !t.desc.Serve || t.path == nativePath {
		return
	}
	env, _, err := s.adapterEnv(pid, tr, t.desc.Env)
	if err != nil || slices.Equal(env, t.env) {
		return
	}
	go s.pool.CloseEnv(t.path, t.env)
}
