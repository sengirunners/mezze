package team_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/integrations"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A hook session on a member machine claims a native issue through its
// replica: the authority holds a bound lease (renewed from the session's
// presence while it runs), and when the session ends on the member the next
// heartbeat releases it at the authority, with no hour-long wait.
func TestReplicaHookSessionLeaseFollowsPresence(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := nativeAuthority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	ss := register(t, r, "harbor", "hook-lease-1")
	heartbeat(t, r)
	is := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "held by a hook session", Area: "solver"}))(t)
	mustf(orchclient.ClaimIssue(ctx, at(r.owner, "", ss.Address), is.ID))(t)
	held := func() *orchapi.Lease {
		for _, l := range mustf(orchclient.Leases(ctx, a.owner, orchapi.LeaseQuery{}))(t) {
			if l.Key == "issue:"+is.ID {
				return &l
			}
		}
		return nil
	}
	if l := held(); l == nil || !l.Bound || l.Holder != ss.Address {
		t.Fatalf("at the authority: %+v", l)
	}
	if got := mustf(orchclient.Session(ctx, a.owner, ss.Address))(t); got.State != orchapi.StateRunning {
		t.Fatalf("the authority's view of the session: %+v", got)
	}

	// The session-end hook on the member.
	mustf(orchclient.Register(ctx, at(r.owner, "harbor", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "hook-lease-1", End: true}))(t)
	heartbeat(t, r)
	if l := held(); l != nil {
		t.Fatalf("still held after the session ended: %+v", l)
	}
	eventually(t, 10*time.Second, func() error { // the issue follows by write-back (SPEC 17.3)
		if got := mustf(orchclient.Issue(ctx, ao, is.ID))(t); got.ClaimedBy != "" {
			return fmt.Errorf("issue still claimed by %s", got.ClaimedBy)
		}
		return nil
	})
}

// End to end, as an agent outside a cockpit on a member machine runs: its hooks
// (`zm hook claude ...`, integrations.RunHook) register it with the member's
// daemon, it claims a native issue through the replica, and while its tool
// hooks keep coming the authority renews the bound lease past several
// bound TTLs (2 s here); its session-end hook releases it.
func TestHookRegisterClaimRenewEndToEnd(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := shortLeaseAuthority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	hook := func(event string, payload string) {
		t.Helper()
		_, err := integrations.RunHook(ctx, func(as, _ string) integrations.Daemon { return orchclient.Hooks(at(r.owner, "harbor", as)) },
			integrations.HookInput{Agent: "claude", Event: event, Payload: []byte(payload), Getenv: func(string) string { return "" }})
		if err != nil {
			t.Fatalf("hook %s: %v", event, err)
		}
	}
	hook(integrations.HookSessionStart, `{"session_id":"e2e-hook-1"}`)
	hook(integrations.HookPrompt, `{"session_id":"e2e-hook-1"}`)
	var addr string
	for _, ss := range mustf(orchclient.Sessions(ctx, r.owner, orchapi.SessionQuery{}))(t) {
		if ss.RuntimeID == "e2e-hook-1" {
			addr = ss.Address
		}
	}
	if addr == "" {
		t.Fatal("the hook registered no session")
	}
	heartbeat(t, r)
	is := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "claimed from a hook session", Area: "solver"}))(t)
	mustf(orchclient.ClaimIssue(ctx, at(r.owner, "", addr), is.ID))(t)
	held := func() *orchapi.Lease {
		for _, l := range mustf(orchclient.Leases(ctx, a.owner, orchapi.LeaseQuery{}))(t) {
			if l.Key == "issue:"+is.ID {
				return &l
			}
		}
		return nil
	}
	if l := held(); l == nil || !l.Bound {
		t.Fatalf("claim at the authority: %+v", l)
	}
	// 7 s of work: a tool hook and a heartbeat every half second.
	for end := time.Now().Add(7 * time.Second); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		hook(integrations.HookTool, `{"session_id":"e2e-hook-1","tool_name":"Bash","tool_input":{"command":"go test"}}`)
		heartbeat(t, r)
	}
	if l := held(); l == nil || l.Holder != addr || !l.Expires.After(time.Now()) {
		t.Fatalf("after 7 s of work (bound TTL 2 s): %+v", l)
	}
	hook(integrations.HookSessionEnd, `{"session_id":"e2e-hook-1","reason":"exit"}`)
	heartbeat(t, r)
	if l := held(); l != nil {
		t.Fatalf("held after the session-end hook: %+v", l)
	}
}

