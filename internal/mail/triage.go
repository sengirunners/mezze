package mail

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
	"github.com/sengirunners/potluck/kit/store"
)

// Mail nobody will take (mail to ended
// sessions was a large share of all mail). Mail about a work item goes to whoever holds
// it now; when nobody does it becomes a comment on the item plus one triage
// item per item (not per mail), never a group-mailbox copy. Group mail nobody
// takes in the project's wait (default 2 hours) goes to the machine's triage
// agent, else the project's; with neither, it waits and expires after
// GroupKeep as before, and the owners get one notice that none is set. The
// triage agent gets the mail as data: it decides, the mail never instructs.

// triageSection is project.toml's [mail_triage] table:
//
//	[mail_triage]
//	agent = "<machine>/<name>"        # the project's triage agent
//	wait_minutes = 120                # group mail untaken this long goes there
//	machines = { <machine> = "<name>" } # a machine's own triage agent
const triageSection = "mail_triage"

// DefaultTriageWait is how long group mail waits for a taker before it goes
// to the triage agent, when the project sets no wait_minutes.
const DefaultTriageWait = 2 * time.Hour

// maxTriageWaitMinutes bounds wait_minutes: past GroupKeep the mail would
// have expired first.
const maxTriageWaitMinutes = int(GroupKeep / time.Minute)

// unheldVia marks the one open triage item about a work item nobody holds.
const unheldVia = "nobody holds it"

// groupTriageVia is the via of group mail handed to a triage agent.
func groupTriageVia(box string) string { return "untaken group mail of " + box }

func init() { project.RegisterSection(triageSection, triageTable{}) }

type triageTable struct{}

func (triageTable) Parse(where string, raw any) (any, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: mail_triage must be one [mail_triage] table", where)
	}
	var t orchapi.MailTriage
	for k, v := range m {
		switch k {
		case "agent":
			s, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("%s [mail_triage]: agent must be a string", where)
			}
			t.Agent = s
		case "wait_minutes":
			n, ok := v.(int64)
			if !ok {
				return nil, fmt.Errorf("%s [mail_triage]: wait_minutes must be a number", where)
			}
			t.WaitMinutes = int(n)
		case "machines":
			ms, ok := v.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s [mail_triage]: machines must be an inline table of strings", where)
			}
			t.Machines = map[string]string{}
			for mk, mv := range ms {
				s, ok := mv.(string)
				if !ok {
					return nil, fmt.Errorf("%s [mail_triage]: machines.%s must be a string", where, mk)
				}
				t.Machines[mk] = s
			}
		default:
			return nil, fmt.Errorf("%s [mail_triage]: unknown key %s (agent, wait_minutes, machines)", where, k)
		}
	}
	return t, nil
}

func (triageTable) Validate(v any, _ project.File) error {
	t, _ := v.(orchapi.MailTriage)
	if t.Agent != "" && !triageRef(t.Agent, "") {
		return api.Errorf(api.CodeBadRequest, "[mail_triage] agent %q is not <machine>/<name> or a session address", t.Agent)
	}
	for m, a := range t.Machines {
		if _, err := api.ParseAddress("machine:" + m); err != nil || m != strings.ToLower(m) {
			return api.Errorf(api.CodeBadRequest, "[mail_triage] machines: %q is not a machine name", m)
		}
		if !triageRef(a, m) {
			return api.Errorf(api.CodeBadRequest, "[mail_triage] machines.%s: %q is not a session name or address on %s", m, a, m)
		}
	}
	if t.WaitMinutes < 0 || t.WaitMinutes > maxTriageWaitMinutes {
		return api.Errorf(api.CodeBadRequest, "[mail_triage] wait_minutes must be 1..%d (0: the default)", maxTriageWaitMinutes)
	}
	return nil
}

