package issues_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
)

// A bare native id must take the same canonical bound lease as the explicit
// work-source form, including immediately after creation, before mirror sync.
func TestBareNativeWorkIDBoundClaim(t *testing.T) {
	ctx := context.Background()
	f := newLeaseFixture(t, true)
	id := f.create("native work claim")
	session, err := orchclient.Register(ctx, f.owner, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "abcdef01-native-work", State: orchapi.StateRunning})
	if err != nil {
		t.Fatal(err)
	}
	c := *f.owner
	c.As = session.Address
	got, err := orchclient.GetWork(ctx, &c, id)
	if err != nil || got.ID != "issue:"+id {
		t.Fatalf("bare get: %+v %v", got, err)
	}
	claim, err := orchclient.Claim(ctx, &c, id, orchapi.ClaimRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if claim.Lease.Key != "issue:"+id || claim.Lease.Holder != session.Address || !claim.Lease.Bound || claim.Lease.Expires == nil {
		t.Fatalf("bare bound claim: %+v", claim.Lease)
	}
	if _, err := orchclient.Claim(ctx, &c, "issue:"+id, orchapi.ClaimRequest{}); err != nil {
		t.Fatalf("explicit reclaim: %v", err)
	}
	leases, err := orchclient.Leases(ctx, &c, orchapi.LeaseQuery{})
	if err != nil || len(leases) != 1 {
		t.Fatalf("aliases made duplicate leases: %+v %v", leases, err)
	}
	if _, err := orchclient.Comment(ctx, &c, id, orchapi.CommentRequest{Body: "claim evidence"}); err != nil {
		t.Fatal(err)
	}
	comments, err := orchclient.Comments(ctx, &c, "issue:"+id)
	if err != nil || len(comments) != 1 || comments[0].Body != "claim evidence" {
		t.Fatalf("canonical comments: %+v %v", comments, err)
	}
	if _, err := orchclient.Release(ctx, &c, id, orchapi.ReleaseRequest{}); err != nil {
		t.Fatal(err)
	}
	if f.lease(id) != nil || f.issue(id).Status != orchapi.IssueOpen {
		t.Fatal("bare release left claim behind")
	}

	// BindLease is the same core entry point used by spawn --work. It must
	// resolve the alias inside the explicitly selected project.
	bound, err := orchcore.Work(f.dm.Deps).BindLease(ctx, f.dm.Deps.System(), "harbor", id, session.Address)
	if err != nil || bound.Key != "issue:"+id || !bound.Bound || bound.Holder != session.Address {
		t.Fatalf("spawn binding: %+v %v", bound, err)
	}
	if _, err := f.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchcore.Work(f.dm.Deps).BindLease(ctx, f.dm.Deps.System(), "other", id, session.Address); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("cross-project binding: %v", err)
	}
}

func TestBareNativeWorkIDKeepsAreaScope(t *testing.T) {
	ctx := context.Background()
	f := newAuthzFixture(t)
	c := f.ownerAt("harbor")
	if res, err := orchclient.AddTracker(ctx, c, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", WaitSeconds: 10}); err != nil || !res.OK {
		t.Fatalf("bind: %+v %v", res, err)
	}
	visible, err := orchclient.CreateIssue(ctx, f.ownerAt("harbor/solver"), orchapi.NewIssueRequest{Title: "visible"})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := orchclient.CreateIssue(ctx, c, orchapi.NewIssueRequest{Title: "SECRET", Labels: []string{"billing"}})
	if err != nil {
		t.Fatal(err)
	}
	code, body := f.as("member", "GET", "/v0/work/"+visible.ID, "")
	if code != 200 || !strings.Contains(body, "issue:"+visible.ID) {
		t.Fatalf("visible bare get: %d %s", code, body)
	}
	for _, rt := range []struct{ method, suffix, body string }{{"GET", "", ""}, {"POST", "/claim", "{}"}} {
		hc, hb := f.as("member", rt.method, "/v0/work/"+hidden.ID+rt.suffix, rt.body)
		mc, mb := f.as("member", rt.method, "/v0/work/harbor-missing"+rt.suffix, rt.body)
		if hc != 404 || hc != mc || strings.ReplaceAll(hb, hidden.ID, "X") != strings.ReplaceAll(mb, "harbor-missing", "X") {
			t.Fatalf("%s hidden %d %s; missing %d %s", rt.method, hc, hb, mc, mb)
		}
	}
}

func TestBareWorkIDRequiresNativeSource(t *testing.T) {
	f := newLeaseFixture(t, false)
	id := f.create("unbound issue")
	if _, err := orchclient.GetWork(context.Background(), f.owner, id); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("unbound issue became work: %v", err)
	}
}
