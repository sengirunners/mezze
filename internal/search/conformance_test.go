package search

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// drivers are every Searcher the conformance suite runs: the walker always,
// ripgrep when rg is on PATH (or ZM_TEST_RG names one). ZM_REQUIRE_RG (set
// in CI) makes a missing rg a failure, not a skip.
func drivers(t *testing.T) []Searcher {
	out := []Searcher{Walk{}}
	if rg, ok := testRipgrep(t); ok {
		out = append(out, rg)
	}
	return out
}

func testRipgrep(t *testing.T) (Ripgrep, bool) {
	t.Helper()
	rg, ok := NewRipgrep(os.Getenv("ZM_TEST_RG"))
	if !ok {
		if os.Getenv("ZM_REQUIRE_RG") != "" {
			t.Fatal("ZM_REQUIRE_RG is set and there is no rg on PATH")
		}
		t.Log("no rg: the ripgrep driver is not run here")
	}
	return rg, ok
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixture is a project tree with every case the suite checks, plus a
// directory outside it that a symlink points to.
func fixture(t *testing.T) (root, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, outside = filepath.Join(base, "proj"), filepath.Join(base, "outside")
	write(t, filepath.Join(root, "a.go"), "package a\n// needle one\nfunc x() {}\n")
	write(t, filepath.Join(root, "dir", "b.txt"), "no\nhéllo needle\n"+strings.Repeat("x", 300)+" needle\n")
	write(t, filepath.Join(root, ".gitignore"), "ignored/\n*.log\n!keep.log\n/anchored.txt\n")
	write(t, filepath.Join(root, "ignored", "x.txt"), "needle ignored dir\n")
	write(t, filepath.Join(root, "y.log"), "needle ignored file\n")
	write(t, filepath.Join(root, "keep.log"), "needle kept by negation\n")
	write(t, filepath.Join(root, "anchored.txt"), "needle anchored\n")
	write(t, filepath.Join(root, "dir", "anchored.txt"), "needle not anchored here\n")
	write(t, filepath.Join(root, ".hidden", "z.txt"), "needle hidden\n")
	write(t, filepath.Join(root, "bin.dat"), "needle\x00binary\n")
	write(t, filepath.Join(root, "flags.txt"), "run --version here\n-e pattern\n")
	write(t, filepath.Join(outside, "secret.txt"), "needle outside the project\n")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(root, "filelink.txt")); err != nil {
		t.Fatal(err)
	}
	return root, outside
}

