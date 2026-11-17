// Package wiring decides which modules a daemon has: each func here is a
// server.Wiring that constructs modules, fills core.Deps and returns the
// modules in start order. The daemon shell (potluck/kit/server) imports no
// module; the binary picks a wiring (cmd/zm: Full). Full is zm: the shared
// base and compute wirings (Kit, Compute) live in potluck/wiring,
// which links no orchestration. Owned by the integrator. Adding a module =
// one line in Full.
package wiring

import (
	"github.com/sengirunners/mezze/internal/approvals"
	"github.com/sengirunners/mezze/internal/automations"
	"github.com/sengirunners/mezze/internal/connections"
	"github.com/sengirunners/mezze/internal/door"
	"github.com/sengirunners/mezze/internal/ghlink"
	"github.com/sengirunners/mezze/internal/homes"
	"github.com/sengirunners/mezze/internal/issues"
	"github.com/sengirunners/mezze/internal/mail"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/owed"
	"github.com/sengirunners/mezze/internal/search"
	"github.com/sengirunners/mezze/internal/secretkeys"
	"github.com/sengirunners/mezze/internal/session"
	"github.com/sengirunners/mezze/internal/summaries"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/mezze/internal/webhooks"
	"github.com/sengirunners/mezze/internal/webui"
	"github.com/sengirunners/mezze/internal/work"
	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/issuers"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/swap"
	"github.com/sengirunners/potluck/runners"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/usage"
	base "github.com/sengirunners/potluck/wiring"
)

// Full is every module zm has.
func Full(d *core.Deps) ([]core.Module, error) {
	k := base.NewKit(d)
	m, w, s, a, tm := mail.New(d), work.New(d), session.New(d), approvals.New(d), team.New(d, k.Link)
	o := orchcore.Wire(d)
	o.Mail, o.Sessions, o.Team = m, s, tm
	// d.Team stays Potluck's teamlink (NewKit set it); team sits on it
	// through its hooks and is reached as orchcore.Team. The session
	// safe-outputs gate goes in front of teamlink's proxy and the router.
	d.AddRequestGate(s.GateSafeOutputs)
	// Keys bound by a GitHub sign-in (approvals keybind.go): the owners
	// brick runs the login, approvals binds. Its gate comes after team's
	// origin gate, which marks a checked caller.
	d.KeyLogin = a.KeyLogin()
	d.AddRequestGate(a.GateKeyLogin)
	for _, p := range Probes {
		d.AddProbe(p.Name, p.Run)
	}
	o.Work = w
	iss := issues.New(d)
	o.Issues = iss
	d.AddSchema(swap.KindIssues, issues.Migrations)
	hm := homes.New(d)
	o.Homes, o.Trackers = hm, w
	d.Projects.SetIdentityGuard(a.IdentityNameFree)
	m.SetAsks(a) // an undecided ask's mail is answered only by its decision
	web := webui.New(d)
	o.DevicePerson, o.DevicePush, d.Aliases = a.DevicePerson, web, a.Aliases
	// A configured person link folds accounts wherever people fold (only
	// people folding reads LinkedAccount); a verified GitHub link still wins.
	o.People, d.LinkedAccount = a, a.LinkedPerson(d.LinkedAccount)
	dw := newDriverWatch(d)
	healthParts(d, dw)
	rn := runners.New(d)
	rn.Command = "zm" // the broker's hints and job reasons name zm's commands
	// The order is the start order and the reverse close order. homes comes
	// right after work: it starts after the mirrors and closes before them,
	// because an export backup reads the mirror.
	// teamlink loads the joined projects before team opens their copies, and
	// before run targets start: a host's shares and handle ledger are keyed
	// by project id, and run targets move slug-keyed files using the joins
	// teamlink loaded (TestStartOrder). The secrets issuer comes after run
	// targets (it hooks their ends; construct it after them) and before the
	// broker (it issues): modules close in reverse, so the broker stops first.
	rts := runtarget.New(d)
	return []core.Module{w, hm, s, m, k.Net, a, k.Link, tm, webhooks.New(d), connections.New(d), ghlink.New(d), web, iss, search.New(d), rts, issuers.NewModule(d), rn, usage.New(d), grants.New(d), secretkeys.New(d), summaries.New(d), owed.New(d), door.New(d), automations.New(d), k.Own, k.Tun, dw}, nil
}
