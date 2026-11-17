package main

import (
	"runtime/debug"
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

func TestVersionFromVCS(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	set := func(kv ...string) []debug.BuildSetting {
		var out []debug.BuildSetting
		for i := 0; i < len(kv); i += 2 {
			out = append(out, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return out
	}
	for _, c := range []struct {
		name, commit     string
		settings         []debug.BuildSetting
		ok               bool
		version, wantRev string
	}{
		{"clean tree, no stamp", unstampedCommit, set("vcs", "git", "vcs.revision", sha, "vcs.modified", "false"), true, "main-0123456789ab", sha},
		{"stamped by -X: the stamp wins", "abc1234", set("vcs.revision", sha, "vcs.modified", "false"), false, "", ""},
		{"local changes", unstampedCommit, set("vcs.revision", sha, "vcs.modified", "true"), false, "", ""},
		{"no modified flag", unstampedCommit, set("vcs.revision", sha), false, "", ""},
		{"no VCS stamp", unstampedCommit, nil, false, "", ""},
		{"short revision", unstampedCommit, set("vcs.revision", "0123", "vcs.modified", "false"), false, "", ""},
	} {
		v, r, ok := versionFromVCS(c.commit, c.settings)
		if ok != c.ok || v != c.version || r != c.wantRev {
			t.Errorf("%s: %q %q %v, want %q %q %v", c.name, v, r, ok, c.version, c.wantRev, c.ok)
		}
	}
}

// go test links no -X and no VCS stamp, so api.Commit is still kit/api's
// default here: unstampedCommit must be that default, or the fallback never
// fires.
func TestUnstampedCommitIsTheDefault(t *testing.T) {
	if buildStamped {
		t.Skip("this test binary carries a VCS stamp")
	}
	if api.Commit != unstampedCommit {
		t.Fatalf("api.Commit unstamped is %q, unstampedCommit is %q", api.Commit, unstampedCommit)
	}
	if got := versionLine(); got != api.Version+" ("+unstampedCommit+", "+api.WireProtocol+")" {
		t.Fatalf("versionLine %q", got)
	}
}