func files(res orchapi.SearchResult, root string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range res.Matches {
		rel, _ := filepath.Rel(root, m.File)
		if !seen[rel] {
			seen[rel] = true
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

func TestConformance(t *testing.T) {
	for _, drv := range drivers(t) {
		t.Run(drv.Name(), func(t *testing.T) {
			ctx := context.Background()
			root, outside := fixture(t)
			res, err := drv.Search(ctx, Query{Text: "needle", Roots: []string{root}})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"a.go", "dir/anchored.txt", "dir/b.txt", "keep.log"}
			if got := files(res, root); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("files %v, want %v (ignored, hidden, binary and symlinked ones left out)", got, want)
			}
			for _, m := range res.Matches {
				if strings.HasPrefix(m.File, outside) || strings.Contains(m.Text, "outside") {
					t.Fatalf("a symlink led outside: %+v", m)
				}
				if !filepath.IsAbs(m.File) {
					t.Fatalf("relative file %q", m.File)
				}
			}
			byText := map[string]orchapi.SearchMatch{}
			for _, m := range res.Matches {
				byText[m.Text] = m
			}
			if m := byText["// needle one"]; m.Line != 2 || m.Col != 4 {
				t.Errorf("a.go match %+v", m)
			}
			if m := byText["héllo needle"]; m.Line != 2 || m.Col != 7 {
				t.Errorf("col in characters: %+v", m)
			}
			var long *orchapi.SearchMatch
			for i := range res.Matches {
				if res.Matches[i].Line == 3 && strings.HasSuffix(res.Matches[i].File, "b.txt") {
					long = &res.Matches[i]
				}
			}
			if long == nil || !long.TextClipped || len([]rune(long.Text)) != orchapi.SearchMaxLine || long.Col != 302 {
				t.Errorf("long line %+v", long)
			}
			if res.Truncated != nil || res.Engine != drv.Name() {
				t.Errorf("truncated %+v engine %q", res.Truncated, res.Engine)
			}

			// Queries shaped like options are text after "--".
			for _, q := range []string{"--version", "-e pattern", "--files"} {
				res, err := drv.Search(ctx, Query{Text: q, Roots: []string{root}})
				if q == "--files" {
					if err != nil || len(res.Matches) != 0 {
						t.Errorf("%q: %v %v (it is text, matching nothing)", q, res.Matches, err)
					}
					continue
				}
				if err != nil || len(res.Matches) != 1 || !strings.HasSuffix(res.Matches[0].File, "flags.txt") {
					t.Errorf("%q: %+v %v", q, res.Matches, err)
				}
			}
			// Fixed string unless Regex.
			if res, _ := drv.Search(ctx, Query{Text: "ne+dle", Roots: []string{root}}); len(res.Matches) != 0 {
				t.Errorf("a fixed string matched as a regex: %v", res.Matches)
			}
			if res, _ := drv.Search(ctx, Query{Text: "ne+dle", Regex: true, Roots: []string{root}}); len(res.Matches) != 5 {
				t.Errorf("regex: %d matches", len(res.Matches))
			}

			// Limits.
			res, _ = drv.Search(ctx, Query{Text: "needle", Roots: []string{root}, MaxMatches: 2})
			if len(res.Matches) != 2 || res.Truncated == nil || !res.Truncated.Matches {
				t.Errorf("max matches: %d %+v", len(res.Matches), res.Truncated)
			}
			res, _ = drv.Search(ctx, Query{Text: "needle", Roots: []string{root}, MaxFiles: 1})
			if len(files(res, root)) != 1 || res.Truncated == nil || !res.Truncated.Files {
				t.Errorf("max files: %v %+v", files(res, root), res.Truncated)
			}
			res, _ = drv.Search(ctx, Query{Text: "needle", Roots: []string{root}, Timeout: time.Nanosecond})
			if res.Truncated == nil || !res.Truncated.Time {
				t.Errorf("timeout: %+v", res.Truncated)
			}
			// No roots: nothing, no error.
			if res, err := drv.Search(ctx, Query{Text: "needle"}); err != nil || len(res.Matches) != 0 {
				t.Errorf("no roots: %v %v", res.Matches, err)
			}
		})
	}
}

func TestGlobRegexp(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"*.log", "a.log", true},
		{"*.log", "a/b.log", false},
		{"**/build", "x/y/build", true},
		{"**/build", "build", true},
		{"docs/**", "docs/a/b", true},
		{"docs/**", "docs", false}, // inside docs, not docs itself
		{"a/**/b", "a/x/y/b", true},
		{"a/**/b", "a/b", true},
		{"f?o", "foo", true},
		{"[ab].txt", "b.txt", true},
		{"[!ab].txt", "c.txt", true},
		{"a.b", "axb", false},
	}
	for _, c := range cases {
		re := "^" + globRegexp(c.glob) + "$"
		if got := mustMatch(re, c.path); got != c.want {
			t.Errorf("%q vs %q (%s): %v", c.glob, c.path, re, got)
		}
	}
}

func mustMatch(re, s string) bool {
	r, err := compileRegex(re)
	return err == nil && r.MatchString(s)
}

