package main

// `zm attach --embed` (SPEC 8): the daemon replays a session's scrollback on
// attach, and the host terminal answers every query sequence in it (cursor
// position, device attributes, colours, version, modes). Those answers would
// reach the agent as typed input, as if a person had typed. While the
// replay is still arriving, replyFilter drops input that is exactly such an
// answer and passes everything else (a person's keys included).

import (
	"os"
	"strconv"
	"sync"
	"time"
)

const (
	rfNone   = iota
	rfEsc    // saw ESC
	rfCSI    // ESC [ params intermediates
	rfOSC    // ESC ] ...
	rfOSCEsc // ESC inside an OSC (ESC \ ends it)
	rfDCS    // ESC P ...
	rfDCSEsc // ESC inside a DCS
)

// maxReply bounds a held sequence: anything longer is passed as it is.
const maxReply = 4096

// replyFilter drops terminal answers to queries from an input stream,
// across read boundaries. A sequence cut at the end of a read is held until
// the next read completes it, or until Flush (the caller flushes after a
// short quiet spell, so a lone Escape key still arrives).
type replyFilter struct {
	st   int
	pend []byte
	// paste: inside a bracketed paste (ESC [ 200 ~ ... ESC [ 201 ~), passed
	// verbatim; pasteEnd counts the bytes of the end marker seen so far.
	paste    bool
	pasteEnd int
}

// pasteStart and pasteStop are the bracketed paste markers.
var pasteStart, pasteStop = []byte("\x1b[200~"), []byte("\x1b[201~")

// Pending reports bytes held back (an unfinished sequence; never inside a
// paste, which passes as it comes).
func (f *replyFilter) Pending() bool { return len(f.pend) > 0 }

// Flush returns the held bytes as they are and forgets them.
func (f *replyFilter) Flush() []byte {
	out := f.pend
	f.pend, f.st = nil, rfNone
	return out
}

// Feed reads b and returns what to pass on now.
func (f *replyFilter) Feed(b []byte) []byte {
	out, _ := f.feed(b, false)
	return out
}

// Resolve reads b only until the held sequence is resolved (dropped as an
// answer or passed), and returns what to pass and the unread rest. The
// caller passes the rest as it is: the window closed with a sequence half
// read, so it is finished first, never sent half as keys.
func (f *replyFilter) Resolve(b []byte) (out, rest []byte) {
	return f.feed(b, true)
}

func (f *replyFilter) feed(b []byte, untilResolved bool) ([]byte, []byte) {
	var out []byte
	pass := func() { out = append(out, f.pend...); f.pend, f.st = nil, rfNone }
	for i := 0; i < len(b); i++ {
		if untilResolved && f.st == rfNone && !f.paste {
			return out, b[i:]
		}
		c := b[i]
		if f.paste { // verbatim to the end marker
			out = append(out, c)
			if c == pasteStop[f.pasteEnd] {
				f.pasteEnd++
			} else if f.pasteEnd = 0; c == pasteStop[0] {
				f.pasteEnd = 1
			}
			if f.pasteEnd == len(pasteStop) {
				f.paste, f.pasteEnd = false, 0
			}
			continue
		}
		switch f.st {
		case rfNone:
			if c == 0x1b {
				f.pend, f.st = append(f.pend[:0], c), rfEsc
			} else {
				out = append(out, c)
			}
			continue
		case rfEsc:
			switch c {
			case '[':
				f.st = rfCSI
			case ']':
				f.st = rfOSC
			case 'P':
				f.st = rfDCS
			case 0x1b: // Escape pressed twice: the first passes
				out = append(out, 0x1b)
				continue
			default: // alt-<key>, SS3 (ESC O A) ...
				f.pend = append(f.pend, c)
				pass()
				continue
			}
			f.pend = append(f.pend, c)
		case rfCSI:
			switch {
			case c >= 0x20 && c <= 0x3f: // parameters, intermediates
				f.pend = append(f.pend, c)
			case c >= 0x40 && c <= 0x7e: // final byte
				f.pend = append(f.pend, c)
				switch {
				case csiReply(f.pend):
					f.pend, f.st = nil, rfNone
				case string(f.pend) == string(pasteStart):
					f.paste = true
					pass()
				default:
					pass()
				}
				continue
			default: // not a CSI after all: pass what was held, read c again
				pass()
				i--
				continue
			}
		case rfOSC:
			switch c {
			case 0x07:
				f.pend = append(f.pend, c)
				f.endOSC(&out)
				continue
			case 0x1b:
				f.st = rfOSCEsc
			}
			f.pend = append(f.pend, c)
		case rfDCS:
			if c == 0x1b {
				f.st = rfDCSEsc
			}
			f.pend = append(f.pend, c)
		case rfOSCEsc, rfDCSEsc:
			if c == '\\' {
				f.pend = append(f.pend, c)
				if f.st == rfOSCEsc {
					f.endOSC(&out)
				} else {
					f.pend, f.st = nil, rfNone // every DCS a terminal sends is an answer
				}
				continue
			}
			// The ESC started something else: pass what came before it.
			f.pend = f.pend[:len(f.pend)-1]
			pass()
			f.pend, f.st = []byte{0x1b}, rfEsc
			i--
			continue
		}
		if len(f.pend) > maxReply {
			pass()
		}
	}
	return out, nil
}

