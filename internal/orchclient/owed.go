package orchclient

import (
	"context"
	"net/http"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
)

// OwedQuery is GET /v0/owed. With neither Session nor Person, a session
// asks about itself and a person about their own sessions.
type OwedQuery struct {
	Project string
	Session string // a session's address or name
	Person  string // "me" or the caller's own name
	Since   string // a duration like 168h: how far back a person's ended sessions count
}

// Owed is GET /v0/owed: what a session, or all of a person's sessions, still
// owe, from zm's records, with what zm cannot see shown as unknown.
func Owed(ctx context.Context, c *client.Client, q OwedQuery) (orchapi.OwedList, error) {
	var l orchapi.OwedList
	return l, c.Do(ctx, http.MethodGet, client.Query("/v0/owed", "project", q.Project, "session", q.Session, "person", q.Person, "since", q.Since), nil, &l)
}
