package team_test

import (
	"context"
	"github.com/sengirunners/potluck/kit/teamlink"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// nativeAuthority is authority with native issues bound as tracker "issue".
func nativeAuthority(t *testing.T, tn *tailnettest.Net) (*daemon, *client.Client) {
	t.Helper()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	if res, err := orchclient.AddTracker(context.Background(), ao, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", WaitSeconds: 10}); err != nil || !res.OK {
		t.Fatalf("add native tracker: %+v %v", res, err)
	}
	return a, ao
}

// A replica has no native store of its own: on its socket, with no host, no
// token and no project named, a client reaches the project's issues at the
// authority, which is where bd-zm's calls go on a member's machine.
func TestReplicaIssues(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := nativeAuthority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	rc := at(r.owner, "harbor", "")

	mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "made at the authority", Area: "solver"}))(t)
	// Found with no project named (bd's first call), merged with this
	// machine's own, which has none.
	ps := mustf(orchclient.IssueProjects(ctx, r.owner))(t)
	if len(ps.Projects) != 1 || ps.Projects[0].Slug != "harbor" || len(ps.Skipped) != 0 {
		t.Fatalf("projects: %+v", ps)
	}
	is := mustf(orchclient.CreateIssue(ctx, rc, orchapi.NewIssueRequest{Title: "made through the replica", Area: "solver"}))(t)
	if got := mustf(orchclient.Issue(ctx, a.owner, is.ID))(t); got.Title != "made through the replica" {
		t.Fatalf("at the authority: %+v", got)
	}
	// An id needs no project; a list names one, or merges.
	if got := mustf(orchclient.Issue(ctx, r.owner, is.ID))(t); got.ID != is.ID {
		t.Fatalf("by id: %+v", got)
	}
	named := mustf(orchclient.Issues(ctx, r.owner, orchapi.IssueQuery{Project: "harbor"}))(t)
	merged := mustf(orchclient.Issues(ctx, r.owner, orchapi.IssueQuery{}))(t)
	if named.Total != 2 || len(named.Issues) != 2 || merged.Total != 2 || len(merged.Issues) != 2 {
		t.Fatalf("named %+v, merged %+v", named, merged)
	}
	// Every other route a bd verb uses.
	mustf(orchclient.CommentIssue(ctx, rc, is.ID, orchapi.IssueCommentRequest{Text: "through the replica"}))(t)
	mustf(orchclient.AddIssueLabel(ctx, rc, is.ID, "proxied"))(t)
	if cs := mustf(orchclient.IssueComments(ctx, r.owner, is.ID))(t); len(cs) != 1 {
		t.Fatalf("comments: %+v", cs)
	}
	if ls := mustf(orchclient.IssueLabels(ctx, rc, "harbor", nil))(t); len(ls) != 1 || ls[0] != "proxied" {
		t.Fatalf("labels: %+v", ls)
	}
	if st := mustf(orchclient.IssueStats(ctx, rc, "harbor"))(t); st.TotalIssues != 2 {
		t.Fatalf("stats: %+v", st)
	}
	mustf(orchclient.Remember(ctx, rc, "k", "v"))(t)
	if m := mustf(orchclient.Memories(ctx, rc, "harbor"))(t); m["k"] != "v" {
		t.Fatalf("memories: %+v", m)
	}
	if res := mustf(orchclient.IssueSQL(ctx, rc, "harbor", "SELECT count(*) AS n FROM issues"))(t); len(res.Rows) != 1 {
		t.Fatalf("sql: %+v", res)
	}
}

