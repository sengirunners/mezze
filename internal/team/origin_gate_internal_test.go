package team

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// stubSessions answers CallerSession with origin and err.
type stubSessions struct {
	orchcore.SessionService
	origin string
	err    error
}

func (s stubSessions) CallerSession(context.Context, *authz.Subject) (string, error) {
	return s.origin, s.err
}

func (s stubSessions) CallerSessionNow(context.Context, *authz.Subject) (string, bool, error) {
	return s.origin, s.err == nil, s.err
}

// The gate fails closed: no session module, or a peer it cannot read,
// refuses the call before it is served; a network call is not its to check.
func TestGateOriginFailsClosed(t *testing.T) {
	d := &core.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := &Service{d: d}
	run := func(local bool, method, target, body string) (code int, served *http.Request) {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		if local {
			r = r.WithContext(core.WithLocal(r.Context()))
		}
		w := httptest.NewRecorder()
		serve := func(_ http.ResponseWriter, r *http.Request) { served = r }
		if !s.gateOrigin(w, r, serve) {
			serve(w, r)
		}
		return w.Code, served
	}
	for _, c := range []struct {
		name     string
		sessions orchcore.SessionService
	}{{"no session module", nil}, {"unreadable peer", stubSessions{err: errors.New("calling process no longer exists")}}} {
		orchcore.Wire(d).Sessions = c.sessions
		for _, m := range [][2]string{{"POST", "/v0/asks/A1/answer"}, {"POST", "/v0/login/github/start"}, {"DELETE", "/v0/approvals/key"}} {
			body := ""
			if m[1] == "/v0/login/github/start" {
				body = `{"purpose":"key"}`
			}
			if code, served := run(true, m[0], m[1], body); code != http.StatusForbidden || served != nil {
				t.Errorf("%s: %s %s answered %d, served %v", c.name, m[0], m[1], code, served != nil)
			}
		}
		// choose is only logged, whatever the lookup says.
		if _, served := run(true, "POST", "/v0/asks/A1/choose", ""); served == nil {
			t.Errorf("%s: choose was not served", c.name)
		}
		// A sign-in that binds no key is served unchecked, the body intact.
		if _, served := run(true, "POST", "/v0/login/github/start", `{"purpose":"link"}`); served == nil {
			t.Errorf("%s: a link sign-in was not served", c.name)
		} else if b, _ := io.ReadAll(served.Body); string(b) != `{"purpose":"link"}` {
			t.Errorf("%s: the sign-in body reached the router as %q", c.name, b)
		}
		// Over the network the mark is left for the authority to read.
		if _, served := run(false, "POST", "/v0/asks/A1/answer?"+orchapi.OriginChecked+"=1", ""); served == nil || served.URL.Query().Get(orchapi.OriginChecked) != "1" {
			t.Errorf("%s: a network call was touched", c.name)
		}
	}
	// Outside a session the gate marks the call and drops the caller's own mark.
	orchcore.Wire(d).Sessions = stubSessions{}
	if _, served := run(true, "POST", "/v0/asks/A1/answer?project=p&"+orchapi.OriginChecked+"=0", ""); served == nil ||
		served.URL.Query()[orchapi.OriginChecked][0] != "1" || len(served.URL.Query()[orchapi.OriginChecked]) != 1 || served.URL.Query().Get("project") != "p" {
		t.Fatalf("an answer from outside any session: %+v", served)
	}
}

// Minting a limited token (a limit or a device in the body, read as the
// mint handler reads it) is refused from inside a session and when nobody
// can tell, and a body the handler could not read is refused too; an
// owner-token mint is served untouched, and every served body reaches the
// handler byte for byte.
func TestGateOriginLimitedMint(t *testing.T) {
	d := &core.Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s := &Service{d: d}
	run := func(body string) (code int, served string, ok bool) {
		r := httptest.NewRequest("POST", "/v0/tokens", strings.NewReader(body))
		r = r.WithContext(core.WithLocal(r.Context()))
		w := httptest.NewRecorder()
		serve := func(_ http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			served, ok = string(b), true
		}
		if !s.gateOrigin(w, r, serve) {
			serve(w, r)
		}
		return w.Code, served, ok
	}
	limited := []string{
		`{"limit":"answer","project":"p","node":"phone","ttl_seconds":60}`,
		`{"LIMIT":"answer","project":"p","node":"phone","ttl_seconds":60}`, // the decoder folds case
		`{"node":"phone"}`,                    // a device alone still sends the handler to the limited mint
		`{"limit":"","limit":"answer"}`,       // the last duplicate wins
		`{"limit":"answer"} {"limit":""}`,     // only the first value is read
		`{"Node":"phone","as":"","role":""}`,  // case again
		`{"limit":"answer","project":"p"}   `, // trailing space
	}
	plain := []string{``, `{}`, `{"as":"bot","role":"member","project":"p"}`, `{"as":"bot","role":"member"} {"limit":"answer"}`}
	unreadable := []string{`{"limit":"answer",`, `{"limit":1}`, `{"limit":"answer","extra":true}`, `not json`}

	for _, c := range []struct {
		name     string
		sessions orchcore.SessionService
	}{{"inside a session", stubSessions{origin: "sess-1"}}, {"no session module", nil}, {"unreadable peer", stubSessions{err: errors.New("gone")}}} {
		orchcore.Wire(d).Sessions = c.sessions
		for _, b := range limited {
			if code, _, ok := run(b); ok || code != http.StatusForbidden {
				t.Errorf("%s: limited mint %s answered %d, served %v", c.name, b, code, ok)
			}
		}
		for _, b := range plain {
			if _, got, ok := run(b); !ok || got != b {
				t.Errorf("%s: owner-token mint %q served %v as %q", c.name, b, ok, got)
			}
		}
		for _, b := range unreadable {
			if code, _, ok := run(b); ok || code != http.StatusBadRequest {
				t.Errorf("%s: unreadable mint %s answered %d, served %v", c.name, b, code, ok)
			}
		}
	}
	// Outside any session a limited mint goes on, its body intact.
	orchcore.Wire(d).Sessions = stubSessions{}
	for _, b := range limited {
		if _, got, ok := run(b); !ok || got != b {
			t.Errorf("outside a session: %s served %v as %q", b, ok, got)
		}
	}
}
