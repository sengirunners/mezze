package team_test

// Teams over the tunnel transport (SPEC 17.11): the same join, sync,
// claims, hedges and outbox as over the tailnet, with a loopback proxy
// (tunneltest) standing in for Cloudflare in front of the home's tunnel
// listener.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/tunnel"
	"github.com/sengirunners/potluck/kit/tunnel/tunneltest"
)

func tunnelOf(t *testing.T, d *daemon) *tunnel.Service {
	t.Helper()
	for _, m := range d.dm.Modules() {
		if s, ok := m.(*tunnel.Service); ok {
			return s
		}
	}
	t.Fatal("no tunnel module")
	return nil
}

// tunnelHome starts machine with its tunnel listener on and a proxy in
// front (the stand-in for Cloudflare); tunnel.url names the proxy. tn nil:
// the machine has no tailnet at all.
func tunnelHome(t *testing.T, tn *tailnettest.Net, machine string, id tailnettest.Identity) (*daemon, *tunneltest.Proxy) {
	t.Helper()
	return tunnelHomeWith(t, tn, machine, id, nil)
}

// tunnelHomeWith is tunnelHome with mut applied to the config too.
func tunnelHomeWith(t *testing.T, tn *tailnettest.Net, machine string, id tailnettest.Identity, mut func(*config.Config)) (*daemon, *tunneltest.Proxy) {
	t.Helper()
	d := startDaemonWith(t, tn, machine, id, slog.New(slog.NewTextHandler(io.Discard, nil)), func(c *config.Config) {
		c.File.Tunnel.Listen = "127.0.0.1:0"
		if mut != nil {
			mut(c)
		}
	})
	var st api.TunnelStatus
	eventually(t, 5*time.Second, func() error {
		if st = tunnelOf(t, d).Status(); !st.Running {
			return fmt.Errorf("tunnel listener not up: %+v", st)
		}
		return nil
	})
	p, err := tunneltest.New(st.Listen)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	mustf(d.owner.TunnelUp(context.Background(), api.TunnelUpRequest{Listen: st.Listen, URL: p.URL()}))(t)
	return d, p
}

// teamHarbor makes harbor (areas solver, billing) a team project on a.
func teamHarbor(t *testing.T, a *daemon) {
	t.Helper()
	ctx := context.Background()
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor", RepoRules: api.RepoRules{Remotes: []string{"github.com/example-org/harbor"}}}))(t)
	for _, ar := range []string{"solver", "billing"} {
		mustf(a.owner.AddArea(ctx, "harbor", api.Area{Name: ar, Work: []string{"local-" + ar + "-*"}}))(t)
	}
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
}

// tunnelMember starts machine with no tailnet and joins a's proj over the
// tunnel.
func tunnelMember(t *testing.T, a *daemon, machine, proj, as, role string) (*daemon, api.JoinResponse) {
	t.Helper()
	ctx := context.Background()
	r := startDaemon(t, nil, machine, tailnettest.Identity{})
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: proj, As: as, Role: role, Via: api.ViaTunnel}))(t)
	j := mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	if j.Remote == nil {
		t.Fatalf("join: %+v", j)
	}
	return r, j
}

// signedClient is a client that signs with d's key for the home fp.
func signedClient(t *testing.T, d *daemon, fp string) *http.Client {
	return &http.Client{Transport: tunnelOf(t, d).Transport(fp, time.Second, nil)}
}

