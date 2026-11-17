package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/spf13/cobra"
)

func TestCIStatusTextAndJSON(t *testing.T) {
	runtimeClientEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		if r.Method != "GET" {
			t.Fatalf("status wrote: %s", r.Method)
		}
		switch r.URL.Path {
		case "/v0/runners/setup":
			fmt.Fprint(w, `{"pool":"home","label":"home","job_ttl_s":5400,"app":{"id":1}}`)
		case "/v0/runners/jobs":
			fmt.Fprint(w, `{"jobs":[{"job_id":1,"repo":"acme/worm","state":"ignored","why":"Fork PRs are refused"},{"job_id":2,"repo":"acme/worm","state":"running","why":"Executing the workflow","limit_s":6000,"limit_source":"workflow .github/workflows/proof.yml at 71d481b384735653cdfd223b2a4209f5508892b3 job build: literal timeout-minutes","started_at":1700000000,"deadline":1700006000},{"job_id":4,"repo":"acme/worm","state":"ignored","why":"acme/worm is not connected to this pool (zm runners allow acme/worm)"}]}`)
		case "/v0/github/repos":
			fmt.Fprint(w, `{"repositories":[{"project":"P","repo":"acme/worm"}]}`)
		case "/v0/runners/links":
			fmt.Fprint(w, `{"links":[]}`)
		case "/v0/runners/repos":
			fmt.Fprint(w, `{"repos":[{"repo":"acme/worm","installation_id":7,"public":true,"by":"ana","since":"2026-11-12T00:00:00Z","visibility":"public","warning":"Outside contributors who have contributed before can start workflow runs on this public repository without your approval."}]}`)
		case "/v0/github/data":
			if r.URL.Query().Get("resource") == "jobs" {
				fmt.Fprint(w, `{"payload":{"total_count":1,"jobs":[{"id":3,"name":"native Mac build","html_url":"https://github.com/acme/worm/actions/runs/10/job/3","status":"queued","labels":["home"],"created_at":"2026-01-01T00:00:00Z"}]}}`)
			} else if r.URL.Query().Get("status") == "queued" {
				fmt.Fprint(w, `{"payload":{"total_count":1,"workflow_runs":[{"id":10,"status":"queued"}]}}`)
			} else {
				fmt.Fprint(w, `{"payload":{"total_count":0,"workflow_runs":[]}}`)
			}
		case "/v0/run_targets":
			fmt.Fprint(w, `{"targets":[{"name":"mac","kind":"box","host_class":"trusted","describe":{"available":true,"capabilities":["github-jit","github-jit-guard"],"size":{"name":"c4m16","os":"darwin"}}}]}`)
		default:
			t.Errorf("unexpected request %s", r.URL)
			w.WriteHeader(500)
		}
	}))
	oldJSON := flagJSON
	t.Cleanup(func() { flagJSON = oldJSON })
	run := func(jsonMode bool) string {
		flagJSON = jsonMode
		r := &cobra.Command{Use: "zm"}
		r.AddCommand(ciCmd())
		out, err := runCLI(t, r, "ci", "status")
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	text, raw := run(false), run(true)
	var status orchapi.CIStatus
	if err := json.Unmarshal([]byte(raw), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Repos) != 1 {
		t.Fatalf("JSON: %s", raw)
	}
	r := status.Repos[0]
	for _, want := range []string{r.Repo + ": " + r.Summary, "\n" + r.RunsOn + "\n", "Fallback job limit: 5400 s", "Fork PRs are refused", "Executing the workflow", "6000 s", "workflow .github/workflows/proof.yml at 71d481b384735653cdfd223b2a4209f5508892b3 job build: literal timeout-minutes", "Queued on GitHub", "Connected repo: Outside contributors who have contributed before", "native Mac build", "https://github.com/acme/worm/actions/runs/10/job/3"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks JSON fact %q:\n%s", want, text)
		}
	}
	var runtimeJob *orchapi.CIJob
	for i := range r.Jobs {
		if r.Jobs[i].ID == 2 {
			runtimeJob = &r.Jobs[i]
		}
	}
	if runtimeJob == nil || runtimeJob.State != "running" || runtimeJob.LimitS != 6000 || runtimeJob.LimitSource == "" || runtimeJob.StartedAt != 1700000000 || runtimeJob.Deadline != 1700006000 {
		t.Fatalf("CI JSON dropped runtime metadata: %s", raw)
	}
	flagJSON = false
	ls, err := runCLI(t, runnersCmd(), "ls")
	if err != nil || !strings.Contains(ls, "\n"+r.RunsOn+"\n") || !strings.Contains(ls, "fallback    5400 s") || !strings.Contains(ls, "workflow timeout-minutes takes precedence") {
		t.Fatalf("runners ls differs: %v\n%s", err, ls)
	}
	// ls lists the connected repositories and the broker's warning for them.
	if !strings.Contains(ls, "acme/worm") || !strings.Contains(ls, "Outside contributors who have contributed before") || strings.Contains(ls, "potluck") {
		t.Fatalf("runners ls repos: %s", ls)
	}
	jobs, err := runCLI(t, runnersCmd(), "jobs")
	if err != nil || !strings.Contains(jobs, "Fork PRs are refused") || !strings.Contains(jobs, "Executing the workflow") || !strings.Contains(jobs, "REASON") ||
		!strings.Contains(jobs, "(zm runners allow acme/worm)") || strings.Contains(jobs, "potluck") {
		t.Fatalf("runners jobs: %v\n%s", err, jobs)
	}
	for _, want := range []string{"LIMIT", "SOURCE", "6000 s", runtimeJob.LimitSource} {
		if !strings.Contains(jobs, want) {
			t.Errorf("runners jobs missing %q: %s", want, jobs)
		}
	}
	flagJSON = true
	jobsJSON, err := runCLI(t, runnersCmd(), "jobs")
	if err != nil {
		t.Fatal(err)
	}
	var broker struct {
		Jobs []orchapi.CIJob `json:"jobs"`
	}
	if err := json.Unmarshal([]byte(jobsJSON), &broker); err != nil || len(broker.Jobs) != 3 || broker.Jobs[1].LimitS != 6000 || broker.Jobs[1].LimitSource != runtimeJob.LimitSource || broker.Jobs[1].StartedAt != runtimeJob.StartedAt || broker.Jobs[1].Deadline != runtimeJob.Deadline {
		t.Fatalf("runners jobs JSON dropped runtime metadata: %v %s", err, jobsJSON)
	}
	var labels []string
	if err := json.Unmarshal([]byte(strings.TrimPrefix(r.RunsOn, "runs-on: ")), &labels); err != nil {
		t.Fatal(err)
	}
	if strings.Join(labels, ",") != "self-hosted,macos,home:c4m16" {
		t.Fatalf("workflow line lost OS/size: %s", r.RunsOn)
	}
}

