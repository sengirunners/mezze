package browse

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

type Fetch func(context.Context, string, ...string) ([]byte, error)

const maxEntries = 128
const maxCacheBytes = 32 << 20
const maxPayload = 2 << 20

type entry struct {
	value      orchapi.GitHubData
	expires    time.Time
	used       time.Time
	running    chan struct{}
	generation uint64
}

type Service struct {
	d     *core.Deps
	fetch Fetch
	mu    sync.Mutex
	cache map[string]*entry
	bytes int
	slots chan struct{}
	Now   func() time.Time
}

func New(d *core.Deps, fetch Fetch) *Service {
	return &Service{d: d, fetch: fetch, cache: map[string]*entry{}, slots: make(chan struct{}, 4), Now: time.Now}
}

func (s *Service) Routes(r *core.Router) {
	r.HandleFunc("GET /v0/github/repos", core.Authed, "read: explicit project/area repository mappings in ReadScope, plus joined projects' from the team header for the socket user; hidden and missing match", s.handleRepos)
	r.HandleFunc("GET /v0/github/data", core.Authed, "read: mapped repositories only; fixed bounded resources, shared cache, no caller upstream URL or query", s.handleData)
}

func (s *Service) handleRepos(w http.ResponseWriter, r *http.Request) {
	v, err := s.repositories(r, core.SubjectOf(r.Context()))
	answer(w, v, err)
}

func (s *Service) handleData(w http.ResponseWriter, r *http.Request) {
	v, err := s.data(r, core.SubjectOf(r.Context()))
	answer(w, v, err)
}

func answer(w http.ResponseWriter, v any, err error) {
	if err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, v)
}

func (s *Service) data(r *http.Request, sub *authz.Subject) (orchapi.GitHubData, error) {
	mapping, err := s.mapped(r, sub)
	if err != nil {
		return orchapi.GitHubData{}, err
	}
	request, err := plan(r.URL.Query(), mapping)
	if err != nil {
		return orchapi.GitHubData{}, err
	}
	if request.run != 0 && !s.knownRun(mapping, request.run) {
		return orchapi.GitHubData{}, api.NotFound("workflow run", strconv.FormatInt(request.run, 10))
	}
	if request.head != "" && !s.knownHead(mapping, request.pull, request.head) {
		return orchapi.GitHubData{}, api.Errorf(api.CodeConflict, "refresh the pull request before reading checks for its head")
	}
	return s.read(r.Context(), mapping, request)
}

func (s *Service) key(m orchapi.GitHubRepository, p request) string {
	return m.Project + "\x00" + m.Repo + "\x00" + p.key
}

