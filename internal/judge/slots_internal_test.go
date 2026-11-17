package judge

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A daemon runs at most Parallel provider CLIs at once; a call that
// finds every slot busy past its deadline is an error (held), never a pass.
func TestSlotsBoundCLIs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	for range Parallel {
		slots <- struct{}{}
	}
	defer func() {
		for range Parallel {
			<-slots
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := CLI{Provider: "claude"}.Call(ctx, "x", json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "every judge slot stayed busy") {
		t.Fatalf("busy slots: %v", err)
	}
	j := Judge{Name: "t", Prompt: "Judge it.", Schema: json.RawMessage(`{}`), Timeout: 50 * time.Millisecond}
	if v := j.Run(context.Background(), CLI{Provider: "claude"}, "x"); v.Status != Held {
		t.Fatalf("busy slots judged: %+v", v)
	}
}

// A schema that asks for a reason gets it back on a pass or a fail.
func TestParseReason(t *testing.T) {
	v := Parse(json.RawMessage(`{"pass":false,"fixes":["Log in yourself."],"reason":"It asks the reader\n to log in."}`))
	if v.Status != Fail || v.Reason != "It asks the reader to log in." {
		t.Fatalf("%+v", v)
	}
	if v := Parse(json.RawMessage(`{"pass":true,"fixes":[]}`)); v.Status != Pass || v.Reason != "" {
		t.Fatalf("%+v", v)
	}
}
