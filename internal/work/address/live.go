package address

import (
	"context"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// LiveRecipient keeps mail about a work item (in project, area) out of an
// ended session's inbox. A session that has ended, or that the
// registry no longer has (on an authority, a team member's session the
// presence of its machine no longer lists), is not mailed: a session on
// this machine whose stable name is now held by a live session of the
// item's project that can read its area hands it to that successor (a
// handover); otherwise the mail goes to the group mailbox of the session's
// machine. Other addresses and a registry that does not answer keep addr;
// via never names the new recipient. The error is always nil.
func LiveRecipient(ctx context.Context, sessions orchcore.SessionService, cfg *config.Config, project, area, addr, via string) (string, string, error) {
	a, err := api.ParseAddress(addr)
	if err != nil || a.Kind != api.KindSession || sessions == nil || cfg == nil {
		return addr, via, nil
	}
	var ss orchapi.Session
	var ok bool
	if le, can := sessions.(orchcore.SessionLookupErr); can {
		if ss, ok, err = le.LookupErr(ctx, addr); err != nil {
			return addr, via, nil
		}
	} else {
		ss, ok = sessions.Lookup(ctx, addr)
	}
	if ok && ss.State != orchapi.StateEnded {
		return addr, via, nil
	}
	if names, can := sessions.(orchcore.SessionNames); can && ok && ss.Name != "" && a.Machine == cfg.Machine {
		next, live := names.ResolveName(ctx, a.Machine+"/"+ss.Name)
		if live && next.Address != "" && next.Address != ss.Address && next.State != orchapi.StateEnded &&
			next.Project == project && (next.Area == "" || next.Area == area) {
			return next.Address, via + " (successor of an ended session)", nil
		}
	}
	return "machine:" + a.Machine, via + " (that session has ended)", nil
}
