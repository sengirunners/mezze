package main

// The issue-* rows: native issues over the daemon's socket on a generated
// store of trackerSize issues, the 16 ms bar (SPEC "Native issues"). The
// store needs no beads copy and no secret. It has the shape of a real
// import in DESIGN-NOTES "Native issues", per issue: 1.3 comments (most have
// none, a few have dozens), 1.05 dependencies, 1.1 labels, 6 history rows;
// 80% closed. The same file seeds the team rows' authority (replica.go).

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/issues/store/sqlite"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/ids"
)

const (
	issueProject = "nat"     // the project of the issue rows, and of the team rows' authority
	issuePrefix  = "harbor"  // ids: harbor-a1b2c
	issueArea    = "trackb"  // the area member's area: issues labelled areaLabel
	areaLabel    = "track-b" // on about 8% of the issues
	workPrefix   = "issue"   // the native tracker binding's prefix
	seedProject  = "seed"    // the generated store's directory name
	seedBatch    = 1000      // issues per import batch
	childEvery   = 12        // one issue in childEvery is a child of the one before
	idSpace      = 60466176  // 36^5: five base-36 characters
	idStep       = 48271     // coprime with idSpace, so i*idStep%idSpace never repeats
)

// searchWords are what the search rows ask for. Each is in about one issue
// of five (word), as "solver" or "deploy" are in a real store: enough to
// rank and page, far from every row.
var searchWords = []string{"solver", "migration", "coverage", "swap", "deploy", "board", "resident", "call", "mcp", "rls"}

// rankWords alternates one search word and two (two common words walk two
// long posting lists: the slower ranking).
func rankWords(i int) string {
	w := searchWords[i%len(searchWords)]
	if i%2 == 1 {
		w += " " + searchWords[(i+3)%len(searchWords)]
	}
	return w
}

// fillerStems and fillerEnds make the rest of the vocabulary.
var (
	fillerStems = strings.Fields("sched block roster shift rota queue lease mail sync audit trade cover")
	fillerEnds  = strings.Fields("ing er ed ment ance ity ful less able ism ive ure")
)

func nativeRow(name string) bool { return strings.HasPrefix(name, "issue-") }

// nativeEnv is what the issue rows need.
type nativeEnv struct {
	seed   string   // the generated issues.db
	ids    []string // every issue
	open   []string // the open ones
	owner  *client.Client
	member *client.Client // a member of area issueArea
}

// wanted reports whether this run measures a row that match accepts.
func (e *env) wanted(match func(string) bool) bool {
	for _, name := range e.want {
		if match(name) {
			return true
		}
	}
	return false
}

// setupIssues generates the store and starts what the rows of this run need.
func (e *env) setupIssues() error {
	ctx := context.Background()
	team := e.replica && e.wanted(teamRow)
	if !team && !e.wanted(nativeRow) {
		return nil
	}
	t0 := time.Now()
	n, err := genStore(ctx, filepath.Join(e.tmp, "seed"))
	if err != nil {
		return fmt.Errorf("generating the issue store: %w", err)
	}
	fmt.Printf("generated %d issues in %s\n", len(n.ids), time.Since(t0).Round(time.Millisecond))
	e.native = n
	if e.wanted(nativeRow) {
		if err := e.startIssueDaemon(); err != nil {
			return err
		}
		sock := filepath.Join(e.issueHome, "zm.sock")
		tok, err := bindNative(ctx, client.NewUnix(sock), e.issueHome, n.seed, "mina")
		if err != nil {
			return fmt.Errorf("binding the issue store: %w", err)
		}
		n.owner = client.NewUnix(sock)
		n.owner.Project, n.owner.ProjectFrom = issueProject, "flag"
		n.member = client.NewUnix(sock)
		n.member.Token = tok
	}
	if team {
		return e.startTeam(ctx)
	}
	return nil
}

// startIssueDaemon starts a second daemon for the issue rows. On the first
// one, a second 11.5k-item project would change what the other rows measure:
// zm work ls -q searches every project the caller sees.
func (e *env) startIssueDaemon() error {
	e.issueHome = filepath.Join(e.tmp, "i")
	if err := os.MkdirAll(e.issueHome, 0o700); err != nil {
		return err
	}
	c := e.cmd([]string{"ZM_HOME=" + e.issueHome}, "daemon")
	if err := c.Start(); err != nil {
		return err
	}
	e.issueDaemon = c
	return waitHealth(filepath.Join(e.issueHome, "zm.sock"), 10*time.Second)
}

