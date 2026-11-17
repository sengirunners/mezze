package team_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// movedFixture: an issue moved from harbor to zm, a replica in both projects,
// and one in harbor only.
type movedFixture struct {
	a          *daemon
	ao, az, rc *client.Client // the authority's harbor and zm; the replica's harbor
	r, only    *daemon
	oldID, nid string // issue ids
}

func newMovedFixture(t *testing.T) movedFixture {
	t.Helper()
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "zm"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "zm"))(t)
	ao, az := at(a.owner, "harbor", ""), at(a.owner, "zm", "")
	for slug, c := range map[string]*client.Client{"harbor": ao, "zm": az} {
		mustf(orchclient.SetupIssues(ctx, c, slug))(t)
		if res, err := orchclient.AddTracker(ctx, c, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "bd", WaitSeconds: 10}); err != nil || !res.OK {
			t.Fatalf("add native tracker: %+v %v", res, err)
		}
	}
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "zm", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	only, _ := replica(t, tn, a, "cyd-mac", "nCYD", "harbor", "cyd", "member")
	it := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "zm: moves"}))(t)
	res := mustf(orchclient.MoveIssues(ctx, ao, orchapi.IssueMoveRequest{To: "zm", IDs: []string{it.ID}}))(t)
	if res.Moved != 1 {
		t.Fatalf("move %+v", res)
	}
	f := movedFixture{a: a, ao: ao, az: az, rc: at(r.owner, "harbor", ""), r: r, only: only, oldID: it.ID, nid: res.Items[0].NewID}
	var moveID string
	if err := ao.Events(ctx, api.EventQuery{Project: "harbor", NoLive: true, Since: "0", Types: orchapi.EventWorkMoved}, func(e api.Event) error {
		if e.Subject == "bd:"+it.ID {
			moveID = e.ID
		}
		return nil
	}); err != nil || moveID == "" {
		t.Fatalf("move event: id %q, error %v", moveID, err)
	}
	// A copy is caught up only once its snapshot or changelog passed the move.
	for _, d := range []*daemon{r, only} {
		deadline := time.Now().Add(15 * time.Second)
		for teamService(t, d).CopyCursor("harbor") < moveID {
			if time.Now().After(deadline) {
				t.Fatalf("%s's copy did not reach move %s", d.cfg.Machine, moveID)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if teamService(t, d).CopyHolds(ctx, "bd:"+it.ID) {
			t.Fatalf("%s's copy kept the old id", d.cfg.Machine)
		}
	}
	return f
}

// zm who, by the old id: through a replica with and without the old project
// named, and on the authority itself.
func TestMovedWorkReadsFollow(t *testing.T) {
	ctx := context.Background()
	f := newMovedFixture(t)
	old, nw := "bd:"+f.oldID, "bd:"+f.nid
	for name, c := range map[string]*client.Client{
		"replica, old project named": f.rc,
		"replica, no project":        f.r.owner,
		"authority, old project":     f.ao,
		"authority, no project":      f.a.owner,
	} {
		h, err := orchclient.Holder(ctx, c, old)
		if err != nil || h.ID != nw {
			t.Errorf("%s: holder by the old id: %+v, %v", name, h, err)
		}
	}
	if it := mustf(orchclient.GetWork(ctx, f.rc, old))(t); it.ID != nw {
		t.Fatalf("work get by the old id: %+v", it)
	}
}

// Hidden answers exactly like missing, whichever door asks.
func TestMovedWorkHiddenIsMissing(t *testing.T) {
	ctx := context.Background()
	f := newMovedFixture(t)
	old := "bd:" + f.oldID
	onlyc := at(f.only.owner, "harbor", "")
	_, hidden := orchclient.Holder(ctx, onlyc, old)
	_, missing := orchclient.Holder(ctx, onlyc, "bd:harbor-zzzzz")
	if !api.IsCode(hidden, api.CodeNotFound) || !api.IsCode(missing, api.CodeNotFound) {
		t.Fatalf("hidden %v, missing %v", hidden, missing)
	}
	if hm, mm := strings.ReplaceAll(hidden.Error(), f.oldID, "X"), strings.ReplaceAll(missing.Error(), "harbor-zzzzz", "X"); hm != mm {
		t.Fatalf("hidden and missing differ: %q / %q", hm, mm)
	}
	if _, err := orchclient.Send(ctx, onlyc, orchapi.SendMailRequest{To: old, Body: "hi"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("mail to what the replica cannot see: %v", err)
	}
	if _, err := orchclient.Holder(ctx, f.rc, "bd:zm-"+strings.TrimPrefix(f.oldID, "harbor-")+"x"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a missing new-looking id: %v", err)
	}
}

// zm send, by the old id: the mail is stored in the issue's project, under its
// new id, once.
func TestMovedWorkMailFollows(t *testing.T) {
	ctx := context.Background()
	f := newMovedFixture(t)
	old, nw := "bd:"+f.oldID, "bd:"+f.nid
	for name, c := range map[string]*client.Client{
		"replica, old project named": f.rc,
		"authority, old project":     f.ao,
	} {
		m, err := orchclient.Send(ctx, c, orchapi.SendMailRequest{To: old, Body: name})
		if err != nil {
			t.Fatalf("%s: send: %v", name, err)
		}
		if m.Ref != nw || m.To != nw {
			t.Errorf("%s: mail %+v, want to and ref %s", name, m, nw)
		}
	}
	all := mustf(orchclient.Mail(ctx, f.az, orchapi.MailQuery{Project: "zm"}))(t)
	if len(all) != 2 {
		b, _ := json.Marshal(all)
		t.Fatalf("mail in zm: %s", b)
	}
	if got := mustf(orchclient.Mail(ctx, f.ao, orchapi.MailQuery{Project: "harbor"}))(t); len(got) != 0 {
		t.Fatalf("mail stayed in harbor: %+v", got)
	}
	// A ref by the old id, too.
	m, err := orchclient.Send(ctx, f.rc, orchapi.SendMailRequest{To: "human:bea", Ref: old, Body: "about it"})
	if err != nil || m.Ref != nw {
		t.Fatalf("ref by the old id: %+v, %v", m, err)
	}
}

// Writes on the item are not followed: the id they name is the id they change.
func TestMovedWorkWritesDoNotFollow(t *testing.T) {
	ctx := context.Background()
	f := newMovedFixture(t)
	old := "bd:" + f.oldID
	for name, c := range map[string]*client.Client{"replica": f.rc, "authority": f.ao} {
		if _, err := orchclient.Claim(ctx, c, old, orchapi.ClaimRequest{}); !api.IsCode(err, api.CodeNotFound) {
			t.Errorf("%s: claim by the old id: %v", name, err)
		}
	}
}

// The move itself takes the old row out of the mirror: no window in which the
// old id names the old row at the authority, or a claim takes it.
func TestMovedWorkLeavesNoOldRow(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "zm"}))(t)
	ao, az := at(a.owner, "harbor", ""), at(a.owner, "zm", "")
	for slug, c := range map[string]*client.Client{"harbor": ao, "zm": az} {
		mustf(orchclient.SetupIssues(ctx, c, slug))(t)
		if res, err := orchclient.AddTracker(ctx, c, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "bd", WaitSeconds: 10}); err != nil || !res.OK {
			t.Fatalf("add native tracker: %+v %v", res, err)
		}
	}
	it := mustf(orchclient.CreateIssue(ctx, ao, orchapi.NewIssueRequest{Title: "moves"}))(t)
	mustf(orchclient.GetWork(ctx, ao, "bd:"+it.ID))(t) // it is in the mirror
	res := mustf(orchclient.MoveIssues(ctx, ao, orchapi.IssueMoveRequest{To: "zm", IDs: []string{it.ID}}))(t)
	nw := "bd:" + res.Items[0].NewID
	if h := mustf(orchclient.Holder(ctx, ao, "bd:"+it.ID))(t); h.ID != nw {
		t.Fatalf("holder answered the old row: %+v", h)
	}
	if m := mustf(orchclient.Send(ctx, ao, orchapi.SendMailRequest{To: "bd:" + it.ID, Body: "hi"}))(t); m.To != nw {
		t.Fatalf("mail to the old row: %+v", m)
	}
	if _, err := orchclient.Claim(ctx, ao, "bd:"+it.ID, orchapi.ClaimRequest{}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a claim on the old id: %v", err)
	}
	for _, w := range mustf(orchclient.Work(ctx, ao, orchapi.WorkQuery{Project: "harbor"}))(t) {
		if w.ID == "bd:"+it.ID {
			t.Fatalf("the old id is still listed: %+v", w)
		}
	}
}

