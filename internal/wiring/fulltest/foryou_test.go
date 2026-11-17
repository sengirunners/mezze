package fulltest

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// For you (SPEC 9 "For you"): mail addressed to a person is one item in that
// person's Decisions, private to them, and only their own Got it marks it read.

// forYouFixture: the leak fixture's owner (human:ivo on the socket), an
// agent session in harbor/solver, and bea, an owner of harbor on a token.
func forYouFixture(t *testing.T) (f *leakFixture, owner, agent, bea *client.Client) {
	t.Helper()
	ctx := context.Background()
	f = newLeakFixture(t)
	owner = f.ownerAt("harbor/solver", "")
	addr := f.sessions["harbor/solver"]
	if addr == "" {
		t.Fatal("fixture has no session in harbor/solver")
	}
	agent = f.ownerAt("harbor/solver", addr)
	tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: "bea", Project: "harbor", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	bea = client.WithHandler(f.dm.Handler)
	bea.Token, bea.Project, bea.ProjectFrom = tok.Secret, "harbor", "flag"
	return f, owner, agent, bea
}

func gotIt(ctx context.Context, c *client.Client, id string) (orchapi.AckResponse, error) {
	var out orchapi.AckResponse
	err := c.Do(ctx, http.MethodPost, "/v0/mail/"+id+"/got-it", map[string]any{}, &out)
	return out, err
}

// An agent's plain FYI to a person, with no audience named, is that person's
// alone: another owner of the project never reads, lists, threads or takes it.
func TestForYouPrivateToItsPerson(t *testing.T) {
	ctx := context.Background()
	_, owner, agent, bea := forYouFixture(t)
	m, err := orchclient.Send(ctx, agent, orchapi.SendMailRequest{Kind: orchapi.MailKindReport, To: "human:ivo", Body: "FYI the nightly import finished", Needs: orchapi.NeedsAny})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{m.ID, m.ID[18:]} {
		if _, err := orchclient.GetMail(ctx, bea, id); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("bea read ivo's FYI (%s): %v", id, err)
		}
		if _, err := orchclient.Thread(ctx, bea, id); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("bea read ivo's FYI thread (%s): %v", id, err)
		}
		if _, err := gotIt(ctx, bea, id); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("bea's Got it on ivo's FYI (%s): %v", id, err)
		}
		if _, err := orchclient.Ack(ctx, bea, id); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("bea acked ivo's FYI (%s): %v", id, err)
		}
	}
	lists := map[string]func() ([]orchapi.Mail, error){
		"mail":  func() ([]orchapi.Mail, error) { return orchclient.Mail(ctx, bea, orchapi.MailQuery{Limit: 500}) },
		"inbox": func() ([]orchapi.Mail, error) { return orchclient.Inbox(ctx, bea, orchapi.InboxQuery{All: true}) },
	}
	for name, list := range lists {
		ms, err := list()
		if err != nil {
			t.Fatal(name, err)
		}
		for _, v := range ms {
			if v.ID == m.ID || strings.Contains(v.Body, "nightly import") {
				t.Fatalf("bea's %s lists ivo's FYI", name)
			}
		}
	}
	// An agent's FYI is a report (SPEC 9 "Reports"): a person reads it in their
	// reports, never their inbox.
	if m.Kind != orchapi.MailKindReport {
		t.Fatalf("an agent's FYI to a person: %+v", m)
	}
	ms, err := orchclient.Inbox(ctx, owner, orchapi.InboxQuery{Reports: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, v := range ms {
		found = found || v.ID == m.ID
	}
	if !found {
		t.Fatal("ivo's reports do not list his FYI")
	}
	for _, v := range must2(orchclient.Inbox(ctx, owner, orchapi.InboxQuery{}))(t) {
		if v.ID == m.ID {
			t.Fatal("an agent's FYI is in ivo's inbox")
		}
	}
}

// Read state is the person's own: no agent, and no CLI ack or reply, marks
// an FYI read; Got it, called by its person, does.
func TestForYouReadOnlyByItsPerson(t *testing.T) {
	ctx := context.Background()
	// A person's FYI (an agent's is a report, SPEC 9 "Reports").
	_, owner, _, agent := forYouFixture(t)
	for _, needs := range []orchapi.Needs{orchapi.NeedsAny, orchapi.NeedsHuman} {
		m, err := orchclient.Send(ctx, agent, orchapi.SendMailRequest{To: "human:ivo", Body: "FYI the deploy went out (" + string(needs) + ")", Needs: needs})
		if err != nil {
			t.Fatal(err)
		}
		unread := func(step string) {
			t.Helper()
			got, err := orchclient.GetMail(ctx, owner, m.ID)
			if err != nil {
				t.Fatal(step, err)
			}
			if got.AckedAt != nil {
				t.Fatalf("%s marked ivo's FYI read (acked by %s)", step, got.AckedBy)
			}
		}
		if _, err := orchclient.Ack(ctx, agent, m.ID); err == nil {
			t.Fatal("the sending agent acked the FYI")
		}
		unread("the agent's ack")
		if _, err := gotIt(ctx, agent, m.ID); err == nil {
			t.Fatal("the sending agent's Got it was taken")
		}
		unread("the agent's Got it")
		if _, err := orchclient.Ack(ctx, owner, m.ID); !api.IsCode(err, api.CodeForbidden) {
			t.Fatalf("zm ack on the socket: %v, want forbidden", err)
		}
		unread("zm ack")
		if _, err := orchclient.Reply(ctx, owner, m.ID, orchapi.ReplyRequest{Body: "thanks"}); err != nil {
			t.Fatal("ivo's reply was refused:", err)
		}
		unread("a reply")
		a, err := gotIt(ctx, owner, m.ID)
		if err != nil {
			t.Fatal("ivo's Got it:", err)
		}
		if !a.Acked || a.AckedBy != "human:ivo" {
			t.Fatalf("Got it answered %+v", a)
		}
		got, err := orchclient.GetMail(ctx, owner, m.ID)
		if err != nil || got.AckedAt == nil {
			t.Fatalf("after Got it the FYI is still unread: %+v %v", got, err)
		}
	}
}
