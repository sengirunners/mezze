package webhooks_test

// Acceptance tests for outgoing webhooks: signed delivery, filters, retries,
// auto-disable, a durable queue across a daemon restart, the creator's scope
// (areas, revoked grants and tokens, hidden sessions), and SSRF refusals.
// Receivers are httptest servers on loopback; nothing leaves the machine.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/webhooks"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/server"
	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"
)

func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "BEADS_") || strings.HasPrefix(k, "BD_") || strings.HasPrefix(k, "DOLT_") {
			os.Unsetenv(k)
		}
	}
	os.Exit(m.Run())
}

type hx struct {
	t      *testing.T
	home   string
	dm     *server.Daemon
	svc    *webhooks.Service
	owner  *client.Client
	cancel context.CancelFunc
}

func newHome(t *testing.T) string {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	return home
}

// open wires a daemon on home (not started).
func open(t *testing.T, home string) *hx {
	t.Helper()
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	h := &hx{t: t, home: home, dm: dm, owner: client.WithHandler(dm.SocketHandler())}
	for _, m := range dm.Modules() {
		if s, ok := m.(*webhooks.Service); ok {
			h.svc = s
		}
	}
	if h.svc == nil {
		t.Fatal("webhooks module not wired")
	}
	h.svc.Backoff = []time.Duration{20 * time.Millisecond}
	h.svc.Tick = 10 * time.Millisecond
	h.svc.Timeout = 2 * time.Second
	t.Cleanup(h.close)
	return h
}

func (h *hx) start() {
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	if err := h.svc.Start(ctx); err != nil {
		h.t.Fatal(err)
	}
}

func (h *hx) close() {
	if h.dm == nil {
		return
	}
	if h.cancel != nil {
		h.cancel()
	}
	h.dm.Close()
	h.dm = nil
}

func newHX(t *testing.T) *hx {
	h := open(t, newHome(t))
	ctx := context.Background()
	if _, err := h.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}); err != nil {
		t.Fatal(err)
	}
	for _, a := range []string{"solver", "billing"} {
		if _, err := h.owner.AddArea(ctx, "harbor", api.Area{Name: a}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "side"}); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *hx) at(project string) *client.Client {
	c := client.WithHandler(h.dm.SocketHandler())
	c.Project, c.ProjectFrom = project, "flag"
	return c
}

func (h *hx) work(project, area, title string) orchapi.WorkItem {
	h.t.Helper()
	it, err := orchclient.CreateWork(context.Background(), h.at(project), orchapi.CreateWorkRequest{Title: title, Area: area})
	if err != nil {
		h.t.Fatal(err)
	}
	return it
}

// ---- a receiver ----

type got struct {
	hdr  http.Header
	body []byte
	ev   api.Event
}

type receiver struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	all    []got
	calls  atomic.Int32
	status func(n int) int // n = 1-based call number
}

func newReceiver(t *testing.T) *receiver {
	r := &receiver{t: t, status: func(int) int { return 200 }}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		n := int(r.calls.Add(1))
		b, _ := io.ReadAll(req.Body)
		st := r.status(n)
		if st >= 200 && st < 300 {
			var ev api.Event
			_ = json.Unmarshal(b, &ev)
			r.mu.Lock()
			r.all = append(r.all, got{hdr: req.Header.Clone(), body: b, ev: ev})
			r.mu.Unlock()
		}
		if st == http.StatusFound {
			w.Header().Set("Location", "http://169.254.169.254/latest/meta-data/")
		}
		w.WriteHeader(st)
		_, _ = w.Write([]byte("answer\x00with\ncontrol"))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *receiver) received() []got {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]got(nil), r.all...)
}

func (r *receiver) types() []string {
	var out []string
	for _, g := range r.received() {
		out = append(out, g.ev.Type)
	}
	return out
}