// A claim through the replica is held by the session that made it, at the
// authority: a second session is refused, a hook session's lease is bound,
// and a session that ends its own claim has not lost a lease.
func TestReplicaIssueClaimIsSessionHeld(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := nativeAuthority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	one := register(t, r, "harbor", "claim-sess-1")
	two := register(t, r, "harbor", "claim-sess-2")
	heartbeat(t, r)
	lease := func(key string) *orchapi.Lease {
		t.Helper()
		for _, l := range mustf(orchclient.Leases(ctx, a.owner, orchapi.LeaseQuery{}))(t) {
			if l.Key == key {
				return &l
			}
		}
		return nil
	}

	is := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "claim me", Area: "solver"}))(t)
	key := "issue:" + is.ID
	if got := mustf(orchclient.ClaimIssue(ctx, at(r.owner, "", one.Address), is.ID))(t); got.ClaimedBy != one.Address {
		t.Fatalf("claim: %+v", got)
	}
	if l := lease(key); l == nil || l.Holder != one.Address || !l.Bound {
		t.Fatalf("a hook session's claim (bound: renewed while it runs): %+v", l)
	}
	if _, err := orchclient.ClaimIssue(ctx, at(r.owner, "", two.Address), is.ID); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("second session's claim: %v", err)
	}
	if l := lease(key); l == nil || l.Holder != one.Address {
		t.Fatalf("after the refused claim: %+v", l)
	}

	// Closing your own claim is a release, not a lost lease: the heartbeat
	// learns the key, the close drops it.
	heartbeat(t, r)
	mustf(orchclient.CloseIssue(ctx, at(r.owner, "", one.Address), is.ID, orchapi.CloseIssueRequest{Reason: "done"}))(t)
	heartbeat(t, r)
	if got := mustf(orchclient.Session(ctx, r.owner, one.Address))(t); got.State == orchapi.StateNeedsHuman {
		t.Fatalf("closing its own claim counted as lost: %+v", got)
	}
	if l := lease(key); l != nil {
		t.Fatalf("lease after the close: %+v", l)
	}

	// Claiming it again is held again. A later real loss drops the held
	// cache without manufacturing a human request.
	again := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "claimed twice", Area: "solver"}))(t)
	mustf(orchclient.ClaimIssue(ctx, at(r.owner, "", one.Address), again.ID))(t)
	heartbeat(t, r)
	mustf(orchclient.CloseIssue(ctx, at(r.owner, "", one.Address), again.ID, orchapi.CloseIssueRequest{Reason: "oops"}))(t)
	mustf(orchclient.ReopenIssue(ctx, at(r.owner, "", one.Address), again.ID, orchapi.ReopenIssueRequest{}))(t)
	empty := ""
	mustf(orchclient.UpdateIssue(ctx, ao, again.ID, orchapi.UpdateIssueRequest{Assignee: &empty}))(t)
	mustf(orchclient.ClaimIssue(ctx, at(r.owner, "", one.Address), again.ID))(t)
	heartbeat(t, r)
	heartbeat(t, r)
	if got := mustf(orchclient.Session(ctx, r.owner, one.Address))(t); got.State == orchapi.StateNeedsHuman {
		t.Fatalf("a lease claimed again counted as lost: %+v", got)
	}
	beforeLoss := mustf(orchclient.Session(ctx, r.owner, one.Address))(t)
	if !teamService(t, r).ReportsLeaseHeld(one.Address, "issue:"+again.ID) {
		t.Fatal("reclaimed issue was not reported held before its loss")
	}
	mustf(orchclient.CloseIssue(ctx, ao, again.ID, orchapi.CloseIssueRequest{Reason: "not yours"}))(t)
	heartbeat(t, r)
	if got := mustf(orchclient.Session(ctx, r.owner, one.Address))(t); got.State != beforeLoss.State || got.Note != beforeLoss.Note {
		t.Fatalf("a lost lease changed the session request: %+v", got)
	}
	if teamService(t, r).ReportsLeaseHeld(one.Address, "issue:"+again.ID) {
		t.Fatal("a lost lease claimed twice remained reported held")
	}
	mustf(orchclient.SetState(ctx, r.owner, one.Address, orchapi.StateRequest{State: orchapi.StateRunning}))(t)
	heartbeat(t, r)

	// So is ending it with zm work status, which the outbox carries.
	viaWork := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "done through zm work", Area: "solver"}))(t)
	mustf(orchclient.ClaimIssue(ctx, at(r.owner, "", one.Address), viaWork.ID))(t)
	heartbeat(t, r)
	eventually(t, 10*time.Second, func() error {
		_, err := orchclient.GetWork(ctx, r.owner, "issue:"+viaWork.ID)
		return err
	})
	mustf(orchclient.SetStatus(ctx, at(r.owner, "", one.Address), "issue:"+viaWork.ID, orchapi.StatusRequest{Status: orchapi.WorkDone}))(t)
	waitDrained(t, r)
	heartbeat(t, r)
	if got := mustf(orchclient.Session(ctx, r.owner, one.Address))(t); got.State == orchapi.StateNeedsHuman {
		t.Fatalf("zm work status on its own claim counted as lost: %+v", got)
	}
	if l := lease("issue:" + viaWork.ID); l != nil {
		t.Fatalf("lease after zm work status: %+v", l)
	}

	// A native lease taken away by its owner is evicted, without changing
	// the session's genuine request state.
	lost := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "taken away", Area: "solver"}))(t)
	mustf(orchclient.ClaimIssue(ctx, at(r.owner, "", one.Address), lost.ID))(t)
	heartbeat(t, r)
	beforeLoss = mustf(orchclient.Session(ctx, r.owner, one.Address))(t)
	if !teamService(t, r).ReportsLeaseHeld(one.Address, "issue:"+lost.ID) {
		t.Fatal("native claim was not reported held before its loss")
	}
	mustf(orchclient.CloseIssue(ctx, ao, lost.ID, orchapi.CloseIssueRequest{Reason: "not yours"}))(t)
	heartbeat(t, r)
	if got := mustf(orchclient.Session(ctx, r.owner, one.Address))(t); got.State != beforeLoss.State || got.Note != beforeLoss.Note {
		t.Fatalf("native lease loss changed the session request: %+v", got)
	}
	if teamService(t, r).ReportsLeaseHeld(one.Address, "issue:"+lost.ID) {
		t.Fatal("a lost native lease remained reported held")
	}

	// A managed session's claim is bound (presence says managed).
	sessionModule(t, r).Probe = func(...string) orchapi.AdmissionProbe {
		return orchapi.AdmissionProbe{MemoryFreePercent: 80, DiskFreeMB: 100000}
	}
	dir := t.TempDir()
	ms := mustf(orchclient.Spawn(ctx, at(r.owner, "harbor/solver", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: dir}))(t)
	t.Cleanup(func() {
		_, _ = orchclient.Stop(context.Background(), r.owner, ms.Address, orchapi.StopRequest{Signal: "KILL"})
	})
	heartbeat(t, r)
	bound := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "bound", Area: "solver"}))(t)
	mustf(orchclient.ClaimIssue(ctx, at(r.owner, "", ms.Address), bound.ID))(t)
	if l := lease("issue:" + bound.ID); l == nil || l.Holder != ms.Address || !l.Bound {
		t.Fatalf("a managed session's claim is not bound: %+v", l)
	}
}

