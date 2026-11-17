package measures_test

import (
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/measures"
	"github.com/sengirunners/mezze/internal/orchapi"
)

// All names are made up: nothing here depends on a real person or machine.
var (
	zone = time.FixedZone("test", 3600) // +1h, so a day cut in UTC would differ
	d1   = time.Date(2030, 3, 4, 8, 0, 0, 0, zone)
	now  = time.Date(2030, 3, 7, 12, 0, 0, 0, zone)
)

func at(day int, h, m int) time.Time { return time.Date(2030, 3, day, h, m, 0, 0, zone) }
func ptr(t time.Time) *time.Time     { return &t }

func decided(a orchapi.Ask, by, choice string, when time.Time) orchapi.Ask {
	a.Decision = &orchapi.Decision{Ask: a.ID, Decision: choice, Approver: by, Time: when}
	a.Status = orchapi.AskAnswered
	return a
}

func card(id, owner string, created time.Time) orchapi.Ask {
	return orchapi.Ask{ID: id, Owner: owner, Created: created, AskedBy: "agent-" + id, Card: &orchapi.AskCard{Recommend: "a"}}
}

func run(asks []orchapi.Ask, mail []orchapi.Mail, cfg measures.Config) measures.Report {
	return measures.Compute(measures.Input{Asks: asks, Mail: mail, From: d1.Truncate(0).Add(-8 * time.Hour), To: at(7, 0, 0), Now: now, Loc: zone, Cfg: cfg}, measures.Rows)
}

func cell(t *testing.T, r measures.Report, id, group, day, owner string) measures.Cell {
	t.Helper()
	for _, s := range r.Series {
		if s.ID == id && s.Group == group {
			return s.Cells[day][owner]
		}
	}
	t.Fatalf("no series %s [%s]; have %d series", id, group, len(r.Series))
	return measures.Cell{}
}

func TestFiledAndAnsweredAreKeptApart(t *testing.T) {
	asks := []orchapi.Ask{
		decided(card("a1", "ada", at(4, 9, 0)), "bo", "a", at(5, 10, 0)), // filed day 4 for ada, answered day 5 by bo
		card("a2", "", at(5, 0, 30)),                                     // for no one by name: day 5 in the zone, day 4 in UTC
		card("a3", "ada", at(5, 1, 0)),
	}
	r := run(asks, nil, measures.DefaultConfig())
	if got := cell(t, r, "filed", "", "2030-03-04", "ada").N; got != 1 {
		t.Errorf("ada filed on day 4: %d", got)
	}
	if got := cell(t, r, "filed", "", "2030-03-05", measures.OwnersLabel).N; got != 1 {
		t.Errorf("a card for no one by name: %d (zone-cut day wrong?)", got)
	}
	if got := cell(t, r, "filed", "", measures.Window, measures.AllOwners).N; got != 3 {
		t.Errorf("filed in the window: %d", got)
	}
	if got := cell(t, r, "answered", "", "2030-03-05", "bo").N; got != 1 {
		t.Errorf("bo answered on day 5: %d", got)
	}
	if got := cell(t, r, "answered", "", "2030-03-04", "ada").N; got != 0 {
		t.Errorf("ada answered nothing: %d", got)
	}
}

func TestRecommendationRateIsPerKindAndUnlabelledShowsAsSuch(t *testing.T) {
	a1 := decided(card("a1", "ada", at(4, 9, 0)), "ada", "a", at(4, 10, 0)) // with the recommendation
	a1.Labels = map[string]string{orchapi.LabelKind: "demo"}
	a2 := decided(card("a2", "ada", at(4, 9, 0)), "ada", "b", at(4, 10, 0)) // against it
	a2.Labels = map[string]string{orchapi.LabelKind: "demo"}
	a3 := decided(card("a3", "ada", at(4, 9, 0)), "ada", "a", at(4, 10, 0)) // no label at all
	noRec := decided(card("a4", "ada", at(4, 9, 0)), "ada", "a", at(4, 10, 0))
	noRec.Card.Recommend = "" // nothing to follow: not counted
	r := run([]orchapi.Ask{a1, a2, a3, noRec}, nil, measures.DefaultConfig())
	if c := cell(t, r, "recommended", "kind=demo", measures.Window, measures.AllOwners); c.N != 2 || c.Hits != 1 {
		t.Errorf("kind=demo %+v", c)
	}
	if c := cell(t, r, "recommended", "kind="+measures.Unlabelled, measures.Window, measures.AllOwners); c.N != 1 || c.Hits != 1 {
		t.Errorf("unlabelled %+v", c)
	}
}

