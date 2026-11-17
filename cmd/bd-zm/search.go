package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
)

// ---- search (ranked: SPEC "Native issues", bd door: Search) ----

func searchCmd() *cobra.Command {
	var query, status, assignee, typ, parent, sortBy, descContains, notesContains string
	var labels, labelAny []string
	var limit int
	var reverse bool
	cmd := &cobra.Command{
		Use:   "search [query]",
		Short: "Search issues: ranked by relevance (bm25 over title, description, notes), best first",
		Long: `Search issues, best match first: every word must match (the last also as a prefix), ranked by
bm25 over title (weighted most), description and notes; each result shows its score. Use it before filing to
find an existing issue. Filters: --status (default: all but closed; all = with closed), --label, --type,
--parent, --assignee. Default limit 10 (--limit 0: every match). A query too common to rank within zm's
16 ms budget (a common word over every status) comes newest first, unranked: add words or filters. --sort, or a bare issue id,
searches bd's way instead (substring of title, id or external ref).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if query == "" {
				query = strings.Join(args, " ")
			}
			if strings.TrimSpace(query) == "" {
				_ = cmd.Help()
				return errors.New("search query required")
			}
			parentFlag = parent
			if err := oneScope(); err != nil {
				return err
			}
			c, err := connect(ctx)
			if err != nil {
				return err
			}
			rank := sortBy == "" && !idLike(ctx, c, query)
			q := orchapi.IssueQuery{Assignee: assignee, Type: normalizeType(typ), Labels: labels, LabelAny: labelAny, Parent: parent, Sort: sortBy,
				Reverse: reverse, DescContains: descContains, NotesContains: notesContains, Limit: limit, Count: true}
			if rank {
				q.Q, q.Sort = query, "rank"
				if !cmd.Flags().Changed("limit") {
					q.Limit = 10
				}
			} else {
				q.Substr = query
			}
			switch status {
			case "":
				q.Status = "open,in_progress,in_review,blocked,deferred,pinned,hooked" // bd search hides only closed
			case "all":
				q.All = true
			default:
				q.Status = status
			}
			if q.Limit == 0 {
				q.Limit = unlimited
			}
			if q.Parent != "" {
				if q.Parent, q.Project, err = parentOf(ctx, c, q.Parent); err != nil {
					return err
				}
			}
			l, err := listIssues(ctx, c, q)
			if rank && api.IsCode(err, api.CodeBadRequest) && strings.Contains(err.Error(), `invalid sort "rank"`) {
				// A daemon older than ranked search: bd's substring search, said so.
				fmt.Fprintln(stderr, "bd: this zm daemon does not rank searches yet (upgrade zm): substring search")
				rank, q.Q, q.Sort, q.Substr = false, "", "", query
				l, err = listIssues(ctx, c, q)
			}
			if err != nil {
				return err
			}
			if rank && len(l.Issues) == 0 && !strings.ContainsAny(strings.TrimSpace(query), " \t") {
				// One token nothing ranks for may be a bare id hash (ab12): bd's substring search.
				rank, q.Q, q.Sort, q.Substr, q.Limit = false, "", "", resolveID(ctx, c, query), unlimited
				if l, err = listIssues(ctx, c, q); err != nil {
					return err
				}
			}
			many := "many"
			if l.Total > 0 {
				many = fmt.Sprintf("up to %d", l.Total)
			}
			unranked := fmt.Sprintf("'%s' matches %s issues, too many to rank in budget: newest first, unranked. Add words or a filter (--status, --label, --type, --parent)", query, many)
			if asJSON(cmd) {
				if l.Unranked {
					fmt.Fprintln(stderr, "bd: "+unranked)
				}
				out := make([]*bdWithCounts, 0, len(l.Issues))
				for _, it := range l.Issues {
					out = append(out, withCounts(it, false))
				}
				return writeJSON(stdout, out, false)
			}
			if len(l.Issues) == 0 {
				fmt.Fprintf(stdout, "No issues found matching '%s'\n", query)
				return nil
			}
			switch {
			case l.Unranked:
				fmt.Fprintln(stdout, unranked+":")
			case rank && l.More:
				fmt.Fprintf(stdout, "Found %d issues matching '%s', best first (score); top %d shown, --limit N for more:\n", l.Total, query, len(l.Issues))
			case rank:
				fmt.Fprintf(stdout, "Found %d issues matching '%s', best first (score):\n", len(l.Issues), query)
			default:
				fmt.Fprintf(stdout, "Found %d issues matching '%s':\n", len(l.Issues), query)
			}
			for _, it := range l.Issues {
				line := fmt.Sprintf("%s [P%d] [%s] %s", it.ID, it.Priority, it.IssueType, it.Status)
				if rank && !l.Unranked {
					line = fmt.Sprintf("%7.3g  %s", it.Score, line) // a word in most issues scores near 0
				}
				if it.Assignee != "" {
					line += " @" + it.Assignee
				}
				if len(it.Labels) > 0 {
					line += " [" + strings.Join(it.Labels, " ") + "]"
				}
				fmt.Fprintln(stdout, line+" - "+it.Title)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&query, "query", "", "search text")
	f.StringVarP(&status, "status", "s", "", "status (all includes closed)")
	f.StringVarP(&assignee, "assignee", "a", "", "assignee")
	f.StringVarP(&typ, "type", "t", "", "issue type")
	f.StringVar(&parent, "parent", "", "only children of this issue")
	f.StringSliceVarP(&labels, "label", "l", nil, "labels (all must match)")
	f.StringSliceVar(&labelAny, "label-any", nil, "labels (any matches)")
	f.IntVarP(&limit, "limit", "n", 10, "limit (0: every match)")
	f.StringVar(&sortBy, "sort", "", "sort field (searches bd's way: substring, unranked)")
	f.BoolVarP(&reverse, "reverse", "r", false, "reverse")
	f.StringVar(&descContains, "desc-contains", "", "description contains")
	f.StringVar(&notesContains, "notes-contains", "", "notes contain")
	return cmd
}

// idLike: the query is one issue id or id prefix (acme-ab12, acme-ab12.3)
// of a project the caller reads: bd's substring search finds it, full-text
// ranking would not.
func idLike(ctx context.Context, c *client.Client, q string) bool {
	q = strings.ToLower(strings.TrimSpace(q))
	if !strings.Contains(q, "-") || strings.ContainsAny(q, " \t") {
		return false
	}
	m, err := issueProjects(ctx, c, false)
	if err != nil {
		return false
	}
	for _, p := range m.ps.Projects {
		if p.Prefix != "" && strings.HasPrefix(q, strings.ToLower(p.Prefix)+"-") {
			return true
		}
	}
	return false
}
