package team_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// remoteState is the replica's view of its one joined project.
func remoteState(t *testing.T, r *daemon) api.ReplicaStatus {
	t.Helper()
	rs := mustf(r.owner.Remotes(context.Background()))(t)
	if len(rs) != 1 || rs[0].Replica == nil {
		t.Fatalf("remotes %+v", rs)
	}
	return *rs[0].Replica
}

func waitReachable(t *testing.T, r *daemon, want bool) {
	t.Helper()
	eventually(t, 5*time.Second, func() error {
		if st := remoteState(t, r); st.Reachable != want || st.State == "loading" {
			return fmt.Errorf("replica %+v, want reachable=%v", st, want)
		}
		return nil
	})
}

func waitDrained(t *testing.T, r *daemon) {
	t.Helper()
	eventually(t, 10*time.Second, func() error {
		if st := remoteState(t, r); st.Pending != 0 {
			return fmt.Errorf("outbox still pending: %+v", st)
		}
		return nil
	})
}

// The round C goal: with its authority unreachable a replica keeps
// working. Reads come from the copy (marked stale), writes queue in the
// outbox and drain in order when the authority returns, and the authority
// arbitrates: a queued claim that loses comes back as a clear conflict.
func TestReplicaOfflineReadsAndOutbox(t *testing.T) {
	timing := newTeamTiming(t, "offline")
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	it1 := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "contested", Area: "solver"}))(t)
	it2 := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "to review", Area: "solver"}))(t)
	it3 := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "free", Area: "solver"}))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-sess-1")
	rs := at(r.owner, "", cs.Address)
	eventually(t, 2*time.Second, func() error {
		if wl := mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{}))(t); len(wl.Items) != 3 {
			return fmt.Errorf("copy has %d items", len(wl.Items))
		}
		return nil
	})

	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	waitReachable(t, r, false)

	// Reads: served from the copy, marked stale, no tailnet wait.
	start := time.Now()
	wl := mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{}))(t)
	timing.record("team/offline-list", []time.Duration{time.Since(start)}, 50*time.Millisecond)
	if len(wl.Items) != 3 || len(wl.Behind) != 1 || wl.Behind[0].State != orchapi.SyncStale {
		t.Fatalf("offline list: %+v", wl)
	}
	if got := mustf(orchclient.GetWork(ctx, rs, it1.ID))(t); got.Stale == "" || got.Title != "contested" {
		t.Fatalf("offline show %+v", got)
	}

	// Writes queue. A comment and a status change answer at once and show
	// in reads at once (overlaid).
	start = time.Now()
	c := mustf(orchclient.Comment(ctx, rs, it2.ID, orchapi.CommentRequest{Body: "looked at it\u202e offline"}))(t)
	timing.record("team/offline-comment", []time.Duration{time.Since(start)}, 50*time.Millisecond)
	if c.ID == "" || c.Body != "looked at it offline" {
		t.Fatalf("offline comment: %+v", c)
	}
	mustf(orchclient.SetStatus(ctx, rs, it2.ID, orchapi.StatusRequest{Status: orchapi.WorkInReview}))(t)
	got := mustf(orchclient.GetWork(ctx, rs, it2.ID))(t)
	if got.Status != orchapi.WorkInReview || len(got.Comments) != 1 || got.Comments[0].ID != c.ID {
		t.Fatalf("overlay %+v", got)
	}
	// Claims queue too, and say so: not a lease.
	for _, id := range []string{it3.ID, it1.ID} {
		if _, err := orchclient.Claim(ctx, rs, id, orchapi.ClaimRequest{}); !api.IsCode(err, api.CodeQueued) {
			t.Fatalf("offline claim %s: %v", id, err)
		}
	}
	m := mustf(orchclient.Send(ctx, rs, orchapi.SendMailRequest{Kind: orchapi.MailKindReport, To: "human:mini-human", Body: "sent while\u200b offline", Needs: orchapi.NeedsAny}))(t)
	if !strings.HasPrefix(m.Via, "queued") || m.Body != "sent while offline" {
		t.Fatalf("offline mail %+v", m)
	}
	if es := mustf(r.owner.Outbox(ctx, "", false))(t); len(es) != 5 {
		t.Fatalf("outbox %+v", es)
	}

	// Meanwhile someone at the authority takes it1: the queued claim loses.
	mustf(orchclient.Claim(ctx, at(a.owner, "harbor", ""), it1.ID, orchapi.ClaimRequest{}))(t)

	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	waitDrained(t, r)

	// The authority applied them in order, each once.
	cs2 := mustf(orchclient.Comments(ctx, ao, it2.ID))(t)
	if len(cs2) != 1 || cs2[0].ID != c.ID || cs2[0].Author != cs.Address {
		t.Fatalf("authority comments %+v", cs2)
	}
	if st := mustf(orchclient.GetWork(ctx, ao, it2.ID))(t); st.Status != orchapi.WorkInReview {
		t.Fatalf("authority status %+v", st)
	}
	ls := mustf(orchclient.Leases(ctx, ao, orchapi.LeaseQuery{Project: "harbor"}))(t)
	holders := map[string]string{}
	for _, l := range ls {
		holders[l.Key] = l.Holder
	}
	if holders[it3.ID] != cs.Address || holders[it1.ID] == cs.Address || holders[it1.ID] == "" {
		t.Fatalf("authority leases %+v", ls)
	}
	evs := projectEvents(t, a)
	order := map[string]int{}
	for i, e := range evs {
		key := e.Type + " " + e.Subject
		if _, ok := order[key]; !ok {
			order[key] = i
		}
	}
	cIdx, sIdx, kIdx := order[orchapi.EventWorkCommented+" "+it2.ID], order[orchapi.EventWorkStatusChanged+" "+it2.ID], order[orchapi.EventWorkClaimed+" "+it3.ID]
	if !(cIdx < sIdx && sIdx < kIdx) {
		t.Fatalf("applied out of order: comment %d status %d claim %d", cIdx, sIdx, kIdx)
	}
	// The lost claim is a clear conflict: in the outbox, and on the session.
	es := mustf(r.owner.Outbox(ctx, "", false))(t)
	if len(es) != 1 || es[0].State != api.OutboxConflict || es[0].Item != it1.ID || es[0].Kind != "claim" {
		t.Fatalf("outbox after drain %+v", es)
	}
	if s := mustf(orchclient.Session(ctx, r.owner, cs.Address))(t); s.State != orchapi.StateNeedsHuman || !strings.Contains(s.Note, "claim conflict") {
		t.Fatalf("session after the lost claim %+v", s)
	}
	// The copy converges: the mail is in the sent list once, the status is
	// the authority's, the lease is the session's.
	eventually(t, 2*time.Second, func() error {
		sent := mustf(orchclient.Sent(ctx, rs, "", 50))(t)
		n := 0
		for _, x := range sent {
			if x.ID == m.ID && x.Recipient != "" {
				n++
			}
		}
		if n != 1 {
			return fmt.Errorf("sent %+v", sent)
		}
		if l := mustf(orchclient.Leases(ctx, r.owner, orchapi.LeaseQuery{}))(t); len(l) != 2 {
			return fmt.Errorf("replica leases %+v", l)
		}
		return nil
	})
	// Online again: a claim is one round trip and answers the lease.
	cl := mustf(orchclient.Claim(ctx, rs, it3.ID, orchapi.ClaimRequest{Renew: true}))(t)
	if cl.Lease.Holder != cs.Address {
		t.Fatalf("online renew %+v", cl)
	}
}

