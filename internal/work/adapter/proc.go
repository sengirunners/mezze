package adapter

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// CancelGrace is how long a serve-mode adapter has to answer a request the
// daemon gave up on (after the cancel line) before the daemon decides it is
// hung and kills its process group.
var CancelGrace = 2 * time.Second

// RestartBackoff is the least time between two starts of one adapter, so a
// crashing adapter cannot spin.
var RestartBackoff = time.Second

// Proc is one adapter running in serve mode (orchapi.ServeFlag): one process,
// requests and responses as JSON lines matched by rid, many in flight.
type Proc struct {
	path    string
	name    string
	mod     time.Time
	started time.Time
	cmd     *exec.Cmd
	in      *os.File
	stderr  *syncWriter
	secrets []string // masked in its stderr tail (WithSecrets)

	wmu sync.Mutex

	mu      sync.Mutex
	seq     uint64
	pending map[string]chan orchapi.AdapterResponse
	done    chan struct{} // closed once the process is gone
	err     error         // why it is gone
	killed  string        // why zm killed it, if it did
}

// StartProcEnv starts the adapter at path in serve mode with the adapter's
// environment (nil inherits; see ExecEnv). The adapter leads its own process
// group, so killing it also kills the tracker CLI it runs.
func StartProcEnv(path string, env []string) (*Proc, error) { return startProc(path, env, nil) }

// startProc is StartProcEnv with the secret values its stopped error masks.
func startProc(path string, env, secrets []string) (*Proc, error) {
	p := &Proc{path: path, name: filepath.Base(path), started: time.Now(), stderr: &syncWriter{w: &limitBuffer{max: 64 << 10}},
		secrets: secrets, pending: map[string]chan orchapi.AdapterResponse{}, done: make(chan struct{})}
	if st, err := os.Stat(path); err == nil {
		p.mod = st.ModTime()
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		inR.Close()
		inW.Close()
		return nil, err
	}
	p.cmd = exec.Command(path, orchapi.ServeFlag)
	p.cmd.Env = env
	p.cmd.Stdin, p.cmd.Stdout, p.cmd.Stderr = inR, outW, p.stderr
	OwnGroup(p.cmd)
	p.cmd.Cancel = nil // no context: the pool kills the group itself
	if err := p.cmd.Start(); err != nil {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
		return nil, api.Errorf(api.CodeUnavailable, "adapter %s: %v", p.name, err)
	}
	inR.Close()
	outW.Close()
	p.in = inW
	go p.read(outR)
	return p, nil
}

// read dispatches responses until stdout closes, then reaps the process.
func (p *Proc) read(out *os.File) {
	r := bufio.NewReaderSize(out, 256<<10)
	var why error
	for {
		line, err := readLine(r, MaxLine)
		if errors.Is(err, errLineTooLong) {
			why = fmt.Errorf("a response line over %d bytes", MaxLine)
			break
		}
		if len(line) > 0 {
			var resp orchapi.AdapterResponse
			if jerr := json.Unmarshal(line, &resp); jerr != nil {
				why = fmt.Errorf("invalid JSON response: %v", jerr)
				break
			}
			p.mu.Lock()
			ch := p.pending[resp.RID]
			delete(p.pending, resp.RID)
			p.mu.Unlock()
			if ch != nil {
				ch <- resp
			}
		}
		if err != nil {
			break
		}
	}
	_ = KillGroup(p.cmd)
	out.Close()
	werr := p.cmd.Wait() // also waits for stderr to be copied, so fail can quote it
	p.mu.Lock()
	killed := p.killed
	p.mu.Unlock()
	switch {
	case killed != "":
		why = errors.New(killed)
	case why == nil && werr != nil:
		why = werr
	case why == nil:
		why = errors.New("exited")
	}
	p.fail(why)
}

func (p *Proc) fail(why error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.done:
		return
	default:
	}
	msg := errTail(p.stderr.String(), TailMax, p.stderr.over(), p.secrets)
	if msg != "" {
		why = fmt.Errorf("%v: %s", why, msg)
	}
	p.err = api.Errorf(api.CodeUnavailable, "adapter %s (serve mode) stopped: %v", p.name, why)
	close(p.done)
	p.in.Close()
	clear(p.pending) // callers see done; no answer can come
}

// forget drops a request's slot (it timed out or was never sent).
func (p *Proc) forget(rid string) {
	p.mu.Lock()
	delete(p.pending, rid)
	p.mu.Unlock()
}

