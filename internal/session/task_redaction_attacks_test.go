package session

import (
	"bytes"
	"context"
	"fmt"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

type atkProc struct {
	pid   int
	start int64
	group bool
}

func atkTrackSession(t *testing.T, h *harness, ss orchapi.Session) {
	t.Helper()
	var hp int
	var hs int64
	if err := h.d.Store.Registry().QueryRow(`SELECT holder_pid,holder_start FROM session_holders WHERE id=?`, ss.ID).Scan(&hp, &hs); err != nil {
		t.Fatal(err)
	}
	child, ok := procStat(ss.PID)
	if !ok || hs == 0 {
		t.Fatal("cannot identify test session")
	}
	t.Cleanup(func() {
		for _, p := range []atkProc{{pid: ss.PID, start: child.start}, {pid: hp, start: hs}} {
			if st, ok := procStat(p.pid); ok && st.start == p.start && !st.zombie {
				_ = syscall.Kill(p.pid, syscall.SIGKILL)
			}
		}
	})
}

func atkTrack(t *testing.T, h *harness, id string) (holderPID, childPID, pgid int) {
	t.Helper()
	db := h.d.Store.Registry()
	var hs, gs int64
	_ = db.QueryRow(`SELECT holder_pid,holder_start FROM session_holders WHERE id=?`, id).Scan(&holderPID, &hs)
	_ = db.QueryRow(`SELECT pgid,group_start FROM session_tasks WHERE id=?`, id).Scan(&pgid, &gs)
	ss, err := orchclient.Session(context.Background(), h.c, id)
	if err == nil {
		childPID = ss.PID
	}
	var cs int64
	if st, ok := procStat(childPID); ok {
		cs = st.start
	}
	procs := []atkProc{{holderPID, hs, false}, {childPID, cs, false}, {pgid, gs, true}}
	if f := os.Getenv("ATK_PIDLOG"); f != "" {
		fh, _ := os.OpenFile(f, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
		fmt.Fprintf(fh, "%s holder=%d/%d child=%d/%d group=%d/%d\n", t.Name(), holderPID, hs, childPID, cs, pgid, gs)
		fh.Close()
	}
	t.Cleanup(func() {
		h.close() // the daemon first: a killed holder must not race a closing store
		for _, p := range procs {
			if p.pid <= 1 || p.start == 0 {
				continue
			}
			if st, ok := procStat(p.pid); ok && st.start == p.start {
				if p.group {
					if table, err := procTable(); err == nil && table[p.pid].pgid == p.pid {
						_ = syscall.Kill(-p.pid, syscall.SIGKILL)
					}
				} else {
					_ = syscall.Kill(p.pid, syscall.SIGKILL)
				}
			}
		}
	})
	return
}

// A double-forked, setsid escapee keeps stdout. It writes the secret once the marker appears.

func atkEscapee(marker, pidfile, line string) string {
	return `perl -e 'use POSIX qw(setsid); exit if fork; setsid(); $|=1; open(P,">","` + pidfile + `"); print P $$; close P; $t=0; while(!-e "` + marker + `" && $t<600){select(undef,undef,undef,0.05);$t++} print "` + line + `\n"; select(undef,undef,undef,0.3); exit 0' & `
}

func atkWaitFile(t *testing.T, path string) {
	t.Helper()
	dl := time.Now().Add(10 * time.Second)
	for time.Now().Before(dl) {
		if b, err := os.ReadFile(path); err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil && len(bytes.TrimSpace(b)) == 0 {
				// The escapee's open(">") creates the file before it
				// prints its pid: an empty read is "not yet".
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if err != nil || pid <= 1 {
				t.Fatalf("invalid escapee pid %q", b)
			}
			st, ok := procStat(pid)
			if !ok {
				t.Fatal("cannot identify escapee")
			}
			t.Cleanup(func() {
				if p, ok := procStat(pid); ok && p.start == st.start && !p.zombie {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("never appeared: %s", path)
}

func atkFiles(h *harness, id string) map[string]string {
	out := map[string]string{}
	for _, p := range []string{holder.TaskOutputPath(h.home, id), holder.TaskErrorPath(h.home, id), h.s.scrollbackPath(id)} {
		if b, err := os.ReadFile(p); err == nil {
			out[filepath.Base(p)] = string(b)
		} else {
			out[filepath.Base(p)] = "<absent>"
		}
	}
	return out
}

func atkLeaked(m map[string]string, secret string) []string {
	var hits []string
	for k, v := range m {
		safe, err := taskRedactionSafe(v, []string{secret}, 0)
		if err != nil || !safe {
			hits = append(hits, k)
		}
	}
	return hits
}

func atkWaitExit(t *testing.T, h *harness, id string, d time.Duration) {
	t.Helper()
	dl := time.Now().Add(d)
	for time.Now().Before(dl) {
		if _, ok := holder.ReadExit(h.home, id); ok {
			return
		}
		// The daemon may read the exit file, record the end and forget the
		// holder's files (holder.Forget: the exit file, then the log the
		// holder was spawned with) between two polls.
		if _, err := os.Stat(holder.LogPath(h.home, id)); os.IsNotExist(err) {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("holder wrote no exit file in %s", d)
}

func atkWaitClear(t *testing.T, h *harness, id string, d time.Duration) (orchapi.Task, bool) {
	t.Helper()
	dl := time.Now().Add(d)
	var st orchapi.Task
	for time.Now().Before(dl) {
		var err error
		st, err = orchclient.TaskStatus(context.Background(), h.c, id, 0)
		if err == nil && !st.RedactionPending {
			return st, true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return st, false
}

func atkFailureOf(h *harness, id string) string {
	var f string
	_ = h.d.Store.Registry().QueryRow(`SELECT failure FROM session_tasks WHERE id=?`, id).Scan(&f)
	return f
}

// atkCancelDuringDrain starts cancellation without waiting for the holder to
// reap its group anchor: that happens only after output draining has finished.
// The returned function joins the request after the test releases its writer.
func atkCancelDuringDrain(t *testing.T, h *harness, id string) func() orchapi.Task {
	t.Helper()
	child, err := orchclient.Session(context.Background(), h.c, id)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	done := make(chan struct{})
	var task orchapi.Task
	var cancelErr error
	go func() {
		defer close(done)
		task, cancelErr = orchclient.TaskCancel(ctx, h.c, id)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("cancel request did not stop after context cancellation")
		}
	})
	waitGone(t, child.PID)
	if _, exited := holder.ReadExit(h.home, id); exited {
		t.Fatal("holder finished before the drain assertions")
	}
	return func() orchapi.Task {
		t.Helper()
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatalf("cancel request did not finish: %v", ctx.Err())
		}
		if cancelErr != nil || task.State != "ended" {
			t.Fatalf("cancel: state=%s err=%v", task.State, cancelErr)
		}
		return task
	}
}

// A1. Holder unreachable, signal verified: hold remains throughout output drain.

func TestAtkA1UnreachableSignalledWriterAfterScrub(t *testing.T) {
	dir := t.TempDir()
	marker, pidfile := filepath.Join(dir, "m"), filepath.Join(dir, "p")

	secret := "A1 secret rewritten after a completed scrub"
	script := `cat >/dev/null; ` + atkEscapee(marker, pidfile, secret) + ` echo ready; exec sleep 60`
	h, parent, task := zzStart(t, script, "", "")
	h.waitOutput(task.ID, "ready")
	atkWaitFile(t, pidfile)
	atkTrack(t, h, task.ID)
	sc := core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}}
	err := h.d.ScrubAll(context.Background(), sc)
	t.Logf("redaction while running: %v", err)
	if err := os.Remove(holder.SocketPath(h.home, task.ID)); err != nil {
		t.Fatal(err)
	}
	finishCancel := atkCancelDuringDrain(t, h, task.ID)
	_, cleared := atkWaitClear(t, h, task.ID, 500*time.Millisecond)
	if cleared {
		t.Fatal("hold cleared while the holder was still draining")
	}

	err = h.d.ScrubAll(context.Background(), sc)
	if err == nil {
		t.Fatal("redaction completed while the holder was draining")
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	st := finishCancel()
	t.Logf("cancel: state=%s evidence=%q failure=%q", st.State, resultEvidence(st), atkFailureOf(h, task.ID))
	atkWaitExit(t, h, task.ID, 8*time.Second)
	if _, ok := atkWaitClear(t, h, task.ID, 8*time.Second); !ok {
		t.Fatal("hold did not clear after holder exit")
	}
	m := atkFiles(h, task.ID)
	out, oerr := orchclient.Output(context.Background(), h.c, task.ID, 0)
	st, _ = orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
	t.Logf("files: %q", m)
	t.Logf("output route: %q err=%v; status pending=%v", out.Data, oerr, st.RedactionPending)
	if hits := atkLeaked(m, secret); len(hits) > 0 {
		t.Fatalf("LEAK: redaction finished and hold cleared, secret is in %v; pending=%v", hits, st.RedactionPending)
	}
}

func resultEvidence(t orchapi.Task) string {
	if t.Result == nil {
		return ""
	}
	return t.Result.Outcome.Evidence
}

// A2. Control: reachable holder, double fork, secret during the drain.

func atkUncertain(t *testing.T, script, secret string) (*harness, orchapi.Session, orchapi.Task, core.Scrub) {
	t.Helper()
	h, parent, task := zzStart(t, script, "", "")
	h.waitOutput(task.ID, "ready")
	atkTrack(t, h, task.ID)
	if _, err := h.d.Store.Registry().Exec(`UPDATE session_tasks SET home_dev='mismatch' WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(holder.SocketPath(h.home, task.ID)); err != nil {
		t.Fatal(err)
	}
	_, _ = orchclient.TaskCancel(context.Background(), h.c, task.ID)
	sc := core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}}
	if err := h.d.ScrubAll(context.Background(), sc); err == nil {
		t.Fatal("scrub completed with a live uncertain writer")
	}
	return h, parent, task, sc
}

// B2. Uncertain row, holder killed outright, child exits. Everything that could write is gone.

func TestAtkB2HolderKilledAnchorRemains(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "g")
	secret := "B2 holder killed secret"
	h, _, task, _ := atkUncertain(t, `cat >/dev/null; echo ready; while [ ! -e `+gate+` ]; do sleep .05; done; exit 0`, secret)
	var hp int
	var hs, gs int64
	var pgid int
	db := h.d.Store.Registry()
	_ = db.QueryRow(`SELECT holder_pid,holder_start FROM session_holders WHERE id=?`, task.ID).Scan(&hp, &hs)
	_ = db.QueryRow(`SELECT pgid,group_start FROM session_tasks WHERE id=?`, task.ID).Scan(&pgid, &gs)
	if st, ok := procStat(hp); !ok || st.start != hs {
		t.Fatal("holder identity")
	}
	_ = syscall.Kill(hp, syscall.SIGKILL)
	_ = os.WriteFile(gate, nil, 0600)
	st, cleared := atkWaitClear(t, h, task.ID, 12*time.Second)
	r0, _ := h.s.load(context.Background(), task.ID)
	cst, cok := procStat(r0.PID)
	table, _ := procTable()
	var inGroup []int
	for pid, p := range table {
		if p.pgid == pgid {
			inGroup = append(inGroup, pid)
		}
	}
	t.Logf("cleared=%v pending=%v childalive=%v/%v group members=%v kill(-pgid,0)=%v exit=%v", cleared, st.RedactionPending, cok, cst.zombie, inGroup, syscall.Kill(-pgid, 0), func() bool { _, ok := holder.ReadExit(h.home, task.ID); return ok }())
	_, derr := atkDeleteTranscript(h, task.ID)
	t.Logf("transcript cleanup: %v", derr)
	if !cleared {
		t.Fatalf("STUCK: child and holder are gone; only the anchor is left, hold never clears: %+v", st.Result)
	}
}

// B3. PID reuse of the group id.

func TestAtkB5MovedProject(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "g")
	secret := "B5 moved project secret"
	h, parent, task := zzStart(t, `cat >/dev/null; echo ready; while [ ! -e `+gate+` ]; do sleep .05; done; echo '`+secret+`'; `+zzReport, "", "")
	h.waitOutput(task.ID, "ready")
	sc := core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}}
	if err := h.d.ScrubAll(context.Background(), sc); err == nil {
		t.Fatal("expected hold")
	}
	dir := filepath.Join(h.home, "beta")
	_ = os.MkdirAll(dir, 0700)
	beta := h.project("beta", dir)
	as := h.c.As
	h.c.As = ""
	moved, err := orchclient.MoveSession(context.Background(), h.c, task.ID, orchapi.MoveRequest{Project: "beta"})
	h.c.As = as
	t.Logf("from=%s beta=%s moved=%s err=%v", parent.Project, beta.ID, moved.Project, err)
	if !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("task move should be refused: %v", err)
	}
	_ = os.WriteFile(gate, nil, 0600)
	_ = waitTask(t, h, task.ID)
	st, cleared := atkWaitClear(t, h, task.ID, 6*time.Second)
	m := atkFiles(h, task.ID)
	t.Logf("cleared=%v pending=%v withheld=%v leak=%v", cleared, st.RedactionPending, st.Result != nil && st.Result.TextWithheld, atkLeaked(m, secret))
	if !cleared {
		_, derr := atkDeleteTranscript(h, task.ID)
		t.Fatalf("STUCK: hold lives in project %s, row moved to %s; cleanup: %v", parent.Project, beta.ID, derr)
	}
}

func atkDeleteTranscript(h *harness, id string) (int, error) {
	return 0, h.c.Do(context.Background(), "DELETE", "/v0/tasks/"+id+"/transcript", nil, nil)
}

// C1. Hold pending, child ends while the daemon is down.

func TestAtkB1RealHomeMove(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "fake-agent")
	gate := filepath.Join(t.TempDir(), "g")
	secret := "B1 real home move secret"
	if err := os.WriteFile(bin, []byte("#!/bin/sh\ncat >/dev/null; echo ready; echo '"+secret+"'; while [ ! -e "+gate+" ]; do sleep .05; done; exit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	base, _ := os.MkdirTemp("", "zm")
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	homeA, homeB := filepath.Join(base, "a"), filepath.Join(base, "b")
	_ = os.MkdirAll(homeA, 0700)
	mut := func(c *config.Config) { c.File.Agents = map[string][]string{"codex": {bin}, "claude": {bin}} }
	h := startHarness(t, homeA, mut, true)
	parent := h.spawn("shell", "exec sleep 30", nil)
	atkTrackSession(t, h, parent)
	taskClient(h, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "p", Worktree: &no})
	if err != nil {
		h.close()
		t.Fatal(err)
	}
	h.waitOutput(task.ID, "ready")
	atkTrack(t, h, task.ID)
	h.close()
	if err := os.Rename(homeA, homeB); err != nil {
		t.Fatal(err)
	}
	h2 := startHarness(t, homeB, mut, false)
	t.Cleanup(h2.close)
	taskClient(h2, parent)
	time.Sleep(1200 * time.Millisecond)
	r, _ := h2.s.load(context.Background(), task.ID)
	t.Logf("after move: state=%s proc=%v unreachable=%q", r.State, h2.s.proc(task.ID) != nil, h2.s.unreachableWhy(task.ID))
	got, cerr := orchclient.TaskCancel(context.Background(), h2.c, task.ID)
	t.Logf("cancel: %+v err=%v failure=%q", got.Result, cerr, atkFailureOf(h2, task.ID))
	sc := core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}}
	err = h2.d.ScrubAll(context.Background(), sc)
	t.Logf("redaction: %v", err)
	_ = os.WriteFile(gate, nil, 0600)
	time.Sleep(1500 * time.Millisecond)
	var exitA, exitB bool
	_, exitA = holder.ReadExit(homeA, task.ID)
	_, exitB = holder.ReadExit(homeB, task.ID)
	var hp int
	var hs int64
	_ = h2.d.Store.Registry().QueryRow(`SELECT holder_pid,holder_start FROM session_holders WHERE id=?`, task.ID).Scan(&hp, &hs)
	cur, alive := procStat(hp)
	st, cleared := atkWaitClear(t, h2, task.ID, 8*time.Second)
	r, _ = h2.s.load(context.Background(), task.ID)
	cst, cok := procStat(r.PID)
	var withheld bool
	_ = h2.d.Store.Registry().QueryRow(`SELECT text_withheld FROM session_tasks WHERE id=?`, task.ID).Scan(&withheld)
	t.Logf("exit in old path=%v new path=%v; holder alive=%v child alive=%v(%v) cleared=%v pending=%v withheld=%v start=%d", exitA, exitB, alive && !cur.zombie && cur.start == hs, cok, cst.zombie, cleared, st.RedactionPending, withheld, cur.start)
	_, derr := atkDeleteTranscript(h2, task.ID)
	t.Logf("cleanup route: %v", derr)
	var tr []byte
	var comp int
	_ = h2.d.Store.Registry().QueryRow(`SELECT transcript FROM session_tasks WHERE id=?`, task.ID).Scan(&tr)
	_ = h2.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_task_compactions`).Scan(&comp)
	t.Logf("secret still in db transcript=%v; compactions queued and blocked=%d; files=%q", strings.Contains(string(tr), secret), comp, atkFiles(h2, task.ID))
	if !cleared {
		t.Fatal("STUCK after a real home move: writer gone, no way out")
	}
}

// B6. Task moved out of the project before the redaction, emits the secret afterwards.

func TestAtkB6MovedBeforeRedaction(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "g")
	secret := "B6 moved before redaction secret"
	h, parent, task := zzStart(t, `cat >/dev/null; echo ready; while [ ! -e `+gate+` ]; do sleep .05; done; echo '`+secret+`'; `+zzReport, "", "")
	h.waitOutput(task.ID, "ready")
	dir := filepath.Join(h.home, "beta")
	_ = os.MkdirAll(dir, 0700)
	h.project("beta", dir)
	as := h.c.As
	h.c.As = ""
	if _, err := orchclient.MoveSession(context.Background(), h.c, task.ID, orchapi.MoveRequest{Project: "beta"}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("task move should be refused: %v", err)
	}
	h.c.As = as
	err := h.d.ScrubAll(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}})
	if err == nil {
		t.Fatal("redaction should hold the running task")
	}
	_ = os.WriteFile(gate, nil, 0600)
	_ = waitTask(t, h, task.ID)
	o, oerr := orchclient.Output(context.Background(), h.c, task.ID, 0)
	if oerr == nil && strings.Contains(o.Data, secret) {
		t.Fatal("redacted secret in output")
	}
}

// G1 with a keep-alive daemon, as zm daemon runs.

func TestRedactionWithIncompatibleLiveHolder(t *testing.T) {
	dir := t.TempDir()
	marker, pidfile := filepath.Join(dir, "marker"), filepath.Join(dir, "pid")
	secret := "incompatible holder late secret"
	bin := filepath.Join(dir, "agent")
	script := `#!/bin/sh
cat >/dev/null; ` + atkEscapee(marker, pidfile, secret) + ` echo ready; exec sleep 30
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	home := keptHome(t)
	cfg := func(c *config.Config) { c.File.Agents = map[string][]string{"codex": {bin}} }
	h := startHarness(t, home, cfg, true)
	parent := h.spawn("shell", "exec sleep 30", nil)
	atkTrackSession(t, h, parent)
	taskClient(h, parent)
	no := false
	task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "probe", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	h.waitOutput(task.ID, "ready")
	atkWaitFile(t, pidfile)
	atkTrack(t, h, task.ID)
	sc := core.Scrub{Project: parent.Project, Subjects: []string{"other"}, Texts: []string{secret}}
	if err := h.d.ScrubAll(context.Background(), sc); err == nil {
		t.Fatal("running task was not held")
	}
	h.close()
	original := holder.ProtoMajor
	holder.ProtoMajor = "999"
	t.Cleanup(func() { holder.ProtoMajor = original })
	h2 := startHarness(t, home, cfg, true)
	t.Cleanup(h2.close)
	taskClient(h2, parent)
	if why := h2.s.unreachableWhy(task.ID); !strings.Contains(why, "older or newer") {
		t.Fatalf("holder not incompatible: %q", why)
	}
	finishCancel := atkCancelDuringDrain(t, h2, task.ID)
	if _, clear := atkWaitClear(t, h2, task.ID, 500*time.Millisecond); clear {
		t.Fatal("incompatible holder still draining but hold cleared")
	}
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	finishCancel()
	atkWaitExit(t, h2, task.ID, 8*time.Second)
	if _, clear := atkWaitClear(t, h2, task.ID, 8*time.Second); !clear {
		t.Fatal("hold did not clear after incompatible holder exited")
	}
	if hits := atkLeaked(atkFiles(h2, task.ID), secret); len(hits) > 0 {
		t.Fatalf("leak: %v", hits)
	}
}

func TestDiscardUnprovableRedactionHold(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	secret := "discard pending redaction secret"
	h, _, task, _ := atkUncertain(t, `cat >/dev/null; echo ready; while [ ! -e `+gate+` ]; do sleep .05; done; echo '`+secret+`'; `+zzReport, secret)
	state, err := orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
	if err != nil || state.State != "running" {
		t.Fatalf("unverified child must stay running: %+v %v", state, err)
	}
	// A previous release could persist an ended row while this writer was
	// still alive. Preserve the discard/leak regression for those legacy rows
	// without letting the current stop path create another one.
	h.s.endWithGroup(context.Background(), task.ID, orchapi.Outcome{Kind: orchapi.OutcomeFailed, Evidence: "child may still be running"}, false)
	h.s.completeTask(task.ID)
	url := "/v0/tasks/" + task.ID + "/transcript?discard_redaction=1"
	if err := h.c.Do(context.Background(), "DELETE", url, nil, nil); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("young hold: %v", err)
	}
	if _, err := h.d.Store.Registry().Exec(`UPDATE session_task_holds SET created=? WHERE task_id=?`, time.Now().Add(-time.Minute).UnixMilli(), task.ID); err != nil {
		t.Fatal(err)
	}
	var originalPath string
	if err := h.d.Store.Registry().QueryRow(`SELECT home_path FROM session_holders WHERE id=?`, task.ID).Scan(&originalPath); err != nil {
		t.Fatal(err)
	}
	if _, err := h.d.Store.Registry().Exec(`UPDATE session_holders SET home_path=? WHERE id=?`, originalPath+"-old", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Do(context.Background(), "DELETE", url, nil, nil); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("unsealed former home: %v", err)
	}
	if _, err := h.d.Store.Registry().Exec(`UPDATE session_holders SET home_path=? WHERE id=?`, originalPath, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.c.Do(context.Background(), "DELETE", url, nil, nil); err != nil {
		t.Fatalf("discard pending redaction: %v", err)
	}
	st, err := orchclient.TaskStatus(context.Background(), h.c, task.ID, 0)
	if err != nil || st.RedactionPending {
		t.Fatalf("hold remains: %+v %v", st, err)
	}
	// A prior holder's atomic scrollback writer must fail before creating text.
	if err := os.WriteFile(h.s.scrollbackPath(task.ID)+".tmp", []byte(secret), 0600); err == nil {
		t.Fatal("old holder could recreate temporary scrollback")
	}
	if err := h.c.Do(context.Background(), "DELETE", "/v0/tasks/"+task.ID+"/transcript", nil, nil); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("ordinary cleanup removed discard barriers: %v", err)
	}
	if err := h.d.ScrubAll(context.Background(), core.Scrub{Project: taskProject(t, h, task.ID), Subjects: []string{"other"}, Texts: []string{secret}}); err == nil {
		t.Fatal("later scrub should still wait for the writer")
	}
	h.s.pruneScrollback(0)
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	atkWaitExit(t, h, task.ID, 8*time.Second)
	for _, p := range []string{holder.TaskOutputPath(h.home, task.ID), holder.TaskErrorPath(h.home, task.ID), h.s.scrollbackPath(task.ID), h.s.scrollbackPath(task.ID) + ".tmp"} {
		fi, err := os.Stat(p)
		if err != nil || !fi.IsDir() {
			t.Fatalf("output barrier missing: %s %v", p, err)
		}
	}
	var transcript []byte
	if err := h.d.Store.Registry().QueryRow(`SELECT transcript FROM session_tasks WHERE id=?`, task.ID).Scan(&transcript); err != nil || strings.Contains(string(transcript), secret) {
		t.Fatalf("retained transcript: %q %v", transcript, err)
	}
}

func taskProject(t *testing.T, h *harness, id string) string {
	t.Helper()
	r, ok := h.s.load(context.Background(), id)
	if !ok {
		t.Fatal("task session missing")
	}
	return r.Project
}

func TestRedactionRecoveryWhenHolderStartMissing(t *testing.T) {
	gate := filepath.Join(t.TempDir(), "gate")
	h, _, task, _ := atkUncertain(t, `cat >/dev/null; echo ready; while [ ! -e `+gate+` ]; do sleep .05; done; exit 0`, "zero start missing holder secret")
	var pid int
	var start int64
	if err := h.d.Store.Registry().QueryRow(`SELECT holder_pid,holder_start FROM session_holders WHERE id=?`, task.ID).Scan(&pid, &start); err != nil {
		t.Fatal(err)
	}
	if p, ok := procStat(pid); !ok || start == 0 || p.start != start {
		t.Fatal("cannot verify holder")
	}
	if _, err := h.d.Store.Registry().Exec(`UPDATE session_holders SET holder_start=0 WHERE id=?`, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gate, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if st, clear := atkWaitClear(t, h, task.ID, 12*time.Second); !clear {
		t.Fatalf("absent zero-start holder kept the hold: %+v", st)
	}
}

// The escapee's pid file exists empty before its pid is written: the wait
// reads that as "not yet", never as an invalid pid.
func TestAtkWaitFileWaitsThroughAnEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pid")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(path, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	}()
	atkWaitFile(t, path)
}
