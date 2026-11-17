package approvals_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/server"
)

func TestIndependentReviewNativeCommentCrashWindow(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	owner := h.ownerIn("harbor/solver", "")
	res, err := orchclient.AddTracker(ctx, owner, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", Area: "solver", WaitSeconds: 10})
	if err != nil || !res.OK {
		t.Fatalf("native tracker: %+v %v", res, err)
	}
	it := must(orchclient.CreateIssue(ctx, owner, orchapi.NewIssueRequest{Title: "native crash"}))
	a := must(orchclient.NewAsk(ctx, owner, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: "issue:" + it.ID}, Card: card()}))
	must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a"))
	if n := len(must(orchclient.IssueComments(ctx, owner, it.ID))); n != 1 {
		t.Fatalf("seed comments %d", n)
	}
	// Model the native comment commit succeeding, followed by daemon death
	// before the separate work_comments cache and decision-delivery updates.
	db, err := h.dm.Deps.Store.Project(h.dm.Deps.Projects.Local("").Scope, authz.Read, a.Project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `DELETE FROM work_comments WHERE id=(SELECT id FROM work_comments WHERE item=? LIMIT 1)`, a.Subject.Work); err != nil {
		t.Fatal(err)
	}
	if _, err = h.dm.Deps.Store.Registry().ExecContext(ctx, `UPDATE decisions SET recorded=NULL,next_try=1 WHERE ask=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = h.dm.Deps.Store.Registry().ExecContext(ctx, `UPDATE decision_effects SET state='pending' WHERE ask=? AND kind='comment'`, a.ID); err != nil {
		t.Fatal(err)
	}
	h.svc.RetryDeliveries(ctx)
	if n := len(must(orchclient.IssueComments(ctx, owner, it.ID))); n != 1 {
		t.Errorf("native issue received %d comments for one decision", n)
	}
}

func TestIndependentReviewCrashDelivery(t *testing.T) {
	t.Run("original asker reply survives crash", func(t *testing.T) {
		h := newHarness(t)
		ctx := context.Background()
		h.pair("ivo", ipPhone)
		it := h.workItem("crash")
		agent, addr := h.session("aaaa0091")
		a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
		_, err := h.dm.Deps.Store.Registry().ExecContext(ctx, `INSERT INTO decisions (ask,decision,approver,node_id,time,receipt,text,answered_as,device,next_try) VALUES (?,'a','ivo','nPHONE',1,'{}','','owner','ivo-phone',1)`, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		cfg := h.dm.Deps.Config
		h.dm.Close()
		dm, err := server.New(ctx, cfg, nil, wiring.Full)
		if err != nil {
			t.Fatal(err)
		}
		defer dm.Close()
		h.owner = client.WithHandler(dm.SocketHandler())
		for _, m := range dm.Modules() {
			if svc, ok := m.(*approvals.Service); ok {
				h.svc = svc
				svc.SetClock(h.clock.now)
			}
		}
		h.svc.RetryDeliveries(ctx)
		if th := must(orchclient.Thread(ctx, h.owner, a.ID)); len(th) != 2 {
			t.Errorf("crash leaves %d thread entries, expected ask and answer", len(th))
		}
		if st := must(orchclient.Session(ctx, h.owner, addr)); st.State == orchapi.StateNeedsHuman {
			t.Error("asker still needs_human after successful retry")
		}
	})
	t.Run("holder mail is idempotent", func(t *testing.T) {
		h := newHarness(t)
		ctx := context.Background()
		h.pair("ivo", ipPhone)
		it := h.workItem("retry")
		agent, _ := h.session("aaaa0092")
		holder, _ := h.session("aaaa0093")
		a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
		must(orchclient.Claim(ctx, holder, it.ID, orchapi.ClaimRequest{}))
		must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a"))
		_, err := h.dm.Deps.Store.Registry().ExecContext(ctx, `UPDATE decisions SET recorded=NULL,next_try=1 WHERE ask=?`, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		h.svc.RetryDeliveries(ctx)
		n := 0
		for _, m := range must(orchclient.Inbox(ctx, holder, orchapi.InboxQuery{})) {
			if strings.Contains(m.Body, "OWNER DECISION") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("holder received %d decision mails for one answer", n)
		}
	})
}
