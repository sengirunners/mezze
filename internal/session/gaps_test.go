package session

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/core"
)

// A read-only attach (?input=0, cockpit gap G1) gets the scrollback and live
// output, but what it writes never reaches the PTY, never counts as a
// person answering a permission prompt, and the daemon still answers the
// session's cursor position queries (the viewer is not a terminal).
func TestReadOnlyAttach(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	script := `echo ready; read go; stty raw -echo; printf '\033[6n'; r=$(dd bs=1 count=6 2>/dev/null | od -An -c | tr -d ' \n'); ` +
		`stty sane; echo "got:$r"; exec cat`
	ss := h.spawn("claude", script, nil)
	h.waitOutput(ss.ID, "ready")

	if _, err := h.c.Upgrade(ctx, "/v0/sessions/"+ss.ID+"/attach?input=maybe", orchapi.AttachProtocol); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("attach with a bad input value: %v", err)
	}
	conn, err := orchclient.AttachReadOnly(ctx, h.c, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	readUntil(t, br, "ready") // scrollback replayed

	// The program probes the terminal while only a viewer is attached: the
	// daemon answers, as with nobody attached.
	if err := orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "go", Enter: true}); err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "got:033[1;1R")

	// A permission prompt stays needs_human whatever the viewer sends.
	if _, err := orchclient.SetState(ctx, h.c, ss.ID, orchapi.StateRequest{State: orchapi.StateNeedsHuman, Note: "permission"}); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("viewer-typed\r")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if o, _ := orchclient.Output(ctx, h.c, ss.ID, 0); strings.Contains(o.Data, "viewer-typed") {
		t.Fatalf("a read-only attach typed into the session: %q", o.Data)
	}
	if got, _ := orchclient.Session(ctx, h.c, ss.ID); got.State != orchapi.StateNeedsHuman {
		t.Fatalf("a read-only attach answered the prompt: %+v", got)
	}
	// The viewer still sees live output.
	if err := orchclient.Input(ctx, h.c, ss.ID, orchapi.InputRequest{Data: "typed-by-owner", Enter: true}); err != nil {
		t.Fatal(err)
	}
	readUntil(t, br, "typed-by-owner")
	if got := h.s.proc(ss.ID); got != nil {
		got.mu.Lock()
		n, v := len(got.conns), len(got.viewers)
		got.mu.Unlock()
		if n != 0 || v != 1 {
			t.Fatalf("conns %d viewers %d, want 0 and 1", n, v)
		}
	}
	if _, err := orchclient.Stop(ctx, h.c, ss.ID, orchapi.StopRequest{Signal: "KILL"}); err != nil {
		t.Fatal(err)
	}
	// The viewer's stream ends with the session.
	done := make(chan struct{})
	go func() {
		var b bytes.Buffer
		_, _ = b.ReadFrom(br)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("read-only attach not closed when the session ended")
	}
}

// stopRaw posts a stop and returns the session and the Zm-Stop-Signals header.
func (h *harness) stopRaw(id string, body string) (int, string, string) {
	h.t.Helper()
	c := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", h.sock)
	}}}
	req, _ := http.NewRequest("POST", "http://zm/v0/sessions/"+id+"/stop", strings.NewReader(body))
	req.Header.Set(api.HeaderProtocol, api.Protocol)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, resp.Header.Get(orchapi.HeaderStopSignals), b.String()
}

// escalate: true on the stop route does what `zm kill` does (cockpit gap G7):
// TERM, then HUP for a shell that ignores TERM, then KILL only with force.
func TestStopEscalate(t *testing.T) {
	oldT, oldH, oldK := StopTermWait, StopHupWait, StopKillWait
	StopTermWait, StopHupWait, StopKillWait = 300*time.Millisecond, 300*time.Millisecond, 2*time.Second
	t.Cleanup(func() { StopTermWait, StopHupWait, StopKillWait = oldT, oldH, oldK })
	h := newHarness(t, nil)
	ctx := context.Background()

	for _, bad := range []string{`{"escalate": true, "signal": "KILL"}`, `{"escalate": true, "wait_ms": 100}`, `{"force": true}`} {
		ss := h.spawn("shell", `echo ready; exec sleep 30`, nil)
		if code, _, body := h.stopRaw(ss.ID, bad); code != http.StatusBadRequest {
			t.Errorf("stop %s: %d %s", bad, code, body)
		}
		_, _ = orchclient.Stop(ctx, h.c, ss.ID, orchapi.StopRequest{Signal: "KILL"})
	}

	// Ignores TERM: HUP ends it.
	ss := h.spawn("shell", `trap '' TERM; echo ready; exec sleep 30`, nil)
	h.waitOutput(ss.ID, "ready")
	code, sent, body := h.stopRaw(ss.ID, `{"escalate": true}`)
	got, _ := orchclient.Session(ctx, h.c, ss.ID)
	if code != 200 || sent != "TERM, HUP" || !ended(got) || got.Outcome.Kind != orchapi.OutcomeKilled || got.Outcome.Signal != "HUP" {
		t.Fatalf("escalate on a TERM-ignoring session: %d %q %s %+v", code, sent, body, got.Outcome)
	}

	// Ends at TERM: nothing more is sent.
	ss = h.spawn("shell", `echo ready; exec sleep 30`, nil)
	h.waitOutput(ss.ID, "ready")
	if code, sent, _ := h.stopRaw(ss.ID, `{"escalate": true, "force": true}`); code != 200 || sent != "TERM" {
		t.Fatalf("escalate on a plain session: %d %q", code, sent)
	}

	// Ignores TERM and HUP: still running without force; KILL with it.
	ss = h.spawn("shell", `trap '' TERM HUP; echo ready; exec sleep 30`, nil)
	h.waitOutput(ss.ID, "ready")
	if code, sent, _ := h.stopRaw(ss.ID, `{"escalate": true}`); code != 200 || sent != "TERM, HUP" {
		t.Fatalf("escalate without force: %d %q", code, sent)
	}
	if got, _ := orchclient.Session(ctx, h.c, ss.ID); got.State == orchapi.StateEnded {
		t.Fatalf("ended without KILL: %+v", got)
	}
	code, sent, _ = h.stopRaw(ss.ID, `{"escalate": true, "force": true}`)
	got, _ = orchclient.Session(ctx, h.c, ss.ID)
	if code != 200 || sent != "TERM, HUP, KILL" || !ended(got) || got.Outcome.Signal != "KILL" {
		t.Fatalf("escalate with force: %d %q %+v", code, sent, got.Outcome)
	}
}

