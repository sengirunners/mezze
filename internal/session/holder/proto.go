// Package holder is a managed session's holder: a small process, one per
// session, that owns the session's PTY, its child and the 1 MiB scrollback
// ring, so a session outlives the daemon that started it (a restart, an
// upgrade, a crash). The daemon talks to it over one Unix socket in
// $ZM_HOME/holders (protocol zm-holder/1, below and SPEC 8) and re-adopts it
// when it starts again. Part of the session brick.
//
// Why our own code (docs/ARCHITECTURE.md rule 4): tmux, dtach and abduco are
// not on a stock Mac; dtach and abduco keep no scrollback; tmux re-emulates
// the terminal, keeps lines instead of the raw bytes and hands over no exit
// status. zm needs the raw byte stream at absolute offsets (replay,
// output?since=), the child's wait status (typed outcomes) and the PTY master
// itself (input, resize and tcgetpgrp at today's latency). It is built on
// creack/pty, which the daemon already used.
//
// The holder is the zm binary run as `zm __zm-holder <home> <session8>`; this
// package's init runs it before anything else in any binary that links the
// session module (zm, and the test binaries that start sessions).
package holder

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"time"

	"github.com/sengirunners/mezze/internal/session/holder/replaymode"
)

// Arg is argv[1] of a holder process.
const Arg = "__zm-holder"

// The protocol's version. A different major is never adopted; a minor bump
// only adds frame types and fields, which both sides ignore when unknown.
// Strings so a test build can set them (-ldflags -X).
var (
	ProtoMajor = "1"
	ProtoMinor = "2" // 2: pipe mode (Spec.Pipe, Hello.Pipe)
)

// Proto is the version this binary speaks.
func Proto() (major, minor int) {
	major, _ = strconv.Atoi(ProtoMajor)
	minor, _ = strconv.Atoi(ProtoMinor)
	return major, minor
}

// ProtoName is "zm-holder/<major>".
func ProtoName(major int) string { return fmt.Sprintf("zm-holder/%d", major) }

// Frame types. A frame is type (1 byte), payload length (4 bytes, big
// endian), payload.
const (
	FrameHello  byte = 'H' // holder -> daemon, first on every connection: Hello (JSON); its shape never changes, fields only grow
	FrameAdopt  byte = 'A' // daemon -> holder: Adopt (JSON)
	FrameFD     byte = 'F' // holder -> daemon: empty, carries the PTY master (SCM_RIGHTS)
	FrameReplay byte = 'M' // negotiated mode checkpoint immediately before its matching O
	FrameOutput byte = 'O' // holder -> daemon: 8-byte offset, then the bytes
	FrameExit   byte = 'X' // holder -> daemon after the last output: Exit (JSON)
	FrameSignal byte = 'S' // daemon -> holder: Signal (JSON)
	FrameAck    byte = 'K' // daemon -> holder: the exit is recorded, the holder may go
)

const headerLen = 5

// MaxFrame bounds a payload: one output chunk plus its offset, or a JSON body.
const MaxFrame = 1<<20 + 64

// Hello is what a holder says first.
type Hello struct {
	ReplayModes int    `json:"replay_modes,omitempty"` // bounded checkpoint schema version, currently1
	Proto       int    `json:"proto"`                  // major
	Minor       int    `json:"minor"`
	Version     string `json:"version"` // zm version and commit that started it
	Commit      string `json:"commit"`
	Session     string `json:"session"`
	Home        string `json:"home"`
	HolderPID   int    `json:"holder_pid"`
	ChildPID    int    `json:"child_pid"`
	GroupPID    int    `json:"group_pid,omitempty"`
	Start       int64  `json:"start"` // ring bounds
	End         int64  `json:"end"`
	Exited      bool   `json:"exited,omitempty"` // the child exited: read the exit file
	Pipe        bool   `json:"pipe,omitempty"`   // pipe mode: F carries the child's stdin, O its stdout
}

// Adopt asks for the PTY master and output from Since.
type Adopt struct {
	Since       int64 `json:"since"`
	ReplayModes int   `json:"replay_modes,omitempty"`
}

// Exit is how the child ended (also the exit file).
type Exit struct {
	Code         int       `json:"code"`                    // exit code, -1 when a signal ended it
	Signal       string    `json:"signal,omitempty"`        // TERM, KILL, ...
	OutBytes     int64     `json:"out_bytes"`               // everything it ever printed
	GroupStopped *bool     `json:"group_stopped,omitempty"` // task holder successfully signalled the group
	StopSig      string    `json:"stop_sig,omitempty"`
	StopBy       string    `json:"stop_by,omitempty"` // who asked (a Signal's By)
	At           time.Time `json:"at"`
}

// Signal asks the holder to signal the child's process group.
type Signal struct {
	Sig string `json:"sig"` // TERM, INT, HUP, KILL
	By  string `json:"by,omitempty"`
}

