package approvals

import (
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strings"
	"unicode"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

// Routing asks so people are bugged less (SPEC 16 "Routing"). Code here is
// transport and validation: whether an earlier ruling answers a question,
// and whether two questions are the same, is the asking agent's call. It
// reads candidates (similar), then joins an open ask (same_as) or asks.

var routingMigrations = []store.Migration{
	// owner: the person an ask is for ("" = the project's owners). same_as:
	// the first ask of its group ("" = it is the first).
	{Kind: store.KindRegistry, Name: "approvals/0006_routing", Script: `
ALTER TABLE asks ADD COLUMN owner TEXT NOT NULL DEFAULT '';
ALTER TABLE asks ADD COLUMN same_as TEXT NOT NULL DEFAULT '';
CREATE INDEX asks_same_as ON asks (project, same_as) WHERE same_as != '';
`},
}

// joinTarget fills req from the open card ask it joins (req.SameAs), which
// it returns: the group's first ask. One already answered is a conflict
// carrying it, so the asker reads the answer and nobody is asked.
func (s *Service) joinTarget(ctx context.Context, sub *authz.Subject, req *orchapi.NewAskRequest) (orchapi.Ask, error) {
	if req.Card != nil || req.For != nil || req.Subject.Repo != "" || req.Subject.PR != 0 || req.Subject.HeadSHA != "" || len(req.Options) > 0 {
		return orchapi.Ask{}, api.Errorf(api.CodeBadRequest, "same_as takes the first ask's card: send no card, options, for or pull request")
	}
	p, err := s.readableAsk(ctx, sub, req.SameAs)
	if err == nil && p.SameAs != "" {
		p, err = s.readableAsk(ctx, sub, p.SameAs)
	}
	if err != nil {
		return orchapi.Ask{}, err
	}
	switch {
	case p.Card == nil:
		return orchapi.Ask{}, api.Errorf(api.CodeBadRequest, "only a note card's ask can be joined")
	case p.For != nil:
		return orchapi.Ask{}, api.Errorf(api.CodeBadRequest, "an outside party's ask is one per topic: read its answer when it comes")
	case p.Withdrawn != nil:
		return orchapi.Ask{}, api.Errorf(api.CodeConflict, "ask %s was withdrawn", p.ID).WithHint("ask again with your own card")
	case p.Decision != nil && p.Decision.Decision == orchapi.DecisionNeedsContext:
		return orchapi.Ask{}, api.Errorf(api.CodeConflict, "ask %s was sent back for more context: %s", p.ID, p.Decision.Text).WithHint("ask again with your own card, with that context in it")
	case s.isHeld(ctx, p.ID):
		return orchapi.Ask{}, errHeld(p.ID).WithHint("wait for it to be sent, or ask with your own card")
	case s.releasedUnchecked(ctx, p.ID):
		// A joiner skips the judge and shares this card: never one the judge did not check.
		return orchapi.Ask{}, api.Errorf(api.CodeConflict, "ask %s was released without the card judge's check", p.ID).WithHint("ask again with your own card")
	case p.Decision != nil:
		return orchapi.Ask{}, api.Errorf(api.CodeConflict, "ask %s was already answered: %s", p.ID, first(p.Decision.Answer, p.Decision.Decision)).
			WithHint("its answer is yours: record it on your item as an AGENT DECISION naming the ask").WithData(s.view(ctx, sub, p))
	}
	if o := strings.TrimPrefix(strings.TrimSpace(req.Owner), "human:"); o != "" && !strings.EqualFold(o, p.Owner) {
		return orchapi.Ask{}, api.Errorf(api.CodeBadRequest, "a joined ask goes to its first ask's owner (%q)", p.Owner)
	}
	// A joined ask carries no action and no verify check:
	// the first ask's action runs once, on it, and its check is its own;
	// a joined ask only gets the answer.
	c := *p.Card
	c.Action, c.Verify = nil, nil
	req.Card, req.Owner, req.To = &c, p.Owner, p.To
	req.Priority = &p.Priority
	if p.By != nil && p.By.After(s.now()) {
		req.By = p.By
	}
	req.Topic = ""
	if req.Subject.Work != "" {
		req.Topic = p.Topic // one ask per work item and topic still holds
	}
	return p, nil
}

// readableAsk is ask id as sub may read it; hidden answers like missing.
func (s *Service) readableAsk(ctx context.Context, sub *authz.Subject, id string) (orchapi.Ask, error) {
	a, err := s.ask(ctx, id)
	if err == nil && !mayRead(sub.Scope, sub.Address, a) {
		err = api.NotFound("ask", id)
	}
	return a, err
}

// askOwner is the person an ask is for, as the name the ask keeps and the
// person's id: one named (any identity name, alias or GitHub login of the
// person, human:<name> too; resolvePerson), or the person its work item's
// owner:<name> label names; either must own or administer the project, the
// people who can answer it. Else nobody (the project's owners).
// Naming one takes a caller who reads the whole project, so an area guest
// learns nothing of who has an account here; only an operator's ask reads
// the item's label.
func (s *Service) askOwner(ctx context.Context, sub *authz.Subject, project, area, work, named string) (string, string, error) {
	named = strings.TrimSpace(named)
	if named == "" && work == "" {
		return "", "", nil
	}
	if named != "" && !sub.Scope.WholeProject(project) {
		return "", "", api.Errorf(api.CodeBadRequest, "a named person is for members of the whole project").
			WithHint("leave it out: the project's owners see the ask")
	}
	if named == "" && !sub.Can(authz.Operate, authz.Object{Project: project, Area: area}) {
		return "", "", nil
	}
	all, err := s.people(ctx, project)
	if err != nil {
		return "", "", err
	}
	people := answering(all)
	if named != "" {
		p, name, err := resolvePerson(people, named)
		return name, p.ID, err
	}
	wl, ok := orchcore.Work(s.d).(orchcore.WorkLabeler)
	if !ok {
		return "", "", nil
	}
	labels, err := wl.ItemLabels(ctx, sub, project, work)
	if err != nil {
		return "", "", err
	}
	for _, l := range labels {
		if n, ok := strings.CutPrefix(l, "owner:"); ok {
			if p, name, err := resolvePerson(people, n); err == nil {
				return name, p.ID, nil
			}
		}
	}
	return "", "", nil
}

// answers reports whether name names a person who answers project's asks.
// A lookup error counts as yes, so askOwner runs and reports it.
func (s *Service) answers(ctx context.Context, project, name string) bool {
	people, err := s.people(ctx, project)
	if err != nil {
		return true
	}
	name = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(name)), "human:")
	for _, p := range answering(people) {
		if p.match(name) != 0 {
			return true
		}
	}
	return false
}

