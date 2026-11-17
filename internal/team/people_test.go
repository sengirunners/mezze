package team_test

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// GET /v0/people: the teammates the caller shares projects with,
// merged across authorities on a member's socket. A guest sees only those
// whose grant overlaps its area, never a project it does not read. Since
// Potluck 200fe32d the caller's own row is listed too, flagged You (and only
// that row): it tells the caller nothing it did not know.
func TestPeopleScopedAndMerged(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	cr, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	vr, _ := replica(t, tn, a, "vida-mac", "nVIDA", "harbor/solver", "vida", "guest")
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bill", Role: "guest", Project: "harbor", Area: "billing"}))(t)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "sam", Role: "guest", Project: "harbor", Area: "solver"}))(t)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "side"}))(t)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "secret", Role: "member", Project: "side"}))(t)
	register(t, cr, "harbor", "bea-1")
	heartbeat(t, cr)
	me := a.dm.Deps.Projects.LocalIdentity().Name
	harbor := mustf(a.owner.GetProject(ctx, "harbor"))(t)

	list := func(c *client.Client) map[string]api.Person {
		t.Helper()
		var l api.PeopleList
		if err := c.Do(ctx, http.MethodGet, "/v0/people", nil, &l); err != nil {
			t.Fatal(err)
		}
		if len(l.Skipped) != 0 {
			t.Fatalf("skipped %+v", l.Skipped)
		}
		out := map[string]api.Person{}
		for _, p := range l.People {
			if _, dup := out[p.Name]; dup {
				t.Fatalf("%s twice in %+v", p.Name, l.People)
			}
			out[p.Name] = p
		}
		return out
	}
	// names lists the people seen; exactly the caller's row is flagged You.
	names := func(m map[string]api.Person, self string) string {
		t.Helper()
		var out []string
		for n, p := range m {
			if p.You != (n == self) {
				t.Fatalf("%s you=%v (caller %s) in %+v", n, p.You, self, m)
			}
			out = append(out, n)
		}
		return strings.Join(sortedStrings(out), ",")
	}
	sorted := func(n ...string) string { return strings.Join(sortedStrings(n), ",") }

	owner := list(a.owner)
	if got := names(owner, me); got != sorted("bill", "bea", me, "sam", "secret", "vida") {
		t.Fatalf("authority owner sees %s", got)
	}
	bea := owner["bea"]
	if bea.Kind != api.IdentityHuman || len(bea.Projects) != 1 || bea.Projects[0].Project != harbor.ID || bea.Projects[0].Slug != "harbor" ||
		bea.Projects[0].Identity == "" {
		t.Fatalf("bea %+v", bea)
	}
	if strings.Join(bea.Machines, ",") != "bea-mac" || bea.Seen == nil {
		t.Fatalf("bea's machines and seen from presence: %+v", bea)
	}
	if got := names(list(at(a.owner, "side", "")), me); got != sorted(me, "secret") {
		t.Fatalf("?project narrows: %s", got)
	}

	// The member's socket: the authority's answer for harbor (never side).
	if got := names(list(cr.owner), "bea"); got != sorted("bill", "bea", me, "sam", "vida") {
		t.Fatalf("member sees %s", got)
	}
	// The solver guest learns nothing outside solver: only solver's other
	// guest (and itself), no whole-project member (the owner and bea) and
	// not billing's.
	guest := list(vr.owner)
	if got := names(guest, "vida"); got != sorted("sam", "vida") {
		t.Fatalf("guest sees %s", got)
	}
	// Who owns a project (answers) goes only to a caller reading it whole:
	// the owner's harbor row, never anything a guest sees.
	answers := func(m map[string]api.Person) string {
		var out []string
		for n, p := range m {
			for _, pp := range p.Projects {
				if pp.Answers {
					out = append(out, n+"@"+pp.Slug)
				}
			}
		}
		return strings.Join(sortedStrings(out), ",")
	}
	if got := answers(owner); got != sorted(me+"@harbor", me+"@side") {
		t.Fatalf("owner sees answers on %s", got)
	}
	if got := answers(list(cr.owner)); got != me+"@harbor" {
		t.Fatalf("member sees answers on %s", got)
	}
	if got := answers(guest); got != "" {
		t.Fatalf("guest sees answers on %s", got)
	}
}

