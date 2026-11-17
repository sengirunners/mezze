package team_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// SPEC 17.6 "Writes": a mail or reply the replica queued (its caller was told
// "queued") and the authority refuses at replay is told to its sending session
// once, with the authority's message and wait, through the session's own state:
// a local write, not mail, so no send limit applies to the notice and it cannot
// start a reply loop. The refusal here is the send limit's exact-repeat rule.
func TestQueuedMailRefusedTellsItsSender(t *testing.T) {
	for _, kind := range []string{"mail", "reply", "private mail"} {
		t.Run(kind, func(t *testing.T) {
			quietPresence(t)
			ctx := context.Background()
			tn := tailnettest.New()
			a := authority(t, tn)
			r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
			from, to := register(t, r, "harbor", "bea-mail-a"), register(t, r, "harbor", "bea-mail-b")
			heartbeat(t, r)
			rs := at(r.sock, "harbor", from.Address)
			send := func() (orchapi.Mail, error) {
				return orchclient.Send(ctx, rs, orchapi.SendMailRequest{To: to.Address, Body: "same words"})
			}
			if kind == "private mail" {
				send = func() (orchapi.Mail, error) {
					return orchclient.Send(ctx, rs, orchapi.SendMailRequest{To: to.Address, Body: "same words", Audience: orchapi.MailAudienceParticipants})
				}
			}
			if kind == "reply" {
				parent := mustf(orchclient.Send(ctx, at(r.sock, "harbor", to.Address), orchapi.SendMailRequest{To: from.Address, Body: "question"}))(t)
				send = func() (orchapi.Mail, error) {
					return orchclient.Reply(ctx, rs, parent.ID, orchapi.ReplyRequest{Body: "same words"})
				}
			}
			first := mustf(send())(t)
			if strings.HasPrefix(first.Via, "queued") {
				t.Fatalf("first %s queued: %+v", kind, first)
			}

			tn.Cut("potluck-bea-mac", "potluck-mini", true)
			waitReachable(t, r, false)
			second := mustf(send())(t)
			if !strings.HasPrefix(second.Via, "queued") {
				t.Fatalf("offline %s not queued: %+v", kind, second)
			}
			tn.Cut("potluck-bea-mac", "potluck-mini", false)
			waitDrained(t, r)

			// The sender is told what the authority said and when to retry.
			var told orchapi.Session
			eventually(t, 5*time.Second, func() error {
				told = mustf(orchclient.Session(ctx, r.owner, from.Address))(t)
				if told.State != orchapi.StateNeedsHuman {
					return fmt.Errorf("the sender was not told: %+v", told)
				}
				return nil
			})
			// The note is listed beyond the mail's audience: the rule and the
			// wait, never the authority's words (they name the recipient).
			kindWord := strings.Fields(kind)[len(strings.Fields(kind))-1]
			for _, want := range []string{"queued " + kindWord + " failed: rate_limited (repeat limit); retry after ", "zm team outbox"} {
				if !strings.Contains(told.Note, want) {
					t.Fatalf("note %q lacks %q", told.Note, want)
				}
			}
			if strings.Contains(told.Note, to.Address) || strings.Contains(told.Note, "mail limit") {
				t.Fatalf("note %q carries the authority's message", told.Note)
			}
			// The refused entry stays listed for the sending session, with the
			// authority's words.
			if es := mustf(rs.Outbox(ctx, "", false))(t); len(es) != 1 || es[0].State != "failed" || !strings.Contains(es[0].Error, "mail limit") {
				t.Fatalf("outbox %+v", es)
			}
			if to := mustf(orchclient.Session(ctx, r.owner, to.Address))(t); to.State == orchapi.StateNeedsHuman {
				t.Fatalf("the recipient was told: %+v", to)
			}
			// Nothing was mailed for it: the refusal is not itself a mail.
			if sent := mustf(orchclient.Mail(ctx, at(a.owner, "harbor", ""), orchapi.MailQuery{From: from.Address, Limit: 100}))(t); kind != "private mail" && len(sent) != 1 {
				t.Fatalf("the authority holds %d mails from the sender, want 1: %+v", len(sent), sent)
			}

			// Once: the sender clears it; later syncs and drains do not repeat it.
			mustf(orchclient.SetState(ctx, r.owner, from.Address, orchapi.StateRequest{State: orchapi.StateIdle}))(t)
			heartbeat(t, r)
			time.Sleep(300 * time.Millisecond)
			if got := mustf(orchclient.Session(ctx, r.owner, from.Address))(t); got.State == orchapi.StateNeedsHuman {
				t.Fatalf("told twice: %+v", got)
			}
		})
	}
}
