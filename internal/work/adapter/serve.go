package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"sync"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// MaxLine bounds one serve-mode line, either way (a full listing page can
// be large; one-shot responses stay under MaxOutput).
const MaxLine = 64 << 20

// MaxConcurrent bounds how many requests a serve-mode adapter runs at once.
var MaxConcurrent = 8

// Main is an adapter executable's main: with orchapi.ServeFlag it serves JSON
// lines until stdin closes (ServeLoop), otherwise it answers one request
// (Serve). It returns the process exit code.
func Main(handlers map[string]Handler) int {
	if slices.Contains(os.Args[1:], orchapi.ServeFlag) {
		return ServeLoop(context.Background(), os.Stdin, os.Stdout, handlers)
	}
	return Serve(context.Background(), os.Stdin, os.Stdout, handlers)
}

// ServeLoop is serve mode: one request per line on stdin, one response per
// line on stdout carrying the request's rid, answered concurrently (so out
// of order) and each bounded by ServeTimeout. A {"cancel": rid} line stops
// that request. A line that is not a request answers an error without a
// rid and the loop goes on. It returns 0 when stdin closes (after the
// requests in flight answer), 1 when stdout breaks.
func ServeLoop(ctx context.Context, stdin io.Reader, stdout io.Writer, handlers map[string]Handler) int {
	ctx, stopAll := context.WithCancel(ctx)
	defer stopAll()
	var (
		wmu     sync.Mutex
		broken  bool
		enc     = json.NewEncoder(stdout)
		mu      sync.Mutex
		running = map[string]context.CancelFunc{}
		wg      sync.WaitGroup
		sem     = make(chan struct{}, max(1, MaxConcurrent))
	)
	write := func(resp orchapi.AdapterResponse) {
		wmu.Lock()
		defer wmu.Unlock()
		if !broken && enc.Encode(resp) != nil {
			broken = true
			stopAll()
		}
	}
	r := bufio.NewReaderSize(stdin, 64<<10)
	for {
		line, err := readLine(r, MaxLine)
		if len(bytes.TrimSpace(line)) > 0 {
			var req orchapi.AdapterRequest
			if jerr := json.Unmarshal(line, &req); jerr != nil {
				write(orchapi.AdapterResponse{Error: api.Errorf(api.CodeBadRequest, "invalid request: %v", jerr)})
			} else if req.Cancel != "" {
				mu.Lock()
				if c := running[req.Cancel]; c != nil {
					c()
				}
				mu.Unlock()
			} else {
				rctx, cancel := context.WithTimeout(ctx, serveTimeoutFor(req.Verb))
				mu.Lock()
				if req.RID != "" {
					running[req.RID] = cancel
				}
				mu.Unlock()
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer cancel()
					select {
					case sem <- struct{}{}:
						defer func() { <-sem }()
					case <-rctx.Done():
					}
					resp := answer(rctx, req, handlers)
					resp.RID = req.RID
					mu.Lock()
					delete(running, req.RID)
					mu.Unlock()
					write(resp)
				}()
			}
		}
		if errors.Is(err, errLineTooLong) {
			write(orchapi.AdapterResponse{Error: api.Errorf(api.CodeBadRequest, "request line over %d bytes", MaxLine)})
			continue
		}
		if err != nil {
			break
		}
	}
	wg.Wait()
	wmu.Lock()
	defer wmu.Unlock()
	if broken {
		return 1
	}
	return 0
}

// answer runs one well-formed request through handlers.
func answer(ctx context.Context, req orchapi.AdapterRequest, handlers map[string]Handler) orchapi.AdapterResponse {
	var resp orchapi.AdapterResponse
	if req.Spec != "" && !SameMajor(req.Spec) {
		resp.Error = api.Errorf(api.CodeBadRequest, "protocol %q is not %s", req.Spec, api.AdapterProtocol)
	} else if ctx.Err() != nil {
		resp.Error = api.Errorf(api.CodeUnavailable, "cancelled")
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
	return resp
}

var errLineTooLong = errors.New("line too long")

// readLine reads one '\n'-terminated line (without it) of at most max
// bytes. A longer line is consumed and reported as errLineTooLong. At EOF
// it returns what is left with io.EOF.
func readLine(r *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	over := false
	for {
		chunk, err := r.ReadSlice('\n')
		if !over {
			if len(buf)+len(chunk) > max+1 {
				over, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		switch {
		case err == nil:
			if over {
				return nil, errLineTooLong
			}
			return bytes.TrimSuffix(buf, []byte("\n")), nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			if over {
				return nil, errLineTooLong
			}
			return buf, err
		}
	}
}
