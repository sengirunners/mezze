package approvals

// Person steps (SPEC 16 "Person steps"): a manual step for one person, with
// no real choice in it, that waits at the project's authority until the
// person's daily steps card carries it (zm ask bundle). This file is the
// store and its routes; the step judge is stepjudge.go.
//
// The store API the daily card (bundle compile) and the mail redirect use,
// in this package, all at the authority:
//
//   - saveDraftStep: a refused report to one person, saved whole as a draft
//     step (its sender, work item and body) for its sender to complete.
//   - pendingSteps: one person's pending steps across the projects, oldest
//     first: what the next card carries.
//   - cardSteps: puts pending steps on a filed card (bundle id, ask id,
//     numbers 1..n in the order given); all or nothing.
//   - returnSteps: a card's carded steps (not reported, failed or ended) go
//     back to pending: its answer was "Not today", or it was withdrawn to be
//     cut again.
//   - bundleSteps: a card's steps in card order (by bundle or ask id).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/judge"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// The table's CHECKs keep the states honest: a step that is on a card
// (carded, reported or failed) names its card and number, and only a draft
// lacks a title or a check.
var personStepMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0024_person_steps", Script: `
CREATE TABLE person_steps (
	id           TEXT PRIMARY KEY,
	project      TEXT NOT NULL,
	area         TEXT NOT NULL DEFAULT '',
	person       TEXT NOT NULL,
	person_id    TEXT NOT NULL DEFAULT '',
	added_by     TEXT NOT NULL,
	work         TEXT NOT NULL,
	title        TEXT NOT NULL DEFAULT '',
	brief        TEXT NOT NULL,
	check_cmd    TEXT NOT NULL DEFAULT '',
	risk         TEXT NOT NULL DEFAULT 'none' CHECK (risk IN ('none', 'irreversible', 'spending')),
	state        TEXT NOT NULL CHECK (state IN ('draft', 'pending', 'carded', 'reported', 'done', 'failed', 'dropped')),
	bundle       TEXT NOT NULL DEFAULT '',
	number       INTEGER NOT NULL DEFAULT 0,
	ask          TEXT NOT NULL DEFAULT '',
	created      INTEGER NOT NULL,
	updated      INTEGER NOT NULL,
	report_note  TEXT NOT NULL DEFAULT '',
	reported_by  TEXT NOT NULL DEFAULT '',
	reported     INTEGER,
	checked      INTEGER,
	check_exit   INTEGER,
	check_output TEXT NOT NULL DEFAULT '',
	checked_by   TEXT NOT NULL DEFAULT '',
	CHECK (state NOT IN ('carded', 'reported', 'failed') OR (bundle != '' AND number > 0)),
	CHECK (state = 'draft' OR (title != '' AND check_cmd != ''))
);
CREATE INDEX person_steps_person ON person_steps (person_id, state, created);
CREATE INDEX person_steps_project ON person_steps (project, area, state);
CREATE UNIQUE INDEX person_steps_card ON person_steps (bundle, number) WHERE bundle != '';
CREATE INDEX person_steps_ask ON person_steps (ask) WHERE ask != '';`},
}

func init() { Migrations = append(Migrations, personStepMigrations...) }

// stepRow is a stored step and the directory person it is for.
type stepRow struct {
	orchapi.PersonStep
	personID string
}

const stepCols = `id, project, area, person, person_id, added_by, work, title, brief, check_cmd, risk, state, bundle, number, ask,
	created, updated, report_note, reported_by, reported, checked, check_exit, check_output, checked_by`

func scanStep(sc interface{ Scan(...any) error }) (stepRow, error) {
	var st stepRow
	var created, updated int64
	var reported, checked, exit sql.NullInt64
	err := sc.Scan(&st.ID, &st.Project, &st.Area, &st.Person, &st.personID, &st.AddedBy, &st.Work, &st.Title, &st.Brief, &st.Check,
		&st.Risk, &st.State, &st.Bundle, &st.Number, &st.Ask, &created, &updated, &st.ReportNote, &st.ReportedBy, &reported,
		&checked, &exit, &st.CheckOutput, &st.CheckedBy)
	if err != nil {
		return st, err
	}
	st.Created, st.Updated = store.FromMillis(created), store.FromMillis(updated)
	st.ReportedAt, st.CheckedAt = store.NullMillis(reported), store.NullMillis(checked)
	if exit.Valid {
		n := int(exit.Int64)
		st.CheckExit = &n
	}
	return st, nil
}

func (s *Service) querySteps(ctx context.Context, where string, args ...any) ([]stepRow, error) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT `+stepCols+` FROM person_steps WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []stepRow
	for rows.Next() {
		st, err := scanStep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// step is the stored step id (not_found when there is none).
func (s *Service) step(ctx context.Context, id string) (stepRow, error) {
	st, err := scanStep(s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+stepCols+` FROM person_steps WHERE id = ?`, strings.ToUpper(strings.TrimSpace(id))))
	if errors.Is(err, sql.ErrNoRows) {
		return st, api.NotFound("step", id)
	}
	return st, err
}

// ---- the store API (see the file comment) ----

// saveDraftStep saves body, a refused report from to person (name, directory
// id) about work, as a draft step its sender completes.
func (s *Service) saveDraftStep(ctx context.Context, project, area, person, personID, from, work, body string) (stepRow, error) {
	now := s.now()
	st := stepRow{PersonStep: orchapi.PersonStep{ID: ids.New(), Project: project, Area: area, Person: person, AddedBy: from, Work: work,
		Brief: body, Risk: "none", State: orchapi.StepDraft, Created: now, Updated: now}, personID: personID}
	_, err := s.d.Store.Registry().ExecContext(ctx, `INSERT INTO person_steps (id, project, area, person, person_id, added_by, work, brief, risk, state, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'none', 'draft', ?, ?)`, st.ID, project, area, person, personID, from, work, body, ms(now), ms(now))
	return st, err
}

// pendingSteps is personID's pending steps across the projects, oldest first.
func (s *Service) pendingSteps(ctx context.Context, personID string) ([]stepRow, error) {
	return s.querySteps(ctx, `person_id = ? AND state = 'pending' ORDER BY created, id`, personID)
}

// cardSteps puts the steps stepIDs, in that order, on card bundle (filed as
// ask): carded, numbered from 1. Each must be pending (a card cut again
// returns its steps first); else nothing changes and it is a conflict.
func (s *Service) cardSteps(ctx context.Context, bundle, ask string, stepIDs []string) error {
	if bundle == "" || ask == "" || len(stepIDs) == 0 {
		return api.Errorf(api.CodeBadRequest, "a card needs its bundle, its ask and at least one step")
	}
	tx, err := s.d.Store.Registry().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := ms(s.now())
	for i, id := range stepIDs {
		res, err := tx.ExecContext(ctx, `UPDATE person_steps SET state = 'carded', bundle = ?, number = ?, ask = ?, updated = ? WHERE id = ? AND state = 'pending'`,
			bundle, i+1, ask, now, id)
		if err != nil && strings.Contains(err.Error(), "UNIQUE") {
			return api.Errorf(api.CodeConflict, "card %s already has a step %d: a card is cut once, under a new bundle id", bundle, i+1)
		}
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return api.Errorf(api.CodeConflict, "step %s is not pending", id)
		}
	}
	return tx.Commit()
}

// returnSteps sends card bundle's carded steps back to pending.
func (s *Service) returnSteps(ctx context.Context, bundle string) error {
	_, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE person_steps SET state = 'pending', bundle = '', number = 0, ask = '', updated = ? WHERE bundle = ? AND state = 'carded'`,
		ms(s.now()), bundle)
	return err
}

