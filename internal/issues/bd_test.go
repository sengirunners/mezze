package issues_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// Memories and bd sql are project-level: an area-limited member or guest
// neither reads nor writes them; the owner does.
func TestBdMemoriesAndSQLAuthz(t *testing.T) {
	f := newAuthzFixture(t)
	ctx := context.Background()
	o := f.ownerAt("harbor")
	if _, err := orchclient.CreateIssue(ctx, o, orchapi.NewIssueRequest{Title: "secret plan"}); err != nil {
		t.Fatal(err)
	}
	m, err := orchclient.Remember(ctx, o, "k1", "first")
	if err != nil || m.Action != "remembered" {
		t.Fatalf("remember: %+v %v", m, err)
	}
	if m, err = orchclient.Remember(ctx, o, "k1", "second"); err != nil || m.Action != "updated" {
		t.Fatalf("update: %+v %v", m, err)
	}
	ms, err := orchclient.Memories(ctx, o, "harbor")
	if err != nil || ms["k1"] != "second" || len(ms) != 1 {
		t.Fatalf("memories: %v %v", ms, err)
	}
	r, err := orchclient.IssueSQL(ctx, o, "harbor", "select count(*) n from issues")
	if err != nil || len(r.Rows) != 1 {
		t.Fatalf("sql: %+v %v", r, err)
	}
	for _, who := range []string{"member", "guest"} {
		for _, c := range []struct{ method, path, body string }{
			{"GET", "/v0/issues/memories?project=harbor", ""},
			{"PUT", "/v0/issues/memories/k2", `{"value":"x"}`},
			{"DELETE", "/v0/issues/memories/k1", ""},
			{"POST", "/v0/issues/sql?project=harbor", `{"query":"select title from issues"}`},
		} {
			code, body := f.as(who, c.method, c.path, c.body)
			if code == http.StatusOK || strings.Contains(body, "secret plan") || strings.Contains(body, "second") {
				t.Errorf("%s %s %s: %d %s", who, c.method, c.path, code, body)
			}
		}
	}
	if ms, _ := orchclient.Memories(ctx, o, "harbor"); ms["k1"] != "second" || len(ms) != 1 {
		t.Fatalf("memories changed by a refused caller: %v", ms)
	}
	if m, err := orchclient.Forget(ctx, o, "k1"); err != nil || m.Value != "second" {
		t.Fatalf("forget: %+v %v", m, err)
	}
	if _, err := orchclient.Forget(ctx, o, "k1"); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("forget again: %v", err)
	}
	for _, bad := range []string{"", " ", "a\nb", strings.Repeat("k", 246)} {
		if _, err := orchclient.Remember(ctx, o, bad, "x"); err == nil {
			t.Errorf("key %q accepted", bad)
		}
	}
	// bd accepts any key beads' config table holds, slashes included.
	if _, err := orchclient.Remember(ctx, o, "a/b c", "slashed"); err != nil {
		t.Fatal(err)
	}
	if ms, _ := orchclient.Memories(ctx, o, "harbor"); ms["a/b c"] != "slashed" {
		t.Fatalf("slashed key: %v", ms)
	}
	if _, err := orchclient.Forget(ctx, o, "a/b c"); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Remember(ctx, o, "k3", "  "); err == nil {
		t.Error("empty memory accepted")
	}
}

