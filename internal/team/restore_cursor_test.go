package team_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/sqlbackup"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A member must discard work lost when the authority restores an older
// project.db, including when new writes have already passed its old cursor.
func TestReplicaResyncAfterAuthorityRestore(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	kept := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "in the backup", Area: "solver"}))(t)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	rc := at(r.owner, "harbor", "")
	backup := filepath.Join(t.TempDir(), "project.db")
	if _, err := sqlbackup.Snapshot(ctx, a.dm.Deps.Store.Writer(j.Remote.Project.ID), backup, 0); err != nil {
		t.Fatal(err)
	}
	lost := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "lost after backup", Area: "solver"}))(t)
	var lostEvent string
	for _, e := range projectEvents(t, a) {
		if e.Type == orchapi.EventWorkCreated && e.Subject == lost.ID {
			lostEvent = e.ID
		}
	}
	if lostEvent == "" {
		t.Fatal("post-backup creation has no event")
	}
	eventually(t, 5*time.Second, func() error {
		if !teamService(t, r).CopyHolds(ctx, lost.ID) {
			return fmt.Errorf("copy has not received post-backup item")
		}
		if teamService(t, r).CopyCursor("harbor") < lostEvent {
			return fmt.Errorf("copy has not applied the post-backup cursor")
		}
		return nil
	})
	oldCursor := teamService(t, r).CopyCursor("harbor")
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	waitReachable(t, r, false)
	a.halt()
	projectFile := filepath.Join(a.cfg.ProjectDir(j.Remote.Project.ID), store.FileProject)
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(projectFile + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if err := sqlbackup.Copy(ctx, backup, projectFile); err != nil {
		t.Fatal(err)
	}
	a.boot(tn, tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ao = at(a.owner, "harbor", "")
	for _, e := range projectEvents(t, a) {
		if e.ID == oldCursor {
			t.Fatal("backup unexpectedly retained the member's captured cursor")
		}
	}
	newer := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "after the restore", Area: "solver"}))(t)
	evs := projectEvents(t, a)
	if len(evs) == 0 || evs[len(evs)-1].ID <= oldCursor {
		t.Fatal("restored log has not overtaken the member's lost cursor")
	}
	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	eventually(t, 10*time.Second, func() error {
		wl, err := orchclient.WorkList(ctx, rc, orchapi.WorkQuery{})
		if err != nil {
			return err
		}
		if len(wl.Items) != 2 || !hasItem(wl.Items, kept.ID) || !hasItem(wl.Items, newer.ID) || hasItem(wl.Items, lost.ID) {
			return fmt.Errorf("copy has not replaced its lost history: %+v", wl.Items)
		}
		return nil
	})
	if _, err := orchclient.Comment(ctx, rc, lost.ID, orchapi.CommentRequest{Body: "must not be queued against a ghost"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("comment on lost work: %v", err)
	}
	if _, err := orchclient.SetStatus(ctx, rc, lost.ID, orchapi.StatusRequest{Status: orchapi.WorkInReview}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("status on lost work: %v", err)
	}
}
