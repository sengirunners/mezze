package fulltest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/agentpipe"
	_ "github.com/sengirunners/mezze/internal/agentpipe/builtin"     // zm __zm-agent <driver>, as cmd/zm links it
	_ "github.com/sengirunners/mezze/internal/agentpipe/conformance" // the fake runtime (__zm-agent-fake)
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	. "github.com/sengirunners/potluck/kit/server"
)

// TestSafeOutputsCapASessionsWrites: a session started with safe_outputs
// gets its caps echoed, sends mail up to its cap and is refused the next; a
// write zm refuses does not count; a kind it was given no cap for is refused
// at once, and so is a session it starts; the person and an uncapped session
// are not counted.
func TestSafeOutputsCapASessionsWrites(t *testing.T) {
	home := shortHome(t)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "mini"
	// The machine's own load is not under test: admit any session.
	cfg.File.Admission.MinMemoryFreePercent, cfg.File.Admission.MaxSwapUsedMB, cfg.File.Admission.MinDiskFreeMB = 1, 1<<40, 1
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dm, err := New(ctx, cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	defer func() { cancel(); <-done }()
	c := client.NewUnix(cfg.SocketPath())
	if err := c.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if _, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Paths: []string{repo}}}); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := filepath.Abs(filepath.Join("..", "..", "agentpipe", "testdata", "codex", "turns.rec.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	spawn := func(caps map[string]int) *client.Client {
		ss, err := orchclient.Spawn(ctx, c, orchapi.SpawnRequest{Agent: "codex", Command: []string{exe, agentpipe.FakeArg, rec},
			Cwd: repo, Mode: orchapi.SessionModePipe, SafeOutputs: caps})
		if err != nil {
			t.Fatalf("spawn: %v", err)
		}
		if want := len(caps) > 0; want != (len(ss.SafeOutputs) == len(orchapi.SafeOutputKinds)) || want && ss.SafeOutputs["mail-send"] != caps["mail-send"] {
			t.Fatalf("start answered caps %v for %v", ss.SafeOutputs, caps)
		}
		t.Cleanup(func() { _, _ = orchclient.Stop(context.Background(), c, ss.ID, orchapi.StopRequest{}) })
		as := *c
		as.Project, as.ProjectFrom, as.As = "acme", "flag", ss.Address
		return &as
	}
	sent := 0
	send := func(cl *client.Client, to string) error {
		// A report: an agent's plain note to a person is refused. Each is
		// new: the same mail twice is refused (SPEC 9 "Send limits").
		sent++
		_, err := orchclient.Send(ctx, cl, orchapi.SendMailRequest{Kind: orchapi.MailKindReport, To: to, Body: fmt.Sprintf("status %d", sent)})
		return err
	}
	capped := spawn(map[string]int{"mail-send": 2})
	if err := send(capped, "not an address"); err == nil {
		t.Fatal("mail to a bad address was taken")
	}
	for i := range 2 {
		if err := send(capped, "human:ana"); err != nil {
			t.Fatalf("send %d under the cap: %v", i+1, err)
		}
	}
	if err := send(capped, "human:ana"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("send past the cap: %v", err)
	}
	if _, err := orchclient.CreateWork(ctx, capped, orchapi.CreateWorkRequest{Title: "a bead"}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("bead write with no cap: %v", err)
	}
	if err := capped.Do(ctx, "POST", "/v0/asks", map[string]any{"question": "merge it?"}, nil); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("ask with no cap: %v", err)
	}
	if _, err := orchclient.Spawn(ctx, capped, orchapi.SpawnRequest{Agent: "codex", Command: []string{exe, agentpipe.FakeArg, rec},
		Cwd: repo, Mode: orchapi.SessionModePipe}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a capped session's own start: %v", err)
	}

	free := spawn(nil)
	for range 3 {
		if err := send(free, "human:ana"); err != nil {
			t.Fatalf("uncapped session: %v", err)
		}
	}
	owner := *c
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if err := send(&owner, "human:ana"); err != nil {
		t.Fatalf("the person: %v", err)
	}

	if _, err := orchclient.Spawn(ctx, c, orchapi.SpawnRequest{Agent: "codex", Command: []string{exe, agentpipe.FakeArg, rec},
		Cwd: repo, Mode: orchapi.SessionModePipe, SafeOutputs: map[string]int{"pr-merge": 1}}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("unknown kind: %v", err)
	}
	if _, err := orchclient.Spawn(ctx, c, orchapi.SpawnRequest{Agent: "codex", Cwd: repo, SafeOutputs: map[string]int{"zm-ask": 1}}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("safe_outputs on a pty session: %v", err)
	}
}
