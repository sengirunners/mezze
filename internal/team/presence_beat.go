package team

// The heartbeat's "sessions" part (teamlink.BeatPart): on a replica, the
// project's live sessions, ended outcomes, prompt acks and the keys each
// holds; on the authority, recording them (orchcore.PresenceStore) and
// answering each session's held keys, notices and pending prompts.
// teamlink runs the loop, the body limit and the machine ops.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"sync/atomic"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// partSessions is the sessions part's name.
const partSessions = "sessions"

// beatApplyDelay holds a heartbeat's answer before it is applied (tests:
// DelayBeatApply).
var beatApplyDelay atomic.Int64

// registerBeatParts registers team's heartbeat parts with teamlink.
func (s *Service) registerBeatParts() {
	s.RegisterBeatPart(partSessions, teamlink.BeatPart{
		Kick:    []string{"dev.zm.session."},
		Request: s.sessionsRequest,
		Serve:   s.serveSessions,
		Gone:    s.forgetNotices, // its mail is out of reach now
	})
}

// kickPresence asks for a heartbeat soon (never blocks).
func (s *Service) kickPresence() { s.KickBeat() }

// ---- the authority's side ----

// serveSessions answers a beat's sessions part.
func (s *Service) serveSessions(ctx context.Context, p api.Project, v json.RawMessage) (json.RawMessage, error) {
	if v == nil {
		return nil, nil // a machine without sessions (no session module)
	}
	sub := core.SubjectOf(ctx)
	// Unknown fields are tolerated (unlike other routes).
	var req orchapi.SessionsBeat
	if err := json.Unmarshal(v, &req); err != nil {
		return nil, api.Errorf(api.CodeBadRequest, "invalid sessions part: %v", err)
	}
	if err := checkPresenceSize(req); err != nil {
		return nil, err
	}
	ps, ok := orchcore.Sessions(s.d).(orchcore.PresenceStore)
	if !ok {
		return nil, api.Errorf(api.CodeUnavailable, "the session module keeps no presence")
	}
	// Machine names are unique per authority: every address is on the name
	// this token was redeemed for (Tailscale renaming the node changes
	// nothing).
	addrs := make([]string, 0, len(req.Sessions)+len(req.Elsewhere))
	for _, x := range req.Sessions {
		addrs = append(addrs, x.Address)
	}
	for _, x := range req.Ended {
		addrs = append(addrs, x.Address)
	}
	addrs = append(addrs, req.Elsewhere...)
	for _, x := range addrs {
		a, err := api.ParseAddress(x)
		if err != nil || a.Kind != api.KindSession {
			return nil, api.Errorf(api.CodeBadRequest, "%q is not a session address", x)
		}
		if a.Machine != sub.Machine {
			return nil, api.Errorf(api.CodeForbidden, "%s is not on %s, the machine this replica joined as", a.String(), sub.Machine).
				WithHint("machine names are unique per authority; a replica reports only its own sessions")
		}
	}
	defArea := ""
	if whole, areas := sub.Scope.Areas(p.ID); !whole && len(areas) == 1 {
		defArea = areas[0] // a guest's sessions are in its area
	}
	// Prompts the replica typed into its shared sessions are delivered,
	// whatever else this heartbeat says of each session (one that ended right
	// after typing comes as an ended entry, refused below): marked before
	// the machine's absent sessions are ended, which expires what is still
	// pending.
	if sp, ok := orchcore.Sessions(s.d).(orchcore.SharedPrompts); ok {
		for _, x := range req.Sessions {
			if len(x.Delivered) > 0 {
				a, _ := api.ParseAddress(x.Address)
				sp.MarkDelivered(ctx, p.ID, sub.Machine, a.String(), x.Delivered)
			}
		}
	}
	// Answers keep the request's order.
	out := orchapi.SessionsBeatAnswer{Sessions: make([]orchapi.PresenceAnswer, len(req.Sessions))}
	var keep []orchapi.PresenceSession
	var at []int
	for i, x := range req.Sessions {
		a, _ := api.ParseAddress(x.Address)
		x.Address = a.String()
		out.Sessions[i] = orchapi.PresenceAnswer{Address: x.Address, Held: []string{}}
		if x.Elsewhere {
			keep, at = append(keep, orchapi.PresenceSession{Address: x.Address, Elsewhere: true}), append(at, i)
			continue
		}
		if x.Area == "" {
			x.Area = defArea
		}
		switch x.State {
		case orchapi.StateRunning, orchapi.StateIdle, orchapi.StateNeedsHuman:
		default:
			out.Sessions[i].Error = "state must be running, idle or needs_human (an ended session is left out)"
			continue
		}
		if (x.Area != "" && !s.d.Projects.HasArea(p.ID, x.Area)) || !sub.Can(authz.Write, authz.Object{Project: p.ID, Area: x.Area}) {
			// An area the replica may not write in answers like a missing one.
			out.Sessions[i].Error = api.NotFound("area", x.Area).Error()
			continue
		}
		keep, at = append(keep, x), append(at, i)
	}
	live := map[string]bool{}
	for _, x := range keep {
		live[x.Address] = true
	}
	for _, x := range req.Ended {
		a, _ := api.ParseAddress(x.Address)
		x.Address = a.String()
		if live[x.Address] || x.OutcomeKind == "" {
			continue
		}
		if x.Area == "" {
			x.Area = defArea
		}
		if (x.Area != "" && !s.d.Projects.HasArea(p.ID, x.Area)) || !sub.Can(authz.Write, authz.Object{Project: p.ID, Area: x.Area}) {
			continue
		}
		x.State = orchapi.StateEnded
		keep = append(keep, x)
	}
	if err := ps.RecordPresence(ctx, p.ID, sub.Machine, sub.Identity.Name, keep); err != nil {
		return nil, err
	}
	s.lostCopiesSeen(ctx, p.ID, sub.Machine, live) // a lost task copy no longer live there (task_line.go)
	// Sessions registered outside the project (SPEC 9 "Across projects"):
	// addresses only, so mail in the project reaches them. One also listed
	// as the project's own is the project's.
	var elsewhere []string
	if pe, ok := orchcore.Sessions(s.d).(orchcore.PresenceElsewhere); ok && req.Elsewhere != nil {
		own := map[string]bool{}
		seen := map[string]bool{}
		for _, x := range req.Sessions {
			if x.Elsewhere {
				continue // present in another project of this authority: not this one's own
			}
			a, _ := api.ParseAddress(x.Address)
			own[a.String()] = true
		}
		for _, x := range req.Elsewhere {
			a, _ := api.ParseAddress(x)
			if !own[a.String()] && !seen[a.String()] {
				elsewhere = append(elsewhere, a.String())
				seen[a.String()] = true
			}
		}
		whole, areas := sub.Scope.Areas(p.ID)
		pe.RecordElsewhere(ctx, p.ID, sub.Machine, sub.TokenID, elsewhere, whole, areas)
	}
	lh, _ := orchcore.Work(s.d).(orchcore.LeaseHolders)
	tm, _ := orchcore.Mail(s.d).(orchcore.TeamMail)
	only := func(parts []orchapi.NoticePart) []orchapi.NoticePart {
		var o []orchapi.NoticePart
		for _, x := range parts {
			if x.Project == p.ID {
				o = append(o, x)
			}
		}
		return o
	}
	sp, _ := orchcore.Sessions(s.d).(orchcore.SharedPrompts)
	s.streams.prune()
	for k, x := range keep {
		if x.Elsewhere || x.State == orchapi.StateEnded {
			continue
		}
		ans := &out.Sessions[at[k]]
		if sp != nil && x.Shared && x.Managed && x.Mode != orchapi.SessionModePipe { // shared sessions (SPEC 17.10): pending prompts out
			ans.Prompts = sp.PendingPrompts(ctx, x.Address)
		}
		ans.Stream = s.streamWant(p.ID, x) // a member reads its events here
		if lh != nil {
			if held, err := lh.HeldBy(ctx, p.ID, x.Address); err == nil {
				ans.Held = held
				ans.Finished = lh.FinishedBy(p.ID, x.Address, x.Work)
			}
		}
		if tm != nil {
			if parts, err := tm.NoticeParts(ctx, sub.ForSession(x.Address)); err == nil {
				ans.Notice = only(parts)
			}
			ans.Waiting, _ = tm.OpenAsks(ctx, p.ID, x.Address)
		}
	}
	if tm != nil {
		if parts, err := tm.NoticeParts(ctx, sub); err == nil {
			out.Human = only(parts)
		}
		for _, addr := range elsewhere {
			ans := orchapi.PresenceAnswer{Address: addr, Held: []string{}}
			if parts, err := tm.NoticeParts(ctx, sub.ForSession(addr)); err == nil {
				ans.Notice = only(parts)
			}
			ans.Waiting, _ = tm.OpenAsks(ctx, p.ID, addr)
			out.Elsewhere = append(out.Elsewhere, ans)
		}
	}
	return json.Marshal(out)
}

