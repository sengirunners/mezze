package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

// The daemon runs a setup undo only for a directory strictly inside the
// tracker's own directory that the call made: never one that existed before,
// never through a symlink to elsewhere, never the directory itself.
func TestSetupUndoContained(t *testing.T) {
	root := filepath.Join(t.TempDir(), "trackers", "01PROJECT", "bd")
	old := filepath.Join(root, "old")
	if err := os.MkdirAll(filepath.Join(old, "a", "b", "c"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	before := Snapshot(root)
	// What the call made.
	made := filepath.Join(root, ".beads")
	nested := filepath.Join(old, "new")
	for _, d := range []string{made, nested} {
		os.MkdirAll(d, 0o700)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(old, "a", "b", "c", "d")
	os.MkdirAll(deep, 0o700)
	steps := func(paths ...string) []orchapi.AdapterUndo {
		var out []orchapi.AdapterUndo
		for _, p := range paths {
			out = append(out, orchapi.AdapterUndo{Op: orchapi.UndoRemoveDir, Path: p})
		}
		return out
	}
	for _, c := range []struct {
		u    orchapi.AdapterUndo
		want bool // removable
	}{
		{orchapi.AdapterUndo{Op: orchapi.UndoRemoveDir, Path: made}, true},
		{orchapi.AdapterUndo{Op: orchapi.UndoRemoveDir, Path: nested}, true},
		{orchapi.AdapterUndo{Op: orchapi.UndoRemoveDir, Path: old}, false},                             // existed before
		{orchapi.AdapterUndo{Op: orchapi.UndoRemoveDir, Path: link}, false},                            // a symlink to outside
		{orchapi.AdapterUndo{Op: orchapi.UndoRemoveDir, Path: outside}, false},                         // outside
		{orchapi.AdapterUndo{Op: orchapi.UndoRemoveDir, Path: root}, false},                            // the root itself
		{orchapi.AdapterUndo{Op: orchapi.UndoRemoveDir, Path: filepath.Join(root, "..", "..")}, false}, // dot-dot
		{orchapi.AdapterUndo{Op: orchapi.UndoRemoveDir, Path: deep}, false},                            // too deep to tell
		{orchapi.AdapterUndo{Op: "drop_database", Path: made}, false},                                  // no such op
		{orchapi.AdapterUndo{Op: orchapi.UndoRemoveDir, Path: "relative/path"}, false},                 // not absolute
	} {
		p, err := undoTarget(root, before, c.u)
		if got := err == nil && p != ""; got != c.want {
			t.Errorf("%+v: removable %v (%q, %v), want %v", c.u, got, p, err, c.want)
		}
	}
	var refused int
	Undo(root, before, steps(made, nested, old, link, outside, root, deep), func(string, ...any) { refused++ })
	if refused != 5 {
		t.Fatalf("%d steps refused and logged, want 5", refused)
	}
	switch {
	case pathExists(made) || pathExists(nested):
		t.Fatal("what the call made was left")
	case !pathExists(old) || !pathExists(deep) || !pathExists(outside) || !pathExists(root):
		t.Fatal("undo removed something it must never touch")
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was followed or removed: %v", err)
	}
}

// Options sends exactly the declared flags: an undeclared flag, a
// --password-file the adapter does not take, and a --config key a declared
// option owns are refused.
func TestOptions(t *testing.T) {
	info := &orchapi.AdapterSetupInfo{Options: []string{orchapi.SetupOptMode, orchapi.SetupOptDir, orchapi.SetupOptServer}, Password: true}
	o, err := Options(info, "beads", orchapi.AddTrackerRequest{Mode: "new", Config: map[string]string{"bin": "/b"}})
	if err != nil || len(o) != 1 || o["mode"] != "new" {
		t.Fatalf("declared: %v %v", o, err)
	}
	if o, err := Options(info, "beads", orchapi.AddTrackerRequest{}); err != nil || o != nil {
		t.Fatalf("no flags: %v %v", o, err)
	}
	for _, c := range []struct {
		info *orchapi.AdapterSetupInfo
		req  orchapi.AddTrackerRequest
		want string
	}{
		{info, orchapi.AddTrackerRequest{Database: "d"}, "takes no --database"},
		{nil, orchapi.AddTrackerRequest{Server: "h:1"}, "takes no --server"},
		{&orchapi.AdapterSetupInfo{Options: info.Options}, orchapi.AddTrackerRequest{PasswordFile: "/pw"}, "takes no --password-file"},
		{info, orchapi.AddTrackerRequest{Config: map[string]string{"dir": "/elsewhere"}}, "--config dir"},
	} {
		if _, err := Options(c.info, "beads", c.req); !api.IsCode(err, api.CodeBadRequest) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v, want %q", c.req, err, c.want)
		}
	}
}

func pathExists(p string) bool { _, err := os.Stat(p); return err == nil }
