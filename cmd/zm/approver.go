package main

// Owned by the approvals builder: `zm approver add|ls|rm` and `zm ask
// new|ls|show|receipt` (SPEC section 16).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet"
)

func init() {
	ap := &cobra.Command{Use: "approver", Short: "Paired devices that may decide asks (the decide right never belongs to this socket)"}
	var noQR bool
	var forWho string
	add := &cobra.Command{
		Use: "add <name>", Short: "Reserve an approver name and print a one-time pairing code (and QR) for the device", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, cfg, err := newClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			r, err := orchclient.AddApproverFor(ctx, c, args[0], forWho)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(r)
			}
			url := ""
			st, err := c.Tailnet(ctx)
			if err == nil && (st.HTTPSURL != "" || st.URL != "") {
				// HTTPS when served: a phone installs the Decisions page only from it.
				url = strings.TrimRight(first(st.HTTPSURL, st.URL), "/") + r.PairPath
			}
			fmt.Printf("approver %s reserved. Pairing code: %s (expires %s)\n", r.Approver.Name, r.Code, r.Expires.Local().Format(time.Kitchen))
			if url == "" {
				fmt.Printf("This daemon is not serving the tailnet yet: run zm tailnet up, then on the device open\n  http://%s:%s%s\n",
					tailnetNodeName(cfg, st), client.TailnetPort, r.PairPath)
			} else {
				fmt.Printf("On the device (signed in to your tailnet, not this Mac), open:\n  %s\n", url)
				if !noQR && term.IsTerminal(int(os.Stdout.Fd())) {
					if q, err := approvals.QR(url); err == nil {
						fmt.Print(q.Terminal())
					}
				}
			}
			switch {
			case r.Note != "":
				fmt.Println("Warning: " + r.Note)
			case r.Confirm:
				fmt.Println("After pairing, an existing approver must confirm the new device on theirs.")
			}
			return nil
		},
	}
	pairMe := &cobra.Command{
		Use:   "pair-me [name]",
		Short: "Pair your own phone with the project's Decisions page (at its authority); an existing approver taps confirm",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			r, err := orchclient.AddMyPhone(ctx, c, name)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(r)
			}
			// The page lives where the project does: its authority for a joined
			// project, else this daemon.
			base := ""
			if rs, err := c.Remotes(ctx); err == nil {
				want := strings.SplitN(first(flagProject, c.Project), "/", 2)[0]
				for _, rm := range rs {
					if want != "" && (rm.Project.Slug == want || rm.Project.ID == want) || want == "" && len(rs) == 1 {
						base = rm.URL
					}
				}
			}
			if base == "" {
				if st, err := c.Tailnet(ctx); err == nil {
					base = first(st.HTTPSURL, st.URL)
				}
			}
			url := strings.TrimRight(base, "/") + r.PairPath
			fmt.Printf("Phone %s reserved for you. Code %s (expires %s).\nOn your phone (signed in to the tailnet), open:\n  %s\n",
				r.Approver.Name, r.Code, r.Expires.Local().Format(time.Kitchen), url)
			if !noQR && term.IsTerminal(int(os.Stdout.Fd())) {
				if q, err := approvals.QR(url); err == nil {
					fmt.Print(q.Terminal())
				}
			}
			fmt.Println("Add the page to your Home Screen, then ask someone already paired there to tap Confirm on theirs.")
			return nil
		},
	}
	pairMe.Flags().BoolVar(&noQR, "no-qr", false, "do not draw the QR code")
	ap.AddCommand(pairMe)
	add.Flags().BoolVar(&noQR, "no-qr", false, "do not draw the QR code")
	add.Flags().StringVar(&forWho, "for", "", "whose device this is (an identity's name): its Decisions page lists their decisions; without it the device answers as this machine's owner only while they are a project's one answerer, and never grants a permit")
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List approvers, the daemon each is paired on, and (-P) where a project's asks are decided",
		Long: `List this daemon's approvers and their devices. An approver decides only the asks its own daemon
decides: a joined project's asks are decided at its authority. With -P <project>, list the approvers
where that project's asks are decided: this daemon's, or at a joined project's authority only your own
(pair one there: zm approver pair-me -P <project>).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if flagProject != "" {
				p, err := approverPlace(ctx, c, flagProject)
				if err != nil {
					return err
				}
				if flagJSON {
					return printJSON(p)
				}
				fmt.Print(textclean.Clean(approverPlaceText(p)))
				return nil
			}
			l, err := orchclient.ApproversHere(ctx, c)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(l.Paired())
			}
			if len(l.Approvers) == 0 {
				fmt.Printf("no approvers on %s (zm approver add <name>)\n", l.Daemon)
			} else {
				fmt.Printf("paired on %s:\n%s", l.Daemon, textclean.Clean(approverRows(l.Approvers)))
			}
			// Joined projects' asks are decided at their authorities, where
			// these approvers decide nothing.
			if rs, err := c.Remotes(ctx); err == nil {
				for _, rm := range rs {
					if rm.Authority != "" && rm.Authority != l.Daemon {
						fmt.Printf("project %s: asks decided at %s, not here (zm approver ls -P %s)\n", rm.Project.Slug, rm.Authority, rm.Project.Slug)
					}
				}
			}
			return nil
		},
	}
	rm := &cobra.Command{
		Use: "rm <name>", Short: "Remove an approver (exact name; takes effect at once)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Approver, error) {
				return orchclient.RemoveApprover(ctx, c, args[0])
			},
				func(a orchapi.Approver) string { return "removed approver " + a.Name })
		},
	}
	ap.AddCommand(add, ls, rm)
	rootCmd.AddCommand(ap, askCmd())
}

// approverRows prints approvers one per line.
func approverRows(as []orchapi.Approver) string {
	var b strings.Builder
	for _, a := range as {
		dev := a.NodeName
		if a.State == orchapi.ApproverPending && a.CodeExpires != nil {
			dev = "code expires " + a.CodeExpires.Local().Format(time.DateTime)
		}
		conf := ""
		if a.ConfirmedBy != "" {
			conf = "confirmed by " + a.ConfirmedBy
		}
		if a.Identity != "" {
			conf = strings.TrimSpace(conf + " · for " + a.Identity)
		}
		fmt.Fprintf(&b, "%-16s %-12s %-24s %s\n", a.Name, a.State, dev, conf)
		if a.Note != "" {
			fmt.Fprintf(&b, "  ! %s\n", a.Note)
		}
	}
	return b.String()
}

// approverPlace is where project's asks are decided and the approvers there:
// a joined project's authority answers GET /v0/approvers/self
// (its full list is local to it); else this daemon lists all of its own.
func approverPlace(ctx context.Context, c *client.Client, project string) (orchapi.ApproverPlace, error) {
	want := strings.SplitN(project, "/", 2)[0]
	p := orchapi.ApproverPlace{Project: want}
	rs, err := c.Remotes(ctx)
	if err != nil {
		return p, err
	}
	for _, rm := range rs {
		if rm.Project.Slug == want || rm.Project.ID == want {
			p.Project, p.DecidedAt, p.Joined = rm.Project.Slug, rm.Authority, true
			break
		}
	}
	var l orchapi.ApproversAt
	if p.Joined {
		l, err = orchclient.MyApprovers(ctx, c)
		p.Only = "yours: the authority's full list is local to it"
	} else {
		l, err = orchclient.ApproversHere(ctx, c)
	}
	if err != nil {
		return p, err
	}
	p.DecidedAt = first(l.Daemon, p.DecidedAt)
	p.Approvers = l.Paired()
	return p, nil
}

func approverPlaceText(p orchapi.ApproverPlace) string {
	var b strings.Builder
	if !p.Joined {
		fmt.Fprintf(&b, "project %s: asks decided here, on %s\n", p.Project, p.DecidedAt)
		if len(p.Approvers) == 0 {
			b.WriteString("no approvers here (zm approver add <name>)\n")
		}
	} else {
		fmt.Fprintf(&b, "project %s: asks decided at %s (its authority); your approvers there:\n", p.Project, p.DecidedAt)
		if len(p.Approvers) == 0 {
			fmt.Fprintf(&b, "none: pair your phone there with zm approver pair-me -P %s\n", p.Project)
		}
	}
	as := make([]orchapi.Approver, 0, len(p.Approvers))
	for _, a := range p.Approvers {
		as = append(as, a.Approver)
	}
	b.WriteString(approverRows(as))
	return b.String()
}

func askCmd() *cobra.Command {
	ask := &cobra.Command{Use: "ask", Short: "Ask a person to decide (a note card with a subject; the project's owner answers, or a paired device)"}
	var req orchapi.NewAskRequest
	var card orchapi.AskCard
	var forAsk orchapi.AskFor
	var choices []string
	var priority int
	var by string
	var ackMail []string
	var ackOn string
	var attach []string
	var verify string
	var act askActionFlags
	var step askStepFlags
	newCmd := &cobra.Command{
		Use:   "new <question...|->",
		Short: "Ask for a decision on a work item, a plain question or a pull request's head commit",
		Long: `Ask a person to decide. Go with your gut unless it could truly go either way: a person's
