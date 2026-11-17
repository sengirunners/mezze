package agentpipe

import (
	"encoding/json"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// Session is an adapter's deterministic core: given operations, runtime
// lines and the runtime's exit in some order, it emits the same events
// (Seq included) and writes the same runtime lines. The runner feeds it from
// processes; the conformance replay feeds it a recording.
type Session struct {
	D    Driver
	Em   *Emitter
	Rec  *Recording
	Log  func(string, ...any)
	Send func([]byte) // to the runtime's stdin (nil: dropped)
	// Stopping is called once when an op stop (or the runner) asks the
	// runtime to end: the runner closes its stdin and arms its timers.
	Stopping func()

	started  bool
	stopping bool
	startID  string
	st       Start
}

type sio struct{ s *Session }

func (x sio) Emit(ev Event) { x.s.Em.Emit(ev) }
func (x sio) Send(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		x.s.Log("encode: %v", err)
		return
	}
	if x.s.Rec != nil {
		x.s.Rec.Write(RecIn, x.s.st.ScrubMCP(b), x.s.Em.Count())
	}
	if x.s.Send != nil {
		x.s.Send(b)
	}
}
func (x sio) Logf(format string, a ...any) { x.s.Log(format, a...) }

func (s *Session) io() IO { return sio{s} }

// Op handles one operation line. It returns the Start when this op is the
// start (the caller then starts the runtime and calls Started).
func (s *Session) Op(b []byte) *Start {
	var op Op
	if err := json.Unmarshal(b, &op); err != nil {
		s.Em.Emit(Refuse("", RefuseBadRequest, "not an operation: "+err.Error()))
		return nil
	}
	if s.Rec != nil {
		if op.Start != nil && len(op.Start.MCP) > 0 { // header values and URLs may be credentials
			red := op
			st := *op.Start
			st.MCP = orchapi.RedactedMCP(st.MCP)
			red.Start = &st
			if rb, err := json.Marshal(red); err == nil {
				b = op.Start.ScrubMCP(rb)
			}
		}
		s.Rec.WriteOp(b, s.Em.Count())
	}
	if !s.started {
		if op.Op != OpStart || op.Start == nil || op.Start.Cwd == "" {
			s.Em.Emit(Refuse(op.ID, RefuseBadRequest, "the first operation must be start, with a cwd"))
			return nil
		}
		if len(op.Start.MCP) > 0 {
			if !s.D.Describe().Capabilities.MCP {
				s.Em.Emit(Refuse(op.ID, RefuseUnsupported, "this adapter cannot take mcp servers"))
				return nil
			}
			if err := orchapi.CheckAgentMCP(op.Start.MCP); err != nil {
				s.Em.Emit(Refuse(op.ID, RefuseBadRequest, err.Error()))
				return nil
			}
		}
		if len(op.Start.AllowedTools) > 0 || op.Start.PermissionMode != "" {
			if !s.D.Describe().Capabilities.Tools {
				s.Em.Emit(Refuse(op.ID, RefuseUnsupported, "this adapter cannot take a tool grant"))
				return nil
			}
			if err := orchapi.CheckToolGrant(op.Start.AllowedTools, op.Start.PermissionMode); err != nil {
				s.Em.Emit(Refuse(op.ID, RefuseBadRequest, err.Error()))
				return nil
			}
		}
		s.started, s.startID, s.st = true, op.ID, *op.Start
		d := s.D.Describe()
		c := d.Capabilities
		s.Em.Emit(Event{Type: EvSession, Status: StateStarting, Capabilities: &c, Cwd: s.st.Cwd, Model: s.st.Model})
		st := s.st
		return &st
	}
	switch op.Op {
	case OpStart:
		s.Em.Emit(Refuse(op.ID, RefuseConflict, "already started"))
	case OpStop:
		s.Em.Emit(OpResult(op.ID, nil))
		s.Stop()
	default:
		if s.stopping {
			s.Em.Emit(Refuse(op.ID, RefuseConflict, "the session is stopping"))
			return nil
		}
		s.D.Op(s.io(), op)
	}
	return nil
}

// Started reports the runtime's start: nil error runs the driver's Begin.
func (s *Session) Started(err error) {
	if err != nil {
		s.Em.Emit(Refuse(s.startID, ErrInfra, err.Error()))
		s.Em.Emit(Event{Type: EvSession, Status: StateError, Error: &Error{Kind: ErrInfra, Message: err.Error()}})
		return
	}
	s.D.Begin(s.io())
	s.Em.Emit(OpResult(s.startID, nil))
}

// Stop asks the runtime to end (once).
func (s *Session) Stop() {
	if s.stopping {
		return
	}
	s.stopping = true
	s.D.Stop(s.io())
	if s.Stopping != nil {
		s.Stopping()
	}
}

// IsStopping reports whether a stop was asked.
func (s *Session) IsStopping() bool { return s.stopping }

// Line handles one runtime stdout line.
func (s *Session) Line(b []byte) {
	if s.Rec != nil {
		s.Rec.Write(RecOut, s.st.ScrubMCP(b), s.Em.Count())
	}
	s.D.Line(s.io(), b)
}

// Exit handles the runtime's exit.
func (s *Session) Exit(code int, signaled bool) {
	if s.Rec != nil {
		s.Rec.WriteExit(code, signaled, s.Em.Count())
	}
	s.D.Exit(s.io(), code, signaled || (s.stopping && code != 0))
}
