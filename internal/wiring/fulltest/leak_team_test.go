package fulltest

// The golden leak test extended to teams (SPEC 17.7, rounds B1 and B2):
// invites seeded in every hidden place, proxied calls, cross-machine mail
// and presence. The fixture's projects become team projects on an authority
// with a node on the loopback tailnet (tailnettest). A member replica
// (zz-bea-mac) reports presence of sessions in a hidden area and at
// project level, and mail goes to them, to the guest's replica group
// mailbox in a hidden area, and from a hidden replica session to the
// guest's session. The guest then walks every route at the authority (four
// transports; over the tunnel with a key-bound token), and a second daemon, a guest replica of harbor/solver, walks
// every route on its own socket as its owner, so every proxied work, lease,
// mail, native issue and session call reaches the authority with the
// replica's node-bound token. Nothing outside solver may come back (in mail,
// presence, events, errors or counts), and hidden ids answer like missing
// ones, end to end.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/sengirunners/mezze/internal/wiring"
	. "github.com/sengirunners/potluck/kit/server"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// heartbeatNow makes dm's replica send its presence now.
func heartbeatNow(t *testing.T, dm *Daemon) {
	t.Helper()
	for _, m := range dm.Modules() {
		if s, ok := m.(*team.Service); ok {
			s.Heartbeat(context.Background())
			return
		}
	}
	t.Fatal("no team module")
}

