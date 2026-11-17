package approvals

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

// Standing permissions (SPEC 16 "Standing permissions"). A
// permit covers one kind of act zm performs itself, bound at each use to
// its exact target (the work item, the machine, the commit or brief). The
// kinds and box classes below are the closed list; their text is what an
// owner reads on the card that grants one. No model is in this path: every
// condition is a fact from zm's own records, and a fact zm cannot read is a
// condition that fails.
//
// Stage 1 is a dry run: zm records, for every new card with an action,
// whether a permit of each dry-run kind would have covered it. It grants
// nothing by itself and skips no card; the weekly report (PermitDryRun)
// compares those records with how a person answered.

var permitMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0013_permits", Script: `
CREATE TABLE permits (
	id TEXT PRIMARY KEY,
	project TEXT NOT NULL,
	kind TEXT NOT NULL,
	kind_sha TEXT NOT NULL,
	ask TEXT NOT NULL UNIQUE,
	approver TEXT NOT NULL,
	granted_by TEXT NOT NULL,
	created INTEGER NOT NULL,
	expires INTEGER NOT NULL,
	revoked INTEGER,
	revoked_by TEXT NOT NULL DEFAULT '',
	replaced_by TEXT NOT NULL DEFAULT ''
);
CREATE INDEX permits_project ON permits (project, kind);
CREATE TABLE box_marks (
	project TEXT NOT NULL,
	machine TEXT NOT NULL,
	class TEXT NOT NULL,
	ask TEXT NOT NULL UNIQUE,
	approver TEXT NOT NULL,
	set_at INTEGER NOT NULL,
	PRIMARY KEY (project, machine, class)
);
CREATE TABLE permit_dryrun (
	ask TEXT NOT NULL,
	kind TEXT NOT NULL,
	project TEXT NOT NULL,
	kind_sha TEXT NOT NULL,
	covers INTEGER NOT NULL,
	takes TEXT NOT NULL DEFAULT '',
	failed TEXT NOT NULL DEFAULT '',
	target TEXT NOT NULL DEFAULT '',
	at INTEGER NOT NULL,
	PRIMARY KEY (ask, kind)
);
CREATE INDEX permit_dryrun_at ON permit_dryrun (project, at);
CREATE TABLE permit_asks (
	ask TEXT PRIMARY KEY,
	used INTEGER NOT NULL
);
CREATE TABLE permit_narrowed (
	project TEXT NOT NULL,
	target TEXT NOT NULL,
	at INTEGER NOT NULL
);
CREATE INDEX permit_narrowed_target ON permit_narrowed (project, target, at);
`},
}

// Choice keys of a permit card: only yes grants.
const (
	permitYes = "yes"
	permitNo  = "no"
	// maxPermitDays bounds a grant's days (a syntax bound; the proposer
	// picks the days and the owner reads them on the card).
	maxPermitDays = 365
	// classNoSecrets is the box class machine-pick reads.
	classNoSecrets = "no-secrets"
)

// permitFacts is what the conditions read, loaded on first use. Every
// lookup that fails leaves its fact unknown, and a condition on an unknown
// fact fails.
type permitFacts struct {
	s    *Service
	sub  *authz.Subject
	ask  orchapi.Ask
	card orchapi.AskCard

	item     *orchapi.WorkItem
	itemDone bool
}

func (f *permitFacts) action() *orchapi.AskAction { return f.card.Action }

// workItem is the card's work item as the asker reads it now, or nil.
func (f *permitFacts) workItem(ctx context.Context) *orchapi.WorkItem {
	if f.itemDone {
		return f.item
	}
	f.itemDone = true
	if f.ask.Subject.Work == "" {
		return nil
	}
	rd, ok := orchcore.Work(f.s.d).(orchcore.WorkItemReader)
	if !ok {
		return nil
	}
	it, err := rd.ReadItem(ctx, f.sub, f.ask.Project, f.ask.Subject.Work)
	if err != nil {
		f.s.d.Log.Debug("approvals: permit fact: work item", "ask", f.ask.ID, "err", err)
		return nil
	}
	f.item = &it
	return f.item
}

// permitCondition is one condition: its id, its version (bumped by hand
// whenever check changes what it accepts, so a permit granted before is
// void), the text an owner reads, and the check.
type permitCondition struct {
	id      string
	version int
	text    string
	check   func(ctx context.Context, f *permitFacts) bool
}

var permitConditions = map[string]permitCondition{
	"zm-runs-it": {id: "zm-runs-it", version: 1,
		text: "zm itself starts the work, on the one machine the card names",
		check: func(_ context.Context, f *permitFacts) bool {
			a := f.action()
			return a != nil && (a.Kind == orchapi.ActionRun || a.Kind == orchapi.ActionCommand) && a.Machine != ""
		}},
	"agent-task": {id: "agent-task", version: 1,
		text: "what zm starts is an agent working from the brief on the card",
		check: func(_ context.Context, f *permitFacts) bool {
			a := f.action()
			return a != nil && a.Kind == orchapi.ActionRun && a.Agent != "" && a.Brief != "" && a.Machine != ""
		}},
	"work-open": {id: "work-open", version: 1,
		text: "the card names a work item zm can read now, from a current copy, that is not done",
		check: func(ctx context.Context, f *permitFacts) bool {
			it := f.workItem(ctx)
			if it == nil || it.Stale != "" {
				return false
			}
			switch it.Status {
			case orchapi.WorkOpen, orchapi.WorkClaimed, orchapi.WorkInReview:
				return true
			}
			return false
		}},
	"work-planned": {id: "work-planned", version: 1,
		text: "that work item was planned before the card was asked, by someone other than the asker",
		check: func(ctx context.Context, f *permitFacts) bool {
			it := f.workItem(ctx)
			return it != nil && !it.Created.IsZero() && it.Created.Before(f.ask.Created) &&
				it.CreatedBy != "" && it.CreatedBy != f.ask.AskedBy
		}},
	"pinned-work": {id: "pinned-work", version: 1,
		text: "what zm starts is fixed on the card: an agent's brief, or a script at one exact commit",
		check: func(_ context.Context, f *permitFacts) bool {
			a := f.action()
			return a != nil && a.Kind == orchapi.ActionRun && (a.Agent != "" && a.Brief != "" || a.Script != "" && a.Commit != "")
		}},
	"box-no-secrets": {id: "box-no-secrets", version: 1,
		text: "an owner marked that machine as holding no secrets, and the mark is still on",
		check: func(ctx context.Context, f *permitFacts) bool {
			a := f.action()
			if a == nil || a.Machine == "" {
				return false
			}
			marked, err := f.s.boxMarked(ctx, f.ask.Project, a.Machine, classNoSecrets)
			return err == nil && marked
		}},
}

// permitKindDef is one kind: its name, a plain title for its card, the
// frozen text and its conditions, in order.
type permitKindDef struct {
	name, title, text string
	conditions        []string
	dryRun            bool
}

// permitKinds is the closed list. A new kind is a code change plus one
// owner card that grants it.
var permitKinds = []permitKindDef{
	{name: "lane-start", title: "Start agents on planned work", dryRun: true,
		text:       "zm may start an agent on a piece of work that was already planned, on a machine of this project, when a card asks for exactly that. It covers starting the agent, not what the agent then does.",
		conditions: []string{"zm-runs-it", "agent-task", "work-open", "work-planned"}},
	{name: "machine-pick", title: "Use boxes that hold no secrets", dryRun: true,
		text:       "zm may place work it starts on any box an owner marked as holding no secrets, without asking which box.",
		conditions: []string{"zm-runs-it", "pinned-work", "box-no-secrets"}},
}

// boxClasses is the closed list of classes an owner may put on a machine.
var boxClasses = []orchapi.BoxClass{
	{Name: classNoSecrets, Text: "This box holds no secrets: no keys, tokens or signed-in accounts that matter. Any work placed on it may read everything on it."},
}

func permitKindByName(name string) (permitKindDef, bool) {
	for _, k := range permitKinds {
		if k.name == name {
			return k, true
		}
	}
	return permitKindDef{}, false
}

func boxClassByName(name string) (orchapi.BoxClass, bool) {
	for _, c := range boxClasses {
		if c.Name == name {
			return c, true
		}
	}
	return orchapi.BoxClass{}, false
}

// view is k as clients read it, with its SHA over everything a grant binds.
func (k permitKindDef) view() orchapi.PermitKind {
	out := orchapi.PermitKind{Name: k.name, Text: k.text, DryRun: k.dryRun, Conditions: []orchapi.PermitCondition{}}
	h := []string{"zm-permit-kind/1", k.name, k.title, k.text}
	for _, id := range k.conditions {
		c := permitConditions[id]
		out.Conditions = append(out.Conditions, orchapi.PermitCondition{ID: c.id, Text: c.text})
		h = append(h, fmt.Sprintf("%s/%d/%s", c.id, c.version, c.text))
	}
	out.SHA = textSHA(strings.Join(h, "\n"))
	return out
}

// evaluate checks k's conditions in order: the first that fails, or "".
func (k permitKindDef) evaluate(ctx context.Context, f *permitFacts) string {
	for _, id := range k.conditions {
		c, ok := permitConditions[id]
		if !ok || !c.check(ctx, f) {
			return id
		}
	}
	return ""
}

// permitTarget is what a use would be bound to.
type permitTarget struct {
	Work     string `json:"work,omitempty"`
	Machine  string `json:"machine,omitempty"`
	Commit   string `json:"commit,omitempty"`
	BriefSHA string `json:"brief_sha256,omitempty"`
}

// permitDryRun records, for a new card with an action that zm runs, whether
// a permit of each dry-run kind would have covered it. It never changes how
// the ask goes: errors are logged, and nothing is skipped.
func (s *Service) permitDryRun(ctx context.Context, sub *authz.Subject, a orchapi.Ask, cardJSON string) {
	if testHookPermitDryRun != nil {
		testHookPermitDryRun(a.ID)
	}
	if cardJSON == "" || a.SameAs != "" {
		return
	}
	var card orchapi.AskCard
	if err := json.Unmarshal([]byte(cardJSON), &card); err != nil || card.Action == nil {
		return
	}
	if k := card.Action.Kind; k != orchapi.ActionRun && k != orchapi.ActionCommand {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), permitFactsWithin)
	defer cancel()
	f := &permitFacts{s: s, sub: sub, ask: a, card: card}
	act := card.Action
	target := marshal(permitTarget{Work: a.Subject.Work, Machine: act.Machine, Commit: act.Commit, BriefSHA: act.BriefSHA})
	// takes is the choice a permit would have taken for the person: the
	// action's (default the first).
	takes := act.On
	if takes == "" && len(card.Choices) > 0 {
		takes = first(card.Choices[0].Key, "a")
	}
	for _, k := range permitKinds {
		if !k.dryRun {
			continue
		}
		failed := k.evaluate(ctx, f)
		if ctx.Err() != nil { // a fact read cut short is unread: would not cover
			failed = first(failed, "unread")
		}
		covers := failed == ""
		if _, err := s.d.Store.Registry().ExecContext(context.WithoutCancel(ctx), `INSERT OR IGNORE INTO permit_dryrun (ask, kind, project, kind_sha, covers, takes, failed, target, at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, a.ID, k.name, a.Project, k.view().SHA, covers, takes, failed, target, ms(a.Created)); err != nil {
			s.d.Log.Warn("approvals: record a permit dry run", "ask", a.ID, "kind", k.name, "err", err)
			continue
		}
		if covers {
			s.d.Log.Info("approvals: permit dry run: this card would have been covered", "ask", a.ID, "kind", k.name, "project", a.Project)
		}
	}
}