// bd sql: bd's tables and columns, DATETIME text, read-only, one statement.
func TestBdSQL(t *testing.T) {
	f := newAuthzFixture(t)
	ctx := context.Background()
	o := f.ownerAt("harbor")
	ep, err := orchclient.CreateIssue(ctx, o, orchapi.NewIssueRequest{Title: "epic", IssueType: "epic", Labels: []string{"x", "y"}})
	if err != nil {
		t.Fatal(err)
	}
	var kids []string
	for _, title := range []string{"a", "b", "c"} {
		k, err := orchclient.CreateIssue(ctx, o, orchapi.NewIssueRequest{Title: title, Parent: ep.ID})
		if err != nil {
			t.Fatal(err)
		}
		kids = append(kids, k.ID)
	}
	if _, err := orchclient.ClaimIssue(ctx, o, kids[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.ClaimIssue(ctx, o, ep.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.CommentIssue(ctx, o, kids[1], orchapi.IssueCommentRequest{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	// An agent mail tool's in-flight query, verbatim in shape.
	working := "select id from issues where status = 'in_progress'"
	q := `select i.id, i.title, coalesce(i.assignee, '') assignee, i.priority, i.updated_at, i.issue_type,
      coalesce(k.kids_open, 0) kids_open, coalesce(k.kids_working, 0) kids_working, coalesce(k.kids_total, 0) kids_total,
      coalesce(lb.labels, '') labels
    from issues i
    left join (select d.depends_on_issue_id pid, sum(c.status in ('open', 'blocked')) kids_open,
        sum(c.status = 'in_progress') kids_working, count(*) kids_total
      from dependencies d join issues c on c.id = d.issue_id
      where d.type = 'parent-child' and d.depends_on_issue_id in (` + working + `) group by d.depends_on_issue_id) k on k.pid = i.id
    left join (select l.issue_id, group_concat(l.label) labels from labels l
      where l.issue_id in (` + working + `) group by l.issue_id) lb on lb.issue_id = i.id
    where i.status = 'in_progress'`
	r, err := orchclient.IssueSQL(ctx, o, "harbor", q)
	if err != nil {
		t.Fatal(err)
	}
	rows := byCol(r)
	if len(rows) != 2 {
		t.Fatalf("rows: %+v", rows)
	}
	for _, row := range rows {
		if row["id"] == ep.ID {
			if row["kids_open"] != float64(2) || row["kids_working"] != float64(1) || row["kids_total"] != float64(3) || row["labels"] != "x,y" {
				t.Errorf("epic row: %+v", row)
			}
			if s, _ := row["updated_at"].(string); len(s) != 20 || s[10] != 'T' || s[19] != 'Z' {
				t.Errorf("updated_at %q is not bd's DATETIME form", row["updated_at"])
			}
		}
	}
	// test-prune / daily-summary: children of an epic; comments and events exist under bd's names.
	r, err = orchclient.IssueSQL(ctx, o, "harbor", "select i.id, i.title, i.status from issues i join dependencies d on d.issue_id = i.id where d.type = 'parent-child' and d.depends_on_issue_id = '"+ep.ID+"' order by i.created_at desc limit 60;")
	if err != nil || len(r.Rows) != 3 {
		t.Fatalf("children: %+v %v", r, err)
	}
	r, err = orchclient.IssueSQL(ctx, o, "harbor", "select c.text, e.event_type from comments c join events e on e.issue_id = c.issue_id where c.issue_id = '"+kids[1]+"' and e.event_type = 'created'")
	if err != nil || len(r.Rows) != 1 || r.Rows[0][0] != "hello" {
		t.Fatalf("comments/events: %+v %v", r, err)
	}
	// MySQL-style comparison with a DATETIME literal.
	r, err = orchclient.IssueSQL(ctx, o, "harbor", "select count(*) n from issues where created_at > '2000-01-01 00:00:00' and external_ref is null and assignee is null")
	if err != nil || r.Rows[0][0] != float64(2) {
		t.Fatalf("datetime/null compare: %+v %v", r, err)
	}
	for _, bad := range []string{
		"delete from issues",
		"update issues set title = 'x'",
		"select 1; delete from issues",
		"select 1; select 2",
		"pragma table_info(issues)",
		"attach database '/tmp/x.db' as x",
		"/* x */ delete from issues",
		"with x as (select 1) delete from issues",
		"with x as (select 1) insert into labels select 'a', 'b'",
	} {
		if _, err := orchclient.IssueSQL(ctx, o, "harbor", bad); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// A semicolon inside a string or a comment is not a second statement.
	if r, err := orchclient.IssueSQL(ctx, o, "harbor", "select 'a;b' s -- c;d\n"); err != nil || r.Rows[0][0] != "a;b" {
		t.Errorf("quoted semicolon: %+v %v", r, err)
	}
	// The query sees only the bd tables: not the native columns zm redacts
	// (session addresses), not other native tables, not the file's path.
	for _, q := range []string{"select author_addr from main.comments", "select created_by_addr from issues",
		"select claimed_by from main.issues", "select actor_addr from history", "select * from main.kv", "select * from src.issues"} {
		if _, err := orchclient.IssueSQL(ctx, o, "harbor", q); !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%q: %v", q, err)
		}
	}
	if r, err := orchclient.IssueSQL(ctx, o, "harbor", "select group_concat(file) f from pragma_database_list"); err != nil || strings.Contains(fmt.Sprint(r.Rows), "issues.db") {
		t.Errorf("database list: %+v %v", r, err)
	}
	// Only time columns (and expressions over them) are rewritten; replace() is a function, not a write.
	if _, err := orchclient.UpdateIssue(ctx, o, kids[2], orchapi.UpdateIssueRequest{Title: ptr("2026-01-01 00:00:00")}); err != nil {
		t.Fatal(err)
	}
	r, err = orchclient.IssueSQL(ctx, o, "harbor", "select title, max(created_at) m, replace(title, '-', '/') r from issues where id = '"+kids[2]+"'")
	if err != nil || r.Rows[0][0] != "2026-01-01 00:00:00" || !strings.HasSuffix(r.Rows[0][1].(string), "Z") || r.Rows[0][2] != "2026/01/01 00:00:00" {
		t.Errorf("datetime rewrite: %+v %v", r, err)
	}
	// Still four issues: nothing above wrote.
	if r, _ := orchclient.IssueSQL(ctx, o, "harbor", "select count(*) from issues"); r.Rows[0][0] != float64(4) {
		t.Errorf("count after refused writes: %+v", r)
	}
}

func byCol(r orchapi.SQLResult) []map[string]any {
	// Round-trip through JSON, as the client sees it.
	b, _ := json.Marshal(r)
	var x orchapi.SQLResult
	_ = json.Unmarshal(b, &x)
	var out []map[string]any
	for _, row := range x.Rows {
		m := map[string]any{}
		for i, c := range x.Columns {
			m[c] = row[i]
		}
		out = append(out, m)
	}
	return out
}

func ptr(s string) *string { return &s }
