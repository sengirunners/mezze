package team

import (
	"context"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// Reports on a replica's copy (SPEC 9 "Reports"): an agent's mail to a person
// is filed as kind "report" at the authority; the copy keeps it out of every
// inbox and notice and lists it for its person with ?box=reports.

// notReport is the copy's filter for mail that is not a report (SPEC 9
// "Reports"): reports are never in an inbox or a notice.
const notReport = "json_extract(json, '$.kind') IS NOT 'report'"

// reports is the reports filed for a person (human callers only), from the
// mailboxes the inbox reads.
func (s *Service) reports(ctx context.Context, rp *replica, snap api.Remote, me string, all bool, limit int) ([]orchapi.Mail, error) {
	a, err := api.ParseAddress(me)
	if err != nil || a.Kind != api.KindHuman {
		return []orchapi.Mail{}, nil
	}
	open := "acked = 0"
	if all {
		open = "1=1"
	}
	m := s.d.Config.Machine
	where := "json_extract(json, '$.kind') = 'report' AND " + open + " AND (recipient = ? OR recipient = ? OR (recipient >= ? AND recipient < ?))"
	ms, err := rp.mailWhere(ctx, snap, me, false, "("+where+")", []any{me, "machine:" + m, m + "/", m + "0"}, limit)
	if err != nil || all {
		return ms, err
	}
	return s.dropTaken(ctx, rp, snap, me, ms), nil
}

// dropTaken leaves out the mail the caller's unsettled acks and replies take
// (a reply takes its parent too): they leave the inbox at once.
func (s *Service) dropTaken(ctx context.Context, rp *replica, snap api.Remote, me string, ms []orchapi.Mail) []orchapi.Mail {
	es, err := rp.overlays(ctx, "kind IN ('ack', 'reply')")
	if err != nil || len(es) == 0 {
		return ms
	}
	acked := map[string]bool{}
	for _, e := range es {
		if e.authorOr("human:"+snap.Identity) == me {
			acked[e.item] = true
		}
	}
	keep := ms[:0]
	for _, x := range ms {
		if !acked[x.ID] {
			keep = append(keep, x)
		}
	}
	return keep
}
