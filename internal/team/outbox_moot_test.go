package team_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// A queued status write that lost to the item being closed meanwhile
// no longer applies. Its session is told (needs_human), then the entry goes:
// the outbox and the copy's refused count read 0, not for 7 days. A queued
// claim that lost on an item still open stays (TestReplicaOfflineReadsAndOutbox).
func TestRefusalOnDoneItemIsDropped(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	it := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "closed meanwhile", Area: "solver"}))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-sess-1")
	rs := at(r.owner, "", cs.Address)
	eventually(t, 2*time.Second, func() error {
		if _, err := orchclient.GetWork(ctx, rs, it.ID); err != nil {
			return err
		}
		return nil
	})

	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	waitReachable(t, r, false)
	mustf(orchclient.SetStatus(ctx, rs, it.ID, orchapi.StatusRequest{Status: orchapi.WorkInReview}))(t)
	mustf(orchclient.SetStatus(ctx, ao, it.ID, orchapi.StatusRequest{Status: orchapi.WorkDone}))(t)
	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	waitDrained(t, r)

	eventually(t, 5*time.Second, func() error {
		es := mustf(r.owner.Outbox(ctx, "", false))(t)
		if st := remoteState(t, r); len(es) != 0 || st.Conflicts != 0 {
			return fmt.Errorf("outbox %+v, conflicts %d", es, st.Conflicts)
		}
		return nil
	})
	if s := mustf(orchclient.Session(ctx, r.owner, cs.Address))(t); s.State != orchapi.StateNeedsHuman || !strings.Contains(s.Note, "status conflict") {
		t.Fatalf("the session was not told: %+v", s)
	}
	if got := mustf(orchclient.GetWork(ctx, rs, it.ID))(t); got.Status != orchapi.WorkDone {
		t.Fatalf("copy %+v", got)
	}
}

// A link from a copy to a work item the copy does not hold (another
// project's) waits for the authority, so its refusal reaches the caller at
// once, with a hint, and never sits in the outbox as failed.
func TestLinkAcrossProjectsRefusedWhenWritten(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	it := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "links out", Area: "solver"}))(t)
	near := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "next door", Area: "solver"}))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-sess-1")
	rs := at(r.owner, "", cs.Address)
	eventually(t, 2*time.Second, func() error {
		_, err := orchclient.GetWork(ctx, rs, near.ID)
		return err
	})
	waitReachable(t, r, true)

	_, err := orchclient.Link(ctx, rs, it.ID, orchapi.Link{Rel: "related", Target: "bd:sengi-55s"})
	var ae *api.Error
	if !api.IsCode(err, api.CodeNotFound) || !errors.As(err, &ae) || !strings.Contains(ae.Hint, "link its URL") {
		t.Fatalf("link to another project's item: %v (%+v)", err, ae)
	}
	// The caller is told first; the settle drops the entry and the drain
	// recounts right after (outbox.go settle), so wait that moment out.
	eventually(t, 2*time.Second, func() error {
		if es := mustf(r.owner.Outbox(ctx, "", false))(t); len(es) != 0 || remoteState(t, r).Conflicts != 0 {
			return fmt.Errorf("the refused link stayed queued: %+v", es)
		}
		return nil
	})
	// A link inside the copy still answers at once and lands.
	mustf(orchclient.Link(ctx, rs, it.ID, orchapi.Link{Rel: "related", Target: near.ID}))(t)
	waitDrained(t, r)
	got := mustf(orchclient.GetWork(ctx, ao, it.ID))(t)
	if len(got.Links) != 1 || got.Links[0].Target != near.ID {
		t.Fatalf("authority links %+v", got.Links)
	}
}

// A link to another project's item queued while the authority was
// out answers the caller as written; once the authority refuses it, its
// session is told (needs_human) and the entry goes. It never applies by
// waiting, so it must not sit failed in the outbox for 7 days (the Mac's
// local:dc3vqp -> bd:sengi-55s).
func TestQueuedLinkAcrossProjectsDropped(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	it := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "links out offline", Area: "solver"}))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-sess-1")
	rs := at(r.owner, "", cs.Address)
	eventually(t, 2*time.Second, func() error {
		_, err := orchclient.GetWork(ctx, rs, it.ID)
		return err
	})

	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	waitReachable(t, r, false)
	mustf(orchclient.Link(ctx, rs, it.ID, orchapi.Link{Rel: "related", Target: "bd:sengi-55s"}))(t)
	if es := mustf(r.owner.Outbox(ctx, "", false))(t); len(es) != 1 {
		t.Fatalf("the offline link was not queued: %+v", es)
	}
	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	waitDrained(t, r)

	eventually(t, 5*time.Second, func() error {
		es := mustf(r.owner.Outbox(ctx, "", false))(t)
		if st := remoteState(t, r); len(es) != 0 || st.Conflicts != 0 {
			return fmt.Errorf("the refused link stayed: %+v, refused %d", es, st.Conflicts)
		}
		return nil
	})
	if s := mustf(orchclient.Session(ctx, r.owner, cs.Address))(t); s.State != orchapi.StateNeedsHuman || !strings.Contains(s.Note, "queued link failed") {
		t.Fatalf("the session was not told: %+v", s)
	}
}
