package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
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

func doctorByName(rs []doctorResult) map[string]doctorResult {
	m := map[string]doctorResult{}
	for _, r := range rs {
		m[r.Check] = r
	}
	return m
}

// TestDoctorNoDaemon: with the daemon down, zm doctor fails the daemon
// check with a fix line and skips what needs it.
func TestDoctorNoDaemon(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	env := &doctorEnv{Config: cfg, DaemonErr: api.Errorf(api.CodeUnavailable, "daemon not running"),
		LookPath: func(string) (string, error) { return "", exec.ErrNotFound }}
	rs := doctorByName(runDoctor(context.Background(), env))
	if r := rs["home"]; r.Status != doctorOK {
		t.Errorf("home: %+v", r)
	}
	if r := rs["daemon"]; r.Status != doctorFail || !strings.Contains(r.Fix, "zm daemon") {
		t.Errorf("daemon: %+v", r)
	}
	for _, name := range []string{"trackers", "tailnet"} {
		if r := rs[name]; r.Status != doctorSkip {
			t.Errorf("%s: %+v, want skip", name, r)
		}
	}
	if r := rs["agents"]; r.Status != doctorWarn || r.Fix == "" {
		t.Errorf("agents: %+v", r)
	}
	text := doctorText(runDoctor(context.Background(), env))
	if !strings.Contains(text, "fix: zm daemon") || !strings.Contains(text, "1 failed") {
		t.Errorf("text:\n%s", text)
	}
}

// TestDoctorHomeProblems: a home that is a file, or open to others, gets a
// plain fix line; a missing one is only a warning.
func TestDoctorHomeProblems(t *testing.T) {
	dir, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	cases := []struct {
		name, status string
		setup        func(string)
	}{
		{"missing", doctorWarn, func(string) {}},
		{"file", doctorFail, func(p string) { os.WriteFile(p, nil, 0o600) }},
		{"open", doctorWarn, func(p string) { os.Mkdir(p, 0o755); os.Chmod(p, 0o755) }},
	}
	for _, c := range cases {
		p := dir + "/" + c.name
		c.setup(p)
		cfg := &config.Config{Home: p}
		r := doctorHome(context.Background(), &doctorEnv{Config: cfg})
		if r.Status != c.status || r.Fix == "" {
			t.Errorf("%s: %+v, want %s with a fix", c.name, r, c.status)
		}
	}
	r := doctorHome(context.Background(), &doctorEnv{ConfErr: errors.New("bad json")})
	if r.Status != doctorFail || !strings.Contains(r.Fix, "config.json") {
		t.Errorf("config error: %+v", r)
	}
}

// TestDoctorWithDaemon: against a live (in-process) daemon every check
// that needs it runs and passes on a fresh home.
func TestDoctorWithDaemon(t *testing.T) {
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
	h, err := orchclient.Health(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	env := &doctorEnv{Config: cfg, Client: c, Health: &h, LookPath: func(string) (string, error) { return "/bin/claude", nil }}
	rs := doctorByName(runDoctor(ctx, env))
	for _, name := range []string{"home", "daemon", "version", "trackers", "agents"} {
		if r := rs[name]; r.Status != doctorOK {
			t.Errorf("%s: %+v", name, r)
		}
	}
	if r := rs["tailnet"]; r.Status == doctorFail {
		t.Errorf("tailnet never fails doctor: %+v", r)
	}
}

// TestDoctorVerdicts: each failure gets the fix that fits it.
func TestDoctorVerdicts(t *testing.T) {
	doc, _, err := rootCmd.Find([]string{"doctor"})
	if err != nil || doc.Name() != "doctor" {
		t.Fatalf("zm doctor: %v", err)
	}
	if autostartAllowed(doc, func(string) string { return "" }) {
		t.Fatal("zm doctor may start the daemon")
	}
	cfg := &config.Config{Home: t.TempDir()}
	down := func(err error, remote bool) doctorResult {
		return doctorDaemon(context.Background(), &doctorEnv{Config: cfg, DaemonErr: err, Remote: remote})
	}
	if r := down(api.Errorf(api.CodeUnauthenticated, "bad token"), false); !strings.Contains(r.Fix, "ZM_TOKEN") {
		t.Errorf("refused: %+v", r)
	}
	if r := down(api.Errorf(api.CodeUnavailable, "no socket"), true); !strings.Contains(r.Fix, "ZM_HOST") || strings.Contains(r.Fix, "--detach") {
		t.Errorf("remote: %+v", r)
	}
	if r := down(context.DeadlineExceeded, false); !strings.Contains(r.Detail, "within") {
		t.Errorf("timeout: %+v", r)
	}
	h := orchapi.Health{Health: api.Health{Version: "v9", Commit: "abc", SelfCheck: &api.SelfCheck{OK: false, Problems: []string{"issues store"}}}}
	env := &doctorEnv{Config: cfg, Health: &h}
	if r := doctorDaemon(context.Background(), env); r.Status != doctorFail || !strings.Contains(r.Detail, "issues store") {
		t.Errorf("self-check: %+v", r)
	}
	if r := doctorVersion(context.Background(), env); r.Status != doctorWarn || !strings.Contains(r.Fix, "zm daemon restart") {
		t.Errorf("version: %+v", r)
	}
	f := &fakeTrackers{views: []orchapi.TrackerView{{Slug: "app", Prefix: "bd", Sync: &orchapi.WorkSync{Error: "bd: not found"}}}}
	if r := doctorTrackers(context.Background(), &doctorEnv{Client: client.WithHandler(f)}); r.Status != doctorFail || r.Fix != "zm tracker test bd shows which check fails" {
		t.Errorf("trackers: %+v", r)
	}
}

type fakeTrackers struct{ views []orchapi.TrackerView }

func (f *fakeTrackers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(api.HeaderProtocol, api.WireProtocol)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(orchapi.TrackerList{Trackers: f.views})
}
