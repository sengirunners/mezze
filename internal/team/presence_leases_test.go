package team

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

func TestCanonicalLeaseCachePreservesConcurrentAnswers(t *testing.T) {
	const p, addr = "p", "bea-mac/1234abcd"
	for _, mutation := range []string{"claim", "release", "alias-release", "ABA", "heartbeat", "old-scope-heartbeat", "native-route"} {
		t.Run(mutation, func(t *testing.T) {
			c := replicaCache{}
			c.claimedIn(p, addr, "local:old")
			c.claimedIn("other", addr, "lock:other")
			before := c.admitLeaseRequest()
			canonical := map[string]map[string]bool{addr: {"local:old": true, "lock:canonical": true}}
			switch mutation {
			case "claim":
				c.claimedIn(p, addr, "lock:fresh")
			case "release":
				c.released(addr, "local:old")
			case "alias-release":
				c.endLease(addr, "old")
			case "ABA":
				c.released(addr, "local:old")
				c.claimedIn(p, addr, "local:old")
				c.claimed(addr, "local:old")
				canonical[addr] = map[string]bool{}
			case "heartbeat":
				c.held[p][addr] = map[string]bool{}
				delete(c.leaseIn[addr], p)
				c.touchLeaseActor(p, addr, 1, c.admitLeaseRequest())
			case "old-scope-heartbeat":
				c.held[p][addr]["lock:old-scope"] = true
				c.touchLeaseActor(p, addr, 0, c.admitLeaseRequest())
			case "native-route":
				c.held[p][addr] = map[string]bool{}
				canonical[addr] = map[string]bool{}
				c.claimedIn(p, addr, "")
			}
			c.reconcileLeases(p, canonical, before, 1)
			if !c.held["other"][addr]["lock:other"] || !c.leaseIn[addr]["other"] {
				t.Fatal("another project's accepted lease changed")
			}
			switch mutation {
			case "claim":
				if !c.held[p][addr]["lock:fresh"] || !c.held[p][addr]["lock:canonical"] {
					t.Fatalf("fresh answer or canonical lease lost: %v", c.held[p][addr])
				}
			case "old-scope-heartbeat":
				if c.held[p][addr]["lock:old-scope"] || !c.held[p][addr]["lock:canonical"] {
					t.Fatalf("old scope survived canonical replacement: %v", c.held[p][addr])
				}
			case "release", "alias-release":
				if c.held[p][addr]["local:old"] || !c.held[p][addr]["lock:canonical"] {
					t.Fatalf("release resurrected or unrelated canonical lease lost: %v", c.held[p][addr])
				}
			case "ABA":
				if !c.held[p][addr]["local:old"] {
					t.Fatal("new claim with the same value was overwritten")
				}
			case "heartbeat":
				if len(c.held[p][addr]) != 0 || c.leaseIn[addr][p] {
					t.Fatal("newer authority loss was overwritten")
				}
			case "native-route":
				if !c.leaseIn[addr][p] || len(c.held[p][addr]) != 0 {
					t.Fatal("native claim route lost or invented a held key")
				}
			}
		})
	}
}

