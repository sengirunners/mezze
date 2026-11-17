package session

// Sessions outside a project (SPEC 9 "Across projects", 17.3 B2): on a team
// authority, each replica's heartbeat for a project also lists the addresses
// of its live sessions registered outside it (another project or none), so
// mail in the project can reach them. Addresses only, in memory: a restart
// forgets them until the next heartbeat (at most 15 s).

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/authz"
)

var _ orchcore.PresenceElsewhere = (*Service)(nil)

// elsewhereKeep is how long an address stays known after a heartbeat last
// listed it (like presence rows: an ended session still receives).
const elsewhereKeep = presenceKeep

type elsewhereRow struct {
	machine string
	token   string // the replica's token that listed it (send reads its scope now)
	seen    time.Time
	listed  bool // in the machine's latest set for the project
	whole   bool // what that token read of the project at the heartbeat
	areas   []string
}

// elsewhereSet is project -> address -> row.
type elsewhereSet struct {
	mu sync.Mutex
	by map[string]map[string]*elsewhereRow
	// beat is project -> machine -> its latest heartbeat (for Live).
	beat map[string]map[string]time.Time
}

// RecordElsewhere implements orchcore.PresenceElsewhere.
func (s *Service) RecordElsewhere(_ context.Context, project, machine, token string, addrs []string, whole bool, areas []string) {
	now := time.Now()
	e := &s.elsewhere
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.by == nil {
		e.by, e.beat = map[string]map[string]*elsewhereRow{}, map[string]map[string]time.Time{}
	}
	rows := e.by[project]
	if rows == nil {
		rows = map[string]*elsewhereRow{}
		e.by[project] = rows
	}
	if e.beat[project] == nil {
		e.beat[project] = map[string]time.Time{}
	}
	e.beat[project][machine] = now
	for addr, r := range rows {
		switch {
		case now.Sub(r.seen) > elsewhereKeep:
			delete(rows, addr)
		case r.machine == machine:
			r.listed = false
		}
	}
	areas = slices.Clone(areas)
	for _, a := range addrs {
		rows[a] = &elsewhereRow{machine: machine, token: token, seen: now, listed: true, whole: whole, areas: areas}
	}
}

// pruneElsewhere drops rows past the keep window, and machines' heartbeat
// times as old, with the maps they leave empty (from the session ticker: a
// project nobody heartbeats any more is never pruned by RecordElsewhere).
func (s *Service) pruneElsewhere() {
	now := time.Now()
	e := &s.elsewhere
	e.mu.Lock()
	defer e.mu.Unlock()
	for project, rows := range e.by {
		for addr, r := range rows {
			if now.Sub(r.seen) > elsewhereKeep {
				delete(rows, addr)
			}
		}
		if len(rows) == 0 {
			delete(e.by, project)
		}
	}
	for project, ms := range e.beat {
		for m, at := range ms {
			if now.Sub(at) > elsewhereKeep {
				delete(ms, m)
			}
		}
		if len(ms) == 0 {
			delete(e.beat, project)
		}
	}
}

// Elsewhere implements orchcore.PresenceElsewhere.
func (s *Service) Elsewhere(project, address string) (orchcore.ElsewhereSession, bool) {
	e := &s.elsewhere
	e.mu.Lock()
	defer e.mu.Unlock()
	r := e.by[project][address]
	if r == nil || time.Since(r.seen) > elsewhereKeep {
		return orchcore.ElsewhereSession{}, false
	}
	live := r.listed && time.Since(e.beat[project][r.machine]) <= s.presenceLost()
	return orchcore.ElsewhereSession{Live: live, Token: r.token, Whole: r.whole, Areas: slices.Clone(r.areas)}, true
}

// elsewhereVisible reports whether a reader with scope sc may see address
// as a replica's session registered outside a project its machine still
// joined: a reader of the whole project, or, for a replica that reads only
// some areas of it (a guest's), a reader of one of them (mail to it is filed
// there). The same readers may address it (mail's acrossProjects, SPEC 9).
func (s *Service) elsewhereVisible(ctx context.Context, sc authz.Scope, address string) bool {
	type cand struct {
		project, machine string
		whole            bool
		areas            []string
	}
	var cs []cand
	e := &s.elsewhere
	e.mu.Lock()
	for project, rows := range e.by {
		if r := rows[address]; r != nil && time.Since(r.seen) <= elsewhereKeep {
			cs = append(cs, cand{project, r.machine, r.whole, r.areas})
		}
	}
	e.mu.Unlock()
	for _, c := range cs {
		if all, shared := orchcore.ElsewhereShared(sc, c.project, c.whole, c.areas); (all || len(shared) > 0) &&
			(s.d.Projects == nil || slices.Contains(s.d.Projects.MachineProjects(ctx, c.machine), c.project)) {
			return true
		}
	}
	return false
}
