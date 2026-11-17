package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --attach needs --work, a readable image or HTML page and stdin at most
// once; each is refused before anything is read or sent.
func TestAskAttachRefusals(t *testing.T) {
	dir := t.TempDir()
	page := filepath.Join(dir, "mock.html")
	svg := filepath.Join(dir, "x.svg")
	if err := os.WriteFile(page, []byte("<!doctype html><p>hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(svg, []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"new", "q?", "--attach", page}, "--work"},
		{[]string{"new", "q?", "--repo", "a/b", "--pr", "1", "--attach", page}, "--work"},
		{[]string{"new", "q?", "--work", "bd:zm-1", "--attach", svg}, "not a PNG, JPEG or WebP image or a UTF-8 HTML page"},
		{[]string{"new", "-", "--work", "bd:zm-1", "--attach", "-"}, `stdin ("-") is read once`},
		{[]string{"new", "q?", "--work", "bd:zm-1", "--attach", "-", "--attach", "-"}, `stdin ("-") is read once`},
	} {
		cmd := askCmd()
		cmd.SetArgs(c.args)
		cmd.SetErr(&bytes.Buffer{})
		cmd.SetOut(&bytes.Buffer{})
		if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%v: %v", c.args, err)
		}
	}
}
