package session

// Who and reach (SPEC 8): whose session it is and how far its launcher can
// act, resolved by the daemon that answers. A local session's launcher is
// this machine's owner on its socket (reach all); a replica's session at a
// team authority (presence) belongs to the identity its machine joined as,
// and reaches what that node-bound token is capped to: the project, or an
// area of it.

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// identTTL bounds how long a resolved identity is cached.
const identTTL = time.Minute

type identEntry struct {
	id api.Identity
	at time.Time
}

// identity resolves an identity name or ULID through the registry, cached.
func (s *Service) identity(ctx context.Context, ref string) (api.Identity, bool) {
	if ref == "" || s.d.Projects == nil {
		return api.Identity{}, false
	}
	if v, ok := s.idents.Load(ref); ok {
		if e := v.(identEntry); time.Since(e.at) < identTTL {
			return e.id, true
		}
	}
	id, err := s.d.Projects.Identity(ctx, ref)
	if err != nil {
		return api.Identity{}, false
	}
	s.idents.Store(ref, identEntry{id: id, at: time.Now()})
	return id, true
}

// dress sets Who and Reach on a session this daemon answers for, and
// keeps a term_title that must not leave the machine (orchapi.SharesTermTitle)
// from any reader but its owner (a token or tailnet caller: a teammate
// through the proxy). No subject: an event, whose bus does the same per
// reader (publishNow, events.Input.OwnerOnly).
func (s *Service) dress(ctx context.Context, ss orchapi.Session) orchapi.Session {
	if ss.Address == "" {
		return ss
	}
	return s.dressWith(ctx, ss, s.parents(ctx, []orchapi.Session{ss})[ss.Address])
}

// dressWith is dress with the session's parent already read. A parent the
// reader cannot see is left out, like a missing one (SPEC 6).
func (s *Service) dressWith(ctx context.Context, ss orchapi.Session, parent string) (out orchapi.Session) {
	// Decorations may introduce text from live samples or shared prompts. Apply
	// the durable hold last, without taking outputIO: callers can already hold
	// its read side while delivering an authorized output response.
	defer func() {
		out = s.maskSessionOutputMetadata(ctx, out)
		// A post-redaction metadata reload must retain the caller's terminal
		// title policy, just like the original decoration did.
		if sub := core.SubjectOf(ctx); sub != nil && !sub.Scope.IsAll() && !orchapi.SharesTermTitle(out.Agent, out.Command) {
			out.TermTitle = ""
		}
	}()
	if ss.Address == "" {
		return ss
	}
	ss.Parent = parent
	owner := true
	if sub := core.SubjectOf(ctx); sub != nil && !sub.Scope.IsAll() {
		owner = false
		if ss.Parent != "" && !s.Visible(ctx, sub.Scope, ss.Parent) {
			ss.Parent = ""
		}
		if !orchapi.SharesTermTitle(ss.Agent, ss.Command) {
			ss.TermTitle = ""
		}
	}
	ss.Fg, ss.Busy, ss.Ports, ss.Jobs, ss.Unreachable = nil, false, nil, nil, ""
	if owner && ss.Machine == s.d.Config.Machine {
		if ss.Managed && ss.State != orchapi.StateEnded {
			ss.Unreachable = s.unreachableWhy(ss.ID)
		}
		// fg, busy, ports, jobs: never to a token or tailnet reader. No
		// subject: an event, which carries what the sampler published.
		ss = s.withSample(ss, core.SubjectOf(ctx) == nil)
	}
	ss = s.dressShared(ctx, ss)
	if ss.Machine != s.d.Config.Machine { // a replica's session (presence): Launcher is the identity's name
		ss.Who = nil
		if id, ok := s.identity(ctx, ss.Launcher); ok {
			ss.Who = &orchapi.SessionWho{Name: id.Name, Kind: id.Kind}
		} else if ss.Launcher != "" {
			ss.Who = &orchapi.SessionWho{Name: ss.Launcher, Kind: api.IdentityHuman}
		}
		if ss.Reach == "" {
			ss.Reach = orchapi.ReachProject
		}
		return ss
	}
	id, ok := s.identity(ctx, ss.Launcher)
	if !ok && s.d.Projects != nil {
		id = s.d.Projects.LocalIdentity()
		ok = id.Name != ""
	}
	if ok {
		ss.Who = &orchapi.SessionWho{Name: id.Name, Kind: id.Kind}
	}
	ss.Reach = orchapi.ReachAll
	return ss
}