func TestCIJobPrinterPreservesUnseenDetails(t *testing.T) {
	// Both text doors call this printer; untrusted names/reasons stay on one row.
	var out strings.Builder
	if err := printCIJobs(&out, []orchapi.CIJob{{Name: "Mac\nbuild", URL: "https://github.com/acme/app/actions/runs/10/job/1"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Mac build") || !strings.Contains(out.String(), "https://github.com/acme/app/actions/runs/10/job/1") {
		t.Fatal(out.String())
	}
}

// zm ci doctor prints the broker's checks, exits nonzero on a failure, and
// says so when the daemon has no doctor route (one older than this zm).
func TestCIDoctor(t *testing.T) {
	for _, mode := range []string{"checks", "old-daemon"} {
		t.Run(mode, func(t *testing.T) {
			runtimeClientEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(api.HeaderProtocol, api.Protocol)
				if r.Method != "GET" || r.URL.Path != "/v0/runners/doctor" || r.URL.Query().Get("repo") != "acme/web" {
					t.Errorf("doctor read %s %s", r.Method, r.URL)
				}
				if mode == "old-daemon" {
					api.WriteError(w, api.NotFound("route", ""))
					return
				}
				fmt.Fprint(w, `{"reports":[{"repo":"acme/web","checks":[{"number":1,"status":"pass","message":"ok"},{"number":5,"status":"fail","message":"acme/web is not connected\nto this pool","fix":"zm runners allow acme/web"}]}],"warnings":["allow_public_repos does nothing"]}`)
			}))
			oldJSON := flagJSON
			t.Cleanup(func() { flagJSON = oldJSON })
			for _, jsonMode := range []bool{false, true} {
				flagJSON = jsonMode
				r := &cobra.Command{Use: "zm"}
				r.AddCommand(ciCmd())
				out, err := runCLI(t, r, "ci", "doctor", "acme/web")
				if mode == "old-daemon" {
					if err == nil || !strings.Contains(err.Error(), "this daemon has no CI doctor: zm daemon restart") {
						t.Fatalf("an old daemon is not said: %v\n%s", err, out)
					}
					continue
				}
				if err == nil || !strings.Contains(err.Error(), "repository checks failed") {
					t.Fatalf("a failed check exited zero: %v\n%s", err, out)
				}
				want := []string{" 5 fail acme/web is not connected to this pool", "   Next: zm runners allow acme/web", "warn: allow_public_repos does nothing"}
				if jsonMode {
					var res struct {
						Reports []struct {
							Repo   string `json:"repo"`
							Checks []struct {
								Status string `json:"status"`
							} `json:"checks"`
						} `json:"reports"`
					}
					if json.Unmarshal([]byte(out), &res) != nil || len(res.Reports) != 1 || res.Reports[0].Repo != "acme/web" {
						t.Fatalf("doctor JSON: %s", out)
					}
					continue
				}
				for _, w := range want {
					if !strings.Contains(out, w) {
						t.Errorf("doctor text lacks %q:\n%s", w, out)
					}
				}
			}
		})
	}
}
