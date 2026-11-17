package team_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/agentpipe"
	_ "github.com/sengirunners/mezze/internal/agentpipe/builtin"     // zm __zm-agent <driver>, as cmd/zm links it
	_ "github.com/sengirunners/mezze/internal/agentpipe/conformance" // the fake runtime (__zm-agent-fake)
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/team"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// spawnPipe starts a shared structured codex session on d in harbor whose
// runtime replays a recording (internal/agentpipe/testdata).
func spawnPipe(t *testing.T, d *daemon, rec string, shared bool) orchapi.Session {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(filepath.Join("..", "agentpipe", "testdata", rec))
	if err != nil {
		t.Fatal(err)
	}
	return mustf(orchclient.Spawn(context.Background(), at(d.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "codex", Command: []string{exe, agentpipe.FakeArg, abs},
		Cwd: t.TempDir(), Mode: orchapi.SessionModePipe, Shared: shared}))(t)
}

// beating heartbeats d every 50 ms until the test ends.
func beating(t *testing.T, d *daemon) {
	var s *team.Service
	for _, m := range d.dm.Modules() {
		if x, ok := m.(*team.Service); ok {
			s = x
		}
	}
	if s == nil {
		t.Fatal("no team module")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			s.Heartbeat(ctx)
			select {
			case <-ctx.Done():
			case <-time.After(50 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
}

// readUntil reads id's events through c until a turn ends, checking seqs.
func readUntil(t *testing.T, c *client.Client, id string, after *int64) []orchapi.AgentEvent {
	t.Helper()
	var seen []orchapi.AgentEvent
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		page, err := orchclient.SessionEvents(context.Background(), c, id, *after, 0, 2)
		if err != nil {
			t.Fatalf("events of %s: %v", id, err)
		}
		for _, raw := range page.Events {
			var ev orchapi.AgentEvent
			if err := json.Unmarshal(raw, &ev); err != nil {
				t.Fatal(err)
			}
			if ev.Seq != *after+1 {
				t.Fatalf("event seq %d after %d", ev.Seq, *after)
			}
			*after = ev.Seq
			seen = append(seen, ev)
			if ev.Type == agentpipe.EvTurn && agentpipe.Terminal(ev.Status) {
				return seen
			}
		}
	}
	t.Fatalf("no turn ended on %s; saw %+v", id, seen)
	return nil
}

func saidWord(evs []orchapi.AgentEvent) string {
	text := ""
	for _, ev := range evs {
		if ev.Type == agentpipe.EvMessage && ev.Role == "assistant" && ev.Status == agentpipe.ItemCompleted {
			text = ev.Text
		}
	}
	return text
}

// Structured sessions on other machines: the authority's
// owner drives a replica's shared structured session (turns run once on the
// replica, events come back through the heartbeat and the upload route); a
// member on the replica drives the authority's own one (forwarded). An
// agent session may not operate it; an unshared one stays on its machine.
func TestStructuredSessionAcrossMachines(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, roomy)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	r := startDaemonWith(t, tn, "bea-mac", tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea-mac", Login: "bea@example.com", Host: "bea-mac"}, discard, roomy)
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)

	cs := spawnPipe(t, r, "codex/turns.rec.jsonl", true) // on the replica
	if !cs.Shared || cs.Mode != orchapi.SessionModePipe {
		t.Fatalf("spawned: %+v", cs)
	}
	plain := spawnPipe(t, r, "codex/turns.rec.jsonl", false)
	beating(t, r)
	waitFor(t, "the replica's session at the authority", func() bool {
		ss, err := orchclient.Session(ctx, a.owner, cs.Address)
		return err == nil && ss.Mode == orchapi.SessionModePipe && ss.Capabilities != nil
	})

	// The authority's owner: two turns, each run once on the replica, the
	// events read back in order through the relay.
	var after int64
	res, err := orchclient.Turn(ctx, a.owner, cs.Address, orchapi.SessionTurnRequest{Text: "Reply with exactly the word: alpha"})
	if err != nil || (!res.OK && !res.Pending) {
		t.Fatalf("relayed turn: %+v %v", res, err)
	}
	if w := saidWord(readUntil(t, a.owner, cs.Address, &after)); w != "alpha" {
		t.Fatalf("first turn said %q", w)
	}
	if _, err := orchclient.Turn(ctx, a.owner, cs.Address, orchapi.SessionTurnRequest{Text: "x", Steer: true}); err == nil {
		t.Fatal("a steer with no running turn went through") // refused on the replica, the error relayed
	}
	if _, err := orchclient.Turn(ctx, a.owner, cs.Address, orchapi.SessionTurnRequest{Text: "x", Images: []string{"/etc/hosts"}}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("images from another machine: %v", err)
	}
	if _, err := orchclient.Turn(ctx, at(a.owner, "", "mini/agent001"), cs.Address, orchapi.SessionTurnRequest{Text: "x"}); err == nil {
		t.Fatal("an agent session operated a shared session")
	}
	res, err = orchclient.Turn(ctx, a.owner, cs.Address, orchapi.SessionTurnRequest{Text: "Now reply with exactly the word: beta"})
	if err != nil || (!res.OK && !res.Pending) {
		t.Fatalf("second relayed turn: %+v %v", res, err)
	}
	if w := saidWord(readUntil(t, a.owner, cs.Address, &after)); w != "beta" {
		t.Fatalf("second turn said %q", w)
	}
	// The replica's own view agrees, seq for seq.
	var local int64
	readUntil(t, r.owner, cs.ID, &local)
	if w := saidWord(readUntil(t, r.owner, cs.ID, &local)); w != "beta" || local != after {
		t.Fatalf("replica's own read: %q at %d, relayed at %d", w, local, after)
	}
	// Not shared: refused, never relayed.
	if _, err := orchclient.SessionEvents(ctx, a.owner, plain.Address, 0, 0, 0); err == nil {
		t.Fatal("an unshared structured session's events left its machine")
	}

	// A member on the replica drives the authority's own shared session.
	ms := spawnPipe(t, a, "codex/turns.rec.jsonl", true)
	var mafter int64
	if res, err := orchclient.Turn(ctx, r.owner, ms.Address, orchapi.SessionTurnRequest{Text: "Reply with exactly the word: alpha"}); err != nil || !res.OK {
		t.Fatalf("forwarded turn: %+v %v", res, err)
	}
	if w := saidWord(readUntil(t, r.owner, ms.Address, &mafter)); w != "alpha" {
		t.Fatalf("forwarded read said %q", w)
	}
}
