package main

import (
	"testing"

	"github.com/sengirunners/potluck/kit/api"
)

func TestAlsoLoweredIsPrinted(t *testing.T) {
	if got := alsoLowered(api.Grant{}); got != "" {
		t.Errorf("none: %q", got)
	}
	if got := alsoLowered(api.Grant{AlsoLowered: []string{"sam-2", "sam-3"}}); got != "\nalso lowered (same GitHub account): sam-2, sam-3" {
		t.Errorf("two: %q", got)
	}
}
