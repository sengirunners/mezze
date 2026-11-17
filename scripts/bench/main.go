// Command bench measures zm's latency end to end, process start included,
// against a real daemon in a throwaway ZM_HOME, and fails when a p95 is
// over its budget. Run it through scripts/bench.sh (make bench), which builds
// zm and gives it a clean environment.
//
//	go run ./scripts/bench -zm bin/zm [-n 40] [-budgets=false] [-only hook,ls] [-out bench.json]
//	go run ./scripts/bench compare [-base main.json] bench.json   markdown for the job summary
//	go run ./scripts/bench tighten base-ceilings.json             the ceilings only tighten
//	go run ./scripts/bench repeat [-warn] -prev main.json bench.json   red only on the same row over twice
//	go run ./scripts/bench calibrate -class zm-bench run*.json      propose a runner class's ceilings
//	go run ./scripts/bench probe bin/probe                           runner/probe-late, after the rows
//
// The ceilings are scripts/ceilings/ceilings.json, one file with the load
// test's. A row over its ceiling is measured twice more before it fails. In
// CI (bench.yml) a run over them only warns until bench repeat says otherwise.
//
// It never touches ~/.zm or real dotfiles: HOME and ZM_HOME point into
// a temp dir, and every process it starts is killed before it exits.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/creack/pty"

	"github.com/sengirunners/mezze/internal/work/fake"
	"github.com/sengirunners/mezze/scripts/ceilings"
	"github.com/sengirunners/potluck/kit/authz"
)

var order = []string{"version", "hook-prompt", "hook-stop", "hook-down", "notice", "inbox", "send", "ls", "ls-tty",
	"leases", "project-why", "events-first", "mcp-tools", "mcp-down", "daemon-ready", "zero-start",
	"api-notice", "api-inbox", "api-sessions", "api-leases", "api-state", "api-hook", "api-guest",
	"work-ls", "work-ls-guest", "work-search", "work-show", "who", "work-sources", "work-rollup",
	"issue-list", "issue-list-deps", "issue-ready", "issue-show", "issue-search", "issue-substr", "issue-rank", "issue-rank-all",
	"issue-list-area", "issue-search-area", "issue-create", "issue-update", "issue-comment", "issue-claim", "issue-close",
	"replica-ls", "replica-search", "replica-show", "replica-rollup", "replica-comment", "replica-status", "replica-link",
	"team-claim", "team-release", "team-create"}

// trackerSize is the size of the fake tracker and of the generated
// native store (a live tracker).
const trackerSize = 11512

type env struct {
	bin, tmp, home, repo string
	session, runtimeID   string
	base                 []string
	daemon               *exec.Cmd
	issueDaemon          *exec.Cmd // the issue-* rows' own daemon (native.go)
	issueHome            string
	hc                   *http.Client
	scale                int
	token                string // a guest of bench
	areaToken            string // a guest of bench/lbl (a label area)
	labelled             string // a tracker item in bench/lbl
	ceilings             map[string]ceilings.Row
	replica              bool       // the replica rows run (-replica)
	mailLimits           bool       // the projects set their own [mail_limits] (-mail-limits; a build before them refuses the key)
	want                 []string   // the rows this run measures
	native               *nativeEnv // the issue-* rows' store (native.go)
	team                 *teamEnv   // the replica-* and team-* rows' daemons (replica.go)
	probe                string     // an empty Go binary: runner/probe, no ceiling; marks a contended Sengi run (repeat.go)
}

func main() {
	if code, ok := fake.Main(); ok { // this binary is also the fake tracker adapter
		os.Exit(code)
	}
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		os.Exit(subcommand(os.Args[1:]))
	}
	bin := flag.String("zm", "zm", "the zm binary to measure")
	n := flag.Int("n", 40, "runs per measurement (daemon-ready uses n/4)")
	check := flag.Bool("budgets", true, "exit 1 when a p95 is over its ceiling (scripts/ceilings/ceilings.json)")
	only := flag.String("only", "", "comma list of measurements (default: all)")
	asJSON := flag.Bool("json", false, "print results as JSON")
	out := flag.String("out", "", "also write the results as JSON here")
	mailLimits := flag.Bool("mail-limits", true, "the bench projects raise their own [mail_limits] (SPEC 9): off for a -zm build from before send limits, which refuses the key")
	replica := flag.Bool("replica", true, "run the replica-* and team-* rows (in this process: not of -zm)")
	scale := flag.Int("scale", 5000, "mail rows seeded over the API (plus 60 sessions and 40 leases)")
	keep := flag.Bool("keep", false, "keep the temp ZM_HOME (printed) for a look afterwards")
	probe := flag.String("probe", "", "an empty Go binary (scripts/bench/probe): times this runner's process start as runner/probe (no ceiling; on Sengi it marks a contended run)")
	flag.Parse()
	abs, err := exec.LookPath(*bin)
	if err == nil {
		abs, err = filepath.Abs(abs)
	}
	if err != nil {
		fatal(err)
	}
	// BENCH_LABEL (bench.yml): a calibrated class's ceilings decide which rows are retried (class.go).
	c, err := benchCeilings(os.Getenv("BENCH_LABEL"))
	if err != nil {
		fatal(err)
	}
	e := &env{bin: abs, scale: *scale, ceilings: c, replica: *replica, mailLimits: *mailLimits, probe: *probe}
	code := run(e, *n, *check, *only, *asJSON, *out)
	if *keep {
		e.stopDaemon()
		fmt.Fprintln(os.Stderr, "kept", e.tmp)
	} else {
		e.cleanup()
	}
	os.Exit(code)
}

