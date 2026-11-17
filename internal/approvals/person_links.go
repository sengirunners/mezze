package approvals

// Person links: one person may hold several accounts
// (identities) at an authority without a GitHub link joining them. A link
// says account L belongs to the person whose main account is P. It is data
// at the authority, never a name in code.
//
// What a live link does: L folds into P's person everywhere people fold
// (GET /v0/people, an ask's owner_person and a decision's approver_person,
// naming the person by any of its names), so a card for the person answered
// from any of its accounts is that person's answer. What it never does:
// grant anything. Answering an ask, owning a project and every other right
// stay with each identity's own grants, and a signed answer stays required
// wherever it is required.
//
// Who makes a link live: the main account P always agrees (nobody is given
// an extra account without their say), and so does L itself or someone who
// administers every project either account is in and is not L (an account
// may not label itself as someone else). An admin's record is checked when
// it is made; it stays until removed. Mail is private, so it follows consent only: mail
// to an account reaches the person's other accounts only when that account
// itself turned sharing on (share_mail), never by anyone else's say. Sharing
// belongs to the link row, pairwise along it (L's mail to P, P's mail to L):
// moving or removing a link drops it, so no one inherits another's consent.

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/store"
)

var personLinksMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "approvals/0014_person_links", Script: `
CREATE TABLE person_links (
	identity  TEXT PRIMARY KEY,
	person    TEXT NOT NULL,
	by_admin  TEXT NOT NULL DEFAULT '',
	by_self   INTEGER NOT NULL DEFAULT 0,
	by_person INTEGER NOT NULL DEFAULT 0,
	self_shares   INTEGER NOT NULL DEFAULT 0,
	person_shares INTEGER NOT NULL DEFAULT 0,
	created   INTEGER NOT NULL
);
CREATE INDEX person_links_person ON person_links (person);`},
}

func (s *Service) personLinkRoutes(r *core.Router) {
	r.HandleFunc("GET /v0/person-links", core.Authed, "the links the caller is either account of, or administers every project of (as POST); nothing else", s.handlePersonLinks)
	r.HandleFunc("POST /v0/person-links", core.Authed, "either account itself (each records its own consent), or a caller that is not the linked account and administers every project either account holds a grant in (core.Deps.ProjectAdmin); share_mail only ever for the caller's own account (403 otherwise); a party who is no such admin names only an account it shares a project with and never moves a link it is no party to; every not-found case is one person link not_found", s.handleAddPersonLink)
	r.HandleFunc("DELETE /v0/person-links/{identity}", core.Authed, "either account, or an admin as POST; else not_found", s.handleRemovePersonLink)
}

// personLink is one person_links row.
type personLink struct {
	identity, person, byAdmin string
	bySelf, byPerson          bool
	selfShares, personShares  bool // each account's own mail switch
}

const linkCols = `identity, person, by_admin, by_self, by_person, self_shares, person_shares`

type scanner interface{ Scan(...any) error }

func scanLink(r scanner) (personLink, error) {
	var l personLink
	err := r.Scan(&l.identity, &l.person, &l.byAdmin, &l.bySelf, &l.byPerson, &l.selfShares, &l.personShares)
	return l, err
}

// live: the main account agreed, and so did the linked account or an admin
// of every project both are in. Nobody is given an extra account without
// their own say.
func (l personLink) live() bool { return l.byPerson && (l.bySelf || l.byAdmin != "") }

func (s *Service) linkOf(ctx context.Context, identity string) (personLink, bool) {
	l, err := scanLink(s.d.Store.Registry().QueryRowContext(ctx, `SELECT `+linkCols+` FROM person_links WHERE identity = ?`, identity))
	return l, err == nil
}

// folds reports whether l folds now: live, and not contradicted by a
// verified GitHub link made since (LinkedPerson splits such accounts too, so
// mail sharing stops with the fold).
func (s *Service) folds(ctx context.Context, l personLink) bool {
	if !l.live() {
		return false
	}
	if p := s.ownAccount.Load(); p != nil && *p != nil {
		own := *p
		if accL, _ := own(ctx, l.identity); accL != "" {
			if accP, _ := own(ctx, l.person); accP != accL {
				return false
			}
		}
	}
	return true
}

