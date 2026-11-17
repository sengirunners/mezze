package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/release"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A binary knows the public repo, and the repo it was built from when that is another; a value that is not
// owner/name is ignored, and the public repo is never listed twice.
func TestSourceRepos(t *testing.T) {
	for built, want := range map[string]string{
		"":                      publicRepo,
		publicRepo:              publicRepo,
		"Sengirunners/Mezze":    publicRepo,
		"example-org/zm":        publicRepo + " example-org/zm",
		"https://example.com/x": publicRepo,
		"a/b/c":                 publicRepo,
		"a b/c":                 publicRepo,
	} {
		if got := strings.Join(sourceRepos(publicRepo, built), " "); got != want {
			t.Errorf("sourceRepos(%q) = %q, want %q", built, got, want)
		}
	}
	if buildRepo == "" && (len(updateRepos) != 1 || updateBuildRepo != publicRepo) {
		t.Errorf("a plain build: updateRepos %q updateBuildRepo %q, want only %s", updateRepos, updateBuildRepo, publicRepo)
	}
}

func TestUpdateSelectsOnlyFinishedGreenCanonicalPush(t *testing.T) {
	sha := strings.Repeat("a", 40)
	good := updateRun{SHA: sha, Branch: "main", Event: "push", Status: "completed", Conclusion: "success", Number: 10}
	good.Repository.FullName = updateBuildRepo
	runs := []updateRun{good}
	for _, mut := range []func(*updateRun){
		func(r *updateRun) { r.Event = "pull_request" },
		func(r *updateRun) { r.Event = "workflow_dispatch" },
		func(r *updateRun) { r.Status = "in_progress" },
		func(r *updateRun) { r.Conclusion = "failure" },
		func(r *updateRun) { r.Branch = "feature" },
		func(r *updateRun) { r.Repository.FullName = "fork/mezze" },
		func(r *updateRun) { r.SHA = "bad; touch injected" },
	} {
		r := good
		r.Number = 99
		mut(&r)
		runs = append(runs, r)
	}
	b, _ := json.Marshal(map[string]any{"workflow_runs": runs})
	got, err := latestUpdate(b)
	if err != nil || got != sha {
		t.Fatalf("selected %q: %v", got, err)
	}
	b, _ = json.Marshal(map[string]any{"workflow_runs": runs[1:]})
	if _, err = latestUpdate(b); err == nil {
		t.Fatal("selected ineligible run")
	}
	// GitHub's full_name and the origin a binary was built from may differ in case: still the canonical repo.
	upper := good
	upper.Repository.FullName = strings.ToUpper(updateBuildRepo)
	b, _ = json.Marshal(map[string]any{"workflow_runs": []updateRun{upper}})
	if got, err := latestUpdate(b); err != nil || got != sha {
		t.Fatalf("a run on %s: %q %v", upper.Repository.FullName, got, err)
	}
	// A completed green run with a higher run number wins even if API order differs.
	newer := good
	newer.Number = 11
	newer.SHA = strings.Repeat("b", 40)
	b, _ = json.Marshal(map[string]any{"workflow_runs": []updateRun{newer, good}})
	if got, _ = latestUpdate(b); got != newer.SHA {
		t.Fatal("did not select newest green main")
	}
}

func TestUpdateHoldWindowsAndFile(t *testing.T) {
	for _, tc := range []struct {
		at, window string
		want       bool
	}{
		{"23:00", "22:00-06:00", true}, {"05:59", "22:00-06:00", true}, {"06:00", "22:00-06:00", false},
		{"12:00", "10:00-12:00", false}, {"10:00", "10:00-12:00", true}, {"12:00", "00:00-00:00", true},
	} {
		now, _ := time.Parse("15:04", tc.at)
		why, err := updateHeld(now, "", tc.window)
		if err != nil || (why != "") != tc.want {
			t.Fatalf("%+v: %s %v", tc, why, err)
		}
	}
	if _, err := updateHeld(time.Now(), "", "bad"); err == nil {
		t.Fatal("invalid window accepted")
	}
	f := filepath.Join(t.TempDir(), "hold")
	os.WriteFile(f, nil, 0600)
	if why, err := updateHeld(time.Now(), f, ""); why == "" || err != nil {
		t.Fatalf("file hold: %s %v", why, err)
	}
	// The hold switch writes $ZM_HOME/update.hold with its reason; resume removes it.
	t.Setenv("ZM_HOME", t.TempDir())
	hold := updateHoldCmd()
	hold.SetArgs([]string{"investigating", "dev-05"})
	hold.SetOut(io.Discard)
	if err := hold.Execute(); err != nil {
		t.Fatal(err)
	}
	cfg, _ := loadConfig()
	if why, err := updateHeld(time.Now(), release.HoldPath(cfg.Home), ""); !strings.Contains(why, "(investigating dev-05)") || err != nil {
		t.Fatalf("hold switch: %q %v", why, err)
	}
	resume := updateResumeCmd()
	resume.SetArgs(nil)
	resume.SetOut(io.Discard)
	if err := resume.Execute(); err != nil {
		t.Fatal(err)
	}
	if why, err := updateHeld(time.Now(), release.HoldPath(cfg.Home), ""); why != "" || err != nil {
		t.Fatalf("after resume: %q %v", why, err)
	}
}

