package webui_test

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"os"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/webui"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
)

type fixture struct {
	dm    *server.Daemon
	owner *client.Client
	svc   *webui.Service
	cfg   *config.Config
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close(); os.RemoveAll(home) })
	f := &fixture{dm: dm, owner: client.WithHandler(dm.SocketHandler()), cfg: cfg}
	for _, m := range dm.Modules() {
		if s, ok := m.(*webui.Service); ok {
			f.svc = s
		}
	}
	if f.svc == nil {
		t.Fatal("web module not wired")
	}
	return f
}

// pairPhone subscribes a phone (node) of the person tok was minted for, as
// the Decisions page does once that phone is an active approver: the daemon
// treats node as that person's device from now on.
func (f *fixture) pairPhone(t *testing.T, tok api.MintTokenResponse, node, endpoint string) {
	t.Helper()
	ctx := context.Background()
	sub, err := f.dm.Deps.SubscriptionSubject(ctx, tok.Token.ID)
	if err != nil {
		t.Fatal(err)
	}
	person := *sub
	person.TokenID, person.Via = "", authz.ViaTailnet // a device's person: no token
	orchcore.Wire(f.dm.Deps).DevicePerson = func(_ context.Context, n string) (*authz.Subject, bool) {
		return &person, n == node
	}
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var req orchapi.DevicePushRequest
	req.Endpoint = endpoint
	req.Keys.P256DH = base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes())
	req.Keys.Auth = base64.RawURLEncoding.EncodeToString(make([]byte, 16))
	if err := f.svc.SubscribeDevice(ctx, &person, node, req); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
}

func TestRealMailAndSessionPairQueuesOnce(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := f.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "alpha"}); err != nil {
		t.Fatal(err)
	}
	tok, err := f.owner.MintToken(ctx, api.MintTokenRequest{As: "ana", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	f.pairPhone(t, tok, "nPHONE", "https://fcm.googleapis.com/test-real-pair")
	if err := f.svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	f.owner.Project = "alpha"
	f.owner.ProjectFrom = "flag"
	session, err := orchclient.Register(ctx, f.owner, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "push-real-pair"})
	if err != nil {
		t.Fatal(err)
	}
	f.owner.As = session.Address
	// An ask: an agent's other mail to a person is a report, which never
	// pushes (SPEC 9 "Reports").
	ask, err := orchclient.NewAsk(ctx, f.owner, orchapi.NewAskRequest{Card: &orchapi.AskCard{
		Context: "The deploy is ready.", Background: "Tests pass.", Question: "Deploy now?",
		Choices:   []orchapi.AskChoice{{Label: "Deploy", Pros: "shipped", Cons: "risk"}, {Label: "Wait", Pros: "safe", Cons: "late"}},
		Recommend: "a", Why: "It is ready.", Searched: "past rulings and the deploy notes: nothing on this"}})
	if err != nil {
		t.Fatal(err)
	}
	m := orchapi.Mail{ID: ask.ID}
	deadline := time.Now().Add(1800 * time.Millisecond)
	for time.Now().Before(deadline) {
		var state string
		err = f.dm.Deps.Store.Registry().QueryRowContext(ctx, `SELECT state FROM web_push_session_state WHERE address=?`, session.Address).Scan(&state)
		if err == nil && state == string(orchapi.StateNeedsHuman) {
			var count int
			if err = f.dm.Deps.Store.Registry().QueryRowContext(ctx, `SELECT count(*) FROM web_push_queue`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("real mail %s and state queued %d pushes, want 1", m.ID, count)
			}
			sub, err := f.dm.Deps.SubscriptionSubject(ctx, tok.Token.ID)
			if err != nil {
				t.Fatal(err)
			}
			mail := orchcore.Mail(f.dm.Deps).(orchcore.PushMail)
			open, err := mail.PushOpen(ctx, sub, m.ID)
			if err != nil || !open {
				t.Fatalf("open mail: %v %v", open, err)
			}
			if _, err = orchclient.WithdrawAsk(ctx, f.owner, ask.ID); err != nil { // the asker takes it back: its mail is taken
				t.Fatal(err)
			}
			open, err = mail.PushOpen(ctx, sub, m.ID)
			if err != nil || open {
				t.Fatalf("acknowledged mail remains eligible: %v %v", open, err)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session state was not processed: %v", err)
}

func TestRealMachineSessionPush(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := f.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "alpha"}); err != nil {
		t.Fatal(err)
	}
	owner, err := f.owner.MintToken(ctx, api.MintTokenRequest{As: "ana", Role: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	f.pairPhone(t, owner, "nPHONE", "https://fcm.googleapis.com/machine-session")
	if err := f.svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	ss, err := orchclient.Register(ctx, f.owner, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "push-machine", State: orchapi.StateNeedsHuman, Title: "Machine session"})
	if err != nil {
		t.Fatal(err)
	}
	if ss.Project != api.ProjectNone {
		t.Fatalf("want machine project: %+v", ss)
	}
	deadline := time.Now().Add(1800 * time.Millisecond)
	for time.Now().Before(deadline) {
		var n int
		if err = f.dm.Deps.Store.Registry().QueryRowContext(ctx, `SELECT count(*) FROM web_push_queue WHERE project=?`, api.ProjectNone).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	var n int
	if err = f.dm.Deps.Store.Registry().QueryRowContext(ctx, `SELECT count(*) FROM web_push_queue WHERE project=?`, api.ProjectNone).Scan(&n); err != nil || n != 1 {
		t.Fatalf("machine event queued %d: %v", n, err)
	}
}
