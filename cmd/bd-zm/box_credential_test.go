package main

// bd-zm in a Potluck pool box: the box credential file alone
// (POTLUCK_BOX_CREDENTIAL) reaches the project's authority over its tunnel.
// No socket, no autostart, no prefix cache on disk, no fallback.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// boxAuthority starts a zm daemon whose tunnel door serves on loopback at
// its tunnel.url (no Unix socket listener), with project oc and identity ci
// a member, and answers a box credential for ci (a token bound to a new box
// key).
func boxAuthority(t *testing.T) (*server.Daemon, tunnel.BoxCredential) {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "dev"
	srv := httptest.NewUnstartedServer(nil)
	cfg.File.Tunnel.URL = "http://" + srv.Listener.Addr().String()
	ctx := context.Background()
	dm, err := server.New(ctx, cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	svc := tunnel.From(dm.Deps)
	srv.Config.Handler = svc.Handler(dm.Handler)
	srv.Start()
	t.Cleanup(srv.Close)
	reg := dm.Deps.Projects
	if _, err := reg.Create(ctx, reg.Local(""), api.NewProjectRequest{Slug: "oc"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Grant(ctx, reg.Local(""), api.GrantRequest{Identity: "ci", Role: "member", Project: "oc"}); err != nil {
		t.Fatal(err)
	}
	key, err := tunnel.NewBoxKey()
	if err != nil {
		t.Fatal(err)
	}
	out, err := reg.Mint(ctx, reg.Local(""), api.MintTokenRequest{As: "ci", Project: "oc", Role: "member", TTLSeconds: 600,
		NoGrant: true, BoxKey: tunnel.BoxKeyID(key)})
	if err != nil {
		t.Fatal(err)
	}
	url, fp, err := svc.Home()
	if err != nil {
		t.Fatal(err)
	}
	return dm, tunnel.BoxCredential{Token: out.Secret, Key: key, URL: url, Home: fp}
}

// boxRun runs bd-zm with env as its whole environment (newClient is the
// real one) and answers stdout, stderr and the exit code.
func boxRun(t *testing.T, env map[string]string, args ...string) (string, string, int) {
	t.Helper()
	// If the fail-closed branch were lost, the fallback reads the real
	// environment: keep it off the developer's home and off autostart.
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	oldHome, hadHome := os.LookupEnv("ZM_HOME")
	oldNoAuto, hadNoAuto := os.LookupEnv("ZM_NO_AUTOSTART")
	os.Setenv("ZM_HOME", home)
	os.Setenv("ZM_NO_AUTOSTART", "1")
	defer restoreEnv("ZM_HOME", oldHome, hadHome)
	defer restoreEnv("ZM_NO_AUTOSTART", oldNoAuto, hadNoAuto)
	oldGetenv, oldClient, oldCache := getenv, newClient, cacheHome
	getenv = func(k string) string {
		if k == "ZM_NO_AUTOSTART" {
			return "1" // bd-zm reads it through getenv: a lost fail-closed branch must not start a daemon
		}
		return env[k]
	}
	newClient = defaultClient
	var o, e bytes.Buffer
	stdout, stderr = &o, &e
	defer func() {
		getenv, newClient, cacheHome, stdout, stderr = oldGetenv, oldClient, oldCache, os.Stdout, os.Stderr
	}()
	code := run(context.Background(), args)
	return o.String(), e.String(), code
}

func boxFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "POTLUCK_TOKEN")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The bd verbs credentialed box jobs use (create, list, show, update, comments add)
// with the credential file alone; every refusal is plain, non-zero and
// quotes neither the token nor the key; unset is today's client.
func TestBoxCredentialBd(t *testing.T) {
	git := gitUserName
	gitUserName = func() string { return "" }
	t.Cleanup(func() { gitUserName = git })
	_, cred := boxAuthority(t)
	enc, err := cred.Encode()
	if err != nil {
		t.Fatal(err)
	}
	secrets := []string{cred.Token, hex.EncodeToString(cred.Key.Seed())}
	env := map[string]string{orchclient.EnvBoxCredential: boxFile(t, enc), "ZM_PROJECT": "oc",
		"ZM_HOST": "http://127.0.0.1:1", "ZM_TOKEN": "zm_ignored"}
	bd := func(args ...string) string {
		t.Helper()
		out, errOut, code := boxRun(t, env, args...)
		for _, s := range secrets {
			if strings.Contains(out+errOut, s) {
				t.Errorf("bd %v printed the box's token or key", args)
			}
		}
		if code != 0 {
			t.Fatalf("bd %v: exit %d\n%s%s", args, code, out, errOut)
		}
		return out
	}

	id := strings.TrimSpace(bd("create", "Orchid deploy slow", "-p", "P1", "--silent"))
	if !strings.HasPrefix(id, "oc-") {
		t.Fatalf("create answered %q", id)
	}
	bd("update", id, "--status", "in_progress")
	bd("comments", "add", id, "job: rolled back")
	var shown []struct {
		ID, Status string
		Comments   []struct{ Text string }
	}
	if err := json.Unmarshal([]byte(bd("show", id, "--json")), &shown); err != nil || len(shown) != 1 || shown[0].Status != "in_progress" {
		t.Fatalf("show: %+v %v", shown, err)
	}
	if out := bd("comments", id); !strings.Contains(out, "job: rolled back") {
		t.Fatalf("comments: %s", out)
	}
	if out := bd("list"); !strings.Contains(out, id) {
		t.Fatalf("list lacks %s:\n%s", id, out)
	}
	if out := bd("where"); !strings.Contains(out, "box credential") {
		t.Fatalf("where: %s", out)
	}

	wrongHome := cred
	wrongHome.Home = strings.Repeat("a", 26)
	other, err := wrongHome.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ content, want string }{
		"the token alone":                 {cred.Token, "not a potluck-box/1 file"},
		"wrong kind":                      {strings.Replace(enc, tunnel.BoxCredentialKind, "potluck-box/2", 1), "not a potluck-box/1 file"},
		"another authority's fingerprint": {other, "fingerprint mismatch"},
	} {
		bad := map[string]string{orchclient.EnvBoxCredential: boxFile(t, tc.content), "ZM_PROJECT": "oc"}
		out, errOut, code := boxRun(t, bad, "list")
		if code == 0 || strings.Count(errOut, orchclient.EnvBoxCredential) != 1 || !strings.Contains(errOut, tc.want) ||
			(tc.want != "fingerprint mismatch" && strings.Contains(errOut, "refused")) {
			t.Errorf("%s: exit %d\n%s%s", name, code, out, errOut)
		}
		for _, s := range secrets {
			if strings.Contains(out+errOut, s) {
				t.Errorf("%s: printed the box's token or key", name)
			}
		}
	}

	// Unset: today's client (the socket under ZM_HOME; nothing answers).
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("ZM_HOME", home)
	t.Setenv("ZM_HOST", "")
	t.Setenv(orchclient.EnvBoxCredential, "")
	oldCache := cacheHome
	t.Cleanup(func() { cacheHome = oldCache })
	c, err := defaultClient(context.Background())
	if err != nil || c.Socket == "" || c.Autostart == nil {
		t.Fatalf("unset: %+v %v, want the socket client with autostart", c, err)
	}
}

func restoreEnv(k, v string, had bool) {
	if had {
		os.Setenv(k, v)
	} else {
		os.Unsetenv(k)
	}
}
