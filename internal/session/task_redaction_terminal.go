package session

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// The terminal view used by both the scrubber and the verifier, each with its
// own parser: complete controls are removed; C1 controls (U+0080-U+009F) are
// their ESC forms; an unfinished control drops only its introducer, so its
// remaining bytes read as text; line breaks, whitespace (CSI n C is a space)
// and box-drawing characters are dropped, which joins hard wraps, indented
// continuations and TUI box borders. This is not a terminal emulator. The
// verifier also notes edits it cannot follow (checkTaskTerminal).

// isTaskTerminalBlank reports runes the view drops.
func isTaskTerminalBlank(r rune) bool {
	return r < 0x20 || r == 0x7f || r == ' ' || r >= 0x2500 && r <= 0x257f
}

// expandTaskC1 rewrites C1 controls, UTF-8 encoded or as stray 8-bit bytes,
// to ESC plus the equivalent byte. Both bytes keep the control's source span.
func expandTaskC1(x []normalizedByte) []normalizedByte {
	b := make([]byte, len(x))
	for i := range x {
		b[i] = x[i].b
	}
	out := make([]normalizedByte, 0, len(x))
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		c := byte(0)
		if size == 2 && r >= 0x80 && r <= 0x9f {
			c = byte(r) - 0x40
		} else if size == 1 && b[i] >= 0x80 && b[i] <= 0x9f {
			c = b[i] - 0x40
		}
		if c == 0 {
			out = append(out, x[i:i+size]...)
		} else {
			start, end := x[i].start, x[i+size-1].end
			out = append(out, normalizedByte{'\x1b', start, end}, normalizedByte{c, start, end})
		}
		i += size
	}
	return out
}

// normalizeTaskTerminal is the scrubber's view. Each kept byte retains its
// source span, so a match covers the intervening controls and line breaks.
func normalizeTaskTerminal(x []normalizedByte) []normalizedByte {
	x = expandTaskC1(x)
	b := make([]byte, len(x))
	for i := range x {
		b[i] = x[i].b
	}
	in := func(i int, lo, hi byte) bool { return i < len(b) && b[i] >= lo && b[i] <= hi }
	out := make([]normalizedByte, 0, len(x))
	for i := 0; i < len(b); {
		if b[i] != '\x1b' {
			r, size := utf8.DecodeRune(b[i:])
			if r == utf8.RuneError || !isTaskTerminalBlank(r) {
				out = append(out, x[i:i+size]...)
			}
			i += size
			continue
		}
		if i+1 == len(b) {
			break // cut off: nothing renders
		}
		j := i + 2
		switch b[i+1] {
		case '[':
			for in(j, 0x30, 0x3f) {
				j++
			}
			for in(j, 0x20, 0x2f) {
				j++
			}
			if !in(j, 0x40, 0x7e) {
				j = i + 2 // unfinished: its parameters read as text
			} else {
				j++
			}
		case ']', 'P', 'X', '^', '_':
			osc := b[i+1] == ']'
			for k := j; k < len(b); k++ {
				if osc && b[k] == '\a' {
					j = k + 1
					break
				}
				if b[k] == '\x1b' {
					if k+1 < len(b) && b[k+1] == '\\' {
						j = k + 2
					}
					break // otherwise unfinished: drop only the introducer
				}
			}
		default:
			k := i + 1
			for in(k, 0x20, 0x2f) {
				k++
			}
			if in(k, 0x30, 0x7e) {
				j = k + 1
			} else {
				j = i + 1 // unfinished: drop only ESC
			}
		}
		i = j
	}
	return out
}

// The verifier's independent parser. Complete controls, as one alternation;
// an introducer without its sequence matches the last alternative alone.
var taskTerminalControls = regexp.MustCompile(
	`\x1b\[([0-?]*[ -/]*)([@-~])|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)|\x1b[PX^_][^\x1b]*\x1b\\|\x1b([ -/]*)([0-~])`)

// CSI finals that only style, erase or set modes; C (cursor forward) reads
// as a space. Every other CSI moves the cursor or shifts cells.
const taskTerminalStill = "mKJhlqnct"

