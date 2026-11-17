package main

// Owned by the integrator: project, area, grant/revoke/grants, token, identity.

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	rootCmd.AddCommand(projectCmd(), areaCmd(), grantCmd(), revokeCmd(), grantsCmd(), tokenCmd(), identityCmd())
}

// projectArg is the project a command acts on: its argument, else --project.
func projectArg(args []string) (string, error) {
	if len(args) > 0 {
		return args[0], nil
	}
	if flagProject != "" {
		return flagProject, nil
	}
	return "", fmt.Errorf("name a project (argument or --project)")
}

func projectLines(ps []api.Project) string {
	var b strings.Builder
	for _, p := range ps {
		var areas []string
		for _, a := range p.Areas {
			areas = append(areas, a.Name)
		}
		fmt.Fprintf(&b, "%-16s %-7s %s", p.Slug, p.Role, p.Name)
		if len(areas) > 0 {
			fmt.Fprintf(&b, "  areas: %s", strings.Join(areas, ", "))
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func projectCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "project", Short: "Projects: the top-level boundary for mail, work and sessions"}

	var req api.NewProjectRequest
	newCmd := &cobra.Command{
		Use:   "new <slug>",
		Short: "Create a project (you become its owner)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Slug = args[0]
			for i, p := range req.Paths {
				if abs, err := filepath.Abs(expandHome(p)); err == nil {
					req.Paths[i] = abs
				}
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (api.Project, error) { return c.CreateProject(ctx, req) },
				func(p api.Project) string { return "created project " + p.Slug + " (" + p.ID + ")" })
		},
	}
	newCmd.Flags().StringVar(&req.Name, "name", "", "display name")
	newCmd.Flags().StringSliceVar(&req.Paths, "path", nil, "a directory that belongs to the project (repeatable)")
	newCmd.Flags().StringSliceVar(&req.Remotes, "remote", nil, "a git remote that belongs to the project (repeatable)")

	ls := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "Projects you can see",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) ([]api.Project, error) {
				l, err := c.ProjectList(ctx)
				for _, sk := range l.Skipped { // a joined team project whose authority did not answer
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: a joined team project is not listed: %s\n", sk.Error)
				}
				return l.Projects, err
			}, projectLines)
		},
	}

	show := &cobra.Command{
		Use:   "show [slug | slug/area]",
		Short: "One project (or one area: acme/api) as you can see it",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := projectArg(args)
			if err != nil {
				return err
			}
			if p, a := splitProjectArea(ref); a != "" {
				return call(cmd, func(ctx context.Context, c *client.Client) (api.AreaView, error) { return c.GetArea(ctx, p, a) }, nil)
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (api.Project, error) { return c.GetProject(ctx, ref) }, nil)
		},
	}

	why := &cobra.Command{
		Use:   "why",
		Short: "Which project a write from here goes to, and which rule decided",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var none bool
			err := call(cmd, func(ctx context.Context, c *client.Client) (api.Resolution, error) {
				r, err := c.Resolve(ctx)
				none = err == nil && r.Project == nil
				return r, err
			},
				func(r api.Resolution) string {
					var b strings.Builder
					for _, s := range r.Steps {
						fmt.Fprintf(&b, "  %-16s %-40s %s\n", s.Rule, s.Input, s.Result)
					}
					if r.Project == nil {
						return "no project: writes from here are refused\n" + b.String()
					}
					where := r.Project.Slug
					if r.Area != "" {
						where += "/" + r.Area
					}
					return fmt.Sprintf("%s (rule: %s)\n%s", where, r.Rule, b.String())
				})
			if err == nil && none {
				return exitSilently(1) // scripts can test it: nothing resolves here
			}
			return err
		},
	}

	var area string
	use := &cobra.Command{
		Use:   "use <slug>",
		Short: "Mark the git repo you are in as belonging to a project (writes .zm/project.toml)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			p, err := c.GetProject(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			root := project.GitRoot(cwd)
			if root == "" {
				return fmt.Errorf("%s is not inside a git repository; use --project or ZM_PROJECT instead", cwd)
			}
			path, err := project.WriteRepoFile(root, project.RepoFile{Project: p.Slug, Area: area})
			if err != nil {
				return err
			}
			fmt.Printf("wrote %s: this repo is project %s\n", path, p.Slug)
			return nil
		},
	}
	use.Flags().StringVar(&area, "area", "", "the area inside the project")

	var dir string
	export := &cobra.Command{
		Use:   "export [slug] --dir <dir>",
		Short: "Copy a project's database and config into a directory",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := projectArg(args)
			if err != nil {
				return err
			}
			abs, err := filepath.Abs(expandHome(dir))
			if err != nil || dir == "" {
				return fmt.Errorf("--dir is required")
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (api.ExportResponse, error) {
				return c.ExportProject(ctx, ref, api.ExportRequest{Dir: abs})
			}, func(r api.ExportResponse) string { return "wrote " + strings.Join(r.Files, ", ") })
		},
	}
	export.Flags().StringVar(&dir, "dir", "", "directory to create")

	var rename, name string
	var guestIssues bool
	var paths, remotes []string
	set := &cobra.Command{
		Use:   "set [slug]",
		Short: "Change a project's slug, name, paths, remotes or whether guests write issues",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := projectArg(args)
			if err != nil {
				return err
			}
			var u api.UpdateProjectRequest
			if cmd.Flags().Changed("slug") {
				u.Slug = &rename
			}
			if cmd.Flags().Changed("name") {
				u.Name = &name
			}
			if cmd.Flags().Changed("path") || cmd.Flags().Changed("remote") {
				u.Repos = &api.RepoRules{Paths: absAll(paths), Remotes: remotes}
			}
			if cmd.Flags().Changed("guest-issues") {
				u.GuestIssues = &guestIssues
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (api.Project, error) { return c.UpdateProject(ctx, ref, u) }, nil)
		},
	}
	set.Flags().StringVar(&rename, "slug", "", "new slug")
	set.Flags().StringVar(&name, "name", "", "new display name")
	set.Flags().StringSliceVar(&paths, "path", nil, "replace the project's paths")
	set.Flags().StringSliceVar(&remotes, "remote", nil, "replace the project's remotes")
	set.Flags().BoolVar(&guestIssues, "guest-issues", false, "let the project's guests write native issues (create, comment, claim, close); =false takes it back")

	cmd.AddCommand(newCmd, ls, show, why, use, export, set, projectCloudCmd(), projectTriageCmd(), projectRulingCmd())
	cmd.AddCommand(projectModeCmds()...)
	return cmd
}

