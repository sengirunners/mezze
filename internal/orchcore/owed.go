package orchcore

import (
	"context"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// OwedWho is whose work an owed list covers: the sessions (live or ended)
// and, when the list is a person's, that person's name.
type OwedWho struct {
	Sessions []orchapi.Session
	Person   string
}

// Addresses are the sessions' addresses.
func (w OwedWho) Addresses() []string {
	out := make([]string, 0, len(w.Sessions))
	for _, s := range w.Sessions {
		out = append(out, s.Address)
	}
	return out
}

// OwedSource is one module's part of what a session still owes: the items
// its own records show, scoped by that module's own rules for sub (sc is
// the reader's read scope). It never guesses: what it cannot read is an
// error, which the list shows as unknown.
type OwedSource struct {
	Name string // shown when the source fails: "work", "asks", "sessions"
	Owed func(ctx context.Context, sub *authz.Subject, sc authz.Scope, who OwedWho) (OwedPart, error)
}

// OwedPart is what a source read: its rows, and a gap for anything it read
// only in part (a cap hit, a mirror that is not current), shown as unknown.
type OwedPart struct {
	Items []orchapi.OwedItem
	Gaps  []orchapi.OwedGap
}

// Partial is a gap naming source's records that were read only in part.
func Partial(source, why string) orchapi.OwedGap {
	return orchapi.OwedGap{Kind: orchapi.OwedGapPartial, Name: source, Why: why}
}

// AddOwedSource registers a module's owed source on d (at wiring time).
func AddOwedSource(d *core.Deps, src OwedSource) {
	s := Wire(d)
	s.owedMu.Lock()
	s.owedSources = append(s.owedSources, src)
	s.owedMu.Unlock()
}

// OwedSources are the sources registered on d, in registration order.
func OwedSources(d *core.Deps) []OwedSource {
	s := of(d)
	if s == nil {
		return nil
	}
	s.owedMu.Lock()
	defer s.owedMu.Unlock()
	return append([]OwedSource(nil), s.owedSources...)
}

// PersonSessions is implemented by the session module: the sessions sc may
// read that person started (the daemon's own answer of whose a session is),
// live or last updated since the cutoff, newest first.
type PersonSessions interface {
	// truncated: more sessions matched the window than one read takes.
	SessionsOf(ctx context.Context, sc authz.Scope, person string, since time.Time) (ss []orchapi.Session, truncated bool, err error)
}
