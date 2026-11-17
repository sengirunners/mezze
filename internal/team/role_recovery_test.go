package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/teamlink"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
)

type roleAuthority struct {
	mu       sync.Mutex
	h        api.TeamHeader
	requests []string
	bad      string
}

func (a *roleAuthority) RoundTrip(r *http.Request) (*http.Response, error) {
	a.mu.Lock()
	a.requests = append(a.requests, r.Method+" "+r.URL.Path)
	h := a.h
	if r.Header.Get(api.HeaderAuthorization) != "Bearer fixture-role-token" || !api.SameProtocol(api.GetHeader(r.Header, api.HeaderProtocol), api.WireProtocol) {
		a.bad = "missing original token/protocol"
	}
	if r.URL.Path == "/v0/projects/"+h.Project.ID && (api.GetHeader(r.Header, api.HeaderScopeVersion) == "" || api.GetHeader(r.Header, api.HeaderWriteID) != "") {
		a.bad = "probe lost scope version or carried a write id"
	}
	if _, ok := r.Context().Deadline(); !ok {
		a.bad = "unbounded request"
	}
	a.mu.Unlock()
	status, body := http.StatusOK, `{}`
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v0/projects/"+h.Project.ID:
		if h.Role == "" {
			status = http.StatusNotFound
		}
	case r.Method == http.MethodGet && r.URL.Path == "/v0/sync/snapshot":
		item, _ := json.Marshal(orchapi.WorkItem{ID: "local:canonical", Project: h.Project.ID, Title: "fresh canonical", Area: "solver"})
		now := time.Now()
		first, _ := json.Marshal(api.SnapshotLine{Kind: orchapi.SnapItem, Data: item})
		last, _ := json.Marshal(api.SnapshotLine{Kind: api.SnapEnd, Cursor: "03", Now: &now})
		body = string(first) + "\n" + string(last) + "\n"
	case r.Method == http.MethodGet && r.URL.Path == "/v0/sync":
		<-r.Context().Done()
		return nil, r.Context().Err()
	default:
		status, body = http.StatusServiceUnavailable, `{"error":{"code":"unavailable","message":"unexpected mutation"}}`
	}
	return &http.Response{StatusCode: status, Header: http.Header{api.HeaderTeam: {api.EncodeTeamHeader(h)}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func (a *roleAuthority) grant(role string, version int64) api.TeamHeader {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.h.Role, a.h.Cap.Role, a.h.ScopeVersion = role, role, version
	return a.h
}

func roleFixture(t *testing.T) (*Service, *remote, *roleAuthority) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	home := t.TempDir()
	db, err := storetest.Open(ctx, home, Migrations...)
	if err != nil {
		t.Fatal(err)
	}
	p := api.ProjectRef{ID: ids.New(), Slug: "harbor"}
	h := api.TeamHeader{Project: p, Role: "member", ScopeVersion: 1, Cap: api.TeamCap{Project: p.ID, Slug: p.Slug, Role: "member"}}
	a := &roleAuthority{h: h}
	x := &remote{R: api.Remote{Project: p, URL: "http://authority.test", State: api.RemoteOK, Role: h.Role, Cap: h.Cap, ScopeVersion: h.ScopeVersion}, Token: "fixture-role-token", HC: &http.Client{Transport: a}}
	x.HeaderB64 = api.EncodeTeamHeader(h)
	js, _ := json.Marshal(h)
	x.HeaderRaw = string(js)
	s := &Service{d: &core.Deps{Store: db, Config: &config.Config{Home: home, Machine: "bea-mac"}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, rctx: ctx}
	s.link(ctx, x)
	if err := teamlink.WriteFileAtomic(s.TokenPath(p.ID), []byte(x.Token)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, x); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cancel()
		defer db.Close()
		s.stopReplica(x, false)
		roleWait(t, func() bool { x.Mu.Lock(); defer x.Mu.Unlock(); return !x.Recovery.Queued })
	})
	return s, x, a
}

func roleWait(t *testing.T, ready func() bool) {
	t.Helper()
	limit := time.NewTimer(5 * time.Second)
	defer limit.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-limit.C:
			t.Fatal("role recovery barrier timed out")
		case <-tick.C:
		}
	}
}

func roleDeliver(s *Service, x *remote, h api.TeamHeader, status int, sent int64) {
	s.Observe(context.Background(), x, &http.Response{StatusCode: status, Header: http.Header{api.HeaderTeam: {api.EncodeTeamHeader(h)}}}, x.Sent.Add(1), sent)
}

