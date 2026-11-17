package holder

import (
	"bytes"
	"github.com/sengirunners/mezze/internal/session/holder/replaymode"
	"math/rand"
	"testing"
)

const modeSetup = "\x1b[?1049;1003;1006;2004h"

func TestReplayRingEveryFloorMatchesFullPrefix(t *testing.T) {
	input := []byte(modeSetup + "\x1b]52;c;opaque\xc3\x9b?47l\x1b\\hello\xe2\x9b\x9c\x1bP1;2qopaque\x1b\\\x1b_\x9b?47l\x1b\\\x1b[?1000l\x1b[?2004l")
	for _, size := range []int{1, 2, 7, 16, 64, 128} {
		for _, chunk := range []int{1, 3, 17, 200} {
			r := NewRing(size)
			var full []byte
			for i := 0; i < len(input); i += chunk {
				end := min(i+chunk, len(input))
				part := input[i:end]
				r.Write(part)
				full = append(full, part...)
				start, finish := r.Bounds()
				for off := start; off <= finish; off++ {
					got := r.NextReplay(off, 11)
					var want replaymode.Parser
					want.Feed(full[:off])
					if got.State != want || got.From != off || got.To != min(off+11, finish) || !bytes.Equal(got.Data, full[off:got.To]) {
						t.Fatalf("size%d chunk%d off%d wrong checkpoint/data", size, chunk, off)
					}
				}
			}
		}
	}
}
func TestReplayRingRandomChunksAndLargeWrites(t *testing.T) {
	rng := rand.New(rand.NewSource(123))
	input := append([]byte(modeSetup), bytes.Repeat([]byte("\x1b[Hsynthetic\r\n\x1b[?47l\x1b[?1049h"), 200)...)
	r := NewRing(37)
	var seen []byte
	for len(input) > 0 {
		n := min(1+rng.Intn(200), len(input))
		r.Write(input[:n])
		seen = append(seen, input[:n]...)
		input = input[n:]
		got := r.NextReplay(0, 1000)
		var want replaymode.Parser
		want.Feed(seen[:got.From])
		if got.State != want || !bytes.Equal(got.Data, seen[got.From:]) {
			t.Fatal("oversize prefix not checkpointed")
		}
	}
}
func TestReplayMirrorMetadataGapsDuplicatesAndNoRawMutation(t *testing.T) {
	source := NewRing(17)
	source.Write([]byte(modeSetup + "\x1b]52;opaque-prefix"))
	first := source.NextReplay(0, 100)
	mirror := NewRing(17)
	mirror.WriteAt(first.From, nil)
	beforeStart, beforeEnd := mirror.Bounds()
	if !mirror.CheckpointNextWrite(ReplayCheckpoint{From: first.From, State: first.State}) {
		t.Fatal("checkpoint not staged")
	}
	if s, e := mirror.Bounds(); s != beforeStart || e != beforeEnd {
		t.Fatal("metadata changed raw bounds")
	}
	mirror.WriteAt(first.From, first.Data)
	a, b := source.NextReplay(0, 100), mirror.NextReplay(0, 100)
	if a.State != b.State || !bytes.Equal(a.Data, b.Data) {
		t.Fatal("adoption lost partial parser")
	}
	// Duplicate/overlap cannot replace observed prefix state or duplicate bytes.
	if mirror.CheckpointNextWrite(ReplayCheckpoint{From: first.From, State: replaymode.Lost()}) {
		t.Fatal("accepted historical metadata")
	}
	mirror.WriteAt(first.From, first.Data)
	mirror.WriteAt(first.From+1, first.Data[1:])
	if got := mirror.NextReplay(0, 100); got.State != b.State || !bytes.Equal(got.Data, b.Data) {
		t.Fatal("duplicate changed raw/checkpoint")
	}
	// Uninterrupted future output advances the seeded partial string state,
	// including an incoming prefix larger than capacity, without new metadata.
	tail := []byte("tail\x1b\\" + modeSetup + "\x1b[?47lnew")
	source.Write(tail)
	mirror.WriteAt(first.To, tail)
	a, b = source.NextReplay(0, 100), mirror.NextReplay(0, 100)
	if a.State != b.State || !bytes.Equal(a.Data, b.Data) {
		t.Fatal("continuous mirror evictions lost checkpoint")
	}
	// A gap without metadata has unknown grammar/modes. No default normal bank.
	mirror.WriteAt(first.To+100, []byte("lost suffix"))
	got := mirror.NextReplay(0, 100)
	if got.State.SafeBoundary() || got.State.Modes().AlternateKnown {
		t.Fatal("gap guessed state")
	}
	// Metadata at the next discontinuity recovers exact state atomically.
	var recovered replaymode.Parser
	recovered.Feed([]byte(modeSetup))
	off := got.To + 100
	mirror.CheckpointNextWrite(ReplayCheckpoint{From: off, State: recovered})
	mirror.WriteAt(off, []byte("new"))
	got = mirror.NextReplay(0, 100)
	if got.From != off || got.State != recovered || string(got.Data) != "new" {
		t.Fatal("gap metadata not paired")
	}
}
func TestReplayChunkSnapshotDoesNotChangeWhenWriterEvicts(t *testing.T) {
	r := NewRing(32)
	r.Write([]byte(modeSetup + "old"))
	snapshot := r.NextReplay(0, 32)
	state, data := snapshot.State, append([]byte(nil), snapshot.Data...)
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			r.Write([]byte("\x1b[?47lnew"))
		}
		close(done)
	}()
	<-done
	if snapshot.State != state || !bytes.Equal(snapshot.Data, data) {
		t.Fatal("snapshot aliases ring")
	}
}
