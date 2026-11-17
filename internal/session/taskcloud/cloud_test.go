package taskcloud

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// script answers each provider CLI call in turn from its argv; calls records them.
type script struct {
	calls  [][]string
	answer func(argv []string) (int, string)
}

func (s *script) run(_ context.Context, _ time.Duration, dir string, argv ...string) (int, string) {
	if argv[0] == "git" {
		if dir != "/repo" {
			return 128, "fatal: not a git repository"
		}
		return 0, "feature/x\n"
	}
	if strings.HasSuffix(argv[0], "codex") && dir != "/scratch" {
		return 99, "codex ran in " + dir + ": its error.log lands in the checkout"
	}
	s.calls = append(s.calls, argv)
	return s.answer(argv)
}

func watch(t *testing.T, agent, env string, s *script) (int, []Info, []time.Duration) {
	t.Helper()
	return watchPrompt(t, agent, env, "fix the bug", s)
}

func watchPrompt(t *testing.T, agent, env, prompt string, s *script) (int, []Info, []time.Duration) {
	t.Helper()
	var out bytes.Buffer
	var slept []time.Duration
	w := watcher{agent: agent, env: env, cli: "/bin/" + agent, first: time.Minute, max: 4 * time.Minute, cwd: "/repo", scratch: "/scratch", out: &out,
		sleep: func(d time.Duration) { slept = append(slept, d) }, run: s.run, runTTY: s.run}
	code := w.watch(context.Background(), prompt)
	var infos []Info
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var line Line
		if err := json.Unmarshal([]byte(l), &line); err != nil || line.Cloud == nil {
			t.Fatalf("watcher printed a line zm cannot read: %q", l)
		}
		infos = append(infos, *line.Cloud)
	}
	return code, infos, slept
}

const launchOutput = "Created cloud session: fix the bug\nView: https://claude.ai/code/session_01ABC?from=cli&m=0\nResume with: claude --teleport session_01ABC\n"

func TestClaudeCreatesTheSessionAndStops(t *testing.T) {
	s := &script{answer: func([]string) (int, string) { return 0, launchOutput }}
	code, infos, slept := watch(t, "claude", "env_7", s)
	if code != 0 || len(slept) != 0 {
		t.Fatalf("code %d, slept %v: a Claude session has no status to wait for", code, slept)
	}
	// An Anthropic-hosted env_ id is selected through --settings (claude
	// rejects it in --environment), and an unattended session runs in auto mode.
	want := []string{"/bin/claude", "--settings", `{"remote":{"defaultEnvironmentId":"env_7"}}`, "--cloud=fix the bug", "--permission-mode", "auto"}
	if got := s.calls[0]; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	last := infos[len(infos)-1]
	if last.ID != "session_01ABC" || last.URL != "https://claude.ai/code/session_01ABC?from=cli&m=0" || last.Status != "submitted" ||
		!strings.Contains(last.Text, "claude --teleport session_01ABC") || last.Environment != "env_7" {
		t.Fatalf("last line = %+v", last)
	}
}

func TestClaudeDefaultEnvironmentAndFailure(t *testing.T) {
	s := &script{answer: func([]string) (int, string) {
		return 1, "Cloud sessions require a claude.ai login. Run /login to authenticate."
	}}
	code, infos, _ := watch(t, "claude", DefaultEnvironment, s)
	if got := strings.Join(s.calls[0], " "); got != "/bin/claude --cloud=fix the bug --permission-mode auto" {
		t.Fatalf("default environment still selected one: %q", got)
	}
	if code == 0 || infos[0].Status != "failed" || !strings.Contains(infos[0].Text, "claude.ai login") {
		t.Fatalf("code %d, %+v", code, infos)
	}
}