// linksTo is every link naming person as its main account.
func (s *Service) linksTo(ctx context.Context, person string) ([]personLink, error) {
	return s.queryLinks(ctx, s.d.Store.Registry(), `SELECT `+linkCols+` FROM person_links WHERE person = ? ORDER BY identity`, person)
}

type querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
}

func (s *Service) queryLinks(ctx context.Context, db querier, q string, args ...any) ([]personLink, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []personLink
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LinkedPerson wraps the owners brick's LinkedAccount (core.Deps.LinkedAccount,
// which only people folding reads): an identity's own verified account wins;
// else a live person link folds it into its main account's key (that
// account's verified account, else its identity id, which is what an
// unlinked identity keys by already).
func (s *Service) LinkedPerson(own func(ctx context.Context, identity string) (string, string)) func(ctx context.Context, identity string) (string, string) {
	s.ownAccount.Store(&own)
	return func(ctx context.Context, identity string) (string, string) {
		if own != nil {
			if acc, login := own(ctx, identity); acc != "" {
				return acc, login
			}
		}
		l, ok := s.linkOf(ctx, identity)
		if !ok || !l.live() {
			return "", ""
		}
		if own != nil {
			if acc, login := own(ctx, l.person); acc != "" {
				return acc, login
			}
		}
		return l.person, ""
	}
}

// personAccounts is the identities (by id) live-linked with identity into
// one person, identity first; just identity when it has no live link.
func (s *Service) personAccounts(ctx context.Context, identity string) ([]string, error) {
	main := identity
	if l, ok := s.linkOf(ctx, identity); ok && s.folds(ctx, l) {
		main = l.person
	}
	out := []string{identity}
	if main != identity {
		out = append(out, main)
	}
	ls, err := s.linksTo(ctx, main)
	if err != nil {
		return nil, err
	}
	for _, l := range ls {
		if s.folds(ctx, l) && !slices.Contains(out, l.identity) {
			out = append(out, l.identity)
		}
	}
	return out, nil
}

// humanAddrs is the human addresses of ids other than skip.
func (s *Service) humanAddrs(ctx context.Context, ids []string, skip string) []string {
	var out []string
	for _, id := range ids {
		if id == skip {
			continue
		}
		who, err := s.d.Projects.Identity(ctx, id)
		if err != nil || who.Kind != api.IdentityHuman {
			continue
		}
		if ad, err := api.ParseAddress("human:" + who.Name); err == nil && ad.Kind == api.KindHuman {
			out = append(out, ad.String())
		}
	}
	slices.Sort(out)
	return out
}

// identityOfAddr is the human identity human:<name> names.
func (s *Service) identityOfAddr(ctx context.Context, addr string) (api.Identity, bool) {
	ad, err := api.ParseAddress(addr)
	if err != nil || ad.Kind != api.KindHuman {
		return api.Identity{}, false
	}
	who, err := s.d.Projects.Identity(ctx, ad.Human)
	if err != nil || who.Kind != api.IdentityHuman || !strings.EqualFold(who.Name, ad.Human) {
		return api.Identity{}, false
	}
	return who, true
}

// AccountsOf is the other human addresses of the person human:<name> is
// (orchcore.PersonAccounts): which mail a person's inbox lists besides their
// own. It reveals nothing by itself: a private mail lists only when the
// reader is also one of its participants (ShareMailWith put them there).
func (s *Service) AccountsOf(ctx context.Context, addr string) []string {
	who, ok := s.identityOfAddr(ctx, addr)
	if !ok {
		return nil
	}
	ids, err := s.personAccounts(ctx, who.ID)
	if err != nil {
		return nil
	}
	return s.humanAddrs(ctx, ids, who.ID)
}

// ShareMailWith is the other accounts mail to human:<name> also goes to
// (orchcore.PersonAccounts): pairwise along live links, only where this
// account itself turned sharing on for that link. Any error shares nothing.
func (s *Service) ShareMailWith(ctx context.Context, addr string) []string {
	who, ok := s.identityOfAddr(ctx, addr)
	if !ok {
		return nil
	}
	var ids []string
	if l, ok := s.linkOf(ctx, who.ID); ok && s.folds(ctx, l) && l.selfShares {
		ids = append(ids, l.person)
	}
	ls, err := s.linksTo(ctx, who.ID)
	if err != nil {
		return nil
	}
	for _, l := range ls {
		if s.folds(ctx, l) && l.personShares {
			ids = append(ids, l.identity)
		}
	}
	// Never into the authority's own account: anyone at the box, and a phone
	// paired for no one, reads as it.
	if local := s.d.Projects.LocalIdentity(); local.ID != "" {
		ids = slices.DeleteFunc(ids, func(id string) bool { return id == local.ID })
	}
	return s.humanAddrs(ctx, ids, who.ID)
}

// shareProject reports whether a and b both hold a grant in one project:
// a party who is no admin names only an account it could already see.
func (s *Service) shareProject(ctx context.Context, a, b api.Identity) bool {
	seen := map[string]bool{}
	for i, who := range []api.Identity{a, b} {
		tuples, err := s.d.Projects.Tuples(ctx, who.ID)
		if err != nil {
			return false
		}
		for _, t := range tuples {
			p, _, err := authz.ParseObject(t.Object)
			if err != nil || p == api.ProjectNone {
				continue
			}
			if i == 0 {
				seen[p] = true
			} else if seen[p] {
				return true
			}
		}
	}
	return false
}

// mayAdminLink says whether sub administers every project either account
// holds a grant in, and is not the linked account itself.
func (s *Service) mayAdminLink(ctx context.Context, sub *authz.Subject, linked, main api.Identity) bool {
	if sub == nil || sub.Identity.ID == linked.ID {
		return false
	}
	if sub.Via == authz.ViaLocal && sub.TokenID == "" {
		return true // the authority's owner, as for aliases
	}
	var projects []string
	for _, who := range []api.Identity{linked, main} {
		tuples, err := s.d.Projects.Tuples(ctx, who.ID)
		if err != nil {
			return false
		}
		for _, t := range tuples {
			p, _, err := authz.ParseObject(t.Object)
			if err != nil {
				return false
			}
			if !slices.Contains(projects, p) {
				projects = append(projects, p)
			}
		}
	}
	for _, p := range projects {
		if ok, err := s.d.ProjectAdmin(ctx, sub, p); err != nil || !ok {
			return false
		}
	}
	return len(projects) > 0
}

func (s *Service) humanIdentity(ctx context.Context, ref string) (api.Identity, error) {
	who, err := s.d.Projects.Identity(ctx, strings.TrimPrefix(strings.TrimSpace(ref), "human:"))
	if err != nil || who.Kind != api.IdentityHuman {
		return api.Identity{}, api.NotFound("identity", ref)
	}
	return who, nil
}

func (s *Service) handleAddPersonLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req orchapi.PersonLinkRequest
	if err := api.DecodeJSON(r, &req); err != nil {
		api.WriteError(w, err)
		return
	}
	sub := core.SubjectOf(ctx)
	if sub == nil || sub.Identity.ID == "" {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "the caller has no identity"))
		return
	}
	// One answer for a missing account, a hidden one and a link the caller
	// may not make, so no name's existence shows.
	nf := api.NotFound("person link", req.Identity)
	linked, err1 := s.humanIdentity(ctx, req.Identity)
	main, err2 := s.humanIdentity(ctx, req.Person)
	if err1 != nil || err2 != nil {
		api.WriteError(w, nf)
		return
	}
	self, mine := sub.Identity.ID == linked.ID, sub.Identity.ID == main.ID
	admin := s.mayAdminLink(ctx, sub, linked, main)
	if !self && !mine && !admin || !admin && !s.shareProject(ctx, linked, main) {
		api.WriteError(w, nf) // nothing recorded
		return
	}
	if req.ShareMail != nil && !self && !mine {
		api.WriteError(w, api.Errorf(api.CodeForbidden, "only an account itself shares its own mail").
			WithHint("run it as "+linked.Name+" or "+main.Name+": zm identity person-link "+linked.Name+" --person "+main.Name+" --share-mail"))
		return
	}
	if err := s.addPersonLink(ctx, sub, linked, main, self, mine, admin, req.ShareMail); err != nil {
		api.WriteError(w, err)
		return
	}
	v, err := s.personLinksView(ctx, sub)
	respond(w, v, err)
}

