package mail

import (
	"context"

	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
)

// parseTo parses a recipient. A session name (<machine>/<name>, SPEC 8
// "Names") of this machine resolves now to its live session's address,
// which is what the mail stores: the name given to another session later
// never redirects it. A name that resolves to nothing, or to a session
// sub may not see, answers like a missing session.
//
// A session address (<machine>/<session8>, stored or typed) is never
// read as a name: a name is never a session8. Another machine's names are
// not resolved here (on a team replica the proxy resolves its own machine's
// names before it forwards): not_found.
func (s *Service) parseTo(ctx context.Context, sub *authz.Subject, to string) (api.Address, error) {
	a, err := api.ParseAddress(to)
	machine, name, named := api.SessionNameRef(to)
	sn, resolves := orchcore.Sessions(s.d).(orchcore.SessionNames)
	switch {
	case !named || !resolves:
		return a, err
	case machine == "":
		return a, s.bareName(ctx, sub, sn, a, name)
	case machine != s.machine():
		return a, api.NotFound("session", to).WithHint("names of other machines' sessions are not resolved yet: use its address")
	}
	if ss, ok := sn.ResolveName(ctx, machine+"/"+name); ok {
		return api.ParseAddress(ss.Address)
	}
	return api.Address{}, api.NotFound("session", to)
}

// bareName refuses a bare name that is no machine anyone joined but a live
// session's name here that sub may see, with the address to use: a bare
// name is a machine's group mailbox (SPEC 7), and the sender meant the
// session.
func (s *Service) bareName(ctx context.Context, sub *authz.Subject, sn orchcore.SessionNames, a api.Address, name string) error {
	if a.Kind != api.KindMachine || a.Machine == s.machine() || len(s.d.Projects.MachineProjects(ctx, a.Machine)) > 0 {
		return nil
	}
	ss, ok := sn.ResolveName(ctx, name)
	if !ok || !s.d.Visible(ctx, sub.Scope, ss.Address) {
		return nil
	}
	return api.NotFound("machine", name).WithHint("did you mean the session " + s.machine() + "/" + name + "? A bare name is a machine's group mailbox")
}
