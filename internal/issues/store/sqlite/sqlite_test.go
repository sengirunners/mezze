package sqlite

import (
	"context"
	"os"
	"strings"
	"testing"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
)

func testProject(t *testing.T) istore.ProjectStore {
	t.Helper()
	home, err := os.MkdirTemp("", "zmis")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	s := Open(home)
	t.Cleanup(func() { s.Close() })
	p, err := s.Project(context.Background(), "01PROJ", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetPrefix(context.Background(), "oc"); err != nil {
		t.Fatal(err)
	}
	return p
}

var ana = istore.Actor{Name: "Ana", Address: "human:ana"}
var bob = istore.Actor{Name: "Bob", Address: "mini/0000bbbb"}

func apply(t *testing.T, p istore.ProjectStore, m istore.Mutation) istore.Result {
	t.Helper()
	if m.Actor.Name == "" {
		m.Actor = ana
	}
	r, err := p.Apply(context.Background(), m)
	if err != nil {
		t.Fatalf("%s %s: %v", m.Op, m.ID, err)
	}
	return r
}

func create(t *testing.T, p istore.ProjectStore, req orchapi.NewIssueRequest) orchapi.Issue {
	t.Helper()
	return apply(t, p, istore.Mutation{Op: istore.OpCreate, Create: &req}).Issue
}

func idList(its []orchapi.Issue) string {
	var out []string
	for _, it := range its {
		out = append(out, it.ID)
	}
	return strings.Join(out, " ")
}

// TestHandleLen: one new mint hits an existing handle with probability <= 1/1000.
func TestHandleLen(t *testing.T) {
	for n, want := range map[int]int{0: 3, 29: 3, 30: 4, 919: 4, 920: 5, 28596: 5, 28597: 6, 887241: 6, 887242: 7, 1e9: 8} {
		if got := handleLen(n); got != want {
			t.Errorf("handleLen(%d) = %d, want %d", n, got, want)
		}
	}
}

// TestNumberBackfill: migration 0002 numbers rows that were created before it,
// in creation order (the same statement numbers imported issues).
func TestNumberBackfill(t *testing.T) {
	ctx := context.Background()
	p := testProject(t).(*project)
	var ids []string
	for i := 0; i < 3; i++ {
		ids = append(ids, create(t, p, orchapi.NewIssueRequest{Title: "x"}).ID)
	}
	// Make the creation order differ from the insert order, then forget the numbers.
	if _, err := p.w.Exec(`UPDATE issues SET created_at = 1000 - number, number = NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.w.Exec(migrations[1].sql[strings.Index(migrations[1].sql, "UPDATE"):strings.Index(migrations[1].sql, "CREATE UNIQUE")]); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		it, err := p.Get(ctx, id, istore.Areas{All: true})
		if err != nil || it.Number != 3-i {
			t.Fatalf("%s number %d, want %d", id, it.Number, 3-i)
		}
	}
}
