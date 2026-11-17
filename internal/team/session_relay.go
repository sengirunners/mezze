package team

// Structured sessions on other machines (SPEC 17.10
// "Structured"): a shared structured session's events and operations reach
// a project member through the project's authority, which never connects to
// the session's machine. Reads: a reader at the authority leaves a want; the
// heartbeat answer for that session carries it (Stream, the seq to read
// after); the machine uploads a page (POST /v0/team/session-events) into a
// bounded per-session ring the authority serves from. Operations (turn,
// steer, interrupt, answer) are machine ops (OpSession on team_ops): taken
// with a heartbeat answer, recorded before they run, run at most once, their
// result reported in the next beat. A shared live session beats every
// SharedEvery, so both take a few seconds. The ring is memory only: the
// machine's spool stays the record, and a redaction in the project drops
// the project's rings (the machine's own pages already honor its holds).

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/sengirunners/potluck/kit/teamlink"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
)

// Tunables (tests shorten them).
var (
	// relayWantFor: a reader's want lasts this long after its last read.
	relayWantFor = 30 * time.Second
	// relayRingMax bounds the event bytes the authority keeps per session.
	relayRingMax = 4 << 20
	// relayOpWait: how long an operation waits for the machine's result
	// before answering pending.
	relayOpWait = 15 * time.Second
	// relayOpTake: a session operation no machine took by then is dropped
	// (a turn typed minutes late would surprise its sender).
	relayOpTake = time.Minute
	// relayUploadMax bounds an upload's body.
	relayUploadMax = 8 << 20
	// relayRedactKeep: how long the authority refuses uploaded pages holding
	// a text redacted in their project. A replica scrubs its own session
	// spools only once its copy took the redaction (and only if the copy held
	// the comment), so for this long a page that still holds the text is
	// dropped, never served.
	relayRedactKeep = time.Hour
)

// relayRedactMax bounds the texts kept per project.
const relayRedactMax = 256

const relayPage = 500 // events per page, as a local read's

var _ orchcore.SessionRelay = (*Service)(nil)

// relayEvent is one event in a ring, by its seq.
type relayEvent struct {
	seq int64
	raw json.RawMessage
}

// relayRing is the authority's copy of one remote session's recent events.
type relayRing struct {
	project string
	filled  bool  // a page arrived
	base    int64 // events after base are held (from the first upload, or evicted up to)
	next    int64 // the machine's Next of its last page
	evs     []relayEvent
	bytes   int
	state   orchapi.SessionState
	ended   bool
	trunc   bool
	// withheld: a page holding a redacted text came, so reads are refused
	// (as a local read under a redaction hold is) till then, asking nothing.
	withheld time.Time
	want     int64     // the seq the machine reads after for the next upload
	until    time.Time // the want lasts till then
	changed  chan struct{}
}

// relayState is the authority's rings, and on a machine the uploads under way.
type relayState struct {
	mu       sync.Mutex
	rings    map[string]*relayRing   // by session address
	pushing  map[string]bool         // a machine's uploads in flight, by address
	redacted map[string][]redactNote // by project: texts redacted lately
}

// redactNote is a text redacted in a project, kept until a time.
type redactNote struct {
	needles []string // the text, and as it shows inside a JSON string
	until   time.Time
}

func (r *relayRing) wake() {
	close(r.changed)
	r.changed = make(chan struct{})
}

// ring is address's ring, made on first use (call with mu held).
func (st *relayState) ring(address, project string) *relayRing {
	if st.rings == nil {
		st.rings = map[string]*relayRing{}
	}
	r := st.rings[address]
	if r == nil || r.project != project {
		r = &relayRing{project: project, changed: make(chan struct{})}
		st.rings[address] = r
	}
	return r
}

