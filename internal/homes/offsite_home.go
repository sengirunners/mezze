package homes

// Off-site home backups (SPEC 17.9 "Off-site home backups"): every few
// minutes the whole home (registry.db, each project's project.db, issues.db
// and project.toml, config.json, each tracker's home.json) is copied with
// VACUUM INTO, gzipped, and written as one set on an off-site target
// (internal/homes/offsite: S3 such as Cloudflare R2, or a directory), its
// manifest last. A file that has not changed since the last set is copied on
// the target instead of sent. Retention keeps the newest sets plus hourly and
// daily ones. Secrets (keys, tokens, passwords) never go: a restored home gets
// them again by hand.
//
// Every file of a set is encrypted with the home's backup key (bkcrypt,
// $ZM_HOME/secrets/backup.key, made when the backup is set or first runs):
// project.db holds secret names (grants, the secret ledger). The target's own
// encryption (--encrypt, rclone crypt) is a second layer, not a substitute.
// A home whose last set was plain (written before encryption) treats its first
// encrypted set like a redaction's: every older set is deleted once it is sent.
//
// A redaction reaches the sets the way it reaches sqlite copies: it marks the
// backup (ScrubDue), a run started after the mark writes a fresh set and then
// deletes every older one, and until that succeeds the mark stays and the
// redaction says it is not fully scrubbed.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/homes/bkcrypt"
	"github.com/sengirunners/mezze/internal/homes/home"
	"github.com/sengirunners/mezze/internal/homes/offsite"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/tools"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/sqlbackup"
	zmstore "github.com/sengirunners/potluck/kit/store"
)

const (
	offsiteFile         = "backup.json" // $ZM_HOME/backup.json
	offsiteDir          = "offsite"     // $ZM_HOME/offsite: staging and rclone's scratch
	defaultOffsiteEvery = 15 * time.Minute
	minOffsiteEvery     = time.Minute
	offsiteOp           = "the home's off-site backup"
	// nativeStoreFile is a project's native issue store beside its
	// project.db (SPEC 2). Only its existence is read here: the issues
	// module copies it (SnapshotNative), and would create one for a project
	// without.
	nativeStoreFile = "issues.db"
)

// offsiteState is $ZM_HOME/backup.json: the off-site backup and the
// watchdog heartbeat. It holds secret references, never secrets.
type offsiteState struct {
	Backup   *offsiteRec  `json:"backup,omitempty"`
	Watchdog *watchdogRec `json:"watchdog,omitempty"`
}

type offsiteRec struct {
	offsite.Config
	offsite.Policy
	Home      string            `json:"home"`
	Every     int64             `json:"every"` // seconds
	LastOK    *time.Time        `json:"last_ok,omitempty"`
	LastRun   *time.Time        `json:"last_run,omitempty"`
	Bytes     int64             `json:"bytes,omitempty"`
	Failures  int               `json:"failures,omitempty"`
	LastError string            `json:"last_error,omitempty"`
	ScrubDue  *time.Time        `json:"scrub_due,omitempty"`
	Last      *offsite.Manifest `json:"last,omitempty"` // the newest good set (names and hashes)
}

type watchdogRec struct {
	URL    string `json:"url"`
	Secret string `json:"secret"` // a reference
	Home   string `json:"home"`
}

// offsiteMu serialises backup.json's read-modify-write.
var offsiteMu sync.Mutex

func (s *Service) offsitePath() string { return filepath.Join(s.home(), offsiteFile) }

func (s *Service) loadOffsite() (offsiteState, error) {
	var st offsiteState
	b, err := os.ReadFile(s.offsitePath())
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return st, fmt.Errorf("%s: %v", s.offsitePath(), err)
	}
	return st, nil
}

func (s *Service) updateOffsite(fn func(*offsiteState) error) (offsiteState, error) {
	offsiteMu.Lock()
	defer offsiteMu.Unlock()
	st, err := s.loadOffsite()
	if err != nil {
		return st, err
	}
	if err := fn(&st); err != nil {
		return st, err
	}
	return st, home.WriteJSON(s.offsitePath(), st)
}

