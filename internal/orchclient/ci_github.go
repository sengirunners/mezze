package orchclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/runners"
)

// A status refresh consumes at most half of the shared 128-entry GitHub cache.
// Keep the bound across repositories, and disclose every unscanned remainder.
type ciScan struct {
	remaining int
	runs      map[string]bool
}

func (s *ciScan) read(ctx context.Context, c *client.Client, m orchapi.GitHubRepository, resource, status string, run int64, page int, out any) error {
	if s.remaining == 0 {
		return fmt.Errorf("CI status reached its 64-read GitHub budget; remaining repos/runs/jobs were not checked")
	}
	s.remaining--
	return ciGitHubRead(ctx, c, m, resource, status, run, page, out)
}

// The existing GitHub read door caps each resource at ten pages. Reaching that
// bound is reported, not interpreted as proof that no unseen job exists.
func ciGitHubJobs(ctx context.Context, c *client.Client, m orchapi.GitHubRepository, setup runners.Setup, v *orchapi.CIRepoStatus, scan *ciScan) {
	fail := func(err error) {
		v.Partial = true
		v.Issues = append(v.Issues, "Unseen jobs could not be fully checked: "+err.Error())
	}
	known := map[int64]bool{}
	for _, j := range v.Jobs {
		known[j.ID] = true
	}
	for _, status := range []string{"queued", "in_progress", "waiting", "pending", "requested", "action_required"} {
		if !ciGitHubRuns(ctx, c, m, setup, status, v, known, fail, scan) || ctx.Err() != nil {
			return
		}
	}
}

func ciGitHubRuns(ctx context.Context, c *client.Client, m orchapi.GitHubRepository, setup runners.Setup, status string, v *orchapi.CIRepoStatus, known map[int64]bool, fail func(error), scan *ciScan) bool {
	for page := 1; page <= 10; page++ {
		var runs struct {
			Total int `json:"total_count"`
			Runs  []struct {
				ID     int64  `json:"id"`
				Status string `json:"status"`
			} `json:"workflow_runs"`
		}
		if err := scan.read(ctx, c, m, "runs", status, 0, page, &runs); err != nil {
			fail(err)
			return false
		}
		// Empty status filters need no job reads or further pages. Probe them
		// again next refresh so newly queued jobs cannot be hidden by old counts.
		if runs.Total == 0 {
			return true
		}
		for _, run := range runs.Runs {
			if run.Status == "completed" {
				continue
			}
			key := strings.ToLower(m.Repo) + ":" + strconv.FormatInt(run.ID, 10)
			if scan.runs[key] {
				continue
			}
			scan.runs[key] = true
			for jp := 1; jp <= 10; jp++ {
				var jobs struct {
					Total int `json:"total_count"`
					Jobs  []struct {
						ID      int64    `json:"id"`
						Name    string   `json:"name"`
						Status  string   `json:"status"`
						Labels  []string `json:"labels"`
						URL     string   `json:"html_url"`
						Created string   `json:"created_at"`
					} `json:"jobs"`
				}
				if err := scan.read(ctx, c, m, "jobs", "", run.ID, jp, &jobs); err != nil {
					fail(err)
					return false
				}
				for _, j := range jobs.Jobs {
					status := j.Status
					// A run can mix gated jobs with jobs already in the runner
					// queue. Its gate applies only without a job status.
					if status == "" {
						status = run.Status
					}
					// The broker's own label rules decide whether the job asks for
					// this pool. Not RunsOn: that answers for one serving target,
					// and a queued job with none must stay visible.
					if known[j.ID] || (status != "queued" && status != "waiting" && status != "pending" && status != "requested" && status != "action_required") || !runners.ParseLabels(j.Labels, setup).Ours {
						continue
					}
					known[j.ID] = true
					v.Jobs = append(v.Jobs, orchapi.CIJob{
						JobView: runners.JobView{ID: j.ID, Repo: m.Repo, Run: run.ID, Labels: j.Labels, State: "unseen",
							Why: ciUnseenReason(status, j.Created), Created: j.Created, Updated: j.Created},
						Name: j.Name, URL: j.URL, GitHubStatus: j.Status,
					})
				}
				if jp*100 >= jobs.Total {
					break
				}
				if jp == 10 {
					fail(fmt.Errorf("only the first 1000 jobs of run %d were checked", run.ID))
				}
			}
		}
		if page*30 >= runs.Total {
			return true
		}
		if page == 10 {
			fail(fmt.Errorf("only the first 300 %s GitHub runs were checked; older waiting jobs may be missing", status))
		}
	}
	return true
}

func ciUnseenReason(status, created string) string {
	switch status {
	case "waiting":
		return "Waiting on GitHub environment protection or approval; inspect the job on GitHub"
	case "pending":
		return "Pending on GitHub, for example waiting for a concurrency slot; inspect the run on GitHub"
	case "requested":
		return "GitHub has requested this job; it has not reached the runner queue yet"
	case "action_required":
		return "GitHub requires action before this run can proceed; inspect the run on GitHub"
	}
	if at, err := time.Parse(time.RFC3339, created); err == nil && time.Since(at) < 2*time.Minute {
		return "Newly queued on GitHub; allow up to 2 minutes for webhook delivery and broker processing"
	}
	return "Queued on GitHub but absent from the latest CI job records; check that the GitHub app covers this repo and its connection is confirmed (zm runners ls)"
}

func ciGitHubRead(ctx context.Context, c *client.Client, m orchapi.GitHubRepository, resource, status string, run int64, page int, out any) error {
	q := url.Values{"project": {m.Project}, "repo": {m.Repo}, "area": {m.Area}, "resource": {resource}, "page": {strconv.Itoa(page)}}
	if status != "" {
		q.Set("status", status)
	}
	if run != 0 {
		q.Set("run", strconv.FormatInt(run, 10))
	}
	v, err := GitHubData(ctx, c, q)
	if err != nil {
		return err
	}
	if v.Error != nil {
		return fmt.Errorf("%s", v.Error.Message)
	}
	if v.Stale || len(v.Payload) == 0 {
		return fmt.Errorf("GitHub data is unavailable or stale; check the daemon's GitHub login")
	}
	return json.Unmarshal(v.Payload, out)
}