func (s *Service) read(ctx context.Context, mapping orchapi.GitHubRepository, p request) (orchapi.GitHubData, error) {
	key := s.key(mapping, p)
	for {
		now := s.Now().UTC()
		s.mu.Lock()
		e := s.cache[key]
		if e != nil && e.running != nil {
			ready := e.running
			s.mu.Unlock()
			select {
			case <-ready:
				continue
			case <-ctx.Done():
				return orchapi.GitHubData{}, ctx.Err()
			}
		}
		if e != nil && now.Before(e.expires) {
			e.used = now
			v := e.value
			s.mu.Unlock()
			return v, nil
		}
		if e == nil {
			if !s.makeRoom(0, key) {
				s.mu.Unlock()
				return orchapi.GitHubData{}, api.Errorf(api.CodeUnavailable, "GitHub refresh capacity is busy")
			}
			e = &entry{}
			s.cache[key] = e
		}
		e.running, e.used = make(chan struct{}), now
		previous := e.value
		generation := e.generation
		s.mu.Unlock()

		data, fetchErr := s.boundedFetch(ctx, mapping.Project, p.args)
		if fetchErr != nil && (p.resource == "pull" || p.resource == "files") && json.Valid(data) {
			if _, err := checkedPayload(data, p); api.IsCode(err, api.CodeNotFound) {
				fetchErr = err
			}
		}
		if fetchErr == nil && (len(data) > maxPayload || !json.Valid(data)) {
			fetchErr = api.Errorf(api.CodeUnavailable, "invalid GitHub response")
		}
		if fetchErr == nil {
			data, fetchErr = checkedPayload(data, p)
		}
		if api.IsCode(fetchErr, api.CodeNotFound) {
			// A missing pull answers like a hidden one, not as an outage; nothing is cached for it.
			s.mu.Lock()
			close(e.running)
			e.running = nil
			e.expires = time.Time{}
			s.mu.Unlock()
			return orchapi.GitHubData{}, fetchErr
		}
		fetched := s.Now().UTC()
		v := orchapi.GitHubData{Project: mapping.Project, Repo: mapping.Repo, Resource: p.resource, HeadSHA: p.head}
		if fetchErr == nil {
			v.Payload, v.FetchedAt = json.RawMessage(data), &fetched
			v.Partial = partial(data, p)
		} else {
			v = previous
			v.Project, v.Repo, v.Resource, v.HeadSHA = mapping.Project, mapping.Repo, p.resource, p.head
			v.Stale = true
			v.Error = &orchapi.GitHubReadError{Code: "github_unavailable", Message: "GitHub could not be read. Check the daemon's GitHub login or try again later.", RetryAfter: fetched.Add(time.Minute)}
		}
		s.mu.Lock()
		if generation != e.generation {
			close(e.running)
			e.running = nil
			e.expires = time.Time{}
			s.mu.Unlock()
			return orchapi.GitHubData{}, api.Errorf(api.CodeConflict, "GitHub changed during refresh; try again")
		}
		s.bytes -= len(e.value.Payload)
		if !s.makeRoom(len(v.Payload), key) {
			v.Payload, v.FetchedAt = nil, nil
			v.Stale = true
			v.Error = &orchapi.GitHubReadError{Code: "github_busy", Message: "GitHub cache capacity is busy. Try again later.", RetryAfter: fetched.Add(time.Minute)}
		}
		e.value, e.expires = v, fetched.Add(p.ttl)
		if fetchErr != nil {
			e.expires = fetched.Add(time.Minute)
		}
		s.bytes += len(v.Payload)
		close(e.running)
		e.running = nil
		if ctx.Err() != nil {
			e.expires = fetched
		}
		s.makeRoom(0, key)
		s.mu.Unlock()
		return v, nil
	}
}

// Invalidate fences in-flight reads as well as cached values after a PR event.
func (s *Service) Invalidate(project, repo string) {
	prefix := project + "\x00" + strings.ToLower(repo) + "\x00"
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, e := range s.cache {
		if strings.HasPrefix(key, prefix) {
			s.bytes -= len(e.value.Payload)
			e.value = orchapi.GitHubData{}
			e.expires = time.Time{}
			e.generation++
		}
	}
}

func (s *Service) boundedFetch(ctx context.Context, project string, args []string) ([]byte, error) {
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	bounded, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return s.fetch(bounded, project, args...)
}

// Call only under mu; running entries are never evicted out from under waiters.
func (s *Service) makeRoom(extra int, keep string) bool {
	for len(s.cache) >= maxEntries || s.bytes+extra > maxCacheBytes {
		oldest := ""
		var used time.Time
		for key, e := range s.cache {
			if key == keep || e.running != nil {
				continue
			}
			if oldest == "" || e.used.Before(used) {
				oldest, used = key, e.used
			}
		}
		if oldest == "" {
			return false
		}
		s.bytes -= len(s.cache[oldest].value.Payload)
		delete(s.cache, oldest)
	}
	return true
}

// notFound: GitHub's GraphQL says the pull request does not exist (answered like a hidden one).
func notFound(errors []json.RawMessage) bool {
	for _, raw := range errors {
		var e struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Type == "NOT_FOUND" {
			return true
		}
	}
	return false
}

