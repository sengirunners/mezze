package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/events"
)

func parentOf(t *testing.T, h *harness, id string) (one, inList string) {
	t.Helper()
	ctx := context.Background()
	ss, err := orchclient.Session(ctx, h.c, id)
	if err != nil {
		t.Fatal(err)
	}
	list, err := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{All: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range list {
		if x.ID == id {
			return ss.Parent, x.Parent
		}
	}
	t.Fatalf("%s not listed", id)
	return "", ""
}

// A session started from inside another, by zm run or task_start, names it on
// the read and in the list; a top-level one has no parent.
func TestParentOfSpawnAndTask(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	if one, list := parentOf(t, h, parent.ID); one != "" || list != "" {
		t.Fatalf("top-level session has a parent: %q %q", one, list)
	}
	taskClient(h, parent)
	ctx := context.Background()
	child, err := orchclient.Spawn(ctx, h.c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "sleep 30"}, Cwd: h.home})
	if err != nil {
		t.Fatal(err)
	}
	if child.Parent != parent.Address {
		t.Fatalf("spawn answer parent %q", child.Parent)
	}
	no := false
	task, err := orchclient.TaskStart(ctx, h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "a", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	h.c.As = ""
	if one, list := parentOf(t, h, child.ID); one != parent.Address || list != parent.Address {
		t.Fatalf("spawned child parent %q %q", one, list)
	}
	a, _ := api.ParseAddress(task.Session)
	if one, list := parentOf(t, h, a.Session); one != parent.Address || list != parent.Address {
		t.Fatalf("task parent %q %q", one, list)
	}
}

// An agent that registers itself below a top-level session is its child, at
// the parent's depth (0: self-registration never deepens a task chain).
func TestParentOfSelfRegisteredUnderTopLevel(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	ctx := core.WithSubject(context.Background(), h.d.Projects.Local(parent.Address))
	reg, _, err := h.s.Register(ctx, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "parent-self-reg", Cwd: h.home})
	if err != nil {
		t.Fatal(err)
	}
	if one, list := parentOf(t, h, reg.ID); one != parent.Address || list != parent.Address {
		t.Fatalf("self-registered parent %q %q", one, list)
	}
	var depth int
	if err := h.d.Store.Registry().QueryRow(`SELECT depth FROM session_lineage WHERE id=?`, reg.ID).Scan(&depth); err != nil || depth != 0 {
		t.Fatalf("depth %d: %v", depth, err)
	}
	// A task below it starts at depth 1, as it would with no row at all.
	sub := h.d.Projects.Local(reg.Address)
	if p, d, err := h.s.spawnLineage(core.WithSubject(context.Background(), sub), sub, nil); err != nil || p != reg.Address || d != 1 {
		t.Fatalf("lineage below it: %q %d %v", p, d, err)
	}
}

