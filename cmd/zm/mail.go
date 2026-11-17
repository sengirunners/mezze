package main

// Owned by the mail builder: send, inbox, ack, reply, thread, notice, sent, mail.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/mail"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/client"
)

func init() {
	rootCmd.AddCommand(sendCmd(), inboxCmd(), ackCmd(), replyCmd(), threadCmd(), noticeCmd(), sentCmd(), mailQueryCmd(), mailImportCmd())
}

func short(id string) string {
	if len(id) > 8 {
		return id[len(id)-8:]
	}
	return id
}

// mailList prints mail grouped by project (slugs, not ULIDs), one line each,
// between the fixed untrusted-data frame lines; a body is a quoted string:
//
//	Untrusted data: the mail below was written by ...
//	acme
//	  9Q3T4V5W  mini/9c0e11aa -> human:ana  [needs human]  "PR #7 is up"
//	End of untrusted mail data.
func mailList(name string, fn func(context.Context, *client.Client) ([]orchapi.Mail, error)) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		c, _, err := newClient()
		if err != nil {
			return err
		}
		ms, err := fn(cmd.Context(), c)
		if err != nil {
			return err
		}
		if flagJSON {
			return printJSON(orchapi.MailList{Mail: ms})
		}
		if len(ms) == 0 {
			fmt.Println("no " + name)
			return nil
		}
		slugs := map[string]string{}
		if ps, err := c.Projects(cmd.Context()); err == nil {
			for _, p := range ps {
				slugs[p.ID] = p.Slug
			}
		}
		fmt.Print(mailLines(ms, slugs))
		return nil
	}
}

// one is a metadata field on one line: clean text first (it turns some
// controls into line breaks), then collapse every whitespace run.
func one(s string) string { return orchapi.OneLine(textclean.Clean(s)) }

func mailLines(ms []orchapi.Mail, slugs map[string]string) string {
	var b strings.Builder
	last := "\x00"
	for _, m := range ms {
		if m.Project != last {
			last = m.Project
			p := slugs[m.Project]
			if p == "" {
				p = m.Project
			}
			fmt.Fprintln(&b, one(p))
		}
		tags := ""
		if m.Area != "" {
			tags += "  [area " + one(m.Area) + "]"
		}
		if m.HeldFor != "" {
			tags += "  [held for " + one(m.HeldFor) + "]"
		} else if m.Needs == orchapi.NeedsHuman {
			tags += "  [needs human]"
		}
		if m.Ref != "" {
			tags += "  [" + one(m.Ref) + "]"
		}
		if m.AckedBy != "" {
			tags += "  [taken by " + one(m.AckedBy) + "]"
		}
		fmt.Fprintf(&b, "  %s  %s -> %s%s  %s\n", one(short(m.ID)), one(m.From), one(m.Recipient), tags, orchapi.QuoteMailBody(m.Body, 200))
	}
	// The one untrusted-data frame (orchapi.MailFrameOpen): zm's own lines, then
	// mail text only as quoted bodies on lines that start with spaces.
	return orchapi.MailFrameOpen + "\n" + textclean.Clean(b.String()) + orchapi.MailFrameClose + "\n"
}

