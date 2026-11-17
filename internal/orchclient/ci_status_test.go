package orchclient_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// Go through the exported read model: a repo with a refused job and an old
// queued run must preserve both reasons without changing the caller or GitHub.
func TestCIStatusReposAndUnseenJobs(t *testing.T) {
	for _, mode := range []string{"normal", "github-down", "machines-down", "mac-asleep", "truncated", "jobs-page-two"} {
		t.Run(mode, func(t *testing.T) {
			c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer scoped-token" || api.GetHeader(r.Header, api.HeaderAs) != "mini/agent" {
					t.Errorf("lost read-only caller: %s %s %v", r.Method, r.URL, r.Header)
				}
				respond := func(s string) { fmt.Fprint(w, s) }
				switch r.URL.Path {
				case "/v0/runners/setup":
					respond(`{"pool":"home","label":"my-mac","job_ttl_s":5400,"app":{"id":1},"default_size":"c2m4"}`)
				case "/v0/runners/jobs":
					if r.URL.Query().Get("limit") != "500" {
						t.Error("short broker read")
					}
					respond(`{"jobs":[{"job_id":2,"repo":"Acme/Worm","state":"ignored","why":"Fork PRs are refused","updated":"2026-11-12T02:00:00Z"},{"job_id":1,"repo":"Acme/Worm","state":"waiting","why":"Your Mac is asleep","updated":"2026-11-12T01:00:00Z"}]}`)
				case "/v0/run_targets":
					if api.GetHeader(r.Header, api.HeaderProject) != "home" || r.URL.Query().Get("describe") != "1" {
						t.Error("used shell project for CI machines")
					}
					if mode == "machines-down" {
						api.WriteError(w, api.Errorf(api.CodeUnavailable, "offline"))
						return
					}
					fmt.Fprintf(w, `{"targets":[{"name":"linux","kind":"box","host_class":"trusted","describe":{"available":true,"capabilities":["github-jit","github-jit-guard"],"size":{"name":"c2m4","os":"linux"}}},{"name":"mac","kind":"box","host_class":"trusted","describe":{"available":%t,"why":"asleep","capabilities":["github-jit","github-jit-guard"],"size":{"name":"c4m16","os":"darwin"}}}]}`, mode != "mac-asleep")
				case "/v0/runners/repos":
					// acme/empty is mapped but not connected.
					respond(`{"repos":[{"repo":"acme/worm","installation_id":7,"by":"ana","visibility":"private"}]}`)
				case "/v0/github/repos":
					respond(`{"repositories":[{"project":"P","repo":"acme/worm"},{"project":"Q","repo":"ACME/WORM"},{"project":"P","repo":"acme/empty"}]}`)
				case "/v0/github/data":
					q := r.URL.Query()
					if q.Get("project") != "P" {
						t.Errorf("mapping was not retained: %s", r.URL)
					}
					if mode == "github-down" {
						respond(`{"stale":true,"error":{"message":"Sign in to GitHub"}}`)
						return
					}
					payload := `{"total_count":0,"workflow_runs":[]}`
					if q.Get("resource") == "runs" && q.Get("repo") == "acme/worm" && q.Get("status") == "queued" {
						// It need not be one of the most recent unfiltered runs.
						payload = `{"total_count":1,"workflow_runs":[{"id":10,"status":"queued"}]}`
						if mode == "truncated" {
							payload = `{"total_count":301,"workflow_runs":[]}`
						}
					}
					if q.Get("resource") == "jobs" {
						if q.Get("run") != "10" {
							t.Error("lost run")
						}
						payload = `{"total_count":4,"jobs":[{"id":2,"status":"queued","labels":["my-mac"]},{"id":3,"name":"native","status":"queued","labels":["self-hosted","macos","my-mac:c4m16"],"html_url":"https://github.com/acme/worm/actions/runs/10/job/3","created_at":"2026-11-12T03:00:00Z"},{"id":4,"status":"queued","labels":["macos-15"]},{"id":5,"status":"completed","labels":["my-mac"]}]}`
						if mode == "jobs-page-two" && q.Get("page") == "1" {
							payload = `{"total_count":101,"jobs":[]}`
						}
					}
					api.WriteJSON(w, 200, map[string]any{"payload": json.RawMessage(payload)})
				default:
					t.Errorf("unexpected call %s", r.URL)
					w.WriteHeader(500)
				}
			}))
			c.As, c.Token, c.Project = "mini/agent", "scoped-token", "shell-project"
			st, err := orchclient.CIStatus(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			if c.Project != "shell-project" {
				t.Error("mutated shared client")
			}
			if len(st.Repos) != 2 || st.Repos[0].Repo != "acme/empty" {
				t.Fatalf("not one row per repo: %+v", st)
			}
			r := st.Repos[1]
			if empty := strings.Join(st.Repos[0].Issues, "; "); !strings.Contains(empty, "Not connected in the pool areas you can read, so its jobs are not served: zm runners allow acme/empty") ||
				strings.Contains(strings.Join(r.Issues, "; "), "onnected") {
				t.Errorf("connected repos: %+v", st.Repos)
			}
			if r.JobLimitS != 5400 {
				t.Errorf("limit lost: %+v", r)
			}
			if mode == "machines-down" {
				if r.RunsOn != "" || !st.Partial || len(st.Issues) == 0 {
					t.Fatalf("invented serving machine: %+v", st)
				}
				for _, repo := range st.Repos {
					issues := strings.Join(repo.Issues, "; ")
					if !repo.Partial || !strings.Contains(issues, "unavailable") || strings.Contains(issues, "No serving machine") || strings.Contains(issues, "zm pool host") {
						t.Fatalf("machine outage mistaken for missing host: %+v", repo)
					}
				}
			} else if mode == "mac-asleep" {
				if r.RunsOn != `runs-on: ["self-hosted","linux","my-mac:c2m4"]` {
					t.Fatalf("sleeping Mac advertised: %s", r.RunsOn)
				}
			} else if r.RunsOn != `runs-on: ["self-hosted","macos","my-mac:c4m16"]` {
				t.Errorf("wrong Mac request: %s", r.RunsOn)
			}
			if mode == "github-down" || mode == "truncated" {
				if !st.Partial || !r.Partial || len(r.Issues) == 0 {
					t.Fatalf("false all-clear: %+v", st)
				}
			} else {
				if len(r.Jobs) != 3 || r.Jobs[0].ID != 3 || r.Jobs[0].State != "unseen" || !strings.Contains(r.Jobs[0].Why, "GitHub") {
					t.Fatalf("lost unseen job or duplicated known/foreign job: %+v", r)
				}
				if r.Summary != "1 waiting, 1 refused, 1 unseen" {
					t.Errorf("summary: %s", r.Summary)
				}
			}
			var reasons []string
			for _, j := range r.Jobs {
				reasons = append(reasons, j.Why)
			}
			if !strings.Contains(strings.Join(reasons, ";"), "Fork PRs are refused") || !strings.Contains(strings.Join(reasons, ";"), "Your Mac is asleep") {
				t.Fatalf("producer reasons lost: %+v", r)
			}
			b, err := json.Marshal(st)
			var roundtrip orchapi.CIStatus
			if err != nil || json.Unmarshal(b, &roundtrip) != nil || len(roundtrip.Repos) != 2 {
				t.Fatalf("JSON contract: %s %v", b, err)
			}
		})
	}
}

