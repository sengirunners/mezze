package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// ---- show ----

func showCmd() *cobra.Command {
	var idFlags []string
	var withDependents, withComments bool
	cmd := &cobra.Command{
		Use: "show [id...]", Aliases: []string{"view"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			ids := append(append([]string{}, args...), idFlags...)
			if len(ids) == 0 {
				return errors.New("no issue id given")
			}
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			var found []orchapi.IssueDetail
			for _, raw := range ids {
				r, err := route(ctx, c, raw)
				var d orchapi.IssueDetail
				if err == nil {
					d, err = orchclient.Issue(ctx, r.c, r.id)
				}
				if notFound(err) {
					fmt.Fprintln(stderr, missing(raw, err))
					continue
				}
				if err != nil {
					return err
				}
				d.Comments = liveComments(d.Comments)
				found = append(found, d)
			}
			if len(found) == 0 {
				if asJSON(cmd) {
					_ = writeJSON(stdout, map[string]any{"error": "no issues found matching the provided IDs"}, true)
					return exitErr(1)
				}
				return errors.New("no issues found matching the provided IDs")
			}
			if asJSON(cmd) {
				out := make([]*bdDetails, 0, len(found))
				for _, d := range found {
					out = append(out, details(d, withDependents, withComments))
				}
				return writeJSON(stdout, out, false)
			}
			for i, d := range found {
				if i > 0 {
					fmt.Fprintf(stdout, "\n%s\n", strings.Repeat("─", 60))
				}
				printShow(stdout, d)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&idFlags, "id", nil, "issue id (repeatable)")
	f.BoolVar(&withDependents, "include-dependents", false, "include dependents in JSON")
	f.BoolVar(&withComments, "include-comments", false, "include comments in JSON")
	f.Bool("short", false, "accepted")
	f.Bool("long", false, "accepted")
	f.Bool("local-time", false, "accepted")
	return cmd
}

// ---- list, children, ready, search ----

type listFlags struct {
	status, prio, assignee, typ, title, titleContains, descContains, notesContains, ids, sort, parent string
	labels, labelAny                                                                                  []string
	limit                                                                                             int
	all, reverse, noAssignee, flat, long, ready, pretty                                               bool
}

func (lf *listFlags) bind(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVarP(&lf.status, "status", "s", "", "status filter (comma-separated; all = every status)")
	f.StringVarP(&lf.prio, "priority", "p", "", "priority 0-4 or P0-P4")
	f.StringVarP(&lf.assignee, "assignee", "a", "", "assignee")
	f.StringVarP(&lf.typ, "type", "t", "", "issue type")
	f.StringSliceVarP(&lf.labels, "label", "l", nil, "labels (all must match)")
	f.StringSliceVar(&lf.labelAny, "label-any", nil, "labels (any matches)")
	f.StringVar(&lf.title, "title", "", "title contains")
	f.StringVar(&lf.titleContains, "title-contains", "", "title contains")
	f.StringVar(&lf.descContains, "desc-contains", "", "description contains")
	f.StringVar(&lf.notesContains, "notes-contains", "", "notes contain")
	f.StringVar(&lf.ids, "id", "", "ids (comma-separated)")
	f.IntVarP(&lf.limit, "limit", "n", 50, "limit (0 = all)")
	f.BoolVar(&lf.all, "all", false, "include closed")
	f.StringVar(&lf.sort, "sort", "", "priority|created|updated|closed|status|id|title|type|assignee")
	f.BoolVarP(&lf.reverse, "reverse", "r", false, "reverse the order")
	f.BoolVar(&lf.noAssignee, "no-assignee", false, "only unassigned")
	f.StringVar(&lf.parent, "parent", "", "children of this issue")
	f.BoolVar(&lf.ready, "ready", false, "only ready work")
	f.BoolVar(&lf.flat, "flat", false, "one line per issue")
	f.BoolVar(&allProjects, "all-projects", false, "every project you read, not bd's one")
	f.BoolVar(&lf.long, "long", false, "long format")
	f.BoolVar(&lf.pretty, "pretty", false, "accepted")
	f.Bool("tree", true, "tree view (default)")
	f.Bool("no-pager", false, "accepted")
}

func (lf *listFlags) query(cmd *cobra.Command) (orchapi.IssueQuery, int, error) {
	q := orchapi.IssueQuery{Status: lf.status, Assignee: lf.assignee, Type: normalizeType(lf.typ), Labels: lf.labels, LabelAny: lf.labelAny,
		TitleContains: lf.titleContains, DescContains: lf.descContains, NotesContains: lf.notesContains, Sort: lf.sort, Reverse: lf.reverse,
		NoAssignee: lf.noAssignee, Parent: lf.parent, Ready: lf.ready, All: lf.all, Deps: true}
	if lf.title != "" {
		q.TitleContains = lf.title
	}
	if lf.ids != "" {
		q.IDs = strings.Split(lf.ids, ",")
	}
	if lf.prio != "" {
		p, err := parsePriority(lf.prio)
		if err != nil {
			return q, 0, err
		}
		q.Priority = &p
	}
	if q.Status == "all" {
		q.Status, q.All = "", true
	}
	limit := lf.limit
	if !cmd.Flags().Changed("limit") {
		switch {
		case lf.all:
			limit = 0
		case agentMode():
			limit = 20
		}
	}
	q.Limit = limit
	if limit == 0 {
		q.Limit = unlimited
	}
	return q, limit, nil
}

// unlimited is "no limit" without widening the status filter.
const unlimited = 1 << 30

func normalizeType(t string) string {
	switch t = strings.ToLower(strings.TrimSpace(t)); t {
	case "feat", "enhancement":
		return "feature"
	case "dec", "adr":
		return "decision"
	case "mr":
		return "merge-request"
	case "mol":
		return "molecule"
	}
	return t
}

func listCmd() *cobra.Command {
	lf := &listFlags{}
	cmd := &cobra.Command{
		Use: "list",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return fmt.Errorf("list takes no arguments (did you mean `bd %s`?)", args[0])
			}
			return runList(cmd, lf)
		},
	}
	lf.bind(cmd)
	return cmd
}

