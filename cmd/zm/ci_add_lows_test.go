package main

// Review lows on zm ci add, compute_setup and compute_pause,
// each with a test that fails without its fix.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sengirunners/mezze/internal/mcp"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
	"github.com/spf13/cobra"
)

func ciAddJSON(t *testing.T, args ...string) ciAddResult {
	t.Helper()
	raw, _, _ := ciAddRun(t, true, args...)
	var res ciAddResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	return res
}

func ciAddRoot() *cobra.Command {
	r := &cobra.Command{Use: "zm", SilenceErrors: true, SilenceUsage: true}
	r.AddCommand(ciCmd())
	return r
}

// Switching jobs on for an org's repository names --org, or zm runners
// setup makes the app on the person's own account and refuses the repo.
func TestCIAddSwitchOnNamesTheOrg(t *testing.T) {
	b := newFakeBroker(t)
	b.pool = ""
	cfg, err := config.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	b.lent = []string{strings.ToLower(cfg.Machine) + "-c4m16"}
	ciOwnerIsOrg = func(_ context.Context, owner string) (bool, error) { return owner == "acme", nil }
	if res := ciAddJSON(t, "acme/app"); res.Next == nil || res.Next.Command != "zm runners setup "+lendPool+" --repo acme/app --org acme" || res.Next.Kind != "ask" {
		t.Fatalf("org repo: %+v", res.Next)
	}
	if res := ciAddJSON(t, "judy/app"); res.Next == nil || res.Next.Command != "zm runners setup "+lendPool+" --repo judy/app" {
		t.Fatalf("own account: %+v", res.Next)
	}
	if res := ciAddJSON(t, "acme/app", "--org="); res.Next == nil || strings.Contains(res.Next.Command, "--org") {
		t.Fatalf("--org= is the own account: %+v", res.Next)
	}
	ciOwnerIsOrg = func(context.Context, string) (bool, error) { return false, errors.New("gh: not signed in") }
	if res := ciAddJSON(t, "acme/app"); res.Next == nil || strings.Contains(res.Next.Command, "--org") || !strings.Contains(res.Next.Say, "add --org acme") {
		t.Fatalf("org unknown: %+v", res.Next)
	}
}

