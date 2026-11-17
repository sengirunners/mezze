package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"github.com/sengirunners/potluck/computeclient"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/boxcp"
	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/conformance/refdriver"
	"github.com/sengirunners/potluck/hostprobe"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runtarget"
)

// zm sandbox cp on a box another machine lends: a file
// of several full 512 KiB chunks and a directory go up and come back
// through the authority's relay, byte for byte.
func TestSandboxCpOnARemoteHost(t *testing.T) {
	old, oldAdvert := teamlink.BeatEvery, advertWait
	teamlink.BeatEvery, advertWait = time.Hour, 10*time.Second // the test beats by hand
	t.Cleanup(func() { teamlink.BeatEvery, advertWait = old, oldAdvert })
	ctx := context.Background()
	tn := tailnettest.New()
	_, owner := poolDaemon(t, tn, "mini", "ivo@example.com")
	hostDM, host := poolDaemon(t, tn, "lender", "bea@example.com")
	var out bytes.Buffer
	if err := poolCreate(ctx, owner, "friends", poolInvites{Hosts: []string{"bea"}, Images: []string{boxPin}}, &out); err != nil {
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
	u := inPool(owner, "friends")
	const target = "lender-c1m2@lender"
	h, err := computeclient.AcquireRunTarget(ctx, u, target, computeapi.AcquireRunTargetRequest{RequestID: "cp1", TTLS: 300})
	if err != nil {
		t.Fatal(err)
	}
	id := h.Handle[strings.LastIndex(h.Handle, "/")+1:]
	t.Cleanup(func() { _, _ = computeclient.ReleaseRunTarget(context.Background(), u, target, id) })
	x := boxpush.Exec(u, target, id)

	local := t.TempDir()
	data := make([]byte, 2<<20+100)
	rand.Read(data)
	write := func(p string, b []byte) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(local, "tree", "data.bin"), data)
	write(filepath.Join(local, "tree", "sub", "note.txt"), []byte("from the renter"))
	start := time.Now()
	up, err := boxcp.Upload(ctx, x, filepath.Join(local, "tree", "data.bin"), "in/", boxcp.Options{})
	if err != nil || up.Chunks != 5 || up.Sent != 5 {
		t.Fatalf("a file up across: %+v %v", up, err)
	}
	if up, err = boxcp.Upload(ctx, x, filepath.Join(local, "tree"), "in/", boxcp.Options{}); err != nil || up.Files != 2 {
		t.Fatalf("a directory up across: %+v %v", up, err)
	}
	down := t.TempDir()
	got, err := boxcp.Download(ctx, x, "in", down, boxcp.Options{})
	if err != nil {
		t.Fatalf("down across: %v", err)
	}
	t.Logf("relayed copies: %v", time.Since(start))
	for p, want := range map[string][]byte{"in/data.bin": data, "in/tree/data.bin": data, "in/tree/sub/note.txt": []byte("from the renter")} {
		if b, err := os.ReadFile(filepath.Join(down, p)); err != nil || !bytes.Equal(b, want) {
			t.Fatalf("%s came back as %d bytes (%v); %+v", p, len(b), err, got)
		}
	}
}
