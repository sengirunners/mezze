package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
	"time"
)

// drainWait bounds how long Serve keeps answering after its input closed.
var drainWait = 30 * time.Second

// pending counts requests read but not yet answered, so that when the input
// closes (`printf ... | zm mcp`) Serve answers what it already read before
// it stops, instead of dropping the replies.
type pending struct {
	mu   sync.Mutex
	n    int
	zero *sync.Cond
}

func newPending() *pending {
	p := &pending{}
	p.zero = sync.NewCond(&p.mu)
	return p
}

func (p *pending) add(d int) {
	p.mu.Lock()
	p.n += d
	if p.n <= 0 {
		p.n = 0
		p.zero.Broadcast()
	}
	p.mu.Unlock()
}

// wait returns once nothing is pending or after d.
func (p *pending) wait(d time.Duration) {
	t := time.AfterFunc(d, func() { p.mu.Lock(); p.n = 0; p.zero.Broadcast(); p.mu.Unlock() })
	defer t.Stop()
	p.mu.Lock()
	for p.n > 0 {
		p.zero.Wait()
	}
	p.mu.Unlock()
}

// message is the part of a JSON-RPC message pending looks at.
type message struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
}

// requests counts the requests (id and method) among newline-delimited
// messages; responses counts the replies (id, no method).
func count(b []byte, request bool) int {
	n := 0
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		var m message
		if len(bytes.TrimSpace(line)) == 0 || json.Unmarshal(line, &m) != nil || len(m.ID) == 0 || string(m.ID) == "null" {
			continue
		}
		if (m.Method != "") == request {
			n++
		}
	}
	return n
}

// drainReader counts each complete request line it passes on; at EOF it
// waits until they are answered.
type drainReader struct {
	r    io.Reader
	p    *pending
	part []byte
}

func (d *drainReader) Read(b []byte) (int, error) {
	n, err := d.r.Read(b)
	if n > 0 {
		buf := append(d.part, b[:n]...)
		i := bytes.LastIndexByte(buf, '\n')
		if i >= 0 {
			d.p.add(count(buf[:i], true))
			d.part = append([]byte(nil), buf[i+1:]...)
		} else {
			d.part = buf
		}
	}
	if err == io.EOF {
		if len(d.part) > 0 {
			d.p.add(count(d.part, true))
			d.part = nil
		}
		if n == 0 {
			d.p.wait(drainWait)
		}
	}
	return n, err
}

// drainWriter counts the responses written.
type drainWriter struct {
	w io.Writer
	p *pending
}

func (d drainWriter) Write(b []byte) (int, error) {
	n, err := d.w.Write(b)
	d.p.add(-count(b, false))
	return n, err
}

func (drainWriter) Close() error { return nil }
