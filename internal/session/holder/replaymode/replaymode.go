// Package replaymode checkpoints a small, explicitly observed subset of Ghostty
// terminal modes. It is not a VT emulator or a screen/cursor snapshot. No I/O is
// performed. A Parser is a fixed-size value; copying it checkpoints partial input.
//
// Semantics target Ghostty v1.4.0, 83fb95624b0439ee660c2679b43b38c8b63769cd:
// src/termio/stream_handler.zig (setMode), src/terminal/Terminal.zig
// (switchScreenMode), src/terminal/parse_table.zig and src/terminal/modes.zig.
// Mouse resets clear the effective selection even when resetting another mode.
// ESC exits control strings. OSC/DCS high bytes are payload; APC/SOS/PM
// retain the target parse table's C1 transitions (even UTF-8 continuation bytes).
package replaymode

import "strconv"

// Event is the effective mouse reporting selector (not independent DEC bits).
type Event uint16

const (
	NoEvents     Event = 0
	X10Events    Event = 9
	NormalEvents Event = 1000
	ButtonEvents Event = 1002
	AnyEvents    Event = 1003
)

// Format is the effective mouse report encoding.
type Format uint16

const (
	X10Format   Format = 0
	UTF8Format  Format = 1005
	SGRFormat   Format = 1006
	URXVTFormat Format = 1015
	PixelFormat Format = 1016
)

// Modes contains observed state. A false Known flag means unknown, not disabled.
// Complete covers only this package's whitelist, never full terminal state.
type Modes struct {
	Alternate, AlternateKnown bool
	Mouse                     Event
	MouseKnown                bool
	Encoding                  Format
	EncodingKnown             bool
	Paste, PasteKnown         bool
}

func (m Modes) Complete() bool {
	return m.AlternateKnown && m.MouseKnown && m.EncodingKnown && m.PasteKnown
}

type grammar uint8

const (
	ground grammar = iota
	escape
	escapeIntermediate
	csi
	osc
	dcsHeader
	controlString // DCS payload
	apcString     // SOS/PM/APC retain C1 transitions
	unsynced
)
const maxParams = 16
const maxCSI = 128

// Parser's zero value starts at a known Ground boundary, with all modes unknown.
// Use Lost for a missing prefix whose grammar state is unknown. Feed only output
// bytes in order; callers must invalidate with Lost on a gap. Copy by value for
// a complete bounded checkpoint, including pending grammar/UTF-8 state.
type Parser struct {
	modes        Modes
	state        grammar
	utf8         uint8
	params       [maxParams]uint16
	count        uint8
	length       uint8
	private      bool
	digits       bool
	invalid      bool
	intermediate byte
}

// Lost starts without a known parser boundary or any assumed defaults.
func Lost() Parser { return Parser{state: unsynced} }

// Modes returns a copy of the observed whitelist state.
func (p Parser) Modes() Modes { return p.modes }

// SafeBoundary means the next byte can be rendered from Ground without exposing
// the suffix of a control string/escape/UTF-8 character as a new command.
// It says nothing about modes being complete or screen/cursor accuracy.
func (p Parser) SafeBoundary() bool { return p.state == ground && p.utf8 == 0 }

