package session

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
)

// Sizes of the scrollback kept per session and replayed on attach.
const (
	scrollbackSize = 1 << 20  // last 1 MiB in memory, written to disk when the session ends
	attachReplay   = 64 << 10 // replayed to each new attach, unless it asks for all of it
	tailSize       = 8 << 10  // the output tail outcome detectors look at
	enterDelay     = 150 * time.Millisecond
)

// proc is a live managed session as the daemon sees it: its holder process
// (package holder) owns the agent's process, the PTY and the scrollback; the
// daemon has its own descriptor for the PTY master (input, resize, the
// foreground group) and a mirror of the scrollback at the same offsets.
type proc struct {
	id8       string
	addr      string
	agent     string
	pid       int           // the agent's process (the holder's child)
	hc        *holder.Conn  // the holder's socket
	tie       io.Closer     // a tied holder's stdin (nil when it outlives the daemon)
	pty       *os.File      // the daemon's descriptor for the PTY master
	out       *holder.Ring  // mirror of the holder's ring
	exited    chan struct{} // closed once the session's end is recorded
	rawExited chan struct{} // holder reported exit, before a task stop sweep completes
	// replayEnd is the holder's end offset when this daemon adopted it: bytes
	// before it are history (no idle wake, no cursor-query answer); caught
	// closes once the mirror has them all.
	replayEnd int64
	caught    chan struct{}
	detached  atomic.Bool // the daemon let go of it on purpose (Close, keeping sessions)
	spawned   bool        // started by this daemon's Spawn, not readopted

	wmu sync.Mutex // serialises writes to the PTY (input, attach, nudge, prompt)

	mu        sync.Mutex
	state     orchapi.SessionState
	started   time.Time
	lastOut   time.Time // last PTY output (zero: none yet)
	timerIdle bool      // the idle timer set idle; new output sets running again
	// hookHuman: a hook (a permission prompt) set needs_human. Human input
	// (answered) followed by output sets running again; a mail-set
	// needs_human is cleared only by mail.
	hookHuman      bool
	answered       bool
	stateGen       uint64 // bumped by every reported state; a wake loses to a newer report
	stopped        bool   // zm session stop was asked
	taskSweepFinal bool   // a completed sweep already covered an ended task root
	stopSig        string
	stopBy         string
	daemonStop     bool // the daemon is shutting down
	lastNudge      time.Time
	sample         sample    // the sampler's last look (sample.go)
	sampledAt      time.Time // when fg, busy and jobs were last sampled
	// told is the sample events carry: set only by the sampler's cycle when
	// it publishes, never by a GET ?sample=1 (whose look only the asker
	// gets), so a later change or forget is measured against what readers
	// were actually told.
	told     sample
	portsAt  time.Time // when its ports were last read
	treeKey  string    // the process tree they were read for
	portsErr bool      // that read failed: the next waits portsEvery, tree changes or not
	// utf8Mouse: the program turned on UTF-8 mouse mode (DECSET 1005); modes
	// follows it in the output (read loop only).
	utf8Mouse atomic.Bool
	modes     modeWatch
	// bracketedPaste: the program turned on bracketed paste (DECSET 2004).
	bracketedPaste atomic.Bool
	// typedAt: when a person on this machine last typed into it (attach or
	// zm session input), for a shared session's in_use (shared.go).
	typedAt time.Time
	// draft: a person's keys may be waiting unsent in the program's input
	// (draft.go). lastKey is the last byte of them; draftFile keeps draft
	// across a daemon restart. draftKnown distinguishes proof from unknown
	// input inherited from a pre-tracking daemon.
	draft      bool
	draftKnown bool
	lastKey    byte
	draftFile  string
	// title is the latest terminal title parsed (tp, read loop only);
	// titleTimer, when set, writes it; titleAt is the last write.
	tp         titleParser
	title      string
	titleTimer *time.Timer
	titleAt    time.Time
	conns      map[io.Closer]struct{} // attached clients that may type (they answer cursor queries)
	viewers    map[io.Closer]struct{} // read-only attaches (?input=0): never counted as a terminal
	// pipe is set for a structured session (pipe.go): its stdin takes
	// operations, its output is an event stream.
	pipe *pipeState
}

func (p *proc) write(b []byte) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	_, err := p.pty.Write(b)
	return err
}

// humanInput records keystrokes from a person (input, attach): after a
// hook-set needs_human they mean the prompt was answered.
func (p *proc) humanInput() {
	p.mu.Lock()
	if p.hookHuman {
		p.answered = true
	}
	p.mu.Unlock()
}

// typeLine types text and, with enter, a carriage return a moment later, so
// TUIs that detect pastes treat the Enter as a submit. It holds the writes all
// the while: a person's key waits out the gap instead of being sent by this
// Enter.
func (p *proc) typeLine(text string, enter bool) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	return p.typeLocked(text, enter)
}

