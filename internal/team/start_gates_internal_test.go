package team

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// The early-cancel map stays bounded: a start prunes the
// expired cancels without any later cancel arriving, and a flood of cancels
// for starts that never come keeps at most earlyMax, dropping the oldest,
// while a fresh early cancel still stops its start.
func TestStartGatesEarlyBounded(t *testing.T) {
	var g startGates
	g.cancel("old")
	g.early["old"] = time.Now().Add(-2*opTakeWithin - time.Second)
	g.cancel("fresh")
	g.early["fresh"] = time.Now().Add(-time.Minute)
	g.cancel("old2")
	g.early["old2"] = time.Now().Add(-3 * opTakeWithin)
	_, sg, ok := g.begin(context.Background(), "a-start")
	if !ok {
		t.Fatal("a start with no cancel refused")
	}
	g.end("a-start", sg, "")
	if _, ok := g.early["old"]; ok || len(g.early) != 1 {
		t.Fatalf("begin left expired early cancels: %v", g.early)
	}
	if _, _, ok := g.begin(context.Background(), "fresh"); ok {
		t.Fatal("a start cancelled early ran")
	}

	for i := 0; i < earlyMax+50; i++ {
		g.cancel(fmt.Sprintf("never-%d", i))
	}
	if len(g.early) > earlyMax {
		t.Fatalf("early cancels: %d, cap %d", len(g.early), earlyMax)
	}
	last := fmt.Sprintf("never-%d", earlyMax+49)
	if _, _, ok := g.begin(context.Background(), last); ok {
		t.Fatal("the newest early cancel was dropped")
	}
}
