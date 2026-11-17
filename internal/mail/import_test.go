package mail

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/store"
)

func importArtifact(f *fixture) orchapi.LegacyMailImportRequest {
	sent := f.clock.Add(-48 * time.Hour)
	return orchapi.LegacyMailImportRequest{SourceInstance: "fixture-hub-installation", MappingVersion: "explicit-1", SourceIDs: []string{"18615"}, Mappings: []orchapi.LegacyMailMapping{{SourceProject: "ownerprivate", SourceRecipient: "personivo", Recipient: "human:ana"}}, Records: []orchapi.LegacyMailRecord{{Kind: "mail", ID: "18615", Project: "ownerprivate", Sender: "bea-mac/01a1a544", Recipient: "personivo", SentAt: sent, Body: strings.Repeat("invented fixture paragraph. ", 120), BodyComplete: true, Delivery: orchapi.LegacyDelivery{AckBy: "digest", AckAt: &sent}}}}
}

func TestLegacyImportRecoveryAudienceAndReplay(t *testing.T) {
	f := newFixture(t)
	owner := f.as("", "harbor")
	req := importArtifact(f)
	req.DryRun = true
	dry := must(orchclient.ImportLegacyMail(ctx, owner, req))
	if dry.Records[0].Status != "would_import" {
		t.Fatal(dry)
	}
	var n int
	must(0, f.d.Store.Registry().QueryRow(`SELECT count(*) FROM mail_imports`).Scan(&n))
	if n != 0 {
		t.Fatal("dry run reserved an import")
	}
	req.DryRun = false
	sub := f.d.Projects.Local("")
	ch, cancel := f.d.Events.Subscribe(sub, events.Filter{}, 8)
	defer cancel()
	hidden := f.d.Projects.Local("mini/11111111")
	hiddenCh, stop := f.d.Events.Subscribe(hidden, events.Filter{}, 8)
	defer stop()
	result := must(orchclient.ImportLegacyMail(ctx, owner, req))
	r := result.Records[0]
	if r.Status != "imported" || r.NativeID == "" {
		t.Fatal(result)
	}
	m := must(orchclient.GetMail(ctx, owner, r.NativeID))
	if m.AckedAt != nil || m.AckedBy != "" || m.Body != req.Records[0].Body || m.LegacyDelivery.State != "digest_held" || m.Origin.Trust != "source_reported" || m.Origin.Sender != "bea-mac/01a1a544" || m.From != "human:ana" || m.Origin.ImportedBy != "human:ana" || !m.Created.Equal(req.Records[0].SentAt) {
		t.Fatalf("import lost state: %+v", m)
	}
	barrier := importEventBarrier(t, f, "after-import")
	initial := importEventsThrough(t, ch, barrier)
	if len(initial) != 1 || initial[0].Type != orchapi.EventMailImported || initial[0].Subject != m.ID || initial[0].Project != m.Project {
		t.Fatalf("expected exactly one import event: %+v", initial)
	}
	if got := importEventsThrough(t, hiddenCh, barrier); len(got) != 0 {
		t.Fatalf("operator event leak %+v", got)
	}
	for _, c := range []*authz.Subject{hidden, {Identity: sub.Identity, Address: "human:other", Scope: authz.All()}} {
		if _, _, err := f.s.find(ctx, c.Scope, c.Address, m.ID); !api.IsCode(err, api.CodeNotFound) {
			t.Fatal("private by-id leaked", err)
		}
		if _, _, err := f.s.find(ctx, c.Scope, c.Address, m.ID[20:]); !api.IsCode(err, api.CodeNotFound) {
			t.Fatal("private suffix leaked", err)
		}
		rows := must(f.s.query(ctx, c.Scope, c.Address, 50, func(scoped) (string, []any, bool) { return "1=1", nil, true }))
		if len(rows) != 0 {
			t.Fatal("operator list leak", rows)
		}
		if err := f.s.Snapshot(ctx, c, f.harbor.ID, func(_ string, v any) error { t.Fatal("snapshot leak", v); return nil }); err != nil {
			t.Fatal(err)
		}
		replay := must(f.d.Events.Since(ctx, c, "", events.Filter{}, 50))
		if len(replay) != 0 {
			t.Fatal("event replay leaked", replay)
		}
	}
	_, err := orchclient.Reply(ctx, owner, m.ID, orchapi.ReplyRequest{Body: "invented reply"})
	wantCode(t, err, api.CodeConflict)
	_, err = orchclient.Send(ctx, owner, orchapi.SendMailRequest{To: "human:ana", ReplyTo: m.ID, Body: "invented alternate reply"})
	wantCode(t, err, api.CodeConflict)
	if n := f.sess.takeNudged(); len(n) != 0 {
		t.Fatal("import nudged", n)
	}
	must(orchclient.Ack(ctx, owner, m.ID))
	again := must(orchclient.ImportLegacyMail(ctx, owner, req))
	if again.Records[0].NativeID != m.ID || again.Records[0].Status != "already_present" {
		t.Fatal(again)
	}
	if must(orchclient.GetMail(ctx, owner, m.ID)).AckedAt == nil {
		t.Fatal("reimport resurrected unread")
	}
	req.Records[0].Body = "changed invented payload"
	conflict := must(orchclient.ImportLegacyMail(ctx, owner, req))
	if conflict.Records[0].Status != "conflict" {
		t.Fatal(conflict)
	}
	req = importArtifact(f)
	remap := must(orchclient.ImportLegacyMail(ctx, f.as("", "side"), req))
	if remap.Records[0].Status != "conflict" {
		t.Fatal("cross-project duplicate", remap)
	}
	// A barrier follows every completed retry/conflict, so an asynchronously
	// forwarded duplicate cannot hide behind an immediate empty-channel check.
	barrier = importEventBarrier(t, f, "after-retries")
	if got := importEventsThrough(t, ch, barrier); len(got) != 1 || got[0].Type != orchapi.EventMailAcked || got[0].Subject != m.ID || got[0].Project != m.Project {
		t.Fatalf("expected only explicit acknowledgement after retries: %+v", got)
	}
	if got := importEventsThrough(t, hiddenCh, barrier); len(got) != 0 {
		t.Fatalf("retry operator event leak %+v", got)
	}
	imports := must(f.d.Events.Since(ctx, sub, "", events.Filter{Types: []string{orchapi.EventMailImported}}, 50))
	if len(imports) != 1 || imports[0].ID != initial[0].ID || imports[0].Subject != m.ID {
		t.Fatalf("expected exactly one durable import event: %+v", imports)
	}
	// Disposable copies must not contain deleted payload bytes or event metadata.
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	dst := filepath.Join(home, "export.db")
	err = f.d.Store.CopyProject(ctx, f.harbor.ID, dst, func(ctx context.Context, db *sql.DB) error { return store.FilterExport(ctx, db, hidden.Address) })
	if err != nil {
		t.Fatal(err)
	}
	b := must(os.ReadFile(dst))
	if strings.Contains(string(b), "invented fixture paragraph") || strings.Contains(string(b), m.ID) || strings.Contains(string(b), "bea-mac/01a1a544") {
		t.Fatal("export leaked private bytes/metadata")
	}
}

