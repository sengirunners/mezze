package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/potluck/kit/api"
)

// mem is a minimal correct adapter kept in memory.
func mem() map[string]adapter.Handler {
	it := orchapi.WorkItem{ID: "mm:1", Source: "mm", Title: "one", Status: orchapi.WorkOpen}
	get := func(_ context.Context, req orchapi.AdapterRequest) (any, error) {
		if req.ID != it.ID {
			return nil, api.Errorf(api.CodeNotFound, "no %s", req.ID)
		}
		return it, nil
	}
	return map[string]adapter.Handler{
		orchapi.VerbDescribe: func(context.Context, orchapi.AdapterRequest) (any, error) {
			return orchapi.AdapterDescribe{Spec: api.Spec, Name: "mem", Prefix: "mm", Verbs: orchapi.RequiredVerbs}, nil
		},
		orchapi.VerbList: func(context.Context, orchapi.AdapterRequest) (any, error) {
			return orchapi.WorkList{Items: []orchapi.WorkItem{it}}, nil
		},
		orchapi.VerbGet: get,
		orchapi.VerbClaim: func(ctx context.Context, req orchapi.AdapterRequest) (any, error) {
			var a orchapi.AdapterClaimArgs
			_ = json.Unmarshal(req.Args, &a)
			if it.Owner != "" && it.Owner != a.Holder {
				return nil, api.Errorf(api.CodeConflict, "held by %s", it.Owner)
			}
			it.Owner, it.Status = a.Holder, orchapi.WorkClaimed
			return it, nil
		},
		orchapi.VerbRelease: func(ctx context.Context, req orchapi.AdapterRequest) (any, error) {
			var a orchapi.AdapterReleaseArgs
			_ = json.Unmarshal(req.Args, &a)
			if it.Owner != "" && it.Owner != a.Holder {
				return nil, api.Errorf(api.CodeConflict, "held by %s", it.Owner)
			}
			it.Owner, it.Status = "", orchapi.WorkOpen
			return it, nil
		},
		orchapi.VerbComment: func(_ context.Context, req orchapi.AdapterRequest) (any, error) {
			var a orchapi.AdapterCommentArgs
			_ = json.Unmarshal(req.Args, &a)
			if a.Body == "" {
				return nil, api.Errorf(api.CodeBadRequest, "empty")
			}
			return struct{}{}, nil
		},
		orchapi.VerbSetStatus: func(_ context.Context, req orchapi.AdapterRequest) (any, error) {
			var a orchapi.AdapterSetStatusArgs
			_ = json.Unmarshal(req.Args, &a)
			if !orchapi.ValidStatus(a.Status) {
				return nil, api.Errorf(api.CodeBadRequest, "bad")
			}
			it.Status = a.Status
			if a.Status == orchapi.WorkOpen {
				it.Owner = ""
			}
			return it, nil
		},
	}
}

func inProcess(h map[string]adapter.Handler) RoundTrip {
	return func(ctx context.Context, in []byte) (orchapi.AdapterResponse, error) {
		var out bytes.Buffer
		adapter.Serve(ctx, bytes.NewReader(in), &out, h)
		var r orchapi.AdapterResponse
		return r, json.Unmarshal(out.Bytes(), &r)
	}
}

func TestGoodAdapter(t *testing.T) {
	res := Run(context.Background(), inProcess(mem()), Options{Write: true})
	if n := Failed(res); n > 0 {
		t.Fatalf("%d failed:\n%s", n, Format(res))
	}
	if !strings.Contains(Format(res), "PASS  link answers not_implemented when not listed") {
		t.Fatalf("link check missing:\n%s", Format(res))
	}
	res = Run(context.Background(), inProcess(mem()), Options{})
	if Failed(res) > 0 || !strings.Contains(Format(res), "SKIP  write checks") {
		t.Fatalf("read-only:\n%s", Format(res))
	}
}

// Read-only runs never send a write verb, even to an adapter that lists them all.
func TestReadOnlyNeverWrites(t *testing.T) {
	var verbs []string
	h := mem()
	rt := func(ctx context.Context, in []byte) (orchapi.AdapterResponse, error) {
		var req orchapi.AdapterRequest
		if json.Unmarshal(in, &req) == nil {
			verbs = append(verbs, req.Verb)
			if req.Verb != "" && req.Config["dir"] != "/x" {
				t.Errorf("config not sent with %s: %v", req.Verb, req.Config)
			}
		}
		return inProcess(h)(ctx, in)
	}
	res := Run(context.Background(), rt, Options{ID: "mm:1", Config: map[string]string{"dir": "/x"}})
	if Failed(res) > 0 {
		t.Fatalf("read-only:\n%s", Format(res))
	}
	for _, v := range verbs {
		for _, w := range orchapi.WriteVerbs {
			if v == w {
				t.Fatalf("read-only run sent %q (all: %v)", v, verbs)
			}
		}
	}
	if len(verbs) < 4 {
		t.Fatalf("too few calls: %v", verbs)
	}
	// The guard itself: a write call in a read-only run fails without sending.
	r := &run{ctx: context.Background(), rt: rt}
	n := len(verbs)
	if _, err := r.call(orchapi.VerbClaim, "mm:1", orchapi.AdapterClaimArgs{Holder: HolderA}, nil); err == nil || len(verbs) != n {
		t.Fatalf("guard let a write through: %v %v", err, verbs[n:])
	}
}

