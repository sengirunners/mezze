package secretkeys_test

// The key directory (SPEC 17.12) through its routes: the authority's owner
// publishes its own key, a member machine publishes the key its transport
// certified (over the loopback tailnet), nobody publishes under another
// machine's name, revoke is the machine's or an admin's, and the sender's
// pins say new, pinned or changed.

import (
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/secretkeys"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/sealbox"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/tunnel"
)

type daemon struct {
	dm    *server.Daemon
	cfg   *config.Config
	owner *client.Client
	stop  func()
}

// newHome is a temp ZM_HOME, removed when the test ends.
func newHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	return home
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// start serves a daemon for machine on tn (nil: no tailnet node at all)
// in a new home.
func start(t *testing.T, tn *tailnettest.Net, machine string, id tailnettest.Identity) *daemon {
	t.Helper()
	return startIn(t, tn, machine, id, newHome(t))
}

// startIn is start in home (a restart keeps it).
func startIn(t *testing.T, tn *tailnettest.Net, machine string, id tailnettest.Identity, home string) *daemon {
	t.Helper()
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = machine+"-human", machine
	ctx, cancel := context.WithCancel(context.Background())
	dm, err := server.New(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), wiring.Full)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	for _, m := range dm.Modules() {
		if s, ok := m.(*tailnet.Service); ok {
			if tn == nil {
				tn = tailnettest.New()
				id = tailnettest.Identity{NodeID: "nNONE-" + machine}
			}
			s.NewNode = tn.NodeFunc(id)
		}
	}
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Errorf("daemon %s did not stop", machine)
			}
		})
	}
	t.Cleanup(stop)
	d := &daemon{dm: dm, cfg: cfg, owner: client.WithHandler(dm.SocketHandler()), stop: stop}
	sock := client.NewUnix(cfg.SocketPath())
	for i := 0; ; i++ {
		if _, err := sock.Health(ctx); err == nil {
			break
		}
		if i > 400 {
			t.Fatalf("%s: socket never answered", machine)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if id.Login != "" {
		if st, err := d.owner.TailnetUp(ctx, api.TailnetUpRequest{}); err != nil || !st.Running {
			t.Fatalf("%s: tailnet up: %+v %v", machine, st, err)
		}
	}
	return d
}

// ownKey is d's seal key id and public key, as its tunnel module has them.
func (d *daemon) ownKey(t *testing.T) (string, string) {
	t.Helper()
	for _, m := range d.dm.Modules() {
		if s, ok := m.(*tunnel.Service); ok {
			idB64, cert, err := s.SealIdentity()
			if err != nil {
				t.Fatal(err)
			}
			id, pub, ok := sealbox.Peer(idB64, cert)
			if !ok {
				t.Fatal("own certificate does not verify")
			}
			return id, secretkeys.Fingerprint(id, pub)
		}
	}
	t.Fatal("no tunnel module")
	return "", ""
}

func at(c *client.Client, project string) *client.Client {
	x := *c
	x.Project, x.ProjectFrom = project, "flag"
	return &x
}

func code(err error) string {
	if e, ok := err.(*api.Error); ok {
		return e.Code
	}
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestAuthorityOwnerPublishesListsRevokes(t *testing.T) {
	ctx := context.Background()
	a := start(t, nil, "mini", tailnettest.Identity{})
	must[api.Project](t)(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))
	own := at(a.owner, "harbor")
	keyID, fp := a.ownKey(t)

	k := must[orchapi.SecretKey](t)(orchclient.PublishSecretKey(ctx, own))
	if k.Machine != "mini" || k.KeyID != keyID || k.Fingerprint != fp || k.State != orchapi.SecretKeyActive || k.Project != "harbor" {
		t.Fatalf("published %+v (want key %s fp %s)", k, keyID, fp)
	}
	again := must[orchapi.SecretKey](t)(orchclient.PublishSecretKey(ctx, own))
	if again.Published != k.Published {
		t.Fatalf("a second publish changed the row: %+v", again)
	}
	l := must[[]orchapi.SecretKey](t)(orchclient.SecretKeys(ctx, own, "machine:mini", false))
	if len(l) != 1 || l[0].Fingerprint != fp {
		t.Fatalf("list: %+v", l)
	}
	if l := must[[]orchapi.SecretKey](t)(orchclient.SecretKeys(ctx, own, "machine:dock", false)); len(l) != 0 {
		t.Fatalf("another machine's list: %+v", l)
	}
	if _, err := orchclient.SecretKeys(ctx, own, "dock", false); code(err) != api.CodeBadRequest {
		t.Fatalf("a bare to: %v", err)
	}

	// The same machine's key in another project is that project's row.
	must[api.Project](t)(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "dockyard"}))
	if k2 := must[orchapi.SecretKey](t)(orchclient.PublishSecretKey(ctx, at(a.owner, "dockyard"))); k2.Fingerprint != fp || k2.Project != "dockyard" {
		t.Fatalf("dockyard: %+v", k2)
	}
	if l := must[[]orchapi.SecretKey](t)(orchclient.SecretKeys(ctx, a.owner, "", false)); len(l) != 2 {
		t.Fatalf("both projects: %+v", l)
	}
	// A minted token is no machine: it cannot publish, even as an owner.
	tok := must[api.MintTokenResponse](t)(a.owner.MintToken(ctx, api.MintTokenRequest{As: "vida", Project: "harbor", Role: "owner"}))
	net := client.WithHandler(a.dm.Handler)
	net.Token = tok.Secret
	if _, err := orchclient.PublishSecretKey(ctx, at(net, "harbor")); code(err) != api.CodeForbidden {
		t.Fatalf("a minted token published: %v", err)
	}
	// Unknown and malformed fingerprints are not_found alike.
	for _, bad := range []string{"aaaaaaaaaaaaaaaaaaaaaaaaaa", "nope"} {
		if _, err := orchclient.RevokeSecretKey(ctx, own, bad); code(err) != api.CodeNotFound {
			t.Fatalf("revoke %s: %v", bad, err)
		}
	}

	r := must[orchapi.SecretKey](t)(orchclient.RevokeSecretKey(ctx, own, fp))
	if r.State != orchapi.SecretKeyRevoked || r.Ended == nil || r.EndedBy == "" {
		t.Fatalf("revoked: %+v", r)
	}
	if l := must[[]orchapi.SecretKey](t)(orchclient.SecretKeys(ctx, own, "", false)); len(l) != 0 {
		t.Fatalf("a revoked key is listed as active: %+v", l)
	}
	if l := must[[]orchapi.SecretKey](t)(orchclient.SecretKeys(ctx, own, "", true)); len(l) != 1 {
		t.Fatalf("all=1: %+v", l)
	}
	if _, err := orchclient.PublishSecretKey(ctx, own); code(err) != api.CodeConflict {
		t.Fatalf("a revoked key came back: %v", err)
	}
}

