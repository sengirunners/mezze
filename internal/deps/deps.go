// Package deps pins every third-party dependency the team uses, so builders
// never need 'go get' or 'go mod tidy' while working in parallel.
// Only the architect/integrator edits this file, go.mod and go.sum.
package deps

import (
	_ "github.com/creack/pty"
	// pgx: zm-connect's Postgres client (the leads database). Pure Go, maintained, the
	// common choice; database/sql adds nothing zm-connect uses.
	_ "github.com/jackc/pgx/v5"
	_ "github.com/modelcontextprotocol/go-sdk/mcp"
	_ "github.com/oklog/ulid/v2"
	// potluck: the run-target drivers (vmhost, boxproc, conformance, ghapp,
	// zm-run-*) live in github.com/sengirunners/potluck, a public module;
	// make install and the release build its commands from it.
	// go.mod pins the commit zm builds against.
	_ "github.com/sengirunners/potluck/protocol"
	_ "github.com/spf13/cobra"
	// x/image/webp: decodes a WebP attachment so it is re-encoded without
	// metadata (internal/issues/imgsafe); the Go team's, pure Go, no cgo.
	_ "golang.org/x/image/webp"
	_ "golang.org/x/oauth2"
	_ "golang.org/x/sys/unix"
	_ "golang.org/x/term"
	_ "golang.org/x/text/unicode/norm"
	_ "modernc.org/sqlite"
	_ "tailscale.com/client/local"
	_ "tailscale.com/tsnet"
)
