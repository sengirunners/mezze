package session

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// Structured sessions (SPEC 8 "Structured sessions"): a pipe session's
// holder runs a zm-agent/1 adapter instead of a PTY program. Its stdout is
// the session's event stream, spooled whole by the holder; the daemon
// writes operations to its stdin and derives the session's state from the
// events. Nothing here checks an agent's name: what a session can do is the
// capabilities its adapter declared.

// Limits on what a caller hands an adapter.
const (
	maxEventLine  = 64 << 20 // as the adapter's own bound on a line
	maxTurnText   = 1 << 20
	maxEventsPage = 5000
	opWait        = 10 * time.Second
	maxEventsWait = 45 * time.Second
)

// pipeState is a live pipe session's view of its stream.
type pipeState struct {
	mu      sync.Mutex
	buf     []byte // a partial line
	skip    bool   // drop up to the first newline (the ring began mid-line)
	lastSeq int64
	notify  chan struct{}                      // closed and replaced when events arrive
	waiters map[string]chan orchapi.AgentEvent // op id -> its answer
	pending map[string]string                  // approval request -> kind
	tools   map[string]string                  // running tool item -> kind
	derived pipeDerived                        // what the stream says now
	applied pipeDerived                        // what the row was last given
	// startSent, on a session this daemon spawned, closes once its start op
	// is written: Spawn returns before that, and the adapter refuses any
	// op that comes first. nil: open (an adopted session already started).
	startSent chan struct{}
	startOnce sync.Once
}

// pipeDerived is the session row's part the stream decides.
type pipeDerived struct {
	state     orchapi.SessionState
	note      string
	activity  string
	runtimeID string
	model     string
}

func newPipeState() *pipeState {
	return &pipeState{notify: make(chan struct{}), waiters: map[string]chan orchapi.AgentEvent{},
		pending: map[string]string{}, tools: map[string]string{}, derived: pipeDerived{state: orchapi.StateRunning}}
}

// changed returns the channel closed by the next event.
func (ps *pipeState) changed() <-chan struct{} {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.notify
}

var activityOf = map[string]string{orchapi.AgentToolCommand: "Running a command", orchapi.AgentToolFileChange: "Editing files",
	orchapi.AgentToolMCP: "Using a tool", orchapi.AgentToolWeb: "Searching the web", orchapi.AgentToolTask: "Running a subagent",
	orchapi.AgentToolOther: "Using a tool"}

var approvalNote = map[string]string{orchapi.AgentApprovalCommand: "asks to run a command", orchapi.AgentApprovalFileChange: "asks to edit files",
	orchapi.AgentApprovalTool: "asks to use a tool", orchapi.AgentApprovalPermissions: "asks for more permissions",
	orchapi.AgentApprovalQuestion: "asks a question"}

// see folds one event into the derived state (ps.mu held). Notes and
// activity never carry a command, a path or the agent's words: they leave
// the machine in presence (Session.Activity).
func (ps *pipeState) see(ev orchapi.AgentEvent) {
	d := &ps.derived
	switch ev.Type {
	case orchapi.AgentEvSession:
		if ev.RuntimeID != "" {
			d.runtimeID = ev.RuntimeID
		}
		if ev.Model != "" {
			d.model = ev.Model
		}
		switch ev.Status {
		case orchapi.AgentStateReady:
			d.state, d.note, d.activity = orchapi.StateIdle, "", ""
			ps.tools = map[string]string{}
		case orchapi.AgentStateRunning, orchapi.AgentStateStarting:
			d.state, d.note = orchapi.StateRunning, ""
		case orchapi.AgentStateError:
			d.note = "runtime error"
		}
	case orchapi.AgentEvApproval:
		if ev.Status == orchapi.AgentItemPending {
			ps.pending[ev.Request] = ev.Kind
		} else {
			delete(ps.pending, ev.Request)
		}
	case orchapi.AgentEvTool:
		if ev.Status == orchapi.AgentItemRunning {
			ps.tools[ev.Item] = ev.Kind
		} else {
			delete(ps.tools, ev.Item)
		}
	case orchapi.AgentEvTurn:
		if orchapi.AgentTurnTerminal(ev.Status) {
			ps.tools = map[string]string{}
		}
	}
	if len(ps.pending) > 0 {
		var reqs []string
		for r := range ps.pending {
			reqs = append(reqs, r)
		}
		sort.Strings(reqs)
		d.state, d.note = orchapi.StateNeedsHuman, approvalNote[ps.pending[reqs[0]]]
		if d.note == "" {
			d.note = "asks for approval"
		}
	} else if d.state == orchapi.StateNeedsHuman {
		d.state, d.note = orchapi.StateRunning, ""
	}
	d.activity = ""
	if d.state == orchapi.StateRunning {
		d.activity = "Working"
		for _, k := range ps.tools {
			if a := activityOf[k]; a != "" {
				d.activity = a
				break
			}
		}
	}
}

