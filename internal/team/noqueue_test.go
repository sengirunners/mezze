package team_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/tunnel/tunneltest"
)

// No-queue lease writes and cancel by write id.

func claimWait(t *testing.T, d time.Duration) {
	t.Helper()
	old := team.ClaimWait
	team.ClaimWait = d
	t.Cleanup(func() { team.ClaimWait = old })
}

// nqNoQueue returns c with Zm-No-Queue set.
func nqNoQueue(c *client.Client) *client.Client {
	x := *c
	x.NoQueue = true
	return &x
}

// nqOutcome is the outcome in a no-queue 503's data (and its write id).
func nqOutcome(t *testing.T, err error) api.NoQueueData {
	t.Helper()
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeUnavailable {
		t.Fatalf("want a 503 unavailable, got %v", err)
	}
	var d api.NoQueueData
	if json.Unmarshal(e.Data, &d) != nil || d.Outcome == "" {
		t.Fatalf("no outcome in %+v", e)
	}
	return d
}

type nqRig struct {
	a, r *daemon
	p    *tunneltest.Proxy
	ao   *client.Client // the authority's owner on the project
	rs   *client.Client // a session on the replica, no-queue
	cs   orchapi.Session
}

func nqSetup(t *testing.T) *nqRig {
	t.Helper()
	quietPresence(t)
	a, p := tunnelHome(t, nil, "mini", tailnettest.Identity{})
	teamHarbor(t, a)
	r, _ := tunnelMember(t, a, "bea-mac", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-sess-1")
	rg := &nqRig{a: a, r: r, p: p, ao: at(a.owner, "harbor", ""), cs: cs}
	rg.rs = nqNoQueue(at(r.owner, "", cs.Address))
	waitReachable(t, r, true)
	// A warm connection for the stall tests to hold.
	mustf(orchclient.Lease(context.Background(), rg.rs, orchapi.ClaimRequest{Key: "lock:warm"}))(t)
	eventually(t, 5*time.Second, func() error { // the copy shows it: a release is judged by the copy first
		for _, l := range mustf(orchclient.Leases(context.Background(), r.owner, orchapi.LeaseQuery{Project: "harbor"}))(t) {
			if l.Key == "lock:warm" {
				return nil
			}
		}
		return fmt.Errorf("lock:warm not in the copy")
	})
	return rg
}

func (rg *nqRig) held(t *testing.T, key string) bool {
	t.Helper()
	for _, l := range mustf(orchclient.Leases(context.Background(), rg.ao, orchapi.LeaseQuery{}))(t) {
		if l.Key == key {
			return true
		}
	}
	return false
}

func (rg *nqRig) outbox(t *testing.T) int {
	t.Helper()
	return len(mustf(at(rg.r.owner, "harbor", "").Outbox(context.Background(), "harbor", true))(t))
}

// a. Authority unreachable: nothing is sent, nothing is queued.
func TestNoQueueUnreachableNotSent(t *testing.T) {
	ctx := context.Background()
	rg := nqSetup(t)
	rg.p.Offline(true)
	waitReachable(t, rg.r, false)
	_, err := orchclient.Lease(ctx, rg.rs, orchapi.ClaimRequest{Key: "lock:a"})
	if d := nqOutcome(t, err); d.Outcome != api.OutcomeNotSent {
		t.Fatalf("claim: %+v", d)
	}
	_, err = orchclient.ReleaseLease(ctx, rg.rs, "lock:warm", orchapi.ReleaseRequest{})
	if d := nqOutcome(t, err); d.Outcome != api.OutcomeNotSent {
		t.Fatalf("release: %+v", d)
	}
	if n := rg.outbox(t); n != 0 {
		t.Fatalf("outbox has %d entries", n)
	}
	rg.p.Offline(false)
	waitReachable(t, rg.r, true)
	time.Sleep(300 * time.Millisecond)
	if rg.held(t, "lock:a") || !rg.held(t, "lock:warm") {
		t.Fatal("something was applied later")
	}
}

// b. A delivery held past ClaimWait is cancelled; when the held request does
// arrive it is refused, and the lease is never taken.
func TestNoQueueHeldDeliveryCancelled(t *testing.T) {
	ctx := context.Background()
	rg := nqSetup(t)
	claimWait(t, 400*time.Millisecond)
	for try := 0; ; try++ {
		key := fmt.Sprintf("lock:b%d", try)
		rg.p.Stall(1500 * time.Millisecond) // the open connections only; a new one is not held
		t0 := time.Now()
		_, err := orchclient.Lease(ctx, rg.rs, orchapi.ClaimRequest{Key: key})
		if err == nil && time.Since(t0) < 300*time.Millisecond && try < 4 {
			// It went out on a fresh connection: not the case under test.
			time.Sleep(time.Until(t0.Add(1600 * time.Millisecond)))
			continue
		}
		if time.Since(t0) < 300*time.Millisecond {
			t.Fatalf("the claim was not held (%s): %v", time.Since(t0), err)
		}
		if d := nqOutcome(t, err); d.Outcome != api.OutcomeCancelled || d.Write == "" {
			t.Fatalf("outcome %+v", d)
		}
		if !strings.Contains(err.Error(), "nothing was claimed") {
			t.Fatalf("message: %v", err)
		}
		time.Sleep(time.Until(t0.Add(2200 * time.Millisecond))) // the held request is delivered
		if rg.held(t, key) {
			t.Fatal("the cancelled claim landed")
		}
		break
	}
	if n := rg.outbox(t); n != 0 {
		t.Fatalf("outbox has %d entries", n)
	}
}

// c. The write ran but its answer was held: the cancel says "ran", the
// replica resends the same id and relays the recorded answer.
func TestNoQueueRanThenResent(t *testing.T) {
	ctx := context.Background()
	rg := nqSetup(t)
	claimWait(t, 400*time.Millisecond)
	var key string
	var cr orchapi.ClaimResponse
	for try := 0; ; try++ {
		key = fmt.Sprintf("lock:c%d", try)
		rg.p.StallReplies(1500 * time.Millisecond) // the open connections only; a new one is not held
		t0 := time.Now()
		var err error
		cr, err = orchclient.Lease(ctx, rg.rs, orchapi.ClaimRequest{Key: key})
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(t0) < 300*time.Millisecond && try < 4 {
			// It went out on a fresh connection: not the case under test.
			time.Sleep(time.Until(t0.Add(1600 * time.Millisecond)))
			continue
		}
		if time.Since(t0) < 300*time.Millisecond {
			t.Fatalf("the answer was not held (%s)", time.Since(t0))
		}
		break
	}
	if cr.Lease.Key != key || cr.Lease.Holder != rg.cs.Address {
		t.Fatalf("lease %+v", cr.Lease)
	}
	if !rg.held(t, key) {
		t.Fatal("not held at the authority")
	}
	// The replica knows it holds it (a no-queue release of it is live and works).
	if _, err := orchclient.ReleaseLease(ctx, rg.rs, key, orchapi.ReleaseRequest{}); err != nil {
		t.Fatal(err)
	}
}

// g. Neither the write nor its cancel gets an answer: 503 unknown with the
// write id and a hint that names the project; zm project team cancel (no -P, the
// project from the caller's header) settles it later, consistent with the
// lease.
func TestNoQueueUnknownThenSettled(t *testing.T) {
	ctx := context.Background()
	rg := nqSetup(t)
	claimWait(t, 400*time.Millisecond)
	oldW := team.CancelWait
	team.CancelWait = 300 * time.Millisecond
	t.Cleanup(func() { team.CancelWait = oldW })
	rg.p.StallAll(2 * time.Second)
	_, err := orchclient.Lease(ctx, rg.rs, orchapi.ClaimRequest{Key: "lock:g"})
	d := nqOutcome(t, err)
	if d.Outcome != api.OutcomeUnknown || d.Write == "" {
		t.Fatalf("outcome %+v", d)
	}
	var e *api.Error
	if !errors.As(err, &e) || !strings.Contains(e.Hint, "zm project team cancel -P harbor "+d.Write) {
		t.Fatalf("hint: %+v", e)
	}
	if n := rg.outbox(t); n != 0 {
		t.Fatalf("outbox has %d entries", n)
	}
	var r api.WriteCancelResponse
	eventually(t, 10*time.Second, func() (err error) {
		r, err = at(rg.r.owner, "harbor", "").CancelWrite(ctx, "", d.Write)
		return err
	})
	if held := rg.held(t, "lock:g"); held != (r.State == api.WriteRan) {
		t.Fatalf("settled %+v but held=%v", r, held)
	}
	time.Sleep(300 * time.Millisecond) // anything still in flight has landed
	if held := rg.held(t, "lock:g"); held != (r.State == api.WriteRan) {
		t.Fatalf("settled %+v but later held=%v", r, held)
	}
}

// f. A no-queue release of a lease the caller holds: 200, gone.
func TestNoQueueReleaseHeld(t *testing.T) {
	ctx := context.Background()
	rg := nqSetup(t)
	if _, err := orchclient.Lease(ctx, rg.rs, orchapi.ClaimRequest{Key: "lock:f"}); err != nil {
		t.Fatal(err)
	}
	if !rg.held(t, "lock:f") {
		t.Fatal("not held")
	}
	rr, err := orchclient.ReleaseLease(ctx, rg.rs, "lock:f", orchapi.ReleaseRequest{})
	if err != nil || !rr.Released {
		t.Fatalf("release: %+v %v", rr, err)
	}
	if rg.held(t, "lock:f") {
		t.Fatal("still held")
	}
	if n := rg.outbox(t); n != 0 {
		t.Fatalf("outbox has %d entries", n)
	}
	// zm project team cancel on an id that ran or never did, through the replica.
	id := ids.New()
	cc := at(rg.r.owner, "harbor", "")
	if r := mustf(cc.CancelWrite(ctx, "harbor", id))(t); r.State != api.WriteCancelled {
		t.Fatalf("cancel: %+v", r)
	}
}

// d, e. At the authority, with a replica's token: a cancelled id is a
// definitive 409 live and queued, never a run; a write that ran is "ran";
// tombstones keep out of the count cap and go by time only.
func TestCancelAtAuthority(t *testing.T) {
	quietPresence(t)
	tn := tailnettest.New()
	a := authority(t, tn)
	_, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	hc := tailnetClientAs(t, tn, "bea-mac")
	token := readToken(t, j.Remote.TokenFile)
	pid := j.Remote.Project.ID
	db := mustf(a.dm.Deps.Store.Project(authz.All(), authz.Read, pid))(t)
	base := "http://potluck-mini:7433"
	hdr := func(id string) map[string]string {
		h := map[string]string{api.HeaderProject: pid}
		if id != "" {
			h[api.HeaderWriteID] = id
		}
		return h
	}
	cancel := func(id string) api.WriteCancelResponse {
		t.Helper()
		st, body := rawCall(t, hc, "POST", base+"/v0/team/writes/"+id+"/cancel?project="+pid, token, struct{}{}, hdr(""))
		var r api.WriteCancelResponse
		if st != 200 || json.Unmarshal([]byte(body), &r) != nil {
			t.Fatalf("cancel: %d %s", st, body)
		}
		return r
	}
	live := func(id, key string) (int, string) {
		return rawCall(t, hc, "POST", base+"/v0/leases", token, orchapi.ClaimRequest{Key: key}, hdr(id))
	}
	tombs := func() int {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM team_writes WHERE req = 'x1:cancelled'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	held := func(key string) bool {
		for _, l := range mustf(orchclient.Leases(context.Background(), at(a.owner, "harbor", ""), orchapi.LeaseQuery{}))(t) {
			if l.Key == key {
				return true
			}
		}
		return false
	}

	// d. Cancelled, then delivered queued and live: 409, never run.
	id := ids.New()
	if r := cancel(id); r.State != api.WriteCancelled {
		t.Fatalf("cancel: %+v", r)
	}
	if r := cancel(id); r.State != api.WriteCancelled {
		t.Fatalf("cancel again: %+v", r)
	}
	st, body := rawCall(t, hc, "POST", base+"/v0/team/writes", token, api.WritesRequest{Project: pid, Writes: []api.QueuedWrite{
		{ID: id, Method: "POST", Path: "/v0/leases", Body: json.RawMessage(`{"key":"lock:d"}`)}}}, hdr(""))
	var out api.WritesResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil || st != 200 {
		t.Fatalf("drain: %d %s", st, body)
	}
	if out.Stopped != "" || len(out.Results) != 1 || out.Results[0].Status != 409 || !strings.Contains(string(out.Results[0].Body), "never ran") ||
		strings.Contains(string(out.Results[0].Body), "retained for review") {
		t.Fatalf("queued delivery of a cancelled id stuck or ran: %s", body)
	}
	if st, body := live(id, "lock:d"); st != 409 || !strings.Contains(body, "never ran") {
		t.Fatalf("live delivery: %d %s", st, body)
	}
	if held("lock:d") {
		t.Fatal("the cancelled write ran")
	}

	// A write that ran is "ran" with its status, never its body.
	ran := ids.New()
	if st, body := live(ran, "lock:ran"); st != 200 {
		t.Fatalf("live: %d %s", st, body)
	}
	if r := cancel(ran); r.State != api.WriteRan || r.Status != 200 {
		t.Fatalf("cancel after run: %+v", r)
	}
	if !held("lock:ran") {
		t.Fatal("cancel undid a write that ran")
	}

	// e. The count cap never takes a tombstone; time does.
	oldN, oldP, oldK := team.MaxLiveRecords, team.LivePruneEvery, team.CancelKeep
	team.MaxLiveRecords, team.LivePruneEvery = 1, 1
	t.Cleanup(func() { team.MaxLiveRecords, team.LivePruneEvery, team.CancelKeep = oldN, oldP, oldK })
	for i := 0; i < 4; i++ {
		if st, body := live(ids.New(), fmt.Sprintf("lock:cap%d", i)); st != 200 {
			t.Fatalf("live %d: %d %s", i, st, body)
		}
	}
	var l2 int
	_ = db.QueryRow(`SELECT count(*) FROM team_writes WHERE req LIKE 'l2:%'`).Scan(&l2)
	if l2 > 2 {
		t.Fatalf("count cap did not prune live records: %d", l2)
	}
	if n := tombs(); n != 1 {
		t.Fatalf("tombstones after the count prune: %d", n)
	}
	team.CancelKeep = 50 * time.Millisecond
	time.Sleep(120 * time.Millisecond)
	if st, body := live(ids.New(), "lock:after"); st != 200 {
		t.Fatalf("live: %d %s", st, body)
	}
	if n := tombs(); n != 0 {
		t.Fatalf("tombstones after CancelKeep: %d", n)
	}

	// An id older than the live records kept cannot be settled: a missing
	// record no longer proves it never ran.
	oldKeep, oldSkew := team.LiveWriteKeep, team.CancelSkew
	t.Cleanup(func() { team.LiveWriteKeep, team.CancelSkew = oldKeep, oldSkew })
	stale := ids.New()
	team.LiveWriteKeep, team.CancelSkew = 50*time.Millisecond, 0
	time.Sleep(120 * time.Millisecond)
	if st, body := rawCall(t, hc, "POST", base+"/v0/team/writes/"+stale+"/cancel?project="+pid, token, struct{}{}, hdr("")); st != 409 || !strings.Contains(body, "too old to settle") {
		t.Fatalf("stale cancel: %d %s", st, body)
	}
	if n := tombs(); n != 0 {
		t.Fatalf("a stale cancel left a tombstone: %d", n)
	}
}
