package team_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// SPEC 8 "Names" on a team replica: a raw POST /v0/mail naming one of the
// replica's sessions by name, and a Zm-As naming one, reach the authority
// as addresses (the authority does not know the replica's names).
func TestReplicaMailBySessionName(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-sender")
	tgt := register(t, r, "harbor/solver", "bea-target")
	mustf(orchclient.SetSessionName(ctx, r.owner, tgt.Address, "auth-fix"))(t)
	heartbeat(t, r)

	var m orchapi.Mail
	if err := at(r.owner, "harbor", cs.Address).Do(ctx, http.MethodPost, "/v0/mail",
		map[string]any{"to": "bea-mac/auth-fix", "body": "by name"}, &m); err != nil {
		t.Fatal(err)
	}
	if m.Recipient != tgt.Address || m.To != tgt.Address {
		t.Fatalf("raw mail by name: %+v", m)
	}
	back := mustf(orchclient.Send(ctx, at(r.owner, "harbor", "bea-mac/auth-fix"), orchapi.SendMailRequest{To: cs.Address, Body: "as my name"}))(t)
	if back.From != tgt.Address {
		t.Fatalf("Zm-As by name: %+v", back)
	}
	// A name nobody has goes on as typed and is missing at the authority.
	err := at(r.owner, "harbor", cs.Address).Do(ctx, http.MethodPost, "/v0/mail", map[string]any{"to": "bea-mac/nobody", "body": "x"}, &m)
	if !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing name: %v", err)
	}
	if _, err := orchclient.Send(ctx, at(r.owner, "harbor", "bea-mac/nobody"), orchapi.SendMailRequest{To: cs.Address, Body: "x"}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("Zm-As of no session: %v", err)
	}
}

// A name passes to another session after its holder ends: mail and
// replies sent before still reach the old session's address, never the
// new holder. A session whose id came from a non-UUID runtime id (8
// letters) keeps it as its address: no session can take it as a name.
func TestSessionNameReusedNeverRedirects(t *testing.T) {
	ctx := context.Background()
	a := authority(t, tailnettest.New())
	sender := register(t, a, "harbor", "sender-sess")
	old := register(t, a, "harbor", "old-sess")
	mustf(orchclient.SetSessionName(ctx, a.owner, old.Address, "front-end"))(t)

	m1 := mustf(orchclient.Send(ctx, at(a.owner, "harbor", sender.Address), orchapi.SendMailRequest{To: "mini/front-end", Body: "to the old one"}))(t)
	m2 := mustf(orchclient.Send(ctx, at(a.owner, "harbor", "mini/front-end"), orchapi.SendMailRequest{To: sender.Address, Body: "from the old one"}))(t)
	if m1.Recipient != old.Address || m2.From != old.Address {
		t.Fatalf("by name: %+v %+v", m1, m2)
	}
	mustf(orchclient.Register(ctx, a.owner, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "old-sess", End: true}))(t)
	nu := register(t, a, "harbor", "new-sess")
	mustf(orchclient.SetSessionName(ctx, a.owner, nu.Address, "front-end"))(t)

	rep := mustf(orchclient.Reply(ctx, at(a.owner, "harbor", sender.Address), m2.ID, orchapi.ReplyRequest{Body: "answer"}))(t)
	if rep.Recipient != old.Address {
		t.Fatalf("the reply went to %s, not the old session %s", rep.Recipient, old.Address)
	}
	if in := mustf(orchclient.Inbox(ctx, at(a.owner, "", nu.Address), orchapi.InboxQuery{All: true}))(t); len(in) != 0 {
		t.Fatalf("the new holder got older mail: %+v", in)
	}
	in := mustf(orchclient.Inbox(ctx, at(a.owner, "", old.Address), orchapi.InboxQuery{All: true}))(t)
	if !hasMail(in, m1.ID) || !hasMail(in, rep.ID) {
		t.Fatalf("the old session's inbox: %+v", in)
	}
	// The name now reaches the new holder.
	m3 := mustf(orchclient.Send(ctx, at(a.owner, "harbor", sender.Address), orchapi.SendMailRequest{To: "mini/front-end", Body: "to the new one"}))(t)
	if m3.Recipient != nu.Address {
		t.Fatalf("new holder: %+v", m3)
	}

	// The sender's own id is letters (rid "sender-sess"): mail from it is
	// stored under that address, which no session can take as a name.
	if sender.ID != "senderse" {
		t.Fatalf("sender id %q", sender.ID)
	}
	if _, err := orchclient.SetSessionName(ctx, a.owner, nu.Address, sender.ID); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("a session took another's id as its name: %v", err)
	}
	mustf(orchclient.Register(ctx, a.owner, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "sender-sess", End: true}))(t)
	rep2 := mustf(orchclient.Reply(ctx, at(a.owner, "harbor", old.Address), rep.ID, orchapi.ReplyRequest{Body: "to the ended sender"}))(t)
	if rep2.Recipient != sender.Address {
		t.Fatalf("a reply to an ended sender went to %s", rep2.Recipient)
	}
}
