// Package rollup counts work items per theme and epic (SPEC 10 "Rollup"), for
// GET /v0/work/rollup. It knows nothing of who is asking: the caller gives a
// Source already narrowed to what the reader may see, so an item, an edge or
// an epic out of sight does not exist here.
//
// Source is the seam. Its one driver is SQL, over a table of work items whose
// tracker fields sit in JSON (the work mirror at an authority, the items copy
// on a replica). The second driver we expect is the native issue store read
// directly, once it has a driver that is not SQLite.
//
// A theme is an epic labelled "theme" with no open epic above it; an epic is
// an item of type "epic" that is not done; both come from the tracker's own
// fields (SPEC 11 "extra"), so a tracker that reports no hierarchy still gets
// its counts, under NoEpic.
package rollup

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// ThemeLabel marks an epic as a theme.
const ThemeLabel = "theme"

// EpicType is the extra.issue_type of an epic.
const EpicType = "epic"

// UrgentPriority is the lowest priority number that counts as urgent.
const UrgentPriority = 1

// returnsWithin is how far ahead a deferred item's return shows as a deadline.
const returnsWithin = 7 * 24 * time.Hour

// Group is the count of items alike: one source and parent, one type, status
// and priority. Priority is NoPriority when the tracker reports none.
type Group struct {
	Source, Parent, Type, Status, TrackerStatus string
	Priority                                    int
	Count                                       int
	Changed                                     time.Time
}

// NoPriority is an item with no priority.
const NoPriority = -1

// Item is one item with what a node, a deadline or a claim shows of it.
type Item struct {
	ID, Source, Ref, Parent, Title, Type, Status, TrackerStatus string
	Priority                                                    int
	Labels                                                      []string
	DueAt, DeferUntil, Changed                                  time.Time
}

// Source reads what the rollup is made of, within one reader's sight.
type Source interface {
	// Groups counts every item, epics too (Build leaves them out of counts).
	Groups(ctx context.Context) ([]Group, error)
	// Marked is every epic and every item with a due or defer time.
	Marked(ctx context.Context) ([]Item, error)
	// Parents answers, by ref, the parent of each of refs in source that
	// exists (a parent that is not an epic has one of its own to follow).
	Parents(ctx context.Context, source string, refs []string) (map[string]string, error)
	// Items reads the items with these work ids.
	Items(ctx context.Context, ids []string) ([]Item, error)
}

// Claim is a live lease on a work item, already redacted for the reader.
type Claim struct {
	Lease orchapi.Lease
	Mine  bool
	// Holder is the holding session's state as the caller knows it
	// (HolderState): "" for a person, a hidden holder or a session not known.
	Holder orchapi.SessionState
}

// HolderState is Claim.Holder for holder, a lease holder as the reader sees
// it: lookup finds a session by address. A person, a redacted holder and a
// session lookup does not know are "".
func HolderState(holder string, lookup func(string) (orchapi.Session, bool)) orchapi.SessionState {
	a, err := api.ParseAddress(holder)
	if err != nil || a.Kind != api.KindSession || lookup == nil {
		return ""
	}
	ss, ok := lookup(a.String())
	if !ok {
		return ""
	}
	return ss.State
}

// claimState is a claim's orchapi.RollupClaim.State: blocked when its item is
// blocked or its holder waits on a person or has ended (a lease left
// behind), live when its holder is running or idle, else neither.
func claimState(c Claim, it Item) string {
	switch {
	case strings.EqualFold(it.TrackerStatus, orchapi.IssueBlocked), c.Holder == orchapi.StateNeedsHuman, c.Holder == orchapi.StateEnded:
		return orchapi.ClaimBlocked
	case c.Holder == orchapi.StateRunning, c.Holder == orchapi.StateIdle:
		return orchapi.ClaimLive
	}
	return ""
}