// testHookPermitDryRun, when set, sees every ask the dry run is offered.
var testHookPermitDryRun func(ask string)

// permitFactsWithin bounds the dry run's fact reads for one card.
var permitFactsWithin = 3 * time.Second

// boxMarked reports whether an owner put class on machine in project.
func (s *Service) boxMarked(ctx context.Context, project, machine, class string) (bool, error) {
	var n int
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT count(*) FROM box_marks WHERE project = ? AND machine = ? AND class = ?`,
		project, machine, class).Scan(&n)
	return n > 0, err
}

// PermitDryRun is the weekly report's row per dry-run kind for project,
// over cards asked in [from, to).
func (s *Service) PermitDryRun(ctx context.Context, project string, from, to time.Time) ([]orchapi.PermitDryRunRow, error) {
	rows := map[string]*orchapi.PermitDryRunRow{}
	out := []orchapi.PermitDryRunRow{}
	for _, k := range permitKinds {
		if k.dryRun {
			rows[k.name] = &orchapi.PermitDryRunRow{Kind: k.name}
		}
	}
	q, err := s.d.Store.Registry().QueryContext(ctx, `SELECT p.kind, p.covers, p.failed, coalesce(d.decision, ''),
		p.takes, a.withdrawn IS NOT NULL
		FROM permit_dryrun p JOIN asks a ON a.id = p.ask LEFT JOIN decisions d ON d.ask = p.ask
		WHERE p.project = ? AND p.at >= ? AND p.at < ?`, project, ms(from), ms(to))
	if err != nil {
		return nil, err
	}
	defer q.Close()
	for q.Next() {
		var kind, failed, decision, takes string
		var covers, withdrawn bool
		if err := q.Scan(&kind, &covers, &failed, &decision, &takes, &withdrawn); err != nil {
			return nil, err
		}
		r := rows[kind]
		if r == nil {
			r = &orchapi.PermitDryRunRow{Kind: kind}
			rows[kind] = r
		}
		r.Checked++
		if !covers {
			if r.NotCovered == nil {
				r.NotCovered = map[string]int{}
			}
			r.NotCovered[failed]++
			continue
		}
		r.WouldCover++
		switch {
		case decision != "" && decision == takes:
			r.Saved++ // the person picked what the permit would have run
		case decision != "":
			r.WronglyCovered++ // no, another choice, free text or sent back
		case withdrawn:
			r.Withdrawn++
		default:
			r.Open++
		}
	}
	if err := q.Err(); err != nil {
		return nil, err
	}
	for _, k := range permitKinds {
		if r := rows[k.name]; r != nil {
			out = append(out, *r)
			delete(rows, k.name)
		}
	}
	for _, r := range rows { // a kind no longer in the list still reports its window
		out = append(out, *r)
	}
	return out, nil
}

// permitCard is the card that grants req's kind for its days, or puts its
// class on its machine, with recommend, why and searched left to the
// asker.
func permitCard(req orchapi.PermitCardRequest) (orchapi.AskCard, error) {
	bad := func(format string, a ...any) error {
		return api.Errorf(api.CodeBadRequest, format, a...).
			WithHint("zm permit ask <kind> --days N, or zm permit ask --box <machine> --class no-secrets (zm permit list shows the kinds)")
	}
	switch {
	case req.Kind != "" && (req.Machine != "" || req.Class != ""):
		return orchapi.AskCard{}, bad("a permit card is for a kind or for a box, not both")
	case req.Kind != "":
		k, ok := permitKindByName(req.Kind)
		if !ok {
			return orchapi.AskCard{}, bad("no permit kind %q", req.Kind)
		}
		if req.Days < 1 || req.Days > maxPermitDays {
			return orchapi.AskCard{}, bad("days: 1 to %d", maxPermitDays)
		}
		var conds []string
		for _, id := range k.conditions {
			conds = append(conds, permitConditions[id].text)
		}
		days := fmt.Sprintf("%d days", req.Days)
		if req.Days == 1 {
			days = "1 day"
		}
		return orchapi.AskCard{
			Title: "Standing permission: " + strings.ToLower(k.title[:1]) + k.title[1:],
			Context: k.text + " Each time, zm checks from its own records that " + strings.Join(conds, "; ") +
				". If any of that cannot be read, zm asks with a card as now. It lasts " + days +
				"; an owner or admin of the project can revoke it at any time with one press.",
			Question: "Let zm do this without a card each time, for the next " + days + "?",
			Choices: []orchapi.AskChoice{
				{Key: permitYes, Label: "Yes, for " + days},
				{Key: permitNo, Label: "No, keep asking each time"},
			},
		}, nil
	case req.Machine != "" || req.Class != "":
		c, ok := boxClassByName(req.Class)
		if !ok {
			return orchapi.AskCard{}, bad("no box class %q", req.Class)
		}
		if !plainArg.MatchString(req.Machine) || len(req.Machine) > maxActionField || req.Days != 0 {
			return orchapi.AskCard{}, bad("a box card names one machine by its name, and no days")
		}
		return orchapi.AskCard{
			Title:    "Mark a box as holding no secrets",
			Context:  c.Text + " The box is the machine named " + req.Machine + ". zm reads this mark when it chooses where to place work; an owner or admin of the project can take it off at any time.",
			Question: "Mark the machine named " + req.Machine + " as holding no secrets?",
			Choices: []orchapi.AskChoice{
				{Key: permitYes, Label: "Yes, it holds no secrets"},
				{Key: permitNo, Label: "No"},
			},
		}, nil
	}
	return orchapi.AskCard{}, bad("name a kind and days, or a machine and a class")
}

// sameFrozen reports whether got (an ask's card) shows exactly what want
// (permitCard, normalised as a filed card is) shows a person.
func sameFrozen(got *orchapi.AskCard, want orchapi.AskCard) bool {
	if got == nil || got.Action != nil || got.Verify != nil || got.Background != "" {
		return false
	}
	want.Recommend, want.Why = permitNo, "x"
	want.Searched = strings.Repeat("x ", minSearched+1)
	norm, _, err := cleanCard(want, nil)
	if err != nil || got.Title != norm.Title || got.Context != norm.Context || got.Question != norm.Question || len(got.Choices) != len(norm.Choices) {
		return false
	}
	for i, ch := range got.Choices {
		w := norm.Choices[i]
		if ch.Key != w.Key || ch.Label != w.Label || ch.Pros != "" || ch.Cons != "" {
			return false
		}
	}
	return true
}

func (s *Service) handlePermitCard(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := orchapi.PermitCardRequest{Kind: q.Get("kind"), Machine: q.Get("machine"), Class: q.Get("class")}
	if v := q.Get("days"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &req.Days); err != nil {
			api.WriteError(w, api.Errorf(api.CodeBadRequest, "days: a whole number"))
			return
		}
	}
	c, err := permitCard(req)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, c)
}

// permitProject is the caller's project for the permit routes: the whole
// project, operated on (permits and box marks are about all of it).
func (s *Service) permitProject(ctx context.Context) (api.ProjectRef, error) {
	t, err := s.d.WriteTarget(ctx, "", authz.Operate)
	if err != nil {
		return api.ProjectRef{}, err
	}
	if !core.SubjectOf(ctx).Scope.WholeProject(t.Project.ID) {
		return api.ProjectRef{}, api.Errorf(api.CodeForbidden, "standing permissions are for members of the whole project")
	}
	return t.Project, nil
}

func (s *Service) handlePermits(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.permitProject(ctx)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	out, err := s.permitList(ctx, p.ID)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	out.Project = p.Slug
	for i := range out.Permits {
		out.Permits[i] = s.redactPermit(ctx, core.SubjectOf(ctx), out.Permits[i])
	}
	api.WriteJSON(w, http.StatusOK, out)
}

// redactPermit hides the session addresses on p from who may not see them.
func (s *Service) redactPermit(ctx context.Context, sub *authz.Subject, p orchapi.Permit) orchapi.Permit {
	p.GrantedBy, p.RevokedBy = s.d.Redact(ctx, sub, p.GrantedBy), s.d.Redact(ctx, sub, p.RevokedBy)
	return p
}

func (s *Service) permitList(ctx context.Context, project string) (orchapi.PermitList, error) {
	out := orchapi.PermitList{Kinds: []orchapi.PermitKind{}, Classes: boxClasses, Permits: []orchapi.Permit{}, Boxes: []orchapi.BoxMark{}}
	for _, k := range permitKinds {
		out.Kinds = append(out.Kinds, k.view())
	}
	reg := s.d.Store.Registry()
	rows, err := reg.QueryContext(ctx, `SELECT `+permitCols+` FROM permits WHERE project = ? ORDER BY id DESC LIMIT 200`, project)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := s.scanPermit(rows)
		if err != nil {
			return out, err
		}
		out.Permits = append(out.Permits, p)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	b, err := reg.QueryContext(ctx, `SELECT project, machine, class, ask, approver, set_at FROM box_marks WHERE project = ? ORDER BY machine, class`, project)
	if err != nil {
		return out, err
	}
	defer b.Close()
	for b.Next() {
		var m orchapi.BoxMark
		var at int64
		if err := b.Scan(&m.Project, &m.Machine, &m.Class, &m.Ask, &m.Approver, &at); err != nil {
			return out, err
		}
		m.Set = store.FromMillis(at)
		out.Boxes = append(out.Boxes, m)
	}
	return out, b.Err()
}

const permitCols = `id, project, kind, kind_sha, ask, approver, granted_by, created, expires, revoked, revoked_by, replaced_by`

func (s *Service) scanPermit(sc interface{ Scan(...any) error }) (orchapi.Permit, error) {
	var p orchapi.Permit
	var created, expires int64
	var revoked sql.NullInt64
	if err := sc.Scan(&p.ID, &p.Project, &p.Kind, &p.KindSHA, &p.Ask, &p.Approver, &p.GrantedBy, &created, &expires, &revoked, &p.RevokedBy, &p.ReplacedBy); err != nil {
		return p, err
	}
	p.Created, p.Expires = store.FromMillis(created), store.FromMillis(expires)
	k, known := permitKindByName(p.Kind)
	switch {
	case revoked.Valid:
		t := store.FromMillis(revoked.Int64)
		p.Revoked, p.State = &t, orchapi.PermitRevoked
	case p.ReplacedBy != "":
		p.State = orchapi.PermitReplaced
	case !known || k.view().SHA != p.KindSHA:
		p.State = orchapi.PermitVoid
	case !s.now().Before(p.Expires):
		p.State = orchapi.PermitExpired
	default:
		p.State = orchapi.PermitLive
	}
	return p, nil
}

// handleGrant makes a permit, or a box mark, from an ask a person answered
// yes on a paired device or with a key (a signed answer: the owner's socket
// answer, which every process of their OS user reaches, never counts). The
// ask's card must be exactly the one permitCard makes for the request, and
// the person must still own or administer the project. A grant for a kind
// with a live permit replaces it (renewing or widening).
func (s *Service) handleGrant(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	var req orchapi.PermitGrantRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	want, err := permitCard(req.PermitCardRequest)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	a, err := s.ask(ctx, strings.TrimSpace(req.Ask))
	if err != nil || !sub.Can(authz.Operate, authz.Object{Project: a.Project}) || !sub.Scope.WholeProject(a.Project) {
		if err != nil && !api.IsCode(err, api.CodeNotFound) {
			api.WriteError(w, err)
			return
		}
		api.WriteError(w, api.NotFound("ask", req.Ask))
		return
	}
	refuse := func(format string, args ...any) {
		api.WriteError(w, api.Errorf(api.CodeConflict, format, args...).
			WithHint("file the card from zm permit ask unchanged, and have an owner answer yes on a paired phone or in the Mezze app"))
	}
	switch {
	case !sameFrozen(a.Card, want):
		refuse("ask %s is not the card for this grant", a.ID)
		return
	case a.Withdrawn != nil:
		refuse("ask %s was withdrawn", a.ID)
		return
	case a.Decision == nil:
		refuse("ask %s has no answer yet", a.ID)
		return
	case a.Decision.Decision != permitYes || a.Decision.Text != "" ||
		a.Decision.AnsweredAs != "" && a.Decision.AnsweredAs != orchapi.AnsweredAsOwner:
		refuse("ask %s was not answered with a plain yes by an owner speaking for themselves", a.ID)
		return
	case a.Decision.Method != orchapi.MethodApprover:
		refuse("ask %s was answered without a signature: a standing permission takes an answer on a paired device or with a key", a.ID)
		return
	}
	why, err := s.approverAdmin(ctx, a.Decision.Approver, a.Decision.Receipt.Node, ms(a.Decision.Time), a.Project)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if why != "" {
		refuse("%s, who answered, %s", a.Decision.Approver, why)
		return
	}
	if req.Machine != "" {
		known, err := permitMachineKnown(ctx, s, a.Project, req.Machine)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		if !known {
			refuse("%s is not a machine of this project now", req.Machine)
			return
		}
	}
	now := s.now().Truncate(time.Millisecond)
	answered := a.Decision.Time.Truncate(time.Millisecond)
	target := narrowTarget(req.PermitCardRequest)
	tx, err := s.d.Store.Registry().BeginTx(ctx, nil)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	defer tx.Rollback()
	// One signed yes is used once, ever (a mark taken off or a permit
	// revoked cannot be brought back with the answer that made it), and an
	// answer given before the latest revoke or unmark of its target no longer
	// counts.
	if _, err := tx.ExecContext(ctx, `INSERT INTO permit_asks (ask, used) VALUES (?, ?)`, a.ID, ms(now)); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "PRIMARY") {
			refuse("ask %s was already used for a grant", a.ID)
			return
		}
		api.WriteError(w, err)
		return
	}
	var narrowed int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM permit_narrowed WHERE project = ? AND target = ? AND at >= ?`,
		a.Project, target, ms(answered)).Scan(&narrowed); err != nil {
		api.WriteError(w, err)
		return
	}
	if narrowed > 0 {
		refuse("ask %s was answered before an owner last revoked this or took it off: it needs a new answer", a.ID)
		return
	}
	if req.Kind != "" { // an older answer never replaces a newer grant
		var newer int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM permits WHERE project = ? AND kind = ? AND created >= ?`,
			a.Project, req.Kind, ms(answered)).Scan(&newer); err != nil {
			api.WriteError(w, err)
			return
		}
		if newer > 0 {
			refuse("ask %s was answered before the kind's latest grant: it needs a new answer", a.ID)
			return
		}
	}
	var out orchapi.PermitGrant
	if req.Kind != "" {
		k, _ := permitKindByName(req.Kind)
		p := orchapi.Permit{ID: ids.New(), Project: a.Project, Kind: k.name, KindSHA: k.view().SHA, Ask: a.ID, Approver: a.Decision.Approver,
			GrantedBy: sub.Address, Created: now, Expires: answered.Add(time.Duration(req.Days) * 24 * time.Hour), State: orchapi.PermitLive}
		if !now.Before(p.Expires) {
			refuse("ask %s was answered more than %d days ago: its permission is over", a.ID, req.Days)
			return
		}
		if _, err := tx.ExecContext(ctx, `UPDATE permits SET replaced_by = ? WHERE project = ? AND kind = ? AND revoked IS NULL AND replaced_by = ''`,
			p.ID, p.Project, p.Kind); err != nil {
			api.WriteError(w, err)
			return
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO permits (`+permitCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, '', '')`,
			p.ID, p.Project, p.Kind, p.KindSHA, p.Ask, p.Approver, p.GrantedBy, ms(p.Created), ms(p.Expires)); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				refuse("ask %s already granted a permit", a.ID)
				return
			}
			api.WriteError(w, err)
			return
		}
		p = s.redactPermit(ctx, sub, p)
		out.Permit = &p
	} else {
		m := orchapi.BoxMark{Project: a.Project, Machine: req.Machine, Class: req.Class, Ask: a.ID, Approver: a.Decision.Approver, Set: now}
		if _, err := tx.ExecContext(ctx, `INSERT INTO box_marks (project, machine, class, ask, approver, set_at) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (project, machine, class) DO UPDATE SET ask = excluded.ask, approver = excluded.approver, set_at = excluded.set_at`,
			m.Project, m.Machine, m.Class, m.Ask, m.Approver, ms(now)); err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				refuse("ask %s already marked a box", a.ID)
				return
			}
			api.WriteError(w, err)
			return
		}
		out.Box = &m
	}
	if err := tx.Commit(); err != nil {
		api.WriteError(w, err)
		return
	}
	s.d.Log.Info("approvals: standing permission granted", "ask", a.ID, "kind", req.Kind, "machine", req.Machine, "class", req.Class, "approver", a.Decision.Approver)
	api.WriteJSON(w, http.StatusOK, out)
}

// narrowTarget is what a revoke or unmark narrows, as permit_narrowed
// keys it: a kind, or a class on a machine.
func narrowTarget(req orchapi.PermitCardRequest) string {
	if req.Kind != "" {
		return "kind:" + req.Kind
	}
	return "box:" + req.Class + ":" + req.Machine
}

// narrow runs change and records that target was narrowed at now, in one
// transaction: answers given before it no longer grant (handleGrant).
func (s *Service) narrow(ctx context.Context, project, target string, now time.Time, change string, args ...any) error {
	tx, err := s.d.Store.Registry().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, change, args...); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO permit_narrowed (project, target, at) VALUES (?, ?, ?)`, project, target, ms(now)); err != nil {
		return err
	}
	return tx.Commit()
}

