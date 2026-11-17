package approvals

import (
	"bytes"
	"embed"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"strings"
	"sync"

	"github.com/sengirunners/potluck/kit/api"
)

// The Decisions page is installable (an iPhone's Add to Home Screen): a
// manifest, an icon and its two scripts, served on the device routes beside
// the page. web/zm-decisions.js is generated from t3code's zm-client
// (scripts/sync-zm-client.sh); web/app.js renders with it.

//go:embed web/app.js web/zm-decisions.js web/sw.js
var webFiles embed.FS

const manifest = `{"name":"zm Decisions","short_name":"Decisions","start_url":"/v0/approvals","scope":"/v0/",` +
	`"display":"standalone","background_color":"#151515","theme_color":"#151515",` +
	`"icons":[{"src":"/v0/approvals/icon.png","sizes":"512x512","type":"image/png","purpose":"any"}]}`

func (s *Service) handleAsset(w http.ResponseWriter, r *http.Request) {
	if _, err := s.device(r); err != nil {
		api.WriteError(w, err)
		return
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-cache")
	var body []byte
	switch name := r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]; name {
	case "sw.js":
		body, _ = webFiles.ReadFile("web/sw.js")
		h.Set("Content-Type", "text/javascript; charset=utf-8")
	case "app.js", "zm-decisions.js":
		body, _ = webFiles.ReadFile("web/" + name)
		h.Set("Content-Type", "text/javascript; charset=utf-8")
	case "manifest.webmanifest":
		body = []byte(manifest)
		h.Set("Content-Type", "application/manifest+json")
	case "icon.png":
		body = iconPNG()
		h.Set("Content-Type", "image/png")
	default:
		http.NotFound(w, r)
		return
	}
	_, _ = w.Write(body)
}

var (
	iconOnce sync.Once
	iconData []byte
)

// iconPNG draws the home-screen icon: a check mark on a dark square (iOS
// rounds the corners itself).
func iconPNG() []byte {
	iconOnce.Do(func() {
		const n = 512
		img := image.NewRGBA(image.Rect(0, 0, n, n))
		bg, fg := color.RGBA{0x15, 0x15, 0x15, 0xff}, color.RGBA{0x4c, 0xc0, 0x72, 0xff}
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				img.Set(x, y, bg)
			}
		}
		// Two strokes: (150,270)->(225,345) and (225,345)->(370,180), 36 px wide.
		stroke := func(x0, y0, x1, y1 float64) {
			for t := 0.0; t <= 1; t += 0.001 {
				cx, cy := x0+(x1-x0)*t, y0+(y1-y0)*t
				for dy := -18.0; dy <= 18; dy++ {
					for dx := -18.0; dx <= 18; dx++ {
						if dx*dx+dy*dy <= 18*18 {
							img.Set(int(cx+dx), int(cy+dy), fg)
						}
					}
				}
			}
		}
		stroke(150, 270, 225, 345)
		stroke(225, 345, 370, 180)
		var b bytes.Buffer
		_ = png.Encode(&b, img)
		iconData = b.Bytes()
	})
	return iconData
}

// httpsURL is where the page should be opened instead, when it was asked
// for over plain HTTP and this node serves HTTPS on its MagicDNS name: an
// iPhone installs and keeps a page only from a secure origin. "" otherwise.
func (s *Service) httpsURL(r *http.Request) string {
	if r.TLS != nil || s.d.Net == nil {
		return ""
	}
	self, err := s.d.Net.Self(r.Context())
	if err != nil || !self.HTTPS || self.DNSName == "" {
		return ""
	}
	u := "https://" + self.DNSName + "/v0/approvals"
	if c := normCode(r.URL.Query().Get("code")); c != "" && !strings.ContainsFunc(c, func(r rune) bool { return !strings.ContainsRune(codeAlphabet, r) }) && len(c) <= 16 {
		u += "?code=" + c
	}
	return u
}
