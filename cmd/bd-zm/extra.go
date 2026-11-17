package main

// The bd verbs around the issue verbs that agents and scripts run on a
// machine whose issues moved to zm: prime, memories, export, sql, the dolt
// verbs, note, version and where. Verbs with no zm meaning (config, init,
// import, backup and bd's maintenance verbs) refuse with a reason; sync and
// the dolt push/pull/commit verbs succeed with a one-line note, because zm
// already holds and syncs the data.

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/client"
)

// bdVersion is the bd release whose flags and output bd-zm follows.
const bdVersion = "1.0.5"

func extraCmds() []*cobra.Command {
	cmds := []*cobra.Command{primeCmd(), rememberCmd(), memoriesCmd(), forgetCmd(), recallCmd(), exportCmd(), sqlCmd(), doltCmd(),
		noteCmd(), historyCmd(), versionCmd(), whereCmd(), contextCmd(), noteOnly("sync", "nothing to sync; zm holds this project's issues")}
	for verb, why := range refused {
		cmds = append(cmds, refuseCmd(verb, why))
	}
	return cmds
}

// refused are bd verbs with no meaning on zm's store: they fail with the reason.
var refused = map[string]string{
	"config":        "settings live in zm (zm project, zm tracker); memories are bd remember/memories",
	"init":          "this project's issues already live in zm",
	"import":        "this project's issues already live in zm",
	"backup":        "zm backs up the issue store (zm tracker backup)",
	"restore":       "zm restores the issue store (zm issues restore)",
	"migrate":       "zm owns the schema",
	"doctor":        "check zm instead: `zm doctor`, `bd dolt show`",
	"prune":         "zm keeps every issue",
	"purge":         "zm keeps every issue",
	"gc":            "zm compacts its own store",
	"compact":       "zm compacts its own store",
	"flatten":       "zm compacts its own store",
	"cleanup":       "zm keeps every issue",
	"delete":        "zm keeps every issue: close it instead",
	"edit":          "use bd update with the field's flag",
	"rename":        "issue ids are permanent in zm",
	"rename-prefix": "issue ids are permanent in zm",
	"kv":            "use bd remember/memories/recall/forget",
	"mol":           "molecules are not carried by zm",
	"gate":          "gates are not carried by zm",
	"mail":          "use zm mail (zm send, zm inbox)",
	"federation":    "zm syncs projects itself",
	"branch":        "zm's store has no branches",
	"vc":            "zm's store has no version control verbs; history is per issue (bd history <id>)",
	"diff":          "zm's store has no version control verbs; history is per issue (bd history <id>)",
	"hooks":         "zm installs no git hooks for issues",
	"setup":         "zm installs its own agent integrations (zm init)",
	"upgrade":       "bd-zm is upgraded with zm",
}

// historyCmd reads the existing, access-scoped issue changelog rather than
// redirecting callers to a command another binary might not implement.
func historyCmd() *cobra.Command {
	return &cobra.Command{
		Use: "history <id>", Short: "Show an issue's changes, including previous field values", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			r, err := route(ctx, c, args[0])
			if err != nil {
				return err
			}
			hs, err := orchclient.IssueHistory(ctx, r.c, r.id)
			if err != nil {
				return err
			}
			if asJSON(cmd) {
				if hs == nil {
					hs = []orchapi.IssueChange{}
				}
				return writeJSON(stdout, hs, false)
			}
			for _, h := range hs {
				fmt.Fprintf(stdout, "%s  %s  %s\n", h.At.Format(time.RFC3339), h.Kind, h.Actor)
				fields := make([]string, 0, len(h.Fields))
				for field := range h.Fields {
					fields = append(fields, field)
				}
				sort.Strings(fields)
				for _, field := range fields {
					fmt.Fprintf(stdout, "  %s: %s\n", field, h.Fields[field])
				}
				if h.Comment != "" {
					fmt.Fprintf(stdout, "  %s\n", h.Comment)
				}
			}
			return nil
		},
	}
}

func refuseCmd(verb, why string) *cobra.Command {
	return &cobra.Command{
		Use: verb, DisableFlagParsing: true, Hidden: true,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("bd %s is not served: this project's issues live in zm (zm-native); %s", verb, why)
		},
	}
}

