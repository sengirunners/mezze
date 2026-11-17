package ghlink_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// prEvents are the dev.zm.github.pr.* events in project's log, oldest first.
func (g *gx) prEvents(project string) []api.Event {
	g.t.Helper()
	var out []api.Event
	err := g.owner.Events(context.Background(), api.EventQuery{Project: project, Types: "dev.zm.github.pr.", Since: "0", NoLive: true},
		func(e api.Event) error { out = append(out, e); return nil })
	if err != nil {
		g.t.Fatal(err)
	}
	return out
}

func prData(t *testing.T, e api.Event) orchapi.GitHubPREvent {
	t.Helper()
	var d orchapi.GitHubPREvent
	b, _ := json.Marshal(e.Data)
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func types(es []api.Event) []string {
	var out []string
	for _, e := range es {
		out = append(out, e.Type)
	}
	return out
}

func wantTypes(t *testing.T, got []api.Event, want ...string) {
	t.Helper()
	if fmt.Sprint(types(got)) != fmt.Sprint(want) {
		t.Fatalf("PR events: got %v, want %v", types(got), want)
	}
}

func headPull(n int, title, sha string, draft bool, state string, merged bool, at time.Time) pull {
	p := mkPull(n, title, "", "feat/x", state, merged, at)
	p.Head.SHA, p.Draft, p.User.Login = sha, draft, "octo"
	return p
}

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// The receiver: a draft is not ready; ready_for_review is; a new head on a
// ready PR moves; a merge closes. Redeliveries and replays fire nothing.
func TestPREventsFromReceiver(t *testing.T) {
	g := newGX(t)
	it := g.item("harbor", "review me")
	g.link(orchapi.GitHubLinkRequest{})
	t0 := time.Now().Add(-time.Hour)
	title := "Fix [" + it.ID + "]"
	step := func(delivery, action string, p pull) {
		t.Helper()
		if st, b := g.deliver("pull_request", delivery, g.secret, prEvent(action, p, "example-org/harbor")); st != 202 {
			t.Fatalf("%s: %d %s", action, st, b)
		}
		g.settle()
	}
	step("d-1", "opened", headPull(5, title, shaA, true, "open", false, t0))
	wantTypes(t, g.prEvents("harbor"))
	step("d-2", "ready_for_review", headPull(5, title, shaA, false, "open", false, t0.Add(time.Minute)))
	wantTypes(t, g.prEvents("harbor"), orchapi.EventGitHubPRReady)
	step("d-2", "ready_for_review", headPull(5, title, shaA, false, "open", false, t0.Add(time.Minute)))    // redelivery
	step("d-3", "edited", headPull(5, title+" ", shaA, false, "open", false, t0.Add(2*time.Minute)))        // same head
	step("d-4", "ready_for_review", headPull(5, title, shaA, false, "open", false, t0.Add(30*time.Second))) // older
	wantTypes(t, g.prEvents("harbor"), orchapi.EventGitHubPRReady)
	step("d-5", "synchronize", headPull(5, title, shaB, false, "open", false, t0.Add(3*time.Minute)))
	step("d-6", "closed", headPull(5, title, shaB, false, "closed", true, t0.Add(4*time.Minute)))
	evs := g.prEvents("harbor")
	wantTypes(t, evs, orchapi.EventGitHubPRReady, orchapi.EventGitHubPRHeadMoved, orchapi.EventGitHubPRClosed)
	ready, moved, closed := prData(t, evs[0]), prData(t, evs[1]), prData(t, evs[2])
	if ready.Repo != "example-org/harbor" || ready.Number != 5 || ready.HeadSHA != shaA || ready.Base != "main" || ready.Draft ||
		ready.Author != "octo" || ready.URL != "https://github.com/example-org/harbor/pull/5" || ready.Title != title ||
		len(ready.Items) != 1 || ready.Items[0] != it.ID || evs[0].Subject != "example-org/harbor#5" {
		t.Fatalf("ready: %+v %+v", ready, evs[0])
	}
	if moved.HeadSHA != shaB || moved.PrevHeadSHA != shaA {
		t.Fatalf("head_moved: %+v", moved)
	}
	if closed.State != "merged" || closed.HeadSHA != shaB {
		t.Fatalf("closed: %+v", closed)
	}
	// Another project's log carries none of it.
	wantTypes(t, g.prEvents("side"))
}

// Polling alone fires the same transitions, and a poll that sees a PR the
// receiver already delivered (newer updated_at, same head) fires nothing
// more. A repeated transition at the same head is deduped.
func TestPREventsFromPollAndDedupe(t *testing.T) {
	g := newGX(t)
	dir := t.TempDir()
	gh := writeFakeGH(t, dir)
	g.link(orchapi.GitHubLinkRequest{Bin: gh})
	now := time.Now()
	sync := func(pulls ...pull) {
		t.Helper()
		writeJSON(t, filepath.Join(dir, "pulls.json"), pulls)
		if r, err := orchclient.SyncGitHub(context.Background(), g.owner, "harbor"); err != nil || len(r.Errors) > 0 {
			t.Fatalf("sync: %+v %v", r, err)
		}
		g.settle()
	}
	// Webhook first, then a poll of the same PR: one ready.
	if st, b := g.deliver("pull_request", "w-1", g.secret, prEvent("opened", headPull(8, "No item", shaA, false, "open", false, now.Add(-5*time.Minute)), "example-org/harbor")); st != 202 {
		t.Fatalf("opened: %d %s", st, b)
	}
	g.settle()
	sync(headPull(8, "No item (labelled)", shaA, false, "open", false, now.Add(-4*time.Minute)))
	wantTypes(t, g.prEvents("harbor"), orchapi.EventGitHubPRReady)
	if d := prData(t, g.prEvents("harbor")[0]); len(d.Items) != 0 || d.Items == nil {
		t.Fatalf("a PR naming no item: items %#v", d.Items)
	}

	// Poll only: a new head, closed unmerged, reopened (ready at a head no
	// ready named yet), closed again at the same head (deduped per head and
	// type), and the poll overlap.
	sync(headPull(8, "No item", shaB, false, "open", false, now.Add(-3*time.Minute)))
	sync(headPull(8, "No item", shaB, false, "closed", false, now.Add(-2*time.Minute)))
	sync(headPull(8, "No item", shaB, false, "open", false, now.Add(-90*time.Second)))
	sync(headPull(8, "No item", shaB, false, "closed", false, now.Add(-time.Minute)))
	sync(headPull(8, "No item", shaB, false, "closed", false, now.Add(-time.Minute)))
	evs := g.prEvents("harbor")
	wantTypes(t, evs, orchapi.EventGitHubPRReady, orchapi.EventGitHubPRHeadMoved, orchapi.EventGitHubPRClosed, orchapi.EventGitHubPRReady)
	if d := prData(t, evs[2]); d.State != "closed" || d.HeadSHA != shaB {
		t.Fatalf("closed: %+v", d)
	}
}

// Settled checks on a PR's head publish checks_settled once per outcome,
// whether or not the PR names an item.
func TestPREventsChecksSettled(t *testing.T) {
	g := newGX(t)
	dir := t.TempDir()
	gh := writeFakeGH(t, dir)
	now := time.Now()
	writeJSON(t, filepath.Join(dir, "pulls.json"), []pull{headPull(7, "No item", shaA, false, "open", false, now.Add(-time.Minute))})
	g.link(orchapi.GitHubLinkRequest{Bin: gh})
	sync := func() {
		t.Helper()
		if r, err := orchclient.SyncGitHub(context.Background(), g.owner, "harbor"); err != nil || len(r.Errors) > 0 {
			t.Fatalf("sync: %+v %v", r, err)
		}
		g.settle()
	}
	sync() // ready; the first CI pass sets the cursor
	writeJSON(t, filepath.Join(dir, "pull.json"), headPull(7, "No item", shaA, false, "open", false, now.Add(-time.Minute)))
	later := now.Add(time.Minute)
	ci := run(201, 1, "ci", "pull_request", "completed", "success", shaA, 7, 1, later)
	bench := run(202, 2, "bench", "pull_request", "in_progress", "", shaA, 7, 1, later)
	writeJSON(t, filepath.Join(dir, "runs.json"), map[string]any{"workflow_runs": []fakeRun{ci}})
	writeJSON(t, filepath.Join(dir, "headruns.json"), map[string]any{"workflow_runs": []fakeRun{ci, bench}})
	sync()
	wantTypes(t, g.prEvents("harbor"), orchapi.EventGitHubPRReady) // bench still running
	bench = run(202, 2, "bench", "pull_request", "completed", "failure", shaA, 7, 1, later.Add(time.Second))
	writeJSON(t, filepath.Join(dir, "runs.json"), map[string]any{"workflow_runs": []fakeRun{bench, ci}})
	writeJSON(t, filepath.Join(dir, "headruns.json"), map[string]any{"workflow_runs": []fakeRun{ci, bench}})
	sync()
	sync() // a repeat pass fires nothing
	evs := g.prEvents("harbor")
	wantTypes(t, evs, orchapi.EventGitHubPRReady, orchapi.EventGitHubPRChecksSettled)
	if d := prData(t, evs[1]); d.Conclusion != "failure" || d.HeadSHA != shaA || d.Number != 7 || d.Author != "octo" {
		t.Fatalf("checks_settled: %+v", d)
	}
	// A re-run that passes is a new outcome.
	bench = run(202, 2, "bench", "pull_request", "completed", "success", shaA, 7, 2, later.Add(2*time.Minute))
	writeJSON(t, filepath.Join(dir, "runs.json"), map[string]any{"workflow_runs": []fakeRun{bench, ci}})
	writeJSON(t, filepath.Join(dir, "headruns.json"), map[string]any{"workflow_runs": []fakeRun{ci, bench}})
	sync()
	evs = g.prEvents("harbor")
	wantTypes(t, evs, orchapi.EventGitHubPRReady, orchapi.EventGitHubPRChecksSettled, orchapi.EventGitHubPRChecksSettled)
	if d := prData(t, evs[2]); d.Conclusion != "success" {
		t.Fatalf("re-run: %+v", d)
	}
}

// A project webhook filtered to dev.zm.github.pr. gets one signed CloudEvent
// per transition (the Mezze app pr-review-land consumer's subscription).
func TestPREventsReachAProjectWebhook(t *testing.T) {
	g := newGX(t)
	// Start the webhooks module without importing another brick.
	for _, m := range g.dm.Modules() {
		if s, ok := m.(interface{ Start(context.Context) error }); ok && fmt.Sprintf("%T", m) == "*webhooks.Service" {
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			if err := s.Start(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	var mu sync.Mutex
	var got []struct {
		ev  api.Event
		sig string
		raw []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var ev api.Event
		_ = json.Unmarshal(b, &ev)
		mu.Lock()
		got = append(got, struct {
			ev  api.Event
			sig string
			raw []byte
		}{ev, r.Header.Get(orchapi.HeaderHubSignature), b})
		mu.Unlock()
	}))
	t.Cleanup(srv.Close)
	hook, err := orchclient.AddWebhook(context.Background(), g.owner, orchapi.NewWebhookRequest{URL: srv.URL, Project: "harbor",
		Events: []string{"dev.zm.github.pr."}, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	it := g.item("harbor", "work event: not this hook's")
	g.link(orchapi.GitHubLinkRequest{})
	t0 := time.Now().Add(-time.Hour)
	title := "Fix [" + it.ID + "]"
	for i, p := range []pull{
		headPull(3, title, shaA, false, "open", false, t0),
		headPull(3, title, shaA, false, "open", false, t0.Add(time.Minute)),
		headPull(3, title, shaB, false, "open", false, t0.Add(2*time.Minute)),
		headPull(3, title, shaB, false, "closed", true, t0.Add(3*time.Minute)),
	} {
		if st, b := g.deliver("pull_request", fmt.Sprintf("h-%d", i), g.secret, prEvent("synchronize", p, "example-org/harbor")); st != 202 {
			t.Fatalf("delivery %d: %d %s", i, st, b)
		}
		g.settle()
	}
	n := func() int { mu.Lock(); defer mu.Unlock(); return len(got) }
	eventually(t, "three deliveries", func() bool { return n() >= 3 })
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	var ts []string
	for _, d := range got {
		ts = append(ts, d.ev.Type)
		if d.sig != sign(hook.Secret, d.raw) {
			t.Fatalf("%s: X-Hub-Signature-256 does not verify", d.ev.Type)
		}
	}
	if fmt.Sprint(ts) != fmt.Sprint([]string{orchapi.EventGitHubPRReady, orchapi.EventGitHubPRHeadMoved, orchapi.EventGitHubPRClosed}) {
		t.Fatalf("deliveries: %v", ts)
	}
}