// typeInput keeps draft accounting in the same order as bytes and Enter.
// Holding wmu through the final note prevents a later writer's draft from
// being cleared by this input's Enter after that writer has started.
func (p *proc) typeInput(text string, enter bool) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.humanInput()
	p.typed()
	if text == "" && !enter {
		return nil
	}
	if err := p.prepareInput(); err != nil {
		return err
	}
	if text != "" {
		if _, err := p.pty.Write([]byte(text)); err != nil {
			p.mu.Lock()
			p.unknownDraftLocked()
			p.mu.Unlock()
			return err
		}
		if err := p.noteKeys([]byte(text)); err != nil {
			return err
		}
		if enter {
			time.Sleep(enterDelay)
		}
	}
	if enter {
		if err := p.prepareInput(); err != nil {
			return err
		}
		if _, err := p.pty.Write([]byte{'\r'}); err != nil {
			p.mu.Lock()
			p.unknownDraftLocked()
			p.mu.Unlock()
			return err
		}
		return p.noteKeys([]byte{'\r'})
	}
	return nil
}

// writeInput orders attach keys and their accounting with scripted input.
func (p *proc) writeInput(b []byte, human bool) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if human {
		p.humanInput()
		p.typed()
		if err := p.prepareInput(); err != nil {
			return err
		}
	}
	_, err := p.pty.Write(b)
	if human {
		if err != nil {
			p.mu.Lock()
			p.unknownDraftLocked()
			p.mu.Unlock()
		} else {
			err = p.noteKeys(b)
		}
	}
	return err
}

// typeLocked is typeLine with wmu held.
func (p *proc) typeLocked(text string, enter bool) error {
	if text != "" {
		if _, err := p.pty.Write([]byte(text)); err != nil {
			return err
		}
		if enter {
			time.Sleep(enterDelay)
		}
	}
	if enter {
		_, err := p.pty.Write([]byte{'\r'})
		return err
	}
	return nil
}

// signal asks the holder to send sig to the agent's process group (the PTY
// child is a session leader); with the holder unreachable the daemon sends
// it itself.
func (p *proc) signal(sig syscall.Signal) error {
	p.mu.Lock()
	by := p.stopBy
	p.mu.Unlock()
	if p.hc != nil && p.hc.Signal(signalName(sig), by) == nil {
		return nil
	}
	return fmt.Errorf("holder unreachable; process left running")
}

func (p *proc) addConn(c io.Closer, readOnly bool) {
	p.mu.Lock()
	if readOnly {
		if p.viewers == nil {
			p.viewers = map[io.Closer]struct{}{}
		}
		p.viewers[c] = struct{}{}
	} else {
		p.conns[c] = struct{}{}
	}
	p.mu.Unlock()
}

func (p *proc) removeConn(c io.Closer) {
	p.mu.Lock()
	delete(p.conns, c)
	delete(p.viewers, c)
	p.mu.Unlock()
}

func (p *proc) closeConns() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for c := range p.conns {
		_ = c.Close()
	}
	for c := range p.viewers {
		_ = c.Close()
	}
}

// waitCaught waits (briefly) until the mirror holds what the holder held
// when this daemon adopted it.
func (p *proc) waitCaught() {
	select {
	case <-p.caught:
	case <-time.After(catchUpWait):
	}
}

func (p *proc) isExited() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

var signals = map[string]syscall.Signal{"": syscall.SIGTERM, "TERM": syscall.SIGTERM, "INT": syscall.SIGINT,
	"KILL": syscall.SIGKILL, "HUP": syscall.SIGHUP}

func parseSignal(s string) (syscall.Signal, string, error) {
	name := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(s)), "SIG")
	sig, ok := signals[name]
	if !ok {
		return 0, "", api.Errorf(api.CodeBadRequest, "unknown signal %q (want TERM, INT, KILL or HUP)", s)
	}
	if name == "" {
		name = "TERM"
	}
	return sig, name, nil
}

// signalName is "TERM" for SIGTERM etc.
func signalName(sig syscall.Signal) string {
	for k, v := range signals {
		if v == sig && k != "" {
			return k
		}
	}
	return strings.ToUpper(strings.TrimPrefix(sig.String(), "signal "))
}

// lookPath finds name in pathEnv (a PATH value), like exec.LookPath but with
// the client's PATH instead of the daemon's.
func lookPath(name, pathEnv string) (string, error) {
	if strings.Contains(name, "/") {
		if isExecutable(name) {
			return name, nil
		}
		return "", api.Errorf(api.CodeBadRequest, "%s is not an executable file", name)
	}
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			dir = "."
		}
		if p := filepath.Join(dir, name); isExecutable(p) {
			return p, nil
		}
	}
	return "", api.Errorf(api.CodeBadRequest, "%q not found on PATH", name).
		WithHint("install it, pass its full path, or set config.json \"agents\"")
}

func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

// oneLine keeps the first line of s, without control characters, at most 200 bytes.
func oneLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.TrimSpace(s)
}

// clipLine keeps the first line of s without control characters (C0, DEL,
// C1) and at most orchapi.MaxActivityLen characters: what activity and terminal
// titles may hold.
func clipLine(s string) string {
	s, _, _ = strings.Cut(strings.ReplaceAll(s, "\r", "\n"), "\n")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > orchapi.MaxActivityLen {
		rs := []rune(s)
		s = strings.TrimSpace(string(rs[:orchapi.MaxActivityLen-1])) + "…"
	}
	return s
}
