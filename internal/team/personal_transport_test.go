package team_test

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A member's OS-user name need not match the person it joined as.
// Both snapshot and live private mail must be listed as that joined person,
// including from the offline copy, without making the person's agents parties.
func TestPersonalMailListsUseJoinedIdentity(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ac := at(a.owner, "harbor", "")
	mustf(a.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "bea"}))(t)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Project: "harbor", Role: "member"}))(t)
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	send := func(body string) orchapi.Mail {
		t.Helper()
		return mustf(orchclient.Send(ctx, ac, orchapi.SendMailRequest{
			To: "human:bea", Kind: orchapi.MailKindReport, Body: body,
			Audience: orchapi.MailAudienceParticipants,
		}))(t)
	}
	before := send("invented report before join")
	r := startDaemonWith(t, tn, "bea-mac", tailnettest.Identity{
		NodeID: "nBEA", NodeKey: "nodekey:bea-mac", Login: "bea@example.com", Host: "bea-mac",
	}, a.dm.Deps.Log, func(c *config.Config) { c.Human = "a12066" })
	j := mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	if j.Remote == nil || j.Remote.Identity != "bea" {
		t.Fatalf("joined as %+v, want bea", j.Remote)
	}
	if who := mustf(r.sock.Whoami(ctx))(t); who.As != "human:a12066" {
		t.Fatalf("join changed local identity: %+v", who)
	}
	waitReachable(t, r, true)
	agent := register(t, r, "harbor/solver", "private-list-agent")
	after := send("invented report after join")
	query := orchapi.MailQuery{From: "human:mini-human", To: "human:bea", Limit: 1000}
	check := func() error {
		for _, project := range []string{"harbor", ""} {
			ms, err := orchclient.Mail(ctx, at(r.sock, project, ""), query)
			if err != nil {
				return err
			}
			if len(ms) != 2 || ms[0].ID != after.ID || ms[1].ID != before.ID {
				return fmt.Errorf("project %q: mail %+v, want both private reports", project, ms)
			}
			for _, m := range ms {
				if m.Audience != orchapi.MailAudienceParticipants || m.Recipient != "human:bea" {
					return fmt.Errorf("lost canonical audience: %+v", m)
				}
			}
			agentMail, err := orchclient.Mail(ctx, at(r.sock, project, agent.Address), query)
			if err != nil || len(agentMail) != 0 {
				return fmt.Errorf("project %q: agent read owner's reports: %+v (%v)", project, agentMail, err)
			}
		}
		return nil
	}
	eventually(t, 3*time.Second, check)
	// No project filter must also find the reports; the local project keeps
	// its own identity and does not acquire an alias for the remote person.
	if ms := mustf(orchclient.Mail(ctx, r.sock, orchapi.MailQuery{Limit: 1000}))(t); len(ms) != 2 {
		t.Fatalf("unfiltered combined list: %+v", ms)
	}
	if ms := mustf(orchclient.Mail(ctx, at(r.sock, api.ProjectNone, ""), query))(t); len(ms) != 0 {
		t.Fatalf("remote reports entered local project: %+v", ms)
	}
	if _, err := orchclient.Mail(ctx, at(r.sock, "harbor", "human:bea"), query); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("local socket allowed impersonating remote bea: %v", err)
	}
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	waitReachable(t, r, false)
	if err := check(); err != nil {
		t.Fatal("offline:", err)
	}
}