// GET /v0/sessions pages (cockpit gap G6): ?limit= rows, most recently updated
// first, and next is the cursor for the rest; walking the pages returns
// every session once, ties on updated included.
func TestSessionPaging(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	want := map[string]bool{}
	for i := range 7 {
		ss, _, err := h.s.Register(core.WithSubject(ctx, h.owner()), orchapi.RegisterRequest{Agent: "claude",
			RuntimeID: "page-" + string(rune('a'+i)) + "-0000-uuid", Cwd: h.home})
		if err != nil {
			t.Fatal(err)
		}
		want[ss.ID] = true
	}
	seen, before, pages := map[string]bool{}, "", 0
	for {
		l, err := orchclient.SessionPage(ctx, h.c, orchapi.SessionQuery{All: true, Limit: 3, Before: before})
		if err != nil {
			t.Fatal(err)
		}
		pages++
		if len(l.Sessions) > 3 {
			t.Fatalf("page of %d", len(l.Sessions))
		}
		for _, s := range l.Sessions {
			if seen[s.ID] {
				t.Fatalf("session %s on two pages", s.ID)
			}
			seen[s.ID] = true
		}
		if l.Next == "" {
			break
		}
		before = l.Next
		if pages > 5 {
			t.Fatal("paging does not end")
		}
	}
	if len(seen) != len(want) || pages != 3 {
		t.Fatalf("pages %d saw %d of %d", pages, len(seen), len(want))
	}
	// next only when more rows follow: an exactly full last page has none
	// (no extra request for an empty page), one row short of all has one.
	if l, err := orchclient.SessionPage(ctx, h.c, orchapi.SessionQuery{All: true, Limit: 7}); err != nil || len(l.Sessions) != 7 || l.Next != "" {
		t.Fatalf("exactly full page: %d sessions, next %q, %v", len(l.Sessions), l.Next, err)
	}
	if l, err := orchclient.SessionPage(ctx, h.c, orchapi.SessionQuery{All: true, Limit: 6}); err != nil || len(l.Sessions) != 6 || l.Next == "" {
		t.Fatalf("page short of all: %d sessions, next %q, %v", len(l.Sessions), l.Next, err)
	}
	if _, err := orchclient.SessionPage(ctx, h.c, orchapi.SessionQuery{Before: "nope"}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("bad cursor: %v", err)
	}
	if _, err := orchclient.SessionPage(ctx, h.c, orchapi.SessionQuery{Limit: 501}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("limit over 500: %v", err)
	}
}

// A plain attach replays the last 64 KiB; ?replay=all replays all that is held.
func TestAttachReplayAll(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	ss := h.spawn("shell", `i=0; while [ $i -lt 8000 ]; do printf 'line %05d ................................\n' $i; i=$((i+1)); done; `+
		`echo done-writing; exec cat`, nil)
	h.waitOutput(ss.ID, "done-writing")
	read := func(attach func(context.Context, *client.Client, string) (io.ReadWriteCloser, error)) string {
		conn, err := attach(ctx, h.c, ss.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var got bytes.Buffer
		br := bufio.NewReader(conn)
		for !strings.Contains(got.String(), "done-writing") {
			line, err := br.ReadString('\n')
			got.WriteString(line)
			if err != nil {
				t.Fatalf("after %d bytes: %v", got.Len(), err)
			}
		}
		return got.String()
	}
	if s := read(orchclient.Attach); strings.Contains(s, "line 00000") || len(s) > 70<<10 {
		t.Fatalf("plain attach replayed %d bytes", len(s))
	}
	if s := read(orchclient.AttachAll); !strings.HasPrefix(s, "line 00000") {
		t.Fatalf("replay=all replayed %d bytes, starting %q", len(s), s[:min(len(s), 40)])
	}
	if _, err := h.c.Upgrade(ctx, "/v0/sessions/"+ss.ID+"/attach?replay=some", orchapi.AttachProtocol); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("attach with a bad replay value: %v", err)
	}
	if _, err := orchclient.Stop(ctx, h.c, ss.ID, orchapi.StopRequest{Signal: "KILL"}); err != nil {
		t.Fatal(err)
	}
}
