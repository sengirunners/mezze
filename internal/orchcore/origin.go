package orchcore

import (
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// OriginChecked refuses a call whose caller nobody checked (SPEC "Key
// approvers", rule d): the calls a member daemon checks on its socket for
// an agent session before it forwards them, marking them
// orchapi.OriginChecked (internal/team/origin_gate.go). marked is whether
// the call carries the mark. From a member's node-bound token without it,
// its zm is too old (named; what says what is refused, as "its key sign-ins
// and key answers"); on this daemon's socket, the origin gate did not run
// (fail closed); any other caller (a minted token, a tailnet login) gets
// other. Approvals (key answers, sign-ins) and automations (a home's record
// writes) call it.
func OriginChecked(sub *authz.Subject, marked bool, what string, other error) error {
	if marked {
		return nil
	}
	if sub != nil && sub.Via == authz.ViaToken && sub.Node != "" && sub.Machine != "" {
		return api.Errorf(api.CodeForbidden, "machine %s runs a zm too old to check who calls it, so %s are refused", sub.Machine, what).
			WithHint("update zm on " + sub.Machine).WithData(orchapi.Refusal{Reason: orchapi.ReasonMemberTooOld, Machine: sub.Machine})
	}
	if sub == nil || sub.Via != authz.ViaLocal {
		return other
	}
	return api.Errorf(api.CodeForbidden, "this daemon did not check whether the call comes from an agent session, so it is refused").
		WithData(orchapi.Refusal{Reason: orchapi.ReasonMemberTooOld})
}