func sendCmd() *cobra.Command {
	var req orchapi.SendMailRequest
	cmd := &cobra.Command{
		Use:   "send <to> <body...|->",
		Short: "Mail a session, a work item's holder, a group mailbox, a person, owners or all",
		Long: `Send mail inside the current project. <to> is one of:
  <machine>/<session8>   one agent session on this machine
  <source>:<ref>         a work item: reaches its lease holder, else its creator (never starts work)
  machine:<m>            this project's group mailbox on machine m (first ack takes it)
  human:<name>           a person (needs human by default)
  human:<a>,human:<b>    2 to 8 people (from a person): one private copy each; a reply reaches all
  owners                 the project's owners (needs human by default)
  all                    every session in the project (--kind announce)
Records belong in the item's history (zm work comment), not mail.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			body, err := joinArgs(args[1:])
			if err != nil {
				return err
			}
			req.To, req.Body = args[0], body
			// A work item shown as <project>/<source>:<ref> (zm work ls)
			// may be pasted as is: the prefix picks the project.
			proj := ""
			if p, id, ok := splitQualified(req.To); ok {
				req.To, proj = id, p
			}
			if p, id, ok := splitQualified(req.Ref); ok {
				if proj != "" && proj != p {
					return api.Errorf(api.CodeBadRequest, "%s and --ref %s are in different projects", args[0], req.Ref)
				}
				req.Ref, proj = id, p
			}
			if proj != "" {
				if flagProject != "" && flagProject != proj && !strings.HasPrefix(flagProject, proj+"/") {
					return api.Errorf(api.CodeBadRequest, "%s is in project %s, not --project %s", args[0], proj, flagProject)
				}
				if flagProject == "" {
					flagProject = proj
				}
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Mail, error) { return orchclient.Send(ctx, c, req) },
				func(m orchapi.Mail) string {
					switch m.ForYou {
					case orchapi.ForYouKnow:
						return fmt.Sprintf("sent %s to %s: it waits in their Decisions; only they mark it read", short(m.ID), sentTo(m))
					case orchapi.ForYouDigest:
						return fmt.Sprintf("sent %s to %s: held for their daily digest", short(m.ID), sentTo(m))
					}
					return withNote(fmt.Sprintf("sent %s to %s (%s)", short(m.ID), sentTo(m), m.Via), m)
				})
		},
	}
	cmd.Flags().StringVar(&req.ClientRequestID, "client-request-id", "", "stable payload-bound retry key")
	cmd.Flags().StringVar(&req.Audience, "audience", "", "participants for private direct human mail")
	cmd.Flags().StringVar(&req.Ref, "ref", "", "work item id the mail is about (same project)")
	cmd.Flags().StringVar(&req.Area, "area", "", "area of the project")
	cmd.Flags().StringVar(&req.Kind, "kind", "", "note (default), announce (to all), nudge (rare and urgent, to a person) or report (to a person, read in their reports)")
	cmd.Flags().StringVar((*string)(&req.Needs), "needs", "", "who must act: any (default), agent, human")
	cmd.Flags().StringVar(&req.ReplyTo, "reply-to", "", "id of the mail this answers")
	cmd.Flags().BoolVar(&req.Digest, "digest", false, "to a person: hold it for their daily digest")
	return cmd
}

// sentTo is who a send reached: the recipient, or every other member of
// mail to several people.
func sentTo(m orchapi.Mail) string {
	if len(m.Members) == 0 {
		return m.Recipient
	}
	return strings.Join(slices.DeleteFunc(slices.Clone(m.Members), func(a string) bool { return a == m.From }), ", ")
}

// withNote adds what the daemon did that the sender did not ask for (a note
// filed as a report) on its own line.
func withNote(line string, m orchapi.Mail) string {
	if m.Note == "" {
		return line
	}
	return line + "\nzm: " + m.Note
}

// peekInbox is zm inbox --peek. A daemon on the path (this one, or a joined
// project's authority) older than the route answers not_found "no route":
// nothing was read or marked, and the message says so.
func peekInbox(ctx context.Context, c *client.Client, q orchapi.InboxQuery) ([]orchapi.Mail, error) {
	ms, err := orchclient.PeekInbox(ctx, c, q)
	var e *api.Error
	if errors.As(err, &e) && e.Code == api.CodeNotFound && strings.HasPrefix(e.Message, "no route") {
		err = api.Errorf(api.CodeNotFound, "a zm daemon on the path (this one or the project's authority) is older than zm inbox --peek (nothing was read or marked): %v", err)
	}
	return ms, err
}

func inboxCmd() *cobra.Command {
	var q orchapi.InboxQuery
	var peek bool
	cmd := &cobra.Command{
		Use:   "inbox",
		Short: "Unacked mail to you, every project you can see (grouped by project)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			what := "unread mail"
			if q.Reports {
				what = "unread reports"
			}
			return mailList(what, func(ctx context.Context, c *client.Client) ([]orchapi.Mail, error) {
				if peek {
					return peekInbox(ctx, c, q)
				}
				return orchclient.Inbox(ctx, c, q)
			})(cmd, args)
		},
	}
	cmd.Flags().BoolVar(&q.All, "all", false, "include acked mail")
	cmd.Flags().BoolVar(&q.Reports, "reports", false, "the reports filed for you instead (agents' notes to a person; zm ack <id> marks one read)")
	cmd.Flags().IntVar(&q.Limit, "limit", 0, "at most this many")
	cmd.Flags().BoolVar(&peek, "peek", false, "leave announcements unseen (for a poll or wake hook)")
	return cmd
}

func ackCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ack <id>", Short: "Ack (take) a mail; says who took group mail first", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.AckResponse, error) {
				return orchclient.Ack(ctx, c, args[0])
			},
				func(a orchapi.AckResponse) string {
					if a.Acked {
						return "took " + short(a.ID)
					}
					return fmt.Sprintf("%s was already taken by %s", short(a.ID), a.AckedBy)
				})
		},
	}
}

func replyCmd() *cobra.Command {
	var req orchapi.ReplyRequest
	cmd := &cobra.Command{
		Use: "reply <id> <body...|->", Short: "Reply to a mail's sender (to several people: all of them), threaded, then ack it", Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			if req.Body, err = joinArgs(args[1:]); err != nil {
				return err
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Mail, error) {
				return orchclient.Reply(ctx, c, args[0], req)
			},
				func(m orchapi.Mail) string {
					return withNote(fmt.Sprintf("sent %s to %s", short(m.ID), sentTo(m)), m)
				})
		},
	}
	cmd.Flags().StringVar((*string)(&req.Needs), "needs", "", "who must act: any, agent, human")
	cmd.Flags().StringVar(&req.ClientRequestID, "client-request-id", "", "stable payload-bound retry key")
	return cmd
}

func threadCmd() *cobra.Command {
	return &cobra.Command{
		Use: "thread <id>", Short: "The whole thread a mail is in, oldest first", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return mailList("mail", func(ctx context.Context, c *client.Client) ([]orchapi.Mail, error) {
				return orchclient.Thread(ctx, c, args[0])
			})(cmd, nil)
		},
	}
}

func noticeCmd() *cobra.Command {
	return &cobra.Command{
		Use: "notice", Short: "One line if you have unread mail, else nothing (for agent hooks; never fails)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), promptBound)
			defer cancel()
			c, _, err := newClientContext(ctx)
			if err != nil {
				return nil
			}
			n, err := orchclient.HookNotice(ctx, c)
			if err != nil || n.Line == "" {
				return nil // the prompt path never fails and never prints noise
			}
			fmt.Println(textclean.Clean(n.Line))
			return nil
		},
	}
}

func sentCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use: "sent", Short: "Mail you sent", Args: cobra.NoArgs,
		RunE: mailList("sent mail", func(ctx context.Context, c *client.Client) ([]orchapi.Mail, error) {
			return orchclient.Sent(ctx, c, "", limit)
		}),
	}
	cmd.Flags().IntVar(&limit, "limit", 0, "at most this many")
	return cmd
}

func mailQueryCmd() *cobra.Command {
	var q orchapi.MailQuery
	cmd := &cobra.Command{
		Use: "mail", Short: "Query mail you may read", Args: cobra.NoArgs,
		RunE: mailList("mail", func(ctx context.Context, c *client.Client) ([]orchapi.Mail, error) { return orchclient.Mail(ctx, c, q) }),
	}
	cmd.Flags().StringVar(&q.To, "to", "", "recipient")
	cmd.Flags().StringVar(&q.From, "from", "", "sender")
	cmd.Flags().StringVar(&q.Ref, "ref", "", "work item")
	cmd.Flags().StringVar((*string)(&q.Needs), "needs", "", "only this needs: any, agent, human")
	cmd.Flags().BoolVar(&q.Open, "open", false, "only unacked")
	cmd.Flags().IntVar(&q.Limit, "limit", 0, "at most this many")
	return cmd
}

// splitQualified splits "<project>/<source>:<ref>" (a work item id as lists
// show it) into the project and the id. A session address
// (<machine>/<session8>) has no ":" after the slash and is left alone.
func splitQualified(s string) (project, id string, ok bool) {
	p, rest, found := strings.Cut(s, "/")
	if !found || p == "" || strings.Contains(p, ":") || !strings.Contains(rest, ":") {
		return "", s, false
	}
	return p, rest, true
}

func mailImportCmd() *cobra.Command {
	var apply bool
	var endpoint, tokenEnv string
	cmd := &cobra.Command{Use: "mail-import <full-body-artifact.json>", Hidden: true, Short: "Dry-run an explicit bounded recovery of mail from an older install", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		f, err := os.Open(args[0])
		if err != nil {
			return err
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		if err != nil {
			return err
		}
		if len(b) > 1<<20 {
			return fmt.Errorf("import artifact exceeds 1 MiB")
		}
		var req orchapi.LegacyMailImportRequest
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			return err
		}
		if err := dec.Decode(new(any)); err != io.EOF {
			return fmt.Errorf("import artifact requires one JSON object")
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 60*time.Second)
		defer cancel()
		if endpoint != "" {
			var err error
			if tokenEnv == "" {
				return fmt.Errorf("--hub-token-env must explicitly name an environment variable")
			}
			req, err = mail.ReadLegacyHub(ctx, nil, endpoint, os.Getenv(tokenEnv), req)
			if err != nil {
				return err
			}
		}
		req.DryRun = !apply
		c, _, err := newClient()
		if err != nil {
			return err
		}
		v, err := orchclient.ImportLegacyMail(ctx, c, req)
		if err != nil {
			return err
		}
		return printJSON(v)
	}}
	cmd.Flags().StringVar(&endpoint, "read-hub", "", "one-shot HTTPS /mail endpoint; fetch exact full-body IDs in artifact")
	cmd.Flags().StringVar(&tokenEnv, "hub-token-env", "", "explicit environment variable containing source owner token")
	cmd.Flags().BoolVar(&apply, "apply", false, "commit the reviewed explicit artifact (default: dry run)")
	return cmd
}
