package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	potluckcli "github.com/sengirunners/potluck/cli"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
	"github.com/spf13/cobra"
)

// fakeBroker is the daemon's runner routes as zm ci add uses them, with
// GitHub's side (the person's presses) set by the test.
type fakeBroker struct {
	mu       sync.Mutex
	t        *testing.T
	pool     string
	app      bool
	linked   map[string]bool // owner -> confirmed install
	public   map[string]bool // repo -> public on GitHub
	repos    []runners.Repo
	doctor   map[string][]runners.DoctorCheck
	setups   []runners.SetupRequest
	connects []runners.AllowRequest
	onPoll   func()   // called on each links read
	lent     []string // this machine's lent sizes in the personal pool (zm lend)
	mapped   []map[string]string
	badDoc   string // a repository whose doctor read fails
	badMsg   string // its error (default: the doctor broke)
}

func (b *fakeBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	w.Header().Set(api.HeaderProtocol, api.Protocol)
	if name, ok := strings.CutPrefix(r.URL.Path, "/v0/run_targets/"); ok { // zm lend pause
		switch {
		case r.Method == "GET" && strings.HasSuffix(name, "/handles"):
			api.WriteJSON(w, 200, computeapi.RunTargetHandles{Handles: []computeapi.RunTargetHandle{}})
		case r.Method == "DELETE":
			b.lent = slices.DeleteFunc(b.lent, func(n string) bool { return n == name })
			w.WriteHeader(204)
		default:
			b.t.Errorf("unexpected request %s %s", r.Method, r.URL)
			w.WriteHeader(500)
		}
		return
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /v0/runners/setup":
		st := runners.Setup{Pool: b.pool, Label: "home"}
		if b.app {
			st.App = &runners.SetupApp{ID: 1, Slug: "potluck-judy"}
		}
		api.WriteJSON(w, 200, st)
	case "POST /v0/runners/setup":
		var req runners.SetupRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		b.setups = append(b.setups, req)
		if !b.app {
			api.WriteJSON(w, 200, runners.Setup{Pool: req.Pool, Next: "create_app", URL: "https://pool.example/v0/runners/app/new?state=s1"})
			return
		}
		api.WriteJSON(w, 200, runners.Setup{Pool: req.Pool, Next: "install", URL: "https://github.example/apps/potluck-judy/installations/new?state=s2"})
	case "GET /v0/runners/links":
		if b.onPoll != nil {
			b.onPoll()
		}
		var out struct {
			Links []runners.Link `json:"links"`
		}
		out.Links = []runners.Link{}
		for o, ok := range b.linked {
			if ok {
				out.Links = append(out.Links, runners.Link{Installation: 7, Account: o})
			}
		}
		api.WriteJSON(w, 200, out)
	case "GET /v0/runners/repos":
		api.WriteJSON(w, 200, map[string]any{"repos": append([]runners.Repo{}, b.repos...)})
	case "POST /v0/runners/repos":
		var req runners.AllowRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		b.connects = append(b.connects, req)
		owner, _, _ := strings.Cut(req.Repo, "/")
		switch {
		case !b.linked[owner]:
			api.WriteError(w, api.Errorf(api.CodeNotFound, "the App potluck-judy is not installed on %s, or not given that repository", req.Repo))
		case b.public[req.Repo] && !req.Public:
			api.WriteError(w, api.Errorf(api.CodeBadRequest, "%s is public: anyone can propose code to it", req.Repo).WithHint("zm runners allow "+req.Repo+" --public serves its own branches"))
		default:
			vis := "private"
			if b.public[req.Repo] {
				vis = "public"
			}
			rp := runners.Repo{Repo: req.Repo, Installation: 7, Public: req.Public, Visibility: vis, By: "judy"}
			b.repos = append(b.repos, rp)
			api.WriteJSON(w, 200, rp)
		}
	case "GET /v0/run_targets":
		l := computeapi.RunTargetList{Project: lendPool, Targets: []computeapi.RunTargetView{}}
		for _, n := range b.lent {
			l.Targets = append(l.Targets, computeapi.RunTargetView{RunTarget: computeapi.RunTarget{Name: n, Kind: "box", Images: []string{"x@sha256:" + strings.Repeat("a", 64)}}})
		}
		api.WriteJSON(w, 200, l)
	case "GET /v0/mail/notice": // zm mcp's mail line
		api.WriteJSON(w, 200, map[string]any{})
	case "GET /v0/github/repos":
		m := b.mapped
		if m == nil {
			m = []map[string]string{{"project": "P", "repo": "judy/app"}, {"project": "Q", "repo": "judy/site"}}
		}
		api.WriteJSON(w, 200, map[string]any{"repositories": m})
	case "GET /v0/runners/doctor":
		repo := r.URL.Query().Get("repo")
		if repo == b.badDoc {
			msg := "the doctor broke on " + repo
			if b.badMsg != "" {
				msg = b.badMsg
			}
			api.WriteError(w, api.Errorf(api.CodeInternal, "%s", msg))
			return
		}
		checks := b.doctor[repo]
		if checks == nil {
			checks = []runners.DoctorCheck{{Number: 1, Status: "pass", Message: "GitHub accepts the connection key."},
				{Number: 2, Status: "pass"}, {Number: 3, Status: "pass"}, {Number: 4, Status: "pass"}, {Number: 5, Status: "pass"},
				{Number: 14, Status: "warn", Message: "unavailable: image Xcode inventory is not reported, see zm-abc1"}}
		}
		api.WriteJSON(w, 200, runners.DoctorResult{Reports: []runners.DoctorReport{{Repo: repo, Checks: checks}}})
	default:
		b.t.Errorf("unexpected request %s %s", r.Method, r.URL)
		w.WriteHeader(500)
	}
}

