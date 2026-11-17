package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/orchcore"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/config"
)

// promptAgent is a real program at a real prompt on the session's PTY: the
// terminal's line discipline holds what has been typed until Enter, and only
// then does the program see it. So a Nudge's Enter is what would send a
// person's half-written line, as it did in Claude Code.
const promptAgent = `printf 'ready\n'; while IFS= read -r line; do printf 'SENT<%s>\n' "$line"; done`

func (h *harness) promptSession(t *testing.T) orchapi.Session {
	t.Helper()
	ss := h.spawn("shell", promptAgent, nil)
	h.waitOutput(ss.ID, "ready")
	h.markIdle(t, ss)
	return ss
}

func (h *harness) markIdle(t *testing.T, ss orchapi.Session) {
	t.Helper()
	if err := h.s.SetState(context.Background(), ss.Address, orchapi.StateRequest{State: orchapi.StateIdle}); err != nil {
		t.Fatal(err)
	}
}

// nudgeSettles waits long enough for an accepted nudge to have typed its line
// and pressed Enter, whatever Nudge answered.
func nudgeSettles() { time.Sleep(enterDelay + 150*time.Millisecond) }

func (h *harness) refuteOutput(t *testing.T, id string, banned ...string) {
	t.Helper()
	o, err := orchclient.Output(context.Background(), h.c, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range banned {
		if strings.Contains(o.Data, b) {
			t.Fatalf("output has %q: %q", b, o.Data)
		}
	}
}

func (h *harness) waitConns(t *testing.T, id string, n int) {
	t.Helper()
	p := h.s.proc(id)
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		p.mu.Lock()
		got := len(p.conns)
		p.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("attached clients: %d, want %d", got, n)
		}
	}
}

// The reported failure: a person has half a message in the prompt, attached,
// and mail comes. Nothing is typed, nothing is sent, and the rest of what they
// type joins the same line.
func TestNudgeLeavesAHalfWrittenPromptAttached(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.promptSession(t)
	conn, err := orchclient.Attach(ctx, h.c, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	h.waitConns(t, ss.ID, 1)
	if _, err := conn.Write([]byte("go with your choices th")); err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "go with your choices th") // the terminal echoed it: it is in the prompt

	if ok, _ := h.s.Nudge(ctx, ss.Address, NudgeLine); ok {
		t.Error("nudged a session a person is typing in")
	}
	nudgeSettles()
	h.refuteOutput(t, ss.ID, NudgeLine, "SENT<")

	if _, err := conn.Write([]byte("o\r")); err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "SENT<go with your choices tho>")
}

// The person walks away from the draft: nobody is attached, but the text is
// still in the prompt, and the nudge's Enter would send it.
func TestNudgeLeavesADraftAfterDetach(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.promptSession(t)
	conn, err := orchclient.Attach(ctx, h.c, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("half a thought")); err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "half a thought")
	conn.Close()
	h.waitConns(t, ss.ID, 0)

	if ok, _ := h.s.Nudge(ctx, ss.Address, NudgeLine); ok {
		t.Error("nudged over a draft nobody sent")
	}
	nudgeSettles()
	h.refuteOutput(t, ss.ID, NudgeLine, "SENT<")

	// They come back and finish it: the same line, and from then on the
	// session can be woken again.
	conn, err = orchclient.Attach(ctx, h.c, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte(" finished\r")); err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "SENT<half a thought finished>")
	conn.Close()
	h.waitConns(t, ss.ID, 0)
	h.markIdle(t, ss)
	if ok, err := h.s.Nudge(ctx, ss.Address, NudgeLine); !ok || err != nil {
		t.Fatalf("nudge after the draft was sent: %v %v", ok, err)
	}
	h.waitOutput(ss.ID, "SENT<"+NudgeLine+">")
}

