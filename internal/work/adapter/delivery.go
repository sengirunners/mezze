package adapter

import (
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// DurableComment checks a describe result (or its lookup error) before a
// durable effect may reach an adapter. Unsupported writes require reconciliation.
func DurableComment(d orchapi.AdapterDescribe, lookupErr error) error {
	if lookupErr != nil {
		return lookupErr
	}
	if !d.IdempotentComments {
		return api.Errorf(api.CodeNotImplemented, "unknown: external comment delivery has no supported idempotency contract; reconciliation required")
	}
	return nil
}
