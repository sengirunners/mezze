package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/authz"
	"github.com/sengirunners/potluck/kit/core"
)

func holdMetadataOutput(t *testing.T, h *harness, r row) {
	t.Helper()
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET state='idle',output_drained=0,output_redacted=1 WHERE id=?`, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Store.Registry().Exec(`INSERT INTO session_output_holds(session_id,hold_key,project,subjects,texts,created) VALUES(?,?,?,?,?,?)`, r.ID, "metadata", r.Project, "[]", `["metadata private secret"]`, 1); err != nil {
		t.Fatal(err)
	}
}

func TestSessionMetadataWithholdsCachedFieldsDuringHold(t *testing.T) {
	const secret = "metadata private secret"
	h, r := streamOutputFixture(t, "", []byte(secret))
	holdMetadataOutput(t, h, r)
	ss := r.Session
	ss.State = orchapi.StateNeedsHuman
	ss.Title, ss.Note, ss.Activity, ss.TermTitle = secret, secret, secret, secret
	ss.Command = []string{"shell", secret}
	ss.Cwd, ss.Worktree, ss.Branch, ss.Scratch, ss.Sandbox = secret, secret, secret, secret, secret
	ss.Outcome = &orchapi.Outcome{Kind: orchapi.OutcomeFailed, Note: secret, Evidence: secret}
	ss.RuntimeAgent = &orchapi.RuntimeAgent{Agent: "codex", RuntimeID: "runtime-id", Task: secret, Model: "model"}
	ss.Subagents = []orchapi.Subagent{{ID: "sub-id", Type: "Explore", Description: secret, Activity: secret, State: orchapi.StateRunning}}
	ss.Needs = &orchapi.SessionNeeds{Kind: orchapi.NeedsInput, Subagent: "sub-id"}
	ss.LastPrompt = &orchapi.SessionLastPrompt{Text: secret}
	ss.Fg = &orchapi.SessionFg{PID: 42, Name: secret}
	ss.Unreachable = secret
	out := h.s.dressWith(core.WithSubject(context.Background(), h.owner()), ss, "mini/parent00")
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("held metadata escaped: %s", data)
	}
	if out.Address != ss.Address || out.Project != ss.Project || out.State != ss.State || out.Parent != "mini/parent00" || out.Needs == nil || out.Needs.Subagent != "sub-id" || out.RuntimeAgent == nil || out.RuntimeAgent.RuntimeID != "runtime-id" || out.RuntimeAgent.Model != "model" || len(out.Subagents) != 1 || out.Subagents[0].ID != "sub-id" {
		t.Fatalf("identity or routing changed: %+v", out)
	}
	if ss.Outcome.Evidence != secret || ss.RuntimeAgent.Task != secret || ss.Subagents[0].Description != secret {
		t.Fatal("mask mutated cached input")
	}
}

func TestSessionMetadataReloadsAfterCompletedScrub(t *testing.T) {
	const secret = "completed metadata private secret"
	h, r := streamOutputFixture(t, "", []byte(secret))
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET title=?,note=?,activity=?,term_title=?,command=?,shared=1 WHERE id=?`, secret, secret, secret, secret, `["shell","`+secret+`"]`, r.ID); err != nil {
		t.Fatal(err)
	}
	cached, ok := h.s.load(context.Background(), r.ID)
	if !ok {
		t.Fatal("missing session")
	}
	// A cached event's lifecycle remains its own snapshot, but descriptive text
	// must not be restored from before the completed redaction.
	cached.State = orchapi.StateRunning
	h.s.lastPrompts.Store(r.Address, orchapi.SessionLastPrompt{Text: secret})
	if err := h.s.scrubSessionOutputs(context.Background(), core.Scrub{Project: r.Project, Texts: []string{secret}}); err != nil {
		t.Fatal(err)
	}
	out := h.s.dressWith(core.WithSubject(context.Background(), h.owner()), cached.Session, "")
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) || !strings.Contains(string(data), core.Redacted) || out.LastPrompt != nil {
		t.Fatalf("stale metadata restored after completed scrub: %s", data)
	}
	if out.State != orchapi.StateRunning {
		t.Fatalf("changed event lifecycle snapshot: %s", out.State)
	}
}

