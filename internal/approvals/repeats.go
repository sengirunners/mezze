package approvals

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/store"
)

// Repeats (SPEC 16 "Card judge"): the card judge also says
// which open card of the new card's area, if any, asks the same question.
// Whether two questions are the same is the judge's call (cardjudge.md,
// with eval cases), never a word match. Then:
//   - the new card is for the same person (or both for the owners), has no
//     attachments, and carries no action or the same action on the same
//     target as the open card: the ask joins the open card (same_as), as
//     `zm ask join` does, and nobody is asked twice;
//   - otherwise: the card is judged and filed as its own, linked to the
//     open one.
//
// A ruling or a status report sends a card back even when it repeats one.
// The judge is shown only open cards the asker may read (mayRead): every
// card of the area for who may operate there, else its own and those put
// to it.
//
// A joined ask never carries an action or a verify check: the first ask's
// action runs once, on the first ask, and a joined ask only gets its answer.

var repeatsMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0013_ask_links", Script: `
CREATE TABLE ask_links (
	ask   TEXT PRIMARY KEY,
	other TEXT NOT NULL
);`},
}

// Bounds on what the judge reads of the open cards.
const (
	openCardsMax     = 30
	openCardContext  = 600
	openCardQuestion = 400
)

// openCards is the open cards a new ask a may repeat: first asks of their
// groups in a's project and area, with a card, not held, not answered, not
// withdrawn, not held for an outside party, that a's asker may read
// (operator: it may operate on the area; else only its own and those put to
// it, as mayRead); newest first.
func (s *Service) openCards(ctx context.Context, a orchapi.Ask, operator bool) []judgeOpenCard {
	mine, args := "", []any{a.Project, a.Area, a.ID}
	if !operator {
		mine = ` AND (a.asked_by = ? OR a.to_addr = ?)`
		args = append(args, a.AskedBy, a.AskedBy)
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT `+askCols+askFrom+` WHERE a.project = ? AND a.area = ? AND a.id != ?
 AND a.card != '' AND a.same_as = '' AND a.for_group = '' AND a.withdrawn IS NULL AND d.ask IS NULL`+mine+notHeld+` ORDER BY a.id DESC LIMIT ?`,
		append(args, openCardsMax)...)
	if err != nil {
		s.d.Log.Warn("approvals: read open cards for the card judge", "err", err)
		return nil
	}
	defer rows.Close()
	var out []judgeOpenCard
	for rows.Next() {
		o, err := scanAsk(rows)
		if err != nil || o.Card == nil {
			continue
		}
		c := o.Card
		jc := judgeOpenCard{ID: o.ID, Title: c.Title, Context: clipText(c.Context, openCardContext), Question: clipText(c.Question, openCardQuestion),
			Action: actionText(c.Action)}
		for _, ch := range c.Choices {
			jc.Choices = append(jc.Choices, ch.Label)
		}
		out = append(out, jc)
	}
	return out
}

// clipText is s cut to at most n bytes on a rune boundary.
func clipText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

// readsCard reports whether the asker of a may read open card o (as
// openCards chose it).
func readsCard(a, o orchapi.Ask, operator bool) bool {
	return o.Project == a.Project && o.Area == a.Area && (operator || o.AskedBy == a.AskedBy || o.To == a.AskedBy)
}

// joinsRepeat reports whether ask a, whose verdict v names open card root
// as the one it repeats, joins root rather than being filed and linked.
func joinsRepeat(a, root orchapi.Ask, v cardResult) bool {
	return !v.Firm && a.For == nil && a.Card != nil && root.Card != nil && root.Decision == nil && root.Withdrawn == nil &&
		strings.EqualFold(a.Owner, root.Owner) && len(a.Attachments) == 0 && joinable(actionOf(a), actionOf(root)) &&
		sameVerify(a.Card.Verify, root.Card.Verify) && sameStep(a.Card.OwnerStep, root.Card.OwnerStep)
}

// sameStep reports whether a card's person step survives a join: neither
// card has one, or both have the same (a join must never drop a step).
func sameStep(mine, theirs *orchapi.AskStep) bool {
	if mine == nil || theirs == nil {
		return mine == theirs
	}
	return mine.Label == theirs.Label && mine.Link == theirs.Link && slices.Equal(mine.Steps, theirs.Steps)
}

