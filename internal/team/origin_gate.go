package team

// The caller check on this machine's socket:
// a signed ask answer, a key sign-in and removing a key are the person's
// own, so a call to them from inside a registered agent session is refused
// here, before any forward, and one from outside goes on marked
// orchapi.OriginChecked for the authority. The authority sees a member's
// forwarded call as the machine's token only, so only the daemon that took
// the socket call can see its process. choose is not refused: each choose
// on the socket logs where it came from, to learn who still calls it from
// sessions before a later change refuses it. A home's automation record
// writes (SPEC 18.6, the home rule) are checked and marked the same way:
// the home's Mezze app server runs outside any agent session. Minting an
// answer-only token (POST /v0/tokens with a limit) is the person's own too,
// and refused from a session the same way, failing closed; an owner-token
// mint is unchanged.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// loginPurposeKey is the GitHub login purpose that binds a signing key
// (the kit's owners module adds it, plan 1.1).
const loginPurposeKey = "key"

const (
	originChoose = "POST /v0/asks/{id}/choose"
	originAnswer = "POST /v0/asks/{id}/answer"
	originLogin  = "POST /v0/login/github/start"
	originUnbind = "DELETE /v0/approvals/key"
	originMint   = "POST /v0/tokens"

	originAutoCreate = "POST /v0/automations"
	originAutoPatch  = "PATCH /v0/automations/{id}"
	originAutoDelete = "DELETE /v0/automations/{id}"
	originAutoHealth = "PUT /v0/automations/{id}/health"
	originAutoClaim  = "PUT /v0/automation-homes/{home}"
)

// automationRoutes are the home rule's writes (internal/automations).
var automationRoutes = map[string]bool{originAutoCreate: true, originAutoPatch: true, originAutoDelete: true, originAutoHealth: true, originAutoClaim: true}

var originRoutes = func() *http.ServeMux {
	m := http.NewServeMux()
	for _, p := range []string{originChoose, originAnswer, originLogin, originUnbind, originMint, originAutoCreate, originAutoPatch, originAutoDelete, originAutoHealth, originAutoClaim} {
		m.HandleFunc(p, func(http.ResponseWriter, *http.Request) {})
	}
	return m
}()

// originRoute is the row of originRoutes r matches ("" none).
func originRoute(r *http.Request) string {
	_, pat := originRoutes.Handler(r)
	switch pat {
	case originChoose, originAnswer, originLogin, originUnbind, originMint:
		return pat
	}
	if automationRoutes[pat] {
		return pat
	}
	return ""
}