// bundleSteps is the steps of the card id (its bundle id, or the ask that
// carries it), in card order.
func (s *Service) bundleSteps(ctx context.Context, id string) ([]stepRow, error) {
	id = strings.ToUpper(strings.TrimSpace(id))
	if id == "" {
		return nil, nil
	}
	return s.querySteps(ctx, `bundle = ? OR (ask = ? AND bundle != '') ORDER BY number, id`, id, id)
}

// ---- who may see and change a step ----

// callerPerson is the directory person sub is, in project, or "".
func (s *Service) callerPerson(ctx context.Context, sub *authz.Subject, project string) string {
	if sub == nil {
		return ""
	}
	name := ""
	if sub.Identity.Kind == api.IdentityHuman {
		name = sub.Identity.Name
	}
	if name == "" && sub.Via == authz.ViaLocal {
		name = s.d.Config.Human // this machine's own person, on its socket
	}
	if name == "" {
		return ""
	}
	return s.personOf(ctx, project, name)
}

// isStepPerson: sub is the person st is for (any of their sessions).
func (s *Service) isStepPerson(ctx context.Context, sub *authz.Subject, st stepRow) bool {
	if st.personID == "" {
		return false
	}
	return s.callerPerson(ctx, sub, st.Project) == st.personID
}

// mayReadStep: read on the step's area, and operate there, or its adder, or
// the person it is for. Anyone else gets exactly not_found.
func (s *Service) mayReadStep(ctx context.Context, sub *authz.Subject, st stepRow) bool {
	o := authz.Object{Project: st.Project, Area: st.Area}
	if sub == nil || !sub.Can(authz.Read, o) {
		return false
	}
	return sub.Can(authz.Operate, o) || st.AddedBy == sub.Address || s.isStepPerson(ctx, sub, st)
}

