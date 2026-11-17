package work

import (
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/mezze/internal/work/wbcause"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/events"
)

// A replica's status write is never refused because of a status
// event its own claim's write-back minted, however late the background writer
// gets to it (the harness runs no writer: a queued job waits until the test
// drains it). A status change anyone else made after the base still is, and
// so is a write by another writer over the claimer's write-back.
func TestTeamBaseIgnoresOwnWriteBack(t *testing.T) {
	quiet(t)
	h := newHarness(t)
	// Open the team pool before starting the mirror: sync completion can
	// still be publishing its refreshed event when h.sync returns.
	if _, err := h.s.MoveLeases(h.ctx, h.harbor.ID, true); err != nil {
		t.Fatal(err)
	}
	state := h.installFake()
	st, _ := fake.Load(state)
	for _, id := range []string{"solver-1", "solver-2", "solver-3"} {
		st.Issues[id] = &fake.Issue{ID: id, Title: id, Status: "open"}
	}
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd", Config: map[string]string{"actor": "zm"}, Env: "inherit"}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
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
	// base is what a replica's copy has seen: the newest event so far.
	base := func() string {
		es := h.events()
		return es[len(es)-1].ID
	}
	statusEvents := func(id, after string) (n int) {
		for _, e := range h.events(orchapi.EventWorkStatusChanged) {
			if e.Subject == id && e.ID > after {
				n++
			}
		}
		return n
	}
	set := func(sub *authz.Subject, id string, to orchapi.WorkStatus, base string) error {
		l, err := h.s.locate(h.ctx, sub, sub.Scope, id)
		if err != nil {
			t.Fatal(err)
		}
		_, err = h.s.setStatus(h.ctx, sub, l, to, base, 0)
		return err
	}
	bea := h.session("mini/aaaaaaaa", h.harbor.ID, "solver", false, orchapi.StateRunning)
	ana := h.session("mini/bbbbbbbb", h.harbor.ID, "solver", false, orchapi.StateRunning)
	c, a := h.as(bea, ""), h.as(ana, "")

	// The claim answers with its write-back still queued (no wait), and the
	// write-back's move to claimed lands after the replica's base.
	start := time.Now()
	if _, err := orchclient.Claim(h.ctx, c, "bd:solver-1", orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second || len(h.s.wb) != 1 {
		t.Fatalf("claim took %v with %d write-backs queued; want at once, 1 queued", d, len(h.s.wb))
	}
	b := base()
	drain()
	if statusEvents("bd:solver-1", b) != 1 {
		t.Fatalf("write-back minted no status event after the base")
	}
	// The cause is stored for the authority's own check; readers that are
	// not the machine's owner (a replica) never get it.
	var stored string
	if err := mustDB(t, h).QueryRowContext(h.ctx, `SELECT json FROM events WHERE subject = ? AND type = ? AND id > ?`, "bd:solver-1", orchapi.EventWorkStatusChanged, b).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	guest, _ := h.d.Events.Since(h.ctx, h.guest(), b, events.Filter{Types: []string{orchapi.EventWorkStatusChanged}}, 0)
	if !strings.Contains(stored, `"`+wbcause.Key+`":"mini/aaaaaaaa `) || len(guest) != 1 || strings.Contains(string(guest[0].Data), wbcause.Key) {
		t.Fatalf("write-back cause: stored %s, guest %+v", stored, guest)
	}
	if err := set(ana, "bd:solver-1", orchapi.WorkDone, b); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("another writer over bea's write-back: %v, want conflict", err)
	}
	if err := set(bea, "bd:solver-1", orchapi.WorkDone, b); err != nil {
		t.Fatalf("done refused over its own claim's write-back: %v", err)
	}
	if st, _ = fake.Load(state); st.Issues["solver-1"].Status != "closed" {
		t.Fatalf("tracker status %s, want closed", st.Issues["solver-1"].Status)
	}

	// An expired holder's release, queued ahead of the claim (one FIFO
	// worker), is the claim's own too.
	if _, err := orchclient.Claim(h.ctx, a, "bd:solver-3", orchapi.ClaimRequest{TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	drain()
	h.clock.add(2 * time.Minute)
	if _, err := orchclient.Claim(h.ctx, c, "bd:solver-3", orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(h.s.wb) != 2 {
		t.Fatalf("%d write-backs queued, want the release and the claim", len(h.s.wb))
	}
	b = base()
	drain()
	if statusEvents("bd:solver-3", b) == 0 {
		t.Fatalf("write-backs minted no status event after the base")
	}
	if err := set(bea, "bd:solver-3", orchapi.WorkDone, b); err != nil {
		t.Fatalf("done refused over the release its claim queued: %v", err)
	}

	// Someone else's status change after the base is still refused, even
	// though bea holds the item and its own write-back is newer still.
	if _, err := orchclient.Claim(h.ctx, c, "bd:solver-2", orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	b = base()
	if err := set(h.owner, "bd:solver-2", orchapi.WorkInReview, ""); err != nil {
		t.Fatal(err)
	}
	drain()
	if err := set(bea, "bd:solver-2", orchapi.WorkDone, b); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("done over another writer's status change: %v, want conflict", err)
	}
}