// feedPipe takes a pipe session's stream bytes from the holder. History
// (before replayEnd) only rebuilds the derived state; the row is written
// once the stream is caught up, and on each change after.
func (s *Service) feedPipe(p *proc, off int64, data []byte) {
	p.out.WriteAt(off, data)
	end := off + int64(len(data))
	ps := p.pipe
	ps.mu.Lock()
	if len(ps.buf) == 0 && off > 0 && off == p.hc.Hello.Start && ps.lastSeq == 0 {
		ps.skip = true // the holder's ring starts mid-stream
	}
	ps.buf = append(ps.buf, data...)
	var answered []struct {
		ch chan orchapi.AgentEvent
		ev orchapi.AgentEvent
	}
	got := false
	for {
		i := bytes.IndexByte(ps.buf, '\n')
		if i < 0 {
			break
		}
		line := ps.buf[:i]
		ps.buf = ps.buf[i+1:]
		if ps.skip {
			ps.skip = false
			continue
		}
		var ev orchapi.AgentEvent
		if json.Unmarshal(line, &ev) != nil || ev.Seq <= ps.lastSeq {
			continue
		}
		ps.lastSeq, got = ev.Seq, true
		ps.see(ev)
		if ev.Type == orchapi.AgentEvOp && ev.Op != "" {
			if ch, ok := ps.waiters[ev.Op]; ok {
				delete(ps.waiters, ev.Op)
				answered = append(answered, struct {
					ch chan orchapi.AgentEvent
					ev orchapi.AgentEvent
				}{ch, ev})
			}
		}
	}
	if len(ps.buf) > maxEventLine {
		ps.buf = nil // a runaway line: drop it
		ps.skip = true
	}
	if got {
		close(ps.notify)
		ps.notify = make(chan struct{})
	}
	caughtUp := end >= p.replayEnd
	derived, applied := ps.derived, ps.applied
	if caughtUp {
		ps.applied = derived
	}
	ps.mu.Unlock()
	for _, a := range answered {
		a.ch <- a.ev
	}
	if caughtUp {
		select {
		case <-p.caught:
		default:
			close(p.caught)
		}
		if derived != applied {
			s.applyPipe(p, derived, applied)
		}
	}
}

// applyPipe writes the stream's state to the session row: one
// state_changed when the state or note changed, else an activity-only
// update.
func (s *Service) applyPipe(p *proc, d, was pipeDerived) {
	typ := ""
	_, err := s.update(s.ctx, p.id8, "", func(r *row) (bool, error) {
		if r.State == orchapi.StateEnded {
			return false, nil
		}
		wrote := false
		if d.runtimeID != "" && r.RuntimeID != d.runtimeID {
			r.RuntimeID, wrote = d.runtimeID, true
		}
		if d.model != "" && r.Model == "" {
			r.Model, wrote = d.model, true
		}
		if r.State != d.state || r.Note != d.note {
			r.State, r.Note, typ = d.state, d.note, orchapi.EventSessionStateChanged
		}
		if r.Activity != d.activity {
			r.Activity, wrote = d.activity, true
			if d.activity == "" {
				r.ActivityAt = nil
			} else {
				now := time.Now().UTC()
				r.ActivityAt = &now
			}
			if typ == "" {
				typ = activityOnly
			}
		}
		return typ != "" || wrote, nil
	})
	if err != nil {
		s.d.Log.Warn("pipe session state", "session", p.id8, "err", err)
		return
	}
	if typ != "" {
		if ss, ok := s.load(s.ctx, p.id8); ok {
			s.publish(typ, ss.Session)
		}
	}
}