func childrenCmd() *cobra.Command {
	lf := &listFlags{}
	cmd := &cobra.Command{
		Use: "children <parent>", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			lf.parent, lf.status, lf.limit = args[0], "all", 50
			if agentMode() {
				lf.limit = 20
			}
			return runList(cmd, lf)
		},
	}
	return cmd
}

func runList(cmd *cobra.Command, lf *listFlags) error {
	ctx := cmd.Context()
	q, limit, err := lf.query(cmd)
	if err != nil {
		return err
	}
	parentFlag = q.Parent
	if err := oneScope(); err != nil {
		return err
	}
	c, err := connect(ctx)
	if err != nil {
		return err
	}
	if q.Parent != "" {
		if q.Parent, q.Project, err = parentOf(ctx, c, q.Parent); err != nil {
			return err
		}
	}
	l, err := listIssues(ctx, c, q)
	if err != nil {
		return err
	}
	if asJSON(cmd) {
		out := make([]*bdWithCounts, 0, len(l.Issues))
		for _, it := range l.Issues {
			out = append(out, withCounts(it, true))
		}
		if err := writeJSON(stdout, out, false); err != nil {
			return err
		}
	} else {
		printList(lf, l.Issues)
	}
	if l.More && limit > 0 {
		fmt.Fprintf(stderr, "\nShowing %d issues; more results matched but were hidden by --limit. Use --limit 0 for all, or --limit N to raise the cap.\n", len(l.Issues))
	}
	return nil
}

