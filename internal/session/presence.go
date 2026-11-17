package session

// Presence (SPEC 17.3, round B2): on a team project's authority, the
// sessions of each replica, as its heartbeats report them. Sessions on a
// replica are local to it (spawn, attach, input and stop stay there); the
// authority keeps only what presence carries (address, area, state, work
// held, agent, title) so that mail can reach them, leases can be bound to
// them and readers see who holds what. Lookup, Visible, Live and List answer
// for these rows like for local sessions. A heartbeat older than
// PresenceStale counts as idle (note "presence stale"), so a bound lease
// held by a session whose replica went quiet stops renewing.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var presenceMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "session/0008_presence_outcome", Script: `ALTER TABLE presence ADD COLUMN outcome_kind TEXT NOT NULL DEFAULT '';`},
	{Kind: store.KindRegistry, Name: "session/0016_presence_parent", Script: `ALTER TABLE presence ADD COLUMN parent TEXT NOT NULL DEFAULT '';`},
	// A shared structured session's mode and capabilities.
	{Kind: store.KindRegistry, Name: "session/0020_presence_mode", Script: `ALTER TABLE presence ADD COLUMN mode TEXT NOT NULL DEFAULT '';
ALTER TABLE presence ADD COLUMN caps TEXT NOT NULL DEFAULT '';`},
	{Kind: store.KindRegistry, Name: "session/0002_presence", Script: `
CREATE TABLE presence (
	address  TEXT PRIMARY KEY,           -- <machine>/<session8> on a replica
	machine  TEXT NOT NULL,
	project  TEXT NOT NULL,
	area     TEXT NOT NULL DEFAULT '',
	identity TEXT NOT NULL DEFAULT '',   -- the replica's identity (its token's)
	agent    TEXT NOT NULL DEFAULT '',
	title    TEXT NOT NULL DEFAULT '',
	state    TEXT NOT NULL,
	note     TEXT NOT NULL DEFAULT '',
	managed  INTEGER NOT NULL DEFAULT 0,
	work     TEXT NOT NULL DEFAULT '',   -- the first key the replica says it holds
	started  INTEGER NOT NULL,
	seen     INTEGER NOT NULL,
	ended    INTEGER
);
CREATE INDEX presence_scope ON presence (project, area, state);
CREATE INDEX presence_machine ON presence (machine, project);`},
}

// PresenceStale is how old a heartbeat may be before its sessions count as
// idle (tests shorten it).
var PresenceStale = 60 * time.Second

// presenceKeep is how long ended presence rows are kept.
const presenceKeep = 7 * 24 * time.Hour

var _ orchcore.PresenceStore = (*Service)(nil)

const presenceCols = `address, machine, project, area, identity, agent, title, state, note, managed, work, started, seen, ended,
	activity, activity_at, term_title, reach, shared, in_use, outcome_kind, account, for_id, job_kind, tokens, mode, caps`

func scanPresence(sc scanner) (orchapi.Session, error) {
	var (
		ss                 orchapi.Session
		identity, state    string
		managed, shared    int
		started, seen      int64
		ended, actAt       sql.NullInt64
		inUse, outcomeKind string
		tokens             string
		caps               string
	)
	err := sc.Scan(&ss.Address, &ss.Machine, &ss.Project, &ss.Area, &identity, &ss.Agent, &ss.Title, &state, &ss.Note,
		&managed, &ss.Work, &started, &seen, &ended, &ss.Activity, &actAt, &ss.TermTitle, &ss.Reach, &shared, &inUse, &outcomeKind,
		&ss.Account, &ss.For, &ss.JobKind, &tokens, &ss.Mode, &caps)
	if err != nil {
		return ss, err
	}
	ss.Tokens = parseTokens(tokens)
	if caps != "" {
		var c orchapi.AgentCapabilities
		if json.Unmarshal([]byte(caps), &c) == nil {
			ss.Capabilities = &c
		}
	}
	if a, err := api.ParseAddress(ss.Address); err == nil {
		ss.ID = a.Session
	}
	if outcomeKind != "" {
		ss.Outcome = &orchapi.Outcome{Kind: orchapi.OutcomeKind(outcomeKind)}
	}
	ss.Launcher = identity
	ss.State = orchapi.SessionState(state)
	ss.Managed, ss.Shared = managed != 0, shared != 0
	if inUse != "" {
		var u orchapi.SessionInUse
		if json.Unmarshal([]byte(inUse), &u) == nil {
			ss.InUse = &u
		}
	}
	ss.Started = store.FromMillis(started)
	t := store.FromMillis(seen)
	ss.Seen, ss.Updated = &t, t
	ss.Ended = store.NullMillis(ended)
	ss.ActivityAt = store.NullMillis(actAt)
	return ss, nil
}

