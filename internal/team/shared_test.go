package team_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/wiring"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/server"
	"github.com/sengirunners/potluck/kit/tailnet"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
)

// roomy lets spawns through whatever this machine's memory looks like.
func roomy(c *config.Config) {
	c.File.Admission = config.Admission{MinMemoryFreePercent: 1, MaxSwapUsedMB: 1 << 40, MinDiskFreeMB: 1}
}

func prompt(t *testing.T, c *client.Client, id, text string) (orchapi.SessionPrompt, error) {
	t.Helper()
	var p orchapi.SessionPrompt
	err := c.Do(context.Background(), http.MethodPost, "/v0/sessions/"+client.Seg(id)+"/prompt", orchapi.PromptRequest{Text: text}, &p)
	return p, err
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// These fixtures capture raw PTY input. A shared prompt stages exactly its
// text; delivery/ack does not authorize an Enter or any other extra bytes.
func waitStaged(t *testing.T, path, want string) {
	t.Helper()
	waitFor(t, "exact staged text without Enter", func() bool {
		b, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		if strings.ContainsAny(string(b), "\r\n") {
			t.Fatalf("shared prompt submitted Enter: %q", b)
		}
		return string(b) == want
	})
}

func assertStaged(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(string(b), "\r\n") || string(b) != want {
		t.Fatalf("staged PTY input = %q, want exactly %q without Enter", b, want)
	}
}

// Installed before joining, as with issueStallTransport: normal authenticated
// traffic is untouched, but an armed gate loses ack-bearing presence before
// it reaches the authority. Staging's immediate heartbeat kick cannot race
// the restart-before-ack fixture. No timer or production behavior is changed.
type sharedAckGate struct {
	armed   atomic.Bool
	blocked atomic.Uint64
}

type sharedAckTransport struct {
	altTransport
	gate *sharedAckGate
}

func (s sharedAckTransport) Client(target core.JoinTarget, opts core.ClientOptions) *http.Client {
	hc := *s.TeamTransport.Client(target, opts)
	hc.Transport = sharedAckRoundTripper{RoundTripper: hc.Transport, gate: s.gate}
	return &hc
}

type sharedAckRoundTripper struct {
	http.RoundTripper
	gate *sharedAckGate
}

func (s sharedAckRoundTripper) CloseIdleConnections() {
	if t, ok := s.RoundTripper.(interface{ CloseIdleConnections() }); ok {
		t.CloseIdleConnections()
	}
}

func (s sharedAckRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if s.gate.armed.Load() && req.Method == http.MethodPost && req.URL.Path == "/v0/team/presence" {
		body, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		var beat api.PresenceRequest
		if err := json.Unmarshal(body, &beat); err != nil {
			return nil, err
		}
		var presence orchapi.SessionsBeat
		if err := json.Unmarshal(beat.Parts["sessions"], &presence); err != nil {
			return nil, err
		}
		for _, ss := range presence.Sessions {
			if len(ss.Delivered) > 0 {
				s.gate.blocked.Add(1)
				return nil, errors.New("fixture: delivery ack held before authority")
			}
		}
	}
	return s.RoundTripper.RoundTrip(req)
}

// Like daemon.restart, retaining the same home and real session lifecycle,
// with the test-only transport registered before Serve loads saved remotes.
func restartSharedWithAckGate(t *testing.T, r *daemon, tn *tailnettest.Net, id tailnettest.Identity, gate *sharedAckGate) {
	t.Helper()
	r.halt()
	ctx, cancel := context.WithCancel(context.Background())
	dm, err := server.New(ctx, r.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), wiring.Full)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	for _, m := range dm.Modules() {
		if net, ok := m.(*tailnet.Service); ok {
			net.NewNode = tn.NodeFunc(id)
		}
	}
	dm.Deps.AddTransport(sharedAckTransport{altTransport: altTransport{dm.Deps.Transport(core.ViaTailnet)}, gate: gate})
	done := make(chan error, 1)
	go func() { done <- dm.Serve(ctx) }()
	r.dm, r.owner = dm, client.WithHandler(dm.SocketHandler())
	r.halt = func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("daemon %s did not stop", r.cfg.Machine)
		}
	}
	waitFor(t, "restarted socket", func() bool { _, err := r.sock.Health(context.Background()); return err == nil })
	st := mustf(r.owner.TailnetUp(context.Background(), api.TailnetUpRequest{}))(t)
	if !st.Running {
		t.Fatal("restarted fake tailnet is not running", st)
	}
}

