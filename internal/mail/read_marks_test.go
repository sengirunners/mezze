package mail

import (
	"bytes"
	"context"
	"database/sql"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/store"
)

func getMarks(c *client.Client, project string) ([]orchapi.ReadMark, error) {
	var l orchapi.ReadMarkList
	path := "/v0/mail/read-marks"
	if project != "" {
		path += "?project=" + project
	}
	err := c.Do(ctx, http.MethodGet, path, nil, &l)
	return l.Marks, err
}

func putMarks(c *client.Client, project string, ms ...orchapi.ReadMarkSet) ([]orchapi.ReadMark, error) {
	var l orchapi.ReadMarkList
	err := c.Do(ctx, http.MethodPut, "/v0/mail/read-marks?project="+project, orchapi.ReadMarksRequest{Marks: ms}, &l)
	return l.Marks, err
}

func mark(project, conv string, upto int64) orchapi.ReadMark {
	return orchapi.ReadMark{Project: project, Conv: conv, UptoMs: upto}
}

// SPEC 9 "Read marks": one high-water mark per person per conversation;
// an older write never lowers it.
func TestReadMarksMaxMerge(t *testing.T) {
	f := newFixture(t)
	ana := f.as("", "")
	now := store.Millis(f.clock)
	got := must(putMarks(ana, "side", orchapi.ReadMarkSet{Conv: "human:bea", UptoMs: now - 100}, orchapi.ReadMarkSet{Conv: "side", UptoMs: now - 50}))
	if want := []orchapi.ReadMark{mark("side", "human:bea", now-100), mark("side", "side", now-50)}; !slices.Equal(got, want) {
		t.Fatalf("put answered %+v, want %+v", got, want)
	}
	// An older mark (another device, behind) leaves it; a newer one moves it.
	got = must(putMarks(ana, "side", orchapi.ReadMarkSet{Conv: "human:bea", UptoMs: now - 500}))
	if !slices.Contains(got, mark("side", "human:bea", now-100)) {
		t.Fatalf("an older write lowered the mark: %+v", got)
	}
	must(putMarks(ana, "side", orchapi.ReadMarkSet{Conv: "human:bea", UptoMs: now - 10}))
	must(putMarks(ana, "harbor", orchapi.ReadMarkSet{Conv: "harbor", UptoMs: now - 1}))
	all := must(getMarks(ana, ""))
	want := []orchapi.ReadMark{mark("side", "human:bea", now-10), mark("side", "side", now-50), mark("harbor", "harbor", now-1)}
	slices.SortFunc(all, func(a, b orchapi.ReadMark) int { return strings.Compare(a.Project+a.Conv, b.Project+b.Conv) })
	slices.SortFunc(want, func(a, b orchapi.ReadMark) int { return strings.Compare(a.Project+a.Conv, b.Project+b.Conv) })
	if !slices.Equal(all, want) {
		t.Fatalf("all marks %+v, want %+v", all, want)
	}
	if one := must(getMarks(ana, "harbor")); !slices.Equal(one, []orchapi.ReadMark{mark("harbor", "harbor", now-1)}) {
		t.Fatalf("?project=harbor %+v", one)
	}
	// A mark past now would hide mail not yet sent: it stops at now.
	if got := must(putMarks(ana, "harbor", orchapi.ReadMarkSet{Conv: "future", UptoMs: now + int64(time.Hour/time.Millisecond)})); !slices.Contains(got, mark("harbor", "future", now)) {
		t.Fatalf("a future mark was kept: %+v", got)
	}
}

