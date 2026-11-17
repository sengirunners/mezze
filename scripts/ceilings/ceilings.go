// Package ceilings is the speed gate's one file: ceilings.json holds the p95
// ceiling of every `make bench` row and every gated load test operation.
// scripts/bench and scripts/loadtest embed it, and exit 1 when a measured p95
// goes over; in CI, bench.yml goes red only on the same row twice in a row on
// main (bench repeat). The file only tightens: a ceiling goes up, or a row goes
// away, only with a one-line reason in the same PR (Loosened; ci.yml's check
// job runs it). An agent may make that edit itself; no sign-off.
//
// Runner classes: ceilings.json is Sengi's, where its numbers came
// from. Another verdict class (the dedicated zm-bench VM) has its own file,
// ceilings.<class>.json, proposed from its own runs (`bench calibrate`) and
// tighten-only from its first commit. While a class has no file, its runs
// are calibration: listed, never judged.
package ceilings

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
)

// Path is the file, from the repo root.
const Path = "scripts/ceilings/ceilings.json"

// BarMS is the product bar (SPEC "Native issues", AGENTS.md): native-issue
// reads and writes, and reads and writes served from a replica's copy, under
// 16 ms p95 on 11.5k issues. No ceiling on a bar row may pass it, whatever
// its reason: the file cannot lower the bar, only this constant can.
const BarMS = 16

// OnBar reports whether a bench row is held to BarMS. team-* rows wait for
// the authority by design (SPEC 17.6) and are not.
func OnBar(row string) bool {
	return strings.HasPrefix(row, "issue-") || strings.HasPrefix(row, "replica-")
}

// Row is one ceiling.
type Row struct {
	P95 float64 `json:"p95_ms"`
	// Why is the one-line reason of the last raise.
	Why string `json:"why,omitempty"`
}

// File is ceilings.json.
type File struct {
	Doc      string         `json:"_doc"`
	Bench    map[string]Row `json:"bench"`
	Loadtest map[string]Row `json:"loadtest"`
	// Retired names rows taken out on purpose ("bench/<row>" or
	// "loadtest/<op>") with the reason.
	Retired map[string]string `json:"retired,omitempty"`
}

//go:embed ceilings.json
var shipped []byte

// classFiles holds every ceilings*.json shipped: Sengi's and each calibrated
// class's.
//
//go:embed ceilings*.json
var classFiles embed.FS

// Sengi is the class of sengi-* runners: ceilings.json's.
const Sengi = "sengi"

// Dedicated is the class of a dedicated bench runner (runs-on label zm-bench,
// set by the repo variable ZM_BENCH_RUNNER).
const Dedicated = "zm-bench"

// BoxPrefix starts the class of a zm pool box size: runs-on label
// "<pool label>:<size>" (ZM_BENCH_RUNNER, served by zm's runner broker) is
// class "box-<size>". Each size is its own class: its binding's machine is not
// another size's.
const BoxPrefix = "box-"

var boxSize = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ClassOf names the verdict class of a runs-on label ("<label> <runner>" is
// fine: only the label counts; so is GitHub's comma-separated label list), or
// "" when its timing judges nothing (the Factory pool, a Mac). A class's own
// name is its class. scripts/ci/bench-class.sh is the same rule in shell.
func ClassOf(label string) string {
	label, _, _ = strings.Cut(strings.TrimSpace(label), " ")
	for _, l := range strings.Split(label, ",") {
		if pool, size, isBox := strings.Cut(l, ":"); isBox { // a pool box, whatever its pool label looks like
			if pool != "" && boxSize.MatchString(size) {
				return BoxPrefix + size
			}
			continue
		}
		switch {
		case strings.HasPrefix(l, "sengi-"):
			return Sengi
		case l == Dedicated || strings.HasPrefix(l, Dedicated+"-"):
			return Dedicated
		case strings.HasPrefix(l, BoxPrefix) && boxSize.MatchString(strings.TrimPrefix(l, BoxPrefix)):
			return l
		}
	}
	return ""
}

var className = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ClassPath is a class's file, from the repo root.
func ClassPath(class string) string {
	if class == Sengi {
		return Path
	}
	return "scripts/ceilings/ceilings." + class + ".json"
}

// Load reads the embedded file.
func Load() (File, error) { return Parse(shipped) }

// LoadClass reads a class's embedded file. ok is false while the class has
// none: it is calibrating, and its runs give no verdict.
func LoadClass(class string) (f File, ok bool, err error) {
	if class == Sengi {
		f, err = Load()
		return f, err == nil, err
	}
	if !className.MatchString(class) {
		return f, false, fmt.Errorf("ceilings: bad class %q", class)
	}
	b, err := classFiles.ReadFile("ceilings." + class + ".json")
	if errors.Is(err, fs.ErrNotExist) {
		return f, false, nil
	}
	if err != nil {
		return f, false, err
	}
	f, err = parse(ClassPath(class), b)
	return f, err == nil, err
}

// Classes lists the classes with a shipped file other than Sengi's.
func Classes() []string {
	var out []string
	names, _ := fs.Glob(classFiles, "ceilings.*.json")
	for _, n := range names {
		out = append(out, strings.TrimSuffix(strings.TrimPrefix(n, "ceilings."), ".json"))
	}
	sort.Strings(out)
	return out
}

// Parse reads and checks a ceilings file.
func Parse(b []byte) (File, error) { return parse(Path, b) }

func parse(path string, b []byte) (File, error) {
	var f File
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return f, fmt.Errorf("%s: %w", path, err)
	}
	for _, s := range f.sections() {
		for name, r := range s.rows {
			if r.P95 <= 0 {
				return f, fmt.Errorf("%s: %s/%s: p95_ms must be above zero", path, s.name, name)
			}
		}
	}
	for name, r := range f.Bench {
		if OnBar(name) && r.P95 > BarMS {
			return f, fmt.Errorf("%s: bench/%s: %g ms passes the %d ms bar: fix the path, the bar does not move", path, name, r.P95, BarMS)
		}
	}
	return f, nil
}

type section struct {
	name string
	rows map[string]Row
}

func (f File) sections() []section {
	return []section{{"bench", f.Bench}, {"loadtest", f.Loadtest}}
}

// Loosened lists what f gives away compared with base, the file on main,
// without a reason: a raised ceiling whose why is empty or unchanged, a row
// gone without an entry in retired. Each line says the fix.
func (f File) Loosened(base File) []string { return f.LoosenedAt(base, Path) }

// LoosenedAt is Loosened for the file at path (a class's).
func (f File) LoosenedAt(base File, path string) []string {
	var out []string
	for i, s := range base.sections() {
		now := f.sections()[i].rows
		for name, was := range s.rows {
			key := s.name + "/" + name
			r, ok := now[name]
			switch {
			case !ok && strings.TrimSpace(f.Retired[key]) == "":
				out = append(out, fmt.Sprintf("%s is gone: put the row back, or add %q with a one-line reason to retired in %s", key, key, path))
			case ok && r.P95 > was.P95 && (strings.TrimSpace(r.Why) == "" || r.Why == was.Why):
				out = append(out, fmt.Sprintf("%s went from %g to %g ms with no new reason: lower p95_ms again, or set this row's why in %s to one line saying why the cost is worth it", key, was.P95, r.P95, path))
			}
		}
	}
	sort.Strings(out)
	return out
}
