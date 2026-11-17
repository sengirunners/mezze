package holder

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func envPreflightScript(t *testing.T, dir, firstLine string) Spec {
	t.Helper()
	path := filepath.Join(dir, "task-agent")
	if err := os.WriteFile(path, []byte(firstLine+"\necho must-not-run > executed\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return Spec{Task: true, Path: path, Args: []string{path}, Dir: dir, Env: []string{"PATH=" + filepath.Join(dir, "absent-bin")}}
}

func envPreflightCandidate(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestTaskEnvPreflightMissingInterpreterUsesOnlyChildPath(t *testing.T) {
	dir := t.TempDir()
	spec := envPreflightScript(t, dir, "#!/usr/bin/env fixture-python")
	parentBin := filepath.Join(dir, "parent-bin")
	envPreflightCandidate(t, filepath.Join(parentBin, "fixture-python"), 0700)
	t.Setenv("PATH", parentBin)
	if err := preflightTaskEnv(spec); err == nil {
		t.Fatal("the parent's available interpreter hid its absence from the child PATH")
	}
	if _, err := os.Stat(filepath.Join(dir, "executed")); !os.IsNotExist(err) {
		t.Fatalf("preflight executed the script: %v", err)
	}
	spec.Env = []string{"PATH=" + parentBin}
	if err := preflightTaskEnv(spec); err != nil {
		t.Fatalf("available child interpreter refused: %v", err)
	}
}

func TestTaskEnvPreflightPathAndCwd(t *testing.T) {
	for _, search := range []string{"", ":missing-bin", "missing-bin:", "bin", "./bin"} {
		t.Run("path="+search, func(t *testing.T) {
			dir := t.TempDir()
			spec := envPreflightScript(t, dir, "#!/usr/bin/env fixture-python")
			spec.Env = []string{"PATH=" + search}
			if err := preflightTaskEnv(spec); err == nil {
				t.Fatal("interpreter absent from child-relative PATH was not detected")
			}
			candidate := filepath.Join(dir, "fixture-python")
			if search == "bin" || search == "./bin" {
				candidate = filepath.Join(dir, "bin", "fixture-python")
			}
			envPreflightCandidate(t, candidate, 0700)
			if err := preflightTaskEnv(spec); err != nil {
				t.Fatalf("child-relative interpreter refused: %v", err)
			}
		})
	}
	t.Run("last duplicate PATH wins", func(t *testing.T) {
		dir := t.TempDir()
		spec := envPreflightScript(t, dir, "#!/usr/bin/env fixture-python")
		envPreflightCandidate(t, filepath.Join(dir, "bin", "fixture-python"), 0700)
		spec.Env = []string{"PATH=" + filepath.Join(dir, "bin"), "PATH=" + filepath.Join(dir, "absent")}
		if err := preflightTaskEnv(spec); err == nil {
			t.Fatal("used the first PATH instead of the last")
		}
		spec.Env[0], spec.Env[1] = spec.Env[1], spec.Env[0]
		if err := preflightTaskEnv(spec); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("file PATH component cannot contain interpreter", func(t *testing.T) {
		dir := t.TempDir()
		spec := envPreflightScript(t, dir, "#!/usr/bin/env fixture-python")
		file := filepath.Join(dir, "not-a-directory")
		envPreflightCandidate(t, file, 0700)
		spec.Env = []string{"PATH=" + file + ":" + filepath.Join(dir, "absent-bin")}
		if err := preflightTaskEnv(spec); err == nil {
			t.Fatal("ENOTDIR hid an interpreter absent from every child search location")
		}
		// env continues searching after ENOTDIR; a later real candidate wins.
		envPreflightCandidate(t, filepath.Join(dir, "available-bin", "fixture-python"), 0700)
		spec.Env[0] += ":" + filepath.Join(dir, "available-bin")
		if err := preflightTaskEnv(spec); err != nil {
			t.Fatalf("later available interpreter refused: %v", err)
		}
	})
	t.Run("relative script path", func(t *testing.T) {
		dir := t.TempDir()
		spec := envPreflightScript(t, dir, "#!/usr/bin/env fixture-python")
		spec.Path = "./task-agent"
		if err := preflightTaskEnv(spec); err == nil {
			t.Fatal("relative script did not resolve against child cwd")
		}
	})
}

func TestTaskEnvPreflightLiteralInterpreterPaths(t *testing.T) {
	for _, name := range []string{"./bin/fixture-python", "bin/fixture-python", "/abs"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			command, candidate := name, filepath.Join(dir, "bin", "fixture-python")
			if name == "/abs" {
				command = candidate
			}
			spec := envPreflightScript(t, dir, "#!/usr/bin/env "+command)
			spec.Env = nil // slash-containing commands do not search PATH
			if err := preflightTaskEnv(spec); err == nil {
				t.Fatal("absent literal interpreter path accepted")
			}
			envPreflightCandidate(t, candidate, 0700)
			if err := preflightTaskEnv(spec); err != nil {
				t.Fatalf("available literal path refused: %v", err)
			}
		})
	}
	t.Run("preserve symlink dotdot semantics", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "target", "subdir"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(dir, "target", "subdir"), filepath.Join(dir, "link")); err != nil {
			t.Fatal(err)
		}
		envPreflightCandidate(t, filepath.Join(dir, "target", "bin", "fixture-python"), 0700)
		spec := envPreflightScript(t, dir, "#!/usr/bin/env fixture-python")
		spec.Env = []string{"PATH=link/../bin"}
		if err := preflightTaskEnv(spec); err != nil {
			t.Fatalf("lexical cleanup lost a reachable interpreter: %v", err)
		}
	})
}

func TestTaskEnvPreflightConservativeCases(t *testing.T) {
	for _, line := range []string{
		"#!/usr/bin/env -S fixture-python -u", "#!/usr/bin/env -- fixture-python",
		"#!/usr/bin/env VAR=x", "#!/usr/bin/env fixture-python extra",
		"#!/usr/bin/env fixture-python\r", "#!/usr/bin/env fixture-python ",
		"#!/usr/bin/env 'fixture-python'", "#!/usr/bin/env fixture\x00python",
		"#!/usr/bin/env fixture\u00a0python", "#! /usr/bin/env fixture-python",
		"#!/bin/sh", "#!/another/env fixture-python", "#!/usr/bin/env",
		"#!/usr/bin/env " + strings.Repeat("x", 300),
	} {
		t.Run(line, func(t *testing.T) {
			spec := envPreflightScript(t, t.TempDir(), line)
			if err := preflightTaskEnv(spec); err != nil {
				t.Fatalf("unsupported shebang refused: %v", err)
			}
		})
	}
	for _, kind := range []string{"not-task", "unset-path", "unknown-cwd", "bare-script", "not-executable", "directory-input", "fifo-input", "unterminated-line", "nonexec-candidate", "directory-candidate", "lookup-loop"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			spec := envPreflightScript(t, dir, "#!/usr/bin/env fixture-python")
			switch kind {
			case "not-task":
				spec.Task = false
			case "unset-path":
				spec.Env = nil
			case "unknown-cwd":
				spec.Dir, spec.Env = "", []string{"PATH="}
			case "bare-script":
				spec.Path = "task-agent"
			case "not-executable":
				if err := os.Chmod(spec.Path, 0600); err != nil {
					t.Fatal(err)
				}
			case "directory-input":
				spec.Path = dir
			case "fifo-input":
				spec.Path = filepath.Join(dir, "fifo")
				if err := syscall.Mkfifo(spec.Path, 0700); err != nil {
					t.Fatal(err)
				}
			case "unterminated-line":
				if err := os.WriteFile(spec.Path, []byte("#!/usr/bin/env fixture-python"), 0700); err != nil {
					t.Fatal(err)
				}
			case "nonexec-candidate":
				spec.Env = []string{"PATH=" + dir}
				envPreflightCandidate(t, filepath.Join(dir, "fixture-python"), 0600)
			case "directory-candidate":
				spec.Env = []string{"PATH=" + dir}
				if err := os.Mkdir(filepath.Join(dir, "fixture-python"), 0700); err != nil {
					t.Fatal(err)
				}
			case "lookup-loop":
				spec.Env = []string{"PATH=" + dir}
				if err := os.Symlink("fixture-python", filepath.Join(dir, "fixture-python")); err != nil {
					t.Fatal(err)
				}
			}
			if err := preflightTaskEnv(spec); err != nil {
				t.Fatalf("uncertain case refused: %v", err)
			}
		})
	}
}
