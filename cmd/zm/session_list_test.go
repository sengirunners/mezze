package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// Exercise the real ls command: typed pages and JSON must not erase the
// union's partial/stale status, and labels must not query live authorities.
func TestSessionListPreservesPageAndBoundsLabels(t *testing.T) {
	oldJSON, oldAs, oldProject := flagJSON, flagAs, flagProject
	oldStdout := os.Stdout
	t.Cleanup(func() { flagJSON, flagAs, flagProject = oldJSON, oldAs, oldProject; os.Stdout = oldStdout })
	flagAs, flagProject = "", ""
	t.Setenv("ZM_HOME", t.TempDir())
	t.Setenv("ZM_PROJECT", "")
	t.Setenv("ZM_TOKEN", "")
	seen := "2026-11-04T12:00:00Z"
	// Raw authority envelope avoids constructing the expected answer with
	// the same new API fields whose preservation this regression checks.
	page := `{"sessions":[{"id":"aaaa1111","address":"mini/aaaa1111","machine":"mini","project":"PLOCAL","agent":"codex","state":"idle","note":"original note","title":"visible local row","seen":"` + seen + `"},{"id":"bbbb2222","address":"other/bbbb2222","machine":"other","project":"PJOINED","agent":"codex","state":"needs_human","title":"visible joined row"}],"next":"opaque-next-page","skipped":[{"project":"PMISSING","source":"authority","error":"authority unavailable"}],"behind":[{"project":"PJOINED","source":"authority","state":"stale","synced":"2026-11-04T11:59:00Z","error":"refresh pending"}]}`
	for _, tc := range []struct {
		name                string
		machine, labelsFail bool
	}{
		{name: "human"}, {name: "human-label-failure", labelsFail: true}, {name: "json", machine: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flagJSON = tc.machine
			release := make(chan struct{})
			discovery := make(chan struct{}, 1)
			var mu sync.Mutex
			var metadata []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				switch r.URL.Path {
				case "/v0/sessions":
					if r.URL.Query().Get("all") != "1" || r.URL.Query().Get("agent") != "codex" || r.URL.Query().Get("state") != "idle" {
						t.Errorf("session query changed: %s", r.URL)
					}
					w.Header().Set("Content-Type", "application/json")
					w.Write([]byte(page))
				case "/v0/projects":
					mu.Lock()
					metadata = append(metadata, r.URL.String())
					mu.Unlock()
					if r.URL.Query().Get("local") != "1" {
						select {
						case discovery <- struct{}{}:
						default:
						}
						select {
						case <-release:
						case <-r.Context().Done():
						}
						return
					}
					if tc.labelsFail {
						api.WriteError(w, api.Errorf(api.CodeUnavailable, "metadata unavailable"))
						return
					}
					api.WriteJSON(w, 200, api.ProjectList{Projects: []api.Project{{ID: "PLOCAL", Slug: "local-project"}}})
				case "/v0/team/remotes":
					mu.Lock()
					metadata = append(metadata, r.URL.String())
					mu.Unlock()
					if tc.labelsFail {
						api.WriteError(w, api.Errorf(api.CodeForbidden, "not local"))
						return
					}
					api.WriteJSON(w, 200, api.RemoteList{Remotes: []api.Remote{
						{Project: api.ProjectRef{ID: "PJOINED", Slug: "joined-project"}, State: api.RemoteOK},
						{Project: api.ProjectRef{ID: "PMISSING", Slug: "revoked-label"}, State: api.RemoteRevoked},
					}})
				default:
					t.Errorf("unexpected lookup %s", r.URL)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			defer close(release)
			t.Setenv("ZM_HOST", server.URL)
			path := filepath.Join(t.TempDir(), "stdout")
			output, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			os.Stdout = output
			cmd := lsCmd()
			cmd.SetArgs([]string{"--all", "--agent", "codex", "--state", "idle"})
			var warnings bytes.Buffer
			cmd.SetErr(&warnings)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err = cmd.ExecuteContext(ctx)
			os.Stdout = oldStdout
			output.Close()
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-discovery:
				t.Fatal("session display waited on blocked default project discovery")
			default:
			}
			mu.Lock()
			calls := append([]string(nil), metadata...)
			mu.Unlock()
			if tc.machine {
				if len(calls) != 0 {
					t.Fatalf("JSON performed optional metadata I/O: %v", calls)
				}
				var got, want map[string]json.RawMessage
				if err := json.Unmarshal(raw, &got); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal([]byte(page), &want); err != nil {
					t.Fatal(err)
				}
				if len(got) != 4 {
					t.Fatalf("JSON envelope changed: %s", raw)
				}
				for _, key := range []string{"next", "skipped", "behind"} {
					var actual, expected any
					if err := json.Unmarshal(got[key], &actual); err != nil {
						t.Fatalf("missing %s: %s", key, raw)
					}
					if err := json.Unmarshal(want[key], &expected); err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(actual, expected) {
						t.Fatalf("JSON lost %s: %s", key, raw)
					}
				}
				var actualRows, expectedRows []orchapi.Session
				if err := json.Unmarshal(got["sessions"], &actualRows); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(want["sessions"], &expectedRows); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(actualRows, expectedRows) {
					t.Fatalf("JSON altered session state, seen or rows: %s", raw)
				}

				if warnings.Len() != 0 {
					t.Fatalf("JSON metadata should remain in the page: %s", warnings.String())
				}
				return
			}
			if len(calls) != 2 {
				t.Fatalf("bounded local label lookups: %v", calls)
			}
			if !strings.Contains(string(raw), "visible local row") || !strings.Contains(string(raw), "visible joined row") || !strings.Contains(string(raw), "original note") {
				t.Fatalf("partial list lost visible rows: %s", raw)
			}
			local, joined := "local-project", "joined-project"
			if tc.labelsFail {
				local, joined = "PLOCAL", "PJOINED"
			}
			if !strings.Contains(string(raw), local) || !strings.Contains(string(raw), joined) {
				t.Fatalf("project labels/fallback missing: %s", raw)
			}
			if !strings.Contains(warnings.String(), "PMISSING/authority unavailable; session list is incomplete") || !strings.Contains(warnings.String(), joined+"/authority session list is not current (stale;") || !strings.Contains(warnings.String(), "refresh pending") || strings.Contains(warnings.String(), "revoked-label") {
				t.Fatalf("partial/stale warnings lost or misleading: %s", warnings.String())
			}
		})
	}
}