// Shared sessions across machines (SPEC 17.10): the authority's owner
// prompts a replica's shared session (logged at the authority, pending until
// the replica's heartbeat carries it, then staged without Enter and marked
// delivered); a member on the replica prompts the authority's shared session
// (forwarded, typed at once). Both logs live at the authority.
func TestSharedSessionAcrossMachines(t *testing.T) {
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

	script := `stty raw -echo; printf '\033[?2004h'; exec cat > "$ZM_SCRATCH/in"`
	dir := t.TempDir()
	spawn := func(d *daemon) orchapi.Session {
		t.Helper()
		ss := mustf(orchclient.Spawn(ctx, at(d.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "claude", Command: []string{"/bin/sh", "-c", script},
			Cwd: dir, Shared: true, Env: map[string]string{"PATH": os.Getenv("PATH")}}))(t)
		if !ss.Shared {
			t.Fatalf("not shared: %+v", ss)
		}
		return ss
	}
	cs := spawn(r) // on the replica
	ms := spawn(a) // on the authority
	heartbeat(t, r)

	// The authority's owner prompts the replica's session: pending, then the
	// replica's heartbeat types it, the next one marks it delivered.
	pr, err := prompt(t, a.owner, cs.Address, "rerun the solver")
	if err != nil || pr.State != orchapi.PromptPending {
		t.Fatalf("prompt to the replica's session: %+v %v", pr, err)
	}
	heartbeat(t, r) // carries it; the replica types it
	waitStaged(t, filepath.Join(cs.Scratch, "in"), "rerun the solver")
	heartbeat(t, r) // reports it delivered
	var l orchapi.PromptList
	if err := a.owner.Do(ctx, http.MethodGet, "/v0/sessions/"+client.Seg(cs.Address)+"/prompts", nil, &l); err != nil ||
		len(l.Prompts) != 1 || l.Prompts[0].State != orchapi.PromptDelivered || l.Prompts[0].Who != "mini-human" {
		t.Fatalf("authority log: %+v %v", l, err)
	}
	heartbeat(t, r) // nothing pending: typed once only
	time.Sleep(200 * time.Millisecond)
	if b, _ := os.ReadFile(filepath.Join(cs.Scratch, "in")); strings.Count(string(b), "rerun the solver") != 1 {
		t.Fatalf("typed %d times", strings.Count(string(b), "rerun the solver"))
	}
	assertStaged(t, filepath.Join(cs.Scratch, "in"), "rerun the solver")
	// The replica's member reads the same log through its daemon.
	var rl orchapi.PromptList
	if err := r.owner.Do(ctx, http.MethodGet, "/v0/sessions/"+client.Seg(cs.ID)+"/prompts", nil, &rl); err != nil || len(rl.Prompts) != 1 {
		t.Fatalf("replica log read: %+v %v", rl, err)
	}

	// A member on the replica prompts the authority's session: forwarded,
	// typed at once, logged with the member as sender.
	pr, err = prompt(t, r.owner, ms.Address, "check the queue")
	if err != nil || pr.State != orchapi.PromptDelivered || pr.Who != "bea" {
		t.Fatalf("prompt to the authority's session: %+v %v", pr, err)
	}
	waitStaged(t, filepath.Join(ms.Scratch, "in"), "check the queue")
	// An agent session on the replica may not prompt (refused there, and at
	// the authority by the forwarded Zm-As).
	if _, err := prompt(t, at(r.owner, "", cs.Address), ms.Address, "agent to agent"); !api.IsCode(err, api.CodeForbidden) {
		t.Fatalf("an agent session's prompt via the replica: %v", err)
	}
	// And the replica's own shared session, by its session8 on the replica:
	// still through the authority (the log is there).
	pr, err = prompt(t, r.owner, cs.ID, "own session")
	if err != nil || pr.State != orchapi.PromptPending {
		t.Fatalf("prompt to its own session: %+v %v", pr, err)
	}
	heartbeat(t, r)
	waitStaged(t, filepath.Join(cs.Scratch, "in"), "rerun the solverown session")
	// A prompt pending when its session ends expires.
	pr, err = prompt(t, a.owner, cs.Address, "too late")
	if err != nil {
		t.Fatal(err)
	}
	mustf(orchclient.Stop(ctx, r.owner, cs.ID, orchapi.StopRequest{Signal: "KILL"}))(t)
	heartbeat(t, r) // the session is gone from presence: ended there
	if err := a.owner.Do(ctx, http.MethodGet, "/v0/sessions/"+client.Seg(cs.Address)+"/prompts", nil, &l); err != nil || l.Prompts[0].State != orchapi.PromptExpired {
		t.Fatalf("after the end: %+v %v", l.Prompts, err)
	}
	assertStaged(t, filepath.Join(cs.Scratch, "in"), "rerun the solverown session")
	assertStaged(t, filepath.Join(ms.Scratch, "in"), "check the queue")
}

