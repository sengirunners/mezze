package fulltest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/runtarget"
)

// TestRunTargetRenewalFollowsSession: a session-held handle is renewed by
// the sweep while its session lives (once a third of its TTL is left, at
// most once a minute) and not after it ends; run-target keys are refused by
// the lease verbs. Moved from potluck/runtarget (TestBindSetup keeps the
// bind --setup part): it needs zm's sessions and work.
func TestRunTargetRenewalFollowsSession(t *testing.T) {
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	t.Setenv("HOME", home)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ivo", "mini"
	cfg.File.Admission = config.Admission{MinMemoryFreePercent: 1, MaxSwapUsedMB: 1 << 40, MinDiskFreeMB: 1} // the session spawns on any host
	dm, err := server.New(context.Background(), cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dm.Close() })
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	repo, err := refdriver.NewRepo(fix, "#!/bin/sh\nsleep 60\n")
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(fix, "state")
	ref, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	var svc *runtarget.Service
	for _, m := range dm.Modules() {
		if st, ok := m.(interface{ Start(context.Context) error }); ok {
			if _, isSession := m.(*session.Service); isSession {
				if err := st.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
		}
		if s, ok := m.(*runtarget.Service); ok {
			svc = s
			s.SetLookPath(func(string) (string, error) { return ref, nil })
		}
	}
	ctx := context.Background()
	owner := client.WithHandler(dm.SocketHandler())
	owner.Project, owner.ProjectFrom = "acme", "flag"
	if _, err := owner.CreateProject(ctx, api.NewProjectRequest{Slug: "acme", RepoRules: api.RepoRules{Paths: []string{fix}, Remotes: []string{repo.Remote}}}); err != nil {
		t.Fatal(err)
	}
	code := func(err error) string {
		var e *api.Error
		if errors.As(err, &e) {
			return e.Code
		}
		if err != nil {
			return err.Error()
		}
		return ""
	}
	idOf := func(h string) string { return h[strings.LastIndex(h, "/")+1:] }

	// A session holds a handle; the sweep renews it while the session lives.
	if _, err := computeclient.BindRunTarget(ctx, owner, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "vm", Driver: "ref", Kind: "run",
		Isolation: "ephemeral", Repo: repo.Remote, Branch: "main", Config: map[string]string{"remote": repo.Remote}}}); err != nil {
		t.Fatal(err)
	}
	sess, err := orchclient.Spawn(ctx, owner, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: fix})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = orchclient.Stop(context.Background(), owner, sess.Address, orchapi.StopRequest{}) })
	h, err := computeclient.AcquireRunTarget(ctx, owner, "vm", computeapi.AcquireRunTargetRequest{RequestID: "r1", Ref: "main", Script: "probe.sh", TTLS: 90, Actor: sess.Address})
	if err != nil {
		t.Fatal(err)
	}
	exp0, _ := time.Parse(time.RFC3339Nano, h.ExpiresAt)
	at := func(d time.Duration) { svc.SetNow(func() time.Time { return time.Now().Add(d) }) }
	at(10 * time.Second) // most of the TTL left: no renewal
	svc.Sweep(ctx)
	if st, _ := computeclient.RunTargetStatus(ctx, owner, "vm", idOf(h.Handle)); st.ExpiresAt != h.ExpiresAt {
		t.Errorf("renewed too early: %s -> %s", h.ExpiresAt, st.ExpiresAt)
	}
	at(65 * time.Second) // under a third left: renewed
	svc.Sweep(ctx)
	st, _ := computeclient.RunTargetStatus(ctx, owner, "vm", idOf(h.Handle))
	exp1, _ := time.Parse(time.RFC3339Nano, st.ExpiresAt)
	if !exp1.After(exp0) {
		t.Fatalf("not renewed: %s -> %s", h.ExpiresAt, st.ExpiresAt)
	}
	// The session ends: no more renewal.
	if _, err := orchclient.Stop(ctx, owner, sess.Address, orchapi.StopRequest{}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if s, err := orchclient.Session(ctx, owner, sess.Address); err == nil && s.State == orchapi.StateEnded {
			break
		}
	}
	// A third left again and past the once-a-minute throttle, but the session ended.
	svc.SetNow(func() time.Time { return exp1.Add(-20 * time.Second) })
	svc.Sweep(ctx)
	if st2, _ := computeclient.RunTargetStatus(ctx, owner, "vm", idOf(h.Handle)); st2.ExpiresAt != st.ExpiresAt {
		t.Errorf("renewed after the session ended: %s -> %s", st.ExpiresAt, st2.ExpiresAt)
	}
	svc.SetNow(time.Now)

	// Run-target keys are not lease keys.
	for _, k := range []string{"run:vm/r1", "db:x/y", "box:b/1"} {
		if _, err := orchclient.Lease(ctx, owner, orchapi.ClaimRequest{Key: k}); code(err) != api.CodeBadRequest {
			t.Errorf("lease on %s: %v", k, err)
		}
	}
}
