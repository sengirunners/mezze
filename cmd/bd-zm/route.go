package main

// Routing by issue id (SPEC "Native issues", bd-compatible door): an id names
// its own project by its prefix, so a call on it names that project (as
// --project does) and a team replica sends it straight to that project's
// authority. The prefix map is GET /v0/issues/projects, kept in a small file
// for prefixTTL: bd is a process per call, so memory would not outlive it,
// and a warm call then costs no extra round trip.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// prefixTTL is how long a cached prefix map is used.
var prefixTTL = 30 * time.Second

// cacheHome is $ZM_HOME for the prefix map file ("" = no file: memory only).
// defaultClient sets it; tests set it to a temp dir.
var cacheHome string

// now is the clock (tests move it).
var now = time.Now

// prefixMap is what GET /v0/issues/projects answered, and when.
type prefixMap struct {
	Key string                `json:"key"`
	At  time.Time             `json:"at"`
	Raw json.RawMessage       `json:"projects"` // the answer as sent (newer fields kept)
	ps  orchapi.IssueProjects // decoded Raw
	// partial: an authority did not answer (a replica's "skipped"), so a
	// prefix missing here may still exist.
	partial bool
	fresh   bool // fetched by this process
}

// memo is this process's map (reset by run).
var memo *prefixMap

func cacheKey(c *client.Client) string {
	h := sha256.Sum256([]byte(c.Socket + "\x00" + getenv("ZM_HOST") + "\x00" + c.Token))
	return hex.EncodeToString(h[:])
}

func cachePath() string {
	if cacheHome == "" {
		return ""
	}
	return filepath.Join(cacheHome, "cache", "bd-prefixes.json")
}

func decodeMap(m *prefixMap) bool {
	var skipped struct {
		Skipped []json.RawMessage `json:"skipped"`
	}
	if json.Unmarshal(m.Raw, &m.ps) != nil || json.Unmarshal(m.Raw, &skipped) != nil {
		return false
	}
	m.partial = len(skipped.Skipped) > 0
	return true
}

// issueProjects is the prefix map: this process's, else the cached file's
// while younger than prefixTTL, else the daemon's (cached when complete).
// fresh skips the file.
func issueProjects(ctx context.Context, c *client.Client, fresh bool) (*prefixMap, error) {
	key := cacheKey(c)
	if memo != nil && memo.Key == key && (memo.fresh || !fresh) {
		return memo, nil
	}
	if !fresh {
		if m := readCache(key); m != nil {
			memo = m
			return m, nil
		}
	}
	var raw json.RawMessage
	if err := c.Do(ctx, http.MethodGet, "/v0/issues/projects", nil, &raw); err != nil {
		return nil, err
	}
	m := &prefixMap{Key: key, At: now(), Raw: raw, fresh: true}
	if !decodeMap(m) {
		return nil, fmt.Errorf("bad answer from GET /v0/issues/projects")
	}
	memo = m
	if !m.partial {
		writeCache(m)
	}
	return m, nil
}

func readCache(key string) *prefixMap {
	p := cachePath()
	if p == "" {
		return nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var m prefixMap
	if json.Unmarshal(b, &m) != nil || m.Key != key || !decodeMap(&m) || m.partial {
		return nil
	}
	if age := now().Sub(m.At); age < 0 || age >= prefixTTL {
		return nil
	}
	return &m
}

// writeCache stores m atomically (0600); a failure only costs the next call
// a round trip.
func writeCache(m *prefixMap) {
	p := cachePath()
	if p == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return
	}
	b, err := json.Marshal(m)
	if err != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".bd-prefixes-*")
	if err != nil {
		return
	}
	defer os.Remove(f.Name()) // after the rename, nothing to remove
	_, werr := f.Write(b)
	if cerr := f.Close(); werr == nil && cerr == nil {
		_ = os.Rename(f.Name(), p)
	}
}

// routed is an issue id and the client that names its project.
type routed struct {
	id      string
	project string // the id's project slug ("" = not routed: the daemon finds it)
	c       *client.Client
}

// unknownPrefix is a not_found for an id whose prefix no visible project has.
type unknownPrefix struct{ err *api.Error }

func (e unknownPrefix) Error() string { return e.err.Error() }
func (e unknownPrefix) Unwrap() error { return e.err }

