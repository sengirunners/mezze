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
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/spf13/cobra"
)

func TestWorkDisplayUsesLocalLabels(t *testing.T) {
	oldJSON, oldAs, oldProject := flagJSON, flagAs, flagProject
	oldStdout := os.Stdout
	t.Cleanup(func() { flagJSON, flagAs, flagProject = oldJSON, oldAs, oldProject; os.Stdout = oldStdout })
	flagAs, flagProject = "", ""
	t.Setenv("ZM_HOME", t.TempDir())
	t.Setenv("ZM_PROJECT", "")
	t.Setenv("ZM_TOKEN", "")
	items := []orchapi.WorkItem{
		{ID: "local:one", Project: "PLOCAL", Title: "local work", Status: orchapi.WorkOpen},
		{ID: "bd:two", Project: "PJOINED", Title: "joined work", Status: orchapi.WorkOpen},
		{ID: "bd:unknown", Project: "PUNKNOWN", Title: "unknown project", Status: orchapi.WorkOpen},
		{ID: "bd:revoked", Project: "PREVOKED", Title: "revoked label", Status: orchapi.WorkOpen},
	}
	for _, machine := range []bool{false, true} {
		for _, show := range []bool{false, true} {
			name := "list"
			if show {
				name = "show"
			}
			if machine {
				name += "/json"
			}
			t.Run(name, func(t *testing.T) {
				flagJSON = machine
				var mu sync.Mutex
				var metadata []string
				discovery := make(chan struct{}, 1)
				release := make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set(api.HeaderProtocol, api.Protocol)
					switch r.URL.Path {
					case "/v0/work":
						api.WriteJSON(w, 200, orchapi.WorkList{Items: items,
							Skipped: []api.WorkSkipped{{Project: "PJOINED", Source: "bd", Error: "tracker unavailable"}},
							Behind:  []orchapi.WorkBehind{{Project: "PLOCAL", Source: "bd", State: orchapi.SyncLoading}}})
					case "/v0/work/bd:two":
						api.WriteJSON(w, 200, items[1])
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
						api.WriteJSON(w, 200, api.ProjectList{Projects: []api.Project{{ID: "PLOCAL", Slug: "local-project"}}})
					case "/v0/team/remotes":
						mu.Lock()
						metadata = append(metadata, r.URL.String())
						mu.Unlock()
						api.WriteJSON(w, 200, api.RemoteList{Remotes: []api.Remote{
							{Project: api.ProjectRef{ID: "PJOINED", Slug: "joined-project"}, State: api.RemoteOK},
							{Project: api.ProjectRef{ID: "PREVOKED", Slug: "must-not-use"}, State: api.RemoteRevoked},
							{Project: api.ProjectRef{ID: "PPURGED", Slug: "purged"}, State: api.RemotePurged},
						}})
					default:
						t.Errorf("unexpected %s", r.URL)
						w.WriteHeader(404)
					}
				}))
				defer server.Close()
				defer close(release)
				t.Setenv("ZM_HOST", server.URL)
				file := filepath.Join(t.TempDir(), "stdout")
				out, err := os.Create(file)
				if err != nil {
					t.Fatal(err)
				}
				os.Stdout = out
				var cmd *cobra.Command
				if show {
					cmd = workShowCmd()
					cmd.SetArgs([]string{"bd:two"})
				} else {
					cmd = workListCmd()
				}
				var warnings bytes.Buffer
				cmd.SetErr(&warnings)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				err = cmd.ExecuteContext(ctx)
				os.Stdout = oldStdout
				out.Close()
				if err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-discovery:
					t.Fatal("display asked joined authorities through full project discovery")
				default:
				}
				mu.Lock()
				calls := append([]string(nil), metadata...)
				mu.Unlock()
				if machine {
					if len(calls) != 0 {
						t.Fatalf("JSON performed display I/O: %v", calls)
					}
					want := any(items)
					got := any(&[]orchapi.WorkItem{})
					if show {
						want = items[1]
						got = &orchapi.WorkItem{}
					}
					if err := json.Unmarshal(raw, got); err != nil {
						t.Fatal(err)
					}
					expected, _ := json.Marshal(want)
					var actual any
					json.Unmarshal(raw, &actual)
					normalized, _ := json.Marshal(actual)
					var expectedAny any
					json.Unmarshal(expected, &expectedAny)
					canonical, _ := json.Marshal(expectedAny)
					if !bytes.Equal(normalized, canonical) {
						t.Fatalf("JSON shape/content changed: %s != %s", raw, expected)
					}
				} else {
					if len(calls) != 2 {
						t.Fatalf("local metadata calls: %v", calls)
					}
					if !strings.Contains(string(raw), "joined-project/bd:two") {
						t.Fatalf("joined label missing: %s", raw)
					}
					if !show && (!strings.Contains(string(raw), "local-project/local:one") || !strings.Contains(string(raw), "bd:unknown") || strings.Contains(string(raw), "must-not-use")) {
						t.Fatalf("local/unknown/revoked labels: %s", raw)
					}
				}
				if !show {
					project := "joined-project"
					local := "local-project"
					if machine {
						project, local = "PJOINED", "PLOCAL"
					}
					if !strings.Contains(warnings.String(), project+"/bd unavailable") || !strings.Contains(warnings.String(), local+"/bd is still loading") {
						t.Fatalf("lost skipped/behind warnings: %s", warnings.String())
					}
				}
			})
		}
	}
}

