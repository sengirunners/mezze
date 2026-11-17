package team_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/release"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"net/http"
	"slices"
	"strings"
)

// A replica's sessions reach the authority with their activity and terminal
// title; the authority says whose they are (the identity the replica joined
// as) and how far that token reaches (project, or area for a guest). The
// replica's merged list carries the authority's answer for other machines'
// sessions and its own for local ones.
func TestPresenceActivityWhoReach(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	cr, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	vr, _ := replica(t, tn, a, "vida-mac", "nVIDA", "harbor/solver", "vida", "guest")

	hook := func(d *daemon, proj, rid, activity string) orchapi.Session {
		t.Helper()
		r := mustf(orchclient.Hook(ctx, at(d.owner, proj, ""), orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude",
			RuntimeID: rid, State: orchapi.StateRunning}, Activity: activity}))(t)
		return r.Session
	}
	cs := hook(cr, "harbor/solver", "bea-act-1", "Editing store.go")
	vs := hook(vr, "harbor", "vela-act-1", "Searching")
	ms := hook(a, "harbor", "mini-act-1", "Thinking")
	if cs.Activity != "Editing store.go" || cs.Who == nil || cs.Who.Name != "bea-mac-human" || cs.Reach != orchapi.ReachAll {
		t.Fatalf("replica's own session %+v", cs)
	}
	heartbeat(t, cr)
	heartbeat(t, vr)

	all := mustf(orchclient.Sessions(ctx, at(a.owner, "", ""), orchapi.SessionQuery{}))(t)
	check := func(ss []orchapi.Session, addr, activity, who, kind, reach string) {
		t.Helper()
		p := findSession(ss, addr)
		if p == nil || p.Activity != activity || p.ActivityAt == nil || p.Who == nil || p.Who.Name != who || p.Who.Kind != kind || p.Reach != reach {
			t.Fatalf("%s: %+v (who %+v)", addr, p, func() any {
				if p == nil {
					return nil
				}
				return p.Who
			}())
		}
	}
	check(all, cs.Address, "Editing store.go", "bea", api.IdentityHuman, orchapi.ReachProject)
	check(all, vs.Address, "Searching", "vida", api.IdentityHuman, orchapi.ReachArea)
	check(all, ms.Address, "Thinking", "mini-human", api.IdentityHuman, orchapi.ReachAll)
	one := mustf(orchclient.Session(ctx, at(a.owner, "", ""), vs.Address))(t)
	if one.Activity != "Searching" || one.Reach != orchapi.ReachArea || one.Who == nil || one.Who.Name != "vida" {
		t.Fatalf("authority get %+v", one)
	}

	// The activity moves: the next heartbeat carries it.
	hook(cr, "harbor/solver", "bea-act-1", "Running the tests")
	heartbeat(t, cr)
	all = mustf(orchclient.Sessions(ctx, at(a.owner, "", ""), orchapi.SessionQuery{}))(t)
	check(all, cs.Address, "Running the tests", "bea", api.IdentityHuman, orchapi.ReachProject)

	// On bea's replica: its own session from here, the authority's and
	// vida's sessions as the authority answers them.
	merged := mustf(orchclient.Sessions(ctx, cr.owner, orchapi.SessionQuery{}))(t)
	check(merged, cs.Address, "Running the tests", "bea-mac-human", api.IdentityHuman, orchapi.ReachAll)
	check(merged, ms.Address, "Thinking", "mini-human", api.IdentityHuman, orchapi.ReachAll)
	check(merged, vs.Address, "Searching", "vida", api.IdentityHuman, orchapi.ReachArea)

	// A guest sees the solver sessions with the same who and reach.
	gm := mustf(orchclient.Sessions(ctx, vr.owner, orchapi.SessionQuery{}))(t)
	check(gm, cs.Address, "Running the tests", "bea", api.IdentityHuman, orchapi.ReachProject)
	if findSession(gm, ms.Address) != nil {
		t.Fatalf("guest sees a project-level session: %+v", gm)
	}
}