// endOSC drops a complete OSC colour answer (4, 10, 11, 12) and passes any
// other OSC.
func (f *replyFilter) endOSC(out *[]byte) {
	p := f.pend[2:] // after ESC ]
	n := 0
	for n < len(p) && p[n] >= '0' && p[n] <= '9' {
		n++
	}
	switch string(p[:n]) {
	case "4", "10", "11", "12":
		if n < len(p) && p[n] == ';' {
			f.pend, f.st = nil, rfNone
			return
		}
	}
	*out = append(*out, f.pend...)
	f.pend, f.st = nil, rfNone
}

// csiReply reports a complete CSI sequence (ESC [ ... final) that only a
// terminal answering a query sends, never a key: cursor position (R, also
// ?-prefixed), device attributes (?…c, >…c, =…c), device status (0n, 3n),
// mode reports ($y), the kitty keyboard flags (?…u) and window reports
// (digits…t).
func csiReply(seq []byte) bool {
	body := seq[2 : len(seq)-1]
	final := seq[len(seq)-1]
	prefix := byte(0)
	if len(body) > 0 && (body[0] == '?' || body[0] == '>' || body[0] == '=') {
		prefix, body = body[0], body[1:]
	}
	inter := ""
	for len(body) > 0 && body[len(body)-1] >= 0x20 && body[len(body)-1] <= 0x2f {
		inter = string(body[len(body)-1]) + inter
		body = body[:len(body)-1]
	}
	digits := len(body) > 0
	for _, c := range body {
		if (c < '0' || c > '9') && c != ';' {
			digits = false
		}
	}
	switch {
	case final == 'y' && inter == "$": // DECRPM: ESC [ ? 2004 ; 1 $ y
		return digits && (prefix == 0 || prefix == '?')
	case inter != "":
		return false
	case final == 'R': // CPR ESC [ r ; c R, DECXCPR ESC [ ? r ; c R
		if prefix == 0 && len(body) == 3 && body[0] == '1' && body[1] == ';' && body[2] >= '2' && body[2] <= '8' {
			return false // Shift/Alt/Ctrl+F3 (ESC [ 1 ; m R): a key, though a CPR at row 1 looks the same
		}
		return digits && (prefix == 0 || prefix == '?') && containsByte(body, ';')
	case final == 'c': // DA1 ESC [ ? … c, DA2 ESC [ > … c, DA3 ESC [ = … c
		return prefix != 0
	case final == 'n': // DSR: ESC [ 0 n (ok), ESC [ 3 n (malfunction)
		return prefix == 0 && (string(body) == "0" || string(body) == "3")
	case final == 'u': // kitty keyboard flags: ESC [ ? flags u
		return prefix == '?' && (len(body) == 0 || digits)
	case final == 't': // XTWINOPS reports: ESC [ 8 ; rows ; cols t
		return prefix == 0 && digits
	}
	return false
}

func containsByte(b []byte, c byte) bool {
	for _, x := range b {
		if x == c {
			return true
		}
	}
	return false
}

// replyWindow is how long the filter runs: from attach until output, once
// it has begun, has been quiet for replyQuiet (the replay and the agent's
// redraw are written), at most replyCap. Once closed it stays closed: later
// answers are to live queries, which the real terminal answers correctly.
type replyWindow struct {
	mu      sync.Mutex
	start   time.Time
	lastOut time.Time
	closed  bool
	now     func() time.Time
	quiet   time.Duration
	cap     time.Duration
}

const (
	replyQuiet = 250 * time.Millisecond
	replyCap   = 3 * time.Second
	// envReplyQuiet widens the quiet (ms) for a host a round trip away: over
	// ssh a terminal's answers arrive that much after the output (remote.go).
	// An environment variable, so an older zm on a box just ignores it.
	envReplyQuiet = "ZM_REPLY_QUIET_MS"
)

func newReplyWindow(now func() time.Time) *replyWindow {
	if now == nil {
		now = time.Now
	}
	w := &replyWindow{start: now(), now: now, quiet: replyQuiet, cap: replyCap}
	if ms, err := strconv.Atoi(os.Getenv(envReplyQuiet)); err == nil && ms > 0 && ms <= 10_000 {
		w.quiet = time.Duration(ms) * time.Millisecond
		w.cap += w.quiet
	}
	return w
}

// output records session output arriving.
func (w *replyWindow) output() {
	w.mu.Lock()
	w.lastOut = w.now()
	w.mu.Unlock()
}

// open reports whether input is still filtered.
func (w *replyWindow) open() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false
	}
	now := w.now()
	last := w.start
	if w.lastOut.After(last) {
		last = w.lastOut
	}
	// Quiet counts only once output has begun: on a loaded machine the
	// replay's first bytes may come late. Until then only the cap applies.
	if now.Sub(w.start) >= w.cap || (!w.lastOut.IsZero() && now.Sub(last) >= w.quiet) {
		w.closed = true
	}
	return !w.closed
}
