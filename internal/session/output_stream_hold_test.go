package session

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

// This fixture has no processes: a previously drained managed session remains
// readable from its files, just as after daemon restart.
func streamOutputFixture(t *testing.T, mode string, data []byte) (*harness, row) {
	t.Helper()
	h := newHarness(t, nil)
	h.project("stream-output", h.home)
	ctx := core.WithSubject(context.Background(), h.owner())
	ss, _, err := h.s.Register(ctx, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "stream-output-fixture", Cwd: h.home, State: orchapi.StateIdle})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.d.Store.Registry().Exec(`UPDATE sessions SET managed=1,state='ended',output_drained=1,mode=?,out_bytes=? WHERE id=?`, mode, len(data), ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := h.s.scrollbackPath(ss.ID)
	if mode == orchapi.SessionModePipe {
		path = holder.PipeEventsPath(h.home, ss.ID)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	r, ok := h.s.load(context.Background(), ss.ID)
	if !ok {
		t.Fatal("session missing")
	}
	return h, r
}

type streamBufferConn struct {
	net.Conn
	output bytes.Buffer
}

func (c *streamBufferConn) Write(p []byte) (int, error) { return c.output.Write(p) }

func TestSessionAttachRejectsCachedBytesAfterHoldClears(t *testing.T) {
	secret := "cached attach private secret"
	h, r := streamOutputFixture(t, "", []byte(secret))
	sink := &streamBufferConn{}
	c := &sessionOutputConn{Conn: sink, service: h.s, ctx: context.Background(), id: r.ID, revision: h.s.outputRevisions[r.ID]}
	if err := h.s.scrubSessionOutputs(context.Background(), core.Scrub{Project: r.Project, Texts: []string{secret}}); err != nil {
		t.Fatal(err)
	}
	if held, err := h.s.sessionOutputHeld(context.Background(), r.ID); err != nil || held {
		t.Fatalf("completed scrub held=%v err=%v", held, err)
	}
	if n, err := c.Write([]byte(secret)); n != 0 || !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("stale attach write n=%d err=%v", n, err)
	}
	if sink.output.Len() != 0 {
		t.Fatalf("cached output escaped: %q", sink.output.String())
	}
	// A separate session's stream does not inherit this session's revision.
	other := &sessionOutputConn{Conn: sink, service: h.s, ctx: context.Background(), id: "other-session"}
	if _, err := other.Write([]byte("unrelated")); err != nil {
		t.Fatal(err)
	}
}

type streamScrubOnFlush struct {
	http.ResponseWriter
	first func()
}

func (w *streamScrubOnFlush) Flush() {
	if w.first != nil {
		f := w.first
		w.first = nil
		f()
	}
	if fl, ok := w.ResponseWriter.(http.Flusher); ok {
		fl.Flush()
	}
}

func TestSessionSSERechecksPageAfterCompletedScrub(t *testing.T) {
	secret := "cached SSE page private secret"
	event, _ := json.Marshal(orchapi.AgentEvent{Seq: 1, Type: orchapi.AgentEvMessage, Text: secret})
	h, r := streamOutputFixture(t, orchapi.SessionModePipe, append(event, '\n'))
	var scrubErr error
	var once sync.Once
	wrap := func(w http.ResponseWriter) http.ResponseWriter {
		sw := &streamScrubOnFlush{ResponseWriter: w}
		once.Do(func() {
			sw.first = func() {
				// hEvents has already read its initial page. Complete and clear a hold
				// before it sends that page: checking only the current hold is insufficient.
				scrubErr = h.s.scrubSessionOutputs(context.Background(), core.Scrub{Project: r.Project, Texts: []string{secret}})
			}
		})
		return sw
	}
	h.wrap.Store(&wrap)
	// Through the socket, as the owner on this machine reads it.
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", h.sock)
	}}}
	req, _ := http.NewRequest("GET", "http://zm/v0/sessions/"+r.ID+"/events", nil)
	req.Header.Set("Accept", "text/event-stream")
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	h.wrap.Store(nil)
	if scrubErr != nil {
		t.Fatal(scrubErr)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("SSE status=%d body=%s", resp.StatusCode, b)
	}
	body := string(b)
	if strings.Contains(body, secret) || !strings.Contains(body, core.Redacted) || !strings.Contains(body, "id: 1\n") {
		t.Fatalf("SSE delivered stale page: %q", body)
	}
}

func TestSessionOutputGateReadsDurableHoldAfterServiceRestart(t *testing.T) {
	h, r := streamOutputFixture(t, "", []byte("private output"))
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET state='idle',output_drained=0 WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	_, err := h.d.Store.Registry().Exec(`INSERT INTO session_output_holds(session_id,hold_key,project,subjects,texts,created) VALUES(?,?,?,?,?,?)`, r.ID, "restart", r.Project, "[]", `["private output"]`, 1)
	if err != nil {
		t.Fatal(err)
	}
	fresh := New(h.d)
	defer fresh.cancel()
	// No in-memory revision or live process is needed to honor the durable hold.
	called := false
	err = fresh.withOutputRow(context.Background(), r, 0, func(orchapi.Output) error { called = true; return nil })
	if called || !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("durable gate callback=%v err=%v", called, err)
	}
}