func TestUpdateIgnoresInheritedUnsafeOverrides(t *testing.T) {
	got := strings.Join(updateEnv([]string{"PATH=/test/bin", "ZM_HOME=/test/home", "FORCE=1", "END_SESSIONS=1", "MAKEFLAGS=-e", "INSTALL_DIR=/bad", "GOFLAGS=-modfile=/bad", "GIT_DIR=/bad", "GH_HOST=evil.invalid", "GH_DEBUG=api"}), "\n")
	if strings.Contains(got, "/bad") || strings.Contains(got, "=1") || strings.Contains(got, "evil") || strings.Contains(got, "GH_DEBUG") {
		t.Fatal(got)
	}
	for _, want := range []string{"PATH=/test/bin", "ZM_HOME=/test/home", "GOENV=off", "GOWORK=off", "GOFLAGS=", "GH_HOST=github.com"} {
		if !strings.Contains(got, want) {
			t.Fatal(got)
		}
	}
}

// The update build passes the caller's GOPRIVATE through unchanged (potluck
// is a public module and needs no entry) and builds potluck's drivers next to
// ./cmd/....
func TestUpdateKeepsCallersGoprivateAndBuildsPotluck(t *testing.T) {
	gp := func(env []string) []string {
		var out []string
		for _, e := range updateEnv(env) {
			if strings.HasPrefix(e, "GOPRIVATE=") {
				out = append(out, e)
			}
		}
		return out
	}
	if got := gp([]string{"PATH=/test/bin"}); len(got) != 0 {
		t.Errorf("no GOPRIVATE: %q", got)
	}
	if got := gp([]string{"GOPRIVATE=example.com/x"}); len(got) != 1 || got[0] != "GOPRIVATE=example.com/x" {
		t.Errorf("caller's GOPRIVATE: %q", got)
	}
	if strings.Join(potluckCmds, " ") != "github.com/sengirunners/potluck/cmd/potluck-driver-tart github.com/sengirunners/potluck/cmd/potluck-driver-local github.com/sengirunners/potluck/cmd/potluck-conformance "+
		"github.com/sengirunners/potluck/cmd/zm-run-tart github.com/sengirunners/potluck/cmd/zm-run-local github.com/sengirunners/potluck/cmd/zm-run-conformance github.com/sengirunners/potluck/cmd/zm-e2b" {
		t.Errorf("potluck commands: %v", potluckCmds)
	}
}

