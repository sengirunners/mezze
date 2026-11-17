package session

import (
	"context"
	"encoding/json"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

// maxOwedSessions bounds one owed read of sessions; one more row says there
// are more, and the read reports itself partial.
var maxOwedSessions = 5000

// owedSource is this module's part of what a session still owes (SPEC 8
// "What a session still owes"): the sessions it started (its children, by
// lineage) that are still alive, which it has to watch or finish. On a team
// authority the replicas' live sessions count too (their parent is on the
// presence row).
func (s *Service) owedSource() orchcore.OwedSource {
	return orchcore.OwedSource{Name: "sessions", Owed: s.owed}
}

func (s *Service) owed(ctx context.Context, _ *authz.Subject, sc authz.Scope, who orchcore.OwedWho) (orchcore.OwedPart, error) {
	var out orchcore.OwedPart
	parents := who.Addresses()
	if len(parents) == 0 {
		return out, nil
	}
	// One query by lineage, so a session that updates meanwhile is neither
	// skipped nor read twice (a cursor over "newest first" would).
	where, args := sc.SQL("project", "area", authz.Read)
	js, _ := json.Marshal(parents)
	args = append(args, string(orchapi.StateEnded), string(js), maxOwedSessions+1)
	rows, err := listRows(ctx, s.d.Store.Registry(), "WHERE "+where+" AND state != ? AND id IN (SELECT id FROM session_lineage WHERE parent IN (SELECT value FROM json_each(?))) ORDER BY updated DESC, id DESC LIMIT ?", args...)
	if err != nil {
		return out, err
	}
	if len(rows) > maxOwedSessions {
		rows = rows[:maxOwedSessions]
		out.Gaps = append(out.Gaps, orchcore.Partial("sessions", "more running sessions than one read takes: the newest are listed"))
	}
	ss := make([]orchapi.Session, len(rows))
	for i, r := range rows {
		ss[i] = r.Session
	}
	// On a team authority, the replicas' live sessions (presence) too: their
	// parent is on the presence row.
	pres, err := s.listPresence(ctx, sc, orchapi.SessionQuery{})
	if err != nil {
		return out, err
	}
	if len(pres) >= presenceListCap {
		out.Gaps = append(out.Gaps, orchcore.Partial("sessions", "more sessions on other machines than one read takes: the most recently seen are listed"))
	}
	asked := map[string]bool{}
	for _, a := range parents {
		asked[a] = true
	}
	// A replica's session is owed only if its parent reads as asked: a failed
	// read would drop it, so say it. (The listed children below only lose a
	// displayed holder when theirs fails.)
	if s.dressAllErr(ctx, pres) != nil { // the presence row's parent, for the reader's view
		out.Gaps = append(out.Gaps, orchcore.Partial("sessions", "who started some sessions could not be read just now: sessions it started may be missing"))
	}
	for _, p := range pres {
		if p.State != orchapi.StateEnded && asked[p.Parent] {
			ss = append(ss, p)
		}
	}
	s.dressAll(ctx, ss) // sets Parent, for the reader's view
	seen := map[string]bool{}
	for _, c := range ss {
		if seen[c.Address] {
			continue
		}
		seen[c.Address] = true
		title := c.Title
		if title == "" {
			title = c.Name
		}
		started := c.Started
		out.Items = append(out.Items, orchapi.OwedItem{Kind: orchapi.OwedLane, Ref: c.Address, Title: title, State: string(c.State),
			Project: c.Project, Area: c.Area, Holder: c.Parent, Since: &started})
	}
	return out, nil
}

// SessionsOf implements orchcore.PersonSessions: the sessions sc may read
// that person's launcher started (Who names them): live ones, and ended ones
// updated since the cutoff, newest first, in one read (no cursor to drift).
func (s *Service) SessionsOf(ctx context.Context, sc authz.Scope, person string, since time.Time) ([]orchapi.Session, bool, error) {
	where, args := sc.SQL("project", "area", authz.Read)
	args = append(args, string(orchapi.StateEnded), store.Millis(since), maxOwedSessions+1)
	// A live session counts however long it has been idle; an ended one only
	// since the cutoff.
	rows, err := listRows(ctx, s.d.Store.Registry(), "WHERE "+where+" AND (state != ? OR updated >= ?) ORDER BY updated DESC, id DESC LIMIT ?", args...)
	if err != nil {
		return nil, false, err
	}
	truncated := len(rows) > maxOwedSessions
	if truncated {
		rows = rows[:maxOwedSessions]
	}
	all := make([]orchapi.Session, len(rows))
	for i, r := range rows {
		all[i] = r.Session
	}
	// On a team authority, the replicas' sessions (presence) too.
	pres, err := s.listPresence(ctx, sc, orchapi.SessionQuery{All: true})
	if err != nil {
		return nil, false, err
	}
	truncated = truncated || len(pres) >= presenceListCap
	for _, p := range pres {
		if p.State != orchapi.StateEnded || !p.Updated.Before(since) {
			all = append(all, p)
		}
	}
	s.dressAll(ctx, all)
	var out []orchapi.Session
	seen := map[string]bool{}
	for _, ss := range all {
		if seen[ss.Address] {
			continue
		}
		seen[ss.Address] = true
		if ss.Who != nil && ss.Who.Kind == api.IdentityHuman && ss.Who.Name == person {
			out = append(out, ss)
		}
	}
	return out, truncated, nil
}
