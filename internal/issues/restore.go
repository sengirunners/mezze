package issues

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/sqlbackup"
)

// The store's side of a sqlite backup (SPEC 17.9). homes owns the schedule,
// the copies and the home lock; this module owns the file. A restore finishes
// its work on the copy before the live store is touched: the redaction list
// is applied there, so a redacted text is never served, and a failure leaves
// the store as it was.

func (s *Service) restorer() (istore.Restorer, error) {
	r, ok := s.st.(istore.Restorer)
	if !ok {
		return nil, api.Errorf(api.CodeNotImplemented, "this issue store has no file to copy")
	}
	return r, nil
}

// SnapshotNative implements core.IssueService.
func (s *Service) SnapshotNative(ctx context.Context, project, dst string, prev int64) (int64, error) {
	r, err := s.restorer()
	if err != nil {
		return 0, err
	}
	var n int64
	err = r.Copy(ctx, project, func(ctx context.Context, db *sql.DB) error {
		info, err := sqlbackup.Snapshot(ctx, db, dst, prev)
		n = info.Bytes
		return err
	})
	return n, err
}

// importState (store meta) was "running" while the retired beads import
// wrote a store and "done" after it verified. A copy taken
// mid-import is not whole: a restore refuses it.
const importState = "import_state"

// RestoreNative implements core.IssueService.
func (s *Service) RestoreNative(ctx context.Context, project, path string, dry bool) (orchapi.IssueRestoreResult, error) {
	res := orchapi.IssueRestoreResult{Project: project, DryRun: dry}
	r, err := s.restorer()
	if err != nil {
		return res, err
	}
	cp, done, err := r.OpenCopy(ctx, project, path)
	if errors.Is(err, istore.ErrSchemaNewer) {
		return res, api.Errorf(api.CodeConflict, "this copy was written by a newer zm (%v); nothing changed", err).
			WithHint("restore with the zm that wrote it, or a newer one")
	}
	if err != nil {
		return res, api.Errorf(api.CodeBadRequest, "this copy cannot be restored: %v", err)
	}
	open := true
	defer func() {
		if open {
			_ = done()
		}
	}()
	if st, err := cp.Meta(ctx, importState); err != nil {
		return res, err
	} else if st == "running" {
		return res, api.Errorf(api.CodeConflict, "this copy was taken while an import ran, so it is not whole; pick an older one")
	}
	prefix, err := cp.Prefix(ctx)
	if err != nil {
		return res, err
	}
	if err := s.checkPrefix(ctx, project, prefix); err != nil {
		return res, err
	}
	if res.Redactions, err = s.redactListed(ctx, project, cp, nil); err != nil {
		return res, fmt.Errorf("applying the redaction list: %w", err)
	}
	if res.Redactions > 0 {
		if err := cp.Compact(ctx); err != nil { // no free page keeps the text
			return res, fmt.Errorf("compacting the copy: %w", err)
		}
	}
	if res.Counts, err = cp.Counts(ctx); err != nil {
		return res, err
	}
	open = false
	if err := done(); err != nil {
		return res, err
	}
	if dry {
		return res, nil
	}
	if err := r.Replace(ctx, project, path); err != nil {
		return res, api.Errorf(api.CodeUnavailable, "replacing the store failed: %v", err)
	}
	s.mu.Lock()
	if prefix != "" {
		s.prefixes[prefix] = project
	}
	s.mu.Unlock()
	s.rearea(ctx, project) // the area rules may have changed since the copy
	return res, nil
}

// checkPrefix refuses a copy of another project's store: its issue ids and
// its readers belong elsewhere.
func (s *Service) checkPrefix(ctx context.Context, project, prefix string) error {
	// A damaged store fails this, and a damaged store is why one restores:
	// the check then has what it has.
	if err := s.loadPrefixes(ctx); err != nil {
		s.warn("restore: reading the stores' prefixes", "err", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if owner, ok := s.prefixes[prefix]; ok && prefix != "" && owner != project {
		return api.Errorf(api.CodeConflict, "this copy's issues are %s-*, which belong to another project", prefix)
	}
	for pre, owner := range s.prefixes {
		if owner == project && pre != prefix {
			return api.Errorf(api.CodeConflict, "this copy's issues are %q, this project's are %q: it is another project's store", prefix, pre)
		}
	}
	return nil
}
