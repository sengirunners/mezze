package issues_test

import (
	"context"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/issues"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/server"
)

func TestMoveIssuesBetweenProjects(t *testing.T) {
	ctx := context.Background()
	cfg, err := config.LoadFrom(shortHome(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	dm, err := server.New(ctx, cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	owner := client.WithHandler(dm.SocketHandler())
	at := func(p string) *client.Client {
		c := client.WithHandler(dm.SocketHandler())
		c.Project, c.ProjectFrom = p, "flag"
		return c
	}
	for _, p := range []string{"harbor", "sengi"} {
		if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: p}); err != nil {
			t.Fatal(err)
		}
	}
	oc, sg := at("harbor"), at("sengi")
	if _, err := orchclient.SetupIssues(ctx, oc, "harbor"); err != nil {
		t.Fatal(err)
	}
	mk := func(req orchapi.NewIssueRequest) orchapi.Issue {
		t.Helper()
		it, err := orchclient.CreateIssue(ctx, oc, req)
		if err != nil {
			t.Fatal(err)
		}
		return it
	}
	stays := mk(orchapi.NewIssueRequest{Title: "harbor work that stays"})
	epic := mk(orchapi.NewIssueRequest{Title: "THEME: Sengi", IssueType: "epic"})
	claimed := mk(orchapi.NewIssueRequest{Title: "Sengi: claimed", Parent: epic.ID})
	if _, err := orchclient.ClaimIssue(ctx, oc, claimed.ID); err != nil {
		t.Fatal(err)
	}
	child := mk(orchapi.NewIssueRequest{Title: "Sengi: multi-host", Parent: epic.ID, Deps: []string{stays.ID, claimed.ID}})
	waits := mk(orchapi.NewIssueRequest{Title: "harbor waits on multi-host", Deps: []string{child.ID}})
	if _, err := orchclient.CommentIssue(ctx, oc, child.ID, orchapi.IssueCommentRequest{Text: "host two is up"}); err != nil {
		t.Fatal(err)
	}

	req := orchapi.IssueMoveRequest{To: "sengi", IDs: []string{epic.ID}, Children: true, DryRun: true}
	if _, err := orchclient.MoveIssues(ctx, oc, req); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a target with no prefix: %v", err)
	}
	if _, err := orchclient.SetupIssues(ctx, sg, "sengi"); err != nil {
		t.Fatal(err)
	}
	newID := func(id string) string { return "sengi-" + strings.TrimPrefix(id, "harbor-") }
	plan, err := orchclient.MoveIssues(ctx, oc, req)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]orchapi.IssueMoveItem{}
	for _, it := range plan.Items {
		got[it.ID] = it
	}
	if len(plan.Items) != 3 || plan.Refused != 1 || got[epic.ID].Result != "move" || got[epic.ID].NewID != newID(epic.ID) ||
		got[claimed.ID].Result != "refused" || !strings.Contains(got[claimed.ID].Note, "--transfer-claims") ||
		strings.Join(got[child.ID].Cross, " ") != sorted("blocks:"+stays.ID, "blocks:"+claimed.ID, waits.ID+":blocks") {
		t.Fatalf("plan %+v", plan)
	}
	if _, err := orchclient.Issue(ctx, oc, epic.ID); err != nil {
		t.Fatalf("a dry run moved something: %v", err)
	}

	req.DryRun, req.TransferClaims = false, true
	res, err := orchclient.MoveIssues(ctx, oc, req)
	if err != nil || res.Moved != 3 || res.Refused != 0 {
		t.Fatalf("move %+v %v", res, err)
	}
	for _, it := range res.Items {
		// The claimed sibling moved too, so that edge stays inside sengi.
		if it.ID == child.ID && strings.Join(it.Cross, " ") != sorted("blocks:"+stays.ID, waits.ID+":blocks") {
			t.Fatalf("the run's cross-project edges %v", it.Cross)
		}
	}
	// The old id finds the new issue, with its comment and both edges.
	d, err := orchclient.Issue(ctx, oc, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != newID(child.ID) || d.MovedFrom != child.ID || d.Parent != newID(epic.ID) || len(d.Comments) != 1 {
		t.Fatalf("old id answers %+v", d)
	}
	found := false
	for _, ln := range d.DependsOn {
		if ln.ID == stays.ID && ln.Title == stays.Title && ln.DependencyType == "blocks" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the cross-project edge to %s: %+v", stays.ID, d.DependsOn)
	}
	w, err := orchclient.Issue(ctx, oc, waits.ID)
	if err != nil || len(w.DependsOn) != 1 || w.DependsOn[0].ID != newID(child.ID) || w.DependsOn[0].Title != child.Title {
		t.Fatalf("the edge from harbor: %+v %v", w.DependsOn, err)
	}
	// Writes through the old id land on the new issue.
	if _, err := orchclient.CommentIssue(ctx, oc, child.ID, orchapi.IssueCommentRequest{Text: "via the old id"}); err != nil {
		t.Fatal(err)
	}
	if cs, err := orchclient.IssueComments(ctx, sg, newID(child.ID)); err != nil || len(cs) != 2 {
		t.Fatalf("comments on the new issue %+v %v", cs, err)
	}
	c, err := orchclient.Issue(ctx, sg, newID(claimed.ID))
	if err != nil || c.Status != orchapi.IssueInProgress || c.ClaimedBy == "" {
		t.Fatalf("the claim moved with it: %+v %v", c.Issue, err)
	}
	// Again: nothing left to do.
	again, err := orchclient.MoveIssues(ctx, oc, orchapi.IssueMoveRequest{To: "sengi", IDs: []string{epic.ID, child.ID}})
	if err != nil || again.Moved != 0 || again.Items[0].Result != "already moved" || again.Items[1].Result != "already moved" {
		t.Fatalf("second run %+v %v", again, err)
	}

	// A member of harbor alone cannot follow the redirect into sengi.
	tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: "mina", Project: "harbor", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	get := func(path string) int {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set(api.HeaderProtocol, api.Protocol)
		r.Header.Set(api.HeaderAuthorization, "Bearer "+tok.Secret)
		r = r.WithContext(core.WithRemote(r.Context(), core.Remote{Login: "mina@example.com"}))
		rec := httptest.NewRecorder()
		dm.Handler.ServeHTTP(rec, r)
		return rec.Code
	}
	if code := get("/v0/issues/" + child.ID); code != 404 {
		t.Fatalf("mina reads a moved issue in a project she cannot see: %d", code)
	}
	if code := get("/v0/issues/" + stays.ID); code != 200 {
		t.Fatalf("mina reads harbor: %d", code)
	}
	if _, err := orchclient.MoveIssues(ctx, at("harbor"), orchapi.IssueMoveRequest{To: "nope", IDs: []string{stays.ID}}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("an unknown target: %v", err)
	}
}

