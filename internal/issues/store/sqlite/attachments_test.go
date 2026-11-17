package sqlite

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"testing"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	zmstore "github.com/sengirunners/potluck/kit/store"
)

// A new attachment file gives freed pages back (auto_vacuum) and overwrites
// them (secure_delete). A removal whose log truncation another reader holds
// up is not an error: the row is gone, and Settle truncates once the reader
// lets go, so no frame keeps the bytes.
func TestAttachmentsDeleteSettles(t *testing.T) {
	ctx := context.Background()
	s := Open(t.TempDir())
	defer s.Close()
	as, err := s.Attachments(ctx, "p1", true)
	if err != nil {
		t.Fatal(err)
	}
	a := as.(*attachments)
	var av, sd int
	if err := a.db.QueryRow(`PRAGMA auto_vacuum`).Scan(&av); err != nil || av != 1 {
		t.Fatalf("auto_vacuum %d %v", av, err)
	}
	if err := a.db.QueryRow(`PRAGMA secure_delete`).Scan(&sd); err != nil || sd != 1 {
		t.Fatalf("secure_delete %d %v", sd, err)
	}
	data := make([]byte, 64<<10)
	rand.Read(data)
	now := time.Now()
	at := istore.Attachment{IssueID: "x-1", IssueAttachment: orchapi.IssueAttachment{ID: "01AAAAAAAAAAAAAAAAAAAAAAAA", Type: "image/png",
		Size: int64(len(data)), SHA256: "00", Created: now, Expires: now.Add(time.Hour)}}
	if err := as.Add(ctx, at, data, 3, now); err != nil {
		t.Fatal(err)
	}
	path := s.attachmentsPath("p1")
	inFile := func() bool {
		var all []byte
		for _, suf := range []string{"", "-wal"} {
			b, _ := os.ReadFile(path + suf)
			all = append(all, b...)
		}
		return bytes.Contains(all, data[1000:1064])
	}
	// Another reader holds a snapshot: the checkpoint cannot truncate.
	other, err := zmstore.OpenFile(ctx, path, 1, time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM attachments`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("reader: %d %v", n, err)
	}
	gone, err := as.Delete(ctx, []string{at.ID, "01BBBBBBBBBBBBBBBBBBBBBBBB"})
	if err != nil || len(gone) != 1 || gone[0] != at.ID {
		t.Fatalf("delete: %v %v", gone, err)
	}
	if again, err := as.Delete(ctx, []string{at.ID}); err != nil || len(again) != 0 {
		t.Fatalf("a second delete removes nothing: %v %v", again, err)
	}
	if !a.pending {
		t.Fatal("the held-up truncation is not pending")
	}
	tx.Rollback()
	if err := as.Settle(ctx); err != nil || a.pending {
		t.Fatalf("settle: %v pending=%v", err, a.pending)
	}
	if inFile() {
		t.Fatal("the removed bytes are still in the file")
	}
}
