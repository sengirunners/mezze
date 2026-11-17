package homes

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	standardwebhooks "github.com/standard-webhooks/standard-webhooks/libraries/go"

	"github.com/sengirunners/mezze/internal/homes/bkcrypt"
	"github.com/sengirunners/mezze/internal/homes/offsite"
	"github.com/sengirunners/mezze/internal/homes/offsite/offsitetest"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/mezze/internal/tools"
	"github.com/sengirunners/mezze/internal/tools/toolstest"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/project"
)

// marker writes a row into harbor's project.db, so a restore can be proven
// to carry that file's content.
func (h *nativeHarness) marker(text string) {
	h.t.Helper()
	db, err := h.d.Store.Project(authz.All(), authz.Write, h.harbor.ID)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := db.ExecContext(h.ctx, `CREATE TABLE IF NOT EXISTS zz_marker (t TEXT); INSERT INTO zz_marker VALUES (?)`, text); err != nil {
		h.t.Fatal(err)
	}
}

// backupKey is the key the home's backups are encrypted with.
func (h *nativeHarness) backupKey() *bkcrypt.Key {
	h.t.Helper()
	k, err := bkcrypt.Open(h.d.Config.Home)
	if err != nil {
		h.t.Fatal(err)
	}
	return k
}

func (h *nativeHarness) setOffsite(target string, extra func(*orchapi.OffsiteRequest)) {
	h.t.Helper()
	req := orchapi.OffsiteRequest{Target: target, Home: "test-home"}
	if extra != nil {
		extra(&req)
	}
	if _, err := orchclient.SetBackup(h.ctx, h.as(h.owner, ""), req); err != nil {
		h.t.Fatal(err)
	}
}

// restoreAndOpen restores the newest set into a fresh directory and opens
// it as zm would: the store migrates, the registry knows both projects, and
// harbor's project.db has every marker.
func restoreAndOpen(t *testing.T, tg offsite.Target, key *bkcrypt.Key, markers ...string) string {
	t.Helper()
	ctx := context.Background()
	sets, err := offsite.Sets(ctx, tg, "test-home")
	if err != nil {
		t.Fatal(err)
	}
	stamp, err := offsite.Pick(sets, "")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "restored")
	m, err := offsite.Restore(ctx, tg, "test-home", stamp, dst, false, key)
	if err != nil {
		t.Fatal(err)
	}
	if m.Cipher != bkcrypt.Cipher || m.KeyID != key.ID() {
		t.Fatalf("set %s is not encrypted with the home's key: %q %q", stamp, m.Cipher, m.KeyID)
	}
	for _, f := range m.Files {
		if strings.HasPrefix(f.Path, "secrets/") || strings.HasSuffix(f.Path, ".key") || strings.HasSuffix(f.Path, ".token") {
			t.Fatalf("a secret went off-site: %s", f.Path)
		}
	}
	st, err := storetest.Open(ctx, dst)
	if err != nil {
		t.Fatalf("the restored home does not open: %v", err)
	}
	defer st.Close()
	cfg, err := config.LoadFrom(dst)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := project.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	var slugs []string
	for _, p := range st.Projects(authz.All(), authz.Read) {
		if f, ok := reg.File(p.ID); ok {
			slugs = append(slugs, f.Slug)
			if f.Slug != "harbor" {
				continue
			}
			db, _ := st.Project(authz.All(), authz.Read, p.ID)
			for _, mk := range markers {
				var n int
				if err := db.QueryRowContext(ctx, `SELECT count(*) FROM zz_marker WHERE t = ?`, mk).Scan(&n); err != nil || n != 1 {
					t.Fatalf("marker %q in the restored project.db: %d, %v", mk, n, err)
				}
			}
			if _, err := os.Stat(filepath.Join(st.ProjectDir(p.ID), nativeStoreFile)); err != nil {
				t.Fatalf("no issue store restored: %v", err)
			}
		}
	}
	if len(slugs) != 2 {
		t.Fatalf("restored projects %v, want harbor and side", slugs)
	}
	return dst
}

