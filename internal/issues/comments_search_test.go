package issues_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// GET /v0/issues/comments: pages past its limit, hides comments on issues
// the caller cannot read, never names a hidden author, escapes LIKE.
func TestSearchComments(t *testing.T) {
	ctx := context.Background()
	f := newAuthzFixture(t)
	solver := f.ownerAt("harbor/solver")
	a, err := orchclient.CreateIssue(ctx, solver, orchapi.NewIssueRequest{Title: "many comments"})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 150 {
		if _, err := orchclient.CommentIssue(ctx, solver, a.ID, orchapi.IssueCommentRequest{Text: fmt.Sprintf("zm: note %03d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	search := func(q string) orchapi.IssueCommentList {
		t.Helper()
		req := httptest.NewRequest("GET", "/v0/issues/comments?"+q, nil)
		req.Header.Set(api.HeaderProtocol, api.Protocol)
		rec := httptest.NewRecorder()
		f.dm.SocketHandler().ServeHTTP(rec, req)
		var l orchapi.IssueCommentList
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &l) != nil {
			t.Fatalf("%s: %d %s", q, rec.Code, rec.Body.String())
		}
		return l
	}
	// One store with more than the limit: a next page, then the rest.
	p1 := search("project=harbor&prefix=zm:&limit=100")
	if len(p1.Comments) != 100 || p1.Next == "" {
		t.Fatalf("page 1: %d comments, next %q", len(p1.Comments), p1.Next)
	}
	p2 := search("project=harbor&prefix=zm:&limit=100&after=" + p1.Next)
	if len(p2.Comments) != 50 || p2.Next != "" || p2.Comments[0].ID <= p1.Next {
		t.Fatalf("page 2: %d comments, next %q", len(p2.Comments), p2.Next)
	}
	// LIKE escaping: % and _ are literal.
	if _, err := orchclient.CommentIssue(ctx, solver, a.ID, orchapi.IssueCommentRequest{Text: "100% done"}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.CommentIssue(ctx, solver, a.ID, orchapi.IssueCommentRequest{Text: "1000 done"}); err != nil {
		t.Fatal(err)
	}
	if l := search("project=harbor&prefix=100%25"); len(l.Comments) != 1 || l.Comments[0].Text != "100% done" {
		t.Fatalf("prefix 100%%: %+v", l.Comments)
	}
	// A comment on a billing issue: the solver guest never sees it.
	b, err := orchclient.CreateIssue(ctx, f.ownerAt("harbor"), orchapi.NewIssueRequest{Title: "SECRET billing", Labels: []string{"billing"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.CommentIssue(ctx, f.ownerAt("harbor"), b.ID, orchapi.IssueCommentRequest{Text: "zm: secret plan"}); err != nil {
		t.Fatal(err)
	}
	code, body := f.as("guest", "GET", "/v0/issues/comments?project=harbor&contains=secret", "")
	if code != 200 || strings.Contains(body, "secret plan") {
		t.Fatalf("guest saw a hidden issue's comment: %d %s", code, body)
	}
	if l := search("project=harbor&contains=secret"); len(l.Comments) != 1 {
		t.Fatalf("owner search for the billing comment: %+v", l.Comments)
	}
	// A comment by a session the guest cannot see: its author shows as
	// someone, and an author filter naming that session finds nothing.
	sess := f.ownerAt("harbor/solver")
	sess.As = "mini/0000beef"
	if c, err := orchclient.CommentIssue(ctx, sess, a.ID, orchapi.IssueCommentRequest{Text: "from a hidden session"}); err != nil || c.AuthorAddress != "mini/0000beef" {
		t.Fatalf("comment as a session: %+v %v", c, err)
	}
	code, body = f.as("guest", "GET", "/v0/issues/comments?project=harbor&author=mini/0000beef", "")
	if code != 200 || strings.Contains(body, "hidden session") {
		t.Fatalf("an author filter named a hidden session: %d %s", code, body)
	}
	// An author filter never returns rows whose author is shown as someone
	// (that would name them).
	code, body = f.as("guest", "GET", "/v0/issues/comments?project=harbor&author=human:ivo", "")
	if code != 200 {
		t.Fatalf("guest author search: %d %s", code, body)
	}
	var gl orchapi.IssueCommentList
	_ = json.Unmarshal([]byte(body), &gl)
	for _, c := range gl.Comments {
		if c.AuthorAddress != "human:ivo" {
			t.Fatalf("an author filter returned a redacted author: %+v", c)
		}
	}
}
