package fulltest

import (
	"context"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func TestPersonalMailHiddenFromProjectOperator(t *testing.T) {
	f := newLeakFixture(t)
	ctx := context.Background()
	owner := f.ownerAt("harbor/solver", "")
	tok, err := owner.MintToken(ctx, api.MintTokenRequest{As: "fixture-operator", Project: "harbor", Role: "member"})
	if err != nil {
		t.Fatal(err)
	}
	m, err := orchclient.Send(ctx, owner, orchapi.SendMailRequest{To: "human:ivo", Body: "invented personal operator fixture", Needs: orchapi.NeedsAny, Audience: orchapi.MailAudienceParticipants})
	if err != nil {
		t.Fatal(err)
	}
	op := client.WithHandler(f.dm.Handler)
	op.Token = tok.Secret
	op.Project = "harbor"
	op.ProjectFrom = "flag"
	for _, id := range []string{m.ID, m.ID[20:]} {
		if _, err := orchclient.GetMail(ctx, op, id); !api.IsCode(err, api.CodeNotFound) {
			t.Fatal("operator read personal mail", err)
		}
		if _, err := orchclient.Thread(ctx, op, id); !api.IsCode(err, api.CodeNotFound) {
			t.Fatal("operator read personal thread", err)
		}
	}
	ms, err := orchclient.Mail(ctx, op, orchapi.MailQuery{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range ms {
		if v.ID == m.ID {
			t.Fatal("operator listed private metadata")
		}
	}
}
