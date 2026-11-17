package approvals

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// One-tap phone pairing (SPEC 16): a phone signed in to the tailnet as the
// person who put this daemon on it pairs by opening the page and tapping,
// with no code. Every check code pairing makes still holds, confirmation
// included; the OS is self-reported, so it narrows and never grants alone.

// phoneRefusal is why the calling device cannot pair with one tap, or nil.
func phoneRefusal(rm *core.Remote) error {
	switch {
	case rm.Via == core.ViaTunnel:
		return api.Errorf(api.CodeForbidden, "one-tap pairing needs the tailnet")
	case rm.Self:
		return api.Errorf(api.CodeForbidden, "this request comes from the daemon's own machine").
			WithHint("open this page on your phone")
	case rm.Tagged:
		return api.Errorf(api.CodeForbidden, "tagged nodes (servers, CI) are never a person's device")
	case rm.Zm || api.IsDaemonNode(rm.Node):
		return api.Errorf(api.CodeForbidden, "%s runs a zm daemon (its agents could answer)", rm.Node)
	case !isPhoneOS(rm.OS):
		return api.Errorf(api.CodeForbidden, "one-tap pairing is for a phone").
			WithHint("scan this page's QR code with your phone's camera")
	case !rm.SameLogin:
		return api.Errorf(api.CodeForbidden, "this phone is signed in to Tailscale as someone other than this zm's owner").
			WithHint("sign the phone in to Tailscale with the account this zm uses, or ask an approver to add you")
	}
	return nil
}

func isPhoneOS(os string) bool {
	return strings.EqualFold(os, "iOS") || strings.EqualFold(os, "android")
}

// phoneName is a valid approver name from a node name: its first label,
// lower case, other characters dropped.
func phoneName(node string) string {
	label, _, _ := strings.Cut(strings.ToLower(node), ".")
	var b strings.Builder
	for _, r := range label {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if n := strings.Trim(b.String(), "-_"); n != "" && n[0] >= 'a' && n[0] <= 'z' {
		return n
	}
	return "phone"
}

// PairPhone pairs the calling phone: the first one is active (bootstrap);
// a later one waits for an existing approver's tap, as code pairing does.
func (s *Service) PairPhone(ctx context.Context, rm *core.Remote) (orchapi.Approver, error) {
	if err := phoneRefusal(rm); err != nil {
		return orchapi.Approver{}, err
	}
	if prev, ok := s.approverOf(ctx, rm); ok {
		return prev.Approver, nil
	}
	// Checks Pair makes after the name is taken, made first so a refusal
	// leaves no pending name behind.
	if !s.mayTry(rm.NodeID) {
		return orchapi.Approver{}, api.Errorf(api.CodeForbidden, "too many pairing attempts; wait %s", CodeTTL)
	}
	var bound string
	switch err := s.d.Store.Registry().QueryRowContext(ctx, `SELECT name FROM approvers WHERE node_id = ?`, rm.NodeID).Scan(&bound); {
	case err == nil:
		return orchapi.Approver{}, api.Errorf(api.CodeConflict, "this phone's node key changed since it paired as approver %s; it must pair again", bound).
			WithHint("on the daemon's machine: zm approver rm " + bound + ", then open this page again")
	case !errors.Is(err, sql.ErrNoRows):
		return orchapi.Approver{}, err
	}
	base := phoneName(rm.Node)
	var add orchapi.NewApproverResponse
	var err error
	for i := 1; i <= 9; i++ {
		name := base
		if i > 1 {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		if add, err = s.Add(ctx, "phone:"+rm.Node, name); err == nil {
			break
		}
		var e *api.Error
		if !errors.As(err, &e) || e.Code != api.CodeConflict {
			return orchapi.Approver{}, err
		}
	}
	if err != nil {
		return orchapi.Approver{}, err
	}
	a, err := s.Pair(ctx, rm, add.Code)
	if err != nil {
		if _, derr := s.d.Store.Registry().ExecContext(ctx, `DELETE FROM approvers WHERE name = ? AND state = ?`,
			add.Approver.Name, orchapi.ApproverPending); derr != nil {
			s.d.Log.Warn("approvals: one-tap pairing left a pending name", "approver", add.Approver.Name, "err", derr)
		}
	}
	return a, err
}

func (s *Service) handlePairPhone(w http.ResponseWriter, r *http.Request) {
	rm, err := s.device(r)
	if err != nil {
		api.WriteError(w, err)
		return
	}
	a, err := s.PairPhone(r.Context(), rm)
	respond(w, a, err)
}
