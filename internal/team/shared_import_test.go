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
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

func sharedArtifact(person string) orchapi.LegacyMailImportRequest {
	sent := time.Now().UTC().Add(-48 * time.Hour)
	return orchapi.LegacyMailImportRequest{SourceInstance: "fixture-source-installation", MappingVersion: "explicit-self", SourceIDs: []string{"18615"}, Mappings: []orchapi.LegacyMailMapping{{SourceProject: "ownerprivate", SourceRecipient: "person:legacy", Recipient: "human:" + person}}, Records: []orchapi.LegacyMailRecord{{Kind: "mail", ID: "18615", Project: "ownerprivate", Sender: "source-reported-agent", Recipient: "person:legacy", SentAt: sent, Body: "invented digest-held source text", BodyComplete: true, Delivery: orchapi.LegacyDelivery{AckBy: "digest", AckAt: &sent}}}}
}

// Regression for the retained joined-dev05 design reproduction: all daemons,
// artifacts, identities and transports are disposable; no legacy source is read.
func TestSharedOwnPersonImportAuthorityAndIsolation(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, joined := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	c := at(r.owner, "harbor", "")
	native := mustf(orchclient.Send(ctx, c, orchapi.SendMailRequest{To: "human:bea", Body: "invented ordinary self mail", Audience: orchapi.MailAudienceParticipants}))(t)
	if native.From != "human:bea" {
		t.Fatal(native)
	}
	req := sharedArtifact("bea")
	if _, err := orchclient.ImportLegacyMail(ctx, r.owner, req); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatal("joined import inferred a project", err)
	}
	wrong := req
	wrong.Mappings = append([]orchapi.LegacyMailMapping(nil), req.Mappings...)
	wrong.Mappings[0].Recipient = "human:mini-human"
	if _, err := orchclient.ImportLegacyMail(ctx, c, wrong); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatal("cross-person mapping permitted", err)
	}
	req.DryRun = true
	dry := mustf(orchclient.ImportLegacyMail(ctx, c, req))(t)
	if dry.Records[0].Status != "would_import" {
		t.Fatal(dry)
	}
	var count int
	if err := a.dm.Deps.Store.Registry().QueryRow(`SELECT count(*) FROM mail_imports`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("dry run reserved: %d %v", count, err)
	}
	req.DryRun = false
	// Interrupt the authority between its FULL registry reservation and atomic
	// project sink, then restart with no in-memory import state.
	db := a.dm.Deps.Store.Writer(joined.Remote.Project.ID)
	if _, err := db.Exec(`CREATE TRIGGER fail_shared_import BEFORE INSERT ON mail_import_sink BEGIN SELECT RAISE(ABORT,'fixture interruption'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.ImportLegacyMail(ctx, c, req); err == nil {
		t.Fatal("synthetic interruption was hidden")
	}
	var reserved string
	if err := a.dm.Deps.Store.Registry().QueryRow(`SELECT mail_id FROM mail_imports`).Scan(&reserved); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM mail_import_sink`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial sink survived: %d %v", count, err)
	}
	if _, err := db.Exec(`DROP TRIGGER fail_shared_import`); err != nil {
		t.Fatal(err)
	}
	a.restart(tn, tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"})
	result := mustf(orchclient.ImportLegacyMail(ctx, c, req))(t)
	id := result.Records[0].NativeID
	if id == "" || id != reserved || result.Records[0].Status != "imported" {
		t.Fatal(result)
	}
	m := mustf(orchclient.GetMail(ctx, c, id))(t)
	if m.From != "human:bea" || m.Recipient != "human:bea" || m.AckedAt != nil || m.Origin == nil || m.Origin.Trust != "source_reported" || m.Origin.ImportedBy != "human:bea" || m.LegacyDelivery == nil || m.LegacyDelivery.State != "digest_held" || m.ReplyUnavailable == "" {
		t.Fatalf("bad recovered mail: %+v", m)
	}
	if _, err := orchclient.Reply(ctx, c, id, orchapi.ReplyRequest{Body: "invented reply"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatal("unverified reply permitted", err)
	}
	if _, err := orchclient.GetMail(ctx, at(a.owner, "harbor", ""), id); !api.IsCode(err, api.CodeNotFound) {
		t.Fatal("authority owner saw other person's mail", err)
	}
	if err := r.dm.Deps.Store.Registry().QueryRow(`SELECT count(*) FROM mail_imports`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("replica owns import ledger: %d %v", count, err)
	}
	// Same canonical human on another node uses the identity namespace, not a
	// device/token namespace. Final ledger replay works after authority restart.
	r2, _ := replica(t, tn, a, "bea-laptop", "nBEA2", "harbor", "bea", "member")
	a.restart(tn, tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"})
	again := mustf(orchclient.ImportLegacyMail(ctx, at(r2.owner, "harbor", ""), req))(t)
	if again.Records[0].NativeID != id || again.Records[0].Status != "already_present" {
		t.Fatal(again)
	}
	// An unrelated person cannot poison or discover the first person's tuple.
	v, _ := replica(t, tn, a, "vida-mac", "nVIDA", "harbor", "vida", "member")
	vreq := req
	vreq.Mappings = append([]orchapi.LegacyMailMapping(nil), req.Mappings...)
	vreq.Mappings[0].Recipient = "human:vida"
	other := mustf(orchclient.ImportLegacyMail(ctx, at(v.owner, "harbor", ""), vreq))(t)
	if other.Records[0].Status != "imported" || other.Records[0].NativeID == id {
		t.Fatal(other)
	}
	if _, err := orchclient.GetMail(ctx, at(v.owner, "harbor", ""), id); !api.IsCode(err, api.CodeNotFound) {
		t.Fatal("cross-person leak", err)
	}
	req.Records[0].Body = "changed invented source content"
	conflict := mustf(orchclient.ImportLegacyMail(ctx, c, req))(t)
	if conflict.Records[0].Status != "conflict" || conflict.Records[0].NativeID != "" {
		t.Fatal(conflict)
	}
}

func TestSharedImportLiveReplayAuthAndNoReplicaFallback(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	r, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	hc := tailnetClientAs(t, tn, "bea-mac")
	token := readToken(t, j.Remote.TokenFile)
	req := sharedArtifact("bea")
	hdr := map[string]string{api.HeaderWriteID: ids.New(), api.HeaderProject: j.Remote.Project.ID, api.HeaderProjectFrom: "flag", api.HeaderPersonalMail: api.PersonalMailProtocol}
	call := func() (int, string) {
		return rawCall(t, hc, http.MethodPost, "http://potluck-mini:7433/v0/mail/imports", token, req, hdr)
	}
	st, body := call()
	var out orchapi.LegacyMailImportResult
	if st/100 != 2 || json.Unmarshal([]byte(body), &out) != nil || len(out.Records) != 1 || out.Records[0].NativeID == "" {
		t.Fatalf("direct import: %d %s", st, body)
	}
	id := out.Records[0].NativeID
	if st2, b2 := call(); st2 != st || b2 != body {
		t.Fatalf("live replay changed: %d %s", st2, b2)
	}
	assertHidden := func() {
		t.Helper()
		status, got := call()
		if status/100 == 2 || strings.Contains(got, id) || strings.Contains(got, req.Records[0].Body) {
			t.Fatalf("unauthorized import/cache disclosure: %d %s", status, got)
		}
	}
	delete(hdr, api.HeaderPersonalMail)
	assertHidden()
	// The same unawareness guard also applies before any cache exists.
	savedID := hdr[api.HeaderWriteID]
	hdr[api.HeaderWriteID] = ids.New()
	assertHidden()
	hdr[api.HeaderWriteID] = savedID
	hdr[api.HeaderPersonalMail] = api.PersonalMailProtocol
	hdr[api.HeaderAs] = "bea-mac/1234abcd"
	assertHidden()
	hdr[api.HeaderAs] = "human:mini-human"
	assertHidden()
	delete(hdr, api.HeaderAs)
	hdr[api.HeaderProjectFrom] = "env"
	assertHidden()
	hdr[api.HeaderProjectFrom] = "flag"
	bare := client.WithHandler(a.dm.SocketHandler())
	bare.Token, bare.Project, bare.ProjectFrom = token, j.Remote.Project.ID, "flag"
	if _, err := orchclient.ImportLegacyMail(ctx, bare, req); err == nil {
		t.Fatal("bare bearer imported without node proof")
	}
	// New live request IDs still converge on the durable import mapping.
	hdr[api.HeaderWriteID] = ids.New()
	if status, got := call(); status/100 != 2 || !strings.Contains(got, id) || !strings.Contains(got, "already_present") {
		t.Fatalf("ledger replay: %d %s", status, got)
	}
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Project: "harbor", Area: "solver", Role: "guest"}))(t)
	mustf(a.owner.Revoke(ctx, api.GrantRequest{Identity: "bea", Project: "harbor"}))(t)
	assertHidden()
	// No queued import or local sink can appear when the named authority fails.
	a.halt()
	// halt is already consumed; cleanup must not call it a second time.
	a.halt = func() {}
	if _, err := orchclient.ImportLegacyMail(ctx, at(r.owner, "harbor", ""), req); err == nil {
		t.Fatal("offline import looked successful")
	}
	var count int
	if err := r.dm.Deps.Store.Registry().QueryRow(`SELECT count(*) FROM mail_imports`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("offline replica import: %d %v", count, err)
	}
}

