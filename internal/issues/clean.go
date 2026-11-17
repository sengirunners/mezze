package issues

import (
	"strings"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
)

func cleanMutation(m *istore.Mutation) error {
	clean := func(p *string) error {
		if p == nil {
			return nil
		}
		raw := *p
		cleaned := textclean.Clean(raw)
		if raw != "" && (cleaned != raw || strings.ContainsRune(raw, 0x2800)) && textclean.Blank(cleaned) {
			return api.Errorf(api.CodeBadRequest, "text contains only hidden or blank characters")
		}
		*p = cleaned
		return nil
	}
	cleanList := func(list []string) error {
		for i := range list {
			if err := clean(&list[i]); err != nil {
				return err
			}
		}
		return nil
	}
	// A removal names an existing label, including legacy invisible names.
	// Keep hidden-only selectors intact so the store can match their identity.
	cleanRemoval := func(p *string) error {
		if p != nil && strings.TrimSpace(*p) != "" && textclean.Blank(*p) {
			return nil
		}
		return clean(p)
	}
	if c := m.Create; c != nil {
		if textclean.Blank(c.Title) {
			return api.Errorf(api.CodeBadRequest, "empty title")
		}
		for _, p := range []*string{&c.Title, &c.Description, &c.Design, &c.AcceptanceCriteria, &c.Notes, &c.SpecID, &c.Assignee, &c.Owner, &c.ExternalRef, &c.IssueType, &c.Status} {
			if err := clean(p); err != nil {
				return err
			}
		}
		if err := cleanList(c.Labels); err != nil {
			return err
		}
		c.Metadata = textclean.JSON(c.Metadata)
	}
	if u := m.Update; u != nil {
		if u.AppendNotes != nil && textclean.Blank(*u.AppendNotes) {
			return api.Errorf(api.CodeBadRequest, "empty appended notes")
		}
		if u.Title != nil && textclean.Blank(*u.Title) {
			return api.Errorf(api.CodeBadRequest, "empty title")
		}
		for _, p := range []*string{u.Title, u.Description, u.Design, u.AcceptanceCriteria, u.Notes, u.AppendNotes, u.SpecID, u.Assignee, u.ExternalRef, u.IssueType, u.Status, &u.Session} {
			if err := clean(p); err != nil {
				return err
			}
		}
		for _, list := range [][]string{u.AddLabels, u.SetLabels} {
			if err := cleanList(list); err != nil {
				return err
			}
		}
		for i := range u.RemoveLabels {
			if err := cleanRemoval(&u.RemoveLabels[i]); err != nil {
				return err
			}
		}
		if textclean.HasHiddenOnlyJSON(u.Metadata) {
			return api.Errorf(api.CodeBadRequest, "metadata contains hidden-only text")
		}
		u.Metadata = textclean.JSON(u.Metadata)
	}
	if err := clean(&m.Assignee); err != nil {
		return err
	}
	if m.Op == istore.OpLabelRemove {
		if err := cleanRemoval(&m.Label); err != nil {
			return err
		}
	} else {
		if err := clean(&m.Label); err != nil {
			return err
		}
	}
	if m.Comment != nil {
		if textclean.Blank(m.Comment.Text) {
			return api.Errorf(api.CodeBadRequest, "empty comment")
		}
		if err := clean(&m.Comment.Text); err != nil {
			return err
		}
	}
	if m.CommentEdit != nil {
		if textclean.Blank(m.CommentEdit.Text) {
			return api.Errorf(api.CodeBadRequest, "empty comment")
		}
		if err := clean(&m.CommentEdit.Text); err != nil {
			return err
		}
	}
	if m.Close != nil {
		if err := clean(&m.Close.Session); err != nil {
			return err
		}
		if err := clean(&m.Close.Reason); err != nil {
			return err
		}
	}
	if m.Reopen != nil {
		if err := clean(&m.Reopen.Reason); err != nil {
			return err
		}
	}
	return nil
}