// checkPresenceSize bounds both the replacement set and nested work/ack
// processing before the authority changes presence or acknowledges prompts.
func checkPresenceSize(req orchapi.SessionsBeat) error {
	if len(req.Sessions) > maxPresenceSessions || len(req.Ended) > maxPresenceEnded || len(req.Elsewhere) > maxPresenceLive ||
		len(req.Sessions)+len(req.Ended)+len(req.Elsewhere) > maxPresenceRows {
		return api.Errorf(api.CodeTooLarge, "presence inventory exceeds its row limits")
	}
	live, ended, acks, work := 0, 0, 0, 0
	for _, ss := range req.Sessions {
		if ss.State == orchapi.StateEnded && !ss.Elsewhere {
			ended++
		} else {
			live++
		}
		acks += len(ss.Delivered)
		work += len(ss.Work)
	}
	for _, ss := range req.Ended {
		acks += len(ss.Delivered)
		work += len(ss.Work)
	}
	if live > maxPresenceLive || ended > maxPresenceAckSessions || acks > maxPresenceAcks || work > maxPresenceWork {
		return api.Errorf(api.CodeTooLarge, "presence inventory exceeds its live-session, work or acknowledgment limits")
	}
	return nil
}

// ---- the replica's side ----

// sessionLister is the session module's List.
type sessionLister interface {
	List(ctx context.Context, sc authz.Scope, q orchapi.SessionQuery) ([]orchapi.Session, error)
}

