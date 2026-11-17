package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/potluck/kit/api"
)

type fakeDaemon struct {
	as, cwd string
	f       *fakeState
}

type fakeState struct {
	log         []string
	notice      string
	known       map[string]bool   // registered addresses; nil = every address is known
	project     string            // project of registered sessions (default "01PROJ")
	noHook      bool              // a daemon without POST /v0/sessions/hook
	oldHook     bool              // a daemon whose hook route predates its activity field
	pids        []int             // each hook's pid
	addresses   map[string]string // canonical addresses by full runtime id
	registerErr error             // a refused registration
	oldSub      bool              // a daemon whose hook route predates subagent and needs
	preActivity bool              // a daemon whose hook route predates activity (strict decode, as api.DecodeJSON)
	preUsage    bool              // a daemon whose hook route predates for, job_kind and agent_home
	onHook      func(orchapi.HookRequest)
}

func (d fakeDaemon) Register(_ context.Context, r orchapi.RegisterRequest) (orchapi.Session, error) {
	d.f.log = append(d.f.log, "register "+r.Agent+" "+r.RuntimeID+" cwd="+r.Cwd+" model="+r.Model+" as="+d.as)
	if r.Session != "" || r.State != "" || r.End {
		d.f.log = append(d.f.log, "  session="+r.Session+" state="+string(r.State)+" end="+map[bool]string{true: "1", false: "0"}[r.End])
	}
	if d.f.registerErr != nil {
		return orchapi.Session{}, d.f.registerErr
	}
	addr := "mini/" + api.Session8(r.RuntimeID)
	if canonical, ok := d.f.addresses[r.RuntimeID]; ok {
		addr = canonical
	}
	if d.f.known != nil {
		d.f.known[addr] = true
	}
	p := d.f.project
	if p == "" {
		p = "01PROJ"
	}
	return orchapi.Session{Address: addr, ID: api.Session8(r.RuntimeID), Project: p}, nil
}

func (d fakeDaemon) SetState(_ context.Context, id string, r orchapi.StateRequest) (orchapi.Session, error) {
	d.f.log = append(d.f.log, "state "+id+" "+string(r.State)+" "+r.Note+" as="+d.as)
	if d.f.known != nil && !d.f.known[id] {
		return orchapi.Session{}, api.NotFound("session", id)
	}
	return orchapi.Session{}, nil
}

func (d fakeDaemon) Hook(_ context.Context, r orchapi.HookRequest) (orchapi.HookResponse, error) {
	d.f.pids = append(d.f.pids, r.PID)
	d.f.log = append(d.f.log, "hook "+r.Agent+" "+r.RuntimeID+" cwd="+r.Cwd+" model="+r.Model+" as="+d.as+
		" session="+r.Session+" state="+string(r.State)+" note="+r.Note+" notice="+map[bool]string{true: "1", false: "0"}[r.Notice]+
		map[bool]string{true: " activity=" + r.Activity}[r.Activity != ""])
	if d.f.noHook {
		return orchapi.HookResponse{}, api.Errorf(api.CodeNotFound, "no route POST /v0/sessions/hook")
	}
	if d.f.preActivity {
		b, _ := json.Marshal(r)
		hr, _ := http.NewRequest(http.MethodPost, "/v0/sessions/hook", bytes.NewReader(b))
		var old struct {
			orchapi.RegisterRequest
			Notice bool `json:"notice,omitempty"`
		}
		if err := api.DecodeJSON(hr, &old); err != nil {
			return orchapi.HookResponse{}, err
		}
	}
	if d.f.oldSub && (r.Subagent != nil || r.Needs != "") {
		return orchapi.HookResponse{}, api.Errorf(api.CodeBadRequest, `invalid JSON body: json: unknown field "subagent"`)
	}
	if d.f.preUsage && (r.For != "" || r.JobKind != "" || r.AgentHome != "") {
		return orchapi.HookResponse{}, api.Errorf(api.CodeBadRequest, `invalid JSON body: json: unknown field "job_kind"`)
	}
	if d.f.onHook != nil {
		d.f.onHook(r)
	}
	if d.f.oldHook && r.Activity != "" {
		return orchapi.HookResponse{}, api.Errorf(api.CodeBadRequest, `invalid JSON body: json: unknown field "activity"`)
	}
	p := d.f.project
	if p == "" {
		p = "01PROJ"
	}
	addr := "mini/" + api.Session8(r.RuntimeID)
	if r.Session != "" {
		addr = r.Session
	}
	out := orchapi.HookResponse{Session: orchapi.Session{Address: addr, ID: api.Session8(r.RuntimeID), Project: p}}
	if r.Notice {
		out.Notice = &orchapi.Notice{Line: d.f.notice}
	}
	return out, nil
}