// shortLeaseAuthority is nativeAuthority with a 2 s bound TTL and a 1 s
// lease tick, so renewals and adoptions happen within a test.
func shortLeaseAuthority(t *testing.T, tn *tailnettest.Net) (*daemon, *client.Client) {
	t.Helper()
	ctx := context.Background()
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), func(c *config.Config) {
			c.File.BoundLeaseTTLSeconds, c.File.LeaseTickSeconds = 2, 1
		})
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.AddArea(ctx, "harbor", api.Area{Name: "solver"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	ao := at(a.owner, "harbor", "")
	if res, err := orchclient.AddTracker(ctx, ao, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", WaitSeconds: 10}); err != nil || !res.OK {
		t.Fatalf("add native tracker: %+v %v", res, err)
	}
	return a, ao
}

// A hook session registered in another project on the member (its cwd is
// outside the team project's checkout; it claims naming the project) is in
// the team project's heartbeat while it holds a key there, so the authority
// knows it: its claim is bound at the next tick, renewed while it runs, and
// released when it ends.
func TestReplicaSessionOfAnotherProjectHoldsItsLease(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := shortLeaseAuthority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	mustf(r.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "side"}))(t)
	ss := register(t, r, "side", "elsewhere-1")
	heartbeat(t, r)
	if _, err := orchclient.Session(ctx, a.owner, ss.Address); err == nil {
		t.Fatal("a session of another project is in the team's presence before it holds anything")
	}
	is := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "claimed from elsewhere", Area: "solver"}))(t)
	mustf(orchclient.ClaimIssue(ctx, at(r.owner, "harbor", ss.Address), is.ID))(t)
	held := func() *orchapi.Lease {
		for _, l := range mustf(orchclient.Leases(ctx, a.owner, orchapi.LeaseQuery{}))(t) {
			if l.Key == "issue:"+is.ID {
				return &l
			}
		}
		return nil
	}
	for end := time.Now().Add(6 * time.Second); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		heartbeat(t, r)
	}
	if got, err := orchclient.Session(ctx, a.owner, ss.Address); err != nil || got.State != orchapi.StateRunning {
		t.Fatalf("the authority's view: %+v %v", got, err)
	}
	if l := held(); l == nil || !l.Bound || !l.Expires.After(time.Now()) {
		t.Fatalf("after 6 s (bound TTL 2 s): %+v", l)
	}
	mustf(orchclient.Register(ctx, at(r.owner, "side", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "elsewhere-1", End: true}))(t)
	heartbeat(t, r)
	if l := held(); l != nil {
		t.Fatalf("held after the session ended: %+v", l)
	}
}