func (triageTable) Encode(v any) string {
	t, _ := v.(orchapi.MailTriage)
	if t.Agent == "" && t.WaitMinutes == 0 && len(t.Machines) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n[mail_triage]\n")
	if t.Agent != "" {
		fmt.Fprintf(&b, "agent = %s\n", project.TOMLString(t.Agent))
	}
	if t.WaitMinutes != 0 {
		fmt.Fprintf(&b, "wait_minutes = %d\n", t.WaitMinutes)
	}
	if len(t.Machines) > 0 {
		ms := make([]string, 0, len(t.Machines))
		for m := range t.Machines {
			ms = append(ms, m)
		}
		sort.Strings(ms)
		for i, m := range ms {
			ms[i] = project.TOMLKey(m) + " = " + project.TOMLString(t.Machines[m])
		}
		fmt.Fprintf(&b, "machines = { %s }\n", strings.Join(ms, ", "))
	}
	return b.String()
}

// triageRef reports whether ref names a session: <machine>/<name> or a
// session address (on machine, when given). A bare name is valid only for a
// machine's own entry.
func triageRef(ref, machine string) bool {
	if m, _, ok := api.SessionNameRef(ref); ok {
		return m != "" && (machine == "" || m == machine) || m == "" && machine != ""
	}
	a, err := api.ParseAddress(ref)
	return err == nil && a.Kind == api.KindSession && (machine == "" || a.Machine == machine)
}

func (s *Service) triageSettings(project string) orchapi.MailTriage {
	t, _ := s.d.Projects.Section(project, triageSection).(orchapi.MailTriage)
	return t
}

// triageWait is how long project's group mail waits before triage.
func (s *Service) triageWait(project string) time.Duration {
	if n := s.triageSettings(project).WaitMinutes; n > 0 {
		return time.Duration(n) * time.Minute
	}
	return DefaultTriageWait
}

// triageAgent is the live session that takes machine's untaken mail in
// project's area: machine's own triage agent, else the project's; "" when neither
// is set or neither is live and reachable from this daemon. A name resolves
// only on this machine (SPEC 8 "Names"); another machine's agent is named by
// its session address.
func (s *Service) triageAgent(ctx context.Context, project, area, machine string, ended func(string) bool) string {
	t := s.triageSettings(project)
	for _, ref := range []string{t.Machines[machine], t.Agent} {
		if ref == "" {
			continue
		}
		if !strings.Contains(ref, "/") {
			ref = machine + "/" + ref
		}
		if addr := s.liveSession(ctx, project, area, ref, ended); addr != "" {
			return addr
		}
	}
	return ""
}

// liveSession resolves ref (<machine>/<name> or a session address) to a
// live session this daemon delivers to that is registered in project and
// reads area (its own area, or the whole project), or "": mail handed to a
// session that cannot read it would sit unseen for good.
func (s *Service) liveSession(ctx context.Context, project, area, ref string, ended func(string) bool) string {
	addr := ""
	if m, _, named := api.SessionNameRef(ref); named {
		sn, ok := orchcore.Sessions(s.d).(orchcore.SessionNames)
		if !ok || m != s.machine() {
			return ""
		}
		ss, ok := sn.ResolveName(ctx, ref)
		if !ok {
			return ""
		}
		addr = ss.Address
	} else if a, err := api.ParseAddress(ref); err == nil && a.Kind == api.KindSession && (a.Machine == s.machine() || s.team(project)) {
		addr = a.String()
	}
	if addr == "" || ended(addr) {
		return ""
	}
	ss, ok := orchcore.Sessions(s.d).Lookup(ctx, addr)
	if !ok || ss.Project != project || ss.Area != "" && ss.Area != area {
		return ""
	}
	return addr
}

// reachable reports whether agent (set for machine, or the project's when
// "") is one this daemon can resolve: a session address, or a name of a
// session on this machine. "" (taking it back) is.
func (s *Service) reachable(agent, machine string) bool {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return true
	}
	m, _, named := api.SessionNameRef(agent)
	if !named {
		return true // an address (or invalid: Validate answers that)
	}
	return cmp.Or(m, machine) == s.machine()
}

// itemMail reports whether m is about a work item: a ref filed in the
// item's own area (as an event carries it).
func itemMail(m row) bool { return m.Ref != "" && m.RefArea == m.Area }

