package work

import (
	"context"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// WaitingItems reads a bounded page of waiting items as sub.
func (s *Service) WaitingItems(ctx context.Context, sub *authz.Subject, sc authz.Scope, f orchapi.WaitingFilter) (orchapi.WorkPage, error) {
	return s.page(ctx, sub, sc, orchapi.WorkPageQuery{Waiting: &f, After: f.After, Limit: pageMax, Order: "updated",
		Status: []orchapi.WorkStatus{orchapi.WorkOpen, orchapi.WorkClaimed, orchapi.WorkInReview}})
}

func (s *Service) ResolveItem(ctx context.Context, sub *authz.Subject, project, id string) (string, string, string, error) {
	l, err := s.locateRecorded(ctx, sub, project, id)
	return l.id, l.area, l.project, err
}

// ItemLabels implements orchcore.WorkLabeler.
func (s *Service) ItemLabels(ctx context.Context, sub *authz.Subject, project, id string) ([]string, error) {
	l, err := s.locateRecorded(ctx, sub, project, id)
	if err != nil {
		return nil, err
	}
	it, err := s.load(ctx, sub, l)
	return it.Labels, err
}

// ItemText implements orchcore.WorkDescriber.
func (s *Service) ItemText(ctx context.Context, sub *authz.Subject, project, id string) (string, string, []string, error) {
	l, err := s.locateRecorded(ctx, sub, project, id)
	if err != nil {
		return "", "", nil, err
	}
	it, err := s.load(ctx, sub, l)
	return it.Title, it.Body, it.Labels, err
}

// ReadItem implements orchcore.WorkItemReader.
func (s *Service) ReadItem(ctx context.Context, sub *authz.Subject, project, id string) (orchapi.WorkItem, error) {
	l, err := s.locateRecorded(ctx, sub, project, id)
	if err != nil {
		return orchapi.WorkItem{}, err
	}
	return s.load(ctx, sub, l)
}

// RecordComment implements orchcore.WorkRecorder.
func (s *Service) RecordComment(ctx context.Context, sub *authz.Subject, project, id, writeID, body string) error {
	l, err := s.locateRecorded(ctx, sub, project, id)
	if err == nil && !l.local() {
		t, e := s.tracker(ctx, l.project, *l.tr)
		err = adapter.DurableComment(t.desc, e)
	}
	if err != nil {
		return err
	}
	_, err = s.comment(ctx, sub, l, body, writeID, 0)
	return err
}

// locateRecorded follows moves; bare IDs are tried under the project's trackers.
func (s *Service) locateRecorded(ctx context.Context, sub *authz.Subject, project, id string) (loc, error) {
	if !strings.Contains(id, ":") {
		if f, ok := s.d.Projects.File(project); ok {
			for _, tr := range orchcore.ProjectTrackers(f) {
				l, err := s.locateRecorded(ctx, sub, project, tr.Prefix+":"+id)
				if !api.IsCode(err, api.CodeNotFound) {
					return l, err
				}
			}
		}
		return loc{}, api.NotFound("work item", id)
	}
	l, err := s.locate(ctx, sub, sub.Scope.Only(project), id)
	if api.IsCode(err, api.CodeNotFound) {
		if m, ok, merr := s.locateMoved(ctx, sub, id); merr != nil {
			return loc{}, merr
		} else if ok {
			return m, nil
		}
	}
	return l, err
}
