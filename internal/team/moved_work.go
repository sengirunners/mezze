package team

// A work item's id that moved (zm issues move). The replica's copy of a project
// drops an issue that left it (dev.zm.work.moved), and a project's token cannot
// see the project the issue went to, so the old id is resolved the way
// forwardIssue resolves it: a GET of the id at the joined projects, whose
// authority follows the redirect for this machine's token there.
//
// Reads (GET /v0/work/{id}[/holder|/comments]) and mail follow. A write on the
// item (claim, comment, status) names the id it has now.

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/sengirunners/potluck/kit/teamlink"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/project"
)

// workRead says whether r reads one work item, with its id and what follows it
// in the path.
func workRead(r *http.Request) (id, rest string, ok bool) {
	if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/v0/work/") {
		return "", "", false
	}
	id, verb := segs(r.URL.Path, "/v0/work/")
	if a, err := api.ParseAddress(id); err != nil || a.Kind != api.KindWork {
		return "", "", false
	}
	switch verb {
	case "":
		return id, "", true
	case "holder", "comments":
		return id, "/" + verb, true
	}
	return "", "", false
}

// forwardWorkRead forwards a work read to the project named for it, then, on a
// 404, follows the id if it moved.
func (s *Service) forwardWorkRead(w http.ResponseWriter, r *http.Request, x *remote, area string) {
	resp, err := s.ForwardResp(r, x, area, true, nil)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound && s.followMovedRead(w, r, x) {
		return
	}
	s.relay(w, r, resp)
}

// resolveWait bounds resolving an id: a miss should not hold up a send for long.
const resolveWait = 3 * time.Second

// followMovedRead answers a work read whose id moved, or reports false and
// writes nothing. x, when a project was named, keeps the question on its
// authority: a move never crosses authorities.
func (s *Service) followMovedRead(w http.ResponseWriter, r *http.Request, x *remote) bool {
	id, rest, ok := workRead(r)
	if !ok {
		return false
	}
	y, nid, moved, err := s.resolveWork(r, x, id)
	if err != nil {
		api.WriteError(w, err)
		return true
	}
	if y == nil || !moved {
		return false
	}
	g := r.Clone(r.Context())
	g.URL.Path, g.URL.RawPath = "/v0/work/"+nid+rest, "/v0/work/"+url.PathEscape(nid)+rest
	dropProject(g)
	g.Body, g.ContentLength = http.NoBody, 0
	if rp := rep(y); rp.isReady() && s.serveCopy(w, g, y, rp, "", []byte{}) {
		return true
	}
	s.Forward(w, g, y, "", false)
	return true
}

// resolveWork asks the joined projects, on x's authority when x is known, for
// work item id: the project that has it, its work id, and whether that id is
// new. Each authority follows the redirect for this machine's token there, and
// only for a native tracker's id. It never waits long or on an authority known
// to be away. No project found is no project and no error: the caller's usual
// answer stands. An authority that failed or did not answer is an error, not a
// not_found it never gave.
func (s *Service) resolveWork(r *http.Request, x *remote, id string) (*remote, string, bool, error) {
	var ys []*remote
	for _, y := range s.List() {
		rp := rep(y)
		if teamlink.Ended(y.Snapshot()) != nil || !rp.isReady() || !rp.isReachable() {
			continue
		}
		if x == nil || y.Snapshot().Authority == x.Snapshot().Authority {
			ys = append(ys, y)
		}
	}
	if len(ys) == 0 {
		return nil, "", false, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), resolveWait)
	defer cancel()
	f, err := resolveMoved(ctx, ys, func(ctx context.Context, y *remote) (*http.Response, error) {
		g := r.Clone(ctx)
		g.Method, g.Body, g.ContentLength = http.MethodGet, http.NoBody, 0
		g.URL.Path, g.URL.RawPath = "/v0/work/"+id, "/v0/work/"+url.PathEscape(id)
		dropProject(g)
		g.Header.Del("Content-Type")
		return s.ForwardResp(g, y, "", false, []byte{})
	})
	if err != nil && (f == nil || f.id == "") {
		return nil, "", false, api.Errorf(api.CodeUnavailable, "looking up %s at the joined projects: %v", id, err)
	}
	if f == nil || f.id == "" {
		return nil, "", false, nil
	}
	moved := true
	if a, err := api.ParseAddress(id); err == nil && strings.EqualFold(a.String(), f.id) {
		moved = false // the same id, however it was spelled
	}
	return f.y, f.id, moved, nil
}