// A session holding keys in two team projects of one authority is in one of
// their heartbeats, stably, so the authority's one presence row does not
// move between them on every beat (no left and registered events each time),
// and its bound leases in both are renewed.
func TestSessionInTwoProjectsOfOneAuthorityDoesNotFlap(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a, _ := shortLeaseAuthority(t, tn)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "second"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "second"))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "second", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	mustf(r.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "side"}))(t)
	ss := register(t, r, "side", "two-projects-1")
	mustf(orchclient.Lease(ctx, at(r.owner, "harbor", ss.Address), orchapi.ClaimRequest{Key: "lock:zone"}))(t)
	// The first lease reaches the presence cache before the second is taken.
	// Its answer can be fenced out of the cache (a harbor copy snapshot
	// began while it was in flight) and land later through the copy sync;
	// a beat before that sees only "second", and the row then moves once to
	// harbor, the lower id: three events from arrival order, not a flap.
	eventually(t, 10*time.Second, func() error {
		heartbeat(t, r)
		if !teamService(t, r).ReportsLeaseHeld(ss.Address, "lock:zone") {
			return fmt.Errorf("lock:zone not in the presence cache yet")
		}
		return nil
	})
	mustf(orchclient.Lease(ctx, at(r.owner, "second", ss.Address), orchapi.ClaimRequest{Key: "lock:atwo"}))(t)
	// Sample once the replica's copies hold both leases: a beat sent before
	// the second lease reached its copy carries one project only, and the
	// one move that follows is the lease arriving, not a flap.
	eventually(t, 10*time.Second, func() error {
		heartbeat(t, r) // bound leases (2 s TTL) stay renewed while waiting
		for proj, key := range map[string]string{"harbor": "lock:zone", "second": "lock:atwo"} {
			ls, err := orchclient.Leases(ctx, at(r.owner, proj, ""), orchapi.LeaseQuery{})
			if err != nil {
				return err
			}
			if !slices.ContainsFunc(ls, func(l orchapi.Lease) bool { return l.Key == key && l.Holder == ss.Address }) {
				return fmt.Errorf("%s's copy does not hold %s yet: %+v", proj, key, ls)
			}
		}
		return nil
	})
	var projects []string
	for end := time.Now().Add(6 * time.Second); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		heartbeat(t, r)
		if got, err := orchclient.Session(ctx, a.owner, ss.Address); err == nil {
			projects = append(projects, got.Project)
		}
	}
	for i := 1; i < len(projects); i++ {
		if projects[i] != projects[0] {
			t.Fatalf("the session moved between projects: %v", projects)
		}
	}
	if len(projects) == 0 {
		t.Fatal("never present at the authority")
	}
	// The row's work names a key of the row's own project only: its members
	// read it, and the other project's keys are not theirs to see (the keys
	// sort so that the other project's would come first).
	want := map[string]string{mustf(a.owner.GetProject(ctx, "harbor"))(t).ID: "lock:zone", mustf(a.owner.GetProject(ctx, "second"))(t).ID: "lock:atwo"}
	got := mustf(orchclient.Session(ctx, a.owner, ss.Address))(t)
	if got.Work != want[got.Project] {
		t.Fatalf("work %q in project %s, want %q", got.Work, got.Project, want[got.Project])
	}
	// The project its row is not in still takes mail for it (SPEC 9 "Across
	// projects"): its entry there marked elsewhere does not make it that
	// project's own.
	other := map[string]string{want0(a, t, "harbor"): "second", want0(a, t, "second"): "harbor"}[got.Project]
	if _, err := orchclient.Send(ctx, at(a.owner, other, ""), orchapi.SendMailRequest{To: ss.Address, Body: "to a session whose row is in the other project"}); err != nil {
		t.Fatalf("mail in %s to it: %v", other, err)
	}
	var registered int
	_ = a.owner.Events(ctx, api.EventQuery{Types: "dev.zm.session.registered,dev.zm.session.left", Since: "0", NoLive: true}, func(e api.Event) error {
		if e.Subject == ss.Address {
			registered++
		}
		return nil
	})
	if registered > 1 {
		t.Fatalf("%d registered/left events for one session", registered)
	}
	ls := mustf(orchclient.Leases(ctx, a.owner, orchapi.LeaseQuery{}))(t)
	bound := 0
	for _, l := range ls {
		if l.Holder == ss.Address && l.Bound && l.Expires.After(time.Now()) {
			bound++
		}
	}
	if bound != 2 {
		t.Fatalf("leases after 6 s (bound TTL 2 s): %+v", ls)
	}
}

