package replaymode

import (
	"encoding/binary"
	"errors"
)

// CheckpointSize is the fixed-size version1 wire form. It contains bounded
// numeric grammar state only, never input/string payloads or escape text.
const CheckpointSize = 44

var ErrCheckpoint = errors.New("invalid replay mode checkpoint")

// MarshalBinary encodes an exact value checkpoint, including partial grammar.
func (p Parser) MarshalBinary() ([]byte, error) {
	b := make([]byte, CheckpointSize)
	b[0] = 1
	b[1] = byte(p.state)
	b[2] = p.utf8
	b[3] = p.count
	b[4] = p.length
	b[6] = p.intermediate
	if p.private {
		b[5] |= 1
	}
	if p.digits {
		b[5] |= 2
	}
	if p.invalid {
		b[5] |= 4
	}
	if p.modes.Alternate {
		b[7] |= 1
	}
	if p.modes.AlternateKnown {
		b[7] |= 2
	}
	if p.modes.MouseKnown {
		b[7] |= 4
	}
	if p.modes.EncodingKnown {
		b[7] |= 8
	}
	if p.modes.Paste {
		b[7] |= 16
	}
	if p.modes.PasteKnown {
		b[7] |= 32
	}
	binary.BigEndian.PutUint16(b[8:10], uint16(p.modes.Mouse))
	binary.BigEndian.PutUint16(b[10:12], uint16(p.modes.Encoding))
	for i, v := range p.params {
		binary.BigEndian.PutUint16(b[12+i*2:], v)
	}
	return b, nil
}

// UnmarshalBinary validates the complete schema before replacing p. A failure
// leaves the receiver unchanged. Unknown versions must degrade to unknown.
func (p *Parser) UnmarshalBinary(b []byte) error {
	if len(b) != CheckpointSize || b[0] != 1 || grammar(b[1]) > unsynced || b[2] > 3 || b[3] > maxParams || b[4] > maxCSI || b[5]&^7 != 0 || b[7]&^63 != 0 || (b[6] != 0 && (b[6] < 0x20 || b[6] > 0x2f)) {
		return ErrCheckpoint
	}
	q := Parser{state: grammar(b[1]), utf8: b[2], count: b[3], length: b[4], private: b[5]&1 != 0, digits: b[5]&2 != 0, invalid: b[5]&4 != 0, intermediate: b[6]}
	if q.state == csi && q.count == 0 || q.utf8 != 0 && q.state != ground {
		return ErrCheckpoint
	}
	q.modes = Modes{Alternate: b[7]&1 != 0, AlternateKnown: b[7]&2 != 0, MouseKnown: b[7]&4 != 0, EncodingKnown: b[7]&8 != 0, Paste: b[7]&16 != 0, PasteKnown: b[7]&32 != 0, Mouse: Event(binary.BigEndian.Uint16(b[8:10])), Encoding: Format(binary.BigEndian.Uint16(b[10:12]))}
	switch q.modes.Mouse {
	case NoEvents, X10Events, NormalEvents, ButtonEvents, AnyEvents:
	default:
		return ErrCheckpoint
	}
	switch q.modes.Encoding {
	case X10Format, UTF8Format, SGRFormat, URXVTFormat, PixelFormat:
	default:
		return ErrCheckpoint
	}
	for i := range q.params {
		q.params[i] = binary.BigEndian.Uint16(b[12+i*2:])
	}
	*p = q
	return nil
}