// pipeAdapter is the adapter command for agent and what it can do: zm's
// own driver (`zm __zm-agent <agent>`, linked by cmd/zm), else a
// `zm-agent-<agent>` on PATH. Both are asked with describe, as processes:
// the session module links no driver and checks no agent's name.
func (s *Service) pipeAdapter(agent, pathEnv string) ([]string, orchapi.AgentCapabilities, error) {
	exe, err := s.holderExe()
	if err != nil {
		return nil, orchapi.AgentCapabilities{}, err
	}
	builtin := []string{exe, orchapi.AgentArg, agent}
	if d, err := s.describeAdapter(builtin, pathEnv, true); err == nil {
		return builtin, d.Capabilities, nil
	}
	bin, err := lookPath("zm-agent-"+agent, pathEnv)
	if err != nil {
		return nil, orchapi.AgentCapabilities{}, api.Errorf(api.CodeBadRequest, "no structured-session adapter for %s", agent).
			WithHint("zm drives claude and codex itself; another agent needs a zm-agent-" + agent + " on PATH (SPEC 8 \"Structured sessions\")")
	}
	d, err := s.describeAdapter([]string{bin}, pathEnv, false)
	if err != nil {
		return nil, orchapi.AgentCapabilities{}, api.Errorf(api.CodeBadRequest, "adapter %s: describe: %v", bin, err)
	}
	return []string{bin}, d.Capabilities, nil
}

// describeAdapter runs argv describe (5 s); zm's own answers are cached for
// the daemon's life.
func (s *Service) describeAdapter(argv []string, pathEnv string, cache bool) (orchapi.AgentDescribe, error) {
	key := strings.Join(argv, "\x00")
	if v, ok := s.adapters.Load(key); ok && cache {
		return v.(orchapi.AgentDescribe), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], append(argv[1:], "describe")...)
	cmd.Env = []string{"PATH=" + pathEnv}
	out, err := cmd.Output()
	var d orchapi.AgentDescribe
	if err == nil {
		err = json.Unmarshal(bytes.TrimSpace(out), &d)
	}
	if err == nil && d.Proto != orchapi.AgentProto {
		err = errors.New("it speaks " + d.Proto + ", not " + orchapi.AgentProto)
	}
	if err != nil {
		return d, err
	}
	if cache {
		s.adapters.Store(key, d)
	}
	return d, nil
}

func newOpID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "zm-" + hex.EncodeToString(b[:])
}

// pipeProc is the live pipe session id an operation is for, and its
// capabilities.
func (s *Service) pipeProc(ctx context.Context, sub *authz.Subject, id string) (*proc, orchapi.AgentCapabilities, error) {
	r, err := s.operable(ctx, sub, id)
	if err != nil {
		return nil, orchapi.AgentCapabilities{}, err
	}
	if r.Mode != orchapi.SessionModePipe {
		return nil, orchapi.AgentCapabilities{}, api.Errorf(api.CodeConflict, "session %s is a terminal session: it takes input, not turns", r.Address).
			WithHint("type into it: zm session input " + r.ID + "; start a structured one with zm run --pipe")
	}
	p, err := s.liveProc(r)
	if err != nil {
		return nil, orchapi.AgentCapabilities{}, err
	}
	if p.pipe == nil {
		return nil, orchapi.AgentCapabilities{}, api.Errorf(api.CodeConflict, "session %s is not reachable as a structured session", r.Address)
	}
	caps := orchapi.AgentCapabilities{}
	if r.Capabilities != nil {
		caps = *r.Capabilities
	}
	return p, caps, nil
}

