package team

// For you on a member Mac (SPEC 9 "For you", orchcore.TeamForYou).
// A device paired here acts for its person; the owner's for-you mail in a
// joined project lives at that project's authority, so it is listed and
// marked read there, live, with the project's node token: the token speaks
// for the machine's owner (the identity the replica joined as), never for
// another person paired here. Listed live, not from the copy: only the
// authority knows which needs=human mail is an ask (a decision, not news).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/sengirunners/potluck/kit/teamlink"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/project"
)

var _ orchcore.TeamForYou = (*Service)(nil)

// ForYouWait bounds each authority's answer to a device page's list or Got
// it (the page polls every 30 s).
var ForYouWait = ClaimWait

// ownerOf reports whether person is this machine's owner, the person every
// joined project's node token speaks for.
func (s *Service) ownerOf(person *authz.Subject) bool {
	if person == nil || person.Session != "" || s.d.Projects == nil {
		return false
	}
	own := s.d.Projects.Local("").Identity.ID
	return own != "" && person.Identity.ID == own
}

// forYouRemotes is the joined projects still joined whose authority may
// answer now (a copy that knows it is unreachable is skipped at once).
func (s *Service) forYouRemotes() (live []*remote, skipped []api.WorkSkipped) {
	for _, x := range s.List() {
		snap := x.Snapshot()
		if teamlink.Ended(snap) != nil {
			continue
		}
		if rp := rep(x); rp != nil && rp.isReady() && !rp.isReachable() {
			skipped = append(skipped, forYouSkip(snap, "the authority "+snap.Authority+" is unreachable"))
			continue
		}
		live = append(live, x)
	}
	return live, skipped
}

func forYouSkip(snap api.Remote, why string) api.WorkSkipped {
	return api.WorkSkipped{Project: snap.Project.ID, Source: snap.Project.Slug, Error: "for-you mail: " + why}
}

// TeamForYou implements orchcore.TeamForYou.
func (s *Service) TeamForYou(ctx context.Context, person *authz.Subject, limit int) ([]orchapi.Mail, []api.WorkSkipped) {
	if !s.ownerOf(person) || s.Count() == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	live, skipped := s.forYouRemotes()
	got := make([][]orchapi.Mail, len(live))
	errs := make([]error, len(live))
	var wg sync.WaitGroup
	for i, x := range live {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i], errs[i] = s.forYouAt(ctx, x, limit)
		}()
	}
	wg.Wait()
	var out []orchapi.Mail
	for i, x := range live {
		if errs[i] != nil {
			snap := x.Snapshot()
			s.d.Log.Debug("team: for-you list", "project", snap.Project.Slug, "err", errs[i])
			skipped = append(skipped, forYouSkip(snap, "the authority "+snap.Authority+" did not answer"))
			continue
		}
		out = append(out, got[i]...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].ID > out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, skipped
}

// forYouAt is the owner's unread for-you mail at x's authority: its open
// mail to the owner as the authority answers it (an ask's mail carries no
// for_you there), less notices, the owner's own and digest items not due.
func (s *Service) forYouAt(ctx context.Context, x *remote, limit int) ([]orchapi.Mail, error) {
	snap := x.Snapshot()
	me := "human:" + snap.Identity
	q := url.Values{"project": {snap.Project.ID}, "to": {me}, "open": {"1"}, "limit": {strconv.Itoa(limit)}}
	cctx, cancel := context.WithTimeout(ctx, ForYouWait)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, snap.URL+"/v0/mail?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	api.SetHeader(req.Header, api.HeaderProject, snap.Project.ID)
	api.SetHeader(req.Header, api.HeaderProjectFrom, project.RuleFlag)
	resp, err := s.Send(cctx, x, req, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var ml orchapi.MailList
	if err := teamlink.Decode(resp, &ml); err != nil {
		return nil, err
	}
	now := time.Now()
	out := make([]orchapi.Mail, 0, len(ml.Mail))
	for _, m := range ml.Mail {
		if m.ForYou == "" || m.Recipient != me || m.From == me || m.Kind == orchapi.MailKindNotice || m.AckedAt != nil ||
			(m.HeldUntil != nil && m.HeldUntil.After(now)) {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

// TeamGotIt implements orchcore.TeamForYou: Got it at the authority of the
// joined project holding id (its copy first, then each other authority:
// one that lacks it answers not_found and takes nothing).
func (s *Service) TeamGotIt(ctx context.Context, person *authz.Subject, id string) (orchapi.AckResponse, error) {
	missing := api.NotFound("mail", id)
	if !s.ownerOf(person) || s.Count() == 0 || id == "" {
		return orchapi.AckResponse{}, missing
	}
	live, _ := s.forYouRemotes()
	var holds, rest []*remote
	for _, x := range live {
		if s.copyHolds(ctx, x, id) {
			holds = append(holds, x)
		} else {
			rest = append(rest, x)
		}
	}
	var last error = missing
	for _, x := range append(holds, rest...) {
		cctx, cancel := context.WithTimeout(ctx, ForYouWait)
		var ar orchapi.AckResponse
		err := s.callAs(cctx, x, "", http.MethodPost, "/v0/mail/"+url.PathEscape(id)+"/got-it", struct{}{}, &ar)
		cancel()
		if api.IsCode(err, api.CodeNotFound) {
			continue
		}
		if err != nil {
			var e *api.Error
			if !errors.As(err, &e) || e.Code == api.CodeUnavailable || e.Code == api.CodeInternal {
				last = err // no answer: another authority may still hold it
				continue
			}
			return orchapi.AckResponse{}, err // the authority's refusal (not a for-you item, not the owner's)
		}
		s.gotItInCopy(ctx, x, ar)
		return ar, nil
	}
	return orchapi.AckResponse{}, last
}

// copyHolds reports whether x's copy holds mail id.
func (s *Service) copyHolds(ctx context.Context, x *remote, id string) bool {
	rp := rep(x)
	if rp == nil || !rp.isReady() {
		return false
	}
	_, ok := rp.findMail(ctx, id)
	return ok
}

// gotItInCopy puts the authority's answer to a Got it into x's copy, as a
// settled ack's is (its change event brings the same later).
func (s *Service) gotItInCopy(ctx context.Context, x *remote, ar orchapi.AckResponse) {
	rp := rep(x)
	if rp == nil || !rp.isReady() {
		return
	}
	body, err := json.Marshal(ar)
	if err != nil {
		return
	}
	if err := rp.writeApply(ctx, func(tx *sql.Tx) error {
		return applyResult(ctx, tx, outEntry{kind: "ack"}, body)
	}); err != nil {
		s.d.Log.Warn("team: Got it into the copy", "project", x.Slug(), "err", err)
	}
}