// projectCloudCmd opts the project in to cloud tasks (SPEC 8 "Cloud tasks").
func projectCloudCmd() *cobra.Command {
	var claude, codex string
	cmd := &cobra.Command{
		Use:   "cloud [slug]",
		Short: "Show or set the provider environment a cloud task of each runtime runs in (none: zm sends that runtime no code)",
		Long: "A task_start with where cloud runs on the provider's cloud computer through this machine's own claude or\n" +
			"codex login. Only a runtime the project names an environment for here may get its code: --claude takes a claude\n" +
			"cloud environment id (env_... Anthropic-hosted, ccpool_... self-hosted; or default, the account's own), --codex a\n" +
			"codex cloud exec --env id; \"\" takes it back. Claude shows an account's environments by name in /remote-env.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				flagProject = args[0]
			}
			var u orchapi.TaskCloudUpdate
			if cmd.Flags().Changed("claude") {
				u.Claude = &claude
			}
			if cmd.Flags().Changed("codex") {
				u.Codex = &codex
			}
			human := func(s orchapi.TaskCloudSettings) string {
				return fmt.Sprintf("%s: claude %s, codex %s", s.Project, cmp.Or(s.Claude, "(not opted in)"), cmp.Or(s.Codex, "(not opted in)"))
			}
			if u.Claude == nil && u.Codex == nil {
				return call(cmd, orchclient.TaskCloud, human)
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.TaskCloudSettings, error) {
				return orchclient.SetTaskCloud(ctx, c, u)
			}, human)
		},
	}
	cmd.Flags().StringVar(&claude, "claude", "", "Claude cloud environment id (env_... or ccpool_...), or default")
	cmd.Flags().StringVar(&codex, "codex", "", "Codex cloud environment id")
	return cmd
}

