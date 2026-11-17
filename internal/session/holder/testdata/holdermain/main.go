// Command holdermain is a session holder alone: tests build it with other
// protocol versions (-ldflags -X) to stand in for another zm build.
package main

import (
	"fmt"
	"os"

	_ "github.com/sengirunners/mezze/internal/session/holder" // its init runs the holder
)

func main() {
	fmt.Fprintln(os.Stderr, "holdermain: run as `holdermain __zm-holder <home> <session8>`")
	os.Exit(2)
}