func newFakeBroker(t *testing.T) *fakeBroker {
	b := &fakeBroker{t: t, pool: "home", linked: map[string]bool{}, public: map[string]bool{}, doctor: map[string][]runners.DoctorCheck{}}
	runtimeClientEnv(t, b)
	old := ciOwnerIsOrg
	ciOwnerIsOrg = func(context.Context, string) (bool, error) { return false, nil }
	t.Cleanup(func() { ciOwnerIsOrg = old })
	// Potluck's ci pr reads workflows with gh: never the real one (a test
	// that wants answers puts its own fake first).
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho 'gh: not signed in' >&2\nexit 4\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	// By default the repository has no job to move (a test that wants
	// Potluck's own ci pr sets ciPotluck back).
	oldPR := ciPotluck
	ciPotluck = func(_ context.Context, args []string, _ io.Reader, _, stderr io.Writer) int {
		fmt.Fprintf(stderr, "potluck: no job in %s at main (.github/workflows/ci.yml) runs on a GitHub-hosted macOS label; name the job with --job <id>", args[2])
		return 1
	}
	t.Cleanup(func() { ciPotluck = oldPR })
	t.Setenv(potluckcli.EnvNoAutostart, os.Getenv(potluckcli.EnvNoAutostart)) // ci add sets it: restored after
	return b
}

// ciAddRun runs zm ci add args; it answers what it printed, its last line,
// and whether it ended done (exit 0) or with a next step (exit 1).
func ciAddRun(t *testing.T, jsonMode bool, args ...string) (string, string, bool) {
	t.Helper()
	oldJSON := flagJSON
	flagJSON = jsonMode
	t.Cleanup(func() { flagJSON = oldJSON })
	r := &cobra.Command{Use: "zm", SilenceErrors: true, SilenceUsage: true}
	r.AddCommand(ciCmd())
	out, err := runCLI(t, r, append([]string{"ci", "add"}, args...)...)
	switch err.(type) {
	case nil:
	case exitSilently:
		if int(err.(exitSilently)) != 1 {
			t.Fatalf("exit %d", err)
		}
	default:
		t.Fatalf("zm ci add %v: %v\n%s", args, err, out)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	return out, lines[len(lines)-1], err == nil
}

func (b *fakeBroker) counts() (setups, connects int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.setups), len(b.connects)
}

// Not lending yet: the one next step is zm lend, and nothing is changed.
func TestCIAddLendsFirst(t *testing.T) {
	b := newFakeBroker(t)
	b.pool = ""
	out, last, done := ciAddRun(t, false, "judy/app")
	want := "Run: zm lend"
	if done || !strings.HasSuffix(last, want) || !strings.HasPrefix(last, "next: This Mac") {
		t.Fatalf("last line %q:\n%s", last, out)
	}
	if s, c := b.counts(); s != 0 || c != 0 {
		t.Fatalf("changed something before lending: %d setups, %d connects", s, c)
	}
}

// Lent with zm lend, but GitHub's jobs not switched on: the next step is the
// switch, never zm lend again (no loop).
func TestCIAddAfterLendSwitchesJobsOn(t *testing.T) {
	b := newFakeBroker(t)
	b.pool = ""
	cfg, err := config.LoadClient()
	if err != nil {
		t.Fatal(err)
	}
	b.lent = []string{strings.ToLower(cfg.Machine) + "-c4m16"}
	out, last, done := ciAddRun(t, false, "judy/app")
	if done || !strings.HasSuffix(last, "Run: zm runners setup "+lendPool+" --repo judy/app") {
		t.Fatalf("last line %q:\n%s", last, out)
	}
}

