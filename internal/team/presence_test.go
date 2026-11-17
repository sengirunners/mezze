package team_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// heartbeat makes d's replica send its presence now.
func heartbeat(t *testing.T, d *daemon) {
	t.Helper()
	for _, m := range d.dm.Modules() {
		if s, ok := m.(*team.Service); ok {
			s.Heartbeat(context.Background())
			return
		}
	}
	t.Fatal("no team module")
}

// sessionModule is d's session module.
func sessionModule(t *testing.T, d *daemon) *session.Service {
	t.Helper()
	for _, m := range d.dm.Modules() {
		if s, ok := m.(*session.Service); ok {
			return s
		}
	}
	t.Fatal("no session module")
	return nil
}

// quietPresence stops the background heartbeat for the test (it calls
// heartbeat itself), so what each heartbeat carries is deterministic.
func quietPresence(t *testing.T) {
	old, oldShared := teamlink.BeatEvery, teamlink.FastBeatEvery
	teamlink.BeatEvery, teamlink.FastBeatEvery = time.Hour, time.Hour // shared sessions' faster beat too: no background beat races a test's
	t.Cleanup(func() { teamlink.BeatEvery, teamlink.FastBeatEvery = old, oldShared })
}

// register registers a hook session on d in project (slug[/area]).
func register(t *testing.T, d *daemon, proj, rid string) orchapi.Session {
	t.Helper()
	ss := mustf(orchclient.Register(context.Background(), at(d.owner, proj, ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: rid, Title: rid}))(t)
	return ss
}

func findSession(ss []orchapi.Session, addr string) *orchapi.Session {
	for i := range ss {
		if ss[i].Address == addr {
			return &ss[i]
		}
	}
	return nil
}

