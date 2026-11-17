package session

import (
	"context"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// The task driver's agent name is for sandbox tasks only: the session
// limit and the machine's session count leave those sessions out, so a
// plain session (by agent or by its program's name) may not carry it.
func TestSandboxAgentReserved(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	for _, req := range []orchapi.SpawnRequest{
		{Agent: "sandbox", Command: []string{"/bin/sh", "-c", "exec sleep 30"}, Cwd: h.home},
		{Command: []string{"/usr/local/bin/sandbox"}, Cwd: h.home},
	} {
		_, err := orchclient.Spawn(ctx, h.c, req)
		wantErrCode(t, err, api.CodeBadRequest)
	}
	h.spawn("shell", "exec sleep 30", nil)
	if n := h.s.managedCounted(ctx); n != 1 {
		t.Fatalf("counted %d", n)
	}
}
