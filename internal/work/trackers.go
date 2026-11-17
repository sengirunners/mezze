package work

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/mezze/internal/work/clean"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// describeTTL bounds how stale a cached describe answer may be (an adapter
// replaced on disk is described again at once: the cache keys on mtime).
var describeTTL = 30 * time.Second

type described struct {
	mod  time.Time
	at   time.Time
	desc orchapi.AdapterDescribe
	err  error
}

type adapterCache struct {
	mu sync.Mutex
	m  map[string]described
}

// adapterPath finds zm-work-<tracker.Adapter> (config work_adapters first, then PATH).
func (s *Service) adapterPath(tr api.Tracker) (string, error) {
	if tr.Adapter == orchapi.AdapterNative {
		if orchcore.Issues(s.d) == nil {
			return "", api.Errorf(api.CodeUnavailable, "native issues are not available in this daemon")
		}
		return nativePath, nil
	}
	var extra []string
	if s.d.Config != nil {
		extra = s.d.Config.File.WorkAdapters
	}
	p, err := adapter.Find(tr.Adapter, extra)
	if err != nil {
		return "", api.Errorf(api.CodeUnavailable, "work adapter %q is not installed: %v", tr.Adapter, err).
			WithHint("put " + adapter.ExecPrefix + tr.Adapter + " on the daemon's PATH, or list it in config.json work_adapters")
	}
	return p, nil
}

// describe returns the adapter's describe answer, checked: same protocol
// major, a valid prefix, every required verb, a describe.env zm can honour.
// It runs with env (nil: inherit, for env = "inherit"), so an adapter that
// needs a variable from the daemon's environment can describe itself; the
// cache keeps inherited and base answers apart.
func (s *Service) describe(ctx context.Context, path string, env []string) (orchapi.AdapterDescribe, error) {
	var mod time.Time
	if st, err := os.Stat(path); err == nil {
		mod = st.ModTime()
	}
	key := path
	if env == nil {
		key += "\x00inherit"
	}
	s.ads.mu.Lock()
	c, ok := s.ads.m[key]
	s.ads.mu.Unlock()
	if ok && c.mod.Equal(mod) && time.Since(c.at) < describeTTL {
		return c.desc, c.err
	}
	dctx, cancel := context.WithTimeout(ctx, adapter.Timeout)
	defer cancel()
	var d orchapi.AdapterDescribe
	err := adapter.CallEnv(dctx, path, env, orchapi.AdapterRequest{Verb: orchapi.VerbDescribe}, &d)
	if err == nil {
		switch {
		case !adapter.SameMajor(d.Spec):
			err = api.Errorf(api.CodeUnavailable, "adapter %s speaks %q, this daemon %s", d.Name, d.Spec, api.AdapterProtocol).
				WithHint("install the adapter version that matches this zm")
		case !orchapi.ValidTrackerPrefix(d.Prefix):
			err = api.Errorf(api.CodeUnavailable, "adapter %s has a bad prefix %q", d.Name, d.Prefix)
		default:
			var missing []string
			for _, v := range orchapi.RequiredVerbs {
				if !slices.Contains(d.Verbs, v) {
					missing = append(missing, v)
				}
			}
			if len(missing) > 0 {
				err = api.Errorf(api.CodeUnavailable, "adapter %s lacks required verbs: %s", d.Name, strings.Join(missing, ", "))
			} else if e := adapter.CheckEnv(d.Env); e != nil {
				err = api.Errorf(api.CodeUnavailable, "adapter %s: %v", d.Name, e)
			}
		}
	}
	if ctx.Err() == nil { // never cache a failure caused by the caller going away
		s.ads.mu.Lock()
		s.ads.m[key] = described{mod: mod, at: time.Now(), desc: d, err: err}
		s.ads.mu.Unlock()
	}
	return d, err
}

// tracked is a tracker bound to a project, ready to call.
type tracked struct {
	project string
	tr      api.Tracker
	path    string
	desc    orchapi.AdapterDescribe
	env     []string // the adapter's environment (nil: inherit); see adapterEnv
	secrets []string // values in env never relayed in an error
}

// nativePath stands in for the executable of the built-in native adapter.
const nativePath = "(built-in native issues)"

func (s *Service) tracker(ctx context.Context, project string, tr api.Tracker) (tracked, error) {
	t := tracked{project: project, tr: tr}
	t.tr.Config = adapterConfig(tr.Config)
	if tr.Adapter == orchapi.AdapterNative {
		// Native issues: the issues module answers in process (SPEC "Native issues").
		if orchcore.Issues(s.d) == nil {
			return t, api.Errorf(api.CodeUnavailable, "native issues are not available in this daemon")
		}
		t.path, t.desc = nativePath, orchcore.Issues(s.d).NativeDescribe()
		return t, nil
	}
	var err error
	if t.path, err = s.adapterPath(tr); err != nil {
		return t, err
	}
	// describe runs with the base (inherit: the daemon's) and declares the rest.
	base, _, _ := s.adapterEnv(project, tr, nil)
	if t.desc, err = s.describe(ctx, t.path, base); err != nil {
		return t, err
	}
	t.env, t.secrets, err = s.adapterEnv(project, tr, t.desc.Env)
	return t, err
}

func (t tracked) has(verb string) bool { return slices.Contains(t.desc.Verbs, verb) }

// toAdapter rewrites an id from the project's prefix to the adapter's own.
func (t tracked) toAdapter(id string) string {
	if ref, ok := strings.CutPrefix(id, t.tr.Prefix+":"); ok {
		return t.desc.Prefix + ":" + ref
	}
	return id
}