// mayChangeStep: its adder, or who may operate on its area.
func mayChangeStep(sub *authz.Subject, st stepRow) bool {
	return st.AddedBy == sub.Address || sub.Can(authz.Operate, authz.Object{Project: st.Project, Area: st.Area})
}

// visibleStep loads step id for sub: not_found unless sub may read it.
func (s *Service) visibleStep(ctx context.Context, sub *authz.Subject, id string) (stepRow, error) {
	st, err := s.step(ctx, id)
	if err == nil && !s.mayReadStep(ctx, sub, st) {
		err = api.NotFound("step", id)
	}
	return st, err
}

// stepView is st as sub reads it.
func (s *Service) stepView(ctx context.Context, sub *authz.Subject, st stepRow) orchapi.PersonStep {
	v := st.PersonStep
	v.AddedBy = s.d.RedactFor(ctx, sub, v.AddedBy)
	v.ReportedBy = s.d.RedactFor(ctx, sub, v.ReportedBy)
	v.CheckedBy = s.d.RedactFor(ctx, sub, v.CheckedBy)
	return v
}

// ---- routes ----

func (s *Service) stepRoutes(r *core.Router) {
	r.HandleFunc("POST /v0/steps", core.Authed, "write on the work item's area (WriteTarget, then the item); to names a member of the whole project, for a caller who reads all of it; draft: read on the step (else not_found), then its adder or operate on its area; from_ask: read on the ask (else not_found), then its asker or operate on its area; the step judge passes it (fail 400, no answer 503)", s.handleStepAdd)
	r.HandleFunc("GET /v0/steps", core.Authed, "ReadScope: read on the step's area, and operate there, its adder or the person it is for", s.handleSteps)
	r.HandleFunc("GET /v0/steps/{id}", core.Authed, "read on the step's area, and operate there, its adder or the person it is for, else not_found", s.handleGetStep)
	r.HandleFunc("POST /v0/steps/{id}/drop", core.Authed, "read on the step (else not_found), then its adder or operate on its area", s.handleStepDrop)
	r.HandleFunc("POST /v0/steps/{id}/check", core.Authed, "read on the step (else not_found), then its adder or operate on its area; records its check's exit", s.handleStepCheck)
	r.HandleFunc("GET /v0/bundles/{id}", core.Authed, "the card's steps the caller may read (as GET /v0/steps/{id}); none is not_found", s.handleBundle)
	r.HandleFunc("POST /v0/bundles/{id}/steps/{n}/report", core.Authed, "read on the step (else not_found), then the person it is for (any of their sessions) or operate on its area", s.handleStepReport)
}

// cleanStepText checks a step's title, brief and check against the plain
// limits (the step judge reads their meaning).
func cleanStepText(title, brief, check string) (string, string, string, error) {
	title = strings.TrimSpace(textclean.Clean(title))
	brief = strings.TrimSpace(textclean.Clean(brief))
	check = strings.TrimSpace(check)
	hint := "zm ask bundle add --to human:<name> --work <id> --title '<plain line>' --brief <file> --check '<shell line>'"
	switch {
	case title == "" || len(title) > orchapi.MaxPersonStepTitle || !utf8.ValidString(title) || unsafePresentation(title):
		return "", "", "", api.Errorf(api.CodeBadRequest, "title: one plain line of at most %d bytes", orchapi.MaxPersonStepTitle).WithHint(hint)
	case brief == "" || len(brief) > orchapi.MaxPersonStepBrief || !utf8.ValidString(brief):
		return "", "", "", api.Errorf(api.CodeBadRequest, "brief: the numbered steps to done, at most %d bytes (it has %d)", orchapi.MaxPersonStepBrief, len(brief)).WithHint(hint)
	case check == "" || len(check) > orchapi.MaxPersonStepCheck || !utf8.ValidString(check) || strings.ContainsAny(check, "\r\n\x00") || textclean.Clean(check) != check:
		return "", "", "", api.Errorf(api.CodeBadRequest, "check: one printable shell line of at most %d bytes that exits 0 only once the step is done", orchapi.MaxPersonStepCheck).WithHint(hint)
	}
	return title, brief, check, nil
}