func TestCIStatusDoesNotGuessMacOrHideIncompleteHistory(t *testing.T) {
	for _, mode := range []string{"none", "public-mac", "override", "alias", "pool-only", "history-cap", "off", "read-refused", "binding-cap", "driver-cap", "uppercase-size", "invalid-size"} {
		t.Run(mode, func(t *testing.T) {
			c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				switch r.URL.Path {
				case "/v0/runners/setup":
					if mode == "read-refused" {
						api.WriteError(w, api.NotFound("setup", ""))
						return
					}
					if mode == "off" {
						fmt.Fprint(w, `{}`)
						return
					}
					sizes, defaultSize := map[string]string{}, ""
					if mode == "override" || mode == "pool-only" {
						sizes["c4m16"] = "linux"
						if mode == "override" {
							defaultSize = "c4m16"
						}
					} else if mode == "alias" {
						sizes["fast"] = "mac"
					}
					api.WriteJSON(w, 200, map[string]any{"pool": "home", "label": "home", "job_ttl_s": 5400, "app": map[string]int{"id": 1}, "sizes": sizes, "default_size": defaultSize})
				case "/v0/runners/jobs":
					var jobs []map[string]any
					if mode == "history-cap" {
						for i := 1; i <= 500; i++ {
							jobs = append(jobs, map[string]any{"job_id": i, "repo": "acme/app", "state": "waiting"})
						}
					}
					api.WriteJSON(w, 200, map[string]any{"jobs": jobs})
				case "/v0/github/repos":
					fmt.Fprint(w, `{"repositories":[{"project":"P","repo":"acme/app"}]}`)
				case "/v0/runners/repos":
					fmt.Fprint(w, `{"repos":[{"repo":"acme/app"}]}`)
				case "/v0/github/data":
					fmt.Fprint(w, `{"payload":{"total_count":0,"workflow_runs":[]}}`)
				case "/v0/run_targets":
					if mode == "override" || mode == "public-mac" || mode == "pool-only" {
						hostClass := "trusted"
						if mode == "public-mac" {
							hostClass = "public"
						}
						fmt.Fprintf(w, `{"targets":[{"name":"mac","kind":"box","host_class":%q,"describe":{"available":true,"capabilities":["github-jit","github-jit-guard"],"size":{"name":"c4m16","os":"darwin"}}},{"name":"linux","kind":"box","host_class":"trusted","describe":{"available":true,"capabilities":["github-jit","github-jit-guard"],"size":{"name":"c4m16","os":"linux"}}}]}`, hostClass)
					} else if mode == "alias" {
						// An alias selects this binding; trust, availability and runtime
						// plus startup fitness still come from its description.
						fmt.Fprint(w, `{"targets":[{"name":"mac","kind":"box","host_class":"trusted","ttl_max_s":7200,"describe":{"available":true,"ttl_max_s":7200,"capabilities":["github-jit","github-jit-guard"],"size":{"name":"c4m16","os":"linux"}}}]}`)
					} else if mode == "binding-cap" || mode == "driver-cap" || mode == "uppercase-size" || mode == "invalid-size" {
						size := "c4m16"
						bindingCap, driverCap := 0, 0
						switch mode {
						case "binding-cap":
							bindingCap = 3600
						case "driver-cap":
							driverCap = 3600
						case "uppercase-size":
							size = "C4M16"
						case "invalid-size":
							size = "bad:size"
						}
						fmt.Fprintf(w, `{"targets":[{"name":"linux","kind":"box","host_class":"trusted","ttl_max_s":%d,"describe":{"available":true,"ttl_max_s":%d,"capabilities":["github-jit","github-jit-guard"],"size":{"name":%q,"os":"darwin"}}}]}`, bindingCap, driverCap, size)
					} else {
						fmt.Fprint(w, `{"targets":[]}`)
					}
				default:
					t.Errorf("unexpected request %s", r.URL)
				}
			}))
			st, err := orchclient.CIStatus(context.Background(), c)
			if mode == "read-refused" {
				if !api.IsCode(err, api.CodeNotFound) {
					t.Fatalf("error lost: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "off" {
				if st.State != "not_configured" || len(st.Issues) == 0 || len(st.Repos) != 0 {
					t.Fatalf("off: %+v", st)
				}
				return
			}
			r := st.Repos[0]
			if mode == "override" || mode == "public-mac" {
				if r.RunsOn != `runs-on: ["self-hosted","linux","home:c4m16"]` {
					t.Fatalf("misleading Mac line: %s", r.RunsOn)
				}
			} else if mode == "alias" {
				if r.RunsOn != `runs-on: ["self-hosted","linux","home:fast"]` {
					t.Fatalf("broker alias lost: %s", r.RunsOn)
				}
			} else if mode == "pool-only" {
				if r.RunsOn != `runs-on: ["self-hosted","macos","home"]` {
					t.Fatalf("broker pool-only fallback lost: %s", r.RunsOn)
				}
			} else if r.RunsOn != "" || len(r.Issues) == 0 {
				t.Fatalf("invented machine: %+v", r)
			}
			if mode == "history-cap" {
				if !st.Partial || len(st.Issues) == 0 {
					t.Fatalf("hid history bound: %+v", st)
				}
				for _, j := range r.Jobs {
					if j.Why == "" {
						t.Fatal("silent waiting job")
					}
				}
			}
		})
	}
}