// A session whose own project is joined at one authority and that claims in
// a project of another authority is present at that other authority too
// (each authority keeps its own row), so its lease there is bound and
// renewed, not left to run out.
func TestSessionOfOneAuthorityClaimsAtAnother(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a, _ := shortLeaseAuthority(t, tn)
	b := startDaemonWith(t, tn, "mini2", tailnettest.Identity{NodeID: "nMINI2", NodeKey: "nodekey:mini2", Login: "ivo@example.com", Host: "mini2"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), func(c *config.Config) {
			c.File.BoundLeaseTTLSeconds, c.File.LeaseTickSeconds = 2, 1
		})
	mustf(b.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "solver"}))(t)
	mustf(b.owner.ProjectTeam(ctx, "solver"))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	inv := mustf(b.owner.Invite(ctx, api.InviteRequest{Project: "solver", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	ss := register(t, r, "harbor", "two-authorities-1") // its own project: harbor, at a
	mustf(orchclient.Lease(ctx, at(r.owner, "solver", ss.Address), orchapi.ClaimRequest{Key: "lock:at-b"}))(t)
	for end := time.Now().Add(6 * time.Second); time.Now().Before(end); time.Sleep(500 * time.Millisecond) {
		heartbeat(t, r)
	}
	if got, err := orchclient.Session(ctx, a.owner, ss.Address); err != nil || got.State != orchapi.StateRunning {
		t.Fatalf("at its own project's authority: %+v %v", got, err)
	}
	if got, err := orchclient.Session(ctx, b.owner, ss.Address); err != nil || got.State != orchapi.StateRunning {
		t.Fatalf("at the other authority: %+v %v", got, err)
	}
	ls := mustf(orchclient.Leases(ctx, b.owner, orchapi.LeaseQuery{}))(t)
	if len(ls) != 1 || !ls[0].Bound || !ls[0].Expires.After(time.Now()) {
		t.Fatalf("its lease at the other authority after 6 s (bound TTL 2 s): %+v", ls)
	}
}

// teamService is d's team module.
func teamService(t *testing.T, d *daemon) *team.Service {
	t.Helper()
	for _, m := range d.dm.Modules() {
		if s, ok := m.(*team.Service); ok {
			return s
		}
	}
	t.Fatal("no team module")
	return nil
}

// A session holding keys in two projects of one authority releases the key
// in the project its presence row is in: the row moves to the other project
// without ever passing through ended, so its lease there stays held (it
// used to be dropped with the session "gone from presence", then reported
// lost), whichever project's heartbeat lands first.
func TestPresenceMoveKeepsTheOtherLease(t *testing.T) {
	// R: a heartbeat to the row's project, O: to the other one. R R comes
	// first in one: the row's project's answer forgets the released key, and
	// its next heartbeat no longer carries the session before the other's
	// has moved the row.
	for _, seq := range []string{"RROORROO", "ORROORRO", "OORROORR", "RORORORO", "PPPPPPPP"} { // P: every project at once, as the daemon beats
		t.Run(seq, func(t *testing.T) {
			quietPresence(t)
			ctx := context.Background()
			tn := tailnettest.New()
			a, _ := shortLeaseAuthority(t, tn)
			mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "second"}))(t)
			mustf(a.owner.ProjectTeam(ctx, "second"))(t)
			r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
			inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "second", As: "bea", Role: "member"}))(t)
			mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
			mustf(r.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "side"}))(t)
			ss := register(t, r, "side", "move-1")
			slugOf := map[string]string{}
			for _, slug := range []string{"harbor", "second"} {
				slugOf[mustf(a.owner.GetProject(ctx, slug))(t).ID] = slug
			}
			mustf(orchclient.Lease(ctx, at(r.owner, "harbor", ss.Address), orchapi.ClaimRequest{Key: "lock:harbor"}))(t)
			mustf(orchclient.Lease(ctx, at(r.owner, "second", ss.Address), orchapi.ClaimRequest{Key: "lock:second"}))(t)
			ts := teamService(t, r)
			for i := 0; i < 3; i++ {
				ts.BeatProject(ctx, "harbor")
				ts.BeatProject(ctx, "second")
			}
			eventually(t, 10*time.Second, func() error { // both bound (the ticker, 1 s here)
				for _, l := range mustf(orchclient.Leases(ctx, a.owner, orchapi.LeaseQuery{}))(t) {
					if !l.Bound {
						return fmt.Errorf("not bound yet: %+v", l)
					}
				}
				return nil
			})
			row := slugOf[mustf(orchclient.Session(ctx, a.owner, ss.Address))(t).Project]
			other := map[string]string{"harbor": "second", "second": "harbor"}[row]
			if row == "" {
				t.Fatal("no presence row")
			}
			mustf(orchclient.ReleaseLease(ctx, at(r.owner, row, ss.Address), "lock:"+row, orchapi.ReleaseRequest{}))(t)
			for i, c := range seq {
				to := map[rune]string{'R': row, 'O': other, 'P': "both"}[c]
				if c == 'P' {
					heartbeat(t, r)
				} else {
					ts.BeatProject(ctx, to)
				}
				ls := mustf(orchclient.Leases(ctx, a.owner, orchapi.LeaseQuery{}))(t)
				if len(ls) != 1 || ls[0].Key != "lock:"+other || ls[0].Holder != ss.Address || !ls[0].Bound {
					t.Fatalf("after heartbeat %d (%s): %+v", i+1, to, ls)
				}
				if got := mustf(orchclient.Session(ctx, a.owner, ss.Address))(t); got.State == orchapi.StateEnded {
					t.Fatalf("after heartbeat %d the session ended at the authority: %+v", i+1, got)
				}
			}
			if got := slugOf[mustf(orchclient.Session(ctx, a.owner, ss.Address))(t).Project]; got != other {
				t.Fatalf("the row did not move to %s: %s", other, got)
			}
			if got := mustf(orchclient.Session(ctx, r.owner, ss.Address))(t); got.State != orchapi.StateRunning {
				t.Fatalf("the session on the member: %+v", got)
			}
			var ended int
			_ = a.owner.Events(ctx, api.EventQuery{Types: "dev.zm.session.ended", Since: "0", NoLive: true}, func(e api.Event) error {
				if e.Subject == ss.Address {
					ended++
				}
				return nil
			})
			if ended != 0 {
				t.Fatalf("%d ended events at the authority", ended)
			}
		})
	}
}