// read is the page after `after` the ring holds (ok false: it holds none of
// it yet), leaving a want for the machine. ch closes on the next upload.
func (st *relayState) read(ss orchapi.Session, after int64, limit int) (page orchapi.SessionEvents, ch <-chan struct{}, ok bool, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	r := st.ring(ss.Address, ss.Project)
	if time.Now().Before(r.withheld) {
		return orchapi.SessionEvents{}, nil, false, api.Errorf(api.CodeConflict, "session output withheld pending redaction")
	}
	want := after
	if r.filled && after >= r.base {
		want = max(after, r.next) // held from after on: only newer ones are owed
	}
	r.want, r.until = want, time.Now().Add(relayWantFor)
	if !r.filled || after < r.base {
		return orchapi.SessionEvents{}, r.changed, false, nil
	}
	i := sort.Search(len(r.evs), func(i int) bool { return r.evs[i].seq > after })
	out := []json.RawMessage{}
	next := after
	for ; i < len(r.evs) && len(out) < limit; i++ {
		out = append(out, r.evs[i].raw)
		next = r.evs[i].seq
	}
	if len(out) == 0 {
		next = max(after, r.next)
	}
	return orchapi.SessionEvents{Events: out, Next: next, State: r.state, Ended: r.ended && len(out) == 0 && next >= r.next,
		Truncated: r.trunc}, r.changed, true, nil
}

// wanted is the seq to ask address's machine to read after, while a reader
// waits on it.
func (st *relayState) wanted(address, project string) (int64, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	r := st.rings[address]
	if r == nil || r.project != project || time.Now().After(r.until) {
		return 0, false
	}
	return r.want, true
}

// take merges an uploaded page into address's ring. A page no reader wants
// any more is dropped.
func (st *relayState) take(address, project string, after int64, page orchapi.SessionEvents) {
	st.mu.Lock()
	defer st.mu.Unlock()
	r := st.rings[address]
	if r == nil || r.project != project || time.Now().After(r.until) {
		return
	}
	if st.holdsRedacted(project, page.Events) {
		// The machine has not scrubbed it: never served from here, and not
		// asked for again (it would send the same page every beat).
		r.evs, r.bytes, r.filled = nil, 0, false
		r.withheld, r.until = time.Now().Add(relayRedactKeep), time.Time{}
		r.wake()
		return
	}
	if !r.filled || after != r.next {
		// Not the continuation of what the ring holds: start over from it.
		r.evs, r.bytes, r.base = nil, 0, after
	}
	last := after
	if n := len(r.evs); n > 0 {
		last = r.evs[n-1].seq
	}
	for _, raw := range page.Events {
		var head struct {
			Seq int64 `json:"seq"`
		}
		if json.Unmarshal(raw, &head) != nil || head.Seq <= last {
			continue // out of order or a repeat: never held twice
		}
		r.evs = append(r.evs, relayEvent{seq: head.Seq, raw: raw})
		r.bytes += len(raw)
		last = head.Seq
	}
	for len(r.evs) > 0 && r.bytes > relayRingMax {
		r.base = r.evs[0].seq
		r.bytes -= len(r.evs[0].raw)
		r.evs = r.evs[1:]
	}
	r.filled = true
	r.next = max(page.Next, last)
	r.state, r.ended, r.trunc = page.State, page.Ended, page.Truncated
	r.want = r.next
	r.wake()
}

// holdsRedacted reports whether any event holds a text redacted lately in
// project (call with mu held).
func (st *relayState) holdsRedacted(project string, evs []json.RawMessage) bool {
	now := time.Now()
	keep := st.redacted[project][:0]
	for _, n := range st.redacted[project] {
		if n.until.After(now) {
			keep = append(keep, n)
		}
	}
	if len(keep) == 0 {
		delete(st.redacted, project)
		return false
	}
	st.redacted[project] = keep
	for _, ev := range evs {
		for _, n := range keep {
			for _, needle := range n.needles {
				if strings.Contains(string(ev), needle) {
					return true
				}
			}
		}
	}
	return false
}