// The daemon restarts while a draft sits in a session that outlives it.
func TestNudgeLeavesADraftAcrossARestart(t *testing.T) {
	home, err := os.MkdirTemp("", "zm") // short: Unix socket paths
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	h := startHarness(t, home, nil, true)
	ctx := context.Background()
	ss := h.promptSession(t)
	conn, err := orchclient.Attach(ctx, h.c, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("half a thought")); err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "half a thought")
	conn.Close()
	h.waitConns(t, ss.ID, 0)
	h.close()

	h2 := startHarness(t, home, nil, false) // false: this one ends the session on close
	defer h2.close()
	h2.markIdle(t, ss)
	if ok, _ := h2.s.Nudge(ctx, ss.Address, NudgeLine); ok {
		t.Error("the restarted daemon nudged over the draft")
	}
	nudgeSettles()
	h2.refuteOutput(t, ss.ID, NudgeLine, "SENT<")
}

// `zm session input --enter=false` leaves text in the prompt too.
func TestNudgeLeavesInputWithoutEnter(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.promptSession(t)
	if err := orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "half from a script"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := h.s.Nudge(ctx, ss.Address, NudgeLine); ok {
		t.Error("nudged over input with no Enter")
	}
	nudgeSettles()
	h.refuteOutput(t, ss.ID, NudgeLine, "SENT<")
	if err := orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: " done", Enter: true}); err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "SENT<half from a script done>")
}

// A person's key cannot land between the line a nudge typed and its Enter,
// whichever way the race goes: the original splice was "th" + the line + "o".
func TestNudgeEnterCarriesNoPersonsKey(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.promptSession(t)
	if ok, err := h.s.Nudge(ctx, ss.Address, NudgeLine); !ok || err != nil {
		t.Fatalf("nudge: %v %v", ok, err)
	}
	// Into the gap, once the line is in: as an attach or `zm session input` would.
	h.waitOutput(ss.ID, NudgeLine)
	if err := orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "o", Enter: true}); err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "SENT<o>")
	h.waitOutput(ss.ID, "SENT<"+NudgeLine+">")
}

// An encoded key is ambiguous to the daemon. A hook has no input sequence,
// so it cannot prove that the current draft is the prompt it acknowledged.
func TestPromptHookPreservesUncorrelatedDraft(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.promptSession(t)
	conn, err := orchclient.Attach(ctx, h.c, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("hello\x1b[13u")); err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "hello")
	conn.Close()
	h.waitConns(t, ss.ID, 0)
	if ok, _ := h.s.Nudge(ctx, ss.Address, NudgeLine); ok {
		t.Fatal("nudged before the hook said the prompt went")
	}
	r := orchapi.HookRequest{Notice: true, Session: ss.Address}
	r.Agent, r.RuntimeID, r.Cwd, r.State = "shell", "rt-1", h.home, orchapi.StateRunning
	out, err := orchclient.Hook(ctx, h.c, r)
	if err != nil {
		t.Fatal(err)
	}
	if out.Session.State != orchapi.StateRunning || out.Notice == nil {
		t.Fatalf("prompt hook lost state/notice behavior: %+v", out)
	}
	h.markIdle(t, ss)
	if ok, err := h.s.Nudge(ctx, ss.Address, NudgeLine); ok || err != nil {
		t.Fatalf("uncorrelated prompt hook cleared draft: %v %v", ok, err)
	}
	nudgeSettles()
	h.refuteOutput(t, ss.ID, NudgeLine, "SENT<")
}

// An agent nobody has touched still wakes the moment mail comes: the session
// `zm run -d` started with its first prompt typed by the daemon (not a
// person), gone idle on the output timer alone, as Codex without hooks does.
func TestNudgeStillWakesAnUnattendedAgent(t *testing.T) {
	h := newHarness(t, func(c *config.Config) { c.File.IdleAfterSeconds = 1 })
	ctx := context.Background()
	// The pause: output from before the daemon adopted the session is history,
	// and the first prompt waits for output it sees (else 10 s, too long here).
	ss := h.spawn("codex", "sleep 1; "+promptAgent, func(r *orchapi.SpawnRequest) { r.Prompt = "first job" })
	h.waitOutput(ss.ID, "SENT<first job>")
	h.wait(ss.ID, "idle", func(s orchapi.Session) bool { return s.State == orchapi.StateIdle && s.Note == "no output" })
	if ok, err := h.s.Nudge(ctx, ss.Address, NudgeLine); !ok || err != nil {
		t.Fatalf("nudge: %v %v", ok, err)
	}
	h.waitOutput(ss.ID, "SENT<"+NudgeLine+">")
}

