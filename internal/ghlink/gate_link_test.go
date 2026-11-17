package ghlink_test

// Gates (SPEC 18.3) and --match, end to end through the receiver: a fake
// gate (gatetest) decides; nothing calls GitHub.

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/ghlink/gate"
	"github.com/sengirunners/mezze/internal/ghlink/gate/gatetest"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// clock is a settable clock for the linker (retention).
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func gated(t *testing.T, cfg gatetest.Config, req orchapi.GitHubLinkRequest) (*gx, string, string) {
	t.Helper()
	g := newGX(t)
	dir := t.TempDir()
	path, err := gatetest.Install(dir, cfg)
	if err != nil {
		t.Fatal(err)
	}
	g.svc.GateRecheck = 50 * time.Millisecond
	req.Gate = path
	g.link(req)
	return g, dir, path
}

func decides(dir string) []gate.Request {
	var out []gate.Request
	for _, l := range gatetest.Requests(dir) {
		if l.Verb == "decide" {
			var r gate.Request
			_ = json.Unmarshal(l.Args, &r)
			out = append(out, r)
		}
	}
	return out
}

func TestMatchTitleOnly(t *testing.T) {
	g := newGX(t)
	named := g.item("harbor", "named in the title")
	mentioned := g.item("harbor", "only mentioned in the body")
	g.link(orchapi.GitHubLinkRequest{Match: []string{"title"}})
	t0 := time.Now().Add(-time.Hour)
	g.deliver("pull_request", "m1", g.secret, prEvent("closed", mkPull(7, "Fix ["+named.ID+"]", "see also "+mentioned.ID, "x/"+mentioned.ID, "closed", true, t0), "example-org/harbor"))
	eventually(t, "the titled item done", func() bool { return g.get("harbor", named.ID).Status == orchapi.WorkDone })
	g.settle()
	if it := g.get("harbor", mentioned.ID); it.Status != orchapi.WorkOpen || len(g.comments("harbor", mentioned.ID)) != 0 {
		t.Fatalf("an item only the body and branch name was touched: %+v", it)
	}
	if _, err := orchclient.LinkGitHub(context.Background(), g.owner, orchapi.GitHubLinkRequest{Project: "harbor", Repos: []string{"a/b"}, Match: []string{"subject"}}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("bad --match: %v", err)
	}
}

// skip writes nothing; note comments and links but moves nothing; apply moves.
func TestGateSkipNoteApply(t *testing.T) {
	cfg := gatetest.Config{Decide: map[string]map[string]any{
		"open":   {"decision": "skip"},
		"merged": {"decision": "note", "note": "PR merged; not auto-closed (epic)"},
	}}
	g, dir, _ := gated(t, cfg, orchapi.GitHubLinkRequest{})
	it := g.item("harbor", "an epic")
	t0 := time.Now().Add(-time.Hour)
	g.deliver("pull_request", "s1", g.secret, prEvent("opened", mkPull(3, "["+it.ID+"]", "", "b", "open", false, t0), "example-org/harbor"))
	eventually(t, "the gate asked about open", func() bool { return len(decides(dir)) == 1 })
	g.settle()
	if got := g.get("harbor", it.ID); got.Status != orchapi.WorkOpen || len(got.Links) != 0 || len(g.comments("harbor", it.ID)) != 0 {
		t.Fatalf("skip wrote: %+v", got)
	}
	d := decides(dir)[0]
	if d.Proposed.To != "in_review" || d.Item.ID != it.ID || d.PR.State != "open" || d.Attempt != 1 {
		t.Fatalf("open request: %+v", d)
	}
	g.deliver("pull_request", "s2", g.secret, prEvent("closed", mkPull(3, "["+it.ID+"]", "", "b", "closed", true, t0.Add(time.Minute)), "example-org/harbor"))
	eventually(t, "the note", func() bool { return len(g.comments("harbor", it.ID)) == 1 })
	got := g.get("harbor", it.ID)
	if got.Status != orchapi.WorkOpen || len(got.Links) != 1 || g.comments("harbor", it.ID)[0].Body != "PR merged; not auto-closed (epic)" {
		t.Fatalf("note: %+v %+v", got, g.comments("harbor", it.ID))
	}
	m := decides(dir)[1]
	if m.PR.MergedAt == nil || m.PR.Base != "main" || m.Proposed.To != "done" || !strings.Contains(m.Proposed.Comment, "Closed by merged PR") {
		t.Fatalf("merged request: %+v", m)
	}
	// Asked once per state: a redelivered newer snapshot of the same state asks nothing.
	g.deliver("pull_request", "s3", g.secret, prEvent("edited", mkPull(3, "["+it.ID+"]", "x", "b", "closed", true, t0.Add(2*time.Minute)), "example-org/harbor"))
	g.settle()
	if n := len(decides(dir)); n != 2 {
		t.Fatalf("asked %d times", n)
	}
}

