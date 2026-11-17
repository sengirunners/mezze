// Package issues is the native issues module (SPEC "Native issues"): zm's
// own issue tracker, at parity with how agents use beads. Issues live in the
// Store brick (internal/issues/store; SQLite driver by default, one file per
// project), are served on /v0/issues with zm's authz (tokens, roles, areas),
// and every change is an event (dev.zm.issue.*) for the web UI and webhooks
// to consume. cmd/bd-zm is the bd-compatible door.
//
// Authz: an issue's area comes from the project's area rules (work globs on
// its id, else labels), else the area it was filed in. Reads need read on
// that area; writes need write there and a role above guest (guests are
// read-only on issues unless the project sets guest_issues). A write that moves an issue to another area (a
// label) needs write on both. Hidden issues answer exactly like missing
// ones, and edges to them are neither shown nor counted.
package issues

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/issues/store/sqlite"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
)

// Service is the issues module.
type Service struct {
	d   *core.Deps
	st  istore.Store
	now func() time.Time

	mu       sync.Mutex
	loaded   bool
	prefixes map[string]string // prefix -> project ULID
	held     map[string]string // project -> why its writes are held (a move)
	// gates: per project, every write holds its read side from the held
	// check through its commit; HoldWrites takes the write side, so writes
	// already past the check drain before a hold returns.
	gates map[string]*sync.RWMutex
	// pendingScrub: redactions whose other copies are not scrubbed yet
	// (project\x00issue\x00comment id -> texts), in memory only, retried by Start.
	pendingScrub map[string]pendingScrub
	recovery     redactionRecovery       // owned by the retry worker
	redactCut    func(step string) error // test hook: cuts a redaction off at a step (nil in the daemon)

	stop context.CancelFunc
	wg   sync.WaitGroup
}

var (
	_ core.Module  = (*Service)(nil)
	_ core.Starter = (*Service)(nil)
	_ core.Closer  = (*Service)(nil)
)

// New returns the module with the SQLite store under $ZM_HOME.
func New(d *core.Deps) *Service {
	return NewWithStore(d, sqlite.Open(d.Config.Home))
}

// StoreMigrations are this module's tables in the daemon's store
// (core.Deps.AddMigrations), apart from issues.db's (Migrations); NewWithStore
// adds them.
var StoreMigrations = slices.Concat(redactMigrations, redactionStopsMigrations, redactionAckMigrations)

// NewWithStore uses another Store driver.
func NewWithStore(d *core.Deps, st istore.Store) *Service {
	d.AddMigrations(StoreMigrations...)
	return &Service{d: d, st: st, now: time.Now, prefixes: map[string]string{}, held: map[string]string{}, gates: map[string]*sync.RWMutex{}, pendingScrub: map[string]pendingScrub{}}
}

// Store is the module's store (tests).
func (s *Service) Store() istore.Store { return s.st }

// Start watches area-rule edits: issues move between areas when the rules do.
// Every 30 s it also puts unleased claims back to open (releaseUnleased).
func (s *Service) Start(ctx context.Context) error {
	ctx, s.stop = context.WithCancel(ctx)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		s.recoverRedactions(ctx)
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.retryPendingScrubs(ctx)
				s.recoverRedactions(ctx)
				s.releaseUnleased(ctx, unleasedGrace)
			}
		}
	}()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		t := time.NewTicker(sweepEvery)
		defer t.Stop()
		for {
			s.sweepAttachments(ctx)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	ch, cancel := s.d.Events.Subscribe(s.d.System(), events.Filter{Types: []string{"dev.zm.project."}}, 256)
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer cancel()
		for {
			select {
			case <-ctx.Done():
				return
			case e, ok := <-ch:
				if !ok {
					// Overflowed: re-check every project once, then resubscribe.
					s.reareaAll(ctx)
					ch, cancel = s.d.Events.Subscribe(s.d.System(), events.Filter{Types: []string{"dev.zm.project."}}, 256)
					continue
				}
				if e.Type == api.EventProjectCreated {
					continue
				}
				s.rearea(ctx, e.Project)
			}
		}
	}()
	s.watchEnded(ctx)
	return nil
}

