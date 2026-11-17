package imgsafe

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"testing"
)

func pic(w, h int) *image.NRGBA {
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			m.Set(x, y, color.NRGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	return m
}

func encPNG(t *testing.T, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// chunk is one PNG chunk with its CRC.
func chunk(typ string, data []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(data)))
	out = append(out, typ...)
	out = append(out, data...)
	return binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(append([]byte(typ), data...)))
}

// A PNG keeps its pixels and loses its text chunks; a JPEG loses its EXIF
// (GPS) segment; anything after the image is gone.
func TestCleanStripsMetadata(t *testing.T) {
	const secret = "GPS 37.7749 N, 122.4194 W"
	p := encPNG(t, pic(40, 30))
	// tEXt after IHDR (8 signature bytes + 25 IHDR bytes).
	withText := append(append(append([]byte{}, p[:33]...), chunk("tEXt", []byte("Comment\x00"+secret))...), p[33:]...)
	withText = append(withText, []byte("trailing "+secret)...)
	out, typ, err := Clean(withText)
	if err != nil || typ != PNG {
		t.Fatalf("png: %v %s", err, typ)
	}
	if bytes.Contains(out, []byte(secret)) {
		t.Fatal("png text chunk survived")
	}
	got, err := png.Decode(bytes.NewReader(out))
	if err != nil || got.Bounds().Dx() != 40 || got.Bounds().Dy() != 30 {
		t.Fatalf("png pixels: %v %v", got.Bounds(), err)
	}

	var j bytes.Buffer
	if err := jpeg.Encode(&j, pic(40, 30), nil); err != nil {
		t.Fatal(err)
	}
	app1 := []byte("Exif\x00\x00" + secret)
	seg := append([]byte{0xff, 0xe1}, binary.BigEndian.AppendUint16(nil, uint16(len(app1)+2))...)
	withExif := append(append(append([]byte{}, j.Bytes()[:2]...), append(seg, app1...)...), j.Bytes()[2:]...)
	if _, err := jpeg.Decode(bytes.NewReader(withExif)); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	out, typ, err = Clean(withExif)
	if err != nil || typ != JPEG {
		t.Fatalf("jpeg: %v %s", err, typ)
	}
	if bytes.Contains(out, []byte(secret)) || bytes.Contains(out, []byte("Exif")) {
		t.Fatal("jpeg exif survived")
	}
}

// A lossless 1x1 WebP with alpha: decoded and kept as a PNG (Go has no
// WebP encoder).
const webp1x1 = "UklGRhoAAABXRUJQVlA4TA0AAAAvAAAAEAcQERGIiP4HAA=="

func TestCleanWebP(t *testing.T) {
	b, _ := base64.StdEncoding.DecodeString(webp1x1)
	if Sniff(b) != WebP {
		t.Fatalf("sniff: %q", Sniff(b))
	}
	out, typ, err := Clean(b)
	if err != nil {
		t.Fatal(err)
	}
	if typ != PNG && typ != JPEG {
		t.Fatalf("type %s", typ)
	}
	if Sniff(out) != typ {
		t.Fatalf("answer is %s but its bytes say %s", typ, Sniff(out))
	}
}

