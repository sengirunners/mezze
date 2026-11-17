package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sengirunners/mezze/internal/mcp"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
)

// Exercise the real registry's random collision fallback across a restart,
// then prove CLI and MCP calls use that address for both sender and inbox.
func TestRuntimeCollisionMailRouting(t *testing.T) {
	ctx := context.Background()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	repo := filepath.Join(home, "repo")
	addresses := map[string]string{}
	runtimeIDs := []string{"01a1a548-1111-4111-8111-111111111111", "01a1a548-2222-4222-8222-222222222222"}
	for generation := 0; generation < 2; generation++ {
		dm, err := server.New(ctx, cfg, nil, wiring.Full)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer dm.Close()
			owner := client.WithHandler(dm.SocketHandler())
			owner.Project = "collision-test"
			if generation == 0 {
				if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: owner.Project, RepoRules: api.RepoRules{Paths: []string{repo}}}); err != nil {
					t.Fatal(err)
				}
			}
			for _, rid := range runtimeIDs {
				c := client.WithHandler(dm.SocketHandler())
				c.Project, c.Cwd = owner.Project, repo
				// This is the obsolete guess supplied by DefaultAs. Resolution
				// must disregard it even when it names an existing session.
				c.As = api.SessionAddress(cfg.Machine, api.Session8(rid))
				reg := orchapi.RegisterRequest{Agent: "codex", RuntimeID: rid, Cwd: repo}
				if err := orchclient.ResolveRuntimeSession(ctx, c, reg); err != nil {
					t.Fatal(err)
				}
				if generation == 0 {
					addresses[rid] = c.As
				} else if c.As != addresses[rid] {
					t.Fatalf("runtime address changed after reopen: %q -> %q", addresses[rid], c.As)
				}
				mail, err := orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "owners", Body: fmt.Sprintf("CLI collision sender (generation %d)", generation), Kind: orchapi.MailKindReport})
				if err != nil || mail.From != addresses[rid] {
					t.Fatalf("CLI sender=%q want=%q err=%v", mail.From, addresses[rid], err)
				}
				incoming, err := orchclient.Send(ctx, owner, orchapi.SendMailRequest{To: c.As, Body: "private runtime inbox"})
				if err != nil {
					t.Fatal(err)
				}
				inbox, err := orchclient.Inbox(ctx, c, orchapi.InboxQuery{})
				if err != nil || len(inbox) != 1 || inbox[0].ID != incoming.ID {
					t.Fatalf("wrong runtime inbox: %+v err=%v", inbox, err)
				}
				if _, err := orchclient.Ack(ctx, c, incoming.ID); err != nil {
					t.Fatal(err)
				}
				cs := session(t, owner, mcp.Options{Register: &reg})
				result, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "mail_send", Arguments: map[string]any{"to": "owners", "body": fmt.Sprintf("MCP collision sender (generation %d)", generation), "kind": "report"}})
				if err != nil || result.IsError {
					t.Fatalf("MCP send: %+v err=%v", result, err)
				}
				cs.Close()
				sent, err := orchclient.Sent(ctx, c, owner.Project, 10)
				if err != nil || len(sent) == 0 || sent[0].From != addresses[rid] || sent[0].Body != fmt.Sprintf("MCP collision sender (generation %d)", generation) {
					t.Fatalf("MCP sender did not match CLI: %+v err=%v", sent, err)
				}
			}
			if addresses[runtimeIDs[0]] == addresses[runtimeIDs[1]] {
				t.Fatal("colliding runtime ids share an address")
			}
		}()
	}
}

func TestRuntimeCollisionResumesOnlyWithLiveAgent(t *testing.T) {
	dm := daemon(t)
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project = "resume-test"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: owner.Project}); err != nil {
		t.Fatal(err)
	}
	first, err := orchclient.Register(ctx, owner, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "aabbccdd-first", State: orchapi.StateNeedsHuman})
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_ = child.Wait()
	})
	reg := orchapi.RegisterRequest{Agent: "codex", RuntimeID: "aabbccdd-second", PID: child.Process.Pid}
	second, err := orchclient.Register(ctx, owner, reg)
	if err != nil {
		t.Fatal(err)
	}
	end := reg
	end.End = true
	if _, err := orchclient.Register(ctx, owner, end); err != nil {
		t.Fatal(err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	reg.PID = 0 // recorded process is demonstrably gone, not merely unknown
	c := client.WithHandler(dm.SocketHandler())
	c.Project, c.As = owner.Project, first.Address
	if err := orchclient.ResolveRuntimeSession(ctx, c, reg); !api.IsCode(err, api.CodeConflict) || c.As != "" {
		t.Fatalf("ended session accepted without liveness: as=%q err=%v", c.As, err)
	}
	reg.PID = os.Getpid() // live process evidence; no provider/runtime needed
	if err := orchclient.ResolveRuntimeSession(ctx, c, reg); err != nil || c.As != second.Address {
		t.Fatalf("resume changed canonical identity: as=%q want=%q err=%v", c.As, second.Address, err)
	}
	resumed, err := orchclient.Session(ctx, owner, second.Address)
	if err != nil || resumed.State != orchapi.StateRunning || resumed.Outcome != nil {
		t.Fatalf("session did not resume: %+v err=%v", resumed, err)
	}
	untouched, err := orchclient.Session(ctx, owner, first.Address)
	if err != nil || untouched.State != orchapi.StateNeedsHuman {
		t.Fatalf("other colliding session changed: %+v err=%v", untouched, err)
	}
	if err := orchclient.ResolveRuntimeSession(ctx, c, reg); err != nil {
		t.Fatal(err)
	}
	mail, err := orchclient.Send(ctx, owner, orchapi.SendMailRequest{To: second.Address, Body: "resumed runtime inbox"})
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := orchclient.Inbox(ctx, c, orchapi.InboxQuery{})
	if err != nil || len(inbox) != 1 || inbox[0].ID != mail.ID {
		t.Fatalf("resumed inbox: %+v err=%v", inbox, err)
	}
}
