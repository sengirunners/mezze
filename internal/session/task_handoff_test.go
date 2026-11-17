package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/client"
	"github.com/sengirunners/potluck/kit/core"
)

func TestHandoffContinuesAndReadsOmittedMessage(t *testing.T) {
	// Runtime event shapes below are controlled fixtures, not recorded production output.
	message := strings.Repeat("é", 9000)
	event, _ := json.Marshal(map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": message}})
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf(`if [ "$1" = exec ]; then printf '%%s\n' '%s' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else ZM_HANDOFF_TEST=1 exec %s -test.run=^TestHandoffReadAgentHelper$; fi`, event, shellQuote(exe))
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "original request", OnLimit: "next_agent", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	source = waitTask(t, h, source.ID)
	if source.Result.Outcome.Kind != orchapi.OutcomeQuota {
		t.Fatalf("source: %+v", source)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		source, err = orchclient.TaskStatus(context.Background(), h.c, source.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if source.ContinuationError != "" {
			t.Fatalf("handoff failed: %s", source.ContinuationError)
		}
		if source.Continuation != "" && source.Continuation != "pending" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if source.Continuation == "" || source.Continuation == "pending" {
		t.Fatalf("no continuation: %+v", source)
	}
	child := waitTask(t, h, source.Continuation)
	if child.Agent != "claude" || child.Parent != source.Parent || child.ContinuedFrom != source.ID || child.Result.Finished != "done" {
		t.Fatalf("continuation: %+v", child)
	}
	data, err := os.ReadFile(filepath.Join(h.home, "scratch", child.ID, "read.json"))
	if err != nil {
		t.Fatalf("agent did not read omitted item: %v", err)
	}
	var agentPage orchapi.TaskHistoryPage
	if err = json.Unmarshal(data, &agentPage); err != nil || len(agentPage.Items) != 1 || agentPage.Items[0].Text != message[:4000] {
		t.Fatalf("agent read: %+v %v", agentPage, err)
	}
	taskClient(h, orchapi.Session{Address: child.Session})
	page, err := orchclient.TaskRead(context.Background(), h.c, source.ID, source.ID+":0", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || page.Items[0].Text != message[:4000] || page.Next != "4000" {
		t.Fatalf("page: %+v", page)
	}
	other := h.spawn("shell", "exec sleep 30", nil)
	taskClient(h, other)
	if _, err = orchclient.TaskRead(context.Background(), h.c, source.ID, source.ID+":0", 0); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("other session read: %v", err)
	}
}

func TestHandoffSelectionWholeMessages(t *testing.T) {
	items := []orchapi.TaskHistoryItem{{ID: "a", Text: strings.Repeat("x", 16000)}, {ID: "b", Text: "newest"}}
	prompt := handoffPrompt("request", "source", "branch", "unreported", items)
	if strings.Contains(prompt, items[0].Text) || !strings.Contains(prompt, "newest") || !strings.Contains(prompt, `"a"`) || !strings.Contains(prompt, "untrusted") || !strings.Contains(prompt, `"finished":"unreported"`) {
		t.Fatalf("selection: %s", prompt)
	}
}

func TestHandoffTriggersOnlyForLimitOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, event string
		want        orchapi.OutcomeKind
		handoff     bool
	}{
		{"auth", `{"type":"turn.failed","error":{"code":"authentication_error"}}`, orchapi.OutcomeAuth, true},
		{"failure", `{"type":"turn.failed","error":{"code":"other"}}`, orchapi.OutcomeFailed, false},
		{"not_done", `{"type":"item.completed","item":{"type":"agent_message","text":"{\"finished\":\"not_done\",\"text\":\"unfinished\"}"}}
{"type":"turn.completed"}`, orchapi.OutcomeOK, false},
		{"zero exit detector phrase", `{"type":"item.completed","item":{"type":"agent_message","text":"rate limit reached"}}
{"type":"turn.completed"}`, orchapi.OutcomeOK, false},
		{"unreported", `{"type":"item.completed","item":{"type":"agent_message","text":"unstructured"}}
{"type":"turn.completed"}`, orchapi.OutcomeOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			script := fmt.Sprintf(`if [ "$1" = exec ]; then printf '%%s\n' '%s'; else cat >/dev/null; printf '%%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"finished":"done","text":"continued"}}'; fi`, tc.event)
			h, parent := taskHarness(t, script)
			taskClient(h, parent)
			no := false
			source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", OnLimit: "next_agent", Worktree: &no})
			if err != nil {
				t.Fatal(err)
			}
			source = waitTask(t, h, source.ID)
			if source.Result.Outcome.Kind != tc.want {
				t.Fatalf("outcome: %+v", source.Result)
			}
			deadline := time.Now().Add(6 * time.Second)
			for time.Now().Before(deadline) {
				source, err = orchclient.TaskStatus(context.Background(), h.c, source.ID, 0)
				if err != nil {
					t.Fatal(err)
				}
				if source.Continuation != "" && source.Continuation != "pending" {
					break
				}
				if !tc.handoff {
					break
				}
				time.Sleep(50 * time.Millisecond)
			}
			if tc.handoff && (source.Continuation == "" || source.Continuation == "pending" || source.ContinuationError != "") {
				t.Fatalf("missing handoff: %+v", source)
			}
			if !tc.handoff && source.Continuation != "" {
				t.Fatalf("unexpected handoff: %+v", source)
			}
		})
	}
}

