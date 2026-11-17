package setup

import (
	"sync"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// Prefixes serializes a tracker's setup, removal and scan reconciliation.
// A conflict fails fast before side effects can touch the same directory,
// adapter or mirror while another lifecycle operation owns it.
type Prefixes struct {
	mu sync.Mutex
	m  map[string]prefixReservation
}

type prefixReservation struct {
	scan bool
	done chan struct{}
}

func (a *Prefixes) Reserve(pid, prefix string) bool {
	return a.reserve(pid, prefix, false)
}

// TryScan skips busy prefixes. Mutations wait for scans instead of reporting
// a conflict with background maintenance; mutations still conflict mutually.
func (a *Prefixes) TryScan(pid, prefix string) bool { return a.reserve(pid, prefix, true) }

func (a *Prefixes) reserve(pid, prefix string, scan bool) bool {
	k := orchapi.TrackerKey(pid, prefix)
	for {
		a.mu.Lock()
		if held, exists := a.m[k]; exists {
			a.mu.Unlock()
			if scan || !held.scan {
				return false
			}
			<-held.done
			continue
		}
		if a.m == nil {
			a.m = map[string]prefixReservation{}
		}
		a.m[k] = prefixReservation{scan: scan, done: make(chan struct{})}
		a.mu.Unlock()
		return true
	}
}

func (a *Prefixes) Release(pid, prefix string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := orchapi.TrackerKey(pid, prefix)
	if held, exists := a.m[k]; exists {
		delete(a.m, k)
		close(held.done)
	}
}
