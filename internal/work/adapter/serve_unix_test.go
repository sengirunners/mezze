//go:build unix

package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

func slow(ctx context.Context, req orchapi.AdapterRequest) (any, error) {
	a, _ := Args[struct{ MS int }](req)
	select {
	case <-time.After(time.Duration(a.MS) * time.Millisecond):
		return map[string]string{"done": "slow"}, nil
	case <-ctx.Done():
		return nil, api.Errorf(api.CodeUnavailable, "cancelled")
	}
}

// heldWrite acknowledges entry from the child and waits for the test to release it.
func heldWrite(ctx context.Context, req orchapi.AdapterRequest) (any, error) {
	a, err := Args[struct{ Barrier string }](req)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", a.Barrier)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if _, err := conn.Write([]byte{1}); err != nil {
		return nil, err
	}
	var release [1]byte
	if _, err := io.ReadFull(conn, release[:]); err != nil {
		return nil, err
	}
	return map[string]string{"done": "held"}, nil
}

// testHandlers answer "get" with the id, "slow" after args.ms milliseconds
// (or when cancelled), and "pid" with the process id.
func testHandlers() map[string]Handler {
	return map[string]Handler{
		orchapi.VerbGet: func(_ context.Context, req orchapi.AdapterRequest) (any, error) {
			return orchapi.WorkItem{ID: req.ID, Title: "t", Status: orchapi.WorkOpen}, nil
		},
		"held":           heldWrite,
		"slow":           slow, // a write, as far as the pool knows: never sent twice
		orchapi.VerbList: slow, // a read: sent again after a crash
		"pid":            func(context.Context, orchapi.AdapterRequest) (any, error) { return os.Getpid(), nil },
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("ZM_TEST_SERVE_ADAPTER") == "1" {
		os.Exit(Main(testHandlers()))
	}
	os.Exit(m.Run())
}

// selfAdapter is the test binary as a serve-mode adapter.
func selfAdapter(t *testing.T) string {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), ExecPrefix+"self")
	if err := os.Symlink(self, p); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZM_TEST_SERVE_ADAPTER", "1")
	return p
}

