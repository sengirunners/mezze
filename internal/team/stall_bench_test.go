package team_test

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

func envInt(k string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return def
}

// TestReadStallBench: native issue reads through a replica (epic list, bd
// sql's parent links, stats) on ZM_STALL_N issues (default 12000) while the
// authority and the replica take writes and the copy syncs, for
// ZM_STALL_SECS (default 60); p50/p99/max per route are logged.
// ZM_STALL_INJECT=1 stalls the replica's open connections for 3-7 s every
// 5 s; ZM_HEDGE_OFF=1 is the unhedged "before". Opt in: ZM_STALL_BENCH=1.
func TestReadStallBench(t *testing.T) {
	if os.Getenv("ZM_STALL_BENCH") == "" {
		t.Skip("ZM_STALL_BENCH=1 runs it (minutes, 12k issues)")
	}
	quietPresence(t)
	if os.Getenv("ZM_HEDGE_OFF") != "" {
		hedging(t, time.Second, 0)
	}
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := nativeAuthority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	rs := at(r.sock, "harbor", "")

	n := envInt("ZM_STALL_N", 12000)
	desc := strings.Repeat("Some description text that runs for a while. ", 30)
	start := time.Now()
	var made atomic.Int64
	ids := make([]string, n)
	var epics []string
	var emu sync.Mutex
	var wg sync.WaitGroup
	work := make(chan int)
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				req := orchapi.NewIssueRequest{Title: fmt.Sprintf("issue %d", i), Description: desc, Area: "solver", Labels: []string{"l" + strconv.Itoa(i%7)}}
				if i%40 == 0 {
					req.IssueType = "epic"
				}
				is, err := orchclient.CreateIssue(ctx, ao, req)
				if err != nil {
					t.Errorf("create: %v", err)
					return
				}
				ids[i] = is.ID
				if i%40 == 0 {
					emu.Lock()
					epics = append(epics, is.ID)
					emu.Unlock()
				}
				made.Add(1)
			}
		}()
	}
	for i := 0; i < n; i++ {
		work <- i
	}
	close(work)
	wg.Wait()
	t.Logf("seeded %d in %s", made.Load(), time.Since(start))
	// parent links for a quarter of them
	for i := 0; i < n/4; i++ {
		emu.Lock()
		p := epics[i%len(epics)]
		emu.Unlock()
		if ids[i] == p {
			continue
		}
		_, _ = orchclient.AddIssueDep(ctx, ao, ids[i], orchapi.IssueDepRequest{Target: p, Type: "parent-child"})
	}
	t.Logf("linked in %s", time.Since(start))

	dur := time.Duration(envInt("ZM_STALL_SECS", 60)) * time.Second
	stop := make(chan struct{})
	time.AfterFunc(dur, func() { close(stop) })
	// writers: at the authority and through the replica
	for w := 0; w < envInt("ZM_STALL_WRITERS", 4); w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			c := ao
			if w%2 == 1 {
				c = rs
			}
			for {
				select {
				case <-stop:
					return
				default:
				}
				id := ids[rand.IntN(len(ids))]
				switch rand.IntN(3) {
				case 0:
					_, _ = orchclient.CommentIssue(ctx, c, id, orchapi.IssueCommentRequest{Text: "a comment " + desc[:200]})
				case 1:
					notes := "notes " + strconv.Itoa(rand.Int())
					_, _ = orchclient.UpdateIssue(ctx, c, id, orchapi.UpdateIssueRequest{Notes: &notes})
				case 2:
					p := rand.IntN(4)
					_, _ = orchclient.UpdateIssue(ctx, c, id, orchapi.UpdateIssueRequest{Priority: &p})
				}
				time.Sleep(time.Duration(envInt("ZM_STALL_PAUSE_MS", 20)) * time.Millisecond)
			}
		}(w)
	}
	if os.Getenv("ZM_STALL_INJECT") != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				case <-time.After(5 * time.Second):
				}
				tn.Stall("potluck-bea-mac", "potluck-mini", 3*time.Second+time.Duration(rand.IntN(4000))*time.Millisecond)
			}
		}()
	}
	type sample struct {
		route string
		d     time.Duration
	}
	var smu sync.Mutex
	var samples []sample
	read := func(route string, f func() error) {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			t0 := time.Now()
			err := f()
			d := time.Since(t0)
			if err != nil {
				t.Logf("%s: %v", route, err)
			}
			smu.Lock()
			samples = append(samples, sample{route, d})
			smu.Unlock()
			time.Sleep(50 * time.Millisecond)
		}
	}
	for k := 0; k < envInt("ZM_STALL_READERS", 2); k++ {
		wg.Add(3)
		go read("epics", func() error {
			_, err := orchclient.Issues(ctx, rs, orchapi.IssueQuery{Project: "harbor", Type: "epic"})
			return err
		})
		go read("sql", func() error {
			_, err := orchclient.IssueSQL(ctx, rs, "harbor", "SELECT issue_id, depends_on_issue_id FROM dependencies WHERE type = 'parent-child'")
			return err
		})
		go read("stats", func() error { _, err := orchclient.IssueStats(ctx, rs, "harbor"); return err })
	}
	wg.Wait()
	by := map[string][]time.Duration{}
	for _, s := range samples {
		by[s.route] = append(by[s.route], s.d)
	}
	for _, k := range []string{"epics", "sql", "stats"} {
		ds := by[k]
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		if len(ds) == 0 {
			continue
		}
		q := func(p float64) time.Duration { return ds[min(len(ds)-1, int(p*float64(len(ds))))] }
		t.Logf("%-6s n=%d p50=%s p99=%s max=%s", k, len(ds), q(0.5).Round(time.Millisecond), q(0.99).Round(time.Millisecond), ds[len(ds)-1].Round(time.Millisecond))
	}
}