// sameVerify reports whether a card's own verify check survives a join:
// it has none, or the first's is the same (a joined ask carries none, and
// the first's tracks the work).
func sameVerify(mine, theirs *orchapi.AskVerify) bool {
	return mine == nil || (theirs != nil && strings.TrimSpace(mine.Command) == strings.TrimSpace(theirs.Command) && mine.On == theirs.On)
}

// joinable reports whether a card with action mine may join an open card
// with action theirs: mine carries none, or the same action on the same
// target. Comparing the frozen actions' fields is syntax, not meaning.
func joinable(mine, theirs *orchapi.AskAction) bool {
	if mine == nil {
		return true
	}
	if theirs == nil {
		return false
	}
	return actionKey(*mine) == actionKey(*theirs)
}

// actionKey is what an action does and where (the commit a ref pointed
// at included), without what zm derived for display (counts, samples,
// hashes) or which choice runs it.
func actionKey(a orchapi.AskAction) string {
	a.On, a.Count, a.Samples, a.BriefSHA, a.KeyComment, a.KeyFingerprint = "", 0, nil, "", "", ""
	a.IDs = slices.Clone(a.IDs)
	slices.Sort(a.IDs)
	a.Brief = strings.TrimSpace(a.Brief)
	b, _ := json.Marshal(a)
	return string(b)
}

// link records that ask id repeats open ask other with a different action.
func (s *Service) link(ctx context.Context, id, other string) {
	if _, err := s.d.Store.Registry().ExecContext(ctx, `INSERT OR REPLACE INTO ask_links (ask, other) VALUES (?, ?)`, id, other); err != nil {
		s.d.Log.Warn("approvals: link an ask to the open ask it repeats", "ask", id, "other", other, "err", err)
	}
}

// joinedNote is what the asker of an ask zm joined to an open one is told.
func joinedNote(root string) string {
	return "joined open ask " + root + ": the card judge found it asks the same question, so nobody is asked twice. " +
		"Its card is now yours, and its answer reaches you as this ask's"
}

// actionNote tells the asker of an ask joined to root, when root carries
// an action, that the action is root's and runs there.
func actionNote(root orchapi.Ask) string {
	act := actionOf(root)
	if act == nil {
		return ""
	}
	return "Ask " + root.ID + " carries an action (" + actionText(act) + "): it is that ask's and runs only there, once; this ask carries none and only gets the answer"
}

// linkedNote is what the asker of an ask linked to an open one is told
// (other: its id, or "" when the asker may not read it).
func linkedNote(other string) string {
	if other == "" {
		return "linked to an open ask that asks the same question, which this one could not join"
	}
	return "linked to open ask " + other + ": it asks the same question, but this one could not join it"
}

