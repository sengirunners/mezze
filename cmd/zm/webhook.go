package main

// `zm webhook add|ls|show|rm|test|enable|disable` and `zm github
// link|ls|repos|unlink|sync` (SPEC "Webhooks and GitHub linking").

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/client"
)

func init() {
	wh := &cobra.Command{Use: "webhook", Aliases: []string{"webhooks"}, Short: "Send the project's events to HTTP endpoints",
		Long: `A webhook POSTs every event it may see (CloudEvents JSON) to a URL, signed
per Standard Webhooks (webhook-id, webhook-timestamp, webhook-signature: verify
with any standardwebhooks.com library) and with GitHub's X-Hub-Signature-256.
Retries carry the same webhook-id: dedupe on it. Zm-Webhook-Signature is
deprecated and goes away on 2027-05-01. A hook sees only what its creator
may read, rechecked on every grant or token change. Deliveries are queued on
disk (they survive a restart) and retried with backoff; a hook failing for a
day is disabled (zm webhook enable turns it back on).`}
	wh.AddCommand(webhookAddCmd(), webhookLsCmd(), webhookShowCmd(), webhookRmCmd(), webhookTestCmd(),
		webhookToggleCmd("enable", true), webhookToggleCmd("disable", false))
	gh := &cobra.Command{Use: "github", Short: "Move work items when GitHub pull requests name them",
		Long: `A branch name, PR title or body naming a work item (acme-abc12, [bd:acme-abc12],
local:k3x9qa) links the PR to it and moves it: opened -> in progress (unless
someone holds it), merged -> done (the comment names the PR), closed unmerged
-> open again. PRs arrive from GitHub's webhook (point it at
https://<daemon>/v0/github/hook with the printed secret, content type
application/json) or by polling with gh api (--poll; no inbound port).`}
	gh.AddCommand(githubLinkCmd(), githubLsCmd(), githubReposCmd(), githubUnlinkCmd(), githubSyncCmd())
	rootCmd.AddCommand(wh, gh)
}

func explicitProject() string {
	if flagProject != "" {
		return flagProject
	}
	return os.Getenv("ZM_PROJECT")
}

func webhookAddCmd() *cobra.Command {
	var events []string
	var secretFile, note string
	var allowPrivate, all bool
	cmd := &cobra.Command{
		Use:   "add <url>",
		Short: "Add a webhook for a project (-P), or every project (--all, machine owner only)",
		Example: `  zm webhook add https://hooks.example.com/zm -P acme --events work.*,mail.*
  zm webhook add https://ci.example.com/hook -P acme/api --secret-file ~/.zm-hook-secret
  zm webhook add http://127.0.0.1:9000/ --all --allow-private`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := orchapi.NewWebhookRequest{URL: args[0], Project: explicitProject(), Events: events, AllowPrivate: allowPrivate, Note: note}
			if req.Project == "" && !all {
				return fmt.Errorf("name the project with -P <slug>[/<area>] (or --all for every project, machine owner only)")
			}
			if all {
				req.Project = ""
			}
			if secretFile != "" {
				v, err := readSecretFile(cmd.Context(), secretFile)
				if err != nil {
					return err
				}
				req.Secret = v.Reveal()
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.NewWebhookResponse, error) {
				return orchclient.AddWebhook(ctx, c, req)
			}, func(r orchapi.NewWebhookResponse) string {
				s := "added webhook " + r.Webhook.ID + " -> " + r.Webhook.URL
				if r.Secret != "" {
					s += "\nsigning secret (shown once): " + r.Secret
				}
				return s
			})
		},
	}
	cmd.Flags().StringSliceVar(&events, "events", nil, "event types to send, e.g. work.*,mail.sent (default: all)")
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "read the signing secret from this file (default: a new one, printed once)")
	cmd.Flags().BoolVar(&allowPrivate, "allow-private", false, "allow loopback, private, link-local and tailnet addresses (machine owner only)")
	cmd.Flags().BoolVar(&all, "all", false, "every project (and machine events); machine owner only")
	cmd.Flags().StringVar(&note, "note", "", "a note shown in zm webhook ls")
	return cmd
}

func webhookLine(w orchapi.Webhook) string {
	scope := w.Project
	if scope == "" {
		scope = "(all)"
	} else if w.Area != "" {
		scope += "/" + w.Area
	}
	ev := "all events"
	if len(w.Events) > 0 {
		ev = strings.Join(w.Events, ",")
	}
	s := fmt.Sprintf("%s  %-8s %-16s %s  [%s] pending %d", w.ID, w.State, scope, w.URL, ev, w.Pending)
	if w.State == orchapi.WebhookDisabled && w.Disabled != "" {
		s += "\n    disabled: " + w.Disabled
	} else if w.LastError != "" {
		s += "\n    last error: " + w.LastError
	}
	return s
}

func webhookLsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ls", Short: "List the webhooks you administer", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) ([]orchapi.Webhook, error) {
				return orchclient.Webhooks(ctx, c, flagProject)
			}, func(ws []orchapi.Webhook) string {
				if len(ws) == 0 {
					return "no webhooks"
				}
				var b strings.Builder
				for _, w := range ws {
					b.WriteString(webhookLine(w) + "\n")
				}
				return b.String()
			})
		},
	}
}

func webhookShowCmd() *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use: "show <id>", Aliases: []string{"log"}, Short: "Show a webhook and its latest delivery attempts", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.WebhookDetail, error) {
				return orchclient.Webhook(ctx, c, args[0], limit)
			}, func(d orchapi.WebhookDetail) string {
				var b strings.Builder
				b.WriteString(webhookLine(d.Webhook) + "\n")
				for _, x := range d.Deliveries {
					fmt.Fprintf(&b, "  %s  %-9s #%d %-28s %3d %5dms %s", x.Time.Local().Format(time.DateTime), x.Outcome, x.Attempt, x.Type, x.Status, x.DurationMS, x.Delivery)
					if x.Error != "" {
						b.WriteString("  " + x.Error)
					}
					b.WriteString("\n")
				}
				return b.String()
			})
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "attempts to show")
	return cmd
}

func webhookRmCmd() *cobra.Command {
	return &cobra.Command{
		Use: "rm <id>", Short: "Remove a webhook and its queue", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Webhook, error) {
				return orchclient.RemoveWebhook(ctx, c, args[0])
			}, func(w orchapi.Webhook) string { return "removed webhook " + w.ID })
		},
	}
}

func webhookTestCmd() *cobra.Command {
	return &cobra.Command{
		Use: "test <id>", Short: "Send one signed ping now and show the answer", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			r, err := orchclient.TestWebhook(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(r)
			}
			if r.OK {
				fmt.Printf("ok: HTTP %d in %d ms\n", r.Delivery.Status, r.Delivery.DurationMS)
				return nil
			}
			fmt.Printf("failed: %s\n", r.Delivery.Error)
			return exitSilently(1)
		},
	}
}

func webhookToggleCmd(verb string, on bool) *cobra.Command {
	return &cobra.Command{
		Use: verb + " <id>", Short: strings.ToUpper(verb[:1]) + verb[1:] + " a webhook", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.Webhook, error) {
				return orchclient.UpdateWebhook(ctx, c, args[0], orchapi.UpdateWebhookRequest{Enabled: &on})
			}, webhookLine)
		},
	}
}

// ---- github ----

