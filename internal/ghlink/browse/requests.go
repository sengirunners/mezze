package browse

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

const pullQuery = `query($owner:String!,$name:String!,$after:String){repository(owner:$owner,name:$name){pullRequests(first:50,after:$after,states:OPEN,orderBy:{field:UPDATED_AT,direction:DESC}){pageInfo{hasNextPage endCursor}nodes{number title url author{login}headRefName headRefOid isDraft createdAt updatedAt reviewDecision commits(last:1){nodes{commit{oid statusCheckRollup{state}}}}}}}}`

// pullOneQuery is one pull request in any state: what a PR shepherd needs to say where it is on its way to
// production (mergeability, labels, the head's checks and statuses, review-marker comments, the merge commit's
// Actions runs). Fixed and bounded like pullQuery; files past the first 100 come from the files resource.
const pullOneQuery = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){number title url author{login}state isDraft headRefOid headRefName baseRefName mergeable mergeStateStatus mergedAt updatedAt mergeCommit{oid checkSuites(first:100,filterBy:{appId:15368}){totalCount nodes{status conclusion workflowRun{databaseId event url workflow{resourcePath}}}}}labels(first:20){nodes{name}}files(first:100){pageInfo{hasNextPage endCursor}nodes{path changeType}}comments(last:20){nodes{body}}commits(last:1){nodes{commit{oid statusCheckRollup{contexts(first:100){pageInfo{hasNextPage}nodes{__typename ... on CheckRun{name status conclusion startedAt checkSuite{workflowRun{databaseId workflow{name}}}}... on StatusContext{context state description createdAt}}}}}}}}}}`

const filesQuery = `query($owner:String!,$name:String!,$number:Int!,$after:String){repository(owner:$owner,name:$name){pullRequest(number:$number){number files(first:100,after:$after){pageInfo{hasNextPage endCursor}nodes{path changeType}}}}}`

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Filters a caller may put on a runs listing: a workflow file name, a branch and an event, each matched
// whole so nothing else reaches the upstream path or query.
var (
	workflowPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}\.ya?ml$`)
	branchPattern   = regexp.MustCompile(`^[A-Za-z0-9_./-]{1,200}$`)
	eventPattern    = regexp.MustCompile(`^[a-z_]{1,40}$`)
)

type request struct {
	resource string
	key      string
	args     []string
	head     string
	pull     int64
	run      int64
	page     int
	ttl      time.Duration
}

