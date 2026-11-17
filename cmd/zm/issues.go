package main

// `zm issues` (SPEC "Native issues"): set a project's issue prefix, list the
// projects with native issues, import a beads Dolt database, restore a
// backup, read issue history, move open issues to another project, redact a
// comment and acknowledge a redaction gap. Everyday issue verbs are
// the bd-compatible door, cmd/bd-zm.

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/issues/imgsafe"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/client"
)

func init() { rootCmd.AddCommand(issuesCmd()) }

func issuesCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "issues", Short: "Native issues: history, prefix, projects, import from a bd (Beads) tracker, restore a backup, move (daily verbs: bd-zm)"}
	cmd.AddCommand(issuesRestoreCmd(), issuesMoveCmd(), issuesHistoryCmd(), issuesRedactCmd(), issuesAckGapCmd())
	cmd.AddCommand(issuesAttachCmd(), issuesAttachmentsCmd())
	cmd.AddCommand(&cobra.Command{
		Use: "ls", Short: "Projects with native issues and their id prefix", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.IssueProjects, error) {
				return orchclient.IssueProjects(ctx, c)
			},
				func(p orchapi.IssueProjects) string {
					var b strings.Builder
					for _, x := range p.Projects {
						fmt.Fprintf(&b, "%s\t%s-\n", x.Slug, x.Prefix)
					}
					return b.String()
				})
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use: "setup <prefix>", Short: "Give the resolved project its issue id prefix (before its first issue)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.IssueProjectInfo, error) {
				return orchclient.SetupIssues(ctx, c, args[0])
			},
				func(p orchapi.IssueProjectInfo) string {
					return fmt.Sprintf("%s: issues are %s-<id>", p.Slug, p.Prefix)
				})
		},
	})
	return cmd
}

func issuesHistoryCmd() *cobra.Command {
	return &cobra.Command{
		Use: "history <id>", Short: "Show an issue's changes, including previous field values", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) ([]orchapi.IssueChange, error) {
				return orchclient.IssueHistory(ctx, c, args[0])
			}, issueHistoryText)
		},
	}
}

func issuesRedactCmd() *cobra.Command {
	return &cobra.Command{
		Use: "redact <id> <comment-id>", Short: "Overwrite a comment's text and every copy of it (cannot be undone)", Args: cobra.ExactArgs(2),
		Long: `Redact a native issue comment: an admin of the issue's area, or the machine
owner. A conflict names the stops zm cannot prove clean; once one is dealt with
by hand, zm issues ack-gap clears it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.IssueComment, error) {
				return orchclient.RedactComment(ctx, c, args[0], args[1])
			}, func(cm orchapi.IssueComment) string { return "redacted " + cm.ID })
		},
	}
}

func issuesAckGapCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ack-gap <id> <comment-id> <stop>",
		Short: "Acknowledge a redaction gap dealt with out of band, so redact stops reporting it",
		Args:  cobra.ExactArgs(3),
		Long: `Clear one stop a redaction reported as a gap ("cannot prove every copy of its
text is gone"), after its copies were dealt with by hand. Name the stop as the
conflict does: project:issue (or old>new for a hop). Whoever may redact the
comment may do this; zm records who and when. It searches no text and deletes
no backup: it only clears the gap.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.RedactionGapAck, error) {
				return orchclient.AckRedactionGap(ctx, c, args[0], args[1], args[2])
			}, ackGapText)
		},
	}
}

func ackGapText(a orchapi.RedactionGapAck) string {
	s := fmt.Sprintf("acknowledged %s (%s) on comment %s\n", a.Stop, a.Why, a.Comment)
	if len(a.Remaining) == 0 {
		return s + "  no gaps left: redact again to confirm\n"
	}
	return s + "  still open: " + strings.Join(a.Remaining, ", ") + "\n"
}

func issueHistoryText(hs []orchapi.IssueChange) string {
	var b strings.Builder
	for _, h := range hs {
		fmt.Fprintf(&b, "%s  %s  %s\n", h.At.Format(time.RFC3339), h.Kind, h.Actor)
		fields := make([]string, 0, len(h.Fields))
		for field := range h.Fields {
			fields = append(fields, field)
		}
		sort.Strings(fields)
		for _, field := range fields {
			fmt.Fprintf(&b, "  %s: %s\n", field, h.Fields[field])
		}
		if h.Comment != "" {
			fmt.Fprintf(&b, "  %s\n", h.Comment)
		}
	}
	return b.String()
}