func TestWithdrawnFastRepeatsAndMedian(t *testing.T) {
	fast := card("f", "ada", at(4, 9, 0))
	fast.Withdrawn = ptr(at(4, 9, 9))
	slow := card("s", "ada", at(4, 9, 0))
	slow.Withdrawn = ptr(at(4, 9, 11))
	joined := card("j", "ada", at(4, 9, 0))
	joined.SameAs = "f"
	labelled := card("l", "ada", at(4, 9, 0))
	labelled.Labels = map[string]string{orchapi.LabelRepeatOf: "f"}
	words := card("w", "ada", at(4, 9, 0)) // says "again" but no record says it is a repeat
	words.Body = "asking again, same as before"
	w1 := decided(card("m1", "ada", at(4, 9, 0)), "bo", "a", at(4, 9, 1))
	w2 := decided(card("m2", "ada", at(4, 9, 0)), "bo", "a", at(4, 9, 11))
	w3 := decided(card("m3", "ada", at(4, 9, 0)), "bo", "a", at(4, 9, 31))
	r := run([]orchapi.Ask{fast, slow, joined, labelled, words, w1, w2, w3}, nil, measures.DefaultConfig())
	if got := cell(t, r, "withdrawn_fast", "", measures.Window, measures.AllOwners).N; got != 1 {
		t.Errorf("withdrawn fast: %d", got)
	}
	if got := cell(t, r, "repeats", "", measures.Window, measures.AllOwners).N; got != 2 {
		t.Errorf("repeats: %d (only the join record and the judge label count)", got)
	}
	if c := cell(t, r, "median_answer", "", measures.Window, "bo"); c.N != 3 || c.Value != 11*60 {
		t.Errorf("median %+v", c)
	}
}

func TestStuckYesNeedsTheWait(t *testing.T) {
	old := decided(card("o", "ada", at(4, 9, 0)), "bo", "a", at(4, 10, 0))
	old.NotDone = true
	fresh := decided(card("n", "ada", at(6, 9, 0)), "bo", "a", at(6, 13, 0)) // 23h before now
	fresh.NotDone = true
	done := decided(card("d", "ada", at(4, 9, 0)), "bo", "a", at(4, 10, 0))
	r := run([]orchapi.Ask{old, fresh, done}, nil, measures.DefaultConfig())
	if got := cell(t, r, "yes_not_done", "", measures.Window, measures.AllOwners).N; got != 1 {
		t.Errorf("stuck yeses: %d", got)
	}
	cfg := measures.DefaultConfig()
	cfg.NotDoneAfter = time.Minute
	if got := cell(t, run([]orchapi.Ask{old, fresh, done}, nil, cfg), "yes_not_done", "", measures.Window, measures.AllOwners).N; got != 2 {
		t.Errorf("with a one-minute wait: %d", got)
	}
}