type result struct {
	Name   string  `json:"name"`
	N      int     `json:"n"`
	P50    float64 `json:"p50_ms"`
	P95    float64 `json:"p95_ms"`
	Max    float64 `json:"max_ms"`
	Budget float64 `json:"budget_ms"`
	OK     bool    `json:"ok"`
	Tries  int     `json:"tries"`
	// First is the first try's p95. P95 is the best try, which the ceiling
	// judges; runs compare on First, or a retried row would look faster.
	First float64 `json:"first_p95_ms"`
}

// retries is how many more times a row over its ceiling is measured: one
// slow spell on a shared VM passes on the next try, a real regression does
// not.
const retries = 2

func run(e *env, n int, check bool, only string, asJSON bool, out string) int {
	e.want = order
	if only != "" {
		e.want = strings.Split(only, ",")
	}
	if err := e.setup(); err != nil {
		fmt.Fprintln(os.Stderr, "bench setup:", err)
		return 2
	}
	// What a commit that waits on the disk costs on this runner: the
	// replica's outbox commits do (synchronous FULL), so its write rows and
	// team-claim move with it from one runner to another.
	if d, err := fsyncP50(e.tmp); err == nil {
		fmt.Printf("disk: fsync p50 %.2f ms (the replica's outbox commits wait on one)\n", float64(d)/float64(time.Millisecond))
	}
	var results []result
	var over []string
	if e.probe != "" {
		r, err := timeProbe(probeRow, e.probe, 3*n)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bench %s: %v\n", probeRow, err)
			return 2
		}
		results = append(results, r)
		if !asJSON {
			fmt.Printf("%-17s n=%-3d p50 %7.1f ms  p95 %7.1f ms  max %7.1f ms  no ceiling (this runner's process start; contended on Sengi above %g)\n",
				r.Name, r.N, r.P50, r.P95, r.Max, contendedP95)
		}
	}
	for _, name := range e.want {
		f, ok := e.measures()[name]
		if !ok {
			fmt.Fprintln(os.Stderr, "bench: unknown measurement", name)
			return 2
		}
		c, ok := e.ceilings[name]
		if !ok {
			fmt.Fprintf(os.Stderr, "bench: %s has no ceiling: add it to %s\n", name, ceilings.Path)
			return 2
		}
		if teamRow(name) && e.team == nil {
			fmt.Printf("%-17s skipped: -replica=false\n", name)
			continue
		}
		runs := n
		switch {
		case name == "daemon-ready" || name == "zero-start":
			runs = max(5, n/4)
		case nativeRow(name) || teamRow(name):
			runs = 5 * n // cheap, and on the bar: a steadier p95
		}
		r, err := measure(name, runs, c.P95, check, f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "bench %s: %v\n", name, err)
			return 2
		}
		if check && !r.OK {
			over = append(over, fmt.Sprintf("  %s: p95 %.1f ms over its ceiling %g ms, %d tries", r.Name, r.P95, r.Budget, r.Tries))
		}
		results = append(results, r)
		if !asJSON {
			mark := "ok"
			if !r.OK {
				mark = "OVER"
			}
			fmt.Printf("%-17s n=%-3d p50 %7.1f ms  p95 %7.1f ms  max %7.1f ms  ceiling %6.1f ms  %s\n",
				r.Name, r.N, r.P50, r.P95, r.Max, r.Budget, mark)
		}
	}
	if asJSON {
		_ = json.NewEncoder(os.Stdout).Encode(results)
	}
	if out != "" {
		b, err := json.MarshalIndent(results, "", " ")
		if err == nil {
			err = os.WriteFile(out, b, 0o644)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "bench:", err)
			return 2
		}
	}
	if len(over) > 0 {
		fmt.Fprintf(os.Stderr, "bench: over a ceiling:\n%s\n%s\n", strings.Join(over, "\n"), overFix)
		return 1
	}
	return 0
}

