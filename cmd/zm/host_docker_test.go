package main

import (
	"context"
	"testing"

	"github.com/spf13/cobra"
)

// --docker is tri-state for a re-run: given, it decides;
// --docker=false turns it off; left out, hostprobe keeps an earlier share's.
func TestShareDockerFlag(t *testing.T) {
	for _, c := range []struct {
		args        []string
		docker, set bool
	}{{nil, false, false}, {[]string{"--docker"}, true, true}, {[]string{"--docker=false"}, false, true}} {
		t.Setenv("ZM_HOME", t.TempDir())
		t.Setenv("ZM_NO_AUTOSTART", "1")
		var sf shareFlags
		cmd := &cobra.Command{Use: "share"}
		sf.add(cmd, true)
		if err := cmd.ParseFlags(append([]string{"--name-prefix", "h", "--dry-run"}, c.args...)); err != nil {
			t.Fatal(err)
		}
		cmd.SetContext(context.Background())
		o, err := sf.options(cmd)
		if err != nil || o.Docker != c.docker || o.DockerSet != c.set {
			t.Errorf("%v: docker %v set %v (%v), want %v %v", c.args, o.Docker, o.DockerSet, err, c.docker, c.set)
		}
	}
}

// --root is tri-state like --docker: given, it decides;
// --root=false turns it off; left out, hostprobe keeps an earlier share's.
func TestShareRootFlag(t *testing.T) {
	for _, c := range []struct {
		args      []string
		root, set bool
	}{{nil, false, false}, {[]string{"--root"}, true, true}, {[]string{"--root=false"}, false, true}} {
		t.Setenv("ZM_HOME", t.TempDir())
		t.Setenv("ZM_NO_AUTOSTART", "1")
		var sf shareFlags
		cmd := &cobra.Command{Use: "share"}
		sf.add(cmd, true)
		if err := cmd.ParseFlags(append([]string{"--name-prefix", "h", "--dry-run"}, c.args...)); err != nil {
			t.Fatal(err)
		}
		cmd.SetContext(context.Background())
		o, err := sf.options(cmd)
		if err != nil || o.Root != c.root || o.RootSet != c.set {
			t.Errorf("%v: root %v set %v (%v), want %v %v", c.args, o.Root, o.RootSet, err, c.root, c.set)
		}
	}
}
