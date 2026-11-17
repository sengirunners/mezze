package mail

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// member mints a member token for name in project side and answers its client.
func (f *fixture) member(name string) *client.Client {
	f.t.Helper()
	tok, err := f.d.Projects.Mint(ctx, f.d.Projects.Local(""), api.MintTokenRequest{As: name, Project: "side", Role: "member"})
	if err != nil {
		f.t.Fatal(err)
	}
	c := client.WithHandler(f.h)
	c.Token = tok.Secret
	return c
}

func gotIt(c *client.Client, id string) error {
	return c.Do(ctx, http.MethodPost, "/v0/mail/"+id+"/got-it", map[string]any{}, nil)
}

// SPEC 9 "Mail to several people": one private two-party copy per person,
// all checked before any is stored; a reply reaches every other member.
func TestMailToSeveralPeople(t *testing.T) {
	f := newFixture(t)
	bea, cyd, dana := f.member("bea"), f.member("cyd"), f.member("dana")
	ana := f.as("", "side")
	f.sess.add("mini/aaaaaaaa", f.side.ID, "", true, orchapi.StateRunning)

	// all or nothing: one bad recipient refuses the whole send
	for _, c := range []struct {
		from *client.Client
		to   string
		code string
	}{
		{ana, "human:bea,human:nobody", api.CodeNotFound},
		{ana, "human:bea,owners", api.CodeBadRequest},
		{ana, "human:bea,mini/aaaaaaaa", api.CodeBadRequest},
		{ana, "human:bea,human:bea,human:ana", api.CodeBadRequest}, // one person besides the sender
		{ana, "human:a,human:b,human:c,human:d,human:e,human:f,human:g,human:h,human:i", api.CodeBadRequest},
		{f.as("mini/aaaaaaaa", "side"), "human:bea,human:cyd", api.CodeBadRequest}, // people only send it
	} {
		_, err := orchclient.Send(ctx, c.from, orchapi.SendMailRequest{To: c.to, Body: "x"})
		wantCode(t, err, c.code)
	}
	if all := must(orchclient.Mail(ctx, f.as("", ""), orchapi.MailQuery{})); len(all) != 0 {
		t.Fatalf("a refused send stored mail: %+v", all)
	}

	req := orchapi.SendMailRequest{To: "human:cyd, human:bea,human:cyd,human:ana", Body: "lunch?", ClientRequestID: "k1"}
	m := must(orchclient.Send(ctx, ana, req))
	members := []string{"human:ana", "human:bea", "human:cyd"}
	if !slices.Equal(m.Members, members) || m.Batch != m.ID || m.Thread != m.ID || m.Recipient != "human:bea" {
		t.Fatalf("answer %+v", m)
	}
	sent := must(orchclient.Mail(ctx, f.as("", ""), orchapi.MailQuery{From: "human:ana"}))
	if len(sent) != 2 {
		t.Fatalf("copies %+v", sent)
	}
	for _, c := range sent {
		if c.Audience != orchapi.MailAudienceParticipants || !slices.Equal(c.Participants, []string{"human:ana", c.Recipient}) ||
			c.To != c.Recipient || !slices.Equal(c.Members, members) || c.Batch != m.ID || c.Thread != m.ID {
			t.Fatalf("copy %+v", c)
		}
	}
	// retry-safe: the same request answers the same send; another payload conflicts
	if again := must(orchclient.Send(ctx, ana, req)); again.ID != m.ID {
		t.Fatalf("retry sent again: %+v", again)
	}
	req.To = "human:bea,human:dana"
	_, err := orchclient.Send(ctx, ana, req)
	wantCode(t, err, api.CodeConflict)
	if n := len(must(orchclient.Mail(ctx, f.as("", ""), orchapi.MailQuery{From: "human:ana"}))); n != 2 {
		t.Fatalf("%d copies after retries", n)
	}

	// each copy is private to its two parties
	beaIn, cydIn := must(orchclient.Inbox(ctx, bea, orchapi.InboxQuery{})), must(orchclient.Inbox(ctx, cyd, orchapi.InboxQuery{}))
	if len(beaIn) != 1 || beaIn[0].Recipient != "human:bea" || len(cydIn) != 1 || cydIn[0].Recipient != "human:cyd" {
		t.Fatalf("inboxes bea %+v cyd %+v", beaIn, cydIn)
	}
	_, err = orchclient.GetMail(ctx, bea, cydIn[0].ID)
	wantCode(t, err, api.CodeNotFound)
	if seen := must(orchclient.Mail(ctx, dana, orchapi.MailQuery{})); len(seen) != 0 {
		t.Fatalf("a third person reads the copies: %+v", seen)
	}

	// a reply reaches every other member, in the same thread, a fresh batch
	r := must(orchclient.Reply(ctx, bea, beaIn[0].ID, orchapi.ReplyRequest{Body: "yes"}))
	if !slices.Equal(r.Members, members) || r.Batch != r.ID || r.Thread != m.ID || r.ReplyTo != beaIn[0].ID || r.From != "human:bea" {
		t.Fatalf("reply %+v", r)
	}
	var cydReply string
	for who, c := range map[string]*client.Client{"human:ana": f.as("", ""), "human:cyd": cyd} {
		var got []orchapi.Mail
		for _, x := range must(orchclient.Inbox(ctx, c, orchapi.InboxQuery{})) {
			if x.From == "human:bea" {
				got = append(got, x)
			}
		}
		if len(got) != 1 || got[0].Recipient != who || got[0].Batch != r.ID || got[0].Thread != m.ID {
			t.Fatalf("%s got %+v", who, got)
		}
		if who == "human:cyd" {
			cydReply = got[0].ID
		}
	}
	if th := must(orchclient.Thread(ctx, cyd, cydReply)); len(th) != 2 || th[0].ID != cydIn[0].ID || th[1].ID != cydReply {
		t.Fatalf("cyd's thread %+v", th)
	}

	// read per copy: bea's Got it leaves cyd's copy unread
	if err := gotIt(bea, beaIn[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := gotIt(bea, cydIn[0].ID); err == nil {
		t.Fatal("bea marked cyd's copy read")
	}
	for _, c := range must(orchclient.Mail(ctx, f.as("", ""), orchapi.MailQuery{From: "human:ana"})) {
		if read := c.AckedAt != nil; read != (c.Recipient == "human:bea") || (read && c.AckedBy != "human:bea") {
			t.Fatalf("copy %s read=%v by %q", c.Recipient, read, c.AckedBy)
		}
	}
}

// thread, members and batch are in mail JSON; a two-party mail has a thread
// and neither of the others.
func TestMailJSONCarriesThreadMembersBatch(t *testing.T) {
	f := newFixture(t)
	f.member("bea")
	f.member("cyd")
	ana := f.as("", "side")
	g := must(orchclient.Send(ctx, ana, orchapi.SendMailRequest{To: "human:bea,human:cyd", Body: "hi both"}))
	one := must(orchclient.Send(ctx, ana, orchapi.SendMailRequest{To: "human:bea", Body: "hi you"}))
	raw := func(id string) map[string]json.RawMessage {
		rec := httptest.NewRecorder()
		f.local.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v0/mail/"+id, nil))
		b, _ := io.ReadAll(rec.Body)
		var out map[string]json.RawMessage
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatalf("%s: %s", err, b)
		}
		return out
	}
	gj, oj := raw(g.ID), raw(one.ID)
	if string(gj["thread"]) != `"`+g.ID+`"` || string(gj["batch"]) != `"`+g.ID+`"` ||
		string(gj["members"]) != `["human:ana","human:bea","human:cyd"]` {
		t.Fatalf("group mail JSON %v", gj)
	}
	if string(oj["thread"]) != `"`+one.ID+`"` || oj["members"] != nil || oj["batch"] != nil {
		t.Fatalf("two-party mail JSON %v", oj)
	}
	if !strings.HasPrefix(string(gj["participants"]), `["human:ana"`) {
		t.Fatalf("participants %s", gj["participants"])
	}
}

// A member who loses access drops out of replies; the rest still reach each
// other, down to a plain two-party reply.
func TestGroupReplySkipsMemberWhoLostAccess(t *testing.T) {
	f := newFixture(t)
	bea := f.member("bea")
	f.member("cyd")
	f.member("dana")
	ana := f.as("", "side")
	must(orchclient.Send(ctx, ana, orchapi.SendMailRequest{To: "human:bea,human:cyd,human:dana", Body: "lunch?"}))
	if _, err := f.d.Projects.Revoke(ctx, f.d.Projects.Local(""), api.GrantRequest{Identity: "dana", Project: "side", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	beaIn := must(orchclient.Inbox(ctx, bea, orchapi.InboxQuery{}))
	r := must(orchclient.Reply(ctx, bea, beaIn[0].ID, orchapi.ReplyRequest{Body: "yes"}))
	if !slices.Equal(r.Members, []string{"human:ana", "human:bea", "human:cyd"}) {
		t.Fatalf("reply members %+v", r.Members)
	}
	if _, err := f.d.Projects.Revoke(ctx, f.d.Projects.Local(""), api.GrantRequest{Identity: "cyd", Project: "side", Role: "member"}); err != nil {
		t.Fatal(err)
	}
	two := must(orchclient.Reply(ctx, bea, beaIn[0].ID, orchapi.ReplyRequest{Body: "still?"}))
	if two.To != "human:ana" || len(two.Members) != 0 {
		t.Fatalf("two-party reply %+v", two)
	}
}