func hasMail(ms []orchapi.Mail, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

// Mail to a team project's session, item holder, group and human is routed
// by the authority; replicas fetch it through the proxy, and the notice on
// the replica comes from the presence cache (no tailnet call).
func TestCrossMachineMailAndPresence(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	pid := j.Remote.Project.ID

	// A session on the replica in the team project is local to it, with
	// the authority's project id.
	cs := register(t, r, "harbor/solver", "bea-sess-1")
	if cs.Project != pid || cs.Area != "solver" || cs.Machine != "bea-mac" {
		t.Fatalf("replica session %+v", cs)
	}
	// Before presence the authority cannot route to it.
	ms := register(t, a, "harbor", "mini-sess-1")
	am := at(a.owner, "harbor", ms.Address)
	if _, err := orchclient.Send(ctx, am, orchapi.SendMailRequest{To: cs.Address, Body: "too early"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("mail before presence: %v", err)
	}
	heartbeat(t, r)
	// The authority sees it (presence), with its last heartbeat.
	all := mustf(orchclient.Sessions(ctx, at(a.owner, "", ""), orchapi.SessionQuery{}))(t)
	if p := findSession(all, cs.Address); p == nil || p.Seen == nil || p.State != cs.State || p.Area != "solver" || p.Project != pid {
		t.Fatalf("authority sessions %+v", all)
	}

	// Session to session across machines.
	m1 := mustf(orchclient.Send(ctx, am, orchapi.SendMailRequest{To: cs.Address, Body: "hello bea"}))(t)
	if m1.Recipient != cs.Address {
		t.Fatalf("routed %+v", m1)
	}
	rs := at(r.owner, "", cs.Address)
	eventually(t, 2*time.Second, func() error {
		if in := mustf(orchclient.Inbox(ctx, rs, orchapi.InboxQuery{}))(t); !hasMail(in, m1.ID) {
			return fmt.Errorf("replica inbox %+v", in)
		}
		return nil
	})
	// The notice on the replica is the presence cache: after a heartbeat it
	// counts the team mail, through the hook route too.
	heartbeat(t, r)
	if n := mustf(orchclient.Notice(ctx, rs))(t); n.Unread != 1 || !strings.Contains(n.Line, "harbor") || !strings.Contains(n.Line, ms.Address) {
		t.Fatalf("replica notice %+v", n)
	}
	hk := mustf(orchclient.Hook(ctx, r.owner, orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "bea-sess-1", State: orchapi.StateRunning}, Notice: true}))(t)
	if hk.Notice == nil || hk.Notice.Unread != 1 {
		t.Fatalf("hook notice %+v", hk.Notice)
	}
	// Reply crosses back; the original is taken.
	rep := mustf(orchclient.Reply(ctx, rs, m1.ID, orchapi.ReplyRequest{Body: "hi mini"}))(t)
	if rep.Recipient != ms.Address || rep.From != cs.Address {
		t.Fatalf("reply %+v", rep)
	}
	if in := mustf(orchclient.Inbox(ctx, am, orchapi.InboxQuery{}))(t); !hasMail(in, rep.ID) {
		t.Fatalf("authority inbox %+v", in)
	}
	heartbeat(t, r)
	eventually(t, 2*time.Second, func() error {
		if n := mustf(orchclient.Notice(ctx, rs))(t); n.Unread != 0 {
			return fmt.Errorf("notice after the reply took it: %+v", n)
		}
		return nil
	})

	// Mail to a work item reaches its holder on the replica.
	it := mustf(orchclient.CreateWork(ctx, at(r.owner, "harbor", cs.Address), orchapi.CreateWorkRequest{Title: "held on bea", Area: "solver"}))(t)
	mustf(orchclient.Claim(ctx, rs, it.ID, orchapi.ClaimRequest{}))(t)
	m2 := mustf(orchclient.Send(ctx, am, orchapi.SendMailRequest{To: it.ID, Body: "about your item"}))(t)
	if m2.Recipient != cs.Address || !strings.HasPrefix(m2.Via, "holder of") {
		t.Fatalf("item mail %+v", m2)
	}
	// zm who at the authority and on the replica names the holder with its presence.
	for _, c := range []*client.Client{at(a.owner, "harbor", ""), at(r.owner, "", "")} {
		h := mustf(orchclient.Holder(ctx, c, it.ID))(t)
		if h.Holder != cs.Address || h.Session == nil || h.Session.Seen == nil || h.Session.Machine != "bea-mac" {
			t.Fatalf("holder %+v", h)
		}
	}

	// The replica's group mailbox: every session of the project there sees
	// it; the first ack takes it.
	cs2 := register(t, r, "harbor/solver", "bea-sess-2")
	heartbeat(t, r)
	m3 := mustf(orchclient.Send(ctx, am, orchapi.SendMailRequest{To: "machine:bea-mac", Body: "anyone on bea?"}))(t)
	if m3.Recipient != "machine:bea-mac" || m3.Via != "group" {
		t.Fatalf("group mail %+v", m3)
	}
	for _, s := range []string{cs.Address, cs2.Address} {
		eventually(t, 2*time.Second, func() error {
			if in := mustf(orchclient.Inbox(ctx, at(r.owner, "", s), orchapi.InboxQuery{}))(t); !hasMail(in, m3.ID) {
				return fmt.Errorf("%s inbox lacks group mail: %+v", s, in)
			}
			return nil
		})
	}
	if ack := mustf(orchclient.Ack(ctx, at(r.owner, "", cs2.Address), m3.ID))(t); !ack.Acked {
		t.Fatalf("ack %+v", ack)
	}
	if ack := mustf(orchclient.Ack(ctx, rs, m3.ID))(t); ack.Acked || ack.AckedBy != cs2.Address {
		t.Fatalf("second ack %+v", ack)
	}
	// A machine that never joined is not a mailbox.
	if _, err := orchclient.Send(ctx, am, orchapi.SendMailRequest{To: "machine:nobody", Body: "x"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("mail to a machine that never joined: %v", err)
	}

	// An ask from the replica's session: it waits for a human here, until
	// the authority says the ask's mail is answered. (An agent's other
	// needs=human mail is a report, which waits on nobody.)
	ask := mustf(orchclient.NewAsk(ctx, rs, orchapi.NewAskRequest{Card: &orchapi.AskCard{Context: "Two residents want the same holiday off, and only one can have it.",
		Question: "Who gets it?", Choices: []orchapi.AskChoice{{Label: "The senior one"}, {Label: "The one who asked first"}},
		Recommend: "b", Why: "First come is the rule they already know.", Searched: "the program's past rulings: nothing on this"}}))(t)
	if got := mustf(orchclient.Session(ctx, r.owner, cs.Address))(t); got.State != orchapi.StateNeedsHuman || !strings.HasPrefix(got.Note, "waiting on mail") {
		t.Fatalf("asking session %+v", got)
	}
	mustf(orchclient.WithdrawAsk(ctx, at(a.owner, "harbor", ""), ask.ID))(t)
	heartbeat(t, r)
	if got := mustf(orchclient.Session(ctx, r.owner, cs.Address))(t); got.State != orchapi.StateIdle || got.Note != "answered" {
		t.Fatalf("answered session %+v", got)
	}
	// The answer is in the session's inbox on the replica.
	eventually(t, 2*time.Second, func() error {
		if in := mustf(orchclient.Inbox(ctx, rs, orchapi.InboxQuery{}))(t); len(in) == 0 {
			return fmt.Errorf("no answer in the replica session's inbox")
		}
		return nil
	})

	// A person's mail to the replica's human (its identity at the authority)
	// reaches the replica owner's inbox, and its notice. (An agent's would be
	// a report: TestReplicaReports.)
	hm := mustf(orchclient.Send(ctx, at(a.owner, "harbor", ""), orchapi.SendMailRequest{To: "human:bea", Body: "for bea himself"}))(t)
	eventually(t, 2*time.Second, func() error {
		if in := mustf(orchclient.Inbox(ctx, r.owner, orchapi.InboxQuery{}))(t); !hasMail(in, hm.ID) {
			return fmt.Errorf("replica human inbox %+v", in)
		}
		return nil
	})
	heartbeat(t, r)
	if n := mustf(orchclient.Notice(ctx, r.owner))(t); n.Unread == 0 {
		t.Fatalf("replica human notice %+v", n)
	}

	// The replica's zm ls shows the authority's sessions (presence of the
	// team project) next to its own, its own once.
	ls := mustf(orchclient.Sessions(ctx, r.owner, orchapi.SessionQuery{}))(t)
	if findSession(ls, ms.Address) == nil || findSession(ls, cs.Address) == nil {
		t.Fatalf("replica ls %+v", ls)
	}
	n := 0
	for _, s := range ls {
		if s.Address == cs.Address {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("own session listed %d times: %+v", n, ls)
	}

	// A session that ends here is gone from presence at the next heartbeat.
	mustf(orchclient.Register(ctx, r.owner, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "bea-sess-2", End: true}))(t)
	heartbeat(t, r)
	got := mustf(orchclient.Session(ctx, at(a.owner, "", ""), cs2.Address))(t)
	if got.State != orchapi.StateEnded {
		t.Fatalf("ended session at the authority %+v", got)
	}
}

// presenceReq is a heartbeat's project and its sessions part.
type presenceReq struct {
	Project   string
	Sessions  []orchapi.PresenceSession
	Ended     []orchapi.PresenceSession
	Elsewhere []string
}

// wire is req as sent: the sessions part under its name.
func (req presenceReq) wire() api.PresenceRequest {
	part, _ := json.Marshal(orchapi.SessionsBeat{Sessions: req.Sessions, Ended: req.Ended, Elsewhere: req.Elsewhere})
	return api.PresenceRequest{Project: req.Project, Parts: map[string]json.RawMessage{"sessions": part}}
}

// sessionsAnswer is a heartbeat answer's sessions part.
func sessionsAnswer(t *testing.T, body string) orchapi.SessionsBeatAnswer {
	t.Helper()
	var pr api.PresenceResponse
	var out orchapi.SessionsBeatAnswer
	if err := json.Unmarshal([]byte(body), &pr); err != nil {
		t.Fatalf("presence answer %s: %v", body, err)
	}
	if err := json.Unmarshal(pr.Parts["sessions"], &out); err != nil {
		t.Fatalf("presence answer %s: %v", body, err)
	}
	return out
}

// presenceAs posts a heartbeat with the replica's own token from its node.
func presenceAs(t *testing.T, tn *tailnettest.Net, r *daemon, tokenFile string, req presenceReq) (int, string) {
	t.Helper()
	b, _ := json.Marshal(req.wire())
	hr, _ := http.NewRequest(http.MethodPost, "http://potluck-mini:7433/v0/team/presence", bytes.NewReader(b))
	hr.Header.Set(api.HeaderProtocol, api.Protocol)
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set(api.HeaderAuthorization, "Bearer "+readToken(t, tokenFile))
	resp, err := tailnetClientAs(t, tn, r.cfg.Machine).Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// Machine names are unique per authority: a replica reports only sessions
// on the name it joined as; a guest's sessions are in its area; nobody but a
// node-bound token posts presence.
func TestPresenceChecks(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, j := replica(t, tn, a, "vida-mac", "nVIDA", "harbor/solver", "vida", "guest")
	pid := j.Remote.Project.ID
	for _, addr := range []string{"mini/abcd1234", "bea-mac/abcd1234"} {
		code, body := presenceAs(t, tn, r, j.Remote.TokenFile, presenceReq{Project: pid,
			Sessions: []orchapi.PresenceSession{{Address: addr, State: orchapi.StateRunning}}})
		if code != http.StatusForbidden || !strings.Contains(body, "vida-mac") {
			t.Fatalf("presence for %s: %d %s", addr, code, body)
		}
	}
	// A guest's session defaults to its area; another area is refused like
	// a missing one, and stored nowhere.
	code, body := presenceAs(t, tn, r, j.Remote.TokenFile, presenceReq{Project: pid, Sessions: []orchapi.PresenceSession{
		{Address: "vida-mac/aaaaaaaa", State: orchapi.StateRunning},
		{Address: "vida-mac/bbbbbbbb", State: orchapi.StateRunning, Area: "billing"},
		{Address: "vida-mac/cccccccc", State: orchapi.StateRunning, Area: "nope-area"},
	}})
	if code != http.StatusOK {
		t.Fatalf("presence: %d %s", code, body)
	}
	pr := sessionsAnswer(t, body)
	if len(pr.Sessions) != 3 || pr.Sessions[0].Error != "" {
		t.Fatalf("presence answer %s", body)
	}
	billing, nope := pr.Sessions[1].Error, pr.Sessions[2].Error
	if billing == "" || strings.ReplaceAll(billing, "billing", "X") != strings.ReplaceAll(nope, "nope-area", "X") {
		t.Fatalf("hidden area answered %q, missing area %q", billing, nope)
	}
	all := mustf(orchclient.Sessions(ctx, at(a.owner, "", ""), orchapi.SessionQuery{All: true}))(t)
	if p := findSession(all, "vida-mac/aaaaaaaa"); p == nil || p.Area != "solver" {
		t.Fatalf("guest presence %+v", all)
	}
	if findSession(all, "vida-mac/bbbbbbbb") != nil {
		t.Fatalf("refused presence stored: %+v", all)
	}
	// A hidden project is not found, like a missing one.
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "zz-side"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "zz-side"))(t)
	side := mustf(a.owner.GetProject(ctx, "zz-side"))(t)
	c1, b1 := presenceAs(t, tn, r, j.Remote.TokenFile, presenceReq{Project: side.ID})
	c2, b2 := presenceAs(t, tn, r, j.Remote.TokenFile, presenceReq{Project: "01JNOPE0000000000000000000"})
	if c1 != c2 || strings.ReplaceAll(b1, side.ID, "X") != strings.ReplaceAll(b2, "01JNOPE0000000000000000000", "X") {
		t.Fatalf("hidden project %d %s, missing %d %s", c1, b1, c2, b2)
	}
	// The socket (no node-bound token) cannot post presence.
	if err := a.owner.Do(ctx, http.MethodPost, "/v0/team/presence", api.PresenceRequest{Project: pid}, nil); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("presence from the socket: %v", err)
	}
	// On the replica, a session in the team project outside the guest's
	// area is refused locally (fast error), and one with no area gets it.
	if _, err := orchclient.Register(ctx, at(r.owner, "harbor/billing", ""), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "v-billing"}); err != nil {
		// register never fails: it falls back to project none
		t.Fatalf("register: %v", err)
	} else if got := mustf(orchclient.Sessions(ctx, r.owner, orchapi.SessionQuery{}))(t); len(got) != 1 || got[0].Project != api.ProjectNone {
		t.Fatalf("register outside the guest's area: %+v", got)
	}
	vs := register(t, r, "harbor", "v-solver")
	if vs.Project != pid || vs.Area != "solver" {
		t.Fatalf("guest session %+v", vs)
	}
}

