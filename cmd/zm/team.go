package main

// Teams (SPEC 17.3, 17.4): zm invite, zm join, zm leave, zm remotes, and
// zm project team|solo (registered from project.go's projectCmd).

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/project"
)

func init() {
	rootCmd.AddCommand(inviteCmd(), joinCmd(), leaveCmd(), remotesCmd(), outboxCmd(), t3Cmd(), machineClassCmd())
}

func projectModeCmds() []*cobra.Command {
	team := &cobra.Command{
		Use:   "team <slug>",
		Short: "Make this machine the project's authority, so members can join it (moves its leases into project.db)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (api.ProjectModeResponse, error) {
				return c.ProjectTeam(ctx, args[0])
			},
				func(r api.ProjectModeResponse) string {
					return fmt.Sprintf("%s is a team project; this machine is its authority (%d leases moved)\nnext: zm tailnet up; zm invite %s --as <name> --role member",
						r.Project.Slug, r.Moved, r.Project.Slug)
				})
		},
	}
	solo := &cobra.Command{
		Use:   "solo <slug>",
		Short: "Make a team project solo again (refused while a replica is joined)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (api.ProjectModeResponse, error) {
				return c.ProjectSolo(ctx, args[0])
			},
				func(r api.ProjectModeResponse) string {
					return fmt.Sprintf("%s is solo again (%d leases moved)", r.Project.Slug, r.Moved)
				})
		},
	}
	cancel := &cobra.Command{
		Use:   "cancel <write-id>",
		Short: "Settle a write id at the project's authority: it ran, or it never will (after a --no-queue write with unknown outcome)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (api.WriteCancelResponse, error) {
				return c.CancelWrite(ctx, flagProject, args[0])
			}, func(r api.WriteCancelResponse) string {
				if r.State == api.WriteRan {
					return fmt.Sprintf("%s already ran (status %d)", args[0], r.Status)
				}
				return args[0] + " cancelled: it never ran and never will"
			})
		},
	}
	team.AddCommand(cancel)
	return []*cobra.Command{team, solo}
}

func inviteCmd() *cobra.Command {
	var req api.InviteRequest
	var ttl time.Duration
	cmd := &cobra.Command{
		Use:   "invite <project>[/<area>] --as NAME --role member|guest [--class trusted|public]",
		Short: "Invite a member's machine into a team project: prints a one-time join code (--class: the machine's operator class once it joins)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Project = args[0]
			req.TTLSeconds = int64(ttl / time.Second)
			return call(cmd, func(ctx context.Context, c *client.Client) (api.InviteResponse, error) { return c.Invite(ctx, req) },
				func(r api.InviteResponse) string {
					where := r.Invite.Project
					if r.Invite.Area != "" {
						where += "/" + r.Invite.Area
					}
					how := "logged in to the same tailnet"
					if strings.HasPrefix(r.Code, "http") {
						how = "no tailnet needed"
					}
					class := ""
					if r.Invite.Class != "" {
						class = ", its machine " + r.Invite.Class
					}
					return fmt.Sprintf("invite %s: %s as %s in %s%s, until %s\n\n  %s\n\non %s's machine (%s): zm join '%s'",
						r.Invite.ID, r.Invite.As, r.Invite.Role, where, class, r.Invite.Expires.Local().Format("Jan 2 15:04"), r.Code, r.Invite.As, how, r.Code)
				})
		},
	}
	cmd.Flags().StringVar(&req.As, "as", "", "the member's identity name (created at redemption if new)")
	cmd.Flags().StringVar(&req.Role, "role", "member", "member or guest")
	cmd.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "how long the code works")
	cmd.Flags().BoolVar(&req.AllowTagged, "allow-tagged", false, "allow a tagged tailnet node (a CI or farm box) to redeem it")
	cmd.Flags().StringVar(&req.Via, "via", "", "tailnet or tunnel (an https code for a home behind cloudflared; default: the tailnet when it runs)")
	cmd.Flags().StringVar(&req.Class, "class", "", "the operator class the machine that redeems it gets: trusted (gets the project's secrets and code) or public (secret-free work only); default: trusted for a member of the whole project, public otherwise")
	_ = cmd.MarkFlagRequired("as")
	ls := &cobra.Command{
		Use:   "ls",
		Short: "Invites on this machine (live, redeemed, expired)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) ([]api.Invite, error) { return c.Invites(ctx) },
				func(is []api.Invite) string {
					var b strings.Builder
					for _, i := range is {
						where := i.Project
						if i.Area != "" {
							where += "/" + i.Area
						}
						extra := "until " + i.Expires.Local().Format("Jan 2 15:04")
						if i.State == api.InviteRedeemed {
							extra = "by " + i.Machine
							if i.Used {
								extra += " (in use)"
							}
						}
						if i.Class != "" {
							extra += ", " + i.Class
						}
						fmt.Fprintf(&b, "%s  %-8s %-10s %-7s %-20s %s\n", i.ID, i.State, i.As, i.Role, where, extra)
					}
					return b.String()
				})
		},
	}
	rm := &cobra.Command{
		Use:   "rm <id>",
		Short: "Remove an invite (a redeemed one's token stays: zm token revoke)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (api.Invite, error) { return c.RemoveInvite(ctx, args[0]) },
				func(i api.Invite) string { return "removed invite " + i.ID })
		},
	}
	cmd.AddCommand(ls, rm)
	return cmd
}

