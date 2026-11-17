package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runners"
)

// brokerDaemon is a real daemon with the runner broker on, its pool linked to
// installation 7 of a fake GitHub that has a private judy/app and a public
// acme/pub. Only GitHub is fake: zm's commands run through the daemon's
// socket against the broker's own rules.
func brokerDaemon(t *testing.T) {
	t.Helper()
	oldJSON, oldAs, oldProject := flagJSON, flagAs, flagProject
	t.Cleanup(func() { flagJSON, flagAs, flagProject = oldJSON, oldAs, oldProject })
	flagJSON, flagAs, flagProject = false, "", ""
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	for k, v := range map[string]string{"ZM_HOME": home, "ZM_NO_AUTOSTART": "1", "ZM_HOST": "", "ZM_TOKEN": "", "ZM_PROJECT": "", "ZM_AS": ""} {
		t.Setenv(k, v)
	}
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "judy", "mini"
	secret := func(name, value string) string {
		p := filepath.Join(home, name)
		if err := os.WriteFile(p, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		return "file:" + p
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyRef := secret("key.pem", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/app/installations/7":
			fmt.Fprint(w, `{"id":7,"account":{"login":"judy","type":"User"}}`)
		case "/login/oauth/access_token":
			fmt.Fprint(w, `{"access_token":"user-token"}`)
		case "/user":
			fmt.Fprint(w, `{"login":"judy"}`)
		case "/user/installations":
			fmt.Fprint(w, `{"total_count":1,"installations":[{"id":7}]}`)
		case "/app/installations/7/access_tokens":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"installation-token","expires_at":%q}`, time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
		case "/repos/judy/app/installation", "/repos/acme/pub/installation":
			fmt.Fprint(w, `{"id":7}`)
		case "/repos/judy/app":
			fmt.Fprint(w, `{"id":42,"full_name":"judy/app","private":true,"visibility":"private"}`)
		case "/repos/acme/pub":
			fmt.Fprint(w, `{"id":43,"full_name":"acme/pub","private":false,"visibility":"public"}`)
		case "/repos/acme/pub/actions/permissions/fork-pr-contributor-approval":
			fmt.Fprint(w, `{"approval_policy":"first_time_contributors"}`)
		default:
			// The checker's lists: nothing is waiting.
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Not Found"}`)
		}
	}))
	t.Cleanup(gh.Close)
	cfg.File.Runners = config.Runners{PublicURL: "https://pool.example", Pool: config.RunnersPool{Project: "pool"}, GitHub: config.RunnersGitHub{
		AppID: 1, ClientID: "test-client", APIURL: gh.URL, WebURL: gh.URL, PrivateKey: keyRef,
		ClientSecret: secret("client-secret", "test-client-secret"), WebhookSecret: secret("hook-secret", "test-hook")}}
	ctx, cancel := context.WithCancel(context.Background())
	dm, err := server.New(ctx, cfg, nil, wiring.Full)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("daemon did not stop")
		}
	})
	sock := client.NewUnix(cfg.SocketPath())
	for i := 0; ; i++ {
		if _, err := sock.Health(ctx); err == nil {
			break
		}
		if i > 400 {
			t.Fatal("socket never answered")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := sock.CreateProject(ctx, api.NewProjectRequest{Slug: "pool"}); err != nil {
		t.Fatal(err)
	}
	var link struct {
		URL string `json:"authorize_url"`
	}
	if err := sock.Do(ctx, "POST", "/v0/runners/links", map[string]any{"installation_id": 7}, &link); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link.URL)
	if err != nil {
		t.Fatal(err)
	}
	public := func(method, path, body, contentType string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", contentType)
		w := httptest.NewRecorder()
		dm.Handler.ServeHTTP(w, req)
		return w
	}
	w := public("GET", "/v0/runners/oauth?state="+u.Query().Get("state")+"&code=test-code", "", "")
	match := regexp.MustCompile(`name="state" value="([0-9a-f]{48})"`).FindStringSubmatch(w.Body.String())
	if w.Code != 200 || match == nil {
		t.Fatalf("link confirmation: %d %s", w.Code, w.Body)
	}
	if w := public("POST", "/v0/runners/oauth", "state="+match[1], "application/x-www-form-urlencoded"); w.Code != 200 {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
}

// The repository commands through the real broker: a private repository is
// connected plain, a public one only with --public, the broker's refusals
// name zm's commands, a repository name with URL characters never reaches
// another repository's row, and unlinking the installation disconnects what
// it connected.
func TestRunnersAllowDisallowThroughBroker(t *testing.T) {
	brokerDaemon(t)
	cli := func(args ...string) (string, error) { return runCLI(t, runnersCmd(), args...) }

	// A private repository needs no yes; --public on it is refused with
	// zm's command in the hint.
	_, err := cli("allow", "judy/app", "--public")
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "is private") || !strings.Contains(err.Error(), "zm runners allow judy/app") || strings.Contains(err.Error(), "potluck") {
		t.Fatalf("allow --public on a private repository: %v", err)
	}
	if out, err := cli("repos"); err != nil || !strings.Contains(out, "none connected") {
		t.Fatalf("the refused allow connected something: %v\n%s", err, out)
	}
	out, err := cli("allow", "judy/app")
	if err != nil || !strings.Contains(out, "zm: judy/app connected (served), from its next job on") {
		t.Fatalf("allow judy/app: %v\n%s", err, out)
	}

	// A public repository is refused without --public, and with it served for
	// its own branches only, with the broker's warning about its approval rule.
	_, err = cli("allow", "acme/pub")
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "zm runners allow acme/pub --public") || strings.Contains(err.Error(), "potluck") {
		t.Fatalf("allow a public repository without --public: %v", err)
	}
	out, err = cli("allow", "acme/pub", "--public")
	if err != nil || !strings.Contains(out, "zm: acme/pub connected (served while public: its own branches only, never a fork's pull request)") ||
		!strings.Contains(out, "zm: warning: Outside contributors who have contributed before") {
		t.Fatalf("allow acme/pub --public: %v\n%s", err, out)
	}
	out, err = cli("repos")
	for _, want := range []string{"judy/app", "private", "acme/pub", "public", "Outside contributors who have contributed before"} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("repos lacks %q: %v\n%s", want, err, out)
		}
	}
	if strings.Contains(out, "not served until") {
		t.Errorf("a public repository with the owner's yes is said not to be served:\n%s", out)
	}

	// disallow escapes the name it sends: "app?x" is not "app" with a query.
	_, err = cli("disallow", "judy/app?x")
	if !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("disallow judy/app?x: %v", err)
	}
	if out, err = cli("repos"); err != nil || !strings.Contains(out, "judy/app") {
		t.Fatalf("disallow judy/app?x disconnected judy/app: %v\n%s", err, out)
	}
	if out, err = cli("disallow", "judy/app"); err != nil || !strings.Contains(out, "zm: judy/app disconnected") {
		t.Fatalf("disallow judy/app: %v\n%s", err, out)
	}

	// Unlinking the installation takes acme/pub with it, and says it will.
	if out, err = cli("unlink", "--help"); err != nil || !strings.Contains(out, "disconnected too") {
		t.Fatalf("unlink help: %v\n%s", err, out)
	}
	if out, err = cli("unlink", "7"); err != nil || !strings.Contains(out, "installation 7 unlinked") {
		t.Fatalf("unlink: %v\n%s", err, out)
	}
	if out, err = cli("repos"); err != nil || !strings.Contains(out, "none connected") {
		t.Fatalf("unlink left repositories connected: %v\n%s", err, out)
	}
}

