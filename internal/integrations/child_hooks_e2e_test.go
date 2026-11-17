package integrations_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/sengirunners/mezze/internal/integrations"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/server"
)

const parentRuntime = "aabbccdd-1111-parent"
const childRuntime = "aabbccdd-2222-child"

type hookFixture struct {
	t      *testing.T
	home   string
	owner  *client.Client
	parent orchapi.Session
	work   orchapi.WorkItem
}

// Exercise the public hook and claim routes against a real temporary daemon.
// The only managed process is a synthetic sleep; daemon cleanup ends it.
func newHookFixture(t *testing.T) *hookFixture {
	t.Helper()
	home, err := os.MkdirTemp("", "zm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	cfg, err := config.LoadFrom(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Human, cfg.Machine = "ana", "mini"
	dm, err := server.New(context.Background(), cfg, nil, wiring.Full)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dm.Close)
	for _, m := range dm.Modules() {
		if s, ok := m.(*session.Service); ok {
			s.Probe = func(...string) orchapi.AdmissionProbe {
				return orchapi.AdmissionProbe{MemoryFreePercent: 80, DiskFreeMB: 100000}
			}
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	c := client.WithHandler(dm.SocketHandler())
	ctx := context.Background()
	if _, err := c.CreateProject(ctx, api.NewProjectRequest{Slug: "hooks", RepoRules: api.RepoRules{Paths: []string{home}}}); err != nil {
		t.Fatal(err)
	}
	c.Project, c.ProjectFrom = "hooks", "flag"
	w, err := orchclient.CreateWork(ctx, c, orchapi.CreateWorkRequest{Title: "keep the parent claim"})
	if err != nil {
		t.Fatal(err)
	}
	parent, err := orchclient.Spawn(ctx, c, orchapi.SpawnRequest{Agent: "codex", Command: []string{"/bin/sh", "-c", "exec sleep 60"}, Cwd: home, Work: w.ID})
	if err != nil {
		t.Fatal(err)
	}
	return &hookFixture{t: t, home: home, owner: c, parent: parent, work: w}
}

func (f *hookFixture) hook(event string, payload map[string]string, runtimeEnv string) {
	f.t.Helper()
	payload["cwd"] = f.home
	b, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	env := map[string]string{"ZM_SESSION": f.parent.Address, "ZM_AS": f.parent.Address, "CODEX_THREAD_ID": runtimeEnv}
	_, err = integrations.RunHook(context.Background(), func(as, cwd string) integrations.Daemon {
		c := *f.owner
		c.As, c.Cwd = as, cwd
		return orchclient.Hooks(&c)
	}, integrations.HookInput{Agent: "codex", Event: event, Payload: b, Machine: "mini", Getenv: func(k string) string { return env[k] }})
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *hookFixture) assertParent(runtime string, state orchapi.SessionState) {
	f.t.Helper()
	ctx := context.Background()
	got, err := orchclient.Session(ctx, f.owner, f.parent.ID)
	if err != nil || !got.Managed || got.RuntimeID != runtime || got.State != state || got.Ended != nil {
		f.t.Fatalf("parent changed: %+v, %v", got, err)
	}
	ls, err := orchclient.Leases(ctx, f.owner, orchapi.LeaseQuery{})
	if err != nil || len(ls) != 1 || ls[0].Key != f.work.ID || ls[0].Holder != f.parent.Address || !ls[0].Bound {
		f.t.Fatalf("parent claim changed: %+v, %v", ls, err)
	}
	it, err := orchclient.GetWork(ctx, f.owner, f.work.ID)
	if err != nil || it.Status != orchapi.WorkClaimed {
		f.t.Fatalf("parent work lost its claim: %+v, %v", it, err)
	}
}

func (f *hookFixture) runtime(id string) orchapi.Session {
	f.t.Helper()
	ss, err := orchclient.Sessions(context.Background(), f.owner, orchapi.SessionQuery{All: true})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, s := range ss {
		if s.RuntimeID == id {
			return s
		}
	}
	f.t.Fatalf("runtime %q has no session: %+v", id, ss)
	return orchapi.Session{}
}

func TestChildHookCannotChangeManagedParent(t *testing.T) {
	for _, tc := range []struct {
		name, event, env string
		payload          map[string]string
	}{
		{"subagent stop inherits root runtime", integrations.HookStop, parentRuntime, map[string]string{"session_id": parentRuntime, "agent_id": childRuntime}},
		{"subagent stop has own runtime environment", integrations.HookStop, childRuntime, map[string]string{"session_id": parentRuntime, "agent_id": childRuntime}},
		{"notify contradicts runtime environment", integrations.HookNotify, parentRuntime, map[string]string{"thread-id": childRuntime, "type": "agent-turn-complete"}},
		{"notify has child runtime environment", integrations.HookNotify, childRuntime, map[string]string{"thread-id": childRuntime, "type": "agent-turn-complete"}},
		{"subagent end", integrations.HookSessionEnd, childRuntime, map[string]string{"session_id": parentRuntime, "agent_id": childRuntime}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookFixture(t)
			f.hook(integrations.HookSessionStart, map[string]string{"session_id": parentRuntime}, parentRuntime)
			f.hook(integrations.HookTool, map[string]string{"session_id": parentRuntime}, parentRuntime)
			f.assertParent(parentRuntime, orchapi.StateRunning)
			f.hook(tc.event, tc.payload, tc.env)
			f.assertParent(parentRuntime, orchapi.StateRunning)
			child := f.runtime(childRuntime)
			want := orchapi.StateIdle
			if tc.event == integrations.HookSessionEnd {
				want = orchapi.StateEnded
			}
			if child.Address == f.parent.Address || child.Managed || child.State != want {
				t.Fatalf("child not isolated: %+v", child)
			}
			// A genuine root stop must still update that root, not a new row.
			f.hook(integrations.HookStop, map[string]string{"session_id": parentRuntime}, parentRuntime)
			f.assertParent(parentRuntime, orchapi.StateIdle)
		})
	}
}

func TestChildStopCannotTakeUnboundManagedRuntime(t *testing.T) {
	f := newHookFixture(t)
	f.hook(integrations.HookStop, map[string]string{"session_id": parentRuntime, "agent_id": childRuntime}, childRuntime)
	f.assertParent("", orchapi.StateRunning)
	child := f.runtime(childRuntime)
	if child.Managed || child.Address == f.parent.Address || child.State != orchapi.StateIdle {
		t.Fatalf("child took parent before its first hook: %+v", child)
	}
	f.hook(integrations.HookSessionStart, map[string]string{"session_id": parentRuntime}, parentRuntime)
	f.assertParent(parentRuntime, orchapi.StateIdle)
}

func TestFirstChildNotifyCannotTakeUnboundManagedRuntime(t *testing.T) {
	for _, oldClient := range []bool{false, true} {
		t.Run(map[bool]string{false: "current hook client", true: "old hook route client"}[oldClient], func(t *testing.T) {
			f := newHookFixture(t)
			if oldClient {
				c := *f.owner
				c.As = f.parent.Address
				if _, err := orchclient.Hook(context.Background(), &c, orchapi.HookRequest{RegisterRequest: orchapi.RegisterRequest{
					Agent: "codex", RuntimeID: childRuntime, Session: f.parent.Address, State: orchapi.StateIdle}}); err != nil {
					t.Fatal(err)
				}
			} else {
				f.hook(integrations.HookNotify, map[string]string{"thread-id": childRuntime, "type": "agent-turn-complete"}, childRuntime)
			}
			f.assertParent("", orchapi.StateRunning)
			child := f.runtime(childRuntime)
			if child.Managed || child.Address == f.parent.Address || child.State != orchapi.StateIdle {
				t.Fatalf("first notify took parent: %+v", child)
			}
			f.hook(integrations.HookSessionStart, map[string]string{"session_id": parentRuntime}, parentRuntime)
			f.assertParent(parentRuntime, orchapi.StateIdle)
		})
	}
}

func TestManagedHookOverrideRequiresSameAgent(t *testing.T) {
	f := newHookFixture(t)
	c := *f.owner
	c.As = f.parent.Address
	child, err := orchclient.Register(context.Background(), &c, orchapi.RegisterRequest{
		Agent: "claude", RuntimeID: childRuntime, Session: f.parent.Address, State: orchapi.StateIdle})
	if err != nil || child.Address == f.parent.Address || child.Managed || child.Agent != "claude" {
		t.Fatalf("cross-agent override adopted parent: %+v, %v", child, err)
	}
	f.assertParent("", orchapi.StateRunning)
	f.hook(integrations.HookTool, map[string]string{"session_id": parentRuntime}, parentRuntime)
	f.assertParent(parentRuntime, orchapi.StateRunning)
}

// An older client can still send the inherited Session override. The daemon
// must route by runtime identity instead of trusting that stale hint.
func TestStaleManagedHookOverrideCannotStealRuntime(t *testing.T) {
	for _, boundParent := range []bool{false, true} {
		t.Run(map[bool]string{false: "unbound parent and known child", true: "bound parent and new child"}[boundParent], func(t *testing.T) {
			f := newHookFixture(t)
			wantParentRuntime := ""
			if boundParent {
				f.hook(integrations.HookTool, map[string]string{"session_id": parentRuntime}, parentRuntime)
				wantParentRuntime = parentRuntime
			} else {
				if _, err := orchclient.Register(context.Background(), f.owner, orchapi.RegisterRequest{Agent: "codex", RuntimeID: childRuntime, State: orchapi.StateRunning}); err != nil {
					t.Fatal(err)
				}
			}
			c := *f.owner
			c.As = f.parent.Address
			got, err := orchclient.Register(context.Background(), &c, orchapi.RegisterRequest{
				Agent: "codex", RuntimeID: childRuntime, Session: f.parent.Address, State: orchapi.StateIdle})
			if err != nil || got.Address == f.parent.Address || got.RuntimeID != childRuntime || got.Managed {
				t.Fatalf("stale managed override stole runtime: %+v, %v", got, err)
			}
			f.assertParent(wantParentRuntime, orchapi.StateRunning)
		})
	}
}
