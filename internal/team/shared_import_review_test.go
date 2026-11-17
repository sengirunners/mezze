package team_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

func TestIndependentSharedImportCacheBindsExplicitProject(t *testing.T) {
	quietPresence(t)
	tn := tailnettest.New()
	a := authority(t, tn)
	_, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	hc := tailnetClientAs(t, tn, "bea-mac")
	token := readToken(t, j.Remote.TokenFile)
	req := sharedArtifact("bea")
	hdr := map[string]string{api.HeaderWriteID: ids.New(), api.HeaderProject: j.Remote.Project.ID, api.HeaderProjectFrom: "flag", api.HeaderPersonalMail: api.PersonalMailProtocol}
	call := func() (int, string) {
		return rawCall(t, hc, http.MethodPost, "http://potluck-mini:7433/v0/mail/imports", token, req, hdr)
	}
	st, body := call()
	var out orchapi.LegacyMailImportResult
	if st/100 != 2 || json.Unmarshal([]byte(body), &out) != nil || len(out.Records) != 1 {
		t.Fatalf("initial %d %s", st, body)
	}
	hdr[api.HeaderProject] = "missing-project"
	st, body = call()
	if st/100 == 2 || strings.Contains(body, out.Records[0].NativeID) {
		t.Fatalf("changed explicit project bypassed handler authorization: %d %s", st, body)
	}
}

func TestSharedImportCacheCanonicalDestination(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	_, j := replica(t, tn, a, "bea-mac", "nBEA", "harbor", "bea", "member")
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "hidden-side"}))(t)
	hc := tailnetClientAs(t, tn, "bea-mac")
	token := readToken(t, j.Remote.TokenFile)
	req := sharedArtifact("bea")
	base := "http://potluck-mini:7433/v0/mail/imports"
	hdr := map[string]string{api.HeaderWriteID: ids.New(), api.HeaderProject: j.Remote.Project.ID + "/billing", api.HeaderProjectFrom: "flag", api.HeaderPersonalMail: api.PersonalMailProtocol}
	call := func(query string) (int, string) { return rawCall(t, hc, http.MethodPost, base+query, token, req, hdr) }
	status, first := call("")
	var out orchapi.LegacyMailImportResult
	if status/100 != 2 || json.Unmarshal([]byte(first), &out) != nil || len(out.Records) != 1 || out.Records[0].NativeID == "" {
		t.Fatalf("initial %d %s", status, first)
	}
	id := out.Records[0].NativeID
	for _, tc := range []struct {
		name, header, query string
		accepted            bool
	}{
		{"canonical slug alias", "harbor/billing", "", true},
		{"same query overrides missing header", "missing-project", "?project=harbor%2Fbilling", true},
		{"query with canonical ID", "hidden-side", "?project=" + j.Remote.Project.ID + "%2Fbilling", true},
		{"missing project", "missing-project", "", false},
		{"hidden other project", "hidden-side", "", false},
		{"missing area", "harbor/missing-area", "", false},
		{"different writable area", "harbor/solver", "", false},
		{"wrong query overrides valid header", "harbor/billing", "?project=missing-project", false},
		{"different query area", "harbor/billing", "?project=harbor%2Fsolver", false},
		{"absent destination", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hdr[api.HeaderProject] = tc.header
			st, got := call(tc.query)
			if tc.accepted {
				if st != status || got != first {
					t.Fatalf("equivalent canonical destination failed: %d %s", st, got)
				}
			} else if st/100 == 2 || strings.Contains(got, id) {
				t.Fatalf("changed destination got successful cached import: %d %s", st, got)
			}
		})
	}
	// None of the rejected selections may re-dispatch a completed write or
	// alter its original immutable import destination/result.
	hdr[api.HeaderProject] = "harbor/billing"
	if st, got := call(""); st != status || got != first {
		t.Fatalf("original replay changed: %d %s", st, got)
	}
	var count int
	if err := a.dm.Deps.Store.Registry().QueryRow(`SELECT count(*) FROM mail_imports`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("destination retry modified ledger: %d %v", count, err)
	}
}