func TestCanonicalLeaseCacheCommitsAndScope(t *testing.T) {
	ctx := context.Background()
	const p, addr = "p", "bea-mac/1234abcd"
	for _, mode := range []string{"snapshot", "changes", "scope", "rollback", "lost-commit"} {
		t.Run(mode, func(t *testing.T) {
			rp := testReplica(t)
			s := &Service{d: &core.Deps{Config: &config.Config{Machine: "bea-mac"}}}
			x := &remote{R: api.Remote{Project: api.ProjectRef{ID: p}, State: api.RemoteOK, ScopeVersion: 1}}
			if mode == "scope" {
				s.cache.claimedIn(p, addr, "lock:old-scope")
			}
			c := s.prepareLeaseCache(x, x.Snapshot())
			l := orchapi.Lease{Project: p, Key: "lock:accepted", Holder: addr, Bound: true}
			js, err := json.Marshal(l)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rp.enqueueID(ctx, "pending", "lease", "POST", "/v0/work/lease", []byte(`{}`), addr, "lock:pending", "", outVisibility{}); err != nil {
				t.Fatal(err)
			}
			if mode == "scope" {
				x.R.ScopeVersion++
			}
			if mode == "rollback" {
				fail := errors.New("rollback after canonical write")
				err = rp.writeApplyCommitted(ctx, func(tx *sql.Tx) error {
					if err := putLease(ctx, tx, l); err != nil {
						return err
					}
					return fail
				}, c)
				if !errors.Is(err, fail) {
					t.Fatalf("rollback result: %v", err)
				}
			} else {
				if mode == "lost-commit" {
					applyCommitsLost.Store(true)
					defer applyCommitsLost.Store(false)
				}
				if mode == "changes" {
					_, err = rp.applyChanges(ctx, []api.Event{{ID: "02", Type: orchapi.EventWorkClaimed, Data: js}}, "02", time.Now(), c)
				} else {
					foreign := l
					foreign.Key, foreign.Holder = "lock:foreign", "elsewhere/1234abcd"
					foreignJS, _ := json.Marshal(foreign)
					expired := l
					expired.Key, expired.Bound = "lock:expired", false
					past := time.Now().Add(-time.Hour)
					expired.Expires = &past
					expiredJS, _ := json.Marshal(expired)
					err = rp.applySnapshot(ctx, []api.SnapshotLine{{Kind: orchapi.SnapLease, Data: js}, {Kind: orchapi.SnapLease, Data: foreignJS}, {Kind: orchapi.SnapLease, Data: expiredJS}}, "02", time.Now(), 0, snapshotAnswer{leases: c})
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			want := mode == "snapshot" || mode == "changes"
			// A stale-scope callback must leave the preexisting route intact,
			// not publish its new lease or clear another scope's cache.
			wantRoute := want || mode == "scope"
			if got := s.cache.held[p][addr][l.Key]; got != want || s.cache.leaseIn[addr][p] != wantRoute {
				t.Fatalf("canonical cache held=%v want=%v route=%v want=%v", got, want, s.cache.leaseIn[addr][p], wantRoute)
			}
			if mode == "scope" && !s.cache.holds(addr, "lock:old-scope") {
				t.Fatal("stale-scope callback changed the preexisting cache")
			}
			if s.cache.holds(addr, "lock:pending") {
				t.Fatal("pending intent became an accepted lease")
			}
			if s.cache.holds("elsewhere/1234abcd", "lock:foreign") || s.cache.holds(addr, "lock:expired") {
				t.Fatal("foreign or expired lease entered the local presence view")
			}
			if want {
				next := s.prepareLeaseCache(x, x.Snapshot())
				if _, err := rp.applyChanges(ctx, []api.Event{{ID: "03", Type: orchapi.EventWorkReleased, Data: js}}, "03", time.Now(), next); err != nil {
					t.Fatal(err)
				}
				if s.cache.holds(addr, l.Key) || s.cache.leaseIn[addr][p] {
					t.Fatal("canonical release did not clear the cache and route")
				}
			}
			if mode == "scope" {
				next := s.prepareLeaseCache(x, x.Snapshot())
				if err := rp.applySnapshot(ctx, nil, "03", time.Now(), 1, snapshotAnswer{leases: next}); err != nil {
					t.Fatal(err)
				}
				if s.cache.holds(addr, "lock:old-scope") || s.cache.leaseIn[addr][p] {
					t.Fatal("current-scope snapshot retained prior-scope cache")
				}
			}
		})
	}
}

// The streamed snapshot can omit a claim already accepted by the authority,
// or still contain a lease just released. Both answers must beat the stream,
// including a release before this project's cache has held any key at all.
func TestCanonicalLeaseCacheReplyDuringSnapshot(t *testing.T) {
	ctx := context.Background()
	const p, addr, key = "p", "bea-mac/1234abcd", "lock:overlap"
	for _, release := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh-claim", true: "first-known-release"}[release], func(t *testing.T) {
			rp := testReplica(t)
			s := &Service{d: &core.Deps{Config: &config.Config{Machine: "bea-mac"}}}
			x := &remote{R: api.Remote{Project: api.ProjectRef{ID: p}, State: api.RemoteOK, ScopeVersion: 1}}
			c := s.prepareLeaseCache(x, x.Snapshot())
			var lines []api.SnapshotLine
			if release {
				js, _ := json.Marshal(orchapi.Lease{Project: p, Holder: addr, Key: key, Bound: true})
				lines = []api.SnapshotLine{{Kind: orchapi.SnapLease, Data: js}}
				s.cache.released(addr, key)
			} else {
				s.cache.claimedIn(p, addr, key)
			}
			if err := rp.applySnapshot(ctx, lines, "02", time.Now(), 0, snapshotAnswer{leases: c}); err != nil {
				t.Fatal(err)
			}
			if got := s.cache.holds(addr, key); got == release {
				t.Fatalf("overlapping reply overwritten: held=%v release=%v", got, release)
			}
		})
	}
}

