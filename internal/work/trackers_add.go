package work

// zm tracker add|ls|test|rm (SPEC 17.2): one command binds a tracker to a
// project, live. The daemon rewrites project.toml (no restart), runs the
// read-only conformance checks and the mirror's first full sync, and rolls
// the binding back if a check fails.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/mezze/internal/work/conformance"
	"github.com/sengirunners/mezze/internal/work/mirrorstate"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/secrets"
)

// defaultFirstSyncWait bounds how long add waits for the first full sync.
var defaultFirstSyncWait = 5 * time.Minute

// plan turns an add request into the binding (no side effects).
func (s *Service) plan(pid string, f project.File, req orchapi.AddTrackerRequest) (api.Tracker, error) {
	name := strings.TrimSpace(req.Adapter)
	if name == "" || strings.ContainsRune(name, os.PathSeparator) {
		return api.Tracker{}, api.Errorf(api.CodeBadRequest, "name an adapter: native, or <name> for zm-work-<name>")
	}
	prefix := req.Prefix
	if prefix == "" {
		prefix = name
	}
	if !orchapi.ValidTrackerPrefix(prefix) || prefix == LocalSource || prefix == "human" || prefix == "machine" {
		return api.Tracker{}, api.Errorf(api.CodeBadRequest, "bad tracker prefix %q (lowercase letters, digits, -; not local/human/machine)", prefix)
	}
	if _, taken := orchapi.FindTracker(orchcore.ProjectTrackers(f), prefix); taken {
		return api.Tracker{}, orchapi.TrackerPrefixTaken(f.Slug, prefix)
	}
	if req.Env != "" && req.Env != api.TrackerEnvInherit {
		return api.Tracker{}, api.Errorf(api.CodeBadRequest, "env must be %q or absent", api.TrackerEnvInherit)
	}
	cfg := map[string]string{}
	for k, v := range req.Config {
		cfg[k] = v
	}
	tr := api.Tracker{Adapter: name, Prefix: prefix, Area: req.Area, Env: req.Env, Config: cfg}
	if req.Readonly {
		cfg["readonly"] = "true"
	}
	if err := genericConfig(cfg); err != nil {
		return tr, err
	}
	if len(cfg) == 0 {
		tr.Config = nil
	}
	return tr, nil
}

// inlineSecretKeys are config keys that would put a secret in project.toml:
// refused for every adapter (the secret stays in a file or keychain item
// the config names).
var inlineSecretKeys = []string{"key", "api_key", "apikey", "token", "secret", "password", "client_secret"}

// genericConfig applies the rules every adapter's config gets: no inline
// secret, and paths (keys ending in _file or _dir) made absolute so the
// adapter, started elsewhere, finds them.
func genericConfig(cfg map[string]string) error {
	for k, v := range cfg {
		lk := strings.ToLower(k)
		if slices.Contains(inlineSecretKeys, lk) {
			return api.Errorf(api.CodeBadRequest, "config %q would store a secret in project.toml", k).
				WithHint("put it in a file (" + k + "_file) or a keychain item the adapter reads; see its README")
		}
		if v != "" && (strings.HasSuffix(lk, "_file") || strings.HasSuffix(lk, "_dir")) {
			abs, err := filepath.Abs(core.ExpandHome(v))
			if err != nil {
				return err
			}
			cfg[k] = abs
		}
	}
	return nil
}

func (s *Service) human() string {
	if s.d.Config == nil {
		return ""
	}
	return s.d.Config.Human
}