// A machine leaving one project (zm leave, or its token revoked) does not
// end its session there when the session holds leases in another project of
// the same authority: ending would release its bound leases in every
// project. Its row moves to the other project, only the left project's bound
// leases go, and the other one stays held through later heartbeats.
func TestLeaveKeepsLeasesInTheOtherProject(t *testing.T) {
	for _, how := range []string{"leave", "revoke"} {
		t.Run(how, func(t *testing.T) {
			quietPresence(t)
			ctx := context.Background()
			tn := tailnettest.New()
			a, _ := shortLeaseAuthority(t, tn)
			mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "second"}))(t)
			mustf(a.owner.ProjectTeam(ctx, "second"))(t)
			r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
			inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "second", As: "bea", Role: "member"}))(t)
			mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
			ss := register(t, r, "harbor", "leave-1")
			heartbeat(t, r)
			mustf(orchclient.Lease(ctx, at(r.owner, "harbor", ss.Address), orchapi.ClaimRequest{Key: "lock:harbor"}))(t)
			mustf(orchclient.Lease(ctx, at(r.owner, "second", ss.Address), orchapi.ClaimRequest{Key: "lock:second"}))(t)
			eventually(t, 10*time.Second, func() error {
				heartbeat(t, r)
				ls := mustf(orchclient.Leases(ctx, a.owner, orchapi.LeaseQuery{}))(t)
				for _, l := range ls {
					if !l.Bound {
						return fmt.Errorf("not bound yet: %+v", l)
					}
				}
				if len(ls) != 2 {
					return fmt.Errorf("leases %+v", ls)
				}
				return nil
			})
			second := mustf(a.owner.GetProject(ctx, "second"))(t).ID
			switch how {
			case "leave":
				mustf(r.owner.Leave(ctx, "harbor"))(t)
			case "revoke":
				revoked := false
				for _, tk := range mustf(a.owner.Tokens(ctx))(t) {
					if tk.Project == "harbor" && tk.Machine == "bea-mac" && tk.Revoked == nil {
						mustf(a.owner.RevokeToken(ctx, tk.ID))(t)
						revoked = true
					}
				}
				if !revoked {
					t.Fatal("no node-bound token to revoke")
				}
			}
			check := func(when string) {
				t.Helper()
				ls := mustf(orchclient.Leases(ctx, a.owner, orchapi.LeaseQuery{}))(t)
				if len(ls) != 1 || ls[0].Key != "lock:second" || !ls[0].Bound || ls[0].Holder != ss.Address {
					t.Fatalf("%s: %+v", when, ls)
				}
				if got := mustf(orchclient.Session(ctx, a.owner, ss.Address))(t); got.State == orchapi.StateEnded || got.Project != second {
					t.Fatalf("%s: the session at the authority: %+v", when, got)
				}
			}
			// Nothing of the left project goes with the row: its key is not in
			// the row or in the other project's stored events.
			leak := func(when string) {
				t.Helper()
				js, _ := json.Marshal(mustf(orchclient.Session(ctx, a.owner, ss.Address))(t))
				if strings.Contains(string(js), "lock:harbor") {
					t.Fatalf("%s: the moved row names the left project's key: %s", when, js)
				}
				_ = a.owner.Events(ctx, api.EventQuery{Project: "second", Since: "0", NoLive: true}, func(e api.Event) error {
					if js, _ := json.Marshal(e); strings.Contains(string(js), "lock:harbor") {
						t.Fatalf("%s: an event in the other project names the left project's key: %s", when, js)
					}
					return nil
				})
			}
			check("right after " + how)
			leak("right after " + how)
			for i := 0; i < 8; i++ {
				heartbeat(t, r)
				time.Sleep(300 * time.Millisecond)
				check(fmt.Sprintf("heartbeat %d", i+1))
			}
			leak("after 8 heartbeats")
			if got := mustf(orchclient.Session(ctx, r.owner, ss.Address))(t); got.State == orchapi.StateNeedsHuman {
				t.Fatalf("the member reported a lost lease: %+v", got)
			}
		})
	}
}

