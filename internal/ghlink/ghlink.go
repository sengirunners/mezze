// Package ghlink moves work items when GitHub pull requests name them (SPEC
// "Webhooks and GitHub linking"). A branch name, PR title or PR body that
// names a work item id links the PR to the item and moves it: opened ->
// in progress (unless someone holds it), merged -> done (the comment names
// the PR), closed unmerged -> open again. PR snapshots arrive from GitHub's
// webhook (POST /v0/github/hook, signature-verified) or from polling with
// `gh api` (no inbound port), land in one durable inbox and are applied in
// order by one worker.
//
// The linker changes items only through the work API (/v0/work), as its own
// identity (zm-github) with a member token capped to the linked project, so
// the work module's authz and events apply and native issues plug in
// through the same routes.
package ghlink

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/ghlink/browse"
	"github.com/sengirunners/mezze/internal/ghlink/gate"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/secrets"
	"github.com/sengirunners/potluck/kit/store"
)

const schema = `
CREATE TABLE ghlink_links (
	project      TEXT PRIMARY KEY,
	repos        TEXT NOT NULL,
	prefixes     TEXT NOT NULL DEFAULT '[]',
	sources      TEXT NOT NULL DEFAULT '[]',
	on_open      TEXT NOT NULL,
	on_merge     TEXT NOT NULL,
	on_close     TEXT NOT NULL,
	poll_seconds INTEGER NOT NULL DEFAULT 0,
	bin          TEXT NOT NULL DEFAULT '',
	host         TEXT NOT NULL DEFAULT '',
	token_file   TEXT NOT NULL DEFAULT '',
	secret       TEXT NOT NULL,
	token_id     TEXT NOT NULL DEFAULT '',
	cursors      TEXT NOT NULL DEFAULT '{}',
	created      INTEGER NOT NULL,
	last_poll    INTEGER,
	last_event   INTEGER,
	last_error   TEXT NOT NULL DEFAULT ''
);
CREATE TABLE ghlink_inbox (
	id       TEXT PRIMARY KEY,
	project  TEXT NOT NULL,
	pr       TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	next_at  INTEGER NOT NULL
);
CREATE TABLE ghlink_prs (
	project TEXT NOT NULL,
	repo    TEXT NOT NULL,
	number  INTEGER NOT NULL,
	updated INTEGER NOT NULL,
	PRIMARY KEY (project, repo, number)
);
CREATE TABLE ghlink_items (
	project TEXT NOT NULL,
	repo    TEXT NOT NULL,
	number  INTEGER NOT NULL,
	item    TEXT NOT NULL,
	state   TEXT NOT NULL,
	PRIMARY KEY (project, repo, number, item)
);
CREATE TABLE ghlink_deliveries (
	project  TEXT NOT NULL,
	delivery TEXT NOT NULL,
	time     INTEGER NOT NULL,
	PRIMARY KEY (project, delivery)
);`

// schema2 adds gates (SPEC 18.3): the link's gate and match texts, when the
// linker last wrote to an item for a PR, and the decisions a gate holds.
const schema2 = `
ALTER TABLE ghlink_links ADD COLUMN gate TEXT NOT NULL DEFAULT '';
ALTER TABLE ghlink_links ADD COLUMN match TEXT NOT NULL DEFAULT '[]';
ALTER TABLE ghlink_items ADD COLUMN last_write INTEGER;
CREATE TABLE ghlink_waits (
	project   TEXT NOT NULL,
	repo      TEXT NOT NULL,
	number    INTEGER NOT NULL,
	item      TEXT NOT NULL,
	state     TEXT NOT NULL,
	pr        TEXT NOT NULL,
	first     INTEGER NOT NULL,
	next_at   INTEGER NOT NULL,
	attempts  INTEGER NOT NULL DEFAULT 0,
	last_note TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (project, repo, number, item)
);
CREATE INDEX ghlink_waits_due ON ghlink_waits (next_at);`

// schema3 records which CI results were mailed to which item (ci.go), so a
// repeated pass sends nothing.
const schema3 = `
CREATE TABLE ghlink_ci (
	project TEXT NOT NULL,
	repo    TEXT NOT NULL,
	number  INTEGER NOT NULL,
	item    TEXT NOT NULL,
	result  TEXT NOT NULL,
	time    INTEGER NOT NULL,
	PRIMARY KEY (project, repo, number, item, result)
);`

