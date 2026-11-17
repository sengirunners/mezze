package search

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/project"
)

// Service is the search module: GET /v0/search (SPEC 21).
type Service struct {
	d *core.Deps
	// Searcher is the driver (tests set it); nil = Pick() per request, so a
	// ripgrep installed while the daemon runs is used.
	Searcher Searcher
}

var _ core.Module = (*Service)(nil)

// New returns the search module.
func New(d *core.Deps) *Service { return &Service{d: d} }

// Routes registers GET /v0/search.
func (s *Service) Routes(r *core.Router) {
	r = r.For("search")
	r.HandleFunc("GET /v0/search", core.Local, "machine owner only: the named project's paths (or path, one of them)", s.handleSearch)
}

func (s *Service) handleSearch(w http.ResponseWriter, r *http.Request) {
	res, err := s.search(r)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, res)
}

func (s *Service) search(r *http.Request) (orchapi.SearchResult, error) {
	ctx, q := r.Context(), r.URL.Query()
	sub := core.SubjectOf(ctx)
	text := q.Get("q")
	switch {
	case text == "":
		return orchapi.SearchResult{}, api.Errorf(api.CodeBadRequest, "q is required")
	case len(text) > orchapi.SearchMaxQuery:
		return orchapi.SearchResult{}, api.Errorf(api.CodeBadRequest, "q is over %d bytes", orchapi.SearchMaxQuery)
	case strings.ContainsRune(text, 0) || strings.ContainsAny(text, "\r\n"):
		return orchapi.SearchResult{}, api.Errorf(api.CodeBadRequest, "q is one line of text")
	}
	ref := q.Get("project")
	if ref == "" {
		return orchapi.SearchResult{}, api.Errorf(api.CodeBadRequest, "project is required")
	}
	p, err := s.d.Projects.Get(sub.Scope, ref)
	if err != nil {
		return orchapi.SearchResult{}, err
	}
	paths := append([]string(nil), p.Paths...)
	for _, a := range p.Areas {
		paths = append(paths, a.Paths...)
	}
	// The project's roots as mcp and spawn see them: plain rules expanded,
	// globs replaced by the directories they match now.
	want := q.Get("path")
	if want != "" {
		want = project.ExpandPath(want)
	}
	var roots []string
	found := false
	seen := map[string]bool{}
	for _, e := range project.ExpandRoots(paths) {
		if want != "" && e != want {
			continue
		}
		found = true
		real, err := filepath.EvalSymlinks(e) // a root that is a symlink is resolved once
		if err != nil {
			continue
		}
		if fi, err := os.Stat(real); err != nil || !fi.IsDir() || seen[real] {
			continue
		}
		seen[real] = true
		roots = append(roots, real)
	}
	if want != "" && !found {
		return orchapi.SearchResult{}, api.Errorf(api.CodeBadRequest, "path %s is not one of project %s's paths", q.Get("path"), p.Slug)
	}
	roots = outermost(roots)
	regex := false
	switch q.Get("regex") {
	case "", "0", "false":
	case "1", "true":
		regex = true
	default:
		return orchapi.SearchResult{}, api.Errorf(api.CodeBadRequest, "regex must be 0 or 1")
	}
	drv := s.Searcher
	if drv == nil {
		drv = Pick()
	}
	if regex { // one check for both drivers (ripgrep's syntax is a superset of what Go accepts here)
		if _, err := compileRegex(text); err != nil {
			return orchapi.SearchResult{}, api.Errorf(api.CodeBadRequest, "invalid regex: %v", err)
		}
	}
	res, err := drv.Search(ctx, Query{Text: text, Regex: regex, Roots: roots})
	if err != nil {
		return orchapi.SearchResult{}, err
	}
	res.Project = p.ID
	if res.Roots == nil {
		res.Roots = []string{}
	}
	return res, nil
}

// outermost drops every root under another (an area's path inside the
// project's): each file is searched, and answered, once.
func outermost(roots []string) []string {
	var out []string
	for _, r := range roots {
		inside := false
		for _, o := range roots {
			if o != r && strings.HasPrefix(r, o+string(filepath.Separator)) {
				inside = true
				break
			}
		}
		if !inside {
			out = append(out, r)
		}
	}
	return out
}
