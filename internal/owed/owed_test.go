package owed

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// fakeSessions answers for made-up sessions only.
type fakeSessions struct {
	orchcore.SessionService
	all     map[string]orchapi.Session
	visible map[string]bool
	person  map[string][]orchapi.Session
	// truncated is what SessionsOf says of its window
	truncated bool
}

func (f *fakeSessions) Lookup(_ context.Context, a string) (orchapi.Session, bool) {
	s, ok := f.all[a]
	return s, ok
}
func (f *fakeSessions) Visible(_ context.Context, _ authz.Scope, a string) bool { return f.visible[a] }
func (f *fakeSessions) SessionsOf(_ context.Context, _ authz.Scope, person string, _ time.Time) ([]orchapi.Session, bool, error) {
	return f.person[person], f.truncated, nil
}

func fixture(t *testing.T) (*Service, *fakeSessions, *authz.Subject) {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "zm-owed")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	st, err := storetest.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{Home: home, Human: "ada", Machine: "box"}
	reg, err := project.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	fs := &fakeSessions{
		all: map[string]orchapi.Session{
			"box/aaaa1111": {Address: "box/aaaa1111", ID: "aaaa1111"},
			"box/bbbb2222": {Address: "box/bbbb2222", ID: "bbbb2222"},
			"box/cccc3333": {Address: "box/cccc3333", ID: "cccc3333"},
		},
		visible: map[string]bool{"box/aaaa1111": true, "box/bbbb2222": true},
		person:  map[string][]orchapi.Session{"ada": {{Address: "box/aaaa1111"}, {Address: "box/bbbb2222"}}},
	}
	d := &core.Deps{Store: st, Projects: reg, Config: cfg}
	orchcore.Wire(d).Sessions = fs
	return New(d), fs, reg.Local("")
}

func addSource(s *Service, name string, f func(orchcore.OwedWho) ([]orchapi.OwedItem, error)) {
	orchcore.AddOwedSource(s.d, orchcore.OwedSource{Name: name, Owed: func(_ context.Context, _ *authz.Subject, _ authz.Scope, w orchcore.OwedWho) (orchcore.OwedPart, error) {
		items, err := f(w)
		return orchcore.OwedPart{Items: items}, err
	}})
}

