package search

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
)

// The walker's small matcher, for where git cannot answer (repoFiles): a
// directory outside any repository, no git on PATH, or git failing. It reads
// .gitignore and .ignore from the searched directory down, never above it.
// Blank lines and # comments; "!" negates; a trailing "/" matches directories
// only; a pattern with a "/" (other than a trailing one) is anchored to its
// file's directory, else it matches a name at any depth; "*", "?", "[...]"
// and "**" glob ("dir/**" matches what is inside dir, not dir itself); a
// backslash escapes the next character, and trailing spaces are dropped
// unless escaped ("a\ "). The last matching pattern wins, and an ignored
// directory is not entered (so a file under it cannot be re-included, as in
// git).

// foldIgnoreCase: on macOS patterns match case-insensitively, as git does
// there; ripgrep gets --ignore-file-case-insensitive to match.
const foldIgnoreCase = runtime.GOOS == "darwin"

type ignorePat struct {
	dir      string // the absolute directory the pattern came from
	re       *regexp.Regexp
	neg      bool
	dirOnly  bool
	anchored bool
}

type ignores struct {
	ctx  context.Context // a hostile file must not outlast the search's time
	fold bool            // match case-insensitively (foldIgnoreCase)
	pats []ignorePat
}

// maxIgnoreFile bounds what is read of one ignore file.
const maxIgnoreFile = 1 << 20

// inRepo reports dir at or under a directory with a .git directory or file.
func inRepo(dir string) bool {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		if d == filepath.Dir(d) {
			return false
		}
	}
}

// readIgnoreFile reads path when it is a regular file, through a symlink or
// not. A FIFO or a device never is: O_NONBLOCK keeps the open of a FIFO from
// hanging, and the check is on the opened file. At most maxIgnoreFile bytes
// are read; a longer file keeps its whole lines only, so a pattern cut in
// two never matches something else.
func readIgnoreFile(path string) ([]byte, bool) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, maxIgnoreFile+1))
	if err != nil {
		return nil, false
	}
	if len(b) > maxIgnoreFile {
		b = b[:bytes.LastIndexByte(b[:maxIgnoreFile], '\n')+1]
	}
	return b, true
}

// load reads dir's ignore files.
func (ig *ignores) load(dir string) {
	ig.readFile(filepath.Join(dir, ".gitignore"), dir)
	ig.readFile(filepath.Join(dir, ".ignore"), dir)
}

func (ig *ignores) readFile(path, dir string) {
	b, ok := readIgnoreFile(path)
	if !ok {
		return
	}
	for n, line := range strings.Split(string(b), "\n") {
		if n%256 == 0 && ig.ctx != nil && ig.ctx.Err() != nil {
			return
		}
		ig.add(line, dir)
	}
}

func (ig *ignores) add(line, dir string) {
	line = trimTrailingSpaces(strings.TrimSuffix(line, "\r"))
	if line == "" || strings.HasPrefix(line, "#") {
		return
	}
	p := ignorePat{dir: dir}
	if strings.HasPrefix(line, "!") {
		p.neg, line = true, line[1:]
	} // "\#" and "\!" are literal: globRegexp unescapes them
	if strings.HasSuffix(line, "/") {
		p.dirOnly, line = true, strings.TrimSuffix(line, "/")
	}
	if strings.Contains(line, "/") {
		p.anchored, line = true, strings.TrimPrefix(line, "/")
	}
	if line == "" {
		return
	}
	flags := ""
	if ig.fold {
		flags = "(?i)"
	}
	re, err := regexp.Compile(flags + "^" + globRegexp(line) + "$")
	if err != nil {
		return
	}
	p.re = re
	ig.pats = append(ig.pats, p)
}

// trimTrailingSpaces drops trailing spaces, keeping one escaped by a
// backslash (as git does; tabs are kept).
func trimTrailingSpaces(line string) string {
	end := 0 // just past the last character to keep
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line):
			i++
			end = i + 1
		case line[i] != ' ':
			end = i + 1
		}
	}
	return line[:end]
}

// ignored reports whether path (absolute) is ignored.
func (ig *ignores) ignored(path string, isDir bool) bool {
	out := false
	for _, p := range ig.pats {
		rel, err := filepath.Rel(p.dir, path)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			continue
		}
		sub := filepath.ToSlash(rel)
		target := sub
		if !p.anchored {
			target = sub[strings.LastIndexByte(sub, '/')+1:]
		}
		if (p.dirOnly && !isDir) || !p.re.MatchString(target) {
			continue
		}
		out = !p.neg
	}
	return out
}

// globRegexp turns a gitignore glob into a regexp body.
func globRegexp(g string) string {
	var b strings.Builder
	for i := 0; i < len(g); i++ {
		c := g[i]
		switch {
		case strings.HasPrefix(g[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(g[i:], "/**") && i+3 == len(g):
			b.WriteString("/.*") // inside the directory, not the directory
			i += 2
		case strings.HasPrefix(g[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '[':
			j := strings.IndexByte(g[i+1:], ']')
			if j < 0 {
				b.WriteString(`\[`)
				continue
			}
			class := g[i+1 : i+1+j]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + strings.ReplaceAll(class, `\`, `\\`) + "]")
			i += j + 1
		case c == '\\' && i+1 < len(g):
			i++
			b.WriteString(regexp.QuoteMeta(string(g[i])))
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}