attention is the scarcest thing on the team. When you decide yourself, record it on the item:
zm work comment <id> "AGENT DECISION: <what you chose and why>". People read those as a quiet log.

First look for an earlier answer: zm ask similar <a few words>. If an answered ask settles your
question, it is your answer: record it as an AGENT DECISION naming that ask, and ask nobody. If an
open ask is the same question, join it (zm ask join <id> --work <yours>): one card, and its answer
reaches you and your item too. Whether it is the same question is your call.

Asking never blocks: this returns at once. Put your default in --recommend and keep working on it;
the answer comes back later. Prefer this to your agent's own blocking question tool, except before
an irreversible step.

When waiting costs more than a choice that is easy to change back, add --apply-default (with
--recommend and --by): if nobody answers by then, zm records DEFAULT APPLIED on the item (not an
owner decision) and replies to your ask, so proceed with your recommendation. The person can still
undo it; DEFAULT UNDONE mail then tells you to put it back. zm refuses it when the ask or its item
names an irreversible step: a production write, a publish, an outbound message, money, access or
a secret.

A work item (--work) or a plain question (neither --work nor --repo) needs a note card, in plain
words with no ids, hashes or paths: --context (explain it like they are 15), the question, --choice
(2 to 4: "label | pros | cons"; they become a, b, c, d), --recommend, --why, --searched.
--attach <file> (repeatable, needs --work) puts an image or a static HTML mockup on the card:
one self-contained page with inline CSS and data: images; scripts and network do not run. The card stays
decidable alone. The answer lands on the work item whoever asked. --ack-mail <ids> makes one choice (--ack-on, default a) ack
exactly those mails as the approver, needs=human ones too: the ids are frozen when you ask, and the
card shows their count. A pull request (--repo, --pr, --head) needs no card, and a CI
gate can verify its receipt.

