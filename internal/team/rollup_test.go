package team_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

func rollupOf(t *testing.T, l orchapi.WorkRollupList, slug string) orchapi.WorkRollup {
	t.Helper()
	for _, r := range l.Rollups {
		if r.Slug == slug {
			return r
		}
	}
	t.Fatalf("no rollup for %s in %+v", slug, l)
	return orchapi.WorkRollup{}
}

func rollupNode(r orchapi.WorkRollup, id string) *orchapi.RollupNode {
	for i := range r.Nodes {
		if r.Nodes[i].ID == id {
			return &r.Nodes[i]
		}
	}
	return nil
}

// A rollup is the same at the authority and on a replica's copy, needs no
// tailnet once copied, follows changes, merges with no project named, and
// shows a guest only its area.
func TestRollupAtAuthorityAndCopy(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	// The tracker's area makes it visible to a reader of that area, as for GET
	// /v0/work; each row's own area then decides.
	if res, err := orchclient.AddTracker(ctx, ao, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", Area: "solver", WaitSeconds: 10}); err != nil || !res.OK {
		t.Fatalf("add native tracker: %+v %v", res, err)
	}
	due := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)

	theme := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "the theme", Area: "solver", IssueType: "epic", Labels: []string{"theme"}}))(t)
	epic := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "the epic", Area: "solver", IssueType: "epic", Parent: theme.ID}))(t)
	urgent := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "urgent", Area: "solver", Parent: epic.ID, Priority: new(0), DueAt: &due}))(t)
	_ = mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "plain", Area: "solver", Parent: epic.ID, Priority: new(2)}))(t)
	secretTheme := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "hidden theme", Area: "billing", IssueType: "epic", Labels: []string{"theme"}}))(t)
	_ = mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "hidden item", Area: "billing", Parent: secretTheme.ID, Priority: new(0)}))(t)
	_ = mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "no epic", Area: "solver"}))(t)

	check := func(r orchapi.WorkRollup) error {
		e, th := rollupNode(r, "issue:"+epic.ID), rollupNode(r, "issue:"+theme.ID)
		if e == nil || th == nil {
			return fmt.Errorf("nodes %+v", r.Nodes)
		}
		if e.Total != 2 || e.Urgent != 1 || e.Open != 2 || e.Parent != th.ID || e.Kind != orchapi.RollupEpic || e.Changed.IsZero() {
			return fmt.Errorf("epic %+v", e)
		}
		if th.Total != 2 || th.Kind != orchapi.RollupTheme || len(th.Children) != 1 || th.Children[0] != e.ID {
			return fmt.Errorf("theme %+v", th)
		}
		if r.NoEpic.Total != 1 {
			return fmt.Errorf("no epic %+v", r.NoEpic)
		}
		if len(r.Deadlines) != 1 || r.Deadlines[0].ID != "issue:"+urgent.ID || r.Deadlines[0].DueAt == nil {
			return fmt.Errorf("deadlines %+v", r.Deadlines)
		}
		return nil
	}
	eventually(t, 10*time.Second, func() error {
		return check(rollupOf(t, mustf(orchclient.WorkRollup(ctx, a.owner, "harbor", ""))(t), "harbor"))
	})

	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	eventually(t, 10*time.Second, func() error {
		return check(rollupOf(t, mustf(orchclient.WorkRollup(ctx, r.owner, "harbor", ""))(t), "harbor"))
	})
	// No project named: this machine's own merged with the joined one's.
	if err := check(rollupOf(t, mustf(orchclient.WorkRollup(ctx, r.owner, "", ""))(t), "harbor")); err != nil {
		t.Fatal(err)
	}
	// The copy answers with the authority out of reach, and says so once stale.
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	if err := check(rollupOf(t, mustf(orchclient.WorkRollup(ctx, r.owner, "harbor", ""))(t), "harbor")); err != nil {
		t.Fatalf("cut off: %v", err)
	}
	tn.Cut("potluck-bea-mac", "potluck-mini", false)

	// A change comes through.
	mustf(orchclient.CloseIssue(ctx, ao, urgent.ID, orchapi.CloseIssueRequest{}))(t)
	eventually(t, 10*time.Second, func() error {
		got := rollupOf(t, mustf(orchclient.WorkRollup(ctx, r.owner, "harbor", ""))(t), "harbor")
		if e := rollupNode(got, "issue:"+epic.ID); e == nil || e.Done != 1 || e.Urgent != 0 || e.Open != 1 {
			return fmt.Errorf("after close: %+v", e)
		}
		if len(got.Deadlines) != 0 {
			return fmt.Errorf("a done item's deadline: %+v", got.Deadlines)
		}
		return nil
	})

	// A guest's replica holds only its area: no trace of the hidden theme.
	g, _ := replica(t, tn, a, "vida-mac", "nVIDA", "harbor/solver", "vida", "guest")
	eventually(t, 10*time.Second, func() error {
		got := rollupOf(t, mustf(orchclient.WorkRollup(ctx, g.owner, "harbor", ""))(t), "harbor")
		if rollupNode(got, "issue:"+epic.ID) == nil {
			return fmt.Errorf("guest nodes %+v", got.Nodes)
		}
		js, _ := json.Marshal(got)
		if strings.Contains(string(js), secretTheme.ID) || strings.Contains(string(js), "hidden") {
			return fmt.Errorf("the guest sees the other area: %s", js)
		}
		if got.Totals.Total != 3 {
			return fmt.Errorf("guest totals %+v", got.Totals)
		}
		return nil
	})
}

