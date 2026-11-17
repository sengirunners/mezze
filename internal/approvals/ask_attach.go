package approvals

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

var askAttachMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0009_attachments", Script: `ALTER TABLE asks ADD COLUMN attachments TEXT NOT NULL DEFAULT '';`},
}

// askAttachments is the metadata of want, live attachments of the native
// work item an ask is about (zm ask new --attach), in the order asked. The
// bytes stay on the item: its read check serves them.
func (s *Service) askAttachments(ctx context.Context, project, work string, want []string) ([]orchapi.AskAttachment, error) {
	if len(want) == 0 {
		return nil, nil
	}
	if work == "" {
		return nil, api.Errorf(api.CodeBadRequest, "attachments go on the work item the ask is about: add work")
	}
	if len(want) > orchapi.MaxIssueAttachments {
		return nil, api.Errorf(api.CodeBadRequest, "an item keeps at most %d attachments", orchapi.MaxIssueAttachments)
	}
	live, err := s.itemAttachments(ctx, project, work)
	if api.IsCode(err, api.CodeUnavailable) {
		return nil, err
	}
	if err != nil {
		return nil, api.Errorf(api.CodeBadRequest, "%s keeps no attachments (they are for native work items)", work)
	}
	var out []orchapi.AskAttachment
	for _, w := range want {
		i := slices.IndexFunc(live, func(a orchapi.IssueAttachment) bool { return a.ID == w })
		if !ids.Valid(w) || i < 0 {
			return nil, api.Errorf(api.CodeBadRequest, "attachment %q is not live on %s", w, work)
		}
		if slices.ContainsFunc(out, func(a orchapi.AskAttachment) bool { return a.ID == w }) {
			continue
		}
		a := live[i]
		out = append(out, orchapi.AskAttachment{ID: a.ID, Type: a.Type, Size: a.Size, SHA256: a.SHA256, Created: a.Created, Expires: a.Expires})
	}
	return out, nil
}

// itemAttachments is the live attachments of the native work item work.
func (s *Service) itemAttachments(ctx context.Context, project, work string) ([]orchapi.IssueAttachment, error) {
	iss := orchcore.Issues(s.d)
	if iss == nil {
		return nil, api.Errorf(api.CodeUnavailable, "the issues module is not running")
	}
	_, id, _ := strings.Cut(work, ":")
	var wi orchapi.WorkItem
	if err := iss.NativeCall(ctx, project, orchapi.AdapterRequest{Verb: orchapi.VerbGet, ID: id}, &wi); err != nil {
		return nil, err
	}
	var live []orchapi.IssueAttachment
	raw, _ := json.Marshal(wi.Extra["attachments"])
	_ = json.Unmarshal(raw, &live)
	return live, nil
}

// liveAttachments drops an ask's attachments that expired or left its work
// item since (zm issues attachments rm, or a failed ask's rollback). When
// the item cannot be read for another reason, only the expired go.
func (s *Service) liveAttachments(ctx context.Context, a orchapi.Ask) []orchapi.AskAttachment {
	if len(a.Attachments) == 0 {
		return a.Attachments
	}
	now := s.now()
	live, err := s.itemAttachments(ctx, a.Project, a.Subject.Work)
	gone := api.IsCode(err, api.CodeNotFound)
	return slices.DeleteFunc(slices.Clone(a.Attachments), func(at orchapi.AskAttachment) bool {
		return gone || !at.Expires.After(now) ||
			err == nil && !slices.ContainsFunc(live, func(l orchapi.IssueAttachment) bool { return l.ID == at.ID })
	})
}