// The matcher outside a repository, ripgrep alike, on what git does with the
// same file (checked once against git status, macOS, core.ignorecase true):
// an escaped trailing space, an escaped backslash, "foo/**" with a
// re-included file, "a/**/" for directories only, unescaped trailing spaces
// dropped, and case folding on macOS.
func TestConformanceIgnoreSyntax(t *testing.T) {
	for _, drv := range drivers(t) {
		t.Run(drv.Name(), func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(base, "proj")
			write(t, filepath.Join(root, ".gitignore"),
				"sp\\ \n\\\\x\nfoo/**\n!foo/keep.txt\na/**/\nUpper.TXT\ntrail   \n")
			for _, f := range []string{"sp ", "sp", `\x`, "x", "foo/drop.txt", "foo/keep.txt", "a/f.txt", "a/d/g.txt", "upper.txt", "trail"} {
				write(t, filepath.Join(root, f), "needle\n")
			}
			res, err := drv.Search(context.Background(), Query{Text: "needle", Roots: []string{root}})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"a/f.txt", "foo/keep.txt", "sp", "x"}
			if !foldIgnoreCase {
				want = []string{"a/f.txt", "foo/keep.txt", "sp", "upper.txt", "x"}
			}
			if got := files(res, root); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("files %q, want %q", got, want)
			}
		})
	}
}

// Case folding either way, whatever the platform (the walker's matcher).
func TestIgnoreFold(t *testing.T) {
	for _, fold := range []bool{true, false} {
		ig := &ignores{ctx: context.Background(), fold: fold}
		ig.add("Upper.TXT", "/r")
		ig.add("[A-C].md", "/r")
		if got := ig.ignored("/r/upper.txt", false); got != fold {
			t.Errorf("fold %v: upper.txt ignored %v", fold, got)
		}
		if got := ig.ignored("/r/b.md", false); got != fold {
			t.Errorf("fold %v: b.md ignored %v", fold, got)
		}
		if !ig.ignored("/r/Upper.TXT", false) {
			t.Errorf("fold %v: the exact name not ignored", fold)
		}
	}
}

// git runs git in dir for a test (a throwaway home, skipped without git).
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + t.TempDir()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// Ignore files above the searched root, up to the repository's top, count:
// a parent .gitignore that ignores a secret keeps it out, as does the
// repository's info/exclude. Both drivers.
func TestConformanceParentIgnores(t *testing.T) {
	for _, drv := range drivers(t) {
		t.Run(drv.Name(), func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			repo := filepath.Join(base, "repo")
			git(t, repo, "init", "-q")
			write(t, filepath.Join(repo, ".git", "info", "exclude"), "excluded.txt\n")
			write(t, filepath.Join(repo, ".gitignore"), "secret.txt\n")
			write(t, filepath.Join(repo, "sub", ".gitignore"), "local.txt\n")
			root := filepath.Join(repo, "sub", "proj")
			write(t, filepath.Join(root, "secret.txt"), "needle secret\n")
			write(t, filepath.Join(root, "excluded.txt"), "needle excluded\n")
			write(t, filepath.Join(root, "local.txt"), "needle local\n")
			write(t, filepath.Join(root, "open.txt"), "needle open\n")
			// Above the repository's top: never read.
			write(t, filepath.Join(base, ".gitignore"), "open.txt\n")
			res, err := drv.Search(context.Background(), Query{Text: "needle", Roots: []string{root}})
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(files(res, root), ","); got != "open.txt" {
				t.Fatalf("files %s, want open.txt only", got)
			}
		})
	}
}

// A worktree's info/exclude is the main repository's (both drivers).
func TestConformanceWorktreeExclude(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	main := filepath.Join(base, "main")
	git(t, main, "init", "-q")
	write(t, filepath.Join(main, "seed.txt"), "seed\n")
	git(t, main, "add", "seed.txt")
	git(t, main, "commit", "-qm", "seed")
	wt := filepath.Join(base, "wt")
	git(t, main, "worktree", "add", "-q", wt)
	write(t, filepath.Join(main, ".git", "info", "exclude"), "excluded.txt\n")
	write(t, filepath.Join(wt, "excluded.txt"), "needle excluded\n")
	write(t, filepath.Join(wt, "kept.txt"), "needle kept\n")
	for _, drv := range drivers(t) {
		res, err := drv.Search(context.Background(), Query{Text: "needle", Roots: []string{wt}})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(files(res, wt), ","); got != "kept.txt" {
			t.Fatalf("%s: files %s", drv.Name(), got)
		}
	}
}

