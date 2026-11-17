package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

// TestNoDaemon (SPEC 17.1): with no daemon the server still answers
// initialize and tools/list within 200 ms, every tool call is an error
// saying the daemon is not running, and nothing starts one. Timing is opt-in
// (ZM_MCP_SPEED=1); ZM_MCP_JSON writes advisory rows for bench repeat.
func TestNoDaemon(t *testing.T) {
	t.Setenv("ZM_NO_AUTOSTART", "1")
	timing := newMCPNoDaemonTiming(t)
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	c := client.NewUnix(filepath.Join(home, "zm.sock"))
	started := 0
	c.Autostart = func(context.Context) error { started++; return nil }

	t0 := time.Now()
	cs := connect(t, c, Options{Register: &orchapi.RegisterRequest{Agent: "claude", RuntimeID: "nodaemon-1"}}) // initialize
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	timing.record("mcp/no-daemon/initialize-list", time.Since(t0))
	if len(res.Tools) != len(tools) {
		t.Fatalf("tools/list: %d tools, want %d", len(res.Tools), len(tools))
	}
	args := map[string]map[string]any{
		"ask_new": {"card": map[string]any{"question": "x"}}, "mail_send": {"to": "owners", "body": "x"}, "mail_ack": {"id": "01JA0M2C9Q3T4V5W6X7Y8Z9A0B"},
		"mail_reply": {"id": "01JA0M2C9Q3T4V5W6X7Y8Z9A0B", "body": "x"}, "work_get": {"id": "local:abcdef"},
		"work_claim": {"id": "local:abcdef"}, "work_renew": {"id": "local:abcdef"}, "work_release": {"id": "local:abcdef"},
		"work_set_status": {"id": "local:abcdef", "status": "done"}, "work_comment": {"id": "local:abcdef", "body": "x"},
		"task_start": {"agent": "codex", "prompt": "x"}, "task_status": {"id": "abcdef12"}, "task_read": {"id": "abcdef12", "item": "abcdef12:0"}, "task_cancel": {"id": "abcdef12"},
	}
	for _, tl := range res.Tools {
		t1 := time.Now()
		r, text := callTool(t, cs, tl.Name, args[tl.Name])
		if !r.IsError || !strings.Contains(text, "daemon not running") {
			t.Errorf("%s with no daemon: isError=%v %q", tl.Name, r.IsError, text)
		}
		timing.record("mcp/no-daemon/tool/"+tl.Name, time.Since(t1))
	}
	if started != 0 {
		t.Errorf("the MCP server started the daemon %d times; it must never", started)
	}
	if _, err := os.Stat(filepath.Join(home, "zm.sock")); err == nil {
		t.Error("a socket appeared")
	}
}