func sorted(ss ...string) string {
	sort.Strings(ss)
	return strings.Join(ss, " ")
}

// A live lease moves with --transfer-claims: the old one ends and the
// holder holds the new id's lease in a target that binds native issues. A
// claim taken after the dry run is refused by the run.
func TestMoveTransfersALease(t *testing.T) {
	ctx := context.Background()
	f := newLeaseFixture(t, true)
	if _, err := f.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "sengi"}); err != nil {
		t.Fatal(err)
	}
	sg := client.WithHandler(f.dm.SocketHandler())
	sg.Project, sg.ProjectFrom = "sengi", "flag"
	if _, err := orchclient.SetupIssues(ctx, sg, "sengi"); err != nil {
		t.Fatal(err)
	}
	if res, err := orchclient.AddTracker(ctx, sg, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "sgi", WaitSeconds: 10}); err != nil || !res.OK {
		t.Fatalf("bind sengi: %+v %v", res, err)
	}
	held := f.create("Sengi: held")
	later := f.create("Sengi: claimed after the plan")
	if code, body := f.claim(held, 600); code != 200 {
		t.Fatalf("claim: %d %s", code, body)
	}
	if l := f.lease(held); l == nil {
		t.Fatal("no lease before the move")
	}
	plan, err := orchclient.MoveIssues(ctx, f.owner, orchapi.IssueMoveRequest{To: "sengi", IDs: []string{later}, DryRun: true})
	if err != nil || plan.Items[0].Result != "move" {
		t.Fatalf("plan %+v %v", plan, err)
	}
	if code, body := f.claim(later, 600); code != 200 {
		t.Fatalf("claim: %d %s", code, body)
	}
	res, err := orchclient.MoveIssues(ctx, f.owner, orchapi.IssueMoveRequest{To: "sengi", IDs: []string{later}})
	if err != nil || res.Moved != 0 || res.Items[0].Result != "refused" || !strings.Contains(res.Items[0].Note, "claimed by") {
		t.Fatalf("a claim taken after the plan: %+v %v", res, err)
	}
	res, err = orchclient.MoveIssues(ctx, f.owner, orchapi.IssueMoveRequest{To: "sengi", IDs: []string{held}, TransferClaims: true})
	if err != nil || res.Moved != 1 || res.Items[0].Note != "" {
		t.Fatalf("move %+v %v", res, err)
	}
	nid := res.Items[0].NewID
	if l := f.lease(held); l != nil {
		t.Fatalf("the old lease is still live: %+v", l)
	}
	ls, err := orchclient.Leases(ctx, sg, orchapi.LeaseQuery{Project: "sengi"})
	if err != nil {
		t.Fatal(err)
	}
	var got *orchapi.Lease
	for _, l := range ls {
		if l.Key == "sgi:"+nid {
			got = &l
		}
	}
	if got == nil || got.Holder != "human:ivo" {
		t.Fatalf("the lease on %s: %+v (all: %+v)", nid, got, ls)
	}
	it, err := orchclient.Issue(ctx, sg, nid)
	if err != nil || it.Status != orchapi.IssueInProgress || it.ClaimedBy != "human:ivo" {
		t.Fatalf("the moved issue %+v %v", it.Issue, err)
	}
}

