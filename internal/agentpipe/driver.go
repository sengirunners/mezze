package agentpipe

import (
	"encoding/json"

	"github.com/sengirunners/mezze/internal/orchapi"
	"sort"
	"sync"
	"time"
)

// A Driver translates between zm-agent/1 and one runtime's own wire format.
// The runner calls it from one goroutine only, so a driver keeps plain state
// and no locks. A driver never reads a clock or a random source: given the
// same operations and runtime lines it must emit the same events (the
// conformance replays depend on it).
type Driver interface {
	// Describe says what this driver can do.
	Describe() Describe
	// Argv is the runtime's argv for a start: cli is the configured command
	// ([bin, flags...]); the driver adds its protocol flags.
	Argv(cli []string, st Start) ([]string, error)
	// Begin runs once the runtime has started (a handshake, if any).
	Begin(io IO)
	// Op handles one operation (never start or stop: the runner does).
	// It answers every op with exactly one EvOp event.
	Op(io IO, op Op)
	// Line handles one line the runtime wrote on stdout.
	Line(io IO, line []byte)
	// Stop asks the runtime to end gracefully (the runner closes its stdin
	// after, then signals it if it lingers).
	Stop(io IO)
	// Exit runs once the runtime has exited: close open turns and items,
	// and emit the session's last state.
	Exit(io IO, code int, signaled bool)
}

// IO is a driver's side of the runner.
type IO interface {
	Emit(Event)          // the runner sets Seq and At
	Send(v any)          // one JSON line to the runtime's stdin
	Logf(string, ...any) // to the adapter's stderr (the session's pipe-err file)
}

var (
	regMu   sync.Mutex
	drivers = map[string]func() Driver{}
)

// Register makes a driver available by name (built-ins register in init).
func Register(name string, f func() Driver) {
	regMu.Lock()
	defer regMu.Unlock()
	drivers[name] = f
}

// New returns a fresh driver by name.
func New(name string) (Driver, bool) {
	regMu.Lock()
	defer regMu.Unlock()
	f, ok := drivers[name]
	if !ok {
		return nil, false
	}
	return f(), true
}

// Names lists the registered drivers.
func Names() []string {
	regMu.Lock()
	defer regMu.Unlock()
	var out []string
	for n := range drivers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Emitter stamps events: Seq from 1, At from Now. It is the one place a
// clock is read.
type Emitter struct {
	seq int64
	Now func() time.Time
	Out func(Event)
}

// Emit stamps and hands on one event.
func (e *Emitter) Emit(ev Event) {
	e.seq++
	ev.Seq = e.seq
	if e.Now != nil {
		ev.At = e.Now().UTC()
	}
	e.Out(ev)
}

// Count is how many events went out.
func (e *Emitter) Count() int64 { return e.seq }

// OpResult is the EvOp event answering op id.
func OpResult(id string, err *Error) Event {
	ok := err == nil
	return Event{Type: EvOp, Op: id, OK: &ok, Error: err}
}

// Refuse is an EvOp refusal.
func Refuse(id, kind, msg string) Event { return OpResult(id, &Error{Kind: kind, Message: msg}) }

// Refusal kinds an op may get (besides the runtime's own Err*).
const (
	RefuseUnsupported = orchapi.AgentRefuseUnsupported
	RefuseConflict    = orchapi.AgentRefuseConflict
	RefuseBadRequest  = orchapi.AgentRefuseBadRequest
)

// Clip bounds a string to n bytes, keeping its end (where a command's
// result is), on a UTF-8 boundary.
func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := len(s) - n
	for cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut++
	}
	return "…" + s[cut:]
}

// Head bounds a string to n bytes, keeping its start, on a UTF-8 boundary.
func Head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// Bounds on what one event carries.
const (
	MaxInput  = 8 << 10  // a tool's arguments
	MaxOutput = 32 << 10 // a tool's result
	MaxChunk  = 16 << 10 // one output delta
	MaxTitle  = 200
)

// BoundJSON is raw, or a JSON string of its head when raw is over MaxInput.
func BoundJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || len(raw) <= MaxInput {
		return raw
	}
	b, _ := json.Marshal(Head(string(raw), MaxInput))
	return b
}

// OneLine is s cut to its first line and MaxTitle bytes.
func OneLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' || s[i] == '\r' {
			s = s[:i]
			break
		}
	}
	return Head(s, MaxTitle)
}
