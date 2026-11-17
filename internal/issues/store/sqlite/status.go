package sqlite

import (
	"github.com/sengirunners/mezze/internal/orchapi"
)

// statusChange applies beads' side effects of a status move: closed_at on
// close, cleared on reopen (with the reason); started_at on the first
// in_progress; and zm's: a status whose zm status is open ends claimed_by.
func (w *writer) statusChange(nw *orchapi.Issue, from, to, session string, d fieldDiff) {
	if from == to {
		return
	}
	d.set("status", from, to)
	nw.Status, nw.ZmStatus = to, orchapi.ZmStatus(to)
	now := w.m.At.UTC()
	if to == orchapi.IssueClosed {
		nw.ClosedAt = &now
		if session != "" {
			nw.ClosedBySession = session
		}
	} else if from == orchapi.IssueClosed {
		nw.ClosedAt, nw.CloseReason, nw.ClosedBySession = nil, "", ""
	}
	if to == orchapi.IssueInProgress && nw.StartedAt == nil {
		nw.StartedAt = &now
	}
	if nw.ZmStatus == orchapi.WorkOpen && nw.ClaimedBy != "" { // the claim ended: claimed_by goes, the assignee stays (bd)
		d.set("claimed_by", nw.ClaimedBy, "")
		nw.ClaimedBy = ""
	}
}