func TestHandoffWithheldHistoryFailsClosed(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'`)
	taskClient(h, parent)
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "secret", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	source = waitTask(t, h, source.ID)
	source.Result.TextWithheld = true
	resultJSON, _ := json.Marshal(source.Result)
	_, err = h.s.d.Store.Registry().Exec(`UPDATE session_tasks SET result=? WHERE id=?`, string(resultJSON), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", ContinueFrom: source.ID})
	if !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("withheld continuation: %v", err)
	}
	_, err = orchclient.TaskRead(context.Background(), h.c, source.ID, source.ID+":0", 0)
	if !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("withheld read: %v", err)
	}
}

func TestHandoffAfterVerifiedScrubUsesNoWithheldHistory(t *testing.T) {
	promptFile := filepath.Join(t.TempDir(), "prompt")
	script := `if [ "$1" = exec ]; then printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"private history marker"}}' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else cat > ` + shellQuote(promptFile) + `; exec sleep 30; fi`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "private request marker", Work: "bd:probe-1", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, source.ID)
	_, err = h.s.d.Store.Registry().Exec(`UPDATE session_tasks SET text_withheld=1 WHERE id=?`, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	scrub := core.Scrub{Project: parent.Project, Subjects: []string{"probe-1"}, Texts: []string{"private request marker", "private history marker"}}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = h.s.scrubTasks(context.Background(), scrub)
		if err == nil {
			break
		}
		if !api.IsCode(err, api.CodeUnavailable) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	child, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", ContinueFrom: source.ID})
	if err != nil {
		t.Fatal(err)
	}
	_ = child
	var prompt []byte
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		prompt, err = os.ReadFile(promptFile)
		if err == nil && len(prompt) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || strings.Contains(string(prompt), "private request marker") || strings.Contains(string(prompt), "private history marker") {
		t.Fatalf("unsafe continuation prompt: %s, %v", prompt, err)
	}
}