// In a repository the walker searches what git lists, and the drivers differ
// where SPEC 21 says: a tracked file an ignore rule matches is shown by the
// walker and hidden by ripgrep.
func TestRepoTrackedAndIgnored(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "repo")
	git(t, root, "init", "-q")
	write(t, filepath.Join(root, ".gitignore"), "*.log\n")
	write(t, filepath.Join(root, "tracked.log"), "needle tracked\n")
	write(t, filepath.Join(root, "plain.log"), "needle ignored\n")
	write(t, filepath.Join(root, "a.txt"), "needle\n")
	git(t, root, "add", "-f", "tracked.log", "a.txt")
	want := map[string]string{"walk": "a.txt,tracked.log", "ripgrep": "a.txt"}
	for _, drv := range drivers(t) {
		res, err := drv.Search(context.Background(), Query{Text: "needle", Roots: []string{root}})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(files(res, root), ","); got != want[drv.Name()] {
			t.Fatalf("%s: files %s, want %s", drv.Name(), got, want[drv.Name()])
		}
	}
}

// The walker in a repository: untracked files count; hidden ones, binaries,
// deleted ones and symlinks do not; a path the index still names under a
// directory that became a symlink is never followed out of the root; a
// nested repository is searched by its own rules.
func TestWalkRepo(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	git(t, root, "init", "-q")
	write(t, filepath.Join(outside, "x.txt"), "needle outside\n")
	write(t, filepath.Join(root, "a.txt"), "needle\n")
	write(t, filepath.Join(root, ".hidden.txt"), "needle hidden\n")
	write(t, filepath.Join(root, ".dir", "b.txt"), "needle hidden dir\n")
	write(t, filepath.Join(root, "bin.dat"), "needle\x00\n")
	write(t, filepath.Join(root, "gone.txt"), "needle\n")
	write(t, filepath.Join(root, "d", "x.txt"), "needle was here\n")
	if err := os.Symlink(filepath.Join(outside, "x.txt"), filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	git(t, root, "add", "-A")
	git(t, root, "commit", "-qm", "all")
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "d")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "d")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "new.txt"), "needle untracked\n")
	nested := filepath.Join(root, "nested")
	git(t, nested, "init", "-q")
	write(t, filepath.Join(nested, ".gitignore"), "no.txt\n")
	write(t, filepath.Join(nested, "n.txt"), "needle nested\n")
	write(t, filepath.Join(nested, "no.txt"), "needle ignored there\n")
	res, err := Walk{}.Search(context.Background(), Query{Text: "needle", Roots: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(files(res, root), ","); got != "a.txt,nested/n.txt,new.txt" {
		t.Fatalf("files %s", got)
	}
}

// git's global excludes count in a repository (the walker only: ripgrep runs
// with --no-ignore-global, SPEC 21).
func TestWalkGlobalExcludes(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(base, "home")
	write(t, filepath.Join(home, ".gitconfig"), "[core]\n\texcludesFile = "+filepath.Join(home, "ignore")+"\n")
	write(t, filepath.Join(home, "ignore"), "global.txt\n")
	root := filepath.Join(base, "repo")
	git(t, root, "init", "-q")
	write(t, filepath.Join(root, "global.txt"), "needle\n")
	write(t, filepath.Join(root, "a.txt"), "needle\n")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	res, _ := Walk{}.Search(context.Background(), Query{Text: "needle", Roots: []string{root}})
	if got := strings.Join(files(res, root), ","); got != "a.txt" {
		t.Fatalf("files %s", got)
	}
}

// No git on PATH: the matcher, from the searched directory down.
func TestWalkRepoWithoutGit(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "repo")
	git(t, root, "init", "-q")
	write(t, filepath.Join(root, ".gitignore"), "*.log\n")
	write(t, filepath.Join(root, "x.log"), "needle\n")
	write(t, filepath.Join(root, "a.txt"), "needle\n")
	t.Setenv("PATH", "")
	res, _ := Walk{}.Search(context.Background(), Query{Text: "needle", Roots: []string{root}})
	if got := strings.Join(files(res, root), ","); got != "a.txt" {
		t.Fatalf("files %s", got)
	}
}