// Feed advances through bytes with constant memory, retaining no string payload.
func (p *Parser) Feed(b []byte) {
	for _, c := range b {
		p.feed(c)
	}
}
func (p *Parser) beginCSI() {
	p.state = csi
	p.params = [maxParams]uint16{}
	p.count = 1
	p.length = 0
	p.private = false
	p.digits = false
	p.invalid = false
	p.intermediate = 0
}
func (p *Parser) feed(c byte) {
	// Ground is UTF-8 only in Ghostty; raw C1 bytes never start commands.
	if p.state == ground && p.utf8 != 0 {
		if c >= 0x80 && c <= 0xbf {
			p.utf8--
			return
		}
		p.utf8 = 0 // invalid sequence; process its non-continuation normally
	}
	if c == 0x18 || c == 0x1a {
		p.state = ground
		p.utf8 = 0
		return
	}
	if c == 0x1b {
		p.state = escape
		p.utf8 = 0
		return
	}
	switch p.state {
	case unsynced:
		return // only ESC or CAN/SUB establishes a trustworthy boundary
	case ground:
		switch {
		case c >= 0xc2 && c <= 0xdf:
			p.utf8 = 1
		case c >= 0xe0 && c <= 0xef:
			p.utf8 = 2
		case c >= 0xf0 && c <= 0xf4:
			p.utf8 = 3
		}
	case escape:
		switch {
		case c == '[':
			p.beginCSI()
		case c == ']':
			p.state = osc
		case c == 'P':
			p.state = dcsHeader
		case c == 'X' || c == '^' || c == '_':
			p.state = apcString
		case c == 'c':
			p.modes = Modes{}
			p.state = ground // RIS: defaults/config unknown
		case c >= 0x20 && c <= 0x2f:
			p.state = escapeIntermediate
		case c >= 0x30 && c <= 0x7e:
			p.state = ground
		case c >= 0x80:
			p.modes = Modes{}
			p.state = unsynced // unsupported C1 in an escape
		}
	case escapeIntermediate:
		if c >= 0x30 && c <= 0x7e {
			p.state = ground
		}
		if c >= 0x80 {
			p.modes = Modes{}
			p.state = unsynced
		}
	case osc:
		if c == 7 {
			p.state = ground
		} // all high bytes remain payload
	case apcString:
		// Unlike OSC/DCS, this pinned state overrides only ASCII bytes.
		// Its C1 transitions apply even when the byte looks like a UTF-8
		// continuation: no UTF-8 decoder is active in this string state.
		switch {
		case c == 0x90:
			p.state = dcsHeader
		case c == 0x9b:
			p.beginCSI()
		case c == 0x9d:
			p.state = osc
		case c == 0x9c, c >= 0x80 && c <= 0x8f,
			c >= 0x91 && c <= 0x97, c == 0x99, c == 0x9a:
			p.state = ground
			// 98/9e/9f are self-transitions; a0..ff are ignored.
		}
	case controlString:
		// DCS high bytes and BEL are payload; ESC/CAN/SUB terminate above.
	case dcsHeader:
		if c >= 0x40 && c <= 0x7e {
			p.state = controlString
		}
		if c >= 0x80 {
			p.modes = Modes{}
			p.state = unsynced
		}
	case csi:
		p.csiByte(c)
	}
}
func (p *Parser) csiByte(c byte) {
	// C0 executes without becoming part of the CSI; DEL is ignored.
	if c < 0x20 || c == 0x7f {
		return
	}
	if c >= 0x80 {
		p.modes = Modes{}
		p.state = unsynced
		return
	}
	if p.length < maxCSI {
		p.length++
	} else {
		p.invalid = true
	}
	if c >= 0x40 && c <= 0x7e {
		if p.invalid || (p.private && !p.digits) {
			// Malformed/overflowing mode commands may have been partly
			// accepted by the target. Unrelated SGR/string bytes cannot
			// change this whitelist, so do not erase state for those.
			if c == 'h' || c == 'l' || c == 'r' || c == 'p' {
				p.modes = Modes{}
			}
		} else {
			p.dispatch(c)
		}
		p.state = ground
		return
	}
	if p.invalid {
		return
	}
	switch {
	case c == '?' && p.length == 1:
		p.private = true
	case c >= '0' && c <= '9' && p.intermediate == 0:
		n := uint32(p.params[p.count-1])*10 + uint32(c-'0')
		if n > 65535 {
			p.invalid = true
		} else {
			p.params[p.count-1] = uint16(n)
			p.digits = true
		}
	case c == ';' && p.intermediate == 0 && p.digits:
		if p.count == maxParams {
			p.invalid = true
		} else {
			p.count++
			p.digits = false
		}
	case c >= 0x20 && c <= 0x2f && p.intermediate == 0:
		p.intermediate = c
	default:
		p.invalid = true
	}
}
func (p *Parser) dispatch(final byte) {
	if !p.private {
		// Soft reset can change defaults and saved state; never invent them.
		if final == 'p' && p.intermediate == '!' {
			p.modes = Modes{}
		}
		return
	}
	if p.intermediate != 0 || !p.digits {
		return
	}
	switch final {
	case 'h', 'l':
		for _, n := range p.params[:p.count] {
			p.set(n, final == 'h')
		}
	case 'r':
		// Ghostty saves per-mode bits, which can differ from effective screen
		// and mouse selectors. A restore is deliberately unknown for its group.
		for _, n := range p.params[:p.count] {
			p.forget(n)
		}
		// 's' saves bits but does not change current effective modes.
	}
}
func (p *Parser) forget(n uint16) {
	switch n {
	case 47, 1047, 1049:
		p.modes.AlternateKnown = false
	case 9, 1000, 1002, 1003:
		p.modes.MouseKnown = false
	case 1005, 1006, 1015, 1016:
		p.modes.EncodingKnown = false
	case 2004:
		p.modes.PasteKnown = false
	}
}
func (p *Parser) set(n uint16, enabled bool) {
	switch n {
	case 47, 1047, 1049:
		p.modes.Alternate = enabled
		p.modes.AlternateKnown = true
	case 9, 1000, 1002, 1003:
		p.modes.Mouse = NoEvents
		if enabled {
			p.modes.Mouse = Event(n)
		}
		p.modes.MouseKnown = true
	case 1005, 1006, 1015, 1016:
		p.modes.Encoding = X10Format
		if enabled {
			p.modes.Encoding = Format(n)
		}
		p.modes.EncodingKnown = true
	case 2004:
		p.modes.Paste = enabled
		p.modes.PasteKnown = true
	}
}

// Prefix returns only static whitelisted DEC mode commands for a NEW frontend
// surface, or nil at an unsafe boundary. Unknown groups are omitted. It must
// never be written into the managed PTY or counted in raw output offsets.
// Screen selection uses47, avoiding1049's cursor-save/clear side effects. Cursor,
// saved state and prior screen contents are intentionally not reconstructed.
// Mouse selector/encoding resets precede their chosen effective selections.
func (p Parser) Prefix() []byte {
	if !p.SafeBoundary() {
		return nil
	}
	b := make([]byte, 0, 96)
	emit := func(n uint16, on bool) {
		b = append(b, "\x1b[?"...)
		b = strconv.AppendUint(b, uint64(n), 10)
		if on {
			b = append(b, 'h')
		} else {
			b = append(b, 'l')
		}
	}
	if p.modes.AlternateKnown {
		emit(47, p.modes.Alternate)
	}
	if p.modes.MouseKnown {
		emit(1000, false)
		if p.modes.Mouse != NoEvents {
			emit(uint16(p.modes.Mouse), true)
		}
	}
	if p.modes.EncodingKnown {
		emit(1006, false)
		if p.modes.Encoding != X10Format {
			emit(uint16(p.modes.Encoding), true)
		}
	}
	if p.modes.PasteKnown {
		emit(2004, p.modes.Paste)
	}
	return b
}