func plan(q url.Values, mapping orchapi.GitHubRepository) (request, error) {
	p := request{resource: q.Get("resource"), page: 1, ttl: time.Minute}
	if v := q.Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 10 {
			return p, api.Errorf(api.CodeBadRequest, "page must be between 1 and 10")
		}
		p.page = n
	}
	if q.Get("url") != "" || q.Get("query") != "" || q.Get("host") != "" {
		return p, api.Errorf(api.CodeBadRequest, "upstream URLs, hosts and queries are not accepted")
	}
	path := "repos/" + mapping.Repo
	p.args = []string{"api"}
	switch p.resource {
	case "pulls":
		after, err := cursor(q.Get("after"))
		if err != nil {
			return p, err
		}
		parts := strings.Split(mapping.Repo, "/")
		p.args = append(p.args, "graphql", "-f", "query="+pullQuery, "-f", "owner="+parts[0], "-f", "name="+parts[1])
		if after != "" {
			p.args = append(p.args, "-f", "after="+after)
		}
		p.key = "pulls:" + after
		return p, nil
	case "pull", "files":
		id, err := positive(q.Get("pull"))
		if err != nil {
			return p, err
		}
		after, err := cursor(q.Get("after"))
		if err != nil {
			return p, err
		}
		if q.Get("page") != "" {
			return p, api.Errorf(api.CodeBadRequest, "pull and files page with after, not page")
		}
		if p.resource == "pull" && after != "" {
			return p, api.Errorf(api.CodeBadRequest, "a pull read takes no cursor; page its files with resource=files")
		}
		p.pull = id
		parts := strings.Split(mapping.Repo, "/")
		gql := pullOneQuery
		if p.resource == "files" {
			gql = filesQuery
		}
		n := strconv.FormatInt(id, 10)
		p.args = append(p.args, "graphql", "-f", "query="+gql, "-f", "owner="+parts[0], "-f", "name="+parts[1], "-F", "number="+n)
		if after != "" {
			p.args = append(p.args, "-f", "after="+after)
		}
		p.key = p.resource + ":" + n + ":" + after
		return p, nil
	case "runs":
		f := url.Values{}
		for _, c := range []struct {
			name string
			re   *regexp.Regexp
		}{{"branch", branchPattern}, {"event", eventPattern}} {
			if v := q.Get(c.name); v != "" {
				if !c.re.MatchString(v) || strings.Contains(v, "..") {
					return p, api.Errorf(api.CodeBadRequest, "invalid runs %s filter", c.name)
				}
				f.Set(c.name, v)
			}
		}
		if status := q.Get("status"); status != "" {
			switch status {
			case "queued", "in_progress", "waiting", "pending", "requested", "action_required", "completed":
				f.Set("status", status)
			default:
				return p, api.Errorf(api.CodeBadRequest, "invalid runs status filter")
			}
		}
		f.Set("per_page", "30")
		f.Set("page", strconv.Itoa(p.page))
		if wf := q.Get("workflow"); wf != "" {
			if !workflowPattern.MatchString(wf) || strings.Contains(wf, "..") {
				return p, api.Errorf(api.CodeBadRequest, "workflow must be a workflow file name such as ci.yml")
			}
			path += "/actions/workflows/" + wf + "/runs?" + f.Encode()
		} else {
			path += "/actions/runs?" + f.Encode()
		}
	case "jobs":
		id, err := positive(q.Get("run"))
		if err != nil {
			return p, err
		}
		p.run = id
		path += "/actions/runs/" + strconv.FormatInt(id, 10) + "/jobs?filter=latest&per_page=100&page=" + strconv.Itoa(p.page)
		p.ttl = 20 * time.Second
	case "runners":
		path += "/actions/runners?per_page=100&page=" + strconv.Itoa(p.page)
		p.ttl = 5 * time.Minute
	case "checks", "status":
		id, err := positive(q.Get("pull"))
		if err != nil {
			return p, err
		}
		p.pull, p.head = id, strings.ToLower(q.Get("head_sha"))
		if !shaPattern.MatchString(p.head) {
			return p, api.Errorf(api.CodeBadRequest, "head_sha must be a full commit SHA")
		}
		path += "/commits/" + p.head
		if p.resource == "checks" {
			path += "/check-runs?filter=latest&per_page=100&page=" + strconv.Itoa(p.page)
		} else {
			path += "/status?per_page=100&page=" + strconv.Itoa(p.page)
		}
	default:
		return p, api.Errorf(api.CodeBadRequest, "resource must be pulls, pull, files, runs, jobs, runners, checks or status")
	}
	p.args = append(p.args, path)
	p.key = p.resource + ":" + path
	return p, nil
}

func cursor(after string) (string, error) {
	if len(after) > 1024 || strings.ContainsAny(after, "\x00\r\n") {
		return "", api.Errorf(api.CodeBadRequest, "invalid pull request cursor")
	}
	return after, nil
}

func positive(s string) (int64, error) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 {
		return 0, api.Errorf(api.CodeBadRequest, "a positive pull or run id is required")
	}
	return n, nil
}

func (s *Service) knownRun(m orchapi.GitHubRepository, run int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	base := m.Project + "\x00" + m.Repo + "\x00"
	for key, e := range s.cache {
		if e.value.Stale || !s.Now().Before(e.expires) {
			continue
		}
		if strings.HasPrefix(key, base+"pull:") {
			// A merged pull's own Actions runs (its merge commit's check suites) are listed runs too.
			var pr struct {
				Data struct {
					Repository struct {
						Pull struct {
							Merge struct {
								Suites struct {
									Nodes []struct {
										Run struct {
											ID int64 `json:"databaseId"`
										} `json:"workflowRun"`
									} `json:"nodes"`
								} `json:"checkSuites"`
							} `json:"mergeCommit"`
						} `json:"pullRequest"`
					} `json:"repository"`
				} `json:"data"`
			}
			if json.Unmarshal(e.value.Payload, &pr) == nil {
				for _, n := range pr.Data.Repository.Pull.Merge.Suites.Nodes {
					if n.Run.ID == run {
						return true
					}
				}
			}
			continue
		}
		if !strings.HasPrefix(key, base+"runs:") {
			continue
		}
		var data struct {
			Runs []struct {
				ID int64 `json:"id"`
			} `json:"workflow_runs"`
		}
		if json.Unmarshal(e.value.Payload, &data) == nil {
			for _, r := range data.Runs {
				if r.ID == run {
					return true
				}
			}
		}
	}
	return false
}

