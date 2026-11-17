package integrations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/mcp"
)

// The compute plugin (integrations/claude-code-compute) serves the sandbox
// tools through `zm mcp --sandboxes`, installing zm first when the user
// allowed it; the default plugin stays as it is.

func readJSON(t *testing.T, p string, v any) {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", p, err)
	}
}

type pluginMCP struct {
	Command      string            `json:"command"`
	Args         []string          `json:"args"`
	Env          map[string]string `json:"env"`
	EnabledTools []string          `json:"enabled_tools"`
}

func TestComputePluginManifest(t *testing.T) {
	var man struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		UserConfig map[string]struct {
			Type    string `json:"type"`
			Default any    `json:"default"`
		} `json:"userConfig"`
	}
	readJSON(t, filepath.Join(computeDir, ".claude-plugin", "plugin.json"), &man)
	if man.Name != "zm-compute" || man.Version == "" {
		t.Errorf("plugin.json name %q version %q", man.Name, man.Version)
	}
	// Installing needs the user's yes: the option defaults to off.
	if o := man.UserConfig["install_zm"]; o.Type != "boolean" || o.Default != false {
		t.Errorf("userConfig.install_zm = %+v, want a boolean defaulting to false", o)
	}

	var f struct {
		MCP map[string]pluginMCP `json:"mcpServers"`
	}
	readJSON(t, filepath.Join(computeDir, ".mcp.json"), &f)
	e, ok := f.MCP["zm"]
	if len(f.MCP) != 1 || !ok {
		t.Fatalf("compute .mcp.json servers %v, want only zm", f.MCP)
	}
	if e.Command != "${CLAUDE_PLUGIN_ROOT}/scripts/zm-run" || !slices.Equal(e.Args, []string{"mcp", "--sandboxes"}) {
		t.Errorf("compute MCP entry runs %q %q, want ${CLAUDE_PLUGIN_ROOT}/scripts/zm-run mcp --sandboxes", e.Command, e.Args)
	}
	if e.Env["ZM_COMPUTE_INSTALL"] != "${user_config.install_zm}" {
		t.Errorf("compute MCP env %v does not pass the install_zm option", e.Env)
	}
	for _, s := range []string{"zm-run", "zm-install"} {
		if fi, err := os.Stat(filepath.Join(computeDir, "scripts", s)); err != nil || fi.Mode()&0o111 == 0 {
			t.Errorf("scripts/%s is not an executable file (%v)", s, err)
		}
	}

	// Codex reads .codex-plugin/plugin.json before .claude-plugin/plugin.json and does
	// not substitute ${CLAUDE_PLUGIN_ROOT} in MCP commands: it runs zm from PATH and
	// lists only the sandbox tools.
	var cx struct {
		Name       string `json:"name"`
		Version    string `json:"version"`
		MCPServers string `json:"mcpServers"`
	}
	readJSON(t, filepath.Join(computeDir, ".codex-plugin", "plugin.json"), &cx)
	if cx.Name != man.Name || cx.Version != man.Version || !strings.HasPrefix(cx.MCPServers, "./") {
		t.Errorf("codex plugin.json %+v, want name %q version %q and a ./ mcpServers file", cx, man.Name, man.Version)
	}
	var cf struct {
		MCP map[string]pluginMCP `json:"mcpServers"`
	}
	readJSON(t, filepath.Join(computeDir, cx.MCPServers), &cf)
	var sandbox []string
	for _, tool := range mcp.SandboxTools() {
		sandbox = append(sandbox, tool.Name)
	}
	if ce := cf.MCP["zm"]; len(cf.MCP) != 1 || !isZmMCP(mustJSON(t, ce)) || !slices.Equal(ce.Args, []string{"mcp", "--sandboxes"}) ||
		!slices.Equal(ce.EnabledTools, sandbox) {
		t.Errorf("codex MCP servers %+v, want zm mcp --sandboxes with enabled_tools %v", cf.MCP, sandbox)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The default plugin's surface is unchanged: plain `zm mcp` (no sandbox
// tools), no install step.
func TestDefaultPluginHasNoSandboxes(t *testing.T) {
	var f struct {
		MCP map[string]pluginMCP `json:"mcpServers"`
	}
	readJSON(t, "../../integrations/claude-code/.mcp.json", &f)
	if e := f.MCP["zm"]; len(f.MCP) != 1 || e.Command != "zm" || !slices.Equal(e.Args, []string{"mcp"}) || len(e.Env) != 0 {
		t.Errorf("default plugin MCP changed: %+v", f.MCP)
	}
	var man map[string]any
	readJSON(t, "../../integrations/claude-code/.claude-plugin/plugin.json", &man)
	if _, ok := man["userConfig"]; ok {
		t.Error("default plugin grew a userConfig")
	}
}

// Both marketplaces (the repo root, for `marketplace add sengirunners/mezze`,
// and integrations/, for a clone) list the same plugins at the same folders.
func TestMarketplacesListComputePlugin(t *testing.T) {
	type entry struct {
		Name   string `json:"name"`
		Source string `json:"source"`
	}
	list := func(root string) map[string]string {
		var m struct {
			Name    string  `json:"name"`
			Plugins []entry `json:"plugins"`
		}
		readJSON(t, filepath.Join(root, ".claude-plugin", "marketplace.json"), &m)
		if m.Name != "zm" {
			t.Errorf("%s marketplace name %q", root, m.Name)
		}
		got := map[string]string{}
		for _, p := range m.Plugins {
			abs, err := filepath.Abs(filepath.Join(root, p.Source))
			if err != nil || !strings.HasPrefix(p.Source, "./") {
				t.Errorf("%s: source %q", root, p.Source)
			}
			got[p.Name] = abs
		}
		return got
	}
	repo, integ := list("../.."), list("../../integrations")
	want, _ := filepath.Abs(computeDir)
	for name, m := range map[string]map[string]string{"root": repo, "integrations": integ} {
		if m["zm-compute"] != want || m["zm"] == "" || len(m) != 2 {
			t.Errorf("%s marketplace lists %v, want zm and zm-compute (%s)", name, m, want)
		}
	}
	if repo["zm"] != integ["zm"] {
		t.Errorf("the marketplaces disagree on zm: %s vs %s", repo["zm"], integ["zm"])
	}
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"claude plugin marketplace add sengirunners/mezze", "zm-compute@zm", "[mcp_servers.zm-compute]",
		"reconnect\nit in `/mcp`", `command = "/Users/you/.local/bin/zm"`} {
		if !strings.Contains(string(readme), s) {
			t.Errorf("README lacks %q", s)
		}
	}
}