func TestDisplayLabelsPartialFailureAndSharedDeadline(t *testing.T) {
	for _, fail := range []string{"local", "remotes", "both", "neither"} {
		t.Run(fail, func(t *testing.T) {
			var deadlines []time.Time
			c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				deadline, ok := r.Context().Deadline()
				if !ok {
					t.Error("display request has no deadline")
				}
				deadlines = append(deadlines, deadline)
				switch r.URL.Path {
				case "/v0/projects":
					if r.URL.Query().Get("local") != "1" {
						t.Error("fell back to remote discovery")
					}
					if fail == "local" || fail == "both" {
						api.WriteError(w, api.Errorf(api.CodeUnavailable, "local metadata missing"))
						return
					}
					api.WriteJSON(w, 200, api.ProjectList{Projects: []api.Project{{ID: "L", Slug: "local"}}})
				case "/v0/team/remotes":
					if fail == "remotes" || fail == "both" {
						api.WriteError(w, api.Errorf(api.CodeForbidden, "not local"))
						return
					}
					api.WriteJSON(w, 200, api.RemoteList{Remotes: []api.Remote{{Project: api.ProjectRef{ID: "R", Slug: "remote"}, State: api.RemoteOK}}})
				default:
					t.Errorf("unexpected metadata fallback %s", r.URL)
				}
			}))
			labels := slugs(context.Background(), c)
			if len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) {
				t.Fatalf("display calls do not share one deadline: %v", deadlines)
			}
			if (labels["L"] == "local") != (fail != "local" && fail != "both") || (labels["R"] == "remote") != (fail != "remotes" && fail != "both") {
				t.Fatalf("partial labels %v (%s)", labels, fail)
			}
			if shown(labels, "unknown", "bd:item") != "bd:item" || slugOf(labels, "unknown") != "unknown" {
				t.Fatal("unknown ID fallback changed")
			}
		})
	}
}

// An older daemon may ignore local=1 and wait on discovery anyway. Its
// cancellation must consume the single display budget, never cancel work.
func TestDisplayLabelsOldServerCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, canceled := false, false
	var firstDeadline time.Time
	c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		if r.URL.Path == "/v0/projects" {
			entered = true
			var ok bool
			firstDeadline, ok = r.Context().Deadline()
			if !ok {
				t.Error("old-server discovery is unbounded")
				return
			}
			<-r.Context().Done()
			canceled = true
			return
		}
		if deadline, ok := r.Context().Deadline(); !ok || !deadline.Equal(firstDeadline) {
			t.Error("second display call received a fresh budget")
		}
	}))
	labels := slugs(ctx, c)
	if !entered || !canceled || len(labels) != 0 || ctx.Err() != nil {
		t.Fatalf("old daemon display did not cancel independently: entered=%v canceled=%v labels=%v workErr=%v", entered, canceled, labels, ctx.Err())
	}
}