A judge (a small model) reads every card before anyone sees it and sends it back with fixes unless
a person who does not code could decide from it alone (what it is, why now, what each choice changes,
its cost or risk) and every step a choice leaves someone is carried by an action that does all of it
after one press; a person only judges. If the judge does not answer, the ask is held and sent only
after a pass. When the approver picks the action's choice (--ack-on, default a) on a
paired device or in the cockpit app, zm does exactly that, as them, and the card shows how it went:
  --run-on <machine> --script <path> --ref <ref> --commit <full sha> [--arg ...]   a script run
  --run-on <machine> --agent claude|codex --brief <file>                          an agent task
  --run-on <machine> --command '<one line>' --ref <ref> --commit <full sha>        one command, shown verbatim
  --authorize-key <file.pub> --on-machine <machine> --user <account>              adds an SSH key
  --hand --agent claude|codex --brief <file> --verify '<check>'   no machine of theirs takes tasks:
    their own agent (Hand to my agent) works from the numbered brief, setup steps included
  --browser-on <machine> --plan <file> --origin <url>... [--sign-in <name>] [--submit]  (refused
until a browser runtime enforces the scope). The machine is a trusted machine of the project that
takes remote tasks: zm ask machines human:<name> lists the approver's, and with --to human:<name>
and no --run-on zm picks their only one. The run's result mails you. Never mail the step as a
report: zm refuses a report that asks a person to run, deploy or log in.

A script or command run's --ref is a branch or tag name (main, v1.2), never refs/heads/... or
refs/tags/...; --commit is the full 40-character id git rev-parse <ref> prints now (a tag's commit,
not the tag object). zm checks both on the project's remote when you ask and refuses a card that
could not run. The run uses exactly that commit, never a newer one: when zm saw it was the ref's
head when you asked, it still runs after the branch moves on (while the commit is on it); when zm
could not reach the remote to check, it runs only while the ref still points at it. A tag is the
safest ref for a card that may wait. If an approved run fails before it starts (its machine never
took it, or zm could not hand it over), zm tells you and the approver, and you or a project owner
can start the identical action again within an hour of the yes, without a new card: zm ask retry <id>.

