package conformance

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"slices"
	"strconv"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/potluck/kit/api"
)

// ServeTimeout bounds one serve-mode answer during a conformance run.
var ServeTimeout = 30 * time.Second

// serveConn is one adapter process in serve mode, driven line by line.
type serveConn struct {
	cmd   *exec.Cmd
	in    io.WriteCloser
	lines chan []byte
	exit  chan error
	seq   int
}

func startServe(path string) (*serveConn, error) {
	cmd := exec.Command(path, orchapi.ServeFlag)
	adapter.OwnGroup(cmd)
	cmd.Cancel = nil
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &serveConn{cmd: cmd, in: in, lines: make(chan []byte, 64), exit: make(chan error, 1)}
	go func() {
		r := bufio.NewReaderSize(out, 1<<20)
		for {
			b, err := r.ReadBytes('\n')
			if len(b) > 0 {
				c.lines <- b
			}
			if err != nil {
				close(c.lines)
				c.exit <- cmd.Wait()
				return
			}
		}
	}()
	return c, nil
}

func (c *serveConn) kill() { _ = adapter.KillGroup(c.cmd) }

func (c *serveConn) write(b []byte) error {
	_, err := c.in.Write(append(b, '\n'))
	return err
}

func (c *serveConn) read(ctx context.Context) (orchapi.AdapterResponse, error) {
	var resp orchapi.AdapterResponse
	t := time.NewTimer(ServeTimeout)
	defer t.Stop()
	select {
	case b, ok := <-c.lines:
		if !ok {
			return resp, errors.New("the adapter exited")
		}
		if err := json.Unmarshal(b, &resp); err != nil {
			return resp, fmt.Errorf("invalid JSON line: %v", err)
		}
		return resp, nil
	case <-t.C:
		return resp, fmt.Errorf("no answer within %s", ServeTimeout)
	case <-ctx.Done():
		return resp, ctx.Err()
	}
}

// roundTrip sends one request (a rid is added to a JSON object; anything
// else goes as is) and reads one answer, which must carry the same rid.
func (c *serveConn) roundTrip(ctx context.Context, in []byte) (orchapi.AdapterResponse, error) {
	var obj map[string]any
	rid := ""
	if json.Unmarshal(in, &obj) == nil && obj != nil {
		c.seq++
		rid = "c" + strconv.Itoa(c.seq)
		obj["rid"] = rid
		in, _ = json.Marshal(obj)
	}
	if err := c.write(in); err != nil {
		return orchapi.AdapterResponse{}, err
	}
	resp, err := c.read(ctx)
	if err == nil && resp.RID != rid {
		err = fmt.Errorf("answer carries rid %q, want %q", resp.RID, rid)
	}
	return resp, err
}

// RunServe checks the adapter at path in serve mode, when its describe
// says it has one: the whole Run suite over one long-lived process, then
// what serve mode adds (pipelined requests matched by rid, a bad line
// does not end the process, cancel lines, exit when stdin closes).
func RunServe(ctx context.Context, path string, opt Options) []Result {
	var d orchapi.AdapterDescribe
	r := &run{ctx: ctx, rt: Exec(path), opt: opt}
	if err := must(r.call(orchapi.VerbDescribe, "", nil, &d)); err != nil || !d.Serve {
		return []Result{{Name: "serve mode", OK: true, Skipped: true, Detail: "not offered (describe has no \"serve\": true)"}}
	}
	c, err := startServe(path)
	if err != nil {
		return []Result{{Name: "serve mode starts", Detail: err.Error()}}
	}
	defer c.kill()
	var out []Result
	for _, res := range Run(ctx, c.roundTrip, opt) {
		res.Name = "serve: " + res.Name
		out = append(out, res)
	}
	sr := &run{ctx: ctx, opt: opt}
	sr.add("serve: pipelined requests are answered by rid", c.pipelined(ctx, d.Prefix))
	sr.add("serve: a bad line answers an error and serving goes on", c.badLine(ctx))
	sr.add("serve: a cancel line answers nothing and serving goes on", c.cancelLine(ctx))
	sr.add("serve: exits 0 when stdin closes", c.closes())
	return append(out, sr.out...)
}