// wait holds the item, comments the wait's note once, re-asks on the timer
// with the first ask kept and last_write set, and applies when the gate says so.
func TestGateWaitThenApply(t *testing.T) {
	cfg := gatetest.Config{Decide: map[string]map[string]any{"merged": {"decision": "wait", "note": "merged; awaiting the deploy"}}}
	g, dir, _ := gated(t, cfg, orchapi.GitHubLinkRequest{})
	it := g.item("harbor", "ships a migration")
	t0 := time.Now().Add(-time.Hour)
	g.deliver("pull_request", "w1", g.secret, prEvent("closed", mkPull(5, "["+it.ID+"]", "", "m", "closed", true, t0), "example-org/harbor"))
	eventually(t, "three asks", func() bool { return len(decides(dir)) >= 3 })
	if got := g.get("harbor", it.ID); got.Status != orchapi.WorkOpen {
		t.Fatalf("a wait moved the item: %s", got.Status)
	}
	if cs := g.comments("harbor", it.ID); len(cs) != 1 || cs[0].Body != "merged; awaiting the deploy" {
		t.Fatalf("wait note once: %+v", cs)
	}
	ds := decides(dir)
	if !ds[0].FirstAsked.Equal(ds[2].FirstAsked) || ds[2].Attempt != 3 || ds[2].Item.LastWrite == nil || ds[0].Item.LastWrite != nil {
		t.Fatalf("re-asks: %+v / %+v", ds[0], ds[2])
	}
	ls, _ := orchclient.GitHubLinks(context.Background(), g.owner)
	if ls[0].Waiting != 1 || ls[0].GateName != "fake-gate 1" {
		t.Fatalf("link view: %+v", ls[0])
	}
	gatetest.Set(dir, gatetest.Config{Decide: map[string]map[string]any{"merged": {"decision": "apply", "note": "merged; on prod (deploy 42)"}}})
	eventually(t, "applied", func() bool { return g.get("harbor", it.ID).Status == orchapi.WorkDone })
	eventually(t, "the apply comment", func() bool { return len(g.comments("harbor", it.ID)) == 2 })
	if c := g.comments("harbor", it.ID)[1].Body; c != "merged; on prod (deploy 42)" {
		t.Fatalf("apply comment: %q", c)
	}
	eventually(t, "no wait left", func() bool { ls, _ := orchclient.GitHubLinks(context.Background(), g.owner); return ls[0].Waiting == 0 })
}

// A gate that fails or answers garbage is a wait: nothing moves, the error is
// named, and the decision is asked again.
func TestGateErrorIsWait(t *testing.T) {
	for name, bad := range map[string]gatetest.Config{"exit 1": {FailDecide: true}, "garbage": {Garbage: true},
		"unknown decision": {Decide: map[string]map[string]any{"merged": {"decision": "close"}}}} {
		t.Run(name, func(t *testing.T) {
			g, dir, _ := gated(t, gatetest.Config{}, orchapi.GitHubLinkRequest{})
			it := g.item("harbor", "x")
			gatetest.Set(dir, bad)
			g.deliver("pull_request", "e1", g.secret, prEvent("closed", mkPull(6, "["+it.ID+"]", "", "e", "closed", true, time.Now().Add(-time.Hour)), "example-org/harbor"))
			eventually(t, "a wait", func() bool { ls, _ := orchclient.GitHubLinks(context.Background(), g.owner); return ls[0].Waiting == 1 })
			time.Sleep(200 * time.Millisecond)
			if got := g.get("harbor", it.ID); got.Status != orchapi.WorkOpen || len(g.comments("harbor", it.ID)) != 0 {
				t.Fatalf("moved on a gate error: %+v", got)
			}
			if ls, _ := orchclient.GitHubLinks(context.Background(), g.owner); !strings.Contains(ls[0].LastError, "gate") {
				t.Fatalf("error not named: %q", ls[0].LastError)
			}
			gatetest.Set(dir, gatetest.Config{})
			eventually(t, "applied once the gate answers", func() bool { return g.get("harbor", it.ID).Status == orchapi.WorkDone })
		})
	}
}