// Delivered once typed (SPEC 17.10): a prompt typed into a session that ends before
// the next heartbeat is still reported delivered (not expired); and after
// the replica's daemon restarts between typing and acking, the restarted
// daemon still reports it delivered and never types it again.
func TestSharedSessionAckAfterEndAndRestart(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := startDaemonWith(t, tn, "mini", tailnettest.Identity{NodeID: "nMINI", NodeKey: "nodekey:mini", Login: "ivo@example.com", Host: "mini"}, discard, roomy)
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "harbor"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "harbor"))(t)
	beaID := tailnettest.Identity{NodeID: "nBEA", NodeKey: "nodekey:bea-mac", Login: "bea@example.com", Host: "bea-mac"}
	r := startDaemonWith(t, tn, "bea-mac", beaID, discard, roomy)
	gate := &sharedAckGate{}
	r.dm.Deps.AddTransport(sharedAckTransport{altTransport: altTransport{r.dm.Deps.Transport(core.ViaTailnet)}, gate: gate})
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "harbor", As: "bea", Role: "member"}))(t)
	mustf(r.owner.Join(ctx, api.JoinRequest{Code: "alt+" + inv.Code}))(t)
	dir := t.TempDir()
	spawn := func() orchapi.Session {
		return mustf(orchclient.Spawn(ctx, at(r.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "claude", Shared: true, Cwd: dir,
			Command: []string{"/bin/sh", "-c", `stty raw -echo; exec cat > "$ZM_SCRATCH/in"`}, Env: map[string]string{"PATH": os.Getenv("PATH")}}))(t)
	}
	state := func(addr, id string) string {
		var l orchapi.PromptList
		if err := a.owner.Do(ctx, http.MethodGet, "/v0/sessions/"+client.Seg(addr)+"/prompts", nil, &l); err != nil {
			t.Fatal(err)
		}
		for _, p := range l.Prompts {
			if p.ID == id {
				return p.State
			}
		}
		return "?"
	}

	// Ends right after typing: the next beat still acks it.
	s1 := spawn()
	heartbeat(t, r)
	p1 := mustf(prompt(t, a.owner, s1.Address, "one"))(t)
	heartbeat(t, r) // typed
	waitStaged(t, filepath.Join(s1.Scratch, "in"), "one")
	waitFor(t, "last_prompt on the replica", func() bool {
		return mustf(orchclient.Session(ctx, r.owner, s1.ID))(t).LastPrompt != nil
	})
	mustf(orchclient.Stop(ctx, r.owner, s1.ID, orchapi.StopRequest{Signal: "KILL"}))(t)
	waitFor(t, "the session ended, its last_prompt gone", func() bool {
		ss := mustf(orchclient.Session(ctx, r.owner, s1.ID))(t)
		return ss.State == orchapi.StateEnded && ss.LastPrompt == nil
	})
	heartbeat(t, r) // the session is gone; its ack goes with this beat
	if got := state(s1.Address, p1.ID); got != orchapi.PromptDelivered {
		t.Fatalf("typed, then the session ended: %s", got)
	}
	assertStaged(t, filepath.Join(s1.Scratch, "in"), "one")

	// The daemon restarts between typing and acking.
	s2 := spawn()
	heartbeat(t, r)
	gate.armed.Store(true)
	p2 := mustf(prompt(t, a.owner, s2.Address, "two"))(t)
	heartbeat(t, r) // staged; the gate keeps acknowledgements from the authority
	waitStaged(t, filepath.Join(s2.Scratch, "in"), "two")
	heartbeat(t, r) // the transport refuses the ack, including any kicked beat
	if got := state(s2.Address, p2.ID); got != orchapi.PromptPending {
		t.Fatalf("before the ack: %s", got)
	}
	var recorded string
	var acked int
	if err := r.dm.Deps.Store.Registry().QueryRow(`SELECT state,acked FROM team_prompts_typed WHERE id=?`, p2.ID).Scan(&recorded, &acked); err != nil || recorded != "typed" || acked != 0 || gate.blocked.Load() == 0 {
		t.Fatalf("staged prompt not durably awaiting blocked ack: state=%s acked=%d blocked=%d err=%v", recorded, acked, gate.blocked.Load(), err)
	}
	restartSharedWithAckGate(t, r, tn, beaID, gate) // managed sessions end; durable unacked prompt survives
	if got := state(s2.Address, p2.ID); got != orchapi.PromptPending {
		t.Fatalf("restart bypassed blocked ack: %s", got)
	}
	assertStaged(t, filepath.Join(s2.Scratch, "in"), "two")
	gate.armed.Store(false)
	heartbeat(t, r)
	if got := state(s2.Address, p2.ID); got != orchapi.PromptDelivered {
		t.Fatalf("after the restart: %s", got)
	}
	if err := r.dm.Deps.Store.Registry().QueryRow(`SELECT state,acked FROM team_prompts_typed WHERE id=?`, p2.ID).Scan(&recorded, &acked); err != nil || recorded != "typed" || acked != 1 {
		t.Fatalf("restarted prompt ack not persisted: state=%s acked=%d err=%v", recorded, acked, err)
	}
	assertStaged(t, filepath.Join(s2.Scratch, "in"), "two")
}

