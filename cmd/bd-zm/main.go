// Command bd-zm is the bd-compatible door to zm's native issues (SPEC
// "Native issues"): the bd verbs agents use, with bd's flags and output
// (bd v1.0.5; --json output field for field), routed to the zm daemon as the
// caller's zm identity. Install it as `bd` (a symlink) where a project's
// issues live in zm.
//
// The author of every write is the caller's zm identity (socket user,
// token, or session); comments add --author is ignored. bd's actor
// (--actor, BEADS_ACTOR, BD_ACTOR, FARM_BD_ACTOR) is used for one thing: the
// assignee a claim writes, a routing label (bdActor). Memories, prime, export and a read-only sql are served (extra.go);
// the dolt push/pull/commit verbs and sync are no-ops with a note. Not
// supported: bd's markdown batch create (create -f), --watch, time travel
// (--as-of), molecules, gates and wisps.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
)

var (
	flagActor string
	flagJSON  bool
	flagQuiet bool
	flagDir   string
	// flagProject is --project: the project every call names (SPEC "Native
	// issues": beats $ZM_PROJECT, turns off routing by id).
	flagProject string
	// allProjects is list/ready --all-projects: no project chosen.
	allProjects bool
	stdout      io.Writer = os.Stdout
	stderr      io.Writer = os.Stderr
	stdin       io.Reader = os.Stdin
	getenv                = os.Getenv
	// newClient is replaced in tests.
	newClient = defaultClient
)

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "bd",
		Short:         "bd-compatible issues, served by zm",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			fl := cmd.Flags()
			if v, _ := fl.GetBool("version"); v && !cmd.HasParent() {
				return nil // bd --version names no database
			}
			if db, _ := fl.GetString("db"); db != "" || getenv("BEADS_DB") != "" {
				return errors.New("bd-zm serves this project's zm issues: --db and BEADS_DB (another database) are not supported; use real bd for a scratch database")
			}
			if g, _ := fl.GetBool("global"); g {
				return errors.New("bd-zm has no global database (--global)")
			}
			if ro, _ := fl.GetBool("readonly"); ro && writeVerbs[cmd.CommandPath()] {
				return errors.New("--readonly: this command writes")
			}
			if flagDir != "" {
				return os.Chdir(flagDir)
			}
			return nil
		},
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&flagJSON, "json", false, "output JSON")
	pf.StringVar(&flagActor, "actor", "", "the assignee a claim writes (a routing label such as a hub address); authors are your zm identity")
	pf.BoolVarP(&flagQuiet, "quiet", "q", false, "suppress non-essential output")
	pf.BoolVarP(new(bool), "verbose", "v", false, "ignored")
	pf.StringVarP(&flagDir, "directory", "C", "", "run as if started in this directory")
	pf.StringVar(&flagProject, "project", "", "the zm project (slug); beats $ZM_PROJECT and routing by id")
	pf.String("db", "", "not supported (zm owns the store)")
	pf.Bool("readonly", false, "refuse writes")
	pf.Bool("sandbox", false, "ignored")
	pf.Bool("global", false, "not supported")
	pf.String("format", "", "json = --json")
	// bd's --version/-V on the root: the same line as bd version, no zm call.
	root.Flags().BoolP("version", "V", false, "print version information")
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		if v, _ := cmd.Flags().GetBool("version"); v {
			return printVersion(cmd)
		}
		return cmd.Help()
	}
	root.AddCommand(createCmd(), updateCmd(), showCmd(), listCmd(), readyCmd(), closeCmd(), reopenCmd(), commentsCmd(), commentCmd(),
		depCmd(), labelCmd(), searchCmd(), childrenCmd(), statsCmd("stats"), statsCmd("status"), countCmd(), blockedCmd())
	root.AddCommand(extraCmds()...)
	return root
}

// writeVerbs are refused under --readonly.
var writeVerbs = map[string]bool{"bd create": true, "bd new": true, "bd update": true, "bd close": true, "bd done": true,
	"bd reopen": true, "bd comments add": true, "bd comment": true, "bd dep": true, "bd dep add": true, "bd dep remove": true,
	"bd label add": true, "bd label remove": true, "bd note": true, "bd remember": true, "bd forget": true}

