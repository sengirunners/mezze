package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

func TestTrackerHomeText(t *testing.T) {
	ok := time.Now().Add(-12 * time.Minute)
	w := orchapi.TrackerWhere{
		Tracker: orchapi.TrackerView{Slug: "harbor", Prefix: "bd", Adapter: orchapi.AdapterNative, Sync: &orchapi.WorkSync{State: "ok", Items: 11547}},
		Home:    orchapi.HomeView{Target: orchapi.AdapterNative, Kind: "service"},
		Backups: []orchapi.BackupView{{ID: "file:///a", Kind: "sqlite", EverySeconds: 3600, LastOK: &ok, Bytes: 3 << 30, Path: "/a/harbor-bd/sqlite"},
			{ID: "file:///b", Kind: "export", EverySeconds: 86400, Failures: 2, LastError: "disk full", Path: "/b/harbor-bd/export"}},
		Notes: []string{"a note"},
	}
	got := whereText(w)
	for _, want := range []string{"harbor bd (native)", "zm's native issues, on this machine", "mirror:   ok",
		"file:///a (sqlite, hourly): last ok 12m ago, 3.0 GB", "file:///b (export, daily): never ran, FAILING (2x): disk full",
		"note:     a note"} {
		if !strings.Contains(got, want) {
			t.Errorf("where lacks %q:\n%s", want, got)
		}
	}
	w.Tracker.Adapter = "acme"
	if got := whereText(w); !strings.Contains(got, "the acme service (backups are export snapshots)") {
		t.Errorf("service tracker:\n%s", got)
	}
}
