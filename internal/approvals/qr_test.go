package approvals

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"strings"
	"testing"
)

// The URL `zm approver add` encodes, at its longest realistic length.
const qrSample = "http://zm-ivo-mac.tail1234.ts.net:7433/v0/approvals?code=ABCDEFGH"

func TestQRStructure(t *testing.T) {
	for _, text := range []string{"x", "http://zm-mini:7433/v0/approvals?code=ABCDEFGH", qrSample, strings.Repeat("a", 106)} {
		q, err := QR(text)
		if err != nil {
			t.Fatalf("%d bytes: %v", len(text), err)
		}
		n := q.Size
		// Finder patterns: dark 7x7 ring, light ring, dark 3x3 core.
		for _, c := range [][2]int{{0, 0}, {n - 7, 0}, {0, n - 7}} {
			for dy := 0; dy < 7; dy++ {
				for dx := 0; dx < 7; dx++ {
					ring := max(abs(dx-3), abs(dy-3))
					if q.Modules[c[1]+dy][c[0]+dx] != (ring != 2) {
						t.Fatalf("%q: finder at %v broken at %d,%d", text, c, dx, dy)
					}
				}
			}
		}
		for i := 8; i < n-8; i++ { // timing
			if q.Modules[6][i] != (i%2 == 0) || q.Modules[i][6] != (i%2 == 0) {
				t.Fatalf("%q: timing pattern broken at %d", text, i)
			}
		}
		if !q.Modules[n-8][8] {
			t.Fatal("dark module missing")
		}
		// Both format copies agree.
		var a, b []bool
		for i := 0; i <= 5; i++ {
			a = append(a, q.Modules[i][8])
		}
		a = append(a, q.Modules[7][8], q.Modules[8][8], q.Modules[8][7])
		for i := 9; i < 15; i++ {
			a = append(a, q.Modules[8][14-i])
		}
		for i := 0; i < 8; i++ {
			b = append(b, q.Modules[8][n-1-i])
		}
		for i := 8; i < 15; i++ {
			b = append(b, q.Modules[n-15+i][8])
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("%q: format copies differ at bit %d", text, i)
			}
		}
	}
	if _, err := QR(strings.Repeat("a", 107)); err != ErrQRTooLong {
		t.Fatalf("107 bytes: %v", err)
	}
	q, _ := QR(qrSample)
	if s := q.Terminal(); strings.Count(s, "\n") != (q.Size+8+1)/2 {
		t.Fatalf("terminal rendering has %d lines", strings.Count(s, "\n"))
	}
}

// Reed-Solomon: every codeword polynomial vanishes at alpha^0..alpha^(n-1).
func TestRSRemainder(t *testing.T) {
	data := []byte("zm approvals rs check")
	for _, n := range []int{7, 10, 15, 20, 26} {
		cw := append(append([]byte{}, data...), rsRemainder(data, n)...)
		root := byte(1)
		for i := 0; i < n; i++ {
			var v byte
			for _, c := range cw {
				v = gfMul(v, root) ^ c
			}
			if v != 0 {
				t.Fatalf("n=%d: syndrome %d = %d", n, i, v)
			}
			root = gfMul(root, 2)
		}
	}
}

// ZM_QR_PNG=<path> writes the sample as a PNG, for checking with a real
// decoder by hand (never in CI).
func TestQRWritePNG(t *testing.T) {
	path := os.Getenv("ZM_QR_PNG")
	if path == "" {
		t.Skip("set ZM_QR_PNG to write the sample")
	}
	text := qrSample
	if s := os.Getenv("ZM_QR_TEXT"); s != "" {
		text = s
	}
	q, err := QR(text)
	if err != nil {
		t.Fatal(err)
	}
	const scale, quiet = 10, 4
	n := (q.Size + 2*quiet) * scale
	img := image.NewGray(image.Rect(0, 0, n, n))
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			mx, my := x/scale-quiet, y/scale-quiet
			c := color.Gray{255}
			if mx >= 0 && my >= 0 && mx < q.Size && my < q.Size && q.Modules[my][mx] {
				c = color.Gray{0}
			}
			img.SetGray(x, y, c)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}
