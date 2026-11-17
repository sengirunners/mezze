package session

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/config"
)

// Use the real registry without a child writing concurrently to the fixture
// spool. A newly constructed Service must resume using only its durable cursor.
func captureFixture(t *testing.T) (*harness, string, *os.File, int64) {
	t.Helper()
	h := newHarness(t, nil)
	ss, err := orchclient.Register(context.Background(), h.c, orchapi.RegisterRequest{Agent: "codex", RuntimeID: "capture-fixture", Cwd: h.home, State: orchapi.StateRunning})
	if err != nil {
		t.Fatal(err)
	}
	last := time.Now().Add(-time.Second).UnixMilli()
	if _, err := h.d.Store.Registry().Exec(`INSERT INTO session_tasks(id,parent,request_json,depth,last_output) VALUES(?,?,'{}',0,?)`, ss.ID, ss.Address, last); err != nil {
		t.Fatal(err)
	}
	path := holder.TaskOutputPath(h.home, ss.ID)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return h, ss.ID, f, last
}

func captureState(t *testing.T, h *harness, id string) (offset, partial, last int64) {
	t.Helper()
	if err := h.d.Store.Registry().QueryRow(`SELECT output_offset,length(transcript),last_output FROM session_tasks WHERE id=?`, id).Scan(&offset, &partial, &last); err != nil {
		t.Fatal(err)
	}
	return
}

func TestTaskCaptureSpoolBoundsPartialPersistence(t *testing.T) {
	h, id, spool, last := captureFixture(t)
	db := h.d.Store.Registry()
	if _, err := db.Exec(`CREATE TABLE capture_sizes(bytes INTEGER NOT NULL);
CREATE TRIGGER capture_size AFTER UPDATE OF transcript ON session_tasks BEGIN
INSERT INTO capture_sizes(bytes) VALUES(length(NEW.transcript)); END;`); err != nil {
		t.Fatal(err)
	}
	chunk := bytes.Repeat([]byte("x"), 32<<10)
	for off := int64(0); off < 16<<20; off += int64(len(chunk)) {
		if _, err := spool.Write(chunk); err != nil {
			t.Fatal(err)
		}
		h.s.captureTask(id, off, chunk)
		next, partial, gotLast := captureState(t, h, id)
		if next != off+int64(len(chunk)) || partial != 0 || gotLast != last {
			t.Fatalf("newline-free capture offset=%d want=%d persisted_partial=%d progress_changed=%v", next, off+int64(len(chunk)), partial, gotLast != last)
		}
	}
	var updates, largest int64
	if err := db.QueryRow(`SELECT count(*),max(bytes) FROM capture_sizes`).Scan(&updates, &largest); err != nil || updates != 512 || largest != 0 {
		t.Fatalf("partial persistence audit updates=%d largest=%d err=%v", updates, largest, err)
	}
	// Completing the raw line is not progress; the following runtime event is.
	tail := []byte("\n" + `{"type":"item.completed","item":{"type":"agent_message","text":"after burst"}}` + "\n")
	if _, err := spool.Write(tail); err != nil {
		t.Fatal(err)
	}
	h.s.captureTask(id, 16<<20, tail)
	next, partial, gotLast := captureState(t, h, id)
	if next != 16<<20+int64(len(tail)) || partial != 0 || gotLast <= last {
		t.Fatalf("completed event offset=%d partial=%d progress=%v", next, partial, gotLast > last)
	}
}

func TestTaskCaptureSpoolSplitProgressReplayAndGap(t *testing.T) {
	h, id, spool, last := captureFixture(t)
	var end int64
	write := func(text string) (int64, []byte) {
		t.Helper()
		off := end
		data := []byte(text)
		if _, err := spool.Write(data); err != nil {
			t.Fatal(err)
		}
		end += int64(len(data))
		return off, data
	}
	assertState := func(wantProgress bool) {
		t.Helper()
		next, partial, got := captureState(t, h, id)
		if next != end || partial != 0 || (got > last) != wantProgress {
			t.Fatalf("offset=%d want=%d partial=%d progress=%v want=%v", next, end, partial, got > last, wantProgress)
		}
	}
	off, data := write("diagnostic\n" + `{"type":"tool_progress"}` + "\n" + `{"type":"item.compl`)
	h.s.captureTask(id, off, data)
	assertState(false)
	// No in-memory partial buffer survives service reconstruction/adoption.
	resumed := &Service{d: h.d}
	off, data = write(`eted","item":{"type":"agent_message","text":"done"}}`)
	resumed.captureTask(id, off, data)
	assertState(false)                  // a complete JSON object without its newline is not progress
	write("\n" + `{"type":"turn.compl`) // this frame is lost from the holder ring
	off, data = write(`eted"}` + "\n")
	resumed.captureTask(id, off, data)
	assertState(true) // the spool recovers both missing and partial event bytes
	if _, err := h.d.Store.Registry().Exec(`UPDATE session_tasks SET last_output=? WHERE id=?`, last, id); err != nil {
		t.Fatal(err)
	}
	resumed.captureTask(id, off, data) // repeated adoption replay is not progress
	assertState(false)
	off, data = write(`{"type":"tool_progress"}` + "\nraw output\n")
	resumed.captureTask(id, off, data)
	assertState(false) // old completed events in the spool must not be replayed
}

