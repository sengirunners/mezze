package work

// The work module's side of the homes seam (orchcore.TrackerHost, SPEC 17.9):
// what internal/homes asks of the tracker registry and the mirror. The
// export backup is here because it is a snapshot of the mirror; home_redact.go
// scrubs those snapshots.

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

var _ orchcore.TrackerHost = (*Service)(nil)

// TrackerDir is orchcore.TrackerHost's trackerDir.
func (s *Service) TrackerDir(project, prefix string) string { return s.trackerDir(project, prefix) }

// TrackerView is orchcore.TrackerHost's trackerView.
func (s *Service) TrackerView(ctx context.Context, pid, slug string, tr api.Tracker) orchapi.TrackerView {
	return s.trackerView(ctx, pid, slug, tr)
}

// Describe is tr's adapter's describe answer.
func (s *Service) Describe(ctx context.Context, pid string, tr api.Tracker) (orchapi.AdapterDescribe, error) {
	t, err := s.tracker(ctx, pid, tr)
	return t.desc, err
}

// Relist drops tr's mirror and lists it afresh: its source went back in time,
// and the mirror keeps the newest of every row. The listing is announced to
// the copies, and that is owed on disk from the wipe on, so a restart before
// it ends still announces.
func (s *Service) Relist(ctx context.Context, pid string, tr api.Tracker) error {
	if err := s.m.forgetAnnounce(ctx, pid, tr.Prefix, orchapi.TrackerBinding(tr)); err != nil {
		return err
	}
	sy := s.m.syncer(pid, tr)
	sy.mu.Lock()
	sy.row.announce, sy.again = true, true
	sy.mu.Unlock()
	sy.kick()
	return nil
}

// Export writes the tracker's items (every status, with zm's comments) from
// the mirror as one JSON Lines file in dir, and returns how many and the
// file's path. at names the file and is when the mirror's state is read, so
// one call has one clock. A mirror still on its first sync is refused (a
// snapshot must be whole); a stale one is written and says so in its first
// line.
func (s *Service) Export(ctx context.Context, pid, slug string, tr api.Tracker, dir string, at time.Time) (int, string, error) {
	v := s.m.syncer(pid, tr).view(at)
	if v.state == orchapi.SyncLoading {
		return 0, "", api.Errorf(api.CodeUnavailable, "the mirror of %s has not finished its first sync; a snapshot must be whole", tr.Prefix)
	}
	db, err := s.d.Store.Project(authz.All(), authz.Read, pid)
	if err != nil {
		return 0, "", err
	}
	var items []orchapi.WorkItem
	for _, status := range []orchapi.WorkStatus{"", orchapi.WorkDone} {
		l, err := mirrorList(ctx, db, authz.All(), pid, mirrorQuery{source: tr.Prefix, status: status, limit: 1 << 30})
		if err != nil {
			return 0, "", err
		}
		items = append(items, l...)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, "", err
	}
	path := filepath.Join(dir, slug+"-"+tr.Prefix+"-"+at.UTC().Format("20060102T150405Z")+".jsonl")
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, "", err
	}
	w := bufio.NewWriterSize(f, 256<<10)
	enc := json.NewEncoder(w)
	head := map[string]any{"zm_export": 1, "project": slug, "tracker": tr.Prefix, "adapter": tr.Adapter, "items": len(items),
		"synced": v.synced, "state": v.state}
	err = enc.Encode(head)
	for i := 0; err == nil && i < len(items); i++ {
		it := items[i]
		if it.Comments, err = listComments(ctx, db, it.ID); err != nil {
			break
		}
		err = enc.Encode(it)
	}
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return 0, "", err
	}
	return len(items), path, nil
}
