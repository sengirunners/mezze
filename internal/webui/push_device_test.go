package webui

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
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

// Phone alerts for a paired device (SPEC 16 "Phone alerts"): each device's
// person hears only their own decisions and news, and nothing once the
// device stops being an active approver.
func TestDevicePushIsItsPersonsOwn(t *testing.T) {
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
	cfg.Machine, cfg.Human = "mini", "ana"
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
	o := orchcore.Wire(d)
	o.Sessions = &pushSessionStub{}
	o.Mail = pushMailStub{}
	s := New(d)
	transport := &pushTransport{}
	s.pushHTTP = &http.Client{Transport: transport}
	p, err := reg.Create(ctx, reg.Local(""), api.NewProjectRequest{Slug: "alpha"})
	if err != nil {
		t.Fatal(err)
	}
	// Two people, both owners of the project: each phone is its own person's.
	people := map[string]*authz.Subject{}
	active := map[string]bool{}
	for _, name := range []string{"ivo", "bea"} {
		tok, err := reg.Mint(ctx, reg.Local(""), api.MintTokenRequest{As: name, Project: "alpha", Role: "owner"})
		if err != nil {
			t.Fatal(err)
		}
		sub, err := reg.FromToken(ctx, tok.Secret, true)
		if err != nil {
			t.Fatal(err)
		}
		person := *sub
		person.TokenID, person.Via = "", authz.ViaTailnet // a device's person: no token
		people["n"+name] = &person
		active["n"+name] = true
	}
	orchcore.Wire(d).DevicePerson = func(_ context.Context, node string) (*authz.Subject, bool) {
		return people[node], active[node]
	}
	for node, person := range people {
		priv, err := ecdh.P256().GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		auth := make([]byte, 16)
		_, _ = rand.Read(auth)
		var req orchapi.DevicePushRequest
		req.Endpoint = "https://fcm.googleapis.com/" + node
		req.Keys.P256DH, req.Keys.Auth = raw64.EncodeToString(priv.PublicKey().Bytes()), raw64.EncodeToString(auth)
		if err := s.SubscribeDevice(ctx, person, node, req); err != nil {
			t.Fatal(err)
		}
	}
	sent := func(what string, m orchapi.Mail) []string {
		t.Helper()
		transport.mu.Lock()
		transport.paths = nil
		transport.mu.Unlock()
		m.ID, m.Project, m.Created = ids.New(), p.ID, time.Now()
		e, err := bus.Publish(ctx, events.Input{Type: orchapi.EventMailSent, Subject: m.ID, Project: p.ID, Data: m})
		if err != nil {
			t.Fatal(what, err)
		}
		if err = s.processPush(ctx, e); err != nil {
			t.Fatal(what, err)
		}
		_, _ = db.Registry().ExecContext(ctx, `DELETE FROM web_push_rate`)
		_, _ = db.Registry().ExecContext(ctx, `UPDATE web_push_queue SET next_at=0`)
		if err = s.drainPush(ctx); err != nil {
			t.Fatal(what, err)
		}
		transport.mu.Lock()
		defer transport.mu.Unlock()
		return slices.Clone(transport.paths)
	}
	ivo := people["nivo"].Address
	news := orchapi.Mail{From: "mini/aaaaaaaa", To: ivo, Recipient: ivo, Kind: orchapi.MailKindNote, Needs: orchapi.NeedsAny,
		Body: "FYI", ForYou: orchapi.ForYouKnow, Audience: orchapi.MailAudienceParticipants, Participants: []string{"mini/aaaaaaaa", ivo}}
	if got := sent("news for ivo", news); !slices.Equal(got, []string{"/nivo"}) {
		t.Fatalf("news for ivo reached %v, want only ivo's phone", got)
	}
	held := news
	at := time.Now().Add(time.Hour)
	held.ForYou, held.HeldUntil = orchapi.ForYouDigest, &at
	if got := sent("a held digest item", held); len(got) != 0 {
		t.Fatalf("a digest item buzzed before 08:00: %v", got)
	}
	active["nivo"] = false // ivo's phone is no longer an approver
	if got := sent("news after unpairing", news); len(got) != 0 {
		t.Fatalf("an unpaired phone still got alerts: %v", got)
	}
}

// zm web test-push reaches the machine owner's paired phone and, like every
// alert, opens the Decisions page.
func TestTestPushReachesOwnersPhone(t *testing.T) {
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
	cfg.Machine, cfg.Human = "mini", "ana"
	db, err := storetest.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	reg, err := project.Open(ctx, db, cfg)
	if err != nil {
		t.Fatal(err)
	}
	d := &core.Deps{Config: cfg, Store: db, Projects: reg, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := New(d)
	transport := &pushTransport{}
	s.pushHTTP = &http.Client{Transport: transport}
	test := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.pushTest(w, httptest.NewRequest("POST", "/v0/web/push/test", nil))
		return w
	}
	if w := test(); w.Code != http.StatusNotFound {
		t.Fatalf("no phone yet: %d %s", w.Code, w.Body.String())
	}
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	var req orchapi.DevicePushRequest
	req.Endpoint = "https://fcm.googleapis.com/nphone"
	req.Keys.P256DH, req.Keys.Auth = raw64.EncodeToString(priv.PublicKey().Bytes()), raw64.EncodeToString(auth)
	pairForTest(t, s, reg.Local(""), "nphone", req)
	if w := test(); w.Code != http.StatusOK {
		t.Fatalf("test push: %d %s", w.Code, w.Body.String())
	}
	if !slices.Equal(transport.paths, []string{"/nphone"}) {
		t.Fatalf("test push reached %v", transport.paths)
	}
	if msg := decryptPushForTest(t, priv, auth, transport.bodies[0]); msg.URL != decisionsPage {
		t.Fatalf("test push opens %q, want %q", msg.URL, decisionsPage)
	}
}