// addPersonLink records the caller's say on linked -> main: its own consent
// (and its own mail switch, share) as either account, or an admin's record.
// The checks run inside the write, so two links racing cannot chain.
func (s *Service) addPersonLink(ctx context.Context, sub *authz.Subject, linked, main api.Identity, self, mine, admin bool, share *bool) error {
	if linked.ID == main.ID {
		return api.Errorf(api.CodeBadRequest, "an account is already its own person")
	}
	if p := s.ownAccount.Load(); p != nil && *p != nil {
		own := *p // the verified links alone, not folded by person links
		if accL, _ := own(ctx, linked.ID); accL != "" {
			if accP, _ := own(ctx, main.ID); accP != accL {
				return api.Errorf(api.CodeConflict, "%s is linked to a different GitHub account than %s", linked.Name, main.Name).
					WithHint("a verified GitHub link decides who an account is; unlink it first (zm identity unlink)")
			}
		}
	}
	tx, err := s.d.Store.Registry().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The registry's transactions are immediate (potluck kit/store): this
	// one holds the write lock, so the checks below see every committed link.
	refuse := func(who string) error {
		return api.Errorf(api.CodeConflict, "this link cannot be made as asked").
			WithHint("ask " + who + " or an admin of every project both accounts are in")
	}
	cur, err := scanLink(tx.QueryRowContext(ctx, `SELECT `+linkCols+` FROM person_links WHERE identity = ?`, linked.ID))
	switch {
	case err == nil && cur.person != main.ID && !admin && !self && (cur.live() || cur.bySelf):
		// Only the account itself or an admin moves a live link or the
		// account's own claim: a third party never cuts someone else's.
		return refuse(linked.Name)
	case errors.Is(err, sql.ErrNoRows), err == nil && cur.person != main.ID:
		cur = personLink{} // new, or moved to another person: no earlier say or mail switch carries over
	case err != nil:
		return err
	}
	if admin { // never the linked account itself (mayAdminLink)
		cur.byAdmin = sub.Address
	}
	cur.bySelf = cur.bySelf || self
	cur.byPerson = cur.byPerson || mine
	if share != nil && self {
		cur.selfShares = *share
	}
	if share != nil && mine {
		cur.personShares = *share
	}
	if cur.live() {
		// One level, among live links only (a pending claim blocks nothing):
		// the main account is no live link's account, and the linked account
		// no live link's main account.
		if top, err := scanLink(tx.QueryRowContext(ctx, `SELECT `+linkCols+` FROM person_links WHERE identity = ?`, main.ID)); err == nil && top.live() {
			if !admin && !mine {
				return refuse(main.Name)
			}
			return api.Errorf(api.CodeConflict, "%s is itself linked to another account: link to that account instead", main.Name)
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		ls, err := s.queryLinks(ctx, tx, `SELECT `+linkCols+` FROM person_links WHERE person = ? AND identity != ?`, linked.ID, main.ID)
		if err != nil {
			return err
		}
		for _, l := range ls {
			if l.live() {
				if !admin && !self {
					return refuse(linked.Name)
				}
				return api.Errorf(api.CodeConflict, "other accounts are linked to %s: it is a person's main account", linked.Name).
					WithHint("link " + main.Name + " to " + linked.Name + " instead, or remove those links first")
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO person_links (identity, person, by_admin, by_self, by_person, self_shares, person_shares, created) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT (identity) DO UPDATE SET person = excluded.person, by_admin = excluded.by_admin, by_self = excluded.by_self, by_person = excluded.by_person,
 self_shares = excluded.self_shares, person_shares = excluded.person_shares`,
		linked.ID, main.ID, cur.byAdmin, cur.bySelf, cur.byPerson, cur.selfShares, cur.personShares, ms(s.now())); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) handleRemovePersonLink(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	ref := r.PathValue("identity")
	linked, err := s.humanIdentity(ctx, ref)
	if err != nil {
		api.WriteError(w, api.NotFound("person link", ref)) // as a missing link
		return
	}
	l, ok := s.linkOf(ctx, linked.ID)
	if !ok || sub == nil {
		api.WriteError(w, api.NotFound("person link", ref))
		return
	}
	main, err := s.d.Projects.Identity(ctx, l.person)
	if err != nil {
		main = api.Identity{ID: l.person}
	}
	if sub.Identity.ID != linked.ID && sub.Identity.ID != main.ID && !s.mayAdminLink(ctx, sub, linked, main) {
		api.WriteError(w, api.NotFound("person link", ref))
		return
	}
	if _, err := s.d.Store.Registry().ExecContext(ctx, `DELETE FROM person_links WHERE identity = ?`, linked.ID); err != nil {
		api.WriteError(w, err)
		return
	}
	v, err := s.personLinksView(ctx, sub)
	respond(w, v, err)
}

func (s *Service) handlePersonLinks(w http.ResponseWriter, r *http.Request) {
	v, err := s.personLinksView(r.Context(), core.SubjectOf(r.Context()))
	respond(w, v, err)
}

// personLinksView is the links sub is a party to or administers.
func (s *Service) personLinksView(ctx context.Context, sub *authz.Subject) (orchapi.PersonLinks, error) {
	out := orchapi.PersonLinks{Links: []orchapi.PersonLink{}}
	if sub == nil {
		return out, nil
	}
	ls, err := s.queryLinks(ctx, s.d.Store.Registry(), `SELECT `+linkCols+` FROM person_links ORDER BY identity`)
	if err != nil {
		return out, err
	}
	for _, l := range ls {
		linked, err1 := s.d.Projects.Identity(ctx, l.identity)
		main, err2 := s.d.Projects.Identity(ctx, l.person)
		if err1 != nil || err2 != nil {
			continue
		}
		if sub.Identity.ID != linked.ID && sub.Identity.ID != main.ID && !s.mayAdminLink(ctx, sub, linked, main) {
			continue
		}
		out.Links = append(out.Links, orchapi.PersonLink{Identity: linked.Name, Person: main.Name, Live: l.live(),
			ByAdmin: s.d.Redact(ctx, sub, l.byAdmin), BySelf: l.bySelf, ByPerson: l.byPerson,
			SharesMail: l.selfShares, PersonSharesMail: l.personShares})
	}
	return out, nil
}

// approverPerson is the person who answered a's decision, or "" when that
// is not known: a paired device bound to no identity is no one's
// unless this machine's owner is the one person who answers (soleAnswerer);
// an owner answer on this authority's own socket (anyone at the box answers
// as its local account) is the local account's only while that account is
// linked into no one's person. Such an answer never becomes a linked person's.
func (s *Service) approverPerson(ctx context.Context, a orchapi.Ask) string {
	d := a.Decision
	if d == nil || d.Approver == "" {
		return ""
	}
	byDefault := false
	local := s.d.Projects.LocalIdentity()
	switch {
	case d.Method == orchapi.MethodApprover:
		var id string
		// The device that answered (its node, from the signed receipt), as
		// paired then: a name paired again later is another pairing.
		var paired sql.NullInt64
		err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT identity, paired FROM approvers WHERE name = ? AND node_id = ?`, d.Approver, d.Receipt.Node).Scan(&id, &paired)
		if err != nil || !paired.Valid || paired.Int64 > d.Time.UnixMilli() {
			return ""
		}
		if id == "" && solo(s.soleAnswerer(ctx, a.Project)) == "" {
			return "" // a device paired for no one answers as no one (approverName)
		}
		byDefault = id == "" || id == local.ID
	case local.ID != "" && strings.EqualFold(d.Approver, local.Name) && (d.Machine == s.d.Config.Machine || d.Surface == orchapi.SurfaceAnswerToken):
		// The socket, or an answer-only token, which only the socket's own
		// account mints: both are this box's account, not a linked person.
		byDefault = true
	}
	if byDefault && local.ID != "" {
		// Linked either way round: as the linked account or as a main one.
		if ids, err := s.personAccounts(ctx, local.ID); err != nil || len(ids) > 1 {
			return ""
		}
	}
	return s.personOf(ctx, a.Project, s.approverAddress(ctx, a))
}