type unreadMail struct {
	orchcore.MailService
	parts []orchapi.NoticePart
}

func (m unreadMail) NoticeParts(context.Context, *authz.Subject) ([]orchapi.NoticePart, error) {
	return m.parts, nil
}
func (m unreadMail) OpenAsks(context.Context, string, string) (int, error) { return 0, nil }

// Mail refused while a person had the terminal open still wakes the agent when
// they leave, if they left nothing in the prompt. Read-only viewers and mail
// that never wakes anybody do not.
func TestDetachWakesForMailHeldBack(t *testing.T) {
	one, zero := 1, 0
	direct := []orchapi.NoticePart{{Project: "p", Unread: 1}}
	for _, tc := range []struct {
		name  string
		parts []orchapi.NoticePart
		keys  string
		wake  bool
	}{
		{"direct mail, nothing typed", direct, "", true},
		{"direct mail, sent a line", direct, "sent it\r", true},
		{"direct mail, left a draft", direct, "half", false},
		{"direct plus quiet group", []orchapi.NoticePart{{Project: "p", Unread: 2, Group: 1, Quiet: 1, DirectWakeable: &one}}, "", true},
		{"direct quiet plus group", []orchapi.NoticePart{{Project: "p", Unread: 2, Group: 1, Quiet: 1, DirectWakeable: &zero}}, "", false},
		{"no mail", nil, "", false},
		{"only group mail", []orchapi.NoticePart{{Project: "p", Unread: 1, Group: 1}}, "", false},
		{"only announcements", []orchapi.NoticePart{{Project: "p", Unread: 2, Announce: 2}}, "", false},
		{"only notices", []orchapi.NoticePart{{Project: "p", Unread: 1, Quiet: 1}}, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, nil)
			orchcore.Wire(h.s.d).Mail = unreadMail{parts: tc.parts}
			ctx := context.Background()
			ss := h.promptSession(t)
			conn, err := orchclient.Attach(ctx, h.c, ss.ID)
			if err != nil {
				t.Fatal(err)
			}
			h.waitConns(t, ss.ID, 1)
			if tc.keys != "" {
				if _, err := conn.Write([]byte(tc.keys)); err != nil {
					t.Fatal(err)
				}
				h.waitOutput(ss.ID, strings.TrimSuffix(tc.keys, "\r"))
				if strings.HasSuffix(tc.keys, "\r") {
					h.waitOutput(ss.ID, "SENT<")
				}
				h.markIdle(t, ss)
			}
			conn.Close()
			h.waitConns(t, ss.ID, 0)
			if tc.wake {
				h.waitOutput(ss.ID, "SENT<"+NudgeLine+">")
				return
			}
			nudgeSettles()
			h.refuteOutput(t, ss.ID, NudgeLine)
		})
	}
}

// Taken over from PR #178. A client that can type holds the nudge back; a
// read-only viewer does not.
func TestNudgeLeavesAPersonsTerminalAlone(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.promptSession(t)
	view, err := orchclient.AttachReadOnly(ctx, h.c, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	conn, err := orchclient.Attach(ctx, h.c, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	h.waitConns(t, ss.ID, 1)
	if ok, _ := h.s.Nudge(ctx, ss.Address, NudgeLine); ok {
		t.Fatal("nudged a session a person is attached to")
	}
	nudgeSettles()
	h.refuteOutput(t, ss.ID, NudgeLine)
	conn.Close()
	h.waitConns(t, ss.ID, 0)
	if ok, err := h.s.Nudge(ctx, ss.Address, NudgeLine); !ok || err != nil {
		t.Fatalf("nudge with only a viewer left: %v %v", ok, err)
	}
	h.waitOutput(ss.ID, "SENT<"+NudgeLine+">")
}

// A writer waiting behind another input's text/Enter transaction must not
// change the parser context before its bytes reach the terminal.
func TestConcurrentInputDraftAccountingFollowsWrites(t *testing.T) {
	h := newHarness(t, nil)
	ss := h.promptSession(t)
	p := h.s.proc(ss.ID)
	ctx := context.Background()
	first := make(chan error, 1)
	go func() {
		first <- orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "submitted", Enter: true})
	}()
	h.waitOutput(ss.ID, "submitted") // inside the first input's Enter delay
	second := make(chan error, 1)
	go func() { second <- orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "unsent"}) }()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "SENT<submitted>")
	h.waitOutput(ss.ID, "unsent")
	p.mu.Lock()
	draft, last := p.draft, p.lastKey
	p.mu.Unlock()
	if !draft || last != 't' {
		t.Fatalf("later input lost its draft: %v %q", draft, last)
	}
	h.markIdle(t, ss)
	if ok, err := h.s.Nudge(ctx, ss.Address, NudgeLine); ok || err != nil {
		t.Fatalf("nudge over concurrent input draft: %v %v", ok, err)
	}
	nudgeSettles()
	h.refuteOutput(t, ss.ID, NudgeLine, "SENT<unsent")
}