func eventually(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (h *hx) addHook(req orchapi.NewWebhookRequest) orchapi.NewWebhookResponse {
	h.t.Helper()
	r, err := orchclient.AddWebhook(context.Background(), h.owner, req)
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

// ---- tests ----

func TestDeliverySignedAndFiltered(t *testing.T) {
	h := newHX(t)
	h.start()
	rcv := newReceiver(t)
	res := h.addHook(orchapi.NewWebhookRequest{URL: rcv.srv.URL + "/hook", Project: "harbor", Events: []string{"work.*"}, AllowPrivate: true})
	if res.Secret == "" || !strings.HasPrefix(res.Secret, "whsec_") {
		t.Fatalf("no generated secret: %+v", res)
	}
	it := h.work("harbor", "solver", "the first")
	h.work("side", "", "elsewhere") // another project: not this hook's
	if _, err := orchclient.Send(context.Background(), h.at("harbor"), orchapi.SendMailRequest{To: "human:ivo", Body: "mail"}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the work.created delivery", func() bool { return len(rcv.received()) >= 1 })
	time.Sleep(100 * time.Millisecond)
	gs := rcv.received()
	if len(gs) != 1 {
		t.Fatalf("want exactly the harbor work event, got %v", rcv.types())
	}
	g := gs[0]
	if g.ev.Type != orchapi.EventWorkCreated || g.ev.Subject != it.ID {
		t.Fatalf("delivered %s %s", g.ev.Type, g.ev.Subject)
	}
	secret := []byte(res.Secret)
	if err := webhooks.Verify(secret, g.hdr.Get(orchapi.HeaderWebhookSignature), g.hdr.Get(orchapi.HeaderWebhookTimestamp), g.body, time.Now(), 0); err != nil {
		t.Fatalf("signature: %v", err)
	}
	if !webhooks.VerifyHub(secret, g.hdr.Get(orchapi.HeaderHubSignature), g.body) {
		t.Fatal("X-Hub-Signature-256 does not verify")
	}
	// Standard Webhooks: the reference library verifies a daemon-made secret
	// as given, and webhook-id is the delivery id.
	std, err := standardwebhooks.NewWebhook(res.Secret)
	if err != nil {
		t.Fatalf("daemon secret is not a Standard Webhooks secret: %v", err)
	}
	if err := std.Verify(g.body, g.hdr); err != nil {
		t.Fatalf("webhook-signature: %v", err)
	}
	if err := webhooks.VerifyStd(res.Secret, g.hdr, g.body, time.Now(), 0); err != nil {
		t.Fatalf("VerifyStd: %v", err)
	}
	if g.hdr.Get(orchapi.HeaderStdWebhookID) == "" || g.hdr.Get(orchapi.HeaderStdWebhookID) != g.hdr.Get(orchapi.HeaderWebhookDelivery) {
		t.Errorf("webhook-id %q, delivery %q", g.hdr.Get(orchapi.HeaderStdWebhookID), g.hdr.Get(orchapi.HeaderWebhookDelivery))
	}
	if webhooks.Verify([]byte("wrong-secret-0123456789"), g.hdr.Get(orchapi.HeaderWebhookSignature), g.hdr.Get(orchapi.HeaderWebhookTimestamp), g.body, time.Now(), 0) == nil {
		t.Fatal("a wrong secret verified")
	}
	// Replay: the same signed body an hour later is refused; a changed body too.
	if err := webhooks.Verify(secret, g.hdr.Get(orchapi.HeaderWebhookSignature), g.hdr.Get(orchapi.HeaderWebhookTimestamp), g.body, time.Now().Add(time.Hour), 0); err != webhooks.ErrStale {
		t.Fatalf("an hour-old delivery verified: %v", err)
	}
	tampered := append([]byte(nil), g.body...)
	tampered[len(tampered)-2] ^= 1
	if webhooks.Verify(secret, g.hdr.Get(orchapi.HeaderWebhookSignature), g.hdr.Get(orchapi.HeaderWebhookTimestamp), tampered, time.Now(), 0) == nil {
		t.Fatal("a tampered body verified")
	}
	for _, k := range []string{orchapi.HeaderWebhookID, orchapi.HeaderWebhookDelivery, orchapi.HeaderWebhookEvent} {
		if g.hdr.Get(k) == "" {
			t.Errorf("missing header %s", k)
		}
	}
	if g.hdr.Get(orchapi.HeaderWebhookID) != res.Webhook.ID || g.hdr.Get(orchapi.HeaderWebhookEvent) != orchapi.EventWorkCreated {
		t.Errorf("headers: %v", g.hdr)
	}
	d, err := orchclient.Webhook(context.Background(), h.owner, res.Webhook.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Deliveries) != 1 || d.Deliveries[0].Outcome != "delivered" || d.Deliveries[0].Status != 200 || d.Webhook.LastOK == nil {
		t.Fatalf("log: %+v", d)
	}
	// The secret is never shown again.
	ws, _ := orchclient.Webhooks(context.Background(), h.owner, "")
	js, _ := json.Marshal(ws)
	if strings.Contains(string(js), res.Secret) {
		t.Fatal("ls shows the secret")
	}
}

func TestRetryThenDeliverSameDeliveryID(t *testing.T) {
	h := newHX(t)
	h.start()
	rcv := newReceiver(t)
	rcv.status = func(n int) int {
		if n <= 2 {
			return 503
		}
		return 204
	}
	var ids []string
	var mu sync.Mutex
	inner := rcv.srv.Config.Handler
	rcv.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ids = append(ids, r.Header.Get(orchapi.HeaderWebhookDelivery))
		mu.Unlock()
		inner.ServeHTTP(w, r)
	})
	res := h.addHook(orchapi.NewWebhookRequest{URL: rcv.srv.URL, Project: "harbor", AllowPrivate: true, Secret: "a-secret-of-sixteen+"})
	if res.Secret != "" {
		t.Fatal("a supplied secret was echoed")
	}
	h.work("harbor", "", "x")
	eventually(t, "delivery after retries", func() bool { return len(rcv.received()) == 1 })
	mu.Lock()
	if len(ids) != 3 || ids[0] != ids[1] || ids[1] != ids[2] {
		t.Fatalf("delivery ids across retries: %v", ids)
	}
	mu.Unlock()
	// The receiver answers before the daemon records the attempt: wait for it.
	var d orchapi.WebhookDetail
	eventually(t, "the delivery recorded", func() bool {
		d, _ = orchclient.Webhook(context.Background(), h.owner, res.Webhook.ID, 0)
		return d.Webhook.LastOK != nil && len(d.Deliveries) == 3
	})
	var outcomes []string
	for _, x := range d.Deliveries {
		outcomes = append(outcomes, x.Outcome)
		if x.Outcome == "retry" && (!strings.HasPrefix(x.Error, "HTTP 503") || strings.ContainsAny(x.Error, "\x00\n")) {
			t.Errorf("retry error not sanitised: %q", x.Error)
		}
	}
	if strings.Join(outcomes, ",") != "delivered,retry,retry" {
		t.Fatalf("outcomes (newest first): %v", outcomes)
	}
	if d.Webhook.FailingSince != nil || d.Webhook.State != orchapi.WebhookActive {
		t.Fatalf("streak not reset: %+v", d.Webhook)
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	h := newHX(t)
	h.svc.MaxAttempts = 1
	h.start()
	rcv := newReceiver(t)
	rcv.status = func(int) int { return http.StatusFound }
	res := h.addHook(orchapi.NewWebhookRequest{URL: rcv.srv.URL, Project: "harbor", AllowPrivate: true})
	h.work("harbor", "", "x")
	eventually(t, "a failed attempt", func() bool {
		d, _ := orchclient.Webhook(context.Background(), h.owner, res.Webhook.ID, 0)
		return len(d.Deliveries) == 1 && d.Deliveries[0].Outcome == "failed" && d.Deliveries[0].Status == 302
	})
}

func TestAutoDisableAndEnable(t *testing.T) {
	h := newHX(t)
	h.svc.DisableAfter = 50 * time.Millisecond
	h.svc.DisableMinFailures = 3
	h.start()
	ch, cancel := h.dm.Deps.Events.Subscribe(h.dm.Deps.System(), events.Filter{Types: []string{orchapi.EventWebhookDisabled}}, 16)
	defer cancel()
	rcv := newReceiver(t)
	var failing atomic.Bool
	failing.Store(true)
	rcv.status = func(int) int {
		if failing.Load() {
			return 500
		}
		return 200
	}
	res := h.addHook(orchapi.NewWebhookRequest{URL: rcv.srv.URL, Project: "harbor", AllowPrivate: true})
	h.work("harbor", "", "x")
	select {
	case e := <-ch:
		if e.Subject != res.Webhook.ID || e.Project != api.ProjectNone {
			t.Fatalf("disabled event: %+v", e)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("never disabled")
	}
	ws, _ := orchclient.Webhooks(context.Background(), h.owner, "harbor")
	if len(ws) != 1 || ws[0].State != orchapi.WebhookDisabled || !strings.Contains(ws[0].Disabled, "failing since") || ws[0].Pending != 1 {
		t.Fatalf("after disable: %+v", ws)
	}
	// Disabled: new events are not queued.
	h.work("harbor", "", "while off")
	time.Sleep(100 * time.Millisecond)
	if ws, _ := orchclient.Webhooks(context.Background(), h.owner, "harbor"); ws[0].Pending != 1 {
		t.Fatalf("queued while disabled: %d", ws[0].Pending)
	}
	// A ping works on a disabled hook (to check a fix).
	failing.Store(false)
	tr, err := orchclient.TestWebhook(context.Background(), h.owner, res.Webhook.ID)
	if err != nil || !tr.OK || tr.Delivery.Outcome != "ping" {
		t.Fatalf("test: %+v %v", tr, err)
	}
	on := true
	if _, err := orchclient.UpdateWebhook(context.Background(), h.owner, res.Webhook.ID, orchapi.UpdateWebhookRequest{Enabled: &on}); err != nil {
		t.Fatal(err)
	}
	h.work("harbor", "", "after")
	eventually(t, "the queued event and the new one", func() bool {
		var titles []string
		for _, g := range rcv.received() {
			if g.ev.Type == orchapi.EventWorkCreated {
				var it orchapi.WorkItem
				_ = json.Unmarshal(g.ev.Data, &it)
				titles = append(titles, it.Title)
			}
		}
		return strings.Join(titles, ",") == "x,after"
	})
}

func TestGoneDisablesAtOnce(t *testing.T) {
	h := newHX(t)
	h.start()
	rcv := newReceiver(t)
	rcv.status = func(int) int { return http.StatusGone }
	res := h.addHook(orchapi.NewWebhookRequest{URL: rcv.srv.URL, Project: "harbor", AllowPrivate: true})
	h.work("harbor", "", "x")
	eventually(t, "disabled by 410", func() bool {
		d, _ := orchclient.Webhook(context.Background(), h.owner, res.Webhook.ID, 0)
		return d.Webhook.State == orchapi.WebhookDisabled
	})
}

// The queue and the cursor are on disk: a delivery pending at shutdown is
// delivered after the restart, and events published while the module was
// not running are caught up from the event log, each exactly once.
func TestDurableAcrossRestart(t *testing.T) {
	h := newHX(t)
	h.start()
	rcv := newReceiver(t)
	var up atomic.Bool
	rcv.status = func(int) int {
		if up.Load() {
			return 200
		}
		return 503
	}
	res := h.addHook(orchapi.NewWebhookRequest{URL: rcv.srv.URL, Project: "harbor", AllowPrivate: true, Events: []string{"work.created"}})
	h.work("harbor", "", "before restart")
	eventually(t, "a failed attempt", func() bool {
		d, _ := orchclient.Webhook(context.Background(), h.owner, res.Webhook.ID, 0)
		return len(d.Deliveries) > 0
	})
	h.close()

	// Restart: the module is wired but not started while two more items land.
	h2 := open(t, h.home)
	h2.work("harbor", "", "while stopped 1")
	h2.work("harbor", "", "while stopped 2")
	up.Store(true)
	h2.start()
	want := "before restart,while stopped 1,while stopped 2"
	eventually(t, "all three, in order", func() bool {
		var titles []string
		for _, g := range rcv.received() {
			var it orchapi.WorkItem
			_ = json.Unmarshal(g.ev.Data, &it)
			titles = append(titles, it.Title)
		}
		return strings.Join(titles, ",") == want
	})
	time.Sleep(150 * time.Millisecond)
	if n := len(rcv.received()); n != 3 {
		t.Fatalf("delivered %d times, want 3", n)
	}
}

// A follower that falls behind is cut off by the bus (never blocking it)
// and catches up from the log: every event arrives exactly once, in order.
func TestBurstNeverBlocksAndLosesNothing(t *testing.T) {
	h := newHX(t)
	h.start()
	rcv := newReceiver(t)
	p, _ := h.owner.GetProject(context.Background(), "harbor")
	h.addHook(orchapi.NewWebhookRequest{URL: rcv.srv.URL, Project: "harbor", AllowPrivate: true, Events: []string{"test.burst"}})
	n := 1500
	if raceOn {
		n = 300 // the race detector makes each delivery ~50x slower; 300 still overflows the 256 buffer
	}
	start := time.Now()
	for i := 0; i < n; i++ {
		if _, err := h.dm.Deps.Events.Publish(context.Background(), events.Input{Type: "dev.zm.test.burst", Subject: "s", Project: p.ID, Data: i}); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("publishing %d events took %s: the bus waited on the webhook", n, d)
	}
	deadline := time.Now().Add(60 * time.Second)
	for len(rcv.received()) < n && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := len(rcv.received()); got < n {
		var pending int
		h.dm.Deps.Store.Registry().QueryRow(`SELECT count(*) FROM webhook_queue`).Scan(&pending)
		t.Fatalf("delivered %d of %d (queued %d)", got, n, pending)
	}
	time.Sleep(100 * time.Millisecond)
	gs := rcv.received()
	if len(gs) != n {
		t.Fatalf("got %d deliveries, want %d", len(gs), n)
	}
	for i, g := range gs {
		var v int
		_ = json.Unmarshal(g.ev.Data, &v)
		if v != i {
			t.Fatalf("delivery %d carries %d: out of order or duplicated", i, v)
		}
	}
}

// A hook sees only what its creator may read, re-evaluated when grants or
// tokens change.
func TestCreatorScope(t *testing.T) {
	h := newHX(t)
	h.start()
	ctx := context.Background()
	rcv := newReceiver(t)
	tok, err := h.owner.MintToken(ctx, api.MintTokenRequest{As: "vida", Project: "harbor", Area: "solver", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	v := client.WithHandler(h.dm.Handler) // a network caller with the token
	v.Token = tok.Secret

	// Network callers: a project is required, private targets refused, and
	// only an admin of the area may add one.
	if _, err := orchclient.AddWebhook(ctx, v, orchapi.NewWebhookRequest{URL: "https://hooks.example.com/x"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("no project: %v", err)
	}
	if _, err := orchclient.AddWebhook(ctx, v, orchapi.NewWebhookRequest{URL: rcv.srv.URL, Project: "harbor/solver", AllowPrivate: true}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("allow_private from the network: %v", err)
	}
	if _, err := orchclient.AddWebhook(ctx, v, orchapi.NewWebhookRequest{URL: rcv.srv.URL, Project: "harbor/solver"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("loopback from the network: %v", err)
	}
	if _, err := orchclient.AddWebhook(ctx, v, orchapi.NewWebhookRequest{URL: "https://hooks.example.com/x", Project: "harbor"}); err == nil {
		t.Fatal("an area owner added a project-wide hook")
	}
	if _, err := orchclient.AddWebhook(ctx, v, orchapi.NewWebhookRequest{URL: "https://hooks.example.com/x", Project: "side"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("hidden project: %v", err)
	}
	res, err := orchclient.AddWebhook(ctx, v, orchapi.NewWebhookRequest{URL: "https://hooks.example.com/x", Project: "harbor/solver"})
	if err != nil {
		t.Fatal(err)
	}
	// Point it at the loopback receiver behind the creation check (as if
	// DNS said so; the creator's scope is what this test is about).
	if _, err := h.dm.Deps.Store.Registry().Exec(`UPDATE webhooks SET url = ?, allow_private = 1 WHERE id = ?`, rcv.srv.URL, res.Webhook.ID); err != nil {
		t.Fatal(err)
	}
	h.svc.Refresh = 20 * time.Millisecond

	// The owner's machine-wide hooks are invisible to vida.
	own := h.addHook(orchapi.NewWebhookRequest{URL: "https://hooks.example.com/owner"})
	if ws, err := orchclient.Webhooks(ctx, v, ""); err != nil || len(ws) != 1 || ws[0].ID != res.Webhook.ID {
		t.Fatalf("vida's list: %+v %v", ws, err)
	}
	if _, err := orchclient.Webhook(ctx, v, own.Webhook.ID, 0); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("owner's hook to vida: %v", err)
	}

	solver := h.work("harbor", "solver", "in solver")
	h.work("harbor", "", "project level")
	h.work("harbor", "billing", "in billing")
	h.work("side", "", "other project")
	// A session vida cannot see claims the solver item: the lease event
	// arrives with the holder redacted.
	var reg struct {
		Address string `json:"address"`
	}
	if err := h.at("harbor").Do(ctx, http.MethodPost, "/v0/sessions/register", map[string]string{"agent": "claude", "runtime_id": "bbbbbbbb-uuid", "title": "hidden2"}, &reg); err != nil {
		t.Fatal(err)
	}
	sc := h.at("harbor")
	sc.As = reg.Address
	if _, err := orchclient.Claim(ctx, sc, solver.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	// The claim publishes work.claimed, then work.status_changed (open ->
	// claimed): wait for both, or a late status_changed lands after the
	// revoke's count below and reads as a delivery after the revoke.
	eventually(t, "the solver events", func() bool {
		ts := strings.Join(rcv.types(), ",")
		return strings.Contains(ts, orchapi.EventWorkClaimed) && strings.Contains(ts, orchapi.EventWorkStatusChanged)
	})
	time.Sleep(100 * time.Millisecond)
	for _, g := range rcv.received() {
		if g.ev.Area != "solver" {
			t.Fatalf("vida's hook got an event outside solver: %s area %q", g.ev.Type, g.ev.Area)
		}
		if strings.Contains(string(g.body), reg.Address) {
			t.Fatalf("hidden session address leaked: %s", g.body)
		}
	}
	// Revoking vida's grant stops the hook at once.
	before := len(rcv.received())
	if _, err := h.owner.Revoke(ctx, api.GrantRequest{Identity: "vida", Role: "owner", Project: "harbor", Area: "solver"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	h.work("harbor", "solver", "after revoke")
	time.Sleep(300 * time.Millisecond)
	if n := len(rcv.received()); n != before {
		t.Fatalf("revoked creator's hook still delivers: %d -> %d (%v)", before, n, rcv.types()[before:])
	}
	// Granted again, with the token revoked first: still nothing. (Revoking
	// after the grant would race: the grant wakes the follower, which may
	// deliver the "after revoke" event from its cursor first, per spec.)
	if _, err := h.owner.RevokeToken(ctx, tok.Token.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.owner.Grant(ctx, api.GrantRequest{Identity: "vida", Role: "owner", Project: "harbor", Area: "solver"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	h.work("harbor", "solver", "after token revoke")
	time.Sleep(300 * time.Millisecond)
	if n := len(rcv.received()); n != before {
		t.Fatalf("revoked token's hook still delivers: %d -> %d", before, n)
	}
}

// A delivery queued while the creator could read it is skipped at send
// time once they cannot.
func TestQueuedDeliverySkippedAfterRevoke(t *testing.T) {
	h := newHX(t)
	ctx := context.Background()
	rcv := newReceiver(t)
	var up atomic.Bool
	rcv.status = func(int) int {
		if up.Load() {
			return 200
		}
		return 500
	}
	tok, _ := h.owner.MintToken(ctx, api.MintTokenRequest{As: "vida", Project: "harbor", Area: "solver", Role: "owner"})
	v := client.WithHandler(h.dm.Handler)
	v.Token = tok.Secret
	res, err := orchclient.AddWebhook(ctx, v, orchapi.NewWebhookRequest{URL: "https://hooks.example.com/x", Project: "harbor/solver"})
	if err != nil {
		t.Fatal(err)
	}
	h.dm.Deps.Store.Registry().Exec(`UPDATE webhooks SET url = ?, allow_private = 1 WHERE id = ?`, rcv.srv.URL, res.Webhook.ID)
	h.start()
	h.work("harbor", "solver", "queued")
	eventually(t, "a failed attempt", func() bool {
		d, _ := orchclient.Webhook(ctx, h.owner, res.Webhook.ID, 0)
		return len(d.Deliveries) > 0
	})
	h.owner.Revoke(ctx, api.GrantRequest{Identity: "vida", Role: "owner", Project: "harbor", Area: "solver"})
	up.Store(true)
	eventually(t, "skipped", func() bool {
		d, _ := orchclient.Webhook(ctx, h.owner, res.Webhook.ID, 0)
		return len(d.Deliveries) > 0 && d.Deliveries[0].Outcome == "skipped"
	})
	if len(rcv.received()) != 0 {
		t.Fatal("delivered to a revoked creator")
	}
}

func TestOwnerURLChecks(t *testing.T) {
	h := newHX(t)
	ctx := context.Background()
	for _, u := range []string{"http://hooks.example.com/x", "https://127.0.0.1/x", "https://169.254.169.254/latest", "https://[::1]/",
		"https://localhost/x", "https://metadata.google.internal/", "https://100.100.1.1/", "https://user:pw@hooks.example.com/",
		"ftp://hooks.example.com/", "https://10.1.2.3/", "https://[fd00:ec2::254]/", "https://mini.tail1234.ts.net/"} {
		if _, err := orchclient.AddWebhook(ctx, h.owner, orchapi.NewWebhookRequest{URL: u, Project: "harbor"}); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%s: %v", u, err)
		}
	}
	if _, err := orchclient.AddWebhook(ctx, h.owner, orchapi.NewWebhookRequest{URL: "https://hooks.example.com/x", Project: "harbor", Events: []string{"work.*;rm"}}); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("bad events: %v", err)
	}
	if _, err := orchclient.AddWebhook(ctx, h.owner, orchapi.NewWebhookRequest{URL: "https://hooks.example.com/x", Project: "harbor", Secret: "short"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("short secret: %v", err)
	}
	r := h.addHook(orchapi.NewWebhookRequest{URL: "https://hooks.example.com/x", Project: "harbor/solver", Events: []string{"work.*", "mail.sent", "dev.zm.session."}})
	if strings.Join(r.Webhook.Events, " ") != "dev.zm.work. dev.zm.mail.sent dev.zm.session." || r.Webhook.Area != "solver" || r.Webhook.Project != "harbor" {
		t.Fatalf("normalised: %+v", r.Webhook)
	}
	if _, err := orchclient.RemoveWebhook(ctx, h.owner, r.Webhook.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Webhook(ctx, h.owner, r.Webhook.ID, 0); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("after rm: %v", err)
	}
}

// Catch-up pages past events the creator may not receive (the bus filters
// audience after its SQL limit): a page that is all filtered is not the
// end of the log.
func TestCatchUpPastFilteredEvents(t *testing.T) {
	h := newHX(t)
	ctx := context.Background()
	rcv := newReceiver(t)
	tok, _ := h.owner.MintToken(ctx, api.MintTokenRequest{As: "vida", Project: "harbor", Area: "solver", Role: "owner"})
	v := client.WithHandler(h.dm.Handler)
	v.Token = tok.Secret
	res, err := orchclient.AddWebhook(ctx, v, orchapi.NewWebhookRequest{URL: "https://hooks.example.com/x", Project: "harbor/solver"})
	if err != nil {
		t.Fatal(err)
	}
	h.dm.Deps.Store.Registry().Exec(`UPDATE webhooks SET url = ?, allow_private = 1 WHERE id = ?`, rcv.srv.URL, res.Webhook.ID)
	// vida is now only a guest of solver: operate-audience events are not theirs.
	h.owner.Revoke(ctx, api.GrantRequest{Identity: "vida", Role: "owner", Project: "harbor", Area: "solver"})
	if _, err := h.owner.Grant(ctx, api.GrantRequest{Identity: "vida", Role: "guest", Project: "harbor", Area: "solver"}); err != nil {
		t.Fatal(err)
	}
	p, _ := h.owner.GetProject(ctx, "harbor")
	for i := 0; i < 700; i++ {
		if _, err := h.dm.Deps.Events.Publish(ctx, events.Input{Type: "dev.zm.test.restricted", Subject: "r", Project: p.ID, Area: "solver",
			Data: i, Audience: &events.Audience{Need: "operate"}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.dm.Deps.Events.Publish(ctx, events.Input{Type: "dev.zm.test.open", Subject: "o", Project: p.ID, Area: "solver", Data: "visible"}); err != nil {
		t.Fatal(err)
	}
	h.start()
	eventually(t, "the visible event behind 700 filtered ones", func() bool {
		for _, g := range rcv.received() {
			if g.ev.Type == "dev.zm.test.open" {
				return true
			}
		}
		return false
	})
	for _, g := range rcv.received() {
		if g.ev.Type == "dev.zm.test.restricted" {
			t.Fatal("an operate-audience event reached a guest creator's hook")
		}
	}
}

// The secret ledger is not a webhook, including for a hook that asks for
// every event. A page of those events is not the end of the log: the cursor
// moves past them and a later event is still delivered. A live one is not
// posted either, and neither is stored in the queue.
func TestSecretLedgerNeverPosted(t *testing.T) {
	h := newHX(t)
	ctx := context.Background()
	rcv := newReceiver(t)
	h.addHook(orchapi.NewWebhookRequest{URL: rcv.srv.URL + "/hook", Project: "harbor", Events: []string{"*"}, AllowPrivate: true})
	p, err := h.owner.GetProject(ctx, "harbor")
	if err != nil {
		t.Fatal(err)
	}
	const name = "WEBHOOK-SECRET-NAME"
	publish := func(typ string, data any) {
		t.Helper()
		if _, err := h.dm.Deps.Events.Publish(ctx, events.Input{Type: typ, Subject: "s", Project: p.ID, Area: "solver", Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 501; i++ {
		publish("dev.zm.secret.granted", map[string]any{"names": []string{name}, "n": i})
	}
	publish("dev.zm.secret_grant.added", map[string]any{"name": name})
	behind := h.work("harbor", "solver", "after a page of secret events")
	h.start()
	eventually(t, "the work event behind a page of secret events", func() bool {
		for _, g := range rcv.received() {
			if g.ev.Subject == behind.ID {
				return true
			}
		}
		return false
	})
	publish("dev.zm.secret.granted", map[string]any{"names": []string{name}, "live": true})
	live := h.work("harbor", "solver", "after a live secret event")
	eventually(t, "the work event after a live secret event", func() bool {
		for _, g := range rcv.received() {
			if g.ev.Subject == live.ID {
				return true
			}
		}
		return false
	})
	time.Sleep(100 * time.Millisecond)
	for _, g := range rcv.received() {
		if strings.Contains(g.ev.Type, "secret") || strings.Contains(string(g.body), name) {
			t.Fatalf("a secret event was posted: %s %s", g.ev.Type, g.body)
		}
	}
	var n int
	if err := h.dm.Deps.Store.Registry().QueryRow(`SELECT count(*) FROM webhook_queue WHERE event_type LIKE 'dev.zm.secret%'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("secret events queued: %d %v", n, err)
	}
}

// An owner's webhook follows the bus as the daemon (scope all, via system),
// not as the owner in person: owner-only keys (a shell's terminal title, a
// session's foreground process and ports) never reach its URL.
func TestOwnerOnlyKeysNeverPosted(t *testing.T) {
	h := newHX(t)
	h.start()
	rcv := newReceiver(t)
	h.addHook(orchapi.NewWebhookRequest{URL: rcv.srv.URL + "/hook", Project: "harbor", Events: []string{"session.*"}, AllowPrivate: true})
	p, err := h.owner.GetProject(context.Background(), "harbor")
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"address": "mini/abcd1234", "state": "running", "term_title": "ssh prod-db-01",
		"fg": map[string]any{"pid": 42, "name": "vim"}, "busy": true, "ports": []int{3000}}
	if _, err := h.dm.Deps.Events.Publish(context.Background(), events.Input{Type: orchapi.EventSessionUpdated, Subject: "mini/abcd1234",
		Project: p.ID, Data: data, OwnerOnly: []string{"term_title", "fg", "busy", "ports"}}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the session.updated delivery", func() bool { return len(rcv.received()) >= 1 })
	b := string(rcv.received()[0].body)
	for _, k := range []string{"term_title", "prod-db-01", `"fg"`, `"busy"`, `"ports"`} {
		if strings.Contains(b, k) {
			t.Fatalf("the webhook got %s: %s", k, b)
		}
	}
	if !strings.Contains(b, `"state":"running"`) {
		t.Fatalf("payload lost the rest: %s", b)
	}
}

// TestRestoredHookWithoutSecretNeverSends: the module's backup
// scrubber blanks every hook's secret (as in a staged off-site copy), and a
// hook restored that way never posts an unsigned delivery: it says why.
func TestRestoredHookWithoutSecretNeverSends(t *testing.T) {
	h := newHX(t)
	rcv := newReceiver(t)
	res := h.addHook(orchapi.NewWebhookRequest{URL: rcv.srv.URL + "/hook", Project: "harbor", Events: []string{"work.*"}, AllowPrivate: true})
	ctx := context.Background()
	reg := h.dm.Deps.Store.Registry()
	tx, err := reg.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range h.dm.Deps.BackupScrubbers() {
		if err := f(ctx, tx); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var secret string
	if err := reg.QueryRowContext(ctx, `SELECT secret FROM webhooks WHERE id = ?`, res.Webhook.ID).Scan(&secret); err != nil || secret != "" {
		t.Fatalf("after the backup scrubbers: %q %v", secret, err)
	}
	h.start()
	h.work("harbor", "solver", "after the restore")
	eventually(t, "the refusal recorded", func() bool {
		d, err := orchclient.Webhook(ctx, h.owner, res.Webhook.ID, 5)
		return err == nil && strings.Contains(d.Webhook.LastError, "no signing secret")
	})
	if n := len(rcv.received()); n != 0 {
		t.Fatalf("%d unsigned deliveries sent", n)
	}
}
