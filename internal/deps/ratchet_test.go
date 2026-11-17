package deps

// Size ratchet: internal/work's top level and every non-test .go file have a
// line cap in caps.json. The test fails when the code grows past a cap, and
// also when it shrinks well below one (lower the cap to the number printed,
// so the room is not refilled by accident). caps.json is a plain file: an
// agent may change a cap in its own PR with a one-line reason.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type fileCap struct {
	Cap   int    `json:"cap"`
	Owner string `json:"owner"`
	Added string `json:"added"`
	Why   string `json:"why"`
}

type caps struct {
	WorkTopLines int                `json:"work_top_lines"`
	WorkTopSlack int                `json:"work_top_slack"`
	FileMaxLines int                `json:"file_max_lines"`
	FileSlack    int                `json:"file_slack"`
	FilesOverCap map[string]fileCap `json:"files_over_cap"`
}

func loadCaps(t *testing.T) caps {
	t.Helper()
	b, err := os.ReadFile("caps.json")
	if err != nil {
		t.Fatal(err)
	}
	var c caps
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&c); err != nil {
		t.Fatalf("caps.json: %v", err)
	}
	if c.WorkTopLines <= 0 || c.FileMaxLines <= 0 {
		t.Fatal("caps.json: work_top_lines and file_max_lines are required")
	}
	return c
}

func lines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(b, []byte("\n"))
}

// goFiles lists the repo's non-test .go files, tracked or new (not ignored),
// repo-relative; deleted files are left out.
func goFiles(t *testing.T) []string {
	t.Helper()
	if _, err := os.Stat("../../.git"); err != nil {
		t.Skip("not a git checkout")
	}
	cmd := exec.Command("git", "ls-files", "-co", "--exclude-standard", "*.go")
	cmd.Dir = "../.."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	var fs []string
	seen := map[string]bool{}
	for _, f := range strings.Fields(string(out)) {
		if strings.HasSuffix(f, "_test.go") || seen[f] {
			continue
		}
		if _, err := os.Stat(filepath.Join("../..", f)); err != nil {
			continue // deleted in the working tree
		}
		seen[f] = true
		fs = append(fs, f)
	}
	return fs
}

func TestSizeRatchet(t *testing.T) {
	c := loadCaps(t)
	var fails []string

	// internal/work's top level.
	top := 0
	ents, err := os.ReadDir("../../internal/work")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if n := e.Name(); !e.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
			top += lines(t, filepath.Join("../../internal/work", n))
		}
	}
	switch {
	case top > c.WorkTopLines:
		fails = append(fails, fmt.Sprintf("internal/work's top level has %d non-test lines, over its cap of %d: it does not grow. Put new code in a sub-package with its own interface (internal/work/<concern>) or move code out to make room; if it must grow, raise work_top_lines in internal/deps/caps.json with a one-line reason; see docs/ARCHITECTURE.md.", top, c.WorkTopLines))
	case top < c.WorkTopLines-c.WorkTopSlack:
		fails = append(fails, fmt.Sprintf("internal/work's top level shrank to %d lines, well under its cap of %d: lower work_top_lines in internal/deps/caps.json to %d.", top, c.WorkTopLines, top))
	}

	// Every non-test file.
	seen := map[string]bool{}
	for _, f := range goFiles(t) {
		n := lines(t, filepath.Join("../..", f))
		fc, listed := c.FilesOverCap[f]
		seen[f] = listed
		switch {
		case !listed && n > c.FileMaxLines:
			fails = append(fails, fmt.Sprintf("%s has %d lines, over the %d-line cap for a file: split it by concern (see docs/ARCHITECTURE.md), or add it to files_over_cap in internal/deps/caps.json with a cap, owner and one-line reason (move the entry when a listed file is renamed).", f, n, c.FileMaxLines))
		case listed && n > fc.Cap:
			fails = append(fails, fmt.Sprintf("%s has %d lines, over its cap of %d (caps.json, owner %s): split it, or raise its cap with a one-line reason.", f, n, fc.Cap, fc.Owner))
		case listed && n <= c.FileMaxLines:
			fails = append(fails, fmt.Sprintf("%s is down to %d lines, under the %d-line cap: delete its entry from files_over_cap in internal/deps/caps.json.", f, n, c.FileMaxLines))
		case listed && n < fc.Cap-c.FileSlack:
			fails = append(fails, fmt.Sprintf("%s shrank to %d lines, well under its cap of %d: lower its cap in internal/deps/caps.json to %d.", f, n, fc.Cap, n))
		}
	}
	for f := range c.FilesOverCap {
		if _, ok := seen[f]; !ok {
			fails = append(fails, fmt.Sprintf("caps.json lists %s, which is gone: delete its entry.", f))
		}
	}
	sort.Strings(fails)
	for _, f := range fails {
		t.Error(f)
	}
}
