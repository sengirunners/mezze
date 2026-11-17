package session

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// A spawned structured session's first op written is its start:
// Spawn returns before pipeStart writes it, so a turn sent at once (a local
// one, or one forwarded from another machine) waits for it instead of
// reaching the adapter first and being refused.
func TestPipeOpsWaitForTheStart(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })
	p := &proc{id8: "s1", addr: "mini/s1", pty: w, exited: make(chan struct{}), pipe: newPipeState()}
	p.pipe.startSent = make(chan struct{})
	s := &Service{}
	gone, cancel := context.WithCancel(context.Background())
	cancel()

	// Before the start: a turn whose caller gives up is refused, unwritten.
	if _, err := s.sendOp(gone, p, orchapi.AgentOp{Op: orchapi.AgentOpTurn, Text: "early"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a turn before the start: %v", err)
	}
	// One that waits is written after the start, whichever is sent first.
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	turned := make(chan error, 1)
	go func() {
		_, err := s.sendOp(ctx, p, orchapi.AgentOp{Op: orchapi.AgentOpTurn, Text: "waits"})
		turned <- err
	}()
	if res, err := s.sendOp(gone, p, orchapi.AgentOp{Op: orchapi.AgentOpStart, Start: &orchapi.AgentStart{Cwd: "/"}}); err != nil || !res.Pending {
		t.Fatalf("start: %+v %v", res, err)
	}
	br := bufio.NewReader(r)
	var ops []string
	for range 2 {
		line, err := br.ReadBytes('\n')
		if err != nil {
			t.Fatal(err)
		}
		var op orchapi.AgentOp
		if err := json.Unmarshal(line, &op); err != nil {
			t.Fatal(err)
		}
		ops = append(ops, op.Op+":"+op.Text)
	}
	if ops[0] != orchapi.AgentOpStart+":" || ops[1] != orchapi.AgentOpTurn+":waits" {
		t.Fatalf("written in order %v", ops)
	}
	stop()
	if err := <-turned; err != nil {
		t.Fatalf("the waiting turn: %v", err)
	}
}
