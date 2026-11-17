package approvals

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"image/png"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

type fakeNet struct{ self core.NetSelf }

func (fakeNet) Dial(context.Context, string, string) (net.Conn, error) { return nil, net.ErrClosed }
func (fakeNet) Peer(context.Context, string) (core.Remote, error)      { return core.Remote{}, nil }
func (f fakeNet) Self(context.Context) (core.NetSelf, error)           { return f.self, nil }
func (fakeNet) Up(context.Context, string, time.Duration) (api.TailnetStatus, error) {
	return api.TailnetStatus{}, nil
}

// Plain HTTP goes to the node's HTTPS name once it serves one (an iPhone
// installs a page only from a secure origin); never before, never from
// HTTPS itself, and a pairing code survives the hop only in its own alphabet.
func TestHTTPSRedirect(t *testing.T) {
	serving := core.NetSelf{Running: true, DNSName: "zm-mini.example.ts.net", HTTPS: true}
	for _, c := range []struct {
		name string
		self core.NetSelf
		tls  bool
		url  string
		want string
	}{
		{"https served", serving, false, "/v0/approvals", "https://zm-mini.example.ts.net/v0/approvals"},
		{"code kept", serving, false, "/v0/approvals?code=k7qm-3xrt", "https://zm-mini.example.ts.net/v0/approvals?code=K7QM3XRT"},
		{"bad code dropped", serving, false, "/v0/approvals?code=%3Cx%3E", "https://zm-mini.example.ts.net/v0/approvals"},
		{"already https", serving, true, "/v0/approvals", ""},
		{"no certificate", core.NetSelf{Running: true, DNSName: "zm-mini.example.ts.net"}, false, "/v0/approvals", ""},
		{"no name", core.NetSelf{Running: true, HTTPS: true}, false, "/v0/approvals", ""},
	} {
		s := &Service{d: &core.Deps{Net: fakeNet{c.self}}}
		r := httptest.NewRequest("GET", c.url, nil)
		if c.tls {
			r.TLS = &tls.ConnectionState{}
		}
		if got := s.httpsURL(r); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	if got := (&Service{d: &core.Deps{}}).httpsURL(httptest.NewRequest("GET", "/v0/approvals", nil)); got != "" {
		t.Errorf("no tailnet: %q", got)
	}
}

// zm-decisions.js is t3code's zm-client decisions.ts with only its types
// stripped (scripts/sync-zm-client.sh): a hand edit breaks the recorded hash.
func TestVendoredZmClientIntact(t *testing.T) {
	b, err := webFiles.ReadFile("web/zm-decisions.js")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitN(string(b), "\n", 4)
	if len(lines) < 4 || !strings.HasPrefix(lines[1], "// source: t3code packages/zm-client/src/decisions.ts @ ") ||
		!strings.HasPrefix(lines[2], "// body-sha256: ") {
		t.Fatalf("header: %q", lines[:3])
	}
	sum := sha256.Sum256([]byte(lines[3]))
	if want := strings.TrimPrefix(lines[2], "// body-sha256: "); hex.EncodeToString(sum[:]) != want {
		t.Fatalf("zm-decisions.js was edited by hand (sha256 %x, header %s): change decisions.ts in t3code and run scripts/sync-zm-client.sh", sum, want)
	}
	app, _ := webFiles.ReadFile("web/app.js")
	for _, want := range []string{`from "./zm-decisions.js"`, "groupAsks", "workAction"} {
		if !bytes.Contains(app, []byte(want)) {
			t.Errorf("app.js lacks %q", want)
		}
	}
	if bytes.Contains(app, []byte("innerHTML")) || bytes.Contains(app, []byte("insertAdjacentHTML")) {
		t.Error("app.js sets HTML from data")
	}
}

func TestIconIsAPNG(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(iconPNG()))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 512 || b.Dy() != 512 {
		t.Fatalf("icon %v", b)
	}
}

// The phone's Decisions page refreshes every 30 s by rebuilding its cards. A
// refresh must never take what the person is typing, its focus and caret, a
// ticked box or an open fold; a refused answer keeps the words. A
// draft follows its group when the first ask is answered elsewhere; focus with
// no input for a minute stops holding refreshes off; drafts and folds that left
// the view are forgotten.
// testdata/refresh.mjs runs the embedded app.js against a small fake DOM.
func TestDecisionsRefreshKeepsWhatYouType(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node here")
	}
	dir := t.TempDir()
	harness, err := os.ReadFile("testdata/refresh.mjs")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"package.json": []byte(`{"type":"module"}`), "refresh.mjs": harness}
	for _, f := range []string{"app.js", "zm-decisions.js"} {
		if files[f], err = webFiles.ReadFile("web/" + f); err != nil {
			t.Fatal(err)
		}
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, filepath.Join(dir, "refresh.mjs"), filepath.Join(dir, "app.js")).CombinedOutput()
	if err != nil || !bytes.HasSuffix(out, []byte("ok\n")) {
		t.Fatalf("%v\n%s", err, out)
	}
}