func TestHandoffStartsFromSourceBranch(t *testing.T) {
	script := `if [ "$1" = exec ]; then git -c user.name=test -c user.email=test@example.com -c commit.gpgsign=false commit -q --allow-empty -m source-work; printf "work" > untracked.txt; printf "ignored.txt\n" > .gitignore; printf "ignored" > ignored.txt; printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else cat > "$ZM_SCRATCH/input.txt"; sleep 2; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"finished":"done","text":"continued"}}'; fi`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	if _, err := git(context.Background(), h.home, "init", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := git(context.Background(), h.home, "-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "initial"); err != nil {
		t.Fatal(err)
	}
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", OnLimit: "next_agent"})
	if err != nil {
		t.Fatal(err)
	}
	source = waitTask(t, h, source.ID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		source, err = orchclient.TaskStatus(context.Background(), h.c, source.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if source.ContinuationError != "" {
			t.Fatal(source.ContinuationError)
		}
		if source.Continuation != "" && source.Continuation != "pending" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if source.Continuation == "" || source.Continuation == "pending" {
		t.Fatal("no continuation")
	}
	candidates, err := orchclient.SessionGC(context.Background(), h.c)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, candidate := range candidates {
		if candidate.Session == source.Session {
			found = true
			if candidate.Safe || candidate.Remove != "" {
				t.Fatalf("source worktree removable while continuation runs: %+v", candidate)
			}
		}
	}
	if !found {
		t.Fatal("source worktree absent from GC listing")
	}
	if err = h.c.Do(context.Background(), "DELETE", "/v0/tasks/"+source.ID+"/transcript", nil, nil); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("source transcript removed during continuation: %v", err)
	}
	child := waitTask(t, h, source.Continuation)
	if child.Result == nil || child.Result.Branch == "" {
		t.Fatalf("continuation branch: %+v", child)
	}
	subject, err := git(context.Background(), h.home, "log", "-1", "--format=%s", child.Result.Branch)
	if err != nil || subject != handoffSnapshotSubject {
		t.Fatalf("branch base: %q %v", subject, err)
	}
	content, err := git(context.Background(), h.home, "show", child.Result.Branch+":untracked.txt")
	if err != nil || content != "work" {
		t.Fatalf("snapshot content: %q %v", content, err)
	}
	if _, err = git(context.Background(), h.home, "show", child.Result.Branch+":ignored.txt"); err == nil {
		t.Fatal("ignored file was committed")
	}
	input, err := os.ReadFile(filepath.Join(h.home, "scratch", child.ID, "input.txt"))
	if err != nil || !strings.Contains(string(input), "unreviewed work") {
		t.Fatalf("snapshot note missing: %v", err)
	}
}

func TestHandoffDoesNotRetryLimitedAgents(t *testing.T) {
	script := `if [ "$1" = exec ]; then printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else printf '%s\n' '{"type":"result","subtype":"error_during_execution","is_error":true,"api_error_status":401}'; fi`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", OnLimit: "next_agent", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, source.ID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		source, err = orchclient.TaskStatus(context.Background(), h.c, source.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if source.Continuation != "" && source.Continuation != "pending" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if source.Continuation == "" || source.Continuation == "pending" {
		t.Fatalf("no continuation: %+v", source)
	}
	second := waitTask(t, h, source.Continuation)
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		second, err = orchclient.TaskStatus(context.Background(), h.c, second.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if second.ContinuationError != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if second.Result.Outcome.Kind != orchapi.OutcomeAuth || second.Continuation != "" || !strings.Contains(second.ContinuationError, "no untried") {
		t.Fatalf("chain: %+v", second)
	}
	var count int
	if err = h.s.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_tasks WHERE parent=?`, parent.Address).Scan(&count); err != nil || count != 2 {
		t.Fatalf("task count %d: %v", count, err)
	}
}

func TestHandoffBudgetChargesUTF8Text(t *testing.T) {
	original := strings.Repeat("o", 1057)
	text := strings.Repeat("<", 14943)
	prompt := handoffPrompt(original, "abcd1234", "zm/source", "unreported", []orchapi.TaskHistoryItem{{ID: "abcd1234:0", Text: text}})
	if !strings.Contains(prompt, text) || strings.Contains(prompt, `\u003c`) {
		t.Fatal("whole text was not selected at the UTF-8 byte limit")
	}
}

func TestExplicitContinuationIdempotency(t *testing.T) {
	script := `if [ "$1" = exec ]; then printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else cat >/dev/null; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"finished":"done","text":"continued"}}'; fi`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, source.ID)
	req := orchapi.TaskStartRequest{Agent: "claude", ContinueFrom: source.ID, ClientRequestID: "retry"}
	first, err := orchclient.TaskStart(context.Background(), h.c, req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := orchclient.TaskStart(context.Background(), h.c, req)
	if err != nil || first.ID != again.ID {
		t.Fatalf("retry: %+v %v", again, err)
	}
	req.OnLimit = "next_agent"
	if _, err = orchclient.TaskStart(context.Background(), h.c, req); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("changed request: %v", err)
	}
	if _, err = orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "other", ClientRequestID: "retry", Worktree: &no}); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("reused request id: %v", err)
	}
}