func roleIdle(x *remote) bool {
	x.Mu.Lock()
	defer x.Mu.Unlock()
	return !x.Recovery.Queued
}

func TestRoleRecoveryRegrantAfter700ms(t *testing.T) {
	s, x, a := roleFixture(t)
	ctx := s.rctx
	rp, err := s.openReplica(ctx, x.R.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	x.Copy = rp
	old, _ := json.Marshal(orchapi.WorkItem{ID: "local:revoked", Title: "must disappear"})
	if err := rp.applySnapshot(ctx, []api.SnapshotLine{{Kind: orchapi.SnapItem, Data: old}}, "01", time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := rp.enqueueID(ctx, "revoked-pending", "mail", http.MethodPost, "/v0/mail", []byte(`{"to":"human:other","body":"do not replay"}`), "", "", ""); err != nil {
		t.Fatal(err)
	}
	s.cache.claimedIn(x.R.Project.ID, "bea-mac/1234abcd", "lock:old")
	s.cache.claimedIn("other", "bea-mac/1234abcd", "lock:other")
	a.grant("", 2)
	cctx, cancel := context.WithTimeout(ctx, teamlink.BeatTimeout)
	_ = s.CallRemote(cctx, x, http.MethodGet, "/v0/projects/"+x.R.Project.ID, nil, nil)
	cancel()
	roleWait(t, func() bool {
		_, err := os.Stat(s.replicaPath(x.R.Project.ID))
		return x.Snapshot().State == api.RemoteRevoked && os.IsNotExist(err) && roleIdle(x)
	})
	if err := teamlink.Ended(x.Snapshot()); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatalf("revoked access: %v", err)
	}
	for _, path := range []string{"/v0/team/presence", "/v0/team/writes"} {
		if err := s.CallRemote(ctx, x, http.MethodPost, path, struct{}{}, nil); !api.IsCode(err, api.CodeUnauthenticated) {
			t.Fatalf("revoked mutation %s: %v", path, err)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(retryMin)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				s.BeatRemote(ctx, x)
				if x.Snapshot().State == api.RemoteOK {
					return
				}
			}
		}
	}()
	// This is the product's reported gap, after an actual revoked+deleted
	// barrier, rather than a timing-dependent revoke/grant without a gap.
	timer := time.NewTimer(700 * time.Millisecond)
	<-timer.C
	if x.Snapshot().State != api.RemoteRevoked {
		t.Fatal("recovered without a new grant")
	}
	a.grant("member", 3)
	roleWait(t, func() bool { return x.Snapshot().State == api.RemoteOK && rep(x).isReady() })
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("autonomous probe sender did not finish")
	}
	fresh := rep(x)
	if fresh == rp {
		t.Fatal("revoked copy reused")
	}
	if _, _, ok := fresh.findItem(ctx, "local:canonical"); !ok {
		t.Fatal("fresh snapshot not applied")
	}
	if _, _, ok := fresh.findItem(ctx, "local:revoked"); ok {
		t.Fatal("revoked data reused")
	}
	var pending int
	if err := fresh.db.QueryRow(`SELECT count(*) FROM outbox`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("revoked outbox replayable: %d %v", pending, err)
	}
	s.pmu.Lock()
	oldHeld, otherHeld := s.cache.holds("bea-mac/1234abcd", "lock:old"), s.cache.holds("bea-mac/1234abcd", "lock:other")
	s.pmu.Unlock()
	if oldHeld || !otherHeld {
		t.Fatalf("project cache purge: old=%v other=%v", oldHeld, otherHeld)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.bad != "" {
		t.Fatal(a.bad)
	}
	for _, r := range a.requests {
		if !strings.HasPrefix(r, "GET ") {
			t.Fatalf("probe replayed a mutation: %s", r)
		}
	}
}