// Each step zm took with approved work still not done is one line, on the
// day it took it, for the approver.
func TestNotDoneSteps(t *testing.T) {
	laned := decided(card("l", "ada", at(4, 9, 0)), "bo", "a", at(4, 10, 0))
	laned.ActionState = &orchapi.ActionState{State: orchapi.ActionApproved, RemindedAt: ptr(at(5, 10, 0)),
		RelanedAt: ptr(at(6, 10, 0)), RelaneTask: "task-1", RelaneMachine: "box-a", EscalatedAt: ptr(at(6, 20, 0))}
	refused := decided(card("r", "ada", at(4, 9, 0)), "bo", "a", at(4, 10, 0))
	refused.ActionState = &orchapi.ActionState{State: orchapi.ActionApproved, RemindedAt: ptr(at(5, 11, 0)),
		RelanedAt: ptr(at(6, 11, 0)), RelaneReason: "no lane machine", EscalatedAt: ptr(at(6, 11, 0))}
	old := decided(card("o", "ada", at(4, 9, 0)), "bo", "a", at(4, 10, 0)) // marked by the migration: no reason, no task
	old.ActionState = &orchapi.ActionState{State: orchapi.ActionApproved, RelanedAt: ptr(at(5, 9, 0)), EscalatedAt: ptr(at(5, 9, 0))}
	evidence := decided(card("e", "ada", at(4, 9, 0)), "bo", "a", at(4, 10, 0))
	evidence.ActionState = &orchapi.ActionState{State: orchapi.ActionDone, CheckedAt: ptr(at(5, 12, 0)), DoneEvidence: "the page is live"}
	verified := decided(card("v", "ada", at(4, 9, 0)), "bo", "a", at(4, 10, 0))
	verified.ActionState = &orchapi.ActionState{State: orchapi.ActionDone, CheckedAt: ptr(at(5, 12, 0)), VerifiedAt: ptr(at(5, 12, 0))}
	r := run([]orchapi.Ask{laned, refused, old, evidence, verified}, nil, measures.DefaultConfig())
	for group, want := range map[string]int{"step=asker": 2, "step=lane": 1, "step=no_lane": 1, "step=approver": 3, "step=evidence": 1} {
		if got := cell(t, r, "not_done_steps", group, measures.Window, measures.AllOwners).N; got != want {
			t.Errorf("%s: %d, want %d", group, got, want)
		}
	}
	if got := cell(t, r, "not_done_steps", "step=lane", "2030-03-06", "bo").N; got != 1 {
		t.Errorf("the lane on day 6 for bo: %d", got)
	}
}

func TestMail(t *testing.T) {
	m := func(id, rec, via string, created time.Time, acked *time.Time) orchapi.Mail {
		return orchapi.Mail{ID: id, Recipient: rec, Via: via, Kind: orchapi.MailKindNote, Created: created, AckedAt: acked}
	}
	human := m("h", "machine:box1", "group", at(4, 9, 0), nil)
	human.Needs = orchapi.NeedsHuman
	mail := []orchapi.Mail{
		m("e", "box1/abcd1234", "orphan of box1/dead0000", at(4, 9, 0), nil),
		m("g1", "machine:box1", "group", at(4, 9, 0), nil),               // open past a day
		m("g2", "machine:box1", "group", at(4, 9, 0), ptr(at(4, 9, 5))),  // taken at once
		m("g3", "machine:box1", "group", at(4, 9, 0), ptr(at(5, 10, 0))), // taken after a day (the sweep)
		m("g4", "machine:box1", "group", at(6, 20, 0), nil),              // open, not yet a day
		human, // for a human: not group mail an agent takes
	}
	r := run(nil, mail, measures.DefaultConfig())
	if got := cell(t, r, "mail_ended", "", measures.Window, measures.AllOwners).N; got != 1 {
		t.Errorf("mail to ended sessions: %d", got)
	}
	if got := cell(t, r, "group_untaken", "", measures.Window, measures.AllOwners).N; got != 2 {
		t.Errorf("group mail untaken: %d", got)
	}
}

func TestHarmCountsOwnerPressesOnly(t *testing.T) {
	obj := decided(card("o", "ada", at(4, 9, 0)), "bo", "a", at(4, 10, 0))
	obj.Objection = &orchapi.AskObjection{At: at(5, 8, 0), By: "ada"}
	undone := card("u", "ada", at(4, 9, 0))
	undone.DefaultUndone, undone.DefaultUndoneBy = ptr(at(5, 9, 0)), "bo"
	quiet := decided(card("q", "ada", at(4, 9, 0)), "bo", "a", at(4, 10, 0))
	r := run([]orchapi.Ask{obj, undone, quiet}, nil, measures.DefaultConfig())
	if got := cell(t, r, "harm", "", "2030-03-05", measures.AllOwners).N; got != 2 {
		t.Errorf("harm on day 5: %d", got)
	}
	if got := cell(t, r, "harm", "", "2030-03-05", "ada").N; got != 1 {
		t.Errorf("ada's objection: %d", got)
	}
}

