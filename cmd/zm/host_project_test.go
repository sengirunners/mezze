package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/tailnet/tailnettest"
	"github.com/sengirunners/potluck/kit/teamlink"
)

// Zm host share binds into the project of the directory it
// runs in; with none it refuses before conformance and names -P, and a
// project name from $ZM_PROJECT is called out as coming from there (never
// read as a built-in default).
func TestShareProject(t *testing.T) {
	ctx := context.Background()
	dm, _ := poolDaemon(t, tailnettest.New(), "mini", "ana@example.com")
	c := client.NewUnix(dm.Deps.Config.SocketPath()) // the socket: only it carries the directory
	base, _ := filepath.EvalSymlinks(t.TempDir())

	empty := *c
	empty.Project, empty.ProjectFrom, empty.Cwd = "", "", filepath.Join(base, "none")
	err := shareProject(ctx, &empty, io.Discard)
	var e *api.Error
	if !errors.As(err, &e) || e.Code != api.CodeBadRequest || !strings.Contains(e.Hint, "-P") || !strings.Contains(e.Hint, "zm init") {
		t.Fatalf("no project: want a bad_request naming -P and zm init, got %v", err)
	}

	env := empty
	env.Project, env.ProjectFrom = "harbor", "env"
	err = shareProject(ctx, &env, io.Discard)
	if !errors.As(err, &e) || e.Code != api.CodeNotFound || !strings.Contains(e.Message, "$ZM_PROJECT") || !strings.Contains(e.Message, `"harbor"`) {
		t.Fatalf("ZM_PROJECT naming a missing project: want not_found naming $ZM_PROJECT, got %v", err)
	}

	dir := filepath.Join(base, "friend")
	in := empty
	in.Cwd = dir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runInit(ctx, &in, dir, "friend", false); err != nil {
		t.Fatal(err)
	}
	var said bytes.Buffer
	if err := shareProject(ctx, &in, &said); err != nil || !strings.Contains(said.String(), "binding into friend (from this directory)") {
		t.Fatalf("in a zm init directory: want its project said, got %v %q", err, said.String())
	}

	area := in
	area.Project, area.ProjectFrom = "friend/nope", "flag"
	err = shareProject(ctx, &area, io.Discard)
	if !errors.As(err, &e) || e.Code != api.CodeNotFound || !strings.Contains(e.Message, "no area nope in friend") || !strings.Contains(e.Message, "-P") {
		t.Fatalf("-P naming a missing area: want the resolver's reason, got %v", err)
	}

	envOK := in
	envOK.Project, envOK.ProjectFrom, envOK.Cwd = "friend", "env", filepath.Join(base, "none")
	said.Reset()
	if err := shareProject(ctx, &envOK, &said); err != nil || !strings.Contains(said.String(), "binding into friend (from $ZM_PROJECT)") {
		t.Fatalf("$ZM_PROJECT naming a project: want it said, got %v %q", err, said.String())
	}
}

// A joined project named by -P or $ZM_PROJECT is not a project of this
// machine, yet its boxes are lent to its pool: share says --pool, as the
// bind would, not "no project".
func TestShareProjectJoined(t *testing.T) {
	old := teamlink.BeatEvery
	teamlink.BeatEvery = time.Hour
	t.Cleanup(func() { teamlink.BeatEvery = old })
	ctx := context.Background()
	tn := tailnettest.New()
	_, owner := poolDaemon(t, tn, "mini", "ivo@example.com")
	userDM, user := poolDaemon(t, tn, "laptop", "ana@example.com")
	var out bytes.Buffer
	if err := poolCreate(ctx, owner, "friends", poolInvites{Renters: []string{"ana"}}, &out); err != nil {
		t.Fatalf("create: %v\n%s", err, out.String())
	}
	if _, err := poolJoin(ctx, user, joinCode(joinRe.FindAllStringSubmatch(out.String(), -1)), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	base, _ := filepath.EvalSymlinks(t.TempDir())
	for _, from := range []string{"env", "flag"} {
		c := client.NewUnix(userDM.Deps.Config.SocketPath())
		c.Project, c.ProjectFrom, c.Cwd = "friends", from, base
		err := shareProject(ctx, c, io.Discard)
		var e *api.Error
		if !errors.As(err, &e) || !strings.Contains(e.Hint, "--pool friends") {
			t.Fatalf("%s naming a joined project: want --pool, got %v", from, err)
		}
	}
}