// A terminal title leaves the replica only for a known agent runtime: a
// shell's titles (preexec, vim, PROMPT_COMMAND) carry command lines, paths
// and hosts, so teammates never get them. The local row keeps both.
func TestPresenceTermTitleOnlyForAgentRuntimes(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r := startDaemonWith(t, tn, "bea-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea-mac", Login: "bea@example.com", Host: "bea-mac"},
		slog.New(slog.NewTextHandler(io.Discard, nil)), func(c *config.Config) {
			c.File.Admission = config.Admission{MinMemoryFreePercent: 1, MaxSwapUsedMB: 1 << 40, MinDiskFreeMB: 1}
		})
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)

	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in --version) echo 1.0; exit 0;; esac\nprintf '\\033]0;%s\\007' \"$TITLE\"\nexec sleep 30\n"
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	spawn := func(agent string, cmd []string, title string) orchapi.Session {
		t.Helper()
		return mustf(orchclient.Spawn(ctx, at(r.owner, "harbor", ""), orchapi.SpawnRequest{Agent: agent, Command: cmd, Cwd: dir,
			Env: map[string]string{"PATH": os.Getenv("PATH"), "TITLE": title}}))(t)
	}
	sh := spawn("shell", []string{"/bin/sh", bin}, "curl -u ana:hunter2 https://x")
	cl := spawn("claude", []string{bin}, "Fix the presence bug")
	for _, ss := range []orchapi.Session{sh, cl} {
		deadline := time.Now().Add(10 * time.Second)
		for {
			got := mustf(orchclient.Session(ctx, r.owner, ss.ID))(t)
			if got.TermTitle != "" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never titled: %+v", ss.Address, got)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}
	heartbeat(t, r)
	all := mustf(orchclient.Sessions(ctx, at(a.owner, "", ""), orchapi.SessionQuery{}))(t)
	if p := findSession(all, sh.Address); p == nil || p.TermTitle != "" {
		t.Fatalf("a shell's title reached the authority: %+v", p)
	}
	if p := findSession(all, cl.Address); p == nil || p.TermTitle != "Fix the presence bug" {
		t.Fatalf("an agent's title did not: %+v", p)
	}
	if got := mustf(orchclient.Session(ctx, r.owner, sh.ID))(t); got.TermTitle != "curl -u ana:hunter2 https://x" {
		t.Fatalf("local row lost its title: %+v", got)
	}
}

// An activity change is a live-only event now (never stored); the replica's
// presence loop, which listens live, still sends it to the authority within
// about a second, with no 15 s tick.
func TestPresenceActivityLiveKick(t *testing.T) {
	quietPresence(t) // no periodic beat: only kicks
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	cr, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	heartbeat(t, cr) // the authority knows the replica's sessions from here on
	r := mustf(orchclient.Hook(ctx, at(cr.owner, "harbor/solver", ""), orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude",
		RuntimeID: "bea-live-1", State: orchapi.StateIdle}}))(t)
	deadline := time.Now().Add(5 * time.Second)
	for {
		all := mustf(orchclient.Sessions(ctx, at(a.owner, "", ""), orchapi.SessionQuery{}))(t)
		if p := findSession(all, r.Session.Address); p != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the session never reached the authority")
		}
		time.Sleep(50 * time.Millisecond)
	}
	mustf(orchclient.Hook(ctx, at(cr.owner, "harbor/solver", ""), orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "claude",
		RuntimeID: "bea-live-1"}, Activity: "Editing store.go"}))(t)
	deadline = time.Now().Add(5 * time.Second)
	for {
		all := mustf(orchclient.Sessions(ctx, at(a.owner, "", ""), orchapi.SessionQuery{}))(t)
		if p := findSession(all, r.Session.Address); p != nil && p.Activity == "Editing store.go" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the activity never reached the authority (no kick from the live event)")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Machine numbers follow a heartbeat on their own route; GET /v0/machines
// shows them with the presence privacy, members' machines only: a guest's
// machine is neither recorded nor shown (it sees only itself), and the
// authority's own machine goes to whole-project readers.
func TestMachinesInPresence(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	cr, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	vr, vj := replica(t, tn, a, "vida-mac", "nVIDA", "harbor/solver", "vida", "guest")
	// What bea-mac learned on the release channel: the gate on other machines reads it here.
	var rs release.State
	rs.MarkStable("main-1eacf23e722f")
	rs.MarkFailed(release.Failure{Version: "main-00000000bad", At: time.Now(), Result: "rolled_back"})
	if err := release.WriteState(cr.cfg.Home, rs); err != nil {
		t.Fatal(err)
	}
	undoEvery := team.ReportEveryBeat() // the join reported already
	heartbeat(t, cr)
	undoEvery()
	heartbeat(t, vr)
	names := func(ms []api.Machine) []string {
		var out []string
		for _, m := range ms {
			out = append(out, m.Machine)
		}
		return out
	}
	list := func(c *client.Client) []api.Machine {
		t.Helper()
		var l api.MachineList
		if err := c.Do(ctx, http.MethodGet, "/v0/machines", nil, &l); err != nil {
			t.Fatal(err)
		}
		return l.Machines
	}
	// The authority's owner: itself first, then the member's machine.
	ms := list(a.owner)
	if got := strings.Join(names(ms), ","); got != "mini,bea-mac" {
		t.Fatalf("authority: %s", got)
	}
	self := ms[0].Stats
	if self.MemoryTotalMB <= 0 || self.MemoryUsedMB < 0 || self.MemoryUsedMB > self.MemoryTotalMB || self.DiskTotalMB <= 0 ||
		self.DiskFreeMB < 0 || self.DiskFreeMB > self.DiskTotalMB || self.MaxSessions != 0 || self.At.IsZero() || ms[0].Seen != nil {
		t.Fatalf("self stats %+v seen %v", self, ms[0].Seen)
	}
	if ms[1].Seen == nil || ms[1].Stats.MemoryTotalMB <= 0 || len(ms[1].Projects) != 1 {
		t.Fatalf("bea-mac row %+v", ms[1])
	}
	// Each row carries the machine's zm build and channel state (SPEC 2 "Release channel"),
	// under "potluck" or "zm" (Build reads either).
	for _, m := range ms {
		if z := m.Stats.Build(); z == nil || z.Version != api.Version || z.Started.IsZero() {
			t.Fatalf("%s zm %+v", m.Machine, z)
		}
	}
	if z := ms[1].Stats.Build(); !slices.Equal(z.Stable, []string{"main-1eacf23e722f"}) || !slices.Equal(z.Failed, []string{"main-00000000bad"}) {
		t.Fatalf("bea-mac's vouch and veto at the authority: %+v", z)
	}
	// The member's replica: its own (local) and the authority's; its own
	// copy at the authority dropped; the guest's machine not there at all.
	if got := strings.Join(names(list(cr.owner)), ","); got != "bea-mac,mini" {
		t.Fatalf("member replica: %s", got)
	}
	// The guest's replica: only its own machine.
	if got := strings.Join(names(list(vr.owner)), ","); got != "vida-mac" {
		t.Fatalf("guest replica: %s", got)
	}
	// Numbers a machine sent while it was a member go once it reports as a
	// guest (demoted): a row standing in for the member era is dropped.
	mp := orchcore.Sessions(a.dm.Deps).(core.MachinePresence)
	if err := mp.RecordMachine(ctx, vj.Remote.Project.ID, "vida-mac", api.MachineStats{MemoryTotalMB: 1}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(list(a.owner)), ","); got != "mini,bea-mac,vida-mac" {
		t.Fatalf("member-era row not listed: %s", got)
	}
	undo := team.ReportEveryBeat()
	heartbeat(t, vr)
	undo()
	if got := strings.Join(names(list(a.owner)), ","); got != "mini,bea-mac" {
		t.Fatalf("demoted machine still listed: %s", got)
	}
	// A machine that left is no longer listed.
	if err := cr.owner.Do(ctx, http.MethodDelete, "/v0/team/remotes/harbor", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(list(a.owner)), ","); got != "mini" {
		t.Fatalf("after bea left: %s", got)
	}
}

// An authority older than the machine route: heartbeats still carry every
// session (nothing new in their body), the numbers simply are not there,
// and the authority takes a heartbeat with fields it does not know.
func TestMachinesOlderAuthority(t *testing.T) {
	quietPresence(t)
	defer team.PlayOlderAuthority()()
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	cr, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := register(t, cr, "harbor", "bea-old-1")
	heartbeat(t, cr)
	heartbeat(t, cr) // the second goes the same way, quietly (no retry storm)
	all := mustf(orchclient.Sessions(ctx, at(a.owner, "", ""), orchapi.SessionQuery{}))(t)
	if findSession(all, cs.Address) == nil {
		t.Fatalf("presence lost: %+v", all)
	}
	var l api.MachineList
	if err := a.owner.Do(ctx, http.MethodGet, "/v0/machines", nil, &l); err != nil || len(l.Machines) != 1 {
		t.Fatalf("machines at an older authority: %+v %v", l, err)
	}
	// A heartbeat with a field this authority does not know is taken.
	code, body := presenceRaw(t, tn, cr, j.Remote.TokenFile, `{"project":"`+j.Remote.Project.ID+`","parts":{"sessions":{"sessions":[{"address":"`+cs.Address+
		`","state":"running","started":"2026-11-01T00:00:00Z","from_the_future":1}],"also_new":1},"from_the_future":{"x":1}},"also_new":{"x":1}}`)
	if code != http.StatusOK {
		t.Fatalf("unknown fields: %d %s", code, body)
	}
}

// An authority older than the zm block refuses the field under either name
// ("potluck", then "zm"): the replica sends its numbers again without it, so
// they still arrive.
func TestMachinesAuthorityBeforeZm(t *testing.T) {
	quietPresence(t)
	defer team.PlayAuthorityBeforeZm()()
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	cr, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	heartbeat(t, cr)
	var l api.MachineList
	if err := a.owner.Do(ctx, http.MethodGet, "/v0/machines", nil, &l); err != nil || len(l.Machines) != 2 {
		t.Fatalf("machines: %+v %v", l, err)
	}
	if m := l.Machines[1]; m.Machine != "bea-mac" || m.Stats.MemoryTotalMB <= 0 || m.Stats.Build() != nil {
		t.Fatalf("bea-mac row %+v", m)
	}
}

// TestMachinesAuthorityBeforeScriptRuns: a machine that runs scripts
// reporting to an authority older than script_runs (which knows the zm
// block) or older than both still gets its numbers through: script_runs is
// left out first, so the zm block (a canary's vouch) still arrives
// (a machine went unheard for an hour). An
// authority from before the block's new name ("potluck") gets it as "zm".
func TestMachinesAuthorityBeforeScriptRuns(t *testing.T) {
	quietPresence(t)
	for _, tc := range []struct {
		refuse []string
		zm     bool
	}{{[]string{"script_runs"}, true}, {[]string{"script_runs", "potluck"}, true}, {[]string{"script_runs", "potluck", "zm"}, false}} {
		func() {
			defer team.PlayAuthorityBefore(tc.refuse...)()
			ctx := context.Background()
			tn := tailnettest.New()
			a := authority(t, tn)
			discard := slog.New(slog.NewTextHandler(io.Discard, nil))
			box := startDaemonWith(t, tn, "box", tailnettest.Identity{NodeID: "nBOX", NodeKey: "nodekey:box", Login: "box@example.com", Host: "box"}, discard,
				func(c *config.Config) { c.File.RemoteTasks = true })
			inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "box", Role: "member"}))(t)
			mustf(box.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
			heartbeat(t, box)
			var l api.MachineList
			if err := a.owner.Do(ctx, http.MethodGet, "/v0/machines", nil, &l); err != nil || len(l.Machines) != 2 {
				t.Fatalf("refusing %v: machines: %+v %v", tc.refuse, l, err)
			}
			m := l.Machines[1]
			if m.Machine != "box" || m.Seen == nil || !m.Stats.RemoteTasks || m.Stats.ScriptRuns || (m.Stats.Build() != nil) != tc.zm {
				t.Fatalf("refusing %v: box row %+v", tc.refuse, m)
			}
		}()
	}
}

