package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
)

// TestStoreSpeed times store-level queries on a copy of an imported
// issues.db (ZM_ISSUES_DB=<path to a copy>); skipped otherwise.
func TestStoreSpeed(t *testing.T) {
	src := os.Getenv("ZM_ISSUES_DB")
	if src == "" {
		t.Skip("ZM_ISSUES_DB not set")
	}
	home := t.TempDir()
	dir := filepath.Join(home, "projects", "P")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, FileName), b, 0o600); err != nil {
		t.Fatal(err)
	}
	s := Open(home)
	defer s.Close()
	ctx := context.Background()
	p, err := s.Project(ctx, "P", false)
	if err != nil {
		t.Fatal(err)
	}
	all := istore.Areas{All: true}
	now := time.Now()
	run := func(name string, q orchapi.IssueQuery) {
		var ds []time.Duration
		for i := 0; i < 100; i++ {
			t0 := time.Now()
			if _, err := p.List(ctx, q, all, now); err != nil {
				t.Fatal(err)
			}
			ds = append(ds, time.Since(t0))
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		t.Logf("%-20s p50 %v p95 %v", name, ds[50], ds[95])
	}
	run("default", orchapi.IssueQuery{})
	run("default+count", orchapi.IssueQuery{Count: true})
	run("default+deps", orchapi.IssueQuery{Deps: true})
	run("ready100", orchapi.IssueQuery{Ready: true, Limit: 100, Deps: true})
	run("fts call", orchapi.IssueQuery{Q: "call", Status: "all"})
	run("fts solver", orchapi.IssueQuery{Q: "solver", Status: "all"})
	run("substr call", orchapi.IssueQuery{Substr: "call", Status: "open,in_progress,blocked,deferred"})
}

func TestStoreWriteSpeed(t *testing.T) {
	src := os.Getenv("ZM_ISSUES_DB")
	if src == "" {
		t.Skip("ZM_ISSUES_DB not set")
	}
	home := t.TempDir()
	dir := filepath.Join(home, "projects", "P")
	_ = os.MkdirAll(dir, 0o700)
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, FileName), b, 0o600)
	s := Open(home)
	defer s.Close()
	ctx := context.Background()
	p, err := s.Project(ctx, "P", false)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	var ds []time.Duration
	for i := 0; i < 300; i++ {
		t0 := time.Now()
		r, err := p.Apply(ctx, istore.Mutation{Op: istore.OpCreate, Actor: istore.Actor{Name: "a"}, Create: &orchapi.NewIssueRequest{Title: "bench", Labels: []string{"bench"}}})
		if err != nil {
			t.Fatal(err)
		}
		ds = append(ds, time.Since(t0))
		ids = append(ids, r.Issue.ID)
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	t.Logf("create p50 %v p95 %v max %v", ds[150], ds[285], ds[299])
	ds = ds[:0]
	for i := 0; i < 300; i++ {
		n := "x"
		t0 := time.Now()
		if _, err := p.Apply(ctx, istore.Mutation{Op: istore.OpUpdate, ID: ids[i], Actor: istore.Actor{Name: "a"}, Update: &orchapi.UpdateIssueRequest{Notes: &n}}); err != nil {
			t.Fatal(err)
		}
		ds = append(ds, time.Since(t0))
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	t.Logf("update p50 %v p95 %v max %v", ds[150], ds[285], ds[299])
}
