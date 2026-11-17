// Package imgsafe makes an image safe to keep and serve as a work item's
// attachment (SPEC "Native issues", attachments): PNG, JPEG or WebP only,
// recognised from its bytes (never a name or a declared type), decoded and
// encoded again, so nothing but pixels survives (no EXIF, GPS, XMP, ICC,
// text chunks or trailing data). The daemon runs it on every attach; the CLI
// runs it first too, so what it sends fits a forwarded body.
package imgsafe

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"

	"golang.org/x/image/webp"
)

// Types an attachment may have.
const (
	PNG  = "image/png"
	JPEG = "image/jpeg"
	WebP = "image/webp"
)

const (
	// MaxBytes is an attachment's size after re-encoding.
	MaxBytes = 1 << 20
	// MaxPixels bounds an image's size (a 5K screenshot fits) and
	// MaxDecodeBytes what its decode may allocate: 8 bytes a pixel for a
	// 16-bit image, 4 otherwise.
	MaxPixels      = 20_000_000
	MaxDecodeBytes = 100 << 20
	// jpegQuality keeps a re-encoded photo close to its source.
	jpegQuality = 90
)

// Errors Clean answers (wrapped with detail).
var (
	ErrType     = errors.New("PNG, JPEG or WebP only")
	ErrTooLarge = errors.New("too large")
	ErrBad      = errors.New("not a readable image")
)

// Sniff is the type b's bytes say it is ("" = none of the three).
func Sniff(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return PNG
	case bytes.HasPrefix(b, []byte("\xff\xd8\xff")):
		return JPEG
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return WebP
	}
	return ""
}

// Clean decodes b and encodes it again, answering the new bytes and their
// type. A PNG stays a PNG and a JPEG a JPEG; a WebP becomes a JPEG when it
// is opaque, else a PNG (Go has no WebP encoder). The result is at most
// MaxBytes.
func Clean(b []byte) ([]byte, string, error) {
	typ := Sniff(b)
	var cfg func([]byte) (image.Config, error)
	var dec func([]byte) (image.Image, error)
	switch typ {
	case PNG:
		cfg = func(b []byte) (image.Config, error) { return png.DecodeConfig(bytes.NewReader(b)) }
		dec = func(b []byte) (image.Image, error) { return png.Decode(bytes.NewReader(b)) }
	case JPEG:
		cfg = func(b []byte) (image.Config, error) { return jpeg.DecodeConfig(bytes.NewReader(b)) }
		dec = func(b []byte) (image.Image, error) { return jpeg.Decode(bytes.NewReader(b)) }
	case WebP:
		cfg = func(b []byte) (image.Config, error) { return webp.DecodeConfig(bytes.NewReader(b)) }
		dec = func(b []byte) (image.Image, error) { return webp.Decode(bytes.NewReader(b)) }
	default:
		return nil, "", ErrType
	}
	c, err := cfg(b)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrBad, err)
	}
	if c.Width <= 0 || c.Height <= 0 {
		return nil, "", fmt.Errorf("%w: empty image", ErrBad)
	}
	px := int64(c.Width) * int64(c.Height)
	if px > MaxPixels || px*bytesPerPixel(c.ColorModel) > MaxDecodeBytes {
		return nil, "", fmt.Errorf("%w: %dx%d is over what an attachment may be (%d pixels, %d at 16 bits)", ErrTooLarge, c.Width, c.Height, MaxPixels, MaxDecodeBytes/8)
	}
	img, err := dec(b)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrBad, err)
	}
	out := typ
	if typ == WebP {
		out = PNG
		if o, ok := img.(interface{ Opaque() bool }); ok && o.Opaque() {
			out = JPEG
		}
	}
	var buf bytes.Buffer
	if out == PNG {
		err = (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&buf, img)
	} else {
		err = jpeg.Encode(&buf, img, &jpeg.Options{Quality: jpegQuality})
	}
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrBad, err)
	}
	if buf.Len() > MaxBytes {
		return nil, "", fmt.Errorf("%w: %d bytes after re-encoding, over %d (crop or scale it down)", ErrTooLarge, buf.Len(), MaxBytes)
	}
	return buf.Bytes(), out, nil
}

// bytesPerPixel is what a decoded image of model takes per pixel.
func bytesPerPixel(m color.Model) int64 {
	switch m {
	case color.RGBA64Model, color.NRGBA64Model:
		return 8
	}
	return 4
}
