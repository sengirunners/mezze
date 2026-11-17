package orchapi

import "github.com/sengirunners/potluck/kit/config"

// zm's home and socket names. Kit holds none of its own (potluck
// kit/config.Names): a program sets them before its first Load, and this
// package does it for zm when it loads. Every zm binary, module and daemon
// imports orchapi, so none starts unnamed. The private ops names go ahead of
// these when zm reads both.
const (
	EnvHome    = "ZM_HOME" // the home's variable; the daemon's sessions and services get it
	HomeDir    = ".zm"     // the home in the user's home when no variable is set
	SocketName = "zm.sock" // the daemon's socket in the home
)

// Names is zm's names in kit's order.
var Names = config.Names{HomeEnvs: []string{EnvHome}, HomeDirs: []string{HomeDir}, Sockets: []string{SocketName}}

func init() { config.Use(Names) }
