package session

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/orchclient"
)

func TestSnapshotCleanTreeNeverRunsSignatureVerifier(t *testing.T) {
	dir := zrepo(t)
	head := zg(t, dir, "rev-parse", "HEAD")
	raw := zg(t, dir, "cat-file", "-p", head)
	pieces := strings.SplitN(raw, "\n\n", 2)
	if len(pieces) != 2 {
		t.Fatal("invalid commit")
	}
	signed := pieces[0] + "\ngpgsig -----BEGIN PGP SIGNATURE-----\n fake\n -----END PGP SIGNATURE-----\n\n" + pieces[1] + "\n"
	cmd := exec.Command("git", "-C", dir, "hash-object", "-t", "commit", "-w", "--stdin")
	cmd.Stdin = strings.NewReader(signed)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
	zg(t, dir, "update-ref", "refs/heads/main", strings.TrimSpace(string(out)))
	marker := filepath.Join(t.TempDir(), "ran")
	verifier := filepath.Join(t.TempDir(), "gpg")
	zw(t, verifier, "#!/bin/sh\necho invoked >> "+shellQuote(marker)+"\nexit 1\n")
	zg(t, dir, "config", "gpg.program", verifier)
	zg(t, dir, "config", "log.showSignature", "true")
	ok, err := zsnap(dir)
	if err != nil || ok {
		t.Fatalf("clean snapshot: %v %v", ok, err)
	}
	if b, err := os.ReadFile(marker); err == nil {
		t.Fatalf("signature verifier ran: %q", b)
	}
}

func TestSnapshotRefusesUntrackedNestedRepository(t *testing.T) {
	dir := zrepo(t)
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	zg(t, nested, "init", "-q", "-b", "main")
	zw(t, filepath.Join(nested, "committed"), "nested work\n")
	zg(t, nested, "add", "committed")
	zg(t, nested, "-c", "user.name=t", "-c", "user.email=t@e", "commit", "-qm", "nested")
	zw(t, filepath.Join(nested, "dirty.txt"), "not carried\n")
	zw(t, filepath.Join(dir, "plain.go"), "package main\n")
	head := zg(t, dir, "rev-parse", "HEAD")
	ok, err := zsnap(dir)
	if err == nil || ok || !strings.Contains(err.Error(), "nested") {
		t.Fatalf("nested repository accepted: %v %v", ok, err)
	}
	if got := zg(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("snapshot committed: %s", got)
	}
	if got, err := os.ReadFile(filepath.Join(nested, "dirty.txt")); err != nil || !bytes.Equal(got, []byte("not carried\n")) {
		t.Fatalf("nested work: %q %v", got, err)
	}
}

func TestSnapshotRacyUnchangedFilteredFileRefusesClearly(t *testing.T) {
	dir := zrepo(t)
	zg(t, dir, "config", "filter.rot.clean", rot)
	zg(t, dir, "config", "filter.rot.smudge", rot)
	zw(t, filepath.Join(dir, ".gitattributes"), "filtered.txt filter=rot\n")
	path := filepath.Join(dir, "filtered.txt")
	zw(t, path, "unchanged cleartext\n")
	zg(t, dir, "add", "-A")
	zg(t, dir, "commit", "-qm", "base")
	// A stat-only timestamp change forces Git to recheck the unchanged file.
	stamp := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	zw(t, filepath.Join(dir, "plain.go"), "package main\n")
	ok, err := zsnap(dir)
	if err == nil || ok || !strings.Contains(err.Error(), "cannot verify") {
		t.Fatalf("unsafe or unclear filtered result: %v %v", ok, err)
	}
}

func TestHandoffRefusesUntrackedNestedRepository(t *testing.T) {
	script := `if [ "$1" = exec ]; then mkdir nested; git -C nested init -q -b main; printf tracked > nested/tracked; git -C nested add tracked; git -C nested -c user.name=t -c user.email=t@e commit -qm nested; printf dirty > nested/dirty.txt; printf 'package main\n' > plain.go; printf '%s\n' '{"type":"turn.failed","error":{"code":"usage_limit_reached"}}'; else cat >/dev/null; printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"structured_output":{"finished":"done","text":"continued"}}'; fi`
	h, parent := taskHarness(t, script)
	taskClient(h, parent)
	zg(t, h.home, "init", "-q", "-b", "main")
	zg(t, h.home, "-c", "user.name=t", "-c", "user.email=t@e", "commit", "-q", "--allow-empty", "-m", "initial")
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
		if source.ContinuationError != "" || (source.Continuation != "" && source.Continuation != "pending") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if source.Continuation != "" || !strings.Contains(source.ContinuationError, "nested") {
		t.Fatalf("handoff did not refuse: %+v", source)
	}
}
