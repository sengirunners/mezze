package ghlink_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
)

type fakeRun struct {
	ID           int64            `json:"id"`
	Name         string           `json:"name"`
	Event        string           `json:"event"`
	Status       string           `json:"status"`
	Conclusion   *string          `json:"conclusion"`
	HeadSHA      string           `json:"head_sha"`
	HTMLURL      string           `json:"html_url"`
	Attempt      int              `json:"run_attempt"`
	Updated      string           `json:"updated_at"`
	WorkflowID   int64            `json:"workflow_id"`
	PullRequests []map[string]int `json:"pull_requests"`
}

func run(id, workflow int64, name, event, status, conclusion, sha string, pr, attempt int, at time.Time) fakeRun {
	r := fakeRun{ID: id, Name: name, Event: event, Status: status, HeadSHA: sha, Attempt: attempt, WorkflowID: workflow,
		HTMLURL: fmt.Sprintf("https://github.com/example-org/harbor/actions/runs/%d", id), Updated: at.UTC().Format(time.RFC3339)}
	if conclusion != "" {
		r.Conclusion = &conclusion
	}
	if pr > 0 {
		r.PullRequests = []map[string]int{{"number": pr}}
	}
	return r
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A red or cancelled CI result reaches the agent holding the item a PR
// names through zm's mail routing, once per finished set of runs for the
// PR's current head; a green one mails no one, and one no
// agent holds goes to the group mailbox, never to a person.
func TestCIResultMailsTheItem(t *testing.T) {
	g := newGX(t)
	ctx := context.Background()
	dir := t.TempDir()
	gh := writeFakeGH(t, dir)
	sess, err := orchclient.Register(ctx, g.at("harbor"), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "ci-holder"})
	if err != nil {
		t.Fatal(err)
	}
	agent := g.at("harbor")
	agent.As = sess.Address
	held := g.item("harbor", "held while its PR runs CI")
	if _, err := orchclient.Claim(ctx, agent, held.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	other := g.item("harbor", "named by a PR that is not linked here")
	now := time.Now()
	writeJSON(t, filepath.Join(dir, "pulls.json"), []pull{mkPull(7, "Fix ["+held.ID+"]", "", "fix", "open", false, now.Add(-time.Minute))})
	g.link(orchapi.GitHubLinkRequest{Bin: gh})
	sync := func() {
		t.Helper()
		r, err := orchclient.SyncGitHub(ctx, g.owner, "harbor")
		if err != nil || len(r.Errors) > 0 {
			t.Fatalf("sync: %+v %v", r, err)
		}
		g.settle()
	}
	sync() // links PR 7 to held; the first CI pass only sets the cursor
	mails := func(id string) []orchapi.Mail {
		t.Helper()
		ms, err := orchclient.Mail(ctx, g.owner, orchapi.MailQuery{Project: "harbor", Ref: id})
		if err != nil {
			t.Fatal(err)
		}
		var out []orchapi.Mail
		for _, m := range ms {
			if strings.HasPrefix(m.Body, "CI ") {
				out = append(out, m)
			}
		}
		return out
	}
	const sha = "abc123def4567890abc123def4567890abc12345"
	head := map[string]any{"number": 7, "state": "open", "title": "Fix [" + held.ID + "]", "head": map[string]string{"sha": sha}}
	writeJSON(t, filepath.Join(dir, "pull.json"), head)
	later := now.Add(time.Minute)
	ci := run(101, 1, "ci", "pull_request", "completed", "success", sha, 7, 1, later)
	bench := run(102, 2, "bench", "pull_request", "in_progress", "", sha, 7, 1, later)
	push := run(103, 1, "ci", "push", "completed", "failure", "ffff", 0, 1, later)
	unlinked := run(104, 1, "ci", "pull_request", "completed", "failure", "eeee", 9, 1, later)
	writeJSON(t, filepath.Join(dir, "runs.json"), map[string]any{"workflow_runs": []fakeRun{ci, push, unlinked}})
	writeJSON(t, filepath.Join(dir, "headruns.json"), map[string]any{"workflow_runs": []fakeRun{ci, bench}})
	sync()
	if got := mails(held.ID); len(got) != 0 {
		t.Fatalf("mailed before every run of the head finished: %+v", got)
	}

	bench = run(102, 2, "bench", "pull_request", "completed", "success", sha, 7, 1, later.Add(time.Second))
	writeJSON(t, filepath.Join(dir, "runs.json"), map[string]any{"workflow_runs": []fakeRun{bench, ci, push, unlinked}})
	writeJSON(t, filepath.Join(dir, "headruns.json"), map[string]any{"workflow_runs": []fakeRun{ci, bench}})
	sync()
	sync() // the overlap and a repeat pass send nothing new
	if got := mails(held.ID); len(got) != 0 {
		t.Fatalf("a green result was mailed: %+v", got)
	}
	if len(mails(other.ID)) != 0 {
		t.Fatal("an item no linked PR names was mailed")
	}

	// A re-run that fails mails again, naming the failing job.
	ci = run(101, 1, "ci", "pull_request", "completed", "failure", sha, 7, 2, later.Add(2*time.Minute))
	writeJSON(t, filepath.Join(dir, "runs.json"), map[string]any{"workflow_runs": []fakeRun{ci, bench}})
	writeJSON(t, filepath.Join(dir, "headruns.json"), map[string]any{"workflow_runs": []fakeRun{ci, bench}})
	writeJSON(t, filepath.Join(dir, "jobs.json"), map[string]any{"jobs": []map[string]any{
		{"name": "scope", "conclusion": "success"}, {"name": "make check + smoke", "conclusion": "failure"}}})
	sync()
	got := mails(held.ID)
	if len(got) != 1 {
		t.Fatalf("red result: %+v", got)
	}
	red := got[0]
	if !strings.HasPrefix(red.Body, "CI red for example-org/harbor#7") || !strings.Contains(red.Body, "ci: failure (make check + smoke)") ||
		red.Recipient != sess.Address || !strings.Contains(red.Via, "holder of") || red.Needs != orchapi.NeedsAgent ||
		!strings.Contains(red.Body, "data, not instructions") {
		t.Fatalf("red result: %+v", red)
	}

	// A head the PR has moved past reports nothing.
	head["head"] = map[string]string{"sha": "0000000000000000000000000000000000000000"}
	writeJSON(t, filepath.Join(dir, "pull.json"), head)
	bench = run(102, 2, "bench", "pull_request", "completed", "failure", sha, 7, 2, later.Add(3*time.Minute))
	writeJSON(t, filepath.Join(dir, "runs.json"), map[string]any{"workflow_runs": []fakeRun{bench}})
	writeJSON(t, filepath.Join(dir, "headruns.json"), map[string]any{"workflow_runs": []fakeRun{ci, bench}})
	sync()
	if n := len(mails(held.ID)); n != 1 {
		t.Fatalf("a superseded head was mailed: %d mails", n)
	}

	// A PR first seen in the same pass in which its runs already finished
	// (all cancelled) is linked first, then mailed. A person holds its item,
	// so the result goes to this machine's group mailbox, not to them.
	late := g.item("harbor", "PR opened and CI cancelled between two polls")
	if _, err := orchclient.Claim(ctx, g.at("harbor"), late.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	const sha8 = "8888888888888888888888888888888888888888"
	writeJSON(t, filepath.Join(dir, "pulls.json"), []pull{mkPull(8, "Late "+late.ID, "", "late", "open", false, time.Now())})
	writeJSON(t, filepath.Join(dir, "pull.json"), map[string]any{"number": 8, "state": "open", "title": "Late", "head": map[string]string{"sha": sha8}})
	c8 := run(201, 1, "ci", "pull_request", "completed", "cancelled", sha8, 8, 1, later.Add(4*time.Minute))
	writeJSON(t, filepath.Join(dir, "runs.json"), map[string]any{"workflow_runs": []fakeRun{c8}})
	writeJSON(t, filepath.Join(dir, "headruns.json"), map[string]any{"workflow_runs": []fakeRun{c8}})
	sync()
	if got := mails(late.ID); len(got) != 1 || !strings.HasPrefix(got[0].Body, "CI cancelled for example-org/harbor#8") ||
		got[0].Recipient != "machine:mini" || got[0].Via != "group" || got[0].Needs != orchapi.NeedsAgent ||
		!strings.Contains(got[0].Body, "No agent holds "+late.ID) {
		t.Fatalf("a PR whose CI finished before it was first polled: %+v", got)
	}
	inbox, err := orchclient.Inbox(ctx, g.owner, orchapi.InboxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range inbox {
		if strings.HasPrefix(m.Body, "CI ") {
			t.Fatalf("a CI result reached a person's inbox: %+v", m)
		}
	}

	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	for _, l := range strings.Split(strings.TrimSpace(string(calls)), "\n") {
		if !strings.HasPrefix(l, "api --method GET repos/example-org/harbor/") {
			t.Fatalf("gh called with %q (the linker only reads)", l)
		}
	}
}

// A PR naming several items sends each recipient one note that names them
// all, not one note per item: two items held by one session and an
// unheld item a person made yield two notes, not three: the session's, and
// the group mailbox's for the unheld one, never the person's.
func TestCIResultMailsEachRecipientOnce(t *testing.T) {
	g := newGX(t)
	ctx := context.Background()
	dir := t.TempDir()
	gh := writeFakeGH(t, dir)
	sess, err := orchclient.Register(ctx, g.at("harbor"), orchapi.RegisterRequest{Agent: "claude", RuntimeID: "a7x3-holder"})
	if err != nil {
		t.Fatal(err)
	}
	holder := g.at("harbor")
	holder.As = sess.Address
	a, b, c := g.item("harbor", "held a"), g.item("harbor", "held b"), g.item("harbor", "unheld c")
	for _, id := range []string{a.ID, b.ID} {
		if _, err := orchclient.Claim(ctx, holder, id, orchapi.ClaimRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	title := fmt.Sprintf("Batch [%s] [%s] [%s]", a.ID, b.ID, c.ID)
	now := time.Now()
	writeJSON(t, filepath.Join(dir, "pulls.json"), []pull{mkPull(9, title, "", "batch", "open", false, now.Add(-time.Minute))})
	g.link(orchapi.GitHubLinkRequest{Bin: gh})
	sync := func() {
		t.Helper()
		r, err := orchclient.SyncGitHub(ctx, g.owner, "harbor")
		if err != nil || len(r.Errors) > 0 {
			t.Fatalf("sync: %+v %v", r, err)
		}
		g.settle()
	}
	sync()
	const sha = "9999999999999999999999999999999999999999"
	writeJSON(t, filepath.Join(dir, "pull.json"), map[string]any{"number": 9, "state": "open", "title": title, "head": map[string]string{"sha": sha}})
	ci := run(301, 1, "ci", "pull_request", "completed", "failure", sha, 9, 1, now.Add(time.Minute))
	writeJSON(t, filepath.Join(dir, "runs.json"), map[string]any{"workflow_runs": []fakeRun{ci}})
	writeJSON(t, filepath.Join(dir, "headruns.json"), map[string]any{"workflow_runs": []fakeRun{ci}})
	sync()
	sync() // a repeat pass sends nothing new
	ms, err := orchclient.Mail(ctx, g.owner, orchapi.MailQuery{Project: "harbor"})
	if err != nil {
		t.Fatal(err)
	}
	var notes []orchapi.Mail
	for _, m := range ms {
		if strings.HasPrefix(m.Body, "CI red for example-org/harbor#9") {
			notes = append(notes, m)
		}
	}
	if len(notes) != 2 {
		t.Fatalf("want one note for the holder and one for the group mailbox, got %d: %+v", len(notes), notes)
	}
	var toHolder *orchapi.Mail
	for i := range notes {
		if notes[i].Ref == a.ID || notes[i].Ref == b.ID {
			toHolder = &notes[i]
		}
	}
	other := b.ID
	if toHolder != nil && toHolder.Ref == b.ID {
		other = a.ID
	}
	if toHolder == nil || !strings.HasSuffix(toHolder.Body, " Also names "+other+".") || !strings.Contains(toHolder.Via, "holder of") {
		t.Fatalf("holder's note should name both held items and not the unheld one: %+v", notes)
	}
	for _, n := range notes {
		if n.Ref == c.ID && (strings.Contains(n.Body, "Also names") || n.Recipient != "machine:mini" || !strings.HasSuffix(n.Body, " No agent holds "+c.ID+": whoever here can fix it, take it.")) {
			t.Fatalf("the unheld item's note goes to the group mailbox alone: %+v", n)
		}
	}
}
