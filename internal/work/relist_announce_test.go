package work

import (
	"context"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/potluck/kit/api"
)

// listOnlyHandlers are the nocursor tracker without changes: synced in
// list mode (installNoCursor puts it on PATH too).
func listOnlyHandlers() map[string]adapter.Handler {
	hs := noCursorHandlers()
	delete(hs, orchapi.VerbChanges)
	hs[orchapi.VerbDescribe] = func(context.Context, orchapi.AdapterRequest) (any, error) {
		return orchapi.AdapterDescribe{Spec: api.Protocol, Name: "listonly", Prefix: "nc", Verbs: orchapi.RequiredVerbs}, nil
	}
	return hs
}

// A relist owes the copies its items on disk from the wipe on.
// A daemon that stops after the wipe, before the listing ends, still
// announces the restored items when it starts again; the debt then clears.
// Both sync modes: changes (fullChanges) and list (fullList).
func TestRelistAnnounceSurvivesRestart(t *testing.T) {
	for _, name := range []string{"nocursor", "listonly"} {
		t.Run(name, func(t *testing.T) { relistAnnounceSurvivesRestart(t, name) })
	}
}

func relistAnnounceSurvivesRestart(t *testing.T, name string) {
	quiet(t)
	h := newHarness(t)
	installNoCursor(t)
	p := h.harbor
	tr := api.Tracker{Adapter: name, Prefix: "nc", Env: api.TrackerEnvInherit}
	if err := h.s.m.syncer(p.ID, tr).wait(h.ctx); err != nil {
		t.Fatal(err)
	}
	announced := func() bool {
		for _, e := range h.events(orchapi.EventWorkChanged) {
			if e.Subject == "nc:1" {
				return true
			}
		}
		return false
	}
	if announced() {
		t.Fatal("a first, unannounced listing announced nc:1")
	}
	// The relist's wipe, then the daemon stops before any listing runs.
	if err := h.s.m.forgetAnnounce(h.ctx, p.ID, "nc", orchapi.TrackerBinding(tr)); err != nil {
		t.Fatal(err)
	}
	h.s.m.close()
	h.s.m = newMirrors(h.s)
	if err := h.s.m.syncer(p.ID, tr).wait(h.ctx); err != nil {
		t.Fatal(err)
	}
	if !announced() {
		t.Fatal("after a restart mid-relist, the listing did not announce nc:1")
	}
	db, err := h.s.m.syncer(p.ID, tr).db()
	if err != nil {
		t.Fatal(err)
	}
	// The loop saves the row just after the cycle wait saw.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		r, ok, err := loadSyncRow(h.ctx, db, "nc")
		if err == nil && ok && !r.announce && r.complete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sync row %+v ok=%v err=%v: want complete, nothing owed", r, ok, err)
		}
	}
}
