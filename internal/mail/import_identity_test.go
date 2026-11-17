package mail

import (
	"context"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/project"
)

func TestImportIdentityAdditiveMigrationRetainsUnfinishedJournal(t *testing.T) {
	home := t.TempDir()
	st, err := storetest.Open(ctx, home, Migrations...)
	if err != nil {
		t.Fatal(err)
	}
	// Reconstruct the previously shipped schema, including its applied migration.
	_, err = st.Registry().Exec(`DROP TABLE mail_imports; DROP TABLE mail_import_legacy_state; ALTER TABLE mail_import_legacy RENAME TO mail_imports; DELETE FROM schema_migrations WHERE name='mail/0002_import_identity'; INSERT INTO mail_imports VALUES('retained-source','mail','42','retained-hash','retained-project','retained-native-id')`)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = storetest.Open(ctx, home, Migrations...)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var hash, native string
	if err = st.Registry().QueryRow(`SELECT payload_hash,mail_id FROM mail_import_legacy WHERE source_instance='retained-source' AND source_id='42'`).Scan(&hash, &native); err != nil || hash != "retained-hash" || native != "retained-native-id" {
		t.Fatalf("legacy reservation lost: %s %s %v", hash, native, err)
	}
	var count int
	if err = st.Registry().QueryRow(`SELECT count(*) FROM mail_imports`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("migration guessed importer: %d %v", count, err)
	}
}

func makeLegacyReservation(t *testing.T, f *fixture, id string) {
	t.Helper()
	_, err := f.d.Store.Registry().Exec(`INSERT INTO mail_import_legacy(source_instance,record_kind,source_id,payload_hash,project,mail_id) SELECT source_instance,record_kind,source_id,payload_hash,project,mail_id FROM mail_imports WHERE mail_id=?`, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.d.Store.Registry().Exec(`DELETE FROM mail_imports WHERE mail_id=?`, id); err != nil {
		t.Fatal(err)
	}
}

func TestImportLegacyCompletedOwnerContinuityAndReadOnlyPreview(t *testing.T) {
	f := newFixture(t)
	c := f.as("", "harbor")
	req := importArtifact(f)
	first := must(orchclient.ImportLegacyMail(ctx, c, req)).Records[0].NativeID
	must(orchclient.Ack(ctx, c, first))
	makeLegacyReservation(t, f, first)
	req.DryRun = true
	preview := must(orchclient.ImportLegacyMail(ctx, c, req))
	if preview.Records[0].Status != "already_present" || preview.Records[0].NativeID != first {
		t.Fatal("legacy preview lost stable sink", preview)
	}
	for _, table := range []string{"mail_imports", "mail_import_legacy_state"} {
		var count int
		if err := f.d.Store.Registry().QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("dry preview mutated %s: %d %v", table, count, err)
		}
	}
	req.DryRun = false
	result := must(orchclient.ImportLegacyMail(ctx, c, req))
	if result.Records[0].Status != "already_present" || result.Records[0].NativeID != first || must(orchclient.GetMail(ctx, c, first)).AckedAt == nil {
		t.Fatal("legacy replay changed read state", result)
	}
	var actor, state string
	if err := f.d.Store.Registry().QueryRow(`SELECT importer_identity FROM mail_imports WHERE mail_id=?`, first).Scan(&actor); err != nil || actor != f.d.Projects.LocalIdentity().ID {
		t.Fatal("owner continuity not bound", actor, err)
	}
	if err := f.d.Store.Registry().QueryRow(`SELECT state FROM mail_import_legacy_state`).Scan(&state); err != nil || state != "migrated" {
		t.Fatal(state, err)
	}
	req.Records[0].Body = "changed immutable legacy body"
	if got := must(orchclient.ImportLegacyMail(ctx, c, req)); got.Records[0].Status != "conflict" || got.Records[0].NativeID != "" {
		t.Fatal("changed artifact adopted legacy sink", got)
	}
}

