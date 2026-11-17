package session

// The foreground-process and port sampler (SPEC 8): every sampleEvery, for
// each live managed session that has an attached client or that this
// machine's owner read by itself (GET /v0/sessions/{id}) within askedFor,
// read the PTY's foreground process group (tcgetpgrp) and the process tree
// under the session's process (one process-table read for all); the tree's
// listening TCP ports (one listenPorts call for all that need it) only
// every portsEvery, or sooner when the tree's set of processes changed, but
// never within portsSoon of the last read (an agent's tree changes with
// every tool run, and lsof on a loaded Mac costs a fifth of a core). A
// list (GET /v0/sessions, which a cockpit reloads on events) does not count as
// asking. Results live in memory on the proc; reads
// overlay them (dress) for the owner only, and a change publishes the
// coalesced dev.zm.session.updated. Never on a request's path.

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// Tunables (tests shorten them).
var (
	sampleEvery = 2 * time.Second
	portsEvery  = 10 * time.Second
	portsSoon   = 5 * time.Second
	askedFor    = 30 * time.Second
)

// readPorts is listenPorts and readProcTable procTable (tests replace them).
var (
	readPorts     = listenPorts
	readProcTable = procTable
)

// sample is what the sampler last saw of a session.
type sample struct {
	fg    *orchapi.SessionFg
	busy  bool
	ports []int
	jobs  *int // processes in the tree besides the session's own; nil = fg not sampled
}

func (a sample) equal(b sample) bool {
	if (a.fg == nil) != (b.fg == nil) || (a.fg != nil && *a.fg != *b.fg) {
		return false
	}
	if (a.jobs == nil) != (b.jobs == nil) || (a.jobs != nil && *a.jobs != *b.jobs) {
		return false
	}
	return a.busy == b.busy && slices.Equal(a.ports, b.ports)
}

// worthPublishing reports whether a sample change goes out as an event:
// any change of fg, busy or ports, but a change of jobs alone only when it
// crosses 0 <-> more (or appears or goes): an agent's helpers come and go
// with every tool run, and only "has jobs or not" matters to a reader that
// is not asking now (reads carry the exact count).
func worthPublishing(old, new sample) bool {
	o, n := old, new
	o.jobs, n.jobs = nil, nil
	if !o.equal(n) {
		return true
	}
	return jobsBucket(old.jobs) != jobsBucket(new.jobs)
}

// jobsBucket is -1 unknown, 0 none, 1 some.
func jobsBucket(j *int) int {
	switch {
	case j == nil:
		return -1
	case *j == 0:
		return 0
	}
	return 1
}

// asked records that this machine's owner read a session (GET), so the
// sampler keeps it fresh for askedFor.
func (s *Service) asked(id8 string) {
	if s.proc(id8) != nil {
		s.askedAt.Store(id8, time.Now())
	}
}

// due reports whether p should be sampled now.
func (s *Service) due(p *proc) bool {
	p.mu.Lock()
	attached := len(p.conns)+len(p.viewers) > 0
	p.mu.Unlock()
	if attached {
		return true
	}
	if v, ok := s.askedAt.Load(p.id8); ok {
		if time.Since(v.(time.Time)) < askedFor {
			return true
		}
		s.askedAt.Delete(p.id8)
	}
	return false
}

// sampleLoop runs the sampler until the service stops.
func (s *Service) sampleLoop(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(sampleEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-ctx.Done():
			return
		case <-s.ctx.Done():
			return
		}
		s.sampleOnce()
	}
}

// sampleOnce samples every due session once.
func (s *Service) sampleOnce() {
	var due []*proc
	var forgot []telling
	s.sampleMu.Lock() // the due decisions and forgetting, as one step against a GET ?sample=1
	for _, p := range s.live() {
		switch {
		case p.isExited():
		case s.due(p):
			due = append(due, p)
		default:
			if t, ok := s.forgetSample(p); ok {
				forgot = append(forgot, t)
			}
		}
	}
	s.sampleMu.Unlock()
	s.publishSamples(forgot)
	if len(due) > 0 {
		s.sampleProcs(due, true)
	}
}

