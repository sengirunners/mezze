package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/swap"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Commands run only against these fake tools and a temporary HOME. The fake
// retains a cdhash on kickstart, and refreshes it only on bootstrap.
func launchAgentRig(t *testing.T) (*supervisor, string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(bin, "zm")
	if err := os.WriteFile(exe, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", "own.plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0700); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(launchdJob{Label: "own", ProgramArguments: []string{exe, "daemon"}, EnvironmentVariables: map[string]string{"ZM_HOME": filepath.Join(home, "state"), "KEPT": "exact"}})
	if err := os.WriteFile(plist, body, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_PLIST", plist)
	t.Setenv("FAKE_EXE", exe)
	t.Setenv("FAKE_DIR", home)
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$FAKE_DIR/calls"
case "$1" in
 list) if test -f "$FAKE_DIR/loaded";then printf '345\t0\town\n';fi;;
 print) if test -f "$FAKE_DIR/unloading";then
 if test -f "$FAKE_DIR/uninspectable";then exit 1;fi
 if test -f "$FAKE_DIR/foreign";then FAKE_EXE=/foreign/zm;fi
 if test -f "$FAKE_DIR/replacement";then echo "pid = 999";fi
 if ! test -f "$FAKE_DIR/never";then
 n=$(cat "$FAKE_DIR/unloading"); n=$((n-1)); echo "$n" > "$FAKE_DIR/unloading"
 if test "$n" -le 0;then rm -f "$FAKE_DIR/loaded" "$FAKE_DIR/unloading";fi
 fi
 fi
 test -f "$FAKE_DIR/loaded" || exit 1
 printf 'path = %s\nprogram = %s\narguments = {\n' "$FAKE_PLIST" "$FAKE_EXE"
cat "$FAKE_DIR/args"
printf '}\ninherited environment = {\n'
if test -f "$FAKE_DIR/inherited";then cat "$FAKE_DIR/inherited";fi
printf '}\nenvironment = {\n'
cat "$FAKE_DIR/environment"
printf '}\n';;
 kickstart) echo 'Launch Constraint Violation' >&2;exit 1;;
 bootout) test "$2" = "gui/$(id -u)/own" || exit 2
 if test -f "$FAKE_DIR/bootout_error_loaded";then rm "$FAKE_DIR/bootout_error_loaded";exit 1;fi
 if test -f "$FAKE_DIR/async";then echo 3 > "$FAKE_DIR/unloading";else rm "$FAKE_DIR/loaded";fi
 if test -f "$FAKE_DIR/bootout_error_removed";then rm "$FAKE_DIR/bootout_error_removed";exit 1;fi;;
 bootstrap) test "$3" = "$FAKE_PLIST" || exit 2
 if test -f "$FAKE_DIR/loaded";then echo 'Operation already in progress' >&2;exit 37;fi
 if test -f "$FAKE_DIR/fail";then rm "$FAKE_DIR/fail";exit 1;fi
 cp "$FAKE_EXE" "$FAKE_DIR/started"
 touch "$FAKE_DIR/loaded";;
 *) exit 2;;
