package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every kind of answer a terminal gives to a query in a replay.
var replies = map[string]string{
	"CPR":             "\x1b[12;40R",
	"DECXCPR":         "\x1b[?12;40;1R",
	"DA1":             "\x1b[?62;22;52c",
	"DA2":             "\x1b[>1;10;0c",
	"DA3-ish":         "\x1b[=0c",
	"DSR ok":          "\x1b[0n",
	"DSR fault":       "\x1b[3n",
	"OSC 4 BEL":       "\x1b]4;1;rgb:cdcd/0000/0000\x07",
	"OSC 10 ST":       "\x1b]10;rgb:ffff/ffff/ffff\x1b\\",
	"OSC 11":          "\x1b]11;rgb:1e1e/1e1e/2e2e\x1b\\",
	"OSC 12":          "\x1b]12;rgb:ffff/0000/0000\x07",
	"XTVERSION":       "\x1bP>|ghostty 1.2.0\x1b\\",
	"DECRQSS":         "\x1bP1$r0;1m\x1b\\",
	"XTGETTCAP":       "\x1bP1+r544e=787465726d\x1b\\",
	"DECRQM private":  "\x1b[?2004;1$y",
	"DECRQM ansi":     "\x1b[4;2$y",
	"kitty keyboard":  "\x1b[?15u",
	"kitty kbd empty": "\x1b[?u",
	"XTWINOPS size":   "\x1b[8;40;120t",
}

// Input that must always pass: a person's keys.
var keys = map[string]string{
	"text":                            "hello world\r",
	"ctrl-]":                          "\x1d",
	"arrow":                           "\x1b[A",
	"ctrl-arrow":                      "\x1b[1;5C",
	"F5":                              "\x1b[15~",
	"SS3 F1":                          "\x1bOP",
	"alt-x":                           "\x1bx",
	"bracketed paste":                 "\x1b[200~pasted\x1b[201~",
	"focus in":                        "\x1b[I",
	"sgr mouse":                       "\x1b[<0;10;5M",
	"kitty key event":                 "\x1b[97;5u",
	"OSC 52 (not an answer zm drops)": "\x1b]52;c;aGk=\x07",
	"utf8":                            "héllo ✳",
	"shift-F3":                        "\x1b[1;2R",
	"ctrl-alt-F3":                     "\x1b[1;7R",
	"paste of answers":                "\x1b[200~\x1b[12;40R\x1b[?62c\x1bP>|x\x1b\\\x1b]11;rgb:0/0/0\x07\x1b[201~",
}

// feedAll runs input through a filter in the given chunks and returns what
// passed, flushing what is held at the end.
func feedAll(chunks ...string) string {
	var f replyFilter
	var out []byte
	for _, c := range chunks {
		out = append(out, f.Feed([]byte(c))...)
	}
	return string(append(out, f.Flush()...))
}

// splits feeds s whole, byte by byte, and cut in two at every point.
func splits(t *testing.T, name, s, want string) {
	t.Helper()
	if got := feedAll(s); got != want {
		t.Errorf("%s whole: %q, want %q", name, got, want)
	}
	var bytesOne []string
	for i := 0; i < len(s); i++ {
		bytesOne = append(bytesOne, s[i:i+1])
	}
	if got := feedAll(bytesOne...); got != want {
		t.Errorf("%s byte by byte: %q, want %q", name, got, want)
	}
	for cut := 0; cut <= len(s); cut++ {
		if got := feedAll(s[:cut], s[cut:]); got != want {
			t.Errorf("%s cut at %d: %q, want %q", name, cut, got, want)
		}
	}
}

func TestReplyFilterDropsEveryAnswer(t *testing.T) {
	for name, r := range replies {
		splits(t, name, r, "")
	}
}

func TestReplyFilterPassesKeys(t *testing.T) {
	for name, k := range keys {
		splits(t, name, k, k)
	}
}

func TestReplyFilterMixed(t *testing.T) {
	var in, want strings.Builder
	i := 0
	for _, r := range replies {
		in.WriteString(r)
		for _, k := range []string{"a", "\x1b[A", "\x1bx", "\x1b[1;5C"} {
			if i%4 == 0 {
				in.WriteString(k)
				want.WriteString(k)
			}
			i++
		}
	}
	in.WriteString("typed\r")
	want.WriteString("typed\r")
	splits(t, "mixed", in.String(), want.String())
	// Two Escapes, then an answer: the Escapes are a person's.
	splits(t, "escapes", "\x1b\x1b\x1b[12;1R", "\x1b\x1b")
	// An OSC cut short by another sequence passes as it is.
	splits(t, "broken osc", "\x1b]11;rgb:\x1b[A", "\x1b]11;rgb:\x1b[A")
}

