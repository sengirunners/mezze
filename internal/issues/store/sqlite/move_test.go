package sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

func twoProjects(t *testing.T) (src, dst istore.ProjectStore) {
	t.Helper()
	home, err := os.MkdirTemp("", "zmis")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	s := Open(home)
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	for i, pre := range []string{"oc", "sg"} {
		p, err := s.Project(ctx, []string{"01SRC", "01DST"}[i], true)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.SetPrefix(ctx, pre); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			src = p
		} else {
			dst = p
		}
	}
	return src, dst
}

// moveOne moves id from src to dst as newID, as the service does.
func moveOne(t *testing.T, src, dst istore.ProjectStore, id, newID string, rename map[string]string) istore.Result {
	t.Helper()
	return moveVia(t, src, "01SRC", dst, "01DST", id, newID, rename)
}

// moveVia moves id from store a (project aID) to b (bID) as newID: move
// in, move out, done.
func moveVia(t *testing.T, a istore.ProjectStore, aID string, b istore.ProjectStore, bID, id, newID string, rename map[string]string) istore.Result {
	t.Helper()
	ctx := context.Background()
	mv, err := a.(istore.Mover).Export(ctx, id)
	if err != nil {
		t.Fatalf("export %s: %v", id, err)
	}
	apply(t, b, istore.Mutation{Op: istore.OpMoveIn, ID: id, Move: &istore.Move{Project: aID, NewID: newID, From: &mv, Rename: rename, HomeArea: mv.HomeArea}})
	r := apply(t, a, istore.Mutation{Op: istore.OpMoveOut, ID: id, Move: &istore.Move{Project: bID, NewID: newID}})
	apply(t, b, istore.Mutation{Op: istore.OpMoveDone, ID: id, Move: &istore.Move{Project: aID, NewID: newID}})
	return r
}

func depIDs(ls []orchapi.IssueLink) string {
	var out []string
	for _, l := range ls {
		out = append(out, l.DependencyType+":"+l.Issue.ID)
	}
	return strings.Join(out, " ")
}

