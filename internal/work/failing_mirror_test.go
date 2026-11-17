package work

import (
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/fake"
)

// A mirror whose syncs keep failing says so in tracker ls (failing since,
// how many in a row) and to the notice line (orchcore.TrackerHealth), and both
// clear on the next sync that works.
func TestFailingMirrorSurfaces(t *testing.T) {
	quiet(t)
	h := newHarness(t)
	state := h.installFake()
	owner := h.as(h.owner, "harbor")
	if _, err := orchclient.AddTracker(h.ctx, owner, orchapi.AddTrackerRequest{Adapter: "fake", Prefix: "bd"}); err != nil {
		t.Fatal(err)
	}
	if fs := h.s.FailingTrackers(h.owner.Scope, 0); len(fs) != 0 {
		t.Fatalf("healthy tracker reported failing: %+v", fs)
	}
	edit(t, state, func(st *fake.State) { st.FailList = "Error: table not found: leases" })
	for i := 0; i < 2; i++ {
		_ = h.s.syncAll(h.ctx) // fails, by design
	}
	vs, err := orchclient.Trackers(h.ctx, owner, "harbor")
	if err != nil || len(vs) != 1 || vs[0].Sync == nil {
		t.Fatalf("trackers: %+v %v", vs, err)
	}
	s := vs[0].Sync
	if s.FailingSince == nil || s.Failures < 2 || !strings.Contains(s.Error, "leases") || s.State != orchapi.SyncStale {
		t.Fatalf("failing mirror: %+v", s)
	}
	fs := h.s.FailingTrackers(h.owner.Scope, 0)
	if len(fs) != 1 || fs[0].Prefix != "bd" || fs[0].Slug != "harbor" || fs[0].Failures < 2 {
		t.Fatalf("FailingTrackers: %+v", fs)
	}
	if fs := h.s.FailingTrackers(h.owner.Scope, time.Hour); len(fs) != 0 {
		t.Fatalf("a fresh failure passed the threshold: %+v", fs)
	}
	edit(t, state, func(st *fake.State) { st.FailList = "" })
	h.sync()
	if fs := h.s.FailingTrackers(h.owner.Scope, 0); len(fs) != 0 {
		t.Fatalf("still failing after a good sync: %+v", fs)
	}
	if vs, _ := orchclient.Trackers(h.ctx, owner, "harbor"); vs[0].Sync.FailingSince != nil {
		t.Fatalf("failing_since kept: %+v", vs[0].Sync)
	}
}
