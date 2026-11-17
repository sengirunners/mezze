package homes

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/potluck/kit/api"
)

// fakeHomes points the user's home at a temp dir: no test touches $HOME.
func fakeHomes(t *testing.T) string {
	t.Helper()
	uh, err := os.MkdirTemp("", "zmuh")
	if err != nil {
		t.Fatal(err)
	}
	oldH := userHomeDir
	userHomeDir = func() (string, error) { return uh, nil }
	t.Cleanup(func() {
		userHomeDir = oldH
		os.RemoveAll(uh)
	})
	return uh
}

// TestHomeServiceTracker: a tracker whose service is its home (an adapter's)
// gets export backups, which snapshot its items.
func TestHomeServiceTracker(t *testing.T) {
	fakeHomes(t)
	h := newHarness(t)
	state := h.installFake()
	edit(t, state, func(st *fake.State) {
		st.Issues["app-1"] = &fake.Issue{ID: "app-1", Title: "one", Status: "open", UpdatedAt: fake.Now()}
		st.Issues["app-2"] = &fake.Issue{ID: "app-2", Title: "two", Status: "closed", UpdatedAt: fake.Now()}
	})
	trs := []api.Tracker{{Adapter: "fake", Prefix: "gh", Env: api.TrackerEnvInherit}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	owner := h.as(h.owner, "harbor")

	w, err := orchclient.TrackerHome(h.ctx, owner, "gh")
	if err != nil {
		t.Fatal(err)
	}
	if w.Home.Kind != "service" || w.Home.Target != "fake" || len(w.Kinds) != 1 || w.Kinds[0] != orchapi.BackupExport {
		t.Fatalf("where: %+v", w)
	}
	for _, kind := range []string{orchapi.BackupSqlite, "dolt"} {
		if _, err := orchclient.AddTrackerBackup(h.ctx, owner, "gh", orchapi.AddBackupRequest{Target: "file://" + t.TempDir(), Kind: kind}); !isCode(err, api.CodeBadRequest) {
			t.Fatalf("a %s backup of a service tracker: %v", kind, err)
		}
	}
	for _, bad := range []string{"local", "ssh://ana@box", "server://127.0.0.1:3307/beads", "s3://b/x", "relative", "file:///Volumes/not-mounted-zm-test"} {
		if _, err := orchclient.AddTrackerBackup(h.ctx, owner, "gh", orchapi.AddBackupRequest{Target: bad}); err == nil {
			t.Fatalf("backup to %q accepted", bad)
		}
	}
	dir := t.TempDir()
	r, err := orchclient.AddTrackerBackup(h.ctx, owner, "gh", orchapi.AddBackupRequest{Target: "file://" + dir, Keep: 2, RunNow: true})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Ran || r.Backup.Kind != orchapi.BackupExport || r.Backup.Items != 2 || r.Backup.Path != filepath.Join(dir, "harbor-gh", "export") {
		t.Fatalf("export: %+v", r)
	}
	if _, err := orchclient.AddTrackerBackup(h.ctx, owner, "gh", orchapi.AddBackupRequest{Target: dir}); !isCode(err, api.CodeConflict) {
		t.Fatalf("the same backup twice: %v", err)
	}
	snaps := func() []string {
		des, _ := os.ReadDir(r.Backup.Path)
		var out []string
		for _, d := range des {
			out = append(out, d.Name())
		}
		return out
	}
	got := snaps()
	if len(got) != 1 || !strings.HasPrefix(got[0], "harbor-gh-") || !strings.HasSuffix(got[0], ".jsonl") {
		t.Fatalf("snapshots %v", got)
	}
	f, _ := os.Open(filepath.Join(r.Backup.Path, got[0]))
	sc := bufio.NewScanner(f)
	var lines []map[string]any
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, m)
	}
	f.Close()
	if len(lines) != 3 || lines[0]["zm_export"] != float64(1) || lines[1]["id"] != "gh:app-1" || lines[2]["status"] != "done" {
		t.Fatalf("snapshot lines %v", lines)
	}
	// Keep 2: a third run drops the oldest.
	for i := 0; i < 2; i++ {
		h.clock.add(time.Second)
		if _, err := orchclient.RunTrackerBackups(h.ctx, owner, "gh", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if got := snaps(); len(got) != 2 {
		t.Fatalf("keep 2: %v", got)
	}
	// A record with no keep (an older zm wrote some) keeps snapshots: Keep 0
	// used to delete every one, including the new one, and still report
	// success.
	setKeep := func(n int) {
		if _, err := h.s.updateHome(h.harbor.ID, "gh", func(st *homeState) error {
			for i := range st.Backups {
				st.Backups[i].Keep = n
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	setKeep(0)
	h.clock.add(time.Second)
	if _, err := orchclient.RunTrackerBackups(h.ctx, owner, "gh", "", ""); err != nil {
		t.Fatal(err)
	}
	if got := snaps(); len(got) != 3 {
		t.Fatalf("no keep on record: %v", got)
	}
	setKeep(2)
	// The scheduler runs a due backup and records it.
	h.clock.add(2 * time.Hour)
	h.s.backupDue(h.ctx)
	h.s.wg.Wait() // only the backups: the harness never calls Start, whose ticker would hold Wait
	bs, err := orchclient.TrackerBackups(h.ctx, owner, "gh")
	if err != nil || len(bs) != 1 || bs[0].LastOK == nil || bs[0].LastOK.Before(h.clock.now().Add(-time.Minute)) {
		t.Fatalf("scheduled run: %+v %v", bs, err)
	}
	// A busy tracker: a manual run is refused, the scheduler waits.
	release, _ := h.s.ops.acquire(h.harbor.ID, "gh", "a restore")
	if _, err := orchclient.RunTrackerBackups(h.ctx, owner, "gh", dir, ""); !isCode(err, api.CodeConflict) {
		t.Fatalf("run while busy: %v", err)
	}
	release()
	if v, err := orchclient.RemoveTrackerBackup(h.ctx, owner, "gh", dir, ""); err != nil || v.Kind != orchapi.BackupExport {
		t.Fatalf("rm: %+v %v", v, err)
	}
	if got := snaps(); len(got) != 2 {
		t.Fatalf("rm deleted copies: %v", got)
	}
}

// TestHomeBackupsPerTracker: each tracker's backups run in their own
// goroutine, so one tracker's long copy never holds up another's.
func TestHomeBackupsPerTracker(t *testing.T) {
	fakeHomes(t)
	h := newHarness(t)
	state := h.installFake()
	edit(t, state, func(st *fake.State) {
		st.Issues["app-1"] = &fake.Issue{ID: "app-1", Title: "one", Status: "open", UpdatedAt: fake.Now()}
	})
	trs := []api.Tracker{{Adapter: "fake", Prefix: "gh", Env: api.TrackerEnvInherit},
		{Adapter: "fake", Prefix: "gh2", Env: api.TrackerEnvInherit}}
	if _, err := h.d.Projects.Update(h.ctx, h.owner, "harbor", api.UpdateProjectRequest{Trackers: &trs}); err != nil {
		t.Fatal(err)
	}
	h.sync()
	owner := h.as(h.owner, "harbor")
	dir := t.TempDir()
	for _, p := range []string{"gh", "gh2"} {
		if _, err := orchclient.AddTrackerBackup(h.ctx, owner, p, orchapi.AddBackupRequest{Target: dir}); err != nil {
			t.Fatal(err)
		}
	}
	unblock := make(chan struct{})
	backupHook = func(prefix string) {
		if prefix == "gh" {
			<-unblock
		}
	}
	t.Cleanup(func() { backupHook = nil })
	h.s.backupDue(h.ctx)
	deadline := time.Now().Add(10 * time.Second)
	for {
		bs, _ := orchclient.TrackerBackups(h.ctx, owner, "gh2")
		if len(bs) == 1 && bs[0].LastOK != nil {
			break
		}
		if time.Now().After(deadline) {
			close(unblock)
			t.Fatal("gh2's backup waited for gh's")
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.s.backupDue(h.ctx) // gh's runner is still busy: no second one starts
	close(unblock)
	h.s.wg.Wait()
	if bs, _ := orchclient.TrackerBackups(h.ctx, owner, "gh"); len(bs) != 1 || bs[0].LastOK == nil {
		t.Fatalf("gh: %+v", bs)
	}
}
