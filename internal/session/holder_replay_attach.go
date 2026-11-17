package session

import (
	"net"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/session/holder"
)

// streamReplayAttach changes only outbound representation. The existing attach
// handler owns ALL authorization, task/redaction gates and raw inbound keys.
func streamReplayAttach(r *holder.Ring, c net.Conn, detached <-chan struct{}, replay int64) {
	write := func(kind byte, p []byte) error {
		_ = c.SetWriteDeadline(time.Now().Add(30 * time.Second))
		return orchapi.WriteReplayFrame(c, kind, p)
	}
	start, end := r.Bounds()
	off := max(start, end-replay)
	first := r.NextReplay(off, 32<<10)
	metadata, err := holder.EncodeReplay(holder.ReplayCheckpoint{From: first.From, State: first.State})
	if err != nil {
		return
	}
	if write(orchapi.ReplayMetadata, metadata) != nil {
		return
	}
	data, from, next, wait, closed := first.Data, first.From, first.To, first.Wait, first.Closed
	off = first.From
	for {
		if from != off {
			gap, err := orchapi.ReplayBytes(from, nil)
			if err == nil {
				_ = write(orchapi.ReplayGap, gap)
			}
			return // caller closes; never restore modes on an existing surface
		}
		if len(data) > 0 {
			body, err := orchapi.ReplayBytes(from, data)
			if err != nil || write(orchapi.ReplayOutput, body) != nil {
				return
			}
			off = next
		}
		if closed {
			// Next may return a bounded part of a closed ring; drain all raw
			// bytes before announcing end, exactly like legacy attach.
			if _, end := r.Bounds(); off >= end {
				_ = write(orchapi.ReplayEnd, nil)
				return
			}
		}
		if len(data) == 0 {
			select {
			case <-wait:
			case <-detached:
				return
			}
		}
		data, from, next, wait, closed = r.Next(off, 32<<10)
	}
}
