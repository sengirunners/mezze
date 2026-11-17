package main

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
)

// TestAsksResult: an owned project with no active approver warns with the
// pairing command; an active approver (device or key) passes; a pending or
// unconfirmed one does not count; no owned project is nothing to say.
func TestAsksResult(t *testing.T) {
	active := orchapi.Approver{Name: "a", State: orchapi.ApproverActive}
	key := orchapi.Approver{Name: "k", State: orchapi.ApproverActive, Key: "p256:0123456789abcdef"}
	pending := orchapi.Approver{Name: "p", State: orchapi.ApproverPending}
	unconf := orchapi.Approver{Name: "u", State: orchapi.ApproverUnconfirmed}
	cases := []struct {
		name   string
		owned  []string
		as     []orchapi.Approver
		status string
		detail string
	}{
		{"none owned", nil, nil, doctorOK, "no project"},
		{"none owned, approver", nil, []orchapi.Approver{active}, doctorOK, "no project"},
		{"no approver", []string{"web"}, nil, doctorWarn, "project web"},
		{"pending only", []string{"web"}, []orchapi.Approver{pending}, doctorWarn, "1 pairing(s) not finished"},
		{"unconfirmed only", []string{"web"}, []orchapi.Approver{unconf}, doctorWarn, "1 pairing(s) not finished"},
		{"device", []string{"web"}, []orchapi.Approver{active}, doctorOK, "1 paired"},
		{"key approver", []string{"web", "api"}, []orchapi.Approver{pending, key}, doctorOK, "projects web, api"},
		{"many", []string{"a", "b", "c", "d", "e"}, nil, doctorWarn, "projects a, b, c and 2 more"},
	}
	for _, c := range cases {
		r := asksResult(c.owned, c.as)
		if r.Status != c.status || !strings.Contains(r.Detail, c.detail) {
			t.Errorf("%s: %+v, want %s containing %q", c.name, r, c.status, c.detail)
		}
		if r.Status == doctorWarn && !strings.Contains(r.Fix, "zm approver add") {
			t.Errorf("%s: fix %q does not name the pairing command", c.name, r.Fix)
		}
		if r.Status == doctorOK && r.Fix != "" {
			t.Errorf("%s: ok result has a fix: %+v", c.name, r)
		}
	}
}

// TestOwnedHere: only owner-role projects count, and a joined project is
// dropped by its id, never its slug (a local project may reuse the slug).
func TestOwnedHere(t *testing.T) {
	projects := []api.Project{
		{ID: "01A", Slug: "mine", Role: api.RoleOwner},
		{ID: "01B", Slug: "team", Role: api.RoleOwner},
		{ID: "01C", Slug: "guest", Role: "member"},
		{ID: "01D", Slug: "shared", Role: api.RoleOwner},
	}
	remotes := []api.Remote{
		{Project: api.ProjectRef{ID: "01B", Slug: "team"}},
		{Project: api.ProjectRef{ID: "01Z", Slug: "shared"}}, // same slug as a local project, another id
	}
	got := ownedHere(projects, remotes)
	if strings.Join(got, ",") != "mine,shared" {
		t.Errorf("owned = %v, want [mine shared]", got)
	}
}

// TestDoctorAsksSkips: with no home, a remote daemon or no daemon the check
// skips, and says why.
func TestDoctorAsksSkips(t *testing.T) {
	ctx := context.Background()
	for name, env := range map[string]*doctorEnv{
		"no config": {},
		"remote":    {Config: &config.Config{Home: t.TempDir()}, Remote: true},
		"no daemon": {Config: &config.Config{Home: t.TempDir()}},
	} {
		if r := doctorAsks(ctx, env); r.Status != doctorSkip || r.Detail == "" {
			t.Errorf("%s: %+v, want skip", name, r)
		}
	}
}

// TestDoctorAsksLive: against an in-process daemon, a fresh home has nothing
// owned (ok); an owned project with no approver warns; a reserved (pending)
// approver still warns. The client's own project and address are put back.
func TestDoctorAsksLive(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := client.WithHandler(dm.SocketHandler())
	env := &doctorEnv{Config: cfg, Client: c}

	if r := doctorAsks(ctx, env); r.Status != doctorOK {
		t.Fatalf("fresh home: %+v", r)
	}
	if _, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: "web"}); err != nil {
		t.Fatal(err)
	}
	c.Project, c.ProjectFrom, c.As, c.Cwd = "elsewhere", "flag", "human:ivo", "/nowhere"
	r := doctorAsks(ctx, env)
	if r.Status != doctorWarn || !strings.Contains(r.Detail, "web") || !strings.Contains(r.Fix, "zm approver add") {
		t.Fatalf("owned project, no approver: %+v", r)
	}
	if c.Project != "elsewhere" || c.ProjectFrom != "flag" || c.As != "human:ivo" || c.Cwd != "/nowhere" {
		t.Errorf("the check left the client changed: %q %q %q %q", c.Project, c.ProjectFrom, c.As, c.Cwd)
	}
	c.Project, c.ProjectFrom, c.As, c.Cwd = "", "", "", ""
	if _, err := orchclient.AddApprover(ctx, c, "phone"); err != nil {
		t.Fatal(err)
	}
	if r := doctorAsks(ctx, env); r.Status != doctorWarn || !strings.Contains(r.Detail, "not finished") {
		t.Errorf("pending approver: %+v", r)
	}
}
