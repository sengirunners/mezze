package approvals

import (
	"context"
	"slices"
	"strings"

	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// meHops bounds the walk up a session's parents.
const meHops = 8

// meOwner is the person an ask to "me" is for: starting at the
// asking session and following its Parent, the first launcher who is a
// person answering project, as the name the ask keeps, with via saying which
// session gave it. A remote task's own launcher is the machine that ran it
// for its parent, so the walk passes it by. A caller that is no session
// starts from its own identity. Never the authority's or a member machine's
// human by default: a guest's, box's or runner's session names nobody, and
// the ask falls to the work item's owner: label, then the owners.
func (s *Service) meOwner(ctx context.Context, sub *authz.Subject, project string) (name, via string) {
	if sub == nil || !sub.Scope.WholeProject(project) {
		return "", ""
	}
	people, err := s.people(ctx, project)
	if err != nil {
		return "", ""
	}
	people = answering(people)
	origins, _ := orchcore.Sessions(s.d).(orchcore.SessionOrigins)
	tasks, _ := orchcore.Team(s.d).(orchcore.TaskParents)
	addr := sub.Address
	if a, err := api.ParseAddress(addr); err != nil || a.Kind != api.KindSession {
		if n := s.launcherPerson(ctx, people, sub.Identity.ID); n != "" {
			return n, "me: " + n + ", who asked"
		}
		return "", ""
	}
	seen := map[string]bool{}
	for hop := 0; hop < meHops && addr != "" && !seen[addr]; hop++ {
		seen[addr] = true
		if tasks != nil {
			if p := tasks.TaskParent(ctx, addr); p != "" {
				addr = p
				continue
			}
		}
		launcher, parent, ok := "", "", false
		if origins != nil {
			launcher, parent, ok = origins.Origin(ctx, addr)
		}
		if !ok && hop == 0 {
			// A replica's session its heartbeat has not listed yet: every
			// session of a member machine is launched by the identity it joined as.
			launcher = sub.Identity.ID
		}
		if n := s.launcherPerson(ctx, people, launcher); n != "" {
			return n, "me: " + n + ", who launched " + addr
		}
		addr = parent
	}
	return "", ""
}

// launcherPerson is the identity name of launcher (an identity ULID or
// name) when it is one of people's names, else "".
func (s *Service) launcherPerson(ctx context.Context, people []person, launcher string) string {
	if launcher == "" || s.d.Projects == nil {
		return ""
	}
	id, err := s.d.Projects.Identity(ctx, launcher)
	if err != nil || id.Kind != api.IdentityHuman {
		return ""
	}
	n := strings.ToLower(id.Name)
	for _, p := range people {
		if slices.Contains(p.Names, n) {
			return n
		}
	}
	return ""
}
