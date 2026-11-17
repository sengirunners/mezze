package session

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
)

func holderIdentityControlSession(t *testing.T, h *harness) orchapi.Session {
	t.Helper()
	ss, err := orchclient.Register(context.Background(), h.c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "holder-identity-control", Cwd: h.home})
	if err != nil {
		t.Fatal(err)
	}
	return ss
}

// The test process supplies a stable live PID: these checks only probe it and
// never authorize a signal. Missing identity must be conservative, while an
// exact kernel-start mismatch still proves that the recorded process is gone.
func TestRecordedHolderKernelStartEvidence(t *testing.T) {
	h := newHarness(t, nil)
	ss := holderIdentityControlSession(t, h)
	st, ok := procStat(os.Getpid())
	if !ok || st.start <= 0 {
		t.Skip("kernel process start unavailable")
	}
	ctx := context.Background()
	if err := h.s.recordStartedHolder(ctx, ss.ID, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		start int64
		gone  bool
	}{
		{"matching identity stays live", st.start, false},
		{"missing identity stays live", 0, false},
		{"different verified start proves reuse", st.start + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.d.Store.Registry().Exec(`UPDATE session_holders SET holder_start=? WHERE id=?`, tc.start, ss.ID); err != nil {
				t.Fatal(err)
			}
			if gone, why := h.s.recordedHolderGone(ctx, ss.ID); gone != tc.gone {
				t.Fatalf("gone=%v want %v: %s", gone, tc.gone, why)
			}
		})
	}
}

// Reaping a real child supplies an absent PID without inventing an invalid
// number. If the OS immediately reused it, skip rather than touching its owner.
func TestRecordedHolderWithoutStartRecognizesAbsentPID(t *testing.T) {
	h := newHarness(t, nil)
	ss := holderIdentityControlSession(t, h)
	p := exec.Command("true") // PATH: /usr/bin/true on macOS, /bin/true on Linux
	if err := p.Run(); err != nil {
		t.Fatal(err)
	}
	pid := p.Process.Pid
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Skip("exited PID is no longer provably absent")
	}
	ctx := context.Background()
	if err := h.s.recordStartedHolder(ctx, ss.ID, pid); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Store.Registry().Exec(`UPDATE session_holders SET holder_start=0 WHERE id=?`, ss.ID); err != nil {
		t.Fatal(err)
	}
	if gone, why := h.s.recordedHolderGone(ctx, ss.ID); !gone {
		// An intervening reuse is legitimate uncertainty, never signal it.
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Skip("PID reused while checking absence")
		}
		t.Fatalf("absent holder not recognized: %s", why)
	}
}
