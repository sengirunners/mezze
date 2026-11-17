package approvals_test

import (
	"context"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
)

// A second process can omit session attribution on the owner socket. It must
// not invalidate the one-time code already displayed to the human.
func TestRereviewUnattributedSocketCannotReplaceLiveCode(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	human := must(orchclient.AddApprover(ctx, h.owner, "ivo"))
	anotherProcess := client.WithHandler(h.dm.SocketHandler())
	_, err := orchclient.AddApprover(ctx, anotherProcess, "ivo")
	if !api.IsCode(err, api.CodeConflict) {
		t.Errorf("second socket process replaced live code: %v", err)
	}
	if _, err := orchclient.Pair(ctx, h.device(ipPhone), human.Code); err != nil {
		t.Errorf("original human code was invalidated: %v", err)
	}
}

// Unlike the original regression, reopen the native store after removing the
// work cache as well as all delivery markers, retaining only sink commits.
func TestRereviewNativeAllEffectsReplayAfterRestart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	owner := h.ownerIn("harbor/solver", "")
	res, err := orchclient.AddTracker(ctx, owner, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: "issue", Area: "solver", WaitSeconds: 10})
	if err != nil || !res.OK {
		t.Fatalf("tracker: %+v %v", res, err)
	}
	it := must(orchclient.CreateIssue(ctx, owner, orchapi.NewIssueRequest{Title: "native restart"}))
	a := must(orchclient.NewAsk(ctx, owner, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: "issue:" + it.ID}, Card: card()}))
	before := must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a"))
	db := must(h.dm.Deps.Store.Project(authz.All(), authz.Write, a.Project))
	if _, err := db.ExecContext(ctx, `DELETE FROM work_comments WHERE item=?`, a.Subject.Work); err != nil {
		t.Fatal(err)
	}
	reg := h.dm.Deps.Store.Registry()
	if _, err := reg.ExecContext(ctx, `UPDATE decision_effects SET state='pending' WHERE ask=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.ExecContext(ctx, `UPDATE decisions SET recorded=NULL,mailed=NULL,next_try=1 WHERE ask=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	reopenDecisions(t, h)
	h.svc.RetryDeliveries(ctx)
	owner = h.ownerIn("harbor/solver", "")
	got := must(orchclient.GetAsk(ctx, owner, a.ID))
	if got.Status != orchapi.AskRecorded || got.Decision.Receipt.Sig != before.Receipt.Sig {
		t.Fatalf("replay: %+v", got)
	}
	if n := len(must(orchclient.IssueComments(ctx, owner, it.ID))); n != 1 {
		t.Errorf("native comments=%d", n)
	}
	// The owner asked and decided: nobody is told what they just did (SPEC 9
	// "No notice about what you just did"); the replay takes the ask's mail.
	th := must(orchclient.Thread(ctx, owner, a.ID))
	if len(th) != 1 || th[0].AckedAt == nil {
		t.Errorf("thread %+v", th)
	}
}

// The migration must preserve existing decision slots when introducing the
// authority namespace, rather than leave old keys outside the new namespace.
func TestRereviewUpgradePreservesExistingDecisionSlot(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	owner := h.ownerIn("harbor/solver", "")
	a := must(orchclient.NewAsk(ctx, owner, orchapi.NewAskRequest{Card: card(), Topic: "existing-topic"}))
	reg := h.dm.Deps.Store.Registry()
	for _, q := range []string{
		`UPDATE asks SET dedupe='topic:existing-topic'`,
		`DROP TABLE decision_effects`,
		`ALTER TABLE decisions DROP COLUMN mail_error`,
		`ALTER TABLE decisions DROP COLUMN mailed`,
		`DROP INDEX asks_work`,
		`DROP INDEX asks_once`,
		`CREATE UNIQUE INDEX asks_once ON asks(project,dedupe) WHERE dedupe!=''`,
		`DELETE FROM schema_migrations WHERE name='approvals/0003_effects'`,
	} {
		if _, err := reg.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	reopenDecisions(t, h)
	owner = h.ownerIn("harbor/solver", "")
	if _, err := orchclient.NewAsk(ctx, owner, orchapi.NewAskRequest{Card: card(), Topic: "existing-topic"}); !api.IsCode(err, api.CodeConflict) {
		t.Errorf("legacy ask %s no longer reserves its decision slot: %v", a.ID, err)
	}
}