func (s *Service) knownHead(m orchapi.GitHubRepository, pull int64, sha string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	var newest time.Time
	var found string
	base := m.Project + "\x00" + m.Repo + "\x00"
	for key, e := range s.cache {
		if e.value.Stale || !s.Now().Before(e.expires) || e.value.FetchedAt == nil {
			continue
		}
		if strings.HasPrefix(key, base+"pull:"+strconv.FormatInt(pull, 10)+":") {
			var one struct {
				Data struct {
					Repository struct {
						Pull struct {
							Head string `json:"headRefOid"`
						} `json:"pullRequest"`
					} `json:"repository"`
				} `json:"data"`
			}
			if json.Unmarshal(e.value.Payload, &one) == nil && one.Data.Repository.Pull.Head != "" && (found == "" || e.value.FetchedAt.After(newest)) {
				found, newest = strings.ToLower(one.Data.Repository.Pull.Head), *e.value.FetchedAt
			}
			continue
		}
		if !strings.HasPrefix(key, base+"pulls:") {
			continue
		}
		var data struct {
			Data struct {
				Repository struct {
					Pulls struct {
						Nodes []struct {
							Number int64  `json:"number"`
							Head   string `json:"headRefOid"`
						} `json:"nodes"`
					} `json:"pullRequests"`
				} `json:"repository"`
			} `json:"data"`
		}
		if json.Unmarshal(e.value.Payload, &data) != nil || e.value.FetchedAt == nil {
			continue
		}
		for _, n := range data.Data.Repository.Pulls.Nodes {
			if n.Number == pull && (found == "" || e.value.FetchedAt.After(newest)) {
				found, newest = strings.ToLower(n.Head), *e.value.FetchedAt
			}
		}
	}
	return found != "" && found == sha
}

func partial(data []byte, p request) bool {
	if p.resource == "pull" || p.resource == "files" {
		var d struct {
			Data struct {
				Repository struct {
					Pull struct {
						Files struct {
							Page struct {
								More bool `json:"hasNextPage"`
							} `json:"pageInfo"`
						} `json:"files"`
						Merge struct {
							Suites struct {
								Total int               `json:"totalCount"`
								Nodes []json.RawMessage `json:"nodes"`
							} `json:"checkSuites"`
						} `json:"mergeCommit"`
						Commits struct {
							Nodes []struct {
								Commit struct {
									Rollup struct {
										Contexts struct {
											Page struct {
												More bool `json:"hasNextPage"`
											} `json:"pageInfo"`
										} `json:"contexts"`
									} `json:"statusCheckRollup"`
								} `json:"commit"`
							} `json:"nodes"`
						} `json:"commits"`
					} `json:"pullRequest"`
				} `json:"repository"`
			} `json:"data"`
		}
		_ = json.Unmarshal(data, &d)
		pr := d.Data.Repository.Pull
		more := pr.Files.Page.More || pr.Merge.Suites.Total > len(pr.Merge.Suites.Nodes)
		for _, c := range pr.Commits.Nodes {
			more = more || c.Commit.Rollup.Contexts.Page.More
		}
		return more
	}
	if p.resource == "pulls" {
		var d struct {
			Data struct {
				Repository struct {
					Pulls struct {
						Page struct {
							More bool `json:"hasNextPage"`
						} `json:"pageInfo"`
					} `json:"pullRequests"`
				} `json:"repository"`
			} `json:"data"`
		}
		_ = json.Unmarshal(data, &d)
		return d.Data.Repository.Pulls.Page.More
	}
	var d struct {
		Count int `json:"total_count"`
	}
	_ = json.Unmarshal(data, &d)
	perPage := 100
	if p.resource == "runs" {
		perPage = 30
	}
	return d.Count > p.page*perPage
}
