package webui

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/project"
)

type pushTransport struct {
	mu         sync.Mutex
	paths      []string
	bodies     [][]byte
	fail       bool
	status     int
	retryAfter string
	err        error
}

type stalledPushTransport struct {
	release <-chan struct{}
	other   chan struct{}
	once    sync.Once
}

func (p *stalledPushTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Path == "/alice" {
		<-p.release
	} else {
		p.once.Do(func() { close(p.other) })
	}
	return &http.Response{StatusCode: 201, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
}

func (p *pushTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paths = append(p.paths, r.URL.Path)
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		p.bodies = append(p.bodies, b)
	}
	if p.err != nil {
		return nil, p.err
	}
	status := 201
	if p.fail {
		status = 503
	}
	if p.status != 0 {
		status = p.status
	}
	header := make(http.Header)
	if p.retryAfter != "" {
		header.Set("Retry-After", p.retryAfter)
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader("")), Header: header}, nil
}
func TestPushEventsRespectPersonAndScope(t *testing.T) {
	ctx := context.Background()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Machine = "mini"
	cfg.Human = "ana"
	db, err := storetest.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg, err := project.Open(ctx, db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := events.New(ctx, db, cfg.Machine)
	if err != nil {
		t.Fatal(err)
	}
	d := &core.Deps{Config: cfg, Store: db, Projects: reg, Events: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	live := &pushSessionStub{}
	o := orchcore.Wire(d)
	o.Sessions = live
	o.Mail = pushMailStub{}
	s := New(d)
	first, err := s.vapid()
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(cfg.Path("secrets/web-push-vapid"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("VAPID secret permissions: %v %v", st, err)
	}
	second, err := New(d).vapid()
	if err != nil || first.D.Cmp(second.D) != 0 {
		t.Fatalf("VAPID key not persistent: %v", err)
	}
	transport := &pushTransport{}
	s.pushHTTP = &http.Client{Transport: transport}
	project1, err := reg.Create(ctx, reg.Local(""), api.NewProjectRequest{Slug: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = reg.Create(ctx, reg.Local(""), api.NewProjectRequest{Slug: "beta"})
	if err != nil {
		t.Fatal(err)
	}
	var tokens []api.MintTokenResponse
	for _, v := range []struct{ name, project string }{{"alice", "alpha"}, {"bob", "beta"}} {
		tok, e := reg.Mint(ctx, reg.Local(""), api.MintTokenRequest{As: v.name, Project: v.project, Role: "owner"})
		if e != nil {
			t.Fatal(e)
		}
		tokens = append(tokens, tok)
		sub, e := reg.FromToken(ctx, tok.Secret, true)
		if e != nil {
			t.Fatal(e)
		}
		priv, e := ecdh.P256().GenerateKey(rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		auth := make([]byte, 16)
		if _, e = rand.Read(auth); e != nil {
			t.Fatal(e)
		}
		req := pushSubscription{Endpoint: "https://fcm.googleapis.com/" + v.name}
		req.Keys.P256DH = raw64.EncodeToString(priv.PublicKey().Bytes())
		req.Keys.Auth = raw64.EncodeToString(auth)
		pairForTest(t, s, sub, "n"+v.name, req)
	}
	pub := func(typ string, data any) {
		t.Helper()
		if ss, ok := data.(orchapi.Session); ok {
			live.session = ss
		}
		e, err := bus.Publish(ctx, events.Input{Type: typ, Subject: "mini/aaaaaaaa", Project: project1.ID, Data: data})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.processPush(ctx, e); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Registry().ExecContext(ctx, `DELETE FROM web_push_rate`); err != nil {
			t.Fatal(err)
		}
		if _, err = db.Registry().ExecContext(ctx, `UPDATE web_push_queue SET next_at=0`); err != nil {
			t.Fatal(err)
		}
		if err = s.drainPush(ctx); err != nil {
			t.Fatal(err)
		}
	}
	old := orchapi.Session{Address: "mini/bbbbbbbb", Project: project1.ID, State: orchapi.StateNeedsHuman}
	if _, err := bus.Publish(ctx, events.Input{Type: orchapi.EventSessionStateChanged, Subject: old.Address, Project: project1.ID, Data: old}); err != nil {
		t.Fatal(err)
	}
	old.Note = "updated note"
	note, err := bus.Publish(ctx, events.Input{Type: orchapi.EventSessionStateChanged, Subject: old.Address, Project: project1.ID, Data: old})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.processPush(ctx, note); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Registry().ExecContext(ctx, `UPDATE web_push_queue SET next_at=0`); err != nil {
		t.Fatal(err)
	}
	if err = s.drainPush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(transport.paths) != 0 {
		t.Fatalf("already waiting session notified again: %v", transport.paths)
	}
	aliceSub, err := reg.FromToken(ctx, tokens[0].Secret, true)
	if err != nil {
		t.Fatal(err)
	}
	session := orchapi.Session{Address: "mini/aaaaaaaa", Project: project1.ID, State: orchapi.StateNeedsHuman, Launcher: aliceSub.Identity.ID}
	pub(orchapi.EventSessionStateChanged, session)
	pub(orchapi.EventSessionStateChanged, session)
	if strings.Join(transport.paths, ",") != "/alice" {
		t.Fatalf("entry and repeated state sent to %v", transport.paths)
	}
	session.State = orchapi.StateRunning
	pub(orchapi.EventSessionStateChanged, session)
	session.State = orchapi.StateNeedsHuman
	pub(orchapi.EventSessionStateChanged, session)
	session.State = orchapi.StateEnded
	session.Outcome = &orchapi.Outcome{Kind: orchapi.OutcomeOK}
	pub(orchapi.EventSessionEnded, session)
	session.Outcome = &orchapi.Outcome{Kind: orchapi.OutcomeFailed}
	pub(orchapi.EventSessionEnded, session)
	m := orchapi.Mail{ID: "01M00000000000000000000000", Needs: orchapi.NeedsHuman, Recipient: "human:alice"}
	pub(orchapi.EventMailSent, m)
	m.Recipient = "human:bob"
	pub(orchapi.EventMailSent, m)
	if len(transport.paths) != 4 {
		t.Fatalf("expected needs entry twice, failure and Alice mail: %v", transport.paths)
	}
	transport.fail = true
	session.State = orchapi.StateRunning
	pub(orchapi.EventSessionStateChanged, session)
	session.State = orchapi.StateNeedsHuman
	pub(orchapi.EventSessionStateChanged, session)
	var pending int
	if err = db.Registry().QueryRowContext(ctx, `SELECT count(*) FROM web_push_queue`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("failed delivery not queued: %d %v", pending, err)
	}
	transport.fail = false
	if _, err = db.Registry().ExecContext(ctx, `UPDATE web_push_queue SET next_at=0`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Registry().ExecContext(ctx, `UPDATE web_push_queue SET next_at=0`); err != nil {
		t.Fatal(err)
	}
	if err = s.drainPush(ctx); err != nil {
		t.Fatal(err)
	}
	if err = db.Registry().QueryRowContext(ctx, `SELECT count(*) FROM web_push_queue`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("retry not cleared: %d %v", pending, err)
	}
	transport.status = 403
	session.State = orchapi.StateRunning
	pub(orchapi.EventSessionStateChanged, session)
	session.State = orchapi.StateNeedsHuman
	pub(orchapi.EventSessionStateChanged, session)
	if err = db.Registry().QueryRowContext(ctx, `SELECT count(*) FROM web_push_queue`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("permanent 403 was retried: %d %v", pending, err)
	}
	transport.status = 429
	transport.retryAfter = "120"
	session.State = orchapi.StateRunning
	pub(orchapi.EventSessionStateChanged, session)
	session.State = orchapi.StateNeedsHuman
	beforeRetry := time.Now()
	pub(orchapi.EventSessionStateChanged, session)
	var next int64
	var attempts int
	if err = db.Registry().QueryRowContext(ctx, `SELECT next_at,attempts FROM web_push_queue`).Scan(&next, &attempts); err != nil || attempts != 1 || next < beforeRetry.Add(119*time.Second).Unix() {
		t.Fatalf("429 retry schedule: next=%d attempts=%d err=%v", next, attempts, err)
	}
	if _, err = db.Registry().ExecContext(ctx, `DELETE FROM web_push_queue`); err != nil {
		t.Fatal(err)
	}
	transport.retryAfter = ""
	transport.status = 0
	records, err := s.pushRecords(ctx, "")
	if err != nil || len(records) == 0 {
		t.Fatalf("subscriptions: %v", err)
	}
	transport.err = errors.New("network failed")
	if err = s.sendPush(ctx, records[0], pushMessage{Title: "test", URL: decisionsPage}); err == nil || strings.Contains(err.Error(), records[0].endpoint) {
		t.Fatalf("capability URL in delivery error: %v", err)
	}
	transport.err = nil
	transport.status = 410
	if err = s.sendPush(ctx, records[0], pushMessage{Title: "test", URL: decisionsPage}); !errors.Is(err, errPushExpired) {
		t.Fatalf("expired subscription: %v", err)
	}
}

func TestOnlySessionsOwnPersonIsNotified(t *testing.T) {
	s := &Service{}
	sess := orchapi.Session{Address: "mini/aaaaaaaa", Launcher: "alice-id", State: orchapi.StateNeedsHuman}
	data, _ := json.Marshal(sess)
	e := api.Event{Type: orchapi.EventSessionStateChanged, Data: data, Project: "project"}
	alice := &authz.Subject{Identity: api.Identity{ID: "alice-id", Kind: api.IdentityHuman}, Scope: authz.All()}
	bob := &authz.Subject{Identity: api.Identity{ID: "bob-id", Kind: api.IdentityHuman}, Scope: authz.All()}
	if _, ok := s.pushEventMessage(alice, e); !ok {
		t.Fatal("launcher not notified")
	}
	if _, ok := s.pushEventMessage(bob, e); ok {
		t.Fatal("other project member notified")
	}
	sess.State = orchapi.StateEnded
	sess.Outcome = &orchapi.Outcome{Kind: orchapi.OutcomeKilled}
	e.Type = orchapi.EventSessionEnded
	e.Data, _ = json.Marshal(sess)
	if _, ok := s.pushEventMessage(alice, e); ok {
		t.Fatal("person-initiated kill notified")
	}
	// A joined replica's session names its launcher by identity name.
	cfg := &config.Config{Machine: "mini"}
	s = &Service{d: &core.Deps{Config: cfg}}
	alice.Identity.Name = "alice"
	replica := orchapi.Session{Address: "bea-mac/aaaaaaaa", Machine: "bea-mac", Launcher: "alice", State: orchapi.StateNeedsHuman}
	e = api.Event{Type: orchapi.EventSessionStateChanged, Project: "project"}
	e.Data, _ = json.Marshal(replica)
	if msg, ok := s.pushEventMessage(alice, e); !ok || msg.URL != decisionsPage {
		t.Fatalf("replica launcher: %+v %v", msg, ok)
	}
	if _, ok := s.pushEventMessage(bob, e); ok {
		t.Fatal("another person notified of a replica session")
	}
}

func TestBurstIsCoalesced(t *testing.T) {
	ctx := context.Background()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Machine = "mini"
	cfg.Human = "ana"
	db, err := storetest.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg, err := project.Open(ctx, db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	bus, err := events.New(ctx, db, cfg.Machine)
	if err != nil {
		t.Fatal(err)
	}
	d := &core.Deps{Config: cfg, Store: db, Projects: reg, Events: bus, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := New(d)
	tr := &pushTransport{}
	orchcore.Wire(d).Mail = pushMailStub{}
	s.pushHTTP = &http.Client{Transport: tr}
	proj, err := reg.Create(ctx, reg.Local(""), api.NewProjectRequest{Slug: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := reg.Mint(ctx, reg.Local(""), api.MintTokenRequest{As: "alice", Project: "alpha", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := reg.FromToken(ctx, tok.Secret, true)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	req := pushSubscription{Endpoint: "https://fcm.googleapis.com/alice"}
	req.Keys.P256DH = raw64.EncodeToString(key.PublicKey().Bytes())
	req.Keys.Auth = raw64.EncodeToString(make([]byte, 16))
	pairForTest(t, s, sub, "nalice", req)
	req.Endpoint = "https://fcm.googleapis.com/alice-second"
	pairForTest(t, s, sub, "nalice2", req)
	for i := 0; i < 300; i++ {
		m := orchapi.Mail{ID: ids.New(), Needs: orchapi.NeedsHuman, Recipient: "human:alice"}
		e, err := bus.Publish(ctx, events.Input{Type: orchapi.EventMailSent, Project: proj.ID, Subject: m.ID, Data: m})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.processPush(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Registry().ExecContext(ctx, `UPDATE web_push_queue SET next_at=0`); err != nil {
		t.Fatal(err)
	}
	if err = s.drainPush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(tr.paths) != 2 {
		t.Fatalf("burst produced %d pushes", len(tr.paths))
	}
	summary := decryptPushForTest(t, key, make([]byte, 16), tr.bodies[0])
	if summary.Title != "300 things need you" || summary.URL != decisionsPage {
		t.Fatalf("wrong burst summary: %+v", summary)
	}
	summary = decryptPushForTest(t, key, make([]byte, 16), tr.bodies[1])
	if summary.Title != "300 things need you" {
		t.Fatalf("second device lost person-level burst: %+v", summary)
	}
	if _, err = db.Registry().ExecContext(ctx, `DELETE FROM web_push_subscriptions WHERE endpoint_hash=?`, pushHash(req.Endpoint)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Registry().ExecContext(ctx, `DELETE FROM web_push_rate`); err != nil {
		t.Fatal(err)
	}
	live := &pushSessionStub{sessions: map[string]orchapi.Session{}}
	orchcore.Wire(d).Sessions = live
	for i := 0; i < 200; i++ {
		addr := fmt.Sprintf("mini/%08x", i)
		ss := orchapi.Session{Address: addr, Machine: "mini", Project: proj.ID, Launcher: sub.Identity.ID, State: orchapi.StateEnded, Outcome: &orchapi.Outcome{Kind: orchapi.OutcomeInfra}}
		live.sessions[addr] = ss
		e, err := bus.Publish(ctx, events.Input{Type: orchapi.EventSessionEnded, Project: proj.ID, Subject: addr, Data: ss})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.processPush(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	beforeInfra := len(tr.paths)
	if _, err = db.Registry().ExecContext(ctx, `UPDATE web_push_queue SET next_at=0`); err != nil {
		t.Fatal(err)
	}
	if err = s.drainPush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(tr.paths) != beforeInfra+1 {
		t.Fatalf("200 restart failures produced %d pushes", len(tr.paths)-beforeInfra)
	}
	if _, err = db.Registry().ExecContext(ctx, `DELETE FROM web_push_rate`); err != nil {
		t.Fatal(err)
	}
	addr := "mini/flapping"
	ss := orchapi.Session{Address: addr, Machine: "mini", Project: proj.ID, Launcher: sub.Identity.ID}
	for i := 0; i < 100; i++ {
		for _, st := range []orchapi.SessionState{orchapi.StateRunning, orchapi.StateNeedsHuman} {
			ss.State = st
			live.sessions[addr] = ss
			e, err := bus.Publish(ctx, events.Input{Type: orchapi.EventSessionStateChanged, Project: proj.ID, Subject: addr, Data: ss})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.processPush(ctx, e); err != nil {
				t.Fatal(err)
			}
		}
	}
	beforeFlaps := len(tr.paths)
	if _, err = db.Registry().ExecContext(ctx, `UPDATE web_push_queue SET next_at=0`); err != nil {
		t.Fatal(err)
	}
	if err = s.drainPush(ctx); err != nil {
		t.Fatal(err)
	}
	if len(tr.paths) != beforeFlaps+1 {
		t.Fatalf("100 flaps produced %d pushes", len(tr.paths)-beforeFlaps)
	}
	otherTok, err := reg.Mint(ctx, reg.Local(""), api.MintTokenRequest{As: "bob", Project: "alpha", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	otherSub, err := reg.FromToken(ctx, otherTok.Secret, true)
	if err != nil {
		t.Fatal(err)
	}
	req.Endpoint = "https://fcm.googleapis.com/bob"
	pairForTest(t, s, otherSub, "nbob", req)
	if _, err = db.Registry().ExecContext(ctx, `DELETE FROM web_push_rate`); err != nil {
		t.Fatal(err)
	}
	for _, person := range []string{"alice", "bob"} {
		m := orchapi.Mail{ID: ids.New(), Needs: orchapi.NeedsHuman, Recipient: "human:" + person}
		e, err := bus.Publish(ctx, events.Input{Type: orchapi.EventMailSent, Project: proj.ID, Subject: m.ID, Data: m})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.processPush(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = db.Registry().ExecContext(ctx, `UPDATE web_push_queue SET next_at=0`); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	other := make(chan struct{})
	s.pushHTTP = &http.Client{Transport: &stalledPushTransport{release: release, other: other}}
	start := time.Now()
	err = s.drainPush(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) || elapsed > 4*time.Second {
		close(release)
		s.pushWorkers.Wait()
		t.Fatalf("drain not bounded: %s %v", elapsed, err)
	}
	select {
	case <-other:
	default:
		close(release)
		s.pushWorkers.Wait()
		t.Fatal("slow endpoint starved another person")
	}
	close(release)
	s.pushWorkers.Wait()
}

func decryptPushForTest(t *testing.T, receiver *ecdh.PrivateKey, auth, body []byte) pushMessage {
	t.Helper()
	if len(body) < 86 {
		t.Fatal("short push record")
	}
	sender, err := ecdh.P256().NewPublicKey(body[21:86])
	if err != nil {
		t.Fatal(err)
	}
	shared, err := receiver.ECDH(sender)
	if err != nil {
		t.Fatal(err)
	}
	info := append([]byte("WebPush: info\x00"), receiver.PublicKey().Bytes()...)
	info = append(info, sender.Bytes()...)
	ikm, err := hkdf.Key(sha256.New, shared, auth, string(info), 32)
	if err != nil {
		t.Fatal(err)
	}
	cek, err := hkdf.Key(sha256.New, ikm, body[:16], "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := hkdf.Key(sha256.New, ikm, body[:16], "Content-Encoding: nonce\x00", 12)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	clear, err := gcm.Open(nil, nonce, body[86:], nil)
	if err != nil || len(clear) == 0 {
		t.Fatalf("decrypt: %v", err)
	}
	var msg pushMessage
	if err = json.Unmarshal(clear[:len(clear)-1], &msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

type pushSessionStub struct {
	session  orchapi.Session
	sessions map[string]orchapi.Session
}

type pushMailStub struct{}

func (pushMailStub) Send(context.Context, *authz.Subject, api.ProjectRef, string, orchapi.SendMailRequest) (orchapi.Mail, error) {
	return orchapi.Mail{}, nil
}
func (pushMailStub) Reply(context.Context, *authz.Subject, string, string, orchapi.ReplyRequest) (orchapi.Mail, error) {
	return orchapi.Mail{}, nil
}
func (pushMailStub) PushOpen(context.Context, *authz.Subject, string) (bool, error) { return true, nil }

func (x *pushSessionStub) Lookup(_ context.Context, addr string) (orchapi.Session, bool) {
	if x.sessions != nil {
		ss, ok := x.sessions[addr]
		return ss, ok
	}
	return x.session, true
}
func (*pushSessionStub) Visible(context.Context, authz.Scope, string) bool            { return true }
func (*pushSessionStub) Live(context.Context, string) bool                            { return true }
func (*pushSessionStub) SetState(context.Context, string, orchapi.StateRequest) error { return nil }
func (*pushSessionStub) Nudge(context.Context, string, string) (bool, error)          { return false, nil }
func (*pushSessionStub) ManagedLive(context.Context) int                              { return 0 }
func (*pushSessionStub) RestartBlockers(context.Context) []orchapi.Session            { return nil }
func TestResolvedSessionIsNotRetried(t *testing.T) {
	sess := orchapi.Session{Address: "mini/aaaaaaaa", State: orchapi.StateNeedsHuman, Launcher: "alice"}
	data, _ := json.Marshal(sess)
	e := api.Event{Type: orchapi.EventSessionStateChanged, Data: data}
	subject := &authz.Subject{Identity: api.Identity{ID: "alice"}, Scope: authz.All()}
	live := &pushSessionStub{session: sess}
	s := &Service{d: withSessions(&core.Deps{}, live)}
	if open, err := s.pushStillOpen(context.Background(), subject, e); err != nil || !open {
		t.Fatal("waiting session dropped")
	}
	live.session.State = orchapi.StateRunning
	if open, err := s.pushStillOpen(context.Background(), subject, e); err != nil || open {
		t.Fatal("resolved session still queued")
	}
}

// pairForTest makes node a paired device of sub's person, as the Decisions
// page does for an active approver, and subscribes req for it.
func pairForTest(t *testing.T, s *Service, sub *authz.Subject, node string, req pushSubscription) {
	t.Helper()
	person := *sub
	person.TokenID, person.Via = "", authz.ViaTailnet // a device's person: no token
	prev := orchcore.DevicePerson(s.d)
	orchcore.Wire(s.d).DevicePerson = func(ctx context.Context, n string) (*authz.Subject, bool) {
		if n == node {
			return &person, true
		}
		if prev != nil {
			return prev(ctx, n)
		}
		return nil, false
	}
	if err := s.SubscribeDevice(context.Background(), &person, node, req); err != nil {
		t.Fatalf("subscribe %s: %v", node, err)
	}
}

// withSessions is d with ss as its session module.
func withSessions(d *core.Deps, ss orchcore.SessionService) *core.Deps {
	orchcore.Wire(d).Sessions = ss
	return d
}
