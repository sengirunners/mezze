package work

// Releases and fenced writes (SPEC 10 "Leases", "Fencing"): who may drop a
// lease, and the check a write made under one makes.

import (
	"context"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// checkFence refuses a write sub makes under its lease on the item at l
// (fence, 0 = none named) unless that lease is still sub's and live
// (SPEC "Leases": fencing). q is where the project's leases are: a team
// project's transaction, so the check and the write commit together.
func (s *Service) checkFence(ctx context.Context, q queryer, sub *authz.Subject, l loc, fence int64) error {
	if fence == 0 {
		return nil
	}
	now := s.now()
	cur, err := itemLease(ctx, q, l.project, l.area, l.id, now)
	if err != nil {
		return err
	}
	if !fenceOK(cur, sub.Address, fence, now, true) {
		return staleLease(l.id, fence)
	}
	return nil
}

var _ orchcore.LeaseFencer = (*Service)(nil)

// CheckFence implements orchcore.LeaseFencer (checkFence on the item's
// current area, which locate finds as any work route would).
func (s *Service) CheckFence(ctx context.Context, sub *authz.Subject, project, id string, fence int64) error {
	if fence == 0 {
		return nil
	}
	l, err := s.locate(ctx, sub, sub.Scope.Only(project), id)
	if err != nil {
		if api.IsCode(err, api.CodeNotFound) {
			return staleLease(id, fence) // no work item, so no lease: the caller located the issue itself
		}
		return err
	}
	return s.checkFence(ctx, s.leaseReadDB(l.project), sub, l, fence)
}

// release drops the live lease cur. Only its holder may, unless force
// (operate on its project/area, the machine owner on the socket). fence,
// when set, must be cur's, and without force the caller cur's holder (else
// stale-lease): a late release never drops a newer lease.
func (s *Service) release(ctx context.Context, sub *authz.Subject, cur lease, force bool, fence int64) (orchapi.ReleaseResponse, error) {
	ctx = withActor(ctx, sub) // tracker writes are made for the caller
	obj := authz.Object{Project: cur.Project, Area: cur.Area}
	if err := sub.Check(authz.Write, obj, "lease", cur.Key); err != nil {
		return orchapi.ReleaseResponse{}, err
	}
	who := sub.Address
	if force {
		who = cur.Holder // an owner's forced release of exactly that lease (its rights are checked below)
	}
	if !fenceOK(&cur, who, fence, s.now(), false) {
		return orchapi.ReleaseResponse{}, staleLease(cur.Key, fence)
	}
	if cur.Holder != sub.Address {
		if !force {
			return orchapi.ReleaseResponse{}, api.Errorf(api.CodeForbidden, "%s is held by %s, not you", cur.Key, s.d.Redact(ctx, sub, cur.Holder)).
				WithHint("only the holder releases; mail them: zm send " + cur.Key + " ...")
		}
		if sub.Via != authz.ViaLocal || sub.Remote || !sub.Can(authz.Operate, obj) {
			return orchapi.ReleaseResponse{}, api.Errorf(api.CodeForbidden, "--force needs operate on the project, from this machine's socket")
		}
	}
	lk := s.locks.of(cur.Project)
	lk.RLock()
	defer lk.RUnlock()
	ok, err := s.dropAndAfter(ctx, cur, "")
	if err != nil {
		return orchapi.ReleaseResponse{}, err
	}
	if !ok {
		return orchapi.ReleaseResponse{Key: cur.Key}, nil // renewed or retaken meanwhile: nothing of ours to release
	}
	return orchapi.ReleaseResponse{Key: cur.Key, Released: true}, nil
}

// releaseItem is POST /v0/work/{id}/release.
func (s *Service) releaseItem(ctx context.Context, sub *authz.Subject, l loc, force bool, fence int64) (orchapi.ReleaseResponse, error) {
	if err := sub.Check(authz.Write, l.obj(), "work item", l.id); err != nil {
		return orchapi.ReleaseResponse{}, err
	}
	cur, err := itemLease(ctx, s.leaseReadDB(l.project), l.project, l.area, l.id, s.now())
	if err != nil {
		return orchapi.ReleaseResponse{}, err
	}
	if cur == nil {
		return orchapi.ReleaseResponse{Key: l.id}, nil
	}
	if cur.Area != l.area { // claimed before the item moved: it follows the item first
		if err := moveLease(ctx, s.leaseDB(l.project), l.project, l.id, l.area, s.now()); err != nil {
			return orchapi.ReleaseResponse{}, err
		}
		cur.Area = l.area
	}
	if !cur.live(s.now()) && cur.Holder != sub.Address && !force {
		return orchapi.ReleaseResponse{Key: l.id}, nil // the ticker expires it
	}
	return s.release(ctx, sub, *cur, force, fence)
}

// releaseKey is DELETE /v0/leases/{key}: the caller's own lease on key in
// the projects sc reads, else (with force) the one visible lease on it.
//
// fence, when set, picks the caller's lease with that fence; a lease of the
// caller's with another one is refused (stale-lease), never dropped.
func (s *Service) releaseKey(ctx context.Context, sub *authz.Subject, sc authz.Scope, key string, force bool, fence int64) (orchapi.ReleaseResponse, error) {
	nf := api.NotFound("lease", key)
	k, err := canonKey(key)
	if err != nil {
		return orchapi.ReleaseResponse{}, nf
	}
	where, args := sc.SQL("project", "area", authz.Read)
	ls, err := s.queryLeasesAll(ctx, where+" AND key = ?", append(args, k)...)
	if err != nil {
		return orchapi.ReleaseResponse{}, err
	}
	var mine, others []lease
	for _, l := range ls {
		switch {
		case l.Holder == sub.Address && (fence == 0 || l.Fence == fence):
			mine = append(mine, l)
		case l.Holder == sub.Address:
			others = append([]lease{l}, others...) // the caller's, under another fence: first, so the answer is stale-lease
		case l.live(s.now()):
			others = append(others, l)
		}
	}
	pick := mine
	if len(pick) == 0 && force {
		pick = others
		if fence != 0 { // a forced release of exactly the lease with that fence
			pick = nil
			for _, l := range others {
				if l.Fence == fence {
					pick = append(pick, l)
				}
			}
		}
	}
	switch {
	case len(pick) == 1:
		return s.release(ctx, sub, pick[0], force, fence)
	case len(pick) > 1:
		return orchapi.ReleaseResponse{}, api.Errorf(api.CodeBadRequest, "%s is leased in several projects or areas", k).
			WithHint("pass --project <slug>[/<area>]")
	case len(others) > 0:
		return s.release(ctx, sub, others[0], false, fence) // stale-lease, or explains who holds it (forbidden)
	}
	return orchapi.ReleaseResponse{}, nf
}