func issuesRestoreCmd() *cobra.Command {
	var req orchapi.IssueRestoreRequest
	c := &cobra.Command{
		Use:   "restore [prefix] [--from <backup>] [--at <stamp>]",
		Short: "Replace the project's native issues with a sqlite backup's copy",
		Long: `Replace the project's native issue store with a copy from a sqlite backup
(zm tracker backup add <prefix> <target> --kind sqlite). Writes are held, the
current store is copied next to the backup's other copies, and the redaction
list is applied to the copy before it replaces the store, so a redacted comment
does not come back. A copy from a newer zm, or of another project, is refused.
Issue changes made since the copy are gone from the store. project.db is not
restored: mail, leases and local work stay as they are.

--dry-run does everything but the replacing: it proves a copy restores.`,
		Example: `  zm issues restore --dry-run                     # the newest copy of any sqlite backup
  zm issues restore --from file:///Volumes/b --at 20261101T12
  zm issues restore --from /Volumes/b/copy.issues.db`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				req.Prefix = args[0]
			}
			if strings.HasPrefix(req.From, "~") {
				if err := absFile(&req.From); err != nil {
					return err
				}
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.IssueRestoreResult, error) {
				return orchclient.RestoreIssues(ctx, c, req)
			}, restoreText)
		},
	}
	f := c.Flags()
	f.StringVar(&req.From, "from", "", "a backup id (file:///x), a directory or a copy (default: every sqlite backup)")
	f.StringVar(&req.At, "at", "", "a copy's time stamp, or part of one (default: the newest)")
	f.BoolVar(&req.DryRun, "dry-run", false, "check everything, change nothing")
	f.BoolVar(&req.Force, "force", false, "restore although the current store could not be copied first")
	return c
}

func restoreText(r orchapi.IssueRestoreResult) string {
	var b strings.Builder
	if r.DryRun {
		b.WriteString("dry run, nothing changed: this copy restores\n")
	} else {
		b.WriteString("restored\n")
	}
	fmt.Fprintf(&b, "  copy:  %s\n  holds: %d issues, %d comments\n", r.Copy, r.Counts["issues"], r.Counts["comments"])
	if r.Redactions > 0 {
		fmt.Fprintf(&b, "  redacted again: %d comments the copy held in clear\n", r.Redactions)
	}
	if r.Kept != "" {
		fmt.Fprintf(&b, "  the store it replaced: %s\n", r.Kept)
	}
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "  note: %s\n", n)
	}
	return b.String()
}

func issuesMoveCmd() *cobra.Command {
	var req orchapi.IssueMoveRequest
	var from string
	c := &cobra.Command{
		Use:   "move --to <project> [id...] [--ids-from <file>]",
		Short: "Move open issues of the resolved project to another project on the same authority",
		Long: `Move open issues, with their comments, history and edges, to another project
on the same authority. The new id is the target's prefix plus the old id
without its prefix (acme-r0e3s.41 becomes side-r0e3s.41); the old id keeps
working (bd show follows it). Closed issues are refused (they stay). A claimed
issue is refused unless --transfer-claims: then its claim moves and its lease
is made again on the new id. Admin on both projects. Run --dry-run first; a
move that stopped part way is finished by running it again.`,
		Example: `  zm issues move -P acme --to side acme-r0e3s --children --dry-run
  zm issues move -P acme --to side --ids-from side-ids.txt --transfer-claims`,
		RunE: func(cmd *cobra.Command, args []string) error {
			req.IDs = append(req.IDs, args...)
			if from != "" {
				b, err := os.ReadFile(from)
				if err != nil {
					return err
				}
				req.IDs = append(req.IDs, strings.Fields(string(b))...)
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.IssueMoveResult, error) {
				return orchclient.MoveIssues(ctx, c, req)
			}, moveText)
		},
	}
	f := c.Flags()
	f.StringVar(&req.To, "to", "", "the project to move them to (slug)")
	f.StringVar(&from, "ids-from", "", "a file of ids (whitespace-separated)")
	f.BoolVar(&req.Children, "children", false, "also every open descendant")
	f.BoolVar(&req.TransferClaims, "transfer-claims", false, "move claimed issues with their claim")
	f.BoolVar(&req.DryRun, "dry-run", false, "plan, change nothing")
	_ = c.MarkFlagRequired("to")
	return c
}

func moveText(r orchapi.IssueMoveResult) string {
	var b strings.Builder
	for _, it := range r.Items {
		fmt.Fprintf(&b, "%-13s %s", it.Result, it.ID)
		if it.NewID != "" {
			fmt.Fprintf(&b, " -> %s", it.NewID)
		}
		if it.Title != "" {
			fmt.Fprintf(&b, "  %s", it.Title)
		}
		if it.Claim != "" {
			fmt.Fprintf(&b, "  [claimed: %s]", it.Claim)
		}
		if len(it.Cross) > 0 {
			fmt.Fprintf(&b, "  [cross-project: %s]", strings.Join(it.Cross, " "))
		}
		if it.Note != "" {
			fmt.Fprintf(&b, "  (%s)", it.Note)
		}
		b.WriteByte('\n')
	}
	if r.DryRun {
		n := 0
		for _, it := range r.Items {
			if it.Result == "move" {
				n++
			}
		}
		fmt.Fprintf(&b, "dry run, nothing changed: %d would move from %s to %s, %d refused\n", n, r.From, r.To, r.Refused)
	} else {
		fmt.Fprintf(&b, "%d moved from %s to %s, %d refused\n", r.Moved, r.From, r.To, r.Refused)
	}
	return b.String()
}

