package orchcore

import (
	"context"

	"github.com/sengirunners/mezze/internal/orchapi"
)

// AskRunTargets is implemented by the team module: at a team
// project's authority, each machine of the project with whose it is (the
// identities that joined it) and whether an approved action may run there
// (SPEC 16 "Asks that carry an action"). The approvals module reads no team
// tables: it lists and defaults --run-on from this.
type AskRunTargets interface {
	AskRunTargets(ctx context.Context, project string) ([]orchapi.RunTarget, error)
}

// MachinePeople is implemented by the team module: at a
// team project's authority, the person whose own machine machine is (an
// identity name), or "" for a team machine. An action on a person's machine
// runs only on that person's signed yes (SPEC 16 "Asks that carry an
// action"); the approvals module reads no team tables.
type MachinePeople interface {
	MachinePerson(ctx context.Context, project, machine string) (string, error)
}
