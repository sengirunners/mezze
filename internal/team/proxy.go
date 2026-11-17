package team

// team's part in teamlink's proxy (SPEC 17.3, round B; teamlink proxy.go):
// the copy serves while the authority is away (round C), relayed answers
// teach the lease cache and the needs-human state (round B2), and the
// orchestration routes' rows (routes_table.go) with their hooks.
//
// Native issues have no copy here: every call waits for the authority, and
// an authority that does not answer is an error, never an empty answer. A
// call on one issue that the named project answers 404 has its id resolved
// by the other joined projects on the same authority, then goes out once to
// the one that found it (forwardIssue: a moved issue).

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// copyState is Hooks.Copy: x's copy, ready and reachable.
func (s *Service) copyState(x *remote) (ready, reachable bool, lastErr string) {
	rp := rep(x)
	if !rp.isReady() {
		return false, true, ""
	}
	return true, rp.isReachable(), rp.view().LastError
}

// serveFromCopy is Hooks.ServeCopy (round C: the copy answers).
func (s *Service) serveFromCopy(w http.ResponseWriter, r *http.Request, x *remote, area string, body []byte) bool {
	rp := rep(x)
	return rp.isReady() && s.serveCopy(w, r, x, rp, area, body)
}

// serveHeld is Hooks.ServeHeld: a copy that holds the item (or mail)
// answers it here.
func (s *Service) serveHeld(w http.ResponseWriter, r *http.Request, x *remote, body []byte) bool {
	rp := rep(x)
	if !rp.isReady() {
		return false
	}
	return (rp.holds(r.Context(), r.URL.Path) || s.noQueueHeld(r, x)) && s.serveCopy(w, r, x, rp, "", body)
}

// noQueueHeld: a no-queue release of a lease the caller took through this
// replica, which the copy may not list yet (a no-queue write is never
// forwarded, so it must be served here).
func (s *Service) noQueueHeld(r *http.Request, x *remote) bool {
	if !noQueue(r) || r.Method != http.MethodDelete || !strings.HasPrefix(r.URL.Path, "/v0/leases/") {
		return false
	}
	key, _ := segs(r.URL.Path, "/v0/leases/")
	snap := x.Snapshot()
	as, _ := s.callerAddr(r, snap)
	s.pmu.Lock()
	defer s.pmu.Unlock()
	return as != "" && s.cache.held[snap.Project.ID][as][key]
}

// listShare is Hooks.ListShare: the machine owner's own list from the
// roster cache when the authority does not answer in time (roster.go), else
// a copy that serves the list (no tailnet call).
func (s *Service) listShare(r *http.Request, x *remote) (map[string]json.RawMessage, bool, error) {
	if theirs, ok := s.rosterAnswer(r, x); ok {
		return theirs, true, nil
	}
	rp := rep(x)
	if !rp.isReady() || copyKind(r.Method, r.URL.Path) != "read" {
		return nil, false, nil
	}
	v, ok, err := s.copyRead(r, x, rp, "")
	if !ok {
		return nil, false, nil
	}
	if err != nil {
		return nil, true, err
	}
	js, _ := json.Marshal(v)
	var theirs map[string]json.RawMessage
	if err := json.Unmarshal(js, &theirs); err != nil {
		return nil, true, teamlink.ErrBadAnswer
	}
	return theirs, true, nil
}

// createNamed is POST /v0/work's Named hook: items are made at the
// authority, and with a copy the new item goes into it at once
// (forwardCreate). By the cwd, an authority the copy saw go is an error.
func (s *Service) createNamed(w http.ResponseWriter, r *http.Request, x *remote, area string, explicit bool, _ []byte) bool {
	rp := rep(x)
	if !rp.isReady() {
		return false
	}
	if !explicit && !rp.isReachable() {
		api.WriteError(w, api.Errorf(api.CodeUnavailable, "the authority %s is unreachable: new items are made there", x.Snapshot().Authority).
			WithHint("comments, status, mail and claims queue while it is away; zm work new once it answers"))
		return true
	}
	s.forwardCreate(w, r, x, area, explicit)
	return true
}

