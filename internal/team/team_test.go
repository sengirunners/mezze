package team_test

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

func TestJoinAndProxiedWork(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")

	x := j.Remote
	if x.Project.Slug != "harbor" || x.Cap.Role != "member" || x.Identity != "bea" || x.Machine != "bea-mac" || x.Authority != "mini" {
		t.Fatalf("remote: %+v", x)
	}
	if len(x.Areas) != 2 || x.Role != "member" || x.ScopeVersion == 0 {
		t.Fatalf("header not cached: %+v", x)
	}
	fi, err := os.Stat(x.TokenFile)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file %s: %v %v", x.TokenFile, fi, err)
	}

	// The CLI names team projects by slug: the replica lists them too.
	ps := mustf(r.owner.Projects(ctx))(t)
	if len(ps) != 1 || ps[0].Slug != "harbor" || ps[0].Role != "member" || ps[0].ID != x.Project.ID {
		t.Fatalf("replica projects %+v", ps)
	}
	if p := mustf(r.owner.GetProject(ctx, "harbor"))(t); p.ID != x.Project.ID || len(p.Areas) != 2 {
		t.Fatalf("replica project %+v", p)
	}
	// A write naming the team project lands at the authority.
	sess := "bea-mac/abcd1234"
	rc := at(r.owner, "harbor", sess)
	it := mustf(orchclient.CreateWork(ctx, rc, orchapi.CreateWorkRequest{Title: "from bea", Area: "solver"}))(t)
	if it.CreatedBy != sess || it.Area != "solver" {
		t.Fatalf("created %+v", it)
	}
	if got := mustf(orchclient.GetWork(ctx, at(a.owner, "harbor", ""), it.ID))(t); got.Title != "from bea" {
		t.Fatalf("authority has %+v", got)
	}
	// A list naming nothing is this machine's answer merged with the
	// authority's; the item's id alone finds it there.
	wl := mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{}))(t)
	if !hasItem(wl.Items, it.ID) || len(wl.Skipped) != 0 {
		t.Fatalf("union list: %+v", wl)
	}
	// Claim from a session on the replica: the holder is that session.
	cl := mustf(orchclient.Claim(ctx, at(r.owner, "", sess), it.ID, orchapi.ClaimRequest{}))(t)
	if cl.Lease.Holder != sess || cl.Item.Status != orchapi.WorkClaimed {
		t.Fatalf("claim %+v", cl)
	}
	// The authority's owner sees the replica's session as holder.
	ls := mustf(orchclient.Leases(ctx, at(a.owner, "harbor", ""), orchapi.LeaseQuery{Project: "harbor"}))(t)
	if len(ls) != 1 || ls[0].Holder != sess {
		t.Fatalf("authority leases %+v", ls)
	}
	// Another machine's session cannot be claimed as (Zm-As limited to the
	// replica's machine): the replica forwards only its own sessions.
	if _, err := orchclient.Claim(ctx, at(r.owner, "", "mini/deadbeef"), it.ID, orchapi.ClaimRequest{}); err == nil {
		t.Fatal("claimed as another machine's session")
	}
	// release --force is the authority's.
	if _, err := orchclient.Release(ctx, at(r.owner, "harbor", ""), it.ID, orchapi.ReleaseRequest{Force: true}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("force from a replica: %v", err)
	}
	if _, err := orchclient.ReleaseLease(ctx, r.owner, it.ID, orchapi.ReleaseRequest{Force: true}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("lease force from a replica: %v", err)
	}
	rel := mustf(orchclient.Release(ctx, at(r.owner, "", sess), it.ID, orchapi.ReleaseRequest{}))(t)
	if !rel.Released {
		t.Fatalf("release %+v", rel)
	}
	// Remotes lists it; leave forgets it and revokes the token.
	rs := mustf(r.owner.Remotes(ctx))(t)
	if len(rs) != 1 || rs[0].State != api.RemoteOK {
		t.Fatalf("remotes %+v", rs)
	}
	lv := mustf(r.owner.Leave(ctx, "harbor"))(t)
	if !lv.Revoked || len(lv.Removed) != 1 {
		t.Fatalf("leave %+v", lv)
	}
	if _, err := os.Stat(x.TokenFile); !os.IsNotExist(err) {
		t.Fatalf("token file kept: %v", err)
	}
	if _, err := orchclient.GetWork(ctx, at(r.owner, "harbor", ""), it.ID); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("after leave: %v", err)
	}
	// solo is allowed once nobody is joined.
	if _, err := a.owner.ProjectSolo(ctx, "harbor"); err != nil {
		t.Fatalf("solo after leave: %v", err)
	}
}

