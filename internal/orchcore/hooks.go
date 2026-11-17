package orchcore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// The hooks through which potluck's base asks zm's modules (recipe steps 3
// and 4): the health answer's backup, heartbeat and
// holders sections, the owners module's notices, and every question the
// base has about a session. Wire registers them once per Deps. Each reads
// the Services field at call time, exactly as the base used to read the
// core.Deps field, so a test that swaps a module on a running daemon is
// still heard.
func wireHooks(d *core.Deps) {
	var cache hookCache
	d.SetSessionHooks(func() *core.SessionHooks {
		ss := Sessions(d)
		if ss == nil {
			return nil
		}
		return cache.hooks(ss)
	})
	d.AddHealthPart("backup", func(context.Context) any {
		if br, ok := Homes(d).(BackupReporter); ok {
			return br.HomeBackup()
		}
		return nil
	})
	d.AddHealthPart("heartbeat", func(context.Context) any {
		if br, ok := Homes(d).(BackupReporter); ok {
			return br.Heartbeat()
		}
		return nil
	})
	d.AddHealthPart("holders", func(context.Context) any {
		if hr, ok := Sessions(d).(HolderReporter); ok {
			return hr.HolderReport()
		}
		return nil
	})
	d.AddNotifier(func(ctx context.Context, n core.Notice) error {
		m := Mail(d)
		if m == nil {
			// No mail module: as before, the notice is not sent (the
			// owners module counts it as untold).
			return core.ErrNoNotifier
		}
		_, err := m.Send(ctx, d.System(), n.Project, "", orchapi.SendMailRequest{To: n.To, Body: n.Body})
		return err
	})
}

// sessionHooks are the base's session questions answered by ss and the
// optional interfaces it implements, by the same assertions the base made
// on core.Deps.Sessions before the hooks.
func sessionHooks(ss SessionService) *core.SessionHooks {
	h := &core.SessionHooks{
		Place: func(ctx context.Context, address string) (string, string, bool) {
			s, ok := ss.Lookup(ctx, address)
			return s.Project, s.Area, ok
		},
		Visible:     ss.Visible,
		Live:        ss.ManagedLive,
		StopBlocked: func(ctx context.Context) error { return stopBlocked(ctx, ss) },
	}
	if sn, ok := ss.(SessionNames); ok {
		h.Name = func(ctx context.Context, ref string) (string, bool) {
			s, ok := sn.ResolveName(ctx, ref)
			return s.Address, ok
		}
	}
	if sc, ok := ss.(SessionCaps); ok {
		h.Cap = sc.SessionCap
	}
	if pr, ok := ss.(PresenceReaper); ok {
		h.Reap = pr.ReapPresence
	}
	if l, ok := ss.(SessionLister); ok {
		h.Seen = func(ctx context.Context, sc authz.Scope) ([]core.SessionSeen, error) {
			list, err := l.List(ctx, sc, orchapi.SessionQuery{})
			if err != nil {
				return nil, err
			}
			out := make([]core.SessionSeen, 0, len(list))
			for _, x := range list {
				if x.Who == nil {
					continue
				}
				seen := x.Updated
				if x.Seen != nil && x.Seen.After(seen) {
					seen = *x.Seen
				}
				out = append(out, core.SessionSeen{Person: x.Who.Name, Machine: x.Machine, Seen: seen})
			}
			return out, nil
		}
	}
	if mp, ok := ss.(core.MachinePresence); ok {
		h.Machines = mp
	}
	if e, ok := ss.(SessionEnder); ok {
		h.End = e.EndManaged
	}
	if k, ok := ss.(SessionKeeper); ok {
		h.Keep = func() { k.KeepSessions(true) }
	}
	return h
}

// stopBlocked refuses a daemon stop, restart or upgrade (without force)
// that would end ss's managed sessions.
func stopBlocked(ctx context.Context, ss SessionService) error {
	bl := ss.RestartBlockers(ctx)
	if len(bl) == 0 {
		return nil
	}
	data, _ := json.Marshal(orchapi.SessionList{Sessions: bl})
	hint := "stop them first (zm session stop), or: zm daemon stop --end-sessions"
	if k, ok := ss.(SessionKeeper); ok {
		if why := k.RestartNote(ctx); why != "" {
			hint = why + "; or " + hint
		}
	}
	e := api.Errorf(api.CodeConflict, "%d managed sessions are running; stopping the daemon ends them (outcome infra): %s",
		len(bl), blockerLine(bl)).
		WithHint(hint)
	e.Data = data
	return e
}

func blockerLine(bl []orchapi.Session) string {
	var parts []string
	for i, s := range bl {
		if i == 5 {
			parts = append(parts, fmt.Sprintf("and %d more", len(bl)-5))
			break
		}
		parts = append(parts, s.Address+" ("+s.Agent+")")
	}
	return strings.Join(parts, ", ")
}
