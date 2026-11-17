package work

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
)

func leaseRows(t *testing.T, h *harness, team bool, project string) int {
	t.Helper()
	db := h.d.Store.Registry()
	if team {
		db = h.d.Store.Writer(project)
	}
	var n int
	if err := db.QueryRowContext(h.ctx, `SELECT count(*) FROM leases WHERE project = ?`, project).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// zm project team moves the leases into project.db; the same lease code
// serves them there (claims, conflicts, renewals, release, expiry and
// the mail to its holder); solo moves them back.
func TestTeamLeaseMove(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	it, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "keep me", Area: "solver"})
	exp, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "expire me", Area: "solver"})
	a := h.as(h.session("mini/aaaaaaaa", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
	b := h.as(h.session("mini/bbbbbbbb", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
	if _, err := orchclient.Claim(h.ctx, a, it.ID, orchapi.ClaimRequest{TTLSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Lease(h.ctx, owner, orchapi.ClaimRequest{Key: "lock:deploy", TTLSeconds: 3600}); err != nil {
		t.Fatal(err)
	}
	// A live bound lease refuses the move.
	bound := h.as(h.session("mini/managed1", h.harbor.ID, "solver", true, orchapi.StateRunning), "")
	if _, err := orchclient.Claim(h.ctx, bound, exp.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.MoveLeases(h.ctx, h.harbor.ID, true); !isCode(err, api.CodeConflict) {
		t.Fatalf("moved with a live bound lease: %v", err)
	}
	if err := h.s.SessionEnded(h.ctx, "mini/managed1"); err != nil {
		t.Fatal(err)
	}
	n, err := h.s.MoveLeases(h.ctx, h.harbor.ID, true)
	if err != nil || n != 2 || !h.d.Projects.IsTeam(h.harbor.ID) || !h.d.Store.IsTeam(h.harbor.ID) {
		t.Fatalf("move: %d %v", n, err)
	}
	if leaseRows(t, h, false, h.harbor.ID) != 0 || leaseRows(t, h, true, h.harbor.ID) != 2 {
		t.Fatal("rows not moved")
	}
	ls, _ := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{})
	if len(ls) != 2 {
		t.Fatalf("leases after the move %+v", ls)
	}
	// Conflicts, renewals and holders work from project.db.
	if _, err := orchclient.Claim(h.ctx, b, it.ID, orchapi.ClaimRequest{}); !isCode(err, api.CodeConflict) {
		t.Fatalf("conflict after the move: %v", err)
	}
	if r, err := orchclient.Claim(h.ctx, a, it.ID, orchapi.ClaimRequest{Renew: true}); err != nil || !r.Lease.Expires.After(h.clock.now()) {
		t.Fatalf("renew: %+v %v", r, err)
	}
	if got, _ := orchclient.GetWork(h.ctx, owner, it.ID); got.Owner != "mini/aaaaaaaa" || got.Status != orchapi.WorkClaimed {
		t.Fatalf("item %+v", got)
	}
	// A team claim commits its event with it.
	r, err := orchclient.Claim(h.ctx, b, exp.ID, orchapi.ClaimRequest{TTLSeconds: 60})
	if err != nil || r.Item.Status != orchapi.WorkClaimed {
		t.Fatalf("team claim %+v %v", r, err)
	}
	claimed := h.events(orchapi.EventWorkClaimed)
	if last := claimed[len(claimed)-1]; last.Subject != exp.ID || last.Project != h.harbor.ID {
		t.Fatalf("claimed event %+v", last)
	}
	statusOf := func(id string) []orchapi.WorkStatus {
		var out []orchapi.WorkStatus
		for _, e := range h.events(orchapi.EventWorkStatusChanged) {
			if e.Subject == id {
				var it orchapi.WorkItem
				if err := json.Unmarshal(e.Data, &it); err != nil {
					t.Fatal(err)
				}
				out = append(out, it.Status)
			}
		}
		return out
	}
	// A team claim that moves a local item publishes its status too (G3).
	if got := statusOf(exp.ID); len(got) == 0 || got[len(got)-1] != orchapi.WorkClaimed {
		t.Fatalf("status events after the team claim: %v", got)
	}
	// Expiry: the ticker walks project.db too.
	h.clock.add(61 * time.Second)
	h.s.tick(h.ctx)
	if sent := h.mail.all(); len(sent) != 1 || sent[0].req.Ref != exp.ID || sent[0].req.To != "mini/bbbbbbbb" {
		t.Fatalf("team mail to the holder %+v", sent)
	}
	if evs := h.events(orchapi.EventWorkLeaseExpired); len(evs) != 1 || evs[0].Subject != exp.ID {
		t.Fatalf("lease_expired %+v", evs)
	}
	if got, _ := orchclient.GetWork(h.ctx, owner, exp.ID); got.Status != orchapi.WorkOpen {
		t.Fatalf("expired team item %+v", got)
	}
	// Release by the holder.
	if rel, err := orchclient.Release(h.ctx, a, it.ID, orchapi.ReleaseRequest{}); err != nil || !rel.Released {
		t.Fatalf("release %+v %v", rel, err)
	}
	if got := statusOf(it.ID); len(got) == 0 || got[len(got)-1] != orchapi.WorkOpen {
		t.Fatalf("status events after the team release: %v", got)
	}
	// Back to solo.
	if n, err := h.s.MoveLeases(h.ctx, h.harbor.ID, false); err != nil || n != 1 || h.d.Store.IsTeam(h.harbor.ID) {
		t.Fatalf("solo: %d %v", n, err)
	}
	if leaseRows(t, h, false, h.harbor.ID) != 1 {
		t.Fatal("rows not moved back")
	}
	if ls, _ := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{}); len(ls) != 1 || ls[0].Key != "lock:deploy" {
		t.Fatalf("leases after solo %+v", ls)
	}
}

// A crash between copying and the commit point leaves rows in the file
// that is not the project's; the next start drops them.
func TestStrayLeasesDropped(t *testing.T) {
	h := newHarness(t)
	db := h.d.Store.Writer(h.harbor.ID)
	if _, err := db.ExecContext(h.ctx, `INSERT INTO leases (project, area, key, holder, bound, note, claimed, renewed, expires)
		VALUES (?, '', 'lock:x', 'human:ana', 0, '', 1, 1, 99999999999999)`, h.harbor.ID); err != nil {
		t.Fatal(err)
	}
	h.s.dropStrayLeases(h.ctx)
	if leaseRows(t, h, true, h.harbor.ID) != 0 {
		t.Fatal("stray row kept")
	}
}

// Team claims of tracker items: the lease is the claim; the tracker's
// assignee follows by write-back; a tracker holding the item for someone
// else releases the lease ("tracker conflict") and mails the holder, at
// write-back and when a later sync disagrees.
func TestTeamTrackerWriteBack(t *testing.T) {
	h := newHarness(t)
	state := h.installFake()
	st, _ := fake.Load(state)
	st.Issues["solver-1"] = &fake.Issue{ID: "solver-1", Title: "free", Status: "open"}
	st.Issues["solver-4"] = &fake.Issue{ID: "solver-4", Title: "taken in beads", Status: "in_progress", Assignee: "bea"}
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd", Config: map[string]string{"actor": "zm"}, Env: "inherit"}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	if _, err := h.s.MoveLeases(h.ctx, h.harbor.ID, true); err != nil {
		t.Fatal(err)
	}
	drain := func() {
		for {
			select {
			case j := <-h.s.wb:
				h.s.writeOne(h.ctx, j)
			default:
				return
			}
		}
	}
	a := h.as(h.session("mini/aaaaaaaa", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
	// The claim is the lease, at once; the tracker follows.
	if r, err := orchclient.Claim(h.ctx, a, "bd:solver-1", orchapi.ClaimRequest{}); err != nil || r.Lease.Holder != "mini/aaaaaaaa" {
		t.Fatalf("team tracker claim %+v %v", r, err)
	}
	drain()
	st, _ = fake.Load(state)
	if is := st.Issues["solver-1"]; is.Status != "in_progress" {
		t.Fatalf("write-back did not claim in the tracker: %+v", is)
	}
	// The tracker holds solver-4 for bea: the claim is taken, then
	// released by write-back with "tracker conflict", and the holder told.
	if _, err := orchclient.Claim(h.ctx, a, "bd:solver-4", orchapi.ClaimRequest{}); err != nil {
		t.Fatalf("claim of a tracker-held item: %v", err)
	}
	drain()
	if l, _ := h.s.liveLease(h.ctx, h.harbor.ID, "solver", "bd:solver-4"); l != nil {
		t.Fatalf("lease kept after a tracker conflict: %+v", l)
	}
	rel := h.events(orchapi.EventWorkReleased)
	if len(rel) == 0 || !strings.Contains(string(rel[len(rel)-1].Data), NoteTrackerConflict) {
		t.Fatalf("released events %+v", rel)
	}
	if sent := h.mail.all(); len(sent) != 1 || sent[0].req.To != "mini/aaaaaaaa" || sent[0].req.Ref != "bd:solver-4" {
		t.Fatalf("holder not told: %+v", sent)
	}
	// An agreeing tracker never trips it (its owner is the holder the
	// write-back recorded).
	old := conflictGrace
	conflictGrace = 0
	t.Cleanup(func() { conflictGrace = old })
	h.sync()
	h.s.tick(h.ctx)
	if l, _ := h.s.liveLease(h.ctx, h.harbor.ID, "solver", "bd:solver-1"); l == nil {
		t.Fatalf("an agreeing tracker released the lease (mirror owner vs holder)")
	}
	// Later the tracker assigns solver-1 to someone else directly.
	st, _ = fake.Load(state)
	st.Issues["solver-1"].Assignee = "dev-01/deadbeef"
	st.Issues["solver-1"].UpdatedAt = fake.Now()
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	h.sync()
	h.s.tick(h.ctx)
	if l, _ := h.s.liveLease(h.ctx, h.harbor.ID, "solver", "bd:solver-1"); l != nil {
		t.Fatalf("lease kept after the tracker disagreed: %+v", l)
	}
	if sent := h.mail.all(); len(sent) != 2 || sent[1].req.Ref != "bd:solver-1" {
		t.Fatalf("holder not told of the later conflict: %+v", sent)
	}
}

// A bound lease's renewal takes the project's lease lock like every lease
// operation, so it never lands in the file a move is emptying (between
// MoveLeases' copy and the mode flip).
func TestBoundRenewalWaitsForMove(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	it, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "bound", Area: "solver"})
	if _, err := h.s.MoveLeases(h.ctx, h.harbor.ID, true); err != nil {
		t.Fatal(err)
	}
	bound := h.as(h.session("mini/managed1", h.harbor.ID, "solver", true, orchapi.StateRunning), "")
	if _, err := orchclient.Claim(h.ctx, bound, it.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	renewed := func() int64 {
		var n int64
		if err := h.d.Store.Writer(h.harbor.ID).QueryRowContext(h.ctx, `SELECT renewed FROM leases WHERE project = ?`, h.harbor.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := renewed()
	h.clock.add(5 * time.Second)
	lk := h.s.locks.of(h.harbor.ID)
	lk.Lock() // a move in progress
	done := make(chan struct{})
	go func() { h.s.tickBound(h.ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	if got := renewed(); got != before {
		lk.Unlock()
		<-done
		t.Fatalf("renewed during a move: %d -> %d", before, got)
	}
	lk.Unlock()
	<-done
	if got := renewed(); got == before {
		t.Fatal("not renewed after the move")
	}
}