// zm-run, the MCP server command: stdout is the protocol, so only zm writes
// there; it installs only with the user's yes, and starts the local daemon.
func TestComputeRun(t *testing.T) {
	path := sysPath(t)
	r := newFakeRelease(t)
	srv := r.serve(t)
	scripts := pluginCopy(t, srv.URL, r.tag, r.version, r.sumsSHA)
	run := filepath.Join(scripts, "zm-run")

	t.Run("missing zm, install not allowed", func(t *testing.T) {
		home := t.TempDir()
		res := runScript(t, home, path, []string{"ZM_COMPUTE_INSTALL=false"}, run, "mcp", "--sandboxes")
		if res.err == nil || res.stdout != "" || !strings.Contains(res.stderr, "Install zm if it is missing") {
			t.Fatalf("err=%v stdout=%q stderr:\n%s", res.err, res.stdout, res.stderr)
		}
		if n := len(r.requests()); n != 0 {
			t.Errorf("downloaded without the user's yes: %v", r.requests())
		}
	})
	t.Run("missing zm, install allowed", func(t *testing.T) {
		home := t.TempDir()
		log := filepath.Join(home, "zm.log")
		res := runScript(t, home, path, []string{"ZM_COMPUTE_INSTALL=true", "ZM_FAKE_LOG=" + log}, run, "mcp", "--sandboxes")
		if res.err != nil {
			t.Fatalf("%v\n%s", res.err, res.stderr)
		}
		if res.stdout != "MCP mcp --sandboxes\n" {
			t.Errorf("stdout %q: only zm mcp may write there", res.stdout)
		}
		b, _ := os.ReadFile(log)
		if got, want := string(b), "--version\nstatus\ndaemon --detach\nmcp --sandboxes\n"; got != want {
			t.Errorf("zm ran:\n%s\nwant:\n%s", got, want)
		}
	})
	t.Run("present zm, remote host", func(t *testing.T) {
		home, bin := t.TempDir(), t.TempDir()
		if err := os.WriteFile(filepath.Join(bin, "zm"), []byte(fakeZm), 0o755); err != nil {
			t.Fatal(err)
		}
		log := filepath.Join(home, "zm.log")
		before := len(r.requests())
		res := runScript(t, home, bin+":"+path, []string{"ZM_HOST=studio", "ZM_FAKE_LOG=" + log}, run, "mcp", "--sandboxes")
		if res.err != nil || res.stdout != "MCP mcp --sandboxes\n" {
			t.Fatalf("err=%v stdout=%q\n%s", res.err, res.stdout, res.stderr)
		}
		if b, _ := os.ReadFile(log); string(b) != "mcp --sandboxes\n" {
			t.Errorf("with ZM_HOST zm ran %q, want only mcp (no local daemon)", b)
		}
		if len(r.requests()) != before {
			t.Error("downloaded although zm is present")
		}
	})
}
