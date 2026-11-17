package session

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
)

const pendingDBSecret = "uncertain-writer-database-canary"
const pendingDBFailure = "task descendant may still be running; inspect it manually"

type pendingDBFixture struct {
	h  *harness
	id string
	sc core.Scrub
}

func pendingDBCopiesFixture(t *testing.T, state string) pendingDBFixture {
	t.Helper()
	h := newHarness(t, nil)
	// These are synthetic rows, not launched agents. Keep background task
	// delivery/recovery out of the assertions; direct scrub/recovery calls below
	// exercise the same production paths without a writer or child process.
	h.s.cancel()
	p := h.project("pending-copies", h.home)
	ss, err := orchclient.Register(context.Background(), h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "pending-db", Cwd: h.home})
	if err != nil {
		t.Fatal(err)
	}
	if state == "ended" {
		if _, err := orchclient.Register(context.Background(), h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "pending-db", End: true}); err != nil {
			t.Fatal(err)
		}
	}
	db := h.d.Store.Registry()
	// Unknown session text columns already participate in the schema-driven
	// scrubber, including JSON with fields a future runtime could retain.
	if _, err := db.Exec(`ALTER TABLE sessions ADD COLUMN retained_copy TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	outcome := `{"kind":"failed","evidence":"evidence ` + pendingDBSecret + `","note":"outcome note ` + pendingDBSecret + `"}`
	command := `["codex","argument ` + pendingDBSecret + `"]`
	retained := `{"last_prompt":{"text":"` + pendingDBSecret + `"},"counter":9007199254740993,"flag":true,"nothing":null}`
	if _, err := db.Exec(`UPDATE sessions SET title=?,note=?,command=?,outcome=?,retained_copy=? WHERE id=?`,
		"title "+pendingDBSecret, "note "+pendingDBSecret, command, outcome, retained, ss.ID); err != nil {
		t.Fatal(err)
	}
	request := `{"agent":"codex","prompt":"prompt ` + pendingDBSecret + `","cwd":"retained ` + pendingDBSecret + `","counter":9007199254740993,"flag":true,"nothing":null}`
	result := `{"finished":"not_done","text":"result ` + pendingDBSecret + `","outcome":` + outcome + `,"counter":9007199254740993,"flag":true,"nothing":null}`
	if _, err := db.Exec(`INSERT INTO session_tasks(id,parent,request_json,depth,work,state,transcript,last_output,failure,result,mail_done,comment_done,mail_error,comment_error,request_verified)
 VALUES(?,? ,?,0,'bd:held-copy',?,?,?, ?,?,'sent','sent',?,?,1)`,
		ss.ID, ss.Address, request, state, []byte("transcript "+pendingDBSecret), time.Now().UnixMilli(), pendingDBFailure, result,
		"mail error "+pendingDBSecret, "comment error "+pendingDBSecret); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO session_handoffs(source,status,error) VALUES(?,'failed',?)`, ss.ID, "handoff error "+pendingDBSecret); err != nil {
		t.Fatal(err)
	}
	return pendingDBFixture{h: h, id: ss.ID, sc: core.Scrub{Project: p.ID, Subjects: []string{"bd:held-copy"}, Texts: []string{pendingDBSecret}}}
}