// timeProbe times n starts of the empty binary bin as row name: no ceiling,
// never over, never retried; on Sengi the worse probe marks a contended run
// (repeat.go). runner/probe is timed before the rows, runner/probe-late by
// bench probe after the last timing step.
func timeProbe(name, bin string, n int) (result, error) {
	r, err := measure(name, n, 0, false, func() (time.Duration, error) {
		t := time.Now()
		err := exec.Command(bin).Run()
		return time.Since(t), err
	})
	r.OK = true
	return r, err
}

// probeCmd is `bench probe`: runner/probe-late, as a one-row bench.json on w
// for bench.yml to add after its last timing step: a VM that slows
// after the first probe is contended too.
func probeCmd(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	n := fs.Int("n", 120, "starts to time (runner/probe's: 3 x make bench's 40)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: bench probe [-n 120] <empty Go binary (scripts/bench/probe)>")
	}
	r, err := timeProbe(lateProbeRow, fs.Arg(0), *n)
	if err != nil {
		return fmt.Errorf("%s: %w", lateProbeRow, err)
	}
	return json.NewEncoder(w).Encode([]result{r})
}

// overFix is what to do about a row over its ceiling.
const overFix = "Fix: find what made that path slower (the job summary compares every row with main's last run) and speed it up. " +
	"If the cost is worth it, raise p95_ms for the row in " + ceilings.Path +
	" with a one-line why; use a number from a Sengi run, never a Mac run. " +
	"issue-*, replica-* and rank/* rows cannot pass the 16 ms bar. One run over can be a stalled VM: " +
	"CI reds only when the same row is over in two main runs in a row (bench repeat)."

// measure samples a row, up to retries more times while it stays over its
// ceiling, and keeps the best try.
func measure(name string, runs int, ceiling float64, check bool, f func() (time.Duration, error)) (result, error) {
	var r result
	var first float64
	for try := range retries + 1 {
		ds, err := sample(runs, f)
		if err != nil {
			return r, err
		}
		s := summarize(name, ds, ceiling)
		if try == 0 {
			first = s.P95
		}
		if try == 0 || s.P95 < r.P95 {
			r = s
		}
		r.Tries, r.First = try+1, first
		if r.OK || !check {
			break
		}
	}
	return r, nil
}

func sample(n int, f func() (time.Duration, error)) ([]time.Duration, error) {
	for range 2 { // warm the page cache and the daemon's statements
		if _, err := f(); err != nil {
			return nil, err
		}
	}
	ds := make([]time.Duration, 0, n)
	for range n {
		d, err := f()
		if err != nil {
			return nil, err
		}
		ds = append(ds, d)
	}
	return ds, nil
}

