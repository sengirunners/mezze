package fulltest

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
)

// What a session still owes (SPEC 8 "What a session still owes"): a list read
// from records only, scoped to what the reader may see, with what zm cannot
// see shown as unknown.

func owedKinds(l orchapi.OwedList) map[string][]orchapi.OwedItem {
	out := map[string][]orchapi.OwedItem{}
	for _, it := range l.Items {
		out[it.Kind] = append(out[it.Kind], it)
	}
	return out
}

func TestOwedFromRecords(t *testing.T) {
	ctx := context.Background()
	f := newLeakFixture(t)
	addr := f.sessions["harbor/solver"]
	agent := f.ownerAt("harbor/solver", addr)

	item, err := orchclient.CreateWork(ctx, agent, orchapi.CreateWorkRequest{Title: "tidy the solver", Area: "solver"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Claim(ctx, agent, item.ID, orchapi.ClaimRequest{}); err != nil {
		t.Fatal(err)
	}
	ask, err := orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Body: "may I tidy the solver tonight?", Area: "solver",
		Subject: orchapi.AskSubject{Repo: "example/solver", PR: 9, HeadSHA: strings.Repeat("cd", 20)}})
	if err != nil {
		t.Fatal(err)
	}

	// The session's own list, by default.
	l, err := orchclient.Owed(ctx, agent, orchclient.OwedQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if l.For != orchapi.OwedForSession || l.Subject != addr {
		t.Fatalf("list is for %s %s, want session %s", l.For, l.Subject, addr)
	}
	by := owedKinds(l)
	if len(by[orchapi.OwedWork]) != 1 || by[orchapi.OwedWork][0].Ref != item.ID || by[orchapi.OwedWork][0].Holder != addr {
		t.Fatalf("held work: %+v", by[orchapi.OwedWork])
	}
	if len(by[orchapi.OwedAskOpen]) != 1 || by[orchapi.OwedAskOpen][0].Ref != ask.ID {
		t.Fatalf("open cards: %+v", by[orchapi.OwedAskOpen])
	}
	unseen := map[string]bool{}
	for _, g := range l.Unknown {
		unseen[g.Name] = g.Kind == orchapi.OwedGapUnseen
	}
	for _, name := range []string{"cloud_tasks", "outside_lanes", "background_processes", "worktree_changes", "unattributed_pull_requests"} {
		if !unseen[name] {
			t.Errorf("unknown is missing %s: %+v", name, l.Unknown)
		}
	}
	// The other session in the project holds the fixture's own item: not this list's.
	for _, it := range l.Items {
		if it.Holder != "" && it.Holder != addr {
			t.Fatalf("another session's row: %+v", it)
		}
	}

	// The owner's list covers all their sessions, this one included.
	owner := f.ownerAt("", "")
	pl, err := orchclient.Owed(ctx, owner, orchclient.OwedQuery{Person: "me"})
	if err != nil {
		t.Fatal(err)
	}
	if pl.For != orchapi.OwedForPerson || pl.Subject != f.dm.Deps.Config.Human {
		t.Fatalf("person list is for %s %s", pl.For, pl.Subject)
	}
	got := map[string]bool{}
	for _, it := range pl.Items {
		got[it.Kind+" "+it.Ref] = true
	}
	for _, want := range []string{orchapi.OwedWork + " " + item.ID, orchapi.OwedAskOpen + " " + ask.ID} {
		if !got[want] {
			t.Errorf("person list lacks %s: %+v", want, pl.Items)
		}
	}
	other := false
	for _, a := range pl.Sessions {
		other = other || a != addr
	}
	if !other {
		t.Errorf("person list covers only %v", pl.Sessions)
	}

	// An open pull request linked to the held item is owed too, and stays owed
	// when the lease lapses (the item says the PR is open: claimed or in review).
	const prURL = "https://example.test/example/solver/pull/9"
	if _, err := orchclient.Link(ctx, agent, item.ID, orchapi.Link{Rel: "pr", Target: prURL}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.SetStatus(ctx, agent, item.ID, orchapi.StatusRequest{Status: orchapi.WorkInReview}); err != nil {
		t.Fatal(err)
	}
	if l, err = orchclient.Owed(ctx, agent, orchclient.OwedQuery{}); err != nil {
		t.Fatal(err)
	}
	if prs := owedKinds(l)[orchapi.OwedPR]; len(prs) != 1 || prs[0].Ref != prURL || prs[0].Holder != addr {
		t.Fatalf("open pull request: %+v", l.Items)
	}
	if _, err := orchclient.Release(ctx, agent, item.ID, orchapi.ReleaseRequest{}); err != nil {
		t.Fatal(err)
	}
	// What a session last held is on its record when a replica's presence says
	// so (RecordWork); the PR stays owed by it.
	orchcore.Sessions(f.dm.Deps).(orchcore.SessionWorkRecorder).RecordWork(ctx, addr, item.ID)
	if l, err = orchclient.Owed(ctx, agent, orchclient.OwedQuery{}); err != nil {
		t.Fatal(err)
	}
	if by := owedKinds(l); len(by[orchapi.OwedWork]) != 0 || len(by[orchapi.OwedPR]) != 1 {
		t.Fatalf("after the lease lapsed: %+v", l.Items)
	}
	// Merged (the item is done): no longer owed.
	if _, err := orchclient.SetStatus(ctx, agent, item.ID, orchapi.StatusRequest{Status: orchapi.WorkDone}); err != nil {
		t.Fatal(err)
	}
	l, err = orchclient.Owed(ctx, agent, orchclient.OwedQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if by := owedKinds(l); len(by[orchapi.OwedWork]) != 0 || len(by[orchapi.OwedPR]) != 0 {
		t.Fatalf("done work still owed: %+v", l.Items)
	}
	// A withdrawn card is no longer owed.
	if _, err := orchclient.WithdrawAsk(ctx, agent, ask.ID); err != nil {
		t.Fatal(err)
	}
	if l, err = orchclient.Owed(ctx, agent, orchclient.OwedQuery{}); err != nil || len(owedKinds(l)[orchapi.OwedAskOpen]) != 0 {
		t.Fatalf("withdrawn card still owed: %+v %v", l.Items, err)
	}
}

// A guest reads nothing of a session it cannot see: asking about one answers
// exactly as asking about a missing one, and no hidden id or title appears.
func TestOwedHiddenAnswersLikeMissing(t *testing.T) {
	f := newLeakFixture(t)
	hidden := f.sessions["harbor"] // a project-level session: not the guest's area
	if hidden == "" {
		t.Fatal("fixture has no hidden session")
	}
	for _, tr := range []transport{viaTailnet, viaUnmarked, viaSocket} { // the tunnel needs a key-bound token (leak_team_test.go walks it)
		hid := f.call("GET", "/v0/owed?session="+hidden, "", tr)
		mis := f.call("GET", "/v0/owed?session=nope-zz/zzzzzzzz", "", tr)
		f.checkNoLeak(t, tr.String()+" owed hidden session", leakResp{hid.status, normalize(hid, hidden)}) // its own input echoed is no leak
		if hid.status != 404 || hid.status != mis.status || normalize(hid, hidden) != normalize(mis, "nope-zz/zzzzzzzz") {
			t.Errorf("%s: hidden session %d %q, missing %d %q", tr, hid.status, clip(hid.body), mis.status, clip(mis.body))
		}
		// Another person's sessions answer like a missing person.
		per := f.call("GET", "/v0/owed?person="+f.dm.Deps.Config.Human, "", tr)
		none := f.call("GET", "/v0/owed?person=nobody-zz", "", tr)
		f.checkNoLeak(t, tr.String()+" owed other person", per)
		if per.status != 404 || per.status != none.status || normalize(per, f.dm.Deps.Config.Human) != normalize(none, "nobody-zz") {
			t.Errorf("%s: other person %d %q, missing %d %q", tr, per.status, clip(per.body), none.status, clip(none.body))
		}
	}
	// A guest's own list is empty of anything it may not read, and still says what is unknown.
	r := f.call("GET", "/v0/owed", "", viaTailnet)
	f.checkNoLeak(t, "guest's own owed list", r)
	var l orchapi.OwedList
	if r.status != 200 || json.Unmarshal([]byte(r.body), &l) != nil || l.For != orchapi.OwedForPerson || len(l.Items) != 0 || len(l.Unknown) == 0 {
		t.Fatalf("guest's own list: %d %s", r.status, clip(r.body))
	}
}
