package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

const taskResultSchema = `{"type":"object","properties":{"finished":{"type":"string","enum":["done","not_done"]},"text":{"type":"string"}},"required":["finished","text"],"additionalProperties":false}`

// taskEventResult is derived only from a runtime's explicit terminal event.
// A missing terminal event must not turn an exit status of zero into success.
type taskEventResult struct {
	Text              string
	Finished          string
	Ambiguous         bool
	LowConfidence     bool
	Success           bool
	Complete          bool
	Kind              orchapi.OutcomeKind
	Evidence          string
	InputTokens       int64
	OutputTokens      int64
	CachedInputTokens int64
}

// parseTaskEvents tolerates PTY CRLF and diagnostics interleaved with JSON.
func parseTaskEvents(agent string, data []byte) taskEventResult {
	var result taskEventResult
	var exhausted string // claude: a rate_limit_event that rejected, its window not yet reset
	s := bufio.NewScanner(bytes.NewReader(data))
	s.Buffer(make([]byte, 4096), taskTranscriptLimit)
	for s.Scan() {
		var e struct {
			Type           string `json:"type"`
			Subtype        string `json:"subtype"`
			IsError        bool   `json:"is_error"`
			APIErrorStatus int    `json:"api_error_status"`
			Error          struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
			Result           string          `json:"result"`
			StructuredOutput json.RawMessage `json:"structured_output"`
			Item             struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
			Usage struct {
				Input     int64 `json:"input_tokens"`
				Output    int64 `json:"output_tokens"`
				Cached    int64 `json:"cached_input_tokens"`
				CacheRead int64 `json:"cache_read_input_tokens"`
			} `json:"usage"`
			RateLimit *struct {
				Status   string `json:"status"`
				Type     string `json:"rateLimitType"`
				ResetsAt int64  `json:"resetsAt"`
			} `json:"rate_limit_info"`
		}
		if json.Unmarshal(bytes.TrimSpace(s.Bytes()), &e) != nil {
			continue
		}
		if result.Complete {
			result.Ambiguous = true
			continue
		}
		switch agent {
		case "codex":
			if e.Type == "item.completed" && e.Item.Type == "agent_message" {
				result.Text = e.Item.Text
			}
			if e.Type == "turn.completed" {
				result.Complete, result.Success = true, true
				result.Kind, result.Evidence = orchapi.OutcomeOK, "turn.completed"
				result.InputTokens, result.OutputTokens, result.CachedInputTokens = e.Usage.Input, e.Usage.Output, e.Usage.Cached
			}
			if e.Type == "turn.failed" {
				result.Complete, result.Success = true, false
				result.Kind, result.LowConfidence = taskRuntimeErrorKind("codex", e.Error.Code, 0, e.Error.Message)
				result.Evidence = "turn.failed"
			}
		case "claude":
			if e.Type == "rate_limit_event" && e.RateLimit != nil && e.RateLimit.Status == "rejected" &&
				(e.RateLimit.ResetsAt == 0 || time.Unix(e.RateLimit.ResetsAt, 0).After(time.Now())) {
				exhausted = "usage window exhausted (" + e.RateLimit.Type + ")"
			}
			if e.Type == "result" {
				result.Complete, result.Success = true, e.Subtype == "success" && !e.IsError
				result.Kind, result.LowConfidence = taskRuntimeErrorKind("claude", e.Subtype, e.APIErrorStatus, e.Result)
				result.Evidence = "result:" + e.Subtype
				// A bare 429 is a rate limit, not a spent plan: it
				// is quota only with a fresh exhausted window in this run or the
				// runtime's own usage-limit words, else a plain failure.
				if result.Kind == orchapi.OutcomeQuota && !result.Success {
					switch {
					case exhausted != "":
						result.Evidence += "; " + exhausted
					case !claudeUsageLimitText(e.Result):
						result.Kind, result.LowConfidence = orchapi.OutcomeFailed, false
						result.Evidence += "; rate limited without an exhausted usage window"
					}
				}
				if result.Success {
					result.Kind = orchapi.OutcomeOK
				}
				result.Text = e.Result
				if len(e.StructuredOutput) > 0 {
					result.Text = string(e.StructuredOutput)
				}
				result.InputTokens, result.OutputTokens, result.CachedInputTokens = e.Usage.Input, e.Usage.Output, e.Usage.CacheRead
			}
		}
	}
	result.Finished = "unreported"
	var report struct {
		Finished string `json:"finished"`
		Text     string `json:"text"`
	}
	if result.Complete && result.Success && json.Unmarshal([]byte(result.Text), &report) == nil && (report.Finished == "done" || report.Finished == "not_done") {
		result.Finished, result.Text = report.Finished, report.Text
	}
	return result
}

