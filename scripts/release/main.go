// Command release builds the release tarballs (SPEC 17.1): one static
// zm (plus bd-zm and the helper commands in Commands) per OS and architecture, packed as
// dist/zm_<version>_<os>_<arch>.tar.gz, and dist/SHA256SUMS in the
// format `shasum -a 256 -c` reads. It publishes nothing.
//
//	go run ./scripts/release -version v0.1.0 -commit abc1234 [-out dist] [-targets darwin/arm64,linux/amd64]
//
// Builds are CGO_ENABLED=0 and -trimpath; archive entries are sorted and
// carry one fixed time (SOURCE_DATE_EPOCH, else the commit's time), so the
// same commit and Go version give byte-identical tarballs.
package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Targets are the platforms a release ships for (zm is macOS and Linux).
var Targets = []string{"darwin/arm64", "darwin/amd64", "linux/amd64", "linux/arm64"}

// Commands are the binaries in every tarball. zm-run-farm (boxes on team
// machines over zm exec) is pure Go and runs anywhere, so every tarball
// carries it: without a run-target driver a release cannot lend a machine.
// The local driver (credentialed jobs on the machine itself, such as the
// automations home's) is pure Go too. Potluck's programs ship under their new
// names (potluck-conformance, potluck-driver-*: the daemon looks for those
// first) and their old ones (zm-run-*), one program each.
var Commands = []string{"zm", "bd-zm", "zm-work-conformance", "potluck-conformance", "zm-run-conformance", "zm-run-farm", "potluck-driver-local", "zm-run-local", "zm-connect"}

// TargetCommands are the extra binaries one platform ships: the tart driver
// (potluck-driver-tart, and zm-run-tart, its old name) is the driver zm host share binds on every layer, Tart VMs on Apple-silicon
// Macs (none on Intel) and vm=oci (Kata or gVisor) on Linux.
var TargetCommands = map[string][]string{"darwin/arm64": {"potluck-driver-tart", "zm-run-tart"}, "linux/amd64": {"potluck-driver-tart", "zm-run-tart"}, "linux/arm64": {"potluck-driver-tart", "zm-run-tart"}}

// PotluckCommands live in sengirunners/potluck: they are built
// from that module at the version go.mod requires, so a release still ships
// them next to zm.
var PotluckCommands = map[string]bool{"potluck-conformance": true, "potluck-driver-local": true, "potluck-driver-tart": true, "zm-run-conformance": true, "zm-run-local": true, "zm-run-tart": true}

// commandPkg is the package go build takes for command c.
func commandPkg(c string) string {
	if PotluckCommands[c] {
		return "github.com/sengirunners/potluck/cmd/" + c
	}
	return "./cmd/" + c
}

// commandsFor is the list of binaries in the tarball for goos/goarch.
func commandsFor(goos, goarch string) []string {
	return append(append([]string(nil), Commands...), TargetCommands[goos+"/"+goarch]...)
}

// Docs are copied next to the binaries.
var Docs = []string{"README.md", "SPEC.md", "API.md"}

// potluckPkg holds kit/api, whose Version and Commit the binaries are stamped with.
const potluckPkg = "github.com/sengirunners/potluck"

func main() {
	version := flag.String("version", "v0.0.0-dev", "version stamped into the binaries and file names")
	commit := flag.String("commit", "unknown", "commit stamped into the binaries")
	out := flag.String("out", "dist", "output directory (its old tarballs, SHA256SUMS and .build are replaced; nothing else is touched)")
	targets := flag.String("targets", strings.Join(Targets, ","), "comma list of os/arch")
	repo := flag.String("repo", os.Getenv("GITHUB_REPOSITORY"), "the GitHub repository (owner/name) releasing these: zm reads its releases after the public repo's")
	flag.Parse()
	if err := run(*version, *commit, *repo, *out, strings.Split(*targets, ",")); err != nil {
		fmt.Fprintln(os.Stderr, "release:", err)
		os.Exit(1)
	}
}