func TestListJoinsSourcesAndNamesWhatIsUnknown(t *testing.T) {
	s, _, owner := fixture(t)
	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	t1 := t0.Add(time.Hour)
	addSource(s, "work", func(w orchcore.OwedWho) ([]orchapi.OwedItem, error) {
		return []orchapi.OwedItem{
			{Kind: orchapi.OwedLane, Ref: "box/bbbb2222", Since: &t0},
			{Kind: orchapi.OwedWork, Ref: "local:x", Since: &t1},
			{Kind: orchapi.OwedWork, Ref: "local:y", Since: &t0},
			{Kind: orchapi.OwedPR, Ref: "https://example.test/pull/1"},
		}, nil
	})
	addSource(s, "asks", func(orchcore.OwedWho) ([]orchapi.OwedItem, error) {
		return []orchapi.OwedItem{{Kind: orchapi.OwedWork, Ref: "local:x"}}, nil // a repeat: counted once
	})
	addSource(s, "sessions", func(orchcore.OwedWho) ([]orchapi.OwedItem, error) { return nil, errors.New("private canary text") })

	l, err := s.list(context.Background(), owner, owner.Scope, "box/aaaa1111", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range l.Items {
		got = append(got, it.Kind+" "+it.Ref)
	}
	want := []string{"pr https://example.test/pull/1", "work local:y", "work local:x", "lane box/bbbb2222"}
	if len(got) != len(want) {
		t.Fatalf("items %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("items %v, want %v", got, want)
		}
	}
	names := map[string]orchapi.OwedGap{}
	for _, g := range l.Unknown {
		names[g.Name] = g
	}
	// A source that failed is unknown, with no word of its error.
	if g := names["sessions"]; g.Kind != orchapi.OwedGapSource || g.Why != "did not answer" {
		t.Fatalf("failed source: %+v", l.Unknown)
	}
	for _, n := range []string{"cloud_tasks", "outside_lanes", "background_processes", "worktree_changes", "unattributed_pull_requests"} {
		if names[n].Kind != orchapi.OwedGapUnseen {
			t.Fatalf("%s not listed as not recorded: %+v", n, l.Unknown)
		}
	}
	if l.For != orchapi.OwedForSession || l.Subject != "box/aaaa1111" || len(l.Sessions) != 1 {
		t.Fatalf("who: %+v", l)
	}
}

func TestAPersonsListCoversTheirSessions(t *testing.T) {
	s, _, owner := fixture(t)
	var seen orchcore.OwedWho
	addSource(s, "work", func(w orchcore.OwedWho) ([]orchapi.OwedItem, error) { seen = w; return nil, nil })
	for _, named := range []string{"", "me", "ada"} {
		l, err := s.list(context.Background(), owner, owner.Scope, "", named, time.Hour)
		if err != nil {
			t.Fatalf("person %q: %v", named, err)
		}
		if l.For != orchapi.OwedForPerson || l.Subject != "ada" || len(l.Sessions) != 2 || seen.Person != "ada" || len(seen.Sessions) != 2 {
			t.Fatalf("person %q: %+v / %+v", named, l, seen)
		}
	}
	if len(seen.Addresses()) != 2 {
		t.Fatalf("addresses %v", seen.Addresses())
	}
}

func TestASessionAsksAboutItselfByDefault(t *testing.T) {
	s, _, owner := fixture(t)
	me := *owner
	me.Session = "box/bbbb2222"
	l, err := s.list(context.Background(), &me, me.Scope, "", "", time.Hour)
	if err != nil || l.For != orchapi.OwedForSession || l.Subject != "box/bbbb2222" {
		t.Fatalf("%+v %v", l, err)
	}
}

func TestHiddenAndOtherPeopleAnswerLikeMissing(t *testing.T) {
	s, _, owner := fixture(t)
	ctx := context.Background()
	for _, ref := range []string{"box/cccc3333", "box/nope0000", "no-such-name"} { // hidden, missing, unknown name
		if _, err := s.list(ctx, owner, owner.Scope, ref, "", time.Hour); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("session %q: %v", ref, err)
		}
	}
	if _, err := s.list(ctx, owner, owner.Scope, "", "grace", time.Hour); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("another person: %v", err)
	}
	if _, err := s.list(ctx, owner, owner.Scope, "box/aaaa1111", "ada", time.Hour); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("both named: %v", err)
	}
	// Someone with no person to name and no session of their own gets a plain refusal.
	nobody := *owner
	nobody.Via, nobody.Identity = authz.ViaToken, api.Identity{Name: "a-machine", Kind: api.IdentityMachine}
	if _, err := s.list(ctx, &nobody, nobody.Scope, "", "", time.Hour); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("no one to list: %v", err)
	}
}

func TestNoSourceIsNeverAnEmptyList(t *testing.T) {
	s, _, owner := fixture(t)
	l, err := s.list(context.Background(), owner, owner.Scope, "box/aaaa1111", "", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Items) != 0 || l.Unknown[0].Kind != orchapi.OwedGapSource || l.Unknown[0].Name != "all" {
		t.Fatalf("a daemon with no source must say so: %+v", l.Unknown)
	}
}

func TestPartialReadsAreUnknown(t *testing.T) {
	s, fs, owner := fixture(t)
	fs.truncated = true
	orchcore.AddOwedSource(s.d, orchcore.OwedSource{Name: "asks", Owed: func(context.Context, *authz.Subject, authz.Scope, orchcore.OwedWho) (orchcore.OwedPart, error) {
		return orchcore.OwedPart{Gaps: []orchapi.OwedGap{orchcore.Partial("asks", "capped")}}, nil
	}})
	l, err := s.list(context.Background(), owner, owner.Scope, "", "me", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	partial := map[string]bool{}
	for _, g := range l.Unknown {
		if g.Kind == orchapi.OwedGapPartial {
			partial[g.Name] = true
		}
	}
	if !partial["sessions"] || !partial["asks"] {
		t.Fatalf("a partial read must be listed as unknown: %+v", l.Unknown)
	}
}