// moveFixture is a daemon with projects harbor (prefix harbor) and sengi
// (prefix sengi), and its issues module.
func moveFixture(t *testing.T) (*server.Daemon, *issues.Service, *client.Client, *client.Client) {
	t.Helper()
	ctx := context.Background()
	cfg, err := config.LoadFrom(shortHome(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	dm, err := server.New(ctx, cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	var svc *issues.Service
	for _, m := range dm.Modules() {
		if s, ok := m.(*issues.Service); ok {
			svc = s
		}
	}
	at := func(p string) *client.Client {
		c := client.WithHandler(dm.SocketHandler())
		c.Project, c.ProjectFrom = p, "flag"
		return c
	}
	owner := client.WithHandler(dm.SocketHandler())
	for _, p := range []string{"harbor", "sengi"} {
		if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: p}); err != nil {
			t.Fatal(err)
		}
		if _, err := orchclient.SetupIssues(ctx, at(p), p); err != nil {
			t.Fatal(err)
		}
	}
	if svc == nil {
		t.Fatal("no issues module")
	}
	return dm, svc, at("harbor"), at("sengi")
}

// A panic part way through a move leaves no hold behind: both projects
// take writes again at once.
func TestMovePanicReleasesTheHolds(t *testing.T) {
	ctx := context.Background()
	_, _, oc, sg := moveFixture(t)
	a, err := orchclient.CreateIssue(ctx, oc, orchapi.NewIssueRequest{Title: "A"})
	if err != nil {
		t.Fatal(err)
	}
	defer issues.SetMoveHook(func(string) { panic("injected") })()
	func() {
		defer func() { _ = recover() }()
		_, _ = orchclient.MoveIssues(ctx, oc, orchapi.IssueMoveRequest{To: "sengi", IDs: []string{a.ID}})
	}()
	if _, err := orchclient.CommentIssue(ctx, oc, a.ID, orchapi.IssueCommentRequest{Text: "harbor still writes"}); err != nil {
		t.Fatalf("harbor after the panic: %v", err)
	}
	if _, err := orchclient.CreateIssue(ctx, sg, orchapi.NewIssueRequest{Title: "sengi still writes"}); err != nil {
		t.Fatalf("sengi after the panic: %v", err)
	}
}

// A hold waits for a write already past its held check, so the move's plan
// sees it; a write after the hold is refused.
func TestHoldWritesIsABarrier(t *testing.T) {
	ctx := context.Background()
	_, svc, oc, _ := moveFixture(t)
	a, err := orchclient.CreateIssue(ctx, oc, orchapi.NewIssueRequest{Title: "A"})
	if err != nil {
		t.Fatal(err)
	}
	inside, release := make(chan struct{}), make(chan struct{})
	undo := issues.SetWriteHook(func(string) {
		close(inside)
		<-release
	})
	wrote := make(chan error, 1)
	go func() {
		_, err := orchclient.CommentIssue(ctx, oc, a.ID, orchapi.IssueCommentRequest{Text: "in flight"})
		wrote <- err
	}()
	<-inside
	undo()
	project := a.Project
	held := make(chan func(), 1)
	go func() {
		rel, err := svc.HoldWrites(project, "a test hold")
		if err != nil {
			t.Error(err)
		}
		held <- rel
	}()
	select {
	case <-held:
		t.Fatal("the hold returned while a write was between its check and its commit")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if err := <-wrote; err != nil {
		t.Fatalf("the in-flight write: %v", err)
	}
	rel := <-held
	defer rel()
	if cs, err := orchclient.IssueComments(ctx, oc, a.ID); err != nil || len(cs) != 1 {
		t.Fatalf("the in-flight write committed before the hold: %+v %v", cs, err)
	}
	if _, err := orchclient.CommentIssue(ctx, oc, a.ID, orchapi.IssueCommentRequest{Text: "after the hold"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a write after the hold: %v", err)
	}
}
