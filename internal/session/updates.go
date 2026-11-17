package session

// Coalesced dev.zm.session.updated (SPEC 8, 12): activity and term_title
// change with every tool call, and every event is stored in its project's
// log (the last 20 000 kept), so a busy agent would push mail and work
// history out of replay within hours. Per session at most one updated
// event is published per updatedEvery: a change inside the gap schedules a
// trailing publish of the session as it is then, so the last change still
// goes out. Any other event of the session carries the whole Session and
// cancels a pending trailing one. events.Input has no bus-only (unstored)
// publish, so this is the session module's own.

import (
	"context"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// updatedEvery is the least gap between two updated events of a session
// (tests shorten it).
var updatedEvery = 5 * time.Second

type updates struct {
	mu sync.Mutex
	m  map[string]*updateSlot // session address ->
}

type updateSlot struct {
	last  time.Time   // the last updated published
	timer *time.Timer // a trailing publish is scheduled
	// durable: a change the trailing publish carries is more than the
	// activity, so that event is stored (else it goes live only).
	durable bool
	// held: the session's started event is not out yet (spawn holds it from
	// before the proc is live), so an updated waits for it; pending: one did.
	held, pending bool
}

// hold makes address's updated events wait for its started event: from the
// holder's adoption on, the sampler, its title and the agent's hooks can
// update a session that spawn has not yet announced.
func (u *updates) hold(address string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.m == nil {
		u.m = map[string]*updateSlot{}
	}
	u.m[address] = &updateSlot{held: true}
}

// releaseUpdated lets address's updated events go out after its started
// event; one that waited goes out at once, as the session is then.
func (s *Service) releaseUpdated(address string) {
	u := &s.updates
	u.mu.Lock()
	defer u.mu.Unlock()
	sl := u.m[address]
	if sl == nil || !sl.held {
		return
	}
	sl.held = false
	if sl.pending {
		sl.pending = false
		sl.timer = time.AfterFunc(0, func() { s.trailingUpdated(address, sl) })
	}
}

// settle cancels a pending trailing updated of address (another event
// carried the session); gone forgets the session (it ended), always.
func (u *updates) settle(address string, gone bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	sl := u.m[address]
	if sl == nil {
		return
	}
	if sl.timer != nil && sl.timer.Stop() {
		sl.timer = nil
	}
	if gone {
		// Forget it even when its timer is firing right now: that trailing
		// publish finds its slot gone and does nothing.
		delete(u.m, address)
	}
}

// publishUpdated publishes ss's updated event now, or schedules one
// trailing publish (the session as it is then) within updatedEvery of the
// last. activity: the change is the activity alone, so the event is
// delivered live and not stored (unless the trailing publish also carries
// another change).
func (s *Service) publishUpdated(ss orchapi.Session, activity bool) {
	u := &s.updates
	u.mu.Lock()
	if u.m == nil {
		u.m = map[string]*updateSlot{}
	}
	sl := u.m[ss.Address]
	if sl == nil {
		sl = &updateSlot{}
		u.m[ss.Address] = sl
	}
	if sl.held { // releaseUpdated publishes it after started
		sl.pending, sl.durable = true, sl.durable || !activity
		u.mu.Unlock()
		return
	}
	if sl.timer != nil { // the trailing publish will carry this change
		sl.durable = sl.durable || !activity
		u.mu.Unlock()
		return
	}
	now := time.Now()
	if wait := updatedEvery - now.Sub(sl.last); wait > 0 {
		addr, slot := ss.Address, sl
		sl.durable = !activity
		sl.timer = time.AfterFunc(wait, func() { s.trailingUpdated(addr, slot) })
		u.mu.Unlock()
		return
	}
	sl.last = now
	u.mu.Unlock()
	s.publishEvent(orchapi.EventSessionUpdated, ss, activity)
}

// trailingUpdated publishes the session's updated event as it is now,
// unless slot was settled or forgotten meanwhile.
func (s *Service) trailingUpdated(address string, slot *updateSlot) {
	u := &s.updates
	u.mu.Lock()
	sl := u.m[address]
	if sl != slot || sl.timer == nil {
		u.mu.Unlock()
		return
	}
	durable := sl.durable
	sl.timer, sl.last, sl.durable = nil, time.Now(), false
	u.mu.Unlock()
	if s.ctx.Err() != nil {
		return
	}
	var ss orchapi.Session
	var ok bool
	if s.remoteAddr(address) {
		ss, ok = s.loadPresence(context.Background(), address)
	} else {
		var r row
		r, ok = s.load(context.Background(), address)
		ss = r.Session
	}
	if !ok || ss.State == orchapi.StateEnded {
		u.settle(address, true)
		return
	}
	s.publishEvent(orchapi.EventSessionUpdated, ss, !durable)
}
