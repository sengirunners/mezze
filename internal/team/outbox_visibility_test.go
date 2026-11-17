package team

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
)

func TestOutboxAudienceSurvivesParentRemovalBeforeLimit(t *testing.T) {
	rp := testReplica(t)
	ctx := context.Background()
	p := api.ProjectRef{ID: ids.New(), Slug: "fixture"}
	owner, agent := "human:bea", "bea-mac/1234abcd"
	parent := orchapi.Mail{ID: ids.New(), Area: "solver", Audience: orchapi.MailAudienceParticipants, Participants: []string{owner, "human:other"}, From: "human:other", To: owner}
	if err := rp.write(ctx, func(tx *sql.Tx) error { return putMail(ctx, tx, parent) }); err != nil {
		t.Fatal(err)
	}
	// An older public entry must remain discoverable behind 1001 hidden rows.
	public, err := rp.enqueue(ctx, "mail", http.MethodPost, "/v0/mail", []byte(`{"to":"human:other","body":"invented public","area":"solver"}`), owner, "", "")
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"body":"invented private reply","client_request_id":"invented-key"}`)
	v := rp.writeVisibility(ctx, "reply", "/v0/mail/"+parent.ID+"/reply", body, owner)
	raw, _ := json.Marshal(v)
	if err := rp.write(ctx, func(tx *sql.Tx) error {
		for i := 0; i < 1001; i++ {
			if _, err := tx.Exec(`INSERT INTO outbox (id,kind,method,path,body,as_addr,item,state,error,created,visibility) VALUES (?,'reply','POST','/fixture',?,?,?,'failed',?,0,?)`, ids.New(), string(body), owner, parent.ID, "invented-private-error "+parent.ID, string(raw)); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`DELETE FROM mail WHERE id=?`, parent.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, all := range []bool{false, true} {
		got, err := rp.entries(ctx, p, all, authz.All(), agent, true)
		if err != nil || len(got) != 1 || got[0].ID != public.id {
			t.Fatalf("private rows/metadata leaked or consumed limit: %d %v", len(got), err)
		}
		got, err = rp.entries(ctx, p, all, authz.All(), owner, true)
		if err != nil || len(got) != 1000 {
			t.Fatalf("participant lost rows: %d %v", len(got), err)
		}
	}
	// Scope and replica capability still apply to exact participants.
	sc := authz.FromTuples([]authz.Tuple{{Subject: "g", Relation: authz.Guest, Object: authz.AreaObject(p.ID, "billing")}}, nil)
	got, err := rp.entries(ctx, p, true, sc, owner, true)
	if err != nil || len(got) != 0 {
		t.Fatal("area leak", got, err)
	}
	got, err = rp.entries(ctx, p, true, authz.All(), owner, false)
	if err != nil || len(got) != 1 || got[0].ID != public.id {
		t.Fatal("unaware replica leak", len(got), err)
	}
	// A new unresolved reply never infers public visibility.
	v = rp.writeVisibility(ctx, "reply", "/v0/mail/"+parent.ID+"/reply", body, owner)
	if len(v.Readers) != 1 || v.Readers[0] != owner {
		t.Fatal("missing parent opened audience", v)
	}
}
