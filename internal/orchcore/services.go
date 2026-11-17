package orchcore

import (
	"context"
	"sync"

	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

// Name is the service name zm's orchestration services are provided under
// (core.Deps.Provide).
const Name = "orch"

// Services is the orchestration modules' in-process APIs on one daemon,
// published on core.Deps through its service registry: the fields that
// left core.Deps. As with those fields, the wiring (or
// a test harness) sets them once before serving; modules read them at call
// time through the typed getters, never in constructors.
type Services struct {
	// Mail is the mail module's in-process API, or nil.
	Mail MailService
	// Sessions is the session module's in-process API, or nil.
	Sessions SessionService
	Work     WorkService
	// Homes is the tracker homes module (SPEC 17.9), or nil: the homes
	// module's side of the seam whose work module side is Trackers.
	Homes HomeService
	// Trackers is the work module's side of the homes seam (SPEC 17.9):
	// Homes is the other. The work and homes modules call each other
	// through them without checking for nil, so wiring.Full refuses to wire
	// one without the other; a daemon with neither leaves both nil.
	Trackers TrackerHost
	// Issues is the native issues module (SPEC "Native issues"), or nil.
	Issues IssueService
	// DevicePerson is the person an active approver device (by tailnet node
	// ID) acts for, by that person's own grants; ok false for any other node
	// (the approvals brick sets it; nil in a module's tests).
	DevicePerson func(ctx context.Context, node string) (sub *authz.Subject, ok bool)
	// DevicePush is the web brick's phone alerts for a paired device (nil
	// in a module's tests).
	DevicePush DevicePush
	// People is the approvals brick's person links: which
	// accounts are one person's (nil in a module's tests: each account is
	// its own person).
	People PersonAccounts
	// Team is the team module (internal/team), which sits on Potluck's
	// teamlink; modules assert it to the team seams (TeamReplica,
	// TeamLeases, TaskRelay, RemoteTasks, ...). nil: no team module, so no
	// seam answers. core.Deps.Team stays teamlink's proxy (Potluck's
	// TeamReplicas, TeamRepos and the swap's listers are teamlink's).
	Team any

	noticeMu       sync.Mutex
	noticeCounters []NoticeCounter // notices.go

	owedMu      sync.Mutex
	owedSources []OwedSource // owed.go
}

var wireMu sync.Mutex

// Wire is d's Services, provided on first use: the wiring and test
// harnesses set its fields. The first use also registers the hooks potluck's
// base asks these services through (hooks.go).
func Wire(d *core.Deps) *Services {
	wireMu.Lock()
	defer wireMu.Unlock()
	if s := of(d); s != nil {
		return s
	}
	s := &Services{}
	d.Provide(Name, s)
	wireHooks(d)
	return s
}

func of(d *core.Deps) *Services {
	s, _ := d.Service(Name).(*Services)
	return s
}

// Work is the work module's in-process API, or nil when none is wired.
func Work(d *core.Deps) WorkService {
	if s := of(d); s != nil {
		return s.Work
	}
	return nil
}

// Trackers is the work module's API for the homes module, or nil when none
// is wired.
func Trackers(d *core.Deps) TrackerHost {
	if s := of(d); s != nil {
		return s.Trackers
	}
	return nil
}

// Issues is the native issues module's in-process API, or nil when none is
// wired.
func Issues(d *core.Deps) IssueService {
	if s := of(d); s != nil {
		return s.Issues
	}
	return nil
}

// DevicePerson is the approvals brick's device lookup, or nil when none is
// wired.
func DevicePerson(d *core.Deps) func(ctx context.Context, node string) (sub *authz.Subject, ok bool) {
	if s := of(d); s != nil {
		return s.DevicePerson
	}
	return nil
}

// PersonAccounts says which human accounts are one person's. Addresses are
// human:<name>; both answer nil when the address is no linked account.
type PersonAccounts interface {
	// AccountsOf is the person's other accounts: whose private mail the
	// address may list when it is also that mail's participant.
	AccountsOf(ctx context.Context, addr string) []string
	// ShareMailWith is the other accounts mail to addr also goes to: only
	// when that account itself turned sharing on.
	ShareMailWith(ctx context.Context, addr string) []string
}

// People is the approvals brick's person links, or nil when none is wired.
func People(d *core.Deps) PersonAccounts {
	if s := of(d); s != nil {
		return s.People
	}
	return nil
}

// DevicePushOf is the web brick's phone alerts, or nil when none is
// wired (named apart from the DevicePush interface).
func DevicePushOf(d *core.Deps) DevicePush {
	if s := of(d); s != nil {
		return s.DevicePush
	}
	return nil
}

// Mail is the mail module's in-process API, or nil when none is wired.
func Mail(d *core.Deps) MailService {
	if s := of(d); s != nil {
		return s.Mail
	}
	return nil
}

// Sessions is the session module's in-process API, or nil when none is
// wired.
func Sessions(d *core.Deps) SessionService {
	if s := of(d); s != nil {
		return s.Sessions
	}
	return nil
}

// Team is the team module, or nil when none is wired: assert it to the
// team seam a caller needs (a nil Team asserts to none).
func Team(d *core.Deps) any {
	if s := of(d); s != nil {
		return s.Team
	}
	return nil
}

// Homes is the homes module's in-process API, or nil when none is wired.
func Homes(d *core.Deps) HomeService {
	if s := of(d); s != nil {
		return s.Homes
	}
	return nil
}