func TestLegacyImportRejectsUnknownsAndBounds(t *testing.T) {
	f := newFixture(t)
	c := f.as("", "harbor")
	req := importArtifact(f)
	_, err := orchclient.ImportLegacyMail(ctx, f.as("mini/11111111", "harbor"), req)
	wantCode(t, err, api.CodeForbidden)
	_, err = orchclient.ImportLegacyMail(ctx, f.guest(), req)
	wantCode(t, err, api.CodeForbidden)
	req.DryRun = true
	req.Records[0].BodyComplete = false
	if got := must(orchclient.ImportLegacyMail(ctx, c, req)); got.Records[0].Status != "invalid" {
		t.Fatal(got)
	}
	req = importArtifact(f)
	req.DryRun = true
	req.Records[0].Recipient = "personunknown"
	if got := must(orchclient.ImportLegacyMail(ctx, c, req)); got.Records[0].Status != "unmapped" {
		t.Fatal(got)
	}
	req = importArtifact(f)
	req.SourceIDs = append(req.SourceIDs, "99999")
	if got := must(orchclient.ImportLegacyMail(ctx, c, req)); !got.Partial || got.Records[1].Status != "missing" {
		t.Fatal(got)
	}
	req = importArtifact(f)
	req.Mappings[0].Recipient = "human:bea"
	_, err = orchclient.ImportLegacyMail(ctx, c, req)
	wantCode(t, err, api.CodeBadRequest)
	req = importArtifact(f)
	req.Records[0].Body = strings.Repeat("x", MaxImportBodyBytes+1)
	if got := must(orchclient.ImportLegacyMail(ctx, c, req)); got.Records[0].Status != "invalid" {
		t.Fatal(got)
	}
}