// HomeBackup is the off-site backup at a glance (nil: none set up), for
// GET /v0/health, a replica's sync and the heartbeat (core.BackupReporter).
func (s *Service) HomeBackup() *orchapi.HomeBackup {
	st, err := s.loadOffsite()
	if err != nil || st.Backup == nil {
		return nil
	}
	return st.Backup.brief()
}

func (b *offsiteRec) brief() *orchapi.HomeBackup {
	return &orchapi.HomeBackup{Kind: b.Kind(), EverySeconds: int(b.Every), LastOK: b.LastOK, Bytes: b.Bytes,
		Failing: b.Failures > 0, Failures: b.Failures, Error: b.LastError}
}

func (s *Service) offsiteView() (orchapi.OffsiteView, error) {
	st, err := s.loadOffsite()
	if err != nil {
		return orchapi.OffsiteView{}, err
	}
	v := orchapi.OffsiteView{Running: s.ops.running("", offsiteOp) != ""}
	if b := st.Backup; b != nil {
		v.Configured, v.Target, v.Home = true, b.Config.String(), b.Home
		v.AccessKey, v.SecretKey, v.Encrypt, v.EncryptSalt = b.AccessKey, b.SecretKey, b.Encrypt, b.EncryptSalt
		v.EverySeconds, v.Keep, v.KeepHourly, v.KeepDaily = int(b.Every), b.Keep, b.Hourly, b.Daily
		v.Status, v.LastRun, v.ScrubDue = b.brief(), b.LastRun, b.ScrubDue
		if b.Last != nil {
			v.Set = b.Last.Stamp
		}
	}
	if w := st.Watchdog; w != nil {
		v.Watchdog = s.hb.view(w)
	}
	return v, nil
}

// opener opens targets with the secrets brick and the pinned rclone (when
// installed: a plain file target needs none).
func (s *Service) opener() offsite.Opener {
	o := offsite.Opener{Secrets: secrets.Default(), Scratch: filepath.Join(s.home(), offsiteDir, "rclone")}
	if p, err := tools.Verify(s.home(), tools.Rclone); err == nil {
		o.Bin = p
	}
	return o
}

// setOffsite is PUT /v0/backup: it checks the target answers with these
// keys before it saves anything.
func (s *Service) setOffsite(ctx context.Context, req orchapi.OffsiteRequest) (orchapi.OffsiteView, error) {
	c := offsite.Config{URL: req.Target, Endpoint: req.Endpoint, Region: req.Region, AccessKey: req.AccessKey, SecretKey: req.SecretKey, Encrypt: req.Encrypt, EncryptSalt: req.EncryptSalt}
	if err := offsite.CheckHome(req.Home); err != nil {
		return orchapi.OffsiteView{}, err
	}
	every := time.Duration(req.EverySeconds) * time.Second
	if every == 0 {
		every = defaultOffsiteEvery
	}
	if every < minOffsiteEvery {
		return orchapi.OffsiteView{}, api.Errorf(api.CodeBadRequest, "--every is at least %s", minOffsiteEvery)
	}
	p := offsite.DefaultPolicy
	if req.Keep > 0 {
		p.Keep = req.Keep
	}
	if req.KeepHourly > 0 {
		p.Hourly = req.KeepHourly
	}
	if req.KeepDaily > 0 {
		p.Daily = req.KeepDaily
	}
	t, err := s.opener().Open(ctx, c)
	if err != nil {
		return orchapi.OffsiteView{}, err
	}
	// The key exists from now on, so it can be copied off the machine before
	// the first set needs it.
	if _, err := bkcrypt.Ensure(s.home()); err != nil {
		return orchapi.OffsiteView{}, fmt.Errorf("the backup key: %w", err)
	}
	if _, err := offsite.Sets(ctx, t, req.Home); err != nil {
		return orchapi.OffsiteView{}, api.Errorf(api.CodeBadRequest, "the target does not answer: %v", err)
	}
	if _, err := s.updateOffsite(func(st *offsiteState) error {
		rec := &offsiteRec{Config: c, Policy: p, Home: req.Home, Every: int64(every / time.Second)}
		if old := st.Backup; old != nil && old.URL == c.URL && old.Endpoint == c.Endpoint && old.Home == req.Home && old.Encrypt == c.Encrypt && old.EncryptSalt == c.EncryptSalt {
			// The same place: keep its history (and a redaction's mark).
			rec.LastOK, rec.LastRun, rec.Bytes, rec.Last, rec.ScrubDue = old.LastOK, old.LastRun, old.Bytes, old.Last, old.ScrubDue
		}
		st.Backup = rec
		return nil
	}); err != nil {
		return orchapi.OffsiteView{}, err
	}
	return s.offsiteView()
}

