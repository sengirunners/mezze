package main

// --deps, --deps-wait and --deps-scripts on zm sandbox create and zm job run:
// a dependency layer of the holder's (Potluck
// docs/daemon.md, Dependency layers). The flags, the acquire's deps, the
// answer's line and the mismatch warning are Potluck's depscli, as potluck's
// own CLI has them; the daemon checks everything again. What is zm's alone
// is below.

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/sengirunners/potluck/computeapi"
	"github.com/sengirunners/potluck/computeclient"
	"github.com/sengirunners/potluck/depscli"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/runtarget"
)

// depsUnder is dir's place in its repository ("" its top) when a layer of
// deps is mounted at depscli.DefaultPath there: zm job run runs the command
// in that directory, beside the layer, not at the checkout's top.
func depsUnder(ctx context.Context, dir, dest string, deps []computeapi.BoxDeps) string {
	if len(deps) == 0 || strings.HasPrefix(dest, "/") {
		return ""
	}
	def := depscli.DefaultPath(ctx, dir, dest)
	top := strings.TrimSuffix(dest, "/") + "/"
	if !strings.HasPrefix(def, top) {
		return ""
	}
	for _, d := range deps {
		if d.Path == def {
			return strings.TrimPrefix(def, top)
		}
	}
	return ""
}

// depsImageOK refuses an image name with deps on a binding another machine
// serves before zm sandbox create builds its bundle: the daemon keys such a
// layer by the pin its host advertises for a template, so it takes a
// template, never an image name (Potluck #152; the daemon refuses it too,
// which is all zm job run needs, its bundle made before the box is placed).
// A binding another machine serves is the one with a host_binding.
func depsImageOK(ctx context.Context, c *client.Client, target, image string, deps []computeapi.BoxDeps) error {
	if len(deps) == 0 || image == "" {
		return nil
	}
	v, err := computeclient.DescribeRunTarget(ctx, c, target)
	if err != nil || v.HostBinding == "" {
		return nil // the acquire says what is wrong
	}
	return fmt.Errorf("--deps on run target %s, which %s serves, takes a template (--template %s), not --image %s: the layer is keyed by the image the host advertises for it", v.Name, v.Host, strings.Join(computeapi.BoxTemplates(), "|"), image)
}

// depsTimeout is how long a retried acquire with deps may take beyond a
// plain one (Potluck's own bound): deps_wait_s, then the layers' builds.
func depsTimeout(deps []computeapi.BoxDeps, waitS *int) time.Duration {
	wait := computeapi.DepsWaitDefaultS
	if waitS != nil {
		wait = *waitS
	}
	return runtarget.DepsAnswerWithin(len(deps), wait)
}
