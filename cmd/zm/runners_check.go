package main

// zm runners ls: how the broker learns of waiting jobs, and how its last
// round of asking GitHub went (GET /v0/runners/setup "check", potluck
// runners/check.go).

import (
	"fmt"
	"io"

	"github.com/sengirunners/potluck/runners"
)

// printRunnersCheck prints the checker's mode and, per linked installation,
// when it last heard every list it asks for and what failed or was cut short
// since. Nothing is printed before the App exists.
func printRunnersCheck(w io.Writer, c *runners.Checking) {
	if c == nil {
		return
	}
	if c.Mode == "webhook" {
		fmt.Fprintf(w, "check       GitHub's webhooks serve; a catch-up check every %d s\n", c.EveryS)
	} else {
		fmt.Fprintf(w, "check       asking GitHub every %d s (no public url: no webhook arrives)\n", c.EveryS)
	}
	if c.Error != "" {
		fmt.Fprintf(w, "            could not check at %s: %s\n", dash(c.ErrorAt), c.Error)
	}
	for _, i := range c.Installations {
		fmt.Fprintf(w, "            installation %d: %d repositories, last complete round %s\n", i.Installation, i.Repos, dash(i.LastOK))
		if i.Error != "" {
			fmt.Fprintf(w, "              errors since (%s): %s\n", dash(i.ErrorAt), i.Error)
		}
		if i.Cut != "" {
			fmt.Fprintf(w, "              cut short (%s): %s\n", dash(i.CutAt), i.Cut)
		}
	}
}