A step only the person can do (a sign-in, a press in a service's own console; never something an
agent could do with access) goes on the card as their step, not an action: --to human:<name>
--step-label '<what>' --step '<1st>' --step '<2nd>' --step-link https://<the service's page>
[--step-minutes 2] --verify '<check>'. Any member of the project may be the person; their machine
need not take tasks. After the yes it is their turn until zm ask check passes (they press "I did
it": zm ask step-done, and you are mailed to run the check); your evidence never closes it.

When a yes means work gets done (by an action, by you, or by them), add --verify '<command>': one
shell line that exits 0 only once all of it is done (check every side, not one). A yes then records
APPROVED, NOT DONE on the item; run zm ask check <id> when you think it is done. It comes back to
you, then to the approver, until the check passes. Never close the item before DONE.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			stdin := 0
			if len(args) == 1 && args[0] == "-" {
				stdin++
			}
			for _, a := range attach {
				if a == "-" {
					stdin++
				}
			}
			if stdin > 1 {
				return fmt.Errorf(`stdin ("-") is read once: give the question or one --attach as "-", the rest as arguments or files`)
			}
			body, err := joinArgs(args)
			if err != nil {
				return err
			}
			r := req
			r.Body = body
			var files [][]byte
			if len(attach) > 0 {
				if r.Subject.Work == "" {
					return fmt.Errorf("--attach goes on the work item the ask is about: add --work <id>")
				}
				if len(attach) > orchapi.MaxIssueAttachments {
					return fmt.Errorf("--attach: at most %d files", orchapi.MaxIssueAttachments)
				}
				for _, name := range attach {
					data, err := readAttachment(name)
					if err != nil {
						return err
					}
					files = append(files, data)
				}
			}
			if card.Context != "" || len(choices) > 0 || r.Subject.Work != "" || (r.Subject.Repo == "" && r.Subject.PR == 0 && r.Subject.HeadSHA == "") {
				c := card
				c.Question = body
				for _, ch := range choices {
					parts := strings.SplitN(ch, "|", 4)
					x := orchapi.AskChoice{Label: strings.TrimSpace(parts[0])}
					if len(parts) > 1 {
						x.Pros = strings.TrimSpace(parts[1])
					}
					if len(parts) > 2 {
						x.Cons = strings.TrimSpace(parts[2])
					}
					if len(parts) > 3 {
						x.Effect = strings.TrimSpace(parts[3])
					}
					c.Choices = append(c.Choices, x)
				}
				if r.Subject.Repo == "" && r.Subject.PR == 0 && r.Subject.HeadSHA == "" {
					if err := cardFlagsMissing(c); err != nil {
						return err // a pull request's ask takes no card: the daemon says so
					}
				}
				if len(ackMail) > 0 {
					c.Action = &orchapi.AskAction{Kind: orchapi.ActionAckMail, IDs: ackMail, On: ackOn}
				}
				a, err := act.action(ackOn)
				if err != nil {
					return err
				}
				if a != nil {
					if c.Action != nil {
						return fmt.Errorf("one action per card: --ack-mail or one of --run-on, --browser-on, --authorize-key")
					}
					c.Action = a
				}
				if verify != "" {
					c.Verify = &orchapi.AskVerify{Command: verify, On: ackOn}
				}
				if c.OwnerStep, err = step.step(); err != nil {
					return err
				}
				r.Card = &c
			}
			if forAsk.Group != "" || forAsk.Contact != "" {
				f := forAsk
				r.For = &f
			}
			if cmd.Flags().Changed("priority") {
				r.Priority = &priority
			}
			if by != "" {
				t, err := time.Parse(time.RFC3339, by)
				if err != nil {
					return fmt.Errorf("--by: an RFC 3339 time, e.g. 2026-11-07T17:00:00-08:00: %w", err)
				}
				r.By = &t
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Ask, error) {
				return orchclient.NewAskWithFiles(ctx, c, r, files, attach)
			},
				func(a orchapi.Ask) string {
					if a.Updated {
						return fmt.Sprintf("ask %s %s. Wait: zm ask show %s", a.ID, a.Note, a.ID)
					}
					if strings.HasPrefix(a.Note, "held") {
						return fmt.Sprintf("ask %s %s. Wait: zm ask show %s", a.ID, a.Note, a.ID)
					}
					s := fmt.Sprintf("ask %s sent %s (%s). Wait: zm ask show %s", a.ID, askRecipient(a), subjectLine(a.Subject), a.ID)
					if a.ApplyDefault && a.By != nil {
						s += fmt.Sprintf("\nwith no answer by %s, the recommendation applies and you get a DEFAULT APPLIED reply", a.By.Local().Format(time.DateTime))
					}
					if a.OwnerVia != "" {
						s += "\n" + a.OwnerVia
					}
					if a.Note != "" {
						s += "\n" + a.Note
					}
					return s
				})
		},
	}
	f := newCmd.Flags()
	f.StringVar(&req.Subject.Work, "work", "", "the work item it is about (bd:zm-1 or zm-1)")
	f.StringVar(&req.Subject.Repo, "repo", "", "pull request: repository, owner/name or host/owner/name")
	f.IntVar(&req.Subject.PR, "pr", 0, "pull request: its number")
	f.StringVar(&req.Subject.HeadSHA, "head", "", "pull request: its full head commit id (git rev-parse HEAD)")
	f.StringVar(&req.To, "to", "owners", "who it is for: a person's name, alias or GitHub login (zm people lists them), me (the person who started your session, else the work item's owner: label, else owners), or owners; a person must own or administer the project")
	f.StringVar(&req.Area, "area", "", "area inside the project")
	f.StringSliceVar(&req.Options, "option", nil, "pull request: answers to offer (default approve, decline)")
	f.StringVar(&card.Title, "title", "", "card: a short headline (default: the question)")
	f.StringVar(&card.Context, "context", "", "card: explain it like the reader is 15")
	f.StringVar(&card.Background, "background", "", "card: what else they need to know")
	f.StringArrayVar(&choices, "choice", nil, `card: "label | pros | cons | effect", 2 to 4 (keys a, b, c, d); effect: what picking it changes, one line`)
	f.StringVar(&card.Recommend, "recommend", "", "card: the choice you would pick (a, b, ...)")
	f.StringVar(&card.Why, "why", "", "card: what the recommendation does, good and bad")
	f.StringVar(&card.AfterPress, "after-press", "", "card: who acts after the press and how the person will know")
	f.StringArrayVar(&attach, "attach", nil, "card: an image or a self-contained HTML page (inline CSS, data: images; no scripts or network) to show on it (repeatable, at most 8; needs --work)")
	f.StringVar(&card.Searched, "searched", "", "card: what you checked before asking, and what you found")
	f.IntVar(&priority, "priority", 2, "0 (first) to 4")
	f.StringVar(&req.Topic, "topic", "", "one ask per subject and topic (several decisions on one work item)")
	f.StringVar(&req.ThreadTitle, "thread-title", "", "card: the title of the thread you are asking from, shown beside the card (not card text)")
	f.StringVar(&req.RepeatKey, "repeat-key", "", "card: the id of the thing that can break again (an automation's id); while a card with this key is open, zm counts this ask on it instead of filing another")
	f.StringVar(&req.Latest, "latest", "", "card, with --repeat-key: its newest state in one plain line, shown with the count")
	f.StringVar(&req.PRURL, "pr-url", "", "card: an https pull request URL the card is about, shown as a link (not card text)")
	f.StringVar(&req.Owner, "owner", "", "the person it is for: any of their names, aliases or GitHub login (zm people lists them), an owner or admin of the project (default: whom the work item's owner:<name> label names)")
	f.StringSliceVar(&ackMail, "ack-mail", nil, "card: mail ids zm acks as the approver if they pick --ack-on (frozen now; you must see them)")
	f.StringVar(&ackOn, "ack-on", "", "card: the choice that runs the card's action (default the first, a)")
	act.flags(f)
	step.flags(f)
	f.StringVar(&verify, "verify", "", "card: a shell line that exits 0 once the approved work is done; zm ask check <id> runs it (on --ack-on's choice)")
	f.StringVar(&by, "by", "", "when you need it, RFC 3339 (deadlines come first in the inbox)")
	f.BoolVar(&req.ApplyDefault, "apply-default", false, "reversible only: at --by with no answer, zm applies --recommend as DEFAULT APPLIED and replies to you")
	f.StringVar(&forAsk.Group, "program", "", "held for someone outside the team: their organisation (needs --for and --topic; no --by)")
	f.StringVar(&forAsk.Contact, "for", "", "held for someone outside the team: who there decides")
	f.StringVar(&forAsk.Ref, "program-ref", "", "held for someone outside the team: the organisation's id elsewhere")
	f.StringVar(&forAsk.ContactRef, "for-ref", "", "held for someone outside the team: the contact's id elsewhere")
	similar := &cobra.Command{
		Use: "similar <words...>", Short: "Earlier asks sharing your question's words: answered ones carry their answer", Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) ([]orchapi.Ask, error) {
				return orchclient.SimilarAsks(ctx, c, flagProject, strings.Join(args, " "))
			},
				func(as []orchapi.Ask) string {
					var b strings.Builder
					for _, a := range as {
						state := "open (join it: zm ask join " + a.ID + " --work <yours>)"
						if d := a.Decision; d != nil {
							state = "answered: " + firstLine(first(d.Answer, d.Decision))
						}
						fmt.Fprintf(&b, "%s %s\n  %s\n", a.ID, a.Card.Question, state)
					}
					if b.Len() == 0 {
						return "nothing similar was asked: ask (zm ask new)"
					}
					return b.String()
				})
		},
	}
	var join orchapi.NewAskRequest
	joinCmd := &cobra.Command{
		Use:   "join <id>",
		Short: "Ask the same question as an open ask: one card, its answer reaches you and your item",
		Long: `Join an open ask with the same question instead of asking again. Your own ask takes its card,
priority and owner; the owner's one answer decides every ask of the group, each landing on its own
work item and reaching its asker. An ask already answered is refused with its answer: that answer
is yours (record it as an AGENT DECISION naming the ask).`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r := join
			r.SameAs = args[0]
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Ask, error) { return orchclient.NewAsk(ctx, c, r) },
				func(a orchapi.Ask) string {
					return fmt.Sprintf("ask %s joined %s (%s). Wait: zm ask show %s", a.ID, a.SameAs, subjectLine(a.Subject), a.ID)
				})
		},
	}
	joinCmd.Flags().StringVar(&join.Subject.Work, "work", "", "your work item (its answer lands there too)")
	joinCmd.Flags().StringVar(&join.Area, "area", "", "area inside the project")
	var open bool
	ls := &cobra.Command{
		Use: "ls", Short: "Asks you may see", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) ([]orchapi.Ask, error) {
				return orchclient.Asks(ctx, c, flagProject, open)
			},
				func(as []orchapi.Ask) string {
					var b strings.Builder
					for _, a := range as {
						fmt.Fprintf(&b, "%s %-10s %s  %s\n", a.ID, askState(a), subjectLine(a.Subject), firstLine(a.Body))
					}
					if b.Len() == 0 {
						return "no asks"
					}
					return b.String()
				})
		},
	}
	ls.Flags().BoolVar(&open, "open", false, "only undecided asks")
	show := &cobra.Command{
		Use: "show <id>", Short: "One ask and its decision", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Ask, error) {
				return orchclient.GetAsk(ctx, c, args[0])
			},
				func(a orchapi.Ask) string {
					s := fmt.Sprintf("ask %s in %s: %s\n%s\n%s\n", a.ID, a.Project, subjectLine(a.Subject), askRecipient(a), a.Body)
					for _, at := range a.Attachments {
						s += fmt.Sprintf("attachment %s %s %d bytes (zm issues attachments get %s %s)\n", at.ID, at.Type, at.Size, a.Subject.Work, at.ID)
					}
					if r := a.Repeat; r != nil && r.Seen > 1 {
						s += fmt.Sprintf("seen %d times, last at %s", r.Seen, r.LastSeen.Local().Format(time.DateTime))
						if r.Latest != "" {
							s += ": " + r.Latest
						}
						s += "\n"
					}
					if d := a.Decision; d != nil {
						s += fmt.Sprintf("decided: %s by approver %s at %s (receipt: zm ask receipt %s)", first(d.Answer, d.Decision), d.Approver,
							d.Time.Local().Format(time.DateTime), a.ID)
						if d.NotTheirs != "" {
							s += fmt.Sprintf("\nnot %s's answer: %s did not decide it", d.NotTheirs, d.NotTheirs)
						}
						if d.WriteError != "" {
							s += "\nnot on the work item yet: " + d.WriteError
						}
						return s
					}
					if a.Status == orchapi.AskWithdrawn {
						return s + "withdrawn"
					}
					return s + defaultLine(a)
				})
		},
	}
	receipt := &cobra.Command{
		Use: "receipt <id>", Short: "Print an ask's signed receipt (JSON) for zm receipt verify or a PR", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			a, err := orchclient.GetAsk(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			if a.Decision == nil {
				return api.Errorf(api.CodeConflict, "ask %s is not decided yet", a.ID)
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(a.Decision.Receipt)
		},
	}
	undo := &cobra.Command{
		Use: "undo <id>", Short: "Take back a default zm applied at an ask's deadline (the owner); the asker puts it back", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Ask, error) {
				return orchclient.UndoDefault(ctx, c, "", args[0])
			},
				func(a orchapi.Ask) string {
					return "ask " + a.ID + ": default undone; the asker was told to put it back. Answer it: zm ask answer " + a.ID + " <choice>"
				})
		},
	}
	withdraw := &cobra.Command{
		Use: "withdraw <id>", Short: "Take back an open ask you no longer need answered", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Ask, error) {
				return orchclient.WithdrawAsk(ctx, c, args[0])
			},
				func(a orchapi.Ask) string { return "ask " + a.ID + " " + a.Status })
		},
	}
	var answer orchapi.DecideRequest
	answerCmd := &cobra.Command{
		Use:   "answer <id> [choice]",
		Short: "Answer an ask as the project's owner (a choice's key, or --text alone)",
		Long: `Answer a note card's ask as its project's owner or admin, from your own machine (on a member
machine it goes to the project's authority). The choice is a card key (a, b, c, d); --text adds a
written answer, or alone records "other". "needs_context" with --text sends the ask back: nothing is
decided, nothing lands on the work item, and the asker gets your note. A pull request's ask is decided
on a paired device.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := answer
			if len(args) == 2 {
				req.Decision = strings.ToLower(strings.TrimSpace(args[1]))
			} else if strings.TrimSpace(req.Text) == "" {
				return fmt.Errorf("give a choice (a, b, ...) or --text")
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Decision, error) {
				return orchclient.Choose(ctx, c, "", args[0], req)
			},
				func(d orchapi.Decision) string {
					s := fmt.Sprintf("ask %s decided: %s, by %s on %s", d.Ask, first(d.Answer, d.Decision), d.Approver, first(d.Machine, d.Device))
					if d.MailError != "" {
						s += "\nthe asker's mail was not answered yet: " + d.MailError
					}
					return s
				})
		},
	}
	answerCmd.Flags().StringVar(&answer.Text, "text", "", "a written answer (alone: decision other)")
	answerCmd.Flags().StringVar(&answer.As, "as", "", "owner (default) or contact: what the ask's outside contact said")
	var status string
	var everyone, agents bool
	inbox := &cobra.Command{
		Use: "inbox", Short: "What waits on a person: open asks, then labelled steps with no ask", Args: cobra.NoArgs,
		Long: `What waits on a person: open asks, then labelled steps with no ask.

A question labelled for a person (questions, owner-decision, ask:<who>, type decision) with no ask
waits on an agent, not a person: --agents lists them. For each, write the ask (zm ask new --work,
with choices and a recommendation), or, when the item states a default and nothing irreversible
hangs on it, record zm work comment <id> "AGENT DECISION: ..." and close it.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if everyone && agents {
				return fmt.Errorf("give --everyone or --agents, not both")
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Inbox, error) {
				switch {
				case everyone:
					return orchclient.DecisionsFor(ctx, c, flagProject, status, "everyone")
				case agents:
					return orchclient.DecisionsFor(ctx, c, flagProject, status, "agents")
				}
				return orchclient.Decisions(ctx, c, flagProject, status)
			},
				func(in orchapi.Inbox) string {
					if agents && len(in.Decisions) == 0 {
						return "no question waits on an agent"
					}
					return askInboxText(in)
				})
		},
	}
	inbox.Flags().StringVar(&status, "status", "", "open, answered, recorded, withdrawn (default open,answered)")
	inbox.Flags().BoolVar(&everyone, "everyone", false, "work waiting on anyone, not only on you")
	inbox.Flags().BoolVar(&agents, "agents", false, "questions with no ask yet: an agent writes the ask or decides")
	ask.AddCommand(newCmd, similar, joinCmd, ls, show, receipt, undo, withdraw, askCheckCmd(), askRetryCmd(), answerCmd, inbox, askMachinesCmd(), askContentCountCmd(), askObjectCmd(), askStepDoneCmd(), askBundleCmd())
	return ask
}

