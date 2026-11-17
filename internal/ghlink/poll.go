package ghlink

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/store"
)

// ghPull is the part of GitHub's pull request JSON the linker reads (the
// REST list and the webhook payload share it).
type ghPull struct {
	Number   int     `json:"number"`
	Title    string  `json:"title"`
	Body     *string `json:"body"`
	State    string  `json:"state"` // open | closed
	Merged   bool    `json:"merged"`
	MergedAt *string `json:"merged_at"`
	HTMLURL  string  `json:"html_url"`
	Draft    bool    `json:"draft"`
	Updated  string  `json:"updated_at"`
	Head     struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	User           struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (g ghPull) snapshot(repo string) (PR, bool) {
	t, err := time.Parse(time.RFC3339, g.Updated)
	if err != nil || g.Number <= 0 {
		return PR{}, false
	}
	p := PR{Repo: strings.ToLower(repo), Number: g.Number, Title: g.Title, Branch: g.Head.Ref, URL: g.HTMLURL, Draft: g.Draft,
		Updated: t.UTC()}
	if g.Body != nil {
		p.Body = *g.Body
	}
	p.HeadSHA, p.Base, p.Author = g.Head.SHA, g.Base.Ref, g.User.Login
	for _, lb := range g.Labels {
		if lb.Name != "" {
			p.Labels = append(p.Labels, lb.Name)
		}
	}
	switch {
	case g.State == "open":
		p.State = StateOpen
	case g.Merged || (g.MergedAt != nil && *g.MergedAt != ""):
		p.State = StateMerged
		if g.MergedAt != nil {
			if t, err := time.Parse(time.RFC3339, *g.MergedAt); err == nil {
				t = t.UTC()
				p.MergedAt = &t
			}
		}
		p.MergeSHA = g.MergeCommitSHA // only meaningful once merged
	case g.State == "closed":
		p.State = StateClosed
	default:
		return PR{}, false
	}
	return p, true
}

// startPoller starts (or restarts, or stops when it polls no more) the
// poller for l. No-op before Start.
func (s *Service) startPoller(l link) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx == nil || s.ctx.Err() != nil {
		return
	}
	if cancel := s.pollers[l.Project]; cancel != nil {
		cancel()
		delete(s.pollers, l.Project)
	}
	if l.PollSeconds <= 0 {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.pollers[l.Project] = cancel
	project := l.Project
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			l, err := s.link(ctx, project)
			if err != nil || l.PollSeconds <= 0 {
				s.reap(ctx, project)
				return
			}
			if _, err := s.poll(ctx, l); err != nil && ctx.Err() == nil {
				s.setError(ctx, project, "poll: "+errText(err))
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(max(time.Duration(l.PollSeconds)*time.Second, s.MinPoll)):
			}
		}
	}()
}

func (s *Service) stopPoller(project string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cancel := s.pollers[project]; cancel != nil {
		cancel()
		delete(s.pollers, project)
	}
}

// poll lists each repo's pull requests updated since its cursor (newest
// first, page by page, at most 10 pages of 50) and queues them oldest first.
func (s *Service) poll(ctx context.Context, l link) (int, error) {
	total := 0
	var errs []string
	for _, repo := range l.Repos {
		since := s.now().Add(-s.Lookback)
		if c, ok := l.Cursors[repo]; ok {
			if t, err := time.Parse(time.RFC3339Nano, c); err == nil {
				since = t
			}
		}
		prs, newest, err := s.pollRepo(ctx, l, repo, since)
		if err != nil {
			errs = append(errs, repo+": "+errText(err))
			continue
		}
		if err := s.enqueue(ctx, l.Project, prs); err != nil {
			return total, err
		}
		total += len(prs)
		// The cursor moves only after the snapshots are safely queued.
		l.Cursors[repo] = newest.Format(time.RFC3339Nano)
		// CI results (ci.go). Apply what was just queued first, so a PR
		// first seen in this pass, whose runs already finished, is linked to
		// its items before its runs are looked at (apply is serialized and
		// idempotent, so racing the worker is harmless).
		s.drain(ctx)
		cur, err := s.ciPoll(ctx, l, repo)
		if err != nil {
			errs = append(errs, repo+" CI: "+errText(err))
		}
		if cur != "" {
			l.Cursors["ci:"+repo] = cur
		}
	}
	cur, _ := json.Marshal(l.Cursors)
	_, _ = s.db().ExecContext(ctx, `UPDATE ghlink_links SET cursors = ?, last_poll = ? WHERE project = ?`,
		string(cur), store.Millis(s.now()), l.Project)
	if len(errs) > 0 {
		return total, errors.New(strings.Join(errs, "; "))
	}
	return total, nil
}

