package work

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/ids"
)

// TestMirrorPagePlans checks the mirror page queries' plans on the real
// schema (every registered migration, no statistics, as a fresh daemon):
// the default created order walks work_mirror_created with no sort, and an
// area-scoped page seeks work_mirror_area instead of filtering the walk.
func TestMirrorPagePlans(t *testing.T) {
	ctx := context.Background()
	st, err := storetest.Open(ctx, t.TempDir(), Migrations...)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	db, err := st.CreateProject(ctx, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	plan := func(areaScoped bool, where, order string) string {
		q := `EXPLAIN QUERY PLAN SELECT id FROM ` + mirrorFrom(areaScoped) + ` WHERE source = ? AND ` + where +
			` ORDER BY ` + mirrorCreated + ` ` + order + `, id ` + order + ` LIMIT 50`
		rows, err := db.QueryContext(ctx, q, "bd", "a", "open", int64(5), int64(5), "x")
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			out = append(out, detail)
		}
		return strings.Join(out, "; ")
	}
	k := mirrorCreated
	cursor := fmt.Sprintf("(%s < ? OR (%s = ? AND id < ?))", k, k)
	for _, c := range []struct {
		name       string
		areaScoped bool
		where      string
		order      string
		want       string
		sorts      bool
	}{
		{"owner, newest first", false, "1=1 AND ? <> '' AND ? <> '' AND ? > 0 AND ? > 0 AND ? <> ''", "DESC", "work_mirror_created", false},
		{"owner, oldest first", false, "1=1 AND ? <> '' AND ? <> '' AND ? > 0 AND ? > 0 AND ? <> ''", "ASC", "work_mirror_created", false},
		{"owner, next page", false, "? <> '' AND ? <> '' AND " + cursor, "DESC", "work_mirror_created", false},
		{"area + status", true, "area IN (?) AND status IN (?) AND ? > 0 AND ? > 0 AND ? <> ''", "DESC", "work_mirror_area", true},
		{"area, next page", true, "area IN (?) AND ? <> '' AND " + cursor, "DESC", "work_mirror_area", true},
	} {
		p := plan(c.areaScoped, c.where, c.order)
		if !strings.Contains(p, c.want) || strings.Contains(p, "TEMP B-TREE") != c.sorts {
			t.Errorf("%s: want %s (sort %v), plan: %s", c.name, c.want, c.sorts, p)
		}
	}
}