// zm ci status says which mapped repos the broker serves: one not connected
// names the command that connects it, a connected one carries the broker's
// warning, and a list it cannot read is partial, never an all-clear.
func TestCIStatusConnectedRepos(t *testing.T) {
	for _, mode := range []string{"listed", "refused"} {
		t.Run(mode, func(t *testing.T) {
			c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				switch r.URL.Path {
				case "/v0/runners/setup":
					fmt.Fprint(w, `{"pool":"home","label":"home","app":{"id":1}}`)
				case "/v0/runners/jobs":
					fmt.Fprint(w, `{"jobs":[]}`)
				case "/v0/run_targets":
					fmt.Fprint(w, `{"targets":[]}`)
				case "/v0/github/repos":
					fmt.Fprint(w, `{"repositories":[{"project":"P","repo":"acme/off"},{"project":"P","repo":"acme/pub"},{"project":"P","repo":"Acme/Ok"},{"project":"P","repo":"acme/new"}]}`)
				case "/v0/github/data":
					fmt.Fprint(w, `{"payload":{"total_count":0,"workflow_runs":[]}}`)
				case "/v0/runners/repos":
					if mode == "refused" {
						api.WriteError(w, api.NotFound("route", ""))
						return
					}
					fmt.Fprint(w, `{"repos":[{"repo":"acme/pub","by":"upgrade","visibility":"public","warning":"Outside contributors can start runs."},{"repo":"acme/ok","by":"ana","visibility":"private"},{"repo":"acme/new","by":"upgrade"}]}`)
				default:
					t.Fatalf("unexpected %s", r.URL)
				}
			}))
			st, err := orchclient.CIStatus(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			issues := map[string]string{}
			for _, r := range st.Repos {
				issues[r.Repo] = strings.Join(r.Issues, "; ")
			}
			if mode == "refused" {
				if !st.Partial || !strings.Contains(strings.Join(st.Issues, "; "), "Could not read which repos are connected") {
					t.Fatalf("unread connected list is an all-clear: %+v", st)
				}
				for _, r := range st.Repos {
					if !r.Partial {
						t.Errorf("row %s not partial", r.Repo)
					}
				}
				return
			}
			for repo, want := range map[string]string{
				"acme/off": "Not connected in the pool areas you can read, so its jobs are not served: zm runners allow acme/off (--public for a public one)",
				"acme/pub": "Connected repo: not served until zm runners allow acme/pub --public; Outside contributors can start runs.",
				"acme/new": "if public, not served until zm runners allow acme/new --public",
			} {
				if !strings.Contains(issues[repo], want) {
					t.Errorf("%s: want %q in %q", repo, want, issues[repo])
				}
			}
			if strings.Contains(issues["Acme/Ok"], "onnected") || strings.Contains(issues["Acme/Ok"], "potluck") {
				t.Errorf("a connected private repo is flagged: %q", issues["Acme/Ok"])
			}
		})
	}
}
