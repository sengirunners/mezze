package webui

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/project"
	zmstore "github.com/sengirunners/potluck/kit/store"
)

// TestPushSubscriptionsNeverInABackup: a registry copy staged like an
// off-site set (every core.BackupScrubber, then VACUUM) holds no
// subscription's endpoint or keys; the live registry keeps them.
func TestPushSubscriptionsNeverInABackup(t *testing.T) {
	ctx := context.Background()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Machine, cfg.Human = "mini", "ana"
	db, err := storetest.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg, err := project.Open(ctx, db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := events.New(ctx, db, cfg.Machine)
	if err != nil {
		t.Fatal(err)
	}
	d := &core.Deps{Config: cfg, Store: db, Projects: reg, Events: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	orchcore.Wire(d)
	s := New(d)
	if _, err := reg.Create(ctx, reg.Local(""), api.NewProjectRequest{Slug: "alpha"}); err != nil {
		t.Fatal(err)
	}
	tok, err := reg.Mint(ctx, reg.Local(""), api.MintTokenRequest{As: "ivo", Project: "alpha", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := reg.FromToken(ctx, tok.Secret, true)
	if err != nil {
		t.Fatal(err)
	}
	person := *sub
	person.TokenID, person.Via = "", authz.ViaTailnet
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	var req orchapi.DevicePushRequest
	req.Endpoint = "https://fcm.googleapis.com/fcm/send/cap-url-for-the-backup-test-0123456789"
	req.Keys.P256DH, req.Keys.Auth = raw64.EncodeToString(priv.PublicKey().Bytes()), raw64.EncodeToString(auth)
	if err := s.SubscribeDevice(ctx, &person, "nivo", req); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Registry().ExecContext(ctx, `INSERT INTO web_push_queue(event_id,endpoint_hash,project,type) VALUES('e1',?,'p','t')`, pushHash(req.Endpoint)); err != nil {
		t.Fatal(err)
	}

	staged := filepath.Join(t.TempDir(), "registry.db")
	if _, err := db.Registry().ExecContext(ctx, `VACUUM INTO ?`, staged); err != nil {
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
	for _, f := range d.BackupScrubbers() {
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
	var n int
	if err := cp.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM web_push_subscriptions) + (SELECT count(*) FROM web_push_queue)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("staged copy keeps %d push rows (%v)", n, err)
	}
	cp.Close()
	b, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{req.Endpoint, req.Keys.P256DH, req.Keys.Auth} {
		if bytes.Contains(b, []byte(v)) {
			t.Fatalf("the staged registry still holds %q", v)
		}
	}
	if err := db.Registry().QueryRowContext(ctx, `SELECT count(*) FROM web_push_subscriptions`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("the live registry changed: %d %v", n, err)
	}
}