func summarize(name string, ds []time.Duration, ceiling float64) result {
	slices.Sort(ds)
	pct := func(p float64) float64 {
		i := int(float64(len(ds)-1) * p)
		return ms(ds[i])
	}
	r := result{Name: name, N: len(ds), P50: pct(0.50), P95: pct(0.95), Max: ms(ds[len(ds)-1]), Budget: ceiling}
	r.OK = r.P95 <= ceiling
	return r
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// ---- set-up ----

// benchJudgeStub stands in for `claude -p --output-format json` in the bench daemon's judges: a passing verdict.
const benchJudgeStub = `#!/bin/sh
cat >/dev/null
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"","structured_output":{"pass":true,"fixes":[],"reason":"bench stub judge"}}'
`

func (e *env) setup() error {
	tmp, err := os.MkdirTemp("/tmp", "pcb") // short: socket paths max 103 bytes
	if err != nil {
		return err
	}
	e.tmp = tmp
	e.home = filepath.Join(tmp, "h")
	e.repo = filepath.Join(tmp, "repo")
	for _, d := range []string{e.home, e.repo, filepath.Join(tmp, "u")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}
	// The bench times zm, not a model: its daemon's meaning judges (internal/judge: the mail-chore judge on the
	// seeded reports to ana) call a stub claude that passes at once. A bench runner has no claude CLI
	// or plan login, and a held judge refuses the seed (503), which left the bench with no rows.
	stub := filepath.Join(tmp, "judge-claude")
	if err := os.WriteFile(stub, []byte(benchJudgeStub), 0o700); err != nil {
		return err
	}
	cfg, err := json.Marshal(map[string]any{
		"admission": map[string]any{"min_memory_free_percent": 1, "max_swap_used_mb": 100000000, "min_disk_free_mb": 10},
		"agents":    map[string][]string{"claude": {stub}},
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(e.home, "config.json"), cfg, 0o600); err != nil {
		return err
	}
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "ZM_") || k == "HOME" || k == "CLAUDE_CODE_SESSION_ID" || k == "CODEX_THREAD_ID" ||
			k == "CI" || k == "CLAUDE_ENV_FILE" {
			continue
		}
		e.base = append(e.base, kv)
	}
	e.base = append(e.base, "HOME="+filepath.Join(tmp, "u"), "ZM_HOME="+e.home, "ZM_MACHINE=bench", "ZM_HUMAN=ana",
		"TERM=xterm-256color")
	if err := e.fakeTracker(); err != nil {
		return err
	}
	if err := e.startDaemon(); err != nil {
		return err
	}
	steps := [][]string{
		{"project", "new", "bench", "--path", e.repo},
		{"lease", "lock:bench-a", "--project", "bench", "--ttl", "3600"},
		{"lease", "lock:bench-b", "--project", "bench", "--ttl", "3600"},
	}
	for _, s := range steps {
		if out, err := e.zm(nil, s...); err != nil {
			return fmt.Errorf("zm %s: %v: %s", strings.Join(s, " "), err, out)
		}
	}
	e.runtimeID = "4bb7545a-1111-2222-3333-444455556666"
	out, err := e.zm([]byte(e.payload("SessionStart")), "hook", "claude", "session-start")
	if err != nil {
		return fmt.Errorf("hook session-start: %v: %s", err, out)
	}
	e.session = "bench/4bb7545a"
	for i := range 20 {
		for _, to := range []string{e.session, "human:ana"} {
			if out, err := e.zm(nil, "send", to, fmt.Sprintf("bench mail %d", i), "--project", "bench"); err != nil {
				return fmt.Errorf("send: %v: %s", err, out)
			}
		}
	}
	out, err = e.zm(nil, "token", "mint", "--as", "vida", "--role", "guest", "--project", "bench")
	if err != nil {
		return fmt.Errorf("token mint: %v: %s", err, out)
	}
	for _, l := range strings.Split(string(out), "\n") {
		if authz.LooksLikeToken(strings.TrimSpace(l)) {
			e.token = strings.TrimSpace(l)
		}
	}
	if err := e.seed(); err != nil {
		return err
	}
	if err := e.bindTracker(); err != nil {
		return err
	}
	return e.setupIssues()
}