func TestExcludeAskerAndWindowAndQuietDays(t *testing.T) {
	imported := card("i", "ada", at(4, 9, 0))
	imported.AskedBy = "import-bot"
	inside := card("n", "ada", at(5, 9, 0))
	outside := card("x", "ada", at(1, 9, 0)) // before the window
	cfg := measures.DefaultConfig()
	cfg.ExcludeAskedBy = []string{"IMPORT-BOT"}
	r := run([]orchapi.Ask{imported, inside, outside}, nil, cfg)
	if got := cell(t, r, "filed", "", measures.Window, measures.AllOwners).N; got != 1 {
		t.Errorf("filed %d: the import and the early card are out", got)
	}
	if len(r.Days) != 3 || r.Days[0] != "2030-03-04" || r.Days[2] != "2030-03-06" {
		t.Errorf("days %v: every day shows, even quiet ones", r.Days)
	}
}

func TestRenderShowsEveryRowAndBothFormats(t *testing.T) {
	a := decided(card("a", "ada", at(4, 9, 0)), "bo", "a", at(4, 9, 30))
	r := run([]orchapi.Ask{a}, nil, measures.DefaultConfig())
	for _, f := range []measures.Format{measures.Text, measures.Markdown} {
		out := measures.Render(r, f)
		for _, want := range []string{"cards filed", "cards answered", "median time to answer", "harm", "2030-03-04", "30m0s", "1/1 (100%)", "unlabelled"} {
			if !strings.Contains(out, want) {
				t.Errorf("format %d: no %q in\n%s", f, want, out)
			}
		}
	}
	if !strings.Contains(measures.Render(r, measures.Markdown), "| measure |") {
		t.Error("markdown has no table")
	}
}

func TestAddingARowNeedsNoOtherChange(t *testing.T) {
	extra := measures.Row{ID: "demo", Title: "demo row", Agg: measures.Count, Scan: func(in *measures.Input, emit func(measures.Obs)) {
		for _, a := range in.Asks {
			emit(measures.Obs{At: a.Created, Owner: "ada"})
		}
	}}
	r := measures.Compute(measures.Input{Asks: []orchapi.Ask{card("a", "ada", at(4, 9, 0))}, From: at(4, 0, 0), To: at(5, 0, 0), Now: now, Loc: zone}, append(append([]measures.Row{}, measures.Rows...), extra))
	if got := cell(t, r, "demo", "", measures.Window, "ada").N; got != 1 {
		t.Errorf("new row: %d", got)
	}
	if !strings.Contains(measures.Render(r, measures.Text), "demo row") {
		t.Error("the new row is not rendered")
	}
}

func TestJoinedCardsAreOnePressAndSendBacksAreNotAnswers(t *testing.T) {
	first := decided(card("f", "ada", at(4, 9, 0)), "bo", "a", at(4, 10, 0))
	joined := decided(card("j", "ada", at(4, 9, 30)), "bo", "a", at(4, 10, 0))
	joined.SameAs = "f" // decided by the first's press
	back := decided(card("b", "ada", at(4, 9, 0)), "bo", orchapi.DecisionNeedsContext, at(4, 10, 0))
	r := run([]orchapi.Ask{first, joined, back}, nil, measures.DefaultConfig())
	if got := cell(t, r, "answered", "", measures.Window, measures.AllOwners).N; got != 1 {
		t.Errorf("answered %d: one press, and a send-back is no answer", got)
	}
	if got := cell(t, r, "filed", "", measures.Window, measures.AllOwners).N; got != 3 {
		t.Errorf("filed %d: all three were filed", got)
	}
	if c := cell(t, r, "median_answer", "", measures.Window, measures.AllOwners); c.N != 1 {
		t.Errorf("median over %d answers", c.N)
	}
}

func TestCanonMergesOneOwnersNames(t *testing.T) {
	a := card("a", "ada", at(4, 9, 0))
	b := card("b", "ada-alias", at(4, 9, 0))
	r := measures.Compute(measures.Input{Asks: []orchapi.Ask{a, b}, From: at(4, 0, 0), To: at(5, 0, 0), Now: now, Loc: zone,
		Canon: func(n string) string {
			if n == "ada-alias" {
				return "ada"
			}
			return n
		}}, measures.Rows)
	if got := cell(t, r, "filed", "", measures.Window, "ada").N; got != 2 || len(r.Owners) != 1 {
		t.Errorf("ada filed %d across owners %v", got, r.Owners)
	}
}