func TestTaskCaptureLegacyPartialAndMissingSpoolGap(t *testing.T) {
	h, id, spool, last := captureFixture(t)
	if err := spool.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(holder.TaskOutputPath(h.home, id)); err != nil {
		t.Fatal(err)
	}
	prefix := []byte(`{"type":"item.compl`)
	h.s.captureTask(id, 0, prefix)
	next, partial, gotLast := captureState(t, h, id)
	if next != int64(len(prefix)) || partial != int64(len(prefix)) || gotLast != last {
		t.Fatalf("legacy partial offset=%d bytes=%d progress_changed=%v", next, partial, gotLast != last)
	}
	tail := []byte(`eted"}` + "\n")
	h.s.captureTask(id, next, tail)
	next, partial, gotLast = captureState(t, h, id)
	if next != int64(len(prefix)+len(tail)) || partial != next || gotLast <= last {
		t.Fatalf("legacy completion offset=%d bytes=%d progress=%v", next, partial, gotLast > last)
	}
	h.s.captureTask(id, next+128, []byte(`{"type":"turn.completed"}`+"\n"))
	var failed bool
	if err := h.d.Store.Registry().QueryRow(`SELECT failure='task output overflow' FROM session_tasks WHERE id=?`, id).Scan(&failed); err != nil || !failed {
		t.Fatalf("missing-spool gap must fail closed: failed=%v err=%v", failed, err)
	}
	end, _, _ := captureState(t, h, id)
	if end != next {
		t.Fatalf("missing-spool gap advanced offset from %d to %d", next, end)
	}
}

func TestTaskCaptureSpoolCap(t *testing.T) {
	h, id, spool, last := captureFixture(t)
	if err := spool.Truncate(taskTranscriptLimit); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Store.Registry().Exec(`UPDATE session_tasks SET output_offset=? WHERE id=?`, taskTranscriptLimit-1, id); err != nil {
		t.Fatal(err)
	}
	h.s.captureTask(id, taskTranscriptLimit-1, []byte("x\n"))
	next, partial, gotLast := captureState(t, h, id)
	var failed bool
	if err := h.d.Store.Registry().QueryRow(`SELECT failure='task output exceeded 32 MiB' FROM session_tasks WHERE id=?`, id).Scan(&failed); err != nil || !failed || next != taskTranscriptLimit-1 || partial != 0 || gotLast != last {
		t.Fatalf("cap failure=%v offset=%d partial=%d progress_changed=%v err=%v", failed, next, partial, gotLast != last, err)
	}
}

func TestTaskCaptureBurstAdoptionKeepsSplitTerminal(t *testing.T) {
	home := keptHome(t)
	release := filepath.Join(t.TempDir(), "release")
	bin := filepath.Join(t.TempDir(), "agent")
	prefix := "\n" + `{"type":"item.compl`
	script := "#!/bin/sh\nhead -c 2097152 /dev/zero | tr '\\000' x\nprintf '%s' " + shellQuote(prefix) +
		"\nwhile [ ! -e " + shellQuote(release) + " ]; do sleep .02; done\nprintf '%s\\n' " +
		shellQuote(`eted","item":{"type":"agent_message","text":"{\"finished\":\"not_done\",\"text\":\"after adoption\"}"}}`) + " " + shellQuote(`{"type":"turn.completed"}`) + "\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := func(c *config.Config) { c.File.Agents = map[string][]string{"codex": {bin}} }
	h1 := startHarness(t, home, cfg, true)
	t.Cleanup(h1.close)
	parent := h1.spawn("shell", "exec sleep 30", nil)
	taskClient(h1, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h1.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "burst", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	child, err := orchclient.Session(context.Background(), h1.c, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ps pids
	ps.add(t, parent.PID, child.PID, h1.holderPID(parent.ID), h1.holderPID(child.ID))
	want := int64(2<<20 + len(prefix))
	deadline := time.Now().Add(5 * time.Second)
	var next, partial int64
	for time.Now().Before(deadline) {
		next, partial, _ = captureState(t, h1, task.ID)
		if next == want {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if next != want || partial != 0 {
		t.Fatalf("pre-adoption capture offset=%d want=%d partial=%d %s", next, want, partial, taskCaptureDiagnostic(h1, task.ID))
	}
	h1.close()
	h2 := startHarness(t, home, cfg, false)
	t.Cleanup(h2.close)
	taskClient(h2, parent)
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	done := waitTask(t, h2, task.ID)
	if done.Result == nil || done.Result.Finished != "not_done" || done.Result.Text != "after adoption" || done.Result.Outcome.Kind != orchapi.OutcomeOK {
		t.Fatalf("split terminal result lost after adoption: %+v %s", done.Result, taskCaptureDiagnostic(h2, task.ID))
	}
}

// Timeout diagnostics contain only state and byte counts, never transcript text.
func taskCaptureDiagnostic(h *harness, id string) string {
	var state string
	var offset, partial int64
	var failed bool
	err := h.d.Store.Registry().QueryRow(`SELECT state,output_offset,length(transcript),failure!='' FROM session_tasks WHERE id=?`, id).Scan(&state, &offset, &partial, &failed)
	var sessionState orchapi.SessionState
	var total int64
	if r, ok := h.s.load(context.Background(), id); ok {
		sessionState, total = r.State, r.outBytes
	}
	spoolBytes := int64(-1)
	if st, e := os.Stat(holder.TaskOutputPath(h.home, id)); e == nil {
		spoolBytes = st.Size()
	}
	var exit holder.Exit
	exited := false
	if b, e := os.ReadFile(holder.ExitPath(h.home, id)); e == nil {
		exited = json.Unmarshal(b, &exit) == nil
	}
	return fmt.Sprintf("task_state=%q session_state=%q capture_offset=%d partial_bytes=%d session_bytes=%d spool_bytes=%d failed=%v query_ok=%v holder_exited=%v holder_exit_bytes=%d holder_exit_code=%d", state, sessionState, offset, partial, total, spoolBytes, failed, err == nil, exited, exit.OutBytes, exit.Code)
}