// presenceSessions asks for a complete local inventory, independently of UI
// pagination. A legacy source whose result could be a full page is unsafe:
// sending it as a replacement would end sessions it silently omitted.
func (s *Service) presenceSessions(ctx context.Context) ([]orchapi.Session, error) {
	if source, ok := orchcore.Sessions(s.d).(orchcore.PresenceSource); ok {
		return source.PresenceSessions(ctx, maxPresenceLive, maxPresenceEnded)
	}
	sl, ok := orchcore.Sessions(s.d).(sessionLister)
	if !ok {
		return nil, api.Errorf(api.CodeUnavailable, "the session module has no complete presence source")
	}
	all, err := sl.List(ctx, authz.All(), orchapi.SessionQuery{})
	if err != nil {
		return nil, err
	}
	if len(all) >= 500 {
		return nil, api.Errorf(api.CodeUnavailable, "the legacy session list may be truncated; a complete presence source is required")
	}
	ended, err := sl.List(ctx, authz.All(), orchapi.SessionQuery{State: orchapi.StateEnded, All: true, Limit: maxPresenceEnded})
	if err != nil {
		return nil, err
	}
	return append(all, ended...), nil
}

// sessionsRequest is the sessions part of one beat to b's authority, and
// what applies the answer.
func (s *Service) sessionsRequest(ctx context.Context, b teamlink.Beat) (json.RawMessage, func(context.Context, json.RawMessage), error) {
	if time.Since(time.Unix(0, s.prunedTyped.Load())) > time.Hour {
		s.prunedTyped.Store(time.Now().UnixNano())
		s.pruneTyped(ctx)
	}
	x, fence, snap := b.Remote, b.Fence, b.Snap
	// Presence replaces the authority's inventory. A snapshot taken before
	// teamlink took the beat's lock can follow a newer beat and end its new
	// sessions (releasing their bound leases). Read the complete source under
	// the same lock that orders sends; never trust an inventory supplied by
	// a caller.
	all, err := s.presenceSessions(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list sessions: %w", err)
	}
	req := orchapi.SessionsBeat{Sessions: []orchapi.PresenceSession{}, Elsewhere: []string{}}
	reported := map[string][]string{}
	local := map[string]orchapi.Session{}
	unacked := s.unacked(ctx, snap.Project.ID) // typed prompts the authority has not heard of (SPEC 17.10)
	sharedLive := false
	same := map[string]bool{} // the joined projects this authority serves
	for _, y := range s.List() {
		if ys := y.Snapshot(); ys.URL == snap.URL {
			same[ys.Project.ID] = true
		}
	}
	s.pmu.Lock()
	s.cache.init()
	leaseRequest := s.cache.admitLeaseRequest()
	elsewhere := map[string]bool{}
	for _, ss := range all {
		if ss.Machine != s.d.Config.Machine {
			continue
		}
		if ss.State == orchapi.StateEnded {
			if ss.Project == snap.Project.ID && ss.Outcome != nil && ss.Ended != nil && time.Since(*ss.Ended) < 24*time.Hour && len(req.Ended) < maxPresenceEnded {
				req.Ended = append(req.Ended, orchapi.PresenceSession{Address: ss.Address, Area: ss.Area, Agent: ss.Agent, Title: ss.Title, Parent: ss.Parent, Started: ss.Started, OutcomeKind: ss.Outcome.Kind, EndedAt: ss.Ended,
					Account: ss.Account, For: ss.For, JobKind: ss.JobKind, Tokens: ss.Tokens})
			}
			continue
		}
		if in := s.cache.presentIn(ss, same); in != snap.Project.ID {
			if ss.Project != snap.Project.ID {
				// Registered outside this project: its address, so the
				// project's mail reaches it (SPEC 9 "Across projects"); never
				// costs the heartbeat itself.
				req.Elsewhere = append(req.Elsewhere, ss.Address)
				elsewhere[ss.Address] = true
			}
			if in != "" {
				// Present in another project of this authority: named here, so
				// this heartbeat never takes it for gone while its row moves.
				req.Sessions = append(req.Sessions, orchapi.PresenceSession{Address: ss.Address, Elsewhere: true})
			}
			continue
		}
		if ss.Project != snap.Project.ID {
			ss.Area = "" // registered elsewhere: project-level here
		}
		// This project's keys only: the row (work) is read by this project's
		// members, and this project's answer confirms only these.
		var work []string
		for k := range s.cache.held[snap.Project.ID][ss.Address] {
			work = append(work, k)
		}
		sort.Strings(work)
		reported[ss.Address] = work
		local[ss.Address] = ss
		title := ""
		if orchapi.SharesTermTitle(ss.Agent, ss.Command) {
			title = ss.TermTitle // a shell's titles (preexec, vim) carry command lines, paths and hosts: never sent
		}
		req.Sessions = append(req.Sessions, orchapi.PresenceSession{Address: ss.Address, Area: ss.Area, Agent: ss.Agent, Title: ss.Title, Parent: ss.Parent,
			State: ss.State, Note: ss.Note, Managed: ss.Managed, Started: ss.Started, Work: work,
			Activity: ss.Activity, ActivityAt: ss.ActivityAt, TermTitle: title,
			Shared: ss.Shared, InUse: ss.InUse, Account: ss.Account, For: ss.For, JobKind: ss.JobKind, Tokens: ss.Tokens})
		if ss.Shared && ss.Mode == orchapi.SessionModePipe {
			n := len(req.Sessions) - 1
			req.Sessions[n].Mode, req.Sessions[n].Capabilities = ss.Mode, ss.Capabilities
		}
		if ss.Shared && ss.Managed {
			sharedLive = true
		}
	}
	s.pmu.Unlock()
	if wr, ok := orchcore.Sessions(s.d).(orchcore.SessionWorkRecorder); ok {
		for addr, keys := range reported {
			if len(keys) > 0 && !slices.Contains(keys, local[addr].Work) {
				wr.RecordWork(ctx, addr, keys[0])
			}
		}
	}
	sentAcks := boundedPresenceAcks(unacked, local)
	for i := range req.Sessions {
		if !req.Sessions[i].Elsewhere {
			req.Sessions[i].Delivered = sentAcks[req.Sessions[i].Address]
		}
	}
	// Acks for sessions that ended right after a prompt was typed.
	req.Sessions = append(req.Sessions, endedAcks(sentAcks, local)...)
	if err := checkPresenceSize(req); err != nil {
		return nil, nil, fmt.Errorf("inventory not sent: %w", err)
	}
	v, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	apply := func(ctx context.Context, answer json.RawMessage) {
		if answer == nil {
			return // an authority without sessions took none of it
		}
		var resp orchapi.SessionsBeatAnswer
		if err := json.Unmarshal(answer, &resp); err != nil {
			s.d.Log.Warn("team presence: answer", "project", snap.Project.Slug, "err", err)
			return
		}
		var acked []string
		for _, ids := range sentAcks {
			acked = append(acked, ids...)
		}
		s.markAcked(ctx, acked) // the authority took them
		if sharedLive {
			s.BeatFast() // its prompts arrive with beats (SPEC 17.10)
		}
		if d := beatApplyDelay.Load(); d > 0 {
			time.Sleep(time.Duration(d)) // tests: an answer that arrives late
		}
		s.applyPresence(ctx, snap, local, elsewhere, reported, resp, presenceAnswerContext{leaseRequest: leaseRequest, role: &fence})
		if fence.Current() {
			s.pushStreams(x, snap.Project.ID, local, resp) // pages the authority's readers wait for
		}
	}
	return v, apply, nil
}