func main() {
	ctx := context.Background()
	os.Exit(run(ctx, os.Args[1:]))
}

// run executes args and returns the exit code.
func run(ctx context.Context, args []string) int {
	oldOut, oldErr := stdout, stderr
	stdout, stderr = cleanWriter{stdout}, cleanWriter{stderr}
	defer func() { stdout, stderr = oldOut, oldErr }()
	root := rootCmd()
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	flagJSON, flagQuiet, flagDir, scopeDone, scopeSlug, scopeQuiet, flagActor, actorDone = false, false, "", false, "", false, "", false
	flagProject, allProjects, parentFlag, memo = "", false, "", nil
	err := root.ExecuteContext(ctx)
	if err == nil {
		return 0
	}
	var ex exitErr
	if errors.As(err, &ex) {
		return int(ex)
	}
	if strings.HasPrefix(err.Error(), "unknown command") {
		err = fmt.Errorf("%v: bd-zm (this project's issues live in zm) does not serve it; `bd --help` lists what it does", err)
	}
	fail(err)
	err = orchclient.BoxCallError(getenv, err) // an unsigned answer from a box's authority is unavailable too
	if client.NotRunning(err) || api.IsCode(err, api.CodeUnavailable) {
		return 3 // no daemon, or its authority does not answer: not an answer about the issues
	}
	return 1
}

// exitErr ends with a code after the output was already written.
type exitErr int

func (e exitErr) Error() string { return "exit " + strconv.Itoa(int(e)) }

// fail prints bd's error: "Error: msg" (or {"error", "schema_version"} with --json).
func fail(err error) {
	err = orchclient.BoxCallError(getenv, err)
	msg := err.Error()
	var ae *api.Error
	if errors.As(err, &ae) {
		msg = ae.Message
	}
	if flagJSON {
		_ = writeJSON(stderr, map[string]any{"error": msg}, true)
		return
	}
	fmt.Fprintln(stderr, "Error: "+msg)
}

func asJSON(cmd *cobra.Command) bool {
	if flagJSON {
		return true
	}
	f, _ := cmd.Flags().GetString("format")
	if f == "json" {
		flagJSON = true
	}
	return flagJSON
}

func defaultClient(ctx context.Context) (*client.Client, error) {
	// In a pool box the credential file alone names the authority: no
	// socket, no autostart, no prefix cache on disk, no fallback.
	if c, ok, err := orchclient.BoxClient(getenv); ok {
		cacheHome = ""
		return c, err
	}
	cfg, err := config.LoadClient()
	if err != nil {
		return nil, err
	}
	c := client.New(cfg)
	cacheHome = cfg.Home
	if c.Socket != "" && getenv("ZM_NO_AUTOSTART") == "" {
		c.Autostart = func(ctx context.Context) error {
			// zm starts its daemon on any command that needs it (SPEC 17.1).
			zm, err := exec.LookPath("zm")
			if err != nil {
				if self, e := os.Executable(); e == nil {
					zm = filepath.Join(filepath.Dir(self), "zm")
				}
			}
			cmd := exec.CommandContext(ctx, zm, "whoami")
			cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
			return cmd.Run()
		}
	}
	return c, nil
}

// connect is the client for this command: newClient with --project applied.
func connect(ctx context.Context) (*client.Client, error) {
	c, err := newClient(ctx)
	if err != nil {
		return nil, err
	}
	if flagProject != "" {
		c.Project, c.ProjectFrom = flagProject, "flag"
	}
	return c, nil
}

// agentMode is bd's: BD_AGENT_MODE=1 or CLAUDE_CODE set (lists default to 20).
func agentMode() bool { return getenv("BD_AGENT_MODE") == "1" || getenv("CLAUDE_CODE") != "" }

// ---- helpers ----

// parsePriority accepts 0-4 or P0-P4 (bd's ParsePriority).
func parsePriority(s string) (int, error) {
	t := strings.TrimSpace(s)
	t = strings.TrimPrefix(strings.TrimPrefix(t, "P"), "p")
	n := 0
	i := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		n = n*10 + int(t[i]-'0')
		i++
	}
	if i == 0 || n < 0 || n > 4 {
		return 0, fmt.Errorf("invalid priority %q (expected 0-4 or P0-P4, not words like high/medium/low)", s)
	}
	return n, nil
}