// A replica acks only its own sessions' prompts (SPEC 17.10): Bob's replica
// naming a prompt to Carol's session leaves it pending, whether the entry
// carries Carol's address (refused) or one of Bob's; Carol's ack marks it.
// And an ack counts only in the prompt's project.
func TestSharedSessionAckOnlyOwn(t *testing.T) {
	quietPresence(t)
	ctx := context.Background()
	tn := tailnettest.New()
	a := authority(t, tn)
	bob, bj := replica(t, tn, a, "bob-mac", "nBOB", "harbor", "bob", "member")
	carol, cj := replica(t, tn, a, "carol-mac", "nCAROL", "harbor", "carol", "member")
	pid := cj.Remote.Project.ID
	cs := "carol-mac/cccccccc"
	if code, body := presenceAs(t, tn, carol, cj.Remote.TokenFile, presenceReq{Project: pid, Sessions: []orchapi.PresenceSession{
		{Address: cs, State: orchapi.StateRunning, Agent: "claude", Managed: true, Shared: true, Started: time.Now()}}}); code != http.StatusOK {
		t.Fatalf("carol's presence: %d %s", code, body)
	}
	pr, err := prompt(t, a.owner, cs, "carol only")
	if err != nil || pr.State != orchapi.PromptPending {
		t.Fatalf("prompt: %+v %v", pr, err)
	}
	state := func() string {
		var l orchapi.PromptList
		if err := a.owner.Do(ctx, http.MethodGet, "/v0/sessions/"+client.Seg(cs)+"/prompts", nil, &l); err != nil || len(l.Prompts) != 1 {
			t.Fatalf("log: %+v %v", l, err)
		}
		return l.Prompts[0].State
	}
	for _, addr := range []string{cs, "bob-mac/bbbbbbbb"} {
		code, body := presenceAs(t, tn, bob, bj.Remote.TokenFile, presenceReq{Project: pid, Sessions: []orchapi.PresenceSession{
			{Address: addr, State: orchapi.StateRunning, Started: time.Now(), Delivered: []string{pr.ID}}}})
		if addr == cs && code != http.StatusForbidden {
			t.Fatalf("bob's ack under carol's address: %d %s", code, body)
		}
		if got := state(); got != orchapi.PromptPending {
			t.Fatalf("bob acked carol's prompt (as %s): %s", addr, got)
		}
	}
	if code, body := presenceAs(t, tn, carol, cj.Remote.TokenFile, presenceReq{Project: pid, Sessions: []orchapi.PresenceSession{
		{Address: cs, State: orchapi.StateRunning, Agent: "claude", Managed: true, Shared: true, Started: time.Now(), Delivered: []string{pr.ID}}}}); code != http.StatusOK {
		t.Fatalf("carol's ack: %d %s", code, body)
	}
	if got := state(); got != orchapi.PromptDelivered {
		t.Fatalf("carol's own ack: %s", got)
	}
	// An ack that lands after the prompt expired still marks it delivered.
	late := mustf(prompt(t, a.owner, cs, "late ack"))(t)
	if _, err := a.dm.Deps.Store.Registry().ExecContext(ctx, `UPDATE session_prompts SET state = ? WHERE id = ?`, orchapi.PromptExpired, late.ID); err != nil {
		t.Fatal(err)
	}
	if code, body := presenceAs(t, tn, carol, cj.Remote.TokenFile, presenceReq{Project: pid, Sessions: []orchapi.PresenceSession{
		{Address: cs, State: orchapi.StateRunning, Agent: "claude", Managed: true, Shared: true, Started: time.Now(), Delivered: []string{late.ID}}}}); code != http.StatusOK {
		t.Fatalf("carol's late ack: %d %s", code, body)
	}
	var ll orchapi.PromptList
	if err := a.owner.Do(ctx, http.MethodGet, "/v0/sessions/"+client.Seg(cs)+"/prompts", nil, &ll); err != nil || len(ll.Prompts) != 2 ||
		ll.Prompts[0].State != orchapi.PromptDelivered || ll.Prompts[0].DeliveredAt == nil {
		t.Fatalf("a late ack: %+v %v", ll.Prompts, err)
	}

	// Bob's own session's prompt, acked in a heartbeat for another project
	// of the same authority: left pending (acks are per project).
	mustf(a.owner.CreateProject(ctx, api.NewProjectRequest{Slug: "two"}))(t)
	mustf(a.owner.ProjectTeam(ctx, "two"))(t)
	inv := mustf(a.owner.Invite(ctx, api.InviteRequest{Project: "two", As: "bob", Role: "member"}))(t)
	j2 := mustf(bob.owner.Join(ctx, api.JoinRequest{Code: inv.Code}))(t)
	bs := "bob-mac/bbbbbbbb"
	if code, body := presenceAs(t, tn, bob, bj.Remote.TokenFile, presenceReq{Project: pid, Sessions: []orchapi.PresenceSession{
		{Address: bs, State: orchapi.StateRunning, Agent: "claude", Managed: true, Shared: true, Started: time.Now()}}}); code != http.StatusOK {
		t.Fatalf("bob's presence: %d %s", code, body)
	}
	bp := mustf(prompt(t, a.owner, bs, "bob's"))(t)
	if code, body := presenceAs(t, tn, bob, j2.Remote.TokenFile, presenceReq{Project: j2.Remote.Project.ID, Sessions: []orchapi.PresenceSession{
		{Address: bs, State: orchapi.StateRunning, Started: time.Now(), Delivered: []string{bp.ID}}}}); code != http.StatusOK {
		t.Fatalf("bob's presence in two: %d %s", code, body)
	}
	var l orchapi.PromptList
	if err := a.owner.Do(ctx, http.MethodGet, "/v0/sessions/"+client.Seg(bs)+"/prompts", nil, &l); err != nil || len(l.Prompts) != 1 ||
		l.Prompts[0].State != orchapi.PromptPending {
		t.Fatalf("acked from another project: %+v %v", l.Prompts, err)
	}
}