// askRecipient says whom an ask is for: the person it names, else where its
// mail went.
func askRecipient(a orchapi.Ask) string {
	if a.Owner != "" {
		return "for human:" + a.Owner + " (their Decisions)"
	}
	if a.To == "owners" || a.To == "" {
		return "to the project's owners"
	}
	return "to " + a.To
}

// askInboxText prints the ask inbox. A joined ask (same_as) is one card: it
// prints under the ask it joined.
func askInboxText(in orchapi.Inbox) string {
	var b strings.Builder
	listed := map[string]bool{}
	joined := map[string][]orchapi.Ask{}
	for _, x := range in.Decisions {
		if x.Ask != nil {
			listed[x.Ask.ID] = true
		}
	}
	for _, x := range in.Decisions {
		if x.Ask != nil && x.Ask.SameAs != "" && listed[x.Ask.SameAs] {
			joined[x.Ask.SameAs] = append(joined[x.Ask.SameAs], *x.Ask)
		}
	}
	party := ""
	for _, x := range in.Decisions {
		if x.Ask != nil {
			if x.Ask.SameAs != "" && listed[x.Ask.SameAs] {
				continue
			}
			// Asks held for an outside party follow the reader's, by party: they
			// wait on that party, so each party gets a heading of its own.
			if f := x.Ask.For; f != nil && x.Ask.Status == orchapi.AskOpen && f.Group != party {
				party = f.Group
				fmt.Fprintf(&b, "\nwaiting on %s (theirs to answer; record what they said):\n", f.Group)
			}
			title := firstLine(x.Ask.Body)
			if x.Ask.Card != nil {
				title = x.Ask.Card.Title
			}
			fmt.Fprintf(&b, "P%d %-10s %s %s  %s\n", x.Priority, x.Ask.Status, x.ProjectSlug, x.Ask.ID, title)
			if line := uncheckedLine(*x.Ask); line != "" {
				fmt.Fprintf(&b, "   %s\n", line)
			}
			if x.Ask.ApplyDefault && x.Ask.Decision == nil {
				fmt.Fprintf(&b, "   %s\n", defaultLine(*x.Ask))
			}
			for _, j := range joined[x.Ask.ID] {
				fmt.Fprintf(&b, "   %-10s %s %s  same question, %s\n", "joined", x.ProjectSlug, j.ID, subjectLine(j.Subject))
			}
		} else if x.Work != nil {
			fmt.Fprintf(&b, "P%d %-10s %s %s  %s\n", x.Priority, "no ask", x.ProjectSlug, x.Work.ID, x.Work.Title)
		}
	}
	if b.Len() == 0 {
		return "nothing waits on a person"
	}
	return b.String()
}