// The whole home goes to a file target and comes back into a fresh home;
// a second set copies what did not change; retention runs.
func TestOffsiteRoundTripFile(t *testing.T) {
	h := newNativeHarness(t)
	// Noon: the runs below span hours, and one that crosses UTC midnight rightly
	// keeps another daily set, which made this fail every evening.
	now := h.clock.now()
	h.clock.add(time.Date(now.Year(), now.Month(), now.Day()+1, 12, 0, 0, 0, time.UTC).Sub(now))
	root := t.TempDir()
	h.marker("first")
	h.setOffsite("file://"+root, func(r *orchapi.OffsiteRequest) { r.Keep, r.KeepHourly, r.KeepDaily = 2, 1, 1 })
	r1, err := h.s.runOffsite(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !r1.Ran || r1.Files < 6 || r1.Copied != 0 {
		t.Fatalf("first run %+v", r1)
	}
	h.clock.add(time.Minute)
	h.marker("second")
	r2, err := h.s.runOffsite(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Copied == 0 || r2.Uploaded == 0 {
		t.Fatalf("a second run sent everything again or nothing: %+v", r2)
	}
	b := h.s.HomeBackup()
	if b == nil || b.LastOK == nil || !b.LastOK.Equal(h.clock.now().UTC()) || b.Bytes != r2.Bytes || b.Failing || b.Kind != "file" {
		t.Fatalf("HomeBackup %+v", b)
	}
	restoreAndOpen(t, offsite.Dir{Root: root}, h.backupKey(), "first", "second")

	// Keep 2 newest, 1 hourly, 1 daily: a run an hour on, then another, leaves 2.
	for range 3 {
		h.clock.add(time.Hour)
		if _, err := h.s.runOffsite(h.ctx); err != nil {
			t.Fatal(err)
		}
	}
	sets, _ := offsite.Sets(h.ctx, offsite.Dir{Root: root}, "test-home")
	if len(sets) != 2 {
		t.Fatalf("retention left %d sets", len(sets))
	}
}

func TestOffsiteConfigSanitizesTunnelTrust(t *testing.T) {
	h := newNativeHarness(t)
	const secret = "PRIVATE-edge-secret-not-for-backups"
	live := []byte(`{
  "human": "ana", "machine": "mini", "default_project": "harbor",
  "github": {"lookup_token": "env:ZM_GITHUB_LOOKUP"},
  "future": {"enabled": true, "counter": 900719925474099312345},
  "tunnel": {"listen": "127.0.0.1:7435", "url": "https://team.example",
    "edge_secret": "` + secret + `", "future_timeout": 900719925474099312345}
}`)
	path := filepath.Join(h.s.home(), "config.json")
	if err := os.WriteFile(path, live, 0o600); err != nil {
		t.Fatal(err)
	}
	check := func(path string) {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte(secret)) {
			t.Fatal("inline tunnel secret reached the backup copy")
		}
		var got struct {
			DefaultProject string                     `json:"default_project"`
			GitHub         map[string]json.RawMessage `json:"github"`
			Future         map[string]json.RawMessage `json:"future"`
			Tunnel         map[string]json.RawMessage `json:"tunnel"`
		}
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		if _, ok := got.Tunnel["edge_secret"]; ok {
			t.Error("tunnel edge secret field was retained")
		}
		if _, ok := got.Tunnel["listen"]; ok {
			t.Error("listener whose edge trust was removed remains enabled")
		}
		if got.DefaultProject != "harbor" || string(got.GitHub["lookup_token"]) != `"env:ZM_GITHUB_LOOKUP"` ||
			string(got.Tunnel["url"]) != `"https://team.example"` || string(got.Future["enabled"]) != "true" ||
			string(got.Future["counter"]) != "900719925474099312345" || string(got.Tunnel["future_timeout"]) != "900719925474099312345" {
			t.Fatal("ordinary settings, unknown fields or secret references changed")
		}
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("backup config is not mode 0600: %v, %v", st, err)
		}
	}
	stage := t.TempDir()
	if _, err := h.s.stageHome(h.ctx, stage, nil); err != nil {
		t.Fatal(err)
	}
	check(filepath.Join(stage, "config.json"))
	root := t.TempDir()
	h.setOffsite("file://"+root, nil)
	if _, err := h.s.runOffsite(h.ctx); err != nil {
		t.Fatal(err)
	}
	restored := restoreAndOpen(t, offsite.Dir{Root: root}, h.backupKey())
	check(filepath.Join(restored, "config.json"))
	if b, err := os.ReadFile(path); err != nil || !bytes.Equal(b, live) {
		t.Fatalf("backup changed the live configuration: %v", err)
	}
}

