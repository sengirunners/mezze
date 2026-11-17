package mcp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

func TestSandboxPreviewCallerProxy(t *testing.T) {
	var calls []computeapi.PreviewRequest
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/run_targets/linux/handles/b2/previews" {
			(&fakeDaemon{}).ServeHTTP(w, r)
			return
		}
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		if api.GetHeader(r.Header, api.HeaderAs) != "mini/aaaaaaaa" {
			t.Error("preview lost caller identity")
		}
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
	})
	c := client.WithHandler(h)
	c.As = "mini/aaaaaaaa"
	cs := connect(t, c, Options{Sandboxes: true})
	for _, tc := range []struct {
		name string
		args map[string]any
		fail bool
		want string
	}{
		{"sandbox_preview_publish", map[string]any{"handle": "box:linux/b2", "port": 8080}, true, "public"},
		{"sandbox_preview_publish", map[string]any{"handle": "box:linux/b2", "port": 8080, "public": false}, true, "public:true"},
		{"sandbox_preview_publish", map[string]any{"handle": "box:linux/b2", "port": 8080, "public": true}, false, "pv1.example.com"},
		{"sandbox_preview_list", map[string]any{"handle": "box:linux/b2"}, false, "previews"},
		{"sandbox_preview_revoke", map[string]any{"handle": "box:linux/b2", "preview": "pv1"}, false, `"revoked": true`},
		{"sandbox_preview_publish", map[string]any{"handle": "run:linux/r1", "port": 8080, "public": true}, true, ""},
		{"sandbox_preview_list", map[string]any{"handle": "box:scratch/d1"}, true, ""},
	} {
		r, txt := callTool(t, cs, tc.name, tc.args)
		if r.IsError != tc.fail || !strings.Contains(txt, tc.want) {
			t.Fatalf("%s: error=%v %s", tc.name, r.IsError, txt)
		}
	}
	if len(calls) != 3 || calls[0].Public || !calls[1].Public || calls[1].Port != 8080 || calls[2].Revoke != "pv1" {
		t.Fatalf("preview requests: %+v", calls)
	}
}