// schema4 records what each CI result was and where it went, so
// the daily summary counts the results no one was mailed.
const schema4 = `
ALTER TABLE ghlink_ci ADD COLUMN verdict TEXT NOT NULL DEFAULT '';
ALTER TABLE ghlink_ci ADD COLUMN sent TEXT NOT NULL DEFAULT '';
CREATE INDEX ghlink_ci_time ON ghlink_ci (project, time);`

var ghlinkMigrations = []store.Migration{
	{Kind: store.KindRegistry, Name: "ghlink/0001_init", Script: schema},
	{Kind: store.KindRegistry, Name: "ghlink/0002_gate", Script: schema2},
	{Kind: store.KindRegistry, Name: "ghlink/0003_ci", Script: schema3},
	{Kind: store.KindRegistry, Name: "ghlink/0004_ci_sent", Script: schema4},
}

// Match texts (--match).
const (
	MatchTitle  = "title"
	MatchBranch = "branch"
	MatchBody   = "body"
)

// Identity is who the linker's item writes are attributed to.
const Identity = orchapi.IdentityGitHub

// ReceiverPath is GitHub's webhook URL path on any zm listener.
const ReceiverPath = "/v0/github/hook"

// PR is one pull request snapshot, from a webhook or a poll.
type PR struct {
	Repo    string    `json:"repo"` // owner/name, lower case
	Number  int       `json:"number"`
	Title   string    `json:"title"`
	Body    string    `json:"body"`
	Branch  string    `json:"branch"`
	URL     string    `json:"url"`
	State   string    `json:"state"` // open | merged | closed
	Draft   bool      `json:"draft,omitempty"`
	Updated time.Time `json:"updated"`
	// For a gate (18.3).
	Labels   []string   `json:"labels,omitempty"`
	MergedAt *time.Time `json:"merged_at,omitempty"`
	MergeSHA string     `json:"merge_sha,omitempty"`
	HeadSHA  string     `json:"head_sha,omitempty"`
	Base     string     `json:"base,omitempty"`   // the branch it merges into
	Author   string     `json:"author,omitempty"` // the PR author's GitHub login
}

// PR states.
const (
	StateOpen   = "open"
	StateMerged = "merged"
	StateClosed = "closed"
)

// Service is the GitHub linking module.
type Service struct {
	d *core.Deps

	// Lookback is how far back the first poll of a repo looks.
	Lookback time.Duration
	// MinPoll is the shortest poll interval allowed.
	MinPoll time.Duration
	// Retry is the pause before an inbox entry that failed is tried again
	// (doubling, at most MaxAttempts times).
	Retry       time.Duration
	MaxAttempts int
	// GateRecheck, when set, overrides a gate's recheck pace (tests).
	GateRecheck time.Duration
	Now         func() time.Time

	applyMu sync.Mutex               // one apply at a time: snapshots of one PR never race
	gates   map[string]*gate.Process // per gate path: describe is cached until the file changes
	mu      sync.Mutex
	pollers map[string]context.CancelFunc
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	wake    chan struct{}
	browse  *browse.Service
}

// Migrations are this module's tables (core.Deps.AddMigrations); New adds them.
var Migrations = slices.Concat(ghlinkMigrations, repoListMigrations, eventsMigrations)

// New returns the module.
func New(d *core.Deps) *Service {
	d.AddMigrations(Migrations...)
	s := &Service{d: d, Lookback: 24 * time.Hour, MinPoll: 15 * time.Second, Retry: 30 * time.Second, MaxAttempts: 8,
		Now: time.Now, pollers: map[string]context.CancelFunc{}, wake: make(chan struct{}, 1), gates: map[string]*gate.Process{}}
	s.browse = browse.New(d, s.browseFetch)
	s.browse.Now = s.now
	orchcore.AddNoticeCounter(d, s.noticeCounts)
	d.AddBackupScrubber(func(ctx context.Context, tx *sql.Tx) error {
		// The link's webhook secret never goes off-site: a restored
		// home answers no GitHub delivery (verifyHub refuses an empty secret)
		// until it is linked again (a fresh secret is minted and printed).
		_, err := tx.ExecContext(ctx, `UPDATE ghlink_links SET secret = ''`)
		return err
	})
	return s
}

func (s *Service) now() time.Time { return s.Now().UTC() }
func (s *Service) db() *sql.DB    { return s.d.Store.Registry() }

