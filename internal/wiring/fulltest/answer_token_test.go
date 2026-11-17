package fulltest

// The answer-only token's route walk (SPEC 16 "Answer-only tokens"): every
// route any module registers is called with an answer token from the one
// tailnet device it is pinned to. Only the routes that admit it answer, and
// they answer exactly as for an owner token of the same project; every
// other route, an unmatched path and HEAD of a refused GET included,
// refuses it with reason token-limit before anything is looked up. Module
// routes join automatically: a new route is refused until it admits the
// limit on purpose.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	. "github.com/sengirunners/potluck/kit/server"
)

// answerNet is the tailnet as the daemon's node sees it: one phone.
type answerNet struct{}

func (answerNet) Dial(context.Context, string, string) (net.Conn, error) { return nil, net.ErrClosed }
func (answerNet) Peer(context.Context, string) (core.Remote, error) {
	return core.Remote{}, net.ErrClosed
}
func (answerNet) Self(context.Context) (core.NetSelf, error) {
	return core.NetSelf{Running: true, NodeID: "nSELF", DNSName: "zm-mini.example.ts.net", PeerList: []core.NetPeer{answerPhone}}, nil
}
func (answerNet) Up(context.Context, string, time.Duration) (api.TailnetStatus, error) {
	return api.TailnetStatus{}, nil
}

var answerPhone = core.NetPeer{Host: "ivo-phone", DNSName: "ivo-phone.example.ts.net", NodeID: "nPHONE", NodeKey: "nodekey:phone", Login: "ivo@example.com", OS: "iOS"}

// answerAdmitted is every route an answer token reaches (design: the
// project's asks, card answers and receipt checks; whoami is the kit's).
var answerAdmitted = []string{
	"GET /v0/asks",
	"GET /v0/asks/{id}",
	"GET /v0/decisions",
	"GET /v0/receipts/verify",
	"GET /v0/whoami",
	"POST /v0/asks/{id}/choose",
}

