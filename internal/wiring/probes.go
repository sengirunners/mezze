package wiring

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/swap"
)

// Probes are the swap probe's orchestration parts (SPEC 2, "Swaps"): bd's
// issue reads, the work list and the trackers' adapters. Full adds them to
// the daemon's self-check (d.AddProbe) and `zm daemon update` passes them to
// swap.Probe; Potluck's own parts (health, projects, joined) run first.
var Probes = []swap.Part{{Name: "issues", Run: probeIssues}, {Name: "work", Run: probeWork}, {Name: "adapters", Run: probeAdapters}}

func probeIssues(c *client.Client, p *swap.Probing) {
	var slugs []string
	_ = p.Call(func(ctx context.Context) error {
		ip, err := orchclient.IssueProjects(ctx, c)
		if err != nil {
			p.Add(swap.ItemIssues, "*", err)
			return err
		}
		for _, x := range ip.Projects {
			slugs = append(slugs, x.Slug)
		}
		for _, s := range ip.Skipped {
			p.Add(swap.ItemIssues, p.Name(s.Project), fmt.Errorf("%s", s.Error))
		}
		return nil
	})
	for _, s := range slugs {
		p.Add(swap.ItemIssues, s, p.Call(func(ctx context.Context) error {
			_, err := orchclient.Issues(ctx, c, orchapi.IssueQuery{Project: s, Limit: 1})
			return err
		}))
	}
}

func probeWork(c *client.Client, p *swap.Probing) {
	for _, s := range p.Slugs() {
		p.Add(swap.ItemWork, s, p.Call(func(ctx context.Context) error {
			_, err := orchclient.WorkList(ctx, c, orchapi.WorkQuery{Project: s, Limit: 1})
			return err
		}))
	}
}

func probeAdapters(c *client.Client, p *swap.Probing) {
	_ = p.Call(func(ctx context.Context) error {
		srcs, err := orchclient.WorkSources(ctx, c, "")
		if err != nil {
			p.Add(swap.ItemAdapter, "*", err)
			return err
		}
		for _, s := range srcs {
			if s.Name == "local" {
				continue
			}
			// A mirror still loading right after a start has not failed.
			var e error
			if !s.OK && (s.Sync == nil || s.Sync.State != orchapi.SyncLoading) {
				e = fmt.Errorf("%s", firstOf(s.Error, syncErr(s.Sync), "not ok"))
			}
			p.Add(swap.ItemAdapter, p.Name(s.Project)+"/"+s.Prefix, e)
		}
		return nil
	})
}

// ProbeAgents adds to check the task agents c's daemon can start, one
// swap.ItemAgent each (OK: its binary resolves, see agentItem). A swap's
// verify compares them: an agent the old daemon could start and the new
// one cannot is a regression (a swap that lost claude from its
// PATH). Not part of swap.Probe: the self-check would list every agent not
// installed here as a problem.
func ProbeAgents(ctx context.Context, c *client.Client, check api.ServeCheck) api.ServeCheck {
	cctx, cancel := context.WithTimeout(ctx, swap.ProbeTimeout)
	defer cancel()
	items := slices.Clone(check.Items)
	ta, err := orchclient.TaskAgents(cctx, c)
	if err != nil {
		items = append(items, swap.Item(swap.ItemAgent, "*", err))
	}
	for _, a := range ta.Agents {
		items = append(items, agentItem(a))
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Key() < items[j].Key() })
	check.Items = items
	return check
}

// agentItem is a's ItemAgent. OK is "its binary resolves": available, or
// found with its auth probe failing (Authenticated is set only for a
// binary that resolved). Login state is not the build's doing: an auth
// flap or a CLI slower than the probe's 3 s at verify would otherwise
// roll back a good build and mark its release failed here. The reason
// stays in Error for display.
func agentItem(a orchapi.TaskAgent) api.ServeItem {
	it := api.ServeItem{Kind: swap.ItemAgent, Name: a.Agent, OK: a.Available || a.Authenticated != nil}
	if !a.Available {
		it.Error = "unavailable: " + swap.OneLine(firstOf(a.Reason, "no reason given"))
		if !it.OK {
			it.Code = api.CodeUnavailable
		}
	}
	return it
}

func syncErr(s *orchapi.WorkSync) string {
	if s == nil {
		return ""
	}
	return s.Error
}

func firstOf(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}
