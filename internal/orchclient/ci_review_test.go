package orchclient_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// The consumer contract uses literal wire keys, independently of the Go structs.
// Failed/lost/taken jobs must remain visible even after a newer job succeeds.
func TestCIStatusWireContractAndExceptions(t *testing.T) {
	c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		switch r.URL.Path {
		case "/v0/runners/setup":
			fmt.Fprint(w, `{"pool":"home","label":"home","app":{"id":1},"job_ttl_s":5400}`)
		case "/v0/run_targets":
			fmt.Fprint(w, `{"targets":[]}`)
		case "/v0/runners/repos":
			fmt.Fprint(w, `{"repos":[{"repo":"a/b"}]}`)
		case "/v0/github/repos":
			fmt.Fprint(w, `{"repositories":[]}`)
		case "/v0/runners/jobs":
			fmt.Fprint(w, `{"jobs":[{"job_id":4,"repo":"a/b","state":"done","updated":"2026-11-12T04:00:00Z"},{"job_id":3,"repo":"a/b","state":"failed","why":"GitHub refused the runner","updated":"2026-11-12T03:00:00Z"},{"job_id":2,"repo":"a/b","state":"lost","why":"Mac reclaimed","updated":"2026-11-12T02:00:00Z"},{"job_id":1,"repo":"a/b","state":"taken","updated":"2026-11-12T01:00:00Z"}]}`)
		default:
			t.Fatalf("unexpected %s", r.URL)
		}
	}))
	st, err := orchclient.CIStatus(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(st)
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"state", "repos", "issues", "partial"} {
		if _, ok := wire[key]; !ok {
			t.Fatalf("missing %s: %s", key, raw)
		}
	}
	var repos []map[string]json.RawMessage
	_ = json.Unmarshal(wire["repos"], &repos)
	for _, key := range []string{"repo", "runs_on", "job_limit_s", "summary", "issues", "jobs", "partial"} {
		if _, ok := repos[0][key]; !ok {
			t.Fatalf("missing repo %s: %s", key, raw)
		}
	}
	var jobs []map[string]json.RawMessage
	_ = json.Unmarshal(repos[0]["jobs"], &jobs)
	for _, key := range []string{"job_id", "repo", "run_id", "labels", "state", "created", "updated"} {
		if _, ok := jobs[0][key]; !ok {
			t.Fatalf("missing job %s: %s", key, raw)
		}
	}
	for _, want := range []string{"1 failed", "1 lost", "1 taken"} {
		if !strings.Contains(st.Repos[0].Summary, want) {
			t.Fatalf("exception hidden: %s", raw)
		}
	}
	if st.Repos[0].Jobs[1].Why != "GitHub refused the runner" || st.Repos[0].Jobs[2].Why != "Mac reclaimed" || st.Repos[0].Jobs[3].Why == "" {
		t.Fatalf("reasons lost: %s", raw)
	}
}