// Only a native tracker's id follows, on the replica too.
func TestMovedWorkOtherSourcesDoNotFollow(t *testing.T) {
	ctx := context.Background()
	f := newMovedFixture(t)
	for _, id := range []string{"gh:" + f.oldID, "local:" + f.oldID} {
		if _, err := orchclient.Holder(ctx, f.rc, id); !api.IsCode(err, api.CodeNotFound) {
			t.Errorf("holder %s: %v", id, err)
		}
		if _, err := orchclient.Send(ctx, f.rc, orchapi.SendMailRequest{To: id, Body: "x"}); !api.IsCode(err, api.CodeNotFound) {
			t.Errorf("mail %s: %v", id, err)
		}
	}
}

// A ref follows when the recipient is a work id too; mail to everyone stays put.
func TestMovedWorkMailEdges(t *testing.T) {
	ctx := context.Background()
	f := newMovedFixture(t)
	old, nw := "bd:"+f.oldID, "bd:"+f.nid
	if m, err := orchclient.Send(ctx, f.rc, orchapi.SendMailRequest{To: nw, Ref: old, Body: "both"}); err != nil || m.Ref != nw {
		t.Fatalf("to the new id, ref the old: %+v, %v", m, err)
	}
	if m, err := orchclient.Send(ctx, f.ao, orchapi.SendMailRequest{To: nw, Ref: old, Body: "both", Area: ""}); err != nil || m.Ref != nw {
		t.Fatalf("at the authority: %+v, %v", m, err)
	}
	for name, c := range map[string]*client.Client{"replica": f.rc, "authority": f.ao} {
		if m, err := orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "all", Kind: orchapi.MailKindAnnounce, Ref: old, Body: "everyone"}); err == nil {
			t.Fatalf("%s: an announcement moved to the ref's project: %+v", name, m)
		} else if !api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// Mail to an item that lives in another joined project reaches it when the
