package team_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// The default class is capped by the current grant as well as the joined
// token's role; changing that grant needs no rejoin.
func TestOperatorClassFollowsCurrentGrant(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	box := startDaemon(t, tn, "box", tailnettest.Identity{NodeID: "nBOX", NodeKey: "nodekey:box", Login: "bea@example.com", Host: "box"})
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	j := mustf(box.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	link := teamService(t, a)
	for _, role := range []string{"member", "guest", "member"} {
		mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Project: "harbor", Role: role}))(t)
		want := api.MachineTrusted
		if role == "guest" {
			want = api.MachinePublic
		}
		if c, err := link.MachineClass(ctx, j.Remote.Project.ID, "box"); err != nil || c.Class != want || c.SetBy != "" || c.FromInvite {
			t.Errorf("class with current %s grant: %+v %v; want default %s", role, c, err, want)
		}
	}
}

// Operator class (SPEC 8): the authority decides each machine's class (a
// member's machine trusted, a guest's public, by default), only the owner
// changes it (also from a member Mac, sent on to the authority), and remote
// tasks go only to trusted machines: named public is forbidden, auto skips
// it, and going back to the default lets it take them again.
func TestOperatorClassPlacesRemoteTasks(t *testing.T) {
	quietPresence(t)
	defer team.ReportEveryBeat()()
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, roomy)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	join := func(machine, as, role string, mut func(*config.Config)) *daemon {
		d := startDaemonWith(t, tn, machine, tailnettest.Identity{NodeID: "n" + strings.ToUpper(machine), NodeKey: "nodekey:" + machine, Login: as + "@example.com", Host: machine}, discard, mut)
		inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: as, Role: role}))(t)
		mustf(d.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
		return d
	}
	box := join("box", "box", "member", fakeTaskAgent(t, "box"))
	lent := join("lent", "lender", "guest", fakeTaskAgent(t, "lent"))
	mac := join("mac", "ivogh", "member", roomy)
	bea := join("bea-mac", "bea", "member", roomy)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "ivogh", Project: "harbor", Role: "owner"}))(t)
	for _, d := range []*daemon{box, lent, mac, bea} {
		heartbeat(t, d)
	}

	classes := func(c *client.Client) map[string]api.MachineClass {
		t.Helper()
		l := mustf(c.MachineClasses(ctx, "harbor"))(t)
		out := map[string]api.MachineClass{}
		for _, m := range l.Machines {
			out[m.Machine] = m
		}
		return out
	}
	cs := classes(a.owner)
	if cs["mini"].Class != api.MachineTrusted || cs["box"].Class != api.MachineTrusted || cs["bea-mac"].Class != api.MachineTrusted ||
		cs["lent"].Class != api.MachinePublic || !strings.Contains(cs["lent"].Why, "guest") {
		t.Fatalf("defaults: %+v", cs)
	}
	// The owner's member Mac reads them through the authority too.
	if got := classes(mac.owner); got["box"].Class != api.MachineTrusted || got["box"].Why != cs["box"].Why {
		t.Fatalf("from the Mac: %+v", got)
	}

	no := false
	ss := mustf(orchclient.Spawn(ctx, at(mac.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: t.TempDir()}))(t)
	pc := at(mac.owner, "harbor", ss.Address)
	start := func(machine string) (orchapi.Task, error) {
		return orchclient.TaskStart(ctx, pc, orchapi.TaskStartRequest{Agent: "codex", Prompt: "do it", Worktree: &no, Machine: machine, Cwd: t.TempDir()})
	}
	if t1, err := start("box"); err != nil || t1.Machine != "box" {
		t.Fatalf("trusted box: %+v %v", t1, err)
	}

	// The owner, from the Mac, makes box public: named, it is refused with
	// the way back in the hint; auto never picks it.
	set := mustf(mac.owner.SetMachineClass(ctx, "box", api.MachineClassRequest{Project: "harbor", Class: api.MachinePublic}))(t)
	if set.Class != api.MachinePublic || set.SetBy != "ivogh" || set.SetAt == nil || !strings.Contains(set.Why, "ivogh") {
		t.Fatalf("set: %+v", set)
	}
	_, err := start("box")
	var ae *api.Error
	if !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "public") {
		t.Fatalf("named public machine: %v", err)
	}
	if errors.As(err, &ae) && !strings.Contains(ae.Hint, "machine-class box trusted") {
		t.Fatalf("hint: %q", ae.Hint)
	}
	if _, err := start("lent"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a guest's machine took a task: %v", err)
	}
	// The Mezze app is placed the same way (box has no t3 block either, so the
	// refusal must be the class's).
	if _, err := orchclient.EnsureT3(ctx, mac.owner, "box", orchapi.T3EnsureRequest{Project: "harbor"}); !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "public") {
		t.Fatalf("the Mezze app on a public machine: %v", err)
	}
	// box and lent are the only machines that take remote tasks: with both
	// public, auto has none.
	if t2, err := start("auto"); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("auto with only public machines: %+v %v", t2, err)
	}

	// Back to the default: box takes tasks again.
	back := mustf(a.owner.SetMachineClass(ctx, "box", api.MachineClassRequest{Project: "harbor"}))(t)
	if back.Class != api.MachineTrusted || back.SetBy != "" {
		t.Fatalf("default again: %+v", back)
	}
	if t3, err := start("box"); err != nil || t3.Machine != "box" {
		t.Fatalf("box after the default: %+v %v", t3, err)
	}

	// Only the owner sets it; an unknown machine and a bad class are refused.
	if _, err := bea.owner.SetMachineClass(ctx, "box", api.MachineClassRequest{Project: "harbor", Class: api.MachinePublic}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a member set a class: %v", err)
	}
	if _, err := bea.owner.MachineClasses(ctx, "harbor"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a member read the classes: %v", err)
	}
	if _, err := a.owner.SetMachineClass(ctx, "nope", api.MachineClassRequest{Project: "harbor", Class: api.MachineTrusted}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("unknown machine: %v", err)
	}
	if _, err := a.owner.SetMachineClass(ctx, "box", api.MachineClassRequest{Project: "harbor", Class: "kinda"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("bad class: %v", err)
	}
	// A start queued for box that no heartbeat carried yet is refused when
	// box becomes public.
	queued, err := start("box")
	if err != nil || queued.State != orchapi.TaskPending {
		t.Fatalf("queued start: %+v %v", queued, err)
	}
	mustf(a.owner.SetMachineClass(ctx, "box", api.MachineClassRequest{Project: "harbor", Class: api.MachinePublic}))(t)
	if st := mustf(orchclient.TaskStatus(ctx, pc, queued.ID, 0))(t); st.State != orchapi.TaskRefused || !strings.Contains(st.Error, "public") {
		t.Fatalf("queued start after box became public: %+v", st)
	}
	mustf(a.owner.SetMachineClass(ctx, "box", api.MachineClassRequest{Project: "harbor"}))(t)
	// The authority's own machine is always trusted.
	if _, err := a.owner.SetMachineClass(ctx, "mini", api.MachineClassRequest{Project: "harbor", Class: api.MachinePublic}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("authority made public: %v", err)
	}

	// The token's own role decides the default: bea, a member, joining a
	// spare machine with a guest invite gets a public machine.
	spare := startDaemonWith(t, tn, "spare", tailnettest.Identity{NodeID: "nSPARE", NodeKey: "nodekey:spare", Login: "bea@example.com", Host: "spare"}, discard, fakeTaskAgent(t, "spare"))
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "guest"}))(t)
	mustf(spare.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	heartbeat(t, spare)
	if c := classes(a.owner)["spare"]; c.Class != api.MachinePublic {
		t.Fatalf("a member's guest-invited machine: %+v", c)
	}

	// A guest made trusted by the owner takes tasks.
	mustf(a.owner.SetMachineClass(ctx, "lent", api.MachineClassRequest{Project: "harbor", Class: api.MachineTrusted}))(t)
	if t4, err := start("lent"); err != nil || t4.Machine != "lent" {
		t.Fatalf("trusted lent: %+v %v", t4, err)
	}

	// A class set for one node does not pass to another that takes the
	// name later: lent leaves, a stranger's machine joins as lent, public.
	mustf(lent.owner.Leave(ctx, "harbor"))(t)
	lent.stop()
	lent2 := startDaemonWith(t, tn, "lent", tailnettest.Identity{NodeID: "nLENT2", NodeKey: "nodekey:lent2", Login: "stranger@example.com", Host: "lent"}, discard, fakeTaskAgent(t, "lent"))
	inv2 := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "stranger", Role: "guest"}))(t)
	mustf(lent2.owner.Join(ctx, api.JoinRequest{Code: inv2.Code}))(t)
	heartbeat(t, lent2)
	if c := classes(a.owner)["lent"]; c.Class != api.MachinePublic || c.SetBy != "" {
		t.Fatalf("the new lent inherited the old one's class: %+v", c)
	}
	if _, err := start("lent"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("the new lent took a task: %v", err)
	}
}

