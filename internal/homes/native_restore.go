package homes

// zm issues restore (SPEC 17.9): a sqlite backup's copy becomes a project's
// native store. The disaster path, and the only way native issues go back in
// time, so it is careful in this order: pick and check the copy in staging;
// let the issues module work on that copy (refusing a newer schema or another
// project's store, applying the redaction list); only then hold writes, copy
// the current store into the backup's directory (so a restore can be undone
// by another, and that copy has a scrubber like every other), and swap.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sengirunners/mezze/internal/homes/bkcrypt"
	"github.com/sengirunners/mezze/internal/homes/home"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/sqlbackup"
)

func (s *Service) restoreNative(ctx context.Context, req orchapi.IssueRestoreRequest) (orchapi.IssueRestoreResult, error) {
	var res orchapi.IssueRestoreResult
	pid, slug, f, err := s.d.TrackerTarget(ctx)
	if err != nil {
		return res, err
	}
	if orchcore.Issues(s.d) == nil {
		return res, api.Errorf(api.CodeUnavailable, "native issues are not available in this daemon")
	}
	tr, err := nativeTracker(orchcore.ProjectTrackers(f), req.Prefix)
	if err != nil {
		return res, err
	}
	ctx = homeCtx(ctx) // a Ctrl-C does not cut a restore short
	release, err := s.ops.acquire(pid, tr.Prefix, "a restore")
	if err != nil {
		return res, err
	}
	defer release()
	st, err := s.loadHome(pid, tr.Prefix)
	if err != nil {
		return res, err
	}
	src, err := s.pickCopy(st, slug, tr.Prefix, req)
	if err != nil {
		return res, err
	}
	s.sweepScratch(pid, tr.Prefix)
	scratch := s.localScratch(pid, tr.Prefix, "restore-"+strings.ToLower(ids.New()))
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		return res, err
	}
	defer removeAllQuiet(scratch)
	staged := filepath.Join(scratch, "issues.db")
	err = s.stageCopy(ctx, src, staged)
	if err == nil {
		_, err = sqlbackup.Check(ctx, staged)
	}
	if err != nil {
		return res, api.Errorf(api.CodeBadRequest, "the copy %s cannot be restored: %v; nothing changed", src, err).
			WithHint("--at picks an older copy")
	}
	// The refusals come before anything changes; this also proves the copy
	// restores, which is all a dry run is for.
	res, err = orchcore.Issues(s.d).RestoreNative(ctx, pid, staged, true)
	res.Copy = src
	if err != nil || req.DryRun {
		return res, err
	}
	hold, err := orchcore.Issues(s.d).HoldWrites(pid, "a restore of tracker "+tr.Prefix+" runs")
	if err != nil {
		return res, err
	}
	defer hold()
	kept, err := s.keepCurrent(ctx, pid, slug, tr.Prefix, st)
	if err != nil {
		if !req.Force {
			return res, api.Errorf(api.CodeConflict, "the current store could not be copied first: %v; nothing changed", err).
				WithHint("--force restores without that copy (a store too damaged to copy, say)")
		}
		res.Notes = append(res.Notes, "the store it replaced was not kept: "+err.Error())
	}
	res.DryRun, res.Kept = false, kept
	done, err := orchcore.Issues(s.d).RestoreNative(ctx, pid, staged, false)
	if err != nil {
		return res, withKept(err, kept)
	}
	res.Counts = done.Counts
	res.Redactions += done.Redactions // a redaction that landed since the dry pass
	// The next copy is compared with this store, not the one it replaced.
	if _, err := s.updateHome(pid, tr.Prefix, func(st *homeState) error {
		for i := range st.Backups {
			if st.Backups[i].Kind == orchapi.BackupSqlite {
				st.Backups[i].StoreBytes = 0
			}
		}
		return nil
	}); err != nil {
		res.Notes = append(res.Notes, "the backup records were not updated: "+err.Error())
	}
	if err := orchcore.Trackers(s.d).Relist(ctx, pid, tr); err != nil {
		res.Notes = append(res.Notes, "the mirror did not list the restored issues again: "+err.Error()+"; restore again, or zm work shows the old ones")
	}
	res.Notes = append(res.Notes,
		"project.db was not restored: mail, leases and local work stay as they are, and a lease may name an issue the store has rewound",
		"open web pages show the old issues until they reload")
	return res, nil
}

// stageCopy puts the copy src at staged: decrypted with the home's backup
// key, or copied as it is if an older zm wrote it plain.
func (s *Service) stageCopy(ctx context.Context, src, staged string) error {
	sealed, err := bkcrypt.Sealed(src)
	if err != nil {
		return err
	}
	if !sealed {
		return sqlbackup.Copy(ctx, src, staged)
	}
	key, err := bkcrypt.Open(s.home())
	if err != nil {
		return err
	}
	_, _, err = bkcrypt.DecryptFile(ctx, src, staged, key)
	return err
}