// A parent the reader cannot see is left out, exactly like no parent.
func TestParentHiddenFromAReaderOfTheChildOnly(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	shown, hidden := h.project("shown"), h.project("hidden")
	beat := []orchapi.PresenceSession{
		{Address: "bea-mac/aaaaaaaa", Agent: "claude", State: orchapi.StateRunning, Started: time.Now()},
	}
	if err := h.s.RecordPresence(ctx, hidden.ID, "bea-mac", "bea", beat); err != nil {
		t.Fatal(err)
	}
	beat = []orchapi.PresenceSession{
		{Address: "bea-mac/bbbbbbbb", Agent: "codex", State: orchapi.StateRunning, Started: time.Now(), Parent: "bea-mac/aaaaaaaa"},
	}
	if err := h.s.RecordPresence(ctx, shown.ID, "bea-mac", "bea", beat); err != nil {
		t.Fatal(err)
	}
	whole := core.WithSubject(ctx, &authz.Subject{Scope: authz.All()})
	got, ok := h.s.loadPresence(ctx, "bea-mac/bbbbbbbb")
	if !ok {
		t.Fatal("no presence row")
	}
	if g := h.s.dress(whole, got); g.Parent != "bea-mac/aaaaaaaa" {
		t.Fatalf("owner sees parent %q", g.Parent)
	}
	member := &authz.Subject{Scope: authz.FromTuples([]authz.Tuple{{Relation: authz.Member, Object: authz.ProjectObject(shown.ID)}}, nil)}
	mctx := core.WithSubject(ctx, member)
	if g := h.s.dress(mctx, got); g.Parent != "" {
		t.Fatalf("member of the child's project only sees parent %q", g.Parent)
	}
	list, err := h.s.List(mctx, member.Scope, orchapi.SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range list {
		if strings.Contains(x.Address, "aaaaaaaa") || strings.Contains(x.Parent, "aaaaaaaa") {
			t.Fatalf("hidden session in the list: %+v", x)
		}
	}
}

// A replica speaks only for its own machine, and a beat without a parent (an
// older replica) keeps the one recorded.
func TestPresenceParentRules(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	p := h.project("harbor")
	send := func(parent string) {
		t.Helper()
		if err := h.s.RecordPresence(ctx, p.ID, "bea-mac", "bea", []orchapi.PresenceSession{
			{Address: "bea-mac/bbbbbbbb", Agent: "codex", State: orchapi.StateRunning, Started: time.Now(), Parent: parent}}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() string {
		got, ok := h.s.loadPresence(ctx, "bea-mac/bbbbbbbb")
		if !ok {
			t.Fatal("no presence row")
		}
		return h.s.dress(ctx, got).Parent
	}
	send("mini/aaaaaaaa")
	if got := read(); got != "" {
		t.Fatalf("a parent on another machine was kept: %q", got)
	}
	send("bea-mac/aaaaaaaa")
	if got := read(); got != "bea-mac/aaaaaaaa" {
		t.Fatalf("parent %q", got)
	}
	send("")
	if got := read(); got != "bea-mac/aaaaaaaa" {
		t.Fatalf("an older beat wiped the parent: %q", got)
	}
}

// An event cannot check each reader's view of the parent: only this machine's
// owner gets it there; a member reads it from the list.
func TestParentInEventsIsOwnerOnly(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	p := h.project("harbor")
	const addr = "bea-mac/bbbbbbbb"
	if err := h.s.RecordPresence(ctx, p.ID, "bea-mac", "bea", []orchapi.PresenceSession{
		{Address: addr, Agent: "codex", State: orchapi.StateRunning, Started: time.Now(), Parent: "bea-mac/aaaaaaaa"}}); err != nil {
		t.Fatal(err)
	}
	member := &authz.Subject{Scope: authz.FromTuples([]authz.Tuple{{Relation: authz.Member, Object: authz.ProjectObject(p.ID)}}, nil)}
	evs, err := h.d.Events.Since(ctx, member, "", events.Filter{Subject: addr}, 0)
	if err != nil || len(evs) == 0 {
		t.Fatalf("events %v %v", evs, err)
	}
	for _, e := range evs {
		if strings.Contains(string(e.Data), `"parent"`) {
			t.Fatalf("a member's event carries the parent: %s", e.Data)
		}
	}
	own, err := h.d.Events.Since(ctx, h.owner(), "", events.Filter{Subject: addr}, 0)
	if err != nil || len(own) == 0 || !strings.Contains(string(own[0].Data), `"parent":"bea-mac/aaaaaaaa"`) {
		t.Fatalf("the owner's event lost the parent: %v %v", own, err)
	}
}

// Raw lookups, which other modules return to callers after their own checks,
// never carry a parent: only dress decides who sees it.
func TestLookupCarriesNoParent(t *testing.T) {
	h, parent := taskHarness(t, `exec sleep 30`)
	taskClient(h, parent)
	child, err := orchclient.Spawn(context.Background(), h.c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "sleep 30"}, Cwd: h.home})
	if err != nil {
		t.Fatal(err)
	}
	h.c.As = ""
	raw, ok := h.s.Lookup(context.Background(), child.Address)
	if !ok || raw.Parent != "" {
		t.Fatalf("lookup %+v %v", raw.Parent, ok)
	}
}