// A public repository the upgrade connected has no yes yet: its row says it
// is not served and what to run, and the upgrade step is under the table and
// in the commands' help.
func TestRunnersReposSayWhatTheUpgradeLeft(t *testing.T) {
	var out strings.Builder
	printRunnersRepos(&out, []runners.Repo{
		{Repo: "acme/pub", Visibility: "public", By: "upgrade", Warning: "Outside contributors can start workflow runs on this public repository without your approval."},
		{Repo: "judy/app", Visibility: "private", By: "upgrade"},
	})
	for _, want := range []string{"not served until zm runners allow acme/pub --public; Outside contributors can start", "zm runners allow <owner/repo> (--public for a public one)", "last 30 days"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("repos lacks %q:\n%s", want, out.String())
		}
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "judy/app") && strings.Contains(line, "not served") {
			t.Errorf("a private repository is said not to be served:\n%s", out.String())
		}
	}
	// A row the upgrade made before the broker read its visibility may be a
	// public one, which is not served: it says so.
	var unread strings.Builder
	printRunnersRepos(&unread, []runners.Repo{{Repo: "acme/new", By: "upgrade"}, {Repo: "judy/mine", By: "ana"}})
	rows := 0
	for _, line := range strings.Split(unread.String(), "\n") {
		switch {
		case strings.Contains(line, "acme/new"):
			rows++
			if !strings.Contains(line, "if public, not served until zm runners allow acme/new --public") {
				t.Errorf("unread visibility row:\n%s", unread.String())
			}
		case strings.Contains(line, "judy/mine"):
			rows++
			if strings.Contains(line, "not served") {
				t.Errorf("a repository with a person's connection is said not to be served:\n%s", unread.String())
			}
		}
	}
	if rows != 2 {
		t.Errorf("rows missing:\n%s", unread.String())
	}
	var rest strings.Builder
	printRunnersRepos(&rest, []runners.Repo{{Repo: "judy/app", Visibility: "private", By: "ana"}})
	if strings.Contains(rest.String(), "last 30 days") {
		t.Errorf("the upgrade step is printed with no upgrade row:\n%s", rest.String())
	}
	for _, sub := range []string{"repos", "allow"} {
		help, err := runCLI(t, runnersCmd(), sub, "--help")
		if err != nil || !strings.Contains(help, "last 30 days") || !strings.Contains(help, "zm runners allow") {
			t.Errorf("zm runners %s --help lacks the upgrade step: %v\n%s", sub, err, help)
		}
	}
}