// forgetSample drops what only a fresh sample may say (fg, busy, jobs) from
// a session nobody is looking at any more: served later, it would be stale
// ("jobs 0, not busy" from minutes ago). Ports, read rarely anyway, stay. A
// sample younger than sampleEvery (a GET ?sample=1 that has not answered
// yet) is kept. The caller holds sampleMu; ok: readers were told fg, busy
// or jobs, so an event must take them back (whatever the current sample
// holds: a GET may have replaced or cleared it meanwhile).
func (s *Service) forgetSample(p *proc) (telling, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if time.Since(p.sampledAt) < sampleEvery {
		return telling{}, false
	}
	p.sample.fg, p.sample.busy, p.sample.jobs = nil, false, nil
	if p.told.fg == nil && !p.told.busy && p.told.jobs == nil {
		return telling{}, false
	}
	t := telling{p: p, before: p.told}
	p.told.fg, p.told.busy, p.told.jobs = nil, false, nil
	t.after = p.told
	return t, true
}

// telling is one event the sampler is about to publish: told moves from
// before to after now (the event's data is read from told), and back if
// the event cannot go out (publishSamples), so the next cycle retries.
type telling struct {
	p             *proc
	before, after sample
}

// tell moves told to p's current sample when it differs from what readers
// were told (worthPublishing); the caller holds p.mu.
func (p *proc) tell() (telling, bool) {
	if !worthPublishing(p.told, p.sample) {
		return telling{}, false
	}
	t := telling{p: p, before: p.told, after: p.sample}
	p.told = p.sample
	return t, true
}

// tellings collects the events of one sampler pass, one per session (its
// first before, its last after).
type tellings []telling

func (ts *tellings) add(t telling) {
	for i := range *ts {
		if (*ts)[i].p == t.p {
			(*ts)[i].after = t.after
			return
		}
	}
	*ts = append(*ts, t)
}

// sampleNow samples one live managed session at once, without its ports
// (GET /v0/sessions/{id}?sample=1: a UI about to close it wants fg, busy
// and jobs as of now, not as of the last cycle). It neither makes the
// session due nor publishes: the asker gets the answer, and a one-off look
// must not start a cycle of events.
func (s *Service) sampleNow(id8 string) {
	p := s.proc(id8)
	switch {
	case p == nil: // no live process: reads overlay no sample at all
	case p.isExited():
		// Exited, its end not recorded yet: nothing to look at, and the old
		// look must not answer as fresh.
		s.sampleMu.Lock()
		p.mu.Lock()
		p.sample.fg, p.sample.busy, p.sample.jobs = nil, false, nil
		p.mu.Unlock()
		s.sampleMu.Unlock()
	default:
		s.sampleProcs([]*proc{p}, false)
	}
}