// followMovedMail makes POST /v0/mail reach a work item wherever it now is: a
// recipient or ref named by its old id is rewritten to the new one and the call
// names the item's project. Mail to an item in another joined project goes to
// that project too, unless the caller named a project with --project. Mail to
// everyone or to the owners stays put, and a send from this machine's own
// project never waits on an authority. Anything the copies and authorities
// cannot settle goes on as typed.
func (s *Service) followMovedMail(r *http.Request) error {
	if r.Method != http.MethodPost || r.URL.Path != "/v0/mail" {
		return nil
	}
	body, err := teamlink.ReadBody(r)
	if err != nil {
		return err
	}
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return nil // the authority answers the bad body
	}
	var to, ref string
	_ = json.Unmarshal(m["to"], &to)
	_ = json.Unmarshal(m["ref"], &ref)
	workID := func(id string) bool {
		a, err := api.ParseAddress(id)
		return id != "" && err == nil && a.Kind == api.KindWork
	}
	if a, err := api.ParseAddress(to); err == nil && (a.Kind == api.KindBroadcast || a.Kind == api.KindOwners) {
		return nil
	}
	if !workID(to) && !workID(ref) {
		return nil
	}

	ctx := r.Context()
	named := r.URL.Query().Get("project")
	fromEnv := false
	if named == "" {
		named = api.GetHeader(r.Header, api.HeaderProject)
		fromEnv = api.GetHeader(r.Header, api.HeaderProjectFrom) == project.RuleEnv
	}
	var x *remote
	if named != "" {
		slug, _, _ := strings.Cut(named, "/")
		if _, local := s.d.Projects.Lookup(slug); local {
			return nil // this machine's project: the mail module follows the id
		}
		if x = s.ByName(slug); x == nil {
			return nil // no such project: the router answers
		}
	} else if x, _ = s.ByCwd(r); x == nil && s.d.Resolve(ctx).Project != nil {
		return nil // the same, by the directory
	}

	var target *remote
	for _, id := range []string{to, ref} {
		if !workID(id) || (x != nil && rep(x).hasItem(ctx, id)) {
			continue
		}
		y := (*remote)(nil)
		if named == "" || fromEnv {
			for _, c := range s.List() {
				if rep(c).hasItem(ctx, id) {
					y = c
					break
				}
			}
		}
		nid, moved := id, false
		if y == nil {
			if y, nid, moved, err = s.resolveWork(r, x, id); err != nil {
				return err
			}
			if y == nil || (!moved && named != "" && !fromEnv) {
				continue
			}
		}
		if moved {
			for _, k := range []string{"to", "ref"} {
				var v string
				if json.Unmarshal(m[k], &v) == nil && v == id {
					if m[k], err = json.Marshal(nid); err != nil {
						return err
					}
				}
			}
		}
		if target == nil {
			target = y
		}
	}
	if target == nil {
		return nil
	}
	delete(m, "area") // an area of the project the caller named: the item's own answers
	if body, err = json.Marshal(m); err != nil {
		return err
	}
	dropProject(r)
	api.SetHeader(r.Header, api.HeaderProject, target.Slug())
	r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
	return nil
}

// dropProject removes what named a project on r.
func dropProject(r *http.Request) {
	q := r.URL.Query()
	q.Del("project")
	r.URL.RawQuery = q.Encode()
	api.DelHeader(r.Header, api.HeaderProject)
	api.DelHeader(r.Header, api.HeaderProjectFrom)
}

// hasItem reports whether a copy that can answer holds item id.
func (rp *replica) hasItem(ctx context.Context, id string) bool {
	if !rp.isReady() {
		return false
	}
	_, _, ok := rp.findItem(ctx, id)
	return ok
}