// effective applies the stale rule: a live session whose replica has not
// been heard from for PresenceStale is idle.
func effective(ss orchapi.Session, now time.Time) orchapi.Session {
	if ss.State != orchapi.StateEnded && ss.Seen != nil && now.Sub(*ss.Seen) > PresenceStale {
		ss.State, ss.Note = orchapi.StateIdle, "presence stale"
	}
	return ss
}

// remoteAddr reports whether address is a session on another machine.
func (s *Service) remoteAddr(address string) bool {
	a, err := api.ParseAddress(address)
	return err == nil && a.Kind == api.KindSession && a.Machine != s.d.Config.Machine
}

// loadPresence returns the presence row of a session on another machine.
func (s *Service) loadPresence(ctx context.Context, address string) (orchapi.Session, bool) {
	ss, ok, err := s.loadPresenceErr(ctx, address)
	if err != nil {
		s.d.Log.Warn("presence load", "address", address, "err", err)
	}
	return ss, ok
}

// loadPresenceErr is loadPresence telling missing (false, nil) from an error.
func (s *Service) loadPresenceErr(ctx context.Context, address string) (orchapi.Session, bool, error) {
	a, err := api.ParseAddress(address)
	if err != nil || a.Kind != api.KindSession || a.Machine == s.d.Config.Machine {
		return orchapi.Session{}, false, nil
	}
	ss, err := scanPresence(s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+presenceCols+` FROM presence WHERE address = ?`, a.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return orchapi.Session{}, false, nil
	}
	if err != nil {
		return orchapi.Session{}, false, err
	}
	return effective(ss, time.Now()), true, nil
}