func TestOffsiteConfigWithoutEdgeSecret(t *testing.T) {
	for name, suffix := range map[string]string{"absent": "", "empty": `,"edge_secret":""`, "null": `,"edge_secret":null`} {
		t.Run(name, func(t *testing.T) {
			h := newNativeHarness(t)
			path := filepath.Join(h.s.home(), "config.json")
			live := []byte(`{"tunnel":{"listen":"127.0.0.1:7435","url":"https://team.example"` + suffix + `}}`)
			if err := os.WriteFile(path, live, 0o600); err != nil {
				t.Fatal(err)
			}
			stage := t.TempDir()
			if _, err := h.s.stageHome(h.ctx, stage, nil); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadFrom(stage)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.File.Tunnel.Listen != "127.0.0.1:7435" || cfg.File.Tunnel.URL != "https://team.example" || cfg.File.Tunnel.EdgeSecret != "" {
				t.Fatal("listener without an edge secret changed")
			}
		})
	}
}

func TestOffsiteConfigTunnelAliases(t *testing.T) {
	h := newNativeHarness(t)
	const secret = "PRIVATE-config-alias-sentinel"
	live := []byte(`{"Tunnel":{"Edge_Secret":"` + secret + `","URL":"https://team.example","Listen":"127.0.0.1:7435"}}`)
	if err := os.WriteFile(filepath.Join(h.s.home(), "config.json"), live, 0o600); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	if _, err := h.s.stageHome(h.ctx, stage, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(stage, "config.json"))
	if err != nil || bytes.Contains(b, []byte(secret)) {
		t.Fatalf("case-insensitive tunnel secret was not sanitized: %v", err)
	}
	cfg, err := config.LoadFrom(stage)
	if err != nil || cfg.File.Tunnel.Listen != "" || cfg.File.Tunnel.EdgeSecret != "" || cfg.File.Tunnel.URL != "https://team.example" {
		t.Fatalf("case-insensitive tunnel settings lost sanitation or URL: %v", err)
	}
}

