package imgsafe

import (
	"bytes"
	"fmt"
	"unicode/utf8"
)

// HTML is the type of an attached HTML page: a self-contained mockup kept
// verbatim, served only inside a sandbox with no network.
const HTML = "text/html"

var htmlStarts = [][]byte{[]byte("<!doctype html"), []byte("<html"), []byte("<head"), []byte("<body"),
	[]byte("<meta"), []byte("<title"), []byte("<style"), []byte("<script"), []byte("<div"), []byte("<main"),
	[]byte("<section"), []byte("<article"), []byte("<table"), []byte("<h1"), []byte("<p>"), []byte("<p ")}

// IsHTML reports whether b is an HTML page by its bytes: valid UTF-8, no NUL,
// and after a BOM, blanks and comments it opens like a page (a doctype or a
// common element). SVG and XML never match.
func IsHTML(b []byte) bool {
	if !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	for {
		b = bytes.TrimLeft(b, " \t\r\n")
		if !bytes.HasPrefix(b, []byte("<!--")) {
			break
		}
		end := bytes.Index(b, []byte("-->"))
		if end < 0 {
			return false
		}
		b = b[end+3:]
	}
	head := bytes.ToLower(b[:min(len(b), 16)])
	for _, p := range htmlStarts {
		if bytes.HasPrefix(head, p) {
			return true
		}
	}
	return false
}

// ForwardSafe is a body a team replica forwards whole (its limit).
const ForwardSafe = 1 << 20

// ForUpload is what a client sends for an attachment (the CLI and the MCP
// server): an HTML page as it is, an image re-encoded first when it is over
// ForwardSafe, as the authority would; anything else is refused.
func ForUpload(b []byte) ([]byte, error) {
	if Sniff(b) == "" {
		if !IsHTML(b) {
			return nil, fmt.Errorf("not a PNG, JPEG or WebP image or a UTF-8 HTML page (by its bytes; SVG is not taken)")
		}
		if len(b) > MaxBytes {
			return nil, fmt.Errorf("over %d bytes: an HTML page is kept as it is, so trim it", MaxBytes)
		}
		return b, nil
	}
	if len(b) > ForwardSafe {
		c, _, err := Clean(b)
		return c, err
	}
	return b, nil
}
