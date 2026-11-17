package approvals

import (
	"errors"
	"fmt"
	"strings"
)

// A small QR code encoder for the pairing URL `zm approver add` draws in
// the terminal (no QR dependency is pinned): byte mode, error correction
// level L, versions 1 to 5 (one Reed-Solomon block each, up to 106 bytes),
// the mask with the lowest ISO 18004 penalty.

// QRCode is a square of modules; true is dark.
type QRCode struct {
	Size    int
	Modules [][]bool
}

// ErrQRTooLong means the text does not fit a version 1-5 code.
var ErrQRTooLong = errors.New("text too long for a small QR code")

var (
	qrDataCodewords = [...]int{0, 19, 34, 55, 80, 108} // per version, level L
	qrECCodewords   = [...]int{0, 7, 10, 15, 20, 26}
)

// QR encodes text.
func QR(text string) (*QRCode, error) {
	data := []byte(text)
	ver := 0
	for v := 1; v <= 5; v++ {
		if 4+8+8*len(data) <= 8*qrDataCodewords[v] {
			ver = v
			break
		}
	}
	if ver == 0 {
		return nil, ErrQRTooLong
	}
	// Bit stream: mode 0100 (byte), 8-bit count, data, terminator, padding.
	var bits []bool
	put := func(v, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, (v>>i)&1 == 1)
		}
	}
	put(0b0100, 4)
	put(len(data), 8)
	for _, b := range data {
		put(int(b), 8)
	}
	capBits := 8 * qrDataCodewords[ver]
	put(0, min(4, capBits-len(bits)))
	for len(bits)%8 != 0 {
		bits = append(bits, false)
	}
	for pad := 0xEC; len(bits) < capBits; pad ^= 0xEC ^ 0x11 {
		put(pad, 8)
	}
	cw := make([]byte, len(bits)/8)
	for i, b := range bits {
		if b {
			cw[i/8] |= 0x80 >> (i % 8)
		}
	}
	cw = append(cw, rsRemainder(cw, qrECCodewords[ver])...)

	q := newQR(ver)
	q.placeData(cw)
	best, bestScore := 0, -1
	for m := 0; m < 8; m++ {
		q.applyMask(m)
		q.drawFormat(m)
		if s := q.penalty(); bestScore < 0 || s < bestScore {
			best, bestScore = m, s
		}
		q.applyMask(m) // undo (XOR)
	}
	q.applyMask(best)
	q.drawFormat(best)
	return &QRCode{Size: q.size, Modules: q.mod}, nil
}

type qrBuilder struct {
	size int
	mod  [][]bool
	fn   [][]bool // function modules (never masked)
}

func newQR(ver int) *qrBuilder {
	n := 17 + 4*ver
	q := &qrBuilder{size: n, mod: make([][]bool, n), fn: make([][]bool, n)}
	for i := range q.mod {
		q.mod[i], q.fn[i] = make([]bool, n), make([]bool, n)
	}
	for i := 0; i < n; i++ { // timing patterns
		q.set(6, i, i%2 == 0)
		q.set(i, 6, i%2 == 0)
	}
	for _, c := range [][2]int{{3, 3}, {n - 4, 3}, {3, n - 4}} { // finders + separators
		for dy := -4; dy <= 4; dy++ {
			for dx := -4; dx <= 4; dx++ {
				x, y := c[0]+dx, c[1]+dy
				if x >= 0 && x < n && y >= 0 && y < n {
					d := max(abs(dx), abs(dy))
					q.set(x, y, d != 2 && d != 4)
				}
			}
		}
	}
	if ver >= 2 { // one alignment pattern, bottom right
		c := n - 7
		for dy := -2; dy <= 2; dy++ {
			for dx := -2; dx <= 2; dx++ {
				q.set(c+dx, c+dy, max(abs(dx), abs(dy)) != 1)
			}
		}
	}
	q.drawFormat(0) // reserve the format areas (and the dark module)
	return q
}

// set draws a function module at column x, row y.
func (q *qrBuilder) set(x, y int, dark bool) {
	q.mod[y][x], q.fn[y][x] = dark, true
}

func (q *qrBuilder) drawFormat(mask int) {
	data := 1<<3 | mask // level L is 01
	rem := data
	for i := 0; i < 10; i++ {
		rem = (rem << 1) ^ ((rem >> 9) * 0x537)
	}
	bits := (data<<10 | rem) ^ 0x5412
	bit := func(i int) bool { return (bits>>i)&1 == 1 }
	n := q.size
	for i := 0; i <= 5; i++ {
		q.set(8, i, bit(i))
	}
	q.set(8, 7, bit(6))
	q.set(8, 8, bit(7))
	q.set(7, 8, bit(8))
	for i := 9; i < 15; i++ {
		q.set(14-i, 8, bit(i))
	}
	for i := 0; i < 8; i++ {
		q.set(n-1-i, 8, bit(i))
	}
	for i := 8; i < 15; i++ {
		q.set(8, n-15+i, bit(i))
	}
	q.set(8, n-8, true)
}

