package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/potluck/boxproc"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/runtarget"
)

// jobBox is a daemon with one box binding on the reference driver, bound
// without a repo as a remote box is: its owner's client, its ZM_HOME and the
// driver's state directory, and the daemon's socket handler.
func jobBox(t *testing.T) (*client.Client, string, string, http.Handler) {
	t.Helper()
	dm, c := poolDaemon(t, tailnettest.New(), "mini", "ivo@example.com")
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	for _, m := range dm.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		}
	}
	ctx := context.Background()
	c.Project, c.ProjectFrom = "acme", "flag"
	if _, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: "acme"}); err != nil {
		t.Fatal(err)
	}
	if _, err := computeclient.BindRunTarget(ctx, c, computeapi.BindRunTargetRequest{Target: computeapi.RunTarget{Name: "box", Driver: "ref", Kind: "box", Isolation: "ephemeral", Images: []string{boxPin}}}); err != nil {
		t.Fatal(err)
	}
	return c, dm.Deps.Config.Home, state, dm.SocketHandler()
}

// zmCmd is the zm binary (this test binary) on home's daemon, in dir,
// with none of this session's zm identity.
func zmCmd(home, dir string, args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Dir = dir
	cmd.Env = []string{"ZM_TEST_MAIN=1", "ZM_HOME=" + home, "ZM_NO_AUTOSTART=1"}
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ZM_") && !strings.HasPrefix(kv, "CLAUDE") && !strings.HasPrefix(kv, "CODEX") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	return cmd
}

// boxState is the handle's recorded state at the daemon.
func boxState(t *testing.T, c *client.Client, handle string) string {
	t.Helper()
	target, id, err := computeclient.SplitBoxHandle(handle)
	if err != nil {
		t.Fatal(err)
	}
	h, err := computeclient.RunTargetStatus(context.Background(), c, target, id)
	if err != nil {
		t.Fatal(err)
	}
	return h.State
}

// lineWatch is a writer the test reads while the job writes, calling on the
// first time a line appears.
type lineWatch struct {
	mu   sync.Mutex
	b    bytes.Buffer
	on   string
	call func()
}

func (s *lineWatch) Write(p []byte) (int, error) {
	s.mu.Lock()
	n, _ := s.b.Write(p)
	hit := s.call != nil && strings.Contains(s.b.String(), s.on)
	f := s.call
	if hit {
		s.call = nil
	}
	s.mu.Unlock()
	if hit {
		f()
	}
	return n, nil
}

