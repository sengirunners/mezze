package session

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/config"
)

func TestAdmit(t *testing.T) {
	lim := config.Admission{} // defaults: 20%, 4096 MB swap, 5120 MB disk, no count limit
	ok := orchapi.AdmissionProbe{MemoryFreePercent: 45, SwapUsedMB: 700, DiskFreeMB: 80000, ManagedSessions: 2}
	cases := []struct {
		name   string
		mut    func(*orchapi.AdmissionProbe)
		lim    config.Admission
		reason string
	}{
		{"healthy", func(*orchapi.AdmissionProbe) {}, lim, ""},
		{"low memory", func(p *orchapi.AdmissionProbe) { p.MemoryFreePercent = 12 }, lim, orchapi.AdmitMemory},
		{"memory at threshold", func(p *orchapi.AdmissionProbe) { p.MemoryFreePercent = 20 }, lim, ""},
		{"swap", func(p *orchapi.AdmissionProbe) { p.SwapUsedMB = 6100 }, lim, orchapi.AdmitMemory},
		{"disk", func(p *orchapi.AdmissionProbe) { p.DiskFreeMB = 100 }, lim, orchapi.AdmitDisk},
		{"no count limit by default", func(p *orchapi.AdmissionProbe) { p.ManagedSessions = 500 }, lim, ""},
		{"custom limit", func(p *orchapi.AdmissionProbe) { p.ManagedSessions = 2 }, config.Admission{MaxManagedSessions: 2}, orchapi.AdmitSessions},
		{"memory wins over disk", func(p *orchapi.AdmissionProbe) { p.MemoryFreePercent, p.DiskFreeMB = 5, 5 }, lim, orchapi.AdmitMemory},
		{"unknown values never refuse", func(p *orchapi.AdmissionProbe) { p.MemoryFreePercent, p.SwapUsedMB, p.DiskFreeMB = -1, -1, -1 }, lim, ""},
	}
	for _, c := range cases {
		p := ok
		c.mut(&p)
		a := Admit(p, c.lim)
		if a.OK != (c.reason == "") || a.Reason != c.reason || a.Probe != p {
			t.Errorf("%s: got %+v, want reason %q", c.name, a, c.reason)
		}
		if !a.OK && a.Detail == "" {
			t.Errorf("%s: refusal without detail", c.name)
		}
	}
}

func TestProbeMachine(t *testing.T) {
	p := probeMachine(t.TempDir())
	if p.DiskFreeMB <= 0 {
		t.Fatalf("disk probe: %+v", p)
	}
	if p.MemoryFreePercent < -1 || p.MemoryFreePercent > 100 {
		t.Fatalf("memory probe: %+v", p)
	}
}

