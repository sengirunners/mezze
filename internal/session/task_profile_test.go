package session

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

func TestTaskRecordedEvents(t *testing.T) {
	for _, tc := range []struct {
		agent                 string
		input, output, cached int64
	}{{"codex", 19515, 7, 12160}, {"claude", 2, 8, 11491}} {
		t.Run(tc.agent, func(t *testing.T) {
			b, err := os.ReadFile("testdata/tasks/" + tc.agent + ".jsonl")
			if err != nil {
				t.Fatal(err)
			}
			b = append([]byte("diagnostic on PTY\r\n"), bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))...)
			r := parseTaskEvents(tc.agent, b)
			if !r.Complete || !r.Success || r.Text != "lane probe ok" || r.InputTokens != tc.input || r.OutputTokens != tc.output || r.CachedInputTokens != tc.cached {
				t.Fatalf("result: %+v", r)
			}
		})
	}
}

func TestTaskRequiresTerminalEvent(t *testing.T) {
	for _, tc := range []struct {
		agent, events string
		complete      bool
	}{{"codex", `{"type":"item.completed","item":{"type":"agent_message","text":"hello"}}`, false}, {"codex", `{"type":"turn.failed"}`, true}, {"claude", `{"type":"result","subtype":"error_during_execution","is_error":true}`, true}} {
		r := parseTaskEvents(tc.agent, []byte(tc.events))
		if r.Success || r.Complete != tc.complete {
			t.Fatalf("%s: %+v", tc.agent, r)
		}
	}
}

// claudeRejected is Claude Code's stream-json event for a spent five-hour
// window (shape as recorded in internal/agentpipe/testdata/claude), resetting
// in 2100.
const claudeRejected = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","resetsAt":4102444800}}`

func TestTaskStructuredErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		agent, stream string
		want          orchapi.OutcomeKind
	}{
		// A bare 429 is a rate limit, not a spent plan.
		{"claude", `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429}`, orchapi.OutcomeFailed},
		{"claude", claudeRejected + "\n" + `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429}`, orchapi.OutcomeQuota},
		{"claude", `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","resetsAt":1000}}` + "\n" + `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429}`, orchapi.OutcomeFailed},
		{"claude", `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour","resetsAt":4102444800}}` + "\n" + `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429}`, orchapi.OutcomeFailed},
		{"claude", `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"You've hit your limit · resets 3pm"}`, orchapi.OutcomeQuota},
		{"claude", `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429,"result":"Claude AI usage limit reached|1793833800"}`, orchapi.OutcomeQuota},
		{"claude", `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429,"result":"You've hit your weekly limit"}`, orchapi.OutcomeQuota},
		{"claude", `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429,"result":"5-hour limit reached ∙ resets 3pm"}`, orchapi.OutcomeQuota},
		{"claude", `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429,"result":"Opus limit reached"}`, orchapi.OutcomeQuota},
		{"claude", `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429,"result":"You're out of extra usage"}`, orchapi.OutcomeQuota},
		{"claude", `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":429,"result":"API Error: 429 rate_limit_error"}`, orchapi.OutcomeFailed},
		{"claude", `{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":401}`, orchapi.OutcomeAuth},
		{"codex", `{"type":"turn.failed","error":{"code":"usage_limit_reached"}}`, orchapi.OutcomeQuota},
		{"codex", `{"type":"turn.failed","error":{"message":"quota exceeded"}}`, orchapi.OutcomeFailed},
	} {
		r := parseTaskEvents(tc.agent, []byte(tc.stream))
		if !r.Complete || r.Success || r.Kind != tc.want {
			t.Fatalf("%s: %+v", tc.agent, r)
		}
	}
}

func TestTaskRecordedAuthFailures(t *testing.T) {
	for _, tc := range []struct{ agent, file string }{{"codex", "codex-auth.jsonl"}, {"claude", "claude-auth.jsonl"}, {"claude", "claude-invalid-key.jsonl"}} {
		data, err := os.ReadFile("testdata/tasks/" + tc.file)
		if err != nil {
			t.Fatal(err)
		}
		got := parseTaskEvents(tc.agent, data)
		if !got.Complete || got.Success || got.Kind != orchapi.OutcomeAuth || got.LowConfidence {
			t.Fatalf("%s: %+v", tc.file, got)
		}
	}
}
func TestTaskUnrecordedQuotaHasLowConfidence(t *testing.T) {
	for _, tc := range []struct{ agent, stream string }{{"codex", `{"type":"turn.failed","error":{"code":"usage_limit_reached"}}`}, {"claude", claudeRejected + "\n" + `{"type":"result","is_error":true,"api_error_status":429}`}} {
		got := parseTaskEvents(tc.agent, []byte(tc.stream))
		outcome := taskOutcome(got, "", nil)
		if outcome.Kind != orchapi.OutcomeQuota || outcome.Confidence != "low" {
			t.Fatalf("%s: %+v", tc.agent, outcome)
		}
	}
}

func TestTaskProfilesIsolateHooksAndKeepDelegation(t *testing.T) {
	s := &Service{HolderExe: "/tmp/zm-test", d: &core.Deps{Config: &config.Config{}}}
	claude, err := s.taskCommand("claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	var mcp map[string]any
	var settingSources string
	var hasSettingSources bool
	for i, arg := range claude {
		if i+1 >= len(claude) {
			break
		}
		switch arg {
		case "--setting-sources":
			hasSettingSources, settingSources = true, claude[i+1]
		case "--settings":
			if err := json.Unmarshal([]byte(claude[i+1]), &settings); err != nil {
				t.Fatal(err)
			}
		case "--mcp-config":
			if err := json.Unmarshal([]byte(claude[i+1]), &mcp); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !hasSettingSources || settingSources != "" {
		t.Fatalf("Claude can load settings that replace plan credentials: %v", claude)
	}
	if settings["disableAllHooks"] != true {
		t.Fatalf("Claude hooks are enabled: %v", claude)
	}
	servers, _ := mcp["mcpServers"].(map[string]any)
	if _, ok := servers["zm"]; !ok {
		t.Fatalf("Claude lost zm MCP: %v", claude)
	}
	codex, err := s.taskCommand("codex", nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(codex, " ")
	if !strings.Contains(joined, "features.hooks=false") || !strings.Contains(joined, "notify=[]") || !strings.Contains(joined, "mcp_servers.zm.command=") {
		t.Fatalf("Codex profile: %v", codex)
	}
}