func TestAnswerTokenRouteWalk(t *testing.T) {
	ctx := context.Background()
	cfg, err := config.LoadFrom(shortHome(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	dm, err := New(ctx, cfg, nil, func(d *core.Deps) ([]core.Module, error) {
		mods, err := wiring.Full(d)
		d.Net = answerNet{}
		return mods, err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	freezeProbe(dm)
	liftNameLookups(dm)
	owner := client.WithHandler(dm.SocketHandler())
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor", RepoRules: api.RepoRules{Paths: []string{"/nonexistent/harbor"}}}); err != nil {
		t.Fatal(err)
	}
	in := owner
	in.Project, in.ProjectFrom = "harbor", "flag"
	var item obj
	if err := in.Do(ctx, http.MethodPost, "/v0/work", obj{"title": "pick a colour"}, &item); err != nil {
		t.Fatal(err)
	}
	var ask obj
	if err := in.Do(ctx, http.MethodPost, "/v0/asks", obj{"subject": obj{"work": item.str("id")}, "card": obj{
		"context": "The page needs one header colour.", "question": "Which colour?",
		"choices":   []obj{{"label": "Blue", "pros": "calm", "cons": "common"}, {"label": "Green", "pros": "fresh", "cons": "rare"}},
		"recommend": "a", "why": "calm reads well", "searched": "the page's comments: nothing chosen before",
	}}, &ask); err != nil {
		t.Fatal(err)
	}
	var answer, plain api.MintTokenResponse
	if err := owner.Do(ctx, http.MethodPost, "/v0/tokens", api.MintTokenRequest{Limit: orchapi.AnswerLimit, Project: "harbor", Node: "ivo-phone", TTLSeconds: 3600}, &answer); err != nil {
		t.Fatal(err)
	}
	if err := owner.Do(ctx, http.MethodPost, "/v0/tokens", api.MintTokenRequest{As: "ivo", Project: "harbor", Role: "owner"}, &plain); err != nil {
		t.Fatal(err)
	}
	atPhone := core.Remote{Login: answerPhone.Login, Node: answerPhone.DNSName, NodeID: answerPhone.NodeID, NodeKey: answerPhone.NodeKey}
	call := func(secret, method, target string) leakResp {
		c, cancel := context.WithTimeout(ctx, 2*time.Second) // ends a stream
		defer cancel()
		req := httptest.NewRequestWithContext(c, method, target, strings.NewReader("{}"))
		req.Header.Set(api.HeaderProtocol, api.Protocol)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(api.HeaderProject, "harbor")
		if secret != "" {
			req.Header.Set(api.HeaderAuthorization, "Bearer "+secret)
		}
		req = req.WithContext(core.WithRemote(req.Context(), atPhone))
		rec := httptest.NewRecorder()
		dm.Handler.ServeHTTP(rec, req)
		return leakResp{status: rec.Code, body: rec.Body.String()}
	}
	tokenLimit := func(r leakResp) bool {
		var e struct {
			Error api.Error `json:"error"`
		}
		return r.status == http.StatusForbidden && json.Unmarshal([]byte(r.body), &e) == nil && string(e.Error.Data) == `{"reason":"token-limit"}`
	}

	routes := dm.Router.Routes()
	if len(routes) < 40 {
		t.Fatalf("only %d routes registered", len(routes))
	}
	var admitted []string
	for _, rt := range routes {
		if len(rt.Limits) > 0 {
			admitted = append(admitted, rt.Pattern)
		}
	}
	slices.Sort(admitted)
	if !slices.Equal(admitted, answerAdmitted) {
		t.Fatalf("routes that admit a limit: %q, want exactly %q", admitted, answerAdmitted)
	}
	ids := []string{"x", ask.str("id"), item.str("id"), answer.Token.ID, plain.Token.ID}
	for _, rt := range routes {
		method, path, _ := strings.Cut(rt.Pattern, " ")
		for _, id := range ids {
			target := paramRe.ReplaceAllString(path, id)
			if target == path && id != "x" {
				continue // no parameter: once is enough
			}
			got := call(answer.Secret, method, target)
			switch {
			case slices.Contains(answerAdmitted, rt.Pattern):
				checkAsOwner(t, rt.Pattern, target, got, call, answer.Secret, plain.Secret)
			case rt.Access == core.Public:
				// A public route runs for it as for anyone: no subject.
				if anon := call("", method, target); got.status != anon.status {
					t.Errorf("public %s: the answer token got %d, nobody %d", target, got.status, anon.status)
				}
			default:
				if !tokenLimit(got) {
					t.Errorf("%s %s: answered %d %s, want 403 token-limit", method, target, got.status, clip(got.body))
				}
				if method == http.MethodGet {
					if h := call(answer.Secret, http.MethodHead, target); h.status != http.StatusForbidden {
						t.Errorf("HEAD %s: answered %d, want 403", target, h.status)
					}
				}
			}
		}
	}
	for _, c := range [][2]string{{"GET", "/"}, {"GET", "/v0/no-such-route"}, {"POST", "/v0/asks/x/no-such-step"}, {"DELETE", "/v0/asks/x"}, {"PUT", "/v0/decisions"}} {
		if r := call(answer.Secret, c[0], c[1]); !tokenLimit(r) {
			t.Errorf("unmatched %s %s: answered %d %s", c[0], c[1], r.status, clip(r.body))
		}
	}
	// Not vacuous: the token does read the ask and answer it.
	if r := call(answer.Secret, "GET", "/v0/asks/"+ask.str("id")); r.status != 200 || !strings.Contains(r.body, ask.str("id")) {
		t.Fatalf("the ask: %d %s", r.status, clip(r.body))
	}
	req := httptest.NewRequest(http.MethodPost, "/v0/asks/"+ask.str("id")+"/choose", strings.NewReader(`{"decision":"a"}`))
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(api.HeaderAuthorization, "Bearer "+answer.Secret)
	req = req.WithContext(core.WithRemote(req.Context(), atPhone))
	rec := httptest.NewRecorder()
	dm.Handler.ServeHTTP(rec, req)
	var d orchapi.Decision
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &d) != nil || d.Surface != orchapi.SurfaceAnswerToken || d.Receipt.Node != "owner:answer-token:"+answer.Token.ID {
		t.Fatalf("choose: %d %s", rec.Code, clip(rec.Body.String()))
	}
}

// checkAsOwner: an admitted route answers the answer token as it answers an
// owner token of the project (the Decisions list as with work=0, the asks
// only; whoami says its own limit).
func checkAsOwner(t *testing.T, pattern, target string, got leakResp, call func(secret, method, target string) leakResp, answer, plain string) {
	t.Helper()
	method, _, _ := strings.Cut(pattern, " ")
	switch pattern {
	case "GET /v0/whoami":
		var who struct{ Limit string }
		if got.status != 200 || json.Unmarshal([]byte(got.body), &who) != nil || who.Limit != orchapi.AnswerLimit {
			t.Errorf("whoami: %d %s", got.status, clip(got.body))
		}
		return
	case "GET /v0/decisions":
		target += sep(target) + "work=0"
	}
	want := call(plain, method, target)
	if got.status != want.status || got.body != want.body {
		t.Errorf("%s %s: the answer token got %d %s, an owner token %d %s", method, target, got.status, clip(got.body), want.status, clip(want.body))
	}
	if method == http.MethodGet {
		if h, ho := call(answer, http.MethodHead, target), call(plain, http.MethodHead, target); h.status != ho.status {
			t.Errorf("HEAD %s: the answer token got %d, an owner token %d", target, h.status, ho.status)
		}
	}
}
