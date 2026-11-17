package main

// Owned by the work builder: `zm work ...`, plus the short forms claim,
// release, who, lease, leases, inflight.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/client"
)

func init() {
	work := &cobra.Command{Use: "work", Short: "Work items in every project you can see (local:, bd:, ...)",
		Long: `Work items: local ones (local:<id>, stored by zm per project) and tracker
items (bd:<id>, ...) from the project's adapters. Ids show as <project>/<id>;
you may pass them that way, or plain with --project when an id exists in
several projects. Claims are leases with deadlines: claim again (or renew)
before it expires, release when done.`}
	work.AddCommand(workListCmd(), workShowCmd(), workNewCmd(), claimCmd(), renewCmd(), releaseCmd(), workStatusCmd(),
		workCommentCmd(), workCommentsCmd(), workLinkCmd(), whoCmd(), workSourcesCmd(), workExportCmd(), inflightCmd(), leasesCmd())
	rootCmd.AddCommand(work, claimCmd(), releaseCmd(), whoCmd(), leaseCmd(), leasesCmd(), inflightCmd())
}

// itemArg accepts "<project-slug>/<id>" (as ids are displayed): the slug
// names the project for this call. Anything else is returned unchanged.
func itemArg(c *client.Client, arg string) string {
	slug, id, ok := strings.Cut(arg, "/")
	// Native references are not machine names: custom ids may contain
	// uppercase letters and exceed ValidName's 63-byte limit. Match the work
	// resolver's bare-id shape; explicit adapter references keep their syntax.
	bare := id != "" && !strings.ContainsAny(id, ":/ \t\r\n")
	if ok && slug != "" && !strings.Contains(slug, ":") && (strings.Contains(id, ":") || bare) {
		c.Project, c.ProjectFrom = slug, "flag"
		return id
	}
	return arg
}

// Display labels are optional: older daemons may ignore local=1 and try
// authority discovery, so both local lookups share this small budget.
const displayLookupWait = 100 * time.Millisecond

// slugs maps project ULIDs to locally known labels. Missing metadata leaves
// callers' existing raw-ID fallback intact; display never refreshes a remote.
func slugs(ctx context.Context, c *client.Client) map[string]string {
	ctx, cancel := context.WithTimeout(ctx, displayLookupWait)
	defer cancel()
	m := map[string]string{}
	if ps, err := c.LocalProjects(ctx); err == nil {
		for _, p := range ps {
			if p.ID != "" && p.Slug != "" {
				m[p.ID] = p.Slug
			}
		}
	}
	if rs, err := c.Remotes(ctx); err == nil {
		for _, r := range rs {
			if r.State == api.RemoteOK && r.Project.ID != "" && r.Project.Slug != "" {
				m[r.Project.ID] = r.Project.Slug
			}
		}
	}
	return m
}

func shown(sl map[string]string, project, id string) string {
	if s := sl[project]; s != "" {
		return s + "/" + id
	}
	return id
}

func slugOf(sl map[string]string, project string) string {
	if s := sl[project]; s != "" {
		return s
	}
	return project
}

// slugged is a result plus the slugs to show it with; --json prints V alone.
type slugged[T any] struct {
	V     T
	slugs map[string]string
}

func (s slugged[T]) MarshalJSON() ([]byte, error) { return json.Marshal(s.V) }

func withSlugs[T any](ctx context.Context, c *client.Client, v T, err error) (slugged[T], error) {
	if err != nil || flagJSON {
		return slugged[T]{V: v}, err
	}
	return slugged[T]{V: v, slugs: slugs(ctx, c)}, nil
}

func itemsText(v slugged[[]orchapi.WorkItem]) string {
	var b strings.Builder
	for _, it := range v.V {
		owner := it.Owner
		if owner == "" {
			owner = "-"
		}
		area := it.Area
		if area == "" {
			area = "-"
		}
		fmt.Fprintf(&b, "%-30s %-10s %-9s %-20s %s\n", shown(v.slugs, it.Project, it.ID), area, it.Status, clip(owner, 20), it.Title)
	}
	return textclean.Clean(b.String())
}

// clip shortens s to n runes with a trailing "~" so columns stay aligned
// (tracker holders like dev-01/task-10256-962b48d6-... are long).
func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "~"
}

