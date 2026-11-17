package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/config"
)

// zm t3 enable writes the switch (the default checkout) and restarts a
// running daemon; again it changes nothing; disable removes the key. A zm
// session is refused: the switch is the owner's consent.
func TestT3EnableDisable(t *testing.T) {
	z := newZeroEnv(t)
	noStart := []string{"ZM_NO_AUTOSTART=1"}
	cfgPath := filepath.Join(z.zmHome(), config.FileConfig)
	block := func() (config.T3, bool) {
		t.Helper()
		var doc map[string]json.RawMessage
		b, _ := os.ReadFile(cfgPath)
		_ = json.Unmarshal(b, &doc)
		raw, ok := doc["t3"]
		var c config.T3
		_ = json.Unmarshal(raw, &c)
		return c, ok
	}

	if _, e, code := z.run(z.home, append(noStart, "ZM_SESSION=sess:zero/aaaa1111"), "t3", "enable"); code == 0 || !strings.Contains(e, "not a zm session") {
		t.Fatalf("enable from a session: %d %s", code, e)
	}
	if _, ok := block(); ok {
		t.Fatal("a refused enable wrote the switch")
	}
	out, e, code := z.run(z.home, noStart, "t3", "enable")
	if code != 0 || !strings.Contains(out, "enabled on zero") || !strings.Contains(out, "not running") {
		t.Fatalf("enable: %d %s %s", code, out, e)
	}
	if c, ok := block(); !ok || c.Checkout != filepath.Join(z.zmHome(), "t3code") {
		t.Fatalf("after enable: %+v %v", c, ok)
	}
	if z.daemonPID() != 0 {
		t.Fatal("enable started a daemon")
	}
	if out, _, code := z.run(z.home, noStart, "t3", "enable"); code != 0 || !strings.Contains(out, "already enabled") {
		t.Fatalf("enable again: %d %s", code, out)
	}

	// With a daemon running: disable restarts it.
	if _, e, code := z.run(z.home, nil, "daemon", "--detach"); code != 0 {
		t.Fatalf("start: %s", e)
	}
	pid := z.daemonPID()
	out, e, code = z.run(z.home, nil, "t3", "disable")
	if code != 0 || !strings.Contains(out, "disabled on zero") || !strings.Contains(out, "zm daemon restarted") {
		t.Fatalf("disable: %d %s %s", code, out, e)
	}
	if p := z.daemonPID(); p == 0 || p == pid {
		t.Fatalf("disable did not restart the daemon (pid %d, was %d)", p, pid)
	}
	if _, ok := block(); ok {
		t.Fatal("disable left the key")
	}
}