// caller named no project; a project named with --project is taken at its word.
func TestWorkMailFindsItsProject(t *testing.T) {
	ctx := context.Background()
	f := newMovedFixture(t)
	nw := "bd:" + f.nid
	waitCopy(t, f.r, "zm", nw)
	if m, err := orchclient.Send(ctx, f.r.owner, orchapi.SendMailRequest{To: nw, Body: "no project named"}); err != nil || m.To != nw {
		t.Fatalf("no project named: %+v, %v", m, err)
	}
	if _, err := orchclient.Send(ctx, f.rc, orchapi.SendMailRequest{To: nw, Body: "the wrong project named"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("--project harbor on a zm item: %v", err)
	}
}

func waitCopy(t *testing.T, d *daemon, slug, id string) {
	t.Helper()
	c := at(d.owner, slug, "")
	for i := 0; i < 500; i++ {
		if _, err := orchclient.GetWork(context.Background(), c, id); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never reached %s's copy", id, slug)
}

// A send with a project that does not exist gets not_found, moved id or not.
func TestMovedWorkGhostProject(t *testing.T) {
	ctx := context.Background()
	f := newMovedFixture(t)
	for name, c := range map[string]*client.Client{"replica": f.r.owner, "authority": f.a.owner} {
		if _, err := orchclient.Send(ctx, at(c, "ghost", ""), orchapi.SendMailRequest{To: "bd:" + f.oldID, Body: "x"}); !api.IsCode(err, api.CodeNotFound) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// The old project's readers learn the issue left, never where it went, and a
// moved issue reaches the new project's copy even if a read got there first.
func TestMovedWorkEventAndNewCopy(t *testing.T) {
	ctx := context.Background()
	f := newMovedFixture(t)
	var all []api.Event
	if err := f.ao.Events(ctx, api.EventQuery{Project: "harbor", NoLive: true, Since: "0", Types: orchapi.EventWorkMoved}, func(e api.Event) error {
		all = append(all, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || strings.Contains(string(all[0].Data), f.nid) {
		t.Fatalf("moved event: %+v", all)
	}
	// A move then an immediate read: the copy still gets the new id.
	it := mustf(orchclient.CreateIssue(ctx, f.ao, orchapi.NewIssueRequest{Title: "read at once"}))(t)
	res := mustf(orchclient.MoveIssues(ctx, f.ao, orchapi.IssueMoveRequest{To: "zm", IDs: []string{it.ID}}))(t)
	nw := "bd:" + res.Items[0].NewID
	mustf(orchclient.Holder(ctx, f.rc, "bd:"+it.ID))(t)
	deadline := time.Now().Add(15 * time.Second)
	for !teamService(t, f.r).CopyHolds(ctx, nw) {
		if time.Now().After(deadline) {
			t.Fatalf("%s never reached the copy", nw)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Rename step 1: zm send by the old id with the project named under the
// Potluck-* headers (a later client's) follows the item like the Zm-* ones,
// and the project the replica names for the forward is the item's, under
// either name (the caller's Potluck-Project does not survive the rewrite).
func TestMovedWorkMailFollowsNewHeaderNames(t *testing.T) {
	ctx := context.Background()
	f := newMovedFixture(t)
	old, nw := "bd:"+f.oldID, "bd:"+f.nid
	b, _ := json.Marshal(orchapi.SendMailRequest{To: old, Body: "named by the new headers"})
	req := httptest.NewRequest(http.MethodPost, "/v0/mail", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(api.HeaderNew(api.HeaderProtocol), api.ProtocolNext)
	req.Header.Set(api.HeaderNew(api.HeaderProject), "harbor")
	req.Header.Set(api.HeaderNew(api.HeaderProjectFrom), "flag")
	rec := httptest.NewRecorder()
	f.r.dm.SocketHandler().ServeHTTP(rec, req)
	var m orchapi.Mail
	if rec.Code/100 != 2 || json.Unmarshal(rec.Body.Bytes(), &m) != nil || m.To != nw || m.Ref != nw {
		t.Fatalf("send by the old id: %d %s", rec.Code, rec.Body)
	}
	if all := mustf(orchclient.Mail(ctx, f.az, orchapi.MailQuery{Project: "zm"}))(t); len(all) != 1 {
		t.Fatalf("mail in zm: %+v", all)
	}
	if got := mustf(orchclient.Mail(ctx, f.ao, orchapi.MailQuery{Project: "harbor"}))(t); len(got) != 0 {
		t.Fatalf("mail stayed in harbor: %+v", got)
	}
}
