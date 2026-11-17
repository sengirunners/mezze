package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/tools"
)

// tracker ls names what needs attention; the age comes from a file on disk.
func TestBackupBrief(t *testing.T) {
	at := time.Now().Add(-90 * time.Minute)
	for _, c := range []struct {
		v    orchapi.TrackerView
		want string
	}{
		{orchapi.TrackerView{}, "backup: none"},
		{orchapi.TrackerView{Backup: &orchapi.BackupBrief{Kind: orchapi.BackupSqlite}}, "backup: no copy on disk"},
		{orchapi.TrackerView{Adapter: orchapi.AdapterNative, Backup: &orchapi.BackupBrief{Kind: orchapi.BackupSqlite, Newest: &at}}, "backup: 1h old (sqlite)"},
		{orchapi.TrackerView{Adapter: orchapi.AdapterNative, Backup: &orchapi.BackupBrief{Kind: orchapi.BackupExport, Newest: &at}}, "backup: 1h old (export) (cannot restore the store)"},
		{orchapi.TrackerView{Backup: &orchapi.BackupBrief{Kind: orchapi.BackupSqlite, Newest: &at, Overdue: true, Failing: true, ScrubPending: true}}, "backup: 1h old (sqlite) OVERDUE FAILING REDACTION WAITING"},
	} {
		if got := backupBrief(c.v); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
}

// "created" is worded by the bound mode: a new database is created; a
// first --server add (created: its local metadata) is only bound.
func TestAddTextCreatedByMode(t *testing.T) {
	newRes := orchapi.TrackerResult{Created: true, Tracker: orchapi.TrackerView{Slug: "harbor", Prefix: "bd", Adapter: "acme", Mode: orchapi.TrackerModeNew}}
	if got := addText(newRes, 0); !strings.HasPrefix(got, "created and bound bd") {
		t.Fatalf("new:\n%s", got)
	}
	srv := orchapi.TrackerResult{Created: true, Schema: &orchapi.SchemaCheck{Server: 66, Pinned: 66},
		Tracker: orchapi.TrackerView{Slug: "harbor", Prefix: "bd", Adapter: "acme", Mode: orchapi.TrackerModeServer}}
	if got := addText(srv, 0); strings.Contains(got, "created") || !strings.HasPrefix(got, "bound bd") {
		t.Fatalf("server:\n%s", got)
	}
}

// Off a terminal zm fetches without asking (there is no one to ask) and
// says so first; --yes and the help say the same.
func TestProvisionNonInteractiveSaysSo(t *testing.T) {
	old := tools.Pins
	t.Cleanup(func() { tools.Pins = old })
	tools.Pins = append([]tools.Pin(nil), old...)
	for i := range tools.Pins {
		tools.Pins[i].URL = "https://127.0.0.1:1/never" // nothing is fetched
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var errb bytes.Buffer
	err := provisionNames(ctx, &errb, t.TempDir(), []string{tools.Rclone}, true, false, false)
	if err == nil || !strings.Contains(errb.String(), "not on a terminal") || !strings.Contains(errb.String(), "--no-download") {
		t.Fatalf("err %v, stderr %q", err, errb.String())
	}
	set, _, err := backupCmd().Find([]string{"set"})
	if err != nil {
		t.Fatal(err)
	}
	f := set.Flags().Lookup("yes")
	if f == nil || !strings.Contains(f.Usage, "not a terminal") {
		t.Fatalf("--yes help: %+v", f)
	}
}
