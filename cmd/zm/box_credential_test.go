package main

// A Potluck pool box reaches its project's authority with the box
// credential file alone (POTLUCK_BOX_CREDENTIAL, potluck-box/1): no daemon
// in the box, no socket, no ZM_HOST/ZM_TOKEN, nothing printed that holds
// the token or the key, and every problem fails closed.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// boxAuthority is a zm daemon whose tunnel door serves on a loopback HTTP
// server at its tunnel.url, with identity ci holding member on orchid. It
// has no Unix socket listener: a box never has one to reach.
type boxAuthority struct {
	dm  *server.Daemon
	svc *tunnel.Service
}

func newBoxAuthority(t *testing.T) *boxAuthority {
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
	if _, err := reg.Create(ctx, reg.Local(""), api.NewProjectRequest{Slug: "orchid"}); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Grant(ctx, reg.Local(""), api.GrantRequest{Identity: "ci", Role: "member", Project: "orchid"}); err != nil {
		t.Fatal(err)
	}
	return &boxAuthority{dm: dm, svc: svc}
}

// mint writes a box credential file for ci (a token bound to a new box
// key, ttl seconds) and answers its path and what it holds.
func (a *boxAuthority) mint(t *testing.T, ttl int) (string, tunnel.BoxCredential) {
	t.Helper()
	key, err := tunnel.NewBoxKey()
	if err != nil {
		t.Fatal(err)
	}
	reg := a.dm.Deps.Projects
	out, err := reg.Mint(context.Background(), reg.Local(""), api.MintTokenRequest{As: "ci", Project: "orchid", Role: "member",
		TTLSeconds: int64(ttl), NoGrant: true, BoxKey: tunnel.BoxKeyID(key)})
	if err != nil {
		t.Fatal(err)
	}
	url, fp, err := a.svc.Home()
	if err != nil {
		t.Fatal(err)
	}
	cred := tunnel.BoxCredential{Token: out.Secret, Key: key, URL: url, Home: fp}
	return writeBoxFile(t, cred), cred
}

func writeBoxFile(t *testing.T, cred tunnel.BoxCredential) string {
	t.Helper()
	enc, err := cred.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return writeRaw(t, enc)
}

func writeRaw(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "POTLUCK_TOKEN")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// boxEnv is zm's environment in a box: the credential file, plus a
// ZM_HOST/ZM_TOKEN pair that must be ignored (it points nowhere).
func boxEnv(path string) []string {
	return []string{orchclient.EnvBoxCredential + "=" + path, "ZM_HOST=http://127.0.0.1:1", "ZM_TOKEN=zm_ignored", "ZM_NO_AUTOSTART="}
}

// noSecrets fails when out holds cred's token or key.
func noSecrets(t *testing.T, what, out string, cred tunnel.BoxCredential) {
	t.Helper()
	for _, s := range []string{cred.Token, hex.EncodeToString(cred.Key.Seed())} {
		if s != "" && strings.Contains(out, s) {
			t.Errorf("%s printed the box's token or key:\n%s", what, out)
		}
	}
}

// The verbs credentialed box jobs use, against the authority, with nothing but the
// credential file: zm whoami, send, lease (take with a note, release), who,
// ask new. No daemon starts and nothing is written under the box's home.
func TestBoxCredentialVerbs(t *testing.T) {
	a := newBoxAuthority(t)
	path, cred := a.mint(t, 600)
	z := newZeroEnv(t)
	env := boxEnv(path)
	zm := func(args ...string) string {
		t.Helper()
		out, errOut, code := z.run(z.home, env, args...)
		noSecrets(t, "zm "+strings.Join(args, " "), out+errOut, cred)
		if code != 0 {
			t.Fatalf("zm %s: exit %d\n%s%s", strings.Join(args, " "), code, out, errOut)
		}
		return out
	}

	var w api.Whoami
	if err := json.Unmarshal([]byte(zm("--json", "whoami")), &w); err != nil || w.Identity.Name != "ci" || w.Via != "token" {
		t.Fatalf("whoami in the box: %+v %v", w, err)
	}
	if out := zm("-P", "orchid", "send", "owners", "--needs", "human", "deploy of orchid finished"); !strings.Contains(out, "sent ") {
		t.Fatalf("send: %s", out)
	}
	if out := zm("-P", "orchid", "lease", "lock:deploy", "--note", "job: deploying orchid"); !strings.Contains(out, "lock:deploy") {
		t.Fatalf("lease: %s", out)
	}
	if out := zm("-P", "orchid", "who", "lock:deploy"); !strings.Contains(out, "held by human:ci") {
		t.Fatalf("who: %s", out)
	}
	var h struct{ Lease struct{ Holder, Note string } }
	if err := json.Unmarshal([]byte(zm("--json", "-P", "orchid", "who", "lock:deploy")), &h); err != nil || h.Lease.Note != "job: deploying orchid" {
		t.Fatalf("who --json: %+v %v", h, err)
	}
	if out := zm("-P", "orchid", "lease", "lock:deploy", "--release"); !strings.Contains(out, "released lock:deploy") {
		t.Fatalf("lease --release: %s", out)
	}
	var asked struct{ ID string }
	if err := json.Unmarshal([]byte(zm("--json", "-P", "orchid", "ask", "new",
		"--title", "Roll back orchid", "--context", "The orchid deploy made pages slower for everyone.",
		"--choice", "Roll back | fast again | loses today's fix", "--choice", "Wait | keeps the fix | slow for a day",
		"--recommend", "a", "--why", "speed matters more than the fix today",
		"--searched", "I read the earlier deploy notes and found no rollback decided before",
		"--", "Should orchid roll back to yesterday's version?")), &asked); err != nil || asked.ID == "" {
		t.Fatalf("ask new: %+v %v", asked, err)
	}

	if pid := z.daemonPID(); pid != 0 {
		t.Errorf("a daemon started in the box (pid %d)", pid)
	}
	if _, err := os.Stat(filepath.Join(z.zmHome(), "zm.sock")); err == nil {
		t.Error("the box has a zm socket")
	}
}

