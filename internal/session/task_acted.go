package session

// A remote task's outward actions its own machine can see (SPEC 8 "Waiting
// line"): a push. The authority sees the rest (its session's mail, comments
// and status changes go through it) and decides from both whether a task
// lost with its machine may run again elsewhere.

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// pushCheckEvery spaces one task's push checks (status is asked every beat).
const pushCheckEvery = 10 * time.Second

type pushCheck struct {
	at     time.Time
	pushed bool
}

// taskActed is what the machine adds to a remote task's status: "push" once
// a commit of its worktree branch made after the branch was created is on a
// remote-tracking ref.
func (s *Service) taskActed(ctx context.Context, taskID, address string) []string {
	if v, ok := s.pushes.Load(taskID); ok {
		pc := v.(pushCheck)
		if pc.pushed || time.Since(pc.at) < pushCheckEvery {
			if pc.pushed {
				return []string{"push"}
			}
			return nil
		}
	}
	ss, ok := s.Lookup(ctx, address)
	pushed := ok && branchPushed(ctx, ss.Worktree, ss.Branch)
	s.pushes.Store(taskID, pushCheck{at: time.Now(), pushed: pushed})
	if pushed {
		return []string{"push"}
	}
	return nil
}

// branchPushed: branch (in worktree) has a commit after its creation point
// (the first entry of its reflog: zm makes the branch) that a remote-tracking
// ref also reaches. No reflog or no git: false (nothing zm can see).
func branchPushed(ctx context.Context, worktree, branch string) bool {
	if worktree == "" || branch == "" {
		return false
	}
	ref := "refs/heads/" + branch
	log, err := git(ctx, worktree, "log", "-g", "--format=%H", ref, "--")
	if err != nil || strings.TrimSpace(log) == "" {
		return false
	}
	lines := strings.Fields(log)
	base := lines[len(lines)-1]
	count := func(args ...string) int {
		out, err := git(ctx, worktree, append([]string{"rev-list", "--count"}, args...)...)
		if err != nil {
			return -1
		}
		n, err := strconv.Atoi(strings.TrimSpace(out))
		if err != nil {
			return -1
		}
		return n
	}
	all := count(ref, "^"+base)
	local := count(ref, "^"+base, "--not", "--remotes")
	return all > 0 && local >= 0 && local < all
}
