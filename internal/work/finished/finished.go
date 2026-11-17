// Package finished remembers leases the GitHub link's close ended, so the
// holder's replica is not told it lost them: a merged PR closing a session's
// issue is the work ending (SPEC 17.3). It is in memory: after the daemon
// restarts, or past Keep, such a lease reads as lost like any other that went
// while nobody was looking.
package finished

import (
	"sort"
	"sync"
	"time"
)

// Keep is how long a record lasts: long enough for a Mac asleep overnight.
const Keep = 24 * time.Hour

type key struct{ project, holder, key string }

// Leases is the set of records. The zero value is ready.
type Leases struct {
	mu sync.Mutex
	at map[key]time.Time
}

// Add records that holder's lease on k in project ended by the link's close.
func (f *Leases) Add(project, holder, k string, now time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.at == nil {
		f.at = map[key]time.Time{}
	}
	for x, t := range f.at {
		if now.Sub(t) > Keep {
			delete(f.at, x)
		}
	}
	f.at[key{project, holder, k}] = now
}

// Forget ends a record: the holder claimed k again, so a later end is judged on
// its own.
func (f *Leases) Forget(project, holder, k string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.at, key{project, holder, k})
}

// Of is the keys, of those given, that have a live record for holder.
func (f *Leases) Of(project, holder string, keys []string, now time.Time) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, k := range keys {
		if t, ok := f.at[key{project, holder, k}]; ok && now.Sub(t) <= Keep {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
