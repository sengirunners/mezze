package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/swap"
)

// serves is "" when the new daemon serves what before did. A local
// regression is why not at once, unless every local one fails
// `unavailable` (a store still busy at start), which is probed again until
// swapLocalGrace has passed; one that can be its tsnet node still coming
// up (a joined project's reads) is probed again every swapRemoteRetry
// until swapRemoteGrace has passed.
func (s *swapper) serves(before api.ServeCheck) string {
	start := time.Now()
	deadline, localDeadline := start.Add(swapRemoteGrace), start.Add(swapLocalGrace)
	waited, saidUnknown := false, map[string]bool{}
	for {
		after := probe(s.ctx, s.c)
		local, remote := swap.SplitRegressions(before, after)
		// A binding the old daemon ran (described before a file moved) that
		// the new one cannot is a regression, after the same grace: a
		// driver may be busy, or reach out to a host still coming up. One
		// whose listing failed now is unknown: said, never counted.
		var drivers []string
		if s.driversBefore != nil {
			var unknown []string
			drivers, unknown = wiring.DriverRegressions(s.driversBefore, driverProbe(s.ctx, s.c))
			for _, u := range unknown {
				if !saidUnknown[u] {
					saidUnknown[u] = true
					fmt.Fprintln(os.Stderr, "zm: verify:", u)
				}
			}
		}
		if len(local) > 0 {
			all := strings.Join(append(append(local, remote...), drivers...), "; ")
			if hard, _ := swap.SplitRegressions(before, unavailableAsServed(after)); len(hard) > 0 {
				return "it does not serve what the old daemon served: " + all
			}
			if !time.Now().Before(localDeadline) || s.ctx.Err() != nil {
				return fmt.Sprintf("it does not serve what the old daemon served (still unavailable after %s): %s", swapLocalGrace, all)
			}
		} else if len(remote) == 0 && len(drivers) == 0 {
			return ""
		} else if !time.Now().Before(deadline) || s.ctx.Err() != nil {
			why := "its team authorities did not answer"
			if len(remote) == 0 {
				why = "its run targets' drivers did not answer"
			}
			return fmt.Sprintf("it does not serve what the old daemon served (%s within %s): %s",
				why, swapRemoteGrace, strings.Join(append(remote, drivers...), "; "))
		}
		if !waited {
			waited = true
			fmt.Fprintf(os.Stderr, "zm: the new daemon does not serve %d item(s) yet (team authorities, run-target drivers or a store still starting); waiting up to %s\n",
				len(local)+len(remote)+len(drivers), swapRemoteGrace)
		}
		select {
		case <-s.ctx.Done():
		case <-time.After(swapRemoteRetry):
		}
	}
}

// unavailableAsServed is check with every item failing `unavailable`
// counted as served: what still regresses against it is a regression no
// wait fixes.
func unavailableAsServed(check api.ServeCheck) api.ServeCheck {
	out := check
	out.Items = make([]api.ServeItem, len(check.Items))
	for i, it := range check.Items {
		if !it.OK && it.Code == api.CodeUnavailable {
			it.OK, it.Code, it.Error = true, "", ""
		}
		out.Items[i] = it
	}
	return out
}

// driverProbe is wiring.DriverItems (tests replace it).
var driverProbe = wiring.DriverItems
