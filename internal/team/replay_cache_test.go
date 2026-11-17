package team_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// Exercise both entry points against the same journal, including responses
// without a Mail representation. All fixtures use disposable fake transports.
func TestReplayCacheAuthorizationAndInterop(t *testing.T) {
	for _, firstMode := range []string{"queued", "live"} {
		t.Run(firstMode, func(t *testing.T) {
			quietPresence(t)
			ctx := context.Background()
			tn := tailnettest.New()
			a := authority(t, tn)
			_, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
			hc := tailnetClientAs(t, tn, "bea-mac")
			token := readToken(t, j.Remote.TokenFile)
			pid := j.Remote.Project.ID
			db := mustf(a.dm.Deps.Store.Project(authz.All(), authz.Read, pid))(t)
			call := func(mode string, w api.QueuedWrite, capable bool) (int, string) {
				t.Helper()
				hdr := map[string]string{api.HeaderProject: pid}
				if capable {
					hdr[api.HeaderPersonalMail] = api.PersonalMailProtocol
				}
				if mode == "live" {
					hdr[api.HeaderWriteID], hdr[api.HeaderAs], hdr[api.HeaderBase] = w.ID, w.As, w.Base
					return rawCall(t, hc, w.Method, "http://potluck-mini:7433"+w.Path, token, w.Body, hdr)
				}
				status, body := rawCall(t, hc, "POST", "http://potluck-mini:7433/v0/team/writes", token, api.WritesRequest{Project: pid, Writes: []api.QueuedWrite{w}}, hdr)
				var out api.WritesResponse
				if json.Unmarshal([]byte(body), &out) != nil {
					t.Fatal(body)
				}
				if len(out.Results) == 1 {
					return out.Results[0].Status, string(out.Results[0].Body)
				}
				if out.Stopped == "" {
					t.Fatalf("missing result or hold: %d %s", status, body)
				}
				// A stopped batch cannot settle/delete this write on the replica.
				return http.StatusServiceUnavailable, body
			}
			otherMode := "live"
			if firstMode == "live" {
				otherMode = "queued"
			}
			send := api.QueuedWrite{ID: ids.New(), Method: "POST", Path: "/v0/mail", Body: json.RawMessage(`{"to":"human:mini-human","area":"billing","body":"cache-secret-canary","audience":"participants"}`)}
			st, body := call(firstMode, send, true)
			if st/100 != 2 {
				t.Fatalf("initial send: %d %s", st, body)
			}
			var mail orchapi.Mail
			if json.Unmarshal([]byte(body), &mail) != nil || mail.ID == "" {
				t.Fatal(body)
			}
			assertReplay := func(w api.QueuedWrite, expected string) {
				t.Helper()
				for _, mode := range []string{firstMode, otherMode} {
					status, got := call(mode, w, true)
					if status/100 != 2 || strings.TrimSpace(got) != strings.TrimSpace(expected) {
						t.Fatalf("%s valid replay: %d %s want %s", mode, status, got, expected)
					}
				}
			}
			assertHidden := func(w api.QueuedWrite, capable bool, secrets ...string) {
				t.Helper()
				for _, mode := range []string{firstMode, otherMode} {
					status, got := call(mode, w, capable)
					if status/100 == 2 {
						t.Fatalf("%s accepted changed evidence: %d %s", mode, status, got)
					}
					for _, secret := range secrets {
						if secret != "" && strings.Contains(got, secret) {
							t.Errorf("%s disclosed %q: %s", mode, secret, got)
						}
					}
				}
			}
			assertReplay(send, body)
			equivalentActor := send
			equivalentActor.As = mail.From
			assertReplay(equivalentActor, body) // authenticated address, not raw Zm-As
			var evidence string
			if err := db.QueryRow(`SELECT req FROM team_writes WHERE id=?`, send.ID).Scan(&evidence); err != nil || !strings.HasPrefix(evidence, "q2:") {
				t.Fatal("queued retry did not adopt durable retention", evidence, err)
			}
			secrets := []string{mail.ID, mail.Body, mail.From, mail.To, "participants", "billing"}
			for _, actor := range []string{"bea-mac/1234abcd", "other-mac/1234abcd"} {
				changed := send
				changed.As = actor
				// Failed authentication may name the token's own human in its
				// ordinary error; it must not expose the cached private mail.
				if actor == "other-mac/1234abcd" {
					assertHidden(changed, true, mail.ID, mail.Body, mail.To, "participants", "billing")
				} else {
					assertHidden(changed, true, secrets...)
				}
			}
			assertHidden(send, false, secrets...)
			changed := send
			changed.Body = json.RawMessage(`{"to":"human:mini-human","body":"different"}`)
			assertHidden(changed, true, secrets...)
			changed = send
			changed.Path += "/" + mail.ID + "/reply"
			assertHidden(changed, true, secrets...)
			changed = send
			changed.Base = ids.New()
			assertHidden(changed, true, secrets...)
			// Deleting the original mail cannot destroy the server-owned evidence.
			if _, err := db.Exec(`DELETE FROM mail WHERE id=?`, mail.ID); err != nil {
				t.Fatal(err)
			}
			assertReplay(send, body)
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM mail WHERE id=?`, mail.ID).Scan(&count); err != nil || count != 0 {
				t.Fatal("replay redispatched deleted mail", count, err)
			}

			parent := mustf(orchclient.Send(ctx, at(a.owner, "harbor", ""), orchapi.SendMailRequest{To: "human:bea", Body: "private parent", Audience: orchapi.MailAudienceParticipants}))(t)
			// Mail to a person is never acked (only their Got it reads it, SPEC 9 "For you"): the
			// private write replayed here is bea's reply to it.
			ack := api.QueuedWrite{ID: ids.New(), Method: "POST", Path: "/v0/mail/" + parent.ID + "/reply", Body: json.RawMessage(`{"body":"private answer"}`)}
			status, ackBody := call(firstMode, ack, true)
			if status/100 != 2 || !strings.Contains(ackBody, parent.ID) {
				t.Fatalf("ack: %d %s", status, ackBody)
			}
			assertReplay(ack, ackBody)
			if _, err := db.Exec(`DELETE FROM mail WHERE id=?`, parent.ID); err != nil {
				t.Fatal(err)
			}
			assertReplay(ack, ackBody)
			changed = ack
			changed.As = "bea-mac/1234abcd"
			assertHidden(changed, true, parent.ID, "human:bea")
			assertHidden(ack, false, parent.ID, "human:bea")

			// An error body is opaque too: never infer authorization from its shape.
			errorBody := `{"error":{"code":"conflict","message":"private-error-metadata ` + parent.ID + ` human:bea"}}`
			if _, err := db.Exec(`UPDATE team_writes SET status=409,body=? WHERE id=?`, errorBody, ack.ID); err != nil {
				t.Fatal(err)
			}
			for _, mode := range []string{firstMode, otherMode} {
				status, got := call(mode, ack, true)
				if status != 409 || strings.TrimSpace(got) != errorBody {
					t.Fatalf("same actor error replay: %d %s", status, got)
				}
			}
			changed = ack
			changed.As = "bea-mac/1234abcd"
			assertHidden(changed, true, parent.ID, "private-error-metadata", "human:bea")
			assertHidden(ack, false, parent.ID, "private-error-metadata", "human:bea")

			// Mixed concurrent deliveries of an operation without its own mail-ID
			// dedupe must still execute once, using the shared journal lock.
			mixed := api.QueuedWrite{ID: ids.New(), Method: "POST", Path: "/v0/work", Body: json.RawMessage(`{"title":"mixed retry once","area":"billing"}`)}
			type answer struct {
				status int
				body   string
			}
			answers := make(chan answer, 2)
			start := make(chan struct{})
			for _, mode := range []string{firstMode, otherMode} {
				go func(mode string) {
					<-start
					status, got := call(mode, mixed, true)
					answers <- answer{status, got}
				}(mode)
			}
			close(start)
			var firstAnswer string
			for range 2 {
				select {
				case result := <-answers:
					if result.status/100 != 2 {
						t.Fatalf("mixed delivery: %d %s", result.status, result.body)
					}
					if firstAnswer != "" && strings.TrimSpace(result.body) != firstAnswer {
						t.Fatal("mixed retry changed original result", result.body, firstAnswer)
					}
					firstAnswer = strings.TrimSpace(result.body)
				case <-time.After(5 * time.Second):
					t.Fatal("mixed retry deadlocked")
				}
			}
			work := mustf(orchclient.WorkList(ctx, at(a.owner, "harbor", ""), orchapi.WorkQuery{}))(t)
			n := 0
			for _, item := range work.Items {
				if item.Title == "mixed retry once" {
					n++
				}
			}
			if n != 1 {
				t.Fatal("mixed retry executed more than once", n)
			}

			// Both generations of legacy rows must remain held, even if ancient.
			for _, oldReq := range []string{"", "POST /v0/mail legacy-payload-hash"} {
				if _, err := db.Exec(`UPDATE team_writes SET req=?,time=0 WHERE id=?`, oldReq, send.ID); err != nil {
					t.Fatal(err)
				}
				assertHidden(send, true, secrets...)
				var retained string
				if err := db.QueryRow(`SELECT body FROM team_writes WHERE id=?`, send.ID).Scan(&retained); err != nil || retained == "" {
					t.Fatal("legacy evidence removed", err)
				}
			}
			// Restore the ack proof to check current scope independently of legacy rows.
			mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Project: "harbor", Area: "solver", Role: "guest"}))(t)
			mustf(a.owner.Revoke(ctx, api.GrantRequest{Identity: "bea", Project: "harbor"}))(t)
			assertHidden(ack, true, parent.ID, "private-error-metadata", "human:bea")
		})
	}
}

func TestReplayCacheLegacyKeepsOutboxPending(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	waitReachable(t, r, false)
	m := mustf(orchclient.Send(ctx, at(r.owner, "harbor", ""), orchapi.SendMailRequest{To: "human:mini-human", Body: "pending secret", Audience: orchapi.MailAudienceParticipants, ClientRequestID: "legacy-held"}))(t)
	token := readToken(t, j.Remote.TokenFile)
	subject := mustf(a.dm.Deps.Projects.FromTokenAt(ctx, token, true, "nBEA"))(t)
	db := mustf(a.dm.Deps.Store.Project(authz.All(), authz.Read, j.Remote.Project.ID))(t)
	if _, err := db.Exec(`INSERT INTO team_writes(identity,id,status,body,time,req) VALUES (?,?,200,?,0,'')`, subject.Identity.ID, m.ID, `{"id":"private-legacy-result","body":"legacy secret"}`); err != nil {
		t.Fatal(err)
	}
	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	eventually(t, 5*time.Second, func() error {
		st := remoteState(t, r)
		if st.Pending != 1 || !strings.Contains(st.LastError, "retained for review") {
			return fmt.Errorf("want held pending write: %+v", st)
		}
		return nil
	})
	entries := mustf(at(r.owner, "harbor", "").Outbox(ctx, "harbor", true))(t)
	if len(entries) != 1 || entries[0].ID != m.ID || entries[0].State != api.OutboxPending {
		t.Fatalf("ambiguous result settled pending write: %+v", entries)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM mail WHERE id=?`, m.ID).Scan(&count); err != nil || count != 0 {
		t.Fatal("legacy replay automatically resent mail", count, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM team_writes WHERE id=?`, m.ID).Scan(&count); err != nil || count != 1 {
		t.Fatal("pruning discarded legacy evidence", count, err)
	}
}

// Rename step 1: a live write's Zm-* headers are read under their Potluck-*
// names too (the new first, as kit reads them), so a write recorded under one
// set of names replays under the other, and the base in its replay evidence
// is the one kit stores. A queued batch's own Zm-As (either name) never
// replaces the write's.
func TestReplayCacheHeaderNames(t *testing.T) {
	quietPresence(t)
	tn := tailnettest.New()
	a := authority(t, tn)
	_, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	hc := tailnetClientAs(t, tn, "bea-mac")
	token := readToken(t, j.Remote.TokenFile)
	pid := j.Remote.Project.ID
	nw := api.HeaderNew
	live := func(w api.QueuedWrite, hdr map[string]string) (int, string) {
		t.Helper()
		hdr[api.HeaderPersonalMail] = api.PersonalMailProtocol
		return rawCall(t, hc, w.Method, "http://potluck-mini:7433"+w.Path, token, w.Body, hdr)
	}
	base, other := ids.New(), ids.New()
	send := api.QueuedWrite{ID: ids.New(), Method: "POST", Path: "/v0/mail", Body: json.RawMessage(`{"to":"human:mini-human","body":"renamed headers"}`)}
	st, body := live(send, map[string]string{nw(api.HeaderProject): pid, nw(api.HeaderWriteID): send.ID, nw(api.HeaderBase): base})
	if st/100 != 2 || !strings.Contains(body, "renamed headers") {
		t.Fatalf("live write under the new names: %d %s", st, body)
	}
	for name, hdr := range map[string]map[string]string{
		"old names":       {api.HeaderProject: pid, api.HeaderWriteID: send.ID, api.HeaderBase: base},
		"new base first":  {api.HeaderProject: pid, api.HeaderWriteID: send.ID, api.HeaderBase: other, nw(api.HeaderBase): base},
		"empty new base":  {api.HeaderProject: pid, api.HeaderWriteID: send.ID, api.HeaderBase: base, nw(api.HeaderBase): ""},
		"mixed new names": {nw(api.HeaderProject): pid, api.HeaderWriteID: send.ID, nw(api.HeaderBase): base},
	} {
		if st2, b2 := live(send, hdr); st2 != st || b2 != body {
			t.Errorf("%s: replay %d %s, want %d %s", name, st2, b2, st, body)
		}
	}
	if st2, b2 := live(send, map[string]string{api.HeaderProject: pid, api.HeaderWriteID: send.ID, api.HeaderBase: base, nw(api.HeaderBase): other}); st2/100 == 2 {
		t.Errorf("a changed base under the new name replayed: %d %s", st2, b2)
	}

	// Queued: the batch carries a Potluck-As of its own; the write (no As) is
	// recorded as the token's default address, so a live retry of it replays.
	queued := api.QueuedWrite{ID: ids.New(), Method: "POST", Path: "/v0/mail", Body: json.RawMessage(`{"to":"human:mini-human","body":"queued, renamed"}`)}
	qs, qb := rawCall(t, hc, "POST", "http://potluck-mini:7433/v0/team/writes", token, api.WritesRequest{Project: pid, Writes: []api.QueuedWrite{queued}},
		map[string]string{api.HeaderProject: pid, api.HeaderPersonalMail: api.PersonalMailProtocol, nw(api.HeaderAs): "bea-mac/1234abcd"})
	var out api.WritesResponse
	if qs/100 != 2 || json.Unmarshal([]byte(qb), &out) != nil || len(out.Results) != 1 || out.Results[0].Status/100 != 2 {
		t.Fatalf("queued write: %d %s", qs, qb)
	}
	if st2, b2 := live(queued, map[string]string{api.HeaderProject: pid, api.HeaderWriteID: queued.ID}); st2/100 != 2 || strings.TrimSpace(b2) != strings.TrimSpace(string(out.Results[0].Body)) {
		t.Errorf("live retry of the queued write: %d %s, want %s", st2, b2, out.Results[0].Body)
	}
}
