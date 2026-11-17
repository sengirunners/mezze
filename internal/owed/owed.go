package owed

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// Bounds of one read. defaultSince is how far back a person's ended sessions
// count (their last work and open PRs may outlive them); ?since= changes it.
const (
	defaultSince  = 14 * 24 * time.Hour
	maxSince      = 90 * 24 * time.Hour
	sourceTimeout = 10 * time.Second
)

// Service is the owed brick.
type Service struct {
	d   *core.Deps
	now func() time.Time
}

var _ core.Module = (*Service)(nil)

// New returns the module.
func New(d *core.Deps) *Service { return &Service{d: d, now: time.Now} }

// Routes registers GET /v0/owed.
func (s *Service) Routes(r *core.Router) {
	r = r.For("owed")
	r.HandleFunc("GET /v0/owed", core.Authed, "read: each part by its own module's scope on ReadScope(?project=); ?session= a session the caller may read, else the caller's own session; ?person= only the caller itself; hidden and missing answer alike", s.handle)
}

func (s *Service) handle(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := core.SubjectOf(ctx)
	q := r.URL.Query()
	sc, err := s.d.ReadScope(ctx, q.Get("project"))
	if err != nil {
		api.WriteError(w, err)
		return
	}
	since := defaultSince
	if v := q.Get("since"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute || d > maxSince {
			api.WriteError(w, api.Errorf(api.CodeBadRequest, "since must be a duration between 1m and %s, like 168h", maxSince))
			return
		}
		since = d
	}
	out, err := s.list(ctx, sub, sc, q.Get("session"), q.Get("person"), since)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, out)
}

// list is what the caller asked about, from every registered source.
func (s *Service) list(ctx context.Context, sub *authz.Subject, sc authz.Scope, session, person string, since time.Duration) (orchapi.OwedList, error) {
	out := orchapi.OwedList{Sessions: []string{}, Items: []orchapi.OwedItem{}, Unknown: []orchapi.OwedGap{}}
	who := orchcore.OwedWho{}
	switch {
	case session != "" && person != "":
		return out, api.Errorf(api.CodeBadRequest, "name a session or a person, not both")
	case session != "" || person == "" && sub.Session != "":
		ss, err := s.session(ctx, sc, sub, session)
		if err != nil {
			return out, err
		}
		who.Sessions = []orchapi.Session{ss}
		out.For, out.Subject = orchapi.OwedForSession, ss.Address
	default:
		name, err := s.person(sub, person)
		if err != nil {
			return out, err
		}
		ps, ok := orchcore.Sessions(s.d).(orchcore.PersonSessions)
		if !ok {
			return out, api.Errorf(api.CodeUnavailable, "the session module is not running")
		}
		var truncated bool
		if who.Sessions, truncated, err = ps.SessionsOf(ctx, sc, name, s.now().Add(-since)); err != nil {
			return out, err
		}
		if truncated {
			out.Unknown = append(out.Unknown, orchcore.Partial("sessions", "more sessions than one read takes: the newest are listed, so older ones' work may be missing"))
		}
		who.Person = name
		out.For, out.Subject = orchapi.OwedForPerson, name
	}
	for _, ss := range who.Sessions {
		out.Sessions = append(out.Sessions, ss.Address)
	}
	srcs := orchcore.OwedSources(s.d)
	if len(srcs) == 0 {
		out.Unknown = append(out.Unknown, orchapi.OwedGap{Kind: orchapi.OwedGapSource, Name: "all", Why: "no module reports what is owed"})
	}
	seen := map[string]bool{}
	for _, src := range srcs {
		sctx, cancel := context.WithTimeout(ctx, sourceTimeout)
		part, err := src.Owed(sctx, sub, sc, who)
		cancel()
		if err != nil {
			why := "did not answer"
			if errors.Is(err, context.DeadlineExceeded) {
				why = "did not answer in time"
			}
			out.Unknown = append(out.Unknown, orchapi.OwedGap{Kind: orchapi.OwedGapSource, Name: src.Name, Why: why})
			continue
		}
		out.Unknown = append(out.Unknown, part.Gaps...)
		for _, it := range part.Items {
			if k := it.Kind + "\x00" + it.Ref; !seen[k] {
				seen[k] = true
				out.Items = append(out.Items, it)
			}
		}
	}
	sortItems(out.Items)
	if s.joined(sc) {
		out.Unknown = append(out.Unknown, orchapi.OwedGap{Kind: orchapi.OwedGapUnseen, Name: "joined_projects",
			Why: "projects joined from another machine keep their leases and cards at their authority: this list reads what this machine holds"})
	}
	out.Unknown = append(out.Unknown, unseenGaps()...)
	return out, nil
}

// session resolves ref (a name or an address; empty: the caller's own
// session) to a session the reader may see. A hidden one answers like a
// missing one.
func (s *Service) session(ctx context.Context, sc authz.Scope, sub *authz.Subject, ref string) (orchapi.Session, error) {
	if ref == "" {
		ref = sub.Session
	}
	ss := orchcore.Sessions(s.d)
	if ss == nil {
		return orchapi.Session{}, api.Errorf(api.CodeUnavailable, "the session module is not running")
	}
	address := ref
	if n, ok := ss.(orchcore.SessionNames); ok {
		if byName, found := n.ResolveName(ctx, ref); found {
			address = byName.Address
		}
	}
	found, ok := ss.Lookup(ctx, address)
	if !ok || !ss.Visible(ctx, sc, found.Address) {
		return orchapi.Session{}, api.NotFound("session", ref)
	}
	return found, nil
}

// person is the person whose sessions are listed: the caller itself, named
// or as "me". Another person's sessions answer like a missing person.
func (s *Service) person(sub *authz.Subject, named string) (string, error) {
	self := ""
	switch {
	case sub.Identity.Kind == api.IdentityHuman && sub.Identity.Name != "":
		self = sub.Identity.Name
	case sub.Via == authz.ViaLocal && sub.Session == "": // the machine's owner at its socket
		self = s.d.Config.Human
	}
	if self == "" {
		return "", api.Errorf(api.CodeBadRequest, "name a session (?session=) or sign in as a person")
	}
	if named == "" || named == "me" || named == self {
		return self, nil
	}
	return "", api.NotFound("person", named)
}

// joined reports whether sc reads a project this machine holds only as a
// replica.
func (s *Service) joined(sc authz.Scope) bool {
	tr, ok := s.d.Team.(core.TeamReplicas)
	if !ok {
		return false
	}
	for _, p := range s.d.Store.Projects(sc, authz.Read) {
		if tr.Replicates(p.ID) {
			return true
		}
	}
	return false
}

// kindRank orders the list by how soon each kind needs someone.
var kindRank = map[string]int{orchapi.OwedPR: 0, orchapi.OwedAskNotDone: 1, orchapi.OwedAskOpen: 2, orchapi.OwedWork: 3, orchapi.OwedLane: 4}

func sortItems(items []orchapi.OwedItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if kindRank[a.Kind] != kindRank[b.Kind] {
			return kindRank[a.Kind] < kindRank[b.Kind]
		}
		switch {
		case a.Since == nil && b.Since == nil:
			return a.Ref < b.Ref
		case a.Since == nil:
			return false
		case b.Since == nil:
			return true
		case !a.Since.Equal(*b.Since):
			return a.Since.Before(*b.Since)
		}
		return a.Ref < b.Ref
	})
}