// ServeLoop answers concurrently and matches answers to requests by rid;
// a bad line answers an error and serving goes on; a cancel line stops its
// request; EOF ends the loop after what is in flight.
func TestServeLoop(t *testing.T) {
	inR, inW := io.Pipe()
	var out syncBuf
	done := make(chan int)
	go func() { done <- ServeLoop(context.Background(), inR, &out, testHandlers()) }()
	send := func(v any) {
		b, _ := json.Marshal(v)
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	send(orchapi.AdapterRequest{RID: "a", Verb: "slow", Args: json.RawMessage(`{"ms": 400}`)})
	send(orchapi.AdapterRequest{RID: "b", Verb: orchapi.VerbGet, ID: "x:1"})
	inW.Write([]byte("not json\n"))
	send(orchapi.AdapterRequest{RID: "c", Verb: "slow", Args: json.RawMessage(`{"ms": 60000}`)})
	send(orchapi.AdapterRequest{Cancel: "c"})
	send(orchapi.AdapterRequest{RID: "d", Verb: "nope"})
	inW.Close()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeLoop did not end after EOF")
	}
	var order []string
	byRID := map[string]orchapi.AdapterResponse{}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var r orchapi.AdapterResponse
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("bad line %q", l)
		}
		order = append(order, r.RID)
		byRID[r.RID] = r
	}
	if len(order) != 5 || order[len(order)-1] != "a" {
		t.Fatalf("answers %v (the slow one must come last)", order)
	}
	if !byRID["b"].OK || byRID[""].OK || byRID["c"].OK || byRID["d"].Error.Code != api.CodeNotImplemented || !byRID["a"].OK {
		t.Fatalf("answers %+v", byRID)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// One process serves many calls at once; a crash is noticed, the read in
// flight is sent again to a fresh process, and the pool restarts it.
func TestPoolCrashRestart(t *testing.T) {
	old := RestartBackoff
	RestartBackoff = 0
	defer func() { RestartBackoff = old }()
	path := selfAdapter(t)
	pl := NewPool()
	defer pl.Close()
	ctx := context.Background()
	var pid1 int
	if err := pl.Call(ctx, path, orchapi.AdapterRequest{Verb: "pid"}, &pid1); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var it orchapi.WorkItem
			id := "x:" + string(rune('a'+i))
			if err := pl.Call(ctx, path, orchapi.AdapterRequest{Verb: orchapi.VerbGet, ID: id}, &it); err != nil || it.ID != id {
				t.Errorf("concurrent get %s: %+v %v", id, it, err)
			}
		}()
	}
	wg.Wait()
	var pid int
	if err := pl.Call(ctx, path, orchapi.AdapterRequest{Verb: "pid"}, &pid); err != nil || pid != pid1 {
		t.Fatalf("one process serves every call: %d vs %d (%v)", pid, pid1, err)
	}
	// Crash it while a read is in flight: the read is answered by a new one.
	errc := make(chan error, 1)
	go func() {
		errc <- pl.Call(ctx, path, orchapi.AdapterRequest{Verb: orchapi.VerbList, Args: json.RawMessage(`{"ms": 300}`)}, nil)
	}()
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(pid1, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("read across a crash: %v", err)
	}
	var pid2 int
	if err := pl.Call(ctx, path, orchapi.AdapterRequest{Verb: "pid"}, &pid2); err != nil || pid2 == pid1 || pid2 == 0 {
		t.Fatalf("restarted: %d (was %d) %v", pid2, pid1, err)
	}
	// A write is never sent twice: one cut off by a crash fails.
	go func() {
		errc <- pl.Call(ctx, path, orchapi.AdapterRequest{Verb: "slow", Args: json.RawMessage(`{"ms": 2000}`)}, nil)
	}()
	time.Sleep(100 * time.Millisecond)
	syscall.Kill(pid2, syscall.SIGKILL)
	if err := <-errc; err == nil {
		t.Fatal("a non-read verb was retried after a crash")
	}
	pl.Close()
	if !gone(pid2) {
		t.Fatal("adapter left running after Close")
	}
}

// A serve-mode adapter that stops answering is killed with its whole
// process group (the tracker CLI it started) once a cancelled request is
// not answered within CancelGrace; the pool then starts a new one.
func TestProcHungIsKilled(t *testing.T) {
	oldT, oldG, oldB := Timeout, CancelGrace, RestartBackoff
	Timeout, CancelGrace, RestartBackoff = 300*time.Millisecond, 300*time.Millisecond, 0
	defer func() { Timeout, CancelGrace, RestartBackoff = oldT, oldG, oldB }()
	dir := t.TempDir()
	child := filepath.Join(dir, "child")
	p := script(t, "zm-work-hung", `sleep 60 >/dev/null 2>&1 & echo $! >`+child+`.$$.pid; cat >/dev/null; wait`)
	pl := NewPool()
	defer pl.Close()
	err := pl.Call(context.Background(), p, orchapi.AdapterRequest{Verb: orchapi.VerbGet}, nil)
	if !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("hung call: %v", err)
	}
	// A safe read can retry if the first process finishes before Call checks
	// Alive. Once Call returns, its final process cannot be replaced here.
	// Read the stored process even if done already closed (Proc filters it).
	pl.mu.Lock()
	pr := pl.procs[poolKey(p, nil)]
	pl.mu.Unlock()
	if pr == nil {
		t.Fatal("hung call did not leave a process to inspect")
	}
	// Each adapter writes its own child's PID: a retry must not accidentally
	// check the first process's child while waiting for the final process.
	pid := readPid(t, child+"."+strconv.Itoa(pr.Pid())+".pid")
	if !gone(pid) {
		t.Fatalf("the hung adapter's child %d survived", pid)
	}
	// A child's exit can precede Wait and stderr finalization. Pool.Proc
	// follows this exact process's completion, not the child's lifetime.
	select {
	case <-pr.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the hung adapter did not finish after its child died")
	}
	if pr := pl.Proc(p); pr != nil {
		t.Fatalf("hung adapter still in the pool (pid %d)", pr.Pid())
	}
}