func githubLinkCmd() *cobra.Command {
	var req orchapi.GitHubLinkRequest
	var secretFile string
	var poll time.Duration
	cmd := &cobra.Command{
		Use:   "link",
		Short: "Link a project to GitHub repos (create or replace)",
		Example: `  zm github link -P acme --repo acme/app                 # receiver: point a GitHub webhook at /v0/github/hook
  zm github link -P acme --repo acme/app --poll 1m        # behind NAT: poll with gh api
  zm github link -P acme --repo acme/app --prefix acme --source bd --on-open in_review`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			req.Project = explicitProject()
			if req.Project == "" {
				res, err := c.Resolve(cmd.Context())
				if err != nil {
					return err
				}
				if res.Project == nil {
					return fmt.Errorf("name the project with -P <slug>")
				}
				req.Project = res.Project.Slug
			}
			req.Project, _, _ = strings.Cut(req.Project, "/")
			req.PollSeconds = int(poll.Seconds())
			if req.Gate != "" && req.Gate != "none" { // "none" removes the gate
				g, err := filepath.Abs(expandHome(req.Gate))
				if err != nil {
					return err
				}
				req.Gate = g
			}
			if secretFile != "" {
				v, err := readSecretFile(cmd.Context(), secretFile)
				if err != nil {
					return err
				}
				req.Secret = v.Reveal()
			}
			if req.TokenFile != "" {
				abs, err := filepath.Abs(expandTilde(req.TokenFile))
				if err != nil {
					return err
				}
				req.TokenFile = abs
			}
			r, err := orchclient.LinkGitHub(cmd.Context(), c, req)
			if err != nil {
				return err
			}
			if flagJSON {
				return printJSON(r)
			}
			fmt.Println(githubLine(r.Link))
			if r.Secret != "" {
				fmt.Println("receiver secret (shown once; set it as the GitHub webhook's secret): " + r.Secret)
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&req.Repos, "repo", nil, "owner/name (repeatable)")
	cmd.Flags().StringSliceVar(&req.Prefixes, "prefix", nil, "bare id prefix recognised in branches and PRs (default: the project slug)")
	cmd.Flags().StringSliceVar(&req.Sources, "source", nil, "tracker source bare ids are looked up in (default: every tracker of the project)")
	cmd.Flags().StringVar(&req.OnOpen, "on-open", "", "status when a PR opens: claimed (default), in_review, none")
	cmd.Flags().StringVar(&req.OnMerge, "on-merge", "", "status when it merges: done (default), in_review, none")
	cmd.Flags().StringVar(&req.OnClose, "on-close", "", "status when it closes unmerged: open (default), none")
	cmd.Flags().DurationVar(&poll, "poll", 0, "poll with gh api this often (0: webhook receiver only)")
	cmd.Flags().StringVar(&req.Bin, "bin", "", "the gh binary (default: gh on PATH)")
	cmd.Flags().StringVar(&req.Host, "host", "", "GitHub Enterprise host (GH_HOST)")
	cmd.Flags().StringVar(&req.TokenFile, "token-file", "", "a file holding GH_TOKEN for polling (default: gh's own login)")
	cmd.Flags().StringVar(&secretFile, "secret-file", "", "the receiver's secret (default: keep the current one, or make one)")
	cmd.Flags().StringSliceVar(&req.Match, "match", nil, "PR texts that name items: title,branch,body, or all (default: keep the link's, else all three)")
	cmd.Flags().StringVar(&req.Gate, "gate", "", "an executable asked before every item write (a fixed checkout); none removes it; default: keep the link's")
	return cmd
}

func githubLine(l orchapi.GitHubLink) string {
	mode := "receiver"
	if l.PollSeconds > 0 {
		mode = fmt.Sprintf("receiver + poll every %ds", l.PollSeconds)
	}
	sources := strings.Join(l.Sources, ",")
	if sources == "" {
		sources = "(no trackers: only local:<id> written out)"
	}
	s := fmt.Sprintf("%s  %s  ids %s-* in %s  open->%s merge->%s close->%s  (%s, pending %d)", l.Project, strings.Join(l.Repos, ","),
		strings.Join(l.Prefixes, ","), sources, l.OnOpen, l.OnMerge, l.OnClose, mode, l.Pending)
	s += "\n    match: " + strings.Join(l.Match, ",")
	if l.Gate != "" {
		s += fmt.Sprintf("\n    gate: %s (%s), %d waiting", l.Gate, l.GateName, l.Waiting)
		if l.GateError != "" {
			s += "\n    gate error: " + l.GateError
		}
	}
	if l.LastError != "" {
		s += "\n    last error: " + l.LastError
	}
	return s
}

func githubLsCmd() *cobra.Command {
	return &cobra.Command{
		Use: "ls", Short: "List this machine's GitHub links; with -P, the project's repo list (from any member machine)", Args: cobra.NoArgs,
		Example: `  zm github ls                 # this machine's links
  zm github ls -P acme --json  # acme's repo list, as automations read it (API.md GitHubRepoList)`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if flagProject != "" { // -P only: $ZM_PROJECT keeps the links list
				return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.GitHubRepoList, error) {
					p, _, _ := strings.Cut(flagProject, "/") // the whole project's list
					return orchclient.GitHubRepoList(ctx, c, p)
				}, repoListText)
			}
			return call(cmd, func(ctx context.Context, c *client.Client) ([]orchapi.GitHubLink, error) {
				return orchclient.GitHubLinks(ctx, c)
			}, func(ls []orchapi.GitHubLink) string {
				if len(ls) == 0 {
					return "no GitHub links"
				}
				var b strings.Builder
				for _, l := range ls {
					b.WriteString(githubLine(l) + "\n")
				}
				return b.String()
			})
		},
	}
}

func repoListText(l orchapi.GitHubRepoList) string {
	src := "set"
	if l.Source == orchapi.RepoListRemotes {
		src = "never set: the project's GitHub remotes"
	}
	if len(l.Repos) == 0 {
		return fmt.Sprintf("%s: no repos (%s)", l.Project, src)
	}
	return fmt.Sprintf("%s (%s):\n  %s", l.Project, src, strings.Join(l.Repos, "\n  "))
}

