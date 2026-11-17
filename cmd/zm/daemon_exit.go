package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// A daemon says why it ended (one died to a stray `pkill -9 -f`
// and left no line at all). What it can catch it logs as it exits; what it
// cannot (SIGKILL, a power cut) the next daemon reports from the run marker
// left behind; a Go crash also lands in daemon-crash.log.

const (
	runMarkerName = "daemon.run"
	crashLogName  = "daemon-crash.log"
)

type runMarker struct {
	PID     int       `json:"pid"`
	Build   string    `json:"build"`
	Started time.Time `json:"started"`
}

// noteLastExit logs how the previous daemon on home ended when it left its
// run marker behind (it never reached a clean exit). Call with the home
// lock held, before markRunning.
func noteLastExit(home string, log *slog.Logger) {
	b, err := os.ReadFile(filepath.Join(home, runMarkerName))
	if err != nil {
		return
	}
	var m runMarker
	_ = json.Unmarshal(b, &m)
	cause := "killed by a signal it cannot catch (SIGKILL: kill -9, pkill -9, the OS) or the machine stopped; zm cannot see who"
	if st, err := os.Stat(filepath.Join(home, crashLogName)); err == nil && st.Size() > 0 && !st.ModTime().Before(m.Started) {
		cause = "crashed: see " + filepath.Join(home, crashLogName)
	}
	log.Warn("previous daemon ended without a clean exit", "pid", m.PID, "build", m.Build,
		"started", m.Started.Format(time.RFC3339), "cause", cause)
}

// markRunning writes the run marker and sends a Go crash's report to
// daemon-crash.log too; done removes the marker on a clean exit.
func markRunning(home string, log *slog.Logger) (done func()) {
	path := filepath.Join(home, runMarkerName)
	b, _ := json.Marshal(runMarker{PID: os.Getpid(), Build: api.Version, Started: time.Now()})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		log.Warn("daemon run marker", "err", err)
	}
	if f, err := os.OpenFile(filepath.Join(home, crashLogName), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		_ = debug.SetCrashOutput(f, debug.CrashOptions{})
		_ = f.Close() // SetCrashOutput keeps its own duplicate
	}
	return func() { _ = os.Remove(path) }
}

// exitSignals ends ctx on INT, TERM or HUP (HUP used to kill the daemon
// silently) and remembers which one came, for the exit line.
func exitSignals(parent context.Context) (ctx context.Context, which func() string, stop func()) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	var got atomic.Value
	go func() {
		select {
		case sig := <-ch:
			got.Store(sig.String())
			cancel()
		case <-ctx.Done():
		}
	}()
	which = func() string {
		select { // the signal may still sit in ch when main's own handler won the race
		case sig := <-ch:
			got.CompareAndSwap(nil, sig.String())
		default:
		}
		if s, ok := got.Load().(string); ok {
			return s
		}
		return ""
	}
	return ctx, which, func() { signal.Stop(ch); cancel() }
}

// exitCause is the daemon's last line's reason.
func exitCause(sig string, err error) string {
	switch {
	case sig != "":
		return "signal " + sig
	case err != nil:
		return "error: " + err.Error()
	default:
		return "asked to stop (zm daemon stop, restart or upgrade)"
	}
}
