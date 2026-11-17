// Package search is the folder-search brick (SPEC 21): one Searcher
// interface with two drivers, ripgrep (rg on PATH) and a Go walker, and a
// conformance suite both pass (conformance_test.go). The route
// (GET /v0/search, http.go) picks a driver and the roots; drivers only
// search.
//
// Why our own walker next to ripgrep: zm must work where rg is not
// installed (a fresh Mac, a minimal box), and the walker is small. ripgrep
// is preferred whenever present (it is the maintained, faster tool).
package search

import (
	"cmp"
	"context"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// Query is one search.
type Query struct {
	Text  string
	Regex bool
	// Roots are absolute directories, already resolved (no symlinks).
	Roots []string
	// Limits; zero means the SPEC's.
	MaxMatches, MaxFiles int
	Timeout              time.Duration
}

// Searcher is a search driver.
type Searcher interface {
	Name() string
	// Search returns what it found within q's limits. A limit reached is not
	// an error: the result's Truncated says so.
	Search(ctx context.Context, q Query) (orchapi.SearchResult, error)
}

func (q *Query) defaults() {
	if q.MaxMatches <= 0 {
		q.MaxMatches = orchapi.SearchMaxMatches
	}
	if q.MaxFiles <= 0 {
		q.MaxFiles = orchapi.SearchMaxFiles
	}
	if q.Timeout <= 0 {
		q.Timeout = 5 * time.Second
	}
}

// collector applies the limits as matches arrive.
type collector struct {
	q     Query
	res   orchapi.SearchResult
	files map[string]bool
	trunc orchapi.SearchTruncated
}

func newCollector(q Query, engine string) *collector {
	return &collector{q: q, files: map[string]bool{},
		res: orchapi.SearchResult{Engine: engine, Roots: q.Roots, Matches: []orchapi.SearchMatch{}}}
}

// add records a match; false: a limit was reached, stop searching.
func (c *collector) add(file string, line int, text string, byteCol int) bool {
	if !c.inRoots(file) {
		return true // outside every root (a path a symlink led to): never answered
	}
	if !c.files[file] {
		if len(c.files) >= c.q.MaxFiles {
			c.trunc.Files = true
			return false
		}
		c.files[file] = true
	}
	if len(c.res.Matches) >= c.q.MaxMatches {
		c.trunc.Matches = true
		return false
	}
	text = strings.TrimRight(text, "\r\n")
	if byteCol < 0 || byteCol > len(text) {
		byteCol = 0
	}
	col := utf8.RuneCountInString(text[:byteCol]) + 1
	clipped, cut := clip(text)
	c.res.Matches = append(c.res.Matches, orchapi.SearchMatch{File: file, Line: line, Col: col, Text: clipped, TextClipped: cut})
	if len(c.res.Matches) >= c.q.MaxMatches {
		c.trunc.Matches = true
		return false
	}
	return true
}

// full reports a limit reached already.
func (c *collector) full() bool { return c.trunc.Matches || c.trunc.Files }

// inRoots reports file inside one of the roots.
func (c *collector) inRoots(file string) bool {
	for _, r := range c.q.Roots {
		if rel, err := filepath.Rel(r, file); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			return true
		}
	}
	return false
}

func (c *collector) result(timedOut bool) orchapi.SearchResult {
	// One answer order for every driver: by file (comparePaths), then line,
	// then column. Under a limit, which matches were kept may differ between
	// drivers (rg finds files in any order), never their order.
	sort.SliceStable(c.res.Matches, func(i, j int) bool {
		a, b := c.res.Matches[i], c.res.Matches[j]
		if n := comparePaths(a.File, b.File); n != 0 {
			return n < 0
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Col < b.Col
	})
	c.trunc.Time = c.trunc.Time || timedOut
	if c.trunc.Matches || c.trunc.Files || c.trunc.Time {
		t := c.trunc
		c.res.Truncated = &t
	}
	return c.res
}

// comparePaths orders paths segment by segment, each by its bytes: the order
// filepath.WalkDir visits, directories' contents in place, so dir/x comes
// before dir-b and dir.go. So the walker's matches under a limit are a true
// prefix of the whole answer. "/" sorts first: a segment ending where
// another goes on is the lesser.
func comparePaths(a, b string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		switch {
		case a[i] == b[i]:
		case a[i] == '/':
			return -1
		case b[i] == '/':
			return 1
		default:
			return cmp.Compare(a[i], b[i])
		}
	}
	return cmp.Compare(len(a), len(b))
}

// clip keeps at most api.SearchMaxLine characters of a line.
func clip(s string) (string, bool) {
	if utf8.RuneCountInString(s) <= orchapi.SearchMaxLine {
		return s, false
	}
	rs := []rune(s)
	return string(rs[:orchapi.SearchMaxLine-1]) + "…", true
}

// Pick returns the ripgrep driver when rg is on PATH, else the walker.
func Pick() Searcher {
	if rg, ok := NewRipgrep(""); ok {
		return rg
	}
	return Walk{}
}