// personOf is the id of the person name (an identity name, alias or login)
// names among everyone here, answering or not, or "".
func (s *Service) personOf(ctx context.Context, project, name string) string {
	people, err := s.people(ctx, project)
	if err != nil {
		return ""
	}
	p, _, _ := resolvePerson(people, name)
	return p.ID
}

// decideGroup gives every other open ask of a's group (same_as) the owner's
// answer, each its own decision, receipt, write-back and mail. A failure on
// one is logged; the answer to a stands. sub nil is a signed answer's
// approver, who decides any ask of the project.
//
// joinedActionAsk refuses answering a joined ask whose first ask carries an
// action: its answer would reach the first ask, and run its action, though
// the person never saw that action on the joined card.
func (s *Service) decideGroup(ctx context.Context, sub *authz.Subject, me approverRow, a orchapi.Ask, req orchapi.DecideRequest) {
	root := first(a.SameAs, a.ID)
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT `+askCols+askFrom+` WHERE a.project = ? AND (a.id = ? OR a.same_as = ?) AND a.id != ?
 AND d.ask IS NULL AND a.withdrawn IS NULL`+notHeld+` ORDER BY a.id`, a.Project, root, root, a.ID)
	if err != nil {
		s.d.Log.Error("approvals: read an ask's group", "ask", a.ID, "err", err)
		return
	}
	var group []orchapi.Ask
	for rows.Next() {
		g, err := scanAsk(rows)
		if err != nil {
			s.d.Log.Error("approvals: read an ask's group", "ask", a.ID, "err", err)
			break
		}
		// A member that carries an action is a group's first ask: only a
		// signed answer to it runs that action, never an answer given to
		// another ask of its group (joinedActionAsk refuses those).
		if (sub == nil || mayRead(sub.Scope, sub.Address, g)) && g.Card != nil && g.Card.Action == nil && !g.Subject.IsPR() {
			group = append(group, g)
		}
	}
	rows.Close()
	// card_sha256 names the card the person read on the ask answered, not
	// on its joined asks, whose own cards hash differently.
	req.CardSHA = ""
	for _, g := range group {
		if _, err := s.record(ctx, me, g.ID, req, func(cur orchapi.Ask) error {
			if cur.Subject.IsPR() || cur.Project != a.Project {
				return api.NotFound("ask", g.ID)
			}
			return nil
		}); err != nil {
			s.d.Log.Warn("approvals: answer a joined ask", "ask", g.ID, "first", a.ID, "err", err)
		}
	}
}

// similarLimit is how many asks GET /v0/asks?q= returns; similarScan how
// many recent card asks it reads to rank.
const (
	similarLimit = 10
	similarScan  = 2000
)

// handleSimilar is GET /v0/asks?q=: the first asks of groups, answered or
// open (never withdrawn), that sc reads, ranked by how many of q's words
// they share. A rank, not a verdict: the agent reads them and decides.
func (s *Service) handleSimilar(w http.ResponseWriter, r *http.Request, sub *authz.Subject, sc authz.Scope, q string) {
	ctx := r.Context()
	want := wordSet(q)
	if len(want) == 0 {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "q: say the question in a few words"))
		return
	}
	where, args := sc.SQL("a.project", "a.area", authz.Read)
	args = append(args, sub.Address, similarScan)
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT `+askCols+askFrom+` WHERE `+where+
		` AND a.card != '' AND a.same_as = '' AND a.withdrawn IS NULL AND (a.asked_by = ? OR NOT EXISTS (SELECT 1 FROM ask_judging j WHERE j.ask = a.id)) ORDER BY a.id DESC LIMIT ?`, args...)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	defer rows.Close()
	type scored struct {
		a orchapi.Ask
		n int
	}
	var hits []scored
	for rows.Next() {
		a, err := scanAsk(rows)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		if !mayRead(sc, sub.Address, a) {
			continue
		}
		have := wordSet(askText(a))
		n := 0
		for word := range want {
			if have[word] {
				n++
			}
		}
		if n > 0 {
			hits = append(hits, scored{a, n})
		}
	}
	if err := rows.Err(); err != nil && err != sql.ErrNoRows {
		api.WriteError(w, err)
		return
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].n != hits[j].n {
			return hits[i].n > hits[j].n
		}
		return ruled(hits[i].a) && !ruled(hits[j].a) // a ruling before an open ask
	})
	out := orchapi.AskList{Asks: []orchapi.Ask{}}
	for i := 0; i < len(hits) && i < similarLimit; i++ {
		out.Asks = append(out.Asks, s.view(ctx, sub, hits[i].a))
	}
	api.WriteJSON(w, http.StatusOK, out)
}

