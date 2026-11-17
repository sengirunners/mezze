// Package storetest opens a test daemon's store the way a wired daemon has it:
// Potluck's modules give their tables through the wiring (store.AddMigrations),
// and zm's own modules, whose New adds theirs the same way, extend some of them
// (team/0020 alters teamlink's team_task_refs; mail reads grants'
// secret_grants). A test that builds core.Deps by hand opens its store here
// instead of store.Open, so those tables exist before zm's migrations run.
package storetest

import (
	"context"
	"slices"

	"github.com/sengirunners/potluck/grants"
	"github.com/sengirunners/potluck/issuers"
	"github.com/sengirunners/potluck/kit/owners"
	"github.com/sengirunners/potluck/kit/store"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runners"
	"github.com/sengirunners/potluck/runtarget"
	"github.com/sengirunners/potluck/usage"
)

// Migrations are the Potluck modules' migrations a Compute daemon's wiring adds.
var Migrations = slices.Concat(owners.Migrations, teamlink.Migrations, runtarget.Migrations, runners.Migrations,
	issuers.Migrations, usage.Migrations, grants.Migrations)

// Open opens (creating) the store at home with Migrations, more (a zm
// module's, for a test that opens its tables without its New) and every
// migration registered from init() (Potluck's base), applied in one
// name-ordered pass.
func Open(ctx context.Context, home string, more ...store.Migration) (*store.Store, error) {
	st, err := store.OpenForWiring(ctx, home)
	if err != nil {
		return nil, err
	}
	if err := st.AddMigrations(ctx, slices.Concat(Migrations, more)...); err != nil {
		st.Close()
		return nil, err
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}