// With the authority away nothing reads as "no issues" or "not found", and
// right after this daemon starts it asks rather than assuming the worst.
func TestReplicaIssuesAuthorityAway(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := nativeAuthority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	is := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "kept at the authority", Area: "solver"}))(t)
	mustf(orchclient.Issue(ctx, r.owner, is.ID))(t)

	// A restarted replica has a copy and no answer from the authority yet.
	r.restart(tn, tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea-mac", Login: "bea@example.com", Host: "bea-mac"})
	if ps := mustf(orchclient.IssueProjects(ctx, r.owner))(t); len(ps.Projects) != 1 {
		t.Fatalf("projects right after a restart: %+v", ps)
	}
	if got := mustf(orchclient.Issue(ctx, r.owner, is.ID))(t); got.ID != is.ID {
		t.Fatalf("issue right after a restart: %+v", got)
	}

	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	t.Cleanup(func() { tn.Cut("potluck-bea-mac", "potluck-mini", false) })
	ps := mustf(orchclient.IssueProjects(ctx, r.owner))(t)
	if len(ps.Projects) != 0 || len(ps.Skipped) != 1 || ps.Skipped[0].Project == "" {
		t.Fatalf("projects with the authority away: %+v", ps)
	}
	if _, err := orchclient.Issue(ctx, r.owner, is.ID); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("by id with the authority away: %v", err)
	}
	if _, err := orchclient.Issues(ctx, r.owner, orchapi.IssueQuery{Project: ps.Skipped[0].Project}); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("list by the skipped project: %v", err)
	}
	if _, err := orchclient.CreateIssue(ctx, at(r.owner, "harbor", ""), orchapi.NewIssueRequest{Title: "x"}); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("create with the authority away: %v", err)
	}
	if l, err := orchclient.Issues(ctx, r.owner, orchapi.IssueQuery{}); err != nil || len(l.Skipped) != 1 {
		t.Fatalf("a merged list hides the authority: %+v %v", l, err)
	}
}