func (s *Service) handleStepAdd(w http.ResponseWriter, r *http.Request) {
	ctx := withPeopleCache(r.Context())
	var req orchapi.StepAddRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	sub := core.SubjectOf(ctx)
	st, err := s.addStep(ctx, sub, req)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, s.stepView(ctx, sub, st))
}

// addStep judges and stores req: a new step, a completed draft, or a hand
// ask's brief and check (whose ask is then withdrawn).
func (s *Service) addStep(ctx context.Context, sub *authz.Subject, req orchapi.StepAddRequest) (stepRow, error) {
	var st stepRow
	var from *orchapi.Ask
	switch {
	case req.Draft != "" && req.FromAsk != "":
		return st, api.Errorf(api.CodeBadRequest, "draft or from_ask, not both")
	case (req.Draft != "" || req.FromAsk != "") && (req.To != "" || req.Work != ""):
		return st, api.Errorf(api.CodeBadRequest, "a draft or a hand ask already names its person and work item: leave out to and work")
	case req.FromAsk != "" && (req.Brief != "" || req.Check != ""):
		return st, api.Errorf(api.CodeBadRequest, "a hand ask's step takes its own brief and verify line: leave out brief and check")
	case req.Draft != "":
		d, err := s.visibleStep(ctx, sub, req.Draft)
		switch {
		case err != nil:
			return st, err
		case !mayChangeStep(sub, d):
			return st, api.Errorf(api.CodeForbidden, "only its adder, or who may operate on its area, completes a draft step")
		case d.State != orchapi.StepDraft:
			return st, api.Errorf(api.CodeConflict, "step %s is %s, not a draft", d.ID, d.State)
		}
		st = d
	case req.FromAsk != "":
		a, err := s.handAskForStep(ctx, sub, req.FromAsk)
		if err != nil {
			return st, err
		}
		from = &a
		if req.Title == "" {
			req.Title = a.Card.Title
		}
		req.Brief, req.Check = a.Card.Action.Brief, a.Card.Verify.Command
		st.Project, st.Area, st.Person, st.personID, st.Work = a.Project, a.Area, a.Owner, a.OwnerPerson, a.Subject.Work
		if st.personID == "" {
			st.personID = s.personOf(ctx, a.Project, a.Owner)
		}
	default:
		if err := s.newStepTarget(ctx, sub, req, &st); err != nil {
			return st, err
		}
	}
	title, brief, check, err := cleanStepText(req.Title, req.Brief, req.Check)
	if err != nil {
		return st, err
	}
	v := s.judgeStep(ctx, stepJudgeInput{Person: st.Person, Title: title, Brief: brief, Check: check}, StepJudgeWait)
	switch v.Status {
	case judge.Pass:
	case judge.Fail:
		e := api.Errorf(api.CodeBadRequest, "the step judge sent this step back: %s", numbered(v.Fixes))
		if isDecision(v.Verdict) {
			return st, e.WithHint("a real choice is its own card: zm ask new --to human:" + st.Person + " --work " + st.Work + " with choices and a recommendation")
		}
		return st, e.WithHint("rewrite it and add it again: zm ask bundle add ...")
	default:
		return st, api.Errorf(api.CodeUnavailable, "not added: the step judge could not answer (%s), and a step is never stored unchecked", v.Reason).
			WithHint("add it again in a minute; if it keeps failing, the authority has no provider CLI signed in (ZM_JUDGE)")
	}
	now := s.now()
	st.Title, st.Brief, st.Check, st.Risk, st.State, st.Updated = title, brief, check, v.Risk, orchapi.StepPending, now
	reg := s.d.Store.Registry()
	if req.Draft != "" {
		res, err := reg.ExecContext(ctx, `UPDATE person_steps SET title = ?, brief = ?, check_cmd = ?, risk = ?, state = 'pending', updated = ? WHERE id = ? AND state = 'draft'`,
			title, brief, check, v.Risk, ms(now), st.ID)
		if err != nil {
			return st, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return st, api.Errorf(api.CodeConflict, "step %s changed meanwhile", st.ID).WithHint("read it again: zm ask bundle ls")
		}
		return st, nil
	}
	st.ID, st.AddedBy, st.Created = ids.New(), sub.Address, now
	if _, err := reg.ExecContext(ctx, `INSERT INTO person_steps (id, project, area, person, person_id, added_by, work, title, brief, check_cmd, risk, state, created, updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?, ?)`, st.ID, st.Project, st.Area, st.Person, st.personID, st.AddedBy, st.Work,
		title, brief, check, v.Risk, ms(now), ms(now)); err != nil {
		return st, err
	}
	if from != nil {
		// The step replaces the ask: this call withdraws it, or the step goes
		// again (the ask was answered or withdrawn meanwhile, perhaps by
		// another move of the same ask).
		_, withdrew, err := s.withdraw(ctx, sub, *from)
		switch {
		case withdrew && err != nil: // withdrawn, only not read back: the step stands
			s.d.Log.Warn("approvals: read a hand ask back after moving it into a step", "ask", from.ID, "err", err)
			err = nil
		case !withdrew && err == nil:
			err = api.Errorf(api.CodeConflict, "ask %s was withdrawn meanwhile: nothing added", from.ID)
		}
		if err != nil {
			if _, derr := reg.ExecContext(context.WithoutCancel(ctx), `DELETE FROM person_steps WHERE id = ?`, st.ID); derr != nil {
				s.d.Log.Error("approvals: drop a step whose ask was not withdrawn", "step", st.ID, "err", derr)
			}
			return st, err
		}
	}
	return st, nil
}