// Start resumes the pollers and the inbox worker.
func (s *Service) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.ctx != nil {
		s.mu.Unlock()
		return nil
	}
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.mu.Unlock()
	links, err := s.links(s.ctx)
	if err != nil {
		return err
	}
	for _, l := range links {
		s.startPoller(l)
	}
	s.wg.Add(1)
	go s.work(s.ctx)
	return nil
}

// Close stops pollers and the worker; the inbox survives for the next start.
func (s *Service) Close() error {
	s.mu.Lock()
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.wg.Wait()
	return nil
}

func (s *Service) poke() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// ---- link config ----

type link struct {
	Project                  string
	Repos, Prefixes, Sources []string
	OnOpen, OnMerge, OnClose string
	PollSeconds              int
	Bin, Host, TokenFile     string
	Secret, TokenID          string
	Gate                     string   // an executable (18.3), "" = none
	Match                    []string // texts the matcher reads; empty = all three
	Cursors                  map[string]string
	Created                  int64
	LastPoll, LastEvent      sql.NullInt64
	LastError                string
}

const linkCols = `project, repos, prefixes, sources, on_open, on_merge, on_close, poll_seconds, bin, host, token_file, secret,
	token_id, cursors, created, last_poll, last_event, last_error, gate, match`

func scanLink(sc interface{ Scan(...any) error }) (link, error) {
	var l link
	var repos, prefixes, sources, cursors, match string
	err := sc.Scan(&l.Project, &repos, &prefixes, &sources, &l.OnOpen, &l.OnMerge, &l.OnClose, &l.PollSeconds, &l.Bin, &l.Host,
		&l.TokenFile, &l.Secret, &l.TokenID, &cursors, &l.Created, &l.LastPoll, &l.LastEvent, &l.LastError, &l.Gate, &match)
	_ = json.Unmarshal([]byte(match), &l.Match)
	_ = json.Unmarshal([]byte(repos), &l.Repos)
	_ = json.Unmarshal([]byte(prefixes), &l.Prefixes)
	_ = json.Unmarshal([]byte(sources), &l.Sources)
	_ = json.Unmarshal([]byte(cursors), &l.Cursors)
	if l.Cursors == nil {
		l.Cursors = map[string]string{}
	}
	return l, err
}

// hosted reports whether this machine is still the home of project's link: the
// project is in the registry and this machine holds no replica of it. When a
// project moves, this machine joins its new authority under the same ULID
// (team remotes) while the old registry entry stays under another slug, so
// the registry alone proves nothing. A row of a project that is not hosted
// here must never supply gh, GH_HOST or GH_TOKEN, poll, apply or take a
// webhook.
func (s *Service) hosted(project string) bool {
	if s.d.Projects != nil {
		if _, ok := s.d.Projects.Lookup(project); !ok {
			return false
		}
	}
	return !s.replicated(project)
}

// replicated reports whether this machine holds a joined copy of project.
func (s *Service) replicated(project string) bool {
	tr, ok := s.d.Team.(core.TeamReplicas)
	return ok && tr.Replicates(project)
}

// linkRow is project's row whether or not it is hosted here (delete and the
// hold on waits need it); everything else uses link.
func (s *Service) linkRow(ctx context.Context, project string) (link, error) {
	l, err := scanLink(s.db().QueryRowContext(ctx, `SELECT `+linkCols+` FROM ghlink_links WHERE project = ?`, project))
	if errors.Is(err, sql.ErrNoRows) {
		return l, api.NotFound("github link", project)
	}
	return l, err
}

// link is project's link, or NotFound (also for a project not hosted here).
func (s *Service) link(ctx context.Context, project string) (link, error) {
	if !s.hosted(project) {
		return link{}, api.NotFound("github link", project)
	}
	return s.linkRow(ctx, project)
}