func TestRoleRecoveryHeaderControls(t *testing.T) {
	for _, kind := range []string{"missing", "malformed", "foreign", "stale", "401", "real", "cached-restart"} {
		t.Run(kind, func(t *testing.T) {
			s, x, a := roleFixture(t)
			h := a.grant("", 2)
			roleDeliver(s, x, h, http.StatusNotFound, 1)
			roleWait(t, func() bool { return roleIdle(x) })
			h = a.grant("member", 3)
			status, sent := http.StatusOK, int64(2)
			raw := api.EncodeTeamHeader(h)
			switch kind {
			case "missing":
				raw = ""
			case "malformed":
				raw = "not-a-team-header"
			case "foreign":
				h.Project.ID = ids.New()
				raw = api.EncodeTeamHeader(h)
			case "stale":
				h.ScopeVersion = 1
				sent = 1
				raw = api.EncodeTeamHeader(h)
			case "401":
				status = http.StatusUnauthorized
			case "cached-restart":
				// A persisted role is not live authorization. Even when the
				// next authenticated header serializes identically, it is.
				x.Mu.Lock()
				x.R.Role, x.R.Cap, x.R.ScopeVersion = h.Role, h.Cap, h.ScopeVersion
				js, _ := json.Marshal(h)
				x.HeaderRaw = string(js)
				x.HeaderB64 = ""
				x.Recovery.Ready = false
				x.Mu.Unlock()
				if err := s.SaveRoleRemote(s.rctx, x); err != nil {
					t.Fatal(err)
				}
				if x.Snapshot().State != api.RemoteRevoked {
					t.Fatal("cached disk role revived access")
				}
			}
			s.Observe(s.rctx, x, &http.Response{StatusCode: status, Header: http.Header{api.HeaderTeam: {raw}}}, x.Sent.Add(1), sent)
			want := kind == "real" || kind == "cached-restart"
			roleWait(t, func() bool { return roleIdle(x) })
			if got := x.Snapshot().State == api.RemoteOK; got != want {
				t.Fatalf("header %s recovered=%v want=%v", kind, got, want)
			}
		})
	}
}

func TestRoleRecoveryWaitsForTeardown(t *testing.T) {
	s, x, a := roleFixture(t)
	rp, err := s.openReplica(s.rctx, x.R.Project.ID)
	if err != nil {
		t.Fatal(err)
	}
	x.Copy = rp
	stopping := make(chan struct{})
	x.StopCopy = func() { close(stopping) }
	x.CopyWG.Add(1)
	released := false
	defer func() {
		if !released {
			x.CopyWG.Done()
		}
	}()
	h := a.grant("", 2)
	roleDeliver(s, x, h, http.StatusNotFound, 1)
	select {
	case <-stopping:
	case <-time.After(5 * time.Second):
		t.Fatal("teardown not started")
	}
	h = a.grant("member", 3)
	roleDeliver(s, x, h, http.StatusOK, 2)
	if x.Snapshot().State != api.RemoteRevoked || rep(x) != nil {
		t.Fatal("reopened before old loops terminated")
	}
	if _, err := os.Stat(s.replicaPath(x.R.Project.ID)); err != nil {
		t.Fatal("teardown barrier no longer holds old copy")
	}
	x.CopyWG.Done()
	released = true
	roleWait(t, func() bool { return x.Snapshot().State == api.RemoteOK && roleIdle(x) })
	fresh := rep(x)
	if fresh == nil || fresh == rp {
		t.Fatal("did not open a fresh generation")
	}
	// A duplicate old cleanup request rechecks current state; it cannot
	// delete the newly opened copy after waiting for the lifecycle mutex.
	s.QueueRoleReconcile(x)
	roleWait(t, func() bool { return roleIdle(x) })
	if rep(x) != fresh {
		t.Fatal("late cleanup replaced fresh copy")
	}
	if _, err := os.Stat(s.replicaPath(x.R.Project.ID)); err != nil {
		t.Fatal("late cleanup deleted fresh copy")
	}
}

func TestRoleRecoveryLateForegroundReply(t *testing.T) {
	s, x, a := roleFixture(t)
	fence := x.RoleReply()
	h := a.grant("", 2)
	roleDeliver(s, x, h, http.StatusNotFound, 1)
	roleWait(t, func() bool { return roleIdle(x) })
	h = a.grant("member", 3)
	roleDeliver(s, x, h, http.StatusOK, 2)
	roleWait(t, func() bool { return roleIdle(x) })
	// Also exercise an ABA of visible scope/cap; lifetime still differs.
	x.Mu.Lock()
	x.R.ScopeVersion = fence.Snap.ScopeVersion
	x.R.Cap = fence.Snap.Cap
	x.Mu.Unlock()
	r := httptest.NewRequest(http.MethodPost, "http://replica/v0/leases", strings.NewReader(`{}`))
	r = r.WithContext(core.WithSubject(r.Context(), &authz.Subject{Address: "bea-mac/1234abcd", Session: "bea-mac/1234abcd"}))
	body, _ := json.Marshal(orchapi.ClaimResponse{Lease: orchapi.Lease{Project: x.R.Project.ID, Key: "lock:stale", Holder: "bea-mac/1234abcd"}})
	remoteReq := r.WithContext(teamlink.WithReplyFence(r.Context(), fence))
	s.relay(httptest.NewRecorder(), r, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body))), Request: remoteReq})
	if s.cache.holds("bea-mac/1234abcd", "lock:stale") {
		t.Fatal("old foreground reply repopulated recovered cache")
	}
	if unguard, ok := fence.Guard(); ok {
		unguard()
		t.Fatal("old heartbeat lifetime admitted after recovery")
	}
}

