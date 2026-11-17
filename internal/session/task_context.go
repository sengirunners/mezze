package session

import (
	"context"
	"path/filepath"

	"github.com/sengirunners/mezze/internal/session/tasklaunch"

	"github.com/sengirunners/mezze/internal/session/taskcontext"
	"github.com/sengirunners/potluck/kit/api"
)

// Called only after worktree resolution, before publishing/starting the holder.
// Snapshot stays with scratch for the holder's lifetime and daemon re-adoption;
// failed launches use spawn's existing undo. Continuations get a new snapshot.
func (s *Service) prepareClaudeTaskContext(ctx context.Context, bin string, argv []string, env map[string]string, cwd, scratch, addr string) ([]string, bool, error) {
	snapshot, err := taskcontext.Build(ctx, cwd, scratch)
	if err != nil {
		return nil, false, api.Errorf(api.CodeBadRequest, "Claude task context: %v", err)
	}
	if snapshot.Plugin != "" {
		// Filesystem-only preparation. The managed private launcher probes
		// capability after the holder/group identity has been durably recorded.
		if filepath.Base(bin) != "claude" {
			return nil, false, api.Errorf(api.CodeUnavailable, "Claude task skills require a verifiable Claude Code executable")
		}
		argv = append(argv, "--plugin-dir", snapshot.Plugin)
		found := false
		for i := len(argv) - 2; i >= 0; i-- {
			if argv[i] == "--settings" {
				argv[i+1] = `{"disableAllHooks":true,"disableSkillShellExecution":true}`
				found = true
				break
			}
		}
		if !found {
			return nil, false, api.Errorf(api.CodeUnavailable, "Claude task hook isolation settings are missing")
		}
	}
	for _, dir := range snapshot.Directories {
		argv = append(argv, "--add-dir", dir)
	}
	env["CLAUDE_CODE_ADDITIONAL_DIRECTORIES_CLAUDE_MD"] = "1"
	return argv, snapshot.Plugin != "", nil
}

func claudeTaskSkillVersion(value string) bool { return tasklaunch.SupportedVersion(value) }
