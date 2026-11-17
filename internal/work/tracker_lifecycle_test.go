package work

import (
	"context"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/authz"
	"testing"
)

func TestForgetDoesNotWipeReplacementSyncer(t *testing.T) {
	h := newHarness(t)
	stopped, release := make(chan struct{}), make(chan struct{})
	old := &syncer{done: release, cancel: func() { close(stopped) }}
	key := orchapi.TrackerKey(h.harbor.ID, "bd")
	h.s.m.mu.Lock()
	h.s.m.by[key] = old
	h.s.m.mu.Unlock()
	finished := make(chan error, 1)
	go func() { finished <- h.s.m.forget(context.Background(), h.harbor.ID, "bd") }()
	<-stopped
	// The old cycle is still exiting; another request has rebound the prefix.
	replacement := &syncer{done: make(chan struct{}), cancel: func() {}}
	h.s.m.mu.Lock()
	h.s.m.by[key] = replacement
	h.s.m.mu.Unlock()
	db, err := h.d.Store.Project(authz.All(), authz.Read, h.harbor.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := mirrorUpsert(h.ctx, db, "bd", []orchapi.WorkItem{{ID: "bd:new", Title: "new binding", Status: orchapi.WorkOpen}}, 1); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if _, ok, err := mirrorGet(h.ctx, db, h.harbor.ID, "bd", "new"); err != nil || !ok {
		t.Fatalf("cleanup wiped the replacement mirror: present=%v err=%v", ok, err)
	}
	h.s.m.mu.Lock()
	delete(h.s.m.by, key)
	h.s.m.mu.Unlock()
}
