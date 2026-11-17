package issues

// Native issues as a work source (SPEC "Native issues"): a project's
// [[trackers]] entry with adapter "native" is served by this module in
// process, through the section 11 verbs, so `zm work`, leases, the mirror,
// inflight, MCP and the web UI see native issues like any tracker's items.
// A binding's prefix names its work ids (bd:acme-ofoqu), whatever the
// store's own issue prefix.
//
// Reads (list, get, changes) see every issue: the work module scopes what it
// serves from its mirror. Writes act as the caller in the request's context,
// with the native rules (guests read-only, area moves), and the holder or
// author the work module passes as the zm address.

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/ids"
)

// NativePrefix is the native adapter's own id prefix (issue:acme-ofoqu);
// a binding's prefix replaces it in zm work ids.
const NativePrefix = "issue"

var _ orchcore.IssueService = (*Service)(nil)

// NativeDescribe implements orchcore.IssueService.
func (s *Service) NativeDescribe() orchapi.AdapterDescribe {
	return orchapi.AdapterDescribe{Spec: api.AdapterProtocol, Name: orchapi.AdapterNative, IdempotentComments: true, Prefix: NativePrefix, Version: api.Version, Backup: []string{orchapi.BackupSqlite},
		Verbs: []string{orchapi.VerbDescribe, orchapi.VerbList, orchapi.VerbGet, orchapi.VerbClaim, orchapi.VerbRelease, orchapi.VerbComment,
			orchapi.VerbSetStatus, orchapi.VerbChanges}}
}

// nativePage is how many issues one changes page carries.
const nativePage = 1000

