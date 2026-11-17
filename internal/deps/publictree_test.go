package deps

// The public tree carries no personal addresses, private names or secrets.
// Every tracked file is scanned with the rules in
// internal/deps/publictree, which scripts/publish also runs over the exact tree
// it pushes. An intentional mention goes in publictree_allow.txt with its
// reason; an allow line that no longer matches anything fails too, so the list
// only shrinks.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/deps/publictree"
)

func trackedFiles(t *testing.T) []string {
	t.Helper()
	ls := exec.Command("git", "ls-files", "-z")
	ls.Dir = "../.."
	out, err := ls.Output()
	if err != nil {
		t.Fatalf("git ls-files: %v (the gate needs the checkout's git)", err)
	}
	return strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
}

func TestPublicTreeHasNoPersonalOrInternalValues(t *testing.T) {
	files := trackedFiles(t)
	b, err := os.ReadFile("publictree_allow.txt")
	if err != nil {
		t.Fatal(err)
	}
	allow, err := publictree.ParseAllow(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file == "" {
			continue
		}
		b, err := os.ReadFile("../../" + file)
		if err != nil { // gone (a symlink, a submodule)
			continue
		}
		for _, h := range publictree.Scan(file, b, allow) {
			t.Errorf("%s: use a placeholder or a setting the owner supplies, or add an "+
				"allow line with its reason to internal/deps/publictree_allow.txt", h)
		}
	}
	// The rules that hold only in a public copy (a retired public name), over the files it keeps.
	paths, pub := publicFiles(t, files)
	for _, p := range paths {
		b, err := os.ReadFile("../../" + pub[p])
		if err != nil { // gone (a symlink, a submodule)
			continue
		}
		for _, h := range publictree.ScanPublic(p, b, allow) {
			t.Errorf("%s: a retired public name: say the Mezze app, or add an allow line with its reason to "+
				"internal/deps/publictree_allow.txt", h)
		}
	}
	// A public copy leaves out some files allow lines name (and carries its own versions of others): the private
	// repo, which has them all, keeps the list shrinking.
	private := privateTree(t)
	if private {
		scanPrivateNames(t, paths, pub, allow)
	}
	for _, a := range allow {
		if private && !a.Used && !publictree.PublishOnly(a.Rule) { // scripts/publish names its own unused lines
			t.Errorf("publictree_allow.txt: %s %s matches nothing now: delete the line", a.Glob, a.Rule)
		}
	}
}

// scanPrivateNames runs the private names list (publictree.NamesFile, which a public copy does not have) over the
// files a public copy keeps: the exclude list applied, each override standing in for the file it replaces. The
// private repo runs it on every change, so a name cannot come back between publishes; scripts/publish runs the
// same rule over the exact tree it pushes.
func scanPrivateNames(t *testing.T, paths []string, pub map[string]string, allow []*publictree.Allow) {
	t.Helper()
	nb, err := os.ReadFile("../../" + publictree.NamesFile)
	if err != nil {
		t.Fatalf("%v: the private repo keeps the list of names the public tree must not carry", err)
	}
	names, err := publictree.ParseNames(nb)
	if err != nil {
		t.Fatal(err)
	}
	if _, kept := pub[publictree.NamesFile]; kept {
		t.Fatalf("public_exclude.txt must leave out %s: the list of our private names must not ship", publictree.NamesFile)
	}
	for _, p := range paths {
		b, err := os.ReadFile("../../" + pub[p])
		if err != nil { // gone (a symlink, a submodule)
			continue
		}
		for _, h := range names.Scan(p, b, allow) {
			t.Errorf("%s: a name the public copy must not carry (internal/deps/private_names.txt): reword it, make it a "+
				"setting, or add a private-name allow line with a reason that names nothing", h)
		}
	}
}

