package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/session/holder/replaymode"
)

const syntheticModes = "\x1b[?1049;1003;1006;2004h"

func replayFixture(t *testing.T, p replaymode.Parser, off int64, chunks ...[]byte) *bytes.Buffer {
	t.Helper()
	var b bytes.Buffer
	state, _ := p.MarshalBinary()
	meta, _ := orchapi.ReplayBytes(off, state)
	if err := orchapi.WriteReplayFrame(&b, orchapi.ReplayMetadata, meta); err != nil {
		t.Fatal(err)
	}
	for _, data := range chunks {
		if len(data) == 0 {
			continue
		}
		body, _ := orchapi.ReplayBytes(off, data)
		if err := orchapi.WriteReplayFrame(&b, orchapi.ReplayOutput, body); err != nil {
			t.Fatal(err)
		}
		off += int64(len(data))
	}
	orchapi.WriteReplayFrame(&b, orchapi.ReplayEnd, nil)
	return &b
}

type replayInputSpy struct {
	*bytes.Reader
	writes bytes.Buffer
}

func (s *replayInputSpy) Write(b []byte) (int, error) { return s.writes.Write(b) }
func TestEmbedReplayPrefixOnlyFrontendAndRawCursor(t *testing.T) {
	var p replaymode.Parser
	p.Feed([]byte(syntheticModes))
	raw := []byte("\x1b[HInvented redraw")
	wire := replayFixture(t, p, 100, raw)
	source := &replayInputSpy{Reader: bytes.NewReader(wire.Bytes())}
	var out bytes.Buffer
	warnings, outputs := 0, 0
	result, err := renderEmbedReplay(&out, source, func() { outputs++ }, func(message string) {
		warnings++
		if !strings.Contains(message, "Recorded conversation") {
			t.Error("missing reader guidance")
		}
	})
	if err != nil || !result.Ended || result.Cursor != 100+int64(len(raw)) || result.Skipped != 0 {
		t.Fatalf("cursor/result %+v %v", result, err)
	}
	if out.String() != "\x1b[?47h\x1b[?2004h"+string(raw) {
		t.Fatalf("unapproved prefix/mouse mode %q", out.String())
	}
	if source.writes.Len() != 0 || warnings != 1 || outputs != 1 {
		t.Fatal("input write or missing degradation")
	}
}
func TestEmbedReplayPartialLeadingControlAndUTF8(t *testing.T) {
	cases := []struct {
		before, tail, prefix string
		skip                 int
	}{
		{syntheticModes + "\x1b]52;opaque;", "payload\xc3\x9b?47l\x1b\\Visible", "\x1b[?47h\x1b[?2004h", len("payload\xc3\x9b?47l\x1b\\")},
		{syntheticModes + "\x1bPqopaque;", "payload\a\x1b\\Visible", "\x1b[?47h\x1b[?2004h", len("payload\a\x1b\\")},
		{syntheticModes + "\x1b[?47", "lVisible", "\x1b[?47l\x1b[?2004h", 1},
		{syntheticModes + "\xe2", "\x9b\x9cVisible", "\x1b[?47h\x1b[?2004h", 2},
	}
	for _, tc := range cases {
		for split := 0; split <= len(tc.tail); split++ {
			var p replaymode.Parser
			p.Feed([]byte(tc.before))
			wire := replayFixture(t, p, 200, []byte(tc.tail[:split]), []byte(tc.tail[split:]))
			var out bytes.Buffer
			warnings := 0
			result, err := renderEmbedReplay(&out, wire, nil, func(string) { warnings++ })
			if err != nil || result.Cursor != 200+int64(len(tc.tail)) || result.Skipped != int64(tc.skip) || out.String() != tc.prefix+"Visible" || warnings != 1 {
				t.Fatalf("partial split%d before%q: %q %+v %v", split, tc.before, out.String(), result, err)
			}
		}
	}
}
func TestEmbedReplayUnknownDoesNotGuessOrExposeString(t *testing.T) {
	wire := replayFixture(t, replaymode.Lost(), 1000, []byte("opaque unknown suffix ?1049h"))
	var out bytes.Buffer
	warnings := 0
	result, err := renderEmbedReplay(&out, wire, nil, func(string) { warnings++ })
	if err != nil || out.Len() != 0 || result.Skipped == 0 || warnings != 1 || !result.Ended {
		t.Fatal("unknown replay guessed a mode/boundary")
	}
}
func TestEmbedReplayRejectsMidstreamResetGapAndMalformed(t *testing.T) {
	var p replaymode.Parser
	p.Feed([]byte(syntheticModes))
	state, _ := p.MarshalBinary()
	meta, _ := orchapi.ReplayBytes(100, state)
	for _, kind := range []byte{orchapi.ReplayMetadata, orchapi.ReplayGap, orchapi.ReplayOutput} {
		var wire bytes.Buffer
		orchapi.WriteReplayFrame(&wire, orchapi.ReplayMetadata, meta)
		body := meta
		if kind != orchapi.ReplayMetadata {
			body, _ = orchapi.ReplayBytes(999, nil)
			if kind == orchapi.ReplayOutput {
				body, _ = orchapi.ReplayBytes(999, []byte("MUST-NOT-RENDER"))
			}
		}
		orchapi.WriteReplayFrame(&wire, kind, body)
		var out bytes.Buffer
		_, err := renderEmbedReplay(&out, &wire, nil, nil)
		if err == nil || out.String() != "\x1b[?47h\x1b[?2004h" {
			t.Fatal("midstream metadata/gap rendered bytes or reset")
		}
		if kind == orchapi.ReplayGap && !errors.Is(err, errReplayReconnect) {
			t.Fatal("gap not reconnectable")
		}
	}
	var bad bytes.Buffer
	state[0] = 255
	meta, _ = orchapi.ReplayBytes(0, state)
	orchapi.WriteReplayFrame(&bad, orchapi.ReplayMetadata, meta)
	var out bytes.Buffer
	if _, err := renderEmbedReplay(&out, &bad, nil, nil); err == nil || out.Len() != 0 {
		t.Fatal("unknown checkpoint schema rendered")
	}
	if _, err := renderEmbedReplay(io.Discard, bytes.NewReader([]byte("unframed raw bytes")), nil, nil); err == nil {
		t.Fatal("raw bytes accepted as framed fallback")
	}
}