// parseWhen reads bd's time flags: RFC 3339, YYYY-MM-DD, +6h/+2d/+1w, tomorrow.
func parseWhen(s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	now := time.Now().UTC()
	switch strings.ToLower(s) {
	case "":
		return nil, nil
	case "tomorrow":
		t := now.Add(24 * time.Hour)
		return &t, nil
	case "today", "now":
		return &now, nil
	}
	if strings.HasPrefix(s, "+") && len(s) > 2 {
		n, err := strconv.Atoi(s[1 : len(s)-1])
		if err == nil {
			unit := map[byte]time.Duration{'m': time.Minute, 'h': time.Hour, 'd': 24 * time.Hour, 'w': 7 * 24 * time.Hour}[s[len(s)-1]]
			if unit > 0 {
				t := now.Add(time.Duration(n) * unit)
				return &t, nil
			}
		}
	}
	for _, l := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(l, s); err == nil {
			t = t.UTC()
			return &t, nil
		}
	}
	return nil, fmt.Errorf("cannot parse time %q (use +6h, tomorrow, YYYY-MM-DD or RFC 3339)", s)
}

func readText(v string) (string, error) {
	if v != "-" {
		return v, nil
	}
	b, err := io.ReadAll(stdin)
	return string(b), err
}

func readFile(path string) (string, error) {
	if path == "-" {
		b, err := io.ReadAll(stdin)
		return string(b), err
	}
	b, err := os.ReadFile(path)
	return string(b), err
}

// scope is the project bd works in, like bd's one database (pickProject),
// set on c so creates and memory writes land there too. "" = no project
// chosen: lists cover every project the caller reads.
func scope(ctx context.Context, c *client.Client) string {
	if scopeDone {
		return scopeSlug
	}
	scopeDone = true
	scopeSlug = pickProject(ctx, c)
	if scopeSlug != "" && c.Project == "" {
		// Like $ZM_PROJECT: resolves writes, does not narrow reads (an id
		// is found in any project the caller reads).
		c.Project, c.ProjectFrom = scopeSlug, "env"
	}
	return scopeSlug
}

var (
	scopeDone bool
	scopeSlug string
)

// pickProject chooses bd's project:
//  1. --project/$ZM_PROJECT (c.Project), as given;
//  2. the project the directory resolves to (zm's rules: .zm/project.toml,
//     repo paths and remotes, the session, default_project) when it has
//     native issues; else the one $BEADS_DIR's directory resolves to;
//  3. the only project with native issues the caller reads (bd on this
//     machine always reached one database, from any directory);
//  4. the resolved project even without native issues yet (its first issue
//     creates them), else "".
func pickProject(ctx context.Context, c *client.Client) string {
	if c.Project != "" {
		return c.Project
	}
	m, err := issueProjects(ctx, c, false)
	if err != nil {
		return ""
	}
	slug, native, note := pickFrom(ctx, c, m.ps)
	if !native && !m.fresh {
		// A cached map may predate the resolved project's first issue: look again.
		if m, err = issueProjects(ctx, c, true); err != nil {
			return ""
		}
		slug, _, note = pickFrom(ctx, c, m.ps)
	}
	if note != "" && !scopeQuiet {
		fmt.Fprintln(stderr, note)
	}
	return slug
}