func (s *Service) offsiteOff() (orchapi.OffsiteView, error) {
	release, err := s.ops.acquire("", offsiteOp, "turning it off")
	if err != nil {
		return orchapi.OffsiteView{}, err
	}
	defer release()
	v, err := s.offsiteView()
	if err != nil {
		return v, err
	}
	if !v.Configured {
		return v, api.Errorf(api.CodeNotFound, "no off-site backup is set up")
	}
	_, err = s.updateOffsite(func(st *offsiteState) error { st.Backup = nil; return nil })
	return v, err
}

func (s *Service) offsiteSets(ctx context.Context) (orchapi.OffsiteSets, error) {
	st, err := s.loadOffsite()
	if err != nil {
		return orchapi.OffsiteSets{}, err
	}
	b := st.Backup
	if b == nil {
		return orchapi.OffsiteSets{}, api.Errorf(api.CodeNotFound, "no off-site backup is set up").WithHint("zm backup set")
	}
	t, err := s.opener().Open(ctx, b.Config)
	if err != nil {
		return orchapi.OffsiteSets{}, err
	}
	sets, err := offsite.Sets(ctx, t, b.Home)
	if err != nil {
		return orchapi.OffsiteSets{}, api.Errorf(api.CodeUnavailable, "%v", err)
	}
	out := orchapi.OffsiteSets{Target: b.Config.String(), Home: b.Home, Sets: []orchapi.OffsiteSet{}}
	for _, x := range sets {
		out.Sets = append(out.Sets, orchapi.OffsiteSet{Stamp: x.Stamp, Complete: x.Complete, Bytes: x.Stored, Objects: x.Objects})
	}
	return out, nil
}

// runOffsite takes and sends one set, then applies retention (or, for a
// redaction's mark, deletes every older set). The outcome is recorded; the
// error is also returned.
func (s *Service) runOffsite(ctx context.Context) (orchapi.OffsiteRunResult, error) {
	release, err := s.ops.acquire("", offsiteOp, "a backup")
	if err != nil {
		return orchapi.OffsiteRunResult{}, err
	}
	defer release()
	st, err := s.loadOffsite()
	if err != nil {
		return orchapi.OffsiteRunResult{}, err
	}
	b := st.Backup
	if b == nil {
		return orchapi.OffsiteRunResult{}, api.Errorf(api.CodeNotFound, "no off-site backup is set up").WithHint("zm backup set")
	}
	start := s.now().UTC()
	res, m, err := s.sendSet(ctx, b, start)
	res.Seconds = s.now().Sub(start).Seconds()
	if err != nil {
		res.Error = err.Error()
	}
	if _, uerr := s.updateOffsite(func(cur *offsiteState) error {
		r := cur.Backup
		if r == nil || r.URL != b.URL || r.Home != b.Home {
			return nil // turned off or moved meanwhile
		}
		r.LastRun = &start
		if err != nil {
			r.Failures++
			r.LastError = clip(err.Error(), 300)
			return nil
		}
		r.Failures, r.LastError = 0, ""
		r.LastOK, r.Bytes, r.Last = &start, m.Stored, &m
		if r.ScrubDue != nil && !start.Before(*r.ScrubDue) && res.scrubbed {
			r.ScrubDue = nil
		}
		return nil
	}); uerr != nil && err == nil {
		err = uerr
	}
	if err != nil {
		s.warn("off-site backup failed", "err", err)
	}
	return res.OffsiteRunResult, err
}

