package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runners"
)

// runCLI runs cmd with args, answering what it printed.
func runCLI(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	oldStdout := os.Stdout
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = f
	cmd.SetArgs(args)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = cmd.ExecuteContext(ctx)
	os.Stdout = oldStdout
	f.Close()
	b, _ := os.ReadFile(f.Name())
	return string(b), err
}

// zm runners setup turns the broker on through the daemon (no config.json
// edit, no restart) and prints the one URL to open; ls shows the result.
func TestRunnersSetupAndLs(t *testing.T) {
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
	cfg.Human, cfg.Machine = "ana", "mini"
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

	out, err := runCLI(t, runnersCmd(), "ls")
	if err != nil || !strings.Contains(out, "off on this machine") {
		t.Fatalf("ls before setup: %v\n%s", err, out)
	}
	if _, err := runCLI(t, runnersCmd(), "setup", "pool"); err == nil || !strings.Contains(err.Error(), "zm runners setup <pool> --url https://<host>") || strings.Contains(err.Error(), "potluck") {
		t.Fatalf("setup with no tunnel and no --url: %v", err)
	}
	out, err = runCLI(t, runnersCmd(), "setup", "pool", "--url", "https://pool.example")
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Open this URL to create the GitHub App") || !strings.Contains(out, "\n    https://pool.example/v0/runners/app/new?state=") {
		t.Fatalf("setup printed:\n%s", out)
	}
	var st runners.Setup
	if err := sock.Do(ctx, "GET", "/v0/runners/setup", nil, &st); err != nil || st.Listening == "" || st.Listening != st.Listen {
		t.Fatalf("setup status %+v %v", st, err)
	}
	disk, _ := config.LoadFrom(home)
	if r := disk.File.Runners; r.Listen != st.Listen || r.PublicURL != "https://pool.example" || r.Pool.Project != "pool" {
		t.Fatalf("config.json runners %+v", r)
	}
	out, err = runCLI(t, runnersCmd(), "ls")
	for _, want := range []string{"pool        pool", "public url  https://pool.example", "listener    " + st.Listen, "app         none yet", "links       none"} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("ls lacks %q: %v\n%s", want, err, out)
		}
	}
	// zm runners set changes a setting through the daemon; ls shows it.
	for _, want := range []string{"repos       none connected: no job is served (zm runners allow <owner/repo>)", "fallback    3600 s"} {
		if !strings.Contains(out, want) {
			t.Errorf("ls lacks %q:\n%s", want, out)
		}
	}
	out, err = runCLI(t, runnersCmd(), "set", "job_ttl_s", "5400")
	if err != nil || !strings.Contains(out, "zm: job_ttl_s set, from the next job on") || !strings.Contains(out, "fallback    5400 s") {
		t.Fatalf("set job_ttl_s: %v\n%s", err, out)
	}
	// The global public switch is gone: the broker's answer names zm's
	// per-repository command, not potluck's.
	if _, err := runCLI(t, runnersCmd(), "set", "allow_public_repos", "on"); err == nil || !strings.Contains(err.Error(), "zm runners allow <owner/repo> --public") || strings.Contains(err.Error(), "potluck") {
		t.Fatalf("set allow_public_repos: %v", err)
	}
	if _, err := runCLI(t, runnersCmd(), "set", "job_ttl_s", "60"); err == nil || !strings.Contains(err.Error(), "whole seconds from 900") {
		t.Fatalf("set job_ttl_s under the least: %v", err)
	}
	if disk, _ := config.LoadFrom(home); disk.File.Runners.JobTTLS != 5400 {
		t.Fatalf("config.json runners after set %+v", disk.File.Runners)
	}
	out, err = runCLI(t, runnersCmd(), "ls")
	for _, want := range []string{"fallback    5400 s"} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("ls after set lacks %q: %v\n%s", want, err, out)
		}
	}
	// Repositories are connected one by one, through the broker's own rule:
	// none yet, a bad name refused, one not connected cannot be disconnected,
	// and with no App made there is nothing to connect through.
	if out, err = runCLI(t, runnersCmd(), "repos"); err != nil || !strings.Contains(out, "none connected") {
		t.Fatalf("repos: %v\n%s", err, out)
	}
	if _, err := runCLI(t, runnersCmd(), "allow", "not-a-repo"); err == nil || !strings.Contains(err.Error(), "owner/name") {
		t.Fatalf("allow a bad name: %v", err)
	}
	if _, err := runCLI(t, runnersCmd(), "allow", "acme/app", "--public"); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("allow with no App: %v", err)
	}
	if _, err := runCLI(t, runnersCmd(), "disallow", "acme/app"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("disallow a repository never connected: %v", err)
	}
	if _, err := runCLI(t, runnersCmd(), "disallow", "acme"); err == nil || !strings.Contains(err.Error(), "not owner/repo") {
		t.Fatalf("disallow a bad name: %v", err)
	}
	// A tunnel on another hostname is not touched; a quick one cannot route.
	dir := filepath.Join(home, "tunnel", "cloudflared")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"hostname":"other.example","tunnel_id":"x"}`), 0o600)
	if msg, err := runnersIngress(runnersCmd(), home, st); err != nil || msg != "" {
		t.Errorf("ingress for another hostname: %q %v", msg, err)
	}
	_ = os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"hostname":"pool.example","quick":true}`), 0o600)
	if _, err := runnersIngress(runnersCmd(), home, st); err == nil || !strings.Contains(err.Error(), "quick tunnel") {
		t.Errorf("ingress on a quick tunnel: %v", err)
	}
}
