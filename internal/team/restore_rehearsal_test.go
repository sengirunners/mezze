package team_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/homes/bkcrypt"
	"github.com/sengirunners/mezze/internal/homes/offsite"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/tunnel"
	"github.com/sengirunners/potluck/kit/tunnel/tunneltest"
)

// restoreBar is the plan's pass bar for a restored home: the first member write
// lands, and the member drops what the home lost, within 60 s of start.
const restoreBar = 60 * time.Second

// rehearsal is the restore rehearsal: a home
// and a member joined over the tunnel, a set to file://, a member write
// after the set, the home stopped, its disk gone, restored from the set.
type rehearsal struct {
	a       *daemon
	p       *tunneltest.Proxy
	r       *daemon
	sess    orchapi.Session
	kept    orchapi.WorkItem // written before the set
	lost    orchapi.WorkItem // written by the member after the set
	dead    string           // the lost disk (kept aside to read its key)
	started time.Time        // the restored daemon's start
}

// rehearseRestore runs the rehearsal up to the restored home's start.
// setup, when given, runs on the live home before the set is taken. carry
// runs between the restore and the start, with the lost disk and the
// restored home (what the owner does by hand, if anything).
func rehearseRestore(t *testing.T, setup func(a *daemon), carry func(dead, home string)) *rehearsal {
	t.Helper()
	quietPresence(t)
	ctx := context.Background()
	a, p := tunnelHome(t, nil, "mini", tailnettest.Identity{})
	teamHarbor(t, a)
	machine, human := a.cfg.Machine, a.cfg.Human
	r, _ := tunnelMember(t, a, "bea-mac", "harbor", "bea", "member")
	rc := at(r.owner, "harbor", "")
	x := &rehearsal{a: a, p: p, r: r}
	x.sess = register(t, r, "harbor/solver", "bea-rehearsal")
	x.kept = mustf(orchclient.CreateWork(ctx, rc, orchapi.CreateWorkRequest{Title: "before the set", Area: "solver"}))(t)

	if err := a.cfg.Save(); err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(a)
	}
	root := t.TempDir()
	const homeID = "rehearsal-home"
	mustf(orchclient.SetBackup(ctx, a.owner, orchapi.OffsiteRequest{Target: "file://" + root, Home: homeID, EverySeconds: 3600}))(t)
	set := mustf(orchclient.RunBackup(ctx, a.owner))(t)
	if !set.Ran || set.Set == "" {
		t.Fatalf("backup: %+v", set)
	}
	x.lost = mustf(orchclient.CreateWork(ctx, rc, orchapi.CreateWorkRequest{Title: "after the set", Area: "solver"}))(t)
	eventually(t, 10*time.Second, func() error {
		if _, err := orchclient.GetWork(ctx, at(a.owner, "harbor", ""), x.lost.ID); err != nil {
			return fmt.Errorf("the home has not taken the member's write: %v", err)
		}
		if !teamService(t, r).CopyHolds(ctx, x.lost.ID) {
			return fmt.Errorf("the member's copy does not list its write")
		}
		return nil
	})

	// The home stops; its disk is gone.
	a.halt()
	a.halt = func() {}
	home := a.cfg.Home
	x.dead = home + ".lost"
	if err := os.Rename(home, x.dead); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(x.dead) })
	key, err := bkcrypt.Open(x.dead) // the owner kept a copy of the home's backup key
	if err != nil {
		t.Fatal(err)
	}
	if _, err := offsite.Restore(ctx, offsite.Dir{Root: root}, homeID, set.Set, home, false, key); err != nil {
		t.Fatal(err)
	}
	if carry != nil {
		carry(x.dead, home)
	}
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Machine, cfg.Human = machine, human // as startDaemonWith: never the environment's
	a.cfg = cfg
	x.started = time.Now()
	a.boot(nil, tailnettest.Identity{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var st api.TunnelStatus
	eventually(t, 5*time.Second, func() error {
		if st = tunnelOf(t, a).Status(); !st.Running {
			return fmt.Errorf("restored tunnel listener not up: %+v", st)
		}
		return nil
	})
	p.SetTarget(st.Listen)
	return x
}

// carryKeyFile copies the lost disk's home key into the restored home: the
// owner carrying it by hand, which a later step replaces.
func carryKeyFile(t *testing.T) func(dead, home string) {
	return func(dead, home string) {
		b, err := os.ReadFile(filepath.Join(dead, tunnel.KeyFile))
		if err != nil {
			t.Fatal(err)
		}
		to := filepath.Join(home, tunnel.KeyFile)
		if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// With the key carried, the restored home has no replay
// journal. Its first member write must land within the bar, not after the
// 11-minute replay hold.
func TestRestoreRehearsalFirstMemberWrite(t *testing.T) {
	x := rehearseRestore(t, nil, carryKeyFile(t))
	landed := firstMemberWrite(t, x)
	// The restored home started without its replay journal (a set never
	// carries it), so it is in its replay hold. That the hold refuses a
	// write without the signed recovery challenge is held by
	// potluck/kit/tunnel/recovery_challenge_test.go and
	// internal/team/recovery_challenge_test.go, not by this test.
	if _, err := os.Stat(filepath.Join(x.a.cfg.Home, tunnel.NonceFile+".hold")); err != nil {
		t.Fatalf("the restored home is not in its replay hold, so this did not rehearse it: %v", err)
	}
	t.Logf("first member write landed %s after the restored home started", landed.Round(time.Millisecond))
}

// The home key is never in a set. With the key escrowed outside
// the home (tunnel.key, a secret reference), the restored home starts with
// the same key and no step by the owner, so the member takes its answers.
func TestRestoreRehearsalMemberAcceptsRestoredHome(t *testing.T) {
	var fp string
	escrow := filepath.Join(t.TempDir(), "home-key")
	x := rehearseRestore(t, func(a *daemon) {
		fp = mustf(tunnelOf(t, a).Fingerprint())(t)
		b, err := os.ReadFile(filepath.Join(a.cfg.Home, tunnel.KeyFile))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(escrow, b, 0o600); err != nil {
			t.Fatal(err)
		}
		setTunnelKeyRef(t, a, "file:"+escrow)
	}, nil)
	if got, err := tunnelOf(t, x.a).Fingerprint(); err != nil || got != fp {
		t.Fatalf("the restored home's key: %q %v, want the escrowed %q", got, err, fp)
	}
	landed := firstMemberWrite(t, x)
	t.Logf("first member write landed %s after the restored home started (no step by the owner)", landed.Round(time.Millisecond))
}

// The whole rehearsal, the owner's one step taken
// through the real route before the disaster and nothing after: the
// restored home keeps its key, the member drops what the home lost and
// refuses writes on it, and its first write lands, each within the bar.
func TestRestoreRehearsalEndToEnd(t *testing.T) {
	ctx := context.Background()
	var fp string
	escrow := filepath.Join(t.TempDir(), "home-key")
	x := rehearseRestore(t, func(a *daemon) {
		fp = mustf(tunnelOf(t, a).Fingerprint())(t)
		b, err := os.ReadFile(filepath.Join(a.cfg.Home, tunnel.KeyFile))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(escrow, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if st := mustf(a.owner.TunnelKey(ctx, "file:"+escrow))(t); st.KeyEscrow != "file:"+escrow {
			t.Fatalf("escrow: %+v", st)
		}
	}, nil)
	if got, err := tunnelOf(t, x.a).Fingerprint(); err != nil || got != fp {
		t.Fatalf("the restored home's key: %q %v, want %q", got, err, fp)
	}
	rc := at(x.r.owner, "harbor", "")
	var ghostGone time.Duration
	for time.Since(x.started) < restoreBar {
		wl, err := orchclient.WorkList(ctx, rc, orchapi.WorkQuery{})
		if err == nil && hasItem(wl.Items, x.kept.ID) && !hasItem(wl.Items, x.lost.ID) {
			ghostGone = time.Since(x.started)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if ghostGone == 0 {
		t.Fatalf("the member still lists the item the restored home lost %s after its start", restoreBar)
	}
	if _, err := orchclient.Comment(ctx, rc, x.lost.ID, orchapi.CommentRequest{Body: "on a lost item"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("a comment on the lost item: %v (want not found)", err)
	}
	landed := firstMemberWrite(t, x)
	t.Logf("rehearsal: key kept (no owner step after the disaster); ghost gone at %s; first member write at %s (bar %s)",
		ghostGone.Round(time.Millisecond), landed.Round(time.Millisecond), restoreBar)
}

// setTunnelKeyRef names ref as a's home key escrow in its config.json, as
// the owner's one step does.
func setTunnelKeyRef(t *testing.T, a *daemon, ref string) {
	t.Helper()
	path := filepath.Join(a.cfg.Home, config.FileConfig)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	tn, _ := m["tunnel"].(map[string]any)
	if tn == nil {
		tn = map[string]any{}
	}
	tn["key"] = ref
	m["tunnel"] = tn
	b, _ = json.Marshal(m)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// firstMemberWrite sends mail from the member's session until it lands at
// the restored home, failing past restoreBar; it returns the time from the
// restored home's start.
func firstMemberWrite(t *testing.T, x *rehearsal) time.Duration {
	t.Helper()
	ctx := context.Background()
	body := "first write after the restore"
	var landed time.Duration
	deadline := x.started.Add(restoreBar)
	sent := false
	var lastErr error
	for time.Now().Before(deadline) {
		if !sent {
			_, lastErr = orchclient.Send(ctx, at(x.r.owner, "harbor", x.sess.Address), orchapi.SendMailRequest{Kind: orchapi.MailKindReport, To: "human:mini-human", Body: body})
			sent = lastErr == nil
		}
		if sent && hasMailBody(t, x.a, body) {
			landed = time.Since(x.started)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if landed == 0 {
		t.Fatalf("no member write landed at the restored home within %s of its start (sent %v, last send error: %v)", restoreBar, sent, lastErr)
	}
	return landed
}

func hasMailBody(t *testing.T, a *daemon, body string) bool {
	t.Helper()
	ms, err := orchclient.Mail(context.Background(), at(a.owner, "harbor", ""), orchapi.MailQuery{})
	if err != nil {
		return false
	}
	for _, m := range ms {
		if m.Body == body {
			return true
		}
	}
	return false
}
