package orchclient

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/runners"
)

var (
	ciRefresh     atomic.Uint64
	ciRefreshSeed = uint64(time.Now().UnixNano())
)

// CIStatus reads the local broker plus the caller's mapped GitHub repositories.
// It never changes GitHub, config or jobs. All component reads retain the caller.
func CIStatus(ctx context.Context, c *client.Client) (orchapi.CIStatus, error) {
	out := orchapi.CIStatus{State: "configured", Repos: []orchapi.CIRepoStatus{}, Issues: []string{}}
	var setup runners.Setup
	if err := c.Do(ctx, "GET", "/v0/runners/setup", nil, &setup); err != nil {
		return out, fmt.Errorf("read CI configuration on its own machine: %w", err)
	}
	if setup.Pool == "" || setup.App == nil {
		out.State = "not_configured"
		out.Issues = append(out.Issues, "GitHub is not connected yet; run zm runners setup <pool>")
		return out, nil
	}
	var jobs struct {
		Jobs []runners.JobView `json:"jobs"`
	}
	if err := c.Do(ctx, "GET", "/v0/runners/jobs?limit=500", nil, &jobs); err != nil {
		return out, err
	}
	if len(jobs.Jobs) == 500 {
		out.Partial = true
		out.Issues = append(out.Issues, "Only the latest 500 recorded jobs were read; older waiting or refused jobs may be missing")
	}
	mapped, err := GitHubRepositories(ctx, c, "")
	if err != nil {
		return out, err
	}
	// Target discovery must use the CI project, never the shell's current one.
	line, err := CIWorkflowLine(ctx, c, setup)
	machinesErr := err
	if err != nil {
		out.Issues = append(out.Issues, "Could not read serving machines: "+err.Error())
		out.Partial = true
	}
	byRepo := map[string]*orchapi.CIRepoStatus{}
	row := func(repo string) *orchapi.CIRepoStatus {
		key := strings.ToLower(repo)
		if byRepo[key] == nil {
			v := &orchapi.CIRepoStatus{Repo: repo, RunsOn: line, JobLimitS: setup.JobTTLS, Issues: []string{}, Jobs: []orchapi.CIJob{}}
			if machinesErr != nil {
				v.Issues = append(v.Issues, "Serving machine information is unavailable; retry when machine discovery is reachable")
				v.Partial = true
			} else if line == "" {
				v.Issues = append(v.Issues, "No serving machine was found; run zm pool host "+setup.Pool+" on the machine to lend")
			}
			byRepo[key] = v
		}
		return byRepo[key]
	}
	for _, j := range jobs.Jobs {
		v := row(j.Repo)
		v.Jobs = append(v.Jobs, orchapi.CIJob{JobView: j})
	}
	seen := map[string]bool{}
	repos := make([]orchapi.GitHubRepository, 0, len(mapped.Repositories))
	for _, m := range mapped.Repositories {
		key := strings.ToLower(m.Repo)
		if seen[key] {
			continue
		}
		seen[key] = true
		repos = append(repos, m)
	}
	// Each refresh starts at the next repo so a busy prefix cannot starve the
	// tail. Time also rotates fresh CLI processes, which have no shared counter.
	start := int((ciRefreshSeed + ciRefresh.Add(1) - 1) % uint64(max(1, len(repos))))
	scan := ciScan{remaining: 64, runs: map[string]bool{}}
	for i := range repos {
		m := repos[(start+i)%len(repos)]
		v := row(m.Repo)
		ciGitHubJobs(ctx, c, m, setup, v, &scan)
	}
	ciConnected(ctx, c, &out, byRepo)
	for key, v := range byRepo {
		if !seen[key] {
			v.Partial = true
			v.Issues = append(v.Issues, "Unseen jobs could not be checked: this repo has no readable project repository mapping")
		}
		sort.SliceStable(v.Jobs, func(i, j int) bool {
			if v.Jobs[i].Updated != v.Jobs[j].Updated {
				return v.Jobs[i].Updated > v.Jobs[j].Updated
			}
			return v.Jobs[i].ID > v.Jobs[j].ID
		})
		ciSummary(v)
		out.Partial = out.Partial || v.Partial
		out.Repos = append(out.Repos, *v)
	}
	slices.SortFunc(out.Repos, func(a, b orchapi.CIRepoStatus) int {
		return strings.Compare(strings.ToLower(a.Repo), strings.ToLower(b.Repo))
	})
	if len(out.Repos) == 0 {
		out.State = "no_repositories"
		out.Issues = append(out.Issues, "No repos are known yet: connect GitHub with zm runners link and add the repo as a project remote")
	}
	return out, ctx.Err()
}

func ciSummary(v *orchapi.CIRepoStatus) {
	waiting, refused, unseen, failed, lost, taken := 0, 0, 0, 0, 0, 0
	for i := range v.Jobs {
		j := &v.Jobs[i]
		switch j.State {
		case "waiting":
			waiting++
			if j.Why == "" {
				j.Why = "No waiting reason was recorded; run zm runners jobs"
			}
		case "ignored":
			refused++
			if j.Why == "" {
				j.Why = "This job was refused, but no reason was recorded; run zm runners jobs"
			}
		case "unseen":
			unseen++
		case "failed", "lost", "taken":
			switch j.State {
			case "failed":
				failed++
			case "lost":
				lost++
			case "taken":
				taken++
			}
			if j.Why == "" {
				j.Why = "No " + j.State + " reason was recorded; run zm runners jobs"
			}
		}
	}
	switch {
	case waiting+refused+unseen+failed+lost+taken > 0:
		v.Summary = fmt.Sprintf("%d waiting, %d refused, %d unseen", waiting, refused, unseen)
		for _, count := range []struct {
			n     int
			state string
		}{{failed, "failed"}, {lost, "lost"}, {taken, "taken"}} {
			if count.n > 0 {
				v.Summary += fmt.Sprintf(", %d %s", count.n, count.state)
			}
		}
	case len(v.Jobs) > 0:
		v.Summary = "Last job: " + v.Jobs[0].State
	default:
		v.Summary = "No jobs seen yet; check that the GitHub app covers this repo"
	}
}
