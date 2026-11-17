package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/project"
)

// The sandbox door (optional): zm run --sandbox wraps the agent in
// Anthropic's sandbox runtime, `srt`, when it is on PATH. The policy lets the
// agent write only the project's repos, its worktree and scratch dir (plus the
// agent's own state dir and temp); everything else is read-only. Without
// srt, --sandbox is refused, never silently ignored.

// sandboxNetwork is what a sandboxed agent may reach: model APIs, git hosts
// and package registries.
var sandboxNetwork = []string{
	"anthropic.com", "*.anthropic.com", "claude.ai", "*.claude.ai",
	"openai.com", "*.openai.com", "chatgpt.com", "*.chatgpt.com",
	"googleapis.com", "*.googleapis.com",
	"github.com", "*.github.com", "*.githubusercontent.com",
	"registry.npmjs.org", "proxy.golang.org", "sum.golang.org", "pypi.org", "files.pythonhosted.org",
}

// agentState is where each runtime keeps its own state (it must write there to run at all).
var agentState = map[string][]string{
	"claude":   {"~/.claude", "~/.claude.json", "~/.config/claude"},
	"codex":    {"~/.codex"},
	"gemini":   {"~/.gemini"},
	"opencode": {"~/.local/share/opencode", "~/.config/opencode", "~/.cache/opencode"},
}

type srtPolicy struct {
	Network    srtNetwork    `json:"network"`
	Filesystem srtFilesystem `json:"filesystem"`
}

type srtNetwork struct {
	AllowedDomains []string `json:"allowedDomains"`
	DeniedDomains  []string `json:"deniedDomains"`
}

type srtFilesystem struct {
	DenyRead   []string `json:"denyRead"`
	AllowWrite []string `json:"allowWrite"`
	DenyWrite  []string `json:"denyWrite"`
}

// sandboxPaths are the directories a sandboxed session may write.
func sandboxPaths(repos []string, agent, worktree, scratch string, env map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p = project.ExpandPath(p); p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, r := range repos {
		add(r)
	}
	add(worktree)
	add(scratch)
	for _, p := range agentState[agent] {
		add(p)
	}
	add("/tmp")
	add("/private/tmp")
	if t := env["TMPDIR"]; t != "" {
		add(t)
	}
	sort.Strings(out)
	return out
}

// writeSandboxPolicy writes the srt settings file into the scratch dir.
func writeSandboxPolicy(scratch string, allow []string) (string, error) {
	pol := srtPolicy{
		Network:    srtNetwork{AllowedDomains: sandboxNetwork, DeniedDomains: []string{}},
		Filesystem: srtFilesystem{DenyRead: []string{"~/.ssh", "~/.aws", "~/.gnupg"}, AllowWrite: allow, DenyWrite: []string{}},
	}
	b, err := json.MarshalIndent(pol, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(scratch, "srt-settings.json")
	return path, os.WriteFile(path, append(b, '\n'), 0o600)
}

// sandboxArgv wraps argv for srt. The command goes as one shell-quoted
// string, which is right whether srt runs it through a shell or joins its
// arguments first.
func sandboxArgv(srt, policy string, argv []string) []string {
	return []string{srt, "--settings", policy, shellJoin(argv)}
}

func shellJoin(argv []string) string {
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = shellQuote(a)
	}
	return strings.Join(q, " ")
}

func shellQuote(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./=:,@+%", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// errNoSandbox is the refusal when srt is missing.
func errNoSandbox() error {
	return api.Errorf(api.CodeBadRequest, "--sandbox needs Anthropic's sandbox runtime (srt) on PATH; it is not installed").
		WithHint("install it (npm install -g @anthropic-ai/sandbox-runtime) or run without --sandbox")
}
