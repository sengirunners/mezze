package work

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
)

// liveSize is the live harbor beads at the time of measuring.
const liveSize = 11512

// bigTracker binds a fake tracker holding n issues shaped like the
// live harbor beads (80% closed, 2% labelled solver, ~1 KiB bodies,
// updated over a year) and waits for its mirror's first full sync.
func bigTracker(tb testing.TB, n int) (*harness, *client.Client, *client.Client, time.Duration) {
	quiet(tb)
	h := newHarness(tb)
	state := h.installFake()
	r := rand.New(rand.NewPCG(1, 2))
	body := strings.Repeat("Steps to reproduce, logs and the fix plan. ", 24)
	edit(tb, state, func(st *fake.State) {
		for i := range n {
			id := fmt.Sprintf("harbor-%05d", i)
			status := "closed"
			switch x := r.IntN(100); {
			case x < 18:
				status = "open"
			case x < 19:
				status = "deferred"
			case x == 19:
				status = "in_progress"
			}
			var labels []string
			if r.IntN(100) < 2 {
				labels = append(labels, "solver")
			}
			if r.IntN(3) == 0 {
				labels = append(labels, "ux")
			}
			up := time.Now().Add(-time.Duration(r.IntN(365*24)) * time.Hour).UTC().Format(time.RFC3339)
			st.Issues[id] = &fake.Issue{ID: id, Title: fmt.Sprintf("Issue %d: fix the thing in module %d", i, i%40), Description: body,
				Status: status, Assignee: map[bool]string{true: "dev-01/task-" + id}[status == "in_progress"], Labels: labels, UpdatedAt: up}
		}
	})
	if _, err := h.d.Projects.AddArea(h.ctx, h.owner, "harbor", api.Area{Name: "lbl", Labels: []string{"solver"}}); err != nil {
		tb.Fatal(err)
	}
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd"}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		tb.Fatal(err)
	}
	start := time.Now()
	h.sync()
	took := time.Since(start)
	tok, err := h.d.Projects.Mint(h.ctx, h.owner, api.MintTokenRequest{As: "vida", Project: "harbor", Area: "lbl", Role: "guest"})
	if err != nil {
		tb.Fatal(err)
	}
	gs, err := h.d.Projects.FromToken(h.ctx, tok.Secret, true)
	if err != nil {
		tb.Fatal(err)
	}
	return h, h.as(h.owner, ""), h.as(gs, ""), took
}

type benchCase struct {
	name string
	run  func() error
}

func bigCases(h *harness, owner, guest *client.Client) []benchCase {
	ctx := h.ctx
	return []benchCase{
		{"work ls (owner)", func() error { _, err := orchclient.WorkList(ctx, owner, orchapi.WorkQuery{}); return err }},
		{"work ls (guest, label area)", func() error { _, err := orchclient.WorkList(ctx, guest, orchapi.WorkQuery{}); return err }},
		{"work ls -q (owner)", func() error { _, err := orchclient.WorkList(ctx, owner, orchapi.WorkQuery{Q: "module 7"}); return err }},
		{"work ls --status done", func() error {
			_, err := orchclient.WorkList(ctx, owner, orchapi.WorkQuery{Status: orchapi.WorkDone})
			return err
		}},
		{"work ls --owner", func() error {
			_, err := orchclient.WorkList(ctx, owner, orchapi.WorkQuery{Owner: "dev-01/task-harbor-00019"})
			return err
		}},
		{"work show", func() error { _, err := orchclient.GetWork(ctx, owner, "bd:harbor-00042"); return err }},
		{"who (label-area item)", func() error { _, err := orchclient.Holder(ctx, owner, "bd:harbor-00042"); return err }},
		{"work show (guest, hidden)", func() error {
			if _, err := orchclient.GetWork(ctx, guest, "bd:harbor-00043"); !api.IsCode(err, api.CodeNotFound) && err != nil {
				return err
			}
			return nil
		}},
		{"work sources", func() error { _, err := orchclient.WorkSources(ctx, owner, ""); return err }},
	}
}

// Against a fake 11,512-item tracker after its first sync, every read is
// answered from the mirror well inside the budget (50 ms p95 end to end
// for work ls/show/who/sources; this measures the daemon's part, without
// the CLI's process start, about 10 ms on the dev Mac). Timing assertions opt
// in with ZM_MIRROR_SPEED=1; ZM_MIRROR_JSON writes advisory bench rows.
func TestMirror11kBudget(t *testing.T) {
	if testing.Short() || raceOn {
		t.Skip("seeds 11,512 items; timings mean nothing under -race")
	}
	h, owner, guest, took := bigTracker(t, liveSize)
	if n, _ := mirrorCount(h.ctx, mustDB(t, h), "bd"); n != liveSize {
		t.Fatalf("mirrored %d of %d", n, liveSize)
	}
	t.Logf("first full sync of %d items (fake tracker): %s", liveSize, took.Round(time.Millisecond))
	cases := bigCases(h, owner, guest)
	timing := newMirrorTiming(t, cases)
	for _, c := range cases {
		var ds []time.Duration
		for range 100 {
			s := time.Now()
			if err := c.run(); err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			ds = append(ds, time.Since(s))
		}
		slices.Sort(ds)
		p50, p95 := ds[len(ds)/2], ds[len(ds)*95/100]
		t.Logf("%-28s p50 %6.2f ms  p95 %6.2f ms", c.name, ms(p50), ms(p95))
		timing.record(c.name, ds)
	}
	// The guest's list is its label area only.
	gl, err := orchclient.WorkList(h.ctx, guest, orchapi.WorkQuery{Limit: 5000})
	if err != nil || len(gl.Items) == 0 {
		t.Fatalf("guest list %d %v", len(gl.Items), err)
	}
	for _, it := range gl.Items {
		if it.Area != "lbl" || !slices.Contains(it.Labels, "solver") {
			t.Fatalf("guest sees %+v", it)
		}
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

func mustDB(tb testing.TB, h *harness) *sql.DB {
	db, err := h.d.Store.Project(authz.All(), authz.Read, h.harbor.ID)
	if err != nil {
		tb.Fatal(err)
	}
	return db
}

// BenchmarkMirror11k: go test -run '^$' -bench Mirror11k ./internal/work/
func BenchmarkMirror11k(b *testing.B) {
	h, owner, guest, _ := bigTracker(b, liveSize)
	for _, c := range bigCases(h, owner, guest) {
		b.Run(strings.NewReplacer(" ", "_", "(", "", ")", "", ",", "").Replace(c.name), func(b *testing.B) {
			for b.Loop() {
				if err := c.run(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
