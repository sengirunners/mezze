package work

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/events"
)

var localIDRe = regexp.MustCompile(`^local:[a-z2-7]{6}$`)

func TestLocalItems(t *testing.T) {
	h := newHarness(t)
	c := h.as(h.owner, "harbor")
	it, err := orchclient.CreateWork(h.ctx, c, orchapi.CreateWorkRequest{Title: "Add the down migration", Area: "solver", Labels: []string{"v0"}})
	if err != nil {
		t.Fatal(err)
	}
	if !localIDRe.MatchString(it.ID) || it.Project != h.harbor.ID || it.Area != "solver" || it.Status != orchapi.WorkOpen ||
		it.CreatedBy != "human:ana" || len(it.Labels) != 1 {
		t.Fatalf("created %+v", it)
	}
	if _, err := orchclient.CreateWork(h.ctx, c, orchapi.CreateWorkRequest{Title: " "}); !isCode(err, api.CodeBadRequest) {
		t.Fatalf("empty title: %v", err)
	}
	if _, err := orchclient.CreateWork(h.ctx, c, orchapi.CreateWorkRequest{Title: "x", Area: "nope"}); !isCode(err, api.CodeNotFound) {
		t.Fatalf("missing area: %v", err)
	}
	// No project resolves: refused, never filed into a default.
	if _, err := orchclient.CreateWork(h.ctx, h.as(h.owner, ""), orchapi.CreateWorkRequest{Title: "x"}); !isCode(err, api.CodeBadRequest) {
		t.Fatalf("no project: %v", err)
	}
	other, _ := orchclient.CreateWork(h.ctx, c, orchapi.CreateWorkRequest{Title: "Second"})
	sideItem, err := orchclient.CreateWork(h.ctx, h.as(h.owner, "side"), orchapi.CreateWorkRequest{Title: "Side item"})
	if err != nil {
		t.Fatal(err)
	}

	all := h.as(h.owner, "")
	items, err := orchclient.Work(h.ctx, all, orchapi.WorkQuery{})
	if err != nil || len(items) != 3 {
		t.Fatalf("list all %d %v", len(items), err)
	}
	if items, _ = orchclient.Work(h.ctx, c, orchapi.WorkQuery{}); len(items) != 2 {
		t.Fatalf("--project narrows: %d", len(items))
	}
	if items, _ = orchclient.Work(h.ctx, all, orchapi.WorkQuery{Area: "solver"}); len(items) != 1 || items[0].ID != it.ID {
		t.Fatalf("area filter %+v", items)
	}
	if items, _ = orchclient.Work(h.ctx, all, orchapi.WorkQuery{Q: "MIGRATION"}); len(items) != 1 {
		t.Fatalf("text filter %+v", items)
	}

	// Comments are the item's history.
	if _, err := orchclient.Comment(h.ctx, all, it.ID, orchapi.CommentRequest{Body: "PR #9 is up"}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Comment(h.ctx, all, it.ID, orchapi.CommentRequest{Body: "  "}); !isCode(err, api.CodeBadRequest) {
		t.Fatalf("empty comment: %v", err)
	}
	cs, err := orchclient.Comments(h.ctx, all, it.ID)
	if err != nil || len(cs) != 1 || cs[0].Author != "human:ana" || cs[0].Item != it.ID {
		t.Fatalf("comments %+v %v", cs, err)
	}

	// Status.
	got, err := orchclient.SetStatus(h.ctx, all, it.ID, orchapi.StatusRequest{Status: orchapi.WorkInReview})
	if err != nil || got.Status != orchapi.WorkInReview {
		t.Fatalf("status %+v %v", got, err)
	}
	if _, err := orchclient.SetStatus(h.ctx, all, it.ID, orchapi.StatusRequest{Status: "doing"}); !isCode(err, api.CodeBadRequest) {
		t.Fatalf("bad status: %v", err)
	}
	if _, err := orchclient.SetStatus(h.ctx, all, it.ID, orchapi.StatusRequest{Status: orchapi.WorkClaimed}); !isCode(err, api.CodeBadRequest) {
		t.Fatalf("status claimed without a lease: %v", err)
	}

	// Links: URL, an item in the same project, an item in another project, a session.
	h.session("mini/abcd1234", h.harbor.ID, "solver", false, orchapi.StateRunning)
	for _, l := range []orchapi.Link{{Rel: "pr", Target: "https://github.com/o/r/pull/9"}, {Rel: "blocks", Target: other.ID},
		{Rel: "related", Target: sideItem.ID}, {Rel: "session", Target: "mini/abcd1234"}} {
		if _, err := orchclient.Link(h.ctx, all, it.ID, l); err != nil {
			t.Fatalf("link %+v: %v", l, err)
		}
	}
	for _, bad := range []orchapi.Link{{Rel: "nope", Target: "https://x"}, {Rel: "session", Target: "https://x"},
		{Rel: "pr", Target: other.ID}, {Rel: "blocks", Target: it.ID}} {
		if _, err := orchclient.Link(h.ctx, all, it.ID, bad); !isCode(err, api.CodeBadRequest) {
			t.Fatalf("bad link %+v: %v", bad, err)
		}
	}
	if _, err := orchclient.Link(h.ctx, all, it.ID, orchapi.Link{Rel: "blocks", Target: "local:zzzzzz"}); !isCode(err, api.CodeNotFound) {
		t.Fatalf("missing target: %v", err)
	}
	full, err := orchclient.GetWork(h.ctx, all, it.ID)
	if err != nil || len(full.Links) != 4 || len(full.Comments) != 1 {
		t.Fatalf("get %+v %v", full, err)
	}
	if full.Links[2].Project != h.side.ID || full.Links[1].Project != "" {
		t.Fatalf("cross-project link %+v", full.Links)
	}

	// Ids are case-insensitive for local items; unknown ones are not_found.
	if _, err := orchclient.GetWork(h.ctx, all, strings.ToUpper(it.ID[6:])); !isCode(err, api.CodeNotFound) {
		t.Fatalf("bare ref: %v", err)
	}
	if _, err := orchclient.GetWork(h.ctx, all, "LOCAL:"+strings.ToUpper(it.ID[6:])); err != nil {
		t.Fatalf("upper-case id: %v", err)
	}
	if evs := h.events(orchapi.EventWorkCreated); len(evs) != 3 || evs[0].Project != h.harbor.ID || evs[0].Area != "solver" {
		t.Fatalf("created events %+v", evs)
	}
	if evs := h.events(orchapi.EventWorkLinked); len(evs) != 4 || strings.Contains(string(evs[0].Data), `"links"`) {
		t.Fatalf("linked events carry no links: %+v", evs)
	}
}

func TestClaimIsAtomic(t *testing.T) {
	h := newHarness(t)
	it, err := orchclient.CreateWork(h.ctx, h.as(h.owner, "harbor"), orchapi.CreateWorkRequest{Title: "Race"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			addr := "mini/racer" + string(rune('a'+i)) + "xx"[:2]
			sub := h.session(addr[:13], h.harbor.ID, "", false, orchapi.StateRunning)
			_, err := orchclient.Claim(h.ctx, h.as(sub, ""), it.ID, orchapi.ClaimRequest{})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				wins++
			case isCode(err, api.CodeConflict):
				conflicts++
			default:
				t.Errorf("claim: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 || conflicts != 11 {
		t.Fatalf("wins %d conflicts %d", wins, conflicts)
	}
}

func TestLeases(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	it, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "Lease me", Area: "solver"})
	a := h.as(h.session("mini/aaaaaaaa", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
	b := h.as(h.session("mini/bbbbbbbb", h.harbor.ID, "solver", false, orchapi.StateRunning), "")

	r, err := orchclient.Claim(h.ctx, a, it.ID, orchapi.ClaimRequest{TTLSeconds: 600, Note: "on it"})
	if err != nil || r.Lease.Holder != "mini/aaaaaaaa" || r.Lease.Bound || r.Lease.Expires == nil || r.Item.Status != orchapi.WorkClaimed ||
		r.Item.Owner != "mini/aaaaaaaa" || r.Lease.Area != "solver" {
		t.Fatalf("claim %+v %v", r, err)
	}
	first := *r.Lease.Expires
	if !first.Equal(h.clock.now().Add(600 * time.Second)) {
		t.Fatalf("deadline %v", first)
	}
	_, err = orchclient.Claim(h.ctx, b, it.ID, orchapi.ClaimRequest{})
	var e *api.Error
	if !isCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "mini/aaaaaaaa") {
		t.Fatalf("conflict: %v", err)
	}
	e = err.(*api.Error)
	var data orchapi.Lease
	if json.Unmarshal(e.Data, &data) != nil || data.Holder != "mini/aaaaaaaa" || data.Key != it.ID {
		t.Fatalf("conflict data %s", e.Data)
	}
	// Renewal moves the deadline, keeps claimed.
	h.clock.add(time.Minute)
	r2, err := orchclient.Claim(h.ctx, a, it.ID, orchapi.ClaimRequest{TTLSeconds: 600, Renew: true})
	if err != nil || !r2.Lease.Expires.After(first) || !r2.Lease.Claimed.Equal(r.Lease.Claimed) || r2.Lease.Note != "on it" {
		t.Fatalf("renew %+v %v", r2, err)
	}
	if _, err := orchclient.Claim(h.ctx, b, it.ID, orchapi.ClaimRequest{Renew: true}); !isCode(err, api.CodeConflict) {
		t.Fatalf("renew someone else's: %v", err)
	}
	if _, err := orchclient.Claim(h.ctx, a, it.ID, orchapi.ClaimRequest{TTLSeconds: 8 * 24 * 3600}); !isCode(err, api.CodeBadRequest) {
		t.Fatalf("ttl cap: %v", err)
	}
	// Who holds it and where mail goes.
	hr, err := orchclient.Holder(h.ctx, owner, it.ID)
	if err != nil || hr.Holder != "mini/aaaaaaaa" || hr.MailTo != "mini/aaaaaaaa" || hr.Via != "holder of "+it.ID {
		t.Fatalf("holder %+v %v", hr, err)
	}
	// Only the holder releases; force needs the local owner.
	if _, err := orchclient.Release(h.ctx, b, it.ID, orchapi.ReleaseRequest{}); !isCode(err, api.CodeForbidden) {
		t.Fatalf("release by another: %v", err)
	}
	guestSub := h.guest()
	if _, err := orchclient.Release(h.ctx, h.as(guestSub, ""), it.ID, orchapi.ReleaseRequest{Force: true}); !isCode(err, api.CodeForbidden) {
		t.Fatalf("guest force: %v", err)
	}
	rr, err := orchclient.Release(h.ctx, a, it.ID, orchapi.ReleaseRequest{})
	if err != nil || !rr.Released {
		t.Fatalf("release %+v %v", rr, err)
	}
	if got, _ := orchclient.GetWork(h.ctx, owner, it.ID); got.Status != orchapi.WorkOpen || got.Owner != "" {
		t.Fatalf("after release %+v", got)
	}
	if rr, err = orchclient.Release(h.ctx, a, it.ID, orchapi.ReleaseRequest{}); err != nil || rr.Released {
		t.Fatalf("release again %+v %v", rr, err)
	}
	// Force by the machine owner on the socket.
	if _, err := orchclient.Claim(h.ctx, b, it.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if rr, err = orchclient.Release(h.ctx, owner, it.ID, orchapi.ReleaseRequest{Force: true}); err != nil || !rr.Released {
		t.Fatalf("force %+v %v", rr, err)
	}
	// A done item cannot be claimed.
	if _, err := orchclient.SetStatus(h.ctx, owner, it.ID, orchapi.StatusRequest{Status: orchapi.WorkDone}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Claim(h.ctx, a, it.ID, orchapi.ClaimRequest{}); !isCode(err, api.CodeConflict) {
		t.Fatalf("claim done: %v", err)
	}

	// Plain locks: per (project, area, key).
	solver := h.as(h.owner, "harbor/solver")
	billing := h.as(h.d.Projects.Local("mini/cccccccc"), "harbor/billing")
	if _, err := orchclient.Lease(h.ctx, solver, orchapi.ClaimRequest{Key: "lock:migrations"}); err != nil {
		t.Fatal(err)
	}
	if lr, err := orchclient.Lease(h.ctx, billing, orchapi.ClaimRequest{Key: "lock:migrations"}); err != nil || lr.Lease.Area != "billing" {
		t.Fatalf("same key, other area %+v %v", lr, err)
	}
	if _, err := orchclient.Lease(h.ctx, h.as(h.d.Projects.Local("mini/cccccccc"), "harbor/solver"), orchapi.ClaimRequest{Key: "lock:migrations"}); !isCode(err, api.CodeConflict) {
		t.Fatalf("same key, same area: %v", err)
	}
	if _, err := orchclient.Lease(h.ctx, solver, orchapi.ClaimRequest{Key: "  "}); !isCode(err, api.CodeBadRequest) {
		t.Fatalf("empty key: %v", err)
	}
	// A lease key that is a work item of the project claims the item.
	if _, err := orchclient.SetStatus(h.ctx, owner, it.ID, orchapi.StatusRequest{Status: orchapi.WorkOpen}); err != nil {
		t.Fatal(err)
	}
	if lr, err := orchclient.Lease(h.ctx, owner, orchapi.ClaimRequest{Key: it.ID}); err != nil || lr.Item == nil || lr.Lease.Area != "solver" {
		t.Fatalf("lease on an item key %+v %v", lr, err)
	}
	ls, err := orchclient.Leases(h.ctx, h.as(h.owner, ""), orchapi.LeaseQuery{})
	if err != nil || len(ls) != 3 {
		t.Fatalf("leases %+v %v", ls, err)
	}
	for _, l := range ls {
		if l.Expires == nil {
			t.Fatalf("lease without deadline %+v", l)
		}
	}
	if ls, _ = orchclient.Leases(h.ctx, h.as(h.owner, ""), orchapi.LeaseQuery{Holder: "mini/cccccccc"}); len(ls) != 1 {
		t.Fatalf("holder filter %+v", ls)
	}
	if rr, err = orchclient.ReleaseLease(h.ctx, billing, "lock:migrations", orchapi.ReleaseRequest{}); err != nil || !rr.Released {
		t.Fatalf("release lock %+v %v", rr, err)
	}
	if _, err = orchclient.ReleaseLease(h.ctx, billing, "lock:nothing", orchapi.ReleaseRequest{}); !isCode(err, api.CodeNotFound) {
		t.Fatalf("release missing lock: %v", err)
	}
	inf, err := orchclient.Inflight(h.ctx, h.as(h.owner, ""), "")
	if err != nil || len(inf) != 2 {
		t.Fatalf("inflight %+v %v", inf, err)
	}
	for _, row := range inf {
		if row.Lease.Key == it.ID && (row.Item == nil || row.Item.Title != "Lease me") {
			t.Fatalf("inflight item %+v", row)
		}
	}
}

// A guest of harbor/solver sees solver only: other areas, other projects
// and their holders answer exactly like missing things.
func TestGuestScope(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	solverItem, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "VISIBLE", Area: "solver"})
	billingItem, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "HIDDEN billing", Area: "billing"})
	projItem, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "HIDDEN project-level"})
	sideItem, _ := orchclient.CreateWork(h.ctx, h.as(h.owner, "side"), orchapi.CreateWorkRequest{Title: "HIDDEN side"})
	hidden := h.session("mini/hidden01", h.harbor.ID, "billing", false, orchapi.StateRunning)
	if _, err := orchclient.Claim(h.ctx, h.as(hidden, ""), solverItem.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Comment(h.ctx, h.as(hidden, ""), solverItem.ID, orchapi.CommentRequest{Body: "from a hidden session"}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{billingItem.ID, sideItem.ID, projItem.ID} {
		if _, err := orchclient.Link(h.ctx, owner, solverItem.ID, orchapi.Link{Rel: "blocks", Target: target}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := orchclient.Link(h.ctx, owner, solverItem.ID, orchapi.Link{Rel: "url", Target: "https://example.com/x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Lease(h.ctx, h.as(h.owner, "harbor/billing"), orchapi.ClaimRequest{Key: "lock:HIDDEN"}); err != nil {
		t.Fatal(err)
	}

	g := h.as(h.guest(), "")
	items, err := orchclient.Work(h.ctx, g, orchapi.WorkQuery{})
	if err != nil || len(items) != 1 || items[0].ID != solverItem.ID || items[0].Owner != events.Someone {
		t.Fatalf("guest list %+v %v", items, err)
	}
	it, err := orchclient.GetWork(h.ctx, g, solverItem.ID)
	if err != nil {
		t.Fatal(err)
	}
	js, _ := json.Marshal(it)
	for _, bad := range []string{"HIDDEN", "mini/hidden01", billingItem.ID, sideItem.ID, projItem.ID, h.side.ID} {
		if strings.Contains(string(js), bad) {
			t.Fatalf("guest item leaks %q: %s", bad, js)
		}
	}
	if len(it.Links) != 2 || !it.Links[0].Hidden || it.Links[0].Rel != "blocks" || it.Links[1].Target != "https://example.com/x" {
		t.Fatalf("hidden links not collapsed per rel: %+v", it.Links)
	}
	if it.Comments[0].Author != events.Someone {
		t.Fatalf("comment author %+v", it.Comments)
	}
	// Hidden ids answer exactly like missing ones on every item route.
	for _, id := range []string{billingItem.ID, sideItem.ID, projItem.ID} {
		_, errH := orchclient.GetWork(h.ctx, g, id)
		_, errM := orchclient.GetWork(h.ctx, g, "local:zzzzzz")
		if !isCode(errH, api.CodeNotFound) || strings.ReplaceAll(errH.Error(), id, "X") != strings.ReplaceAll(errM.Error(), "local:zzzzzz", "X") {
			t.Fatalf("hidden %v vs missing %v", errH, errM)
		}
		if _, err := orchclient.Claim(h.ctx, g, id, orchapi.ClaimRequest{}); !isCode(err, api.CodeNotFound) {
			t.Fatalf("claim hidden: %v", err)
		}
		if _, err := orchclient.Link(h.ctx, g, solverItem.ID, orchapi.Link{Rel: "related", Target: id}); !isCode(err, api.CodeNotFound) {
			t.Fatalf("link to hidden: %v", err)
		}
	}
	if _, err := orchclient.Link(h.ctx, g, solverItem.ID, orchapi.Link{Rel: "session", Target: "mini/hidden01"}); !isCode(err, api.CodeNotFound) ||
		!strings.Contains(err.Error(), `no session "mini/hidden01"`) {
		t.Fatalf("link to hidden session: %v", err)
	}
	// Conflict data is redacted.
	_, err = orchclient.Claim(h.ctx, g, solverItem.ID, orchapi.ClaimRequest{})
	if !isCode(err, api.CodeConflict) || strings.Contains(err.Error(), "hidden01") || strings.Contains(string(err.(*api.Error).Data), "hidden01") {
		t.Fatalf("conflict leaks the holder: %v %s", err, err.(*api.Error).Data)
	}
	hr, err := orchclient.Holder(h.ctx, g, solverItem.ID)
	if err != nil || hr.Holder != events.Someone || hr.MailTo != events.Someone {
		t.Fatalf("holder %+v %v", hr, err)
	}
	ls, _ := orchclient.Leases(h.ctx, g, orchapi.LeaseQuery{})
	if len(ls) != 1 || ls[0].Holder != events.Someone {
		t.Fatalf("guest leases %+v", ls)
	}
	inf, _ := orchclient.Inflight(h.ctx, g, "")
	if len(inf) != 1 || inf[0].Session != nil || inf[0].Lease.Holder != events.Someone {
		t.Fatalf("guest inflight %+v", inf)
	}
	if _, err := orchclient.ReleaseLease(h.ctx, g, "lock:HIDDEN", orchapi.ReleaseRequest{}); !isCode(err, api.CodeNotFound) {
		t.Fatalf("release hidden lock: %v", err)
	}
	// Hidden project names: like missing ones.
	_, e1 := orchclient.Work(h.ctx, h.as(h.guest(), "side"), orchapi.WorkQuery{})
	_, e2 := orchclient.Work(h.ctx, h.as(h.guest(), "nope"), orchapi.WorkQuery{})
	if !isCode(e1, api.CodeNotFound) || strings.ReplaceAll(e1.Error(), "side", "X") != strings.ReplaceAll(e2.Error(), "nope", "X") {
		t.Fatalf("hidden project %v vs %v", e1, e2)
	}
	if items, _ := orchclient.Work(h.ctx, g, orchapi.WorkQuery{Area: "billing"}); len(items) != 0 {
		t.Fatalf("hidden area filter %+v", items)
	}
	// Guest writes land in its area by default; other areas are not_found.
	if w, err := orchclient.CreateWork(h.ctx, h.as(h.guest(), "harbor"), orchapi.CreateWorkRequest{Title: "guest item"}); err != nil || w.Area != "solver" {
		t.Fatalf("guest create %+v %v", w, err)
	}
	if _, err := orchclient.CreateWork(h.ctx, h.as(h.guest(), "harbor"), orchapi.CreateWorkRequest{Title: "x", Area: "billing"}); !isCode(err, api.CodeNotFound) {
		t.Fatalf("guest create in hidden area: %v", err)
	}
	srcs, err := orchclient.WorkSources(h.ctx, g, "")
	if err != nil || len(srcs) != 1 || srcs[0].Project != h.harbor.ID {
		t.Fatalf("guest sources %+v %v", srcs, err)
	}
}

func TestTickerTTL(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	it, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "Expire me", Area: "solver"})
	a := h.as(h.session("mini/aaaaaaaa", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
	if _, err := orchclient.Claim(h.ctx, a, it.ID, orchapi.ClaimRequest{TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Lease(h.ctx, h.as(h.owner, "harbor"), orchapi.ClaimRequest{Key: "lock:deploy", TTLSeconds: 120}); err != nil {
		t.Fatal(err)
	}
	h.s.tick(h.ctx)
	if len(h.mail.all()) != 0 {
		t.Fatal("escalated a live lease")
	}
	h.clock.add(61 * time.Second)
	// Expired but not yet ticked: lists already hide it.
	if ls, _ := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{}); len(ls) != 1 {
		t.Fatalf("expired lease still listed %+v", ls)
	}
	h.s.tick(h.ctx)
	sent := h.mail.all()
	if len(sent) != 1 || sent[0].req.To != "mini/aaaaaaaa" || sent[0].req.Needs != orchapi.NeedsAgent || sent[0].req.Kind != orchapi.MailKindNotice || sent[0].req.Ref != it.ID ||
		sent[0].project.ID != h.harbor.ID || sent[0].area != "solver" || sent[0].from != "machine:mini" {
		t.Fatalf("mail to the holder %+v", sent)
	}
	if evs := h.events(orchapi.EventWorkLeaseExpired); len(evs) != 1 || evs[0].Subject != it.ID || evs[0].Area != "solver" {
		t.Fatalf("lease_expired events %+v", evs)
	}
	if got, _ := orchclient.GetWork(h.ctx, owner, it.ID); got.Status != orchapi.WorkOpen || got.Owner != "" {
		t.Fatalf("item after expiry %+v", got)
	}
	// The old holder must claim again; the key is free for anyone.
	if _, err := orchclient.Claim(h.ctx, a, it.ID, orchapi.ClaimRequest{Renew: true}); !isCode(err, api.CodeConflict) {
		t.Fatalf("renew after expiry: %v", err)
	}
	h.clock.add(time.Minute)
	h.s.tick(h.ctx)
	// That lock's holder is a person: nobody is mailed.
	if sent = h.mail.all(); len(sent) != 1 {
		t.Fatalf("mail for a person's lock %+v", sent)
	}
	if evs := h.events(orchapi.EventWorkLeaseExpired); len(evs) != 2 {
		t.Fatalf("lock's lease_expired %+v", evs)
	}
	h.s.tick(h.ctx)
	if len(h.mail.all()) != 1 {
		t.Fatal("mailed twice")
	}
}

func TestTickerBound(t *testing.T) {
	h := newHarness(t)
	h.d.Config.File.BoundLeaseTTLSeconds = 600
	owner := h.as(h.owner, "harbor")
	items := map[string]orchapi.WorkItem{}
	for _, n := range []string{"run", "idle", "ended", "spawn"} {
		items[n], _ = orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: n, Area: "solver"})
	}
	runSub := h.session("mini/run00001", h.harbor.ID, "solver", true, orchapi.StateRunning)
	idleSub := h.session("mini/idle0001", h.harbor.ID, "solver", true, orchapi.StateRunning)
	endSub := h.session("mini/ended001", h.harbor.ID, "solver", true, orchapi.StateRunning)
	for n, sub := range map[string]*authz.Subject{"run": runSub, "idle": idleSub, "ended": endSub} {
		r, err := orchclient.Claim(h.ctx, h.as(sub, ""), items[n].ID, orchapi.ClaimRequest{})
		if err != nil || !r.Lease.Bound || !r.Lease.Expires.Equal(h.clock.now().Add(600*time.Second)) {
			t.Fatalf("bound claim %s %+v %v", n, r, err)
		}
	}
	// spawn --work: BindLease.
	h.sessions.put(orchapi.Session{Address: "mini/spawn001", Project: h.harbor.ID, Area: "solver", Managed: true, State: orchapi.StateRunning})
	bl, err := h.s.BindLease(h.ctx, h.owner, "harbor", items["spawn"].ID, "mini/spawn001")
	if err != nil || !bl.Bound || bl.Holder != "mini/spawn001" {
		t.Fatalf("bind %+v %v", bl, err)
	}

	h.sessions.setState("mini/idle0001", orchapi.StateIdle)
	h.sessions.setState("mini/ended001", orchapi.StateEnded)
	h.clock.add(300 * time.Second)
	h.s.tick(h.ctx)
	ls, _ := orchclient.Leases(h.ctx, h.as(h.owner, ""), orchapi.LeaseQuery{})
	exp := map[string]time.Time{}
	for _, l := range ls {
		exp[l.Holder] = *l.Expires
	}
	if _, ok := exp["mini/ended001"]; ok || len(ls) != 3 {
		t.Fatalf("ended holder's lease not released: %+v", ls)
	}
	if !exp["mini/run00001"].Equal(h.clock.now().Add(600*time.Second)) || exp["mini/idle0001"].Equal(h.clock.now().Add(600*time.Second)) {
		t.Fatalf("renewal: %+v", exp)
	}
	if rel := h.events(orchapi.EventWorkReleased); len(rel) != 1 || !strings.Contains(string(rel[0].Data), "session ended") {
		t.Fatalf("released event %+v", rel)
	}
	if got, _ := orchclient.GetWork(h.ctx, owner, items["ended"].ID); got.Status != orchapi.WorkOpen {
		t.Fatalf("ended holder's item %+v", got)
	}
	// The idle holder is past its deadline after 600 s without running: expire + tell it.
	h.clock.add(301 * time.Second)
	h.s.tick(h.ctx)
	if sent := h.mail.all(); len(sent) != 1 || sent[0].req.Ref != items["idle"].ID || sent[0].req.To != "mini/idle0001" {
		t.Fatalf("mail to the idle holder %+v", sent)
	}
	// SessionEnded releases at once.
	if err := h.s.SessionEnded(h.ctx, "mini/spawn001"); err != nil {
		t.Fatal(err)
	}
	ls, _ = orchclient.Leases(h.ctx, h.as(h.owner, ""), orchapi.LeaseQuery{})
	if len(ls) != 1 || ls[0].Holder != "mini/run00001" {
		t.Fatalf("after SessionEnded %+v", ls)
	}
}

func TestMailTarget(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	it, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "Mail me", Area: "solver"})
	hiddenItem, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "x", Area: "billing"})
	to, via, area, err := h.s.MailTarget(h.ctx, h.owner, h.harbor.ID, it.ID)
	if err != nil || to != "human:ana" || via != "creator of "+it.ID || area != "solver" {
		t.Fatalf("creator %q %q %q %v", to, via, area, err)
	}
	sub := h.session("mini/aaaaaaaa", h.harbor.ID, "", false, orchapi.StateRunning) // project-level: hidden from solver guests
	if _, err := orchclient.Claim(h.ctx, h.as(sub, ""), it.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if to, via, _, err = h.s.MailTarget(h.ctx, h.owner, "harbor", it.ID); err != nil || to != "mini/aaaaaaaa" || via != "holder of "+it.ID {
		t.Fatalf("holder %q %q %v", to, via, err)
	}
	if o, err := h.s.OwnerOf(h.ctx, h.guest(), "harbor", it.ID); err != nil || o != events.Someone {
		t.Fatalf("OwnerOf for a guest %q %v", o, err)
	}
	// An item whose creator session has ended reaches the group mailbox,
	// not a dead inbox.
	maker := h.session("mini/c0ffee00", h.harbor.ID, "", false, orchapi.StateRunning)
	made, err := orchclient.CreateWork(h.ctx, h.as(maker, ""), orchapi.CreateWorkRequest{Title: "made by a session", Area: "solver"})
	if err != nil {
		t.Fatal(err)
	}
	if to, _, _, err = h.s.MailTarget(h.ctx, h.owner, "harbor", made.ID); err != nil || to != "mini/c0ffee00" {
		t.Fatalf("live creator %q %v", to, err)
	}
	h.sessions.setState("mini/c0ffee00", orchapi.StateEnded)
	if to, via, _, err = h.s.MailTarget(h.ctx, h.owner, "harbor", made.ID); err != nil || to != "machine:mini" || via != "creator of "+made.ID+" (that session has ended)" {
		t.Fatalf("ended creator %q %q %v", to, via, err)
	}
	g := h.guest()
	_, _, _, errH := h.s.MailTarget(h.ctx, g, h.harbor.ID, hiddenItem.ID)
	_, _, _, errM := h.s.MailTarget(h.ctx, g, h.harbor.ID, "local:zzzzzz")
	_, _, _, errP := h.s.MailTarget(h.ctx, g, h.side.ID, it.ID)
	for _, e := range []error{errH, errM, errP} {
		if !isCode(e, api.CodeNotFound) {
			t.Fatalf("hidden/missing: %v %v %v", errH, errM, errP)
		}
	}
}