// The whole path for an agent: the GitHub page comes back as an ask (no
// waiting in --json), the run after the press connects and checks the
// repository and is done, and a third run changes nothing and is done.
func TestCIAddJSONAsksThenConnectsThenIsDone(t *testing.T) {
	b := newFakeBroker(t)
	raw, _, _ := ciAddRun(t, true, "judy/app")
	var res ciAddResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatalf("first run: %v\n%s", err, raw)
	}
	if res.State != "needs_human" || res.Next == nil || res.Next.Kind != "ask" || res.Next.URL != "https://pool.example/v0/runners/app/new?state=s1" ||
		!strings.Contains(res.Next.Say, "Create, then Install on judy with only judy/app selected (already ticked on the page), then Yes") {
		t.Fatalf("first run's step: %+v", res)
	}
	if len(b.setups) != 1 || len(b.setups[0].Repos) != 1 || b.setups[0].Repos[0] != "judy/app" {
		t.Fatalf("the install page does not pre-select the repository: %+v", b.setups)
	}
	if len(res.Repos) != 1 || res.Repos[0].Next == nil {
		t.Fatalf("the repository carries no step: %+v", res.Repos)
	}
	if s, c := b.counts(); s != 1 || c != 0 {
		t.Fatalf("first run: %d setups, %d connects", s, c)
	}

	// The person pressed Create, Install and Yes.
	b.mu.Lock()
	b.app, b.linked["judy"] = true, true
	b.mu.Unlock()
	raw, _, _ = ciAddRun(t, true, "judy/app")
	res = ciAddResult{}
	if err := json.Unmarshal([]byte(raw), &res); err != nil || res.State != "done" || res.Next != nil {
		t.Fatalf("second run: %v\n%s", err, raw)
	}
	if got := res.Repos[0]; got.State != "done" || len(got.Changed) != 1 || got.Changed[0] != "connected judy/app" {
		t.Fatalf("second run's repository: %+v", got)
	}
	for _, n := range res.Repos[0].Notes {
		if strings.HasPrefix(n, "unavailable:") {
			t.Errorf("missing evidence reported as a note: %q", n)
		}
	}

	out, last, done := ciAddRun(t, false, "judy/app")
	if !done || last != "done" || !strings.Contains(out, "No job moved onto this Mac: no job in judy/app") || !strings.Contains(out, "To move one: zm ci add judy/app --job <id>") {
		t.Fatalf("third run: %q\n%s", last, out)
	}
	if strings.Contains(out, "Will:") {
		t.Errorf("a run with nothing to do says it will change something:\n%s", out)
	}
	if s, c := b.counts(); s != 1 || c != 1 {
		t.Fatalf("re-runs changed something: %d setups, %d connects", s, c)
	}
}

// For people the command waits for GitHub's confirmation, saying so, and
// goes on once the press is made; an org's repository gets the app on the org.
func TestCIAddWaitsForThePress(t *testing.T) {
	b := newFakeBroker(t)
	ciOwnerIsOrg = func(_ context.Context, owner string) (bool, error) { return owner == "acme", nil }
	old := ciAddPoll
	ciAddPoll = 10 * time.Millisecond
	t.Cleanup(func() { ciAddPoll = old })
	polls := 0
	b.onPoll = func() {
		if polls++; polls == 3 {
			b.app, b.linked["acme"] = true, true
		}
	}
	out, last, done := ciAddRun(t, false, "acme/tool", "--wait", "5s")
	if !done || last != "done" {
		t.Fatalf("last %q:\n%s", last, out)
	}
	for _, want := range []string{"Will: create a GitHub app for this Mac on the GitHub org acme", "https://pool.example/v0/runners/app/new?state=s1", "GitHub confirmed acme.", "Will: connect acme/tool", "Connected: acme/tool."} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if len(b.setups) != 1 || b.setups[0].Org != "acme" || b.setups[0].Pool != "home" {
		t.Fatalf("setup asked for %+v", b.setups)
	}

	// The app is acme's and private: beta's repository is named as blocked,
	// and acme's is still served.
	b.onPoll = nil
	raw, _, _ := ciAddRun(t, true, "acme/tool", "beta/other")
	var res ciAddResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatal(err)
	}
	if res.State != "blocked" || res.Repos[0].State != "done" || res.Repos[1].State != "blocked" || !strings.Contains(res.Repos[1].Next.Say, "belongs to acme") || res.Repos[1].Next.Command != "" {
		t.Fatalf("a second owner: %s", raw)
	}

	// A press never made: the step is the page again, after the wait; the
	// other owner's repository waits for a run after it.
	b.linked = map[string]bool{}
	out, last, done = ciAddRun(t, false, "acme/tool", "beta/other", "--wait", "50ms")
	if done || !strings.HasPrefix(last, "next: Open this once") || !strings.Contains(last, "install the app on acme with only acme/tool selected (already ticked on the page), then Yes. Then run this again. Open: https://github.example/apps/potluck-judy/installations/new?state=s2") {
		t.Fatalf("unpressed install: %q\n%s", last, out)
	}
	raw, _, _ = ciAddRun(t, true, "acme/tool", "beta/other")
	res = ciAddResult{}
	if err := json.Unmarshal([]byte(raw), &res); err != nil || res.Repos[1].Next == nil || res.Repos[1].Next.Kind != "info" || !strings.Contains(res.Repos[1].Next.Say, "zm ci add beta/other again") {
		t.Fatalf("the second owner's step: %v %s", err, raw)
	}
}

