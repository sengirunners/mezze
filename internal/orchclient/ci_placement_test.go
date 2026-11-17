package orchclient_test

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runners"
	"github.com/sengirunners/potluck/runtarget/rt"
)

type ciAcquire struct {
	target           string
	subject, project string
	req              computeapi.AcquireRunTargetRequest
}

// Only the host boundary is fake: the real broker parses the generated labels,
// checks GitHub trust, chooses a box, mints a runner and acquires that box.
// No subprocess or actual VM is needed to prove placement.
type ciBoxes struct {
	project    string
	rt.Service // Unused host operations fail loudly if the test grows to need them.
	views      computeapi.RunTargetList
	acquired   chan ciAcquire
}

func (b *ciBoxes) List(context.Context, *authz.Subject, string, bool) (computeapi.RunTargetList, error) {
	return b.views, nil
}
func (b *ciBoxes) Acquire(_ context.Context, sub *authz.Subject, project, target string, req computeapi.AcquireRunTargetRequest) (computeapi.RunTargetHandle, error) {
	b.acquired <- ciAcquire{target: target, req: req, subject: sub.Address, project: project}
	return computeapi.RunTargetHandle{Handle: "box:test"}, nil
}
func (b *ciBoxes) Routes(r *core.Router) {
	r.HandleFunc("GET /v0/run_targets", core.Authed, "test host descriptions", func(w http.ResponseWriter, r *http.Request) { api.WriteJSON(w, 200, b.views) })
}

