package team_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/tunnel"
)

// Recovery freshness must compose with real key-bound tokens, route scope,
// write-id idempotency and the replica's independent rescope retry. A bare
// tunnel.Handler fixture has none of those authority checks.
func TestTunnelRecoveryChallengeWithScopedTokensAndRescope(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	a, p := tunnelHome(t, nil, "mini", tailnettest.Identity{})
	teamHarbor(t, a)
	ao := at(a.owner, "harbor", "")
	hidden := mustf(orchclient.CreateWork(ctx, ao, orchapi.CreateWorkRequest{Title: "private billing", Area: "billing"}))(t)
	fp := mustf(tunnelOf(t, a).Fingerprint())(t)

	// The first POST is already signed with the member's old scope version
	// when the grants change. Background sync cannot remove this race: the
	// captured request itself must encounter409 after recovery negotiation.
	target, _ := url.Parse(p.URL())
	proxy := httputil.NewSingleHostReverseProxy(target)
	var armed atomic.Bool
	var narrowed sync.Once
	var mu sync.Mutex
	type attempt struct {
		status int
		hdr    http.Header
		body   []byte
	}
	var attempts []attempt
	edge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !armed.Load() || r.Method != http.MethodPost || r.URL.Path != "/v0/work" {
			proxy.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			http.Error(w, "body", 400)
			return
		}
		hdr := r.Header.Clone()
		narrowed.Do(func() {
			_, err := a.owner.Grant(ctx, api.GrantRequest{Identity: "bea", Role: "guest", Project: "harbor", Area: "solver"})
			if err == nil {
				_, err = a.owner.Revoke(ctx, api.GrantRequest{Identity: "bea", Project: "harbor"})
			}
			if err != nil {
				t.Errorf("narrowing after signed request: %v", err)
			}
		})
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, r)
		mu.Lock()
		attempts = append(attempts, attempt{rec.Code, hdr, body})
		mu.Unlock()
		for k, vs := range rec.Header() {
			w.Header()[k] = vs
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
	}))
	t.Cleanup(edge.Close)
	mustf(a.owner.TunnelUp(ctx, api.TunnelUpRequest{Listen: tunnelOf(t, a).Status().Listen, URL: edge.URL}))(t)
	r, joined := tunnelMember(t, a, "bea-mac", "harbor", "bea", "member")
	g, guest := tunnelMember(t, a, "guest-mac", "harbor/solver", "guest", "guest")
	other := startDaemon(t, nil, "other-mac", tailnettest.Identity{})
	memberToken := readToken(t, joined.Remote.TokenFile)
	guestToken := readToken(t, guest.Remote.TokenFile)
	minted := mustf(a.owner.MintToken(ctx, api.MintTokenRequest{As: "bea", Project: "harbor", Role: "member"}))(t)

	// Retain every token and project but lose the home replay journal. Stop
	// members during the restart so no unrelated timeout obscures readiness.
	r.halt()
	g.halt()
	a.halt()
	if err := os.Remove(filepath.Join(a.cfg.Home, tunnel.NonceFile)); err != nil {
		t.Fatal(err)
	}
	a.boot(nil, tailnettest.Identity{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var st api.TunnelStatus
	eventually(t, 5*time.Second, func() error {
		if st = tunnelOf(t, a).Status(); !st.Running {
			return fmt.Errorf("restored tunnel listener not up: %+v", st)
		}
		return nil
	})
	p.SetTarget(st.Listen)
	if got := mustf(tunnelOf(t, a).Fingerprint())(t); got != fp {
		t.Fatal("restored home changed its pinned key")
	}
	r.boot(nil, tailnettest.Identity{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.boot(nil, tailnettest.Identity{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	waitReachable(t, r, true)
	waitReachable(t, g, true)

	armed.Store(true)
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	created, err := orchclient.CreateWork(writeCtx, at(r.owner, "harbor", ""), orchapi.CreateWorkRequest{Title: "one recovered write", Area: "solver"})
	if err != nil {
		t.Fatalf("member write during replay hold: %v", err)
	}
	armed.Store(false)
	mu.Lock()
	got := append([]attempt(nil), attempts...)
	mu.Unlock()
	wantStatus := []int{503, 409, 503, 200}
	if len(got) != len(wantStatus) {
		t.Fatalf("recovery/rescope attempts: %+v", got)
	}
	writeID := api.GetHeader(got[0].hdr, api.HeaderWriteID)
	if writeID == "" {
		t.Fatal("normal member forwarding omitted its write id")
	}
	seenNonces := map[string]bool{}
	for i, x := range got {
		if x.status != wantStatus[i] || api.GetHeader(x.hdr, api.HeaderWriteID) != writeID || !bytes.Equal(x.body, got[0].body) {
			t.Fatalf("attempt%d changed write or refusal: %+v", i, x)
		}
		if x.hdr.Get(api.HeaderAuthorization) != "Bearer "+memberToken || api.GetHeader(x.hdr, api.HeaderProject) != joined.Remote.Project.ID {
			t.Fatalf("attempt%d changed token/project", i)
		}
		nonce := api.GetHeader(x.hdr, tunnel.HeaderNonce)
		if nonce == "" || seenNonces[nonce] {
			t.Fatalf("attempt%d reused its signing nonce", i)
		}
		seenNonces[nonce] = true
		if (i%2 == 0) != (api.GetHeader(x.hdr, tunnel.HeaderRecovery) == "") {
			t.Fatalf("attempt%d recovery challenge was not negotiated: %+v", i, x.hdr)
		}
	}
	if api.GetHeader(got[0].hdr, api.HeaderScopeVersion) == api.GetHeader(got[2].hdr, api.HeaderScopeVersion) {
		t.Fatal("outer retry kept the refused scope version")
	}
	ao = at(a.owner, "harbor", "")
	wl := mustf(orchclient.WorkList(ctx, ao, orchapi.WorkQuery{}))(t)
	count := 0
	for _, it := range wl.Items {
		if it.Title == "one recovered write" {
			count++
			if it.ID != created.ID || it.Area != "solver" {
				t.Fatalf("authority write differs: %+v", it)
			}
		}
	}
	if count != 1 {
		t.Fatalf("recovery/rescope dispatched %d creations", count)
	}
	if _, err := os.Stat(filepath.Join(a.cfg.Home, tunnel.NonceFile+".hold")); err != nil {
		t.Fatalf("capable write removed the legacy recovery hold: %v", err)
	}

	// The recovery challenge is freshness only: a guest's hidden reads and
	// writes still answer exactly like missing, without disclosing data.
	gc := signedClient(t, g, fp)
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		var codes []int
		var bodies []string
		for _, id := range []string{hidden.ID, "local:zzzzzz"} {
			path := "/v0/work/" + id
			var body any
			if method == http.MethodPost {
				path += "/comment"
				body = orchapi.CommentRequest{Body: "must stay refused"}
			}
			status, text := rawCall(t, gc, method, edge.URL+path, guestToken, body, map[string]string{api.HeaderProject: guest.Remote.Project.ID, api.HeaderWriteID: ids.New()})
			codes = append(codes, status)
			bodies = append(bodies, strings.ReplaceAll(text, id, "ITEM"))
			if strings.Contains(text, "private billing") {
				t.Fatal("recovery disclosed hidden work")
			}
		}
		if codes[0] != 404 || codes[1] != 404 || bodies[0] != bodies[1] {
			t.Fatalf("%s hidden/missing differ: %v %q", method, codes, bodies)
		}
	}
	if cs := mustf(orchclient.Comments(ctx, ao, hidden.ID))(t); len(cs) != 0 {
		t.Fatalf("guest recovery wrote hidden comments: %+v", cs)
	}
	for _, tc := range []struct {
		name  string
		who   *daemon
		token string
	}{
		{"another-key", other, memberToken},
		{"tampered-token", r, memberToken + "x"},
		{"unbound-token", r, minted.Secret},
	} {
		if code, body := rawCall(t, signedClient(t, tc.who, fp), "POST", edge.URL+"/v0/work", tc.token,
			orchapi.CreateWorkRequest{Title: "unauthorized recovery", Area: "solver"}, map[string]string{api.HeaderProject: joined.Remote.Project.ID, api.HeaderWriteID: ids.New()}); code != 401 {
			t.Fatalf("%s gained write authority: %d %s", tc.name, code, body)
		}
	}
	if wl := mustf(orchclient.WorkList(ctx, ao, orchapi.WorkQuery{}))(t); len(wl.Items) != 2 {
		t.Fatalf("unauthorized recovery changed authority work: %+v", wl.Items)
	}
}
