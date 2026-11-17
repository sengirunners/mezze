package issues

import (
	"context"
	"errors"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

// Redaction (identifiers design, stage 2b): an admin of the issue's area, or
// the machine owner, makes a comment's text stop existing. The store
// overwrites the comment and every history copy of it; every other module
// scrubs its own copies (core.Scrub: the work module's comment copies, the
// event log, the webhook queue, team write records); the touched databases
// are compacted so no old page keeps the text; then every sqlite backup
// writes a fresh copy and deletes its older ones (orchcore.HomeService
// PurgeBackups, last, so a fresh copy holds no scrubbed text). The redaction
// list lives in registry.db, outside the store it protects: a restore of an
// older copy re-applies it, so redacted text cannot come back.

var redactMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "issues/0001_redactions", Script: `
CREATE TABLE issue_redactions (
	project    TEXT NOT NULL,
	comment_id TEXT NOT NULL,
	issue_id   TEXT NOT NULL,
	who        TEXT NOT NULL,
	at         INTEGER NOT NULL,
	PRIMARY KEY (project, comment_id)
);`},
}

// redact is POST /v0/issues/{id}/comments/{cid}/redact.
func (s *Service) redact(ctx context.Context, sub *authz.Subject, l located, cid string) (any, error) {
	if !moderates(sub, l.project, "") {
		// The store checks the issue's own area; this only keeps a caller who
		// moderates nothing from writing to the list.
		if it, err := l.ps.Get(ctx, l.id, l.vis); err != nil || !moderates(sub, l.project, it.Area) {
			return nil, api.Errorf(api.CodeForbidden, "only an admin of the issue's area, or the machine owner, redacts a comment")
		}
	}
	res, err := s.redactEverywhere(ctx, context.WithoutCancel(ctx), sub, l, cid, sub.Address, s.now().UnixMilli())
	if err != nil {
		return nil, err
	}
	return s.commentAnswer(ctx, sub, l, res.Comment)
}

// finishRedaction lets maintenance honor cancellation while a user's redaction
// keeps scrubbing after its HTTP request ends. scrubbed says it had texts
// (this redaction's or an earlier pending one's) and scrubbed every live
// copy of them; older backups may still wait for their purge. fresh asks
// for a fresh purge (PurgeBackups) even with no texts: a stop left unsettled
// may have failed its purge before homes marked its backups, so a purge of
// only marked backups would replace nothing. Texts scrubbed here always purge
// fresh.
func (s *Service) finishRedaction(ctx context.Context, l located, cid string, texts []string, fresh bool) (scrubbed bool, err error) {
	key := l.project + "\x00" + l.id + "\x00" + cid
	at := s.now() // this redaction's time, carried by its retries (core.Scrub.At)
	s.mu.Lock()
	if p, ok := s.pendingScrub[key]; ok {
		texts = append(texts, p.texts...) // an earlier scrub that has not finished
	}
	s.mu.Unlock()
	if err := s.scrubWithRetry(ctx, l.project, l.id, texts, at); err != nil {
		// The store's copy is gone; another copy may not be. The texts stay
		// in memory only (writing them down would defeat the redaction), and
		// zm keeps trying.
		s.mu.Lock()
		s.pendingScrub[key] = pendingScrub{project: l.project, issue: l.id, cid: cid, texts: texts, at: at}
		s.mu.Unlock()
		return false, api.Errorf(api.CodeUnavailable, "the comment is redacted in the issue store, but scrubbing its other copies has not finished: %v", err).
			WithHint("zm retries every 30 s until it succeeds or the daemon stops; redact again to retry now")
	}
	s.mu.Lock()
	delete(s.pendingScrub, key)
	s.mu.Unlock()
	if err := orchcore.Homes(s.d).PurgeBackups(ctx, l.project, fresh || len(texts) > 0); err != nil {
		// Older copies stay (a backup is never deleted before a good one
		// replaces it). The purge needs no text, so a backup keeps its place in
		// line across a restart.
		hint := "zm keeps trying, and zm tracker where shows which backup waits; redact again to retry now"
		if gaps, waiting := orchcore.UnreplacedCopies(err); len(gaps) > 0 {
			copyHint := "no backup schedule replaces " + gaps[0] + " (or any other copy the message names): delete it if it may hold the text, then redact again"
			if waiting {
				copyHint += "; for the other backups, " + hint
			}
			hint = copyHint
		}
		return len(texts) > 0, api.Errorf(api.CodeUnavailable, "the comment is redacted, but it is not fully scrubbed: older backup copies may still hold its text: %v", err).
			WithHint(hint)
	}
	return len(texts) > 0, nil
}

// pendingScrub is a redaction whose other copies are not scrubbed yet.
type pendingScrub struct {
	project, issue, cid string
	texts               []string
	at                  time.Time // when it was redacted (its retries keep it)
}

