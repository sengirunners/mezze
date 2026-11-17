package team_test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A forwarded body over 1 MiB is refused as such, never sent cut short
// (the authority would answer "invalid JSON body").
func TestProxyBodyOverLimit(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	rc := at(r.owner, "harbor", "")
	it := mustf(orchclient.CreateWork(ctx, rc, orchapi.CreateWorkRequest{Title: "big comment", Area: "solver"}))(t)
	_, err := orchclient.Comment(ctx, rc, it.ID, orchapi.CommentRequest{Body: strings.Repeat("x", 2<<20)})
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "over 1 MiB") {
		t.Fatalf("2 MB body: %v", err)
	}
	// One under the limit still goes through.
	if _, err := orchclient.Comment(ctx, rc, it.ID, orchapi.CommentRequest{Body: "small"}); err != nil {
		t.Fatalf("small comment: %v", err)
	}
}

// An authority whose node the tailnet renamed (a duplicate potluck-mini became
// potluck-mini-1) refuses to print a join code naming potluck-mini: a replica would
// dial the other node.
func TestInviteRefusesRenamedNode(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	tailnetNode(t, tn, tailnettest.Identity{NodeID: "nOTHER", NodeKey: "ko", Login: "other@example.com", Host: "other"}, "potluck-mini")
	a := authority(t, tn)
	_, err := a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"})
	if !api.IsCode(err, api.CodeConflict) || !strings.Contains(err.Error(), "potluck-mini-1") {
		t.Fatalf("invite on a renamed node: %v", err)
	}
	if ls := mustf(a.owner.Invites(ctx))(t); len(ls) != 0 {
		t.Fatalf("an invite was stored: %+v", ls)
	}
}

// A join refused for a local slug conflict leaves no token file behind (zm
// leave could not see it); the same code then joins once the slug is free.
func TestJoinSlugConflictLeavesNoTokenFile(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r := startDaemon(t, tn, "bea-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "kc", Login: "bea@example.com", Host: "bea-mac"})
	mustf(r.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	if _, err := r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("join over a local slug: %v", err)
	}
	ents, err := os.ReadDir(filepath.Join(r.cfg.Home, "remotes"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("orphan token files: %v", ents)
	}
	if rs := mustf(r.owner.Remotes(ctx))(t); len(rs) != 0 {
		t.Fatalf("remotes %+v", rs)
	}
}

// GET /v0/projects on a replica names an authority that does not answer in
// skipped: the list never looks complete when it is not.
func TestProjectsNamesUnreachableAuthority(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	if pl := mustf(r.owner.ProjectList(ctx))(t); len(pl.Projects) != 1 || len(pl.Skipped) != 0 {
		t.Fatalf("before: %+v", pl)
	}
	mustf(a.owner.TailnetDown(ctx))(t)
	pl := mustf(r.owner.ProjectList(ctx))(t)
	// Round C: the project stays listed (its copy serves it) and is named
	// in skipped.
	if len(pl.Projects) != 1 || len(pl.Skipped) != 1 || pl.Skipped[0].Source != "authority" || !strings.Contains(pl.Skipped[0].Error, "harbor") {
		t.Fatalf("authority down: %+v", pl)
	}
}

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// Proxied requests get the request debug line too (with proxied=true) and
// a line naming the team project they went to.
func TestProxiedRequestLogged(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	var buf syncBuf
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	r := startDaemonLog(t, tn, "bea-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "kc", Login: "bea@example.com", Host: "bea-mac"}, log)
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	mustf(orchclient.WorkList(ctx, at(r.owner, "harbor", ""), orchapi.WorkQuery{}))(t)
	out := buf.String()
	if !(strings.Contains(out, "msg=\"request proxied\"") || strings.Contains(out, "msg=\"request served from the replica copy\"")) || !strings.Contains(out, "project=harbor") {
		t.Fatalf("no proxied line:\n%s", out)
	}
	if !strings.Contains(out, "path=/v0/work") || !strings.Contains(out, "proxied=true") {
		t.Fatalf("no request line for the proxied call:\n%s", out)
	}
}