// A replica that learns its session's lease is gone leaves its state alone;
// a session whose presence is stale counts as idle at the authority, so its
// bound lease stops renewing and expires.
func TestLeaseLostAndStalePresence(t *testing.T) {
	quietPresence(t)
	oldStale := session.PresenceStale
	session.PresenceStale = 1500 * time.Millisecond
	t.Cleanup(func() { session.PresenceStale = oldStale })
	ctx := context.Background()
	tn := tailnettest.New()
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), func(c *config.Config) {
			c.File.LeaseTickSeconds, c.File.BoundLeaseTTLSeconds = 1, 2
		})
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.AddArea(ctx, "harbor", api.Area{Name: "solver"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")

	// 1. A lease the authority drops (an owner's force release) is lost.
	cs := register(t, r, "harbor/solver", "bea-lost")
	heartbeat(t, r)
	rs := at(r.owner, "", cs.Address)
	it := mustf(orchclient.CreateWork(ctx, at(r.owner, "harbor", cs.Address), orchapi.CreateWorkRequest{Title: "to lose", Area: "solver"}))(t)
	mustf(orchclient.Claim(ctx, rs, it.ID, orchapi.ClaimRequest{}))(t)
	heartbeat(t, r)
	if got := mustf(orchclient.Session(ctx, r.owner, cs.Address))(t); got.State == orchapi.StateNeedsHuman {
		t.Fatalf("a held lease reported lost: %+v", got)
	}
	mustf(orchclient.Release(ctx, at(a.owner, "harbor", ""), it.ID, orchapi.ReleaseRequest{Force: true}))(t)
	heartbeat(t, r)
	got := mustf(orchclient.Session(ctx, r.owner, cs.Address))(t)
	if got.State != cs.State || got.Note != cs.Note {
		t.Fatalf("after the lease went: %+v", got)
	}
	// A release through the proxy is not a loss.
	it2 := mustf(orchclient.CreateWork(ctx, at(r.owner, "harbor", cs.Address), orchapi.CreateWorkRequest{Title: "to release", Area: "solver"}))(t)
	mustf(orchclient.Claim(ctx, rs, it2.ID, orchapi.ClaimRequest{}))(t)
	mustf(orchclient.SetState(ctx, r.owner, cs.Address, orchapi.StateRequest{State: orchapi.StateRunning}))(t)
	heartbeat(t, r)
	mustf(orchclient.Release(ctx, rs, it2.ID, orchapi.ReleaseRequest{}))(t)
	heartbeat(t, r)
	if got := mustf(orchclient.Session(ctx, r.owner, cs.Address))(t); got.State != orchapi.StateRunning {
		t.Fatalf("a release through the proxy counted as lost: %+v", got)
	}

	// 2. A managed session on the replica: its claim at the authority is
	// bound (presence says managed), renewed while heartbeats come.
	sessionModule(t, r).Probe = func(...string) orchapi.AdmissionProbe {
		return orchapi.AdmissionProbe{MemoryFreePercent: 80, DiskFreeMB: 100000}
	}
	dir, _ := os.MkdirTemp("", "zmcwd")
	t.Cleanup(func() { os.RemoveAll(dir) })
	ms := mustf(orchclient.Spawn(ctx, at(r.owner, "harbor/solver", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: dir}))(t)
	t.Cleanup(func() {
		_, _ = orchclient.Stop(context.Background(), r.owner, ms.Address, orchapi.StopRequest{Signal: "KILL"})
	})
	heartbeat(t, r)
	it3 := mustf(orchclient.CreateWork(ctx, at(r.owner, "harbor", ms.Address), orchapi.CreateWorkRequest{Title: "bound", Area: "solver"}))(t)
	cl := mustf(orchclient.Claim(ctx, at(r.owner, "", ms.Address), it3.ID, orchapi.ClaimRequest{}))(t)
	if !cl.Lease.Bound {
		t.Fatalf("a managed replica session's claim is not bound: %+v", cl.Lease)
	}
	for i := 0; i < 8; i++ { // 4 s, twice the bound TTL: renewed while heartbeats come
		time.Sleep(500 * time.Millisecond)
		heartbeat(t, r)
	}
	if h := mustf(orchclient.Holder(ctx, at(a.owner, "harbor", ""), it3.ID))(t); h.Holder != ms.Address {
		t.Fatalf("bound lease not renewed while present: %+v", h)
	}
	// Past the claim's deadline the replica's copy still lists it, with no
	// deadline: renewals reach the authority only, so the copy's would read expired.
	var copied *orchapi.Lease
	for _, l := range mustf(orchclient.Leases(ctx, at(r.owner, "harbor", ""), orchapi.LeaseQuery{}))(t) {
		if l.Key == it3.ID {
			copied = &l
		}
	}
	if copied == nil || !copied.Bound || copied.Expires != nil {
		t.Fatalf("the replica's copy of a renewed bound lease: %+v", copied)
	}
	// No heartbeat: stale after 1.5 s (idle), not renewed, expired within
	// the 2 s TTL plus a tick.
	deadline := time.Now().Add(10 * time.Second)
	for {
		h := mustf(orchclient.Holder(ctx, at(a.owner, "harbor", ""), it3.ID))(t)
		if h.Holder == "" {
			break
		}
		if h.Session != nil && h.Session.State != orchapi.StateIdle && time.Since(*h.Session.Seen) > 2*time.Second {
			t.Fatalf("stale presence not idle: %+v", h.Session)
		}
		if time.Now().After(deadline) {
			t.Fatalf("a stale holder's bound lease never expired: %+v", h)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if p := findSession(mustf(orchclient.Sessions(ctx, at(a.owner, "", ""), orchapi.SessionQuery{}))(t), ms.Address); p == nil || p.State != orchapi.StateIdle || p.Note != "presence stale" {
		t.Fatalf("stale presence %+v", p)
	}
	heartbeat(t, r)
	if got := mustf(orchclient.Session(ctx, r.owner, ms.Address))(t); got.State != ms.State || got.Note != ms.Note {
		t.Fatalf("managed session after its lease expired: %+v", got)
	}
}

// Hooks on a replica stay local: the prompt hook (state + notice) answers
// from the presence cache with the authority up or gone, within the hook
// budget; proxied mail reads and sends cost about one round trip; a
// heartbeat is measured too.
func TestReplicaMailLatency(t *testing.T) {
	if testing.Short() {
		t.Skip("latency")
	}
	timing := newTeamTiming(t, "mail")
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	var peers []string
	for i := 0; i < 8; i++ {
		peers = append(peers, register(t, r, "harbor/solver", "bea-lat-"+string(rune('a'+i))).Address)
	}
	cs := register(t, r, "harbor/solver", "bea-lat-main")
	heartbeat(t, r)
	am := at(a.owner, "harbor", "")
	for i := 0; i < 20; i++ {
		mustf(orchclient.Send(ctx, am, orchapi.SendMailRequest{To: cs.Address, Body: "mail " + string(rune('a'+i))}))(t)
	}
	heartbeat(t, r)
	const n = 200
	measure := func(f func(i int) error) []time.Duration {
		var ds []time.Duration
		for i := 0; i < n; i++ {
			t0 := time.Now()
			if err := f(i); err != nil {
				t.Fatal(err)
			}
			ds = append(ds, time.Since(t0))
		}
		sortDurations(ds)
		return ds
	}
	hook := orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude", RuntimeID: "bea-lat-main", State: orchapi.StateRunning}, Notice: true}
	hookOnce := func(int) error {
		h, err := orchclient.Hook(ctx, r.sock, hook)
		if err == nil && (h.Notice == nil || h.Notice.Unread != 20) {
			t.Fatalf("hook notice %+v", h.Notice)
		}
		return err
	}
	rs := at(r.sock, "", cs.Address)
	beat := measure(func(int) error { heartbeat(t, r); return nil })
	hookSamples := measure(hookOnce)
	inbox := measure(func(int) error { _, err := orchclient.Inbox(ctx, rs, orchapi.InboxQuery{}); return err })
	send := measure(func(i int) error {
		// Spread over peers with distinct bodies: SPEC 9 "Send limits" refuses one
		// pair's 61st mail in 10 minutes and the same mail twice.
		_, err := orchclient.Send(ctx, at(r.sock, "harbor", cs.Address), orchapi.SendMailRequest{To: peers[i%len(peers)], Body: fmt.Sprintf("lat %d", i)})
		return err
	})
	a.stop()
	down := measure(hookOnce)
	t.Logf("loopback, %d runs, p50 / p95: heartbeat (9 sessions) %v / %v; prompt hook with notice %v / %v; proxied inbox %v / %v; proxied send %v / %v; prompt hook, authority down %v / %v",
		n, beat[n/2], beat[n*95/100], hookSamples[n/2], hookSamples[n*95/100], inbox[n/2], inbox[n*95/100], send[n/2], send[n*95/100], down[n/2], down[n*95/100])
	timing.record("team/mail-hook", hookSamples, 15*time.Millisecond)
	timing.record("team/mail-hook-authority-down", down, 15*time.Millisecond)
}

func sortDurations(ds []time.Duration) {
	for i := 1; i < len(ds); i++ {
		for j := i; j > 0 && ds[j] < ds[j-1]; j-- {
			ds[j], ds[j-1] = ds[j-1], ds[j]
		}
	}
}

// SPEC 9 "Send limits": a replica's mail is counted once, at the authority,
// and the authority's refusal reaches the replica's sender with its wait.
func TestReplicaMailSendLimit(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	from, to := register(t, r, "harbor", "bea-limit-a"), register(t, r, "harbor", "bea-limit-b")
	heartbeat(t, r)
	c := at(r.sock, "harbor", from.Address)
	for i := range 60 { // the default pair limit: counted twice, this would stop halfway
		m := mustf(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: to.Address, Body: fmt.Sprintf("note %d", i)}))(t)
		if strings.HasPrefix(m.Via, "queued") {
			t.Fatalf("mail %d queued, not answered by the authority: %+v", i, m)
		}
	}
	_, err := orchclient.Send(ctx, c, orchapi.SendMailRequest{To: to.Address, Body: "one more"})
	if e, _ := err.(*api.Error); e == nil || e.Code != api.CodeRateLimited || e.RetryAfter <= 0 || !strings.Contains(e.Message, "try again in") {
		t.Fatalf("over the pair limit through a replica: %v", err)
	}
	if sent := mustf(orchclient.Mail(ctx, at(a.owner, "harbor", ""), orchapi.MailQuery{From: from.Address, Limit: 100}))(t); len(sent) != 60 {
		t.Fatalf("the authority stored %d, want 60", len(sent))
	}
}

// Mail to a session on another machine is filed in that session's area when
// the sender names none: a member mailing a guest's session from the repo
// root would otherwise file it at project level, where the guest can never
// read it, while the sender saw "sent" (dogfood, round B2 rehearsal).
func TestMailToGuestSessionFiledInItsArea(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	v, _ := replica(t, tn, a, "vida-mac", "nVIDA", "harbor/solver", "vida", "guest")
	cs := register(t, r, "harbor", "bea-sess-1") // project level
	vs := register(t, v, "harbor", "vida-sess-1")
	if vs.Area != "solver" {
		t.Fatalf("guest session %+v", vs)
	}
	heartbeat(t, r)
	heartbeat(t, v)
	m := mustf(orchclient.Send(ctx, at(r.owner, "harbor", cs.Address), orchapi.SendMailRequest{To: vs.Address, Body: "hi vida"}))(t)
	if m.Area != "solver" || m.Recipient != vs.Address {
		t.Fatalf("mail %+v", m)
	}
	eventually(t, 2*time.Second, func() error {
		if in := mustf(orchclient.Inbox(ctx, at(v.owner, "", vs.Address), orchapi.InboxQuery{}))(t); !hasMail(in, m.ID) {
			return fmt.Errorf("guest inbox %+v", in)
		}
		return nil
	})
	// The sender's own area still wins when named.
	m2 := mustf(orchclient.Send(ctx, at(r.owner, "harbor", cs.Address), orchapi.SendMailRequest{To: vs.Address, Area: "billing", Body: "in billing"}))(t)
	if m2.Area != "billing" {
		t.Fatalf("explicit area %+v", m2)
	}
	if in := mustf(orchclient.Inbox(ctx, at(v.owner, "", vs.Address), orchapi.InboxQuery{}))(t); hasMail(in, m2.ID) {
		t.Fatalf("guest sees billing mail %+v", in)
	}
	// So is the mail's ref: an item's area.
	bill := mustf(orchclient.CreateWork(ctx, at(a.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "b", Area: "billing"}))(t)
	m3 := mustf(orchclient.Send(ctx, at(r.owner, "harbor", cs.Address), orchapi.SendMailRequest{To: vs.Address, Ref: bill.ID, Body: "ref in billing"}))(t)
	if m3.Area != "billing" {
		t.Fatalf("ref area %+v", m3)
	}
}

// A heartbeat answer that left before a release through the proxy and
// arrives after it still lists the released key: taken as it was, it put the
// key back, and the next heartbeat reported the released lease lost (a
// -race flake of TestLeaseLostAndStalePresence, when the session-event kick
// sent a heartbeat during the release).
func TestStaleAnswerAfterReleaseIsNotALoss(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-stale")
	heartbeat(t, r)
	rs := at(r.owner, "", cs.Address)
	it := mustf(orchclient.CreateWork(ctx, at(r.owner, "harbor", cs.Address), orchapi.CreateWorkRequest{Title: "to release", Area: "solver"}))(t)
	mustf(orchclient.Claim(ctx, rs, it.ID, orchapi.ClaimRequest{}))(t)
	heartbeat(t, r)

	undo := team.DelayBeatApply(700 * time.Millisecond)
	done := make(chan struct{})
	go func() { heartbeat(t, r); close(done) }() // its answer, listing the key, is applied late
	time.Sleep(200 * time.Millisecond)           // sent and answered, not yet applied
	mustf(orchclient.Release(ctx, rs, it.ID, orchapi.ReleaseRequest{}))(t)
	<-done
	undo()
	heartbeat(t, r)
	if got := mustf(orchclient.Session(ctx, r.owner, cs.Address))(t); got.State == orchapi.StateNeedsHuman {
		t.Fatalf("a release through the proxy counted as lost: %+v", got)
	}
}

// A session's issue closed by the GitHub link (its PR merged) is the work
// finishing, not a lost lease; the same close by a person, and a later lease on
// the key that really goes, still read as lost. (14 of 16 "lost" lines
// after a release were this.)
func TestGitHubCloseIsNotALostLease(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := nativeAuthority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := register(t, r, "harbor", "bea-finish")
	heartbeat(t, r)
	rs := at(r.owner, "", cs.Address)
	state := func() orchapi.Session { return mustf(orchclient.Session(ctx, r.owner, cs.Address))(t) }
	// The link acts as a member token of the identity zm-github, on the authority.
	link, _ := replica(t, tn, a, "gh-box", "nGH", "harbor", orchapi.IdentityGitHub, "member")
	github := func(id string) {
		t.Helper()
		mustf(orchclient.SetStatus(ctx, at(link.owner, "harbor", ""), id, orchapi.StatusRequest{Status: orchapi.WorkDone}))(t)
	}

	is := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "merged"}))(t)
	mustf(orchclient.ClaimIssue(ctx, rs, is.ID))(t)
	heartbeat(t, r)
	github("issue:" + is.ID)
	waitDrained(t, link)
	if h := mustf(orchclient.IssueHistory(ctx, ao, is.ID))(t); h[len(h)-1].Actor != orchapi.IdentityGitHub {
		t.Fatalf("the close is not the link's: %+v", h[len(h)-1])
	}
	if _, err := orchclient.Holder(ctx, ao, "issue:"+is.ID); err != nil {
		t.Fatal(err)
	}
	heartbeat(t, r)
	if got := state(); got.State == orchapi.StateNeedsHuman {
		t.Fatalf("a merged PR's close read as lost: %+v", got)
	}
	// The holder still hears why: a PR may only have mentioned the issue.
	told := false
	for deadline := time.Now().Add(10 * time.Second); !told; time.Sleep(50 * time.Millisecond) { // the copy syncs it
		for _, m := range mustf(orchclient.Inbox(ctx, rs, orchapi.InboxQuery{}))(t) {
			told = told || (m.Kind == orchapi.MailKindNotice && strings.Contains(m.Body, "the GitHub link closed"))
		}
		if !told && time.Now().After(deadline) {
			t.Fatal("the holder was not told the link closed its issue")
		}
	}

	// The record does not outlive the lease: claimed again, then forced away.
	mustf(orchclient.ReopenIssue(ctx, ao, is.ID, orchapi.ReopenIssueRequest{}))(t)
	mustf(orchclient.ClaimIssue(ctx, rs, is.ID))(t)
	heartbeat(t, r)
	beforeLoss := state()
	if !teamService(t, r).ReportsLeaseHeld(cs.Address, "issue:"+is.ID) {
		t.Fatal("reclaimed issue was not reported held")
	}
	mustf(orchclient.Release(ctx, ao, "issue:"+is.ID, orchapi.ReleaseRequest{Force: true}))(t)
	heartbeat(t, r)
	if got := state(); got.State != beforeLoss.State || got.Note != beforeLoss.Note {
		t.Fatalf("a real loss after an earlier merge changed the session request: %+v", got)
	}
	if teamService(t, r).ReportsLeaseHeld(cs.Address, "issue:"+is.ID) {
		t.Fatal("a real loss after an earlier merge remained reported held")
	}
}

