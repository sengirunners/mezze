package issues_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"testing"

	"github.com/sengirunners/mezze/internal/issues"
	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
)

// Seed exactly what a pre-cleaner native store could hold. Going through the
// current create/add doors would correctly reject these invisible names.
func seedLegacyLabels(t *testing.T, f *authzFixture, id string, labels ...string) {
	t.Helper()
	ctx := context.Background()
	p, err := f.owner.GetProject(ctx, "harbor")
	if err != nil {
		t.Fatal(err)
	}
	ps, err := orchcore.Issues(f.dm.Deps).(*issues.Service).Store().Project(ctx, p.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range labels {
		if _, err := ps.Apply(ctx, istore.Mutation{Op: istore.OpLabelAdd, ID: id, Label: label, Actor: istore.Actor{Name: "fixture", Address: "human:ivo"}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRemoveLegacyHiddenLabelThroughBothDoors(t *testing.T) {
	for _, update := range []bool{false, true} {
		name := "label door"
		if update {
			name = "update door"
		}
		t.Run(name, func(t *testing.T) {
			f := newAuthzFixture(t)
			o := f.ownerAt("harbor/solver")
			ctx := context.Background()
			is, err := orchclient.CreateIssue(ctx, o, orchapi.NewIssueRequest{Title: "legacy labels", Labels: []string{"keep"}})
			if err != nil {
				t.Fatal(err)
			}
			const target, other = "\u200b", "\u2066"
			seedLegacyLabels(t, f, is.ID, target, other)
			if update {
				_, err = orchclient.UpdateIssue(ctx, o, is.ID, orchapi.UpdateIssueRequest{RemoveLabels: []string{target}})
			} else {
				_, err = orchclient.RemoveIssueLabel(ctx, o, is.ID, target)
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := orchclient.Issue(ctx, o, is.ID)
			if err != nil || !slices.Equal(got.Labels, []string{"keep", other}) {
				t.Fatalf("legacy removal: labels %q err %v", got.Labels, err)
			}
			if _, err := orchclient.UpdateIssue(ctx, o, is.ID, orchapi.UpdateIssueRequest{RemoveLabels: []string{""}}); err != nil {
				t.Fatalf("empty update selector: %v", err)
			}
			got, err = orchclient.Issue(ctx, o, is.ID)
			if err != nil || !slices.Equal(got.Labels, []string{"keep", other}) {
				t.Fatalf("empty selector changed labels %q: %v", got.Labels, err)
			}
		})
	}
}

func TestLegacyLabelRemovalKeepsAddValidation(t *testing.T) {
	f := newAuthzFixture(t)
	o := f.ownerAt("harbor/solver")
	ctx := context.Background()
	is, err := orchclient.CreateIssue(ctx, o, orchapi.NewIssueRequest{Title: "normal labels", Labels: []string{"keep"}})
	if err != nil {
		t.Fatal(err)
	}
	const hidden = "\u200b"
	if _, err := orchclient.CreateIssue(ctx, o, orchapi.NewIssueRequest{Title: "refused", Labels: []string{hidden}}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("hidden create label: %v", err)
	}
	if _, err := orchclient.AddIssueLabel(ctx, o, is.ID, hidden); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("hidden add label: %v", err)
	}
	for _, req := range []orchapi.UpdateIssueRequest{{AddLabels: []string{hidden}}, {SetLabels: []string{hidden}}} {
		if _, err := orchclient.UpdateIssue(ctx, o, is.ID, req); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("hidden update labels: %v", err)
		}
	}
	if _, err := orchclient.RemoveIssueLabel(ctx, o, is.ID, " "); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("whitespace label selector: %v", err)
	}
	got, err := orchclient.Issue(ctx, o, is.ID)
	if err != nil || !slices.Equal(got.Labels, []string{"keep"}) {
		t.Fatalf("invalid write changed labels %q: %v", got.Labels, err)
	}
	if _, err := orchclient.AddIssueLabel(ctx, o, is.ID, "ta\u200bg"); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.RemoveIssueLabel(ctx, o, is.ID, "ta\u200bg"); err != nil {
		t.Fatalf("normal visible removal stopped cleaning: %v", err)
	}
	got, err = orchclient.Issue(ctx, o, is.ID)
	if err != nil || !slices.Equal(got.Labels, []string{"keep"}) {
		t.Fatalf("visible cleanup labels %q: %v", got.Labels, err)
	}
}

func TestLegacyLabelRemovalPreservesScope(t *testing.T) {
	f := newAuthzFixture(t)
	ctx := context.Background()
	const hidden = "\u200b"
	for _, tc := range []struct {
		name, area string
		code       int
	}{{"guest", "solver", http.StatusForbidden}, {"member", "billing", http.StatusNotFound}} {
		o := f.ownerAt("harbor/" + tc.area)
		is, err := orchclient.CreateIssue(ctx, o, orchapi.NewIssueRequest{Title: "scoped legacy label", Labels: []string{"keep"}})
		if err != nil {
			t.Fatal(err)
		}
		seedLegacyLabels(t, f, is.ID, hidden)
		path := "/v0/issues/" + is.ID
		b, err := json.Marshal(orchapi.UpdateIssueRequest{RemoveLabels: []string{hidden}})
		if err != nil {
			t.Fatal(err)
		}
		for _, req := range []struct{ method, path, body string }{
			{http.MethodDelete, path + "/labels/" + url.PathEscape(hidden), ""},
			{http.MethodPatch, path, string(b)},
		} {
			if code, body := f.as(tc.name, req.method, req.path, req.body); code != tc.code {
				t.Fatalf("%s %s label removal: %d %s", tc.name, req.method, code, body)
			}
		}
		got, err := orchclient.Issue(ctx, o, is.ID)
		if err != nil || !slices.Equal(got.Labels, []string{"keep", hidden}) {
			t.Fatalf("denied removal changed labels %q: %v", got.Labels, err)
		}
	}
}
