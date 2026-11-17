package agentpipe

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// A recording is one real session between an adapter and its runtime, the
// raw material of the conformance replays: JSON lines, a header, then
// entries in the order the runner handled them.
//
//	{"t":"zm-agent-recording","v":1,"driver":"codex","scenario":"approve"}
//	{"t":"op","after":3,"op":{...}}     an operation zm sent; after = events emitted before it
//	{"t":"in","frame":{...}}            a line the adapter wrote to the runtime
//	{"t":"out","frame":{...}}           a line the runtime wrote (raw: a line that is not JSON)
//	{"t":"exit","code":0}               the runtime exited
//
// Recordings committed to the repo are scrubbed of paths, ids and accounts
// (scripts in internal/agentpipe/testdata/README.md).
const (
	RecHeader = "zm-agent-recording"
	RecOp     = "op"
	RecIn     = "in"
	RecOut    = "out"
	RecExit   = "exit"
)

// RecEntry is one recording line.
type RecEntry struct {
	T        string          `json:"t"`
	V        int             `json:"v,omitempty"`
	Driver   string          `json:"driver,omitempty"`
	Scenario string          `json:"scenario,omitempty"`
	After    int64           `json:"after,omitempty"`
	Op       json.RawMessage `json:"op,omitempty"`
	Frame    json.RawMessage `json:"frame,omitempty"`
	Raw      string          `json:"raw,omitempty"`
	Code     int             `json:"code,omitempty"`
	Signaled bool            `json:"signaled,omitempty"`
}

// Recording writes one.
type Recording struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

// CreateRecording starts a recording file (0600).
func CreateRecording(path, driver string) (*Recording, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	r := &Recording{f: f, w: bufio.NewWriter(f)}
	r.put(RecEntry{T: RecHeader, V: 1, Driver: driver})
	return r, nil
}

func (r *Recording) put(e RecEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b, _ := json.Marshal(e)
	_, _ = r.w.Write(append(b, '\n'))
	_ = r.w.Flush()
}

// Write records a line to (RecIn) or from (RecOut) the runtime.
func (r *Recording) Write(t string, line []byte, after int64) {
	e := RecEntry{T: t}
	if json.Valid(line) {
		e.Frame = append(json.RawMessage(nil), line...)
	} else {
		e.Raw = string(line)
	}
	r.put(e)
}

// WriteOp records an operation.
func (r *Recording) WriteOp(op []byte, after int64) {
	r.put(RecEntry{T: RecOp, After: after, Op: append(json.RawMessage(nil), op...)})
}

// WriteExit records the runtime's exit.
func (r *Recording) WriteExit(code int, signaled bool, after int64) {
	r.put(RecEntry{T: RecExit, Code: code, Signaled: signaled})
}

// Close flushes and closes.
func (r *Recording) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_ = r.w.Flush()
	return r.f.Close()
}

// ReadRecording parses one.
func ReadRecording(rd io.Reader) (RecEntry, []RecEntry, error) {
	s := bufio.NewScanner(rd)
	s.Buffer(make([]byte, 64<<10), MaxLine)
	var head RecEntry
	var out []RecEntry
	for s.Scan() {
		b := bytes.TrimSpace(s.Bytes())
		if len(b) == 0 {
			continue
		}
		var e RecEntry
		if err := json.Unmarshal(b, &e); err != nil {
			return head, nil, fmt.Errorf("recording line %d: %v", len(out)+2, err)
		}
		if head.T == "" {
			if e.T != RecHeader {
				return head, nil, errors.New("not a zm-agent recording")
			}
			head = e
			continue
		}
		out = append(out, e)
	}
	return head, out, s.Err()
}

// Canonical is a JSON value re-encoded with sorted keys, for comparing
// frames.
func Canonical(b []byte) string {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return string(b)
	}
	var buf bytes.Buffer
	canon(&buf, v)
	return buf.String()
}

func canon(buf *bytes.Buffer, v any) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			canon(buf, x[k])
		}
		buf.WriteByte('}')
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			canon(buf, e)
		}
		buf.WriteByte(']')
	default:
		b, _ := json.Marshal(x)
		buf.Write(b)
	}
}