func TestRoleRecoveryTerminalLeaveAndReplacement(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprint(replacement), func(t *testing.T) {
			s, x, a := roleFixture(t)
			rp, err := s.openReplica(s.rctx, x.R.Project.ID)
			if err != nil {
				t.Fatal(err)
			}
			x.Copy = rp
			stopping := make(chan struct{})
			x.StopCopy = func() { close(stopping) }
			x.CopyWG.Add(1)
			released := false
			defer func() {
				if !released {
					x.CopyWG.Done()
				}
			}()
			h := a.grant("", 2)
			roleDeliver(s, x, h, http.StatusNotFound, 1)
			select {
			case <-stopping:
			case <-time.After(5 * time.Second):
				t.Fatal("old cleanup not waiting")
			}
			done := make(chan error, 1)
			go func() {
				if replacement {
					done <- s.RetireRoleRemote(x)
				} else {
					s.Remove(s.rctx, x)
					done <- nil
				}
			}()
			roleWait(t, func() bool { x.Mu.Lock(); defer x.Mu.Unlock(); return x.Left })
			h = a.grant("member", 3)
			roleDeliver(s, x, h, http.StatusOK, 2)
			x.CopyWG.Done()
			released = true
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("terminal retirement deadlocked")
			}
			if replacement {
				// This is finishJoin's ordering: retire old object completely,
				// then publish the replacement sharing that same copy path.
				fresh := &remote{R: api.Remote{Project: h.Project, State: api.RemoteOK, Role: h.Role, Cap: h.Cap, ScopeVersion: h.ScopeVersion}, Token: "fixture-role-token", HC: x.HC}
				s.Add(fresh)
				if err := s.Save(s.rctx, fresh); err != nil {
					t.Fatal(err)
				}
				rp, err := s.openReplica(s.rctx, h.Project.ID)
				if err != nil {
					t.Fatal(err)
				}
				fresh.Copy = rp
				t.Cleanup(func() { s.stopReplica(fresh, false) })
				s.QueueRoleReconcile(x)
				roleWait(t, func() bool { return roleIdle(x) })
				if rep(fresh) != rp {
					t.Fatal("old queued retirement touched replacement")
				}
				if _, err := os.Stat(s.replicaPath(h.Project.ID)); err != nil {
					t.Fatal("old cleanup deleted replacement path")
				}
			} else {
				roleDeliver(s, x, h, http.StatusOK, 2)
				if err := s.SaveRoleRemote(s.rctx, x); err != nil {
					t.Fatal(err)
				}
				var n int
				if err := s.d.Store.Registry().QueryRow(`SELECT count(*) FROM team_remotes WHERE project=?`, h.Project.ID).Scan(&n); err != nil || n != 0 {
					t.Fatalf("left remote reinserted: %d %v", n, err)
				}
				a.mu.Lock()
				before := len(a.requests)
				a.mu.Unlock()
				s.ProbeRole(s.rctx, x)
				a.mu.Lock()
				after := len(a.requests)
				a.mu.Unlock()
				if after != before {
					t.Fatal("left remote probed authority")
				}
			}
		})
	}
}

