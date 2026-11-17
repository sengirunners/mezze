package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/secrets/inventory"
)

// zm doctor's key check: every secret key this home holds is listed in the
// key inventory (keys_inventory.go) and held where its class allows. An
// unlisted key, a key held below its class with no exception, a key file
// others can read and a literal key in config.json fail; a runner App key in
// a home that also runs agent sessions warns, since every agent session of
// this user can read it. It reads names, modes and config structure, never a key.

func init() {
	doctorChecks = append(doctorChecks, doctorCheck{"secret keys", doctorKeys})
}

func doctorKeys(ctx context.Context, env *doctorEnv) doctorResult {
	if env.Config == nil || env.ConfErr != nil {
		return doctorResult{Status: doctorSkip, Detail: "no home to check (see the home line)"}
	}
	if env.Remote {
		return doctorResult{Status: doctorSkip, Detail: "the daemon is elsewhere (ZM_HOST); its keys are on its own machine",
			Fix: "run zm doctor on the machine that runs the daemon"}
	}
	fi, err := os.Stat(env.Config.Home)
	if errors.Is(err, fs.ErrNotExist) {
		return doctorResult{Status: doctorSkip, Detail: "the home does not exist yet: no keys"}
	}
	if err != nil || !fi.IsDir() {
		if err == nil {
			err = errors.New("the home is not a directory")
		}
		return keysResult(inventory.Report{}, err, "")
	}
	agents, why := doctorRunsAgents(ctx, env)
	rep, err := inventory.Check(inventory.Home{Dir: env.Config.Home, RunsAgentSessions: agents}, keyInventory())
	return keysResult(rep, err, why)
}

// keysResult maps a check's report to a doctor result: a check that could not
// run fails (never a pass), a Fail finding fails, only warnings warn.
func keysResult(rep inventory.Report, err error, agentsNote string) doctorResult {
	if err != nil {
		return doctorResult{Status: doctorFail, Detail: "could not check the keys: " + err.Error(),
			Fix: "fix what the message names, then run zm doctor again"}
	}
	if fails := rep.Of(inventory.Fail); len(fails) > 0 {
		return doctorResult{Status: doctorFail, Detail: findingsText(fails), Fix: fixesText(fails)}
	}
	if warns := rep.Of(inventory.Warn); len(warns) > 0 {
		d := findingsText(warns)
		if agentsNote != "" {
			d += " (" + agentsNote + ")"
		}
		return doctorResult{Status: doctorWarn, Detail: d, Fix: fixesText(warns)}
	}
	return doctorResult{Status: doctorOK, Detail: fmt.Sprintf("%d keys held, all listed", len(rep.Held))}
}

// fixesText is every distinct fix, in order, so the first finding's does not hide the rest.
func fixesText(fs []inventory.Finding) string {
	var out []string
	for _, f := range fs {
		if f.Fix != "" && !slices.Contains(out, f.Fix) {
			out = append(out, f.Fix)
		}
	}
	return strings.Join(out, " ")
}

func findingsText(fs []inventory.Finding) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = f.Detail
	}
	return strings.Join(parts, "; ")
}

// doctorRunsAgents is whether agent sessions run in this home, asked of the
// session module: any live session of this machine that is not a plain shell
// (a team's list also carries other machines' sessions; those cannot read this
// home's keys). The machine's name is the daemon's own (the one its sessions
// carry), else the config's; with neither, every session counts. The list is
// asked for every project, whatever -P says. When the daemon cannot answer
// (down, or the list failed) or the page is full, it is unknown, and unknown
// counts as yes: a warning too many beats a silent one. note says so.
func doctorRunsAgents(ctx context.Context, env *doctorEnv) (runs bool, note string) {
	if env.Client == nil {
		return true, "the daemon did not answer, so agent sessions are assumed"
	}
	machine := env.Config.Machine
	if env.Health != nil && env.Health.Machine != "" {
		machine = env.Health.Machine
	}
	project, from := env.Client.Project, env.Client.ProjectFrom
	env.Client.Project, env.Client.ProjectFrom = "", ""
	l, err := orchclient.SessionPage(ctx, env.Client, orchapi.SessionQuery{})
	env.Client.Project, env.Client.ProjectFrom = project, from
	if err != nil {
		return true, "the session list failed, so agent sessions are assumed"
	}
	for _, s := range l.Sessions {
		if (machine == "" || s.Machine == machine) && s.Agent != "shell" && s.State != orchapi.StateEnded {
			return true, ""
		}
	}
	// A full page may hide one; skipped or behind shares are other machines' sessions.
	if l.Next != "" {
		return true, "the session list is incomplete, so agent sessions are assumed"
	}
	return false, ""
}
