package orchapi

import "time"

// Run targets (SPEC 16 "Asks that carry an action"): the machines
// of a team project an approved action may run on, and whose they are. Not
// generated: zm's own shape.

// RunTarget is one machine of a team project, at its authority.
type RunTarget struct {
	Machine string `json:"machine"`
	// People are the identities whose live node-bound tokens joined it
	// (the human of the authority's own machine for that one): whose machine
	// it is. Empty for a machine nobody's person joined.
	People []string `json:"people"`
	// Trusted: its operator class is trusted (only those run actions).
	Trusted bool `json:"trusted"`
	// RemoteTasks: it takes tasks from other machines; ScriptRuns: its zm
	// runs a --script action; CommandRuns: a --command action.
	RemoteTasks bool `json:"remote_tasks"`
	ScriptRuns  bool `json:"script_runs"`
	CommandRuns bool `json:"command_runs"`
	// OSUser is the account zm runs as there (the --user of --authorize-key).
	OSUser string     `json:"os_user,omitempty"`
	Seen   *time.Time `json:"seen,omitempty"`
	// Ready: trusted and takes remote tasks, so --run-on may name it. Why
	// says what keeps it from being ready, and who fixes it.
	Ready bool   `json:"ready"`
	Why   string `json:"why,omitempty"`
}

// RunTargetList is GET /v0/run-targets: the project's machines, or with
// ?person= that person's (Person is the identity the name resolved to).
type RunTargetList struct {
	Project string      `json:"project"`
	Person  string      `json:"person,omitempty"`
	Targets []RunTarget `json:"targets"`
	// Hint says what to do when no target is ready.
	Hint string `json:"hint,omitempty"`
}

// ReadyTargets are the ready ones of ts, in order.
func ReadyTargets(ts []RunTarget) []RunTarget {
	var out []RunTarget
	for _, t := range ts {
		if t.Ready {
			out = append(out, t)
		}
	}
	return out
}

// MachinePersonRequest is POST /v0/team/machines/{machine}/person: Person
// names the person whose own machine it is (an identity name), so an action
// there runs only on that person's signed yes; "" makes it a team machine
// again (either owner's yes).
type MachinePersonRequest struct {
	Project string `json:"project,omitempty"`
	Person  string `json:"person"`
}

// MachinePerson is one machine's person in a team project, at its
// authority: Person "" is a team machine.
type MachinePerson struct {
	Machine string     `json:"machine"`
	Person  string     `json:"person,omitempty"`
	SetBy   string     `json:"set_by,omitempty"`
	SetAt   *time.Time `json:"set_at,omitempty"`
}

// MachinePersonList is GET /v0/team/machines/person: the machines that are
// one person's own (a machine not listed is a team machine).
type MachinePersonList struct {
	Project  string          `json:"project"`
	Machines []MachinePerson `json:"machines"`
}
