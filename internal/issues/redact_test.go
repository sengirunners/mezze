package issues_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/homes/bkcrypt"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// A redaction makes a comment's text stop existing: not in the issue store,
// its history, the work module's copy, the event log, nor any byte of any
// file under the daemon's home (databases and their write-ahead logs).
func TestRedactLeavesNoCopy(t *testing.T) {
	ctx := context.Background()
	f := newLeaseFixture(t, true)
	id := f.create("a leaked secret")
	const first, second = "AKIA-FIRST-SECRET-0001", "AKIA-SECOND-SECRET-0002"
	// Through zm work: the native store, a work comment copy, a work event.
	if _, err := orchclient.Comment(ctx, f.owner, "issue:"+id, orchapi.CommentRequest{Body: "key is " + first}); err != nil {
		t.Fatal(err)
	}
	cs, err := orchclient.IssueComments(ctx, f.owner, id)
	if err != nil || len(cs) != 1 {
		t.Fatalf("native comments: %+v %v", cs, err)
	}
	cid := cs[0].ID
	// An edit: history and an edit event carry both texts.
	if _, err := orchclient.EditComment(ctx, f.owner, id, cid, orchapi.EditCommentRequest{Text: "rotated to " + second}); err != nil {
		t.Fatal(err)
	}
	home := f.dm.Deps.Config.Home
	if !anyFileHas(t, home, first) || !anyFileHas(t, home, second) {
		t.Fatal("the secrets are not on disk before the redaction: the test proves nothing")
	}
	req := httptest.NewRequest("POST", "/v0/issues/"+id+"/comments/"+cid+"/redact", strings.NewReader("{}"))
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	req.Header.Set(api.HeaderProject, "harbor")
	rec := httptest.NewRecorder()
	f.dm.SocketHandler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("redact: %d %s", rec.Code, rec.Body.String())
	}
	cs, _ = orchclient.IssueComments(ctx, f.owner, id)
	if len(cs) != 1 || cs[0].Text != "[redacted]" || !cs[0].Deleted {
		t.Fatalf("after redaction: %+v", cs)
	}
	for _, s := range []string{first, second} {
		if p := fileWith(t, home, s); p != "" {
			t.Errorf("%s is still in %s", s, p)
		}
	}
	// Idempotent: again answers 200.
	if code, body := redactAs(f, id, cid); code != 200 {
		t.Fatalf("redact again: %d %s", code, body)
	}
}