func TestMoveBetweenProjects(t *testing.T) {
	ctx := context.Background()
	all := istore.Areas{All: true}
	src, dst := twoProjects(t)
	x := create(t, src, orchapi.NewIssueRequest{Title: "Stays: the blocker"})
	e := create(t, src, orchapi.NewIssueRequest{Title: "Epic", IssueType: "epic", Labels: []string{"sengi"}})
	c1 := create(t, src, orchapi.NewIssueRequest{Title: "Child one", Parent: e.ID, Deps: []string{x.ID}})
	c2 := create(t, src, orchapi.NewIssueRequest{Title: "Child two (closed, stays)", Parent: e.ID})
	apply(t, src, istore.Mutation{Op: istore.OpClose, ID: c2.ID, Close: &orchapi.CloseIssueRequest{Reason: "done"}})
	y := create(t, src, orchapi.NewIssueRequest{Title: "Stays: waits on child one", Deps: []string{c1.ID}})
	cm := apply(t, src, istore.Mutation{Op: istore.OpComment, ID: c1.ID, Actor: bob, Comment: &orchapi.IssueCommentRequest{Text: "progress"}}).Comment
	if !y.Blocked {
		t.Fatalf("y should start blocked: %+v", y)
	}

	ne, nc1 := "sg-"+strings.TrimPrefix(e.ID, "oc-"), "sg-"+strings.TrimPrefix(c1.ID, "oc-")
	rename := map[string]string{e.ID: ne, c1.ID: nc1}
	out := moveOne(t, src, dst, e.ID, ne, rename)
	if out.Issue.ID != e.ID || len(out.Changes) != 1 || out.Changes[0].Kind != "moved" || out.Noop {
		t.Fatalf("move out of the epic: %+v", out)
	}
	out = moveOne(t, src, dst, c1.ID, nc1, rename)
	if got := idList(out.Touched); got != y.ID {
		t.Fatalf("touched %q, want %s (no longer blocked)", got, y.ID)
	}

	// The source: redirects, nothing left of the moved issues, edges renamed.
	for old, nw := range rename {
		if _, err := src.Get(ctx, old, all); !errors.Is(err, istore.ErrNotFound) {
			t.Fatalf("%s still in the source: %v", old, err)
		}
		to, p, ok, err := src.(istore.Mover).MovedTo(ctx, old)
		if err != nil || !ok || to != nw || p != "01DST" {
			t.Fatalf("redirect %s = %s %s %v %v", old, to, p, ok, err)
		}
		if h, _ := src.History(ctx, old, all); len(h) != 0 {
			t.Fatalf("history of %s stays readable in the source: %+v", old, h)
		}
	}
	yd, err := src.Detail(ctx, y.ID, all)
	if err != nil || yd.Blocked || depIDs(yd.DependsOn) != "blocks:"+nc1 {
		t.Fatalf("y after: blocked %v deps %q %v", yd.Blocked, depIDs(yd.DependsOn), err)
	}
	cd, err := src.Detail(ctx, c2.ID, all)
	if err != nil || cd.Parent != "" || depIDs(cd.DependsOn) != "parent-child:"+ne {
		t.Fatalf("closed child: parent %q deps %q %v", cd.Parent, depIDs(cd.DependsOn), err)
	}
	if cs, _ := src.SearchComments(ctx, orchapi.CommentQuery{Contains: "progress"}, all); len(cs) != 0 {
		t.Fatalf("comments left in the source: %+v", cs)
	}

	// The target: the issues, their labels, comments, history and edges.
	d, err := dst.Detail(ctx, nc1, all)
	if err != nil {
		t.Fatal(err)
	}
	if d.Parent != ne || d.Title != "Child one" || d.Blocked || d.Number != 2 || d.CreatedBy != "Ana" || !d.CreatedAt.Equal(c1.CreatedAt) {
		t.Fatalf("moved child %+v", d.Issue)
	}
	if got := depIDs(d.DependsOn); got != "blocks:"+x.ID+" parent-child:"+ne && got != "parent-child:"+ne+" blocks:"+x.ID {
		t.Fatalf("moved child's edges %q (the blocker stays a cross-project edge)", got)
	}
	if len(d.Comments) != 1 || d.Comments[0].ID != cm.ID || d.Comments[0].Author != "Bob" || d.Comments[0].IssueID != nc1 {
		t.Fatalf("comments %+v", d.Comments)
	}
	h, err := dst.History(ctx, nc1, all)
	if err != nil || len(h) < 3 || h[0].Kind != "created" || h[len(h)-1].Kind != "moved" {
		t.Fatalf("history %+v %v", h, err)
	}
	for i := 1; i < len(h); i++ {
		if h[i].ID <= h[i-1].ID {
			t.Fatalf("history ids out of commit order: %+v", h)
		}
	}
	if from, p, ok, _ := dst.(istore.Mover).MovedFrom(ctx, nc1); !ok || from != c1.ID || p != "01SRC" {
		t.Fatalf("moved from %s %s %v", from, p, ok)
	}
	ed, _ := dst.Detail(ctx, ne, all)
	if strings.Join(ed.Labels, ",") != "sengi" || depIDs(ed.Dependents) != "parent-child:"+nc1 {
		t.Fatalf("moved epic: labels %v dependents %q", ed.Labels, depIDs(ed.Dependents))
	}
	// The child counter came along: no reused child number.
	if n := create(t, dst, orchapi.NewIssueRequest{Title: "New child", Parent: ne}); n.ID != ne+".3" {
		t.Fatalf("new child %s, want %s.3", n.ID, ne)
	}

	// Again: move out is a no-op; move in rewrites its own copy.
	if r := moveOne(t, src, dst, x.ID, "sg-"+strings.TrimPrefix(x.ID, "oc-"), nil); r.Noop {
		t.Fatalf("x moved: %+v", r)
	}
	if r := apply(t, src, istore.Mutation{Op: istore.OpMoveOut, ID: c1.ID, Move: &istore.Move{Project: "01DST", NewID: nc1}}); !r.Noop {
		t.Fatalf("second move out: %+v", r)
	}
	nd, _ := dst.Detail(ctx, nc1, all)
	if !strings.Contains(depIDs(nd.DependsOn), "blocks:sg-"+strings.TrimPrefix(x.ID, "oc-")) {
		t.Fatalf("the cross-project edge did not find x once it moved in: %q", depIDs(nd.DependsOn))
	}
	if !nd.Blocked {
		t.Fatalf("child one waits on x again (both in the target): %+v", nd.Issue)
	}
}

