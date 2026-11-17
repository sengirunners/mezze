package session

import (
	"os"
	"path/filepath"
)

// codexTaskCarry is all a Codex task takes from the real CODEX_HOME: login,
// config, the session history usage reads, and the model cache. Everything
// else (skills, plugins, hooks, databases) stays out, so a skill installed in
// the OS user's ~/.codex/skills cannot reach a task.
var codexTaskCarry = []string{"auth.json", "config.toml", "sessions", "models_cache.json"}

// prepareCodexTaskHome gives a Codex task its own CODEX_HOME under scratch,
// symlinking only codexTaskCarry from the real home (env CODEX_HOME, else
// userHome/.codex). It sets env[CODEX_HOME]; the real home is never written.
func prepareCodexTaskHome(userHome, scratch string, env map[string]string) error {
	src := env["CODEX_HOME"]
	if src == "" && userHome != "" {
		src = filepath.Join(userHome, ".codex")
	}
	dst := filepath.Join(scratch, "codex-home")
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	if src != "" {
		for _, name := range codexTaskCarry {
			from := filepath.Join(src, name)
			if _, err := os.Lstat(from); err != nil {
				continue
			}
			if err := os.Symlink(from, filepath.Join(dst, name)); err != nil && !os.IsExist(err) {
				return err
			}
		}
	}
	env["CODEX_HOME"] = dst
	return nil
}