// projectEvents is every stored event of harbor at the authority, oldest first.
func projectEvents(t *testing.T, a *daemon) []api.Event {
	t.Helper()
	var out []api.Event
	if err := a.owner.Events(context.Background(), api.EventQuery{Project: "harbor", Since: "0", NoLive: true}, func(e api.Event) error {
		out = append(out, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// writes posts a batch to the authority as the replica (its node-bound
// token, from its own node).
func writes(t *testing.T, tn *tailnettest.Net, r *daemon, token string, req api.WritesRequest) api.WritesResponse {
	t.Helper()
	b, _ := json.Marshal(req)
	hr, _ := http.NewRequest(http.MethodPost, "http://potluck-mini:7433/v0/team/writes", bytes.NewReader(b))
	hr.Header.Set(api.HeaderAuthorization, "Bearer "+token)
	hr.Header.Set(api.HeaderProtocol, api.Protocol)
	hr.Header.Set("Content-Type", "application/json")
	resp, err := tailnetClientAs(t, tn, r.cfg.Machine).Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out api.WritesResponse
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&out) != nil {
		t.Fatalf("writes: %s", resp.Status)
	}
	return out
}

// A replayed write never runs twice: the authority answers the recorded
// result, and even with the record gone a comment stored under its write
// id is not stored again. A status write with a stale base is refused.
func TestWritesReplayAndStaleBase(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	it := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "replayed", Area: "solver"}))(t)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	token := readToken(t, j.Remote.TokenFile)
	pid := j.Remote.Project.ID
	evs := projectEvents(t, a)
	base := evs[len(evs)-1].ID

	wid, sid := ids.New(), ids.New()
	batch := api.WritesRequest{Project: pid, Writes: []api.QueuedWrite{
		{ID: wid, Method: http.MethodPost, Path: "/v0/work/" + it.ID + "/comment", Body: json.RawMessage(`{"body":"once"}`), As: "bea-mac/aaaa0001", Item: it.ID},
		{ID: sid, Method: http.MethodPost, Path: "/v0/work/" + it.ID + "/status", Body: json.RawMessage(`{"status":"in_review"}`), As: "bea-mac/aaaa0001", Item: it.ID, Base: base},
		{ID: ids.New(), Method: http.MethodPost, Path: "/v0/work/import", Body: json.RawMessage(`{}`)},
	}}
	first := writes(t, tn, r, token, batch)
	if len(first.Results) != 3 || first.Results[0].Status != 200 || first.Results[1].Status != 200 || first.Results[2].Status != 400 || first.Results[0].Replayed {
		t.Fatalf("first %+v", first)
	}
	second := writes(t, tn, r, token, batch)
	if len(second.Results) != 3 || !second.Results[0].Replayed || !second.Results[1].Replayed || string(second.Results[0].Body) != string(first.Results[0].Body) {
		t.Fatalf("replay %+v", second)
	}
	// The records lost (a crash between the change and its record): the
	// comment is still stored once, under its write id.
	db, err := a.dm.Deps.Store.Project(authz.All(), authz.Read, pid)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM team_writes`); err != nil {
		t.Fatal(err)
	}
	third := writes(t, tn, r, token, api.WritesRequest{Project: pid, Writes: batch.Writes[:1]})
	if third.Results[0].Status != 200 || third.Results[0].Replayed {
		t.Fatalf("third %+v", third)
	}
	if cs := mustf(orchclient.Comments(ctx, ao, it.ID))(t); len(cs) != 1 || cs[0].ID != wid {
		t.Fatalf("comments %+v", cs)
	}
	// Someone changes the status at the authority; a write made before it
	// (its base) is refused, recorded and returned: no silent last writer.
	mustf(orchclient.SetStatus(ctx, ao, it.ID, orchapi.StatusRequest{Status: orchapi.WorkDone}))(t)
	stale := writes(t, tn, r, token, api.WritesRequest{Project: pid, Writes: []api.QueuedWrite{
		{ID: ids.New(), Method: http.MethodPost, Path: "/v0/work/" + it.ID + "/status", Body: json.RawMessage(`{"status":"open"}`), As: "bea-mac/aaaa0001", Item: it.ID, Base: base},
	}})
	if stale.Results[0].Status != http.StatusConflict {
		t.Fatalf("stale base %+v", stale)
	}
	if st := mustf(orchclient.GetWork(ctx, ao, it.ID))(t); st.Status != orchapi.WorkDone {
		t.Fatalf("status after a stale write %+v", st)
	}
	// A comment changing after base does not refuse a status write: base is
	// per field. (seen from the first batch is past the done change? no:
	// take the newest id now.)
	evs = projectEvents(t, a)
	fresh := evs[len(evs)-1].ID
	mustf(orchclient.Comment(ctx, ao, it.ID, orchapi.CommentRequest{Body: "a comment after base"}))(t)
	ok := writes(t, tn, r, token, api.WritesRequest{Project: pid, Writes: []api.QueuedWrite{
		{ID: ids.New(), Method: http.MethodPost, Path: "/v0/work/" + it.ID + "/status", Body: json.RawMessage(`{"status":"open"}`), As: "bea-mac/aaaa0001", Item: it.ID, Base: fresh},
	}})
	if ok.Results[0].Status != 200 {
		t.Fatalf("fresh base %+v", ok)
	}
	// Review finding: a comment (or any other write) earlier in the batch
	// must not count as having seen someone else's status change.
	evs = projectEvents(t, a)
	b0 := evs[len(evs)-1].ID
	mustf(orchclient.SetStatus(ctx, ao, it.ID, orchapi.StatusRequest{Status: orchapi.WorkDone}))(t)
	masked := writes(t, tn, r, token, api.WritesRequest{Project: pid, Writes: []api.QueuedWrite{
		{ID: ids.New(), Method: http.MethodPost, Path: "/v0/work/" + it.ID + "/comment", Body: json.RawMessage(`{"body":"first a comment"}`), As: "bea-mac/aaaa0001", Item: it.ID},
		{ID: ids.New(), Method: http.MethodPost, Path: "/v0/work/" + it.ID + "/status", Body: json.RawMessage(`{"status":"in_review"}`), As: "bea-mac/aaaa0001", Item: it.ID, Base: b0},
	}})
	if masked.Results[0].Status != 200 || masked.Results[1].Status != http.StatusConflict {
		t.Fatalf("a comment masked a status change: %+v", masked)
	}
	// But the replica's own claim (which moved the item to claimed) counts:
	// claim then set status, queued together, is not a conflict.
	it2 := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "claimed then reviewed", Area: "solver"}))(t)
	evs = projectEvents(t, a)
	b1 := evs[len(evs)-1].ID
	own := writes(t, tn, r, token, api.WritesRequest{Project: pid, Writes: []api.QueuedWrite{
		{ID: ids.New(), Method: http.MethodPost, Path: "/v0/work/" + it2.ID + "/claim", Body: json.RawMessage(`{}`), As: "bea-mac/aaaa0001", Item: it2.ID},
		{ID: ids.New(), Method: http.MethodPost, Path: "/v0/work/" + it2.ID + "/status", Body: json.RawMessage(`{"status":"in_review"}`), As: "bea-mac/aaaa0001", Item: it2.ID, Base: b1},
	}})
	if own.Results[0].Status != 200 || own.Results[0].StatusSeen == "" || own.Results[1].Status != 200 {
		t.Fatalf("own claim then status: %+v", own)
	}
	// Only a node-bound token may post writes.
	var er api.ErrorResponse
	err = at(a.owner, "", "").Do(ctx, http.MethodPost, "/v0/team/writes", batch, &er)
	if !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("socket writes: %v", err)
	}
}

// A replica whose cursor fell below what the authority keeps gets 410
// resync: it takes a snapshot (rows not in it go), keeps its outbox and
// replays it.
func TestResyncKeepsOutbox(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	it := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "kept", Area: "solver"}))(t)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	rs := at(r.owner, "", "bea-mac/aaaa0002")
	waitReachable(t, r, true)
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	waitReachable(t, r, false)
	c := mustf(orchclient.Comment(ctx, rs, it.ID, orchapi.CommentRequest{Body: "across a resync"}))(t)
	// The authority moves on and prunes everything the replica had seen.
	for i := 0; i < 5; i++ {
		mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: fmt.Sprint("later ", i), Area: "solver"}))(t)
	}
	a.dm.Deps.Events.PruneTeam(ctx, j.Remote.Project.ID, 2, time.Now().Add(time.Hour))
	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	waitDrained(t, r)
	eventually(t, 3*time.Second, func() error {
		if wl := mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{}))(t); len(wl.Items) != 6 {
			return fmt.Errorf("copy after resync: %d items", len(wl.Items))
		}
		return nil
	})
	if cs := mustf(orchclient.Comments(ctx, ao, it.ID))(t); len(cs) != 1 || cs[0].ID != c.ID {
		t.Fatalf("comments %+v", cs)
	}
}

// A guest replica's copy holds only its area: items, comments, leases and
// mail outside it never reach the file.
func TestGuestCopyHoldsOnlyItsArea(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	sol := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "solver item", Area: "solver"}))(t)
	bil := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "HIDDEN billing", Area: "billing"}))(t)
	mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "HIDDEN project level"}))(t)
	mustf(orchclient.Comment(ctx, ao, bil.ID, orchapi.CommentRequest{Body: "HIDDEN comment"}))(t)
	mustf(orchclient.Claim(ctx, ao, bil.ID, orchapi.ClaimRequest{}))(t)
	v, j := replica(t, tn, a, "vida-mac", "nVIDA", "harbor/solver", "vida", "guest")
	// After the snapshot, more changes flow by sync.
	mustf(orchclient.Comment(ctx, ao, bil.ID, orchapi.CommentRequest{Body: "HIDDEN later comment"}))(t)
	mustf(orchclient.Comment(ctx, ao, sol.ID, orchapi.CommentRequest{Body: "visible later comment"}))(t)
	eventually(t, 3*time.Second, func() error {
		if got := mustf(orchclient.GetWork(ctx, at(v.owner, "", ""), sol.ID))(t); len(got.Comments) != 1 {
			return fmt.Errorf("guest copy comments %+v", got.Comments)
		}
		return nil
	})
	db, err := store.OpenReplica(ctx, filepath.Join(v.cfg.Home, team.DirReplicas, j.Remote.Project.ID+".db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{`SELECT json FROM items`, `SELECT json FROM comments`, `SELECT json FROM leases`, `SELECT json FROM mail`} {
		rows, err := db.QueryContext(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var js string
			_ = rows.Scan(&js)
			if strings.Contains(js, "HIDDEN") || strings.Contains(js, bil.ID) {
				t.Fatalf("%s: guest copy holds %s", q, js)
			}
		}
		rows.Close()
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM items`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("guest copy items %d %v", n, err)
	}
}