// Fixture transcripts are named <agent>__<kind>__<exit>[suffix].txt.
func TestDetectorFixtures(t *testing.T) {
	files, _ := filepath.Glob("testdata/outcomes/*.txt")
	if len(files) < 10 {
		t.Fatalf("only %d fixtures", len(files))
	}
	for _, f := range files {
		parts := strings.Split(strings.TrimSuffix(filepath.Base(f), ".txt"), "__")
		agent, want := parts[0], orchapi.OutcomeKind(parts[1])
		code, _ := strconv.Atoi(strings.TrimRight(parts[2], "abcdefghijklmnopqrstuvwxyz"))
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		o := classify(exitInfo{agent: agent, code: code, tail: b})
		if o.Kind != want {
			t.Errorf("%s: got %s (%q), want %s", f, o.Kind, o.Evidence, want)
		}
		if o.Kind == want && o.Kind != orchapi.OutcomeOK && o.Kind != orchapi.OutcomeFailed && (o.Evidence == "" || o.Confidence != "low") {
			t.Errorf("%s: a text match needs evidence and low confidence: %+v", f, o)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		x    exitInfo
		want orchapi.OutcomeKind
		conf string
	}{
		{"exit 0", exitInfo{agent: "claude", code: 0}, orchapi.OutcomeOK, "high"},
		{"exit 1", exitInfo{agent: "claude", code: 1}, orchapi.OutcomeFailed, "high"},
		{"quota with exit 0 is quota", exitInfo{agent: "claude", code: 0, tail: []byte("Claude AI usage limit reached|1")}, orchapi.OutcomeQuota, "low"},
		{"stopped by zm", exitInfo{agent: "codex", code: -1, signal: "TERM", stopped: true, stopBy: "human:me"}, orchapi.OutcomeKilled, "high"},
		{"stopped, clean exit", exitInfo{agent: "codex", code: 0, stopped: true, stopBy: "human:me"}, orchapi.OutcomeOK, "high"},
		{"stopped, exit 143", exitInfo{agent: "shell", code: 143, stopped: true, stopBy: "human:me"}, orchapi.OutcomeKilled, "high"},
		{"outside signal", exitInfo{agent: "shell", code: -1, signal: "KILL"}, orchapi.OutcomeKilled, "high"},
		{"daemon stop", exitInfo{agent: "shell", code: -1, signal: "TERM", daemonStop: true, tail: []byte("quota exceeded")}, orchapi.OutcomeInfra, "high"},
		{"ansi wrapped", exitInfo{agent: "codex", code: 1, tail: []byte("\x1b[31mERROR\x1b[0m: \x1b[1mYou've hit your usage limit\x1b[0m\r\n")}, orchapi.OutcomeQuota, "low"},
		{"other runtime's phrase is not matched", exitInfo{agent: "claude", code: 0, tail: []byte("usage_limit_reached")}, orchapi.OutcomeOK, "high"},
	}
	for _, c := range cases {
		o := classify(c.x)
		if o.Kind != c.want || o.Confidence != c.conf {
			t.Errorf("%s: got %+v, want %s/%s", c.name, o, c.want, c.conf)
		}
		if c.x.code >= 0 && (o.ExitCode == nil || *o.ExitCode != c.x.code) {
			t.Errorf("%s: exit code not recorded: %+v", c.name, o)
		}
	}
}

// TestOutsideKillNamesNoCause: zm cannot see who sent a signal, so an
// outside KILL never blames memory pressure (a stray pkill -9 -f
// was reported as the OS), and one read from an exit file written while no
// daemon ran says so.
func TestOutsideKillNamesNoCause(t *testing.T) {
	o := classify(exitInfo{agent: "claude", code: -1, signal: "KILL"})
	if strings.Contains(o.Note, "memory") || !strings.Contains(o.Note, "sender is unknown") || o.Signal != "KILL" {
		t.Fatalf("outside KILL: %+v", o)
	}
	if strings.Contains(o.Note, "no zm daemon") {
		t.Fatalf("ended under a live daemon, yet the note says none ran: %q", o.Note)
	}
	o = classify(exitInfo{agent: "claude", code: -1, signal: "KILL", noDaemon: true})
	if !strings.Contains(o.Note, "ended while no zm daemon ran") {
		t.Fatalf("ended while no daemon ran: %+v", o)
	}
}

func TestParseHook(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	claude := func(ev, extra string) []byte {
		return []byte(`{"session_id":"4BB7545A-1111-2222","cwd":"/w/repo","hook_event_name":"` + ev + `"` + extra + `}`)
	}
	cases := []struct {
		event, agent string
		in           []byte
		state        orchapi.SessionState
		end          bool
		note         string
	}{
		{"session-start", "", claude("SessionStart", ""), orchapi.StateIdle, false, ""},
		{"prompt", "", claude("UserPromptSubmit", `,"prompt":"hi"`), orchapi.StateRunning, false, ""},
		{"stop", "", claude("Stop", ""), orchapi.StateIdle, false, ""},
		{"notification", "", claude("Notification", `,"message":"Claude needs your permission to use Bash"`), orchapi.StateNeedsHuman, false, "Claude needs your permission to use Bash"},
		{"notification", "", claude("Notification", `,"message":"Claude is waiting for your input"`), orchapi.StateIdle, false, ""},
		{"session-end", "", claude("SessionEnd", `,"reason":"logout"`), "", true, "logout"},
		{"", "", claude("Stop", ""), orchapi.StateIdle, false, ""},
		{"prompt", "gemini", claude("BeforeAgent", ""), orchapi.StateRunning, false, ""},
	}
	for _, c := range cases {
		r, ok := ParseHook(c.event, c.agent, c.in, getenv)
		if !ok || r.RuntimeID != "4BB7545A-1111-2222" || r.Cwd != "/w/repo" || r.State != c.state || r.End != c.end || r.Note != c.note {
			t.Errorf("%s: %+v ok=%v", c.event, r, ok)
		}
		if want := c.agent; want == "" && r.Agent != "claude" || want != "" && r.Agent != want {
			t.Errorf("%s: agent %q", c.event, r.Agent)
		}
	}
	r, ok := ParseHook("codex-notify", "", []byte(`{"type":"agent-turn-complete","thread-id":"0199a213-81c0","turn-id":"1","cwd":"/w"}`), getenv)
	if !ok || r.Agent != "codex" || r.RuntimeID != "0199a213-81c0" || r.State != orchapi.StateIdle {
		t.Errorf("codex: %+v", r)
	}
	if _, ok := ParseHook("stop", "", []byte(`not json`), getenv); ok {
		t.Error("no session id anywhere should report nothing")
	}
	env["CLAUDE_CODE_SESSION_ID"], env["ZM_SESSION"], env["PWD"] = "abc", "mini/12345678", "/here"
	r, ok = ParseHook("stop", "", nil, getenv)
	if !ok || r.RuntimeID != "abc" || r.Session != "mini/12345678" || r.Cwd != "/here" {
		t.Errorf("env fallback: %+v", r)
	}
}

// TestEnvMapDropsAgentMarkers: a daemon started from inside an agent session
// must not pass that session to the ones it starts. A Claude Code that inherits
// CLAUDE_CODE_CHILD_SESSION saves no transcript, so its session can't be resumed.
func TestEnvMapDropsAgentMarkers(t *testing.T) {
	for _, k := range []string{"CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDECODE", "CMUX_SURFACE_ID"} {
		t.Setenv(k, "1")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "/c")
	m, err := envMap(map[string]string{"CLAUDE_CODE_CHILD_SESSION": "1", "KEEP": "y"})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"CLAUDE_CODE_CHILD_SESSION", "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDECODE", "CMUX_SURFACE_ID"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s reached the session", k)
		}
	}
	if m["CLAUDE_CONFIG_DIR"] != "/c" || m["KEEP"] != "y" {
		t.Errorf("settings were dropped: %q %q", m["CLAUDE_CONFIG_DIR"], m["KEEP"])
	}
}