// projectTriageCmd shows or sets who takes mail nobody else will (SPEC 9 "Triage").
func projectTriageCmd() *cobra.Command {
	var agent, machine string
	var wait int
	cmd := &cobra.Command{
		Use:   "triage [slug]",
		Short: "Show or set the triage agent that takes group mail nobody took, and how long it waits first",
		Long: "Group mail nobody takes in --wait minutes (default two hours) goes to the machine's triage agent, else the\n" +
			"project's; with neither it closes unread after 24 hours. --agent <machine>/<name> sets the project's;\n" +
			"with --machine <m>, --agent <name> sets that machine's. \"\" takes it back. The agent gets the mail as\n" +
			"data: it decides what to do, the mail never instructs it.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				flagProject = args[0]
			}
			u := orchapi.MailTriageUpdate{Machine: machine}
			if cmd.Flags().Changed("agent") {
				u.Agent = &agent
			}
			if cmd.Flags().Changed("wait") {
				u.WaitMinutes = &wait
			}
			human := func(t orchapi.MailTriage) string {
				var b strings.Builder
				fmt.Fprintf(&b, "%s: triage agent %s, group mail waits %d min", t.Project, cmp.Or(t.Agent, "(none)"), t.WaitMinutes)
				ms := make([]string, 0, len(t.Machines))
				for m := range t.Machines {
					ms = append(ms, m)
				}
				sort.Strings(ms)
				for _, m := range ms {
					fmt.Fprintf(&b, "\n  machine %s: %s", m, t.Machines[m])
				}
				return b.String()
			}
			if u.Agent == nil && u.WaitMinutes == nil && machine == "" {
				return call(cmd, orchclient.MailTriage, human)
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.MailTriage, error) {
				return orchclient.SetMailTriage(ctx, c, u)
			}, human)
		},
	}
	cmd.Flags().StringVar(&agent, "agent", "", "the triage agent: <machine>/<name> or a session address (with --machine: a name there)")
	cmd.Flags().StringVar(&machine, "machine", "", "set this machine's triage agent instead of the project's")
	cmd.Flags().IntVar(&wait, "wait", 0, "minutes group mail waits for a taker before triage (0: the default)")
	return cmd
}

// projectRulingCmd lists and changes the project's owner rulings (SPEC 16
// "Card judge"): standing answers the card judge sends a card back with.
func projectRulingCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "ruling", Short: "List or change the project's owner rulings (the card judge quotes them)"}
	human := func(l orchapi.RulingList) string {
		if len(l.Rulings) == 0 {
			return l.Project + ": no owner rulings"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "%s: %d owner rulings", l.Project, len(l.Rulings))
		for _, r := range l.Rulings {
			fmt.Fprintf(&b, "\n\n%s", r.Name)
			if r.Ask != "" {
				fmt.Fprintf(&b, " (ask %s)", r.Ask)
			}
			fmt.Fprintf(&b, "\n  %s", r.Text)
		}
		return b.String()
	}
	ls := &cobra.Command{
		Use:     "ls",
		Aliases: []string{"list"},
		Short:   "List the project's owner rulings",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return call(cmd, orchclient.Rulings, human) },
	}
	var req orchapi.RulingAddRequest
	add := &cobra.Command{
		Use:   "add <name> --text <ruling> --ask <id>",
		Short: "Add an owner ruling an owner approved on a card (the signed ask is the authority; an agent may carry it out)",
		Long: "Adds a [[rulings]] table to the project's project.toml at its authority; the card judge uses it at once.\n" +
			"--ask names the owner's signed, approved ask carrying the ruling's name and text, each quoted; the ruling\n" +
			"records it. An agent (anyone but a person who owns or administers the project) may carry it out when the\n" +
			"ruling is in the approved action's brief or plan. One ask adds a ruling of a name once.\n" +
			"--text - reads the text from stdin.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Name = args[0]
			if req.Text == "-" {
				t, err := joinArgs([]string{"-"})
				if err != nil {
					return err
				}
				req.Text = t
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.RulingList, error) {
				return orchclient.AddRuling(ctx, c, req)
			}, human)
		},
	}
	add.Flags().StringVar(&req.Text, "text", "", "the ruling, exactly as the approved card carries it (- reads stdin)")
	add.Flags().StringVar(&req.Ask, "ask", "", "the approved ask whose card carries the text")
	_ = add.MarkFlagRequired("text")
	_ = add.MarkFlagRequired("ask")
	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove an owner ruling (a person who owns or administers the project, never an agent)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.RulingList, error) {
				return orchclient.RemoveRuling(ctx, c, args[0])
			}, human)
		},
	}
	cmd.AddCommand(ls, add, rm)
	return cmd
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func absAll(ps []string) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		if abs, err := filepath.Abs(expandHome(p)); err == nil {
			out = append(out, abs)
		}
	}
	return out
}

func areaCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "area", Short: "Areas: scopes inside a project"}
	var a api.Area
	add := &cobra.Command{
		Use:   "add <project>/<name> | <name> --project P",
		Short: "Add an area to a project (applied at once, no restart)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, name, err := areaArg(args[0])
			if err != nil {
				return err
			}
			a.Name, a.Paths = name, absAll(a.Paths)
			return call(cmd, func(ctx context.Context, c *client.Client) (api.Project, error) { return c.AddArea(ctx, ref, a) },
				func(p api.Project) string { return "added area " + p.Slug + "/" + name })
		},
	}
	add.Flags().StringSliceVar(&a.Paths, "path", nil, "a directory in this area (repeatable)")
	add.Flags().StringSliceVar(&a.Remotes, "remote", nil, "a git remote in this area (repeatable)")
	add.Flags().StringSliceVar(&a.Work, "work", nil, "a work id glob in this area, e.g. 'bd:solver-*' (repeatable)")
	add.Flags().StringSliceVar(&a.Labels, "label", nil, "a tracker label whose items are in this area, e.g. solver (repeatable)")
	ls := &cobra.Command{
		Use:   "ls [project]",
		Short: "Areas you can see in a project",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := projectArg(args)
			if err != nil {
				return err
			}
			return call(cmd, func(ctx context.Context, c *client.Client) ([]api.Area, error) { return c.Areas(ctx, ref) },
				func(as []api.Area) string {
					var b strings.Builder
					for _, a := range as {
						fmt.Fprintf(&b, "%-16s paths=%v remotes=%v work=%v labels=%v\n", a.Name, a.Paths, a.Remotes, a.Work, a.Labels)
					}
					return b.String()
				})
		},
	}
	show := &cobra.Command{
		Use:   "show <project>/<area>",
		Short: "One area as you can see it: its rules and the trackers that serve it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, name, err := areaArg(args[0])
			if err != nil {
				return err
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (api.AreaView, error) { return c.GetArea(ctx, p, name) }, nil)
		},
	}
	var paths, remotes, work, labels []string
	set := &cobra.Command{
		Use:   "set <project>/<area> [--path P]... [--remote R]... [--work G]... [--label L]...",
		Short: "Change an area's rules; the daemon applies them at once (no restart)",
		Long: `Change an area's rules. Each flag given replaces that whole list; flags not given
keep theirs; an empty value clears it (--label ''). The daemon applies the change at
once: resolution, work item areas and label filters see it on the next call.
paths and remotes are machine settings: only this machine's owner, on its socket.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, name, err := areaArg(args[0])
			if err != nil {
				return err
			}
			var u api.UpdateAreaRequest
			changed := func(flag string, v []string) *[]string {
				if !cmd.Flags().Changed(flag) {
					return nil
				}
				out := []string{}
				for _, x := range v {
					if x = strings.TrimSpace(x); x != "" {
						out = append(out, x)
					}
				}
				return &out
			}
			u.Paths, u.Remotes, u.Work, u.Labels = changed("path", paths), changed("remote", remotes), changed("work", work), changed("label", labels)
			if u.Paths != nil {
				abs := absAll(*u.Paths)
				u.Paths = &abs
			}
			if u.Paths == nil && u.Remotes == nil && u.Work == nil && u.Labels == nil {
				return fmt.Errorf("nothing to change: pass --path, --remote, --work or --label")
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (api.AreaView, error) {
				return c.UpdateArea(ctx, p, name, u)
			},
				func(v api.AreaView) string {
					return fmt.Sprintf("area %s/%s: paths=%v remotes=%v work=%v labels=%v", v.Project.Slug, v.Name, v.Paths, v.Remotes, v.Work, v.Labels)
				})
		},
	}
	set.Flags().StringSliceVar(&paths, "path", nil, "replace the area's directories (repeatable)")
	set.Flags().StringSliceVar(&remotes, "remote", nil, "replace the area's git remotes (repeatable)")
	set.Flags().StringSliceVar(&work, "work", nil, "replace the area's work id globs, e.g. 'bd:solver-*' (repeatable)")
	set.Flags().StringSliceVar(&labels, "label", nil, "replace the area's tracker labels, e.g. solver (repeatable)")
	cmd.AddCommand(add, ls, show, set)
	return cmd
}

// areaArg splits "<project>/<area>"; a bare area takes the project from --project.
func areaArg(s string) (string, string, error) {
	p, a := splitProjectArea(s)
	if a == "" {
		a, p = p, flagProject
		if q, qa := splitProjectArea(p); qa != "" {
			p = q
		}
	}
	if p == "" || a == "" {
		return "", "", fmt.Errorf("name the area as <project>/<area> (or <area> with --project)")
	}
	return p, a, nil
}

// splitProjectArea splits "slug/area".
func splitProjectArea(s string) (string, string) {
	p, a, _ := strings.Cut(s, "/")
	return p, a
}

func grantCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "grant <identity|@github-user|@github-org> <owner|member|guest> <project[/area]>",
		Short: "Give an identity, or a GitHub user or org (zm join <project>, no code), a role in a project or area",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, a := splitProjectArea(args[2])
			if strings.HasPrefix(args[0], "@") {
				req := api.GitHubGrantRequest{Owner: args[0], Role: args[1], Project: p, Area: a}
				return call(cmd, func(ctx context.Context, c *client.Client) (api.GitHubGrant, error) { return c.GrantGitHub(ctx, req) },
					func(g api.GitHubGrant) string {
						who := "GitHub @" + g.Login
						if g.Kind == "org" {
							who = "members of GitHub org @" + g.Login
						}
						return fmt.Sprintf("%s (%s) %s %s of %s%s; they join with: zm join %s", who, g.Account,
							map[bool]string{true: "are", false: "is"}[g.Kind == "org"], g.Role, g.Project, slashArea(g.Area), g.Project)
					})
			}
			req := api.GrantRequest{Identity: args[0], Role: args[1], Project: p, Area: a}
			return call(cmd, func(ctx context.Context, c *client.Client) (api.Grant, error) { return c.Grant(ctx, req) },
				func(g api.Grant) string {
					return fmt.Sprintf("%s is %s of %s%s", g.Identity, g.Role, g.Project, slashArea(g.Area)) + alsoLowered(g)
				})
		},
	}
}

// alsoLowered names the other identities of the same GitHub account an
// owner's removal or lowering took down with it (Potluck #179), so the owner
// sees who else lost access.
func alsoLowered(g api.Grant) string {
	if len(g.AlsoLowered) == 0 {
		return ""
	}
	return "\nalso lowered (same GitHub account): " + strings.Join(g.AlsoLowered, ", ")
}

func slashArea(a string) string {
	if a == "" {
		return ""
	}
	return "/" + a
}

func revokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <identity|@github-user|@github-org> <project[/area]>",
		Short: "Remove an identity's (or a GitHub user's or org's) role in a project or area",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, a := splitProjectArea(args[1])
			if strings.HasPrefix(args[0], "@") {
				req := api.GitHubGrantRequest{Owner: args[0], Project: p, Area: a}
				return call(cmd, func(ctx context.Context, c *client.Client) (api.GitHubRevoke, error) { return c.RevokeGitHub(ctx, req) },
					func(r api.GitHubRevoke) string {
						return fmt.Sprintf("revoked %s on %s%s, and what it gave %d joins (their grants and node-bound tokens)", args[0], p, slashArea(a), r.Admissions)
					})
			}
			req := api.GrantRequest{Identity: args[0], Project: p, Area: a}
			return call(cmd, func(ctx context.Context, c *client.Client) (api.Grant, error) { return c.Revoke(ctx, req) },
				func(g api.Grant) string {
					return fmt.Sprintf("revoked %s's %s on %s%s", g.Identity, g.Role, g.Project, slashArea(g.Area)) + alsoLowered(g)
				})
		},
	}
}

func grantsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "grants [project]",
		Short: "Who has which role (in projects you administer)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref := flagProject
			if len(args) > 0 {
				ref = args[0]
			}
			c, _, err := newClient()
			if err != nil {
				return err
			}
			gs, err := c.Grants(cmd.Context(), ref)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(gs) // unchanged for scripts; GitHub grants: GET /v0/grants/github
			}
			for _, g := range gs {
				fmt.Printf("%-24s %-7s %s\n", g.Project+slashArea(g.Area), g.Role, g.Identity)
			}
			if ref != "" { // GitHub grants: an admin of the project only
				if gh, err := c.GitHubGrants(cmd.Context(), ref); err == nil {
					for _, g := range gh.Grants {
						fmt.Printf("%-24s %-7s @%s (GitHub %s, %s)\n", g.Project+slashArea(g.Area), g.Role, g.Login, g.Kind, g.Account)
					}
				}
			}
			return nil
		},
	}
}

func tokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Bearer tokens for remote callers (the tailnet door)"}
	var req api.MintTokenRequest
	var ttl time.Duration
	var answerOnly bool
	mint := &cobra.Command{
		Use:   "mint (--as <name> --role <role> [--project P [--area A]] | --answer-only --project P --node <device>) [--ttl 720h]",
		Short: "Mint a token (adds the matching grant); the secret is shown once",
		Long: `Mint a token (adds the matching grant); the secret is shown once.

--answer-only mints a token that can only read the project's asks and
answer card asks as you, for an app on one device: --node names that
tailnet device (it works from there only, never from this machine), and
--project and --ttl are required. It is for your own identity and adds no
grant, so --as, --role and --no-grant are not taken.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req.Project = flagProject
			if p, a := splitProjectArea(req.Project); a != "" {
				req.Project, req.Area = p, a
			}
			req.TTLSeconds = int64(ttl.Seconds())
			if err := tokenMintFlags(cmd, &req, answerOnly, ttl); err != nil {
				return err
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (api.MintTokenResponse, error) {
				return c.MintToken(ctx, req)
			},
				func(r api.MintTokenResponse) string {
					if r.Token.Limit != "" {
						return limitedMintText(r)
					}
					return fmt.Sprintf("token %s for %s (%s of %s%s)\n%s\nGive it to them once: ZM_HOST=<this machine's tailnet URL> ZM_TOKEN=<token>",
						r.Token.ID, r.Token.Identity, r.Token.Role, r.Token.Project, slashArea(r.Token.Area), r.Secret)
				})
		},
	}
	mint.Flags().StringVar(&req.As, "as", "", "identity name (created if missing)")
	mint.Flags().StringVar(&req.Role, "role", "", "owner, member or guest (the token never acts above it)")
	mint.Flags().StringVar(&req.Area, "area", "", "limit to one area")
	mint.Flags().DurationVar(&ttl, "ttl", 0, "expire after this long (default: never; required with --answer-only)")
	mint.Flags().StringVar(&req.Note, "note", "", "what it is for")
	mint.Flags().BoolVar(&req.NoGrant, "no-grant", false, "do not add the grant (the identity must already have one)")
	mint.Flags().BoolVar(&answerOnly, "answer-only", false, "a token that only reads the project's asks and answers card asks as you")
	mint.Flags().StringVar(&req.Node, "node", "", "with --answer-only: the tailnet device (its MagicDNS name) the token works from")
	ls := &cobra.Command{
		Use:   "ls",
		Short: "Every token (never the secrets)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) ([]api.Token, error) { return c.Tokens(ctx) },
				func(ts []api.Token) string {
					var b strings.Builder
					for _, t := range ts {
						state := "active"
						if t.Revoked != nil {
							state = "revoked"
						} else if t.Expires != nil && t.Expires.Before(time.Now()) {
							state = "expired"
						}
						role := t.Role
						if t.Limit != "" {
							role = t.Limit + "-only@" + t.Device
						}
						fmt.Fprintf(&b, "%s %-10s %-7s %-20s %s %s\n", t.ID, t.Identity, role, t.Project+slashArea(t.Area), state, t.Note)
					}
					return b.String()
				})
		},
	}
	revoke := &cobra.Command{
		Use:   "revoke <id>",
		Short: "Revoke a token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (api.Token, error) { return c.RevokeToken(ctx, args[0]) },
				func(t api.Token) string { return "revoked " + t.ID })
		},
	}
	cmd.AddCommand(mint, ls, revoke)
	return cmd
}

