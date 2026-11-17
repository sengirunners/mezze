package main

// zm ci add <owner/repo>...: connect repositories to the Mac this machine
// lends, in one re-runnable command. It holds no rule of its own: it drives the broker's routes
// (GET/POST /v0/runners/setup, GET /v0/runners/links, GET/POST
// /v0/runners/repos, GET /v0/runners/doctor) and Potluck's own ci pr
// (sengirunners/potluck cli), and turns what they answer into one next
// step. Human steps (a GitHub press, a public repository's yes, opening a
// pull request with the person's own gh sign-in) are never taken for the
// person: they are printed, or in --json returned as asks.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sengirunners/mezze/internal/orchclient"
	potluckcli "github.com/sengirunners/potluck/cli"
	"github.com/sengirunners/potluck/jobrule"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/runners"
)

// ciStep is one thing left to do. Kind "ask" is a person's own press or
// yes (an agent asks for it and never does it); "run" is a command (or a
// plain fix) the person or their agent may do; "info" is a block with
// nothing to do yet. Lending this Mac, switching the broker on and the
// doctor's zm fixes are the owner's own steps: asks, with the command the
// owner runs.
type ciStep struct {
	Kind    string   `json:"kind"`
	Say     string   `json:"say"`
	URL     string   `json:"url,omitempty"`
	Command string   `json:"command,omitempty"`
	Choices []string `json:"choices,omitempty"`
}

// line is the step as the command's last line.
func (s ciStep) line() string {
	switch {
	case s.URL != "":
		return "next: " + s.Say + " Open: " + s.URL
	case s.Command != "":
		return "next: " + s.Say + " Run: " + s.Command
	}
	return "next: " + s.Say
}

type ciAddRepo struct {
	Repo    string   `json:"repo"`
	State   string   `json:"state"` // done, needs_human, blocked
	Changed []string `json:"changed"`
	Notes   []string `json:"notes"`
	Next    *ciStep  `json:"next,omitempty"`
}

// ciAddResult is zm ci add --json: State is done when every repository is.
type ciAddResult struct {
	State string      `json:"state"`
	Repos []ciAddRepo `json:"repos"`
	Next  *ciStep     `json:"next,omitempty"`
	// Said is what a person would have read (what will change, the
	// workflow diff), so an agent sees it too.
	Said []string `json:"said"`
}

type ciAddOpts struct {
	public, mine bool
	job, org     string
	workflow     string
	wait         time.Duration
	waitSet      bool
	orgSet       bool                 // --org given, even empty (= own account)
	json         bool                 // --json (or MCP compute_setup): nothing waits unless wait is set
	say          func(string, ...any) // progress for people (nothing in --json)
}

