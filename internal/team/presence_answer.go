package team

import (
	"context"
	"github.com/sengirunners/potluck/kit/teamlink"
	"slices"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
)

// presenceAnswerContext keeps request ordering and role lifetime independent.
// Direct test callers without either have no claim of request freshness.
type presenceAnswerContext struct {
	leaseRequest uint64
	role         *teamlink.RoleReplyFence
}

// applyPresence records what an authority answered a heartbeat.
func (s *Service) applyPresence(ctx context.Context, snap api.Remote, local map[string]orchapi.Session, elsewhere map[string]bool,
	reported map[string][]string, resp orchapi.SessionsBeatAnswer, answers ...presenceAnswerContext) {
	type lostLease struct{ addr, key string }
	type delivery struct {
		addr string
		p    orchapi.SessionPrompt
	}
	var deliver []delivery
	var lost []lostLease
	var nudge, answered []string
	groupGrew := false
	var answer presenceAnswerContext
	if len(answers) > 0 {
		answer = answers[0]
	}
	current := func() bool { return answer.role == nil || answer.role.Current() }
	if !current() {
		return
	}
	// Session lookups may do SQL or call another module. Compute their
	// inputs before the short publication/cache critical section. A session
	// added meanwhile is conservatively left for the next heartbeat.
	cleanup := map[string]bool{}
	s.pmu.Lock()
	for addr := range s.cache.held[snap.Project.ID] {
		if _, sent := local[addr]; !sent {
			cleanup[addr] = false
		}
	}
	for addr := range s.cache.ended[snap.Project.ID] {
		if _, sent := local[addr]; !sent {
			cleanup[addr] = false
		}
	}
	s.pmu.Unlock()
	for addr := range cleanup {
		cleanup[addr] = s.inProject(addr, snap.Project.ID)
	}
	elsewhereSessions := map[string]orchapi.Session{}
	if orchcore.Sessions(s.d) != nil {
		for _, ans := range resp.Elsewhere {
			if elsewhere[ans.Address] && local[ans.Address].Address == "" {
				if ss, ok := orchcore.Sessions(s.d).Lookup(ctx, ans.Address); ok {
					elsewhereSessions[ans.Address] = ss
				}
			}
		}
	}
	for _, ans := range resp.Sessions {
		if _, ok := local[ans.Address]; ok && ans.Error != "" {
			s.d.Log.Debug("team presence: session refused", "session", ans.Address, "err", ans.Error)
		}
	}
	unguard := func() {}
	if answer.role != nil {
		var ok bool
		unguard, ok = answer.role.Guard()
		if !ok {
			return
		}
	}
	s.pmu.Lock()
	s.cache.init()
	byProject := map[string][]orchapi.NoticePart{}
	for _, ans := range resp.Sessions {
		addr := ans.Address
		if _, ok := local[addr]; !ok {
			continue
		}
		if ans.Error != "" {
			continue
		}
		now := map[string]bool{}
		for _, k := range ans.Held {
			now[k] = true
		}
		was := map[string]bool{}
		for _, k := range reported[addr] {
			was[k] = true
		}
		pid := snap.Project.ID
		cur := s.cache.held[pid][addr]
		next := now
		s.cache.dropEnded(pid, addr, now)
		for k := range cur {
			switch {
			case !was[k]: // claimed through the proxy after this heartbeat left
				next[k] = true
			case !now[k] && slices.Contains(ans.Finished, k):
				// The work is done: its PR merged and closed it. Not a loss.
			case !now[k]:
				// Reported as held, gone at the authority, and not released
				// through the proxy since: the lease is lost.
				lost = append(lost, lostLease{addr, k})
			}
		}
		if s.cache.held[pid] == nil {
			s.cache.held[pid] = map[string]map[string]bool{}
		}
		s.cache.held[pid][addr] = next
		s.cache.touchLeaseActor(pid, addr, snap.ScopeVersion, answer.leaseRequest)
		if len(next) == 0 {
			delete(s.cache.leaseIn[addr], pid)
		}
		byProject[addr] = ans.Notice
		d, g := 0, 0
		for _, p := range ans.Notice {
			d += p.Unread - p.Group - p.Announce - p.Quiet
			g += p.Group
		}
		if d > s.cache.direct[addr] {
			nudge = append(nudge, addr)
		}
		if g > s.cache.group[addr] {
			groupGrew = true
		}
		s.cache.direct[addr], s.cache.group[addr] = d, g
		if ss := local[addr]; ans.Waiting == 0 && ss.State == orchapi.StateNeedsHuman && strings.HasPrefix(ss.Note, "waiting on mail") {
			answered = append(answered, addr)
		}
		// Shared sessions (SPEC 17.10): a pending prompt this machine has no
		// record of (team_prompts_typed, kept across a restart) is recorded,
		// then typed, below, oldest first.
		for _, p := range ans.Prompts {
			deliver = append(deliver, delivery{addr, p})
		}
	}
	byProject[s.d.Config.HumanAddress()] = resp.Human
	// Sessions registered outside the project: their mail in it shows in
	// their notice and nudges them like the project's own sessions'. Their
	// counts are kept per project (one may be outside several).
	took := map[string]bool{}
	for _, ans := range resp.Elsewhere {
		addr := ans.Address
		if !elsewhere[addr] || local[addr].Address != "" {
			continue
		}
		took[addr] = true
		byProject[addr] = ans.Notice
		d := 0
		for _, p := range ans.Notice {
			d += p.Unread - p.Group - p.Announce - p.Quiet
		}
		k := elsewhereKey(snap.Project.ID, addr)
		if d > s.cache.direct[k] {
			nudge = append(nudge, addr)
		}
		s.cache.direct[k], s.cache.waiting[k] = d, ans.Waiting
	}
	for k := range s.cache.direct {
		if p, addr, ok := strings.Cut(k, " "); ok && p == snap.Project.ID && !took[addr] {
			delete(s.cache.direct, k)
			delete(s.cache.waiting, k)
		}
	}
	// One that asked a human (needs=human mail sent through the proxy) is
	// answered once no project it is outside of says a mail is still open.
	for addr := range took {
		open := false
		for k, n := range s.cache.waiting {
			if _, a, _ := strings.Cut(k, " "); a == addr && n > 0 {
				open = true
			}
		}
		if open || orchcore.Sessions(s.d) == nil {
			continue
		}
		if ss := elsewhereSessions[addr]; ss.State == orchapi.StateNeedsHuman && strings.HasPrefix(ss.Note, "waiting on mail") {
			answered = append(answered, addr)
		}
	}
	s.cache.notices[snap.Project.ID] = byProject
	pid := snap.Project.ID
	for addr := range s.cache.held[pid] { // sessions gone from this project
		if _, ok := local[addr]; !ok && cleanup[addr] {
			delete(s.cache.held[pid], addr)
			s.cache.touchLeaseActor(pid, addr, snap.ScopeVersion, answer.leaseRequest)
			delete(s.cache.ended[pid], addr)
			delete(s.cache.direct, addr)
			delete(s.cache.group, addr)
		}
	}
	for addr := range s.cache.ended[pid] { // a session that never held a key here
		if _, ok := local[addr]; !ok && cleanup[addr] {
			delete(s.cache.ended[pid], addr)
		}
	}
	s.pmu.Unlock()
	unguard()
	if sp, ok := orchcore.Sessions(s.d).(orchcore.SharedPrompts); ok {
		typed := 0
		wctx := context.WithoutCancel(ctx)
		for _, d := range deliver {
			if !current() {
				break
			}
			if !s.claimTyping(wctx, snap.Project.ID, d.addr, d.p.ID) {
				continue // typed (or being typed) already, or not recordable: never twice
			}
			if !current() {
				s.typingDone(wctx, d.p.ID, false) // no typing began; safe to retry
				break
			}
			ok, err := sp.DeliverPrompt(wctx, d.addr, d.p)
			if err != nil {
				// Refused before typing: the claim goes, the next beat tries
				// again. Cut off while typing: the claim stays "typing", so it
				// is neither typed again nor reported delivered.
				if ok {
					s.d.Log.Warn("team: shared session prompt cut off", "session", d.addr, "prompt", d.p.ID, "err", err)
				} else {
					// Debug: a refusal repeats every beat until the prompt expires.
					s.d.Log.Debug("team: shared session prompt refused", "session", d.addr, "prompt", d.p.ID, "err", err)
					s.typingDone(wctx, d.p.ID, false)
				}
				continue
			}
			s.typingDone(wctx, d.p.ID, true)
			typed++
		}
		if typed > 0 {
			s.kickPresence() // report them delivered soon
		}
	}
	for _, addr := range answered {
		if !current() {
			break
		}
		s.setLocalState(addr, orchapi.StateIdle, "answered")
	}
	for _, l := range lost {
		s.d.Log.Info("team: lease lost", "session", l.addr, "key", l.key, "authority", snap.Authority)
	}
	if orchcore.Sessions(s.d) == nil {
		return
	}
	nudged := map[string]bool{}
	for _, addr := range nudge {
		if !current() {
			break
		}
		if ok, _ := orchcore.Sessions(s.d).Nudge(context.WithoutCancel(ctx), addr, NudgeLine); ok {
			nudged[addr] = true
		}
	}
	if groupGrew { // group mail wakes one idle managed session of the project
		for addr, ss := range local {
			if nudged[addr] || !ss.Managed || ss.State != orchapi.StateIdle {
				continue
			}
			if !current() {
				break
			}
			if ok, _ := orchcore.Sessions(s.d).Nudge(context.WithoutCancel(ctx), addr, NudgeLine); ok {
				break
			}
		}
	}
}