func (s *lineWatch) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// `zm job run`, the binary itself: the commit's .zm/job runs in the box's
// checkout, its output streams, the command exits with its code (3), and
// the box is released; --json answers the result on stdout alone.
func TestJobRunExitsWithTheScriptsCode(t *testing.T) {
	c, home, _, _ := jobBox(t)
	dir := testRepo(t, `cat msg.txt; echo; echo to-stderr >&2; exit 3`)
	run := func(args ...string) (string, string, int) {
		cmd := zmCmd(home, dir, append([]string{"job", "run", "-P", "acme"}, args...)...)
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		err := cmd.Run()
		var ee *exec.ExitError
		if err != nil && !errors.As(err, &ee) {
			t.Fatal(err)
		}
		return out.String(), errb.String(), cmd.ProcessState.ExitCode()
	}
	out, errs, code := run()
	if code != 3 || out != "from the renter\n" || !strings.Contains(errs, "to-stderr") {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	out, errs, code = run("--json", "--", "sh", "-c", "echo hi; exit 0")
	var r jobResult
	if err := json.Unmarshal([]byte(out), &r); err != nil || code != 0 || r.State != "exited" || *r.Exit != 0 || !r.Released || !strings.Contains(errs, "hi") {
		t.Fatalf("--json: exit %d, %q, %+v %v; stderr %q", code, out, r, err, errs)
	}
	if st := boxState(t, c, r.Handle); st != "released" {
		t.Errorf("the box is %s after the job", st)
	}
	if hs, _, err := computeclient.Sandboxes(context.Background(), c, true); err != nil || len(hs) != 0 {
		t.Errorf("live boxes after the jobs: %+v %v", hs, err)
	}
}

// Ctrl-C, the timeout and a lost process each end the job and release the
// box; --keep keeps it; a lost process is a failure, never a pass.
func TestJobRunEnds(t *testing.T) {
	ctx := context.Background()
	c, _, state, _ := jobBox(t)
	dir := testRepo(t, `echo started; sleep 60; echo never`)
	opts := func(w *lineWatch) jobOptions {
		return jobOptions{repo: dir, timeout: time.Minute, out: w, errw: &bytes.Buffer{}}
	}

	// Ctrl-C (the command's context ends) once the script is going.
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w := &lineWatch{on: "started", call: cancel}
	r, err := jobRun(cctx, c, opts(w))
	if err == nil || r.State != "interrupted" || r.code() != 130 || !r.Released || boxState(t, c, r.Handle) != "released" {
		t.Fatalf("Ctrl-C: %+v %v; out %q", r, err, w.String())
	}

	// The timeout: the driver kills the script (exit 124), the box goes.
	o := opts(&lineWatch{})
	o.timeout = 2 * time.Second
	r, err = jobRun(ctx, c, o)
	if err == nil || r.State != "timeout" || r.code() != 124 || !r.Released || boxState(t, c, r.Handle) != "released" {
		t.Fatalf("timeout: %+v %v", r, err)
	}

	// A lost process: its holder dies, so it ends with no exit code.
	w = &lineWatch{on: "started", call: func() { killHolders(t, state) }}
	r, err = jobRun(ctx, c, opts(w))
	if err == nil || r.State != "lost" || r.code() == 0 || r.Exit != nil || !r.Released {
		t.Fatalf("lost: %+v %v", r, err)
	}

	// --keep: the box stays up with the checkout, for inspection.
	o = opts(&lineWatch{})
	o.keep, o.argv = true, []string{"sh", "-c", "echo kept > out.txt"}
	r, err = jobRun(ctx, c, o)
	if err != nil || r.State != "exited" || r.code() != 0 || !r.Kept || r.Released || boxState(t, c, r.Handle) != "ready" {
		t.Fatalf("--keep: %+v %v", r, err)
	}
	target, id, _ := computeclient.SplitBoxHandle(r.Handle)
	x, err := computeclient.ExecRunTarget(ctx, c, target, id, computeapi.ExecRunTargetRequest{Argv: []string{"cat", "checkout/out.txt", "checkout/msg.txt"}})
	if err != nil || x.Stdout != "kept\nfrom the renter" {
		t.Fatalf("the kept box: %+v %v", x, err)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, c, target, id); err != nil {
		t.Fatal(err)
	}

	// No command and no .zm/job: refused before any box is made.
	o = opts(&lineWatch{})
	o.repo = testRepo(t, "true")
	if err := os.Remove(filepath.Join(o.repo, ".zm", "job")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-C", o.repo, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-qam", "no job")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if r, err := jobRun(ctx, c, o); err == nil || r.Handle != "" || !strings.Contains(err.Error(), "no command given") {
		t.Fatalf("no job: %+v %v", r, err)
	}
}

// A closed terminal (SIGHUP) ends zm job run as Ctrl-C does: the command
// is stopped, the box released, exit 130.
func TestJobRunHangupReleases(t *testing.T) {
	c, home, _, _ := jobBox(t)
	cmd := zmCmd(home, testRepo(t, `echo started; sleep 60`), "job", "run", "-P", "acme")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill() })
	line, _ := bufio.NewReader(out).ReadString('\n')
	if line != "started\n" {
		t.Fatalf("stdout %q; stderr %q", line, errb.String())
	}
	if err := cmd.Process.Signal(syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	if code := cmd.ProcessState.ExitCode(); code != 130 {
		t.Fatalf("exit %d after SIGHUP; stderr %q", code, errb.String())
	}
	if hs, _, err := computeclient.Sandboxes(context.Background(), c, true); err != nil || len(hs) != 0 {
		t.Errorf("live boxes after the hangup: %+v %v", hs, err)
	}
}

