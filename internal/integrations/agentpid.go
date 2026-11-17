package integrations

import (
	"os"
	"path/filepath"
	"strings"
)

// maxAgentDepth bounds the walk from a hook up to its agent (shells and
// wrappers such as env, timeout, nice, direnv or mise exec, npx between).
const maxAgentDepth = 8

// AgentPID is the pid of the agent a hook runs under: the nearest ancestor
// of the hook that looks like agent (looksLikeAgent), past any shells and
// wrappers. 0 when none does, or when the hook was orphaned (Codex runs
// notify without waiting, so the hook may already belong to init, launchd
// or a subreaper): the daemon then falls back to expiring a quiet session
// instead of watching the wrong process. It reads the process table
// directly (/proc, sysctl): no ps, inside the hook budget.
func AgentPID(agent string) int {
	pid, _ := NearestAgent(agent)
	return pid
}

// NearestAgent is the nearest ancestor that looks like any of agents, and
// which one (AgentPID's walk); 0 and "" when none does. With several kinds
// it tells which agent this process really runs under when one agent
// launched another (Codex from Claude's shell inherits Claude's variables).
func NearestAgent(agents ...string) (int, string) {
	var kinds []string
	for _, a := range agents {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			kinds = append(kinds, a)
		}
	}
	pid := os.Getppid()
	for range maxAgentDepth {
		if pid <= 1 || len(kinds) == 0 {
			return 0, ""
		}
		ppid, comm, ok := parentOf(pid)
		if !ok {
			return 0, ""
		}
		for _, a := range kinds {
			if looksLikeAgent(a, comm, "", nil) {
				return pid, a
			}
		}
		exe, argv := procArgs(pid)
		for _, a := range kinds {
			if looksLikeAgent(a, comm, exe, argv) {
				return pid, a
			}
		}
		pid = ppid
	}
	return 0, ""
}

// agentPackages name each JavaScript agent's package, as it shows in the
// argv of the node or bun running it.
var agentPackages = map[string][]string{
	"claude":   {"claude-code"},
	"gemini":   {"gemini-cli"},
	"codex":    {"@openai/codex"},
	"opencode": {"opencode"},
}

// looksLikeAgent reports whether a process (command name comm, executable
// exe, argv) can be agent's runtime: its own binary by name (codex*,
// claude*, ...; comm, exe or argv[0]), a versioned install of it
// (~/.local/share/claude/versions/2.1.316, whose comm is the version), or
// node or bun whose script (its first argument that is not a flag) is the
// agent's package or bin. Any other node or bun (npm run, another tool) is
// not the agent.
func looksLikeAgent(agent, comm, exe string, argv []string) bool {
	names := []string{comm, exe}
	if len(argv) > 0 {
		names = append(names, argv[0])
	}
	runtime := false
	for _, n := range names {
		if n == "" {
			continue
		}
		b := strings.ToLower(strings.TrimPrefix(filepath.Base(n), "-"))
		if strings.HasPrefix(b, agent) || strings.Contains(strings.ToLower(n), "/"+agent+"/versions/") {
			return true
		}
		runtime = runtime || b == "node" || b == "bun"
	}
	if !runtime || len(argv) < 2 {
		return false
	}
	for _, a := range argv[1:] {
		if strings.HasPrefix(a, "-") {
			continue // the runtime's own flags
		}
		a = strings.ToLower(a) // the script it runs
		if strings.HasPrefix(filepath.Base(a), agent) {
			return true
		}
		for _, p := range agentPackages[agent] {
			if strings.Contains(a, p) {
				return true
			}
		}
		return false
	}
	return false
}
