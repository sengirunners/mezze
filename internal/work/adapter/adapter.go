// Package adapter implements both sides of the work adapter contract
// (SPEC.md "Work adapters"): Call runs a zm-work-<name> executable from the
// daemon, Serve is the main loop of an adapter executable. Owned by the work builder.
package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// ExecPrefix names adapter executables on PATH: zm-work-<name>.
const ExecPrefix = "zm-work-"

// Timeout bounds one call (SPEC: no answer in 30 s is unavailable).
var Timeout = 30 * time.Second

// ServeTimeout bounds one request inside an adapter (Serve): shorter than
// Timeout, so an adapter kills its own tracker CLI (and answers) before the
// daemon gives up on it.
var ServeTimeout = 25 * time.Second

// SetupTimeout bounds one setup call (SPEC 11: setup answers in 120 s; bd
// init makes a Dolt database).
var SetupTimeout = 2 * time.Minute

// TimeoutFor is how long the daemon waits for verb.
func TimeoutFor(verb string) time.Duration {
	if verb == orchapi.VerbSetup {
		return SetupTimeout
	}
	return Timeout
}

// serveTimeoutFor bounds verb inside an adapter: under TimeoutFor(verb) by
// the same margin as ServeTimeout is under Timeout.
func serveTimeoutFor(verb string) time.Duration {
	if verb == orchapi.VerbSetup {
		return SetupTimeout - (Timeout - ServeTimeout)
	}
	return ServeTimeout
}

// MaxOutput bounds an adapter's stdout for one call.
const MaxOutput = 16 << 20

// Exec runs the adapter at path with raw stdin and returns its decoded
// response. A crash, non-zero exit, timeout or invalid JSON is an
// "unavailable" error naming only the executable's base name; an ok:false
// response is returned as a response, not an error. Conformance checks use
// it to send malformed requests.
func Exec(ctx context.Context, path string, stdin []byte) (orchapi.AdapterResponse, error) {
	return ExecEnv(ctx, path, nil, stdin)
}

// ExecEnv is Exec with the adapter's environment: env (KEY=value) is all
// it gets; nil inherits the caller's (the conformance command). The daemon
// passes each tracker's allowlisted environment (SPEC 17.2).
func ExecEnv(ctx context.Context, path string, env []string, stdin []byte) (orchapi.AdapterResponse, error) {
	return execEnv(ctx, path, env, stdin, Timeout)
}

func execEnv(ctx context.Context, path string, env []string, stdin []byte, timeout time.Duration) (orchapi.AdapterResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path)
	cmd.Env = env
	cmd.Stdin = bytes.NewReader(stdin)
	// The adapter leads its own process group; a timeout kills the group,
	// so a tracker CLI it is running is not orphaned.
	OwnGroup(cmd)
	// A child that keeps stdout open (a shell script's subprocess) must not hang us.
	cmd.WaitDelay = 2 * time.Second
	stdout := &limitBuffer{max: MaxOutput}
	stderr := &limitBuffer{max: 64 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	name := filepath.Base(path)
	err := cmd.Run()
	if errors.Is(err, exec.ErrWaitDelay) || ctx.Err() != nil {
		// Something in the group outlived the adapter (holding its pipes) or
		// the call was cut short: kill what is left, never leave it running.
		_ = KillGroup(cmd)
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return orchapi.AdapterResponse{}, api.Errorf(api.CodeUnavailable, "adapter %s: no answer within %s", name, timeout)
		}
		msg := errTail(stderr.String(), TailMax, stderr.over, secretsOf(ctx))
		if msg != "" {
			msg = ": " + msg
		}
		return orchapi.AdapterResponse{}, api.Errorf(api.CodeUnavailable, "adapter %s: %v%s", name, err, msg)
	}
	if stdout.over {
		return orchapi.AdapterResponse{}, api.Errorf(api.CodeUnavailable, "adapter %s: response larger than %d bytes", name, MaxOutput)
	}
	var resp orchapi.AdapterResponse
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &resp); err != nil {
		return orchapi.AdapterResponse{}, api.Errorf(api.CodeUnavailable, "adapter %s: invalid JSON response: %v", name, err)
	}
	return resp, nil
}

// CallEnv runs the adapter at path with one request and the adapter's
// environment (see ExecEnv), and decodes its result into out (nil = ignore).
// An ok:false response is returned as its *api.Error; a crash, non-zero
// exit, timeout or invalid JSON is an "unavailable" error.
func CallEnv(ctx context.Context, path string, env []string, req orchapi.AdapterRequest, out any) error {
	if req.Spec == "" {
		req.Spec = api.AdapterProtocol
	}
	in, err := json.Marshal(req)
	if err != nil {
		return err
	}
	resp, err := execEnv(ctx, path, env, in, TimeoutFor(req.Verb))
	if err != nil {
		return err
	}
	return Decode(filepath.Base(path), req.Verb, resp, out)
}

