package team

// A call on one native issue whose id moved to another project (zm issues
// move): the named project answers 404, so the old id is resolved by the
// other joined projects on the same authority, and the call goes out once to
// the one that found it (SPEC "Native issues": moves).

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/sengirunners/potluck/kit/teamlink"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// forwardIssue forwards a call on one native issue (bd-zm names the project
// its id's prefix routes to) to that project's authority. When that answers
// 404 (only then), the issue may have moved (zm issues move): the old id is
// resolved first, by a GET of it sent at once to the other joined projects on
// the same authority, whose authority follows the move's redirect (never a
// prefix swap) for this machine's token there. Exactly one project must find
// it; the call then goes out once, to that project (a GET is answered by the
// resolving GET itself). A write is never fanned out. With nothing found, an
// error from any project is the answer, else the 404. A hit pays nothing
// extra; a miss pays one parallel round of GETs.
func (s *Service) forwardIssue(w http.ResponseWriter, r *http.Request, x *remote, area string) {
	body, err := teamlink.ReadBody(r)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	resp, err := s.ForwardResp(r, x, area, true, body)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	defer resp.Body.Close()
	ys := issueRetries(s.List(), x)
	if !retryMoved(resp.StatusCode) || len(ys) == 0 {
		s.relay(w, r, resp)
		return
	}
	id, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v0/issues/"), "/")
	f, err := resolveMoved(r.Context(), ys, func(ctx context.Context, y *remote) (*http.Response, error) {
		g := r.Clone(ctx)
		g.Method, g.Body, g.ContentLength = http.MethodGet, http.NoBody, 0
		g.URL.Path, g.URL.RawPath = "/v0/issues/"+id, ""
		g.Header.Del("Content-Type")
		return s.ForwardResp(g, y, "", false, []byte{})
	})
	switch {
	case f == nil && err != nil:
		api.WriteError(w, err)
	case f == nil:
		s.relay(w, r, resp)
	case r.Method == http.MethodGet && rest == "":
		if ct := f.header.Get("Content-Type"); ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		w.WriteHeader(f.status)
		_, _ = w.Write(f.body)
	default:
		other, err := s.ForwardResp(r, f.y, "", false, body)
		if err != nil {
			api.WriteError(w, err)
			return
		}
		defer other.Body.Close()
		s.relay(w, r, other)
	}
}

// retryMoved reports whether the named project's answer sends a call on
// one issue on to the other joined projects: a 404 only (a refusal, a
// conflict or a failure there is the answer).
func retryMoved(status int) bool { return status == http.StatusNotFound }

// issueRetries are the joined projects a missed issue call is asked of:
// every other live one on x's authority (a move never crosses authorities).
func issueRetries(xs []*remote, x *remote) []*remote {
	auth := x.Snapshot().Authority
	var out []*remote
	for _, y := range xs {
		if snap := y.Snapshot(); y != x && snap.Authority == auth && teamlink.Ended(snap) == nil {
			out = append(out, y)
		}
	}
	return out
}

// moved is the project that resolved a moved issue's old id, with its
// answer to the GET (read whole: one issue).
type moved struct {
	y           *remote
	status      int
	header      http.Header
	body        []byte
	id, project string // the issue the answer names
}

// resolveMoved sends get to every remote in ys at once and waits for their
// answers, at most CallTimeout for the round (a remote that has not answered
// by then counts as failed). A 2xx names the issue; a 404 is nothing; any
// other status (401, 403, 5xx) or a send error is an error, the answer only
// when no project found the issue. Several 2xx answers naming one issue: the
// project that holds it; naming different issues: conflict, never a guess.
// Every body is read and closed here.
func resolveMoved(parent context.Context, ys []*remote, get func(context.Context, *remote) (*http.Response, error)) (*moved, error) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	type result struct {
		m   *moved
		err error
	}
	ch := make(chan result, len(ys))
	for _, y := range ys {
		go func() {
			resp, err := get(ctx, y)
			if err != nil {
				ch <- result{err: err}
				return
			}
			b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
			resp.Body.Close() // before the answer is sent: a returned round holds no open body
			switch {
			case err != nil:
				ch <- result{err: api.Errorf(api.CodeUnavailable, "reading %s's answer: %v", y.Slug(), err)}
			case resp.StatusCode == http.StatusNotFound:
				ch <- result{}
			case resp.StatusCode/100 != 2:
				var er api.ErrorResponse // the wire shape: {"error": {...}}
				if json.Unmarshal(b, &er) != nil || er.Error == nil || er.Error.Code == "" {
					er.Error = &api.Error{Code: api.CodeUnavailable, Message: fmt.Sprintf("%s answered %d", y.Slug(), resp.StatusCode)}
				}
				ch <- result{err: er.Error}
			default:
				m := &moved{y: y, status: resp.StatusCode, header: resp.Header, body: b}
				var it struct{ ID, Project string }
				_ = json.Unmarshal(b, &it)
				m.id, m.project = it.ID, it.Project
				ch <- result{m: m}
			}
		}()
	}
	timer := time.NewTimer(teamlink.CallTimeout)
	defer timer.Stop()
	var found []*moved
	var firstErr error
	for range len(ys) {
		select {
		case res := <-ch:
			if res.m != nil {
				found = append(found, res.m)
			} else if res.err != nil && firstErr == nil {
				firstErr = res.err
			}
		case <-timer.C:
			cancel() // the rest close their own bodies as they end
			if firstErr == nil {
				firstErr = api.Errorf(api.CodeUnavailable, "the other projects did not answer within %s", teamlink.CallTimeout)
			}
			if len(found) == 0 {
				return nil, firstErr
			}
			return pickMoved(found)
		}
	}
	if len(found) == 0 {
		return nil, firstErr
	}
	return pickMoved(found)
}

// pickMoved is the one project that holds the issue the answers name (an
// issue's project and a remote's Project.ID are both the authority's ULID).
func pickMoved(found []*moved) (*moved, error) {
	var ids []string
	for _, m := range found {
		if !slices.Contains(ids, m.id) {
			ids = append(ids, m.id)
		}
	}
	if len(ids) > 1 {
		sort.Strings(ids)
		return nil, api.Errorf(api.CodeConflict, "this id resolves to several issues (%s)", strings.Join(ids, ", ")).
			WithHint("name the issue by its new id")
	}
	for _, m := range found {
		if m.project != "" && m.project == m.y.Snapshot().Project.ID {
			return m, nil
		}
	}
	return found[0], nil
}
