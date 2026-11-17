package issues_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// Comments: the author edits (history keeps the old text; base refuses an
// unseen change), any session of the author's identity may; the author or
// a moderator deletes (a tombstone whose text and deleter only moderators
// see); nobody else does either, guests nothing.
func TestCommentEditDelete(t *testing.T) {
	ctx := context.Background()
	f := newAuthzFixture(t)
	owner := f.ownerAt("harbor/solver")
	it, err := orchclient.CreateIssue(ctx, owner, orchapi.NewIssueRequest{Title: "commented"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := orchclient.CommentIssue(ctx, owner, it.ID, orchapi.IssueCommentRequest{Text: "first draft"})
	if err != nil {
		t.Fatal(err)
	}
	// The author edits; history keeps the old text.
	e, err := orchclient.EditComment(ctx, owner, it.ID, c.ID, orchapi.EditCommentRequest{Text: "second draft"})
	if err != nil || e.Text != "second draft" || e.EditedAt == nil {
		t.Fatalf("edit: %+v %v", e, err)
	}
	hs, err := orchclient.IssueHistory(ctx, owner, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	last := hs[len(hs)-1]
	if last.Kind != "comment_edited" || !strings.Contains(string(last.Fields["text"]), "first draft") {
		t.Fatalf("history: %+v", last)
	}
	// base: an edit over a change it did not see is refused.
	if _, err := orchclient.EditComment(ctx, owner, it.ID, c.ID, orchapi.EditCommentRequest{Text: "stale", Base: hs[0].ID}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("stale base: %v", err)
	}
	if _, err := orchclient.EditComment(ctx, owner, it.ID, c.ID, orchapi.EditCommentRequest{Text: "third draft", Base: last.ID}); err != nil {
		t.Fatalf("current base: %v", err)
	}
	// Another session of the same identity is the author too.
	sess := f.ownerAt("harbor/solver")
	sess.As = "mini/0000aaaa"
	if _, err := orchclient.EditComment(ctx, sess, it.ID, c.ID, orchapi.EditCommentRequest{Text: "from another session"}); err != nil {
		t.Fatalf("same identity, another session: %v", err)
	}
	// A member may neither edit nor delete someone else's comment; a guest
	// nothing at all.
	path := "/v0/issues/" + it.ID + "/comments/" + c.ID
	if code, body := f.as("member", "PATCH", path, `{"text":"hijack"}`); code != 403 {
		t.Fatalf("member edit: %d %s", code, body)
	}
	if code, body := f.as("member", "DELETE", path, ""); code != 403 {
		t.Fatalf("member delete: %d %s", code, body)
	}
	if code, body := f.as("guest", "PATCH", path, `{"text":"guest"}`); code != 403 {
		t.Fatalf("guest edit: %d %s", code, body)
	}
	// The member's own comment: they edit and delete it.
	code, body := f.as("member", "POST", "/v0/issues/"+it.ID+"/comments", `{"text":"mina says"}`)
	var mc orchapi.IssueComment
	if code != 200 || json.Unmarshal([]byte(body), &mc) != nil {
		t.Fatalf("member comment: %d %s", code, body)
	}
	if code, body := f.as("member", "PATCH", "/v0/issues/"+it.ID+"/comments/"+mc.ID, `{"text":"mina says again"}`); code != 200 {
		t.Fatalf("member edits own: %d %s", code, body)
	}
	// The owner (an admin) deletes the member's comment as a moderator.
	d, err := orchclient.DeleteComment(ctx, owner, it.ID, mc.ID)
	if err != nil || !d.Deleted {
		t.Fatalf("moderator delete: %+v %v", d, err)
	}
	// The member sees a tombstone: no text, no deleter; history names "a
	// moderator" and drops the deleted comment's edit texts.
	code, body = f.as("member", "GET", "/v0/issues/"+it.ID+"/comments", "")
	if code != 200 || strings.Contains(body, "mina says") || strings.Contains(body, "human:ivo\",\"deleted") || !strings.Contains(body, `"deleted":true`) {
		t.Fatalf("member view of a tombstone: %d %s", code, body)
	}
	code, body = f.as("member", "GET", "/v0/issues?comments=1&status=all", "")
	if code != 200 || strings.Contains(body, "mina says") || !strings.Contains(body, `"deleted":true`) {
		t.Fatalf("member's issue list with comments shows a tombstone's text: %d %s", code, body)
	}
	// The author deleting again (already a moderator's tombstone) is a no-op
	// that still does not name the moderator.
	code, body = f.as("member", "DELETE", "/v0/issues/"+it.ID+"/comments/"+mc.ID, "")
	if code != 200 || strings.Contains(body, "human:ivo") || strings.Contains(body, "mina says") {
		t.Fatalf("author re-delete: %d %s", code, body)
	}
	code, body = f.as("member", "GET", "/v0/issues/"+it.ID+"/history", "")
	if code != 200 || strings.Contains(body, "mina says") || !strings.Contains(body, "a moderator") {
		t.Fatalf("member view of history: %d %s", code, body)
	}
	// The moderator sees the text and who deleted it.
	cs, err := orchclient.IssueComments(ctx, owner, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	var tomb *orchapi.IssueComment
	for i := range cs {
		if cs[i].ID == mc.ID {
			tomb = &cs[i]
		}
	}
	if tomb == nil || tomb.Text != "mina says again" || tomb.DeletedBy == "" {
		t.Fatalf("moderator view of a tombstone: %+v", tomb)
	}
	// A deleted comment cannot be edited.
	if code, _ := f.as("member", "PATCH", "/v0/issues/"+it.ID+"/comments/"+mc.ID, `{"text":"back"}`); code != 409 {
		t.Fatalf("edit after delete: %d", code)
	}
}

func TestCleanIssueWrites(t *testing.T) {
	ctx := context.Background()
	f := newAuthzFixture(t)
	c := f.ownerAt("harbor/solver")
	it, err := orchclient.CreateIssue(ctx, c, orchapi.NewIssueRequest{Title: "hi\u200b", Description: "text\u202e"})
	if err != nil || it.Title != "hi" || it.Description != "text" {
		t.Fatalf("create: %+v %v", it, err)
	}
	comment, err := orchclient.CommentIssue(ctx, c, it.ID, orchapi.IssueCommentRequest{Text: "hi\U000e0061 👩‍💻"})
	if err != nil || comment.Text != "hi 👩‍💻" {
		t.Fatalf("comment: %+v %v", comment, err)
	}
	edited, err := orchclient.EditComment(ctx, c, it.ID, comment.ID, orchapi.EditCommentRequest{Text: "edit\u200b"})
	if err != nil || edited.Text != "edit" {
		t.Fatalf("edit: %+v %v", edited, err)
	}
}

func TestReviewInvisibleUpdateDoesNotClear(t *testing.T) {
	ctx := context.Background()
	f := newAuthzFixture(t)
	c := f.ownerAt("harbor/solver")
	it, err := orchclient.CreateIssue(ctx, c, orchapi.NewIssueRequest{Title: "x", Description: "preserve", Notes: "first"})
	if err != nil {
		t.Fatal(err)
	}
	hidden := "\u200b"
	if _, err := orchclient.UpdateIssue(ctx, c, it.ID, orchapi.UpdateIssueRequest{Description: &hidden}); err == nil {
		t.Fatal("accepted invisible update")
	}
	if _, err := orchclient.UpdateIssue(ctx, c, it.ID, orchapi.UpdateIssueRequest{AppendNotes: &hidden}); err == nil {
		t.Fatal("accepted invisible append")
	}
}

func TestReviewIssueFieldsAndAppend(t *testing.T) {
	ctx := context.Background()
	f := newAuthzFixture(t)
	c := f.ownerAt("harbor/solver")
	it, err := orchclient.CreateIssue(ctx, c, orchapi.NewIssueRequest{Title: "x", Notes: "first", Labels: []string{"a\u200bb"}, Assignee: "a\u200bb", ExternalRef: "e\u202ef", SpecID: "s\u200bt", Metadata: json.RawMessage(`{"k\u202e":"v\u200b"}`)})
	if err != nil || len(it.Labels) != 1 || it.Labels[0] != "ab" || it.Assignee != "ab" || it.ExternalRef != "ef" || it.SpecID != "st" || string(it.Metadata) != `{"k":"v"}` {
		t.Fatalf("create %+v %v", it, err)
	}
	hidden := "\u200b"
	if _, err := orchclient.UpdateIssue(ctx, c, it.ID, orchapi.UpdateIssueRequest{AppendNotes: &hidden}); err == nil {
		t.Fatal("invisible append accepted")
	}
	got, err := orchclient.Issue(ctx, c, it.ID)
	if err != nil || got.Notes != "first" {
		t.Fatalf("notes %+v %v", got, err)
	}
	next := "more\u200b"
	if _, err := orchclient.UpdateIssue(ctx, c, it.ID, orchapi.UpdateIssueRequest{AppendNotes: &next}); err != nil {
		t.Fatal(err)
	}
	got, err = orchclient.Issue(ctx, c, it.ID)
	if err != nil || got.Notes != "first\nmore" {
		t.Fatalf("append %+v %v", got, err)
	}
}

func TestReview2MetadataDoesNotClear(t *testing.T) {
	ctx := context.Background()
	f := newAuthzFixture(t)
	c := f.ownerAt("harbor/solver")
	it, err := orchclient.CreateIssue(ctx, c, orchapi.NewIssueRequest{Title: "x", Metadata: json.RawMessage(`{"a":"keep"}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = orchclient.UpdateIssue(ctx, c, it.ID, orchapi.UpdateIssueRequest{Metadata: json.RawMessage(`{"a":"\u200b"}`)})
	if err == nil {
		t.Fatal("hidden-only metadata accepted")
	}
	got, err := orchclient.Issue(ctx, c, it.ID)
	if err != nil || string(got.Metadata) != `{"a":"keep"}` {
		t.Fatalf("metadata %s %v", got.Metadata, err)
	}
}
