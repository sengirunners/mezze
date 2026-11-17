package team

// Round B2 (SPEC 17.3 "Mail and presence across machines"): presence and
// the replica's side of cross-machine mail.
//
// On the authority, POST /v0/team/presence takes a replica's heartbeat for
// one team project (its node-bound token only; every address on the machine
// name recorded at redemption; every area one the replica may write in),
// stores it through the session module (orchcore.PresenceStore) and answers,
// per session, the keys it holds, its unread mail as notice parts and how
// many needs=human mails it sent are still open.
//
// teamlink runs the heartbeat (its loop, route, body limit and machine ops);
// this is its "sessions" part (presence_beat.go). On the replica, a
// heartbeat goes to each joined authority every teamlink.BeatEvery and soon
// after any local session event; the answers feed an
// in-memory cache that the mail module's notice reads (orchcore.TeamReplica):
// hooks never wait on the tailnet. A key the replica saw its session claim
// that the authority no longer has it holding is a lost lease: it is logged
// and dropped from what the replica reports it holds. Losing a lease does
// not change the session's state or turn an expiry into a human request.

import (
	"context"
	"github.com/sengirunners/potluck/kit/teamlink"
	"slices"
	"sort"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// Presence is one complete replacement, never independent pages. Reserve
// separate space for acknowledgments from sessions that ended after typing.
const (
	maxPresenceLive        = 10000
	maxPresenceEnded       = 500
	maxPresenceAckSessions = 500
	maxPresenceSessions    = maxPresenceLive + maxPresenceAckSessions
	maxPresenceRows        = maxPresenceSessions + maxPresenceLive + maxPresenceEnded
	maxPresenceAcks        = 10000
	maxPresenceWork        = 100000
)

// NudgeLine is the fixed line typed into an idle managed session when its
// team mail grew (the same line the mail module types for local mail).
const NudgeLine = "zm: you have mail. Run: zm inbox"

var _ orchcore.TeamReplica = (*Service)(nil)

// replicaCache is the replica's view of what its authorities said.
type replicaCache struct {
	// notices[project][address]: unread team mail per local address.
	notices map[string]map[string][]orchapi.NoticePart
	// held[project][address][key]: keys the session holds in the project
	// (claims seen through the proxy, merged with that project's heartbeat
	// answers). Per project: one project's answer says nothing of another's.
	held map[string]map[string]map[string]bool
	// ended[project][address][id]: leases the session ended itself (a key, or
	// a native issue's id, the key's last part), until that project's answer
	// stops listing them: a heartbeat in flight during the write must not
	// bring one back to be reported lost.
	ended map[string]map[string]map[string]bool
	// direct/group unread counts per address at the last answer (nudges).
	direct, group map[string]int
	// leaseIn[address][project]: a session claimed in this joined project
	// though registered in another (cwd outside the repo, --project): it is
	// in a heartbeat to the project's authority while that project's answer
	// says it holds a key, so its bound leases follow it there (SPEC 10).
	leaseIn map[string]map[string]bool
	// waiting[elsewhereKey]: open needs=human mails a session registered
	// outside a project sent in it, at the last answer.
	waiting    map[string]int
	leaseClock presenceLeaseClock // private lease mutations overlapping sync I/O
}

func (c *replicaCache) init() {
	if c.notices == nil {
		c.notices = map[string]map[string][]orchapi.NoticePart{}
		c.held = map[string]map[string]map[string]bool{}
		c.ended = map[string]map[string]map[string]bool{}
		c.leaseIn = map[string]map[string]bool{}
		c.direct, c.group, c.waiting = map[string]int{}, map[string]int{}, map[string]int{}
	}
}

// mark sets m[a][b][k] (creating the maps).
func mark(m map[string]map[string]map[string]bool, a, b, k string) {
	if m[a] == nil {
		m[a] = map[string]map[string]bool{}
	}
	if m[a][b] == nil {
		m[a][b] = map[string]bool{}
	}
	m[a][b][k] = true
}

// claimedIn records that addr holds key in project (a claim answered).
func (c *replicaCache) claimedIn(project, addr, key string) {
	c.init()
	if project == "" {
		return
	}
	if key != "" {
		mark(c.held, project, addr, key)
		c.touchLease(project, addr, key)
	}
	if c.leaseIn[addr] == nil {
		c.leaseIn[addr] = map[string]bool{}
	}
	c.leaseIn[addr][project] = true
	c.touchLeaseRoute(project, addr)
}

// released forgets key for addr, in whichever project holds it, and marks it
// ended like a close: a heartbeat answer that left before the release (one
// the session-event kick sent meanwhile) still lists the key, and taken as
// it is it put the key back, so the next heartbeat reported the released
// lease lost.
func (c *replicaCache) released(addr, key string) {
	c.endLease(addr, key)
}

// endLease records that addr ended the lease named by id itself, in every
// project (an id names one lease; which project's answer lists it is not
// known yet).
func (c *replicaCache) endLease(addr, id string) {
	c.init()
	for p, byAddr := range c.held {
		c.touchLease(p, addr, id)
		for k := range byAddr[addr] {
			if keyIs(k, id) {
				delete(byAddr[addr], k)
			}
		}
		mark(c.ended, p, addr, id)
	}
	for p := range c.leaseIn[addr] {
		mark(c.ended, p, addr, id)
		c.touchLease(p, addr, id)
	}
}

// holds reports whether addr is known to hold the lease named by id.
func (c *replicaCache) holds(addr, id string) bool {
	for _, byAddr := range c.held {
		for k := range byAddr[addr] {
			if keyIs(k, id) {
				return true
			}
		}
	}
	return false
}

// claimed forgets that addr ended the lease named by id (a key or a native
// issue's id, either way round): it claimed it again.
func (c *replicaCache) claimed(addr, id string) {
	for p := range c.held {
		c.touchLease(p, addr, id)
	}
	for _, byAddr := range c.ended {
		for e := range byAddr[addr] {
			if keyIs(e, id) || keyIs(id, e) {
				delete(byAddr[addr], e)
			}
		}
	}
}

// dropEnded takes the leases addr ended out of project's answer's held keys,
// and forgets those that answer no longer lists.
func (c *replicaCache) dropEnded(project, addr string, keys map[string]bool) {
	for id := range c.ended[project][addr] {
		listed := false
		for k := range keys {
			if keyIs(k, id) {
				delete(keys, k)
				listed = true
			}
		}
		if !listed {
			delete(c.ended[project][addr], id)
		}
	}
}

// presentIn is the one project, of those at one authority (same), whose
// heartbeat carries ss: an authority keeps one presence row per session, so
// a session in two of its projects' heartbeats would move between them on
// every beat. Its own project when that is one of them; else the first (by
// id) it holds keys in; "" for none. Its bound leases in the others are
// renewed all the same: the authority renews from the session, not from the
// project it is present in.
func (c *replicaCache) presentIn(ss orchapi.Session, same map[string]bool) string {
	if same[ss.Project] {
		return ss.Project
	}
	first := ""
	for p := range c.leaseIn[ss.Address] {
		if same[p] && (first == "" || p < first) {
			first = p
		}
	}
	return first
}

// keyIs reports whether lease key k is the one id names.
func keyIs(k, id string) bool { return k == id || strings.HasSuffix(k, ":"+id) }

// boundedPresenceAcks reserves a finite number of ended-session rows and
// prompt ids. Unsent ids stay unacknowledged on disk for the next heartbeat.
func boundedPresenceAcks(unacked map[string][]string, live map[string]orchapi.Session) map[string][]string {
	addrs := make([]string, 0, len(unacked))
	for addr := range unacked {
		addrs = append(addrs, addr)
	}
	sort.Strings(addrs)
	out := map[string][]string{}
	ended, ids := 0, 0
	for _, addr := range addrs {
		_, running := live[addr]
		if !running && ended >= maxPresenceAckSessions {
			continue
		}
		n := min(len(unacked[addr]), maxPresenceAcks-ids)
		if n == 0 {
			continue
		}
		out[addr] = unacked[addr][:n]
		ids += n
		if !running {
			ended++
		}
	}
	return out
}

// elsewhereKey keys the replica cache's counts for a session registered
// outside project (session addresses never contain a space).
func elsewhereKey(project, address string) string { return project + " " + address }

// inProject reports whether address is a local session of project (as far
// as the session module knows).
func (s *Service) inProject(address, project string) bool {
	if orchcore.Sessions(s.d) == nil {
		return false
	}
	ss, ok := orchcore.Sessions(s.d).Lookup(context.Background(), address)
	return !ok || ss.Project == project
}

func (s *Service) setLocalState(addr string, st orchapi.SessionState, note string) {
	if orchcore.Sessions(s.d) == nil {
		return
	}
	if err := orchcore.Sessions(s.d).SetState(context.Background(), addr, orchapi.StateRequest{State: st, Note: note}); err != nil {
		s.d.Log.Debug("team: session state", "session", addr, "err", err)
	}
}

// forgetNotices drops what the cache holds for project (left, purged or
// revoked: its mail is out of reach).
func (s *Service) forgetNotices(project string) {
	s.pmu.Lock()
	delete(s.cache.notices, project)
	s.pmu.Unlock()
}

// TeamNotice implements orchcore.TeamReplica.
func (s *Service) TeamNotice(address string) []orchapi.NoticePart {
	ps, _, _ := s.teamNotices(context.Background(), address)
	return ps
}

// TeamMailNotices implements orchcore.TeamMailNotices without a network call.
func (s *Service) TeamMailNotices(ctx context.Context, address string) ([]orchcore.MailNotice, []orchapi.NoticePart) {
	_, ms, fallback := s.teamNotices(ctx, address)
	return ms, fallback
}

var _ orchcore.TeamMailNotices = (*Service)(nil)

func (s *Service) teamNotices(ctx context.Context, address string) ([]orchapi.NoticePart, []orchcore.MailNotice, []orchapi.NoticePart) {
	if s.Count() == 0 {
		return nil, nil, nil
	}
	// Round C: a project with a ready copy counts from it (local SQL, current
	// to the last change synced); the others from the last heartbeat.
	fromCopy := map[string]bool{}
	var out []orchapi.NoticePart
	var metadata []orchcore.MailNotice
	for _, x := range s.List() {
		rp := rep(x)
		if !rp.isReady() {
			continue
		}
		snap := x.Snapshot()
		if snap.State != api.RemoteOK {
			continue
		}
		me := address
		if a, err := api.ParseAddress(address); err == nil && a.Kind == api.KindHuman {
			me = "human:" + snap.Identity // the replica's identity is its machine's human there
		}
		ms, err := s.inboxMark(ctx, rp, snap, me, false, 500, false)
		if err != nil {
			continue
		}
		fromCopy[snap.Project.ID] = true
		ms = slices.DeleteFunc(ms, func(m orchapi.Mail) bool { return m.HeldFor != "" }) // waits on an outside party
		if len(ms) == 0 {
			continue
		}
		direct := 0
		p := orchapi.NoticePart{Project: snap.Project.ID, Slug: snap.Project.Slug, DirectWakeable: &direct}
		seen := map[string]bool{}
		for _, m := range ms {
			// The authority has scoped/redacted the copy, exactly as for
			// inbox reads here. The seam cannot carry bodies or subjects.
			n := orchcore.MailNotice{Project: snap.Project.ID, ID: m.ID, Created: m.Created,
				Group: m.Recipient == "all" || strings.HasPrefix(m.Recipient, "machine:")}
			if !n.Group {
				n.From, n.Ref = m.From, m.Ref
			}
			metadata = append(metadata, n)
			p.Unread++
			if m.Kind == orchapi.MailKindNotice {
				p.Quiet++
			}
			switch {
			case m.Recipient == "all":
				p.Announce++
			case strings.HasPrefix(m.Recipient, "machine:"):
				p.Group++
			case m.Recipient == "owners":
				p.Owners++
			default:
				if m.Kind != orchapi.MailKindNotice {
					*p.DirectWakeable++
				}
			}
			if !seen[m.From] {
				seen[m.From] = true
				p.From = append(p.From, m.From)
			}
		}
		out = append(out, p)
	}
	s.pmu.Lock()
	defer s.pmu.Unlock()
	var fallback []orchapi.NoticePart
	for project, m := range s.cache.notices {
		if !fromCopy[project] {
			out = append(out, m[address]...)
			fallback = append(fallback, m[address]...)
		}
	}
	return out, metadata, fallback
}

// TeamTarget implements orchcore.TeamReplica.
func (s *Service) TeamTarget(ctx context.Context, cwd, area string) (api.ProjectRef, string, bool, error) {
	if s.Count() == 0 {
		return api.ProjectRef{}, "", false, nil
	}
	var x *remote
	in := core.ResolveInputOf(ctx)
	a := ""
	if in.Explicit != "" {
		slug, ar, _ := strings.Cut(in.Explicit, "/")
		if _, local := s.d.Projects.Lookup(slug); local {
			return api.ProjectRef{}, "", false, nil
		}
		if x = s.ByName(slug); x == nil {
			return api.ProjectRef{}, "", false, nil
		}
		a = ar
	} else if x, a = s.TeamFor(ctx, cwd); x == nil {
		return api.ProjectRef{}, "", false, nil
	}
	snap := x.Snapshot()
	if err := teamlink.Ended(snap); err != nil {
		return api.ProjectRef{}, "", false, err
	}
	if area == "" {
		area = a
	}
	// The replica checks the scope it holds for a fast local error; the
	// authority checks again on every presence write.
	if snap.Cap.Area != "" {
		if area == "" {
			area = snap.Cap.Area
		} else if area != snap.Cap.Area {
			return api.ProjectRef{}, "", false, api.NotFound("area", area)
		}
	}
	if area != "" && len(snap.Areas) > 0 && !contains(snap.Areas, area) {
		return api.ProjectRef{}, "", false, api.NotFound("area", area)
	}
	return snap.Project, area, true, nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
