package team

import (
	"context"
	"errors"
	"github.com/sengirunners/potluck/kit/teamlink"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
)

// Only a 404 from the named project sends a call on one issue on: a
// refusal, a conflict or a failure there is the answer.
func TestRetryMovedOnlyOn404(t *testing.T) {
	for code, want := range map[int]bool{200: false, 400: false, 403: false, 404: true, 409: false, 500: false, 503: false} {
		if retryMoved(code) != want {
			t.Errorf("%d: retry %v, want %v", code, !want, want)
		}
	}
}

func fakeRemote(slug, authority string, state string) *remote {
	return &remote{R: api.Remote{Project: api.ProjectRef{Slug: slug}, Authority: authority, State: state}}
}

// A missed issue call is asked of the other live projects on the named
// project's authority only: a move never crosses authorities.
func TestIssueRetriesSameAuthority(t *testing.T) {
	x := fakeRemote("harbor", "box-a", api.RemoteOK)
	zm := fakeRemote("zm", "box-a", api.RemoteOK)
	other := fakeRemote("side", "box-b", api.RemoteOK)
	gone := fakeRemote("sengi", "box-a", "left")
	got := issueRetries([]*remote{x, zm, other, gone}, x)
	if len(got) != 1 || got[0] != zm {
		var names []string
		for _, y := range got {
			names = append(names, y.R.Project.Slug)
		}
		t.Fatalf("retries %v, want [zm]", names)
	}
}

// trackedBody records that it was closed.
type trackedBody struct {
	io.Reader
	closed *atomic.Int32
}

func (b trackedBody) Close() error { b.closed.Add(1); return nil }

// The old id is resolved by a GET to every other project at once: only a
// 2xx finds it; 401, 403, 5xx and send errors are the answer only when
// nothing found it; two answers naming different issues are a conflict;
// every body is closed; the wait is one CallTimeout.
func TestResolveMoved(t *testing.T) {
	a := fakeRemote("a", "box", api.RemoteOK)
	b := fakeRemote("b", "box", api.RemoteOK)
	c := fakeRemote("c", "box", api.RemoteOK)
	a.R.Project.ID, b.R.Project.ID, c.R.Project.ID = "PA", "PB", "PC"
	ys := []*remote{a, b, c}
	var closed, sent atomic.Int32
	reply := func(code int, body string) *http.Response {
		return &http.Response{StatusCode: code, Header: http.Header{}, Body: trackedBody{strings.NewReader(body), &closed}}
	}
	issue := func(id, project string) string { return `{"id":"` + id + `","project":"` + project + `"}` }
	boom := errors.New("connection reset")
	run := func(answers map[*remote]func() (*http.Response, error)) (*moved, error) {
		t.Helper()
		closed.Store(0)
		sent.Store(0)
		m, err := resolveMoved(context.Background(), ys, func(_ context.Context, y *remote) (*http.Response, error) {
			resp, err := answers[y]()
			if resp != nil {
				sent.Add(1)
			}
			return resp, err
		})
		if closed.Load() != sent.Load() {
			t.Fatalf("%d of %d bodies closed", closed.Load(), sent.Load())
		}
		return m, err
	}
	nf := func() (*http.Response, error) {
		return reply(404, `{"error":{"code":"not_found","message":"no issue"}}`), nil
	}
	// Every 404: nothing, no error.
	if m, err := run(map[*remote]func() (*http.Response, error){a: nf, b: nf, c: nf}); m != nil || err != nil {
		t.Fatalf("all 404: %+v %v", m, err)
	}
	// A stale token's 403, a 500 and a hit: the hit, whatever came first.
	m, err := run(map[*remote]func() (*http.Response, error){
		a: func() (*http.Response, error) {
			return reply(403, `{"error":{"code":"forbidden","message":"stale"}}`), nil
		},
		b: func() (*http.Response, error) { return reply(500, `oops`), nil },
		c: func() (*http.Response, error) { return reply(200, issue("zm-x", "PC")), nil },
	})
	if err != nil || m == nil || m.y != c || m.id != "zm-x" || string(m.body) != issue("zm-x", "PC") {
		t.Fatalf("a hit among refusals: %+v %v", m, err)
	}
	// No hit: a 401, 403, 5xx or send error is the answer, never a 404, and
	// a refusal keeps its own code (clients retry unavailable, not forbidden).
	for _, tc := range []struct {
		name string
		bad  func() (*http.Response, error)
		code string
	}{
		{"401", func() (*http.Response, error) {
			return reply(401, `{"error":{"code":"unauthenticated","message":"invalid or expired token"}}`), nil
		}, api.CodeUnauthenticated},
		{"403", func() (*http.Response, error) {
			return reply(403, `{"error":{"code":"forbidden","message":"guests are read-only"}}`), nil
		}, api.CodeForbidden},
		{"503 with no body", func() (*http.Response, error) { return reply(503, ``), nil }, api.CodeUnavailable},
		{"send error", func() (*http.Response, error) { return nil, boom }, ""},
	} {
		m, err := run(map[*remote]func() (*http.Response, error){a: nf, b: tc.bad, c: nf})
		if m != nil || err == nil || api.IsCode(err, api.CodeNotFound) {
			t.Fatalf("%s and 404s: %+v %v", tc.name, m, err)
		}
		if tc.code != "" && !api.IsCode(err, tc.code) {
			t.Fatalf("%s: %v, want code %s", tc.name, err, tc.code)
		}
		if tc.code == "" && !errors.Is(err, boom) {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
	// Two projects name one issue: the one that holds it.
	m, err = run(map[*remote]func() (*http.Response, error){
		a: func() (*http.Response, error) { return reply(200, issue("zm-x", "PC")), nil },
		b: nf,
		c: func() (*http.Response, error) { return reply(200, issue("zm-x", "PC")), nil },
	})
	if err != nil || m == nil || m.y != c {
		t.Fatalf("one issue twice: %+v %v", m, err)
	}
	// Different issues: a conflict, never a guess.
	if m, err := run(map[*remote]func() (*http.Response, error){
		a: func() (*http.Response, error) { return reply(200, issue("sengi-x", "PA")), nil },
		b: nf,
		c: func() (*http.Response, error) { return reply(200, issue("zm-x", "PC")), nil },
	}); m != nil || !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("two issues: %+v %v", m, err)
	}
	// One that hangs: the round ends at CallTimeout, with the others' answers.
	old := teamlink.CallTimeout
	teamlink.CallTimeout = 100 * time.Millisecond
	defer func() { teamlink.CallTimeout = old }()
	start := time.Now()
	m, err = resolveMoved(context.Background(), ys, func(ctx context.Context, y *remote) (*http.Response, error) {
		if y == a {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		if y == b {
			return reply(200, issue("zm-x", "PB")), nil
		}
		return reply(404, ``), nil
	})
	if m == nil || m.y != b || err != nil || time.Since(start) > time.Second {
		t.Fatalf("a hang and a hit: %+v %v after %s", m, err, time.Since(start))
	}
}