// Ctrl-C before the command starts: while the bundle is made (no misleading
// "not in a git checkout"), while the box is being made (the daemon goes on
// acquiring after the client left: the same box is taken and released),
// and while the code is sent. Each is exit 130 with no box left up.
func TestJobRunInterruptedEarly(t *testing.T) {
	ctx := context.Background()
	c, _, _, sock := jobBox(t)
	dir := testRepo(t, `echo never`)
	live := func() int {
		t.Helper()
		hs, _, err := computeclient.Sandboxes(ctx, c, true)
		if err != nil {
			t.Fatal(err)
		}
		return len(hs)
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	r, err := jobRun(gone, c, jobOptions{repo: dir, timeout: time.Minute, out: io.Discard, errw: io.Discard})
	if !errors.Is(err, errInterrupted) || r.State != "interrupted" || r.code() != 130 || r.Handle != "" {
		t.Fatalf("Ctrl-C while bundling: %+v %v", r, err)
	}

	// Mid-acquire: the request reaches the daemon and the box boots after the
	// caller is gone (as over the socket: the daemon acquires on).
	var acquires int
	cut := func(on string) (*client.Client, context.Context) {
		cctx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		w := client.WithHandler(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
			if cctx.Err() == nil && req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/"+on) {
				if on == "acquire" {
					acquires++
				}
				cancel()
				rec := httptest.NewRecorder()
				sock.ServeHTTP(rec, req.WithContext(context.WithoutCancel(req.Context())))
				api.WriteError(rw, api.Errorf(api.CodeUnavailable, "the caller went"))
				return
			}
			sock.ServeHTTP(rw, req)
		}))
		w.Project, w.ProjectFrom = c.Project, c.ProjectFrom
		return w, cctx
	}
	w, wctx := cut("acquire")
	r, err = jobRun(wctx, w, jobOptions{repo: dir, timeout: time.Minute, out: io.Discard, errw: io.Discard})
	if !errors.Is(err, errInterrupted) || r.State != "interrupted" || r.code() != 130 || !r.Released || acquires != 1 || live() != 0 {
		t.Fatalf("Ctrl-C mid-acquire: %+v %v (live %d)", r, err, live())
	}
	if hs, _, _ := computeclient.Sandboxes(ctx, c, false); len(hs) != 1 || hs[0].State != "released" {
		t.Fatalf("one box, released: %+v", hs)
	}

	// While the code is sent: the first exec is the last.
	w, wctx = cut("exec")
	r, err = jobRun(wctx, w, jobOptions{repo: dir, timeout: time.Minute, out: io.Discard, errw: io.Discard})
	if !errors.Is(err, errInterrupted) || r.State != "interrupted" || r.code() != 130 || !r.Released || r.PID != "" || live() != 0 {
		t.Fatalf("Ctrl-C while pushing: %+v %v", r, err)
	}
}

// killHolders SIGKILLs every background process's holder in the driver's
// state, as a crash of the box's host would.
func killHolders(t *testing.T, state string) {
	t.Helper()
	n := 0
	filepath.WalkDir(state, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Name() != "meta.json" {
			return nil
		}
		m, err := boxproc.Dir(filepath.Dir(p)).Meta()
		if err == nil && m.Holder > 0 && syscall.Kill(m.Holder, 0) == nil {
			syscall.Kill(m.Holder, syscall.SIGKILL)
			n++
		}
		return nil
	})
	if n == 0 {
		t.Error("no holder to kill")
	}
}

// through is c's daemon reached through a handler that answers the requests
// failing says to with "unavailable", as a client does whose authority
// stopped answering.
func through(c *client.Client, sock http.Handler, failing func(*http.Request) bool) *client.Client {
	w := client.WithHandler(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if failing(req) {
			api.WriteError(rw, api.Errorf(api.CodeUnavailable, "authority unreachable"))
			return
		}
		sock.ServeHTTP(rw, req)
	}))
	w.Project, w.ProjectFrom = c.Project, c.ProjectFrom
	return w
}