func noteOnly(verb, note string) *cobra.Command {
	return &cobra.Command{
		Use: verb, DisableFlagParsing: true,
		RunE: func(*cobra.Command, []string) error {
			fmt.Fprintln(stdout, "zm-native: "+note)
			return nil
		},
	}
}

// ---- version, where ----

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:  "version",
		RunE: func(cmd *cobra.Command, _ []string) error { return printVersion(cmd) },
	}
}

// servedBy names where the client talks to zm.
func servedBy(c *client.Client) string {
	if c.Socket != "" {
		return "zm daemon " + c.Socket
	}
	if getenv(orchclient.EnvBoxCredential) != "" {
		return "zm authority (box credential)"
	}
	if h := getenv("ZM_HOST"); h != "" {
		return "zm at " + h
	}
	return "zm"
}

func whereCmd() *cobra.Command {
	return &cobra.Command{
		Use: "where",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			p := scope(ctx, c)
			authority := ""
			if r := joined(ctx, c, p); r != nil {
				p, authority = r.Project.Slug, r.Authority
			}
			if asJSON(cmd) {
				v := map[string]any{"backend": "zm-native", "project": p, "server": servedBy(c)}
				if authority != "" {
					v["authority"] = authority
				}
				return writeJSON(stdout, v, true)
			}
			if p == "" {
				p = "every project you read"
			}
			if authority != "" {
				fmt.Fprintf(stdout, "zm-native: %s, served by %s, authority %s\n", p, servedBy(c), authority)
				return nil
			}
			fmt.Fprintf(stdout, "zm-native: %s, served by %s\n", p, servedBy(c))
			return nil
		},
	}
}

// joined is project (a slug or ULID) as this machine joined it, nil for one
// its own daemon holds, or when asked through ZM_HOST (remotes are local).
func joined(ctx context.Context, c *client.Client, project string) *api.Remote {
	if project == "" {
		return nil
	}
	rs, err := c.Remotes(ctx)
	if err != nil {
		return nil
	}
	for i := range rs {
		if rs[i].Project.Slug == project || rs[i].Project.ID == project {
			return &rs[i]
		}
	}
	return nil
}

// ---- dolt ----

func doltCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "dolt", Short: "no-ops under zm (zm holds and syncs the issues)"}
	show := &cobra.Command{
		Use: "show",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			// A real check: the line bd-guard greps for is printed only when zm answers.
			ps, err := orchclient.IssueProjects(ctx, c)
			if err != nil {
				fmt.Fprintf(stdout, "zm-native: issues are served by %s\n  ✗ Server not reachable: %s\n", servedBy(c), errMsg(err))
				return exitErr(1)
			}
			p := scope(ctx, c)
			if asJSON(cmd) {
				return writeJSON(stdout, map[string]any{"backend": "zm-native", "project": p, "server": servedBy(c), "connection_ok": true,
					"projects": len(ps.Projects)}, true)
			}
			fmt.Fprintf(stdout, "Backend: zm-native (no Dolt)\n  Project: %s\n  Server:  %s\n  ✓ Server connection OK\n", p, servedBy(c))
			return nil
		},
	}
	cmd.AddCommand(show,
		noteOnly("push", "nothing to push; zm syncs"),
		noteOnly("pull", "nothing to pull; zm syncs"),
		noteOnly("commit", "nothing to commit; every zm write is already committed"),
		noteOnly("status", "no Dolt working set; every zm write is already committed"))
	cmd.RunE = func(*cobra.Command, []string) error {
		return errors.New("bd dolt: under zm only show, push, pull, commit and status are served (as no-ops)")
	}
	// Other dolt verbs (remote, start, stop, ...) refuse instead of pretending.
	for _, v := range []string{"remote", "start", "stop", "killall", "log", "clean-databases", "set", "test", "config"} {
		cmd.AddCommand(refuseCmd(v, "zm's store has no Dolt server or remotes"))
	}
	return cmd
}

// ---- note ----