// Build reads src and makes one project's rollup. now decides overdue and
// which deferred items come back soon. claims are the project's live leases on
// work items, oldest first.
func Build(ctx context.Context, src Source, project, slug string, claims []Claim, now time.Time) (orchapi.WorkRollup, error) {
	out := orchapi.WorkRollup{Project: project, Slug: slug, Nodes: []orchapi.RollupNode{}, Claimed: []orchapi.RollupClaim{}, Deadlines: []orchapi.RollupDeadline{}}
	groups, err := src.Groups(ctx)
	if err != nil {
		return out, err
	}
	marked, err := src.Marked(ctx)
	if err != nil {
		return out, err
	}
	up, err := parentChains(ctx, src, groups, marked)
	if err != nil {
		return out, err
	}
	nodes := nodesOf(marked, up)
	direct := map[string]*orchapi.RollupCounts{}
	for _, g := range groups {
		if g.Type == EpicType {
			continue
		}
		add(&out.Totals, g)
		owner := ""
		if g.Parent != "" {
			owner = nearest(g.Source, g.Parent, nodes, up)
		}
		if owner == "" {
			add(&out.NoEpic, g)
			continue
		}
		c := direct[owner]
		if c == nil {
			c = &orchapi.RollupCounts{}
			direct[owner] = c
		}
		add(c, g)
	}
	out.Nodes = tree(nodes, direct)
	out.Deadlines = deadlines(marked, now)
	epic := func(it Item) string { return idOf(nodes, nearest(it.Source, it.Parent, nodes, up)) }
	if out.Claimed, err = claimed(ctx, src, claims, epic); err != nil {
		return out, err
	}
	countClaims(out.Nodes, out.Claimed)
	return out, nil
}

// countClaims adds each claim to the node that counts its item and to every
// node above it, as the item counts roll up. A claim whose node was left out
// of nodes names none.
func countClaims(nodes []orchapi.RollupNode, claims []orchapi.RollupClaim) {
	at := make(map[string]int, len(nodes))
	for i := range nodes {
		at[nodes[i].ID] = i
	}
	for i := range claims {
		c := &claims[i]
		j, ok := at[c.Epic]
		if !ok {
			c.Epic = ""
			continue
		}
		for steps := 0; ok && steps <= len(nodes); steps++ { // tree broke every cycle; the bound is a guard
			switch c.State {
			case orchapi.ClaimLive:
				nodes[j].Claims.Live++
			case orchapi.ClaimBlocked:
				nodes[j].Claims.Blocked++
			}
			j, ok = at[nodes[j].Parent]
		}
	}
}

func add(c *orchapi.RollupCounts, g Group) {
	c.Total += g.Count
	switch {
	case g.Status == string(orchapi.WorkDone):
		c.Done += g.Count
	case g.Status == string(orchapi.WorkClaimed) || g.Status == string(orchapi.WorkInReview):
		c.Working += g.Count
	case strings.EqualFold(g.TrackerStatus, orchapi.IssueBlocked):
		c.Blocked += g.Count
		if urgent(g.Priority) {
			c.Urgent += g.Count
		}
	case strings.EqualFold(g.TrackerStatus, orchapi.IssueDeferred):
		c.Deferred += g.Count
	default:
		c.Open += g.Count
		if urgent(g.Priority) {
			c.Urgent += g.Count
		}
	}
	if g.Changed.After(c.Changed) {
		c.Changed = g.Changed
	}
}

func urgent(priority int) bool { return priority >= 0 && priority <= UrgentPriority }

func merge(c *orchapi.RollupCounts, o orchapi.RollupCounts) {
	c.Total += o.Total
	c.Open += o.Open
	c.Urgent += o.Urgent
	c.Working += o.Working
	c.Blocked += o.Blocked
	c.Deferred += o.Deferred
	c.Done += o.Done
	if o.Changed.After(c.Changed) {
		c.Changed = o.Changed
	}
}

// key names an item by source and ref (a parent names its ref, in its own source).
func key(source, ref string) string { return source + ":" + ref }