// sendOp writes op to p's adapter and waits up to opWait for its answer.
func (s *Service) sendOp(ctx context.Context, p *proc, op orchapi.AgentOp) (orchapi.SessionOpResult, error) {
	if op.ID == "" {
		op.ID = newOpID()
	}
	b, err := json.Marshal(op)
	if err != nil {
		return orchapi.SessionOpResult{}, err
	}
	ch := make(chan orchapi.AgentEvent, 1)
	p.pipe.mu.Lock()
	p.pipe.waiters[op.ID] = ch
	p.pipe.mu.Unlock()
	defer func() {
		p.pipe.mu.Lock()
		delete(p.pipe.waiters, op.ID)
		p.pipe.mu.Unlock()
	}()
	if op.Op != orchapi.AgentOpStart && p.pipe.startSent != nil {
		select {
		case <-p.pipe.startSent:
		case <-p.exited:
			return orchapi.SessionOpResult{}, api.Errorf(api.CodeConflict, "session %s has ended", p.addr)
		case <-ctx.Done():
			return orchapi.SessionOpResult{}, api.Errorf(api.CodeConflict, "session %s is still starting", p.addr)
		}
	}
	err = p.write(append(b, '\n'))
	if op.Op == orchapi.AgentOpStart && p.pipe.startSent != nil {
		p.pipe.startOnce.Do(func() { close(p.pipe.startSent) }) // a failed write fails the next ones too
	}
	if err != nil {
		return orchapi.SessionOpResult{}, api.Errorf(api.CodeConflict, "session %s: write: %v", p.addr, err)
	}
	timer := time.NewTimer(opWait)
	defer timer.Stop()
	select {
	case ev := <-ch:
		res := orchapi.SessionOpResult{Op: op.ID, OK: ev.OK != nil && *ev.OK, Error: ev.Error}
		if !res.OK {
			code := api.CodeConflict
			if ev.Error != nil && (ev.Error.Kind == orchapi.AgentRefuseBadRequest || ev.Error.Kind == orchapi.AgentRefuseUnsupported) {
				code = api.CodeBadRequest
			}
			msg := "refused"
			if ev.Error != nil {
				msg = ev.Error.Message
			}
			return res, api.Errorf(code, "session %s: %s", p.addr, msg).WithData(res)
		}
		return res, nil
	case <-timer.C:
		return orchapi.SessionOpResult{Op: op.ID, Pending: true}, nil
	case <-p.exited:
		return orchapi.SessionOpResult{}, api.Errorf(api.CodeConflict, "session %s has ended", p.addr)
	case <-ctx.Done():
		return orchapi.SessionOpResult{Op: op.ID, Pending: true}, nil
	}
}

func refuseCap(addr, what string) error {
	return api.Errorf(api.CodeConflict, "session %s's adapter cannot %s", addr, what).
		WithHint("its capabilities are on the session (zm session show)")
}

// Turn starts a turn in a pipe session, or with Steer adds to the running one.
func (s *Service) Turn(ctx context.Context, sub *authz.Subject, id string, req orchapi.SessionTurnRequest) (orchapi.SessionOpResult, error) {
	p, caps, err := s.pipeProc(ctx, sub, id)
	if err != nil {
		return orchapi.SessionOpResult{}, err
	}
	if req.Text == "" && len(req.Images) == 0 {
		return orchapi.SessionOpResult{}, api.Errorf(api.CodeBadRequest, "a turn needs text")
	}
	if len(req.Text) > maxTurnText {
		return orchapi.SessionOpResult{}, api.Errorf(api.CodeBadRequest, "a turn's text is at most %d bytes", maxTurnText)
	}
	if len(req.Images) > 0 {
		if !caps.Images {
			return orchapi.SessionOpResult{}, refuseCap(p.addr, "take images")
		}
		for _, im := range req.Images {
			if fi, err := os.Stat(im); !filepath.IsAbs(im) || err != nil || !fi.Mode().IsRegular() {
				return orchapi.SessionOpResult{}, api.Errorf(api.CodeBadRequest, "image %q is not an absolute path to a file", im)
			}
		}
	}
	op := orchapi.AgentOp{Op: orchapi.AgentOpTurn, Text: req.Text, Images: req.Images, Model: req.Model}
	if req.Steer {
		if !caps.Steer {
			return orchapi.SessionOpResult{}, refuseCap(p.addr, "steer a running turn")
		}
		if req.Model != "" {
			return orchapi.SessionOpResult{}, api.Errorf(api.CodeBadRequest, "a steer cannot change the model")
		}
		op.Op, op.Turn = orchapi.AgentOpSteer, req.Turn
	} else if req.Model != "" && !caps.Model {
		return orchapi.SessionOpResult{}, refuseCap(p.addr, "change the model")
	}
	return s.sendOp(ctx, p, op)
}

// Interrupt ends a pipe session's running turn.
func (s *Service) Interrupt(ctx context.Context, sub *authz.Subject, id string, req orchapi.SessionInterruptRequest) (orchapi.SessionOpResult, error) {
	p, caps, err := s.pipeProc(ctx, sub, id)
	if err != nil {
		return orchapi.SessionOpResult{}, err
	}
	if !caps.Interrupt {
		return orchapi.SessionOpResult{}, refuseCap(p.addr, "interrupt a turn")
	}
	return s.sendOp(ctx, p, orchapi.AgentOp{Op: orchapi.AgentOpInterrupt, Turn: req.Turn})
}

