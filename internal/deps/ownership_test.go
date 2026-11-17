package deps

// Every brick directory has exactly one owner row in OWNERSHIP.md: each
// internal/* and cmd/* directory.
//
// OWNERSHIP.md's contract with this test: one markdown table whose first
// column names the owner and whose second column lists backticked paths. A
// row owns directory D when one of its paths is `D`, `D/` or `D/**` (braces
// expand: `internal/{a,b}`). A row listing only files inside D
// (`internal/api/mail.go`) does not own D; those file entries are finer-grained
// exceptions to D's owner.

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/deps/publictree"
)

type ownerRow struct {
	owner string
	paths []string
}

var tick = regexp.MustCompile("`([^`]+)`")

// ownershipRows parses OWNERSHIP.md's owner table: the one headed
// "| builder | owns |" (or "| owner | owns |"), up to the first line that is
// not a table row. The owner is the bold name in the first cell.
func ownershipRows(t *testing.T) []ownerRow {
	t.Helper()
	b, err := os.ReadFile("../../OWNERSHIP.md")
	if os.IsNotExist(err) { // a public copy: public_exclude.txt leaves OWNERSHIP.md out
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var rows []ownerRow
	in := false
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if !in {
			in = strings.HasPrefix(line, "|") && len(cells) >= 2 && (cells[0] == "builder" || cells[0] == "owner") && cells[1] == "owns"
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break
		}
		if len(cells) < 2 || strings.HasPrefix(cells[0], "---") {
			continue
		}
		name := cells[0]
		if m := bold.FindStringSubmatch(name); m != nil {
			name = m[1]
		}
		r := ownerRow{owner: name}
		for _, m := range tick.FindAllStringSubmatch(strings.Join(cells[1:], " | "), -1) {
			r.paths = append(r.paths, expand(m[1])...)
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		t.Fatal("OWNERSHIP.md has no owner table (a table headed | builder | owns |)")
	}
	return rows
}

var bold = regexp.MustCompile(`\*\*([^*]+)\*\*`)

// expand expands one level of braces: internal/{a,b}/** -> internal/a/**, internal/b/**.
func expand(p string) []string {
	i, j := strings.Index(p, "{"), strings.Index(p, "}")
	if i < 0 || j < i {
		return []string{p}
	}
	var out []string
	for _, alt := range strings.Split(p[i+1:j], ",") {
		out = append(out, expand(p[:i]+strings.TrimSpace(alt)+p[j+1:])...)
	}
	return out
}

// ownsDir: the path names directory d itself.
func ownsDir(p, d string) bool {
	p = strings.TrimSuffix(strings.TrimSuffix(p, "/**"), "/")
	return p == d
}

// brickDirs are the directories that need an owner.
func brickDirs(t *testing.T) []string {
	t.Helper()
	var dirs []string
	for _, parent := range []string{"internal", "cmd"} {
		ents, err := os.ReadDir(filepath.Join("../..", parent))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if e.IsDir() {
				dirs = append(dirs, parent+"/"+e.Name())
			}
		}
	}
	return dirs
}

// dirOwners maps each brick directory to the rows that own it.
func dirOwners(t *testing.T) map[string][]string {
	t.Helper()
	rows := ownershipRows(t)
	out := map[string][]string{}
	for _, d := range brickDirs(t) {
		out[d] = nil
		for _, r := range rows {
			for _, p := range r.paths {
				if ownsDir(p, d) {
					out[d] = append(out[d], r.owner)
					break
				}
			}
		}
	}
	return out
}

// ownersByDir maps each brick directory to its owner ("" when not exactly one).
func ownersByDir(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for d, who := range dirOwners(t) {
		if len(who) == 1 {
			out[d] = who[0]
		}
	}
	return out
}

// privateTree is true in the private repo: it still holds a file public_exclude.txt names. A public copy
// leaves every one of them out, so tests of what only the private repo holds skip there. Deleting one file
// (OWNERSHIP.md included) does not turn this off: the others still exist, and the tests that guard the
// exclude list then fail loudly.
func privateTree(t *testing.T) bool {
	t.Helper()
	b, err := os.ReadFile("public_exclude.txt")
	if err != nil {
		t.Fatal(err)
	}
	ex, err := publictree.ParseExclude(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ex {
		if strings.HasPrefix(l.Why, "guard:") || strings.ContainsAny(l.Glob, "*?[") {
			continue
		}
		if _, err := os.Stat(filepath.Join("../..", l.Glob)); err == nil {
			return true
		}
	}
	return false
}

func TestOwnership(t *testing.T) {
	if !privateTree(t) {
		t.Skip("OWNERSHIP.md is not in a public copy; the private repo runs this")
	}
	rows := ownershipRows(t)
	var fails []string
	for d, who := range dirOwners(t) {
		switch len(who) {
		case 1:
		case 0:
			fails = append(fails, fmt.Sprintf("%s has no owner row in OWNERSHIP.md: add `%s/**` to its owner's row (or a new row; \"unowned / zm lead\" when nobody owns it yet).", d, d))
		default:
			fails = append(fails, fmt.Sprintf("%s is owned by %d rows in OWNERSHIP.md (%s): keep the directory in one row; other rows may list single files.", d, len(who), strings.Join(who, "; ")))
		}
	}
	// A row naming a directory that does not exist is stale.
	known := map[string]bool{}
	for _, d := range brickDirs(t) {
		known[d] = true
	}
	for _, r := range rows {
		for _, p := range r.paths {
			d := strings.TrimSuffix(strings.TrimSuffix(p, "/**"), "/")
			if (strings.HasPrefix(d, "internal/") || strings.HasPrefix(d, "cmd/")) && strings.Count(d, "/") == 1 && !known[d] {
				fails = append(fails, fmt.Sprintf("OWNERSHIP.md row %q lists %s, which does not exist: remove it.", r.owner, p))
			}
		}
	}
	sort.Strings(fails)
	for _, f := range fails {
		t.Error(f)
	}
}

func TestExpandBraces(t *testing.T) {
	got := expand("internal/{a,b}/**")
	if strings.Join(got, " ") != "internal/a/** internal/b/**" {
		t.Fatal(got)
	}
	if !ownsDir("internal/a/**", "internal/a") || !ownsDir("internal/a/", "internal/a") || ownsDir("internal/a/x.go", "internal/a") {
		t.Fatal("ownsDir")
	}
}
