package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

func TestServe(t *testing.T) {
	h := map[string]Handler{
		orchapi.VerbDescribe: func(context.Context, orchapi.AdapterRequest) (any, error) {
			return orchapi.AdapterDescribe{Spec: api.Spec, Name: "fake", Prefix: "fk", Verbs: orchapi.RequiredVerbs}, nil
		},
		orchapi.VerbClaim: func(context.Context, orchapi.AdapterRequest) (any, error) {
			return nil, api.Errorf(api.CodeConflict, "held")
		},
	}
	run := func(in string) orchapi.AdapterResponse {
		var out bytes.Buffer
		if code := Serve(context.Background(), strings.NewReader(in), &out, h); code != 0 {
			t.Fatalf("exit %d", code)
		}
		var r orchapi.AdapterResponse
		if err := json.Unmarshal(out.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := run(`{"zm":"zm/v0","verb":"describe"}`); !r.OK || !strings.Contains(string(r.Result), `"prefix":"fk"`) {
		t.Fatalf("describe %+v", r)
	}
	if r := run(`{"zm":"zm/v0","verb":"claim","id":"fk:1"}`); r.OK || r.Error.Code != api.CodeConflict {
		t.Fatalf("claim %+v", r)
	}
	if r := run(`{"verb":"watch"}`); r.OK || r.Error.Code != api.CodeNotImplemented {
		t.Fatalf("watch %+v", r)
	}
	if r := run(`nope`); r.OK || r.Error.Code != api.CodeBadRequest {
		t.Fatalf("bad %+v", r)
	}
}

func script(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCallTypedErrors(t *testing.T) {
	ctx := context.Background()
	ok := script(t, "zm-work-ok", `cat >/dev/null; echo '{"ok":true,"result":{"id":"fk:1","title":"t","status":"open"}}'`)
	var it orchapi.WorkItem
	if err := Call(ctx, ok, orchapi.AdapterRequest{Verb: orchapi.VerbGet, ID: "fk:1"}, &it); err != nil || it.ID != "fk:1" {
		t.Fatalf("ok %+v %v", it, err)
	}
	refuse := script(t, "zm-work-refuse", `cat >/dev/null; echo '{"ok":false,"error":{"code":"teapot","message":"no","data":{"secret":1}}}'`)
	err := Call(ctx, refuse, orchapi.AdapterRequest{Verb: orchapi.VerbGet}, nil)
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeInternal || e.Data != nil {
		t.Fatalf("unknown code / data relayed: %#v", err)
	}
	for name, body := range map[string]string{
		"crash":   `echo boom >&2; exit 3`,
		"badjson": `cat >/dev/null; echo not json`,
		"silent":  `cat >/dev/null`,
	} {
		p := script(t, "zm-work-"+name, body)
		if err := Call(ctx, p, orchapi.AdapterRequest{Verb: orchapi.VerbGet}, nil); !api.IsCode(err, api.CodeUnavailable) || strings.Contains(err.Error(), filepath.Dir(p)) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	old := Timeout
	Timeout = 200 * time.Millisecond
	defer func() { Timeout = old }()
	slow := script(t, "zm-work-slow", `exec sleep 5`)
	start := time.Now()
	if err := Call(ctx, slow, orchapi.AdapterRequest{Verb: orchapi.VerbGet}, nil); !api.IsCode(err, api.CodeUnavailable) || time.Since(start) > 4*time.Second {
		t.Fatalf("timeout: %v after %s", err, time.Since(start))
	}
}

func TestProtocolAndFind(t *testing.T) {
	var out bytes.Buffer
	Serve(context.Background(), strings.NewReader(`{"zm":"zm/v9","verb":"describe"}`), &out, map[string]Handler{
		orchapi.VerbDescribe: func(context.Context, orchapi.AdapterRequest) (any, error) { return "x", nil },
	})
	if !strings.Contains(out.String(), `"bad_request"`) {
		t.Fatalf("other major accepted: %s", out.String())
	}
	if !SameMajor("zm/v0") || !SameMajor("zm/v0.3") || SameMajor("zm/v1") || SameMajor("") {
		t.Fatal("SameMajor")
	}
	p := script(t, "zm-work-zz", "true")
	t.Setenv("PATH", filepath.Dir(p))
	if got, err := Find("zz", nil); err != nil || got != p {
		t.Fatalf("find on PATH %q %v", got, err)
	}
	extra := script(t, "zm-work-yy", "true")
	if got, err := Find("yy", []string{"/nope/zm-work-yy", extra}); err != nil || got != extra {
		t.Fatalf("find in work_adapters %q %v", got, err)
	}
	if _, err := Find("missing", nil); err == nil {
		t.Fatal("found a missing adapter")
	}
}

// Serve bounds a request below the daemon's Timeout, so an adapter kills its
// own tracker CLI before the daemon kills the adapter.
func TestServeDeadline(t *testing.T) {
	if ServeTimeout >= Timeout {
		t.Fatalf("ServeTimeout %s must be under Timeout %s", ServeTimeout, Timeout)
	}
	var left time.Duration
	h := map[string]Handler{orchapi.VerbGet: func(ctx context.Context, _ orchapi.AdapterRequest) (any, error) {
		d, ok := ctx.Deadline()
		if !ok {
			return nil, errors.New("no deadline")
		}
		left = time.Until(d)
		return struct{}{}, nil
	}}
	var out bytes.Buffer
	Serve(context.Background(), strings.NewReader(`{"verb":"get"}`), &out, h)
	if left <= 0 || left > ServeTimeout {
		t.Fatalf("handler deadline %s (out %s)", left, out.String())
	}
}

// setup answers in 120 s (SPEC 11): the daemon waits that long, and the
// adapter bounds it under that by the usual margin; other verbs keep 30 s.
func TestSetupTimeout(t *testing.T) {
	if TimeoutFor(orchapi.VerbSetup) != SetupTimeout || TimeoutFor(orchapi.VerbList) != Timeout {
		t.Fatalf("TimeoutFor: setup %s, list %s", TimeoutFor(orchapi.VerbSetup), TimeoutFor(orchapi.VerbList))
	}
	if d := serveTimeoutFor(orchapi.VerbSetup); d >= SetupTimeout || d <= Timeout {
		t.Fatalf("serve bound for setup %s", d)
	}
	if serveTimeoutFor(orchapi.VerbGet) != ServeTimeout {
		t.Fatal("serve bound for get changed")
	}
}
