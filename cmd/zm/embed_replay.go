package main

import (
	"errors"
	"io"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/session/holder/replaymode"
)

var errReplayReconnect = errors.New("terminal replay gap; reconnect a fresh surface")

const replayDegradedNotice = "Terminal replay is incomplete; mouse capture may be unavailable. Recorded conversation is still available."

type replayRenderResult struct {
	Cursor, Skipped int64
	Ended           bool
}

// renderEmbedReplay has no managed-input writer: restoration can leave only
// through frontend stdout. It neither owns nor changes the existing key loop.
func renderEmbedReplay(out io.Writer, src io.Reader, output func(), degrade func(string)) (replayRenderResult, error) {
	var result replayRenderResult
	kind, body, err := orchapi.ReadReplayFrame(src)
	if err != nil {
		return result, err
	}
	if kind != orchapi.ReplayMetadata {
		return result, orchapi.ErrReplayFrame
	}
	// M was size-checked above. Only its first8 bytes are a raw cursor;
	// the44 checkpoint bytes are control state, not output to consume.
	from, _, err := orchapi.ParseReplayBytes(body[:8])
	stateBytes := body[8:]
	if err != nil {
		return result, err
	}
	var state replaymode.Parser
	if err = state.UnmarshalBinary(stateBytes); err != nil {
		return result, orchapi.ErrReplayFrame
	}
	result.Cursor = from
	started, notified := false, false
	warn := func() {
		if !notified {
			notified = true
			if degrade != nil {
				degrade(replayDegradedNotice)
			}
		}
	}
	emit := func() error {
		m := state.Modes()
		if !m.Complete() || m.Mouse != replaymode.NoEvents || m.Encoding != replaymode.X10Format || result.Skipped != 0 {
			warn()
		}
		// Mouse modes are deliberately OMITTED: the daemon's UTF8 mouse
		// classifier still derives from raw bytes. Rendering metadata never
		// enables paste permission or changes any input-authority state.
		var prefix []byte
		if m.AlternateKnown {
			if m.Alternate {
				prefix = append(prefix, "\x1b[?47h"...)
			} else {
				prefix = append(prefix, "\x1b[?47l"...)
			}
		}
		if m.PasteKnown {
			if m.Paste {
				prefix = append(prefix, "\x1b[?2004h"...)
			} else {
				prefix = append(prefix, "\x1b[?2004l"...)
			}
		}
		started = true
		return writeReplayFrontend(out, prefix)
	}
	if state.SafeBoundary() {
		if err = emit(); err != nil {
			return result, err
		}
	} else {
		warn()
	}
	for {
		kind, body, err = orchapi.ReadReplayFrame(src)
		if err != nil {
			return result, err
		}
		switch kind {
		case orchapi.ReplayMetadata:
			return result, orchapi.ErrReplayFrame // never reset an existing surface
		case orchapi.ReplayGap:
			next, _, e := orchapi.ParseReplayBytes(body)
			if e != nil || next <= result.Cursor {
				return result, orchapi.ErrReplayFrame
			}
			return result, errReplayReconnect
		case orchapi.ReplayEnd:
			result.Ended = true
			return result, nil
		case orchapi.ReplayOutput:
			from, data, e := orchapi.ParseReplayBytes(body)
			if e != nil || from != result.Cursor {
				return result, errReplayReconnect
			}
			result.Cursor += int64(len(data))
			if output != nil {
				output()
			}
			if !started {
				consumed := 0
				for consumed < len(data) && !state.SafeBoundary() {
					state.Feed(data[consumed : consumed+1])
					consumed++
					result.Skipped++
				}
				data = data[consumed:]
				if state.SafeBoundary() {
					if err = emit(); err != nil {
						return result, err
					}
				}
			}
			if started {
				if err = writeReplayFrontend(out, data); err != nil {
					return result, err
				}
			}
		default:
			return result, orchapi.ErrReplayFrame
		}
	}
}
func writeReplayFrontend(out io.Writer, b []byte) error {
	if len(b) == 0 {
		return nil
	}
	n, err := out.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}
