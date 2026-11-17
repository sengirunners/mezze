package approvals_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/server"
)

func reopenDecisions(t *testing.T, h *harness) {
	t.Helper()
	cfg := h.dm.Deps.Config
	h.dm.Close()
	dm, err := server.New(context.Background(), cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	h.dm = dm
	h.owner = client.WithHandler(dm.SocketHandler())
	for _, m := range dm.Modules() {
		if svc, ok := m.(*approvals.Service); ok {
			h.svc = svc
			svc.SetClock(h.clock.now)
			svc.Publisher = h.pub
		}
	}
}

// Every sink has committed, but no completion marker survived. Reopen the
// actual store, then prove that replay reaches each sink without duplicating it.
func TestEffectSinkReplayAfterRestart(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("all effects")
	agent, _ := h.session("abab0010")
	holder, _ := h.session("abab0011")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	must(orchclient.Claim(ctx, holder, it.ID, orchapi.ClaimRequest{}))
	must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a"))
	reg := h.dm.Deps.Store.Registry()
	if _, err := reg.ExecContext(ctx, `UPDATE decision_effects SET state='pending' WHERE ask=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.ExecContext(ctx, `UPDATE decisions SET recorded=NULL,mailed=NULL,next_try=1 WHERE ask=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	reopenDecisions(t, h)
	h.svc.RetryDeliveries(ctx)
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Status != orchapi.AskRecorded || len(got.Decision.Deliveries) != 4 {
		t.Fatalf("delivery: %+v", got)
	}
	if n := len(must(orchclient.Thread(ctx, h.owner, a.ID))); n != 2 {
		t.Fatalf("thread entries: %d", n)
	}
	if n := len(h.comments(it.ID)); n != 1 {
		t.Fatalf("comments: %d", n)
	}
	count := 0
	var target string
	if err := h.dm.Deps.Store.Registry().QueryRowContext(ctx, `SELECT target FROM decision_effects WHERE ask=? AND kind='holder'`, a.ID).Scan(&target); err != nil {
		t.Fatal(err)
	}
	count = 0
	for _, m := range must(orchclient.Mail(ctx, h.owner, orchapi.MailQuery{})) {
		if m.To == target && strings.Contains(m.Body, "OWNER DECISION") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("holder notices: %d", count)
	}
	count = 0
	for _, e := range h.events(orchapi.EventDecisionRecorded) {
		if strings.Contains(string(e.Data), a.ID) {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("events: %d", count)
	}
}

// A failed original reply must retry after comment success; the DTO must tell
// the app that work is recorded while notifications are pending.
func TestReplyFailureDoesNotLoseIndependentEffects(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("retry original reply")
	agent, addr := h.session("abab0020")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	db := must(h.dm.Deps.Store.Project(authz.All(), authz.Write, a.Project))
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_reply BEFORE INSERT ON mail BEGIN SELECT RAISE(ABORT,'reply unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	d := must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a"))
	if d.MailError == "" || d.RecordedAt != nil || len(h.comments(it.ID)) != 1 {
		t.Fatalf("partial: %+v", d)
	}
	states := map[string]string{}
	for _, e := range d.Deliveries {
		states[e.Kind] = e.State
	}
	if states["comment"] != "succeeded" || states["reply"] != "pending" || states["event"] != "succeeded" {
		t.Fatalf("states: %+v", states)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_reply`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.dm.Deps.Store.Registry().ExecContext(ctx, `UPDATE decisions SET next_try=1 WHERE ask=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	reopenDecisions(t, h)
	h.svc.RetryDeliveries(ctx)
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Status != orchapi.AskRecorded || got.Decision.MailError != "" {
		t.Fatalf("retry: %+v", got)
	}
	if n := len(must(orchclient.Thread(ctx, h.owner, a.ID))); n != 2 {
		t.Fatalf("thread: %d", n)
	}
	if n := len(h.comments(it.ID)); n != 1 {
		t.Fatalf("comments: %d", n)
	}
	if st := must(orchclient.Session(ctx, h.owner, addr)); st.State == orchapi.StateNeedsHuman {
		t.Fatal("original asker still needs_human")
	}
}

func TestUpgradePreservesReceiptsAndStopsAmbiguousLegacyDelivery(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, _ := h.session("abab0030")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card()}))
	d := must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a"))
	reg := h.dm.Deps.Store.Registry()
	// Restore only the old schema in this temporary home, retaining the receipt
	// and the already committed reply but losing legacy delivery completion.
	for _, q := range []string{
		`UPDATE decisions SET recorded=NULL,next_try=1`,
		`DROP TABLE decision_effects`,
		`ALTER TABLE decisions DROP COLUMN mail_error`,
		`ALTER TABLE decisions DROP COLUMN mailed`,
		`DROP INDEX asks_work`,
		`DROP INDEX asks_once`,
		`CREATE UNIQUE INDEX asks_once ON asks(project,dedupe) WHERE dedupe!=''`,
		`DELETE FROM schema_migrations WHERE name='approvals/0003_effects'`,
	} {
		if _, err := reg.ExecContext(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	reopenDecisions(t, h)
	h.svc.RetryDeliveries(ctx)
	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if got.Decision.Receipt.Sig != d.Receipt.Sig || got.Decision.RecordedAt != nil || !strings.Contains(got.Decision.WriteError, "unknown:") {
		t.Fatalf("upgrade: %+v", got)
	}
	if n := len(must(orchclient.Thread(ctx, h.owner, a.ID))); n != 2 {
		t.Fatalf("upgrade replayed legacy reply: %d", n)
	}
}

// Holder delivery is bound when the decision commits. A later claim cannot
// retarget a retry, and a failed event must not be lost behind a good comment.
func TestCapturedHolderAndEventRetryIndependently(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("captured holder")
	agent, _ := h.session("abab0040")
	holder, holderAddr := h.session("abab0041")
	next, nextAddr := h.session("abab0042")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	must(orchclient.Claim(ctx, holder, it.ID, orchapi.ClaimRequest{}))
	db := must(h.dm.Deps.Store.Project(authz.All(), authz.Write, a.Project))
	// Fail the notice (not the original reply) and the decision event separately.
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_holder BEFORE INSERT ON mail WHEN NEW.to_addr='`+holderAddr+`' BEGIN SELECT RAISE(ABORT,'holder unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_event BEFORE INSERT ON events WHEN NEW.type='`+orchapi.EventDecisionRecorded+`' BEGIN SELECT RAISE(ABORT,'event unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	d := must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a"))
	if d.RecordedAt != nil || d.MailError == "" || d.WriteError == "" {
		t.Fatalf("partial delivery: %+v", d)
	}
	must(orchclient.Release(ctx, holder, it.ID, orchapi.ReleaseRequest{}))
	must(orchclient.Claim(ctx, next, it.ID, orchapi.ClaimRequest{}))
	for _, q := range []string{`DROP TRIGGER fail_holder`, `DROP TRIGGER fail_event`} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.dm.Deps.Store.Registry().ExecContext(ctx, `UPDATE decisions SET next_try=1 WHERE ask=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	reopenDecisions(t, h)
	h.svc.RetryDeliveries(ctx)
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Status != orchapi.AskRecorded {
		t.Fatalf("retry: %+v", got)
	}
	counts := map[string]int{}
	for _, m := range must(orchclient.Mail(ctx, h.owner, orchapi.MailQuery{})) {
		if strings.Contains(m.Body, "OWNER DECISION") {
			counts[m.To]++
		}
	}
	if counts[holderAddr] != 1 || counts[nextAddr] != 0 {
		t.Fatalf("retargeted notice: %+v", counts)
	}
	count := 0
	for _, e := range h.events(orchapi.EventDecisionRecorded) {
		if strings.Contains(string(e.Data), a.ID) {
			count++
		}
	}
	if count != 1 || len(h.comments(it.ID)) != 1 || len(must(orchclient.Thread(ctx, h.owner, a.ID))) != 2 {
		t.Fatalf("events=%d comments=%d", count, len(h.comments(it.ID)))
	}
}

// Unsupported sinks are honest about unknown delivery and never turn a signed
// receipt into a claim of work recording or a background write replay.
type unknownRecorder struct {
	orchcore.WorkService
	recorder orchcore.WorkRecorder
}

func (w unknownRecorder) ResolveItem(ctx context.Context, sub *authz.Subject, project, id string) (string, string, string, error) {
	return w.recorder.ResolveItem(ctx, sub, project, id)
}
func (w unknownRecorder) WaitingItems(ctx context.Context, sub *authz.Subject, scope authz.Scope, f orchapi.WaitingFilter) (orchapi.WorkPage, error) {
	return w.recorder.WaitingItems(ctx, sub, scope, f)
}
func (w unknownRecorder) RecordComment(context.Context, *authz.Subject, string, string, string, string) error {
	return api.Errorf(api.CodeNotImplemented, "unknown: external sink has no supported delivery idempotency")
}

func TestUnknownEffectSurvivesRestartWithoutReplay(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	it := h.workItem("unsupported sink")
	agent, _ := h.session("abab0050")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Subject: orchapi.AskSubject{Work: it.ID}, Card: card()}))
	work := orchcore.Work(h.dm.Deps)
	orchcore.Wire(h.dm.Deps).Work = unknownRecorder{WorkService: work, recorder: work.(orchcore.WorkRecorder)}
	d := must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a"))
	if d.RecordedAt != nil || !strings.Contains(d.WriteError, "unknown:") {
		t.Fatalf("unknown: %+v", d)
	}
	orchcore.Wire(h.dm.Deps).Work = work
	reopenDecisions(t, h)
	h.svc.RetryDeliveries(ctx)
	got := must(orchclient.GetAsk(ctx, h.owner, a.ID))
	if got.Status != orchapi.AskAnswered || got.Decision.Receipt.Sig != d.Receipt.Sig || !strings.Contains(got.Decision.WriteError, "unknown:") || len(h.comments(it.ID)) != 0 {
		t.Fatalf("replayed unknown: %+v", got)
	}
}

func TestReplyAckFailureRetriesWithoutSendingAgain(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, addr := h.session("abab0060")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card()}))
	db := must(h.dm.Deps.Store.Project(authz.All(), authz.Write, a.Project))
	if _, err := db.ExecContext(ctx, `CREATE TRIGGER fail_ack BEFORE UPDATE OF acked_at ON mail BEGIN SELECT RAISE(ABORT,'ack unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	d := must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a"))
	if d.RecordedAt != nil || d.MailError == "" || len(must(orchclient.Thread(ctx, h.owner, a.ID))) != 2 {
		t.Fatalf("ack failure not visible: %+v", d)
	}
	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_ack`); err != nil {
		t.Fatal(err)
	}
	if _, err := h.dm.Deps.Store.Registry().ExecContext(ctx, `UPDATE decisions SET next_try=1 WHERE ask=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	reopenDecisions(t, h)
	h.svc.RetryDeliveries(ctx)
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Status != orchapi.AskRecorded {
		t.Fatalf("retry: %+v", got)
	}
	if len(must(orchclient.Thread(ctx, h.owner, a.ID))) != 2 {
		t.Fatal("ack retry duplicated reply")
	}
	if st := must(orchclient.Session(ctx, h.owner, addr)); st.State == orchapi.StateNeedsHuman {
		t.Fatal("asker still needs_human")
	}
}

func TestReplyRepairsCrashAfterAckBeforeSessionUpdate(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, addr := h.session("abab0070")
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: card()}))
	must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a"))
	// Ack is durably committed; death lost only the session transition and
	// the original reply effect's completion marker.
	must(orchclient.SetState(ctx, h.owner, addr, orchapi.StateRequest{State: orchapi.StateNeedsHuman}))
	if _, err := h.dm.Deps.Store.Registry().ExecContext(ctx, `UPDATE decision_effects SET state='pending' WHERE ask=? AND kind='reply'`, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.dm.Deps.Store.Registry().ExecContext(ctx, `UPDATE decisions SET recorded=NULL,next_try=1 WHERE ask=?`, a.ID); err != nil {
		t.Fatal(err)
	}
	reopenDecisions(t, h)
	h.svc.RetryDeliveries(ctx)
	if st := must(orchclient.Session(ctx, h.owner, addr)); st.State == orchapi.StateNeedsHuman {
		t.Fatal("ack replay lost the original asker state transition")
	}
	if n := len(must(orchclient.Thread(ctx, h.owner, a.ID))); n != 2 {
		t.Fatalf("state repair sent another reply: %d", n)
	}
}

func TestUnavailableReceiptKeyRefusesAskInsteadOfFallback(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	path := filepath.Join(h.dm.Deps.Config.Home, approvals.KeyFile)
	if err := os.WriteFile(path, []byte("invalid key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.NewAsk(ctx, h.ownerIn("harbor/solver", ""), orchapi.NewAskRequest{Card: card()}); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("invalid key: %v", err)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "invalid key\n" {
		t.Fatalf("key was replaced: %q %v", b, err)
	}
}

// ---- asks that carry an action (ack-mail) ----

// actionCard is card() whose choice on acks ids.
func actionCard(on string, ids ...string) *orchapi.AskCard {
	c := card()
	c.Action = &orchapi.AskAction{Kind: orchapi.ActionAckMail, IDs: ids, On: on}
	return c
}

// humanMails has a second session send n needs=human mails to the agent at to.
func (h *harness) humanMails(to string, n int) []orchapi.Mail {
	h.t.Helper()
	from, _ := h.session("cdcd" + to[len(to)-4:])
	var out []orchapi.Mail
	for i := 0; i < n; i++ {
		out = append(out, must(orchclient.Send(context.Background(), from, orchapi.SendMailRequest{Kind: orchapi.MailKindReport, To: to, Needs: orchapi.NeedsHuman,
			Body: fmt.Sprintf("stale lease note %d\nmore", i)})))
	}
	return out
}

func (h *harness) takenBy(id string) string {
	h.t.Helper()
	m := must(orchclient.GetMail(context.Background(), h.owner, id))
	if m.AckedAt == nil {
		return ""
	}
	return m.AckedBy
}

func (h *harness) ackEvents(id string) int {
	n := 0
	for _, e := range h.events(orchapi.EventMailAcked) {
		if strings.Contains(string(e.Data), id) {
			n++
		}
	}
	return n
}

func TestAckActionApprovedAcksExactlyTheFrozenMail(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, addr := h.session("abab0030")
	ms := h.humanMails(addr, 3)
	// The rule stands: an agent cannot take needs=human mail itself.
	if _, err := orchclient.Ack(ctx, agent, ms[0].ID); code(err) != api.CodeForbidden {
		t.Fatalf("agent acked needs=human mail: %v", err)
	}
	// A short id resolves; a repeat is dropped. The third mail is not named.
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: actionCard("", ms[0].ID[len(ms[0].ID)-8:], ms[1].ID, ms[1].ID)}))
	act := a.Card.Action
	if act == nil || act.Count != 2 || act.On != "a" || act.IDs[0] != ms[0].ID || act.IDs[1] != ms[1].ID {
		t.Fatalf("frozen action: %+v", act)
	}
	if len(act.Samples) != 2 || act.Samples[0] != "stale lease note 0" || !strings.Contains(a.Body, "acks 2 mails as you") {
		t.Fatalf("card shows: %+v\n%s", act.Samples, a.Body)
	}
	d := must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a"))
	if !strings.Contains(d.Receipt.Action, "ack-mail count=2 ") || !strings.HasSuffix(d.Receipt.Action, " on=a") {
		t.Fatalf("receipt action: %q", d.Receipt.Action)
	}
	if v := approvals.Verify(d.Receipt, must(h.svc.PublicKey()), orchapi.ReceiptExpect{Ask: a.ID}); !v.Valid {
		t.Fatalf("receipt: %+v", v)
	}
	states := map[string]string{}
	for _, e := range d.Deliveries {
		states[e.Kind] = e.State
	}
	if states["ack"] != "succeeded" {
		t.Fatalf("deliveries: %+v", d.Deliveries)
	}
	for i, want := range []string{"human:ivo", "human:ivo", ""} {
		if got := h.takenBy(ms[i].ID); got != want {
			t.Errorf("mail %d taken by %q, want %q", i, got, want)
		}
	}
	// Replay and a second delivery ack nothing twice.
	reg := h.dm.Deps.Store.Registry()
	for range 2 {
		if _, err := reg.ExecContext(ctx, `UPDATE decision_effects SET state='pending' WHERE ask=?`, a.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := reg.ExecContext(ctx, `UPDATE decisions SET recorded=NULL,next_try=1 WHERE ask=?`, a.ID); err != nil {
			t.Fatal(err)
		}
		h.svc.RetryDeliveries(ctx)
	}
	if got := must(orchclient.GetAsk(ctx, h.owner, a.ID)); got.Status != orchapi.AskRecorded {
		t.Errorf("after replay: %s", got.Status)
	}
	for i := range 2 {
		if n := h.ackEvents(ms[i].ID); n != 1 {
			t.Errorf("mail %d acked %d times", i, n)
		}
	}
	// The same answer again returns the same decision and runs nothing new.
	if again := must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "a")); again.Receipt.Sig != d.Receipt.Sig {
		t.Error("a repeated answer signed a new receipt")
	}
	if h.takenBy(ms[2].ID) != "" {
		t.Error("an unnamed mail was acked")
	}
}

func TestAckActionDeclineLeavesMailUntouched(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	h.pair("ivo", ipPhone)
	agent, addr := h.session("abab0031")
	ms := h.humanMails(addr, 2)
	a := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: actionCard("a", ms[0].ID, ms[1].ID)}))
	d := must(orchclient.Decide(ctx, h.device(ipPhone), a.ID, "b"))
	if !strings.HasPrefix(d.Receipt.Action, "ack-mail count=2 ") {
		t.Errorf("a decline's receipt still names the action: %q", d.Receipt.Action)
	}
	for _, e := range d.Deliveries {
		if e.Kind == "ack" {
			t.Errorf("a decline has an ack effect: %+v", d.Deliveries)
		}
	}
	// A free-text answer runs nothing either.
	b := must(orchclient.NewAsk(ctx, agent, orchapi.NewAskRequest{Card: actionCard("a", ms[0].ID), Topic: "free-text"}))
	must(orchclient.DecideWith(ctx, h.device(ipPhone), b.ID, orchapi.DecideRequest{Text: "not yet"}))
	for _, m := range ms {
		if got := h.takenBy(m.ID); got != "" {
			t.Errorf("mail %s taken by %s", m.ID, got)
		}
	}
}
