package team

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

func TestReplicaPersonalMailFiltersBeforeLimitsAndSuffixes(t *testing.T) {
	rp := testReplica(t)
	ctx := context.Background()
	owner := "human:bea"
	visible := orchapi.Mail{ID: "01M6JW00000000000000000000", From: "human:other", Recipient: "human:bea", Created: time.Unix(100, 0), Body: "invented public note"}
	private := orchapi.Mail{ID: "01M6JW00000000000000000001", From: "human:other", Recipient: owner, Audience: orchapi.MailAudienceParticipants, Participants: []string{owner}, Body: "invented private note", Created: time.Unix(200, 0), Origin: &orchapi.MailOrigin{Trust: "source_reported"}, ReplyUnavailable: "legacy_return_route_unverified"}
	if err := rp.write(ctx, func(tx *sql.Tx) error {
		if err := putMail(ctx, tx, visible); err != nil {
			return err
		}
		raw, _ := json.Marshal(private)
		_, err := applyEvent(ctx, tx, api.Event{Type: orchapi.EventMailImported, Data: raw})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, reader := range []string{"bea-mac/1234abcd", "human:other", ""} {
		for _, id := range []string{private.ID, private.ID[20:]} {
			if m, ok := rp.findMail(ctx, id, reader); ok {
				t.Fatal("private ID or suffix leak", reader, m)
			}
		}
		ms, err := rp.mailWhere(ctx, api.Remote{}, reader, false, "1=1", nil, 1)
		if err != nil || len(ms) != 1 || ms[0].ID != visible.ID {
			t.Fatal("filter must precede limit", reader, ms, err)
		}
	}
	m, ok := rp.findMail(ctx, private.ID, owner)
	if !ok || m.Origin == nil || m.ReplyUnavailable == "" {
		t.Fatal("owner lost personal provenance", m)
	}
	ms, err := rp.mailWhere(ctx, api.Remote{}, owner, false, "1=1", nil, 1)
	if err != nil || len(ms) != 1 || ms[0].ID != private.ID {
		t.Fatal(ms, err)
	}
	// A private child cannot enter an agent's public thread traversal.
	private.ReplyTo = visible.ID
	if err := rp.write(ctx, func(tx *sql.Tx) error { return putMail(ctx, tx, private) }); err != nil {
		t.Fatal(err)
	}
	ms, err = rp.thread(ctx, visible, "bea-mac/1234abcd")
	if err != nil || len(ms) != 1 {
		t.Fatal("thread leaked private child", ms, err)
	}
}

func TestReplicaClientMailReservationAndOutboxReplay(t *testing.T) {
	rp := testReplica(t)
	ctx := context.Background()
	body := []byte(`{"to":"human:bea","body":"invented queued message","client_request_id":"queued-1"}`)
	id, err := rp.reserveMailRequest(ctx, "mail", "POST", "/v0/mail", body, "")
	if err != nil {
		t.Fatal(err)
	}
	again, err := rp.reserveMailRequest(ctx, "mail", "POST", "/v0/mail", body, "")
	if err != nil || id != again {
		t.Fatal(id, again, err)
	}
	first, err := rp.enqueueID(ctx, id, "mail", "POST", "/v0/mail", body, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := rp.enqueueID(ctx, id, "mail", "POST", "/v0/mail", body, "", "", "")
	if err != nil || first.id != second.id {
		t.Fatal(first, second, err)
	}
	var n int
	if err := rp.db.QueryRow(`SELECT count(*) FROM outbox`).Scan(&n); err != nil || n != 1 {
		t.Fatal("duplicate queued send", n, err)
	}
	changed := []byte(`{"to":"human:bea","body":"changed payload","client_request_id":"queued-1"}`)
	if _, err := rp.reserveMailRequest(ctx, "mail", "POST", "/v0/mail", changed, ""); !api.IsCode(err, api.CodeConflict) {
		t.Fatal("replica payload conflict missing", err)
	}
	if _, err := rp.reserveMailRequest(ctx, "reply", "POST", "/v0/mail/parent/reply", body, ""); !api.IsCode(err, api.CodeConflict) {
		t.Fatal("operation/parent not hash-bound", err)
	}
}