// Progress means a completed agent or tool step, not a heartbeat or streamed tool output.
func taskProgress(agent string, line []byte) bool {
	if outputAgent(agent) {
		return true // a script's (a cloud watcher's) output is its progress
	}
	var e struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	if json.Unmarshal(bytes.TrimSpace(line), &e) != nil {
		return false
	}
	switch agent {
	case "claude":
		return e.Type == "assistant" || e.Type == "user" || e.Type == "result"
	case "codex":
		return e.Type == "item.completed" || e.Type == "turn.completed" || e.Type == "turn.failed"
	}
	return false
}

// Classification sees only a runtime terminal error event, never plain output.
func taskRuntimeErrorKind(agent, code string, status int, message string) (orchapi.OutcomeKind, bool) {
	kind := taskErrorKind(code, status)
	if kind == orchapi.OutcomeQuota {
		return kind, true
	} // quota formats have not been recorded locally
	if kind != orchapi.OutcomeFailed {
		return kind, false
	}
	text := strings.ToLower(strings.TrimSpace(message))
	switch agent {
	case "codex":
		if strings.HasPrefix(text, "unexpected status 401 unauthorized:") || strings.Contains(text, "auth error code: invalid_api_key") {
			return orchapi.OutcomeAuth, false
		}
		if strings.Contains(text, "you've hit your usage limit") || strings.Contains(text, "usage limit has been reached") {
			return orchapi.OutcomeQuota, true
		}
	case "claude":
		if strings.HasPrefix(text, "not logged in") || strings.HasPrefix(text, "invalid api key") {
			return orchapi.OutcomeAuth, false
		}
	}
	return orchapi.OutcomeFailed, false
}

// claudeUsageLimitText is Claude Code's own result text for a spent plan
// window (older builds send it without a rate_limit_event): the phrases the
// PTY detector table knows (outcome.go), not its bare rate_limit_error.
func claudeUsageLimitText(result string) bool { return claudeLimitText().MatchString(result) }

var claudeLimitText = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`(?i)usage limit reached|you'?ve hit your (usage |weekly |session |monthly )?limit|\b5-hour limit reached|(weekly|opus) limit reached|limit will reset at|limit reached [∙·] resets|out of extra usage|credit balance is too low`)
})

// Structured runtime codes and statuses carry quota/auth classifications.
func taskErrorKind(code string, status int) orchapi.OutcomeKind {
	switch {
	case status == 401 || status == 403 || code == "authentication_error" || code == "invalid_api_key":
		return orchapi.OutcomeAuth
	case status == 429 || code == "rate_limit_error" || code == "usage_limit_reached" || code == "insufficient_quota":
		return orchapi.OutcomeQuota
	default:
		return orchapi.OutcomeFailed
	}
}

// taskProfile records what the one-shot runtime supports. Each builder adds
// only headless, no-prompt flags; model and prompt are appended by the caller.
type taskProfile struct {
	name                           string
	structured, delegation, resume bool
	argv                           func(*Service, []string) ([]string, error)
}

var taskProfiles = map[string]taskProfile{
	"claude": {name: "claude", structured: true, delegation: true, resume: true, argv: func(s *Service, cmd []string) ([]string, error) {
		exe, err := s.holderExe()
		if err != nil {
			return nil, err
		}
		mcp, err := json.Marshal(map[string]any{"mcpServers": map[string]any{"zm": map[string]any{"command": exe, "args": []string{"mcp"}}}})
		if err != nil {
			return nil, err
		}
		// Project settings can replace plan credentials; keep them disabled.
		return append(cmd, "-p", "--output-format", "stream-json", "--verbose", "--setting-sources", "", "--settings", `{"disableAllHooks":true}`, "--permission-mode", "bypassPermissions", "--mcp-config", string(mcp), "--strict-mcp-config", "--json-schema", taskResultSchema), nil
	}},
	"codex": {name: "codex", structured: true, delegation: true, resume: true, argv: func(s *Service, cmd []string) ([]string, error) {
		exe, err := s.holderExe()
		if err != nil {
			return nil, err
		}
		return append(cmd, "exec", "--json", "--dangerously-bypass-approvals-and-sandbox",
			"-c", "features.hooks=false",
			"-c", "notify=[]",
			"-c", "mcp_servers.zm.command="+strconv.Quote(exe),
			"-c", `mcp_servers.zm.args=["mcp"]`,
			"-c", `mcp_servers.zm.env_vars=["ZM_HOME","ZM_AS","ZM_SESSION","ZM_MACHINE","ZM_HUMAN","ZM_TASK_DEPTH","ZM_SCRATCH","CODEX_THREAD_ID"]`), nil
	}},
}

