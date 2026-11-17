package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// zm ask bundle end to end against a real daemon: an agent adds steps for a
// person (the test binary's step judge passes them), lists them, runs a
// step's check line as its adder (exit 0 records it done and exits 0; a
// failing check exits 1 and changes nothing), and drops one; a check run by
// anyone but its adder needs --yes; an unknown card is not found.
func TestAskBundleCLI(t *testing.T) {
	z := newZeroEnv(t)
	repo := gitRepo(t, filepath.Join(z.home, "demo"), "https://example.com/acme/demo.git")
	zm := func(args ...string) (string, string, int) {
		t.Helper()
		return z.run(repo, nil, args...)
	}
	ok := func(args ...string) string {
		t.Helper()
		out, errOut, code := zm(args...)
		if code != 0 {
			t.Fatalf("zm %s: exit %d\n%s%s", strings.Join(args, " "), code, out, errOut)
		}
		return out
	}
	ok("init", "--slug", "demo")
	var reg struct{ Address string }
	decodeInto(t, ok("--json", "session", "register", "--agent", "claude", "--runtime-id", "rt-bundle-1", "--title", "bundle", "--cwd", repo), &reg)
	var item struct{ ID string }
	decodeInto(t, ok("--json", "work", "new", "--", "update zm on every machine"), &item)
	brief := filepath.Join(t.TempDir(), "brief.md")
	if err := os.WriteFile(brief, []byte("1. Open Terminal.\n2. Run: zm daemon upgrade\n3. Run: zm version and see the newest version from https://example.com/zm/releases.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	add := func(title, check string) string {
		t.Helper()
		var st struct{ ID, State, Person string }
		decodeInto(t, ok("--json", "--as", reg.Address, "ask", "bundle", "add", "--to", "human:ana", "--work", item.ID,
			"--title", title, "--brief", brief, "--check", check), &st)
		if st.ID == "" || st.State != "pending" || st.Person != "ana" {
			t.Fatalf("added: %+v", st)
		}
		return st.ID
	}
	good := add("Update zm on your Mac", "test -n done")
	bad := add("Turn on daily zm updates", "test -z not-yet")
	if out := ok("ask", "bundle", "ls", "--to", "human:ana"); !strings.Contains(out, good) || !strings.Contains(out, bad) || !strings.Contains(out, "Update zm on your Mac") {
		t.Fatalf("ls: %s", out)
	}
	if _, errOut, code := zm("ask", "bundle", "add", "--to", "human:ana", "--work", item.ID, "--title", "x", "--check", "true"); code == 0 || !strings.Contains(errOut, "--brief") {
		t.Fatalf("add with no brief: exit %d %s", code, errOut)
	}

	// Only its adder runs the check, unless --yes.
	if _, errOut, code := zm("ask", "bundle", "step-check", good); code == 0 || !strings.Contains(errOut, "--yes") {
		t.Fatalf("step-check by another: exit %d %s", code, errOut)
	}
	if out, errOut, code := zm("--as", reg.Address, "ask", "bundle", "step-check", bad); code != 1 || !strings.Contains(out, "check exited 1; pending") {
		t.Fatalf("a failing check: exit %d %s%s", code, out, errOut)
	}
	if out := ok("--as", reg.Address, "ask", "bundle", "step-check", good); !strings.Contains(out, "check exited 0; done") {
		t.Fatalf("a passing check: %s", out)
	}
	if out := ok("ask", "bundle", "drop", bad); !strings.Contains(out, "dropped") {
		t.Fatalf("drop: %s", out)
	}
	if out := ok("ask", "bundle", "ls"); strings.Contains(out, good) || strings.Contains(out, bad) {
		t.Fatalf("done and dropped steps in the default list: %s", out)
	}
	if out := ok("ask", "bundle", "ls", "--state", "done"); !strings.Contains(out, good) {
		t.Fatalf("ls --state done: %s", out)
	}

	if _, errOut, code := zm("ask", "bundle", "check", "01JZZZZZZZZZZZZZZZZZZZZZZZ"); code == 0 || !strings.Contains(errOut, "no bundle") {
		t.Fatalf("check of no card: exit %d %s", code, errOut)
	}
	if _, errOut, code := zm("ask", "bundle", "report", "01JZZZZZZZZZZZZZZZZZZZZZZZ", "first", "done", "x"); code == 0 || !strings.Contains(errOut, "number on the card") {
		t.Fatalf("report with no number: exit %d %s", code, errOut)
	}
	if _, errOut, code := zm("ask", "bundle", "report", "01JZZZZZZZZZZZZZZZZZZZZZZZ", "1", "done", "x"); code == 0 || !strings.Contains(errOut, "no bundle") {
		t.Fatalf("report on no card: exit %d %s", code, errOut)
	}
}