func rawCall(t *testing.T, c *http.Client, method, url, token string, body any, hdr map[string]string) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, url, bytes.NewReader(b))
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set(api.HeaderAuthorization, "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// A member with no tailnet joins over the tunnel, syncs a copy, claims
// and releases; the token works only signed by its own key.
func TestTunnelJoinSyncClaimRelease(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	a, p := tunnelHome(t, nil, "mini", tailnettest.Identity{})
	teamHarbor(t, a)
	ao := at(a.owner, "harbor", "")
	it := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "over the tunnel", Area: "solver"}))(t)
	fp, _ := tunnelOf(t, a).Fingerprint()

	// No tailnet node runs here: the invite is a tunnel code by default.
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	if !strings.HasPrefix(inv.Code, p.URL()+"#"+fp+":") {
		t.Fatalf("code %q", inv.Code)
	}
	r := startDaemon(t, nil, "bea-mac", tailnettest.Identity{})
	j := mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	x := j.Remote
	if x == nil || x.Via != api.ViaTunnel || x.URL != p.URL() || x.Fingerprint != fp || x.Identity != "bea" || x.Machine != "bea-mac" {
		t.Fatalf("remote %+v", x)
	}
	if rs := mustf(r.owner.Remotes(ctx))(t); len(rs) != 1 || rs[0].Via != api.ViaTunnel {
		t.Fatalf("remotes %+v", rs)
	}
	// The copy syncs: the item, then a new one.
	it2 := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "second", Area: "billing"}))(t)
	eventually(t, 5*time.Second, func() error {
		wl := mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{}))(t)
		if !hasItem(wl.Items, it.ID) || !hasItem(wl.Items, it2.ID) {
			return fmt.Errorf("copy: %+v", wl.Items)
		}
		return nil
	})
	// Claim and release from a session on the member.
	cs := register(t, r, "harbor/solver", "tunnel-sess-1")
	rs := at(r.owner, "", cs.Address)
	if cl := mustf(orchclient.Claim(ctx, rs, it.ID, orchapi.ClaimRequest{}))(t); cl.Lease.Holder != cs.Address {
		t.Fatalf("claim %+v", cl)
	}
	if ls := mustf(orchclient.Leases(ctx, ao, orchapi.LeaseQuery{Project: "harbor"}))(t); len(ls) != 1 || ls[0].Holder != cs.Address {
		t.Fatalf("authority leases %+v", ls)
	}
	if _, err := orchclient.Claim(ctx, at(r.owner, "", "mini/deadbeef"), it2.ID, orchapi.ClaimRequest{}); err == nil {
		t.Fatal("claimed as the home's session")
	}
	mustf(orchclient.Release(ctx, rs, it.ID, orchapi.ReleaseRequest{}))(t)
	if ls := mustf(orchclient.Leases(ctx, ao, orchapi.LeaseQuery{Project: "harbor"}))(t); len(ls) != 0 {
		t.Fatalf("leases after release %+v", ls)
	}
	// Presence crossed the tunnel too.
	eventually(t, 5*time.Second, func() error {
		heartbeat(t, r)
		if findSession(mustf(orchclient.Sessions(ctx, ao, orchapi.SessionQuery{}))(t), cs.Address) == nil {
			return fmt.Errorf("no presence for %s", cs.Address)
		}
		return nil
	})

	// The token alone is nothing: unsigned, signed by another daemon's key,
	// or a minted token signed by any key.
	tok := readToken(t, x.TokenFile)
	if code, body := rawCall(t, http.DefaultClient, "GET", p.URL()+"/v0/projects", tok, nil, nil); code != 401 {
		t.Fatalf("unsigned with the token: %d %s", code, body)
	}
	other := startDaemon(t, nil, "mallory", tailnettest.Identity{})
	if code, body := rawCall(t, signedClient(t, other, fp), "GET", p.URL()+"/v0/projects", tok, nil, nil); code != 401 {
		t.Fatalf("another key with the token: %d %s", code, body)
	}
	minted := mustf(a.owner.MintToken(ctx, api.MintTokenRequest{As: "bea", Project: "harbor", Role: "member"}))(t)
	if code, body := rawCall(t, signedClient(t, other, fp), "GET", p.URL()+"/v0/projects", minted.Secret, nil, nil); code != 401 {
		t.Fatalf("a minted token over the tunnel: %d %s", code, body)
	}
	// Signed by the member's own key it works (the replica's own path).
	if code, body := rawCall(t, signedClient(t, r, fp), "GET", p.URL()+"/v0/projects/harbor", tok, nil, nil); code != 200 {
		t.Fatalf("the member's key: %d %s", code, body)
	}
	// Leave revokes it.
	if lv := mustf(r.owner.Leave(ctx, "harbor"))(t); !lv.Revoked {
		t.Fatalf("leave %+v", lv)
	}
	if code, _ := rawCall(t, signedClient(t, r, fp), "GET", p.URL()+"/v0/projects/harbor", tok, nil, nil); code != 401 {
		t.Fatalf("token after leave: %d", code)
	}
}