func TestEventsScoped(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	solverItem, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "VISIBLE", Area: "solver"})
	_, _ = orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "HIDDEN", Area: "billing"})
	hidden := h.session("mini/hidden01", h.harbor.ID, "billing", false, orchapi.StateRunning)
	if _, err := orchclient.Claim(h.ctx, h.as(hidden, ""), solverItem.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	evs, err := h.d.Events.Since(h.ctx, h.guest(), "", events.Filter{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 { // created, claimed, status_changed (holder redacted in both)
		t.Fatalf("guest events %d", len(evs))
	}
	for _, e := range evs {
		js, _ := json.Marshal(e)
		if strings.Contains(string(js), "HIDDEN") || strings.Contains(string(js), "hidden01") {
			t.Fatalf("event leaks: %s", js)
		}
	}
}

func TestTracker(t *testing.T) {
	h := newHarness(t)
	state := h.installFake()
	st, _ := fake.Load(state)
	st.Issues["solver-1"] = &fake.Issue{ID: "solver-1", Title: "Fix solver timeout", Status: "open", CreatedBy: "human:ana"}
	st.Issues["solver-4"] = &fake.Issue{ID: "solver-4", Title: "Taken in beads", Status: "in_progress", Assignee: "bea"}
	st.Issues["billing-2"] = &fake.Issue{ID: "billing-2", Title: "HIDDEN invoice bug", Status: "open"}
	st.Issues["x-3"] = &fake.Issue{ID: "x-3", Title: "HIDDEN project-level", Status: "open"}
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	// A broken adapter (another protocol) and a missing one sit next to it.
	dir := t.TempDir()
	old := dir + "/" + adapter.ExecPrefix + "old"
	if err := os.WriteFile(old, []byte("#!/bin/sh\ncat >/dev/null\necho '{\"ok\":true,\"result\":{\"zm\":\"zm/v9\",\"name\":\"old\",\"prefix\":\"old\",\"verbs\":[]}}'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.d.Config.File.WorkAdapters = []string{old}
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd", Config: map[string]string{"actor": "zm"}},
		{Adapter: "old", Prefix: "old"}, {Adapter: "nope", Prefix: "np"}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	owner := h.as(h.owner, "")
	srcs, err := orchclient.WorkSources(h.ctx, owner, "harbor")
	if err != nil || len(srcs) != 4 || !srcs[1].OK || srcs[1].Path == "" || srcs[2].OK || !strings.Contains(srcs[2].Error, "zm/v9") || srcs[3].OK {
		t.Fatalf("sources %+v %v", srcs, err)
	}
	items, err := orchclient.Work(h.ctx, owner, orchapi.WorkQuery{Source: "bd"})
	if err != nil || len(items) != 4 {
		t.Fatalf("owner tracker list %+v %v", items, err)
	}
	// A broken or missing tracker is never dropped silently: a full list
	// names it in Skipped, and naming it with --source fails with its error.
	all, err := orchclient.WorkList(h.ctx, owner, orchapi.WorkQuery{})
	if err != nil || len(all.Items) != 4 || len(all.Skipped) != 2 ||
		all.Skipped[0].Project != h.harbor.ID || all.Skipped[0].Source != "old" || !strings.Contains(all.Skipped[0].Error, "zm/v9") ||
		all.Skipped[1].Source != "np" || all.Skipped[1].Error == "" {
		t.Fatalf("skipped trackers %+v %v", all, err)
	}
	if _, err := orchclient.Work(h.ctx, owner, orchapi.WorkQuery{Source: "old"}); !isCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "zm/v9") {
		t.Fatalf("--source on a broken tracker: %v", err)
	}
	if _, err := orchclient.Work(h.ctx, owner, orchapi.WorkQuery{Source: "np"}); err == nil {
		t.Fatal("--source on a missing tracker succeeded")
	}
	if l, err := orchclient.WorkList(h.ctx, owner, orchapi.WorkQuery{Source: "bd"}); err != nil || len(l.Skipped) != 0 {
		t.Fatalf("a named healthy tracker reports skips %+v %v", l, err)
	}
	areas := map[string]string{}
	for _, it := range items {
		areas[it.ID] = it.Area
	}
	if areas["bd:solver-1"] != "solver" || areas["bd:billing-2"] != "billing" || areas["bd:x-3"] != "" {
		t.Fatalf("areas from work globs %+v", areas)
	}

	g := h.as(h.guest(), "")
	gl, err := orchclient.WorkList(h.ctx, g, orchapi.WorkQuery{})
	gItems := gl.Items
	if err != nil || len(gItems) != 2 || gItems[0].ID != "bd:solver-1" || gItems[1].ID != "bd:solver-4" || len(gl.Skipped) != 0 {
		t.Fatalf("guest tracker list (trackers it cannot see are not even skipped) %+v %v", gl, err)
	}
	_, errH := orchclient.GetWork(h.ctx, g, "bd:billing-2")
	_, errM := orchclient.GetWork(h.ctx, g, "bd:solver-999")
	if !isCode(errH, api.CodeNotFound) || strings.ReplaceAll(errH.Error(), "bd:billing-2", "X") != strings.ReplaceAll(errM.Error(), "bd:solver-999", "X") {
		t.Fatalf("hidden %v vs missing %v", errH, errM)
	}
	gs, _ := orchclient.WorkSources(h.ctx, g, "")
	for _, s := range gs {
		if s.Path != "" || strings.Contains(s.Error, "/") {
			t.Fatalf("guest sees adapter internals: %+v", s)
		}
	}
	// A broken tracker the guest can see is reported to it, sanitised.
	trs2 := append(trs, api.Tracker{Adapter: "old", Prefix: "sv", Area: "solver"})
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs2}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	gl, err = orchclient.WorkList(h.ctx, g, orchapi.WorkQuery{})
	if err != nil || len(gl.Items) != 2 || len(gl.Skipped) != 1 || gl.Skipped[0].Source != "sv" ||
		!strings.Contains(gl.Skipped[0].Error, "the old tracker is unavailable") || strings.Contains(gl.Skipped[0].Error, "/") {
		t.Fatalf("guest skipped %+v %v", gl, err)
	}
	if _, err := orchclient.Work(h.ctx, g, orchapi.WorkQuery{Source: "sv"}); !isCode(err, api.CodeUnavailable) || strings.Contains(err.Error(), "zm/v9") {
		t.Fatalf("guest --source on a broken tracker: %v", err)
	}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()

	sess := h.as(h.session("mini/aaaaaaaa", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
	r, err := orchclient.Claim(h.ctx, sess, "bd:solver-1", orchapi.ClaimRequest{})
	if err != nil || r.Item.Owner != "mini/aaaaaaaa" || r.Item.Status != orchapi.WorkClaimed || r.Lease.Area != "solver" {
		t.Fatalf("tracker claim %+v %v", r, err)
	}
	st, _ = fake.Load(state)
	if is := st.Issues["solver-1"]; is.Status != "in_progress" || is.Assignee != "mini/aaaaaaaa" {
		t.Fatalf("bd after claim %+v", is)
	}
	// The tracker arbitrates: its conflict refuses and no lease is left behind.
	if _, err := orchclient.Claim(h.ctx, sess, "bd:solver-4", orchapi.ClaimRequest{}); !isCode(err, api.CodeConflict) {
		t.Fatalf("tracker conflict: %v", err)
	}
	if ls, _ := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{}); len(ls) != 1 {
		t.Fatalf("lease left after a tracker refusal %+v", ls)
	}
	if _, err := orchclient.Comment(h.ctx, sess, "bd:solver-1", orchapi.CommentRequest{Body: "on it"}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Link(h.ctx, sess, "bd:solver-1", orchapi.Link{Rel: "pr", Target: "https://github.com/o/r/pull/1"}); err != nil {
		t.Fatal(err)
	}
	full, err := orchclient.GetWork(h.ctx, owner, "bd:solver-1")
	if err != nil || len(full.Comments) != 1 || len(full.Links) != 1 || full.Owner != "mini/aaaaaaaa" {
		t.Fatalf("tracker item %+v %v", full, err)
	}
	if to, via, _, err := h.s.MailTarget(h.ctx, h.owner, "harbor", "bd:solver-1"); err != nil || to != "mini/aaaaaaaa" || via != "holder of bd:solver-1" {
		t.Fatalf("mail target %q %q %v", to, via, err)
	}
	if rr, err := orchclient.Release(h.ctx, sess, "bd:solver-1", orchapi.ReleaseRequest{}); err != nil || !rr.Released {
		t.Fatalf("release %+v %v", rr, err)
	}
	// The claim and the release moved the tracker item's status (open ->
	// claimed -> open): each published work.status_changed (cockpit gap G3).
	var moves []orchapi.WorkStatus
	for _, e := range h.events(orchapi.EventWorkStatusChanged) {
		var w orchapi.WorkItem
		if e.Subject == "bd:solver-1" && json.Unmarshal(e.Data, &w) == nil {
			moves = append(moves, w.Status)
		}
	}
	if len(moves) != 2 || moves[0] != orchapi.WorkClaimed || moves[1] != orchapi.WorkOpen {
		t.Fatalf("tracker status events %v", moves)
	}
	st, _ = fake.Load(state)
	is := st.Issues["solver-1"]
	if is.Status != "open" || is.Assignee != "" || len(is.CommentLog) != 1 || !strings.HasPrefix(is.CommentLog[0], "mini/aaaaaaaa: ") ||
		!strings.Contains(is.Notes, "zm: link pr https://github.com/o/r/pull/1") {
		t.Fatalf("bd after release %+v", is)
	}
	for _, c := range st.Calls {
		if !strings.HasSuffix(c, " [actor=zm]") {
			t.Fatalf("tracker config not passed: %q", c)
		}
	}
}

// A project-level lock covers every area: an owner's project-level session
// and a guest's solver session never both hold lock:migrations. Two areas
// stay independent (a hidden area's lock neither conflicts nor shows).
func TestProjectLeaseCoversAreas(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	g := h.as(h.guest(), "harbor")
	proj := h.as(h.session("mini/projlvl1", h.harbor.ID, "", false, orchapi.StateRunning), "harbor")
	if _, err := orchclient.Lease(h.ctx, proj, orchapi.ClaimRequest{Key: "lock:deploy"}); err != nil {
		t.Fatal(err)
	}
	_, err := orchclient.Lease(h.ctx, g, orchapi.ClaimRequest{Key: "lock:deploy"})
	if !isCode(err, api.CodeConflict) {
		t.Fatalf("guest took a key the project-level owner holds: %v", err)
	}
	if strings.Contains(err.Error(), "projlvl1") || strings.Contains(string(err.(*api.Error).Data), "projlvl1") {
		t.Fatalf("conflict names the hidden holder: %v %s", err, err.(*api.Error).Data)
	}
	// The other way round: the area holds it first, the project-level claim waits.
	if _, err := orchclient.Lease(h.ctx, g, orchapi.ClaimRequest{Key: "lock:migrations"}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Lease(h.ctx, owner, orchapi.ClaimRequest{Key: "lock:migrations"}); !isCode(err, api.CodeConflict) {
		t.Fatalf("project-level claim over a live area lease: %v", err)
	}
	// Areas are independent of each other.
	if _, err := orchclient.Lease(h.ctx, h.as(h.owner, "harbor/billing"), orchapi.ClaimRequest{Key: "lock:migrations"}); err != nil {
		t.Fatalf("billing blocked by solver: %v", err)
	}
	ls, _ := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{})
	n := 0
	for _, l := range ls {
		if l.Key == "lock:deploy" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("lock:deploy held %d times: %+v", n, ls)
	}
	// Once the project-level lease expires the area may take it.
	h.clock.add(2 * time.Hour)
	if _, err := orchclient.Lease(h.ctx, g, orchapi.ClaimRequest{Key: "lock:deploy"}); err != nil {
		t.Fatalf("after expiry: %v", err)
	}
}

// An expiry mails only a session that can still act on it, a plain lock is
// named in the body, and a run-target lease runs out quietly. Owners get nothing.
func TestExpiryTellsOnlyALiveHolder(t *testing.T) {
	h := newHarness(t)
	owner := h.as(h.owner, "harbor")
	live, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "live", Area: "solver"})
	gone, _ := orchclient.CreateWork(h.ctx, owner, orchapi.CreateWorkRequest{Title: "gone", Area: "solver"})
	a := h.as(h.session("mini/liveone1", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
	b := h.as(h.session("mini/goneone1", h.harbor.ID, "solver", false, orchapi.StateRunning), "")
	if _, err := orchclient.Claim(h.ctx, a, live.ID, orchapi.ClaimRequest{TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Claim(h.ctx, b, gone.ID, orchapi.ClaimRequest{TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Lease(h.ctx, a, orchapi.ClaimRequest{Key: "lock:migrations", TTLSeconds: 60}); err != nil {
		t.Fatal(err)
	}
	h.sessions.setState("mini/goneone1", orchapi.StateEnded)
	h.clock.add(61 * time.Second)
	h.s.tickExpire(h.ctx)
	sent := h.mail.all()
	if len(sent) != 2 {
		t.Fatalf("mail %+v", sent)
	}
	for _, m := range sent {
		if m.req.To != "mini/liveone1" || m.req.Needs != orchapi.NeedsAgent {
			t.Fatalf("mail to %q, needs %q: %+v", m.req.To, m.req.Needs, m)
		}
	}
	if !strings.Contains(sent[0].req.Body+sent[1].req.Body, "lock:migrations") {
		t.Fatalf("the lock is not named: %+v", sent)
	}
	if evs := h.events(orchapi.EventWorkLeaseExpired); len(evs) != 3 {
		t.Fatalf("every expiry stays an event: %+v", evs)
	}
	if got, _ := orchclient.GetWork(h.ctx, owner, gone.ID); got.Status != orchapi.WorkOpen {
		t.Fatalf("a gone holder's item is not open: %+v", got)
	}
}

// Dogfood (live beads, 11.5k issues): no issue id starts with solver-; the
// solver work carries the label "solver". An area matched by labels must
// hold those items for a guest, by list (pushed down to bd) and by id, and
// work sources must not call a tracker ok when its server is unreachable.
func TestAreaByLabelsAndSourceProbe(t *testing.T) {
	h := newHarness(t)
	state := h.installFake()
	st, _ := fake.Load(state)
	for i := 0; i < 150; i++ { // a page of other work sorts first
		id := fmt.Sprintf("harbor-a%03d", i)
		st.Issues[id] = &fake.Issue{ID: id, Title: "HIDDEN other", Status: "open", Labels: []string{"ux"}}
	}
	st.Issues["harbor-z1"] = &fake.Issue{ID: "harbor-z1", Title: "Research one", Status: "open", Labels: []string{"research"}}
	st.Issues["harbor-z2"] = &fake.Issue{ID: "harbor-z2", Title: "Research two", Status: "in_progress", Assignee: "bea", Labels: []string{"ux", "research"}}
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Projects.AddArea(h.ctx, h.owner, "harbor", api.Area{Name: "research", Labels: []string{"research"}}); err != nil {
		t.Fatal(err)
	}
	down := filepath.Join(t.TempDir(), "down") // the bx tracker's server is unreachable
	if err := os.WriteFile(down, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd"}, {Adapter: "fake", Prefix: "bx", Config: map[string]string{"down": down}}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	owner := h.as(h.owner, "")
	srcs, err := orchclient.WorkSources(h.ctx, owner, "harbor")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range srcs {
		if s.Prefix == "bx" && (s.OK || !strings.Contains(s.Error, "unreachable")) {
			t.Fatalf("an unreachable tracker reported ok: %+v", s)
		}
		if s.Prefix == "bd" && !s.OK {
			t.Fatalf("a working tracker reported broken: %+v", s)
		}
	}
	z1, err := orchclient.GetWork(h.ctx, owner, "bd:harbor-z1")
	if err != nil || z1.Area != "research" {
		t.Fatalf("owner get by label area %+v %v", z1, err)
	}
	if a, err := orchclient.GetWork(h.ctx, owner, "bd:harbor-a001"); err != nil || a.Area != "" {
		t.Fatalf("unlabeled item %+v %v", a, err)
	}

	tok, err := h.d.Projects.Mint(h.ctx, h.owner, api.MintTokenRequest{As: "rae", Project: "harbor", Area: "research", Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	gs, err := h.d.Projects.FromToken(h.ctx, tok.Secret, true)
	if err != nil {
		t.Fatal(err)
	}
	g := h.as(gs, "")
	gl, err := orchclient.WorkList(h.ctx, g, orchapi.WorkQuery{Source: "bd"})
	if err != nil || len(gl.Items) != 2 || gl.Items[0].ID != "bd:harbor-z1" || gl.Items[1].ID != "bd:harbor-z2" {
		t.Fatalf("guest list by labels (pushed down past 150 others) %+v %v", gl, err)
	}
	if it, err := orchclient.GetWork(h.ctx, g, "bd:harbor-z2"); err != nil || it.Area != "research" {
		t.Fatalf("guest get %+v %v", it, err)
	}
	_, errH := orchclient.GetWork(h.ctx, g, "bd:harbor-a001")
	_, errM := orchclient.GetWork(h.ctx, g, "bd:harbor-nope")
	if !isCode(errH, api.CodeNotFound) || strings.ReplaceAll(errH.Error(), "bd:harbor-a001", "X") != strings.ReplaceAll(errM.Error(), "bd:harbor-nope", "X") {
		t.Fatalf("hidden %v vs missing %v", errH, errM)
	}
	// A tracker that is down says so (generic for a guest), never "no work item".
	if _, err := orchclient.GetWork(h.ctx, g, "bx:harbor-z1"); !isCode(err, api.CodeUnavailable) || strings.Contains(err.Error(), "Dolt") {
		t.Fatalf("guest get on a down tracker: %v", err)
	}
	if _, err := orchclient.GetWork(h.ctx, owner, "bx:harbor-z1"); !isCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("owner get on a down tracker: %v", err)
	}
	// The owner narrowing to the area gets the same push-down.
	ol, err := orchclient.WorkList(h.ctx, owner, orchapi.WorkQuery{Source: "bd", Area: "research"})
	if err != nil || len(ol.Items) != 2 {
		t.Fatalf("owner --area research %+v %v", ol, err)
	}
	// A tracker holder that is not a zm address is named, not "nobody".
	if hr, err := orchclient.Holder(h.ctx, owner, "bd:harbor-z2"); err != nil || hr.TrackerOwner != "bea" || hr.MailTo != "" {
		t.Fatalf("holder %+v %v", hr, err)
	}
	// An area matched by an id glob and by labels lists both kinds.
	if _, err := h.d.Projects.AddArea(h.ctx, h.owner, "harbor", api.Area{Name: "mixed", Work: []string{"bd:a-mx-*"}, Labels: []string{"mixed"}}); err != nil {
		t.Fatal(err)
	}
	st, _ = fake.Load(state)
	st.Issues["a-mx-1"] = &fake.Issue{ID: "a-mx-1", Title: "By id", Status: "open", UpdatedAt: fake.Now()}
	st.Issues["harbor-y1"] = &fake.Issue{ID: "harbor-y1", Title: "By label", Status: "open", Labels: []string{"mixed"}, UpdatedAt: fake.Now()}
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	h.sync()
	ml, err := orchclient.WorkList(h.ctx, owner, orchapi.WorkQuery{Source: "bd", Area: "mixed"})
	if err != nil || len(ml.Items) != 2 {
		t.Fatalf("mixed area %+v %v", ml, err)
	}
}

// Review: `zm area set --label` must move items that are
// already known in and out of the area on the next read, with no restart and
// no wait for a sync. Here items are located per request; once reads come
// from a mirror this is the cross-stream check that the mirror re-places
// stored items when the rules change (its rules fingerprint covers areas).
func TestAreaSetMovesItemsLive(t *testing.T) {
	h := newHarness(t)
	state := h.installFake()
	st, _ := fake.Load(state)
	st.Issues["harbor-q1"] = &fake.Issue{ID: "harbor-q1", Title: "QA one", Status: "open", Labels: []string{"qa"}}
	st.Issues["harbor-q2"] = &fake.Issue{ID: "harbor-q2", Title: "Other", Status: "open"}
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd"}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Projects.AddArea(h.ctx, h.owner, "harbor", api.Area{Name: "qa", Work: []string{"bd:nothing-*"}}); err != nil {
		t.Fatal(err)
	}
	tok, err := h.d.Projects.Mint(h.ctx, h.owner, api.MintTokenRequest{As: "quinn", Project: "harbor", Area: "qa", Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	gs, err := h.d.Projects.FromToken(h.ctx, tok.Secret, true)
	if err != nil {
		t.Fatal(err)
	}
	g, owner := h.as(gs, ""), h.as(h.owner, "")
	ids := func(l orchapi.WorkList) (out []string) {
		for _, it := range l.Items {
			out = append(out, it.ID)
		}
		return out
	}
	// Owner reads first, so the items are known before the rules change. The
	// mirror syncs once here and not again: area set must re-place stored items.
	h.sync()
	if it, err := orchclient.GetWork(h.ctx, owner, "bd:harbor-q1"); err != nil || it.Area != "" {
		t.Fatalf("before: %+v %v", it, err)
	}
	if l, err := orchclient.WorkList(h.ctx, g, orchapi.WorkQuery{Source: "bd"}); err != nil || len(l.Items) != 0 {
		t.Fatalf("guest before: %v %v", ids(l), err)
	}
	// A lease taken before the move follows the item (area set must not strand it).
	if _, err := orchclient.Claim(h.ctx, owner, "bd:harbor-q1", orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	set := func(labels ...string) {
		t.Helper()
		if labels == nil {
			labels = []string{}
		}
		if _, err := h.d.Projects.UpdateArea(h.ctx, h.owner, "harbor", "qa", api.UpdateAreaRequest{Labels: &labels}); err != nil {
			t.Fatal(err)
		}
	}
	set("qa")
	if l, err := orchclient.WorkList(h.ctx, g, orchapi.WorkQuery{Source: "bd"}); err != nil || strings.Join(ids(l), ",") != "bd:harbor-q1" {
		t.Fatalf("guest after area set --label qa: %v %v", ids(l), err)
	}
	if it, err := orchclient.GetWork(h.ctx, owner, "bd:harbor-q1"); err != nil || it.Area != "qa" || it.Owner != h.owner.Address {
		t.Fatalf("owner after: %+v %v", it, err)
	}
	if hr, err := orchclient.Holder(h.ctx, owner, "bd:harbor-q1"); err != nil || hr.Lease == nil || hr.Lease.Area != "qa" {
		t.Fatalf("who after area set: %+v %v", hr, err)
	}
	if ls, err := orchclient.Leases(h.ctx, g, orchapi.LeaseQuery{}); err != nil || len(ls) != 1 || ls[0].Key != "bd:harbor-q1" || ls[0].Area != "qa" {
		t.Fatalf("the qa guest sees the moved lease: %+v %v", ls, err)
	}
	if _, err := orchclient.Claim(h.ctx, owner, "bd:harbor-q1", orchapi.ClaimRequest{Renew: true}); err != nil {
		t.Fatalf("renew after area set: %v", err)
	}
	if r, err := orchclient.Release(h.ctx, owner, "bd:harbor-q1", orchapi.ReleaseRequest{}); err != nil || !r.Released {
		t.Fatalf("release after area set: %+v %v", r, err)
	}
	if ls, err := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{}); err != nil || len(ls) != 0 {
		t.Fatalf("leases after release: %+v %v", ls, err)
	}
	if n := len(h.events(orchapi.EventWorkLeaseExpired)); n != 0 {
		t.Fatalf("%d lease_expired events", n)
	}
	set()
	_, errH := orchclient.GetWork(h.ctx, g, "bd:harbor-q1")
	_, errM := orchclient.GetWork(h.ctx, g, "bd:harbor-nope")
	if !isCode(errH, api.CodeNotFound) || strings.ReplaceAll(errH.Error(), "bd:harbor-q1", "X") != strings.ReplaceAll(errM.Error(), "bd:harbor-nope", "X") {
		t.Fatalf("label cleared: hidden %v vs missing %v", errH, errM)
	}
}

// A lease row left in another area than its item's (the daemon stopped
// between a re-area and moving it, or the item moved in its tracker) is
// still the item's lease: who, show, renew and release find it, and it
// moves to the item's area.
func TestStrandedItemLease(t *testing.T) {
	h := newHarness(t)
	state := h.installFake()
	st, _ := fake.Load(state)
	st.Issues["harbor-q1"] = &fake.Issue{ID: "harbor-q1", Title: "QA one", Status: "open"}
	if err := st.Save(state); err != nil {
		t.Fatal(err)
	}
	trs := []api.Tracker{{Adapter: "fake", Prefix: "bd"}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	owner := h.as(h.owner, "")
	if _, err := orchclient.Claim(h.ctx, owner, "bd:harbor-q1", orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Store.Registry().ExecContext(h.ctx, `UPDATE leases SET area = 'gone' WHERE key = 'bd:harbor-q1'`); err != nil {
		t.Fatal(err)
	}
	if hr, err := orchclient.Holder(h.ctx, owner, "bd:harbor-q1"); err != nil || hr.Holder != h.owner.Address {
		t.Fatalf("who: %+v %v", hr, err)
	}
	if it, err := orchclient.GetWork(h.ctx, owner, "bd:harbor-q1"); err != nil || it.Owner != h.owner.Address {
		t.Fatalf("show: %+v %v", it, err)
	}
	if l, err := orchclient.WorkList(h.ctx, owner, orchapi.WorkQuery{Source: "bd"}); err != nil || len(l.Items) != 1 || l.Items[0].Owner != h.owner.Address {
		t.Fatalf("ls: %+v %v", l, err)
	}
	r, err := orchclient.Claim(h.ctx, owner, "bd:harbor-q1", orchapi.ClaimRequest{Renew: true})
	if err != nil || r.Lease.Area != "" {
		t.Fatalf("renew: %+v %v", r, err)
	}
	var n int
	if err := h.d.Store.Registry().QueryRowContext(h.ctx, `SELECT count(*) FROM leases WHERE key = 'bd:harbor-q1'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d lease rows after renew (%v)", n, err)
	}
	if _, err := h.d.Store.Registry().ExecContext(h.ctx, `UPDATE leases SET area = 'gone' WHERE key = 'bd:harbor-q1'`); err != nil {
		t.Fatal(err)
	}
	if r, err := orchclient.Release(h.ctx, owner, "bd:harbor-q1", orchapi.ReleaseRequest{}); err != nil || !r.Released {
		t.Fatalf("release: %+v %v", r, err)
	}
	if ls, err := orchclient.Leases(h.ctx, owner, orchapi.LeaseQuery{}); err != nil || len(ls) != 0 {
		t.Fatalf("leases after release: %+v %v", ls, err)
	}
}

func TestCleanWorkWrites(t *testing.T) {
	h := newHarness(t)
	c := h.as(h.owner, "harbor")
	it, err := orchclient.CreateWork(h.ctx, c, orchapi.CreateWorkRequest{Title: "title\u200b", Body: "body\u202e", Area: "solver"})
	if err != nil || it.Title != "title" || it.Body != "body" {
		t.Fatalf("create: %+v %v", it, err)
	}
	comment, err := orchclient.Comment(h.ctx, c, it.ID, orchapi.CommentRequest{Body: "hi\U000e0061 👩‍💻"})
	if err != nil || comment.Body != "hi 👩‍💻" {
		t.Fatalf("comment: %+v %v", comment, err)
	}
	if _, err := orchclient.Comment(h.ctx, c, it.ID, orchapi.CommentRequest{Body: "\u200b"}); !isCode(err, api.CodeBadRequest) {
		t.Fatalf("empty comment: %v", err)
	}
}

func TestReviewBlankTitlesAndLabels(t *testing.T) {
	h := newHarness(t)
	c := h.as(h.owner, "harbor")
	for _, title := range []string{"\ufe0f", "\u2800", "safe\rEVIL", "a\u2028b"} {
		if _, err := orchclient.CreateWork(h.ctx, c, orchapi.CreateWorkRequest{Title: title, Area: "solver"}); err == nil {
			t.Errorf("accepted %q", title)
		}
	}
	it, err := orchclient.CreateWork(h.ctx, c, orchapi.CreateWorkRequest{Title: "ok", Area: "solver", Labels: []string{"a\u200bb"}})
	if err != nil || len(it.Labels) != 1 || it.Labels[0] != "ab" {
		t.Fatalf("labels: %+v %v", it, err)
	}
}

func TestReviewSearchNormalizesLineSeparators(t *testing.T) {
	h := newHarness(t)
	c := h.as(h.owner, "harbor")
	_, err := orchclient.CreateWork(h.ctx, c, orchapi.CreateWorkRequest{Title: "item", Body: "first\u2028end", Area: "solver"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := orchclient.WorkList(h.ctx, c, orchapi.WorkQuery{Q: "\u2028end"})
	if err != nil || len(got.Items) != 1 {
		t.Fatalf("search %+v %v", got, err)
	}
}
