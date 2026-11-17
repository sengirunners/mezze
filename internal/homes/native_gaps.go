package homes

// A retired prefix has no live schedule that can prove its copies were
// replaced. Report those copies to redaction rather than silently calling
// the purge complete, or deleting the only surviving backup.

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

func (s *Service) nativeBackupGaps(pid, slug string, trackers []api.Tracker) error {
	prefixes := map[string]bool{}
	for _, tr := range trackers {
		prefixes[tr.Prefix] = true
	}
	// A config edit can leave home.json under the old prefix, including a
	// target no current tracker uses. It is still a known backup location.
	root := filepath.Dir(orchcore.Trackers(s.d).TrackerDir(pid, "x"))
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		if e.IsDir() && orchapi.ValidTrackerPrefix(e.Name()) {
			prefixes[e.Name()] = true
		}
	}
	// Another slug can extend this one or this one it: "harbor-ios-issue" is
	// project harbor-ios's folder (prefix "issue"), or harbor's (prefix
	// "ios-issue"). It is the other project's only when that project is known
	// to use its prefix (the test this one gets above) and this one is not.
	// Otherwise the copy is reported: unproven ownership fails closed.
	type sibling struct {
		id, stem string
		uses     map[string]bool
	}
	var others []sibling
	for _, p := range s.d.Store.Projects(authz.All(), authz.Read) {
		o, ok := s.d.Projects.File(p.ID)
		if !ok || p.ID == pid || !strings.HasPrefix(o.Slug, slug+"-") && !strings.HasPrefix(slug, o.Slug+"-") {
			continue
		}
		uses := map[string]bool{}
		for _, tr := range orchcore.ProjectTrackers(o) {
			uses[tr.Prefix] = true
		}
		others = append(others, sibling{p.ID, o.Slug + "-", uses})
	}
	theirs := func(name string) bool {
		return slices.ContainsFunc(others, func(o sibling) bool {
			rest, ok := strings.CutPrefix(name, o.stem)
			if !ok || !orchapi.ValidTrackerPrefix(rest) {
				return false
			}
			if o.uses[rest] {
				return true
			}
			fi, err := os.Stat(orchcore.Trackers(s.d).TrackerDir(o.id, rest))
			return err == nil && fi.IsDir()
		})
	}
	bases := map[string]bool{}
	managed := map[string]bool{}
	var errs []error
	for prefix := range prefixes {
		st, err := s.loadHome(pid, prefix)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, b := range st.Backups {
			if b.Kind != orchapi.BackupSqlite {
				continue
			}
			bases[b.Base] = true
			if tr, ok := orchapi.FindTracker(trackers, prefix); ok && tr.Adapter == orchapi.AdapterNative {
				managed[backupDir(b.Base, slug, prefix, b.Kind)] = true
			}
		}
	}
	for base := range bases {
		dirs, err := os.ReadDir(base)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, d := range dirs {
			prefix, ok := strings.CutPrefix(d.Name(), slug+"-")
			if !d.IsDir() || !ok || !orchapi.ValidTrackerPrefix(prefix) || !prefixes[prefix] && theirs(d.Name()) {
				continue
			}
			dir := backupDir(base, slug, prefix, orchapi.BackupSqlite)
			copies, err := os.ReadDir(dir)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				errs = append(errs, err)
				continue
			}
			for _, c := range copies {
				name := strings.TrimSuffix(c.Name(), ".tmp")
				stem := name
				if i := strings.LastIndexByte(name, '-'); i >= 0 {
					stem = name[:i+1]
				}
				if !strings.HasPrefix(name, slug+"-") || managed[dir] && stem == slug+"-"+prefix+"-" {
					continue
				}
				for _, suf := range []string{copyIssues, copyProject, plainIssues, plainProject} {
					if strings.HasSuffix(name, suf) {
						errs = append(errs, &orchcore.UnreplacedCopy{Path: filepath.Join(dir, c.Name())})
						break
					}
				}
			}
		}
	}
	return errors.Join(errs...)
}