// A public repository needs the owner's own yes, asked for and never
// assumed; with --public it is connected.
func TestCIAddPublicAsksForTheYes(t *testing.T) {
	b := newFakeBroker(t)
	b.app, b.linked["acme"], b.public["acme/pub"] = true, true, true
	raw, _, _ := ciAddRun(t, true, "acme/pub")
	var res ciAddResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil || res.State != "needs_human" {
		t.Fatalf("%v\n%s", err, raw)
	}
	if res.Next == nil || res.Next.Kind != "ask" || res.Next.Command != "" || len(res.Next.Choices) != 2 || !strings.Contains(res.Next.Say, "Only the owner says yes, by running zm ci add acme/pub --public") {
		t.Fatalf("public step: %+v", res.Next)
	}
	if len(b.repos) != 0 {
		t.Fatalf("a public repository was connected without the yes: %+v", b.repos)
	}
	_, last, done := ciAddRun(t, false, "acme/pub", "--public")
	if !done || last != "done" || len(b.repos) != 1 || !b.repos[0].Public {
		t.Fatalf("with --public: %q %+v", last, b.repos)
	}
	// Run again without the flag: already connected with the yes, done.
	if _, last, done := ciAddRun(t, false, "acme/pub"); !done || last != "done" || len(b.connects) != 2 {
		t.Fatalf("re-run: %q, %d connects", last, len(b.connects))
	}
}

// A failed doctor check is the next step: a page to open (widening the
// install), or a zm command to run.
func TestCIAddDoctorFailIsTheStep(t *testing.T) {
	b := newFakeBroker(t)
	b.app, b.linked["acme"] = true, true
	b.repos = []runners.Repo{{Repo: "acme/tool", Visibility: "private"}}
	b.doctor["acme/tool"] = []runners.DoctorCheck{
		{Number: 2, Status: "pass", Message: "The app is installed on acme."},
		{Number: 3, Status: "fail", Message: "acme/tool is not included: GitHub never tells this machine about its jobs.", Fix: "ask the owner to widen the selected repositories at https://github.example/settings/installations/7 (organization owner: https://github.example/organizations/acme/settings/installations/7)"},
	}
	raw, _, _ := ciAddRun(t, true, "acme/tool")
	var res ciAddResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatal(err)
	}
	if res.Next == nil || res.Next.Kind != "ask" || res.Next.URL != "https://github.example/settings/installations/7" || !strings.Contains(res.Next.Say, "acme/tool is not included") {
		t.Fatalf("widen step: %+v", res.Next)
	}
	b.doctor["acme/tool"] = []runners.DoctorCheck{{Number: 17, Status: "fail", Message: "settings in memory differ from settings on disk", Fix: "restart the zm daemon"}}
	_, last, done := ciAddRun(t, false, "acme/tool")
	if done || last != "next: settings in memory differ from settings on disk Fix: restart the zm daemon." {
		t.Fatalf("command step: %q", last)
	}
}

// --job runs Potluck's own ci pr (gh is faked here: no real GitHub): the
// step is the script that opens the pull request with the person's gh
// sign-in; once the job already asks for this Mac, the run is done.
func TestCIAddJobRunsPotluckCIPR(t *testing.T) {
	b := newFakeBroker(t)
	b.app, b.linked["acme"] = true, true
	b.repos = []runners.Repo{{Repo: "acme/app", Visibility: "private"}}
	ciPotluck = potluckcli.Run // Potluck's own ci pr, on a fake gh
	dir, bin := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
if [ "$1" = api ] && [ "$2" = --method ] && [ "$3" = GET ]; then
  f="$FAKE_GH_DIR/$(printf %s "$4" | tr '/?=&:%' '______')"
  if [ -f "$f" ]; then cat "$f"; exit 0; fi
  echo "gh: Not Found (HTTP 404)" >&2; exit 1
fi
echo "fake gh: unexpected $*" >&2; exit 2
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_GH_DIR", dir)
	t.Setenv("TMPDIR", t.TempDir())
	answer := func(path string, v any) {
		raw, _ := json.Marshal(v)
		name := strings.NewReplacer("/", "_", "?", "_", "=", "_", "&", "_", ":", "_", "%", "_").Replace(path)
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	workflow := func(text string) {
		p := ".github/workflows/ui.yml"
		answer("repos/acme/app/contents/.github/workflows?ref=main", []any{map[string]any{"name": "ui.yml", "path": p, "type": "file"}})
		answer("repos/acme/app/contents/"+p+"?ref=main", map[string]any{"path": p, "sha": "b10b", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(text))})
	}
	answer("repos/acme/app", map[string]any{"default_branch": "main"})
	answer("repos/acme/app/git/ref/heads/main", map[string]any{"object": map[string]any{"sha": "1111111111111111111111111111111111111111"}})
	answer("repos/acme/app/pulls?state=open&head=acme%3Apotluck%2Fruns-on-native", []any{})
	workflow("on:\n  push:\n  pull_request:\njobs:\n  native:\n    runs-on: macos-15\n    steps:\n      - run: xcodebuild\n")

	// No --job: Potluck picks the one job on a GitHub-hosted macOS label.
	out, last, done := ciAddRun(t, false, "acme/app")
	if done || !strings.HasPrefix(last, "next: Open the pull request with your own gh sign-in") || !strings.Contains(last, "Run: sh ") || !strings.Contains(last, "open-pr.sh") ||
		!strings.Contains(last, "only job native's runs-on") {
		t.Fatalf("last %q:\n%s", last, out)
	}
	if !strings.Contains(out, "-    runs-on: macos-15") || strings.Contains(last, "potluck ci pr") || strings.Contains(last, "potluck runners") {
		t.Errorf("the change is not shown first, or names potluck's command:\n%s", out)
	}

	// The pull request merged: the job already asks for this Mac.
	var changed string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "+    runs-on: ") {
			changed = strings.TrimPrefix(l, "+    runs-on: ")
		}
	}
	if changed == "" {
		t.Fatalf("no new runs-on in:\n%s", out)
	}
	workflow(fmt.Sprintf("on:\n  push:\n  pull_request:\njobs:\n  native:\n    runs-on: %s\n    steps:\n      - run: xcodebuild\n", changed))
	if out, last, done := ciAddRun(t, false, "acme/app", "--job", "native"); !done || last != "done" {
		t.Fatalf("after the merge: %q\n%s", last, out)
	}
}

