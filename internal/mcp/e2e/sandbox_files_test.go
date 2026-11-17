package e2e

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sengirunners/mezze/internal/mcp"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/runtarget"
)

// sandbox_write_file and sandbox_read_file on the reference driver:
// text and base64 in, pages out from an offset, a
// non-UTF-8 page as base64, the read marked untrusted; content over 1 MiB,
// bad base64, a directory, a page over 512 KiB and a missing file refused.
func TestSandboxFileTools(t *testing.T) {
	ctx := context.Background()
	dm := daemon(t)
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		}
	}
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "box", Driver: "ref", Kind: "box", Isolation: "ephemeral",
		Images: []string{"ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"}}}); err != nil {
		t.Fatal(err)
	}
	h, err := computeclient.AcquireRunTarget(ctx, owner, "box", computeapi.AcquireRunTargetRequest{RequestID: "f1", TTLS: 120})
	if err != nil {
		t.Fatal(err)
	}
	cs := session(t, owner, mcp.Options{Sandboxes: true})
	call := func(name string, args map[string]any) (*sdk.CallToolResult, string) {
		t.Helper()
		args["handle"] = h.Handle
		r, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, c := range r.Content {
			if tc, ok := c.(*sdk.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		return r, b.String()
	}
	if r, txt := call("sandbox_write_file", map[string]any{"path": "notes/a.txt", "content": "héllo\nwörld\n"}); r.IsError || !strings.Contains(txt, "/notes/a.txt") || !strings.HasPrefix(txt, mcp.UntrustedNote) {
		t.Fatalf("write text: %s", txt)
	}
	if r, txt := call("sandbox_write_file", map[string]any{"path": "b.bin", "content": "AP8A", "encoding": "base64"}); r.IsError {
		t.Fatalf("write base64: %s", txt)
	}
	type filePage struct {
		Size, Next        int64
		EOF               bool
		Encoding, Content string
	}
	page := func(args map[string]any) (bool, filePage) {
		t.Helper()
		r, txt := call("sandbox_read_file", args)
		if r.IsError {
			t.Fatalf("read %v: %s", args, txt)
		}
		var p filePage
		if err := json.Unmarshal([]byte(txt[strings.Index(txt, "{"):strings.LastIndex(txt, "}")+1]), &p); err != nil {
			t.Fatalf("read answer %s: %v", txt, err)
		}
		return strings.HasPrefix(txt, mcp.UntrustedNote), p
	}
	untrusted, p := page(map[string]any{"path": "notes/a.txt", "offset": 7})
	if !untrusted || p.Content != "wörld\n" || p.Encoding != "text" || !p.EOF || p.Size != 14 {
		t.Fatalf("a text page: untrusted %v %+v", untrusted, p)
	}
	if _, p = page(map[string]any{"path": "b.bin"}); p.Encoding != "base64" || p.Content != "AP8A" {
		t.Fatalf("a binary page: %+v", p)
	}
	if _, p = page(map[string]any{"path": "notes/a.txt", "max_bytes": 2}); p.Content != "h" || p.Next != 1 || p.EOF { // é cut: back to a whole character
		t.Fatalf("a page cut mid-character: %+v", p)
	}
	for _, bad := range []struct {
		name string
		args map[string]any
	}{
		{"sandbox_write_file", map[string]any{"path": "big", "content": strings.Repeat("x", 1<<20+1)}},
		{"sandbox_write_file", map[string]any{"path": "x", "content": "!!", "encoding": "base64"}},
		{"sandbox_write_file", map[string]any{"path": "notes", "content": "x"}},
		{"sandbox_read_file", map[string]any{"path": "notes/a.txt", "max_bytes": 1 << 20}},
		{"sandbox_read_file", map[string]any{"path": "nope"}},
	} {
		if r, txt := call(bad.name, bad.args); !r.IsError {
			t.Fatalf("%s %v was taken: %s", bad.name, bad.args, txt)
		}
	}
}

// sandbox_write_file replaces a file in the box and its answer names a
// path the box gave: annotated destructive, its result marked untrusted.
func TestSandboxWriteFileAnnotations(t *testing.T) {
	ctx := context.Background()
	dm := daemon(t)
	owner := client.WithHandler(dm.SocketHandler())
	cs := session(t, owner, mcp.Options{Sandboxes: true})
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		if tl.Name == "sandbox_write_file" && (tl.Annotations == nil || tl.Annotations.DestructiveHint == nil || !*tl.Annotations.DestructiveHint) {
			t.Fatalf("sandbox_write_file annotations %+v", tl.Annotations)
		}
	}
}
