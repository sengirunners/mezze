package team

// Run targets (SPEC 16 "Asks that carry an action"): at a team
// project's authority, each machine of the project, whose it is and whether
// an approved action may run there, so an agent finds the approver's machine
// instead of falling back to a plain ask or a report. A machine is a
// person's when that person's identity holds a live node-bound token for it
// in the project (the token zm join minted for it, still backed by a grant,
// as MachineJoin counts it); the authority's own machine is its human's.
// Ready is what AskActionMachine accepts for a run (trusted, and taking
// remote tasks), on a machine only one person joined.

import (
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

var _ orchcore.AskRunTargets = (*Service)(nil)

// AskRunTargets implements orchcore.AskRunTargets. project is a ULID.
func (s *Service) AskRunTargets(ctx context.Context, project string) ([]orchapi.RunTarget, error) {
	if !s.d.Store.IsTeam(project) {
		return nil, api.Errorf(api.CodeBadRequest, "an action runs on a machine of a team project; this project has no machines").
			WithHint("ask in a team project, or file the step as work")
	}
	rt, err := s.runner()
	if err != nil {
		return nil, err
	}
	tokens, err := s.d.Projects.Tokens(ctx)
	if err != nil {
		return nil, err
	}
	ids, err := s.d.Projects.Identities(ctx)
	if err != nil {
		return nil, err
	}
	idOf := map[string]string{}
	for _, id := range ids {
		idOf[id.Name] = id.ID
	}
	// granted: the token's own identity still holds a grant for the token's
	// area (MachineJoin asks only that some token on the node does).
	granted := func(tk api.Token) (bool, error) {
		tuples, err := s.d.Projects.Tuples(ctx, idOf[tk.Identity])
		if err != nil || idOf[tk.Identity] == "" {
			return false, err
		}
		return authz.FromTuples(tuples, nil).Can(authz.Read, authz.Object{Project: project, Area: tk.Area}), nil
	}
	slug, self, now := s.d.Projects.Slug(project), s.d.Config.Machine, time.Now()
	out := []orchapi.RunTarget{}
	for _, m := range rt.ProjectMachines(ctx, project) {
		c, err := s.MachineClass(ctx, project, m.Machine)
		if err != nil {
			return nil, err
		}
		t := orchapi.RunTarget{Machine: m.Machine, People: []string{}, Trusted: c.Class == api.MachineTrusted,
			RemoteTasks: m.Stats.RemoteTasks, ScriptRuns: m.Stats.ScriptRuns, CommandRuns: m.Stats.CommandRuns, OSUser: m.Stats.OSUser, Seen: m.Seen}
		if m.Machine == self {
			if s.d.Config.Human != "" {
				t.People = append(t.People, s.d.Config.Human)
			}
		} else {
			j, err := s.d.Projects.MachineJoin(ctx, project, m.Machine)
			if err != nil {
				return nil, err
			}
			for _, tk := range tokens {
				if tk.Machine != m.Machine || tk.Project != slug || tk.Node == "" || tk.Revoked != nil ||
					tk.Expires != nil && !tk.Expires.After(now) || !slices.Contains(j.Nodes, tk.Node) || slices.Contains(t.People, tk.Identity) {
					continue
				}
				if ok, err := granted(tk); err != nil {
					return nil, err
				} else if ok {
					t.People = append(t.People, tk.Identity)
				}
			}
			sort.Strings(t.People)
		}
		switch {
		case len(t.People) > 1:
			// One machine name, several people's live tokens: a name reused
			// by a new node while an old token lives. Which node answers is
			// not known here, so no action is placed on it.
			t.Why = "joined by several people (" + strings.Join(t.People, ", ") + "): a shared machine or a reused name, so zm never picks it: name it with --run-on if it is theirs, or an owner revokes a stale token (zm token ls, zm token revoke <id>)"
		case !t.Trusted:
			t.Why = "public in this project: actions run only on trusted machines (an owner who trusts whoever runs it: zm machine-class " + m.Machine + " trusted -P " + slug + ")"
		case !t.RemoteTasks:
			t.Why = "takes no tasks from other machines: its owner sets \"remote_tasks\": true in its zm config.json and restarts its daemon"
		default:
			t.Ready = true
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Machine < out[j].Machine })
	return out, nil
}
