package orchcore

import (
	"reflect"
	"sync/atomic"

	"github.com/sengirunners/potluck/kit/core"
)

// hookCache keeps the hooks built for the last session module seen, so the
// base's hot paths (Deps.Visible on every redacted address) do not build
// them per call. Only a pointer module is cached: comparing it is always
// safe (a test fake that is a struct may hold values == panics on).
type hookCache struct {
	last atomic.Pointer[cachedHooks]
}

type cachedHooks struct {
	ss SessionService
	h  *core.SessionHooks
}

func (c *hookCache) hooks(ss SessionService) *core.SessionHooks {
	if reflect.TypeOf(ss).Kind() != reflect.Pointer {
		return sessionHooks(ss)
	}
	if l := c.last.Load(); l != nil && l.ss == ss {
		return l.h
	}
	h := sessionHooks(ss)
	c.last.Store(&cachedHooks{ss, h})
	return h
}
