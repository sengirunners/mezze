package issues

import (
	"context"
	"slices"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/events"
)

// OrphanedLabel marks an issue whose filer ended and left it open with nobody
// on it (SPEC "Native issues", Orphaned): a follow-up a session filed
// or named in its handoff and never dispatched. A supervisor's queue is
// `bd ready --label orphaned`; the label goes when the issue gets a holder.
const OrphanedLabel = "orphaned"

// watchEnded marks what each ending session left orphaned. An overflow drops
// the ends it missed: a mark is a hint for the supervisor, not a guarantee.
func (s *Service) watchEnded(ctx context.Context) {
	f := events.Filter{Types: []string{orchapi.EventSessionEnded}}
	ch, cancel := s.d.Events.Subscribe(s.d.System(), f, 256)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { cancel() }()
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-ch:
				if !ok {
					s.warn("session-ended events overflowed: some orphaned issues are not marked")
					ch, cancel = s.d.Events.Subscribe(s.d.System(), f, 256)
					continue
				}
				if e.Type == orchapi.EventSessionEnded && e.Subject != "" { // the session's address
					s.markOrphans(ctx, e.Subject)
				}
			}
		}
	}()
}

// markOrphans labels every issue address filed that is still open with no
// assignee, in every project store this daemon keeps (the authority's, for a
// team project: a replica's sessions end there through presence).
func (s *Service) markOrphans(ctx context.Context, address string) {
	metas, err := s.st.Existing(ctx)
	if err != nil {
		s.warn("orphans: list stores", "err", err)
		return
	}
	sys := s.d.System()
	for _, m := range metas {
		ps, err := s.st.Project(ctx, m.Project, false)
		if err != nil {
			continue
		}
		l, err := ps.List(ctx, orchapi.IssueQuery{Status: orchapi.IssueOpen, NoAssignee: true, CreatedByAddr: address, All: true}, istore.Areas{All: true}, s.now())
		if err != nil {
			s.warn("orphans: list", "project", m.Project, "err", err)
			continue
		}
		for _, it := range l.Issues {
			if it.Ephemeral || it.Pinned || slices.Contains(it.Labels, OrphanedLabel) {
				continue
			}
			if _, _, err := s.mutate(ctx, sys, m.Project, ps, istore.Mutation{Op: istore.OpLabelAdd, ID: it.ID, Label: OrphanedLabel}); err != nil {
				s.warn("orphans: mark", "issue", it.ID, "err", err)
			}
		}
	}
}

// unorphan drops the label from an issue a write gave a holder or took out of
// open (a claim, an assignee, a close): orphaned stays true of what it labels.
// The add itself re-checks: a claim between markOrphans' list and its add
// leaves no mark on a held issue.
func (s *Service) unorphan(ctx context.Context, project string, ps istore.ProjectStore, m istore.Mutation, it orchapi.Issue) {
	switch m.Op {
	case istore.OpClaim, istore.OpUpdate, istore.OpClose, istore.OpLabelAdd:
	default:
		return
	}
	if !slices.Contains(it.Labels, OrphanedLabel) || (it.Status == orchapi.IssueOpen && it.Assignee == "") {
		return
	}
	if _, _, err := s.mutate(ctx, s.d.System(), project, ps, istore.Mutation{Op: istore.OpLabelRemove, ID: it.ID, Label: OrphanedLabel}); err != nil {
		s.warn("orphans: clear", "issue", it.ID, "err", err)
	}
}
