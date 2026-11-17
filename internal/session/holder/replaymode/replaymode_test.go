package replaymode

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

const enabled = "\x1b[?1049;1003;1006;2004h"

func parsed(s string) Parser { var p Parser; p.Feed([]byte(s)); return p }
func TestGhosttyEffectiveSelectorsAndPrefix(t *testing.T) {
	cases := []struct {
		input  string
		alt    bool
		event  Event
		format Format
		paste  bool
	}{
		{enabled, true, AnyEvents, SGRFormat, true},
		{enabled + "\x1b[?47l\x1b[?1000l\x1b[?1005l\x1b[?2004l", false, NoEvents, X10Format, false},
		{enabled + "\x1b[?1047l\x1b[?1002h\x1b[?1016h", false, ButtonEvents, PixelFormat, true},
		{enabled + "\x1b[?47l\x1b[?1047h\x1b[?1000;9h\x1b[?1015h", true, X10Events, URXVTFormat, true},
	}
	for _, tc := range cases {
		p := parsed(tc.input)
		m := p.Modes()
		if !m.Complete() || m.Alternate != tc.alt || m.Mouse != tc.event || m.Encoding != tc.format || m.Paste != tc.paste {
			t.Fatalf("%q: %+v", tc.input, m)
		}
		prefix := p.Prefix()
		if bytes.Contains(prefix, []byte("1049")) || bytes.Contains(prefix, []byte("1047")) {
			t.Fatal("prefix saves/clears cursor")
		}
		q := parsed(string(prefix))
		if q.Modes() != m {
			t.Fatalf("prefix does not reconstruct supported effective state: %q %+v %+v", prefix, m, q.Modes())
		}
	}
}
func TestEverySplitAndBytewiseCheckpoint(t *testing.T) {
	input := enabled + "\x1b]52;c;do-not-retain\a\x1bP1;2qpayload\x1b\\\x1b[?47l\x1b[?1005h"
	whole := parsed(input)
	for i := 0; i <= len(input); i++ {
		var a Parser
		a.Feed([]byte(input[:i]))
		b := a // checkpoint partial grammar
		b.Feed([]byte(input[i:]))
		if b != whole {
			t.Fatalf("split %d differs", i)
		}
	}
	var a Parser
	for i := range input {
		a.Feed([]byte(input[i : i+1]))
	}
	if a != whole {
		t.Fatal("bytewise differs")
	}
}
func TestControlStringsC1CancellationAndEscape(t *testing.T) {
	for _, open := range []string{"\x1b]52;c;", "\x1bPq"} {
		for _, end := range []string{"\x1b\\", "\x18", "\x1a"} {
			p := parsed(enabled)
			p.Feed([]byte(open + "?47l \xc3\x9b?1049l \xc3\x9c \x9b?2004l"))
			if p.SafeBoundary() || p.Prefix() != nil || !p.Modes().Complete() || !p.Modes().Alternate || !p.Modes().Paste {
				t.Fatalf("string escaped payload: %q %+v", open, p)
			}
			p.Feed([]byte(end))
			if !p.SafeBoundary() || !p.Modes().Alternate {
				t.Fatalf("terminator %q", end)
			}
		}
		// Pinned Ghostty ESC aborts strings: ESC [ is a REAL CSI, not payload.
		p := parsed(enabled + open + "payload\x1b[?47l")
		if !p.SafeBoundary() || !p.Modes().AlternateKnown || p.Modes().Alternate {
			t.Fatalf("ESC did not abort %q", open)
		}
	}
	p := parsed(enabled + "\x1b]52;opaque\a")
	if !p.SafeBoundary() {
		t.Fatal("OSC BEL")
	}
	p = parsed(enabled + "\x1bPqopaque\a")
	if p.SafeBoundary() {
		t.Fatal("DCS BEL incorrectly terminated")
	}
}
func TestUnsafeBoundaryAndUTF8(t *testing.T) {
	for _, s := range []string{"\x1b", "\x1b[?104", "\x1b]52;x", "\x1bP1;", "\x1bPqdata", "\xe2", "\xe2\x9b"} {
		p := parsed(enabled + s)
		if p.SafeBoundary() || len(p.Prefix()) != 0 {
			t.Fatalf("unsafe boundary accepted: %q", s)
		}
	}
	p := parsed(enabled + "\xc3\x9b?47l")
	if !p.SafeBoundary() || !p.Modes().Alternate {
		t.Fatal("UTF8 continuation parsed as CSI")
	}
	p = parsed(enabled + "\x9b?47l")
	if !p.SafeBoundary() || !p.Modes().Alternate {
		t.Fatal("raw C1 in Ground parsed as CSI")
	}
	p = parsed(enabled + "\xe2\x1b[?47l")
	if !p.SafeBoundary() || p.Modes().Alternate {
		t.Fatal("invalid UTF8 hid ESC")
	}
}
func TestUnknownAndResetRestore(t *testing.T) {
	var p Parser
	if p.Modes().Complete() || len(p.Prefix()) != 0 || !p.SafeBoundary() {
		t.Fatal("invented initial mode")
	}
	p = Lost()
	p.Feed([]byte("tail ?1049h"))
	if p.SafeBoundary() || len(p.Prefix()) != 0 {
		t.Fatal("invented lost boundary")
	}
	p.Feed([]byte(enabled))
	if !p.SafeBoundary() || !p.Modes().Complete() {
		t.Fatal("ESC did not resynchronize")
	}
	p.Feed([]byte("\x1b[?1049s"))
	if !p.Modes().Complete() {
		t.Fatal("save changed current state")
	}
	p.Feed([]byte("\x1b[?47r"))
	if p.Modes().AlternateKnown || !p.Modes().MouseKnown {
		t.Fatal("restore did not isolate unknown group")
	}
	for _, reset := range []string{"\x1bc", "\x1b[!p"} {
		p = parsed(enabled + reset)
		if p.Modes().Complete() || len(p.Prefix()) != 0 || !p.SafeBoundary() {
			t.Fatalf("guessed reset defaults: %q", reset)
		}
	}
	p = parsed(enabled + "\x1b[?9999h\x1b[38:2:1:2:3m")
	if !p.Modes().Complete() {
		t.Fatal("unsupported unrelated mode/SGR erased whitelist")
	}
}
func TestOverflowAndMalformedNeverInventModes(t *testing.T) {
	for _, bad := range []string{
		"\x1b[?9999999999999999999999999999999991049h",
		"\x1b[?" + strings.Repeat("1;", 40) + "1049h",
		"\x1b[?" + strings.Repeat("0", 200) + "1049h",
		"\x1b[?1049;h", "\x1b[?1049:2004h",
	} {
		p := parsed(enabled + bad)
		if p.Modes().Complete() || len(p.Prefix()) != 0 || !p.SafeBoundary() {
			t.Fatalf("malformed mode retained guessed state %q %+v", bad, p)
		}
		p.Feed([]byte(enabled))
		if !p.Modes().Complete() {
			t.Fatal("did not recover after complete observed commands")
		}
	}
}
func TestBoundedPayloadMemoryAndSafePrefix(t *testing.T) {
	// Fixed-size value, no slice/map/string/pointer fields, even in nested state.
	var check func(reflect.Type)
	check = func(ty reflect.Type) {
		switch ty.Kind() {
		case reflect.Struct:
			for i := 0; i < ty.NumField(); i++ {
				check(ty.Field(i).Type)
			}
		case reflect.Array:
			check(ty.Elem())
		case reflect.Slice, reflect.Map, reflect.String, reflect.Pointer:
			t.Fatalf("variable storage %v", ty)
		}
	}
	check(reflect.TypeOf(Parser{}))
	if unsafe.Sizeof(Parser{}) > 128 {
		t.Fatal("checkpoint unexpectedly large")
	}
	payload := bytes.Repeat([]byte("synthetic-opaque-\x9b?47l"), 4096)
	p := parsed(enabled + "\x1b]52;c;")
	allocs := testing.AllocsPerRun(1, func() {
		for i := 0; i < 32; i++ {
			p.Feed(payload)
		}
	})
	if allocs != 0 || p.SafeBoundary() || !p.Modes().Alternate {
		t.Fatalf("unbounded/string state: %v %+v", allocs, p)
	}
	p.Feed([]byte("\a"))
	prefix := string(p.Prefix())
	if prefix != "\x1b[?47h\x1b[?1000l\x1b[?1003h\x1b[?1006l\x1b[?1006h\x1b[?2004h" {
		t.Fatalf("unexpected commands %q", prefix)
	}
}
func FuzzChunkInvariant(f *testing.F) {
	for _, s := range []string{enabled, "\x1b]52;c;\x1b[?1049h", "\xe2\x9b\x9c\x1bPq\x18"} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var whole, split Parser
		whole.Feed(b)
		for _, c := range b {
			split.Feed([]byte{c})
		}
		if whole != split {
			t.Fatal("chunk dependent")
		}
		if !whole.SafeBoundary() && whole.Prefix() != nil {
			t.Fatal("unsafe prefix")
		}
		// Re-parsing a generated prefix must produce identical known state;
		// unknown fields deliberately remain unknown on a fresh parser.
		if whole.SafeBoundary() {
			q := parsed(string(whole.Prefix()))
			if q.Modes() != whole.Modes() {
				// Unknown groups may retain historical values, which carry no claim.
				a, z := q.Modes(), whole.Modes()
				if !z.AlternateKnown {
					z.Alternate = false
				}
				if !z.MouseKnown {
					z.Mouse = 0
				}
				if !z.EncodingKnown {
					z.Encoding = 0
				}
				if !z.PasteKnown {
					z.Paste = false
				}
				if a != z {
					t.Fatalf("prefix mismatch %+v %+v", a, z)
				}
			}
		}
	})
}

