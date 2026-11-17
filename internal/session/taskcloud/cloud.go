// Package taskcloud is what a cloud task runs (SPEC 8 "Cloud tasks"): it
// hands the prompt to the provider's cloud computer through the machine's own
// CLI login (claude --cloud, codex cloud exec), follows the provider's task
// where its CLI can, and prints what it learns as JSON lines for the daemon.
// It runs inside the holder's managed group like any task child; cancelling
// the task stops this watcher, never the provider's task.
package taskcloud

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// Arg is argv[1] of the zm binary run as a cloud task's watcher.
const Arg = "__zm-task-cloud"

// DefaultEnvironment, as a project's Claude environment, sends no
// environment setting: the account's default cloud environment.
const DefaultEnvironment = "default"

// Info is one {"zm_cloud": Info} line: the provider's task as the watcher
// last saw it. The last line carries Text, the task's result text.
type Info struct {
	Agent       string `json:"agent"`
	Environment string `json:"environment"`
	ID          string `json:"id,omitempty"`
	URL         string `json:"url,omitempty"`
	// Status: submitted, pending, ready, failed or unknown.
	Status string `json:"status"`
	Text   string `json:"text,omitempty"`
}

// Line is how Info is printed.
type Line struct {
	Cloud *Info `json:"zm_cloud"`
}

// Arguments is the watcher's argv after the binary: agent (claude or
// codex), the provider environment, the provider CLI and the poll backoff.
func Arguments(agent, env, cli string, first, max time.Duration) []string {
	return []string{Arg, agent, env, cli, strconv.FormatInt(first.Milliseconds(), 10), strconv.FormatInt(max.Milliseconds(), 10)}
}

// Main runs the watcher: the prompt is stdin. 0 means the provider took the
// task (and, where zm can follow it, finished it); anything else failed.
func Main(args []string) int {
	if len(args) != 5 {
		fmt.Fprintln(os.Stderr, "zm cloud task: bad arguments")
		return 2
	}
	// A stop cancels the watch, removes the scratch, then dies by the same
	// signal: the task's outcome stays killed, not a plain failure.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	got := make(chan os.Signal, 1)
	go func() {
		if sig, ok := <-sigs; ok {
			got <- sig
			cancel()
		}
	}()
	code := run(ctx, args)
	signal.Stop(sigs)
	close(sigs)
	select {
	case sig := <-got:
		signal.Reset()
		_ = syscall.Kill(os.Getpid(), sig.(syscall.Signal))
		time.Sleep(time.Second)
	default:
	}
	return code
}

func run(ctx context.Context, args []string) int {
	first, err1 := strconv.ParseInt(args[3], 10, 64)
	max, err2 := strconv.ParseInt(args[4], 10, 64)
	if err1 != nil || err2 != nil || first <= 0 || max < first {
		fmt.Fprintln(os.Stderr, "zm cloud task: bad poll interval")
		return 2
	}
	prompt, err := io.ReadAll(io.LimitReader(os.Stdin, 1<<20))
	if err != nil {
		fmt.Fprintln(os.Stderr, "zm cloud task: read prompt:", err)
		return 2
	}
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, "zm cloud task:", err)
		return 2
	}
	// codex cloud writes an error.log into its working directory: keep it out
	// of the task's checkout, in the session's scratch (zm session gc's).
	scratch, err := os.MkdirTemp(os.Getenv("ZM_SCRATCH"), "cloud-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "zm cloud task:", err)
		return 2
	}
	defer os.RemoveAll(scratch)
	w := watcher{agent: args[0], env: args[1], cli: args[2], first: time.Duration(first) * time.Millisecond,
		max: time.Duration(max) * time.Millisecond, cwd: cwd, scratch: scratch, out: os.Stdout, run: runCLI, runTTY: runCLITerminal,
		sleep: func(d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		}}
	return w.watch(ctx, string(prompt))
}

// submitTimeout bounds the submit call; pollTimeout each status, list or
// diff call. A poll that cannot answer counts toward maxPollFailures.
const (
	submitTimeout   = 10 * time.Minute
	pollTimeout     = 2 * time.Minute
	maxPollFailures = 5
	outputCap       = 64 << 10
	diffCap         = 48 << 10
)

type watcher struct {
	agent, env, cli string
	first, max      time.Duration
	// cwd is the task's checkout; scratch a private directory removed after.
	cwd, scratch string
	out          io.Writer
	sleep        func(time.Duration)
	// run runs argv in dir.
	run func(ctx context.Context, timeout time.Duration, dir string, argv ...string) (int, string)
	// runTTY is run with a terminal as the CLI's stdin and stdout.
	runTTY func(ctx context.Context, timeout time.Duration, dir string, argv ...string) (int, string)
}

func (w watcher) emit(i Info) {
	i.Agent, i.Environment = w.agent, w.env
	b, _ := json.Marshal(Line{Cloud: &i})
	fmt.Fprintf(w.out, "%s\n", b)
}