func TestCanonicalLeaseCacheCommitBoundary(t *testing.T) {
	ctx := context.Background()
	for _, failRead := range []bool{false, true} {
		t.Run(map[bool]string{false: "commit", true: "rollback-read"}[failRead], func(t *testing.T) {
			rp := testReplica(t)
			called := false
			fail := errors.New("canonical lease query failed")
			c := &leaseCacheCommit{
				read: func(ctx context.Context, tx *sql.Tx) error {
					var n int
					if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM leases`).Scan(&n); err != nil {
						return err
					}
					if n != 1 {
						t.Fatalf("canonical query missed in-transaction rows: %d", n)
					}
					if failRead {
						return fail
					}
					return nil
				},
				committed: func() {
					called = true
					if rp.snapshotMu.TryLock() {
						rp.snapshotMu.Unlock()
						t.Error("canonical publication escaped snapshot fence")
					}
					if rp.writing.TryLock() {
						rp.writing.Unlock()
						t.Error("accepted writer can overtake canonical publication")
					}
					var n int
					if err := rp.db.QueryRowContext(ctx, `SELECT count(*) FROM leases`).Scan(&n); err != nil || n != 1 {
						t.Errorf("publication before commit: rows=%d err=%v", n, err)
					}
				},
			}
			js, _ := json.Marshal(orchapi.Lease{Key: "lock:accepted", Bound: true})
			err := rp.applySnapshot(ctx, []api.SnapshotLine{{Kind: orchapi.SnapLease, Data: js}}, "02", time.Now(), 0, snapshotAnswer{leases: c})
			if failRead {
				if !errors.Is(err, fail) || called {
					t.Fatalf("failed read published cache: err=%v called=%v", err, called)
				}
				var n int
				if err := rp.db.QueryRowContext(ctx, `SELECT count(*) FROM leases`).Scan(&n); err != nil || n != 0 {
					t.Fatalf("failed canonical query committed SQL: rows=%d err=%v", n, err)
				}
			} else if err != nil || !called {
				t.Fatalf("commit callback missing: err=%v called=%v", err, called)
			}
		})
	}
}

// Both replies arrive after the canonical request starts. Only a heartbeat
// admitted after that request can supersede its newly accepted SQL lease.
func TestCanonicalLeaseCacheHeartbeatRequestOrder(t *testing.T) {
	ctx := context.Background()
	const p, addr, key = "p", "bea-mac/1234abcd", "lock:canonical"
	for _, newerRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "old-request-late-reply", true: "new-request-late-reply"}[newerRequest], func(t *testing.T) {
			rp := testReplica(t)
			s := &Service{d: &core.Deps{Config: &config.Config{Machine: "bea-mac"}}}
			x := &remote{R: api.Remote{Project: api.ProjectRef{ID: p}, State: api.RemoteOK, ScopeVersion: 1}}
			request := s.cache.admitLeaseRequest()
			canonical := s.prepareLeaseCache(x, x.Snapshot())
			if newerRequest {
				request = s.cache.admitLeaseRequest()
			}
			// The authority's old answer contains no leases. Apply the actual
			// response path between the snapshot's admission and commit.
			s.applyPresence(ctx, x.Snapshot(), map[string]orchapi.Session{addr: {Address: addr}}, nil,
				map[string][]string{addr: nil}, orchapi.SessionsBeatAnswer{Sessions: []orchapi.PresenceAnswer{{Address: addr}}}, presenceAnswerContext{leaseRequest: request})
			js, _ := json.Marshal(orchapi.Lease{Project: p, Holder: addr, Key: key, Bound: true})
			if err := rp.applySnapshot(ctx, []api.SnapshotLine{{Kind: orchapi.SnapLease, Data: js}}, "02", time.Now(), 0, snapshotAnswer{leases: canonical}); err != nil {
				t.Fatal(err)
			}
			if got := s.cache.holds(addr, key); got == newerRequest {
				t.Fatalf("request order lost: held=%v newer-request=%v", got, newerRequest)
			}
			if s.cache.leaseIn[addr][p] == newerRequest {
				t.Fatal("canonical route disagrees with request freshness")
			}
		})
	}
}