func TestSyntheticEvictedPrefixCheckpoint(t *testing.T) {
	// Invented full-screen redraws exceed the holder's existing 1MiB. This
	// exercises a future floor checkpoint without changing Ring or any I/O.
	stream := []byte(enabled + strings.Repeat("\x1b[HInvented redraw\r\n\x1b[38:2:10:20:30m", 40000))
	floor := len(stream) - (1 << 20)
	if floor <= 0 {
		t.Fatal("fixture did not evict mode setup")
	}
	var checkpoint Parser
	checkpoint.Feed(stream[:floor])
	checkpoint.Feed(stream[floor:])
	var complete Parser
	complete.Feed(stream)
	if checkpoint.Modes() != complete.Modes() || !checkpoint.Modes().Complete() || !checkpoint.Modes().Alternate || checkpoint.Modes().Mouse != AnyEvents {
		t.Fatal("floor replay lost observed modes")
	}
	var clipped Parser
	clipped.Feed(stream[floor:])
	if clipped.Modes().AlternateKnown || clipped.Modes().MouseKnown {
		t.Fatal("legacy clipped replay guessed missing setup")
	}
}

func TestSafeBoundaryAfterDiscardedLeadingFragment(t *testing.T) {
	p := parsed(enabled + "\x1b]52;opaque;")
	tail := []byte("payload\xc3\x9b?47l\x1b\\Visible")
	boundary := -1
	for i, c := range tail {
		p.Feed([]byte{c})
		if p.SafeBoundary() {
			boundary = i + 1
			break
		}
	}
	if boundary != len(tail)-len("Visible") || !p.Modes().Alternate {
		t.Fatalf("unsafe payload boundary %d", boundary)
	}
	// A partial mode command may complete inside the discarded fragment;
	// its effect must be included in the checkpoint BEFORE choosing a prefix.
	p = parsed(enabled + "\x1b[?47")
	if p.SafeBoundary() {
		t.Fatal("partial CSI")
	}
	p.Feed([]byte("l"))
	if !p.SafeBoundary() || p.Modes().Alternate {
		t.Fatal("discarded final did not update mode")
	}
}

