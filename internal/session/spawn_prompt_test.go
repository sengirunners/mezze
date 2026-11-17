package session

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
	"github.com/sengirunners/mezze/internal/session/holder"
	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/events"
)

func TestPromptRecognizesStartupOutputReplayedAtAdoption(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	p := &proc{pty: w, out: holder.NewRing(1024), exited: make(chan struct{})}
	p.out.Write([]byte("startup ready")) // adoption replay leaves lastOut zero
	s := &Service{ctx: ctx, PromptQuiet: 10 * time.Millisecond}
	go s.typePrompt(p, "followup")
	got := make(chan string, 1)
	go func() {
		b := make([]byte, 9)
		_, err := io.ReadFull(r, b)
		if err != nil {
			got <- err.Error()
			return
		}
		got <- string(b)
	}()
	select {
	case text := <-got:
		if text != "followup\r" {
			t.Fatalf("prompt bytes %q", text)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replayed startup output incorrectly waited for the no-output fallback")
	}
}

func TestCodexInitialPrompt(t *testing.T) {
	for _, tc := range []struct {
		name, agent string
		argv        []string
		want        bool
	}{
		{"bare", "codex", []string{"codex"}, true},
		{"options", "codex", []string{"/opt/bin/codex", "--model", "m", "-c", "a=b", "--profile=p", "--no-alt-screen"}, true},
		{"separator", "codex", []string{"codex", "--"}, true},
		{"other agent", "claude", []string{"codex"}, false},
		{"wrapper", "codex", []string{"wrapper", "codex"}, false},
		{"shell", "codex", []string{"sh", "-c", "codex"}, false},
		{"subcommand", "codex", []string{"codex", "exec"}, false},
		{"resume", "codex", []string{"codex", "resume", "--last"}, false},
		{"explicit prompt", "codex", []string{"codex", "already supplied"}, false},
		{"explicit after separator", "codex", []string{"codex", "--", "--a prompt"}, false},
		{"unknown option", "codex", []string{"codex", "--future", "value"}, false},
		{"missing value", "codex", []string{"codex", "--model"}, false},
		{"help", "codex", []string{"codex", "--help"}, false},
		{"empty", "codex", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexInitialPrompt(tc.agent, tc.argv); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCodexPromptCommand(t *testing.T) {
	for _, argv := range [][]string{{"codex", "--model", "m"}, {"codex", "--model", "m", "--"}} {
		before := append([]string(nil), argv...)
		const prompt = "--task\nwith 'quotes' and $(literal)"
		cmd := codexPromptCommand("/bin/codex", argv, prompt)
		if !reflect.DeepEqual(cmd, []string{"/bin/codex", "--model", "m", "--", prompt}) {
			t.Fatalf("command %q", cmd)
		}
		if !reflect.DeepEqual(argv, before) {
			t.Fatalf("mutated recorded argv: %q", argv)
		}
		// srt takes a shell command, so the prompt must be inside that
		// command and quoted, rather than appended as another srt argument.
		wrapped := sandboxArgv("srt", "policy", cmd)
		if len(wrapped) != 4 || !strings.HasSuffix(wrapped[3], shellQuote(prompt)) {
			t.Fatalf("sandbox command %q", wrapped)
		}
	}
}

// A startup screen prints and then waits for human input. It deliberately
// outlasts PromptQuiet: the old quiet-output heuristic types into this menu.
func TestCodexPromptDoesNotTypeIntoStartupScreen(t *testing.T) {
	for _, launch := range []string{"default", "configured", "explicit"} {
		t.Run(launch, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "codex")
			script := `#!/bin/sh
if [ "$1" = --version ]; then echo codex-test; exit; fi
printf '%s\000' "$@" > "$ARGV_FILE"
stty -echo -icanon min 1 time 0
printf 'Review hooks / Sign in: waiting for a person\n'
exec dd bs=1 count=1 of="$INPUT_FILE" 2>/dev/null
`
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			h := newHarness(t, func(c *config.Config) {
				if launch == "configured" {
					c.File.Agents = map[string][]string{"codex": {bin, "--no-alt-screen", "-c", "features.hooks=true"}}
				}
			})
			ch, cancel := h.d.Events.Subscribe(h.owner(), events.Filter{Types: []string{orchapi.EventSessionStarted}}, 16)
			defer cancel()
			const prompt = "--private task\nsecond line with 'quotes' and $literal"
			argFile, inputFile := filepath.Join(dir, "argv"), filepath.Join(dir, "input")
			req := orchapi.SpawnRequest{Agent: "codex", Cwd: h.home, Prompt: prompt, Model: "test-model",
				Env: map[string]string{"PATH": dir + ":/usr/bin:/bin", "ARGV_FILE": argFile, "INPUT_FILE": inputFile}}
			if launch == "explicit" {
				req.Command = []string{bin, "--"}
				req.Model = ""
			}
			ss, err := orchclient.Spawn(context.Background(), h.c, req)
			if err != nil {
				t.Fatal(err)
			}
			h.waitOutput(ss.ID, "waiting for a person")
			time.Sleep(600 * time.Millisecond) // six times the harness's quiet interval
			b, err := os.ReadFile(argFile)
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Split(strings.TrimSuffix(string(b), "\x00"), "\x00")
			want := []string{"--model", "test-model", "--", prompt}
			if launch == "configured" {
				want = append([]string{"--no-alt-screen", "-c", "features.hooks=true"}, want...)
			} else if launch == "explicit" {
				want = []string{"--", prompt}
			}
			if !reflect.DeepEqual(args, want) {
				t.Fatalf("child args %q, want %q", args, want)
			}
			if b, err := os.ReadFile(inputFile); err != nil || len(b) != 0 {
				t.Fatalf("startup screen received keystrokes %q: %v", b, err)
			}
			stored, ok := h.s.load(context.Background(), ss.ID)
			if !ok {
				t.Fatal("session not stored")
			}
			for _, v := range []any{ss, stored.Session} {
				b, _ := json.Marshal(v)
				if strings.Contains(string(b), "--private task") {
					t.Fatalf("prompt leaked in session metadata: %s", b)
				}
			}
			select {
			case ev := <-ch:
				b, _ := json.Marshal(ev)
				if strings.Contains(string(b), "--private task") {
					t.Fatalf("prompt leaked in event: %s", b)
				}
			case <-time.After(time.Second):
				t.Fatal("no session-started event")
			}
			if _, err := orchclient.Stop(context.Background(), h.c, ss.ID, orchapi.StopRequest{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCustomCommandPromptStillTyped(t *testing.T) {
	h := newHarness(t, nil)
	// typeLine submits with CR; shell read needs it translated to newline.
	ss := h.spawn("codex", `stty icanon icrnl -igncr -inlcr || exit 1; printf 'ready\n'; IFS= read -r line; printf 'received:%s\n' "$line"`, func(req *orchapi.SpawnRequest) {
		req.Prompt = "custom task"
	})
	h.waitOutput(ss.ID, "received:custom task")
	h.wait(ss.ID, "ended", ended)
}

func TestCodexExplicitCommandPromptPreserved(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "codex")
	script := `#!/bin/sh
if [ "$1" = --version ]; then echo codex-test; exit; fi
# typeLine submits with CR; shell read needs it translated to newline.
stty icanon icrnl -igncr -inlcr || exit 1
printf 'explicit:%s argc:%s\n' "$1" "$#"
IFS= read -r line
printf 'followup:%s\n' "$line"
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, nil)
	ss, err := orchclient.Spawn(context.Background(), h.c, orchapi.SpawnRequest{Agent: "codex", Cwd: h.home,
		Command: []string{bin, "existing prompt"}, Prompt: "followup task"})
	if err != nil {
		t.Fatal(err)
	}
	h.waitOutput(ss.ID, "explicit:existing prompt argc:1")
	h.waitOutput(ss.ID, "followup:followup task")
	h.wait(ss.ID, "ended", ended)
}