// missing is bd's line for an id that was not found.
func missing(raw string, err error) string {
	var up unknownPrefix
	if errors.As(err, &up) {
		return fmt.Sprintf("Issue %s not found: %s", raw, up.err.Message)
	}
	return fmt.Sprintf("Issue %s not found", raw)
}

// route completes raw (resolveID) and names its project by its prefix. With
// --project (or a client that names a project from a flag) nothing is
// routed. A map that cannot be had leaves the id unnamed.
func route(ctx context.Context, c *client.Client, raw string) (routed, error) {
	if strings.HasPrefix(raw, "external:") {
		return routed{id: raw, c: c}, nil // text, never an issue: never completed or routed
	}
	id := resolveID(ctx, c, raw)
	r := routed{id: id, c: c}
	if c.ProjectFrom == "flag" || !strings.Contains(id, "-") {
		return r, nil
	}
	m, err := issueProjects(ctx, c, false)
	if err != nil {
		return r, nil
	}
	hits := match(m.ps.Projects, id)
	if len(hits) == 0 && !m.fresh {
		if m, err = issueProjects(ctx, c, true); err != nil {
			return r, nil
		}
		hits = match(m.ps.Projects, id)
	}
	switch {
	case len(hits) == 1:
		r.project = hits[0].Slug
		if r.project == "" {
			r.project = hits[0].Project
		}
		cc := *c
		cc.Project, cc.ProjectFrom = r.project, "flag"
		r.c = &cc
		return r, nil
	case len(hits) > 1:
		names := make([]string, len(hits))
		for i, h := range hits {
			if names[i] = h.Slug; names[i] == "" {
				names[i] = h.Project
			}
		}
		return r, api.Errorf(api.CodeBadRequest, "issue prefix %q of %s is used by %d projects you see (%s): name one, like bd --project %s ...",
			hits[0].Prefix, id, len(hits), strings.Join(names, ", "), names[0])
	case m.partial:
		return r, nil // its project's authority may be the one that did not answer
	}
	var seen []string
	for _, p := range m.ps.Projects {
		seen = append(seen, p.Prefix+"- ("+p.Slug+")")
	}
	sort.Strings(seen)
	msg := "no project you see uses its prefix"
	if len(seen) > 0 {
		msg += "; you see " + strings.Join(seen, ", ")
	}
	return r, unknownPrefix{api.Errorf(api.CodeNotFound, "%s", msg)}
}

// parentFlag is a list's --parent (or children's parent), for oneScope.
var parentFlag string

// oneScope refuses --all-projects with --project.
func oneScope() error {
	if allProjects && flagProject != "" {
		return errors.New("--all-projects lists every project you read; --project names one: give one of them")
	}
	if allProjects && parentFlag != "" {
		return errors.New("--all-projects lists every project you read; --parent lists its parent's: give one of them")
	}
	return nil
}

// parentOf is a --parent id and the project a list under it covers (its
// own; "" when not routed: bd's project, or --project).
func parentOf(ctx context.Context, c *client.Client, raw string) (string, string, error) {
	r, err := route(ctx, c, raw)
	if err != nil {
		return "", "", fmt.Errorf("--parent %s: %s", raw, errMsg(err))
	}
	return r.id, r.project, nil
}

// match is the projects whose prefix is id's longest matching one.
func match(ps []orchapi.IssueProjectInfo, id string) []orchapi.IssueProjectInfo {
	best := ""
	var out []orchapi.IssueProjectInfo
	for _, p := range ps {
		pre := p.Prefix
		if pre == "" || !strings.HasPrefix(id, pre+"-") {
			continue
		}
		switch {
		case len(pre) > len(best):
			best, out = pre, []orchapi.IssueProjectInfo{p}
		case pre == best:
			out = append(out, p)
		}
	}
	return out
}

// crossProject refuses a dependency between issues of two projects.
func crossProject(from, to routed, remove bool) error {
	if from.project == "" || to.project == "" || from.project == to.project {
		return nil
	}
	if remove {
		return fmt.Errorf("%s is in %s and %s in %s: no dependency joins two projects (one recorded as text is removed with bd dep remove %s external:%s)",
			from.id, from.project, to.id, to.project, from.id, to.id)
	}
	return fmt.Errorf("%s is in %s and %s in %s: a dependency lives in one project's issues, whose ready and blocked cannot follow another project's; record it without blocking: bd dep add %s external:%s --type related (or a comment)",
		from.id, from.project, to.id, to.project, from.id, to.id)
}