func TestSessionMetadataQueryFailureFailsClosed(t *testing.T) {
	h, r := streamOutputFixture(t, "", nil)
	r.Title = "metadata private secret"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got := h.s.maskSessionOutputMetadata(ctx, r.Session)
	if got.Title != "" || got.Address != r.Address || got.State != r.State {
		t.Fatalf("query failure projection: %+v", got)
	}
}

func TestSessionMetadataHTTPHolds(t *testing.T) {
	const secret = "metadata private secret"
	h, r := streamOutputFixture(t, "", []byte(secret))
	holdMetadataOutput(t, h, r)
	subs, _ := json.Marshal([]orchapi.Subagent{{ID: "sub-id", Description: secret, Activity: secret, State: orchapi.StateRunning, Updated: time.Now()}})
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET title=?,note=?,activity=?,subagents=? WHERE id=?`, secret, secret, secret, string(subs), r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Store.Registry().Exec(`INSERT INTO session_activity(session,subagent,at,text) VALUES(?,?,?,?)`, r.ID, "sub-id", time.Now().UnixMilli(), secret); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, path string
		handler    http.HandlerFunc
		status     int
	}{
		{"show", "/v0/sessions/" + r.ID, h.s.hGet, http.StatusOK},
		{"list", "/v0/sessions?project=" + r.Project, h.s.hList, http.StatusOK},
		{"activity", "/v0/sessions/" + r.ID + "/activity", h.s.hActivity, http.StatusConflict},
		{"subagents", "/v0/sessions/" + r.ID + "/subagents", h.s.hSubagents, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tc.path, nil)
			req.SetPathValue("id", r.ID)
			req = req.WithContext(core.WithSubject(req.Context(), h.owner()))
			w := httptest.NewRecorder()
			tc.handler(w, req)
			if w.Code != tc.status || strings.Contains(w.Body.String(), secret) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestSessionMetadataReloadPreservesTerminalTitleScope(t *testing.T) {
	const secret = "metadata private secret"
	h, r := streamOutputFixture(t, "", []byte(secret))
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET title=?,term_title=?,command=? WHERE id=?`, secret, "private shell terminal topic", `["sh"]`, r.ID); err != nil {
		t.Fatal(err)
	}
	cached, ok := h.s.load(context.Background(), r.ID)
	if !ok {
		t.Fatal("missing session")
	}
	if err := h.s.scrubSessionOutputs(context.Background(), core.Scrub{Project: r.Project, Texts: []string{secret}}); err != nil {
		t.Fatal(err)
	}
	sub := &authz.Subject{Scope: authz.FromTuples([]authz.Tuple{{Relation: authz.Member, Object: authz.ProjectObject(r.Project)}}, nil)}
	out := h.s.dressWith(core.WithSubject(context.Background(), sub), cached.Session, "")
	if out.TermTitle != "" {
		t.Fatalf("metadata reload exposed private terminal title: %q", out.TermTitle)
	}
}

func TestSessionMetadataReloadDoesNotCrossSnapshotScope(t *testing.T) {
	h, r := streamOutputFixture(t, "", nil)
	other := h.project("metadata-other")
	if _, err := h.d.Store.Registry().Exec(`UPDATE sessions SET project=?,title=?,output_redacted=1 WHERE id=?`, other.ID, "new project private title", r.ID); err != nil {
		t.Fatal(err)
	}
	cached := r.Session
	cached.Title = "old cached title"
	sub := &authz.Subject{Scope: authz.FromTuples([]authz.Tuple{{Relation: authz.Member, Object: authz.ProjectObject(r.Project)}}, nil)}
	out := h.s.dressWith(core.WithSubject(context.Background(), sub), cached, "")
	if out.Project != r.Project || out.Title != "" {
		t.Fatalf("metadata crossed snapshot scope: project=%q title=%q", out.Project, out.Title)
	}
}