// A guest's replica forwards as the guest: its area, read-only, and what it
// may not see answers like what does not exist.
func TestReplicaIssuesGuest(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := nativeAuthority(t, tn)
	vis := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "in solver", Area: "solver"}))(t)
	hid := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "HIDDEN billing", Area: "billing"}))(t)
	r, _ := replica(t, tn, a, "vida-mac", "nVIDA", "harbor/solver", "vida", "guest")

	l := mustf(orchclient.Issues(ctx, r.owner, orchapi.IssueQuery{Project: "harbor"}))(t)
	if l.Total != 1 || len(l.Issues) != 1 || l.Issues[0].ID != vis.ID {
		t.Fatalf("guest list: %+v", l)
	}
	_, e1 := orchclient.Issue(ctx, r.owner, hid.ID)
	_, e2 := orchclient.Issue(ctx, r.owner, "harbor-nope9")
	if !api.IsCode(e1, api.CodeNotFound) || strings.ReplaceAll(e1.Error(), hid.ID, "X") != strings.ReplaceAll(e2.Error(), "harbor-nope9", "X") {
		t.Fatalf("hidden %v, missing %v", e1, e2)
	}
	if _, err := orchclient.CreateIssue(ctx, at(r.owner, "harbor/solver", ""), orchapi.NewIssueRequest{Title: "guest write", Area: "solver"}); err == nil {
		t.Fatal("a guest wrote an issue through its replica")
	}
	if _, err := orchclient.IssueSQL(ctx, at(r.owner, "harbor", ""), "harbor", "SELECT count(*) FROM issues"); err == nil {
		t.Fatal("a guest ran raw SQL through its replica")
	}
}

// The replica's socket is its owner's, not the authority's: redacting a
// comment through it needs the authority's own rule, and an authority's
// redaction shows on the next read, since no native text is kept here.
func TestReplicaIssueRedact(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := nativeAuthority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	is := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "a leak", Area: "solver"}))(t)
	const secret = "AKIA-PROXIED-SECRET-0007"
	c := mustf(orchclient.CommentIssue(ctx, at(r.owner, "harbor", ""), is.ID, orchapi.IssueCommentRequest{Text: secret}))(t)

	path := "/v0/issues/" + is.ID + "/comments/" + c.ID + "/redact"
	err := at(r.owner, "harbor", "").Do(ctx, http.MethodPost, path, map[string]any{}, nil)
	if !api.IsCode(err, api.CodeForbidden) && !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a member redacting through its replica: %v", err)
	}
	if cs := mustf(orchclient.IssueComments(ctx, ao, is.ID))(t); len(cs) != 1 || cs[0].Text != secret {
		t.Fatalf("comment after the refused redaction: %+v", cs)
	}
	if err := ao.Do(ctx, http.MethodPost, path, map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, 3*time.Second, func() error {
		for _, x := range mustf(orchclient.IssueComments(ctx, r.owner, is.ID))(t) {
			if strings.Contains(x.Text, secret) {
				return api.Errorf(api.CodeConflict, "the replica still reads the secret")
			}
		}
		return nil
	})
}