// ls shows how jobs arrive and the checker's last round, and still prints
// everything else when the repositories cannot be read (a daemon not yet
// restarted into this zm's version answers 404 for them).
func TestRunnersLsShowsCheckAndSurvivesNoRepos(t *testing.T) {
	runtimeClientEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		switch r.URL.Path {
		case "/v0/runners/setup":
			fmt.Fprint(w, `{"pool":"home","label":"home","public_url":"https://pool.example","listen":"127.0.0.1:1","listening":"127.0.0.1:1","job_ttl_s":3600,"app":{"id":1,"slug":"z3"},
				"check":{"mode":"check","every_s":15,"error":"no App client","error_at":"2026-11-12T10:00:00Z","installations":[{"installation_id":7,"repos":3,"last_ok":"2026-11-12T09:00:00Z","error":"judy/app runs: 502","error_at":"2026-11-12T09:30:00Z","cut":"budget spent","cut_at":"2026-11-12T09:31:00Z"}]}}`)
		case "/v0/runners/links":
			fmt.Fprint(w, `{"links":[]}`)
		case "/v0/runners/repos":
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"code":"not_found","message":"no route"}`)
		case "/v0/run_targets":
			fmt.Fprint(w, `{"targets":[]}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(500)
		}
	}))
	out, err := runCLI(t, runnersCmd(), "ls")
	if err != nil {
		t.Fatalf("ls with the repositories unreadable: %v\n%s", err, out)
	}
	for _, want := range []string{
		"check       asking GitHub every 15 s",
		"could not check at 2026-11-12T10:00:00Z: no App client",
		"installation 7: 3 repositories, last complete round 2026-11-12T09:00:00Z",
		"errors since (2026-11-12T09:30:00Z): judy/app runs: 502",
		"cut short (2026-11-12T09:31:00Z): budget spent",
		"repos       could not read: ",
		"links       none",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("ls lacks %q:\n%s", want, out)
		}
	}
	flagJSON = true
	t.Cleanup(func() { flagJSON = false })
	out, err = runCLI(t, runnersCmd(), "ls")
	if err != nil || !strings.Contains(out, `"repos_error"`) {
		t.Fatalf("ls --json with the repositories unreadable: %v\n%s", err, out)
	}
}
