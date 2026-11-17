package team_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A hook's poll on a replica: the copy serves the inbox, and
// GET /v0/mail/inbox/peek lists the announcement, direct and group mail
// without marking the announcement seen; the plain inbox then marks it.
func TestReplicaPeekLeavesAnnouncementsUnseen(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := register(t, r, "harbor", "bea-peek-1")
	heartbeat(t, r)
	ms := register(t, a, "harbor", "mini-peek-1")
	am := at(a.owner, "harbor", ms.Address)
	ann := mustf(orchclient.Send(ctx, am, orchapi.SendMailRequest{To: "all", Kind: "announce", Body: "main is frozen"}))(t)
	grp := mustf(orchclient.Send(ctx, am, orchapi.SendMailRequest{To: "machine:bea-mac", Body: "anyone on bea?"}))(t)
	dir := mustf(orchclient.Send(ctx, am, orchapi.SendMailRequest{To: cs.Address, Body: "yours"}))(t)
	rs := at(r.owner, "", cs.Address)
	want := func(in []orchapi.Mail, ids ...string) error {
		for _, id := range ids {
			if !hasMail(in, id) {
				return fmt.Errorf("missing %s in %+v", id, in)
			}
		}
		return nil
	}
	eventually(t, 2*time.Second, func() error {
		return want(mustf(orchclient.PeekInbox(ctx, rs, orchapi.InboxQuery{}))(t), ann.ID, grp.ID, dir.ID)
	})
	for i := 0; i < 2; i++ {
		if err := want(mustf(orchclient.PeekInbox(ctx, rs, orchapi.InboxQuery{}))(t), ann.ID, grp.ID, dir.ID); err != nil {
			t.Fatalf("peek %d: %v", i, err)
		}
	}
	if err := want(mustf(orchclient.Inbox(ctx, rs, orchapi.InboxQuery{}))(t), ann.ID); err != nil {
		t.Fatalf("inbox after peeks: %v", err)
	}
	if in := mustf(orchclient.PeekInbox(ctx, rs, orchapi.InboxQuery{}))(t); hasMail(in, ann.ID) || !hasMail(in, dir.ID) {
		t.Fatalf("plain inbox did not mark the announcement seen: %+v", in)
	}
}
