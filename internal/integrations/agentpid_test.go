package integrations

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A hook sends its agent's pid (here no ancestor is codex: 0, the daemon's
// fallback), so the daemon can end the session when the agent exits
// without a session-end hook (Codex never sends one).
func TestHookSendsAgentPID(t *testing.T) {
	f := &fakeState{}
	runHook(t, HookInput{Agent: "codex", Event: HookNotify,
		Payload: []byte(`{"type":"agent-turn-complete","thread-id":"0199a1b2-c3d4","cwd":"/w"}`)}, nil, f)
	if len(f.pids) != 1 || f.pids[0] != AgentPID("codex") {
		t.Fatalf("pids %v, AgentPID %d", f.pids, AgentPID("codex"))
	}
}

func TestLooksLikeAgent(t *testing.T) {
	for _, c := range []struct {
		agent, comm, exe string
		argv             []string
		want             bool
	}{
		{"codex", "codex", "", nil, true},
		{"codex", "codex-aarch64-a", "", nil, true},
		{"codex", "/usr/local/bin/codex", "", nil, true},
		{"claude", "claude", "", nil, true},
		// Native Claude on macOS: the binary is a version, so comm is "2.1.316".
		{"claude", "2.1.316", "/Users/x/.local/share/claude/versions/2.1.316", []string{"/Users/x/.local/bin/claude"}, true},
		{"claude", "2.1.316", "/Users/x/.local/share/claude/versions/2.1.316", nil, true},
		{"claude", "2.1.316", "", []string{"claude", "--resume"}, true},
		// node or bun counts only when its script is the agent.
		{"claude", "node", "/usr/local/bin/node", []string{"node", "/usr/lib/node_modules/@anthropic-ai/claude-code/cli.js"}, true},
		{"claude", "node", "/usr/local/bin/node", []string{"node", "--no-warnings", "/opt/homebrew/bin/claude"}, true},
		{"claude", "node", "/usr/local/bin/node", []string{"node", "/usr/local/lib/node_modules/npm/bin/npm-cli.js", "run", "claude-thing"}, false},
		{"claude", "node", "", nil, false},
		{"gemini", "node", "", []string{"node", "/usr/lib/node_modules/@google/gemini-cli/dist/index.js"}, true},
		{"opencode", "bun", "", []string{"bun", "/x/opencode/src/index.ts"}, true},
		{"codex", "node", "", []string{"node", "/x/node_modules/@openai/codex/bin/codex.js"}, true},
		// Wrappers and shells are never the agent.
		{"claude", "env", "/usr/bin/env", []string{"env", "claude"}, false},
		{"codex", "timeout", "", []string{"timeout", "60", "codex"}, false},
		{"codex", "-zsh", "", nil, false},
		{"claude", "mise", "", []string{"mise", "exec", "--", "claude"}, false},
	} {
		if got := looksLikeAgent(c.agent, c.comm, c.exe, c.argv); got != c.want {
			t.Errorf("looksLikeAgent(%q, %q, %q, %q) = %v", c.agent, c.comm, c.exe, c.argv, got)
		}
	}
}

// procArgs reads this process's own executable and argv.
func TestProcArgsOfSelf(t *testing.T) {
	exe, argv := procArgs(os.Getpid())
	if exe == "" && argv == nil {
		t.Skip("no process arguments here")
	}
	if len(argv) == 0 || argv[0] != os.Args[0] {
		t.Fatalf("argv %q, want it to start with %q", argv, os.Args[0])
	}
}

// The test binary plays the agent: its command starts "integrations".
const testAgent = "integrations"

// child runs this test binary as a helper (mode in ZM_AGENTPID_CHILD) under
// shell (sh -c), and returns what it printed.
func child(t *testing.T, mode, shell string) string {
	t.Helper()
	bin := "'" + os.Args[0] + "' -test.run='^TestAgentPIDHelper$'"
	cmd := exec.Command("/bin/sh", "-c", strings.ReplaceAll(shell, "BIN", bin))
	cmd.Env = append(os.Environ(), "ZM_AGENTPID_CHILD="+mode)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("%s: %v %s", shell, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestAgentPIDHelper(t *testing.T) {
	switch os.Getenv("ZM_AGENTPID_CHILD") {
	case "walk":
		os.Stdout.WriteString(strconv.Itoa(AgentPID(testAgent)))
	case "orphan":
		// Wait to be orphaned: the sh that started us in the background exits.
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if _, comm, ok := parentOf(os.Getppid()); !ok || !strings.HasSuffix(comm, "sh") {
				break
			}
		}
		os.Stdout.WriteString(strconv.Itoa(AgentPID(testAgent)))
	default:
		return
	}
	os.Exit(0)
}

// AgentPID walks past shells and wrappers to the agent (here three shells
// deep; "; true" keeps each sh from exec-ing in place).
func TestAgentPIDFindsTheAgentPastShells(t *testing.T) {
	if _, _, ok := parentOf(os.Getpid()); !ok {
		t.Skip("no process table here")
	}
	if ppid, comm, ok := parentOf(os.Getpid()); !ok || ppid != os.Getppid() || comm == "" {
		t.Fatalf("parentOf(self) = %d %q %v, want %d", ppid, comm, ok, os.Getppid())
	}
	got := child(t, "walk", `/bin/sh -c "/bin/sh -c \"BIN; true\"; true"; true`)
	if got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("AgentPID under three shells = %s, want this process %d", got, os.Getpid())
	}
	if got := child(t, "walk", "BIN; true"); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("AgentPID under one shell = %s", got)
	}
}

// A hook orphaned before it looks (Codex runs notify without waiting; the
// hook then belongs to init, launchd or a subreaper) sends no pid, never 1
// or an unrelated ancestor.
func TestAgentPIDOrphanSendsNone(t *testing.T) {
	if _, _, ok := parentOf(os.Getpid()); !ok {
		t.Skip("no process table here")
	}
	if got := child(t, "orphan", "BIN &"); got != "0" {
		t.Fatalf("an orphaned hook's AgentPID = %s, want 0", got)
	}
}