// permitMachineKnown reports whether machine is one of project's machines
// now (the run targets an approved action may name). Tests replace it.
var permitMachineKnown = func(ctx context.Context, s *Service, project, machine string) (bool, error) {
	l, err := s.runTargets(ctx, project, "")
	if err != nil {
		return false, err
	}
	for _, t := range l.Targets {
		if t.Machine == machine {
			return true, nil
		}
	}
	return false, nil
}

// approverAdmin is why approver's yes cannot grant a permit in project,
// "" when its person (its --for identity) owns or administers project now.
// The approver must still be the device that answered (node, from the
// signed receipt) as paired when it answered (answered, ms): a name or
// device paired again later is another pairing, so an old yes never
// borrows the new pairing's person. A device paired for no one is
// no one's: its yes never grants, renews or widens a permit, whoever holds
// it.
func (s *Service) approverAdmin(ctx context.Context, approver, node string, answered int64, project string) (string, error) {
	const notAdmin = "does not own or administer the project now"
	var identity string
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT identity FROM approvers WHERE name = ? AND node_id = ? AND node_id != '' AND state = ?
		AND paired IS NOT NULL AND paired <= ?`, approver, node, orchapi.ApproverActive, answered).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		return "is not the paired device that answered, now", nil
	}
	if err != nil {
		return "", err
	}
	if identity == "" {
		return "is a device paired for no one, so zm cannot say who said yes (pair it for its person: zm approver add <name> --for <person>)", nil
	}
	sub, err := s.d.Projects.IdentitySubject(ctx, identity)
	if api.IsCode(err, api.CodeNotFound) {
		return notAdmin, nil
	}
	if err != nil {
		return "", err
	}
	ok, err := s.d.ProjectAdmin(ctx, sub, project)
	if err != nil || ok {
		return "", err
	}
	return notAdmin, nil
}

// handleRevoke ends the permit's kind at once: that permit and any live one
// that renewed or widened it. Any owner or admin of its project may,
// unsigned: revoking only narrows.
func (s *Service) handleRevoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	id := r.PathValue("id")
	p, err := s.scanPermit(s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+permitCols+` FROM permits WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) || err == nil && (!sub.Can(authz.Operate, authz.Object{Project: p.Project}) || !sub.Scope.WholeProject(p.Project)) {
		api.WriteError(w, api.NotFound("permit", id))
		return
	}
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if !s.mayNarrow(ctx, w, sub, p.Project) {
		return
	}
	now := s.now().Truncate(time.Millisecond)
	if err := s.narrow(ctx, p.Project, narrowTarget(orchapi.PermitCardRequest{Kind: p.Kind}), now,
		`UPDATE permits SET revoked = ?, revoked_by = ? WHERE project = ? AND kind = ? AND revoked IS NULL`, ms(now), sub.Address, p.Project, p.Kind); err != nil {
		api.WriteError(w, err)
		return
	}
	s.d.Log.Info("approvals: standing permission revoked", "permit", p.ID, "kind", p.Kind, "by", sub.Address)
	if p, err = s.scanPermit(s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+permitCols+` FROM permits WHERE id = ?`, id)); err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, s.redactPermit(ctx, sub, p))
}

