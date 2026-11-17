package team

// A session's lease taken at spawn on a joined project's item
// (orchcore.TeamLeases). Not through the outbox: a claim queued while the
// authority is away could land after the spawn that wanted it rolled back,
// assigning the work to a session that never ran.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/sengirunners/potluck/kit/teamlink"
	"net/http"
	"net/url"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/ids"
	"github.com/sengirunners/potluck/kit/project"
)

var _ orchcore.TeamLeases = (*Service)(nil)

// BindAt implements orchcore.TeamLeases. sub (the spawning caller, this
// machine's owner on its socket) is not checked here: the claim reaches no
// further than the session claiming from inside through the proxy would,
// and the authority judges it by this replica's grant.
func (s *Service) BindAt(ctx context.Context, sub *authz.Subject, proj, id, session string) (orchapi.Lease, error) {
	x := s.ByName(proj)
	if x == nil {
		return orchapi.Lease{}, api.NotFound("project", proj)
	}
	fence := x.RoleReply()
	snap := fence.Snap
	if err := teamlink.Ended(snap); err != nil {
		return orchapi.Lease{}, err
	}
	a, err := api.ParseAddress(session)
	if err != nil || a.Kind != api.KindSession || a.Machine != s.d.Config.Machine {
		return orchapi.Lease{}, api.Errorf(api.CodeBadRequest, "%q is not a session on %s", session, s.d.Config.Machine)
	}
	if id == "" {
		return orchapi.Lease{}, api.Errorf(api.CodeBadRequest, "a work item id is required")
	}
	if rp := rep(x); rp.isReady() && !rp.isReachable() {
		return orchapi.Lease{}, api.Errorf(api.CodeUnavailable, "the authority %s is unreachable: the session takes %s once it answers", snap.Authority, id).
			WithHint("start without --work and claim it from the session, or try again")
	}
	cctx, cancel := context.WithTimeout(ctx, ClaimWait)
	defer cancel()
	var cr orchapi.ClaimResponse
	err = s.callAs(cctx, x, a.String(), http.MethodPost, "/v0/work/"+url.PathEscape(id)+"/claim", orchapi.ClaimRequest{}, &cr)
	if err != nil {
		var e *api.Error
		if errors.As(err, &e) && e.Code != api.CodeUnavailable && e.Code != api.CodeInternal {
			return orchapi.Lease{}, err // the authority's answer (not_found, forbidden, a conflict): nothing was taken
		}
		// No answer, or a failed one: it may have landed. Release it as the
		// session, best effort (else its default TTL ends it: never bound,
		// since the session never heartbeats).
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), ClaimWait)
			defer rcancel()
			_ = s.callAs(rctx, x, a.String(), http.MethodPost, "/v0/work/"+url.PathEscape(id)+"/release", struct{}{}, nil)
		}()
		return orchapi.Lease{}, api.Errorf(api.CodeUnavailable, "the authority %s did not take the claim of %s: %v", snap.Authority, id, err).
			WithHint("start without --work and claim it from the session, or try again")
	}
	if cr.Lease.Key == "" {
		return orchapi.Lease{}, api.Errorf(api.CodeInternal, "the authority answered no lease")
	}
	// Claimed as the session: the authority redacts its address until a
	// heartbeat shows it (no presence row yet), so name it here.
	cr.Lease.Holder = a.String()
	// What the session holds, for its heartbeats (the authority binds the
	// lease once presence shows the session) and the lost-lease check.
	if unguard, ok := fence.Guard(); ok {
		s.pmu.Lock()
		s.cache.claimedIn(cr.Lease.Project, a.String(), cr.Lease.Key)
		s.cache.claimed(a.String(), cr.Lease.Key)
		s.pmu.Unlock()
		unguard()
	}
	s.kickPresence()
	return cr.Lease, nil
}

// callAs is one live write at x's authority as as (a session or machine
// address on this machine, sent as Zm-As; "": the replica's identity, its
// machine's owner), with a write id so it runs once.
func (s *Service) callAs(ctx context.Context, x *remote, as, method, path string, in, out any) error {
	snap := x.Snapshot()
	body, _ := json.Marshal(in)
	req, err := http.NewRequestWithContext(ctx, method, snap.URL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	api.SetHeader(req.Header, api.HeaderWriteID, ids.New())
	api.SetHeader(req.Header, api.HeaderProject, snap.Project.ID)
	api.SetHeader(req.Header, api.HeaderProjectFrom, project.RuleFlag)
	if as != "" {
		api.SetHeader(req.Header, api.HeaderAs, as)
	}
	start := time.Now()
	resp, err := s.Send(ctx, x, req, body)
	if err != nil {
		return api.Errorf(api.CodeUnavailable, "%v (after %s)", err, time.Since(start).Round(time.Millisecond))
	}
	defer resp.Body.Close()
	return teamlink.Decode(resp, out)
}

// ReleaseAt implements orchcore.TeamLeases: the lease a session whose start
// failed took with BindAt, given back as that session. Live while the
// authority answers, else queued: a release by that address only ever drops
// that session's own lease, so it is safe to run later.
func (s *Service) ReleaseAt(ctx context.Context, proj, id, session string) error {
	x := s.ByName(proj)
	if x == nil {
		return api.NotFound("project", proj)
	}
	if err := teamlink.Ended(x.Snapshot()); err != nil {
		return err
	}
	a, err := api.ParseAddress(session)
	if err != nil || a.Kind != api.KindSession || a.Machine != s.d.Config.Machine {
		return api.Errorf(api.CodeBadRequest, "%q is not a session on %s", session, s.d.Config.Machine)
	}
	path := "/v0/work/" + url.PathEscape(id) + "/release"
	rp := rep(x)
	if !rp.isReady() || rp.isReachable() {
		cctx, cancel := context.WithTimeout(ctx, ClaimWait)
		err := s.callAs(cctx, x, a.String(), http.MethodPost, path, struct{}{}, nil)
		cancel()
		var e *api.Error
		if err == nil || (errors.As(err, &e) && e.Code != api.CodeUnavailable && e.Code != api.CodeInternal) {
			s.forgetHeld(a.String(), id)
			return err // done, or the authority's answer (not held by it: nothing to give back)
		}
	}
	if !rp.isReady() {
		return api.Errorf(api.CodeUnavailable, "the authority %s did not answer, and there is no copy to queue the release in", x.Snapshot().Authority)
	}
	if _, err := rp.enqueue(ctx, "release", http.MethodPost, path, []byte("{}"), a.String(), id, ""); err != nil {
		return err
	}
	s.forgetHeld(a.String(), id)
	return nil
}

// forgetHeld drops key from what this machine's heartbeat says addr holds.
func (s *Service) forgetHeld(addr, key string) {
	s.pmu.Lock()
	s.cache.endLease(addr, key)
	s.pmu.Unlock()
}
