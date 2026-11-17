package issues_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand/v2"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/issues"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/store"
)

func pngOf(t *testing.T, w, h int, noise bool) []byte {
	t.Helper()
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	r := rand.New(rand.NewPCG(uint64(w), uint64(h)))
	for y := range h {
		for x := range w {
			c := color.NRGBA{uint8(x), uint8(y), 99, 255}
			if noise {
				c = color.NRGBA{uint8(r.Uint32()), uint8(r.Uint32()), uint8(r.Uint32()), 255}
			}
			m.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// call is f.as with the whole answer (headers too).
func (f *authzFixture) call(who, method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	req.Header.Set(api.HeaderAuthorization, "Bearer "+f.tok[who])
	req.Header.Set(api.HeaderProject, "harbor/solver")
	req = req.WithContext(core.WithRemote(req.Context(), core.Remote{Login: who + "@example.com"}))
	rec := httptest.NewRecorder()
	f.dm.Handler.ServeHTTP(rec, req)
	return rec
}

func (f *authzFixture) attachmentsFile(project string) string {
	return filepath.Join(f.dm.Deps.Config.Home, store.DirProjects, project, "attachments.db")
}

// Attachments: whoever may comment attaches; readers of the item read
// (metadata on the item, bytes nosniff and inline); a hidden item's answer
// equals a missing one's; only PNG, JPEG, WebP; at most 3 live; the
// attacher or an area admin removes one; history and the work item carry
// metadata only.
func TestAttachments(t *testing.T) {
	ctx := context.Background()
	f := newAuthzFixture(t)
	owner := f.ownerAt("harbor/solver")
	it, err := orchclient.CreateIssue(ctx, owner, orchapi.NewIssueRequest{Title: "decide the runner tier"})
	if err != nil {
		t.Fatal(err)
	}
	billing, err := orchclient.CreateIssue(ctx, f.ownerAt("harbor"), orchapi.NewIssueRequest{Title: "SECRET billing", Labels: []string{"billing"}})
	if err != nil {
		t.Fatal(err)
	}
	img := pngOf(t, 64, 48, false)
	a, err := orchclient.AttachIssueImage(ctx, owner, it.ID, img, "")
	if err != nil {
		t.Fatal(err)
	}
	if !ids.Valid(a.ID) || a.Type != "image/png" || a.Size == 0 || a.CreatedBy == "" ||
		a.Expires.Sub(a.Created) != orchapi.IssueAttachmentTTL || len(a.SHA256) != 64 {
		t.Fatalf("metadata: %+v", a)
	}
	// The contract's names, exactly.
	raw, _ := json.Marshal(a)
	var keys map[string]any
	_ = json.Unmarshal(raw, &keys)
	for _, k := range []string{"id", "type", "size", "sha256", "created", "created_by", "expires"} {
		if _, ok := keys[k]; !ok || len(keys) != 7 {
			t.Fatalf("metadata keys: %s", raw)
		}
	}
	d, err := orchclient.Issue(ctx, owner, it.ID)
	if err != nil || len(d.Attachments) != 1 || d.Attachments[0].ID != a.ID {
		t.Fatalf("item json: %+v %v", d.Attachments, err)
	}
	if l, err := orchclient.Issues(ctx, owner, orchapi.IssueQuery{Status: "all"}); err != nil || len(l.Issues) == 0 || l.Issues[0].Attachments != nil {
		t.Fatalf("lists carry no attachments: %v", err)
	}

	// Bytes: the item's readers, a guest too; served as the image, nosniff, inline.
	path := "/v0/issues/" + it.ID + "/attachments/" + a.ID
	for _, who := range []string{"guest", "member"} {
		rec := f.call(who, "GET", path, nil)
		h := rec.Header()
		sum := sha256.Sum256(rec.Body.Bytes())
		if rec.Code != 200 || h.Get("Content-Type") != "image/png" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" ||
			h.Get("Content-Security-Policy") != "default-src 'none'; sandbox" || !strings.HasPrefix(h.Get("Content-Disposition"), "inline") || hex.EncodeToString(sum[:]) != a.SHA256 {
			t.Fatalf("%s bytes: %d %v", who, rec.Code, h)
		}
		if rec := f.call(who, "GET", "/v0/issues/"+it.ID+"/attachments", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), a.ID) {
			t.Fatalf("%s list: %d %s", who, rec.Code, rec.Body)
		}
	}
	// A guest reads but may not attach; a member attaches.
	if rec := f.call("guest", "POST", "/v0/issues/"+it.ID+"/attachments", img); rec.Code != 403 {
		t.Fatalf("guest attach: %d %s", rec.Code, rec.Body)
	}
	rec := f.call("member", "POST", "/v0/issues/"+it.ID+"/attachments?expires=30d", img)
	if rec.Code != 200 {
		t.Fatalf("member attach: %d %s", rec.Code, rec.Body)
	}
	var mine orchapi.IssueAttachment
	_ = json.Unmarshal(rec.Body.Bytes(), &mine)
	if mine.Expires.Sub(mine.Created) != 30*24*time.Hour {
		t.Fatalf("expires: %+v", mine)
	}

	// Hidden answers like missing: the billing item, its list and its image.
	hidden, err := orchclient.AttachIssueImage(ctx, f.ownerAt("harbor"), billing.ID, img, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/v0/issues/" + billing.ID + "/attachments", "/v0/issues/" + billing.ID + "/attachments/" + hidden.ID,
		"/v0/issues/" + it.ID + "/attachments/" + hidden.ID} {
		missing := strings.Replace(p, billing.ID, it.ID+"zz", 1)
		got, want := f.call("member", "GET", p, nil), f.call("member", "GET", missing, nil)
		if got.Code != 404 || got.Code != want.Code || strings.Contains(got.Body.String(), "SECRET") {
			t.Fatalf("%s: %d %s (missing: %d)", p, got.Code, got.Body, want.Code)
		}
	}
	if rec := f.call("member", "POST", "/v0/issues/"+billing.ID+"/attachments", img); rec.Code != 404 {
		t.Fatalf("hidden attach: %d", rec.Code)
	}

	// Limits: by the bytes (no SVG), a sane expiry, at most 8 live.
	for name, body := range map[string][]byte{"svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), "text": []byte("hello")} {
		if _, err := orchclient.AttachIssueImage(ctx, owner, it.ID, body, ""); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for _, e := range []string{"91d", "0s", "-1h", "soon", "99999999999999d"} {
		if _, err := orchclient.AttachIssueImage(ctx, owner, it.ID, img, e); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("expires %s: %v", e, err)
		}
	}
	if _, err := orchclient.AttachIssueImage(ctx, owner, it.ID, pngOf(t, 800, 800, true), ""); !api.IsCode(err, api.CodeTooLarge) {
		t.Fatalf("over 1 MiB after re-encoding: %v", err)
	}
	third, err := orchclient.AttachIssueImage(ctx, owner, it.ID, img, "90d")
	if err != nil {
		t.Fatal(err)
	}

	// Removal: not someone else's (a member), the attacher's own, and an
	// area admin's (the machine owner on the socket) of anyone's.
	if rec := f.call("member", "DELETE", path, nil); rec.Code != 403 {
		t.Fatalf("member removes the owner's: %d", rec.Code)
	}
	if rec := f.call("guest", "DELETE", "/v0/issues/"+it.ID+"/attachments/"+mine.ID, nil); rec.Code != 403 {
		t.Fatalf("guest removes: %d", rec.Code)
	}
	if rec := f.call("member", "DELETE", "/v0/issues/"+it.ID+"/attachments/"+mine.ID, nil); rec.Code != 200 {
		t.Fatalf("member removes their own: %d %s", rec.Code, rec.Body)
	}
	if _, err := orchclient.RemoveIssueAttachment(ctx, owner, it.ID, third.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := orchclient.RemoveIssueAttachment(ctx, owner, it.ID, third.ID); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("removed twice: %v", err)
	}
	if rec := f.call("member", "GET", "/v0/issues/"+it.ID+"/attachments/"+mine.ID, nil); rec.Code != 404 {
		t.Fatalf("removed is gone: %d", rec.Code)
	}
	l, err := orchclient.IssueAttachments(ctx, owner, it.ID)
	if err != nil || len(l.Attachments) != 1 || l.Attachments[0].ID != a.ID {
		t.Fatalf("left: %+v %v", l, err)
	}

	// History and the native work item: metadata, never bytes.
	hs, err := orchclient.IssueHistory(ctx, owner, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, h := range hs {
		kinds[h.Kind]++
		if strings.Contains(string(h.Fields["attachment"]), "base64") || len(h.Fields["attachment"]) > 400 {
			t.Fatalf("history carries more than metadata: %s", h.Fields["attachment"])
		}
	}
	if kinds["attached"] != 3 || kinds["attachment_removed"] != 2 {
		t.Fatalf("history kinds: %v", kinds)
	}
	svc := orchcore.Issues(f.dm.Deps).(*issues.Service)
	var wi orchapi.WorkItem
	if err := svc.NativeCall(ctx, it.Project, orchapi.AdapterRequest{Verb: orchapi.VerbGet, ID: it.ID}, &wi); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(wi.Extra["attachments"])
	if !strings.Contains(string(b), a.ID) || !strings.Contains(string(b), `"created_by":""`) {
		t.Fatalf("work item extra: %s", b)
	}
}

// An HTML page is kept verbatim and served sandboxed (inline styles and data:
// images render, no scripts or fetches); SVG, video and HTML
// that is not UTF-8 are refused; an item keeps at most 8.
func TestAttachHTMLAndCap(t *testing.T) {
	ctx := context.Background()
	f := newAuthzFixture(t)
	owner := f.ownerAt("harbor/solver")
	it, err := orchclient.CreateIssue(ctx, owner, orchapi.NewIssueRequest{Title: "mockup"})
	if err != nil {
		t.Fatal(err)
	}
	page := []byte("\xef\xbb\xbf<!-- mock -->\n<!DOCTYPE html><html><body><script>1</script> caf\u00e9\r\n</body></html>")
	a, err := orchclient.AttachIssueImage(ctx, owner, it.ID, page, "")
	if err != nil || a.Type != "text/html" || a.Size != int64(len(page)) {
		t.Fatalf("html: %+v %v", a, err)
	}
	rec := f.call("member", "GET", "/v0/issues/"+it.ID+"/attachments/"+a.ID, nil)
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), page) || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" ||
		rec.Header().Get("Content-Security-Policy") != "default-src 'none'; style-src 'unsafe-inline'; img-src data:; font-src data:; sandbox" || !strings.HasSuffix(rec.Header().Get("Content-Disposition"), `.html"`) {
		t.Fatalf("html bytes: %d %v", rec.Code, rec.Header())
	}
	big := append([]byte("<html>"), bytes.Repeat([]byte("a"), 1<<20)...)
	for name, c := range map[string]struct {
		body []byte
		code string
	}{
		"svg":        {[]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), api.CodeBadRequest},
		"xml":        {[]byte(`<?xml version="1.0"?><html/>`), api.CodeBadRequest},
		"mp4":        {append([]byte("\x00\x00\x00\x18ftypmp42"), make([]byte, 64)...), api.CodeBadRequest},
		"not utf-8":  {[]byte("<html>\xff\xfe</html>"), api.CodeBadRequest},
		"over 1 MiB": {big, api.CodeTooLarge},
	} {
		if _, err := orchclient.AttachIssueImage(ctx, owner, it.ID, c.body, ""); !api.IsCode(err, c.code) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	for i := 1; i < orchapi.MaxIssueAttachments; i++ {
		if _, err := orchclient.AttachIssueImage(ctx, owner, it.ID, page, ""); err != nil {
			t.Fatalf("attachment %d: %v", i+1, err)
		}
	}
	if orchapi.MaxIssueAttachments != 8 {
		t.Fatalf("an item keeps 8, not %d", orchapi.MaxIssueAttachments)
	}
	if _, err := orchclient.AttachIssueImage(ctx, owner, it.ID, page, ""); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("a ninth: %v", err)
	}
}

