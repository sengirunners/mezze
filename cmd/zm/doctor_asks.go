package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
)

// zm doctor's ask check: who can answer this machine's asks. A project this
// daemon decides and this machine owns, with no active paired device and no
// active key approver, has asks that any process running as the owner's OS
// user can answer through the socket (SPEC section 16, README "Asks: who can
// answer"). That is the default and it is allowed, so this only warns, with
// the command that pairs a device. A project joined from a team is decided at
// its authority, not here, so it is left out. Read-only: names and states only.

func init() {
	doctorChecks = append(doctorChecks, doctorCheck{"asks", doctorAsks})
}

func doctorAsks(ctx context.Context, env *doctorEnv) doctorResult {
	if env.Config == nil || env.ConfErr != nil {
		return doctorResult{Status: doctorSkip, Detail: "no home to check (see the home line)"}
	}
	if env.Remote {
		return doctorResult{Status: doctorSkip, Detail: "the daemon is elsewhere (ZM_HOST); its asks are decided on its own machine",
			Fix: "run zm doctor on the machine that runs the daemon"}
	}
	if env.Client == nil {
		return doctorResult{Status: doctorSkip, Detail: "needs the daemon (see the daemon line)"}
	}
	// Asked as this machine's owner on its socket, from a copy of the client:
	// not as a session's address (ZM_AS), a token, a project (-P) or a
	// directory (which routes to a joined project's authority); any of them
	// could hide an owned project or answer for another place and pass.
	rc := *env.Client
	rc.Project, rc.ProjectFrom, rc.As, rc.Token, rc.Cwd = "", "", "", "", ""
	cl := &rc

	projects, err := cl.LocalProjects(ctx)
	if err != nil {
		return asksUnknown(err)
	}
	remotes, err := cl.Remotes(ctx)
	if err != nil {
		return asksUnknown(err)
	}
	owned := ownedHere(projects, remotes)
	// The owner's own approvers, as the daemon filters them (a device paired
	// for another person is not in this list), not by a name this process guesses.
	mine, err := orchclient.MyApprovers(ctx, cl)
	if err != nil {
		return asksUnknown(err)
	}
	return asksResult(owned, mine.Approvers)
}

// ownedHere is the slugs of the projects this machine owns and this daemon
// decides: a project joined from a team is decided at its authority. Joined
// ones are matched by id only: a local project may share a joined one's slug.
func ownedHere(projects []api.Project, remotes []api.Remote) []string {
	joined := map[string]bool{}
	for _, rm := range remotes {
		joined[rm.Project.ID] = true
	}
	var owned []string
	for _, p := range projects {
		if p.Role == api.RoleOwner && !joined[p.ID] {
			owned = append(owned, p.Slug)
		}
	}
	return owned
}

// asksUnknown is a check that could not run: said so, never a pass.
func asksUnknown(err error) doctorResult {
	return doctorResult{Status: doctorWarn, Detail: "could not check who can answer asks: " + err.Error(),
		Fix: "zm status shows what is wrong; then run zm doctor again"}
}

// asksResult judges the owned projects against the owner's approvers. Only an
// active one counts: a pending code or an unconfirmed device does not decide
// anything yet. An approver adds a stronger way to answer; the socket still
// takes the owner's answers.
func asksResult(owned []string, approvers []orchapi.Approver) doctorResult {
	if len(owned) == 0 {
		return doctorResult{Status: doctorOK, Detail: "no project here is owned by this machine"}
	}
	active, waiting := 0, 0
	for _, a := range approvers {
		switch a.State {
		case orchapi.ApproverActive:
			active++
		case orchapi.ApproverPending, orchapi.ApproverUnconfirmed:
			waiting++
		}
	}
	if active > 0 {
		return doctorResult{Status: doctorOK, Detail: fmt.Sprintf("%d paired approver(s) can answer the asks of %s with a signed answer", active, projectsText(owned))}
	}
	d := fmt.Sprintf("no paired device or key approver: any process running as your OS user can answer the asks of %s", projectsText(owned))
	fix := "zm approver add <name> prints a code to pair a device on your tailnet; an answer from a paired device is provably a person's"
	if waiting > 0 {
		d += fmt.Sprintf(" (%d pairing(s) not finished)", waiting)
		fix += "; zm approver ls shows the unfinished one(s)"
	}
	return doctorResult{Status: doctorWarn, Detail: d, Fix: fix}
}

// projectsText names up to three projects, then counts the rest.
func projectsText(slugs []string) string {
	if len(slugs) == 1 {
		return "project " + slugs[0]
	}
	if len(slugs) <= 3 {
		return "projects " + strings.Join(slugs, ", ")
	}
	return fmt.Sprintf("projects %s and %d more", strings.Join(slugs[:3], ", "), len(slugs)-3)
}