func TestNativeMailPublicIdempotencyConcurrentAndReply(t *testing.T) {
	f := newFixture(t)
	must(f.d.Projects.Grant(ctx, f.d.Projects.Local(""), api.GrantRequest{Identity: "bea", Project: "harbor", Role: "member"}))
	c := f.as("", "harbor")
	req := orchapi.SendMailRequest{To: "human:bea", Body: "invented native conversation", Audience: orchapi.MailAudienceParticipants, ClientRequestID: "native-op-1"}
	const count = 8
	ids := make(chan string, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m, err := orchclient.Send(ctx, c, req); ids <- m.ID; errs <- err }()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatal("duplicate native mails", first, id)
		}
	}
	req.Body = "changed invented native conversation"
	_, err := orchclient.Send(ctx, c, req)
	wantCode(t, err, api.CodeConflict)
	parent := must(orchclient.GetMail(ctx, c, first))
	bea := &authz.Subject{Identity: api.Identity{Name: "bea", ID: "fixture-bea"}, Address: "human:bea", Scope: authz.All()}
	rr := orchapi.ReplyRequest{Body: "invented native answer", ClientRequestID: "reply-op-1"}
	a := must(f.s.Reply(ctx, bea, parent.Project, parent.ID, rr))
	b := must(f.s.Reply(ctx, bea, parent.Project, parent.ID, rr))
	if a.ID != b.ID || a.To != "human:ana" || a.Audience != orchapi.MailAudienceParticipants || a.From != "human:bea" {
		t.Fatal(a, b)
	}
	// Mail to a person is a for-you item: only ana's Got it marks it read,
	// never a reply (SPEC 9 "For you").
	if must(orchclient.GetMail(ctx, c, first)).AckedAt != nil {
		t.Fatal("a reply marked a for-you item read")
	}
	rr.Body = "changed answer"
	_, err = f.s.Reply(ctx, bea, parent.Project, parent.ID, rr)
	wantCode(t, err, api.CodeConflict)
	// A reply key cannot be reused to send a new operation or a different parent.
	_, err = f.s.Send(ctx, bea, api.ProjectRef{ID: parent.Project}, "", orchapi.SendMailRequest{To: "human:ana", Body: "invented native answer", ClientRequestID: "reply-op-1"})
	wantCode(t, err, api.CodeConflict)
	// Assert only the original native mail and one reply were durably stored.
	db := must(f.d.Store.Project(authz.All(), authz.Read, parent.Project))
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM mail`).Scan(&n); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	bts, _ := json.Marshal(a)
	if !strings.Contains(string(bts), `"audience":"participants"`) {
		t.Fatal(string(bts))
	}
}

func TestLegacyImportRecoversReservationAfterAtomicSinkFailure(t *testing.T) {
	f := newFixture(t)
	c := f.as("", "harbor")
	req := importArtifact(f)
	db := must(f.d.Store.Project(authz.All(), authz.Write, f.harbor.ID))
	must(db.Exec(`CREATE TRIGGER fail_import_sink BEFORE INSERT ON mail_import_sink BEGIN SELECT RAISE(ABORT,'fixture sink failure'); END`))
	_, err := orchclient.ImportLegacyMail(ctx, c, req)
	if err == nil {
		t.Fatal("expected synthetic sink failure")
	}
	var reserved string
	if err := f.d.Store.Registry().QueryRow(`SELECT mail_id FROM mail_imports`).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM mail`).Scan(&n); err != nil || n != 0 {
		t.Fatal("mail escaped rolled-back sink", n, err)
	}
	must(db.Exec(`DROP TRIGGER fail_import_sink`))
	// A fresh service has no in-memory import recovery state.
	f.s = New(f.d)
	orchcore.Wire(f.d).Mail = f.s
	got := must(f.s.ImportLegacy(ctx, f.d.Projects.Local(""), api.ProjectRef{ID: f.harbor.ID}, "", req))
	if got.Records[0].Status != "imported" || got.Records[0].NativeID != reserved {
		t.Fatal("reservation not replayed", got)
	}
}

