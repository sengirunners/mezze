package session

import (
	"bytes"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/mezze/internal/session/holder/replaymode"
	"testing"
)

func TestHolderReplayMirrorFirstGapAndUnknownCompatibility(t *testing.T) {
	var known replaymode.Parser
	known.Feed([]byte("\x1b[?1049;1003;1006;2004h\x1b]52;partial"))
	b, err := holder.EncodeReplay(holder.ReplayCheckpoint{From: 200, State: known})
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{0, 1, 2} {
		r := holder.NewRing(16)
		r.WriteAt(100, nil) // Hello can precede a clamp
		pending := readHolderReplay(r, version, b)
		prepareHolderReplay(r, pending, 200)
		if s, e := r.Bounds(); s != 100 || e != 100 {
			t.Fatal("metadata changed raw bounds")
		}
		r.WriteAt(200, []byte("suffix")) // actual s.feed remains the only raw writer
		got := r.NextReplay(200, 100)
		if got.From != 200 || string(got.Data) != "suffix" {
			t.Fatal("raw stream changed")
		}
		if version == 1 {
			if got.State != known {
				t.Fatal("known first-gap parser lost")
			}
		} else if got.State.SafeBoundary() || got.State.Modes().AlternateKnown {
			t.Fatal("old/unsupported holder guessed modes")
		}
	}
	r := holder.NewRing(16)
	r.WriteAt(100, nil)
	pending := readHolderReplay(r, 1, b)
	prepareHolderReplay(r, pending, 201)
	r.WriteAt(201, []byte("wrong-offset"))
	if got := r.NextReplay(201, 100); got.State.Modes().AlternateKnown {
		t.Fatal("mismatched M applied")
	}
	if readHolderReplay(r, 1, b) != nil {
		t.Fatal("accepted historical metadata")
	}
	if readHolderReplay(r, 1, bytes.Repeat([]byte("x"), 1000)) != nil {
		t.Fatal("malformed metadata accepted")
	}
}