// Claimed: every live lease in sight, the caller's own marked, with a holder
// that a reader may not see redacted.
func TestRollupClaims(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a, ao := nativeAuthority(t, tn)
	mine := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "mine", Area: "solver"}))(t)
	other := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "someone else's", Area: "solver"}))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	rc := at(r.owner, "harbor", "")
	sess := mustf(orchclient.Register(ctx, rc, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "rollup-claim", State: orchapi.StateRunning}))(t)
	as := at(r.owner, "harbor", sess.Address)
	eventually(t, 10*time.Second, func() error {
		_, err := orchclient.Claim(ctx, as, "issue:"+mine.ID, orchapi.ClaimRequest{})
		return err
	})
	mustf(orchclient.Claim(ctx, ao, "issue:"+other.ID, orchapi.ClaimRequest{}))(t)

	want := func(l orchapi.WorkRollupList, mineWanted bool) error {
		got := rollupOf(t, l, "harbor")
		if len(got.Claimed) != 2 {
			return fmt.Errorf("claimed %+v", got.Claimed)
		}
		for _, c := range got.Claimed {
			switch c.ID {
			case "issue:" + mine.ID:
				if c.Mine != mineWanted || c.Title != "mine" || c.Holder != sess.Address {
					return fmt.Errorf("own claim %+v", c)
				}
			case "issue:" + other.ID:
				if c.Mine {
					return fmt.Errorf("another's claim is mine: %+v", c)
				}
			default:
				return fmt.Errorf("claim %+v", c)
			}
		}
		return nil
	}
	// The session itself, and the person who started it, on the replica.
	eventually(t, 10*time.Second, func() error { return want(mustf(orchclient.WorkRollup(ctx, as, "harbor", ""))(t), true) })
	if err := want(mustf(orchclient.WorkRollup(ctx, rc, "harbor", ""))(t), true); err != nil {
		t.Fatalf("the person: %v", err)
	}
	// Forwarded or at the authority, a session of another machine's person is not this owner's.
	got := rollupOf(t, mustf(orchclient.WorkRollup(ctx, ao, "harbor", ""))(t), "harbor")
	for _, c := range got.Claimed {
		if c.ID == "issue:"+mine.ID && c.Mine {
			t.Fatalf("the authority's owner does not hold it: %+v", c)
		}
		if c.ID == "issue:"+other.ID && !c.Mine {
			t.Fatalf("the authority's owner holds this one: %+v", c)
		}
	}
}