type runResult struct {
	orchapi.OffsiteRunResult
	scrubbed bool // every set older than this one is gone
}

func (s *Service) sendSet(ctx context.Context, b *offsiteRec, start time.Time) (runResult, offsite.Manifest, error) {
	var res runResult
	t, err := s.opener().Open(ctx, b.Config)
	if err != nil {
		return res, offsite.Manifest{}, err
	}
	key, err := bkcrypt.Ensure(s.home())
	if err != nil {
		return res, offsite.Manifest{}, fmt.Errorf("the backup key: %w", err)
	}
	stamp := start.Format(offsite.StampLayout)
	if b.Last != nil && b.Last.Stamp >= stamp {
		return res, offsite.Manifest{}, fmt.Errorf("the last set (%s) is not older than now (%s): is the clock right?", b.Last.Stamp, stamp)
	}
	stage := filepath.Join(s.home(), offsiteDir, "staging")
	if err := os.RemoveAll(stage); err != nil { // a crash's leftover holds what a set holds
		return res, offsite.Manifest{}, err
	}
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return res, offsite.Manifest{}, err
	}
	defer os.RemoveAll(stage)
	s.snapping.Store(true)
	files, err := s.stageHome(ctx, stage, b.Last)
	s.snapping.Store(false)
	if err != nil {
		return res, offsite.Manifest{}, err
	}
	m, err := offsite.Upload(ctx, t, offsite.Manifest{Home: b.Home, Stamp: stamp, Created: start, Version: api.Version, Commit: api.Commit}, files, b.Last, key)
	if err != nil {
		return res, m, err
	}
	res.Ran, res.Set, res.Files, res.Bytes = true, stamp, len(m.Files), m.Stored
	// Upload carries a file over only from a set sealed the same way.
	same := b.Last != nil && b.Last.Cipher == m.Cipher && b.Last.KeyID == m.KeyID
	for _, f := range m.Files {
		if old := lastFile(b.Last, f.Path); same && old != nil && old.SHA256 == f.SHA256 {
			res.Copied++
		} else {
			res.Uploaded++
		}
	}
	sets, err := offsite.Sets(ctx, t, b.Home)
	if err != nil {
		return res, m, fmt.Errorf("listing sets after the upload: %w", err)
	}
	// The sets before this one may hold the text a redaction removed, or be
	// plain: the last good set was written before encryption, and the older
	// ones with it. A failure keeps b.Last as it was, so the next run retries.
	redaction := b.ScrubDue != nil && !start.Before(*b.ScrubDue)
	scrub := redaction || (b.Last != nil && b.Last.Cipher == "")
	var drop []string
	if scrub {
		for _, x := range sets {
			if x.Stamp != stamp {
				drop = append(drop, x.Stamp)
			}
		}
	} else {
		drop = offsite.Retain(sets, stamp, start, b.Policy)
	}
	var errs []error
	for _, d := range drop {
		if err := offsite.DeleteSet(ctx, t, b.Home, d); err != nil {
			errs = append(errs, fmt.Errorf("deleting set %s: %w", d, err))
			continue
		}
		res.Deleted = append(res.Deleted, d)
	}
	if err := errors.Join(errs...); err != nil {
		if redaction {
			return res, m, fmt.Errorf("a redaction's older sets remain: %w", err)
		}
		if scrub {
			return res, m, fmt.Errorf("older unencrypted sets remain: %w", err)
		}
		s.warn("off-site backup: retention", "err", err) // the new set is good
	}
	res.scrubbed = scrub
	return res, m, nil
}

func lastFile(m *offsite.Manifest, path string) *offsite.File {
	if m == nil {
		return nil
	}
	for i := range m.Files {
		if m.Files[i].Path == path {
			return &m.Files[i]
		}
	}
	return nil
}

