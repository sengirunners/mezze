package main

import (
	"runtime/debug"

	"github.com/sengirunners/potluck/kit/api"
)

// unstampedCommit is potluck kit/api's Commit when no -X set it.
const unstampedCommit = "unknown"

// buildStamped fills api.Version and api.Commit from the Go toolchain's VCS
// stamp when the link set neither. Version and Commit
// moved to potluck's kit/api, so a zm whose updater still stamps
// -X <the old module path>/internal/api.* (every machine on a build from before
// the move) links a new tree with no stamp at all; without this the new
// daemon reports "unknown" and its own updater then refuses to run. A clean
// checkout's revision gives the same values that updater meant to stamp:
// main-<sha12> and the full sha. A modified tree or no VCS stamp leaves the
// defaults. rootCmd's Version reads it through versionLine, so it is set
// before any package-level use in this binary.
var buildStamped = stampFromBuildInfo()

func stampFromBuildInfo() bool {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return false
	}
	v, c, ok := versionFromVCS(api.Commit, bi.Settings)
	if ok {
		api.Version, api.Commit = v, c
	}
	return ok
}

// versionFromVCS is the version and commit a build with commit (api.Commit
// as linked) and the VCS settings settings should report; ok is false when
// it keeps what the link set: a stamped commit, no revision, or a tree with
// local changes.
func versionFromVCS(commit string, settings []debug.BuildSetting) (version, rev string, ok bool) {
	if commit != unstampedCommit {
		return "", "", false
	}
	modified := ""
	for _, s := range settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if len(rev) < 12 || modified != "false" {
		return "", "", false
	}
	return "main-" + rev[:12], rev, true
}

// versionLine is zm --version's text, after buildStamped.
func versionLine() string {
	_ = buildStamped
	return api.Version + " (" + api.Commit + ", " + api.WireProtocol + ")"
}