// Spec is what the daemon hands a new holder on its stdin (one JSON line).
type Spec struct {
	Session    string   `json:"session"`
	Home       string   `json:"home"`
	Path       string   `json:"path"` // the executable
	Args       []string `json:"args"` // argv, argv[0] included
	Dir        string   `json:"dir"`
	Env        []string `json:"env"`
	Rows       int      `json:"rows"`
	Cols       int      `json:"cols"`
	TaskPrompt string   `json:"task_prompt,omitempty"`
	Task       bool     `json:"task,omitempty"`

	RecordedLaunch bool `json:"recorded_launch,omitempty"` // fd3 waits for durable task group recording
	// Pipe runs the child on pipes instead of a PTY (SPEC 8 "Structured
	// sessions"): F carries the write end of its stdin, O frames its stdout
	// (a structured event stream, also spooled to PipeEventsPath), and its
	// stderr goes to PipeErrorPath. Never with Task.
	Pipe bool `json:"pipe,omitempty"`
	// Tied: end the child when stdin reaches EOF (the daemon that started it
	// went away). Every daemon but `zm daemon` ties its holders.
	Tied bool `json:"tied,omitempty"`
	// Nice: lower the child's process group to this nice value once it runs
	// (0: as the holder), so an agent's builds yield to interactive UIs.
	Nice int `json:"nice,omitempty"`
}

// Ready is the holder's one line on stdout once its child runs.
type Ready struct {
	ChildPID int    `json:"child_pid,omitempty"`
	GroupPID int    `json:"group_pid,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Paths, all under $ZM_HOME.
func Dir(home string) string                 { return filepath.Join(home, "holders") }
func SocketPath(home, session string) string { return filepath.Join(Dir(home), session+".sock") }
func ExitPath(home, session string) string   { return filepath.Join(Dir(home), session+".exit") }
func LogPath(home, session string) string    { return filepath.Join(Dir(home), session+".log") }
func DraftPath(home, session string) string  { return filepath.Join(Dir(home), session+".draft") }
func TaskOutputPath(home, session string) string {
	return filepath.Join(home, "sessions", session+".task")
}
func TaskErrorPath(home, session string) string {
	return filepath.Join(home, "sessions", session+".task-err")
}
func TaskOverflowPath(home, session string) string {
	return filepath.Join(home, "sessions", session+".task-overflow")
}

// PipeEventsPath is a pipe session's whole stdout (its event stream), up to
// PipeSpoolLimit; PipeErrorPath its stderr, up to PipeErrLimit.
func PipeEventsPath(home, session string) string {
	return filepath.Join(home, "sessions", session+".events")
}
func PipeErrorPath(home, session string) string {
	return filepath.Join(home, "sessions", session+".pipe-err")
}

const (
	PipeSpoolLimit = 64 << 20
	PipeErrLimit   = 4 << 20
)

// ScrollbackPath is where an ended session's last output is kept (SPEC 2).
func ScrollbackPath(home, session string) string {
	return filepath.Join(home, "sessions", session+".out")
}

// ErrFrame is a malformed frame.
var ErrFrame = errors.New("holder: malformed frame")

// WriteFrame writes one frame.
func WriteFrame(w io.Writer, typ byte, payload []byte) error {
	b := make([]byte, headerLen+len(payload))
	b[0] = typ
	binary.BigEndian.PutUint32(b[1:], uint32(len(payload)))
	copy(b[headerLen:], payload)
	_, err := w.Write(b)
	return err
}

// WriteJSON writes one frame with v as JSON.
func WriteJSON(w io.Writer, typ byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, typ, b)
}

// ReadFrame reads one frame.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var h [headerLen]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > MaxFrame {
		return 0, nil, ErrFrame
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, nil, err
	}
	return h[0], b, nil
}

// outputFrame is an O frame's payload.
func outputFrame(off int64, data []byte) []byte {
	b := make([]byte, 8+len(data))
	binary.BigEndian.PutUint64(b, uint64(off))
	copy(b[8:], data)
	return b
}

// ParseOutput splits an O frame's payload into its offset and bytes.
func ParseOutput(p []byte) (int64, []byte, error) {
	if len(p) < 8 {
		return 0, nil, ErrFrame
	}
	return int64(binary.BigEndian.Uint64(p)), p[8:], nil
}

// ReplayFrameSize is fixed; metadata never includes raw escape/string content.
const ReplayFrameSize = 8 + replaymode.CheckpointSize

// EncodeReplay emits schema1 checkpoint metadata at an exact nonnegative offset.
func EncodeReplay(c ReplayCheckpoint) ([]byte, error) {
	if c.From < 0 {
		return nil, ErrFrame
	}
	state, err := c.State.MarshalBinary()
	if err != nil {
		return nil, err
	}
	b := make([]byte, ReplayFrameSize)
	binary.BigEndian.PutUint64(b, uint64(c.From))
	copy(b[8:], state)
	return b, nil
}

// ParseReplay rejects unknown/malformed schemas without echoing input data.
func ParseReplay(b []byte) (ReplayCheckpoint, error) {
	if len(b) != ReplayFrameSize {
		return ReplayCheckpoint{}, ErrFrame
	}
	off := int64(binary.BigEndian.Uint64(b))
	if off < 0 {
		return ReplayCheckpoint{}, ErrFrame
	}
	var p replaymode.Parser
	if err := p.UnmarshalBinary(b[8:]); err != nil {
		return ReplayCheckpoint{}, ErrFrame
	}
	return ReplayCheckpoint{From: off, State: p}, nil
}