// call runs one verb. id is in the project's prefix; results are fixed up by the caller (fix).
func (s *Service) call(ctx context.Context, t tracked, verb, id string, args, out any) error {
	var raw json.RawMessage
	if args != nil {
		raw, _ = json.Marshal(args)
	}
	req := orchapi.AdapterRequest{Verb: verb, Args: raw, Config: t.tr.Config}
	if slices.Contains(orchapi.WriteVerbs, verb) {
		req.Actor = s.actorOf(ctx, args)
	}
	if id != "" {
		req.ID = t.toAdapter(id)
	}
	return s.send(ctx, t, req, out)
}

// callAs runs one verb with an explicit actor (setup: the identity adding
// the tracker).
func (s *Service) callAs(ctx context.Context, t tracked, verb, actor string, args, out any) error {
	var raw json.RawMessage
	if args != nil {
		raw, _ = json.Marshal(args)
	}
	return s.send(ctx, t, orchapi.AdapterRequest{Verb: verb, Args: raw, Config: t.tr.Config, Actor: actor}, out)
}

// send delivers req to t's adapter: in process, over serve mode, or one-shot.
func (s *Service) send(ctx context.Context, t tracked, req orchapi.AdapterRequest, out any) error {
	if t.tr.Adapter == orchapi.AdapterNative && orchcore.Issues(s.d) != nil {
		return orchcore.Issues(s.d).NativeCall(ctx, t.project, req, out)
	}
	if t.desc.Serve {
		return clean.Err(s.pool.CallEnv(adapter.WithSecrets(ctx, t.secrets), t.path, t.env, req, out), t.secrets)
	}
	return clean.Err(adapter.CallEnv(adapter.WithSecrets(ctx, t.secrets), t.path, t.env, req, out), t.secrets)
}

// fix normalizes an adapter item and assigns its project, source and area.
func (s *Service) fix(t tracked, it *orchapi.WorkItem) {
	clean.Item(it)
	if ref, ok := strings.CutPrefix(it.ID, t.desc.Prefix+":"); ok {
		it.ID = t.tr.Prefix + ":" + ref
	}
	if a, err := api.ParseAddress(it.ID); err == nil && a.Kind == api.KindWork {
		it.ID = canonID(a)
	}
	it.Source, it.Project = t.tr.Prefix, t.project
	if t.tr.Adapter != orchapi.AdapterNative { // a native issue's area is the store's (its rules, else where it was filed)
		it.Area = orchcore.AreaForItem(s.d.Projects, t.project, it.ID, it.Labels)
	}
	if !orchapi.ValidStatus(it.Status) {
		it.Status = orchapi.WorkOpen
	}
	it.Comments = nil // zm's own history is the item's comments
}

// adapterErr shapes an adapter failure for sub: a missing item is exactly
// api.NotFound; network and token callers never see adapter internals.
func (s *Service) adapterErr(sub *authz.Subject, t tracked, id string, err error) error {
	if err == nil {
		return nil
	}
	if api.IsCode(err, api.CodeNotFound) && id != "" {
		return api.NotFound("work item", id)
	}
	e, ok := err.(*api.Error)
	if !ok {
		e = api.Errorf(api.CodeUnavailable, "the %s tracker failed", t.tr.Adapter)
	}
	if !localCaller(sub) {
		c := *e
		c.Hint, c.Data = "", nil
		if c.Code == api.CodeUnavailable || c.Code == api.CodeInternal {
			c.Message = fmt.Sprintf("the %s tracker is unavailable", t.tr.Adapter)
		}
		return &c
	}
	return e
}

// localCaller is the machine owner on the socket (adapter paths and raw
// adapter errors are for them only).
func localCaller(sub *authz.Subject) bool {
	return sub != nil && (sub.Via == authz.ViaLocal || sub.Via == authz.ViaSystem) && !sub.Remote
}

// trackerVisible reports whether sub may see tracker tr of project at all:
// a project-wide reader, a reader of the tracker's area, or a reader of an
// area the tracker serves (orchcore.AreaServes: labels, or a glob naming the
// prefix). The area view (project show p/a) uses the same rule.
func (s *Service) trackerVisible(sc authz.Scope, p string, tr api.Tracker, f project.File) bool {
	if sc.WholeProject(p) {
		return true
	}
	if tr.Area != "" && sc.Can(authz.Read, authz.Object{Project: p, Area: tr.Area}) {
		return true
	}
	for _, a := range f.Areas {
		if !sc.Can(authz.Read, authz.Object{Project: p, Area: a.Name}) {
			continue
		}
		if orchcore.AreaServes(a, tr) {
			return true
		}
	}
	return false
}

// expandConfig resolves a leading ~ in config values against the daemon's
// home: an adapter's tracker CLI may run with the tracker's own HOME
// (beads), where ~ would name the wrong directory. A config without ~ is returned as is
// (binding fingerprints it); otherwise it is copied, never changed.
// zmKeys are binding keys zm keeps for itself (SPEC 17.9: "home" says which
// server zm runs); adapters never see them (beads refuses unknown keys).
var zmKeys = []string{"home"}

// adapterConfig is the config an adapter gets: zm's own keys left out, ~
// expanded.
func adapterConfig(cfg map[string]string) map[string]string {
	for _, k := range zmKeys {
		if _, ok := cfg[k]; ok {
			c := maps.Clone(cfg)
			for _, k := range zmKeys {
				delete(c, k)
			}
			return expandConfig(c)
		}
	}
	return expandConfig(cfg)
}

func expandConfig(cfg map[string]string) map[string]string {
	var out map[string]string
	for k, v := range cfg {
		if e := core.ExpandHome(v); e != v {
			if out == nil {
				out = maps.Clone(cfg)
			}
			out[k] = e
		}
	}
	if out == nil {
		return cfg
	}
	return out
}