// RecordPresence implements orchcore.PresenceStore.
func (s *Service) RecordPresence(ctx context.Context, project, machine, identity string, beat []orchapi.PresenceSession) error {
	reg := s.d.Store.Registry()
	now := storedTime(time.Now()) // events carry what the table keeps
	// Every row of the machine, whatever its project: a session that moved
	// here from another project (or area) leaves that one first.
	prev := map[string]orchapi.Session{}
	rows, err := reg.QueryContext(ctx, `SELECT `+presenceCols+` FROM presence WHERE machine = ?`, machine)
	if err != nil {
		return err
	}
	for rows.Next() {
		if ss, err := scanPresence(rows); err == nil {
			prev[ss.Address] = ss
		}
	}
	rows.Close()
	reach := presenceReach(core.SubjectOf(ctx), project)
	var changed []orchapi.Session
	updated := map[string]bool{} // only activity or term_title changed
	actOnly := map[string]bool{} // only the activity changed: a live-only event
	fresh := map[string]bool{}   // new to this project/area: registered
	var left []orchapi.Session   // where moved sessions were (old project/area)
	listed := map[string]bool{}
	err = store.Tx(ctx, reg, func(tx *sql.Tx) error {
		for _, p := range beat {
			a, err := api.ParseAddress(p.Address)
			if err != nil || a.Kind != api.KindSession || a.Machine != machine {
				return api.Errorf(api.CodeForbidden, "%s is not a session on %s", p.Address, machine)
			}
			listed[a.String()] = true
			if p.State == orchapi.StateEnded {
				old, had := prev[a.String()]
				if had && (old.Project != project || old.Area != p.Area) {
					continue
				}
				usage := beatUsage(p)
				if had && old.State == orchapi.StateEnded && old.Outcome != nil {
					// Its final tokens may come after its end: stored, not published.
					if usage.tokens != "" && usage.tokens != tokensJSON(old.Tokens) {
						if _, err := tx.ExecContext(ctx, `UPDATE presence SET tokens = ? WHERE address = ?`, usage.tokens, a.String()); err != nil {
							return err
						}
					}
					continue
				}
				ended := now
				if p.EndedAt != nil && !p.EndedAt.IsZero() && p.EndedAt.Before(now) {
					ended = storedTime(*p.EndedAt)
				}
				started := storedTime(p.Started)
				if started.IsZero() {
					started = ended
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO presence (address,machine,project,area,identity,agent,title,state,started,seen,ended,reach,outcome_kind,parent,account,for_id,job_kind,tokens) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT(address) DO UPDATE SET state=excluded.state,seen=excluded.seen,ended=excluded.ended,outcome_kind=excluded.outcome_kind,parent=CASE WHEN excluded.parent != '' THEN excluded.parent ELSE presence.parent END,`+keepUsage, a.String(), machine, project, p.Area, identity, oneLine(p.Agent), oneLine(textclean.Clean(p.Title)), string(orchapi.StateEnded), store.Millis(started), store.Millis(ended), store.Millis(ended), reach, string(p.OutcomeKind), presenceParent(p.Parent, machine), usage.account, usage.forID, usage.jobKind, usage.tokens); err != nil {
					return err
				}
				t := ended
				changed = append(changed, orchapi.Session{Address: a.String(), ID: a.Session, Machine: machine, Project: project, Area: p.Area, Launcher: identity, Agent: p.Agent, Title: oneLine(textclean.Clean(p.Title)), State: orchapi.StateEnded, Started: started, Updated: ended, Ended: &t, Seen: &t, Outcome: &orchapi.Outcome{Kind: p.OutcomeKind}, Reach: reach,
					Account: usage.account, For: usage.forID, JobKind: usage.jobKind, Tokens: parseTokens(usage.tokens)})
				continue
			}
			if p.Elsewhere {
				continue // present in another project's heartbeat: left as it is here
			}
			work := ""
			if len(p.Work) > 0 {
				work = p.Work[0]
			} else if old, had := prev[a.String()]; had && old.Project == project {
				work = old.Work // the last key it held stays on the row after the release
			}
			usage := beatUsage(p)
			started := storedTime(p.Started)
			if started.IsZero() {
				started = now
			}
			activity, title := clipLine(p.Activity), clipLine(p.TermTitle)
			if !orchapi.AgentRuntimes[strings.ToLower(oneLine(p.Agent))] {
				title = "" // an older replica: never keep a shell's title (SharesTermTitle)
			}
			var actAt *time.Time
			var actMs any
			if activity != "" && p.ActivityAt != nil && !p.ActivityAt.IsZero() {
				t := storedTime(*p.ActivityAt)
				if t.After(now) { // the replica's clock runs ahead
					t = now
				}
				actAt, actMs = &t, store.Millis(t)
			}
			inUse, inUseJS := p.InUse, ""
			if inUse != nil && p.Shared {
				u := orchapi.SessionInUse{By: oneLine(inUse.By), Attached: max(0, inUse.Attached), TypedAt: storedPtr(inUse.TypedAt)}
				inUse = &u
				b, _ := json.Marshal(u)
				inUseJS = string(b)
			} else {
				inUse = nil
			}
			// Mode and capabilities: a shared structured session's only (what
			// a member's turn or read through this authority needs).
			mode, capsJS := "", ""
			if p.Shared && p.Managed && p.Mode == orchapi.SessionModePipe {
				mode = orchapi.SessionModePipe
				if p.Capabilities != nil {
					b, _ := json.Marshal(p.Capabilities)
					capsJS = string(b)
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO presence (`+presenceCols+`, parent) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,?,?,?,?,?,?,'',?,?,?,?,?,?,?)
				ON CONFLICT (address) DO UPDATE SET project = excluded.project, area = excluded.area, identity = excluded.identity,
					agent = excluded.agent, title = excluded.title, state = excluded.state, note = excluded.note,
					managed = excluded.managed, work = excluded.work, seen = excluded.seen, ended = NULL,
					activity = excluded.activity, activity_at = excluded.activity_at, term_title = excluded.term_title,
                    reach = excluded.reach, shared = excluded.shared, in_use = excluded.in_use, outcome_kind = '',
                    mode = excluded.mode, caps = excluded.caps,
                    parent = CASE WHEN excluded.parent != '' THEN excluded.parent ELSE presence.parent END,`+keepUsage,
				a.String(), machine, project, p.Area, identity, oneLine(p.Agent), oneLine(textclean.Clean(p.Title)), string(p.State), oneLine(textclean.Clean(p.Note)),
				boolInt(p.Managed), work, store.Millis(started), store.Millis(now), activity, actMs, title, reach,
				boolInt(p.Shared && p.Managed), inUseJS, usage.account, usage.forID, usage.jobKind, usage.tokens, mode, capsJS, presenceParent(p.Parent, machine)); err != nil {
				return err
			}
			old, had := prev[a.String()]
			relocated := had && (old.Project != project || old.Area != p.Area)
			if relocated && old.State != orchapi.StateEnded {
				left = append(left, old)
			}
			fresh[a.String()] = !had || relocated
			moved := !had || relocated || old.State != p.State || old.Note != oneLine(textclean.Clean(p.Note))
			// A refreshed activity_at alone is stored, not published.
			sharedNow := p.Shared && p.Managed
			if moved || old.Activity != activity || old.TermTitle != title || old.Shared != sharedNow || !sameInUse(old.InUse, inUse) {
				t := now
				changed = append(changed, orchapi.Session{Address: a.String(), ID: a.Session, Machine: machine, Project: project,
					Area: p.Area, Launcher: identity, Agent: p.Agent, Title: oneLine(textclean.Clean(p.Title)), State: p.State, Note: oneLine(textclean.Clean(p.Note)),
					Managed: p.Managed, Work: work, Started: started, Updated: now, Seen: &t,
					Activity: activity, ActivityAt: actAt, TermTitle: title, Reach: reach, Shared: sharedNow, InUse: inUse,
					Account: or(usage.account, old.Account), For: or(usage.forID, old.For), JobKind: or(usage.jobKind, old.JobKind),
					Tokens: parseTokens(or(usage.tokens, tokensJSON(old.Tokens)))})
				updated[a.String()] = !moved
				// Live only when nothing but the activity changed: a title, agent, work or managed change rides along
				// with it in the stored event, so a replaying client sees it.
				actOnly[a.String()] = !moved && old.TermTitle == title && old.Title == oneLine(textclean.Clean(p.Title)) &&
					old.Agent == p.Agent && old.Work == work && old.Managed == p.Managed &&
					old.Shared == sharedNow && sameInUse(old.InUse, inUse)
			}
		}
		// The machine's other sessions in the project ended (or left it).
		for addr, old := range prev {
			if listed[addr] || old.State == orchapi.StateEnded || old.Project != project {
				continue // another project's rows are its own heartbeat's
			}
			if _, err := tx.ExecContext(ctx, `UPDATE presence SET state = ?, note = ?, ended = ?, seen = ? WHERE address = ?`,
				string(orchapi.StateEnded), "gone from presence", store.Millis(now), store.Millis(now), addr); err != nil {
				return err
			}
			t := now
			old.State, old.Note, old.Ended, old.Seen, old.Updated = orchapi.StateEnded, "gone from presence", &t, &t, now
			changed = append(changed, old)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, old := range left {
		s.publishLeft(old.Address, old.Project, old.Area)
	}
	for _, ss := range changed {
		typ := orchapi.EventSessionStateChanged
		if fresh[ss.Address] {
			typ = orchapi.EventSessionRegistered
		} else if ss.State == orchapi.StateEnded {
			typ = orchapi.EventSessionEnded
		} else if actOnly[ss.Address] {
			typ = activityOnly // not stored: superseded at once (SPEC 12)
		} else if updated[ss.Address] {
			typ = orchapi.EventSessionUpdated
		}
		s.presenceChanged(ctx, typ, ss)
	}
	return nil
}

// keepUsage updates a presence row's usage attribution from a beat; a beat
// without a field (an older replica, tokens not read yet) keeps the row's.
const keepUsage = ` account = CASE WHEN excluded.account != '' THEN excluded.account ELSE presence.account END,
	for_id = CASE WHEN excluded.for_id != '' THEN excluded.for_id ELSE presence.for_id END,
	job_kind = CASE WHEN excluded.job_kind != '' THEN excluded.job_kind ELSE presence.job_kind END,
	tokens = CASE WHEN excluded.tokens != '' THEN excluded.tokens ELSE presence.tokens END`

type presenceUsage struct{ account, forID, jobKind, tokens string }

// beatUsage is a beat's usage attribution, cleaned like a local session's.
func beatUsage(p orchapi.PresenceSession) presenceUsage {
	u := presenceUsage{account: cleanFor(p.Account), forID: cleanFor(p.For)}
	if orchapi.ValidJobKind(p.JobKind) {
		u.jobKind = p.JobKind
	}
	if t := p.Tokens; t != nil && t.Input >= 0 && t.CacheRead >= 0 && t.CacheWrite >= 0 && t.Output >= 0 {
		u.tokens = tokensJSON(t)
	}
	return u
}

// presenceParent is the parent a beat names, if it is a session on the
// beat's own machine. A replica speaks only for its own sessions.
func presenceParent(parent, machine string) string {
	a, err := api.ParseAddress(parent)
	if err != nil || a.Kind != api.KindSession || a.Machine != machine {
		return ""
	}
	return a.String()
}

// presenceChanged publishes a presence row's change; an ended one's bound
// leases go at once, like a local session's.
func (s *Service) presenceChanged(ctx context.Context, typ string, ss orchapi.Session) {
	s.presenceScopes.Store(ss.Address, object(ss))
	s.publish(typ, ss)
	if ss.State == orchapi.StateEnded {
		s.expirePrompts(ctx, ss.Address) // a shared session's pending prompts can no longer arrive
	}
	if ss.State == orchapi.StateEnded && orchcore.Work(s.d) != nil {
		if err := orchcore.Work(s.d).SessionEnded(context.WithoutCancel(ctx), ss.Address); err != nil && !api.IsCode(err, api.CodeNotImplemented) {
			s.d.Log.Warn("presence: release bound leases", "session", ss.Address, "err", err)
		}
	}
}

// moveLeft handles a session whose machine left its row's project (zm leave,
// a revoked token, the project gone solo) but that holds leases in another
// project of this authority its machine still has a token for: never ended
// (that would release its bound leases everywhere), it releases its bound
// leases in the project left and its row moves to that other project (the
// first by id), as the member's next heartbeat there would move it. Reports
// whether it moved.
func (s *Service) moveLeft(ctx context.Context, ss orchapi.Session, tokens map[string]bool) bool {
	lh, ok := orchcore.Work(s.d).(orchcore.LeaseHolders)
	pe, ok2 := orchcore.Work(s.d).(orchcore.SessionProjectEnder)
	if !ok || !ok2 {
		return false
	}
	var to []string
	for p := range tokens {
		if held, err := lh.HeldBy(ctx, p, ss.Address); err == nil && len(held) > 0 {
			to = append(to, p)
		}
	}
	if len(to) == 0 {
		return false
	}
	sort.Strings(to)
	// Nothing of the project left goes with it: its work (a key of that
	// project), identity and reach are blank until the session's next
	// heartbeat in its new project fills them.
	res, err := s.d.Store.Registry().ExecContext(ctx, `UPDATE presence SET project = ?, area = '', work = '', identity = '', reach = ''
		WHERE address = ? AND ended IS NULL AND seen = ?`, to[0], ss.Address, store.Millis(*ss.Seen))
	if err != nil {
		s.d.Log.Warn("presence move", "session", ss.Address, "err", err)
		return true // not ended either: the next reap tries again
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return true // heartbeaten since: the replica moved it itself
	}
	if err := pe.SessionLeftProject(context.WithoutCancel(ctx), ss.Address, ss.Project); err != nil {
		s.d.Log.Warn("presence: release the left project's bound leases", "session", ss.Address, "err", err)
	}
	s.publishLeft(ss.Address, ss.Project, ss.Area)
	ss.Project, ss.Area, ss.Note, ss.Work, ss.Launcher, ss.Reach = to[0], "", "", "", "", ""
	s.presenceChanged(ctx, orchapi.EventSessionRegistered, ss)
	return true
}

// presenceLost is how old a heartbeat may be before its sessions are ended
// (note "presence lost"): past bound_lease_ttl (plus two stale periods), so
// a bound lease the replica held has expired first, and never
// under ten stale periods.
func (s *Service) presenceLost() time.Duration {
	return max(10*PresenceStale, s.d.Config.BoundLeaseTTL()+2*PresenceStale)
}

var _ orchcore.PresenceReaper = (*Service)(nil)

// ReapPresence implements orchcore.PresenceReaper: it ends the live presence
// rows no replica stands behind any more. A row whose machine holds no live
// node-bound token for its project (zm leave, zm token revoke, a project
// gone solo) ends with note "replica left"; one whose heartbeat is older
// than presenceLost with "presence lost". The daemon calls it on every
// revocation and from the session ticker. A replica that comes back
// heartbeats its sessions live again.
func (s *Service) ReapPresence(ctx context.Context) {
	s.pruneElsewhere()
	reg := s.d.Store.Registry()
	rows, err := reg.QueryContext(ctx, `SELECT `+presenceCols+` FROM presence WHERE ended IS NULL`)
	if err != nil {
		s.d.Log.Warn("presence reap", "err", err)
		return
	}
	var live []orchapi.Session
	for rows.Next() {
		if ss, err := scanPresence(rows); err == nil {
			live = append(live, ss)
		}
	}
	rows.Close()
	if len(live) == 0 {
		return
	}
	now := storedTime(time.Now())
	lost := now.Add(-s.presenceLost())
	joined := map[string]map[string]bool{} // machine -> projects it holds a live token for
	var gone []orchapi.Session
	for _, ss := range live {
		ps, ok := joined[ss.Machine]
		if !ok {
			ps = map[string]bool{}
			if s.d.Projects != nil {
				for _, p := range s.d.Projects.MachineProjects(ctx, ss.Machine) {
					ps[p] = true
				}
			}
			joined[ss.Machine] = ps
		}
		switch {
		case !ps[ss.Project]:
			ss.Note = "replica left"
		case ss.Seen != nil && ss.Seen.Before(lost):
			ss.Note = "presence lost"
		default:
			continue
		}
		gone = append(gone, ss)
	}
	var ended []orchapi.Session
	for _, ss := range gone {
		if ss.Note == "replica left" && s.moveLeft(ctx, ss, joined[ss.Machine]) {
			continue
		}
		// Only a row still live and not heartbeaten since it was read ends.
		res, err := reg.ExecContext(ctx, `UPDATE presence SET state = ?, note = ?, ended = ? WHERE address = ? AND ended IS NULL AND seen = ?`,
			string(orchapi.StateEnded), ss.Note, store.Millis(now), ss.Address, store.Millis(*ss.Seen))
		if err != nil {
			s.d.Log.Warn("presence reap", "session", ss.Address, "err", err)
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		t := now
		ss.State, ss.Ended, ss.Updated = orchapi.StateEnded, &t, now
		ended = append(ended, ss)
	}
	for _, ss := range ended {
		s.presenceChanged(ctx, orchapi.EventSessionEnded, ss)
	}
}

// presenceVisible is Visible for a session on another machine.
func (s *Service) presenceVisible(ctx context.Context, sc authz.Scope, address string) bool {
	if o, ok := s.presenceScopes.Load(address); ok {
		return sc.Can(authz.Read, o.(authz.Object))
	}
	ss, ok := s.loadPresence(ctx, address)
	if !ok {
		return false
	}
	o := object(ss)
	s.presenceScopes.Store(ss.Address, o)
	return sc.Can(authz.Read, o)
}

// presenceListCap is the most presence rows one list reads.
const presenceListCap = 500

// listPresence is List's share from presence: replicas' sessions sc reads.
func (s *Service) listPresence(ctx context.Context, sc authz.Scope, q orchapi.SessionQuery) ([]orchapi.Session, error) {
	where, a := sc.SQL("project", "area", authz.Read)
	conds := []string{where}
	if q.State == "" && !q.All {
		conds, a = append(conds, "state != ?"), append(a, string(orchapi.StateEnded))
	}
	if q.Agent != "" {
		conds, a = append(conds, "agent = ?"), append(a, strings.ToLower(q.Agent))
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT `+presenceCols+` FROM presence WHERE `+strings.Join(conds, " AND ")+
		` ORDER BY seen DESC LIMIT `+strconv.Itoa(presenceListCap), a...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now()
	var out []orchapi.Session
	for rows.Next() {
		ss, err := scanPresence(rows)
		if err != nil {
			return nil, err
		}
		if ss = effective(ss, now); q.State != "" && ss.State != q.State {
			continue
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

// prunePresence drops ended presence rows older than presenceKeep.
func (s *Service) prunePresence() {
	_, _ = s.d.Store.Registry().ExecContext(s.ctx, `DELETE FROM presence WHERE ended IS NOT NULL AND ended < ?`,
		store.Millis(time.Now().Add(-presenceKeep)))
	// A machine's numbers nobody has heartbeaten for as long (machine.go).
	_, _ = s.d.Store.Registry().ExecContext(s.ctx, `DELETE FROM presence_machines WHERE seen < ?`,
		store.Millis(time.Now().Add(-presenceKeep)))
}

// sameInUse compares two in_use values (times to the stored precision).
func sameInUse(a, b *orchapi.SessionInUse) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ta, tb := storedPtr(a.TypedAt), storedPtr(b.TypedAt)
	return a.By == b.By && a.Attached == b.Attached && (ta == nil) == (tb == nil) && (ta == nil || ta.Equal(*tb))
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
