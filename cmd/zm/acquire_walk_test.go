package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// The ranked-binding walk (Potluck's computeclient.AcquireBox) as zm's three
// callers drive it: a fake daemon with scripted bindings,
// every request logged, so a prep that drifts from Potluck's contract fails
// here: one bundle for the walk, the push to the binding that answered, a
// Misfit passing on, a refusal that is not marked ending the walk, and
// jobRun asking the binding it last asked again, with its own ttl.

// walkReq is one request the fake daemon received.
type walkReq struct {
	method, path, body string
	bundles            string // the zm-push bundle folders alive when it arrived, comma-separated
}

// target is the binding of the path, "" for a route not under one.
func (r walkReq) target() string {
	p := strings.Split(strings.TrimPrefix(r.path, "/v0/run_targets/"), "/")
	if !strings.HasPrefix(r.path, "/v0/run_targets/") {
		return ""
	}
	return p[0]
}

func (r walkReq) is(method, suffix string) bool {
	return r.method == method && strings.HasSuffix(r.path, suffix) && r.target() != ""
}

// acquire is the acquire request's body.
func (r walkReq) acquire(t *testing.T) computeapi.AcquireRunTargetRequest {
	t.Helper()
	var a computeapi.AcquireRunTargetRequest
	if err := json.Unmarshal([]byte(r.body), &a); err != nil {
		t.Fatalf("acquire body %q: %v", r.body, err)
	}
	return a
}

// walkBinding is one box binding of the fake: cpu ranks it (fewest first),
// repo makes it check refs out itself, ttlMax is its lifetime cap, and
// answers are what its acquires say in turn (nil: a box, id "<name>1").
type walkBinding struct {
	name    string
	cpu     int
	repo    string
	ttlMax  int
	answers []*api.Error // a nil entry: success
	// onAcquire runs when an acquire arrives, before its answer.
	onAcquire func(n int)
}

type walkDaemon struct {
	tmp      string // TMPDIR: where a bundle is made
	mu       sync.Mutex
	bindings []*walkBinding
	reqs     []walkReq
	acquires map[string]int
	execErr  *api.Error // nil: exec succeeds, exit 0
}

// refusedBeforeRecord is a refusal the daemon marked as made before it
// recorded the request (computeapi.AcquireRefused).
func refusedBeforeRecord(why string) *api.Error {
	return api.Errorf(api.CodeUnavailable, "binding is %s", why).WithData(computeapi.AcquireRefused{Reason: computeapi.RefusedUnavailable})
}

func newWalkDaemon(t *testing.T, bs ...*walkBinding) (*walkDaemon, *client.Client) {
	t.Helper()
	tmp, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	d := &walkDaemon{tmp: tmp, bindings: bs, acquires: map[string]int{}}
	// Bundles are made under TMPDIR: counting them there says how many a walk made.
	t.Setenv("TMPDIR", tmp)
	return d, client.WithHandler(d)
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

func (d *walkDaemon) log() []walkReq {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]walkReq(nil), d.reqs...)
}

// only is the requests that match, in order.
func (d *walkDaemon) only(method, suffix string) []walkReq {
	var out []walkReq
	for _, r := range d.log() {
		if r.is(method, suffix) {
			out = append(out, r)
		}
	}
	return out
}

func targets(rs []walkReq) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.target())
	}
	return out
}

func (d *walkDaemon) view(b *walkBinding) computeapi.RunTargetView {
	return computeapi.RunTargetView{
		HostClass: api.MachineTrusted,
		RunTarget: computeapi.RunTarget{Name: b.name, Kind: "box", Repo: b.repo, TTLMaxS: b.ttlMax},
		Describe: &computeapi.RunTargetDescribe{Kind: "box", Available: true,
			Size: &computeapi.RunTargetSize{Name: "small", OS: "linux", CPU: b.cpu, MemoryMB: 1024}},
	}
}