func redactAs(f *leaseFixture, id, cid string) (int, string) {
	req := httptest.NewRequest("POST", "/v0/issues/"+id+"/comments/"+cid+"/redact", strings.NewReader("{}"))
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	req.Header.Set(api.HeaderProject, "harbor")
	rec := httptest.NewRecorder()
	f.dm.SocketHandler().ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// JSON-escaped copies: a text with quotes, <>& and a newline is found in its
// escaped form too.
func TestRedactEscapedText(t *testing.T) {
	ctx := context.Background()
	f := newLeaseFixture(t, true)
	id := f.create("escapes")
	secret := "pw=\"<s3cr&t>\"\nline two TOKEN-77731"
	if _, err := orchclient.Comment(ctx, f.owner, "issue:"+id, orchapi.CommentRequest{Body: secret}); err != nil {
		t.Fatal(err)
	}
	cs, _ := orchclient.IssueComments(ctx, f.owner, id)
	if code, body := redactAs(f, id, cs[0].ID); code != 200 {
		t.Fatalf("redact: %d %s", code, body)
	}
	if p := fileWith(t, f.dm.Deps.Config.Home, "TOKEN-77731"); p != "" {
		t.Fatalf("an escaped copy survived in %s", p)
	}
}

// A short text is redacted where it is the comment, and nowhere else: the
// issue's other comments, its events' keys and types stay as they were.
func TestRedactShortTextIsExact(t *testing.T) {
	ctx := context.Background()
	f := newLeaseFixture(t, true)
	id := f.create("short")
	if _, err := orchclient.Comment(ctx, f.owner, "issue:"+id, orchapi.CommentRequest{Body: "fix this soon"}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Comment(ctx, f.owner, "issue:"+id, orchapi.CommentRequest{Body: "fix"}); err != nil {
		t.Fatal(err)
	}
	cs, _ := orchclient.IssueComments(ctx, f.owner, id)
	var short string
	for _, c := range cs {
		if c.Text == "fix" {
			short = c.ID
		}
	}
	if code, body := redactAs(f, id, short); code != 200 {
		t.Fatalf("redact: %d %s", code, body)
	}
	cs, _ = orchclient.IssueComments(ctx, f.owner, id)
	other := false
	for _, c := range cs {
		if c.Text == "fix this soon" {
			other = true
		}
	}
	if !other {
		t.Fatalf("the other comment was touched: %+v", cs)
	}
	n, kept := 0, false
	err := f.owner.Events(ctx, api.EventQuery{Since: "0", NoLive: true}, func(e api.Event) error {
		n++
		if strings.Contains(string(e.Data), `"body":"fix"`) || strings.Contains(string(e.Data), `"fix"]`) {
			t.Errorf("an event still carries the redacted short text: %s %s", e.Type, e.Data)
		}
		if strings.Contains(e.Type, "[redacted]") || !strings.HasPrefix(e.Type, "dev.zm.") || strings.Contains(e.Subject, "[redacted]") {
			t.Errorf("an event was mangled: %q %q", e.Type, e.Subject)
		}
		if strings.Contains(string(e.Data), "fix this soon") {
			kept = true
		}
		return nil
	})
	if err != nil || n == 0 || !kept {
		t.Fatalf("events: %v (%d read, other comment's event kept: %v)", err, n, kept)
	}
}

// Only moderators redact: a member, even the author, cannot.
func TestRedactNeedsAModerator(t *testing.T) {
	ctx := context.Background()
	f := newAuthzFixture(t)
	it, err := orchclient.CreateIssue(ctx, f.ownerAt("harbor/solver"), orchapi.NewIssueRequest{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	code, body := f.as("member", "POST", "/v0/issues/"+it.ID+"/comments", `{"text":"my own words here"}`)
	if code != 200 {
		t.Fatalf("member comment: %d %s", code, body)
	}
	cs, _ := orchclient.IssueComments(ctx, f.ownerAt("harbor"), it.ID)
	for _, who := range []string{"member", "guest"} {
		if code, body := f.as(who, "POST", "/v0/issues/"+it.ID+"/comments/"+cs[0].ID+"/redact", "{}"); code != 403 {
			t.Fatalf("%s redact: %d %s", who, code, body)
		}
	}
}

func anyFileHas(t *testing.T, dir, s string) bool { return fileWith(t, dir, s) != "" }

// backupHomes are the test daemons' homes, whose backup keys open the
// encrypted sqlite copies fileWith meets.
var backupHomes sync.Map

// fileWith is the first file under dir whose bytes contain s ("" = none).
// An encrypted backup is searched as it decrypts: one no test home's key
// opens fails the test, since its content would go unchecked.
func fileWith(t *testing.T, dir, s string) string {
	t.Helper()
	found := ""
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || found != "" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err == nil && bytes.HasPrefix(b, []byte("zmbk\x00")) {
			b = decrypted(t, p)
		}
		if err == nil && bytes.Contains(b, []byte(s)) {
			found = p
		}
		return nil
	})
	return found
}

// decrypted is the encrypted backup at p, opened with a test home's key.
func decrypted(t *testing.T, p string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "plain")
	var got []byte
	backupHomes.Range(func(h, _ any) bool {
		k, err := bkcrypt.Open(h.(string))
		if err != nil {
			return true
		}
		if _, _, err := bkcrypt.DecryptFile(context.Background(), p, out, k); err != nil {
			if !errors.As(err, new(*bkcrypt.WrongKeyError)) {
				t.Fatalf("%s: %v", p, err)
			}
			return true
		}
		got, _ = os.ReadFile(out)
		return false
	})
	if got == nil {
		t.Fatalf("no test home's key opens the encrypted backup %s", p)
	}
	return got
}

// Part 2: an export backup written before the redaction holds the work
// comment copy; the redaction rewrites it (SPEC 17.9 backups keep up to
// Keep snapshots, so an old one would otherwise keep the text for days).
func TestRedactScrubsExportBackups(t *testing.T) {
	ctx := context.Background()
	f := newLeaseFixture(t, true)
	id := f.create("a leaked secret in a backup")
	id2 := f.create("an untouched issue")
	if _, err := orchclient.Comment(ctx, f.owner, "issue:"+id2, orchapi.CommentRequest{Body: "a comment on another issue, 1e21 and 10000000000000001"}); err != nil {
		t.Fatal(err)
	}
	const secret = "AKIA-BACKUP-SECRET-0003"
	const other = "an unrelated comment that stays"
	if _, err := orchclient.Comment(ctx, f.owner, "issue:"+id, orchapi.CommentRequest{Body: "key is " + secret}); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.Comment(ctx, f.owner, "issue:"+id, orchapi.CommentRequest{Body: other}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var res orchapi.BackupResult
	var err error
	for i := 0; i < 50; i++ { // the native mirror's first sync
		if res, err = orchclient.AddTrackerBackup(ctx, f.owner, "issue", orchapi.AddBackupRequest{Target: "file://" + dir, Kind: orchapi.BackupExport, RunNow: i == 0}); err == nil {
			break
		}
		if api.IsCode(err, api.CodeConflict) || strings.Contains(err.Error(), "already") {
			_, err = orchclient.RunTrackerBackups(ctx, f.owner, "issue", "", "")
			if err == nil {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("backup: %+v %v", res, err)
	}
	if !anyFileHas(t, dir, secret) || !anyFileHas(t, dir, other) {
		t.Fatal("the backup does not hold the comments: the test proves nothing")
	}
	snap := fileWith(t, dir, secret)
	lineOf := func(issue string) string {
		b, err := os.ReadFile(snap)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range strings.Split(string(b), "\n") {
			if strings.Contains(l, `"id":"issue:`+issue+`"`) {
				return l
			}
		}
		t.Fatalf("no line for %s in %s", issue, snap)
		return ""
	}
	before := lineOf(id2)
	cs, err := orchclient.IssueComments(ctx, f.owner, id)
	if err != nil || len(cs) != 2 {
		t.Fatalf("native comments: %+v %v", cs, err)
	}
	cid := cs[0].ID
	if !strings.Contains(cs[0].Text, secret) {
		cid = cs[1].ID
	}
	if code, body := redactAs(f, id, cid); code != 200 {
		t.Fatalf("redact: %d %s", code, body)
	}
	if p := fileWith(t, dir, secret); p != "" {
		t.Errorf("the secret is still in backup %s", p)
	}
	if !anyFileHas(t, dir, other) {
		t.Error("the redaction removed an unrelated comment from the backup")
	}
	if !anyFileHas(t, dir, "[redacted]") {
		t.Error("the backup has no redacted copy")
	}
	if after := lineOf(id2); after != before {
		t.Errorf("another issue's line changed:\n%s\n%s", before, after)
	}
}

// The ack-gap route follows redact's scope: a member or guest
// is refused, a hidden issue answers like a missing one, and an owner with
// no such gap gets not_found.
func TestAckGapRouteScope(t *testing.T) {
	ctx := context.Background()
	f := newAuthzFixture(t)
	vis, err := orchclient.CreateIssue(ctx, f.ownerAt("harbor/solver"), orchapi.NewIssueRequest{Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	hid, err := orchclient.CreateIssue(ctx, f.ownerAt("harbor"), orchapi.NewIssueRequest{Title: "y", Labels: []string{"billing"}})
	if err != nil {
		t.Fatal(err)
	}
	cm, err := orchclient.CommentIssue(ctx, f.ownerAt("harbor"), vis.ID, orchapi.IssueCommentRequest{Text: "words here"})
	if err != nil {
		t.Fatal(err)
	}
	hc, err := orchclient.CommentIssue(ctx, f.ownerAt("harbor"), hid.ID, orchapi.IssueCommentRequest{Text: "billing words"})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"stop":"harbor:` + vis.ID + `"}`
	for _, who := range []string{"member", "guest"} {
		if code, b := f.as(who, "POST", "/v0/issues/"+vis.ID+"/comments/"+cm.ID+"/ack-gap", body); code != 403 {
			t.Fatalf("%s ack-gap: %d %s", who, code, b)
		}
		hc1, hb := f.as(who, "POST", "/v0/issues/"+hid.ID+"/comments/"+hc.ID+"/ack-gap", body)
		mc, mb := f.as(who, "POST", "/v0/issues/issue-nope/comments/"+hc.ID+"/ack-gap", body)
		// Hidden answers exactly like missing, byte for byte but the id asked.
		if hc1 != 404 || mc != 404 || strings.Contains(hb, "billing") || strings.ReplaceAll(hb, hid.ID, "X") != strings.ReplaceAll(mb, "issue-nope", "X") {
			t.Fatalf("%s hidden %d %s, missing %d %s", who, hc1, hb, mc, mb)
		}
	}
	if _, err := orchclient.AckRedactionGap(ctx, f.ownerAt("harbor"), vis.ID, cm.ID, "harbor:"+vis.ID); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("owner, no gap: %v", err)
	}
}
