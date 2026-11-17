package session

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// EndManaged waits for actual outcomes before the daemon stop response.
// Unverified or unresponsive sessions are left running and named to the caller.
func (s *Service) EndManaged(ctx context.Context) (int, []api.DaemonSessionFailure, error) {
	s.spawnMu.Lock()
	defer s.spawnMu.Unlock()
	rows, err := listRows(ctx, s.d.Store.Registry(), `WHERE managed = 1 AND machine = ? AND state != ?`, s.d.Config.Machine, string(orchapi.StateEnded))
	if err != nil {
		return 0, nil, err
	}
	type result struct {
		ended   bool
		failure api.DaemonSessionFailure
	}
	results := make(chan result, len(rows))
	var wg sync.WaitGroup
	for _, r := range rows {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p := s.proc(r.ID); p != nil {
				p.mu.Lock()
				p.daemonStop = true
				p.mu.Unlock()
			}
			ss, err := s.Stop(ctx, s.d.System(), r.ID, orchapi.StopRequest{Escalate: true, Force: true})
			if latest, ok := s.load(ctx, r.ID); ok && latest.State == orchapi.StateEnded {
				ss = latest.Session
			}
			if ss.State == orchapi.StateEnded {
				deadline := time.Now().Add(2 * time.Second)
				for {
					st, exists := procStat(r.PID)
					gone := r.PID > 1 && ((!exists && syscall.Kill(r.PID, 0) == syscall.ESRCH) || (exists && (st.zombie || (r.pidStart > 0 && st.start != r.pidStart))))
					if gone {
						results <- result{ended: true}
						return
					}
					if time.Now().After(deadline) || ctx.Err() != nil {
						break
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
			reason := fmt.Sprintf("child pid %d may still be running; inspect and stop it manually", r.PID)
			if err != nil {
				reason = fmt.Sprintf("%s; inspect and stop the process manually", err)
			}
			results <- result{failure: api.DaemonSessionFailure{Session: r.Address, Reason: reason}}
		}()
	}
	wg.Wait()
	close(results)
	var ended int
	var failed []api.DaemonSessionFailure
	for r := range results {
		if r.ended {
			ended++
		} else {
			failed = append(failed, r.failure)
		}
	}
	sort.Slice(failed, func(i, j int) bool { return failed[i].Session < failed[j].Session })
	return ended, failed, nil
}