func (d *walkDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(api.HeaderProtocol, api.Protocol)
	var raw json.RawMessage
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&raw)
	}
	d.mu.Lock()
	d.reqs = append(d.reqs, walkReq{method: r.Method, path: r.URL.Path, body: string(raw), bundles: d.bundles()})
	d.mu.Unlock()
	rest, ok := strings.CutPrefix(r.URL.Path, "/v0/run_targets")
	if !ok {
		api.WriteError(w, api.ErrNotImplemented(r.URL.Path))
		return
	}
	if rest == "" {
		var l computeapi.RunTargetList
		for _, b := range d.bindings {
			l.Targets = append(l.Targets, d.view(b))
		}
		api.WriteJSON(w, 200, l)
		return
	}
	parts := strings.Split(strings.TrimPrefix(rest, "/"), "/")
	var b *walkBinding
	for _, x := range d.bindings {
		if x.name == parts[0] {
			b = x
		}
	}
	if b == nil {
		api.WriteError(w, api.NotFound("run target", parts[0]))
		return
	}
	switch {
	case len(parts) == 1 && r.Method == "GET":
		api.WriteJSON(w, 200, d.view(b))
	case len(parts) == 2 && parts[1] == "acquire":
		d.mu.Lock()
		n := d.acquires[b.name]
		d.acquires[b.name]++
		d.mu.Unlock()
		if b.onAcquire != nil {
			b.onAcquire(n)
		}
		if n < len(b.answers) && b.answers[n] != nil {
			api.WriteError(w, b.answers[n])
			return
		}
		api.WriteJSON(w, 200, computeapi.RunTargetHandle{Handle: fmt.Sprintf("box:%s/%s1", b.name, b.name), Target: b.name, Kind: "box", State: "ready"})
	case len(parts) == 4 && parts[3] == "exec":
		if d.execErr != nil {
			api.WriteError(w, d.execErr)
			return
		}
		api.WriteJSON(w, 200, computeapi.RunTargetExec{Exit: 0})
	case len(parts) == 4 && parts[3] == "release":
		api.WriteJSON(w, 200, computeapi.RunTargetRelease{Handle: "box:" + parts[0] + "/" + parts[2], Released: true})
	default:
		api.WriteError(w, api.ErrNotImplemented(r.URL.Path))
	}
}

// wantRank fails unless placement ranks the bindings in this order: a walk
// test whose first binding is not asked first walks nothing.
func wantRank(t *testing.T, c *client.Client, want ...string) {
	t.Helper()
	got, err := computeclient.PlaceBox(context.Background(), c, computeclient.BoxWant{})
	if err != nil || strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("placement = %v, %v; want %v", got, err, want)
	}
}

func pushOf(repo string) pushFlags {
	return pushFlags{repo: repo, dir: boxpush.DefaultDest, maxMiB: 16}
}

// A refuses before it records, B takes the box: the bundle is made once
// (not once per binding), the push goes to B, and the handle is B's.
func TestSandboxCreateWalkPushesToTheBindingThatAnswered(t *testing.T) {
	repo := testRepo(t, "true")
	a := &walkBinding{name: "A", cpu: 1, answers: []*api.Error{refusedBeforeRecord("busy")}}
	b := &walkBinding{name: "B", cpu: 2}
	d, c := newWalkDaemon(t, a, b)
	wantRank(t, c, "A", "B")

	var log bytes.Buffer
	h, res, err := sandboxCreate(context.Background(), c, computeclient.BoxWant{}, computeapi.AcquireRunTargetRequest{RequestID: "k1", Ref: "HEAD"}, pushOf(repo), &log)
	if err != nil || res == nil {
		t.Fatalf("sandboxCreate = %+v, %v, %v\n%s", h, res, err, &log)
	}
	if h.Handle != "box:B/B1" {
		t.Errorf("handle = %q, want B's", h.Handle)
	}
	acq := d.only("POST", "/acquire")
	if got := strings.Join(targets(acq), ","); got != "A,B" {
		t.Fatalf("acquires went to %s, want A,B (a walk)", got)
	}
	for _, r := range acq {
		if a := r.acquire(t); a.RequestID != "k1" || a.Ref != "" {
			t.Errorf("acquire on %s = %s: want request k1 and the ref cleared (this checkout's commit is pushed in)", r.target(), r.body)
		}
		if r.bundles == "" || strings.Contains(r.bundles, ",") || r.bundles != acq[0].bundles {
			t.Errorf("bundles %q alive at the acquire on %s, want the one same bundle for the whole walk (made once)", r.bundles, r.target())
		}
	}
	execs := d.only("POST", "/exec")
	if len(execs) == 0 {
		t.Fatal("nothing was pushed")
	}
	for _, r := range execs {
		if r.target() != "B" || !strings.Contains(r.path, "/handles/B1/") {
			t.Errorf("push exec went to %s, want B's handle B1", r.path)
		}
	}
	if rel := d.only("POST", "/release"); len(rel) != 0 {
		t.Errorf("released %v: the box is the caller's", targets(rel))
	}
	if d.bundles() != "" {
		t.Errorf("bundle %s left behind", d.bundles())
	}
}

