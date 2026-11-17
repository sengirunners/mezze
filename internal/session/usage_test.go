package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, body string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(body); err != nil {
		t.Fatal(err)
	}
}

// claudeMsg is one transcript line of an assistant message's content block.
func claudeMsg(id, req string, in, write, read, out int) string {
	return fmt.Sprintf(`{"type":"assistant","requestId":%q,"message":{"id":%q,"usage":{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,"output_tokens":%d}}}`+"\n",
		req, id, in, write, read, out)
}

func codexCount(total, last [3]int) string {
	u := func(x [3]int) string {
		return fmt.Sprintf(`{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d}`, x[0], x[1], x[2], x[0]+x[2])
	}
	return `{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":` + u(total) + `,"last_token_usage":` + u(last) + `}}}` + "\n"
}

func tokensOf(t *testing.T, h *harness, id8 string) *orchapi.SessionTokens {
	t.Helper()
	r, err := loadRow(context.Background(), h.d.Store.Registry(), id8)
	if err != nil {
		t.Fatal(err)
	}
	return r.Tokens
}

// TestUsageRegisterAndTokens: a hook-registered session records who it is
// for, its job kind and the Claude account of its config at registration;
// its tokens come from its transcript and subagents, each message once,
// read on from where the last pass stopped, and once more at its end.
func TestUsageRegisterAndTokens(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	claude := t.TempDir()
	writeFile(t, filepath.Join(claude, ".claude.json"), `{"numStartups":3,"oauthAccount":{"emailAddress":"ana@example.com","displayName":"Ana"}}`)
	rid := "4bb7545a-1111-4222-8333-944444444444"
	ss, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: rid, Cwd: h.home, For: "bea-mac", JobKind: "farm", AgentHome: claude})
	if err != nil {
		t.Fatal(err)
	}
	if ss.Account != "ana@example.com" || ss.For != "bea-mac" || ss.JobKind != orchapi.JobKindFarm || ss.Tokens != nil {
		t.Fatalf("registered %+v", ss)
	}
	// The account is what was active at registration, never read again.
	writeFile(t, filepath.Join(claude, ".claude.json"), `{"oauthAccount":{"emailAddress":"bo@example.com"}}`)

	main := filepath.Join(claude, "projects", "-Users-ana-code", rid+".jsonl")
	writeFile(t, main, `{"type":"user","message":{"content":"usage please"}}`+"\n"+
		claudeMsg("msg_a", "req_a", 10, 100, 1000, 5)+claudeMsg("msg_a", "req_a", 10, 100, 1000, 5)+ // two blocks, one message
		claudeMsg("msg_b", "req_b", 1, 0, 2000, 7)+
		`{"type":"assistant","requestId":"req_c","message":{"id":"msg_c","usage":{"input_tokens":3`) // still being written
	writeFile(t, filepath.Join(claude, "projects", "-Users-ana-code", rid, "subagents", "agent-1.jsonl"), claudeMsg("msg_s", "req_s", 2, 20, 200, 4))
	h.s.usagePass()
	if got := tokensOf(t, h, ss.ID); got == nil || *got != (orchapi.SessionTokens{Input: 13, CacheWrite: 120, CacheRead: 3200, Output: 16}) {
		t.Fatalf("tokens after the first pass %+v", got)
	}
	// Read on, not again: a line already read is not counted twice, even
	// when its bytes change; the finished line and a new one are.
	b, _ := os.ReadFile(main)
	_ = os.WriteFile(main, []byte(strings.Replace(string(b), `"output_tokens":7`, `"output_tokens":9`, 1)), 0o600)
	appendFile(t, main, `,"output_tokens":1}}}`+"\n"+claudeMsg("msg_d", "req_d", 0, 0, 0, 100))
	h.s.usagePass()
	if got := tokensOf(t, h, ss.ID); got == nil || *got != (orchapi.SessionTokens{Input: 16, CacheWrite: 120, CacheRead: 3200, Output: 117}) {
		t.Fatalf("tokens after the second pass %+v", got)
	}
	got, _ := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{All: true})
	if len(got) != 1 || got[0].Tokens == nil || got[0].Tokens.Output != 117 || got[0].Account != "ana@example.com" {
		t.Fatalf("listed %+v", got)
	}

	// Its end reads the rest.
	appendFile(t, main, claudeMsg("msg_e", "req_e", 0, 0, 0, 1000))
	if _, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: rid, Cwd: h.home, End: true}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for tk := tokensOf(t, h, ss.ID); tk == nil || tk.Output != 1117; tk = tokensOf(t, h, ss.ID) {
		if time.Now().After(deadline) {
			t.Fatalf("tokens after the end %+v", tk)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// A job kind a hook should not send is dropped, never an error.
	odd, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "5cc7545a-1111-4222-8333-944444444444", Cwd: h.home, JobKind: "batch", AgentHome: claude})
	if err != nil || odd.JobKind != "" || odd.Account != "bo@example.com" {
		t.Fatalf("odd kind %+v %v", odd, err)
	}
}

