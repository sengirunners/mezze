package fulltest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/core"
)

// A mapped tailnet login is an ambient credential (core/ambient.go): every
// page the person's browser opens sends it. These tests are the attacks:
// a cross-site write (a no-cors text/plain POST, a form) and a DNS-rebinding
// page reading answers. A login caller must name this node in Host, never be
// cross-site, and send Zm-Protocol on writes.

type loginReq struct {
	method, target, body string
	hdr                  map[string]string
}

// asLogin sends r as bea's browser on the tailnet: WhoIs says bea, no token.
func asLogin(f *leakFixture, r loginReq) *httptest.ResponseRecorder {
	return asLoginOf(f, "bea@example.com", r)
}

// guardText is in every refusal of the login guard (core/ambient.go).
const guardText = "a tailnet login works from zm clients"

func refusedByGuard(r *httptest.ResponseRecorder) bool {
	return r.Code == 401 && strings.Contains(r.Body.String(), guardText)
}

func asLoginOf(f *leakFixture, login string, r loginReq) *httptest.ResponseRecorder {
	req := httptest.NewRequest(r.method, r.target, strings.NewReader(r.body))
	req.Host = "zm-mini:7433"
	for k, v := range r.hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	// Bounded: a route the guard failed to stop (a live event stream) must
	// fail the test, not hang it.
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	defer cancel()
	req = req.WithContext(core.WithRemote(ctx, core.Remote{Login: login, Node: "bea-phone", NodeID: "nBEA", NodeKey: "nodekey:bea"}))
	rec := httptest.NewRecorder()
	f.dm.Handler.ServeHTTP(rec, req)
	return rec
}

