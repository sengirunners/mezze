package search

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// Walk is the Go driver. In a git repository it searches what git lists
// (git.go), so git's own rules apply; elsewhere, or without git, it walks
// the directory with a small matcher (ignore.go). Either way it never
// follows a symlink and skips hidden entries and binaries.
type Walk struct{}

func (Walk) Name() string { return "walk" }

const (
	binarySniff = 8 << 10  // bytes looked at for a NUL
	maxFileSize = 50 << 20 // larger files are skipped
	maxLine     = 1 << 20  // longer lines are skipped
)

var errStop = errors.New("stop")

func (w Walk) Search(ctx context.Context, q Query) (orchapi.SearchResult, error) {
	q.defaults()
	c := newCollector(q, w.Name())
	var re *regexp.Regexp
	if q.Regex {
		var err error
		if re, err = compileRegex(q.Text); err != nil {
			return orchapi.SearchResult{}, api.Errorf(api.CodeBadRequest, "invalid regex: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, q.Timeout)
	defer cancel()
	find := func(line string) int {
		if re != nil {
			if loc := re.FindStringIndex(line); loc != nil {
				return loc[0]
			}
			return -1
		}
		return strings.Index(line, q.Text)
	}
	roots := slices.Clone(q.Roots)
	slices.SortFunc(roots, comparePaths) // the answer's order, so a limit keeps its first matches
	for _, root := range roots {
		if !searchRoot(ctx, c, q, root, find) {
			break
		}
	}
	return c.result(errors.Is(ctx.Err(), context.DeadlineExceeded)), nil
}

// searchRoot searches one root: what git lists in a repository, else a walk
// with the matcher (ignore.go). false: stop the search.
func searchRoot(ctx context.Context, c *collector, q Query, root string, find func(string) int) bool {
	if inRepo(root) {
		if files, cut, ok := repoFiles(ctx, root, q.Timeout/2); ok {
			for _, f := range files {
				if ctx.Err() != nil || !searchFile(ctx, c, f, find) {
					return false
				}
			}
			c.trunc.Files = c.trunc.Files || cut
			return true
		}
	}
	ig := &ignores{ctx: ctx, fold: foldIgnoreCase}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return errStop
		}
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if path == root {
			ig.load(root)
			return nil
		}
		name := d.Name()
		if strings.HasPrefix(name, ".") || d.Type()&fs.ModeSymlink != 0 {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil // hidden, or a symlink: never followed
		}
		if ig.ignored(path, d.IsDir()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			ig.load(path)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if !searchFile(ctx, c, path, find) {
			return errStop
		}
		return nil
	})
	return !errors.Is(err, errStop)
}

// compileRegex compiles a regex=1 query (Go's RE2).
func compileRegex(s string) (*regexp.Regexp, error) { return regexp.Compile(s) }

// searchFile reads one file line by line; false: stop the search.
func searchFile(ctx context.Context, c *collector, path string, find func(string) int) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || fi.Size() > maxFileSize {
		return true
	}
	br := bufio.NewReaderSize(f, 64<<10)
	if head, _ := br.Peek(binarySniff); bytes.IndexByte(head, 0) >= 0 {
		return true // binary
	}
	for n := 1; ; n++ {
		if n%1024 == 0 && ctx.Err() != nil {
			return false
		}
		line, err := readTextLine(br)
		if line != nil {
			if i := find(string(line)); i >= 0 && !c.add(path, n, string(line), i) {
				return false
			}
		}
		if err != nil {
			return true // the end, or an unreadable rest: on to the next file
		}
	}
}

// readTextLine reads one line (without its newline); a line over maxLine
// bytes is skipped (nil).
func readTextLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	over := false
	for {
		chunk, isPrefix, err := br.ReadLine()
		if !over {
			if len(buf)+len(chunk) > maxLine {
				over, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if err != nil {
			if over || len(buf) == 0 {
				return nil, err
			}
			return buf, nil
		}
		if !isPrefix {
			if over {
				return nil, nil
			}
			if buf == nil {
				buf = []byte{}
			}
			return buf, nil
		}
	}
}