func TestUpdateNativeMovedIDPrecheckReadOnly(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "projects", "p")
	os.MkdirAll(dir, 0700)
	path := filepath.Join(dir, "issues.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE issues(id TEXT); CREATE TABLE moves(old_id TEXT,dir TEXT); INSERT INTO issues VALUES('collided'); INSERT INTO moves VALUES('collided','out')`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	before, _ := os.ReadFile(path)
	if err = checkMovedIssues(context.Background(), home); err == nil || !strings.Contains(err.Error(), "collided") {
		t.Fatalf("collision: %v", err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("precheck wrote to native database")
	}
	db, _ = sql.Open("sqlite", path)
	db.Exec(`DELETE FROM issues`)
	db.Close()
	if err = checkMovedIssues(context.Background(), home); err != nil {
		t.Fatal(err)
	}
}

// Drive the real CLI and a daemon in a throwaway home, but replace network,
// checkout and compiler processes. Observable install side effects must stay
// absent on holds/unchanged/failure, including a hold created DURING the build.
func TestUpdateCommandDoesNotInstallWhileHeld(t *testing.T) {
	for _, scenario := range []string{"held", "hold_after_build", "unchanged", "install", "default_dir", "roll_forward", "downgrade", "unresolved"} {
		t.Run(scenario, func(t *testing.T) {
			z := newZeroEnv(t)

			if scenario != "held" {
				os.MkdirAll(z.zmHome(), 0700)
				ln, err := net.Listen("unix", filepath.Join(z.zmHome(), "zm.sock"))
				if err != nil {
					t.Fatal(err)
				}
				srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Zm-Protocol", api.Protocol)
					if r.Method == "GET" {
						current := strings.Repeat("b", 40)
						if scenario == "unchanged" {
							current = strings.Repeat("a", 40)
						}
						if scenario == "unresolved" {
							current = "unknown"
						}
						json.NewEncoder(w).Encode(api.Health{OK: true, Protocol: api.Protocol, Commit: current})
						return
					}
					var rec api.SwapRecord
					json.NewDecoder(r.Body).Decode(&rec)
					json.NewEncoder(w).Encode(rec)
				})}
				go srv.Serve(ln)
				t.Cleanup(func() { srv.Close() })
			} else if _, e, code := z.run(z.home, nil, "daemon", "--detach"); code != 0 {
				t.Fatal(e)
			}

			fake := filepath.Join(z.home, "tools")
			os.Mkdir(fake, 0700)
			trace, installed, hold := filepath.Join(z.home, "trace"), filepath.Join(z.home, "installed"), filepath.Join(z.home, "hold")
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(fake, name), []byte("#!/bin/sh\nset -eu\n"+body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			write("git", `printf 'git %s\n' "$*" >> "$TEST_TRACE"
case "$1" in
 remote) printf '%s\n' 'https://github.com/sengirunners/mezze.git';;
 rev-parse) printf '%040s\n' bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb;;
 merge-base) if [ "$4" = refs/remotes/origin/main ] || [ "${TEST_DOWNGRADE:-}" = 1 ] || [ "$3" = bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb ]; then exit 0; else exit 1; fi;;
 worktree) if [ "$2" = add ]; then mkdir -p "$4"; fi;;
esac
`)
			write("gh", `printf 'gh\n' >> "$TEST_TRACE"
printf '{"workflow_runs":[{"head_sha":"%s","head_branch":"main","event":"push","status":"completed","conclusion":"success","run_number":1,"repository":{"full_name":"sengirunners/mezze"}}]}' "$TEST_SHA"
`)
			write("go", `printf 'build\n' >> "$TEST_TRACE"
while [ "$1" != -o ]; do shift; done
shift
cat > "$1/zm" <<'STAGED'
#!/bin/sh
printf '%s\n' "$@" > "$TEST_INSTALL"
if [ -n "${FORCE:-}${END_SESSIONS:-}${MAKEFLAGS:-}${INSTALL_DIR:-}" ]; then exit 99; fi
if [ "${TEST_FAIL:-}" = 1 ]; then echo 'NOT rolled back: rolled_forward' >&2; exit 1; fi
STAGED
chmod +x "$1/zm"
if [ "${TEST_HOLD_AFTER:-}" = 1 ]; then touch "$TEST_HOLD"; fi
`)
			sha := strings.Repeat("a", 40)
			if scenario == "held" {
				os.WriteFile(hold, nil, 0600)
			}
			extra := []string{"PATH=" + fake + ":" + os.Getenv("PATH"), "TEST_SHA=" + sha, "TEST_TRACE=" + trace, "TEST_INSTALL=" + installed, "TEST_HOLD=" + hold, "FORCE=1", "END_SESSIONS=1", "MAKEFLAGS=-e", "INSTALL_DIR=/bad"}
			if scenario == "hold_after_build" {
				extra = append(extra, "TEST_HOLD_AFTER=1")
			}
			if scenario == "roll_forward" {
				extra = append(extra, "TEST_FAIL=1")
			}
			if scenario == "downgrade" {
				extra = append(extra, "TEST_DOWNGRADE=1")
			}
			args := []string{"daemon", "update", "--repo", z.home, "--dir", fake, "--hold-file", hold}
			wantDir := fake
			if scenario == "default_dir" { // no --dir: the running zm's directory
				args = slices.Delete(args, 4, 6)
				exe, _ := filepath.EvalSymlinks(os.Args[0])
				wantDir = filepath.Dir(exe)
			}
			out, e, code := z.run(z.home, extra, args...)
			if (scenario == "roll_forward" || scenario == "unresolved") != (code != 0) {
				t.Fatalf("exit %d: %s %s", code, out, e)
			}
			staged, err := os.ReadFile(installed)
			wantInstall := scenario == "install" || scenario == "default_dir" || scenario == "roll_forward"
			if wantInstall != (err == nil) {
				t.Fatalf("installation side effect=%v, want %v; %s %s", err == nil, wantInstall, out, e)
			}
			if wantInstall && (strings.Contains(string(staged), "--force") || strings.Contains(string(staged), "--end-sessions") || !strings.Contains(string(staged), "--dir\n"+wantDir+"\n")) {
				t.Fatal(string(staged))
			}
			commands, _ := os.ReadFile(trace)
			if scenario == "held" && len(commands) > 0 {
				t.Fatalf("held updater ran external commands: %s", commands)
			}
			if (scenario == "unchanged" || scenario == "downgrade" || scenario == "unresolved") && strings.Contains(string(commands), "build") {
				t.Fatal(string(commands))
			}
			result := "held"
			if scenario == "install" || scenario == "default_dir" {
				result = "ok"
			}
			if scenario == "roll_forward" || scenario == "unresolved" {
				result = "failed"
			}
			if scenario == "unchanged" {
				result = "unchanged"
			}
			if !strings.Contains(out, `"result":"`+result+`"`) {
				t.Fatalf("result missing: %s %s", out, e)
			}
		})
	}
}

