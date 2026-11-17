package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
)

func TestSandboxPreviewCallerProxy(t *testing.T) {
	var calls []computeapi.PreviewRequest
	var domains []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		if api.GetHeader(r.Header, api.HeaderAs) != "mini/aaaaaaaa" {
			t.Error("lost caller")
		}
		switch r.URL.Path {
		case "/v0/tunnel/preview-domain":
			if r.Method != "POST" {
				t.Errorf("configure method %s", r.Method)
			}
			var req api.TunnelPreviewDomainRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			domains = append(domains, req.Domain)
			api.WriteJSON(w, 200, api.TunnelStatus{})
		case "/v0/run_targets":
			api.WriteJSON(w, 200, computeapi.RunTargetList{Targets: []computeapi.RunTargetView{{RunTarget: computeapi.RunTarget{Name: "linux", Kind: "box"}}, {RunTarget: computeapi.RunTarget{Name: "scratch", Kind: "db"}}}})
		case "/v0/run_targets/linux/handles/b2/previews":
			if r.Method == "GET" {
				api.WriteJSON(w, 200, computeapi.PreviewList{Previews: []computeapi.Preview{}})
				return
			}
			var req computeapi.PreviewRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			calls = append(calls, req)
			if req.Revoke != "" {
				api.WriteJSON(w, 200, computeapi.PreviewList{})
				return
			}
			if !req.Public {
				api.WriteError(w, api.Errorf(api.CodeBadRequest, "public:true required"))
				return
			}
			api.WriteJSON(w, 200, computeapi.Preview{ID: "pv1", Port: req.Port, URL: "https://pv1.example.com"})
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	t.Setenv("ZM_HOME", t.TempDir())
	t.Setenv("ZM_HOST", server.URL)
	t.Setenv("ZM_TOKEN", "")
	t.Setenv("ZM_PROJECT", "")
	oldAs, oldProject, oldStdout := flagAs, flagProject, os.Stdout
	t.Cleanup(func() { flagAs, flagProject, os.Stdout = oldAs, oldProject, oldStdout })
	flagAs, flagProject = "mini/aaaaaaaa", ""
	for _, tc := range []struct {
		args []string
		fail bool
		want string
	}{
		{[]string{"configure", "preview.example.com"}, false, ""},
		{[]string{"configure", ""}, false, ""},
		{[]string{"publish", "box:linux/b2", "8080"}, true, ""},
		{[]string{"publish", "box:linux/b2", "8080", "--public"}, false, "pv1.example.com"},
		{[]string{"ls", "box:linux/b2"}, false, "previews"},
		{[]string{"revoke", "box:linux/b2", "pv1"}, false, `"revoked": true`},
		{[]string{"publish", "run:linux/r1", "8080", "--public"}, true, ""},
		{[]string{"ls", "box:scratch/d1"}, true, ""},
	} {
		path := filepath.Join(t.TempDir(), "output")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		os.Stdout = f
		cmd := sandboxPreviewCmd()
		cmd.SetArgs(tc.args)
		err = cmd.ExecuteContext(context.Background())
		os.Stdout = oldStdout
		f.Close()
		out, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if (err != nil) != tc.fail || !strings.Contains(string(out), tc.want) {
			t.Fatalf("%v: %v %s", tc.args, err, out)
		}
	}
	if len(domains) != 2 || domains[0] != "preview.example.com" || domains[1] != "" {
		t.Fatalf("configure domains: %+v", domains)
	}
	if len(calls) != 3 || calls[0].Public || !calls[1].Public || calls[1].Port != 8080 || calls[2].Revoke != "pv1" {
		t.Fatalf("preview requests: %+v", calls)
	}
}