func TestRoleRecoveryRestartNeedsFreshHeaderAndToken(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprint(missing), func(t *testing.T) {
			s, x, a := roleFixture(t)
			h := a.grant("member", 3)
			x.Mu.Lock()
			x.R.State = api.RemoteRevoked
			x.R.Role, x.R.Cap, x.R.ScopeVersion = h.Role, h.Cap, h.ScopeVersion
			js, _ := json.Marshal(h)
			x.HeaderRaw = string(js)
			x.Mu.Unlock()
			if err := s.SaveRoleRemote(s.rctx, x); err != nil {
				t.Fatal(err)
			}
			// Leave an actual pre-crash copy/outbox on disk, not an opened
			// pointer: startup must purge it before trusting any recovery.
			rp, err := s.openReplica(s.rctx, h.Project.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rp.enqueueID(s.rctx, "pre-crash", "mail", http.MethodPost, "/v0/mail", []byte(`{}`), "", "", ""); err != nil {
				t.Fatal(err)
			}
			rp.close(false)
			if missing {
				if err := os.Remove(s.TokenPath(h.Project.ID)); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.Load(s.rctx); err != nil {
				t.Fatal(err)
			}
			loaded := s.ByName(h.Project.ID)
			loaded.HC = &http.Client{Transport: a}
			t.Cleanup(func() { s.stopReplica(loaded, false) })
			s.startReplica(s.rctx, loaded)
			if rep(loaded) != nil {
				t.Fatal("cached role opened copy without a fresh authenticated header")
			}
			if _, err := os.Stat(s.replicaPath(h.Project.ID)); !os.IsNotExist(err) {
				t.Fatalf("startup retained revoked copy: %v", err)
			}
			s.ProbeRole(s.rctx, loaded)
			roleWait(t, func() bool { return roleIdle(loaded) })
			if missing {
				if loaded.Snapshot().State != api.RemotePurged {
					t.Fatalf("missing token state: %s", loaded.Snapshot().State)
				}
				a.mu.Lock()
				n := len(a.requests)
				a.mu.Unlock()
				if n != 0 {
					t.Fatal("missing token contacted authority")
				}
			} else {
				if loaded.Snapshot().State != api.RemoteOK {
					t.Fatal("fresh identical serialized header did not recover")
				}
			}
		})
	}
}

func TestRoleRecoveryTokenPurgeAndCloseStayTerminal(t *testing.T) {
	for _, closeOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(closeOnly), func(t *testing.T) {
			s, x, a := roleFixture(t)
			h := a.grant("", 2)
			roleDeliver(s, x, h, http.StatusNotFound, 1)
			roleWait(t, func() bool { return roleIdle(x) })
			if closeOnly {
				s.stopReplica(x, false)
			} else {
				roleDeliver(s, x, h, http.StatusUnauthorized, 2)
				roleWait(t, func() bool { return roleIdle(x) })
				if x.Snapshot().State != api.RemoteRevoked {
					t.Fatal("one transient 401 purged token")
				}
				if _, err := os.Stat(s.TokenPath(h.Project.ID)); err != nil {
					t.Fatal("one transient 401 removed token")
				}
				x.Mu.Lock()
				past := time.Now().Add(-teamlink.PurgeAfter - time.Second)
				x.Unauth = []time.Time{past, past}
				x.Mu.Unlock()
				roleDeliver(s, x, h, http.StatusUnauthorized, 2)
				roleWait(t, func() bool { return roleIdle(x) })
				if x.Snapshot().State != api.RemotePurged {
					t.Fatal("role-revoked token did not reach terminal 401 purge")
				}
				if _, err := os.Stat(s.TokenPath(h.Project.ID)); !os.IsNotExist(err) {
					t.Fatalf("purged token remains: %v", err)
				}
			}
			h = a.grant("member", 3)
			roleDeliver(s, x, h, http.StatusOK, 2)
			a.mu.Lock()
			before := len(a.requests)
			a.mu.Unlock()
			s.ProbeRole(s.rctx, x)
			a.mu.Lock()
			after := len(a.requests)
			a.mu.Unlock()
			roleWait(t, func() bool { return roleIdle(x) })
			if after != before || rep(x) != nil || x.Snapshot().State == api.RemoteOK {
				t.Fatal("terminal remote recovered or probed")
			}
		})
	}
}

func TestRoleRecoveryProbeBackoff(t *testing.T) {
	s, x, a := roleFixture(t)
	h := a.grant("", 2)
	roleDeliver(s, x, h, http.StatusNotFound, 1)
	roleWait(t, func() bool { return roleIdle(x) })
	s.ProbeRole(s.rctx, x)
	a.mu.Lock()
	before := len(a.requests)
	a.mu.Unlock()
	for range 10 {
		s.ProbeRole(s.rctx, x)
	}
	a.mu.Lock()
	after := len(a.requests)
	bad := a.bad
	a.mu.Unlock()
	if before != 1 || after != before || bad != "" {
		t.Fatalf("unbounded probe admission: before=%d after=%d bad=%s", before, after, bad)
	}
	x.Mu.Lock()
	backoff, at := x.Recovery.Backoff, x.Recovery.RetryAt
	x.Mu.Unlock()
	if backoff < retryMin || backoff > retryMax || !at.After(time.Now()) {
		t.Fatalf("probe backoff: %v %v", backoff, at)
	}
}