func run(version, commit, repo, out string, targets []string) error {
	if strings.ContainsAny(repo, " \t'\"") {
		return fmt.Errorf("bad repo %q", repo)
	}
	if strings.ContainsAny(version, "/ \t") {
		return fmt.Errorf("bad version %q", version)
	}
	mtime, err := sourceTime()
	if err != nil {
		return err
	}
	if err := clean(out); err != nil {
		return err
	}
	build := filepath.Join(out, ".build")
	if err := os.MkdirAll(build, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(build)
	ldflags := fmt.Sprintf("-s -w -X %s/kit/api.Version=%s -X %s/kit/api.Commit=%s", potluckPkg, version, potluckPkg, commit)
	if repo != "" {
		ldflags += " -X main.buildRepo=" + repo
	}
	var sums []string
	for _, t := range targets {
		goos, goarch, ok := strings.Cut(strings.TrimSpace(t), "/")
		if !ok || goos == "" || goarch == "" {
			return fmt.Errorf("bad target %q (want os/arch)", t)
		}
		name := fmt.Sprintf("zm_%s_%s_%s", version, goos, goarch)
		dir := filepath.Join(build, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		args := []string{"build", "-trimpath", "-ldflags", ldflags, "-o", dir + "/"}
		for _, c := range commandsFor(goos, goarch) {
			args = append(args, commandPkg(c))
		}
		cmd := exec.Command("go", args...)
		cmd.Env = append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0")
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("build %s: %w", t, err)
		}
		for _, d := range Docs {
			if err := copyFile(d, filepath.Join(dir, d)); err != nil {
				return err
			}
		}
		tarball := filepath.Join(out, name+".tar.gz")
		if err := pack(tarball, dir, name, mtime); err != nil {
			return err
		}
		sum, err := sha256File(tarball)
		if err != nil {
			return err
		}
		sums = append(sums, sum+"  "+filepath.Base(tarball))
		fmt.Println(sum, tarball)
	}
	sort.Strings(sums)
	return os.WriteFile(filepath.Join(out, "SHA256SUMS"), []byte(strings.Join(sums, "\n")+"\n"), 0o644)
}

// sourceTime is SOURCE_DATE_EPOCH, else HEAD's commit time, else now.
// clean removes only what a release writes into out (old tarballs,
// SHA256SUMS, the .build scratch directory), never out itself: -out . or
// any directory with other files in it keeps them.
func clean(out string) error {
	old, err := filepath.Glob(filepath.Join(out, "zm_*.tar.gz"))
	if err != nil {
		return err
	}
	for _, p := range append(old, filepath.Join(out, "SHA256SUMS"), filepath.Join(out, ".build")) {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

func sourceTime() (time.Time, error) {
	if s := os.Getenv("SOURCE_DATE_EPOCH"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("SOURCE_DATE_EPOCH: %w", err)
		}
		return time.Unix(n, 0).UTC(), nil
	}
	if b, err := exec.Command("git", "log", "-1", "--format=%ct").Output(); err == nil {
		if n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64); err == nil {
			return time.Unix(n, 0).UTC(), nil
		}
	}
	return time.Now().UTC().Truncate(time.Second), nil
}

// pack writes dir as a gzip'd tar whose entries live under prefix/, sorted,
// owned by root, with one fixed time.
func pack(path, dir, prefix string, mtime time.Time) error {
	var files []string
	if err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			files = append(files, p)
		}
		return err
	}); err != nil {
		return err
	}
	sort.Strings(files)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewWriterLevel(f, gzip.BestCompression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: prefix + "/", Mode: 0o755, ModTime: mtime, Uname: "root", Gname: "root", Format: tar.FormatPAX}); err != nil {
		return err
	}
	for _, p := range files {
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		mode := int64(0o644)
		if fi.Mode()&0o111 != 0 {
			mode = 0o755
		}
		h := &tar.Header{Typeflag: tar.TypeReg, Name: prefix + "/" + filepath.ToSlash(rel), Mode: mode, Size: fi.Size(),
			ModTime: mtime, Uname: "root", Gname: "root", Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, src)
		src.Close()
		if err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	return f.Close()
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

func sha256File(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