func workListCmd() *cobra.Command {
	var q orchapi.WorkQuery
	cmd := &cobra.Command{
		Use: "ls", Aliases: []string{"list"}, Short: "List work items (every project you see; --project narrows)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (slugged[[]orchapi.WorkItem], error) {
				l, err := orchclient.WorkList(ctx, c, q)
				v, err := withSlugs(ctx, c, l.Items, err)
				if err == nil {
					warnSkipped(cmd.ErrOrStderr(), v.slugs, l.Skipped)
					warnBehind(cmd.ErrOrStderr(), v.slugs, l.Behind)
				}
				return v, err
			}, itemsText)
		},
	}
	cmd.Flags().StringVar(&q.Area, "area", "", "only this area")
	cmd.Flags().StringVar(&q.Source, "source", "", "only this source (local, or a tracker prefix/adapter: bd, beads)")
	cmd.Flags().StringVar((*string)(&q.Status), "status", "", "open, claimed, in_review, done")
	cmd.Flags().StringVar(&q.Owner, "owner", "", "only items held by this address")
	cmd.Flags().StringVarP(&q.Q, "query", "q", "", "text search")
	cmd.Flags().IntVar(&q.Limit, "limit", 0, "at most this many (default 50)")
	return cmd
}

// warnSkipped tells the user which trackers are missing from a list, so a
// broken tracker never reads as "no items".
func warnSkipped(w io.Writer, sl map[string]string, skipped []api.WorkSkipped) {
	for _, sk := range skipped {
		fmt.Fprintf(w, "warning: %s/%s unavailable, its items are not listed: %s\n", textclean.Clean(slugOf(sl, sk.Project)), textclean.Clean(sk.Source), textclean.Clean(sk.Error))
	}
}

// warnBehind names trackers whose items came from a mirror that is not
// current: loading (the list may be missing items) or stale.
func warnBehind(w io.Writer, sl map[string]string, behind []orchapi.WorkBehind) {
	for _, b := range behind {
		src := textclean.Clean(slugOf(sl, b.Project) + "/" + b.Source)
		switch {
		case b.State == orchapi.SyncLoading:
			fmt.Fprintf(w, "warning: %s is still loading (first sync); some of its items may be missing\n", src)
		case b.Synced.IsZero():
			fmt.Fprintf(w, "warning: %s has not synced: %s\n", src, textclean.Clean(b.Error))
		default:
			msg := fmt.Sprintf("warning: %s items as of %s ago", src, ago(b.Synced))
			if b.Error != "" {
				msg += ": " + textclean.Clean(b.Error)
			}
			fmt.Fprintln(w, msg)
		}
	}
}

// ago is a short "how long ago" (3s, 4m, 2h, 3d).
func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

// syncText is a tracker's mirror state for zm work sources.
// authorityFacts is what a replica's authority row knows of the authority: how
// long it has run and its last off-site backup.
func authorityFacts(s orchapi.WorkSource) []string {
	var facts []string
	if !s.Started.IsZero() {
		if s.Sync != nil && s.Sync.State == orchapi.SyncOK {
			facts = append(facts, "authority up "+ago(s.Started))
		} else {
			facts = append(facts, "authority started "+ago(s.Started)+" ago, when last heard from")
		}
	}
	if b := s.Backup; b != nil {
		switch {
		case b.Failing:
			facts = append(facts, "backup FAILING: "+b.Error)
		case b.LastOK != nil:
			facts = append(facts, "backup "+ago(*b.LastOK)+" ago")
		default:
			facts = append(facts, "backup never ran")
		}
	}
	return facts
}

func syncText(s orchapi.WorkSource) string {
	y := s.Sync
	if y == nil {
		if s.OK {
			return "ok"
		}
		return "BROKEN: " + s.Error
	}
	var facts []string
	if !y.Synced.IsZero() {
		facts = append(facts, "synced "+ago(y.Synced)+" ago")
	}
	if y.Items > 0 {
		facts = append(facts, fmt.Sprintf("%d items", y.Items))
	}
	facts = append(facts, authorityFacts(s)...)
	extra := ""
	if len(facts) > 0 {
		extra = " (" + strings.Join(facts, ", ") + ")"
	}
	switch {
	case y.State == orchapi.SyncOK:
		return "ok" + extra
	case y.State == orchapi.SyncLoading && y.Error == "":
		return "loading: first sync in progress" + extra
	case y.State == orchapi.SyncLoading:
		return "BROKEN: " + y.Error + extra
	case y.Error != "":
		return "STALE: " + y.Error + extra
	}
	return "STALE: sync overdue" + extra
}

