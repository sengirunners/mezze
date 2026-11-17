package homes

// Tracker backups (SPEC 17.9): the state zm keeps per tracker (home.json)
// and the lock that keeps a restore and a backup of one tracker apart.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/homes/home"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
)

// userHomeDir is the user's home (icloud targets). Tests replace it.
var userHomeDir = os.UserHomeDir

// homeState is $ZM_HOME/trackers/<project>/<prefix>/home.json: the
// tracker's backups. A home.json written by an older zm may also hold the
// Dolt home's admin login, a move record and a rollback copy ("admin",
// "move", "previous"); they are ignored when read and dropped on the next
// write.
type homeState struct {
	Backups []backupRec `json:"backups,omitempty"`
}

type backupRec struct {
	ID        string     `json:"id"`
	Target    string     `json:"target"`
	Base      string     `json:"base"` // the target's directory; this tracker's copies are under <base>/<slug>-<prefix>/<kind>
	Kind      string     `json:"kind"`
	Every     int        `json:"every_seconds"`
	Keep      int        `json:"keep,omitempty"`
	Created   time.Time  `json:"created"`
	LastRun   *time.Time `json:"last_run,omitempty"`
	LastOK    *time.Time `json:"last_ok,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	Failures  int        `json:"failures,omitempty"`
	Items     int        `json:"items,omitempty"`
	Bytes     int64      `json:"bytes,omitempty"`
	// StoreBytes (sqlite): the issue store's last good copy as stored, what
	// the next one is checked against.
	StoreBytes int64 `json:"store_bytes,omitempty"`
	// ScrubDue (sqlite): a redaction finished then, and copies older than a
	// run started after it may hold its text. Set before the purge runs, kept
	// until one has, so it outlives a daemon stop; it holds no text.
	ScrubDue *time.Time `json:"scrub_due,omitempty"`
}

func (s *Service) homeStateFile(pid, prefix string) string {
	return filepath.Join(orchcore.Trackers(s.d).TrackerDir(pid, prefix), "home.json")
}

// homeStates serialises read-modify-write of home.json files.
type homeStates struct{ mu sync.Mutex }

func (s *Service) loadHome(pid, prefix string) (homeState, error) {
	var st homeState
	b, err := os.ReadFile(s.homeStateFile(pid, prefix))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s: %v", s.homeStateFile(pid, prefix), err)
	}
	return st, nil
}

// updateHome changes home.json under the lock (fn may return an error to
// write nothing).
func (s *Service) updateHome(pid, prefix string, fn func(*homeState) error) (homeState, error) {
	s.hs.mu.Lock()
	defer s.hs.mu.Unlock()
	st, err := s.loadHome(pid, prefix)
	if err != nil {
		return st, err
	}
	if err := fn(&st); err != nil {
		return st, err
	}
	path := s.homeStateFile(pid, prefix)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return st, err
	}
	return st, home.WriteJSON(path, st)
}

// homeOps holds the one restore, backup or scrub running on each tracker.
type homeOps struct {
	mu sync.Mutex
	m  map[string]string
}

// acquire takes tracker (pid, prefix) for what, or says what holds it.
func (h *homeOps) acquire(pid, prefix, what string) (func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := orchapi.TrackerKey(pid, prefix)
	if cur, busy := h.m[k]; busy {
		if pid == "" {
			return nil, api.Errorf(api.CodeConflict, "%s is busy: %s is running; try again when it ends", prefix, cur)
		}
		return nil, api.Errorf(api.CodeConflict, "tracker %s is busy: %s is running; try again when it ends", prefix, cur)
	}
	if h.m == nil {
		h.m = map[string]string{}
	}
	h.m[k] = what
	return func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.m, k)
	}, nil
}

// drop releases what acquire took (for callers holding several).
func (h *homeOps) drop(pid, prefix string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.m, orchapi.TrackerKey(pid, prefix))
}

func (h *homeOps) running(pid, prefix string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.m[orchapi.TrackerKey(pid, prefix)]
}

// homeCtx is a context for a backup or restore's side effects: the caller
// going away (a Ctrl-C on the CLI) must not stop one halfway.
func homeCtx(ctx context.Context) context.Context { return context.WithoutCancel(ctx) }

func timePtr(t time.Time) *time.Time { return &t }

// BlocksRemove refuses `tracker rm` while a backup or restore of the tracker
// runs.
func (s *Service) BlocksRemove(pid, prefix string) error {
	if what := s.ops.running(pid, prefix); what != "" {
		return api.Errorf(api.CodeConflict, "tracker %s is busy: %s is running", prefix, what)
	}
	return nil
}

// Forget drops a removed tracker's home state (a tracker added later under
// the same prefix starts clean; backup copies stay).
func (s *Service) Forget(pid, prefix string) {
	os.Remove(s.homeStateFile(pid, prefix))
}

// localScratch is where a restore stages copies on this machine (the
// directory is named "moves" for what first used it; sweepScratch looks
// there).
func (s *Service) localScratch(pid, prefix, id string) string {
	return filepath.Join(orchcore.Trackers(s.d).TrackerDir(pid, prefix), "moves", id)
}

// removeAllQuiet removes a staging directory, and only one under "moves".
func removeAllQuiet(p string) {
	if p != "" && strings.Contains(p, string(os.PathSeparator)+"moves"+string(os.PathSeparator)) {
		os.RemoveAll(p)
	}
}