// ruled is true of an ask a person decided; one sent back for more context ruled nothing.
func ruled(a orchapi.Ask) bool {
	return a.Decision != nil && a.Decision.Decision != orchapi.DecisionNeedsContext
}

// askText is what a person read on a card ask, and its answer.
func askText(a orchapi.Ask) string {
	c := a.Card
	parts := []string{c.Title, c.Context, c.Background, c.Question, a.Topic}
	for _, ch := range c.Choices {
		parts = append(parts, ch.Label)
	}
	if a.Decision != nil {
		parts = append(parts, a.Decision.Text)
	}
	return strings.Join(parts, " ")
}

// stopWords carry no question of their own.
var stopWords = map[string]bool{"the": true, "and": true, "for": true, "should": true, "what": true, "which": true,
	"with": true, "this": true, "that": true, "are": true, "can": true, "how": true, "does": true, "from": true,
	"its": true, "not": true, "now": true, "our": true, "who": true, "why": true, "will": true, "you": true, "use": true}

// wordSet is s's lower-case words of three or more letters or digits,
// apostrophes dropped ("year's" is "years"), without stop words.
func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	s = strings.NewReplacer("'", "", "’", "").Replace(strings.ToLower(s))
	for _, w := range strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if len([]rune(w)) >= 3 && !stopWords[w] {
			out[w] = true
		}
	}
	return out
}

// joinedActionAsk is a conflict when a is a joined ask whose first ask
// carries an action and is still open: the first ask is the one to answer,
// and its answer reaches a. Once the first is withdrawn or answered (its
// answer did not reach a), a is answered on its own; it carries no action.
func (s *Service) joinedActionAsk(ctx context.Context, a orchapi.Ask) error {
	if a.SameAs == "" {
		return nil
	}
	root, err := s.ask(ctx, a.SameAs)
	if err != nil {
		return err
	}
	if actionOf(root) == nil || root.Withdrawn != nil || root.Decision != nil {
		return nil
	}
	return api.Errorf(api.CodeConflict, "ask %s joined ask %s, which carries an action: answer %s, and its answer reaches this one", a.ID, root.ID, root.ID).
		WithData(orchapi.Refusal{Reason: orchapi.ReasonJoinedAction})
}