// The authority's row in GET /v0/work/sources counts the copy's items (the
// issues home a cockpit shows): the whole project for a project-level member, only
// its area for an area guest, whose copy holds nothing else.
func TestSourceRowCountsTheCopy(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	for _, x := range []struct{ title, area string }{{"one", "solver"}, {"two", "solver"}, {"three", "billing"}} {
		mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: x.title, Area: x.area}))(t)
	}
	m, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	g, _ := replica(t, tn, a, "vida-mac", "nVIDA", "harbor/solver", "vida", "guest")
	row := func(d *daemon) orchapi.WorkSource {
		ss := mustf(orchclient.WorkSources(ctx, d.owner, ""))(t)
		for _, s := range ss {
			if s.Name == "authority" {
				return s
			}
		}
		t.Fatalf("no authority row in %+v", ss)
		return orchapi.WorkSource{}
	}
	eventually(t, 3*time.Second, func() error {
		if s := row(m); s.Sync == nil || s.Sync.Items != 3 {
			return fmt.Errorf("member row %+v", s.Sync)
		}
		return nil
	})
	eventually(t, 3*time.Second, func() error {
		if s := row(g); s.Sync == nil || s.Sync.Items != 2 {
			return fmt.Errorf("guest row %+v", s.Sync)
		}
		return nil
	})
}

