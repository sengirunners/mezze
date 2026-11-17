package owedrows_test

import (
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/work/owedrows"
)

func item(status orchapi.WorkStatus, links ...orchapi.Link) orchapi.WorkItem {
	return orchapi.WorkItem{ID: "local:abc", Title: "make it so", Status: status, Project: "p1", Links: links}
}

func kinds(rows []orchapi.OwedItem) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.Kind+" "+r.Ref)
	}
	return out
}

func TestRows(t *testing.T) {
	pr := orchapi.Link{Rel: "pr", Target: "https://example.test/o/r/pull/7"}
	cases := []struct {
		name string
		h    owedrows.Held
		want []string
	}{
		{"held, no pr", owedrows.Held{Item: item(orchapi.WorkClaimed), Leased: true}, []string{"work local:abc"}},
		{"held with open pr", owedrows.Held{Item: item(orchapi.WorkInReview, pr), Leased: true},
			[]string{"work local:abc", "pr https://example.test/o/r/pull/7"}},
		{"lapsed lease, open pr: only the pr", owedrows.Held{Item: item(orchapi.WorkInReview, pr)},
			[]string{"pr https://example.test/o/r/pull/7"}},
		{"lapsed lease, no pr: nothing", owedrows.Held{Item: item(orchapi.WorkClaimed)}, nil},
		{"merged pr (item done): nothing", owedrows.Held{Item: item(orchapi.WorkDone, pr), Leased: true}, nil},
		{"open item with a pr link is not an open pr", owedrows.Held{Item: item(orchapi.WorkOpen, pr)}, nil},
		{"hidden and repeated links", owedrows.Held{Item: item(orchapi.WorkClaimed, orchapi.Link{Rel: "pr", Hidden: true}, pr, pr)},
			[]string{"pr https://example.test/o/r/pull/7"}},
		{"other link kinds", owedrows.Held{Item: item(orchapi.WorkClaimed, orchapi.Link{Rel: "related", Target: "local:zzz"})}, nil},
	}
	for _, c := range cases {
		got := kinds(owedrows.Rows(c.h))
		if len(got) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: got %v, want %v", c.name, got, c.want)
			}
		}
	}
}