// refuseRetry is how long the sweep waits before asking an item that
// refused an unheld comment again (each ask may reach a tracker).
const refuseRetry = time.Hour

// unheld takes mail m about a work item nobody live holds: it records the
// mail on the item as a comment (as zm, quoting it as data), and in one
// transaction takes m as the machine and opens one triage item about the item
// unless one is open (to machine's triage agent, else machine's group
// mailbox). ok=false: nothing was taken (the item took no comment: no work
// module, gone or refused; or m is restricted, whose words never go where
// the item's readers see them), and the caller keeps today's route. A
// concurrent taker wins: ok=true, nothing more is made.
func (s *Service) unheld(ctx context.Context, db *sql.DB, project, machine string, m row, ended func(string) bool) (bool, error) {
	rec, can := orchcore.Work(s.d).(orchcore.WorkRecorder)
	if !can || !itemMail(m) || m.ParentRestricted || m.Audience != "" || m.Needs == orchapi.NeedsHuman {
		return false, nil
	}
	if m.Via != unheldVia { // a triage item whose taker ended carries no new news
		if s.refusedLately(m.ID) {
			return false, nil
		}
		// Only the item where the mail was filed: a moved item (or one now in
		// another area) has other readers, so the mail keeps its route.
		_, area, where, err := rec.ResolveItem(ctx, s.d.System(), project, m.Ref)
		if err == nil && (where != project || area != m.RefArea) {
			err = api.Errorf(api.CodeConflict, "%s is now in another project or area", m.Ref)
		}
		if err == nil {
			err = rec.RecordComment(ctx, s.d.System(), project, m.Ref, m.ID, unheldComment(m))
		}
		if err != nil {
			s.noteRefused(m.ID)
			s.d.Log.Info("mail: work item took no comment; mail keeps its route", "project", project, "ref", m.Ref, "mail", m.ID, "err", err)
			return false, nil
		}
	}
	to := s.triageAgent(ctx, project, m.Area, machine, ended)
	if to == "" {
		to = "machine:" + machine
	}
	sys := s.d.System().Address
	t := row{Mail: orchapi.Mail{ID: newID(), Project: project, Area: m.Area, From: sys, To: to, Recipient: to, Via: unheldVia,
		Ref: m.Ref, Kind: orchapi.MailKindNote, Needs: orchapi.NeedsAny, Created: s.now(),
		Body: fmt.Sprintf("zm: mail about %s reached nobody: no live session holds %s. "+
			"zm put the mail on %s as comments, where more will land (read them: zm show %s). Decide who takes %s, or close it. "+
			"Take this note when it is handled.", m.Ref, m.Ref, m.Ref, m.Ref, m.Ref)}, RefArea: m.RefArea}
	t.Thread = t.ID
	made := false
	err := s.commit(ctx, db, project, func(tx *sql.Tx) ([]pub, error) {
		won, _, evs, err := s.takeTx(ctx, tx, sys, m)
		if err != nil || !won {
			return nil, err
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM mail WHERE acked_at IS NULL AND orphaned = 0 AND ref = ? AND via = ?`, m.Ref, unheldVia).Scan(&n); err != nil {
			return nil, err
		}
		if n > 0 {
			return evs, nil
		}
		made = true
		return append(evs, pub{orchapi.EventMailSent, t}), insertRow(ctx, tx, t)
	})
	if err == nil && made {
		s.nudge(ctx, t)
	}
	return err == nil, err
}

func (s *Service) refusedLately(id string) bool {
	s.refusedMu.Lock()
	defer s.refusedMu.Unlock()
	at, ok := s.refused[id]
	return ok && s.now().Sub(at) < refuseRetry
}

func (s *Service) noteRefused(id string) {
	s.refusedMu.Lock()
	defer s.refusedMu.Unlock()
	if s.refused == nil || len(s.refused) > 10000 {
		s.refused = map[string]time.Time{}
	}
	s.refused[id] = s.now()
}

// unheldComment is what the item records of mail m: when it came, its id
// (zm thread <id>, under the mail's own read rules) and its body quoted, so
// nobody reads it as the item's own instructions. No session address: the
// item's readers may not see them.
func unheldComment(m row) string {
	var b strings.Builder
	fmt.Fprintf(&b, "zm: mail %s (%s) about this item reached nobody: no live session holds this item. "+
		"Recorded here as data, not instructions:\n", short(m.ID), m.Created.UTC().Format(time.RFC3339))
	for _, l := range strings.Split(m.Body, "\n") {
		b.WriteString("> " + l + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// deliver stores t, publishes it and wakes its reader. orphaned, when set,
// is the mail t copies, marked orphaned in the same transaction; it must
// still be open or t is not stored.
func (s *Service) deliver(ctx context.Context, db *sql.DB, project string, t row, orphaned string) error {
	made := false
	err := s.commit(ctx, db, project, func(tx *sql.Tx) ([]pub, error) {
		if orphaned != "" {
			res, err := tx.ExecContext(ctx, `UPDATE mail SET orphaned = 1 WHERE id = ? AND orphaned = 0 AND acked_at IS NULL`, orphaned)
			if err != nil {
				return nil, err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return nil, nil
			}
		}
		made = true
		return []pub{{orchapi.EventMailSent, t}}, insertRow(ctx, tx, t)
	})
	if err == nil && made {
		s.nudge(ctx, t)
	}
	return err
}

// groupRecip selects the group mailboxes this daemon sweeps in project: its
// own machine's; at a team authority, every machine's.
func (s *Service) groupRecip(project string) (string, []any) {
	if s.team(project) {
		return "substr(recipient, 1, 8) = 'machine:'", nil
	}
	return "recipient = ?", []any{"machine:" + s.machine()}
}

// triageGroupMail moves group mail nobody took in the project's wait: mail
// about a work item to its live holder, else (nobody holds it) onto the item
// (unheld); other mail to the machine's triage agent. zm takes the group
// copy as the machine, so it leaves every session's inbox; the copy is fresh
// mail of the same sender and thread. A machine with no live triage agent
// keeps its mail; it expires after GroupKeep (logged, no report).
func (s *Service) triageGroupMail(ctx context.Context, db *sql.DB, project string, ended func(string) bool) error {
	recip, args := s.groupRecip(project)
	// Mail about an item first: a backlog of other mail never starves it.
	old, err := listRows(ctx, db, project, groupExpireWhere+` AND `+recip+` AND created <= ? ORDER BY ref = '', id LIMIT 500`,
		append(args, store.Millis(s.now().Add(-s.triageWait(project))))...)
	if err != nil || len(old) == 0 {
		return err
	}
	holders, unset := map[string]heldBy{}, map[string]bool{}
	for _, g := range old {
		box, _ := api.ParseAddress(g.Recipient)
		if itemMail(g) && g.Via != unheldVia {
			h, held := s.liveHolder(ctx, project, g, holders, ended)
			if h != "" {
				if err := s.handOn(ctx, db, project, g, h, "holder of "+g.Ref); err != nil {
					return err
				}
				continue
			}
			if !held {
				if done, err := s.unheld(ctx, db, project, box.Machine, g, ended); err != nil || done {
					if err != nil {
						return err
					}
					continue
				}
			}
		}
		to := s.triageAgent(ctx, project, g.Area, box.Machine, ended)
		if to == "" {
			unset[box.Machine] = true
			continue
		}
		if to == g.From {
			continue // the triage agent sent it; it expires as before
		}
		if err := s.handOn(ctx, db, project, g, to, groupTriageVia(g.Recipient)); err != nil {
			return err
		}
	}
	if len(unset) > 0 {
		s.d.Log.Info("mail: group mail waits with no live triage agent", "project", project, "machines", sortedKeys(unset))
	}
	return nil
}

// handOn takes group mail g as the machine and copies it to the session to
// (fresh mail: same sender, thread and ref, via), in one transaction: a
// concurrent taker wins and nothing is copied; a failure copies and takes
// nothing.
func (s *Service) handOn(ctx context.Context, db *sql.DB, project string, g row, to, via string) error {
	if g.Via == unheldVia {
		via = unheldVia // still the one triage item about its ref
	}
	cp := g
	cp.ID, cp.Recipient, cp.Via, cp.OrphanOf, cp.Orphaned = newID(), to, via, "", false
	cp.Created, cp.AckedBy, cp.AckedAt, cp.Members, cp.Batch = s.now(), "", nil, nil, ""
	made := false
	err := s.commit(ctx, db, project, func(tx *sql.Tx) ([]pub, error) {
		won, _, evs, err := s.takeTx(ctx, tx, s.d.System().Address, g)
		if err != nil || !won {
			return nil, err
		}
		made = true
		return append(evs, pub{orchapi.EventMailSent, cp}), insertRow(ctx, tx, cp)
	})
	if err == nil && made {
		s.nudge(ctx, cp)
	}
	return err
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ---- settings routes (GET and PUT /v0/mail/triage) ----

func (s *Service) handleTriageGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	t, err := s.d.WriteTarget(ctx, "", authz.Read)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if !core.SubjectOf(ctx).Scope.WholeProject(t.Project.ID) {
		api.WriteError(w, api.Errorf(api.CodeForbidden, "only a reader of the whole project sees its triage settings"))
		return
	}
	v := s.triageSettings(t.Project.ID)
	v.Project = t.Project.Slug
	v.WaitMinutes = int(s.triageWait(t.Project.ID) / time.Minute) // answers carry the wait in force
	api.WriteJSON(w, http.StatusOK, v)
}

func (s *Service) handleTriagePut(w http.ResponseWriter, r *http.Request) {
	var u orchapi.MailTriageUpdate
	if err := api.DecodeJSON(r, &u); err != nil {
		api.WriteError(w, err)
		return
	}
	ctx := r.Context()
	t, err := s.d.WriteTarget(ctx, "", authz.Admin)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if _, ok := s.d.Projects.File(t.Project.ID); !ok {
		api.WriteError(w, api.NotFound("project", t.Project.Slug).WithHint("a project joined from another machine is set on its authority"))
		return
	}
	u.Machine = strings.ToLower(strings.TrimSpace(u.Machine))
	if u.Agent != nil && !s.reachable(*u.Agent, u.Machine) {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "a session name resolves only on its own machine (%s here): give that triage agent's session address", s.machine()))
		return
	}
	v, err := s.d.Projects.SetSection(ctx, t.Project.ID, triageSection, func(old any) (any, error) {
		c, _ := old.(orchapi.MailTriage)
		if u.Agent != nil {
			a := strings.TrimSpace(*u.Agent)
			if u.Machine == "" {
				c.Agent = a
			} else {
				ms := map[string]string{}
				for k, v := range c.Machines {
					ms[k] = v
				}
				if a == "" {
					delete(ms, u.Machine)
				} else {
					ms[u.Machine] = a
				}
				c.Machines = ms
				if len(ms) == 0 {
					c.Machines = nil
				}
			}
		} else if u.Machine != "" {
			return nil, api.Errorf(api.CodeBadRequest, "machine names whose triage agent to set: give agent too")
		}
		if u.WaitMinutes != nil {
			c.WaitMinutes = *u.WaitMinutes
		}
		if c.Agent == "" && c.WaitMinutes == 0 && len(c.Machines) == 0 {
			return nil, nil
		}
		return c, nil
	})
	if err != nil {
		api.WriteError(w, err)
		return
	}
	c, _ := v.(orchapi.MailTriage)
	c.Project = t.Project.Slug
	c.WaitMinutes = int(s.triageWait(t.Project.ID) / time.Minute)
	api.WriteJSON(w, http.StatusOK, c)
}

// triageRoutes registers the settings routes (Routes calls it).
func (s *Service) triageRoutes(r *core.Router) {
	r.HandleFunc("GET /v0/mail/triage", core.Authed, "read on the whole resolved project (an area guest is forbidden)", s.handleTriageGet)
	r.HandleFunc("PUT /v0/mail/triage", core.Authed, "admin on the resolved project, whose file this daemon holds", s.handleTriagePut)
}