// listItems filters by status in SQL, on the copy's column. A queued status
// write is not in that column yet: the list must still go by what it says.
func TestReplicaListStatusSeesQueuedWrites(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	opened := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "was open", Area: "solver"}))(t)
	done := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "was done", Area: "solver"}))(t)
	mustf(orchclient.SetStatus(ctx, ao, done.ID, orchapi.StatusRequest{Status: orchapi.WorkDone}))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	cs := register(t, r, "harbor/solver", "bea-sess-1")
	rs := at(r.owner, "", cs.Address)
	listed := func(s orchapi.WorkStatus) string {
		var out []string
		for _, it := range mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{Status: s}))(t).Items {
			out = append(out, it.ID)
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}
	eventually(t, 2*time.Second, func() error {
		if got := listed(orchapi.WorkDone); got != done.ID {
			return fmt.Errorf("done items %q", got)
		}
		return nil
	})
	tn.Cut("potluck-bea-mac", "potluck-mini", true)
	waitReachable(t, r, false)
	mustf(orchclient.SetStatus(ctx, rs, opened.ID, orchapi.StatusRequest{Status: orchapi.WorkDone}))(t)
	mustf(orchclient.SetStatus(ctx, rs, done.ID, orchapi.StatusRequest{Status: orchapi.WorkOpen}))(t)
	if got := listed(orchapi.WorkDone); got != opened.ID {
		t.Fatalf("done, with queued writes: %q, want %s", got, opened.ID)
	}
	if got := listed(orchapi.WorkOpen); got != done.ID {
		t.Fatalf("open, with queued writes: %q, want %s", got, done.ID)
	}
	tn.Cut("potluck-bea-mac", "potluck-mini", false)
	waitDrained(t, r)
}