func loginFixture(t *testing.T) *leakFixture {
	t.Helper()
	f := newLeakFixture(t)
	owner := client.WithHandler(f.dm.SocketHandler())
	ctx := context.Background()
	if _, err := owner.AddIdentity(ctx, api.NewIdentityRequest{Name: "bea", TailscaleLogin: "bea@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "member", Project: "harbor"}); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestTailnetLoginHostMustNameThisNode(t *testing.T) {
	f := loginFixture(t)
	// The node's own names and Tailscale addresses answer.
	for _, h := range []string{"zm-mini", "zm-mini:7433", "ZM-MINI:7433", "zm-mini.:7433", "100.101.1.2:7433", "[fd7a:115c:a1e0::1]:7433"} {
		r := asLogin(f, loginReq{method: "GET", target: "/v0/projects", hdr: map[string]string{"Host": h}})
		if r.Code != 200 || !strings.Contains(r.Body.String(), `"harbor"`) {
			t.Errorf("Host %q: %d %s", h, r.Code, clip(r.Body.String()))
		}
	}
	// A rebound name gets nothing, reads included; so do loopback and LAN
	// addresses (they never name this node on the tailnet listener).
	for _, h := range []string{"evil.example", "zm-mini.evil.example", "zm-mini.tail1.ts.net.evil.example:7433", "zm-minix:7433",
		"127.0.0.1:7433", "10.0.0.5:7433", "[::1]:7433", ""} {
		r := asLogin(f, loginReq{method: "GET", target: "/v0/projects", hdr: map[string]string{"Host": h}})
		if !refusedByGuard(r) || strings.Contains(r.Body.String(), "harbor") {
			t.Errorf("rebinding Host %q: %d %s", h, r.Code, clip(r.Body.String()))
		}
		// A rebound page cannot tell a mapped login from an unmapped one.
		u := asLoginOf(f, "mallory@example.com", loginReq{method: "GET", target: "/v0/projects", hdr: map[string]string{"Host": h}})
		if u.Code != r.Code || u.Body.String() != r.Body.String() {
			t.Errorf("Host %q: mapped %d %q, unmapped %d %q", h, r.Code, r.Body.String(), u.Code, u.Body.String())
		}
	}
}

func TestTailnetLoginWritesNeedAZmClient(t *testing.T) {
	f := loginFixture(t)
	owner := client.WithHandler(f.dm.SocketHandler())
	write := func(title string, hdr map[string]string) *httptest.ResponseRecorder {
		h := map[string]string{"Zm-Project": "harbor"}
		for k, v := range hdr {
			h[k] = v
		}
		return asLogin(f, loginReq{method: "POST", target: "/v0/work", body: `{"title":"` + title + `"}`, hdr: h})
	}
	// What a page can send cross-site with no preflight: a no-cors fetch or
	// a form, text/plain or form-encoded, no custom headers.
	refused := map[string]map[string]string{
		"csrf no-cors":       {"Content-Type": "text/plain;charset=UTF-8", "Origin": "https://evil.example", "Sec-Fetch-Site": "cross-site"},
		"csrf old browser":   {"Content-Type": "text/plain"}, // no Origin, no Sec-Fetch-Site: still no Zm-Protocol
		"form":               {"Content-Type": "application/x-www-form-urlencoded"},
		"cross-site header":  {"Zm-Protocol": "zm/v0", "Sec-Fetch-Site": "cross-site"},
		"same-site header":   {"Zm-Protocol": "zm/v0", "Sec-Fetch-Site": "same-site"},
		"foreign Origin":     {"Zm-Protocol": "zm/v0", "Origin": "http://evil.example"},
		"null Origin":        {"Zm-Protocol": "zm/v0", "Origin": "null"},
		"lookalike Origin":   {"Zm-Protocol": "zm/v0", "Origin": "http://zm-mini.evil.example:7433"},
		"rebound Host":       {"Zm-Protocol": "zm/v0", "Host": "evil.example:7433"},
		"rebound Host+Origi": {"Zm-Protocol": "zm/v0", "Host": "evil.example:7433", "Origin": "http://evil.example:7433", "Sec-Fetch-Site": "same-origin"},
		// The header's Potluck name (what a client of this build sends) unlocks no more.
		"cross-site new header": {"Potluck-Protocol": "potluck/1", "Sec-Fetch-Site": "cross-site"},
		"foreign Origin new":    {"Potluck-Protocol": "potluck/1", "Origin": "http://evil.example"},
		"rebound Host new":      {"Potluck-Protocol": "potluck/1", "Host": "evil.example:7433"},
	}
	for name, h := range refused {
		if r := write("ATTACK "+name, h); !refusedByGuard(r) {
			t.Errorf("%s: %d %s, want the guard's 401", name, r.Code, clip(r.Body.String()))
		}
	}
	// A zm client (the header) from the node's own origin works.
	for name, h := range map[string]map[string]string{
		"cli":         {"Zm-Protocol": "zm/v0"},
		"same-origin": {"Zm-Protocol": "zm/v0", "Origin": "http://zm-mini:7433", "Sec-Fetch-Site": "same-origin"},
		"typed URL":   {"Zm-Protocol": "zm/v0", "Sec-Fetch-Site": "none"},
		"cli new":     {"Potluck-Protocol": "potluck/1"},
	} {
		if r := write("ok "+name, h); r.Code != 200 {
			t.Errorf("%s: %d %s, want 200", name, r.Code, clip(r.Body.String()))
		}
	}
	wl, err := orchclient.WorkList(context.Background(), owner, orchapi.WorkQuery{Q: "ATTACK", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if len(wl.Items) != 0 {
		t.Fatalf("a refused write landed: %+v", wl.Items)
	}
}

// Every route, walked as the login caller: a foreign Host is refused
// everywhere, every write without Zm-Protocol is refused, and the guard is
// what refuses on every authed route (local and device routes refuse a
// network login anyway; they must still say no). With the node's name and
// the header, no route is refused by the guard: zm clients keep working.
// Public routes carry nothing scoped and keep their own checks.
func TestTailnetLoginGuardEveryRoute(t *testing.T) {
	f := loginFixture(t)
	for _, rt := range f.dm.Router.Routes() {
		if rt.Access == core.Public {
			continue
		}
		method, path, _ := strings.Cut(rt.Pattern, " ")
		path = paramRe.ReplaceAllString(path, "x") + "?live=0" // the event stream answers and ends
		body := ""
		if method != http.MethodGet {
			body = "{}"
		}
		check := func(what string, r *httptest.ResponseRecorder) {
			t.Helper()
			f.checkNoLeak(t, rt.Pattern+" "+what, leakResp{r.Code, r.Body.String()})
			switch {
			case rt.Access == core.Authed && !refusedByGuard(r):
				t.Errorf("%s %s: %d %s, want the guard's 401", rt.Pattern, what, r.Code, clip(r.Body.String()))
			case r.Code < 400:
				t.Errorf("%s %s: %d, want a refusal", rt.Pattern, what, r.Code)
			}
		}
		check("with a rebound Host", asLogin(f, loginReq{method: method, target: path, body: body,
			hdr: map[string]string{"Zm-Protocol": "zm/v0", "Content-Type": "application/json", "Host": "evil.example"}}))
		// A device route answers the paired device's node, not its login
		// (approvals: its own Host, JSON and Zm-Protocol checks); a cross-site
		// GET of its page is a link someone followed, and unreadable cross-site.
		if !(rt.Access == core.Device && method == http.MethodGet) {
			check("cross-site", asLogin(f, loginReq{method: method, target: path, body: body,
				hdr: map[string]string{"Zm-Protocol": "zm/v0", "Content-Type": "application/json", "Sec-Fetch-Site": "cross-site"}}))
		}
		if method != http.MethodGet {
			check("without Zm-Protocol", asLogin(f, loginReq{method: method, target: path, body: body, hdr: map[string]string{"Content-Type": "text/plain"}}))
		}
		if ok := asLogin(f, loginReq{method: method, target: path, body: body,
			hdr: map[string]string{"Zm-Protocol": "zm/v0", "Content-Type": "application/json"}}); refusedByGuard(ok) {
			t.Errorf("%s from a zm client at the node's name: refused by the guard: %s", rt.Pattern, clip(ok.Body.String()))
		}
	}
}

// Token callers are not ambient (a page cannot send someone's token), so the
// login guard does not apply to them: a CLI with ZM_HOST and ZM_TOKEN and
// team replicas work as before.
func TestTailnetTokenCallersUnchanged(t *testing.T) {
	f := loginFixture(t)
	r := f.call("GET", "/v0/projects", "", viaTailnet)
	if r.status != 200 {
		t.Fatalf("token over the tailnet: %d %s", r.status, clip(r.body))
	}
	req := httptest.NewRequest("POST", "/v0/mail", strings.NewReader(`{}`))
	req.Header.Set(api.HeaderAuthorization, "Bearer "+f.guestTok) // no Zm-Protocol: a token is explicit
	req = req.WithContext(core.WithRemote(req.Context(), core.Remote{Login: "vida@example.com"}))
	rec := httptest.NewRecorder()
	f.dm.Handler.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), guardText) {
		t.Fatalf("token caller hit the login guard: %s", rec.Body.String())
	}
}
