package orchclient

import (
	"context"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/runners"
)

// RepoWarning is what zm says about a connected repository: the broker's own
// warning about its settings, and for a public repository with no yes from
// its owner (one the upgrade connected) that it is not served. A repository
// the upgrade connected whose visibility the broker has not read yet may be
// public, so it says what a public one still needs.
func RepoWarning(r runners.Repo) string {
	var not string
	switch {
	case r.Public:
	case r.Visibility == "public":
		not = "not served until zm runners allow " + r.Repo + " --public"
	case r.Visibility == "" && r.By == "upgrade":
		not = "visibility not read yet: if public, not served until zm runners allow " + r.Repo + " --public"
	}
	switch {
	case not == "":
		return r.Warning
	case r.Warning == "":
		return not
	}
	return not + "; " + r.Warning
}

// ciConnected adds to each repository row what the broker's connected list
// says: a repository that is not connected is never served, so its row says
// how to connect it, and a connected one carries its warning. A list that
// cannot be read (a daemon older than per-repository serving) is an issue,
// never an all-clear.
func ciConnected(ctx context.Context, c *client.Client, out *orchapi.CIStatus, rows map[string]*orchapi.CIRepoStatus) {
	var list struct {
		Repos []runners.Repo `json:"repos"`
	}
	if err := c.Do(ctx, "GET", "/v0/runners/repos", nil, &list); err != nil {
		out.Partial = true
		out.Issues = append(out.Issues, "Could not read which repos are connected to the pool: "+err.Error())
		for _, v := range rows {
			v.Partial = true
		}
		return
	}
	connected := map[string]runners.Repo{}
	for _, r := range list.Repos {
		connected[strings.ToLower(r.Repo)] = r
	}
	for key, v := range rows {
		r, ok := connected[key]
		if !ok {
			v.Issues = append(v.Issues, "Not connected in the pool areas you can read, so its jobs are not served: zm runners allow "+v.Repo+" (--public for a public one)")
			continue
		}
		if w := RepoWarning(r); w != "" {
			v.Issues = append(v.Issues, "Connected repo: "+w)
		}
	}
}