func noteCmd() *cobra.Command {
	var stdinFlag bool
	var file string
	cmd := &cobra.Command{
		Use: "note <id> [text...]", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			var text string
			switch {
			case stdinFlag:
				b, err := io.ReadAll(stdin)
				if err != nil {
					return err
				}
				text = strings.TrimRight(string(b), "\n")
			case file != "":
				t, err := readFile(file)
				if err != nil {
					return err
				}
				text = t
			case len(args) > 1:
				text = strings.Join(args[1:], " ")
			default:
				return errors.New("no note text provided (use positional args, --stdin, or --file)")
			}
			if text == "" {
				return errors.New("note text is empty")
			}
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			r, err := route(ctx, c, args[0])
			if err != nil {
				return err
			}
			w, err := orchclient.UpdateIssue(ctx, r.c, r.id, orchapi.UpdateIssueRequest{AppendNotes: &text})
			if err != nil {
				return fmt.Errorf("updating %s: %s", args[0], errMsg(err))
			}
			if asJSON(cmd) {
				return writeJSON(stdout, toBD(w.Issue, false), true)
			}
			fmt.Fprintf(stdout, "✓ Note added to %s\n", fmtFeedback(w.ID, w.Title))
			return nil
		},
	}
	cmd.Flags().BoolVar(&stdinFlag, "stdin", false, "read the note from stdin")
	cmd.Flags().StringVar(&file, "file", "", "read the note from a file")
	cmd.MarkFlagsMutuallyExclusive("stdin", "file")
	return cmd
}

// ---- memories ----

// projectFor is the project a memory or sql call addresses (bd's one
// database): the scope, which must be one project.
func projectFor(ctx context.Context, c *client.Client) (string, error) {
	if p := scope(ctx, c); p != "" {
		return p, nil
	}
	return "", errors.New("no project with native issues resolves here: run bd from the project's repo or set ZM_PROJECT")
}

func memories(ctx context.Context, c *client.Client) (map[string]string, error) {
	p, err := projectFor(ctx, c)
	if err != nil {
		return nil, err
	}
	return orchclient.Memories(ctx, c, p)
}

func truncateMemory(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// slugify is bd's memory key from its text: the first eight words, at most 60 bytes.
func slugify(s string) string {
	s = strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
	parts := strings.SplitN(s, "-", 10)
	if len(parts) > 8 {
		parts = parts[:8]
	}
	slug := strings.Join(parts, "-")
	if len(slug) > 60 {
		slug = strings.TrimRight(slug[:60], "-")
	}
	return slug
}

func rememberCmd() *cobra.Command {
	var key string
	cmd := &cobra.Command{
		Use: `remember "<insight>"`, Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			insight := args[0]
			if strings.TrimSpace(insight) == "" {
				return errors.New("memory content cannot be empty")
			}
			k := key
			if k == "" {
				k = slugify(insight)
			}
			if k == "" {
				return errors.New("could not generate key from content; use --key to specify one")
			}
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			if _, err := projectFor(ctx, c); err != nil {
				return err
			}
			m, err := orchclient.Remember(ctx, c, k, insight)
			if err != nil {
				return fmt.Errorf("storing memory: %s", errMsg(err))
			}
			if asJSON(cmd) {
				return writeJSON(stdout, map[string]string{"key": m.Key, "value": m.Value, "action": m.Action}, true)
			}
			verb := "Remembered"
			if m.Action == "updated" {
				verb = "Updated"
			}
			fmt.Fprintf(stdout, "%s [%s]: %s\n", verb, m.Key, truncateMemory(insight, 80))
			return nil
		},
	}
	cmd.Flags().StringVar(&key, "key", "", "explicit key (an existing key is updated in place)")
	return cmd
}

func memoriesCmd() *cobra.Command {
	return &cobra.Command{
		Use: "memories [search]", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			all, err := memories(ctx, c)
			if err != nil {
				return fmt.Errorf("listing memories: %s", errMsg(err))
			}
			search := ""
			if len(args) > 0 {
				search = strings.ToLower(args[0])
			}
			ms := map[string]string{}
			for k, v := range all {
				if search == "" || strings.Contains(strings.ToLower(k), search) || strings.Contains(strings.ToLower(v), search) {
					ms[k] = v
				}
			}
			if asJSON(cmd) {
				return writeJSON(stdout, ms, true)
			}
			if len(ms) == 0 {
				if search != "" {
					fmt.Fprintf(stdout, "No memories matching %q\n", search)
				} else {
					fmt.Fprintln(stdout, `No memories stored. Use 'bd remember "insight"' to add one.`)
				}
				return nil
			}
			if search != "" {
				fmt.Fprintf(stdout, "Memories matching %q:\n\n", search)
			} else {
				fmt.Fprintf(stdout, "Memories (%d):\n\n", len(ms))
			}
			for _, k := range sortedKeys(ms) {
				fmt.Fprintf(stdout, "  %s\n    %s\n\n", k, truncateMemory(ms[k], 120))
			}
			return nil
		},
	}
}