// handAskForStep is ask id, when sub may move it into a step: an open,
// unanswered hand ask with a verify check, for one person, on a work item,
// whose asker sub is or whose area sub operates on.
func (s *Service) handAskForStep(ctx context.Context, sub *authz.Subject, id string) (orchapi.Ask, error) {
	a, err := s.ask(ctx, id)
	if err == nil && !mayRead(sub.Scope, sub.Address, a) {
		err = api.NotFound("ask", id)
	}
	if err != nil {
		return a, err
	}
	act := actionOf(a)
	switch {
	case a.AskedBy != sub.Address && !sub.Can(authz.Operate, authz.Object{Project: a.Project, Area: a.Area}):
		return a, api.Errorf(api.CodeForbidden, "only its asker, or who may operate on its area, moves an ask into a step")
	case act == nil || act.Kind != orchapi.ActionHand || verifyOf(a) == nil:
		return a, api.Errorf(api.CodeConflict, "ask %s is not a hand ask with a verify check", a.ID)
	case a.Decision != nil:
		return a, api.Errorf(api.CodeConflict, "ask %s was already answered", a.ID)
	case a.Withdrawn != nil:
		return a, api.Errorf(api.CodeConflict, "ask %s was withdrawn", a.ID)
	case a.Owner == "":
		return a, api.Errorf(api.CodeConflict, "ask %s names no one person", a.ID)
	case a.Subject.Work == "":
		return a, api.Errorf(api.CodeConflict, "ask %s is on no work item: add the step with --to and --work", a.ID)
	}
	return a, nil
}

// newStepTarget fills st's project, area, work item and person from req.
func (s *Service) newStepTarget(ctx context.Context, sub *authz.Subject, req orchapi.StepAddRequest, st *stepRow) error {
	work, to := strings.TrimSpace(req.Work), strings.TrimSpace(req.To)
	if work == "" || to == "" {
		return api.Errorf(api.CodeBadRequest, "a step names its person (to: human:<name>) and its work item (work)")
	}
	t, err := s.d.WriteTarget(ctx, "", authz.Write)
	if err != nil {
		return err
	}
	rec, ok := orchcore.Work(s.d).(orchcore.WorkRecorder)
	if !ok {
		return api.Errorf(api.CodeUnavailable, "the work module is not running")
	}
	canon, area, project, err := rec.ResolveItem(ctx, sub, t.Project.ID, work)
	if err != nil {
		return err
	}
	if err := sub.Check(authz.Write, authz.Object{Project: project, Area: area}, "work item", canon); err != nil {
		return err
	}
	if !sub.Scope.WholeProject(project) {
		return api.Errorf(api.CodeForbidden, "a step for a named person is for members of the whole project")
	}
	name := strings.TrimPrefix(strings.ToLower(to), "human:")
	if name == "" || strings.ContainsAny(name, ":/") || name == "owners" || name == "me" || name == "all" {
		return api.Errorf(api.CodeBadRequest, "to: one person, human:<name> (zm people lists them), not %q", req.To)
	}
	all, err := s.people(ctx, project)
	if err != nil {
		return err
	}
	var members []person
	for _, p := range all {
		if p.Reads {
			members = append(members, p)
		}
	}
	p, ident, err := resolvePerson(members, name)
	if err != nil {
		hint := "name one person by a name only they have (zm people lists them)"
		var ae *api.Error
		if errors.As(err, &ae) && ae.Hint != "" {
			hint = ae.Hint // the people it could mean
		}
		return api.Errorf(api.CodeBadRequest, "to %q: not one person who reads all of this project", req.To).WithHint(hint)
	}
	st.Project, st.Area, st.Work, st.Person, st.personID = project, area, canon, ident, p.ID
	return nil
}