// joinHeld makes held ask a, which the judge found repeats open ask root,
// a joined ask of root (root's card without its action or check, its
// choices, priority and deadline; a and root are for the same person), then
// sends it. Under groupMu, so root's answer either comes first (then a is
// not joined) or reaches a. It reports whether a was joined.
func (s *Service) joinHeld(ctx context.Context, a orchapi.Ask, rootID string, tries int, operator bool) bool {
	s.groupMu.Lock()
	defer s.groupMu.Unlock()
	root, err := s.ask(ctx, rootID)
	if err != nil || root.Card == nil || root.Decision != nil || root.Withdrawn != nil || root.SameAs != "" || root.For != nil ||
		!readsCard(a, root, operator) || !strings.EqualFold(a.Owner, root.Owner) || s.isHeld(ctx, root.ID) {
		return false
	}
	var by any
	if a.By != nil {
		by = ms(*a.By)
	}
	if root.By != nil && root.By.After(s.now()) {
		by = ms(*root.By)
	}
	c := *root.Card
	c.Action, c.Verify = nil, nil
	card := marshal(c)
	sha := cardSHA(card)
	question := a.Subject.Question
	if a.Subject.Work == "" {
		question = textSHA("zm-question/1\n" + a.ID + "\n" + sha)
	}
	ob, _ := json.Marshal(root.Options)
	res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE asks SET same_as = ?, card = ?, card_sha = ?, body = ?,
		question = ?, options = ?, priority = ?, by_ms = ?, apply_default = 0 WHERE id = ? AND withdrawn IS NULL`,
		root.ID, card, sha, renderCard(c, nil), question, string(ob), root.Priority, by, a.ID)
	if err != nil {
		s.d.Log.Warn("approvals: join a held ask to the open ask it repeats", "ask", a.ID, "first", root.ID, "err", err)
		return false
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false
	}
	_, _ = s.d.Store.Registry().ExecContext(ctx, `DELETE FROM ask_links WHERE ask = ?`, a.ID)
	s.release(ctx, a.ID, tries)
	return true
}

// releaseJoined sends a held ask that passed (its earlier send failed),
// under groupMu when it joined another, so the group's answer and its send
// are ordered.
func (s *Service) releaseJoined(ctx context.Context, a orchapi.Ask, tries int) {
	if a.SameAs != "" {
		s.groupMu.Lock()
		defer s.groupMu.Unlock()
	}
	s.release(ctx, a.ID, tries)
}

// dropJoined withdraws held ask a, joined to root, once root was answered
// or withdrawn before a could be sent, and tells its asker: root's answer
// is its answer, or it asks again. Under holdMu (release).
func (s *Service) dropJoined(ctx context.Context, a, root orchapi.Ask) {
	res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE asks SET withdrawn = ?, dedupe = '' WHERE id = ? AND withdrawn IS NULL`, ms(s.now()), a.ID)
	if err != nil {
		s.d.Log.Warn("approvals: withdraw a held joined ask", "ask", a.ID, "err", err)
		return
	}
	if err := s.unhold(ctx, a.ID); err != nil {
		s.d.Log.Warn("approvals: unhold a joined ask", "ask", a.ID, "err", err)
	}
	if n, _ := res.RowsAffected(); n == 0 || !orchapi.AgentSender(a.AskedBy) {
		return
	}
	text := fmt.Sprintf("Ask %s was not sent: it joined ask %s, which was withdrawn before it could go out. Ask again with your own card.", a.ID, a.SameAs)
	if root.Decision != nil && root.Decision.Decision == orchapi.DecisionNeedsContext {
		text = fmt.Sprintf("Ask %s was not sent: it joined ask %s, which was sent back for more context: %s. Ask again with your own card, with that context in it.",
			a.ID, root.ID, root.Decision.Text)
	} else if root.Decision != nil {
		text = fmt.Sprintf("Ask %s was not sent: it joined ask %s, which was answered first: %s. That answer is yours: record it on your item as an AGENT DECISION naming ask %s.",
			a.ID, root.ID, first(root.Decision.Answer, root.Decision.Decision), root.ID)
	}
	if _, err := orchcore.Mail(s.d).Send(orchcore.AsAskNotice(ctx), s.d.Projects.Local(""), api.ProjectRef{ID: a.Project, Slug: s.d.Projects.Slug(a.Project)},
		a.Area, orchapi.SendMailRequest{To: a.AskedBy, Body: text}); err != nil {
		s.d.Log.Warn("approvals: tell the asker a joined ask was dropped", "ask", a.ID, "err", err)
	}
}

// signedAnswer records a signed answer (a paired device's or the Mezze app's key's)
// to ask id. It stays on the ask it signed, except that the first ask of a
// group that carries an action gives it to the group's joined asks, which
// carry none and can be answered no other way. check, when
// set, runs on the ask before anything is written (as record's).
func (s *Service) signedAnswer(ctx context.Context, me approverRow, id string, req orchapi.DecideRequest, check func(orchapi.Ask) error) (orchapi.Decision, error) {
	a, err := s.ask(ctx, id)
	if err != nil {
		return orchapi.Decision{}, err
	}
	grouped := a.Card != nil && a.SameAs == "" && a.Card.Action != nil
	if grouped {
		s.groupMu.Lock() // a join lands before this answer reaches its group, or sees it
		defer s.groupMu.Unlock()
	}
	d, err := s.record(ctx, me, id, req, func(cur orchapi.Ask) error {
		if check != nil {
			if err := check(cur); err != nil {
				return err
			}
		}
		return s.joinedActionAsk(ctx, cur)
	})
	if err == nil && grouped {
		s.decideGroup(ctx, nil, me, a, req)
	}
	return d, err
}

// repeatNotes is a new ask's note: why zm joined it (joinNote), the
// first's action when it joined one that has one, its link (the linked id
// only for an operator), then the rest (note).
func repeatNotes(joinNote string, joined *orchapi.Ask, linked string, operator bool, note string) string {
	var notes []string
	if joinNote != "" {
		notes = append(notes, joinNote)
	}
	if joined != nil {
		if n := actionNote(*joined); n != "" {
			notes = append(notes, n)
		}
	}
	if linked != "" {
		shown := ""
		if operator {
			shown = linked
		}
		notes = append(notes, linkedNote(shown))
	}
	if note != "" {
		notes = append(notes, note)
	}
	return strings.Join(notes, ". ")
}
