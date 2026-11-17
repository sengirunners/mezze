package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"

	"github.com/sengirunners/potluck/boxpush"
	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/depscli"
	"github.com/sengirunners/potluck/kit/client"
)

// depsReq is what --deps args ask for, run from dir.
func depsReq(t *testing.T, dir string, args ...string) (computeapi.AcquireRunTargetRequest, error) {
	t.Helper()
	var df depscli.Flags
	f := pflag.NewFlagSet("t", pflag.ContinueOnError)
	df.Add(f)
	if err := f.Parse(args); err != nil {
		t.Fatal(err)
	}
	var req computeapi.AcquireRunTargetRequest
	err := df.Request(context.Background(), f, dir, boxpush.DefaultDest, &req)
	return req, err
}

// zm job run --deps npm on a binding another machine lends:
// the first job builds the layer, the next one hits it;
// the push checks the commit out around the layer's node_modules at
// checkout/<the project's directory>, where the command runs (the commit's
// .zm/job too); what a box writes there never reaches the layer; an image
// name with deps there is refused before a box (by the daemon for a job, by
// zm before the bundle for a sandbox). zm sandbox create --deps, with --ref,
// pushes around the same layer.
func TestJobRunDepsOnALentBinding(t *testing.T) {
	ctx := context.Background()
	u := lentBox(t)
	dir := testRepo(t, `pwd >&2; cat node_modules/marker main.txt; git status --porcelain; echo box >node_modules/marker`)
	app := filepath.Join(dir, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, c := range map[string]string{".gitignore": "node_modules/\n", "app/package-lock.json": `{"lockfileVersion":3,"packages":{"":{}}}`,
		"app/package.json": "{}", "app/main.txt": "code"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "app")
	// The reference driver's boxes run sh here: an npm on PATH stands in.
	npmBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(npmBin, "npm"), []byte("#!/bin/sh\nmkdir -p node_modules && echo layer >node_modules/marker\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", npmBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	req, err := depsReq(t, app, "--deps", "npm")
	if err != nil || len(req.Deps) != 1 || req.Deps[0].Path != "checkout/app" || len(req.Deps[0].Files) != 2 {
		t.Fatalf("--deps npm from app/: %+v %v", req.Deps, err)
	}
	under := depsUnder(ctx, app, boxpush.DefaultDest, req.Deps)
	if under != "app" {
		t.Fatalf("depsUnder from app/: %q, want app", under)
	}
	job := func(image string, argv ...string) (jobResult, string, string, error) {
		var out, log bytes.Buffer
		r, err := jobRun(ctx, u, jobOptions{want: computeclient.BoxWant{On: "lender-c1m2@lender"}, image: image, repo: app,
			deps: req.Deps, depsWaitS: req.DepsWaitS, under: under, timeout: time.Minute, out: &out, errw: &log, argv: argv})
		return r, out.String(), log.String(), err
	}
	for i, want := range []string{"built", "hit"} {
		var argv []string // the commit's .zm/job, from app/
		if i == 1 {
			argv = []string{"sh", "-c", `cat node_modules/marker main.txt; git status --porcelain; echo box >node_modules/marker`}
		}
		r, out, log, err := job("", argv...)
		if err != nil || r.code() != 0 || !r.Released || len(r.Deps) != 1 || !strings.Contains(log, "zm: deps npm at checkout/app: "+want) {
			t.Fatalf("job %d: %+v %v\n%s", i, r, err, log)
		}
		// node_modules is the layer's, untouched by the push and by the
		// last box's write; the commit's files sit beside it, clean.
		if out != "layer\ncode" || (i == 0 && !strings.Contains(log, "/checkout/app\n")) {
			t.Fatalf("job %d's checkout: %q\n%s", i, out, log)
		}
	}
	r, _, _, err := job("other-image")
	if err == nil || r.Handle != "" || !strings.Contains(err.Error(), "name a template") {
		t.Fatalf("--image with --deps on a lent binding: %+v %v", r, err)
	}

	// zm sandbox create --deps --ref HEAD: the same layer, a hit, and the
	// push checks the commit out around it.
	var log bytes.Buffer
	req.RequestID, req.TTLS, req.Ref = "deps-sb", 300, "HEAD"
	h, res, err := sandboxCreate(ctx, u, computeclient.BoxWant{On: "lender-c1m2@lender", Volumes: true}, req, pushFlags{repo: app, dir: boxpush.DefaultDest, maxMiB: 16}, &log)
	if err != nil || res == nil || len(h.Deps) != 1 || !h.Deps[0].Hit || !strings.Contains(log.String(), "zm: deps npm at checkout/app: hit") {
		t.Fatalf("sandbox create --deps --ref HEAD: %+v %+v %v\n%s", h, res, err, log.String())
	}
	_, id, _ := computeclient.SplitBoxHandle(h.Handle)
	x, err := computeclient.ExecRunTarget(ctx, u, "lender-c1m2@lender", id, computeapi.ExecRunTargetRequest{Argv: []string{"sh", "-c",
		`cd checkout/app && cat node_modules/marker main.txt && git status --porcelain`}})
	if err != nil || x.Exit != 0 || x.Stdout != "layer\ncode" {
		t.Fatalf("the sandbox's checkout: %+v %v", x, err)
	}
	if _, err := computeclient.ReleaseRunTarget(ctx, u, "lender-c1m2@lender", id); err != nil {
		t.Fatal(err)
	}
	req.RequestID, req.Image, req.Ref = "deps-sb-image", "other-image", ""
	if h, _, err := sandboxCreate(ctx, u, computeclient.BoxWant{On: "lender-c1m2@lender"}, req, pushFlags{dir: boxpush.DefaultDest, maxMiB: 16}, &log); err == nil || h.Handle != "" || !strings.Contains(err.Error(), "takes a template") {
		t.Fatalf("sandbox create --image --deps on a lent binding: %+v %v", h, err)
	}
}

// zm sandbox create --ref --deps on a binding that checks refs out itself
// is refused before a box: its checkout is not where the layer mounts.
func TestSandboxCreateDepsRefusesASelfCheckout(t *testing.T) {
	ctx := context.Background()
	c, _, _, sock := jobBox(t)
	// The binding as a repo's: ChecksOutItself reads repo from the list.
	w := client.WithHandler(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/run_targets" {
			sock.ServeHTTP(rw, r)
			return
		}
		rec := httptest.NewRecorder()
		sock.ServeHTTP(rec, r)
		var l computeapi.RunTargetList
		if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
			t.Error(err)
		}
		for i := range l.Targets {
			l.Targets[i].Repo = "acme/app"
		}
		for k, v := range rec.Header() { // the daemon's protocol header among them
			rw.Header()[k] = v
		}
		rw.Header().Del("Content-Length")
		rw.WriteHeader(rec.Code)
		json.NewEncoder(rw).Encode(l)
	}))
	w.Project, w.ProjectFrom = c.Project, c.ProjectFrom
	req := computeapi.AcquireRunTargetRequest{RequestID: "self", Ref: "main", Deps: []computeapi.BoxDeps{{Kind: "npm", Path: "checkout"}}}
	var log bytes.Buffer
	h, _, err := sandboxCreate(ctx, w, computeclient.BoxWant{On: "box"}, req, pushFlags{dir: boxpush.DefaultDest, maxMiB: 16}, &log)
	if err == nil || h.Handle != "" || !strings.Contains(err.Error(), "checks out --ref itself") {
		t.Fatalf("--ref --deps on a binding with a repo: %+v %v", h, err)
	}
	if hs, _, _ := computeclient.Sandboxes(ctx, c, false); len(hs) != 0 {
		t.Fatalf("a box was made: %+v", hs)
	}
}

// depsUnder is the project's directory in the checkout only when a layer
// takes the default path there: an explicit path, an absolute --dir or the
// repository's top runs the command at the checkout's top.
func TestDepsUnder(t *testing.T) {
	ctx := context.Background()
	dir := testRepo(t, `exit 0`)
	app := filepath.Join(dir, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		dir, dest, path, want string
	}{
		{app, "checkout", "checkout/app", "app"},
		{app, "checkout/", "checkout/app", "app"},
		{app, "checkout", "elsewhere", ""},
		{app, "/abs", "/abs", ""},
		{dir, "checkout", "checkout", ""},
	} {
		if got := depsUnder(ctx, c.dir, c.dest, []computeapi.BoxDeps{{Kind: "npm", Path: c.path}}); got != c.want {
			t.Errorf("depsUnder(%s, %q, %q) = %q, want %q", filepath.Base(c.dir), c.dest, c.path, got, c.want)
		}
	}
	if got := depsUnder(ctx, app, "checkout", nil); got != "" {
		t.Errorf("no deps: %q", got)
	}
}
