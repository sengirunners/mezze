package connections_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/connections/fakeci"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	zmstore "github.com/sengirunners/potluck/kit/store"
)

// TestTokenNeverInABackup: a registry copy staged like an off-site set
// (every core.BackupScrubber, then VACUUM) holds no connection's plaintext
// token; the live connection keeps it and still fetches. A home restored
// from such a copy has the connection with no token: the provider refuses
// it until it is registered again (SPEC 17.9).
func TestTokenNeverInABackup(t *testing.T) {
	x := newCX(t)
	ctx := context.Background()
	const token = "ci-token-for-the-backup-scrubber-test"
	ci := fakeci.New(token, "example-org", 1)
	ci.Now = x.clk.Now
	srv := httptest.NewServer(ci)
	t.Cleanup(srv.Close)
	c, err := orchclient.AddConnection(ctx, x.owner, orchapi.NewConnectionRequest{Project: "harbor", Name: "Fake CI",
		Endpoint: srv.URL, Token: token, AllowPrivate: true})
	if err != nil {
		t.Fatal(err)
	}
	reg := x.dm.Deps.Store.Registry()
	staged := filepath.Join(t.TempDir(), "registry.db")
	if _, err := reg.ExecContext(ctx, `VACUUM INTO ?`, staged); err != nil {
		t.Fatal(err)
	}
	cp, err := zmstore.OpenFile(ctx, staged, 1, time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := cp.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range x.dm.Deps.BackupScrubbers() {
		if err := f(ctx, tx); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := cp.ExecContext(ctx, `VACUUM`); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := cp.QueryRowContext(ctx, `SELECT token FROM connections WHERE id = ?`, c.ID).Scan(&got); err != nil || got != "" {
		t.Fatalf("staged token: %q %v", got, err)
	}
	cp.Close()
	b, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(token)) {
		t.Fatal("the staged registry still holds the connection's token")
	}
	if err := reg.QueryRowContext(ctx, `SELECT token FROM connections WHERE id = ?`, c.ID).Scan(&got); err != nil || got != token {
		t.Fatalf("the live registry changed: %q %v", got, err)
	}
	if s := x.show(x.owner, c.ID); s.State != orchapi.ConnectionFresh {
		t.Fatalf("before the restore: %+v", s)
	}

	// As restored: the scrubbers ran on the live registry; the provider
	// refuses the tokenless fetch, so the connection is not fresh.
	tx, err = reg.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range x.dm.Deps.BackupScrubbers() {
		if err := f(ctx, tx); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	x.clk.add(time.Hour)
	if s := x.show(x.owner, c.ID); s.State == orchapi.ConnectionFresh || s.Reason == "" {
		t.Fatalf("a restored connection without its token: %+v", s)
	}
}
