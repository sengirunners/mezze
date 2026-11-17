package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// EnvJudge picks the provider and model: <provider>[:<model>], e.g.
// "claude:haiku" (the default) or "claude:sonnet"; "none" asks no model, so
// every verdict is held.
const EnvJudge = "ZM_JUDGE"

// DefaultProvider and DefaultModel are what an unset ZM_JUDGE means.
const (
	DefaultProvider = "claude"
	DefaultModel    = "haiku"
)

// Parallel bounds the provider CLIs a daemon runs at once for judges.
const Parallel = 4

var slots = make(chan struct{}, Parallel)

// Providers are the CLIs CLI knows how to call.
var Providers = map[string]bool{"claude": true}

// refused are providers a judge may not use yet, with why: a judge's model
// reads text an agent wrote, so it runs with no tools at all.
var refused = map[string]string{
	"codex": "codex exec always offers its model a shell and other tools (browser, apps, plugins), and a judge must run with none",
}

// FromEnv is the CLI that ZM_JUDGE names, run with the machine's configured
// command for that provider (config.json "agents", else the provider's
// name). A bad, refused or "none" ZM_JUDGE is a Caller that always errs, so
// every verdict is held.
func FromEnv(agents map[string][]string) Caller {
	spec := strings.TrimSpace(os.Getenv(EnvJudge))
	if spec == "" {
		spec = DefaultProvider + ":" + DefaultModel
	}
	provider, model, _ := strings.Cut(spec, ":")
	if provider == "none" {
		return Fake{Err: fmt.Errorf("%w (%s=none)", ErrNoProvider, EnvJudge)}
	}
	if why, ok := refused[provider]; ok {
		return Fake{Err: fmt.Errorf("%w: %s=%q: %s", ErrNoProvider, EnvJudge, spec, why)}
	}
	if !Providers[provider] {
		return Fake{Err: fmt.Errorf("%w: %s=%q names no known provider (claude)", ErrNoProvider, EnvJudge, spec)}
	}
	return CLI{Provider: provider, Command: agents[provider], Model: model}
}

// CLI asks a model through a provider's own CLI, one process per call, with
// no tools, no hooks, no MCP servers, no saved session and no extended
// thinking (a verdict in seconds, not minutes). The child's environment is
// an allowlist (childEnv): what finds the CLI and its plan login, never the
// daemon's secrets, API keys or zm session.
type CLI struct {
	Provider string   // "claude"
	Command  []string // argv head (config.json "agents"); default [Provider]
	Model    string   // default haiku
}

// Call runs the CLI with prompt on stdin and returns its structured answer.
func (c CLI) Call(ctx context.Context, prompt string, schema json.RawMessage) (json.RawMessage, error) {
	if c.Provider != "claude" {
		return nil, fmt.Errorf("%w: provider %q", ErrNoProvider, c.Provider)
	}
	argv := append([]string(nil), c.Command...)
	if len(argv) == 0 {
		argv = []string{c.Provider}
	}
	model := c.Model
	if model == "" {
		model = DefaultModel
	}
	argv = append(argv, "-p", "--model", model, "--output-format", "json", "--json-schema", string(schema),
		"--tools", "", "--setting-sources", "", "--settings", `{"disableAllHooks":true,"alwaysThinkingEnabled":false}`,
		"--strict-mcp-config", "--no-session-persistence", "--disable-slash-commands")
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNoProvider, err)
	}
	dir, err := os.MkdirTemp("", "zm-judge")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	// One provider CLI is a few hundred MB: at most Parallel run at once on
	// a daemon; more wait their turn inside their deadline.
	select {
	case slots <- struct{}{}:
		defer func() { <-slots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: every judge slot stayed busy (%d running)", ctx.Err(), Parallel)
	}
	cmd := exec.CommandContext(ctx, bin, argv[1:]...)
	// Its own process group, killed whole at the deadline: a CLI's children
	// must not outlive the call or hold its output open.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = dir
	cmd.Env = childEnv(os.Environ())
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s: %v: %s", c.Provider, err, clip(stderr.String()+stdout.String()))
	}
	return claudeAnswer(stdout.Bytes())
}

// claudeAnswer is the structured output of `claude -p --output-format json`.
func claudeAnswer(b []byte) (json.RawMessage, error) {
	var res struct {
		IsError    bool            `json:"is_error"`
		Subtype    string          `json:"subtype"`
		Result     string          `json:"result"`
		Structured json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(b), &res); err != nil {
		return nil, fmt.Errorf("claude: not a result: %s", clip(string(b)))
	}
	if res.IsError {
		return nil, fmt.Errorf("claude: %s: %s", res.Subtype, clip(res.Result))
	}
	if len(res.Structured) == 0 || string(res.Structured) == "null" {
		// Never the free text: it may echo a verdict from the input.
		return nil, errors.New("claude: no structured output")
	}
	return res.Structured, nil
}

// childEnv keeps of env only what runs the CLI on the machine's plan login:
// where programs and the home are, the user, locale and temp dir, the
// provider's own config and login variables, and the proxy and CA settings
// that reach the provider. Everything else stays with the daemon: API keys
// (the plan login answers, not a key), tailnet and cloud secrets, and the
// ZM_ variables that would make the call some zm session's.
func childEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if keepEnv[k] || strings.HasPrefix(k, "LC_") || strings.HasPrefix(k, "XDG_") {
			out = append(out, kv)
		}
	}
	return out
}

var keepEnv = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true, "LANG": true, "TZ": true, "TMPDIR": true,
	// claude's own config and its plan login (a setup-token login).
	"CLAUDE_CONFIG_DIR": true, "CLAUDE_CODE_OAUTH_TOKEN": true,
	// Reaching the provider from behind a proxy or a private CA.
	"HTTPS_PROXY": true, "https_proxy": true, "HTTP_PROXY": true, "http_proxy": true, "NO_PROXY": true, "no_proxy": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_EXTRA_CA_CERTS": true,
}

func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