// handleOf is the box's record at the daemon.
func handleOf(t *testing.T, c *client.Client, handle string) computeapi.RunTargetHandle {
	t.Helper()
	target, id, err := computeclient.SplitBoxHandle(handle)
	if err != nil {
		t.Fatal(err)
	}
	h, err := computeclient.RunTargetStatus(context.Background(), c, target, id)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// A running job's box holds only a short lease, which the client keeps
// renewing: a script that outlives several leases is not cut off, and a box
// kept with --keep gets the whole planned lease, not the short one.
func TestJobRunLeaseIsShortAndRenewed(t *testing.T) {
	ctx := context.Background()
	c, _, _, _ := jobBox(t)
	dir := testRepo(t, `echo started; sleep 8; echo done`)
	defer func(l, e time.Duration) { jobLeaseTTL, jobRenewEvery = l, e }(jobLeaseTTL, jobRenewEvery)
	jobLeaseTTL, jobRenewEvery = 4*time.Second, 500*time.Millisecond

	var first time.Time
	w := &lineWatch{on: "started"}
	w.call = func() {
		r := handleOf(t, c, "box:"+boxTarget(t, c))
		first, _ = time.Parse(time.RFC3339Nano, r.ExpiresAt)
	}
	r, err := jobRun(ctx, c, jobOptions{repo: dir, timeout: time.Minute, out: w, errw: io.Discard})
	if err != nil || r.State != "exited" || r.Exit == nil || *r.Exit != 0 || !r.Released || !strings.Contains(w.String(), "done") {
		t.Fatalf("a script that outlives its short lease: %+v %v; out %q", r, err, w.String())
	}
	if left := time.Until(first); first.IsZero() || left > 10*time.Second {
		t.Errorf("the running box's lease ends in %v (at %v): want about %v, not the job's whole minute and slack", left, first, jobLeaseTTL)
	}

	o := jobOptions{repo: dir, timeout: time.Minute, out: io.Discard, errw: io.Discard, keep: true, argv: []string{"true"}}
	r, err = jobRun(ctx, c, o)
	if err != nil || !r.Kept {
		t.Fatalf("--keep: %+v %v", r, err)
	}
	exp, _ := time.Parse(time.RFC3339Nano, handleOf(t, c, r.Handle).ExpiresAt)
	if left := time.Until(exp); left < 30*time.Second {
		t.Errorf("a kept box's lease ends in %v: it should be the job's whole lease", left)
	}
	target, id, _ := computeclient.SplitBoxHandle(r.Handle)
	computeclient.ReleaseRunTarget(ctx, c, target, id)
}

// A job that renews its own short lease says so on the acquire (self_renew),
// so the daemon's sweep does not renew it for a live holder session after
// this process dies; a kept box, which nobody renews, does not.
func TestJobRunAcquireSaysItRenewsItself(t *testing.T) {
	ctx := context.Background()
	c, _, _, sock := jobBox(t)
	defer func(l, e time.Duration) { jobLeaseTTL, jobRenewEvery = l, e }(jobLeaseTTL, jobRenewEvery)
	jobLeaseTTL, jobRenewEvery = 4*time.Second, 500*time.Millisecond
	var mu sync.Mutex
	var bodies []computeapi.AcquireRunTargetRequest
	w := through(c, sock, func(req *http.Request) bool {
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/acquire") {
			b, _ := io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewReader(b))
			var ar computeapi.AcquireRunTargetRequest
			if json.Unmarshal(b, &ar) == nil {
				mu.Lock()
				bodies = append(bodies, ar)
				mu.Unlock()
			}
		}
		return false
	})
	dir := testRepo(t, `true`)
	for _, tc := range []struct {
		name string
		keep bool
		want bool
	}{{"leased", false, true}, {"kept", true, false}} {
		mu.Lock()
		bodies = nil
		mu.Unlock()
		r, err := jobRun(ctx, w, jobOptions{repo: dir, timeout: time.Minute, out: io.Discard, errw: io.Discard, keep: tc.keep, argv: []string{"true"}})
		if err != nil {
			t.Fatalf("%s: %+v %v", tc.name, r, err)
		}
		mu.Lock()
		if len(bodies) != 1 || bodies[0].SelfRenew != tc.want {
			t.Errorf("%s: acquire requests %+v: want one with self_renew %v", tc.name, bodies, tc.want)
		}
		mu.Unlock()
		if got := handleOf(t, c, r.Handle).SelfRenew; got != tc.want {
			t.Errorf("%s: the daemon recorded self_renew %v, want %v", tc.name, got, tc.want)
		}
		if tc.keep {
			target, id, _ := computeclient.SplitBoxHandle(r.Handle)
			computeclient.ReleaseRunTarget(ctx, c, target, id)
		}
	}
}