func itemText(sl map[string]string, it orchapi.WorkItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", shown(sl, it.Project, it.ID), it.Title)
	fmt.Fprintf(&b, "status %s", it.Status)
	if it.Area != "" {
		fmt.Fprintf(&b, "  area %s", it.Area)
	}
	if it.Owner != "" {
		fmt.Fprintf(&b, "  held by %s", it.Owner)
	}
	if it.CreatedBy != "" {
		fmt.Fprintf(&b, "  created by %s", it.CreatedBy)
	}
	b.WriteByte('\n')
	if len(it.Labels) > 0 {
		fmt.Fprintf(&b, "labels %s\n", strings.Join(it.Labels, ", "))
	}
	if it.Stale != "" {
		fmt.Fprintf(&b, "warning: %s\n", it.Stale)
	}
	if it.URL != "" {
		fmt.Fprintf(&b, "%s\n", it.URL)
	}
	if it.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(it.Body, "\n"))
	}
	if len(it.Links) > 0 {
		b.WriteString("\nlinks:\n")
		for _, l := range it.Links {
			switch {
			case l.Hidden:
				fmt.Fprintf(&b, "  %s (linked to an item you can't access)\n", l.Rel)
			case l.Project != "":
				fmt.Fprintf(&b, "  %s %s\n", l.Rel, shown(sl, l.Project, l.Target))
			default:
				fmt.Fprintf(&b, "  %s %s\n", l.Rel, l.Target)
			}
		}
	}
	if len(it.Comments) > 0 {
		b.WriteString("\nhistory:\n")
		for _, cm := range it.Comments {
			fmt.Fprintf(&b, "  %s %s: %s\n", cm.Created.Local().Format("2006-01-02 15:04"), cm.Author, cm.Body)
		}
	}
	return textclean.Clean(b.String())
}

func workShowCmd() *cobra.Command {
	return &cobra.Command{
		Use: "show <id>", Short: "Show one work item, its links and history", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (slugged[orchapi.WorkItem], error) {
				it, err := orchclient.GetWork(ctx, c, itemArg(c, args[0]))
				return withSlugs(ctx, c, it, err)
			}, func(v slugged[orchapi.WorkItem]) string { return itemText(v.slugs, v.V) })
		},
	}
}

func workNewCmd() *cobra.Command {
	var req orchapi.CreateWorkRequest
	cmd := &cobra.Command{
		Use: "new <title...>", Short: "Create a local work item in the current project", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Title = strings.Join(args, " ")
			if req.Body == "-" {
				b, err := joinArgs([]string{"-"})
				if err != nil {
					return err
				}
				req.Body = b
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (slugged[orchapi.WorkItem], error) {
				it, err := orchclient.CreateWork(ctx, c, req)
				return withSlugs(ctx, c, it, err)
			}, func(v slugged[orchapi.WorkItem]) string {
				where := ""
				if v.V.Area != "" {
					where = " (area " + v.V.Area + ")"
				}
				return "created " + shown(v.slugs, v.V.Project, v.V.ID) + where
			})
		},
	}
	cmd.Flags().StringVar(&req.Body, "body", "", "description (- reads stdin)")
	cmd.Flags().StringVar(&req.Area, "area", "", "area of the project (default: from the repo you are in)")
	cmd.Flags().StringSliceVar(&req.Labels, "label", nil, "label (repeatable)")
	return cmd
}

func workCommentCmd() *cobra.Command {
	var fence *int64
	cmd := &cobra.Command{
		Use: "comment <id> <body...|->", Short: "Add to a work item's history (a record, not mail)", Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := joinArgs(args[1:])
			if err != nil {
				return err
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Comment, error) {
				return orchclient.Comment(ctx, c, itemArg(c, args[0]), orchapi.CommentRequest{Body: body, Fence: *fence})
			}, func(orchapi.Comment) string { return "commented on " + args[0] })
		},
	}
	fence = fenceFlag(cmd)
	return cmd
}

func workCommentsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "comments <id>", Short: "A work item's history", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) ([]orchapi.Comment, error) {
				return orchclient.Comments(ctx, c, itemArg(c, args[0]))
			}, func(cs []orchapi.Comment) string {
				var b strings.Builder
				for _, cm := range cs {
					fmt.Fprintf(&b, "%s %s: %s\n", cm.Created.Local().Format("2006-01-02 15:04"), cm.Author, cm.Body)
				}
				return b.String()
			})
		},
	}
}

