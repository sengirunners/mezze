package approvals

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// ---- receipts ----

func (s *Service) handleKey(w http.ResponseWriter, r *http.Request) {
	pub, err := s.PublicKey()
	if err != nil {
		s.d.Log.Error("approvals: receipt key", "err", err)
		api.WriteError(w, api.Errorf(api.CodeUnavailable, "no receipt key"))
		return
	}
	api.WriteJSON(w, http.StatusOK, PublicKey(pub))
}

// handleVerify checks ?receipt= (the receipt JSON, base64url or plain)
// against this daemon's key and the expectations in the query: project
// (ULID, or a slug the caller can see), ask, repo, pr, head_sha, decision,
// approver; signed=1 also refuses an owner's unsigned answer (the socket, an
// owner token, an answer-only token). An approver that names a person also matches a receipt a device
// paired for that person signed (answeredBy), but only for a caller who may
// read the ask; otherwise it reads nothing else, so it reveals nothing.
func (s *Service) handleVerify(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	raw := strings.TrimSpace(q.Get("receipt"))
	if raw == "" {
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "?receipt= is required (the receipt JSON, base64url-encoded)"))
		return
	}
	pub, err := s.PublicKey()
	if err != nil {
		api.WriteError(w, api.Errorf(api.CodeUnavailable, "no receipt key"))
		return
	}
	b := []byte(raw)
	if !strings.HasPrefix(raw, "{") {
		if b, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(raw, "=")); err != nil {
			api.WriteJSON(w, http.StatusOK, orchapi.ReceiptCheck{Reason: "receipt is not base64url JSON"})
			return
		}
	}
	rc, err := ParseReceipt(b)
	if err != nil {
		api.WriteJSON(w, http.StatusOK, orchapi.ReceiptCheck{Reason: "not a receipt"})
		return
	}
	exp := orchapi.ReceiptExpect{Ask: q.Get("ask"), Repo: q.Get("repo"), HeadSHA: q.Get("head_sha"), Decision: q.Get("decision"),
		Approver: q.Get("approver"), Work: q.Get("work"), Question: q.Get("question"), Card: q.Get("card")}
	switch q.Get("signed") {
	case "", "0":
	case "1":
		exp.Signed = true
	default:
		api.WriteError(w, api.Errorf(api.CodeBadRequest, "signed must be 1 (or absent)"))
		return
	}
	if p := q.Get("pr"); p != "" {
		if exp.PR, err = strconv.Atoi(p); err != nil || exp.PR <= 0 {
			api.WriteError(w, api.Errorf(api.CodeBadRequest, "pr must be a number"))
			return
		}
	}
	if p := strings.TrimSpace(q.Get("project")); p != "" {
		if len(p) == 26 && strings.ToUpper(p) == p {
			exp.Project = p
		} else {
			pr, err := s.d.Projects.Get(core.SubjectOf(ctx).Scope, p)
			if err != nil {
				api.WriteError(w, err)
				return
			}
			exp.Project = pr.ID
		}
	}
	api.WriteJSON(w, http.StatusOK, s.verifyAs(ctx, core.SubjectOf(ctx), rc, pub, exp))
}

// verifyAs is Verify, except that exp.Approver may name the person whose
// paired device answered: a device signs as its own name (zm approver add
// --for), so a gate that asks for the person would otherwise refuse that
// person's own answer. Offline verification (no daemon) cannot know who a
// device belongs to and stays exact.
func (s *Service) verifyAs(ctx context.Context, sub *authz.Subject, rc orchapi.Receipt, pub ed25519.PublicKey, exp orchapi.ReceiptExpect) orchapi.ReceiptCheck {
	chk := Verify(rc, pub, exp)
	if chk.Valid || exp.Approver == "" || exp.Approver == rc.Approver {
		return chk
	}
	rest := exp
	rest.Approver = ""
	c := Verify(rc, pub, rest)
	if !c.Valid || !s.answeredBy(ctx, sub, rc, exp.Approver) {
		return chk
	}
	c.Checked = append(c.Checked, "approver")
	return c
}

// answeredBy reports whether rc is the recorded decision of an ask sub may
// read, answered on a device paired for the person name names (any of their
// linked accounts, as linked now; approverPerson, which ties the device as
// paired when it answered). A device paired for no one is no one's here,
// even where it answers for a sole owner.
func (s *Service) answeredBy(ctx context.Context, sub *authz.Subject, rc orchapi.Receipt, name string) bool {
	if sub == nil {
		return false
	}
	a, err := s.readableAsk(ctx, sub, rc.Ask)
	if err != nil || a.Project != rc.Project || a.Decision == nil ||
		a.Decision.Receipt.Sig != rc.Sig || a.Decision.Approver != rc.Approver {
		return false
	}
	if a.Decision.Method == orchapi.MethodApprover {
		// Only a device paired for a person (zm approver add --for): one
		// paired for no one stands for a sole owner when answering, but a
		// gate that names a person needs that person named at pairing.
		var id string
		if err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT identity FROM approvers WHERE name = ? AND node_id = ?`,
			a.Decision.Approver, rc.Node).Scan(&id); err != nil || id == "" {
			return false
		}
	}
	ctx = withPeopleCache(ctx)
	got := s.approverPerson(ctx, a)
	if got == "" {
		return false
	}
	people, err := s.people(ctx, a.Project)
	if err != nil {
		return false
	}
	p, _, err := resolvePerson(answering(people), name)
	return err == nil && p.ID == got
}