func (d fakeDaemon) Notice(context.Context) (orchapi.Notice, error) {
	d.f.log = append(d.f.log, "notice as="+d.as)
	return orchapi.Notice{Line: d.f.notice}, nil
}

func runHook(t *testing.T, in HookInput, env map[string]string, f *fakeState) (string, []string) {
	t.Helper()
	in.Getenv = func(k string) string { return env[k] }
	out, err := RunHook(context.Background(), func(as, cwd string) Daemon { return fakeDaemon{as: as, cwd: cwd, f: f} }, in)
	if err != nil {
		t.Fatal(err)
	}
	return out, f.log
}

func TestHookPromptComputesAddressAndPrintsNotice(t *testing.T) {
	out, log := runHook(t, HookInput{Agent: "claude", Event: HookPrompt, Machine: "mini",
		Payload: []byte(`{"session_id":"4bb7545a-1111-2222","cwd":"/w","hook_event_name":"UserPromptSubmit","prompt":"hi"}`)},
		nil, &fakeState{notice: "zm: 1 unread from human:ana. Run: zm inbox"})
	want := []string{"hook claude 4bb7545a-1111-2222 cwd=/w model= as= session= state=running note= notice=1 activity=Thinking"}
	if strings.Join(log, "|") != strings.Join(want, "|") {
		t.Errorf("log = %q (one request: register-or-find, state and notice)", log)
	}
	var o struct {
		H struct{ HookEventName, AdditionalContext string } `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &o); err != nil || o.H.HookEventName != "UserPromptSubmit" || !strings.Contains(o.H.AdditionalContext, "1 unread") {
		t.Errorf("out = %s (%v)", out, err)
	}
}

// An older daemon (no hook route): one request per step, as before.
func TestHookLegacyDaemon(t *testing.T) {
	out, log := runHook(t, HookInput{Agent: "claude", Event: HookPrompt, Machine: "mini",
		Payload: []byte(`{"session_id":"4bb7545a-1111","cwd":"/w"}`)}, nil, &fakeState{noHook: true, notice: "zm: 1 unread"})
	want := []string{
		"hook claude 4bb7545a-1111 cwd=/w model= as= session= state=running note= notice=1 activity=Thinking",
		"register claude 4bb7545a-1111 cwd=/w model= as=",
		"state mini/4bb7545a running  as=mini/4bb7545a",
		"notice as=mini/4bb7545a",
	}
	if strings.Join(log, "|") != strings.Join(want, "|") || !strings.Contains(out, "1 unread") {
		t.Errorf("log = %q out = %q", log, out)
	}
	_, log = runHook(t, HookInput{Agent: "claude", Event: HookSessionStart, Machine: "mini",
		Payload: []byte(`{"session_id":"4bb7545a-x"}`)}, nil, &fakeState{noHook: true, project: api.ProjectNone, notice: "x"})
	if len(log) != 3 || !strings.HasPrefix(log[1], "register ") {
		t.Errorf("legacy session-start outside projects: log = %q (no notice)", log)
	}
}

func TestHookPromptRegistersUnknownSession(t *testing.T) {
	_, log := runHook(t, HookInput{Agent: "claude", Event: HookPrompt, Machine: "mini",
		Payload: []byte(`{"session_id":"4bb7545a-1111","cwd":"/w"}`)}, nil, &fakeState{known: map[string]bool{}, noHook: true})
	want := []string{
		"hook claude 4bb7545a-1111 cwd=/w model= as= session= state=running note= notice=1 activity=Thinking",
		"register claude 4bb7545a-1111 cwd=/w model= as=",
		"state mini/4bb7545a running  as=mini/4bb7545a",
		"notice as=mini/4bb7545a",
	}
	if strings.Join(log, "|") != strings.Join(want, "|") {
		t.Errorf("log = %q", log)
	}
}

func TestHookPromptQuietWithoutMail(t *testing.T) {
	out, _ := runHook(t, HookInput{Agent: "claude", Event: HookPrompt, Machine: "mini", Payload: []byte(`{"session_id":"abcdefgh12"}`)}, nil, &fakeState{})
	if out != "" {
		t.Errorf("out = %q", out)
	}
}

// No runtime id anywhere, only $ZM_AS: report on that address.
func TestHookManagedSessionUsesZmAs(t *testing.T) {
	_, log := runHook(t, HookInput{Agent: "claude", Event: HookStop, Payload: []byte(`{}`)},
		map[string]string{"ZM_AS": "mini/9c0e11aa"}, &fakeState{})
	if len(log) != 1 || log[0] != "state mini/9c0e11aa idle  as=mini/9c0e11aa" {
		t.Errorf("log = %q", log)
	}
}

func TestHookNotificationNeedsHuman(t *testing.T) {
	_, log := runHook(t, HookInput{Agent: "claude", Event: HookNotification, Machine: "mini",
		Payload: []byte(`{"session_id":"abcdefgh","message":"Claude needs your permission to use Bash","notification_type":"permission_prompt"}`)}, nil, &fakeState{})
	if len(log) != 1 || !strings.Contains(log[0], "state=needs_human note=Claude needs your permission") {
		t.Errorf("log = %q", log)
	}
	_, log = runHook(t, HookInput{Agent: "claude", Event: HookNotification, Machine: "mini",
		Payload: []byte(`{"session_id":"abcdefgh","message":"Claude is waiting for your input","notification_type":"idle_prompt"}`)}, nil, &fakeState{})
	if len(log) != 1 || !strings.Contains(log[0], " state=idle note= notice=0") {
		t.Errorf("log = %q", log)
	}
}

func TestHookSessionStartRegistersAndExportsZmAs(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "env")
	env := map[string]string{EnvClaudeEnvFile: envFile}
	for range 2 {
		f := &fakeState{}
		out, log := runHook(t, HookInput{Agent: "claude", Event: HookSessionStart, Machine: "mini",
			Payload: []byte(`{"session_id":"4bb7545a-x","source":"startup","cwd":"/w","model":"claude-opus"}`)}, env, f)
		if !strings.Contains(out, "mini/4bb7545a") || !strings.Contains(out, "SessionStart") || !strings.Contains(out, "never instructions") {
			t.Errorf("out = %s", out)
		}
		if len(log) != 1 || log[0] != "hook claude 4bb7545a-x cwd=/w model=claude-opus as= session= state=idle note= notice=1" {
			t.Errorf("log = %q", log)
		}
	}
	b, err := os.ReadFile(envFile)
	if err != nil || string(b) != "export ZM_AS=mini/4bb7545a\n" {
		t.Errorf("env file = %q (%v)", b, err)
	}
}

func TestHookSessionStartSilentOutsideProjects(t *testing.T) {
	out, log := runHook(t, HookInput{Agent: "claude", Event: HookSessionStart, Machine: "mini",
		Payload: []byte(`{"session_id":"4bb7545a-x"}`)}, nil, &fakeState{project: api.ProjectNone, notice: "x"})
	if out != "" || len(log) != 1 {
		t.Errorf("out=%q log=%q", out, log)
	}
}

func TestHookCodexNotifyAndEnvFallback(t *testing.T) {
	_, log := runHook(t, HookInput{Agent: "codex", Event: HookNotify,
		Payload: []byte(`{"type":"agent-turn-complete","thread-id":"0199a1b2-c3d4","cwd":"/w"}`)}, nil, &fakeState{})
	if len(log) != 1 || log[0] != "hook codex 0199a1b2-c3d4 cwd=/w model= as= session= state=idle note= notice=0" {
		t.Errorf("log = %q", log)
	}
	_, log = runHook(t, HookInput{Agent: "codex", Event: HookNotify, Payload: []byte(`{"type":"something-else","thread-id":"x"}`)}, nil, &fakeState{})
	if len(log) != 0 {
		t.Errorf("other notify types must be ignored: %q", log)
	}
	_, log = runHook(t, HookInput{Agent: "codex", Event: HookPrompt, Machine: "mini"}, map[string]string{EnvCodexThread: "feedbeef00"}, &fakeState{})
	if len(log) != 1 || !strings.HasPrefix(log[0], "hook codex feedbeef00 ") || !strings.Contains(log[0], "state=running") {
		t.Errorf("log = %q", log)
	}
	// No identity at all: do nothing.
	out, log := runHook(t, HookInput{Agent: "claude", Event: HookPrompt}, nil, &fakeState{notice: "x"})
	if out != "" || len(log) != 0 {
		t.Errorf("out=%q log=%q", out, log)
	}
}

func TestHookUnknownEventAndBadPayload(t *testing.T) {
	f := func(as, cwd string) Daemon { return fakeDaemon{f: &fakeState{}} }
	if _, err := RunHook(context.Background(), f, HookInput{Agent: "claude", Event: "nope", Getenv: func(string) string { return "" }}); !api.IsCode(err, api.CodeBadRequest) {
		t.Errorf("err = %v", err)
	}
	if _, err := RunHook(context.Background(), f, HookInput{Agent: "claude", Event: HookStop, Payload: []byte("{"), Getenv: func(string) string { return "" }}); err == nil {
		t.Error("bad JSON accepted")
	}
}

func TestEnvSession(t *testing.T) {
	a, id := EnvSession(func(k string) string { return map[string]string{EnvCodexThread: "t1"}[k] })
	if a != "codex" || id != "t1" {
		t.Errorf("%s %s", a, id)
	}
}

func TestHookInsideZmRunRecordsRuntimeID(t *testing.T) {
	env := map[string]string{"ZM_AS": "mini/abcd1234", "ZM_SESSION": "mini/abcd1234"}
	_, log := runHook(t, HookInput{Agent: "claude", Event: HookSessionStart, Machine: "mini",
		Payload: []byte(`{"session_id":"4bb7545a-1111","cwd":"/w"}`)}, env, &fakeState{})
	want := "hook claude 4bb7545a-1111 cwd=/w model= as=mini/abcd1234 session=mini/abcd1234 state=idle note= notice=1"
	if strings.Join(log, "|") != want {
		t.Errorf("log = %q, want one request on the managed session, notice included", log)
	}
}

func TestHookSessionEnd(t *testing.T) {
	payload := []byte(`{"session_id":"4bb7545a-1111","cwd":"/w"}`)
	_, log := runHook(t, HookInput{Agent: "claude", Event: HookSessionEnd, Payload: payload}, nil, &fakeState{})
	if want := "register claude 4bb7545a-1111 cwd=/w model= as=|  session= state= end=1"; strings.Join(log, "|") != want {
		t.Errorf("unmanaged end: log = %q", log)
	}
	_, log = runHook(t, HookInput{Agent: "claude", Event: HookSessionEnd, Payload: payload},
		map[string]string{"ZM_AS": "mini/abcd1234", "ZM_SESSION": "mini/abcd1234"}, &fakeState{})
	// A session zm started ends when its process exits: inside one, the end
	// goes to it only to unbind an agent bound to a managed shell.
	if want := "hook claude 4bb7545a-1111 cwd=/w model= as=mini/abcd1234 session=mini/abcd1234 state= note=session ended notice=0"; strings.Join(log, "|") != want {
		t.Errorf("end inside a managed session: log = %q", log)
	}
}

// Claude Code subagents: start and stop carry the subagent and no state; a
// tool step inside one names it; the parent's Task tool announces a spawn;
// a permission prompt says why it waits.
func TestHookSubagents(t *testing.T) {
	var got []orchapi.HookRequest
	f := &fakeState{}
	f.onHook = func(r orchapi.HookRequest) { got = append(got, r) }
	for _, in := range []HookInput{
		{Agent: "claude", Event: HookTool, Payload: []byte(`{"session_id":"p1","tool_name":"Task","tool_input":{"description":"Find browse package","subagent_type":"Explore","prompt":"secret"}}`)},
		{Agent: "claude", Event: HookSubagentStart, Payload: []byte(`{"session_id":"p1","agent_id":"a1","agent_type":"Explore"}`)},
		{Agent: "claude", Event: HookTool, Payload: []byte(`{"session_id":"p1","agent_id":"a1","agent_type":"Explore","tool_name":"Read","tool_input":{"file_path":"/x/repos.go"}}`)},
		{Agent: "claude", Event: HookSubagentStop, Payload: []byte(`{"session_id":"p1","agent_id":"a1"}`)},
		{Agent: "claude", Event: HookSubagentStop, Payload: []byte(`{"session_id":"p1"}`)}, // no agent_id: nothing
		{Agent: "claude", Event: HookNotification, Payload: []byte(`{"session_id":"p1","notification_type":"elicitation_dialog","message":"pick one"}`)},
	} {
		runHook(t, in, nil, f)
	}
	want := []string{
		`{"event":"spawn","type":"Explore","description":"Find browse package"} running "Find browse package" ""`,
		`{"event":"start","id":"a1","type":"Explore"}  "" ""`,
		`{"id":"a1","type":"Explore"} running "Reading repos.go" ""`,
		`{"event":"stop","id":"a1"}  "" ""`,
		`null needs_human "" "input"`,
	}
	if len(got) != len(want) {
		t.Fatalf("hooks sent: %+v", got)
	}
	for i, r := range got {
		b, _ := json.Marshal(r.Subagent)
		if s := fmt.Sprintf("%s %s %q %q", b, r.State, r.Activity, r.Needs); s != want[i] {
			t.Errorf("hook %d: %s, want %s", i, s, want[i])
		}
	}
	// An older daemon: a start or stop is dropped, a step goes as before.
	old := &fakeState{oldSub: true}
	_, log := runHook(t, HookInput{Agent: "claude", Event: HookSubagentStart, Payload: []byte(`{"session_id":"p1","agent_id":"a1"}`)}, nil, old)
	if len(log) != 1 {
		t.Errorf("start on an older daemon: %q", log)
	}
	old.log = nil
	_, log = runHook(t, HookInput{Agent: "claude", Event: HookTool, Payload: []byte(`{"session_id":"p1","agent_id":"a1","tool_name":"Grep"}`)}, nil, old)
	if len(log) != 2 || !strings.HasSuffix(log[1], "activity=Searching") {
		t.Errorf("step on an older daemon: %q", log)
	}
}

// Review of PR 294, finding 4: a daemon older than the activity field names
// one unknown field at a time; the hook drops every newer field at once and
// the state still lands.
func TestHookPreActivityDaemon(t *testing.T) {
	for _, in := range []HookInput{
		{Agent: "claude", Event: HookTool, Payload: []byte(`{"session_id":"p1","agent_id":"a1","tool_name":"Grep"}`)},
		{Agent: "claude", Event: HookTool, Payload: []byte(`{"session_id":"p1","tool_name":"Task","tool_input":{"description":"Scope","subagent_type":"Explore"}}`)},
		{Agent: "claude", Event: HookNotification, Payload: []byte(`{"session_id":"p1","notification_type":"permission_prompt","message":"Bash?"}`)},
	} {
		f := &fakeState{preActivity: true}
		_, log := runHook(t, in, nil, f)
		if len(log) < 2 || len(log) > 3 || strings.Contains(log[len(log)-1], "activity=") {
			t.Errorf("%s: %q, want the state alone on the last retry", in.Event, log)
		}
	}
	f := &fakeState{preActivity: true}
	if _, log := runHook(t, HookInput{Agent: "claude", Event: HookSubagentStart, Payload: []byte(`{"session_id":"p1","agent_id":"a1"}`)}, nil, f); len(log) != 1 {
		t.Errorf("start on a pre-activity daemon: %q", log)
	}
}

// A hook sends its session's attribution from its environment; a daemon
// older than those fields refuses one by name, and the hook drops all three.
func TestHookUsageAttribution(t *testing.T) {
	env := map[string]string{"ZM_FOR": "bea-mac", "ZM_JOB_KIND": "farm", "CLAUDE_CONFIG_DIR": "/home/ana/.claude-2", "CODEX_HOME": "/x"}
	in := HookInput{Agent: "claude", Event: HookSessionStart, Payload: []byte(`{"session_id":"p1","cwd":"/w"}`)}
	for _, pre := range []bool{false, true} {
		var got []orchapi.HookRequest
		f := &fakeState{preUsage: pre, onHook: func(r orchapi.HookRequest) { got = append(got, r) }}
		runHook(t, in, env, f)
		want := orchapi.RegisterRequest{For: "bea-mac", JobKind: "farm", AgentHome: "/home/ana/.claude-2"}
		if pre {
			want = orchapi.RegisterRequest{}
		}
		if len(got) != 1 || got[0].For != want.For || got[0].JobKind != want.JobKind || got[0].AgentHome != want.AgentHome || got[0].State != orchapi.StateIdle {
			t.Errorf("older daemon %v: %+v", pre, got)
		}
	}
}

// A first notify cannot prove it belongs to the inherited managed session.
// Route by full runtime; SessionStart/Prompt/Tool establish managed binding.
func TestHookCodexNotifyDoesNotAdoptManagedSession(t *testing.T) {
	_, log := runHook(t, HookInput{Agent: "codex", Event: HookNotify,
		Payload: []byte(`{"type":"agent-turn-complete","thread-id":"01a18eb8-292b","cwd":"/w"}`)},
		map[string]string{"ZM_AS": "mini/7d3f5c5e", "ZM_SESSION": "mini/7d3f5c5e"}, &fakeState{})
	if len(log) != 1 || log[0] != "hook codex 01a18eb8-292b cwd=/w model= as= session= state=idle note= notice=0" {
		t.Errorf("log = %q", log)
	}
}

func TestToolActivity(t *testing.T) {
	cases := []struct{ tool, input, want string }{
		{"Bash", `{"command":"curl -H 'Authorization: Bearer s3cr3t' x","description":"Fetch the release list"}`, "Fetch the release list"},
		{"Bash", `{"command":"rm -rf /tmp/s3cr3t"}`, "Running a command"},
		{"Bash", `{"command":"x","description":"line one\nline two"}`, "line one"},
		{"Edit", `{"file_path":"/Users/ana/code/s3cr3t-dir/store.go","old_string":"s3cr3t"}`, "Editing store.go"},
		{"MultiEdit", `{"file_path":"a/b.go","edits":[]}`, "Editing b.go"},
		{"Write", `{"file_path":"/w/notes.md","content":"s3cr3t"}`, "Writing notes.md"},
		{"Read", `{"file_path":"/w/.env"}`, "Reading .env"},
		{"Read", `{}`, "Reading a file"},
		{"NotebookEdit", `{"notebook_path":"/w/x.ipynb","new_source":"s3cr3t"}`, "Editing x.ipynb"},
		{"Grep", `{"pattern":"s3cr3t","path":"/w"}`, "Searching"},
		{"Glob", `{"pattern":"**/s3cr3t*"}`, "Searching"},
		{"Task", `{"description":"Review the diff","prompt":"s3cr3t"}`, "Review the diff"},
		{"Agent", `{"description":"","prompt":"s3cr3t"}`, "Agent"},
		{"WebFetch", `{"url":"https://go.dev/doc/s3cr3t?q=s3cr3t","prompt":"s3cr3t"}`, "Web: go.dev"},
		{"WebFetch", `{"url":"not a url"}`, "Searching the web"},
		{"WebSearch", `{"query":"s3cr3t"}`, "Searching the web"},
		{"TodoWrite", `{"todos":[{"content":"s3cr3t","status":"completed","activeForm":"Done"},{"content":"s3cr3t","status":"in_progress","activeForm":"Running the tests"}]}`, "Running the tests"},
		{"TodoWrite", `{"todos":[]}`, "TodoWrite"},
		{"mcp__zm__mail_send", `{"body":"s3cr3t"}`, "mcp__zm__mail_send"},
		{"Bash", `not json`, "Running a command"},
	}
	for _, tc := range cases {
		got := ToolActivity(tc.tool, json.RawMessage(tc.input))
		if got != tc.want || strings.Contains(got, "s3cr3t") {
			t.Errorf("ToolActivity(%s, %s) = %q, want %q", tc.tool, tc.input, got, tc.want)
		}
	}
	long := ToolActivity("Bash", json.RawMessage(`{"description":"`+strings.Repeat("ab", 200)+`"}`))
	if n := len([]rune(long)); n != orchapi.MaxActivityLen {
		t.Errorf("long description: %d characters", n)
	}
}

// The tool hook (PreToolUse) is one request: running plus the activity.
func TestHookTool(t *testing.T) {
	payload := []byte(`{"session_id":"4bb7545a-1111","cwd":"/w","hook_event_name":"PreToolUse","tool_name":"Edit",
		"tool_input":{"file_path":"/w/internal/store.go","old_string":"x","new_string":"y"}}`)
	out, log := runHook(t, HookInput{Agent: "claude", Event: HookTool, Machine: "mini", Payload: payload}, nil, &fakeState{})
	want := "hook claude 4bb7545a-1111 cwd=/w model= as= session= state=running note= notice=0 activity=Editing store.go"
	if out != "" || len(log) != 1 || log[0] != want {
		t.Errorf("out = %q log = %q", out, log)
	}
	// Inside zm run: to that session.
	_, log = runHook(t, HookInput{Agent: "claude", Event: HookTool, Machine: "mini", Payload: payload},
		map[string]string{"ZM_SESSION": "mini/9c0e11aa", "ZM_AS": "mini/9c0e11aa"}, &fakeState{})
	if len(log) != 1 || !strings.Contains(log[0], "as=mini/9c0e11aa session=mini/9c0e11aa state=running") {
		t.Errorf("managed: log = %q", log)
	}
	// A daemon that predates the activity field: the state alone, once more.
	_, log = runHook(t, HookInput{Agent: "claude", Event: HookTool, Machine: "mini", Payload: payload}, nil, &fakeState{oldHook: true})
	if len(log) != 2 || !strings.HasSuffix(log[1], "notice=0") {
		t.Errorf("old daemon: log = %q", log)
	}
	// A payload cut at the stdin cap: nothing, no error.
	in := HookInput{Agent: "claude", Event: HookTool, Machine: "mini", Payload: payload[:40], Getenv: func(string) string { return "" }}
	f := &fakeState{}
	if out, err := RunHook(context.Background(), func(as, cwd string) Daemon { return fakeDaemon{as: as, cwd: cwd, f: f} }, in); err != nil || out != "" || len(f.log) != 0 {
		t.Errorf("cut payload: %q %v %q", out, err, f.log)
	}
}

// Codex names a thread three ways: hooks.json's session_id (the root
// thread's, on a subagent thread too) with agent_id (a subagent thread's
// own id), notify's thread-id and the tools' CODEX_THREAD_ID (both the
// thread's own). One thread is one session: a Codex hook takes agent_id
// first, so a subagent's tool hooks keep its own session running, the one
// its notify and its claims (made with its CODEX_THREAD_ID) name.
// (codex-rs: Session::session_id is the root thread's id; hook_runtime.rs
// fills agent_id from the thread for subagents; legacy_notify.rs sends
// thread_id.)
func TestHookCodexThreadIsOneSession(t *testing.T) {
	const root, child = "0199aaaa-root", "0199bbbb-child"
	_, log := runHook(t, HookInput{Agent: "codex", Event: HookTool,
		Payload: []byte(`{"session_id":"` + root + `","agent_id":"` + child + `","tool_name":"shell","tool_input":{}}`)}, nil, &fakeState{})
	if len(log) != 1 || !strings.HasPrefix(log[0], "hook codex "+child+" ") {
		t.Fatalf("a subagent's tool hook: %q", log)
	}
	_, log = runHook(t, HookInput{Agent: "codex", Event: HookNotify,
		Payload: []byte(`{"type":"agent-turn-complete","thread-id":"` + child + `"}`)}, nil, &fakeState{})
	if len(log) != 1 || !strings.HasPrefix(log[0], "hook codex "+child+" ") {
		t.Fatalf("its notify: %q", log)
	}
	_, log = runHook(t, HookInput{Agent: "codex", Event: HookPrompt, Payload: []byte(`{"session_id":"` + root + `"}`)}, nil, &fakeState{})
	if len(log) != 1 || !strings.HasPrefix(log[0], "hook codex "+root+" ") {
		t.Fatalf("the root thread's hook: %q", log)
	}
	// Claude's hooks keep session_id.
	_, log = runHook(t, HookInput{Agent: "claude", Event: HookTool, Payload: []byte(`{"session_id":"c1","agent_id":"a1","tool_name":"Bash","tool_input":{}}`)}, nil, &fakeState{})
	if len(log) != 1 || !strings.HasPrefix(log[0], "hook claude c1 ") {
		t.Fatalf("claude: %q", log)
	}
}

// Codex's SubagentStop ends a subagent thread's turn, not the thread: it is
// installed as stop, so that thread's session (agent_id, not its root's
// session_id) goes idle and keeps its claims for its next turn.
func TestHookCodexSubagentStopIsIdle(t *testing.T) {
	const root, child = "0199aaaa-root", "0199bbbb-child"
	for _, h := range codexHooks {
		if h.Event == "SubagentStop" && h.Arg != HookStop {
			t.Fatalf("SubagentStop installed as %q, want stop", h.Arg)
		}
	}
	_, log := runHook(t, HookInput{Agent: "codex", Event: HookStop,
		Payload: []byte(`{"session_id":"` + root + `","agent_id":"` + child + `","hook_event_name":"SubagentStop","agent_type":"default"}`)}, nil, &fakeState{})
	if len(log) != 1 || !strings.HasPrefix(log[0], "hook codex "+child+" ") || !strings.Contains(log[0], "state=idle") || strings.Contains(log[0], "end=1") {
		t.Fatalf("log = %q", log)
	}
	if !strings.Contains(strings.Join(codexHookEvents(), ","), "SubagentStop") {
		t.Fatal("zm install codex does not install SubagentStop")
	}
}

func codexHookEvents() []string {
	var out []string
	for _, h := range codexHooks {
		out = append(out, h.Event)
	}
	return out
}

// Legacy daemons still choose canonical addresses after runtime-prefix
// collisions. Every hook must resolve that address before state or inbox reads.
func TestHookLegacyRuntimeCollision(t *testing.T) {
	const first, second = "01a1a548-first", "01a1a548-second"
	for _, event := range []string{HookSessionStart, HookPrompt, HookTool, HookStop, HookNotify, HookNotification} {
		t.Run(event, func(t *testing.T) {
			f := &fakeState{noHook: true, notice: "zm: unread", addresses: map[string]string{
				first: "mini/01a1a548", second: "mini/cafebabe",
			}}
			for _, rid := range []string{first, second} {
				f.log = nil
				_, log := runHook(t, HookInput{Agent: "codex", Event: event, Machine: "mini",
					Payload: []byte(`{"session_id":"` + rid + `","cwd":"/w"}`)}, nil, f)
				addr := f.addresses[rid]
				registered, updated, noticed := false, false, false
				for _, entry := range log {
					switch {
					case strings.HasPrefix(entry, "register "):
						registered = strings.Contains(entry, rid)
					case strings.HasPrefix(entry, "state "):
						updated = true
						if !registered || !strings.HasPrefix(entry, "state "+addr+" ") || !strings.HasSuffix(entry, "as="+addr) {
							t.Errorf("state before canonical registration or wrong recipient: %q", log)
						}
					case strings.HasPrefix(entry, "notice "):
						noticed = true
						if entry != "notice as="+addr {
							t.Errorf("wrong inbox: %q", log)
						}
					}
				}
				if !registered || !updated || noticed != (event == HookSessionStart || event == HookPrompt) {
					t.Errorf("missing registration/state or wrong notice behavior: %q", log)
				}
			}
		})
	}
}

func TestHookLegacyRegistrationRefusalStopsActions(t *testing.T) {
	for _, failure := range []string{"refused", "empty address"} {
		t.Run(failure, func(t *testing.T) {
			f := &fakeState{noHook: true}
			if failure == "refused" {
				f.registerErr = api.Errorf(api.CodeForbidden, "registration refused")
			} else {
				f.addresses = map[string]string{"01a1a548-second": ""}
			}
			_, err := RunHook(context.Background(), func(as, cwd string) Daemon { return fakeDaemon{as: as, cwd: cwd, f: f} }, HookInput{
				Agent: "codex", Event: HookPrompt, Machine: "mini",
				Payload: []byte(`{"session_id":"01a1a548-second"}`), Getenv: func(string) string { return "" },
			})
			if err == nil {
				t.Fatal("invalid registration allowed")
			}
			for _, entry := range f.log {
				if strings.HasPrefix(entry, "state ") || strings.HasPrefix(entry, "notice ") {
					t.Errorf("action after failed registration: %q", f.log)
				}
			}
		})
	}
}
