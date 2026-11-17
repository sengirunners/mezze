package team_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// An action ask whose run-on machine is the asking session's own
// machine (an agent on Ivo's Mac asks to run something on that Mac). The
// target refused it ("the parent must be a session on another machine")
// while the ask read "queued" forever, and zm ask check refused to record
// the manual run that finished the work. Now: the approved action runs there
// and reaches done; an action its machine refuses fails, saying why, instead
// of sitting queued; and the asker's passing check on a failed action records
// DONE.
func TestAskActionOnTheAskersOwnMachine(t *testing.T) {
	quietPresence(t)
	// A step that holds starts on the authority; installed before any daemon (and undone after they stop:
	// cleanups run last-first) so no daemon goroutine reads the hook while it changes.
	var hold atomic.Bool
	entered, release := make(chan struct{}, 1), make(chan struct{})
	t.Cleanup(team.SetRunHereHook(func() {
		if hold.Load() {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		}
	}))
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

	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard,
		func(c *config.Config) { roomy(c); c.File.RemoteTasks = true })
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	mac := startDaemonWith(t, tn, "mac", tailnettest.Identity{NodeID: "nMAC", NodeKey: "nodekey:mac", Login: "mac@example.com", Host: "mac"}, discard,
		func(c *config.Config) { roomy(c); c.File.RemoteTasks = true })
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "mac", Role: "member"}))(t)
	mustf(mac.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	// Bea, the project's second owner, approves on his phone.
	laptop := startDaemonWith(t, tn, "laptop", tailnettest.Identity{NodeID: "nLAPTOP", NodeKey: "nodekey:laptop", Login: "bea@example.com", Host: "laptop"}, discard, roomy)
	inv = mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	mustf(laptop.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Project: "harbor", Role: "owner"}))(t)
	// The asking agent runs on the Mac, in the project's checkout there.
	ss := mustf(orchclient.Spawn(ctx, at(mac.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: macClone}))(t)
	agent := at(mac.owner, "harbor", ss.Address)
	heartbeat(t, mac)
	it := mustf(orchclient.CreateWork(ctx, at(mac.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "Send the Stripe email"}))(t)

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
	card := func(question string, act *orchapi.AskAction) *orchapi.AskCard {
		return &orchapi.AskCard{Context: "The customer is waiting on the Stripe email; the draft is ready on the Mac.",
			Question: question, Choices: []orchapi.AskChoice{{Label: "Yes, send it"}, {Label: "No, not now"}},
			Recommend: "a", Why: "The draft was reviewed this morning.", Searched: "the work item's notes: nothing else pending", Action: act}
	}
	state := func(id, want string) orchapi.Ask {
		t.Helper()
		var got orchapi.Ask
		eventually(t, 30*time.Second, func() error {
			heartbeat(t, mac)
			svc.WatchActions(ctx)
			got = mustf(orchclient.GetAsk(ctx, a.owner, id))(t)
			if got.ActionState == nil || got.ActionState.State != want {
				return fmt.Errorf("action state %+v, want %s", got.ActionState, want)
			}
			return nil
		})
		return got
	}

	// 1. Run on the asker's own machine: it runs there and is done, its
	// output mailed to the asking session.
	line := `echo "zm-ran1 $(git rev-parse HEAD)" | tr a-z A-Z`
	ask := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "same-machine",
		Card: card("Send the Stripe email from the Mac?", &orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: "mac", Command: line, Ref: "main", Commit: pushed})}))(t)
	mustf(svc.Decide(ctx, beaPhone, ask.ID, "a"))(t)
	got := state(ask.ID, orchapi.ActionDone)
	if !strings.HasPrefix(got.ActionStateText, "Done on mac") {
		t.Fatalf("state text %q", got.ActionStateText)
	}
	out := "ZM-RAN1 " + strings.ToUpper(pushed)
	eventually(t, 20*time.Second, func() error {
		heartbeat(t, mac)
		for _, m := range mustf(orchclient.Inbox(ctx, agent, orchapi.InboxQuery{}))(t) {
			if strings.Contains(m.Body, out) {
				return nil
			}
		}
		return fmt.Errorf("no result mail with %q for the asking session", out)
	})

	// 1b. An asker stopped before the yes (its stop marks its tasks to stop):
	// the approved action is not the asker's to stop, and still runs.
	ss2 := mustf(orchclient.Spawn(ctx, at(mac.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: macClone}))(t)
	ask1b := mustf(orchclient.NewAsk(ctx, at(mac.owner, "harbor", ss2.Address), orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "asker-stopped",
		Card: card("Send the follow-up email from the Mac?", &orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: "mac", Command: "true", Ref: "main", Commit: pushed})}))(t)
	mustf(orchclient.Stop(ctx, at(mac.owner, "harbor", ""), ss2.ID, orchapi.StopRequest{Escalate: true, Force: true}))(t)
	mustf(svc.Decide(ctx, beaPhone, ask1b.ID, "a"))(t)
	state(ask1b.ID, orchapi.ActionDone)

	// Its asker stopped while it runs: the action runs on to done.
	ss3 := mustf(orchclient.Spawn(ctx, at(mac.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: macClone}))(t)
	goFile := filepath.Join(root, "go")
	ask1c := mustf(orchclient.NewAsk(ctx, at(mac.owner, "harbor", ss3.Address), orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "asker-stopped-midway",
		Card: card("Send the reminder email from the Mac?", &orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: "mac", Command: "i=0; while [ ! -f '" + goFile + "' ] && [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done", Ref: "main", Commit: pushed})}))(t)
	mustf(svc.Decide(ctx, beaPhone, ask1c.ID, "a"))(t)
	state(ask1c.ID, orchapi.ActionRunning)
	mustf(orchclient.Stop(ctx, at(mac.owner, "harbor", ""), ss3.ID, orchapi.StopRequest{Escalate: true, Force: true}))(t)
	time.Sleep(500 * time.Millisecond) // the stop has landed; the action is held by a file the test controls
	state(ask1c.ID, orchapi.ActionRunning)
	if err := os.WriteFile(goFile, nil, 0600); err != nil {
		t.Fatal(err)
	}
	state(ask1c.ID, orchapi.ActionDone)

	// A plain start (not an ask's) still never names its parent's own
	// machine through the authority: the target refuses it.
	_, err := sessionModule(t, mac).StartRemoteTask(ctx, ss.Project, orchapi.RemoteTaskPrefix+"not-an-ask", orchapi.TaskStartOp{Parent: ss.Address, Depth: 1,
		Request: orchapi.TaskStartRequest{Agent: "claude", Prompt: "hello"}})
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "another machine") {
		t.Fatalf("a same-machine parent without the ask mark: %v", err)
	}

	// 2. The machine refuses the start (main moved past the frozen commit):
	// the action fails with the machine's reason, never stays queued.
	verified := card("Send the second Stripe email from the Mac?", &orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: "mac", Command: "true", Ref: "main", Commit: pushed})
	verified.Verify = &orchapi.AskVerify{Command: "test -f sent.txt"}
	ask2 := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "refused-then-manual", Card: verified}))(t)
	if err := os.WriteFile(filepath.Join(seed, "README"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, seed, "commit", "--quiet", "-am", "v2")
	gitT(t, seed, "push", "--quiet", "origin", "main")
	pushed2 := gitT(t, seed, "rev-parse", "HEAD")
	mustf(svc.Decide(ctx, beaPhone, ask2.ID, "a"))(t)
	got = state(ask2.ID, orchapi.ActionFailed)
	if !strings.Contains(got.ActionState.Reason, "is not what main resolves to") || got.NotDone {
		t.Fatalf("refused: %+v not_done %v", got.ActionState, got.NotDone)
	}

	// 3. The asker did the work by hand: a failing check changes nothing, a
	// passing one records DONE (verified) on the ask and the item.
	failed := 1
	got = mustf(orchclient.CheckAsk(ctx, agent, ask2.ID, orchapi.CheckRequest{CardSHA: ask2.CardSHA, Exit: &failed, Output: "no sent.txt"}))(t)
	if got.ActionState.State != orchapi.ActionFailed || got.ActionState.CheckExit == nil || *got.ActionState.CheckExit != 1 {
		t.Fatalf("failing check after a failed action: %+v", got.ActionState)
	}
	passed := 0
	got = mustf(orchclient.CheckAsk(ctx, agent, ask2.ID, orchapi.CheckRequest{CardSHA: ask2.CardSHA, Exit: &passed}))(t)
	if got.ActionState.State != orchapi.ActionDone || got.ActionState.VerifiedAt == nil || got.NotDone {
		t.Fatalf("passing check after a failed action: %+v", got.ActionState)
	}
	if !strings.Contains(got.ActionStateText, "check passed") {
		t.Fatalf("done by hand reads %q", got.ActionStateText)
	}

	// Work its asker gave up is failed for good: a check does not turn it
	// into DONE.
	gave := card("Send the third Stripe email from the Mac?", &orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: "mac", Command: "true", Ref: "main", Commit: pushed2})
	gave.Verify = &orchapi.AskVerify{Command: "test -f sent.txt"}
	ask3 := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "given-up", Card: gave}))(t)
	mustf(svc.Decide(ctx, beaPhone, ask3.ID, "a"))(t)
	state(ask3.ID, orchapi.ActionApproved)
	got = mustf(orchclient.CheckAsk(ctx, agent, ask3.ID, orchapi.CheckRequest{CardSHA: ask3.CardSHA, GiveUp: "not needed any more"}))(t)
	if got.ActionState.State != orchapi.ActionFailed {
		t.Fatalf("given up: %+v", got.ActionState)
	}
	if _, err := orchclient.CheckAsk(ctx, agent, ask3.ID, orchapi.CheckRequest{CardSHA: ask3.CardSHA, Exit: &passed}); !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "failed for good") {
		t.Fatalf("a check after giving up: %v", err)
	}
	if _, err := orchclient.CheckAsk(ctx, agent, ask2.ID, orchapi.CheckRequest{CardSHA: ask2.CardSHA, Exit: &passed}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a second check on done work: %v", err)
	}
	eventually(t, 10*time.Second, func() error {
		svc.WatchNotDone(ctx)
		for _, c := range mustf(orchclient.Comments(ctx, a.owner, it.ID))(t) {
			if strings.HasPrefix(c.Body, "DONE (verified ") && strings.Contains(c.Body, ask2.ID) {
				return nil
			}
		}
		return fmt.Errorf("no DONE (verified) comment for %s", ask2.ID)
	})

	// An approved command runs "as them". On the approver's own machine its zm calls act as that
	// machine's human (the approver at the authority), so an owner-only zm ask check --evidence on the card
	// succeeds; on anyone else's it stays its task session, never borrowing that machine's person.
	whoLine := func(tag string) string { return `echo "zm-who-` + tag + ` as=$ZM_AS session=${ZM_SESSION:-none}"` }
	ranAs := func(tag string) string {
		t.Helper()
		var as string
		mark := "zm-who-" + tag + " as="
		eventually(t, 20*time.Second, func() error {
			heartbeat(t, mac)
			for _, m := range mustf(orchclient.Inbox(ctx, agent, orchapi.InboxQuery{}))(t) {
				if i := strings.Index(m.Body, mark); i >= 0 {
					as = strings.Fields(m.Body[i+len(mark):])[0]
					if !strings.Contains(m.Body, "session=none") && strings.HasPrefix(as, "human:") {
						return fmt.Errorf("ran as %s but kept its session: %q", as, m.Body)
					}
					return nil
				}
			}
			return fmt.Errorf("no result mail with %q", mark)
		})
		return as
	}
	ask5 := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "as-bea-on-mac",
		Card: card("Send the Stripe receipt from the Mac?", &orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: "mac", Command: whoLine("bea"), Ref: "main", Commit: pushed2})}))(t)
	mustf(svc.Decide(ctx, beaPhone, ask5.ID, "a"))(t)
	state(ask5.ID, orchapi.ActionDone)
	if as := ranAs("bea"); !strings.HasPrefix(as, "mac/") {
		t.Fatalf("bea's yes on mac's machine ran as %q, want its task session", as)
	}
	// The Mac's person owns the project too and approves on their own phone.
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "mac", Project: "harbor", Role: "owner"}))(t)
	macPhone := &core.Remote{Node: "mac-phone", NodeID: "nMACPHONE", NodeKey: "nodekey:macphone"}
	code = mustf(svc.AddFor(ctx, "test", "mac-phone", "mac"))(t)
	if ap := mustf(svc.Pair(ctx, macPhone, code.Code))(t); ap.State != orchapi.ApproverActive {
		mustf(svc.ConfirmNode(ctx, beaPhone, "mac-phone", ap.Node))(t) // an active approver confirms a new one
	}
	ask6 := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "as-mac-on-mac",
		Card: card("Send the second Stripe receipt from the Mac?", &orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: "mac", Command: whoLine("mac"), Ref: "main", Commit: pushed2})}))(t)
	mustf(svc.Decide(ctx, macPhone, ask6.ID, "a"))(t)
	state(ask6.ID, orchapi.ActionDone)
	as := ranAs("mac")
	if as != "human:mac-human" {
		t.Fatalf("mac's yes on its own machine ran as %q, want human:mac-human", as)
	}
	// What that identity may do: record another session's approved work done with evidence, which its task
	// session (the bug's caller) may not.
	other := mustf(orchclient.Spawn(ctx, at(mac.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: macClone}))(t)
	owed := card("Send the third Stripe receipt from the Mac?", &orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: "mac", Command: "true", Ref: "main", Commit: pushed2})
	owed.Verify = &orchapi.AskVerify{Command: "test -f receipt.txt"}
	ask7 := mustf(orchclient.NewAsk(ctx, at(mac.owner, "harbor", other.Address), orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "evidence-as-approver", Card: owed}))(t)
	mustf(svc.Decide(ctx, macPhone, ask7.ID, "a"))(t)
	state(ask7.ID, orchapi.ActionApproved)
	ev := orchapi.CheckRequest{CardSHA: ask7.CardSHA, Evidence: "the receipt is in the customer's inbox"}
	if _, err := orchclient.CheckAsk(ctx, agent, ask7.ID, ev); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("evidence from a session that did not ask: %v", err)
	}
	if got := mustf(orchclient.CheckAsk(ctx, at(mac.owner, "harbor", as), ask7.ID, ev))(t); got.ActionState.State != orchapi.ActionDone {
		t.Fatalf("evidence as the approver: %+v", got.ActionState)
	}

	// An action that runs on the authority itself starts off the answer. The start (a fetch, a worktree,
	// a spawn) ran inside the owner's answer request, under the locks every other answer waits on, so a slow one
	// outlasted the Mezze app's timeout and the next owner's answer was lost. Held here, the answer still returns at once.
	hold.Store(true)
	unhold := sync.OnceFunc(func() { close(release) })
	t.Cleanup(unhold) // registered after the daemons, so it runs before they close and wait on a held start
	ask4 := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "on-the-authority",
		Card: card("Send the Stripe email from mini?", &orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: "mini", Command: "true", Ref: "main", Commit: pushed2})}))(t)
	answered := make(chan error, 1)
	go func() { _, err := svc.Decide(ctx, beaPhone, ask4.ID, "a"); answered <- err }()
	select {
	case err := <-answered:
		if err != nil {
			t.Fatalf("answer: %v", err)
		}
	case <-time.After(5 * time.Second):
		unhold()
		t.Fatal("the answer waited on its action's start on the authority")
	}
	select {
	case <-entered: // the start was placed here, and runs off the answer
	case <-time.After(20 * time.Second):
		t.Fatal("the action never started on mini")
	}
	unhold()
	eventually(t, 30*time.Second, func() error {
		svc.WatchActions(ctx)
		got := mustf(orchclient.GetAsk(ctx, a.owner, ask4.ID))(t)
		if got.ActionState == nil || got.ActionState.State == orchapi.ActionApproved || got.ActionState.State == orchapi.ActionQueued {
			return fmt.Errorf("action state %+v: still waiting to start", got.ActionState)
		}
		return nil
	})
}
