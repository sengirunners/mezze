package homes

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

func decodeExact(t *testing.T, b []byte) map[string]any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v map[string]any
	if err := dec.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestLiveHomeJSON: a sanitized copy of a live tracker's home.json from
// before the Dolt homes were deleted (its Dolt admin login, a finished move
// to zm-native, the frozen rollback copy of server://127.0.0.1:3307/beads)
// with an export and a sqlite backup loads; the backups are listed and
// scheduled as before; a write keeps every backup record exactly and drops
// the old Dolt fields.
func TestLiveHomeJSON(t *testing.T) {
	h := newHarness(t)
	pid := h.harbor.ID
	old, err := os.ReadFile(filepath.Join("testdata", "live-home.json"))
	if err != nil {
		t.Fatal(err)
	}
	was := decodeExact(t, old)
	for _, k := range []string{"admin", "move", "previous"} {
		if was[k] == nil {
			t.Fatalf("the fixture lost its %q record: it must stay an old home.json", k)
		}
	}
	path := h.s.homeStateFile(pid, "bd")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}

	st, err := h.s.loadHome(pid, "bd")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Backups) != 2 || st.Backups[0].Kind != orchapi.BackupExport || st.Backups[1].Kind != orchapi.BackupSqlite ||
		st.Backups[1].ScrubDue == nil || st.Backups[1].StoreBytes == 0 || st.Backups[1].Keep != 24 {
		t.Fatalf("backups: %+v", st.Backups)
	}

	// The tracker's routes read it: its backups are listed, with their schedule.
	trs := []api.Tracker{{Adapter: orchapi.AdapterNative, Prefix: "bd"}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	bs, err := orchclient.TrackerBackups(h.ctx, h.as(h.owner, "harbor"), "bd")
	if err != nil || len(bs) != 2 || bs[1].Kind != orchapi.BackupSqlite || !bs[1].ScrubPending || bs[1].Due == nil {
		t.Fatalf("listed: %+v %v", bs, err)
	}

	// A write that changes nothing keeps the backups exactly and drops the rest.
	if _, err := h.s.updateHome(pid, "bd", func(*homeState) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	now := decodeExact(t, got)
	if want := map[string]any{"backups": was["backups"]}; !reflect.DeepEqual(now, want) {
		t.Fatalf("home.json after a write:\nwas %s\nnow %s", old, got)
	}
}