// Past the retention the gate is asked with final; a wait then gives up with
// one comment and moves nothing.
func TestGateRetentionGivesUp(t *testing.T) {
	g, dir, _ := gated(t, gatetest.Config{Decide: map[string]map[string]any{"merged": {"decision": "wait"}}}, orchapi.GitHubLinkRequest{})
	ck := &clock{t: time.Now()}
	g.svc.Now = ck.now
	it := g.item("harbor", "never deployed")
	g.deliver("pull_request", "r1", g.secret, prEvent("closed", mkPull(8, "["+it.ID+"]", "", "r", "closed", true, time.Now().Add(-time.Hour)), "example-org/harbor"))
	eventually(t, "a wait", func() bool { return len(decides(dir)) >= 1 })
	ck.add(15 * 24 * time.Hour)
	eventually(t, "the give-up comment", func() bool { return len(g.comments("harbor", it.ID)) == 1 })
	c := g.comments("harbor", it.ID)[0].Body
	if !strings.Contains(c, "Not moved") || !strings.Contains(c, "decide by hand") || g.get("harbor", it.ID).Status != orchapi.WorkOpen {
		t.Fatalf("give up: %q", c)
	}
	ds := decides(dir)
	if !ds[len(ds)-1].Final {
		t.Fatal("the last ask was not final")
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(decides(dir)); n != len(ds) {
		t.Fatal("asked again after giving up")
	}
}

// A gate's match replaces the matcher; a match that fails holds the PR (it is
// not dropped after the inbox's attempts) until the gate answers.
func TestGateMatch(t *testing.T) {
	g := newGX(t)
	a := g.item("harbor", "named by the gate")
	b := g.item("harbor", "in the title, but the gate says no")
	dir := t.TempDir()
	path, _ := gatetest.Install(dir, gatetest.Config{FailMatch: true})
	g.svc.GateRecheck = 50 * time.Millisecond
	g.svc.MaxAttempts = 1
	g.link(orchapi.GitHubLinkRequest{Gate: path})
	g.deliver("pull_request", "g1", g.secret, prEvent("closed", mkPull(9, "["+b.ID+"]", "", "g", "closed", true, time.Now().Add(-time.Hour)), "example-org/harbor"))
	time.Sleep(300 * time.Millisecond)
	if g.get("harbor", b.ID).Status != orchapi.WorkOpen {
		t.Fatal("moved while match failed")
	}
	if ls, _ := orchclient.GitHubLinks(context.Background(), g.owner); ls[0].Pending != 1 {
		t.Fatalf("the PR was dropped: %+v", ls[0])
	}
	gatetest.Set(dir, gatetest.Config{Match: []string{a.ID, "nope:zzz"}}) // a source the link lacks: ignored
	eventually(t, "a done", func() bool { return g.get("harbor", a.ID).Status == orchapi.WorkDone })
	g.settle()
	if g.get("harbor", b.ID).Status != orchapi.WorkOpen {
		t.Fatal("the matcher was used beside the gate's match")
	}
}

// The gate runs with the allowlisted environment only.
func TestGateEnvironment(t *testing.T) {
	t.Setenv("BEADS_DOLT_PASSWORD", "live-secret")
	t.Setenv("ZM_TOKEN", "zm_live")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-secret-zz9")
	g, dir, _ := gated(t, gatetest.Config{}, orchapi.GitHubLinkRequest{})
	reqs := gatetest.Requests(dir)
	if len(reqs) == 0 {
		t.Fatal("describe never ran")
	}
	env := strings.Join(reqs[0].Env, "\n")
	for _, bad := range []string{"live-secret", "zm_live", "aws-secret-zz9"} {
		if strings.Contains(env, bad) {
			t.Fatalf("%s reached the gate:\n%s", bad, env)
		}
	}
	if !strings.Contains(env, "ZM_HOME="+g.home) || !strings.Contains(env, "ZM_NO_AUTOSTART=1") {
		t.Fatalf("gate env: %s", env)
	}
}

func TestGateRefusedAtLink(t *testing.T) {
	g := newGX(t)
	dir := t.TempDir()
	path, _ := gatetest.Install(dir, gatetest.Config{FailDescribe: true})
	if _, err := orchclient.LinkGitHub(context.Background(), g.owner, orchapi.GitHubLinkRequest{Project: "harbor", Repos: []string{"a/b"}, Gate: path}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("broken gate linked: %v", err)
	}
	for _, p := range []string{"relative/gate", dir, "/nonexistent/gate"} {
		if _, err := orchclient.LinkGitHub(context.Background(), g.owner, orchapi.GitHubLinkRequest{Project: "harbor", Repos: []string{"a/b"}, Gate: p}); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("gate %q: %v", p, err)
		}
	}
}

