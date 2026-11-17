package team_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// From a member Mac, the project's owner starts the Mezze app on another machine and
// gets a one-time pairing credential, with nothing connecting to that
// machine (SPEC 8 "Mezze app on a machine"): the authority queues a t3_ensure op,
// the machine's heartbeat carries it, its answer comes back once. The
// authority keeps no credential after the answer (nor after a resent
// report), the machine none after the ack. A machine whose config.json has
// only "t3": {} runs it with defaults. A machine without a t3 key, a
// member who does not own the project, and an unnamed project are refused;
// presence shows the running server as stats.t3.
func TestT3OnAnotherMachine(t *testing.T) {
	quietPresence(t)
	defer team.ReportEveryBeat()()
	var mu sync.Mutex
	var asked []orchapi.T3EnsureOp
	n := 0
	defer team.FakeT3(func(_ context.Context, c config.T3, op orchapi.T3EnsureOp) (orchapi.T3Environment, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		asked = append(asked, op)
		return orchapi.T3Environment{EnvironmentID: "env-" + c.Home, Version: "0.1.7", Commit: c.Commit, Port: 3773,
			URLs: []string{"http://100.64.0.9:3773"}, Started: n == 1, Credential: fmt.Sprintf("cred-%d", n), Scopes: op.Scopes,
			ExpiresAt: time.Now().Add(5 * time.Minute)}, nil
	}, func(c config.T3) *api.MachineT3 {
		return &api.MachineT3{EnvironmentID: "env-" + c.Home, Version: "0.1.7", Port: 3773}
	})()
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	withT3 := func(home string) func(*config.Config) {
		return func(c *config.Config) {
			roomy(c)
			c.File.T3 = config.T3{Checkout: "/srv/t3code", Commit: strings.Repeat("a", 40), Home: home}
		}
	}
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, withT3("mini"))
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	join := func(machine, as string, mut func(*config.Config)) *daemon {
		d := startDaemonWith(t, tn, machine, tailnettest.Identity{NodeID: "n" + strings.ToUpper(machine), NodeKey: "nodekey:" + machine, Login: as + "@example.com", Host: machine}, discard, mut)
		inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: as, Role: "member"}))(t)
		mustf(d.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
		return d
	}
	box := join("box", "box", withT3("box"))
	// farm's owner set only the switch: "t3": {} in its config.json.
	farm := join("farm", "farm", func(c *config.Config) {
		roomy(c)
		if err := os.WriteFile(filepath.Join(c.Home, config.FileConfig), []byte(`{"t3": {}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	shy := join("shy", "shy", roomy) // no t3 key
	mac := join("mac", "ivogh", roomy)
	bea := join("bea-mac", "bea", roomy)
	mustf(a.owner.Grant(ctx, api.GrantRequest{Identity: "ivogh", Project: "harbor", Role: "owner"}))(t)
	for _, d := range []*daemon{box, farm, shy, mac, bea} {
		heartbeat(t, d)
	}

	// ensure asks from d's socket, beating box (farm, when asked) until the
	// answer comes.
	ensure := func(d *daemon, machine string, req orchapi.T3EnsureRequest) (orchapi.T3Environment, error) {
		beat := box
		if machine == "farm" {
			beat = farm
		}
		type res struct {
			env orchapi.T3Environment
			err error
		}
		ch := make(chan res, 1)
		go func() {
			env, err := orchclient.EnsureT3(ctx, d.owner, machine, req)
			ch <- res{env, err}
		}()
		for {
			select {
			case r := <-ch:
				return r.env, r.err
			case <-time.After(100 * time.Millisecond):
				heartbeat(t, beat)
			}
		}
	}

	env, err := ensure(mac, "box", orchapi.T3EnsureRequest{Project: "harbor", Scopes: []string{"orchestration:read", "terminal:read", "orchestration:read"}})
	if err != nil || env.Machine != "box" || env.Credential != "cred-1" || env.EnvironmentID != "env-box" || !env.Started || len(env.URLs) != 1 {
		t.Fatalf("ensure on box: %+v %v", env, err)
	}
	mu.Lock()
	if len(asked) != 1 || asked[0].Label != "zm: mac" || strings.Join(asked[0].Scopes, " ") != "orchestration:read terminal:read" {
		t.Fatalf("box was asked %+v", asked)
	}
	mu.Unlock()
	// Collected once: the authority keeps no credential.
	ops := team.T3Ops(teamService(t, a))
	if len(ops) != 1 || ops[0].State != "done" || ops[0].Result != "" {
		t.Fatalf("the authority's t3 ops after the answer: %+v", ops)
	}
	// A report box sends again (its ack lost) brings none back.
	team.ApplyReport(teamService(t, a), mustf(a.owner.GetProject(ctx, "harbor"))(t).ID, "box",
		api.MachineOpReport{ID: ops[0].ID, Payload: json.RawMessage(`{"credential":"cred-1"}`)})
	if ops := team.T3Ops(teamService(t, a)); ops[0].Result != "" {
		t.Fatalf("a resent report refilled the result: %+v", ops)
	}
	// Box drops its copy once an answer took it.
	eventually(t, 10*time.Second, func() error {
		heartbeat(t, box)
		runs := team.T3Runs(teamService(t, box))
		if len(runs) != 1 || runs[0].State != "acked" || runs[0].Result != "" {
			return fmt.Errorf("box's t3 runs: %+v", runs)
		}
		return nil
	})

	// Again: a fresh credential, the server reused.
	if env, err := ensure(mac, "box", orchapi.T3EnsureRequest{Project: "harbor"}); err != nil || env.Credential != "cred-2" || env.Started {
		t.Fatalf("second ensure: %+v %v", env, err)
	}
	// On the authority's own machine it runs at once.
	if env, err := ensure(mac, "mini", orchapi.T3EnsureRequest{Project: "harbor"}); err != nil || env.Machine != "mini" || env.EnvironmentID != "env-mini" {
		t.Fatalf("ensure on the authority: %+v %v", env, err)
	}
	mu.Lock()
	if asked[2].Label != "zm: mac" {
		t.Fatalf("the authority was asked %+v", asked[2])
	}
	mu.Unlock()

	// Presence: box's machine numbers carry its server.
	heartbeat(t, box)
	var l api.MachineList
	if err := mac.owner.Do(ctx, http.MethodGet, "/v0/machines", nil, &l); err != nil {
		t.Fatal(err)
	}
	seen := map[string]api.MachineStats{}
	for _, m := range l.Machines {
		seen[m.Machine] = m.Stats
	}
	if st := seen["box"]; !st.T3Enabled || st.T3 == nil || st.T3.EnvironmentID != "env-box" {
		t.Fatalf("box's numbers: %+v", st)
	}
	if st := seen["shy"]; st.T3Enabled || st.T3 != nil {
		t.Fatalf("shy's numbers: %+v", st)
	}
	if st := seen["farm"]; !st.T3Enabled {
		t.Fatalf("farm's numbers (\"t3\": {}): %+v", st)
	}

	// Refusals.
	if _, err := ensure(mac, "shy", orchapi.T3EnsureRequest{Project: "harbor"}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a machine without a t3 block: %v", err)
	}
	if _, err := ensure(mac, "nowhere", orchapi.T3EnsureRequest{Project: "harbor"}); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("an unknown machine: %v", err)
	}
	if _, err := ensure(bea, "box", orchapi.T3EnsureRequest{Project: "harbor"}); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("a member who does not own the project: %v", err)
	}
	if _, err := ensure(mac, "box", orchapi.T3EnsureRequest{}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("no project: %v", err)
	}
	if _, err := ensure(mac, "box", orchapi.T3EnsureRequest{Project: "harbor", Scopes: []string{"--base-dir=/tmp"}}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("a scope that is not one: %v", err)
	}
	mu.Lock()
	if len(asked) != 3 {
		t.Fatalf("a refused ensure reached a machine: %+v", asked)
	}
	mu.Unlock()

	// Box does not answer in time: the op closes with the wait, and a
	// report that comes later keeps no credential at the authority.
	longWait := team.ShortT3Wait(300 * time.Millisecond)
	if _, err := orchclient.EnsureT3(ctx, mac.owner, "box", orchapi.T3EnsureRequest{Project: "harbor"}); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("an ensure box never answered: %v", err)
	}
	ops = team.T3Ops(teamService(t, a))
	late := ops[len(ops)-1]
	if late.State != "done" || late.Result != "" {
		t.Fatalf("the op after its wait ended: %+v", late)
	}
	team.ApplyReport(teamService(t, a), mustf(a.owner.GetProject(ctx, "harbor"))(t).ID, "box",
		api.MachineOpReport{ID: late.ID, Payload: json.RawMessage(`{"credential":"cred-late"}`)})
	if ops := team.T3Ops(teamService(t, a)); ops[len(ops)-1].Result != "" {
		t.Fatalf("a late report filled the result: %+v", ops[len(ops)-1])
	}
	heartbeat(t, box) // nor does box get the closed op
	mu.Lock()
	if len(asked) != 3 {
		t.Fatalf("box ran an op nobody waits for: %+v", asked)
	}
	mu.Unlock()
	longWait()

	// farm runs it with every default (the empty block: the runtime fills
	// them, TestT3Defaults).
	if env, err := ensure(mac, "farm", orchapi.T3EnsureRequest{Project: "harbor"}); err != nil || env.Machine != "farm" || env.Credential != "cred-4" {
		t.Fatalf("ensure on farm: %+v %v", env, err)
	}
	mu.Lock()
	if len(asked) != 4 || asked[3].Label != "zm: mac" {
		t.Fatalf("farm was asked %+v", asked)
	}
	mu.Unlock()
	// The daemon filled farm's empty block with the default checkout, so
	// the tailnet's own config.json write kept the switch.
	var doc struct {
		T3 config.T3 `json:"t3"`
	}
	b, _ := os.ReadFile(filepath.Join(farm.cfg.Home, config.FileConfig))
	if json.Unmarshal(b, &doc) != nil || doc.T3.Checkout != filepath.Join(farm.cfg.Home, "t3code") {
		t.Fatalf("farm's config.json: %s", b)
	}
}