func sortedKeys(m map[string]string) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func recallCmd() *cobra.Command {
	return &cobra.Command{
		Use: "recall <key>", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			all, err := memories(ctx, c)
			if err != nil {
				return fmt.Errorf("recalling memory: %s", errMsg(err))
			}
			v, ok := all[args[0]]
			if asJSON(cmd) {
				if err := writeJSON(stdout, map[string]any{"key": args[0], "value": v, "found": ok}, true); err != nil {
					return err
				}
				if !ok {
					return exitErr(1)
				}
				return nil
			}
			if !ok {
				fmt.Fprintf(stderr, "No memory with key %q\n", args[0])
				return exitErr(1)
			}
			fmt.Fprintln(stdout, v)
			return nil
		},
	}
}

func forgetCmd() *cobra.Command {
	return &cobra.Command{
		Use: "forget <key>", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			if _, err := projectFor(ctx, c); err != nil {
				return err
			}
			m, err := orchclient.Forget(ctx, c, args[0])
			if notFound(err) {
				if asJSON(cmd) {
					_ = writeJSON(stdout, map[string]string{"key": args[0], "found": "false"}, true)
				} else {
					fmt.Fprintf(stderr, "No memory with key %q\n", args[0])
				}
				return exitErr(1)
			}
			if err != nil {
				return fmt.Errorf("forgetting memory: %s", errMsg(err))
			}
			if asJSON(cmd) {
				return writeJSON(stdout, map[string]string{"key": m.Key, "deleted": "true"}, true)
			}
			fmt.Fprintf(stdout, "Forgot [%s]: %s\n", m.Key, truncateMemory(m.Value, 80))
			return nil
		},
	}
}

// ---- prime ----

const primeTruncation = "[bd prime] If this output is truncated by your host, read the full persisted hook output before continuing; it may contain project memories and session rules not visible in the preview.\n\n"

func primeCmd() *cobra.Command {
	var full, mcp, stealth, export, memoriesOnly, hookJSON bool
	cmd := &cobra.Command{
		Use: "prime",
		RunE: func(cmd *cobra.Command, _ []string) error {
			emit := func(s string) error {
				s = textclean.Clean(s)
				if !hookJSON {
					_, err := io.WriteString(stdout, s)
					return err
				}
				var env struct {
					HookSpecificOutput struct {
						HookEventName     string `json:"hookEventName"`
						AdditionalContext string `json:"additionalContext"`
					} `json:"hookSpecificOutput"`
				}
				env.HookSpecificOutput.HookEventName, env.HookSpecificOutput.AdditionalContext = "SessionStart", s
				return json.NewEncoder(stdout).Encode(env)
			}
			// bd's override order: ./.beads/PRIME.md, $BEADS_DIR/PRIME.md, ~/.config/beads/PRIME.md.
			if !export {
				for _, p := range primePaths() {
					if b, err := os.ReadFile(p); err == nil {
						return emit(string(b))
					}
				}
			}
			// Memories with bd's timeout; prime never fails a hook (bd: exit 0, nothing on stderr).
			scopeQuiet = true
			ctx, cancel := context.WithTimeout(cmd.Context(), primeTimeout())
			defer cancel()
			var ms map[string]string
			if c, err := connect(ctx); err == nil {
				ms, _ = memories(ctx, c)
			}
			var b strings.Builder
			switch {
			case memoriesOnly:
				b.WriteString(primeTruncation)
				if len(ms) == 0 {
					b.WriteString("# Beads Persistent Memories\n\nNo memories stored. Use `bd remember \"insight\"` to add one.\n")
				} else {
					b.WriteString(formatMemories(ms, false))
				}
			case mcp && !full:
				b.WriteString(primeTruncation + "# Beads Issue Tracker Active (zm-native)\n\n")
				if len(ms) > 0 {
					b.WriteString(formatMemories(ms, true) + "\n")
				}
				b.WriteString(primeMCP)
			default:
				b.WriteString(primeTruncation + primeCLI)
				b.WriteString(formatMemories(ms, false))
			}
			_ = stealth
			return emit(b.String())
		},
	}
	f := cmd.Flags()
	f.BoolVar(&full, "full", false, "full CLI output")
	f.BoolVar(&mcp, "mcp", false, "minimal output")
	f.BoolVar(&stealth, "stealth", false, "accepted (zm-native output has no git steps)")
	f.BoolVar(&export, "export", false, "print the default content (ignore PRIME.md)")
	f.BoolVar(&memoriesOnly, "memories-only", false, "only the memories")
	f.BoolVar(&hookJSON, "hook-json", false, "wrap the output in the SessionStart hook JSON envelope")
	return cmd
}