func (s *Service) links(ctx context.Context) ([]link, error) {
	rows, err := s.db().QueryContext(ctx, `SELECT `+linkCols+` FROM ghlink_links ORDER BY project`)
	if err != nil {
		return nil, err
	}
	var out []link
	var moved []string
	for rows.Next() {
		l, err := scanLink(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		switch {
		case s.replicated(l.Project):
			moved = append(moved, l.Project)
		case s.hosted(l.Project):
			out = append(out, l)
		}
	}
	err = rows.Err()
	rows.Close()
	for _, p := range moved {
		s.reap(ctx, p)
	}
	return out, err
}

// sources is the tracker sources bare ids are looked up in: the configured
// ones, else every tracker prefix of the project.
func (s *Service) sources(l link) []string {
	if len(l.Sources) > 0 {
		return l.Sources
	}
	f, ok := s.d.Projects.File(l.Project)
	if !ok {
		return nil
	}
	var out []string
	for _, t := range orchcore.ProjectTrackers(f) {
		out = append(out, t.Prefix)
	}
	return out
}

func (s *Service) prefixes(l link) []string {
	if len(l.Prefixes) > 0 {
		return l.Prefixes
	}
	if slug := s.d.Projects.Slug(l.Project); slug != "" && slug != api.ProjectNone {
		return []string{slug}
	}
	return nil
}

func (s *Service) view(ctx context.Context, l link) orchapi.GitHubLink {
	v := orchapi.GitHubLink{Project: s.d.Projects.Slug(l.Project), Repos: l.Repos, Prefixes: s.prefixes(l), Sources: s.sources(l),
		OnOpen: l.OnOpen, OnMerge: l.OnMerge, OnClose: l.OnClose, PollSeconds: l.PollSeconds, Bin: l.Bin, Host: l.Host,
		TokenFile: l.TokenFile, Identity: "human:" + Identity, Created: store.FromMillis(l.Created),
		LastPoll: store.NullMillis(l.LastPoll), LastEvent: store.NullMillis(l.LastEvent), LastError: l.LastError,
		Receiver: ReceiverPath, Match: matchTexts(l), Gate: l.Gate}
	_ = s.db().QueryRowContext(ctx, `SELECT count(*) FROM ghlink_inbox WHERE project = ?`, l.Project).Scan(&v.Pending)
	_ = s.db().QueryRowContext(ctx, `SELECT count(*) FROM ghlink_waits WHERE project = ?`, l.Project).Scan(&v.Waiting)
	if l.Gate != "" {
		if d, err := s.gateFor(l).Describe(ctx); err == nil {
			v.GateName = d.Name + " " + d.Version
		} else {
			v.GateError = errText(err)
		}
	}
	return v
}

// matchTexts is the link's --match, defaulted.
func matchTexts(l link) []string {
	if len(l.Match) == 0 {
		return []string{MatchTitle, MatchBranch, MatchBody}
	}
	return l.Match
}

// gateFor is the link's gate driver with its environment (SPEC 18.3): PATH,
// HOME, LANG, TERM, TZ, GH_HOST and GH_TOKEN as for polling, this daemon's
// ZM_HOME and ZM_NO_AUTOSTART=1. Never the daemon's whole environment.
func (s *Service) gateFor(l link) *gate.Process {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.gates[l.Gate]
	if g == nil {
		g = &gate.Process{Path: l.Gate}
		s.gates[l.Gate] = g
	}
	g.SetEnv(s.gateEnv(l))
	return g
}

func (s *Service) gateEnv(l link) []string {
	env := []string{orchapi.EnvHome + "=" + s.d.Config.Home, "ZM_NO_AUTOSTART=1"}
	for _, k := range []string{"PATH", "HOME", "LANG", "TERM", "TZ"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	if l.Host != "" {
		env = append(env, "GH_HOST="+l.Host)
	}
	if l.TokenFile != "" {
		if v, err := secrets.Get(context.Background(), secrets.File(l.TokenFile)); err == nil {
			env = append(env, "GH_TOKEN="+v.Reveal())
		}
	}
	return env
}

// tokenPath is where the linker's bearer token for project lives (0600).
func (s *Service) tokenPath(project string) string {
	return filepath.Join(s.d.Config.Home, "secrets", "ghlink", project+".token")
}

func (s *Service) token(project string) (string, error) {
	v, err := secrets.Get(context.Background(), secrets.File(s.tokenPath(project)))
	return v.Reveal(), err
}

func writeSecretFile(path, secret string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(secret+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}$`)

// NormRepo validates owner/name (a github.com URL is accepted) and lower-cases it.
func NormRepo(r string) (string, bool) {
	r = strings.TrimSpace(r)
	r = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(r, "https://"), "github.com/"), ".git")
	r = strings.Trim(r, "/")
	if !repoRe.MatchString(r) || strings.Contains(r, "..") || strings.HasPrefix(r, ".") {
		return "", false
	}
	return strings.ToLower(r), true
}

func hasRepo(l link, repo string) bool { return slices.Contains(l.Repos, strings.ToLower(repo)) }