// Keep finalization pending after group death to make the ordering explicit:
// the pool stops exposing a process only once its completion signal closes.
func TestProcPoolWaitsForFinalization(t *testing.T) {
	dir := t.TempDir()
	child := filepath.Join(dir, "child.pid")
	path := script(t, "zm-work-finalize", `sleep 60 >/dev/null 2>&1 & echo $! >`+child+`; cat >/dev/null; wait`)
	pl := NewPool()
	defer pl.Close()
	pr, err := pl.get(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	pid := readPid(t, child)
	// fail reads stderr after Wait. No synchronous Call runs under this
	// mutex, so an unexpected early process exit cannot strand the test.
	pr.stderr.mu.Lock()
	var unlock sync.Once
	release := func() { unlock.Do(pr.stderr.mu.Unlock) }
	defer release() // registered after Close: the gate opens before pool cleanup
	killed := make(chan struct{})
	go func() {
		pr.Kill("test finalization ordering")
		close(killed)
	}()
	defer func() {
		release()
		select {
		case <-killed:
		case <-time.After(10 * time.Second):
			t.Error("adapter kill did not finish after finalization was released")
		}
	}()
	if !gone(pid) {
		t.Fatalf("the adapter's child %d survived group kill", pid)
	}
	select {
	case <-pr.done:
		t.Fatal("process completion preceded gated finalization")
	default:
	}
	if got := pl.Proc(path); got != pr {
		t.Fatal("pool stopped exposing the process before its completion")
	}
	release()
	select {
	case <-pr.done:
	case <-time.After(10 * time.Second):
		t.Fatal("adapter did not complete after finalization was released")
	}
	if got := pl.Proc(path); got != nil {
		t.Fatalf("completed adapter still in the pool (pid %d)", got.Pid())
	}
}

// A request the daemon gave up on leaves nothing behind in the process's
// table: its slot goes when the grace ends (or the process does).
func TestProcTimedOutRequestsAreForgotten(t *testing.T) {
	oldT, oldG := Timeout, CancelGrace
	Timeout, CancelGrace = 100*time.Millisecond, 200*time.Millisecond
	defer func() { Timeout, CancelGrace = oldT, oldG }()
	pr, err := StartProc(script(t, "zm-work-mute", `cat >/dev/null`))
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Kill("test over")
	for range 3 {
		if err := pr.Call(context.Background(), orchapi.AdapterRequest{Verb: orchapi.VerbGet}, nil); !api.IsCode(err, api.CodeUnavailable) {
			t.Fatalf("mute call: %v", err)
		}
	}
	select {
	case <-pr.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the mute adapter was not killed after the grace")
	}
	if n := pr.inflight(); n != 0 {
		t.Fatalf("%d requests still pending after they timed out", n)
	}
}

// A process that exits at once fails calls with its stderr, and is not
// restarted faster than RestartBackoff.
func TestProcCrashLoop(t *testing.T) {
	old := RestartBackoff
	RestartBackoff = time.Hour
	defer func() { RestartBackoff = old }()
	p := script(t, "zm-work-crash", `echo "boom: no config" >&2; exit 3`)
	pl := NewPool()
	defer pl.Close()
	err := pl.Call(context.Background(), p, orchapi.AdapterRequest{Verb: orchapi.VerbGet}, nil)
	if !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("crash: %v", err)
	}
	var e *api.Error
	err2 := pl.Call(context.Background(), p, orchapi.AdapterRequest{Verb: orchapi.VerbGet}, nil)
	if !errors.As(err2, &e) || !strings.Contains(err2.Error(), "boom") {
		t.Fatalf("within the backoff the last failure answers: %v", err2)
	}
}

