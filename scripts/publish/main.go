// Command publish copies one release of this private repository to the public one and tags it (owner ruling
// 2026-11-12: keep working privately, copy each release to public). The copy is one way: it reads only the public
// repo's main and tags, and pushes one new commit whose tree is the release commit's public files and whose only
// parent is the previous public release. No private history, author or left-out file reaches the public repo. The
// commit message ends in a "Source: <private sha>" line, which the fleet's self-update reads to place a public
// release on the private line.
//
//	go run ./scripts/publish -tag v0.1.0 -ref <commit> -ident 'Name <email>' [-push]
//
// Everything it publishes is read from the commit, never the working tree: the file list is the commit's tree less
// the commit's internal/deps/public_exclude.txt. Exactly that tree is scanned before it is written, with the
// commit's allow list: the public-tree gate (internal/deps/publictree: personal addresses, our own names,
// shaped tokens), then gitleaks (pinned by sha256, its default rules, binary files read as their strings), then a
// model judge of every config-like file (a credential or a personal or customer value in an assignment).
//
// It refuses unless:
//   - the commit is on origin/main: everything lands on private main first;
//   - its rolling release (zm-main-<sha12>, zm-release.yml) is at least -soak old: the fleet has run that tree;
//   - go.mod names -module, the public repo's path, or `go install` from it fails;
//   - the public main is absent or sits on a tag: only releases land there. An outside contributor's change is
//     applied here as a patch once a person approves it (AGENTS.md), never merged on the public repo;
//   - -tag is vMAJOR.MINOR.PATCH, new on the public repo, and the tree differs from the last release;
//   - the public tree passes every scan (any hit refuses; an intentional mention needs an allow line on private
//     main); a scan that cannot run, or a judge that does not answer, holds it. The names of our own people,
//     handles and organisations are one of the scans: internal/deps/private_names.txt, which the copy leaves out
//     and the publish refuses to ship;
//   - the plugin installer's pin (integrations/claude-code-compute/zm-release.pin, as the copy carries it) names no
//     release (a blank tag), or a release the public repo serves with exactly the SHA256SUMS the pin hashes: a pin
//     outsiders cannot download is refused;
//   - -ident is a neutral release identity: not a consumer mail address, nor the checkout's or the commit's own.
//
// Without -push it builds the commit locally and prints what it would push. The fleet follows the public tags.
// A file under internal/deps/publiccopy/ is the public copy's version of the file at the rest of its path (the
// workflows that use internal runners); the copy carries it in that file's place.
//
//	go run ./scripts/publish -ref <commit> -export <empty dir>
//
// builds and scans the same public tree and writes its files into the directory, so its tests can run there.
// It checks nothing about releases (tag, soak, the public repo) and never pushes.
package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/deps/publictree"
	"github.com/sengirunners/mezze/internal/judge"
)

type opts struct {
	Dir    string // root of the private checkout
	Ref    string // the private commit to publish
	Tag    string
	To     string // the public repo's git URL
	Module string // the module path go.mod must name
	Ident  string // author and committer of the public commit, "Name <email>"
	Soak   time.Duration
	Push   bool
	Export string // with a directory: write the public tree's files there instead of publishing
	Now    time.Time
	Out    io.Writer // what it reports (os.Stdout in main)
	// Cache keeps the pinned gitleaks archive between runs.
	Cache string
	// Judge reads the config-like files (internal/judge's Caller; main passes judge.Default); nil holds every release.
	Judge Caller
	// PinSums returns the SHA256SUMS file of the public repo's release tag (gh in main, a stub in tests); nil holds
	// every copy whose pin names a release.
	PinSums func(repo, tag string) ([]byte, error)
	// ReleasedAt says when the private release named tag was published (gh in main, a stub in tests).
	ReleasedAt func(dir, tag string) (time.Time, error)
}