// addTracker is POST /v0/trackers.
func (s *Service) addTracker(ctx context.Context, req orchapi.AddTrackerRequest) (orchapi.TrackerResult, error) {
	pid, slug, f, err := s.d.TrackerTarget(ctx)
	if err != nil {
		return orchapi.TrackerResult{}, err
	}
	tr, err := s.plan(pid, f, req)
	if err != nil {
		return orchapi.TrackerResult{}, err
	}
	// Reserve the prefix before any side effect; then look again, in case
	// another add bound it between plan and here.
	if !s.adds.Reserve(pid, tr.Prefix) {
		return orchapi.TrackerResult{}, orchapi.TrackerPrefixTaken(slug, tr.Prefix)
	}
	defer s.adds.Release(pid, tr.Prefix)
	if f, ok := s.d.Projects.File(pid); ok {
		if _, taken := orchapi.FindTracker(orchcore.ProjectTrackers(f), tr.Prefix); taken {
			return orchapi.TrackerResult{}, orchapi.TrackerPrefixTaken(slug, tr.Prefix)
		}
	}
	if err := s.preflight(pid, tr); err != nil {
		return orchapi.TrackerResult{}, err
	}
	// The adapter checks its own config and options (dry run) and may fill
	// defaults, before anything is bound (SPEC 11 setup).
	// The tracker's directory is zm's own: one this add made (the adapter's setup
	// or first call makes it) goes again if the add fails. What the adapter's
	// setup made inside it is undone by its own undo (setupBefore).
	madeDir := !core.PathExists(s.trackerDir(pid, tr.Prefix))
	var undos []func()
	undo := func() {
		for i := len(undos) - 1; i >= 0; i-- {
			undos[i]()
		}
		if madeDir {
			if st, err := os.Lstat(s.trackerDir(pid, tr.Prefix)); err == nil && st.IsDir() {
				os.RemoveAll(s.trackerDir(pid, tr.Prefix))
			}
		}
	}
	t, err := s.tracker(ctx, pid, tr)
	if err != nil {
		undo()
		return orchapi.TrackerResult{}, err
	}
	args, err := s.setupArgs(t, pid, f, tr, req)
	if err != nil {
		undo()
		return orchapi.TrackerResult{}, err
	}
	if err := s.setupDryRun(ctx, t, &tr, args); err != nil {
		undo()
		return orchapi.TrackerResult{}, err
	}
	s.closeStale(t, pid, tr)
	// The password is the daemon's: copied into the tracker's secret slot
	// before setup; the adapter gets it only in its environment.
	if req.PasswordFile != "" {
		secret := s.secretFile(pid, tr.Prefix)
		if err := secrets.CopyFile(core.ExpandHome(req.PasswordFile), secret); err != nil {
			undo()
			return orchapi.TrackerResult{}, err
		}
		undos = append(undos, func() { os.Remove(secret) })
	}
	res := orchapi.TrackerResult{Checks: []orchapi.TrackerCheck{}}
	if t.has(orchapi.VerbSetup) && t.desc.Setup != nil && t.desc.Setup.BeforeBind {
		u, err := s.setupBefore(ctx, pid, &tr, args, &res)
		if err != nil {
			undo()
			return orchapi.TrackerResult{}, err
		}
		undos = append(undos, u)
	}
	if _, err := orchcore.SetTrackers(ctx, s.d.Projects, pid, func(old []api.Tracker) ([]api.Tracker, error) {
		if _, taken := orchapi.FindTracker(old, tr.Prefix); taken {
			return nil, orchapi.TrackerPrefixTaken(slug, tr.Prefix)
		}
		return append(old, tr), nil
	}); err != nil {
		undo()
		return orchapi.TrackerResult{}, err
	}
	wait := defaultFirstSyncWait
	if req.WaitSeconds > 0 {
		wait = time.Duration(req.WaitSeconds) * time.Second
	}
	s.verify(ctx, pid, tr, wait, &res)
	if !res.OK {
		s.unbind(context.WithoutCancel(ctx), pid, tr)
		undo()
		why := "a check failed"
		for _, c := range res.Checks {
			if !c.OK && !c.Skipped {
				why = c.Name + ": " + c.Detail
				break
			}
		}
		hint := "zm tracker add --json shows every check; fix the tracker and add it again"
		return orchapi.TrackerResult{}, api.Errorf(api.CodeUnavailable, "tracker %s did not pass its checks, so it was not bound (%s)", tr.Prefix, core.ClipText(why, 400)).
			WithHint(hint).WithData(res)
	}
	// An after-bind adapter's one-time setup, only once bound (a failed add
	// leaves the tracker's own service alone).
	s.setupApply(ctx, pid, tr, args, &res)
	if tr.Adapter == orchapi.AdapterNative {
		// Claims made before the binding become leases (they expire too).
		if n, err := s.adoptNative(ctx, pid, tr); err != nil {
			res.Notes = append(res.Notes, "could not turn existing native claims into leases: "+err.Error())
		} else if n > 0 {
			res.Notes = append(res.Notes, fmt.Sprintf("%d issues already claimed now hold leases (default TTL); their claimers renew by claiming again", n))
		}
	}
	return res, nil
}