// TestHandoffReadAgentHelper runs inside the fixture continuation and calls
// the actual socket API with its session identity before reporting done.
func TestHandoffReadAgentHelper(t *testing.T) {
	if os.Getenv("ZM_HANDOFF_TEST") != "1" {
		return
	}
	input, err := io.ReadAll(os.Stdin)
	if err != nil {
		os.Exit(2)
	}
	const marker = `"source":"`
	_, rest, ok := strings.Cut(string(input), marker)
	if !ok {
		os.Exit(3)
	}
	source, _, ok := strings.Cut(rest, `"`)
	if !ok {
		os.Exit(4)
	}
	c := client.NewUnix(filepath.Join(os.Getenv("ZM_HOME"), "b.sock"))
	c.As = os.Getenv("ZM_SESSION")
	page, err := orchclient.TaskRead(context.Background(), c, source, source+":0", 0)
	if err != nil {
		os.Exit(5)
	}
	b, err := json.Marshal(page)
	if err != nil || os.WriteFile(filepath.Join(os.Getenv("ZM_SCRATCH"), "read.json"), b, 0600) != nil {
		os.Exit(6)
	}
	fmt.Println(`{"type":"result","subtype":"success","is_error":false,"structured_output":{"finished":"done","text":"continued"}}`)
	os.Exit(0)
}

func TestHandoffFailureIsScrubbed(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'`)
	taskClient(h, parent)
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	source = waitTask(t, h, source.ID)
	_, err = h.s.d.Store.Registry().Exec(`INSERT INTO session_handoffs(source,status,error) VALUES(?,'failed',?)`, source.ID, "private failure marker")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = h.s.scrubTasks(context.Background(), core.Scrub{Project: parent.Project, Texts: []string{"private failure marker"}})
		if err == nil {
			break
		}
		if !api.IsCode(err, api.CodeUnavailable) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, err := orchclient.TaskStatus(context.Background(), h.c, source.ID, 0)
	if err != nil || strings.Contains(got.ContinuationError, "private failure marker") {
		t.Fatalf("failure not scrubbed: %+v %v", got, err)
	}
}

