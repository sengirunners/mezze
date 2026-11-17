package issues

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/core"
)

// A redaction's scrub carries its own time (core.Scrub.At),
// and a pending-scrub retry keeps the first attempt's, so the session
// module can tell copies made after the redaction from ones it must hold.
func TestRedactionScrubCarriesItsTimeAcrossRetries(t *testing.T) {
	f := newRecoveryFixture(t)
	id, cid := f.legacy(t, "retry-at-secret")
	t0 := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	f.s.now = func() time.Time { return t0 }
	var mu sync.Mutex
	var seen []time.Time
	fail := true
	f.s.d.AddScrubber(func(_ context.Context, sc core.Scrub) error {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, sc.At)
		if fail {
			return errors.New("temporary scrub failure")
		}
		return nil
	})
	if err := f.s.recoverRedaction(context.Background(), f.pid, id, cid); err == nil {
		t.Fatal("failed scrub reported success")
	}
	mu.Lock()
	fail = false
	first := len(seen)
	mu.Unlock()
	f.s.now = func() time.Time { return t0.Add(time.Hour) } // the retry runs later
	f.s.retryPendingScrubs(context.Background())
	mu.Lock()
	defer mu.Unlock()
	if first == 0 || len(seen) <= first {
		t.Fatalf("scrub attempts: %d before the retry, %d after", first, len(seen))
	}
	for i, at := range seen {
		if !at.Equal(t0) {
			t.Fatalf("attempt %d carried At %v, want the redaction's %v", i, at, t0)
		}
	}
}