// preflight checks what can be checked before binding: the adapter is
// installed and describes itself.
func (s *Service) preflight(pid string, tr api.Tracker) error {
	_, err := s.adapterPath(tr)
	return err
}

// verify runs the read-only conformance checks and the mirror's first full sync of a bound
// tracker at once (on a big tracker both list it), filling res; a sync still running after wait
// is not a failure. A read fails the write check, so it runs between cycles (or skips, noted).
func (s *Service) verify(ctx context.Context, pid string, tr api.Tracker, wait time.Duration, res *orchapi.TrackerResult) {
	start := time.Now()
	sy := s.m.syncer(pid, tr)
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel() // conform's hold runs on wctx
	checks := make(chan []orchapi.TrackerCheck, 1)
	go func() { checks <- s.conform(ctx, pid, tr, conformance.SlotHold(wctx, sy.busy)) }()
	var syncErr error
	if sy.view(time.Now()).state == orchapi.SyncLoading {
		syncErr = sy.waitLoaded(wctx) // the first full listing
	} else {
		syncErr = sy.wait(wctx) // a complete mirror (tracker test): one fresh cycle
	}
	res.SyncSeconds = time.Since(start).Seconds()
	res.Checks = <-checks
	s.adapterCheck(ctx, pid, tr, res)
	v := sy.view(time.Now())
	switch {
	case syncErr != nil && v.state == orchapi.SyncLoading && v.err == nil:
		res.Notes = append(res.Notes, fmt.Sprintf("the first sync is still running after %s; it goes on in the background (zm work sources)", wait.Round(time.Second)))
	case v.state == orchapi.SyncLoading && v.err != nil:
		res.Checks = append(res.Checks, check("first mirror sync", v.err))
	default:
		res.Synced = true
		res.Checks = append(res.Checks, check("first mirror sync", nil))
	}
	if db, err := s.d.Store.Project(authz.All(), authz.Read, pid); err == nil {
		res.Items, _ = mirrorCount(ctx, db, tr.Prefix)
	}
	res.OK = true
	for _, c := range res.Checks {
		res.OK = res.OK && (c.OK || c.Skipped)
		if c.Skipped && c.Name == conformance.CheckWritesName {
			res.Notes = append(res.Notes, c.Name+": "+c.Detail)
		}
	}
	res.Tracker = s.trackerView(ctx, pid, s.slugOf(pid), tr)
}

func check(name string, err error) orchapi.TrackerCheck {
	c := orchapi.TrackerCheck{Name: name, OK: err == nil}
	if err != nil {
		c.Detail = err.Error()
	}
	return c
}

// conform runs zm-work-conformance's read-only checks against the tracker's
// adapter (never a write verb), with its environment, config and Options.Hold.
func (s *Service) conform(ctx context.Context, pid string, tr api.Tracker, hold func(check func()) error) []orchapi.TrackerCheck {
	t, err := s.tracker(ctx, pid, tr)
	if err != nil {
		return []orchapi.TrackerCheck{check("adapter", err)}
	}
	rt := func(ctx context.Context, in []byte) (orchapi.AdapterResponse, error) {
		return adapter.ExecEnv(adapter.WithSecrets(ctx, t.secrets), t.path, t.env, in)
	}
	if tr.Adapter == orchapi.AdapterNative && orchcore.Issues(s.d) != nil { // in process: no executable to run
		rt = func(ctx context.Context, in []byte) (orchapi.AdapterResponse, error) {
			var req orchapi.AdapterRequest
			if err := json.Unmarshal(in, &req); err != nil {
				return orchapi.AdapterResponse{OK: false, Error: api.Errorf(api.CodeBadRequest, "malformed request: %v", err)}, nil
			}
			var out json.RawMessage
			if err := orchcore.Issues(s.d).NativeCall(ctx, pid, req, &out); err != nil {
				e, ok := err.(*api.Error)
				if !ok {
					e = api.Errorf(api.CodeInternal, "%v", err)
				}
				return orchapi.AdapterResponse{OK: false, Error: e}, nil
			}
			return orchapi.AdapterResponse{OK: true, Result: out}, nil
		}
	}
	var out []orchapi.TrackerCheck
	for _, r := range conformance.Run(ctx, rt, conformance.Options{Config: adapterConfig(tr.Config), Hold: hold}) {
		d := r.Detail
		for _, sec := range t.secrets {
			d = strings.ReplaceAll(d, sec, "***")
		}
		out = append(out, orchapi.TrackerCheck{Name: r.Name, OK: r.OK, Skipped: r.Skipped, Detail: d})
	}
	return out
}