func (w watcher) fail(i Info, format string, a ...any) int {
	i.Status, i.Text = "failed", fmt.Sprintf(format, a...)
	w.emit(i)
	return 1
}

// watch passes prompt on as given: a flag-like prompt rides after = or --.
func (w watcher) watch(ctx context.Context, prompt string) int {
	if strings.TrimSpace(prompt) == "" {
		return w.fail(Info{}, "no prompt")
	}
	switch w.agent {
	case "claude":
		return w.claude(ctx, prompt)
	case "codex":
		return w.codex(ctx, prompt)
	}
	return w.fail(Info{}, "unknown cloud agent %q", w.agent)
}

var (
	// What claude --cloud prints for a new session (claude 2.1.326):
	//   Created cloud session: <title>
	//   View: https://claude.ai/code/session_...?from=cli&m=0
	//   Resume with: claude --teleport session_...
	// A follow-up send prints "Session ID:" instead; it is read too. Terminal
	// colour and cursor codes are dropped first (a terminal gets them).
	claudeIDLine  = regexp.MustCompile(`(?m)^[ \t]*Session ID:[ \t]*(session_[A-Za-z0-9_-]+)`)
	claudeResume  = regexp.MustCompile(`(?m)^[ \t]*Resume with:[ \t]*claude --teleport[ \t]+(session_[A-Za-z0-9_-]+)`)
	claudeURLLine = regexp.MustCompile(`(?m)^[ \t]*View:[ \t]*(https?://\S+)`)
	claudeSession = regexp.MustCompile(`/code/(session_[A-Za-z0-9_-]+)`)
	ansiCodes     = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[()#%][ -~]|[@-Z\\-_])`)
)

// settingsForEnvironment is claude's --settings value that selects a cloud
// environment: remote.defaultEnvironmentId holds an Anthropic-hosted env_ id
// or a self-hosted ccpool_ id alike, where --environment takes only the
// latter. Encoded, never joined: the id is data.
func settingsForEnvironment(env string) string {
	b, _ := json.Marshal(map[string]map[string]string{"remote": {"defaultEnvironmentId": env}})
	return string(b)
}

// claudeLaunch reads a session id and its link from claude --cloud's output.
func claudeLaunch(out string) (id, url string) {
	out = plainTerminal(out)
	if m := claudeURLLine.FindStringSubmatch(out); m != nil {
		url = m[1]
	}
	// The Resume line and the link name the session itself; a Session ID line
	// is a follow-up's and comes last.
	if m := claudeResume.FindStringSubmatch(out); m != nil {
		return m[1], url
	}
	if m := claudeSession.FindStringSubmatch(url); m != nil {
		return m[1], url
	}
	if m := claudeIDLine.FindStringSubmatch(out); m != nil {
		return m[1], url
	}
	return "", url
}

// plainTerminal is a terminal's output as lines: escape codes dropped, a
// lone CR (a redraw) a line break.
func plainTerminal(s string) string {
	s = ansiCodes.ReplaceAllString(s, "")
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
}

// claude creates the cloud session (claude --cloud prints the session's link
// and exits). Claude Code has no command that reports a cloud session's
// state without attaching to it, so the task ends here. The CLI gets a
// terminal (it is built for one), and --permission-mode auto so an
// unattended session does not stop on a prompt nobody answers.
func (w watcher) claude(ctx context.Context, prompt string) int {
	argv := []string{w.cli}
	if w.env != DefaultEnvironment {
		argv = append(argv, "--settings", settingsForEnvironment(w.env))
	}
	argv = append(argv, "--cloud="+prompt, "--permission-mode", "auto")
	code, out := w.runTTY(ctx, submitTimeout, w.cwd, argv...)
	id, url := claudeLaunch(out)
	i := Info{ID: id, URL: url}
	if code != 0 || i.ID == "" {
		return w.fail(i, "claude --cloud exited %d without a session id:\n%s", code, tail(plainTerminal(out), 4000))
	}
	i.Status = "submitted"
	w.emit(i)
	i.Text = fmt.Sprintf("Claude cloud session %s started: %s\nzm cannot follow a Claude cloud session (Claude Code has no status command); "+
		"open the link, or run `claude --teleport %s` in the repo to bring it here.", i.ID, i.URL, i.ID)
	w.emit(i)
	return 0
}

var (
	codexID = regexp.MustCompile(`\btask_[A-Za-z0-9_-]+`)
	anyURL  = regexp.MustCompile(`https?://\S+`)
	// statusTag is codex cloud status's leading [STATE]; any other text
	// (a CLI or transport error) says nothing about the task.
	statusTag = regexp.MustCompile(`(?im)^\s*\[(ready|applied|pending|running|in[ _]progress|queued|error|failed|cancell?ed)\]`)
	notReady  = map[string]string{"pending": "pending", "running": "pending", "in progress": "pending", "in_progress": "pending", "queued": "pending",
		"ready": "ready", "applied": "ready", "error": "failed", "failed": "failed", "cancelled": "failed", "canceled": "failed"}
)

// codex submits with codex cloud exec, then asks codex cloud status with
// backoff (minutes: the provider has no webhook) until the task is ready or
// failed, and ends with its diff. Every codex call runs in scratch, so the
// checkout's branch (codex's own default) is passed explicitly.
func (w watcher) codex(ctx context.Context, prompt string) int {
	argv := []string{w.cli, "cloud", "exec", "--env", w.env}
	if code, b := w.run(ctx, pollTimeout, w.cwd, "git", "rev-parse", "--abbrev-ref", "HEAD"); code == 0 && strings.TrimSpace(b) != "HEAD" && strings.TrimSpace(b) != "" {
		argv = append(argv, "--branch="+strings.TrimSpace(b))
	}
	code, out := w.run(ctx, submitTimeout, w.scratch, append(argv, "--", prompt)...)
	var i Info
	if u := anyURL.FindString(out); u != "" {
		i.URL = strings.TrimRight(u, ".,)")
	}
	if id := codexID.FindString(out); id != "" {
		i.ID = id
	} else if i.URL != "" {
		i.ID = i.URL[strings.LastIndexByte(i.URL, '/')+1:]
	}
	if code != 0 || i.ID == "" {
		return w.fail(i, "codex cloud exec exited %d without a task:\n%s", code, tail(out, 4000))
	}
	i.Status = "submitted"
	w.emit(i)
	delay, failures, last := w.first, 0, ""
	for {
		w.sleep(delay)
		if ctx.Err() != nil {
			i.Status, i.Text = "unknown", fmt.Sprintf("zm stopped watching Codex cloud task %s; the provider's task goes on: %s", i.ID, i.URL)
			w.emit(i)
			return 1
		}
		delay = min(delay*2, w.max)
		status, detail, ok := w.codexStatus(ctx, i.ID)
		if !ok {
			if failures++; failures >= maxPollFailures {
				i.Status = "unknown"
				i.Text = fmt.Sprintf("zm lost track of Codex cloud task %s after %d failed status checks: %s\nlast answer:\n%s", i.ID, failures, i.URL, tail(detail, 4000))
				w.emit(i)
				return 1
			}
			continue
		}
		failures = 0
		switch status {
		case "ready":
			_, diff := w.run(ctx, pollTimeout, w.scratch, w.cli, "cloud", "diff", i.ID)
			i.Status = "ready"
			i.Text = fmt.Sprintf("Codex cloud task %s is ready: %s\nApply it here with `codex cloud apply %s`.\n\n%s", i.ID, i.URL, i.ID, head(diff, diffCap))
			w.emit(i)
			return 0
		case "failed":
			return w.fail(i, "Codex cloud task %s failed: %s\n%s", i.ID, i.URL, tail(detail, 4000))
		}
		if status != last {
			i.Status = status
			w.emit(i)
			last = status
		}
	}
}

// codexStatus is the task's state: ready, pending or failed; ok is false
// when neither status nor list answered.
func (w watcher) codexStatus(ctx context.Context, id string) (status, detail string, ok bool) {
	code, out := w.run(ctx, pollTimeout, w.scratch, w.cli, "cloud", "status", id)
	if s := w.codexListed(ctx, id); s != "" {
		return s, out, true
	}
	if m := statusTag.FindStringSubmatch(out); m != nil {
		return notReady[strings.ToLower(m[1])], out, true
	}
	if code == 0 {
		return "ready", out, true // status exits 0 only for a ready task
	}
	return "", out, false
}

// codexListed is the task's state in codex cloud list --json, "" when the
// list does not answer or no longer shows it.
func (w watcher) codexListed(ctx context.Context, id string) string {
	code, out := w.run(ctx, pollTimeout, w.scratch, w.cli, "cloud", "list", "--json", "--env", w.env, "--limit", "20")
	if code != 0 {
		return ""
	}
	var l struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(out)), &l) != nil {
		return ""
	}
	for _, t := range l.Tasks {
		if t["id"] != id {
			continue
		}
		st, _ := t["status"].(string)
		if m, ok := t["status"].(map[string]any); ok {
			st, _ = m["state"].(string)
		}
		return notReady[strings.ToLower(st)]
	}
	return ""
}

// runCLI runs argv in dir with stdin closed and answers its exit code and
// its combined output (capped); -1 when it could not run or timed out.
func runCLI(ctx context.Context, timeout time.Duration, dir string, argv ...string) (int, string) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.WaitDelay = 10 * time.Second // a grandchild holding the pipe does not outlast the timeout
	var b capped
	cmd.Stdout, cmd.Stderr = &b, &b
	err := cmd.Run()
	if err == nil {
		return 0, b.String()
	}
	if ee, ok := err.(*exec.ExitError); ok && ctx.Err() == nil {
		return ee.ExitCode(), b.String()
	}
	return -1, strings.TrimSpace(b.String() + "\n" + err.Error())
}

type capped struct{ bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := outputCap - c.Len(); room > 0 {
		c.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "\n… (cut; the whole diff: codex cloud diff)"
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	s = s[len(s)-n:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return "…" + s
}
