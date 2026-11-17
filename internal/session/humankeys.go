package session

// What counts as a person typing (SPEC 8). A terminal attached to a session
// sends more than keys: with focus reporting on (DECSET 1004) it sends
// ESC[I / ESC[O on every focus change, with mouse reporting on it sends
// mouse reports, and it answers the queries in the scrollback the daemon
// replays on attach (device attributes, cursor position, device status,
// modes, colours, version). They go to the program like any input, but they
// are not a person answering a prompt, so they never clear a hook-set
// needs_human. humanKeys classifies an attach stream, chunk by chunk,
// across chunk boundaries; it never holds bytes back (the caller forwards
// every chunk). Unlike `zm attach --embed`'s filter this is not limited to
// the replay: none of these shapes comes from a person's keys.

import "bytes"

const (
	hkNormal = iota
	hkEsc    // saw ESC
	hkCSI    // ESC [ and parameter bytes
	hkX10    // ESC [ M: three coordinate units follow
	hkOSC    // ESC ] ...
	hkOSCEsc // ESC inside an OSC (ESC \ ends it)
	hkDCS    // ESC P ...
	hkDCSEsc // ESC inside a DCS
)

// maxReport bounds a report being read; anything longer is a person's.
const maxReport = 256

type humanKeys struct {
	st     int
	params []byte // CSI parameters and intermediates, or the OSC body, so far
	units  int    // X10/UTF-8 mouse units still to read
	lead   bool   // an X10 unit's UTF-8 lead byte was read (extended mode)
	// utf8Mouse reports whether the program turned on UTF-8 mouse mode
	// (DECSET 1005); nil = never (plain X10 coordinates are single bytes).
	utf8Mouse func() bool
}

// Feed reads one chunk of input and reports whether any of it is a
// person's: anything but complete focus reports, mouse reports and answers
// to queries. A report cut at the end of a chunk is decided when the next
// chunk completes it, except a bare ESC, ESC [, ESC ] or ESC P, which may be
// the Escape key or alt-[, alt-], alt-P (counted now).
func (h *humanKeys) Feed(b []byte) bool {
	human := false
	reset := func() { h.st, h.params = hkNormal, h.params[:0] }
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch h.st {
		case hkNormal:
			if c == 0x1b {
				h.st = hkEsc
			} else {
				human = true
			}
		case hkEsc:
			switch c {
			case '[':
				h.st, h.params = hkCSI, h.params[:0]
			case ']':
				h.st, h.params = hkOSC, h.params[:0]
			case 'P':
				h.st, h.params = hkDCS, h.params[:0]
			case 0x1b: // Escape pressed, then another ESC
				human = true
			default: // alt-<key>, SS3 keys (ESC O A)
				human, h.st = true, hkNormal
			}
		case hkCSI:
			switch {
			case c == 'M' && len(h.params) == 0: // X10 / UTF-8 mouse: ESC [ M b x y
				h.st, h.units, h.lead = hkX10, 3, false
			case c >= 0x20 && c <= 0x3f && len(h.params) < maxReport:
				h.params = append(h.params, c)
			case c >= 0x40 && c <= 0x7e:
				if !isReport(h.params, c) {
					human = true
				}
				reset()
			default: // not a report after all: a person's, and c is read again
				human = true
				reset()
				i--
			}
		case hkX10:
			utf8 := h.utf8Mouse != nil && h.utf8Mouse()
			switch {
			case h.lead && c >= 0x80 && c <= 0xbf: // the UTF-8 unit's second byte
				h.lead = false
				h.units--
			case h.lead: // the lead was a unit of its own; c is read again
				h.lead = false
				if h.units--; h.units == 0 {
					h.st = hkNormal
				}
				i--
				continue
			case c < 0x20: // coordinates are 32 and up: this was not a report
				human, h.st = true, hkNormal
				i--
				continue
			case utf8 && c >= 0xc2 && c <= 0xdf: // a two-byte UTF-8 unit (mode 1005)
				h.lead = true
			default:
				h.units--
			}
			if h.units == 0 {
				h.st = hkNormal
			}
		case hkOSC:
			switch {
			case c == 0x07:
				if !isOSCAnswer(h.params) {
					human = true
				}
				reset()
			case c == 0x1b:
				h.st = hkOSCEsc
			case c < 0x20 || len(h.params) >= maxReport: // not an answer: a person's, c read again
				human = true
				reset()
				i--
			default:
				h.params = append(h.params, c)
			}
		case hkDCS:
			switch {
			case c == 0x1b:
				h.st = hkDCSEsc
			case c < 0x20 || len(h.params) >= maxReport:
				human = true
				reset()
				i--
			default:
				h.params = append(h.params, c) // counted only for the bound
			}
		case hkOSCEsc, hkDCSEsc:
			if c == '\\' { // ST: the string ended
				if h.st == hkOSCEsc && !isOSCAnswer(h.params) {
					human = true
				}
				reset() // every DCS a terminal sends is an answer
				continue
			}
			// The ESC started something else: what came before was a person's.
			human = true
			h.st, h.params = hkEsc, h.params[:0]
			i--
		}
	}
	switch {
	case h.st == hkEsc, (h.st == hkCSI || h.st == hkOSC || h.st == hkDCS) && len(h.params) == 0:
		human = true // ESC, ESC [, ESC ], ESC P alone may be Escape or an alt-key: counted now
	}
	return human
}

