package team_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/mezze/internal/door"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// The Mezze app door end to end (SPEC 18.4): a member machine's Mezze app registers a
// task through its own socket (sent on to the authority); a delivery to the
// task's public URL at the authority is queued there and reaches the
// member's Mezze app by a door_delivery machine op, its signature header intact
// and the edge's headers gone; a rotated URL stops at once.
func TestDoorDeliversToTheOwningMachine(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard,
		func(c *config.Config) {
			roomy(c)
			c.File.Door.Listen = "127.0.0.1:0"
			c.File.Door.PublicURL = "https://zm.example"
		})
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	mac := startDaemonWith(t, tn, "mac", tailnettest.Identity{NodeID: "nMAC", NodeKey: "nodekey:mac", Login: "bea@example.com", Host: "mac"}, discard, roomy)
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	mustf(mac.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	heartbeat(t, mac)

	// The mac's Mezze app: one webhook task's hook URL on loopback.
	type got struct {
		path, sig, cookie, cf, body string
	}
	var mu sync.Mutex
	var hits []got
	z3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits = append(hits, got{r.URL.Path, r.Header.Get("X-Hub-Signature-256"), r.Header.Get("Cookie"), r.Header.Get("Cf-Connecting-Ip"), string(b)})
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer z3.Close()

	st := mustf(orchclient.SetDoor(ctx, mac.owner, "harbor", true))(t)
	if !st.On || st.Machine != "mac" || !st.Available || st.BaseURL != "https://zm.example" {
		t.Fatalf("door status from the mac: %+v", st)
	}
	if _, err := orchclient.PutDoorTask(ctx, mac.owner, "harbor", "red", orchapi.DoorTaskRequest{Target: "http://10.0.0.5/api/hooks/red/x"}); err == nil {
		t.Fatal("a target off the machine was accepted")
	}
	task := mustf(orchclient.PutDoorTask(ctx, mac.owner, "harbor", "red", orchapi.DoorTaskRequest{Target: z3.URL + "/api/hooks/red/tok", Label: "main red"}))(t)
	if task.Machine != "mac" || !strings.HasPrefix(task.URL, "https://zm.example/v0/door/hook/dh_") {
		t.Fatalf("task: %+v", task)
	}
	if ts := mustf(orchclient.DoorTasks(ctx, a.owner, "harbor"))(t); len(ts) != 0 {
		t.Fatalf("the authority's own machine sees the mac's task: %+v", ts)
	}

	var ds *door.Service
	for _, m := range a.dm.Modules() {
		if s, ok := m.(*door.Service); ok {
			ds = s
		}
	}
	if ds == nil {
		t.Fatal("no door module")
	}
	pub := httptest.NewServer(ds.PublicHandler())
	defer pub.Close()
	post := func(u, delivery string) int {
		req, _ := http.NewRequest(http.MethodPost, pub.URL+strings.TrimPrefix(u, "https://zm.example"), strings.NewReader(`{"action":"completed"}`))
		req.Header.Set("X-GitHub-Delivery", delivery)
		req.Header.Set("X-GitHub-Event", "workflow_run")
		req.Header.Set("X-Hub-Signature-256", "sha256=abc")
		req.Header.Set("Cookie", "edge=1")
		req.Header.Set("Cf-Connecting-Ip", "203.0.113.9")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post(task.URL, "d-1"); c != http.StatusAccepted {
		t.Fatalf("hook: %d", c)
	}
	if c := post(task.URL, "d-1"); c != http.StatusAccepted {
		t.Fatalf("repeated hook: %d", c)
	}
	waitFor(t, "the delivery at the mac's Mezze app", func() bool {
		heartbeat(t, mac) // carries the op (no ops poll: the mac lends no boxes)
		mu.Lock()
		defer mu.Unlock()
		return len(hits) > 0
	})
	waitFor(t, "the delivery recorded", func() bool {
		dl, err := orchclient.DoorDeliveries(ctx, mac.owner, "harbor", "red", 0)
		return err == nil && len(dl) == 1 && dl[0].State == orchapi.DoorDelivered
	})
	mu.Lock()
	h := hits[0]
	n := len(hits)
	mu.Unlock()
	if n != 1 || h.path != "/api/hooks/red/tok" || h.sig != "sha256=abc" || h.cookie != "" || h.cf != "" || h.body != `{"action":"completed"}` {
		t.Fatalf("at the Mezze app (%d): %+v", n, h)
	}
	dl := mustf(orchclient.DoorDeliveries(ctx, mac.owner, "harbor", "red", 0))(t)
	if dl[0].Status != http.StatusAccepted || dl[0].Delivery != "d-1" || dl[0].Event != "workflow_run" {
		t.Fatalf("deliveries: %+v", dl)
	}

	rot := mustf(orchclient.RotateDoorTask(ctx, mac.owner, "harbor", "red"))(t)
	if rot.URL == task.URL || rot.URL == "" {
		t.Fatalf("rotate: %+v", rot)
	}
	if c := post(task.URL, "d-2"); c != http.StatusNotFound {
		t.Fatalf("old URL after rotate: %d", c)
	}
	mustf(orchclient.SetDoor(ctx, mac.owner, "harbor", false))(t)
	if c := post(rot.URL, "d-3"); c != http.StatusNotFound {
		t.Fatalf("URL with the door off: %d", c)
	}
}
