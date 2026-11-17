package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// `zm sandbox usage limit` reads GET /v0/usage/limits and sets with PUT
// /v0/usage/limits/{area}, as the caller (zm's client); the daemon's
// refusal (not an owner or admin of the whole project) is the answer.
func TestSandboxUsageLimit(t *testing.T) {
	var got []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		b, _ := io.ReadAll(r.Body)
		got = append(got, r.Method+" "+r.URL.Path+" "+strings.TrimSpace(string(b)))
		switch {
		case r.Method == "GET" && r.URL.Path == "/v0/usage/limits":
			api.WriteJSON(w, 200, computeapi.AreaMinuteLimits{Project: "acme", Limits: []computeapi.AreaMinuteLimit{{Area: "ci", Minutes: 600, Period: "month"}}})
		case r.Method == "PUT" && r.URL.Path == "/v0/usage/limits/ci":
			api.WriteJSON(w, 200, computeapi.AreaMinuteLimits{Project: "acme", Limits: []computeapi.AreaMinuteLimit{{Area: "ci", Minutes: 90, Period: "week"}}})
		case r.Method == "PUT" && r.URL.Path == "/v0/usage/limits/team1":
			api.WriteError(w, api.Errorf(api.CodeForbidden, "not an owner or admin of the whole project"))
		default:
			api.WriteError(w, api.ErrNotImplemented(r.URL.Path))
		}
	})
	c := client.WithHandler(h)
	var out bytes.Buffer
	if err := sandboxUsageLimit(context.Background(), c, "", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "ci 600 minutes per month\n" {
		t.Errorf("list: %q", out.String())
	}
	out.Reset()
	if err := sandboxUsageLimit(context.Background(), c, "ci", &computeapi.SetAreaMinuteLimitRequest{Minutes: 90, Period: "week"}, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "ci 90 minutes per week\n" {
		t.Errorf("set: %q", out.String())
	}
	if want := `PUT /v0/usage/limits/ci {"minutes":90,"period":"week"}`; len(got) != 2 || got[1] != want {
		t.Errorf("requests: %q, want %s second", got, want)
	}
	err := sandboxUsageLimit(context.Background(), c, "team1", &computeapi.SetAreaMinuteLimitRequest{Minutes: 5}, io.Discard)
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeForbidden {
		t.Errorf("a refused set: %v", err)
	}
}

// The command is mounted under zm sandbox usage, and refuses wrong
// arguments with exit 2 before asking the daemon.
func TestSandboxUsageLimitArgs(t *testing.T) {
	limit, _, err := rootCmd.Find([]string{"sandbox", "usage", "limit"})
	if err != nil || limit.Name() != "limit" {
		t.Fatalf("zm sandbox usage limit: %v %v", limit, err)
	}
	var ew exitWith
	for _, args := range [][]string{{"ci"}, {"ci", "1", "2"}} {
		if err := limit.Args(limit, args); !errors.As(err, &ew) || ew.code != 2 {
			t.Errorf("%q: %v, want exit 2", args, err)
		}
	}
	for _, args := range [][]string{{"ci", "ten"}, {"--per", "week"}} {
		cmd := sandboxUsageLimitCmd()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.Execute(); !errors.As(err, &ew) || ew.code != 2 {
			t.Errorf("%q: %v, want exit 2", args, err)
		}
	}
}

// The usage module's refusal at the limit names Potluck's command; zm
// says it as zm's, in the CLI's report (text and --json) and in MCP.
func TestUsageLimitHintNamesZm(t *testing.T) {
	e := api.Errorf(api.CodeForbidden, "area ci has used 600 of its 600 sandbox minutes this month").
		WithHint("a project owner or admin can raise the limit: potluck usage limit ci <minutes> --per month")
	buf := captureStderr(t)
	old := flagJSON
	t.Cleanup(func() { flagJSON = old })
	for _, json := range []bool{false, true} {
		flagJSON = json
		if code := report(e); code != 1 {
			t.Errorf("json=%v: exit %d", json, code)
		}
	}
	const want = "zm sandbox usage limit ci <minutes> --per month"
	if !buf.waitFor(`"hint":"a project owner or admin can raise the limit: zm sandbox usage limit ci \u003cminutes\u003e --per month"`) {
		t.Fatalf("--json: %s", buf.String())
	}
	if text := buf.String(); !strings.Contains(text, "\n  a project owner or admin can raise the limit: "+want) || strings.Contains(text, "potluck usage") {
		t.Errorf("report: %s", text)
	}
	if !strings.Contains(e.Hint, "potluck usage limit") {
		t.Errorf("report changed the daemon's error: %s", e.Hint)
	}
}