func primePaths() []string {
	out := []string{filepath.Join(".beads", "PRIME.md")}
	if d := getenv("BEADS_DIR"); d != "" {
		out = append(out, filepath.Join(d, "PRIME.md"))
	}
	if d, err := os.UserConfigDir(); err == nil {
		out = append(out, filepath.Join(d, "beads", "PRIME.md"))
	}
	return out
}

func primeTimeout() time.Duration {
	raw := strings.TrimSpace(getenv("BEADS_PRIME_TIMEOUT"))
	for _, s := range []string{raw, raw + "s"} {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return 10 * time.Second
}

func formatMemories(ms map[string]string, compact bool) string {
	if len(ms) == 0 {
		return ""
	}
	var b strings.Builder
	if compact {
		b.WriteString("\n## Memories\n")
		for _, k := range sortedKeys(ms) {
			v := strings.ReplaceAll(ms[k], "\n", " ")
			if len(v) > 150 {
				v = v[:147] + "..."
			}
			fmt.Fprintf(&b, "- **%s**: %s\n", k, v)
		}
		return b.String()
	}
	fmt.Fprintf(&b, "\n## Persistent Memories (%d)\n\n", len(ms))
	b.WriteString("Stored via `bd remember`. Update in place with `bd remember --key <key> \"new content\"`. Search with `bd memories <keyword>`. Remove with `bd forget <key>`.\n\n")
	for _, k := range sortedKeys(ms) {
		fmt.Fprintf(&b, "### %s\n%s\n\n", k, ms[k])
	}
	return b.String()
}

const primeMCP = `# Session close protocol

Before saying "done": bd close <completed-ids>; run checks; report what changed.

## Core rules
- Use bd for task tracking (` + "`bd create`, `bd ready`, `bd close`" + `); claim with ` + "`bd update <id> --claim`" + `.
- Memory: ` + "`bd remember`" + ` for persistent knowledge.
- Issues live in zm: there is nothing to push, pull or sync.
`

const primeCLI = `# Beads Workflow Context (zm-native)

> **Context Recovery**: run ` + "`bd prime`" + ` after compaction, clear, or a new session.
> This project's issues live in zm: ` + "`bd`" + ` is zm's bd-compatible door. Every write is saved at once;
> there is nothing to push, pull, commit or sync (` + "`bd dolt push`/`pull`" + ` are no-ops).

# Session close protocol

[ ] 1. bd close <id1> <id2> ...   (close completed issues)
[ ] 2. run quality gates        (tests, linters, builds when relevant)
[ ] 3. git status               (check what changed; commit or push only with explicit authority)

## Core rules
- Use bd for all task tracking; create the issue before writing code; claim it when you start.
- Memory: ` + "`bd remember`" + ` for persistent knowledge.

## Essential commands

### Finding work
- ` + "`bd ready`" + ` - issues ready to work (no blockers)
- ` + "`bd list --status=open`" + ` / ` + "`bd list --status=in_progress`" + `
- ` + "`bd show <id>`" + ` - detail with dependencies

### Creating and updating
- ` + "`bd create \"Title\" -t task|bug|feature -p 2 -d \"why, what\"`" + `
- ` + "`bd update <id> --claim`" + ` - claim it (atomic; a lease when zm binds the project's issues)
- ` + "`bd update <id> --status=<s> --append-notes \"...\"`" + `
- ` + "`bd close <id> --reason \"...\"`" + `
- ` + "`bd comments add <id> \"...\"`" + `, ` + "`bd note <id> \"...\"`" + `

### Dependencies
- ` + "`bd dep add <issue> <depends-on>`" + `, ` + "`bd blocked`" + `, ` + "`bd children <id>`" + `

### Search
- ` + "`bd search <query>`" + `, ` + "`bd sql --json \"select ...\"`" + ` (read-only)
`

// ---- export ----

func exportCmd() *cobra.Command {
	var output string
	var all, infra, scrub, withMemories, noMemories bool
	cmd := &cobra.Command{
		Use: "export",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			p, err := projectFor(ctx, c)
			if err != nil {
				return err
			}
			its, err := allIssues(ctx, c, p)
			if err != nil {
				return fmt.Errorf("failed to search issues: %s", errMsg(err))
			}
			// bd's default: no infrastructure types, templates (zm lists never
			// carry them) or ephemeral issues; --all keeps them.
			kept := its[:0]
			for _, it := range its {
				if !all && !infra && exportInfra[it.IssueType] {
					continue
				}
				if !all && it.Ephemeral {
					continue
				}
				if scrub && isTestIssue(it.Title) {
					continue
				}
				kept = append(kept, it)
			}
			// bd's order: priority, newest first, id.
			sort.SliceStable(kept, func(i, j int) bool {
				a, b := kept[i], kept[j]
				if a.Priority != b.Priority {
					return a.Priority < b.Priority
				}
				if !a.CreatedAt.Equal(b.CreatedAt) {
					return a.CreatedAt.After(b.CreatedAt)
				}
				return a.ID < b.ID
			})
			var buf bytes.Buffer
			for _, it := range kept {
				w := withCounts(it, true)
				w.Parent = nil
				// bd reads them ORDER BY issue_id: the table's key order, by target.
				sort.SliceStable(w.Dependencies, func(i, j int) bool { return w.Dependencies[i].DependsOnID < w.Dependencies[j].DependsOnID })
				for _, cm := range liveComments(it.Comments) {
					w.Comments = append(w.Comments, toBDComment(cm, false))
				}
				rec := struct {
					Type string `json:"_type"`
					*bdWithCounts
				}{"issue", w}
				b, err := json.Marshal(rec)
				if err != nil {
					return fmt.Errorf("failed to marshal issue %s: %w", it.ID, err)
				}
				buf.Write(textclean.JSONStrings(b))
				buf.WriteByte('\n')
			}
			nMem := 0
			if (withMemories || all) && !noMemories {
				ms, err := orchclient.Memories(ctx, c, p)
				if err != nil {
					return fmt.Errorf("failed to read memories: %s", errMsg(err))
				}
				for _, k := range sortedKeys(ms) {
					b, _ := json.Marshal(map[string]string{"_type": "memory", "key": k, "value": ms[k]})
					buf.Write(textclean.JSONStrings(b))
					buf.WriteByte('\n')
					nMem++
				}
			}
			if output == "" {
				_, err := stdout.Write(buf.Bytes())
				return err
			}
			if err := writeAtomic(output, buf.Bytes()); err != nil {
				return fmt.Errorf("failed to create output file: %w", err)
			}
			if nMem > 0 {
				fmt.Fprintf(stderr, "Exported %d issues and %d memories to %s\n", len(kept), nMem, output)
			} else {
				fmt.Fprintf(stderr, "Exported %d issues to %s\n", len(kept), output)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&output, "output", "o", "", "output file (default: stdout)")
	f.BoolVar(&all, "all", false, "include infrastructure and ephemeral issues, and memories")
	f.BoolVar(&infra, "include-infra", false, "include infrastructure issues (agents, rigs, roles, messages)")
	f.BoolVar(&scrub, "scrub", false, "leave out test/pollution records")
	f.BoolVar(&withMemories, "include-memories", false, "include memories")
	f.BoolVar(&noMemories, "no-memories", false, "leave out memories")
	_ = f.MarkHidden("no-memories")
	return cmd
}

var exportInfra = map[string]bool{"agent": true, "rig": true, "role": true, "message": true}

var testPrefix = regexp.MustCompile(`^(test|benchmark|sample|tmp|temp|debug|dummy)[-_\s]`)

// isTestIssue is bd's pollution test: a title that starts like a test record.
func isTestIssue(title string) bool { return testPrefix.MatchString(strings.ToLower(title)) }

// exportPage is export's page size (tests make it small).
var exportPage = 2000

// allIssues pages every issue of project (every status) with edges and
// comments, by id.
func allIssues(ctx context.Context, c *client.Client, project string) ([]orchapi.Issue, error) {
	var out []orchapi.Issue
	after := ""
	for {
		l, err := orchclient.Issues(ctx, c, orchapi.IssueQuery{Project: project, Status: "all", Sort: "id", Limit: exportPage, IDAfter: after, Deps: true, Comments: true})
		if err != nil {
			return nil, err
		}
		out = append(out, l.Issues...)
		if !l.More || len(l.Issues) == 0 {
			return out, nil
		}
		last := l.Issues[len(l.Issues)-1].ID
		if last <= after {
			return nil, fmt.Errorf("paging stalled after %s", after)
		}
		after = last
	}
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	_ = os.Chmod(tmp.Name(), 0o644)
	return os.Rename(tmp.Name(), path)
}

// ---- sql ----

func sqlCmd() *cobra.Command {
	var csvOut bool
	cmd := &cobra.Command{
		Use: "sql <query>", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			head := strings.ToUpper(strings.TrimSpace(args[0]))
			if !strings.HasPrefix(head, "SELECT") && !strings.HasPrefix(head, "WITH") {
				return errors.New("bd sql under zm is read-only: one SELECT (or WITH) statement over issues, dependencies, labels, comments, events and config; write through bd's verbs")
			}
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			p, err := projectFor(ctx, c)
			if err != nil {
				return err
			}
			r, err := orchclient.IssueSQL(ctx, c, p, args[0])
			if err != nil {
				return errors.New(errMsg(err))
			}
			rows := make([]map[string]any, 0, len(r.Rows))
			for _, vals := range r.Rows {
				m := make(map[string]any, len(r.Columns))
				for i, col := range r.Columns {
					if i < len(vals) {
						m[col] = vals[i]
					}
				}
				rows = append(rows, m)
			}
			switch {
			case asJSON(cmd):
				return writeJSON(stdout, rows, false)
			case csvOut:
				w := csv.NewWriter(stdout)
				_ = w.Write(r.Columns)
				for _, row := range rows {
					rec := make([]string, len(r.Columns))
					for i, col := range r.Columns {
						rec[i] = cell(row[col])
					}
					_ = w.Write(rec)
				}
				w.Flush()
				return w.Error()
			}
			printTable(stdout, r.Columns, rows)
			return nil
		},
	}
	cmd.Flags().BoolVar(&csvOut, "csv", false, "CSV output")
	return cmd
}

// cell is fmt's %v of a value as bd printed it (JSON numbers decode as float64).
func cell(v any) string {
	switch t := v.(type) {
	case nil:
		return "<nil>"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
	}
	return fmt.Sprintf("%v", v)
}

// printTable is bd sql's table: columns at most 60 wide, then a row count.
func printTable(w io.Writer, cols []string, rows []map[string]any) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "(0 rows)")
		return
	}
	widths := make([]int, len(cols))
	for i, c := range cols {
		widths[i] = len(c)
	}
	for _, r := range rows {
		for i, c := range cols {
			if n := len(cell(r[c])); n > widths[i] {
				widths[i] = n
			}
		}
	}
	for i := range widths {
		if widths[i] > 60 {
			widths[i] = 60
		}
	}
	line := func(vals []string) {
		for i, v := range vals {
			if i > 0 {
				fmt.Fprint(w, " | ")
			}
			fmt.Fprintf(w, "%-*s", widths[i], v)
		}
		fmt.Fprintln(w)
	}
	line(cols)
	for i := range cols {
		if i > 0 {
			fmt.Fprint(w, "-+-")
		}
		fmt.Fprint(w, strings.Repeat("-", widths[i]))
	}
	fmt.Fprintln(w)
	for _, r := range rows {
		vals := make([]string, len(cols))
		for i, c := range cols {
			s := cell(r[c])
			if len(s) > 60 {
				s = s[:57] + "..."
			}
			vals[i] = s
		}
		line(vals)
	}
	fmt.Fprintf(w, "(%d rows)\n", len(rows))
}
