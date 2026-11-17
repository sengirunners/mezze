package team_test

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A work item's images live at the authority only: a member's replica
// attaches, lists, reads the bytes (with their headers) and removes through
// it, and holds no attachment file of its own.
func TestReplicaIssueAttachments(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := nativeAuthority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	rc := at(r.owner, "harbor", "")

	is := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "a decision's brief", Area: "solver"}))(t)
	var img bytes.Buffer
	if err := png.Encode(&img, image.NewNRGBA(image.Rect(0, 0, 32, 32))); err != nil {
		t.Fatal(err)
	}
	att := mustf(orchclient.AttachIssueImage(ctx, rc, is.ID, img.Bytes(), "7d"))(t)
	if l := mustf(orchclient.IssueAttachments(ctx, ao, is.ID))(t); len(l.Attachments) != 1 || l.Attachments[0].ID != att.ID {
		t.Fatalf("at the authority: %+v", l)
	}
	if l := mustf(orchclient.IssueAttachments(ctx, r.owner, is.ID))(t); len(l.Attachments) != 1 {
		t.Fatalf("listed through the replica: %+v", l)
	}
	if d := mustf(orchclient.Issue(ctx, r.owner, is.ID))(t); len(d.Attachments) != 1 || d.Attachments[0].SHA256 != att.SHA256 {
		t.Fatalf("item through the replica: %+v", d.Attachments)
	}
	resp, err := orchclient.IssueAttachment(ctx, r.owner, is.ID, att.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if int64(len(body)) != att.Size || resp.Header.Get("Content-Type") != "image/png" ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") {
		t.Fatalf("bytes through the replica: %d bytes, %v", len(body), resp.Header)
	}
	filepath.WalkDir(r.cfg.Home, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), "attachments.db") {
			t.Errorf("the replica keeps %s", p)
		}
		return nil
	})
	mustf(orchclient.RemoveIssueAttachment(ctx, rc, is.ID, att.ID))(t)
	if _, err := orchclient.IssueAttachment(ctx, a.owner, is.ID, att.ID); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("removed through the replica: %v", err)
	}
}
