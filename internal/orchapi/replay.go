package orchapi

import (
	"encoding/binary"
	"errors"
	"io"
)

// AttachReplayProtocol is opt-in, outbound framed replay for a fresh embedded
// surface. Inbound keystrokes retain the existing raw attach protocol.
const AttachReplayProtocol = "zm-pty-replay-v1"

const HeaderReplaySurface = "Zm-Replay-Surface"

const FreshReplaySurface = "fresh-v1"

const (
	ReplayMetadata     byte = 'M' // 8-byte offset +44-byte checkpoint schema1
	ReplayOutput       byte = 'O' // 8-byte offset +1..32KiB raw bytes
	ReplayGap          byte = 'G' // 8-byte next offset; reconnect a NEW surface
	ReplayEnd          byte = 'X' // empty; normal output end
	ReplayMaxFrame          = (32 << 10) + 8
	ReplayMetadataSize      = 52
)

var ErrReplayFrame = errors.New("invalid framed terminal replay")

func validReplayFrame(kind byte, p []byte) bool {
	switch kind {
	case ReplayMetadata:
		return len(p) == ReplayMetadataSize && int64(binary.BigEndian.Uint64(p[:8])) >= 0
	case ReplayOutput:
		return len(p) > 8 && len(p) <= ReplayMaxFrame && int64(binary.BigEndian.Uint64(p[:8])) >= 0
	case ReplayGap:
		return len(p) == 8 && int64(binary.BigEndian.Uint64(p)) >= 0
	case ReplayEnd:
		return len(p) == 0
	}
	return false
}

// WriteReplayFrame writes a validated type-u8/length-u32-BE/payload frame.
func WriteReplayFrame(w io.Writer, kind byte, p []byte) error {
	if !validReplayFrame(kind, p) {
		return ErrReplayFrame
	}
	b := make([]byte, 5+len(p))
	b[0] = kind
	binary.BigEndian.PutUint32(b[1:5], uint32(len(p)))
	copy(b[5:], p)
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}

// ReadReplayFrame bounds allocation before reading a payload. EOF between
// frames is distinguishable from the explicit ReplayEnd marker.
func ReadReplayFrame(r io.Reader) (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > ReplayMaxFrame {
		return 0, nil, ErrReplayFrame
	}
	p := make([]byte, int(n))
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	if !validReplayFrame(h[0], p) {
		return 0, nil, ErrReplayFrame
	}
	return h[0], p, nil
}

// ReplayBytes encodes an offset plus bounded raw output (or an empty gap).
func ReplayBytes(from int64, data []byte) ([]byte, error) {
	if from < 0 || len(data) > 32<<10 || int64(len(data)) > int64(^uint64(0)>>1)-from {
		return nil, ErrReplayFrame
	}
	p := make([]byte, 8+len(data))
	binary.BigEndian.PutUint64(p, uint64(from))
	copy(p[8:], data)
	return p, nil
}

// ParseReplayBytes rejects negative/overflowing raw cursors before use.
func ParseReplayBytes(p []byte) (int64, []byte, error) {
	if len(p) < 8 || len(p) > ReplayMaxFrame {
		return 0, nil, ErrReplayFrame
	}
	from := int64(binary.BigEndian.Uint64(p))
	if from < 0 || int64(len(p)-8) > int64(^uint64(0)>>1)-from {
		return 0, nil, ErrReplayFrame
	}
	return from, p[8:], nil
}