// Re-linking without --gate keeps the gate (and --match); only --gate none
// removes it, and its held decisions go with it instead of applying.
func TestRelinkKeepsGate(t *testing.T) {
	g, dir, path := gated(t, gatetest.Config{Decide: map[string]map[string]any{"merged": {"decision": "wait"}}}, orchapi.GitHubLinkRequest{Match: []string{"title"}})
	it := g.item("harbor", "held")
	g.deliver("pull_request", "k1", g.secret, prEvent("closed", mkPull(4, "["+it.ID+"]", "", "k", "closed", true, time.Now().Add(-time.Hour)), "example-org/harbor"))
	eventually(t, "a wait", func() bool { return len(decides(dir)) >= 1 })
	r, err := orchclient.LinkGitHub(context.Background(), g.owner, orchapi.GitHubLinkRequest{Project: "harbor", Repos: []string{"example-org/harbor"}, PollSeconds: 60})
	if err != nil || r.Link.Gate != path || strings.Join(r.Link.Match, ",") != "title" {
		t.Fatalf("relink dropped the gate or match: %+v %v", r.Link, err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := g.get("harbor", it.ID); got.Status != orchapi.WorkOpen {
		t.Fatalf("held item moved after a relink: %s", got.Status)
	}
	r, err = orchclient.LinkGitHub(context.Background(), g.owner, orchapi.GitHubLinkRequest{Project: "harbor", Repos: []string{"example-org/harbor"}, Gate: "none", Match: []string{"all"}})
	if err != nil || r.Link.Gate != "" || len(r.Link.Match) != 3 || r.Link.Waiting != 0 {
		t.Fatalf("--gate none: %+v %v", r.Link, err)
	}
	time.Sleep(300 * time.Millisecond)
	if got := g.get("harbor", it.ID); got.Status != orchapi.WorkOpen || len(g.comments("harbor", it.ID)) != 0 {
		t.Fatalf("a dropped gate's held decision applied: %+v", got)
	}
}

// A gate that cannot answer is not a verdict: past its retention it is still
// held (no give-up), and only the maximum retention gives up, saying so.
func TestGateErrorAtFinalKeepsHolding(t *testing.T) {
	g, dir, _ := gated(t, gatetest.Config{FailDecide: true}, orchapi.GitHubLinkRequest{})
	ck := &clock{t: time.Now()}
	g.svc.Now = ck.now
	it := g.item("harbor", "gate down")
	g.deliver("pull_request", "f1", g.secret, prEvent("closed", mkPull(2, "["+it.ID+"]", "", "f", "closed", true, time.Now().Add(-time.Hour)), "example-org/harbor"))
	eventually(t, "a failed ask", func() bool { return len(decides(dir)) >= 1 })
	ck.add(15 * 24 * time.Hour)
	n := len(decides(dir))
	eventually(t, "asked again past the retention", func() bool { return len(decides(dir)) > n })
	if len(g.comments("harbor", it.ID)) != 0 || g.get("harbor", it.ID).Status != orchapi.WorkOpen {
		t.Fatal("gave up (or moved) on a gate that never answered, inside the maximum retention")
	}
	ck.add(80 * 24 * time.Hour)
	eventually(t, "the honest give-up", func() bool { return len(g.comments("harbor", it.ID)) == 1 })
	c := g.comments("harbor", it.ID)[0].Body
	if !strings.Contains(c, "did not answer") || strings.Contains(c, dir) {
		t.Fatalf("give-up text: %q", c)
	}
}