func TestSandboxArgv(t *testing.T) {
	got := sandboxArgv("/x/srt", "/s/p.json", []string{"/bin/claude", "--model", "opus", "it's a test"})
	want := []string{"/x/srt", "--settings", "/s/p.json", `/bin/claude --model opus 'it'\''s a test'`}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q", got)
	}
	paths := sandboxPaths([]string{"/r/a", "/r/a"}, "codex", "/h/worktrees/x", "/h/scratch/x", map[string]string{})
	if !contains(paths, "/r/a") || !contains(paths, "/h/worktrees/x") || !contains(paths, "/h/scratch/x") || strings.Count(strings.Join(paths, ","), "/r/a") != 1 {
		t.Fatalf("paths %q", paths)
	}
}

func TestModelOf(t *testing.T) {
	for argv, want := range map[string]string{"claude --model opus": "opus", "codex -m o3": "o3", "x --model=y": "y", "claude": "", "claude -p --model": ""} {
		if got := modelOf(strings.Fields(argv)); got != want {
			t.Errorf("%s: %q", argv, got)
		}
	}
}

func TestOutputTrimsPartialRune(t *testing.T) {
	o := output([]byte("ab\xe2\x8f"), 10, 14)
	if o.Data != "ab" || o.Next != 12 || o.Offset != 10 {
		t.Fatalf("%+v", o)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