// A listing past maxGitList is searched as far as it goes, whole names only,
// and says so.
func TestWalkGitListCapped(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "repo")
	git(t, root, "init", "-q")
	for _, f := range []string{"aaaa.txt", "bbbb.txt", "cccc.txt"} {
		write(t, filepath.Join(root, f), "needle\n")
	}
	old := maxGitList
	maxGitList = 12 // one name and its NUL, and part of the next
	t.Cleanup(func() { maxGitList = old })
	res, err := Walk{}.Search(context.Background(), Query{Text: "needle", Roots: []string{root}})
	if err != nil || res.Truncated == nil || !res.Truncated.Files {
		t.Fatalf("truncated %+v %v", res.Truncated, err)
	}
	if got := strings.Join(files(res, root), ","); got != "aaaa.txt" {
		t.Fatalf("files %s", got)
	}
}

// The matcher takes a symlinked ignore file, and reads at most 1 MiB of
// whole lines: a pattern past the cap, or cut by it, is not applied.
func TestMatcherIgnoreFile(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "proj")
	write(t, filepath.Join(root, "linked.txt"), "needle\n")
	write(t, filepath.Join(root, "a.txt"), "needle\n")
	write(t, filepath.Join(base, "real-ignore"), "linked.txt\n")
	if err := os.Symlink(filepath.Join(base, "real-ignore"), filepath.Join(root, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	res, _ := Walk{}.Search(context.Background(), Query{Text: "needle", Roots: []string{root}})
	if got := strings.Join(files(res, root), ","); got != "a.txt" {
		t.Fatalf("a symlinked .gitignore: files %s", got)
	}
	// "a.txt" past the cap; "a*" would be what is left of a line cut at it.
	fill := "#" + strings.Repeat("x", maxIgnoreFile-4) + "\n"
	write(t, filepath.Join(base, "real-ignore"), fill+"a*bcdef\na.txt\n")
	res, _ = Walk{}.Search(context.Background(), Query{Text: "needle", Roots: []string{root}})
	if got := strings.Join(files(res, root), ","); got != "a.txt,linked.txt" {
		t.Fatalf("past the cap: files %s", got)
	}
}

// The reason given is rg's "error:" line, whatever follows it.
func TestRefusedPattern(t *testing.T) {
	cases := []struct{ stderr, want string }{
		{"rg: regex parse error:\n    (?:\\Q)\n       ^^\nerror: unrecognized escape sequence\n", "unrecognized escape sequence"},
		{"rg: regex parse error:\n    (a)\\1\n       ^^\nerror: backreferences are not supported\n\nConsider enabling PCRE2 with the --pcre2 flag, which can handle backreferences\nand look-around.\n", "backreferences are not supported"},
		{"rg: compiled regex exceeds size limit of 104857600\n", "compiled regex exceeds size limit of 104857600"},
	}
	for _, c := range cases {
		if got, ok := refusedPattern(c.stderr); !ok || got != c.want {
			t.Errorf("%q: %q %v, want %q", c.stderr, got, ok, c.want)
		}
	}
	if _, ok := refusedPattern("rg: /x/locked.txt: Permission denied (os error 13)\n"); ok {
		t.Error("an unreadable file is not a refused pattern")
	}
}

// A pattern Go accepts but ripgrep refuses (its syntax differs: \Q...\E is
// Go's only) is a 400 "pattern not supported", never an empty result.
func TestRipgrepUnsupportedPattern(t *testing.T) {
	rg, ok := testRipgrep(t)
	if !ok {
		t.Skip("no rg")
	}
	root, _ := fixture(t)
	if _, err := compileRegex(`\Qa.b\E`); err != nil {
		t.Fatalf("Go should take it: %v", err)
	}
	_, err := rg.Search(context.Background(), Query{Text: `\Qa.b\E`, Regex: true, Roots: []string{root}})
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "pattern not supported") {
		t.Fatalf("got %v", err)
	}
	// One Go takes and rg compiles past its size limit: refused too, never a
	// 200 with nothing.
	big := strings.Repeat(`\pL{1000}`, 10)
	if _, err := compileRegex(big); err != nil {
		t.Fatalf("Go should take it: %v", err)
	}
	_, err = rg.Search(context.Background(), Query{Text: big, Regex: true, Roots: []string{root}})
	if !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), "exceeds size limit") {
		t.Fatalf("over rg's size limit: %v", err)
	}
	// A pattern both take, with no match, is an empty result.
	res, err := rg.Search(context.Background(), Query{Text: `zz+q`, Regex: true, Roots: []string{root}})
	if err != nil || len(res.Matches) != 0 {
		t.Fatalf("no match: %v %v", res.Matches, err)
	}
}

