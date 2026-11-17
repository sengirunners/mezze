package session

import (
	"context"
	"github.com/sengirunners/mezze/internal/orchclient"
	"os"
	"sort"
	"testing"
	"time"
)

// TestLatency measures attach (to the first replayed byte) and keystroke
// echo (a byte typed on an attach until its echo comes back) through the
// session's holder. ZM_LATENCY=1 runs it; it is a measurement, not a gate
// (SPEC 8 budgets: attach 100 ms, echo as before holders).
func TestLatency(t *testing.T) {
	if os.Getenv("ZM_LATENCY") == "" {
		t.Skip("ZM_LATENCY=1 runs it")
	}
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.spawn("shell", `i=0; while [ $i -lt 2000 ]; do echo "scrollback line $i ................................................"; i=$((i+1)); done; exec cat`, nil)
	h.waitOutput(ss.ID, "scrollback line 1999")
	var attach, echo []time.Duration
	buf := make([]byte, 64<<10)
	for i := 0; i < 50; i++ {
		t0 := time.Now()
		conn, err := orchclient.Attach(ctx, h.c, ss.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Read(buf); err != nil {
			t.Fatal(err)
		}
		attach = append(attach, time.Since(t0))
		conn.Close()
	}
	conn, err := orchclient.Attach(ctx, h.c, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	got := make(chan int, 1024)
	go func() {
		b := make([]byte, 64<<10)
		for {
			n, err := conn.Read(b)
			if err != nil {
				close(got)
				return
			}
			got <- n
		}
	}()
	for quiet := time.After(300 * time.Millisecond); ; { // the replay
		select {
		case <-got:
			quiet = time.After(300 * time.Millisecond)
			continue
		case <-quiet:
		}
		break
	}
	for i := 0; i < 300; i++ {
		t0 := time.Now()
		if _, err := conn.Write([]byte{'x'}); err != nil {
			t.Fatal(err)
		}
		if _, ok := <-got; !ok {
			t.Fatal("attach closed")
		}
		echo = append(echo, time.Since(t0))
		time.Sleep(2 * time.Millisecond)
	}
	pct := func(d []time.Duration, p float64) time.Duration {
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		return d[int(float64(len(d)-1)*p)]
	}
	t.Logf("attach to first byte: p50 %v p95 %v max %v (n=%d)", pct(attach, .5), pct(attach, .95), pct(attach, 1), len(attach))
	t.Logf("keystroke echo:       p50 %v p95 %v max %v (n=%d)", pct(echo, .5), pct(echo, .95), pct(echo, 1), len(echo))
}