func TestMoveInRefusals(t *testing.T) {
	ctx := context.Background()
	src, dst := twoProjects(t)
	a := create(t, src, orchapi.NewIssueRequest{Title: "A"})
	mv, err := src.(istore.Mover).Export(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	taken := create(t, dst, orchapi.NewIssueRequest{Title: "Already here"})
	in := func(newID string) error {
		_, err := dst.Apply(ctx, istore.Mutation{Op: istore.OpMoveIn, ID: a.ID, Actor: ana, Move: &istore.Move{Project: "01SRC", NewID: newID, From: &mv}})
		return err
	}
	if err := in(taken.ID); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("taken id: %v", err)
	}
	nid := "sg-" + strings.TrimPrefix(a.ID, "oc-")
	if err := in(nid); err != nil {
		t.Fatal(err)
	}
	if err := in(nid); err != nil {
		t.Fatalf("a resumed move in rewrites its copy: %v", err)
	}
	if n, _ := dst.Counts(ctx); n["issues"] != 2 {
		t.Fatalf("counts after a rerun %v", n)
	}
	// A pending copy takes no writes.
	if _, err := dst.Apply(ctx, istore.Mutation{Op: istore.OpComment, ID: nid, Actor: ana, Comment: &orchapi.IssueCommentRequest{Text: "to the copy"}}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a write to a pending copy: %v", err)
	}
	apply(t, dst, istore.Mutation{Op: istore.OpMoveDone, ID: a.ID, Move: &istore.Move{Project: "01SRC", NewID: nid}})
	apply(t, dst, istore.Mutation{Op: istore.OpComment, ID: nid, Comment: &orchapi.IssueCommentRequest{Text: "written to the copy"}})
	if err := in(nid); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a copy with its own writes: %v", err)
	}
	if r := apply(t, dst, istore.Mutation{Op: istore.OpMoveDone, ID: a.ID, Move: &istore.Move{Project: "01SRC", NewID: nid}}); !r.Noop {
		t.Fatalf("done again: %+v", r)
	}
	if _, err := dst.Apply(ctx, istore.Mutation{Op: istore.OpMoveIn, ID: a.ID, Actor: ana, Move: &istore.Move{Project: "01SRC", NewID: "sg-zz"}}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("no export: %v", err)
	}
	if _, err := src.(istore.Mover).Export(ctx, "oc-nope"); !errors.Is(err, istore.ErrNotFound) {
		t.Fatalf("export of a missing issue: %v", err)
	}
}