// Decode turns a response into out or its error. Error codes outside the
// spec's set become "internal" so a buggy adapter cannot invent statuses.
func Decode(name, verb string, resp orchapi.AdapterResponse, out any) error {
	if !resp.OK {
		if resp.Error == nil {
			return api.Errorf(api.CodeUnavailable, "adapter %s %s failed without an error", name, verb)
		}
		e := *resp.Error
		e.Data = nil // adapter data is never relayed (it could name anything)
		if !knownCode(e.Code) {
			e.Message = e.Code + ": " + e.Message
			e.Code = api.CodeInternal
		}
		return &e
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return api.Errorf(api.CodeUnavailable, "adapter %s %s: bad result: %v", name, verb, err)
		}
	}
	return nil
}

func knownCode(c string) bool {
	switch c {
	case api.CodeBadRequest, api.CodeNotFound, api.CodeConflict, api.CodeForbidden,
		api.CodeNotImplemented, api.CodeUnavailable, api.CodeInternal:
		return true
	}
	return false
}

// Handler answers one verb. Return any JSON-marshalable result, or an
// *api.Error (e.g. api.Errorf(api.CodeConflict, ...)) to refuse.
type Handler func(ctx context.Context, req orchapi.AdapterRequest) (any, error)

// Serve reads one request from stdin, dispatches it to handlers[verb] and
// writes one response to stdout. A request speaking another protocol major
// is refused; unknown verbs answer not_implemented. It returns the process
// exit code (0 even for ok:false; non-zero only when the response cannot be
// written).
func Serve(ctx context.Context, stdin io.Reader, stdout io.Writer, handlers map[string]Handler) int {
	var req orchapi.AdapterRequest
	var resp orchapi.AdapterResponse
	derr := json.NewDecoder(io.LimitReader(stdin, api.MaxBody)).Decode(&req)
	ctx, cancel := context.WithTimeout(ctx, serveTimeoutFor(req.Verb))
	defer cancel()
	if derr != nil {
		resp.Error = api.Errorf(api.CodeBadRequest, "invalid request: %v", derr)
	} else if req.Spec != "" && !SameMajor(req.Spec) {
		resp.Error = api.Errorf(api.CodeBadRequest, "protocol %q is not %s", req.Spec, api.AdapterProtocol)
	} else if h, ok := handlers[req.Verb]; !ok {
		resp.Error = api.Errorf(api.CodeNotImplemented, "verb %q not supported", req.Verb)
	} else if res, err := h(ctx, req); err != nil {
		var e *api.Error
		if !errors.As(err, &e) {
			e = api.Errorf(api.CodeInternal, "%v", err)
		}
		resp.Error = e
	} else {
		resp.OK = true
		if resp.Result, err = json.Marshal(res); err != nil {
			resp.OK, resp.Result, resp.Error = false, nil, api.Errorf(api.CodeInternal, "marshal result: %v", err)
		}
	}
	if err := json.NewEncoder(stdout).Encode(resp); err != nil {
		return 1
	}
	return 0
}

// SameMajor reports whether p ("zm/v0", "zm/v0.1") has api.AdapterProtocol's major version.
func SameMajor(p string) bool {
	return p != "" && api.ProtocolMajor(p) == api.ProtocolMajor(api.AdapterProtocol)
}

// Verbs lists the verbs a handler map answers, sorted, for describe.
func Verbs(handlers map[string]Handler) []string {
	vs := make([]string, 0, len(handlers))
	for v := range handlers {
		vs = append(vs, v)
	}
	sort.Strings(vs)
	return vs
}

// Find locates the executable for adapter name ("acme" -> zm-work-acme):
// first an entry of extra (config.json work_adapters) with that base name,
// then PATH. A name containing a path separator is used as is.
func Find(name string, extra []string) (string, error) {
	if strings.ContainsRune(name, os.PathSeparator) {
		if !isExecutable(name) {
			return "", fmt.Errorf("%s is not an executable file", name)
		}
		return name, nil
	}
	exe := name
	if !strings.HasPrefix(exe, ExecPrefix) {
		exe = ExecPrefix + exe
	}
	for _, p := range extra {
		if p = strings.TrimSpace(p); p != "" && filepath.Base(p) == exe && isExecutable(p) {
			return p, nil
		}
	}
	p, err := exec.LookPath(exe)
	if err != nil {
		return "", fmt.Errorf("no %s on PATH", exe)
	}
	return p, nil
}

func isExecutable(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0
}

// Args decodes a request's args into T (zero T when absent).
func Args[T any](req orchapi.AdapterRequest) (T, error) {
	var v T
	if len(req.Args) > 0 && string(req.Args) != "null" {
		if err := json.Unmarshal(req.Args, &v); err != nil {
			return v, api.Errorf(api.CodeBadRequest, "bad args: %v", err)
		}
	}
	return v, nil
}

// limitBuffer is a bytes.Buffer that drops writes past max (and remembers it did).
type limitBuffer struct {
	bytes.Buffer
	max  int
	over bool
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.Len(); len(p) > room {
		b.over = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

// ReadFrom goes through Write: io.Copy (os/exec's copier) would otherwise
// use bytes.Buffer's and skip the limit.
func (b *limitBuffer) ReadFrom(r io.Reader) (int64, error) {
	return io.Copy(struct{ io.Writer }{b}, r)
}
