package fulltest

import (
	"context"
	"testing"

	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/issuers"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	. "github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/teamlink"
	"github.com/sengirunners/potluck/runners"
	"github.com/sengirunners/potluck/runtarget"
)

// TestStartOrder: modules start in Full's order. teamlink loads the joins
// before run targets start (a host's slug-keyed shares and ledger move to
// project ids through them; without them relayed host calls are refused),
// and the secrets issuer starts after run targets and before the runner
// broker that issues through it (closing in reverse, the broker stops first).
// registry.db gets run targets' host-key table without a step of zm's.
// The team module sits on teamlink: d.Team stays teamlink's proxy and team
// is reached as orchcore.Team (Stage 1.4).
func TestStartOrder(t *testing.T) {
	home := shortHome(t)
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "mini"
	dm, err := New(context.Background(), cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	defer dm.Close()
	at := map[string]int{}
	for i, m := range dm.Modules() {
		var name string
		switch m.(type) {
		case *teamlink.Service:
			name = "teamlink"
		case *runtarget.Service:
			name = "runtarget"
		case *issuers.Module:
			name = "issuers"
		case *runners.Service:
			name = "runners"
		default:
			continue
		}
		if _, dup := at[name]; dup {
			t.Fatalf("%s wired twice", name)
		}
		at[name] = i
	}
	order := []string{"teamlink", "runtarget", "issuers", "runners"}
	for _, n := range order {
		if _, ok := at[n]; !ok {
			t.Fatalf("%s is not wired (modules: %v)", n, at)
		}
	}
	for i := 1; i < len(order); i++ {
		if at[order[i-1]] >= at[order[i]] {
			t.Errorf("%s (module %d) must start before %s (module %d)", order[i-1], at[order[i-1]], order[i], at[order[i]])
		}
	}
	if _, ok := orchcore.Team(dm.Deps).(*team.Service); !ok {
		t.Errorf("orchcore.Team is %T, not the team module", orchcore.Team(dm.Deps))
	}
	if _, ok := dm.Deps.Team.(core.ProxyLister); !ok {
		t.Errorf("d.Team is %T: not teamlink's proxy", dm.Deps.Team)
	}
	if _, ok := dm.Deps.Team.(orchcore.TaskRelay); ok {
		t.Errorf("d.Team is %T: the team module replaces teamlink there", dm.Deps.Team)
	}
	// Run targets' registry migration (the host-key mark) applies on its own.
	var n int
	if err := dm.Deps.Store.Registry().QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='run_host_keys'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("registry.db has no run_host_keys table: %d %v", n, err)
	}
}