// parentChains maps every item that is a parent to its own parent, reading
// only those the marked rows do not already answer.
func parentChains(ctx context.Context, src Source, groups []Group, marked []Item) (map[string]string, error) {
	up := make(map[string]string, len(marked))
	for _, it := range marked {
		up[key(it.Source, it.Ref)] = it.Parent
	}
	want := map[string]map[string]bool{}
	ask := func(source, ref string) {
		if ref == "" {
			return
		}
		if _, known := up[key(source, ref)]; known {
			return
		}
		if want[source] == nil {
			want[source] = map[string]bool{}
		}
		want[source][ref] = true
	}
	for _, g := range groups {
		ask(g.Source, g.Parent)
	}
	for _, it := range marked {
		ask(it.Source, it.Parent)
	}
	for len(want) > 0 { // one read per level of the tallest chain
		next := map[string]map[string]bool{}
		for source, refs := range want {
			list := make([]string, 0, len(refs))
			for r := range refs {
				up[key(source, r)] = "" // missing or out of sight: the chain stops here
				list = append(list, r)
			}
			found, err := src.Parents(ctx, source, list)
			if err != nil {
				return nil, err
			}
			for ref, parent := range found {
				up[key(source, ref)] = parent
				if _, known := up[key(source, parent)]; parent != "" && !known {
					if next[source] == nil {
						next[source] = map[string]bool{}
					}
					next[source][parent] = true
				}
			}
		}
		want = next
	}
	return up, nil
}

// node is an open epic, in the making.
type node struct {
	item   Item
	parent string // the key of the nearest open epic above
	kids   []string
}

func nodesOf(marked []Item, up map[string]string) map[string]*node {
	nodes := map[string]*node{}
	for _, it := range marked {
		if it.Type == EpicType && it.Status != string(orchapi.WorkDone) {
			nodes[key(it.Source, it.Ref)] = &node{item: it}
		}
	}
	for k, n := range nodes {
		if n.item.Parent == "" {
			continue
		}
		if p := nearest(n.item.Source, n.item.Parent, nodes, up); p != "" && p != k {
			n.parent = p
		}
	}
	breakCycles(nodes)
	for k, n := range nodes {
		if n.parent != "" {
			nodes[n.parent].kids = append(nodes[n.parent].kids, k)
		}
	}
	return nodes
}

// nearest walks up from ref to the first open epic ("" when the chain ends
// first). A tracker's parents may loop; the walk stops at the first repeat.
func nearest(source, ref string, nodes map[string]*node, up map[string]string) string {
	var seen map[string]bool
	for steps := 0; ref != ""; steps++ {
		k := key(source, ref)
		if _, ok := nodes[k]; ok {
			return k
		}
		if steps > 8 { // chains are short: only a long one pays for the map
			if seen == nil {
				seen = map[string]bool{}
			}
			if seen[k] {
				return ""
			}
			seen[k] = true
		}
		ref = up[k]
	}
	return ""
}

// tree rolls counts up the epics and lists them: most urgent first, then most
// being worked on, then latest change. An epic with no items, no epics under
// it and no theme label is left out, as the Farm did.
func tree(nodes map[string]*node, direct map[string]*orchapi.RollupCounts) []orchapi.RollupNode {
	rolled := make(map[string]orchapi.RollupCounts, len(nodes))
	var roll func(k string, path map[string]bool) orchapi.RollupCounts
	roll = func(k string, path map[string]bool) orchapi.RollupCounts {
		if c, ok := rolled[k]; ok {
			return c
		}
		var c orchapi.RollupCounts
		if d := direct[k]; d != nil {
			c = *d
		}
		n := nodes[k]
		if n.item.Changed.After(c.Changed) {
			c.Changed = n.item.Changed
		}
		path[k] = true
		for _, kid := range n.kids {
			if !path[kid] {
				merge(&c, roll(kid, path))
			}
		}
		delete(path, k)
		rolled[k] = c
		return c
	}
	out := make([]orchapi.RollupNode, 0, len(nodes))
	for k, n := range nodes {
		c := roll(k, map[string]bool{})
		theme := n.parent == "" && slices.Contains(n.item.Labels, ThemeLabel)
		if c.Total == 0 && len(n.kids) == 0 && !theme {
			continue
		}
		kind := orchapi.RollupEpic
		if theme {
			kind = orchapi.RollupTheme
		}
		out = append(out, orchapi.RollupNode{ID: n.item.ID, Title: n.item.Title, Kind: kind, Parent: idOf(nodes, n.parent), RollupCounts: c})
	}
	slices.SortFunc(out, func(a, b orchapi.RollupNode) int {
		return cmp.Or(cmp.Compare(b.Urgent, a.Urgent), cmp.Compare(b.Working, a.Working), b.Changed.Compare(a.Changed), cmp.Compare(a.ID, b.ID))
	})
	at := make(map[string]int, len(out))
	for i := range out {
		at[out[i].ID] = i
	}
	for i := range out { // children keep the order of the list
		if j, ok := at[out[i].Parent]; ok {
			out[j].Children = append(out[j].Children, out[i].ID)
		} else {
			out[i].Parent = "" // left out above
		}
	}
	return out
}