// A pool's host joins as a guest of one area (SPEC's pools section), which sends no machine numbers; its owner still sets its class,
// and lists it once set (zm pool create sets it on join).
func TestOperatorClassOfAnAreaGuest(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, roomy)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "pool"}))(t)
	mustf(a.owner.AddArea(ctx, "pool", api.AddAreaRequest{Name: "hosts"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "pool"))(t)
	h := startDaemonWith(t, tn, "lender", tailnettest.Identity{NodeID: "nLENDER", NodeKey: "nodekey:lender", Login: "bea@example.com", Host: "lender"}, discard, roomy)
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "pool", Area: "hosts", As: "bea", Role: "guest"}))(t)
	mustf(h.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	heartbeat(t, h)
	list := func() map[string]api.MachineClass {
		out := map[string]api.MachineClass{}
		for _, m := range mustf(a.owner.MachineClasses(ctx, "pool"))(t).Machines {
			out[m.Machine] = m
		}
		return out
	}
	if _, ok := list()["lender"]; ok {
		t.Fatal("an area guest's machine listed before its class was set")
	}
	set := mustf(a.owner.SetMachineClass(ctx, "lender", api.MachineClassRequest{Project: "pool", Class: api.MachineTrusted}))(t)
	if set.Class != api.MachineTrusted || set.SetBy == "" {
		t.Fatalf("set: %+v", set)
	}
	if c := list()["lender"]; c.Class != api.MachineTrusted {
		t.Fatalf("listed: %+v", c)
	}
	if _, err := a.owner.SetMachineClass(ctx, "nobody", api.MachineClassRequest{Project: "pool", Class: api.MachineTrusted}); !api.IsCode(err, api.CodeNotFound) {
		t.Errorf("a machine that never joined: %v", err)
	}
}

