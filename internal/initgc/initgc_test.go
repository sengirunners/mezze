package initgc

import (
	"runtime/debug"
	"testing"
)

func gcPercent() int { p := debug.SetGCPercent(-1); debug.SetGCPercent(p); return p }

func TestCommand(t *testing.T) {
	for _, c := range []struct {
		args []string
		want bool
	}{
		{[]string{"zm"}, true},
		{[]string{"zm", "--version"}, true},
		{[]string{"zm", "hook", "claude", "prompt"}, true},
		{[]string{"zm", "work-show"}, true},
		{[]string{"zm", "daemon"}, true},
		{[]string{"zm", "__zm-holder", "x"}, false},
		{[]string{"zm", "__zm-task-claude"}, false},
		{[]string{"zm", "__zm-task-cloud"}, false},
		{[]string{"zm", "__zm-task-anchor"}, false},
		{[]string{"zm", "__zm-agent"}, false},
		{[]string{"zm", "__zm-agent-fake"}, false},
		{[]string{"zm", "__zm-launch"}, false},
		{[]string{"zm", "-potluck-driver-local-supervise", "x"}, false},
		{[]string{"zm", "-zm-run-local-supervise", "x"}, false},
		{[]string{"zm", "-zm-ref-driver", "a", "b"}, false},
		{[]string{"zm.test", "-test.run=X"}, false},
		{[]string{"zm", "--json", "send"}, false}, // flags first: GC stays on, merely slower
		{[]string{"zm", ""}, false},
		{[]string{"zm", "Send"}, false},
	} {
		if got := Command(c.args); got != c.want {
			t.Errorf("Command(%q) = %v, want %v", c.args, got, c.want)
		}
	}
}

// The test binary's argv (-test.*) is no plain command, so init left the GC on.
func TestHiddenArgvLeavesGCOn(t *testing.T) {
	if prev != restored {
		t.Fatalf("prev = %d for argv starting -test.*: init turned the GC off", prev)
	}
}

func TestStartPlainTurnsOffThenRestore(t *testing.T) {
	saved, savedPrev := gcPercent(), prev
	defer func() { debug.SetGCPercent(saved); prev = savedPrev }()

	debug.SetGCPercent(80)
	prev = start([]string{"zm", "hook"})
	if got := gcPercent(); got != -1 {
		t.Fatalf("GC percent after start(plain) = %d, want -1 (off)", got)
	}
	Restore()
	if got := gcPercent(); got != 80 {
		t.Fatalf("after Restore GC percent = %d, want 80", got)
	}
	debug.SetGCPercent(80)
	if prev = start([]string{"zm", "__zm-holder"}); gcPercent() != 80 || prev != restored {
		t.Fatalf("start(hidden mode) touched the GC: percent %d, prev %d", gcPercent(), prev)
	}
}

func TestRestore(t *testing.T) {
	saved, savedPrev := gcPercent(), prev
	defer func() { debug.SetGCPercent(saved); prev = savedPrev }()

	prev = 70 // as if GOGC=70 and a plain command turned it off
	debug.SetGCPercent(-1)
	Restore()
	if got := gcPercent(); got != 70 {
		t.Fatalf("after Restore GC percent = %d, want 70", got)
	}
	debug.SetGCPercent(50)
	Restore() // a second call does nothing
	if got := gcPercent(); got != 50 {
		t.Fatalf("second Restore set GC percent to %d, want 50 left alone", got)
	}
}
