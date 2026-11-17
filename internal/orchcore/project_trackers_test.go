package orchcore

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

// These tests came from potluck's kit/project with the code (recipe
// step 6): the tracker and guest_issues cases of its project_test.go and
// trackers_test.go, now through the hooks orchcore registers.

type projFixture struct {
	ctx          context.Context
	reg          *project.Registry
	st           *store.Store
	cfg          *config.Config
	local        *authz.Subject
	orchid, side api.Project
}

func newProjFixture(t *testing.T) *projFixture {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	st, err := storetest.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &projFixture{ctx: ctx, st: st, cfg: &config.Config{Home: home, Human: "judy", Machine: "mini"}}
	if f.reg, err = project.Open(ctx, st, f.cfg); err != nil {
		t.Fatal(err)
	}
	f.local = f.reg.Local("")
	if f.orchid, err = f.reg.Create(ctx, f.local, api.NewProjectRequest{Slug: "orchid", Name: "Orchid"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"sprout", "billing"} {
		if _, err := f.reg.AddArea(ctx, f.local, "orchid", api.Area{Name: a, Work: []string{"bd:" + a + "-*"}}); err != nil {
			t.Fatal(err)
		}
	}
	if f.side, err = f.reg.Create(ctx, f.local, api.NewProjectRequest{Slug: "side"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *projFixture) path(id string) string {
	return filepath.Join(f.st.ProjectDir(id), project.FileName)
}

func (f *projFixture) reopen(t *testing.T) *project.Registry {
	t.Helper()
	r, err := project.Open(f.ctx, f.st, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

const tomlHead = "# zm project config. Change it with `zm project`/`zm area`; hand edits need a daemon restart.\n"

// The bytes the base wrote for guest_issues and [[trackers]] before zm
// owned them: zm writes the same, so no project.toml changes on upgrade.
const trackersToml = tomlHead + `id = "01JAAAAAAAAAAAAAAAAAAAAAAA"
slug = "orchid"
name = "on \"Call\""
paths = ["~/code/Orchid"]
remotes = ["github.com/acme/Orchid"]
guest_issues = true

[[areas]]
name = "sprout"
paths = ["~/code/Orchid/sprout"]
work = ["bd:sprout-*"]
labels = ["sprout"]

[[trackers]]
adapter = "beads"
prefix = "bd"
area = "sprout"
config = { dir = "~/x", "odd key" = "a#b" }
env = "inherit"

[[trackers]]
adapter = "native"
prefix = "issue"
`

func TestProjectFileRoundTrip(t *testing.T) {
	f, err := project.ParseFile(trackersToml)
	if err != nil {
		t.Fatal(err)
	}
	want := []api.Tracker{{Adapter: "beads", Prefix: "bd", Area: "sprout", Config: map[string]string{"dir": "~/x", "odd key": "a#b"}, Env: "inherit"},
		{Adapter: "native", Prefix: "issue"}}
	if !GuestIssues(f) || !reflect.DeepEqual(ProjectTrackers(f), want) {
		t.Fatalf("parsed %v %+v", GuestIssues(f), ProjectTrackers(f))
	}
	if out := f.Encode(); out != trackersToml {
		t.Fatalf("zm no longer writes the bytes the base wrote:\n%s", out)
	}
	// What a Potluck-only daemon writes back (it knows neither name: each
	// table's keys by name) reads the same, and zm's next write is zm's form.
	unowned := strings.Replace(trackersToml, "adapter = \"beads\"\nprefix = \"bd\"\narea = \"sprout\"\nconfig = { dir = \"~/x\", \"odd key\" = \"a#b\" }\nenv = \"inherit\"\n",
		"adapter = \"beads\"\narea = \"sprout\"\nconfig = { dir = \"~/x\", \"odd key\" = \"a#b\" }\nenv = \"inherit\"\nprefix = \"bd\"\n", 1)
	if unowned == trackersToml {
		t.Fatal("test: no rewrite")
	}
	g, err := project.ParseFile(unowned)
	if err != nil || !reflect.DeepEqual(g, f) || g.Encode() != trackersToml {
		t.Fatalf("a Potluck-only write: %v\n%+v", err, g)
	}
	// guest_issues = false is the same as absent.
	h, err := project.ParseFile("slug = \"x\"\nguest_issues = false\n")
	if err != nil || GuestIssues(h) || strings.Contains(h.Encode(), "guest_issues") {
		t.Fatalf("guest_issues = false: %v\n%s", err, h.Encode())
	}
	for bad, want := range map[string]string{
		"slug = \"x\"\nguest_issues = 1\n":                                                                                     "project.toml: guest_issues must be true or false",
		"slug = \"x\"\ntrackers = 1\n":                                                                                         "project.toml: trackers must be [[trackers]] tables",
		"slug = \"x\"\n[[trackers]]\nadapter = \"a\"\nprefix = \"p\"\nbogus = 1\n":                                             "project.toml [[trackers]] #1: unknown keys bogus",
		"slug = \"x\"\n[[trackers]]\nadapter = \"a\"\n":                                                                        "tracker needs adapter and a prefix other than local",
		"slug = \"x\"\n[[trackers]]\nadapter = \"a\"\nprefix = \"local\"\n":                                                    "tracker needs adapter and a prefix other than local",
		"slug = \"x\"\n[[trackers]]\nadapter = \"a\"\nprefix = \"p\"\narea = \"gone\"\n":                                       `tracker "p" names unknown area "gone"`,
		"slug = \"x\"\n[[trackers]]\nadapter = \"a\"\nprefix = \"p\"\nenv = \"all\"\n":                                         `tracker "p": env must be "inherit" or absent`,
		"slug = \"x\"\n[[trackers]]\nadapter = \"a\"\nprefix = \"p\"\n[[trackers]]\nadapter = \"b\"\nprefix = \"p\"\n":         `tracker prefix "p" used twice`,
		"slug = \"x\"\n[[trackers]]\nadapter = \"beads\"\nprefix = \"p\"\nenv = \"inherit\"\nconfig = { mode = \"server\" }\n": `tracker "p": env inherit is refused for a beads server binding`,
	} {
		if _, err := project.ParseFile(bad); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", bad, err, want)
		}
	}
}

// SetTrackers applies at once and rewrites project.toml; a bad result
// (or an error from fn) changes nothing; env round-trips.
func TestSetTrackers(t *testing.T) {
	fx := newProjFixture(t)
	ctx, r, p := fx.ctx, fx.reg, fx.side
	add := func(tr api.Tracker) error {
		_, err := SetTrackers(ctx, r, p.ID, func(old []api.Tracker) ([]api.Tracker, error) { return append(old, tr), nil })
		return err
	}
	if err := add(api.Tracker{Adapter: "beads", Prefix: "bd", Config: map[string]string{"mode": "new"}, Env: "inherit"}); err != nil {
		t.Fatal(err)
	}
	f, _ := r.File(p.ID)
	if trs := ProjectTrackers(f); len(trs) != 1 || trs[0].Env != "inherit" {
		t.Fatalf("live: %+v", trs)
	}
	if tr, ok := Tracker(r, p.ID, "bd"); !ok || tr.Adapter != "beads" {
		t.Fatalf("Tracker: %+v %v", tr, ok)
	}
	if _, ok := Tracker(r, p.ID, "zz"); ok {
		t.Fatal("Tracker found an unbound prefix")
	}
	if _, ok := Tracker(r, "nope", "bd"); ok {
		t.Fatal("Tracker found one in a missing project")
	}
	b, _ := os.ReadFile(fx.path(p.ID))
	pf, err := project.ParseFile(string(b))
	if err != nil || len(ProjectTrackers(pf)) != 1 || ProjectTrackers(pf)[0].Env != "inherit" || !strings.Contains(string(b), `env = "inherit"`) {
		t.Fatalf("project.toml: %v\n%s", err, b)
	}
	for _, bad := range []api.Tracker{
		{Adapter: "beads", Prefix: "bd"}, // prefix twice
		{Adapter: "x", Prefix: "x", Env: "all"},
		{Adapter: "beads", Prefix: "sv", Env: "inherit", Config: map[string]string{"mode": "server"}},
	} {
		if err := add(bad); !api.IsCode(err, api.CodeBadRequest) && !api.IsCode(err, api.CodeConflict) {
			t.Errorf("%+v accepted: %v", bad, err)
		}
	}
	if _, err := SetTrackers(ctx, r, p.ID, func([]api.Tracker) ([]api.Tracker, error) { return nil, api.Errorf(api.CodeConflict, "no") }); err == nil {
		t.Fatal("fn's error ignored")
	}
	if f, _ := r.File(p.ID); len(ProjectTrackers(f)) != 1 {
		t.Fatalf("a refused change applied: %+v", ProjectTrackers(f))
	}
	if _, err := SetTrackers(ctx, r, "nope", func(o []api.Tracker) ([]api.Tracker, error) { return o, nil }); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing project: %v", err)
	}
	// Removing the last one removes the section.
	if got, err := SetTrackers(ctx, r, p.ID, func([]api.Tracker) ([]api.Tracker, error) { return nil, nil }); err != nil || len(got) != 0 {
		t.Fatalf("rm: %+v %v", got, err)
	}
	if b, _ := os.ReadFile(fx.path(p.ID)); strings.Contains(string(b), "trackers") {
		t.Fatalf("section left behind:\n%s", b)
	}
}

// A hand edit made since the daemon loaded project.toml is never silently
// discarded by a tracker add: it is refused with a conflict naming the
// restart, and after a restart it applies and keeps the edit.
func TestSetTrackersKeepsHandEdit(t *testing.T) {
	fx := newProjFixture(t)
	path := fx.path(fx.side.ID)
	b, _ := os.ReadFile(path)
	edited := string(b) + "\n[[areas]]\nname = \"byhand\"\n"
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	add := func(r *project.Registry, prefix string) error {
		_, err := SetTrackers(fx.ctx, r, fx.side.ID, func(old []api.Tracker) ([]api.Tracker, error) {
			return append(old, api.Tracker{Adapter: "beads", Prefix: prefix, Config: map[string]string{"mode": "new"}}), nil
		})
		return err
	}
	if err := add(fx.reg, "bd2"); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "restart") {
		t.Fatalf("tracker add over a hand edit: %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != edited {
		t.Fatalf("hand edit lost:\n%s", got)
	}
	if f, _ := fx.reg.File(fx.side.ID); len(ProjectTrackers(f)) != 0 {
		t.Fatalf("refused change applied in memory: %+v", ProjectTrackers(f))
	}
	r := fx.reopen(t)
	if err := add(r, "bd2"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), `name = "byhand"`) || !strings.Contains(string(got), `prefix = "bd2"`) {
		t.Fatalf("after restart:\n%s", got)
	}
	if err := add(r, "bd3"); err != nil { // the daemon's own write is not a hand edit
		t.Fatal(err)
	}
}

// PATCH /v0/projects/{project} sets guest_issues (admin) and trackers (the
// machine's owner on its socket only); both survive a restart; views show
// each caller what it may see.
func TestProjectExtras(t *testing.T) {
	f := newProjFixture(t)
	ctx := f.ctx
	on, off := true, false
	trk := []api.Tracker{{Adapter: "beads", Prefix: "bd", Config: map[string]string{"readonly": "true"}}, {Adapter: "beads", Prefix: "sv", Area: "sprout"}}
	p, err := f.reg.Update(ctx, f.local, "orchid", api.UpdateProjectRequest{Trackers: &trk, GuestIssues: &on})
	if err != nil || !p.GuestIssues || !reflect.DeepEqual(p.Trackers, trk) {
		t.Fatalf("owner update: %+v %v", p, err)
	}
	if a := AreaForWork(f.reg, f.orchid.ID, "bd:sprout-9"); a != "sprout" {
		t.Fatal(a)
	}
	if a := AreaForWork(f.reg, f.orchid.ID, "sv:9"); a != "sprout" { // the tracker's own area
		t.Fatal(a)
	}
	if a := AreaForWork(f.reg, f.orchid.ID, "bd:9"); a != "" {
		t.Fatal(a)
	}
	if a := AreaForItem(f.reg, f.orchid.ID, "bd:orchid-1", []string{"sprout"}); a != "" {
		t.Fatalf("before: %q", a)
	}
	if AreaNeedsLabels(f.reg, f.orchid.ID, "bd:orchid-1") {
		t.Fatal("no area has labels yet")
	}
	labels := []string{"sprout", " "}
	v, err := f.reg.UpdateArea(ctx, f.local, "orchid", "Sprout", api.UpdateAreaRequest{Labels: &labels})
	if err != nil || len(v.Trackers) != 2 || v.Role != "owner" {
		t.Fatalf("view %+v %v", v, err)
	}
	// Live: work areas and label filters see it on the next call.
	if a := AreaForItem(f.reg, f.orchid.ID, "bd:orchid-1", []string{"sprout"}); a != "sprout" {
		t.Fatalf("labels not applied: %q", a)
	}
	if !AreaNeedsLabels(f.reg, f.orchid.ID, "bd:orchid-1") || AreaNeedsLabels(f.reg, f.orchid.ID, "bd:sprout-1") {
		t.Fatal("AreaNeedsLabels")
	}
	if l, plain := ListLabels(f.reg, f.orchid.ID, "", []string{"sprout"}); !reflect.DeepEqual(l, []string{"sprout"}) || !plain {
		t.Fatalf("ListLabels %v %v", l, plain)
	}
	if l, _ := ListLabels(f.reg, f.orchid.ID, "sprout", []string{"sprout"}); l != nil {
		t.Fatalf("ListLabels for the tracker's own area: %v", l)
	}
	if l, _ := ListLabels(f.reg, f.orchid.ID, "", []string{"sprout", "billing"}); l != nil {
		t.Fatalf("ListLabels with an area without labels: %v", l)
	}
	if _, err := f.reg.AddArea(ctx, f.local, "orchid", api.Area{Name: "docs", Labels: []string{"docs"}}); err != nil {
		t.Fatal(err)
	}
	if a := AreaForItem(f.reg, f.orchid.ID, "bd:orchid-2", []string{"docs"}); a != "docs" {
		t.Fatalf("added area not applied: %q", a)
	}

	// A guest of orchid/sprout (a label area, the dogfood case) sees its area
	// with its own tracker and the project-level bd tracker its labels pull
	// items from (what work sources lists), without bd's config.
	tok, _ := f.reg.Mint(ctx, f.local, api.MintTokenRequest{As: "victor", Project: "orchid", Area: "sprout", Role: "guest"})
	g, _ := f.reg.FromToken(ctx, tok.Secret, true)
	gv, err := f.reg.GetArea(g.Scope, "orchid", "sprout")
	want := []api.Tracker{{Adapter: "beads", Prefix: "bd"}, {Adapter: "beads", Prefix: "sv", Area: "sprout"}}
	if err != nil || gv.Role != "guest" || !reflect.DeepEqual(gv.Trackers, want) {
		t.Fatalf("guest view %+v %v", gv, err)
	}
	if gp, err := f.reg.Get(g.Scope, "orchid"); err != nil || !reflect.DeepEqual(gp.Trackers, want) || !gp.GuestIssues {
		t.Fatalf("guest project view %+v %v", gp, err)
	}
	// An area whose rules cannot reach bd (no labels, a glob on another
	// prefix) is not served by it.
	if _, err := f.reg.AddArea(ctx, f.local, "orchid", api.Area{Name: "notes", Work: []string{"zz:*"}}); err != nil {
		t.Fatal(err)
	}
	nt, _ := f.reg.Mint(ctx, f.local, api.MintTokenRequest{As: "nina", Project: "orchid", Area: "notes", Role: "guest"})
	ng, _ := f.reg.FromToken(ctx, nt.Secret, true)
	if nv, err := f.reg.GetArea(ng.Scope, "orchid", "notes"); err != nil || len(nv.Trackers) != 0 {
		t.Fatalf("notes guest view %+v %v", nv, err)
	}
	if np, err := f.reg.Get(ng.Scope, "orchid"); err != nil || len(np.Trackers) != 0 {
		t.Fatalf("notes guest project view %+v %v", np, err)
	}
	for _, c := range []struct {
		a    api.Area
		want bool
	}{
		{api.Area{Work: []string{"bd:sprout-*"}}, true}, {api.Area{Work: []string{"b*"}}, true}, {api.Area{Work: []string{"*"}}, true},
		{api.Area{Work: []string{"bdx:*"}}, false}, {api.Area{Work: []string{"zz:*"}}, false}, {api.Area{Labels: []string{"x"}}, true},
	} {
		if got := AreaServes(c.a, api.Tracker{Prefix: "bd"}); got != c.want {
			t.Errorf("AreaServes(%+v, bd) = %v", c.a, got)
		}
	}

	// An owner token from the network may set guest_issues, never trackers
	// or repo rules: the refusal has the words it always had.
	ot, _ := f.reg.Mint(ctx, f.local, api.MintTokenRequest{As: "carl", Project: "orchid", Role: "owner"})
	o, _ := f.reg.FromToken(ctx, ot.Secret, true)
	const refusal = "repo rules and trackers are machine settings: only the machine's owner on its own socket may change them"
	for what, req := range map[string]api.UpdateProjectRequest{
		"trackers": {Trackers: &[]api.Tracker{}}, "repo rules": {Repos: &api.RepoRules{Paths: []string{"/x"}}}} {
		if _, err := f.reg.Update(ctx, o, "orchid", req); !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), refusal) {
			t.Fatalf("remote owner set %s: %v", what, err)
		}
	}
	if p, err := f.reg.Update(ctx, o, "orchid", api.UpdateProjectRequest{GuestIssues: &off}); err != nil || p.GuestIssues || len(p.Trackers) != 2 {
		t.Fatalf("remote owner cleared guest_issues: %+v %v", p, err)
	}
	if b, _ := os.ReadFile(f.path(f.orchid.ID)); strings.Contains(string(b), "guest_issues") {
		t.Fatalf("guest_issues left in the file:\n%s", b)
	}
	if _, err := f.reg.Update(ctx, f.local, "orchid", api.UpdateProjectRequest{GuestIssues: &on}); err != nil {
		t.Fatal(err)
	}
	// A restart keeps both, and writes the file it read.
	before, _ := os.ReadFile(f.path(f.orchid.ID))
	r2 := f.reopen(t)
	if p, err := r2.Get(authz.All(), "orchid"); err != nil || !p.GuestIssues || !reflect.DeepEqual(p.Trackers, trk) {
		t.Fatalf("after restart: %+v %v", p, err)
	}
	if after, _ := os.ReadFile(f.path(f.orchid.ID)); string(after) != string(before) {
		t.Fatalf("a restart changed project.toml:\n%s\n--- was\n%s", after, before)
	}
	// An empty list clears the trackers.
	if p, err := r2.Update(ctx, f.local, "orchid", api.UpdateProjectRequest{Trackers: &[]api.Tracker{}}); err != nil || len(p.Trackers) != 0 {
		t.Fatalf("clear: %+v %v", p, err)
	}
}