// genStore writes the store into dir and closes it, so the file can be copied.
func genStore(ctx context.Context, dir string) (*nativeEnv, error) {
	st := sqlite.Open(dir)
	defer st.Close()
	ps, err := st.Project(ctx, seedProject, true)
	if err != nil {
		return nil, err
	}
	if err := ps.SetPrefix(ctx, issuePrefix); err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewPCG(1, 2)) // the same issues every run
	now := time.Now().UTC().Truncate(time.Second)
	n := &nativeEnv{seed: filepath.Join(dir, "projects", seedProject, sqlite.FileName)}
	var b istore.ImportBatch
	for i := range trackerSize {
		genIssue(&b, n, i, rng, now)
		if len(b.Issues) == seedBatch {
			if err := ps.Import(ctx, b); err != nil {
				return nil, err
			}
			b = istore.ImportBatch{}
		}
	}
	if err := ps.Import(ctx, b); err != nil {
		return nil, err
	}
	area := func(_ string, labels []string) string {
		for _, l := range labels {
			if l == areaLabel {
				return issueArea
			}
		}
		return ""
	}
	if err := ps.FinishImport(ctx, area); err != nil {
		return nil, err
	}
	return n, nil
}

// genIssue adds issue i, with its comments, edges and history, to b.
func genIssue(b *istore.ImportBatch, n *nativeEnv, i int, rng *rand.Rand, now time.Time) {
	id := fmt.Sprintf("%s-%05s", issuePrefix, strconv.FormatInt(int64(i+1)*idStep%idSpace, 36))
	created := now.Add(-time.Duration(trackerSize-i) * 45 * time.Minute) // a year, oldest first
	updated := created.Add(time.Duration(rng.IntN(72)) * time.Hour)
	parent := ""
	if i%childEvery == childEvery-1 {
		parent = n.ids[i-1]
		id = fmt.Sprintf("%s.%d", parent, 1)
	}
	it := orchapi.Issue{ID: id, Title: issueTitle(i, rng), Description: issueBody(rng), Status: "closed", Priority: rng.IntN(4),
		IssueType: []string{"task", "task", "bug", "feature"}[rng.IntN(4)], CreatedAt: created, UpdatedAt: updated,
		CreatedBy: "agent", Owner: "dev@example.com", Labels: issueLabels(rng)}
	switch x := rng.IntN(100); {
	case x < 16:
		it.Status = "open"
	case x < 18:
		it.Status = "in_progress"
		it.Assignee = "agent"
	case x < 19:
		it.Status = "deferred"
	case x < 20:
		it.Status = "blocked"
	default:
		closed := updated
		it.ClosedAt, it.CloseReason = &closed, "done"
	}
	for _, l := range it.Labels {
		if l == areaLabel {
			it.Area = issueArea
		}
	}
	b.Issues = append(b.Issues, it)
	n.ids = append(n.ids, id)
	if it.Status == "open" {
		n.open = append(n.open, id)
	}
	if parent != "" {
		b.Deps = append(b.Deps, orchapi.IssueDep{IssueID: id, DependsOnID: parent, Type: "parent-child", CreatedAt: created, CreatedBy: "agent", SourceID: "p" + id})
	} else if i > 0 && rng.IntN(100) < 92 { // a recent issue: work comes in chains
		dep := n.ids[max(0, i-1-rng.IntN(200))]
		if dep != id {
			b.Deps = append(b.Deps, orchapi.IssueDep{IssueID: id, DependsOnID: dep, Type: "blocks", CreatedAt: created, CreatedBy: "agent", SourceID: "d" + id})
		}
	}
	// 1.3 comments on average: 3 in 5 issues have none, a few very many.
	if rng.IntN(5) >= 3 {
		for c := range min(1+int(rng.ExpFloat64()*2.3), 60) {
			at := created.Add(time.Duration(c+1) * time.Minute)
			b.Comments = append(b.Comments, orchapi.IssueComment{ID: ids.New(), IssueID: id, Author: "agent", CreatedAt: at, Text: issueComment(rng)})
		}
	}
	for ev := range 6 {
		kind := []string{"created", "updated", "updated", "status_changed", "commented", "closed"}[ev]
		b.History = append(b.History, istore.ImportedEvent{SourceID: fmt.Sprintf("%s/%d", id, ev), IssueID: id, Kind: kind, Actor: "agent",
			At: created.Add(time.Duration(ev) * time.Minute)})
	}
}