// raiseMailLimits sets each bench project's own [mail_limits] (SPEC 9 "Send limits") before anything is seeded:
// the seed and the timed rows send thousands of mails from a few senders to a few recipients, far faster than any
// real agent, so the defaults would refuse them. Like the load test, the bench measures latency, not the loop
// breaker, and the defaults for real projects are untouched. The setting is read when a project loads, so the
// daemon restarts to take it. The send diagnostic's fixtures are pinned builds from before send limits and leave
// e.mailLimits off: they have no limits to raise, and refuse the unknown key.
func (e *env) raiseMailLimits() error {
	files, err := filepath.Glob(filepath.Join(e.home, "projects", "*", "project.toml"))
	if err != nil || len(files) == 0 {
		return fmt.Errorf("bench project.toml: %d found: %v", len(files), err)
	}
	for _, name := range files {
		f, err := os.OpenFile(name, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(f, "\n[mail_limits]\nper_sender = %d\nper_pair = %d\n", 1_000_000, 1_000_000)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
	}
	e.stopDaemon()
	return e.startDaemon()
}

// checkTracker makes sure the timed reads answer what they should: the
// label-area guest sees only solver items, the owner sees the item.
func (e *env) checkTracker() error {
	out, err := e.cmd([]string{"ZM_TOKEN=" + e.areaToken}, "work", "ls", "--json", "--limit", "1000").Output()
	if err != nil {
		return fmt.Errorf("guest work ls: %v", err)
	}
	var items []struct {
		ID     string   `json:"id"`
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(out, &items); err != nil || len(items) == 0 {
		return fmt.Errorf("guest work ls: %d items, %v", len(items), err)
	}
	for _, it := range items {
		if !slices.Contains(it.Labels, "solver") {
			return fmt.Errorf("guest of bench/lbl sees %s %v", it.ID, it.Labels)
		}
	}
	if out, err := e.zm(nil, "work", "show", e.labelled); err != nil || !strings.Contains(string(out), "Issue 3:") {
		return fmt.Errorf("work show %s: %v: %s", e.labelled, err, out)
	}
	return nil
}

// fakeTracker writes a fake tracker state of trackerSize issues shaped like
// a live tracker (80% closed, 2% labelled solver, ~1 KiB bodies,
// updated over a year) and puts this binary on PATH as zm-work-fake
// (internal/work/fake), before the daemon starts.
func (e *env) fakeTracker() error {
	fb := filepath.Join(e.tmp, "fb")
	if err := os.MkdirAll(fb, 0o700); err != nil {
		return err
	}
	if _, _, err := fake.Install(fb); err != nil {
		return err
	}
	st := &fake.State{Issues: map[string]*fake.Issue{}}
	body := strings.Repeat("Steps to reproduce, logs and the fix plan. ", 24)
	now := time.Now()
	for i := range trackerSize {
		id := fmt.Sprintf("harbor-%05d", i)
		status := "closed"
		switch x := i * 7919 % 100; {
		case x < 18:
			status = "open"
		case x < 19:
			status = "deferred"
		case x == 19:
			status = "in_progress"
		}
		var labels []string
		if i%50 == 3 {
			labels = append(labels, "solver")
			if e.labelled == "" {
				e.labelled = "bd:" + id
			}
		}
		if i%3 == 0 {
			labels = append(labels, "ux")
		}
		up := now.Add(-time.Duration(i*7%(365*24)) * time.Hour).UTC().Format(time.RFC3339)
		// Distinct created times, older than updated, out of id order: an
		// order by creation time must sort for real.
		created := now.Add(-time.Duration(365*24+i*7919%(365*24)) * time.Hour).UTC().Format(time.RFC3339)
		st.Issues[id] = &fake.Issue{ID: id, Title: fmt.Sprintf("Issue %d: fix the thing in module %d", i, i%40),
			Description: body, Status: status, Labels: labels, CreatedAt: created, UpdatedAt: up}
	}
	state := filepath.Join(fb, "state.json")
	if err := st.Save(state); err != nil {
		return err
	}
	for i, kv := range e.base {
		if strings.HasPrefix(kv, "PATH=") {
			e.base[i] = "PATH=" + fb + string(os.PathListSeparator) + strings.TrimPrefix(kv, "PATH=")
		}
	}
	e.base = append(e.base, fake.EnvState+"="+state)
	return nil
}

// bindTracker binds the fake tracker to bench, adds the label area bench/lbl
// with a guest, and waits for the mirror's first full sync (untimed).
func (e *env) bindTracker() error {
	trs := map[string]any{"trackers": []map[string]any{{"adapter": fake.Name, "prefix": "bd"}}}
	if err := e.api("PATCH", "/v0/projects/bench", "", "", trs, nil); err != nil {
		return err
	}
	if out, err := e.zm(nil, "area", "add", "bench/lbl", "--label", "solver"); err != nil {
		return fmt.Errorf("area add: %v: %s", err, out)
	}
	out, err := e.zm(nil, "token", "mint", "--as", "quinn", "--role", "guest", "--project", "bench", "--area", "lbl")
	if err != nil {
		return fmt.Errorf("token mint: %v: %s", err, out)
	}
	for _, l := range strings.Split(string(out), "\n") {
		if authz.LooksLikeToken(strings.TrimSpace(l)) {
			e.areaToken = strings.TrimSpace(l)
		}
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var src struct {
			Sources []struct {
				Prefix string `json:"prefix"`
				Sync   *struct {
					State string `json:"state"`
					Items int    `json:"items"`
					Error string `json:"error"`
				} `json:"sync"`
			} `json:"sources"`
		}
		if err := e.api("GET", "/v0/work/sources?project=bench", "", "", nil, &src); err != nil {
			return err
		}
		for _, s := range src.Sources {
			if s.Prefix == "bd" && s.Sync != nil && s.Sync.State == "ok" && s.Sync.Items >= trackerSize {
				return e.checkTracker()
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tracker mirror not synced after 3m: %+v", src.Sources)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// seed fills the daemon over the API: two more projects, 60 sessions, 40
// leases and e.scale mails (all but 4% acked, like a real history).
func (e *env) seed() error {
	projects := []string{"bench", "b2", "b3"}
	dirs := map[string]string{"bench": e.repo}
	for _, p := range projects[1:] {
		dirs[p] = filepath.Join(e.tmp, p)
		_ = os.MkdirAll(dirs[p], 0o700)
		if out, err := e.zm(nil, "project", "new", p, "--path", dirs[p]); err != nil {
			return fmt.Errorf("project new %s: %v: %s", p, err, out)
		}
	}
	if e.mailLimits {
		if err := e.raiseMailLimits(); err != nil {
			return err
		}
	}
	e.hc = &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", e.socket())
	}, MaxIdleConnsPerHost: 8}}
	type sess struct{ addr, project string }
	var sessions []sess
	for i := range 60 {
		p := projects[i%3]
		var s struct {
			Address string `json:"address"`
		}
		rid := fmt.Sprintf("%08x-0000-4000-8000-%012x", 0xa0000000+i, i)
		if err := e.api("POST", "/v0/sessions/register", "", "", map[string]any{"agent": "claude", "runtime_id": rid, "cwd": dirs[p]}, &s); err != nil {
			return err
		}
		sessions = append(sessions, sess{s.Address, p})
	}
	for i := range 40 {
		p := projects[i%3]
		if err := e.api("POST", "/v0/leases", "", p, map[string]any{"key": fmt.Sprintf("lock:seed-%d", i), "ttl_seconds": 3600}, nil); err != nil {
			return err
		}
	}
	jobs := make(chan int)
	errc := make(chan error, 8)
	for range 8 {
		go func() {
			var err error
			for i := range jobs {
				if err != nil {
					continue
				}
				sn := sessions[i%len(sessions)]
				to, as, ackAs, needs, kind := sn.addr, "", sn.addr, "", ""
				mark := "/ack"
				switch i % 10 {
				case 0:
					to, ackAs = "human:ana", ""
				case 1:
					// From a session to a person and not an ask: an explicit report,
					// which only the person marks read, with ack. A plain
					// note from an agent to a person is refused.
					to, as, ackAs, needs, kind = "human:ana", sn.addr, "", "human", "report" // a session writing to its human
				case 2:
					to, ackAs = "machine:bench", sn.addr
				case 3:
					to, ackAs = "owners", ""
				}
				var m struct {
					ID string `json:"id"`
				}
				body := map[string]any{"to": to, "body": fmt.Sprintf("seeded mail %d", i)}
				if needs != "" {
					body["needs"] = needs
				}
				if kind != "" {
					body["kind"] = kind
				}
				if err = e.api("POST", "/v0/mail", as, sn.project, body, &m); err == nil && i%25 != 0 {
					err = e.api("POST", "/v0/mail/"+m.ID+mark, ackAs, "", nil, nil)
				}
			}
			errc <- err
		}()
	}
	for i := range e.scale {
		jobs <- i
	}
	close(jobs)
	var first error
	for range 8 {
		if err := <-errc; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// api does one request on the kept-alive socket client.
func (e *env) api(method, path, as, project string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://zm"+path, body)
	if err != nil {
		return err
	}
	// The old Zm-* names on purpose: send-diagnostic.yml runs this harness
	// against pinned builds from before the Potluck rename, which read only
	// these; every build since step 1 reads them too.
	req.Header.Set("Zm-Protocol", "zm/v0")
	req.Header.Set("Content-Type", "application/json")
	if as == "guest" {
		req.Header.Set("Authorization", "Bearer "+e.token)
	} else if as != "" {
		req.Header.Set("Zm-As", as)
	}
	if project != "" {
		req.Header.Set("Zm-Project", project)
		req.Header.Set("Zm-Project-From", "flag")
	}
	resp, err := e.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s %s", method, path, resp.Status, b)
	}
	if out != nil {
		return json.Unmarshal(b, out)
	}
	return nil
}

func (e *env) apiTimed(method, path, as string, in any) func() (time.Duration, error) {
	return func() (time.Duration, error) {
		t := time.Now()
		err := e.api(method, path, as, "", in, nil)
		return time.Since(t), err
	}
}

func (e *env) payload(event string) string {
	b, _ := json.Marshal(map[string]any{"session_id": e.runtimeID, "cwd": e.repo, "hook_event_name": event,
		"prompt": "hello", "transcript_path": filepath.Join(e.tmp, "t.jsonl"), "permission_mode": "default"})
	return string(b)
}

func (e *env) cmd(extraEnv []string, args ...string) *exec.Cmd {
	c := exec.Command(e.bin, args...)
	c.Dir = e.repo
	c.Env = append(append([]string(nil), e.base...), extraEnv...)
	return c
}

func (e *env) zm(stdin []byte, args ...string) ([]byte, error) {
	c := e.cmd(nil, args...)
	if stdin != nil {
		c.Stdin = bytes.NewReader(stdin)
	}
	return c.CombinedOutput()
}

func (e *env) socket() string { return filepath.Join(e.home, "zm.sock") }

func (e *env) startDaemon() error {
	c := e.cmd(nil, "daemon")
	logf, err := os.OpenFile(filepath.Join(e.tmp, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) // appended: the setup restarts the daemon
	if err != nil {
		return err
	}
	c.Stdout, c.Stderr = logf, logf
	if err := c.Start(); err != nil {
		return err
	}
	e.daemon = c
	return waitHealth(e.socket(), 10*time.Second)
}

func (e *env) stopDaemon() {
	stopProcess(e.issueDaemon)
	e.issueDaemon = nil
	stopProcess(e.daemon)
	e.daemon = nil
}

func stopProcess(c *exec.Cmd) {
	if c == nil {
		return
	}
	_ = c.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = c.Process.Kill()
		<-done
	}
}

func (e *env) cleanup() {
	e.stopTeam()
	e.stopDaemon()
	if e.tmp != "" {
		_ = os.RemoveAll(e.tmp)
	}
}

func waitHealth(socket string, timeout time.Duration) error {
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}, DisableKeepAlives: true}}
	deadline := time.Now().Add(timeout)
	for {
		resp, err := hc.Get("http://zm/v0/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("daemon not ready after %s", timeout)
		}
		time.Sleep(500 * time.Microsecond)
	}
}

// ---- measurements ----

func (e *env) measures() map[string]func() (time.Duration, error) {
	rows := e.coreRows()
	maps.Copy(rows, e.nativeRows())
	maps.Copy(rows, e.teamRows())
	return rows
}

func (e *env) coreRows() map[string]func() (time.Duration, error) {
	plain := func(stdin string, extra []string, args ...string) func() (time.Duration, error) {
		return func() (time.Duration, error) {
			c := e.cmd(extra, args...)
			if stdin != "" {
				c.Stdin = strings.NewReader(stdin)
			}
			var out bytes.Buffer
			c.Stdout, c.Stderr = &out, &out
			t := time.Now()
			err := c.Run()
			d := time.Since(t)
			if err != nil {
				return 0, fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, out.String())
			}
			return d, nil
		}
	}
	as := []string{"ZM_AS=" + e.session}
	return map[string]func() (time.Duration, error){
		"version":     plain("", nil, "--version"),
		"hook-prompt": plain(e.payload("UserPromptSubmit"), []string{"CLAUDE_CODE_SESSION_ID=" + e.runtimeID}, "hook", "claude", "prompt"),
		"hook-stop":   plain(e.payload("Stop"), []string{"CLAUDE_CODE_SESSION_ID=" + e.runtimeID}, "hook", "claude", "stop"),
		"hook-down": plain(e.payload("UserPromptSubmit"), []string{"ZM_HOME=" + filepath.Join(e.tmp, "nodaemon")},
			"hook", "claude", "prompt"),
		"notice":       plain("", as, "notice"),
		"inbox":        plain("", nil, "inbox"),
		"send":         plain("", nil, "send", "human:ana", "bench", "--project", "bench"),
		"ls":           plain("", nil, "ls"),
		"ls-tty":       e.tty("ls"),
		"leases":       plain("", nil, "leases"),
		"project-why":  plain("", nil, "project", "why"),
		"events-first": e.firstLine("events", "--since", "0"),
		"mcp-tools":    func() (time.Duration, error) { return e.mcp(nil) },
		"mcp-down": func() (time.Duration, error) {
			return e.mcp([]string{"ZM_HOME=" + filepath.Join(e.tmp, "nodaemon")})
		},
		"zero-start":   e.zeroStart,
		"daemon-ready": e.daemonReady,
		"api-notice":   e.apiTimed("GET", "/v0/mail/notice", e.session, nil),
		"api-inbox":    e.apiTimed("GET", "/v0/mail/inbox", "", nil),
		"api-sessions": e.apiTimed("GET", "/v0/sessions", "", nil),
		"api-leases":   e.apiTimed("GET", "/v0/leases", "", nil),
		"api-guest":    e.apiTimed("GET", "/v0/mail/inbox", "guest", nil),
		"api-state":    e.apiTimed("POST", "/v0/sessions/"+url.PathEscape(e.session)+"/state", e.session, map[string]any{"state": "running"}),
		"api-hook": e.apiTimed("POST", "/v0/sessions/hook", "", map[string]any{"agent": "claude", "runtime_id": e.runtimeID,
			"cwd": e.repo, "state": "running", "notice": true}),
		"work-ls":       plain("", nil, "work", "ls"),
		"work-ls-guest": plain("", []string{"ZM_TOKEN=" + e.areaToken}, "work", "ls"),
		"work-search":   plain("", nil, "work", "ls", "-q", "module 17"),
		"work-show":     plain("", nil, "work", "show", "bd:harbor-00042"),
		"who":           plain("", nil, "who", e.labelled),
		"work-sources":  plain("", nil, "work", "sources"),
		"work-rollup":   e.apiTimed("GET", "/v0/work/rollup", "", nil),
	}
}