func printList(lf *listFlags, its []orchapi.Issue) {
	if len(its) == 0 {
		fmt.Fprintln(stdout, "No issues found.")
		return
	}
	switch {
	case lf.long:
		fmt.Fprintf(stdout, "\nFound %d issues:\n\n", len(its))
		for _, it := range its {
			fmt.Fprintf(stdout, "%s [● P%d] [%s] %s\n  %s\n", it.ID, it.Priority, it.IssueType, it.Status, it.Title)
			if it.Assignee != "" {
				fmt.Fprintf(stdout, "  Assignee: %s\n", it.Assignee)
			}
			if it.Description != "" {
				fmt.Fprintf(stdout, "  Description: %s\n", it.Description)
			}
			if len(it.Labels) > 0 {
				fmt.Fprintf(stdout, "  Labels: [%s]\n", strings.Join(it.Labels, " "))
			}
			fmt.Fprintln(stdout)
		}
		return
	case lf.flat:
		for _, it := range its {
			if agentMode() {
				line := it.ID + ": " + it.Title
				if it.Parent != "" {
					line += " (parent: " + it.Parent + ")"
				}
				fmt.Fprintln(stdout, line)
				continue
			}
			fmt.Fprintln(stdout, flatLine(it))
		}
	default:
		printTree(stdout, its)
	}
	open, prog := 0, 0
	for _, it := range its {
		switch it.Status {
		case orchapi.IssueOpen:
			open++
		case orchapi.IssueInProgress:
			prog++
		}
	}
	fmt.Fprintf(stdout, "\n%s\nTotal: %d issues (%d open, %d in progress)\n\n%s\n", strings.Repeat("-", 80), len(its), open, prog, statusLegend)
}

