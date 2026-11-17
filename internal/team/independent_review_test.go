package team_test

import (
	"context"
	"encoding/json"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestIndependentPrivateOutboxHiddenFromAgent(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	agent := register(t, r, "harbor/solver", "review-agent")
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	waitReachable(t, r, false)
	const secret = "invented-private-outbox-canary"
	m := mustf(orchclient.Send(ctx, at(r.owner, "harbor", ""), orchapi.SendMailRequest{To: "human:mini-human", Body: secret, Audience: orchapi.MailAudienceParticipants, ClientRequestID: "review-private"}))(t)
	own := mustf(at(r.owner, "harbor", "").Outbox(ctx, "harbor", false))(t)
	if len(own) != 1 || own[0].ID != m.ID {
		t.Fatal("owner lost private queued send", own)
	}
	private := mustf(orchclient.Send(ctx, at(r.owner, "harbor", agent.Address), orchapi.SendMailRequest{Kind: orchapi.MailKindReport, To: "human:mini-human", Body: "invented private agent note", Audience: orchapi.MailAudienceParticipants, Needs: orchapi.NeedsHuman}))(t)
	ss := mustf(orchclient.Session(ctx, r.owner, agent.Address))(t)
	if strings.Contains(ss.Note, private.ID[len(private.ID)-6:]) {
		t.Fatal("private replica mail identifier in session state", ss.Note)
	}

	entries := mustf(at(r.owner, "harbor", agent.Address).Outbox(ctx, "harbor", false))(t)
	for _, e := range entries {
		if strings.Contains(string(e.Body), secret) {
			t.Fatalf("nonparticipant agent read private queued body; mail=%s entry=%s body=%s", m.ID, e.ID, e.Body)
		}
	}
}

func TestIndependentPrivateWriteMetadataHidden(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	m := mustf(orchclient.Send(ctx, at(a.owner, "harbor", ""), orchapi.SendMailRequest{To: "human:mini-human", Body: "invented authority-only private note", Audience: orchapi.MailAudienceParticipants}))(t)
	// This is a generated fixture token in a disposable test home.
	token := readToken(t, j.Remote.TokenFile)
	result := writes(t, tn, r, token, api.WritesRequest{Project: j.Remote.Project.ID, Writes: []api.QueuedWrite{{ID: ids.New(), Method: http.MethodPost, Path: "/v0/mail/" + m.ID + "/ack", Body: json.RawMessage(`{}`), Item: m.ID}}})
	if len(result.Results) != 1 {
		t.Fatal(result)
	}
	got := result.Results[0]
	if got.Status != 404 {
		t.Fatalf("expected hidden mail 404: %+v", got)
	}
	if got.Seen != "" {
		t.Fatalf("404 for private mail disclosed event cursor %s", got.Seen)
	}
	// A successful public operation also cannot use an unrelated Item to
	// obtain a hidden cursor. This defeats a success-only fix.
	// Group mail to bea's machine: mail to a person is private and read only by their Got it.
	public := mustf(orchclient.Send(ctx, at(a.owner, "harbor", ""), orchapi.SendMailRequest{To: "machine:bea-mac", Body: "invented public note"}))(t)
	id := ids.New()
	req := api.WritesRequest{Project: j.Remote.Project.ID, Writes: []api.QueuedWrite{{ID: id, Method: http.MethodPost, Path: "/v0/mail/" + public.ID + "/ack", Body: json.RawMessage(`{}`), Item: m.ID}}}
	for i := 0; i < 2; i++ {
		res := writes(t, tn, r, token, req)
		if len(res.Results) != 1 || res.Results[0].Status != 200 || res.Results[0].Seen != "" {
			t.Fatalf("successful/replayed unrelated Item leaked: %+v", res)
		}
	}

}

func TestIndependentReplicaReplyRetriesFailedParentAck(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	ac := at(a.owner, "harbor", "")
	// A private note to a replica session: its reply takes the parent (mail to a person never is).
	agent := register(t, r, "harbor/solver", "fixture-agent")
	rc := at(r.owner, "harbor", agent.Address)
	var parent orchapi.Mail
	eventually(t, 3*time.Second, func() error { // until the authority knows the replica's session
		var err error
		parent, err = orchclient.Send(ctx, ac, orchapi.SendMailRequest{To: agent.Address, Body: "invented parent", Audience: orchapi.MailAudienceParticipants, ClientRequestID: "review-parent"})
		return err
	})
	eventually(t, 3*time.Second, func() error { _, err := orchclient.GetMail(ctx, rc, parent.ID); return err })
	db, err := a.dm.Deps.Store.Project(authz.All(), authz.Read, j.Remote.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TRIGGER review_fail_ack BEFORE UPDATE OF acked_at ON mail BEGIN SELECT RAISE(ABORT,'synthetic ack failure'); END`); err != nil {
		t.Fatal(err)
	}
	rr := orchapi.ReplyRequest{Body: "invented reply", ClientRequestID: "retry-parent-ack"}
	// The decision seam now propagates the failed ack. Retry must repair it
	// with the original reply ID rather than claim successful completion.
	if _, err := orchclient.Reply(ctx, rc, parent.ID, rr); err == nil {
		t.Fatal("reply hid injected parent-ack failure")
	}
	var firstID string
	if err := db.QueryRow(`SELECT id FROM mail WHERE reply_to=?`, parent.ID).Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	if got := mustf(orchclient.GetMail(ctx, ac, parent.ID))(t); got.AckedAt != nil {
		t.Fatal("failure injection did not work")
	}
	if _, err = db.Exec(`DROP TRIGGER review_fail_ack`); err != nil {
		t.Fatal(err)
	}
	second := mustf(orchclient.Reply(ctx, rc, parent.ID, rr))(t)
	if firstID != second.ID {
		t.Fatal("duplicated reply", firstID, second.ID)
	}
	if got := mustf(orchclient.GetMail(ctx, ac, parent.ID))(t); got.AckedAt == nil {
		t.Fatal("same-key replica reply retry returned success but did not retry failed parent ack")
	}
}