func TestOffsiteConfigMissing(t *testing.T) {
	h := newNativeHarness(t)
	path := filepath.Join(h.s.home(), "config.json")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	stage := t.TempDir()
	files, err := h.s.stageHome(h.ctx, stage, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Path == "config.json" {
			t.Fatal("missing config was added to the set")
		}
	}
	if _, err := os.Stat(filepath.Join(stage, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("missing config was staged: %v", err)
	}
}

func TestOffsiteConfigInvalidDoesNotDiscloseContents(t *testing.T) {
	const secret = "PRIVATE-config-error-sentinel"
	for name, live := range map[string]string{
		"malformed":        `{"tunnel":{"edge_secret":"` + secret + `"},?`,
		"array":            `["` + secret + `"]`,
		"string":           `"` + secret + `"`,
		"null":             `null`,
		"tunnel":           `{"tunnel":["` + secret + `"]}`,
		"secret":           `{"tunnel":{"edge_secret":{"value":"` + secret + `"}}}`,
		"duplicate_tunnel": `{"tunnel":{"edge_secret":"` + secret + `"},"tunnel":{"listen":"127.0.0.1:7435"}}`,
		"tunnel_aliases":   `{"Tunnel":{"Edge_Secret":"` + secret + `"},"TUNNEL":{"Listen":"127.0.0.1:7435"}}`,
		"duplicate_secret": `{"tunnel":{"edge_secret":"` + secret + `","edge_secret":null,"listen":"127.0.0.1:7435"}}`,
		"secret_aliases":   `{"tunnel":{"edge_secret":"` + secret + `","EDGE_SECRET":null,"listen":"127.0.0.1:7435"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newNativeHarness(t)
			root := t.TempDir()
			h.setOffsite("file://"+root, nil)
			path := filepath.Join(h.s.home(), "config.json")
			if err := os.WriteFile(path, []byte(live), 0o600); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			h.d.Log = slog.New(slog.NewTextHandler(&logs, nil))
			_, err := h.s.runOffsite(h.ctx)
			if err == nil {
				t.Fatal("invalid config was backed up")
			}
			if strings.Contains(err.Error()+logs.String()+h.s.HomeBackup().Error, secret) {
				t.Fatal("config contents appeared in backup error or log")
			}
			sets, err := offsite.Sets(h.ctx, offsite.Dir{Root: root}, "test-home")
			if err != nil || len(sets) != 0 {
				t.Fatalf("invalid config published a backup set: %d, %v", len(sets), err)
			}
			if b, err := os.ReadFile(path); err != nil || string(b) != live {
				t.Fatalf("backup changed invalid live config: %v", err)
			}
		})
	}
}

// A failing target is recorded (and shows in HomeBackup), retried after 10
// minutes, and a redaction removes every older set once a fresh one is in.
func TestOffsiteFailuresAndRedaction(t *testing.T) {
	h := newNativeHarness(t)
	root := filepath.Join(t.TempDir(), "volume")
	os.MkdirAll(root, 0o700)
	h.setOffsite("file://"+root, nil)
	for range 2 {
		if _, err := h.s.runOffsite(h.ctx); err != nil {
			t.Fatal(err)
		}
		h.clock.add(time.Minute)
	}
	if err := os.Rename(root, root+".away"); err != nil {
		t.Fatal(err)
	}
	if err := h.s.PurgeBackups(h.ctx, h.harbor.ID, true); err == nil {
		t.Fatal("a redaction whose off-site sets cannot be replaced said it was done")
	}
	st, _ := h.s.loadOffsite()
	if st.Backup.ScrubDue == nil || st.Backup.Failures != 1 {
		t.Fatalf("after a failed purge: %+v", st.Backup)
	}
	if b := h.s.HomeBackup(); !b.Failing || b.Error == "" {
		t.Fatalf("HomeBackup after a failure: %+v", b)
	}
	if h.s.offsiteDue(st.Backup) {
		t.Fatal("a failing backup is due again at once")
	}
	h.clock.add(retryAfterFailure)
	if !h.s.offsiteDue(st.Backup) {
		t.Fatal("a failing backup is not retried after 10 minutes")
	}
	os.Rename(root+".away", root)
	if err := h.s.PurgeBackups(h.ctx, h.harbor.ID, false); err != nil {
		t.Fatal(err)
	}
	st, _ = h.s.loadOffsite()
	if st.Backup.ScrubDue != nil || st.Backup.Failures != 0 {
		t.Fatalf("after the purge: %+v", st.Backup)
	}
	if sets, _ := offsite.Sets(h.ctx, offsite.Dir{Root: root}, "test-home"); len(sets) != 1 {
		t.Fatalf("a redaction left %d sets, want only the fresh one", len(sets))
	}
}

// Setting the backup makes the home's key (so it can be copied off the
// machine at once); the sets never hold it or project.db's secret names in
// the clear; and the plain sets an older zm wrote go once an encrypted set is
// sent, while a failure to delete them keeps the run failing.
func TestOffsiteSetsAreEncrypted(t *testing.T) {
	h := newNativeHarness(t)
	root := t.TempDir()
	const secretName = "ZZ_GRANTED_SECRET_NAME_FOR_THE_TEST"
	h.marker(secretName)
	// An older zm's plain set, recorded as the last good one.
	h.setOffsite("file://"+root, nil)
	if _, err := os.Stat(bkcrypt.KeyPath(h.d.Config.Home)); err != nil {
		t.Fatalf("setting the backup made no key: %v", err)
	}
	stage := t.TempDir()
	files, err := h.s.stageHome(h.ctx, stage, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := h.clock.now().UTC().Add(-time.Hour)
	plain, err := offsite.Upload(h.ctx, offsite.Dir{Root: root}, offsite.Manifest{Home: "test-home", Stamp: old.Format(offsite.StampLayout), Created: old}, files, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	h.s.updateOffsite(func(st *offsiteState) error { st.Backup.Last, st.Backup.LastOK = &plain, &old; return nil })
	// The plain set cannot be deleted: the run fails, and the next retries.
	if os.Getuid() != 0 { // root deletes it anyway
		setDir := filepath.Join(root, "test-home", plain.Stamp)
		os.Chmod(setDir, 0o500)
		_, err := h.s.runOffsite(h.ctx)
		os.Chmod(setDir, 0o700)
		if err == nil || !strings.Contains(err.Error(), "unencrypted") {
			t.Fatalf("a run that left plain sets: %v", err)
		}
		h.clock.add(retryAfterFailure)
	}
	r, err := h.s.runOffsite(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Copied != 0 {
		t.Fatalf("plain files were carried into the encrypted set: %+v", r)
	}
	sets, _ := offsite.Sets(h.ctx, offsite.Dir{Root: root}, "test-home")
	if len(sets) != 1 || sets[0].Stamp != r.Set {
		t.Fatalf("sets after the first encrypted one: %+v (sent %s)", sets, r.Set)
	}
	filepath.WalkDir(root, func(p string, de os.DirEntry, err error) error {
		if err != nil || de.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		if bytes.Contains(b, []byte(secretName)) || bytes.HasPrefix(b, []byte{0x1f, 0x8b}) {
			t.Errorf("%s is readable on the target", p)
		}
		if strings.Contains(p, "secrets") || strings.HasSuffix(p, ".key") {
			t.Errorf("%s went off-site", p)
		}
		return nil
	})
	restoreAndOpen(t, offsite.Dir{Root: root}, h.backupKey(), secretName)
}

func TestOffsiteRefusals(t *testing.T) {
	h := newNativeHarness(t)
	c := h.as(h.owner, "")
	for _, req := range []orchapi.OffsiteRequest{
		{Target: "file://" + t.TempDir(), Home: "Bad Home"},
		{Target: "file://" + filepath.Join(t.TempDir(), "missing"), Home: "h"},
		{Target: "s3://bucket/x", Home: "h", Endpoint: "https://e.example", AccessKey: "env:ZM_TEST_NO_SUCH", SecretKey: "env:ZM_TEST_NO_SUCH"},
		{Target: "file://" + t.TempDir(), Home: "h", EverySeconds: 5},
	} {
		if _, err := orchclient.SetBackup(h.ctx, c, req); err == nil {
			t.Errorf("SetBackup(%+v) accepted", req)
		}
	}
	if v, _ := orchclient.Backup(h.ctx, c); v.Configured {
		t.Fatal("a refused setting was saved")
	}
}

// The heartbeat: signed per Standard Webhooks with the secret the reference
// names, carrying the home, sync and backup status and nothing else.
func TestHeartbeat(t *testing.T) {
	h := newNativeHarness(t)
	secret := "whsec_" + "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	var (
		mu   sync.Mutex
		got  []map[string]any
		code = http.StatusOK
	)
	wh, _ := standardwebhooks.NewWebhook(secret)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if err := wh.Verify(body, r.Header); err != nil {
			http.Error(w, "bad signature", http.StatusUnauthorized)
			return
		}
		var m map[string]any
		json.Unmarshal(body, &m)
		mu.Lock()
		got = append(got, m)
		c := code
		mu.Unlock()
		w.WriteHeader(c)
	}))
	defer srv.Close()
	c := h.as(h.owner, "")
	ref := offsitetest.SecretFile(t, secret)
	v, err := orchclient.SetWatchdog(h.ctx, c, orchapi.WatchdogRequest{URL: srv.URL + "/v1/heartbeat", Secret: ref, Home: "test-home"})
	if err != nil {
		t.Fatal(err)
	}
	if v.Watchdog == nil || v.Watchdog.LastStatus != 200 || v.Watchdog.LastError != "" {
		t.Fatalf("watchdog %+v", v.Watchdog)
	}
	mu.Lock()
	if len(got) != 1 {
		t.Fatalf("heartbeats %v", got)
	}
	hb := got[0]
	mu.Unlock()
	if hb["home"] != "test-home" || hb["v"] != 1.0 || hb["backup"] != nil {
		t.Fatalf("heartbeat %v", hb)
	}
	// Counts only: no project or tracker name leaves the home.
	if sy, _ := hb["sync"].(map[string]any); sy == nil || sy["failing"] != 0.0 || sy["sources"] != 1.0 {
		t.Fatalf("heartbeat sync %v", hb["sync"])
	}
	for k := range hb {
		if !map[string]bool{"v": true, "home": true, "sent": true, "version": true, "commit": true, "sync": true, "backup": true}[k] {
			t.Fatalf("heartbeat field %q", k)
		}
	}
	// With a backup set up, its status rides along; a refusal is recorded.
	root := t.TempDir()
	h.setOffsite("file://"+root, nil)
	if _, err := h.s.runOffsite(h.ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	code = http.StatusUnauthorized
	mu.Unlock()
	st, _ := h.s.loadOffsite()
	h.s.beat(h.ctx, st.Watchdog)
	mu.Lock()
	bk, _ := got[1]["backup"].(map[string]any)
	mu.Unlock()
	if bk == nil || bk["kind"] != "file" || bk["last_ok"] == nil {
		t.Fatalf("heartbeat backup %v", bk)
	}
	if _, ok := bk["error"]; ok {
		t.Fatalf("the heartbeat carries error text: %v", bk)
	}
	if v, _ := orchclient.Backup(h.ctx, c); v.Watchdog.Failures != 1 || v.Watchdog.LastStatus != 401 {
		t.Fatalf("a refused heartbeat: %+v", v.Watchdog)
	}
	if hb := h.s.Heartbeat(); hb == nil || !hb.Failing || hb.LastStatus != 401 || hb.Failures != 1 {
		t.Fatalf("health's heartbeat after a refusal: %+v", hb)
	}
	// A wrong secret reaches the watchdog as a bad signature, never the secret.
	if _, err := orchclient.SetWatchdog(h.ctx, c, orchapi.WatchdogRequest{URL: srv.URL, Secret: offsitetest.SecretFile(t, "not-a-whsec"), Home: "test-home"}); err == nil {
		t.Fatal("a secret that is not whsec_ was accepted")
	}
	if _, err := orchclient.SetWatchdog(h.ctx, c, orchapi.WatchdogRequest{URL: "http://example.com/x", Secret: ref, Home: "test-home"}); err == nil {
		t.Fatal("a plain-http watchdog off loopback was accepted")
	}
	if _, err := orchclient.WatchdogOff(h.ctx, c); err != nil {
		t.Fatal(err)
	}
}

// TestRealOffsiteS3RoundTrip is the whole path against a local S3 (rclone serve
// s3, started by the test and stopped by its saved process), encrypted: set
// through the route (which checks the target answers), a run, a second run,
// then a restore into a fresh home that opens. ZM_TEST_DOWNLOAD=1 fetches the
// pinned rclone.
func TestRealOffsiteS3RoundTrip(t *testing.T) {
	h := newNativeHarness(t)
	bin := offsitetest.Rclone(t, func() (string, error) {
		if err := toolstest.ProvisionTools(h.ctx, h.d.Config.Home, tools.Options{}, tools.Rclone); err != nil {
			return "", err
		}
		return tools.Verify(h.d.Config.Home, tools.Rclone)
	})
	if _, err := tools.Verify(h.d.Config.Home, tools.Rclone); err != nil {
		t.Skipf("ZM_TEST_RCLONE is not provisioned into the home: %v", err)
	}
	ak, sk := "zmaccess", "zmsecret-for-the-test"
	endpoint, dir := offsitetest.FakeS3(t, bin, ak, sk)
	cfg := offsite.Config{URL: "s3://zm-backups/homes", Endpoint: endpoint,
		AccessKey: offsitetest.SecretFile(t, ak), SecretKey: offsitetest.SecretFile(t, sk), Encrypt: offsitetest.SecretFile(t, "passphrase"), EncryptSalt: offsitetest.SecretFile(t, "salt")}
	h.marker("over s3")
	h.setOffsite(cfg.URL, func(r *orchapi.OffsiteRequest) {
		r.Endpoint, r.AccessKey, r.SecretKey, r.Encrypt, r.EncryptSalt, r.RunNow = cfg.Endpoint, cfg.AccessKey, cfg.SecretKey, cfg.Encrypt, cfg.EncryptSalt, true
	})
	h.clock.add(time.Minute)
	r, err := h.s.runOffsite(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r.Copied == 0 {
		t.Fatalf("no server-side copy on the second run: %+v", r)
	}
	if b := h.s.HomeBackup(); b == nil || b.Kind != "s3" || b.LastOK == nil || b.Failing {
		t.Fatalf("HomeBackup %+v", b)
	}
	sets, err := orchclient.BackupSets(h.ctx, h.as(h.owner, ""))
	if err != nil || len(sets.Sets) != 2 || !strings.Contains(sets.Target, "encrypted") {
		t.Fatalf("sets %+v, %v", sets, err)
	}
	tg, err := offsite.Opener{Bin: bin, Scratch: t.TempDir()}.Open(h.ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	restoreAndOpen(t, tg, h.backupKey(), "over s3")
	// The bucket holds no readable name or database.
	filepath.WalkDir(filepath.Join(dir, "zm-backups"), func(p string, de os.DirEntry, err error) error {
		if err == nil && !de.IsDir() && (strings.Contains(p, "registry") || strings.Contains(p, "test-home")) {
			t.Errorf("a plain name in the bucket: %s", p)
		}
		return err
	})
}

// TestOffsiteRegistryWithoutSecrets: the staged registry copy has
// every module's secrets blanked (core.BackupScrubber) and keeps no trace of
// them in free pages; the live registry is untouched.
func TestOffsiteRegistryWithoutSecrets(t *testing.T) {
	h := newHarness(t)
	const secret = "whsec_c3RhZ2VkLXNpZ25pbmcta2V5LWZvci10aGUtdGVzdA=="
	reg := h.d.Store.Registry()
	if _, err := reg.ExecContext(h.ctx, `CREATE TABLE test_hooks (id TEXT PRIMARY KEY, secret TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.ExecContext(h.ctx, `INSERT INTO test_hooks VALUES ('h1', ?)`, secret); err != nil {
		t.Fatal(err)
	}
	h.d.AddBackupScrubber(func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE test_hooks SET secret = ''`)
		return err
	})
	stage := t.TempDir()
	if _, err := h.s.stageHome(h.ctx, stage, nil); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(stage, "registry.db")
	b, err := os.ReadFile(staged)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(secret)) {
		t.Fatal("the staged registry still holds the secret's bytes")
	}
	db, err := sql.Open("sqlite", staged)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got string
	if err := db.QueryRow(`SELECT secret FROM test_hooks WHERE id = 'h1'`).Scan(&got); err != nil || got != "" {
		t.Fatalf("staged row: %q %v", got, err)
	}
	if err := reg.QueryRowContext(h.ctx, `SELECT secret FROM test_hooks WHERE id = 'h1'`).Scan(&got); err != nil || got != secret {
		t.Fatalf("the live registry changed: %q %v", got, err)
	}
}
