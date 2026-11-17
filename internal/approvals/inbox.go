package approvals

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// What marks a work item as waiting on a person with no ask yet. A team that
// labels differently asks for its own with ?label=.
var (
	// waitingLabels wait on whoever reads the inbox.
	waitingLabels = []string{"owner-decision", "human", "questions", "decision", "needs-night-ruling", "owner-run"}
	// owner:<name> (a person holds it) and ask:<who> (someone must be asked)
	// name who an item waits on: the reader's own names (viewerNames), and
	// ask:owner for an owner of the item's project, count; anyone else's
	// do not.
	waitingPrefixes = []string{"owner:", "ask:"}
	waitingTypes    = []string{"decision"}
	// stepLabels mark a step only a person can run. Any other wait that is a
	// question (decisionLabels, ask:<who>, type decision) and has no ask is a
	// decision nobody wrote up: it waits on an agent (?for=agents), who writes
	// the ask or records an AGENT DECISION, never on a person.
	stepLabels     = []string{"human", "owner-run"}
	decisionLabels = []string{"owner-decision", "decision", "questions", "needs-night-ruling"}
)

// everyoneLabels are the labels of every person's waits (?for=everyone, a team
// view): the generic ones and needs-<x> for each identity name and alias.
func everyoneLabels(people map[string]bool) []string {
	out := slices.Clone(waitingLabels)
	for n := range people {
		out = append(out, "needs-"+n)
	}
	sort.Strings(out)
	return out
}

// unwritten says whether a waiting item with no ask is a decision nobody has
// written up yet, rather than a step for a person.
func unwritten(issueType string, labels []string) bool {
	d := slices.Contains(waitingTypes, issueType)
	for _, l := range labels {
		switch {
		case slices.Contains(stepLabels, l):
			return false
		case slices.Contains(decisionLabels, l), strings.HasPrefix(l, "ask:"):
			d = true
		}
	}
	return d
}

// askOwner waits on whoever owns the item's project.
const askOwner = "ask:owner"

// inboxReader is who an inbox is for: the names its labels may use for the
// reader, the projects it owns, and every person name at this authority
// (identity names and aliases: needs-<x> names someone only when x is one).
// A nil reader reads everyone's waits.
type inboxReader struct {
	names  map[string]bool
	people map[string]bool
	owns   func(project string) bool
}

// labels are the exact labels that may put an item in r's inbox.
func (r *inboxReader) labels() []string {
	out := append([]string{askOwner}, waitingLabels...)
	for n := range r.names {
		out = append(out, "owner:"+n, "ask:"+n, "needs-"+n)
	}
	sort.Strings(out)
	return out
}

// ruledLabel marks an item whose ruling is recorded: a type decision or a
// generic label no longer makes it wait (a name on it still does).
const ruledLabel = "ruled"

// waitsOn says whether an item with these labels waits on r. An item that
// names who it waits on (owner:<name>, ask:<name>, or needs-<name> where
// name is an identity's name or alias here; needs-spec or needs-design-pass
// is a plain tag) is r's only when one of those names is r's, or it asks
// the owner of a project r owns; only an item that names nobody falls back
// to the generic labels and type decision, and not once it is labelled
// ruled.
func (r *inboxReader) waitsOn(project, issueType string, labels []string) bool {
	if r == nil {
		return true
	}
	named, generic, ruled := false, issueType == "decision", false
	for _, l := range labels {
		who, ok := strings.CutPrefix(l, "owner:")
		if !ok {
			who, ok = strings.CutPrefix(l, "ask:")
		}
		if !ok && !slices.Contains(waitingLabels, l) {
			if x, tag := strings.CutPrefix(l, "needs-"); tag && (r.names[x] || r.people[x]) {
				who, ok = x, true
			}
		}
		switch {
		case ok:
			named = true
			if r.names[who] || (l == askOwner && r.owns(project)) {
				return true
			}
		case slices.Contains(waitingLabels, l):
			generic = true
		case l == ruledLabel:
			ruled = true
		}
	}
	return generic && !named && !ruled
}