// withKept adds where the current store went to the error of a swap that
// failed: an error answer carries no result, and it is the one thing the
// caller must not lose.
func withKept(err error, kept string) error {
	if kept == "" {
		return err
	}
	e := &api.Error{Code: api.CodeUnavailable, Message: err.Error()}
	if ae, ok := errors.AsType[*api.Error](err); ok {
		c := *ae
		e = &c
	}
	e.Message += "; the store was copied first, to " + kept
	return e
}

// nativeTracker is the native tracker prefix names, or the project's only one.
func nativeTracker(trs []api.Tracker, prefix string) (api.Tracker, error) {
	var hits []api.Tracker
	for _, tr := range trs {
		if tr.Adapter == orchapi.AdapterNative && (prefix == "" || tr.Prefix == prefix) {
			hits = append(hits, tr)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		if prefix != "" {
			return api.Tracker{}, api.NotFound("native tracker", prefix)
		}
		return api.Tracker{}, api.Errorf(api.CodeBadRequest, "this project has no native tracker").WithHint("zm tracker add native")
	}
	return api.Tracker{}, api.Errorf(api.CodeBadRequest, "this project has %d native trackers; name one", len(hits))
}

// pickCopy finds the copy req names: the newest at or matching req.At, of one
// backup, of a path, or of every sqlite backup of the tracker.
func (s *Service) pickCopy(st homeState, slug, prefix string, req orchapi.IssueRestoreRequest) (string, error) {
	var dirs []string
	if req.From == "" {
		for _, b := range st.Backups {
			if b.Kind == orchapi.BackupSqlite {
				dirs = append(dirs, backupDir(b.Base, slug, prefix, b.Kind))
			}
		}
		if len(dirs) == 0 {
			return "", api.Errorf(api.CodeBadRequest, "tracker %s has no sqlite backup", prefix).WithHint("--from file:///path names a copy")
		}
	} else if b, err := s.findBackup(st, req.From, orchapi.BackupSqlite); err == nil {
		dirs = []string{backupDir(b.Base, slug, prefix, b.Kind)}
	} else {
		uh, _ := userHomeDir()
		t, err := home.Parse(req.From, uh)
		if err != nil {
			return "", api.Errorf(api.CodeBadRequest, "--from %s: want a backup id, file:///path or an absolute path", req.From)
		}
		fi, err := os.Stat(t.Path)
		if err != nil {
			return "", api.Errorf(api.CodeBadRequest, "--from: %v", err)
		}
		if !fi.IsDir() {
			return t.Path, nil
		}
		dirs = []string{t.Path, backupDir(t.Path, slug, prefix, orchapi.BackupSqlite)}
	}
	best, bestName := "", ""
	for _, dir := range dirs {
		names, err := sqlbackup.List(dir, "", copyIssues)
		if err != nil {
			return "", api.Errorf(api.CodeUnavailable, "%v", err)
		}
		old, err := sqlbackup.List(dir, "", plainIssues) // an older zm's, unencrypted
		if err != nil {
			return "", api.Errorf(api.CodeUnavailable, "%v", err)
		}
		for _, n := range append(names, old...) {
			if strings.Contains(n, req.At) && n > bestName {
				best, bestName = filepath.Join(dir, n), n
			}
		}
	}
	if best == "" {
		what := "no copy"
		if req.At != "" {
			what = "no copy matching " + req.At
		}
		return "", api.Errorf(api.CodeBadRequest, "%s in %s", what, strings.Join(dirs, ", "))
	}
	return best, nil
}

// keepCurrent copies the store a restore is about to replace next to the
// tracker's other copies. A store with no history has nothing to keep.
func (s *Service) keepCurrent(ctx context.Context, pid, slug, prefix string, st homeState) (string, error) {
	v, err := orchcore.Issues(s.d).NativeVersion(ctx, pid)
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", nil
	}
	var pick *backupRec
	for i := range st.Backups {
		b := &st.Backups[i]
		if b.Kind == orchapi.BackupSqlite {
			pick = b
			break
		}
	}
	if pick == nil {
		return "", errors.New("the tracker has no sqlite backup to keep it in")
	}
	if err := backupBase(home.KindFile, pick.Base); err != nil {
		return "", err
	}
	dir := backupDir(pick.Base, slug, prefix, orchapi.BackupSqlite)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := copyName(dir, slug+"-"+prefix+"-", s.now())
	if _, _, err := s.writeCopies(ctx, pid, prefix, dir, name, 0); err != nil {
		return "", fmt.Errorf("writing the copy: %w", err)
	}
	return filepath.Join(dir, name+copyIssues), nil
}
