package work

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/sengirunners/mezze/internal/orchapi"
	"github.com/sengirunners/mezze/internal/tools"
	"github.com/sengirunners/mezze/internal/work/adapter"
	"github.com/sengirunners/potluck/kit/api"
	"github.com/sengirunners/potluck/kit/core"
	"github.com/sengirunners/potluck/kit/secrets"
)

// A tracker's own places and its adapter's environment (SPEC 17.2; the
// allowlist itself is adapter.Env). A tracker with env = "inherit" gets the
// daemon's whole environment instead.

// trackerDir is $ZM_HOME/trackers/<project ULID>/<prefix>: the tracker's
// own HOME (home/) and, for --new and --server, its .beads directory.
func (s *Service) trackerDir(project, prefix string) string {
	return filepath.Join(s.home(), "trackers", project, prefix)
}

// secretFile holds a --server binding's password (0600, in a 0700 dir).
func (s *Service) secretFile(project, prefix string) string {
	return filepath.Join(s.home(), "secrets", project, prefix+".password")
}

func (s *Service) home() string {
	if s.d.Config != nil && s.d.Config.Home != "" {
		return s.d.Config.Home
	}
	return os.TempDir() // only bare test services have no config
}

// adapterEnv is the environment tracker tr of project runs with (SPEC
// 17.2), and the secret values in it, never relayed: nil means inherit (env
// = "inherit"). The base plus what the adapter declares in describe.env d;
// its secrets come through the Secrets brick.
func (s *Service) adapterEnv(project string, tr api.Tracker, d *orchapi.AdapterEnv) (env, secretVals []string, err error) {
	if tr.Env == api.TrackerEnvInherit {
		return nil, nil, nil
	}
	zm := map[string]string{orchapi.EnvDataDir: s.trackerDir(project, tr.Prefix), orchapi.EnvToolsDir: tools.Dir(s.home())}
	env, secretVals, err = adapter.Env(d, os.LookupEnv, zm, func(sec orchapi.AdapterSecret) (string, error) {
		if sec.From == orchapi.SecretSlot {
			return secrets.ReadFile(s.secretFile(project, tr.Prefix))
		}
		f := tr.Config[strings.TrimPrefix(sec.From, orchapi.SecretConfig)]
		if f == "" {
			return "", nil
		}
		v, err := secrets.Get(context.Background(), secrets.File(core.ExpandHome(f)))
		return v.Reveal(), err
	})
	if err != nil {
		return nil, nil, api.Errorf(api.CodeUnavailable, "%s tracker %s: its environment: %v", tr.Adapter, tr.Prefix, err)
	}
	return env, secretVals, nil
}
