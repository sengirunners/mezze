package fulltest

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// stabilityLabels are the labels a route row in API.md may carry (API.md "Stability").
var stabilityLabels = map[string]bool{"stable": true, "beta": true, "app": true, "internal": true}

// routeRow matches a route table row: "| `METHOD /path` | label | ...".
var routeRow = regexp.MustCompile("^\\| `((?:GET|POST|PUT|PATCH|DELETE|HEAD) [^`]+)` \\| ([a-z]+) \\|")

// apiRouteLabels reads every route row of API.md: pattern -> stability label.
func apiRouteLabels(t *testing.T, doc string) map[string]string {
	t.Helper()
	labels := map[string]string{}
	for _, line := range strings.Split(doc, "\n") {
		m := routeRow.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if prev, ok := labels[m[1]]; ok && prev != m[2] {
			t.Errorf("API.md labels `%s` both %s and %s", m[1], prev, m[2])
		}
		labels[m[1]] = m[2]
	}
	return labels
}

// TestAPIDocListsEveryRoute keeps API.md honest: every registered route
// pattern has a table row ("| `METHOD /path` | label |") with a stability
// label, and no row names a route that is not registered.
func TestAPIDocListsEveryRoute(t *testing.T) {
	f := newLeakFixture(t)
	doc, err := os.ReadFile("../../../API.md")
	if err != nil {
		t.Fatal(err)
	}
	labels := apiRouteLabels(t, string(doc))
	registered := map[string]bool{}
	for _, rt := range f.dm.Router.Routes() {
		registered[rt.Pattern] = true
		if os.Getenv("ZM_DUMP_ROUTES") != "" {
			t.Logf("| `%s` | %s | %s | %s |", rt.Pattern, rt.Module, rt.Access, rt.Check)
		}
		label, ok := labels[rt.Pattern]
		switch {
		case !ok:
			t.Errorf("API.md has no row for `%s` (%s, %s): add \"| `%s` | <stable|beta|app|internal> | ... |\"", rt.Pattern, rt.Access, rt.Check, rt.Pattern)
		case !stabilityLabels[label]:
			t.Errorf("API.md labels `%s` %q: want stable, beta, app or internal", rt.Pattern, label)
		}
	}
	for pattern := range labels {
		if !registered[pattern] {
			t.Errorf("API.md has a row for `%s`, which is not a registered route", pattern)
		}
	}
	checkClientsDoc(t, labels)
}

// clientsStableRow matches a route row in docs/clients.md's stable table.
var clientsStableRow = regexp.MustCompile("^\\| `((?:GET|POST|PUT|PATCH|DELETE|HEAD) [^`]+)` \\|")

// checkClientsDoc keeps docs/clients.md's "Stable routes" table equal to the
// routes API.md labels stable, so the contract has one list.
func checkClientsDoc(t *testing.T, labels map[string]string) {
	t.Helper()
	b, err := os.ReadFile("../../../docs/clients.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, ok := strings.Cut(string(b), "\n## Stable routes\n")
	if !ok {
		t.Fatal(`docs/clients.md has no "## Stable routes" section`)
	}
	if i := strings.Index(section, "\n## "); i >= 0 {
		section = section[:i]
	}
	inDoc := map[string]bool{}
	for _, line := range strings.Split(section, "\n") {
		if m := clientsStableRow.FindStringSubmatch(line); m != nil {
			inDoc[m[1]] = true
		}
	}
	var missing, extra []string
	for pattern, label := range labels {
		if label == "stable" && !inDoc[pattern] {
			missing = append(missing, pattern)
		}
	}
	for pattern := range inDoc {
		if labels[pattern] != "stable" {
			extra = append(extra, pattern)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	for _, p := range missing {
		t.Errorf("API.md labels `%s` stable but docs/clients.md \"Stable routes\" does not list it", p)
	}
	for _, p := range extra {
		t.Errorf("docs/clients.md \"Stable routes\" lists `%s`, which API.md does not label stable", p)
	}
}