func issueTitle(i int, rng *rand.Rand) string {
	return fmt.Sprintf("Fix the %s %s in module %d", word(rng), []string{"race", "timeout", "drift", "refusal", "copy"}[rng.IntN(5)], i%40)
}

// issueBody is about 1 KiB.
func issueBody(rng *rand.Rand) string {
	var b strings.Builder
	for b.Len() < 900 {
		fmt.Fprintf(&b, "Steps to reproduce the %s failure, the logs from the %s run and the plan for %s. ", word(rng), word(rng), word(rng))
	}
	return b.String()
}

func issueComment(rng *rand.Rand) string {
	var b strings.Builder
	for range 2 + rng.IntN(8) {
		fmt.Fprintf(&b, "Checked the %s path against the %s run, the numbers hold. ", word(rng), word(rng))
	}
	return b.String()
}

// word is one word of 12 a search word, else filler. A body has about 30.
func word(rng *rand.Rand) string {
	if rng.IntN(12) == 0 {
		return searchWords[rng.IntN(len(searchWords))]
	}
	return fillerStems[rng.IntN(len(fillerStems))] + fillerEnds[rng.IntN(len(fillerEnds))]
}

// issueLabels gives 1.1 labels on average, from a short list.
func issueLabels(rng *rand.Rand) []string {
	var ls []string
	for _, l := range []struct {
		name string
		pct  int
	}{{"ux", 30}, {"solver", 16}, {"tech-debt", 14}, {"migration", 12}, {areaLabel, 8}, {"docs", 8}, {"flaky", 7}, {"perf", 6}, {"mobile", 5}, {"api", 4}} {
		if rng.IntN(100) < l.pct {
			ls = append(ls, l.name)
		}
	}
	return ls
}

