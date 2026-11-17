package team_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// TestPersonMachineTakesOnlyThatPersonsYes: a project with
// two owners, the authority's human (mini-human) and bo, who joined the box.
// Only bo marks the box as bo's own (not the other owner, not for someone
// else, not from a session, not on a machine someone else joined). An action
// card on it is then addressed to bo; a card naming mini-human is refused;
// mini-human's paired phone answers neither choice, bo's does, and the action
// runs. Only bo gives the machine back; a card made while it was a team
// machine takes only bo's yes once it is bo's again; on a team machine
// either owner's yes runs it; once bo no longer owns the project, the other
// owner may clear it.
func TestPersonMachineTakesOnlyThatPersonsYes(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	sshDir := t.TempDir()
	defer team.SetSSHHome(sshDir)()
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}

	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ada@example.com", Host: "mini"}, discard, roomy)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	join := func(machine, as, role string, mut func(*config.Config)) *daemon {
		d := startDaemonWith(t, tn, machine, tailnettest.Identity{NodeID: "n" + strings.ToUpper(machine), NodeKey: "nodekey:" + machine, Login: as + "@example.com", Host: machine}, discard, mut)
		inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: as, Role: role}))(t)
		mustf(d.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
		return d
	}
	box := join("box", "bo", "member", func(c *config.Config) { roomy(c); c.File.RemoteTasks = true })
	mac := join("mac", "cy", "member", roomy)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bo", Project: "harbor", Role: "owner"}))(t)
	mustf(orchclient.Spawn(ctx, at(box.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: t.TempDir()}))(t)
	ss := mustf(orchclient.Spawn(ctx, at(mac.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: t.TempDir()}))(t)
	agent := at(mac.owner, "harbor", ss.Address)
	heartbeat(t, box)
	heartbeat(t, mac)
	it := mustf(orchclient.CreateWork(ctx, at(mac.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "A laptop needs to reach the build box"}))(t)

	var svc *approvals.Service
	for _, m := range a.dm.Modules() {
		if s, ok := m.(*approvals.Service); ok {
			svc = s
		}
	}
	pair := func(name, person string, rm *core.Remote) {
		t.Helper()
		code := mustf(svc.AddFor(ctx, "test", name, person))(t)
		ap := mustf(svc.Pair(ctx, rm, code.Code))(t)
		if ap.State != orchapi.ApproverActive {
			mustf(svc.ConfirmNode(ctx, adaPhone, name, ap.Node))(t)
		}
	}
	pair("ada-phone", "mini-human", adaPhone)
	pair("bo-phone", "bo", boPhone)

	key := sshKeyLine(t, "someone@laptop")
	ask := func(topic, to string) (orchapi.Ask, error) {
		return orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: topic, To: to,
			Card: &orchapi.AskCard{Context: "A laptop cannot reach the build box, so the nightly run cannot be checked by hand.",
				Question: "Let the laptop reach the build box (" + topic + ")?", Choices: []orchapi.AskChoice{{Label: "Yes, allow it"}, {Label: "No, not now"}},
				Recommend: "a", Why: "The box is checked daily.", Searched: "the access notes on the work item: nothing yet",
				Action: &orchapi.AskAction{Kind: orchapi.ActionAuthorizeKey, Machine: "box", User: me.Username, Key: key}}})
	}
	setPerson := func(c *client.Client, person string) (orchapi.MachinePerson, error) {
		return orchclient.SetMachinePerson(ctx, c, "box", orchapi.MachinePersonRequest{Project: "harbor", Person: person})
	}
	refused := func(what string, err error, code, want string) {
		t.Helper()
		if !api.IsCode(err, code) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v", what, err)
		}
	}

	eventually(t, 15*time.Second, func() error {
		heartbeat(t, box)
		for _, tg := range mustf(orchclient.RunTargets(ctx, at(a.owner, "harbor", ""), ""))(t).Targets {
			if tg.Machine == "box" && len(tg.People) == 1 {
				return nil
			}
		}
		return fmt.Errorf("box not listed yet")
	})

	// Who may mark a machine, for whom, and which machines.
	_, err = setPerson(a.owner, "bo")
	refused("the other owner marks it bo's", err, api.CodeForbidden, "only bo marks")
	_, err = setPerson(a.owner, "mini-human")
	refused("the other owner marks it their own", err, api.CodeForbidden, "not joined by mini-human alone")
	_, err = setPerson(mac.owner, "cy")
	refused("a member", err, api.CodeForbidden, "only the project's owner")
	_, err = setPerson(agent, "cy")
	refused("a session", err, api.CodeForbidden, "a session does not say")
	_, err = orchclient.SetMachinePerson(ctx, box.owner, "nowhere", orchapi.MachinePersonRequest{Project: "harbor", Person: "bo"})
	refused("an unknown machine", err, api.CodeNotFound, "nowhere")
	if mp := mustf(setPerson(box.owner, "Human:Bo"))(t); mp.Person != "bo" || mp.SetBy != "bo" {
		t.Fatalf("set: %+v", mp)
	}
	if l := mustf(orchclient.MachinePeople(ctx, a.owner, "harbor"))(t); len(l.Machines) != 1 || l.Machines[0].Machine != "box" || l.Machines[0].Person != "bo" {
		t.Fatalf("list: %+v", l)
	}

	// A card naming the other owner is refused: their yes could never run it.
	_, err = ask("to-mini", "human:mini-human")
	refused("card to the other owner", err, api.CodeBadRequest, "box is bo's own machine")
	// A card for the owners goes to the machine's person.
	first := mustf(ask("for-owners", ""))(t)
	if first.Owner != "bo" || first.To != "human:bo" || first.Card.Action.MachinePerson != "bo" {
		t.Fatalf("addressed: owner %q to %q action %+v", first.Owner, first.To, first.Card.Action)
	}
	// The other owner's phone answers neither choice; the person's does.
	for _, c := range []string{"a", "b"} {
		_, err := svc.Decide(ctx, adaPhone, first.ID, c)
		refused("the other owner picks "+c, err, api.CodeForbidden, "own machine")
	}
	mustf(svc.Decide(ctx, boPhone, first.ID, "a"))(t)
	waitDone := func(id string) {
		t.Helper()
		eventually(t, 15*time.Second, func() error {
			heartbeat(t, box)
			svc.WatchActions(ctx)
			got := mustf(orchclient.GetAsk(ctx, a.owner, id))(t)
			if got.ActionState == nil || got.ActionState.State != orchapi.ActionDone {
				return fmt.Errorf("action state %+v", got.ActionState)
			}
			return nil
		})
	}
	waitDone(first.ID)
	if b, err := os.ReadFile(filepath.Join(sshDir, ".ssh", "authorized_keys")); err != nil || string(b) != key+"\n" {
		t.Fatalf("authorized_keys %q %v", b, err)
	}

	// Only the machine's person gives it back to the team.
	_, err = setPerson(a.owner, "")
	refused("the other owner clears it", err, api.CodeForbidden, "only bo")
	if mp := mustf(setPerson(box.owner, ""))(t); mp.Person != "" {
		t.Fatalf("cleared: %+v", mp)
	}

	// A card made on a team machine goes to the owners; once the machine is
	// a person's again, only that person's yes counts (checked as it is now).
	later := mustf(ask("made-on-team", ""))(t)
	if later.Owner != "" || later.Card.Action.MachinePerson != "" {
		t.Fatalf("team machine card: owner %q action %+v", later.Owner, later.Card.Action)
	}
	mustf(setPerson(box.owner, "bo"))(t)
	_, err = svc.Decide(ctx, adaPhone, later.ID, "a")
	refused("the other owner after it became bo's", err, api.CodeForbidden, "own machine")

	// On a team machine either owner's yes runs it.
	mustf(setPerson(box.owner, ""))(t)
	mustf(svc.Decide(ctx, adaPhone, later.ID, "a"))(t)
	waitDone(later.ID)

	// A person who no longer owns the project holds no machine: the other
	// owner may clear it.
	mustf(setPerson(box.owner, "bo"))(t)
	mustf(a.owner.Revoke(ctx, api.GrantRequest{Identity: "bo", Project: "harbor", Role: "owner"}))(t)
	if mp := mustf(setPerson(a.owner, ""))(t); mp.Person != "" {
		t.Fatalf("cleared after bo left: %+v", mp)
	}
}

var (
	adaPhone = &core.Remote{Node: "ada-phone", NodeID: "nADAPHONE", NodeKey: "nodekey:adaphone"}
	boPhone  = &core.Remote{Node: "bo-phone", NodeID: "nBOPHONE", NodeKey: "nodekey:bophone"}
)
