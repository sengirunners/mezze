// Package builtin links zm's own zm-agent/1 drivers (claude, codex) and
// runs an adapter process when this binary was started as one
// (`zm __zm-agent <driver> ...`), before anything else.
package builtin

import (
	"os"

	"github.com/sengirunners/mezze/internal/agentpipe"
	_ "github.com/sengirunners/mezze/internal/agentpipe/claude"
	_ "github.com/sengirunners/mezze/internal/agentpipe/codex"
)

func init() {
	if len(os.Args) > 1 && os.Args[1] == agentpipe.Arg {
		os.Exit(agentpipe.Main(os.Args[2:]))
	}
}

// Linked is true; importing this package for it makes the drivers'
// registration explicit at the call site.
const Linked = true
