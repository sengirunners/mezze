// Package e2e runs the MCP server against a real in-process daemon: the
// tools must see exactly what the caller's token sees (a guest of one area
// gets that area's repos as roots and nothing else). It lives apart from
// package mcp so the unit tests there do not depend on every module building.
package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func daemon(t *testing.T) *server.Daemon {
	t.Helper()
	home, err := os.MkdirTemp("", "zm") // short: Unix socket paths are limited
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	return dm
}

func session(t *testing.T, c *client.Client, options ...mcp.Options) *sdk.ClientSession {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, ct := sdk.NewInMemoryTransports()
	var o mcp.Options
	if len(options) > 0 {
		o = options[0]
	}
	ss, err := mcp.NewServer(c, o).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// Reproduce the sender at the daemon boundary in an isolated home. Losing
// ZM_AS must not turn a surviving managed-session marker into human mail.
func TestManagedSessionMailSender(t *testing.T) {
	dm := daemon(t)
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project = "sender-test"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: owner.Project}); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"human", nil, "human:ivo"},
		{"lost ZM_AS", map[string]string{"ZM_SESSION": "mini/aabbccdd"}, "mini/aabbccdd"},
		{"inherited original", map[string]string{"ZM_AS": "mini/b9098cd4", "ZM_SESSION": "mini/b9098cd4"}, "mini/b9098cd4"},
		{"explicit human", map[string]string{"ZM_AS": "human:ivo", "ZM_SESSION": "mini/aabbccdd"}, "human:ivo"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := client.WithHandler(dm.SocketHandler())
			c.Project = owner.Project
			c.As = client.DefaultAs(&config.Config{Machine: "mini"}, func(k string) string { return tt.env[k] })
			mail, err := orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "owners", Body: "sender regression", Kind: orchapi.MailKindReport})
			if err != nil || mail.From != tt.want {
				t.Fatalf("send: from=%q want=%q err=%v", mail.From, tt.want, err)
			}
		})
	}
}

func TestFailedMCPRegistrationCannotSendHumanMail(t *testing.T) {
	dm := daemon(t)
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project = "sender-test"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: owner.Project}); err != nil {
		t.Fatal(err)
	}
	cs := session(t, owner, mcp.Options{Register: &orchapi.RegisterRequest{Agent: "codex", RuntimeID: "invalid id"}})
	r, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "mail_send", Arguments: map[string]any{"to": "owners", "body": "must not arrive from human"}})
	if err != nil || !r.IsError {
		t.Fatalf("failed registration must refuse send: result=%+v err=%v", r, err)
	}
	mail, err := orchclient.Inbox(ctx, owner, orchapi.InboxQuery{All: true})
	if err != nil || len(mail) != 0 {
		t.Fatalf("failed registration delivered mail: %+v err=%v", mail, err)
	}
}

func call(t *testing.T, cs *sdk.ClientSession, name string) (bool, string) {
	t.Helper()
	r, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return r.IsError, b.String()
}

func TestToolsActWithTheCallersScope(t *testing.T) {
	ctx := context.Background()
	dm := daemon(t)
	repo := filepath.Join(t.TempDir(), "harbor")
	owner := client.WithHandler(dm.SocketHandler())
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor", RepoRules: api.RepoRules{Paths: []string{repo}}}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"solver", "zz-billing"} {
		if _, err := owner.AddArea(ctx, "harbor", api.Area{Name: a, RepoRules: api.RepoRules{Paths: []string{filepath.Join(repo, a)}}}); err != nil {
			t.Fatal(err)
		}
	}
	tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: "vida", Project: "harbor", Area: "solver", Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}

	// The owner, writing into harbor/solver: solver's repo first, then the
	// project's, then the other area's.
	oc := client.WithHandler(dm.SocketHandler())
	oc.Project, oc.ProjectFrom = "harbor/solver", "flag"
	isErr, text := call(t, session(t, oc), "project_current")
	var info mcp.ProjectInfo
	if isErr || json.Unmarshal([]byte(text), &info) != nil {
		t.Fatalf("owner project_current: %s", text)
	}
	want := []string{"file://" + filepath.Join(repo, "solver"), "file://" + repo, "file://" + filepath.Join(repo, "zz-billing")}
	if info.Area != "solver" || strings.Join(info.Roots, " ") != strings.Join(want, " ") {
		t.Errorf("owner info = %+v", info)
	}

	// The guest of harbor/solver over the network handler with its token:
	// solver's repo only; no project-level path, no other area's name.
	gc := client.WithHandler(dm.Handler)
	gc.Token = tok.Secret
	gc.Project, gc.ProjectFrom = "harbor", "flag"
	gs := session(t, gc)
	isErr, text = call(t, gs, "project_current")
	info = mcp.ProjectInfo{}
	if isErr || json.Unmarshal([]byte(text), &info) != nil {
		t.Fatalf("guest project_current: %s", text)
	}
	if strings.Join(info.Roots, " ") != "file://"+filepath.Join(repo, "solver") || strings.Contains(text, "zz-billing") {
		t.Errorf("guest sees too much: %s", text)
	}
	if isErr, text = call(t, gs, "whoami"); isErr || !strings.Contains(text, `"name": "vida"`) {
		t.Errorf("guest whoami: %s", text)
	}

	// No token over the network: refused, never the owner.
	if isErr, text = call(t, session(t, client.WithHandler(dm.Handler)), "whoami"); !isErr || !strings.Contains(text, "unauthenticated") {
		t.Errorf("anonymous whoami: %v %s", isErr, text)
	}
}