// A holder that cannot be told (no presence row) still drops its lost key,
// without manufacturing a human request.
func TestGitHubCloseUntoldReadsLost(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := nativeAuthority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	link, _ := replica(t, tn, a, "gh-box", "nGH", "harbor", orchapi.IdentityGitHub, "member")
	cs := register(t, r, "harbor", "bea-untold")
	is := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "before its first heartbeat"}))(t)
	rs := at(r.owner, "", cs.Address)
	mustf(orchclient.Claim(ctx, rs, "issue:"+is.ID, orchapi.ClaimRequest{TTLSeconds: 3600}))(t) // no presence row yet
	if !teamService(t, r).ReportsLeaseHeld(cs.Address, "issue:"+is.ID) {
		t.Fatal("claim was not reported held before its close")
	}
	mustf(orchclient.SetStatus(ctx, at(link.owner, "harbor", ""), "issue:"+is.ID, orchapi.StatusRequest{Status: orchapi.WorkDone}))(t)
	waitDrained(t, link)
	heartbeat(t, r)
	if got := mustf(orchclient.Session(ctx, r.owner, cs.Address))(t); got.State != cs.State || got.Note != cs.Note {
		t.Fatalf("an untold close changed the session request: %+v", got)
	}
	if teamService(t, r).ReportsLeaseHeld(cs.Address, "issue:"+is.ID) {
		t.Fatal("an untold close remained reported held")
	}
}