// sampleProcs samples the given sessions. cycle: the sampler's own pass,
// which reads the ports of sessions whose ports are due (else they stay as
// they were) and publishes changes; else a GET ?sample=1's, which does
// neither. sampleMu covers the process table, fg and jobs and each
// compare-and-store, never the ports read (lsof may take seconds), so a GET
// ?sample=1 never waits for it.
func (s *Service) sampleProcs(due []*proc, cycle bool) {
	withPorts := cycle
	var publish tellings
	s.sampleMu.Lock()
	table, err := readProcTable()
	if err != nil {
		// No fresh look: what the last one said must not be answered as
		// fresh (a GET ?sample=1 then answers without fg, busy and jobs,
		// and a UI asks).
		for _, p := range due {
			p.mu.Lock()
			p.sample.fg, p.sample.busy, p.sample.jobs = nil, false, nil
			if cycle {
				if t, ok := p.tell(); ok {
					publish.add(t)
				}
			}
			p.mu.Unlock()
		}
		s.sampleMu.Unlock()
		s.d.Log.Warn("session sample: process table", "err", err)
		if cycle {
			s.publishSamples(publish)
		}
		return
	}
	children := childrenOf(table)
	now := time.Now()
	trees := make([][]int, len(due))
	keys := make([]string, len(due))
	needPorts := make([]bool, len(due))
	var all []int
	for i, p := range due {
		var got sample
		leader := p.pid
		if pgrp, err := fgPgrp(p.pty); err == nil && pgrp > 0 {
			got.fg = &orchapi.SessionFg{PID: pgrp, Name: table[pgrp].name}
			got.busy = pgrp != leader
		}
		trees[i] = descendants(table, children, leader)
		if got.fg != nil {
			n := len(trees[i]) - 1 // descendants includes the leader itself
			got.jobs = &n
		}
		keys[i] = treeKey(trees[i])
		p.mu.Lock()
		p.sampledAt = now
		got.ports = p.sample.ports // ports change below, after their read
		since := now.Sub(p.portsAt)
		changed := keys[i] != p.treeKey && !p.portsErr
		needPorts[i] = withPorts && (p.portsAt.IsZero() || since >= portsEvery || (changed && since >= portsSoon))
		p.sample = got
		if cycle { // a GET's look is the asker's only: told untouched
			if t, ok := p.tell(); ok {
				publish.add(t)
			}
		}
		p.mu.Unlock()
		if needPorts[i] {
			all = append(all, trees[i]...)
		}
	}
	s.sampleMu.Unlock()

	if len(all) > 0 {
		byPid, err := readPorts(all)
		if err != nil {
			// A timeout (lsof on a loaded machine): the sampler goes on,
			// ports as they were, and tries again only after portsEvery,
			// not on the next cycle.
			s.d.Log.Warn("session sample: ports", "err", err)
			s.sampleMu.Lock()
			for i, p := range due {
				if needPorts[i] {
					p.mu.Lock()
					p.portsAt, p.treeKey, p.portsErr = now, keys[i], true
					p.mu.Unlock()
				}
			}
			s.sampleMu.Unlock()
		} else {
			s.sampleMu.Lock()
			for i, p := range due {
				if !needPorts[i] {
					continue
				}
				var ports []int
				seen := map[int]bool{}
				for _, pid := range trees[i] {
					for _, port := range byPid[pid] {
						if !seen[port] {
							seen[port] = true
							ports = append(ports, port)
						}
					}
				}
				sort.Ints(ports)
				p.mu.Lock()
				p.portsAt, p.treeKey, p.portsErr = now, keys[i], false
				p.sample.ports = ports
				if t, ok := p.tell(); ok {
					publish.add(t)
				}
				p.mu.Unlock()
			}
			s.sampleMu.Unlock()
		}
	}
	if cycle {
		s.publishSamples(publish)
	}
}

// publishSamples publishes the updated event of each session whose told
// moved. When the session's row cannot be read the event does not go out,
// so told goes back (unless it moved again meanwhile) and the next cycle
// retries; readers are never assumed told what they were not.
func (s *Service) publishSamples(ts []telling) {
	for _, t := range ts {
		r, ok := s.load(s.ctx, t.p.id8)
		if !ok {
			t.p.mu.Lock()
			if t.p.told.equal(t.after) {
				t.p.told = t.before
			}
			t.p.mu.Unlock()
			continue
		}
		if r.State != orchapi.StateEnded { // an ended session's own event says so
			s.publish(orchapi.EventSessionUpdated, r.Session)
		}
	}
}

// treeKey identifies a process tree's set of pids (sorted).
func treeKey(pids []int) string {
	b := make([]byte, 0, len(pids)*6)
	for _, p := range pids {
		b = strconv.AppendInt(b, int64(p), 10)
		b = append(b, ',')
	}
	return string(b)
}

// withSample overlays a live managed session's last sample on ss: told,
// what the sampler published, for an event (so a trailing coalesced event
// carries what told records), else the freshest look. An event's jobs is
// told's, which can lag the real count inside the 0 / more bucket (a jobs
// change that stays on one side of 0 publishes nothing, worthPublishing);
// the exact count comes only from reads.
func (s *Service) withSample(ss orchapi.Session, told bool) orchapi.Session {
	if !ss.Managed || ss.State == orchapi.StateEnded {
		return ss
	}
	p := s.proc(ss.ID)
	if p == nil {
		return ss
	}
	p.mu.Lock()
	sm := p.sample
	if told {
		sm = p.told
	}
	p.mu.Unlock()
	if sm.fg != nil {
		fg := *sm.fg
		ss.Fg = &fg
	}
	ss.Busy, ss.Ports = sm.busy, slices.Clone(sm.ports)
	if sm.jobs != nil {
		n := *sm.jobs
		ss.Jobs = &n
	}
	return ss
}