// isReport tells a complete CSI sequence (parameters with intermediates,
// final byte) that a terminal sends on its own, never a key:
//   - focus in/out: ESC [ I, ESC [ O
//   - mouse: SGR ESC [ < b ; x ; y M|m, urxvt ESC [ b ; x ; y M
//   - device attributes: ESC [ ? … c, ESC [ > … c, ESC [ = … c
//   - cursor position: ESC [ r ; c R, ESC [ ? r ; c R (but not ESC [ 1 ; 2..8 R,
//     Shift/Alt/Ctrl+F3, though a report at row 1 looks the same)
//   - device status: ESC [ 0 n, ESC [ 3 n
//   - mode reports: ESC [ ? … $ y, ESC [ … $ y
//   - kitty keyboard flags: ESC [ ? … u (key events have no '?')
//   - window reports: ESC [ digits … t
func isReport(params []byte, final byte) bool {
	prefix := byte(0)
	body := params
	if len(body) > 0 && (body[0] == '<' || body[0] == '?' || body[0] == '>' || body[0] == '=') {
		prefix, body = body[0], body[1:]
	}
	inter := byte(0)
	if n := len(body); n > 0 && body[n-1] >= 0x20 && body[n-1] <= 0x2f {
		inter, body = body[n-1], body[:n-1]
	}
	numbers, semis := numberList(body)
	switch {
	case inter == '$':
		return final == 'y' && (prefix == 0 || prefix == '?') && numbers
	case inter != 0:
		return false
	case len(params) == 0:
		return final == 'I' || final == 'O'
	case prefix == '<':
		return (final == 'M' || final == 'm') && numbers && semis == 2
	case final == 'c':
		return prefix == '?' || prefix == '>' || prefix == '='
	case final == 'R':
		if prefix == 0 && string(body) >= "1;2" && string(body) <= "1;8" && len(body) == 3 {
			return false // Shift/Alt/Ctrl+F3
		}
		return (prefix == 0 || prefix == '?') && numbers && semis >= 1
	case final == 'n':
		return prefix == 0 && (string(body) == "0" || string(body) == "3")
	case final == 'u':
		return prefix == '?' && (len(body) == 0 || numbers)
	case final == 't':
		return prefix == 0 && numbers
	case final == 'M':
		return prefix == 0 && numbers && semis == 2 // urxvt mouse
	}
	return false
}

// numberList: "d;d;…" with at least one digit in each part; semis counts
// the separators.
func numberList(p []byte) (ok bool, semis int) {
	digits := 0
	for _, c := range p {
		switch {
		case c >= '0' && c <= '9':
			digits++
		case c == ';' && digits > 0:
			semis, digits = semis+1, 0
		default:
			return false, 0
		}
	}
	return digits > 0, semis
}

// isOSCAnswer tells an OSC body (without ESC ] and the terminator) that
// answers a colour query: 4 ; n ; rgb:…, 10/11/12 ; rgb:….
func isOSCAnswer(body []byte) bool {
	n := 0
	for n < len(body) && body[n] >= '0' && body[n] <= '9' {
		n++
	}
	if n == len(body) || body[n] != ';' {
		return false
	}
	switch string(body[:n]) {
	case "4", "10", "11", "12":
		return true
	}
	return false
}

// modeWatch follows the program's DECSET/DECRST of UTF-8 mouse mode (1005)
// in its output, across reads, so an attach knows whether a mouse report's
// coordinates are single bytes (X10) or UTF-8; and of bracketed paste
// (2004), so a shared session's prompt of several lines is typed as one
// paste (SPEC 17.10). Only the read loop feeds it.
type modeWatch struct {
	tail []byte
	join []byte
	on   func(bool) // 1005
	// paste follows 2004 (nil: not watched).
	paste func(bool)
}

// mode1005 and mode2004 are the parameters looked for.
var mode1005, mode2004 = []byte("1005"), []byte("2004")

// feed reads one chunk of output.
func (m *modeWatch) feed(chunk []byte) {
	const keep = 24 // longer than any ESC [ ? … 1005 … h we care about
	if len(m.tail) > 0 {
		m.join = append(append(m.join[:0], m.tail...), chunk[:min(len(chunk), keep)]...)
		m.scan(m.join)
	}
	m.scan(chunk)
	if len(chunk) >= keep {
		m.tail = append(m.tail[:0], chunk[len(chunk)-keep:]...)
	} else {
		m.tail = append(m.tail, chunk...)
		if len(m.tail) > keep {
			m.tail = m.tail[len(m.tail)-keep:]
		}
	}
}

// scan applies every ESC [ ? … h|l in b that names 1005 or 2004, in order.
func (m *modeWatch) scan(b []byte) {
	if !bytes.Contains(b, mode1005) && (m.paste == nil || !bytes.Contains(b, mode2004)) {
		return
	}
	for i := 0; i+3 < len(b); i++ {
		if b[i] != 0x1b || b[i+1] != '[' || b[i+2] != '?' {
			continue
		}
		j := i + 3
		for j < len(b) && (b[j] >= '0' && b[j] <= '9' || b[j] == ';') {
			j++
		}
		if j == len(b) || (b[j] != 'h' && b[j] != 'l') {
			continue
		}
		for _, p := range splitSemis(b[i+3 : j]) {
			switch {
			case string(p) == "1005" && m.on != nil:
				m.on(b[j] == 'h')
			case string(p) == "2004" && m.paste != nil:
				m.paste(b[j] == 'h')
			}
		}
		i = j
	}
}

func splitSemis(b []byte) [][]byte {
	var out [][]byte
	start := 0
	for i, c := range b {
		if c == ';' {
			out = append(out, b[start:i])
			start = i + 1
		}
	}
	return append(out, b[start:])
}