// firstLine times a command until its first line of output, then kills it.
func (e *env) firstLine(args ...string) func() (time.Duration, error) {
	return func() (time.Duration, error) {
		c := e.cmd(nil, args...)
		out, err := c.StdoutPipe()
		if err != nil {
			return 0, err
		}
		t := time.Now()
		if err := c.Start(); err != nil {
			return 0, err
		}
		defer func() { _ = c.Process.Kill(); _ = c.Wait() }()
		if _, err := bufio.NewReader(out).ReadString('\n'); err != nil {
			return 0, fmt.Errorf("%s: no line: %v", strings.Join(args, " "), err)
		}
		return time.Since(t), nil
	}
}

// tty runs a command on a terminal nobody answers (the agent's or a CI
// pty) until it exits.
func (e *env) tty(args ...string) func() (time.Duration, error) {
	return func() (time.Duration, error) {
		c := e.cmd(nil, args...)
		t := time.Now()
		f, err := pty.StartWithSize(c, &pty.Winsize{Rows: 40, Cols: 120})
		if err != nil {
			return 0, err
		}
		defer f.Close()
		go func() { _, _ = io.Copy(io.Discard, f) }()
		exited := make(chan error, 1)
		go func() { exited <- c.Wait() }()
		select {
		case err := <-exited:
			if err != nil {
				return 0, fmt.Errorf("%s on a tty: %v", strings.Join(args, " "), err)
			}
			return time.Since(t), nil
		case <-time.After(15 * time.Second):
			_ = c.Process.Kill()
			<-exited
			return 0, fmt.Errorf("%s on a tty: timeout", strings.Join(args, " "))
		}
	}
}

