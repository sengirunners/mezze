package orchclient

import (
	"context"
	"fmt"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// ResolveRuntimeSession replaces a runtime-derived reply address with the
// canonical address returned by registration of its full runtime id. Callers
// must select this fallback only for local, unmanaged, untokened agents without
// an explicit reply address. New remains a transport-only constructor: hooks
// and MCP own registration and must not recursively register here.
func ResolveRuntimeSession(ctx context.Context, c *client.Client, req orchapi.RegisterRequest) error {
	// A truncated runtime id can name another live session. Registration must
	// run unattributed, and a failed resolution must never retain that guess.
	c.As = ""
	ss, err := Register(ctx, c, req)
	if err != nil {
		return fmt.Errorf("resolve runtime session: %w", err)
	}
	addr, err := api.ParseAddress(ss.Address)
	if err != nil || addr.Kind != api.KindSession {
		return api.Errorf(api.CodeUnavailable, "runtime registration returned no canonical session address")
	}
	if ss.State == orchapi.StateEnded {
		return api.Errorf(api.CodeConflict, "runtime session has ended; registration needs live agent evidence before it can receive mail")
	}
	c.As = ss.Address
	return nil
}
