package approvals

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/authz"
)

// maxOwedAsks bounds one owed read; the newest come first.
var maxOwedAsks = 200

// owedSource is this module's part of what a session still owes (SPEC 8
// "What a session still owes"): the cards it filed that no one has answered,
// and the ones answered yes whose work is not yet done.
func (s *Service) owedSource() orchcore.OwedSource {
	return orchcore.OwedSource{Name: "asks", Owed: s.owed}
}

func (s *Service) owed(ctx context.Context, sub *authz.Subject, sc authz.Scope, who orchcore.OwedWho) (orchcore.OwedPart, error) {
	addrs := who.Addresses()
	if len(addrs) == 0 && who.Person == "" {
		return orchcore.OwedPart{}, nil
	}
	where, args := sc.SQL("a.project", "a.area", authz.Read)
	var by []string
	if len(addrs) > 0 {
		by = append(by, "a.asked_by IN ("+strings.TrimSuffix(strings.Repeat("?,", len(addrs)), ",")+")")
		for _, a := range addrs {
			args = append(args, a)
		}
	}
	if who.Person != "" {
		by = append(by, "a.asker_human = ?")
		args = append(args, who.Person)
	}
	where += " AND (" + strings.Join(by, " OR ") + ") AND a.withdrawn IS NULL" +
		" AND (d.ask IS NULL OR x.state IN ('" + orchapi.ActionQueued + "', '" + orchapi.ActionRunning + "', '" + orchapi.ActionApproved + "'))"
	base, baseArgs := where, slices.Clone(args)
	where += " AND (a.asked_by = ? OR" + strings.TrimPrefix(notHeld, " AND") + ")" // a held card is its asker's alone
	args = append(args, sub.Address)
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT `+askCols+askFrom+` WHERE `+where+` ORDER BY a.id DESC LIMIT `+strconv.Itoa(maxOwedAsks+1), args...)
	if err != nil {
		return orchcore.OwedPart{}, err
	}
	defer rows.Close()
	var asks []orchapi.Ask
	truncated, read := false, 0
	for rows.Next() {
		if read++; read > maxOwedAsks {
			truncated = true // one row past the cap says more follow
			break
		}
		a, err := scanAsk(rows)
		if err != nil {
			return orchcore.OwedPart{}, err
		}
		if mayRead(sc, sub.Address, a) {
			asks = append(asks, a)
		}
	}
	if err := rows.Err(); err != nil {
		return orchcore.OwedPart{}, err
	}
	rows.Close() // view reads the registry again
	var out orchcore.OwedPart
	// A card the judge holds is its asker's alone, so a person's list leaves it
	// out: say that some are, to those who could read them, never what they are.
	held, err := s.heldOwed(ctx, sub, sc, base, baseArgs)
	if err != nil {
		return orchcore.OwedPart{}, err
	}
	if held > 0 {
		out.Gaps = append(out.Gaps, orchcore.Partial("asks", "a card held for review is its asker's alone until it passes: not listed here"))
	}
	if truncated {
		out.Gaps = append(out.Gaps, orchcore.Partial("asks", "more cards than one read takes: the newest are listed"))
	}
	for _, a := range asks {
		a = s.view(ctx, sub, a)
		it := orchapi.OwedItem{Ref: a.ID, Title: owedTitle(a), Project: a.Project, Area: a.Area, Holder: a.AskedBy}
		created := a.Created
		it.Since = &created
		switch {
		case a.NotDone:
			it.Kind = orchapi.OwedAskNotDone
			it.State = "approved, not done"
			if a.ActionStateText != "" {
				it.State += ": " + a.ActionStateText
			}
		case a.Status == orchapi.AskOpen:
			it.Kind = orchapi.OwedAskOpen
			it.State = "waiting for an answer"
		default:
			continue
		}
		out.Items = append(out.Items, it)
	}
	return out, nil
}

// owedTitle is the card's question as the asker wrote it, else its body's
// first line, on one line.
func owedTitle(a orchapi.Ask) string {
	t := a.Subject.Question
	if strings.TrimSpace(t) == "" {
		t, _, _ = strings.Cut(strings.TrimSpace(a.Body), "\n")
	}
	t = strings.Join(strings.Fields(t), " ")
	if r := []rune(t); len(r) > 160 {
		t = string(r[:160])
	}
	return t
}

// heldOwed counts the cards the judge holds that the rest of the query would
// list but for the hold, and that sub may read (mayRead): a reader who could
// not read the card is told nothing of it.
func (s *Service) heldOwed(ctx context.Context, sub *authz.Subject, sc authz.Scope, base string, args []any) (int, error) {
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT `+askCols+askFrom+` WHERE `+base+` AND a.asked_by != ?`+strings.Replace(notHeld, "NOT EXISTS", "EXISTS", 1)+` LIMIT `+strconv.Itoa(maxOwedAsks), append(args, sub.Address)...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		a, err := scanAsk(rows)
		if err != nil {
			return 0, err
		}
		if mayRead(sc, sub.Address, a) {
			n++
		}
	}
	return n, rows.Err()
}
