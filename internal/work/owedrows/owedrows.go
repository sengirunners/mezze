// Package owedrows turns what the work module knows about one item a session
// holds, or last held, into the rows of what that session still owes (SPEC 8
// "What a session still owes"). It is pure: the work module reads the
// records and hands them here.
//
// A pull request is "open" when its item says so: GitHub linking (SPEC 18)
// moves the item to done on a merge and back to open on an unmerged close,
// so an item that is claimed or in review and carries a pr link has an open
// PR (the same rule as package unheld).
package owedrows

import (
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// Held is one item a session holds or last held, as the work module read it.
type Held struct {
	Item   orchapi.WorkItem
	Holder string // the session address, as the reader may see it
	// Leased: a live lease holds the item now. False: the session only last
	// held it (its lease lapsed or ended).
	Leased  bool
	Claimed *time.Time // when the live lease was taken
}

// Rows are the owed rows for h: the item itself while a lease holds it and
// it is not done, and each open pull request linked to it.
func Rows(h Held) []orchapi.OwedItem {
	it := h.Item
	var out []orchapi.OwedItem
	if h.Leased && it.Status != orchapi.WorkDone {
		out = append(out, orchapi.OwedItem{Kind: orchapi.OwedWork, Ref: it.ID, Title: it.Title, State: string(it.Status),
			Project: it.Project, Area: it.Area, Holder: h.Holder, Since: h.Claimed})
	}
	if it.Status != orchapi.WorkClaimed && it.Status != orchapi.WorkInReview {
		return out
	}
	seen := map[string]bool{}
	for _, l := range it.Links {
		if l.Rel != "pr" || l.Hidden || l.Target == "" || seen[l.Target] {
			continue
		}
		seen[l.Target] = true
		out = append(out, orchapi.OwedItem{Kind: orchapi.OwedPR, Ref: l.Target, Title: it.Title, State: "open, linked to " + it.ID,
			Project: it.Project, Area: it.Area, Holder: h.Holder})
	}
	return out
}