// gateOrigin is the request gate (core.RequestGate) for the routes above,
// on socket calls only: a call that came over the network was checked, if
// at all, by the daemon whose socket took it.
func (s *Service) gateOrigin(w http.ResponseWriter, r *http.Request, serve func(http.ResponseWriter, *http.Request)) bool {
	if !core.IsLocal(r.Context()) {
		return false
	}
	route := originRoute(r)
	if route == "" {
		return false
	}
	r = withoutOriginMark(r)
	if route == originChoose {
		origin := "unknown"
		if sc, ok := orchcore.Sessions(s.d).(orchcore.SessionCaller); ok && sc != nil {
			if o, sure, err := sc.CallerSessionNow(r.Context(), core.SubjectOf(r.Context())); err != nil {
				origin = "unknown: " + err.Error()
			} else if o != "" {
				origin = o
			} else if sure {
				origin = "none"
			}
		}
		ask, _ := strings.CutPrefix(strings.TrimSuffix(r.URL.Path, "/choose"), "/v0/asks/")
		s.d.Log.Info("ask choose on the socket", "ask", ask, "origin", origin)
		serve(w, r)
		return true
	}
	if route == originLogin {
		checked, body, err := keyLogin(r)
		if err != nil {
			api.WriteError(w, err)
			return true
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if !checked {
			serve(w, r)
			return true
		}
	}
	if route == originMint {
		limited, body, err := limitedMint(r)
		if err != nil {
			api.WriteError(w, err)
			return true
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		if !limited {
			serve(w, r) // an owner-token mint, unchanged
			return true
		}
	}
	origin, err := s.callerSession(r)
	auto := automationRoutes[route]
	if err != nil {
		hint := "answer the card, sign in or remove the key from the Mezze app or a terminal outside any agent session"
		if route == originMint {
			hint = "mint an answer-only token from a terminal outside any agent session"
		}
		if auto {
			hint = "start the home's Mezze app server outside any agent session"
		}
		api.WriteError(w, api.Errorf(api.CodeForbidden, "cannot tell whether this call comes from an agent session (%v), so it is refused", err).WithHint(hint))
		return true
	}
	if origin != "" && auto {
		api.WriteError(w, api.Errorf(api.CodeForbidden, "this call comes from inside agent session %s: a home's automation records are written by its Mezze app server, outside any agent session", origin).
			WithHint("an agent changes a job through the home's Mezze app, or files a proposal a person approves"))
		return true
	}
	if origin != "" && route == originMint {
		api.WriteError(w, api.Errorf(api.CodeForbidden, "this call comes from inside agent session %s: an answer-only token is minted by the person, outside any agent session", origin).
			WithHint("the person mints it in a terminal of their own (zm token mint --answer-only)"))
		return true
	}
	if origin != "" {
		api.WriteError(w, api.Errorf(api.CodeForbidden, "this call comes from inside agent session %s: answering a card with a key, a key sign-in and removing a key are the person's own", origin).
			WithHint("the person answers in the Mezze app or on their phone; an agent asks with zm ask new"))
		return true
	}
	q := r.URL.Query()
	q.Set(orchapi.OriginChecked, "1")
	r = r.Clone(r.Context())
	r.URL.RawQuery = q.Encode()
	serve(w, r)
	return true
}

// callerSession asks the session module which registered session the
// caller runs inside; with none wired it errs (refused: fail closed).
func (s *Service) callerSession(r *http.Request) (string, error) {
	sc, ok := orchcore.Sessions(s.d).(orchcore.SessionCaller)
	if !ok || sc == nil {
		return "", api.Errorf(api.CodeUnavailable, "no session module to ask")
	}
	return sc.CallerSession(r.Context(), core.SubjectOf(r.Context()))
}

// withoutOriginMark is r (a shallow copy) with any OriginChecked the caller
// sent dropped: only this gate sets it.
func withoutOriginMark(r *http.Request) *http.Request {
	q := r.URL.Query()
	if !q.Has(orchapi.OriginChecked) {
		return r
	}
	q.Del(orchapi.OriginChecked)
	r2 := r.Clone(r.Context())
	r2.URL.RawQuery = q.Encode()
	return r2
}

// limitedMint reads a token mint's body as the mint handler does (the same
// decoder, size limit and fields): limited when it asks for a limited token
// (a limit or a device, which is what sends the handler to the limited
// mint), and refused when it cannot be read, so the check never passes a
// body the handler would read differently. body is what the handler then
// reads, byte for byte.
func limitedMint(r *http.Request) (limited bool, body []byte, err error) {
	if r.Body == nil {
		return false, nil, nil
	}
	if body, err = io.ReadAll(io.LimitReader(r.Body, api.MaxBody)); err != nil {
		return false, nil, api.Errorf(api.CodeBadRequest, "reading the request: %v", err)
	}
	probe := r.Clone(r.Context())
	probe.Body = io.NopCloser(bytes.NewReader(body))
	var req api.MintTokenRequest
	if err := api.DecodeJSON(probe, &req); err != nil {
		return false, nil, err
	}
	return req.Limit != "" || req.Node != "", body, nil
}

// keyLogin reads a login start's body: checked when its purpose binds a
// key, or when the body cannot be read as a login (the authority refuses
// such a body anyway; checking it costs nothing and fails closed).
func keyLogin(r *http.Request) (checked bool, body []byte, err error) {
	if r.Body == nil {
		return false, nil, nil // no body: the default purpose (link)
	}
	body, err = io.ReadAll(io.LimitReader(r.Body, api.MaxBody+1))
	if err != nil {
		return false, nil, api.Errorf(api.CodeBadRequest, "reading the request: %v", err)
	}
	if len(body) > api.MaxBody {
		return false, nil, api.Errorf(api.CodeBadRequest, "request body over %d bytes", api.MaxBody)
	}
	var in struct {
		Purpose string `json:"purpose"`
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return false, body, nil // no body: the default purpose (link)
	}
	if json.Unmarshal(body, &in) != nil {
		return true, body, nil
	}
	return in.Purpose == loginPurposeKey, body, nil
}
