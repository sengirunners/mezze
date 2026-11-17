package team

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/store"
)

// An ack that lost to another session ("already taken by <x>") still
// closes this replica's copy of the group mail, so it leaves the inbox and
// the prompt hook's unread count at once.
func TestLostAckClosesCopy(t *testing.T) {
	ctx := context.Background()
	rp := testReplica(t)
	at := time.Now().UTC().Truncate(time.Millisecond)
	err := store.Tx(ctx, rp.db, func(tx *sql.Tx) error {
		return putMail(ctx, tx, orchapi.Mail{ID: "m1", Recipient: "machine:mini", From: "human:ana", Created: at})
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(orchapi.AckResponse{ID: "m1", Acked: false, AckedBy: "mini/bbbbbbbb", AckedAt: &at})
	err = store.Tx(ctx, rp.db, func(tx *sql.Tx) error {
		return applyResult(ctx, tx, outEntry{kind: "ack", item: "m1"}, body)
	})
	if err != nil {
		t.Fatal(err)
	}
	var acked int
	if err := rp.db.QueryRow(`SELECT acked FROM mail WHERE id = 'm1'`).Scan(&acked); err != nil || acked != 1 {
		t.Fatalf("copy still open after a lost ack: acked=%d err=%v", acked, err)
	}
	// An answer arriving after the taker's event keeps the real taker's name.
	var js string
	if err := rp.db.QueryRow(`SELECT json FROM mail WHERE id = 'm1'`).Scan(&js); err != nil {
		t.Fatal(err)
	}
	taken := at.Add(time.Second)
	err = store.Tx(ctx, rp.db, func(tx *sql.Tx) error {
		return putMail(ctx, tx, orchapi.Mail{ID: "m2", Recipient: "machine:mini", From: "human:ana", Created: at, AckedBy: "mini/bbbbbbbb", AckedAt: &taken})
	})
	if err != nil {
		t.Fatal(err)
	}
	late, _ := json.Marshal(orchapi.AckResponse{ID: "m2", Acked: false, AckedBy: "someone", AckedAt: &taken})
	err = store.Tx(ctx, rp.db, func(tx *sql.Tx) error {
		return applyResult(ctx, tx, outEntry{kind: "ack", item: "m2"}, late)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := rp.db.QueryRow(`SELECT json FROM mail WHERE id = 'm2'`).Scan(&js); err != nil {
		t.Fatal(err)
	}
	var m orchapi.Mail
	if json.Unmarshal([]byte(js), &m) != nil || m.AckedBy != "mini/bbbbbbbb" {
		t.Fatalf("late lost-ack answer replaced the taker: %s", js)
	}
}