// githubReposCmd is zm github repos: show or change a project's repo list.
func githubReposCmd() *cobra.Command {
	var set, add, rm []string
	var reset bool
	cmd := &cobra.Command{
		Use:   "repos",
		Short: "Show or change a project's repo list (any member reads it; an admin sets it from any member machine)",
		Long: `A project's repo list is the GitHub repositories automations scoped to the
project act on (they read zm github ls -P <project> --json). It is project
config kept at the project's authority, so every member machine reads the same
list and an admin of the project may change it from any of them. Never set, it
is the project's and its areas' declared GitHub remotes. It moves no work item
and polls nothing: zm github link does that, on its own machine.`,
		Example: `  zm github repos -P acme
  zm github repos -P acme --set acme/app,acme/api
  zm github repos -P acme --add acme/web --rm acme/api
  zm github repos -P acme --reset            # back to the project's GitHub remotes`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			changing := cmd.Flags().Changed("set") || len(add) > 0 || len(rm) > 0
			if reset && changing {
				return fmt.Errorf("--reset goes back to the project's remotes: it takes no --set, --add or --rm")
			}
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.GitHubRepoList, error) {
				p := explicitProject()
				if p == "" {
					res, err := c.Resolve(ctx)
					if err != nil {
						return orchapi.GitHubRepoList{}, err
					}
					if res.Project == nil {
						return orchapi.GitHubRepoList{}, fmt.Errorf("name the project with -P <slug>")
					}
					p = res.Project.Slug
				}
				p, _, _ = strings.Cut(p, "/") // the whole project's list
				switch {
				case reset:
					return orchclient.SetGitHubRepoList(ctx, c, p, nil)
				case changing:
					return setRepoList(ctx, c, p, cmd.Flags().Changed("set"), set, add, rm)
				}
				return orchclient.GitHubRepoList(ctx, c, p)
			}, repoListText)
		},
	}
	cmd.Flags().StringSliceVar(&set, "set", nil, "replace the list with these owner/name repos ('' empties it)")
	cmd.Flags().StringSliceVar(&add, "add", nil, "add these owner/name repos (repeatable)")
	cmd.Flags().StringSliceVar(&rm, "rm", nil, "remove these owner/name repos (repeatable)")
	cmd.Flags().BoolVar(&reset, "reset", false, "clear the list: back to the project's GitHub remotes")
	return cmd
}

// setRepoList applies --set, then --add and --rm (to the current list when
// --set is not given), and writes the result. Two admins changing one list
// at the same moment: the later write wins.
func setRepoList(ctx context.Context, c *client.Client, p string, replace bool, set, add, rm []string) (orchapi.GitHubRepoList, error) {
	repos := []string{}
	if replace {
		repos = append(repos, set...)
	} else {
		cur, err := orchclient.GitHubRepoList(ctx, c, p)
		if err != nil {
			return cur, err
		}
		repos = append(repos, cur.Repos...)
	}
	repos = append(repos, add...)
	gone := map[string]bool{}
	for _, r := range rm {
		gone[repoKey(r)] = true
	}
	kept := []string{}
	for _, r := range repos {
		if r = strings.TrimSpace(r); r != "" && !gone[repoKey(r)] {
			kept = append(kept, r)
		}
	}
	return orchclient.SetGitHubRepoList(ctx, c, p, &kept)
}

// repoKey is what --rm compares: owner/name, lower case (the daemon
// validates and normalizes what it stores).
func repoKey(r string) string {
	r = strings.TrimSpace(strings.ToLower(r))
	r = strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(r, "https://"), "git@github.com:"), "github.com/")
	return strings.Trim(strings.TrimSuffix(r, ".git"), "/")
}

func githubUnlinkCmd() *cobra.Command {
	return &cobra.Command{
		Use: "unlink <project>", Short: "Remove a project's GitHub link (revokes the linker's token)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.GitHubLink, error) {
				return orchclient.UnlinkGitHub(ctx, c, args[0])
			}, func(l orchapi.GitHubLink) string { return "unlinked " + l.Project })
		},
	}
}

func githubSyncCmd() *cobra.Command {
	return &cobra.Command{
		Use: "sync <project>", Short: "Poll GitHub now and apply what is waiting", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return call(cmd, func(ctx context.Context, c *client.Client) (orchapi.GitHubSyncResponse, error) {
				return orchclient.SyncGitHub(ctx, c, args[0])
			}, func(r orchapi.GitHubSyncResponse) string {
				s := fmt.Sprintf("%d PR snapshots, %d writes", r.PRs, r.Applied)
				for _, e := range r.Errors {
					s += "\n  " + e
				}
				return s
			})
		},
	}
}
