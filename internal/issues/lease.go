package issues

import (
	"context"
	"strings"
	"unicode"

	istore "github.com/sengirunners/mezze/internal/issues/store"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/api/textclean"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// Lease-backed claims (SPEC "Native issues": claims). When a project binds
// its native issues as a work source (a [[trackers]] entry with adapter
// native), a native claim is a section 10 lease on the issue's work id
// (<prefix>:<id>): POST /v0/issues/{id}/claim, and so the bd door's
// update --claim and ready --claim, claims through the work
// module exactly as zm work claim does (TTL lease, or a bound one for a
// managed session; renewed by claiming again; expiry releases the issue).
// A native write that ends the claim (a close, a reopen, another
// assignee) drops the lease. Without the binding a claim stays as it was:
// atomic, with no deadline.

// fromWorkCtx marks a write the work module makes through the native
// adapter (a claim or release that already holds or drops the lease).
type fromWorkCtx struct{}

func fromWork(ctx context.Context) bool {
	v, _ := ctx.Value(fromWorkCtx{}).(bool)
	return v
}

// nativePrefix is the prefix of project's native work source ("" when the
// project does not bind its native issues).
func (s *Service) nativePrefix(project string) string {
	if orchcore.Work(s.d) == nil {
		return ""
	}
	f, ok := s.d.Projects.File(project)
	if !ok {
		return ""
	}
	for _, t := range orchcore.ProjectTrackers(f) {
		if t.Adapter == orchapi.AdapterNative {
			return t.Prefix
		}
	}
	return ""
}

// leaseClaim claims l's issue through the work module when its project
// binds native issues (ok false: it does not; claim natively).
func (s *Service) leaseClaim(ctx context.Context, sub *authz.Subject, l located, req orchapi.ClaimIssueRequest, label string) (writeResult, bool, error) {
	p := s.nativePrefix(l.project)
	if p == "" {
		return writeResult{}, false, nil
	}
	wid := p + ":" + l.id
	res, err := orchcore.Work(s.d).ClaimItem(core.WithClaimLabel(ctx, label), sub, l.project, wid, orchapi.ClaimRequest{TTLSeconds: req.TTLSeconds, Fence: req.Fence})
	if err != nil {
		return writeResult{}, true, err
	}
	view, err := l.ps.Get(ctx, l.id, l.vis)
	if err != nil {
		return writeResult{}, true, s.storeErr(err, l.id)
	}
	if holder := res.Lease.Holder; holder != "" && (view.ClaimedBy != holder || label != "" && view.Assignee != label) {
		// A team project writes the source's claim back later (SPEC 17.3):
		// the native door answers with the claim made, so make it now (the
		// write-back then finds it done). The work module's claim names the
		// caller; a routing label relabels it here. Refused: the lease goes too.
		wctx := context.WithValue(ctx, fromWorkCtx{}, true)
		if _, _, err := s.write(wctx, as(sub, holder), l, istore.Mutation{Op: istore.OpClaim, Assignee: label}); err != nil {
			if derr := orchcore.Work(s.d).EndItemLease(context.WithoutCancel(ctx), l.project, wid, "", "claim refused"); derr != nil {
				s.warn("drop refused native lease", "id", wid, "err", derr)
			}
			return writeResult{}, true, err
		}
		if view, err = l.ps.Get(ctx, l.id, l.vis); err != nil {
			return writeResult{}, true, s.storeErr(err, l.id)
		}
	}
	s.redactIssue(ctx, sub, &view)
	return writeResult{Issue: view, LeaseFence: res.Lease.Fence}, true, nil
}

// mutationFence is the lease fence a write names (0 = none).
func mutationFence(m istore.Mutation) int64 {
	switch {
	case m.Update != nil:
		return m.Update.Fence
	case m.Close != nil:
		return m.Close.Fence
	case m.Reopen != nil:
		return m.Reopen.Fence
	case m.Comment != nil:
		return m.Comment.Fence
	}
	return 0
}

// fenceCheck refuses a write made under the caller's lease on the issue
// (a fence named) when that lease is no longer the caller's and live (SPEC
// "Leases": fencing). Without a native work source there are no leases, so
// a named fence is stale. A comment replayed by its id that is already
// there was taken under its lease: the store answers it as before.
func (s *Service) fenceCheck(ctx context.Context, sub *authz.Subject, l located, m istore.Mutation) error {
	fence := mutationFence(m)
	if fence == 0 {
		return nil
	}
	if m.Op == istore.OpComment && m.Comment.ID != "" {
		if cs, err := l.ps.Comments(ctx, l.id, l.vis); err == nil {
			for _, c := range cs {
				if c.ID == m.Comment.ID {
					return nil
				}
			}
		}
	}
	p := s.nativePrefix(l.project)
	f, ok := orchcore.Work(s.d).(orchcore.LeaseFencer)
	if p == "" || !ok {
		return orchapi.StaleLease(l.id, fence)
	}
	return f.CheckFence(ctx, sub, l.project, p+":"+l.id, fence)
}

// endLease drops the issue's lease after a native write that ended its
// claim: the lease stays only with the address that still claims it
// (claimed or in review). Writes the work module made, and writes that
// touched no claim field, are left alone.
func (s *Service) endLease(ctx context.Context, project string, r istore.Result) {
	if fromWork(ctx) || r.Noop {
		return
	}
	touched := false
	for _, ch := range r.Changes {
		for _, f := range []string{"status", "assignee", "claimed_by"} {
			if _, ok := ch.Fields[f]; ok {
				touched = true
			}
		}
	}
	if !touched {
		return
	}
	p := s.nativePrefix(project)
	if p == "" {
		return
	}
	it := r.Issue
	keep := ""
	if it.ZmStatus == orchapi.WorkClaimed || it.ZmStatus == orchapi.WorkInReview {
		keep = it.ClaimedBy
	}
	if err := orchcore.Work(s.d).EndItemLease(context.WithoutCancel(ctx), project, p+":"+it.ID, keep, "issue "+it.Status); err != nil {
		s.warn("end native lease", "project", project, "id", it.ID, "err", err)
	}
}

// claimLabel checks a claim's routing label (orchapi.ClaimIssueRequest.Assignee).
func claimLabel(s string) (string, error) {
	raw := s
	s = strings.TrimSpace(textclean.Clean(s))
	if raw != "" && textclean.Blank(raw) && raw != s {
		return "", api.Errorf(api.CodeBadRequest, "empty assignee")
	}
	if len(s) > 255 {
		return "", api.Errorf(api.CodeBadRequest, "an assignee is at most 255 bytes")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", api.Errorf(api.CodeBadRequest, "an assignee has no control characters")
		}
	}
	return s, nil
}

// labelHeld refuses, before the work module is asked, a claim under a label
// on an issue claimed under another one: the work module would renew the
// address's lease, then release the issue when the native claim refuses.
// The store's claim checks the same thing in its transaction.
func (s *Service) labelHeld(ctx context.Context, sub *authz.Subject, l located, label string) error {
	if label == "" {
		return nil
	}
	cur, err := l.ps.Get(ctx, l.id, l.vis)
	if err != nil {
		return s.storeErr(err, l.id)
	}
	// Its own unlabelled claim, or its claim under a person's name that this
	// label (naming the caller's session or a lane under it) takes over.
	own := cur.ClaimedBy == sub.Address && (cur.Assignee == actorOf(sub).Name || api.LabelAdopts(cur.Assignee, label, sub.Address))
	if (cur.Status == orchapi.IssueInProgress || cur.Status == orchapi.IssueHooked) && cur.Assignee != "" && cur.Assignee != label && !own {
		return api.Errorf(api.CodeConflict, "issue already claimed by %s", cur.Assignee)
	}
	return nil
}