func pendingDBCopies(t *testing.T, f pendingDBFixture) map[string]string {
	t.Helper()
	var transcript []byte
	var result, request, mailErr, commentErr, handoffErr string
	if err := f.h.d.Store.Registry().QueryRow(`SELECT t.transcript,t.result,t.request_json,t.mail_error,t.comment_error,h.error
 FROM session_tasks t JOIN session_handoffs h ON h.source=t.id WHERE t.id=?`, f.id).
		Scan(&transcript, &result, &request, &mailErr, &commentErr, &handoffErr); err != nil {
		t.Fatal(err)
	}
	copies := map[string]string{"transcript": string(transcript), "result": result, "request": request,
		"mail_error": mailErr, "comment_error": commentErr, "handoff_error": handoffErr}
	fields, err := f.h.s.taskSessionTexts(context.Background(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range fields {
		if field.valid {
			copies["session."+field.name] = field.value
		}
	}
	return copies
}

func assertPendingDBHold(t *testing.T, f pendingDBFixture) {
	t.Helper()
	var holds int
	var withheld, verified bool
	var state, failure string
	if err := f.h.d.Store.Registry().QueryRow(`SELECT state,failure,text_withheld,request_verified,
 (SELECT count(*) FROM session_task_holds WHERE task_id=?) FROM session_tasks WHERE id=?`, f.id, f.id).
		Scan(&state, &failure, &withheld, &verified, &holds); err != nil {
		t.Fatal(err)
	}
	if holds != 1 || !withheld || verified || failure != pendingDBFailure {
		t.Fatalf("pending policy changed: state=%s holds=%d withheld=%v verified=%v failure=%q", state, holds, withheld, verified, failure)
	}
}

func TestScrubPendingTaskDBCopiesBeforeWriterProof(t *testing.T) {
	f := pendingDBCopiesFixture(t, "ended")
	before := pendingDBCopies(t, f)
	for _, key := range []string{"transcript", "result", "request", "mail_error", "comment_error", "handoff_error",
		"session.title", "session.note", "session.command", "session.outcome", "session.retained_copy"} {
		if !strings.Contains(before[key], pendingDBSecret) {
			t.Fatalf("fixture omitted canary in %s", key)
		}
	}
	paths := []string{holder.TaskOutputPath(f.h.home, f.id), holder.TaskErrorPath(f.h.home, f.id), f.h.s.scrollbackPath(f.id)}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(pendingDBSecret), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writer, err := os.OpenFile(paths[0], os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	original, err := writer.Stat()
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := f.h.s.scrubTasks(context.Background(), f.sc); !api.IsCode(err, api.CodeUnavailable) {
			t.Fatalf("uncertain writer no longer holds redaction: %v", err)
		}
		for name, copy := range pendingDBCopies(t, f) {
			if strings.Contains(copy, pendingDBSecret) {
				t.Fatalf("retained database copy %s still has the canary while writer is uncertain", name)
			}
		}
		assertPendingDBHold(t, f)
		for _, path := range paths {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatalf("uncertain writer output path remains: %s err=%v", path, err)
			}
		}
	}
	for _, name := range []string{"result", "request", "session.retained_copy"} {
		var data map[string]json.RawMessage
		if err := json.Unmarshal([]byte(pendingDBCopies(t, f)[name]), &data); err != nil {
			t.Fatal(err)
		}
		if string(data["counter"]) != "9007199254740993" || string(data["flag"]) != "true" || string(data["nothing"]) != "null" {
			t.Fatalf("scrubbing changed nontext metadata in %s: %v", name, data)
		}
	}
	got, err := writer.Stat()
	if err != nil || !os.SameFile(original, got) {
		t.Fatalf("live output inode changed: %v", err)
	}
	if _, err := writer.WriteString(" late " + pendingDBSecret); err != nil {
		t.Fatal(err)
	}
	written := make([]byte, len(pendingDBSecret)*2+len(" late "))
	if _, err := writer.ReadAt(written, 0); err != nil || string(written) != pendingDBSecret+" late "+pendingDBSecret {
		t.Fatalf("scrub rewrote or truncated the live output descriptor: %q err=%v", written, err)
	}
	view, err := orchclient.TaskStatus(context.Background(), f.h.c, f.id, 0)
	if err != nil || !view.RedactionPending || view.Result == nil || !view.Result.TextWithheld || view.Result.Text != "" {
		t.Fatalf("pending output escaped withholding: %+v err=%v", view, err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	// Only independent proof, not purging the copies, permits normal recovery.
	if _, err := f.h.d.Store.Registry().Exec(`UPDATE session_tasks SET group_stopped=1 WHERE id=?`, f.id); err != nil {
		t.Fatal(err)
	}
	f.h.s.recoverTaskHolds(context.Background(), f.id)
	view, err = orchclient.TaskStatus(context.Background(), f.h.c, f.id, 0)
	if err != nil || view.RedactionPending || view.Result == nil || view.Result.TextWithheld || strings.Contains(view.Result.Text, pendingDBSecret) {
		t.Fatalf("writer proof did not restore the existing cleanup path: %+v err=%v", view, err)
	}
	var failure string
	if err := f.h.d.Store.Registry().QueryRow(`SELECT failure FROM session_tasks WHERE id=?`, f.id).Scan(&failure); err != nil || failure != pendingDBFailure {
		t.Fatalf("uncertainty history changed: %q err=%v", failure, err)
	}
}

func TestScrubPendingTaskDBCopiesDropsMalformedJSON(t *testing.T) {
	f := pendingDBCopiesFixture(t, "ended")
	if _, err := f.h.d.Store.Registry().Exec(`UPDATE session_tasks SET result=?,request_json=? WHERE id=?`,
		`{"broken":"`+pendingDBSecret, `{"broken":"`+pendingDBSecret, f.id); err != nil {
		t.Fatal(err)
	}
	if err := f.h.s.scrubTasks(context.Background(), f.sc); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatal(err)
	}
	copies := pendingDBCopies(t, f)
	if copies["result"] != "" || copies["request"] != "" {
		t.Fatal("malformed structured copies retained data")
	}
	assertPendingDBHold(t, f)
}

func TestScrubPendingTaskDBCopiesLeavesRunningCaptureUnchanged(t *testing.T) {
	f := pendingDBCopiesFixture(t, "running")
	before := pendingDBCopies(t, f)
	if err := f.h.s.scrubTasks(context.Background(), f.sc); !api.IsCode(err, api.CodeUnavailable) {
		t.Fatal(err)
	}
	for name, copy := range pendingDBCopies(t, f) {
		if !bytes.Equal([]byte(copy), []byte(before[name])) {
			t.Fatalf("running capture copy %s changed", name)
		}
	}
	assertPendingDBHold(t, f)
}