func TestHandoffReadUsesVerifiedScrubbedSpool(t *testing.T) {
	script := `printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"private \u006darker"}}' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", Work: "bd:probe-1", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	source = waitTask(t, h, source.ID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = h.s.scrubTasks(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"probe-1"}, Texts: []string{"private marker"}})
		if err == nil {
			break
		}
		if !api.IsCode(err, api.CodeUnavailable) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	page, err := orchclient.TaskRead(context.Background(), h.c, source.ID, source.ID+":0", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 1 || strings.Contains(page.Items[0].Text, "private marker") || strings.Contains(page.Items[0].Text, `\u006d`) {
		t.Fatalf("secret in item: %+v", page)
	}
}

func TestHandoffRefusesUnconfirmedGroup(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'`)
	taskClient(h, parent)
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	source = waitTask(t, h, source.ID)
	db := h.d.Store.Registry()
	var originalPGID int
	var originalStart int64
	if err = db.QueryRow(`SELECT pgid,group_start FROM session_tasks WHERE id=?`, source.ID).Scan(&originalPGID, &originalStart); err != nil {
		t.Fatal(err)
	}
	group, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE session_tasks SET pgid=?,group_start=1 WHERE id=?`, group, source.ID); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.Exec(`UPDATE session_tasks SET pgid=?,group_start=? WHERE id=?`, originalPGID, originalStart)
	}()
	if _, err = orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", ContinueFrom: source.ID}); !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "group") {
		t.Fatalf("unconfirmed group continued: %v", err)
	}
	if _, err = orchclient.TaskRead(context.Background(), h.c, source.ID, source.ID+":0", 0); !api.IsCode(err, api.CodeConflict) {
		t.Fatalf("unconfirmed group read: %v", err)
	}
}

func TestContinuationKeepsSourceProject(t *testing.T) {
	script := `if [ "$1" = exec ]; then printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else cat >/dev/null; printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"scoped secret marker"}]}}'; exec sleep 30; fi`
	h, _ := taskHarness(t, script)
	alphaDir := filepath.Join(h.home, "alpha")
	betaDir := filepath.Join(h.home, "beta")
	if err := os.MkdirAll(alphaDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(betaDir, 0700); err != nil {
		t.Fatal(err)
	}
	alpha := h.project("alpha", alphaDir)
	beta := h.project("beta", betaDir)
	parent := h.spawn("shell", "exec sleep 30", func(req *orchapi.SpawnRequest) { req.Cwd = betaDir })
	if parent.Project != beta.ID {
		t.Fatalf("parent project: %+v", parent)
	}
	taskClient(h, parent)
	h.c.Cwd = alphaDir
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, source.ID)
	// The holder can stop a very short-lived group before the daemon reads its
	// start time. Its recorded group and verified stop still permit a handoff.
	if _, err = h.d.Store.Registry().Exec(`UPDATE session_tasks SET group_start=0 WHERE id=?`, source.ID); err != nil {
		t.Fatal(err)
	}
	child, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", ContinueFrom: source.ID})
	if err != nil {
		t.Fatal(err)
	}
	row, ok := h.s.load(context.Background(), child.ID)
	if !ok || row.Project != alpha.ID {
		t.Fatalf("continuation project: %+v", row)
	}
	h.waitOutput(child.ID, "scoped secret marker")
	err = h.s.scrubTasks(context.Background(), core.Scrub{Project: alpha.ID, Texts: []string{"scoped secret marker"}})
	if !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("live scrub: %v", err)
	}
	var holds int
	if err = h.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_task_holds WHERE task_id=?`, child.ID).Scan(&holds); err != nil || holds == 0 {
		t.Fatalf("source-project hold: %d %v", holds, err)
	}
}

func TestContinuationRefusalCanRetry(t *testing.T) {
	script := `if [ "$1" = exec ]; then printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else cat >/dev/null; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"finished":"done","text":"continued"}}'; fi`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, source.ID)
	if _, err = orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", ContinueFrom: source.ID}); !api.IsCode(err, api.CodeBadRequest) {
		t.Fatalf("same agent: %v", err)
	}
	var count int
	if err = h.d.Store.Registry().QueryRow(`SELECT count(*) FROM session_handoffs WHERE source=?`, source.ID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("same agent consumed source: %d %v", count, err)
	}
	bin := h.d.Config.File.Agents["claude"][0]
	if err = os.Rename(bin, bin+".held"); err != nil {
		t.Fatal(err)
	}
	req := orchapi.TaskStartRequest{Agent: "claude", ContinueFrom: source.ID, ClientRequestID: "retry"}
	_, err = orchclient.TaskStart(context.Background(), h.c, req)
	if !api.IsCode(err, api.CodeUnavailable) {
		t.Fatalf("refusal: %v", err)
	}
	if err = os.Rename(bin+".held", bin); err != nil {
		t.Fatal(err)
	}
	child, err := orchclient.TaskStart(context.Background(), h.c, req)
	if err != nil || child.ID == "" {
		t.Fatalf("retry: %+v %v", child, err)
	}
}

func TestCancelSourceStopsContinuation(t *testing.T) {
	script := `if [ "$1" = exec ]; then printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else cat >/dev/null; exec sleep 30; fi`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", OnLimit: "next_agent", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, source.ID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		source, err = orchclient.TaskStatus(context.Background(), h.c, source.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if source.Continuation != "" && source.Continuation != "pending" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if source.Continuation == "" || source.Continuation == "pending" {
		t.Fatalf("missing continuation: %+v", source)
	}
	if _, err = orchclient.TaskCancel(context.Background(), h.c, source.ID); err != nil {
		t.Fatal(err)
	}
	child := waitTask(t, h, source.Continuation)
	if child.Result == nil || child.Result.Outcome.Kind != orchapi.OutcomeKilled {
		t.Fatalf("continuation not cancelled: %+v", child)
	}
}