// mcp times `zm mcp` from start to the tools/list answer.
func (e *env) mcp(extra []string) (time.Duration, error) {
	c := e.cmd(extra, "mcp")
	in, err := c.StdinPipe()
	if err != nil {
		return 0, err
	}
	out, err := c.StdoutPipe()
	if err != nil {
		return 0, err
	}
	t := time.Now()
	if err := c.Start(); err != nil {
		return 0, err
	}
	defer func() { _ = in.Close(); _ = c.Process.Kill(); _ = c.Wait() }()
	msgs := []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"bench","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	}
	if _, err := io.WriteString(in, strings.Join(msgs, "\n")+"\n"); err != nil {
		return 0, err
	}
	r := bufio.NewReader(out)
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return 0, fmt.Errorf("mcp: %v", err)
		}
		if strings.Contains(line, `"id":2`) && strings.Contains(line, `"tools"`) {
			return time.Since(t), nil
		}
	}
}

// zeroStart times a fresh HOME (no ~/.zm, default ZM_HOME) in a new git
// repo from `zm init` (which starts the daemon) to `zm work new`
// printing its item; the autostarted daemon is stopped afterwards, untimed.
func (e *env) zeroStart() (time.Duration, error) {
	home, err := os.MkdirTemp(e.tmp, "z")
	if err != nil {
		return 0, err
	}
	repo := filepath.Join(home, "app")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		return 0, err
	}
	extra := []string{"HOME=" + home, "ZM_HOME="}
	run := func(args ...string) (string, error) {
		c := e.cmd(extra, args...)
		c.Dir = repo
		out, err := c.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("%s: %v: %s", strings.Join(args, " "), err, out)
		}
		return string(out), nil
	}
	defer func() {
		_, _ = run("daemon", "stop", "--force")
		_ = os.RemoveAll(home)
	}()
	t := time.Now()
	if _, err := run("init"); err != nil {
		return 0, err
	}
	out, err := run("work", "new", "first")
	d := time.Since(t)
	if err == nil && !strings.Contains(out, "created app/local:") {
		err = fmt.Errorf("work new: %q", out)
	}
	return d, err
}