// uncheckedLine marks a card the card judge could not check (held: it
// reached nobody and takes only a signed release) or one released unchecked;
// empty for a card the judge passed.
func uncheckedLine(a orchapi.Ask) string {
	switch {
	case a.NotChecked && a.Decision == nil:
		why := ""
		if r := strings.Join(strings.Fields(a.NotCheckedReason), " "); r != "" {
			why = " (" + r + ")"
		}
		return fmt.Sprintf("NOT CHECKED: the card judge could not check it%s, so nobody it is for has seen it; an owner releases it with a signed answer (Release in the Mezze app, or a paired device)", why)
	case a.ReleasedUnchecked != "":
		return "released by " + a.ReleasedUnchecked + " without the card judge's check"
	}
	return ""
}

// defaultLine is an undecided ask's state, in the reader's zone: what its
// default does and when, or that it applied and can be undone.
func defaultLine(a orchapi.Ask) string {
	if !a.ApplyDefault || a.By == nil || a.Card == nil {
		return "not decided yet"
	}
	label := a.Card.Recommend
	for i, c := range a.Card.Choices {
		if c.Key == a.Card.Recommend || (c.Key == "" && string(rune('a'+i)) == a.Card.Recommend) {
			label = c.Label
		}
	}
	at := a.By.Local().Format("Mon 15:04 MST")
	switch {
	case a.DefaultUndone != nil:
		return fmt.Sprintf("default %q undone by %s; not decided yet", label, a.DefaultUndoneBy)
	case a.DefaultApplied != nil:
		return fmt.Sprintf("DEFAULT APPLIED at %s: %q (undo: zm ask undo %s; keep it: zm ask answer %s %s)", a.DefaultApplied.Local().Format("Mon 15:04 MST"), label, a.ID, a.ID, a.Card.Recommend)
	}
	return fmt.Sprintf("I will do %q at %s unless you say no", label, at)
}