func TestCIWorkflowLinePlacesOnMacThroughBroker(t *testing.T) {
	ctx := context.Background()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("ZM_HOME", home)
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
	minted := make(chan []string, 1)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/installation/token":
			w.WriteHeader(http.StatusNoContent)
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
		case "/repos/judy/app/installation":
			fmt.Fprint(w, `{"id":7}`)
		case "/repos/judy/app":
			fmt.Fprint(w, `{"id":42,"full_name":"judy/app","private":true}`)
		case "/repos/judy/app/actions/jobs/1":
			fmt.Fprint(w, `{"id":1,"status":"queued"}`)
		case "/repos/judy/app/actions/runs/10":
			fmt.Fprint(w, `{"id":10,"event":"push","status":"queued","repository":{"full_name":"judy/app"},"head_repository":{"full_name":"judy/app"}}`)
		case "/repos/judy/app/actions/runners/generate-jitconfig":
			w.WriteHeader(http.StatusCreated)
			var request struct {
				Labels []string `json:"labels"`
			}
			_ = json.NewDecoder(r.Body).Decode(&request)
			minted <- request.Labels
			fmt.Fprint(w, `{"runner":{"id":99,"name":"potluck-1"},"encoded_jit_config":"test-jit"}`)
		default:
			t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL)
			w.WriteHeader(500)
		}
	}))
	t.Cleanup(gh.Close)
	cfg.File.Runners = config.Runners{PublicURL: "https://pool.example", Label: "my-mac", JobTTLS: 5400, Pool: config.RunnersPool{Project: "home"}, GitHub: config.RunnersGitHub{AppID: 1, ClientID: "test-client", APIURL: gh.URL, WebURL: gh.URL, PrivateKey: keyRef, ClientSecret: secret("client-secret", "test-client-secret"), WebhookSecret: secret("hook-secret", "test-hook")}}
	// The host must fit the 5400 s fallback runtime plus the broker's
	// 300 s startup allowance; both advertised caps permit that placement.
	boxes := &ciBoxes{acquired: make(chan ciAcquire, 1)}
	if err := json.Unmarshal([]byte(`{"targets":[{"name":"linux","kind":"box","host_class":"trusted","describe":{"available":true,"ttl_max_s":7200,"capabilities":["github-jit","github-jit-guard"],"size":{"name":"c4m16","os":"linux"}}},{"name":"mac","kind":"box","host_class":"trusted","ttl_max_s":7200,"describe":{"available":true,"ttl_max_s":7200,"capabilities":["github-jit","github-jit-guard"],"size":{"name":"c4m16","os":"darwin"}}}]}`), &boxes.views); err != nil {
		t.Fatal(err)
	}
	var broker *runners.Service
	dm, err := server.New(ctx, cfg, nil, func(d *core.Deps) ([]core.Module, error) {
		d.Provide(rt.Name, boxes)
		broker = runners.New(d)
		broker.Retry = 10 * time.Millisecond
		// The doctor's self-check never runs the real gh in a test.
		broker.DoctorRead = func(context.Context, string, any) error { return errors.New("no gh in tests") }
		return []core.Module{broker, boxes}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "home", "flag"
	project, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "home"})
	if err != nil {
		t.Fatal(err)
	}
	boxes.project = project.ID
	var st runners.Setup
	if err := owner.Do(ctx, "GET", "/v0/runners/setup", nil, &st); err != nil {
		t.Fatal(err)
	}
	// A host capped at runtime alone must not be advertised: the producer
	// also needs startup time before it can execute that runtime.
	macCap := boxes.views.Targets[1].Describe.TTLMaxS
	boxes.views.Targets[1].Describe.TTLMaxS = 5400
	shortLine, err := orchclient.CIWorkflowLine(ctx, owner, st)
	if err != nil || strings.Contains(shortLine, `"macos"`) || !strings.Contains(shortLine, `"linux"`) {
		t.Fatalf("runtime-only Mac cap advertised: %q (%v)", shortLine, err)
	}
	for _, cap := range []int{5700, macCap} {
		boxes.views.Targets[1].Describe.TTLMaxS = cap
		line, err := orchclient.CIWorkflowLine(ctx, owner, st)
		if err != nil || !strings.Contains(line, `"macos"`) {
			t.Fatalf("sufficient Mac cap %d not advertised: %q (%v)", cap, line, err)
		}
	}
	line, err := orchclient.CIWorkflowLine(ctx, owner, st)
	if err != nil {
		t.Fatal(err)
	}
	// The YAML flow sequence is JSON: feed it unchanged into a workflow_job.
	var labels []string
	if !strings.HasPrefix(line, "runs-on: ") {
		t.Fatalf("no workflow line: %q", line)
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "runs-on: ")), &labels); err != nil {
		t.Fatal(err)
	}
	var link struct {
		URL string `json:"authorize_url"`
	}
	if err := owner.Do(ctx, "POST", "/v0/runners/links", map[string]any{"installation_id": 7}, &link); err != nil {
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
	// The broker serves only a repository its pool's owner connected.
	var connected runners.Repo
	if err := owner.Do(ctx, "POST", "/v0/runners/repos", runners.AllowRequest{Repo: "judy/app"}, &connected); err != nil || connected.Repo != "judy/app" {
		t.Fatalf("connect judy/app: %+v %v", connected, err)
	}
	if err := broker.Start(ctx); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"action": "queued", "installation": map[string]int{"id": 7}, "repository": map[string]any{"id": 42, "full_name": "judy/app", "private": true}, "workflow_job": map[string]any{"id": 1, "run_id": 10, "labels": labels}})
	mac := hmac.New(sha256.New, []byte("test-hook"))
	mac.Write(body)
	req := httptest.NewRequest("POST", "/v0/runners/hook", strings.NewReader(string(body)))
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	req.Header.Set("X-GitHub-Event", "workflow_job")
	req.Header.Set("X-GitHub-Delivery", "ci-line-test")
	w = httptest.NewRecorder()
	dm.Handler.ServeHTTP(w, req)
	if w.Code != 202 {
		t.Fatalf("hook: %d %s", w.Code, w.Body)
	}
	select {
	case got := <-boxes.acquired:
		if got.target != "mac" || !strings.Contains(got.subject, runners.Identity) || got.project != boxes.project || got.req.TTLS != 5700 || got.req.JITConfig != "test-jit" || got.req.JITGuard == nil {
			t.Fatalf("line %q placed incorrectly: %+v", line, got)
		}
	case <-time.After(5 * time.Second):
		var jobs any
		_ = owner.Do(ctx, "GET", "/v0/runners/jobs", nil, &jobs)
		t.Fatalf("line %q did not place: %+v", line, jobs)
	}
	select {
	case got := <-minted:
		if !reflect.DeepEqual(got, labels) {
			t.Fatalf("labels changed: %v -> %v", labels, got)
		}
	default:
		t.Fatal("no runner minted")
	}
}