// tokenMintFlags checks zm token mint's flags: --answer-only takes
// --project (no area), --node and --ttl and refuses --as, --role and
// --no-grant (the token is the caller's own and adds no grant); without it,
// --as and --role are required and --node is refused.
func tokenMintFlags(cmd *cobra.Command, req *api.MintTokenRequest, answerOnly bool, ttl time.Duration) error {
	if !answerOnly {
		switch {
		case req.Node != "":
			return fmt.Errorf("--node goes with --answer-only")
		case req.As == "" || req.Role == "":
			return fmt.Errorf("--as and --role are required (or --answer-only)")
		}
		return nil
	}
	for _, f := range []string{"as", "role", "no-grant"} {
		if cmd.Flags().Changed(f) {
			return fmt.Errorf("--%s does not go with --answer-only: an answer-only token is your own and adds no grant", f)
		}
	}
	switch {
	case req.Project == "":
		return fmt.Errorf("--answer-only needs --project")
	case req.Area != "":
		return fmt.Errorf("an answer-only token is for the whole project, not an area")
	case strings.TrimSpace(req.Node) == "":
		return fmt.Errorf("--answer-only needs --node: the tailnet device it will work from")
	case ttl < time.Second:
		return fmt.Errorf("--answer-only needs --ttl (at least 1s)")
	}
	req.Limit = orchapi.AnswerLimit
	return nil
}

