package storetest

import (
	"errors"
	"slices"
	"testing"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
)

// listIDs is List's ids (in its order) and Total, failing on an error.
func listIDs(t *testing.T, p istore.ProjectStore, q orchapi.IssueQuery, vis istore.Areas) ([]string, orchapi.IssueList) {
	t.Helper()
	l, err := p.List(bg, q, vis, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, it := range l.Issues {
		out = append(out, it.ID)
	}
	return out, l
}

// testList: List and its Total under area scoping, status filters, sort by
// id, IDAfter paging, and templates (never listed, still readable by id).
func testList(t *testing.T, newStore NewStore) {
	p := project(t, newStore)
	mk := func(title, area, status string) string {
		req := orchapi.NewIssueRequest{Title: title, Status: status}
		if area != "" {
			req.Labels = []string{"area:" + area}
		}
		return apply(t, p, istore.Mutation{Op: istore.OpCreate, AreaOf: labelArea, Create: &req}).Issue.ID
	}
	a1, a2, a3 := mk("a1", "a", ""), mk("a2", "a", orchapi.IssueClosed), mk("a3", "a", orchapi.IssueInProgress)
	b1, p1 := mk("b1", "b", ""), mk("p1", "", "")
	at := time.Now().Add(-time.Hour)
	tpl := "oc-tpl"
	if err := p.Import(bg, istore.ImportBatch{Issues: []orchapi.Issue{{ID: tpl, Title: "template", Status: orchapi.IssueOpen, Priority: 2, IssueType: "task",
		IsTemplate: true, Area: "a", Labels: []string{"area:a"}, CreatedAt: at, UpdatedAt: at}}}); err != nil {
		t.Fatal(err)
	}
	if err := p.FinishImport(bg, labelArea); err != nil {
		t.Fatal(err)
	}
	if it, err := p.Get(bg, tpl, all); err != nil || !it.IsTemplate || it.Area != "a" {
		t.Fatalf("template by id: %+v %v", it, err)
	}
	if n, err := p.Counts(bg); err != nil || n["issues"] != 6 {
		t.Fatalf("counts %v %v", n, err)
	}

	in := func(names ...string) istore.Areas { return istore.Areas{Names: names} }
	for _, tc := range []struct {
		name string
		q    orchapi.IssueQuery
		vis  istore.Areas
		want []string
	}{
		{"default hides closed and templates", orchapi.IssueQuery{}, in("a"), []string{a1, a3}},
		{"all statuses", orchapi.IssueQuery{Status: "all"}, in("a"), []string{a1, a2, a3}},
		{"one status", orchapi.IssueQuery{Status: orchapi.IssueClosed}, in("a"), []string{a2}},
		{"statuses ORed", orchapi.IssueQuery{Status: "open,in_progress"}, in("a", "b"), []string{a1, a3, b1}},
		{"whole project has project-level", orchapi.IssueQuery{Status: "all"}, all, []string{a1, a2, a3, b1, p1}},
		{"no areas sees nothing", orchapi.IssueQuery{Status: "all"}, istore.Areas{}, nil},
		{"unknown area sees nothing", orchapi.IssueQuery{Status: "all"}, in("zz"), nil},
		{"area filter inside vis", orchapi.IssueQuery{Status: "all", Areas: []string{"b"}}, in("a"), nil},
		{"area filter for project-level", orchapi.IssueQuery{Status: "all", Areas: []string{""}}, all, []string{p1}},
		{"IDs stay scoped", orchapi.IssueQuery{Status: "all", IDs: []string{a1, b1, tpl}}, in("b"), []string{b1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.q.Sort, tc.q.Count = "id", true
			got, l := listIDs(t, p, tc.q, tc.vis)
			want := slices.Sorted(slices.Values(tc.want))
			if !slices.Equal(got, want) || l.Total != len(want) || l.More {
				t.Fatalf("ids %v total %d more %v; want %v", got, l.Total, l.More, want)
			}
		})
	}
	if _, err := p.Get(bg, b1, in("a")); !errors.Is(err, istore.ErrNotFound) {
		t.Fatalf("hidden get: %v", err)
	}
	if _, err := p.Get(bg, p1, in("a", "b")); !errors.Is(err, istore.ErrNotFound) {
		t.Fatalf("project-level get from an area reader: %v", err)
	}
	if ls, err := p.Labels(bg, in("b"), nil); err != nil || !slices.Equal(ls, []string{"area:b"}) {
		t.Fatalf("labels %v %v", ls, err)
	}

	// Sort by id, reversed, and paging by IDAfter.
	for range 7 {
		mk("more", "a", "")
	}
	full, _ := listIDs(t, p, orchapi.IssueQuery{Status: "all", Sort: "id", All: true}, in("a"))
	if len(full) != 10 || !slices.IsSorted(full) {
		t.Fatalf("by id %v", full)
	}
	rev, _ := listIDs(t, p, orchapi.IssueQuery{Status: "all", Sort: "id", Reverse: true, All: true}, in("a"))
	want := slices.Clone(full)
	slices.Reverse(want)
	if !slices.Equal(rev, want) {
		t.Fatalf("reversed %v", rev)
	}
	ids, l := listIDs(t, p, orchapi.IssueQuery{Status: "all", Sort: "id", Limit: 3, Count: true}, in("a"))
	if len(ids) != 3 || !l.More || l.Total != len(full) {
		t.Fatalf("first counted page %v more %v total %d, want 3 of %d", ids, l.More, l.Total, len(full))
	}
	var paged []string
	for after := ""; ; {
		page, l := listIDs(t, p, orchapi.IssueQuery{Status: "all", Sort: "id", Limit: 3, IDAfter: after}, in("a"))
		paged = append(paged, page...)
		if !l.More {
			break
		}
		if len(page) != 3 || l.Total != 3 {
			t.Fatalf("cut page %v total %d", page, l.Total)
		}
		after = page[len(page)-1]
	}
	if !slices.Equal(paged, full) {
		t.Fatalf("paged %v, want %v", paged, full)
	}
}

func testUnicodeSubstring(t *testing.T, newStore NewStore) {
	p := project(t, newStore)
	title := create(t, p, orchapi.NewIssueRequest{Title: `Über 100% done_now \ path`, Area: "public"})
	create(t, p, orchapi.NewIssueRequest{Title: "100x donexnow / path", Area: "public"})
	create(t, p, orchapi.NewIssueRequest{Title: "Über hidden", Area: "secret"})
	closed := create(t, p, orchapi.NewIssueRequest{Title: "Über closed", Status: orchapi.IssueClosed, Area: "public"})
	body := create(t, p, orchapi.NewIssueRequest{Title: "Body match", Description: "Résumé details", Area: "public"})
	create(t, p, orchapi.NewIssueRequest{Title: "Hidden body", Description: "Résumé details", Area: "secret"})
	create(t, p, orchapi.NewIssueRequest{Title: "Closed body", Description: "Résumé details", Status: orchapi.IssueClosed, Area: "public"})
	ref := create(t, p, orchapi.NewIssueRequest{Title: "External reference", ExternalRef: "ÅUTH-77", Area: "public"})

	for _, tc := range []struct {
		name string
		q    orchapi.IssueQuery
		area string
		want []string
	}{
		{"exact Unicode title", orchapi.IssueQuery{Substr: "Über"}, "public", []string{title.ID}},
		{"ASCII case still folds", orchapi.IssueQuery{Substr: "ÜBER 100% DONE_NOW"}, "public", []string{title.ID}},
		{"Unicode external reference", orchapi.IssueQuery{Substr: "Åuth-77"}, "public", []string{ref.ID}},
		{"literal percent", orchapi.IssueQuery{Substr: "%"}, "public", []string{title.ID}},
		{"literal underscore", orchapi.IssueQuery{Substr: "_"}, "public", []string{title.ID}},
		{"literal backslash", orchapi.IssueQuery{Substr: `\`}, "public", []string{title.ID}},
		{"exact Unicode body through contains", orchapi.IssueQuery{Contains: "Résumé"}, "public", []string{body.ID}},
		{"closed selected explicitly", orchapi.IssueQuery{Substr: "Über", Status: orchapi.IssueClosed}, "public", []string{closed.ID}},
		{"all statuses stay scoped", orchapi.IssueQuery{Substr: "Über", Status: "all"}, "public", []string{title.ID, closed.ID}},
		{"unrelated area sees nothing", orchapi.IssueQuery{Substr: "Über", Status: "all"}, "other", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.q.Sort, tc.q.Count = "id", true
			got, l := listIDs(t, p, tc.q, istore.Areas{Names: []string{tc.area}})
			want := slices.Sorted(slices.Values(tc.want))
			if !slices.Equal(got, want) || l.Total != len(want) {
				t.Fatalf("substring result ids %v total %d; want %v", got, l.Total, want)
			}
		})
	}
}

// A detached child keeps its dotted ID and still belongs in ready --parent.
// Both that fallback and the explicit parent tree must obey every filter.
func testReadyParent(t *testing.T, newStore NewStore) {
	p := project(t, newStore)
	parent := create(t, p, orchapi.NewIssueRequest{Title: "parent", Area: "public"})
	child := create(t, p, orchapi.NewIssueRequest{Title: "linked child", Parent: parent.ID, Area: "public"})
	orphan := create(t, p, orchapi.NewIssueRequest{Title: "detached child", Parent: parent.ID, Area: "public"})
	hidden := create(t, p, orchapi.NewIssueRequest{Title: "hidden detached child", Parent: parent.ID, Area: "secret"})
	closed := create(t, p, orchapi.NewIssueRequest{Title: "closed detached child", Parent: parent.ID, Area: "public"})
	for _, id := range []string{orphan.ID, hidden.ID, closed.ID} {
		none := ""
		apply(t, p, istore.Mutation{Op: istore.OpUpdate, ID: id, Update: &orchapi.UpdateIssueRequest{Parent: &none}})
	}
	apply(t, p, istore.Mutation{Op: istore.OpClose, ID: closed.ID})

	for _, tc := range []struct {
		name  string
		vis   istore.Areas
		title string
		want  []string
	}{
		{"area and ready status", istore.Areas{Names: []string{"public"}}, "", []string{child.ID, orphan.ID}},
		{"whole project still excludes closed", all, "", []string{child.ID, orphan.ID, hidden.ID}},
		{"later filter applies to linked children", istore.Areas{Names: []string{"public"}}, "detached", []string{orphan.ID}},
		{"later filter applies to detached children", istore.Areas{Names: []string{"public"}}, "linked", []string{child.ID}},
		{"unrelated area sees nothing", istore.Areas{Names: []string{"other"}}, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, l := listIDs(t, p, orchapi.IssueQuery{Ready: true, Parent: parent.ID, TitleContains: tc.title, Count: true}, tc.vis)
			slices.Sort(got)
			want := slices.Sorted(slices.Values(tc.want))
			if !slices.Equal(got, want) || l.Total != len(want) {
				t.Fatalf("ready under parent: ids %v, total %d; want %v", got, l.Total, want)
			}
		})
	}
}