// presenceRaw posts a raw heartbeat body as r's node-bound token.
func presenceRaw(t *testing.T, tn *tailnettest.Net, r *daemon, tokenFile, body string) (int, string) {
	t.Helper()
	hr, _ := http.NewRequest(http.MethodPost, "http://potluck-mini:7433/v0/team/presence", strings.NewReader(body))
	hr.Header.Set(api.HeaderProtocol, api.Protocol)
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set(api.HeaderAuthorization, "Bearer "+readToken(t, tokenFile))
	resp, err := tailnetClientAs(t, tn, r.cfg.Machine).Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestPresenceCarriesParent: a replica's session names the session that
// started it at the authority, for whoever can read both.
func TestPresenceCarriesParent(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	cr, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	hook := func(as, rid string) orchapi.Session {
		t.Helper()
		r := mustf(orchclient.Hook(ctx, at(cr.owner, "harbor", as), orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{Agent: "codex",
			RuntimeID: rid, State: orchapi.StateRunning}}))(t)
		return r.Session
	}
	parent := hook("", "bea-parent-1")
	child := hook(parent.Address, "bea-child-1")
	if child.Parent != parent.Address {
		t.Fatalf("on the replica: child %+v", child)
	}
	heartbeat(t, cr)
	got := findSession(mustf(orchclient.Sessions(ctx, at(a.owner, "", ""), orchapi.SessionQuery{}))(t), child.Address)
	if got == nil || got.Parent != parent.Address {
		t.Fatalf("at the authority: %+v", got)
	}
	if p := findSession(mustf(orchclient.Sessions(ctx, at(a.owner, "", ""), orchapi.SessionQuery{}))(t), parent.Address); p == nil || p.Parent != "" {
		t.Fatalf("the parent itself: %+v", p)
	}
}