func sortedStrings(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// A member's socket merges authorities by stable person id,
// never by name. Two identities are one person only when both are linked to
// the same verified account; the same name at two authorities is two rows.
// Each project keeps its own human:<name>; aliases come along.
func TestPeopleMergeByPersonID(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	b := startDaemon(t, tn, "mini2", tailnettest.Identity{NodeID: "nMINI2", NodeKey: "nodekey:mini2", Login: "ivo@example.com", Host: "mini2"})
	mustf(b.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "side"}))(t)
	mustf(b.owner.ProjectTeam(ctx, "side"))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	inv := mustf(b.owner.Invite(ctx, api.InviteRequest{Project: "side", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "dana", Role: "member", Project: "harbor"}))(t)
	mustf(b.owner.Grant(ctx, api.GrantRequest{Identity: "danadoe", Role: "member", Project: "side"}))(t)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "sam", Role: "member", Project: "harbor"}))(t)
	mustf(b.owner.Grant(ctx, api.GrantRequest{Identity: "sam", Role: "member", Project: "side"}))(t)
	mustf(orchclient.AddAlias(ctx, b.owner, orchapi.AliasRequest{Identity: "danadoe", Alias: "dc"}))(t)

	people := func(c *client.Client) []api.Person {
		t.Helper()
		var l api.PeopleList
		if err := c.Do(ctx, http.MethodGet, "/v0/people", nil, &l); err != nil || len(l.Skipped) != 0 {
			t.Fatalf("people %+v %v", l, err)
		}
		return l.People
	}
	named := func(ps []api.Person, name string) []api.Person {
		var out []api.Person
		for _, p := range ps {
			if p.Name == name {
				out = append(out, p)
			}
		}
		return out
	}
	// Dana is linked to the same GitHub account at both authorities (the
	// owners brick's verified link; written here as the device flow would).
	link := func(d *daemon, name string) string {
		t.Helper()
		ps := named(people(d.owner), name)
		if len(ps) != 1 || len(ps[0].Projects) != 1 {
			t.Fatalf("%s at its authority: %+v", name, ps)
		}
		// Unlinked, the person id is the identity's ULID there.
		if id := ps[0].Projects[0].Identity; ps[0].ID != id || ps[0].Projects[0].Name != name || ps[0].GitHub != "" {
			t.Fatalf("unlinked %s %+v", name, ps[0])
		}
		if _, err := d.dm.Deps.Store.Registry().ExecContext(ctx, `INSERT INTO owner_links (identity, account, login, linked, seen) VALUES (?, 'github:4242', 'dana-gh', 1, 1)`,
			ps[0].Projects[0].Identity); err != nil {
			t.Fatal(err)
		}
		return ps[0].Projects[0].Identity
	}
	da, db := link(a, "dana"), link(b, "danadoe")
	if ps := named(people(b.owner), "danadoe"); len(ps) != 1 || ps[0].ID != "github:4242" || ps[0].GitHub != "dana-gh" || strings.Join(ps[0].Aliases, ",") != "dc" {
		t.Fatalf("linked at its authority: %+v", ps)
	}

	all := people(r.owner)
	if ps := named(all, "danadoe"); len(ps) != 0 {
		t.Fatalf("dana split by name: %+v", all)
	}
	ps := named(all, "dana")
	if len(ps) != 1 || ps[0].ID != "github:4242" || ps[0].GitHub != "dana-gh" || strings.Join(ps[0].Aliases, ",") != "dc" || len(ps[0].Projects) != 2 {
		t.Fatalf("dana, one person across authorities: %+v", ps)
	}
	for _, pp := range ps[0].Projects {
		if (pp.Slug == "harbor") != (pp.Identity == da && pp.Name == "dana") || (pp.Slug == "side") != (pp.Identity == db && pp.Name == "danadoe") {
			t.Fatalf("each project keeps its own identity and name: %+v", ps[0].Projects)
		}
	}
	// Two unlinked sams share a name, not an id: never merged.
	if ps := named(all, "sam"); len(ps) != 2 || ps[0].ID == ps[1].ID || len(ps[0].Projects) != 1 || len(ps[1].Projects) != 1 {
		t.Fatalf("sam merged by name: %+v", ps)
	}
}
