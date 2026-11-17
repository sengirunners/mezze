package toolstest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/sengirunners/mezze/internal/tools"
)

func tarball(t *testing.T, member string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "./" + member, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	tw.Write(body)
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// TestProvisionDownloadsOnce: the real tests of a package (and, with
// ZM_TEST_TOOLS, of every package) share one download of each tool.
func TestProvisionDownloadsOnce(t *testing.T) {
	archives := map[string][]byte{
		tools.Rclone:      tarball(t, "rclone", []byte("#!/bin/sh\necho rclone\n")),
		tools.Cloudflared: tarball(t, "cloudflared", []byte("#!/bin/sh\necho cloudflared\n")),
	}
	var mu sync.Mutex
	hits := map[string]int{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tool := strings.TrimPrefix(r.URL.Path, "/")
		mu.Lock()
		hits[tool]++
		mu.Unlock()
		w.Write(archives[tool])
	}))
	defer srv.Close()
	old := tools.Pins
	t.Cleanup(func() { tools.Pins = old })
	tools.Pins = append([]tools.Pin(nil), old...)
	for i, p := range tools.Pins {
		if p.OS == runtime.GOOS && p.Arch == runtime.GOARCH {
			h := sha256.Sum256(archives[p.Tool])
			tools.Pins[i].URL, tools.Pins[i].SHA256, tools.Pins[i].Member = srv.URL+"/"+p.Tool, hex.EncodeToString(h[:]), p.Tool
		}
	}
	t.Setenv(EnvCache, t.TempDir())
	for range 3 {
		home := t.TempDir()
		if err := ProvisionTools(context.Background(), home, tools.Options{Client: srv.Client()}, tools.Rclone, tools.Cloudflared); err != nil {
			t.Fatal(err)
		}
		for _, tool := range []string{tools.Rclone, tools.Cloudflared} {
			if _, err := tools.Verify(home, tool); err != nil {
				t.Fatalf("%s in a test home: %v", tool, err)
			}
		}
	}
	if hits[tools.Rclone] != 1 || hits[tools.Cloudflared] != 1 {
		t.Fatalf("downloads %v, want one of each", hits)
	}
}