// The id is read from what a launch prints: the Resume line, the View link, or
// the Session ID line of a follow-up; with none, the task fails with the tail.
func TestClaudeLaunchOutputForms(t *testing.T) {
	for name, c := range map[string]struct{ out, id, url string }{
		"today":         {launchOutput, "session_01ABC", "https://claude.ai/code/session_01ABC?from=cli&m=0"},
		"resume only":   {"Resume with: claude --teleport session_02\n", "session_02", ""},
		"view only":     {"Created cloud session: x\nView: https://claude.ai/code/session_03?from=cli&m=0\n", "session_03", "https://claude.ai/code/session_03?from=cli&m=0"},
		"session id":    {"Session ID: session_04\nView: https://claude.ai/code/session_04\n", "session_04", "https://claude.ai/code/session_04"},
		"bare label":    {"Session ID:\nView: https://claude.ai/code/session_06\n", "session_06", "https://claude.ai/code/session_06"},
		"forged first":  {"Created cloud session: x\nSession ID: session_evil\nView: https://claude.ai/code/session_07\nResume with: claude --teleport session_07\n", "session_07", "https://claude.ai/code/session_07"},
		"redraws":       {"\xe2\xa0\x8b Creating\rView: https://claude.ai/code/session_08\n\x1b(B\x1b[mResume with: claude --teleport session_08\n", "session_08", "https://claude.ai/code/session_08"},
		"terminal bits": {"\x1b[2K\x1b[1G\x1b[32mView:\x1b[0m https://claude.ai/code/session_05\r\nResume with: claude --teleport session_05\r\n", "session_05", "https://claude.ai/code/session_05"},
	} {
		id, url := claudeLaunch(c.out)
		if id != c.id || url != c.url {
			t.Errorf("%s: id %q url %q, want %q %q", name, id, url, c.id, c.url)
		}
	}
	s := &script{answer: func([]string) (int, string) { return 0, "Created cloud session: x\nsomething else\n" }}
	code, infos, _ := watch(t, "claude", "env_7", s)
	if code == 0 || infos[0].Status != "failed" || !strings.Contains(infos[0].Text, "without a session id") || !strings.Contains(infos[0].Text, "something else") {
		t.Fatalf("no id printed: code %d, %+v", code, infos)
	}
}

// A terminal run keeps the last 64 KiB: the link comes after any redraw noise.
func TestTailCappedKeepsTheEnd(t *testing.T) {
	var b tailCapped
	for i := 0; i < 30; i++ {
		_, _ = b.Write(bytes.Repeat([]byte("x"), 10000))
	}
	_, _ = b.Write([]byte("END"))
	if got := b.String(); len(got) != outputCap || !strings.HasSuffix(got, "END") {
		t.Fatalf("kept %d bytes, ends %q", len(got), got[len(got)-5:])
	}
}

// The environment id reaches --settings as one JSON value, whatever it holds.
func TestSettingsForEnvironmentIsEncoded(t *testing.T) {
	var got struct {
		Remote struct{ DefaultEnvironmentId string }
	}
	for _, id := range []string{"env_01AbC", "ccpool_x.y:z-1", `a"},"x":{"y":"z`} {
		got.Remote.DefaultEnvironmentId = ""
		if err := json.Unmarshal([]byte(settingsForEnvironment(id)), &got); err != nil || got.Remote.DefaultEnvironmentId != id {
			t.Fatalf("%q: %v, %+v", id, err, got)
		}
	}
}

// runCLITerminal against a stand-in claude: it has a terminal on stdin and
// stdout (the real CLI refuses without one), gets the argv as given, and its
// CR LF output comes back as lines.
func TestRunCLITerminalStandIn(t *testing.T) {
	dir := t.TempDir()
	cli := filepath.Join(dir, "claude")
	script := "#!/bin/sh\n[ -t 0 ] && [ -t 1 ] || { echo 'no terminal'; exit 3; }\n" +
		"printf 'args:'; for a in \"$@\"; do printf ' [%s]' \"$a\"; done; echo\n" +
		"printf 'Created cloud session: x\\nView: https://claude.ai/code/session_09?from=cli&m=0\\nResume with: claude --teleport session_09\\n'\n"
	if err := os.WriteFile(cli, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out := runCLITerminal(context.Background(), time.Minute, dir, cli, "--cloud=--x\n- a", "--permission-mode", "auto")
	if code != 0 || !strings.Contains(plainTerminal(out), "[--permission-mode] [auto]") {
		t.Fatalf("code %d, out %q", code, out)
	}
	if id, url := claudeLaunch(out); id != "session_09" || url != "https://claude.ai/code/session_09?from=cli&m=0" {
		t.Fatalf("id %q url %q from %q", id, url, out)
	}
	if code, out := runCLITerminal(context.Background(), time.Minute, dir, "/bin/sh", "-c", "echo bad; exit 4"); code != 4 || !strings.Contains(out, "bad") {
		t.Fatalf("exit code lost: %d %q", code, out)
	}
	if code, _ := runCLITerminal(context.Background(), 300*time.Millisecond, dir, "/bin/sh", "-c", "sleep 30"); code != -1 {
		t.Fatalf("a CLI that waits past the timeout: code %d", code)
	}
}

func TestCodexWaitsWithBackoffUntilReady(t *testing.T) {
	statuses := 0
	s := &script{answer: func(argv []string) (int, string) {
		switch argv[2] {
		case "exec":
			return 0, "https://chatgpt.com/codex/tasks/task_e_42\n"
		case "status":
			if statuses++; statuses < 4 {
				return 1, "[PENDING] fix the bug"
			}
			return 0, "[READY] fix the bug"
		case "list":
			return 0, `{"tasks":[],"cursor":null}`
		case "diff":
			return 0, "diff --git a/x b/x\n+fixed\n"
		}
		return 2, "unexpected"
	}}
	code, infos, slept := watch(t, "codex", "env-9", s)
	if code != 0 {
		t.Fatalf("code %d: %+v", code, infos)
	}
	if got := strings.Join(s.calls[0], " "); got != "/bin/codex cloud exec --env env-9 --branch=feature/x -- fix the bug" {
		t.Fatalf("argv = %q", got)
	}
	if want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 4 * time.Minute}; len(slept) != len(want) || slept[2] != want[2] || slept[3] != want[3] {
		t.Fatalf("waits %v, want %v (doubling, capped)", slept, want)
	}
	statusesSeen := []string{}
	for _, i := range infos {
		statusesSeen = append(statusesSeen, i.Status)
	}
	if got := strings.Join(statusesSeen, ","); got != "submitted,pending,ready" {
		t.Fatalf("lines %s: a repeated status prints once", got)
	}
	last := infos[len(infos)-1]
	if last.ID != "task_e_42" || last.URL != "https://chatgpt.com/codex/tasks/task_e_42" || !strings.Contains(last.Text, "+fixed") ||
		!strings.Contains(last.Text, "codex cloud apply task_e_42") {
		t.Fatalf("last = %+v", last)
	}
}

