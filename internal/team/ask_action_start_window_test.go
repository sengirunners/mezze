package team_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// An owner's yes on a run action whose machine cannot start it
// (busy, at its session limit, offline) waited in the line for hours and
// then failed, or ran, when nobody expected it. Now it fails once its
// machine has not started it within the start window, saying why, and the
// line never runs it later. With no verify check on the card, an owner who
// did the work by hand records it done with evidence.
func TestAskActionStartWindow(t *testing.T) {
	quietPresence(t)
	t.Cleanup(team.SetAskStartWithin(2 * time.Second)) // undone after the daemons stop
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	gitT(t, root, "init", "--bare", "--initial-branch=main", remote)
	seed := filepath.Join(root, "seed")
	gitT(t, root, "clone", "--quiet", remote, seed)
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, seed, "add", ".")
	gitT(t, seed, "commit", "--quiet", "-m", "v1")
	gitT(t, seed, "push", "--quiet", "origin", "main")
	pushed := gitT(t, seed, "rev-parse", "HEAD")
	macClone := filepath.Join(root, "mac-clone")
	gitT(t, root, "clone", "--quiet", remote, macClone)

	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, roomy)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	// The Mac takes remote tasks but runs one managed session at most: the
	// asking agent fills it, so the approved action cannot start there.
	mac := startDaemonWith(t, tn, "mac", tailnettest.Identity{NodeID: "nMAC", NodeKey: "nodekey:mac", Login: "mac@example.com", Host: "mac"}, discard,
		func(c *config.Config) { roomy(c); c.File.RemoteTasks = true; c.File.Admission.MaxManagedSessions = 1 })
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "mac", Role: "member"}))(t)
	mustf(mac.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	laptop := startDaemonWith(t, tn, "laptop", tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "bea@example.com", Host: "laptop"}, discard, roomy)
	inv = mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	mustf(laptop.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Project: "harbor", Role: "owner"}))(t)
	ss := mustf(orchclient.Spawn(ctx, at(mac.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: macClone}))(t)
	agent := at(mac.owner, "harbor", ss.Address)
	heartbeat(t, mac)
	it := mustf(orchclient.CreateWork(ctx, at(mac.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "Rotate the hub token"}))(t)

	var svc *approvals.Service
	for _, m := range a.dm.Modules() {
		if s, ok := m.(*approvals.Service); ok {
			svc = s
		}
	}
	code := mustf(svc.AddFor(ctx, "test", "bea-phone", "bea"))(t)
	if ap := mustf(svc.Pair(ctx, beaPhone, code.Code))(t); ap.State != orchapi.ApproverActive {
		mustf(svc.ConfirmNode(ctx, beaPhone, "bea-phone", ap.Node))(t)
	}

	ran := filepath.Join(root, "ran")
	ask := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "rotate",
		Card: &orchapi.AskCard{Context: "The hub's GitHub token expires tonight; the new one is in the Mac's keychain.",
			Question: "Rotate the hub's GitHub token from the Mac now?", Choices: []orchapi.AskChoice{{Label: "Yes, rotate it"}, {Label: "No, not now"}},
			Recommend: "a", Why: "Watches stop when the token expires.", Searched: "the work item's notes: nothing else pending",
			Action: &orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: "mac", Command: "touch '" + ran + "'", Ref: "main", Commit: pushed}}}))(t)
	mustf(svc.Decide(ctx, beaPhone, ask.ID, "a"))(t)
	// A second card the same way, which is started again once there is room.
	ran2 := filepath.Join(root, "ran2")
	ask2 := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "rotate-ci",
		Card: &orchapi.AskCard{Context: "The CI token expires tonight too; the new one is in the Mac's keychain.",
			Question: "Rotate the CI token from the Mac now?", Choices: []orchapi.AskChoice{{Label: "Yes, rotate it"}, {Label: "No, not now"}},
			Recommend: "a", Why: "Builds stop when the token expires.", Searched: "the work item's notes: nothing else pending",
			Action: &orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: "mac", Command: "touch '" + ran2 + "'", Ref: "main", Commit: pushed}}}))(t)
	mustf(svc.Decide(ctx, beaPhone, ask2.ID, "a"))(t)

	var got, got2 orchapi.Ask
	eventually(t, 30*time.Second, func() error {
		heartbeat(t, mac)
		svc.WatchActions(ctx)
		got = mustf(orchclient.GetAsk(ctx, a.owner, ask.ID))(t)
		got2 = mustf(orchclient.GetAsk(ctx, a.owner, ask2.ID))(t)
		for _, g := range []orchapi.Ask{got, got2} {
			if g.ActionState == nil || g.ActionState.State != orchapi.ActionFailed {
				return fmt.Errorf("action state %+v, want failed", g.ActionState)
			}
		}
		return nil
	})
	// The line shows it was never handed to the Mac: it may start again, and
	// the approver hears so in plain words.
	if x := got2.ActionState; !x.Retryable || x.RetryUntil == nil {
		t.Fatalf("a start its machine never took is not retryable: %+v", x)
	}
	eventually(t, 10*time.Second, func() error {
		svc.WatchActions(ctx)
		for _, m := range mustf(orchclient.Mail(ctx, a.owner, orchapi.MailQuery{}))(t) {
			if m.To == "human:bea" && strings.Contains(m.Body, ask2.ID) && strings.Contains(m.Body, "nothing ran") {
				return nil
			}
		}
		return fmt.Errorf("no notice to the approver")
	})
	// No plain start, even an owner's session's, takes an action's request
	// id (a planted start would be followed as the action), nor pins a run
	// to an older commit itself.
	planter := mustf(orchclient.Spawn(ctx, at(a.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: t.TempDir()}))(t)
	owners := at(a.owner, "harbor", planter.Address)
	if _, err := orchclient.TaskStartIn(ctx, owners, "harbor", orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun, Machine: "mac", ClientRequestID: "ask-" + strings.ToLower(ask2.ID) + "-1",
		Run: &orchapi.TaskRun{Ref: "main", Commit: pushed, Command: "true"}}); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "kept for approved asks") {
		t.Fatalf("a plain start under an action's request id: %v", err)
	}
	if _, err := orchclient.TaskStartIn(ctx, owners, "harbor", orchapi.TaskStartRequest{Agent: orchapi.TaskAgentRun, Machine: "mac",
		Run: &orchapi.TaskRun{Ref: "main", Commit: pushed, CommitOnRef: true, Command: "true"}}); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "commit_on_ref") {
		t.Fatalf("a plain start with commit_on_ref: %v", err)
	}
	// Three windows of 2s from its start before it fails.
	if r := got.ActionState.Reason; !strings.Contains(r, "mac did not take it within 6s (busy") || !strings.Contains(r, "zm cancelled it before it ran") {
		t.Fatalf("reason %q", r)
	}

	// The asking agent cannot vouch for its own failed action (no check).
	if _, err := orchclient.CheckAsk(ctx, agent, ask.ID, orchapi.CheckRequest{CardSHA: ask.CardSHA, Evidence: "done by hand"}); !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "a person who owns") {
		t.Fatalf("the asker's own evidence on a no-check ask: %v", err)
	}

	// Nor may an agent session on the owner's own socket.
	own := mustf(orchclient.Spawn(ctx, at(a.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: t.TempDir()}))(t)
	if _, err := orchclient.CheckAsk(ctx, at(a.owner, "harbor", own.Address), ask.ID, orchapi.CheckRequest{CardSHA: ask.CardSHA, Evidence: "done by hand"}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an owner's agent session vouching on a no-check ask: %v", err)
	}

	// Room on the Mac later: the cancelled start never runs.
	mustf(orchclient.Stop(ctx, at(mac.owner, "harbor", ""), ss.ID, orchapi.StopRequest{Escalate: true, Force: true}))(t)
	for i := 0; i < 10; i++ {
		heartbeat(t, mac)
		time.Sleep(200 * time.Millisecond)
	}
	if _, err := os.Stat(ran); err == nil {
		t.Fatal("the expired action ran after all")
	}
	if _, err := os.Stat(ran2); err == nil {
		t.Fatal("the second expired action ran after all")
	}
	// Its asker starts the second again, identical, without a new card: it
	// runs once, now that the Mac has room.
	eventually(t, 60*time.Second, func() error {
		heartbeat(t, mac)
		for _, m := range mustf(a.owner.Machines(ctx))(t).Machines {
			if m.Machine == "mac" && m.Stats.Sessions > 0 {
				return fmt.Errorf("mac still runs %d sessions", m.Stats.Sessions)
			}
		}
		return nil
	})
	mustf(orchclient.RetryAction(ctx, agent, "", ask2.ID, ask2.CardSHA))(t)
	eventually(t, 30*time.Second, func() error {
		heartbeat(t, mac)
		svc.WatchActions(ctx)
		g := mustf(orchclient.GetAsk(ctx, a.owner, ask2.ID))(t)
		if g.ActionState.State != orchapi.ActionDone {
			return fmt.Errorf("retried action %+v", g.ActionState)
		}
		return nil
	})
	if _, err := os.Stat(ran2); err != nil {
		t.Fatalf("the retried action did not run: %v", err)
	}
	if _, err := orchclient.RetryAction(ctx, agent, "", ask2.ID, ask2.CardSHA); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a retry of a done action: %v", err)
	}

	// Later reads keep the same reason.
	svc.WatchActions(ctx)
	if again := mustf(orchclient.GetAsk(ctx, a.owner, ask.ID))(t); again.ActionState.Reason != got.ActionState.Reason {
		t.Fatalf("reason changed: %q then %q", got.ActionState.Reason, again.ActionState.Reason)
	}

	// An owner who did the work by hand records it done with evidence.
	ev := "wrangler secret list shows HUB_GITHUB_TOKEN set today"
	got = mustf(orchclient.CheckAsk(ctx, a.owner, ask.ID, orchapi.CheckRequest{CardSHA: ask.CardSHA, Evidence: ev}))(t)
	if x := got.ActionState; x.State != orchapi.ActionDone || x.DoneEvidence != ev || got.NotDone {
		t.Fatalf("done with evidence: %+v", x)
	}
	if !strings.Contains(got.ActionStateText, "recorded it with evidence") {
		t.Fatalf("state text %q", got.ActionStateText)
	}
}
