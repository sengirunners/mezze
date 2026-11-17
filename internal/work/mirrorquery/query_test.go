package mirrorquery

import (
	"reflect"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/authz"
)

func TestListDefaultAndLiteralSearch(t *testing.T) {
	query, args := List(Query{Scope: authz.All(), Project: "project", Source: "bd", Limit: 50})
	want := `SELECT id, area, status, title, body, item FROM work_mirror WHERE source = ? AND 1=1 AND status IN ('open', 'claimed', 'in_review') ORDER BY updated DESC, id LIMIT ?`
	if query != want || !reflect.DeepEqual(args, []any{"bd", 50}) {
		t.Fatalf("default query %s args %v", query, args)
	}
	query, args = List(Query{Scope: authz.All(), Project: "project", Source: "bd", Status: orchapi.WorkDone, Text: `100%_\\`, Limit: 50})
	pattern := `%100\%\_\\\\%`
	if !strings.Contains(query, "INDEXED BY work_mirror_status") || !strings.Contains(query, "status = ?") ||
		!reflect.DeepEqual(args, []any{"bd", "done", pattern, pattern, pattern, 50}) {
		t.Fatalf("literal query %s args %v", query, args)
	}
}

func TestListKeepsScopeAndOwnerParameters(t *testing.T) {
	sc := authz.FromTuples([]authz.Tuple{{Relation: authz.Guest, Object: authz.AreaObject("project", "solver")}}, nil)
	query, args := List(Query{Scope: sc, Project: "project", Source: "bd", Area: "billing", Status: orchapi.WorkClaimed,
		Owner: "mini/owner001", Leased: []string{"bd:one", "lin:two"}, Limit: 9})
	if !strings.Contains(query, "INDEXED BY work_mirror_owner") || !strings.Contains(query, "area IN (?)") ||
		!strings.Contains(query, "area = ?") || !strings.Contains(query, "(owner = ? OR ref IN (?, ?))") ||
		!reflect.DeepEqual(args, []any{"bd", "solver", "claimed", "billing", "mini/owner001", "one", "lin:two", 9}) {
		t.Fatalf("scoped owner query %s args %v", query, args)
	}
	query, _ = List(Query{Scope: authz.Empty(), Project: "project", Source: "bd", Text: "hidden", Limit: 9})
	if !strings.Contains(query, "INDEXED BY work_mirror_area") || !strings.Contains(query, "AND 0=1 AND") {
		t.Fatalf("empty scope did not retain denial: %s", query)
	}
}