// scrubWithRetry scrubs, retrying a busy store a few times before giving up.
func (s *Service) scrubWithRetry(ctx context.Context, project, issue string, texts []string, at time.Time) error {
	var err error
	for i, wait := range []time.Duration{0, 250 * time.Millisecond, time.Second, 4 * time.Second} {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		if len(texts) > 0 {
			orchcore.Homes(s.d).WaitIdle(ctx, project) // an hourly backup would fail this attempt
		}
		if err = s.scrubCopies(ctx, project, issue, texts, at); err == nil {
			return nil
		}
	}
	return err
}

// retryPendingScrubs runs every pending redaction's scrub again.
func (s *Service) retryPendingScrubs(ctx context.Context) {
	s.mu.Lock()
	todo := make(map[string]pendingScrub, len(s.pendingScrub))
	for k, p := range s.pendingScrub {
		todo[k] = p
	}
	s.mu.Unlock()
	for k, p := range todo {
		if err := s.scrubCopies(ctx, p.project, p.issue, p.texts, p.at); err != nil {
			s.warn("redaction scrub retry", "project", p.project, "issue", p.issue, "err", err)
			continue
		}
		s.mu.Lock()
		delete(s.pendingScrub, k)
		s.mu.Unlock()
		state := stopClean // only once the purge succeeded
		if err := orchcore.Homes(s.d).PurgeBackups(ctx, p.project, true); err != nil {
			state = stopPurge
			s.warn("redaction: older backup copies remain", "project", p.project, "issue", p.issue, "err", err)
		}
		if err := s.setStop(ctx, s.d.Store.Registry(), p.cid, p.project, p.issue, state); err != nil {
			s.warn("redaction stop", "project", p.project, "issue", p.issue)
		}
	}
}

// scrubCopies has every module scrub texts filed under issue id (and its
// work ids), then compacts the stores so no free page keeps them.
func (s *Service) scrubCopies(ctx context.Context, project, id string, texts []string, at time.Time) error {
	if len(texts) == 0 {
		return nil
	}
	err := s.d.ScrubAll(ctx, core.Scrub{Project: project, Subjects: s.subjectsOf(project, id), Texts: texts, At: at})
	return errors.Join(err, s.compact(ctx, project))
}

// subjectsOf is what copies of issue id are filed under: the id and its work ids.
func (s *Service) subjectsOf(project, id string) []string {
	subjects := []string{id}
	if f, ok := s.d.Projects.File(project); ok {
		for _, t := range orchcore.ProjectTrackers(f) {
			if t.Adapter == orchapi.AdapterNative {
				subjects = append(subjects, t.Prefix+":"+id)
			}
		}
	}
	return subjects
}

// compact rewrites project's stores: old pages and WAL frames still hold a
// scrubbed text until then. One that could not finish is a failure
// (retried), not a warning.
func (s *Service) compact(ctx context.Context, project string) error {
	var err error
	if ps, perr := s.st.Project(ctx, project, false); perr == nil {
		err = ps.Compact(ctx)
	}
	return errors.Join(err, s.d.Store.Compact(ctx, project))
}

// redactListed redacts, in ps, every comment on project's redaction list that
// it holds, and calls after (if set) with what each overwrote. It returns how
// many still held their text.
func (s *Service) redactListed(ctx context.Context, project string, ps istore.ProjectStore, after func(issue string, texts []string) error) (int, error) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT comment_id, issue_id FROM issue_redactions WHERE project = ?`, project)
	if err != nil {
		return 0, err
	}
	type red struct{ cid, issue string }
	var rs []red
	for rows.Next() {
		var r red
		if err := rows.Scan(&r.cid, &r.issue); err != nil {
			rows.Close()
			return 0, err
		}
		rs = append(rs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	sys := s.d.System()
	n := 0
	for _, r := range rs {
		m := istore.Mutation{Op: istore.OpCommentRedact, ID: r.issue, CommentID: r.cid, Actor: actorOf(sys), At: s.now(),
			AreaOf:    s.areaFunc(project),
			Authorize: func(string, string) error { return nil },
			Moderate:  func(string) bool { return true }}
		res, err := ps.Apply(ctx, m)
		if notFound(err) {
			// The key is (project, comment): the row names one of the ids
			// the comment's issue had here. The comment id is the identity.
			m.ID = ""
			res, err = ps.Apply(ctx, m)
		}
		if notFound(err) {
			continue // not in this copy
		}
		if err != nil {
			return n, err
		}
		if len(res.Redacted) > 0 {
			n++
		}
		if after != nil {
			if err := after(res.Issue.ID, res.Redacted); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func notFound(err error) bool {
	return api.IsCode(err, api.CodeNotFound) || errors.Is(err, istore.ErrNotFound)
}