// ci add reads ci pr's JSON fields, never its text: each state is its step,
// the command is Potluck's next as given, the script is never in Said, and
// an answer it does not know is an error, not a guess.
func TestCIAddReadsCIPRJSON(t *testing.T) {
	b := newFakeBroker(t)
	b.app, b.linked["acme"] = true, true
	b.repos = []runners.Repo{{Repo: "acme/app", Visibility: "private"}}
	var gotArgs []string
	answer, code := "", 0
	old := ciPotluck
	t.Setenv(potluckcli.EnvNoAutostart, "")
	ciPotluck = func(_ context.Context, args []string, _ io.Reader, stdout, stderr io.Writer) int {
		gotArgs = args
		if os.Getenv(potluckcli.EnvNoAutostart) != "1" { // Potluck must never start a daemon past zm's lock
			t.Errorf("ci pr ran with %s=%q", potluckcli.EnvNoAutostart, os.Getenv(potluckcli.EnvNoAutostart))
		}
		if code != 0 {
			fmt.Fprint(stderr, answer)
		} else {
			fmt.Fprint(stdout, answer)
		}
		return code
	}
	t.Cleanup(func() { ciPotluck = old })
	run := func(args ...string) ciAddResult {
		t.Helper()
		raw, _, _ := ciAddRun(t, true, append([]string{"acme/app"}, args...)...)
		var res ciAddResult
		if err := json.Unmarshal([]byte(raw), &res); err != nil {
			t.Fatalf("%v\n%s", err, raw)
		}
		return res
	}

	answer = `{"state":"script","repo":"acme/app","job":"ui","workflow":".github/workflows/ui.yml","labels":["self-hosted","macos","home"],` +
		`"script":"/tmp/x/open-pr.sh","next":"sh '/tmp/x/open-pr.sh'","diff":"-    runs-on: macos-15\n","notes":["note: skipped \"a b.yml\""]}`
	res := run("--workflow", "ui.yml")
	if strings.Join(gotArgs, " ") != "ci pr acme/app --json --workflow ui.yml" {
		t.Errorf("args %q", gotArgs)
	}
	n := res.Next
	if res.State != "needs_human" || n == nil || n.Kind != "ask" || n.Command != "sh '/tmp/x/open-pr.sh'" || !strings.Contains(n.Say, "job ui's runs-on") {
		t.Fatalf("script state: %+v %+v", res, n)
	}
	said := strings.Join(res.Said, "\n")
	if !strings.Contains(said, "-    runs-on: macos-15") || strings.Contains(said, "#!/bin/sh") || !containsFold(res.Repos[0].Notes, `note: skipped "a b.yml"`) {
		t.Errorf("said %q, notes %q", said, res.Repos[0].Notes)
	}

	answer = `{"state":"merge","repo":"acme/app","job":"ui","url":"https://github.example/acme/app/pull/3"}`
	if res := run("--job", "ui"); res.Next == nil || res.Next.Kind != "ask" || res.Next.URL != "https://github.example/acme/app/pull/3" ||
		strings.Join(gotArgs, " ") != "ci pr acme/app --json --job ui" {
		t.Fatalf("merge state: %+v (args %q)", res.Next, gotArgs)
	}
	answer = `{"state":"done","repo":"acme/app","job":"ui"}`
	if res := run(); res.State != "done" || !containsFold(res.Repos[0].Notes, "Job ui already runs on this Mac for acme/app's own branches.") {
		t.Fatalf("done state: %+v", res)
	}

	for _, bad := range []string{`{"state":"dry_run","next":"x"}`, `{"state":"script","next":"sh x"}`, "next: sh /tmp/x/open-pr.sh"} {
		answer = bad
		_, err := runCLI(t, func() *cobra.Command {
			r := &cobra.Command{Use: "zm", SilenceErrors: true, SilenceUsage: true}
			r.AddCommand(ciCmd())
			return r
		}(), "ci", "add", "acme/app", "--job", "ui")
		if err == nil || !strings.Contains(err.Error(), "did not understand") {
			t.Errorf("%q: %v", bad, err)
		}
	}

	// Without --job a job Potluck cannot pick is a note; with it, an error.
	answer, code = "potluck: 2 jobs in acme/app run on a GitHub-hosted macOS label (a, b); name one with --job <id>", 1
	if res := run(); res.State != "done" || !containsFold(res.Repos[0].Notes, "To move one: zm ci add acme/app --job <id>.") {
		t.Fatalf("no job picked: %+v", res)
	}
	if res := run("--workflow", "ui.yml"); !containsFold(res.Repos[0].Notes, "To move one: zm ci add acme/app --job <id> --workflow ui.yml.") {
		t.Fatalf("the note dropped --workflow: %+v", res.Repos[0].Notes)
	}
	r := &cobra.Command{Use: "zm", SilenceErrors: true, SilenceUsage: true}
	r.AddCommand(ciCmd())
	if _, err := runCLI(t, r, "ci", "add", "acme/app", "--job", "x"); err == nil || !strings.Contains(err.Error(), "2 jobs in acme/app") {
		t.Errorf("--job's refusal: %v", err)
	}
	// No workflows at all: nothing to pick, connected all the same.
	answer = "potluck: not found on GitHub: repos/acme/app/contents/.github/workflows?ref=main"
	if res := run(); res.State != "done" || !strings.Contains(strings.Join(res.Repos[0].Notes, " "), "Contents read") {
		t.Fatalf("no workflows (or a token that may not read files): %+v", res)
	}
	// Any other failure (gh not signed in, a broker error as JSON, a refusal
	// quoting the words) is the step, never done; an owner's command in it is an ask.
	// The person's own gh sign-in is theirs: an ask naming gh auth login.
	const again = "zm ci add acme/app --workflow ui.yml"
	for _, c := range []struct{ stderr, want, kind, command string }{
		{"potluck: gh could not read repos/acme/app (gh: not signed in); check gh auth status and your access to the repository", "gh could not read repos/acme/app", "ask", "gh auth login"},
		{"potluck: gh is not installed: install GitHub's gh and run gh auth login", "gh is not installed", "ask", "gh auth login"},
		{`{"error":{"code":"unavailable","message":"the runner broker is not set up","hint":"potluck runners setup <pool>"}}`, "the runner broker is not set up; zm runners setup <pool>", "ask", again},
		{"potluck: .github/workflows/ui.yml: job \"no job in acme/app at x runs on a GitHub-hosted macOS label\" calls a reusable workflow", "calls a reusable workflow", "run", again},
	} {
		answer = c.stderr
		res := run("--workflow", "ui.yml")
		if res.State != "needs_human" || res.Next == nil || res.Next.Kind != c.kind || !strings.Contains(res.Next.Say, c.want) || strings.Contains(res.Next.Say, `{"error"`) ||
			res.Next.Command != c.command {
			t.Errorf("%q: %+v %+v", c.stderr, res, res.Next)
		}
	}
}