func TestEmbedReplayLeavesOrdinaryInputAndPastePathUnchanged(t *testing.T) {
	var p replaymode.Parser
	p.Feed([]byte(syntheticModes))
	wire := replayFixture(t, p, 0, []byte("invented"))
	source := &replayInputSpy{Reader: bytes.NewReader(wire.Bytes())}
	if _, err := renderEmbedReplay(io.Discard, source, nil, nil); err != nil {
		t.Fatal(err)
	}
	if source.writes.Len() != 0 {
		t.Fatal("replay injected managed input")
	}
	keys := []byte("ordinary keys\x1b[200~line one\nline two\x1b[201~")
	chunks := make(chan []byte, 1)
	chunks <- keys
	close(chunks)
	window := newReplyWindow(nil)
	window.closed = true
	runEmbedInput(chunks, source, false, window)
	if !bytes.Equal(source.writes.Bytes(), keys) {
		t.Fatal("ordinary keys/paste changed")
	}
}

func TestEmbedReplayOptInRequiresFreshLocalSurface(t *testing.T) {
	for _, args := range [][]string{{"--replay-modes", "fixture"}, {"--embed", "--replay-modes", "--on", "synthetic-host", "fixture"}} {
		cmd := attachCmd()
		cmd.SetArgs(args)
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		err := cmd.Execute()
		var exit exitWith
		if !errors.As(err, &exit) || exit.code != 2 {
			t.Fatalf("unsafe opt-in accepted %v %v", args, err)
		}
	}
}

func TestEmbedReplayMetadataAtMaxCursorConsumesNoRawBytes(t *testing.T) {
	var p replaymode.Parser
	p.Feed([]byte(syntheticModes))
	state, _ := p.MarshalBinary()
	metadata := make([]byte, orchapi.ReplayMetadataSize)
	binary.BigEndian.PutUint64(metadata, uint64(1<<63-1))
	copy(metadata[8:], state)
	var wire bytes.Buffer
	orchapi.WriteReplayFrame(&wire, orchapi.ReplayMetadata, metadata)
	orchapi.WriteReplayFrame(&wire, orchapi.ReplayEnd, nil)
	var out bytes.Buffer
	result, err := renderEmbedReplay(&out, &wire, nil, nil)
	if err != nil || !result.Ended || result.Cursor != 1<<63-1 || result.Skipped != 0 || out.String() != "\x1b[?47h\x1b[?2004h" {
		t.Fatalf("metadata consumed raw cursor: %+v %v %q", result, err, out.String())
	}
}
func TestEmbedReplayOutputAtMaxCursorCannotOverflow(t *testing.T) {
	var p replaymode.Parser
	p.Feed([]byte(syntheticModes))
	state, _ := p.MarshalBinary()
	metadata := make([]byte, orchapi.ReplayMetadataSize)
	binary.BigEndian.PutUint64(metadata, uint64(1<<63-1))
	copy(metadata[8:], state)
	output := make([]byte, 9)
	binary.BigEndian.PutUint64(output, uint64(1<<63-1))
	output[8] = 'X'
	var wire bytes.Buffer
	orchapi.WriteReplayFrame(&wire, orchapi.ReplayMetadata, metadata)
	orchapi.WriteReplayFrame(&wire, orchapi.ReplayOutput, output)
	var out bytes.Buffer
	result, err := renderEmbedReplay(&out, &wire, nil, nil)
	if err == nil || result.Cursor != 1<<63-1 || out.String() != "\x1b[?47h\x1b[?2004h" {
		t.Fatalf("raw output overflow accepted %+v %v %q", result, err, out.String())
	}
}