// One home serves both transports: a tailnet member and a tunnel member
// race for the same item; exactly one wins, and each remote names its
// transport.
func TestTunnelAndTailnetTogether(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a, _ := tunnelHome(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"})
	teamHarbor(t, a)
	ao := at(a.owner, "harbor", "")
	it := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "contested", Area: "solver"}))(t)
	tr, tj := replica(t, tn, a, "vida-mac", "nVIDA", "harbor", "vida", "member")
	ur, uj := tunnelMember(t, a, "bea-mac", "harbor", "bea", "member")
	if tj.Remote.Via != api.ViaTailnet || uj.Remote.Via != api.ViaTunnel {
		t.Fatalf("vias %q %q", tj.Remote.Via, uj.Remote.Via)
	}
	ts := register(t, tr, "harbor/solver", "v-1")
	us := register(t, ur, "harbor/solver", "c-1")
	_, e1 := orchclient.Claim(ctx, at(tr.owner, "", ts.Address), it.ID, orchapi.ClaimRequest{})
	_, e2 := orchclient.Claim(ctx, at(ur.owner, "", us.Address), it.ID, orchapi.ClaimRequest{})
	if (e1 == nil) == (e2 == nil) {
		t.Fatalf("both or neither won: %v / %v", e1, e2)
	}
	ls := mustf(orchclient.Leases(ctx, ao, orchapi.LeaseQuery{Project: "harbor"}))(t)
	if len(ls) != 1 || (ls[0].Holder != ts.Address && ls[0].Holder != us.Address) {
		t.Fatalf("leases %+v", ls)
	}
	// A tailnet token is useless over the tunnel and the other way round.
	fp, _ := tunnelOf(t, a).Fingerprint()
	if code, _ := rawCall(t, signedClient(t, ur, fp), "GET", a.cfg.File.Tunnel.URL+"/v0/projects/harbor", readToken(t, tj.Remote.TokenFile), nil, nil); code != 401 {
		t.Fatalf("tailnet token over the tunnel: %d", code)
	}
	if code, _ := rawCall(t, tailnetClientAs(t, tn, "vida-mac"), "GET", "http://potluck-mini:7433/v0/projects/harbor", readToken(t, uj.Remote.TokenFile), nil, nil); code != 401 {
		t.Fatalf("tunnel token over the tailnet: %d", code)
	}
}

// A hijacked hostname: another zm home answers at the URL. The member
// accepts nothing from it (403 to its caller, never its data), keeps
// serving its copy, and recovers when the real home is back.
func TestTunnelHijackedHostname(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	a, p := tunnelHome(t, nil, "mini", tailnettest.Identity{})
	teamHarbor(t, a)
	it := mustf(orchclient.CreateWork(ctx, at(a.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "real", Area: "solver"}))(t)
	r, _ := tunnelMember(t, a, "bea-mac", "harbor", "bea", "member")
	eventually(t, 5*time.Second, func() error {
		if !hasItem(mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{}))(t).Items, it.ID) {
			return fmt.Errorf("not synced")
		}
		return nil
	})
	imp, _ := tunnelHome(t, nil, "mini", tailnettest.Identity{})
	teamHarbor(t, imp)
	p.SetTarget(tunnelOf(t, imp).Status().Listen)

	_, err := orchclient.Sessions(ctx, at(r.owner, "harbor", ""), orchapi.SessionQuery{})
	if !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "not signed by the home") {
		t.Fatalf("read through an impostor: %v", err)
	}
	// The copy still answers.
	if wl := mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{}))(t); !hasItem(wl.Items, it.ID) {
		t.Fatalf("copy %+v", wl)
	}
	// An impostor cannot redeem a member's proof for its own key: a proof
	// for key A, signed by key B, is a wrong code.
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "zed", Role: "member", Via: api.ViaTunnel}))(t)
	code := mustf(project.ParseJoinCode(inv.Code))(t)
	kidA := mustf(tunnelOf(t, r).KeyID())(t)
	p.SetTarget(tunnelOf(t, a).Status().Listen)
	proofA := project.RedeemProof(code.Secret, code.Fingerprint, kidA, "zed-mac")
	if st, body := rawCall(t, signedClient(t, imp, code.Fingerprint), "POST", p.URL()+"/v0/invites/redeem", "",
		api.RedeemRequest{Proof: proofA, Machine: "zed-mac"}, nil); st != 403 {
		t.Fatalf("a proof for another key: %d %s", st, body)
	}
	// Back on the real home: forwarded reads work again. Waited for, bounded:
	// a member's pooled connection to the impostor fails once and is replaced
	// (keep this an eventually, not a mustf).
	eventually(t, 10*time.Second, func() error {
		_, err := orchclient.Sessions(ctx, at(r.owner, "harbor", ""), orchapi.SessionQuery{})
		return err
	})
}

