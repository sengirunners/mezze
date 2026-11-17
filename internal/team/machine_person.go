package team

// A person's own machine (SPEC 8 "Operator class"): next to
// its operator class, a trusted machine of a team project is either a team
// machine (an action an ask carries runs on either owner's signed yes) or
// one person's own (it runs only on that person's). It is a setting the
// project's owner keeps at the authority (zm machine-class <machine> person
// <name>); the approvals module reads it through orchcore.MachinePeople, so
// a card with such an action is addressed to that person when it is made,
// and only their signed answer picks it. Nothing is a person's machine until
// an owner says so.
//
// The class itself lives in the kit's table (teamlink); this is zm's,
// because who answers an ask is zm's (approvals), not the kit's.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/teamlink"
)

var machinePersonMigrations = []store.Migration{
	// person: the identity name of the person whose own machine it is. Keyed
	// by the machine's name, not its node: a new node under the name stays
	// that person's until they say otherwise (the stricter rule holds).
	{Kind: store.KindRegistry, Name: "team/0102_machine_person", Script: `
CREATE TABLE team_machine_person (
	project TEXT NOT NULL,
	machine TEXT NOT NULL,
	person  TEXT NOT NULL,
	set_by  TEXT NOT NULL,
	set_at  INTEGER NOT NULL,
	PRIMARY KEY (project, machine)
);`},
}

var _ orchcore.MachinePeople = (*Service)(nil)

func (s *Service) personRoutes(r *core.Router) {
	r.HandleFunc("POST /v0/team/machines/{machine}/person", core.Authed, "this machine's owner on its socket (sent on to a joined project's authority), or at the authority a replica's node-bound token; its identity owns or administers the team project; changing or clearing a person's machine is that person's alone", jsonH(s.handleSetPerson))
	r.HandleFunc("GET /v0/team/machines/person", core.Authed, "as POST /v0/team/machines/{machine}/person", jsonH(s.handleListPeople))
}

// personCaller is the caller of a person route: this machine's owner on its
// socket, or (at the authority) a replica's node-bound token.
func personCaller(r *http.Request) (*authz.Subject, bool, error) {
	sub := core.SubjectOf(r.Context())
	replica := sub != nil && sub.Node != "" && sub.Machine != ""
	if !replica && (sub == nil || sub.Via != authz.ViaLocal) {
		return nil, false, api.Errorf(api.CodeForbidden, "only the project's owner, on their machine's socket, says whose machine is whose").
			WithHint("run it there: zm machine-class <machine> person <name> -P <project>")
	}
	return sub, replica, nil
}