// daemonReady times a second daemon on its own home from exec to its first
// health answer (the store is migrated by the first, untimed start).
func (e *env) daemonReady() (time.Duration, error) {
	home := filepath.Join(e.tmp, "d2")
	_ = os.MkdirAll(home, 0o700)
	c := e.cmd([]string{"ZM_HOME=" + home}, "daemon")
	t := time.Now()
	if err := c.Start(); err != nil {
		return 0, err
	}
	err := waitHealth(filepath.Join(home, "zm.sock"), 10*time.Second)
	d := time.Since(t)
	_ = c.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = c.Process.Kill()
		<-done
	}
	return d, err
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "bench:", err)
	os.Exit(2)
}

// fsyncP50 is the median of 21 4 KiB write+fsync in dir.
func fsyncP50(dir string) (time.Duration, error) {
	f, err := os.CreateTemp(dir, "fsync")
	if err != nil {
		return 0, err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	buf := make([]byte, 4096)
	var ds []time.Duration
	for i := 0; i < 21; i++ {
		t0 := time.Now()
		if _, err := f.WriteAt(buf, int64(i)*4096); err != nil {
			return 0, err
		}
		if err := f.Sync(); err != nil {
			return 0, err
		}
		ds = append(ds, time.Since(t0))
	}
	slices.Sort(ds)
	return ds[len(ds)/2], nil
}