// handleUnmark takes a class off a machine (unsigned, as revoke).
func (s *Service) handleUnmark(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	var req orchapi.BoxUnmarkRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	p, err := s.permitProject(ctx)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if !s.mayNarrow(ctx, w, sub, p.ID) {
		return
	}
	if _, ok := boxClassByName(req.Class); !ok || !plainArg.MatchString(req.Machine) || len(req.Machine) > maxActionField {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "unmark names one machine and a box class"))
		return
	}
	// Recorded whether or not the mark is on: an answer given before it
	// never marks the box (handleGrant).
	marked, err := s.boxMarked(ctx, p.ID, req.Machine, req.Class)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if err := s.narrow(ctx, p.ID, narrowTarget(orchapi.PermitCardRequest{Machine: req.Machine, Class: req.Class}), s.now().Truncate(time.Millisecond),
		`DELETE FROM box_marks WHERE project = ? AND machine = ? AND class = ?`, p.ID, req.Machine, req.Class); err != nil {
		api.WriteError(w, err)
		return
	}
	if !marked {
		api.WriteError(w, api.NotFound("box mark", req.Machine+" "+req.Class))
		return
	}
	s.d.Log.Info("approvals: box mark taken off", "machine", req.Machine, "class", req.Class, "by", sub.Address)
	api.WriteJSON(w, http.StatusOK, req)
}

// mayNarrow writes the refusal and reports false unless sub owns or
// administers project.
func (s *Service) mayNarrow(ctx context.Context, w http.ResponseWriter, sub *authz.Subject, project string) bool {
	admin, err := s.d.ProjectAdmin(ctx, sub, project)
	if err != nil {
		api.WriteError(w, err)
		return false
	}
	if !admin {
		api.WriteError(w, api.Errorf(api.CodeForbidden, "an owner or admin of the project revokes a standing permission or takes a box mark off"))
		return false
	}
	return true
}

func (s *Service) handlePermitDryRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p, err := s.permitProject(ctx)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	to := s.now()
	from := to.Add(-7 * 24 * time.Hour)
	for name, t := range map[string]*time.Time{"from": &from, "to": &to} {
		if v := r.URL.Query().Get(name); v != "" {
			at, err := time.Parse(time.RFC3339, v)
			if err != nil {
				api.WriteError(w, api.Errorf(api.CodeBadRequest, "%s: an RFC 3339 time", name))
				return
			}
			*t = at
		}
	}
	rows, err := s.PermitDryRun(ctx, p.ID, from, to)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, orchapi.PermitDryRunReport{Project: p.Slug, From: from, To: to, Rows: rows})
}
