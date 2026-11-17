package team_test

import (
	"context"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// The claim race (SPEC 17.7): replicas race to claim the same items through
// their proxies; every item has exactly one winner, and every loser gets
// a conflict. Round C may acknowledge a claim as queued: resolve that
// original write's durable result, never make a second claim attempt.
func TestClaimRace(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	const replicas, workers, items = 4, 4, 12
	ao := at(a.owner, "harbor", "")
	var ids []string
	for i := 0; i < items; i++ {
		ids = append(ids, mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: fmt.Sprintf("race %d", i), Area: "solver"}))(t).ID)
	}
	var socks []*client.Client
	var machines []string
	var oracles []queuedClaimOracle
	for i := 0; i < replicas; i++ {
		m := fmt.Sprintf("box-%d", i)
		r, joined := replica(t, tn, a, m, fmt.Sprintf("nBOX%d", i), "harbor", fmt.Sprintf("farm-%d", i), "member")
		socks = append(socks, r.sock)
		machines = append(machines, m)
		oracles = append(oracles, newQueuedClaimOracle(t, a, joined))
	}
	type win struct{ id, holder string }
	var mu sync.Mutex
	var wins []win
	conflicts, other := 0, 0
	type pendingClaim struct {
		oracle       queuedClaimOracle
		item, holder string
		err          error
	}
	var pending []pendingClaim
	record := func(item, holder string, res orchapi.ClaimResponse, err error) {
		switch {
		case err == nil:
			wins = append(wins, win{item, res.Lease.Holder})
		case api.IsCode(err, api.CodeConflict):
			conflicts++
		default:
			other++
			t.Errorf("claim %s as %s: %v", item, holder, err)
		}
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for ri, sc := range socks {
		for w := 0; w < workers; w++ {
			holder := fmt.Sprintf("%s/%08d", machines[ri], w)
			c := at(sc, "harbor", holder)
			order := rand.New(rand.NewSource(int64(ri*100 + w))).Perm(items)
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for _, k := range order {
					res, err := orchclient.Claim(ctx, c, ids[k], orchapi.ClaimRequest{TTLSeconds: 600})
					mu.Lock()
					if api.IsCode(err, api.CodeQueued) {
						pending = append(pending, pendingClaim{oracles[ri], ids[k], holder, err})
					} else {
						record(ids[k], holder, res, err)
					}
					mu.Unlock()
				}
			}()
		}
	}
	close(start)
	wg.Wait()
	// Observing receipts does not submit work or extend ClaimWait. All
	// queued originals share the existing outbox-observation budget.
	settleCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	seen := map[string]bool{}
	for _, claim := range pending {
		q, err := claim.oracle.ticket(claim.item, claim.err)
		if err != nil {
			record(claim.item, claim.holder, orchapi.ClaimResponse{}, err)
			continue
		}
		if seen[q.Write] {
			record(claim.item, claim.holder, orchapi.ClaimResponse{}, fmt.Errorf("duplicate queued write ID %s", q.Write))
			continue
		}
		seen[q.Write] = true
		res, err := claim.oracle.resolve(settleCtx, claim.item, claim.err)
		record(claim.item, claim.holder, res, err)
	}
	byItem := map[string][]string{}
	for _, w := range wins {
		byItem[w.id] = append(byItem[w.id], w.holder)
	}
	for _, id := range ids {
		if len(byItem[id]) != 1 {
			t.Errorf("%s: %d winners %v", id, len(byItem[id]), byItem[id])
		}
	}
	if conflicts != replicas*workers*items-items || other != 0 {
		t.Errorf("conflicts %d (want %d), other errors %d", conflicts, replicas*workers*items-items, other)
	}
	// The authority agrees: one live lease per item, held by the winner.
	ls := mustf(orchclient.Leases(ctx, ao, orchapi.LeaseQuery{Project: "harbor"}))(t)
	if len(ls) != items {
		t.Fatalf("authority holds %d leases, want %d", len(ls), items)
	}
	for _, l := range ls {
		if h := byItem[l.Key]; len(h) != 1 || h[0] != l.Holder {
			t.Errorf("lease %s held by %s, winner %v", l.Key, l.Holder, h)
		}
	}
	t.Logf("%d replicas x %d workers x %d items: %d winners, %d conflicts", replicas, workers, items, len(wins), conflicts)
}

// TestProxyLatency measures, over loopback, a proxied read and a proxied
// claim from a replica's Unix socket against the 17.7 budgets: claim p95 <=
// 50 ms; a proxied read <= one round trip + 30 ms (the round trip is
// measured as the same read made directly at the authority over the
// tailnet with the replica's token).
func TestProxyLatency(t *testing.T) {
	if !teamTimingEnabled() {
		t.Skip("ZM_TEAM_SPEED not set (bench.yml sets it)")
	}
	timing := newTeamTiming(t, "proxy")
	if testing.Short() {
		t.Skip("latency")
	}
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	for i := 0; i < 50; i++ {
		mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: fmt.Sprintf("item %d", i), Area: "solver"}))(t)
	}
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	rc := at(r.sock, "harbor", "bea-mac/00000001")
	direct := client.NewRemote("http://potluck-mini:7433", tailnetClientAs(t, tn, "bea-mac"))
	direct.Token = readToken(t, j.Remote.TokenFile)
	direct.Project = j.Remote.Project.ID
	const n = 200
	measure := func(f func(i int) error) []time.Duration {
		var ds []time.Duration
		for i := 0; i < n; i++ {
			t0 := time.Now()
			if err := f(i); err != nil {
				t.Fatal(err)
			}
			ds = append(ds, time.Since(t0))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds
	}
	q := orchapi.WorkQuery{Limit: 50}
	rtt := measure(func(int) error { _, err := orchclient.WorkList(ctx, direct, q); return err })
	read := measure(func(int) error { _, err := orchclient.WorkList(ctx, rc, q); return err })
	var claimIDs []string
	for i := 0; i < n; i++ {
		claimIDs = append(claimIDs, mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: fmt.Sprintf("claim %d", i), Area: "solver"}))(t).ID)
	}
	claim := measure(func(i int) error {
		_, err := orchclient.Claim(ctx, rc, claimIDs[i], orchapi.ClaimRequest{})
		return err
	})
	t.Logf("loopback, %d runs, p50 / p95: direct read (the round trip) %v / %v; proxied read %v / %v; proxied claim %v / %v",
		n, rtt[n/2], rtt[n*95/100], read[n/2], read[n*95/100], claim[n/2], claim[n*95/100])
	timing.record("team/proxy-claim", claim, 50*time.Millisecond)
	timing.record("team/proxy-read", read, rtt[n*95/100]+30*time.Millisecond)
}
