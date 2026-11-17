package fulltest

// The oracle for an
// outside collaborator is a guest of his own project ("solver", guest_issues
// on). Under a test identity (never his real one) it shows he reads and
// writes solver items and mails a bead's holder, and cannot read harbor's
// beads, another guest's asks (or their answers) or any box.

import (
	"context"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/computeapi"
	. "github.com/sengirunners/potluck/kit/server"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runtarget"
)

// TestMain lets this test binary serve as the reference box driver
// (refdriver.Install execs it), as the runtarget tests do.
func TestMain(m *testing.M) {
	if code, ok := refdriver.Main(); ok {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

const oraclePin = "ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"

func TestVidaOnZ3Oracle(t *testing.T) {
	ctx := context.Background()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	dm, err := New(ctx, cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)

	// A box driver, so harbor can have a real box binding.
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	repo, err := refdriver.NewRepo(fix, "#!/bin/sh\nexit 0\n")
	if err != nil {
		t.Fatal(err)
	}
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

	at := func(project, as string) *client.Client {
		c := client.WithHandler(dm.SocketHandler())
		c.Project, c.ProjectFrom, c.As = project, "flag", as
		return c
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	owner := at("", "")
	// harbor: its own beads and a box.
	_, err = owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor", RepoRules: api.RepoRules{Remotes: []string{repo.Remote}}})
	must(err)
	oc := at("harbor", "")
	_, err = orchclient.SetupIssues(ctx, oc, "harbor")
	must(err)
	hidden, err := orchclient.CreateIssue(ctx, oc, orchapi.NewIssueRequest{Title: "HIDDEN harbor bead"})
	must(err)
	_, err = computeclient.BindRunTarget(ctx, oc, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "sandbox", Driver: "ref", Kind: "box",
		Isolation: "ephemeral", Images: []string{oraclePin}, Repo: repo.Remote, Branch: "main", Config: map[string]string{"remote": repo.Remote}}})
	must(err)

	// solver: the guests' own project, issues writable by its guests, beads
	// keeping their solver- ids, served to zm work as bd:.
	_, err = owner.CreateProject(ctx, api.NewProjectRequest{Slug: "solver", RepoRules: api.RepoRules{Remotes: []string{"github.com/example-org/solver"}}})
	must(err)
	sv := at("solver", "")
	_, err = orchclient.SetupIssues(ctx, sv, "solver")
	must(err)
	on := true
	p, err := owner.UpdateProject(ctx, "solver", api.UpdateProjectRequest{GuestIssues: &on})
	must(err)
	if !p.GuestIssues {
		t.Fatal("guest_issues not set")
	}
	epic, err := orchclient.CreateIssue(ctx, sv, orchapi.NewIssueRequest{ID: "solver-wop", Title: "Solver as a lego brick", IssueType: "epic"})
	must(err)
	if epic.ID != "solver-wop" {
		t.Fatalf("explicit id: %s", epic.ID)
	}
	res, err := orchclient.AddTracker(ctx, sv, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "bd", WaitSeconds: 10})
	must(err)
	if !res.OK {
		t.Fatalf("native tracker: %+v", res)
	}

	// Our side: a session in solver holds the bead (the steward, or whoever).
	steward, err := orchclient.Register(ctx, sv, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "steward-uuid", Title: "solver steward"})
	must(err)
	st := at("solver", steward.Address)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err = orchclient.Claim(ctx, st, "bd:solver-wop", orchapi.ClaimRequest{}); err == nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	must(err)

	// Two test guests of solver (tokens over the network, as from a laptop).
	guest := func(as string) *client.Client {
		tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: as, Project: "solver", Role: "guest"})
		must(err)
		c := client.WithHandler(dm.Handler)
		c.Token, c.Project, c.ProjectFrom = tok.Secret, "solver", "flag"
		return c
	}
	v, g2 := guest("vida-test"), guest("guest-two")

	// 1. Reads and writes solver items.
	got, err := orchclient.Issue(ctx, v, "solver-wop")
	if err != nil || got.Title != "Solver as a lego brick" {
		t.Fatalf("guest reads a solver bead: %+v %v", got.Issue, err)
	}
	mine, err := orchclient.CreateIssue(ctx, v, orchapi.NewIssueRequest{Title: "Bench the portfolio on fixture 3", Parent: "solver-wop"})
	if err != nil || !strings.HasPrefix(mine.ID, "solver-wop.") {
		t.Fatalf("guest creates a solver bead: %+v %v", mine, err)
	}
	if _, err := orchclient.CommentIssue(ctx, v, "solver-wop", orchapi.IssueCommentRequest{Text: "blocked on CI: the node 20 job times out"}); err != nil {
		t.Fatalf("guest comments: %v", err)
	}

	// 2. Mails a bead's holder: it lands in the holder's inbox.
	m, err := orchclient.Send(ctx, v, orchapi.SendMailRequest{To: "bd:solver-wop", Body: "can you look at the node 20 timeout?"})
	if err != nil || m.Recipient != steward.Address {
		t.Fatalf("guest mails the holder: %+v %v", m, err)
	}
	in, err := orchclient.Inbox(ctx, st, orchapi.InboxQuery{Project: "solver"})
	must(err)
	if !hasMail(in, m.ID) {
		t.Fatalf("holder's inbox lacks the guest's mail: %+v", in)
	}

	// 3. His Decisions: an item labelled owner:<him> (a step he holds) waits on
	// him (ask:<who> with no ask goes to agents to write up).
	q, err := orchclient.CreateIssue(ctx, sv, orchapi.NewIssueRequest{Title: "Pick the fixture for the hosted demo", Labels: []string{"owner:vida-test"}})
	must(err)
	deadline = time.Now().Add(10 * time.Second)
	for !inboxHas(t, v, "bd:"+q.ID) {
		if time.Now().After(deadline) {
			t.Fatal("an owner:<guest> item never reached the guest's Decisions")
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 4. Never harbor's beads, nor the project.
	if _, err := orchclient.Issue(ctx, v, hidden.ID); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("guest reads a harbor bead: %v", err)
	}
	for _, pr := range must2(v.Projects(ctx))(t) {
		if pr.Slug != "solver" {
			t.Fatalf("guest sees project %q", pr.Slug)
		}
	}
	toHarbor := client.WithHandler(dm.Handler)
	toHarbor.Token, toHarbor.Project, toHarbor.ProjectFrom = v.Token, "harbor", "flag"
	if _, err := orchclient.CreateIssue(ctx, toHarbor, orchapi.NewIssueRequest{Title: "x"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("guest writes harbor: %v", err)
	}

	// 5. Never another guest's ask, or its answer.
	ask, err := orchclient.NewAsk(ctx, g2, orchapi.NewAskRequest{Card: &orchapi.AskCard{
		Context:  "I want to take next month off for a family matter and need to plan my solver work around it.",
		Question: "Can I take next month off?",
		Choices: []orchapi.AskChoice{{Label: "Yes", Pros: "rested", Cons: "the hosted demo slips"},
			{Label: "No", Pros: "the demo ships", Cons: "no break"}},
		Recommend: "a", Why: "A month off costs one demo date and nothing else.", Searched: "earlier answers: none on leave",
	}})
	must(err)
	if _, err := orchclient.Choose(ctx, owner, "solver", ask.ID, orchapi.DecideRequest{Decision: "a", Text: "HIDDEN approved, take care"}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.GetAsk(ctx, v, ask.ID); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("guest reads another guest's ask: %v", err)
	}
	for _, a := range must2(orchclient.Asks(ctx, v, "solver", false))(t) {
		if a.ID == ask.ID {
			t.Fatal("guest lists another guest's ask")
		}
	}
	// The asker, a person, reads the answer on the ask: zm mails no person a
	// receipt (SPEC 9 "No notice about what you just did").
	if got := must2(orchclient.GetAsk(ctx, g2, ask.ID))(t); got.Decision == nil || got.Decision.Text != "HIDDEN approved, take care" {
		t.Fatalf("the asker never got the answer: %+v", got.Decision)
	}
	if hasBody(must2(orchclient.Mail(ctx, v, orchapi.MailQuery{}))(t), "HIDDEN") {
		t.Fatal("guest reads the answer to another guest's ask")
	}

	// 6. Never a box: none listed in solver, harbor's is hidden like missing.
	if l, err := computeclient.RunTargets(ctx, v, false); err != nil || len(l.Targets) != 0 {
		t.Fatalf("guest lists boxes: %+v %v", l, err)
	}
	if _, err := computeclient.RunTargets(ctx, toHarbor, false); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("guest lists harbor's boxes: %v", err)
	}
	for _, c := range []*client.Client{v, toHarbor} {
		if _, err := computeclient.AcquireRunTarget(ctx, c, "sandbox", computeapi.AcquireRunTargetRequest{RequestID: "v1", Ref: "main", TTLS: 60}); err == nil {
			t.Fatalf("guest acquired a box in %s", c.Project)
		}
	}
}

func hasMail(ms []orchapi.Mail, id string) bool {
	for _, m := range ms {
		if m.ID == id {
			return true
		}
	}
	return false
}

func hasBody(ms []orchapi.Mail, s string) bool {
	for _, m := range ms {
		if strings.Contains(m.Body, s) {
			return true
		}
	}
	return false
}

func inboxHas(t *testing.T, c *client.Client, work string) bool {
	t.Helper()
	in, err := orchclient.Decisions(context.Background(), c, "solver", "open")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range in.Decisions {
		if x.Work != nil && x.Work.ID == work {
			return true
		}
	}
	return false
}

func must2[T any](v T, err error) func(*testing.T) T {
	return func(t *testing.T) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}
