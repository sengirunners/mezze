package session

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/core"
)

func TestFramedReplayAttachRawBytesAndClosedDrain(t *testing.T) {
	r := holder.NewRing(1 << 20)
	input := append([]byte("\x1b[?1049;1003;1006;2004h"), bytes.Repeat([]byte("\x1b[HInvented redraw\r\n"), 70000)...)
	r.Write(input)
	r.Close()
	expected, from, to, _, _ := r.Next(0, 1<<20)
	server, client := net.Pipe()
	defer client.Close()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		streamReplayAttach(r, server, make(chan struct{}), 1<<20)
	}()
	kind, meta, err := orchapi.ReadReplayFrame(client)
	if err != nil || kind != orchapi.ReplayMetadata {
		t.Fatal("missing metadata", err)
	}
	cp, err := holder.ParseReplay(meta)
	if err != nil || cp.From != from || !cp.State.Modes().AlternateKnown || !cp.State.Modes().Alternate {
		t.Fatal("lost evicted mode setup")
	}
	var got bytes.Buffer
	cursor := from
	for {
		kind, body, err := orchapi.ReadReplayFrame(client)
		if err != nil {
			t.Fatal(err)
		}
		if kind == orchapi.ReplayEnd {
			break
		}
		if kind != orchapi.ReplayOutput {
			t.Fatal("unexpected frame")
		}
		off, data, err := orchapi.ParseReplayBytes(body)
		if err != nil || off != cursor {
			t.Fatal("offset changed")
		}
		got.Write(data)
		cursor += int64(len(data))
	}
	<-done
	if cursor != to || !bytes.Equal(got.Bytes(), expected) {
		t.Fatal("closed stream truncated/raw output changed")
	}
}
func TestFramedReplayAttachGapRequiresFreshSurface(t *testing.T) {
	r := holder.NewRing(16)
	r.Write([]byte("first"))
	server, client := net.Pipe()
	defer client.Close()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	go func() { defer server.Close(); streamReplayAttach(r, server, make(chan struct{}), 1<<20) }()
	kind, _, err := orchapi.ReadReplayFrame(client)
	if err != nil || kind != orchapi.ReplayMetadata {
		t.Fatal(err)
	}
	// The first data frame is now blocked writing, so evict before it drains.
	r.Write(bytes.Repeat([]byte("invented-redraw"), 20))
	kind, _, err = orchapi.ReadReplayFrame(client)
	if err != nil || kind != orchapi.ReplayOutput {
		t.Fatal(err)
	}
	kind, body, err := orchapi.ReadReplayFrame(client)
	if err != nil || kind != orchapi.ReplayGap {
		t.Fatal("silent gap", err)
	}
	off, _, err := orchapi.ParseReplayBytes(body)
	if err != nil || off <= 5 {
		t.Fatal("bad gap cursor")
	}
	if _, _, err = orchapi.ReadReplayFrame(client); err != io.EOF {
		t.Fatal("gap did not close")
	}
}
func TestFramedReplayHandlerLegacyAuthAndTaskGates(t *testing.T) {
	h := newHarness(t, nil)
	ctx := context.Background()
	ss, err := orchclient.Register(ctx, h.c, orchapi.RegisterRequest{Agent: "shell", RuntimeID: "synthetic-replay", State: orchapi.StateNeedsHuman})
	if err != nil {
		t.Fatal(err)
	}
	caught := make(chan struct{})
	close(caught)
	r := holder.NewRing(256)
	raw := []byte("invented raw output\x1b[Htail")
	r.Write(raw)
	r.Close()
	p := &proc{id8: ss.ID, out: r, caught: caught, hookHuman: true, state: orchapi.StateNeedsHuman}
	h.s.mu.Lock()
	h.s.procs[ss.ID] = p
	h.s.mu.Unlock()
	defer func() { h.s.mu.Lock(); delete(h.s.procs, ss.ID); h.s.mu.Unlock() }()
	legacy, err := orchclient.AttachReadOnly(ctx, h.c, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(legacy)
	legacy.Close()
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("legacy attach changed", err)
	}
	stream, err := orchclient.AttachEmbed(ctx, h.c, ss.ID, true)
	if err != nil || !stream.Framed {
		t.Fatal("framed selection", err)
	}
	for {
		kind, _, err := orchapi.ReadReplayFrame(stream)
		if err != nil {
			t.Fatal(err)
		}
		if kind == orchapi.ReplayEnd {
			break
		}
	}
	stream.Close()
	current, err := orchclient.Session(ctx, h.c, ss.ID)
	if err != nil || current.State != orchapi.StateNeedsHuman || p.answered || !p.typedAt.IsZero() {
		t.Fatal("replay answered or typed into session", err)
	}
	router := core.NewRouter()
	h.s.Routes(router)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set(api.HeaderProtocol, api.Protocol)
		router.ServeHTTP(w, req.WithContext(h.d.Authenticate(req)))
	}))
	defer remote.Close()
	if _, err = orchclient.AttachEmbed(ctx, client.NewRemote(remote.URL, nil), ss.ID, true); !api.IsCode(err, api.CodeUnauthenticated) {
		t.Fatal("unauthenticated remote attach bypassed local gate", err)
	}
	// Protocol validation must not disclose or bypass missing-session auth.
	if _, err = h.c.Upgrade(ctx, "/v0/sessions/missing/attach", orchapi.AttachReplayProtocol); !api.IsCode(err, api.CodeNotFound) {
		t.Fatal("protocol check preceded session access", err)
	}
	if _, err = h.c.Upgrade(ctx, "/v0/sessions/"+ss.ID+"/attach?replay=all", orchapi.AttachReplayProtocol); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatal("fresh-surface guard missing", err)
	}
	_, err = h.d.Store.Registry().ExecContext(ctx, `INSERT INTO session_tasks(id,parent,request_json,depth,last_output,state) VALUES(?, '', '{}',1,?,'ended')`, ss.ID, time.Now().UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	h.s.taskIDs.Store(ss.ID, true)
	defer h.s.taskIDs.Delete(ss.ID)
	if _, err = orchclient.AttachEmbed(ctx, h.c, ss.ID, false); !api.IsCode(err, api.CodeConflict) {
		t.Fatal("task interactive attach allowed", err)
	}
	_, err = h.d.Store.Registry().ExecContext(ctx, `UPDATE session_tasks SET text_withheld=1 WHERE id=?`, ss.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = orchclient.AttachEmbed(ctx, h.c, ss.ID, true); !api.IsCode(err, api.CodeConflict) {
		t.Fatal("redacted task attach allowed", err)
	}
}
