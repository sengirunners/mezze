package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/mezze/internal/work/clean"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// liveShell is what the user's shell exports on a developer's Mac: the farm's
// live Dolt server and its credentials. None of it may reach an adapter.
var liveShell = map[string]string{
	"BEADS_DOLT_SERVER_HOST":     "live-farm.invalid",
	"BEADS_DOLT_SERVER_PORT":     "3307",
	"BEADS_DOLT_SERVER_USER":     "farm",
	"BEADS_DOLT_SERVER_DATABASE": "harbor",
	"BEADS_DOLT_SERVER_MODE":     "1",
	"BEADS_DOLT_PASSWORD":        "live-farm-password",
	"BEADS_DIR":                  "/live/.beads",
	"BD_REAL":                    "/live/bd",
	"DOLT_CLI_PASSWORD":          "live-dolt-password",
	"GH_TOKEN":                   "ghp_live_shell_token",
}

func exportLiveShell(t *testing.T) {
	for k, v := range liveShell {
		t.Setenv(k, v)
	}
}

func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func loadState(t *testing.T, state string) *fake.State {
	t.Helper()
	st, err := fake.Load(state)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestTrackerAddIsLiveAndAllowlisted: one call binds a tracker without a
// restart, checks it and syncs it; nothing of the user's shell (BEADS_*,
// BD_*, DOLT_*, GH_TOKEN) in the daemon's environment reaches the adapter
// (SPEC 17.2), unless the binding opts out with env = "inherit".
func TestTrackerAddIsLiveAndAllowlisted(t *testing.T) {
	quiet(t)
	h := newHarness(t)
	state := h.installFake()
	exportLiveShell(t)
	owner := h.as(h.owner, "harbor")

	res, err := orchclient.AddTracker(h.ctx, owner, orchapi.AddTrackerRequest{Adapter: "fake", Prefix: "bd"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || !res.Synced || res.Tracker.Prefix != "bd" || res.Tracker.Env != orchapi.TrackerEnvAllowlist {
		t.Fatalf("add: %+v", res)
	}
	for _, c := range res.Checks {
		if !c.OK && !c.Skipped {
			t.Fatalf("check failed: %+v", c)
		}
	}
	// Live: in the registry and in project.toml, no restart.
	f, _ := h.d.Projects.File(h.harbor.ID)
	if len(orchcore.ProjectTrackers(f)) != 1 || orchcore.ProjectTrackers(f)[0].Adapter != "fake" {
		t.Fatalf("binding %+v", orchcore.ProjectTrackers(f))
	}
	b, _ := os.ReadFile(filepath.Join(h.d.Store.ProjectDir(h.harbor.ID), project.FileName))
	if pf, err := project.ParseFile(string(b)); err != nil || len(orchcore.ProjectTrackers(pf)) != 1 || orchcore.ProjectTrackers(pf)[0].Adapter != "fake" {
		t.Fatalf("project.toml not rewritten (%v):\n%s", err, b)
	}

	// The proof: no variable of the user's shell reached any adapter call.
	st := loadState(t, state)
	for k := range liveShell {
		if slices.Contains(st.EnvKeys, k) {
			t.Errorf("%s from the daemon's environment reached the adapter", k)
		}
	}

	// The bound tracker serves reads at once.
	edit(t, state, func(st *fake.State) {
		st.Issues["harbor-1"] = &fake.Issue{ID: "harbor-1", Title: "first item", Status: "open", UpdatedAt: fake.Now()}
	})
	h.sync()
	if _, err := orchclient.GetWork(h.ctx, owner, "bd:harbor-1"); err != nil {
		t.Fatalf("read after add: %v", err)
	}

	// env = "inherit" opts a tracker out: then the shell does reach it.
	res, err = orchclient.AddTracker(h.ctx, owner, orchapi.AddTrackerRequest{Adapter: "fake", Prefix: "inh", Env: "inherit"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tracker.Env != api.TrackerEnvInherit {
		t.Fatalf("inherit: %+v", res.Tracker)
	}
	if env := envMap(loadState(t, state).Env); env["BEADS_DOLT_SERVER_HOST"] != "live-farm.invalid" {
		t.Fatalf("env inherit did not pass the shell through: %v", env)
	}
	ls, err := orchclient.Trackers(h.ctx, owner, "")
	if err != nil || len(ls) != 2 || ls[0].Env != "allowlist" || ls[1].Env != "inherit" {
		t.Fatalf("tracker ls: %+v %v", ls, err)
	}
}

// TestTrackerAddRollsBack: a tracker that fails a check is not bound: the
// binding, its mirror and its directory are gone again.
func TestTrackerAddRollsBack(t *testing.T) {
	quiet(t)
	h := newHarness(t)
	state := h.installFake()
	edit(t, state, func(st *fake.State) { st.FailList = "the tracker is broken" })
	owner := h.as(h.owner, "harbor")
	before, _ := os.ReadFile(filepath.Join(h.d.Store.ProjectDir(h.harbor.ID), project.FileName))
	_, err := orchclient.AddTracker(h.ctx, owner, orchapi.AddTrackerRequest{Adapter: "fake", Prefix: "bd"})
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeUnavailable || !strings.Contains(e.Message, "not bound") {
		t.Fatalf("want a refusal, got %v", err)
	}
	var res orchapi.TrackerResult
	if json.Unmarshal(e.Data, &res) != nil || res.OK || len(res.Checks) == 0 {
		t.Fatalf("error data: %s", e.Data)
	}
	f, _ := h.d.Projects.File(h.harbor.ID)
	after, _ := os.ReadFile(filepath.Join(h.d.Store.ProjectDir(h.harbor.ID), project.FileName))
	if len(orchcore.ProjectTrackers(f)) != 0 || string(before) != string(after) {
		t.Fatalf("binding left behind: %+v\n%s", orchcore.ProjectTrackers(f), after)
	}
	if h.s.m.lookup(h.harbor.ID, "bd") != nil {
		t.Fatal("syncer left running")
	}
	db, err := h.d.Store.Project(authz.All(), authz.Read, h.harbor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := mirrorCount(h.ctx, db, "bd"); n != 0 {
		t.Fatalf("%d mirrored rows left", n)
	}
	if core.PathExists(h.s.trackerDir(h.harbor.ID, "bd")) {
		t.Fatal("the directory it made was left behind")
	}
	// The same add works once the tracker does.
	edit(t, state, func(st *fake.State) { st.FailList = "" })
	if _, err := orchclient.AddTracker(h.ctx, owner, orchapi.AddTrackerRequest{Adapter: "fake", Prefix: "bd"}); err != nil {
		t.Fatal(err)
	}
}

// TestTrackerLsTestRm covers the rest of the commands.
func TestTrackerLsTestRm(t *testing.T) {
	quiet(t)
	h := newHarness(t)
	h.installFake()
	owner := h.as(h.owner, "harbor")
	if _, err := orchclient.AddTracker(h.ctx, owner, orchapi.AddTrackerRequest{Adapter: "fake", Prefix: "bd"}); err != nil {
		t.Fatal(err)
	}
	// A second add of the same prefix is refused.
	if _, err := orchclient.AddTracker(h.ctx, owner, orchapi.AddTrackerRequest{Adapter: "fake", Prefix: "bd"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("same prefix: %v", err)
	}
	res, err := orchclient.TestTracker(h.ctx, owner, "bd")
	if err != nil || !res.OK || !res.Synced {
		t.Fatalf("test: %+v %v", res, err)
	}
	if _, err := orchclient.TestTracker(h.ctx, owner, "nope"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("test missing: %v", err)
	}
	ls, err := orchclient.Trackers(h.ctx, owner, "")
	if err != nil || len(ls) != 1 || ls[0].Sync == nil || ls[0].Sync.State != orchapi.SyncOK || ls[0].Slug != "harbor" {
		t.Fatalf("ls: %+v %v", ls, err)
	}
	if _, err := orchclient.RemoveTracker(h.ctx, owner, "bd"); err != nil {
		t.Fatalf("rm: %v", err)
	}
	if f, _ := h.d.Projects.File(h.harbor.ID); len(orchcore.ProjectTrackers(f)) != 0 {
		t.Fatalf("rm left %+v", orchcore.ProjectTrackers(f))
	}
	if h.s.m.lookup(h.harbor.ID, "bd") != nil {
		t.Fatal("rm left the syncer")
	}
	if _, err := orchclient.AddTracker(h.ctx, owner, orchapi.AddTrackerRequest{Adapter: "fake", Prefix: "bd"}); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	// Token callers never reach these routes (local).
	guest := h.as(h.guest(), "harbor")
	if _, err := orchclient.Trackers(h.ctx, guest, ""); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("guest ls: %v", err)
	}
	if _, err := orchclient.AddTracker(h.ctx, guest, orchapi.AddTrackerRequest{Adapter: "fake"}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("guest add: %v", err)
	}
}

// Secrets of any length are masked, and redacting keeps an error's
// identity for errors.Is (a cancelled call stays context.Canceled) without
// letting errors.As reach the unmasked original.
func TestRedactShortSecretsKeepsIdentity(t *testing.T) {
	sec := []string{"ab1"}
	orig := fmt.Errorf("dial root:ab1@db: %w", context.Canceled)
	err := clean.Err(orig, sec)
	if strings.Contains(err.Error(), "ab1") || !strings.Contains(err.Error(), "root:***@db") {
		t.Fatalf("not masked: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost its identity: %v", err)
	}
	var inner *api.Error
	wrapped := fmt.Errorf("call: %w", api.Errorf(api.CodeInternal, "root:ab1@db"))
	if errors.As(clean.Err(wrapped, sec), &inner) {
		t.Fatalf("errors.As reached the unmasked error: %v", inner)
	}
	if e := clean.Err(api.Errorf(api.CodeUnavailable, "root:ab1@db").WithHint("ab1"), sec); !api.IsCode(e, api.CodeUnavailable) || strings.Contains(e.Error(), "ab1") {
		t.Fatalf("api error: %v", e)
	}
}

// TestDescribeUsesTrackerEnv: a tracker bound with env = "inherit" is
// described with the daemon's environment too, so a custom adapter that
// needs a variable from it (the reason to use --env inherit) works; an
// allowlisted binding of the same adapter still does not see it.
func TestDescribeUsesTrackerEnv(t *testing.T) {
	quiet(t)
	h := newHarness(t)
	dir := t.TempDir()
	script := `#!/bin/sh
read -r line || true
if [ "$ZM_TEST_NEEDENV" != "yes" ]; then
  echo '{"ok":false,"error":{"code":"unavailable","message":"ZM_TEST_NEEDENV is not set"}}'
  exit 0
fi
echo '{"ok":true,"result":{"zm":"zm/v0","name":"needenv","prefix":"ne","version":"0.1.0","verbs":["describe","list","get","claim","release","comment","set_status"]}}'
`
	if err := os.WriteFile(filepath.Join(dir, adapter.ExecPrefix+"needenv"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin:/bin")
	t.Setenv("ZM_TEST_NEEDENV", "yes")
	tr, err := h.s.tracker(h.ctx, h.harbor.ID, api.Tracker{Adapter: "needenv", Prefix: "ne", Env: api.TrackerEnvInherit})
	if err != nil || tr.desc.Name != "needenv" {
		t.Fatalf("inherit: describe failed: %v", err)
	}
	if _, err := h.s.tracker(h.ctx, h.harbor.ID, api.Tracker{Adapter: "needenv", Prefix: "ne2"}); err == nil {
		t.Fatal("an allowlisted tracker saw the daemon's variable")
	}
}

// TestAdapterGetsOnlyDeclaredEnv (SPEC 17.2): with the farm's live shell
// in the daemon's environment, a list call sees the base, the variables
// describe.env declares (zm's own from zm) and its secret from the file its
// config names, and nothing else. The same adapter bound with env =
// "inherit" sees the daemon's environment.
func TestAdapterGetsOnlyDeclaredEnv(t *testing.T) {
	quiet(t)
	h := newHarness(t)
	dir := t.TempDir()
	dump, tok := filepath.Join(dir, "env.txt"), filepath.Join(dir, "token")
	if err := os.WriteFile(tok, []byte("tok-s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
read -r line || true
case "$line" in
*'"verb":"describe"'*) echo '{"ok":true,"result":{"zm":"zm/v0","name":"envdump","prefix":"ed","version":"0.1.0","verbs":["describe","list","get","claim","release","comment","set_status"],"env":{"vars":["ZM_DATA_DIR","ED_WANT"],"secrets":[{"name":"ED_TOKEN","from":"config:token_file"}]}}}' ;;
*) /usr/bin/env > %q; echo '{"ok":true,"result":{"items":[]}}' ;;
esac
`, dump)
	if err := os.WriteFile(filepath.Join(dir, adapter.ExecPrefix+"envdump"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin:/bin")
	exportLiveShell(t)
	t.Setenv("ED_WANT", "yes")
	t.Setenv("ED_UNWANTED", "no")
	t.Setenv("ZM_DATA_DIR", "/from/the/shell")
	t.Setenv("ED_TOKEN", "from-the-shell")
	list := func(tr api.Tracker) map[string]string {
		t.Helper()
		tt, err := h.s.tracker(h.ctx, h.harbor.ID, tr)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.s.call(h.ctx, tt, orchapi.VerbList, "", orchapi.AdapterListArgs{}, nil); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(dump)
		if err != nil {
			t.Fatal(err)
		}
		return envMap(strings.Split(strings.TrimSpace(string(b)), "\n"))
	}
	env := list(api.Tracker{Adapter: "envdump", Prefix: "ed", Config: map[string]string{"token_file": tok}})
	if env["ED_WANT"] != "yes" || env["ED_TOKEN"] != "tok-s3cret" || env["ZM_DATA_DIR"] != h.s.trackerDir(h.harbor.ID, "ed") {
		t.Fatalf("declared variables: %v", env)
	}
	for k := range env {
		switch k {
		case "PATH", "HOME", "LANG", "TERM", "TZ", "ZM_DATA_DIR", "ED_WANT", "ED_TOKEN":
		case "PWD", "SHLVL", "_": // sh's own
		default:
			t.Errorf("%s reached the adapter undeclared", k)
		}
	}
	// No token file: the secret is unset, never the shell's.
	if env := list(api.Tracker{Adapter: "envdump", Prefix: "ed2"}); env["ED_TOKEN"] != "" {
		t.Fatalf("ED_TOKEN without token_file: %q", env["ED_TOKEN"])
	}
	if env := list(api.Tracker{Adapter: "envdump", Prefix: "ed3", Env: api.TrackerEnvInherit}); env["ED_UNWANTED"] != "no" {
		t.Fatalf("inherit: %v", env)
	}
}

// TestVerifyKeepsTheMirrorOutOfTheWriteCheck: verify proves check writes
// nothing by comparing the tracker's files, and a mirror cycle that reads
// them, as bd's embedded Dolt does on any read, must not fail that proof.
// A write by check itself still does.
func TestVerifyKeepsTheMirrorOutOfTheWriteCheck(t *testing.T) {
	h := newHarness(t)
	h.d.Config.SyncPause = time.Millisecond // cycles back to back: one always overlaps an unheld check
	bin, data := t.TempDir(), t.TempDir()
	db, wrote := filepath.Join(data, "db"), filepath.Join(bin, "check-writes")
	script := fmt.Sprintf(`#!/bin/sh
read -r line
case "$line" in
*'"verb":"describe"'*) echo '{"ok":true,"result":{"zm":"zm/v0","name":"reader","prefix":"rd","version":"0.1.0","verbs":["describe","list","get","claim","release","comment","set_status","check"]}}' ;;
*'"verb":"list"'*) echo read >> %[1]q; /bin/sleep 0.02; echo read >> %[1]q; echo '{"ok":true,"result":{"items":[]}}' ;;
*'"verb":"check"'*) /bin/sleep 0.2; if [ -f %[2]q ]; then echo wrote >> %[1]q; fi; echo '{"ok":true,"result":{}}' ;;
*) echo '{"ok":false,"error":{"code":"not_implemented","message":"no"}}' ;;
esac
`, db, wrote)
	if err := os.WriteFile(filepath.Join(bin, adapter.ExecPrefix+"reader"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+"/usr/bin:/bin")
	tr := api.Tracker{Adapter: "reader", Prefix: "rd", Config: map[string]string{"dir": data}}
	writeCheck := func() orchapi.TrackerCheck {
		t.Helper()
		var res orchapi.TrackerResult
		h.s.verify(h.ctx, h.harbor.ID, tr, time.Minute, &res)
		for _, c := range res.Checks {
			if c.Name == "check answers and writes nothing" {
				return c
			}
		}
		t.Fatalf("no write check in %+v", res.Checks)
		return orchapi.TrackerCheck{}
	}
	if c := writeCheck(); !c.OK || c.Skipped {
		t.Fatalf("a mirror read failed the write check: %+v", c)
	}
	if err := os.WriteFile(wrote, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if c := writeCheck(); c.OK || !strings.Contains(c.Detail, "check wrote: changed "+db) {
		t.Fatalf("a check that writes passed: %+v", c)
	}
	// A sync that holds the tracker past wait: the check is skipped, and the
	// result's notes say why rather than only counting a skip.
	sy := h.s.m.syncer(h.harbor.ID, tr)
	sy.busy <- struct{}{}
	var res orchapi.TrackerResult
	h.s.verify(h.ctx, h.harbor.ID, tr, 200*time.Millisecond, &res)
	<-sy.busy
	if !slices.ContainsFunc(res.Notes, func(n string) bool {
		return strings.HasPrefix(n, "check answers and writes nothing: not run: a mirror sync was still reading")
	}) {
		t.Fatalf("a skipped write check left no note: %+v", res.Notes)
	}
}