// Rename step 1: a self import names its project under the Potluck-* headers
// too (a later client's); "env" under either name is still no explicit project.
func TestSharedImportProjectHeaderNames(t *testing.T) {
	quietPresence(t)
	tn := tailnettest.New()
	a := authority(t, tn)
	r, _ := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	b, _ := json.Marshal(sharedArtifact("bea"))
	call := func(hdr map[string]string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v0/mail/imports", bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		r.dm.SocketHandler().ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	nw := api.HeaderNew
	for _, hdr := range []map[string]string{
		{nw(api.HeaderProjectFrom): "env", nw(api.HeaderProject): "harbor"},
		{api.HeaderProjectFrom: "flag", nw(api.HeaderProjectFrom): "env", nw(api.HeaderProject): "harbor"},
		{},
	} {
		if st, body := call(hdr); st != http.StatusBadRequest || !strings.Contains(body, "requires an explicit project") {
			t.Fatalf("%v: %d %s", hdr, st, body)
		}
	}
	st, body := call(map[string]string{nw(api.HeaderProjectFrom): "flag", nw(api.HeaderProject): "harbor"})
	var out orchapi.LegacyMailImportResult
	if st/100 != 2 || json.Unmarshal([]byte(body), &out) != nil || len(out.Records) != 1 || out.Records[0].Status != "imported" {
		t.Fatalf("import named by the new headers: %d %s", st, body)
	}
	var count int
	if err := r.dm.Deps.Store.Registry().QueryRow(`SELECT count(*) FROM mail_imports`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("replica owns import ledger: %d %v", count, err)
	}
}