// The hook for A is delivered after B was typed. No timing interleaving or
// sleeps are needed: even a fully delayed hook cannot acknowledge B's draft.
func TestPromptHookDelayedAfterNewDraft(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.promptSession(t)
	if err := orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "first", Enter: true}); err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "SENT<first>")
	if err := orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "later unsent"}); err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "later unsent")
	r := orchapi.HookRequest{Notice: true, Session: ss.Address}
	r.Agent, r.RuntimeID, r.Cwd, r.State = "shell", "rt-delayed", h.home, orchapi.StateRunning
	if _, err := orchclient.Hook(ctx, h.c, r); err != nil {
		t.Fatal(err)
	}
	h.markIdle(t, ss)
	if ok, err := h.s.Nudge(ctx, ss.Address, NudgeLine); ok || err != nil {
		t.Fatalf("late hook allowed nudge over newer draft: %v %v", ok, err)
	}
	nudgeSettles()
	h.refuteOutput(t, ss.ID, NudgeLine, "SENT<later unsent")
	// A real plain Enter still submits B, clears its draft and allows wake.
	if err := orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Enter: true}); err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "SENT<later unsent>")
	if _, err := orchclient.Hook(ctx, h.c, r); err != nil {
		t.Fatal(err)
	}
	h.markIdle(t, ss)
	if ok, err := h.s.Nudge(ctx, ss.Address, NudgeLine); !ok || err != nil {
		t.Fatalf("known submission followed by hook prevented wake: %v %v", ok, err)
	}
	h.waitOutput(ss.ID, "SENT<"+NudgeLine+">")
}

// A persistence failure must reject new bytes while the old durable proof
// still says empty. Otherwise the next daemon could submit those new bytes.
func TestInputDraftPersistenceFailureRefusesPTY(t *testing.T) {
	for _, attach := range []bool{false, true} {
		t.Run(fmt.Sprint("attach=", attach), func(t *testing.T) {
			h := newHarness(t, nil)
			ss := h.promptSession(t)
			p := h.s.proc(ss.ID)
			// Make the proof directory temporarily unavailable while retaining
			// the existing empty proof. This fails even as root, with no hook.
			dir := filepath.Dir(p.draftFile)
			saved := dir + ".saved"
			if err := os.Rename(dir, saved); err != nil {
				t.Fatal(err)
			}
			restore := func() {
				_ = os.Remove(dir)
				if err := os.Rename(saved, dir); err != nil {
					t.Fatal(err)
				}
			}
			defer func() {
				if _, err := os.Stat(saved); err == nil {
					restore()
				}
			}()
			if err := os.WriteFile(dir, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			if attach {
				err = p.writeInput([]byte("unpersisted"), true)
			} else {
				err = orchclient.Input(context.Background(), h.c, ss.ID, orchapi.InputRequest{Data: "unpersisted"})
			}
			if err == nil {
				t.Fatal("input did not report persistence failure")
			}
			restore()
			// The refused bytes are absent even though old empty proof remains.
			h.refuteOutput(t, ss.ID, "unpersisted")
			adopted := &proc{id8: ss.ID, state: orchapi.StateIdle}
			h.s.loadDraft(adopted)
			if !adopted.mayNudge() {
				t.Fatal("fault did not preserve the old empty proof")
			}
			if p.mayNudge() {
				t.Fatal("failed input did not conservatively hold the live session")
			}
		})
	}
}
