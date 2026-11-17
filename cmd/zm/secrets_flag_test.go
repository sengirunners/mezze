package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestReadSecrets: a regular file (bytes exact) or an environment variable
// becomes a secret; a literal value, a bad or value-looking name, an empty,
// missing, special or oversize source, or a repeat is refused, never with
// the value (or the bad name) in the error.
func TestReadSecrets(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "key")
	if err := os.WriteFile(f, []byte("file-secret-zzq\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(f, link); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"TOKEN": "env-secret-zzq"}
	getenv := func(k string) string { return env[k] }
	got, err := readSecrets([]string{"API_KEY=@" + f, "TOKEN", "VIA_LINK=@" + link}, getenv)
	if err != nil || got["API_KEY"] != "file-secret-zzq\n" || got["TOKEN"] != "env-secret-zzq" || got["VIA_LINK"] != "file-secret-zzq\n" || len(got) != 3 {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := readSecrets(nil, nil); got != nil || err != nil {
		t.Fatalf("no flags: %v %v", got, err)
	}
	empty := filepath.Join(dir, "empty")
	big := filepath.Join(dir, "big")
	fifo := filepath.Join(dir, "fifo")
	_ = os.WriteFile(empty, nil, 0o600)
	_ = os.WriteFile(big, []byte(strings.Repeat("z", 64<<10+1)), 0o600)
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]string{
		{"API_KEY=literal-zzq"}, {"sk-live-zzq"}, {"=@" + f}, {"lower_zzq=@" + f}, {"MISSING"},
		{"A=@" + filepath.Join(dir, "nope")}, {"A=@" + empty}, {"A=@" + big}, {"A=@" + dir}, {"A=@" + fifo},
		{"TOKEN", "TOKEN"}, {"TOKENZZQ"},
	} {
		_, err := readSecrets(bad, getenv)
		if err == nil || strings.Contains(strings.ToLower(err.Error()), "zzq") {
			t.Errorf("%v: %v", bad, err)
		}
	}
}
