package team

// The session roster cache (SPEC 17.6 "Replica reads"). A replica holds no
// sessions table, so the machine owner's `GET /v0/sessions` with no project
// (zm ls) asks every authority, and one slow authority made it take seconds.
// Each copy keeps the list its authority last gave the replica's own
// identity, refreshed every teamlink.BeatEvery and by each owner read that got an
// answer. A read that gets none within RosterWait is answered from it.
//
// Privacy: the list is what the authority returns to the machine owner (no
// Zm-As, the replica's own token). Only that caller is ever served from it:
// the socket's owner, no session, no query. Everyone else forwards as before.
// The cache lives on the copy (gone with it), and every use rechecks the
// role fence and the role it was fetched under.

import (
	"context"
	"encoding/json"
	"github.com/sengirunners/potluck/kit/teamlink"
	"net/http"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// RosterWait is how long an owner read waits for the authority before it
// answers from a cached list (SPEC 17.7: replica reads 50 ms).
var RosterWait = 50 * time.Millisecond

const rosterTTLEvery = 3 // a list older than this many teamlink.BeatEvery is not served

// rosterQuiet is how old a served cached list may be before it says so
// (behind): younger than this it reads as current.
const rosterQuiet = 5 * time.Second

type rosterEntry struct {
	list  map[string]json.RawMessage
	at    time.Time
	fence teamlink.RoleReplyFence
	role  string
}

// rosterCache is one copy's cached list; the zero value is empty.
type rosterCache struct {
	mu   sync.Mutex
	e    *rosterEntry
	kick chan struct{} // asks rosterLoop for a refresh now (one at a time)
}

func cloneRoster(list map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(list)+1)
	for k, v := range list {
		out[k] = v
	}
	return out
}

// refresh asks the copy's rosterLoop to fetch now; a refresh already asked
// for is not asked twice.
func (c *rosterCache) refresh() {
	c.mu.Lock()
	ch := c.kick
	c.mu.Unlock()
	if ch != nil {
		kick(ch)
	}
}

func (c *rosterCache) clear() {
	c.mu.Lock()
	c.e = nil
	c.mu.Unlock()
}

func (c *rosterCache) get() *rosterEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.e
}

// ownerRosterCall: a plain GET /v0/sessions (no query) from the machine
// owner on the socket, not a session (the test handleOutbox makes).
func (s *Service) ownerRosterCall(r *http.Request) bool {
	sub := core.SubjectOf(r.Context())
	return r.Method == http.MethodGet && r.URL.Path == "/v0/sessions" && r.URL.RawQuery == "" &&
		sub != nil && sub.Via == authz.ViaLocal && sub.Session == "" && sub.TokenID == "" && !sub.Remote &&
		sub.Address == s.d.Config.HumanAddress()
}

// publishRoster stores list as rp's roster when fence still holds and rp is
// still x's copy. The fence lock is held only for the store.
func (s *Service) publishRoster(x *remote, rp *replica, fence teamlink.RoleReplyFence, list map[string]json.RawMessage) {
	unguard, ok := fence.Guard()
	if !ok {
		return
	}
	defer unguard()
	if rep(x) != rp {
		return
	}
	rp.roster.mu.Lock()
	// A copy: the caller goes on to edit its own answer (union's dropMachine)
	// while other reads range over the cached one.
	rp.roster.e = &rosterEntry{list: cloneRoster(list), at: time.Now(), fence: fence, role: fence.Snap.Role}
	rp.roster.mu.Unlock()
}

// validRoster is the entry when it is young enough and its lifetime, scope
// and role are still the ones it was fetched under.
func (s *Service) validRoster(x *remote, rp *replica) *rosterEntry {
	e := rp.roster.get()
	if e == nil {
		return nil
	}
	if time.Since(e.at) > rosterTTLEvery*teamlink.BeatEvery || !e.fence.Current() {
		return nil
	}
	if cur := x.Snapshot(); cur.Role != e.role || teamlink.Ended(cur) != nil {
		return nil
	}
	return e
}

// fetchOwnerRoster asks x's authority for the session list as the union arm
// does, with the machine owner as the caller. The status is the authority's
// when it answered.
func (s *Service) fetchOwnerRoster(ctx context.Context, x *remote) (map[string]json.RawMessage, int, error) {
	sub := &authz.Subject{Via: authz.ViaLocal, Address: s.d.Config.HumanAddress()}
	req, err := http.NewRequestWithContext(core.WithSubject(ctx, sub), http.MethodGet, "/v0/sessions", nil)
	if err != nil {
		return nil, 0, err
	}
	resp, err := s.ForwardResp(req, x, "", false, []byte{})
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, teamlink.Decode(resp, nil)
	}
	var theirs map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&theirs); err != nil {
		return nil, 0, teamlink.ErrBadAnswer
	}
	return theirs, http.StatusOK, nil
}

// rosterAnswer is the union arm's answer for x when the caller is the owner
// and the cache holds a valid list: the live answer if the authority gives
// one within RosterWait (and the cache learns it), else the cached list.
// ok false: forward as usual. union post-processes either answer alike.
func (s *Service) rosterAnswer(r *http.Request, x *remote) (map[string]json.RawMessage, bool) {
	if !s.ownerRosterCall(r) {
		return nil, false
	}
	rp := rep(x)
	if rp == nil || s.validRoster(x, rp) == nil {
		return nil, false
	}
	if rp.isReachable() {
		fence := x.RoleReply()
		ctx, cancel := context.WithTimeout(r.Context(), RosterWait)
		theirs, status, err := s.fetchOwnerRoster(ctx, x)
		cancel()
		switch {
		case err == nil:
			s.publishRoster(x, rp, fence, theirs)
			return theirs, true
		case status != 0 && status < 500, teamlink.Ended(x.Snapshot()) != nil:
			return nil, false // refused or ended: the usual path says so
		}
		rp.roster.refresh() // let a whole fetch finish in the copy's loop, not here
	}
	e := s.validRoster(x, rp) // rechecked: the wait is long enough for a revoke
	if e == nil {
		return nil, false
	}
	theirs := cloneRoster(e.list)
	if time.Since(e.at) > rosterQuiet {
		js, _ := json.Marshal([]orchapi.WorkBehind{{Project: x.Snapshot().Project.ID, Source: "authority", State: orchapi.SyncStale, Synced: e.at,
			Error: "session roster as of " + e.at.UTC().Format(time.RFC3339) + " (" + time.Since(e.at).Round(time.Second).String() + " ago): the authority did not answer"}})
		theirs["behind"] = teamlink.AppendArrays(theirs["behind"], js)
	}
	return theirs, true
}

// rosterLoop keeps rp's roster fresh: right away, then every teamlink.BeatEvery.
func (s *Service) rosterLoop(ctx context.Context, x *remote, rp *replica) {
	for {
		if teamlink.Ended(x.Snapshot()) != nil {
			rp.roster.clear()
		} else {
			fence := x.RoleReply()
			cctx, cancel := context.WithTimeout(ctx, teamlink.CallTimeout)
			list, _, err := s.fetchOwnerRoster(cctx, x)
			cancel()
			if ctx.Err() != nil {
				return
			}
			if err == nil {
				s.publishRoster(x, rp, fence, list)
			} else if teamlink.Ended(x.Snapshot()) != nil || api.IsCode(err, api.CodeForbidden) || api.IsCode(err, api.CodeUnauthenticated) {
				rp.roster.clear() // no grant now: keep nothing
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(teamlink.BeatEvery):
		case <-rp.roster.kick:
		}
	}
}
