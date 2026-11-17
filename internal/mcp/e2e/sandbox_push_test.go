package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
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

func TestMain(m *testing.M) {
	if code, ok := refdriver.Main(); ok { // the reference run-target driver runs as this binary
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// sandbox_create with a ref on a box without a repo (as every remote box
// is) checks out the agent's own commit, sent from the checkout dir names
// (absolute; a relative one is refused), and says which repo and commit;
// dirty sends changes to tracked files only.
func TestSandboxCreatePushesTheAgentsCommit(t *testing.T) {
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
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "wip.txt"), []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-q", "-m", "one"}} {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@x"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	for name, body := range map[string]string{"wip.txt": "not committed", ".env": "API_TOKEN=hunter2"} {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	repo, _ = filepath.EvalSymlinks(repo)
	cs := session(t, owner, mcp.Options{Sandboxes: true})
	if r, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "sandbox_create", Arguments: map[string]any{"ref": "HEAD", "dir": "relative/path"}}); err != nil || !r.IsError {
		t.Fatalf("a relative dir: %+v %v", r, err)
	}
	r, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "sandbox_create", Arguments: map[string]any{"ref": "HEAD", "dirty": true, "ttl_s": 120, "dir": repo}})
	if err != nil || r.IsError {
		t.Fatalf("sandbox_create: %+v %v", r, err)
	}
	var got struct {
		Handle string `json:"handle"`
		Push   struct {
			Repo, Commit string
		} `json:"push"`
	}
	for _, c := range r.Content {
		if tc, ok := c.(*sdk.TextContent); ok && strings.Contains(tc.Text, `"push"`) {
			_ = json.Unmarshal([]byte(tc.Text[strings.Index(tc.Text, "{"):]), &got)
		}
	}
	target, id, err := computeclient.SplitBoxHandle(got.Handle)
	if err != nil || got.Push.Commit == "" || got.Push.Repo != repo {
		t.Fatalf("answer %+v: %v", r.Content, err)
	}
	x, err := computeclient.ExecRunTarget(ctx, owner, target, id, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c", "cd checkout && git rev-parse HEAD && cat wip.txt && test ! -e .env"}})
	if err != nil || x.Stdout != got.Push.Commit+"\nnot committed" {
		t.Fatalf("the box: %+v %v", x, err)
	}
}
