package browse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/storetest"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

func fixture(t *testing.T, fetch Fetch) (*Service, *authz.Subject, *authz.Subject, api.Project) {
	t.Helper()
	ctx := context.Background()
	home := t.TempDir()
	st, err := storetest.Open(ctx, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{Home: home, Human: "ivo", Machine: "mini"}
	reg, err := project.Open(ctx, st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	owner := reg.Local("")
	p, err := reg.Create(ctx, owner, api.NewProjectRequest{Slug: "main", RepoRules: api.RepoRules{Remotes: []string{"github.com/acme/private"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = reg.AddArea(ctx, owner, p.ID, api.Area{Name: "public", RepoRules: api.RepoRules{Remotes: []string{"github.com/acme/area"}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = reg.Create(ctx, owner, api.NewProjectRequest{Slug: "side", RepoRules: api.RepoRules{Remotes: []string{"github.com/acme/other"}}})
	if err != nil {
		t.Fatal(err)
	}
	tok, err := reg.Mint(ctx, owner, api.MintTokenRequest{As: "guest", Project: p.ID, Area: "public", Role: "guest"})
	if err != nil {
		t.Fatal(err)
	}
	guest, err := reg.FromToken(ctx, tok.Secret, false)
	if err != nil {
		t.Fatal(err)
	}
	return New(&core.Deps{Config: cfg, Store: st, Projects: reg}, fetch), owner, guest, p
}

func query(s *Service, sub *authz.Subject, project, repo, resource string, extra url.Values) (orchapi.GitHubData, error) {
	q := url.Values{"project": {project}, "repo": {repo}, "resource": {resource}}
	for key, values := range extra {
		q[key] = values
	}
	r := httptest.NewRequest("GET", "/v0/github/data?"+q.Encode(), nil)
	r = r.WithContext(core.WithSubject(r.Context(), sub))
	return s.data(r, sub)
}

func TestScopeBeforeCacheOrUpstream(t *testing.T) {
	var calls atomic.Int32
	s, owner, guest, p := fixture(t, func(context.Context, string, ...string) ([]byte, error) {
		calls.Add(1)
		return []byte(`{"workflow_runs":[],"total_count":0}`), nil
	})
	if _, err := query(s, owner, p.ID, "acme/private", "runs", nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][2]string{{p.ID, "acme/private"}, {p.ID, "acme/missing"}, {"side", "acme/other"}, {"missing", "acme/other"}} {
		v, err := query(s, guest, c[0], c[1], "runs", nil)
		if !api.IsCode(err, api.CodeNotFound) || len(v.Payload) != 0 {
			t.Fatalf("hidden/missing: %+v %v", v, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("unauthorized requests reached GitHub: %d", calls.Load())
	}
	r := httptest.NewRequest("GET", "/v0/github/repos", nil)
	r = r.WithContext(core.WithSubject(r.Context(), guest))
	list, err := s.repositories(r, guest)
	if err != nil || len(list.Repositories) != 1 || list.Repositories[0].Repo != "acme/area" {
		t.Fatalf("scoped repos: %+v %v", list, err)
	}
	if _, err := query(s, guest, p.ID, "acme/area", "runs", nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("authorized area did not fetch independently")
	}
}

func TestOneRefreshForConcurrentReaders(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s, owner, _, p := fixture(t, func(context.Context, string, ...string) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return []byte(`{"workflow_runs":[]}`), nil
	})
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := query(s, owner, p.ID, "acme/private", "runs", nil); errs <- err }()
	}
	<-started
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("refreshes = %d", calls.Load())
	}
}

func TestInvalidationRejectsLateRefresh(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s, owner, _, p := fixture(t, func(context.Context, string, ...string) ([]byte, error) {
		if calls.Add(1) == 1 {
			close(started)
			<-release
			return []byte(`{"workflow_runs":[{"id":1}]}`), nil
		}
		return []byte(`{"workflow_runs":[{"id":2}]}`), nil
	})
	errCh := make(chan error, 1)
	go func() { _, err := query(s, owner, p.ID, "acme/private", "runs", nil); errCh <- err }()
	<-started
	s.Invalidate(p.ID, "acme/private")
	close(release)
	if err := <-errCh; !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("late response: %v", err)
	}
	v, err := query(s, owner, p.ID, "acme/private", "runs", nil)
	if err != nil || !bytes.Contains(v.Payload, []byte(`"id":2`)) {
		t.Fatalf("fresh result: %s %v", v.Payload, err)
	}
}

func TestHeadAndRunAreBoundToMappedSnapshots(t *testing.T) {
	sha := strings.Repeat("a", 40)
	var calls atomic.Int32
	s, owner, _, p := fixture(t, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls.Add(1)
		if len(args) > 1 && args[1] == "graphql" {
			return []byte(`{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"number":7,"headRefOid":"` + sha + `"}]}}}}`), nil
		}
		if strings.Contains(strings.Join(args, " "), "/actions/runs?") {
			return []byte(`{"workflow_runs":[{"id":19}]}`), nil
		}
		return []byte(`{"check_runs":[],"jobs":[]}`), nil
	})
	extra := url.Values{"pull": {"7"}, "head_sha": {sha}}
	if _, err := query(s, owner, p.ID, "acme/private", "checks", extra); !api.IsCode(err, api.CodeConflict) {
		t.Fatal(err)
	}
	if _, err := query(s, owner, p.ID, "acme/private", "jobs", url.Values{"run": {"19"}}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatal(err)
	}
	if calls.Load() != 0 {
		t.Fatal("unbound checks/jobs reached upstream")
	}
	if _, err := query(s, owner, p.ID, "acme/private", "pulls", nil); err != nil {
		t.Fatal(err)
	}
	v, err := query(s, owner, p.ID, "acme/private", "checks", extra)
	if err != nil || v.HeadSHA != sha {
		t.Fatalf("check head: %+v %v", v, err)
	}
	s.Invalidate(p.ID, "acme/private")
	sha = strings.Repeat("b", 40)
	if _, err := query(s, owner, p.ID, "acme/private", "pulls", nil); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	if _, err := query(s, owner, p.ID, "acme/private", "checks", extra); !api.IsCode(err, api.CodeConflict) {
		t.Fatal("old head accepted", err)
	}
	if calls.Load() != before {
		t.Fatal("old head reached upstream")
	}
	if _, err := query(s, owner, p.ID, "acme/private", "runs", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := query(s, owner, p.ID, "acme/private", "jobs", url.Values{"run": {"19"}}); err != nil {
		t.Fatal(err)
	}
}

func TestFailureRetainsStaleDataAndBacksOffWithoutLeakingErrors(t *testing.T) {
	now := time.Unix(1700000000, 0)
	fail := false
	var calls int
	s, owner, _, p := fixture(t, func(context.Context, string, ...string) ([]byte, error) {
		calls++
		if fail {
			return nil, errors.New("credential TOPSECRET server debug")
		}
		return []byte(`{"workflow_runs":[{"id":1}]}`), nil
	})
	s.Now = func() time.Time { return now }
	first, err := query(s, owner, p.ID, "acme/private", "runs", nil)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(61 * time.Second)
	fail = true
	v, err := query(s, owner, p.ID, "acme/private", "runs", nil)
	if err != nil || !v.Stale || v.Error == nil || string(v.Payload) != string(first.Payload) || !v.FetchedAt.Equal(*first.FetchedAt) {
		t.Fatalf("stale: %+v %v", v, err)
	}
	encoded, _ := json.Marshal(v)
	if strings.Contains(string(encoded), "TOPSECRET") {
		t.Fatal("upstream error leaked")
	}
	if _, err := query(s, owner, p.ID, "acme/private", "runs", nil); err != nil || calls != 2 {
		t.Fatalf("backoff %d %v", calls, err)
	}
}

func TestBoundedResourcePlan(t *testing.T) {
	m := orchapi.GitHubRepository{Project: "p", Repo: "acme/private"}
	for _, q := range []url.Values{{"resource": {"mutation"}}, {"resource": {"runs"}, "url": {"https://evil.test"}}, {"resource": {"runs"}, "page": {"999"}}, {"resource": {"pulls"}, "after": {strings.Repeat("x", 1025)}}, {"resource": {"checks"}, "pull": {"1"}, "head_sha": {"main"}}} {
		if _, err := plan(q, m); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("invalid plan accepted: %v %v", q, err)
		}
	}
	for _, bad := range []string{"https://evil.test/a/b", "acme/../private", "../private", "acme/..", "acme/private?token=x"} {
		if repository(bad) != "" {
			t.Fatalf("bad repository %q", bad)
		}
	}
}

// The tests above fake the upstream, so nothing else parses pullQuery; an unclosed brace made GitHub refuse
// every pulls read while the fakes still passed.
func TestPullQueryIsBalanced(t *testing.T) {
	var stack []rune
	for i, r := range pullQuery {
		switch r {
		case '{', '(':
			stack = append(stack, r)
		case '}', ')':
			open := map[rune]rune{'}': '{', ')': '('}[r]
			if len(stack) == 0 || stack[len(stack)-1] != open {
				t.Fatalf("pullQuery: unmatched %q at %d", r, i)
			}
			stack = stack[:len(stack)-1]
		}
	}
	if len(stack) != 0 {
		t.Fatalf("pullQuery: %d unclosed %q", len(stack), string(stack))
	}
	p, err := plan(url.Values{"resource": {"pulls"}}, orchapi.GitHubRepository{Project: "p", Repo: "acme/private"})
	if err != nil || !strings.Contains(strings.Join(p.args, "\n"), "query="+pullQuery) {
		t.Fatalf("pulls plan does not send pullQuery: %v %v", p.args, err)
	}
}

func TestChecksNeverDescribeAnotherHead(t *testing.T) {
	data := []byte(`{"data":{"repository":{"pullRequests":{"nodes":[{"number":1,"headRefOid":"new","commits":{"nodes":[{"commit":{"oid":"old","statusCheckRollup":{"state":"SUCCESS"}}}]}}]}}}}`)
	clean, err := checkedPayload(data, request{resource: "pulls"})
	if err != nil || bytes.Contains(clean, []byte("SUCCESS")) {
		t.Fatalf("old green survived: %s %v", clean, err)
	}
	for _, c := range []struct{ resource, payload string }{
		{"checks", `{"check_runs":[{"head_sha":"old","conclusion":"success"}]}`},
		{"status", `{"sha":"old","state":"success"}`},
		{"runs", `{}`}, {"jobs", `{"jobs":null}`},
	} {
		if _, err := checkedPayload([]byte(c.payload), request{resource: c.resource, head: "new"}); err == nil {
			t.Fatalf("accepted inconsistent %s: %s", c.resource, c.payload)
		}
	}
}

// A PR shepherd reads one pull in any state, its files, filtered runs and its merge commit's jobs; each read is a
// fixed upstream shape, and filters cannot carry a path or a query of their own.
func TestPullFilesAndFilteredRunsPlan(t *testing.T) {
	m := orchapi.GitHubRepository{Project: "p", Repo: "acme/private"}
	for _, gql := range []string{pullOneQuery, filesQuery} {
		depth := 0
		for _, r := range gql {
			switch r {
			case '{', '(':
				depth++
			case '}', ')':
				depth--
			}
			if depth < 0 {
				t.Fatalf("unbalanced query %q", gql)
			}
		}
		if depth != 0 {
			t.Fatalf("unclosed query %q", gql)
		}
	}
	p, err := plan(url.Values{"resource": {"pull"}, "pull": {"12"}}, m)
	if err != nil || p.pull != 12 || !strings.Contains(strings.Join(p.args, "\n"), "query="+pullOneQuery+"\n") || !strings.Contains(strings.Join(p.args, "\n"), "number=12") {
		t.Fatalf("pull plan: %+v %v", p, err)
	}
	p, err = plan(url.Values{"resource": {"files"}, "pull": {"12"}, "after": {"Y3Vyc29y"}}, m)
	if err != nil || !strings.Contains(strings.Join(p.args, "\n"), "after=Y3Vyc29y") || p.key != "files:12:Y3Vyc29y" {
		t.Fatalf("files plan: %+v %v", p, err)
	}
	p, err = plan(url.Values{"resource": {"runs"}, "workflow": {"review-verdict.yml"}, "branch": {"zm-x/fix.2"}, "event": {"pull_request"}}, m)
	if err != nil || p.args[len(p.args)-1] != "repos/acme/private/actions/workflows/review-verdict.yml/runs?branch=zm-x%2Ffix.2&event=pull_request&page=1&per_page=30" || !strings.HasPrefix(p.key, "runs:") {
		t.Fatalf("filtered runs plan: %+v %v", p.args, err)
	}
	for _, q := range []url.Values{
		{"resource": {"pull"}}, {"resource": {"pull"}, "pull": {"-1"}}, {"resource": {"pull"}, "pull": {"1"}, "after": {"x"}},
		{"resource": {"files"}, "pull": {"1"}, "after": {"a\nb"}},
		{"resource": {"runs"}, "workflow": {"../../x.yml"}}, {"resource": {"runs"}, "workflow": {"ci"}},
		{"resource": {"runs"}, "workflow": {"a/b.yml"}}, {"resource": {"runs"}, "branch": {"a..b"}},
		{"resource": {"runs"}, "branch": {"a&per_page=100"}}, {"resource": {"runs"}, "event": {"push&x=1"}},
	} {
		if _, err := plan(q, m); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("invalid plan accepted: %v %v", q, err)
		}
	}
}

func TestPullPayloadIsTheAskedPullAndItsHead(t *testing.T) {
	for _, bad := range []string{
		`{"data":{"repository":{"pullRequest":null}},"errors":[{"type":"NOT_FOUND"}]}`,
		`{"data":{"repository":{"pullRequest":{"number":8,"headRefOid":"h"}}}}`,
		`{"data":null}`,
	} {
		if _, err := checkedPayload([]byte(bad), request{resource: "pull", pull: 7}); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	old := []byte(`{"data":{"repository":{"pullRequest":{"number":7,"headRefOid":"new","commits":{"nodes":[{"commit":{"oid":"old","statusCheckRollup":{"contexts":{"nodes":[{"name":"ci-passed","conclusion":"SUCCESS"}]}}}}]}}}}}`)
	clean, err := checkedPayload(old, request{resource: "pull", pull: 7})
	if err != nil || bytes.Contains(clean, []byte("SUCCESS")) {
		t.Fatalf("old head's checks survived: %s %v", clean, err)
	}
}

func TestMergedPullNamesItsRunsForJobs(t *testing.T) {
	var calls atomic.Int32
	s, owner, guest, p := fixture(t, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls.Add(1)
		if len(args) > 1 && args[1] == "graphql" {
			return []byte(`{"data":{"repository":{"pullRequest":{"number":7,"state":"MERGED","headRefOid":"h","mergeCommit":{"oid":"m","checkSuites":{"totalCount":1,"nodes":[{"status":"COMPLETED","conclusion":"SUCCESS","workflowRun":{"databaseId":41,"workflow":{"resourcePath":"/acme/private/actions/workflows/deploy.yml"}}}]}},"files":{"pageInfo":{"hasNextPage":true}}}}}}`), nil
		}
		return []byte(`{"jobs":[],"total_count":0}`), nil
	})
	if _, err := query(s, owner, p.ID, "acme/private", "jobs", url.Values{"run": {"41"}}); !api.IsCode(err, api.CodeNotFound) || calls.Load() != 0 {
		t.Fatalf("unlisted run read: %v", err)
	}
	if _, err := query(s, guest, p.ID, "acme/private", "pull", url.Values{"pull": {"7"}}); !api.IsCode(err, api.CodeNotFound) || calls.Load() != 0 {
		t.Fatalf("hidden repo pull read: %v", err)
	}
	v, err := query(s, owner, p.ID, "acme/private", "pull", url.Values{"pull": {"7"}})
	if err != nil || !v.Partial {
		t.Fatalf("pull: %+v %v", v, err)
	}
	if _, err := query(s, owner, p.ID, "acme/private", "jobs", url.Values{"run": {"41"}}); err != nil {
		t.Fatalf("merge commit's run: %v", err)
	}
	if _, err := query(s, owner, p.ID, "acme/private", "jobs", url.Values{"run": {"42"}}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("other run: %v", err)
	}
}

func TestPullReviewFixes(t *testing.T) {
	sha := strings.Repeat("c", 40)
	var missing atomic.Bool
	s, owner, _, p := fixture(t, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		if missing.Load() {
			return []byte(`{"data":{"repository":{"pullRequest":null}},"errors":[{"type":"NOT_FOUND"}]}`), errors.New("gh api: exit 1")
		}
		if len(args) > 1 && args[1] == "graphql" {
			return []byte(`{"data":{"repository":{"pullRequest":{"number":9,"headRefOid":"` + sha + `","commits":{"nodes":[{"commit":{"oid":"` + sha + `","statusCheckRollup":{"contexts":{"pageInfo":{"hasNextPage":true},"nodes":[]}}}}]},"files":{"pageInfo":{"hasNextPage":false}}}}}}`), nil
		}
		return []byte(`{"sha":"` + sha + `","state":"success"}`), nil
	})
	v, err := query(s, owner, p.ID, "acme/private", "pull", url.Values{"pull": {"9"}})
	if err != nil || !v.Partial {
		t.Fatalf("truncated contexts not partial: %+v %v", v, err)
	}
	// A pull read binds its head for checks and status, like a pulls page.
	if _, err := query(s, owner, p.ID, "acme/private", "status", url.Values{"pull": {"9"}, "head_sha": {sha}}); err != nil {
		t.Fatalf("status for a pull-read head: %v", err)
	}
	missing.Store(true)
	if _, err := query(s, owner, p.ID, "acme/private", "pull", url.Values{"pull": {"404"}}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("missing pull: %v", err)
	}
	if _, err := checkedPayload([]byte(`{"data":{"repository":{"pullRequest":{"number":3,"files":{}}}}}`), request{resource: "files", pull: 4}); err == nil {
		t.Fatal("files for another pull accepted")
	}
	if _, err := plan(url.Values{"resource": {"pull"}, "pull": {"1"}, "page": {"2"}}, orchapi.GitHubRepository{Project: "p", Repo: "a/b"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatal("page accepted on pull")
	}
}

// Status is a bounded filter on the existing mapped-repo read, and part of its
// cache key: CI status must find an old queued run among many completed runs.
func TestRunsStatusFilter(t *testing.T) {
	var calls int
	s, owner, guest, p := fixture(t, func(_ context.Context, _ string, args ...string) ([]byte, error) {
		calls++
		path := args[len(args)-1]
		if !strings.Contains(path, "status=queued") && !strings.Contains(path, "status=in_progress") {
			t.Errorf("lost active-run filter: %s", path)
		}
		return []byte(`{"workflow_runs":[],"total_count":0}`), nil
	})
	for _, status := range []string{"queued", "in_progress", "queued"} {
		if _, err := query(s, owner, p.ID, "acme/private", "runs", url.Values{"status": {status}}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 {
		t.Fatalf("filter cache keys overlap: %d calls", calls)
	}
	if _, err := query(s, owner, p.ID, "acme/private", "runs", url.Values{"status": {"queued&per_page=1000"}}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("unbounded filter: %v", err)
	}
	if _, err := query(s, guest, p.ID, "acme/private", "runs", url.Values{"status": {"queued"}}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("hidden read: %v", err)
	}
	if calls != 2 {
		t.Fatal("invalid or hidden read reached GitHub")
	}
}
