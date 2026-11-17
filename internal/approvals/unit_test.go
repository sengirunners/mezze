package approvals

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

func TestCheckName(t *testing.T) {
	for _, ok := range []string{"ivo", "bea", "ana-2", "j.c"} {
		if _, err := checkName(ok); err != nil {
			t.Errorf("%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "Ivo", "ivo ", " ivo", "i vo", "i​vo", "ｉｖｏ",
		"ivо", "ivο", "ívo", "-ivo", "none", "owners", "someone", strings.Repeat("a", 64)} {
		if _, err := checkName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSkeleton(t *testing.T) {
	same := [][2]string{
		{"ivo", "iv0"}, {"ivo", "IVO"}, {"ivo", "ivо"}, {"ivo", "ｉｖｏ"},
		{"bea", "bеa"}, {"bea", "beа"}, {"mallory", "rnallory"}, {"william", "wi11iam"}, {"vvalt", "walt"},
		{"i-vo", "ivo"}, {"i_vo", "i.vo"}, {"ivο", "ivo"},
	}
	for _, p := range same {
		if skeleton(p[0]) != skeleton(p[1]) {
			t.Errorf("%q and %q should look alike: %q vs %q", p[0], p[1], skeleton(p[0]), skeleton(p[1]))
		}
	}
	for _, p := range [][2]string{{"ivo", "iv"}, {"ivo", "ivo2"}, {"bea", "beaton"}, {"ana", "anna"}} {
		if skeleton(p[0]) == skeleton(p[1]) {
			t.Errorf("%q and %q are different names", p[0], p[1])
		}
	}
}

func TestCanonRepo(t *testing.T) {
	for in, want := range map[string]string{
		"example-org/harbor":                        "github.com/example-org/harbor",
		"github.com/example-org/harbor":             "github.com/example-org/harbor",
		"https://github.com/example-org/harbor.git": "github.com/example-org/harbor",
		"git@github.com:example-org/harbor.git":     "github.com/example-org/harbor",
		"https://github.example.com/a/b/":           "github.example.com/a/b",
	} {
		if got, err := CanonRepo(in); err != nil || got != want {
			t.Errorf("CanonRepo(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "harbor", "a/b/c/d", "a b/c", "../x/y", "github.com/a"} {
		if got, err := CanonRepo(bad); err == nil {
			t.Errorf("CanonRepo(%q) = %q, want an error", bad, got)
		}
	}
}

func TestKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, KeyFile)
	k1, err := loadOrCreateKey(path)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode: %v %v", fi.Mode(), err)
	}
	k2, err := loadOrCreateKey(path)
	if err != nil || !k1.Equal(k2) {
		t.Fatalf("key not stable across loads: %v", err)
	}
	if err := os.WriteFile(path, []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadOrCreateKey(path); err == nil {
		t.Fatal("a corrupt key file was accepted (must fail closed, never regenerate silently)")
	}
	pk := PublicKey(k1.Public().(ed25519.PublicKey))
	for _, s := range []string{pk.Line, pk.PublicKey, mustJSON(pk)} {
		if p, err := ParsePublicKey(s); err != nil || KeyID(p) != pk.KeyID {
			t.Errorf("ParsePublicKey(%q): %v", s, err)
		}
	}
	if _, err := ParsePublicKey("ed25519:AAAA"); err == nil {
		t.Error("a short key parsed")
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

type ghCall struct{ path, auth, body string }

func fakeGitHub(t *testing.T) (*httptest.Server, *[]ghCall) {
	var mu sync.Mutex
	var calls []ghCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		calls = append(calls, ghCall{r.URL.Path, r.Header.Get("Authorization"), string(b)})
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestGitHubBot(t *testing.T) {
	srv, calls := fakeGitHub(t)
	dir := t.TempDir()
	bot := &GitHubBot{Dir: dir, BaseURL: srv.URL}
	ask := orchapi.Ask{ID: "01JASK", Project: "01JPROJ", Subject: orchapi.AskSubject{Repo: "github.com/example-org/harbor", PR: 7,
		HeadSHA: "0123456789abcdef0123456789abcdef01234567"}}
	d := orchapi.Decision{Ask: ask.ID, Decision: orchapi.DecisionApprove, Approver: "ivo", Receipt: orchapi.Receipt{V: orchapi.ReceiptVersion, Sig: "SIG"}}
	ctx := context.Background()

	if err := bot.Publish(ctx, ask, d); !errors.Is(err, ErrNoCredential) || len(*calls) != 0 {
		t.Fatalf("no token: %v, %d calls", err, len(*calls))
	}
	tok := filepath.Join(dir, "01JPROJ.token")
	if err := os.WriteFile(tok, []byte("ghs_bot\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(tok, 0o644); err != nil { // a umask of 077 (a CI runner's) made it 0600
		t.Fatal(err)
	}
	if err := bot.Publish(ctx, ask, d); err == nil || len(*calls) != 0 {
		t.Fatalf("world-readable token used: %v", err)
	}
	if err := os.Chmod(tok, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := bot.Publish(ctx, ask, d); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 {
		t.Fatalf("calls: %+v", *calls)
	}
	st, cm := (*calls)[0], (*calls)[1]
	if st.path != "/repos/example-org/harbor/statuses/"+ask.Subject.HeadSHA || st.auth != "Bearer ghs_bot" ||
		!strings.Contains(st.body, `"state":"success"`) || !strings.Contains(st.body, `"context":"zm/approval"`) {
		t.Fatalf("status call: %+v", st)
	}
	if cm.path != "/repos/example-org/harbor/issues/7/comments" || !strings.Contains(cm.body, "SIG") {
		t.Fatalf("comment call: %+v", cm)
	}
	// Another host is never posted to with this bot.
	ask.Subject.Repo = "gitlab.com/a/b"
	if err := bot.Publish(ctx, ask, d); err != nil || len(*calls) != 2 {
		t.Fatalf("non-GitHub repo: %v, %d calls", err, len(*calls))
	}
}

// TestSmokeSignReceipt stands in for an approver device in scripts/smoke.sh,
// which has no tailnet: it signs a receipt with a daemon's key file
// (ZM_SMOKE_SIGN) and writes it to ZM_SMOKE_OUT (decision
// ZM_SMOKE_DECISION, default approve). Skipped otherwise.
func TestSmokeSignReceipt(t *testing.T) {
	keyFile, out := os.Getenv("ZM_SMOKE_SIGN"), os.Getenv("ZM_SMOKE_OUT")
	if keyFile == "" || out == "" {
		t.Skip("smoke helper")
	}
	if _, err := os.Stat(keyFile); err != nil {
		t.Fatal(err) // never create a key here
	}
	key, err := loadOrCreateKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	subj, err := CanonSubject(orchapi.AskSubject{Repo: os.Getenv("ZM_SMOKE_REPO"), PR: 7, HeadSHA: os.Getenv("ZM_SMOKE_HEAD")})
	if err != nil {
		t.Fatal(err)
	}
	decision := os.Getenv("ZM_SMOKE_DECISION")
	if decision == "" {
		decision = orchapi.DecisionApprove
	}
	r := Sign(key, orchapi.Receipt{Project: os.Getenv("ZM_SMOKE_PROJECT"), Ask: os.Getenv("ZM_SMOKE_ASK"), Subject: subj,
		Decision: decision, Approver: "ana", Node: "nSMOKEPHONE", Time: "2026-10-30T18:00:00.000Z"})
	if err := os.WriteFile(out, []byte(mustJSON(r)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Common ASCII digit/letter lookalikes fold too (TR39-style).
func TestSkeletonDigits(t *testing.T) {
	for _, p := range [][2]string{{"class", "cla5s"}, {"jazz", "ja2z"}, {"bob", "8ob"}, {"bob", "6ob"}, {"greg", "9reg"},
		{"emma", "ennma"}} {
		if skeleton(p[0]) != skeleton(p[1]) {
			t.Errorf("%q and %q should look alike: %q vs %q", p[0], p[1], skeleton(p[0]), skeleton(p[1]))
		}
	}
}

// The machine-derived name is compared lower case, like the request's Host.
// The full MagicDNS name comes from the node; a name that merely starts with
// the node's (DNS rebinding) is not it.
func TestOwnHostUpperMachine(t *testing.T) {
	s := &Service{d: &core.Deps{Config: &config.Config{Machine: "Mini"}, Net: selfNet{dns: "zm-mini.tail1.ts.net."}}}
	for host, want := range map[string]bool{"zm-mini.tail1.ts.net:7433": true, "ZM-MINI:7433": true, "zm-other": false,
		"zm-mini.attacker.example": false, "zm-mini.tail1.ts.net.attacker.example:7433": false, "100.64.1.2:7433": true, "8.8.8.8": false} {
		if got := s.ownHost(context.Background(), host); got != want {
			t.Errorf("ownHost(%q) = %v, want %v", host, got, want)
		}
	}
}

// A daemon's node is never paired as an approver (its agents could answer),
// whether the node is named zm-<machine> or, after the rename, potluck-<machine>:
// the name alone refuses it, even when nothing else marked it a daemon.
func TestPairRefusesDaemonNodeNames(t *testing.T) {
	s := &Service{}
	for _, node := range []string{"zm-studio", "ZM-Studio", "potluck-studio", "Potluck-Studio"} {
		rm := &core.Remote{Node: node, NodeID: "n1", NodeKey: "nodekey:1", OS: "iOS", SameLogin: true}
		if err := phoneRefusal(rm); err == nil || !strings.Contains(err.Error(), "runs a zm daemon") {
			t.Errorf("one-tap %q: %v, want refused as a daemon", node, err)
		}
		if _, err := s.Pair(context.Background(), rm, "ABCD-EFGH"); err == nil || !strings.Contains(err.Error(), "runs a zm daemon") {
			t.Errorf("pair %q: %v, want refused as a daemon", node, err)
		}
	}
	for _, node := range []string{"iphone", "zmphone", "potluckphone", "my-zm-phone"} {
		if err := phoneRefusal(&core.Remote{Node: node, OS: "iOS", SameLogin: true}); err != nil {
			t.Errorf("one-tap %q refused: %v", node, err)
		}
	}
}

// Pairing codes use rejection sampling: a byte that would bias the
// alphabet is skipped, never folded with %.
func TestCodeNoModuloBias(t *testing.T) {
	src := []byte{255, 240, 0, 1, 2, 3, 4, 5, 6, 7}
	c, err := codeFrom(bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if want := codeAlphabet[0:4] + "-" + codeAlphabet[4:8]; c != want {
		t.Fatalf("code %q, want %q (bytes >= 240 must be skipped)", c, want)
	}
}

// selfNet is a core.NetService that only describes its node.
type selfNet struct{ dns string }

func (n selfNet) Dial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("no")
}
func (n selfNet) Peer(context.Context, string) (core.Remote, error) {
	return core.Remote{}, errors.New("no")
}
func (n selfNet) Self(context.Context) (core.NetSelf, error) {
	return core.NetSelf{Running: true, Hostname: "zm-mini", DNSName: n.dns}, nil
}
func (n selfNet) Up(context.Context, string, time.Duration) (api.TailnetStatus, error) {
	return api.TailnetStatus{}, nil
}

// resolvePerson: any name, alias or GitHub login of one person,
// any case, names that person; none or several is refused with the list.
func TestResolvePerson(t *testing.T) {
	people := []person{
		{ID: "github:1", Name: "bea", Names: []string{"bea", "cyd"}, Aliases: []string{"cj", "pair"}, AliasOf: map[string]string{"cj": "cyd", "pair": "bea"}, GitHub: "beagh", Shown: []string{"cyd"}},
		{ID: "01RAE", Name: "rae", Names: []string{"rae"}, Aliases: []string{"pair"}, AliasOf: map[string]string{"pair": "rae"}},
		{ID: "github:2", Name: "ivo", Names: []string{"ivo"}, GitHub: "cyd2"},
		{ID: "github:3", Name: "zed", Names: []string{"zed"}, GitHub: "cj"}, // a login that is another's alias
	}
	for _, c := range []struct{ in, id, keep, err string }{
		{in: "bea", id: "github:1", keep: "bea"},
		{in: " CYD ", id: "github:1", keep: "cyd"},
		{in: "human:cyd", id: "github:1", keep: "cyd"},
		{in: "CJ", id: "github:1", keep: "cyd"},    // the alias outranks zed's login; kept as its identity
		{in: "BeaGH", id: "github:1", keep: "cyd"}, // a login: the identity the project shows
		{in: "@beagh", id: "github:1", keep: "cyd"},
		{in: "cyd2", id: "github:2", keep: "ivo"},
		{in: "rae", id: "01RAE", keep: "rae"},
		{in: "pair", err: "more than one person"},
		{in: "ivo", id: "github:2", keep: "ivo"},
		{in: "ghost", err: "not a person who owns"},
		{in: "", err: "not a person who owns"},
	} {
		p, keep, err := resolvePerson(people, c.in)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%q: %v, want %q", c.in, err, c.err)
			}
			continue
		}
		if err != nil || p.ID != c.id || keep != c.keep {
			t.Errorf("%q: %q %q %v, want %q %q", c.in, p.ID, keep, err, c.id, c.keep)
		}
	}
}