// FakeArg is argv[1] of a fake runtime replaying a recording's runtime
// side: `<bin> __zm-agent-fake <recording> [the driver's flags...]`. It writes each out entry,
// waits for each in entry and checks it matches (exit 3 and a diff on
// stderr when not), and exits with the recorded code.
const FakeArg = "__zm-agent-fake"

// FakeMain is the fake runtime.
func FakeMain(args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "fake runtime: usage: "+FakeArg+" <recording> [flags...]")
		return 2
	}
	f, err := os.Open(args[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	_, entries, err := ReadRecording(f)
	_ = f.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	setFakePairs(entries)
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 64<<10), MaxLine)
	out := bufio.NewWriter(os.Stdout)
	stopped := false // a stop came first: the runtime exited because its stdin closed
	for i, e := range entries {
		switch e.T {
		case RecOp:
			var op Op
			if json.Unmarshal(e.Op, &op) == nil && op.Op == OpStop {
				stopped = true
			}
		case RecOut:
			if e.Frame != nil {
				_, _ = out.Write(e.Frame)
			} else {
				_, _ = out.WriteString(e.Raw)
			}
			_ = out.WriteByte('\n')
			_ = out.Flush()
		case RecIn:
			_ = out.Flush()
			if !in.Scan() {
				fmt.Fprintf(os.Stderr, "fake runtime: entry %d: stdin closed, want %s\n", i+1, e.Frame)
				return 3
			}
			got, want := Canonical(fakeSubst(in.Bytes())), Canonical(e.Frame)
			if got != want {
				fmt.Fprintf(os.Stderr, "fake runtime: entry %d: got  %s\nwant %s\n", i+1, got, want)
				return 3
			}
		case RecExit:
			_ = out.Flush()
			if e.Signaled || stopped {
				// Ended by a stop: wait for the adapter's (stdin closing),
				// so the replay takes the stop op before the runtime goes.
				_, _ = io.Copy(io.Discard, os.Stdin)
			}
			if e.Signaled {
				return 0
			}
			return e.Code
		}
	}
	_ = out.Flush()
	_, _ = io.Copy(io.Discard, os.Stdin)
	return 0
}

// FakeSubstEnv tells the fake runtime to read a path the adapter writes as
// the recorded one: "<actual>=<recorded>" (a replay's cwd is a temp dir, the
// recording's a scrubbed placeholder).
const FakeSubstEnv = "ZM_AGENT_FAKE_SUBST"

// fakeSubst rewrites where the replay runs to where the recording ran: the
// pair in FakeSubstEnv, else this process's directory (the adapter starts
// its runtime in the session's cwd) to the recording's start cwd.
func fakeSubst(line []byte) []byte {
	for _, p := range fakePairs {
		a, _ := json.Marshal(p[0])
		r, _ := json.Marshal(p[1])
		line = bytes.ReplaceAll(line, a[1:len(a)-1], r[1:len(r)-1])
	}
	return line
}

var fakePairs [][2]string

func setFakePairs(entries []RecEntry) {
	if actual, recorded, ok := strings.Cut(os.Getenv(FakeSubstEnv), "="); ok && actual != "" {
		fakePairs = append(fakePairs, [2]string{actual, recorded})
		return
	}
	recorded := ""
	for _, e := range entries {
		var op Op
		if e.T == RecOp && json.Unmarshal(e.Op, &op) == nil && op.Op == OpStart && op.Start != nil {
			recorded = op.Start.Cwd
			break
		}
	}
	if recorded == "" {
		return
	}
	seen := map[string]bool{recorded: true}
	wd, _ := os.Getwd()
	real, _ := filepath.EvalSymlinks(wd)
	// Longest first, so /private/var/x is replaced before /var/x.
	cands := []string{real, wd, os.Getenv("PWD")}
	sort.Slice(cands, func(i, j int) bool { return len(cands[i]) > len(cands[j]) })
	for _, c := range cands {
		if c != "" && !seen[c] {
			seen[c] = true
			fakePairs = append(fakePairs, [2]string{c, recorded})
		}
	}
	if strings.HasPrefix(real, "/private/") && !seen[strings.TrimPrefix(real, "/private")] {
		fakePairs = append(fakePairs, [2]string{strings.TrimPrefix(real, "/private"), recorded})
	}
}