// A pool's host gets its class from the invite: the owner
// puts it on the invite (zm pool create --host, zm invite --class), the
// redemption gives it to the redeeming machine with its token, and nothing
// the redeemer sends can choose or raise it. Without one the guest's default
// holds; a class set later wins, and dropping it goes back to the invite's.
func TestOperatorClassFromTheInvite(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, roomy)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "pool"}))(t)
	mustf(a.owner.AddArea(ctx, "pool", api.AddAreaRequest{Name: "hosts"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "pool"))(t)
	invite := func(as, class string) api.InviteResponse {
		t.Helper()
		return mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "pool", Area: "hosts", As: as, Role: "guest", Class: class}))(t)
	}
	if _, err := a.owner.Invite(ctx, api.InviteRequest{Project: "pool", Area: "hosts", As: "x", Role: "guest", Class: "owner"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("an invite with class owner: %v", err)
	}
	join := func(machine, code string) *daemon {
		t.Helper()
		d := startDaemonWith(t, tn, machine, tailnettest.Identity{NodeID: "n" + strings.ToUpper(machine), NodeKey: "nodekey:" + machine, Login: machine + "@example.com", Host: machine}, discard, roomy)
		mustf(d.owner.Join(ctx, api.JoinRequest{Code: code}))(t)
		return d
	}
	list := func() map[string]api.MachineClass {
		out := map[string]api.MachineClass{}
		for _, m := range mustf(a.owner.MachineClasses(ctx, "pool"))(t).Machines {
			out[m.Machine] = m
		}
		return out
	}

	trusted := invite("bea", api.MachineTrusted)
	if trusted.Invite.Class != api.MachineTrusted {
		t.Fatalf("the invite: %+v", trusted.Invite)
	}
	join("lender", trusted.Code)
	if c := list()["lender"]; c.Class != api.MachineTrusted || !c.FromInvite || c.SetBy != "" || !strings.Contains(c.Why, "invite") {
		t.Fatalf("a trusted invite's machine, no class set by hand: %+v", c)
	}
	public := invite("sam", api.MachinePublic)
	join("stranger", public.Code)
	if c := list()["stranger"]; c.Class != api.MachinePublic || !c.FromInvite {
		t.Fatalf("a public invite's machine: %+v", c)
	}

	// A plain guest invite: the redeemer cannot ask for a class (the request
	// has no such field), and its machine keeps the guest's default.
	plain := invite("bo", "")
	r := newRedeemer(t, tn, tailnettest.Identity{NodeID: "nSNEAK", NodeKey: "nodekey:sneak", Login: "bo@example.com"})
	for _, body := range []map[string]any{
		{"code": plain.Code, "machine": "sneak", "class": "trusted"},
		{"code": plain.Code, "machine": "sneak", "role": "member", "area": ""},
	} {
		if err := r.c.Do(ctx, "POST", "/v0/invites/redeem", body, &api.RedeemResponse{}); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("a redeem that names %v: %v", body, err)
		}
	}
	join("plain", plain.Code)
	if c, ok := list()["plain"]; ok {
		t.Fatalf("a plain guest invite's machine is listed (enrolled): %+v", c)
	}
	if c := mustf(a.owner.SetMachineClass(ctx, "plain", api.MachineClassRequest{Project: "pool", Class: ""}))(t); c.Class != api.MachinePublic || c.FromInvite {
		t.Fatalf("a plain guest's class: %+v", c)
	}

	// The owner's later choice wins; "default" goes back to the invite's.
	if c := mustf(a.owner.SetMachineClass(ctx, "lender", api.MachineClassRequest{Project: "pool", Class: api.MachinePublic}))(t); c.Class != api.MachinePublic || c.SetBy == "" {
		t.Fatalf("set public: %+v", c)
	}
	if c := mustf(a.owner.SetMachineClass(ctx, "lender", api.MachineClassRequest{Project: "pool", Class: ""}))(t); c.Class != api.MachineTrusted || !c.FromInvite {
		t.Fatalf("back to the default: %+v", c)
	}
	// Removing the redeemed invite keeps the class: it went to the token.
	mustf(a.owner.RemoveInvite(ctx, trusted.Invite.ID))(t)
	if c := list()["lender"]; c.Class != api.MachineTrusted || !c.FromInvite {
		t.Fatalf("after zm invite rm: %+v", c)
	}
}