// Only PNG, JPEG and WebP, by their bytes: SVG, GIF, HTML and a PNG
// signature over garbage are refused.
func TestCleanRefuses(t *testing.T) {
	for name, b := range map[string][]byte{
		"svg":  []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"gif":  []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"),
		"html": []byte("<html><body>hi</body></html>"),
		"none": nil,
	} {
		if _, _, err := Clean(b); !errors.Is(err, ErrType) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, _, err := Clean([]byte("\x89PNG\r\n\x1a\nnot really")); !errors.Is(err, ErrBad) {
		t.Errorf("fake png: %v", err)
	}
}

// Too many pixels is refused before decoding (the header says so); an image
// over MaxBytes after re-encoding is refused.
func TestCleanLimits(t *testing.T) {
	p := encPNG(t, pic(4, 4))
	ihdr := append([]byte{}, p[16:29]...) // width, height, depth, color, ...
	binary.BigEndian.PutUint32(ihdr[0:], 6000)
	binary.BigEndian.PutUint32(ihdr[4:], 6000)
	huge := append(append(append([]byte{}, p[:8]...), chunk("IHDR", ihdr)...), p[33:]...)
	if _, _, err := Clean(huge); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("36 megapixels: %v", err)
	}
	// 16 megapixels pass the pixel cap, but at 16 bits a channel they would
	// take 128 MB to decode.
	binary.BigEndian.PutUint32(ihdr[0:], 4000)
	binary.BigEndian.PutUint32(ihdr[4:], 4000)
	ihdr[8], ihdr[9] = 16, 6 // 16-bit RGBA
	deep := append(append(append([]byte{}, p[:8]...), chunk("IHDR", ihdr)...), p[33:]...)
	if _, _, err := Clean(deep); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("16 megapixels at 16 bits: %v", err)
	}
	noise := image.NewNRGBA(image.Rect(0, 0, 700, 700))
	r := rand.New(rand.NewPCG(1, 2))
	for i := range noise.Pix {
		noise.Pix[i] = uint8(r.Uint32())
	}
	if _, _, err := Clean(encPNG(t, noise)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("2 MB of noise: %v", err)
	}
}

func TestIsHTML(t *testing.T) {
	for _, c := range []struct {
		name string
		in   string
		want bool
	}{
		{"doctype", "<!doctype html><p>hi", true},
		{"uppercase doctype", "<!DOCTYPE HTML>", true},
		{"BOM", "\xef\xbb\xbf<html>", true},
		{"leading comment", "<!-- mock -->\n  <body>", true},
		{"two comments", "<!--a--><!--b--><div>", true},
		{"paragraph", "<p>hi</p>", true},
		{"header", "<header>", true},
		{"pre is not p", "<pre>", false},
		{"comment only", "<!-- just this -->", false},
		{"unterminated comment", "<!-- <html>", false},
		{"xml", `<?xml version="1.0"?><html/>`, false},
		{"svg after a comment", "<!-- x --><svg xmlns=\"http://www.w3.org/2000/svg\"/>", false},
		{"NUL", "<html>\x00</html>", false},
		{"not UTF-8", "<html>\xff</html>", false},
		{"text", "hello", false},
		{"empty", "", false},
	} {
		if got := IsHTML([]byte(c.in)); got != c.want {
			t.Errorf("%s: IsHTML = %v, want %v", c.name, got, c.want)
		}
	}
}

// ForUpload is what the CLI and the MCP server send: HTML as it is, a small
// image as it is, a large one re-encoded under ForwardSafe, others refused.
func TestForUpload(t *testing.T) {
	page := []byte("<!doctype html><style>p{color:red}</style><p>hi")
	if got, err := ForUpload(page); err != nil || !bytes.Equal(got, page) {
		t.Fatalf("html: %v", err)
	}
	if _, err := ForUpload(append([]byte("<html>"), bytes.Repeat([]byte("a"), MaxBytes)...)); err == nil {
		t.Fatal("an HTML page over MaxBytes passed")
	}
	if _, err := ForUpload([]byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)); err == nil {
		t.Fatal("svg passed")
	}
	var small bytes.Buffer
	if err := png.Encode(&small, pic(8, 8)); err != nil {
		t.Fatal(err)
	}
	if got, err := ForUpload(small.Bytes()); err != nil || !bytes.Equal(got, small.Bytes()) {
		t.Fatalf("small image changed: %v", err)
	}
	big := append(bytes.Clone(small.Bytes()), make([]byte, ForwardSafe)...)
	if got, err := ForUpload(big); err != nil || len(got) > ForwardSafe || Sniff(got) != PNG {
		t.Fatalf("large image: %d bytes, %v", len(got), err)
	}
}