func TestNativePersonalConversationAuthorityReplicaRetry(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	agent := register(t, r, "harbor/solver", "fixture-agent")
	rc := at(r.owner, "harbor", "")
	ac := at(a.owner, "harbor", "")
	req := orchapi.SendMailRequest{To: "human:bea", Body: "invented personal conversation", Audience: orchapi.MailAudienceParticipants, ClientRequestID: "authority-request-1"}
	parent := mustf(orchclient.Send(ctx, ac, req))(t)
	eventually(t, 3*time.Second, func() error {
		m, err := orchclient.GetMail(ctx, rc, parent.ID)
		if err != nil {
			return err
		}
		if m.ID != parent.ID || m.Audience != "participants" {
			return fmt.Errorf("missing private mail: %+v", m)
		}
		return nil
	})
	for _, id := range []string{parent.ID, parent.ID[20:]} {
		if _, err := orchclient.GetMail(ctx, at(r.owner, "harbor", agent.Address), id); !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("replica agent saw owner's personal mail: %v", err)
		}
	}
	rr := orchapi.ReplyRequest{Body: "invented person answer", ClientRequestID: "replica-reply-1"}
	first := mustf(orchclient.Reply(ctx, rc, parent.ID, rr))(t)
	again := mustf(orchclient.Reply(ctx, rc, parent.ID, rr))(t)
	if first.ID != again.ID || first.From != "human:bea" || first.Recipient != "human:mini-human" || first.Audience != "participants" {
		t.Fatal(first, again)
	}
	rr.Body = "changed invented answer"
	_, err := orchclient.Reply(ctx, rc, parent.ID, rr)
	if !api.IsCode(err, api.CodeConflict) {
		t.Fatal("replica retry payload conflict missing", err)
	}
	thread := mustf(orchclient.Thread(ctx, ac, parent.ID))(t)
	if len(thread) != 2 {
		t.Fatal("duplicate transport sends", thread)
	}
	// Mail to a person is read only by their Got it (SPEC 9 "For you"), never a reply; bea's Got it
	// from the replica reaches the authority.
	if mustf(orchclient.GetMail(ctx, ac, parent.ID))(t).AckedAt != nil {
		t.Fatal("a reply marked bea's for-you item read")
	}
	if err := rc.Do(ctx, http.MethodPost, "/v0/mail/"+parent.ID+"/got-it", map[string]any{}, nil); err != nil {
		t.Fatal("bea's Got it from the replica:", err)
	}
	if got := mustf(orchclient.GetMail(ctx, ac, parent.ID))(t); got.AckedAt == nil || got.AckedBy != "human:bea" {
		t.Fatalf("Got it did not mark it read as bea: %+v", got)
	}
	// Mail to several people from the replica: one private copy each at the authority.
	mustf(a.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "cyd"}))(t)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "cyd", Project: "harbor", Role: "member"}))(t)
	g := mustf(orchclient.Send(ctx, rc, orchapi.SendMailRequest{To: "human:mini-human,human:cyd", Body: "invented note to both"}))(t)
	members := []string{"human:bea", "human:cyd", "human:mini-human"}
	if !slices.Equal(g.Members, members) || g.Batch != g.ID || g.Recipient != "human:cyd" {
		t.Fatalf("group answer %+v", g)
	}
	var mine []orchapi.Mail
	for _, m := range mustf(orchclient.Mail(ctx, ac, orchapi.MailQuery{From: "human:bea", Limit: 1000}))(t) {
		if m.Batch == g.ID {
			mine = append(mine, m)
		}
	}
	if len(mine) != 1 || mine[0].Recipient != "human:mini-human" || !slices.Equal(mine[0].Members, members) {
		t.Fatalf("the authority's owner reads %+v, want only its own copy", mine)
	}
}

// A phone paired to a member Mac lists its owner's for-you mail from a joined
// project, live from the authority, and its Got it marks it read there; a phone
// paired on the same Mac for another person never sees or takes it.
func TestMemberPhoneForYouFromJoinedProject(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	ac := at(a.owner, "harbor", "")
	mustf(r.owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "dana"}))(t)
	phone := func(node, host, login string) *client.Client {
		n := tailnetNode(t, tn, tailnettest.Identity{NodeID: node, NodeKey: "nodekey:" + host, Login: login, Host: host}, host)
		return client.NewRemote("http://potluck-bea-mac:7433", n.HTTPClient())
	}
	beaPhone, danaPhone := phone("nCPHONE", "bea-phone", "bea@example.com"), phone("nDPHONE", "dana-phone", "dana@example.com")
	mustf(orchclient.Pair(ctx, beaPhone, mustf(orchclient.AddApprover(ctx, r.owner, "bea-phone"))(t).Code))(t)
	if ap := mustf(orchclient.Pair(ctx, danaPhone, mustf(orchclient.AddApproverFor(ctx, r.owner, "dana-phone", "dana"))(t).Code))(t); ap.State != orchapi.ApproverActive {
		mustf(orchclient.ConfirmApprover(ctx, beaPhone, "dana-phone"))(t) // an active approver confirms the next
	}
	fyi := mustf(orchclient.Send(ctx, ac, orchapi.SendMailRequest{To: "human:bea", Body: "invented FYI for bea"}))(t)
	mustf(orchclient.Send(ctx, ac, orchapi.SendMailRequest{To: "human:bea", Body: "invented digest for bea", Digest: true}))(t)
	v := mustf(orchclient.DeviceView(ctx, beaPhone))(t)
	if len(v.ForYou) != 1 || v.ForYou[0].ID != fyi.ID || len(v.Skipped) != 0 {
		t.Fatalf("bea's phone lists %+v (skipped %+v), want only the FYI", v.ForYou, v.Skipped)
	}
	if v := mustf(orchclient.DeviceView(ctx, danaPhone))(t); len(v.ForYou) != 0 {
		t.Fatalf("dana's phone lists bea's mail: %+v", v.ForYou)
	}
	gotIt := func(c *client.Client) error {
		return c.Do(ctx, http.MethodPost, "/v0/approvals/got-it", orchapi.GotItRequest{ID: fyi.ID}, nil)
	}
	if err := gotIt(danaPhone); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("dana's phone took bea's FYI: %v", err)
	}
	if mustf(orchclient.GetMail(ctx, ac, fyi.ID))(t).AckedAt != nil {
		t.Fatal("dana's Got it marked bea's FYI read")
	}
	if err := gotIt(beaPhone); err != nil {
		t.Fatal("bea's Got it from the member's phone:", err)
	}
	if got := mustf(orchclient.GetMail(ctx, ac, fyi.ID))(t); got.AckedAt == nil || got.AckedBy != "human:bea" {
		t.Fatalf("Got it did not mark it read at the authority as bea: %+v", got)
	}
	if v := mustf(orchclient.DeviceView(ctx, beaPhone))(t); len(v.ForYou) != 0 {
		t.Fatalf("after Got it bea's phone still lists %+v", v.ForYou)
	}
}