func TestAPCSOSPMC1Counterexample(t *testing.T) {
	// Reviewer counterexample: the old parser treated all strings as opaque
	// high-byte payload. In pinned Ghostty the C1 CSI aborts SOS/PM/APC and
	// the following DECRST really selects the primary screen.
	for _, open := range []string{"\x1b_", "\x1b^", "\x1bX"} {
		for _, prefix := range []string{"", "\xc3"} { // UTF8 lead is ignored here
			input := enabled + open + prefix + "\x9b?47l\x1b\\"
			for split := 0; split <= len(input); split++ {
				p := parsed(input[:split])
				p.Feed([]byte(input[split:]))
				m := p.Modes()
				if !p.SafeBoundary() || !m.AlternateKnown || m.Alternate {
					t.Fatalf("C1 CSI hidden in %q split%d: %+v", open, split, m)
				}
			}
		}
	}
}

func TestAPCSOSPMByteTransitions(t *testing.T) {
	// Source-derived expectedApcExit from pinned stream.zig; exhaust every
	// byte so OSC/DCS's different high-byte treatment cannot regress this.
	for _, open := range []string{"\x1b_", "\x1b^", "\x1bX"} {
		for n := 0; n < 256; n++ {
			c := byte(n)
			p := parsed(enabled + open)
			p.Feed([]byte{c})
			want := apcString
			switch {
			case c == 0x18 || c == 0x1a || c == 0x9c,
				c >= 0x80 && c <= 0x8f, c >= 0x91 && c <= 0x97, c == 0x99, c == 0x9a:
				want = ground
			case c == 0x1b:
				want = escape
			case c == 0x90:
				want = dcsHeader
			case c == 0x9b:
				want = csi
			case c == 0x9d:
				want = osc
			}
			if p.state != want || p.SafeBoundary() != (want == ground) || !p.Modes().Complete() {
				t.Fatalf("%q byte%02x: state%d want%d %+v", open, c, p.state, want, p.Modes())
			}
		}
	}
	// C1 DCS/OSC introduce their own high-byte-safe strings; no accidental
	// private mode recognition inside the new string, until true ESC abort.
	for _, intro := range []string{"\x90q", "\x9d"} {
		p := parsed(enabled + "\x1b_" + intro + "\x9b?47l")
		if p.SafeBoundary() || !p.Modes().Alternate {
			t.Fatal("new string payload interpreted")
		}
		p.Feed([]byte("\x1b[?47l"))
		if !p.SafeBoundary() || p.Modes().Alternate {
			t.Fatal("ESC abort not recognized")
		}
	}
}
