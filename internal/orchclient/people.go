package orchclient

import (
	"context"
	"net/http"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// People is GET /v0/people narrowed to the people who answer asks (zm
// people, MCP people): a human identity that owns or administers one of
// the projects listed (project, else every one the caller reads). Farm
// boxes, runners and bots, which answer nothing, are left out. Each row
// keeps every name, alias and the GitHub login an ask may name them by.
func People(ctx context.Context, c *client.Client, project string) (api.PeopleList, error) {
	var l api.PeopleList
	if err := c.Do(ctx, http.MethodGet, client.Query("/v0/people", "project", project), nil, &l); err != nil {
		return l, err
	}
	out := []api.Person{}
	for _, p := range l.People {
		if p.Kind != api.IdentityHuman {
			continue
		}
		var answers []api.PersonProject
		for _, pp := range p.Projects {
			if pp.Answers {
				answers = append(answers, pp)
			}
		}
		if len(answers) > 0 {
			p.Projects = answers
			out = append(out, p)
		}
	}
	l.People = out
	return l, nil
}
