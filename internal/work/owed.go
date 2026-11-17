package work

import (
	"context"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/work/claimlog"
	"github.com/sengirunners/mezze/internal/work/owedrows"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

// owedSource is this module's part of what a session still owes: the items
// its leases hold, and the open pull requests linked to them or to the item
// it last held (owedrows has the rules).
func (s *Service) owedSource() orchcore.OwedSource {
	return orchcore.OwedSource{Name: "work", Owed: s.owed}
}

func (s *Service) owed(ctx context.Context, sub *authz.Subject, sc authz.Scope, who orchcore.OwedWho) (orchcore.OwedPart, error) {
	asked := map[string]bool{}
	for _, a := range who.Addresses() {
		asked[a] = true
	}
	ls, err := s.liveLeases(ctx, sc)
	if err != nil {
		return orchcore.OwedPart{}, err
	}
	var out orchcore.OwedPart
	var failed error
	stale := false
	leased := map[string]bool{} // project + key, so a held item is not read twice
	read := func(project, key, holder string, l *lease) {
		il, isItem, err := s.keyItem(ctx, sc, project, key)
		if err == nil && isItem {
			var it orchapi.WorkItem
			if it, err = s.load(ctx, sub, il); err == nil {
				it, err = s.render(ctx, sub, il, it, false)
			}
			if err == nil {
				if l == nil && it.Owner != "" && it.Owner != holder {
					return // another session holds it now: the PR is theirs
				}
				stale = stale || it.Stale != ""
				h := owedrows.Held{Item: it, Holder: s.d.Redact(ctx, sub, holder), Leased: l != nil}
				if l != nil {
					c := l.Claimed
					h.Claimed = &c
				}
				out.Items = append(out.Items, owedrows.Rows(h)...)
				return
			}
		}
		// Hidden or gone answers like missing: nothing to owe here. Anything else
		// is a failure the list must say, never a quiet gap.
		if err != nil && !api.IsCode(err, api.CodeNotFound) {
			failed = err
		}
	}
	for i := range ls {
		l := &ls[i]
		if !asked[l.Holder] {
			continue
		}
		leased[l.Project+"\x00"+l.Key] = true
		read(l.Project, l.Key, l.Holder, l)
	}
	for _, ss := range who.Sessions { // the item a session last held: its PR may outlive the lease
		if ss.Work == "" || leased[ss.Project+"\x00"+ss.Work] {
			continue
		}
		leased[ss.Project+"\x00"+ss.Work] = true
		read(ss.Project, ss.Work, ss.Address, nil)
	}
	// An item a session claimed and no longer holds: the claim history says
	// who had it last, so its PR is still theirs unless someone holds it now.
	incomplete := false
	var started time.Time // when the earliest asked session began, if all are known
	for i, ss := range who.Sessions {
		if ss.Started.IsZero() {
			started = time.Time{}
			break
		}
		if i == 0 || ss.Started.Before(started) {
			started = ss.Started
		}
	}
	var projects []store.ProjectDB
	if len(asked) > 0 {
		projects = s.d.Store.Projects(sc, authz.Read)
	}
	for _, p := range projects {
		claims, bad := claimlog.LastClaimers(ctx, p.DB, s.team(p.ID), sc, p.ID, asked, time.Now().Add(-claimlog.Window), started)
		incomplete = incomplete || bad
		for key, holder := range claims {
			if !leased[p.ID+"\x00"+key] {
				leased[p.ID+"\x00"+key] = true
				read(p.ID, key, holder, nil)
			}
		}
	}
	if incomplete {
		out.Gaps = append(out.Gaps, orchcore.Partial("work", "the claim history could not be read in full: a pull request of an item it no longer holds may be missing"))
	}
	if failed != nil { // the rows read stand; the one that failed is said, not dropped
		out.Gaps = append(out.Gaps, orchcore.Partial("work", "an item it holds could not be read just now: its work and pull requests may be missing"))
	}
	if stale {
		out.Gaps = append(out.Gaps, orchcore.Partial("work", "a tracker's mirror is not current: its items and pull requests are as last seen"))
	}
	return out, nil
}