func TestBadAdapters(t *testing.T) {
	// Missing verbs: describe fails and nothing else runs.
	h := mem()
	delete(h, orchapi.VerbSetStatus)
	h[orchapi.VerbDescribe] = func(context.Context, orchapi.AdapterRequest) (any, error) {
		return orchapi.AdapterDescribe{Spec: api.Spec, Name: "mem", Prefix: "mm", Verbs: []string{"describe"}}, nil
	}
	if res := Run(context.Background(), inProcess(h), Options{}); Failed(res) != 1 || len(res) != 1 {
		t.Fatalf("missing verbs:\n%s", Format(res))
	}
	// Claims nobody can lose and items with the wrong prefix.
	h = mem()
	h[orchapi.VerbClaim] = func(context.Context, orchapi.AdapterRequest) (any, error) {
		return orchapi.WorkItem{ID: "mm:1", Source: "mm", Title: "one", Status: orchapi.WorkClaimed}, nil
	}
	h[orchapi.VerbList] = func(context.Context, orchapi.AdapterRequest) (any, error) {
		return orchapi.WorkList{Items: []orchapi.WorkItem{{ID: "xx:1", Source: "mm", Title: "t", Status: "doing"}}}, nil
	}
	out := Format(Run(context.Background(), inProcess(h), Options{ID: "mm:1", Write: true}))
	for _, want := range []string{"FAIL  list returns valid items", "FAIL  claim by another holder answers conflict"} {
		if !strings.Contains(out, want) {
			t.Fatalf("want %q in:\n%s", want, out)
		}
	}
}

func TestExecutable(t *testing.T) {
	p := filepath.Join(t.TempDir(), "zm-work-crash")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := Run(context.Background(), Exec(p), Options{})
	if len(res) != 1 || res[0].OK {
		t.Fatalf("crashing adapter: %+v", res)
	}
}

// describe.env is checked: a declaration zm cannot honour fails the run, a
// good one (or none) passes the allowlist check.
func TestDescribeEnv(t *testing.T) {
	with := func(e *orchapi.AdapterEnv) map[string]adapter.Handler {
		h := mem()
		h[orchapi.VerbDescribe] = func(context.Context, orchapi.AdapterRequest) (any, error) {
			return orchapi.AdapterDescribe{Spec: api.Spec, Name: "mm", Prefix: "mm", Verbs: orchapi.RequiredVerbs, Env: e}, nil
		}
		return h
	}
	result := func(res []Result) Result {
		for _, r := range res {
			if r.Name == CheckEnvName {
				return r
			}
		}
		t.Fatalf("no %q check:\n%s", CheckEnvName, Format(res))
		return Result{}
	}
	for _, e := range []*orchapi.AdapterEnv{nil, {Vars: []string{orchapi.EnvDataDir, "JIRA_URL"}, Secrets: []orchapi.AdapterSecret{{Name: "TOK", From: orchapi.SecretSlot}}}} {
		if r := result(Run(context.Background(), inProcess(with(e)), Options{ID: "mm:1"})); !r.OK {
			t.Fatalf("%+v: %s", e, r.Detail)
		}
	}
	bad := &orchapi.AdapterEnv{Vars: []string{"TOK"}, Secrets: []orchapi.AdapterSecret{{Name: "TOK", From: orchapi.SecretSlot}}}
	if r := result(Run(context.Background(), inProcess(with(bad)), Options{ID: "mm:1"})); r.OK || !strings.Contains(r.Detail, "also a plain variable") {
		t.Fatalf("a secret also declared plain: %+v", r)
	}
	shell := &orchapi.AdapterEnv{Vars: []string{"GH_TOKEN"}}
	if r := result(Run(context.Background(), inProcess(with(shell)), Options{ID: "mm:1"})); r.OK || !strings.Contains(r.Detail, "declare it in secrets") {
		t.Fatalf("a token from the shell: %+v", r)
	}
}