// A list that takes longer than teamlink.CallTimeout to arrive is not a stuck
// call: every native issue over a home uplink is 36 MB. CallTimeout bounds
// the silences. A real stall fails the call: the caller sees the connection
// drop, never a short answer that ends cleanly. The silence timer restarts on
// each read by the relay, so a loaded runner's scheduling gaps count as
// silence too: CallTimeout stays five times each read's delay.
func TestReplicaIssuesLongTransfer(t *testing.T) {
	old := teamlink.CallTimeout
	teamlink.CallTimeout = time.Second
	t.Cleanup(func() { teamlink.CallTimeout = old })
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := nativeAuthority(t, tn)
	r := startDaemon(t, tn, "bea-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea-mac", Login: "bea@example.com", Host: "bea-mac"})
	stall := &issueStallTransport{
		altTransport: altTransport{r.dm.Deps.Transport(core.ViaTailnet)},
		reached:      make(chan struct{}),
	}
	r.dm.Deps.AddTransport(stall)
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: "alt+" + inv.Code}))(t)
	big := strings.Repeat("a long description ", 1<<14) // 311 KiB
	// Four: over the 1 MiB a hedged read takes whole before relaying it.
	for i := 0; i < 4; i++ {
		mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "big", Area: "solver", Description: big}))(t)
	}
	want := mustf(orchclient.Issues(ctx, ao, orchapi.IssueQuery{Project: "harbor", All: true}))(t)

	tn.Slow("potluck-bea-mac", "potluck-mini", 200*time.Millisecond) // about 20 reads: 4 s
	t0 := time.Now()
	got := mustf(orchclient.Issues(ctx, r.sock, orchapi.IssueQuery{Project: "harbor", All: true}))(t)
	if d := time.Since(t0); d < 2*teamlink.CallTimeout || len(got.Issues) != len(want.Issues) {
		t.Fatalf("in %v: %d issues of %d", d, len(got.Issues), len(want.Issues))
	}

	// Stall the authority's body after the hedge's 1 MiB read-ahead. Waiting
	// until the socket caller reads before slowing the link is racy: the
	// remaining upstream bytes may already be buffered in the socket.
	stall.armed.Store(true)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return net.Dial("unix", r.cfg.SocketPath())
	}}
	t.Cleanup(transport.CloseIdleConnections)
	hc := &http.Client{Transport: transport}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(callCtx, http.MethodGet, "http://zm/v0/issues?project=harbor&all=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if _, err := io.ReadFull(resp.Body, make([]byte, 1024)); err != nil {
		t.Fatal(err)
	}
	// Keep consuming while the proxy drains its read-ahead buffer, allowing
	// its next authority read to reach the gate regardless of socket buffers.
	b, err := io.ReadAll(resp.Body)
	select {
	case <-stall.reached:
	default:
		t.Fatal("the authority's body never reached the injected stall")
	}
	if callCtx.Err() != nil {
		t.Fatal("the proxy did not interrupt the stalled body before the client deadline")
	}
	if err == nil {
		t.Fatalf("a stalled transfer ended cleanly after %d more bytes", len(b))
	}
}

// issueStallTransport retains the real transport and response, but can hold
// a single route's body at a known byte boundary. It is installed before
// joining so replica background calls never race with a client replacement.
type issueStallTransport struct {
	altTransport
	armed   atomic.Bool
	reached chan struct{}
	once    sync.Once
}

func (s *issueStallTransport) Client(target core.JoinTarget, opts core.ClientOptions) *http.Client {
	hc := *s.TeamTransport.Client(target, opts)
	hc.Transport = issueStallRoundTripper{RoundTripper: hc.Transport, stall: s}
	return &hc
}

type issueStallRoundTripper struct {
	http.RoundTripper
	stall *issueStallTransport
}

