// Package initgc keeps the garbage collector off while package init runs on a
// plain zm command.
//
// zm is one binary, so every short CLI call (zm --version, the hooks) runs the
// init of every linked package first: about 7.5 MB of allocations, mostly
// regexps and tables nobody uses on that call. With the default GOGC that is
// two or three collections before main starts, about 0.5 ms each on Sengi,
// which is what pushed the hook and CLI bench rows over their ceilings.
//
// Only cmd/zm imports it. main calls Restore first thing, and so does its
// TestMain. Restoring in main rather than in an init leaves init's garbage
// until the command allocates enough to need a collection: zm --version exits
// with none.
//
// Hidden modes (__zm-holder, __zm-task-*, __zm-agent, __zm-launch) take over
// from some package's init and exit without reaching main, often living for
// hours (cmd/zm's TestHiddenModesKeepGC ties this list to their constants).
// So the collector goes off only for an argv that is a plain command
// (Command), and stays on for anything else.
//
// Go initialises the ready package with the lexically smallest import path
// first (Go 1.21+). This one needs only os and runtime/debug, so it ran about
// halfway through init when written (1.7 MB in, under the 4 MB first-GC goal).
// A new dependency can move it, which costs speed, not correctness: check with
// GODEBUG=inittrace=1 zm --version.
package initgc

import (
	"os"
	"runtime/debug"
)

// prev is the GC percent to restore: 100 or what GOGC says, or restored when
// the collector was never turned off.
var prev = start(os.Args)

func start(args []string) int {
	if !Command(args) {
		return restored
	}
	return debug.SetGCPercent(-1)
}

// Command reports whether args is a plain zm call: no arguments, --version, or
// a lowercase command word first. No hidden mode is one: each starts with __
// or - (the potluck drivers, in sibling binaries, too).
func Command(args []string) bool {
	if len(args) < 2 {
		return true
	}
	a := args[1]
	if a == "--version" {
		return true
	}
	if a == "" || a[0] < 'a' || a[0] > 'z' {
		return false
	}
	for i := 1; i < len(a); i++ {
		if c := a[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// Restore gives the collector back its setting (GOGC, default 100). Call it
// first thing in main (and TestMain); later calls do nothing.
func Restore() {
	if prev != restored {
		debug.SetGCPercent(prev)
		prev = restored
	}
}

// restored marks nothing to restore. SetGCPercent never returns it: off is
// reported as -1.
const restored = -2