// Every binding refuses before it records: nothing was made, so the error
// carries no request id to retry with and MadeNothing holds. A refusal that
// is not marked ends the walk at its binding, and then the key is named.
func TestSandboxCreateWalkEveryBindingRefuses(t *testing.T) {
	t.Run("all refuse before record", func(t *testing.T) {
		repo := testRepo(t, "true")
		a := &walkBinding{name: "A", cpu: 1, answers: []*api.Error{refusedBeforeRecord("busy")}}
		b := &walkBinding{name: "B", cpu: 2, answers: []*api.Error{refusedBeforeRecord("away")}}
		d, c := newWalkDaemon(t, a, b)
		wantRank(t, c, "A", "B")
		_, res, err := sandboxCreate(context.Background(), c, computeclient.BoxWant{}, computeapi.AcquireRunTargetRequest{RequestID: "k9", Ref: "HEAD"}, pushOf(repo), io.Discard)
		if err == nil || res != nil {
			t.Fatalf("sandboxCreate = %+v, %v; want an error", res, err)
		}
		if !computeclient.MadeNothing(err) {
			t.Errorf("MadeNothing(%v) = false", err)
		}
		var e *api.Error
		if !errors.As(err, &e) || strings.Contains(e.Hint, "request-id") || strings.Contains(strings.ToLower(err.Error()), "request id") || strings.Contains(err.Error(), "k9") {
			t.Errorf("error %v (hint %q) names a request id: nothing was made, a retry needs no key", err, e.Hint)
		}
		for _, w := range []string{"A (", "B ("} {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("error %q does not name each binding's reason (%q)", err, w)
			}
		}
		if got := strings.Join(targets(d.only("POST", "/acquire")), ","); got != "A,B" {
			t.Errorf("acquires %s, want A,B", got)
		}
		if n := len(d.only("POST", "/exec")) + len(d.only("POST", "/release")); n != 0 {
			t.Errorf("%d exec/release calls with no box", n)
		}
	})
	t.Run("an unmarked failure ends the walk", func(t *testing.T) {
		repo := testRepo(t, "true")
		a := &walkBinding{name: "A", cpu: 1, answers: []*api.Error{api.Errorf(api.CodeInternal, "driver blew up")}}
		b := &walkBinding{name: "B", cpu: 2}
		d, c := newWalkDaemon(t, a, b)
		wantRank(t, c, "A", "B")
		_, _, err := sandboxCreate(context.Background(), c, computeclient.BoxWant{}, computeapi.AcquireRunTargetRequest{RequestID: "k8", Ref: "HEAD"}, pushOf(repo), io.Discard)
		var e *api.Error
		if !errors.As(err, &e) || e.Code != api.CodeInternal || !strings.Contains(e.Hint, "--request-id k8") {
			t.Errorf("err = %v, want internal with the retry key k8 in its hint", err)
		}
		if computeclient.MadeNothing(err) {
			t.Errorf("MadeNothing(%v): the box may still come up on A", err)
		}
		if got := strings.Join(targets(d.only("POST", "/acquire")), ","); got != "A" {
			t.Errorf("acquires %s, want only A: B may not be asked when A may have made a box", got)
		}
	})
}

