package fulltest

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// Connections (SPEC 24) seeded where the guest of harbor/solver must not see
// them: one by an admin of the hidden project over the network, one in
// another area of harbor, and one by the machine owner in the hidden
// project. Every connection route answers the guest exactly as for a
// missing id, and no list, error or body names them.
func TestConnectionsLeak(t *testing.T) {
	f := newLeakFixture(t)
	ctx := context.Background()
	owner := client.WithHandler(f.dm.SocketHandler())
	var hidden []string
	add := func(c *client.Client, project, name, endpoint string) {
		t.Helper()
		r, err := orchclient.AddConnection(ctx, c, orchapi.NewConnectionRequest{Project: project, Name: name, Endpoint: endpoint,
			Token: "HIDDEN-conn-token", Note: "HIDDEN-conn-note"})
		if err != nil {
			t.Fatal(err)
		}
		hidden = append(hidden, r.ID)
		f.forbid(r.ID, "a hidden connection's id")
		f.forbid(name, "a hidden connection's name")
		f.forbid(endpoint, "a hidden connection's endpoint")
	}
	for _, x := range []struct{ as, project, area, name, url string }{
		{"zz-admin", "zz-side", "", "HIDDEN-side-ci", "https://hidden-side-ci.example.com/zm"},
		{"zz-billing-owner", "harbor", "zz-billing", "HIDDEN-billing-ci", "https://hidden-billing-ci.example.com/zm"},
	} {
		tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: x.as, Project: x.project, Area: x.area, Role: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		c := client.WithHandler(f.dm.Handler)
		c.Token = tok.Secret
		p := x.project
		if x.area != "" {
			p += "/" + x.area
		}
		add(c, p, x.name, x.url)
	}
	add(owner, "zz-side", "HIDDEN-owner-ci", "https://hidden-owner-ci.example.com/zm")
	f.forbid("HIDDEN-conn-note", "a hidden connection's note")
	f.forbid("HIDDEN-conn-token", "a hidden connection's token")
	f.hidden = append(f.hidden, hidden...)

	only := func(p string) bool { return !strings.Contains(p, "/v0/connections") }
	for _, tr := range transports {
		f.walk(t, tr.String(), f.dm.Router.Routes(), func(method, target, project string) leakResp {
			return f.call(method, target, project, tr)
		}, only, nil)
	}
	if r := f.call("GET", "/v0/connections", "", viaTailnet); r.status != 200 || strings.TrimSpace(r.body) != `{"connections":[]}` {
		t.Fatalf("guest list: %d %s", r.status, r.body)
	}
	// A guest cannot register one, even in its own area.
	r := f.callBody("POST", "/v0/connections", "", viaTailnet, `{"name": "x", "endpoint": "https://x.example.com/", "project": "harbor/solver"}`)
	if r.status != 403 {
		t.Fatalf("guest added a connection: %d %s", r.status, r.body)
	}
	cs, err := orchclient.Connections(ctx, owner, "")
	if err != nil || len(cs) != 3 {
		t.Fatalf("owner sees %d connections: %v", len(cs), err)
	}
}
