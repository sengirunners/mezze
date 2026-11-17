package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runtarget"
)

// zm sandbox create --ref HEAD on a box another machine lends:
// the authority relays every chunk to the host's driver,
// the box's checkout is the renter's commit, and nothing in the box names a
// remote or holds a credential. A job runs there the same way.
func TestSandboxCreateRefOnARemoteHost(t *testing.T) {
	ctx := context.Background()
	u := lentBox(t)

	// The renter's checkout: a commit with 3 MiB of random bytes, so its
	// bundle crosses in seven 512 KiB chunks.
	dir := testRepo(t, `echo "on $(uname -s)"; test -f data.bin && exit 3`)
	data := make([]byte, 3<<20)
	rand.Read(data)
	if err := os.WriteFile(filepath.Join(dir, "data.bin"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "data")
	head := strings.TrimSpace(gitIn(t, dir, "rev-parse", "HEAD"))

	var log bytes.Buffer
	start := time.Now()
	h, res, err := sandboxCreate(ctx, u, computeclient.BoxWant{On: "lender-c1m2@lender"}, computeapi.AcquireRunTargetRequest{RequestID: "push1", TTLS: 300, Ref: "HEAD"},
		pushFlags{repo: dir, dir: boxpush.DefaultDest, maxMiB: 16}, &log)
	if err != nil {
		t.Fatalf("create --ref HEAD: %v\n%s", err, log.String())
	}
	t.Logf("relayed push: %d bytes in %d chunks in %v", res.Bytes, res.Chunks, time.Since(start))
	if res.Commit != head || res.Chunks < 7 || res.Sent != res.Chunks || !strings.Contains(log.String(), "checked out "+head[:12]) {
		t.Fatalf("push %+v\n%s", res, log.String())
	}
	if !strings.HasPrefix(h.Handle, "box:lender-c1m2@lender/") || h.Ref != "" {
		t.Fatalf("handle %+v", h)
	}
	id := h.Handle[strings.LastIndex(h.Handle, "/")+1:]
	x, err := computeclient.ExecRunTarget(ctx, u, "lender-c1m2@lender", id, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c",
		`cd checkout && git rev-parse HEAD && git hash-object data.bin && git remote | wc -l | tr -d ' ' && test -f .git && config=$(git rev-parse --git-path config) && test -f "$config" && ! grep -Il -e 'url *=' -e 'zm_' "$config"`}})
	if want := head + "\n" + strings.TrimSpace(gitIn(t, dir, "hash-object", "data.bin")) + "\n0\n"; err != nil || x.Exit != 0 || x.Stdout != want {
		t.Fatalf("the remote box's checkout: %+v %v, want %q", x, err, want)
	}
	if rel, err := computeclient.ReleaseRunTarget(ctx, u, "lender-c1m2@lender", id); err != nil || !rel.Released {
		t.Fatalf("release: %+v %v", rel, err)
	}

	// A job on the same host: its exit code comes back over the relay.
	var jobOut bytes.Buffer
	r, err := jobRun(ctx, u, jobOptions{want: computeclient.BoxWant{On: "lender-c1m2@lender"}, repo: dir, timeout: time.Minute, out: &jobOut, errw: &log})
	if err != nil || r.code() != 3 || !r.Released || !strings.HasPrefix(jobOut.String(), "on ") {
		t.Fatalf("a job across: %+v %v; out %q\n%s", r, err, jobOut.String(), log.String())
	}
}

const boxPin = "ref-image@sha256:0000000000000000000000000000000000000000000000000000000000000001"

// lentBox is a pool, friends, whose owner (mini, the authority) rents box
// lender-c1m2@lender, which bea's machine lends on the reference driver
// (a trusted host): the owner's client in the pool.
func lentBox(t *testing.T) *client.Client {
	t.Helper()
	old, oldAdvert := teamlink.BeatEvery, advertWait
	teamlink.BeatEvery, advertWait = time.Hour, 10*time.Second // the test beats by hand
	t.Cleanup(func() { teamlink.BeatEvery, advertWait = old, oldAdvert })
	ctx := context.Background()
	tn := tailnettest.New()
	_, owner := poolDaemon(t, tn, "mini", "ivo@example.com")
	hostDM, host := poolDaemon(t, tn, "lender", "bea@example.com")
	var out bytes.Buffer
	err := poolCreate(ctx, owner, "friends", poolInvites{Hosts: []string{"bea"}, Images: []string{boxPin}}, &out)
	if err != nil {
		t.Fatalf("create: %v\n%s", err, out.String())
	}
	codes := hostRe.FindAllStringSubmatch(out.String(), -1)
	if len(codes) != 1 {
		t.Fatalf("create printed:\n%s", out.String())
	}
	fix, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(fix, "state")
	drv, err := refdriver.Install(fix, state, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { refdriver.KillAll(state) })
	for _, m := range hostDM.Modules() {
		if s, ok := m.(*runtarget.Service); ok {
			s.SetLookPath(func(string) (string, error) { return drv, nil })
		}
	}
	beat(t, hostDM)
	run := runnerFunc(func(context.Context, string, ...string) (hostprobe.Result, error) {
		return hostprobe.Result{Stdout: "PASS"}, nil
	})
	o := hostprobe.ShareOptions{Report: hostprobe.Report{OS: "linux", Arch: "amd64", Cores: 8, RAMMB: 16 << 10, FreeDiskGB: 100, KVM: true,
		Layers: []hostprobe.Layer{{Name: "oci", Available: true, Supported: true, CLI: "docker", Runtime: "runsc", Runtimes: []string{"runsc"}}}},
		Images: []string{boxPin}, Sizes: []string{"c1m2"}, NamePrefix: "lender", Driver: "ref"}
	if err := poolHost(ctx, host, run, codes[0][1], o, &out, nil, nil); err != nil {
		t.Fatalf("host: %v\n%s", err, out.String())
	}
	return inPool(owner, "friends")
}

// testRepo is a checkout whose commit has job as an executable .zm/job.
func testRepo(t *testing.T, job string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".zm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".zm", "job"), []byte("#!/bin/sh\n"+job+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "msg.txt"), []byte("from the renter"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"commit", "-qm", "job"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@x"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}