func (s issueStallRoundTripper) CloseIdleConnections() {
	if t, ok := s.RoundTripper.(interface{ CloseIdleConnections() }); ok {
		t.CloseIdleConnections()
	}
}

func (s issueStallRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := s.RoundTripper.RoundTrip(req)
	if err == nil && req.Method == http.MethodGet && req.URL.Path == "/v0/issues" && s.stall.armed.Load() {
		resp.Body = &issueStallBody{ReadCloser: resp.Body, ctx: req.Context(), stall: s.stall, remaining: (1 << 20) + (32 << 10)}
	}
	return resp, err
}

type issueStallBody struct {
	io.ReadCloser
	ctx       context.Context
	stall     *issueStallTransport
	remaining int
}

func (b *issueStallBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		b.stall.once.Do(func() { close(b.stall.reached) })
		<-b.ctx.Done()
		return 0, b.ctx.Err()
	}
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= n
	return n, err
}

// An issue moved to another project on the same authority (zm issues move)
// is found by its old id through a replica joined to both, on every route a
// bd verb uses, though bd names the project the old prefix routes to: that
// project's token cannot see the new project, so the replica asks its other
// joined projects there. A replica joined only to the old project gets 404.
func TestReplicaFollowsAMovedIssue(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "zm"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "zm"))(t)
	ao, az := at(a.owner, "harbor", ""), at(a.owner, "zm", "")
	mustf(orchclient.SetupIssues(ctx, ao, "harbor"))(t)
	mustf(orchclient.SetupIssues(ctx, az, "zm"))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "zm", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	only, _ := replica(t, tn, a, "cyd-mac", "nCYD", "harbor", "cyd", "member")

	it := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "zm: moves"}))(t)
	res := mustf(orchclient.MoveIssues(ctx, ao, orchapi.IssueMoveRequest{To: "zm", IDs: []string{it.ID}}))(t)
	if res.Moved != 1 {
		t.Fatalf("move %+v", res)
	}
	nid := res.Items[0].NewID
	rc := at(r.owner, "harbor", "") // as bd-zm calls a harbor- id
	d := mustf(orchclient.Issue(ctx, rc, it.ID))(t)
	if d.ID != nid || d.MovedFrom != it.ID {
		t.Fatalf("old id through the replica: %+v", d.Issue)
	}
	mustf(orchclient.CommentIssue(ctx, rc, it.ID, orchapi.IssueCommentRequest{Text: "by the old id"}))(t)
	mustf(orchclient.UpdateIssue(ctx, rc, it.ID, orchapi.UpdateIssueRequest{Notes: ptr("noted")}))(t)
	mustf(orchclient.AddIssueLabel(ctx, rc, it.ID, "moved"))(t)
	if cs := mustf(orchclient.IssueComments(ctx, rc, it.ID))(t); len(cs) != 1 {
		t.Fatalf("comments by the old id: %+v", cs)
	}
	mustf(orchclient.CloseIssue(ctx, rc, it.ID, orchapi.CloseIssueRequest{Reason: "done"}))(t)
	if got := mustf(orchclient.Issue(ctx, az, nid))(t); got.Status != orchapi.IssueClosed || got.Notes != "noted" || len(got.Labels) != 1 || len(got.Comments) != 1 {
		t.Fatalf("at the authority: %+v", got)
	}
	if _, err := orchclient.Issue(ctx, rc, "harbor-zzzzz"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a missing id: %v", err)
	}
	// The write landed once: one close in the issue's history.
	closes := 0
	for _, h := range mustf(orchclient.IssueHistory(ctx, az, nid))(t) {
		if h.Kind == "closed" {
			closes++
		}
	}
	if closes != 1 {
		t.Fatalf("closes in history: %d", closes)
	}
	if _, err := orchclient.Issue(ctx, at(only.owner, "harbor", ""), it.ID); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a replica that has not joined zm: %v", err)
	}
}