// Both drivers answer in one order, by file (segment by segment), then line.
// Under a limit the walker's matches are the first of the whole answer, in a
// repository and out of one; ripgrep's kept matches may differ, never their
// order.
func TestConformanceOrder(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plain, repo := filepath.Join(base, "plain"), filepath.Join(base, "repo")
	git(t, repo, "init", "-q")
	// No two names differing only in case: macOS folds them into one file.
	names := []string{"a.go", "a/b.txt", "a/z/c.txt", "b.txt", "a-b.txt", "z/a.txt", "m.txt", "a0.txt"}
	for _, f := range names {
		write(t, filepath.Join(plain, f), "needle 1\nx\nneedle 2\n")
		write(t, filepath.Join(repo, f), "needle 1\nx\nneedle 2\n")
	}
	git(t, repo, "add", "b.txt", "z/a.txt") // tracked and untracked: git lists them apart
	order := []string{"a/b.txt", "a/z/c.txt", "a-b.txt", "a.go", "a0.txt", "b.txt", "m.txt", "z/a.txt"}
	var want []string
	for _, f := range order {
		want = append(want, f+":1", f+":3")
	}
	sorted := func(ms []orchapi.SearchMatch) bool {
		return sort.SliceIsSorted(ms, func(i, j int) bool {
			a, b := ms[i], ms[j]
			if n := comparePaths(a.File, b.File); n != 0 {
				return n < 0
			}
			return a.Line < b.Line
		})
	}
	for _, root := range []string{plain, repo} {
		key := func(ms []orchapi.SearchMatch) string {
			var out []string
			for _, m := range ms {
				rel, _ := filepath.Rel(root, m.File)
				out = append(out, rel+":"+strconv.Itoa(m.Line))
			}
			return strings.Join(out, " ")
		}
		for _, drv := range drivers(t) {
			res, err := drv.Search(context.Background(), Query{Text: "needle", Roots: []string{root}})
			if err != nil {
				t.Fatal(err)
			}
			if got := key(res.Matches); got != strings.Join(want, " ") {
				t.Fatalf("%s in %s:\n%s\nwant:\n%s", drv.Name(), root, got, strings.Join(want, " "))
			}
			limited, _ := drv.Search(context.Background(), Query{Text: "needle", Roots: []string{root}, MaxMatches: 5})
			if len(limited.Matches) != 5 || !sorted(limited.Matches) {
				t.Fatalf("%s under a limit: %+v", drv.Name(), limited.Matches)
			}
			if _, walker := drv.(Walk); walker {
				if got := key(limited.Matches); got != strings.Join(want[:5], " ") {
					t.Fatalf("under a limit, not a prefix: %s", got)
				}
			}
		}
	}
	// Several roots: the same order whatever order they came in.
	for _, drv := range drivers(t) {
		res, _ := drv.Search(context.Background(), Query{Text: "needle", Roots: []string{repo, plain}})
		if len(res.Matches) != 2*len(want) || !sorted(res.Matches) || !strings.HasPrefix(res.Matches[0].File, plain) {
			t.Fatalf("%s across roots: %d matches, first %+v", drv.Name(), len(res.Matches), res.Matches[0])
		}
	}
	limited, _ := Walk{}.Search(context.Background(), Query{Text: "needle", Roots: []string{repo, plain}, MaxMatches: 5})
	if !strings.HasPrefix(limited.Matches[0].File, plain) {
		t.Fatalf("under a limit, across roots: %+v", limited.Matches[0])
	}
}