// An adapter replaced on disk gets a new process for new calls, while the
// old one finishes what it has in flight: a write it was running is
// answered, not reported as cut off. Close stops both.
func TestPoolReplacedOnDisk(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), ExecPrefix+"copy") // a copy: its mtime is ours to change
	if err := os.WriteFile(path, b, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZM_TEST_SERVE_ADAPTER", "1")
	pl := NewPool()
	defer pl.Close()
	ctx := context.Background()
	var pid1 int
	if err := pl.Call(ctx, path, orchapi.AdapterRequest{Verb: "pid"}, &pid1); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() {
		errc <- pl.Call(ctx, path, orchapi.AdapterRequest{Verb: "slow", Args: json.RawMessage(`{"ms": 1500}`)}, nil)
	}()
	time.Sleep(200 * time.Millisecond)
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	var pid2 int
	if err := pl.Call(ctx, path, orchapi.AdapterRequest{Verb: "pid"}, &pid2); err != nil || pid2 == pid1 {
		t.Fatalf("new binary, same process: %d vs %d (%v)", pid2, pid1, err)
	}
	if err := <-errc; err != nil {
		t.Fatalf("a write in flight across a replace: %v", err)
	}
	pl.Close()
	if !gone(pid1) || !gone(pid2) {
		t.Fatal("adapter left running after Close")
	}
}

func TestRetireEnvCapturesOldProcessBeforeReplacement(t *testing.T) {
	path := selfAdapter(t)
	pl := NewPool()
	defer pl.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	barrier, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer barrier.Close()
	var oldPID, newPID int
	if err := pl.Call(ctx, path, orchapi.AdapterRequest{Verb: "pid"}, &oldPID); err != nil {
		t.Fatal(err)
	}
	old := pl.Proc(path)
	args, err := json.Marshal(struct{ Barrier string }{barrier.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan struct{})
	var writeErr error
	go func() {
		defer close(finished)
		writeErr = pl.Call(ctx, path, orchapi.AdapterRequest{Verb: "held", Args: args}, nil)
	}()
	var entered net.Conn
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			if entered != nil {
				_, _ = entered.Write([]byte{1})
				entered.Close()
			}
		})
	}
	defer func() {
		release()
		cancel()
		<-finished // join the caller before the pool closes, including on failure
	}()
	if err := barrier.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	entered, err = barrier.Accept()
	if err != nil {
		t.Fatalf("old handler did not enter: %v", err)
	}
	if err := entered.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var receipt [1]byte
	if _, err := io.ReadFull(entered, receipt[:]); err != nil || receipt[0] != 1 {
		t.Fatalf("old handler entry receipt: %v, %v", receipt, err)
	}
	old.mu.Lock()
	pending := len(old.pending)
	old.mu.Unlock()
	if pending != 1 {
		t.Fatalf("old write is not pending after handler entry: %d", pending)
	}
	pl.RetireEnv(path, nil)
	if pl.Proc(path) != nil {
		t.Fatal("retirement returned before detaching the old process")
	}
	if err := pl.Call(ctx, path, orchapi.AdapterRequest{Verb: "pid"}, &newPID); err != nil || newPID == oldPID {
		t.Fatalf("replacement: old=%d new=%d err=%v", oldPID, newPID, err)
	}
	if current := pl.Proc(path); current == nil || current == old || !current.Alive() {
		t.Fatal("replacement is not alive while the old handler is held")
	}
	select {
	case <-finished:
		t.Fatal("old write finished before its handler was released")
	default:
	}
	release()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("old write did not finish after its handler was released")
	}
	if writeErr != nil {
		t.Fatalf("old write was interrupted: %v", writeErr)
	}
	select {
	case <-old.done:
	case <-time.After(3 * time.Second):
		t.Fatal("old process did not finish retirement")
	}
	if current := pl.Proc(path); current == nil || current == old || !current.Alive() {
		t.Fatal("old retirement stopped the replacement")
	}
}