// handleSetPerson is POST /v0/team/machines/{machine}/person.
func (s *Service) handleSetPerson(r *http.Request) (any, error) {
	ctx := r.Context()
	sub, replica, err := personCaller(r)
	if err != nil {
		return nil, err
	}
	var req orchapi.MachinePersonRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		return nil, err
	}
	machine := r.PathValue("machine")
	if req.Project == "" {
		return nil, api.Errorf(api.CodeBadRequest, "name the team project %s is in", machine)
	}
	if sub.Session != "" {
		// An agent never says whose machine is whose: it would widen or
		// narrow whose yes runs things there.
		return nil, api.Errorf(api.CodeForbidden, "a session does not say whose machine is whose: a person does, on their own machine")
	}
	req.Person = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(req.Person)), "human:")
	if !replica {
		if x := s.ByName(req.Project); x != nil {
			snap := x.Snapshot()
			if err := teamlink.Ended(snap); err != nil {
				return nil, err
			}
			fwd := req
			fwd.Project = snap.Project.ID
			var out orchapi.MachinePerson
			err := s.CallRemote(ctx, x, http.MethodPost, "/v0/team/machines/"+url.PathEscape(machine)+"/person", fwd, &out)
			return out, teamlink.OlderFor(err, x, "a person's own machines")
		}
	}
	project, err := s.OwnerProject(ctx, sub, req.Project, "says whose machine is whose")
	if err != nil {
		return nil, err
	}
	me := strings.ToLower(sub.Identity.Name)
	db := s.d.Store.Registry()
	if req.Person != "" {
		// A person's own machine is theirs to declare: no one marks another
		// person's machine (which would make only the marker's yes count
		// there), and only on a machine they alone joined.
		if req.Person != me {
			return nil, api.Errorf(api.CodeForbidden, "only %s marks a machine as their own", req.Person).
				WithHint(req.Person + " runs zm machine-class " + machine + " person " + req.Person + " on their machine")
		}
		if !s.knownMachine(ctx, project, machine) {
			return nil, api.NotFound("machine", machine)
		}
		if err := s.ownerPerson(ctx, project, req.Person); err != nil {
			return nil, err
		}
		if err := s.joinedAlone(ctx, project, machine, me); err != nil {
			return nil, err
		}
		res, err := db.ExecContext(ctx, `INSERT INTO team_machine_person (project, machine, person, set_by, set_at) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (project, machine) DO UPDATE SET set_by=excluded.set_by, set_at=excluded.set_at WHERE team_machine_person.person = excluded.person`,
			project, machine, req.Person, me, store.Millis(time.Now()))
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, s.someoneElses(ctx, project, machine)
		}
		return s.machinePerson(ctx, project, machine)
	}
	// Clearing: the person themselves; or any owner once that person no
	// longer owns or administers the project (their yes could run nothing
	// there anyway). Each is one conditional statement, so a racing change
	// is never overwritten.
	res, err := db.ExecContext(ctx, `DELETE FROM team_machine_person WHERE project=? AND machine=? AND person=?`, project, machine, me)
	if err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		cur, err := s.machinePerson(ctx, project, machine)
		if err != nil {
			return nil, err
		}
		if cur.Person != "" {
			owns, _, err := s.personOwns(ctx, project, cur.Person)
			if err != nil {
				return nil, err // never read a failed lookup as "no longer owns"
			}
			if owns {
				return nil, s.someoneElses(ctx, project, machine)
			}
			if _, err := db.ExecContext(ctx, `DELETE FROM team_machine_person WHERE project=? AND machine=? AND person=?`, project, machine, cur.Person); err != nil {
				return nil, err
			}
		}
	}
	return s.machinePerson(ctx, project, machine)
}

// someoneElses is the refusal for changing another person's own machine.
func (s *Service) someoneElses(ctx context.Context, project, machine string) error {
	cur, err := s.machinePerson(ctx, project, machine)
	if err != nil {
		return err
	}
	return api.Errorf(api.CodeForbidden, "%s is %s's own machine: only %s changes that", machine, cur.Person, cur.Person)
}

// joinedAlone checks that person is the one person whose machine machine
// is by its joins (AskRunTargets' people: the identities whose live
// node-bound tokens joined it; the authority's own machine is its human's).
func (s *Service) joinedAlone(ctx context.Context, project, machine, person string) error {
	ts, err := s.AskRunTargets(ctx, project)
	if err != nil {
		return err
	}
	for _, t := range ts {
		if t.Machine != machine {
			continue
		}
		if len(t.People) == 1 && strings.EqualFold(t.People[0], person) {
			return nil
		}
		return api.Errorf(api.CodeForbidden, "%s was not joined by %s alone, so it is not %s's own machine", machine, person, person)
	}
	return api.Errorf(api.CodeForbidden, "%s has not been heard from in this project, so whose it is is not known", machine)
}

