package sqlite

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
)

// genVocab is a corpus vocabulary: common words first, so a Zipf draw makes
// a few words very common (in thousands of issues) and most rare.
var genVocab = func() []string {
	base := strings.Fields(`the a to of and in for is on that with call schedule solver fix test weekend resident shift
	board rotation block lane night holiday attending program draft publish import export api error timeout cache sync
	page view button grid month week day year swap trade request approval policy rule config migration database index
	query latency budget p95 bench replica authority claim lease session agent mail hub bead issue label status parent
	child epic task bug feature chore review merge ci deploy prod staging worker queue job retry backoff fairness target
	coverage census preview confirm undo history audit token secret redact search rank filter sort limit total count`)
	out := append([]string{}, base...)
	r := rand.New(rand.NewSource(7))
	const letters = "abcdefghijklmnopqrstuvwxyz"
	for len(out) < 4000 {
		n := 4 + r.Intn(7)
		b := make([]byte, n)
		for i := range b {
			b[i] = letters[r.Intn(len(letters))]
		}
		out = append(out, string(b))
	}
	return out
}()

// genStore builds a project store of n issues shaped like harbor's beads:
// most closed, titles of 5-12 words, bodies of 30-400 words, some notes,
// 0-3 labels, some children. One transaction, straight into the tables
// (the FTS index fills from its triggers).
func genStore(t testing.TB, n int) (*project, func()) {
	t.Helper()
	home, err := os.MkdirTemp("", "zmgen")
	if err != nil {
		t.Fatal(err)
	}
	s := Open(home)
	ctx := context.Background()
	ps, err := s.Project(ctx, "01GEN", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.SetPrefix(ctx, "oc"); err != nil {
		t.Fatal(err)
	}
	p := ps.(*project)
	r := rand.New(rand.NewSource(42))
	zipf := rand.NewZipf(r, 1.1, 2, uint64(len(genVocab)-1))
	words := func(k int) string {
		ws := make([]string, k)
		for i := range ws {
			ws[i] = genVocab[zipf.Uint64()]
		}
		return strings.Join(ws, " ")
	}
	labels := []string{"area:solver", "area:call", "area:block", "area:web", "zm", "bd-zm", "perf", "ux", "docs", "infra", "ci", "security"}
	types := []string{"task", "task", "task", "bug", "bug", "feature", "chore", "epic"}
	tx, err := p.w.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	ins, err := tx.PrepareContext(ctx, `INSERT INTO issues (id, status, priority, issue_type, parent, created_at, updated_at, title, description, notes, number)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	lab, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO labels (issue_id, label) VALUES (?, ?)`)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().Add(-365 * 24 * time.Hour).UnixMilli()
	var epics []string
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("oc-g%05d", i)
		typ := types[r.Intn(len(types))]
		parent := ""
		if typ != "epic" && len(epics) > 0 && r.Intn(4) == 0 {
			parent = epics[r.Intn(len(epics))]
			id = fmt.Sprintf("%s.%d", parent, i)
		}
		if typ == "epic" {
			epics = append(epics, id)
		}
		status := "closed"
		switch x := r.Intn(100); {
		case x < 10:
			status = "open"
		case x < 13:
			status = "in_progress"
		case x < 15:
			status = "blocked"
		}
		body := words(30 + int(math.Abs(r.NormFloat64())*150))
		notes := ""
		if r.Intn(3) == 0 {
			notes = words(10 + r.Intn(60))
		}
		at := t0 + int64(i)*2700_000
		if _, err := ins.ExecContext(ctx, id, status, r.Intn(5), typ, parent, at, at, words(5+r.Intn(8)), body, notes, i+1); err != nil {
			t.Fatal(err)
		}
		for k := r.Intn(4); k > 0; k-- {
			if _, err := lab.ExecContext(ctx, id, labels[r.Intn(len(labels))]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.w.ExecContext(ctx, `INSERT INTO issues_fts (issues_fts) VALUES ('optimize')`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.w.ExecContext(ctx, `ANALYZE`); err != nil {
		t.Fatal(err)
	}
	return p, func() { s.Close(); os.RemoveAll(home) }
}

var _ istore.ProjectStore = (*project)(nil)