func TestTunnelRedeemRules(t *testing.T) {
	ctx := context.Background()
	a, p := tunnelHome(t, nil, "mini", tailnettest.Identity{})
	teamHarbor(t, a)
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "zed", Role: "member", Via: api.ViaTunnel}))(t)
	code := mustf(project.ParseJoinCode(inv.Code))(t)
	m := startDaemon(t, nil, "zed-mac", tailnettest.Identity{})
	c := signedClient(t, m, code.Fingerprint)
	kid := mustf(tunnelOf(t, m).KeyID())(t)
	redeem := func(body api.RedeemRequest, ip string) (int, string) {
		return rawCall(t, c, "POST", p.URL()+"/v0/invites/redeem", "", body, map[string]string{"CF-Connecting-IP": ip})
	}
	// The code in the clear is answered like a wrong one.
	if st, body := redeem(api.RedeemRequest{Code: inv.Code, Machine: "zed-mac"}, "198.51.100.1"); st != 403 {
		t.Fatalf("code in the clear: %d %s", st, body)
	}
	// Wrong proofs: 5 per address, then even the right one is refused there.
	for i := 0; i < 4; i++ {
		if st, _ := redeem(api.RedeemRequest{Proof: project.RedeemProof("AAAAA-AAAAA-AAAAA-AAAAA-AAAA"+fmt.Sprint(i), code.Fingerprint, kid, "zed-mac"), Machine: "zed-mac"}, "198.51.100.1"); st != 403 {
			t.Fatalf("wrong proof %d: %d", i, st)
		}
	}
	right := api.RedeemRequest{Proof: project.RedeemProof(code.Secret, code.Fingerprint, kid, "zed-mac"), Machine: "zed-mac"}
	if st, _ := redeem(right, "198.51.100.1"); st != 403 {
		t.Fatalf("right proof from a limited address: %d", st)
	}
	// A proof for another machine name does not match.
	if st, _ := redeem(api.RedeemRequest{Proof: right.Proof, Machine: "other-mac"}, "198.51.100.2"); st != 403 {
		t.Fatalf("proof for another machine: %d", st)
	}
	// From another address the right proof redeems, bound to the key.
	st, body := redeem(right, "198.51.100.3")
	var red api.RedeemResponse
	if st != 200 || json.Unmarshal([]byte(body), &red) != nil || red.Token == "" || red.Machine != "zed-mac" {
		t.Fatalf("redeem: %d %s", st, body)
	}
	// The home's own key is refused before any code is read.
	own := signedClient(t, a, code.Fingerprint)
	if st, body := rawCall(t, own, "POST", p.URL()+"/v0/invites/redeem", "", right, nil); st != 403 || !strings.Contains(body, "own machine") {
		t.Fatalf("own key: %d %s", st, body)
	}
	// The tailnet's budget is separate: tunnel noise does not touch it
	// (redeem_test covers the tailnet's limits).
}

