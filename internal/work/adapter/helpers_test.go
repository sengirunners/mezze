package adapter

import (
	"context"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// Call is CallEnv with the inherited environment.
func Call(ctx context.Context, path string, req orchapi.AdapterRequest, out any) error {
	return CallEnv(ctx, path, nil, req, out)
}

// StartProc is StartProcEnv with the inherited environment.
func StartProc(path string) (*Proc, error) { return StartProcEnv(path, nil) }

// inflight is how many requests wait for an answer.
func (p *Proc) inflight() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending)
}