// limitedMintText is what zm token mint prints for a limited token: the
// device the daemon resolved, for the person to check before handing the
// secret over.
func limitedMintText(r api.MintTokenResponse) string {
	dev := r.Token.Device
	if d := r.Device; d != nil {
		dev = d.Name
		if who := strings.TrimSpace(d.Login + " " + d.OS); who != "" {
			dev += " (" + who + ")"
		}
	}
	until := "no expiry"
	if r.Token.Expires != nil {
		until = r.Token.Expires.Local().Format(time.RFC3339)
	}
	return fmt.Sprintf("token %s for %s, %s only, project %s, from device %s only, until %s\n%s\nCheck the device is the one you meant, then enter the token in its app once; revoke it with zm token revoke %s",
		r.Token.ID, r.Token.Identity, r.Token.Limit, r.Token.Project, dev, until, r.Secret, r.Token.ID)
}

func identityCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "identity", Short: "Identities: people, remote agents and machines that can be granted roles"}
	var req api.NewIdentityRequest
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Add an identity (--tailscale maps a tailnet login to it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Name = args[0]
			return call(cmd, func(ctx context.Context, c *client.Client) (api.Identity, error) { return c.AddIdentity(ctx, req) },
				func(id api.Identity) string { return "added " + id.Kind + " " + id.Name })
		},
	}
	add.Flags().StringVar(&req.Kind, "kind", "human", "human, agent or machine")
	add.Flags().StringVar(&req.TailscaleLogin, "tailscale", "", "tailnet login (WhoIs) that is this identity")
	ls := &cobra.Command{
		Use:   "ls",
		Short: "Every identity",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) ([]api.Identity, error) { return c.Identities(ctx) },
				func(ids []api.Identity) string {
					var b strings.Builder
					for _, id := range ids {
						fmt.Fprintf(&b, "%-16s %-8s %s\n", id.Name, id.Kind, id.TailscaleLogin)
					}
					return b.String()
				})
		},
	}
	var revoke bool
	unlink := &cobra.Command{
		Use:   "unlink github <identity>",
		Short: "Remove an identity's GitHub link (a link nobody meant); --revoke also revokes its node-bound tokens",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "github" {
				return fmt.Errorf("only github links: zm identity unlink github <identity>")
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (api.GitHubUnlink, error) {
				return c.UnlinkGitHubIdentity(ctx, args[1], revoke)
			},
				func(u api.GitHubUnlink) string {
					return fmt.Sprintf("unlinked %s from GitHub @%s; %d node-bound tokens revoked", u.Identity, u.Login, len(u.Revoked))
				})
		},
	}
	unlink.Flags().BoolVar(&revoke, "revoke", false, "also revoke the identity's node-bound tokens")
	cmd.AddCommand(add, ls, unlink, aliasCmd(), personLinkCmd())
	return cmd
}

