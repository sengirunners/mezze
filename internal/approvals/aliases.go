package approvals

// Inbox aliases: the label names an identity also answers to in
// owner:<name>, ask:<name> and needs-<name> (GET /v0/decisions). Teams label
// work with short names ("owner:ivo") that are not the identity's name
// ("ivogh"); an alias says so explicitly, at the authority that
// holds the identity. Identity hygiene (SPEC section 16) holds: an alias is
// matched only exactly, is canonical (checkName), and may not look like
// another identity or another identity's alias, either way round (a new
// identity may not take an alias's name: IdentityNameFree). Only the
// authority's owner, an owner of every project the identity is in, or the
// identity itself when it owns every project here, sets one (mayAlias). An alias only chooses which rows the caller already reads
// count as its own; it grants nothing.

import (
	"context"
	"net/http"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var aliasesMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0004_aliases", Script: `
CREATE TABLE identity_aliases (
	alias    TEXT PRIMARY KEY,
	skeleton TEXT NOT NULL UNIQUE,
	identity TEXT NOT NULL,
	added_by TEXT NOT NULL,
	created  INTEGER NOT NULL
);
CREATE INDEX identity_aliases_identity ON identity_aliases (identity);`},
}

// aliasReserved are words an inbox label uses for a role, not a person.
var aliasReserved = map[string]bool{"owner": true, "chief": true, "chiefs": true, "me": true, "everyone": true}

func (s *Service) aliasRoutes(r *core.Router) {
	r.HandleFunc("GET /v0/aliases", core.Authed, "the identity itself (default the caller), the machine owner on the socket, or another identity that is admin of every project it holds a grant in; anyone else gets the 404 of a missing identity", s.handleAliases)
	r.HandleFunc("POST /v0/aliases", core.Authed, "the machine owner on the socket, another identity that owns every project the target holds a grant in, or the target itself when it owns every project here; ownership by its own grants (core.Deps.ProjectAdmin); else 403 for the identity itself", s.handleAddAlias)
	r.HandleFunc("DELETE /v0/aliases/{alias}", core.Authed, "as POST, for the alias's identity; anyone else gets not_found", s.handleRemoveAlias)
}

// mayAlias says whether sub may add or remove who's aliases. An alias
// applies at every project on this authority, so only someone who already
// answers for all of them may set one: the authority's owner (the machine
// owner on its socket), or another identity that owns or administers every
// project who holds a grant in. An identity sets its own only when it owns or
// administers every project here: a free short name ("bea" while the
// identity is beagh) would otherwise go to whoever asks first.
// Ownership is core.Deps.ProjectAdmin, judged by the identity's own grants,
// so it holds from a member Mac's capped link too. This is how the authority
// already treats identity names (made by its owner or a project's admin);
// nobody approves anything, and the setter is recorded (added_by).
func (s *Service) mayAlias(ctx context.Context, sub *authz.Subject, who api.Identity) bool {
	switch {
	case sub == nil:
		return false
	case sub.Via == authz.ViaLocal && sub.TokenID == "":
		return true
	}
	var projects []string
	if sub.Identity.ID == who.ID {
		for _, p := range s.d.Projects.List(authz.All()) {
			if p.ID != api.ProjectNone {
				projects = append(projects, p.ID)
			}
		}
	} else {
		tuples, err := s.d.Projects.Tuples(ctx, who.ID)
		if err != nil {
			return false
		}
		for _, t := range tuples {
			p, _, err := authz.ParseObject(t.Object)
			if err != nil {
				return false
			}
			projects = append(projects, p)
		}
	}
	for _, p := range projects {
		if ok, err := s.d.ProjectAdmin(ctx, sub, p); err != nil || !ok {
			return false
		}
	}
	return len(projects) > 0
}

// aliasTarget resolves ref ("" = the caller) to an identity sub may read
// (the identity itself may read its own) or, with change, manage.
func (s *Service) aliasTarget(ctx context.Context, sub *authz.Subject, ref string, change bool) (api.Identity, error) {
	if ref == "" {
		if sub == nil || sub.Identity.ID == "" {
			return api.Identity{}, api.Errorf(api.CodeBadRequest, "the caller has no identity: name one with identity")
		}
		ref = sub.Identity.ID
	}
	who, err := s.d.Projects.Identity(ctx, ref)
	if err != nil {
		return api.Identity{}, api.NotFound("identity", ref)
	}
	if s.mayAlias(ctx, sub, who) {
		return who, nil
	}
	if sub != nil && sub.Identity.ID == who.ID {
		if !change {
			return who, nil
		}
		return api.Identity{}, api.Errorf(api.CodeForbidden, "an identity sets its own inbox aliases only when it owns every project on this authority").
			WithHint("the authority's owner, or an admin of every project " + who.Name + " is in, sets them: zm identity alias add <name> --identity " + who.Name)
	}
	return api.Identity{}, api.NotFound("identity", ref)
}

func (s *Service) handleAliases(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	who, err := s.aliasTarget(ctx, core.SubjectOf(ctx), r.URL.Query().Get("identity"), false)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	v, err := s.aliasView(ctx, who)
	respond(w, v, err)
}

// aliasView is who's aliases and who set each.
func (s *Service) aliasView(ctx context.Context, who api.Identity) (orchapi.Aliases, error) {
	out := orchapi.Aliases{Identity: who.Name, Aliases: []string{}, AddedBy: map[string]string{}}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT alias, added_by FROM identity_aliases WHERE identity = ? ORDER BY alias`, who.ID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var a, by string
		if err := rows.Scan(&a, &by); err != nil {
			return out, err
		}
		out.Aliases = append(out.Aliases, a)
		out.AddedBy[a] = by
	}
	return out, rows.Err()
}

