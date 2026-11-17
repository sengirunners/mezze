package main

// Owned by the zero-install stream (SPEC 17.1): `zm init`.

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/project"
)

func init() { rootCmd.AddCommand(initCmd()) }

// initPlan is what `zm init` would create for a directory.
type initPlan struct {
	Slug    string   // from --slug, a pinned repo file, the repo or the directory name
	From    string   // where the slug came from, for the message
	Name    string   // display name (the directory's own spelling)
	Root    string   // the directory the project's path rule names (repo root, or the cwd outside git)
	Git     bool     // Root is a git repository
	Remotes []string // the repo's origin, normalised; none outside git
}

// planInit works out the project for dir. pinned is the slug a
// .zm/project.toml above dir names when that project does not exist here
// (a teammate's repo): using it makes the file resolve.
func planInit(dir, slugFlag, pinned string) (initPlan, error) {
	var p initPlan
	if root := initGitRoot(dir); root != "" {
		p.Root, p.Git = root, true
		if u := project.NormalizeRemote(project.OriginURL(root)); u != "" {
			p.Remotes = []string{u}
		}
	} else {
		p.Root = dir
	}
	// $HOME or / as the root would put every directory under it in the
	// project (a git repo there, a dotfiles repo, is ignored above).
	if broadRoot(p.Root) {
		return p, &api.Error{Code: api.CodeBadRequest, Message: "refusing to make " + p.Root + " a project: every directory under it would belong to it",
			Hint: "cd into the project's own directory (or a git repo) and run zm init there"}
	}
	p.Name = repoName(p)
	switch {
	case slugFlag != "":
		p.Slug, p.From = strings.ToLower(strings.TrimSpace(slugFlag)), "--slug"
		if !api.ValidSlug(p.Slug) {
			return p, &api.Error{Code: api.CodeBadRequest, Message: fmt.Sprintf("bad slug %q", slugFlag),
				Hint: "lowercase letters, digits, . _ - (start with a letter or digit); not none, all or owners"}
		}
		return p, nil
	case pinned != "":
		p.Slug, p.From = pinned, project.RepoFileDir+"/"+project.FileName
		return p, nil
	}
	p.Slug, p.From = slugify(p.Name), "the directory name"
	if p.Git {
		p.From = "the repo name"
	}
	if !api.ValidSlug(p.Slug) {
		return p, &api.Error{Code: api.CodeBadRequest, Message: fmt.Sprintf("cannot make a project slug from %q", p.Name),
			Hint: "name it: zm init --slug <slug>"}
	}
	return p, nil
}

// initGitRoot is the git root init uses for dir: none for a repo at $HOME
// (a dotfiles repo) or /, so init treats dir as it would outside git rather
// than claim, or pin a file covering, every directory under it.
func initGitRoot(dir string) string {
	if root := project.GitRoot(dir); root != "" && !broadRoot(root) {
		return root
	}
	return ""
}

// broadRoot reports whether dir is $HOME or /.
func broadRoot(dir string) bool {
	if dir == "/" {
		return true
	}
	home, err := os.UserHomeDir()
	return err == nil && home != "" && sameDir(dir, home)
}

// repoName is the project's natural name: the origin's repository name in a
// git repo (a worktree's directory is often named after its branch), else
// the directory's own name.
func repoName(p initPlan) string {
	if len(p.Remotes) > 0 {
		if n := path.Base(p.Remotes[0]); n != "" && n != "." && n != "/" {
			return n
		}
	}
	return filepath.Base(p.Root)
}