// A crash between the copy and the removal: the copy refuses writes, the
// original takes them, and a rerun copies it again and finishes.
func TestMoveResumesAfterACrash(t *testing.T) {
	ctx := context.Background()
	all := istore.Areas{All: true}
	src, dst := twoProjects(t)
	a := create(t, src, orchapi.NewIssueRequest{Title: "A", ID: "oc-abc"})
	nid := "sg-abc"
	mv, err := src.(istore.Mover).Export(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	apply(t, dst, istore.Mutation{Op: istore.OpMoveIn, ID: a.ID, Move: &istore.Move{Project: "01SRC", NewID: nid, From: &mv}})
	// crash here: no move out, no done; the holds are gone.
	if _, err := dst.Apply(ctx, istore.Mutation{Op: istore.OpUpdate, ID: nid, Actor: ana, Update: &orchapi.UpdateIssueRequest{Title: ptr("lost")}}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a write to the half-moved copy: %v", err)
	}
	apply(t, src, istore.Mutation{Op: istore.OpComment, ID: a.ID, Comment: &orchapi.IssueCommentRequest{Text: "after the crash"}})
	moveOne(t, src, dst, a.ID, nid, nil)
	d, err := dst.Detail(ctx, nid, all)
	if err != nil || len(d.Comments) != 1 || d.Comments[0].Text != "after the crash" {
		t.Fatalf("the rerun copied the original as it is now: %+v %v", d.Comments, err)
	}
	apply(t, dst, istore.Mutation{Op: istore.OpUpdate, ID: nid, Update: &orchapi.UpdateIssueRequest{Title: ptr("now it takes writes")}})
	// A moved id is not made again.
	if _, err := src.Apply(ctx, istore.Mutation{Op: istore.OpCreate, Actor: ana, Create: &orchapi.NewIssueRequest{Title: "again", ID: a.ID}}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("create --id of a moved id: %v", err)
	}
}

// There and back and there again: each move leaves exactly one live issue
// and one redirect pointing at it.
func TestMoveRoundTrip(t *testing.T) {
	ctx := context.Background()
	all := istore.Areas{All: true}
	src, dst := twoProjects(t)
	create(t, src, orchapi.NewIssueRequest{Title: "Wanderer", ID: "oc-wnd"})
	check := func(live istore.ProjectStore, liveID string, gone istore.ProjectStore, goneID string) {
		t.Helper()
		if _, err := live.Get(ctx, liveID, all); err != nil {
			t.Fatalf("%s: %v", liveID, err)
		}
		if _, err := gone.Get(ctx, goneID, all); !errors.Is(err, istore.ErrNotFound) {
			t.Fatalf("%s is still there: %v", goneID, err)
		}
		if to, _, ok, _ := gone.(istore.Mover).MovedTo(ctx, goneID); !ok || to != liveID {
			t.Fatalf("redirect %s -> %q %v", goneID, to, ok)
		}
		if _, _, ok, _ := live.(istore.Mover).MovedTo(ctx, liveID); ok {
			t.Fatalf("%s is live and still redirects", liveID)
		}
	}
	moveVia(t, src, "01SRC", dst, "01DST", "oc-wnd", "sg-wnd", nil)
	check(dst, "sg-wnd", src, "oc-wnd")
	moveVia(t, dst, "01DST", src, "01SRC", "sg-wnd", "oc-wnd", nil)
	check(src, "oc-wnd", dst, "sg-wnd")
	if r := moveVia(t, src, "01SRC", dst, "01DST", "oc-wnd", "sg-wnd", nil); r.Noop {
		t.Fatalf("the third move did nothing: %+v", r)
	}
	check(dst, "sg-wnd", src, "oc-wnd")
	if h, _ := dst.History(ctx, "sg-wnd", all); h[0].Kind != "created" {
		t.Fatalf("history after three moves %+v", h)
	}
}

// A third stop changes the returning id, but dependencies left at home still
// name the first stop. Those aliases become local edges when the issue returns.
func TestMoveRoundTripThroughThirdProjectRewritesEdges(t *testing.T) {
	ctx := context.Background()
	all := istore.Areas{All: true}
	src, dst := twoProjects(t)
	thirdStore := Open(t.TempDir())
	t.Cleanup(func() { thirdStore.Close() })
	third, err := thirdStore.Project(ctx, "01THIRD", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := third.SetPrefix(ctx, "th"); err != nil {
		t.Fatal(err)
	}
	create(t, src, orchapi.NewIssueRequest{Title: "Wanderer", ID: "oc-wnd"})
	create(t, src, orchapi.NewIssueRequest{Title: "Stays elsewhere", ID: "oc-ext"})
	blocker := create(t, src, orchapi.NewIssueRequest{Title: "Waits here", Deps: []string{"oc-wnd", "oc-ext"}})
	child := create(t, src, orchapi.NewIssueRequest{Title: "Child stays here", Parent: "oc-wnd"})
	moveVia(t, src, "01SRC", dst, "01DST", "oc-ext", "sg-ext", nil)
	moveVia(t, src, "01SRC", dst, "01DST", "oc-wnd", "sg-wnd", nil)
	moveVia(t, dst, "01DST", third, "01THIRD", "sg-wnd", "th-wnd", nil)
	moveVia(t, third, "01THIRD", src, "01SRC", "th-wnd", "oc-wnd", nil)

	d, err := src.Detail(ctx, blocker.ID, all)
	if err != nil || !d.Blocked || len(d.DependsOn) != 2 {
		t.Fatalf("returned dependency detail %+v: %v", d, err)
	}
	if got := depIDs(d.DependsOn); !strings.Contains(got, "blocks:oc-wnd") || !strings.Contains(got, "blocks:sg-ext") || strings.Contains(got, "sg-wnd") {
		t.Fatalf("returned dependency ids %q", got)
	}
	cd, err := src.Detail(ctx, child.ID, all)
	if err != nil || cd.Parent != "oc-wnd" || depIDs(cd.DependsOn) != "parent-child:oc-wnd" {
		t.Fatalf("returned parent %+v: %v", cd, err)
	}
	ls, err := src.List(ctx, orchapi.IssueQuery{Status: "all", Deps: true}, all, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, it := range ls.Issues {
		if it.ID != blocker.ID {
			continue
		}
		found = true
		if len(it.Dependencies) != 2 {
			t.Fatalf("list dependencies %+v", it.Dependencies)
		}
		for _, dep := range it.Dependencies {
			if (dep.DependsOnID != "oc-wnd" || dep.External) && (dep.DependsOnID != "sg-ext" || !dep.External) {
				t.Errorf("list dependency %+v", dep)
			}
		}
	}
	if !found {
		t.Fatal("dependent missing from list")
	}
	q := fmt.Sprintf(`SELECT depends_on_issue_id, depends_on_external FROM dependencies WHERE issue_id = '%s' ORDER BY coalesce(depends_on_issue_id, depends_on_external)`, blocker.ID)
	r, err := src.(istore.SQLReader).ReadSQL(ctx, q, 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Rows) != 2 || r.Rows[0][0] != "oc-wnd" || r.Rows[0][1] != nil || r.Rows[1][0] != nil || r.Rows[1][1] != "sg-ext" {
		t.Fatalf("SQL dependencies %+v", r.Rows)
	}
}

// Every column the move copies arrives as it was; source ids (beads'
// idempotency keys) are kept, and the home area when the service keeps it.
func TestMoveCopiesEveryColumn(t *testing.T) {
	ctx := context.Background()
	src, dst := twoProjects(t)
	a := create(t, src, orchapi.NewIssueRequest{Title: "Full", ID: "oc-full"})
	sp := src.(*project)
	cols, err := mapRows(ctx, sp.r, `SELECT name, type FROM pragma_table_info('issues')`)
	if err != nil {
		t.Fatal(err)
	}
	special := map[string]any{"status": "deferred", "issue_type": "feature", "metadata": `{"k":1}`, "extra": `{"x":"y"}`,
		"versions": "{}", "home_area": "solver", "area": "solver", "is_blocked": 0, "pinned": 1, "is_template": 1, "ephemeral": 1, "priority": 1}
	skip := map[string]bool{"id": true, "number": true, "parent": true, "version": true}
	var sets []string
	var args []any
	for i, c := range cols {
		name := str(c["name"])
		if skip[name] {
			continue
		}
		v, ok := special[name]
		if !ok {
			if str(c["type"]) == "INTEGER" {
				v = int64(1700000000000 + i)
			} else {
				v = "v-" + name
			}
		}
		sets = append(sets, name+" = ?")
		args = append(args, v)
	}
	if _, err := sp.w.ExecContext(ctx, `UPDATE issues SET `+strings.Join(sets, ", ")+` WHERE id = ?`, append(args, a.ID)...); err != nil {
		t.Fatal(err)
	}
	if _, err := sp.w.ExecContext(ctx, `UPDATE history SET source_id = 'beads-ev-1' WHERE issue_id = ?`, a.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := mapRows(ctx, sp.r, `SELECT * FROM issues WHERE id = ?`, a.ID)
	moveOne(t, src, dst, a.ID, "sg-full", nil)
	after, _ := mapRows(ctx, dst.(*project).r, `SELECT * FROM issues WHERE id = 'sg-full'`)
	if len(before) != 1 || len(after) != 1 {
		t.Fatalf("rows %d %d", len(before), len(after))
	}
	for k, v := range before[0] {
		switch k {
		case "id", "number", "version", "versions", "area":
			continue
		}
		if fmt.Sprint(after[0][k]) != fmt.Sprint(v) {
			t.Errorf("%s: %v, want %v", k, after[0][k], v)
		}
	}
	hs, _ := mapRows(ctx, dst.(*project).r, `SELECT source_id FROM history WHERE issue_id = 'sg-full' AND source_id IS NOT NULL`)
	if len(hs) != 1 || str(hs[0]["source_id"]) != "beads-ev-1" {
		t.Fatalf("source ids %v", hs)
	}
}

func ptr[T any](v T) *T { return &v }

// MoveEdges names every surviving record of a stop, from either store: after
// a round trip the source's redirect is gone, but the middle's names it.
func TestMoveEdgesAfterARoundTrip(t *testing.T) {
	ctx := context.Background()
	src, dst := twoProjects(t)
	create(t, src, orchapi.NewIssueRequest{Title: "Wanderer", ID: "oc-edg"})
	c := apply(t, src, istore.Mutation{Op: istore.OpComment, ID: "oc-edg", Comment: &orchapi.IssueCommentRequest{Text: "kept"}}).Comment
	moveVia(t, src, "01SRC", dst, "01DST", "oc-edg", "sg-edg", nil)
	moveVia(t, dst, "01DST", src, "01SRC", "sg-edg", "oc-edg", nil)
	want := istore.MoveEdge{FromProject: "01DST", FromID: "sg-edg", ToProject: "01SRC", ToID: "oc-edg"}
	for _, p := range []istore.ProjectStore{src, dst} {
		es, err := p.(istore.Mover).MoveEdges(ctx, "01SRC", "oc-edg")
		if err != nil || len(es) != 1 {
			t.Fatalf("edges %+v %v", es, err)
		}
		if es[0].At.IsZero() {
			t.Fatal("an edge without its time")
		}
		es[0].At = time.Time{}
		if es[0] != want {
			t.Fatalf("edge %+v, want %+v", es[0], want)
		}
	}
	// A redaction without an issue id finds the comment's issue; an unknown
	// comment answers not found.
	r := apply(t, src, istore.Mutation{Op: istore.OpCommentRedact, CommentID: c.ID, Moderate: func(string) bool { return true }})
	if r.Issue.ID != "oc-edg" || len(r.Redacted) != 1 || r.Redacted[0] != "kept" {
		t.Fatalf("redact by comment id: %+v", r)
	}
	if _, err := src.Apply(ctx, istore.Mutation{Op: istore.OpCommentRedact, CommentID: "missing", Actor: ana, Moderate: func(string) bool { return true }}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("unknown comment: %v", err)
	}
}
