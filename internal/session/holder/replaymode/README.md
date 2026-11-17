# Replay mode checkpoints

The holder and daemon mirror retain these checkpoints (Stage B); terminal
attach restoration remains unwired pending independent review and Stage C.
`Parser` is a bounded value checkpoint (copy it by value) of **observed** effective
screen selection, mouse selector/encoding, and bracketed paste. Its zero value
starts at a known Ground boundary with no assumed mode defaults. `Lost()` marks
both a lost boundary and unknown modes; ESC/CAN/SUB can reestablish grammar.

`Feed` consumes output only, performs no I/O, allocates no payload storage, and
keeps at most16 numeric CSI parameters/128 command bytes. It retains no OSC/DCS
payload. `Modes()` separates known from value. `SafeBoundary()` is false inside
an escape/string/UTF-8 fragment. `Prefix()` emits fixed whitelist grammar for
known groups only, and emits nothing at an unsafe boundary. Callers must examine
known flags: an empty prefix does not mean a normal terminal. A partial prefix
is not proof of complete terminal state.

Prefix bytes are for a **new frontend surface only**. Never put them in PTY input,
raw output, saved transcript, or byte-offset accounting. A future replay caller
must consume any leading partial sequence using the saved parser, wait for a
safe boundary, then generate the prefix from that updated state. This package
does not discard bytes or choose offsets for the caller.

Semantics were read from pinned Ghostty v1.4.0 commit
`83fb95624b0439ee660c2679b43b38c8b63769cd`, the version the embedded terminals were built
against:

- [setMode and restore_mode](https://github.com/ghostty-org/ghostty/blob/83fb95624b0439ee660c2679b43b38c8b63769cd/src/termio/stream_handler.zig): each mouse reset clears its effective selector/encoding, even when resetting a different raw bit.
- [switchScreenMode](https://github.com/ghostty-org/ghostty/blob/83fb95624b0439ee660c2679b43b38c8b63769cd/src/terminal/Terminal.zig):47/1047/1049 select one effective screen; prefix uses47 to avoid1049's save/clear operations. Cursor copy/restore and screen contents remain outside this model.
- [parse table](https://github.com/ghostty-org/ghostty/blob/83fb95624b0439ee660c2679b43b38c8b63769cd/src/terminal/parse_table.zig): ESC aborts control strings, CAN/SUB cancel, OSC accepts BEL, DCS does not; OSC/DCS high bytes are payload. SOS/PM/APC retain C1 transitions, even for bytes following a UTF-8 lead; this is deliberately a different grammar state. Ground is UTF-8 only. Raw C1 in incomplete control grammar is conservatively unknown rather than guessed.
- [modes](https://github.com/ghostty-org/ghostty/blob/83fb95624b0439ee660c2679b43b38c8b63769cd/src/terminal/modes.zig) and [stream dispatch](https://github.com/ghostty-org/ghostty/blob/83fb95624b0439ee660c2679b43b38c8b63769cd/src/terminal/stream.zig).

RIS/DECSTR clear confidence rather than invent configuration-dependent defaults.
Private mode restore invalidates its relevant group; saved raw mode bits are not
reconstructed. Unsupported modes, focus reporting, synchronized output, cursor,
wrap, rendition, margins, charsets, clipboard/title/window operations and queries
are not synthesized. Malformed/overflowing mode commands invalidate confidence;
unsupported non-mode commands do not erase unrelated observed modes. This is a
conservative grammar subset, not a full replacement Ghostty parser.

Tests use synthetic bytes only. They check source-derived selector semantics,
every chunk split, value checkpoint copying, long opaque strings with no Feed
allocations, cancellation/C1/UTF-8, overflows, safe replay boundaries and invented
>1MiB redraw history. They do not execute Ghostty or prove screen fidelity.
Ring/offset/protocol and synthetic Ghostty integration tests are later stages.


Stage B uses a fixed44-byte version1 binary checkpoint containing bounded enums,
flags and numeric CSI parameters; invalid schemas leave the receiver unchanged.
Holder M metadata adds an8-byte raw offset. The enclosing holder protocol
advertises/negotiates replay_modes1 and keeps the PTY descriptor first. The ring
stages checkpoint metadata separately and consumes it only with its matching raw
WriteAt; a gap without valid metadata becomes Lost. No prefix is rendered yet.