// Every problem with the credential exits non-zero with a plain message
// naming POTLUCK_BOX_CREDENTIAL, quotes neither the token nor the key, and
// never falls back to a daemon or another login. Unsetting it (or setting
// it empty) is today's client exactly.
func TestBoxCredentialFailsClosed(t *testing.T) {
	a := newBoxAuthority(t)
	path, cred := a.mint(t, 600)
	z := newZeroEnv(t)
	enc, err := cred.Encode()
	if err != nil {
		t.Fatal(err)
	}
	wrongHome := cred
	wrongHome.Home = strings.Repeat("a", 26)
	expiredPath, expired := a.mint(t, 1)
	unreadable := writeRaw(t, enc)
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, path, want string }{
		{"missing file", filepath.Join(t.TempDir(), "nope"), "no such file"},
		{"not json (the token alone)", writeRaw(t, cred.Token), "not a potluck-box/1 file"},
		{"wrong kind", writeRaw(t, strings.Replace(enc, tunnel.BoxCredentialKind, "potluck-box/2", 1)), "not a potluck-box/1 file"},
		{"bad key", writeRaw(t, strings.Replace(enc, `"key":"`, `"key":"00`, 1)), "not an ed25519 seed"},
		{"another authority's fingerprint", writeBoxFile(t, wrongHome), "fingerprint mismatch"},
	}
	if os.Geteuid() != 0 { // root reads a 0000 file
		cases = append(cases, struct{ name, path, want string }{"unreadable file", unreadable, "permission denied"})
	}
	time.Sleep(1100 * time.Millisecond) // expired's token has lived its second
	cases = append(cases, struct{ name, path, want string }{"expired token", expiredPath, "refused this box's token"})
	for _, tc := range cases {
		out, errOut, code := z.run(z.home, boxEnv(tc.path), "-P", "orchid", "send", "owners", "--needs", "human", "hello")
		local := tc.want != "refused this box's token" && tc.want != "fingerprint mismatch" // nothing was sent
		if code == 0 || !strings.Contains(errOut, orchclient.EnvBoxCredential) || !strings.Contains(errOut, tc.want) ||
			(local && strings.Contains(errOut, "refused")) || strings.Count(errOut, orchclient.EnvBoxCredential) != 1 {
			t.Errorf("%s: exit %d, want non-zero and %q\n%s%s", tc.name, code, tc.want, out, errOut)
		}
		noSecrets(t, tc.name, out+errOut, cred)
		noSecrets(t, tc.name, out+errOut, expired)
	}

	toks, err := a.dm.Deps.Projects.Tokens(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tok := range toks {
		if _, err := a.dm.Deps.Projects.RevokeToken(context.Background(), tok.ID); err != nil {
			t.Fatal(err)
		}
	}
	out, errOut, code := z.run(z.home, boxEnv(path), "whoami")
	if code == 0 || !strings.Contains(errOut, "refused this box's token") {
		t.Errorf("revoked token: exit %d\n%s%s", code, out, errOut)
	}
	noSecrets(t, "revoked token", out+errOut, cred)
	if pid := z.daemonPID(); pid != 0 {
		t.Errorf("a refused box credential started a daemon (pid %d)", pid)
	}

	// Unset or empty: the socket client as today (no daemon here: exit 3).
	for _, env := range [][]string{{"ZM_NO_AUTOSTART=1"}, {orchclient.EnvBoxCredential + "=", "ZM_NO_AUTOSTART=1"}} {
		out, errOut, code := z.run(z.home, env, "whoami")
		if code != 3 || !strings.Contains(errOut, "not running") || strings.Contains(errOut, orchclient.EnvBoxCredential) {
			t.Errorf("%v: exit %d, want today's daemon-not-running\n%s%s", env, code, out, errOut)
		}
	}
}