var (
	tagRE   = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	identRE = regexp.MustCompile(`^([^<>]*[^<>\s]) <([^<>\s]+@[^<>\s]+)>$`)
	// userinfoRE is the credentials part of a URL (the user:token@ after the scheme).
	userinfoRE = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s]+@`)
)

// redact hides any credentials in a URL inside s (a -to with a token in it) before it is printed.
func redact(s string) string { return userinfoRE.ReplaceAllString(s, "${1}***@") }

func main() {
	o := opts{Now: time.Now(), Out: os.Stdout, ReleasedAt: ghReleasedAt, PinSums: ghPinSums}
	if c, err := os.UserCacheDir(); err == nil {
		o.Cache = filepath.Join(c, "zm-publish")
	}
	// The config judge is internal/judge's: ZM_JUDGE picks the model (default claude:haiku) on this machine's
	// plan login, with the child's minimal environment; no answer holds the release.
	o.Judge = judge.Default(nil)
	flag.StringVar(&o.Ref, "ref", "", "the private commit to publish (on origin/main)")
	flag.StringVar(&o.Tag, "tag", "", "the public release tag, vMAJOR.MINOR.PATCH")
	flag.StringVar(&o.To, "to", "https://github.com/sengirunners/mezze.git", "the public repo")
	flag.StringVar(&o.Module, "module", "github.com/sengirunners/mezze", "the module path go.mod must name")
	flag.StringVar(&o.Ident, "ident", "", `author of the public commit, "Name <email>": never a person's private address`)
	flag.DurationVar(&o.Soak, "soak", 5*24*time.Hour, "how long the fleet must have had the commit; shorter only on an owner's say-so")
	flag.BoolVar(&o.Push, "push", false, "push the commit and tag (default: build and print only)")
	flag.StringVar(&o.Export, "export", "", "an empty directory: build and scan -ref's public tree, write its files there, publish nothing")
	flag.Parse()
	dir, err := git("", nil, nil, "rev-parse", "--show-toplevel")
	if err == nil {
		o.Dir = dir
		err = run(o)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "publish:", err)
		os.Exit(1)
	}
}

// run publishes o; its error never carries credentials from a URL (git's own messages name -to).
func run(o opts) error {
	if err := publish(o); err != nil {
		return errors.New(redact(err.Error()))
	}
	return nil
}

func publish(o opts) error {
	if o.Export != "" {
		return export(o)
	}
	if !tagRE.MatchString(o.Tag) {
		return fmt.Errorf("-tag %q: want vMAJOR.MINOR.PATCH", o.Tag)
	}
	id := identRE.FindStringSubmatch(o.Ident)
	if id == nil {
		return fmt.Errorf(`-ident %q: want "Name <email>"`, o.Ident)
	}
	if o.Ref == "" {
		return errors.New("-ref: name the private commit to publish")
	}
	g := func(args ...string) (string, error) { return git(o.Dir, nil, nil, args...) }
	if origin, _ := g("remote", "get-url", "origin"); sameRepo(origin, o.To) {
		return fmt.Errorf("-to %s is this checkout's origin: run from the private repo", o.To)
	}
	sha, err := g("rev-parse", "--verify", "--end-of-options", o.Ref+"^{commit}")
	if err != nil {
		return err
	}
	if err := neutralIdent(g, sha, id[1], id[2]); err != nil {
		return err
	}
	if _, err := g("fetch", "-q", "origin", "+refs/heads/main:refs/remotes/origin/main"); err != nil {
		return err
	}
	if _, err := g("merge-base", "--is-ancestor", sha, "refs/remotes/origin/main"); err != nil {
		return fmt.Errorf("%s is not on origin/main: everything lands on private main first", sha)
	}
	gomod, err := g("show", sha+":go.mod")
	if err != nil {
		return err
	}
	if m := regexp.MustCompile(`(?m)^module\s+(\S+)`).FindStringSubmatch(gomod); m == nil || m[1] != o.Module {
		return fmt.Errorf("go.mod at %s does not name module %s: go install from the public repo would fail", sha[:12], o.Module)
	}
	rel := "zm-main-" + sha[:12]
	at, err := o.ReleasedAt(o.Dir, rel)
	if err != nil {
		return fmt.Errorf("no release %s, so the fleet never ran %s: %w", rel, sha[:12], err)
	}
	if age := o.Now.Sub(at); age < o.Soak {
		return fmt.Errorf("release %s is %s old; the fleet runs a tree %s before it is published", rel, age.Round(time.Minute), o.Soak)
	}

	tree, n, err := publicTree(o, sha)
	if err != nil {
		return err
	}
	if err := checkPin(o, g, tree); err != nil {
		return err
	}
	parent, err := publicHead(g, o.To, o.Tag)
	if err != nil {
		return err
	}
	// The Source line is the fleet's way back to the private line (it reads exactly ^Source: <40 hex>$).
	args := []string{"commit-tree", tree, "-m", "Mezze " + o.Tag, "-m", "Source: " + sha}
	if parent != "" {
		if pt, err := g("rev-parse", parent+"^{tree}"); err != nil {
			return err
		} else if pt == tree {
			return fmt.Errorf("%s has the same public files as the last release %s: nothing to publish", sha[:12], parent[:12])
		}
		args = append(args, "-p", parent)
	}
	// The date is the source commit's moment in UTC ("<unix seconds> +0000"): its own offset would show the
	// committer's time zone in public history for good.
	unix, err := g("show", "-s", "--format=%ct", sha)
	if err != nil {
		return err
	}
	date := unix + " +0000"
	env := []string{"GIT_AUTHOR_NAME=" + id[1], "GIT_AUTHOR_EMAIL=" + id[2], "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=" + id[1], "GIT_COMMITTER_EMAIL=" + id[2], "GIT_COMMITTER_DATE=" + date}
	commit, err := git(o.Dir, env, nil, args...)
	if err != nil {
		return err
	}
	from := "first release"
	if parent != "" {
		stat, _ := g("diff", "--shortstat", parent, commit)
		from = "after " + parent[:12] + ":" + stat
	}
	fmt.Fprintf(o.Out, "%s: public commit %s (%d files, %s) from private %s\n", o.Tag, commit, n, from, sha)
	push := []string{"push", "--atomic", o.To, commit + ":refs/heads/main", commit + ":refs/tags/" + o.Tag}
	if !o.Push {
		fmt.Fprintln(o.Out, redact("dry run; with -push it runs: git "+strings.Join(push, " ")))
		return nil
	}
	if _, err := g(push...); err != nil {
		return err
	}
	fmt.Fprintln(o.Out, redact(fmt.Sprintf("published %s to %s", o.Tag, o.To)))
	return nil
}