// Search takes a cheap first pass over the raw JSON only for text JSON keeps
// as written; anything it escapes still has to be found.
func TestReplicaSearchFindsEscapedText(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	ao := at(a.owner, "harbor", "")
	it := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: `R&D "quoted" <plan>`, Body: "path C:\\temp\nnext line, café\u2028end", Area: "solver"}))(t)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	eventually(t, 2*time.Second, func() error {
		if wl := mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{}))(t); len(wl.Items) != 1 {
			return fmt.Errorf("copy has %d items", len(wl.Items))
		}
		return nil
	})
	for _, q := range []string{"r&d", `"quoted"`, "<plan>", `C:\temp`, "temp\nnext", "CAF", "café", "\u2028end", "plan", strings.TrimPrefix(it.ID, "local:")} {
		wl := mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{Q: q}))(t)
		if len(wl.Items) != 1 {
			t.Errorf("q %q found %d items", q, len(wl.Items))
		}
	}
	if wl := mustf(orchclient.WorkList(ctx, r.owner, orchapi.WorkQuery{Q: "no such text"}))(t); len(wl.Items) != 0 {
		t.Errorf("found %d items for nothing", len(wl.Items))
	}
}

// The authority's off-site backup reaches a member's replica with its next
// sync answer and shows on the authority row (a cockpit's "backed up 2m ago"); an
// area guest's row does not carry it. Health on the authority's socket has it.
func TestSourceRowCarriesTheAuthorityBackup(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	m, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	g, _ := replica(t, tn, a, "vida-mac", "nVIDA", "harbor/solver", "vida", "guest")
	mustf(orchclient.SetBackup(ctx, a.owner, orchapi.OffsiteRequest{Target: "file://" + t.TempDir(), Home: "team-home", RunNow: true}))(t)
	h := mustf(orchclient.Health(ctx, a.owner))(t)
	if h.Backup == nil || h.Backup.LastOK == nil || h.Backup.Kind != "file" {
		t.Fatalf("health backup %+v", h.Backup)
	}
	ao := at(a.owner, "harbor", "")
	mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "wake the sync", Area: "solver"}))(t) // a change answers the long poll
	row := func(d *daemon) orchapi.WorkSource {
		for _, s := range mustf(orchclient.WorkSources(ctx, d.owner, ""))(t) {
			if s.Name == "authority" {
				return s
			}
		}
		t.Fatal("no authority row")
		return orchapi.WorkSource{}
	}
	eventually(t, 10*time.Second, func() error {
		if b := row(m).Backup; b == nil || b.LastOK == nil || !b.LastOK.Equal(*h.Backup.LastOK) {
			return fmt.Errorf("member row backup %+v", b)
		}
		return nil
	})
	if b := row(g).Backup; b != nil {
		t.Fatalf("an area guest got the backup: %+v", b)
	}
}

