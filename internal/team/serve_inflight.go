package team

import (
	"context"
	"encoding/json"
	"maps"
	"slices"

	"github.com/sengirunners/mezze/internal/orchapi"
)

func (rp *replica) inflight(ctx context.Context, project, area string) (orchapi.InflightList, error) {
	ls, err := rp.leases(ctx, area, "")
	if err != nil {
		return orchapi.InflightList{}, err
	}
	out := orchapi.InflightList{Inflight: []orchapi.Inflight{}}
	held := map[string]bool{}
	for _, l := range ls {
		held[l.Key] = true
		row := orchapi.Inflight{Lease: l}
		if it, _, ok := rp.findItem(ctx, l.Key); ok && it.ID == l.Key {
			it.Owner, it.Links, it.Comments = l.Holder, nil, nil
			row.Item = &it
		}
		out.Inflight = append(out.Inflight, row)
	}
	// Then claimed or in-review items with a pr link and no lease, as the
	// authority lists them. The copy holds only what this machine reads.
	// This machine's unsettled status writes count, as in listItems: an item
	// just closed here drops out, one just put in review shows.
	st, err := rp.statusOverlay(ctx, "")
	if err != nil {
		return orchapi.InflightList{}, err
	}
	overlaid, _ := json.Marshal(slices.AppendSeq([]string{}, maps.Keys(st)))
	rows, err := rp.db.QueryContext(ctx, `SELECT json FROM items WHERE (status IN ('claimed', 'in_review') OR id IN (SELECT value FROM json_each(?))) AND (? = '' OR area = ?) ORDER BY created, id`, string(overlaid), area, area)
	if err != nil {
		return orchapi.InflightList{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var js string
		var it orchapi.WorkItem
		if rows.Scan(&js) != nil || json.Unmarshal([]byte(js), &it) != nil || held[it.ID] || !slices.ContainsFunc(it.Links, func(l orchapi.Link) bool { return l.Rel == "pr" }) {
			continue
		}
		if v, ok := st[it.ID]; ok {
			it.Status = v
		}
		if it.Status != orchapi.WorkClaimed && it.Status != orchapi.WorkInReview {
			continue
		}
		it.Owner, it.Links, it.Comments = "", nil, nil
		out.Inflight = append(out.Inflight, orchapi.Inflight{Lease: orchapi.Lease{Project: project, Area: it.Area, Key: it.ID}, Item: &it, Unheld: true})
	}
	return out, rows.Err()
}
