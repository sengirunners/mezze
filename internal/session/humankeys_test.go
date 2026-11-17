package session

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
)

// feedHuman runs chunks through one classifier and reports, per chunk,
// whether it counted as a person typing.
func feedHuman(chunks ...string) []bool {
	return feedHumanMode(false, chunks...)
}

// feedHumanMode is feedHuman with UTF-8 mouse mode (1005) on or off.
func feedHumanMode(utf8 bool, chunks ...string) []bool {
	h := humanKeys{utf8Mouse: func() bool { return utf8 }}
	out := make([]bool, len(chunks))
	for i, c := range chunks {
		out[i] = h.Feed([]byte(c))
	}
	return out
}

func anyTrue(bs []bool) bool {
	for _, b := range bs {
		if b {
			return true
		}
	}
	return false
}

var reports = map[string]string{
	"focus in":      "\x1b[I",
	"focus out":     "\x1b[O",
	"SGR press":     "\x1b[<0;12;5M",
	"SGR release":   "\x1b[<0;12;5m",
	"SGR wheel":     "\x1b[<65;120;40M",
	"X10":           "\x1b[M !!",
	"X10 high byte": "\x1b[M#\xff\xfe",
	"urxvt":         "\x1b[32;12;5M",
	"many":          "\x1b[O\x1b[I\x1b[<35;1;1M\x1b[<35;2;1M\x1b[M !!\x1b[O",
	// Answers to queries in a replayed scrollback.
	"DA1":                "\x1b[?62;22;52c",
	"DA2":                "\x1b[>1;10;0c",
	"DA3":                "\x1b[=0c",
	"CPR":                "\x1b[12;40R",
	"CPR row 1 col 1":    "\x1b[1;1R",
	"CPR row 1 col 40":   "\x1b[1;40R",
	"DECXCPR":            "\x1b[?12;40;1R",
	"DSR ok":             "\x1b[0n",
	"DSR fault":          "\x1b[3n",
	"DECRPM private":     "\x1b[?2004;1$y",
	"DECRPM ansi":        "\x1b[4;2$y",
	"kitty flags":        "\x1b[?15u",
	"kitty flags none":   "\x1b[?u",
	"window size":        "\x1b[8;40;120t",
	"OSC 4 BEL":          "\x1b]4;1;rgb:cdcd/0000/0000\x07",
	"OSC 10 ST":          "\x1b]10;rgb:ffff/ffff/ffff\x1b\\",
	"OSC 11":             "\x1b]11;rgb:1e1e/1e1e/2e2e\x1b\\",
	"OSC 12":             "\x1b]12;rgb:ffff/0000/0000\x07",
	"XTVERSION":          "\x1bP>|ghostty 1.2.0\x1b\\",
	"DECRQSS":            "\x1bP1$r0;1m\x1b\\",
	"XTGETTCAP":          "\x1bP1+r544e=787465726d\x1b\\",
	"a replay's answers": "\x1b[?62;22c\x1b[12;1R\x1b]11;rgb:0/0/0\x07\x1bP>|x\x1b\\\x1b[?2026;2$y\x1b[?1u",
}

var personKeys = map[string]string{
	"text":       "y",
	"enter":      "\r",
	"arrow":      "\x1b[A",
	"ctrl-arrow": "\x1b[1;5C",
	"F5":         "\x1b[15~",
	"SS3":        "\x1bOP",
	"alt-x":      "\x1bx",
	"escape":     "\x1b",
	"two Esc":    "\x1b\x1b",
	"paste":      "\x1b[200~hi\x1b[201~",
	"ctrl-]":     "\x1d",
	"utf8":       "é",
	// Keys that look like answers, but are not.
	"kitty key":        "\x1b[97;5u",
	"kitty key bare":   "\x1b[97u",
	"shift-F3":         "\x1b[1;2R",
	"ctrl-alt-F3":      "\x1b[1;7R",
	"shift-F1":         "\x1b[1;2P",
	"shift-F5":         "\x1b[15;2~",
	"shift-up":         "\x1b[1;2A",
	"shift-tab":        "\x1b[Z",
	"alt-]":            "\x1b]",
	"alt-P":            "\x1bP",
	"OSC 52 (a paste)": "\x1b]52;c;aGk=\x07",
	"OSC 2 title":      "\x1b]2;x\x07",
}

func TestHumanKeysReportsAreNotAPerson(t *testing.T) {
	for name, r := range reports {
		if feedHuman(r)[0] {
			t.Errorf("%s counted as a person", name)
		}
		// Split at every point (but right after ESC, ESC [, ESC ] or ESC P,
		// which may be the Escape key or an alt-key and count at once): never
		// a person.
		for cut := 1; cut < len(r); cut++ {
			if strings.HasSuffix(r[:cut], "\x1b") || strings.HasSuffix(r[:cut], "\x1b[") ||
				strings.HasSuffix(r[:cut], "\x1b]") || strings.HasSuffix(r[:cut], "\x1bP") {
				continue
			}
			if got := feedHuman(r[:cut], r[cut:]); anyTrue(got) {
				t.Errorf("%s cut at %d: %v", name, cut, got)
			}
		}
	}
}

func TestHumanKeysPersonKeysCount(t *testing.T) {
	for name, k := range personKeys {
		if !feedHuman(k)[0] {
			t.Errorf("%s not counted as a person", name)
		}
		var chunks []string
		for i := 0; i < len(k); i++ {
			chunks = append(chunks, k[i:i+1])
		}
		if !anyTrue(feedHuman(chunks...)) {
			t.Errorf("%s byte by byte: not counted", name)
		}
	}
}