func TestUpdateInstallBudgetSurvivesBuildDeadline(t *testing.T) {
	invocation, cancel := context.WithCancel(context.Background())
	defer cancel()
	build, buildCancel := context.WithDeadline(invocation, time.Now().Add(-time.Second))
	defer buildCancel()
	if build.Err() != context.DeadlineExceeded {
		t.Fatal("expected exhausted build budget")
	}
	install, installCancel := updateInstallContext(invocation)
	defer installCancel()
	if install.Err() != nil {
		t.Fatal("build deadline canceled install budget")
	}
	deadline, ok := install.Deadline()
	if !ok || time.Until(deadline) < 9*time.Minute {
		t.Fatal("no reserved swap/rollback budget")
	}
	cancel()
	if install.Err() != context.Canceled {
		t.Fatal("invocation cancellation must still stop install")
	}
}

// The scheduled check on a private repository: GitHub refuses a stranger's gh
// with a 404, and the check says why in a sentence. The tick also pins this
// build's channel key, so a key rotation reaches lent machines.
func TestReleaseCheckRefusedByGitHubSaysWhy(t *testing.T) {
	z := newZeroEnv(t)
	os.MkdirAll(z.zmHome(), 0700)
	other := "ed25519:" + strings.Repeat("A", 43) + "="
	os.WriteFile(filepath.Join(z.zmHome(), "config.json"), []byte(`{"update_keys":["`+other+`"]}`), 0600)
	ln, err := net.Listen("unix", filepath.Join(z.zmHome(), "zm.sock"))
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Zm-Protocol", api.Protocol)
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(api.Health{OK: true, Protocol: api.Protocol, Commit: strings.Repeat("b", 40)})
			return
		}
		var rec api.SwapRecord
		json.NewDecoder(r.Body).Decode(&rec)
		json.NewEncoder(w).Encode(rec)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	fake := filepath.Join(z.home, "tools")
	os.Mkdir(fake, 0700)
	os.WriteFile(filepath.Join(fake, "gh"), []byte("#!/bin/sh\necho 'gh: Not Found (HTTP 404)' >&2\nexit 1\n"), 0700)
	out, e, code := z.run(z.home, []string{"PATH=" + fake + ":/usr/bin:/bin"}, "daemon", "update", "--release", "github", "--dir", fake)
	if code == 0 || !strings.Contains(out, "is private, only accounts with read access") || strings.Contains(out, "HTTP 404") {
		t.Fatalf("exit %d: %s %s", code, out, e)
	}
	b, _ := os.ReadFile(filepath.Join(z.zmHome(), "config.json"))
	if !strings.Contains(string(b), other) || !strings.Contains(string(b), release.ChannelKey) {
		t.Errorf("config after the tick:\n%s", b)
	}
}

// zm update status reads each machine's build from either heartbeat field:
// "zm" (what this build sends) or "potluck" (rename step 2); a machine with
// neither is an older one.
func TestUpdateStatusReadsEitherBuildField(t *testing.T) {
	var ms []api.Machine
	for _, raw := range []string{
		`{"machine":"dev-01","stats":{"zm":{"version":"main-aaa","held":"by hand"}}}`,
		`{"machine":"dev-02","stats":{"potluck":{"version":"main-bbb","failed":["main-ccc"]}}}`,
		`{"machine":"dev-03","stats":{}}`,
	} {
		var m api.Machine
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		ms = append(ms, m)
	}
	out := updateStatusText(api.MachineList{Machines: ms})
	for _, want := range []string{"main-aaa", "HELD since", "main-bbb", "failed here: main-ccc", "dev-03", "(older zm: no update state)"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "(older zm") != 1 {
		t.Errorf("only dev-03 is older:\n%s", out)
	}
}