func TestNativeReplyRetriesParentAckWithoutResend(t *testing.T) {
	f := newFixture(t)
	// A private note to a session: its reply takes the parent (mail to a
	// person never is: SPEC 9 "For you").
	f.sess.add("mini/rrrrrrrr", f.harbor.ID, "", true, orchapi.StateRunning)
	c := f.as("", "harbor")
	parent := must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "mini/rrrrrrrr", Body: "invented question", Audience: orchapi.MailAudienceParticipants}))
	bea := &authz.Subject{Address: "mini/rrrrrrrr", Session: "mini/rrrrrrrr", Scope: authz.All()}
	db := must(f.d.Store.Project(authz.All(), authz.Write, f.harbor.ID))
	must(db.Exec(`CREATE TRIGGER fail_parent_ack BEFORE UPDATE OF acked_at ON mail BEGIN SELECT RAISE(ABORT,'fixture ack failure'); END`))
	req := orchapi.ReplyRequest{Body: "invented answer", ClientRequestID: "ack-retry"}
	if _, err := f.s.Reply(ctx, bea, parent.Project, parent.ID, req); err == nil {
		t.Fatal("injected parent ack failure was hidden")
	}
	var firstID string
	if err := db.QueryRow(`SELECT id FROM mail WHERE reply_to=?`, parent.ID).Scan(&firstID); err != nil {
		t.Fatal(err)
	}
	if must(orchclient.GetMail(ctx, c, parent.ID)).AckedAt != nil {
		t.Fatal("synthetic ack did not fail")
	}
	must(db.Exec(`DROP TRIGGER fail_parent_ack`))
	again := must(f.s.Reply(ctx, bea, parent.Project, parent.ID, req))
	if again.ID != firstID || must(orchclient.GetMail(ctx, c, parent.ID)).AckedAt == nil {
		t.Fatal("retry failed to settle parent without resend", firstID, again)
	}
}

func TestPersonalMailWithheldFromUnawareReplica(t *testing.T) {
	f := newFixture(t)
	owner := f.as("", "harbor")
	sub := f.d.Projects.Local("")
	sub.Node = "fixture-old-replica"
	sub.PersonalMail = false
	ch, cancel := f.d.Events.Subscribe(sub, events.Filter{}, 8)
	defer cancel()
	got := must(orchclient.ImportLegacyMail(ctx, owner, importArtifact(f)))
	id := got.Records[0].NativeID
	select {
	case e := <-ch:
		t.Fatal("old replica received a private live event", e)
	default:
	}
	if rows := must(f.d.Events.Since(ctx, sub, "", events.Filter{}, 50)); len(rows) != 0 {
		t.Fatal("old replica received private replay", rows)
	}
	if changes, _, _, err := f.d.Events.Changes(ctx, sub, f.harbor.ID, "", 50); err != nil || len(changes) != 0 {
		t.Fatal("old replica received private changes", changes, err)
	}
	if last := must(f.d.Events.LastVisible(ctx, sub, f.harbor.ID)); last != "" {
		t.Fatal("old replica learned private cursor", last)
	}
	oldctx := core.WithSubject(ctx, sub)
	if _, _, err := f.s.find(oldctx, sub.Scope, sub.Address, id); !api.IsCode(err, api.CodeNotFound) {
		t.Fatal("old replica fetched private body", err)
	}
	if err := f.s.Snapshot(ctx, sub, f.harbor.ID, func(_ string, v any) error { t.Fatal("old replica received private snapshot", v); return nil }); err != nil {
		t.Fatal(err)
	}
	sub.PersonalMail = true
	if _, _, err := f.s.find(core.WithSubject(ctx, sub), sub.Scope, sub.Address, id); err != nil {
		t.Fatal("capable participant cannot read", err)
	}
	if rows := must(f.d.Events.Since(ctx, sub, "", events.Filter{}, 50)); len(rows) != 1 {
		t.Fatal("capable participant cannot replay", rows)
	}
}

func TestPersonalParticipantMetadataRedactsHiddenSender(t *testing.T) {
	f := newFixture(t)
	hidden := "mini/12121212"
	f.sess.add(hidden, f.harbor.ID, "billing", false, orchapi.StateIdle)
	c := f.as(hidden, "harbor/solver")
	m := must(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "human:vida", Body: "invented private hidden sender", Kind: orchapi.MailKindReport, Audience: orchapi.MailAudienceParticipants}))
	got := must(orchclient.GetMail(ctx, f.guest(), m.ID))
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), hidden) || got.From != "someone" {
		t.Fatal("hidden participant metadata leaked", string(raw))
	}
}