// At most once, never twice (SPEC 17.10): the replica records a prompt
// before typing it. While the record cannot be written the prompt is not
// typed, beat after beat; once it can, the prompt is typed once. A prompt
// recorded but never marked typed (a crash in between) is neither typed nor
// acked.
func TestSharedSessionRecordBeforeTyping(t *testing.T) {
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
	ss := mustf(orchclient.Spawn(ctx, at(r.owner, "harbor", ""), orchapi.SpawnRequest{Agent: "claude", Shared: true, Cwd: t.TempDir(),
		Command: []string{"/bin/sh", "-c", `stty raw -echo; exec cat > "$ZM_SCRATCH/in"`}, Env: map[string]string{"PATH": os.Getenv("PATH")}}))(t)
	heartbeat(t, r)
	reg := r.dm.Deps.Store.Registry()
	state := func(id string) string {
		var l orchapi.PromptList
		if err := a.owner.Do(ctx, http.MethodGet, "/v0/sessions/"+client.Seg(ss.Address)+"/prompts", nil, &l); err != nil {
			t.Fatal(err)
		}
		for _, p := range l.Prompts {
			if p.ID == id {
				return p.State
			}
		}
		return "?"
	}
	typedText := func() string {
		b, _ := os.ReadFile(filepath.Join(ss.Scratch, "in"))
		return string(b)
	}

	// The record cannot be written: never typed.
	if _, err := reg.ExecContext(ctx, `CREATE TRIGGER no_record BEFORE INSERT ON team_prompts_typed BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	p1 := mustf(prompt(t, a.owner, ss.Address, "one"))(t)
	for range 3 {
		heartbeat(t, r)
	}
	time.Sleep(200 * time.Millisecond)
	if got := typedText(); got != "" || state(p1.ID) != orchapi.PromptPending {
		t.Fatalf("typed without a record: %q %s", got, state(p1.ID))
	}
	// It can be written again: typed once, then acked.
	if _, err := reg.ExecContext(ctx, `DROP TRIGGER no_record`); err != nil {
		t.Fatal(err)
	}
	heartbeat(t, r)
	waitStaged(t, filepath.Join(ss.Scratch, "in"), "one")
	heartbeat(t, r)
	heartbeat(t, r)
	if n := strings.Count(typedText(), "one"); n != 1 || state(p1.ID) != orchapi.PromptDelivered {
		t.Fatalf("typed %d times, %s", n, state(p1.ID))
	}
	assertStaged(t, filepath.Join(ss.Scratch, "in"), "one")

	// Recorded, then a crash before typing: never typed, never acked.
	p2 := mustf(prompt(t, a.owner, ss.Address, "two"))(t)
	if _, err := reg.ExecContext(ctx, `INSERT INTO team_prompts_typed (id, project, session, typed, state) VALUES (?, ?, ?, ?, 'typing')`,
		p2.ID, ss.Project, ss.Address, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	heartbeat(t, r)
	heartbeat(t, r)
	time.Sleep(200 * time.Millisecond)
	if strings.Contains(typedText(), "two") || state(p2.ID) != orchapi.PromptPending {
		t.Fatalf("a prompt recorded before a crash: %q %s", typedText(), state(p2.ID))
	}
	assertStaged(t, filepath.Join(ss.Scratch, "in"), "one")
}
