package session

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
)

func snapshotPreflightEnv(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
}

type snapshotPreflightFile struct {
	Mode os.FileMode
	Data string
	Link string
}

// Include the nested repository's metadata, excluding only the outer repo's
// .git directory. A refused preflight must not alter any working content.
func snapshotPreflightFiles(t *testing.T, root string) map[string]snapshotPreflightFile {
	t.Helper()
	out := map[string]snapshotPreflightFile{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == filepath.Join(root, ".git") {
			return filepath.SkipDir
		}
		if path == root {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		file := snapshotPreflightFile{Mode: info.Mode()}
		if info.Mode().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			file.Data = string(b)
		} else if info.Mode()&os.ModeSymlink != 0 {
			file.Link, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out[rel] = file
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSnapshotNestedReplacementPreflightPreservesIndex(t *testing.T) {
	for _, marker := range []string{"directory", "gitfile"} {
		t.Run(marker, func(t *testing.T) {
			snapshotPreflightEnv(t)
			repo := zrepo(t)
			for _, name := range []string{"target", "other", "staged"} {
				zw(t, filepath.Join(repo, name), "base\n")
			}
			zg(t, repo, "add", ".")
			zg(t, repo, "commit", "-qm", "base files")
			// Preserve an existing staged change, plus distinct unstaged data.
			zw(t, filepath.Join(repo, "staged"), "already staged\n")
			zg(t, repo, "add", "staged")
			zw(t, filepath.Join(repo, "staged"), "working change\n")
			zw(t, filepath.Join(repo, "other"), "other change\n")
			target := filepath.Join(repo, "target")
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
			if marker == "directory" {
				if err := os.Mkdir(target, 0700); err != nil {
					t.Fatal(err)
				}
				zg(t, target, "init", "-q", "-b", "nested")
				zg(t, target, "config", "user.name", "test")
				zg(t, target, "config", "user.email", "test@invalid")
				zw(t, filepath.Join(target, "inner"), "committed inner\n")
				zg(t, target, "add", "inner")
				zg(t, target, "commit", "-qm", "nested commit")
			} else {
				owner := zrepo(t)
				zw(t, filepath.Join(owner, "inner"), "committed inner\n")
				zg(t, owner, "add", "inner")
				zg(t, owner, "commit", "-qm", "nested commit")
				zg(t, owner, "worktree", "add", "-q", "-b", "nested", target)
			}
			zw(t, filepath.Join(target, "inner"), "dirty inner\n")
			zw(t, filepath.Join(target, "local-only"), "untracked inner\n")
			status := zg(t, repo, "status", "--porcelain", "--untracked-files=all", "--ignore-submodules=all")
			if !strings.Contains(status, "T target") {
				t.Fatalf("fixture did not expose a tracked type change: %q", status)
			}
			indexPath := filepath.Join(repo, ".git", "index")
			before, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			head := zg(t, repo, "rev-parse", "HEAD")
			files := snapshotPreflightFiles(t, repo)
			nestedHead := zg(t, target, "rev-parse", "HEAD")
			nestedIndexPath := zg(t, target, "rev-parse", "--git-path", "index")
			if !filepath.IsAbs(nestedIndexPath) {
				nestedIndexPath = filepath.Join(target, nestedIndexPath)
			}
			nestedIndex, err := os.ReadFile(nestedIndexPath)
			if err != nil {
				t.Fatal(err)
			}
			ok, err := snapshotTaskWorktree(context.Background(), row{Session: orchapi.Session{Worktree: repo, Branch: "main"}})
			if err == nil || ok || !strings.Contains(err.Error(), "nested repository") {
				t.Fatalf("nested replacement was not refused: ok=%v err=%v", ok, err)
			}
			after, err := os.ReadFile(indexPath)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("refusal changed preexisting index bytes: before=%d after=%d err=%v", len(before), len(after), err)
			}
			if got := zg(t, repo, "rev-parse", "HEAD"); got != head {
				t.Fatalf("refusal moved HEAD: %s -> %s", head, got)
			}
			if got := snapshotPreflightFiles(t, repo); !reflect.DeepEqual(files, got) {
				t.Fatal("refusal changed working or nested repository content")
			}
			if got, err := os.ReadFile(nestedIndexPath); err != nil || !bytes.Equal(nestedIndex, got) {
				t.Fatalf("refusal changed nested index: err=%v", err)
			}
			if got := zg(t, target, "rev-parse", "HEAD"); got != nestedHead {
				t.Fatalf("refusal moved nested HEAD: %s -> %s", nestedHead, got)
			}
			// Move the nested repository out intact. No index reset is needed
			// before retrying an ordinary replacement at the same path.
			backup := filepath.Join(t.TempDir(), "saved-nested")
			if err := os.Rename(target, backup); err != nil {
				t.Fatal(err)
			}
			zw(t, target, "ordinary replacement\n")
			if ok, err := snapshotTaskWorktree(context.Background(), row{Session: orchapi.Session{Worktree: repo, Branch: "main"}}); err != nil || !ok {
				t.Fatalf("retry after removing nested repo needs an index reset: ok=%v err=%v", ok, err)
			}
			for path, want := range map[string]string{"target": "ordinary replacement", "other": "other change", "staged": "working change"} {
				if got := zg(t, repo, "show", "HEAD:"+path); got != want {
					t.Fatalf("retry lost %s: got=%q want=%q", path, got, want)
				}
			}
			if got, err := os.ReadFile(filepath.Join(backup, "inner")); err != nil || string(got) != "dirty inner\n" {
				t.Fatalf("nested work was lost: %q err=%v", got, err)
			}
		})
	}
}

func TestSnapshotPreflightAllowsOrdinaryDirectoryReplacementAndDeletion(t *testing.T) {
	snapshotPreflightEnv(t)
	repo := zrepo(t)
	zw(t, filepath.Join(repo, "target"), "base\n")
	zw(t, filepath.Join(repo, "deleted"), "base\n")
	zg(t, repo, "add", ".")
	zg(t, repo, "commit", "-qm", "base files")
	for _, path := range []string{"target", "deleted"} {
		if err := os.Remove(filepath.Join(repo, path)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(repo, "target"), 0700); err != nil {
		t.Fatal(err)
	}
	zw(t, filepath.Join(repo, "target", "leaf"), "ordinary directory\n")
	if ok, err := snapshotTaskWorktree(context.Background(), row{Session: orchapi.Session{Worktree: repo, Branch: "main"}}); err != nil || !ok {
		t.Fatalf("ordinary directory or deletion was refused: ok=%v err=%v", ok, err)
	}
	if got := zg(t, repo, "show", "HEAD:target/leaf"); got != "ordinary directory" {
		t.Fatalf("directory replacement lost: %q", got)
	}
	if got := zg(t, repo, "ls-tree", "HEAD", "deleted"); got != "" {
		t.Fatalf("deleted path remained: %q", got)
	}
}