// NativeCall implements orchcore.IssueService.
func (s *Service) NativeCall(ctx context.Context, project string, req orchapi.AdapterRequest, out any) error {
	res, err := s.nativeCall(ctx, project, req)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	b, err := json.Marshal(res)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

func (s *Service) nativeCall(ctx context.Context, project string, req orchapi.AdapterRequest) (any, error) {
	if req.Verb == orchapi.VerbClaim || req.Verb == orchapi.VerbRelease {
		// The work module takes or drops the lease itself around these two;
		// every other write (set_status done or open) ends a claim like any
		// native write would.
		ctx = context.WithValue(ctx, fromWorkCtx{}, true)
	}
	if req.Verb == orchapi.VerbDescribe {
		return s.NativeDescribe(), nil
	}
	if !slices.Contains(s.NativeDescribe().Verbs, req.Verb) {
		return nil, api.Errorf(api.CodeNotImplemented, "the native adapter has no verb %q", req.Verb)
	}
	id := strings.TrimPrefix(req.ID, NativePrefix+":")
	ps, err := s.st.Project(ctx, project, false)
	if err != nil {
		if errorsIsNotFound(err) {
			switch req.Verb {
			case orchapi.VerbList:
				return map[string]any{"items": []orchapi.WorkItem{}}, nil
			case orchapi.VerbChanges:
				// No store until the first issue: an empty listing that still
				// ends in a cursor (a sync keeps the one it has).
				var a orchapi.AdapterChangesArgs
				_ = json.Unmarshal(req.Args, &a)
				if a.Since != "" && !ids.Valid(a.Since) {
					return nil, api.Errorf(api.CodeBadRequest, "bad cursor %q", a.Since)
				}
				zero, cursor := 0, a.Since
				if cursor == "" {
					cursor = noChange
				}
				return orchapi.AdapterChanges{Items: []orchapi.WorkItem{}, Count: &zero, Cursor: cursor}, nil
			}
			return nil, api.NotFound("work item", req.ID)
		}
		return nil, s.storeErr(err, id)
	}
	all := istore.Areas{All: true}
	switch req.Verb {
	case orchapi.VerbList:
		var a orchapi.AdapterListArgs
		_ = json.Unmarshal(req.Args, &a)
		q := orchapi.IssueQuery{Status: "all", Limit: a.Limit, Contains: a.Q, ClaimedBy: a.Owner, LabelAny: a.LabelsAny}
		if a.Status != "" {
			q.Status = strings.Join(nativeStatusesOf(a.Status), ",")
		}
		if q.Limit <= 0 {
			q.Limit = 5000
		}
		l, err := ps.List(ctx, q, all, s.now())
		if err != nil {
			return nil, s.storeErr(err, "")
		}
		return map[string]any{"items": s.workItems(ctx, project, l.Issues)}, nil
	case orchapi.VerbGet:
		return s.nativeItem(ctx, ps, id)
	case orchapi.VerbChanges:
		var a orchapi.AdapterChangesArgs
		_ = json.Unmarshal(req.Args, &a)
		return s.nativeChanges(ctx, project, ps, a)
	}
	// Writes: the caller from the request, else the daemon (a team's write-back).
	sub := core.SubjectOf(ctx)
	if sub == nil {
		sub = s.d.System()
	}
	if sub.Via == authz.ViaSystem && req.Actor != "" {
		// The daemon's own call (a team write-back, an expiry): name the zm
		// identity the write is for (SPEC 11 actor), not the machine.
		c := *sub
		c.Identity.Name = req.Actor
		sub = &c
	}
	if _, err := ps.Get(ctx, id, all); err != nil {
		if mv, ok := ps.(istore.Mover); ok && req.Verb == orchapi.VerbRelease && errorsIsNotFound(err) {
			if _, _, moved, _ := mv.MovedTo(ctx, id); moved {
				return orchapi.WorkItem{}, nil // it moved: the lease ends here, the claim went with the issue
			}
		}
		return nil, s.storeErr(err, req.ID)
	}
	l := located{project: project, ps: ps, vis: vis(sub.Scope, project), id: id}
	switch req.Verb {
	case orchapi.VerbClaim:
		var a orchapi.AdapterClaimArgs
		_ = json.Unmarshal(req.Args, &a)
		_, it, err := s.write(ctx, as(sub, a.Holder), l, istore.Mutation{Op: istore.OpClaim, Assignee: core.ClaimLabelOf(ctx)})
		if err != nil {
			return nil, err
		}
		return s.nativeItem(ctx, ps, it.ID)
	case orchapi.VerbRelease:
		var a orchapi.AdapterReleaseArgs
		_ = json.Unmarshal(req.Args, &a)
		cur, err := ps.Get(ctx, id, all)
		if err != nil {
			return nil, s.storeErr(err, req.ID)
		}
		if cur.ClaimedBy != "" && a.Holder != "" && cur.ClaimedBy != a.Holder {
			return nil, api.Errorf(api.CodeConflict, "%s is claimed by someone else", req.ID)
		}
		if cur.ClaimedBy == "" || (cur.ZmStatus != orchapi.WorkClaimed && cur.ZmStatus != orchapi.WorkInReview) {
			// The claim already ended natively (closed, reopened, handed to
			// someone): a lease leaving afterwards changes nothing, so a
			// closed issue keeps its assignee as bd's do.
			return s.nativeItem(ctx, ps, id)
		}
		none := ""
		up := orchapi.UpdateIssueRequest{Assignee: &none}
		if cur.Status == orchapi.IssueInProgress || cur.Status == orchapi.IssueHooked {
			st := orchapi.IssueOpen
			up.Status = &st
		}
		if _, _, err := s.write(ctx, as(sub, a.Holder), l, istore.Mutation{Op: istore.OpUpdate, Update: &up}); err != nil {
			return nil, err
		}
		return s.nativeItem(ctx, ps, id)
	case orchapi.VerbComment:
		var a orchapi.AdapterCommentArgs
		_ = json.Unmarshal(req.Args, &a)
		if _, _, err := s.write(ctx, as(sub, a.Author), l, istore.Mutation{Op: istore.OpComment, Comment: &orchapi.IssueCommentRequest{Text: a.Body, ID: a.DeliveryID}}); err != nil {
			return nil, err
		}
		return map[string]any{}, nil
	case orchapi.VerbSetStatus:
		var a orchapi.AdapterSetStatusArgs
		_ = json.Unmarshal(req.Args, &a)
		if err := s.setZmStatus(ctx, sub, l, a.Status); err != nil {
			return nil, err
		}
		return s.nativeItem(ctx, ps, id)
	}
	return nil, api.Errorf(api.CodeNotImplemented, "the native adapter has no verb %q", req.Verb)
}

func errorsIsNotFound(err error) bool { return err == istore.ErrNotFound }

// as is sub answering as address (the holder or author the work module
// names), when it is one of sub's own (sub's address or a session of it).
func as(sub *authz.Subject, address string) *authz.Subject {
	if address == "" || address == sub.Address {
		return sub
	}
	c := *sub
	c.Address = address
	return &c
}

func (s *Service) nativeItem(ctx context.Context, ps istore.ProjectStore, id string) (orchapi.WorkItem, error) {
	it, err := ps.Get(ctx, id, istore.Areas{All: true})
	if err != nil {
		return orchapi.WorkItem{}, s.storeErr(err, id)
	}
	return s.workItems(ctx, it.Project, []orchapi.Issue{it})[0], nil
}

// nativeStatusesOf are the native statuses behind a zm status.
func nativeStatusesOf(st orchapi.WorkStatus) []string {
	switch st {
	case orchapi.WorkClaimed:
		return []string{orchapi.IssueInProgress, orchapi.IssueHooked}
	case orchapi.WorkInReview:
		return []string{orchapi.IssueInReview}
	case orchapi.WorkDone:
		return []string{orchapi.IssueClosed}
	}
	return []string{orchapi.IssueOpen, orchapi.IssueBlocked, orchapi.IssueDeferred, orchapi.IssuePinned}
}

func (s *Service) setZmStatus(ctx context.Context, sub *authz.Subject, l located, st orchapi.WorkStatus) error {
	cur, err := l.ps.Get(ctx, l.id, istore.Areas{All: true})
	if err != nil {
		return s.storeErr(err, l.id)
	}
	var m istore.Mutation
	switch st {
	case orchapi.WorkDone:
		m = istore.Mutation{Op: istore.OpClose, Close: &orchapi.CloseIssueRequest{}}
	case orchapi.WorkOpen:
		if cur.Status == orchapi.IssueClosed {
			m = istore.Mutation{Op: istore.OpReopen, Reopen: &orchapi.ReopenIssueRequest{}}
		} else {
			o := orchapi.IssueOpen
			m = istore.Mutation{Op: istore.OpUpdate, Update: &orchapi.UpdateIssueRequest{Status: &o}}
		}
	case orchapi.WorkClaimed:
		m = istore.Mutation{Op: istore.OpClaim}
	case orchapi.WorkInReview:
		r := orchapi.IssueInReview
		m = istore.Mutation{Op: istore.OpUpdate, Update: &orchapi.UpdateIssueRequest{Status: &r}}
	default:
		return api.Errorf(api.CodeBadRequest, "unknown status %q", st)
	}
	_, _, err = s.write(ctx, sub, l, m)
	return err
}

// nativeChanges is the mirror's feed: a full listing in pages of ids, then
// what the history says changed after the cursor (a history row id).
// noChange is the cursor of a native store with no history: below every
// history row id (a ULID).
const noChange = "00000000000000000000000000"

func (s *Service) nativeChanges(ctx context.Context, project string, ps istore.ProjectStore, a orchapi.AdapterChangesArgs) (orchapi.AdapterChanges, error) {
	all := istore.Areas{All: true}
	cl, err := ps.List(ctx, orchapi.IssueQuery{Status: "all", Limit: 1, Count: true}, all, s.now()) // what List serves: templates left out
	if err != nil {
		return orchapi.AdapterChanges{}, s.storeErr(err, "")
	}
	count := cl.Total
	if a.Since == "" {
		last := a.Page
		cursor := ""
		if last == "" {
			// The cursor is read before the listing: a change while it runs
			// is sent again by the next incremental sync (re-sending is fine).
			if cursor, err = ps.LastChange(ctx); err != nil {
				return orchapi.AdapterChanges{}, s.storeErr(err, "")
			}
			if cursor == "" {
				// No history yet (no issue made): the lowest history id, so
				// the next sync reads every change from the first.
				cursor = noChange
			}
		} else if c, rest, ok := strings.Cut(last, "|"); ok {
			cursor, last = c, rest
		}
		l, err := ps.List(ctx, orchapi.IssueQuery{Status: "all", Sort: "id", IDAfter: last, Limit: nativePage}, all, s.now())
		if err != nil {
			return orchapi.AdapterChanges{}, s.storeErr(err, "")
		}
		page := l.Issues
		res := orchapi.AdapterChanges{Items: s.workItems(ctx, project, page), Count: &count}
		if len(page) == nativePage && len(page) > 0 {
			res.Next = cursor + "|" + page[len(page)-1].ID
		} else {
			res.Cursor = cursor
		}
		return res, nil
	}
	if !ids.Valid(a.Since) { // a cursor is a history row id
		return orchapi.AdapterChanges{}, api.Errorf(api.CodeBadRequest, "bad cursor %q", a.Since)
	}
	chs, err := ps.Changes(ctx, a.Since, 10000)
	if err != nil {
		return orchapi.AdapterChanges{}, s.storeErr(err, "")
	}
	res := orchapi.AdapterChanges{Items: []orchapi.WorkItem{}, Count: &count, Cursor: a.Since}
	seen := map[string]bool{}
	var ids []string
	for _, c := range chs {
		res.Cursor = c.ID
		if !seen[c.IssueID] {
			seen[c.IssueID] = true
			ids = append(ids, c.IssueID)
		}
	}
	for i := 0; i < len(ids); i += 500 {
		l, err := ps.List(ctx, orchapi.IssueQuery{Status: "all", IDs: ids[i:min(i+500, len(ids))], Limit: 500}, all, s.now())
		if err != nil {
			return orchapi.AdapterChanges{}, s.storeErr(err, "")
		}
		res.Items = append(res.Items, s.workItems(ctx, project, l.Issues)...)
	}
	return res, nil
}

// workItems are native issues as work items, with their attachments'
// metadata (created_by left empty: the mirror is shared by every reader).
func (s *Service) workItems(ctx context.Context, project string, its []orchapi.Issue) []orchapi.WorkItem {
	s.fillAttachments(ctx, nil, project, its)
	out := make([]orchapi.WorkItem, 0, len(its))
	for _, it := range its {
		out = append(out, workItem(it))
	}
	return out
}

// workItem is a native issue as a work adapter answers it (section 11): the
// native status, priority, type, number and assignee in extra.
func workItem(it orchapi.Issue) orchapi.WorkItem {
	w := orchapi.WorkItem{ID: NativePrefix + ":" + it.ID, Source: NativePrefix, Project: it.Project, Area: it.Area, Title: it.Title, Body: it.Description,
		Status: it.ZmStatus, Owner: it.ClaimedBy, CreatedBy: it.CreatedByAddress, Labels: it.Labels, Created: it.CreatedAt, Updated: it.UpdatedAt,
		Extra: map[string]any{"status": it.Status, "priority": it.Priority, "issue_type": it.IssueType, "number": it.Number}}
	if w.CreatedBy == "" {
		w.CreatedBy = it.CreatedBy
	}
	if it.Assignee != "" {
		w.Extra["assignee"] = it.Assignee
	}
	if it.Parent != "" {
		w.Extra["parent"] = it.Parent
	}
	if it.ClosedAt != nil {
		w.Extra["closed_at"] = it.ClosedAt.UTC().Format(time.RFC3339Nano)
	}
	if it.DueAt != nil {
		w.Extra["due_at"] = it.DueAt.UTC().Format(time.RFC3339Nano)
	}
	if it.DeferUntil != nil {
		w.Extra["defer_until"] = it.DeferUntil.UTC().Format(time.RFC3339Nano)
	}
	if len(it.Attachments) > 0 {
		w.Extra["attachments"] = it.Attachments
	}
	return w
}

// NativeVersion implements orchcore.IssueService.
func (s *Service) NativeVersion(ctx context.Context, project string) (string, error) {
	ps, err := s.st.Project(ctx, project, false)
	if errorsIsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return ps.LastChange(ctx)
}

// HoldWrites implements orchcore.IssueService.
func (s *Service) HoldWrites(project, why string) (func(), error) {
	// A barrier: writes already past their held check commit first.
	g := s.gate(project)
	g.Lock()
	defer g.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur := s.held[project]; cur != "" {
		return nil, api.Errorf(api.CodeConflict, "this project's issues are held already (%s)", cur)
	}
	s.held[project] = why
	return func() {
		s.mu.Lock()
		delete(s.held, project)
		s.mu.Unlock()
	}, nil
}