// pickFrom is pickProject over ps; native says the pick has native issues
// by the resolution rules (not a fallback a newer map could change); note is
// what to tell the caller.
func pickFrom(ctx context.Context, c *client.Client, ps orchapi.IssueProjects) (slug string, native bool, note string) {
	has := func(id string) string {
		for _, p := range ps.Projects {
			if p.Project == id {
				return p.Slug
			}
		}
		return ""
	}
	// away: the authority of a joined project did not answer. Naming the project
	// makes the call fail saying so: another project's list, or an empty one,
	// would read as "no work".
	away := func(id string) bool {
		return slices.ContainsFunc(ps.Skipped, func(s api.WorkSkipped) bool { return s.Project == id })
	}
	resolved := ""
	if r, err := c.Resolve(ctx); err == nil && r.Project != nil {
		if s := has(r.Project.ID); s != "" {
			return s, true, ""
		}
		if away(r.Project.ID) {
			return r.Project.ID, true, ""
		}
		resolved = r.Project.Slug
	}
	if d := getenv("BEADS_DIR"); d != "" && c.Socket != "" {
		cwd := c.Cwd
		c.Cwd = filepath.Dir(filepath.Clean(d))
		r, err := c.Resolve(ctx)
		c.Cwd = cwd
		if err == nil && r.Project != nil {
			if s := has(r.Project.ID); s != "" {
				return s, true, ""
			}
			if away(r.Project.ID) {
				return r.Project.ID, true, ""
			}
		}
	}
	if len(ps.Skipped) > 0 {
		if resolved != "" {
			return resolved, false, "" // a joined project with no issues yet, say; not the away one's
		}
		return ps.Skipped[0].Project, false, "" // "the only project" is not known with one away
	}
	if len(ps.Projects) == 1 {
		if resolved != "" {
			note = fmt.Sprintf("bd: %s has no native issues; using %s", resolved, ps.Projects[0].Slug)
		}
		return ps.Projects[0].Slug, false, note
	}
	return resolved, false, ""
}

// scopeQuiet keeps pickProject's note off stderr (bd prime never writes there).
var scopeQuiet bool

// listIssues is a list in scope.
func listIssues(ctx context.Context, c *client.Client, q orchapi.IssueQuery) (orchapi.IssueList, error) {
	if q.Project == "" && !allProjects {
		q.Project = scope(ctx, c)
	}
	l, err := orchclient.Issues(ctx, c, q)
	for _, sk := range l.Skipped {
		fmt.Fprintf(stderr, "bd: not listed: %s\n", sk.Error)
	}
	return l, err
}

// resolveID completes a bare hash with the only visible issue prefix.
func resolveID(ctx context.Context, c *client.Client, id string) string {
	id = strings.TrimSpace(id)
	if strings.Contains(id, "-") {
		return id
	}
	m, err := issueProjects(ctx, c, false)
	if err != nil || len(m.ps.Projects) != 1 {
		return id
	}
	return m.ps.Projects[0].Prefix + "-" + id
}

func fmtFeedback(id, title string) string {
	if title == "" {
		return id
	}
	return id + " — " + title255(title)
}

func errMsg(err error) string {
	var up unknownPrefix
	if errors.As(err, &up) {
		return "no issue found: " + up.err.Message
	}
	var ae *api.Error
	if errors.As(err, &ae) {
		if ae.Code == api.CodeNotFound {
			return "no issue found"
		}
		return ae.Message
	}
	return err.Error()
}

func notFound(err error) bool { return api.IsCode(err, api.CodeNotFound) }

// ---- create ----

// urgencyAnchored reports whether a new P0/P1 says why it is urgent: a due
// date, an owner, or work it blocks (zm-921e5.24.10). Epics carry no urgency
// of their own, so they keep the priority they are given.
func urgencyAnchored(typ, due, assignee string, deps []string) bool {
	if normalizeType(typ) == "epic" || strings.TrimSpace(due) != "" || strings.TrimSpace(assignee) != "" {
		return true
	}
	for _, d := range deps {
		if k, _, ok := strings.Cut(strings.TrimSpace(d), ":"); ok && k == orchapi.DepBlocks {
			return true
		}
	}
	return false
}