// scrubStagedRegistry blanks every module's secrets (webhook and GitHub-link
// signing keys, core.BackupScrubber) in the staged registry copy at path,
// then rewrites it so no free page keeps them.
func (s *Service) scrubStagedRegistry(ctx context.Context, path string) error {
	fs := s.d.BackupScrubbers()
	if len(fs) == 0 {
		return nil
	}
	db, err := zmstore.OpenFile(ctx, path, 1, copyBusy, false)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, f := range fs {
		if err := f(ctx, tx); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `VACUUM`); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	return nil
}

// stageHome writes into stage a consistent copy of each file a restore
// needs: databases with VACUUM INTO (checked), config without inline tunnel
// trust, the rest copied.
func (s *Service) stageHome(ctx context.Context, stage string, last *offsite.Manifest) ([]offsite.Staged, error) {
	root := s.home()
	var out []offsite.Staged
	put := func(rel string) string {
		p := filepath.Join(stage, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o700)
		out = append(out, offsite.Staged{Path: rel, Local: p})
		return p
	}
	snapFile := func(rel string) error {
		src := filepath.Join(root, filepath.FromSlash(rel))
		db, err := zmstore.OpenFile(ctx, src, 1, copyBusy, false)
		if err != nil {
			return err
		}
		defer db.Close()
		_, err = sqlbackup.Snapshot(ctx, db, put(rel), 0)
		return err
	}
	copyPlain := func(rel string) error {
		src := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Stat(src); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return sqlbackup.Copy(ctx, src, put(rel))
	}
	if err := snapFile(zmstore.FileRegistry); err != nil {
		return nil, fmt.Errorf("registry.db: %w", err)
	}
	if err := s.scrubStagedRegistry(ctx, filepath.Join(stage, zmstore.FileRegistry)); err != nil {
		return nil, fmt.Errorf("registry.db: blank its secrets: %w", err)
	}
	cfg, err := backupConfig(filepath.Join(root, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	if cfg != nil {
		if err := home.WriteJSON(put("config.json"), cfg); err != nil {
			return nil, fmt.Errorf("config.json: %w", err)
		}
	}
	for _, p := range s.d.Store.Projects(authz.All(), authz.Read) {
		dir := zmstore.DirProjects + "/" + p.ID
		if err := snapFile(dir + "/" + zmstore.FileProject); err != nil {
			return nil, fmt.Errorf("%s project.db: %w", s.slugOf(p.ID), err)
		}
		if err := copyPlain(dir + "/project.toml"); err != nil {
			return nil, fmt.Errorf("%s project.toml: %w", s.slugOf(p.ID), err)
		}
		rel := dir + "/" + nativeStoreFile
		if !pathExists(filepath.Join(root, filepath.FromSlash(rel))) {
			continue
		}
		if orchcore.Issues(s.d) == nil {
			return nil, api.Errorf(api.CodeUnavailable, "native issues are not available in this daemon")
		}
		var prev int64
		if f := lastFile(last, rel); f != nil {
			prev = f.Bytes
		}
		if _, err := orchcore.Issues(s.d).SnapshotNative(ctx, p.ID, put(rel), prev); err != nil {
			return nil, fmt.Errorf("%s issues.db: %w", s.slugOf(p.ID), err)
		}
	}
	trackers := filepath.Join(root, "trackers")
	err = filepath.WalkDir(trackers, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if de.IsDir() || de.Name() != "home.json" {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if strings.Count(filepath.ToSlash(rel), "/") != 3 { // trackers/<project>/<prefix>/home.json
			return nil
		}
		return copyPlain(filepath.ToSlash(rel))
	})
	return out, err
}

// backupConfig keeps unknown settings and secret references, but never the
// inline edge trust secret. A listener that depended on it stays off after
// restore until its trust is reconfigured. Errors never include config text.
func backupConfig(path string) (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cfg map[string]json.RawMessage
	if json.Unmarshal(b, &cfg) != nil || cfg == nil {
		return nil, errors.New("expected a JSON object")
	}
	// The struct config loader merges repeated tunnel objects, unlike this
	// map. Refuse ambiguity rather than lose a secret from an earlier object.
	if !uniqueConfigField(b, "tunnel") {
		return nil, errors.New("repeated tunnel settings")
	}
	for key, raw := range cfg {
		if !strings.EqualFold(key, "tunnel") {
			continue
		}
		var tunnel map[string]json.RawMessage
		if json.Unmarshal(raw, &tunnel) != nil {
			return nil, errors.New("invalid tunnel settings")
		}
		if tunnel == nil { // a single null tunnel has no settings
			continue
		}
		// A null string field leaves the struct's previous value unchanged;
		// repeated edge_secret fields cannot safely be collapsed either.
		if !uniqueConfigField(raw, "edge_secret") {
			return nil, errors.New("repeated tunnel edge secret")
		}
		disable := false
		for field, raw := range tunnel {
			if !strings.EqualFold(field, "edge_secret") {
				continue
			}
			var secret string
			if json.Unmarshal(raw, &secret) != nil {
				return nil, errors.New("invalid tunnel edge secret")
			}
			delete(tunnel, field)
			disable = disable || secret != ""
		}
		if disable {
			for field := range tunnel {
				if strings.EqualFold(field, "listen") {
					delete(tunnel, field)
				}
			}
		}
		cfg[key], err = json.Marshal(tunnel)
		if err != nil {
			return nil, errors.New("invalid tunnel settings")
		}
	}
	return cfg, nil
}

// uniqueConfigField checks a previously validated JSON object's keys before
// a RawMessage map can collapse repeated security-sensitive fields. Matching
// follows the config struct decoder's case-insensitive field names.
func uniqueConfigField(b []byte, field string) bool {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	seen := false
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		key, ok := tok.(string)
		if !ok {
			return false
		}
		var raw json.RawMessage
		if dec.Decode(&raw) != nil {
			return false
		}
		if strings.EqualFold(key, field) {
			if seen {
				return false
			}
			seen = true
		}
	}
	return true
}

// offsiteDue: never run, the period passed since the last run (10 minutes
// after a failure, at most the period), or a redaction waits.
func (s *Service) offsiteDue(b *offsiteRec) bool {
	if b.LastRun == nil {
		return true
	}
	if b.ScrubDue != nil && b.Failures == 0 {
		return true // a redaction's run that has not failed yet
	}
	every := time.Duration(b.Every) * time.Second
	if b.Failures > 0 {
		every = min(every, retryAfterFailure)
	}
	return !s.now().Before(b.LastRun.Add(every))
}

// offsiteLoop runs the off-site backup when due until ctx ends.
func (s *Service) offsiteLoop(ctx context.Context) {
	t := time.NewTicker(backupTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		st, err := s.loadOffsite()
		if err != nil || st.Backup == nil || !s.offsiteDue(st.Backup) || s.ops.running("", offsiteOp) != "" {
			continue
		}
		_, _ = s.runOffsite(ctx) // recorded
	}
}

// purgeOffsite is a redaction's step for the off-site sets (PurgeBackups):
// fresh marks the backup and runs it now; otherwise only a mark already
// waiting runs.
func (s *Service) purgeOffsite(ctx context.Context, fresh bool) error {
	st, err := s.loadOffsite()
	if err != nil || st.Backup == nil {
		return err
	}
	if fresh {
		now := s.now().UTC()
		if st, err = s.updateOffsite(func(st *offsiteState) error {
			if st.Backup != nil {
				st.Backup.ScrubDue = &now
			}
			return nil
		}); err != nil || st.Backup == nil {
			return err
		}
	}
	if st.Backup.ScrubDue == nil {
		return nil
	}
	if _, err := s.runOffsite(ctx); err != nil {
		return fmt.Errorf("the off-site backup: %w", err)
	}
	return nil
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