func (s *Service) pollRepo(ctx context.Context, l link, repo string, since time.Time) ([]PR, time.Time, error) {
	var prs []PR
	newest := since
	for page := 1; page <= 10; page++ {
		out, err := s.gh(ctx, l, "api", "--method", "GET",
			fmt.Sprintf("repos/%s/pulls?state=all&sort=updated&direction=desc&per_page=50&page=%d", repo, page))
		if err != nil {
			return nil, since, err
		}
		var pulls []ghPull
		if err := json.Unmarshal(out, &pulls); err != nil {
			return nil, since, errors.New("gh api answered something that is not a pull request list")
		}
		older := false
		for _, g := range pulls {
			p, ok := g.snapshot(repo)
			if !ok {
				continue
			}
			// A short overlap: PRs updated in the same second as the cursor
			// are not missed (the inbox drops what it already applied).
			if p.Updated.Before(since.Add(-2 * time.Minute)) {
				older = true
				break
			}
			prs = append(prs, p)
			if p.Updated.After(newest) {
				newest = p.Updated
			}
		}
		if older || len(pulls) < 50 {
			break
		}
	}
	sort.SliceStable(prs, func(i, j int) bool { return prs[i].Updated.Before(prs[j].Updated) })
	return prs, newest, nil
}

// gh runs the GitHub CLI read-only with an allowlisted environment: PATH,
// HOME (gh's own login), LANG, TERM, TZ, GH_HOST when configured and
// GH_TOKEN only from the link's token file. Never a write.
func (s *Service) gh(ctx context.Context, l link, args ...string) ([]byte, error) {
	return s.ghLimit(ctx, l, 16<<20, args...)
}

func (s *Service) ghLimit(ctx context.Context, l link, maxOutput int, args ...string) ([]byte, error) {
	bin := l.Bin
	if bin == "" {
		bin = "gh"
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.WaitDelay = 2 * time.Second
	env := []string{"GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1"}
	for _, k := range []string{"PATH", "HOME", "LANG", "TERM", "TZ"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if l.Host != "" {
		env = append(env, "GH_HOST="+l.Host)
	}
	if l.TokenFile != "" {
		v, err := secrets.Get(ctx, secrets.File(l.TokenFile))
		if err != nil {
			return nil, api.Errorf(api.CodeUnavailable, "cannot read the link's token file: %v", err)
		}
		env = append(env, "GH_TOKEN="+v.Reveal())
	}
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &limited{w: &out, n: maxOutput}, &limited{w: &errb, n: 64 << 10}
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return nil, api.Errorf(api.CodeUnavailable, "cannot run %s", bin).WithHint("install the GitHub CLI (gh) and run gh auth login")
		}
		line, _, _ := strings.Cut(strings.TrimSpace(errb.String()), "\n")
		// What gh printed is returned too: a GraphQL answer naming a missing object exits 1 with its JSON on stdout.
		return out.Bytes(), api.Errorf(api.CodeUnavailable, "gh api: %s", clip(line, 200))
	}
	return out.Bytes(), nil
}

type limited struct {
	w interface{ Write([]byte) (int, error) }
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	q := p
	if len(q) > l.n {
		q = q[:l.n]
	}
	l.n -= len(q)
	_, err := l.w.Write(q)
	return len(p), err
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