// unbind removes the tracker from the project (if it is still this
// binding) and forgets its mirror and adapter process.
func (s *Service) unbind(ctx context.Context, pid string, tr api.Tracker) {
	_, err := orchcore.SetTrackers(ctx, s.d.Projects, pid, func(old []api.Tracker) ([]api.Tracker, error) {
		return slices.DeleteFunc(old, func(t api.Tracker) bool {
			return t.Prefix == tr.Prefix && orchapi.TrackerBinding(t) == orchapi.TrackerBinding(tr)
		}), nil
	})
	if err != nil {
		s.warn("tracker: unbind", "project", pid, "prefix", tr.Prefix, "err", err)
	}
	if err := s.m.forget(ctx, pid, tr.Prefix); err != nil {
		s.warn("tracker: forget mirror", "project", pid, "prefix", tr.Prefix, "err", err)
	}
	if t, err := s.tracker(ctx, pid, tr); err == nil && t.path != nativePath {
		s.pool.RetireEnv(t.path, t.env)
	}
}

// testTracker is POST /v0/trackers/{prefix}/test.
func (s *Service) testTracker(ctx context.Context, prefix string) (orchapi.TrackerResult, error) {
	pid, _, f, err := s.d.TrackerTarget(ctx)
	if err != nil {
		return orchapi.TrackerResult{}, err
	}
	tr, ok := orchapi.FindTracker(orchcore.ProjectTrackers(f), prefix)
	if !ok {
		return orchapi.TrackerResult{}, api.NotFound("tracker", prefix)
	}
	res := orchapi.TrackerResult{Checks: []orchapi.TrackerCheck{}}
	s.verify(ctx, pid, tr, defaultFirstSyncWait, &res)
	return res, nil
}

// removeTracker is DELETE /v0/trackers/{prefix}: the binding, its mirror,
// its adapter process and its stored password go.
func (s *Service) removeTracker(ctx context.Context, prefix string) (orchapi.RemoveTrackerResult, error) {
	pid, slug, f, err := s.d.TrackerTarget(ctx)
	if err != nil {
		return orchapi.RemoveTrackerResult{}, err
	}
	if !s.adds.Reserve(pid, prefix) {
		return orchapi.RemoveTrackerResult{}, orchapi.TrackerPrefixTaken(slug, prefix)
	}
	defer s.adds.Release(pid, prefix)
	if current, ok := s.d.Projects.File(pid); ok {
		f = current
	}
	tr, ok := orchapi.FindTracker(orchcore.ProjectTrackers(f), prefix)
	if !ok {
		return orchapi.RemoveTrackerResult{}, api.NotFound("tracker", prefix)
	}
	if err := orchcore.Homes(s.d).BlocksRemove(pid, prefix); err != nil {
		return orchapi.RemoveTrackerResult{}, err
	}
	out := orchapi.RemoveTrackerResult{Tracker: s.trackerView(ctx, pid, slug, tr)}
	out.Tracker.Sync = nil
	s.unbind(ctx, pid, tr)
	os.Remove(s.secretFile(pid, prefix))
	orchcore.Homes(s.d).Forget(pid, prefix)
	return out, nil
}

// trackerList is GET /v0/trackers.
func (s *Service) trackerList(ctx context.Context, sc authz.Scope) orchapi.TrackerList {
	out := orchapi.TrackerList{Trackers: []orchapi.TrackerView{}}
	for _, p := range s.d.Store.Projects(sc, authz.Read) {
		f, ok := s.d.Projects.File(p.ID)
		if !ok {
			continue
		}
		for _, tr := range orchcore.ProjectTrackers(f) {
			out.Trackers = append(out.Trackers, s.trackerView(ctx, p.ID, f.Slug, tr))
		}
	}
	return out
}

func (s *Service) slugOf(pid string) string {
	if f, ok := s.d.Projects.File(pid); ok {
		return f.Slug
	}
	return pid
}