func TestComparePaths(t *testing.T) {
	in := []string{"/r/dir.go", "/r/z", "/r/dir/x", "/r/dir-b", "/r/dir/x/y", "/r/dir0", "/r/dir"}
	want := []string{"/r/dir", "/r/dir/x", "/r/dir/x/y", "/r/dir-b", "/r/dir.go", "/r/dir0", "/r/z"}
	slices.SortFunc(in, comparePaths)
	if !slices.Equal(in, want) {
		t.Fatalf("%v", in)
	}
}

// An unreadable file is not a refused pattern: rg exits 2, the search goes
// on, and another root's matches are kept.
func TestRipgrepUnreadableFile(t *testing.T) {
	rg, ok := testRipgrep(t)
	if !ok {
		t.Skip("no rg")
	}
	if os.Getuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r1, r2 := filepath.Join(base, "one"), filepath.Join(base, "two")
	write(t, filepath.Join(r1, "a.txt"), "needle one\n")
	write(t, filepath.Join(r2, "locked.txt"), "needle locked\n")
	if err := os.Chmod(filepath.Join(r2, "locked.txt"), 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(r2, "locked.txt"), 0o644) })
	// A regex that matches nothing in the unreadable root: exit 2 there.
	res, err := rg.Search(context.Background(), Query{Text: "ne+dle one", Regex: true, Roots: []string{r1, r2}})
	if err != nil || len(res.Matches) != 1 {
		t.Fatalf("an unreadable file: %+v %v", res.Matches, err)
	}
	res, err = rg.Search(context.Background(), Query{Text: "zz+", Regex: true, Roots: []string{r2}})
	if err != nil || len(res.Matches) != 0 {
		t.Fatalf("no match beside an unreadable file: %+v %v", res.Matches, err)
	}
}

// A hostile ignore file (a symlink to /dev/zero, a FIFO) never hangs the
// walker: git gets half the time limit, the matcher reads only regular
// files, at most 1 MiB, and checks the time limit while reading.
func TestWalkHostileIgnoreFiles(t *testing.T) {
	cases := map[string]func(t *testing.T, root string){
		".gitignore -> /dev/zero": func(t *testing.T, root string) {
			if err := os.Symlink("/dev/zero", filepath.Join(root, ".gitignore")); err != nil {
				t.Fatal(err)
			}
		},
		".ignore is a FIFO": func(t *testing.T, root string) {
			if err := syscall.Mkfifo(filepath.Join(root, ".ignore"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"info/exclude -> /dev/zero": func(t *testing.T, root string) {
			_ = os.MkdirAll(filepath.Join(root, ".git", "info"), 0o755)
			if err := os.Symlink("/dev/zero", filepath.Join(root, ".git", "info", "exclude")); err != nil {
				t.Fatal(err)
			}
		},
		".git is a FIFO": func(t *testing.T, root string) {
			if err := syscall.Mkfifo(filepath.Join(root, ".git"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"commondir is a FIFO": func(t *testing.T, root string) {
			gd := filepath.Join(filepath.Dir(root), "gd")
			_ = os.MkdirAll(gd, 0o755)
			if err := syscall.Mkfifo(filepath.Join(gd, "commondir"), 0o644); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(root, ".git"), "gitdir: "+gd+"\n")
		},
		"a real repository, .gitignore is a FIFO": func(t *testing.T, root string) { // git itself hangs on it
			git(t, root, "init", "-q")
			if err := syscall.Mkfifo(filepath.Join(root, ".gitignore"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"a huge regular .gitignore": func(t *testing.T, root string) {
			write(t, filepath.Join(root, ".gitignore"), strings.Repeat("pattern-"+strings.Repeat("x", 50)+"\n", 60000))
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root := filepath.Join(base, "proj")
			write(t, filepath.Join(root, "a.txt"), "needle\n")
			setup(t, root)
			// Finished is the whole claim: what it found depends on the time
			// left (a huge .gitignore compiles 17,700 regexps, slow under -race).
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, _ = Walk{}.Search(context.Background(), Query{Text: "needle", Roots: []string{root}, Timeout: 2 * time.Second})
			}()
			select {
			case <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("the walker hung on a hostile ignore file")
			}
		})
	}
}
