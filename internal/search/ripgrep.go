package search

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// Ripgrep is the driver that runs rg (argv only, never a shell).
type Ripgrep struct{ bin string }

// NewRipgrep finds rg (bin "" = on PATH); ok=false when there is none.
func NewRipgrep(bin string) (Ripgrep, bool) {
	if bin == "" {
		bin = "rg"
	}
	p, err := exec.LookPath(bin)
	if err != nil {
		return Ripgrep{}, false
	}
	return Ripgrep{bin: p}, true
}

func (Ripgrep) Name() string { return "ripgrep" }

// rgMessage is the part of rg --json output read here.
type rgMessage struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text *string `json:"text"`
		} `json:"path"`
		Lines struct {
			Text *string `json:"text"`
		} `json:"lines"`
		LineNumber int `json:"line_number"`
		Submatches []struct {
			Start int `json:"start"`
		} `json:"submatches"`
	} `json:"data"`
}

// maxJSONLine bounds one rg output line read (a match on a huge line is
// skipped rather than held in memory).
const maxJSONLine = 1 << 20

func (r Ripgrep) Search(ctx context.Context, q Query) (orchapi.SearchResult, error) {
	q.defaults()
	c := newCollector(q, r.Name())
	if len(q.Roots) == 0 {
		return c.result(false), nil
	}
	ctx, cancel := context.WithTimeout(ctx, q.Timeout)
	defer cancel()
	for _, root := range q.Roots { // one rg per root: whether it sits in a repository decides its ignore flags
		if c.full() || ctx.Err() != nil {
			break
		}
		if err := r.searchRoot(ctx, c, q, root); err != nil {
			if ctx.Err() != nil && !api.IsCode(err, api.CodeBadRequest) {
				break
			}
			return orchapi.SearchResult{}, err
		}
	}
	return c.result(errors.Is(ctx.Err(), context.DeadlineExceeded)), nil
}

// searchRoot runs rg over one root, feeding c until a limit or the end.
func (r Ripgrep) searchRoot(ctx context.Context, c *collector, q Query, root string) error {
	// In a repository, rg's default: ignore files from its top down and its
	// info/exclude. Outside one, the root's and below only, as the walker's
	// matcher reads them. Never core.excludesFile (--no-ignore-global).
	// rg searches in parallel (--sort path would make it walk on one thread,
	// about 5x slower on a large root); the collector sorts what it kept by
	// file, then line (SPEC 21).
	args := []string{"--json", "--no-config", "--no-messages", "--no-ignore-global", "--max-filesize", "50M"}
	if !inRepo(root) {
		args = append(args, "--no-require-git", "--no-ignore-parent")
	}
	if foldIgnoreCase {
		args = append(args, "--ignore-file-case-insensitive")
	}
	if !q.Regex {
		args = append(args, "--fixed-strings")
	}
	// The query is always the argument after "--": never an option.
	args = append(args, "--", q.Text, root)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, r.bin, args...)
	cmd.WaitDelay = time.Second
	cmd.Env = []string{"PATH=/usr/bin:/bin"} // no RIPGREP_CONFIG_PATH or other surprises
	var stderr limitedBuffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err() // the time ran out before rg started
		}
		return fmt.Errorf("start rg: %w", err)
	}
	br := bufio.NewReaderSize(out, 64<<10)
	read := false
	for !c.full() {
		line, err := readLine(br)
		if len(line) > 0 {
			var m rgMessage
			if json.Unmarshal(line, &m) == nil && m.Type == "match" && m.Data.Path.Text != nil && m.Data.Lines.Text != nil {
				read = true
				col := 0
				if len(m.Data.Submatches) > 0 {
					col = m.Data.Submatches[0].Start
				}
				if !c.add(*m.Data.Path.Text, m.Data.LineNumber, *m.Data.Lines.Text, col) {
					break
				}
			}
		}
		if err != nil {
			break
		}
	}
	cancel() // a limit reached: stop rg
	_, _ = io.Copy(io.Discard, out)
	err = cmd.Wait() // exit 1 = no match, 2 = an error: answered with what was found, unless the pattern was refused
	var ee *exec.ExitError
	if q.Regex && !read && ctx.Err() == nil && errors.As(err, &ee) && ee.ExitCode() == 2 {
		// ripgrep's regex syntax is not Go's: a pattern Go took that ripgrep
		// refuses would otherwise answer as "no matches". Only a regex parse
		// error says so (rg prints it whatever --no-messages says); exit 2
		// for an unreadable file is not one, and the search goes on.
		if msg, ok := refusedPattern(stderr.String()); ok {
			return api.Errorf(api.CodeBadRequest, "pattern not supported: %s", msg)
		}
	}
	return nil
}

// refusedPattern finds rg's refusal of a regex in its stderr (a parse error,
// or a compiled regex over its size limit) and returns the reason: the line
// starting "error:" (a hint may follow it), else the last line.
func refusedPattern(stderr string) (string, bool) {
	if !strings.Contains(stderr, "regex parse error") && !strings.Contains(stderr, "exceeds size limit") {
		return "", false
	}
	lines := strings.Split(strings.TrimSpace(stderr), "\n")
	for _, line := range lines {
		if msg, ok := strings.CutPrefix(strings.TrimSpace(line), "error:"); ok {
			return strings.TrimSpace(msg), true
		}
	}
	return strings.TrimPrefix(strings.TrimSpace(lines[len(lines)-1]), "rg: "), true
}

// limitedBuffer keeps the first 4 KiB written to it (rg's stderr).
type limitedBuffer struct{ b []byte }

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := 4096 - len(l.b); room > 0 {
		l.b = append(l.b, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string { return string(l.b) }

// readLine reads one line of at most maxJSONLine bytes; a longer one is
// skipped whole (returned empty).
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	over := false
	for {
		chunk, isPrefix, err := br.ReadLine()
		if !over {
			if len(buf)+len(chunk) > maxJSONLine {
				over, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if err != nil || !isPrefix {
			if over {
				return nil, err
			}
			return buf, err
		}
	}
}