// checkTaskTerminal returns the verifier's view of input and whether input
// edits the screen in a way the view cannot follow: backspace, a carriage
// return that does not end a line, cursor positioning or movement, scrolling,
// cursor save/restore, or a cut-off or unknown control.
func checkTaskTerminal(input string) (view string, edits bool) {
	var c1 strings.Builder
	for i := 0; i < len(input); {
		r, size := utf8.DecodeRuneInString(input[i:])
		switch {
		case size == 2 && r >= 0x80 && r <= 0x9f:
			c1.WriteByte('\x1b')
			c1.WriteByte(byte(r) - 0x40)
		case size == 1 && input[i] >= 0x80 && input[i] <= 0x9f:
			c1.WriteByte('\x1b')
			c1.WriteByte(input[i] - 0x40)
		default:
			c1.WriteString(input[i : i+size])
		}
		i += size
	}
	s := c1.String()
	var plain strings.Builder
	last := 0
	for _, m := range taskTerminalControls.FindAllStringSubmatchIndex(s, -1) {
		plain.WriteString(s[last:m[0]])
		last = m[1]
		switch {
		case m[4] >= 0 && s[m[4]:m[5]] == "C":
			plain.WriteByte(' ')
		case m[4] >= 0:
			edits = edits || !strings.Contains(taskTerminalStill, s[m[4]:m[5]])
		case m[8] >= 0 && m[6] == m[7]:
			// Charset designations (with intermediates), keypad modes, NEL and a
			// stray ST are still; anything else, including an introducer whose
			// CSI or control string is unfinished, is an edit.
			edits = edits || !strings.Contains("=>E\\", s[m[8]:m[9]])
		}
	}
	plain.WriteString(s[last:])
	s = plain.String()
	var out strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '\b' || r == '\x1b':
			edits = true
		case r == '\r':
			j := i
			for j < len(s) && s[j] == '\r' {
				j++
			}
			edits = edits || j == len(s) || s[j] != '\n'
		case r == utf8.RuneError || !isTaskTerminalBlank(r):
			out.WriteString(s[i : i+size])
		}
		i += size
	}
	return out.String(), edits
}

// taskTerminalFragment reports whether view holds needle rearranged into
// pieces: one of half the text (at most 32 bytes), and 8-byte pieces covering
// all but 7 bytes, whatever the length. Used only when the screen edits are
// ambiguous. A rearranged copy prints every piece, so only pieces under 8
// bytes go uncovered (a two-piece swap is always caught); another secret of
// the same kind (a sibling webhook, a reissued or refresh JWT, a key under the
// same account prefix) differs in 8 bytes or more, shows only the shared part
// and must not hold an unrelated session. Texts up to the floor are checked
// whole only. The long piece is probed every stride bytes, so it is found
// from window+stride-1 bytes.
func taskTerminalFragment(view, needle string, minLen int) bool {
	floor := 16
	if minLen == 0 { // scoped: the session is working the redacted issue
		floor = 8
	}
	if len(needle) <= floor {
		return false
	}
	window := min(32, (len(needle)+1)/2)
	stride := max(1, (len(needle)-window)/64)
	for i := 0; ; i += stride {
		i = min(i, len(needle)-window)
		if strings.Contains(view, needle[i:i+window]) {
			break
		}
		if i == len(needle)-window {
			return false
		}
	}
	return len(needle)-taskTerminalCovered(view, needle) <= 7
}

// taskTerminalCovered counts the bytes of needle inside an 8-byte piece of
// needle that view also holds, in one pass over view. Each distinct piece is
// marked once, so a repetitive needle stays linear.
func taskTerminalCovered(view, needle string) int {
	const w = 8
	at := make(map[string][]int, len(needle)-w+1)
	for i := 0; i+w <= len(needle); i++ {
		at[needle[i:i+w]] = append(at[needle[i:i+w]], i)
	}
	hit := make([]bool, len(needle)-w+1)
	for j := 0; j+w <= len(view) && len(at) > 0; j++ {
		if is, ok := at[view[j:j+w]]; ok {
			for _, i := range is {
				hit[i] = true
			}
			delete(at, view[j:j+w])
		}
	}
	covered, end := 0, 0
	for i, h := range hit {
		if h {
			covered += i + w - max(i, end)
			end = i + w
		}
	}
	return covered
}

// taskTerminalInterleaved reports whether view holds needle in order within
// 4x its length: a cell-diff renderer streams text a few cells per frame and
// repaints other cells between frames. Each pass finds the earliest end of a
// subsequence match, then its latest start, so the window found is minimal.
func taskTerminalInterleaved(view, needle string) bool {
	span := 4 * len(needle)
	for from := 0; ; {
		k, e := 0, from
		for ; e < len(view) && k < len(needle); e++ {
			if view[e] == needle[k] {
				k++
			}
		}
		if k < len(needle) {
			return false
		}
		s := e - 1
		for k = len(needle) - 1; ; s-- {
			if view[s] == needle[k] {
				if k--; k < 0 {
					break
				}
			}
		}
		if e-s <= span {
			return true
		}
		from = s + 1
	}
}