// A refused write whose caller was told must not come back after a power
// cut as a "queued claim conflict" (needs_human) for a conflict that caller
// already saw. The caller is told but its row's delete does not happen (it
// commits before the answer; here it is skipped) and what writeApply
// committed is lost; then the outbox drains again.
func TestRefusalToldSurvivesPowerCut(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	holder := register(t, r, "harbor/solver", "cut-holder")
	other := register(t, r, "harbor/solver", "cut-other")
	heartbeat(t, r)
	it := mustf(orchclient.CreateWork(ctx, at(r.owner, "harbor", holder.Address), orchapi.CreateWorkRequest{Title: "contested", Area: "solver"}))(t)
	eventually(t, 10*time.Second, func() error { _, err := orchclient.GetWork(ctx, r.owner, it.ID); return err })
	mustf(orchclient.Claim(ctx, at(r.owner, "", holder.Address), it.ID, orchapi.ClaimRequest{}))(t)

	undo := team.PowerCutAfterAnswer()
	if _, err := orchclient.Claim(ctx, at(r.owner, "", other.Address), it.ID, orchapi.ClaimRequest{}); !api.IsCode(err, api.CodeConflict) {
		undo()
		t.Fatalf("the second claim: %v", err)
	}
	undo() // back up: the outbox drains what it still holds as pending
	waitDrained(t, r)
	time.Sleep(300 * time.Millisecond)
	if got := mustf(orchclient.Session(ctx, r.owner, other.Address))(t); got.State == orchapi.StateNeedsHuman {
		t.Fatalf("a conflict the caller was told came back as a queued conflict: %+v", got)
	}
}