// A Codex subagent thread's turn ends with SubagentStop, run as zm install
// codex installs it: its session goes idle, its claim stays, and its next
// turn finds it held.
func TestCodexSubagentClaimSurvivesItsTurnEnd(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a, _ := shortLeaseAuthority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	hook := func(event, payload string) {
		t.Helper()
		if _, err := integrations.RunHook(ctx, func(as, _ string) integrations.Daemon { return orchclient.Hooks(at(r.owner, "harbor", as)) },
			integrations.HookInput{Agent: "codex", Event: event, Payload: []byte(payload), Getenv: func(string) string { return "" }}); err != nil {
			t.Fatalf("hook %s: %v", event, err)
		}
	}
	const sub = `"session_id":"codex-root-1","agent_id":"codex-sub-1"`
	hook(integrations.HookTool, `{`+sub+`,"tool_name":"shell","tool_input":{}}`) // turn 1
	var addr string
	for _, ss := range mustf(orchclient.Sessions(ctx, r.owner, orchapi.SessionQuery{}))(t) {
		if ss.RuntimeID == "codex-sub-1" {
			addr = ss.Address
		}
	}
	if addr == "" {
		t.Fatal("the subagent's hook registered no session")
	}
	heartbeat(t, r)
	mustf(orchclient.Lease(ctx, at(r.owner, "harbor", addr), orchapi.ClaimRequest{Key: "lock:subagent"}))(t)
	heartbeat(t, r)
	// Turn 1 ends: SubagentStop as zm install codex installs it.
	hook(integrations.CodexHookArg("SubagentStop"), `{`+sub+`,"hook_event_name":"SubagentStop","agent_type":"default"}`)
	if got := mustf(orchclient.Session(ctx, r.owner, addr))(t); got.State != orchapi.StateIdle {
		t.Fatalf("after SubagentStop: %+v", got)
	}
	heartbeat(t, r)
	time.Sleep(1500 * time.Millisecond)                                          // a lease tick or so: idle is not ended
	hook(integrations.HookTool, `{`+sub+`,"tool_name":"shell","tool_input":{}}`) // turn 2
	heartbeat(t, r)
	ls := mustf(orchclient.Leases(ctx, a.owner, orchapi.LeaseQuery{}))(t)
	if len(ls) != 1 || ls[0].Holder != addr {
		t.Fatalf("turn 2 finds: %+v", ls)
	}
	if got := mustf(orchclient.Session(ctx, r.owner, addr))(t); got.State != orchapi.StateRunning {
		t.Fatalf("turn 2: %+v", got)
	}
}

// want0 is project slug's id at a.
func want0(a *daemon, t *testing.T, slug string) string {
	t.Helper()
	return mustf(a.owner.GetProject(context.Background(), slug))(t).ID
}