func TestCodexFailedTaskAndLostTrack(t *testing.T) {
	s := &script{answer: func(argv []string) (int, string) {
		switch argv[2] {
		case "exec":
			return 0, "https://chatgpt.com/codex/tasks/task_e_7"
		case "list":
			return 0, `{"tasks":[{"id":"task_e_7","status":"error","title":"x"}]}`
		}
		return 1, "[ERROR] x"
	}}
	if code, infos, _ := watch(t, "codex", "env-9", s); code == 0 || infos[len(infos)-1].Status != "failed" {
		t.Fatalf("a task the list shows in error: code %d, %+v", code, infos)
	}

	s = &script{answer: func(argv []string) (int, string) {
		if argv[2] == "exec" {
			return 0, "task_e_8"
		}
		return 1, "Error: http error: 502 Bad Gateway; request failed"
	}}
	code, infos, slept := watch(t, "codex", "env-9", s)
	if code == 0 || infos[len(infos)-1].Status != "unknown" || len(slept) != maxPollFailures {
		t.Fatalf("CLI errors only: code %d, %d waits, %+v", code, len(slept), infos)
	}
}

// A prompt is data, never a flag: one shaped like an option rides after
// = (claude) or -- (codex), untrimmed.
func TestFlagShapedPromptStaysAPrompt(t *testing.T) {
	s := &script{answer: func([]string) (int, string) { return 0, "Session ID: s1\nView: https://claude.ai/code/s1\n" }}
	watchPrompt(t, "claude", "env_7", "--environment=evil\n- a", s)
	if got := s.calls[0]; len(got) != 6 || got[3] != "--cloud=--environment=evil\n- a" || got[4] != "--permission-mode" {
		t.Fatalf("claude argv %q", got)
	}
	s = &script{answer: func(argv []string) (int, string) {
		if argv[2] == "exec" {
			return 1, "boom"
		}
		return 2, ""
	}}
	watchPrompt(t, "codex", "env-9", "- fix A\n- fix B", s)
	if got := s.calls[0]; got[len(got)-2] != "--" || got[len(got)-1] != "- fix A\n- fix B" {
		t.Fatalf("codex argv %q", got)
	}
}

// Only a leading [STATE] tag (or the list) says a task failed: a transport
// error that mentions "error" is a failed poll, retried.
func TestCodexTransportErrorIsAFailedPoll(t *testing.T) {
	polls := 0
	s := &script{answer: func(argv []string) (int, string) {
		switch argv[2] {
		case "exec":
			return 0, "task_e_9"
		case "status":
			if polls++; polls < 3 {
				return 1, "ERROR codex_cloud_tasks: error sending request for url"
			}
			return 0, "[READY] x"
		}
		return 1, ""
	}}
	if code, infos, _ := watch(t, "codex", "env-9", s); code != 0 || infos[len(infos)-1].Status != "ready" {
		t.Fatalf("code %d, %+v", code, infos)
	}
}