func (s *Service) handleAddAlias(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req orchapi.AliasRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	sub := core.SubjectOf(ctx)
	who, err := s.aliasTarget(ctx, sub, req.Identity, true)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	if err := s.addAlias(ctx, who, req.Alias, sub.Address); err != nil {
		api.WriteError(w, err)
		return
	}
	v, err := s.aliasView(ctx, who)
	respond(w, v, err)
}

func (s *Service) handleRemoveAlias(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	alias := r.PathValue("alias")
	var owner string
	_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT identity FROM identity_aliases WHERE alias = ?`, alias).Scan(&owner)
	if owner == "" {
		api.WriteError(w, api.NotFound("alias", alias))
		return
	}
	who, err := s.aliasTarget(ctx, core.SubjectOf(ctx), owner, true)
	if api.IsCode(err, api.CodeForbidden) {
		api.WriteError(w, err)
		return
	}
	if err != nil {
		api.WriteError(w, api.NotFound("alias", alias))
		return
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `DELETE FROM identity_aliases WHERE alias = ? AND identity = ?`, alias, who.ID); err != nil {
		api.WriteError(w, err)
		return
	}
	v, err := s.aliasView(ctx, who)
	respond(w, v, err)
}

// addAlias records raw as one of who's names, refusing a name that is not
// canonical, is a role word, or looks like anyone else's name or alias.
func (s *Service) addAlias(ctx context.Context, who api.Identity, raw, by string) error {
	name, err := checkName(raw)
	if err != nil {
		return err
	}
	if aliasReserved[name] {
		return api.Errorf(api.CodeBadRequest, "alias %q is a role word in inbox labels, not a person", name)
	}
	if name == who.Name {
		return api.Errorf(api.CodeBadRequest, "%q is already %s's name", name, who.Name)
	}
	sk := skeleton(name)
	ids, err := s.d.Projects.Identities(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if id.ID != who.ID && skeleton(id.Name) == sk {
			return api.Errorf(api.CodeConflict, "alias %q looks like identity %q", name, id.Name).
				WithHint("an alias must look like no one else's name")
		}
	}
	var holder string
	_ = s.d.Store.Registry().QueryRowContext(ctx, `SELECT identity FROM identity_aliases WHERE skeleton = ?`, sk).Scan(&holder)
	switch {
	case holder == who.ID:
		return nil // already this identity's (the same name or its lookalike: keep the first)
	case holder != "":
		return api.Errorf(api.CodeConflict, "alias %q (or a lookalike) belongs to another identity", name).
			WithHint("the authority's owner, or an admin of every project that identity is in, can remove it: zm identity alias rm " + name)
	}
	_, err = s.d.Store.Registry().ExecContext(ctx, `INSERT INTO identity_aliases (alias, skeleton, identity, added_by, created) VALUES (?, ?, ?, ?, ?)`,
		name, sk, who.ID, by, ms(s.now()))
	if store.IsUnique(err) {
		return api.Errorf(api.CodeConflict, "alias %q was just taken", name)
	}
	return err
}

// aliasesOf is identity's aliases, sorted.
func (s *Service) aliasesOf(ctx context.Context, identity string) ([]string, error) {
	out := []string{}
	if identity == "" {
		return out, nil
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT alias FROM identity_aliases WHERE identity = ? ORDER BY alias`, identity)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Aliases is identity's aliases, sorted; nil when they cannot be read
// (core.Deps.Aliases).
func (s *Service) Aliases(ctx context.Context, identity string) []string {
	out, err := s.aliasesOf(ctx, identity)
	if err != nil || len(out) == 0 {
		return nil
	}
	return out
}

// personNames is every name a person answers to at this authority: each
// identity's name and every alias. needs-<x> names someone only when x is
// one; anything else (needs-spec, needs-design-pass) is a plain tag.
func (s *Service) personNames(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	ids, err := s.d.Projects.Identities(ctx)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		out[strings.ToLower(id.Name)] = true
	}
	rows, err := s.d.Store.Registry().QueryContext(ctx, `SELECT alias FROM identity_aliases`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out[a] = true
	}
	return out, rows.Err()
}

// viewerNames are the names sub answers to in inbox labels: its identity's
// name and aliases, exactly.
func (s *Service) viewerNames(ctx context.Context, sub *authz.Subject) ([]string, error) {
	var names []string
	if sub != nil && sub.Identity.Name != "" {
		names = append(names, strings.ToLower(sub.Identity.Name))
	}
	if sub == nil {
		return names, nil
	}
	more, err := s.aliasesOf(ctx, sub.Identity.ID)
	return append(names, more...), err
}
