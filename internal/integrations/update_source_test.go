package integrations

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The update source is one setting: publicRepo in cmd/zm/daemon_update.go.
// The compute plugin's release pin names it first (installer tries its repos
// first to last) and every plugin manifest names it. The private repo's pin
// may add one more source, its own releases; the public copy's pin
// (internal/deps/publiccopy) is the same pin without it. A drift here would
// leave a machine installing from one repo and updating from another.
func TestUpdateSourceOneSetting(t *testing.T) {
	src, err := os.ReadFile("../../cmd/zm/daemon_update.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`const publicRepo = "([^"]+)"`).FindSubmatch(src)
	if m == nil {
		t.Fatal("publicRepo not found in cmd/zm/daemon_update.go")
	}
	public := string(m[1])
	pinFile := filepath.Join(computeDir, "zm-release.pin")
	pin, err := os.ReadFile(pinFile)
	if err != nil {
		t.Fatal(err)
	}
	var repos, bases []string
	for _, l := range strings.Split(string(pin), "\n") {
		if v, ok := strings.CutPrefix(l, "repo="); ok {
			repos = append(repos, v)
		}
		if v, ok := strings.CutPrefix(l, "base_url="); ok {
			bases = append(bases, v)
		}
	}
	if len(repos) == 0 || len(repos) > 2 || repos[0] != public || len(bases) != len(repos) {
		t.Fatalf("pin repos %q bases %q: want %s first, at most one more", repos, bases, public)
	}
	for i, r := range repos {
		if bases[i] != "https://github.com/"+r+"/releases/download" {
			t.Errorf("base_url %q for %s", bases[i], r)
		}
	}
	if over, err := os.ReadFile(filepath.Join("../../internal/deps/publiccopy", "integrations", "claude-code-compute", "zm-release.pin")); err == nil {
		// The same sources less the second, in the same order. The release lines are the pin's or blank: a blank
		// tag says the public copy pins no release (it cannot name its own), never a private repo's release.
		want := pinSettings(string(pin), repos[1:])
		got := pinSettings(string(over), nil)
		if got["tag"] == "" && (got["version"] != "" || got["sums_sha256"] != "") {
			t.Error("the public copy's zm-release.pin has a version or hash but no tag")
		}
		if tag := got["tag"]; tag != "" && !regexp.MustCompile(`^v\d+\.\d+\.\d+$`).MatchString(tag) {
			t.Errorf("the public copy's zm-release.pin names %q: a rolling release is pruned, and the private repo's cannot be downloaded; use a kept vMAJOR.MINOR.PATCH release or none", tag)
		}
		for _, k := range []string{"tag", "version", "sums_sha256"} {
			if got[k] != "" && got[k] != want[k] {
				t.Errorf("the public copy's zm-release.pin has %s=%q, want the pin's %q or blank", k, got[k], want[k])
			}
			delete(got, k)
			delete(want, k)
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("the public copy's zm-release.pin is not %s without its second source:\n%s", pinFile, over)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}

	for _, p := range []string{
		"../../integrations/claude-code/.claude-plugin/plugin.json",
		filepath.Join(computeDir, ".claude-plugin", "plugin.json"),
		filepath.Join(computeDir, ".codex-plugin", "plugin.json"),
	} {
		var man struct {
			Homepage   string `json:"homepage"`
			Repository string `json:"repository"`
		}
		readJSON(t, p, &man)
		want := "https://github.com/" + public
		if man.Homepage != want || man.Repository != want {
			t.Errorf("%s: homepage %q repository %q, want %q", p, man.Homepage, man.Repository, want)
		}
	}
}

// pinSettings reads a zm-release.pin's lines (comments aside) as settings; the repo= and base_url= lines of the
// repos in drop are left out, and the rest keep their order in one "sources" entry.
func pinSettings(pin string, drop []string) map[string]string {
	out := map[string]string{}
	skip := false
	var sources []string
	for _, l := range strings.Split(pin, "\n") {
		k, v, ok := strings.Cut(l, "=")
		if !ok || strings.HasPrefix(l, "#") {
			continue
		}
		switch k {
		case "repo":
			skip = slices.Contains(drop, v)
			fallthrough
		case "base_url":
			if !skip {
				sources = append(sources, l)
			}
		default:
			out[k] = v
		}
	}
	out["sources"] = strings.Join(sources, "\n")
	return out
}