func req(rid, verb, id string) []byte {
	b, _ := json.Marshal(orchapi.AdapterRequest{Spec: api.AdapterProtocol, RID: rid, Verb: verb, ID: id})
	return b
}

func (c *serveConn) pipelined(ctx context.Context, prefix string) error {
	want := map[string]bool{"p1": true, "p2": true, "p3": true}
	for _, b := range [][]byte{req("p1", orchapi.VerbDescribe, ""), req("p2", "no-such-verb", ""),
		req("p3", orchapi.VerbGet, prefix+":zm-conformance-missing-0")} {
		if err := c.write(b); err != nil {
			return err
		}
	}
	for range 3 {
		resp, err := c.read(ctx)
		if err != nil {
			return err
		}
		if !want[resp.RID] {
			return fmt.Errorf("unexpected or repeated rid %q", resp.RID)
		}
		delete(want, resp.RID)
		if resp.RID == "p2" && (resp.OK || resp.Error == nil || resp.Error.Code != api.CodeNotImplemented) {
			return fmt.Errorf("unknown verb answered %+v", resp)
		}
	}
	return nil
}

func (c *serveConn) badLine(ctx context.Context) error {
	if err := c.write([]byte("this is not json")); err != nil {
		return err
	}
	resp, err := c.read(ctx)
	if err != nil {
		return err
	}
	if resp.OK || resp.Error == nil {
		return fmt.Errorf("a bad line answered ok")
	}
	return c.alive(ctx)
}

func (c *serveConn) cancelLine(ctx context.Context) error {
	b, _ := json.Marshal(orchapi.AdapterRequest{Spec: api.AdapterProtocol, Cancel: "no-such-rid"})
	if err := c.write(b); err != nil {
		return err
	}
	return c.alive(ctx) // its answer must be the describe's, not one for the cancel
}

func (c *serveConn) alive(ctx context.Context) error {
	resp, err := c.roundTrip(ctx, req("", orchapi.VerbDescribe, ""))
	if err != nil {
		return fmt.Errorf("not serving any more: %v", err)
	}
	if !resp.OK {
		return fmt.Errorf("describe refused afterwards: %v", resp.Error)
	}
	return nil
}

func (c *serveConn) closes() error {
	c.in.Close()
	select {
	case err := <-c.exit:
		if err != nil {
			return err
		}
		return nil
	case <-time.After(5 * time.Second):
		return fmt.Errorf("still running 5 s after stdin closed")
	}
}

// changes checks the optional changes verb: a full listing whose pages
// end with a cursor (at most a few pages are walked), then changes since
// that cursor, and a cursor the adapter cannot use refused.
func (r *run) changes() {
	if !slices.Contains(r.verbs, orchapi.VerbChanges) {
		r.skip("changes", "not listed (zm mirrors this tracker with full lists)")
		return
	}
	var cursor string
	page := ""
	var err error
	pages := 0
	for ; pages < 3; pages++ {
		var res orchapi.AdapterChanges
		if err = must(r.call(orchapi.VerbChanges, "", orchapi.AdapterChangesArgs{Page: page}, &res)); err != nil {
			break
		}
		if res.Items == nil {
			err = fmt.Errorf(`result has no "items" array`)
			break
		}
		for _, it := range res.Items {
			if err == nil {
				err = r.checkItem(it, "")
			}
		}
		if err != nil || res.Next == "" {
			cursor = res.Cursor
			if err == nil && cursor == "" {
				err = fmt.Errorf("the last page of a full listing has no cursor")
			}
			break
		}
		page = res.Next
	}
	r.add("changes: a full listing returns valid items in pages ending with a cursor", err)
	if cursor == "" {
		r.skip("changes since a cursor", fmt.Sprintf("the listing has more than %d pages; not walked to its end", pages))
	} else {
		var res orchapi.AdapterChanges
		err = must(r.call(orchapi.VerbChanges, "", orchapi.AdapterChangesArgs{Since: cursor}, &res))
		if err == nil && res.Items == nil {
			err = fmt.Errorf(`result has no "items" array`)
		}
		r.add("changes since a cursor", err)
	}
	r.add("changes with a cursor it cannot use is refused (zm then lists in full)",
		r.expectRefusal(orchapi.VerbChanges, "", orchapi.AdapterChangesArgs{Since: "zm-conformance-bogus-cursor"}))
}
