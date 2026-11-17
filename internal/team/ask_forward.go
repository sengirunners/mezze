package team

// A member's POST /v0/asks goes to the authority, which may hold it for the
// card judge's verdict (orchapi.CardJudgeWait) before it answers: the
// forward waits that long and more (askAnswerWithin), not teamlink's
// CallTimeout. A forward cut after the ask went out is not "unreachable,
// nothing sent": the authority may have stored it (zm-j63j: an asker told
// unreachable filed again and got "already asked").

import (
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	tl "github.com/sengirunners/potluck/kit/teamlink"
)

// askAnswerWithin is how long a forwarded ask may wait for the authority's
// answer to start: the card judge's wait, then the router's rest of it and
// the store and mail after, with margin (tests change it).
var askAnswerWithin = orchapi.CardJudgeWait + 30*time.Second

// askHook is the POST /v0/asks row's hook.
func (s *Service) askHook() *tl.RouteHook {
	return &tl.RouteHook{
		AnswerWithin: func(*http.Request, []byte) time.Duration { return askAnswerWithin },
		Named: func(w http.ResponseWriter, r *http.Request, x *remote, area string, explicit bool, _ []byte) bool {
			s.forwardAsk(w, r, x, area, explicit)
			return true
		},
	}
}

// forwardAsk is Forward for POST /v0/asks: an unavailable answer after the
// request went out (its headers were written) says the ask may be stored.
func (s *Service) forwardAsk(w http.ResponseWriter, r *http.Request, x *remote, area string, explicit bool) {
	var sent atomic.Bool
	trace := &httptrace.ClientTrace{WroteHeaders: func() { sent.Store(true) }}
	r = r.WithContext(httptrace.WithClientTrace(r.Context(), trace))
	resp, err := s.ForwardResp(r, x, area, explicit, nil)
	if err != nil {
		if sent.Load() && api.IsCode(err, api.CodeUnavailable) {
			s.d.Log.Warn("a forwarded ask's answer was cut", "authority", x.Snapshot().Authority, "err", err)
			err = askMayBeStored(x.Snapshot().Authority)
		}
		api.WriteError(w, err)
		return
	}
	defer resp.Body.Close()
	s.relay(w, r, resp)
}

// askMayBeStored is the error for an ask whose answer was cut: it reached
// the authority, which may have stored it (the cut's cause is in the log:
// its "unreachable" would read as nothing sent).
func askMayBeStored(authority string) error {
	return api.Errorf(api.CodeUnavailable, "the ask went to the authority %s but its answer was cut: it may have been stored", authority).
		WithHint("check zm ask ls --open before asking again; a repeat that was stored is refused as already asked").
		WithData(api.NoQueueData{Outcome: api.OutcomeUnknown})
}