// maskSessionOutputMetadata leaves routing, identity and lifecycle facts usable
// while user-derived display text is withheld. A failed hold query fails closed.
func (s *Service) maskSessionOutputMetadata(ctx context.Context, ss orchapi.Session) orchapi.Session {
	if ss.ID == "" || ss.Machine != s.d.Config.Machine {
		return ss
	}
	var redacted, held bool
	if s.d.Store == nil || s.d.Store.Registry() == nil {
		return sessionMetadataContent(ss, orchapi.Session{})
	}
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT output_redacted,EXISTS(SELECT 1 FROM session_output_holds WHERE session_id=sessions.id) FROM sessions WHERE id=?`, ss.ID).Scan(&redacted, &held)
	if err == nil && !redacted && !held {
		return ss
	}
	current := orchapi.Session{}
	if err == nil && !held {
		if r, ok := s.load(ctx, ss.ID); ok && r.Project == ss.Project && r.Area == ss.Area {
			current = r.Session
			current.Subagents = r.subs
		}
	}
	// Old event snapshots can outlive a completed hold. Once a session has
	// been redacted, display content comes only from its current scrubbed row.
	ss = sessionMetadataContent(ss, current)
	// Samples and the shared prompt decoration are not row-backed output.
	// Do not let a cached decoration restore text after a hold clears.
	ss.LastPrompt = nil
	ss.Fg, ss.Busy, ss.Ports, ss.Jobs, ss.Unreachable = nil, false, nil, nil, ""

	return ss
}

// sessionMetadataContent replaces descriptive content without changing the
// caller's identity, lifecycle snapshot, or decision routing facts.
func sessionMetadataContent(ss, current orchapi.Session) orchapi.Session {
	ss.Command = append([]string(nil), current.Command...)
	ss.Cwd, ss.Worktree, ss.Branch, ss.Scratch, ss.Sandbox = current.Cwd, current.Worktree, current.Branch, current.Scratch, current.Sandbox
	ss.Title, ss.Note, ss.Activity, ss.TermTitle = current.Title, current.Note, current.Activity, current.TermTitle
	ss.ActivityAt = current.ActivityAt
	if ss.Outcome != nil {
		outcome := *ss.Outcome
		outcome.Note, outcome.Evidence = "", ""
		if current.Outcome != nil {
			outcome.Note, outcome.Evidence = current.Outcome.Note, current.Outcome.Evidence
		}
		ss.Outcome = &outcome
	}
	if ss.RuntimeAgent != nil {
		agent := *ss.RuntimeAgent
		agent.Task = ""
		if current.RuntimeAgent != nil && sameBinding(&agent, current.RuntimeAgent) {
			agent.Task = current.RuntimeAgent.Task
		}
		ss.RuntimeAgent = &agent
	}
	if len(ss.Subagents) > 0 {
		ss.Subagents = append([]orchapi.Subagent(nil), ss.Subagents...)
		for i := range ss.Subagents {
			ss.Subagents[i].Description, ss.Subagents[i].Activity = "", ""
			for _, a := range current.Subagents {
				if a.ID == ss.Subagents[i].ID {
					ss.Subagents[i].Description, ss.Subagents[i].Activity = a.Description, a.Activity
					break
				}
			}
		}
	}
	return ss
}

// dressAll is dress on every session of a list.
func (s *Service) dressAll(ctx context.Context, ss []orchapi.Session) {
	_ = s.dressAllErr(ctx, ss)
}

// dressAllErr is dressAll, also saying a failed parent read.
func (s *Service) dressAllErr(ctx context.Context, ss []orchapi.Session) error {
	parents, err := s.parentsErr(ctx, ss)
	for i := range ss {
		ss[i] = s.dressWith(ctx, ss[i], parents[ss[i].Address])
	}
	return err
}

// testHookParentsRead, when set, fails parentsErr's reads with what it returns.
var testHookParentsRead func() error

// parents reads who started each session, by address: session_lineage for
// this machine's, the presence row for a replica's. It is a display fact, so
// a failed read leaves the parent out and fails nothing.
func (s *Service) parents(ctx context.Context, ss []orchapi.Session) map[string]string {
	out, _ := s.parentsErr(ctx, ss)
	return out
}

// parentsErr is parents, also saying the first read that failed (the map
// then holds what the other reads found): owed filters on the parent, so it
// must not drop a failure as a missing one.
func (s *Service) parentsErr(ctx context.Context, ss []orchapi.Session) (map[string]string, error) {
	var local, remote []string
	for _, x := range ss {
		if x.Machine == s.d.Config.Machine {
			local = append(local, x.ID)
		} else if x.Address != "" {
			remote = append(remote, x.Address)
		}
	}
	out := make(map[string]string, len(ss))
	var failed error
	read := func(q string, keys []string, address func(string) string) {
		if len(keys) == 0 {
			return
		}
		js, _ := json.Marshal(keys)
		var rows *sql.Rows
		var err error
		if testHookParentsRead != nil {
			err = testHookParentsRead()
		}
		if err == nil {
			rows, err = s.d.Store.Registry().QueryContext(ctx, q, string(js))
		}
		if err != nil {
			failed = cmp.Or(failed, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var k, p string
			if rows.Scan(&k, &p) == nil {
				out[address(k)] = p
			}
		}
		failed = cmp.Or(failed, rows.Err())
	}
	read(`SELECT id, parent FROM session_lineage WHERE id IN (SELECT value FROM json_each(?))`, local, func(id string) string {
		return api.SessionAddress(s.d.Config.Machine, id)
	})
	read(`SELECT address, parent FROM presence WHERE parent != '' AND address IN (SELECT value FROM json_each(?))`, remote, func(a string) string { return a })
	return out, failed
}

// presenceReach is the reach of a replica's sessions: its token (the
// heartbeat's caller) sees the whole project, or only areas of it.
func presenceReach(sub *authz.Subject, project string) string {
	if sub == nil {
		return orchapi.ReachProject
	}
	if whole, _ := sub.Scope.Areas(project); whole {
		return orchapi.ReachProject
	}
	return orchapi.ReachArea
}

// Origin implements orchcore.SessionOrigins.
func (s *Service) Origin(ctx context.Context, address string) (launcher, parent string, ok bool) {
	ss, ok := s.Lookup(ctx, address)
	if !ok {
		return "", "", false
	}
	return ss.Launcher, s.parents(ctx, []orchapi.Session{ss})[ss.Address], true
}