func TestSnapshotCarriesDirtyAndUntrackedButNotIgnored(t *testing.T) {
	repo, home := t.TempDir(), t.TempDir()
	ctx := context.Background()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "initial"}} {
		if _, err := git(ctx, repo, args...); err != nil {
			t.Fatal(err)
		}
	}
	wt, branch, _, err := addWorktree(ctx, home, repo, "abc12345")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(wt, ".gitignore"), []byte("ignored.txt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(wt, "carried.txt"), []byte("unreviewed work"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(wt, "ignored.txt"), []byte("excluded"), 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := snapshotTaskWorktree(ctx, row{Session: orchapi.Session{Worktree: wt, Branch: branch}})
	if err != nil || !snapshot {
		t.Fatalf("snapshot: %v %v", snapshot, err)
	}
	got, err := git(ctx, repo, "show", branch+":carried.txt")
	if err != nil || got != "unreviewed work" {
		t.Fatalf("missing work: %q %v", got, err)
	}
	if _, err = git(ctx, repo, "show", branch+":ignored.txt"); err == nil {
		t.Fatal("ignored file committed")
	}
}

func TestWithheldHistoryContinuesOnlyWithVerifiedOriginal(t *testing.T) {
	promptFile := filepath.Join(t.TempDir(), "prompt")
	script := `if [ "$1" = exec ]; then printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"withheld history secret"}}' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else cat > ` + shellQuote(promptFile) + `; exec sleep 30; fi`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "safe original request", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	source = waitTask(t, h, source.ID)
	source.Result.TextWithheld = true
	source.Result.Text = ""
	b, _ := json.Marshal(source.Result)
	if _, err = h.d.Store.Registry().Exec(`UPDATE session_tasks SET text_withheld=1,request_verified=1,result=? WHERE id=?`, string(b), source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", ContinueFrom: source.ID}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var prompt []byte
	for time.Now().Before(deadline) {
		prompt, err = os.ReadFile(promptFile)
		if err == nil && len(prompt) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || !strings.Contains(string(prompt), "safe original request") || !strings.Contains(string(prompt), "History was withheld") || strings.Contains(string(prompt), "withheld history secret") {
		t.Fatalf("withheld handoff: %q %v", prompt, err)
	}
}

func TestRecordedCodexAuthStartsContinuation(t *testing.T) {
	data, err := os.ReadFile("testdata/tasks/codex-auth.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	script := `if [ "$1" = exec ]; then printf '%s\n' ` + shellQuote(strings.TrimSpace(string(data))) + `; else cat >/dev/null; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"finished":"done","text":"continued"}}'; fi`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", OnLimit: "next_agent", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	source = waitTask(t, h, source.ID)
	if source.Result == nil || source.Result.Outcome.Kind != orchapi.OutcomeAuth || source.Result.Outcome.Confidence != "high" {
		t.Fatalf("recorded auth: %+v", source.Result)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		source, err = orchclient.TaskStatus(context.Background(), h.c, source.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if source.ContinuationError != "" {
			t.Fatal(source.ContinuationError)
		}
		if source.Continuation != "" && source.Continuation != "pending" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if source.Continuation == "" || source.Continuation == "pending" {
		t.Fatalf("no continuation: %+v", source)
	}
	child := waitTask(t, h, source.Continuation)
	if child.Result == nil || child.Result.Finished != "done" {
		t.Fatalf("continuation: %+v", child)
	}
}

func TestUnverifiedGroupStopRefusesAndReportsHandoff(t *testing.T) {
	h, parent := taskHarness(t, `printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'`)
	taskClient(h, parent)
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, source.ID)
	if _, err = h.d.Store.Registry().Exec(`UPDATE session_tasks SET group_stopped=0 WHERE id=?`, source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", ContinueFrom: source.ID}); !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "not verified") {
		t.Fatalf("unverified stop continued: %v", err)
	}
	source, err = orchclient.TaskStatus(context.Background(), h.c, source.ID, 0)
	if err != nil || !strings.Contains(source.ContinuationError, "not verified") || source.Continuation != "" {
		t.Fatalf("parent not told: %+v %v", source, err)
	}
}

func TestFailedSpoolScrubStillTransfersVerifiedOriginal(t *testing.T) {
	promptFile := filepath.Join(t.TempDir(), "prompt")
	deep := strings.Repeat(`\`, 1<<16) + `u0041`
	script := `if [ "$1" = exec ]; then printf '%s\n' ` + shellQuote("bad "+deep+" private secret marker") + ` '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else cat > ` + shellQuote(promptFile) + `; exec sleep 30; fi`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "safe original", Work: "target", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	_ = waitTask(t, h, source.ID)
	deadline := time.Now().Add(5 * time.Second)
	for {
		err = h.s.scrubTasks(context.Background(), core.Scrub{Project: parent.Project, Subjects: []string{"target"}, Texts: []string{"private secret marker"}})
		if err == nil {
			break
		}
		if !api.IsCode(err, api.CodeUnavailable) || time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	source, err = orchclient.TaskStatus(context.Background(), h.c, source.ID, 0)
	if err != nil || source.Result == nil || !source.Result.TextWithheld {
		t.Fatalf("expected withheld result: %+v %v", source, err)
	}
	if _, err = orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", ContinueFrom: source.ID}); err != nil {
		t.Fatal(err)
	}
	var prompt []byte
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		prompt, err = os.ReadFile(promptFile)
		if err == nil && len(prompt) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil || !strings.Contains(string(prompt), "safe original") || !strings.Contains(string(prompt), "History was withheld") || strings.Contains(string(prompt), "private secret marker") {
		t.Fatalf("unsafe handoff: %q %v", prompt, err)
	}
}

func TestRecordedClaudeAuthStartsContinuation(t *testing.T) {
	data, err := os.ReadFile("testdata/tasks/claude-auth.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	script := `if [ "$1" = "-p" ]; then printf '%s\n' ` + shellQuote(strings.TrimSpace(string(data))) + `; else cat >/dev/null; printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"{\"finished\":\"done\",\"text\":\"continued\"}"}}' '{"type":"turn.completed"}'; fi`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	no := false
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "claude", Prompt: "go", OnLimit: "next_agent", Worktree: &no})
	if err != nil {
		t.Fatal(err)
	}
	source = waitTask(t, h, source.ID)
	if source.Result == nil || source.Result.Outcome.Kind != orchapi.OutcomeAuth || source.Result.Outcome.Confidence != "high" {
		t.Fatalf("recorded auth: %+v", source.Result)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		source, err = orchclient.TaskStatus(context.Background(), h.c, source.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if source.ContinuationError != "" {
			t.Fatal(source.ContinuationError)
		}
		if source.Continuation != "" && source.Continuation != "pending" {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if source.Continuation == "" || source.Continuation == "pending" {
		t.Fatalf("no continuation: %+v", source)
	}
	child := waitTask(t, h, source.Continuation)
	if child.Result == nil || child.Result.Finished != "done" {
		t.Fatalf("continuation: %+v", child)
	}
}

func TestSnapshotFailureRefusesAndCanRetry(t *testing.T) {
	script := `if [ "$1" = exec ]; then printf 'dirty' > new.txt; printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else cat >/dev/null; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"finished":"done","text":"continued"}}'; fi`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	ctx := context.Background()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "initial"}} {
		if _, err := git(ctx, h.home, args...); err != nil {
			t.Fatal(err)
		}
	}
	source, err := orchclient.TaskStart(ctx, h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go"})
	if err != nil {
		t.Fatal(err)
	}
	source = waitTask(t, h, source.ID)
	row, ok := h.s.load(ctx, source.ID)
	if !ok {
		t.Fatal("source missing")
	}
	index, err := git(ctx, row.Worktree, "rev-parse", "--git-path", "index")
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(index) {
		index = filepath.Join(row.Worktree, index)
	}
	if err = os.WriteFile(index+".lock", []byte("held"), 0600); err != nil {
		t.Fatal(err)
	}
	req := orchapi.TaskStartRequest{Agent: "claude", ContinueFrom: source.ID}
	if _, err = orchclient.TaskStart(ctx, h.c, req); !api.IsCode(err, api.CodeUnavailable) || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("snapshot failure: %v", err)
	}
	source, err = orchclient.TaskStatus(ctx, h.c, source.ID, 0)
	if err != nil || !strings.Contains(source.ContinuationError, "snapshot") || source.Continuation != "" {
		t.Fatalf("failure not reported: %+v %v", source, err)
	}
	if err = os.Remove(index + ".lock"); err != nil {
		t.Fatal(err)
	}
	child, err := orchclient.TaskStart(ctx, h.c, req)
	if err != nil || child.ID == "" {
		t.Fatalf("snapshot retry: %+v %v", child, err)
	}
}