// Lending, switching the broker on and the doctor's zm fixes are the
// owner's steps: asks (with the command the owner runs), never kind run,
// so an agent calling compute_setup is not told it may do them.
func TestCIAddOwnerStepsAreAsks(t *testing.T) {
	b := newFakeBroker(t)
	b.pool = ""
	raw, _, _ := ciAddRun(t, true, "judy/app")
	var res ciAddResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil || res.Next == nil || res.Next.Kind != "ask" || res.Next.Command != "zm lend" {
		t.Fatalf("not lending: %v %+v", err, res.Next)
	}
	b.pool, b.app, b.linked["judy"] = "home", true, true
	b.repos = []runners.Repo{{Repo: "judy/app", Visibility: "private"}}
	for _, c := range []struct{ fix, kind, command string }{
		{"restart the zm daemon (it reads runners.github at start)", "ask", ""}, // Potluck's check 17 words it so
		{"zm runners link", "ask", "zm runners link"},
		{"zm runners setup home --url https://<host>", "ask", "zm runners setup home --url https://<host>"},
		{"lend a machine to the pool (from that machine)", "ask", ""}, // Potluck's check 12
		{"zm ci doctor judy/app", "run", "zm ci doctor judy/app"},     // a read
		{"zm runners ls", "run", "zm runners ls"},
	} {
		b.doctor["judy/app"] = []runners.DoctorCheck{{Number: 1, Status: "fail", Message: "Not right.", Fix: c.fix}}
		raw, _, _ = ciAddRun(t, true, "judy/app")
		res = ciAddResult{}
		if err := json.Unmarshal([]byte(raw), &res); err != nil || res.Next == nil || res.Next.Kind != c.kind || res.Next.Command != c.command {
			t.Errorf("fix %q: %v %+v", c.fix, err, res.Next)
		}
	}
}

// The broker pre-selects at most 20 repositories: with more, none are sent
// and the person ticks them on the page.
func TestCIAddPreselectLimit(t *testing.T) {
	b := newFakeBroker(t)
	var repos []string
	for i := 0; i <= ciPreselectMax; i++ {
		repos = append(repos, fmt.Sprintf("judy/r%d", i))
	}
	ciAddRun(t, true, repos...)
	if len(b.setups) != 1 || len(b.setups[0].Repos) != 0 {
		t.Fatalf("%d repositories: %+v", len(repos), b.setups)
	}
	ciAddRun(t, true, repos[:ciPreselectMax]...)
	if len(b.setups) != 2 || len(b.setups[1].Repos) != ciPreselectMax {
		t.Fatalf("%d repositories: %+v", ciPreselectMax, b.setups[1:])
	}
}