// Expiry: an expired image is never served, and the sweep removes it for
// good (its bytes leave the file) and notes it on the item. An explicit
// removal is as final.
func TestAttachmentExpiryAndHardDelete(t *testing.T) {
	ctx := context.Background()
	f := newAuthzFixture(t)
	owner := f.ownerAt("harbor/solver")
	it, err := orchclient.CreateIssue(ctx, owner, orchapi.NewIssueRequest{Title: "brief"})
	if err != nil {
		t.Fatal(err)
	}
	img := pngOf(t, 300, 300, true) // incompressible: its bytes are findable in the file
	fileHas := func(probe []byte) bool {
		var all []byte
		for _, suf := range []string{"", "-wal"} {
			b, _ := os.ReadFile(f.attachmentsFile(it.Project) + suf)
			all = append(all, b...)
		}
		return bytes.Contains(all, probe)
	}
	bytesOf := func(aid string) []byte {
		resp, err := orchclient.IssueAttachment(ctx, owner, it.ID, aid)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b
	}

	// 2s: room to read it back on a loaded runner before it expires (50ms
	// raced the read); then wait, bounded, for the expiry.
	short, err := orchclient.AttachIssueImage(ctx, owner, it.ID, img, "2s")
	if err != nil {
		t.Fatal(err)
	}
	probe := bytesOf(short.ID)[2000:2064]
	if !fileHas(probe) {
		t.Fatal("fixture: the stored bytes are not in the file")
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		resp, err := orchclient.IssueAttachment(ctx, owner, it.ID, short.ID)
		if err == nil {
			resp.Body.Close()
		}
		if api.IsCode(err, api.CodeNotFound) {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("expired, before the sweep: %v", err)
		}
	}
	if l, _ := orchclient.IssueAttachments(ctx, owner, it.ID); len(l.Attachments) != 0 {
		t.Fatalf("expired listed: %+v", l)
	}
	orchcore.Issues(f.dm.Deps).(*issues.Service).SweepAttachments(ctx)
	if fileHas(probe) {
		t.Fatal("swept bytes are still in the file")
	}
	hs, _ := orchclient.IssueHistory(ctx, owner, it.ID)
	if last := hs[len(hs)-1]; last.Kind != "attachment_expired" || !strings.Contains(string(last.Fields["attachment"]), short.ID) {
		t.Fatalf("expiry not noted: %+v", last)
	}

	kept, err := orchclient.AttachIssueImage(ctx, owner, it.ID, img, "")
	if err != nil {
		t.Fatal(err)
	}
	probe = bytesOf(kept.ID)[2000:2064]
	if _, err := orchclient.RemoveIssueAttachment(ctx, owner, it.ID, kept.ID); err != nil {
		t.Fatal(err)
	}
	if fileHas(probe) {
		t.Fatal("removed bytes are still in the file")
	}
}