func workStatusCmd() *cobra.Command {
	var fence *int64
	cmd := &cobra.Command{
		Use: "status <id> <open|in_review|done>", Short: "Set a work item's status (claimed comes from claiming)", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.QueuedStatus, error) {
				w, queued, err := orchclient.SetStatusSettled(ctx, c, itemArg(c, args[0]), orchapi.StatusRequest{Status: orchapi.WorkStatus(args[1]), Fence: *fence}, orchclient.StatusWait)
				q := orchapi.QueuedStatus{WorkItem: w}
				if queued { // a replica's write the authority has not answered yet; --json carries it too
					q.Queued = orchapi.StatusQueuedNote
				}
				return q, err
			}, func(q orchapi.QueuedStatus) string {
				if q.Queued != "" {
					return args[0] + " " + args[1] + " is " + q.Queued
				}
				return args[0] + " is " + string(q.Status)
			})
		},
	}
	fence = fenceFlag(cmd)
	return cmd
}

func workLinkCmd() *cobra.Command {
	return &cobra.Command{
		Use: "link <id> <rel> <target>", Short: "Link a work item (rel: " + strings.Join(orchapi.LinkRels, ", ") + ")", Args: cobra.ExactArgs(3),
		Long: `Link a work item to a URL (pr, url), a session (session) or another work item
(blocks, blocked_by, parent, child, related; any project you can see).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.WorkItem, error) {
				return orchclient.Link(ctx, c, itemArg(c, args[0]), orchapi.LinkRequest{Rel: args[1], Target: args[2]})
			}, func(orchapi.WorkItem) string { return "linked " + args[0] + " " + args[1] + " " + args[2] })
		},
	}
}

func workSourcesCmd() *cobra.Command {
	return &cobra.Command{
		Use: "sources", Short: "Work sources per project (local + trackers) and whether each works", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (slugged[[]orchapi.WorkSource], error) {
				s, err := orchclient.WorkSources(ctx, c, "")
				return withSlugs(ctx, c, s, err)
			}, func(v slugged[[]orchapi.WorkSource]) string {
				var b strings.Builder
				for _, s := range v.V {
					fmt.Fprintf(&b, "%-12s %-8s %-10s %s\n", slugOf(v.slugs, s.Project), s.Prefix, s.Name, syncText(s))
				}
				return b.String()
			})
		},
	}
}

// workExportCmd writes every item (with its history) as JSON Lines, one
// item per line: `zm work export --project acme > acme-work.jsonl`.
func workExportCmd() *cobra.Command {
	var source string
	cmd := &cobra.Command{
		Use: "export", Short: "Write work items with their history as JSON Lines (default: local items)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			l, err := orchclient.WorkList(ctx, c, orchapi.WorkQuery{Source: source, Limit: 5000})
			if err != nil {
				return err
			}
			loading := 0
			for _, b := range l.Behind {
				if b.State == orchapi.SyncLoading {
					loading++
				}
			}
			if len(l.Skipped) > 0 || loading > 0 { // an export must be whole or refuse
				sl := slugs(ctx, c)
				warnSkipped(cmd.ErrOrStderr(), sl, l.Skipped)
				warnBehind(cmd.ErrOrStderr(), sl, l.Behind)
				return fmt.Errorf("export incomplete: %d tracker(s) unavailable, %d still loading", len(l.Skipped), loading)
			}
			warnBehind(cmd.ErrOrStderr(), slugs(ctx, c), l.Behind) // stale: whole as of its last sync, and said so
			items := l.Items
			enc := json.NewEncoder(os.Stdout)
			for _, it := range items {
				one := *c
				one.Project, one.ProjectFrom = it.Project, "flag"
				full, err := orchclient.GetWork(ctx, &one, it.ID)
				if err != nil {
					return fmt.Errorf("%s: %w", it.ID, err)
				}
				raw, err := json.Marshal(full)
				if err != nil {
					return err
				}
				if err := enc.Encode(json.RawMessage(textclean.JSON(raw))); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&source, "source", "local", "local, a tracker prefix, or \"\" for every source")
	return cmd
}

func leaseText(l orchapi.Lease) string {
	s := fmt.Sprintf("%s held by %s", l.Key, l.Holder)
	if l.Expires != nil {
		s += " until " + l.Expires.Local().Format("15:04:05")
		if l.Bound {
			s += " (bound: renewed while the session runs)"
		}
	}
	if l.Fence != 0 {
		s += fmt.Sprintf(", fence %d", l.Fence)
	}
	return s
}

// fenceFlag adds --fence: the write or release is one under your lease
// with this fence (zm claim prints it), refused once that lease has gone.
func fenceFlag(cmd *cobra.Command) *int64 {
	return cmd.Flags().Int64("fence", 0, "your lease's fence (zm claim prints it): refused if that lease has gone")
}

func claimFlags(cmd *cobra.Command, req *orchapi.ClaimRequest) {
	cmd.Flags().IntVar(&req.TTLSeconds, "ttl", 0, "seconds until the lease expires unless renewed (default 3600)")
	cmd.Flags().StringVar(&req.Note, "note", "", "why")
}

// noQueueFlag adds --no-queue: on a team replica the write is never queued;
// with no answer it fails and says whether it may have applied.
func noQueueFlag(cmd *cobra.Command) *bool {
	return cmd.Flags().Bool("no-queue", false, "on a team replica, never queue it: no answer means failed, never applied later")
}

func claimCmd() *cobra.Command {
	var req orchapi.ClaimRequest
	var nq *bool
	cmd := &cobra.Command{
		Use: "claim <id>", Short: "Take a lease on a work item (again = renew)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.ClaimResponse, error) {
				c.NoQueue = *nq
				return orchclient.Claim(ctx, c, itemArg(c, args[0]), req)
			}, func(r orchapi.ClaimResponse) string { return leaseText(r.Lease) })
		},
	}
	claimFlags(cmd, &req)
	nq = noQueueFlag(cmd)
	return cmd
}

func renewCmd() *cobra.Command {
	req := orchapi.ClaimRequest{Renew: true}
	cmd := &cobra.Command{
		Use: "renew <id>", Short: "Extend your lease on a work item (refused if you do not hold it)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.ClaimResponse, error) {
				return orchclient.Claim(ctx, c, itemArg(c, args[0]), req)
			}, func(r orchapi.ClaimResponse) string { return leaseText(r.Lease) })
		},
	}
	claimFlags(cmd, &req)
	cmd.Flags().Int64Var(&req.Fence, "fence", 0, "only the lease with this fence (zm claim prints it)")
	return cmd
}

func releaseCmd() *cobra.Command {
	var req orchapi.ReleaseRequest
	var nq *bool
	cmd := &cobra.Command{
		Use: "release <id>", Short: "Release your lease on a work item", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.ReleaseResponse, error) {
				c.NoQueue = *nq
				return orchclient.Release(ctx, c, itemArg(c, args[0]), req)
			}, func(r orchapi.ReleaseResponse) string {
				if !r.Released {
					return r.Key + ": you held no lease on it"
				}
				return "released " + r.Key
			})
		},
	}
	cmd.Flags().BoolVar(&req.Force, "force", false, "release someone else's lease (owners/members, on this machine only)")
	cmd.Flags().Int64Var(&req.Fence, "fence", 0, "only the lease with this fence (zm claim prints it); another is refused")
	nq = noQueueFlag(cmd)
	return cmd
}

func whoCmd() *cobra.Command {
	return &cobra.Command{
		Use: "who <id>", Short: "Who holds a work item or a lock (zm lease), and where mail to it goes", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.HolderResponse, error) {
				h, err := orchclient.Holder(ctx, c, itemArg(c, args[0]))
				if api.IsCode(err, api.CodeNotFound) {
					// Not a work item: a plain lock (zm lease <key>, hub claim <key>) answers the same question.
					if ls, lerr := orchclient.Leases(ctx, c, orchapi.LeaseQuery{}); lerr == nil {
						if l, ok := plainLock(ls, args[0]); ok {
							return lockHolder(l), nil
						}
					}
				}
				return h, err
			}, func(h orchapi.HolderResponse) string {
				s := h.ID + ": nobody holds it"
				if h.Lease != nil {
					s = leaseText(*h.Lease)
					if ss := h.Session; ss != nil {
						s += " (" + string(ss.State)
						if ss.Note != "" {
							s += ": " + ss.Note
						}
						if ss.Seen != nil { // on a team replica: its last heartbeat
							s += ", presence " + age(*ss.Seen) + " ago"
						}
						s += ")"
					}
				} else if h.TrackerOwner != "" {
					s = h.ID + ": held in its tracker by " + h.TrackerOwner + " (no zm lease)"
				}
				if h.MailTo == "" {
					if h.TrackerOwner != "" {
						return s + "; mail to it is refused (that holder is not a zm address)"
					}
					return s + "; mail to it is refused (no holder, no known creator)"
				}
				return fmt.Sprintf("%s; mail goes to %s (%s)", s, h.MailTo, h.Via)
			})
		},
	}
}

// plainLock is the one live lease on key among ls (a lock, not a work
// item); several (the same key in several projects) is none: pass -P.
func plainLock(ls []orchapi.Lease, key string) (orchapi.Lease, bool) {
	var got []orchapi.Lease
	for _, l := range ls {
		if l.Key == strings.TrimSpace(key) {
			got = append(got, l)
		}
	}
	if len(got) != 1 {
		return orchapi.Lease{}, false
	}
	return got[0], true
}

// lockHolder answers zm who for a plain lock: mail goes to its holder.
func lockHolder(l orchapi.Lease) orchapi.HolderResponse {
	return orchapi.HolderResponse{ID: l.Key, Holder: l.Holder, Lease: &l, MailTo: l.Holder, Via: "holder of the lock " + l.Key}
}

func leaseCmd() *cobra.Command {
	var req orchapi.ClaimRequest
	var release, force bool
	var nq *bool
	cmd := &cobra.Command{
		Use: "lease <key>", Short: "Take (or --release) a lease on any key in the current project, e.g. lock:migrations", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if release {
				return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.ReleaseResponse, error) {
					c.NoQueue = *nq
					return orchclient.ReleaseLease(ctx, c, args[0], orchapi.ReleaseRequest{Force: force, Fence: req.Fence})
				}, func(r orchapi.ReleaseResponse) string { return "released " + r.Key })
			}
			req.Key = args[0]
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.ClaimResponse, error) {
				c.NoQueue = *nq
				return orchclient.Lease(ctx, c, req)
			},
				func(r orchapi.ClaimResponse) string { return leaseText(r.Lease) })
		},
	}
	claimFlags(cmd, &req)
	cmd.Flags().BoolVar(&req.Renew, "renew", false, "only extend a lease you hold")
	cmd.Flags().BoolVar(&release, "release", false, "release it")
	cmd.Flags().BoolVar(&force, "force", false, "with --release: someone else's (owners/members, this machine only)")
	cmd.Flags().Int64Var(&req.Fence, "fence", 0, "with --renew or --release: only the lease with this fence")
	nq = noQueueFlag(cmd)
	return cmd
}

func leasesCmd() *cobra.Command {
	var holder string
	cmd := &cobra.Command{
		Use: "leases", Short: "Live leases you can see, with their deadlines", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (slugged[[]orchapi.Lease], error) {
				ls, err := orchclient.Leases(ctx, c, orchapi.LeaseQuery{Holder: holder})
				return withSlugs(ctx, c, ls, err)
			}, func(v slugged[[]orchapi.Lease]) string {
				var b strings.Builder
				for _, l := range v.V {
					fmt.Fprintf(&b, "%-36s %-20s %s%s\n", shown(v.slugs, l.Project, l.Key), l.Holder, until(l.Expires), boundMark(l.Bound))
				}
				return b.String()
			})
		},
	}
	cmd.Flags().StringVar(&holder, "holder", "", "only leases held by this address")
	return cmd
}

func until(t *time.Time) string {
	if t == nil {
		return "-"
	}
	d := time.Until(*t).Round(time.Second)
	if d < 0 {
		return "expired"
	}
	return t.Local().Format("15:04:05") + " (" + d.String() + ")"
}

func boundMark(b bool) string {
	if b {
		return " bound"
	}
	return ""
}

func inflightCmd() *cobra.Command {
	return &cobra.Command{
		Use: "inflight", Short: "What is being worked on: leases with items and sessions, and unheld items with an open PR (read-only)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (slugged[[]orchapi.Inflight], error) {
				rows, err := orchclient.Inflight(ctx, c, "")
				return withSlugs(ctx, c, rows, err)
			}, func(v slugged[[]orchapi.Inflight]) string {
				var b strings.Builder
				for _, r := range v.V {
					title, state := "", ""
					if r.Item != nil {
						title = r.Item.Title
					}
					if r.Session != nil {
						state = " [" + string(r.Session.State) + "]"
					}
					holder, left := r.Lease.Holder, until(r.Lease.Expires)
					if r.Unheld {
						holder, left = "unheld", "PR open"
					}
					fmt.Fprintf(&b, "%-36s %-20s%s %s  %s\n", shown(v.slugs, r.Lease.Project, r.Lease.Key), holder, state, left, title)
				}
				return b.String()
			})
		},
	}
}