// pinFile is the compute plugin's release pin.
const pinFile = "integrations/claude-code-compute/zm-release.pin"

// checkPin refuses a public tree whose installer pin outsiders cannot use: a pin naming a release must name one the
// public repo serves, with the SHA256SUMS the pin hashes. A blank tag= (no release pinned) is fine: the installer says
// where to install from instead.
func checkPin(o opts, g func(...string) (string, error), tree string) error {
	if _, err := g("cat-file", "-e", tree+":"+pinFile); err != nil {
		return nil // the copy has no plugin pin
	}
	text, err := g("show", tree+":"+pinFile)
	if err != nil {
		return err
	}
	// Read the way scripts/zm-install does: the first line of each key, raw (a stray space fails there, so here);
	// each repo= starts a source and the last base_url= after it is where that source's releases are.
	pin := map[string]string{}
	type source struct{ repo, base string }
	var sources []*source
	for _, l := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(l, "=")
		if !ok || strings.HasPrefix(l, "#") {
			continue
		}
		switch k {
		case "repo":
			sources = append(sources, &source{repo: v})
		case "base_url":
			if len(sources) > 0 {
				sources[len(sources)-1].base = v
			}
		}
		if _, seen := pin[k]; !seen {
			pin[k] = v
		}
	}
	if pin["tag"] == "" {
		return nil
	}
	repo := githubSlug(o.To)
	if repo == "" || o.PinSums == nil {
		return fmt.Errorf("held: %s pins release %s, and it cannot be checked against %s (not a GitHub repository, or no way to read its releases)", pinFile, pin["tag"], redact(o.To))
	}
	sums, err := o.PinSums(repo, pin["tag"])
	if err != nil {
		return fmt.Errorf("held: %s pins release %s, and github.com/%s does not serve it (an installer from the public copy would fail): %w", pinFile, pin["tag"], repo, err)
	}
	if got := fmt.Sprintf("%x", sha256.Sum256(sums)); got != pin["sums_sha256"] {
		return fmt.Errorf("%s pins SHA256SUMS %s for release %s, but github.com/%s serves %s: the installer would refuse it", pinFile, pin["sums_sha256"], pin["tag"], repo, got)
	}
	// What the installer then asks for: this repo first, every source reachable over https, and the pinned
	// version's tarball for every platform.
	if len(sources) == 0 || !strings.EqualFold(sources[0].repo, repo) {
		return fmt.Errorf("%s names no first source, or not the repository being published (github.com/%s)", pinFile, repo)
	}
	for _, src := range sources {
		if !strings.HasPrefix(src.base, "https://") {
			return fmt.Errorf("%s: the source %s has base_url %q: the installer needs one over https", pinFile, src.repo, src.base)
		}
	}
	if want := "https://github.com/" + sources[0].repo + "/releases/download"; sources[0].base != want {
		return fmt.Errorf("%s downloads from %q, not %s: the installer would fetch from the wrong place", pinFile, sources[0].base, want)
	}
	if strings.ContainsAny(pin["tag"]+pin["version"], "/ ") || strings.Contains(pin["tag"]+pin["version"], "..") {
		return fmt.Errorf("%s: the tag or version has a slash, a space or '..': the installer refuses it", pinFile)
	}
	listed := string(sums)
	for _, plat := range []string{"darwin_amd64", "darwin_arm64", "linux_amd64", "linux_arm64"} {
		name := "zm_" + pin["version"] + "_" + plat + ".tar.gz"
		if !regexp.MustCompile(`(?m)^[0-9a-f]{64} [ *]` + regexp.QuoteMeta(name) + `$`).MatchString(listed) {
			return fmt.Errorf("%s pins version %q, but release %s of github.com/%s lists no %s: the installer would fail", pinFile, pin["version"], pin["tag"], repo, name)
		}
	}
	return nil
}

