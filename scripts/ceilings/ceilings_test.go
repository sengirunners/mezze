package ceilings

import (
	"strings"
	"testing"
)

func TestShippedFileParses(t *testing.T) {
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}

func TestBarCannotBeRaised(t *testing.T) {
	_, err := Parse([]byte(`{"bench": {"issue-show": {"p95_ms": 17, "why": "because"}}}`))
	if err == nil || !strings.Contains(err.Error(), "bar") {
		t.Fatalf("err = %v", err)
	}
	if _, err := Parse([]byte(`{"bench": {"team-claim": {"p95_ms": 50}}}`)); err != nil {
		t.Fatalf("a team row is not on the bar: %v", err)
	}
}

func TestParseRefusesNonsense(t *testing.T) {
	for _, in := range []string{`{"bench": {"x": {"p95_ms": 0}}}`, `{"benches": {}}`, `{`} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Errorf("%s parsed", in)
		}
	}
}

func TestLoosened(t *testing.T) {
	base := File{Bench: map[string]Row{"a": {P95: 10, Why: "old"}, "b": {P95: 10}}, Loadtest: map[string]Row{"claim": {P95: 100}}}
	cases := []struct {
		name string
		f    File
		want string // substring of the one finding; "" = none
	}{
		{"same", base, ""},
		{"tightened", File{Bench: map[string]Row{"a": {P95: 5}, "b": {P95: 9}}, Loadtest: map[string]Row{"claim": {P95: 90}}}, ""},
		{"new row", File{Bench: map[string]Row{"a": {P95: 10}, "b": {P95: 10}, "c": {P95: 900}}, Loadtest: base.Loadtest}, ""},
		{"raised with a reason", File{Bench: map[string]Row{"a": {P95: 11, Why: "search now ranks"}, "b": {P95: 10}}, Loadtest: base.Loadtest}, ""},
		{"raised, no reason", File{Bench: map[string]Row{"a": {P95: 10}, "b": {P95: 11}}, Loadtest: base.Loadtest}, "bench/b went from 10 to 11"},
		{"raised, the old reason", File{Bench: map[string]Row{"a": {P95: 11, Why: "old"}, "b": {P95: 10}}, Loadtest: base.Loadtest}, "bench/a went from 10 to 11"},
		{"raised, blank reason", File{Bench: map[string]Row{"a": {P95: 11, Why: "  "}, "b": {P95: 10}}, Loadtest: base.Loadtest}, "bench/a went from 10 to 11"},
		{"load test raised", File{Bench: base.Bench, Loadtest: map[string]Row{"claim": {P95: 101}}}, "loadtest/claim went"},
		{"row gone", File{Bench: map[string]Row{"a": {P95: 10}}, Loadtest: base.Loadtest}, "bench/b is gone"},
		{"row retired", File{Bench: map[string]Row{"a": {P95: 10}}, Loadtest: base.Loadtest, Retired: map[string]string{"bench/b": "the path is gone"}}, ""},
	}
	for _, c := range cases {
		got := c.f.Loosened(base)
		switch {
		case c.want == "" && len(got) != 0:
			t.Errorf("%s: %v", c.name, got)
		case c.want != "" && (len(got) != 1 || !strings.Contains(got[0], c.want)):
			t.Errorf("%s: %v, want %q", c.name, got, c.want)
		}
	}
}

// The label decides the class; Factory and a Mac judge nothing.
func TestClassOf(t *testing.T) {
	for label, want := range map[string]string{
		"sengi-standard-4-ubuntu-2404":          Sengi,
		"sengi-standard-4-ubuntu-2404 sengi-07": Sengi,
		"zm-bench":                              Dedicated,
		"zm-bench workshop-bench":               Dedicated,
		"zm-bench-2":                            Dedicated,
		"zm-benchmark":                          "",
		"sengi:c2m8":                            "box-c2m8",
		"sengi:c2m8 zm-123":                     "box-c2m8",
		"self-hosted,sengi:c2m8 zm-123":         "box-c2m8",
		"box-c2m8":                              "box-c2m8",
		":c2m8":                                 "",
		"sengi:C2M8":                            "",
		"sengi:c2m8:x":                          "",
		"box-":                                  "",
		"sengi-x:c2m8":                          "box-c2m8",
		"sengi-x:BAD":                           "",
		" sengi-standard-4":                     Sengi,
		"acme-runner-4 tfp-abc":                 "",
		"":                                      "",
	} {
		if got := ClassOf(label); got != want {
			t.Errorf("ClassOf(%q) = %q, want %q", label, got, want)
		}
	}
}

// Sengi's class is ceilings.json; a class with no file is calibrating, not an error.
func TestLoadClass(t *testing.T) {
	f, ok, err := LoadClass(Sengi)
	if err != nil || !ok || len(f.Bench) == 0 {
		t.Fatalf("sengi: ok %v, %d rows, err %v", ok, len(f.Bench), err)
	}
	if _, ok, err := LoadClass("no-such-class"); ok || err != nil {
		t.Fatalf("missing class: ok %v, err %v", ok, err)
	}
	if _, _, err := LoadClass("../ceilings"); err == nil {
		t.Fatal("a path as a class loaded")
	}
	if ClassPath(Dedicated) != "scripts/ceilings/ceilings.zm-bench.json" || ClassPath(Sengi) != Path {
		t.Fatal(ClassPath(Dedicated))
	}
}

// Every shipped class file parses, so the bar holds on it too.
func TestShippedClassFilesParse(t *testing.T) {
	for _, c := range Classes() {
		if c == Sengi {
			t.Fatalf("class file named after Sengi: %s is ceilings.json", ClassPath(c))
		}
		if _, ok, err := LoadClass(c); !ok || err != nil {
			t.Errorf("%s: ok %v, err %v", ClassPath(c), ok, err)
		}
	}
}