// Alive reports whether the process is still running.
func (p *Proc) Alive() bool {
	select {
	case <-p.done:
		return false
	default:
		return true
	}
}

// Pid is the adapter's process id.
func (p *Proc) Pid() int { return p.cmd.Process.Pid }

// Kill kills the adapter's process group now and waits until it is gone.
func (p *Proc) Kill(why string) {
	p.mu.Lock()
	if p.killed == "" {
		p.killed = why
	}
	p.mu.Unlock()
	_ = KillGroup(p.cmd)
	<-p.done
}

// Close asks the adapter to stop (stdin closes), and kills its group if it
// has not within a second.
func (p *Proc) Close() { p.stop(time.Second, "closed") }

// retire is Close for a process replaced on disk: what it has in flight
// is answered first (a serve-mode adapter ends after its requests on EOF),
// so a write it was running is not reported as cut off.
func (p *Proc) retire() { p.stop(Timeout+CancelGrace, "replaced on disk") }

func (p *Proc) stop(grace time.Duration, why string) {
	p.wmu.Lock()
	p.in.Close()
	p.wmu.Unlock()
	select {
	case <-p.done:
	case <-time.After(grace):
		p.Kill(why)
	}
}

func (p *Proc) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	p.wmu.Lock()
	defer p.wmu.Unlock()
	// A hung adapter that stops reading must not block the daemon.
	_ = p.in.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := p.in.Write(b); err != nil {
		go p.Kill("cannot write to it: " + err.Error()) // not under wmu: Close takes it
		<-p.done
		return p.err
	}
	return nil
}

// Call sends one request and decodes the answer into out, like Call does
// for a one-shot adapter. The request is bounded by ctx (Timeout when ctx
// has no deadline). On timeout a cancel line goes to the adapter; if it
// has not answered CancelGrace later it is killed as hung.
func (p *Proc) Call(ctx context.Context, req orchapi.AdapterRequest, out any) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, TimeoutFor(req.Verb))
		defer cancel()
	}
	if req.Spec == "" {
		req.Spec = api.AdapterProtocol
	}
	ch := make(chan orchapi.AdapterResponse, 1)
	p.mu.Lock()
	select {
	case <-p.done:
		p.mu.Unlock()
		return p.err
	default:
	}
	p.seq++
	req.RID = strconv.FormatUint(p.seq, 36)
	p.pending[req.RID] = ch
	p.mu.Unlock()
	if err := p.send(req); err != nil {
		p.forget(req.RID)
		return err
	}
	select {
	case resp := <-ch:
		return Decode(p.name, req.Verb, resp, out)
	case <-p.done:
		// read delivers complete replies before closing done. Preserve an
		// acknowledged write even when its adapter exits before we resume.
		select {
		case resp := <-ch:
			return Decode(p.name, req.Verb, resp, out)
		default:
			return p.err
		}
	case <-ctx.Done():
	}
	_ = p.send(orchapi.AdapterRequest{Spec: api.AdapterProtocol, Cancel: req.RID})
	grace := time.After(CancelGrace)
	go func() {
		select {
		case <-ch:
		case <-p.done:
		case <-grace:
			p.forget(req.RID) // a late answer is dropped; nothing accumulates
			p.Kill(fmt.Sprintf("request %s %s did not stop after a cancel", req.Verb, req.RID))
		}
	}()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return api.Errorf(api.CodeUnavailable, "adapter %s: no answer to %s in time", p.name, req.Verb)
	}
	return ctx.Err()
}

// Pool keeps one serve-mode process per adapter executable and
// environment (each tracker's allowlisted environment is its own process),
// started on first use, restarted after a crash (at most once per
// RestartBackoff) or when the executable changes on disk.
type Pool struct {
	mu      sync.Mutex
	procs   map[string]*Proc
	retired map[*Proc]bool // replaced on disk, finishing what is in flight
	closed  bool
}

// NewPool returns an empty pool.
func NewPool() *Pool { return &Pool{procs: map[string]*Proc{}, retired: map[*Proc]bool{}} }

// poolKey names one process: the executable plus its environment (nil,
// inherit, is the bare path, so Proc(path) finds it).
func poolKey(path string, env []string) string {
	if env == nil {
		return path
	}
	h := sha256.New()
	for _, kv := range env {
		h.Write([]byte(kv))
		h.Write([]byte{0})
	}
	return path + "\x00" + hex.EncodeToString(h.Sum(nil)[:12])
}