func TestGuestReplicaSeesOnlyItsArea(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	hidden := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "HIDDEN billing", Area: "billing"}))(t)
	mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "HIDDEN project level"}))(t)
	vis := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "solver item", Area: "solver"}))(t)
	r, j := replica(t, tn, a, "vida-mac", "nVIDA", "harbor/solver", "vida", "guest")
	if j.Items != 1 || len(j.Remote.Areas) != 1 || j.Remote.Areas[0] != "solver" || j.Remote.Role != "guest" {
		t.Fatalf("guest join %+v / %+v", j, j.Remote)
	}
	wl := mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{}))(t)
	if len(wl.Items) != 1 || wl.Items[0].ID != vis.ID {
		t.Fatalf("guest list %+v", wl.Items)
	}
	_, hid := orchclient.GetWork(ctx, r.owner, hidden.ID)
	_, mis := orchclient.GetWork(ctx, r.owner, "local:zzzzzz")
	if !api.IsCode(hid, api.CodeNotFound) || !api.IsCode(mis, api.CodeNotFound) ||
		strings.Replace(hid.Error(), hidden.ID, "X", 1) != strings.Replace(mis.Error(), "local:zzzzzz", "X", 1) {
		t.Fatalf("hidden %v missing %v", hid, mis)
	}
	// A guest writes in its area by default.
	it := mustf(orchclient.CreateWork(ctx, at(r.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "guest item"}))(t)
	if it.Area != "solver" {
		t.Fatalf("guest default area: %+v", it)
	}
}

// The node-bound token works only from its node's daemon: never on a Unix
// socket, never from another node, never on a listener without WhoIs.
func TestNodeBoundToken(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	b, err := os.ReadFile(j.Remote.TokenFile)
	if err != nil {
		t.Fatal(err)
	}
	tok := strings.TrimSpace(string(b))
	// On the authority's socket.
	c := client.WithHandler(a.dm.SocketHandler())
	c.Token = tok
	if _, err := c.Projects(ctx); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("socket: %v", err)
	}
	// On the replica's own socket (the agents' door).
	rc := client.NewUnix(r.cfg.SocketPath())
	rc.Token = tok
	if _, err := rc.Projects(ctx); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("replica socket: %v", err)
	}
	// From another node on the tailnet (the host's own Tailscale, a farm box).
	other := startDaemon(t, tn, "other", tailnettest.Identity{NodeID: "nOTHER", NodeKey: "nodekey:other", Login: "bea@example.com"})
	_ = other
	hc := tailnetClient(t, tn, "nOTHER2")
	oc := client.NewRemote("http://potluck-mini:7433", hc)
	oc.Token = tok
	if _, err := oc.Projects(ctx); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("other node: %v", err)
	}
	// An unmarked network caller.
	nc := client.WithHandler(a.dm.Handler)
	nc.Token = tok
	if _, err := nc.Projects(ctx); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("unmarked: %v", err)
	}
	// Its own node: fine.
	mine := client.NewRemote("http://potluck-mini:7433", tailnetClientAs(t, tn, "bea-mac"))
	mine.Token = tok
	ps := mustf(mine.Projects(ctx))(t)
	if len(ps) != 1 || ps[0].Slug != "harbor" {
		t.Fatalf("own node: %+v", ps)
	}
}

func hasItem(items []orchapi.WorkItem, id string) bool {
	for _, it := range items {
		if it.ID == id {
			return true
		}
	}
	return false
}

// tailnetClient is an HTTP client from a fresh node on tn (not a daemon).
func tailnetClient(t *testing.T, tn *tailnettest.Net, nodeID string) *http.Client {
	t.Helper()
	return tailnetNode(t, tn, tailnettest.Identity{NodeID: nodeID, NodeKey: "nodekey:" + nodeID, Login: "x@example.com"}, "zm-"+strings.ToLower(nodeID)).HTTPClient()
}

// A write naming no project goes to the joined project whose recorded
// checkout (or .zm/project.toml) contains the cwd; elsewhere it is this
// machine's, as before.
func TestWritesFromACheckout(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r := startDaemon(t, tn, "bea-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "kc", Login: "bea@example.com", Host: "bea-mac"})
	checkout, pinned := t.TempDir(), t.TempDir()
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	j := mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code, Paths: []string{checkout}}))(t)
	if len(j.Remote.Paths) != 1 {
		t.Fatalf("paths %+v", j.Remote)
	}
	in := func(dir string) *client.Client { c := *r.sock; c.Cwd = dir; return &c } // Zm-Cwd goes on the socket only
	if err := os.MkdirAll(checkout+"/sub", 0o700); err != nil {
		t.Fatal(err)
	}
	it := mustf(orchclient.CreateWork(ctx, in(checkout+"/sub"), orchapi.CreateWorkRequest{Title: "from the checkout"}))(t)
	if it.Project != j.Remote.Project.ID {
		t.Fatalf("checkout write went to %s", it.Project)
	}
	if err := os.MkdirAll(pinned+"/.zm", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pinned+"/.zm/project.toml", []byte("project = \"harbor\"\narea = \"solver\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	it = mustf(orchclient.CreateWork(ctx, in(pinned), orchapi.CreateWorkRequest{Title: "from a pinned repo"}))(t)
	if it.Project != j.Remote.Project.ID || it.Area != "solver" {
		t.Fatalf("pinned write %+v", it)
	}
	// Elsewhere: no project (this machine has none), refused as before.
	if _, err := orchclient.CreateWork(ctx, in(t.TempDir()), orchapi.CreateWorkRequest{Title: "nowhere"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("write from elsewhere: %v", err)
	}
}