// A walk past a Misfit: A checks refs out itself and the sandbox is --dirty,
// so prep passes (nothing asked of A) and B gets the box and the push.
func TestSandboxCreateWalkPastSelfMisfit(t *testing.T) {
	repo := testRepo(t, "true")
	if err := os.WriteFile(repo+"/msg.txt", []byte("uncommitted"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := &walkBinding{name: "A", cpu: 1, repo: "https://example.invalid/r.git"}
	b := &walkBinding{name: "B", cpu: 2}
	d, c := newWalkDaemon(t, a, b)
	wantRank(t, c, "A", "B")
	p := pushOf(repo)
	p.dirty = true
	h, res, err := sandboxCreate(context.Background(), c, computeclient.BoxWant{}, computeapi.AcquireRunTargetRequest{RequestID: "k2"}, p, io.Discard)
	if err != nil || res == nil {
		t.Fatalf("sandboxCreate = %+v, %v, %v", h, res, err)
	}
	if h.Handle != "box:B/B1" {
		t.Errorf("handle = %q, want B's", h.Handle)
	}
	if got := strings.Join(targets(d.only("POST", "/acquire")), ","); got != "B" {
		t.Errorf("acquires %s, want only B: A cannot serve --dirty and was never asked", got)
	}
	for _, r := range d.only("POST", "/exec") {
		if r.target() != "B" {
			t.Errorf("push exec went to %s, want B", r.path)
		}
	}
	// Both checking out themselves and --dirty: every binding misfits, nothing is made.
	b.repo = "https://example.invalid/b.git"
	_, _, err = sandboxCreate(context.Background(), c, computeclient.BoxWant{}, computeapi.AcquireRunTargetRequest{RequestID: "k3"}, p, io.Discard)
	if err == nil || !computeclient.MadeNothing(err) || strings.Contains(err.Error(), "request id") {
		t.Errorf("err = %v, want a made-nothing refusal with no key", err)
	}
	if n := len(d.only("POST", "/acquire")); n != 1 {
		t.Errorf("%d acquires in all, want just the first walk's one", n)
	}
}

func jobOpts(repo string) jobOptions {
	return jobOptions{repo: repo, argv: []string{"true"}, timeout: 5 * time.Minute, out: io.Discard, errw: io.Discard}
}

// A walk past a lifetime Misfit: A's cap is under --timeout plus two
// minutes, so it passes to B, which is asked for a lease within B's cap.
func TestJobRunWalkPastLifetimeMisfit(t *testing.T) {
	repo := testRepo(t, "true")
	a := &walkBinding{name: "A", cpu: 1, ttlMax: 400} // 5m + 2m = 420 s does not fit
	b := &walkBinding{name: "B", cpu: 2, ttlMax: 600} // 15 m of lease cut to its cap
	d, c := newWalkDaemon(t, a, b)
	d.execErr = api.Errorf(api.CodeBadRequest, "stop after the push starts")
	wantRank(t, c, "A", "B")
	r, err := jobRun(context.Background(), c, jobOpts(repo))
	if err == nil {
		t.Fatal("the fake box fails the push: want an error")
	}
	acq := d.only("POST", "/acquire")
	if got := strings.Join(targets(acq), ","); got != "B" {
		t.Fatalf("acquires %s, want only B (A's cap is a Misfit)", got)
	}
	if got := acq[0].acquire(t); got.TTLS != 600 || !strings.HasPrefix(got.RequestID, "job-") {
		t.Errorf("acquire on B = %s, want ttl_s 600 (B's cap)", acq[0].body)
	}
	if r.Target != "B" || r.Handle != "box:B/B1" {
		t.Errorf("result target %q handle %q, want B's", r.Target, r.Handle)
	}
	for _, x := range append(d.only("POST", "/exec"), d.only("POST", "/release")...) {
		if x.target() != "B" {
			t.Errorf("%s %s went to %s, want B", x.method, x.path, x.target())
		}
	}
	if rel := d.only("POST", "/release"); len(rel) != 1 || !r.Released {
		t.Errorf("releases %v, released=%v: the box is ended whatever happened", targets(rel), r.Released)
	}
	if d.bundles() != "" {
		t.Errorf("bundle not removed")
	}
}

// After the walk moved past A, B fails in a way that may have made a box
// (internal or unavailable, not marked refused-before-record): jobRun asks
// B again (the binding last asked) with the same request id and the ttl B
// was asked with, and releases what it made, on B.
func TestJobRunReAsksTheBindingLastAskedAndReleases(t *testing.T) {
	for _, code := range []string{api.CodeInternal, api.CodeUnavailable, ""} {
		for _, ctrlC := range []bool{code == ""} { // "": Ctrl-C, whose answer mayHaveMade rejects
			t.Run(fmt.Sprintf("%s ctrlc=%v", code, ctrlC), func(t *testing.T) {
				repo := testRepo(t, "true")
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				a := &walkBinding{name: "A", cpu: 1, answers: []*api.Error{refusedBeforeRecord("busy")}} // no cap: asked 15 m
				b := &walkBinding{name: "B", cpu: 2, ttlMax: 600, answers: []*api.Error{api.Errorf(code, "cut off")}}
				if ctrlC {
					// The fake ignores ctx, so B answers what mayHaveMade rejects:
					// only jobRun's own ctx.Err() check can send it back to B.
					b.answers = []*api.Error{api.Errorf(api.CodeBadRequest, "cut off by Ctrl-C")}
					b.onAcquire = func(n int) {
						if n == 0 {
							cancel() // Ctrl-C while B's first acquire is out
						}
					}
				}
				d, c := newWalkDaemon(t, a, b)
				wantRank(t, c, "A", "B")
				r, err := jobRun(ctx, c, jobOpts(repo))
				acq := d.only("POST", "/acquire")
				if got := strings.Join(targets(acq), ","); got != "A,B,B" {
					t.Fatalf("acquires %s, want A,B,B (the walk, then B again)", got)
				}
				a0, b0, b1 := acq[0].acquire(t), acq[1].acquire(t), acq[2].acquire(t)
				if !strings.HasPrefix(a0.RequestID, "job-") || a0.RequestID != b0.RequestID || b0.RequestID != b1.RequestID {
					t.Errorf("request ids %q %q %q, want one key throughout", a0.RequestID, b0.RequestID, b1.RequestID)
				}
				if a0.TTLS != 900 || b0.TTLS != 600 || b1.TTLS != 600 {
					t.Errorf("ttl_s %d %d %d, want 900 (A uncapped), 600 and 600 (the re-ask keeps what B was asked)", a0.TTLS, b0.TTLS, b1.TTLS)
				}
				rel := d.only("POST", "/release")
				if len(rel) != 1 || rel[0].target() != "B" || !strings.Contains(rel[0].path, "/handles/B1/") {
					t.Errorf("releases %+v, want one on B's handle", rel)
				}
				if len(d.only("POST", "/exec")) != 0 {
					t.Error("the code was pushed into a box that was only released")
				}
				if !r.Released || r.Handle != "box:B/B1" || r.Target != "B" {
					t.Errorf("result %+v, want B's box released", r)
				}
				switch {
				case ctrlC && (!errors.Is(err, errInterrupted) || r.State != "interrupted"):
					t.Errorf("err %v state %q, want interrupted", err, r.State)
				case !ctrlC && (err == nil || !strings.Contains(err.Error(), "released")):
					t.Errorf("err = %v, want the first failure, noting the box is released", err)
				}
			})
		}
	}
}