func TestCIUnseenReasonsAndScanBudget(t *testing.T) {
	for _, mode := range []string{"queued", "new", "waiting", "pending", "requested", "action_required", "run-waiting", "run-pending", "run-action_required", "budget", "empty"} {
		t.Run(mode, func(t *testing.T) {
			reads := 0
			c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				switch r.URL.Path {
				case "/v0/runners/setup":
					fmt.Fprint(w, `{"pool":"home","label":"home","app":{"id":1}}`)
				case "/v0/runners/jobs":
					fmt.Fprint(w, `{"jobs":[]}`)
				case "/v0/run_targets":
					fmt.Fprint(w, `{"targets":[]}`)
				case "/v0/runners/repos":
					fmt.Fprint(w, `{"repos":[]}`)
				case "/v0/github/repos":
					repos := []orchapi.GitHubRepository{}
					n := 1
					if mode == "budget" {
						n = 30
					}
					if mode == "empty" {
						n = 0
					}
					for i := 0; i < n; i++ {
						repos = append(repos, orchapi.GitHubRepository{Project: "p", Repo: fmt.Sprintf("acme/repo%d", i)})
					}
					api.WriteJSON(w, 200, map[string]any{"repositories": repos})
				case "/v0/github/data":
					reads++
					status := mode
					runStatus := "in_progress"
					created := "2026-01-01T00:00:00Z"
					switch mode {
					case "new":
						status = "queued"
						created = time.Now().UTC().Format(time.RFC3339)
					case "run-waiting", "run-pending", "run-action_required":
						status = "queued"
						runStatus = strings.TrimPrefix(mode, "run-")
					}
					if r.URL.Query().Get("resource") == "runs" {
						// The same run can move between status filters while a refresh reads it.
						fmt.Fprintf(w, `{"payload":{"total_count":1,"workflow_runs":[{"id":10,"status":%q}]}}`, runStatus)
					} else {
						fmt.Fprintf(w, `{"payload":{"total_count":1,"jobs":[{"id":1,"status":%q,"labels":["home"],"name":"Mac build","html_url":"https://github.com/acme/app/actions/runs/10/job/1","created_at":%q}]}}`, status, created)
					}
				default:
					t.Fatalf("unexpected %s", r.URL)
				}
			}))
			st, err := orchclient.CIStatus(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "empty" {
				if st.State != "no_repositories" {
					t.Fatalf("silent empty state: %+v", st)
				}
				return
			}
			if mode == "budget" {
				if reads != 64 || !st.Partial {
					t.Fatalf("unbounded scan: reads=%d %+v", reads, st)
				}
				return
			}
			if reads != 7 {
				t.Fatalf("run read repeatedly across status filters: %d", reads)
			}
			j := st.Repos[0].Jobs[0]
			want := map[string]string{"queued": "check that the GitHub app", "new": "2 minutes", "waiting": "environment protection or approval", "pending": "concurrency slot", "requested": "not reached the runner queue", "action_required": "requires action", "run-waiting": "check that the GitHub app", "run-pending": "check that the GitHub app", "run-action_required": "check that the GitHub app"}[mode]
			if !strings.Contains(j.Why, want) {
				t.Fatalf("wrong reason: %+v", j)
			}
			if mode != "queued" && !strings.HasPrefix(mode, "run-") && strings.Contains(j.Why, "check that the GitHub app") {
				t.Fatalf("false app diagnosis: %+v", j)
			}
			raw, _ := json.Marshal(j)
			var wire map[string]any
			_ = json.Unmarshal(raw, &wire)
			if wire["name"] != "Mac build" || wire["url"] != "https://github.com/acme/app/actions/runs/10/job/1" || wire["github_status"] == nil || wire["state"] != "unseen" || wire["why"] == "" {
				t.Fatalf("lost wire facts: %s", raw)
			}
		})
	}
}