func ciAddCmd() *cobra.Command {
	var o ciAddOpts
	c := &cobra.Command{
		Use:     "add <owner/repo>... [--job <id>] [--public]",
		Aliases: []string{"setup"},
		Short:   "Connect a repository so its own branches' jobs run on the Mac this machine lends",
		Long: `Connects each repository to this Mac in one command. Run it again any time:
it does only what is still missing and says what it will change first. The
last line is done, or the one next step.

It sets up the GitHub app the first time (one page to open: create it,
install it on the repository's account with the repository already
selected, then Yes) and waits until GitHub confirms, connects the
repository, checks it with the CI doctor, and moves a job's runs-on onto this
Mac as a pull request opened with your own gh sign-in: the job --job names,
else the one job that runs on a GitHub-hosted macOS label (with none or
several it says so and stops there).

A public repository needs --public: your yes to run its own branches' jobs
here (a fork's pull request never runs on this Mac). With --mine it connects
the repositories this machine's zm projects map. With --json nothing waits
(unless --wait is given) and every step a person must take comes back as an ask; the exit code is 0
and state says done or needs_human. Without --json it exits 1 until done.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			o.waitSet, o.orgSet, o.json = cmd.Flags().Changed("wait"), cmd.Flags().Changed("org"), flagJSON
			var said []string
			o.say = func(f string, a ...any) {
				if flagJSON {
					said = append(said, fmt.Sprintf(f, a...))
				} else {
					fmt.Printf(f+"\n", a...)
				}
			}
			res, err := ciAdd(cmd.Context(), args, o)
			res.Said = append([]string{}, said...)
			if err != nil {
				if e := (*api.Error)(nil); flagJSON && !errors.As(err, &e) {
					// --json's refusals are JSON too (zm's report prints an api error so).
					return api.Errorf(api.CodeBadRequest, "%s", err.Error())
				}
				return err
			}
			if flagJSON {
				return printJSON(res)
			}
			if res.State == "done" {
				fmt.Println("done")
				return nil
			}
			fmt.Println(res.Next.line())
			return exitSilently(1)
		},
	}
	c.Flags().BoolVar(&o.public, "public", false, "yes: run this public repository's own branches' jobs here (never a fork's pull request)")
	c.Flags().BoolVar(&o.mine, "mine", false, "connect the repositories of this machine's zm projects")
	c.Flags().StringVar(&o.job, "job", "", "the job (its key under jobs:) to move onto this Mac, as a pull request (default: the one on a GitHub-hosted macOS label)")
	c.Flags().StringVar(&o.workflow, "workflow", "", "the workflow file that has the job, when several do")
	c.Flags().StringVar(&o.org, "org", "", "create the GitHub app under this org; --org= for your own account (default: asked of GitHub with gh)")
	c.Flags().DurationVar(&o.wait, "wait", 10*time.Minute, "how long to wait for GitHub to confirm a page you opened (default 0 with --json)")
	return c
}

func ciAdd(ctx context.Context, args []string, o ciAddOpts) (ciAddResult, error) {
	cl, _, err := newClientContext(ctx)
	if err != nil {
		return ciAddResult{}, err
	}
	return ciAddWith(ctx, cl, args, o)
}

// ciAddWith is ciAdd on cl (MCP compute_setup passes its own, which never
// starts a daemon).
func ciAddWith(ctx context.Context, cl *client.Client, args []string, o ciAddOpts) (ciAddResult, error) {
	if cl.Socket == "" {
		return ciAddResult{}, errors.New("zm ci add runs on the Mac that lends, against its own daemon (unset ZM_HOST)")
	}
	var repos []string
	from := map[string]string{} // repo -> the zm project that maps it (--mine)
	for _, a := range args {
		if !containsFold(repos, a) {
			repos = append(repos, a)
		}
	}
	if o.public && (o.mine || len(args) > 1) {
		return ciAddResult{}, errors.New("--public is your yes for one public repository: run zm ci add <owner/repo> --public for each")
	}
	if o.mine {
		mapped, err := orchclient.GitHubRepositories(ctx, cl, "")
		if err != nil {
			return ciAddResult{}, err
		}
		for _, r := range mapped.Repositories {
			if !containsFold(repos, r.Repo) {
				repos = append(repos, r.Repo)
				from[r.Repo] = r.Project
			}
		}
		if len(repos) == 0 {
			return ciAddResult{}, errors.New("no zm project here maps a GitHub repository; name one: zm ci add <owner/repo>")
		}
	}
	if len(repos) == 0 {
		return ciAddResult{}, errors.New("name a repository: zm ci add <owner/repo> (or --mine for your zm projects' repositories)")
	}
	for _, r := range repos {
		if !jobrule.ValidRepo(r) {
			if p := from[r]; p != "" {
				return ciAddResult{}, fmt.Errorf("%q, which zm project %s maps, is not owner/repo: fix that project's repository, or name repositories: zm ci add <owner/repo>", r, p)
			}
			return ciAddResult{}, fmt.Errorf("%q is not owner/repo; for example: zm ci add octocat/hello", r)
		}
	}
	if (o.job != "" || o.workflow != "") && len(repos) > 1 {
		return ciAddResult{}, errors.New("--job and --workflow name one repository's job; run zm ci add <owner/repo> --job <id> for each")
	}
	if !o.waitSet && o.json {
		o.wait = 0
	}
	res := ciAddResult{State: "done", Repos: []ciAddRepo{}}
	for _, r := range repos {
		res.Repos = append(res.Repos, ciAddRepo{Repo: r, State: "needs_human", Changed: []string{}, Notes: []string{}})
	}
	// mark gives every unfinished repository of owner ("" all) this step.
	mark := func(next *ciStep, state, owner string) {
		for i := range res.Repos {
			ro, _, _ := strings.Cut(res.Repos[i].Repo, "/")
			if res.Repos[i].Next == nil && res.Repos[i].State != "done" && (owner == "" || strings.EqualFold(ro, owner)) {
				res.Repos[i].Next, res.Repos[i].State = next, state
			}
		}
	}
	end := func() ciAddResult {
		res.State, res.Next = "done", nil
		for _, rr := range res.Repos {
			if rr.State == "done" {
				continue
			}
			if res.Next == nil || rr.State == "needs_human" && res.State != "needs_human" {
				res.State, res.Next = rr.State, rr.Next
			}
		}
		return res
	}

	// This Mac must lend first: the broker serves the pool it lends to.
	var st runners.Setup
	if err := cl.Do(ctx, "GET", "/v0/runners/setup", nil, &st); err != nil {
		return res, err
	}
	if st.Pool == "" {
		step := &ciStep{Kind: "ask", Say: "This Mac does not lend yet: lend it first, then run this again.", Command: "zm lend"}
		if cfg, err := config.LoadClient(); err == nil {
			if vs, err := lentHere(ctx, cl, strings.ToLower(cfg.Machine)); err == nil && len(vs) > 0 {
				// zm lend made the personal pool; GitHub must reach this Mac before jobs come.
				step.Say = "This Mac lends, but GitHub's jobs are not switched on for it yet: switch them on, then run this again."
				step.Command = "zm runners setup " + lendPool
				if ciOneAccount(repos) && len(repos) <= ciPreselectMax {
					// The install page then has these repositories selected.
					for _, r := range repos {
						step.Command += " --repo " + r
					}
				}
				// The app is made where the repositories live: an org's
				// repositories need --org, or setup refuses them.
				owner, _, _ := strings.Cut(repos[0], "/")
				switch {
				case o.orgSet && o.org != "":
					step.Command += " --org " + o.org
				case !o.orgSet && ciOneAccount(repos):
					if org, err := ciOwnerIsOrg(ctx, owner); errors.Is(err, errNoGitHubAccount) {
						step.Say += " (GitHub has no account " + owner + ": check the repository's name.)"
					} else if err != nil {
						step.Say += " If " + owner + " is a GitHub org, add --org " + owner + "."
					} else if org {
						step.Command += " --org " + owner
					}
				}
			}
		}
		mark(step, "blocked", "")
		return end(), nil
	}

	// The GitHub app, installed and confirmed on each repository's owner.
	owners := []string{}
	for _, r := range repos {
		if o, _, _ := strings.Cut(r, "/"); !containsFold(owners, o) {
			owners = append(owners, o)
		}
	}
	if st.App == nil && len(owners) > 1 {
		// The app is made on one account and installs only there.
		return res, fmt.Errorf("the GitHub app is made on one account first: run zm ci add with %s's repositories alone, then the others", owners[0])
	}
	var accounts []string
	var err error
	if st.App != nil && o.orgSet {
		// --org says where the app is made, and it is made already.
		o.say("note: --org is not used: this Mac's GitHub app is made already.")
		for i := range res.Repos {
			res.Repos[i].Notes = append(res.Repos[i].Notes, "--org is not used: this Mac's GitHub app is made already.")
		}
	}
	if st.App != nil {
		if accounts, err = ciLinkedAccounts(ctx, cl); err != nil {
			return res, err
		}
	}
	ready := map[string]bool{}
	for _, owner := range owners {
		switch {
		case containsFold(accounts, owner):
			ready[strings.ToLower(owner)] = true
		case len(accounts) > 0:
			// The app is private: GitHub installs it only on its own account.
			mark(&ciStep{Kind: "info", Say: "This Mac's GitHub app belongs to " + strings.Join(accounts, ", ") + " and GitHub installs it only there, so " +
				owner + "'s repositories cannot run here yet. Nothing to run for it today."}, "blocked", owner)
		case res.Next == nil && !ciAnyStep(res.Repos):
			step, err := ciAddLink(ctx, cl, st, owner, repos, o)
			if err != nil {
				return res, err
			}
			if step != nil {
				mark(step, "needs_human", owner)
				continue
			}
			ready[strings.ToLower(owner)] = true
			if accounts, err = ciLinkedAccounts(ctx, cl); err != nil {
				return res, err
			}
		}
	}

	// Each repository of a confirmed owner: connected, then checked.
	connected, err := runnersRepos(ctx, cl)
	if err != nil {
		return res, err
	}
	for i := range res.Repos {
		rr := &res.Repos[i]
		if ro, _, _ := strings.Cut(rr.Repo, "/"); !ready[strings.ToLower(ro)] {
			continue
		}
		step, err := ciAddRepoStep(ctx, cl, rr, connected, o)
		if err != nil {
			if len(res.Repos) == 1 || ctx.Err() != nil {
				return res, err
			}
			// One repository's failure is its own step: the others go on.
			// An owner's command or the person's gh sign-in in it is an ask.
			msg := strings.TrimSuffix(err.Error(), ".")
			step := &ciStep{Kind: "run", Say: "Could not finish " + rr.Repo + ": " + msg + ". Fix that, then run this again for it.", Command: ciRerun([]string{rr.Repo}, o)}
			if ciOwnerStep(msg) || ciGHSignIn(msg) {
				step.Kind = "ask"
			}
			rr.State, rr.Next = "blocked", step
			continue
		}
		if step != nil {
			rr.Next = step
			continue
		}
		rr.State = "done"
	}
	for i := range res.Repos {
		if res.Repos[i].State != "done" && res.Repos[i].Next == nil {
			// An owner left for after the one page this run asks for.
			ro, _, _ := strings.Cut(res.Repos[i].Repo, "/")
			var mine []string
			for _, r := range res.Repos {
				if o, _, _ := strings.Cut(r.Repo, "/"); strings.EqualFold(o, ro) {
					mine = append(mine, r.Repo)
				}
			}
			// The app is private: GitHub installs it only on the account it
			// was made on, so this owner may never be able to.
			mark(&ciStep{Kind: "info", Say: "After the page above, run zm ci add " + strings.Join(mine, " ") + " again: GitHub installs this Mac's app only on the account it was made on, so " +
				ro + "'s repositories can run here only if that is " + ro + "."}, "blocked", ro)
		}
	}
	return end(), nil
}

// ciOwnerStep reports whether a doctor's fix changes this Mac or its
// broker (lend, broker setup, links, settings, the tunnel, a daemon
// restart), anywhere in it: the owner's step, an ask. Reads (zm ci doctor,
// zm runners ls) and other plain fixes are run.
func ciOwnerStep(fix string) bool {
	for _, p := range []string{"zm lend", "zm runners setup", "zm runners link", "zm runners unlink", "zm runners set ", "zm tunnel ", "zm daemon",
		"potluck daemon", "restart the zm daemon", "lend a machine"} {
		if strings.Contains(fix, p) {
			return true
		}
	}
	return false
}

// ciOneAccount reports whether all repos belong to one account.
func ciOneAccount(repos []string) bool {
	for _, r := range repos {
		if a, _, _ := strings.Cut(r, "/"); !strings.EqualFold(a, strings.SplitN(repos[0], "/", 2)[0]) {
			return false
		}
	}
	return len(repos) > 0
}

// ciAnyStep reports whether a repository already has a step.
func ciAnyStep(rs []ciAddRepo) bool {
	for _, r := range rs {
		if r.Next != nil {
			return true
		}
	}
	return false
}

// ciLinkedAccounts is the GitHub accounts whose install is confirmed.
func ciLinkedAccounts(ctx context.Context, cl *client.Client) ([]string, error) {
	var out struct {
		Links []runners.Link `json:"links"`
	}
	if err := cl.Do(ctx, "GET", "/v0/runners/links", nil, &out); err != nil {
		return nil, err
	}
	var accounts []string
	for _, l := range out.Links {
		if !containsFold(accounts, l.Account) {
			accounts = append(accounts, l.Account)
		}
	}
	return accounts, nil
}

// ciAddLink makes sure the app exists and a confirmed install covers owner;
// it answers the page to open when one is still needed (after waiting
// o.wait for it), nil when all is in place.
func ciAddLink(ctx context.Context, cl *client.Client, st runners.Setup, owner string, repos []string, o ciAddOpts) (*ciStep, error) {
	linked := func() (bool, error) {
		accounts, err := ciLinkedAccounts(ctx, cl)
		return containsFold(accounts, owner), err
	}
	req := runners.SetupRequest{Pool: st.Pool, Org: o.org}
	if st.App == nil && o.org != "" && !strings.EqualFold(o.org, owner) {
		// A private app made on another account can never install on owner.
		return nil, fmt.Errorf("--org %s is not %s, the repository's owner: the app installs only on the account it is made on; use --org %s, or --org= if %s is your own account", o.org, owner, owner, owner)
	}
	if st.App == nil && !o.orgSet {
		org, err := ciOwnerIsOrg(ctx, owner)
		switch {
		case errors.Is(err, errNoGitHubAccount):
			return nil, fmt.Errorf("GitHub has no account %s: check the repository's name (owner/repo)", owner)
		case err != nil:
			// The app installs only on the account it is made on: never guess.
			return nil, fmt.Errorf("could not ask GitHub whether %s is an org (%v): sign gh in (gh auth login) and run this again, or say where the app goes: zm ci add <repo> --org %s for the org, --org= for your own account",
				owner, err, owner)
		case org:
			req.Org = owner
		}
	}
	var names []string
	for _, r := range repos {
		if ro, _, _ := strings.Cut(r, "/"); strings.EqualFold(ro, owner) {
			names = append(names, r)
		}
	}
	if st.App == nil {
		where := "your own GitHub account"
		if req.Org != "" {
			where = "the GitHub org " + req.Org
		}
		o.say("Will: create a GitHub app for this Mac on %s; it reads jobs and workflows and can change nothing.", where)
	} else {
		o.say("Will: install this Mac's GitHub app on %s.", owner)
	}
	// The install page comes with these repositories already selected (the
	// broker takes at most ciPreselectMax; with more the person ticks them).
	if len(names) <= ciPreselectMax {
		req.Repos = names
	}
	var next runners.Setup
	err := cl.Do(ctx, "POST", "/v0/runners/setup", req, &next)
	if e := (*api.Error)(nil); errors.As(err, &e) && e.Code == api.CodeUnavailable && strings.HasPrefix(e.Message, "repos:") {
		// GitHub would not name a repository's id: the person picks it on the page instead.
		req.Repos, next = nil, runners.Setup{}
		err = cl.Do(ctx, "POST", "/v0/runners/setup", req, &next)
	}
	if err != nil {
		return nil, err
	}
	if next.URL == "" {
		return nil, fmt.Errorf("the broker gave no page to open (next %q); run zm ci doctor %s", next.Next, names[0])
	}
	which := " with only " + strings.Join(names, ", ") + " selected"
	if len(req.Repos) > 0 {
		which = " with only " + strings.Join(names, ", ") + " selected (already ticked on the page)"
	}
	say := "Open this once and press GitHub's buttons yourself: install the app on " + owner + which + ", then Yes."
	if next.Next == "create_app" {
		say = "Open this once and press GitHub's buttons yourself: Create, then Install on " + owner + which + ", then Yes."
	}
	step := &ciStep{Kind: "ask", Say: say, URL: next.URL}
	if o.wait <= 0 {
		return step, nil
	}
	o.say("%s\n    %s", say, next.URL)
	deadline, last := time.Now().Add(o.wait), time.Now()
	stopped := func() error {
		return fmt.Errorf("stopped waiting for GitHub (%v); after the press run %s again", ctx.Err(), ciRerun(names, o))
	}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, stopped()
		case <-time.After(ciAddPoll):
		}
		if ok, err := linked(); err != nil || ok {
			if err != nil && ctx.Err() != nil { // stopped mid-read
				return nil, stopped()
			}
			if ok {
				o.say("GitHub confirmed %s.", owner)
			}
			return nil, err
		}
		if time.Since(last) >= 20*time.Second {
			last = time.Now()
			o.say("Waiting for GitHub to confirm %s (until %s) ...", owner, deadline.Format("15:04"))
		}
	}
	step.Say += " Then run this again."
	return step, nil
}

// ciPreselectMax is the most repositories the broker pre-selects on the
// install page (Potluck's runners preselect limit).
const ciPreselectMax = 20

// ciAddPoll is how often ci add asks whether GitHub confirmed a page.
var ciAddPoll = 3 * time.Second

// ciOwnerIsOrg asks GitHub, with the person's own gh sign-in, whether owner
// is an org (the app installs only on the account that owns it).
var ciOwnerIsOrg = func(ctx context.Context, owner string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, "gh", "api", "users/"+url.PathEscape(owner), "--jq", ".type").Output()
	if ee := (*exec.ExitError)(nil); errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "HTTP 404") {
		return false, errNoGitHubAccount
	}
	if err != nil {
		return false, fmt.Errorf("gh: %v", err)
	}
	return strings.TrimSpace(string(b)) == "Organization", nil
}

// errNoGitHubAccount is GitHub's 404 for an account name.
var errNoGitHubAccount = errors.New("no such GitHub account")

// ciRerun is the command that runs ci add again for names, with the job
// and workflow this run was given.
func ciRerun(names []string, o ciAddOpts) string {
	cmd := "zm ci add " + strings.Join(names, " ")
	if o.job != "" {
		cmd += " --job " + o.job
	}
	if o.workflow != "" {
		cmd += " --workflow " + o.workflow
	}
	return cmd
}

// ciPublicAsk is a public repository's yes: the owner's own, a plain
// choice; no command rides on it for an agent to run.
func ciPublicAsk(repo string) *ciStep {
	return &ciStep{Kind: "ask", Say: repo + " is public. Run its own branches' jobs on this Mac? A fork's pull request never runs here. " +
		"Only the owner says yes, by running zm ci add " + repo + " --public themselves.", Choices: []string{"yes", "no"}}
}

// ciAddRepoStep connects rr's repository, checks it and, with --job, moves
// the job; it answers the step left, nil when done.
func ciAddRepoStep(ctx context.Context, cl *client.Client, rr *ciAddRepo, connected []runners.Repo, o ciAddOpts) (*ciStep, error) {
	repo := rr.Repo
	var have *runners.Repo
	for i := range connected {
		if strings.EqualFold(connected[i].Repo, repo) {
			have = &connected[i]
		}
	}
	switch {
	case have != nil && !o.public:
		// Whether it is served is the broker's to say: the doctor's check 5.
		rr.Notes = append(rr.Notes, repo+" was already connected.")
	default:
		what := "jobs from its branches run on this Mac"
		if o.public {
			what = "jobs from its own branches run on this Mac; a fork's pull request never does"
		}
		o.say("Will: connect %s (%s).", repo, what)
		var rp runners.Repo
		err := cl.Do(ctx, "POST", "/v0/runners/repos", runners.AllowRequest{Repo: repo, Public: o.public}, &rp)
		var e *api.Error
		switch {
		case err == nil:
			rr.Changed = append(rr.Changed, "connected "+rp.Repo)
			o.say("Connected: %s.", rp.Repo)
			if rp.Warning != "" {
				rr.Notes = append(rr.Notes, rp.Warning)
				o.say("warning: %s", rp.Warning)
			}
		case errors.As(err, &e) && e.Code == api.CodeBadRequest && strings.Contains(e.Message, " is public:"):
			return ciPublicAsk(repo), nil
		case errors.As(err, &e) && (e.Code == api.CodeNotFound || e.Code == api.CodeConflict):
			// Not covered or not confirmed: the doctor names the page.
			if step, _, derr := ciAddDoctor(ctx, cl, rr); derr != nil || step != nil {
				return step, derr
			}
			return nil, err
		default:
			return nil, err
		}
	}
	step, workflowOnly, err := ciAddDoctor(ctx, cl, rr)
	switch {
	case err != nil || step != nil && !workflowOnly:
		// Any failed check but the workflow's own (8 to 10) holds the job
		// back: moved here, it would only wait.
		return step, err
	case step == nil:
		return ciAddPR(ctx, rr, o)
	}
	// Only the workflow's checks failed (a file GitHub refuses, runs-on, a
	// job without the fork guard): they do not hold back the job's pull
	// request, which may be what fixes them: its step first, else the doctor's.
	pr, err := ciAddPR(ctx, rr, o)
	if err == nil && pr != nil {
		rr.Notes = append(rr.Notes, "The CI doctor also says: "+step.Say)
		return pr, nil
	}
	if err != nil {
		rr.Notes = append(rr.Notes, "No job moved onto this Mac: "+err.Error())
	}
	return step, nil
}

// ciWorkflowCheck is a doctor check about the workflow file itself (8 a
// file GitHub refuses, 9 runs-on, 10 the fork guard): a fix the job's pull
// request may make, never a reason jobs cannot run here.
func ciWorkflowCheck(n int) bool { return n >= 8 && n <= 10 }

// ciAddDoctor runs the CI doctor on rr's repository and answers its first
// failed check as the step left, and whether every failed check is a
// workflow check (ciWorkflowCheck).
func ciAddDoctor(ctx context.Context, cl *client.Client, rr *ciAddRepo) (*ciStep, bool, error) {
	d, err := orchclient.CIDoctor(ctx, cl, rr.Repo)
	if err != nil {
		return nil, false, err
	}
	workflowOnly, base := true, map[int]bool{}
	for _, r := range d.Reports {
		for _, c := range r.Checks {
			if !strings.EqualFold(r.Repo, rr.Repo) {
				continue
			}
			if c.Status == "pass" {
				base[c.Number] = true
			}
			if c.Status == "fail" && !ciWorkflowCheck(c.Number) {
				workflowOnly = false
			}
		}
	}
	for n := 1; n <= 5; n++ { // checks 1 to 5 must pass: missing evidence is never a pass
		workflowOnly = workflowOnly && base[n]
	}
	passed := map[int]bool{}
	for _, r := range d.Reports {
		if !strings.EqualFold(r.Repo, rr.Repo) {
			continue
		}
		for _, c := range r.Checks {
			if c.Status == "pass" {
				passed[c.Number] = true
			}
			msg, fix := c.Message, c.Fix
			// Checks 1 to 5 (app, install, coverage, confirmation,
			// connection) must pass: missing evidence is never a pass.
			unavailable := c.Status == "warn" && strings.HasPrefix(msg, "unavailable:")
			switch {
			case unavailable && c.Number <= 5:
				msg = "Could not check: " + strings.TrimSpace(strings.TrimPrefix(msg, "unavailable:")) + "."
				fallthrough
			case c.Status == "fail":
				step := &ciStep{Kind: "ask", Say: msg}
				switch {
				case strings.Contains(fix, "--public"):
					// The owner's own yes, never a command an agent runs.
					return ciPublicAsk(rr.Repo), workflowOnly, nil
				case strings.HasPrefix(fix, "zm "):
					// The owner's own command (lend, broker setup, a restart) is asked, never run by an agent; a read is run.
					step.Command = fix
					if !ciOwnerStep(fix) {
						step.Kind = "run"
					}
				case strings.Contains(fix, "https://"):
					at := strings.Index(fix, "https://")
					owner, _, _ := strings.Cut(rr.Repo, "/")
					if org := strings.Index(fix, "organization owner: https://"); org >= 0 {
						if isOrg, _ := ciOwnerIsOrg(ctx, owner); isOrg {
							at = org + len("organization owner: ")
						}
					}
					step.URL = fix[at:]
					if i := strings.IndexAny(step.URL, " )"); i > 0 {
						step.URL = step.URL[:i]
					}
					step.Say = msg + " " + strings.TrimSuffix(strings.TrimSpace(fix[:strings.Index(fix, "https://")]), " at")
				case fix != "":
					step.Say = msg + " Fix: " + fix + "."
					if !ciOwnerStep(fix) {
						step.Kind = "run"
					}
				}
				return step, workflowOnly, nil
			case c.Status == "warn" && !unavailable:
				rr.Notes = append(rr.Notes, msg)
			}
		}
	}
	for n := 1; n <= 5; n++ {
		if !passed[n] {
			return &ciStep{Kind: "run", Say: fmt.Sprintf("The CI doctor did not confirm check %d for %s; see what it says, then run this again.", n, rr.Repo),
				Command: "zm ci doctor " + rr.Repo}, false, nil
		}
	}
	return nil, true, nil
}

// ciPRAnswer is potluck ci pr --json's answer (the fields zm reads).
type ciPRAnswer struct {
	State    string   `json:"state"` // done, merge, script (dry_run is never asked for)
	Job      string   `json:"job"`
	Workflow string   `json:"workflow"`
	Labels   []string `json:"labels"`
	URL      string   `json:"url"`
	Script   string   `json:"script"`
	Next     string   `json:"next"`
	Diff     string   `json:"diff"`
	Notes    []string `json:"notes"`
}

// ciNoJobToPick reports whether ci pr's refusal (without --job) is only
// that repo has no job to pick: no workflows, or none or several on a
// GitHub-hosted macOS label (Potluck's cli/ci_pr.go words). It matches the
// start of the message, never words quoted from a workflow.
func ciNoJobToPick(msg, repo string) bool {
	if strings.HasPrefix(msg, "no job in "+repo+" at ") || strings.HasPrefix(msg, "not found on GitHub: repos/"+repo+"/contents/.github/workflows?") {
		return true
	}
	n, _, ok := strings.Cut(msg, " jobs in "+repo+" run on a GitHub-hosted macOS label")
	return ok && n != "" && strings.Trim(n, "0123456789") == ""
}

// ciGHSignIn reports whether ci pr failed on the person's gh itself (not
// installed, not signed in, or no access): Potluck's ghGet words.
func ciGHSignIn(msg string) bool {
	return strings.HasPrefix(msg, "gh is not installed") || strings.HasPrefix(msg, "gh could not read ") && strings.Contains(msg, "check gh auth status")
}

// ciPotluck runs Potluck's CLI in this process (a test stubs it).
var ciPotluck = potluckcli.Run

// ciAddPR runs Potluck's ci pr --json for o.job (without it, Potluck picks
// the one job on a GitHub-hosted macOS label): the fork-guarded runs-on
// change, prepared for the person's own gh sign-in to open (the app cannot
// change workflows). It answers done, a pull request to merge, or the
// script to run. Without --job a job Potluck cannot pick is only noted: the
// repository is connected either way.
func ciAddPR(ctx context.Context, rr *ciAddRepo, o ciAddOpts) (*ciStep, error) {
	var stdout, stderr bytes.Buffer
	args := []string{"ci", "pr", rr.Repo, "--json"}
	if o.job != "" {
		args = append(args, "--job", o.job)
	}
	if o.workflow != "" {
		args = append(args, "--workflow", o.workflow)
	}
	if err := os.Setenv(potluckcli.EnvNoAutostart, "1"); err != nil { // never Potluck's start, past zm's lock
		return nil, err
	}
	code := ciPotluck(ctx, args, strings.NewReader(""), &stdout, &stderr)
	words := func(s string) string {
		return strings.NewReplacer("potluck ci pr", "zm ci add", "potluck runners ", "zm runners ", "potluck: ", "",
			"this machine's pool", "this Mac").Replace(strings.TrimSpace(s))
	}
	if code != 0 {
		msg := words(stderr.String())
		var ae api.ErrorResponse // a broker error comes as JSON under --json
		if json.Unmarshal(stderr.Bytes(), &ae) == nil && ae.Error != nil {
			msg = words(strings.TrimSpace(ae.Error.Message + "; " + ae.Error.Hint))
			msg = strings.TrimSuffix(msg, ";")
		}
		if msg == "" {
			msg = fmt.Sprintf("the workflow step for %s ended with exit %d", rr.Repo, code)
		}
		switch {
		case o.job == "" && ciNoJobToPick(msg, rr.Repo):
			// Potluck could not pick a job (no workflows, none or several): connected all the same.
			if strings.HasPrefix(msg, "not found on GitHub: ") {
				// GitHub answers 404 both for no workflows and for a sign-in
				// that may not read the repository's files.
				msg = "GitHub found no .github/workflows in " + rr.Repo + " (or your gh sign-in may not read its files: a fine-grained token needs Contents read)"
			}
			move := "zm ci add " + rr.Repo + " --job <id>"
			if o.workflow != "" {
				move += " --workflow " + o.workflow
			}
			rr.Notes = append(rr.Notes, "No job moved onto this Mac: "+msg, "To move one: "+move+".")
			o.say("No job moved onto this Mac: %s\nTo move one: %s", msg, move)
			return nil, nil
		case ciGHSignIn(msg):
			// The person's own gh sign-in: theirs to do, never an agent's.
			return &ciStep{Kind: "ask", Say: "Could not move a job onto this Mac: " + strings.TrimSuffix(msg, ".") +
				". Sign gh in as someone who can read " + rr.Repo + " (gh auth login), then run this again.", Command: "gh auth login"}, nil
		case o.job == "":
			// Connected, but the job step failed (gh not signed in, say): not done.
			step := &ciStep{Kind: "run", Say: "Could not move a job onto this Mac: " + strings.TrimSuffix(msg, ".") + ". Fix that, then run this again (or name the job with --job <id>).",
				Command: "zm ci add " + rr.Repo}
			if o.workflow != "" {
				step.Command += " --workflow " + o.workflow
			}
			if ciOwnerStep(msg) {
				step.Kind = "ask"
			}
			return step, nil
		}
		return nil, errors.New(msg)
	}
	var pr ciPRAnswer
	if err := json.Unmarshal(stdout.Bytes(), &pr); err != nil {
		return nil, fmt.Errorf("zm ci add did not understand the workflow step's answer (%v): %s", err, words(stdout.String()))
	}
	job := pr.Job
	if job == "" {
		job = o.job
	}
	for _, n := range pr.Notes {
		rr.Notes = append(rr.Notes, words(n))
		o.say("%s", words(n))
	}
	if pr.Diff != "" {
		o.say("Job %s in %s: own branches will ask for %s (this Mac); pull requests from forks stay on GitHub.", job, pr.Workflow, strings.Join(pr.Labels, ", "))
		o.say("%s", strings.TrimRight(pr.Diff, "\n"))
	}
	switch {
	case pr.State == "done":
		rr.Notes = append(rr.Notes, "Job "+job+" already runs on this Mac for "+rr.Repo+"'s own branches.")
		return nil, nil
	case pr.State == "merge" && pr.URL != "":
		return &ciStep{Kind: "ask", Say: "Merge the pull request that moves job " + job + " onto this Mac, then run this again.", URL: pr.URL}, nil
	case pr.State == "script" && pr.Next != "" && pr.Script != "":
		rr.Changed = append(rr.Changed, "prepared the workflow change for job "+job)
		if !o.json {
			if b, err := os.ReadFile(pr.Script); err == nil {
				o.say("The pull request is opened with your own gh sign-in (the app cannot change workflows); this script does it:\n%s", strings.TrimRight(string(b), "\n"))
			}
		}
		return &ciStep{Kind: "ask", Say: "Open the pull request with your own gh sign-in (it pushes one branch and changes only job " + job + "'s runs-on), merge it, then run this again.",
			Command: pr.Next}, nil
	}
	return nil, fmt.Errorf("zm ci add did not understand the workflow step's answer: state %q", pr.State)
}

func containsFold(list []string, s string) bool {
	for _, x := range list {
		if strings.EqualFold(x, s) {
			return true
		}
	}
	return false
}
