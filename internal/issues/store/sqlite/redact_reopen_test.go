package sqlite

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
)

func TestRedactReopenReasonHistory(t *testing.T) {
	for _, edited := range []bool{false, true} {
		name := "original reason"
		if edited {
			name = "edited reason"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			p := testProject(t)
			all := istore.Areas{All: true}
			const secret = "sensitive-reopen-reason"
			const replacement = "edited-sensitive-reason"
			const unrelated = "an unrelated earlier reason"
			is := create(t, p, orchapi.NewIssueRequest{Title: "reopened issue"})
			reopen := func(id, reason string) *orchapi.IssueComment {
				t.Helper()
				apply(t, p, istore.Mutation{Op: istore.OpClose, ID: id})
				return apply(t, p, istore.Mutation{Op: istore.OpReopen, ID: id, Reopen: &orchapi.ReopenIssueRequest{Reason: reason}}).Comment
			}
			reopen(is.ID, unrelated)
			comment := reopen(is.ID, secret)
			if comment == nil {
				t.Fatal("reopen did not create a comment")
			}
			if edited {
				apply(t, p, istore.Mutation{Op: istore.OpCommentEdit, ID: is.ID, CommentEdit: &istore.CommentEdit{ID: comment.ID, Text: replacement}})
			}
			other := create(t, p, orchapi.NewIssueRequest{Title: "another issue with the same reason"})
			reopen(other.ID, secret)
			before, err := p.History(ctx, other.ID, all)
			if err != nil {
				t.Fatal(err)
			}
			redact := istore.Mutation{Op: istore.OpCommentRedact, ID: is.ID, CommentID: comment.ID, Moderate: func(string) bool { return true }}
			apply(t, p, redact)
			assertClean := func(label string, v any) {
				t.Helper()
				b, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(b), secret) || strings.Contains(string(b), replacement) {
					t.Errorf("%s retains the redacted reopen reason: %s", label, b)
				}
			}
			hs, err := p.History(ctx, is.ID, all)
			if err != nil {
				t.Fatal(err)
			}
			assertClean("history", hs)
			var kept, scrubbed bool
			for _, h := range hs {
				if h.Kind == "reopened" {
					kept = kept || h.Comment == unrelated
					scrubbed = scrubbed || h.Comment == redactedText
				}
			}
			if !kept || !scrubbed {
				t.Fatalf("reopen history: unrelated kept %v, target redacted %v", kept, scrubbed)
			}
			changes, err := p.Changes(ctx, "", 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, change := range changes {
				if change.IssueID == is.ID {
					assertClean("changes", change)
				}
			}
			exported, err := p.(istore.Mover).Export(ctx, is.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertClean("export", exported)
			after, err := p.History(ctx, other.ID, all)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(before)
			a, _ := json.Marshal(after)
			if string(a) != string(b) {
				t.Fatalf("another issue's history changed: before %s, after %s", b, a)
			}
			if !apply(t, p, redact).Noop {
				t.Error("repeated redaction was not a no-op")
			}
		})
	}
}

func TestRedactLegacyReopenReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		edited bool
		moved  bool
	}{
		{"tombstoned", false, false},
		{"edited then tombstoned", true, false},
		{"moved tombstone", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			src, dst := twoProjects(t)
			is := create(t, src, orchapi.NewIssueRequest{Title: "legacy redaction", ID: "oc-old"})
			const reason = "legacy-sensitive-reopen-reason"
			apply(t, src, istore.Mutation{Op: istore.OpClose, ID: is.ID})
			comment := apply(t, src, istore.Mutation{Op: istore.OpReopen, ID: is.ID, Reopen: &orchapi.ReopenIssueRequest{Reason: reason}}).Comment
			if tc.edited {
				apply(t, src, istore.Mutation{Op: istore.OpCommentEdit, ID: is.ID, CommentEdit: &istore.CommentEdit{ID: comment.ID, Text: "edited legacy reason"}})
			}
			redact := istore.Mutation{Op: istore.OpCommentRedact, ID: is.ID, CommentID: comment.ID, Moderate: func(string) bool { return true }}
			apply(t, src, redact)
			// Model the old binary: the comment and edits are redacted, but
			// the reopened history row still holds its original reason.
			if _, err := src.(*project).w.ExecContext(ctx, `UPDATE history SET comment = ? WHERE issue_id = ? AND kind = 'reopened'`, reason, is.ID); err != nil {
				t.Fatal(err)
			}
			p := src
			if tc.moved {
				moveOne(t, src, dst, is.ID, "sg-old", nil)
				p, redact.ID = dst, "sg-old"
			}
			r := apply(t, p, redact)
			if r.Noop || !slices.Contains(r.Redacted, reason) {
				t.Fatalf("legacy retry: noop=%v redacted=%v", r.Noop, r.Redacted)
			}
			cs, err := p.Comments(ctx, redact.ID, istore.Areas{All: true})
			if err != nil || len(cs) != 1 || cs[0].Text != redactedText {
				t.Fatalf("comments after retry: %+v %v", cs, err)
			}
			hs, err := p.History(ctx, redact.ID, istore.Areas{All: true})
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(hs)
			if err != nil || strings.Contains(string(b), reason) {
				t.Fatalf("history after retry: %s %v", b, err)
			}
			if !apply(t, p, redact).Noop {
				t.Error("repaired legacy redaction was not idempotent")
			}
		})
	}
}