// trackerView is a binding for its local owner: config without secrets
// (none are in config; the password is only a flag), mode, env and sync.
func (s *Service) trackerView(ctx context.Context, pid, slug string, tr api.Tracker) orchapi.TrackerView {
	v := orchapi.TrackerView{Project: pid, Slug: slug, Adapter: tr.Adapter, Prefix: tr.Prefix, Area: tr.Area,
		Mode: tr.Config["mode"], Env: orchapi.TrackerEnvAllowlist, Readonly: tr.Config["readonly"] == "true", Backup: orchcore.Homes(s.d).NewestCopy(pid, slug, tr.Prefix)}
	if tr.Env == api.TrackerEnvInherit {
		v.Env = api.TrackerEnvInherit
	}
	if len(tr.Config) > 0 {
		v.Config = map[string]string{}
		for k, val := range tr.Config {
			v.Config[k] = val
		}
	}
	v.Password = core.PathExists(s.secretFile(pid, tr.Prefix))
	if m := tr.Config["mode"]; m == orchapi.TrackerModeNew || m == orchapi.TrackerModeServer { // made by setup in zm's tracker dir
		v.Data = s.trackerDir(pid, tr.Prefix)
	}
	now := time.Now()
	sv := s.m.syncer(pid, tr).view(now)
	v.Sync = &orchapi.WorkSync{State: sv.state, Synced: sv.synced, FullSync: sv.fullSync, Mode: sv.mode, Serve: sv.desc.Serve}
	if !sv.synced.IsZero() {
		v.Sync.LagSeconds = now.Sub(sv.synced).Seconds()
	}
	if sv.err != nil {
		v.Sync.Error = sv.err.Error()
	}
	if !sv.failFrom.IsZero() {
		t := sv.failFrom.UTC()
		v.Sync.FailingSince, v.Sync.Failures = &t, sv.fails
	}
	if db, err := s.d.Store.Project(authz.All(), authz.Read, pid); err == nil {
		v.Sync.Items, _ = mirrorCount(ctx, db, tr.Prefix)
	}
	return v
}

// forget stops tracker prefix's syncer (waiting for it, so no page lands
// after the wipe) and deletes its mirror.
func (m *mirrors) forget(ctx context.Context, project, prefix string) error {
	return m.forgetAnnounce(ctx, project, prefix, "")
}

// forgetAnnounce is forget that, for a non-empty binding, leaves the wiped
// mirror owing an announced full listing, durably (mirrorstate.Wipe).
func (m *mirrors) forgetAnnounce(ctx context.Context, project, prefix, binding string) error {
	key := orchapi.TrackerKey(project, prefix)
	m.mu.Lock()
	sy := m.by[key]
	if sy != nil {
		sy.stop()
		delete(m.by, key)
	}
	m.mu.Unlock()
	if sy != nil {
		<-sy.done
	}
	db, err := m.s.d.Store.Project(authz.All(), authz.Read, project)
	if err != nil {
		return err
	}
	// Waiting released m.mu: a new binding may now own this prefix. Never
	// wipe its pages, and keep the lock through the wipe to fence new starts.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.by[key] != nil {
		return nil
	}
	return mirrorstate.Wipe(ctx, db, prefix, binding)
}

// adapterCheck runs the adapter's optional check verb (SPEC 11) and adds
// its checks and notes, and the tracker CLI it reports running exactly as
// the adapter runs it (a CLI that does not answer is reported, not refused).
func (s *Service) adapterCheck(ctx context.Context, pid string, tr api.Tracker, res *orchapi.TrackerResult) {
	t, err := s.tracker(ctx, pid, tr)
	if n := adapter.Describe(t.desc.Env); err == nil && n != "" {
		res.Notes = append(res.Notes, n)
	}
	if err != nil || !t.has(orchapi.VerbCheck) {
		return
	}
	var out orchapi.AdapterCheckResult
	if err := s.call(ctx, t, orchapi.VerbCheck, "", nil, &out); err != nil {
		res.Checks = append(res.Checks, check("the adapter's own checks", err))
		return
	}
	res.Checks = append(res.Checks, out.Checks...)
	res.Notes = append(res.Notes, out.Notes...)
	if out.CLI == nil {
		return
	}
	name := filepath.Base(out.CLI.Bin) + " runs as the adapter runs it"
	if out.CLI.Error != "" {
		res.Checks = append(res.Checks, orchapi.TrackerCheck{Name: name + " (" + out.CLI.Bin + " version)", Skipped: true, Detail: out.CLI.Error})
		return
	}
	res.Checks = append(res.Checks, orchapi.TrackerCheck{Name: name, OK: true, Detail: out.CLI.Bin + " -> " + out.CLI.Version})
}