func TestMemberPublishesTheKeyItsTransportCertified(t *testing.T) {
	ctx := context.Background()
	tn := tailnettest.New()
	a := start(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"})
	must[api.Project](t)(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))
	must[api.ProjectModeResponse](t)(a.owner.ProjectTeam(ctx, "harbor"))
	join := func(machine, node, as, role string) *daemon {
		r := start(t, tn, machine, tailnettest.Identity{NodeID: node, NodeKey: "nodekey:" + machine, Login: as + "@example.com", Host: machine})
		inv := must[api.InviteResponse](t)(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: as, Role: role}))
		if j := must[api.JoinResponse](t)(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code})); j.Remote == nil {
			t.Fatalf("join: %+v", j)
		}
		return r
	}
	dock := join("dock", "nDOCK", "bea", "member")
	pier := join("pier", "nPIER", "cy", "member")
	dockKey, dockFP := dock.ownKey(t)

	k := must[orchapi.SecretKey](t)(orchclient.PublishSecretKey(ctx, at(dock.owner, "harbor")))
	if k.Machine != "dock" || k.KeyID != dockKey || k.Fingerprint != dockFP {
		t.Fatalf("dock published %+v (want %s %s)", k, dockKey, dockFP)
	}
	l := must[[]orchapi.SecretKey](t)(orchclient.SecretKeys(ctx, at(a.owner, "harbor"), "machine:dock", false))
	if len(l) != 1 || l[0].KeyID != dockKey || l[0].Owner != "human:bea" {
		t.Fatalf("the authority lists %+v", l)
	}
	// Another member sees it, and cannot revoke it; dock can.
	if l := must[[]orchapi.SecretKey](t)(orchclient.SecretKeys(ctx, at(pier.owner, "harbor"), "human:bea", false)); len(l) != 1 {
		t.Fatalf("pier lists %+v", l)
	}
	if _, err := orchclient.RevokeSecretKey(ctx, at(pier.owner, "harbor"), dockFP); code(err) != api.CodeForbidden {
		t.Fatalf("pier revoked dock's key: %v", err)
	}
	if r := must[orchapi.SecretKey](t)(orchclient.RevokeSecretKey(ctx, at(dock.owner, "harbor"), dockFP)); r.State != orchapi.SecretKeyRevoked {
		t.Fatalf("dock's revoke: %+v", r)
	}

	// pier makes a new seal key (its daemon restarts without one): the new
	// key replaces the old, and the old never comes back.
	pierID := tailnettest.Identity{NodeID: "nPIER", NodeKey: "nodekey:pier", Login: "cy@example.com", Host: "pier"}
	old := must[orchapi.SecretKey](t)(orchclient.PublishSecretKey(ctx, at(pier.owner, "harbor")))
	pier.stop()
	if err := os.Remove(filepath.Join(pier.cfg.Home, tunnel.SealKeyFile)); err != nil {
		t.Fatal(err)
	}
	pier = startIn(t, tn, "pier", pierID, pier.cfg.Home)
	_, newFP := pier.ownKey(t)
	var nk orchapi.SecretKey
	eventually(t, func() error {
		var err error
		nk, err = orchclient.PublishSecretKey(ctx, at(pier.owner, "harbor"))
		return err
	})
	if nk.Fingerprint != newFP || nk.Fingerprint == old.Fingerprint {
		t.Fatalf("pier's new key %+v (old %s, want %s)", nk, old.Fingerprint, newFP)
	}
	all := must[[]orchapi.SecretKey](t)(orchclient.SecretKeys(ctx, at(a.owner, "harbor"), "machine:pier", true))
	if len(all) != 2 || all[0].Fingerprint != newFP || all[0].State != orchapi.SecretKeyActive || all[1].State != orchapi.SecretKeyReplaced {
		t.Fatalf("pier's keys: %+v", all)
	}
	// An admin of the whole project revokes another machine's key.
	if r := must[orchapi.SecretKey](t)(orchclient.RevokeSecretKey(ctx, at(a.owner, "harbor"), newFP)); r.State != orchapi.SecretKeyRevoked {
		t.Fatalf("the admin's revoke: %+v", r)
	}
}