// ---- attachments (SPEC "Native issues", attachments) ----

// nativeID takes a work id (bd:zm-abc1) or a bare issue id.
func nativeID(id string) string {
	if _, rest, ok := strings.Cut(id, ":"); ok {
		return rest
	}
	return id
}

// readAttachment reads an attachment file ("-" is stdin) as imgsafe.ForUpload
// sends it.
func readAttachment(name string) ([]byte, error) {
	var in io.Reader = os.Stdin
	if name != "-" {
		f, err := os.Open(name)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		in = f
	}
	data, err := io.ReadAll(io.LimitReader(in, orchapi.MaxIssueAttachmentUpload+1))
	if err != nil {
		return nil, err
	}
	if len(data) > orchapi.MaxIssueAttachmentUpload {
		return nil, fmt.Errorf("%s is over %d bytes: scale it down first", name, orchapi.MaxIssueAttachmentUpload)
	}
	if data, err = imgsafe.ForUpload(data); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return data, nil
}

func issuesAttachCmd() *cobra.Command {
	var expires string
	cmd := &cobra.Command{
		Use:   "attach <item-id> <file>",
		Short: "Attach an image (PNG, JPEG, WebP) or an HTML page (at most 8 per item, ~1 MiB each) to a work item; it expires (default 14d, at most 90d)",
		Long: `Attach an image to a native work item: a decision's chart or screenshot, the brief's evidence.
The image is checked by its bytes, re-encoded with its metadata (EXIF, GPS) stripped, and kept at the
project's authority only. It expires on its own (--expires, default 14d, at most 90d); its attacher or
an area admin removes it sooner (zm issues attachments rm). "-" reads the image from stdin.
An HTML page (a mockup) is kept as it is and shown sandboxed: inline its CSS and use data: images; scripts and network do not run.`,
		Example: `  zm issues attach zm-abc1 chart.png
  zm issues attach bd:zm-abc1 screenshot.jpg --expires 30d`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			data, err := readAttachment(args[1])
			if err != nil {
				return err
			}
			id := nativeID(args[0])
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.IssueAttachment, error) {
				return orchclient.AttachIssueImage(ctx, c, id, data, expires)
			}, func(a orchapi.IssueAttachment) string {
				return fmt.Sprintf("attached %s to %s (%s, %d bytes, expires %s)", a.ID, id, a.Type, a.Size, a.Expires.Local().Format("2006-01-02 15:04"))
			})
		},
	}
	cmd.Flags().StringVar(&expires, "expires", "", "how long it lives: 14d (default), 36h, at most 90d")
	return cmd
}

func issuesAttachmentsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "attachments <item-id>",
		Short: "List a work item's images (metadata); get and rm act on one",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := nativeID(args[0])
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.IssueAttachmentList, error) {
				return orchclient.IssueAttachments(ctx, c, id)
			}, func(l orchapi.IssueAttachmentList) string {
				if len(l.Attachments) == 0 {
					return id + " has no attachments"
				}
				var b strings.Builder
				for _, a := range l.Attachments {
					fmt.Fprintf(&b, "%s\t%s\t%d bytes\tby %s\texpires %s\n", a.ID, a.Type, a.Size, a.CreatedBy, a.Expires.Local().Format("2006-01-02 15:04"))
				}
				return b.String()
			})
		},
	}
	var out string
	get := &cobra.Command{
		Use:   "get <item-id> <attachment-id>",
		Short: "Save an attachment's bytes (--out <file>, or - for stdout; default <attachment-id>.<ext> here)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			resp, err := orchclient.IssueAttachment(cmd.Context(), c, nativeID(args[0]), args[1])
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if out == "-" {
				_, err = io.Copy(os.Stdout, resp.Body)
				return err
			}
			path := out
			if path == "" {
				path = args[1] + map[string]string{imgsafe.PNG: ".png", imgsafe.JPEG: ".jpg", imgsafe.WebP: ".webp", imgsafe.HTML: ".html"}[strings.SplitN(resp.Header.Get("Content-Type"), ";", 2)[0]]
			}
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			n, err := io.Copy(f, resp.Body)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				os.Remove(path)
				return err
			}
			fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", path, n)
			return nil
		},
	}
	get.Flags().StringVarP(&out, "out", "o", "", "the file to write (- = stdout)")
	rm := &cobra.Command{
		Use:   "rm <item-id> <attachment-id>",
		Short: "Remove an attachment for good (its attacher or an area admin)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.IssueAttachment, error) {
				return orchclient.RemoveIssueAttachment(ctx, c, nativeID(args[0]), args[1])
			}, func(a orchapi.IssueAttachment) string { return "removed " + a.ID })
		},
	}
	cmd.AddCommand(get, rm)
	return cmd
}