// redact drops project's rings and refuses, for relayRedactKeep, pages
// that hold any of texts.
func (st *relayState) redact(project string, texts []string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.dropLocked(project) // under the same lock: no upload lands between
	if st.redacted == nil {
		st.redacted = map[string][]redactNote{}
	}
	until := time.Now().Add(relayRedactKeep)
	for _, t := range texts {
		if len(t) < core.MinUnscoped {
			continue
		}
		enc, _ := json.Marshal(t)
		n := redactNote{needles: []string{t, string(enc[1 : len(enc)-1])}, until: until}
		st.redacted[project] = append(st.redacted[project], n)
	}
	if over := len(st.redacted[project]) - relayRedactMax; over > 0 {
		st.redacted[project] = st.redacted[project][over:]
	}
}

// dropLocked forgets project's rings (call with mu held).
func (st *relayState) dropLocked(project string) {
	for a, r := range st.rings {
		if r.project == project {
			r.wake()
			delete(st.rings, a)
		}
	}
}

// prune forgets the rings no reader asked for in twice relayWantFor (every
// heartbeat at the authority calls it: a ring holds up to relayRingMax).
func (st *relayState) prune() {
	st.mu.Lock()
	defer st.mu.Unlock()
	now := time.Now()
	stale := now.Add(-relayWantFor)
	for a, r := range st.rings {
		if r.until.Before(stale) && !now.Before(r.withheld) { // a withheld ring keeps refusing for its hour
			r.wake()
			delete(st.rings, a)
		}
	}
}

// ---- the authority ----

// RelayEvents implements orchcore.SessionRelay.
func (s *Service) RelayEvents(ctx context.Context, ss orchapi.Session, after int64, limit int, wait time.Duration) (orchapi.SessionEvents, error) {
	if ss.Machine == s.d.Config.Machine || !s.d.Store.IsTeam(ss.Project) {
		return orchapi.SessionEvents{}, api.NotFound("session", ss.Address)
	}
	if limit <= 0 || limit > relayPage {
		limit = relayPage
	}
	// At most 25 s: a member's replica forwards the call, and its relay
	// gives up on an answer silent for its CallTimeout (30 s).
	deadline := time.Now().Add(min(wait, 25*time.Second))
	for {
		page, ch, ok, err := s.streams.read(ss, after, limit)
		if err != nil {
			return orchapi.SessionEvents{}, err
		}
		if ok && (len(page.Events) > 0 || page.Ended || time.Until(deadline) <= 0) {
			return page, nil
		}
		if !ok && (ss.State == orchapi.StateEnded || time.Until(deadline) <= 0) {
			// Nothing came from the machine yet (or the session ended before a
			// reader asked: its machine keeps its stream).
			return orchapi.SessionEvents{Events: []json.RawMessage{}, Next: after, State: ss.State, Ended: ss.State == orchapi.StateEnded}, nil
		}
		t := time.NewTimer(time.Until(deadline))
		select {
		case <-ch:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return orchapi.SessionEvents{Events: []json.RawMessage{}, Next: after, State: ss.State}, nil
		}
		t.Stop()
	}
}

// relayResult is a session op's outcome as team_ops keeps it.
type relayResult struct {
	Code   string                   `json:"code,omitempty"`
	Hint   string                   `json:"hint,omitempty"`
	Result *orchapi.SessionOpResult `json:"result,omitempty"`
}

