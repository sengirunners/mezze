package team

import (
	"context"
	"sync"
	"time"
)

// startCancelled is a start's refusal when a cancel reached this machine
// before the start made its task.
const startCancelled = "cancelled before it started"

// startGates are the task starts running on this machine for an authority
// (a script run's checkout can take minutes), so a cancel that arrives
// meanwhile stops the start before its script runs, and a cancel that
// arrives before its start is remembered for it. Memory only: a start runs
// in this process, and an authority stops carrying a start after
// opTakeWithin. The early cancels are pruned on every begin and cancel:
// kept 2×opTakeWithin, and at most earlyMax (the oldest go first).
type startGates struct {
	mu    sync.Mutex
	open  map[string]*startGate // by op id
	early map[string]time.Time  // cancels with no start here yet
}

// earlyMax bounds the early cancels remembered.
const earlyMax = 1024

type startGate struct {
	cancel    context.CancelFunc
	done      chan struct{}
	cancelled bool   // under startGates.mu
	task      string // the local task the start made; set before done closes
}

// begin opens op id's start: ctx ends when a cancel arrives. ok is false
// when a cancel came first (the start must not run).
func (g *startGates) begin(ctx context.Context, id string) (context.Context, *startGate, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneLocked(time.Now())
	if _, ok := g.early[id]; ok {
		return ctx, nil, false
	}
	if g.open == nil {
		g.open = map[string]*startGate{}
	}
	cctx, cancel := context.WithCancel(ctx)
	sg := &startGate{cancel: cancel, done: make(chan struct{})}
	g.open[id] = sg
	return cctx, sg, true
}

// end closes a start opened by begin, with the local task it made ("" none).
func (g *startGates) end(id string, sg *startGate, task string) {
	g.mu.Lock()
	delete(g.open, id)
	g.mu.Unlock()
	sg.task = task
	sg.cancel()
	close(sg.done)
}

// running reports whether op id's start runs in this process now.
func (g *startGates) running(id string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open[id] != nil
}

// stopped reports whether a cancel reached sg.
func (g *startGates) stopped(sg *startGate) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return sg.cancelled
}

// cancel cancels op id's start: the open one (returned, to wait for), else
// it is remembered so the start, when it comes, never runs.
func (g *startGates) cancel(id string) *startGate {
	g.mu.Lock()
	defer g.mu.Unlock()
	if sg := g.open[id]; sg != nil {
		sg.cancelled = true
		sg.cancel()
		return sg
	}
	if g.early == nil {
		g.early = map[string]time.Time{}
	}
	now := time.Now()
	g.pruneLocked(now)
	if _, ok := g.early[id]; !ok && len(g.early) >= earlyMax {
		g.dropOldestLocked()
	}
	g.early[id] = now
	return nil
}

// pruneLocked forgets early cancels older than 2×opTakeWithin: by then the
// authority no longer carries their start.
func (g *startGates) pruneLocked(now time.Time) {
	for k, at := range g.early {
		if now.Sub(at) > 2*opTakeWithin {
			delete(g.early, k)
		}
	}
}

// dropOldestLocked forgets the oldest early cancel.
func (g *startGates) dropOldestLocked() {
	var old string
	var oldAt time.Time
	for k, at := range g.early {
		if old == "" || at.Before(oldAt) {
			old, oldAt = k, at
		}
	}
	delete(g.early, old)
}