// subjectLine is what an ask is about, on one line.
func subjectLine(s orchapi.AskSubject) string {
	switch {
	case s.Work != "":
		return s.Work
	case s.Question != "":
		return "question"
	}
	head := s.HeadSHA
	if len(head) > 12 {
		head = head[:12]
	}
	return fmt.Sprintf("%s #%d @ %s", s.Repo, s.PR, head)
}

func first(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

func askState(a orchapi.Ask) string {
	if a.Decision != nil {
		return a.Decision.Decision
	}
	if a.Status != "" {
		return a.Status
	}
	return "open"
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}

// tailnetNodeName is this daemon's tailnet node name, as zm tailnet up picks
// it (kit/tailnet): the running node's; else the one config.json records;
// else zm-<machine> for a node made before the rename's step 2 (its state is
// in the home); else potluck-<machine> for a new one.
func tailnetNodeName(cfg *config.Config, st api.TailnetStatus) string {
	if st.Hostname != "" {
		return st.Hostname
	}
	if h := strings.ToLower(strings.TrimSpace(cfg.File.Tailnet.Hostname)); h != "" {
		return h
	}
	if ents, err := os.ReadDir(cfg.Path(tailnet.StateDir)); err == nil && len(ents) > 0 {
		return api.NodePrefix + cfg.Machine
	}
	return api.NodePrefixNew + cfg.Machine
}
