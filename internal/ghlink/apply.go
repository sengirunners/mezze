package ghlink

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/ghlink/gate"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// enqueue adds PR snapshots for project to the inbox (one transaction).
func (s *Service) enqueue(ctx context.Context, project string, prs []PR) error {
	if len(prs) == 0 {
		return nil
	}
	err := store.Tx(ctx, s.db(), func(tx *sql.Tx) error { return s.enqueueTx(ctx, tx, project, prs) })
	if err == nil {
		for _, p := range prs {
			s.browse.Invalidate(project, p.Repo)
		}
		s.poke()
	}
	return err
}

func (s *Service) enqueueTx(ctx context.Context, tx *sql.Tx, project string, prs []PR) error {
	now := store.Millis(s.now())
	for _, p := range prs {
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO ghlink_inbox (id, project, pr, next_at) VALUES (?, ?, ?, ?)`,
			ids.New(), project, string(b), now); err != nil {
			return err
		}
	}
	return nil
}

// work applies inbox entries in arrival order. A failed entry (the tracker
// did not answer) is retried with a doubling pause, then dropped and named
// in the link's last_error.
func (s *Service) work(ctx context.Context) {
	defer s.wg.Done()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	prune := time.NewTicker(time.Hour)
	defer prune.Stop()
	for {
		s.drain(ctx)
		s.recheck(ctx)
		select {
		case <-prune.C:
			_, _ = s.db().ExecContext(ctx, `DELETE FROM ghlink_deliveries WHERE time < ?`, store.Millis(s.now().Add(-7*24*time.Hour)))
			_, _ = s.db().ExecContext(ctx, `DELETE FROM ghlink_ci WHERE time < ?`, store.Millis(s.now().Add(-30*24*time.Hour)))
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-t.C:
		}
	}
}

// drain applies every due inbox entry once.
func (s *Service) drain(ctx context.Context) (applied int, errs []string) {
	for ctx.Err() == nil {
		var id, project, raw string
		var attempts int
		err := s.db().QueryRowContext(ctx, `SELECT id, project, pr, attempts FROM ghlink_inbox WHERE next_at <= ? ORDER BY id LIMIT 1`,
			store.Millis(s.now())).Scan(&id, &project, &raw, &attempts)
		if err != nil {
			return applied, errs
		}
		var p PR
		if json.Unmarshal([]byte(raw), &p) != nil {
			_, _ = s.db().ExecContext(ctx, `DELETE FROM ghlink_inbox WHERE id = ?`, id)
			continue
		}
		n, err := s.apply(ctx, project, p)
		applied += n
		if ctx.Err() != nil {
			return applied, errs
		}
		if err == nil {
			_, _ = s.db().ExecContext(ctx, `DELETE FROM ghlink_inbox WHERE id = ?`, id)
			continue
		}
		msg := fmt.Sprintf("%s#%d: %s", p.Repo, p.Number, errText(err))
		errs = append(errs, msg)
		s.setError(ctx, project, msg)
		if errors.Is(err, errGate) {
			// The gate did not answer match: wait for it at the recheck pace,
			// for as long as a gate may hold a decision, then give up loudly.
			if s.now().Sub(ids.Time(id)) >= gate.MaxRetention {
				_, _ = s.db().ExecContext(ctx, `DELETE FROM ghlink_inbox WHERE id = ?`, id)
				s.d.Log.Warn("ghlink: gave up waiting for the gate", "project", project, "pr", msg)
				continue
			}
			pause := gate.DefaultRecheck
			if s.GateRecheck > 0 {
				pause = s.GateRecheck
			}
			_, _ = s.db().ExecContext(ctx, `UPDATE ghlink_inbox SET next_at = ? WHERE id = ?`, store.Millis(s.now().Add(pause)), id)
			continue
		}
		if attempts+1 >= max(s.MaxAttempts, 1) {
			_, _ = s.db().ExecContext(ctx, `DELETE FROM ghlink_inbox WHERE id = ?`, id)
			s.d.Log.Warn("ghlink: gave up", "project", project, "pr", msg)
			continue
		}
		pause := s.Retry << min(attempts, 10)
		_, _ = s.db().ExecContext(ctx, `UPDATE ghlink_inbox SET attempts = ?, next_at = ? WHERE id = ?`,
			attempts+1, store.Millis(s.now().Add(pause)), id)
	}
	return applied, errs
}

func errText(err error) string {
	var e *api.Error
	if errors.As(err, &e) {
		return e.Message
	}
	return err.Error()
}

func (s *Service) setError(ctx context.Context, project, msg string) {
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	_, _ = s.db().ExecContext(ctx, `UPDATE ghlink_links SET last_error = ? WHERE project = ?`, msg, project)
}

// workClient calls the work API in-process as the linker's identity, with
// its member token capped to project.
func (s *Service) workClient(project string) (*client.Client, error) {
	tok, err := s.token(project)
	if err != nil || tok == "" {
		return nil, api.Errorf(api.CodeUnavailable, "the linker's token for this project is missing").
			WithHint("run zm github link again")
	}
	c := client.WithHandler(s.d.Handler)
	c.Token, c.Project, c.ProjectFrom = tok, project, "flag"
	return c, nil
}

// errGate marks a failure that is the gate's (it did not answer match): the
// snapshot waits for it instead of counting toward MaxAttempts (SPEC 18.3:
// never act on doubt, never drop a merge because a gate was down a while).
var errGate = errors.New("gate unavailable")

// candidates are the work ids p names: the gate's match when it has one,
// else 18.2's matcher over the link's --match texts.
func (s *Service) candidates(ctx context.Context, l link, p PR) ([]string, error) {
	if l.Gate != "" {
		ids, err := s.gateFor(l).Match(ctx, gatePR(p))
		switch {
		case err == nil:
			return s.resolveIDs(l, ids), nil
		case !errors.Is(err, gate.ErrNoMatch):
			return nil, fmt.Errorf("%w: %s", errGate, errText(err))
		}
	}
	var texts []string
	for _, m := range matchTexts(l) {
		switch m {
		case MatchTitle:
			texts = append(texts, p.Title)
		case MatchBranch:
			texts = append(texts, p.Branch)
		case MatchBody:
			texts = append(texts, p.Body)
		}
	}
	return NewMatcher(s.prefixes(l), s.sources(l)).IDs(texts...), nil
}

// resolveIDs turns a gate's ids into work ids: "<source>:<ref>" for one of
// the link's sources (or local) as is, a bare id in each source.
func (s *Service) resolveIDs(l link, ids []string) []string {
	srcs := s.sources(l)
	var out []string
	add := func(id string) {
		if len(out) < MaxIDs && !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	for _, id := range ids {
		if src, ref, ok := strings.Cut(id, ":"); ok {
			if ref != "" && (src == "local" || slices.Contains(srcs, src)) {
				add(id)
			}
			continue
		}
		for _, src := range srcs {
			add(src + ":" + strings.ToLower(id))
		}
	}
	return out
}

// apply moves the items p names. It reports how many writes it made. An
// error means "try again later": what was already done is recorded per
// item, so a retry never repeats a comment.
func (s *Service) apply(ctx context.Context, project string, p PR) (int, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	l, err := s.link(ctx, project)
	if err != nil || !hasRepo(l, p.Repo) {
		return 0, nil // unlinked meanwhile: nothing to do
	}
	switch p.State {
	case StateOpen, StateMerged, StateClosed:
	default:
		return 0, nil
	}
	// Snapshots apply in GitHub's updated_at order: an older (or replayed)
	// one changes nothing.
	var last int64
	err = s.db().QueryRowContext(ctx, `SELECT updated FROM ghlink_prs WHERE project = ? AND repo = ? AND number = ?`,
		project, p.Repo, p.Number).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if err == nil && store.Millis(p.Updated) <= last {
		return 0, nil
	}
	cands, err := s.candidates(ctx, l, p)
	if err != nil {
		return 0, err
	}
	n := 0
	if len(cands) > 0 {
		c, err := s.workClient(project)
		if err != nil {
			return 0, err
		}
		for _, id := range cands {
			k, err := s.applyItem(ctx, c, l, p, id)
			n += k
			if err != nil {
				// The PR's lifecycle does not wait for the tracker: its
				// events go out now with the items linked so far (a retry
				// publishes nothing twice).
				if lerr := s.lifecycle(ctx, project, p); lerr != nil {
					s.d.Log.Warn("ghlink: PR event", "project", project, "pr", p.Number, "err", lerr)
				}
				return n, err
			}
		}
	}
	if err := s.lifecycle(ctx, project, p); err != nil {
		return n, err
	}
	_, err = s.db().ExecContext(ctx, `INSERT INTO ghlink_prs (project, repo, number, updated) VALUES (?, ?, ?, ?)
		ON CONFLICT (project, repo, number) DO UPDATE SET updated = excluded.updated`, project, p.Repo, p.Number, store.Millis(p.Updated))
	if err == nil {
		_, _ = s.db().ExecContext(ctx, `UPDATE ghlink_links SET last_event = ? WHERE project = ?`, store.Millis(s.now()), project)
	}
	return n, err
}

// waitRow is a decision a gate holds (ghlink_waits).
type waitRow struct {
	state    string
	first    time.Time
	attempts int
	lastNote string
}

func (s *Service) loadWait(ctx context.Context, project string, p PR, id string) (*waitRow, error) {
	var w waitRow
	var first int64
	err := s.db().QueryRowContext(ctx, `SELECT state, first, attempts, last_note FROM ghlink_waits WHERE project = ? AND repo = ? AND number = ? AND item = ?`,
		project, p.Repo, p.Number, id).Scan(&w.state, &first, &w.attempts, &w.lastNote)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	w.first = store.FromMillis(first)
	return &w, err
}

func (s *Service) dropWait(ctx context.Context, project string, p PR, id string) {
	_, _ = s.db().ExecContext(ctx, `DELETE FROM ghlink_waits WHERE project = ? AND repo = ? AND number = ? AND item = ?`, project, p.Repo, p.Number, id)
}

// recordWrite notes when the linker last wrote to the item for this PR (a
// gate tells its own writes from a person's later touch by it).
func (s *Service) recordWrite(ctx context.Context, project string, p PR, id string) error {
	_, err := s.db().ExecContext(ctx, `INSERT INTO ghlink_items (project, repo, number, item, state, last_write) VALUES (?, ?, ?, ?, '', ?)
		ON CONFLICT (project, repo, number, item) DO UPDATE SET last_write = excluded.last_write`,
		project, p.Repo, p.Number, id, store.Millis(s.now()))
	return err
}

func gatePR(p PR) gate.PR {
	labels := p.Labels
	if labels == nil {
		labels = []string{}
	}
	return gate.PR{Repo: p.Repo, Number: p.Number, Title: p.Title, Body: p.Body, Branch: p.Branch, URL: p.URL, State: p.State,
		Draft: p.Draft, Labels: labels, MergedAt: p.MergedAt, MergeSHA: p.MergeSHA, HeadSHA: p.HeadSHA, Base: p.Base, Updated: p.Updated}
}

func gateItem(it orchapi.WorkItem, lastWrite sql.NullInt64) gate.Item {
	return gate.Item{ID: it.ID, Title: it.Title, Status: string(it.Status), Owner: it.Owner, Source: it.Source, Labels: it.Labels,
		Extra: it.Extra, LastWrite: store.NullMillis(lastWrite)}
}

func (s *Service) applyItem(ctx context.Context, c *client.Client, l link, p PR, id string) (int, error) {
	var prev string
	var lastWrite sql.NullInt64
	err := s.db().QueryRowContext(ctx, `SELECT state, last_write FROM ghlink_items WHERE project = ? AND repo = ? AND number = ? AND item = ?`,
		l.Project, p.Repo, p.Number, id).Scan(&prev, &lastWrite)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if prev == p.State {
		s.dropWait(ctx, l.Project, p, id)
		return 0, nil
	}
	it, err := orchclient.GetWork(ctx, c, id)
	if api.IsCode(err, api.CodeNotFound) {
		s.dropWait(ctx, l.Project, p, id)
		return 0, nil // not an item (or not one the linker may see): text that only looks like an id
	}
	if err != nil {
		return 0, err
	}
	ref := fmt.Sprintf("%s#%d", p.Repo, p.Number)
	var note string
	var to orchapi.WorkStatus
	held := it.Owner != ""
	switch p.State {
	case StateOpen:
		note = fmt.Sprintf("PR %s opened: %s %s", ref, oneLine(p.Title), p.URL)
		if p.Branch != "" {
			note += " (branch " + p.Branch + ")"
		}
		if st := orchapi.WorkStatus(l.OnOpen); l.OnOpen != "none" && it.Status == orchapi.WorkOpen && !held {
			if st == orchapi.WorkClaimed && it.Source == "local" {
				st = orchapi.WorkInReview // a local item is claimed only by a lease; nobody holds one
			}
			to = st
		} else if held && l.OnOpen != "none" {
			note += "; status left as is (held by " + it.Owner + ")"
		}
	case StateMerged:
		note = fmt.Sprintf("Closed by merged PR %s: %s", ref, p.URL)
		switch {
		case l.OnMerge == "none": // say what happened: nothing moved
			note = fmt.Sprintf("PR %s merged: %s (status left as is: this project's GitHub link moves nothing on merge)", ref, p.URL)
		case l.OnMerge != string(orchapi.WorkDone):
			note = fmt.Sprintf("PR %s merged: %s", ref, p.URL)
		}
		if l.OnMerge != "none" && it.Status != orchapi.WorkStatus(l.OnMerge) {
			to = orchapi.WorkStatus(l.OnMerge)
		}
	case StateClosed:
		note = fmt.Sprintf("PR %s closed without merging: %s", ref, p.URL)
		if l.OnClose != "none" && prev == StateOpen && !held &&
			(it.Status == orchapi.WorkClaimed || it.Status == orchapi.WorkInReview) && it.Status != orchapi.WorkStatus(l.OnClose) {
			to = orchapi.WorkStatus(l.OnClose)
		}
	}
	n := 0
	if l.Gate != "" {
		var dec gate.Decision
		var waitNote bool
		dec, waitNote, n, err = s.ask(ctx, c, l, p, it, lastWrite, to, note)
		if err != nil || waitNote {
			return n, err
		}
		switch dec.Decision {
		case gate.Skip:
			s.dropWait(ctx, l.Project, p, id)
			_, err := s.db().ExecContext(ctx, `INSERT INTO ghlink_items (project, repo, number, item, state) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT (project, repo, number, item) DO UPDATE SET state = excluded.state`, l.Project, p.Repo, p.Number, id, p.State)
			return n, err
		case gate.Note:
			to = ""
			if p.State == StateMerged && dec.Note == "" {
				note = fmt.Sprintf("PR %s merged (not closed here): %s", ref, p.URL)
			}
		}
		if dec.Note != "" {
			note = dec.Note
		}
	}
	if !slices.ContainsFunc(it.Links, func(x orchapi.Link) bool { return x.Rel == "pr" && x.Target == p.URL }) && p.URL != "" {
		if _, err := orchclient.Link(ctx, c, id, orchapi.LinkRequest{Rel: "pr", Target: p.URL}); err != nil {
			return n, err
		}
		n++
	}
	// Status first, then the comment: each step is idempotent on a retry
	// (a moved item no longer meets its condition; the comment is the last
	// write before the state is recorded).
	if to != "" {
		if _, err := orchclient.SetStatus(ctx, c, id, orchapi.StatusRequest{Status: to}); err != nil {
			if !api.IsCode(err, api.CodeConflict) && !api.IsCode(err, api.CodeBadRequest) && !api.IsCode(err, api.CodeForbidden) {
				return n, err
			}
			// The tracker refused (held elsewhere, a status it does not
			// take): say so on the item rather than retry forever.
			note += fmt.Sprintf("; could not move it to %s: %s", to, errText(err))
		} else {
			n++
		}
	}
	if _, err := orchclient.Comment(ctx, c, id, orchapi.CommentRequest{Body: note}); err != nil {
		return n, err
	}
	n++
	s.dropWait(ctx, l.Project, p, id)
	_, err = s.db().ExecContext(ctx, `INSERT INTO ghlink_items (project, repo, number, item, state, last_write) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (project, repo, number, item) DO UPDATE SET state = excluded.state, last_write = excluded.last_write`,
		l.Project, p.Repo, p.Number, id, p.State, store.Millis(s.now()))
	return n, err
}

// ask is the gate's decision for one item and PR state (SPEC 18.3). A wait
// is handled here: the decision is kept in ghlink_waits (its note commented
// once per text) and held=true tells the caller to write nothing more. A
// gate error is a wait. Past the retention the gate is asked once more with
// final; a wait then becomes a give-up note.
func (s *Service) ask(ctx context.Context, c *client.Client, l link, p PR, it orchapi.WorkItem, lastWrite sql.NullInt64, to orchapi.WorkStatus, note string) (dec gate.Decision, held bool, n int, err error) {
	g := s.gateFor(l)
	now := s.now().Truncate(time.Millisecond) // as stored
	recheck, retention, name := gate.DefaultRecheck, gate.DefaultRetention, "the project's gate"
	desc, derr := g.Describe(ctx)
	if derr == nil {
		recheck, retention, name = desc.Recheck(), desc.Retention(), desc.Name
	}
	if s.GateRecheck > 0 {
		recheck = s.GateRecheck
	}
	w, err := s.loadWait(ctx, l.Project, p, it.ID)
	if err != nil {
		return dec, false, 0, err
	}
	first, attempt, lastNote := now, 1, ""
	if w != nil && w.state == p.State {
		first, attempt, lastNote = w.first, w.attempts+1, w.lastNote
	}
	final := now.Sub(first) >= retention
	if derr == nil {
		dec, err = g.Decide(ctx, gate.Request{PR: gatePR(p), Item: gateItem(it, lastWrite),
			Proposed: gate.Proposal{To: string(to), Comment: note}, FirstAsked: first, Attempt: attempt, Final: final})
	} else {
		err = derr
	}
	gateErr := err != nil
	if gateErr {
		s.setError(ctx, l.Project, fmt.Sprintf("gate on %s#%d %s: %s", p.Repo, p.Number, it.ID, errText(err)))
		s.d.Log.Warn("ghlink: gate", "project", l.Project, "pr", p.Number, "item", it.ID, "err", err)
		dec = gate.Decision{Decision: gate.Wait}
	}
	if dec.Decision != gate.Wait {
		return dec, false, 0, nil
	}
	ref := fmt.Sprintf("%s#%d", p.Repo, p.Number)
	switch {
	case final && !gateErr:
		// The gate itself answered wait past its retention: give up, once.
		return gate.Decision{Decision: gate.Note, Note: fmt.Sprintf("Not moved: %s kept PR %s waiting for %s; decide by hand. %s",
			name, ref, shortDur(now.Sub(first)), p.URL)}, false, 0, nil
	case gateErr && now.Sub(first) >= gate.MaxRetention:
		// A gate that never answered is not a verdict: it is held as long as
		// any gate may hold a decision, then named honestly.
		return gate.Decision{Decision: gate.Note, Note: fmt.Sprintf("Not moved: %s did not answer about PR %s for %s; decide by hand. %s",
			name, ref, shortDur(now.Sub(first)), p.URL)}, false, 0, nil
	}
	if dec.Note != "" && dec.Note != lastNote {
		if _, err := orchclient.Comment(ctx, c, it.ID, orchapi.CommentRequest{Body: dec.Note}); err != nil {
			return dec, true, 0, err
		}
		n++
		lastNote = dec.Note
		if err := s.recordWrite(ctx, l.Project, p, it.ID); err != nil {
			return dec, true, n, err
		}
	}
	raw, _ := json.Marshal(p)
	_, err = s.db().ExecContext(ctx, `INSERT INTO ghlink_waits (project, repo, number, item, state, pr, first, next_at, attempts, last_note)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (project, repo, number, item) DO UPDATE SET state = excluded.state, pr = excluded.pr, first = excluded.first,
		next_at = excluded.next_at, attempts = excluded.attempts, last_note = excluded.last_note`,
		l.Project, p.Repo, p.Number, it.ID, p.State, string(raw), store.Millis(first), store.Millis(now.Add(recheck)), attempt, lastNote)
	return dec, true, n, err
}

// recheck asks the gate again for every held decision that is due.
func (s *Service) recheck(ctx context.Context) (applied int) {
	for ctx.Err() == nil {
		var project, repo, item, raw string
		var number int
		now := s.now()
		err := s.db().QueryRowContext(ctx, `SELECT project, repo, number, item, pr FROM ghlink_waits WHERE next_at <= ? ORDER BY next_at LIMIT 1`,
			store.Millis(now)).Scan(&project, &repo, &number, &item, &raw)
		if err != nil {
			return applied
		}
		// Lease it first: whatever happens below, it is not picked again at once.
		pause := max(s.Retry, time.Minute)
		if s.GateRecheck > 0 {
			pause = s.GateRecheck
		}
		_, _ = s.db().ExecContext(ctx, `UPDATE ghlink_waits SET next_at = ? WHERE project = ? AND repo = ? AND number = ? AND item = ?`,
			store.Millis(now.Add(pause)), project, repo, number, item)
		var p PR
		if json.Unmarshal([]byte(raw), &p) != nil {
			_, _ = s.db().ExecContext(ctx, `DELETE FROM ghlink_waits WHERE project = ? AND repo = ? AND number = ? AND item = ?`, project, repo, number, item)
			continue
		}
		applied += s.recheckOne(ctx, project, p, item, raw)
	}
	return applied
}

func (s *Service) recheckOne(ctx context.Context, project string, p PR, item, raw string) int {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	// Read the wait again under the lock: an inbox entry or a sync may have
	// decided it, or replaced it with a newer snapshot, since it was picked.
	var now string
	err := s.db().QueryRowContext(ctx, `SELECT pr FROM ghlink_waits WHERE project = ? AND repo = ? AND number = ? AND item = ?`,
		project, p.Repo, p.Number, item).Scan(&now)
	if err != nil || now != raw {
		return 0
	}
	if !s.hosted(project) {
		return 0 // moved off this machine: the held decisions wait, none is dropped or decided here
	}
	l, err := s.link(ctx, project)
	if err != nil || !hasRepo(l, p.Repo) || l.Gate == "" {
		s.dropWait(ctx, project, p, item) // unlinked, or the gate was removed: its decisions go with it
		return 0
	}
	c, err := s.workClient(project)
	if err != nil {
		s.setError(ctx, project, errText(err))
		return 0
	}
	n, err := s.applyItem(ctx, c, l, p, item)
	if err != nil && ctx.Err() == nil {
		s.setError(ctx, project, fmt.Sprintf("%s#%d %s: %s", p.Repo, p.Number, item, errText(err)))
	}
	return n
}

// shortDur is 45m, 3h, 14d.
func shortDur(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours())/24)
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}
