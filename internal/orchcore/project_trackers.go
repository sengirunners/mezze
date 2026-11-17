package orchcore

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/project"
)

// project.toml's guest_issues and [[trackers]] are zm's (recipe
// step 6): Potluck's project registry stores, validates and writes them
// through the hooks registered here, and asks projectExtras for the tracker
// and guest_issues fields of its project and area answers and of its update
// request. A Potluck-only daemon keeps both names in the file untouched.
//
//	guest_issues = true
//
//	[[trackers]]
//	adapter = "beads"
//	prefix = "bd"
//	config = { dir = "~/code/acme/.beads" }
//
// This file also has the area and label rules that read a project's
// trackers. They live in orchcore, not internal/work, because issues, homes
// and ghlink read them too and a module imports no other module.
const (
	trackersSection = "trackers"
	guestIssuesKey  = "guest_issues"
)

func init() {
	project.RegisterKey(guestIssuesKey, guestIssues{})
	project.RegisterSection(trackersSection, trackers{})
	project.RegisterExtras(project.Extras{Project: projectExtras, Area: areaExtras, Update: updateExtras})
}

// ProjectTrackers are f's tracker bindings. Read-only.
func ProjectTrackers(f project.File) []api.Tracker {
	trs, _ := f.Section(trackersSection).([]api.Tracker)
	return trs
}

// GuestIssues reports whether f's project lets its guests write native issues.
func GuestIssues(f project.File) bool {
	on, _ := f.Key(guestIssuesKey).(bool)
	return on
}

// guestIssues owns the top-level key guest_issues: true, or absent.
type guestIssues struct{}

func (guestIssues) Parse(where string, raw any) (any, error) {
	on, ok := raw.(bool)
	if !ok {
		return nil, fmt.Errorf("%s: %s must be true or false", where, guestIssuesKey)
	}
	if !on {
		return nil, nil
	}
	return true, nil
}

func (guestIssues) Validate(any, project.File) error { return nil }

func (guestIssues) Encode(v any) string {
	if on, _ := v.(bool); on {
		return guestIssuesKey + " = true\n"
	}
	return ""
}

// trackers owns [[trackers]]: a []api.Tracker.
type trackers struct{}