// newReplica is a daemon for machine (human h) with a node on tn, joined to
// the fixture's authority by an invite for proj/area as/role.
func newReplica(t *testing.T, f *leakFixture, tn *tailnettest.Net, machine, h, nodeID string, inv api.InviteRequest, more ...func(*config.Config)) (*Daemon, *client.Client, api.JoinResponse, string) {
	t.Helper()
	ctx := context.Background()
	cfg, err := config.LoadFrom(shortHome(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = h, machine
	for _, m := range more {
		m(cfg)
	}
	rdm, err := New(ctx, cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rdm.Close)
	freezeProbe(rdm)
	useLoopNode(t, rdm, tn, tailnettest.Identity{NodeID: nodeID, NodeKey: "nodekey:" + machine, Login: h + "@example.com", Host: machine})
	r, err := client.WithHandler(f.dm.SocketHandler()).Invite(ctx, inv)
	if err != nil {
		t.Fatal(err)
	}
	ro := client.WithHandler(rdm.SocketHandler())
	j, err := ro.Join(ctx, api.JoinRequest{Code: r.Code})
	if err != nil || j.Remote == nil {
		t.Fatalf("join %s: %+v %v", machine, j, err)
	}
	return rdm, ro, j, r.Code
}

// as is c in project (slug[/area]) acting as address as.
func as(c *client.Client, project, addr string) *client.Client {
	x := *c
	x.Project, x.ProjectFrom, x.As = project, "flag", addr
	return &x
}

func useLoopNode(t *testing.T, dm *Daemon, tn *tailnettest.Net, id tailnettest.Identity) {
	t.Helper()
	for _, m := range dm.Modules() {
		if s, ok := m.(*tailnet.Service); ok {
			s.NewNode = tn.NodeFunc(id)
		}
	}
	st, err := client.WithHandler(dm.SocketHandler()).TailnetUp(context.Background(), api.TailnetUpRequest{})
	if err != nil || !st.Running {
		t.Fatalf("tailnet up: %+v %v", st, err)
	}
}

func TestGoldenLeakTeams(t *testing.T) {
	ctx := context.Background()
	f := newLeakFixture(t)
	tn := tailnettest.New()
	useLoopNode(t, f.dm, tn, tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"})
	owner := client.WithHandler(f.dm.SocketHandler())
	for _, p := range []string{"harbor", "zz-side"} {
		if _, err := owner.ProjectTeam(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	// Invites in every hidden place: the guest never learns their ids,
	// names or codes.
	for _, inv := range []api.InviteRequest{
		{Project: "zz-side", As: "zz-inv-side", Role: "member"},
		{Project: "harbor", Area: "zz-billing", As: "zz-inv-billing", Role: "guest"},
		{Project: "harbor", As: "zz-inv-project", Role: "member"},
		{Project: "harbor", Area: "solver", As: "zz-inv-solver", Role: "guest"},
	} {
		r, err := owner.Invite(ctx, inv)
		if err != nil {
			t.Fatal(err)
		}
		f.hidden = append(f.hidden, r.Invite.ID)
		f.forbid(r.Invite.ID, "an invite's id")
		f.forbid(inv.As, "an invite's identity")
		f.forbid(r.Code[strings.LastIndex(r.Code, ":")+1:], "a join code's secret")
	}

	// A member replica whose sessions are where the guest cannot see:
	// presence in a hidden area and at project level, and mail to them.
	// It runs a task driver, so the sandbox task below has a machine.
	cdm, co, _, ccode := newReplica(t, f, tn, "zz-bea-mac", "zz-bea", "nZZBEA", api.InviteRequest{Project: "harbor", As: "zz-bea2", Role: "member"},
		func(c *config.Config) {
			c.File.RemoteTasks, c.File.SandboxTaskCmd = true, "/nonexistent/HIDDEN-sandbox-driver"
		})
	f.forbid(ccode[strings.LastIndex(ccode, ":")+1:], "a member's join code")
	f.forbid("zz-bea2", "a member's identity")
	hiddenPresence := map[string]string{}
	for _, pl := range []struct{ proj, rid string }{{"harbor/zz-billing", "zzpresb1-uuid"}, {"harbor", "zzpresp1-uuid"}} {
		ss, err := orchclient.Register(ctx, as(co, pl.proj, ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: pl.rid, Title: "HIDDEN-presence"})
		if err != nil || ss.Project != f.harbor {
			t.Fatalf("member replica session: %+v %v", ss, err)
		}
		hiddenPresence[pl.proj] = ss.Address
		f.hidden = append(f.hidden, ss.Address, ss.ID)
		f.forbid(ss.Address, "a hidden replica session's address")
		f.forbid(ss.ID, "a hidden replica session's id")
	}
	// And one in no project (SPEC 9 "Across projects"): its heartbeat lists
	// it as elsewhere, and an owner mails it in harbor at project level.
	nop, err := orchclient.Register(ctx, as(co, "", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "zznoproj-uuid", Title: "HIDDEN-elsewhere"})
	if err != nil || nop.Project != api.ProjectNone {
		t.Fatalf("member replica session in none: %+v %v", nop, err)
	}
	f.hidden = append(f.hidden, nop.Address, nop.ID)
	f.forbid(nop.Address, "a hidden no-project replica session's address")
	f.forbid(nop.ID, "a hidden no-project replica session's id")
	heartbeatNow(t, cdm)
	if m, err := orchclient.Send(ctx, as(owner, "harbor", ""), orchapi.SendMailRequest{To: nop.Address, Body: "HIDDEN-to-elsewhere"}); err != nil {
		t.Fatalf("mail to a no-project replica session: %v", err)
	} else {
		f.hidden = append(f.hidden, m.ID)
		f.forbid(m.ID, "mail to a hidden no-project replica session")
	}
	for proj, addr := range hiddenPresence {
		area := strings.TrimPrefix(strings.TrimPrefix(proj, "harbor"), "/")
		m, err := orchclient.Send(ctx, as(owner, proj, ""), orchapi.SendMailRequest{To: addr, Body: "HIDDEN-to-presence", Area: area})
		if err != nil {
			t.Fatalf("mail to a replica session: %v", err)
		}
		f.hidden = append(f.hidden, m.ID)
		f.forbid(m.ID, "mail to a hidden replica session")
	}

	// An automation record whose home is the member machine, written by it
	// (its node-bound token at the authority), in a hidden area: the walk
	// sends its id through the guest's replica too.
	if err := as(co, "harbor", "").Do(ctx, http.MethodPut, "/v0/automation-homes/zz-bea-mac?project=harbor",
		orchapi.AutomationHomeClaim{Install: "z3-install-bea", Store: "zm", Path: "/nonexistent/HIDDEN-bea-userdata"}, nil); err != nil {
		t.Fatalf("member claims its home: %v", err)
	}
	var mrec orchapi.Automation
	if err := co.Do(ctx, http.MethodPost, "/v0/automations", orchapi.AutomationCreate{Project: "harbor", Area: "zz-billing", Home: "zz-bea-mac",
		Install: "z3-install-bea", Name: "HIDDEN-member automation", Body: "HIDDEN-member prompt", Fields: []byte(`{"on":{"manual":true}}`)}, &mrec); err != nil {
		t.Fatalf("member writes its record: %v", err)
	}
	f.hidden = append(f.hidden, mrec.ID)
	f.forbid(mrec.ID, "a hidden member-homed automation record's id")

	// A guest replica of harbor/solver.
	rdm, ro, j, gcode := newReplica(t, f, tn, "vida-mac", "vida", "nVIDA", api.InviteRequest{Project: "harbor", Area: "solver", As: "vida", Role: "guest"})
	f.forbid(gcode[strings.LastIndex(gcode, ":")+1:], "the replica's own join code")
	// Its session in solver (the guest's area by default), known by presence.
	gs, err := orchclient.Register(ctx, as(ro, "harbor", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "vsolver1-uuid", Title: "VISIBLE-guest-session"})
	if err != nil || gs.Project != j.Remote.Project.ID || gs.Area != "solver" {
		t.Fatalf("guest session %+v %v", gs, err)
	}
	f.visible[gs.Address], f.visible[gs.ID] = true, true
	heartbeatNow(t, rdm)
	// Group mail to the guest's machine: one in solver, one in a hidden area
	// (an owner may mail any area's group mailbox; the guest reads only its
	// own area's).
	vis, err := orchclient.Send(ctx, as(owner, "harbor/solver", ""), orchapi.SendMailRequest{To: "machine:vida-mac", Body: "VISIBLE group", Area: "solver"})
	if err != nil {
		t.Fatal(err)
	}
	f.visible[vis.ID] = true
	hg, err := orchclient.Send(ctx, as(owner, "harbor/zz-billing", ""), orchapi.SendMailRequest{To: "machine:vida-mac", Body: "HIDDEN-group", Area: "zz-billing"})
	if err != nil {
		t.Fatal(err)
	}
	f.hidden = append(f.hidden, hg.ID)
	f.forbid(hg.ID, "group mail in a hidden area")
	// A hidden replica session mails the guest's session in solver: the
	// guest sees "someone".
	fromHidden, err := orchclient.Send(ctx, as(co, "harbor/solver", hiddenPresence["harbor/zz-billing"]), orchapi.SendMailRequest{To: gs.Address, Body: "VISIBLE from a hidden replica session", Area: "solver"})
	if err != nil {
		t.Fatalf("mail from a hidden replica session: %v", err)
	}
	f.visible[fromHidden.ID] = true
	// A guest (area-only reader) cannot probe which machines joined.
	gm := as(ro, "harbor/solver", gs.Address)
	_, e1 := orchclient.Send(ctx, gm, orchapi.SendMailRequest{To: "machine:zz-bea-mac", Body: "x"})
	_, e2 := orchclient.Send(ctx, gm, orchapi.SendMailRequest{To: "machine:nope-machine", Body: "x"})
	if !api.IsCode(e1, api.CodeNotFound) || strings.ReplaceAll(e1.Error(), "zz-bea-mac", "X") != strings.ReplaceAll(e2.Error(), "nope-machine", "X") {
		t.Fatalf("joined machine %v, missing machine %v", e1, e2)
	}
	_, e1 = orchclient.Send(ctx, gm, orchapi.SendMailRequest{To: hiddenPresence["harbor/zz-billing"], Body: "x"})
	_, e2 = orchclient.Send(ctx, gm, orchapi.SendMailRequest{To: "zz-bea-mac/nopenope", Body: "x"})
	if !api.IsCode(e1, api.CodeNotFound) || normalize(leakResp{body: e1.Error()}, hiddenPresence["harbor/zz-billing"]) != normalize(leakResp{body: e2.Error()}, "zz-bea-mac/nopenope") {
		t.Fatalf("hidden replica session %v, missing %v", e1, e2)
	}
	_, e1 = orchclient.Send(ctx, gm, orchapi.SendMailRequest{To: nop.Address, Body: "x"})
	if !api.IsCode(e1, api.CodeNotFound) || normalize(leakResp{body: e1.Error()}, nop.Address) != normalize(leakResp{body: e2.Error()}, "zz-bea-mac/nopenope") {
		t.Fatalf("hidden no-project replica session %v, missing %v", e1, e2)
	}

	// A sandbox task in the waiting line (SPEC 8 "Where it runs") for the
	// member replica's machine, started by an owner's session at project level: task responses carry "sandbox",
	// which the guest never sees (the team task routes are owners' only).
	ps, err := orchclient.Register(ctx, as(owner, "harbor", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "zzsandboxparent-uuid", Title: "HIDDEN-sandbox-parent"})
	if err != nil {
		t.Fatal(err)
	}
	heartbeatNow(t, cdm) // the member replica's machine reports its task driver
	f.forbid(ps.Address, "a hidden session's address")
	f.forbid(ps.ID, "a hidden session's id")
	seedStep(t, f, "a sandbox task", func() (obj, error) {
		st, err := orchclient.TaskStart(ctx, as(owner, "harbor", ps.Address), orchapi.TaskStartRequest{Agent: "codex", Model: "gpt-5.5",
			Prompt: "HIDDEN-sandbox-task", Machine: "auto", Sandbox: &orchapi.TaskSandbox{Size: "c2m8"}})
		if err != nil {
			return nil, err
		}
		f.forbid(st.ID, "a sandbox task's id")
		var l struct {
			Tasks []orchapi.Task `json:"tasks"`
		}
		if err := owner.Do(ctx, http.MethodGet, "/v0/team/tasks?project=harbor", nil, &l); err != nil {
			return nil, err
		}
		for _, x := range l.Tasks {
			if x.ID == st.ID && x.Sandbox != nil && x.Sandbox.Size == "c2m8" {
				return obj{}, nil
			}
		}
		return nil, fmt.Errorf("the owner's team tasks lack the sandbox task %s: %+v", st.ID, l.Tasks)
	})
	f.forbid(`"sandbox":{`, "a sandbox task's sandbox (the project's owners only)")

	// The tunnel (SPEC 17.11): the guest also redeems over it, so the walk
	// runs there with a key-bound guest token, signed by its key.
	f.tunnelTok = tunnelGuestToken(t, f, api.InviteRequest{Project: "harbor", Area: "solver", As: "vida", Role: "guest", Via: api.ViaTunnel}, "vida-key")
	if r := f.call("GET", "/v0/projects", "", viaTunnel); r.status != 200 || !strings.Contains(r.body, `"slug":"harbor"`) || !strings.Contains(r.body, `"role":"guest"`) {
		t.Fatalf("tunnel guest projects: %d %s", r.status, r.body)
	}

	routes := f.dm.Router.Routes()
	for _, tr := range transports {
		f.walk(t, "team "+tr.String(), routes, func(method, target, project string) leakResp { return f.call(method, target, project, tr) }, nil, nil)
	}

	// Counts: the guest session's notice (from the presence cache) counts
	// exactly what it may read: the solver group mail and the mail from the
	// hidden session, never the hidden-area group mail.
	heartbeatNow(t, rdm)
	hk, err := orchclient.Hook(ctx, ro, orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "vsolver1-uuid"}, Notice: true})
	if err != nil || hk.Notice == nil {
		t.Fatalf("guest hook: %+v %v", hk, err)
	}
	in, err := orchclient.Inbox(ctx, as(ro, "", gs.Address), orchapi.InboxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if hk.Notice.Unread != 2 || len(in) != 2 || !strings.Contains(hk.Notice.Line, "someone") {
		t.Fatalf("guest session notice %+v, inbox %+v", hk.Notice, in)
	}
	f.checkNoLeak(t, "guest session notice", leakResp{body: hk.Notice.Line})
	again, err := orchclient.Hook(ctx, ro, orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "vsolver1-uuid"}, Notice: true})
	if err != nil || again.Notice == nil || again.Notice.Unread != 2 || again.Notice.Line != "" {
		t.Fatalf("replica hook repeated mail events: %+v %v", again.Notice, err)
	}
	for _, m := range in {
		if m.Area != "solver" {
			t.Fatalf("guest inbox: %+v", m)
		}
	}

	// Not vacuous: through the replica the guest sees its solver item.
	wl, err := orchclient.WorkList(ctx, ro, orchapi.WorkQuery{})
	if err != nil || len(wl.Items) == 0 {
		t.Fatalf("replica list: %+v %v", wl, err)
	}
	for _, it := range wl.Items {
		if it.Area != "solver" {
			t.Fatalf("replica sees %+v", it)
		}
	}
	// Not vacuous: native issues through the replica are its solver issue,
	// and never the hidden billing one it depends on.
	il, err := orchclient.Issues(ctx, as(ro, "harbor", ""), orchapi.IssueQuery{Project: "harbor"})
	if err != nil || len(il.Issues) == 0 {
		t.Fatalf("replica issues: %+v %v", il, err)
	}
	for _, is := range il.Issues {
		if is.Area != "solver" {
			t.Fatalf("replica sees issue %+v", is)
		}
	}
	replicaCall := ownerCall(rdm)
	// Leave out what would take the replica off the tailnet or stop it.
	// GET /v0/peers is what this machine's own tailnet node knows (peers'
	// owner logins, as tailscale status shows them), not zm data.
	skip := func(p string) bool {
		return p == "POST /v0/tailnet/down" || p == "POST /v0/daemon/stop" || p == "GET /v0/peers"
	}
	// Hidden must equal missing on every proxied route (work, leases, mail,
	// native issues, automation records, reading sessions) and on the session routes (a hidden
	// replica session's address must not answer differently from a missing
	// one); the rest answer from the replica's own data, which its owner sees
	// whole, so only the leak check applies there.
	local := func(p string) bool {
		_, path, _ := strings.Cut(p, " ")
		for _, pre := range []string{"/v0/work", "/v0/leases", "/v0/mail", "/v0/issues", "/v0/sessions", "/v0/automations", "/v0/automation-homes"} {
			if strings.HasPrefix(path, pre) {
				return false
			}
		}
		return true
	}
	f.walk(t, "replica", rdm.Router.Routes(), replicaCall, skip, local)

	// The same guest's replica over the tunnel (no tailnet on it): every
	// proxied call crosses the signed door with its key-bound token.
	tdm, to := tunnelReplica(t, f, "vida-tun", "vida", api.InviteRequest{Project: "harbor", Area: "solver", As: "vida", Role: "guest", Via: api.ViaTunnel})
	f.walk(t, "tunnel replica", tdm.Router.Routes(), ownerCall(tdm), skip, local)
	twl, err := orchclient.WorkList(ctx, to, orchapi.WorkQuery{})
	if err != nil || len(twl.Items) == 0 {
		t.Fatalf("tunnel replica list: %+v %v", twl, err)
	}
	for _, it := range twl.Items {
		if it.Area != "solver" {
			t.Fatalf("tunnel replica sees %+v", it)
		}
	}
	if rs, err := to.Remotes(ctx); err != nil || len(rs) != 1 || rs[0].Via != api.ViaTunnel || len(rs[0].Areas) != 1 || rs[0].Areas[0] != "solver" {
		t.Fatalf("tunnel replica's remote: %+v %v", rs, err)
	}
	// The replica's events (its owner's machine log carries its own team
	// sessions' events) never name a hidden replica session.
	f.checkNoLeak(t, "replica events", replicaCall(http.MethodGet, "/v0/events?since=0&live=0", ""))
	// Every hidden id through each proxied item route answered like a
	// missing one, above; and the replica's own records say only solver.
	rs, err := ro.Remotes(ctx)
	if err != nil || len(rs) != 1 || len(rs[0].Areas) != 1 || rs[0].Areas[0] != "solver" {
		t.Fatalf("replica's remote: %+v %v", rs, err)
	}
}

// ownerCall is a request as dm's owner on its socket.
func ownerCall(dm *Daemon) func(method, target, project string) leakResp {
	return func(method, target, project string) leakResp {
		body := "{}"
		if method != http.MethodPost && method != http.MethodPatch && method != http.MethodDelete {
			body = ""
		}
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set(api.HeaderProtocol, api.Protocol)
		if project != "" {
			req.Header.Set(api.HeaderProject, project)
		}
		req = req.WithContext(core.WithLocal(req.Context()))
		rec := httptest.NewRecorder()
		dm.Handler.ServeHTTP(rec, req)
		return leakResp{status: rec.Code, body: rec.Body.String()}
	}
}

// tunnelGuestToken redeems a tunnel invite with the fixture's guest key
// (a proof, never the code) and returns the key-bound token.
func tunnelGuestToken(t *testing.T, f *leakFixture, inv api.InviteRequest, machine string) string {
	t.Helper()
	ctx := context.Background()
	r, err := client.WithHandler(f.dm.SocketHandler()).Invite(ctx, inv)
	if err != nil {
		t.Fatal(err)
	}
	f.forbid(r.Code[strings.LastIndex(r.Code, ":")+1:], "a tunnel join code's secret")
	code, err := project.ParseJoinCode(r.Code)
	if err != nil {
		t.Fatal(err)
	}
	kid, err := f.tunKey.KeyID()
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(api.RedeemRequest{Proof: project.RedeemProof(code.Secret, code.Fingerprint, kid, machine), Machine: machine})
	req, _ := http.NewRequest("POST", f.tunURL+"/v0/invites/redeem", bytes.NewReader(body))
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.tunHC.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var red api.RedeemResponse
	if err := json.NewDecoder(resp.Body).Decode(&red); err != nil || resp.StatusCode != 200 || red.Token == "" {
		t.Fatalf("tunnel redeem: %d %+v %v", resp.StatusCode, red, err)
	}
	return red.Token
}

// tunnelReplica is a daemon with no tailnet, joined to the fixture's
// authority over its tunnel door.
func tunnelReplica(t *testing.T, f *leakFixture, machine, h string, inv api.InviteRequest) (*Daemon, *client.Client) {
	t.Helper()
	ctx := context.Background()
	cfg, err := config.LoadFrom(shortHome(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = h, machine
	dm, err := New(ctx, cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	freezeProbe(dm)
	for _, m := range dm.Modules() {
		if s, ok := m.(*tailnet.Service); ok {
			// It has no tailnet; a walk of tailnet up must still never start
			// a real tsnet node.
			s.NewNode = tailnettest.New().NodeFunc(tailnettest.Identity{NodeID: "nTUN-" + machine, Host: machine})
		}
	}
	r, err := client.WithHandler(f.dm.SocketHandler()).Invite(ctx, inv)
	if err != nil {
		t.Fatal(err)
	}
	f.forbid(r.Code[strings.LastIndex(r.Code, ":")+1:], "a tunnel join code's secret")
	c := client.WithHandler(dm.SocketHandler())
	j, err := c.Join(ctx, api.JoinRequest{Code: r.Code})
	if err != nil || j.Remote == nil || j.Remote.Via != api.ViaTunnel {
		t.Fatalf("tunnel join %s: %+v %v", machine, j, err)
	}
	return dm, c
}