func TestHumanKeysMixed(t *testing.T) {
	cases := []struct {
		chunks []string
		want   []bool
	}{
		{[]string{"\x1b[O", "y\r", "\x1b[I"}, []bool{false, true, false}},
		{[]string{"\x1b[I" + "y"}, []bool{true}},
		{[]string{"y" + "\x1b[O"}, []bool{true}},
		{[]string{"\x1b[<0;12;5M\x1b[A"}, []bool{true}},
		// A report split across chunks, then a key in the second chunk.
		{[]string{"\x1b[<0;1", "2;5Mq"}, []bool{false, true}},
		// A key split across chunks: decided when it completes.
		{[]string{"\x1b[1;5", "C"}, []bool{false, true}},
		// ESC [ alone at a chunk end counts (it may be alt-[).
		{[]string{"\x1b[", "I"}, []bool{true, false}},
		// An X10 report cut short by a control byte: a person's.
		{[]string{"\x1b[M !\r"}, []bool{true}},
		// Focus then mouse then focus, each its own chunk.
		{[]string{"\x1b[I", "\x1b[M", " !!", "\x1b[O"}, []bool{false, false, false, false}},
		// Answers around keys.
		{[]string{"\x1b[?62c1\r\x1b[12;40R"}, []bool{true}},
		{[]string{"\x1b[?62c", "\x1b]11;rgb:0/0/0", "\x07", "y"}, []bool{false, false, false, true}},
		{[]string{"\x1bP>|ghostty", " 1.2\x1b", "\\", "\x1b[0n"}, []bool{false, false, false, false}},
		// An OSC cut short by a key's ESC: a person's.
		{[]string{"\x1b]11;rgb:\x1b[A"}, []bool{true}},
		// A DCS cut short by a control byte (a person typed alt-P, then Enter).
		{[]string{"\x1bPx\r"}, []bool{true}},
	}
	for _, c := range cases {
		got := feedHuman(c.chunks...)
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q: %v, want %v", c.chunks, got, c.want)
				break
			}
		}
	}
}

// TestAttachFocusAndMouseKeepNeedsHuman: through a real attach, focus and
// mouse reports reach the program but leave a hook-set needs_human alone;
// a key clears it (with the program printing again).
func TestAttachFocusAndMouseKeepNeedsHuman(t *testing.T) { // and query answers
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.spawn("claude", `stty raw -echo; (while :; do printf .; sleep 0.1; done) & exec cat > "$ZM_SCRATCH/in"`, nil)
	conn, err := orchclient.Attach(ctx, h.c, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	go func() { _, _ = io.Copy(io.Discard, conn) }()
	if _, err := orchclient.SetState(ctx, h.c, ss.ID, orchapi.StateRequest{State: orchapi.StateNeedsHuman, Note: "permission"}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"\x1b[O", "\x1b[I", "\x1b[<0;12;5M", "\x1b[<0;12;5m", "\x1b[<0;1", "2;5M", "\x1b[M !!",
		"\x1b[?62;22c\x1b[1;1R\x1b[0n", "\x1b]11;rgb:0/0/0\x07", "\x1bP>|ghostty\x1b\\", "\x1b[?2004;1$y\x1b[?1u"} {
		if _, err := conn.Write([]byte(r)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(400 * time.Millisecond) // output keeps coming
	if got, _ := orchclient.Session(ctx, h.c, ss.ID); got.State != orchapi.StateNeedsHuman {
		t.Fatalf("focus/mouse reports cleared needs_human: %+v", got)
	}
	// They reached the program.
	in := filepath.Join(ss.Scratch, "in")
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, _ := os.ReadFile(in)
		if strings.Contains(string(b), "\x1b[<0;12;5M") && strings.Contains(string(b), "\x1b[M !!") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reports not forwarded: %q", b)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if _, err := conn.Write([]byte("\x1b[I1\r")); err != nil {
		t.Fatal(err)
	}
	h.wait(ss.ID, "running after a key", func(s orchapi.Session) bool { return s.State == orchapi.StateRunning })
}

// UTF-8 mouse coordinates (DECSET 1005) only when the program turned it on:
// in plain X10 a coordinate byte 0xc2-0xdf is one unit, so the key after
// the report still counts.
func TestHumanKeysX10VersusUTF8Mouse(t *testing.T) {
	x10 := "\x1b[M \xc3\xa9" // three single-byte units
	if got := feedHumanMode(false, x10); got[0] {
		t.Fatalf("X10 report counted")
	}
	if got := feedHumanMode(false, x10+"a"); !got[0] {
		t.Fatalf("X10: the key after the report was swallowed")
	}
	utf := "\x1b[M \xc2\xa0!" // space, U+00A0 (two bytes), '!'
	if got := feedHumanMode(true, utf); got[0] {
		t.Fatalf("1005 report counted")
	}
	if got := feedHumanMode(true, utf+"a"); !got[0] {
		t.Fatalf("1005: the key after the report was swallowed")
	}
	if got := feedHumanMode(true, "\x1b[M@\xc4\x80\xc3\xbf"); got[0] {
		t.Fatalf("1005 report with two wide units counted")
	}
}

func TestModeWatch(t *testing.T) {
	var on bool
	m := modeWatch{on: func(v bool) { on = v }}
	steps := []struct {
		chunk string
		want  bool
	}{
		{"hello \x1b[?1000h", false},
		{"\x1b[?10", false},
		{"05h", true}, // split across reads
		{strings.Repeat("x", 100), true},
		{"\x1b[?1000;1005l more", false},
		{"\x1b[?1006;1005;1004h", true},
		{"\x1b[?2004h\x1b[?1005", true},
		{"l", false},
	}
	for i, s := range steps {
		m.feed([]byte(s.chunk))
		if on != s.want {
			t.Fatalf("step %d (%q): on=%v, want %v", i, s.chunk, on, s.want)
		}
	}
}
