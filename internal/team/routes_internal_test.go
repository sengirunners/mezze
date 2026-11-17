package team

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// Only reads are hedged: a write, a long poll or a stream goes out once.
func TestHedgeable(t *testing.T) {
	rows := routesFor("mini")
	for _, c := range []struct {
		method, target string
		accept         string
		want           bool
	}{
		{"GET", "/v0/issues?project=harbor&type=epic", "", true},
		{"GET", "/v0/sessions", "", true},
		{"GET", "/v0/summaries?project=harbor&q=nightly&limit=10", "", true},
		{"GET", "/v0/summaries/config?project=harbor", "", true},
		{"HEAD", "/v0/summaries?project=harbor", "", true},
		{"HEAD", "/v0/summaries/config?project=harbor", "", true},
		{"POST", "/v0/summaries?project=harbor", "", false},
		{"PUT", "/v0/summaries/config?project=harbor", "", false},
		{"GET", "/v0/summaries?project=harbor&wait=30", "", false},
		{"GET", "/v0/summaries/config?project=harbor&wait_ms=1000", "", false},
		{"GET", "/v0/summaries?project=harbor", "text/event-stream", false},
		{"HEAD", "/v0/work/x", "", true},
		{"POST", "/v0/issues/sql?project=harbor", "", true},
		{"POST", "/v0/issues", "", false},
		{"POST", "/v0/work/x/claim", "", false},
		{"PATCH", "/v0/issues/harbor-1", "", false},
		{"DELETE", "/v0/leases/k", "", false},
		{"GET", "/v0/sync?wait_ms=20000", "", false},
		{"GET", "/v0/mail/inbox?wait=30", "", false},
		{"GET", "/v0/mail/inbox", "", false},
		{"GET", "/v0/mail/inbox/peek", "", true},
		{"GET", "/v0/mail/m1/thread", "", true},
		{"GET", "/v0/issues/harbor-1/history", "", true},
		{"GET", "/v0/sessions/bea-mac%2F1234abcd/prompts", "", false},
		{"GET", "/v0/projects", "", false},
		{"GET", "/v0/events", "text/event-stream", false},
		{"GET", "/v0/people", "", true},
		{"GET", "/v0/run_targets/box", "", false},
	} {
		r := httptest.NewRequest(c.method, "http://potluck-mini:7433"+c.target, strings.NewReader(""))
		if c.accept != "" {
			r.Header.Set("Accept", c.accept)
		}
		if got := rows.Hedgeable(r); got != c.want {
			t.Errorf("%s %s: hedgeable %v, want %v", c.method, c.target, got, c.want)
		}
	}
}

// GET /v0/mail/inbox marks what it answers seen at the authority: a second
// attempt would answer without the mail the first one marked. Never hedged.
func TestInboxNeverHedged(t *testing.T) {
	rows := routesFor("mini")
	for _, target := range []string{"/v0/mail/inbox", "/v0/mail/inbox?all=1", "/v0/mail/inbox?project=01ABC&limit=50"} {
		for _, m := range []string{"GET", "HEAD"} {
			if rows.Hedgeable(httptest.NewRequest(m, "http://potluck-mini:7433"+target, nil)) {
				t.Errorf("%s %s is hedged", m, target)
			}
		}
	}
}

// Only writes carry a write id (and are recorded at the authority).
func TestForwardedWrites(t *testing.T) {
	rows := routesFor("mini")
	for _, c := range []struct {
		method, target string
		want           bool
	}{
		{"POST", "/v0/work", true},
		{"POST", "/v0/mail/imports", true},
		{"POST", "/v0/sessions/other%2F1234abcd/turn", true},
		{"POST", "/v0/login/github/start", true},
		{"POST", "/v0/issues/sql", false},
		{"GET", "/v0/work", false},
		{"POST", "/v0/run_targets/box/acquire", false},
		{"POST", "/v0/asks/a1/decide", false},
	} {
		if got := rows.IsForwardedWrite(httptest.NewRequest(c.method, "http://zm"+c.target, nil)); got != c.want {
			t.Errorf("%s %s: write %v, want %v", c.method, c.target, got, c.want)
		}
	}
}
