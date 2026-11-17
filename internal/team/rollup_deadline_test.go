package team_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A cursorless changes adapter is fully listed each time. Deadlines can
// change without updated moving; only work.changed can update an existing
// replica after its join snapshot. No provider or native issue writes occur.
func TestRollupTimestampStableDeadlinesReachCopy(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	script := `#!/bin/sh
IFS= read -r req || :
case "$req" in
 *'"verb":"describe"'*) echo '{"ok":true,"result":{"zm":"zm/v0","name":"deadline","prefix":"bd","verbs":["describe","list","get","claim","release","comment","set_status","changes"]}}' ;;
 *) exec cat "$(dirname "$0")/response.json" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "zm-work-deadline"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	stamp := time.Now().UTC().Truncate(time.Second)
	write := func(extra map[string]any) {
		t.Helper()
		response := map[string]any{"ok": true, "result": orchapi.AdapterChanges{Items: []orchapi.WorkItem{{ID: "bd:a", Title: "deadline task", Status: orchapi.WorkOpen, Updated: stamp, Extra: extra}}}}
		b, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		tmp := filepath.Join(dir, "next.json")
		if err := os.WriteFile(tmp, b, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(dir, "response.json")); err != nil {
			t.Fatal(err)
		}
	}
	write(nil)
	tn := tailnettest.New()
	a := authority(t, tn)
	a.cfg.SyncPause = 50 * time.Millisecond
	trackers := []api.Tracker{{Adapter: "deadline", Prefix: "bd"}}
	mustf(a.owner.UpdateProject(ctx, "harbor", api.UpdateProjectRequest{Trackers: &trackers}))(t)
	check := func(c *client.Client, field, value string) error {
		l, err := orchclient.WorkRollup(ctx, c, "harbor", "")
		if err != nil {
			return err
		}
		r := rollupOf(t, l, "harbor")
		if r.Totals.Total != 1 {
			return fmt.Errorf("totals %+v; sources %+v", r.Totals, mustf(orchclient.WorkSources(ctx, a.owner, "harbor"))(t))
		}
		if value == "" {
			if len(r.Deadlines) != 0 {
				return fmt.Errorf("stale deadlines %+v", r.Deadlines)
			}
		} else {
			if len(r.Deadlines) != 1 {
				return fmt.Errorf("deadlines %+v", r.Deadlines)
			}
			d := r.Deadlines[0]
			got := d.DueAt
			if field == "defer_until" {
				got = d.ReturnsAt
			}
			if got == nil || got.Format(time.RFC3339) != value {
				return fmt.Errorf("deadline %+v, want %s=%s", d, field, value)
			}
		}
		return nil
	}
	eventually(t, 10*time.Second, func() error { return check(a.owner, "", "") })
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	eventually(t, 10*time.Second, func() error { return check(r.owner, "", "") })
	for _, field := range []string{"due_at", "defer_until"} {
		for _, hours := range []int{48, 72, 0} {
			t.Run(fmt.Sprintf("%s/%d", field, hours), func(t *testing.T) {
				value := ""
				var extra map[string]any
				if hours != 0 {
					value = stamp.Add(time.Duration(hours) * time.Hour).Format(time.RFC3339)
					extra = map[string]any{field: value}
				}
				write(extra)
				eventually(t, 10*time.Second, func() error { return check(a.owner, field, value) })
				eventually(t, 10*time.Second, func() error { return check(r.owner, field, value) })
				item := mustf(orchclient.GetWork(ctx, at(r.owner, "harbor", ""), "bd:a"))(t)
				if !item.Updated.Equal(stamp) {
					t.Fatalf("updated moved: %s != %s", item.Updated, stamp)
				}
			})
		}
	}
}