func joinCmd() *cobra.Command {
	var req api.JoinRequest
	var wait time.Duration
	var accessFile string
	cmd := &cobra.Command{
		Use:   "join <code|project[/area]>",
		Short: "Join a team project: with a join code from its owner (a tailnet code starts this machine's tailnet node; an https code needs no tailnet), or by name when it granted your GitHub account (zm grant @you)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if proj, area, ok := strings.Cut(args[0], "/"); ok && !strings.Contains(args[0], ":") {
				if !api.ValidSlug(proj) || !api.ValidSlug(area) {
					return fmt.Errorf("zm join: %q is not project/area", args[0])
				}
				req.Project, req.Area = proj, area // project/area: a GitHub join scoped to one area (SPEC 22)
				if req.From == "" {
					return fmt.Errorf("zm join %s names an area: also say where, --from https://<host>#<fingerprint> (or --from <machine> on the tailnet)", args[0])
				}
			} else if project.LooksLikeJoinCode(args[0]) {
				req.Code = args[0] // potluck-<machine>:<fingerprint>:XXXX-XXXX or zm-<machine>:... (a malformed one is refused as a code)
			} else {
				req.Project = args[0] // a project slug: a GitHub login at the project's authority (SPEC 22)
			}
			req.Cwd, _ = os.Getwd()
			if accessFile != "" {
				b, err := os.ReadFile(accessFile)
				if err != nil {
					return err
				}
				req.AccessSecret = strings.TrimSpace(string(b))
			}
			req.Paths = absAll(req.Paths)
			req.WaitMS = int(wait / time.Millisecond)
			c, _, err := newClient()
			if err != nil {
				return err
			}
			r, err := joinUntilDone(cmd.Context(), c, req)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(r)
			}
			fmt.Println(joinText(r))
			return nil
		},
	}
	cmd.Flags().StringVar(&req.From, "from", "", "with a project name: the authority's machine (asked for on a terminal; required elsewhere), or its tunnel URL with its key fingerprint, https://<host>#<fingerprint>, to join over https with no tailnet. --json prints the first answer: the authorities found, or the code to enter")
	cmd.Flags().StringVar(&req.Role, "role", "", "with project/area: the most you ask for (guest, member or owner on the tailnet; over https only guest), when your GitHub grants give it")
	cmd.Flags().StringVar(&req.Machine, "machine", "", "this machine's name at the authority (default: this daemon's machine name)")
	cmd.Flags().StringVar(&req.AuthKey, "auth-key", "", "a Tailscale auth key for this machine's node (else a login URL is printed); never saved")
	cmd.Flags().StringSliceVar(&req.Paths, "path", nil, "your checkout of the project (repeatable; default: this repo when its remote is the project's)")
	cmd.Flags().DurationVar(&wait, "wait", 10*time.Second, "how long to wait for the tailnet node each try")
	cmd.Flags().StringVar(&req.AccessID, "cf-access-id", "", "tunnel homes behind Cloudflare Access: the service token's client id")
	cmd.Flags().StringVar(&accessFile, "cf-access-secret-file", "", "a file holding that service token's client secret")
	return cmd
}