// publicFiles is the files a public copy of files keeps, sorted, and the file that supplies each (publictree.Public):
// the exclude list applied, each override standing in for the file it replaces.
func publicFiles(t *testing.T, files []string) ([]string, map[string]string) {
	t.Helper()
	eb, err := os.ReadFile("public_exclude.txt")
	if err != nil {
		t.Fatal(err)
	}
	ex, err := publictree.ParseExclude(eb)
	if err != nil {
		t.Fatal(err)
	}
	pub := publictree.Public(ex, files)
	paths := make([]string, 0, len(pub))
	for p := range pub {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	return paths, pub
}

// public_exclude.txt lists what a public copy leaves out (scripts/publish and scripts/public-files.sh read it). Each
// line has a reason and matches a tracked file, so a renamed plan cannot slip back into the copy unnoticed. A
// `dir/**` line whose reason starts with "guard:" is the exception: that directory lives in the private
// ops repo now, and the line keeps a plan or research note someone adds here again out of a public copy.
func TestPublicExcludeLinesMatchTrackedFiles(t *testing.T) {
	if !privateTree(t) {
		t.Skip("a public copy lacks the files these lines leave out; the private repo runs this")
	}
	files := trackedFiles(t)
	b, err := os.ReadFile("public_exclude.txt")
	if err != nil {
		t.Fatal(err)
	}
	ex, err := publictree.ParseExclude(b)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range ex {
		if strings.HasPrefix(l.Why, "guard:") {
			if !strings.HasSuffix(l.Glob, "/**") {
				t.Errorf("public_exclude.txt:%d: a guard line must be a dir/** glob, got %s", l.Line, l.Glob)
			}
			continue
		}
		hit := false
		for _, file := range files {
			if l.Matches(file) {
				hit = true
				break
			}
		}
		if !hit {
			t.Errorf("public_exclude.txt:%d: %s matches no tracked file: fix the path or delete the line", l.Line, l.Glob)
		}
	}
}

// scripts/public-files.sh (what a person runs) and publictree.Exclude (what scripts/publish uses) leave out the
// same files of this commit.
func TestPublicFilesScriptAgreesWithPublish(t *testing.T) {
	sh := exec.Command("bash", "scripts/public-files.sh", "HEAD")
	sh.Dir = "../.."
	out, err := sh.Output()
	if err != nil {
		t.Fatalf("public-files.sh: %v", err)
	}
	ls := exec.Command("git", "ls-tree", "-r", "-z", "--name-only", "HEAD")
	ls.Dir = "../.."
	all, err := ls.Output()
	if err != nil {
		t.Fatal(err)
	}
	b, err := exec.Command("git", "-C", "../..", "show", "HEAD:"+publictree.ExcludeFile).Output()
	if err != nil {
		t.Fatal(err)
	}
	ex, err := publictree.ParseExclude(b)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, f := range strings.Split(strings.TrimRight(string(all), "\x00"), "\x00") {
		if !ex.Excludes(f) {
			kept = append(kept, f)
		}
	}
	if got, want := strings.TrimSpace(string(out)), strings.Join(kept, "\n"); got != want {
		t.Errorf("public-files.sh and publictree.Exclude differ:\nscript: %d files\nGo:     %d files", strings.Count(got, "\n")+1, len(kept))
	}
}

// A file under publictree.OverrideDir is the public copy's version of a file the copy keeps (scripts/publish
// refuses any other); the exclude list leaves the directory itself out.
func TestPublicCopyOverridesReplaceKeptFiles(t *testing.T) {
	if !privateTree(t) {
		t.Skip("a public copy has no override directory; the private repo runs this")
	}
	files := trackedFiles(t)
	b, err := os.ReadFile("public_exclude.txt")
	if err != nil {
		t.Fatal(err)
	}
	ex, err := publictree.ParseExclude(b)
	if err != nil {
		t.Fatal(err)
	}
	tracked := map[string]bool{}
	for _, f := range files {
		tracked[f] = true
	}
	n := 0
	for _, f := range files {
		target, ok := publictree.Override(f)
		if !ok {
			continue
		}
		n++
		if !ex.Excludes(f) {
			t.Errorf("%s: public_exclude.txt must leave %s** out", f, publictree.OverrideDir)
		}
		if !tracked[target] || ex.Excludes(target) {
			t.Errorf("%s replaces %s, which the public copy does not keep", f, target)
		}
	}
	if n == 0 {
		t.Errorf("no file under %s: delete its exclude line and this test", publictree.OverrideDir)
	}
}

// The public copy's workflows run only on GitHub-hosted runners, call no workflow and run no script the copy
// leaves out, and add no job the private workflow lacks (they are the private ones, trimmed).
func TestPublicCopyWorkflowsUseOnlyKeptFiles(t *testing.T) {
	if !privateTree(t) {
		t.Skip("a public copy has no override directory; the private repo runs this")
	}
	b, err := os.ReadFile("public_exclude.txt")
	if err != nil {
		t.Fatal(err)
	}
	ex, err := publictree.ParseExclude(b)
	if err != nil {
		t.Fatal(err)
	}
	kept := func(p string) bool {
		for _, f := range trackedFiles(t) {
			if (f == p || strings.HasPrefix(f, p+"/")) && !ex.Excludes(f) {
				return true
			}
		}
		return false
	}
	hosted := map[string]bool{"ubuntu-latest": true, "${{ vars.RELEASE_RUNNER || 'ubuntu-latest' }}": true}
	runsOn := regexp.MustCompile(`^\s*runs-on:\s*(.+?)\s*$`)
	ref := regexp.MustCompile(`(?:^|[\s"'=./({:,])((?:scripts|\.github|deploy|docs)/[A-Za-z0-9_./-]*[A-Za-z0-9_-])`)
	needs := regexp.MustCompile(`^\s*needs:\s*\[?([^\]]*)\]?\s*$`)
	job := regexp.MustCompile(`^  ([A-Za-z_][A-Za-z0-9_-]*):\s*$`)
	jobs := func(body string) map[string]bool {
		out, in := map[string]bool{}, false
		for _, l := range strings.Split(body, "\n") {
			if l == "jobs:" {
				in = true
			} else if in {
				if m := job.FindStringSubmatch(l); m != nil {
					out[m[1]] = true
				}
			}
		}
		return out
	}
	dir := filepath.Join("../..", publictree.OverrideDir, ".github", "workflows")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Every workflow the copy keeps has its public version here: a new private workflow on our runners must be
	// left out or given one, never published as written.
	for _, f := range trackedFiles(t) {
		if strings.HasPrefix(f, ".github/workflows/") && !ex.Excludes(f) {
			if _, err := os.Stat(filepath.Join(dir, strings.TrimPrefix(f, ".github/workflows/"))); err != nil {
				t.Errorf("%s is kept but has no public version under %s.github/workflows: add one or exclude it", f, publictree.OverrideDir)
			}
		}
	}
	for _, e := range ents {
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for i, l := range strings.Split(string(body), "\n") {
			if m := runsOn.FindStringSubmatch(l); m != nil && !hosted[m[1]] {
				t.Errorf("%s:%d: runs-on %s: the public copy runs on GitHub-hosted ubuntu-latest only", e.Name(), i+1, m[1])
			}
			for _, m := range ref.FindAllStringSubmatch(l, -1) {
				if !kept(m[1]) {
					t.Errorf("%s:%d: %s is not in the public copy", e.Name(), i+1, m[1])
				}
			}
		}
		own := jobs(string(body))
		for i, l := range strings.Split(string(body), "\n") {
			if m := needs.FindStringSubmatch(l); m != nil {
				for _, n := range strings.Split(m[1], ",") {
					if n = strings.TrimSpace(n); n != "" && !own[n] {
						t.Errorf("%s:%d: needs %s, which is not a job here", e.Name(), i+1, n)
					}
				}
			}
		}
		priv, err := os.ReadFile(filepath.Join("../../.github/workflows", e.Name()))
		if err != nil {
			t.Fatalf("%s overrides no private workflow: %v", e.Name(), err)
		}
		pj := jobs(string(priv))
		for j := range jobs(string(body)) {
			if !pj[j] {
				t.Errorf("%s: job %s is not in the private workflow", e.Name(), j)
			}
		}
	}
}
