package offsite

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/potluck/kit/config"
)

const projA = "projects/01JBQ8ZK3Y2XW7Q9M4N5P6R7S8/project.toml"

func stagedFiles(t *testing.T, stage string, paths ...string) []File {
	t.Helper()
	var fs []File
	for _, p := range paths {
		full := filepath.Join(stage, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		fs = append(fs, File{Path: p})
	}
	return fs
}

func TestRestoringMarkerNotAHomeFile(t *testing.T) {
	if HomeFile(config.FileRestoring) || HomeFile(config.FileRestored) {
		t.Fatal("a marker file is one a set may hold")
	}
}

func TestInstallSyncsDeepestFirst(t *testing.T) {
	dst := t.TempDir()
	files := stagedFiles(t, filepath.Join(dst, ".restore"), "registry.db", projA)
	var synced []string
	ops := installOps{os.Rename, func(d string) error { synced = append(synced, d); return nil }}
	if err := install(files, filepath.Join(dst, ".restore"), dst, ops); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(dst, "projects/01JBQ8ZK3Y2XW7Q9M4N5P6R7S8")
	want := []string{proj, filepath.Join(dst, "projects"), dst}
	if strings.Join(synced, "|") != strings.Join(want, "|") {
		t.Fatalf("synced %v, want %v", synced, want)
	}
}

func TestInstallFailureLeavesMarkerAndHomeRefused(t *testing.T) {
	for name, ops := range map[string]installOps{
		"rename": {func(from, to string) error {
			if strings.HasSuffix(to, "project.toml") {
				return errors.New("boom")
			}
			return os.Rename(from, to)
		}, func(string) error { return nil }},
		"sync": {os.Rename, func(string) error { return errors.New("boom") }},
	} {
		dst := t.TempDir()
		stage := filepath.Join(dst, ".restore")
		files := stagedFiles(t, stage, "registry.db", projA)
		if err := config.MarkRestoring(dst, "20261102T010000Z"); err != nil {
			t.Fatal(err)
		}
		if err := install(files, stage, dst, ops); err == nil {
			t.Fatalf("%s: install succeeded", name)
		}
		// The caller (Restore) clears the marker only after install succeeds.
		if err := config.CheckNotRestoring(dst); err == nil || !strings.Contains(err.Error(), "20261102T010000Z") {
			t.Fatalf("%s: home not refused: %v", name, err)
		}
	}
}
