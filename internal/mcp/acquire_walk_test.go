package mcp

import (
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// walkDaemon is a fake daemon with two ranked box bindings, A (one CPU: it
// ranks first) and B, for the sandbox_create walk: A
// refuses before it records the request, B takes the box. It logs every box
// request and the zm-push bundles alive at each acquire.
type walkDaemon struct {
	tmp string // TMPDIR, where a bundle is made
	mu  sync.Mutex
	log []string // "METHOD path bundles=<names> body"
}

// bundles names the zm-push bundle folders alive now, comma-separated: the
// same one name at every acquire is one bundle made for the whole walk.
func (d *walkDaemon) bundles() string {
	es, _ := os.ReadDir(d.tmp)
	var names []string
	for _, e := range es {
		if strings.HasPrefix(e.Name(), "zm-push") && !strings.HasPrefix(e.Name(), "zm-push-index") {
			names = append(names, e.Name())
		}
	}
	return strings.Join(names, ",")
}

func (d *walkDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(api.HeaderProtocol, api.Protocol)
	var body json.RawMessage
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	d.mu.Lock()
	d.log = append(d.log, r.Method+" "+r.URL.Path+" bundles="+d.bundles()+" "+string(body))
	d.mu.Unlock()
	view := func(name string, cpu int) computeapi.RunTargetView {
		return computeapi.RunTargetView{HostClass: api.MachineTrusted, RunTarget: computeapi.RunTarget{Name: name, Kind: "box"},
			Describe: &computeapi.RunTargetDescribe{Kind: "box", Available: true, Size: &computeapi.RunTargetSize{Name: "small", OS: "linux", CPU: cpu, MemoryMB: 1024}}}
	}
	switch {
	case r.Method == "GET" && r.URL.Path == "/v0/run_targets":
		api.WriteJSON(w, 200, computeapi.RunTargetList{Targets: []computeapi.RunTargetView{view("B", 2), view("A", 1)}})
	case r.Method == "POST" && r.URL.Path == "/v0/run_targets/A/acquire":
		api.WriteError(w, api.Errorf(api.CodeUnavailable, "A is busy").WithData(computeapi.AcquireRefused{Reason: computeapi.RefusedUnavailable}))
	case r.Method == "POST" && r.URL.Path == "/v0/run_targets/B/acquire":
		api.WriteJSON(w, 200, computeapi.RunTargetHandle{Handle: "box:B/b1", Target: "B", Kind: "box", State: "ready"})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/exec"):
		api.WriteJSON(w, 200, computeapi.RunTargetExec{Exit: 0})
	default:
		api.WriteError(w, api.ErrNotImplemented(r.URL.Path))
	}
}

func (d *walkDaemon) lines(sub string) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []string
	for _, l := range d.log {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return out
}

// sandbox_create with a ref: A refuses before record, B takes it; the
// bundle is made once for the walk and the push goes to B's handle.
func TestSandboxCreateWalkPushesToTheBindingThatAnswered(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@x", "commit", "-q", "--allow-empty", "-m", "c"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	tmp, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)

	d := &walkDaemon{tmp: tmp}
	cs := connect(t, client.WithHandler(d), Options{Sandboxes: true})
	r, text := callTool(t, cs, "sandbox_create", map[string]any{"ref": "HEAD", "dir": repo, "client_request_id": "k5"})
	if r.IsError {
		t.Fatalf("sandbox_create failed: %s", text)
	}
	if !strings.Contains(text, `"handle":"box:B/b1"`) && !strings.Contains(text, `"handle": "box:B/b1"`) {
		t.Errorf("result %s: want B's handle", text)
	}
	acq := d.lines("/acquire")
	if len(acq) != 2 || !strings.HasPrefix(acq[0], "POST /v0/run_targets/A/acquire") || !strings.HasPrefix(acq[1], "POST /v0/run_targets/B/acquire") {
		t.Fatalf("acquires %q, want A then B (a walk)", acq)
	}
	bundleAt := func(l string) string { _, b, _ := strings.Cut(l, " bundles="); b, _, _ = strings.Cut(b, " "); return b }
	for _, l := range acq {
		if !strings.Contains(l, `"request_id":"k5"`) || strings.Contains(l, `"ref"`) {
			t.Errorf("acquire %s: want request k5 and no ref (the commit is pushed in)", l)
		}
		if b := bundleAt(l); b == "" || strings.Contains(b, ",") || b != bundleAt(acq[0]) {
			t.Errorf("acquire %s: want the one same bundle for the whole walk (made once)", l)
		}
	}
	execs := d.lines("/exec")
	if len(execs) == 0 {
		t.Fatal("nothing was pushed")
	}
	for _, l := range execs {
		if !strings.HasPrefix(l, "POST /v0/run_targets/B/handles/b1/exec") {
			t.Errorf("push went to %s, want B's handle", l)
		}
	}
	if b := d.bundles(); b != "" {
		t.Errorf("bundle %s left behind", b)
	}
}