// forwardCreate forwards POST /v0/work (items are made at the authority)
// and puts the new item into the copy at once, so the maker reads it back.
func (s *Service) forwardCreate(w http.ResponseWriter, r *http.Request, x *remote, area string, explicit bool) {
	rp := rep(x)
	var fence copyFence
	if rp != nil {
		fence = rp.copyFence()
	}
	resp, err := s.ForwardResp(r, x, area, explicit, nil)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		api.WriteError(w, api.Errorf(api.CodeUnavailable, "reading the authority's answer: %v", err))
		return
	}
	if rp != nil && rp == rep(x) && resp.StatusCode == http.StatusOK {
		var it orchapi.WorkItem
		if json.Unmarshal(b, &it) == nil && it.ID != "" {
			done := rp.copyReplyHook("create", it.ID)
			if err := rp.putCreated(r.Context(), it, fence); errors.Is(err, errStaleCopy) {
				rp.want(it.ID) // validate freshly rather than trust the old-scope reply
			}
			if done != nil {
				done()
			}
		}
	}
	if v := resp.Header.Get("Content-Type"); v != "" {
		w.Header().Set("Content-Type", v)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(b)
}

// isIssuePath reports whether path is a native issue route.
func isIssuePath(path string) bool {
	return path == "/v0/issues" || strings.HasPrefix(path, "/v0/issues/")
}

// issueProjectRoute reports whether path is a native issue route that takes
// a project, not an issue id.
func issueProjectRoute(path string) bool {
	switch path {
	case "/v0/issues/stats", "/v0/issues/labels", "/v0/issues/comments", "/v0/issues/memories", "/v0/issues/sql", "/v0/issues/setup",
		"/v0/issues/move":
		return true
	}
	return strings.HasPrefix(path, "/v0/issues/memories/")
}

// promptRoute sends a shared session's prompt (or its log read) to the
// authority that logs and routes it (SPEC 17.10): a session of this machine
// in a joined project to that project's authority, by its full address;
// another machine's session here first, then each authority while 404.
func (s *Service) promptRoute(w http.ResponseWriter, r *http.Request, next http.Handler, body []byte) {
	rest := strings.TrimPrefix(r.URL.Path, "/v0/sessions/")
	suffix := "/prompt"
	if strings.HasSuffix(rest, "/prompts") {
		suffix = "/prompts"
	}
	id := strings.TrimSuffix(rest, suffix)
	if sub := core.SubjectOf(r.Context()); suffix == "/prompt" && sub != nil && sub.Session != "" {
		// Only a person sends a prompt (the authority refuses it too: the
		// forwarded call carries Zm-As).
		api.WriteError(w, api.Errorf(api.CodeForbidden, "only a person sends a prompt to a shared session, not an agent session (%s)", sub.Session))
		return
	}
	if orchcore.Sessions(s.d) != nil {
		if ss, ok := orchcore.Sessions(s.d).Lookup(r.Context(), id); ok && ss.Machine == s.d.Config.Machine {
			if x := s.ByName(ss.Project); x != nil {
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/v0/sessions/" + ss.Address + suffix
				r2.URL.RawPath = "/v0/sessions/" + url.PathEscape(ss.Address) + suffix
				r2.Body = io.NopCloser(bytes.NewReader(body))
				s.Forward(w, r2, x, "", false)
				return
			}
			next.ServeHTTP(w, r) // a project of this machine's own
			return
		}
	}
	s.LocalFirst(w, r, next, body)
}