// A lone Escape is held only until Flush (the caller's short timer).
func TestReplyFilterHoldsUnfinished(t *testing.T) {
	var f replyFilter
	if out := f.Feed([]byte("ab\x1b")); string(out) != "ab" || !f.Pending() {
		t.Fatalf("out %q pending %v", out, f.Pending())
	}
	if out := f.Flush(); string(out) != "\x1b" || f.Pending() {
		t.Fatalf("flush %q", out)
	}
	// Overlong sequences are not answers: they pass.
	long := "\x1bP" + strings.Repeat("x", maxReply+10)
	if got := feedAll(long); got != long {
		t.Fatalf("overlong: %d bytes passed of %d", len(got), len(long))
	}
}

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func TestReplyWindow(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	w := newReplyWindow(clk.now)
	if !w.open() {
		t.Fatal("closed at attach")
	}
	// Output keeps it open while it flows.
	for i := 0; i < 8; i++ {
		clk.add(200 * time.Millisecond)
		w.output()
		if !w.open() {
			t.Fatalf("closed while output flows (%d)", i)
		}
	}
	// Quiet for 250 ms closes it, for good.
	clk.add(250 * time.Millisecond)
	if w.open() {
		t.Fatal("open after 250 ms quiet")
	}
	w.output()
	if w.open() {
		t.Fatal("reopened by later output")
	}
	// Endless output: closed at the 3 s cap.
	w = newReplyWindow(clk.now)
	for i := 0; i < 40; i++ {
		clk.add(100 * time.Millisecond)
		w.output()
		if i < 29 && !w.open() {
			t.Fatalf("closed before the cap (%d)", i)
		}
	}
	if w.open() {
		t.Fatal("open past the cap")
	}
	// No output yet: quiet does not count before the first byte (a loaded
	// machine), only the 3 s cap does.
	w = newReplyWindow(clk.now)
	clk.add(2 * time.Second)
	if !w.open() {
		t.Fatal("closed before any output")
	}
	w.output()
	clk.add(200 * time.Millisecond)
	if !w.open() {
		t.Fatal("closed right after the first output")
	}
	clk.add(100 * time.Millisecond)
	if w.open() {
		t.Fatal("open after 250 ms quiet")
	}
	w = newReplyWindow(clk.now)
	clk.add(3 * time.Second)
	if w.open() {
		t.Fatal("open at the cap with no output")
	}
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuf) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// The input loop: answers dropped in the window, keys passed (a held
// Escape after a short wait), everything passed after the window.
func TestEmbedInputLoop(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	w := newReplyWindow(clk.now)
	chunks := make(chan []byte)
	var out lockedBuf
	done := make(chan struct{})
	go func() { runEmbedInput(chunks, &out, false, w); close(done) }()
	chunks <- []byte("\x1b[?62;22c\x1b[12;")
	chunks <- []byte("40Rk\x1d")
	chunks <- []byte("\x1b") // a lone Escape: passes after replyFlushAfter
	deadline := time.Now().Add(2 * time.Second)
	for out.String() != "k\x1d\x1b" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := out.String(); got != "k\x1d\x1b" {
		t.Fatalf("in the window: %q", got)
	}
	clk.add(4 * time.Second) // the window closes (no output: at the cap)
	chunks <- []byte("\x1b[12;40R")
	close(chunks)
	<-done
	if got := out.String(); got != "k\x1d\x1b\x1b[12;40R" {
		t.Fatalf("after the window: %q", got)
	}
	// Read-only: nothing is written.
	var ro lockedBuf
	c2 := make(chan []byte, 1)
	c2 <- []byte("typed")
	close(c2)
	runEmbedInput(c2, &ro, true, newReplyWindow(clk.now))
	if ro.String() != "" {
		t.Fatalf("read-only wrote %q", ro.String())
	}
}

// The window closing while an answer is half read: the rest of it is still
// read through the filter (dropped), then everything passes.
func TestEmbedInputWindowClosesMidSequence(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1000, 0)}
	w := newReplyWindow(clk.now)
	w.output()
	// The send below returns once the loop has the chunk, maybe before it
	// asks the window; moving the clock then would close the window first.
	// So wait until the loop has read the clock for that chunk.
	judged := make(chan struct{}, 1)
	w.now = func() time.Time {
		t := clk.now()
		select {
		case judged <- struct{}{}:
		default:
		}
		return t
	}
	chunks := make(chan []byte)
	var out lockedBuf
	done := make(chan struct{})
	go func() { runEmbedInput(chunks, &out, false, w); close(done) }()
	chunks <- []byte("a\x1b[12;")
	<-judged
	clk.add(time.Second) // closed now
	chunks <- []byte("40Rk\x1b[?62c")
	chunks <- []byte("\x1b[3;4R")
	close(chunks)
	<-done
	if got := out.String(); got != "ak\x1b[?62c\x1b[3;4R" {
		t.Fatalf("got %q", got)
	}
}

// A bracketed paste passes verbatim, answers inside included; answers
// around it are still dropped.
func TestReplyFilterPaste(t *testing.T) {
	paste := "\x1b[200~ls \x1b[0n\x1b[?62c\x1b[201~"
	splits(t, "paste between answers", "\x1b[12;40R"+paste+"\x1b[?62c\r", paste+"\r")
	// A paste whose end marker looks like it starts but is not it.
	p2 := "\x1b[200~a\x1b[20x\x1b[201b\x1b[201~"
	splits(t, "near end markers", p2+"\x1b[0n", p2)
}
