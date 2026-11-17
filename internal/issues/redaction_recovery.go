package issues

import (
	"context"
	"errors"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/potluck/kit/api"
)

const redactionRecoveryBatch = 32

// One pass per start, with another pass only when an attempt failed. Keeping
// just a keyset cursor bounds memory even when every old redaction is blocked.
// Registry rows hold IDs only; recovered text uses the existing in-memory retry.
type redactionRecovery struct {
	project, comment string
	retry, done      bool
}

func (s *Service) recoverRedactions(parent context.Context) {
	if s.recovery.done || parent.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	type entry struct{ project, comment, issue string }
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT project, comment_id, issue_id FROM issue_redactions
		WHERE (project, comment_id) > (?, ?) ORDER BY project, comment_id LIMIT ?`, s.recovery.project, s.recovery.comment, redactionRecoveryBatch)
	if err != nil {
		s.warn("redaction recovery deferred") // never log stored text or arbitrary error messages
		return
	}
	var batch []entry
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.project, &e.comment, &e.issue); err != nil {
			break
		}
		batch = append(batch, e)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close() // no registry read transaction survives into a write or compaction
	if err != nil {
		s.warn("redaction recovery deferred")
		return
	}
	if len(batch) == 0 {
		if s.recovery.retry {
			s.recovery = redactionRecovery{}
		} else {
			s.recovery.done = true
		}
		return
	}
	for _, e := range batch {
		if ctx.Err() != nil {
			return
		}
		if err := s.recoverRedaction(ctx, e.project, e.issue, e.comment); err != nil {
			s.recovery.retry = true
			s.warn("redaction recovery deferred", "project", e.project, "comment", e.comment)
		}
		s.recovery.project, s.recovery.comment = e.project, e.comment
	}
}

func (s *Service) recoverRedaction(ctx context.Context, project, issue, cid string) error {
	if _, local := s.d.Projects.File(project); !local {
		return nil // joined projects belong to their authority, never this worker
	}
	var who string
	var at int64
	if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT who, at FROM issue_redactions WHERE project = ? AND comment_id = ? AND issue_id = ?`,
		project, cid, issue).Scan(&who, &at); err != nil {
		return err
	}
	for attempt := 0; attempt < 2; attempt++ {
		chain, found, err := s.recoveryLocations(ctx, project, issue)
		if err != nil {
			return err
		}
		if !found {
			if attempt == 0 {
				return nil
			}
			break // a move raced the write: retry on a later maintenance pass
		}
		// Every stop the records name gets the original redaction's identity
		// (so a restore there reapplies it) and, when the store still held
		// text, its own scrub and retry. A stop that only an older zm's
		// scrub could have cleaned is reported, never guessed at.
		_, err = s.redactEverywhere(ctx, ctx, s.d.System(), chain[len(chain)-1], cid, who, at)
		var gap *incompleteError
		if errors.As(err, &gap) {
			s.warn("redaction cleanup incomplete", "project", project, "comment", cid) // ids only
			return nil                                                                 // nothing a retry can change
		}
		if notFound(err) {
			continue // re-resolve from the original recorded project, never the prefix
		}
		return err
	}
	return api.Errorf(api.CodeConflict, "issue moved or comment disappeared during redaction recovery; retry later")
}

// Anchor the lookup in the registry row's recorded project. A prefix may have
// been reused after its old store emptied; global prefix lookup loses that
// provenance. Only explicit store redirects authorize following another project.
func (s *Service) recoveryLocations(ctx context.Context, project, issue string) ([]located, bool, error) {
	var chain []located
	for hop := 0; hop <= maxHops; hop++ {
		if _, local := s.d.Projects.File(project); !local {
			return nil, false, nil
		}
		ps, err := s.st.Project(ctx, project, false)
		if errors.Is(err, istore.ErrNotFound) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		all := istore.Areas{All: true}
		chain = append(chain, located{project: project, id: issue, ps: ps, vis: all})
		if _, err := ps.Get(ctx, issue, all); err == nil {
			return chain, true, nil
		} else if !errors.Is(err, istore.ErrNotFound) {
			return nil, false, err
		}
		mv, ok := ps.(istore.Mover)
		if !ok {
			return nil, false, nil
		}
		next, dest, moved, err := mv.MovedTo(ctx, issue)
		if err != nil || !moved {
			return nil, false, err
		}
		project, issue = dest, next
	}
	return nil, false, nil
}
