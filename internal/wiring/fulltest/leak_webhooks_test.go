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

// Webhooks (SPEC "Webhooks and GitHub linking") seeded where the guest of
// harbor/solver must not see them: one made by an admin of the hidden
// project over the network, one by an owner of another area of harbor, and
// the machine owner's own. Every webhook route answers the guest exactly as
// for a missing id, and no list, error or body names them.
func TestWebhooksLeak(t *testing.T) {
	f := newLeakFixture(t)
	ctx := context.Background()
	owner := client.WithHandler(f.dm.SocketHandler())
	var hidden []string
	add := func(c *client.Client, project, url string) {
		t.Helper()
		r, err := orchclient.AddWebhook(ctx, c, orchapi.NewWebhookRequest{URL: url, Project: project, Note: "HIDDEN-hook-note"})
		if err != nil {
			t.Fatal(err)
		}
		hidden = append(hidden, r.Webhook.ID)
		f.forbid(r.Webhook.ID, "a hidden webhook's id")
		f.forbid(url, "a hidden webhook's url")
	}
	for _, x := range []struct{ as, project, area, url string }{
		{"zz-admin", "zz-side", "", "https://hidden-side.example.com/hook"},
		{"zz-billing-owner", "harbor", "zz-billing", "https://hidden-billing.example.com/hook"},
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
		add(c, p, x.url)
	}
	add(owner, "harbor", "https://hidden-owner.example.com/hook")
	f.forbid("HIDDEN-hook-note", "a hidden webhook's note")
	f.hidden = append(f.hidden, hidden...)

	only := func(p string) bool { return !strings.Contains(p, "/v0/webhooks") }
	for _, tr := range transports {
		f.walk(t, tr.String(), f.dm.Router.Routes(), func(method, target, project string) leakResp {
			return f.call(method, target, project, tr)
		}, only, nil)
	}
	if r := f.call("GET", "/v0/webhooks", "", viaTailnet); r.status != 200 || strings.TrimSpace(r.body) != `{"webhooks":[]}` {
		t.Fatalf("guest list: %d %s", r.status, r.body)
	}
	// The guest cannot add one either (not an admin), even in its own area.
	r := f.callBody("POST", "/v0/webhooks", "", viaTailnet, `{"url": "https://x.example.com/", "project": "harbor/solver"}`)
	if r.status != 403 {
		t.Fatalf("guest added a webhook: %d %s", r.status, r.body)
	}
	// Still there for the owner: the refusals did not delete anything.
	ws, err := orchclient.Webhooks(ctx, owner, "")
	if err != nil || len(ws) != 3 {
		t.Fatalf("owner sees %d hooks: %v", len(ws), err)
	}
}
