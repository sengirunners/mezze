package team_test

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/tunnel/tunneltest"
)

// The home's restore generation on the wire: a sync or a
// write batch that names another generation is a resync, whatever the
// cursor (empty included); one that names none is an older replica's and
// unchecked; the snapshot's end line names it; a damaged generation file
// refuses member syncs rather than reading as "never restored".
func TestRestoreGenerationOnTheWire(t *testing.T) {
	quietPresence(t)
	a, p := tunnelHome(t, nil, "mini", tailnettest.Identity{})
	teamHarbor(t, a)
	r, j := tunnelMember(t, a, "bea-mac", "harbor", "bea", "member")
	fp := mustf(tunnelOf(t, a).Fingerprint())(t)
	tok := readToken(t, j.Remote.TokenFile)
	pid := j.Remote.Project.ID
	sync := func(q url.Values) (int, string) {
		t.Helper()
		q.Set("project", pid)
		return rawCall(t, signedClient(t, r, fp), "GET", p.URL()+"/v0/sync?"+q.Encode(), tok, nil, nil)
	}
	writes := func(gen *string) (int, string) {
		t.Helper()
		u := p.URL() + "/v0/team/writes"
		if gen != nil {
			u += "?gen=" + url.QueryEscape(*gen)
		}
		return rawCall(t, signedClient(t, r, fp), "POST", u, tok, api.WritesRequest{Project: pid, Writes: []api.QueuedWrite{}}, nil)
	}
	// The batch body carries no new field: an older home decodes it with
	// unknown fields refused, so the generation rides in the query.
	if b, _ := json.Marshal(api.WritesRequest{}); string(b) != `{"project":"","writes":null}` {
		t.Fatalf("a write batch's body changed shape (an older home would refuse it): %s", b)
	}
	snapshotGen := func() string {
		t.Helper()
		code, body := rawCall(t, signedClient(t, r, fp), "GET", p.URL()+"/v0/sync/snapshot?project="+pid, tok, nil, nil)
		if code != 200 {
			t.Fatalf("snapshot: %d %s", code, body)
		}
		lines := strings.Split(strings.TrimSpace(body), "\n")
		end := lines[len(lines)-1]
		if !strings.Contains(end, `"kind":"end"`) {
			t.Fatalf("snapshot has no end line: %s", end)
		}
		if i := strings.Index(end, `"generation":"`); i >= 0 {
			rest := end[i+len(`"generation":"`):]
			return rest[:strings.Index(rest, `"`)]
		}
		return ""
	}
	empty := ""
	stale := "0123456789abcdef0123456789abcdef"

	// Never restored: generation "".
	if g := snapshotGen(); g != "" {
		t.Fatalf("a never-restored home names generation %q", g)
	}
	for _, c := range []struct {
		q    url.Values
		want int
	}{
		{url.Values{}, 200},               // an older replica
		{url.Values{"gen": {""}}, 200},    // the right one
		{url.Values{"gen": {stale}}, 410}, // another
		{url.Values{"gen": {stale}, "since": {""}}, 410},
	} {
		if code, body := sync(c.q); code != c.want {
			t.Fatalf("sync %v before any restore: %d %s", c.q, code, body)
		}
	}

	// As zm backup restore: a new generation, then the daemon starts.
	gen := mustf(config.NewRestoreGeneration())(t)
	if err := config.WriteRestoreGeneration(a.cfg.Home, gen); err != nil {
		t.Fatal(err)
	}
	restartTunnelHome(t, a, p)
	if g := snapshotGen(); g != gen {
		t.Fatalf("snapshot names generation %q, want %q", g, gen)
	}
	for _, c := range []struct {
		q    url.Values
		want int
	}{
		{url.Values{"gen": {""}}, 410},                   // taken before the restore, empty cursor
		{url.Values{"gen": {""}, "since": {stale}}, 410}, // ... any cursor
		{url.Values{"gen": {gen}}, 200},
		{url.Values{}, 200}, // an older replica: unchecked, as before
	} {
		code, body := sync(c.q)
		if code != c.want {
			t.Fatalf("sync %v after a restore: %d %s", c.q, code, body)
		}
		if code == 410 && !strings.Contains(body, "restored from a backup") {
			t.Fatalf("410 does not say why: %s", body)
		}
	}
	if code, body := writes(&empty); code != 410 {
		t.Fatalf("a write batch under the old generation: %d %s", code, body)
	}
	if code, body := writes(&gen); code != 200 {
		t.Fatalf("a write batch under the current generation: %d %s", code, body)
	}
	if code, body := writes(nil); code != 200 {
		t.Fatalf("an older replica's write batch: %d %s", code, body)
	}
	// The member's own copy follows the restore on its own.
	eventually(t, 10*time.Second, func() error {
		if c := teamService(t, r).CopyRestore("harbor"); c != gen {
			return fmt.Errorf("copy generation %q", c)
		}
		return nil
	})

	// A damaged generation is refused, never read as "never restored".
	if err := os.WriteFile(filepath.Join(a.cfg.Home, config.FileRestored), []byte("garbage\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restartTunnelHome(t, a, p)
	if code, body := sync(url.Values{"gen": {""}}); code != 503 {
		t.Fatalf("sync with a damaged generation file: %d %s", code, body)
	}
	if code, body := rawCall(t, signedClient(t, r, fp), "GET", p.URL()+"/v0/sync/snapshot?project="+pid, tok, nil, nil); code != 503 {
		t.Fatalf("snapshot with a damaged generation file: %d %s", code, body)
	}
}

// restartTunnelHome restarts a's daemon on its home and points p at its
// new listener.
func restartTunnelHome(t *testing.T, a *daemon, p *tunneltest.Proxy) {
	t.Helper()
	a.restart(nil, tailnettest.Identity{})
	eventually(t, 5*time.Second, func() error {
		st := tunnelOf(t, a).Status()
		if !st.Running {
			return fmt.Errorf("tunnel listener not up")
		}
		p.SetTarget(st.Listen)
		return nil
	})
}