esac
`
	for name, content := range map[string]string{"launchctl": script, "plutil": "#!/bin/sh\ncat \"$FAKE_PLIST\"\n"} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(filepath.Join(home, "loaded"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "args"), []byte(exe+"\ndaemon\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "environment"), []byte("ZM_HOME => "+filepath.Join(home, "state")+"\nKEPT => exact\n"), 0600); err != nil {
		t.Fatal(err)
	}
	v := &supervisor{Kind: "launchd", Unit: "own"}
	if err := prepareOwnedLaunchAgent(context.Background(), v, exe, filepath.Join(home, "state")); err != nil {
		t.Fatal(err)
	}
	return v, exe, home
}

func TestOwnedLaunchAgentRefreshAndRollback(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "new artifact", true: "failed bootstrap restores old artifact"}[fail], func(t *testing.T) {
			v, exe, home := launchAgentRig(t)
			ctx := context.Background()
			// Reproduce the actual old command's launch constraint failure.
			if _, err := supervisorOutput(ctx, supervisorCallWait, true, "launchctl", "kickstart", "-k", v.launch.target); err == nil {
				t.Fatal("kickstart unexpectedly accepted changed identity")
			}
			if err := os.WriteFile(exe, []byte("new verified artifact"), 0600); err != nil {
				t.Fatal(err)
			}
			if fail {
				if err := os.WriteFile(filepath.Join(home, "fail"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err := supervisorRestart(ctx, v)
			if fail {
				if err == nil {
					t.Fatal("bootstrap failure hidden")
				}
				// The guarded swap restores its files before using this same supervisor
				// again. A missing job is allowed only after our own successful bootout.
				if err := os.WriteFile(exe, []byte("old"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := supervisorRestart(ctx, v); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(filepath.Join(home, "started"))
			want := "new verified artifact"
			if fail {
				want = "old"
			}
			if string(got) != want {
				t.Fatalf("started %q, want %q", got, want)
			}
			original, _ := os.ReadFile(v.launch.path)
			if string(original) != string(v.launch.body) {
				t.Fatal("plist or environment changed")
			}
			calls, _ := os.ReadFile(filepath.Join(home, "calls"))
			if strings.Count(string(calls), "bootout ") != 1 {
				t.Fatalf("unexpected unregisters: %s", calls)
			}
		})
	}
}

func TestOwnedLaunchAgentRefusesDrift(t *testing.T) {
	for _, change := range []string{"plist", "missing", "foreign", "arguments", "home", "environment"} {
		t.Run(change, func(t *testing.T) {
			v, _, home := launchAgentRig(t)
			switch change {
			case "plist":
				if err := os.WriteFile(v.launch.path, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				if err := os.Remove(filepath.Join(home, "loaded")); err != nil {
					t.Fatal(err)
				}
			case "arguments":
				os.WriteFile(filepath.Join(home, "args"), []byte(v.launch.program+"\ndaemon\n--tailnet\n"), 0600)
			case "home":
				os.WriteFile(filepath.Join(home, "environment"), []byte("ZM_HOME => /different/home\nKEPT => exact\n"), 0600)
			case "environment":
				os.WriteFile(filepath.Join(home, "environment"), []byte("ZM_HOME => "+filepath.Join(home, "state")+"\nKEPT => changed\n"), 0600)
			case "foreign":
				t.Setenv("FAKE_EXE", "/foreign/zm")
			}
			if err := supervisorRestart(context.Background(), v); err == nil {
				t.Fatal("unsafe job accepted")
			}
			calls, _ := os.ReadFile(filepath.Join(home, "calls"))
			if strings.Contains(string(calls), "bootout ") || strings.Contains(string(calls), "bootstrap ") {
				t.Fatalf("mutated unsafe job: %s", calls)
			}
		})
	}
}

func TestOwnedLaunchAgentRefusesInitialLoadedMismatch(t *testing.T) {
	for _, change := range []string{"arguments", "home"} {
		t.Run(change, func(t *testing.T) {
			v, exe, home := launchAgentRig(t)
			if change == "arguments" {
				os.WriteFile(filepath.Join(home, "args"), []byte(exe+"\ndaemon\n--tailnet\n"), 0600)
			} else {
				os.WriteFile(filepath.Join(home, "environment"), []byte("ZM_HOME => /another/home\nKEPT => exact\n"), 0600)
			}
			fresh := &supervisor{Kind: "launchd", Unit: "own"}
			if err := prepareOwnedLaunchAgent(context.Background(), fresh, exe, filepath.Join(home, "state")); err == nil {
				t.Fatal("disk/loaded mismatch accepted")
			}
			if fresh.launch != nil {
				t.Fatal("unsafe registration retained")
			}
			calls, _ := os.ReadFile(filepath.Join(home, "calls"))
			if strings.Contains(string(calls), "bootout ") {
				t.Fatal("mismatch unloaded job")
			}
			_ = v
		})
	}
}

func TestOwnedLaunchAgentEffectiveHomeDrift(t *testing.T) {
	_, exe, home := launchAgentRig(t)
	plist := filepath.Join(home, "Library", "LaunchAgents", "own.plist")
	job := launchdJob{Label: "own", ProgramArguments: []string{exe, "daemon"}, EnvironmentVariables: map[string]string{"KEPT": "exact"}}
	body, _ := json.Marshal(job)
	for path, data := range map[string][]byte{plist: body, filepath.Join(home, "environment"): []byte("KEPT => exact\n"), filepath.Join(home, "inherited"): []byte("HOME => " + home + "\n")} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	v := &supervisor{Kind: "launchd", Unit: "own"}
	if err := prepareOwnedLaunchAgent(context.Background(), v, exe, filepath.Join(home, ".zm")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "inherited"), []byte("HOME => /another/user\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := supervisorRestart(context.Background(), v); err == nil {
		t.Fatal("inherited effective home drift accepted")
	}
	fresh := &supervisor{Kind: "launchd", Unit: "own"}
	if err := prepareOwnedLaunchAgent(context.Background(), fresh, exe, filepath.Join(home, ".zm")); err == nil {
		t.Fatal("initial inherited home mismatch accepted")
	}
	calls, _ := os.ReadFile(filepath.Join(home, "calls"))
	if strings.Contains(string(calls), "bootout ") {
		t.Fatal("different-home job unloaded")
	}
}

func TestOwnedLaunchAgentFailedUnloadRecovery(t *testing.T) {
	for _, mode := range []string{"removed", "loaded"} {
		t.Run(mode, func(t *testing.T) {
			v, exe, home := launchAgentRig(t)
			if err := os.WriteFile(filepath.Join(home, "bootout_error_"+mode), nil, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(exe, []byte("new"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := supervisorRestart(context.Background(), v); err == nil {
				t.Fatal("unregister failure hidden")
			}
			// Existing guarded swap restores artifacts before retrying the same job.
			if err := os.WriteFile(exe, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := supervisorRestart(context.Background(), v); err != nil {
				t.Fatal(err)
			}
			started, _ := os.ReadFile(filepath.Join(home, "started"))
			if string(started) != "old" {
				t.Fatalf("rollback started %q", started)
			}
			calls, _ := os.ReadFile(filepath.Join(home, "calls"))
			want := 1
			if mode == "loaded" {
				want = 2
			}
			if strings.Count(string(calls), "bootout ") != want {
				t.Fatalf("unexpected unloads: %s", calls)
			}
		})
	}
}

func TestOwnedLaunchAgentFailedUnloadRefusesReplacement(t *testing.T) {
	v, _, home := launchAgentRig(t)
	if err := os.WriteFile(filepath.Join(home, "bootout_error_removed"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := supervisorRestart(context.Background(), v); err == nil {
		t.Fatal("unregister failure hidden")
	}
	// A foreign replacement is never stopped, even after our attempted unload.
	if err := os.WriteFile(filepath.Join(home, "loaded"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_EXE", "/foreign/zm")
	if err := supervisorRestart(context.Background(), v); err == nil {
		t.Fatal("replacement accepted")
	}
	calls, _ := os.ReadFile(filepath.Join(home, "calls"))
	if strings.Count(string(calls), "bootout ") != 1 || strings.Contains(string(calls), "bootstrap ") {
		t.Fatalf("foreign job touched: %s", calls)
	}
}

// The native bootout call may return before launchd removes the label. Bootstrap
// is unsafe throughout that interval, including when the guarded swap rolls back.
func TestOwnedLaunchAgentAsyncUnload(t *testing.T) {
	for _, mode := range []string{"removed", "never", "uninspectable", "foreign", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			v, _, home := launchAgentRig(t)
			for _, name := range []string{"async", mode} {
				if err := os.WriteFile(filepath.Join(home, name), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			err := supervisorRestart(ctx, v)
			if mode == "removed" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("unsafe unload accepted")
			}
			calls, _ := os.ReadFile(filepath.Join(home, "calls"))
			if mode != "removed" && strings.Contains(string(calls), "bootstrap ") {
				t.Fatalf("bootstrapped before safe removal: %s", calls)
			}
		})
	}
}

// Exercise the public upgrade, including its real guarded rollback, with fake
// launchctl commands that reject bootstrap while an asynchronous unload remains.
func TestOwnedLaunchAgentAsyncPublicUpgrade(t *testing.T) {
	for _, mode := range []string{"upgrade", "rollback", "replacement"} {
		t.Run(mode, func(t *testing.T) {
			rollback := mode == "rollback"
			_, _, fake := launchAgentRig(t)
			r := newSwapRig(t)
			plist := filepath.Join(r.home, "Library", "LaunchAgents", "own.plist")
			if err := os.MkdirAll(filepath.Dir(plist), 0700); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(launchdJob{Label: "own", ProgramArguments: []string{r.zm, "daemon"}, EnvironmentVariables: map[string]string{"ZM_HOME": r.zmHome(), "KEPT": "exact"}})
			for path, data := range map[string][]byte{plist: body, filepath.Join(fake, "args"): []byte(r.zm + "\ndaemon\n"), filepath.Join(fake, "environment"): []byte("ZM_HOME => " + r.zmHome() + "\nKEPT => exact\n"), filepath.Join(fake, "async"): nil} {
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("FAKE_PLIST", plist)
			t.Setenv("FAKE_EXE", r.zm)
			t.Setenv("FAKE_DIR", fake)
			t.Setenv("PATH", filepath.Join(fake, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
			// Keep the tool fake; only its bootstrap branch starts this rig's test daemon.
			tool := filepath.Join(fake, "bin", "launchctl")
			script, err := os.ReadFile(tool)
			if err != nil {
				t.Fatal(err)
			}
			script = []byte(strings.Replace(string(script), `cp "$FAKE_EXE" "$FAKE_DIR/started"`, `"$FAKE_EXE" daemon > "$FAKE_DIR/daemon-output" 2>&1 &
 echo $! > "$FAKE_DIR/mainpid"`, 1))
			script = []byte(strings.Replace(string(script), `bootout) test`, `bootout) p="$(cat "$FAKE_DIR/mainpid")"; kill -TERM "$p" 2>/dev/null || true
 i=0; while kill -0 "$p" 2>/dev/null && test "$i" -lt 400; do i=$((i+1)); sleep 0.025; done
 test`, 1))
			if err = os.WriteFile(filepath.Join(fake, "mainpid"), []byte(fmt.Sprint(r.health(t).PID)), 0600); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(tool, script, 0700); err != nil {
				t.Fatal(err)
			}
			detectSupervisor = func(context.Context, int) (*supervisor, error) { return &supervisor{Kind: "launchd", Unit: "own"}, nil }
			supervisorProgram = func(context.Context, *supervisor) (string, error) { return r.zm, nil }
			supervisorMain = func(context.Context, *supervisor) (int, error) {
				b, e := os.ReadFile(filepath.Join(fake, "mainpid"))
				if e != nil {
					return 0, e
				}
				return strconv.Atoi(strings.TrimSpace(string(b)))
			}
			savedProbe := probe
			t.Cleanup(func() {
				detectSupervisor = detectSupervisorDefault
				supervisorProgram = supervisorProgramDefault
				supervisorMain = supervisorMainDefault
				probe = savedProbe
			})
			if rollback {
				calls := 0
				probe = func(ctx context.Context, c *client.Client) api.ServeCheck {
					calls++
					sc := swap.Probe(ctx, c, wiring.Probes...)
					if calls == 2 {
						sc.Items = nil
					}
					return sc
				}
			}
			if mode == "replacement" {
				if err := os.WriteFile(filepath.Join(fake, "replacement"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = runSwap(context.Background(), swapOpts{what: "upgrade", newBin: r.stage(t)})
			if mode == "replacement" {
				if err == nil || !strings.Contains(err.Error(), "process changed while unloading") {
					t.Fatalf("replacement: %v", err)
				}
				calls, e := os.ReadFile(filepath.Join(fake, "calls"))
				if e != nil {
					t.Fatal(e)
				}
				if strings.Count(string(calls), "bootout ") != 1 || strings.Contains(string(calls), "bootstrap ") {
					t.Fatalf("rollback touched replacement: %s", calls)
				}
				return
			}
			if rollback {
				if err == nil || !strings.Contains(err.Error(), "rolled back") {
					t.Fatalf("rollback: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			// runSwap saw a daemon answer; read the receipt from a settled one.
			var h api.Health
			for end := time.Now().Add(5 * time.Second); ; {
				if h, err = client.NewUnix(filepath.Join(r.zmHome(), "zm.sock")).Health(context.Background()); err == nil || time.Now().After(end) {
					break
				}
				time.Sleep(25 * time.Millisecond)
			}
			if err != nil {
				t.Fatalf("no daemon answers after the swap: %v", err)
			}
			if h.LastSwap == nil {
				t.Fatal("no completed swap receipt")
			}
			want := "ok"
			if rollback {
				want = "rolled_back"
			}
			if h.LastSwap.Result != want {
				t.Fatalf("receipt %+v", h.LastSwap)
			}
		})
	}
}
