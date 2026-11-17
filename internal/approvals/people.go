package approvals

import (
	"context"
	"slices"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/store"
)

var peopleMigrations = []store.Migration{
	// owner_person: the person id of owner (api.Ask.OwnerPerson); asker_human:
	// the person whose machine or token asked.
	{Kind: store.KindRegistry, Name: "approvals/0010_people", Script: `
ALTER TABLE asks ADD COLUMN owner_person TEXT NOT NULL DEFAULT '';
ALTER TABLE asks ADD COLUMN asker_human TEXT NOT NULL DEFAULT '';
`},
}

// person is one person at this authority: the human identities
// linked to one GitHub account (core.Deps.LinkedAccount), else one identity.
// ID is GET /v0/people's Person.ID. Names, aliases and login are lowercase.
type person struct {
	ID      string
	Name    string // the least of Names, as /v0/people names a person
	Names   []string
	Aliases []string
	GitHub  string
	Answers bool              // one of its identities' own grants reaches admin on the project
	Reads   bool              // one of them reads the whole project
	Shown   []string          // the names of its identities with a grant in the project: what /v0/people shows
	AliasOf map[string]string // alias -> the identity name it belongs to
}

// match is how n names p: 1 by an identity name or alias, 2 by its GitHub
// login, 0 not at all. A name or alias outranks a login: logins are never
// checked against the names here, so one may equal another person's name.
func (p person) match(n string) int {
	switch {
	case slices.Contains(p.Names, n), slices.Contains(p.Aliases, n):
		return 1
	case p.GitHub != "" && p.GitHub == n:
		return 2
	}
	return 0
}

// label is how a refusal lists p: its name, then its other names, aliases
// and @login.
func (p person) label() string {
	name, also := p.Name, []string{}
	if len(p.Shown) > 0 { // never a linked name this project does not show
		name = p.Shown[0]
		also = append(also, p.Shown[1:]...)
	}
	also = append(also, p.Aliases...)
	if p.GitHub != "" {
		also = append(also, "@"+p.GitHub)
	}
	if len(also) == 0 {
		return name
	}
	return name + " (" + strings.Join(also, ", ") + ")"
}

type peopleCacheKey struct{}

// withPeopleCache lets one request read people once per project (a ctx
// that has a cache keeps it).
func withPeopleCache(ctx context.Context) context.Context {
	if _, ok := ctx.Value(peopleCacheKey{}).(map[string][]person); ok {
		return ctx
	}
	return context.WithValue(ctx, peopleCacheKey{}, map[string][]person{})
}

// people is every person with a human identity here, folded by person id,
// with Answers and Reads for project.
func (s *Service) people(ctx context.Context, project string) ([]person, error) {
	cache, _ := ctx.Value(peopleCacheKey{}).(map[string][]person)
	if out, ok := cache[project]; ok {
		return out, nil
	}
	out, err := s.loadPeople(ctx, project)
	if err == nil && cache != nil {
		cache[project] = out
	}
	return out, err
}