// Close stops the watcher and closes the store.
func (s *Service) Close() error {
	if s.stop != nil {
		s.stop()
	}
	s.wg.Wait()
	return s.st.Close()
}

func (s *Service) warn(msg string, args ...any) {
	if s.d.Log != nil {
		s.d.Log.Warn("issues: "+msg, args...)
	}
}

func (s *Service) reareaAll(ctx context.Context) {
	metas, err := s.st.Existing(ctx)
	if err != nil {
		s.warn("list stores", "err", err)
		return
	}
	for _, m := range metas {
		s.rearea(ctx, m.Project)
	}
}

func (s *Service) rearea(ctx context.Context, project string) {
	ps, err := s.st.Project(ctx, project, false)
	if err != nil {
		return
	}
	moved, err := ps.Rearea(ctx, s.areaFunc(project))
	if err != nil {
		s.warn("rearea", "project", project, "err", err)
		return
	}
	for _, m := range moved {
		s.publishChange(ctx, project, m.Issue, m.Change, false)
	}
}

// ---- projects and prefixes ----

func (s *Service) loadPrefixes(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loaded {
		return nil
	}
	metas, err := s.st.Existing(ctx)
	if err != nil {
		return err
	}
	for _, m := range metas {
		if m.Prefix != "" {
			s.prefixes[m.Prefix] = m.Project
		}
	}
	s.loaded = true
	return nil
}

// projectOf is the project whose prefix id carries (the longest match).
func (s *Service) projectOf(ctx context.Context, id string) (string, error) {
	if err := s.loadPrefixes(ctx); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	best, proj := "", ""
	for pre, p := range s.prefixes {
		if strings.HasPrefix(id, pre+"-") && len(pre) > len(best) {
			best, proj = pre, p
		}
	}
	return proj, nil
}

var prefixRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,31}(-[a-z0-9_]+)*$`)

// setPrefix gives project its issue prefix (only while it has no issues,
// unless it is the same).
func (s *Service) setPrefix(ctx context.Context, project, prefix string, ps istore.ProjectStore) error {
	if !prefixRE.MatchString(prefix) {
		return api.Errorf(api.CodeBadRequest, "an issue prefix is lowercase letters, digits, _ and -, like acme (not %q)", prefix)
	}
	if err := s.loadPrefixes(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.prefixes[prefix]; ok && p != project {
		return api.Errorf(api.CodeConflict, "issue prefix %q is taken on this daemon", prefix)
	}
	cur, err := ps.Prefix(ctx)
	if err != nil {
		return err
	}
	if cur == prefix {
		return nil
	}
	if cur != "" {
		n, err := ps.Counts(ctx)
		if err != nil {
			return err
		}
		if n["issues"] > 0 {
			return api.Errorf(api.CodeConflict, "this project's issues already use prefix %q", cur)
		}
		delete(s.prefixes, cur)
	}
	if err := ps.SetPrefix(ctx, prefix); err != nil {
		return err
	}
	s.prefixes[prefix] = project
	return nil
}

// defaultPrefix is the project's slug made into a prefix.
func defaultPrefix(slug string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(slug) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r == '-' || r == '.' || r == ' ':
			b.WriteByte('_')
		}
	}
	p := strings.Trim(b.String(), "_")
	if p == "" {
		p = "zm"
	}
	if len(p) > 32 {
		p = p[:32]
	}
	return p
}

// writeStore is project's store for a write, created with the default
// prefix on the project's first issue.
func (s *Service) writeStore(ctx context.Context, p api.ProjectRef) (istore.ProjectStore, error) {
	ps, err := s.st.Project(ctx, p.ID, true)
	if err != nil {
		return nil, err
	}
	pre, err := ps.Prefix(ctx)
	if err != nil {
		return nil, err
	}
	if pre == "" {
		if err := s.setPrefix(ctx, p.ID, defaultPrefix(p.Slug), ps); err != nil {
			return nil, err
		}
	}
	return ps, nil
}

// ---- authz ----

// vis is what sc may read in project.
func vis(sc authz.Scope, project string) istore.Areas {
	whole, areas := sc.Areas(project)
	return istore.Areas{All: whole, Names: areas}
}

// canWrite: write on the area and a role above guest, or a guest where the
// project sets guest_issues. Unreadable is not_found.
func (s *Service) canWrite(sub *authz.Subject, project, area, id string) error {
	o := authz.Object{Project: project, Area: area}
	if err := sub.Check(authz.Write, o, "issue", id); err != nil {
		return err
	}
	if sub.Scope.Relation(o) == authz.Guest && !s.guestIssues(project) {
		return api.Errorf(api.CodeForbidden, "guests are read-only on issues")
	}
	return nil
}

// guestIssues reports whether project lets its guests write native issues.
func (s *Service) guestIssues(project string) bool {
	if s.d.Projects == nil {
		return false
	}
	f, ok := s.d.Projects.File(project)
	return ok && orchcore.GuestIssues(f)
}

func (s *Service) areaFunc(project string) istore.AreaFunc {
	return func(id string, labels []string) string {
		if s.d.Projects == nil {
			return ""
		}
		return orchcore.AreaForItem(s.d.Projects, project, id, labels)
	}
}

func actorOf(sub *authz.Subject) istore.Actor {
	name := sub.Identity.Name
	if name == "" {
		name = sub.Address
	}
	return istore.Actor{Name: name, Address: sub.Address, Identity: sub.Identity.ID}
}

// located is an issue a caller may read.
type located struct {
	project string
	ps      istore.ProjectStore
	vis     istore.Areas
	id      string
	from    string // the id asked for, when it moved and this is what it became
}

// locate finds id in the caller's read scope: hidden and missing answer the
// same not_found.
func (s *Service) locate(ctx context.Context, sc authz.Scope, id string) (located, error) {
	nf := api.NotFound("issue", id)
	project, err := s.projectOf(ctx, id)
	if err != nil {
		return located{}, err
	}
	if project == "" || !sc.SeesProject(project) {
		return located{}, nf
	}
	ps, err := s.st.Project(ctx, project, false)
	if errors.Is(err, istore.ErrNotFound) {
		return located{}, nf
	}
	if err != nil {
		return located{}, err
	}
	l := located{project: project, ps: ps, vis: vis(sc, project), id: id}
	if _, err := ps.Get(ctx, id, l.vis); err != nil {
		return located{}, s.storeErr(err, id)
	}
	return l, nil
}

func (s *Service) storeErr(err error, id string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, istore.ErrNotFound):
		return api.NotFound("issue", id)
	case errors.Is(err, istore.ErrConflict):
		return api.Errorf(api.CodeConflict, "%v", err)
	}
	var ae *api.Error
	if errors.As(err, &ae) {
		return ae
	}
	s.warn("store", "err", err)
	return api.Errorf(api.CodeInternal, "internal error")
}

// ---- comments: edit, delete, what readers see ----

// moderates says whether sub moderates comments on an issue in area: an
// admin of the area, or the machine owner on this machine's socket.
func moderates(sub *authz.Subject, project, area string) bool {
	o := authz.Object{Project: project, Area: area}
	return sub.Can(authz.Admin, o) || (sub.Via == authz.ViaLocal && !sub.Remote && sub.Can(authz.Operate, o))
}

// showComment is a comment as sub sees it on an issue in area: addresses
// redacted; a tombstone's text and deleter only for moderators.
func (s *Service) showComment(ctx context.Context, sub *authz.Subject, project, area string, c *orchapi.IssueComment) {
	s.redactComment(ctx, sub, c)
	if !c.Deleted {
		c.DeletedBy = ""
		return
	}
	if moderates(sub, project, area) {
		c.DeletedBy = s.d.RedactFor(ctx, sub, c.DeletedBy)
		return
	}
	c.Text, c.DeletedBy = "", ""
}

// showHistory hides what a deleted comment said (its edits' texts) and who
// moderated, from everyone but the area's moderators.
func (s *Service) showHistory(ctx context.Context, sub *authz.Subject, l located, area string, hs []orchapi.IssueChange) {
	if moderates(sub, l.project, area) {
		return
	}
	cs, err := l.ps.Comments(ctx, l.id, l.vis)
	if err != nil {
		return
	}
	deleted := map[string]bool{}
	for _, c := range cs {
		if c.Deleted {
			deleted[c.ID] = true
		}
	}
	for i := range hs {
		h := &hs[i]
		if h.Kind == "comment_edited" && deleted[h.Comment] {
			delete(h.Fields, "text")
		}
		if _, mod := h.Fields["moderator"]; mod {
			h.Actor, h.ActorAddress = "a moderator", ""
		}
	}
}

// commentAnswer is an edited or deleted comment as its writer sees it.
func (s *Service) commentAnswer(ctx context.Context, sub *authz.Subject, l located, c *orchapi.IssueComment) (any, error) {
	if c == nil {
		return nil, api.Errorf(api.CodeInternal, "internal error")
	}
	it, err := l.ps.Get(ctx, l.id, l.vis)
	if err != nil {
		return nil, s.storeErr(err, l.id)
	}
	out := *c
	s.showComment(ctx, sub, l.project, it.Area, &out)
	return out, nil
}

// ---- redaction ----

func (s *Service) redactIssue(ctx context.Context, sub *authz.Subject, it *orchapi.Issue) {
	if it.ClaimedBy != "" {
		it.ClaimedBy = s.d.RedactFor(ctx, sub, it.ClaimedBy)
	}
	if it.CreatedByAddress != "" {
		it.CreatedByAddress = s.d.RedactFor(ctx, sub, it.CreatedByAddress)
	}
}

func (s *Service) redactComment(ctx context.Context, sub *authz.Subject, c *orchapi.IssueComment) {
	if c.AuthorAddress != "" {
		c.AuthorAddress = s.d.RedactFor(ctx, sub, c.AuthorAddress)
	}
}

// ---- reads ----

func (s *Service) get(ctx context.Context, sub *authz.Subject, l located) (orchapi.IssueDetail, error) {
	d, err := l.ps.Detail(ctx, l.id, l.vis)
	if err != nil {
		return d, s.storeErr(err, l.id)
	}
	d.MovedFrom = l.from
	if err := s.crossEdges(ctx, sub, &d); err != nil {
		return d, err
	}
	s.redactIssue(ctx, sub, &d.Issue)
	for i := range d.DependsOn {
		s.redactIssue(ctx, sub, &d.DependsOn[i].Issue)
	}
	for i := range d.Dependents {
		s.redactIssue(ctx, sub, &d.Dependents[i].Issue)
	}
	for i := range d.Comments {
		s.showComment(ctx, sub, l.project, d.Issue.Area, &d.Comments[i])
	}
	one := []orchapi.Issue{d.Issue}
	s.fillAttachments(ctx, sub, l.project, one)
	d.Attachments = one[0].Attachments
	return d, nil
}

// readStores are the stores of every project in sc that has one.
func (s *Service) readStores(ctx context.Context, sc authz.Scope) ([]located, error) {
	metas, err := s.st.Existing(ctx)
	if err != nil {
		return nil, err
	}
	var out []located
	for _, m := range metas {
		if !sc.SeesProject(m.Project) {
			continue
		}
		ps, err := s.st.Project(ctx, m.Project, false)
		if err != nil {
			return nil, err
		}
		out = append(out, located{project: m.Project, ps: ps, vis: vis(sc, m.Project)})
	}
	return out, nil
}

func (s *Service) list(ctx context.Context, sub *authz.Subject, sc authz.Scope, q orchapi.IssueQuery) (orchapi.IssueList, error) {
	if q.Limit < 0 {
		return orchapi.IssueList{}, api.Errorf(api.CodeBadRequest, "bad limit")
	}
	out := orchapi.IssueList{Issues: []orchapi.Issue{}}
	next := ""
	if q.Parent != "" {
		// Children of an issue the caller cannot read: none (not an oracle).
		if _, err := s.locate(ctx, sc, q.Parent); err != nil {
			return out, nil
		}
	}
	stores, err := s.readStores(ctx, sc)
	if err != nil {
		return orchapi.IssueList{}, s.storeErr(err, "")
	}
	now := s.now()
	for _, l := range stores {
		r, err := l.ps.List(ctx, q, l.vis, now)
		if err != nil {
			return orchapi.IssueList{}, s.storeErr(err, "")
		}
		out.Issues = append(out.Issues, r.Issues...)
		out.Total += r.Total
		out.More = out.More || r.More
		out.Unranked = out.Unranked || r.Unranked
		next = r.Next
	}
	if len(stores) > 1 {
		sortIssues(out.Issues, q.Sort, q.Reverse)
		limit := q.Limit
		if limit == 0 && !q.All {
			limit = 50
		}
		if limit > 0 && len(out.Issues) > limit {
			out.Issues, out.More = out.Issues[:limit], true
		}
		out.Next = ""
		if out.More && len(out.Issues) > 0 && (q.Sort == "created" || q.Sort == "updated") {
			last := out.Issues[len(out.Issues)-1]
			t := last.CreatedAt
			if q.Sort == "updated" {
				t = last.UpdatedAt
			}
			out.Next = istore.EncodeKey(t.UnixMilli(), last.ID)
		}
	} else if len(stores) == 1 {
		out.Next = next
	}
	for i := range out.Issues {
		if len(out.Issues[i].Dependencies) > 0 {
			deps := out.Issues[i].Dependencies[:0]
			for _, dep := range out.Issues[i].Dependencies {
				if !dep.External {
					deps = append(deps, dep)
					continue
				}
				visible, err := s.visibleEdge(ctx, sub, dep.DependsOnID)
				if err != nil {
					return orchapi.IssueList{}, err
				}
				if visible {
					deps = append(deps, dep)
				}
			}
			out.Issues[i].Dependencies = deps
		}
		s.redactIssue(ctx, sub, &out.Issues[i])
		for j := range out.Issues[i].Comments {
			s.showComment(ctx, sub, out.Issues[i].Project, out.Issues[i].Area, &out.Issues[i].Comments[j])
		}
	}
	return out, nil
}

// sortIssues orders a merged list like the store does for one project.
func sortIssues(its []orchapi.Issue, by string, reverse bool) {
	less := func(a, b orchapi.Issue) bool {
		switch by {
		case "rank": // best first (scores of different projects are close enough to merge)
			if a.Score != b.Score {
				return a.Score > b.Score
			}
		case "created": // ties newest id first, like the store's keyset order
			if a.CreatedAt.UnixMilli() != b.CreatedAt.UnixMilli() {
				return a.CreatedAt.After(b.CreatedAt)
			}
			return a.ID > b.ID
		case "updated":
			if a.UpdatedAt.UnixMilli() != b.UpdatedAt.UnixMilli() {
				return a.UpdatedAt.After(b.UpdatedAt)
			}
			return a.ID > b.ID
		case "oldest":
			if !a.CreatedAt.Equal(b.CreatedAt) {
				return a.CreatedAt.Before(b.CreatedAt)
			}
		case "closed":
			ac, bc := a.ClosedAt, b.ClosedAt
			switch {
			case ac == nil && bc != nil:
				return false
			case ac != nil && bc == nil:
				return true
			case ac != nil && !ac.Equal(*bc):
				return ac.After(*bc)
			}
		case "status", "type", "assignee":
			x, y := a.Status, b.Status
			if by == "type" {
				x, y = a.IssueType, b.IssueType
			} else if by == "assignee" {
				x, y = a.Assignee, b.Assignee
			}
			if x != y {
				return x < y
			}
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
		case "id":
		case "title":
			if x, y := strings.ToLower(a.Title), strings.ToLower(b.Title); x != y {
				return x < y
			}
		default:
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			if !a.CreatedAt.Equal(b.CreatedAt) {
				return a.CreatedAt.After(b.CreatedAt)
			}
		}
		return a.ID < b.ID
	}
	sort.SliceStable(its, func(i, j int) bool {
		if reverse && by != "rank" { // a ranking is best first, as one project's store returns it
			return less(its[j], its[i])
		}
		return less(its[i], its[j])
	})
}

func (s *Service) stats(ctx context.Context, sc authz.Scope) (orchapi.IssueStats, error) {
	stores, err := s.readStores(ctx, sc)
	if err != nil {
		return orchapi.IssueStats{}, s.storeErr(err, "")
	}
	var out orchapi.IssueStats
	for _, l := range stores {
		st, err := l.ps.Stats(ctx, l.vis, s.now())
		if err != nil {
			return out, s.storeErr(err, "")
		}
		out.TotalIssues += st.TotalIssues
		out.OpenIssues += st.OpenIssues
		out.InProgressIssues += st.InProgressIssues
		out.ClosedIssues += st.ClosedIssues
		out.BlockedIssues += st.BlockedIssues
		out.DeferredIssues += st.DeferredIssues
		out.ReadyIssues += st.ReadyIssues
		out.PinnedIssues += st.PinnedIssues
	}
	return out, nil
}

func (s *Service) projects(ctx context.Context, sc authz.Scope) (orchapi.IssueProjects, error) {
	metas, err := s.st.Existing(ctx)
	if err != nil {
		return orchapi.IssueProjects{}, s.storeErr(err, "")
	}
	out := orchapi.IssueProjects{Projects: []orchapi.IssueProjectInfo{}}
	for _, m := range metas {
		if !sc.SeesProject(m.Project) {
			continue
		}
		out.Projects = append(out.Projects, orchapi.IssueProjectInfo{Project: m.Project, Slug: s.d.Projects.Slug(m.Project), Prefix: m.Prefix})
	}
	return out, nil
}

// ---- writes ----

// mutate runs m on l's project as sub, publishes its events and returns
// the issue as sub sees it.
func (s *Service) mutate(ctx context.Context, sub *authz.Subject, project string, ps istore.ProjectStore, m istore.Mutation) (istore.Result, orchapi.Issue, error) {
	if err := cleanMutation(&m); err != nil {
		return istore.Result{}, orchapi.Issue{}, err
	}
	g := s.gate(project)
	g.RLock()
	gated := true
	defer func() {
		if gated {
			g.RUnlock()
		}
	}()
	s.mu.Lock()
	why := s.held[project]
	s.mu.Unlock()
	if why != "" {
		return istore.Result{}, orchapi.Issue{}, api.Errorf(api.CodeConflict, "this project's issues take no writes while %s; try again in a moment", why)
	}
	m.Actor, m.At = actorOf(sub), s.now()
	m.AreaOf = s.areaFunc(project)
	id := m.ID
	m.Authorize = func(before, after string) error {
		if err := s.canWrite(sub, project, before, id); err != nil {
			return err
		}
		if after != before {
			if err := s.canWrite(sub, project, after, id); err != nil {
				return api.Errorf(api.CodeForbidden, "this change would move the issue to an area you cannot write in")
			}
		}
		return nil
	}
	m.CanRead = func(area string) bool { return sub.Can(authz.Read, authz.Object{Project: project, Area: area}) }
	m.Moderate = func(area string) bool { return moderates(sub, project, area) }
	if writeHooks.beforeApply != nil {
		writeHooks.beforeApply(project)
	}
	r, err := ps.Apply(ctx, m)
	gated = false
	g.RUnlock() // the rest (events, the lease) may write again
	if err != nil {
		return r, orchapi.Issue{}, s.storeErr(err, id)
	}
	for _, ch := range r.Changes {
		s.publishChange(ctx, project, r.Issue, ch, m.Op == istore.OpCreate)
	}
	for _, t := range r.Touched {
		s.publishIssue(ctx, project, t)
	}
	s.endLease(ctx, project, r)
	if !r.Noop {
		s.unorphan(ctx, project, ps, m, r.Issue)
	}
	view, err := ps.Get(ctx, r.Issue.ID, vis(sub.Scope, project))
	if errors.Is(err, istore.ErrNotFound) {
		view = orchapi.Issue{ID: r.Issue.ID, Project: project} // moved out of the caller's sight (cannot happen: Authorize)
	} else if err != nil {
		return r, view, s.storeErr(err, id)
	}
	s.redactIssue(ctx, sub, &view)
	if r.Comment != nil {
		s.redactComment(ctx, sub, r.Comment)
	}
	return r, view, nil
}

// gate is project's write gate (Service.gates).
func (s *Service) gate(project string) *sync.RWMutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.gates[project]
	if g == nil {
		g = &sync.RWMutex{}
		s.gates[project] = g
	}
	return g
}

// writeHooks are test hooks (nil in the daemon): one runs between a
// write's held check and its commit.
var writeHooks struct{ beforeApply func(project string) }

// eventIssue is an issue as events carry it: no edges and no parent (the
// other end may be in an area some readers of this one cannot see; readers
// fetch the issue).
func eventIssue(it orchapi.Issue) *orchapi.Issue {
	it.Dependencies, it.Parent = nil, ""
	return &it
}

// refFields name other issues in a change; events leave them out and the
// history route hides the ones the reader cannot see.
var refFields = []string{"depends_on", "parent"}

func eventChange(ch orchapi.IssueChange) *orchapi.IssueChange {
	if len(ch.Fields) > 0 {
		f := make(map[string]json.RawMessage, len(ch.Fields))
		for k, v := range ch.Fields {
			if !slices.Contains(refFields, k) && !(ch.Kind == "moved" && k == "id") {
				f[k] = v
			}
		}
		ch.Fields = f
	}
	return &ch
}

func mentions(it orchapi.Issue, ch *orchapi.IssueChange) []string {
	var m []string
	if it.CreatedByAddress != "" {
		m = append(m, it.CreatedByAddress)
	}
	if it.ClaimedBy != "" {
		m = append(m, it.ClaimedBy)
	}
	if ch != nil && ch.ActorAddress != "" {
		m = append(m, ch.ActorAddress)
	}
	return m
}

func (s *Service) publish(ctx context.Context, typ, subject, project, area string, data any, mentions []string) {
	if _, err := s.d.Events.Publish(context.WithoutCancel(ctx), events.Input{Type: typ, Subject: subject, Project: project,
		Area: area, Data: data, Mentions: mentions}); err != nil {
		s.warn("publish", "type", typ, "err", err)
	}
}

// publishChange publishes one history row: created/commented/updated in
// the issue's area, plus moved {id, gone} in the area it left.
func (s *Service) publishChange(ctx context.Context, project string, it orchapi.Issue, ch orchapi.IssueChange, created bool) {
	typ := orchapi.EventIssueUpdated
	switch {
	case created:
		typ = orchapi.EventIssueCreated
	case ch.Kind == "commented":
		typ = orchapi.EventIssueCommented
	case ch.Kind == "comment_edited":
		typ = orchapi.EventIssueCommentEdited
	case ch.Kind == "comment_deleted":
		typ = orchapi.EventIssueCommentDeleted
	case ch.Kind == "comment_redacted":
		typ = orchapi.EventIssueCommentRedacted
	}
	c := eventChange(ch)
	if _, mod := ch.Fields["moderator"]; mod {
		// A moderator's delete: readers learn that it happened, not who did it.
		c.Actor, c.ActorAddress = "a moderator", ""
	}
	s.publish(ctx, typ, it.ID, project, ch.AreaAfter, orchapi.IssueEvent{Issue: eventIssue(it), Change: c}, mentions(it, c))
	if ch.AreaBefore != ch.AreaAfter {
		s.publish(ctx, orchapi.EventIssueMoved, it.ID, project, ch.AreaBefore, orchapi.IssueEvent{ID: it.ID, Gone: true}, nil)
	}
}

func (s *Service) publishIssue(ctx context.Context, project string, it orchapi.Issue) {
	s.publish(ctx, orchapi.EventIssueUpdated, it.ID, project, it.Area, orchapi.IssueEvent{Issue: eventIssue(it)}, mentions(it, nil))
}

func (s *Service) create(ctx context.Context, sub *authz.Subject, req orchapi.NewIssueRequest) (orchapi.Issue, error) {
	t, err := s.d.WriteTarget(ctx, req.Area, authz.Write)
	if err != nil {
		return orchapi.Issue{}, err
	}
	if err := s.canWrite(sub, t.Project.ID, t.Area, ""); err != nil {
		return orchapi.Issue{}, err
	}
	ps, err := s.writeStore(ctx, t.Project)
	if err != nil {
		return orchapi.Issue{}, s.storeErr(err, "")
	}
	req.Area = t.Area
	_, view, err := s.mutate(ctx, sub, t.Project.ID, ps, istore.Mutation{Op: istore.OpCreate, ID: req.ID, Create: &req})
	return view, err
}

func (s *Service) write(ctx context.Context, sub *authz.Subject, l located, m istore.Mutation) (istore.Result, orchapi.Issue, error) {
	m.ID = l.id
	if err := s.fenceCheck(ctx, sub, l, m); err != nil {
		return istore.Result{}, orchapi.Issue{}, err
	}
	return s.mutate(ctx, sub, l.project, l.ps, m)
}

// setup gives the resolved project its issue prefix (admin).
func (s *Service) setup(ctx context.Context, prefix string) (orchapi.IssueProjectInfo, error) {
	t, err := s.d.WriteTarget(ctx, "", authz.Admin)
	if err != nil {
		return orchapi.IssueProjectInfo{}, err
	}
	ps, err := s.st.Project(ctx, t.Project.ID, true)
	if err != nil {
		return orchapi.IssueProjectInfo{}, s.storeErr(err, "")
	}
	if err := s.setPrefix(ctx, t.Project.ID, strings.TrimSpace(prefix), ps); err != nil {
		return orchapi.IssueProjectInfo{}, err
	}
	return orchapi.IssueProjectInfo{Project: t.Project.ID, Slug: t.Project.Slug, Prefix: strings.TrimSpace(prefix)}, nil
}

// redactRefs hides issue ids in a history row's reference fields that the
// reader cannot see ("type:id" or "id", old and new).
func (s *Service) redactRefs(ctx context.Context, l located, ch *orchapi.IssueChange) {
	for _, k := range refFields {
		raw, ok := ch.Fields[k]
		if !ok {
			continue
		}
		var pair []*string
		if json.Unmarshal(raw, &pair) != nil {
			delete(ch.Fields, k)
			continue
		}
		for _, v := range pair {
			if v == nil || *v == "" {
				continue
			}
			ref := *v
			if typ, id, ok := strings.Cut(ref, ":"); ok && typ != "external" {
				ref = id
			}
			if strings.HasPrefix(ref, "external:") {
				continue
			}
			if _, err := l.ps.Get(ctx, ref, l.vis); err != nil {
				*v = events.Someone
			}
		}
		b, _ := json.Marshal(pair)
		ch.Fields[k] = b
	}
}

// searchComments is comments across every native store the caller reads,
// oldest id first (ids are ULIDs: one order across projects).
func (s *Service) searchComments(ctx context.Context, sub *authz.Subject, sc authz.Scope, q orchapi.CommentQuery) (orchapi.IssueCommentList, error) {
	switch {
	case q.Limit <= 0:
		q.Limit = 100
	case q.Limit > 1000:
		q.Limit = 1000
	}
	out := orchapi.IssueCommentList{Comments: []orchapi.IssueComment{}}
	if q.Author != "" && q.Author != sub.Address && !s.d.Visible(ctx, sub.Scope, q.Author) {
		// An author the caller may not see: answer as if they wrote nothing,
		// so the filter cannot name who "someone" is.
		return out, nil
	}
	stores, err := s.readStores(ctx, sc)
	if err != nil {
		return orchapi.IssueCommentList{}, s.storeErr(err, "")
	}
	per := q
	per.Limit = q.Limit + 1 // one more, to know whether there is a next page
	var all []orchapi.IssueComment
	for _, l := range stores {
		cs, err := l.ps.SearchComments(ctx, per, l.vis)
		if err != nil {
			return orchapi.IssueCommentList{}, s.storeErr(err, "")
		}
		for i := range cs {
			cs[i].Project = l.project
		}
		all = append(all, cs...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID < all[j].ID })
	if len(all) > q.Limit {
		all = all[:q.Limit]
		out.Next = all[len(all)-1].ID
	}
	for i := range all {
		s.redactComment(ctx, sub, &all[i])
	}
	out.Comments = append(out.Comments, all...)
	return out, nil
}

// labels is every label on an issue the caller reads (areas: only there).
func (s *Service) labels(ctx context.Context, sc authz.Scope, areas []string) ([]string, error) {
	stores, err := s.readStores(ctx, sc)
	if err != nil {
		return nil, s.storeErr(err, "")
	}
	seen := map[string]bool{}
	out := []string{}
	for _, l := range stores {
		ls, err := l.ps.Labels(ctx, l.vis, areas)
		if err != nil {
			return nil, s.storeErr(err, "")
		}
		for _, x := range ls {
			if !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// Migrations lists the issue store migrations this build knows (the swap
// preflight compares each issues.db with them).
func Migrations() []string { return sqlite.Migrations() }