var githubRE = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// githubSlug is owner/name of a https://github.com/owner/name URL, "" for any other.
func githubSlug(url string) string {
	if m := githubRE.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	return ""
}

func ghPinSums(repo, tag string) ([]byte, error) {
	out, err := exec.Command("gh", "release", "download", tag, "-R", repo, "-p", "SHA256SUMS", "-O", "-").Output()
	if err != nil {
		return nil, fmt.Errorf("gh release download %s -R %s: %w", tag, repo, err)
	}
	return out, nil
}

// export writes the public tree of o.Ref into the empty directory o.Export, after the same scans publish runs.
func export(o opts) error {
	if o.Push {
		return errors.New("-export writes a directory and never pushes: drop -push")
	}
	if o.Ref == "" {
		return errors.New("-ref: name the private commit to export")
	}
	if err := os.MkdirAll(o.Export, 0o755); err != nil {
		return err
	}
	if ents, err := os.ReadDir(o.Export); err != nil {
		return err
	} else if len(ents) > 0 {
		return fmt.Errorf("-export %s is not empty", o.Export)
	}
	sha, err := git(o.Dir, nil, nil, "rev-parse", "--verify", "--end-of-options", o.Ref+"^{commit}")
	if err != nil {
		return err
	}
	tree, n, err := publicTree(o, sha)
	if err != nil {
		return err
	}
	// Write the blobs themselves: git archive would apply a tree's .gitattributes (export-ignore, export-subst)
	// after the scan.
	ls, err := exec.Command("git", "-C", o.Dir, "ls-tree", "-r", "-z", "--full-tree", tree).Output()
	if err != nil {
		return fmt.Errorf("git ls-tree %s: %w", tree, err)
	}
	var paths, objs []string
	modes := map[string]os.FileMode{}
	for _, e := range strings.Split(strings.TrimSuffix(string(ls), "\x00"), "\x00") {
		meta, path, _ := strings.Cut(e, "\t")
		f := strings.Fields(meta)
		if len(f) != 3 || f[1] != "blob" || (f[0] != "100644" && f[0] != "100755") || !filepath.IsLocal(path) {
			return fmt.Errorf("public tree %s: cannot write entry %q", tree, e)
		}
		modes[path] = 0o644
		if f[0] == "100755" {
			modes[path] = 0o755
		}
		paths, objs = append(paths, path), append(objs, f[2])
	}
	err = readBlobs(o.Dir, paths, objs, func(p string, body []byte) error {
		dst := filepath.Join(o.Export, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.WriteFile(dst, body, modes[p])
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(o.Out, "public tree %s (%d files) from private %s, written to %s\n", tree, n, sha, o.Export)
	return nil
}

// publicHead fetches the public main and returns its commit ("" when the repo has no main yet). It refuses when
// the tag exists or when main has moved past its last tag: only this command writes there.
func publicHead(g func(...string) (string, error), to, tag string) (string, error) {
	out, err := g("ls-remote", to)
	if err != nil {
		return "", err
	}
	var head string
	tagged := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		sha, ref, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		switch {
		case ref == "refs/heads/main":
			head = sha
		case ref == "refs/tags/"+tag || ref == "refs/tags/"+tag+"^{}":
			return "", fmt.Errorf("%s already has tag %s", to, tag)
		case strings.HasPrefix(ref, "refs/tags/v"):
			tagged[sha] = true
		}
	}
	if head == "" {
		return "", nil
	}
	if !tagged[head] {
		return "", fmt.Errorf("%s main (%s) is not a release: something was pushed or merged there. Outside changes are "+
			"applied in the private repo as patches; put main back on the last release tag first", to, head[:12])
	}
	if _, err := g("fetch", "-q", to, "+refs/heads/main:refs/publish/main"); err != nil {
		return "", err
	}
	return head, nil
}

// neutralIdent refuses an -ident that looks like a person's: a consumer mail address, or the name or address
// this checkout commits as or the source commit was written by.
func neutralIdent(g func(...string) (string, error), sha, name, email string) error {
	personal := publictree.PersonalEmail(email)
	var seen []string
	for _, v := range []string{"GIT_AUTHOR_IDENT", "GIT_COMMITTER_IDENT"} {
		if out, err := g("var", v); err == nil {
			seen = append(seen, out)
		}
	}
	if out, err := g("show", "-s", "--format=%an <%ae>%n%cn <%ce>", sha); err == nil {
		seen = append(seen, strings.Split(out, "\n")...)
	}
	for _, s := range seen {
		if m := identRE.FindStringSubmatch(strings.TrimSpace(strings.SplitN(s, ">", 2)[0] + ">")); m != nil &&
			(strings.EqualFold(m[2], email) || strings.EqualFold(m[1], name)) {
			personal = true
		}
	}
	if personal {
		return fmt.Errorf("-ident %q looks like a person's (a personal address, or the one this checkout or the commit "+
			"uses): the public commit carries the neutral release identity only", name+" <"+email+">")
	}
	return nil
}

// publicTree writes the tree of sha's public files into the private object store and returns it and its file count.
// Everything comes from the commit itself: its tree, its exclude list and its allow list, so a dirty or stale
// checkout cannot change what is published. The gate scans exactly the files written; any hit refuses.
func publicTree(o opts, sha string) (string, int, error) {
	dir := o.Dir
	g := func(args ...string) (string, error) { return git(dir, nil, nil, args...) }
	exclude, err := g("show", sha+":"+publictree.ExcludeFile)
	if err != nil {
		return "", 0, fmt.Errorf("%s has no %s, so nothing says what stays private: %w", sha[:12], publictree.ExcludeFile, err)
	}
	ex, err := publictree.ParseExclude([]byte(exclude))
	if err != nil {
		return "", 0, fmt.Errorf("%s at %s: %w", publictree.ExcludeFile, sha[:12], err)
	}
	var allow []*publictree.Allow
	if _, err := g("cat-file", "-e", sha+":"+publictree.AllowFile); err == nil {
		b, err := g("show", sha+":"+publictree.AllowFile)
		if err != nil {
			return "", 0, err
		}
		if allow, err = publictree.ParseAllow([]byte(b)); err != nil {
			return "", 0, fmt.Errorf("at %s: %w", sha[:12], err)
		}
	}
	// The names the copy must not carry live in a private file. Without it nothing says what to look for, so the
	// publish holds; and a copy that would keep the file is refused, so the list never ships.
	namesText, err := g("show", sha+":"+publictree.NamesFile)
	if err != nil {
		return "", 0, fmt.Errorf("held: %s has no %s, so nothing says which names stay out of the public tree: %w", sha[:12], publictree.NamesFile, err)
	}
	if !ex.Excludes(publictree.NamesFile) {
		return "", 0, fmt.Errorf("%s at %s does not leave out %s: the list of our private names would ship", publictree.ExcludeFile, sha[:12], publictree.NamesFile)
	}
	names, err := publictree.ParseNames([]byte(namesText))
	if err != nil {
		return "", 0, fmt.Errorf("at %s: %w", sha[:12], err)
	}
	all, err := exec.Command("git", "-C", dir, "ls-tree", "-r", "-z", "--full-tree", sha).Output()
	if err != nil {
		return "", 0, fmt.Errorf("git ls-tree %s: %w", sha, err)
	}
	type entry struct{ path, mode, kind, obj string }
	var ents []entry
	over := map[string]entry{} // a public copy's version of a file, by the path it replaces
	for _, e := range strings.Split(strings.TrimSuffix(string(all), "\x00"), "\x00") {
		meta, path, ok := strings.Cut(e, "\t")
		f := strings.Fields(meta)
		if !ok || len(f) != 3 {
			return "", 0, fmt.Errorf("git ls-tree %s: unreadable entry %q", sha[:12], e)
		}
		if target, ok := publictree.Override(path); ok {
			if f[1] != "blob" || (f[0] != "100644" && f[0] != "100755") {
				return "", 0, fmt.Errorf("%s: a %s %s entry cannot stand in for a file: only a regular file can", path, f[0], f[1])
			}
			if publictree.Exempt(target) || target == publictree.ExcludeFile {
				return "", 0, fmt.Errorf("%s: the public-tree gate's own files are never overridden", path)
			}
			over[target] = entry{target, f[0], f[1], f[2]}
			continue
		}
		ents = append(ents, entry{path, f[0], f[1], f[2]})
	}
	var info bytes.Buffer
	var paths, objs []string
	for _, e := range ents {
		if ex.Excludes(e.path) {
			continue
		}
		if o, ok := over[e.path]; ok {
			if o.mode != e.mode {
				return "", 0, fmt.Errorf("%s%s is mode %s but %s is %s: an override keeps its file's mode", publictree.OverrideDir, e.path, o.mode, e.path, e.mode)
			}
			e = o
			delete(over, e.path)
		}
		if e.kind != "blob" {
			return "", 0, fmt.Errorf("%s: a %s entry (a submodule?) cannot be published; exclude it or remove it", e.path, e.kind)
		}
		fmt.Fprintf(&info, "%s %s %s\t%s\x00", e.mode, e.kind, e.obj, e.path)
		paths, objs = append(paths, e.path), append(objs, e.obj)
	}
	for target := range over {
		return "", 0, fmt.Errorf("%s%s replaces %s, which the public copy of %s does not keep: an override only "+
			"replaces a kept file", publictree.OverrideDir, target, target, sha[:12])
	}
	idx, err := os.MkdirTemp("", "publish")
	if err != nil {
		return "", 0, err
	}
	defer os.RemoveAll(idx)
	if err := scanTree(o, idx, paths, objs, allow, names); err != nil {
		return "", 0, fmt.Errorf("the public tree of %s fails the publish scans, so it is not published; fix it on private "+
			"main, or add an allow line with its reason to %s there, and publish that commit:\n%w", sha[:12], publictree.AllowFile, err)
	}
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(idx, "index")}
	if _, err := git(dir, env, nil, "read-tree", "--empty"); err != nil {
		return "", 0, err
	}
	if _, err := git(dir, env, &info, "update-index", "-z", "--index-info"); err != nil {
		return "", 0, err
	}
	tree, err := git(dir, env, nil, "write-tree")
	return tree, len(paths), err
}

// scanTree runs every scan over exactly the files written, in order, and stops at the first that refuses: the
// public-tree gate (internal/deps/publictree, with the private names list), then gitleaks over the whole tree, then the model judge over the
// config-like files. work is a private temp directory.
func scanTree(o opts, work string, paths, objs []string, allow []*publictree.Allow, names publictree.Names) error {
	sd := &scanDir{root: filepath.Join(work, "scan")}
	var hits []string
	var cfg []judgeFile
	err := readBlobs(o.Dir, paths, objs, func(p string, body []byte) error {
		for _, h := range append(publictree.Scan(p, body, allow), publictree.ScanPublic(p, body, allow)...) {
			hits = append(hits, "  "+h.String())
		}
		for _, h := range names.Scan(p, body, allow) {
			hits = append(hits, "  "+h.String())
		}
		if configLike(p) && !isBinary(body) && !allowed(allow, p, publictree.JudgeRule) {
			cfg = append(cfg, judgeFile{Path: p, Text: string(body)})
		}
		return sd.add(p, body)
	})
	if err != nil {
		return err
	}
	if len(hits) > 0 {
		return errors.New("the public-tree gate:\n" + strings.Join(hits, "\n"))
	}
	bin, err := gitleaksBin(o.Cache, work)
	if err != nil {
		return fmt.Errorf("held: %w", err)
	}
	if hits, err = sd.gitleaks(bin, allow); err != nil {
		return fmt.Errorf("held: %w", err)
	} else if len(hits) > 0 {
		return fmt.Errorf("gitleaks %s (its default rules):\n%s", gitleaksVersion, strings.Join(hits, "\n"))
	}
	if err := judgeConfig(o.Judge, cfg); err != nil {
		return err
	}
	for _, a := range allow {
		if !a.Used && publictree.PublishOnly(a.Rule) {
			fmt.Fprintf(o.Out, "note: %s line %q %s matches nothing in this tree: delete it\n", publictree.AllowFile, a.Glob, a.Rule)
		}
	}
	fmt.Fprintf(o.Out, "scanned %d files: the public-tree gate, gitleaks %s, and the config judge over %d config-like files\n",
		len(paths), gitleaksVersion, len(cfg))
	return nil
}

// allowed says whether an allow line covers rule for p, marking the lines it used.
func allowed(allow []*publictree.Allow, p, rule string) bool {
	ok := false
	for _, a := range allow {
		if a.Matches(p, rule) {
			a.Used, ok = true, true
		}
	}
	return ok
}

// readBlobs hands each blob to fn, read with one git cat-file --batch.
func readBlobs(dir string, paths, objs []string, fn func(path string, body []byte) error) error {
	c := exec.Command("git", "-C", dir, "cat-file", "--batch")
	c.Stdin = strings.NewReader(strings.Join(objs, "\n") + "\n")
	var errb bytes.Buffer
	c.Stderr = &errb
	out, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		return err
	}
	defer func() { _ = c.Process.Kill(); _ = c.Wait() }() // a refusal stops reading early: never leave git blocked
	r := bufio.NewReader(out)
	for i, path := range paths {
		head, err := r.ReadString('\n')
		f := strings.Fields(head)
		if err != nil || len(f) != 3 || f[0] != objs[i] || f[1] != "blob" {
			return fmt.Errorf("git cat-file %s (%s): %q %v %s", objs[i], path, head, err, strings.TrimSpace(errb.String()))
		}
		size, err := strconv.Atoi(f[2])
		if err != nil {
			return err
		}
		body := make([]byte, size+1) // the blob and its trailing newline
		if _, err := io.ReadFull(r, body); err != nil {
			return fmt.Errorf("git cat-file %s (%s): %w", objs[i], path, err)
		}
		if err := fn(path, body[:size]); err != nil {
			return err
		}
	}
	return nil
}

func ghReleasedAt(dir, tag string) (time.Time, error) {
	c := exec.Command("gh", "release", "view", tag, "--json", "publishedAt", "-q", ".publishedAt")
	c.Dir = dir
	out, err := c.Output()
	if err != nil {
		return time.Time{}, fmt.Errorf("gh release view %s: %w", tag, err)
	}
	return time.Parse(time.RFC3339, strings.TrimSpace(string(out)))
}

// sameRepo says whether two git URLs name one repository (case, scheme and a trailing .git aside).
func sameRepo(a, b string) bool {
	norm := func(u string) string {
		u = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(u), ".git"))
		u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "ssh://")
		u = strings.TrimPrefix(u, "git@")
		return strings.Replace(u, ":", "/", 1)
	}
	return a != "" && norm(a) == norm(b)
}

func git(dir string, env []string, stdin *bytes.Buffer, args ...string) (string, error) {
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	if stdin != nil {
		c.Stdin = stdin
	}
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}