// slugify turns a name into a slug: lowercase, runs of anything else become
// one '-', trimmed to start with a letter or digit, at most 63 bytes.
func slugify(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-'
		switch {
		case ok:
			b.WriteRune(r)
			dash = r == '-'
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.TrimLeft(b.String(), "._-")
	if len(out) > 63 {
		out = out[:63]
	}
	return strings.TrimRight(out, "._-")
}

func sameDir(a, b string) bool {
	return project.ExpandPath(a) == project.ExpandPath(b)
}

// cwdRule reports whether rule is one of the directory's own rules (the
// repo file, a path, the repo's remote), as opposed to --project, the
// environment, the calling session or default_project.
func cwdRule(rule string) bool {
	return rule == "cwd-file" || rule == "cwd-path" || rule == "cwd-remote"
}

// pinnedMissing is the slug a .zm/project.toml names when resolution
// stopped there because this machine has no such project.
func pinnedMissing(r api.Resolution, dir string) string {
	if r.Project != nil || len(r.Steps) == 0 {
		return ""
	}
	last := r.Steps[len(r.Steps)-1]
	if last.Rule != "cwd-file" || !strings.HasPrefix(last.Result, "no project ") {
		return ""
	}
	b, err := os.ReadFile(last.Input)
	if err != nil {
		return ""
	}
	f, err := project.ParseRepoFile(string(b))
	if err != nil || f.Area != "" || !api.ValidSlug(f.Project) {
		return "" // an area must be added first; say nothing clever
	}
	return f.Project
}

const initNext = `next:
  zm work new "first thing to do"   track work (local:, no tracker needed)
  zm run claude                     start an agent here (or: zm run shell)
  zm ls                             sessions, the ones that need you first`

func initCmd() *cobra.Command {
	var slug string
	var pin bool
	cmd := &cobra.Command{
		Use:   "init [--slug S] [--pin]",
		Short: "Make the directory you are in a project (git repo: its root and remote; else this directory)",
		Long: `Make the directory you are in a zm project, starting the daemon if needed.

In a git repo the slug comes from the repo name and the project's rules are the
repo root and its origin remote; outside git the slug comes from the directory
name and the rule is this directory. A slug already taken is refused: pass
--slug. Nothing is written into the directory unless --pin, which writes
.zm/project.toml (commit it so teammates' zm finds the project by slug).
In a directory that already belongs to a project, init names it and exits 0.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, _, err := newClient()
			if err != nil {
				return err
			}
			if c.Socket == "" {
				return fmt.Errorf("zm init sets up this machine's daemon; unset ZM_HOST")
			}
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			if real := project.ExpandPath(dir); real != "" {
				dir = real
			}
			return runInit(cmd.Context(), c, dir, slug, pin)
		},
	}
	cmd.Flags().StringVar(&slug, "slug", "", "the project's slug (default: from the repo or directory name)")
	cmd.Flags().BoolVar(&pin, "pin", false, "also write .zm/project.toml naming the project (to commit)")
	return cmd
}

func runInit(ctx context.Context, c *client.Client, dir, slug string, pin bool) error {
	// Only the directory decides: no --project, $ZM_PROJECT or session.
	rc := *c
	rc.Project, rc.ProjectFrom, rc.As, rc.Cwd = "", "", "", dir
	res, err := rc.Resolve(ctx)
	if err != nil {
		return err
	}
	// A different --slug in a directory a broader project's path or remote
	// claims makes a nested project (its path rule is longer, so it wins);
	// a pinned .zm/project.toml always wins, so that one is refused.
	nested := res.Project != nil && slug != "" && slug != res.Project.Slug
	if nested && res.Rule == "cwd-file" {
		return &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("%s is pinned to project %s (%s)", dir, res.Project.Slug, res.Steps[len(res.Steps)-1].Input),
			Hint: "edit or remove that file first"}
	}
	if res.Project != nil && cwdRule(res.Rule) && !nested {
		where := res.Project.Slug
		if res.Area != "" {
			where += "/" + res.Area
		}
		if pin && res.Rule != "cwd-file" {
			if err := pinRepo(dir, project.RepoFile{Project: res.Project.Slug, Area: res.Area}); err != nil {
				return err
			}
		}
		if flagJSON {
			return printJSON(res)
		}
		fmt.Printf("%s is already project %s (rule %s)\n%s\n", dir, where, res.Rule, initNext)
		return nil
	}
	p, err := planInit(dir, slug, pinnedMissing(res, dir))
	if err != nil {
		return err
	}
	if nested && res.Rule == "cwd-path" {
		if err := nestedWins(ctx, c, res.Project.ID, dir, p.Root); err != nil {
			return err
		}
	}
	// A remote another project already lists stays with it: a second clone
	// given its own --slug gets a path rule only, so later clones of the
	// repo still resolve to the original project.
	var kept []string
	if len(p.Remotes) > 0 {
		if p.Remotes, kept, err = freeRemotes(ctx, c, p.Remotes); err != nil {
			return err
		}
	}
	req := api.NewProjectRequest{Slug: p.Slug, Name: p.Name, RepoRules: api.RepoRules{Paths: []string{p.Root}, Remotes: p.Remotes}}
	created, err := c.CreateProject(ctx, req)
	if api.IsCode(err, api.CodeConflict) {
		return &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("project %q already exists (slug from %s)", p.Slug, p.From),
			Hint: "pick another: zm init --slug <slug>"}
	}
	if err != nil {
		return err
	}
	if pin {
		if err := pinRepo(dir, project.RepoFile{Project: created.Slug}); err != nil {
			return err
		}
	}
	if flagJSON {
		return printJSON(created)
	}
	rules := "path " + p.Root
	if len(p.Remotes) > 0 {
		rules += ", remote " + strings.Join(p.Remotes, ", ")
	}
	if len(kept) > 0 {
		rules += "; " + strings.Join(kept, ", ")
	}
	fmt.Printf("created project %s (slug from %s; %s)\n", created.Slug, p.From, rules)
	if after, err := rc.Resolve(ctx); err == nil && (after.Project == nil || after.Project.ID != created.ID) {
		fmt.Println("note: this directory still resolves elsewhere; see zm project why")
	}
	fmt.Println(initNext)
	return nil
}

// pinRepo writes .zm/project.toml at the repo root (or in dir outside git).
func pinRepo(dir string, f project.RepoFile) error {
	root := initGitRoot(dir)
	if root == "" {
		root = dir
	}
	if broadRoot(root) {
		return &api.Error{Code: api.CodeBadRequest, Message: "refusing to pin " + root + ": the file would cover every directory under it",
			Hint: "run zm init --pin in the project's own directory"}
	}
	path, err := project.WriteRepoFile(root, f)
	if err != nil {
		return err
	}
	if !flagJSON {
		fmt.Println("wrote", path)
	}
	return nil
}

// nestedWins refuses a nested project whose path rule would not beat the
// project the directory already resolves to (the same repo root, say), so
// init never leaves a project nothing resolves to.
func nestedWins(ctx context.Context, c *client.Client, id, dir, root string) error {
	cur, err := c.GetProject(ctx, id)
	if err != nil {
		return err
	}
	paths := append([]string(nil), cur.Paths...)
	for _, a := range cur.Areas {
		paths = append(paths, a.Paths...)
	}
	best := 0
	for _, r := range paths {
		if n, ok := project.PathMatch(dir, r); ok && n > best {
			best = n
		}
	}
	if n, ok := project.PathMatch(dir, root); ok && n > best {
		return nil
	}
	return &api.Error{Code: api.CodeConflict, Message: fmt.Sprintf("%s already belongs to project %s, whose path rule covers %s too", dir, cur.Slug, root),
		Hint: "zm project why shows the rule; zm init --slug makes a nested project only for a directory deeper than it"}
}

// freeRemotes splits remotes into those no existing project (or area) lists
// and, for the others, a "remote R stays with project P" note each.
func freeRemotes(ctx context.Context, c *client.Client, remotes []string) (free, kept []string, err error) {
	ps, err := c.Projects(ctx)
	if err != nil {
		return nil, nil, err
	}
	owner := map[string]string{}
	for _, p := range ps {
		rs := append([]string(nil), p.Remotes...)
		for _, a := range p.Areas {
			rs = append(rs, a.Remotes...)
		}
		for _, r := range rs {
			if k := strings.ToLower(project.NormalizeRemote(r)); owner[k] == "" {
				owner[k] = p.Slug
			}
		}
	}
	for _, r := range remotes {
		if o := owner[strings.ToLower(r)]; o != "" {
			kept = append(kept, fmt.Sprintf("remote %s stays with project %s", r, o))
			continue
		}
		free = append(free, r)
	}
	return free, kept, nil
}