// GitHub would not name a repository's id: the page is asked for again
// without the pre-selection, and the person ticks it there.
func TestCIAddPreselectFallsBack(t *testing.T) {
	b := newFakeBroker(t)
	refused := false
	inner := b.ServeHTTP
	runtimeClientEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" && r.URL.Path == "/v0/runners/setup" && !refused {
			refused = true
			var req runners.SetupRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			if len(req.Repos) == 0 {
				t.Errorf("first setup without repos: %+v", req)
			}
			w.Header().Set(api.HeaderProtocol, api.Protocol)
			api.WriteError(w, api.Errorf(api.CodeUnavailable, "repos: GitHub would not say which repository %q is", req.Repos[0]))
			return
		}
		inner(w, r)
	}))
	_, last, done := ciAddRun(t, false, "judy/app", "--wait", "0")
	if done || !strings.Contains(last, "with only judy/app selected, then Yes") || len(b.setups) != 1 || len(b.setups[0].Repos) != 0 {
		t.Fatalf("fallback: %q %+v", last, b.setups)
	}
}

// Every error names what to run.
func TestCIAddRefusesBadNames(t *testing.T) {
	newFakeBroker(t)
	r := &cobra.Command{Use: "zm", SilenceErrors: true, SilenceUsage: true}
	r.AddCommand(ciCmd())
	for _, c := range [][]string{{"ci", "add"}, {"ci", "add", "nope"}, {"ci", "add", "a/b", "c/d", "--job", "x"}} {
		_, err := runCLI(t, r, c...)
		if err == nil || !strings.Contains(err.Error(), "zm ci add") {
			t.Errorf("%v: %v", c, err)
		}
	}
}

// Review fixes: the public yes from the doctor is an ask, never a command;
// --public is for one named repository; the app is made on one account;
// an org's widen page is the org's; a blocked run blocks every repository.
func TestCIAddReviewCases(t *testing.T) {
	b := newFakeBroker(t)
	b.app, b.linked["acme"] = true, true
	b.repos = []runners.Repo{{Repo: "acme/pub", By: "upgrade"}} // stored visibility not read yet
	b.doctor["acme/pub"] = []runners.DoctorCheck{{Number: 5, Status: "fail", Message: "Its jobs are refused: a public repository its owner has not switched on", Fix: "zm runners allow acme/pub --public"}}
	raw, _, _ := ciAddRun(t, true, "acme/pub")
	var res ciAddResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatal(err)
	}
	if res.Next == nil || res.Next.Kind != "ask" || res.Next.Command != "" || len(res.Next.Choices) != 2 {
		t.Fatalf("the doctor's public fix came back as %+v", res.Next)
	}
	if len(b.connects) != 0 { // the doctor's own fix (zm runners allow --public) is never run
		t.Fatalf("connected without the yes: %+v", b.connects)
	}
	// The yes given again re-records it (a stale row is fixed by the POST).
	ciAddRun(t, false, "acme/pub", "--public")
	if len(b.connects) != 1 || !b.connects[0].Public {
		t.Fatalf("--public on a connected repository: %+v", b.connects)
	}
	b.connects, b.repos = nil, []runners.Repo{{Repo: "acme/pub", By: "upgrade"}}

	// Missing evidence on checks 1 to 5 is not done.
	b.doctor["acme/other"] = []runners.DoctorCheck{{Number: 3, Status: "warn", Message: "unavailable: GitHub could not confirm repository coverage", Fix: "https://github.example/apps/potluck-judy/installations/new"}}
	b.repos = append(b.repos, runners.Repo{Repo: "acme/other", Visibility: "private"})
	if _, last, done := ciAddRun(t, false, "acme/other"); done || !strings.Contains(last, "Could not check: GitHub could not confirm repository coverage.") {
		t.Errorf("unavailable coverage: %q", last)
	}

	fresh := func() *cobra.Command {
		r := &cobra.Command{Use: "zm", SilenceErrors: true, SilenceUsage: true}
		r.AddCommand(ciCmd())
		return r
	}
	for _, c := range [][]string{{"ci", "add", "a/b", "c/d", "--public"}, {"ci", "add", "--mine", "--public"}, {"ci", "add", "a/b", "c/d", "--workflow", "x.yml"}} {
		if _, err := runCLI(t, fresh(), c...); err == nil || !strings.Contains(err.Error(), "zm ci add <owner/repo>") {
			t.Errorf("%v: %v", c, err)
		}
	}

	ciOwnerIsOrg = func(_ context.Context, owner string) (bool, error) { return owner == "acme", nil }
	b.doctor["acme/tool"] = []runners.DoctorCheck{{Number: 3, Status: "fail", Message: "acme/tool is not included.", Fix: "ask the owner to widen the selected repositories at https://github.example/settings/installations/7 (organization owner: https://github.example/organizations/acme/settings/installations/7)"}}
	b.repos = append(b.repos, runners.Repo{Repo: "acme/tool", Visibility: "private"})
	if _, last, _ := ciAddRun(t, false, "acme/tool"); !strings.HasSuffix(last, "Open: https://github.example/organizations/acme/settings/installations/7") {
		t.Errorf("org widen step: %q", last)
	}

	b.app = false
	if _, err := runCLI(t, fresh(), "ci", "add", "acme/tool", "judy/app"); err == nil || !strings.Contains(err.Error(), "zm ci add") {
		t.Errorf("two owners with no app: %v", err)
	}
	b.pool = ""
	raw, _, _ = ciAddRun(t, true, "acme/tool", "judy/app")
	res = ciAddResult{}
	if err := json.Unmarshal([]byte(raw), &res); err != nil || res.State != "blocked" {
		t.Fatalf("%v\n%s", err, raw)
	}
	for _, rr := range res.Repos {
		if rr.State != "blocked" || rr.Next == nil || !strings.HasPrefix(rr.Next.Command, "zm ") {
			t.Errorf("repository in a blocked run: %+v", rr)
		}
	}
}