func createCmd() *cobra.Command {
	var title, typ, prio, desc, body, msg, bodyFile, descFile, design, designFile, acceptance, notes, extRef, assignee, id, parent, due, deferS, meta, specID, file string
	var labels, labelAlias, deps []string
	var stdinBody, silent, noInherit, dryRun bool
	var estimate int
	cmd := &cobra.Command{
		Use: "create [title]", Aliases: []string{"new"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if file != "" {
				return errors.New("markdown batch create (-f) is not supported by bd-zm; create issues one at a time")
			}
			if dryRun {
				return errors.New("--dry-run is not supported by bd-zm")
			}
			if len(args) > 0 {
				if strings.HasPrefix(args[0], "-") {
					return fmt.Errorf("title cannot start with '-' (got %q)", args[0])
				}
				if title != "" && title != args[0] {
					return fmt.Errorf("cannot specify both a positional title and --title with different values")
				}
				title = args[0]
			}
			if title == "" {
				return errors.New("title required (or use --file to create from markdown)")
			}
			d, err := description(cmd, desc, body, msg, bodyFile, descFile, stdinBody)
			if err != nil {
				return err
			}
			if designFile != "" {
				if design, err = readFile(designFile); err != nil {
					return err
				}
			}
			req := orchapi.NewIssueRequest{ID: id, Title: title, Description: d, Design: design, AcceptanceCriteria: acceptance, Notes: notes,
				SpecID: specID, IssueType: typ, Assignee: assignee, ExternalRef: extRef, Labels: append(labels, labelAlias...), Parent: parent,
				NoInheritLabels: noInherit, Deps: deps}
			p, err := parsePriority(prio)
			if err != nil {
				return err
			}
			if p <= 1 && !urgencyAnchored(typ, due, assignee, deps) {
				fmt.Fprintf(stderr, "bd: filed at P2, not P%d: a new P0/P1 needs --due, --assignee or a blocks link (--deps blocks:<id>)\n", p)
				p = 2
			}
			req.Priority = &p
			if estimate > 0 {
				req.EstimatedMinutes = &estimate
			}
			if req.DueAt, err = parseWhen(due); err != nil {
				return err
			}
			if req.DeferUntil, err = parseWhen(deferS); err != nil {
				return err
			}
			if meta != "" {
				if strings.HasPrefix(meta, "@") {
					if meta, err = readFile(meta[1:]); err != nil {
						return err
					}
				}
				req.Metadata = json.RawMessage(meta)
			}
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			if req.Parent != "" {
				r, err := route(ctx, c, req.Parent)
				if err != nil {
					return fmt.Errorf("--parent %s: %s", req.Parent, errMsg(err))
				}
				req.Parent, c = r.id, r.c // a child lands in its parent's project
			}
			// A new issue lands in bd's project.
			scope(ctx, c)
			it, err := orchclient.CreateIssue(ctx, c, req)
			if err != nil {
				return err
			}
			switch {
			case asJSON(cmd):
				return writeJSON(stdout, toBD(it, true), true)
			case silent:
				fmt.Fprintln(stdout, it.ID)
			default:
				fmt.Fprintf(stdout, "✓ Created issue: %s\n  Priority: P%d\n  Status: %s\n", fmtFeedback(it.ID, it.Title), it.Priority, it.Status)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&title, "title", "", "issue title")
	f.StringVarP(&typ, "type", "t", "task", "issue type")
	f.StringVarP(&prio, "priority", "p", "2", "priority 0-4 or P0-P4")
	f.StringVarP(&desc, "description", "d", "", "description (- reads stdin)")
	f.StringVar(&body, "body", "", "alias of --description")
	f.StringVarP(&msg, "message", "m", "", "alias of --description")
	f.StringVar(&bodyFile, "body-file", "", "read the description from a file (- = stdin)")
	f.StringVar(&descFile, "description-file", "", "alias of --body-file")
	f.BoolVar(&stdinBody, "stdin", false, "read the description from stdin")
	f.StringVar(&design, "design", "", "design notes")
	f.StringVar(&designFile, "design-file", "", "read design notes from a file")
	f.StringVar(&acceptance, "acceptance", "", "acceptance criteria")
	f.StringVar(&notes, "notes", "", "notes")
	f.String("append-notes", "", "ignored by create")
	f.StringVar(&extRef, "external-ref", "", "external reference")
	f.StringVarP(&assignee, "assignee", "a", "", "assignee")
	f.StringSliceVarP(&labels, "labels", "l", nil, "labels (comma-separated or repeated)")
	f.StringSliceVar(&labelAlias, "label", nil, "alias of --labels")
	f.StringVar(&id, "id", "", "explicit id")
	f.StringVar(&parent, "parent", "", "parent issue (the id becomes <parent>.N)")
	f.BoolVar(&noInherit, "no-inherit-labels", false, "do not copy the parent's labels")
	f.StringSliceVar(&deps, "deps", nil, "dependencies: id or type:id (blocks:X = this blocks X)")
	f.StringVarP(&file, "file", "f", "", "markdown batch (not supported)")
	f.BoolVar(&silent, "silent", false, "print only the id")
	f.BoolVar(&dryRun, "dry-run", false, "not supported")
	f.IntVarP(&estimate, "estimate", "e", 0, "estimate in minutes")
	f.StringVar(&due, "due", "", "due time")
	f.StringVar(&deferS, "defer", "", "defer until (a future time sets status deferred)")
	f.StringVar(&meta, "metadata", "", "metadata JSON (or @file)")
	f.StringVar(&specID, "spec-id", "", "spec id")
	for _, h := range []string{"body", "message", "description-file", "label", "append-notes"} {
		_ = f.MarkHidden(h)
	}
	return cmd
}

// description resolves bd's description flags (body-file wins; both is an error).
func description(cmd *cobra.Command, desc, body, msg, bodyFile, descFile string, stdinBody bool) (string, error) {
	if bodyFile == "" {
		bodyFile = descFile
	}
	if stdinBody {
		bodyFile = "-"
	}
	d := desc
	if d == "" {
		d = body
	}
	if d == "" {
		d = msg
	}
	if bodyFile != "" {
		if d != "" {
			return "", errors.New("cannot combine --body-file with --description")
		}
		return readFile(bodyFile)
	}
	return readText(d)
}

// ---- update ----

func updateCmd() *cobra.Command {
	var status, prio, title, typ, assignee, desc, body, msg, bodyFile, descFile, design, designFile, acceptance, acceptanceAlias, extRef, specID, notes, appendNotes, parent, due, deferS, session, meta string
	var addLabels, removeLabels, setLabels []string
	var claim, stdinBody, replace bool
	var estimate int
	cmd := &cobra.Command{
		Use: "update [id...]",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if len(args) == 0 {
				return errors.New("no issue id given")
			}
			fl := cmd.Flags()
			req := orchapi.UpdateIssueRequest{AddLabels: addLabels, RemoveLabels: removeLabels, SetLabels: setLabels, Session: session}
			n := 0
			str := func(name string, dst **string, v string) {
				if fl.Changed(name) {
					x := v
					*dst = &x
					n++
				}
			}
			str("status", &req.Status, status)
			str("title", &req.Title, title)
			str("type", &req.IssueType, typ)
			str("assignee", &req.Assignee, assignee)
			str("design", &req.Design, design)
			str("external-ref", &req.ExternalRef, extRef)
			str("spec-id", &req.SpecID, specID)
			str("notes", &req.Notes, notes)
			str("append-notes", &req.AppendNotes, appendNotes)
			str("parent", &req.Parent, parent)
			if fl.Changed("acceptance-criteria") {
				acceptance = acceptanceAlias
			}
			if fl.Changed("acceptance") || fl.Changed("acceptance-criteria") {
				req.AcceptanceCriteria = &acceptance
				n++
			}
			if fl.Changed("description") || fl.Changed("body") || fl.Changed("message") || bodyFile != "" || descFile != "" || stdinBody {
				d, err := description(cmd, desc, body, msg, bodyFile, descFile, stdinBody)
				if err != nil {
					return err
				}
				req.Description = &d
				n++
			}
			if designFile != "" {
				d, err := readFile(designFile)
				if err != nil {
					return err
				}
				req.Design = &d
				n++
			}
			if fl.Changed("priority") {
				p, err := parsePriority(prio)
				if err != nil {
					return err
				}
				req.Priority = &p
				n++
			}
			if fl.Changed("estimate") {
				req.EstimatedMinutes = &estimate
				n++
			}
			if fl.Changed("due") {
				t, err := parseWhen(due)
				if err != nil {
					return err
				}
				req.DueAt, req.ClearDue = t, t == nil
				n++
			}
			if fl.Changed("defer") {
				t, err := parseWhen(deferS)
				if err != nil {
					return err
				}
				req.DeferUntil, req.ClearDefer = t, t == nil
				n++
			}
			if meta != "" {
				req.Metadata = json.RawMessage(meta)
				n++
			}
			n += len(addLabels) + len(removeLabels) + len(setLabels)
			if req.Notes != nil && req.AppendNotes != nil {
				return errors.New("cannot combine --notes and --append-notes")
			}
			if replace && req.Notes == nil {
				return errors.New("--replace requires --notes")
			}
			if n == 0 && !claim {
				fmt.Fprintln(stdout, "No updates specified")
				return nil
			}
			if req.Session == "" {
				req.Session = getenv("CLAUDE_SESSION_ID")
			}
			fence := fenceOf(cmd)
			req.Fence = fence
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			var out []*bdIssue
			ok := 0
			for _, raw := range args {
				r, err := route(ctx, c, raw)
				if err != nil {
					if notFound(err) {
						fmt.Fprintln(stderr, missing(raw, err))
					} else {
						fmt.Fprintf(stderr, "Error updating %s: %s\n", raw, errMsg(err))
					}
					continue
				}
				id, c := r.id, r.c
				update := req
				var previousNotes string
				if req.Notes != nil {
					d, err := orchclient.Issue(ctx, c, id)
					if err != nil {
						fmt.Fprintf(stderr, "Error updating %s: %s\n", raw, errMsg(err))
						continue
					}
					if d.Issue.Notes != "" && !replace {
						fmt.Fprintf(stderr, "Error updating %s: notes are not empty; use --append-notes to add text or --notes ... --replace to replace them (bd history %s shows earlier changes)\n", raw, raw)
						continue
					}
					// Guard the read/check/write gap: another agent may append notes
					// after this read, including when the field was empty.
					previousNotes, update.Base = d.Issue.Notes, d.Issue.Version
				}
				var it orchapi.Issue
				var leaseFence int64
				if claim {
					w, err := orchclient.ClaimIssueWith(ctx, c, id, orchapi.ClaimIssueRequest{Assignee: bdActor(c), Fence: fence})
					if err != nil {
						if notFound(err) {
							fmt.Fprintf(stderr, "Issue %s not found\n", raw)
						} else {
							fmt.Fprintf(stderr, "Error claiming %s: %s\n", raw, errMsg(err))
						}
						continue
					}
					it, leaseFence = w.Issue, w.LeaseFence
					if req.Notes != nil {
						if it.Notes != previousNotes {
							fmt.Fprintf(stderr, "Error updating %s: notes changed while claiming; claim succeeded and was not rolled back; read the issue before retrying\n", raw)
							continue
						}
						update.Base = it.Version
					}
				}
				if n > 0 {
					w, err := orchclient.UpdateIssue(ctx, c, id, update)
					if err != nil {
						if notFound(err) {
							fmt.Fprintf(stderr, "Issue %s not found\n", raw)
						} else {
							fmt.Fprintf(stderr, "Error updating %s: %s\n", raw, errMsg(err))
						}
						if claim {
							// Claim and update are separate writes. Never release here:
							// it may have been an existing or concurrently changed lease.
							fmt.Fprintf(stderr, "Claim for %s succeeded before the update failed and was not rolled back; read the issue before retrying\n", raw)
						}
						continue
					}
					it = w.Issue
				}
				ok++
				if asJSON(cmd) {
					b := toBD(it, false)
					b.LeaseFence = leaseFence
					out = append(out, b)
				} else {
					fmt.Fprintf(stdout, "✓ Updated issue: %s\n", fmtFeedback(it.ID, it.Title))
				}
			}
			if asJSON(cmd) && len(out) > 0 {
				if err := writeJSON(stdout, out, false); err != nil {
					return err
				}
			}
			if ok == 0 {
				return exitErr(1)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&status, "status", "s", "", "new status")
	f.StringVarP(&prio, "priority", "p", "", "priority 0-4 or P0-P4")
	f.StringVar(&title, "title", "", "new title")
	f.StringVarP(&typ, "type", "t", "", "issue type")
	f.StringVarP(&assignee, "assignee", "a", "", "assignee (\"\" clears)")
	f.StringVarP(&desc, "description", "d", "", "description (- reads stdin)")
	f.StringVar(&body, "body", "", "alias of --description")
	f.StringVarP(&msg, "message", "m", "", "alias of --description")
	f.StringVar(&bodyFile, "body-file", "", "read the description from a file")
	f.StringVar(&descFile, "description-file", "", "alias of --body-file")
	f.BoolVar(&stdinBody, "stdin", false, "read the description from stdin")
	f.Bool("allow-empty-description", false, "accepted")
	f.StringVar(&design, "design", "", "design notes")
	f.StringVar(&designFile, "design-file", "", "read design notes from a file")
	f.StringVar(&acceptance, "acceptance", "", "acceptance criteria")
	f.StringVar(&acceptanceAlias, "acceptance-criteria", "", "alias of --acceptance")
	f.StringVar(&extRef, "external-ref", "", "external reference (\"\" clears)")
	f.StringVar(&specID, "spec-id", "", "spec id")
	f.IntVarP(&estimate, "estimate", "e", 0, "estimate in minutes")
	f.StringVar(&notes, "notes", "", "set notes (nonempty notes require --replace; use --append-notes to add text)")
	f.BoolVar(&replace, "replace", false, "allow --notes to replace or clear nonempty notes")
	f.StringVar(&appendNotes, "append-notes", "", "append a line to notes")
	f.StringSliceVar(&addLabels, "add-label", nil, "add labels")
	f.StringSliceVar(&removeLabels, "remove-label", nil, "remove labels")
	f.StringSliceVar(&setLabels, "set-labels", nil, "replace every label")
	f.StringVar(&parent, "parent", "", "new parent (\"\" removes it)")
	f.BoolVar(&claim, "claim", false, "claim it: assignee you, status in_progress (atomic)")
	f.StringVar(&due, "due", "", "due time (\"\" clears)")
	f.StringVar(&deferS, "defer", "", "defer until (\"\" clears)")
	f.StringVar(&session, "session", "", "closing session (default $CLAUDE_SESSION_ID)")
	f.StringVar(&meta, "metadata", "", "metadata JSON, merged")
	fenceFlag(cmd, true)
	for _, h := range []string{"body", "message", "description-file", "acceptance-criteria"} {
		_ = f.MarkHidden(h)
	}
	return cmd
}

// bdActor is the assignee a claim writes, as bd resolved its actor: --actor,
// then $BEADS_ACTOR, $BD_ACTOR, $FARM_BD_ACTOR (bd-guard also passes it as
// --actor); inside an agent session, the session the call answers as (c.As:
// $ZM_AS, else the runtime's session id), "<lane>@<session>" with $ZM_LANE;
// then git's user.name, then $USER. hub who/send and the claim hooks route by
// it. It is a routing label, not an identity: the claim is the caller's
// (claimed_by) and every author is the caller's zm identity.
func bdActor(c *client.Client) string {
	if actorDone {
		return actorMemo
	}
	actorDone, actorMemo = true, resolveActor(c.As)
	return actorMemo
}

// actorDone/actorMemo: bdActor resolves once per command (git is a process).
var (
	actorDone bool
	actorMemo string
)

// envLane names the lane (a sub-agent) a session's command runs for: Claude
// Code gives a sub-agent's shell no id of its own, so the dispatcher sets it.
const envLane = "ZM_LANE"

func resolveActor(as string) string {
	if a := strings.TrimSpace(flagActor); a != "" {
		return a
	}
	for _, k := range []string{"BEADS_ACTOR", "BD_ACTOR", "FARM_BD_ACTOR"} {
		if a := strings.TrimSpace(getenv(k)); a != "" {
			return a
		}
	}
	if a, err := api.ParseAddress(as); err == nil && a.Kind == api.KindSession {
		lane := strings.ToLower(strings.TrimSpace(getenv(envLane)))
		if lane != "" && !api.ValidName(lane) {
			fmt.Fprintf(stderr, "bd: $%s %q ignored (the claim names the session only): a lane is letters, digits, '.', '_' or '-', at most 63; rename it\n", envLane, lane)
			lane = ""
		}
		return api.SessionLabel(a.String(), lane)
	}
	if gitUserName != nil {
		if a := strings.TrimSpace(gitUserName()); a != "" {
			return a
		}
	}
	return strings.TrimSpace(getenv("USER"))
}

// gitUserName is `git config user.name` (replaced in tests).
var gitUserName = func() string {
	out, err := exec.Command("git", "config", "user.name").Output()
	if err != nil {
		return ""
	}
	return string(out)
}
