package door

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// fakeRelay answers each delivery with the next scripted outcome.
type fakeRelay struct {
	mu   sync.Mutex
	ops  []orchapi.DoorDeliveryOp
	next func(op orchapi.DoorDeliveryOp) (orchapi.DoorDeliveryResult, error)
}

func (f *fakeRelay) RelayDoor(_ context.Context, _, _ string, op orchapi.DoorDeliveryOp, _ time.Duration) (orchapi.DoorDeliveryResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ops = append(f.ops, op)
	return f.next(op)
}

type fix struct {
	s            *Service
	owner, guest *authz.Subject
	p, solo      api.Project
	relay        *fakeRelay
	now          time.Time
}

func newFix(t *testing.T) *fix {
	t.Helper()
	ctx := context.Background()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	st, err := storetest.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{Home: home, Human: "ivo", Machine: "home"}
	cfg.File.Door = config.Door{Listen: "127.0.0.1:7437", PublicURL: "https://zm.example/"}
	reg, err := project.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	owner := reg.Local("")
	p, err := reg.Create(ctx, owner, api.NewProjectRequest{Slug: "zm"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetTeam(ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	solo, err := reg.Create(ctx, owner, api.NewProjectRequest{Slug: "solo"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := reg.Mint(ctx, owner, api.MintTokenRequest{As: "guest", Project: p.ID, Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	guest, err := reg.FromToken(ctx, tok.Secret, false)
	if err != nil {
		t.Fatal(err)
	}
	f := &fix{owner: owner, guest: guest, p: p, solo: solo, now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	f.relay = &fakeRelay{next: func(orchapi.DoorDeliveryOp) (orchapi.DoorDeliveryResult, error) {
		return orchapi.DoorDeliveryResult{Status: 202}, nil
	}}
	f.s = New(&core.Deps{Store: st, Projects: reg, Config: cfg})
	f.s.Relay = f.relay
	f.s.Now = func() time.Time { return f.now }
	f.s.ctx = ctx
	return f
}

func (f *fix) call(t *testing.T, sub *authz.Subject, method, path string, body any, out any) int {
	t.Helper()
	r := core.NewRouter()
	f.s.Routes(r)
	var b []byte
	if body != nil {
		b, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
	req = req.WithContext(core.WithSubject(req.Context(), sub))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if out != nil && w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
			t.Fatal(err)
		}
	}
	return w.Code
}

func (f *fix) hook(t *testing.T, url string, delivery string, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, strings.TrimPrefix(url, "https://zm.example"), strings.NewReader(body))
	if delivery != "" {
		req.Header.Set("Webhook-Id", delivery)
	}
	w := httptest.NewRecorder()
	f.s.PublicHandler().ServeHTTP(w, req)
	return w.Code
}

// drain runs the worker once and waits for what it started.
func (f *fix) drain() {
	f.s.expire(context.Background())
	f.s.dispatch(context.Background())
	f.s.wg.Wait()
}

func (f *fix) task(t *testing.T, sub *authz.Subject, name string) orchapi.DoorTask {
	t.Helper()
	var task orchapi.DoorTask
	if c := f.call(t, sub, "PUT", "/v0/door/tasks/"+name+"?project=zm", orchapi.DoorTaskRequest{Target: "http://127.0.0.1:3773/api/hooks/" + name + "/t"}, &task); c != 200 {
		t.Fatalf("put task %s: %d", name, c)
	}
	return task
}

func (f *fix) deliveries(t *testing.T, task string) []orchapi.DoorDelivery {
	t.Helper()
	var l orchapi.DoorDeliveryList
	if c := f.call(t, f.owner, "GET", "/v0/door/tasks/"+task+"/deliveries?project=zm", nil, &l); c != 200 {
		t.Fatalf("deliveries: %d", c)
	}
	return l.Deliveries
}

// A machine offline holds its queue in order; the Mezze app's refusal ends a delivery
// without a retry; one queued past 72 h is dropped.
func TestDoorQueueOrderRetryRefuseDrop(t *testing.T) {
	f := newFix(t)
	if c := f.call(t, f.owner, "PUT", "/v0/door?project=zm", orchapi.DoorToggle{On: true}, nil); c != 200 {
		t.Fatal(c)
	}
	task := f.task(t, f.owner, "red")
	if task.URL == "" || !strings.HasPrefix(task.URL, "https://zm.example/v0/door/hook/dh_") {
		t.Fatalf("url: %q", task.URL)
	}
	offline := true
	f.relay.next = func(op orchapi.DoorDeliveryOp) (orchapi.DoorDeliveryResult, error) {
		if offline {
			return orchapi.DoorDeliveryResult{}, api.Errorf(api.CodeUnavailable, "home has not been heard from in 60s")
		}
		if string(op.Body) == "bad" {
			return orchapi.DoorDeliveryResult{Status: 401}, nil
		}
		return orchapi.DoorDeliveryResult{Status: 202}, nil
	}
	for _, b := range []string{"one", "bad", "three"} {
		if c := f.hook(t, task.URL, "id-"+b, b); c != http.StatusAccepted {
			t.Fatalf("hook %s: %d", b, c)
		}
	}
	f.drain()
	if len(f.relay.ops) != 1 || string(f.relay.ops[0].Body) != "one" || f.relay.ops[0].Target != "http://127.0.0.1:3773/api/hooks/red/t" {
		t.Fatalf("offline: only the head goes: %+v", f.relay.ops)
	}
	f.drain() // backing off: nothing goes
	if len(f.relay.ops) != 1 {
		t.Fatalf("retried before its backoff: %d", len(f.relay.ops))
	}
	ds := f.deliveries(t, "red")
	if len(ds) != 3 || ds[2].State != orchapi.DoorQueued || ds[2].Attempts != 1 || !strings.Contains(ds[2].Error, "not been heard") || ds[2].NextTryAt == 0 {
		t.Fatalf("after one failure: %+v", ds)
	}
	offline = false
	f.now = f.now.Add(time.Minute)
	for range 3 {
		f.drain()
	}
	var bodies []string
	for _, op := range f.relay.ops {
		bodies = append(bodies, string(op.Body))
	}
	if strings.Join(bodies, ",") != "one,one,bad,three" {
		t.Fatalf("order: %v", bodies)
	}
	ds = f.deliveries(t, "red")
	if ds[0].State != orchapi.DoorDelivered || ds[1].State != orchapi.DoorRefused || ds[1].Status != 401 || ds[2].State != orchapi.DoorDelivered || ds[2].Attempts != 2 {
		t.Fatalf("outcomes: %+v", ds)
	}
	var left int
	_ = f.s.d.Store.Registry().QueryRow(`SELECT COUNT(*) FROM door_deliveries WHERE length(body) > 0 OR headers != ''`).Scan(&left)
	if left != 0 {
		t.Fatalf("%d ended deliveries keep their copy", left)
	}

	// Dropped after 72 h queued.
	offline = true
	f.hook(t, task.URL, "id-late", "late")
	f.now = f.now.Add(73 * time.Hour)
	f.drain()
	if ds = f.deliveries(t, "red"); ds[0].State != orchapi.DoorDropped {
		t.Fatalf("72 h: %+v", ds[0])
	}
}

// The token is the credential: unknown, rotated, switched off, oversize and
// repeated deliveries are handled before anything is queued.
func TestDoorHookRefusals(t *testing.T) {
	f := newFix(t)
	task := f.task(t, f.owner, "red")
	if task.URL != "" {
		t.Fatalf("a URL while the door is off: %q", task.URL)
	}
	var tasks orchapi.DoorTaskList
	f.call(t, f.owner, "GET", "/v0/door/tasks?project=zm", nil, &tasks)
	f.call(t, f.owner, "PUT", "/v0/door?project=zm", orchapi.DoorToggle{On: true}, nil)
	f.call(t, f.owner, "GET", "/v0/door/tasks?project=zm", nil, &tasks)
	if len(tasks.Tasks) != 1 || tasks.Tasks[0].URL == "" {
		t.Fatalf("tasks: %+v", tasks)
	}
	u := tasks.Tasks[0].URL
	if c := f.hook(t, "/v0/door/hook/dh_nope", "", "x"); c != 404 {
		t.Fatalf("malformed token: %d", c)
	}
	if c := f.hook(t, "/v0/door/hook/"+newToken(), "", "x"); c != 404 {
		t.Fatalf("unknown token: %d", c)
	}
	if c := f.hook(t, u, "", strings.Repeat("x", maxBody+1)); c != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: %d", c)
	}
	f.hook(t, u, "same", "a")
	f.hook(t, u, "same", "a")
	if ds := f.deliveries(t, "red"); len(ds) != 1 {
		t.Fatalf("a repeated delivery id queued twice: %d", len(ds))
	}
	var rot orchapi.DoorTask
	f.call(t, f.owner, "POST", "/v0/door/tasks/red/rotate?project=zm", nil, &rot)
	if rot.URL == u || f.hook(t, u, "", "x") != 404 || f.hook(t, rot.URL, "", "x") != http.StatusAccepted {
		t.Fatal("rotate did not replace the URL")
	}
	var st orchapi.DoorStatus
	f.call(t, f.owner, "PUT", "/v0/door?project=zm", orchapi.DoorToggle{On: false}, &st)
	if st.On || f.hook(t, rot.URL, "", "x") != 404 {
		t.Fatal("the door still answers when off")
	}
	for _, d := range f.deliveries(t, "red") {
		if d.State != orchapi.DoorDropped {
			t.Fatalf("turning the door off left %+v queued", d)
		}
	}
}

// Only a machine's own daemon sets the door up, for its own machine, in a
// team project it may operate in; another machine's tasks look missing.
func TestDoorCallerScope(t *testing.T) {
	f := newFix(t)
	if c := f.call(t, f.guest, "GET", "/v0/door?project=zm", nil, nil); c != http.StatusForbidden {
		t.Fatalf("a token that is no machine's daemon: %d", c)
	}
	mac := *f.owner
	mac.Node, mac.Machine = "nMAC", "mac"
	guestNode := *f.guest
	guestNode.Node, guestNode.Machine = "nG", "g"
	if c := f.call(t, &guestNode, "PUT", "/v0/door/tasks/x?project=zm", orchapi.DoorTaskRequest{Target: "http://127.0.0.1/api/hooks/x"}, nil); c != http.StatusNotFound {
		t.Fatalf("a guest machine registers a task: %d", c)
	}
	if c := f.call(t, f.owner, "GET", "/v0/door?project=solo", nil, nil); c != http.StatusBadRequest {
		t.Fatalf("a solo project: %d", c)
	}
	if c := f.call(t, f.owner, "GET", "/v0/door", nil, nil); c != http.StatusBadRequest {
		t.Fatalf("no project: %d", c)
	}
	for _, bad := range []string{"https://127.0.0.1/api/hooks/x", "http://example.com/api/hooks/x", "http://127.0.0.1/other", "http://u:p@127.0.0.1/api/hooks/x", "http://127.0.0.1/api/hooks/../admin"} {
		if c := f.call(t, f.owner, "PUT", "/v0/door/tasks/x?project=zm", orchapi.DoorTaskRequest{Target: bad}, nil); c != http.StatusBadRequest {
			t.Errorf("target %s: %d", bad, c)
		}
	}
	if c := f.call(t, f.owner, "PUT", "/v0/door/tasks/a%20b?project=zm", orchapi.DoorTaskRequest{Target: "http://127.0.0.1/api/hooks/x"}, nil); c != http.StatusBadRequest {
		t.Fatalf("bad task id: %d", c)
	}
	f.task(t, &mac, "mac-task")
	var l orchapi.DoorTaskList
	f.call(t, f.owner, "GET", "/v0/door/tasks?project=zm", nil, &l)
	if len(l.Tasks) != 0 {
		t.Fatalf("home sees the mac's task: %+v", l)
	}
	for _, c := range []int{
		f.call(t, f.owner, "POST", "/v0/door/tasks/mac-task/rotate?project=zm", nil, nil),
		f.call(t, f.owner, "DELETE", "/v0/door/tasks/mac-task?project=zm", nil, nil),
		f.call(t, f.owner, "GET", "/v0/door/tasks/mac-task/deliveries?project=zm", nil, nil),
	} {
		if c != http.StatusNotFound {
			t.Fatalf("another machine's task: %d", c)
		}
	}
	f.call(t, &mac, "GET", "/v0/door/tasks?project=zm", nil, &l)
	if len(l.Tasks) != 1 || l.Tasks[0].Machine != "mac" {
		t.Fatalf("the mac's own list: %+v", l)
	}
	if c := f.call(t, &mac, "DELETE", "/v0/door/tasks/mac-task?project=zm", nil, nil); c != 200 {
		t.Fatalf("remove: %d", c)
	}
}

// Headers: the edge's and zm's stay at the door, the sender's go on; the
// delivery id is read from the known headers only when well formed.
func TestDoorHeaders(t *testing.T) {
	h := http.Header{}
	for _, k := range []string{"Host", "Cookie", "Cf-Connecting-Ip", "X-Forwarded-For", "Zm-As", "X-Zm-Edge-Secret", "Connection", "X-Hub-Signature-256", "Sentry-Hook-Signature", "Content-Type", "Authorization"} {
		h.Set(k, "v")
	}
	kept, ok := keepHeaders(h)
	if !ok {
		t.Fatal("within limits")
	}
	var names []string
	for k := range kept {
		names = append(names, k)
	}
	if len(kept) != 4 || kept["X-Hub-Signature-256"] == nil || kept["Sentry-Hook-Signature"] == nil || kept["Content-Type"] == nil || kept["Authorization"] == nil {
		t.Fatalf("kept: %v", names)
	}
	big := http.Header{}
	for i := range maxHeaders + 1 {
		big.Set("X-H"+strings.Repeat("a", i), "v")
	}
	if _, ok := keepHeaders(big); ok {
		t.Fatal("over the header count")
	}
	if deliveryID(http.Header{"X-Github-Delivery": {"72d3162e-cc78-11e3-81ab-4c9367dc0958"}}) == "" ||
		deliveryID(http.Header{"Request-Id": {"bad id\n"}}) != "" || deliveryID(http.Header{"Webhook-Id": {strings.Repeat("a", 101)}}) != "" {
		t.Fatal("delivery ids")
	}
}