// boxTarget is the live box's handle's part after "box:" (one box is up).
func boxTarget(t *testing.T, c *client.Client) string {
	t.Helper()
	hs, _, err := computeclient.Sandboxes(context.Background(), c, true)
	if err != nil || len(hs) != 1 {
		t.Fatalf("one live box: %+v %v", hs, err)
	}
	return strings.TrimPrefix(hs[0].Handle, "box:")
}

// A release the authority drops is asked again, so a blip of the network
// does not leave the box up for its lease.
func TestJobRunReleaseIsRetried(t *testing.T) {
	ctx := context.Background()
	c, _, _, sock := jobBox(t)
	var mu sync.Mutex
	dropped := 0
	w := through(c, sock, func(req *http.Request) bool {
		mu.Lock()
		defer mu.Unlock()
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/release") && dropped < 2 {
			dropped++
			return true
		}
		return false
	})
	r, err := jobRun(ctx, w, jobOptions{repo: testRepo(t, `exit 0`), timeout: time.Minute, out: io.Discard, errw: io.Discard})
	if err != nil || r.State != "exited" || !r.Released || dropped != 2 || boxState(t, c, r.Handle) != "released" {
		t.Fatalf("release after %d dropped: %+v %v", dropped, r, err)
	}
}

// A client that dies (or whose authority goes) mid-run can neither renew nor
// release: its box ends at its short lease, not at the job's whole one
// (timeout plus slack), and the failed run says so.
func TestJobRunDeadClientsBoxEndsAtItsShortLease(t *testing.T) {
	ctx := context.Background()
	c, _, _, sock := jobBox(t)
	dir := testRepo(t, `echo started; sleep 120`)
	defer func(l, e, a, b time.Duration) { jobLeaseTTL, jobRenewEvery, jobLostAfter, jobReleaseFor = l, e, a, b }(jobLeaseTTL, jobRenewEvery, jobLostAfter, jobReleaseFor)
	jobLeaseTTL, jobRenewEvery, jobLostAfter, jobReleaseFor = 5*time.Second, 500*time.Millisecond, time.Second, 2*time.Second

	var mu sync.Mutex
	cut := false
	w := through(c, sock, func(*http.Request) bool { mu.Lock(); defer mu.Unlock(); return cut })
	out := &lineWatch{on: "started", call: func() { mu.Lock(); cut = true; mu.Unlock() }}
	errb := &lineWatch{}
	r, err := jobRun(ctx, w, jobOptions{repo: dir, timeout: time.Minute, out: out, errw: errb})
	if !strings.Contains(out.String(), "started") {
		t.Fatalf("the script never started: %+v %v; stderr %q", r, err, errb.String())
	}
	if err == nil || r.Released || r.State != "lost" {
		t.Fatalf("a cut-off client: %+v %v", r, err)
	}
	if !strings.Contains(errb.String(), "could not release") {
		t.Errorf("the failed release is not said: %q", errb.String())
	}
	// The daemon's sweep runs every 30 s: the lease ended well before it.
	for deadline := time.Now().Add(90 * time.Second); ; time.Sleep(time.Second) {
		st := boxState(t, c, r.Handle)
		if st != "ready" {
			if st != "expired" && st != "released" && st != "reclaimed" {
				t.Fatalf("the dead client's box is %s", st)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the dead client's box is still %s 90 s on, its lease was %v (the job's was a minute and its slack)", st, jobLeaseTTL)
		}
	}
}