// An old id finds only what it moved to, never an issue that shares its
// handle under another prefix: the authority follows a redirect, it does not
// swap prefixes.
func TestReplicaMovedIssueNeverSwapsPrefixes(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	for _, p := range []string{"sengi", "zm"} {
		mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: p}))(t)
		mustf(a.owner.ProjectTeam(ctx, p))(t)
		mustf(orchclient.SetupIssues(ctx, at(a.owner, p, ""), p))(t)
		inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: p, As: "bea", Role: "member"}))(t)
		mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	}
	ao, az, as := at(a.owner, "harbor", ""), at(a.owner, "zm", ""), at(a.owner, "sengi", "")
	mustf(orchclient.SetupIssues(ctx, ao, "harbor"))(t)
	// zm-qqq and zm-ppp are unrelated issues; harbor-qqq never existed, and
	// harbor-ppp moved to sengi.
	mustf(orchclient.CreateIssue(ctx, az, orchapi.NewIssueRequest{Title: "unrelated q", ID: "zm-qqq"}))(t)
	mustf(orchclient.CreateIssue(ctx, az, orchapi.NewIssueRequest{Title: "unrelated p", ID: "zm-ppp"}))(t)
	mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "goes to sengi", ID: "harbor-ppp"}))(t)
	if res := mustf(orchclient.MoveIssues(ctx, ao, orchapi.IssueMoveRequest{To: "sengi", IDs: []string{"harbor-ppp"}}))(t); res.Moved != 1 {
		t.Fatalf("move %+v", res)
	}
	rc := at(r.owner, "harbor", "")
	if _, err := orchclient.CloseIssue(ctx, rc, "harbor-qqq", orchapi.CloseIssueRequest{Reason: "x"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("closing an id that never existed: %v", err)
	}
	w := mustf(orchclient.CloseIssue(ctx, rc, "harbor-ppp", orchapi.CloseIssueRequest{Reason: "done"}))(t)
	if w.ID != "sengi-ppp" {
		t.Fatalf("the old id closed %s, want sengi-ppp", w.ID)
	}
	for _, id := range []string{"zm-qqq", "zm-ppp"} {
		if got := mustf(orchclient.Issue(ctx, az, id))(t); got.Status == orchapi.IssueClosed {
			t.Fatalf("%s was closed through another id", id)
		}
	}
	if got := mustf(orchclient.Issue(ctx, as, "sengi-ppp"))(t); got.Status != orchapi.IssueClosed {
		t.Fatalf("sengi-ppp: %s", got.Status)
	}
	// Two hops (harbor-hop to sengi-hop to zm-hop), three joined projects:
	// the old id is resolved first and the comment goes out once.
	mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "hops", ID: "harbor-hop"}))(t)
	if res := mustf(orchclient.MoveIssues(ctx, ao, orchapi.IssueMoveRequest{To: "sengi", IDs: []string{"harbor-hop"}}))(t); res.Moved != 1 {
		t.Fatalf("first hop %+v", res)
	}
	if res := mustf(orchclient.MoveIssues(ctx, as, orchapi.IssueMoveRequest{To: "zm", IDs: []string{"sengi-hop"}}))(t); res.Moved != 1 {
		t.Fatalf("second hop %+v", res)
	}
	zmID := mustf(a.owner.GetProject(ctx, "zm"))(t).ID
	if d := mustf(orchclient.Issue(ctx, rc, "harbor-hop"))(t); d.ID != "zm-hop" || d.Project != zmID {
		t.Fatalf("two hops by the old id: %s in %s (the zm project is %s: pickMoved compares these)", d.ID, d.Project, zmID)
	}
	mustf(orchclient.CommentIssue(ctx, rc, "harbor-hop", orchapi.IssueCommentRequest{Text: "once"}))(t)
	if cs := mustf(orchclient.IssueComments(ctx, az, "zm-hop"))(t); len(cs) != 1 {
		t.Fatalf("comments after one comment through two hops: %d", len(cs))
	}
}

func ptr[T any](v T) *T { return &v }
