package approvals_test

import (
	"context"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// The Decisions list and the ask list build each ask's view (session
// visibility reads the registry) only after their cursor is closed. Holding
// the cursor while view waits for a second connection deadlocked the
// registry pool once a few reads overlapped (the Mezze app's drawer stuck on loading
// after an answer, 2026-11-07). With one connection, the old code never
// returns.
func TestAskListsDoNotHoldTheirCursorWhileViewing(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	agent, _ := h.session("cccccccc")
	a, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card()})
	if err != nil {
		t.Fatal(err)
	}
	reg := h.dm.Deps.Store.Registry()
	// An asker that is gone: visibility misses the session cache and reads the registry.
	if _, err := reg.ExecContext(ctx, `UPDATE asks SET asked_by = ? WHERE id = ?`, "mini/0dead0ff", a.ID); err != nil {
		t.Fatal(err)
	}
	// A member reads through a scope, so visibility is checked (the owner on the socket sees all).
	member := h.tokenClient(api.MintTokenRequest{As: "vida", Project: "harbor", Role: "member"})
	reg.SetMaxOpenConns(1)
	t.Cleanup(func() { reg.SetMaxOpenConns(4) })

	for name, read := range map[string]func(context.Context) (int, error){
		"decisions": func(ctx context.Context) (int, error) {
			out, err := orchclient.Decisions(ctx, member, "", "open")
			return len(out.Decisions), err
		},
		"asks": func(ctx context.Context) (int, error) {
			out, err := orchclient.Asks(ctx, member, "", true)
			return len(out), err
		},
	} {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		n, err := read(ctx)
		cancel()
		if err != nil || n == 0 {
			t.Fatalf("%s with one registry connection: %d rows, %v", name, n, err)
		}
	}
}
