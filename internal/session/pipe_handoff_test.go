package session

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/agentpipe"
	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/config"
)

// TestPipeContinueFrom: a claude pipe session spawned with continue_from a
// codex one starts with zm's handoff of its history, then the caller's
// prompt; its own history keeps only that prompt, and only it (or the owner)
// reads the source's messages through task_read.
func TestPipeContinueFrom(t *testing.T) {
	got := filepath.Join(t.TempDir(), "claude-stdin.jsonl")
	h := newHarness(t, func(c *config.Config) {
		c.File.Agents = map[string][]string{"claude": {"/bin/sh", "-c", "cat > " + shellQuote(got)}}
	})
	ctx := context.Background()
	src := h.spawnPipe("codex/turns.rec.jsonl", "")
	var after int64
	h.until(src.ID, &after, "the session ready", func(ev orchapi.AgentEvent) bool {
		return ev.Type == agentpipe.EvSession && ev.Status == agentpipe.StateReady
	})
	if res, err := orchclient.Turn(ctx, h.c, src.ID, orchapi.SessionTurnRequest{Text: "Reply with exactly the word: alpha"}); err != nil || !res.OK {
		t.Fatalf("turn: %+v %v", res, err)
	}
	h.until(src.ID, &after, "the turn's end", turnEnd)
	pty := h.spawn("shell", "exec sleep 30", nil)
	for _, bad := range []orchapi.SpawnRequest{
		{Agent: "claude", Cwd: h.home, ContinueFrom: src.ID},
		{Agent: "claude", Cwd: h.home, Mode: orchapi.SessionModePipe, ContinueFrom: pty.ID},
	} {
		if _, err := orchclient.Spawn(ctx, h.c, bad); !api.IsCode(err, api.CodeBadRequest) {
			t.Fatalf("spawn %+v: %v", bad, err)
		}
	}
	succ, err := orchclient.Spawn(ctx, h.c, orchapi.SpawnRequest{Agent: "claude", Cwd: h.home, Mode: orchapi.SessionModePipe,
		ContinueFrom: src.ID, Prompt: "Continue where you left off."})
	if err != nil {
		t.Fatal(err)
	}
	var first string
	deadline := time.Now().Add(15 * time.Second)
	for first == "" && time.Now().Before(deadline) {
		if f, err := os.Open(got); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 64<<10), 1<<20)
			for sc.Scan() && first == "" {
				var line struct {
					Type    string `json:"type"`
					Message struct {
						Content []struct{ Text string } `json:"content"`
					} `json:"message"`
				}
				if json.Unmarshal(sc.Bytes(), &line) == nil && line.Type == "user" && len(line.Message.Content) > 0 {
					first = line.Message.Content[0].Text
				}
			}
			f.Close()
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.HasPrefix(first, "You are taking over this conversation") || !strings.Contains(first, `"source":"`+src.ID+`","agent":"codex"`) ||
		!strings.Contains(first, `{"id":"`+src.ID+`:0","role":"user","text":"Reply with exactly the word: alpha"}`) ||
		!strings.Contains(first, `"role":"assistant","text":"alpha"`) || !strings.HasSuffix(first, "}\n"+pipeUserMarker+"Continue where you left off.") {
		t.Fatalf("first turn: %s", first)
	}
	// The successor's own history is the prompt, not the handoff around it.
	if items, err := h.s.pipeMessages(ctx, succ.ID); err != nil || len(items) != 1 || items[0].Text != "Continue where you left off." {
		t.Fatalf("successor history: %+v %v", items, err)
	}
	taskClient(h, succ)
	page, err := orchclient.TaskRead(ctx, h.c, src.ID, src.ID+":0", 0)
	if err != nil || len(page.Items) != 1 || page.Items[0].Text != "Reply with exactly the word: alpha" {
		t.Fatalf("successor read: %+v %v", page, err)
	}
	taskClient(h, pty)
	if _, err = orchclient.TaskRead(ctx, h.c, src.ID, src.ID+":0", 0); !api.IsCode(err, api.CodeNotFound) {
		t.Fatalf("other session read: %v", err)
	}
	h.c.As = ""
	for _, id := range []string{src.ID, succ.ID, pty.ID} {
		if _, err := orchclient.Stop(ctx, h.c, id, orchapi.StopRequest{}); err != nil {
			t.Fatal(err)
		}
	}
}
