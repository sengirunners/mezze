package work

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
)

func mirrorSearchDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	ctx := context.Background()
	st, err := storetest.Open(ctx, t.TempDir(), Migrations...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	pid := ids.New()
	db, err := st.CreateProject(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	return db, pid
}

func TestMirrorTextSearchResults(t *testing.T) {
	db, pid := mirrorSearchDB(t)
	ctx := context.Background()
	items := []orchapi.WorkItem{
		{ID: "bd:a", Title: "Needle title", Status: orchapi.WorkOpen, Area: "a", Owner: "mini/owner001"},
		{ID: "bd:b", Body: "needle body", Status: orchapi.WorkClaimed, Area: "b"},
		{ID: "bd:c", Title: "needle review", Status: orchapi.WorkInReview, Area: "a"},
		{ID: "bd:d", Title: "needle done", Status: orchapi.WorkDone, Area: "a"},
		{ID: "bd:e", Title: "unrelated", Status: orchapi.WorkOpen, Area: "a"},
		{ID: "bd:needle-id", Title: "id match", Status: orchapi.WorkOpen, Area: "a"},
		{ID: "bd:literal", Title: `a 50%_\ match`, Status: orchapi.WorkOpen, Area: "a"},
		{ID: "bd:wildcard", Title: `a 50xxZ match`, Status: orchapi.WorkOpen, Area: "a"},
	}
	for i := range items {
		// The first two rows tie: id ascending decides their order.
		items[i].Updated = time.Unix(100-int64(i/2), 0).UTC()
	}
	if err := mirrorUpsert(ctx, db, "bd", items, 1); err != nil {
		t.Fatal(err)
	}
	if err := mirrorUpsert(ctx, db, "other", []orchapi.WorkItem{{ID: "other:z", Title: "needle", Status: orchapi.WorkOpen}}, 1); err != nil {
		t.Fatal(err)
	}
	area := authz.FromTuples([]authz.Tuple{{Relation: authz.Guest, Object: authz.AreaObject(pid, "a")}}, nil)
	for _, c := range []struct {
		name string
		sc   authz.Scope
		q    mirrorQuery
		want []string
	}{
		{"active status union", authz.All(), mirrorQuery{text: "NEEDLE"}, []string{"bd:a", "bd:b", "bd:c", "bd:needle-id"}},
		{"explicit done", authz.All(), mirrorQuery{text: "needle", status: orchapi.WorkDone}, []string{"bd:d"}},
		{"explicit claimed", authz.All(), mirrorQuery{text: "needle", status: orchapi.WorkClaimed}, []string{"bd:b"}},
		{"limit after status merge", authz.All(), mirrorQuery{text: "needle", limit: 2}, []string{"bd:a", "bd:b"}},
		{"area filter", authz.All(), mirrorQuery{text: "needle", area: "a"}, []string{"bd:a", "bd:c", "bd:needle-id"}},
		{"guest area", area, mirrorQuery{text: "needle"}, []string{"bd:a", "bd:c", "bd:needle-id"}},
		{"guest denied area", area, mirrorQuery{text: "needle", area: "b"}, nil},
		{"no scope", authz.Empty(), mirrorQuery{text: "needle"}, nil},
		{"owner and leased overlay candidates", authz.All(), mirrorQuery{text: "needle", owner: "mini/owner001", leased: []string{"bd:c"}}, []string{"bd:a", "bd:c"}},
		{"literal wildcards and escape", authz.All(), mirrorQuery{text: `%_\`}, []string{"bd:literal"}},
		{"no text keeps default list", authz.All(), mirrorQuery{limit: 2}, []string{"bd:a", "bd:b"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.q.source = "bd"
			if c.q.limit == 0 {
				c.q.limit = 100
			}
			got, err := mirrorList(ctx, db, c.sc, pid, c.q)
			if err != nil {
				t.Fatal(err)
			}
			var gotIDs []string
			for _, it := range got {
				gotIDs = append(gotIDs, it.ID)
			}
			if !reflect.DeepEqual(gotIDs, c.want) {
				t.Fatalf("got %v, want %v", gotIDs, c.want)
			}
		})
	}
}

func TestMirrorTextSearchPlans(t *testing.T) {
	db, pid := mirrorSearchDB(t)
	area := authz.FromTuples([]authz.Tuple{{Relation: authz.Guest, Object: authz.AreaObject(pid, "a")}}, nil)
	for _, c := range []struct {
		name, index string
		sc          authz.Scope
		q           mirrorQuery
	}{
		{"global active text", "work_mirror_status", authz.All(), mirrorQuery{text: "needle"}},
		{"global done text", "work_mirror_status", authz.All(), mirrorQuery{text: "needle", status: orchapi.WorkDone}},
		{"guest text", "work_mirror_area", area, mirrorQuery{text: "needle"}},
		{"area text", "work_mirror_area", authz.All(), mirrorQuery{text: "needle", area: "a"}},
		{"owner text", "work_mirror_owner", authz.All(), mirrorQuery{text: "needle", owner: "mini/owner001"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			c.q.source, c.q.limit = "bd", 50
			query, args := mirrorListSQL(c.sc, pid, c.q)
			rows, err := db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+query, args...)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			joined := strings.Join(plan, "; ")
			if !strings.Contains(joined, "USING INDEX "+c.index) {
				t.Fatalf("want %s: %s", c.index, joined)
			}
			if c.index == "work_mirror_status" && !strings.Contains(joined, "source=? AND status=?") {
				t.Fatalf("text search did not seek status before filtering: %s", joined)
			}
		})
	}
	query, _ := mirrorListSQL(authz.All(), pid, mirrorQuery{source: "bd", limit: 50})
	if strings.Contains(query, "INDEXED BY") {
		t.Fatal("non-text global list changed its index choice")
	}
}