// The hedge works the same over the tunnel: a forwarded read on a stalled
// connection is asked again on a new one.
func TestTunnelHedgedRead(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	a, p := tunnelHome(t, nil, "mini", tailnettest.Identity{})
	teamHarbor(t, a)
	r, _ := tunnelMember(t, a, "bea-mac", "harbor", "bea", "member")
	rc := at(r.owner, "harbor", "")
	read := func() time.Duration {
		t.Helper()
		t0 := time.Now()
		mustf(orchclient.Sessions(ctx, rc, orchapi.SessionQuery{}))(t)
		return time.Since(t0)
	}
	read()
	hedging(t, 100*time.Millisecond, 0)
	// StallAll, not Stall: the read may dial a new connection (the warm one
	// busy or closed), which Stall would let through in under a millisecond.
	p.StallAll(1500 * time.Millisecond)
	if d := read(); d < 1400*time.Millisecond {
		t.Fatalf("unhedged read during a 1.5 s stall took %s: the stall did not hold it", d)
	}
	hedging(t, 100*time.Millisecond, 2)
	read()
	p.Stall(20 * time.Second)
	if d := read(); d > 2*time.Second {
		t.Fatalf("hedged read during a 20 s stall took %s", d)
	}
	if h := mustf(r.owner.Health(ctx))(t); h.Hedges == nil || h.Hedges.Won < 1 {
		t.Fatalf("health hedges: %+v", h.Hedges)
	}
}

// The sync loop and outbox behave the same: the home away, reads come from
// the copy and writes queue; back, they drain.
func TestTunnelOfflineOutbox(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	a, p := tunnelHome(t, nil, "mini", tailnettest.Identity{})
	teamHarbor(t, a)
	ao := at(a.owner, "harbor", "")
	it := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "offline", Area: "solver"}))(t)
	r, _ := tunnelMember(t, a, "bea-mac", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-sess-1")
	rs := at(r.owner, "", cs.Address)
	eventually(t, 5*time.Second, func() error {
		if !hasItem(mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{}))(t).Items, it.ID) {
			return fmt.Errorf("not synced")
		}
		return nil
	})
	p.Offline(true)
	waitReachable(t, r, false)
	if wl := mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{}))(t); !hasItem(wl.Items, it.ID) || len(wl.Behind) != 1 {
		t.Fatalf("offline list %+v", wl)
	}
	if _, err := orchclient.Claim(ctx, rs, it.ID, orchapi.ClaimRequest{}); !api.IsCode(err, api.CodeQueued) {
		t.Fatalf("offline claim: %v", err)
	}
	mustf(orchclient.Comment(ctx, rs, it.ID, orchapi.CommentRequest{Body: "while away"}))(t)
	p.Offline(false)
	waitDrained(t, r)
	if ls := mustf(orchclient.Leases(ctx, ao, orchapi.LeaseQuery{Project: "harbor"}))(t); len(ls) != 1 || ls[0].Holder != cs.Address {
		t.Fatalf("authority leases %+v", ls)
	}
	if cm := mustf(orchclient.Comments(ctx, ao, it.ID))(t); len(cm) != 1 || cm[0].Body != "while away" {
		t.Fatalf("authority comments %+v", cm)
	}
}

// Rescope crosses the door: a narrowed grant answers 409 with the new
// header (not the door's 401), the copy drops what left the scope, and a
// grant revoked outright stops the replica.
func TestTunnelRescope(t *testing.T) {
	ctx := context.Background()
	a, _ := tunnelHome(t, nil, "mini", tailnettest.Identity{})
	teamHarbor(t, a)
	ao := at(a.owner, "harbor", "")
	billing := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "billing item", Area: "billing"}))(t)
	r, j := tunnelMember(t, a, "bea-mac", "harbor", "bea", "member")
	sv := j.Remote.ScopeVersion
	eventually(t, 5*time.Second, func() error {
		_, err := orchclient.GetWork(ctx, r.owner, billing.ID)
		return err
	})
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "guest", Project: "harbor", Area: "solver"}))(t)
	mustf(a.owner.Revoke(ctx, api.GrantRequest{Identity: "bea", Project: "harbor"}))(t)
	eventually(t, 5*time.Second, func() error {
		if _, err := orchclient.GetWork(ctx, r.owner, billing.ID); !api.IsCode(err, api.CodeNotFound) {
			return fmt.Errorf("after narrowing, billing: %v", err)
		}
		return nil
	})
	rs := mustf(r.owner.Remotes(ctx))(t)
	if len(rs) != 1 || rs[0].ScopeVersion <= sv || len(rs[0].Areas) != 1 || rs[0].Areas[0] != "solver" || rs[0].State != api.RemoteOK {
		t.Fatalf("header after rescope: %+v", rs)
	}
	mustf(a.owner.Revoke(ctx, api.GrantRequest{Identity: "bea", Project: "harbor", Area: "solver"}))(t)
	eventually(t, 5*time.Second, func() error {
		_, _ = orchclient.Sessions(ctx, at(r.owner, "harbor", ""), orchapi.SessionQuery{})
		if rs := mustf(r.owner.Remotes(ctx))(t); rs[0].State != api.RemoteRevoked {
			return fmt.Errorf("remote %+v", rs[0])
		}
		return nil
	})
}