func (trackers) Parse(where string, raw any) (any, error) {
	ts, ok := raw.([]map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: %s must be [[%s]] tables", where, trackersSection, trackersSection)
	}
	var out []api.Tracker
	for i, t := range ts {
		tr := project.NewTOMLReader(fmt.Sprintf("%s [[trackers]] #%d", where, i+1), t)
		out = append(out, api.Tracker{Adapter: tr.Str("adapter"), Prefix: tr.Str("prefix"),
			Area: tr.Str("area"), Config: tr.StrMap("config"), Env: tr.Str("env")})
		if err := tr.Done(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (trackers) Validate(v any, f project.File) error {
	areas := map[string]bool{}
	for _, a := range f.Areas {
		areas[a.Name] = true
	}
	prefixes := map[string]bool{}
	trs, _ := v.([]api.Tracker)
	for _, t := range trs {
		if t.Adapter == "" || t.Prefix == "" || t.Prefix == "local" {
			return api.Errorf(api.CodeBadRequest, "tracker needs adapter and a prefix other than local")
		}
		if prefixes[t.Prefix] {
			return api.Errorf(api.CodeConflict, "tracker prefix %q used twice", t.Prefix)
		}
		prefixes[t.Prefix] = true
		if t.Area != "" && !areas[t.Area] {
			return api.Errorf(api.CodeBadRequest, "tracker %q names unknown area %q", t.Prefix, t.Area)
		}
		if t.Env != "" && t.Env != api.TrackerEnvInherit {
			return api.Errorf(api.CodeBadRequest, "tracker %q: env must be %q or absent", t.Prefix, api.TrackerEnvInherit)
		}
		if t.Env == api.TrackerEnvInherit && t.Adapter == "beads" && t.Config["mode"] == orchapi.TrackerModeServer {
			// The shell's BEADS_*/DOLT_* would decide which server bd reaches (SPEC 17.2).
			return api.Errorf(api.CodeBadRequest, "tracker %q: env inherit is refused for a beads server binding", t.Prefix)
		}
	}
	return nil
}

func (trackers) Encode(v any) string {
	var b strings.Builder
	trs, _ := v.([]api.Tracker)
	for _, t := range trs {
		fmt.Fprintf(&b, "\n[[trackers]]\nadapter = %s\nprefix = %s\n", project.TOMLString(t.Adapter), project.TOMLString(t.Prefix))
		if t.Area != "" {
			fmt.Fprintf(&b, "area = %s\n", project.TOMLString(t.Area))
		}
		if len(t.Config) > 0 {
			keys := make([]string, 0, len(t.Config))
			for k := range t.Config {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for i, k := range keys {
				keys[i] = project.TOMLKey(k) + " = " + project.TOMLString(t.Config[k])
			}
			fmt.Fprintf(&b, "config = { %s }\n", strings.Join(keys, ", "))
		}
		if t.Env != "" {
			fmt.Fprintf(&b, "env = %s\n", project.TOMLString(t.Env))
		}
	}
	return b.String()
}

// projectExtras adds guest_issues and the trackers sc may see to a project
// view: area guests see their areas' trackers, and of a project-level
// tracker that serves one of their areas only the adapter and prefix.
func projectExtras(p *api.Project, f project.File, sc authz.Scope) {
	p.GuestIssues = GuestIssues(f)
	whole, areas := sc.Areas(p.ID)
	vis := map[string]bool{}
	for _, a := range areas {
		vis[a] = true
	}
	for _, t := range ProjectTrackers(f) {
		switch {
		case whole || (t.Area != "" && vis[t.Area]):
			p.Trackers = append(p.Trackers, t)
		case t.Area == "" && servesVisible(f, vis, t):
			p.Trackers = append(p.Trackers, sharedTracker(t))
		}
	}
}

// servesVisible reports whether a project-level tracker serves one of the
// areas in vis (an area guest's view of the project).
func servesVisible(f project.File, vis map[string]bool, t api.Tracker) bool {
	for _, a := range f.Areas {
		if vis[a.Name] && AreaServes(a, t) {
			return true
		}
	}
	return false
}

// sharedTracker is a project-level tracker as an area-only caller sees it:
// which adapter and prefix serve the area, not the project's adapter config.
func sharedTracker(t api.Tracker) api.Tracker {
	return api.Tracker{Adapter: t.Adapter, Prefix: t.Prefix}
}

// areaExtras adds to an area view the trackers that serve the area, as sc
// may see them.
func areaExtras(v *api.AreaView, f project.File, sc authz.Scope) {
	whole := sc.WholeProject(v.Project.ID)
	for _, t := range ProjectTrackers(f) {
		// The area's own trackers, and project-level ones whose items reach
		// it (labels, a glob naming the prefix): the acme/api case. Same
		// rule as work's tracker visibility, so work sources agrees.
		switch {
		case t.Area == v.Name || (t.Area == "" && whole):
			v.Trackers = append(v.Trackers, t)
		case t.Area == "" && AreaServes(v.Area, t):
			v.Trackers = append(v.Trackers, sharedTracker(t))
		}
	}
}

// updateExtras applies PATCH /v0/projects/{project}'s guest_issues (admin
// on the project, which the registry has checked) and trackers (the
// machine's owner only: they name what the daemon runs and reads here).
func updateExtras(f project.File, req api.UpdateProjectRequest, owner bool) (project.File, error) {
	// Repo rules are the registry's own field and it refuses them itself;
	// they are named here only so the refusal keeps the words it had when
	// one check covered both.
	if (req.Repos != nil || req.Trackers != nil) && !owner {
		return f, project.LocalSettingsError("repo rules and trackers")
	}
	if req.GuestIssues != nil {
		var v any
		if *req.GuestIssues {
			v = true
		}
		f = f.WithKey(guestIssuesKey, v)
	}
	if req.Trackers != nil {
		f = f.WithSection(trackersSection, trackersValue(*req.Trackers))
	}
	return f, nil
}

// trackersValue is trs as the section's value: a copy, nil (no section)
// for none.
func trackersValue(trs []api.Tracker) any {
	if len(trs) == 0 {
		return nil
	}
	return append([]api.Tracker(nil), trs...)
}

// SetTrackers replaces project id's trackers with what fn returns, given
// the current ones, and rewrites project.toml: applied at once, no restart
// (zm tracker add/rm, SPEC 17.2). fn runs under the registry lock, so
// two concurrent changes never lose one another; an error from fn (or an
// invalid result) changes nothing. Daemon-internal: the caller has checked
// that the machine's owner asked (tracker routes are local).
func SetTrackers(ctx context.Context, r *project.Registry, id string, fn func(old []api.Tracker) ([]api.Tracker, error)) ([]api.Tracker, error) {
	var out []api.Tracker
	_, err := r.SetSection(ctx, id, trackersSection, func(old any) (any, error) {
		cur, _ := old.([]api.Tracker)
		trs, err := fn(append([]api.Tracker(nil), cur...))
		if err != nil {
			return nil, err
		}
		out = append([]api.Tracker(nil), trs...)
		return trackersValue(trs), nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Tracker returns the tracker for an id prefix in project id (daemon-internal).
func Tracker(r *project.Registry, id, prefix string) (api.Tracker, bool) {
	f, _ := r.File(id)
	for _, t := range ProjectTrackers(f) {
		if t.Prefix == prefix {
			return t, true
		}
	}
	return api.Tracker{}, false
}

// AreaForWork is the area a work item id belongs to in project id: the first
// area whose work globs match, else the tracker's area, else "".
func AreaForWork(r *project.Registry, id, workID string) string {
	f, ok := r.File(id)
	if !ok {
		return ""
	}
	for _, a := range f.Areas {
		for _, g := range a.Work {
			if project.Glob(g, workID) {
				return a.Name
			}
		}
	}
	return trackerArea(f, workID)
}

// trackerArea is the area of the tracker whose prefix workID has, else "".
func trackerArea(f project.File, workID string) string {
	prefix, _, _ := strings.Cut(workID, ":")
	for _, t := range ProjectTrackers(f) {
		if t.Prefix == prefix {
			return t.Area
		}
	}
	return ""
}

// AreaForItem is the area of an adapter work item with these labels: the
// first area whose work globs match its id, else the first area sharing a
// label with it, else the tracker's area, else "".
func AreaForItem(r *project.Registry, id, workID string, labels []string) string {
	f, ok := r.File(id)
	if !ok {
		return ""
	}
	for _, a := range f.Areas {
		for _, g := range a.Work {
			if project.Glob(g, workID) {
				return a.Name
			}
		}
	}
	for _, a := range f.Areas {
		for _, l := range a.Labels {
			if slices.Contains(labels, l) {
				return a.Name
			}
		}
	}
	return trackerArea(f, workID)
}

// AreaNeedsLabels reports whether the area of workID in project id depends
// on the item's labels: some area has label rules and no work glob matches.
func AreaNeedsLabels(r *project.Registry, id, workID string) bool {
	f, ok := r.File(id)
	if !ok {
		return false
	}
	labels := false
	for _, a := range f.Areas {
		for _, g := range a.Work {
			if project.Glob(g, workID) {
				return false
			}
		}
		labels = labels || len(a.Labels) > 0
	}
	return labels
}

// ListLabels is the label filter an adapter list may apply for a caller who
// can only see these areas of project id: the union of their labels, when
// every one of them has labels and none is the tracker's own area. plain:
// some also match by work glob, so an unfiltered list is needed too. nil
// labels means no filter can be pushed down.
func ListLabels(r *project.Registry, id, trackerArea string, areas []string) (labels []string, plain bool) {
	f, found := r.File(id)
	if !found || len(areas) == 0 {
		return nil, false
	}
	for _, name := range areas {
		if name == "" || name == trackerArea {
			return nil, false
		}
		var area *api.Area
		for i := range f.Areas {
			if f.Areas[i].Name == name {
				area = &f.Areas[i]
			}
		}
		if area == nil || len(area.Labels) == 0 {
			return nil, false
		}
		plain = plain || len(area.Work) > 0
		for _, l := range area.Labels {
			if !slices.Contains(labels, l) {
				labels = append(labels, l)
			}
		}
	}
	return labels, plain
}

// AreaServes reports whether items of tracker tr can land in area a: the
// tracker is the area's own, the area takes items by label (labels can put
// any tracker's items in it), or one of its work globs can match an id with
// the tracker's prefix. Work's tracker visibility and the area view share it.
func AreaServes(a api.Area, tr api.Tracker) bool {
	if tr.Area != "" && tr.Area == a.Name {
		return true
	}
	if len(a.Labels) > 0 {
		return true
	}
	p := tr.Prefix + ":"
	for _, g := range a.Work {
		lit, _, wild := strings.Cut(g, "*")
		if strings.HasPrefix(lit, p) || (wild && strings.HasPrefix(p, lit)) {
			return true
		}
	}
	return false
}
