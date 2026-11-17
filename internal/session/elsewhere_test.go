package session

import (
	"context"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

// A heartbeat's elsewhere set: listed = live; left out = known (an ended
// session still receives) but not live; a quiet machine's set goes stale;
// visible to readers of the whole project, or of a guest replica's area;
// the ticker's prune forgets what is past the window.
func TestElsewhere(t *testing.T) {
	ctx := context.Background()
	s := &Service{d: &core.Deps{Config: &config.Config{}}}
	const p, q = "01PROJ", "01OTHER"
	s.RecordElsewhere(ctx, p, "bea-mac", "tokC", []string{"bea-mac/aaaa1111", "bea-mac/bbbb2222"}, true, nil)
	s.RecordElsewhere(ctx, p, "vela-mac", "tokV", []string{"vela-mac/cccc3333"}, false, []string{"solver"})
	if es, ok := s.Elsewhere(p, "bea-mac/aaaa1111"); !ok || !es.Live || es.Token != "tokC" || !es.Whole {
		t.Fatalf("listed: %+v %v", es, ok)
	}
	if _, ok := s.Elsewhere(q, "bea-mac/aaaa1111"); ok {
		t.Fatal("known in a project whose heartbeat never listed it")
	}
	// The next heartbeat leaves bbbb out: known, not live; vela-mac's set
	// is untouched.
	s.RecordElsewhere(ctx, p, "bea-mac", "tokC", []string{"bea-mac/aaaa1111"}, true, nil)
	if es, ok := s.Elsewhere(p, "bea-mac/bbbb2222"); !ok || es.Live {
		t.Fatalf("left out: %+v %v", es, ok)
	}
	if es, ok := s.Elsewhere(p, "vela-mac/cccc3333"); !ok || !es.Live || es.Token != "tokV" || es.Whole || len(es.Areas) != 1 {
		t.Fatalf("other machine: %+v %v", es, ok)
	}
	// A machine whose heartbeats stop: its sessions stop being live.
	s.elsewhere.mu.Lock()
	s.elsewhere.beat[p]["bea-mac"] = time.Now().Add(-2 * s.presenceLost())
	s.elsewhere.mu.Unlock()
	if es, ok := s.Elsewhere(p, "bea-mac/aaaa1111"); !ok || es.Live {
		t.Fatalf("stale machine: %+v %v", es, ok)
	}
	// Past the keep window: forgotten.
	s.elsewhere.mu.Lock()
	s.elsewhere.by[p]["bea-mac/aaaa1111"].seen = time.Now().Add(-elsewhereKeep - time.Minute)
	s.elsewhere.mu.Unlock()
	if _, ok := s.Elsewhere(p, "bea-mac/aaaa1111"); ok {
		t.Fatal("kept past the window")
	}

	whole := authz.FromTuples([]authz.Tuple{{Relation: authz.Member, Object: authz.ProjectObject(p)}}, nil)
	solver := authz.FromTuples([]authz.Tuple{{Relation: authz.Guest, Object: "area:" + p + "/solver"}}, nil)
	billing := authz.FromTuples([]authz.Tuple{{Relation: authz.Guest, Object: "area:" + p + "/billing"}}, nil)
	other := authz.FromTuples([]authz.Tuple{{Relation: authz.Owner, Object: authz.ProjectObject(q)}}, nil)
	for _, c := range []struct {
		sc   authz.Scope
		addr string
		want bool
	}{
		{whole, "bea-mac/bbbb2222", true},
		{solver, "bea-mac/bbbb2222", false}, // whole-project readers only
		{solver, "vela-mac/cccc3333", true}, // a guest replica's: readers of its area
		{whole, "vela-mac/cccc3333", true},
		{billing, "vela-mac/cccc3333", false},
		{other, "bea-mac/bbbb2222", false},
		{whole, "bea-mac/zzzz9999", false},
	} {
		if got := s.elsewhereVisible(ctx, c.sc, c.addr); got != c.want {
			t.Errorf("visible(%v, %s) = %v, want %v", c.sc, c.addr, got, c.want)
		}
	}
	// The ticker's prune: rows and heartbeat times past the window go, with
	// the maps they leave empty.
	s.elsewhere.mu.Lock()
	for _, r := range s.elsewhere.by[p] {
		r.seen = time.Now().Add(-elsewhereKeep - time.Minute)
	}
	for m := range s.elsewhere.beat[p] {
		s.elsewhere.beat[p][m] = time.Now().Add(-elsewhereKeep - time.Minute)
	}
	s.elsewhere.mu.Unlock()
	s.pruneElsewhere()
	if len(s.elsewhere.by) != 0 || len(s.elsewhere.beat) != 0 {
		t.Fatalf("after the prune: %v %v", s.elsewhere.by, s.elsewhere.beat)
	}
}