func (s *Service) handleSteps(w http.ResponseWriter, r *http.Request) {
	ctx := withPeopleCache(r.Context())
	sub := core.SubjectOf(ctx)
	q := r.URL.Query()
	sc, err := s.d.ReadScope(ctx, q.Get("project"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	where, args := sc.SQL("project", "area", authz.Read)
	switch state := strings.TrimSpace(q.Get("state")); state {
	case "":
		where += ` AND state NOT IN ('done', 'dropped')`
	case "all":
	case orchapi.StepDraft, orchapi.StepPending, orchapi.StepCarded, orchapi.StepReported, orchapi.StepDone, orchapi.StepFailed, orchapi.StepDropped:
		where += ` AND state = ?`
		args = append(args, state)
	default:
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "state: all, draft, pending, carded, reported, done, failed or dropped (default: not done or dropped)"))
		return
	}
	// Who may read a step is in the query, so the limit counts only rows
	// the caller gets (a person id is the same in every project).
	operate, oargs := sc.SQL("project", "area", authz.Operate)
	where += ` AND ((` + operate + `) OR added_by = ? OR (person_id != '' AND person_id = ?))`
	args = append(append(args, oargs...), sub.Address, s.callerPerson(ctx, sub, ""))
	if to := strings.TrimPrefix(strings.ToLower(strings.TrimSpace(q.Get("to"))), "human:"); to != "" {
		where += ` AND (person = ? OR (person_id != '' AND person_id = ?))`
		args = append(args, to, s.personOf(ctx, "", to))
	}
	rows, err := s.querySteps(ctx, where+` ORDER BY created, id LIMIT 500`, args...)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	out := orchapi.StepList{Steps: []orchapi.PersonStep{}}
	for _, st := range rows {
		if s.mayReadStep(ctx, sub, st) {
			out.Steps = append(out.Steps, s.stepView(ctx, sub, st))
		}
	}
	api.WriteJSON(w, http.StatusOK, out)
}

func (s *Service) handleGetStep(w http.ResponseWriter, r *http.Request) {
	ctx := withPeopleCache(r.Context())
	sub := core.SubjectOf(ctx)
	st, err := s.visibleStep(ctx, sub, r.PathValue("id"))
	respond(w, s.stepView(ctx, sub, st), err)
}

func (s *Service) handleStepDrop(w http.ResponseWriter, r *http.Request) {
	ctx := withPeopleCache(r.Context())
	sub := core.SubjectOf(ctx)
	st, err := s.visibleStep(ctx, sub, r.PathValue("id"))
	switch {
	case err != nil:
	case !mayChangeStep(sub, st):
		err = api.Errorf(api.CodeForbidden, "only its adder, or who may operate on its area, drops a step")
	case st.State == orchapi.StepDone || st.State == orchapi.StepDropped:
		err = api.Errorf(api.CodeConflict, "step %s is already %s", st.ID, st.State)
	}
	if err == nil {
		err = s.moveStep(ctx, st, orchapi.StepDropped, `updated = ?`, ms(s.now()))
	}
	if err == nil {
		st, err = s.step(ctx, st.ID)
	}
	respond(w, s.stepView(ctx, sub, st), err)
}