func (s *Service) loadPeople(ctx context.Context, project string) ([]person, error) {
	ids, err := s.d.Projects.Identities(ctx)
	if err != nil {
		return nil, err
	}
	var out []person
	at := map[string]int{}
	for _, id := range ids {
		if id.Kind != api.IdentityHuman {
			continue
		}
		key, login := id.ID, ""
		if s.d.LinkedAccount != nil {
			if acc, l := s.d.LinkedAccount(ctx, id.ID); acc != "" {
				key, login = acc, strings.ToLower(l)
			}
		}
		tuples, err := s.d.Projects.Tuples(ctx, id.ID)
		if err != nil {
			return nil, err
		}
		aliases, err := s.aliasesOf(ctx, id.ID)
		if err != nil {
			return nil, err
		}
		i, ok := at[key]
		if !ok {
			i = len(out)
			at[key] = i
			out = append(out, person{ID: key})
		}
		p := &out[i]
		name := strings.ToLower(id.Name)
		if p.Name == "" || name < p.Name {
			p.Name = name
		}
		p.Names = append(p.Names, name)
		for _, a := range aliases {
			if a = strings.ToLower(a); !slices.Contains(p.Aliases, a) {
				p.Aliases = append(p.Aliases, a)
				if p.AliasOf == nil {
					p.AliasOf = map[string]string{}
				}
				p.AliasOf[a] = name
			}
		}
		if p.GitHub == "" {
			p.GitHub = login
		}
		grants := authz.FromTuples(tuples, nil)
		p.Answers = p.Answers || grants.Can(authz.Admin, authz.Object{Project: project})
		p.Reads = p.Reads || grants.Can(authz.Read, authz.Object{Project: project})
		for _, t := range tuples {
			if obj, _, err := authz.ParseObject(t.Object); err == nil && obj == project {
				p.Shown = append(p.Shown, name)
				break
			}
		}
	}
	for i := range out {
		slices.Sort(out[i].Names)
		slices.Sort(out[i].Aliases)
		slices.Sort(out[i].Shown)
	}
	slices.SortFunc(out, func(a, b person) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// resolvePerson is the one person among people that n (any identity name,
// alias or GitHub login, any case, human:<name> too) names, and the identity
// name the ask keeps for them (its mail goes to human:<that name>): n when it
// is an identity name, the identity an alias belongs to, else for a login the
// first identity the project shows. None or several is an error listing the
// candidates.
func resolvePerson(people []person, n string) (person, string, error) {
	n = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(n)), "human:")
	n = strings.TrimPrefix(n, "@")
	var hits []person
	how := 0
	for _, p := range people {
		switch m := p.match(n); {
		case m == 0 || how != 0 && m > how:
		case m < how || how == 0:
			hits, how = []person{p}, m
		default:
			hits = append(hits, p)
		}
	}
	list := func(ps []person) string {
		ls := make([]string, len(ps))
		for i, p := range ps {
			ls[i] = p.label()
		}
		return strings.Join(ls, "; ")
	}
	switch {
	case len(hits) == 1 && slices.Contains(hits[0].Names, n):
		return hits[0], n, nil
	case len(hits) == 1 && hits[0].AliasOf[n] != "":
		return hits[0], hits[0].AliasOf[n], nil
	case len(hits) == 1 && len(hits[0].Shown) > 0:
		return hits[0], hits[0].Shown[0], nil
	case len(hits) == 1:
		return hits[0], hits[0].Name, nil
	case len(hits) > 1:
		return person{}, "", api.Errorf(api.CodeBadRequest, "%q names more than one person who can answer this project's asks", n).
			WithHint("name one of them by a name only they have: " + list(hits))
	}
	return person{}, "", api.Errorf(api.CodeBadRequest, "%q is not a person who owns or administers this project, so they cannot answer its asks", n).
		WithHint("name one of: " + list(people) + " (zm people lists them; zm grant gives someone admin)")
}

// answering is people narrowed to those who answer the project's asks.
func answering(people []person) []person {
	var out []person
	for _, p := range people {
		if p.Answers {
			out = append(out, p)
		}
	}
	return out
}

// samePerson reports whether a and b name the same person who answers
// project's asks.
func (s *Service) samePerson(ctx context.Context, project, a, b string) bool {
	people, err := s.people(ctx, project)
	if err != nil {
		return false
	}
	pa, _, ea := resolvePerson(answering(people), a)
	pb, _, eb := resolvePerson(answering(people), b)
	return ea == nil && eb == nil && pa.ID == pb.ID
}

// askerName is the name of the person who asked: the caller's own human
// identity (a token's), else the human of the asker's machine (askerHuman).
func askerName(sub *authz.Subject, human string) string {
	if sub != nil && sub.Identity.Kind == api.IdentityHuman && sub.Identity.Name != "" {
		return sub.Identity.Name
	}
	return strings.TrimPrefix(human, "human:")
}
