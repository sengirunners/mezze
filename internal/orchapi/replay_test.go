package orchapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

type replayShortWriter struct{}

func (replayShortWriter) Write(b []byte) (int, error) { return len(b) - 1, nil }
func TestFramedReplayCodecBounds(t *testing.T) {
	meta := make([]byte, ReplayMetadataSize)
	meta[8] = 1
	output, _ := ReplayBytes(17, []byte("invented\x1b]52;opaque\a"))
	gap, _ := ReplayBytes(99, nil)
	for _, tc := range []struct {
		k byte
		p []byte
	}{{ReplayMetadata, meta}, {ReplayOutput, output}, {ReplayGap, gap}, {ReplayEnd, nil}} {
		var b bytes.Buffer
		if err := WriteReplayFrame(&b, tc.k, tc.p); err != nil {
			t.Fatal(err)
		}
		k, p, err := ReadReplayFrame(&b)
		if err != nil || k != tc.k || !bytes.Equal(p, tc.p) {
			t.Fatal("roundtrip")
		}
		if err = WriteReplayFrame(replayShortWriter{}, tc.k, tc.p); !errors.Is(err, io.ErrShortWrite) {
			t.Fatal("short write hidden")
		}
	}
	for _, tc := range []struct {
		k byte
		p []byte
	}{{'?', nil}, {ReplayMetadata, nil}, {ReplayOutput, make([]byte, 8)}, {ReplayEnd, []byte("x")}, {ReplayGap, make([]byte, 9)}} {
		if WriteReplayFrame(io.Discard, tc.k, tc.p) == nil {
			t.Fatal("invalid frame accepted")
		}
	}
	var header [5]byte
	header[0] = ReplayOutput
	binary.BigEndian.PutUint32(header[1:], ReplayMaxFrame+1)
	if _, _, err := ReadReplayFrame(bytes.NewReader(header[:])); !errors.Is(err, ErrReplayFrame) {
		t.Fatal("oversize header allocated/read")
	}
	if _, err := ReplayBytes(1<<63-1, []byte("x")); err == nil {
		t.Fatal("offset overflow")
	}
	if _, err := ReplayBytes(-1, nil); err == nil {
		t.Fatal("negative offset")
	}
	if _, _, err := ReadReplayFrame(bytes.NewReader([]byte{'O', 0, 0})); err == nil {
		t.Fatal("truncated header")
	}
}
