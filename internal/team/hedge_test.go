package team_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// hedging sets the hedge tunables for one test.
func hedging(t *testing.T, after time.Duration, max int) {
	t.Helper()
	oldA, oldM := teamlink.HedgeAfter, teamlink.MaxHedges
	teamlink.HedgeAfter, teamlink.MaxHedges = after, max
	t.Cleanup(func() { teamlink.HedgeAfter, teamlink.MaxHedges = oldA, oldM })
}

// A read forwarded to the authority on a connection that has stopped
// moving is asked again on a new one and answers in about HedgeAfter, not
// when the stuck connection wakes up.
func TestHedgedReadOutrunsStuckConnection(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	rc := at(r.owner, "harbor", "")
	read := func() time.Duration {
		t.Helper()
		t0 := time.Now()
		mustf(orchclient.Sessions(ctx, rc, orchapi.SessionQuery{}))(t) // forwarded: the copy has no sessions
		return time.Since(t0)
	}
	read()

	// Without hedging the stall is the read's (so the stall is real).
	hedging(t, 100*time.Millisecond, 0)
	tn.Stall("potluck-bea-mac", "potluck-mini", 1500*time.Millisecond)
	if d := read(); d < 1400*time.Millisecond {
		t.Fatalf("unhedged read during a 1.5 s stall took %s: the stall did not hold it", d)
	}

	hedging(t, 100*time.Millisecond, 2)
	read()
	tn.Stall("potluck-bea-mac", "potluck-mini", 20*time.Second)
	if d := read(); d > 2*time.Second {
		t.Fatalf("hedged read during a 20 s stall took %s", d)
	}
	// The socket's health shows it.
	h := mustf(r.owner.Health(ctx))(t)
	if h.Hedges == nil || h.Hedges.Hedges < 1 || h.Hedges.Won < 1 || h.Hedges.Reads1m < 1 {
		t.Fatalf("health hedges: %+v", h.Hedges)
	}

	// With the budget spent, a read waits the stall out and the refusal is
	// counted.
	oldShare := teamlink.HedgeShare
	teamlink.HedgeShare = 0
	t.Cleanup(func() { teamlink.HedgeShare = oldShare })
	oldFloor := teamlink.HedgeFloor
	teamlink.HedgeFloor = 0
	t.Cleanup(func() { teamlink.HedgeFloor = oldFloor })
	read()
	tn.Stall("potluck-bea-mac", "potluck-mini", 1500*time.Millisecond)
	if d := read(); d < 1400*time.Millisecond {
		t.Fatalf("read with no hedge budget took %s during a 1.5 s stall", d)
	}
	if h := mustf(r.owner.Health(ctx))(t); h.Hedges == nil || h.Hedges.Refused < 1 {
		t.Fatalf("health hedges after a refusal: %+v", h.Hedges)
	}
}

// Reads through a replica while the authority takes writes, the copy
// syncs, and the replica's open connections stall now and then (each for
// 3 s): no read waits the stall out (max under 2 s). p50, p99 and max are
// logged; ZM_HEDGE_OFF=1 runs the same load unhedged (the "before").
func TestForwardedReadsP99UnderStalls(t *testing.T) {
	quietPresence(t)
	if os.Getenv("ZM_HEDGE_OFF") != "" {
		hedging(t, 300*time.Millisecond, 0)
	} else {
		hedging(t, 300*time.Millisecond, 2)
	}
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	rc := at(r.owner, "harbor", "")

	run := 8 * time.Second
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // writes at the authority: the copy syncs them
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
			}
			if it, err := orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: fmt.Sprintf("w%d", i), Area: "solver"}); err == nil {
				_, _ = orchclient.Comment(ctx, ao, it.ID, orchapi.CommentRequest{Body: "c"})
			}
		}
	}()
	wg.Add(1)
	go func() { // stalls: every 1.5 s the open connections stop for 3 s
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(1500 * time.Millisecond):
			}
			tn.Stall("potluck-bea-mac", "potluck-mini", 3*time.Second)
		}
	}()
	var mu sync.Mutex
	var ds []time.Duration
	for k := 0; k < 3; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				t0 := time.Now()
				if _, err := orchclient.Sessions(ctx, rc, orchapi.SessionQuery{}); err != nil {
					t.Errorf("read: %v", err)
				}
				mu.Lock()
				ds = append(ds, time.Since(t0))
				mu.Unlock()
				time.Sleep(10 * time.Millisecond)
			}
		}()
	}
	time.Sleep(run)
	close(stop)
	wg.Wait()
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	p := func(q float64) time.Duration {
		return ds[min(len(ds)-1, int(q*float64(len(ds))))].Round(time.Millisecond)
	}
	t.Logf("forwarded reads under 3 s stalls: n=%d p50=%s p99=%s max=%s", len(ds), p(0.5), p(0.99), ds[len(ds)-1].Round(time.Millisecond))
	// Every stall lasts 3 s (they chain): an unhedged read caught in one
	// waits that long or longer; a hedged one about HedgeAfter.
	if max := ds[len(ds)-1]; os.Getenv("ZM_HEDGE_OFF") == "" && max > 2*time.Second {
		t.Fatalf("max %s (p99 %s): a read waited out a stall", max, p(0.99))
	}
}
