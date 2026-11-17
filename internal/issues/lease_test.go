package issues_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/issues"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/server"
)

// leaseFixture is a daemon whose lease ticker runs every second, with
// project harbor binding its native issues as work source "issue".
type leaseFixture struct {
	t     *testing.T
	dm    *server.Daemon
	owner *client.Client
}

func newLeaseFixture(t *testing.T, bind bool) *leaseFixture {
	t.Helper()
	ctx := context.Background()
	cfg, err := config.LoadFrom(shortHome(t))
	if err != nil {
		t.Fatal(err)
	}
	backupHomes.Store(cfg.Home, true) // fileWith reads its encrypted backups
	cfg.Human, cfg.Machine = "ivo", "mini"
	cfg.File.LeaseTickSeconds = 1
	dm, err := server.New(ctx, cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	// Start the modules as Serve does, so the lease ticker runs.
	mctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	for _, m := range dm.Modules() {
		if st, ok := m.(core.Starter); ok {
			if err := st.Start(mctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	owner := client.WithHandler(dm.SocketHandler())
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}); err != nil {
		t.Fatal(err)
	}
	owner.Project, owner.ProjectFrom = "harbor", "flag"
	if bind {
		if res, err := orchclient.AddTracker(ctx, owner, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", WaitSeconds: 10}); err != nil || !res.OK {
			t.Fatalf("add native tracker: %+v %v", res, err)
		}
	}
	return &leaseFixture{t: t, dm: dm, owner: owner}
}

// claim posts a native claim with ttl seconds (0 = the default).
func (f *leaseFixture) claim(id string, ttl int) (int, string) {
	body := "{}"
	if ttl > 0 {
		body = `{"ttl_seconds":` + itoa(ttl) + `}`
	}
	req := httptest.NewRequest("POST", "/v0/issues/"+id+"/claim", strings.NewReader(body))
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	req.Header.Set(api.HeaderProject, "harbor")
	rec := httptest.NewRecorder()
	f.dm.SocketHandler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for ; n > 0; n /= 10 {
		s = string(rune('0'+n%10)) + s
	}
	return s
}

func (f *leaseFixture) lease(id string) *orchapi.Lease {
	ls, err := orchclient.Leases(context.Background(), f.owner, orchapi.LeaseQuery{})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, l := range ls {
		if l.Key == "issue:"+id {
			return &l
		}
	}
	return nil
}

func (f *leaseFixture) issue(id string) orchapi.Issue {
	it, err := orchclient.Issue(context.Background(), f.owner, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return it.Issue
}

func (f *leaseFixture) create(title string) string {
	it, err := orchclient.CreateIssue(context.Background(), f.owner, orchapi.NewIssueRequest{Title: title})
	if err != nil {
		f.t.Fatal(err)
	}
	return it.ID
}

// A native claim in a project that binds its native issues is a lease:
// taken at once (even on an issue made a moment ago, before any sync),
// renewed by claiming again, and expired by the ticker, which releases the
// issue.
func TestNativeClaimIsALease(t *testing.T) {
	ctx := context.Background()
	f := newLeaseFixture(t, true)
	id := f.create("claim me straight away") // no mirror sync yet
	if code, body := f.claim(id, 60); code != 200 {
		t.Fatalf("claim: %d %s", code, body)
	}
	l := f.lease(id)
	if l == nil || l.Holder != "human:ivo" || l.Expires == nil || time.Until(*l.Expires) > 61*time.Second {
		t.Fatalf("lease after a native claim (60 s): %+v", l)
	}
	if it := f.issue(id); it.Status != orchapi.IssueInProgress || it.ClaimedBy != "human:ivo" {
		t.Fatalf("issue after claim: %s %q", it.Status, it.ClaimedBy)
	}
	// Claiming again renews; the bd door's claim does the same.
	if _, err := orchclient.ClaimIssue(ctx, f.owner, id); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if l2 := f.lease(id); l2 == nil || l2.Expires == nil || time.Until(*l2.Expires) < 50*time.Minute {
		t.Fatalf("renewed lease: %+v (was %+v)", l2, l)
	}
	// A short lease expires and the issue goes back to open, unassigned.
	short := f.create("short lease")
	if code, body := f.claim(short, 1); code != 200 {
		t.Fatalf("short claim: %d %s", code, body)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && f.issue(short).Status != orchapi.IssueOpen {
		time.Sleep(100 * time.Millisecond)
	}
	if it := f.issue(short); it.Status != orchapi.IssueOpen || it.ClaimedBy != "" || it.Assignee != "" || f.lease(short) != nil {
		t.Fatalf("after expiry: %s claimed_by %q assignee %q lease %+v", it.Status, it.ClaimedBy, it.Assignee, f.lease(short))
	}
}

// A native write that ends the claim drops the lease; the issue keeps what
// bd keeps (a closed issue its assignee).
func TestNativeWritesEndTheLease(t *testing.T) {
	ctx := context.Background()
	f := newLeaseFixture(t, true)
	closed := f.create("closed while claimed")
	if _, err := orchclient.ClaimIssue(ctx, f.owner, closed); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.CloseIssue(ctx, f.owner, closed, orchapi.CloseIssueRequest{Reason: "done"}); err != nil {
		t.Fatal(err)
	}
	if l := f.lease(closed); l != nil {
		t.Fatalf("lease outlived the close: %+v", l)
	}
	if it := f.issue(closed); it.Status != orchapi.IssueClosed || it.Assignee != "ivo" {
		t.Fatalf("closed issue: %s assignee %q", it.Status, it.Assignee)
	}
	reopened := f.create("set back to open")
	if _, err := orchclient.ClaimIssue(ctx, f.owner, reopened); err != nil {
		t.Fatal(err)
	}
	open := orchapi.IssueOpen
	if _, err := orchclient.UpdateIssue(ctx, f.owner, reopened, orchapi.UpdateIssueRequest{Status: &open}); err != nil {
		t.Fatal(err)
	}
	if l := f.lease(reopened); l != nil {
		t.Fatalf("lease outlived status open: %+v", l)
	}
	if it := f.issue(reopened); it.Status != orchapi.IssueOpen || it.Assignee != "ivo" {
		t.Fatalf("reopened issue: %s assignee %q (bd keeps the assignee)", it.Status, it.Assignee)
	}
	// A comment or an edit while claimed keeps the lease.
	kept := f.create("edited while claimed")
	if _, err := orchclient.ClaimIssue(ctx, f.owner, kept); err != nil {
		t.Fatal(err)
	}
	title := "edited while claimed (renamed)"
	if _, err := orchclient.UpdateIssue(ctx, f.owner, kept, orchapi.UpdateIssueRequest{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if f.lease(kept) == nil {
		t.Fatal("an edit dropped the lease")
	}
	// zm work release of the native item releases it natively too.
	if _, err := orchclient.Release(ctx, f.owner, "issue:"+kept, orchapi.ReleaseRequest{}); err != nil {
		t.Fatal(err)
	}
	if it := f.issue(kept); it.Status != orchapi.IssueOpen || it.ClaimedBy != "" || f.lease(kept) != nil {
		t.Fatalf("after zm work release: %s %q lease %+v", it.Status, it.ClaimedBy, f.lease(kept))
	}
}

// Without a native binding a claim stays native only: atomic, no lease.
func TestNativeClaimWithoutBinding(t *testing.T) {
	f := newLeaseFixture(t, false)
	id := f.create("unbound")
	if code, body := f.claim(id, 0); code != 200 {
		t.Fatalf("claim: %d %s", code, body)
	}
	if it := f.issue(id); it.Status != orchapi.IssueInProgress || f.lease(id) != nil {
		t.Fatalf("unbound claim: %s lease %+v", it.Status, f.lease(id))
	}
	// Binding later adopts it: the claim now holds a lease that expires.
	res, err := orchclient.AddTracker(context.Background(), f.owner, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", WaitSeconds: 10})
	if err != nil || !res.OK {
		t.Fatalf("bind: %+v %v", res, err)
	}
	l := f.lease(id)
	if l == nil || l.Holder != "human:ivo" || l.Note != "adopted" || !strings.Contains(strings.Join(res.Notes, "\n"), "now hold leases") {
		t.Fatalf("adopted lease %+v, notes %q", l, res.Notes)
	}
}

// zm work status on a natively claimed issue ends the claim and its lease,
// like a native write; in_review keeps it; a hand-set assignee drops it.
func TestWorkStatusAndAssigneeEndTheLease(t *testing.T) {
	ctx := context.Background()
	f := newLeaseFixture(t, true)
	done := f.create("done through zm work")
	if _, err := orchclient.ClaimIssue(ctx, f.owner, done); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.SetStatus(ctx, f.owner, "issue:"+done, orchapi.StatusRequest{Status: orchapi.WorkDone}); err != nil {
		t.Fatal(err)
	}
	if l := f.lease(done); l != nil || f.issue(done).Status != orchapi.IssueClosed {
		t.Fatalf("zm work status done: lease %+v status %s", l, f.issue(done).Status)
	}
	review := f.create("in review keeps the lease")
	if _, err := orchclient.ClaimIssue(ctx, f.owner, review); err != nil {
		t.Fatal(err)
	}
	st := orchapi.IssueInReview
	if _, err := orchclient.UpdateIssue(ctx, f.owner, review, orchapi.UpdateIssueRequest{Status: &st}); err != nil {
		t.Fatal(err)
	}
	if f.lease(review) == nil {
		t.Fatal("in_review dropped the lease")
	}
	who := "someone-else"
	if _, err := orchclient.UpdateIssue(ctx, f.owner, review, orchapi.UpdateIssueRequest{Assignee: &who}); err != nil {
		t.Fatal(err)
	}
	if l := f.lease(review); l != nil {
		t.Fatalf("a hand-set assignee kept the lease: %+v", l)
	}
}

// A team project's lease queues the tracker claim for the background
// writer. If that lands before the door's labelled claim, the claim must
// still hold, under the label: every agent claims with a label (BD_ACTOR).
func TestTeamLabelledClaimRacesTheWriteBack(t *testing.T) {
	f := newLeaseFixture(t, true)
	if _, err := f.owner.ProjectTeam(context.Background(), "harbor"); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := 0; i < 60; i++ {
		id := f.create("race")
		ids = append(ids, id)
		req := httptest.NewRequest("POST", "/v0/issues/"+id+"/claim", strings.NewReader(`{"assignee":"mini/agent1"}`))
		req.Header.Set(api.HeaderProtocol, api.Protocol)
		req.Header.Set(api.HeaderProject, "harbor")
		rec := httptest.NewRecorder()
		f.dm.SocketHandler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("claim %d refused: %d %s", i, rec.Code, rec.Body.String())
		}
		if got := f.issue(id); got.Assignee != "mini/agent1" {
			t.Fatalf("claim %d: assignee %q, want the label", i, got.Assignee)
		}
		if f.lease(id) == nil {
			t.Fatalf("claim %d: no lease", i)
		}
	}
	// A write-back that lands after the claim must not relabel it.
	time.Sleep(200 * time.Millisecond)
	for _, id := range ids {
		if got := f.issue(id); got.Assignee != "mini/agent1" {
			t.Fatalf("%s relabelled: %q", id, got.Assignee)
		}
	}
}

// A claimed issue no lease names (a hand-set in_progress, or a claim whose
// lease a hand-set assignee dropped) goes back to open once it has sat
// unchanged for the grace: nothing else would ever renew or release it, and
// the ready queue skips it. A leased claim, a fresh one, and a
// project that does not bind its native issues are left alone.
func TestUnleasedClaimGoesBackToOpen(t *testing.T) {
	ctx := context.Background()
	f := newLeaseFixture(t, true)
	svc := orchcore.Issues(f.dm.Deps).(*issues.Service)
	ip := orchapi.IssueInProgress
	handSet := f.create("set in progress by hand")
	if _, err := orchclient.UpdateIssue(ctx, f.owner, handSet, orchapi.UpdateIssueRequest{Status: &ip}); err != nil {
		t.Fatal(err)
	}
	handedOn := f.create("claimed, then handed to a reviewer")
	if _, err := orchclient.ClaimIssue(ctx, f.owner, handedOn); err != nil {
		t.Fatal(err)
	}
	reviewer := "reviewer/1"
	if _, err := orchclient.UpdateIssue(ctx, f.owner, handedOn, orchapi.UpdateIssueRequest{Assignee: &reviewer}); err != nil {
		t.Fatal(err)
	}
	held := f.create("claimed and held")
	if _, err := orchclient.ClaimIssue(ctx, f.owner, held); err != nil {
		t.Fatal(err)
	}
	if f.lease(handSet) != nil || f.lease(handedOn) != nil || f.lease(held) == nil {
		t.Fatalf("leases before the sweep: %+v %+v %+v", f.lease(handSet), f.lease(handedOn), f.lease(held))
	}
	svc.ReleaseUnleased(ctx, time.Hour) // all fresh: nothing moves
	for _, id := range []string{handSet, handedOn, held} {
		if st := f.issue(id).Status; st != ip {
			t.Fatalf("%s moved inside the grace: %s", id, st)
		}
	}
	time.Sleep(5 * time.Millisecond)
	svc.ReleaseUnleased(ctx, 0)
	if it := f.issue(handSet); it.Status != orchapi.IssueOpen || it.Assignee != "" {
		t.Fatalf("hand-set in_progress after the sweep: %s %q", it.Status, it.Assignee)
	}
	if it := f.issue(handedOn); it.Status != orchapi.IssueOpen || it.Assignee != reviewer || it.ClaimedBy != "" {
		t.Fatalf("handed-on claim after the sweep: %s assignee %q claimed_by %q", it.Status, it.Assignee, it.ClaimedBy)
	}
	if it := f.issue(held); it.Status != ip || f.lease(held) == nil {
		t.Fatalf("a leased claim was swept: %s lease %+v", it.Status, f.lease(held))
	}
	cs, err := orchclient.IssueComments(ctx, f.owner, handedOn)
	if err != nil || len(cs) != 1 || !strings.Contains(cs[0].Text, "back to open") || !strings.Contains(cs[0].Text, reviewer) {
		t.Fatalf("the sweep's comment: %+v %v", cs, err)
	}
	// Without the binding a claim has no lease by design: never swept.
	u := newLeaseFixture(t, false)
	id := u.create("unbound claim")
	if code, body := u.claim(id, 0); code != 200 {
		t.Fatalf("claim: %d %s", code, body)
	}
	time.Sleep(5 * time.Millisecond)
	orchcore.Issues(u.dm.Deps).(*issues.Service).ReleaseUnleased(ctx, 0)
	if st := u.issue(id).Status; st != ip {
		t.Fatalf("an unbound project's claim was swept: %s", st)
	}
}