// A failed check after 5 (a missing fork guard, check 10) no longer holds
// back the job's pull request, which may be what fixes it; with nothing to
// open, the check is the step.
func TestCIAddLaterCheckDoesNotGateTheJob(t *testing.T) {
	b := newFakeBroker(t)
	b.app, b.linked["acme"] = true, true
	b.repos = []runners.Repo{{Repo: "acme/app", Visibility: "private"}}
	b.doctor["acme/app"] = []runners.DoctorCheck{{Number: 1, Status: "pass"}, {Number: 2, Status: "pass"}, {Number: 3, Status: "pass"}, {Number: 4, Status: "pass"}, {Number: 5, Status: "pass"},
		{Number: 10, Status: "fail", Message: "ui.yml job ui: no fork guard", Fix: "add `if: x` to the job in .github/workflows/ui.yml"}}
	answer := `{"state":"script","repo":"acme/app","job":"ui","workflow":".github/workflows/ui.yml","labels":["self-hosted"],"script":"/tmp/x/open-pr.sh","next":"sh '/tmp/x/open-pr.sh'"}`
	ran := 0
	ciPotluck = func(_ context.Context, _ []string, _ io.Reader, stdout, _ io.Writer) int {
		ran++
		fmt.Fprint(stdout, answer)
		return 0
	}
	if res := ciAddJSON(t, "acme/app", "--job", "ui"); ran != 1 || res.Next == nil || res.Next.Command != "sh '/tmp/x/open-pr.sh'" {
		t.Fatalf("check 10 held back the pull request (ran %d): %+v", ran, res.Next)
	}
	answer = `{"state":"done","repo":"acme/app","job":"ui"}`
	if res := ciAddJSON(t, "acme/app", "--job", "ui"); res.State == "done" || res.Next == nil || !strings.Contains(res.Next.Say, "no fork guard") {
		t.Fatalf("the failed check is not the step: %+v", res)
	}
	// An owner's step after 5 (check 12: no machine takes a job) still
	// holds the job back, and so do checks 1 to 5.
	answer = `{"state":"script","repo":"acme/app","job":"ui","workflow":".github/workflows/ui.yml","labels":["self-hosted"],"script":"/tmp/x/open-pr.sh","next":"sh '/tmp/x/open-pr.sh'"}`
	b.doctor["acme/app"][5] = runners.DoctorCheck{Number: 12, Status: "fail", Message: "No machine in the pool can take a job.", Fix: "lend a machine to the pool (from that machine)"}
	ran = 0
	if res := ciAddJSON(t, "acme/app", "--job", "ui"); ran != 0 || res.Next == nil || res.Next.Kind != "ask" || !strings.Contains(res.Next.Say, "No machine") {
		t.Fatalf("check 12 (ran %d): %+v", ran, res.Next)
	}
	// A workflow check failing first does not hide a later one that stops
	// jobs (13: a box got no runner, 15: the broker's GitHub checks fail).
	for _, later := range []runners.DoctorCheck{
		{Number: 13, Status: "fail", Message: "A box got no working GitHub runner.", Fix: "read that host's daemon log"},
		{Number: 15, Status: "fail", Message: "The broker's checks of GitHub are failing.", Fix: "check the daemon log for runners"},
		{Number: 12, Status: "fail", Message: "No machine in the pool can take a job.", Fix: "lend a machine to the pool (from that machine)"},
	} {
		b.doctor["acme/app"] = append(b.doctor["acme/app"][:5], runners.DoctorCheck{Number: 10, Status: "fail", Message: "no fork guard", Fix: "add `if: x`"}, later)
		if res := ciAddJSON(t, "acme/app", "--job", "ui"); ran != 0 || res.Next == nil || !strings.Contains(res.Next.Say, "no fork guard") {
			t.Fatalf("check 10 then %d (ran %d): %+v", later.Number, ran, res.Next)
		}
	}
	b.doctor["acme/app"] = b.doctor["acme/app"][:6]
	// Check 4 missing, or unavailable, beside a check 10 fail: still held back.
	for _, four := range []runners.DoctorCheck{{Number: 4, Status: "warn", Message: "unavailable: GitHub did not answer"}, {Number: 6, Status: "pass"}} {
		b.doctor["acme/app"][3] = four
		if ciAddJSON(t, "acme/app", "--job", "ui"); ran != 0 {
			t.Fatalf("ci pr ran without check 4's pass (%+v)", four)
		}
	}
	b.doctor["acme/app"][3] = runners.DoctorCheck{Number: 4, Status: "pass"}
	b.doctor["acme/app"][2].Status = "fail"
	if ciAddJSON(t, "acme/app", "--job", "ui"); ran != 0 {
		t.Fatal("ci pr ran past a failed check 3")
	}
}

// One repository's unexpected error is its own blocked step: the others
// still connect.
func TestCIAddOneRepoErrorIsItsStep(t *testing.T) {
	b := newFakeBroker(t)
	b.app, b.linked["judy"] = true, true
	b.badDoc = "judy/bad"
	res := ciAddJSON(t, "judy/bad", "judy/app")
	if len(res.Repos) != 2 || res.Repos[0].State != "blocked" || res.Repos[0].Next == nil || !strings.Contains(res.Repos[0].Next.Say, "the doctor broke on judy/bad") ||
		res.Repos[0].Next.Kind != "run" || res.Repos[1].State != "done" {
		t.Fatalf("%+v", res)
	}
	// An owner's command in that error is an ask.
	b.badMsg = "the runner broker is not set up: zm runners setup home"
	if res := ciAddJSON(t, "judy/bad", "judy/app"); res.Repos[0].Next == nil || res.Repos[0].Next.Kind != "ask" {
		t.Fatalf("owner's step in an error: %+v", res.Repos[0].Next)
	}
	b.badMsg = ""
	// Alone, it is the run's error, as before.
	if _, err := runCLI(t, ciAddRoot(), "ci", "add", "judy/bad"); err == nil || !strings.Contains(err.Error(), "the doctor broke") {
		t.Fatalf("one repository: %v", err)
	}
}