// RelayOp implements orchcore.SessionRelay.
func (s *Service) RelayOp(ctx context.Context, sub *authz.Subject, ss orchapi.Session, op orchapi.SessionOp) (orchapi.SessionOpResult, error) {
	if ss.Machine == s.d.Config.Machine || !s.d.Store.IsTeam(ss.Project) {
		return orchapi.SessionOpResult{}, api.NotFound("session", ss.Address)
	}
	n := 0
	for _, x := range []bool{op.Turn != nil, op.Interrupt != nil, op.Answer != nil, op.Stop != nil} {
		if x {
			n++
		}
	}
	if n != 1 {
		return orchapi.SessionOpResult{}, api.Errorf(api.CodeBadRequest, "one operation: a turn, an interrupt, an answer or a stop")
	}
	if op.Turn != nil && len(op.Turn.Images) > 0 {
		// Paths name files on the session's machine: never from someone else's.
		return orchapi.SessionOpResult{}, api.Errorf(api.CodeBadRequest, "a turn from another machine carries no images")
	}
	s.ExpireOps(ctx)
	payload, _ := json.Marshal(sessionOp{Session: ss.Address, Op: op})
	mop := api.MachineOp{ID: ids.New(), Kind: opSession, From: sub.Identity.Name, Payload: payload}
	if err := s.QueueOp(ctx, teamlink.Op{MachineOp: mop, Project: ss.Project, Machine: ss.Machine, FromMachine: s.d.Config.Machine, Parent: ss.Address}); err != nil {
		return orchapi.SessionOpResult{}, err
	}
	deadline := time.Now().Add(relayOpWait)
	for {
		o, err := s.LoadOp(ctx, mop.ID)
		if err != nil {
			return orchapi.SessionOpResult{}, err
		}
		if o.State == teamlink.OpDone {
			var rr relayResult
			_ = json.Unmarshal([]byte(o.Result), &rr)
			if o.Error != "" {
				code := rr.Code
				if code == "" {
					code = api.CodeUnavailable
				}
				e := &api.Error{Code: code, Message: o.Error, Hint: rr.Hint}
				if rr.Result != nil {
					e.Data, _ = json.Marshal(rr.Result)
				}
				return orchapi.SessionOpResult{}, e
			}
			if rr.Result == nil {
				return orchapi.SessionOpResult{}, api.Errorf(api.CodeUnavailable, "%s answered no result", ss.Machine)
			}
			return *rr.Result, nil
		}
		if time.Now().After(deadline) {
			return orchapi.SessionOpResult{Pending: true}, nil
		}
		select {
		case <-ctx.Done():
			return orchapi.SessionOpResult{Pending: true}, nil
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// relayRoutes registers the authority's upload route.
func (s *Service) relayRoutes(r *core.Router) {
	r.HandleFunc("POST /v0/team/session-events", core.Authed, "a replica's node-bound token only; a shared structured session on its machine in the project, in an area it may write; dropped unless a reader waits", jsonH(s.handleSessionEvents))
}

// handleSessionEvents is POST /v0/team/session-events.
func (s *Service) handleSessionEvents(r *http.Request) (any, error) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	if sub.Node == "" || sub.Machine == "" {
		return nil, api.Errorf(api.CodeForbidden, "session events come from a team replica's daemon (its node-bound token)")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, int64(relayUploadMax)+1))
	if err != nil {
		return nil, api.Errorf(api.CodeBadRequest, "reading the body: %v", err)
	}
	if len(body) > relayUploadMax {
		return nil, api.Errorf(api.CodeTooLarge, "session events over %d bytes", relayUploadMax)
	}
	var up orchapi.SessionEventsUpload
	if err := json.Unmarshal(body, &up); err != nil {
		return nil, api.Errorf(api.CodeBadRequest, "invalid JSON body: %v", err)
	}
	p, err := s.d.Projects.Get(sub.Scope, up.Project)
	if err != nil {
		return nil, err
	}
	if !s.d.Store.IsTeam(p.ID) {
		return nil, api.NotFound("project", up.Project)
	}
	a, err := api.ParseAddress(up.Session)
	if err != nil || a.Kind != api.KindSession {
		return nil, api.Errorf(api.CodeBadRequest, "%q is not a session address", up.Session)
	}
	if a.Machine != sub.Machine {
		return nil, api.Errorf(api.CodeForbidden, "%s is not on %s, the machine this replica joined as", a.String(), sub.Machine)
	}
	ss, ok := orchcore.Sessions(s.d).Lookup(ctx, a.String())
	if !ok || ss.Project != p.ID || !ss.Shared || ss.Mode != orchapi.SessionModePipe || !sub.Can(authz.Write, authz.Object{Project: p.ID, Area: ss.Area}) {
		return nil, api.NotFound("session", a.String())
	}
	for _, ev := range up.Page.Events {
		if !json.Valid(ev) {
			return nil, api.Errorf(api.CodeBadRequest, "an event is not JSON")
		}
	}
	s.streams.take(ss.Address, p.ID, up.After, up.Page)
	return struct{}{}, nil
}

// streamWant is the Stream a heartbeat answer carries for x.
func (s *Service) streamWant(project string, x orchapi.PresenceSession) *int64 {
	if !x.Shared || !x.Managed || x.Mode != orchapi.SessionModePipe {
		return nil
	}
	if after, ok := s.streams.wanted(x.Address, project); ok {
		return &after
	}
	return nil
}

// ---- the session's machine ----

// pushStreams uploads, off the heartbeat, the pages its answer asked for
// (one upload per session at a time).
func (s *Service) pushStreams(x *remote, project string, local map[string]orchapi.Session, resp orchapi.SessionsBeatAnswer) {
	sp, ok := orchcore.Sessions(s.d).(orchcore.SessionPipe)
	if !ok {
		return
	}
	for _, ans := range resp.Sessions {
		if ans.Stream == nil {
			continue
		}
		if _, mine := local[ans.Address]; !mine {
			continue
		}
		s.streams.mu.Lock()
		if s.streams.pushing == nil {
			s.streams.pushing = map[string]bool{}
		}
		busy := s.streams.pushing[ans.Address]
		s.streams.pushing[ans.Address] = true
		s.streams.mu.Unlock()
		if busy {
			continue
		}
		s.wg.Add(1)
		go func(addr string, after int64) {
			defer s.wg.Done()
			defer func() {
				s.streams.mu.Lock()
				delete(s.streams.pushing, addr)
				s.streams.mu.Unlock()
			}()
			base := s.rctx
			if base == nil {
				base = context.Background()
			}
			ctx, cancel := context.WithTimeout(base, 30*time.Second)
			defer cancel()
			page, err := sp.PipeEvents(ctx, project, addr, after, relayPage)
			if err != nil {
				s.d.Log.Debug("team: read session events for the authority", "session", addr, "err", err)
				return
			}
			page = capPage(page)
			up := orchapi.SessionEventsUpload{Project: project, Session: addr, After: after, Page: page}
			if err := s.CallRemote(ctx, x, http.MethodPost, "/v0/team/session-events", up, nil); err != nil {
				s.d.Log.Debug("team: upload session events", "session", addr, "err", err)
			}
		}(ans.Address, *ans.Stream)
	}
}

// capPage keeps an upload under half of relayUploadMax: the first events
// that fit, Next their last seq. An event too big to fit alone is left out
// (the reader sees its seq skipped), so a page never wedges a reader by
// being refused every beat.
func capPage(page orchapi.SessionEvents) orchapi.SessionEvents {
	budget := relayUploadMax / 2
	kept, n := []json.RawMessage{}, 0
	for _, ev := range page.Events {
		var head struct {
			Seq int64 `json:"seq"`
		}
		_ = json.Unmarshal(ev, &head)
		if len(ev) > budget {
			if len(kept) == 0 {
				if head.Seq > 0 {
					page.Next = head.Seq // skipped alone: the next page goes on after it
				} // else no seq to go by: the page's own Next skips all of it
				page.Events = kept
				page.Ended = false
				return page
			}
			break
		}
		if n+len(ev) > budget {
			break
		}
		kept, n = append(kept, ev), n+len(ev)
		page.Next = head.Seq
	}
	if len(kept) < len(page.Events) {
		page.Ended = false
	}
	page.Events = kept
	return page
}

// opSession is a shared structured session's operation, from a member
// through the authority: payload sessionOp, report
// orchapi.SessionOpResult (also with a refusal, as its error's data).
const opSession = "session_op"

// sessionOp is a session_op's payload.
type sessionOp struct {
	Session string            `json:"session"`
	Op      orchapi.SessionOp `json:"op"`
}

// registerSessionOps registers session_op with teamlink's queue.
func (s *Service) registerSessionOps() {
	s.RegisterOp(opSession, teamlink.OpHandler{Run: s.runSessionOp, Apply: s.applySessionOp, Expiry: relayOpTake})
}

// applySessionOp keeps a session op's outcome (relayResult).
func (s *Service) applySessionOp(ctx context.Context, o teamlink.Op, rep api.MachineOpReport) {
	rr := relayResult{Code: rep.Code, Hint: rep.Hint}
	if len(rep.Payload) > 0 {
		var r orchapi.SessionOpResult
		if json.Unmarshal(rep.Payload, &r) == nil {
			rr.Result = &r
		}
	}
	b, _ := json.Marshal(rr)
	s.FinishOp(ctx, o.ID, string(b), rep.Error)
}

// runSessionOp runs a relayed session op here.
func (s *Service) runSessionOp(ctx context.Context, project string, op api.MachineOp) (json.RawMessage, error) {
	sp, ok := orchcore.Sessions(s.d).(orchcore.SessionPipe)
	var so sessionOp
	if !ok || json.Unmarshal(op.Payload, &so) != nil {
		return nil, api.Errorf(api.CodeUnavailable, "this machine relays no session operations")
	}
	res, err := sp.PipeOp(ctx, project, so.Session, so.Op)
	if err != nil {
		var e *api.Error
		if errors.As(err, &e) && len(e.Data) > 0 {
			var r orchapi.SessionOpResult
			if json.Unmarshal(e.Data, &r) == nil {
				b, _ := json.Marshal(r)
				return b, err
			}
		}
		return nil, err
	}
	b, _ := json.Marshal(res)
	return b, nil
}

// ---- a member's machine ----

// relayPath: a structured session's read or operation naming a session on
// another machine.
func relayPath(method, path, machine string) bool {
	id, verb, ok := relayID(path)
	if !ok {
		return false
	}
	switch {
	case method == http.MethodGet && verb == "events":
	case method == http.MethodPost && (verb == "turn" || verb == "interrupt" || verb == "answer" || verb == "stop"):
	default:
		return false
	}
	a, err := api.ParseAddress(id)
	return err == nil && a.Kind == api.KindSession && a.Machine != machine
}

// relayID splits /v0/sessions/{id}/{verb}: an address holds a "/" itself.
func relayID(path string) (id, verb string, ok bool) {
	rest, ok := strings.CutPrefix(path, "/v0/sessions/")
	i := strings.LastIndex(rest, "/")
	if !ok || i <= 0 {
		return "", "", false
	}
	id, _ = url.PathUnescape(rest[:i])
	return id, rest[i+1:], true
}

// relayRoute sends another machine's structured-session call to the
// authority that has the session (here first, then each joined authority
// while 404). An event stream (SSE) is copied as it comes.
func (s *Service) relayRoute(w http.ResponseWriter, r *http.Request, next http.Handler, body []byte) {
	if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
		s.LocalFirst(w, r, next, body)
		return
	}
	id, _, _ := relayID(r.URL.Path)
	if orchcore.Sessions(s.d) != nil {
		if _, ok := orchcore.Sessions(s.d).Lookup(r.Context(), id); ok {
			next.ServeHTTP(w, r) // this machine is its authority
			return
		}
	}
	for _, x := range s.List() {
		if teamlink.Ended(x.Snapshot()) != nil {
			continue
		}
		resp, err := s.ForwardResp(r, x, "", false, body)
		if err != nil {
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			resp.Body.Close()
			continue
		}
		copyStream(w, resp)
		resp.Body.Close()
		return
	}
	next.ServeHTTP(w, r) // not found anywhere: as this machine answers
}

// copyStream copies an event stream to the caller as it arrives.
func copyStream(w http.ResponseWriter, resp *http.Response) {
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			return // the stream ended (or broke: the caller's stream ends too)
		}
	}
}