// relay is Hooks.Relay: it copies an authority's answer to the caller,
// noting what it says about this machine's sessions (round B2): a claim or
// release by a session (what it holds, for the lost-lease check),
// needs=human mail it sent (it waits for a human here too, until the
// authority says the mail is answered), and any mail call (the notice cache
// refreshes soon).
func (s *Service) relay(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/v0/mail") {
		defer s.kickPresence()
	}
	sub := core.SubjectOf(r.Context())
	kind := callKind(r)
	if kind == "" || resp.StatusCode != http.StatusOK || sub == nil || sub.Session == "" {
		teamlink.CopyResponse(w, resp)
		return
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if v := resp.Header.Get("Content-Type"); v != "" {
		w.Header().Set("Content-Type", v)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(b)
	if err != nil {
		return
	}
	// Foreground native replies are outside repWG. A late response from a
	// retired copy lifetime must not restore private cache/session metadata.
	fence, ok := teamlink.ReplyFence(resp)
	if !ok {
		return
	}
	if kind != "mail" && kind != "ask" {
		unguard, ok := fence.Guard()
		if !ok {
			return
		}
		defer unguard()
	}
	switch kind {
	case "claim":
		var cr orchapi.ClaimResponse
		if json.Unmarshal(b, &cr) == nil && cr.Lease.Holder == sub.Session && cr.Lease.Key != "" {
			s.pmu.Lock()
			s.cache.claimedIn(cr.Lease.Project, sub.Session, cr.Lease.Key)
			s.cache.claimed(sub.Session, cr.Lease.Key)
			s.pmu.Unlock()
		}
	case "status":
		var it orchapi.WorkItem
		if json.Unmarshal(b, &it) == nil && it.ID != "" && !heldStatus(it.Status) {
			s.pmu.Lock()
			s.cache.endLease(sub.Session, it.ID)
			s.pmu.Unlock()
		}
	case "issue-claim":
		var is orchapi.Issue
		if json.Unmarshal(b, &is) == nil && is.ClaimedBy == sub.Session {
			s.pmu.Lock()
			s.cache.init()
			s.cache.claimed(sub.Session, is.ID)
			s.cache.claimedIn(is.Project, sub.Session, "")
			s.pmu.Unlock()
		}
	case "release":
		var rr orchapi.ReleaseResponse
		if json.Unmarshal(b, &rr) == nil && rr.Released {
			s.pmu.Lock()
			s.cache.init()
			s.cache.released(sub.Session, rr.Key)
			s.pmu.Unlock()
		}
	case "issue":
		// A native write that ended the session's own claim is a release: the
		// lease it held (<prefix>:<id>, learned from a heartbeat) must not
		// read as lost at the next one.
		var is orchapi.Issue
		if json.Unmarshal(b, &is) == nil && is.ID != "" {
			s.pmu.Lock()
			if own := is.ClaimedBy == sub.Session; (own && !heldStatus(is.ZmStatus)) || (!own && s.cache.holds(sub.Session, is.ID)) {
				s.cache.endLease(sub.Session, is.ID)
			}
			s.pmu.Unlock()
		}
	case "mail":
		var m orchapi.Mail
		if json.Unmarshal(b, &m) == nil && m.Needs == orchapi.NeedsHuman && m.HeldFor == "" && m.Kind != orchapi.MailKindReport && m.From == sub.Session && m.ID != "" {
			if !fence.Current() {
				return
			}
			id := m.ID
			if len(id) > 8 {
				id = id[len(id)-8:]
			}
			s.setLocalState(sub.Session, orchapi.StateNeedsHuman, "waiting on mail "+id)
		}
	case "ask":
		// An ask is needs=human mail sent at the authority: the same wait,
		// except one held for an outside party (it waits on them, not here).
		var a orchapi.Ask
		if json.Unmarshal(b, &a) == nil && a.AskedBy == sub.Session && a.ID != "" && a.For == nil {
			if !fence.Current() {
				return
			}
			id := a.ID
			if len(id) > 8 {
				id = id[len(id)-8:]
			}
			s.setLocalState(sub.Session, orchapi.StateNeedsHuman, "waiting on mail "+id)
		}
	}
}

// callKind names the calls relay reads the answer of.
func callKind(r *http.Request) string {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodPost && (p == "/v0/leases" || (strings.HasPrefix(p, "/v0/work/") && strings.HasSuffix(p, "/claim"))):
		return "claim"
	case isRelease(r):
		return "release"
	case r.Method == http.MethodPost && (p == "/v0/mail" || (strings.HasPrefix(p, "/v0/mail/") && strings.HasSuffix(p, "/reply"))):
		return "mail"
	case r.Method == http.MethodPost && p == "/v0/asks":
		return "ask"
	case issueClaimEnding(r):
		return "issue"
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/v0/issues/") && strings.HasSuffix(p, "/claim"):
		return "issue-claim"
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/v0/work/") && strings.HasSuffix(p, "/status"):
		return "status"
	}
	return ""
}