// Answer answers a pipe session's pending approval.
func (s *Service) Answer(ctx context.Context, sub *authz.Subject, id string, req orchapi.SessionAnswerRequest) (orchapi.SessionOpResult, error) {
	p, caps, err := s.pipeProc(ctx, sub, id)
	if err != nil {
		return orchapi.SessionOpResult{}, err
	}
	if !caps.Approvals {
		return orchapi.SessionOpResult{}, refuseCap(p.addr, "ask for approvals")
	}
	switch req.Decision {
	case orchapi.AgentDecisionAccept, orchapi.AgentDecisionAcceptSession, orchapi.AgentDecisionDecline, orchapi.AgentDecisionCancel:
	default:
		return orchapi.SessionOpResult{}, api.Errorf(api.CodeBadRequest, "decision must be accept, accept_session, decline or cancel")
	}
	if req.Request == "" {
		return orchapi.SessionOpResult{}, api.Errorf(api.CodeBadRequest, "request is required")
	}
	return s.sendOp(ctx, p, orchapi.AgentOp{Op: orchapi.AgentOpAnswer, Request: req.Request, Decision: req.Decision, Answer: req.Answer})
}

// eventIndex remembers where each event starts in a spool file, so a read
// from a seq does not scan the whole file each time.
type eventIndex struct {
	mu   sync.Mutex
	size int64   // bytes indexed
	offs []int64 // offs[i]: where seq i+1 starts
}

func (s *Service) index(id8 string) *eventIndex {
	v, _ := s.pipeIdx.LoadOrStore(id8, &eventIndex{})
	return v.(*eventIndex)
}

// readEvents returns up to limit events after seq from id8's spool.
func (s *Service) readEvents(id8 string, after int64, limit int) ([]json.RawMessage, int64, bool, error) {
	path := holder.PipeEventsPath(s.d.Config.Home, id8)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, after, false, nil
	}
	if err != nil {
		return nil, after, false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, after, false, err
	}
	ix := s.index(id8)
	ix.mu.Lock()
	if fi.Size() < ix.size { // a new file (not expected): start over
		ix.size, ix.offs = 0, nil
	}
	if fi.Size() > ix.size {
		if _, err := f.Seek(ix.size, io.SeekStart); err != nil {
			ix.mu.Unlock()
			return nil, after, false, err
		}
		br := bufio.NewReaderSize(f, 64<<10)
		pos := ix.size
		for {
			line, err := br.ReadSlice('\n')
			if errors.Is(err, bufio.ErrBufferFull) {
				// A long event: skip to its end, counting its bytes.
				n := int64(len(line))
				for errors.Is(err, bufio.ErrBufferFull) {
					line, err = br.ReadSlice('\n')
					n += int64(len(line))
				}
				if err != nil {
					break
				}
				ix.offs = append(ix.offs, pos)
				pos += n
				continue
			}
			if err != nil {
				break // a partial last line waits for its end
			}
			ix.offs = append(ix.offs, pos)
			pos += int64(len(line))
		}
		ix.size = pos
	}
	offs := ix.offs
	size := ix.size
	ix.mu.Unlock()
	truncated := size >= holder.PipeSpoolLimit
	if after < 0 {
		after = 0
	}
	if after >= int64(len(offs)) {
		return nil, after, truncated, nil
	}
	if _, err := f.Seek(offs[after], io.SeekStart); err != nil {
		return nil, after, truncated, err
	}
	br := bufio.NewReaderSize(f, 64<<10)
	var out []json.RawMessage
	next := after
	for i := after; i < int64(len(offs)) && len(out) < limit; i++ {
		end := size
		if i+1 < int64(len(offs)) {
			end = offs[i+1]
		}
		b := make([]byte, end-offs[i])
		if _, err := io.ReadFull(br, b); err != nil {
			return out, next, truncated, err
		}
		b = bytes.TrimRight(b, "\r\n")
		if json.Valid(b) {
			out = append(out, json.RawMessage(b))
		}
		next = i + 1
	}
	return out, next, truncated, nil
}