// reader builds sub's inbox reader: its identity's name and aliases, and
// the projects it owns, by its scope or by its identity's own grants (a
// replica's token is capped below owner; this only picks among rows the
// scope already reads, so no grant is exercised).
func (s *Service) reader(ctx context.Context, sub *authz.Subject) (*inboxReader, error) {
	names, err := s.viewerNames(ctx, sub)
	if err != nil {
		return nil, err
	}
	r := &inboxReader{names: map[string]bool{}}
	for _, n := range names {
		r.names[n] = true
	}
	if r.people, err = s.personNames(ctx); err != nil {
		return nil, err
	}
	owned := map[string]bool{}
	if sub != nil && sub.Identity.ID != "" {
		tuples, err := s.d.Projects.Tuples(ctx, sub.Identity.ID)
		if err != nil {
			return nil, err
		}
		for _, t := range tuples {
			if p, area, err := authz.ParseObject(t.Object); err == nil && area == "" && t.Relation == authz.Owner {
				owned[p] = true
			}
		}
	}
	r.owns = func(project string) bool {
		return owned[project] || (sub != nil && sub.Scope.Can(authz.Admin, authz.Object{Project: project}))
	}
	return r, nil
}

// inboxLimit bounds the asks one read returns (tests change it).
var inboxLimit = 500

// handleInbox is GET /v0/decisions: the one place a person's decisions wait.
// ?status= takes open, answered, recorded and withdrawn (default open,answered);
// ?work=0 leaves out the labelled work items; ?for=everyone lists work that
// waits on anyone (a team view), not only on the reader (for=me, the default).
// A person's lists hold steps; ?for=agents is the decisions with no ask yet,
// and no asks.
func (s *Service) handleInbox(w http.ResponseWriter, r *http.Request) {
	out, err := s.inbox(r.Context(), r.URL.Query())
	respond(w, out, err)
}