// --json's refusals are JSON (an api error), never plain text.
func TestCIAddJSONRefusalIsJSON(t *testing.T) {
	newFakeBroker(t)
	old := flagJSON
	flagJSON = true
	t.Cleanup(func() { flagJSON = old })
	_, err := runCLI(t, ciAddRoot(), "ci", "add", "nope")
	var e *api.Error
	if !errors.As(err, &e) || !strings.Contains(e.Message, "is not owner/repo") {
		t.Fatalf("%T %v", err, err)
	}
}

// Small gaps: --org once the app exists says it is not used; a bad name
// from --mine names its project; GitHub without the owner says so.
func TestCIAddSmallGaps(t *testing.T) {
	b := newFakeBroker(t)
	b.app, b.linked["judy"] = true, true
	if res := ciAddJSON(t, "judy/app", "--org", "judy"); !strings.Contains(strings.Join(res.Repos[0].Notes, " "), "--org is not used") {
		t.Errorf("--org silently ignored: %+v", res.Repos[0])
	}
	b.mapped = []map[string]string{{"project": "P", "repo": "judy/app"}, {"project": "broken", "repo": "not a repo"}}
	if _, err := runCLI(t, ciAddRoot(), "ci", "add", "--mine"); err == nil || !strings.Contains(err.Error(), "zm project broken") {
		t.Errorf("--mine bad name: %v", err)
	}
	b.app = false
	ciOwnerIsOrg = func(context.Context, string) (bool, error) { return false, errNoGitHubAccount }
	if _, err := runCLI(t, ciAddRoot(), "ci", "add", "nobody/app"); err == nil || !strings.Contains(err.Error(), "GitHub has no account nobody") || strings.Contains(err.Error(), "gh auth login") {
		t.Errorf("missing owner: %v", err)
	}
}

// Stopped while waiting for the press: the re-run command keeps --job and
// --workflow.
func TestCIAddStoppedWaitKeepsJob(t *testing.T) {
	b := newFakeBroker(t)
	old := ciAddPoll
	ciAddPoll = 0
	t.Cleanup(func() { ciAddPoll = old })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b.onPoll = cancel
	c, _, err := newClientContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = ciAddWith(ctx, c, []string{"judy/app"}, ciAddOpts{job: "ui", workflow: "ui.yml", wait: time.Hour, say: func(string, ...any) {}})
	if err == nil || !strings.Contains(err.Error(), "zm ci add judy/app --job ui --workflow ui.yml again") {
		t.Fatalf("%v", err)
	}
}