// Events returns a pipe session's events after seq, waiting up to wait for
// one when none is there yet and the session runs.
func (s *Service) Events(ctx context.Context, sub *authz.Subject, id string, after int64, limit int, wait time.Duration) (orchapi.SessionEvents, error) {
	r, err := s.operable(ctx, sub, id)
	if err != nil {
		return orchapi.SessionEvents{}, err
	}
	if r.Mode != orchapi.SessionModePipe {
		return orchapi.SessionEvents{}, api.Errorf(api.CodeConflict, "session %s is a terminal session: it has output, not events", r.Address).
			WithHint("zm session output " + r.ID)
	}
	if limit <= 0 || limit > maxEventsPage {
		limit = maxEventsPage
	}
	wait = min(wait, maxEventsWait)
	deadline := time.Now().Add(wait)
	for {
		var changed <-chan struct{}
		p := s.proc(r.ID)
		if p != nil && p.pipe != nil {
			changed = p.pipe.changed() // before reading: an event between the two wakes us
		}
		var page orchapi.SessionEvents
		err := s.withEventPage(ctx, r.ID, after, limit, func(v orchapi.SessionEvents) error { page = v; return nil })
		if err != nil {
			return orchapi.SessionEvents{}, err
		}
		if len(page.Events) > 0 || page.State == orchapi.StateEnded || changed == nil || time.Until(deadline) <= 0 {
			return page, nil
		}
		t := time.NewTimer(time.Until(deadline))
		select {
		case <-changed:
		case <-p.exited:
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return orchapi.SessionEvents{Events: []json.RawMessage{}, Next: after, State: page.State}, nil
		}
		t.Stop()
	}
}

// withEventPage reads and consumes one page under the same output barrier.
// Stream handlers use it at delivery time rather than sending a cached page.
func (s *Service) withEventPage(ctx context.Context, id string, after int64, limit int, consume func(orchapi.SessionEvents) error) error {
	return s.withSessionOutput(ctx, id, func() error {
		evs, next, truncated, err := s.readEvents(id, after, limit)
		if err != nil {
			return err
		}
		cur, ok := s.load(ctx, id)
		if !ok {
			return api.NotFound("session", id)
		}
		if evs == nil {
			evs = []json.RawMessage{}
		}
		return consume(orchapi.SessionEvents{Events: evs, Next: next, State: cur.State,
			Ended: cur.State == orchapi.StateEnded && len(evs) == 0, Truncated: truncated})
	})
}

// pipeStart hands a new pipe session's adapter its start, then the first
// turn when the spawn carried a prompt.
func (s *Service) pipeStart(p *proc, st orchapi.AgentStart, prompt string) {
	res, err := s.sendOp(s.ctx, p, orchapi.AgentOp{Op: orchapi.AgentOpStart, Start: &st})
	if err != nil || (!res.OK && !res.Pending) {
		s.d.Log.Warn("pipe session start", "session", p.id8, "err", err)
		return
	}
	if prompt != "" {
		if _, err := s.sendOp(s.ctx, p, orchapi.AgentOp{Op: orchapi.AgentOpTurn, Text: prompt}); err != nil {
			s.d.Log.Warn("pipe session first turn", "session", p.id8, "err", err)
		}
	}
}

func refusePipe(addr, what string) error {
	return api.Errorf(api.CodeConflict, "session %s is a structured session: it takes turns, not %s", addr, what).
		WithHint("send a turn: zm session turn <id> <text> (POST /v0/sessions/{id}/turn)")
}

// pipeCapsFit refuses a pipe start that is malformed, or that asks the
// agent's adapter for what it did not declare.
func pipeCapsFit(agent string, req orchapi.SpawnRequest, caps orchapi.AgentCapabilities) error {
	if err := orchapi.CheckAgentMCP(req.MCP); err != nil {
		return err
	}
	if err := orchapi.CheckToolGrant(req.AllowedTools, req.PermissionMode); err != nil {
		return err
	}
	if err := orchapi.CheckSafeOutputs(req.SafeOutputs); err != nil {
		return err
	}
	switch {
	case req.Resume != "" && !caps.Resume:
		return api.Errorf(api.CodeBadRequest, "the %s adapter cannot resume", agent)
	case len(req.MCP) > 0 && !caps.MCP:
		return api.Errorf(api.CodeBadRequest, "the %s adapter cannot take mcp servers", agent)
	case (len(req.AllowedTools) > 0 || req.PermissionMode != "") && !caps.Tools:
		return api.Errorf(api.CodeBadRequest, "the %s adapter cannot take a tool grant", agent)
	}
	return nil
}