// issueClaimEnding reports whether r is a native write that can end the
// caller's claim: closing the issue or changing it (status, assignee).
func issueClaimEnding(r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/v0/issues/") || issueProjectRoute(r.URL.Path) {
		return false
	}
	_, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v0/issues/"), "/")
	return (r.Method == http.MethodPost && rest == "close") || (r.Method == http.MethodPatch && rest == "")
}

// heldStatus reports whether a native issue in zm status st keeps its lease
// (SPEC "Native issues").
func heldStatus(st orchapi.WorkStatus) bool {
	return st == orchapi.WorkClaimed || st == orchapi.WorkInReview
}

// mergeIssuePage adds one store's page of a native list that named no
// project to merged. Keyset cursors are per store, so such a list is every
// store's first page: its total and more add up and it has no next (a
// caller that pages names the project).
func mergeIssuePage(merged, theirs map[string]json.RawMessage) {
	var a, b orchapi.IssueList
	_ = json.Unmarshal(merged["total"], &a.Total)
	_ = json.Unmarshal(theirs["total"], &b.Total)
	_ = json.Unmarshal(merged["more"], &a.More)
	_ = json.Unmarshal(theirs["more"], &b.More)
	merged["total"], _ = json.Marshal(a.Total + b.Total)
	merged["more"], _ = json.Marshal(a.More || b.More)
	_ = json.Unmarshal(merged["unranked"], &a.Unranked)
	_ = json.Unmarshal(theirs["unranked"], &b.Unranked)
	if a.Unranked || b.Unranked {
		merged["unranked"] = json.RawMessage("true")
	}
	delete(merged, "next")
}

// dropMachine removes the sessions on machine from a JSON array of them.
func dropMachine(raw json.RawMessage, machine string) json.RawMessage {
	var rows []json.RawMessage
	if json.Unmarshal(raw, &rows) != nil {
		return raw
	}
	keep := rows[:0]
	for _, x := range rows {
		var m struct {
			Machine string `json:"machine"`
		}
		if json.Unmarshal(x, &m) == nil && m.Machine == machine {
			continue
		}
		keep = append(keep, x) // as the authority sent it: nothing lost in a decode
	}
	out, _ := json.Marshal(nonNil(keep))
	return out
}

func isRelease(r *http.Request) bool {
	return (r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v0/work/") && strings.HasSuffix(r.URL.Path, "/release")) ||
		(r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v0/leases/"))
}

func forced(body []byte) bool {
	var req orchapi.ReleaseRequest
	return json.Unmarshal(body, &req) == nil && req.Force
}

// dedupMachines folds the rows of one machine that several authorities
// sent (it joined projects at more than one): the latest seen wins, the
// projects are united. A row without seen (the answering machine's own)
// wins over every other.
func dedupMachines(raw json.RawMessage) json.RawMessage {
	var rows []api.Machine
	if json.Unmarshal(raw, &rows) != nil {
		return raw
	}
	var out []api.Machine
	at := map[string]int{}
	for _, m := range rows {
		i, ok := at[m.Machine]
		if !ok {
			at[m.Machine] = len(out)
			out = append(out, m)
			continue
		}
		cur := &out[i]
		projects := unionSorted(cur.Projects, m.Projects)
		if cur.Seen != nil && (m.Seen == nil || m.Seen.After(*cur.Seen)) {
			*cur = m
		}
		cur.Projects = projects
	}
	b, _ := json.Marshal(nonNil(out))
	return b
}

func unionSorted(a, b []string) []string {
	set := map[string]bool{}
	for _, x := range append(append([]string(nil), a...), b...) {
		set[x] = true
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for x := range set {
		out = append(out, x)
	}
	sort.Strings(out)
	return out
}
