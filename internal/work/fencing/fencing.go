// Package fencing is the lease fencing tokens' storage (SPEC 10 "Fencing"):
// each lease file's per-project counter and the check a fenced write makes.
// The work module owns the leases; this package only knows the two columns
// (leases.fence, lease_fences.n) it reads and writes.
package fencing

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
)

// Script adds fencing tokens to a leases table (registry.db and every
// project.db): lease_fences holds each project's last token.
const Script = `ALTER TABLE leases ADD COLUMN fence INTEGER NOT NULL DEFAULT 0;
CREATE TABLE lease_fences (project TEXT PRIMARY KEY, n INTEGER NOT NULL);`

// Queryer is the part of *sql.DB and *sql.Tx this package uses.
type Queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Next is project's next token, inside the claim's transaction: larger than
// every one before it by a random step (1 to 65536), so a reader sees roughly
// how many claims there were, never the exact count.
func Next(ctx context.Context, tx *sql.Tx, project string) (int64, error) {
	var b [2]byte
	_, _ = rand.Read(b[:])
	step := int64(binary.BigEndian.Uint16(b[:])) + 1
	var n int64
	err := tx.QueryRowContext(ctx, `INSERT INTO lease_fences (project, n) VALUES (?, ?)
		ON CONFLICT (project) DO UPDATE SET n = n + excluded.n RETURNING n`, project, step).Scan(&n)
	return n, err
}

// Raise makes project's next token in q larger than n (a move brought
// leases, or a counter, from another file).
func Raise(ctx context.Context, q Queryer, project string, n int64) error {
	if n <= 0 {
		return nil
	}
	_, err := q.ExecContext(ctx, `INSERT INTO lease_fences (project, n) VALUES (?, ?)
		ON CONFLICT (project) DO UPDATE SET n = max(n, excluded.n)`, project, n)
	return err
}

// Last is the largest token project has in q: its counter or a lease row.
func Last(ctx context.Context, q Queryer, project string) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, `SELECT max(coalesce((SELECT n FROM lease_fences WHERE project = ?), 0),
		coalesce((SELECT max(fence) FROM leases WHERE project = ?), 0))`, project, project).Scan(&n)
	return n, err
}

// Lease is what a check needs of the lease a fence names: nil when there is none.
type Lease struct {
	Fence  int64
	Holder string
	Live   bool
}

// OK reports whether cur is holder's lease with fence (0 = no fence named:
// always ok). live: cur must also be live (a write; a release may end its own
// lease past its deadline before the ticker does).
func OK(cur *Lease, holder string, fence int64, live bool) bool {
	if fence == 0 {
		return true
	}
	return cur != nil && cur.Fence == fence && cur.Holder == holder && (!live || cur.Live)
}
