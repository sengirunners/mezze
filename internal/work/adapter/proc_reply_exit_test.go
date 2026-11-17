package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

func TestProcCallKeepsBufferedReplyAfterExit(t *testing.T) {
	rpcErr := api.Errorf(api.CodeConflict, "claim refused")
	for _, tc := range []struct {
		name  string
		reply *orchapi.AdapterResponse
		want  string
		code  string
	}{
		{"success", &orchapi.AdapterResponse{OK: true, Result: json.RawMessage(`"acknowledged"`)}, "acknowledged", ""},
		{"rpc error", &orchapi.AdapterResponse{Error: rpcErr}, "", api.CodeConflict},
		{"bad result", &orchapi.AdapterResponse{OK: true, Result: json.RawMessage(`{`)}, "", api.CodeUnavailable},
		{"no reply", nil, "", api.CodeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Buffered replies and done are ready together. Repetition exercises
			// both select choices rather than depending on one scheduler choice.
			for range 64 {
				got, err := callWithReplyAndExitReady(t, tc.reply)
				if got != tc.want || (tc.code == "" && err != nil) || (tc.code != "" && !api.IsCode(err, tc.code)) {
					t.Fatalf("Call = %q, %v; want %q, code %q", got, err, tc.want, tc.code)
				}
				if tc.reply == nil && !errors.Is(err, errReplyTestExit) {
					t.Fatalf("empty reply channel lost process error: %v", err)
				}
				if tc.reply != nil && tc.reply.Error != nil && err.Error() != rpcErr.Error() {
					t.Fatalf("RPC error changed: %v", err)
				}
			}
		})
	}
}

var errReplyTestExit = api.Errorf(api.CodeUnavailable, "test adapter exited")

func callWithReplyAndExitReady(t *testing.T, reply *orchapi.AdapterResponse) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	p := &Proc{name: "reply-exit", in: writer, pending: map[string]chan orchapi.AdapterResponse{}, done: make(chan struct{}), err: errReplyTestExit}
	// Hold send at its writer lock after Call registers the response slot.
	// Its small request fits in the empty pipe once released, so neither
	// process exit nor a response can race ahead of registration or behind
	// the Call select. No child process or production hook is needed.
	p.wmu.Lock()
	var unlock sync.Once
	release := func() { unlock.Do(p.wmu.Unlock) }
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var got string
	var callErr error
	go func() {
		defer close(finished)
		callErr = p.Call(ctx, orchapi.AdapterRequest{Verb: "write"}, &got)
	}()
	defer func() {
		p.mu.Lock()
		select {
		case <-p.done:
		default:
			close(p.done)
		}
		clear(p.pending)
		p.mu.Unlock()
		release()
		cancel()
		<-finished
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		p.mu.Lock()
		for rid, ch := range p.pending {
			if reply != nil {
				ch <- *reply
			}
			delete(p.pending, rid) // read removes the slot before delivering
			close(p.done)
			p.mu.Unlock()
			release()
			<-finished
			return got, callErr
		}
		p.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("Call did not register its response slot")
		}
		runtime.Gosched()
	}
}