// bindNative makes project issueProject on the daemon c talks to, gives it
// the generated store (a copy, before the daemon opens it) and a native
// tracker binding, waits for the work mirror, and returns a token of a member
// of the area issueArea.
func bindNative(ctx context.Context, c *client.Client, home, seed, member string) (string, error) {
	p, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: issueProject})
	if err != nil {
		return "", err
	}
	if _, err := c.AddArea(ctx, issueProject, api.Area{Name: issueArea, Labels: []string{areaLabel}}); err != nil {
		return "", err
	}
	tok, err := c.MintToken(ctx, api.MintTokenRequest{As: member, Project: issueProject, Area: issueArea, Role: "member"})
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(seed)
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "projects", p.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, sqlite.FileName), b, 0o600); err != nil {
		return "", err
	}
	at := *c
	at.Project, at.ProjectFrom = issueProject, "flag"
	if _, err := orchclient.AddTracker(ctx, &at, orchapi.AddTrackerRequest{Adapter: orchapi.AdapterNative, Prefix: workPrefix, WaitSeconds: 10}); err != nil {
		return "", err
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var src orchapi.WorkSourceList
		if err := at.Do(ctx, "GET", "/v0/work/sources?project="+issueProject, nil, &src); err != nil {
			return "", err
		}
		for _, s := range src.Sources {
			if s.Prefix == workPrefix && s.Sync != nil && s.Sync.State == orchapi.SyncOK && s.Sync.Items >= trackerSize {
				return tok.Secret, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("the mirror of the native tracker did not fill after 3m: %+v", src.Sources)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// timedN times f, which gets a count that goes up by one on every call.
func timedN(f func(i int) error) func() (time.Duration, error) {
	i := -1
	return func() (time.Duration, error) {
		i++
		t := time.Now()
		err := f(i)
		return time.Since(t), err
	}
}

// fresh is a new open issue, untimed: a claim or a close needs its own.
func (n *nativeEnv) fresh(ctx context.Context) (string, error) {
	it, err := orchclient.CreateIssue(ctx, n.owner, orchapi.NewIssueRequest{Title: "bench fresh issue"})
	return it.ID, err
}

func (e *env) nativeRows() map[string]func() (time.Duration, error) {
	ctx := context.Background()
	n := e.native
	if n == nil || n.owner == nil {
		return nil
	}
	rng := rand.New(rand.NewPCG(3, 4))
	anyID := func() string { return n.ids[rng.IntN(len(n.ids))] }
	openID := func() string { return n.open[rng.IntN(len(n.open))] }
	const allOpen = "open,in_progress,blocked,deferred,pinned,hooked,in_review"
	list := func(c *client.Client, q func(i int) orchapi.IssueQuery) func() (time.Duration, error) {
		return timedN(func(i int) error {
			l, err := orchclient.Issues(ctx, c, q(i))
			if err == nil && len(l.Issues) == 0 {
				err = fmt.Errorf("an empty list for %+v", q(i)) // a row must time something real
			}
			return err
		})
	}
	claimOrClose := func(do func(id string) error) func() (time.Duration, error) {
		return func() (time.Duration, error) {
			id, err := n.fresh(ctx)
			if err != nil {
				return 0, err
			}
			t := time.Now()
			err = do(id)
			return time.Since(t), err
		}
	}
	search := func(c *client.Client) func(i int) orchapi.IssueQuery {
		return func(i int) orchapi.IssueQuery {
			return orchapi.IssueQuery{Q: searchWords[i%len(searchWords)], Status: "all"}
		}
	}
	return map[string]func() (time.Duration, error){
		"issue-list":      list(n.owner, func(int) orchapi.IssueQuery { return orchapi.IssueQuery{} }),
		"issue-list-deps": list(n.owner, func(int) orchapi.IssueQuery { return orchapi.IssueQuery{Deps: true} }),
		"issue-ready":     list(n.owner, func(int) orchapi.IssueQuery { return orchapi.IssueQuery{Ready: true, Limit: 100, Deps: true} }),
		"issue-show": timedN(func(int) error {
			d, err := orchclient.Issue(ctx, n.owner, anyID())
			if err == nil && d.Title == "" {
				err = fmt.Errorf("an empty issue")
			}
			return err
		}),
		"issue-search": list(n.owner, search(n.owner)),
		"issue-substr": list(n.owner, func(i int) orchapi.IssueQuery {
			return orchapi.IssueQuery{Substr: searchWords[i%len(searchWords)], Status: allOpen}
		}),
		// bd search: ranked (bm25), as an agent checks for an existing issue.
		"issue-rank": list(n.owner, func(i int) orchapi.IssueQuery {
			return orchapi.IssueQuery{Q: rankWords(i), Status: allOpen, Sort: "rank", Limit: 10, Count: true}
		}),
		"issue-rank-all": list(n.owner, func(i int) orchapi.IssueQuery {
			return orchapi.IssueQuery{Q: rankWords(i), Status: "all", Sort: "rank", Limit: 10, Count: true}
		}),
		"issue-list-area":   list(n.member, func(int) orchapi.IssueQuery { return orchapi.IssueQuery{Deps: true, Status: "all"} }),
		"issue-search-area": list(n.member, search(n.member)),
		"issue-create": timedN(func(i int) error {
			_, err := orchclient.CreateIssue(ctx, n.owner, orchapi.NewIssueRequest{Title: fmt.Sprintf("bench issue %d", i), Description: "bench", Labels: []string{"bench"}})
			return err
		}),
		"issue-update": timedN(func(i int) error {
			notes := fmt.Sprintf("note %d", i)
			_, err := orchclient.UpdateIssue(ctx, n.owner, openID(), orchapi.UpdateIssueRequest{Notes: &notes})
			return err
		}),
		"issue-comment": timedN(func(int) error {
			_, err := orchclient.CommentIssue(ctx, n.owner, anyID(), orchapi.IssueCommentRequest{Text: "bench comment"})
			return err
		}),
		"issue-claim": claimOrClose(func(id string) error { _, err := orchclient.ClaimIssue(ctx, n.owner, id); return err }),
		"issue-close": claimOrClose(func(id string) error {
			_, err := orchclient.CloseIssue(ctx, n.owner, id, orchapi.CloseIssueRequest{Reason: "bench"})
			return err
		}),
	}
}
