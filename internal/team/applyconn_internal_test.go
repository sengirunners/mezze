package team

import (
	"context"
	"os"
	"testing"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

// A copy applies what the authority sends, and settles, on its NORMAL
// connection; only the outbox's own writes wait on the disk (FULL).
func TestReplicaApplyConnection(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{d: &core.Deps{Config: cfg, Store: copies(t)}}
	rp, err := s.openReplica(context.Background(), "01TESTPROJECT")
	if err != nil {
		t.Fatal(err)
	}
	defer rp.close(true)
	if rp.apply == nil {
		t.Fatal("no apply connection")
	}
	var full, normal int
	_ = rp.db.QueryRowContext(context.Background(), `PRAGMA synchronous`).Scan(&full)
	_ = rp.apply.QueryRowContext(context.Background(), `PRAGMA synchronous`).Scan(&normal)
	if full != 2 || normal != 1 {
		t.Fatalf("synchronous: outbox %d (want 2, FULL), apply %d (want 1, NORMAL)", full, normal)
	}
}
