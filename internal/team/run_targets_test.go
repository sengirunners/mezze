package team_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/mail"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// An agent whose choice needs Bea to act finds Bea's machine
// that takes approved actions. zm ask machines lists the project's machines
// with whose they are and whether an action may run there; an agent task
// with no --run-on runs on Bea's only ready machine; the card judge reads
// Bea's machines, and a card it sends back names that machine in its
// refusal; a report asking Bea to log in is refused, from the agent's
// replica and at the authority.
func TestRunTargetsFindTheApproversMachine(t *testing.T) {
	quietPresence(t)
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
	takes := func(c *config.Config) { roomy(c); c.File.RemoteTasks = true }
	box := join("box", "box", "member", takes)
	mac := join("mac", "bea", "member", roomy)       // Bea's laptop: takes no remote tasks
	studio := join("studio", "bea", "member", takes) // Bea's desktop: takes them
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Project: "harbor", Role: "owner"}))(t)
	for range 2 { // the first beat after the grant learns the new role; the next carries the numbers
		for _, d := range []*daemon{box, mac, studio} {
			heartbeat(t, d)
		}
	}
	ss := mustf(orchclient.Spawn(ctx, at(mac.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: t.TempDir()}))(t)
	agent := at(mac.owner, "harbor", ss.Address)
	it := mustf(orchclient.CreateWork(ctx, at(mac.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "Ship the analytics fix"}))(t)

	// Everyone's machines, from the agent's replica (forwarded to the
	// authority).
	all := mustf(orchclient.RunTargets(ctx, agent, ""))(t)
	byName := map[string]orchapi.RunTarget{}
	for _, x := range all.Targets {
		byName[x.Machine] = x
	}
	if all.Project != "harbor" || len(byName) != 4 {
		t.Fatalf("all targets: %+v", all)
	}
	if x := byName["studio"]; !x.Ready || !slices.Equal(x.People, []string{"bea"}) || !x.RemoteTasks {
		t.Fatalf("studio: %+v", x)
	}
	if x := byName["mac"]; x.Ready || !slices.Equal(x.People, []string{"bea"}) || !strings.Contains(x.Why, "remote_tasks") {
		t.Fatalf("mac: %+v", x)
	}
	if x := byName["box"]; !x.Ready || !slices.Equal(x.People, []string{"box"}) {
		t.Fatalf("box: %+v", x)
	}
	if x := byName["mini"]; x.Ready || len(x.People) != 1 {
		t.Fatalf("mini (the authority, takes no remote tasks): %+v", x)
	}

	// Bea's only: by name, with or without human:.
	for _, who := range []string{"bea", "human:bea", "Bea"} {
		l := mustf(orchclient.RunTargets(ctx, agent, who))(t)
		var names []string
		for _, x := range l.Targets {
			names = append(names, x.Machine)
		}
		if l.Person != "bea" || !slices.Equal(names, []string{"mac", "studio"}) || l.Hint != "" {
			t.Fatalf("%s: %+v", who, l)
		}
	}
	// Nobody, a machine-ish identity outside the project and a person of
	// another project answer the same not_found.
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "other"}))(t)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "olga", Project: "other", Role: "member"}))(t)
	for _, who := range []string{"nobody", "olga"} {
		if _, err := orchclient.RunTargets(ctx, agent, who); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("%s: %v", who, err)
		}
	}
	// An area member learns nothing of who has a machine here.
	mustf(a.owner.AddArea(ctx, "harbor", api.Area{Name: "billing"}))(t)
	guest := startDaemonWith(t, tn, "guestbox", tailnettest.Identity{NodeID: "nGUESTBOX", NodeKey: "nodekey:guestbox", Login: "eve@example.com", Host: "guestbox"}, discard, roomy)
	ginv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", Area: "billing", As: "eve", Role: "member"}))(t)
	mustf(guest.owner.Join(ctx, api.JoinRequest{Code: ginv.Code}))(t)
	if _, err := orchclient.RunTargets(ctx, at(guest.owner, "harbor/billing", ""), ""); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an area member: %v", err)
	}
	card := func(question string, act *orchapi.AskAction) *orchapi.AskCard {
		return &orchapi.AskCard{Context: "The analytics fix is merged but the live app still runs the old code.",
			Question: question, Choices: []orchapi.AskChoice{{Label: "Yes, ship it"}, {Label: "No, wait"}},
			Recommend: "a", Why: "The fix is small and tested.", Searched: "the work item's notes: nothing else blocks it", Action: act}
	}
	brief := "Deploy the analytics fix to the live app and check the events arrive."

	// An agent task with no machine, for Bea (--to human:bea): it runs on
	// studio, frozen on the card.
	ask := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "ship-fix", To: "human:bea",
		Card: card("Ship the analytics fix to the live app now?", &orchapi.AskAction{Kind: orchapi.ActionRun, Agent: "claude", Brief: brief})}))(t)
	if ask.Card.Action == nil || ask.Card.Action.Machine != "studio" || !strings.Contains(ask.ActionText, "on studio") {
		t.Fatalf("defaulted machine: %+v %q", ask.Card.Action, ask.ActionText)
	}

	// A script run needs a machine whose zm runs scripts: studio does.
	sask := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "ship-fix-script", Owner: "human:bea",
		Card: card("Ship the analytics fix to the live app now?", &orchapi.AskAction{Kind: orchapi.ActionRun, Script: "scripts/deploy.sh", Ref: "main", Commit: strings.Repeat("a", 40)})}))(t)
	if sask.Card.Action == nil || sask.Card.Action.Machine != "studio" {
		t.Fatalf("script, defaulted machine: %+v", sask.Card.Action)
	}

	// A command defaults the same way, to a machine whose zm
	// runs commands: studio does.
	if !byName["studio"].CommandRuns || byName["mac"].CommandRuns {
		t.Fatalf("command runs: studio %+v mac %+v", byName["studio"], byName["mac"])
	}
	cask := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "ship-fix-command", To: "human:bea",
		Card: card("Ship the analytics fix to the live app now?", &orchapi.AskAction{Kind: orchapi.ActionCommand, Command: "make deploy", Ref: "main", Commit: strings.Repeat("a", 40)})}))(t)
	if cask.Card.Action == nil || cask.Card.Action.Kind != orchapi.ActionCommand || cask.Card.Action.Machine != "studio" || !strings.Contains(cask.ActionText, "on studio") {
		t.Fatalf("command, defaulted machine: %+v %q", cask.Card.Action, cask.ActionText)
	}

	// No person named: no guess.
	_, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "ship-fix-nobody",
		Card: card("Ship the analytics fix to the live app now?", &orchapi.AskAction{Kind: orchapi.ActionRun, Agent: "claude", Brief: brief})})
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "names its machine") {
		t.Fatalf("no person: %v", err)
	}

	// A person with no ready machine: the list says how to get one, and the
	// ask is refused with why.
	laptop := join("laptop", "dana", "member", roomy)
	heartbeat(t, laptop)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "dana", Project: "harbor", Role: "owner"}))(t)
	if l := mustf(orchclient.RunTargets(ctx, agent, "dana"))(t); len(l.Targets) != 1 || l.Targets[0].Ready || !strings.Contains(l.Hint, "remote_tasks") {
		t.Fatalf("dana's: %+v", l)
	}
	_, err = orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "ship-fix-dana", Owner: "human:dana",
		Card: card("Ship the analytics fix to the live app now?", &orchapi.AskAction{Kind: orchapi.ActionRun, Agent: "claude", Brief: brief})})
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "dana has no machine in harbor") || !strings.Contains(err.Error(), "laptop takes no tasks") {
		t.Fatalf("dana, no ready machine: %v", err)
	}

	// Two ready machines: named, never picked.
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "box", Project: "harbor", Role: "owner"}))(t)
	desk := join("desk", "box", "member", takes)
	heartbeat(t, desk)
	_, err = orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "ship-fix-box", Owner: "human:box",
		Card: card("Ship the analytics fix to the live app now?", &orchapi.AskAction{Kind: orchapi.ActionRun, Agent: "claude", Brief: brief})})
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "2 machines") || !strings.Contains(err.Error(), "box, desk") {
		t.Fatalf("two machines: %v", err)
	}
	_, err = orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "ship-fix-box-command", Owner: "human:box",
		Card: card("Ship the analytics fix to the live app now?", &orchapi.AskAction{Kind: orchapi.ActionCommand, Command: "make deploy", Ref: "main", Commit: strings.Repeat("a", 40)})})
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "2 machines") {
		t.Fatalf("two machines, command: %v", err)
	}

	// A chore card for Bea: the judge reads his machines and sends it back;
	// the refusal points at studio and the flags.
	var svc *approvals.Service
	for _, m := range a.dm.Modules() {
		if s, ok := m.(*approvals.Service); ok {
			svc = s
		}
	}
	var seen string
	svc.SetJudge(judge.Fake{Fn: func(prompt string) (json.RawMessage, error) {
		seen = prompt
		return json.RawMessage(`{"decidable":true,"person_step":true,"step_carried":false,"brief_needed":false,"brief_complete":true,"asks_decision":true,"ruled_by":"","repeat_of":"","risk":"none","pass":false,"fixes":["Choice A asks Bea to deploy: carry it as a run action"]}`), nil
	}})
	_, err = orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "ship-fix-chore", Owner: "human:bea",
		Card: card("Can you deploy the analytics fix to the live app?", nil)})
	svc.SetJudge(nil)
	var e *api.Error
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "asks Bea to deploy") {
		t.Fatalf("chore: %v", err)
	}
	if !errors.As(err, &e) || !strings.Contains(e.Hint, "--run-on studio") || !strings.Contains(e.Hint, "--to human:bea") {
		t.Fatalf("chore hint: %+v", e)
	}
	if !strings.Contains(seen, `"approver": "bea"`) || !strings.Contains(seen, `"machine": "studio"`) {
		t.Fatalf("the judge saw: %s", seen)
	}

	// A report asking Bea to log in: refused from the replica and at the
	// authority, each by its own daemon's mail-chore judge (scripted here;
	// the model's cases are orchapi's testdata/mailchore); one with nothing
	// to do passes; a judge that cannot answer holds it, never sends it.
	choreJudge := judge.Fake{Fn: func(prompt string) (json.RawMessage, error) {
		if _, in, _ := strings.Cut(prompt, "<input>"); strings.Contains(in, "You need to log in") {
			return json.RawMessage(`{"pass":false,"fixes":["Log in yourself and report the numbers."],"reason":"It asks Bea to log in."}`), nil
		}
		return json.RawMessage(`{"pass":true,"fixes":[],"reason":"It only reports."}`), nil
	}}
	var authMail *mail.Service
	for _, m := range a.dm.Modules() {
		if s, ok := m.(*mail.Service); ok {
			authMail = s
		}
	}
	var replicaTeam *team.Service
	for _, m := range mac.dm.Modules() {
		if s, ok := m.(*team.Service); ok {
			replicaTeam = s
		}
	}
	authMail.SetJudge(choreJudge)
	replicaTeam.SetJudge(choreJudge)
	chore := orchapi.SendMailRequest{To: "human:bea", Kind: orchapi.MailKindReport, Body: "The fix is merged. You need to log in to the analytics site and run the migration."}
	if _, err := orchclient.Send(ctx, agent, chore); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "asks bea to do a step (It asks Bea to log in)") {
		t.Fatalf("report from the replica: %v", err)
	}
	asp := mustf(orchclient.Spawn(ctx, at(a.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: t.TempDir()}))(t)
	if _, err := orchclient.Send(ctx, at(a.owner, "harbor", asp.Address), chore); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "zm ask machines") {
		t.Fatalf("report at the authority: %v", err)
	}
	mustf(orchclient.Send(ctx, agent, orchapi.SendMailRequest{To: "human:bea", Kind: orchapi.MailKindReport, Body: "Shipped the analytics fix; I ran the migration and the events arrive."}))(t)
	replicaTeam.SetJudge(judge.Fake{Err: errors.New("claude: not on PATH")})
	if _, err := orchclient.Send(ctx, agent, orchapi.SendMailRequest{To: "human:bea", Kind: orchapi.MailKindReport, Body: "Shipped it."}); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("held at the replica: %v", err)
	}
}
