package issues

import (
	"context"
	"fmt"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// unleasedGrace is how long a claimed native issue may sit with no lease
// before it goes back to open: the default lease TTL (SPEC 10), so an
// unleased claim ends as an unrenewed lease would.
const unleasedGrace = time.Hour

// releaseUnleased puts back to open every claimed (in_progress or hooked)
// native issue of a project that binds its native issues when no lease names
// it and it has not changed for grace. Such a claim has no
// deadline and nobody holds it: a hand-set status, a hand-set assignee (which
// is not a zm claim, so its lease went), or a release that found nothing to
// release. The ready queue skips it, so its work stalls unseen. The assignee
// stays, as bd keeps it on a status open, and a comment says why. Only the
// authority's copy is swept; a team replica's follows it.
func (s *Service) releaseUnleased(ctx context.Context, grace time.Duration) {
	w := orchcore.Work(s.d)
	if w == nil {
		return
	}
	metas, err := s.st.Existing(ctx)
	if err != nil {
		s.warn("unleased claims: list stores", "err", err)
		return
	}
	tr, _ := s.d.Team.(core.TeamReplicas)
	for _, m := range metas {
		if ctx.Err() != nil {
			return
		}
		p := s.nativePrefix(m.Project)
		if p == "" || tr != nil && tr.Replicates(m.Project) {
			continue
		}
		ps, err := s.st.Project(ctx, m.Project, false)
		if err != nil {
			continue
		}
		cutoff := s.now().Add(-grace)
		l, err := ps.List(ctx, orchapi.IssueQuery{Status: orchapi.IssueInProgress + "," + orchapi.IssueHooked, UpdatedBefore: &cutoff, All: true},
			istore.Areas{All: true}, s.now())
		if err != nil {
			s.warn("unleased claims: list", "project", m.Project, "err", err)
			continue
		}
		for _, it := range l.Issues {
			leased, err := w.ItemLeased(ctx, m.Project, p+":"+it.ID)
			if err != nil || leased {
				continue // an expired lease is the work ticker's to release
			}
			s.releaseOne(ctx, m.Project, ps, it)
		}
	}
}

// releaseOne is releaseUnleased's write for it, refused if it changed since.
func (s *Service) releaseOne(ctx context.Context, project string, ps istore.ProjectStore, it orchapi.Issue) {
	open := orchapi.IssueOpen
	sys := s.d.System()
	_, _, err := s.mutate(ctx, sys, project, ps, istore.Mutation{Op: istore.OpUpdate, ID: it.ID, Update: &orchapi.UpdateIssueRequest{Status: &open},
		Check: func(cur orchapi.Issue) error {
			if cur.Status != it.Status || !cur.UpdatedAt.Equal(it.UpdatedAt) {
				return api.Errorf(api.CodeConflict, "%s changed since the sweep read it", it.ID)
			}
			return nil
		}})
	if err != nil {
		if !api.IsCode(err, api.CodeConflict) {
			s.warn("unleased claims: release", "project", project, "issue", it.ID, "err", err)
		}
		return
	}
	body := fmt.Sprintf("zm: back to open: %s since %s and no lease held it, so nothing would renew or release it.", it.Status,
		it.UpdatedAt.UTC().Format(time.RFC3339))
	if it.Assignee != "" {
		body += " The assignee (" + it.Assignee + ") stays; an assignee is not a claim."
	}
	body += " Claim it (bd update " + it.ID + " --claim) if you are on it."
	if _, _, err := s.mutate(ctx, sys, project, ps, istore.Mutation{Op: istore.OpComment, ID: it.ID,
		Comment: &orchapi.IssueCommentRequest{Text: body}}); err != nil {
		s.warn("unleased claims: comment", "project", project, "issue", it.ID, "err", err)
	}
}