func TestRedactReopenReasonPreservesSameTextOnAnotherReopen(t *testing.T) {
	ctx := context.Background()
	p := testProject(t)
	is := create(t, p, orchapi.NewIssueRequest{Title: "same reason twice"})
	const reason = "same reason"
	var target *orchapi.IssueComment
	for i := 0; i < 2; i++ {
		apply(t, p, istore.Mutation{Op: istore.OpClose, ID: is.ID})
		target = apply(t, p, istore.Mutation{Op: istore.OpReopen, ID: is.ID, Reopen: &orchapi.ReopenIssueRequest{Reason: reason}}).Comment
	}
	apply(t, p, istore.Mutation{Op: istore.OpCommentRedact, ID: is.ID, CommentID: target.ID, Moderate: func(string) bool { return true }})
	hs, err := p.History(ctx, is.ID, istore.Areas{All: true})
	if err != nil {
		t.Fatal(err)
	}
	var kept, redacted int
	for _, h := range hs {
		if h.Kind == "reopened" {
			if h.Comment == reason {
				kept++
			}
			if h.Comment == redactedText {
				redacted++
			}
		}
	}
	if kept != 1 || redacted != 1 {
		t.Fatalf("reopened rows: kept %d redacted %d; want one each", kept, redacted)
	}
}

func TestRedactReopenReasonRequiresNativeAssociation(t *testing.T) {
	for _, tc := range []struct {
		name string
		q    string
	}{
		{"imported anchor", `UPDATE history SET source_id = 'imported-anchor' WHERE kind = 'commented'`},
		{"imported reopen", `UPDATE history SET source_id = 'imported-reopen' WHERE kind = 'reopened'`},
		{"actor mismatch", `UPDATE history SET actor = 'someone else' WHERE kind = 'reopened'`},
		{"time mismatch", `UPDATE history SET created_at = created_at + 1 WHERE kind = 'reopened'`},
		{"ambiguous anchor", `INSERT INTO history SELECT id || 'X', issue_id, kind, actor, actor_addr, fields, comment, area_before, area_after, created_at, NULL FROM history WHERE kind = 'commented'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			p := testProject(t)
			is := create(t, p, orchapi.NewIssueRequest{Title: "association guards"})
			const reason = "unproven historical text"
			apply(t, p, istore.Mutation{Op: istore.OpClose, ID: is.ID})
			comment := apply(t, p, istore.Mutation{Op: istore.OpReopen, ID: is.ID, Reopen: &orchapi.ReopenIssueRequest{Reason: reason}}).Comment
			if _, err := p.(*project).w.ExecContext(ctx, tc.q); err != nil {
				t.Fatal(err)
			}
			apply(t, p, istore.Mutation{Op: istore.OpCommentRedact, ID: is.ID, CommentID: comment.ID, Moderate: func(string) bool { return true }})
			hs, err := p.History(ctx, is.ID, istore.Areas{All: true})
			if err != nil {
				t.Fatal(err)
			}
			var kept bool
			for _, h := range hs {
				kept = kept || h.Kind == "reopened" && h.Comment == reason
			}
			if !kept {
				t.Fatal("unproven reopen association changed history")
			}
		})
	}
}
