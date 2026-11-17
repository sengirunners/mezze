package team_test

import (
	"regexp"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// Every route row a Full daemon registers is whole (a forwarded row has a
// hedge decision: Hedged, Once with why, or Write; no pattern twice; the
// daemon would not start otherwise) and names a route the daemon has. A new
// forwarded route needs a row, and a row a decision on whether sending it
// twice is safe.
func TestRouteRowsAreWhole(t *testing.T) {
	tn := tailnettest.New()
	a := authority(t, tn)
	// d.Team is teamlink behind the safe-outputs gate (wiring.Full).
	ts, ok := a.dm.Deps.Team.(interface {
		CheckRoutes() error
		Rows() []teamlink.RouteRule
		Proxies(method, path string) bool
	})
	if !ok {
		t.Fatalf("d.Team is %T", a.dm.Deps.Team)
	}
	if err := ts.CheckRoutes(); err != nil {
		t.Fatal(err)
	}
	routes := map[string]bool{}
	for _, rt := range a.dm.Router.Routes() {
		routes[rt.Pattern] = true
	}
	rest := regexp.MustCompile(`\{[^}]*\.\.\.\}$`)
	forwarded := 0
	for _, row := range ts.Rows() {
		if row.Kind != teamlink.KindLocal {
			forwarded++
		}
		if !routes[row.Pattern] && !rest.MatchString(row.Pattern) {
			t.Errorf("%s has a row but is no route of the daemon", row.Pattern)
		}
	}
	if forwarded < 40 {
		t.Fatalf("only %d forwarded rows: the table is broken", forwarded)
	}
	// Proxies (the swap manifest) agrees with the rows.
	for _, p := range []string{"GET /v0/work", "POST /v0/issues", "GET /v0/projects/x", "GET /v0/people"} {
		m, path, _ := strings.Cut(p, " ")
		if !ts.Proxies(m, path) {
			t.Errorf("%s is not in Proxies", p)
		}
	}
	if ts.Proxies("POST", "/v0/asks/x/decide") || ts.Proxies("GET", "/v0/mail/notice") {
		t.Error("a local route is in Proxies")
	}
}
