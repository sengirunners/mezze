package session

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/potluck/kit/config"
	"github.com/sengirunners/potluck/kit/core"
)

// userSkillHome is a temp HOME holding a dummy skill for each agent.
func userSkillHome(t *testing.T) string {
	home := t.TempDir()
	for _, d := range []string{".claude/skills/zzuserskill", ".codex/skills/zzuserskill", ".codex/sessions"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	body := []byte("---\nname: zzuserskill\ndescription: dummy user-level skill\n---\nbody\n")
	for _, d := range []string{".claude", ".codex"} {
		if err := os.WriteFile(filepath.Join(home, d, "skills/zzuserskill/SKILL.md"), body, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(home, ".codex", "auth.json"), []byte("{}"), 0o600)
	os.WriteFile(filepath.Join(home, ".codex", "config.toml"), nil, 0o600)
	os.WriteFile(filepath.Join(home, ".codex", "state_5.sqlite"), []byte("x"), 0o600)
	return home
}

func TestCodexTaskHomeCarriesNoUserSkills(t *testing.T) {
	home := userSkillHome(t)
	scratch := t.TempDir()
	env := map[string]string{}
	if err := prepareCodexTaskHome(home, scratch, env); err != nil {
		t.Fatal(err)
	}
	dst := env["CODEX_HOME"]
	if dst != filepath.Join(scratch, "codex-home") {
		t.Fatalf("CODEX_HOME = %q", dst)
	}
	ents, _ := os.ReadDir(dst)
	var got []string
	for _, e := range ents {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != "auth.json,config.toml,sessions" {
		t.Fatalf("task home holds %v", got)
	}
	if _, err := os.Stat(filepath.Join(dst, "auth.json")); err != nil {
		t.Fatalf("auth not linked: %v", err)
	}
	// An explicit CODEX_HOME is the source instead of the user home.
	alt := t.TempDir()
	os.WriteFile(filepath.Join(alt, "auth.json"), []byte("{}"), 0o600)
	env = map[string]string{"CODEX_HOME": alt}
	if err := prepareCodexTaskHome(home, t.TempDir(), env); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(env["CODEX_HOME"], "auth.json")); err != nil {
		t.Fatalf("explicit home auth not linked: %v", err)
	}
}

// Runs the real agents (skipped when absent) in a temp HOME with a seeded
// user-level skill and checks neither lists it with the task launch setup.
func TestTaskAgentsIgnoreUserSkills(t *testing.T) {
	home := userSkillHome(t)
	cwd := t.TempDir()
	base := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + t.TempDir()}
	s := &Service{HolderExe: "/tmp/zm-test", versions: map[string]string{}, d: &core.Deps{Config: &config.Config{UserHome: home}}}

	t.Run("claude", func(t *testing.T) {
		bin, err := exec.LookPath("claude")
		if err != nil {
			t.Skip("claude not installed")
		}
		argv, err := s.taskCommand("claude", map[string]string{"PATH": os.Getenv("PATH")})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, bin, append(argv[1:], "hi")...)
		cmd.Dir, cmd.Env = cwd, base
		out, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { cancel(); _ = cmd.Wait() }()
		sc := bufio.NewScanner(out)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, `"subtype":"init"`) {
				if !strings.Contains(line, `"skills"`) {
					t.Fatalf("init lists no skills field; probe is blind: %.300s", line)
				}
				if strings.Contains(line, "zzuserskill") {
					t.Fatal("Claude task loaded ~/.claude/skills")
				}
				return
			}
		}
		t.Fatal("no init event from claude")
	})

	t.Run("codex", func(t *testing.T) {
		bin, err := exec.LookPath("codex")
		if err != nil {
			t.Skip("codex not installed")
		}
		env := map[string]string{}
		if err := prepareCodexTaskHome(home, t.TempDir(), env); err != nil {
			t.Fatal(err)
		}
		run := func(codexHome string) string {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, bin, "debug", "prompt-input", "hi")
			cmd.Dir, cmd.Env = cwd, append(base, "CODEX_HOME="+codexHome)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("codex debug prompt-input: %v\n%s", err, out)
			}
			return string(out)
		}
		if !strings.Contains(run(filepath.Join(home, ".codex")), "zzuserskill") {
			t.Skip("probe cannot see user skills in this codex version")
		}
		if strings.Contains(run(env["CODEX_HOME"]), "zzuserskill") {
			t.Fatal("Codex task loaded ~/.codex/skills")
		}
	})
}