// The home restarts inside the 5-minute window: a live write captured on
// the way (as Cloudflare could) and sent again is refused, and the mail
// exists once. The write carried a write id, which the mail took.
func TestTunnelReplayAfterHomeRestart(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	a, p := tunnelHome(t, nil, "mini", tailnettest.Identity{})
	teamHarbor(t, a)
	ao := at(a.owner, "harbor", "")
	_ = ao
	// A capturing proxy in front of the stand-in edge.
	type captured struct {
		method, uri string
		hdr         http.Header
		body        []byte
	}
	var mu sync.Mutex
	var last *captured
	target, _ := url.Parse(p.URL())
	rp := httputil.NewSingleHostReverseProxy(target)
	cap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost && (r.URL.Path == "/v0/mail" || r.URL.Path == "/v0/team/writes") {
			mu.Lock()
			last = &captured{r.Method, r.URL.RequestURI(), r.Header.Clone(), b}
			mu.Unlock()
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		rp.ServeHTTP(w, r)
	}))
	t.Cleanup(cap.Close)
	mustf(a.owner.TunnelUp(ctx, api.TunnelUpRequest{Listen: tunnelOf(t, a).Status().Listen, URL: cap.URL}))(t)
	r, _ := tunnelMember(t, a, "bea-mac", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-sess-1")
	m := mustf(orchclient.Send(ctx, at(r.owner, "harbor", cs.Address), orchapi.SendMailRequest{Kind: orchapi.MailKindReport, To: "human:mini-human", Body: "sent once"}))(t)
	mu.Lock()
	got := last
	mu.Unlock()
	// With a copy, the write goes as a batch (its write id in the body);
	// without one it is forwarded with Zm-Write-Id. Either way the mail took
	// the write id.
	if got == nil || !(strings.EqualFold(m.ID, api.GetHeader(got.hdr, api.HeaderWriteID)) || bytes.Contains(got.body, []byte(m.ID))) {
		t.Fatalf("live write id: captured %+v, mail %+v", got, m)
	}
	sent := func() int {
		n := 0
		for _, x := range mustf(orchclient.Mail(ctx, at(a.owner, "harbor", ""), orchapi.MailQuery{}))(t) {
			if x.Body == "sent once" {
				n++
			}
		}
		return n
	}

	// Restart the home: same key and home; the replay journal outlives it.
	a.restart(nil, tailnettest.Identity{})
	var st api.TunnelStatus
	eventually(t, 5*time.Second, func() error {
		if st = tunnelOf(t, a).Status(); !st.Running {
			return fmt.Errorf("tunnel listener not up")
		}
		return nil
	})
	p.SetTarget(st.Listen)
	req, _ := http.NewRequest(got.method, p.URL()+got.uri, bytes.NewReader(got.body))
	req.Header = got.hdr.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 400 || !strings.Contains(string(b), "already received") {
		t.Fatalf("replay after restart: %d %s", resp.StatusCode, b)
	}
	if n := sent(); n != 1 {
		t.Fatalf("mail after the replay: %d copies", n)
	}
	// The member still works after the restart (its next calls are fresh).
	eventually(t, 5*time.Second, func() error {
		_, err := orchclient.Send(ctx, at(r.owner, "harbor", cs.Address), orchapi.SendMailRequest{Kind: orchapi.MailKindReport, To: "human:mini-human", Body: "after"})
		return err
	})
}

