package team_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/agentpipe"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// fakePipeAgent makes codex on a machine a structured-session runtime that
// replays rec (internal/agentpipe/testdata), and opts the machine in.
func fakePipeAgent(t *testing.T, rec string) func(*config.Config) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(filepath.Join("..", "agentpipe", "testdata", rec))
	if err != nil {
		t.Fatal(err)
	}
	return func(c *config.Config) {
		roomy(c)
		c.File.Agents = map[string][]string{"codex": {exe, agentpipe.FakeArg, abs}}
		c.File.RemoteTasks = true
	}
}

// Sessions on other machines (SPEC 8): the project's owner on
// a member Mac starts a shared structured session on box through the
// authority, drives it there (through its relay) and stops it. A retry
// with the same client_request_id answers the same session. Acting as the
// session on box sees nothing outside the project (SPEC 1.3). A member who
// does not own the project, a machine that has not opted in, and a stop of a
// session box started for itself are refused.
func TestSessionOnAnotherMachine(t *testing.T) {
	quietPresence(t)
	defer team.ReportEveryBeat()()
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, roomy)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	join := func(machine, as string, mut func(*config.Config)) *daemon {
		d := startDaemonWith(t, tn, machine, tailnettest.Identity{NodeID: "n" + strings.ToUpper(machine), NodeKey: "nodekey:" + machine, Login: as + "@example.com", Host: machine}, discard, mut)
		inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: as, Role: "member"}))(t)
		mustf(d.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
		return d
	}
	box := join("box", "box", fakePipeAgent(t, "codex/turns.rec.jsonl"))
	shy := join("shy", "shy", roomy) // never opted in
	mac := join("mac", "ivogh", roomy)
	bea := join("bea-mac", "bea", roomy)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "ivogh", Project: "harbor", Role: "owner"}))(t)

	// box knows its checkout of the project from a session that ran there.
	checkout := t.TempDir()
	mustf(orchclient.Spawn(ctx, at(box.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: checkout}))(t)
	// And a project of its own the remote session must not see.
	mustf(box.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "private"}))(t)
	priv := mustf(orchclient.Spawn(ctx, at(box.owner, "private", ""), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: t.TempDir()}))(t)
	for _, d := range []*daemon{box, shy, mac, bea} {
		heartbeat(t, d) // presence and machine numbers at the authority
	}
	beating(t, box)

	start := func(d *daemon, machine, rid string) (orchapi.Session, error) {
		return orchclient.Spawn(ctx, at(d.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "codex", Mode: orchapi.SessionModePipe, Machine: machine,
			Cwd: t.TempDir(), ClientRequestID: rid, Title: "night triage"})
	}
	ss, err := start(mac, "box", "run-1")
	if err != nil || !strings.HasPrefix(ss.Address, "box/") || !ss.Shared || ss.Mode != orchapi.SessionModePipe {
		t.Fatalf("start on box: %+v %v", ss, err)
	}
	if ss.Cwd != checkout {
		t.Fatalf("cwd on box %q, want its checkout %q", ss.Cwd, checkout)
	}
	if again, err := start(mac, "box", "run-1"); err != nil || again.Address != ss.Address {
		t.Fatalf("retry with the same client_request_id: %+v %v", again, err)
	}
	// safe_outputs cross: box keeps and counts them, and the answer says so.
	capped, err := orchclient.Spawn(ctx, at(mac.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "codex", Mode: orchapi.SessionModePipe, Machine: "box",
		Cwd: t.TempDir(), ClientRequestID: "run-capped", SafeOutputs: map[string]int{"mail-send": 1}})
	if err != nil || capped.SafeOutputs["mail-send"] != 1 || len(capped.SafeOutputs) != len(orchapi.SafeOutputKinds) {
		t.Fatalf("capped start on box: %+v %v", capped.SafeOutputs, err)
	}
	for i, want := range []string{"", api.CodeForbidden} {
		_, err := orchclient.Send(ctx, at(box.owner, "harbor", capped.Address), orchapi.SendMailRequest{To: capped.Address, Body: "status"})
		if want == "" && err != nil || want != "" && !api.IsCode(err, want) {
			t.Fatalf("capped session's mail %d: %v", i+1, err)
		}
	}

	// Driven from the Mac through the authority.
	var after int64
	res, err := orchclient.Turn(ctx, mac.owner, ss.Address, orchapi.SessionTurnRequest{Text: "Reply with exactly the word: alpha"})
	if err != nil || (!res.OK && !res.Pending) {
		t.Fatalf("turn: %+v %v", res, err)
	}
	if w := saidWord(readUntil(t, mac.owner, ss.Address, &after)); w != "alpha" {
		t.Fatalf("turn said %q", w)
	}

	// The cap: as the session, box's own project is out of sight; the
	// session's own project is not. Box's owner still sees both.
	as := at(box.owner, "", ss.Address)
	if _, err := orchclient.Session(ctx, as, priv.Address); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("the remote session saw another project's session: %v", err)
	}
	if _, err := orchclient.Session(ctx, as, ss.Address); err != nil {
		t.Fatalf("the remote session lost its own: %v", err)
	}
	if _, err := orchclient.Session(ctx, box.owner, priv.Address); err != nil {
		t.Fatalf("box's owner: %v", err)
	}

	// A session it starts takes its cap.
	child := mustf(orchclient.Spawn(ctx, at(box.owner, "harbor", ss.Address), orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: checkout}))(t)
	if _, err := orchclient.Session(ctx, at(box.owner, "", child.Address), priv.Address); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("the remote session's child saw another project's session: %v", err)
	}

	// Refusals.
	if _, err := orchclient.Spawn(ctx, at(mac.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "codex", Mode: orchapi.SessionModePipe, Machine: "box",
		Cwd: t.TempDir(), Prompt: "go"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("a prompt with machine: %v", err)
	}
	if _, err := orchclient.Stop(ctx, mac.owner, ss.Address, orchapi.StopRequest{Escalate: true}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("an escalating stop from another machine: %v", err)
	}
	if _, err := start(bea, "box", ""); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a member who does not own the project: %v", err)
	}
	if _, err := start(mac, "shy", ""); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a machine that never opted in: %v", err)
	}
	// Naming its own machine starts it here, in the cwd given, uncapped (a Mezze app
	// on the machine its automations target): the retry key is not refused.
	selfCwd := t.TempDir()
	self, err := orchclient.Spawn(ctx, at(box.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "codex", Mode: orchapi.SessionModePipe, Machine: "box",
		Cwd: selfCwd, ClientRequestID: "self-1", Title: "night triage"})
	if err != nil || !strings.HasPrefix(self.Address, "box/") || self.Cwd != selfCwd {
		t.Fatalf("start on its own machine: %+v %v", self, err)
	}
	if _, err := orchclient.Session(ctx, at(box.owner, "", self.Address), priv.Address); err != nil {
		t.Fatalf("a session its own machine's owner started is capped: %v", err)
	}
	own := spawnPipe(t, box, "codex/turns.rec.jsonl", true)
	heartbeat(t, box)
	waitFor(t, "box's own session at the authority", func() bool { _, err := orchclient.Session(ctx, a.owner, own.Address); return err == nil })
	if _, err := orchclient.Stop(ctx, mac.owner, own.Address, orchapi.StopRequest{}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("stopped a session box started for itself: %v", err)
	}

	// Stopped from the Mac: it ends on box.
	if _, err := orchclient.Stop(ctx, mac.owner, ss.Address, orchapi.StopRequest{}); err != nil {
		t.Fatalf("stop from the Mac: %v", err)
	}
	waitFor(t, "the session ended on box", func() bool {
		x, err := orchclient.Session(ctx, box.owner, ss.Address)
		return err == nil && x.State == orchapi.StateEnded
	})
}
