package team_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// queuedClaimOracle observes only this brick's canonical write receipts.
// The public outbox's Body is the original request, not a claim result;
// disappearance from that outbox is not evidence of a successful claim.
type queuedClaimOracle struct {
	db                *sql.DB
	identity, project string
}

func newQueuedClaimOracle(t *testing.T, a *daemon, joined api.JoinResponse) queuedClaimOracle {
	t.Helper()
	if joined.Remote == nil {
		t.Fatal("claim oracle needs a joined project")
	}
	ctx := context.Background()
	identity := mustf(a.dm.Deps.Projects.Identity(ctx, joined.Remote.Identity))(t)
	db := mustf(a.dm.Deps.Store.Project(authz.All(), authz.Read, joined.Remote.Project.ID))(t)
	return queuedClaimOracle{db, identity.ID, joined.Remote.Project.ID}
}

func (o queuedClaimOracle) ticket(item string, queued error) (api.QueuedResponse, error) {
	var detail *api.Error
	var q api.QueuedResponse
	if !errors.As(queued, &detail) || detail.Code != api.CodeQueued || json.Unmarshal(detail.Data, &q) != nil ||
		!q.Queued || !ids.Valid(q.Write) || q.Project != o.project || q.Item != item {
		return q, fmt.Errorf("invalid queued claim receipt for %s: %v", item, queued)
	}
	return q, nil
}

func (o queuedClaimOracle) resolve(ctx context.Context, item string, queued error) (orchapi.ClaimResponse, error) {
	var result orchapi.ClaimResponse
	q, err := o.ticket(item, queued)
	if err != nil {
		return result, err
	}
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var status int
		var body string
		err := o.db.QueryRowContext(ctx, `SELECT status, body FROM team_writes WHERE identity=? AND id=?`, o.identity, q.Write).Scan(&status, &body)
		switch {
		case err == nil:
			switch status {
			case http.StatusOK:
				if err := json.Unmarshal([]byte(body), &result); err != nil || result.Lease.Project != q.Project || result.Lease.Key != item || result.Lease.Holder == "" {
					return result, fmt.Errorf("queued claim %s has invalid successful result: %q (%v)", q.Write, body, err)
				}
				return result, nil
			case http.StatusConflict:
				var response api.ErrorResponse
				if err := json.Unmarshal([]byte(body), &response); err != nil || response.Error == nil || response.Error.Code != api.CodeConflict {
					return result, fmt.Errorf("queued claim %s has invalid conflict: %q (%v)", q.Write, body, err)
				}
				response.Error.HTTPStatus = status
				return result, response.Error
			default:
				return result, fmt.Errorf("queued claim %s settled with unexpected status %d: %s", q.Write, status, body)
			}
		case !errors.Is(err, sql.ErrNoRows):
			return result, fmt.Errorf("queued claim %s receipt: %w", q.Write, err)
		}
		select {
		case <-ctx.Done():
			return result, fmt.Errorf("queued claim %s has no terminal receipt: %w", q.Write, ctx.Err())
		case <-tick.C:
		}
	}
}