// eventually retries f for a few seconds (a restarted member reconnects).
func eventually(t *testing.T, f func() error) {
	t.Helper()
	var err error
	for i := 0; i < 300; i++ {
		if err = f(); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal(err)
}

func TestPins(t *testing.T) {
	p := secretkeys.OpenPins(newHome(t))
	key := func(project, machine, pub string) orchapi.SecretKey {
		id := "zmkey:" + strings.Repeat("a", 26)
		return orchapi.SecretKey{Project: project, Machine: machine, KeyID: id,
			SealKey: base64.RawURLEncoding.EncodeToString([]byte(pub)), Fingerprint: secretkeys.Fingerprint(id, []byte(pub))}
	}
	one, two := key("harbor", "dock", strings.Repeat("1", 32)), key("harbor", "dock", strings.Repeat("2", 32))
	if v, _, err := p.Check(one); err != nil || v != secretkeys.PinNew {
		t.Fatalf("first: %s %v", v, err)
	}
	if err := p.Trust(one, time.Now()); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := p.Check(one); v != secretkeys.PinPinned {
		t.Fatalf("after trust: %s", v)
	}
	if v, old, _ := p.Check(two); v != secretkeys.PinChanged || old.Fingerprint != one.Fingerprint {
		t.Fatalf("another key: %s %+v", v, old)
	}
	// Another project's dock is another machine.
	if v, _, _ := p.Check(key("dockyard", "dock", strings.Repeat("2", 32))); v != secretkeys.PinNew {
		t.Fatalf("another project's dock: %s", v)
	}
	st, err := os.Stat(p.Path())
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("pins file: %v %v", st, err)
	}
	// A listing whose fingerprint its key does not make is refused, so the
	// pin is always computed from the key.
	forged := two
	forged.Fingerprint = one.Fingerprint
	if _, _, err := p.Check(forged); err == nil {
		t.Fatal("a forged fingerprint checked")
	}
	if err := p.Trust(forged, time.Now()); err == nil {
		t.Fatal("a forged fingerprint was pinned")
	}
	// A broken file is an error, never "no pins" (that would trust every key anew).
	if err := os.WriteFile(p.Path(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Check(one); err == nil {
		t.Fatal("a broken pins file read as empty")
	}
}