// personLinkCmd is zm identity person-link: one person's several accounts at the
// project's authority (-P or the current directory picks it), as data.
func personLinkCmd() *cobra.Command {
	var person string
	var share, noShare bool
	show := func(l orchapi.PersonLinks) string {
		if len(l.Links) == 0 {
			return "no person links"
		}
		lines := make([]string, 0, len(l.Links))
		for _, x := range l.Links {
			state := "live"
			if !x.Live {
				state = "waiting for the main account, and the account or an admin of every project either is in"
			}
			var who []string
			if x.ByAdmin != "" {
				who = append(who, "recorded by "+x.ByAdmin)
			}
			if x.BySelf {
				who = append(who, x.Identity+" agreed")
			}
			if x.ByPerson {
				who = append(who, x.Person+" agreed")
			}
			line := x.Identity + " is " + x.Person + ": " + state
			if len(who) > 0 {
				line += " (" + strings.Join(who, ", ") + ")"
			}
			if x.SharesMail {
				line += "; mail to " + x.Identity + " reaches " + x.Person
			}
			if x.PersonSharesMail {
				line += "; mail to " + x.Person + " reaches " + x.Identity
			}
			lines = append(lines, line)
		}
		return strings.Join(lines, "\n")
	}
	cmd := &cobra.Command{
		Use:   "person-link <account> --person <main account>",
		Short: "Say an account belongs to a person (one person, several accounts); grants nothing",
		Long: `Say <account> belongs to the person whose main account is --person. A live link folds the accounts
into one person: zm people shows them once, and a card for the person answered from any of their accounts is
their answer. It grants nothing: each account answers asks and owns projects by its own grants only.

It is live when the main account ran it, and so did <account> itself or someone who administers every
project either account is in (never <account> itself). --share-mail (run as the account whose mail it is) also
delivers that account's private mail to the link's other account; nobody else can turn it on, and moving
or removing the link turns it off. Like a cc, people who mail the account then see that the other account
reads it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if person == "" {
				return fmt.Errorf("name the person's main account: --person <name>")
			}
			req := orchapi.PersonLinkRequest{Identity: args[0], Person: person}
			if share || noShare {
				v := share
				req.ShareMail = &v
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.PersonLinks, error) {
				return orchclient.AddPersonLink(ctx, c, req)
			}, show)
		},
	}
	cmd.Flags().StringVar(&person, "person", "", "the person's main account (an identity name)")
	cmd.Flags().BoolVar(&share, "share-mail", false, "also deliver your own account's private mail to this link's other account")
	cmd.Flags().BoolVar(&noShare, "no-share-mail", false, "stop delivering your own account's private mail to this link's other account")
	cmd.MarkFlagsMutuallyExclusive("share-mail", "no-share-mail")
	ls := &cobra.Command{
		Use:   "ls",
		Short: "The person links you are an account of or administer",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.PersonLinks, error) {
				return orchclient.PersonLinks(ctx, c)
			}, show)
		},
	}
	rm := &cobra.Command{
		Use:   "rm <account>",
		Short: "Remove an account's person link (either account, or an admin)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.PersonLinks, error) {
				return orchclient.RemovePersonLink(ctx, c, args[0])
			}, show)
		},
	}
	cmd.AddCommand(ls, rm)
	return cmd
}

// aliasCmd is zm identity alias: the exact label names an identity also
// answers to in inbox labels (owner:<name>, ask:<name>), at the project's
// authority (-P or the current directory picks it).
func aliasCmd() *cobra.Command {
	var who string
	show := func(a orchapi.Aliases) string {
		if len(a.Aliases) == 0 {
			return a.Identity + ": no aliases"
		}
		parts := make([]string, 0, len(a.Aliases))
		for _, x := range a.Aliases {
			if by := a.AddedBy[x]; by != "" {
				x += " (set by " + by + ")"
			}
			parts = append(parts, x)
		}
		return a.Identity + ": " + strings.Join(parts, ", ")
	}
	cmd := &cobra.Command{Use: "alias", Short: "Names besides its own an identity answers to in inbox labels (owner:<name>, ask:<name>)"}
	add := &cobra.Command{
		Use:   "add <name>",
		Short: "Count owner:<name> and ask:<name> as --identity's (the authority's owner, or an admin of all its projects); exact names only",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Aliases, error) {
				return orchclient.AddAlias(ctx, c, orchapi.AliasRequest{Alias: args[0], Identity: who})
			}, show)
		},
	}
	add.Flags().StringVar(&who, "identity", "", "the identity it names (default your own: only the authority's owner sets its own)")
	rm := &cobra.Command{
		Use:   "rm <name>",
		Short: "Remove an alias",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Aliases, error) {
				return orchclient.RemoveAlias(ctx, c, args[0])
			}, show)
		},
	}
	ls := &cobra.Command{
		Use:   "ls",
		Short: "An identity's aliases (default yours)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Aliases, error) {
				return orchclient.Aliases(ctx, c, who)
			}, show)
		},
	}
	ls.Flags().StringVar(&who, "identity", "", "another identity")
	cmd.AddCommand(add, rm, ls)
	return cmd
}
