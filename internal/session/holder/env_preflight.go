package holder

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// preflightTaskEnv refuses only a simple env shebang whose named interpreter
// is absent from every child search location. It runs no commands and changes
// no state. Options, assignments, noncanonical shebangs and uncertain lookups
// remain the normal launch path; an exit status must never substitute for this
// prelaunch proof. A race making an interpreter disappear after this check is
// therefore still an ordinary, potentially started failure.
func preflightTaskEnv(spec Spec) error {
	if !spec.Task || (!filepath.IsAbs(spec.Path) && !strings.Contains(spec.Path, "/")) {
		return nil
	}
	path, known := envChildPath(spec.Path, spec.Dir)
	if !known {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return nil
	}
	// Stat before opening rejects FIFOs/devices. Nonblocking open also avoids
	// waiting on a FIFO substituted between that check and open.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return nil
	}
	b, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return nil
	}
	end := bytes.IndexByte(b, '\n')
	if end < 0 {
		return nil // no complete bounded first line: do not guess kernel truncation
	}
	line := string(b[:end])
	const prefix = "#!/usr/bin/env"
	if !strings.HasPrefix(line, prefix) || len(line) <= len(prefix) || (line[len(prefix)] != ' ' && line[len(prefix)] != '\t') {
		return nil
	}
	command := strings.TrimLeft(line[len(prefix):], " \t")
	if command == "" || command[0] == '-' {
		return nil
	}
	// These are literal single-token names/paths, not shell words. In
	// particular, never split -S, strip CR, unquote, expand or follow wrappers.
	for _, c := range command {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("_./+-", c)) {
			return nil
		}
	}
	var candidates []string
	if strings.Contains(command, "/") {
		candidate, known := envChildPath(command, spec.Dir)
		if !known {
			return nil
		}
		candidates = []string{candidate}
	} else {
		search, found := "", false
		for _, entry := range spec.Env {
			if strings.HasPrefix(entry, "PATH=") {
				search, found = entry[len("PATH="):], true // exec.Cmd keeps the last duplicate
			}
		}
		if !found {
			return nil // libc's unset-PATH default is not part of Spec
		}
		for _, dir := range strings.Split(search, ":") {
			candidate := command
			if dir != "" {
				candidate = dir + "/" + command
			}
			candidate, known = envChildPath(candidate, spec.Dir)
			if !known {
				return nil
			}
			candidates = append(candidates, candidate)
		}
	}
	for _, candidate := range candidates {
		_, err := os.Stat(candidate)
		// Any existing candidate, including nonexecutables/directories, or
		// an error other than ENOENT/ENOTDIR is inconclusive. Do not second-guess ACLs,
		// filesystem execution policy or env's search/error behavior.
		if !errors.Is(err, syscall.ENOENT) && !errors.Is(err, syscall.ENOTDIR) {
			return nil
		}
	}
	return fmt.Errorf("task env interpreter %q is absent from the child search path", command)
}

func envChildPath(path, dir string) (string, bool) {
	if filepath.IsAbs(path) {
		return path, true
	}
	if !filepath.IsAbs(dir) {
		return "", false
	}
	// Do not filepath.Join/Clean: symlink/../ has kernel semantics that a
	// lexical cleanup would change. Empty PATH components mean the child cwd.
	return dir + "/" + path, true
}