func (s *Service) taskCommand(agent string, env map[string]string) ([]string, error) {
	p, ok := taskProfiles[agent]
	if !ok {
		return nil, api.Errorf(api.CodeBadRequest, "unknown task agent %q", agent)
	}
	return p.argv(s, s.agentCommand(agent, env))
}

func (s *Service) taskAgents() orchapi.TaskAgents {
	env, _ := envMap(nil)
	out := orchapi.TaskAgents{Agents: []orchapi.TaskAgent{}}
	for _, agent := range []string{"claude", "codex"} {
		cmd := s.agentCommand(agent, env)
		p := taskProfiles[agent]
		a := orchapi.TaskAgent{Agent: agent, Models: []string{}, Capabilities: orchapi.TaskCapabilities{Structured: p.structured, Delegation: p.delegation, Resume: p.resume}}
		if len(cmd) == 0 {
			a.Reason = "no command configured"
		} else if bin, err := lookPath(cmd[0], env["PATH"]); err != nil {
			a.Reason = err.Error()
		} else {
			a.Available = true
			if filepath.Base(bin) == agent {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				args := []string{"login", "status"}
				if agent == "claude" {
					args = []string{"auth", "status"}
				}
				probe := exec.CommandContext(ctx, bin, args...)
				for k, v := range env {
					probe.Env = append(probe.Env, k+"="+v)
				}
				err := probe.Run()
				cancel()
				ok := err == nil
				a.Authenticated = &ok
				if !ok {
					a.Available = false
					a.Reason = "authentication status unavailable or not logged in"
				}
			} else {
				a.Reason = "custom command: authentication not probed"
			}
		}
		out.Agents = append(out.Agents, a)
	}
	return out
}

func taskOutcome(parsed taskEventResult, failure string, process *orchapi.Outcome) orchapi.Outcome {
	outcome := orchapi.Outcome{Kind: orchapi.OutcomeFailed, Confidence: "high", Evidence: "runtime did not emit a terminal result"}
	if process != nil {
		outcome.ExitCode = process.ExitCode
		outcome.Signal = process.Signal
	}
	switch {
	case strings.Contains(failure, "may still be running"):
		outcome.Kind, outcome.Evidence = orchapi.OutcomeFailed, failure
	case failure == "cancelled":
		outcome.Kind, outcome.Evidence = orchapi.OutcomeKilled, "cancelled"
	case process != nil && process.Signal != "":
		outcome.Kind, outcome.Evidence = orchapi.OutcomeKilled, "killed by SIG"+process.Signal
		if failure != "" {
			outcome.Evidence = failure + "; " + outcome.Evidence
		}
	case failure != "":
		outcome.Evidence = failure
	case process != nil && process.Kind == orchapi.OutcomeInfra:
		outcome = *process
	case parsed.Complete && parsed.Success && process != nil && process.ExitCode != nil && *process.ExitCode == 0:
		outcome = orchapi.Outcome{Kind: orchapi.OutcomeOK, Confidence: "high", Evidence: "runtime reported success", ExitCode: process.ExitCode}
	case parsed.Complete && !parsed.Success:
		outcome.Kind, outcome.Evidence = parsed.Kind, parsed.Evidence
	case parsed.Complete && parsed.Success:
		outcome.Evidence = "process exited unsuccessfully after runtime success"
	}
	if (parsed.Ambiguous || parsed.LowConfidence) && failure != "cancelled" && (process == nil || process.Signal == "") {
		outcome.Confidence = "low"
	}
	return outcome
}