func idOf(nodes map[string]*node, k string) string {
	if n := nodes[k]; n != nil {
		return n.item.ID
	}
	return ""
}

// deadlines is every item not done with a due time, then the deferred ones
// coming back within a week; each part by day, then priority.
func deadlines(marked []Item, now time.Time) []orchapi.RollupDeadline {
	today := now.UTC().Truncate(24 * time.Hour)
	out := []orchapi.RollupDeadline{}
	for _, it := range marked {
		if it.Status == string(orchapi.WorkDone) {
			continue
		}
		d := orchapi.RollupDeadline{ID: it.ID, Title: it.Title, Status: it.Status, Priority: priorityPtr(it.Priority)}
		switch {
		case !it.DueAt.IsZero():
			t := it.DueAt.UTC()
			day := typedDay(t)
			d.DueAt, d.Day, d.Overdue = &t, day.Format(time.DateOnly), day.Before(today)
		case !it.DeferUntil.IsZero():
			t := it.DeferUntil.UTC()
			day := typedDay(t)
			if day.Before(today) || day.After(today.Add(returnsWithin)) {
				continue
			}
			d.ReturnsAt, d.Day = &t, day.Format(time.DateOnly)
		default:
			continue
		}
		out = append(out, d)
	}
	slices.SortFunc(out, func(a, b orchapi.RollupDeadline) int {
		return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a.Day, b.Day), cmp.Compare(prio(a.Priority), prio(b.Priority)), cmp.Compare(a.ID, b.ID))
	})
	return out
}

func rank(d orchapi.RollupDeadline) int {
	if d.DueAt != nil {
		return 0
	}
	return 1
}

func prio(p *int) int {
	if p == nil {
		return 99
	}
	return *p
}

func priorityPtr(p int) *int {
	if p == NoPriority {
		return nil
	}
	return &p
}

// typedDay is the calendar day that was typed. bd stores `--due 2026-10-11`
// as midnight in the typist's time zone, so the nearest UTC midnight is it.
func typedDay(t time.Time) time.Time {
	return t.UTC().Add(12 * time.Hour).Truncate(24 * time.Hour)
}

// claimed lists the claims on items in sight; epic names the node that
// counts an item (a node id, or "").
func claimed(ctx context.Context, src Source, claims []Claim, epic func(Item) string) ([]orchapi.RollupClaim, error) {
	out := make([]orchapi.RollupClaim, 0, len(claims))
	if len(claims) == 0 {
		return out, nil
	}
	ids := make([]string, len(claims))
	for i, c := range claims {
		ids[i] = c.Lease.Key
	}
	items, err := src.Items(ctx, ids)
	if err != nil {
		return nil, err
	}
	by := make(map[string]Item, len(items))
	for _, it := range items {
		by[it.ID] = it
	}
	for _, c := range claims {
		it, ok := by[c.Lease.Key]
		if !ok {
			continue // out of sight, or a lease on a key that is no item
		}
		out = append(out, orchapi.RollupClaim{ID: it.ID, Title: it.Title, Status: it.Status, Priority: priorityPtr(it.Priority),
			Holder: c.Lease.Holder, Mine: c.Mine, Bound: c.Lease.Bound, Claimed: c.Lease.Claimed, Expires: c.Lease.Expires,
			Epic: epic(it), State: claimState(c, it)})
	}
	return out, nil
}
