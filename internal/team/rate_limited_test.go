package team_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// SPEC 9 "Send limits" on a team replica: a send limit's refusal (429
// rate_limited) is for now, not for good. The authority never records it
// under the write id (live or queued, and the queued batch goes on past
// it), and the replica frees the client_request_id, so a retry with the
// same key after retry_after is sent, and a retry after that replays it.
func TestReplicaRateLimitedKeyedRetry(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-sender")
	tgt := register(t, r, "harbor/solver", "bea-target")
	heartbeat(t, r)
	waitReachable(t, r, true)
	db := mustf(a.dm.Deps.Store.Project(authz.All(), authz.Read, j.Remote.Project.ID))(t)
	send := func(key, body string) (orchapi.Mail, error) {
		var m orchapi.Mail
		err := at(r.owner, "harbor", cs.Address).Do(ctx, http.MethodPost, "/v0/mail",
			map[string]any{"to": tgt.Address, "body": body, "client_request_id": key}, &m)
		return m, err
	}
	limited := func(err error) {
		t.Helper()
		var ae *api.Error
		if !errors.As(err, &ae) || ae.Code != api.CodeRateLimited || ae.RetryAfter < 1 {
			t.Fatalf("want 429 rate_limited with retry_after, got %v (%+v)", err, ae)
		}
	}
	unrecorded := func() {
		t.Helper()
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM team_writes WHERE status = 429`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("the authority recorded %d rate_limited answers (%v)", n, err)
		}
	}
	// retry_after passes: the ledger's sends leave every window.
	waitOut := func() {
		t.Helper()
		if _, err := db.Exec(`UPDATE mail_sends SET at = at - ?`, (24 * time.Hour).Milliseconds()); err != nil {
			t.Fatal(err)
		}
	}

	first := mustf(send("k-1", "the same words"))(t)
	// The same mail again (the repeat rule), under a new key: refused.
	_, err := send("k-2", "the same words")
	limited(err)
	unrecorded()
	waitOut()
	again := mustf(send("k-2", "the same words"))(t)
	if again.ID == "" || again.ID == first.ID {
		t.Fatalf("the keyed retry after retry_after was not sent: %+v (first %s)", again, first.ID)
	}
	if replay := mustf(send("k-2", "the same words"))(t); replay.ID != again.ID {
		t.Fatalf("the key no longer replays its send: %s, want %s", replay.ID, again.ID)
	}

	// Queued: a refused mail ahead of another in one batch. The batch goes
	// on (the second lands), and the refused key is free again.
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	waitReachable(t, r, false)
	mustf(send("q-1", "the same words"))(t)
	mustf(send("q-2", "something new"))(t)
	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	waitDrained(t, r)
	eventually(t, 5*time.Second, func() error {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM mail WHERE body = 'something new'`).Scan(&n); err != nil || n != 1 {
			return fmt.Errorf("the write behind the refused one: %d (%v)", n, err)
		}
		return nil
	})
	es := mustf(r.owner.Outbox(ctx, "", false))(t)
	failed := 0
	for _, e := range es {
		if e.State == api.OutboxFailed {
			failed++
		}
	}
	if failed != 1 {
		t.Fatalf("want the refused queued mail failed in the outbox: %+v", es)
	}
	unrecorded()
	waitOut()
	queued := mustf(send("q-1", "the same words"))(t)
	if queued.ID == "" || queued.ID == again.ID {
		t.Fatalf("the queued key's retry after retry_after was not sent: %+v", queued)
	}
}

// The live half of the same rule: a write forwarded with a write id and
// refused rate_limited is answered, not recorded, so the same write id sent
// again after retry_after runs, and only then replays.
func TestLiveWriteRateLimitedNotRecorded(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	a, _ := tunnelHome(t, nil, "mini", tailnettest.Identity{})
	teamHarbor(t, a)
	fp := mustf(tunnelOf(t, a).Fingerprint())(t)
	r, j := tunnelMember(t, a, "bea-mac", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-sender")
	tgt := register(t, a, "harbor/solver", "mini-target")
	hc, tok := signedClient(t, r, fp), readToken(t, j.Remote.TokenFile)
	db := mustf(a.dm.Deps.Store.Project(authz.All(), authz.Read, j.Remote.Project.ID))(t)
	call := func(wid string) (int, string, http.Header) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"to": tgt.Address, "body": "the same words"})
		req, _ := http.NewRequest(http.MethodPost, a.cfg.File.Tunnel.URL+"/v0/mail", bytes.NewReader(b))
		req.Header.Set(api.HeaderProtocol, api.Protocol)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(api.HeaderAuthorization, "Bearer "+tok)
		req.Header.Set(api.HeaderProject, j.Remote.Project.ID)
		req.Header.Set(api.HeaderWriteID, wid)
		api.SetHeader(req.Header, api.HeaderAs, cs.Address)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(out), resp.Header
	}
	rows := func(where string) int {
		t.Helper()
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM team_writes WHERE `+where).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if st, b, _ := call(ids.New()); st != 200 {
		t.Fatalf("first send: %d %s", st, b)
	}
	wid := ids.New()
	if st, b, _ := call(wid); st != http.StatusTooManyRequests || !strings.Contains(b, "rate_limited") {
		t.Fatalf("repeat send: %d %s", st, b)
	}
	if n := rows(`status = 429`); n != 0 {
		t.Fatalf("the authority recorded %d rate_limited answers", n)
	}
	if _, err := db.Exec(`UPDATE mail_sends SET at = at - ?`, (24 * time.Hour).Milliseconds()); err != nil {
		t.Fatal(err)
	}
	st, b, _ := call(wid)
	if st != 200 {
		t.Fatalf("the same write id after retry_after: %d %s", st, b)
	}
	if st2, b2, h := call(wid); st2 != 200 || b2 != b || h.Get(api.HeaderWriteReplayed) != "1" {
		t.Fatalf("the send does not replay: %d %s %v", st2, b2, h)
	}
}