// TestUsageCodexTokens: a Codex rollout's token_count events add up by
// their last request; a re-emitted count and a context-size report are not
// requests. Input leaves out the cached part.
func TestUsageCodexTokens(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	codex := t.TempDir()
	rid := "019a2b3c-4d5e-7f60-8a9b-0c1d2e3f4a5b"
	ss, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: rid, Cwd: h.home, AgentHome: codex})
	if err != nil {
		t.Fatal(err)
	}
	if ss.Account != "" {
		t.Fatalf("a codex session has no Claude account: %+v", ss)
	}
	day := time.Now()
	path := filepath.Join(codex, "sessions", day.Format("2006"), day.Format("01"), day.Format("02"), "rollout-2026-11-05T10-00-00-"+rid+".jsonl")
	writeFile(t, path, `{"type":"session_meta","payload":{"id":"`+rid+`"}}`+"\n"+
		codexCount([3]int{100, 40, 10}, [3]int{100, 40, 10})+
		codexCount([3]int{100, 40, 10}, [3]int{100, 40, 10})+ // re-emitted
		codexCount([3]int{300, 140, 30}, [3]int{200, 100, 20})+
		codexCount([3]int{0, 0, 0}, [3]int{0, 0, 0})) // a context-size report
	h.s.usagePass()
	if got := tokensOf(t, h, ss.ID); got == nil || *got != (orchapi.SessionTokens{Input: 160, CacheRead: 140, Output: 30}) {
		t.Fatalf("codex tokens %+v", got)
	}
}

// TestUsageSpawn: zm run's for and job kind are recorded and reach the
// child's environment; the account is the one in the child's
// CLAUDE_CONFIG_DIR; a job kind zm does not know is refused.
func TestUsageSpawn(t *testing.T) {
	h := newHarness(t, nil)
	claude := t.TempDir()
	writeFile(t, filepath.Join(claude, ".claude.json"), `{"oauthAccount":{"emailAddress":"ana@example.com"}}`)
	out := filepath.Join(h.home, "env.out")
	ss := h.spawn("claude", `echo "$ZM_FOR $ZM_JOB_KIND" > `+out+`; sleep 30`, func(r *orchapi.SpawnRequest) {
		r.For, r.JobKind, r.Env = "bea-mac", "standing", map[string]string{"CLAUDE_CONFIG_DIR": claude}
	})
	if ss.Account != "ana@example.com" || ss.For != "bea-mac" || ss.JobKind != orchapi.JobKindStanding {
		t.Fatalf("spawned %+v", ss)
	}
	waitFile(t, out, "bea-mac standing\n")
	_, err := orchclient.Spawn(context.Background(), h.c, orchapi.SpawnRequest{Agent: "sh", Command: []string{"/bin/sh", "-c", "true"}, Cwd: h.home, JobKind: "batch"})
	if !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("job kind batch: %v", err)
	}
}