func TestRoleRecoveryOldPurgeCannotDeleteReplacementToken(t *testing.T) {
	s, x, _ := roleFixture(t)
	// Pause the old observer's file operation at the exact persistence
	// boundary. Retirement wins that boundary, then a new join publishes a
	// distinct token at the shared path before the old purge can continue.
	x.PersistMu.Lock()
	started, done := make(chan struct{}), make(chan struct{})
	go func() { close(started); s.PurgeRoleToken(x); close(done) }()
	<-started
	x.RetireRoleLocked()
	const replacement = "fixture-replacement-token"
	if err := teamlink.WriteFileAtomic(s.TokenPath(x.Snapshot().Project.ID), []byte(replacement)); err != nil {
		x.PersistMu.Unlock()
		t.Fatal(err)
	}
	x.PersistMu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("old purge did not finish")
	}
	b, err := os.ReadFile(s.TokenPath(x.Snapshot().Project.ID))
	if err != nil || string(b) != replacement {
		t.Fatalf("old purge deleted replacement token: %q %v", b, err)
	}
	// Conversely a current object's purge still removes its own token.
	s2, x2, _ := roleFixture(t)
	s2.PurgeRoleToken(x2)
	if _, err := os.Stat(s2.TokenPath(x2.Snapshot().Project.ID)); !os.IsNotExist(err) {
		t.Fatalf("current token was not purged: %v", err)
	}
}

type roleCalloutSessions struct {
	orchcore.SessionService
	orchcore.SharedPrompts
	session orchapi.Session
	effect  func(string)
}

func (s *roleCalloutSessions) Lookup(context.Context, string) (orchapi.Session, bool) {
	s.effect("lookup")
	return s.session, true
}
func (s *roleCalloutSessions) SetState(context.Context, string, orchapi.StateRequest) error {
	s.effect("state")
	return nil
}
func (s *roleCalloutSessions) Nudge(context.Context, string, string) (bool, error) {
	s.effect("nudge")
	return true, nil
}
func (s *roleCalloutSessions) DeliverPrompt(context.Context, string, orchapi.SessionPrompt) (bool, error) {
	s.effect("prompt")
	return true, nil
}