func (pl *Pool) get(path string, env, secrets []string) (*Proc, error) {
	key := poolKey(path, env)
	var mod time.Time
	if st, err := os.Stat(path); err == nil {
		mod = st.ModTime()
	}
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if pl.closed {
		return nil, api.Errorf(api.CodeUnavailable, "the daemon is stopping")
	}
	p := pl.procs[key]
	if p != nil && p.Alive() && p.mod.Equal(mod) {
		return p, nil
	}
	if p != nil {
		if p.Alive() { // replaced on disk: a fresh process for the new binary
			pl.retired[p] = true
			go func() {
				p.retire()
				pl.mu.Lock()
				delete(pl.retired, p)
				pl.mu.Unlock()
			}()
		} else if time.Since(p.started) < RestartBackoff {
			return nil, p.err
		}
	}
	np, err := startProc(path, env, secrets)
	if err != nil {
		return nil, err
	}
	pl.procs[key] = np
	return np, nil
}

// readVerbs are safe to send again after the process died under them.
var readVerbs = map[string]bool{orchapi.VerbDescribe: true, orchapi.VerbList: true, orchapi.VerbGet: true, orchapi.VerbChanges: true}

// Call sends req to the serve-mode adapter at path. A read that was cut
// off by the process dying is sent once more to a fresh process; a write
// never is (it may have reached the tracker).
func (pl *Pool) Call(ctx context.Context, path string, req orchapi.AdapterRequest, out any) error {
	return pl.CallEnv(ctx, path, nil, req, out)
}

// CallEnv is Call for the process of path running with env (see ExecEnv).
func (pl *Pool) CallEnv(ctx context.Context, path string, env []string, req orchapi.AdapterRequest, out any) error {
	p, err := pl.get(path, env, secretsOf(ctx))
	if err != nil {
		return err
	}
	err = p.Call(ctx, req, out)
	if err != nil && !p.Alive() && readVerbs[req.Verb] && ctx.Err() == nil {
		if p, err2 := pl.get(path, env, secretsOf(ctx)); err2 == nil {
			return p.Call(ctx, req, out)
		}
	}
	return err
}

// Proc returns a live process for path (whatever its environment), if any (tests).
func (pl *Pool) Proc(path string) *Proc {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if p := pl.procs[path]; p != nil && p.Alive() {
		return p
	}
	for k, p := range pl.procs {
		if strings.HasPrefix(k, path+"\x00") && p.Alive() {
			return p
		}
	}
	return nil
}

// CloseEnv stops the process of path running with env, if any (a tracker
// was unbound, or its environment changed): stdin closes, so it finishes
// what it has in flight first.
func (pl *Pool) CloseEnv(path string, env []string) {
	if p := pl.detachEnv(path, env); p != nil {
		pl.finishRetirement(p)
	}
}

// RetireEnv detaches the current process before returning. Its asynchronous
// drain captures that process, never a future replacement at the same key.
func (pl *Pool) RetireEnv(path string, env []string) {
	if p := pl.detachEnv(path, env); p != nil {
		go pl.finishRetirement(p)
	}
}

func (pl *Pool) detachEnv(path string, env []string) *Proc {
	key := poolKey(path, env)
	pl.mu.Lock()
	p := pl.procs[key]
	delete(pl.procs, key)
	if p != nil {
		pl.retired[p] = true
	}
	pl.mu.Unlock()
	return p
}

func (pl *Pool) finishRetirement(p *Proc) {
	p.retire()
	pl.mu.Lock()
	delete(pl.retired, p)
	pl.mu.Unlock()
}

// Close stops every process.
func (pl *Pool) Close() {
	pl.mu.Lock()
	pl.closed = true
	ps := slices.Collect(maps.Values(pl.procs))
	for p := range pl.retired { // the daemon is stopping: no more waiting on them
		ps = append(ps, p)
	}
	pl.procs = map[string]*Proc{}
	pl.mu.Unlock()
	var wg sync.WaitGroup
	for _, p := range ps {
		wg.Add(1)
		go func() { defer wg.Done(); p.Close() }()
	}
	wg.Wait()
}

// syncWriter serialises writes into a limitBuffer (stderr is written by
// os/exec's copier while fail reads it).
type syncWriter struct {
	mu sync.Mutex
	w  *limitBuffer
}

// over reports whether output was dropped.
func (s *syncWriter) over() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.over
}

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(b)
}

func (s *syncWriter) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.String()
}