// --mine connects the repositories this machine's zm projects map.
func TestCIAddMine(t *testing.T) {
	b := newFakeBroker(t)
	b.app, b.linked["judy"] = true, true
	if out, last, done := ciAddRun(t, false, "--mine"); !done || last != "done" {
		t.Fatalf("%q\n%s", last, out)
	}
	if len(b.connects) != 2 || b.connects[0].Repo != "judy/app" || b.connects[1].Repo != "judy/site" || b.connects[0].Public {
		t.Fatalf("connects %+v", b.connects)
	}
}

// Review round 3: an actionable step wins over a blocked one whatever the
// order; each leftover repository gets its own owner's command; a gh
// failure on the org question stops instead of guessing the account, and
// --org= says "my own account"; a doctor that confirms nothing is not done.
func TestCIAddStepsPerRepository(t *testing.T) {
	b := newFakeBroker(t)
	b.app, b.linked["acme"], b.public["acme/pub"] = true, true, true
	raw, _, _ := ciAddRun(t, true, "beta/other", "acme/pub")
	var res ciAddResult
	if err := json.Unmarshal([]byte(raw), &res); err != nil {
		t.Fatal(err)
	}
	if res.State != "needs_human" || res.Next == nil || len(res.Next.Choices) != 2 || res.Repos[0].Next.Kind != "info" {
		t.Fatalf("blocked first: %s", raw)
	}

	b.linked = map[string]bool{}
	raw, _, _ = ciAddRun(t, true, "acme/a", "beta/x", "beta/y", "gamma/z", "acme/a")
	res = ciAddResult{}
	if err := json.Unmarshal([]byte(raw), &res); err != nil || len(res.Repos) != 4 {
		t.Fatalf("%v %s", err, raw)
	}
	// The app is private: another owner's step says it may never install
	// there, and is not a command to run.
	want := []string{"", "zm ci add beta/x beta/y again", "zm ci add beta/x beta/y again", "zm ci add gamma/z again"}
	for i, rr := range res.Repos {
		if i == 0 {
			if rr.Next == nil || rr.Next.URL == "" {
				t.Errorf("acme/a has no page: %+v", rr.Next)
			}
			continue
		}
		if rr.Next == nil || rr.Next.Kind != "info" || rr.Next.Command != "" || rr.State != "blocked" || !strings.Contains(rr.Next.Say, want[i]) ||
			!strings.Contains(rr.Next.Say, "only on the account it was made on") {
			t.Errorf("%s: %+v, want %q", rr.Repo, rr.Next, want[i])
		}
	}
	if res.State != "needs_human" || res.Next == nil || res.Next.URL == "" {
		t.Errorf("the page is not the run's step: %+v", res.Next)
	}

	b.app = false
	ciOwnerIsOrg = func(context.Context, string) (bool, error) { return false, errors.New("gh: not signed in") }
	r := &cobra.Command{Use: "zm", SilenceErrors: true, SilenceUsage: true}
	r.AddCommand(ciCmd())
	if _, err := runCLI(t, r, "ci", "add", "acme/a"); err == nil || !strings.Contains(err.Error(), "--org acme") || !strings.Contains(err.Error(), "--org=") {
		t.Fatalf("gh failure: %v", err)
	}
	n := len(b.setups)
	r2 := &cobra.Command{Use: "zm", SilenceErrors: true, SilenceUsage: true}
	r2.AddCommand(ciCmd())
	if _, err := runCLI(t, r2, "ci", "add", "acme/a", "--org", "acm"); err == nil || !strings.Contains(err.Error(), "--org acme") {
		t.Fatalf("--org for another account: %v", err)
	}
	ciAddRun(t, true, "acme/a", "--org=")
	if len(b.setups) != n+1 || b.setups[n].Org != "" {
		t.Fatalf("--org= did not make the app on the own account: %+v", b.setups)
	}

	b.app, b.linked["acme"] = true, true
	b.repos = []runners.Repo{{Repo: "acme/a", Visibility: "private"}}
	b.doctor["acme/a"] = []runners.DoctorCheck{{Number: 1, Status: "pass"}}
	if _, last, done := ciAddRun(t, false, "acme/a"); done || !strings.HasSuffix(last, "Run: zm ci doctor acme/a") {
		t.Fatalf("a doctor that confirms nothing: %q", last)
	}
}