// moveStep sets st's state to state (and set, a SET list with its args),
// only while it is still in the state it was read in.
func (s *Service) moveStep(ctx context.Context, st stepRow, state, set string, args ...any) error {
	args = append([]any{state}, args...)
	res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE person_steps SET state = ?, `+set+` WHERE id = ? AND state = ?`, append(args, st.ID, st.State)...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return api.Errorf(api.CodeConflict, "step %s changed meanwhile", st.ID).WithHint("read it again: zm ask bundle ls")
	}
	return nil
}

// handleStepCheck records the exit of a step's check as its adder ran it
// (zm ask bundle step-check): 0 makes it done; another exit after a done
// report makes it failed; else the step stays as it is.
func (s *Service) handleStepCheck(w http.ResponseWriter, r *http.Request) {
	ctx := withPeopleCache(r.Context())
	sub := core.SubjectOf(ctx)
	var req orchapi.StepCheckRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	st, err := s.visibleStep(ctx, sub, r.PathValue("id"))
	switch {
	case err != nil:
	case !mayChangeStep(sub, st):
		err = api.Errorf(api.CodeForbidden, "only its adder, or who may operate on its area, reports a step's check")
	case req.Exit == nil:
		err = api.Errorf(api.CodeBadRequest, "exit is required: the check's exit status")
	case st.State == orchapi.StepDraft:
		err = api.Errorf(api.CodeConflict, "step %s is a draft: complete it first (zm ask bundle add --draft %s)", st.ID, st.ID)
	case st.State == orchapi.StepDone || st.State == orchapi.StepDropped:
		err = api.Errorf(api.CodeConflict, "step %s is already %s", st.ID, st.State)
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	next := st.State
	switch {
	case *req.Exit == 0:
		next = orchapi.StepDone
	case st.State == orchapi.StepReported:
		next = orchapi.StepFailed
	}
	now := ms(s.now())
	if err := s.moveStep(ctx, st, next, `checked = ?, check_exit = ?, check_output = ?, checked_by = ?, updated = ?`,
		now, *req.Exit, checkOutput(req.Output), sub.Address, now); err != nil {
		api.WriteError(w, err)
		return
	}
	if next == orchapi.StepDone {
		s.stepComment(ctx, st, fmt.Sprintf("STEP DONE (verified by its check, run by %s): %s, person step %s for %s.", sub.Address, quoted(st.Title), st.ID, st.Person))
	}
	st, err = s.step(ctx, st.ID)
	respond(w, s.stepView(ctx, sub, st), err)
}

// visibleBundle is card id's steps sub may read (not_found when none) and
// whether every step of the card, read or not, is done or dropped.
func (s *Service) visibleBundle(ctx context.Context, sub *authz.Subject, id string) ([]stepRow, bool, error) {
	all, err := s.bundleSteps(ctx, id)
	if err != nil {
		return nil, false, err
	}
	var out []stepRow
	done := true
	for _, st := range all {
		done = done && (st.State == orchapi.StepDone || st.State == orchapi.StepDropped)
		if s.mayReadStep(ctx, sub, st) {
			out = append(out, st)
		}
	}
	if len(out) == 0 {
		return nil, false, api.NotFound("bundle", id)
	}
	return out, done, nil
}

// handleBundle is a card's steps the caller may read; done is the whole
// card's, so a reader of some of it never sees an unfinished card done.
func (s *Service) handleBundle(w http.ResponseWriter, r *http.Request) {
	ctx := withPeopleCache(r.Context())
	sub := core.SubjectOf(ctx)
	steps, done, err := s.visibleBundle(ctx, sub, r.PathValue("id"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	out := orchapi.Bundle{ID: steps[0].Bundle, Ask: steps[0].Ask, Steps: []orchapi.PersonStep{}, Done: done}
	for _, st := range steps {
		out.Steps = append(out.Steps, s.stepView(ctx, sub, st))
	}
	api.WriteJSON(w, http.StatusOK, out)
}

// handleStepReport is the person's agent saying step n of a card is done
// or failed: it is recorded, commented on the step's work item, and its
// adder is mailed to run the check. Only the check makes it done.
func (s *Service) handleStepReport(w http.ResponseWriter, r *http.Request) {
	ctx := withPeopleCache(r.Context())
	sub := core.SubjectOf(ctx)
	var req orchapi.StepReportRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	id := r.PathValue("id")
	steps, _, err := s.visibleBundle(ctx, sub, id)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	n, _ := strconv.Atoi(r.PathValue("n"))
	var st stepRow
	for _, x := range steps {
		if x.Number == n {
			st = x
		}
	}
	note := strings.TrimSpace(textclean.Clean(req.Note))
	result := strings.ToLower(strings.TrimSpace(req.Result))
	switch {
	case st.ID == "":
		err = api.NotFound("step", fmt.Sprintf("%s %d", id, n))
	case !s.isStepPerson(ctx, sub, st) && !sub.Can(authz.Operate, authz.Object{Project: st.Project, Area: st.Area}):
		err = api.Errorf(api.CodeForbidden, "only %s's agent, or who may operate on its area, reports a step", st.Person)
	case result != orchapi.StepDone && result != orchapi.StepFailed:
		err = api.Errorf(api.CodeBadRequest, "result: done or failed")
	case note == "" || len(note) > orchapi.MaxPersonStepNote || strings.ContainsAny(note, "\r\n") || unsafePresentation(note):
		err = api.Errorf(api.CodeBadRequest, "note: one plain line of at most %d bytes saying what happened", orchapi.MaxPersonStepNote)
	case st.State != orchapi.StepCarded && st.State != orchapi.StepReported && st.State != orchapi.StepFailed:
		err = api.Errorf(api.CodeConflict, "step %d of card %s is %s", n, id, st.State)
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	next := orchapi.StepReported
	if result == orchapi.StepFailed {
		next = orchapi.StepFailed
	}
	now := ms(s.now())
	if err := s.moveStep(ctx, st, next, `report_note = ?, reported_by = ?, reported = ?, updated = ?`, note, sub.Address, now, now); err != nil {
		api.WriteError(w, err)
		return
	}
	who := first(sub.Identity.Name, sub.Address)
	if result == orchapi.StepDone {
		s.stepComment(ctx, st, fmt.Sprintf("STEP %d REPORTED DONE by %s: %s (person step %s for %s): %s. Its adder runs the check: zm ask bundle step-check %s", n, who, quoted(st.Title), st.ID, st.Person, note, st.ID))
		s.stepMail(ctx, st, fmt.Sprintf("STEP REPORTED DONE: %s's agent says step %s is done (%s): %s. Run its check now: zm ask bundle step-check %s", st.Person, st.ID, quoted(st.Title), note, st.ID))
	} else {
		s.stepComment(ctx, st, fmt.Sprintf("STEP %d FAILED, reported by %s: %s (person step %s for %s): %s", n, who, quoted(st.Title), st.ID, st.Person, note))
		s.stepMail(ctx, st, fmt.Sprintf("STEP FAILED: %s's agent could not do step %s (%s): %s. If it is done another way, run zm ask bundle step-check %s; else drop it (zm ask bundle drop %s) and add a better one", st.Person, st.ID, quoted(st.Title), note, st.ID, st.ID))
	}
	st, err = s.step(ctx, st.ID)
	respond(w, s.stepView(ctx, sub, st), err)
}

// stepComment adds text to st's work item as the authority. Best effort: the
// step's own record is the state.
func (s *Service) stepComment(ctx context.Context, st stepRow, text string) {
	ctx = context.WithoutCancel(ctx)
	rec, ok := orchcore.Work(s.d).(orchcore.WorkRecorder)
	if !ok {
		return
	}
	if err := rec.RecordComment(ctx, s.d.Projects.Local(""), st.Project, st.Work, ids.New(), text); err != nil {
		s.d.Log.Warn("approvals: comment a person step on its work item", "step", st.ID, "err", err)
	}
}

// stepMail mails st's adder (an agent session, else its work item's holder)
// text. Best effort.
func (s *Service) stepMail(ctx context.Context, st stepRow, text string) {
	ctx = context.WithoutCancel(ctx)
	local := s.d.Projects.Local("")
	to := ""
	if orchapi.AgentSender(st.AddedBy) {
		to = st.AddedBy
	} else {
		to, _, _, _ = orchcore.Work(s.d).MailTarget(ctx, local, st.Project, st.Work)
	}
	if to == "" {
		return
	}
	if _, err := orchcore.Mail(s.d).Send(ctx, local, api.ProjectRef{ID: st.Project, Slug: s.d.Projects.Slug(st.Project)}, st.Area,
		orchapi.SendMailRequest{ID: ids.New(), To: to, Body: text, Ref: st.Work}); err != nil {
		s.d.Log.Warn("approvals: mail a person step's adder", "step", st.ID, "err", err)
	}
}