func checkedPayload(data []byte, p request) ([]byte, error) {
	var object map[string]json.RawMessage
	if json.Unmarshal(data, &object) != nil || object == nil {
		return nil, api.Errorf(api.CodeUnavailable, "invalid GitHub response")
	}
	if p.resource == "pull" || p.resource == "files" {
		var errors []json.RawMessage
		_ = json.Unmarshal(object["errors"], &errors)
		var root map[string]any
		_ = json.Unmarshal(object["data"], &root)
		repo, _ := root["repository"].(map[string]any)
		node, _ := repo["pullRequest"].(map[string]any)
		if node == nil && notFound(errors) {
			return nil, api.NotFound("pull request", strconv.FormatInt(p.pull, 10))
		}
		if len(errors) != 0 || node == nil {
			return nil, api.Errorf(api.CodeUnavailable, "GitHub query failed")
		}
		if number, _ := node["number"].(float64); int64(number) != p.pull {
			return nil, api.Errorf(api.CodeUnavailable, "GitHub returned a different pull request")
		}
		delete(object, "errors")
		// As for pulls: the checks shown must be the head's, never an older commit's.
		head, _ := node["headRefOid"].(string)
		if commits, ok := node["commits"].(map[string]any); ok {
			items, _ := commits["nodes"].([]any)
			oid := ""
			if len(items) != 0 {
				item, _ := items[len(items)-1].(map[string]any)
				commit, _ := item["commit"].(map[string]any)
				oid, _ = commit["oid"].(string)
			}
			if head == "" || head != oid {
				delete(node, "commits")
			}
		}
		object["data"], _ = json.Marshal(root)
		return json.Marshal(object)
	}
	if p.resource == "pulls" {
		var errors []json.RawMessage
		_ = json.Unmarshal(object["errors"], &errors)
		if len(errors) != 0 || len(object["data"]) == 0 || strings.TrimSpace(string(object["data"])) == "null" {
			return nil, api.Errorf(api.CodeUnavailable, "GitHub query failed")
		}
		delete(object, "errors")
		// A check summary must describe the same commit as its PR, even if an
		// upstream response is partial or internally inconsistent.
		var root map[string]any
		_ = json.Unmarshal(object["data"], &root)
		repo, _ := root["repository"].(map[string]any)
		if repo == nil {
			return nil, api.Errorf(api.CodeUnavailable, "GitHub repository is unavailable")
		}
		pulls, _ := repo["pullRequests"].(map[string]any)
		nodes, _ := pulls["nodes"].([]any)
		for _, raw := range nodes {
			node, _ := raw.(map[string]any)
			head, _ := node["headRefOid"].(string)
			commits, _ := node["commits"].(map[string]any)
			items, _ := commits["nodes"].([]any)
			oid := ""
			if len(items) != 0 {
				item, _ := items[len(items)-1].(map[string]any)
				commit, _ := item["commit"].(map[string]any)
				oid, _ = commit["oid"].(string)
			}
			if head == "" || head != oid {
				delete(node, "commits")
			}
		}
		object["data"], _ = json.Marshal(root)
	} else if _, failed := object["message"]; failed {
		return nil, api.Errorf(api.CodeUnavailable, "GitHub read failed")
	}
	arrayKey := map[string]string{"runs": "workflow_runs", "jobs": "jobs", "runners": "runners", "checks": "check_runs"}[p.resource]
	if arrayKey != "" {
		var rows []json.RawMessage
		if json.Unmarshal(object[arrayKey], &rows) != nil || rows == nil {
			return nil, api.Errorf(api.CodeUnavailable, "incomplete GitHub response")
		}
	}
	if p.resource == "checks" {
		var d struct {
			Runs []struct {
				Head string `json:"head_sha"`
			} `json:"check_runs"`
		}
		_ = json.Unmarshal(data, &d)
		for _, run := range d.Runs {
			if run.Head != p.head {
				return nil, api.Errorf(api.CodeUnavailable, "GitHub returned checks for a different head")
			}
		}
	}
	if p.resource == "status" {
		var d struct {
			SHA string `json:"sha"`
		}
		_ = json.Unmarshal(data, &d)
		if d.SHA != p.head {
			return nil, api.Errorf(api.CodeUnavailable, "GitHub returned status for a different head")
		}
	}
	return json.Marshal(object)
}