func readyCmd() *cobra.Command {
	var prio int
	var assignee, sortBy, typ, parent string
	var labels, labelAny []string
	var limit int
	var unassigned, plain, claim bool
	cmd := &cobra.Command{
		Use: "ready",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			q := orchapi.IssueQuery{Ready: true, Assignee: assignee, NoAssignee: unassigned, Labels: labels, LabelAny: labelAny, Type: normalizeType(typ),
				Parent: parent, Limit: limit, Deps: true, Count: true}
			switch sortBy {
			case "", "priority", "hybrid":
			case "oldest":
				q.Sort = "oldest"
			default:
				return fmt.Errorf("invalid sort %q (priority, hybrid, oldest)", sortBy)
			}
			if cmd.Flags().Changed("priority") {
				q.Priority = &prio
			}
			if limit == 0 {
				q.Limit = unlimited
			}
			parentFlag = q.Parent
			if err := oneScope(); err != nil {
				return err
			}
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			if q.Parent != "" {
				if q.Parent, q.Project, err = parentOf(ctx, c, q.Parent); err != nil {
					return err
				}
			}
			if claim {
				if assignee != "" {
					return errors.New("--claim cannot be combined with --assignee")
				}
				return readyClaim(cmd, c, q)
			}
			l, err := listIssues(ctx, c, q)
			if err != nil {
				return err
			}
			if asJSON(cmd) {
				out := make([]*bdWithCounts, 0, len(l.Issues))
				for _, it := range l.Issues {
					out = append(out, withCounts(it, true))
				}
				if err := writeJSON(stdout, out, false); err != nil {
					return err
				}
				if l.More {
					fmt.Fprintf(stderr, "Showing %d of %d ready issues. Use --limit 0 for all, or --limit N to raise the cap.\n", len(l.Issues), l.Total)
				}
				return nil
			}
			if len(l.Issues) == 0 {
				fmt.Fprint(stdout, "\n✨ No ready work found (all issues have blocking dependencies)\n\n")
				return nil
			}
			if plain {
				fmt.Fprintf(stdout, "\n📋 Ready work (%d issues with no active blockers):\n\n", len(l.Issues))
				for i, it := range l.Issues {
					fmt.Fprintf(stdout, "%d. [● P%d] [%s] %s: %s\n", i+1, it.Priority, it.IssueType, it.ID, it.Title)
					if it.EstimatedMinutes != nil {
						fmt.Fprintf(stdout, "   Estimate: %d min\n", *it.EstimatedMinutes)
					}
					if it.Assignee != "" {
						fmt.Fprintf(stdout, "   Assignee: %s\n", it.Assignee)
					}
				}
				fmt.Fprintln(stdout)
			} else {
				for _, it := range l.Issues {
					fmt.Fprintln(stdout, listLine(it))
				}
				fmt.Fprintf(stdout, "\n%s\nReady: %d issues with no active blockers\n\n%s\n", strings.Repeat("-", 80), len(l.Issues), statusLegend)
			}
			if l.More {
				fmt.Fprintf(stdout, "Showing %d of %d ready issues. Use -n to show more.\n\n", len(l.Issues), l.Total)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.IntVarP(&limit, "limit", "n", 100, "limit (0 = all)")
	f.IntVarP(&prio, "priority", "p", 0, "priority")
	f.StringVarP(&assignee, "assignee", "a", "", "assignee")
	f.BoolVarP(&unassigned, "unassigned", "u", false, "only unassigned")
	f.StringVarP(&sortBy, "sort", "s", "priority", "priority|hybrid|oldest")
	f.StringSliceVarP(&labels, "label", "l", nil, "labels (all must match)")
	f.StringSliceVar(&labelAny, "label-any", nil, "labels (any matches)")
	f.StringVarP(&typ, "type", "t", "", "issue type")
	f.StringVar(&parent, "parent", "", "descendants of this issue")
	f.Bool("pretty", true, "default view")
	f.BoolVar(&plain, "plain", false, "numbered list")
	f.BoolVar(&claim, "claim", false, "claim the first ready issue")
	f.BoolVar(&allProjects, "all-projects", false, "every project you read, not bd's one")
	return cmd
}

func readyClaim(cmd *cobra.Command, c *client.Client, q orchapi.IssueQuery) error {
	ctx := cmd.Context()
	q.Limit, q.All = 20, false
	l, err := listIssues(ctx, c, q)
	if err != nil {
		return err
	}
	for _, it := range l.Issues {
		r, err := route(ctx, c, it.ID)
		if err != nil {
			fmt.Fprintf(stderr, "bd: skipping %s: %s\n", it.ID, errMsg(err))
			continue // like a conflict: try the next
		}
		w, err := orchclient.ClaimIssueAs(ctx, r.c, r.id, bdActor(r.c))
		if api.IsCode(err, api.CodeConflict) {
			continue // someone took it first: try the next
		}
		if err != nil {
			return err
		}
		if asJSON(cmd) {
			got := w.Issue
			got.Dependencies = it.Dependencies
			return writeJSON(stdout, []*bdWithCounts{withCounts(got, true)}, false)
		}
		fmt.Fprintf(stdout, "✓ Claimed issue: %s\n", fmtFeedback(w.ID, w.Title))
		return nil
	}
	if asJSON(cmd) {
		return writeJSON(stdout, []*bdWithCounts{}, false)
	}
	fmt.Fprint(stdout, "\n○ No ready work to claim\n\n")
	return nil
}

// ---- close, reopen ----

func closeCmd() *cobra.Command {
	var reasons []string
	var reasonFile, session string
	var force bool
	cmd := &cobra.Command{
		Use: "close [id...]", Aliases: []string{"done"},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if cmd.CalledAs() == "done" && len(args) >= 2 && len(reasons) == 0 {
				reasons, args = []string{args[len(args)-1]}, args[:len(args)-1]
			}
			if len(args) == 0 {
				return errors.New("no issue id given")
			}
			if reasonFile != "" {
				if len(reasons) > 0 {
					return errors.New("cannot combine --reason and --reason-file")
				}
				r, err := readFile(reasonFile)
				if err != nil {
					return err
				}
				reasons = []string{strings.TrimRight(r, "\n")}
			}
			if len(reasons) > 1 && len(reasons) != len(args) {
				return fmt.Errorf("got %d reasons for %d issues (give one reason, or one per issue)", len(reasons), len(args))
			}
			if session == "" {
				session = getenv("CLAUDE_SESSION_ID")
			}
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			var out []*bdIssue
			closed := 0
			for i, raw := range args {
				reason := "Closed"
				switch {
				case len(reasons) == 1:
					reason = reasons[0]
				case len(reasons) > 1:
					reason = reasons[i]
				}
				r, err := route(ctx, c, raw)
				var w orchclient.IssueWrite
				if err == nil {
					w, err = orchclient.CloseIssue(ctx, r.c, r.id, orchapi.CloseIssueRequest{Reason: reason, Session: session, Force: force, Fence: fenceOf(cmd)})
				}
				if err != nil {
					if notFound(err) {
						fmt.Fprintln(stderr, missing(raw, err))
					} else {
						fmt.Fprintf(stderr, "Error closing %s: %s\n", raw, errMsg(err))
					}
					continue
				}
				closed++
				if asJSON(cmd) {
					out = append(out, toBD(w.Issue, false))
				} else {
					fmt.Fprintf(stdout, "✓ Closed %s: %s\n", fmtFeedback(w.ID, w.Title), w.CloseReason)
				}
			}
			if asJSON(cmd) && len(out) > 0 {
				if err := writeJSON(stdout, out, false); err != nil {
					return err
				}
			}
			if closed == 0 {
				return exitErr(1)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringArrayVarP(&reasons, "reason", "r", nil, "reason (repeat once per issue)")
	f.StringArrayVar(&reasons, "resolution", nil, "alias of --reason")
	f.StringArrayVarP(&reasons, "message", "m", nil, "alias of --reason")
	f.StringArrayVar(&reasons, "comment", nil, "alias of --reason")
	f.StringVar(&reasonFile, "reason-file", "", "read the reason from a file (- = stdin)")
	f.BoolVarP(&force, "force", "f", false, "close despite open children or blockers")
	f.StringVar(&session, "session", "", "closing session (default $CLAUDE_SESSION_ID)")
	fenceFlag(cmd, true)
	for _, h := range []string{"resolution", "message", "comment"} {
		_ = f.MarkHidden(h)
	}
	return cmd
}

func reopenCmd() *cobra.Command {
	var reason string
	cmd := &cobra.Command{
		Use: "reopen [id...]", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			var out []*bdIssue
			failed := false
			for _, raw := range args {
				r, err := route(ctx, c, raw)
				var w orchclient.IssueWrite
				if err == nil {
					w, err = orchclient.ReopenIssue(ctx, r.c, r.id, orchapi.ReopenIssueRequest{Reason: reason, Fence: fenceOf(cmd)})
				}
				if err != nil {
					failed = true
					fmt.Fprintf(stderr, "Error reopening %s: %s\n", raw, errMsg(err))
					continue
				}
				if w.Noop {
					fmt.Fprintf(stderr, "%s is already open\n", w.ID)
					continue
				}
				if asJSON(cmd) {
					out = append(out, toBD(w.Issue, false))
					continue
				}
				line := "↻ Reopened " + w.ID
				if reason != "" {
					line += ": " + reason
				}
				fmt.Fprintln(stdout, line)
			}
			if asJSON(cmd) && len(out) > 0 {
				if err := writeJSON(stdout, out, false); err != nil {
					return err
				}
			}
			if failed {
				return exitErr(1)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&reason, "reason", "r", "", "reason (added as a comment)")
	fenceFlag(cmd, true)
	return cmd
}

// ---- comments ----

func commentsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "comments <id>", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if args[0] == "list" {
				return errors.New("use `bd comments <id>` to list comments")
			}
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			r, err := route(ctx, c, args[0])
			if err != nil {
				return err
			}
			id := r.id
			cs, err := orchclient.IssueComments(ctx, r.c, id)
			if err != nil {
				return err
			}
			cs = liveComments(cs)
			if asJSON(cmd) {
				out := make([]*bdComment, 0, len(cs))
				for _, cm := range cs {
					out = append(out, toBDComment(cm, false))
				}
				return writeJSON(stdout, out, false)
			}
			if len(cs) == 0 {
				fmt.Fprintf(stdout, "No comments on %s\n", id)
				return nil
			}
			fmt.Fprintf(stdout, "\nComments on %s:\n\n", id)
			for _, cm := range cs {
				fmt.Fprintf(stdout, "[%s] at %s\n", cm.Author, cm.CreatedAt.UTC().Format("2006-01-02 15:04"))
				for _, line := range strings.Split(cm.Text, "\n") {
					fmt.Fprintf(stdout, "  %s\n", line)
				}
				fmt.Fprintln(stdout)
			}
			return nil
		},
	}
	cmd.Flags().Bool("local-time", false, "accepted")
	var file, author string
	add := &cobra.Command{
		Use: "add <id> [text]", Args: cobra.MinimumNArgs(1), // bd uses only the first text argument
		RunE: func(cmd *cobra.Command, args []string) error {
			text := ""
			if len(args) > 1 {
				text = args[1]
			}
			if file != "" {
				t, err := readFile(file)
				if err != nil {
					return err
				}
				text = t
			}
			if text == "" && file == "" {
				return errors.New("comment text required (use -f to read from file)")
			}
			if strings.TrimSpace(text) == "" {
				return errors.New("comment text cannot be empty")
			}
			if author != "" {
				fmt.Fprintln(stderr, "bd-zm: --author ignored; the author is your zm identity")
			}
			cm, _, err := addComment(cmd, args[0], text)
			if err != nil {
				return err
			}
			if asJSON(cmd) {
				return writeJSON(stdout, toBDComment(cm, true), true)
			}
			fmt.Fprintf(stdout, "Comment added to %s\n", cm.IssueID)
			return nil
		},
	}
	add.Flags().StringVarP(&file, "file", "f", "", "read the comment from a file")
	add.Flags().StringVarP(&author, "author", "a", "", "ignored: the author is your zm identity")
	fenceFlag(add, false)
	cmd.AddCommand(add)
	return cmd
}

func addComment(cmd *cobra.Command, raw, text string) (orchapi.IssueComment, string, error) {
	ctx := cmd.Context()
	c, err := connect(ctx)
	if err != nil {
		return orchapi.IssueComment{}, "", err
	}
	r, err := route(ctx, c, raw)
	if err != nil {
		return orchapi.IssueComment{}, "", err
	}
	id, c := r.id, r.c
	cm, err := orchclient.CommentIssue(ctx, c, id, orchapi.IssueCommentRequest{Text: text, Fence: fenceOf(cmd)})
	if err != nil {
		return cm, "", err
	}
	title := ""
	if d, err := orchclient.Issues(ctx, c, orchapi.IssueQuery{IDs: []string{id}, All: true, Limit: 1}); err == nil && len(d.Issues) == 1 {
		title = d.Issues[0].Title
	}
	return cm, title, nil
}

func commentCmd() *cobra.Command {
	var file string
	var fromStdin bool
	cmd := &cobra.Command{
		Use: "comment <id> [text...]", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := strings.Join(args[1:], " ")
			switch {
			case file != "" && fromStdin:
				return errors.New("--stdin and --file cannot be combined")
			case fromStdin:
				t, err := readFile("-")
				if err != nil {
					return err
				}
				text = strings.TrimRight(t, "\n")
			case file != "":
				t, err := readFile(file)
				if err != nil {
					return err
				}
				text = t
			}
			if strings.TrimSpace(text) == "" {
				return errors.New("comment text cannot be empty")
			}
			cm, title, err := addComment(cmd, args[0], text)
			if err != nil {
				return err
			}
			if asJSON(cmd) {
				return writeJSON(stdout, toBDComment(cm, true), true)
			}
			fmt.Fprintf(stdout, "✓ Comment added to %s\n", fmtFeedback(cm.IssueID, title))
			return nil
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "read the comment from a file (- = stdin)")
	cmd.Flags().BoolVar(&fromStdin, "stdin", false, "read the comment from stdin")
	fenceFlag(cmd, false)
	return cmd
}

// fenceFlag adds zm's --fence: the write is one under your lease with this
// fence (update --claim --json prints it as lease_fence), refused once that
// lease has gone (SPEC "Leases": fencing). Not a bd flag.
//
// ids: the command takes several ids, and a fence names one lease, so
// --fence with more than one id is refused before anything is written.
func fenceFlag(cmd *cobra.Command, ids bool) {
	cmd.Flags().Int64("fence", 0, "zm: your lease's fence (update --claim --json prints lease_fence); refused if that lease has gone")
	if !ids {
		return
	}
	prev := cmd.Args
	cmd.Args = func(c *cobra.Command, args []string) error {
		if prev != nil {
			if err := prev(c, args); err != nil {
				return err
			}
		}
		if fenceOf(c) != 0 && len(args) > 1 {
			return errors.New("--fence names one lease: give one id")
		}
		return nil
	}
}

// fenceOf is --fence (0 when unset or not a flag of cmd).
func fenceOf(cmd *cobra.Command) int64 {
	n, _ := cmd.Flags().GetInt64("fence")
	return n
}

// ---- stats, count, blocked ----

func statsCmd(name string) *cobra.Command {
	cmd := &cobra.Command{
		Use: name,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			s, err := orchclient.IssueStats(ctx, c, scope(ctx, c))
			if err != nil {
				return err
			}
			if asJSON(cmd) {
				return writeJSON(stdout, map[string]any{"summary": map[string]any{"average_lead_time_hours": 0, "blocked_issues": s.BlockedIssues,
					"closed_issues": s.ClosedIssues, "deferred_issues": s.DeferredIssues, "epics_eligible_for_closure": 0,
					"in_progress_issues": s.InProgressIssues, "open_issues": s.OpenIssues, "pinned_issues": s.PinnedIssues,
					"ready_issues": s.ReadyIssues, "total_issues": s.TotalIssues}}, true)
			}
			fmt.Fprintf(stdout, "\n📊 Issue Database Status\n\nSummary:\n  Total Issues:           %d\n  Open:                   %d\n  In Progress:            %d\n  Blocked:                %d\n  Closed:                 %d\n  Ready to Work:          %d\n",
				s.TotalIssues, s.OpenIssues, s.InProgressIssues, s.BlockedIssues, s.ClosedIssues, s.ReadyIssues)
			if s.PinnedIssues > 0 {
				fmt.Fprintf(stdout, "\nExtended:\n  Pinned:                 %d\n", s.PinnedIssues)
			}
			fmt.Fprint(stdout, "\nFor more details, use 'bd list' to see individual issues.\n\n")
			return nil
		},
	}
	cmd.Flags().Bool("all", false, "accepted")
	cmd.Flags().Bool("no-activity", false, "accepted")
	return cmd
}

func countCmd() *cobra.Command {
	var status, assignee, typ string
	var labels []string
	var prio int
	var byStatus, byPriority bool
	cmd := &cobra.Command{
		Use: "count",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			base := orchapi.IssueQuery{Assignee: assignee, Type: normalizeType(typ), Labels: labels, Limit: 1, Count: true}
			if cmd.Flags().Changed("priority") {
				base.Priority = &prio
			}
			count := func(q orchapi.IssueQuery) (int, error) {
				l, err := listIssues(ctx, c, q)
				return l.Total, err
			}
			if !byStatus && !byPriority {
				q := base
				if status == "" || status == "all" {
					q.All = true
					q.Status = "all"
				} else {
					q.Status = status
				}
				n, err := count(q)
				if err != nil {
					return err
				}
				if asJSON(cmd) {
					return writeJSON(stdout, map[string]any{"count": n}, true)
				}
				fmt.Fprintln(stdout, n)
				return nil
			}
			type group struct {
				Count int    `json:"count"`
				Group string `json:"group"`
			}
			var groups []group
			total := 0
			if byStatus {
				for _, st := range orchapi.IssueStatuses {
					q := base
					q.Status = st
					n, err := count(q)
					if err != nil {
						return err
					}
					if n > 0 {
						groups = append(groups, group{n, st})
						total += n
					}
				}
			} else {
				for p := 0; p <= 4; p++ {
					q := base
					pp := p
					q.Priority, q.Status = &pp, "all"
					n, err := count(q)
					if err != nil {
						return err
					}
					if n > 0 {
						groups = append(groups, group{n, fmt.Sprintf("P%d", p)})
						total += n
					}
				}
			}
			sort.Slice(groups, func(i, j int) bool { return groups[i].Group < groups[j].Group })
			if asJSON(cmd) {
				return writeJSON(stdout, map[string]any{"groups": groups, "total": total}, true)
			}
			fmt.Fprintf(stdout, "Total: %d\n\n", total)
			for _, g := range groups {
				fmt.Fprintf(stdout, "%s: %d\n", g.Group, g.Count)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVarP(&status, "status", "s", "", "status")
	f.IntVarP(&prio, "priority", "p", 0, "priority")
	f.StringVarP(&assignee, "assignee", "a", "", "assignee")
	f.StringVarP(&typ, "type", "t", "", "issue type")
	f.StringSliceVarP(&labels, "label", "l", nil, "labels")
	f.BoolVar(&byStatus, "by-status", false, "group by status")
	f.BoolVar(&byPriority, "by-priority", false, "group by priority")
	return cmd
}

func blockedCmd() *cobra.Command {
	var parent string
	cmd := &cobra.Command{
		Use: "blocked",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			q := orchapi.IssueQuery{Blocked: true, All: true, Deps: true}
			if parent != "" {
				if q.Parent, q.Project, err = parentOf(ctx, c, parent); err != nil {
					return err
				}
			}
			l, err := listIssues(ctx, c, q)
			if err != nil {
				return err
			}
			type blockedIssue struct {
				bdIssue
				BlockedByCount int      `json:"blocked_by_count"`
				BlockedBy      []string `json:"blocked_by"`
			}
			var targets []string
			for _, it := range l.Issues {
				for _, d := range it.Dependencies {
					if d.Type == orchapi.DepBlocks || d.Type == orchapi.DepConditionalBlocks {
						targets = append(targets, d.DependsOnID)
					}
				}
			}
			open := map[string]bool{}
			if len(targets) > 0 {
				tl, err := orchclient.Issues(ctx, c, orchapi.IssueQuery{IDs: targets, Status: "open,in_progress,in_review,blocked,deferred,hooked", Limit: len(targets)})
				if err != nil {
					return err
				}
				for _, t := range tl.Issues {
					open[t.ID] = true
				}
			}
			out := []*blockedIssue{}
			for _, it := range l.Issues {
				b := &blockedIssue{bdIssue: *toBD(it, false), BlockedBy: []string{}}
				for _, d := range it.Dependencies {
					if (d.Type == orchapi.DepBlocks || d.Type == orchapi.DepConditionalBlocks) && open[d.DependsOnID] {
						b.BlockedBy = append(b.BlockedBy, d.DependsOnID)
					}
				}
				if len(b.BlockedBy) == 0 && it.Parent != "" {
					b.BlockedBy = []string{it.Parent}
				}
				b.BlockedByCount = len(b.BlockedBy)
				out = append(out, b)
			}
			if asJSON(cmd) {
				return writeJSON(stdout, out, false)
			}
			if len(out) == 0 {
				fmt.Fprint(stdout, "\n✨ No blocked issues\n\n")
				return nil
			}
			fmt.Fprintf(stdout, "\n🚫 Blocked issues (%d):\n\n", len(out))
			for _, b := range out {
				fmt.Fprintf(stdout, "[● P%d] %s: %s\n  Blocked by %d open dependencies: [%s]\n\n", b.Priority, b.ID, b.Title, b.BlockedByCount, strings.Join(b.BlockedBy, " "))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&parent, "parent", "", "only under this issue")
	return cmd
}

// liveComments leaves out deleted comments: bd has no tombstones.
func liveComments(cs []orchapi.IssueComment) []orchapi.IssueComment {
	out := cs[:0:0]
	for _, c := range cs {
		if !c.Deleted {
			out = append(out, c)
		}
	}
	return out
}