// compute_setup through the MCP SDK (input decode, the server's own
// client): owner steps come back as asks, and an agent's public: true
// never connects a public repository.
func TestComputeSetupThroughMCP(t *testing.T) {
	b := newFakeBroker(t)
	c, _, err := newClientContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, ct := sdk.NewInMemoryTransports()
	ss, err := mcp.NewServer(c, mcp.Options{Compute: true, Setup: computeSetup, Pause: computePause, Probe: computeProbe}).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	call := func(args map[string]any) (ciAddResult, bool, string) {
		t.Helper()
		r, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "compute_setup", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var text string
		for _, ct := range r.Content {
			if tc, ok := ct.(*sdk.TextContent); ok {
				text += tc.Text
			}
		}
		var res ciAddResult
		if !r.IsError {
			if err := json.Unmarshal([]byte(text[strings.Index(text, "{"):]), &res); err != nil {
				t.Fatalf("%v: %s", err, text)
			}
		}
		return res, r.IsError, text
	}

	b.pool = ""
	if res, isErr, text := call(map[string]any{"repos": []string{"judy/app"}}); isErr || res.Next == nil || res.Next.Kind != "ask" || res.Next.Command != "zm lend" {
		t.Fatalf("not lending: %s", text)
	}
	b.pool, b.app, b.linked["judy"] = "home", true, true
	b.repos = []runners.Repo{{Repo: "judy/app", Visibility: "private"}}
	for _, fix := range []string{"zm runners link", "zm runners setup home --url https://<host>", "restart the zm daemon (it reads runners.github at start)"} {
		b.doctor["judy/app"] = []runners.DoctorCheck{{Number: 1, Status: "fail", Message: "Not right.", Fix: fix}}
		if res, isErr, text := call(map[string]any{"repos": []string{"judy/app"}}); isErr || res.Next == nil || res.Next.Kind != "ask" {
			t.Errorf("fix %q: %s", fix, text)
		}
	}
	b.doctor["judy/app"] = nil
	b.repos, b.public["judy/pub"] = nil, true
	res, isErr, text := call(map[string]any{"repos": []string{"judy/pub"}, "public": true})
	if !isErr && (res.Next == nil || res.Next.Kind != "ask" || len(res.Next.Choices) != 2) {
		t.Fatalf("public through MCP: %s", text)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, req := range b.connects {
		if req.Public {
			t.Fatalf("compute_setup said the owner's public yes: %+v", b.connects)
		}
	}
	if len(b.repos) != 0 {
		t.Fatalf("a public repository was connected without its owner: %+v", b.repos)
	}
}

// compute_pause's real wrapper reads this machine's name and $ZM_HOME; a
// lend going on is refused without its process id; an unbind that fails
// part-way says which sizes are off, keeps them chosen, and pausing again
// finishes it.
func TestComputePauseWrapper(t *testing.T) {
	b := newFakeBroker(t)
	cfg, err := config.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	m := strings.ToLower(cfg.Machine)
	b.lent = []string{m + "-c2m4", m + "-c4m8"}
	c, _, err := newClientContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	unlock, err := lendLock(cfg.Home)
	if err != nil {
		t.Fatal(err)
	}
	v, err := computePause(ctx, c)
	unlock()
	res, _ := v.(LendPause)
	if err != nil || res.State != "refused" || res.Refused.Reason != "lend_running" || strings.Contains(res.Refused.Message, "process") || strings.Contains(res.Refused.Message, "kill") {
		t.Fatalf("lend going on: %+v %v", res.Refused, err)
	}

	old := lendUnbind
	t.Cleanup(func() { lendUnbind = old })
	n := 0
	lendUnbind = func(ctx context.Context, c *client.Client, name string) error {
		if n++; n == 2 {
			return api.Errorf(api.CodeUnavailable, "the daemon went away")
		}
		return old(ctx, c, name)
	}
	_, err = computePause(ctx, c)
	if e := (*api.Error)(nil); !errors.As(err, &e) || e.Code != api.CodeUnavailable || !strings.Contains(e.Message, "sizes c2m4 are no longer lent") {
		t.Fatalf("part-way: %v", err)
	}
	lendUnbind = old
	v, err = computePause(ctx, c)
	if res, _ := v.(LendPause); err != nil || res.State != "paused" || strings.Join(res.Sizes, ",") != "c4m8" {
		t.Fatalf("pause again: %+v %v", v, err)
	}
	if st := readLendSettings(cfg.Home); st == nil || !st.Paused || strings.Join(st.Sizes, ",") != "c2m4,c4m8" {
		t.Fatalf("settings after a part-way pause: %+v", st)
	}
	if len(b.lent) != 0 {
		t.Fatalf("still lent: %v", b.lent)
	}
}

// zm help ci pr is Potluck's own help (its flags), not zm's globals.
func TestHelpCIPRIsPotlucks(t *testing.T) {
	r := &cobra.Command{Use: "zm", SilenceErrors: true, SilenceUsage: true}
	r.AddCommand(ciCmd())
	var out bytes.Buffer
	r.SetOut(&out)
	r.SetErr(&out)
	r.SetArgs([]string{"help", "ci", "pr"})
	if err := r.Execute(); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "--job") || !strings.Contains(s, "--workflow") || strings.Contains(s, "potluck ") {
		t.Fatalf("help:\n%s", s)
	}

}
