package replaymode

import "testing"

func TestCheckpointWireEveryPartialState(t *testing.T) {
	input := enabled + "\xe2\x9b\x9c\x1bP1qopaque\x1b\\\x1b_\x9b?47l\x1b]52;opaque\a"
	for i := 0; i <= len(input); i++ {
		p := parsed(input[:i])
		b, err := p.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		var q Parser
		if err = q.UnmarshalBinary(b); err != nil || q != p {
			t.Fatalf("checkpoint split%d mismatch %v", i, err)
		}
		q.Feed([]byte(input[i:]))
		want := parsed(input)
		if q != want {
			t.Fatalf("checkpoint continuation split%d differs", i)
		}
	}
	p := parsed(enabled)
	before := p
	if p.UnmarshalBinary([]byte("invalid")) == nil || p != before {
		t.Fatal("invalid checkpoint changed receiver")
	}
}