func TestRoleRecoveryCalloutsDoNotHoldPublicationLock(t *testing.T) {
	for _, kind := range []string{"lookup", "nudge", "prompt", "answered", "mail", "ask"} {
		t.Run(kind, func(t *testing.T) {
			s, x, a := roleFixture(t)
			const addr = "bea-mac/1234abcd"
			fence := x.RoleReply()
			s.cache.claimedIn(x.R.Project.ID, addr, "lock:old")
			if kind == "lookup" {
				s.cache.claimedIn(x.R.Project.ID, "bea-mac/abcd1234", "lock:lookup")
			}
			ss := orchapi.Session{Address: addr, Project: x.R.Project.ID, State: orchapi.StateRunning}
			if kind == "answered" {
				ss.State, ss.Note = orchapi.StateNeedsHuman, "waiting on mail x"
			}
			revoked, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce, effectOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(func() {
				unblock()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("callout did not finish")
				}
			})
			target := kind
			if kind == "answered" || kind == "mail" || kind == "ask" {
				target = "state"
			}
			orchcore.Wire(s.d).Sessions = &roleCalloutSessions{session: ss, effect: func(effect string) {
				if effect != target {
					return
				}
				effectOnce.Do(func() {
					// Reentrant observe used to deadlock trying to upgrade the
					// read lock held by this very callback. Then keep the effect
					// blocked while revocation and explicit leave fully complete.
					roleDeliver(s, x, a.grant("", 2), http.StatusNotFound, 1)
					close(revoked)
					<-release
				})
			}}
			prompt := orchapi.SessionPrompt{ID: ids.New()}
			go func() {
				defer close(done)
				if kind == "mail" || kind == "ask" {
					path, body := "/v0/mail", []byte(nil)
					if kind == "mail" {
						body, _ = json.Marshal(orchapi.Mail{ID: "needs-human", From: addr, Needs: orchapi.NeedsHuman})
					} else {
						path = "/v0/asks"
						body, _ = json.Marshal(orchapi.Ask{ID: "ask-human", AskedBy: addr})
					}
					r := httptest.NewRequest(http.MethodPost, "http://replica"+path, nil)
					r = r.WithContext(core.WithSubject(r.Context(), &authz.Subject{Address: addr, Session: addr}))
					out := r.WithContext(teamlink.WithReplyFence(r.Context(), fence))
					s.relay(httptest.NewRecorder(), r, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body))), Request: out})
					return
				}
				answer := orchapi.PresenceAnswer{Address: addr, Held: []string{"lock:old"}}
				if kind == "nudge" {
					answer.Notice = []orchapi.NoticePart{{Unread: 1}}
				}
				if kind == "prompt" {
					answer.Prompts = []orchapi.SessionPrompt{prompt}
				}
				s.applyPresence(s.rctx, fence.Snap, map[string]orchapi.Session{addr: ss}, nil, map[string][]string{addr: {"lock:old"}}, orchapi.SessionsBeatAnswer{Sessions: []orchapi.PresenceAnswer{answer}}, presenceAnswerContext{role: &fence})
			}()
			select {
			case <-revoked:
			case <-time.After(5 * time.Second):
				t.Fatal("reentrant callout blocked authority revocation")
			}
			roleWait(t, func() bool { return roleIdle(x) })
			left := make(chan struct{})
			go func() { s.Remove(s.rctx, x); close(left) }()
			select {
			case <-left:
			case <-time.After(5 * time.Second):
				t.Fatal("blocked callout held explicit leave")
			}
			if s.cache.holds(addr, "lock:old") {
				t.Fatal("revoked cache survived blocked callout")
			}
			unblock()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("callout did not finish after release")
			}
			if s.cache.holds(addr, "lock:old") {
				t.Fatal("callout resurrected revoked cache")
			}
			if kind == "prompt" {
				var state string
				if err := s.d.Store.Registry().QueryRow(`SELECT state FROM team_prompts_typed WHERE id=?`, prompt.ID).Scan(&state); err != nil || state != "typed" {
					t.Fatalf("admitted prompt lost durable completion: %q %v", state, err)
				}
			}
		})
	}
}

func TestRoleRecoveryRetriesSaveAndOpenFailures(t *testing.T) {
	s, x, a := roleFixture(t)
	h := a.grant("member", 3)
	x.Mu.Lock()
	x.R.State = api.RemoteRevoked
	x.R.Role, x.R.Cap, x.R.ScopeVersion = h.Role, h.Cap, h.ScopeVersion
	x.Recovery.Ready = true
	x.Mu.Unlock()
	if err := s.SaveRoleRemote(s.rctx, x); err != nil {
		t.Fatal(err)
	}
	fail := errors.New("injected recovery save failure")
	x.ReplicaMu.Lock()
	err := s.ReconcileRoleLocked(s.rctx, x, teamlink.RoleRecoveryOps{Save: func(context.Context, *remote) error { return fail }})
	x.ReplicaMu.Unlock()
	if !errors.Is(err, fail) {
		t.Fatalf("save fault missing: %v", err)
	}
	x.Mu.Lock()
	needed, dirty := x.Recovery.Opening, x.Recovery.Dirty
	x.Mu.Unlock()
	if !needed || !dirty || rep(x) != nil {
		t.Fatal("failed save lost recovery obligation")
	}
	x.ReplicaMu.Lock()
	err = s.ReconcileRoleLocked(s.rctx, x, teamlink.RoleRecoveryOps{Open: func(context.Context, *remote) {}})
	x.ReplicaMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	x.Mu.Lock()
	needed, dirty = x.Recovery.Opening, x.Recovery.Dirty
	x.Mu.Unlock()
	if !needed || dirty || rep(x) != nil {
		t.Fatal("failed open lost recovery obligation")
	}
	// The ordinary heartbeat path must admit a bounded retry even though
	// the genuine header already moved the remote to OK and consumed ready.
	s.BeatRemote(s.rctx, x)
	roleWait(t, func() bool { return rep(x).isReady() && roleIdle(x) })
	x.Mu.Lock()
	needed = x.Recovery.Opening
	x.Mu.Unlock()
	if needed {
		t.Fatal("successful fresh open did not retire recovery obligation")
	}
}