// inbox is GET /v0/decisions for the subject in ctx.
func (s *Service) inbox(ctx context.Context, q url.Values) (orchapi.Inbox, error) {
	ctx = withPeopleCache(ctx)
	sub := core.SubjectOf(ctx)
	if sub.Limit() != "" {
		// An answer-only token reads asks only, whatever the query says:
		// work items (work, label=, for=) are the owner's other reads.
		q = maps.Clone(q)
		q.Set("work", "0")
		q.Del("label")
		q.Del("for")
	}
	forWho := q.Get("for")
	switch forWho {
	case "", "me", "everyone", "agents":
	default:
		return orchapi.Inbox{}, api.Errorf(api.CodeBadRequest, "for %q: me, everyone or agents", forWho)
	}
	sc, err := s.d.ReadScope(ctx, q.Get("project"))
	if err != nil {
		return orchapi.Inbox{}, err
	}
	want := map[string]bool{orchapi.AskOpen: true, orchapi.AskAnswered: true}
	if v := q.Get("status"); v != "" {
		want = map[string]bool{}
		for _, st := range strings.Split(v, ",") {
			switch st {
			case orchapi.AskOpen, orchapi.AskAnswered, orchapi.AskRecorded, orchapi.AskWithdrawn:
				want[st] = true
			default:
				return orchapi.Inbox{}, api.Errorf(api.CodeBadRequest, "status %q: open, answered, recorded or withdrawn", st)
			}
		}
	}
	out := orchapi.Inbox{Decisions: []orchapi.InboxItem{}}
	where, args := sc.SQL("a.project", "a.area", authz.Read)
	var states []string
	if want[orchapi.AskOpen] {
		states = append(states, `(d.ask IS NULL AND a.withdrawn IS NULL)`)
		// An answered ask whose action nobody confirmed sits with the open
		// ones (a done one for a day): queued, running, or failed on top.
		// Approved work that is not done (queued, running, approved) is listed
		// on its own, after the open asks; on top once it went back to the approver.
		states = append(states, fmt.Sprintf(`(x.ask IS NOT NULL AND x.confirmed IS NULL AND (x.state != 'done' OR coalesce(x.verified, x.ended) IS NULL OR coalesce(x.verified, x.ended) >= %d))`,
			ms(s.now().Add(-doneListed))))
	}
	if want[orchapi.AskAnswered] {
		states = append(states, `(d.ask IS NOT NULL AND d.recorded IS NULL)`)
	}
	if want[orchapi.AskRecorded] {
		states = append(states, `d.recorded IS NOT NULL`)
	}
	if want[orchapi.AskWithdrawn] {
		states = append(states, `a.withdrawn IS NOT NULL`)
	}
	if forWho == "agents" {
		states = []string{"0"} // the decisions no ask covers: no asks
	}
	operate, opArgs := sc.SQL("a.project", "a.area", authz.Operate)
	where += " AND (" + operate + " OR a.asked_by = ? OR a.to_addr = ?)"
	args = append(args, opArgs...)
	// A card the judge could not check is listed at once, for who may operate
	// on it, marked not_checked; one it passed and is only
	// waiting to send is nobody's yet.
	hv, hargs := heldVisible(sc, sub.Address, true)
	args = append(args, sub.Address, sub.Address)
	args = append(args, hargs...)
	args = append(args, inboxLimit+1)
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT `+askCols+askFrom+` WHERE `+where+hv+` AND (`+strings.Join(states, " OR ")+`)
 ORDER BY CASE WHEN x.state = 'failed' AND x.confirmed IS NULL THEN 0
 WHEN x.state IN ('queued', 'running', 'approved') AND x.escalated IS NOT NULL THEN 0
 WHEN x.state IN ('queued', 'running', 'approved') THEN 2
 WHEN EXISTS (SELECT 1 FROM ask_judging j WHERE j.ask = a.id) THEN 2 WHEN x.ask IS NOT NULL AND x.confirmed IS NULL THEN 1
 WHEN d.ask IS NOT NULL OR a.withdrawn IS NOT NULL THEN 5 WHEN a.for_group != '' THEN 3 ELSE 1 END,
 CASE WHEN a.for_group = '' AND a.by_ms IS NOT NULL THEN 0 ELSE 1 END,
 CASE WHEN a.for_group = '' THEN a.by_ms END, a.priority,
 CASE WHEN a.for_group != '' THEN json_extract(a.for_group,'$.group') ELSE '' END, a.id LIMIT ?`, args...)
	if err != nil {
		return orchapi.Inbox{}, err
	}
	defer rows.Close()
	var asks []orchapi.Ask
	for rows.Next() {
		a, err := scanAsk(rows)
		if err != nil {
			return orchapi.Inbox{}, err
		}
		if mayRead(sc, sub.Address, a) {
			asks = append(asks, a)
		}
	}
	if err := rows.Err(); err != nil {
		return orchapi.Inbox{}, err
	}
	// view reads the registry again (session visibility). Holding rows while it
	// waits for a second connection deadlocks the pool once a few inbox reads
	// overlap, so the cursor closes first.
	rows.Close()
	for _, a := range asks {
		v := s.view(ctx, sub, a)
		out.Decisions = append(out.Decisions, orchapi.InboxItem{Kind: "ask", ProjectSlug: s.d.Projects.Slug(a.Project), Priority: a.Priority, OwnerName: a.Owner, Ask: &v})
	}
	if q.Get("work") != "0" {
		var rd *inboxReader
		if forWho == "" || forWho == "me" {
			if rd, err = s.reader(ctx, sub); err != nil {
				return orchapi.Inbox{}, err
			}
		}
		if err := s.waitingWork(ctx, sub, sc, rd, forWho == "agents", q["label"], &out); err != nil {
			return orchapi.Inbox{}, err
		}
	}
	sortInbox(out.Decisions)
	if len(out.Decisions) > inboxLimit {
		out.Decisions = out.Decisions[:inboxLimit]
		out.Skipped = append(out.Skipped, api.WorkSkipped{Source: "decisions", Error: "more decisions available; narrow project or status"})
	}
	return out, nil
}

// waitingWork adds the work items waiting on a person that no ask covers: the
// steps, or with agents the decisions nobody wrote up (?label= lists both). A
// withdrawn ask covers nothing; an answered one does, while the label lags.
func (s *Service) waitingWork(ctx context.Context, sub *authz.Subject, sc authz.Scope, rd *inboxReader, agents bool, labels []string, out *orchapi.Inbox) error {
	rec, ok := orchcore.Work(s.d).(orchcore.WorkRecorder)
	if !ok {
		return nil
	}
	var f orchapi.WaitingFilter
	if rd != nil {
		f = orchapi.WaitingFilter{Labels: rd.labels(), Types: waitingTypes}
	} else if len(labels) == 0 {
		// A team view: needs-<x> for every person at this authority.
		people, err := s.personNames(ctx)
		if err != nil {
			return err
		}
		f = orchapi.WaitingFilter{Labels: everyoneLabels(people), Prefixes: waitingPrefixes, Types: waitingTypes}
	}
	if len(labels) > 0 {
		f = orchapi.WaitingFilter{Labels: labels}
		rd = nil // the caller's own labels: whatever carries them
	}
	pg, err := rec.WaitingItems(ctx, sub, sc, f)
	if err != nil {
		return err
	}
	for pages := 1; pg.Next != "" && pages < 20; pages++ {
		f.After = pg.Next
		next, err := rec.WaitingItems(ctx, sub, sc, f)
		if err != nil {
			return err
		}
		pg.Items = append(pg.Items, next.Items...)
		pg.Skipped = append(pg.Skipped, next.Skipped...)
		pg.Behind = append(pg.Behind, next.Behind...)
		if next.Next == pg.Next {
			break
		}
		pg.Next = next.Next
	}
	if pg.Next != "" {
		pg.Skipped = append(pg.Skipped, api.WorkSkipped{Source: "decisions", Error: "more waiting work available; narrow project"})
	}
	// This only takes rows out of a list sc already reads, so it shows nothing
	// of an ask the reader cannot see.
	asked := map[string]bool{}
	read, readArgs := sc.SQL("a.project", "a.area", authz.Read)
	operate, opArgs := sc.SQL("a.project", "a.area", authz.Operate)
	type candidate struct {
		Project string `json:"project"`
		Work    string `json:"work"`
	}
	candidates := make([]candidate, 0, len(pg.Items))
	for _, it := range pg.Items {
		candidates = append(candidates, candidate{it.Project, it.ID})
	}
	// An ask the judge could not check covers its item for who sees it listed
	// (not_checked), like any open ask; one it passed and has yet to send does not.
	hv, hargs := heldVisible(sc, sub.Address, true)
	args := []any{marshal(candidates)}
	args = append(args, hargs...)
	args = append(args, readArgs...)
	args = append(args, opArgs...)
	args = append(args, sub.Address, sub.Address)
	rows, err := s.d.Store.Registry().QueryContext(ctx, `WITH candidates AS (SELECT json_extract(value,'$.project') project,json_extract(value,'$.work') work FROM json_each(?))
 SELECT c.project,c.work FROM candidates c WHERE EXISTS(SELECT 1 FROM asks a WHERE a.project=c.project AND a.work=c.work AND a.work!='' AND a.withdrawn IS NULL`+hv+` AND `+read+` AND (`+operate+` OR a.asked_by=? OR a.to_addr=?))`, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var project, work string
		if err := rows.Scan(&project, &work); err != nil {
			rows.Close()
			return err
		}
		asked[project+"\x00"+work] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	out.Skipped = append(out.Skipped, pg.Skipped...)
	out.Behind = append(out.Behind, pg.Behind...)
	for i := range pg.Items {
		it := pg.Items[i]
		typ := extraString(it.Extra, "issue_type")
		if asked[it.Project+"\x00"+it.ID] || typ == "epic" || !rd.waitsOn(it.Project, typ, it.Labels) ||
			(len(labels) == 0 && (unwritten(typ, it.Labels) != agents || agents && slices.Contains(it.Labels, ruledLabel))) {
			continue
		}
		row := orchapi.InboxItem{Kind: "work", ProjectSlug: s.d.Projects.Slug(it.Project), Priority: extraInt(it.Extra, "priority", 2), Work: &it}
		for _, l := range it.Labels {
			if name, ok := strings.CutPrefix(l, "owner:"); ok {
				row.OwnerName = name
				break
			}
		}
		out.Decisions = append(out.Decisions, row)
	}
	return nil
}

func extraString(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

func extraInt(m map[string]any, k string, def int) int {
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	}
	return def
}

// inboxRank: what needs a person now first; asks held for an outside party
// wait until they are asked; what is answered and work with no ask follow.
func inboxRank(x orchapi.InboxItem) int {
	switch {
	case x.Kind == "work":
		return 4
	case x.Ask.ActionState != nil && x.Ask.ActionState.ConfirmedAt == nil && x.Ask.ActionState.State == orchapi.ActionFailed:
		return 0 // an exception: on top until an owner confirms it
	case x.Ask.NotDone && x.Ask.ActionState.EscalatedAt != nil:
		return 0 // approved long ago and still not done: back to the approver
	case x.Ask.NotDone:
		return 2 // approved, not done: on its own, after what is open
	case x.Ask.ActionState != nil && actionListed(x.Ask.ActionState, time.Now()):
		return 1
	case x.Ask.NotChecked:
		return 2 // held by the judge: on its own, after what is open, deadlines first
	case x.Ask.Status != orchapi.AskOpen:
		return 5
	case x.Ask.For != nil:
		return 3
	}
	return 1
}

// sortInbox orders one daemon's rows; a merged list across authorities is one
// such run per authority, and the reader sorts by the same fields.
func sortInbox(xs []orchapi.InboxItem) {
	sort.SliceStable(xs, func(i, j int) bool {
		a, b := xs[i], xs[j]
		if ra, rb := inboxRank(a), inboxRank(b); ra != rb {
			return ra < rb
		}
		if a.Kind == "ask" && b.Kind == "ask" && a.Ask.For == nil && b.Ask.For == nil {
			// A deadline bubbles up within what is open.
			switch {
			case a.Ask.By != nil && b.Ask.By != nil && !a.Ask.By.Equal(*b.Ask.By):
				return a.Ask.By.Before(*b.Ask.By)
			case (a.Ask.By != nil) != (b.Ask.By != nil):
				return a.Ask.By != nil
			}
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.Kind == "ask" && b.Kind == "ask" {
			if a.Ask.For != nil && b.Ask.For != nil && a.Ask.For.Group != b.Ask.For.Group {
				return a.Ask.For.Group < b.Ask.For.Group
			}
			return a.Ask.ID < b.Ask.ID // oldest first: the order they are read out
		}
		return false
	})
}

// scrub overwrites this module's copies of a redacted text: the rows stay, and
// a receipt keeps verifying because it signs only the hashes.
func (s *Service) scrub(ctx context.Context, sc core.Scrub) error {
	s.deliveryMu.Lock()
	defer s.deliveryMu.Unlock()
	db := s.d.Store.Registry()
	subjects := map[string]bool{}
	for _, x := range sc.Subjects {
		subjects[x] = true
	}
	rows, err := db.QueryContext(ctx, `SELECT a.id, a.work, a.body, a.card, coalesce(d.text, '') FROM asks a LEFT JOIN decisions d ON d.ask = a.id WHERE a.project = ?`, sc.Project)
	if err != nil {
		return err
	}
	type row struct{ id, work, body, card, text string }
	var all []row
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.id, &x.work, &x.body, &x.card, &x.text); err != nil {
			rows.Close()
			return err
		}
		all = append(all, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, x := range all {
		minLen := core.MinUnscoped
		_, ref, _ := strings.Cut(x.work, ":")
		if subjects[x.work] || subjects[ref] {
			minLen = 0
		}
		effectRows, err := db.QueryContext(ctx, `SELECT kind,target,body,state FROM decision_effects WHERE ask=?`, x.id)
		if err != nil {
			return err
		}
		type copyEffect struct{ kind, target, body, state string }
		var copies []copyEffect
		for effectRows.Next() {
			var c copyEffect
			if err := effectRows.Scan(&c.kind, &c.target, &c.body, &c.state); err != nil {
				effectRows.Close()
				return err
			}
			copies = append(copies, c)
		}
		effectRows.Close()
		if err := effectRows.Err(); err != nil {
			return err
		}
		for _, c := range copies {
			body := core.ScrubText(c.body, sc.Texts, minLen)
			if body != c.body {
				state := c.state
				message := ""
				if state != "succeeded" {
					state = "unknown"
					message = "unknown: redacted delivery requires reconciliation"
				}
				if _, err := db.ExecContext(ctx, `UPDATE decision_effects SET body=?,payload_sha=?,state=?,error=? WHERE ask=? AND kind=?`, body, textSHA(c.kind+"\n"+c.target+"\n"+body), state, message, x.id, c.kind); err != nil {
					return err
				}
			}
		}
		body, card, text := core.ScrubText(x.body, sc.Texts, minLen), core.ScrubText(x.card, sc.Texts, minLen), core.ScrubText(x.text, sc.Texts, minLen)
		if body == x.body && card == x.card && text == x.text {
			continue
		}
		if _, err := db.ExecContext(ctx, `UPDATE asks SET body = ?, card = ? WHERE id = ?`, body, card, x.id); err != nil {
			return err
		}
		if _, err := db.ExecContext(ctx, `UPDATE decisions SET text = ? WHERE ask = ?`, text, x.id); err != nil {
			return err
		}
	}
	return nil
}
