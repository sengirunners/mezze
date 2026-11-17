package team

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
)

// The query runs in the canonical transaction; the callback runs only after
// its commit, with snapshotMu and writing held. No raw writer reply or pending
// intent is allowed to seed the presence cache.
type leaseCacheCommit struct {
	read      func(context.Context, *sql.Tx) error
	committed func()
	now       time.Time
}

func firstLeaseCommit(commits []*leaseCacheCommit) *leaseCacheCommit {
	if len(commits) == 0 {
		return nil
	}
	return commits[0]
}

func (s *Service) prepareLeaseCache(x *remote, snap api.Remote) *leaseCacheCommit {
	s.pmu.Lock()
	s.cache.init()
	// Register even an empty project before I/O, so a release of a lease
	// first learned by this stream still leaves a project-specific tombstone.
	if s.cache.held[snap.Project.ID] == nil {
		s.cache.held[snap.Project.ID] = map[string]map[string]bool{}
	}
	before := s.cache.admitLeaseRequest()
	s.pmu.Unlock()
	canonical := map[string]map[string]bool{}
	c := &leaseCacheCommit{}
	c.read = func(ctx context.Context, tx *sql.Tx) error {
		now := c.now
		if now.IsZero() {
			now = time.Now()
		}
		rows, err := tx.QueryContext(ctx, `SELECT json FROM leases WHERE bound = 1 OR expires > ?`, store.Millis(now))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var js string
			if err := rows.Scan(&js); err != nil {
				return err
			}
			var l orchapi.Lease
			if err := json.Unmarshal([]byte(js), &l); err != nil {
				return err
			}
			a, err := api.ParseAddress(l.Holder)
			if err != nil || a.Kind != api.KindSession || a.Machine != s.d.Config.Machine || l.Key == "" || l.Project != snap.Project.ID {
				continue
			}
			if canonical[l.Holder] == nil {
				canonical[l.Holder] = map[string]bool{}
			}
			canonical[l.Holder][l.Key] = true
		}
		return rows.Err()
	}
	c.committed = func() {
		// Keep the version check and publication atomic against observe. A
		// changed scope requests another snapshot; this old stream cannot
		// repopulate the cache while that snapshot is pending.
		x.Mu.Lock()
		defer x.Mu.Unlock()
		if x.R.ScopeVersion != snap.ScopeVersion || x.R.State != api.RemoteOK || x.R.Cap != snap.Cap {
			return
		}
		s.pmu.Lock()
		s.cache.reconcileLeases(snap.Project.ID, canonical, before, snap.ScopeVersion)
		s.pmu.Unlock()
	}
	return c
}

// Lease-cache revisions include deletions: comparing values before and after
// a streamed snapshot would miss a claim/release/claim in between (ABA).
// The presence mutex protects these maps together with held and leaseIn.
type presenceLeaseActor struct{ project, address string }
type presenceLeaseKey struct{ project, address, key string }
type presenceLeaseAnswer struct {
	revision uint64
	request  uint64
	scope    int64
}

type presenceLeaseClock struct {
	next   uint64
	keys   map[presenceLeaseKey]uint64
	actors map[presenceLeaseActor]presenceLeaseAnswer
	routes map[presenceLeaseActor]uint64
}

func (c *replicaCache) touchLease(project, address, key string) {
	if c.leaseClock.keys == nil {
		c.leaseClock.keys = map[presenceLeaseKey]uint64{}
	}
	c.leaseClock.next++
	c.leaseClock.keys[presenceLeaseKey{project, address, key}] = c.leaseClock.next
}

// admitLeaseRequest orders request starts even when no cache value changes
// between them. Response arrival order cannot establish authority freshness.
func (c *replicaCache) admitLeaseRequest() uint64 {
	c.leaseClock.next++
	return c.leaseClock.next
}

func (c *replicaCache) touchLeaseActor(project, address string, scope int64, request uint64) {
	if c.leaseClock.actors == nil {
		c.leaseClock.actors = map[presenceLeaseActor]presenceLeaseAnswer{}
	}
	c.leaseClock.next++
	c.leaseClock.actors[presenceLeaseActor{project, address}] = presenceLeaseAnswer{c.leaseClock.next, request, scope}
}

func (c *replicaCache) touchLeaseRoute(project, address string) {
	if c.leaseClock.routes == nil {
		c.leaseClock.routes = map[presenceLeaseActor]uint64{}
	}
	c.leaseClock.next++
	c.leaseClock.routes[presenceLeaseActor{project, address}] = c.leaseClock.next
}

func (c *replicaCache) leaseTouched(project, address, key string, before uint64) bool {
	for k, rev := range c.leaseClock.keys {
		if rev > before && k.project == project && k.address == address && (keyIs(key, k.key) || keyIs(k.key, key)) {
			return true
		}
	}
	return false
}

// reconcileLeases takes accepted leases from one committed canonical copy.
// Pending outbox intents never enter this view. A later proxy answer or
// heartbeat wins over a snapshot/page that started before that answer.
func (c *replicaCache) reconcileLeases(project string, canonical map[string]map[string]bool, before uint64, scope int64) {
	c.init()
	addresses := map[string]bool{}
	for addr := range canonical {
		addresses[addr] = true
	}
	for addr := range c.held[project] {
		addresses[addr] = true
	}
	for addr, projects := range c.leaseIn {
		if projects[project] {
			addresses[addr] = true
		}
	}
	if c.held[project] == nil {
		c.held[project] = map[string]map[string]bool{}
	}
	for addr := range addresses {
		a := presenceLeaseActor{project, addr}
		if answer := c.leaseClock.actors[a]; answer.request > before && answer.scope == scope {
			continue // a newer authority heartbeat replaced the actor's set
		}
		next := map[string]bool{}
		for key := range canonical[addr] {
			if !c.leaseTouched(project, addr, key, before) {
				next[key] = true
			}
		}
		for key := range c.held[project][addr] {
			if c.leaseTouched(project, addr, key, before) {
				next[key] = true
			}
		}
		c.held[project][addr] = next
		if len(next) > 0 {
			if c.leaseIn[addr] == nil {
				c.leaseIn[addr] = map[string]bool{}
			}
			c.leaseIn[addr][project] = true
		} else if c.leaseClock.routes[a] <= before {
			delete(c.leaseIn[addr], project)
		}
	}
	// A successful canonical commit consumes the old revisions; retain only
	// writes that overlapped this request for the next page/snapshot.
	for k, rev := range c.leaseClock.keys {
		if k.project == project && rev <= before {
			delete(c.leaseClock.keys, k)
		}
	}
	for k, answer := range c.leaseClock.actors {
		if k.project == project && answer.revision <= before {
			delete(c.leaseClock.actors, k)
		}
	}
	for k, rev := range c.leaseClock.routes {
		if k.project == project && rev <= before {
			delete(c.leaseClock.routes, k)
		}
	}
}