// A run-level gate cannot explain a sibling job already queued for a runner.
func TestCIJobStatusOverridesRunGate(t *testing.T) {
	for _, gate := range []string{"waiting", "pending", "action_required"} {
		t.Run(gate, func(t *testing.T) {
			c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				switch r.URL.Path {
				case "/v0/runners/setup":
					fmt.Fprint(w, `{"pool":"home","label":"home","app":{"id":1}}`)
				case "/v0/runners/jobs":
					fmt.Fprint(w, `{"jobs":[]}`)
				case "/v0/run_targets":
					fmt.Fprint(w, `{"targets":[]}`)
				case "/v0/runners/repos":
					fmt.Fprint(w, `{"repos":[]}`)
				case "/v0/github/repos":
					fmt.Fprint(w, `{"repositories":[{"project":"p","repo":"acme/app"}]}`)
				case "/v0/github/data":
					if r.URL.Query().Get("resource") == "runs" {
						fmt.Fprintf(w, `{"payload":{"total_count":1,"workflow_runs":[{"id":10,"status":%q}]}}`, gate)
					} else {
						fmt.Fprintf(w, `{"payload":{"total_count":7,"jobs":[
							{"id":1,"status":"queued","labels":["home"],"created_at":"2026-01-01T00:00:00Z"},
							{"id":2,"status":"waiting","labels":["home"]},
							{"id":3,"status":"pending","labels":["home"]},
							{"id":4,"status":"requested","labels":["home"]},
							{"id":5,"labels":["home"]},
							{"id":6,"status":"completed","labels":["home"]},
							{"id":7,"status":"queued","labels":["home"],"created_at":%q}]}}`, time.Now().UTC().Format(time.RFC3339))
					}
				default:
					t.Fatalf("unexpected %s", r.URL)
				}
			}))
			st, err := orchclient.CIStatus(context.Background(), c)
			if err != nil {
				t.Fatal(err)
			}
			gateReason := map[string]string{"waiting": "environment protection or approval", "pending": "concurrency slot", "action_required": "requires action"}[gate]
			want := map[int64]string{1: "check that the GitHub app", 2: "environment protection or approval", 3: "concurrency slot", 4: "not reached the runner queue", 5: gateReason, 7: "2 minutes"}
			if len(st.Repos[0].Jobs) != len(want) {
				t.Fatalf("lost pending job or included completed job: %+v", st)
			}
			for _, j := range st.Repos[0].Jobs {
				if reason, ok := want[j.ID]; !ok || !strings.Contains(j.Why, reason) {
					t.Errorf("job %d under run gate %s: %+v", j.ID, gate, j)
				}
			}
		})
	}
}

func TestCIRefreshRotatesBudgetAcrossRepositories(t *testing.T) {
	const count = 12 // Six empty status probes per repo exceed the 64-read budget.
	reads := map[string]int{}
	var first string
	c := client.WithHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		switch r.URL.Path {
		case "/v0/runners/setup":
			fmt.Fprint(w, `{"pool":"home","label":"home","app":{"id":1}}`)
		case "/v0/runners/jobs":
			fmt.Fprint(w, `{"jobs":[]}`)
		case "/v0/run_targets":
			fmt.Fprint(w, `{"targets":[]}`)
		case "/v0/runners/repos":
			fmt.Fprint(w, `{"repos":[]}`)
		case "/v0/github/repos":
			repos := []orchapi.GitHubRepository{}
			for i := 0; i < count; i++ {
				repos = append(repos, orchapi.GitHubRepository{Project: "p", Repo: fmt.Sprintf("acme/repo%02d", i)})
			}
			// A second mapping must not consume another scan slot or change scope.
			repos = append(repos, orchapi.GitHubRepository{Project: "q", Repo: "ACME/REPO00"})
			api.WriteJSON(w, 200, map[string]any{"repositories": repos})
		case "/v0/github/data":
			q := r.URL.Query()
			if q.Get("resource") != "runs" || q.Get("page") != "1" || q.Get("project") != "p" {
				t.Fatalf("empty filter caused extra reads or lost mapping: %s", r.URL)
			}
			if first == "" {
				first = q.Get("repo")
			}
			reads[q.Get("repo")]++
			fmt.Fprint(w, `{"payload":{"total_count":0,"workflow_runs":[]}}`)
		default:
			t.Fatalf("unexpected %s", r.URL)
		}
	}))
	starts, complete := map[string]bool{}, map[string]bool{}
	for i := 0; i < count; i++ {
		reads, first = map[string]int{}, ""
		st, err := orchclient.CIStatus(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		starts[first] = true
		total := 0
		for _, n := range reads {
			total += n
		}
		if total != 64 || !st.Partial || len(st.Repos) != count {
			t.Fatalf("lost read bound or rows: reads=%d %+v", total, st)
		}
		for _, repo := range st.Repos {
			if !repo.Partial {
				complete[repo.Repo] = true
				if reads[repo.Repo] != 6 {
					t.Fatalf("repo incorrectly marked complete: %+v", repo)
				}
			}
		}
	}
	if len(starts) != count || len(complete) != count {
		t.Fatalf("starved repositories across refreshes: starts=%v complete=%v", starts, complete)
	}
}