// Force both sides of authority arbitration to queue before reconnecting.
// Reading their original receipts must not submit or renew another claim.
func TestQueuedClaimOracleWinnerAndLoser(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	winner := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "queued winner", Area: "solver"}))(t)
	loser := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "queued loser", Area: "solver"}))(t)
	r, joined := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	oracle := newQueuedClaimOracle(t, a, joined)
	holder, keeper := "bea-mac/00000001", "mini/00000001"
	rc := at(r.owner, "harbor", holder)
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	t.Cleanup(func() { tn.Cut("potluck-bea-mac", "potluck-mini", false) })
	waitReachable(t, r, false)
	type pending struct {
		item string
		err  error
		q    api.QueuedResponse
	}
	var claims []pending
	for _, item := range []string{winner.ID, loser.ID} {
		_, err := orchclient.Claim(ctx, rc, item, orchapi.ClaimRequest{TTLSeconds: 600})
		q, decodeErr := oracle.ticket(item, err)
		if decodeErr != nil {
			t.Fatal(decodeErr)
		}
		var n int
		if err := oracle.db.QueryRowContext(ctx, `SELECT count(*) FROM team_writes WHERE identity=? AND id=?`, oracle.identity, q.Write).Scan(&n); err != nil || n != 0 {
			t.Fatalf("queued write executed before reconnect: %s count=%d err=%v", q.Write, n, err)
		}
		claims = append(claims, pending{item, err, q})
	}
	if claims[0].q.Write == claims[1].q.Write {
		t.Fatal("different claim operations reused a write ID")
	}
	entries := mustf(r.owner.Outbox(ctx, "harbor", true))(t)
	if len(entries) != len(claims) {
		t.Fatalf("original outbox entries: %+v", entries)
	}
	for _, claim := range claims {
		found := false
		for _, entry := range entries {
			if entry.ID != claim.q.Write {
				continue
			}
			var request orchapi.ClaimRequest
			if entry.Item != claim.item || entry.As != holder || entry.Kind != "claim" || entry.State != api.OutboxPending ||
				json.Unmarshal(entry.Body, &request) != nil || request.TTLSeconds != 600 {
				t.Fatalf("queued original request changed: %+v", entry)
			}
			found = true
		}
		if !found {
			t.Fatalf("original write ID %s absent from outbox", claim.q.Write)
		}
	}
	mustf(orchclient.Claim(ctx, at(a.owner, "harbor", keeper), loser.ID, orchapi.ClaimRequest{TTLSeconds: 600}))(t)
	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	settleCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, claim := range claims {
		res, err := oracle.resolve(settleCtx, claim.item, claim.err)
		wantStatus := http.StatusOK
		if claim.item == winner.ID {
			if err != nil || res.Lease.Holder != holder {
				t.Fatalf("queued winner: %+v %v", res, err)
			}
		} else {
			wantStatus = http.StatusConflict
			var detail *api.Error
			var lease orchapi.Lease
			// The keeper's session belongs to another identity: the replica's
			// caller sees its address redacted (core.Redact), never a guess.
			if !errors.As(err, &detail) || detail.Code != api.CodeConflict || detail.HTTPStatus != wantStatus ||
				json.Unmarshal(detail.Data, &lease) != nil || lease.Key != loser.ID || lease.Holder != "someone" {
				t.Fatalf("queued loser did not retain canonical conflict: %+v %v", lease, err)
			}
		}
		var status int
		var body string
		if err := oracle.db.QueryRowContext(ctx, `SELECT status, body FROM team_writes WHERE identity=? AND id=?`, oracle.identity, claim.q.Write).Scan(&status, &body); err != nil || status != wantStatus {
			t.Fatalf("original terminal receipt %s: %d %q %v", claim.q.Write, status, body, err)
		}
		if status == http.StatusOK {
			var original orchapi.ClaimResponse
			if json.Unmarshal([]byte(body), &original) != nil || !reflect.DeepEqual(res, original) {
				t.Fatalf("winner did not preserve original response body: %+v vs %s", res, body)
			}
		} else {
			var original api.ErrorResponse
			var detail *api.Error
			if json.Unmarshal([]byte(body), &original) != nil || original.Error == nil || !errors.As(err, &detail) {
				t.Fatalf("invalid original conflict body: %s %v", body, err)
			}
			original.Error.HTTPStatus = status
			if !reflect.DeepEqual(detail, original.Error) {
				t.Fatalf("loser did not preserve original response body: %+v vs %s", detail, body)
			}
		}
		// Re-reading the terminal receipt is still observation, not a second
		// claim (which could renew a winner or create another write ID).
		again, againErr := oracle.resolve(settleCtx, claim.item, claim.err)
		if claim.item == winner.ID {
			if againErr != nil || again.Lease.Claimed != res.Lease.Claimed || again.Lease.Renewed != res.Lease.Renewed {
				t.Fatalf("receipt observation changed winner: %+v %v", again, againErr)
			}
		} else if !api.IsCode(againErr, api.CodeConflict) {
			t.Fatalf("receipt observation changed loser: %v", againErr)
		}
	}
	waitDrained(t, r)
	var count int
	if err := oracle.db.QueryRowContext(ctx, `SELECT count(*) FROM team_writes WHERE identity=?`, oracle.identity).Scan(&count); err != nil || count != len(claims) {
		t.Fatalf("logical claims executed more than once: receipts=%d want=%d err=%v", count, len(claims), err)
	}
	claimEvents := map[string]int{}
	for _, event := range projectEvents(t, a) {
		if event.Type == orchapi.EventWorkClaimed {
			claimEvents[event.Subject]++
		}
	}
	for _, item := range []string{winner.ID, loser.ID} {
		if claimEvents[item] != 1 {
			t.Errorf("item %s has %d claim executions, want 1", item, claimEvents[item])
		}
	}
	leases := mustf(orchclient.Leases(ctx, ao, orchapi.LeaseQuery{Project: "harbor"}))(t)
	if len(leases) != 2 {
		t.Fatalf("authority leases: %+v", leases)
	}
	for _, lease := range leases {
		want := holder
		if lease.Key == loser.ID {
			want = keeper
		}
		if lease.Holder != want {
			t.Errorf("lease %s holder=%s want=%s", lease.Key, lease.Holder, want)
		}
	}
}