// placeData lays codewords out in the zigzag order, right to left in
// column pairs, skipping the vertical timing column.
func (q *qrBuilder) placeData(cw []byte) {
	n, i := q.size, 0
	for right := n - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vert := 0; vert < n; vert++ {
			for j := 0; j < 2; j++ {
				x := right - j
				y := vert
				if (right+1)&2 == 0 {
					y = n - 1 - vert
				}
				if !q.fn[y][x] && i < len(cw)*8 {
					q.mod[y][x] = (cw[i/8]>>(7-i%8))&1 == 1
					i++
				}
			}
		}
	}
}

func (q *qrBuilder) applyMask(m int) {
	for y := 0; y < q.size; y++ {
		for x := 0; x < q.size; x++ {
			var inv bool
			switch m {
			case 0:
				inv = (x+y)%2 == 0
			case 1:
				inv = y%2 == 0
			case 2:
				inv = x%3 == 0
			case 3:
				inv = (x+y)%3 == 0
			case 4:
				inv = (x/3+y/2)%2 == 0
			case 5:
				inv = x*y%2+x*y%3 == 0
			case 6:
				inv = (x*y%2+x*y%3)%2 == 0
			case 7:
				inv = ((x+y)%2+x*y%3)%2 == 0
			}
			if inv && !q.fn[y][x] {
				q.mod[y][x] = !q.mod[y][x]
			}
		}
	}
}

// penalty is the ISO 18004 mask score (runs, 2x2 blocks, finder-like
// patterns, dark balance); lower is better.
func (q *qrBuilder) penalty() int {
	n, score := q.size, 0
	at := func(x, y int, col bool) bool {
		if col {
			return q.mod[x][y]
		}
		return q.mod[y][x]
	}
	for _, col := range []bool{false, true} {
		for y := 0; y < n; y++ {
			run := 1
			var line strings.Builder
			for x := 0; x < n; x++ {
				if at(x, y, col) {
					line.WriteByte('1')
				} else {
					line.WriteByte('0')
				}
				if x > 0 && at(x, y, col) == at(x-1, y, col) {
					run++
					if run == 5 {
						score += 3
					} else if run > 5 {
						score++
					}
				} else {
					run = 1
				}
			}
			s := "0000" + line.String() + "0000"
			score += 40 * (strings.Count(s, "10111010000") + strings.Count(s, "00001011101"))
		}
	}
	dark := 0
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if q.mod[y][x] {
				dark++
			}
			if x+1 < n && y+1 < n {
				c := q.mod[y][x]
				if q.mod[y][x+1] == c && q.mod[y+1][x] == c && q.mod[y+1][x+1] == c {
					score += 3
				}
			}
		}
	}
	k := abs(dark*20-n*n*10) / (n * n) // 5% steps away from 50%
	return score + 10*k
}

// rsRemainder is the Reed-Solomon error correction for data over GF(256)
// (polynomial 0x11D), with n check codewords.
func rsRemainder(data []byte, n int) []byte {
	gen := make([]byte, n) // generator coefficients, highest degree first (the leading 1 omitted)
	gen[n-1] = 1
	root := byte(1)
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			gen[j] = gfMul(gen[j], root)
			if j+1 < n {
				gen[j] ^= gen[j+1]
			}
		}
		root = gfMul(root, 0x02)
	}
	rem := make([]byte, n)
	for _, b := range data {
		f := b ^ rem[0]
		copy(rem, rem[1:])
		rem[n-1] = 0
		for i := range rem {
			rem[i] ^= gfMul(gen[i], f)
		}
	}
	return rem
}

func gfMul(x, y byte) byte {
	z := 0
	for i := 7; i >= 0; i-- {
		z = (z << 1) ^ ((z >> 7) * 0x11D)
		z ^= int((y>>i)&1) * int(x)
	}
	return byte(z)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// Terminal draws the code with half blocks and explicit black/white colors
// (it scans on dark and light terminal themes alike), with a quiet zone.
func (c *QRCode) Terminal() string {
	const quiet = 4
	n := c.Size + 2*quiet
	dark := func(x, y int) bool {
		x, y = x-quiet, y-quiet
		return x >= 0 && y >= 0 && x < c.Size && y < c.Size && c.Modules[y][x]
	}
	color := func(d bool, fg bool) string {
		switch {
		case d && fg:
			return "30"
		case d:
			return "40"
		case fg:
			return "97"
		}
		return "107"
	}
	var b strings.Builder
	for y := 0; y < n; y += 2 {
		for x := 0; x < n; x++ {
			b.WriteString("\x1b[" + color(dark(x, y), true) + ";" + color(dark(x, y+1), false) + "m▀")
		}
		b.WriteString("\x1b[0m\n")
	}
	return b.String()
}

// SVG draws the code as an inline SVG of black modules on white, with a
// quiet zone, sized by its container.
func (c *QRCode) SVG() string {
	const quiet = 4
	n := c.Size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="qr" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img" aria-label="QR code for this page"><rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n, n, n)
	for y := 0; y < c.Size; y++ {
		for x := 0; x < c.Size; x++ {
			if c.Modules[y][x] {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String()
}