// A live write with a write id runs once, whatever route it names: the
// same native-issue create or comment delivered twice (a retry, a resend)
// is answered from its record, over the tailnet and over the tunnel.
func TestLiveWritesDedupeByWriteID(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a, _ := tunnelHome(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"})
	teamHarbor(t, a)
	ao := at(a.owner, "harbor", "")
	if res, err := orchclient.AddTracker(ctx, ao, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", WaitSeconds: 10}); err != nil || !res.OK {
		t.Fatalf("add native tracker: %+v %v", res, err)
	}
	fp := mustf(tunnelOf(t, a).Fingerprint())(t)
	tr, tj := replica(t, tn, a, "vida-mac", "nVIDA", "harbor", "vida", "member")
	ur, uj := tunnelMember(t, a, "bea-mac", "harbor", "bea", "member")
	_ = tr
	for _, via := range []struct {
		name, base, tok string
		hc              *http.Client
	}{
		{"tailnet", "http://potluck-mini:7433", readToken(t, tj.Remote.TokenFile), tailnetClientAs(t, tn, "vida-mac")},
		{"tunnel", a.cfg.File.Tunnel.URL, readToken(t, uj.Remote.TokenFile), signedClient(t, ur, fp)},
	} {
		wid := ids.New()
		hdr := map[string]string{api.HeaderWriteID: wid, api.HeaderProject: uj.Remote.Project.ID}
		before := mustf(orchclient.Issues(ctx, ao, orchapi.IssueQuery{Project: "harbor"}))(t).Total
		st1, b1 := rawCall(t, via.hc, "POST", via.base+"/v0/issues", via.tok, orchapi.NewIssueRequest{Title: "once via " + via.name, Area: "solver"}, hdr)
		st2, b2 := rawCall(t, via.hc, "POST", via.base+"/v0/issues", via.tok, orchapi.NewIssueRequest{Title: "once via " + via.name, Area: "solver"}, hdr)
		var i1, i2 orchapi.Issue
		_ = json.Unmarshal([]byte(b1), &i1)
		_ = json.Unmarshal([]byte(b2), &i2)
		if st1 != 200 || st2 != 200 || i1.ID == "" || i1.ID != i2.ID {
			t.Fatalf("%s: create twice: %d %s / %d %s", via.name, st1, b1, st2, b2)
		}
		if after := mustf(orchclient.Issues(ctx, ao, orchapi.IssueQuery{Project: "harbor"}))(t).Total; after != before+1 {
			t.Fatalf("%s: %d issues after a create sent twice (was %d)", via.name, after, before)
		}
		chdr := map[string]string{api.HeaderWriteID: ids.New(), api.HeaderProject: uj.Remote.Project.ID}
		for i := 0; i < 2; i++ {
			if st, b := rawCall(t, via.hc, "POST", via.base+"/v0/issues/"+i1.ID+"/comments", via.tok, orchapi.IssueCommentRequest{Text: "one comment"}, chdr); st != 200 {
				t.Fatalf("%s: comment %d: %d %s", via.name, i, st, b)
			}
		}
		if cs := mustf(orchclient.IssueComments(ctx, ao, i1.ID))(t); len(cs) != 1 {
			t.Fatalf("%s: %d comments after one sent twice", via.name, len(cs))
		}
		// A new write id is a new write.
		hdr[api.HeaderWriteID] = ids.New()
		if st, b := rawCall(t, via.hc, "POST", via.base+"/v0/issues", via.tok, orchapi.NewIssueRequest{Title: "twice via " + via.name, Area: "solver"}, hdr); st != 200 || strings.Contains(b, i1.ID) {
			t.Fatalf("%s: a new write id: %d %s", via.name, st, b)
		}
	}
	// Through the replica's socket, a live issue write carries its own id.
	is := mustf(orchclient.CreateIssue(ctx, at(ur.owner, "harbor", ""), orchapi.NewIssueRequest{Title: "through the replica", Area: "solver"}))(t)
	if got := mustf(orchclient.Issue(ctx, ao, is.ID))(t); got.Title != "through the replica" {
		t.Fatalf("authority has %+v", got)
	}
}

// Live write records are tied to their request, only for writes, capped in
// size and in number.
func TestLiveWriteRecordRules(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	a, _ := tunnelHome(t, nil, "mini", tailnettest.Identity{})
	teamHarbor(t, a)
	ao := at(a.owner, "harbor", "")
	if res, err := orchclient.AddTracker(ctx, ao, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", WaitSeconds: 10}); err != nil || !res.OK {
		t.Fatalf("add native tracker: %+v %v", res, err)
	}
	fp := mustf(tunnelOf(t, a).Fingerprint())(t)
	ur, uj := tunnelMember(t, a, "bea-mac", "harbor", "bea", "member")
	hc, base, tok, pid := signedClient(t, ur, fp), a.cfg.File.Tunnel.URL, readToken(t, uj.Remote.TokenFile), uj.Remote.Project.ID
	call := func(path, wid string, body any) (int, string, http.Header) {
		t.Helper()
		b, _ := json.Marshal(body)
		req, _ := http.NewRequest("POST", base+path, bytes.NewReader(b))
		req.Header.Set(api.HeaderProtocol, api.Protocol)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(api.HeaderAuthorization, "Bearer "+tok)
		req.Header.Set(api.HeaderProject, pid)
		req.Header.Set(api.HeaderWriteID, wid)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out), resp.Header
	}
	liveRows := func() int {
		db := mustf(a.dm.Deps.Store.Project(authz.All(), authz.Read, pid))(t)
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM team_writes WHERE req != ''`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// A write id reused for another request: 409, never the old answer.
	wid := ids.New()
	if st, b, _ := call("/v0/issues", wid, orchapi.NewIssueRequest{Title: "first", Area: "solver"}); st != 200 {
		t.Fatalf("first: %d %s", st, b)
	}
	if st, b, _ := call("/v0/issues", wid, orchapi.NewIssueRequest{Title: "second", Area: "solver"}); st != 409 || strings.Contains(b, "first") {
		t.Fatalf("reused write id: %d %s", st, b)
	}
	// Reads are never recorded, write id or not.
	n := liveRows()
	sql := map[string]string{"query": "SELECT count(*) AS n FROM issues"}
	for i := 0; i < 2; i++ {
		if st, b, h := call("/v0/issues/sql?project="+pid, wid+"X", sql); st == 409 || h.Get(api.HeaderWriteReplayed) != "" {
			t.Fatalf("sql %d: %d %s %v", i, st, b, h)
		}
	}
	if liveRows() != n {
		t.Fatal("a read was recorded")
	}
	// Over MaxRecordedBody only the status is kept.
	old := team.MaxRecordedBody
	team.MaxRecordedBody = 16
	t.Cleanup(func() { team.MaxRecordedBody = old })
	big := ids.New()
	call("/v0/issues", big, orchapi.NewIssueRequest{Title: "big answer", Area: "solver"})
	if st, b, h := call("/v0/issues", big, orchapi.NewIssueRequest{Title: "big answer", Area: "solver"}); st != 200 || strings.TrimSpace(b) != "{}" || h.Get(api.HeaderWriteReplayed) != "1" {
		t.Fatalf("big replay: %d %s %v", st, b, h)
	}
	// A body over the limit is refused whole (413), never cut short.
	oldBody := team.MaxLiveBody
	team.MaxLiveBody = 64
	if st, b, _ := call("/v0/issues", ids.New(), orchapi.NewIssueRequest{Title: strings.Repeat("long ", 40), Area: "solver"}); st != 413 {
		t.Fatalf("over the body limit: %d %s", st, b)
	}
	team.MaxLiveBody = oldBody
	// At most MaxLiveRecords per project (pruned before each insert here).
	oldMax, oldEvery := team.MaxLiveRecords, team.LivePruneEvery
	team.MaxLiveRecords, team.LivePruneEvery = 2, 1
	t.Cleanup(func() { team.MaxLiveRecords, team.LivePruneEvery = oldMax, oldEvery })
	for i := 0; i < 5; i++ {
		call("/v0/issues", ids.New(), orchapi.NewIssueRequest{Title: fmt.Sprint("n", i), Area: "solver"})
	}
	if got := liveRows(); got > 3 {
		t.Fatalf("%d live records with a cap of 2", got)
	}
}
