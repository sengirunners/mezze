package session

import (
	"context"
	"os"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session/holder"
)

// Simulate the exact pretracking attach path: bytes were written into a real
// holder-owned PTY, but the old daemon recorded no input state. The subsequent
// daemon must not interpret absent tracking proof as an empty input prompt.
func TestReviewNudgeFirstUpgradePreservesUntrackedDraft(t *testing.T) {
	home := keptHome(t)
	h := startHarness(t, home, nil, true)
	t.Cleanup(h.close)
	ss := h.promptSession(t)
	var ps pids
	ps.add(t, ss.PID, h.holderPID(ss.ID))
	// db2c94ae's attach path called p.write without noteKeys. Do the same
	// here instead of fabricating proc.draft or bypassing the real PTY.
	if err := h.s.proc(ss.ID).write([]byte("preupgrade unsent")); err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "preupgrade unsent")
	// Remove any new-session tracking files to model an old daemon which never
	// created them. This is fixture preparation, before the first adoption.
	for _, path := range []string{holder.DraftPath(home, ss.ID)} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	h.close()
	// Both restarts preserve the holder; the final daemon owns normal cleanup.
	h2 := startHarness(t, home, nil, true)
	t.Cleanup(h2.close)
	assertHeld := func(h *harness) {
		t.Helper()
		h.markIdle(t, ss)
		accepted, err := h.s.Nudge(context.Background(), ss.Address, NudgeLine)
		if err != nil {
			t.Fatal(err)
		}
		nudgeSettles()
		// Inspect the actual PTY result even if Nudge already admitted the write.
		h.refuteOutput(t, ss.ID, NudgeLine, "SENT<")
		if accepted {
			t.Fatal("adopted pretracking draft was admitted for a mail nudge")
		}
	}
	assertHeld(h2)
	h2.close()
	h3 := startHarness(t, home, nil, false)
	t.Cleanup(h3.close)
	assertHeld(h3)
	// A later real plain Enter submits the original draft, proving the holder
	// retained it throughout both adoptions. Only then can a nudge safely wake.
	if err := orchclient.Input(context.Background(), h3.c, ss.ID, orchapi.InputRequest{Data: " completed", Enter: true}); err != nil {
		t.Fatal(err)
	}
	h3.waitOutput(ss.ID, "SENT<preupgrade unsent completed>")
	h3.markIdle(t, ss)
	if ok, err := h3.s.Nudge(context.Background(), ss.Address, NudgeLine); !ok || err != nil {
		t.Fatalf("known submission did not enable unattended wake: %v %v", ok, err)
	}
	h3.waitOutput(ss.ID, "SENT<"+NudgeLine+">")
}
