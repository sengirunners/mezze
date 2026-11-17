package team_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
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
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// sshKeyLine is a fresh ed25519 public key as one authorized_keys line.
func sshKeyLine(t *testing.T, comment string) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var blob []byte
	for _, part := range [][]byte{[]byte("ssh-ed25519"), pub} {
		blob = binary.BigEndian.AppendUint32(blob, uint32(len(part)))
		blob = append(blob, part...)
	}
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(blob) + " " + comment
}

// Asks that carry an action (SPEC 16): an agent on Bea's Mac asks to add
// Ivo's key on the box. The owner's socket cannot pick that choice;
// Bea, the project's second owner, approves it on his phone; the box adds
// exactly that line to its own user's authorized_keys (once), and the ask
// shows done with the open asks until an owner confirms it. An approver who
// does not own the project fails the action, which tells the asker. A script
// run approved the same way runs on the box and shows done.
func TestAskActionsRunAsTheApprover(t *testing.T) {
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

	// The project's remote at a pushed commit, and the box's clone of it.
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	gitT(t, root, "init", "--bare", "--initial-branch=main", remote)
	seed := filepath.Join(root, "seed")
	gitT(t, root, "clone", "--quiet", remote, seed)
	if err := os.MkdirAll(filepath.Join(seed, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "scripts", "deploy.sh"), []byte("#!/bin/sh\necho \"deployed $1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, seed, "add", ".")
	gitT(t, seed, "commit", "--quiet", "-m", "deploy")
	gitT(t, seed, "push", "--quiet", "origin", "main")
	pushed := gitT(t, seed, "rev-parse", "HEAD")
	boxClone := filepath.Join(root, "box-clone")
	gitT(t, root, "clone", "--quiet", remote, boxClone)

	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, roomy)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	join := func(machine, as, role string, mut func(*config.Config)) *daemon {
		d := startDaemonWith(t, tn, machine, tailnettest.Identity{NodeID: "n" + strings.ToUpper(machine), NodeKey: "nodekey:" + machine, Login: as + "@example.com", Host: machine}, discard, mut)
		inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: as, Role: role}))(t)
		mustf(d.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
		return d
	}
	box := join("box", "box", "member", func(c *config.Config) { roomy(c); c.File.RemoteTasks = true })
	mac := join("mac", "bea", "member", roomy)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Project: "harbor", Role: "owner"}))(t)
	mustf(orchclient.Spawn(ctx, at(box.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: boxClone}))(t)
	ss := mustf(orchclient.Spawn(ctx, at(mac.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: t.TempDir()}))(t)
	agent := at(mac.owner, "harbor", ss.Address)
	heartbeat(t, box)
	heartbeat(t, mac)
	it := mustf(orchclient.CreateWork(ctx, at(mac.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "Ivo needs to reach the box"}))(t)

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
			mustf(svc.ConfirmNode(ctx, beaPhone, name, ap.Node))(t)
		}
	}
	pair("bea-phone", "bea", beaPhone)

	key := sshKeyLine(t, "ivo@macbook")
	card := func(question string, act *orchapi.AskAction) *orchapi.AskCard {
		return &orchapi.AskCard{Context: "Ivo's laptop cannot reach the build box, so he cannot check the nightly run himself.",
			Question: question, Choices: []orchapi.AskChoice{{Label: "Yes, allow it"}, {Label: "No, not now"}},
			Recommend: "a", Why: "He owns the project and needs the box daily.", Searched: "the access notes on the work item: nothing yet", Action: act}
	}
	keyAction := &orchapi.AskAction{Kind: orchapi.ActionAuthorizeKey, Machine: "box", User: me.Username, Key: key}

	// Refused at creation: no machine, auto, another user, a browser
	// action, a session elsewhere for a run.
	refusals := []struct {
		name string
		c    *orchapi.AskCard
		code string
		want string
	}{
		{"auto", card("Let Ivo's laptop reach the box?", &orchapi.AskAction{Kind: orchapi.ActionAuthorizeKey, Machine: "auto", User: me.Username, Key: key}), api.CodeBadRequest, "never auto"},
		{"user", card("Let Ivo's laptop reach the box?", &orchapi.AskAction{Kind: orchapi.ActionAuthorizeKey, Machine: "box", User: "root", Key: key}), api.CodeBadRequest, "zm runs as " + me.Username},
		{"machine", card("Let Ivo's laptop reach the box?", &orchapi.AskAction{Kind: orchapi.ActionAuthorizeKey, Machine: "mini", User: me.Username, Key: key}), api.CodeForbidden, "takes no tasks"},
		{"nowhere", card("Let Ivo's laptop reach the box?", &orchapi.AskAction{Kind: orchapi.ActionAuthorizeKey, Machine: "nowhere", User: me.Username, Key: key}), api.CodeNotFound, "nowhere"},
		{"browser", card("Let Ivo's laptop reach the box?", &orchapi.AskAction{Kind: orchapi.ActionBrowser, Machine: "box", Plan: "1. open the page",
			Scope: &orchapi.BrowserScope{Origins: []string{"https://example.com"}}}), api.CodeUnavailable, "browser actions do not run yet"},
		{"options", card("Let Ivo's laptop reach the box?", &orchapi.AskAction{Kind: orchapi.ActionAuthorizeKey, Machine: "box", User: me.Username, Key: "command=\"x\" " + key}), api.CodeBadRequest, "key"},
	}
	for _, c := range refusals {
		_, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "refused-" + c.name, Card: c.c})
		if !api.IsCode(err, c.code) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}

	ask := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "box-key",
		Card: card("Let Ivo's laptop reach the build box?", keyAction)}))(t)
	act := ask.Card.Action
	if act == nil || act.On != "a" || act.KeyFingerprint == "" || !strings.HasPrefix(act.KeyFingerprint, "SHA256:") || act.KeyComment != "ivo@macbook" || act.Key != key {
		t.Fatalf("frozen action: %+v", act)
	}
	if want := "zm adds this key to the " + me.Username + " account on box: " + act.KeyFingerprint; !strings.Contains(ask.ActionText, want) || !strings.Contains(ask.Body, want) {
		t.Fatalf("action text %q, body %q", ask.ActionText, ask.Body)
	}

	// The owner's socket cannot pick the choice that runs something; it may
	// still pick the other.
	if _, err := orchclient.Choose(ctx, a.owner, "", ask.ID, orchapi.DecideRequest{Decision: "a"}); !api.IsCode(err, api.CodeForbidden) || !strings.Contains(err.Error(), "paired device or in the Mezze app") {
		t.Fatalf("choose a: %v", err)
	}

	// Bea approves on his phone: the effect queues the op for the box as
	// Bea; the box takes it in its heartbeat and adds the key once.
	d := mustf(svc.Decide(ctx, beaPhone, ask.ID, "a"))(t)
	if !strings.HasPrefix(d.Receipt.Action, "authorize-key sha256=") || !strings.HasSuffix(d.Receipt.Action, " on=a") {
		t.Fatalf("receipt action %q", d.Receipt.Action)
	}
	got := mustf(orchclient.GetAsk(ctx, a.owner, ask.ID))(t)
	// The box may take, or finish, the op before this read; the key check below proves it ran.
	if st := got.ActionState; st == nil || st.Task == "" || (st.State != orchapi.ActionQueued && st.State != orchapi.ActionRunning && st.State != orchapi.ActionDone) {
		t.Fatalf("after the decision: %+v", got.ActionState)
	}
	keys := filepath.Join(sshDir, ".ssh", "authorized_keys")
	eventually(t, 15*time.Second, func() error {
		heartbeat(t, box)
		svc.WatchActions(ctx)
		got = mustf(orchclient.GetAsk(ctx, a.owner, ask.ID))(t)
		if got.ActionState == nil || got.ActionState.State != orchapi.ActionDone {
			return fmt.Errorf("action state %+v", got.ActionState)
		}
		return nil
	})
	b, err := os.ReadFile(keys)
	if err != nil || string(b) != key+"\n" {
		t.Fatalf("authorized_keys %q %v", b, err)
	}
	for path, mode := range map[string]os.FileMode{keys: 0o600, filepath.Dir(keys): 0o700} {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != mode {
			t.Fatalf("%s: %v %v", path, st.Mode(), err)
		}
	}
	if !strings.HasPrefix(got.ActionStateText, "Done on box") || got.ActionState.EndedAt == nil {
		t.Fatalf("state text %q %+v", got.ActionStateText, got.ActionState)
	}
	// Approved, not done: the yes recorded APPROVED, NOT DONE
	// on the item, and the action's success DONE, once.
	if got.NotDone {
		t.Fatalf("done action is still not done: %+v", got.ActionState)
	}
	itemComments := func() []string {
		cs := mustf(orchclient.Comments(ctx, at(mac.owner, "harbor", ""), it.ID))(t)
		out := make([]string, 0, len(cs))
		for _, c := range cs {
			out = append(out, c.Body)
		}
		return out
	}
	eventually(t, 10*time.Second, func() error {
		svc.WatchNotDone(ctx)
		notDone, done := 0, 0
		for _, c := range itemComments() {
			if strings.Contains(c, "APPROVED, NOT DONE: zm keeps ask "+ask.ID+" open in Decisions until the action reports success") {
				notDone++
			}
			if strings.HasPrefix(c, "DONE (") && strings.Contains(c, ask.ID) && strings.Contains(c, "finished ok") {
				done++
			}
		}
		if notDone != 1 || done != 1 {
			return fmt.Errorf("APPROVED, NOT DONE %d, DONE %d in %q", notDone, done, itemComments())
		}
		return nil
	})
	// Done and not confirmed: with the open asks; confirmed: answered.
	listed := func(id string) bool {
		in := mustf(orchclient.Decisions(ctx, a.owner, "", orchapi.AskOpen))(t)
		for _, x := range in.Decisions {
			if x.Ask != nil && x.Ask.ID == id {
				return true
			}
		}
		return false
	}
	if !listed(ask.ID) {
		t.Fatal("a done action nobody confirmed is not with the open asks")
	}
	conf := mustf(orchclient.ConfirmAction(ctx, a.owner, "", ask.ID))(t)
	if conf.ActionState == nil || conf.ActionState.ConfirmedBy == "" || conf.ActionState.ConfirmedAt == nil || listed(ask.ID) {
		t.Fatalf("confirmed: %+v", conf.ActionState)
	}

	// An approver who does not own the project: the action fails, on top of
	// the inbox, and the asker hears ACTION FAILED.
	boxPhone := &core.Remote{Node: "box-phone", NodeID: "nBOXPHONE", NodeKey: "nodekey:boxphone"}
	pair("box-phone", "box", boxPhone)
	ask2 := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "box-key-again",
		Card: card("Let Ivo's second laptop reach the build box?", &orchapi.AskAction{Kind: orchapi.ActionAuthorizeKey, Machine: "box", User: me.Username, Key: sshKeyLine(t, "ivo@air")})}))(t)
	mustf(svc.Decide(ctx, boxPhone, ask2.ID, "a"))(t)
	got = mustf(orchclient.GetAsk(ctx, a.owner, ask2.ID))(t)
	if got.ActionState == nil || got.ActionState.State != orchapi.ActionFailed || !strings.Contains(got.ActionState.Reason, "may not") {
		t.Fatalf("not an owner: %+v", got.ActionState)
	}
	in := mustf(orchclient.Decisions(ctx, a.owner, "", ""))(t)
	if len(in.Decisions) == 0 || in.Decisions[0].Ask == nil || in.Decisions[0].Ask.ID != ask2.ID {
		t.Fatalf("a failed action is not on top: %+v", in.Decisions)
	}
	eventually(t, 10*time.Second, func() error {
		svc.WatchActions(ctx)
		ms := mustf(orchclient.Inbox(ctx, agent, orchapi.InboxQuery{}))(t)
		for _, m := range ms {
			if strings.HasPrefix(m.Body, "ACTION FAILED: ") && strings.Contains(m.Body, ask2.ID) {
				return nil
			}
		}
		return fmt.Errorf("no ACTION FAILED mail for the asker: %+v", ms)
	})
	if b, _ := os.ReadFile(keys); string(b) != key+"\n" {
		t.Fatalf("a failed action changed authorized_keys: %q", b)
	}

	// A script run, approved by Bea: it runs on the box from the pushed
	// commit, its result goes to the asking session, and it shows done.
	run := &orchapi.AskAction{Kind: orchapi.ActionRun, Machine: "box", Script: "scripts/deploy.sh", Ref: "main", Commit: pushed, Args: []string{"the nightly"}}
	// It carries the asker's check: having run, it is approved, not done
	// until the check passes.
	runCard := card("Ship the nightly build to the box tonight?", run)
	runCard.Verify = &orchapi.AskVerify{Command: "curl -fsS https://box.example.com/nightly"}
	ask3 := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "nightly-deploy",
		Card: runCard}))(t)
	if ask3.Card.Verify == nil || ask3.Card.Verify.On != "a" {
		t.Fatalf("frozen verify %+v", ask3.Card.Verify)
	}
	if want := "zm runs this on box, as you: scripts/deploy.sh 'the nightly', from main at commit " + pushed; !strings.Contains(ask3.ActionText, want) {
		t.Fatalf("run text %q", ask3.ActionText)
	}
	mustf(svc.Decide(ctx, beaPhone, ask3.ID, "a"))(t)
	eventually(t, 20*time.Second, func() error {
		heartbeat(t, box)
		svc.WatchActions(ctx)
		got = mustf(orchclient.GetAsk(ctx, a.owner, ask3.ID))(t)
		if got.ActionState == nil || got.ActionState.State != orchapi.ActionApproved {
			return fmt.Errorf("run state %+v", got.ActionState)
		}
		return nil
	})
	if !strings.HasPrefix(got.ActionState.Task, orchapi.RemoteTaskPrefix) {
		t.Fatalf("run task %q", got.ActionState.Task)
	}
	if !got.NotDone || got.ActionState.EndedAt == nil || !strings.Contains(got.ActionStateText, "not done until the asker's check passes") {
		t.Fatalf("ran, check to pass: %+v %q", got.ActionState, got.ActionStateText)
	}
	passed := 0
	got = mustf(orchclient.CheckAsk(ctx, agent, ask3.ID, orchapi.CheckRequest{CardSHA: ask3.CardSHA, Exit: &passed}))(t)
	if got.ActionState.State != orchapi.ActionDone || got.NotDone || got.ActionState.VerifiedAt == nil {
		t.Fatalf("check passed: %+v", got.ActionState)
	}

	// A command action: one line, shown verbatim, refused like a script
	// would be when it carries a secret or changes user, and otherwise run
	// by Bea's yes in a clean checkout on the box, its output back to the
	// asking session and the work item.
	for name, line := range map[string]string{
		"secret": "gh auth login --with-token <<< ghp_abcdefghijklmnopqrstuvwxyz0123456789",
		"sudo":   "make && sudo make install",
		"lines":  "echo a\necho b",
	} {
		_, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "cmd-refused-" + name,
			Card: card("Run it on the box?", &orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: "box", Command: line, Ref: "main", Commit: pushed})})
		if !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("command %s: %v", name, err)
		}
	}
	if _, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "cmd-no-commit",
		Card: card("Run it on the box?", &orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: "box", Command: "ls", Ref: "main"})}); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("command without a commit: %v", err)
	}
	line := `test -x scripts/deploy.sh && echo "zm-cmd-ok $(git rev-parse HEAD)" | tr a-z A-Z; echo to-stderr >&2`
	ask4 := mustf(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "box-command",
		Card: card("Check the nightly checkout on the box?", &orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: "box", Command: line, Ref: "main", Commit: pushed})}))(t)
	if a := ask4.Card.Action; a == nil || a.Command != line || a.Script != "" || a.Commit != pushed {
		t.Fatalf("frozen command: %+v", a)
	}
	if want := "zm runs this command with /bin/sh on box, as you, in a clean checkout of main at commit " + pushed + ": " + line; !strings.Contains(ask4.ActionText, want) {
		t.Fatalf("command text %q", ask4.ActionText)
	}
	if _, err := orchclient.Choose(ctx, a.owner, "", ask4.ID, orchapi.DecideRequest{Decision: "a"}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("the owner's socket picked a command: %v", err)
	}
	for name, m := range map[string]string{"no remote tasks": "mini", "unknown": "nowhere", "auto": "auto"} {
		_, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Topic: "cmd-machine-" + m,
			Card: card("Run it there?", &orchapi.AskAction{Kind: orchapi.ActionCommand, Machine: m, Command: "ls", Ref: "main", Commit: pushed})})
		if err == nil {
			t.Errorf("command on %s machine %s: accepted", name, m)
		}
	}
	mustf(svc.Decide(ctx, beaPhone, ask4.ID, "a"))(t)
	eventually(t, 20*time.Second, func() error {
		heartbeat(t, box)
		svc.WatchActions(ctx)
		got = mustf(orchclient.GetAsk(ctx, a.owner, ask4.ID))(t)
		if got.ActionState == nil || got.ActionState.State != orchapi.ActionDone {
			return fmt.Errorf("command state %+v", got.ActionState)
		}
		return nil
	})
	out := "ZM-CMD-OK " + strings.ToUpper(pushed)
	eventually(t, 20*time.Second, func() error {
		heartbeat(t, box)
		heartbeat(t, mac)
		mail, item := false, false
		for _, m := range mustf(orchclient.Inbox(ctx, agent, orchapi.InboxQuery{}))(t) {
			if strings.Contains(m.Body, out) && strings.Contains(m.Body, "to-stderr") {
				mail = true
			}
		}
		for _, c := range mustf(orchclient.Comments(ctx, a.owner, it.ID))(t) {
			if strings.Contains(c.Body, out) {
				item = true
			}
		}
		if !mail || !item {
			return fmt.Errorf("command output: asker mail %v, item comment %v", mail, item)
		}
		return nil
	})
}

var beaPhone = &core.Remote{Node: "bea-phone", NodeID: "nBEAPHONE", NodeKey: "nodekey:beaphone"}