func TestImportAmbiguousLegacyReservationsHoldUniformlyWithoutAdoption(t *testing.T) {
	f := newFixture(t)
	c := f.as("", "harbor")
	req := importArtifact(f)
	_, err := f.d.Store.Registry().Exec(`INSERT INTO mail_import_legacy(source_instance,record_kind,source_id,payload_hash,project,mail_id) VALUES(?,?,?,?,?,?)`, req.SourceInstance, "mail", "18615", "unknown-old-owner-hash", f.harbor.ID, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		req.DryRun = dry
		_, firstErr := orchclient.ImportLegacyMail(ctx, c, req)
		if !api.IsCode(firstErr, api.CodeUnavailable) {
			t.Fatalf("ambiguous reservation adopted: %v", firstErr)
		}
		req.SourceInstance = "entirely-other-installation"
		req.SourceIDs = []string{"absent"}
		req.Records[0].ID = "absent"
		_, otherErr := orchclient.ImportLegacyMail(ctx, c, req)
		if otherErr == nil || firstErr.Error() != otherErr.Error() {
			t.Fatalf("legacy hold became source-tuple oracle: %v / %v", firstErr, otherErr)
		}
	}
	var count int
	if err = f.d.Store.Registry().QueryRow(`SELECT count(*) FROM mail_imports`).Scan(&count); err != nil || count != 0 {
		t.Fatal("held reservation created namespace", count, err)
	}
	if err = f.d.Store.Registry().QueryRow(`SELECT count(*) FROM mail_import_legacy`).Scan(&count); err != nil || count != 1 {
		t.Fatal("old reservation was lost", count, err)
	}
	// Rejected callers do not learn whether the authority has a legacy hold.
	_, err = orchclient.ImportLegacyMail(ctx, f.as("mini/11111111", "harbor"), req)
	if !api.IsCode(err, api.CodeForbidden) || strings.Contains(err.Error(), "reconciliation") {
		t.Fatal("session probed hold", err)
	}
}

func TestImportLegacyChangedLocalOwnerDoesNotGuessHistoricalIdentity(t *testing.T) {
	f := newFixture(t)
	c := f.as("", "harbor")
	req := importArtifact(f)
	first := must(orchclient.ImportLegacyMail(ctx, c, req)).Records[0].NativeID
	makeLegacyReservation(t, f, first)
	newRegistry, err := project.Open(ctx, f.d.Store, &config.Config{Home: f.d.Config.Home, Machine: "mini", Human: "replacement-owner"})
	if err != nil {
		t.Fatal(err)
	}
	f.d.Projects = newRegistry
	req.Mappings[0].Recipient = "human:replacement-owner"
	_, err = f.s.ImportLegacy(ctx, newRegistry.Local(""), api.ProjectRef{ID: f.harbor.ID}, "", req)
	if !api.IsCode(err, api.CodeUnavailable) {
		t.Fatal("new owner adopted previous owner's import", err)
	}
	var count int
	if err = f.d.Store.Registry().QueryRow(`SELECT count(*) FROM mail_imports`).Scan(&count); err != nil || count != 0 {
		t.Fatal("owner mismatch created new mapping", count, err)
	}
}

func TestImportAuthRejectsUnprovenActorsBeforeLegacyState(t *testing.T) {
	f := newFixture(t)
	owner := f.d.Projects.Local("")
	remote := *owner
	remote.Via = authz.ViaToken
	remote.Remote = true
	remote.Node = "fixture-node"
	remote.Machine = "fixture-machine"
	remote.TokenID = "fixture-token"
	remote.PersonalMail = true
	remote.Cap = &authz.Cap{Project: f.harbor.ID, Role: authz.Member}
	base := core.WithRemote(context.Background(), core.Remote{NodeID: "fixture-node"})
	base = core.WithResolveInput(base, project.ResolveInput{Explicit: "harbor", ExplicitFrom: project.RuleFlag})
	if err := f.s.authorizeImporter(base, &remote); err != nil {
		t.Fatal("valid authenticated node fact refused", err)
	}
	for name, mutate := range map[string]func(*authz.Subject){
		"bare token": func(s *authz.Subject) { s.Node = "" }, "unaware": func(s *authz.Subject) { s.PersonalMail = false },
		"machine address": func(s *authz.Subject) { s.Address = "machine:fixture-machine" }, "session": func(s *authz.Subject) { s.Session = "fixture-machine/12345678" },
		"agent identity": func(s *authz.Subject) { s.Identity.Kind = api.IdentityAgent }, "no cap": func(s *authz.Subject) { s.Cap = nil }, "no machine": func(s *authz.Subject) { s.Machine = "" },
	} {
		t.Run(name, func(t *testing.T) {
			sub := remote
			mutate(&sub)
			if err := f.s.authorizeImporter(base, &sub); !api.IsCode(err, api.CodeForbidden) {
				t.Fatal("unproven actor accepted", err)
			}
		})
	}
	if err := f.s.authorizeImporter(core.WithRemote(base, core.Remote{NodeID: "other-node"}), &remote); !api.IsCode(err, api.CodeForbidden) {
		t.Fatal("node mismatch", err)
	}
	if err := f.s.authorizeImporter(context.Background(), &remote); !api.IsCode(err, api.CodeForbidden) {
		t.Fatal("no listener proof", err)
	}
	if err := f.s.authorizeImporter(core.WithResolveInput(base, project.ResolveInput{Explicit: "harbor", ExplicitFrom: project.RuleEnv}), &remote); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatal("environment project accepted", err)
	}
}