// handleListPeople is GET /v0/team/machines/person?project=.
func (s *Service) handleListPeople(r *http.Request) (any, error) {
	ctx := r.Context()
	sub, replica, err := personCaller(r)
	if err != nil {
		return nil, err
	}
	name := r.URL.Query().Get("project")
	if name == "" {
		return nil, api.Errorf(api.CodeBadRequest, "name the team project: ?project=")
	}
	if !replica {
		if x := s.ByName(name); x != nil {
			snap := x.Snapshot()
			if err := teamlink.Ended(snap); err != nil {
				return nil, err
			}
			var out orchapi.MachinePersonList
			err := s.CallRemote(ctx, x, http.MethodGet, "/v0/team/machines/person?project="+url.QueryEscape(snap.Project.ID), nil, &out)
			return out, teamlink.OlderFor(err, x, "a person's own machines")
		}
	}
	project, err := s.OwnerProject(ctx, sub, name, "reads whose machine is whose")
	if err != nil {
		return nil, err
	}
	out := orchapi.MachinePersonList{Project: project, Machines: []orchapi.MachinePerson{}}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT machine, person, set_by, set_at FROM team_machine_person WHERE project=? ORDER BY machine`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m orchapi.MachinePerson
		var at int64
		if err := rows.Scan(&m.Machine, &m.Person, &m.SetBy, &at); err != nil {
			return nil, err
		}
		t := time.UnixMilli(at).UTC()
		m.SetAt = &t
		out.Machines = append(out.Machines, m)
	}
	return out, rows.Err()
}

// knownMachine reports whether machine is one of project's: one with
// numbers for it, or one that joined it.
func (s *Service) knownMachine(ctx context.Context, project, machine string) bool {
	if machine == s.d.Config.Machine {
		return true
	}
	if rt, err := s.runner(); err == nil && slices.ContainsFunc(rt.ProjectMachines(ctx, project), func(m api.Machine) bool { return m.Machine == machine }) {
		return true
	}
	j, err := s.d.Projects.MachineJoin(ctx, project, machine)
	return err == nil && len(j.Nodes) > 0
}

// ownerPerson checks that name is a human identity here that owns or
// administers project: only such a person can ever answer an action there.
func (s *Service) ownerPerson(ctx context.Context, project, name string) error {
	owns, found, err := s.personOwns(ctx, project, name)
	switch {
	case err != nil:
		return err
	case !found:
		return api.Errorf(api.CodeBadRequest, "no person here is named %q", name).WithHint("zm people lists them")
	case !owns:
		return api.Errorf(api.CodeBadRequest, "%s neither owns nor administers the project, so could never approve an action there", name)
	}
	return nil
}

// personOwns reports whether name is a human identity here (found) that
// owns or administers project (owns). err is a failed lookup, never an
// answer: a caller must not read it as "does not own".
func (s *Service) personOwns(ctx context.Context, project, name string) (owns, found bool, err error) {
	ids, err := s.d.Projects.Identities(ctx)
	if err != nil {
		return false, false, err
	}
	for _, id := range ids {
		if id.Kind != api.IdentityHuman || !strings.EqualFold(id.Name, name) {
			continue
		}
		sub, err := s.d.Projects.IdentitySubject(ctx, id.ID)
		if err != nil {
			return false, true, err
		}
		admin, err := s.d.ProjectAdmin(ctx, sub, project)
		return admin, true, err
	}
	return false, false, nil
}

// machinePerson is machine's row in project ({Machine} alone for a team
// machine).
func (s *Service) machinePerson(ctx context.Context, project, machine string) (orchapi.MachinePerson, error) {
	m := orchapi.MachinePerson{Machine: machine}
	var at int64
	err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT person, set_by, set_at FROM team_machine_person WHERE project=? AND machine=?`,
		project, machine).Scan(&m.Person, &m.SetBy, &at)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return m, nil
	case err != nil:
		return m, err
	}
	t := time.UnixMilli(at).UTC()
	m.SetAt = &t
	return m, nil
}

// MachinePerson implements orchcore.MachinePeople. project is a ULID. Only
// the project's authority keeps the setting: elsewhere it is unknown, never
// "a team machine".
func (s *Service) MachinePerson(ctx context.Context, project, machine string) (string, error) {
	if s.ByName(project) != nil {
		return "", api.Errorf(api.CodeUnavailable, "only the project's authority knows whose machine %s is", machine)
	}
	m, err := s.machinePerson(ctx, project, machine)
	return m.Person, err
}