// The person is the caller, never the request: two people in one project
// never see each other's marks, and a session keeps none.
func TestReadMarksArePersonal(t *testing.T) {
	f := newFixture(t)
	ana, bea := f.as("", ""), f.member("bea")
	now := store.Millis(f.clock)
	must(putMarks(ana, "side", orchapi.ReadMarkSet{Conv: "side", UptoMs: now - 100}))
	if got := must(getMarks(bea, "")); len(got) != 0 {
		t.Fatalf("bea sees ana's marks: %+v", got)
	}
	got := must(putMarks(bea, "side", orchapi.ReadMarkSet{Conv: "side", UptoMs: now - 900}))
	if !slices.Equal(got, []orchapi.ReadMark{mark("side", "side", now-900)}) {
		t.Fatalf("bea's put answered %+v", got)
	}
	if got := must(getMarks(ana, "side")); !slices.Equal(got, []orchapi.ReadMark{mark("side", "side", now-100)}) {
		t.Fatalf("bea's mark moved ana's: %+v", got)
	}

	// A project export (a disposable copy for one person) keeps only that person's marks.
	dst := filepath.Join(t.TempDir(), "export.db")
	if err := f.d.Store.CopyProject(ctx, f.side.ID, dst, func(ctx context.Context, db *sql.DB) error { return store.FilterExport(ctx, db, "human:bea") }); err != nil {
		t.Fatal(err)
	}
	if b := must(os.ReadFile(dst)); !bytes.Contains(b, []byte("human:bea")) || bytes.Contains(b, []byte("human:ana")) {
		t.Fatal("bea's export kept ana's read marks, or lost bea's")
	}

	f.sess.add("mini/aaaaaaaa", f.side.ID, "", true, orchapi.StateRunning)
	sess := f.as("mini/aaaaaaaa", "side")
	if got, err := getMarks(sess, ""); err != nil || len(got) != 0 {
		t.Fatalf("a session's marks: %+v %v", got, err)
	}
	if got, err := getMarks(sess, "side"); err != nil || len(got) != 0 {
		t.Fatalf("a session's marks in side: %+v %v", got, err)
	}
	_, err := putMarks(sess, "side", orchapi.ReadMarkSet{Conv: "side", UptoMs: now})
	wantCode(t, err, api.CodeForbidden)
}

// A hidden project answers exactly like a missing one, on both routes; a
// PUT names its project in the query.
func TestReadMarksHiddenProject(t *testing.T) {
	f := newFixture(t)
	bea := f.member("bea") // a member of side only
	set := orchapi.ReadMarkSet{Conv: "x", UptoMs: 1}
	_, hid := getMarks(bea, "harbor")
	_, mis := getMarks(bea, "nope")
	wantCode(t, hid, api.CodeNotFound)
	wantCode(t, mis, api.CodeNotFound)
	if strings.ReplaceAll(hid.Error(), "harbor", "P") != strings.ReplaceAll(mis.Error(), "nope", "P") {
		t.Fatalf("GET hidden %q != missing %q", hid, mis)
	}
	_, hid = putMarks(bea, "harbor", set)
	_, mis = putMarks(bea, "nope", set)
	wantCode(t, hid, api.CodeNotFound)
	wantCode(t, mis, api.CodeNotFound)
	if strings.ReplaceAll(hid.Error(), "harbor", "P") != strings.ReplaceAll(mis.Error(), "nope", "P") {
		t.Fatalf("PUT hidden %q != missing %q", hid, mis)
	}
	var l orchapi.ReadMarkList
	err := bea.Do(ctx, http.MethodPut, "/v0/mail/read-marks", orchapi.ReadMarksRequest{Marks: []orchapi.ReadMarkSet{set}}, &l)
	wantCode(t, err, api.CodeBadRequest)
}

// Limits: conv 1 to 200 bytes, 1 to 100 marks per PUT; a refused PUT
// stores none of its marks.
func TestReadMarksLimits(t *testing.T) {
	f := newFixture(t)
	ana := f.as("", "")
	ok := orchapi.ReadMarkSet{Conv: "ok", UptoMs: 1}
	for name, ms := range map[string][]orchapi.ReadMarkSet{
		"none":       nil,
		"empty conv": {ok, {Conv: "", UptoMs: 1}},
		"201 bytes":  {ok, {Conv: strings.Repeat("x", orchapi.MaxReadMarkConv+1), UptoMs: 1}},
		"negative":   {ok, {Conv: "y", UptoMs: -1}},
		"101 marks":  slices.Repeat([]orchapi.ReadMarkSet{ok}, orchapi.MaxReadMarksPut+1),
	} {
		_, err := putMarks(ana, "side", ms...)
		if !api.IsCode(err, api.CodeBadRequest) {
			t.Errorf("%s: %v, want bad_request", name, err)
		}
	}
	if got := must(getMarks(ana, "")); len(got) != 0 {
		t.Fatalf("a refused put stored %+v", got)
	}
	long := strings.Repeat("x", orchapi.MaxReadMarkConv)
	if got := must(putMarks(ana, "side", slices.Repeat([]orchapi.ReadMarkSet{{Conv: long, UptoMs: 2}}, orchapi.MaxReadMarksPut)...)); !slices.Equal(got, []orchapi.ReadMark{mark("side", long, 2)}) {
		t.Fatalf("100 marks of 200 bytes: %+v", got)
	}
}