// TestSessionKeepsItsWork: a session's claim becomes its work, and it keeps
// it after the release; another machine's address is not this machine's to
// record.
func TestSessionKeepsItsWork(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	ss, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "claude", RuntimeID: "6dd7545a-1111-4222-8333-944444444444", Cwd: h.home})
	if err != nil {
		t.Fatal(err)
	}
	claim := func(key string) {
		if _, err := h.d.Events.Publish(ctx, events.Input{Type: orchapi.EventWorkClaimed, Subject: key, Project: ss.Project,
			Data: orchapi.Lease{Key: key, Project: ss.Project, Holder: ss.Address}}); err != nil {
			t.Fatal(err)
		}
	}
	claim("bd:zm-1")
	deadline := time.Now().Add(5 * time.Second)
	for got, _ := h.s.Lookup(ctx, ss.Address); got.Work != "bd:zm-1"; got, _ = h.s.Lookup(ctx, ss.Address) {
		if time.Now().After(deadline) {
			t.Fatalf("work after the claim %q", got.Work)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := h.d.Events.Publish(ctx, events.Input{Type: orchapi.EventWorkReleased, Subject: "bd:zm-1", Project: ss.Project,
		Data: orchapi.Lease{Key: "bd:zm-1", Project: ss.Project, Holder: ss.Address}}); err != nil {
		t.Fatal(err)
	}
	h.s.RecordWork(ctx, "bea-mac/"+ss.ID, "bd:zm-2")
	h.s.RecordWork(ctx, ss.Address, "lock:migrations")
	h.s.RecordWork(ctx, ss.Address, "bd:zm-3") // a later claim replaces it
	if got, _ := h.s.Lookup(ctx, ss.Address); got.Work != "bd:zm-3" {
		t.Fatalf("work %q", got.Work)
	}
}

// TestPresenceUsage: an authority keeps a replica session's attribution and
// tokens from its heartbeats (a beat without them keeps the row's), the
// tokens of an ended session that arrive after its end, and the last key a
// session held after it no longer holds one.
func TestPresenceUsage(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	p := h.project("harbor")
	whole := core.WithSubject(ctx, &authz.Subject{Scope: authz.FromTuples([]authz.Tuple{
		{Relation: authz.Member, Object: authz.ProjectObject(p.ID)}}, nil)})
	at := time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond)
	beat := func(ps ...orchapi.PresenceSession) {
		t.Helper()
		if err := h.s.RecordPresence(whole, p.ID, "bea-mac", "build-box", ps); err != nil {
			t.Fatal(err)
		}
	}
	live := orchapi.PresenceSession{Address: "bea-mac/4bb7545a", Agent: "claude", State: orchapi.StateRunning, Started: at, Work: []string{"bd:zm-9"},
		Account: "bea@example.com", For: "bea-mac", JobKind: "farm", Tokens: &orchapi.SessionTokens{Input: 1, CacheRead: 2, CacheWrite: 3, Output: 4}}
	beat(live)
	bare := orchapi.PresenceSession{Address: live.Address, Agent: "claude", State: orchapi.StateIdle, Started: at, JobKind: "batch"} // older replica, lease released
	beat(bare)
	got, ok := h.s.loadPresence(ctx, live.Address)
	if !ok || got.Account != "bea@example.com" || got.For != "bea-mac" || got.JobKind != "farm" || got.Work != "bd:zm-9" ||
		got.Tokens == nil || *got.Tokens != *live.Tokens {
		t.Fatalf("presence row %+v", got)
	}
	ended := time.Now().UTC().Truncate(time.Millisecond)
	end := orchapi.PresenceSession{Address: live.Address, Agent: "claude", State: orchapi.StateEnded, Started: at, EndedAt: &ended,
		OutcomeKind: orchapi.OutcomeOK, Tokens: &orchapi.SessionTokens{Output: 40}}
	beat(end)
	end.Tokens = &orchapi.SessionTokens{Output: 400} // its final read, a beat later
	beat(end)
	list, err := orchclient.Sessions(ctx, h.c, orchapi.SessionQuery{All: true})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(list)
	if len(list) != 1 || list[0].State != orchapi.StateEnded || list[0].Tokens == nil || list[0].Tokens.Output != 400 ||
		list[0].Account != "bea@example.com" || list[0].JobKind != "farm" || list[0].Work != "bd:zm-9" {
		t.Fatalf("listed %s", b)
	}
	if !strings.Contains(string(b), `"tokens":{"input":0,"cache_read":0,"cache_write":0,"output":400}`) || !strings.Contains(string(b), `"for":"bea-mac"`) {
		t.Fatalf("wire names %s", b)
	}
}