// joinUntilDone sends the join, waiting out a tailnet login and asking for
// the authority or GitHub's code as needed (with --json: the first answer).
func joinUntilDone(ctx context.Context, c *client.Client, req api.JoinRequest) (api.JoinResponse, error) {
	deadline := time.Now().Add(10 * time.Minute)
	shown := ""
	for {
		r, err := c.Join(ctx, req)
		if err != nil {
			return r, err
		}
		if r.AuthURL != "" && !flagJSON {
			if time.Now().After(deadline) {
				return r, fmt.Errorf("no tailnet login within 10 minutes")
			}
			if r.AuthURL != shown {
				fmt.Fprintf(os.Stderr, "zm: log in to your tailnet to continue (waiting): %s\n", r.AuthURL)
				shown = r.AuthURL
			}
			req.AuthKey = "" // used once
			time.Sleep(3 * time.Second)
			continue
		}
		if len(r.Authorities) > 0 && req.From == "" && !flagJSON {
			from, err := pickAuthority(req.Project, r, onATerminal(), os.Stdin, os.Stderr)
			if err != nil {
				return r, err
			}
			req.From = from
			continue
		}
		if g := r.GitHub; g != nil && !flagJSON {
			if req.Login == "" {
				showSkipped(os.Stderr, r.Skipped)
				who := "tailnet owner " + g.Owner
				if g.Owner == "" {
					_, fp, _ := strings.Cut(req.From, "#")
					who = "home key " + fp
				}
				fmt.Fprintf(os.Stderr, "zm: authority %s (%s) asks GitHub to admit %s.\nzm: open %s and enter %s (waiting, until %s).\nzm: enter a code only if you started this yourself.\n",
					g.Authority, who, g.For, g.VerificationURI, g.UserCode, g.Expires.Local().Format("15:04"))
			}
			req.Login = g.Login
			continue
		}
		return r, nil
	}
}

func joinText(r api.JoinResponse) string {
	if r.Remote == nil {
		return "nothing joined"
	}
	x := r.Remote
	var b strings.Builder
	where := x.Project.Slug
	if x.Cap.Area != "" {
		where += "/" + x.Cap.Area
	}
	fmt.Fprintf(&b, "joined %s on %s as %s (%s), machine %s\n", where, x.Authority, x.Identity, x.Cap.Role, x.Machine)
	if len(x.Areas) > 0 {
		fmt.Fprintf(&b, "  areas you see: %s\n", strings.Join(x.Areas, ", "))
	}
	fmt.Fprintf(&b, "  open work items you see: %d\n", r.Items)
	if len(x.Paths) > 0 {
		fmt.Fprintf(&b, "  checkouts: %s\n", strings.Join(x.Paths, ", "))
	}
	fmt.Fprintf(&b, "  token: %s (works only from this machine's daemon)\n", x.TokenFile)
	for _, n := range r.Notes {
		fmt.Fprintf(&b, "  note: %s\n", n)
	}
	fmt.Fprintf(&b, "next: zm work ls -P %s; zm claim <id>", x.Project.Slug)
	return b.String()
}

func leaveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "leave <project>",
		Short: "Leave a team project this machine joined (deletes its token; asks the authority to revoke it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (api.LeaveResponse, error) { return c.Leave(ctx, args[0]) },
				func(r api.LeaveResponse) string {
					s := "left " + r.Project.Slug
					if !r.Revoked {
						s += " (the authority did not answer: its owner can run zm token revoke)"
					}
					return s
				})
		},
	}
}

func t3Cmd() *cobra.Command {
	var scopes []string
	cmd := &cobra.Command{
		Use:   "t3 <machine> -P <project> [--scope S]...",
		Short: "Start or reuse the companion cockpit app on a machine of a team project you own, and mint a one-time pairing credential (--json prints it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			project := flagProject
			if project == "" {
				project = os.Getenv("ZM_PROJECT")
			}
			if project == "" {
				return fmt.Errorf("name the team project %s is in: zm t3 %s -P <project>", args[0], args[0])
			}
			req := orchapi.T3EnsureRequest{Project: project, Scopes: scopes}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.T3Environment, error) {
				return orchclient.EnsureT3(ctx, c, args[0], req)
			},
				func(e orchapi.T3Environment) string {
					how := "already running"
					if e.Started {
						how = "started"
					}
					return fmt.Sprintf("Mezze app on %s (%s, %s at %.12s): environment %s, port %d, %s\n"+
						"a pairing credential was minted (good until %s, single use); it is shown only with --json, so run again with --json to pair",
						e.Machine, how, e.Version, e.Commit, e.EnvironmentID, e.Port, strings.Join(e.URLs, " "), e.ExpiresAt.Local().Format("15:04"))
				})
		},
	}
	cmd.Flags().StringArrayVar(&scopes, "scope", nil, "a T3 scope for the credential (repeatable; default: T3's standard client scopes)")
	cmd.AddCommand(t3SwitchCmd(true), t3SwitchCmd(false)) // this machine's own switch (t3_switch.go)
	return cmd
}

func remotesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remotes",
		Short: "Team projects this machine joined, and their authorities",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) ([]api.Remote, error) { return c.Remotes(ctx) },
				func(rs []api.Remote) string {
					var b strings.Builder
					for _, x := range rs {
						where := x.Project.Slug
						if x.Cap.Area != "" {
							where += "/" + x.Cap.Area
						}
						via := x.Via
						if via == "" {
							via = api.ViaTailnet
						}
						fmt.Fprintf(&b, "%-20s %-7s on %-12s via %-7s as %-10s %s", where, x.Cap.Role, x.Authority, via, x.Identity, x.State)
						if x.LastError != "" {
							fmt.Fprintf(&b, ": %s", x.LastError)
						}
						if x.OwnerNow != "" {
							fmt.Fprintf(&b, "\n  WARNING: the authority's node now belongs to tailnet owner %s, not %s as when joined", x.OwnerNow, x.AuthorityOwner)
						}
						if x.Unsent != "" {
							fmt.Fprintf(&b, "\n  %d unsent writes saved in %s (re-send after re-joining)", x.UnsentCount, x.Unsent)
						}
						if v := x.Replica; v != nil {
							fmt.Fprintf(&b, "\n  copy %s", v.State)
							if v.Synced != nil {
								fmt.Fprintf(&b, ", synced %s ago", time.Duration(v.AgeSecs*float64(time.Second)).Round(time.Second))
							}
							if !v.Reachable {
								b.WriteString(", authority unreachable")
							}
							if v.Pending > 0 || v.Conflicts > 0 {
								fmt.Fprintf(&b, ", outbox %d pending", v.Pending)
								if v.Conflicts > 0 {
									fmt.Fprintf(&b, ", %d refused (zm outbox)", v.Conflicts)
								}
							}
							if v.LastError != "" && x.LastError == "" {
								fmt.Fprintf(&b, ": %s", v.LastError)
							}
						}
						b.WriteByte('\n')
					}
					return b.String()
				})
		},
	}
}

func outboxCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "outbox",
		Short: "Writes this replica queued for its authorities: pending, refused (conflict, failed)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) ([]api.OutboxEntry, error) {
				return c.Outbox(ctx, flagProject, all)
			},
				func(es []api.OutboxEntry) string {
					if len(es) == 0 {
						return "outbox empty"
					}
					var b strings.Builder
					for _, e := range es {
						fmt.Fprintf(&b, "%s  %-8s %-8s %-14s %-18s %s", e.Created.Local().Format("15:04:05"), e.State, e.Kind, e.Project.Slug, e.Item, e.As)
						if e.Error != "" {
							fmt.Fprintf(&b, "\n    %s", e.Error)
						}
						b.WriteByte('\n')
					}
					return b.String()
				})
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include writes the authority applied (kept until the copy shows them)")
	return cmd
}

// showSkipped lists the zm peers a join with no code did not use, and why.
func showSkipped(w io.Writer, skipped []api.JoinSkipped) {
	for _, k := range skipped {
		fmt.Fprintf(w, "zm: skipped %s: %s\n", k.Machine, k.Reason) // its node is potluck-<machine> or zm-<machine>
	}
}

// pickAuthority is the person's choice of authority for a join with no
// code: any zm node can claim to be one, so even a sole answer is shown with
// its node's tailnet owner and confirmed on a terminal; elsewhere --from
// must name it.
func pickAuthority(project string, r api.JoinResponse, terminal bool, in io.Reader, msg io.Writer) (string, error) {
	showSkipped(msg, r.Skipped)
	var names []string
	for _, a := range r.Authorities {
		names = append(names, fmt.Sprintf("%s (tailnet owner %s)", a.Machine, a.Owner))
	}
	if len(r.Authorities) != 1 || !terminal {
		return "", fmt.Errorf("zm authorities that run GitHub logins: %s; choose one: zm join %s --from <machine>", strings.Join(names, ", "), project)
	}
	a := r.Authorities[0]
	fmt.Fprintf(msg, "zm: join %s at %s (tailnet owner %s)? [y/N] ", project, a.Machine, a.Owner)
	line, _ := bufio.NewReader(in).ReadString('\n')
	if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
		return "", fmt.Errorf("not joined")
	}
	return a.Machine, nil
}

func machineClassCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "machine-class [<machine> trusted|public|default | <machine> person <name>|team] -P <project>",
		Short: "Show or set which machines of a team project are trusted with its secrets and code, and which are one person's own",
		Long: `Each machine of a team project is trusted or public there. Remote tasks (zm exec,
task_start --machine) and the cockpit app on a machine go only to trusted machines; a public
machine (a lender's or a stranger's) gets secret-free work only. By default a
machine is trusted when it joined as a member of the project or more (and the
authority's own machine always is), and public when it joined as a guest,
unless its invite carried a class (zm invite --class, zm pool create --host):
then that one. With no arguments it lists every machine's class and why;
"default" drops a class set here, going back to the invite's or the role's.

A machine is also a team machine (the default: an action an ask carries runs
there on either owner's signed yes) or one person's own: "person <name>" marks
it so, and an action an ask carries there then runs only on that person's
signed yes; cards carrying one are addressed to them. Only that person marks a
machine as theirs (one they alone joined; an agent session is refused), and
only they make it a team machine again ("team"), unless they no longer own the
project. Work started on it directly, without an ask (zm exec, task_start
--machine, the cockpit app), is not covered by this.`,
		Args: func(cmd *cobra.Command, args []string) error {
			switch {
			case len(args) == 0, len(args) == 2 && args[1] != "person", len(args) == 3 && args[1] == "person":
				return nil
			}
			return fmt.Errorf("give a machine and trusted, public, default, team or person <name>, or nothing to list")
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			project := flagProject
			if project == "" {
				project = os.Getenv("ZM_PROJECT")
			}
			if project == "" {
				return fmt.Errorf("name the team project: zm machine-class -P <project>")
			}
			line := func(c api.MachineClass) string { return fmt.Sprintf("%-20s %-8s %s", c.Machine, c.Class, c.Why) }
			personLine := func(m orchapi.MachinePerson) string {
				if m.Person == "" {
					return fmt.Sprintf("%-20s team machine: either owner's yes runs an action there", m.Machine)
				}
				return fmt.Sprintf("%-20s %s's own: only %s's yes runs an action there (set by %s)", m.Machine, m.Person, m.Person, m.SetBy)
			}
			if len(args) == 0 {
				type both struct {
					api.MachineClassList
					People    []orchapi.MachinePerson `json:"people"`
					PeopleErr string                  `json:"people_error,omitempty"`
				}
				return call(cmd, func(ctx context.Context, c *client.Client) (both, error) {
					l, err := c.MachineClasses(ctx, project)
					if err != nil {
						return both{}, err
					}
					p, err := orchclient.MachinePeople(ctx, c, project)
					if err != nil { // an older authority: the classes still show
						return both{MachineClassList: l, PeopleErr: err.Error()}, nil
					}
					return both{MachineClassList: l, People: p.Machines}, nil
				}, func(l both) string {
					own := map[string]string{}
					for _, p := range l.People {
						own[p.Machine] = p.Person
					}
					var b strings.Builder
					for _, m := range l.Machines {
						b.WriteString(line(m))
						if p := own[m.Machine]; p != "" {
							b.WriteString("; " + p + "'s own machine")
						}
						b.WriteString("\n")
					}
					if l.PeopleErr != "" {
						b.WriteString("whose machines: " + l.PeopleErr + "\n")
					}
					return strings.TrimSuffix(b.String(), "\n")
				})
			}
			if args[1] == "person" || args[1] == "team" {
				req := orchapi.MachinePersonRequest{Project: project}
				if args[1] == "person" {
					req.Person = args[2]
				}
				return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.MachinePerson, error) {
					return orchclient.SetMachinePerson(ctx, c, args[0], req)
				}, personLine)
			}
			class := args[1]
			switch class {
			case api.MachineTrusted, api.MachinePublic:
			case "default":
				class = ""
			default:
				return fmt.Errorf("class is trusted, public or default, not %q", class)
			}
			req := api.MachineClassRequest{Project: project, Class: class}
			return call(cmd, func(ctx context.Context, c *client.Client) (api.MachineClass, error) {
				return c.SetMachineClass(ctx, args[0], req)
			}, line)
		},
	}
}
