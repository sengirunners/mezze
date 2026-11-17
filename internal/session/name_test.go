package session

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

func wantErrCode(t *testing.T, err error, code string) *api.Error {
	t.Helper()
	e, ok := err.(*api.Error)
	if !ok || e.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
	return e
}

// SPEC 8 "Names": a name reaches a live session by every session route,
// is unique among this machine's live sessions, can be changed and
// cleared, and stops resolving when its session ends.
func TestSessionNames(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	a := h.spawn("shell", "exec sleep 30", func(r *orchapi.SpawnRequest) { r.Name = "Auth-Fix"; r.Title = "fix the login bug" })
	if a.Name != "auth-fix" || a.Title != "fix the login bug" {
		t.Fatalf("spawned %+v", a)
	}
	for _, id := range []string{"auth-fix", "mini/auth-fix", " MINI/Auth-Fix "} {
		if got, err := orchclient.Session(ctx, h.c, id); err != nil || got.Address != a.Address {
			t.Fatalf("%q: %+v %v", id, got, err)
		}
	}
	if _, err := orchclient.Session(ctx, h.c, "other/auth-fix"); err == nil {
		t.Fatal("another machine's name resolved here")
	}
	// Taken: refused, naming the holder to the owner.
	_, err := orchclient.Spawn(ctx, h.c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 30"}, Cwd: h.home, Name: "auth-fix"})
	if e := wantErrCode(t, err, api.CodeConflict); e.Message != "the name auth-fix is taken by "+a.Address {
		t.Fatalf("taken: %+v", e)
	}
	b := h.spawn("shell", "exec sleep 30", nil)
	if _, err := orchclient.SetSessionName(ctx, h.c, b.ID, "auth-fix"); err == nil {
		t.Fatal("renamed onto a live session's name")
	}
	for _, bad := range []string{"deadbeef", "4bb7545a", "frontend", "analysis", "all", "fix bug", "-x", "ass", "ok.kill"} {
		if _, err := orchclient.SetSessionName(ctx, h.c, b.ID, bad); err == nil {
			t.Fatalf("name %q accepted", bad)
		} else {
			wantErrCode(t, err, api.CodeBadRequest)
		}
	}
	for _, good := range []string{"class-review", "analytics", "front-end", "reviewer2"} {
		if got, err := orchclient.SetSessionName(ctx, h.c, b.ID, good); err != nil || got.Name != good {
			t.Fatalf("name %q: %+v %v", good, got, err)
		}
	}

	// A runtime id that is no UUID gives a session8 of letters ("senderse"):
	// that string is an address, never a name, so nothing can take it.
	o, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "opencode", RuntimeID: "senderse-1", Cwd: h.home})
	if err != nil || o.ID != "senderse" {
		t.Fatalf("register: %+v %v", o, err)
	}
	if _, err := orchclient.SetSessionName(ctx, h.c, b.ID, "senderse"); err == nil {
		t.Fatal("named after a session id")
	} else {
		wantErrCode(t, err, api.CodeBadRequest)
	}
	if got, err := orchclient.Session(ctx, h.c, "mini/senderse"); err != nil || got.Address != o.Address {
		t.Fatalf("mini/senderse: %+v %v", got, err)
	}

	// Renaming frees the old name; clearing frees the new one.
	if got, err := orchclient.SetSessionName(ctx, h.c, "auth-fix", "login"); err != nil || got.Name != "login" {
		t.Fatalf("rename: %+v %v", got, err)
	}
	if _, err := orchclient.Session(ctx, h.c, "auth-fix"); err == nil {
		t.Fatal("the old name still resolves")
	}
	if got, err := orchclient.SetSessionName(ctx, h.c, b.ID, "auth-fix"); err != nil || got.Name != "auth-fix" {
		t.Fatalf("freed name: %+v %v", got, err)
	}
	if got, err := orchclient.SetSessionName(ctx, h.c, "login", ""); err != nil || got.Name != "" {
		t.Fatalf("clear: %+v %v", got, err)
	}
	if l, _ := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{}); len(l) != 3 {
		t.Fatalf("list %+v", l)
	}

	// An ended session keeps its name, which no longer resolves and is free.
	u, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-name", Cwd: h.home})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.SetSessionName(ctx, h.c, u.ID, "hooked"); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-name", End: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := orchclient.Session(ctx, h.c, u.ID); err != nil || got.State != orchapi.StateEnded || got.Name != "hooked" {
		t.Fatalf("ended: %+v %v", got, err)
	}
	if _, err := orchclient.Session(ctx, h.c, "hooked"); err == nil {
		t.Fatal("an ended session's name resolved")
	}
	if _, err := orchclient.SetSessionName(ctx, h.c, u.ID, "other"); err == nil {
		t.Fatal("named an ended session")
	}
	if _, err := orchclient.SetSessionName(ctx, h.c, a.ID, "hooked"); err != nil {
		t.Fatalf("an ended session's name is free: %v", err)
	}
	// Revived by its hooks, it gives the name up rather than take it back.
	r, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rt-name", State: orchapi.StateRunning})
	if err != nil || r.Address != u.Address || r.State != orchapi.StateRunning || r.Name != "" {
		t.Fatalf("revived: %+v %v", r, err)
	}
	if got, err := orchclient.Session(ctx, h.c, "hooked"); err != nil || got.Address != a.Address {
		t.Fatalf("the name moved back: %+v %v", got, err)
	}
}

func TestValidSessionName(t *testing.T) {
	for s, want := range map[string]bool{"auth-fix": true, "a": true, "frontend": false, "deadbeef": false, "senderse": false, "front-end": true, "none": false,
		"owners": false, "Auth": false, "fix_2.b": true, "x/y": false, "sex": false, "sussex": true, "kill-switch": false} {
		if got := api.ValidSessionName(s); got != want {
			t.Errorf("%q: %v", s, got)
		}
	}
	if m, n, ok := api.SessionNameRef(" Mini/Auth-Fix "); !ok || m != "mini" || n != "auth-fix" {
		t.Errorf("%q %q %v", m, n, ok)
	}
	if _, _, ok := api.SessionNameRef("mini/4bb7545a"); ok {
		t.Error("a session address is no name")
	}
}

// No name hides a literal route under /v0/sessions/ (GET /v0/sessions/gc
// would never reach a session named gc).
func TestSessionNamesSkipRoutes(t *testing.T) {
	r := core.NewRouter()
	newHarness(t, nil).s.Routes(r)
	n := 0
	for _, rt := range r.Routes() {
		_, path, _ := strings.Cut(rt.Pattern, " ")
		rest, ok := strings.CutPrefix(path, "/v0/sessions/")
		if !ok || strings.ContainsAny(rest, "{/") {
			continue
		}
		n++
		if api.ValidSessionName(rest) {
			t.Errorf("a session may be named %q, which /v0/sessions/%s hides", rest, rest)
		}
	}
	if n == 0 {
		t.Fatal("no literal session routes found")
	}
}
