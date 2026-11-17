package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
)

const rot = "tr a-zA-Z n-za-mN-ZA-M"

func zrepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	zg(t, dir, "init", "-q", "-b", "main")
	zg(t, dir, "config", "user.name", "t")
	zg(t, dir, "config", "user.email", "t@e")
	zg(t, dir, "commit", "-q", "--allow-empty", "-m", "initial")
	return dir
}
func zg(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "protocol.file.allow=always"}, args...)...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func zgno(dir string, args ...string) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	_ = cmd.Run()
}
func zw(t *testing.T, p, c string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(c), 0700); err != nil {
		t.Fatal(err)
	}
}
func zsnap(dir string) (bool, error) {
	return snapshotTaskWorktree(context.Background(), row{Session: orchapi.Session{Worktree: dir, Branch: "main"}})
}
func zmark(t *testing.T) (string, string) {
	m := filepath.Join(t.TempDir(), "ran")
	return m, "echo invoked >> " + shellQuote(m)
}
func zcheck(t *testing.T, m string) {
	t.Helper()
	if b, err := os.ReadFile(m); err == nil {
		t.Fatalf("LEAK: repository code ran during snapshot: %q", b)
	}
}

// Blocker 1
func TestZMFilterEndToEnd(t *testing.T) {
	seen := filepath.Join(t.TempDir(), "seen")
	// Start from an ordinary checkout. The agent then introduces the filter;
	// the test exercises snapshot refusal rather than checkout refusal.
	script := `if [ "$1" = exec ]; then git config filter.rot.clean ` + shellQuote(rot) + ` || exit 1; git config filter.rot.smudge ` + shellQuote(rot) + ` || exit 1; printf 'f.txt filter=rot\n' > .gitattributes; printf 'agent wrote this\n' > f.txt; printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else cat >/dev/null; cat f.txt > ` + seen + `; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"finished":"done","text":"c"}}'; fi`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	repo := h.home
	zg(t, repo, "init", "-q", "-b", "main")
	zw(t, repo+"/f.txt", "base\n")
	zg(t, repo, "add", "f.txt")
	zg(t, repo, "-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "-m", "i")
	time.Sleep(1100 * time.Millisecond)
	source, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", OnLimit: "next_agent"})
	if err != nil {
		t.Fatal(err)
	}
	source = waitTask(t, h, source.ID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		source, _ = orchclient.TaskStatus(context.Background(), h.c, source.ID, 0)
		if source.ContinuationError != "" || (source.Continuation != "" && source.Continuation != "pending") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if source.ContinuationError == "" || !strings.Contains(source.ContinuationError, "filter") || source.Continuation != "" {
		t.Fatalf("filtered source should be refused: %+v", source)
	}
	if _, err := os.Stat(seen); !os.IsNotExist(err) {
		t.Fatalf("continuation ran: %v", err)
	}
}

func TestZBFilterSecretPlaintext(t *testing.T) {
	dir := zrepo(t)
	zg(t, dir, "config", "filter.crypt.clean", "base64")
	zg(t, dir, "config", "filter.crypt.smudge", "base64 -D")
	zg(t, dir, "config", "filter.crypt.required", "true")
	zw(t, dir+"/.gitattributes", "secret.env filter=crypt\n")
	zw(t, dir+"/secret.env", "A=1\n")
	zg(t, dir, "add", "-A")
	zg(t, dir, "commit", "-qm", "base")
	time.Sleep(1100 * time.Millisecond)
	zw(t, dir+"/secret.env", "API_KEY=hunter2\n")
	ok, err := zsnap(dir)
	t.Logf("snap %v %v", ok, err)
	blob := zg(t, dir, "show", "HEAD:secret.env")
	if err == nil || ok {
		t.Fatalf("filtered path was not refused: %v %v", ok, err)
	}
	if strings.Contains(blob, "hunter2") {
		t.Fatalf("PLAINTEXT: encrypted-at-rest file committed in the clear: %q", blob)
	}
}

// Blocker 2, route A
func TestZFOrphanThroughClearedMarker(t *testing.T) {
	h, parent := taskHarness(t, `cat >/dev/null; exec sleep 25`)
	repo := filepath.Join(h.home, "repo")
	os.MkdirAll(repo, 0700)
	zg(t, repo, "init", "-q", "-b", "main")
	zg(t, repo, "-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "--allow-empty", "-m", "i")
	os.MkdirAll(repo+"/.git/hooks", 0700)
	inhook := filepath.Join(h.home, "inhook")
	zw(t, repo+"/.git/hooks/post-checkout", "#!/bin/sh\ntouch "+inhook+"\nsleep 2\n")
	h.c.As = parent.Address
	h.c.Cwd = repo
	var wg sync.WaitGroup
	var task orchapi.Task
	var terr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		task, terr = orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", Cwd: repo})
	}()
	for i := 0; i < 400; i++ {
		if _, err := os.Stat(inhook); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := h.s.Stop(context.Background(), h.d.System(), parent.ID, orchapi.StopRequest{Escalate: true, Force: true}); err != nil {
		t.Fatal(err)
	}
	h.wait(parent.ID, "ended", func(s orchapi.Session) bool { return s.State == orchapi.StateEnded })
	wg.Wait()
	t.Logf("task=%q err=%v", task.ID, terr)
	if terr == nil {
		time.Sleep(1500 * time.Millisecond)
		s, _ := orchclient.Session(context.Background(), h.c, task.ID)
		p, _ := orchclient.Session(context.Background(), h.c, parent.ID)
		t.Logf("parent state=%s child state=%s pid=%d", p.State, s.State, s.PID)
		running := s.State != orchapi.StateEnded
		h.s.Stop(context.Background(), h.d.System(), task.ID, orchapi.StopRequest{Escalate: true, Force: true})
		if running {
			t.Fatalf("LEAK: child task running with parent ended, marker cleared")
		}
	}
}

// Blocker 2, route B
func TestZFStartDuringParentGrace(t *testing.T) {
	for _, tc := range []struct{ name, script string }{
		{"term-then-killed", `trap '' TERM; echo ready; while :; do sleep 1; done`},
		{"term-exit0-after-grace", `trap 'sleep 1; exit 0' TERM; echo ready; while :; do sleep 0.1; done`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := taskHarness(t, `cat >/dev/null; exec sleep 25`)
			parent := h.spawn("shell", tc.script, nil)
			h.waitOutput(parent.ID, "ready")
			taskClient(h, parent)
			done := make(chan struct{})
			go func() {
				defer close(done)
				h.s.Stop(context.Background(), h.d.System(), parent.ID, orchapi.StopRequest{Escalate: true, Force: true})
			}()
			time.Sleep(300 * time.Millisecond)
			no := false
			task, err := orchclient.TaskStart(context.Background(), h.c, orchapi.TaskStartRequest{Agent: "codex", Prompt: "go", Worktree: &no})
			t.Logf("task start during stop: id=%q err=%v", task.ID, err)
			<-done
			h.wait(parent.ID, "ended", func(s orchapi.Session) bool { return s.State == orchapi.StateEnded })
			time.Sleep(1500 * time.Millisecond)
			if err == nil {
				s, _ := orchclient.Session(context.Background(), h.c, task.ID)
				t.Logf("child state=%s", s.State)
				running := s.State != orchapi.StateEnded
				h.s.Stop(context.Background(), h.d.System(), task.ID, orchapi.StopRequest{Escalate: true, Force: true})
				t.Fatalf("task start succeeded during stop; child running=%v", running)
			}
		})
	}
}

// Blocker 3
func TestZEPlainSpawnWorktreeBackgroundHook(t *testing.T) {
	h := newHarness(t, nil)
	repo := filepath.Join(h.home, "repo")
	os.MkdirAll(repo, 0700)
	zg(t, repo, "init", "-q", "-b", "main")
	zg(t, repo, "-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "--allow-empty", "-m", "i")
	os.MkdirAll(repo+"/.git/hooks", 0700)
	zw(t, repo+"/.git/hooks/post-checkout", "#!/bin/sh\nsleep 3 &\nexit 0\n")
	start := time.Now()
	ss, err := orchclient.Spawn(context.Background(), h.c, orchapi.SpawnRequest{Agent: "shell", Command: []string{"/bin/sh", "-c", "exec sleep 5"}, Cwd: repo, Worktree: true})
	t.Logf("spawn id=%q err=%v took=%v", ss.ID, err, time.Since(start))
	t.Logf("worktree list: %s", zg(t, repo, "worktree", "list"))
	t.Logf("branches: %s", zg(t, repo, "branch", "-a"))
	if ss.ID != "" {
		h.s.Stop(context.Background(), h.d.System(), ss.ID, orchapi.StopRequest{Escalate: true, Force: true})
	}
	if err != nil {
		t.Fatalf("PLAIN spawn broke: %v", err)
	}
}

// Blocker 4
func TestZAFilterEqualsName(t *testing.T) {
	dir := zrepo(t)
	m, sh := zmark(t)
	zg(t, dir, "config", "filter.a=b.clean", sh+"; cat")
	zw(t, dir+"/.gitattributes", "*.txt filter=a=b\n")
	zw(t, dir+"/w.txt", "raw")
	ok, err := zsnap(dir)
	if err == nil || ok {
		t.Fatalf("filtered path accepted: %v %v", ok, err)
	}
	zcheck(t, m)
}

func TestZAConfigHooks(t *testing.T) {
	for _, ev := range []string{"pre-commit", "prepare-commit-msg", "commit-msg", "post-commit", "post-index-change", "reference-transaction", "pre-auto-gc"} {
		t.Run(ev, func(t *testing.T) {
			dir := zrepo(t)
			m, sh := zmark(t)
			zg(t, dir, "config", "hook.evil.command", sh)
			zg(t, dir, "config", "hook.evil.event", ev)
			zw(t, dir+"/w.txt", "raw")
			ok, err := zsnap(dir)
			t.Logf("snap %v %v", ok, err)
			zcheck(t, m)
		})
	}
}

func TestZASubmoduleFilter(t *testing.T) {
	upstream := zrepo(t)
	zw(t, upstream+"/s.txt", "one\n")
	zw(t, upstream+"/.gitattributes", "*.txt filter=evil\n")
	zg(t, upstream, "add", "-A")
	zg(t, upstream, "commit", "-qm", "u")
	dir := zrepo(t)
	zg(t, dir, "submodule", "add", "-q", upstream, "sub")
	zg(t, dir, "commit", "-qm", "sub")
	m, sh := zmark(t)
	zg(t, dir+"/sub", "config", "filter.evil.clean", sh+"; cat")
	zw(t, dir+"/sub/s.txt", "two\n")
	zw(t, dir+"/w.txt", "raw")
	ok, err := zsnap(dir)
	if err == nil || ok {
		t.Fatalf("submodule accepted: %v %v", ok, err)
	}
	zcheck(t, m)
}

func TestZAPromisorLazyFetch(t *testing.T) {
	dir := zrepo(t)
	body := strings.Repeat("line of text\n", 200)
	zw(t, dir+"/a.txt", body)
	zg(t, dir, "add", "-A")
	zg(t, dir, "commit", "-qm", "a")
	blob := zg(t, dir, "rev-parse", "HEAD:a.txt")
	if err := os.Remove(filepath.Join(dir, ".git/objects", blob[:2], blob[2:])); err != nil {
		t.Fatal(err)
	}
	m, sh := zmark(t)
	zg(t, dir, "config", "core.repositoryformatversion", "1")
	zg(t, dir, "config", "extensions.partialClone", "origin")
	zg(t, dir, "config", "remote.origin.url", "ssh://evil.invalid/x")
	zg(t, dir, "config", "remote.origin.promisor", "true")
	zg(t, dir, "config", "remote.origin.partialclonefilter", "blob:none")
	zg(t, dir, "config", "core.sshCommand", sh+"; exit 1")
	os.Remove(dir + "/a.txt")
	zw(t, dir+"/b.txt", body+"more\n")
	zg(t, dir, "-c", "core.sshCommand=false", "add", "-A")
	ok, err := zsnap(dir)
	if err == nil || ok || !strings.Contains(err.Error(), "partial clone") {
		t.Fatalf("partial clone accepted: %v %v", ok, err)
	}
	zcheck(t, m)
}

// Blocker 5
func zconflict(t *testing.T) (dir, other string) {
	dir = zrepo(t)
	zw(t, dir+"/c.txt", "base\n")
	zg(t, dir, "add", "-A")
	zg(t, dir, "commit", "-qm", "base")
	zg(t, dir, "checkout", "-qb", "other")
	zw(t, dir+"/c.txt", "theirs\n")
	zg(t, dir, "commit", "-qam", "theirs")
	other = zg(t, dir, "rev-parse", "HEAD")
	zg(t, dir, "checkout", "-q", "main")
	zw(t, dir+"/c.txt", "ours\n")
	zg(t, dir, "commit", "-qam", "ours")
	return
}

func TestZLCherryPickStateEaten(t *testing.T) {
	dir, other := zconflict(t)
	zgno(dir, "cherry-pick", other)
	zg(t, dir, "add", "-A")
	_, err := os.Stat(dir + "/.git/CHERRY_PICK_HEAD")
	t.Logf("before: CHERRY_PICK_HEAD present=%v", err == nil)
	ok, serr := zsnap(dir)
	_, err = os.Stat(dir + "/.git/CHERRY_PICK_HEAD")
	blob := zg(t, dir, "show", "HEAD:c.txt")
	t.Logf("snap=%v %v after: CHERRY_PICK_HEAD present=%v author=%s blob=%q", ok, serr, err == nil, zg(t, dir, "log", "-1", "--format=%an"), blob)
	if strings.Contains(blob, "<<<<") {
		t.Fatal("WRONG STATE: conflict markers committed mid cherry-pick")
	}
}

func TestSnapshotRefusesAllOperationStates(t *testing.T) {
	for _, state := range []string{"MERGE_HEAD", "SQUASH_MSG", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply", "sequencer", "BISECT_START", "BISECT_LOG"} {
		t.Run(state, func(t *testing.T) {
			dir := zrepo(t)
			before := zg(t, dir, "rev-parse", "HEAD")
			p := filepath.Join(dir, ".git", state)
			if state == "rebase-merge" || state == "rebase-apply" || state == "sequencer" {
				if err := os.Mkdir(p, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				zw(t, p, "state\n")
			}
			zw(t, filepath.Join(dir, "conflict.txt"), "<<<<<<< ours\n=======\n>>>>>>> theirs\n")
			if ok, err := zsnap(dir); err == nil || ok {
				t.Fatalf("accepted %s: %v %v", state, ok, err)
			}
			if after := zg(t, dir, "rev-parse", "HEAD"); after != before {
				t.Fatalf("HEAD changed: %s -> %s", before, after)
			}
		})
	}
}

func TestSnapshotIgnoresInheritedGitRepository(t *testing.T) {
	dir := zrepo(t)
	other := zrepo(t)
	zw(t, filepath.Join(dir, "own.txt"), "own\n")
	zw(t, filepath.Join(other, "victim.txt"), "victim\n")
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	ok, err := zsnap(dir)
	if err != nil || !ok {
		t.Fatalf("snapshot: %v %v", ok, err)
	}
	_ = os.Unsetenv("GIT_DIR")
	_ = os.Unsetenv("GIT_WORK_TREE")
	if got := zg(t, dir, "show", "HEAD:own.txt"); got != "own" {
		t.Fatalf("own content: %q", got)
	}
	if out, err := exec.Command("git", "-C", other, "show", "HEAD:victim.txt").CombinedOutput(); err == nil {
		t.Fatalf("victim committed: %s", out)
	}
}

func TestFailedWorktreeAddCleansUp(t *testing.T) {
	repo := zrepo(t)
	home := t.TempDir()
	wt, branch, _, err := addWorktreeFrom(context.Background(), home, repo, "bad12345", "refs/heads/missing-base")
	if err == nil {
		t.Fatalf("worktree add succeeded: %s %s", wt, branch)
	}
	if _, err := os.Stat(filepath.Join(home, "worktrees", "bad12345")); !os.IsNotExist(err) {
		t.Fatalf("worktree left behind: %v", err)
	}
	out := zg(t, repo, "branch", "--list", "zm/bad12345")
	if out != "" {
		t.Fatalf("branch left behind: %q", out)
	}
}

func TestSnapshotOverallDeadline(t *testing.T) {
	dir := zrepo(t)
	fifo := filepath.Join(t.TempDir(), "excludes")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	zg(t, dir, "config", "core.excludesFile", fifo)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := snapshotTaskWorktree(ctx, row{Session: orchapi.Session{Worktree: dir, Branch: "main"}})
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("deadline: %v after %v", err, time.Since(start))
	}
}

func TestSnapshotRefusesTouchedPointerFilteredFile(t *testing.T) {
	dir := zrepo(t)
	zg(t, dir, "config", "filter.pointer.clean", "printf 'pointer:original\\n'")
	zg(t, dir, "config", "filter.pointer.smudge", "cat")
	zw(t, filepath.Join(dir, ".gitattributes"), "*.bin filter=pointer\n")
	zw(t, filepath.Join(dir, "large.bin"), strings.Repeat("raw content\n", 1000))
	zg(t, dir, "add", "-A")
	zg(t, dir, "commit", "-qm", "base")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(filepath.Join(dir, "large.bin"), future, future); err != nil {
		t.Fatal(err)
	}
	zw(t, filepath.Join(dir, "other"), "new")
	if ok, err := zsnap(dir); err == nil || ok {
		t.Fatalf("filtered file accepted: %v %v", ok, err)
	}
	if got := zg(t, dir, "show", "HEAD:large.bin"); got != "pointer:original" {
		t.Fatalf("pointer replaced: %q", got)
	}
}
